package check

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/talbet/traits/internal/diff"
	"github.com/talbet/traits/internal/provider"
	"github.com/talbet/traits/internal/trait"
)

// fakeModel answers trait questions from a table keyed by question name and
// records every request.
type fakeModel struct {
	mu       sync.Mutex
	answers  map[string]map[string]float64 // question name -> distribution
	limits   provider.Limits
	requests []provider.Request
	err      error
	// byInput, when set, overrides answers for inputs containing a key.
	byInput map[string]map[string]float64
}

func (m *fakeModel) Limits() provider.Limits { return m.limits }

func (m *fakeModel) Decide(_ context.Context, req provider.Request) (provider.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	if m.err != nil {
		return provider.Response{}, m.err
	}
	var res provider.Response
	for _, q := range req.Questions {
		dist, ok := m.answers[q.Name]
		for sub, d := range m.byInput {
			if strings.Contains(string(req.Input), sub) {
				dist, ok = d, true
			}
		}
		if !ok {
			dist = map[string]float64{"not-applicable": 0.9, "compliant": 0.1}
		}
		best, bestP := "", -1.0
		for k, v := range dist {
			if v > bestP {
				best, bestP = k, v
			}
		}
		res.Answers = append(res.Answers, provider.Answer{Name: q.Name, Type: q.Type, Value: best, Confidence: bestP, Probabilities: dist})
	}
	res.Usage.InputTokens = 100
	return res, nil
}

type memCache struct {
	mu sync.Mutex
	m  map[string]provider.Answer
}

func (c *memCache) Get(k string) (provider.Answer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.m[k]
	return a, ok
}

func (c *memCache) Put(k string, a provider.Answer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = a
}

func mkTrait(name string) trait.Trait {
	return trait.Trait{
		Name: name, Subject: "Code.", Requirement: "Do the thing", Purpose: "Because",
		Fix: "Do it", Scope: trait.ScopeHunk, Status: trait.StatusActive,
	}
}

func hunk(newStart int, added ...string) diff.Hunk {
	h := diff.Hunk{OldStart: newStart, NewStart: newStart, NewLines: len(added)}
	for i, a := range added {
		h.Lines = append(h.Lines, diff.Line{Kind: diff.Added, NewLine: newStart + i, Text: a})
	}
	return h
}

func file(path string, hunks ...diff.Hunk) diff.File {
	return diff.File{OldPath: path, NewPath: path, Hunks: hunks}
}

func TestRunTiersAndLocation(t *testing.T) {
	m := &fakeModel{answers: map[string]map[string]float64{
		"bad":          {"violation": 0.9, "compliant": 0.1},
		"maybe":        {"violation": 0.6, "compliant": 0.4},
		"unsure":       {"inconclusive": 0.7, "violation": 0.2, "compliant": 0.1},
		"fine":         {"compliant": 0.95, "violation": 0.05},
		"locate:bad":   {"h2": 0.8, "h1": 0.2},
		"locate:maybe": {"h1": 0.7, "h2": 0.3},
	}}
	opts := Options{Model: m, ProviderName: "fake", ModelName: "m",
		Traits: []trait.Trait{mkTrait("bad"), mkTrait("maybe"), mkTrait("unsure"), mkTrait("fine"), mkTrait("other")}}
	rep, err := Run(context.Background(), opts, []diff.File{file("a.go", hunk(1, "x"), hunk(40, "y", "z"))})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 1 {
		t.Fatalf("files = %+v", rep.Files)
	}
	got := map[string]Finding{}
	for _, f := range rep.Files[0].Findings {
		got[f.Trait] = f
	}
	if f := got["bad"]; f.Tier != TierFix || f.Location == nil || f.Location.StartLine != 40 || f.Location.EndLine != 41 {
		t.Errorf("bad = %+v loc %+v", f, f.Location)
	}
	if f := got["maybe"]; f.Tier != TierReview || f.Classification != Violation || f.Location.StartLine != 1 {
		t.Errorf("maybe = %+v", f)
	}
	if f := got["unsure"]; f.Tier != TierReview || f.Classification != Inconclusive || f.Location != nil {
		t.Errorf("unsure = %+v", f)
	}
	if _, ok := got["fine"]; ok {
		t.Error("compliant trait reported")
	}
	if rep.Skipped.Compliant != 1 || rep.Skipped.NotApplicable != 1 {
		t.Errorf("skipped = %+v", rep.Skipped)
	}
	if rep.Files[0].Findings[0].Trait != "bad" {
		t.Errorf("fix finding not first: %+v", rep.Files[0].Findings)
	}
	if _, ok := rep.Traits["bad"]; !ok || len(rep.Traits) != 3 {
		t.Errorf("traits = %+v", rep.Traits)
	}
	// One classification request and one locate request (bad and maybe).
	if len(m.requests) != 2 || len(m.requests[1].Questions) != 2 {
		t.Errorf("requests = %d", len(m.requests))
	}
	if !strings.Contains(got["bad"].Interpretation, "lines 40-41") || !strings.Contains(got["bad"].Interpretation, "Fix: Do it.") {
		t.Errorf("interpretation = %q", got["bad"].Interpretation)
	}
}

func TestRunSingleHunkSkipsLocate(t *testing.T) {
	m := &fakeModel{answers: map[string]map[string]float64{"bad": {"violation": 0.9}}}
	opts := Options{Model: m, Traits: []trait.Trait{mkTrait("bad")}}
	rep, err := Run(context.Background(), opts, []diff.File{file("a.go", hunk(7, "x"))})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.requests) != 1 {
		t.Errorf("requests = %d, want 1", len(m.requests))
	}
	if loc := rep.Files[0].Findings[0].Location; loc == nil || loc.StartLine != 7 || loc.Probability != 1 {
		t.Errorf("loc = %+v", loc)
	}
}

func TestRunGating(t *testing.T) {
	m := &fakeModel{}
	ts := mkTrait("ts-only")
	ts.AppliesTo = []string{"*.ts"}
	cand := mkTrait("cand")
	cand.Status = trait.StatusCandidate
	opts := Options{Model: m, Traits: []trait.Trait{ts, cand}}
	rep, err := Run(context.Background(), opts, []diff.File{file("a.go", hunk(1, "x"))})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.requests) != 0 || rep.Skipped.Gated != 2 {
		t.Errorf("requests = %d skipped = %+v", len(m.requests), rep.Skipped)
	}
}

func TestRunMaxQuestionsSplits(t *testing.T) {
	m := &fakeModel{limits: provider.Limits{MaxQuestions: 2}}
	opts := Options{Model: m, Traits: []trait.Trait{mkTrait("a"), mkTrait("b"), mkTrait("c")}}
	if _, err := Run(context.Background(), opts, []diff.File{file("a.go", hunk(1, "x"))}); err != nil {
		t.Fatal(err)
	}
	if len(m.requests) != 2 {
		t.Errorf("requests = %d, want 2", len(m.requests))
	}
}

func TestRunOversizedHunkIsFileError(t *testing.T) {
	m := &fakeModel{limits: provider.Limits{MaxInputBytes: 200}}
	opts := Options{Model: m, Traits: []trait.Trait{mkTrait("a")}}
	big := hunk(10, strings.Repeat("x", 500))
	rep, err := Run(context.Background(), opts, []diff.File{file("a.go", hunk(1, "ok"), big)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0].Error, "@@ -10 +10") {
		t.Errorf("errors = %+v", rep.Errors)
	}
	if len(m.requests) != 1 {
		t.Errorf("small hunk not checked: %d requests", len(m.requests))
	}
}

func TestRunProviderErrorAborts(t *testing.T) {
	m := &fakeModel{err: &provider.Error{Provider: "fake", Err: errors.New("down")}}
	opts := Options{Model: m, Traits: []trait.Trait{mkTrait("a")}}
	_, err := Run(context.Background(), opts, []diff.File{file("a.go", hunk(1, "x")), file("b.go", hunk(1, "y"))})
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunFileScopeWithoutReader(t *testing.T) {
	m := &fakeModel{}
	ft := mkTrait("whole")
	ft.Scope = trait.ScopeFile
	opts := Options{Model: m, Traits: []trait.Trait{ft}}
	rep, err := Run(context.Background(), opts, []diff.File{file("a.go", hunk(1, "x"))})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Errors) != 1 || len(m.requests) != 0 {
		t.Errorf("errors = %+v requests = %d", rep.Errors, len(m.requests))
	}
}

func TestRunCache(t *testing.T) {
	m := &fakeModel{answers: map[string]map[string]float64{"bad": {"violation": 0.9}}}
	c := &memCache{m: map[string]provider.Answer{}}
	opts := Options{Model: m, Traits: []trait.Trait{mkTrait("bad")}, Cache: c}
	files := []diff.File{file("a.go", hunk(1, "x"))}
	if _, err := Run(context.Background(), opts, files); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), opts, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.requests) != 1 || rep.Skipped.Cached != 1 || !rep.Files[0].Findings[0].Cached {
		t.Errorf("requests = %d report = %+v", len(m.requests), rep)
	}
}

func TestRunSkipsBinaryAndDeleted(t *testing.T) {
	m := &fakeModel{}
	opts := Options{Model: m, Traits: []trait.Trait{mkTrait("a")}}
	files := []diff.File{
		{OldPath: "img.png", NewPath: "img.png", Binary: true},
		{OldPath: "gone.go", NewPath: "/dev/null", Hunks: []diff.Hunk{hunk(1, "x")}},
	}
	if _, err := Run(context.Background(), opts, files); err != nil {
		t.Fatal(err)
	}
	if len(m.requests) != 0 {
		t.Errorf("requests = %d", len(m.requests))
	}
}

func TestRunWorstTierAcrossGroups(t *testing.T) {
	// Two hunks that cannot share a request: one answer is inconclusive
	// (review), the other leans compliant with a higher violation
	// probability that is still below review. The review must survive.
	m := &fakeModel{
		limits: provider.Limits{MaxInputBytes: 220},
		byInput: map[string]map[string]float64{
			"AAAA": {"inconclusive": 0.8, "violation": 0.1, "compliant": 0.1},
			"BBBB": {"compliant": 0.7, "violation": 0.3},
		},
	}
	opts := Options{Model: m, Traits: []trait.Trait{mkTrait("t")}}
	rep, err := Run(context.Background(), opts, []diff.File{file("a.go", hunk(1, strings.Repeat("A", 40)), hunk(50, strings.Repeat("B", 40)))})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (one per group)", len(m.requests))
	}
	if len(rep.Files) != 1 || rep.Files[0].Findings[0].Classification != Inconclusive {
		t.Errorf("report = %+v", rep)
	}
}
