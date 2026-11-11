package derive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/talbet/traits/internal/trait"
)

// Outcome is what a plan does with one proposed trait.
type Outcome string

const (
	// OutcomeCreated writes the proposal as a new trait file.
	OutcomeCreated Outcome = "created"
	// OutcomeCovered writes nothing: a stored trait already covers it, and
	// the independent coverage check agreed.
	OutcomeCovered Outcome = "covered"
	// OutcomeClarification rewrites the one stored trait it clarifies, in
	// place, keeping that trait's name and extension fields.
	OutcomeClarification Outcome = "clarification"
	// OutcomeConflict needs an explicit resolution per conflicting trait.
	OutcomeConflict Outcome = "conflict"
	// OutcomeInconclusive needs a human edit: the model could not relate the
	// proposal reliably, or it clarifies more than one stored trait.
	OutcomeInconclusive Outcome = "inconclusive"
	// OutcomeCoverageGap needs a human edit: a stored trait covers the
	// proposal only partly.
	OutcomeCoverageGap Outcome = "coverage-gap"
)

// Resolutions of a conflict, given per conflicting stored trait.
const (
	// Replace deletes the stored trait and writes the proposal.
	Replace = "replace"
	// Keep keeps the stored trait and drops the proposal.
	Keep = "keep"
	// Both keeps the stored trait and writes the proposal too.
	Both = "both"
)

var (
	// ErrStale reports that the store changed since the plan was made.
	ErrStale = errors.New("traits changed since the plan; re-run derive")
	// ErrNameClash reports a new trait whose name is already taken.
	ErrNameClash = errors.New("a trait with this name already exists")
	// ErrNeedsAttention reports a plan that cannot be applied as is.
	ErrNeedsAttention = errors.New("plan needs attention")
)

// Assessment is one proposed trait and its relations to the stored traits.
type Assessment struct {
	Proposal  trait.Trait
	Relations []Relation
}

// Item is the plan for one proposed trait.
type Item struct {
	Outcome  Outcome
	Proposal trait.Trait
	// Result is the trait to write, with Path relative to the traits
	// directory, for OutcomeCreated and OutcomeClarification.
	Result *trait.Trait
	// Targets names the stored traits the outcome is about.
	Targets []string
	// Relations lists the non-distinct relations found.
	Relations []Relation
	// Reason explains the outcome and what to do next.
	Reason string
}

// Plan is what derive proposes to do. ID identifies the plan together with
// the store snapshot it was made against.
type Plan struct {
	ID    string `json:"planId"`
	Items []Item `json:"items"`
}

// Change is one file operation a plan performs.
type Change struct {
	// Action is "create", "update" or "delete".
	Action string `json:"action"`
	Name   string `json:"name"`
	// Path is relative to the traits directory.
	Path  string       `json:"path"`
	Trait *trait.Trait `json:"-"`
}

// BuildPlan turns assessments into a plan against the stored traits. For each
// proposal, in order of precedence: any inconclusive relation or coverage
// check makes it Inconclusive; a covered relation whose coverage check found
// gaps makes it a CoverageGap; a conflict makes it a Conflict; a verified
// covered relation makes it Covered; one clarification makes it a
// Clarification (more than one is Inconclusive); otherwise it is Created. A
// proposal that would be written under the name of a stored trait other
// than the one it replaces is an error wrapping ErrNameClash.
func BuildPlan(existing []trait.Trait, assessed []Assessment) (Plan, error) {
	byName := make(map[string]trait.Trait, len(existing))
	for _, t := range existing {
		byName[t.Name] = t
	}
	var p Plan
	for _, a := range assessed {
		it := Item{Proposal: a.Proposal}
		var unsure, gaps, conflicts, covered, clar []string
		for _, r := range a.Relations {
			if r.Relationship != Distinct {
				it.Relations = append(it.Relations, r)
			}
			switch {
			case r.Relationship == Unsure, r.Relationship == Covered && r.Coverage != Complete && r.Coverage != Incomplete:
				unsure = append(unsure, r.Trait)
			case r.Relationship == Covered && r.Coverage == Incomplete:
				gaps = append(gaps, r.Trait)
			case r.Relationship == Covered:
				covered = append(covered, r.Trait)
			case r.Relationship == Conflicts:
				conflicts = append(conflicts, r.Trait)
			case r.Relationship == Clarifies:
				clar = append(clar, r.Trait)
			}
		}
		name := a.Proposal.Name
		switch {
		case len(unsure) > 0:
			it.Outcome, it.Targets = OutcomeInconclusive, unsure
			it.Reason = fmt.Sprintf("the relationship of %s to %s could not be reliably judged; edit the proposal or the stored trait to remove the ambiguity, then re-run derive", name, list(unsure))
		case len(gaps) > 0:
			it.Outcome, it.Targets = OutcomeCoverageGap, gaps
			it.Reason = fmt.Sprintf("%s covers %s only partly; edit the stored trait to include the missing cases, or make the proposal a distinct rule, then re-run derive", list(gaps), name)
		case len(conflicts) > 0:
			it.Outcome, it.Targets = OutcomeConflict, conflicts
			if t, taken := byName[name]; taken && !slices.Contains(conflicts, name) {
				return Plan{}, clash(name, t)
			}
			it.Reason = fmt.Sprintf("%s conflicts with %s; choose per trait with --resolve NAME=replace|keep|both", name, list(conflicts))
		case len(covered) > 0:
			it.Outcome, it.Targets = OutcomeCovered, covered
			it.Reason = fmt.Sprintf("already covered by %s (coverage verified); nothing to write", list(covered))
		case len(clar) > 1:
			it.Outcome, it.Targets = OutcomeInconclusive, clar
			it.Reason = fmt.Sprintf("%s clarifies several traits (%s); narrow the proposal or merge those traits first", name, list(clar))
		case len(clar) == 1:
			it.Outcome, it.Targets = OutcomeClarification, clar
			r := clarify(byName[clar[0]], a.Proposal)
			it.Result = &r
			it.Reason = fmt.Sprintf("a clearer replacement of %s; it is rewritten in place", clar[0])
		default:
			if t, taken := byName[name]; taken {
				return Plan{}, clash(name, t)
			}
			it.Outcome = OutcomeCreated
			r := a.Proposal
			r.Path = name + ".md"
			it.Result = &r
			it.Reason = "distinct from every stored trait; written as a new trait"
		}
		p.Items = append(p.Items, it)
	}
	p.ID = PlanID(existing, p.Items)
	return p, nil
}

func clash(name string, t trait.Trait) error {
	return fmt.Errorf("%w: %q (%s); rename the proposal or propose a clarification of it", ErrNameClash, name, t.Path)
}

func list(names []string) string { return strings.Join(names, ", ") }

// clarify returns the replacement for x: the proposal's core fields under x's
// name and path, keeping x's extension fields where the proposal sets none.
// A proposal's scope hunk and status active are defaults and count as unset.
func clarify(x, p trait.Trait) trait.Trait {
	r := p
	r.Name, r.Path = x.Name, x.Path
	if len(r.AppliesTo) == 0 {
		r.AppliesTo = x.AppliesTo
	}
	if len(r.When) == 0 {
		r.When = x.When
	}
	if r.Fix == "" {
		r.Fix = x.Fix
	}
	if r.Scope == "" || r.Scope == trait.ScopeHunk {
		r.Scope = x.Scope
	}
	if r.Status == "" || r.Status == trait.StatusActive {
		r.Status = x.Status
	}
	if r.Thresholds == (trait.Thresholds{}) {
		r.Thresholds = x.Thresholds
	}
	return r
}

// PlanID hashes the planned items (outcome, targets, proposal and result
// files) and the store snapshot (every stored trait's path and file, by
// name) to 12 hex characters.
func PlanID(existing []trait.Trait, items []Item) string {
	h := sha256.New()
	put := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	put("traits-derive/1")
	for _, it := range items {
		put("item")
		put(string(it.Outcome))
		put(strings.Join(it.Targets, "\n"))
		put(string(trait.Serialize(it.Proposal)))
		if it.Result != nil {
			put(it.Result.Path)
			put(string(trait.Serialize(*it.Result)))
		}
	}
	store := slices.Clone(existing)
	slices.SortFunc(store, func(a, b trait.Trait) int { return strings.Compare(a.Name, b.Name) })
	for _, t := range store {
		put("stored")
		put(t.Path)
		put(string(trait.Serialize(t)))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Blocked lists why the plan cannot be applied with res: items that need a
// human edit, and conflicts without a resolution. It is empty when the plan
// can be applied.
func (p Plan) Blocked(res map[string]string) []string {
	var out []string
	for _, it := range p.Items {
		switch it.Outcome {
		case OutcomeInconclusive, OutcomeCoverageGap:
			out = append(out, it.Proposal.Name+": "+it.Reason)
		case OutcomeConflict:
			var missing []string
			for _, t := range it.Targets {
				if res[t] == "" {
					missing = append(missing, t)
				}
			}
			if len(missing) > 0 {
				out = append(out, fmt.Sprintf("%s: conflicts with %s; add --resolve %s=replace|keep|both", it.Proposal.Name, list(missing), missing[0]))
			}
		}
	}
	return out
}

// Changes returns the file operations the plan performs with the conflict
// resolutions res (stored trait name to Replace, Keep or Both). It fails
// when the plan is blocked (wrapping ErrNeedsAttention), when res names a
// trait that is not in conflict or holds an unknown choice, or when two
// operations touch the same file.
func (p Plan) Changes(res map[string]string) ([]Change, error) {
	if b := p.Blocked(res); len(b) > 0 {
		return nil, fmt.Errorf("%w:\n  %s", ErrNeedsAttention, strings.Join(b, "\n  "))
	}
	conflicted := map[string]string{} // name -> path
	for _, it := range p.Items {
		if it.Outcome == OutcomeConflict {
			for _, r := range it.Relations {
				if r.Relationship == Conflicts {
					conflicted[r.Trait] = r.Path
				}
			}
		}
	}
	for name, choice := range res {
		if _, ok := conflicted[name]; !ok {
			return nil, fmt.Errorf("--resolve %s: %q is not in conflict with the proposal", name, name)
		}
		switch choice {
		case Replace, Keep, Both:
		default:
			return nil, fmt.Errorf("--resolve %s=%s: choose replace, keep or both", name, choice)
		}
	}

	var out []Change
	for _, it := range p.Items {
		switch it.Outcome {
		case OutcomeCreated:
			out = append(out, Change{Action: "create", Name: it.Result.Name, Path: it.Result.Path, Trait: it.Result})
		case OutcomeClarification:
			out = append(out, Change{Action: "update", Name: it.Result.Name, Path: it.Result.Path, Trait: it.Result})
		case OutcomeConflict:
			cs, err := resolve(it, res, conflicted)
			if err != nil {
				return nil, err
			}
			out = append(out, cs...)
		}
	}
	seen := map[string]bool{}
	for _, c := range out {
		key := filepath.ToSlash(filepath.Clean(c.Path))
		if seen[key] {
			return nil, fmt.Errorf("the plan changes %s twice; derive the proposals one at a time", c.Path)
		}
		seen[key] = true
	}
	return out, nil
}

func resolve(it Item, res map[string]string, paths map[string]string) ([]Change, error) {
	var keep, replace []string
	for _, t := range it.Targets {
		switch res[t] {
		case Keep:
			keep = append(keep, t)
		case Replace:
			replace = append(replace, t)
		}
	}
	if len(keep) > 0 && len(replace) > 0 {
		return nil, fmt.Errorf("%s: cannot keep %s and replace %s: keep drops the proposal", it.Proposal.Name, list(keep), list(replace))
	}
	if len(keep) > 0 {
		return nil, nil
	}
	name := it.Proposal.Name
	t := it.Proposal
	var out []Change
	written := false
	for _, r := range replace {
		if r == name {
			t.Path = paths[r]
			out = append(out, Change{Action: "update", Name: name, Path: t.Path, Trait: &t})
			written = true
			continue
		}
		out = append(out, Change{Action: "delete", Name: r, Path: paths[r]})
	}
	if !written {
		if slices.Contains(it.Targets, name) {
			return nil, fmt.Errorf("%s: the proposal has the same name as the trait it conflicts with; resolve it with replace, or rename the proposal", name)
		}
		t.Path = name + ".md"
		out = append([]Change{{Action: "create", Name: name, Path: t.Path, Trait: &t}}, out...)
	}
	return out, nil
}

// Apply writes plan into dir after checking approvedID: it must equal the
// plan's ID, and recomputing the ID against the traits now in dir must give
// the same value, or Apply fails with ErrStale. Files are written atomically
// (temporary file, then rename); a created trait never replaces an existing
// file (ErrNameClash).
func Apply(dir string, plan Plan, approvedID string, resolutions map[string]string) error {
	if approvedID != plan.ID {
		return fmt.Errorf("approved plan %s, but this plan is %s; %w", approvedID, plan.ID, ErrStale)
	}
	current, err := trait.Load(dir)
	if err != nil && !errors.Is(err, trait.ErrNoDir) {
		return err
	}
	if PlanID(current, plan.Items) != approvedID {
		return ErrStale
	}
	changes, err := plan.Changes(resolutions)
	if err != nil {
		return err
	}
	for _, c := range changes {
		target := filepath.Join(dir, filepath.FromSlash(c.Path))
		switch c.Action {
		case "create":
			err = writeNew(target, trait.Serialize(*c.Trait))
		case "update":
			err = replaceFile(target, trait.Serialize(*c.Trait))
		case "delete":
			err = os.Remove(target)
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", c.Action, c.Path, err)
		}
	}
	return nil
}

// tempFile writes data to a hidden temporary file beside target.
func tempFile(target string, data []byte) (string, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*.tmp")
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// writeNew creates target with data, failing if it exists. The hard link
// makes the no-clobber check and the write one step.
func writeNew(target string, data []byte) error {
	tmp, err := tempFile(target, data)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	err = os.Link(tmp, target)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%w: %s", ErrNameClash, target)
	}
	// Hard links unsupported: check, then rename.
	if _, serr := os.Lstat(target); serr == nil {
		return fmt.Errorf("%w: %s", ErrNameClash, target)
	}
	return os.Rename(tmp, target)
}

func replaceFile(target string, data []byte) error {
	tmp, err := tempFile(target, data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// fileDoc is a trait in JSON: its name, path and complete file text.
type fileDoc struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
	File string `json:"file"`
}

func toDoc(t trait.Trait) fileDoc {
	return fileDoc{Name: t.Name, Path: t.Path, File: string(trait.Serialize(t))}
}

func (d fileDoc) trait() (trait.Trait, error) {
	t, err := trait.Parse("", []byte(d.File))
	if err != nil {
		return trait.Trait{}, fmt.Errorf("trait %s: %w", d.Name, err)
	}
	t.Path = d.Path
	return t, nil
}

type itemJSON struct {
	Outcome   Outcome    `json:"outcome"`
	Proposal  fileDoc    `json:"proposal"`
	Result    *fileDoc   `json:"result,omitempty"`
	Targets   []string   `json:"targets,omitempty"`
	Relations []Relation `json:"relations,omitempty"`
	Reason    string     `json:"reason"`
}

// MarshalJSON encodes traits as {name, path, file} with the complete file
// text, so a plan round-trips exactly.
func (it Item) MarshalJSON() ([]byte, error) {
	j := itemJSON{Outcome: it.Outcome, Proposal: toDoc(it.Proposal), Targets: it.Targets, Relations: it.Relations, Reason: it.Reason}
	if it.Result != nil {
		d := toDoc(*it.Result)
		j.Result = &d
	}
	return json.Marshal(j)
}

// UnmarshalJSON decodes what MarshalJSON encodes, validating the traits.
func (it *Item) UnmarshalJSON(b []byte) error {
	var j itemJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	p, err := j.Proposal.trait()
	if err != nil {
		return err
	}
	*it = Item{Outcome: j.Outcome, Proposal: p, Targets: j.Targets, Relations: j.Relations, Reason: j.Reason}
	if j.Result != nil {
		r, err := j.Result.trait()
		if err != nil {
			return err
		}
		it.Result = &r
	}
	return nil
}
