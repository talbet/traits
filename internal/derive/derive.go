// Package derive manages the lifecycle of a trait collection: it drafts a
// trait from feedback, splits compound proposals, relates a proposal to the
// stored traits (distinct, covered, clarification, conflict, inconclusive),
// independently verifies coverage, and turns the result into a plan that is
// written only when approved against an unchanged store.
//
// The flow is ported from crosshatch/traits (Apache-2.0), traits/Trait.ts,
// traits/TraitConflict.ts and traits/TraitDerivation.ts, to a non-interactive
// CLI: approval is a second invocation carrying the plan ID.
//
// Decision steps (decompose, relate, coverage) need only a decision model.
// Drafting and splitting need a LanguageModel.
package derive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/talbet/traits/internal/provider"
	"github.com/talbet/traits/internal/trait"
)

// LanguageModel generates one JSON object matching a JSON schema.
// *llm.Client implements it.
type LanguageModel interface {
	Generate(ctx context.Context, system, user, schemaName string, schema json.RawMessage) (json.RawMessage, error)
}

// ErrNeedLLM reports a step that needs a LanguageModel when none is
// configured.
var ErrNeedLLM = errors.New("no LLM configured")

// Relationship is how a proposal relates to one stored trait.
type Relationship string

const (
	Distinct  Relationship = "distinct"
	Covered   Relationship = "covered"
	Clarifies Relationship = "clarification"
	Conflicts Relationship = "conflict"
	Unsure    Relationship = "inconclusive"
)

// Coverage is the independent check of a "covered" relationship.
type Coverage string

const (
	Complete       Coverage = "complete"
	Incomplete     Coverage = "incomplete"
	CoverageUnsure Coverage = "inconclusive"
)

// Relation is the assessed relationship between a proposal and one stored
// trait.
type Relation struct {
	Trait        string       `json:"trait"`
	Path         string       `json:"path"`
	Relationship Relationship `json:"relationship"`
	Confidence   float64      `json:"confidence"`
	// Coverage is set only when Relationship is Covered.
	Coverage           Coverage `json:"coverage,omitempty"`
	CoverageConfidence float64  `json:"coverageConfidence,omitempty"`
}

// proposalInput is the decision input: the proposal as a JSON object.
func proposalInput(t trait.Trait) json.RawMessage {
	return json.RawMessage(`{"proposedTrait":` + traitJSON(t) + `}`)
}

// traitJSON is the crosshatch-compatible core of t as compact JSON, with
// empty fields omitted. Extension fields are not part of a trait's meaning
// for relating traits, so they are not sent.
func traitJSON(t trait.Trait) string {
	v := struct {
		Name          string `json:"name"`
		Subject       string `json:"subject,omitempty"`
		Requirement   string `json:"requirement,omitempty"`
		Applicability string `json:"applicability,omitempty"`
		Purpose       string `json:"purpose,omitempty"`
		Context       string `json:"context,omitempty"`
	}{
		Name:          t.Name,
		Subject:       strings.TrimSpace(t.Subject),
		Requirement:   strings.TrimSpace(t.Requirement),
		Applicability: strings.TrimSpace(t.Applicability),
		Purpose:       strings.TrimSpace(t.Purpose),
		Context:       strings.TrimSpace(t.Context),
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // strings always encode
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// Decompose asks dm whether proposal holds several independently applicable
// rules. An atomic proposal is returned as the only element. A decomposable
// one is split by lm into at least two traits; each is validated as a trait
// file and carries the proposal's appliesTo, scope, status and thresholds
// (its when and fix are specific to the compound text and are dropped). A
// decomposable proposal with a nil lm is an error wrapping ErrNeedLLM.
func Decompose(ctx context.Context, dm provider.DecisionModel, lm LanguageModel, proposal trait.Trait) ([]trait.Trait, error) {
	res, err := dm.Decide(ctx, provider.Request{
		Input: proposalInput(proposal),
		Questions: []provider.Question{{
			Name:         "decompose",
			Type:         provider.Choice,
			Instructions: decomposeInstructions,
			Options:      decomposeOptions(),
		}},
	})
	if err != nil {
		return nil, err
	}
	if len(res.Answers) != 1 {
		return nil, fmt.Errorf("decompose: %d answers for 1 question", len(res.Answers))
	}
	if res.Answers[0].Value != "decomposable" {
		return []trait.Trait{proposal}, nil
	}
	if lm == nil {
		return nil, fmt.Errorf("%w: proposal %q holds several independent rules; split it into separate trait files, or configure an LLM to split it", ErrNeedLLM, proposal.Name)
	}
	raw, err := lm.Generate(ctx, decomposeSystem, string(trait.Serialize(proposal)), "traits", json.RawMessage(decomposeSchema))
	if err != nil {
		return nil, err
	}
	var out struct {
		Traits []generated `json:"traits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decompose: LLM answer: %w", err)
	}
	if len(out.Traits) < 2 {
		return nil, fmt.Errorf("decompose: LLM returned %d traits, want at least 2", len(out.Traits))
	}
	parts := make([]trait.Trait, 0, len(out.Traits))
	seen := map[string]bool{}
	for i, g := range out.Traits {
		t := g.trait()
		t.AppliesTo = proposal.AppliesTo
		t.Scope = proposal.Scope
		t.Status = proposal.Status
		t.Thresholds = proposal.Thresholds
		v, err := validate(t)
		if err != nil {
			return nil, fmt.Errorf("decompose: LLM trait %d: %w", i+1, err)
		}
		if seen[v.Name] {
			return nil, fmt.Errorf("decompose: LLM returned two traits named %q", v.Name)
		}
		seen[v.Name] = true
		parts = append(parts, v)
	}
	return parts, nil
}

// generated is a trait as an LLM writes it.
type generated struct {
	Name          string  `json:"name"`
	Subject       string  `json:"subject"`
	Requirement   string  `json:"requirement"`
	Applicability *string `json:"applicability"`
	Purpose       string  `json:"purpose"`
	Context       *string `json:"context"`
}

func (g generated) trait() trait.Trait {
	t := trait.Trait{
		Name:        strings.TrimSpace(g.Name),
		Subject:     strings.TrimSpace(g.Subject),
		Requirement: strings.TrimSpace(g.Requirement),
		Purpose:     strings.TrimSpace(g.Purpose),
	}
	if g.Applicability != nil {
		t.Applicability = strings.TrimSpace(*g.Applicability)
	}
	if g.Context != nil {
		t.Context = strings.TrimSpace(*g.Context)
	}
	return t
}

// validate round-trips t through its file form, so it is exactly what would
// be written and read back.
func validate(t trait.Trait) (trait.Trait, error) {
	return trait.Parse("", trait.Serialize(t))
}

// Relate asks dm how proposal relates to each existing trait: one choice
// question per trait, each embedding that trait, in as few requests as
// dm.Limits().MaxQuestions allows. Every "covered" answer is then verified by
// an independent coverage question, batched the same way. Relations are
// returned in the order of existing.
func Relate(ctx context.Context, dm provider.DecisionModel, existing []trait.Trait, proposal trait.Trait) ([]Relation, error) {
	if len(existing) == 0 {
		return nil, nil
	}
	input := proposalInput(proposal)
	qs := make([]provider.Question, len(existing))
	for i, t := range existing {
		qs[i] = provider.Question{
			Name:         "relate:" + t.Name,
			Type:         provider.Choice,
			Instructions: relateIntro + traitJSON(t) + "\n" + relateInstructions,
			Options:      relateOptions(),
		}
	}
	answers, err := decideAll(ctx, dm, input, qs)
	if err != nil {
		return nil, err
	}
	rels := make([]Relation, len(existing))
	var covered []int
	var cqs []provider.Question
	for i, a := range answers {
		t := existing[i]
		rels[i] = Relation{Trait: t.Name, Path: t.Path, Relationship: Relationship(a.Value), Confidence: a.Confidence}
		if rels[i].Relationship == Covered {
			covered = append(covered, i)
			cqs = append(cqs, provider.Question{
				Name:         "coverage:" + t.Name,
				Type:         provider.Choice,
				Instructions: coverageIntro + traitJSON(t) + "\n" + coverageInstructions,
				Options:      coverageOptions(),
			})
		}
	}
	if len(cqs) == 0 {
		return rels, nil
	}
	canswers, err := decideAll(ctx, dm, input, cqs)
	if err != nil {
		return nil, err
	}
	for j, i := range covered {
		rels[i].Coverage = Coverage(canswers[j].Value)
		rels[i].CoverageConfidence = canswers[j].Confidence
	}
	return rels, nil
}

// decideAll asks qs about input in batches of at most the model's
// MaxQuestions and returns the answers in question order.
func decideAll(ctx context.Context, dm provider.DecisionModel, input json.RawMessage, qs []provider.Question) ([]provider.Answer, error) {
	size := dm.Limits().MaxQuestions
	if size <= 0 {
		size = len(qs)
	}
	out := make([]provider.Answer, 0, len(qs))
	for start := 0; start < len(qs); start += size {
		batch := qs[start:min(start+size, len(qs))]
		res, err := dm.Decide(ctx, provider.Request{Input: input, Questions: batch})
		if err != nil {
			return nil, err
		}
		if len(res.Answers) != len(batch) {
			return nil, fmt.Errorf("%d answers for %d questions", len(res.Answers), len(batch))
		}
		for i, a := range res.Answers {
			if a.Name != "" && a.Name != batch[i].Name {
				return nil, fmt.Errorf("answer %q for question %q", a.Name, batch[i].Name)
			}
		}
		out = append(out, res.Answers...)
	}
	return out, nil
}

// Derive runs the decision steps for proposal against the stored traits:
// Decompose, then Relate for each resulting trait, and returns the plan.
func Derive(ctx context.Context, dm provider.DecisionModel, lm LanguageModel, existing []trait.Trait, proposal trait.Trait) (Plan, error) {
	parts, err := Decompose(ctx, dm, lm, proposal)
	if err != nil {
		return Plan{}, err
	}
	assessed := make([]Assessment, len(parts))
	for i, p := range parts {
		rels, err := Relate(ctx, dm, existing, p)
		if err != nil {
			return Plan{}, err
		}
		assessed[i] = Assessment{Proposal: p, Relations: rels}
	}
	return BuildPlan(existing, assessed)
}

// Draft asks lm to turn feedback, and optionally a diff showing the
// correction, into a proposed trait. When diff is given it becomes the
// trait's context in a ```diff fence, after any context the LLM wrote. The
// result is validated as a trait file.
func Draft(ctx context.Context, lm LanguageModel, feedback, diff string) (trait.Trait, error) {
	if lm == nil {
		return trait.Trait{}, fmt.Errorf("%w: drafting a trait from feedback needs an LLM", ErrNeedLLM)
	}
	if strings.TrimSpace(feedback) == "" {
		return trait.Trait{}, errors.New("feedback is empty")
	}
	in := map[string]string{"feedback": feedback}
	if strings.TrimSpace(diff) != "" {
		in["diff"] = diff
	}
	user, _ := json.Marshal(in)
	raw, err := lm.Generate(ctx, draftSystem, string(user), "trait", json.RawMessage(draftSchema))
	if err != nil {
		return trait.Trait{}, err
	}
	var g generated
	if err := json.Unmarshal(raw, &g); err != nil {
		return trait.Trait{}, fmt.Errorf("draft: LLM answer: %w", err)
	}
	t := g.trait()
	if strings.TrimSpace(diff) != "" {
		fenced := fence("diff", strings.TrimRight(diff, "\n"))
		if t.Context != "" {
			t.Context += "\n\n" + fenced
		} else {
			t.Context = fenced
		}
	}
	v, err := validate(t)
	if err != nil {
		return trait.Trait{}, fmt.Errorf("draft: LLM trait: %w", err)
	}
	return v, nil
}

// fence wraps body in a Markdown code fence longer than any backtick run in
// it.
func fence(lang, body string) string {
	longest, run := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	return f + lang + "\n" + body + "\n" + f
}
