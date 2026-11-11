// Package check evaluates changed files against traits with a decision model
// and combines the answers into a Report.
package check

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/talbet/traits/internal/compile"
	"github.com/talbet/traits/internal/diff"
	"github.com/talbet/traits/internal/provider"
	"github.com/talbet/traits/internal/trait"
)

// Cache stores answers between runs. Keys already include the provider,
// model, compiler version, trait text and input, so a hit is always valid.
type Cache interface {
	Get(key string) (provider.Answer, bool)
	Put(key string, a provider.Answer)
}

// Options configures a run.
type Options struct {
	Model        provider.DecisionModel
	ProviderName string
	ModelName    string
	Traits       []trait.Trait
	// Jobs is how many files are checked concurrently (default 8).
	Jobs int
	// ReadFile returns a file's post-change content, for scope: file traits.
	// When nil those traits are skipped with a file error.
	ReadFile func(path string) ([]byte, error)
	// Cache is optional.
	Cache Cache
}

// defaultMaxInputBytes bounds one request's input when the provider declares
// no limit. crosshatch/traits used 24 KB for input plus questions.
const defaultMaxInputBytes = 16_000

// Run checks files and returns the report. It returns an error only for a
// failure of the provider itself or a cancelled context; problems with one
// file are reported in Report.Errors.
func Run(ctx context.Context, opts Options, files []diff.File) (Report, error) {
	rep := Report{
		Version:  1,
		Provider: opts.ProviderName,
		Model:    opts.ModelName,
		Traits:   map[string]TraitInfo{},
		Files:    []FileResult{},
		Errors:   []FileError{},
	}
	jobs := cmp.Or(opts.Jobs, 8)
	if opts.Model.Limits().Serial {
		jobs = 1
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, jobs)
	)
	for _, f := range files {
		if f.Binary || f.Deleted() || len(f.Hunks) == 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fr, err := checkFile(ctx, opts, f)
			mu.Lock()
			defer mu.Unlock()
			rep.merge(fr)
			if err != nil {
				var pe *provider.Error
				if errors.As(err, &pe) || ctx.Err() != nil {
					cancel(err)
					return
				}
				rep.Errors = append(rep.Errors, FileError{Path: f.Path(), Error: err.Error()})
			}
		}()
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return rep, err
	}

	slices.SortFunc(rep.Files, func(a, b FileResult) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(rep.Errors, func(a, b FileError) int { return strings.Compare(a.Path, b.Path) })
	for _, fr := range rep.Files {
		for _, fd := range fr.Findings {
			t := traitByName(opts.Traits, fd.Trait)
			rep.Traits[fd.Trait] = TraitInfo{Requirement: t.Requirement, Purpose: t.Purpose, Fix: t.Fix, Path: t.Path}
		}
	}
	rep.Summary = Summarize(rep, len(opts.Traits))
	return rep, nil
}

// fileResult is one file's contribution to the report.
type fileResult struct {
	path     string
	findings []Finding
	skipped  Skipped
	usage    Usage
}

func (r *Report) merge(fr fileResult) {
	if len(fr.findings) > 0 {
		r.Files = append(r.Files, FileResult{Path: fr.path, Findings: fr.findings})
	}
	r.Skipped.Gated += fr.skipped.Gated
	r.Skipped.NotApplicable += fr.skipped.NotApplicable
	r.Skipped.Compliant += fr.skipped.Compliant
	r.Skipped.Cached += fr.skipped.Cached
	r.Usage.Requests += fr.usage.Requests
	r.Usage.InputTokens += fr.usage.InputTokens
	r.Usage.CachedTokens += fr.usage.CachedTokens
}

// unit is one input sent to the model with the traits asked about it.
type unit struct {
	input  compile.Input
	traits []trait.Trait
}

func checkFile(ctx context.Context, opts Options, f diff.File) (fileResult, error) {
	path := f.Path()
	fr := fileResult{path: path}
	changed := f.ChangedText()

	var hunkTraits, fileTraits []trait.Trait
	for _, t := range opts.Traits {
		if t.Status != trait.StatusActive || !t.Matches(path) || !t.Gate(changed) {
			fr.skipped.Gated++
			continue
		}
		if t.Scope == trait.ScopeFile {
			fileTraits = append(fileTraits, t)
		} else {
			hunkTraits = append(hunkTraits, t)
		}
	}

	limits := opts.Model.Limits()
	maxBytes := cmp.Or(limits.MaxInputBytes, defaultMaxInputBytes)

	var units []unit
	var errs []error
	if len(hunkTraits) > 0 {
		groups, err := groupHunks(path, f.Hunks, maxBytes)
		if err != nil {
			errs = append(errs, err)
		}
		for _, g := range groups {
			units = append(units, unit{input: g, traits: hunkTraits})
		}
	}
	if len(fileTraits) > 0 {
		in, err := fileInput(opts.ReadFile, f, maxBytes)
		if err != nil {
			errs = append(errs, fmt.Errorf("%d file-scope trait(s) skipped: %w", len(fileTraits), err))
		} else {
			units = append(units, unit{input: in, traits: fileTraits})
		}
	}

	// best holds, per trait, the most severe answer across units (by tier,
	// then violation probability), and the unit it came from.
	type best struct {
		probs  map[Classification]float64
		unit   int
		cached bool
	}
	bests := map[string]best{}
	var order []string

	for ui, u := range units {
		answers, cached, err := ask(ctx, opts, &fr, u.input, questionsFor(u.traits), limits.MaxQuestions)
		if err != nil {
			return fr, err
		}
		for i, t := range u.traits {
			p := classProbs(answers[i])
			b, seen := bests[t.Name]
			if !seen {
				order = append(order, t.Name)
			}
			if !seen || moreSevere(t, p, b.probs) {
				bests[t.Name] = best{probs: p, unit: ui, cached: cached[i]}
			}
		}
	}

	// Pass 2: localise likely violations within multi-hunk units. An
	// inconclusive answer has no violation to point at.
	locate := map[int][]trait.Trait{}
	for _, name := range order {
		b := bests[name]
		t := traitByName(opts.Traits, name)
		_, review := t.Effective()
		if b.probs[Violation] >= review && len(units[b.unit].input.Hunks) > 1 {
			locate[b.unit] = append(locate[b.unit], t)
		}
	}
	locations := map[string]*Location{}
	for ui, ts := range locate {
		qs := make([]provider.Question, len(ts))
		for i, t := range ts {
			qs[i] = compile.LocateQuestion(t, units[ui].input)
		}
		answers, _, err := ask(ctx, opts, &fr, units[ui].input, qs, limits.MaxQuestions)
		if err != nil {
			return fr, err
		}
		for i, t := range ts {
			locations[t.Name] = hunkLocation(units[ui].input, answers[i])
		}
	}

	for _, name := range order {
		b := bests[name]
		t := traitByName(opts.Traits, name)
		if b.cached {
			fr.skipped.Cached++
		}
		tier := tierOf(t, b.probs)
		if tier == "" {
			if top(b.probs) == NotApplicable {
				fr.skipped.NotApplicable++
			} else {
				fr.skipped.Compliant++
			}
			continue
		}
		loc := locations[name]
		if loc == nil {
			loc = unitLocation(units[b.unit].input)
		}
		cls := top(b.probs)
		if tier == TierFix || cls != Inconclusive {
			cls = Violation
		}
		fd := Finding{
			Trait:          name,
			Tier:           tier,
			Classification: cls,
			Probabilities:  b.probs,
			Confidence:     b.probs[cls],
			Location:       loc,
			Cached:         b.cached,
		}
		fd.Interpretation = interpret(t, fd)
		fr.findings = append(fr.findings, fd)
	}
	slices.SortStableFunc(fr.findings, func(a, b Finding) int {
		if a.Tier != b.Tier {
			if a.Tier == TierFix {
				return -1
			}
			return 1
		}
		return cmp.Compare(b.Probabilities[Violation], a.Probabilities[Violation])
	})
	return fr, errors.Join(errs...)
}

func questionsFor(ts []trait.Trait) []provider.Question {
	qs := make([]provider.Question, len(ts))
	for i, t := range ts {
		qs[i] = compile.TraitQuestion(t)
	}
	return qs
}

// ask answers qs about in, using the cache and splitting into requests of at
// most maxQuestions. It returns answers in question order and whether each
// came from the cache.
func ask(ctx context.Context, opts Options, fr *fileResult, in compile.Input, qs []provider.Question, maxQuestions int) ([]provider.Answer, []bool, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, nil, err
	}
	answers := make([]provider.Answer, len(qs))
	cached := make([]bool, len(qs))
	keys := make([]string, len(qs))
	var pending []int
	for i, q := range qs {
		keys[i] = cacheKey(opts, raw, q)
		if opts.Cache != nil {
			if a, ok := opts.Cache.Get(keys[i]); ok {
				answers[i], cached[i] = a, true
				continue
			}
		}
		pending = append(pending, i)
	}
	size := cmp.Or(maxQuestions, len(pending))
	for chunk := range slices.Chunk(pending, max(size, 1)) {
		req := provider.Request{Input: raw, CacheKey: in.Path}
		for _, i := range chunk {
			req.Questions = append(req.Questions, qs[i])
		}
		res, err := opts.Model.Decide(ctx, req)
		if err != nil {
			return nil, nil, err
		}
		if len(res.Answers) != len(chunk) {
			return nil, nil, &provider.Error{Provider: opts.ProviderName, Err: fmt.Errorf("got %d answers for %d questions", len(res.Answers), len(chunk))}
		}
		fr.usage.Requests++
		fr.usage.InputTokens += res.Usage.InputTokens
		fr.usage.CachedTokens += res.Usage.CachedTokens
		for j, i := range chunk {
			answers[i] = res.Answers[j]
			if opts.Cache != nil {
				opts.Cache.Put(keys[i], res.Answers[j])
			}
		}
	}
	return answers, cached, nil
}

func cacheKey(opts Options, input []byte, q provider.Question) string {
	h := sha256.New()
	qj, _ := json.Marshal(q)
	for _, part := range [][]byte{[]byte(opts.ProviderName), []byte(opts.ModelName), []byte(compile.Version), qj, input} {
		fmt.Fprintf(h, "%d:", len(part))
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// groupHunks packs consecutive hunks into inputs whose JSON stays within
// maxBytes. A single hunk that does not fit is an error; the other groups are
// still returned.
func groupHunks(path string, hunks []diff.Hunk, maxBytes int) ([]compile.Input, error) {
	var groups []compile.Input
	var cur []diff.Hunk
	var tooBig []string
	size := func(hs []diff.Hunk) int {
		b, _ := json.Marshal(compile.HunkInput(path, hs))
		return len(b)
	}
	for _, h := range hunks {
		if size([]diff.Hunk{h}) > maxBytes {
			tooBig = append(tooBig, fmt.Sprintf("@@ -%d +%d", h.OldStart, h.NewStart))
			continue
		}
		if len(cur) > 0 && size(append(slices.Clone(cur), h)) > maxBytes {
			groups = append(groups, compile.HunkInput(path, cur))
			cur = nil
		}
		cur = append(cur, h)
	}
	if len(cur) > 0 {
		groups = append(groups, compile.HunkInput(path, cur))
	}
	if len(tooBig) > 0 {
		return groups, fmt.Errorf("hunk(s) %s larger than the %d-byte request budget were not checked", strings.Join(tooBig, ", "), maxBytes)
	}
	return groups, nil
}

func fileInput(read func(string) ([]byte, error), f diff.File, maxBytes int) (compile.Input, error) {
	if read == nil {
		return compile.Input{}, errors.New("the post-change file is not available (pass --repo or file paths)")
	}
	data, err := read(f.Path())
	if err != nil {
		return compile.Input{}, err
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var changed [][2]int
	for _, h := range f.Hunks {
		start, end := 0, 0
		for _, l := range h.Lines {
			if l.Kind == diff.Added {
				if start == 0 {
					start = l.NewLine
				}
				end = l.NewLine
			}
		}
		if start > 0 {
			changed = append(changed, [2]int{start, end})
		}
	}
	in := compile.FileInput(f.Path(), lines, changed)
	b, _ := json.Marshal(in)
	if len(b) > maxBytes {
		return compile.Input{}, fmt.Errorf("file is %d bytes, larger than the %d-byte request budget", len(b), maxBytes)
	}
	return in, nil
}

// classProbs reads the four-way distribution from an answer. A provider that
// reports only the chosen value is treated as giving it all its confidence.
func classProbs(a provider.Answer) map[Classification]float64 {
	p := map[Classification]float64{Compliant: 0, Violation: 0, NotApplicable: 0, Inconclusive: 0}
	if len(a.Probabilities) > 0 {
		for k, v := range a.Probabilities {
			if _, ok := p[Classification(k)]; ok {
				p[Classification(k)] = v
			}
		}
		return p
	}
	if _, ok := p[Classification(a.Value)]; ok {
		p[Classification(a.Value)] = cmp.Or(a.Confidence, 1)
	}
	return p
}

// top returns the most likely classification, preferring the more cautious
// answer on ties (violation, inconclusive, compliant, not-applicable).
func top(p map[Classification]float64) Classification {
	best := Violation
	for _, c := range []Classification{Inconclusive, Compliant, NotApplicable} {
		if p[c] > p[best] {
			best = c
		}
	}
	return best
}

// tierOf returns the finding tier for a trait's answer, or "" when nothing
// should be reported.
func tierOf(t trait.Trait, p map[Classification]float64) Tier {
	fix, review := t.Effective()
	switch {
	case p[Violation] >= fix:
		return TierFix
	case p[Violation] >= review, top(p) == Inconclusive:
		return TierReview
	}
	return ""
}

// moreSevere reports whether answer a should be reported over answer b: a
// higher tier wins, then a higher violation probability.
func moreSevere(t trait.Trait, a, b map[Classification]float64) bool {
	rank := func(p map[Classification]float64) int {
		switch tierOf(t, p) {
		case TierFix:
			return 2
		case TierReview:
			return 1
		}
		return 0
	}
	if ra, rb := rank(a), rank(b); ra != rb {
		return ra > rb
	}
	return a[Violation] > b[Violation]
}

func hunkLocation(in compile.Input, a provider.Answer) *Location {
	for _, h := range in.Hunks {
		if h.ID == a.Value {
			loc := hunkRange(h)
			loc.Probability = cmp.Or(a.Probabilities[a.Value], a.Confidence)
			return loc
		}
	}
	return unitLocation(in)
}

// unitLocation is the location for a unit with one hunk, or the changed
// ranges' span for a whole-file unit. It returns nil when there is no
// single place to point at.
func unitLocation(in compile.Input) *Location {
	if len(in.Hunks) == 1 {
		loc := hunkRange(in.Hunks[0])
		loc.Probability = 1
		return loc
	}
	if len(in.Hunks) == 0 && len(in.Changed) == 1 {
		return &Location{StartLine: in.Changed[0][0], EndLine: in.Changed[0][1], Probability: 1}
	}
	return nil
}

func hunkRange(h compile.Hunk) *Location {
	return &Location{Hunk: h.ID, StartLine: max(h.Start, 1), EndLine: max(h.End, h.Start, 1)}
}

func interpret(t trait.Trait, f Finding) string {
	var b strings.Builder
	where := ""
	if f.Location != nil {
		if f.Location.StartLine == f.Location.EndLine {
			where = fmt.Sprintf(" at line %d", f.Location.StartLine)
		} else {
			where = fmt.Sprintf(" in lines %d-%d", f.Location.StartLine, f.Location.EndLine)
		}
	}
	switch {
	case f.Classification == Inconclusive:
		fmt.Fprintf(&b, "Could not judge %s from this change%s (inconclusive %.2f); check by hand.", t.Name, where, f.Confidence)
	case f.Tier == TierFix:
		fmt.Fprintf(&b, "Likely violates %s%s (%.2f).", t.Name, where, f.Probabilities[Violation])
	default:
		fmt.Fprintf(&b, "May violate %s%s (%.2f).", t.Name, where, f.Probabilities[Violation])
	}
	fmt.Fprintf(&b, " Requirement: %s", sentence(t.Requirement))
	fmt.Fprintf(&b, " Why: %s", sentence(t.Purpose))
	if t.Fix != "" {
		fmt.Fprintf(&b, " Fix: %s", sentence(t.Fix))
	}
	return b.String()
}

// sentence collapses whitespace and ensures terminal punctuation.
func sentence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s != "" && !strings.ContainsAny(s[len(s)-1:], ".!?") {
		s += "."
	}
	return s
}

// Summarize returns the one-line summary of r for a run over traits traits.
func Summarize(r Report, traits int) string {
	var fix, review int
	for _, f := range r.Files {
		for _, fd := range f.Findings {
			if fd.Tier == TierFix {
				fix++
			} else {
				review++
			}
		}
	}
	s := fmt.Sprintf("%d to fix, %d to double-check in %d file(s); %d trait(s), %d request(s).",
		fix, review, len(r.Files), traits, r.Usage.Requests)
	if len(r.Errors) > 0 {
		s += fmt.Sprintf(" %d file(s) could not be fully checked.", len(r.Errors))
	}
	return s
}

func traitByName(ts []trait.Trait, name string) trait.Trait {
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return trait.Trait{Name: name}
}
