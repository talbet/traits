package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/talbet/traits/internal/check"
	"github.com/talbet/traits/internal/diff"
	"github.com/talbet/traits/internal/eval"
	"github.com/talbet/traits/internal/state"
	"github.com/talbet/traits/internal/trait"
)

type evalFlags struct {
	rules     string
	provider  string
	model     string
	split     string
	traits    listFlag
	compare   string
	failUnder float64
	format    string
	promote   bool
	yes       bool
	noCache   bool
	jobs      int
	timeout   time.Duration
	retries   int

	set map[string]bool // flags given on the command line
}

// listFlag is a repeatable flag; each value may also be comma-separated.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for s := range strings.SplitSeq(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

// evalCase is a labelled case bound to its trait and ready to ask.
type evalCase struct {
	eval.Case
	trait int // index into the selected traits
	file  diff.File
}

// evalReport is the JSON output of eval (version 1).
type evalReport struct {
	Version   int       `json:"version"`
	Split     string    `json:"split"`
	FailUnder *float64  `json:"failUnderF1,omitempty"`
	Cases     int       `json:"cases"`
	Runs      []evalRun `json:"runs"`
	// Errors are cases that could not be asked, per run.
	Errors     []caseError `json:"errors"`
	Promotions []promotion `json:"promotions,omitempty"`
	// Failed lists "provider:model/trait" whose fix-threshold F1 fell below
	// --fail-under-f1 (only when that flag was given).
	Failed []string `json:"failed,omitempty"`
}

type evalRun struct {
	Provider string             `json:"provider"`
	Model    string             `json:"model"`
	Traits   []eval.TraitResult `json:"traits"`
	Overall  eval.TraitResult   `json:"overall"`
	Usage    check.Usage        `json:"usage"`

	outcomes [][]eval.Outcome // per selected trait
}

func (r evalRun) label() string { return r.Provider + ":" + r.Model }

type caseError struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Trait    string `json:"trait"`
	Case     string `json:"case"`
	Error    string `json:"error"`
}

type promotion struct {
	Trait     string   `json:"trait"`
	Path      string   `json:"path"`
	HoldoutF1 *float64 `json:"holdoutF1"`
	Threshold float64  `json:"fixThreshold"`
	Positives int      `json:"positives"`
	Negatives int      `json:"negatives"`
	Promote   bool     `json:"promote"`
	Reason    string   `json:"reason"`
	Written   bool     `json:"written"`
}

func runEval(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	_ = stdin
	var f evalFlags
	fl := flag.NewFlagSet("eval", flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.StringVar(&f.rules, "rules", env("TRAITS_DIR", ""), "traits directory (default .agents/traits at the repository root)")
	fl.StringVar(&f.provider, "provider", env("TRAITS_PROVIDER", "openai"), "decision-model provider")
	fl.StringVar(&f.model, "model", env("TRAITS_MODEL", ""), "model name (default: the provider's)")
	fl.StringVar(&f.split, "split", "", "cases to evaluate: dev (default), holdout (default with --promote), all")
	fl.Var(&f.traits, "trait", "evaluate only this trait (repeatable, or comma-separated)")
	fl.StringVar(&f.compare, "compare", "", "compare provider:model pairs, comma-separated (instead of --provider/--model)")
	fl.Float64Var(&f.failUnder, "fail-under-f1", 0.9, "exit 1 when a trait's F1 at its fix threshold is below this; also the --promote bar")
	fl.StringVar(&f.format, "format", "text", "output format: text, json")
	fl.BoolVar(&f.promote, "promote", false, "show candidate traits whose holdout F1 meets --fail-under-f1 (dry run)")
	fl.BoolVar(&f.yes, "yes", false, "with --promote, set those traits to status active")
	fl.BoolVar(&f.noCache, "no-cache", false, "do not read or write the answer cache")
	fl.IntVar(&f.jobs, "jobs", 8, "cases evaluated concurrently")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Second, "per-request timeout, including retries")
	fl.IntVar(&f.retries, "retries", 2, "retries on rate limits and server errors")
	if err := fl.Parse(args); err != nil {
		return exitUsage, usageError{err}
	}
	f.set = map[string]bool{}
	fl.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	if fl.NArg() > 0 {
		return exitUsage, usageError{fmt.Errorf("unexpected argument %q (use --trait to select traits)", fl.Arg(0))}
	}
	specs, err := validateEvalFlags(&f)
	if err != nil {
		return exitUsage, usageError{err}
	}

	dir := traitsDir(f.rules, repoRoot(""))
	all, err := trait.Load(dir)
	if err != nil {
		return exitUsage, usageError{err}
	}
	traits, err := selectTraits(all, f.traits)
	if err != nil {
		return exitUsage, usageError{err}
	}
	cases, err := loadCases(dir, traits, f.split, f.traits, stderr)
	if err != nil {
		return exitUsage, usageError{err}
	}
	warnOrphanCases(dir, all, stderr)

	var cache check.Cache
	if !f.noCache {
		if c, err := state.OpenCache(filepath.Join(dir, ".cache")); err == nil {
			cache = c
		}
	}

	rep := evalReport{Version: 1, Split: f.split, Cases: len(cases), Runs: []evalRun{}, Errors: []caseError{}}
	if f.set["fail-under-f1"] {
		rep.FailUnder = &f.failUnder
	}
	for _, s := range specs {
		model, err := openModel(s[0], s[1], f.timeout, f.retries)
		if err != nil {
			return exitUsage, usageError{err}
		}
		run, errs, err := evalModel(ctx, model, traits, cases, cache, f.jobs)
		if err != nil {
			return exitProvider, err
		}
		rep.Runs = append(rep.Runs, run)
		rep.Errors = append(rep.Errors, errs...)
	}

	failedErrors := false
	if f.set["fail-under-f1"] {
		for _, run := range rep.Runs {
			for _, tr := range run.Traits {
				if tr.Fix.F1 != nil && *tr.Fix.F1 < f.failUnder {
					rep.Failed = append(rep.Failed, run.label()+"/"+tr.Trait)
				}
			}
		}
		failedErrors = len(rep.Errors) > 0
	}

	if f.promote {
		rep.Promotions, err = promote(dir, traits, rep.Runs[0], f.failUnder, f.yes)
		if err != nil {
			return exitProvider, err
		}
	}

	if f.format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(rep); err != nil {
			return exitProvider, err
		}
	} else {
		renderEvalText(stdout, rep, f)
	}
	for _, e := range rep.Errors {
		fmt.Fprintf(stderr, "traits: eval: %s:%s %s %s: %s\n", e.Provider, e.Model, e.Trait, e.Case, e.Error)
	}
	if len(rep.Failed) > 0 {
		fmt.Fprintf(stderr, "traits: eval: F1 at the fix threshold below %.2f: %s\n", f.failUnder, strings.Join(rep.Failed, ", "))
	}
	if len(rep.Failed) > 0 || failedErrors {
		return exitFindings, nil
	}
	return exitOK, nil
}

// validateEvalFlags checks f, fills in the default split, and returns the
// provider/model pairs to run.
func validateEvalFlags(f *evalFlags) ([][2]string, error) {
	switch f.split {
	case "":
		f.split = "dev"
		if f.promote {
			f.split = "holdout"
		}
	case "dev", "holdout", "all":
	default:
		return nil, fmt.Errorf("unknown --split %q (want dev, holdout or all)", f.split)
	}
	switch f.format {
	case "text", "json":
	default:
		return nil, fmt.Errorf("unknown --format %q (want text or json)", f.format)
	}
	switch {
	case f.yes && !f.promote:
		return nil, errors.New("--yes only applies to --promote")
	case f.promote && f.split == "dev":
		return nil, errors.New("--promote judges holdout cases; use --split holdout or all")
	case f.promote && f.compare != "":
		return nil, errors.New("--promote and --compare are mutually exclusive: promote on one model")
	case f.compare != "" && (f.set["provider"] || f.set["model"]):
		return nil, errors.New("--compare replaces --provider and --model")
	case f.failUnder < 0 || f.failUnder > 1:
		return nil, errors.New("--fail-under-f1 must be between 0 and 1")
	case f.jobs < 1:
		return nil, errors.New("--jobs must be at least 1")
	case f.retries < 0:
		return nil, errors.New("--retries must not be negative")
	}
	if f.compare == "" {
		return [][2]string{{f.provider, f.model}}, nil
	}
	var specs [][2]string
	for s := range strings.SplitSeq(f.compare, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		p, m, _ := strings.Cut(s, ":")
		if p == "" {
			return nil, fmt.Errorf("--compare entry %q has no provider", s)
		}
		specs = append(specs, [2]string{p, m})
	}
	if len(specs) == 0 {
		return nil, errors.New("--compare needs at least one provider:model")
	}
	return specs, nil
}

func selectTraits(all []trait.Trait, names []string) ([]trait.Trait, error) {
	if len(names) == 0 {
		return all, nil
	}
	var out []trait.Trait
	for _, t := range all {
		if slices.Contains(names, t.Name) {
			out = append(out, t)
		}
	}
	for _, n := range names {
		if !slices.ContainsFunc(out, func(t trait.Trait) bool { return t.Name == n }) {
			return nil, fmt.Errorf("no trait named %q", n)
		}
	}
	return out, nil
}

// loadCases reads every selected trait's sidecar cases, validating all of
// them, and returns those in split. Traits named with --trait that have no
// cases in the split are reported on stderr.
func loadCases(dir string, traits []trait.Trait, split string, named []string, stderr io.Writer) ([]evalCase, error) {
	var out []evalCase
	var errs []error
	var none []string
	for i, t := range traits {
		rel := eval.CasesPath(t)
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if errors.Is(err, fs.ErrNotExist) {
			none = append(none, t.Name)
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		cs, err := eval.Parse(rel, strings.NewReader(string(data)))
		if err != nil {
			errs = append(errs, err)
		}
		n := 0
		for _, c := range cs {
			if split != "all" && string(c.Split) != split {
				continue
			}
			ec := evalCase{Case: c, trait: i, file: c.File}
			if c.Content != nil {
				ec.file = wholeFile(c.Path, []byte(*c.Content))
			}
			out = append(out, ec)
			n++
		}
		if n == 0 && len(cs) > 0 && slices.Contains(named, t.Name) {
			fmt.Fprintf(stderr, "traits: eval: %s has no %s cases\n", t.Name, split)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if len(none) > 0 {
		fmt.Fprintf(stderr, "traits: eval: no cases file for %d trait(s): %s\n", len(none), strings.Join(none, ", "))
	}
	return out, nil
}

// warnOrphanCases reports cases files with no trait beside them.
func warnOrphanCases(dir string, traits []trait.Trait, stderr io.Writer) {
	known := map[string]bool{}
	for _, t := range traits {
		known[eval.CasesPath(t)] = true
	}
	fsys := os.DirFS(dir)
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, eval.CasesSuffix) && !known[p] {
			fmt.Fprintf(stderr, "traits: eval: warning: %s has no matching trait file\n", p)
		}
		return nil
	})
}

// evalModel asks model every case's trait question and scores the answers.
// The error is a provider failure or cancellation.
func evalModel(ctx context.Context, model resolvedModel, traits []trait.Trait, cases []evalCase, cache check.Cache, jobs int) (evalRun, []caseError, error) {
	run := evalRun{Provider: model.Provider, Model: model.Name, outcomes: make([][]eval.Outcome, len(traits))}
	if model.Limits().Serial {
		jobs = 1
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	outcomes := make([]eval.Outcome, len(cases))
	usages := make([]check.Usage, len(cases))
	fileErrs := make([]error, len(cases))
	var wg sync.WaitGroup
	sem := make(chan struct{}, jobs)
	for i, c := range cases {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			opts := check.Options{
				Model:        model.DecisionModel,
				ProviderName: model.Provider,
				ModelName:    model.Name,
				Traits:       []trait.Trait{traits[c.trait]},
				Cache:        cache,
			}
			if post, ok := c.PostImage(); ok {
				opts.ReadFile = func(string) ([]byte, error) { return post, nil }
			}
			answers, usage, fileErr, err := check.Evaluate(ctx, opts, c.file)
			if err != nil {
				cancel(err)
				return
			}
			o := eval.Outcome{Positive: c.Positive(), Split: c.Split, Gated: answers[0].Gated, Probabilities: answers[0].Probabilities}
			if fileErr != nil && !o.Gated {
				o.Error, fileErrs[i] = true, fileErr
			}
			outcomes[i], usages[i] = o, usage
		}()
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return run, nil, err
	}

	usageBy := make([]check.Usage, len(traits))
	var errs []caseError
	for i, c := range cases {
		run.outcomes[c.trait] = append(run.outcomes[c.trait], outcomes[i])
		u := &usageBy[c.trait]
		u.Requests += usages[i].Requests
		u.InputTokens += usages[i].InputTokens
		u.CachedTokens += usages[i].CachedTokens
		if fileErrs[i] != nil {
			errs = append(errs, caseError{Provider: model.Provider, Model: model.Name, Trait: traits[c.trait].Name, Case: c.Label(), Error: fileErrs[i].Error()})
		}
	}
	for i, t := range traits {
		if len(run.outcomes[i]) == 0 {
			continue
		}
		fix, review := t.Effective()
		tr := eval.Summarize(t.Name, fix, review, run.outcomes[i])
		tr.Status = string(t.Status)
		tr.Usage = usageBy[i]
		run.Traits = append(run.Traits, tr)
	}
	if run.Traits == nil {
		run.Traits = []eval.TraitResult{}
	}
	run.Overall = eval.Overall(run.Traits)
	run.Usage = run.Overall.Usage
	return run, errs, nil
}

// promote decides, for each candidate trait, whether its holdout F1 at the
// fix threshold reaches bar with at least one positive and one negative
// holdout case, and with write set rewrites those traits as active.
// Thresholds are never changed.
func promote(dir string, traits []trait.Trait, run evalRun, bar float64, write bool) ([]promotion, error) {
	ps := []promotion{}
	for i, t := range traits {
		if t.Status != trait.StatusCandidate {
			continue
		}
		var holdout []eval.Outcome
		for _, o := range run.outcomes[i] {
			if o.Split == eval.SplitHoldout {
				holdout = append(holdout, o)
			}
		}
		fix, review := t.Effective()
		r := eval.Summarize(t.Name, fix, review, holdout)
		p := promotion{Trait: t.Name, Path: t.Path, HoldoutF1: r.Fix.F1, Threshold: fix, Positives: r.Positives, Negatives: r.Negatives}
		switch {
		case r.Positives == 0 || r.Negatives == 0:
			p.Reason = fmt.Sprintf("needs at least 1 positive and 1 negative holdout case (has %d and %d)", r.Positives, r.Negatives)
		case r.Errors > 0:
			p.Reason = fmt.Sprintf("%d holdout case(s) could not be evaluated", r.Errors)
		case r.Fix.F1 == nil || *r.Fix.F1 < bar:
			p.Reason = fmt.Sprintf("holdout F1 %s at fix %.2f is below %.2f", fmtRate(r.Fix.F1), fix, bar)
		default:
			p.Promote = true
			p.Reason = fmt.Sprintf("holdout F1 %s at fix %.2f meets %.2f", fmtRate(r.Fix.F1), fix, bar)
		}
		if p.Promote && write {
			path := filepath.Join(dir, filepath.FromSlash(t.Path))
			perm := fs.FileMode(0o644)
			if info, err := os.Stat(path); err == nil {
				perm = info.Mode().Perm()
			}
			t.Status = trait.StatusActive
			if err := os.WriteFile(path, trait.Serialize(t), perm); err != nil {
				return ps, err
			}
			p.Written = true
		}
		ps = append(ps, p)
	}
	return ps, nil
}

func fmtRate(v *float64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

func renderEvalText(w io.Writer, rep evalReport, f evalFlags) {
	if rep.Cases == 0 {
		fmt.Fprintf(w, "No %s cases to evaluate.\n", rep.Split)
	} else if len(rep.Runs) == 1 {
		renderRun(w, rep.Runs[0], rep.Split)
	} else {
		renderCompare(w, rep)
	}
	for _, p := range rep.Promotions {
		switch {
		case p.Written:
			fmt.Fprintf(w, "promoted %s to active (%s): wrote %s\n", p.Trait, p.Reason, p.Path)
		case p.Promote:
			fmt.Fprintf(w, "would promote %s to active (%s); rerun with --yes to write %s\n", p.Trait, p.Reason, p.Path)
		default:
			fmt.Fprintf(w, "%s stays candidate: %s\n", p.Trait, p.Reason)
		}
	}
	if f.promote && len(rep.Promotions) == 0 {
		fmt.Fprintln(w, "No candidate traits to promote.")
	}
}

func renderRun(w io.Writer, run evalRun, split string) {
	u := run.Usage
	fmt.Fprintf(w, "%s  split %s  %d request(s), %d input token(s), %d cached\n\n", run.label(), split, u.Requests, u.InputTokens, u.CachedTokens)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TRAIT\tAT\tTHR\tCASES\tPOS\tTP\tFP\tFN\tTN\tPREC\tRECALL\tF1\tFALSE-ALARM\tINCONCL\tGATED\tERR")
	row := func(name string, tr eval.TraitResult) {
		cases, pos, errs := strconv.Itoa(tr.Cases), strconv.Itoa(tr.Positives), strconv.Itoa(tr.Errors)
		inconcl := fmtRate(tr.InconclusiveRate)
		gated := strconv.Itoa(tr.Gated)
		if tr.GatedPositives > 0 {
			gated += fmt.Sprintf(" (%d pos!)", tr.GatedPositives)
		}
		for _, at := range []struct {
			name string
			r    eval.Rates
		}{{"review", tr.Review}, {"fix", tr.Fix}} {
			thr := "-"
			if at.r.Threshold > 0 {
				thr = strconv.FormatFloat(at.r.Threshold, 'f', 2, 64)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				name, at.name, thr, cases, pos, at.r.TP, at.r.FP, at.r.FN, at.r.TN,
				fmtRate(at.r.Precision), fmtRate(at.r.Recall), fmtRate(at.r.F1), fmtRate(at.r.FalseAlarmRate),
				inconcl, gated, errs)
			// Per-trait counts are shown once, on the first row.
			name, cases, pos, inconcl, gated, errs = "", "", "", "", "", ""
		}
	}
	for _, tr := range run.Traits {
		name := tr.Trait
		if tr.Status == string(trait.StatusCandidate) {
			name += " (candidate)"
		}
		row(name, tr)
	}
	if len(run.Traits) > 1 {
		row("overall", run.Overall)
	}
	tw.Flush()
}

func renderCompare(w io.Writer, rep evalReport) {
	fmt.Fprintf(w, "split %s  %d case(s); F1 precision/recall false-alarm at each trait's fix threshold\n\n", rep.Split, rep.Cases)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := []string{"TRAIT"}
	for _, r := range rep.Runs {
		header = append(header, r.label())
	}
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	cell := func(tr eval.TraitResult) string {
		r := tr.Fix
		return fmt.Sprintf("%s %s/%s %s", fmtRate(r.F1), fmtRate(r.Precision), fmtRate(r.Recall), fmtRate(r.FalseAlarmRate))
	}
	// Every run evaluates the same traits in the same order.
	for i, tr := range rep.Runs[0].Traits {
		line := []string{tr.Trait}
		for _, r := range rep.Runs {
			line = append(line, cell(r.Traits[i]))
		}
		fmt.Fprintln(tw, strings.Join(line, "\t"))
	}
	overall := []string{"overall"}
	reqs := []string{"requests"}
	toks := []string{"tokens (cached)"}
	for _, r := range rep.Runs {
		overall = append(overall, cell(r.Overall))
		reqs = append(reqs, strconv.Itoa(r.Usage.Requests))
		toks = append(toks, fmt.Sprintf("%d (%d)", r.Usage.InputTokens, r.Usage.CachedTokens))
	}
	for _, l := range [][]string{overall, reqs, toks} {
		fmt.Fprintln(tw, strings.Join(l, "\t"))
	}
	tw.Flush()
}
