package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/talbet/traits/internal/derive"
	"github.com/talbet/traits/internal/llm"
	"github.com/talbet/traits/internal/trait"
)

// deriveResolveFlag collects repeated --resolve name=choice flags.
type deriveResolveFlag map[string]string

func (r deriveResolveFlag) String() string { return "" }

func (r deriveResolveFlag) Set(v string) error {
	name, choice, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("want NAME=replace|keep|both, got %q", v)
	}
	switch choice {
	case derive.Replace, derive.Keep, derive.Both:
	default:
		return fmt.Errorf("%s: choose replace, keep or both, not %q", name, choice)
	}
	r[name] = choice
	return nil
}

type deriveFlags struct {
	rules     string
	provider  string
	model     string
	timeout   time.Duration
	retries   int
	feedback  string
	diffFile  string
	approve   string
	format    string
	resolve   deriveResolveFlag
	rulesSet  bool
	source    string // FILE.md, "-", or "" with --from-feedback
	hasSource bool
}

var derivePlanIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// runDerive implements `traits derive`: assess a proposed trait against the
// stored traits and print a plan, or with --approve apply a plan whose store
// snapshot is unchanged. Each plan is saved under <traits dir>/.cache/plans,
// so --approve applies exactly what was shown (including an LLM draft or
// split) without asking any model again; when the saved plan is missing,
// the given input is re-assessed and must produce the same plan ID.
func runDerive(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f := deriveFlags{resolve: deriveResolveFlag{}}
	fs := flag.NewFlagSet("derive", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.rules, "rules", env("TRAITS_DIR", ""), "traits directory (default .agents/traits at the repository root)")
	fs.StringVar(&f.provider, "provider", env("TRAITS_PROVIDER", "openai"), "decision-model provider")
	fs.StringVar(&f.model, "model", env("TRAITS_MODEL", ""), "model name (default: the provider's)")
	fs.DurationVar(&f.timeout, "timeout", 60*time.Second, "per-request decision timeout, including retries")
	fs.IntVar(&f.retries, "retries", 2, "retries on rate limits and server errors")
	fs.StringVar(&f.feedback, "from-feedback", "", "draft the proposal from this feedback with the LLM")
	fs.StringVar(&f.diffFile, "diff-file", "", "with --from-feedback: a diff showing the correction")
	fs.StringVar(&f.approve, "approve", "", "apply the plan with this ID")
	fs.StringVar(&f.format, "format", "text", "output format: text, json")
	fs.Var(f.resolve, "resolve", "resolve a conflict: NAME=replace|keep|both (repeatable)")

	// Accept flags after the positional argument too.
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return exitUsage, usageError{err}
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "rules" {
			f.rulesSet = true
		}
	})
	switch {
	case len(pos) > 1:
		return exitUsage, usageError{errors.New("derive takes one proposal: FILE.md, - or --from-feedback")}
	case len(pos) == 1 && f.feedback != "":
		return exitUsage, usageError{errors.New("give a proposal file or --from-feedback, not both")}
	case f.diffFile != "" && f.feedback == "":
		return exitUsage, usageError{errors.New("--diff-file needs --from-feedback")}
	case f.approve == "" && len(pos) == 0 && f.feedback == "":
		return exitUsage, usageError{errors.New("nothing to derive: give FILE.md, - (stdin) or --from-feedback TEXT")}
	case f.approve != "" && !derivePlanIDPattern.MatchString(f.approve):
		return exitUsage, usageError{fmt.Errorf("--approve %q is not a plan ID (12 hex characters)", f.approve)}
	case f.retries < 0:
		return exitUsage, usageError{errors.New("--retries must not be negative")}
	}
	if len(pos) == 1 {
		f.source, f.hasSource = pos[0], true
	}
	f.hasSource = f.hasSource || f.feedback != ""
	switch f.format {
	case "text", "json":
	default:
		return exitUsage, usageError{fmt.Errorf("unknown --format %q", f.format)}
	}

	root := repoRoot("")
	dir := traitsDir(f.rules, root)
	cache := filepath.Join(dir, ".cache", "plans")

	var plan derive.Plan
	loaded := false
	if f.approve != "" {
		if data, err := os.ReadFile(filepath.Join(cache, f.approve+".json")); err == nil {
			if err := json.Unmarshal(data, &plan); err != nil {
				return exitUsage, usageError{fmt.Errorf("saved plan %s: %w", f.approve, err)}
			}
			loaded = true
		} else if !f.hasSource {
			return exitUsage, usageError{fmt.Errorf("no saved plan %s in %s; re-run derive without --approve", f.approve, cache)}
		}
	}
	if !loaded {
		p, code, err := deriveAssess(ctx, f, dir, stdin, stderr)
		if err != nil || code != exitOK {
			return code, err
		}
		plan = p
	}

	if f.approve != "" {
		return deriveApply(f, dir, root, cache, plan, stdout, stderr)
	}

	if data, err := json.MarshalIndent(plan, "", "  "); err == nil {
		if err := os.MkdirAll(cache, 0o755); err == nil {
			err = os.WriteFile(filepath.Join(cache, plan.ID+".json"), data, 0o644)
		}
		if err != nil {
			fmt.Fprintf(stderr, "traits: warning: could not save the plan (%v); --approve will re-assess the input\n", err)
		}
	}
	blocked := plan.Blocked(f.resolve)
	changes, changesErr := plan.Changes(f.resolve)
	approveCmd := deriveApproveCommand(f, plan)
	if f.format == "json" {
		out := map[string]any{"planId": plan.ID, "items": plan.Items, "blocked": deriveNonNil(blocked), "changes": deriveNonNil(changes)}
		if changesErr == nil {
			out["approve"] = approveCmd
		} else if len(blocked) == 0 {
			out["error"] = changesErr.Error()
		}
		if err := deriveWriteJSON(stdout, out); err != nil {
			return exitProvider, err
		}
	} else {
		derivePrintPlan(stdout, plan, dir, root, blocked, changes, changesErr, approveCmd)
	}
	if changesErr != nil {
		return exitFindings, nil
	}
	return exitOK, nil
}

// deriveAssess reads the proposal and builds the plan. A non-zero code with a nil
// error means a message was already printed.
func deriveAssess(ctx context.Context, f deriveFlags, dir string, stdin io.Reader, stderr io.Writer) (derive.Plan, int, error) {
	existing, err := trait.Load(dir)
	if err != nil && !errors.Is(err, trait.ErrNoDir) {
		return derive.Plan{}, exitUsage, usageError{err}
	}

	var lm derive.LanguageModel
	if c, err := llm.FromEnv(); err == nil {
		c.Retries = f.retries
		lm = c
	}

	var proposal trait.Trait
	if f.feedback != "" {
		if lm == nil {
			return derive.Plan{}, exitUsage, usageError{fmt.Errorf("--from-feedback drafts with an LLM: %w", llm.ErrNotConfigured)}
		}
		var diff []byte
		if f.diffFile != "" {
			if diff, err = deriveReadSource(f.diffFile, stdin); err != nil {
				return derive.Plan{}, exitUsage, usageError{err}
			}
		}
		if proposal, err = derive.Draft(ctx, lm, f.feedback, string(diff)); err != nil {
			return derive.Plan{}, exitProvider, err
		}
	} else {
		data, err := deriveReadSource(f.source, stdin)
		if err != nil {
			return derive.Plan{}, exitUsage, usageError{err}
		}
		if proposal, err = trait.Parse("", data); err != nil {
			return derive.Plan{}, exitUsage, usageError{fmt.Errorf("%s: %w", f.source, err)}
		}
	}

	model, err := openModel(f.provider, f.model, f.timeout, f.retries)
	if err != nil {
		return derive.Plan{}, exitUsage, usageError{err}
	}
	plan, err := derive.Derive(ctx, model, lm, existing, proposal)
	switch {
	case errors.Is(err, derive.ErrNeedLLM):
		return derive.Plan{}, exitUsage, usageError{fmt.Errorf("%w (%s)", err, llm.ErrNotConfigured)}
	case errors.Is(err, derive.ErrNameClash):
		fmt.Fprintln(stderr, "traits:", err)
		return derive.Plan{}, exitFindings, nil
	case err != nil:
		return derive.Plan{}, exitProvider, err
	}
	return plan, exitOK, nil
}

func deriveApply(f deriveFlags, dir, root, cache string, plan derive.Plan, stdout, stderr io.Writer) (int, error) {
	if err := derive.Apply(dir, plan, f.approve, f.resolve); err != nil {
		fmt.Fprintln(stderr, "traits:", err)
		return exitFindings, nil
	}
	changes, _ := plan.Changes(f.resolve) // Apply succeeded, so this does too
	os.Remove(filepath.Join(cache, plan.ID+".json"))
	if f.format == "json" {
		return exitOK, deriveWriteJSON(stdout, map[string]any{"planId": plan.ID, "applied": true, "changes": deriveNonNil(changes)})
	}
	if len(changes) == 0 {
		fmt.Fprintf(stdout, "plan %s applied: nothing to write\n", plan.ID)
		return exitOK, nil
	}
	fmt.Fprintf(stdout, "plan %s applied:\n", plan.ID)
	for _, c := range changes {
		fmt.Fprintf(stdout, "  %-7s %s (%s)\n", c.Action+"d", c.Name, relPath(root, filepath.Join(dir, c.Path)))
	}
	return exitOK, nil
}

func derivePrintPlan(w io.Writer, plan derive.Plan, dir, root string, blocked []string, changes []derive.Change, changesErr error, approveCmd string) {
	fmt.Fprintf(w, "plan %s\n", plan.ID)
	for _, it := range plan.Items {
		fmt.Fprintf(w, "\n%s %s", it.Outcome, it.Proposal.Name)
		if len(it.Targets) > 0 {
			fmt.Fprintf(w, " (%s)", strings.Join(it.Targets, ", "))
		}
		fmt.Fprintf(w, "\n  %s\n", it.Reason)
		for _, r := range it.Relations {
			fmt.Fprintf(w, "  %s: %s %.2f", r.Trait, r.Relationship, r.Confidence)
			if r.Coverage != "" {
				fmt.Fprintf(w, ", coverage %s %.2f", r.Coverage, r.CoverageConfidence)
			}
			fmt.Fprintln(w)
		}
		show := it.Proposal
		if it.Result != nil {
			show = *it.Result
			fmt.Fprintf(w, "  %s:\n", relPath(root, filepath.Join(dir, show.Path)))
		}
		for _, line := range strings.Split(strings.TrimRight(string(trait.Serialize(show)), "\n"), "\n") {
			fmt.Fprintln(w, "  | "+line)
		}
	}
	fmt.Fprintln(w)
	switch {
	case len(blocked) > 0:
		fmt.Fprintln(w, "needs attention:")
		for _, b := range blocked {
			fmt.Fprintln(w, "  "+b)
		}
	case changesErr != nil:
		fmt.Fprintln(w, "cannot apply:", changesErr)
	default:
		if len(changes) == 0 {
			fmt.Fprintln(w, "on approval: nothing to write")
		} else {
			fmt.Fprintln(w, "on approval:")
			for _, c := range changes {
				fmt.Fprintf(w, "  %-6s %s\n", c.Action, relPath(root, filepath.Join(dir, c.Path)))
			}
		}
		fmt.Fprintf(w, "to approve:\n  %s\n", approveCmd)
	}
}

// deriveApproveCommand is the command that applies plan. The saved plan carries
// the proposal, so the input is not repeated.
func deriveApproveCommand(f deriveFlags, plan derive.Plan) string {
	parts := []string{"traits", "derive"}
	if f.rulesSet && f.rules != "" {
		parts = append(parts, "--rules", deriveShellQuote(f.rules))
	}
	parts = append(parts, "--approve", plan.ID)
	names := make([]string, 0, len(f.resolve))
	for n := range f.resolve {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		parts = append(parts, "--resolve", deriveShellQuote(n+"="+f.resolve[n]))
	}
	return strings.Join(parts, " ")
}

var deriveShellSafe = regexp.MustCompile(`^[A-Za-z0-9_./=:@%+-]+$`)

func deriveShellQuote(s string) string {
	if deriveShellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func deriveReadSource(name string, stdin io.Reader) ([]byte, error) {
	if name == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(name)
}

func deriveWriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func deriveNonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
