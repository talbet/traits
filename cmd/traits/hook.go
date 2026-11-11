package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/talbet/traits/internal/check"
	"github.com/talbet/traits/internal/diff"
	"github.com/talbet/traits/internal/hook"
	"github.com/talbet/traits/internal/output"
	"github.com/talbet/traits/internal/state"
	"github.com/talbet/traits/internal/trait"
)

// defaultHookTimeout bounds a whole hook or recheck run.
const defaultHookTimeout = 8 * time.Second

// maxPayload bounds the hook payload read from stdin.
const maxPayload = 16 << 20

// hookFlags are shared by `hook claude|codex` and `recheck`.
type hookFlags struct {
	provider string
	model    string
	rules    string
	timeout  time.Duration
	top      int
}

func parseHookFlags(name string, args []string, stderr io.Writer, withTop bool) (hookFlags, error) {
	var f hookFlags
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // hooks stay silent; errors surface via TRAITS_HOOK_DEBUG
	fs.StringVar(&f.provider, "provider", env("TRAITS_PROVIDER", ""), "decision-model provider (default openai)")
	fs.StringVar(&f.model, "model", env("TRAITS_MODEL", ""), "model name")
	fs.StringVar(&f.rules, "rules", env("TRAITS_DIR", ""), "traits directory")
	fs.DurationVar(&f.timeout, "timeout", hookTimeout(), "hard limit for the whole run ($TRAITS_HOOK_TIMEOUT)")
	if withTop {
		fs.IntVar(&f.top, "top", 5, "most findings reported to the agent")
	}
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if fs.NArg() > 0 {
		return f, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return f, nil
}

// hookTimeout reads TRAITS_HOOK_TIMEOUT as a duration ("8s") or seconds.
func hookTimeout() time.Duration {
	v := os.Getenv("TRAITS_HOOK_TIMEOUT")
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil && s > 0 {
		return time.Duration(s * float64(time.Second))
	}
	return defaultHookTimeout
}

// debugf writes to stderr only when TRAITS_HOOK_DEBUG is set: hooks print
// nothing but their protocol output.
func debugf(stderr io.Writer, format string, args ...any) {
	if os.Getenv("TRAITS_HOOK_DEBUG") != "" {
		fmt.Fprintf(stderr, "traits hook: "+format+"\n", args...)
	}
}

// runHook implements `traits hook claude|codex` (always exit 0, output only
// the hook protocol's JSON) and `traits hook install`.
func runHook(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		return exitUsage, usageError{errors.New("usage: traits hook claude|codex|install")}
	}
	agent := args[0]
	switch agent {
	case "install":
		return runHookInstall(args[1:], stdout, stderr)
	case "claude", "codex":
	default:
		return exitUsage, usageError{fmt.Errorf("unknown hook %q (have claude, codex, install)", agent)}
	}
	f, err := parseHookFlags("hook "+agent, args[1:], stderr, true)
	if err != nil {
		return exitOK, nil // fail open, even on a bad hook command line
	}
	out := failOpen(ctx, f.timeout, stderr, func(ctx context.Context) ([]byte, error) {
		return hookRun(ctx, agent, f, stdin)
	})
	if len(out) > 0 {
		_, _ = stdout.Write(out)
	}
	return exitOK, nil
}

// failOpen runs fn with a hard deadline and returns its output, or nothing
// when it fails, panics or runs out of time.
func failOpen(ctx context.Context, timeout time.Duration, stderr io.Writer, fn func(context.Context) ([]byte, error)) []byte {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: fmt.Errorf("panic: %v", r)}
			}
		}()
		out, err := fn(ctx)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			debugf(stderr, "%v", r.err)
			return nil
		}
		return r.out
	case <-ctx.Done():
		debugf(stderr, "gave up: %v", ctx.Err())
		return nil
	}
}

// hookEnv is the repository and traits a hook run works with.
type hookEnv struct {
	root   string
	dir    string
	traits []trait.Trait
}

func loadHookEnv(cwd, rules string) (hookEnv, error) {
	root := gitRoot(cwd)
	dir := traitsDir(rules, root)
	traits, err := trait.Load(dir)
	if len(traits) == 0 {
		return hookEnv{}, cmp.Or(err, errors.New("no traits"))
	}
	// Traits that failed to parse are skipped; the others still run.
	return hookEnv{root: root, dir: dir, traits: traits}, nil
}

func readPayload(stdin io.Reader) (hook.Payload, error) {
	var p hook.Payload
	data, err := io.ReadAll(io.LimitReader(stdin, maxPayload))
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(data, &p)
}

func hookRun(ctx context.Context, agent string, f hookFlags, stdin io.Reader) ([]byte, error) {
	p, err := readPayload(stdin)
	if err != nil {
		return nil, err
	}
	if p.HookEventName != "" && p.HookEventName != "PostToolUse" {
		return nil, nil
	}
	if p.Cwd == "" {
		p.Cwd, _ = os.Getwd()
	}
	var changes []hook.Change
	if agent == "claude" {
		changes = hook.Claude(p, os.ReadFile)
	} else {
		changes = hook.Codex(p, os.ReadFile)
	}
	if len(changes) == 0 {
		return nil, nil
	}
	he, err := loadHookEnv(p.Cwd, f.rules)
	if err != nil {
		return nil, err
	}
	var files []diff.File
	for _, c := range changes {
		rel, ok := inside(he.root, c.Abs)
		if !ok || strings.HasPrefix(rel, ".agents/traits/") {
			continue
		}
		files = append(files, hook.Relabel(c.File, rel))
	}
	if len(files) == 0 {
		return nil, nil
	}
	model, err := openModel(cmp.Or(f.provider, "openai"), f.model, f.timeout, 0)
	if err != nil {
		return nil, err
	}
	opts := check.Options{
		Model:        model.DecisionModel,
		ProviderName: model.Provider,
		ModelName:    model.Name,
		Traits:       he.traits,
		ReadFile:     repoReader(he.root),
	}
	if c, err := state.OpenCache(filepath.Join(he.dir, ".cache")); err == nil {
		opts.Cache = c
	}
	rep, err := check.Run(ctx, opts, files)
	if err != nil {
		return nil, err
	}
	logFindings(he.dir, rep, files, state.SourceHook, p.SessionID)
	text := hookContext(rep, f.top)
	if text == "" {
		return nil, nil
	}
	return json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PostToolUse",
			"additionalContext": text,
		},
	})
}

// hookContext is the Markdown fed back to the agent: the top findings by
// tier and violation probability, or "" when there are none.
func hookContext(rep check.Report, top int) string {
	type item struct {
		path string
		fd   check.Finding
	}
	var items []item
	for _, fr := range rep.Files {
		for _, fd := range fr.Findings {
			if fd.Tier == check.TierFix || fd.Tier == check.TierReview {
				items = append(items, item{fr.Path, fd})
			}
		}
	}
	if len(items) == 0 {
		return ""
	}
	slices.SortStableFunc(items, func(a, b item) int {
		if a.fd.Tier != b.fd.Tier {
			if a.fd.Tier == check.TierFix {
				return -1
			}
			return 1
		}
		return cmp.Compare(b.fd.Probabilities[check.Violation], a.fd.Probabilities[check.Violation])
	})
	total := len(items)
	if top > 0 && len(items) > top {
		items = items[:top]
	}
	trimmed := rep
	trimmed.Files = nil
	byPath := map[string]int{}
	for _, it := range items {
		i, ok := byPath[it.path]
		if !ok {
			i = len(trimmed.Files)
			byPath[it.path] = i
			trimmed.Files = append(trimmed.Files, check.FileResult{Path: it.path})
		}
		trimmed.Files[i].Findings = append(trimmed.Files[i].Findings, it.fd)
	}
	slices.SortFunc(trimmed.Files, func(a, b check.FileResult) int { return strings.Compare(a.Path, b.Path) })
	trimmed.Errors = nil

	var b strings.Builder
	b.WriteString("traits checked this edit against the project's traits (.agents/traits). Fix the \"Fix\" findings; double-check the others and keep the code if it is right.\n\n")
	if len(items) < total {
		fmt.Fprintf(&b, "Showing the top %d of %d findings; run `traits check --diff --format summary` for all.\n\n", len(items), total)
	}
	_ = output.Summary(&b, trimmed)
	return b.String()
}

// gitRoot returns the nearest ancestor of start containing .git, else start.
func gitRoot(start string) string {
	abs, err := filepath.Abs(start)
	if err != nil {
		return start
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return abs
		}
	}
}

// inside returns abs relative to root, slash-separated, if it is under root.
func inside(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func repoReader(root string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) { return os.ReadFile(filepath.Join(root, filepath.FromSlash(p))) }
}

func runHookInstall(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("hook install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apply := fs.Bool("apply", false, "write the settings file (default: dry run)")
	scope := fs.String("scope", "project", "project (this repository) or user (your home directory)")
	repo := fs.String("repo", "", "repository root (default: the current repository)")
	prov := fs.String("provider", "", "provider to pin in the hook commands")
	model := fs.String("model", "", "model to pin in the hook commands")
	agent, rest := "", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		agent, rest = args[0], args[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return exitUsage, usageError{err}
	}
	if agent == "" && fs.NArg() > 0 {
		agent = fs.Arg(0)
	}
	if agent != "claude" && agent != "codex" {
		return exitUsage, usageError{errors.New("usage: traits hook install claude|codex [--apply] [--scope project|user]")}
	}
	var base string
	switch *scope {
	case "project":
		base = repoRoot(*repo)
	case "user":
		home, err := os.UserHomeDir()
		if err != nil {
			return exitProvider, err
		}
		base = home
	default:
		return exitUsage, usageError{fmt.Errorf("unknown --scope %q", *scope)}
	}
	path := filepath.Join(base, ".claude", "settings.json")
	if agent == "codex" {
		path = filepath.Join(base, ".codex", "hooks.json")
	}
	var extra []string
	if *prov != "" {
		extra = append(extra, "--provider "+*prov)
	}
	if *model != "" {
		extra = append(extra, "--model "+*model)
	}
	cmds := hook.Commands(agent, strings.Join(extra, " "))
	res, err := hook.Install(path, cmds, *apply, time.Now())
	if err != nil {
		return exitProvider, err
	}
	var parts []string
	for i, c := range cmds {
		parts = append(parts, fmt.Sprintf("%s %s", c.Event, res.Actions[i]))
	}
	status := strings.Join(parts, ", ")
	switch {
	case !res.Changed:
		fmt.Fprintf(stdout, "%s: already up to date (%s)\n", path, status)
		return exitOK, nil
	case !*apply:
		fmt.Fprintf(stdout, "%s: would change (%s). Hooks to merge (other settings are kept):\n\n", path, status)
		frag := &hook.Object{}
		if _, err := hook.Merge(frag, cmds); err != nil {
			return exitProvider, err
		}
		data, _ := hook.Format(frag)
		stdout.Write(data)
		fmt.Fprintln(stdout, "\nDry run: nothing written. Re-run with --apply to write it (the old file is backed up first).")
	default:
		fmt.Fprintf(stdout, "%s: %s\n", path, status)
		if res.Backup != "" {
			fmt.Fprintf(stdout, "backup: %s\n", res.Backup)
		}
	}
	if agent == "codex" {
		fmt.Fprintln(stdout, "Codex asks you to review and trust new hooks (/hooks) before they run.")
	}
	fmt.Fprintln(stdout, "The hooks send edited code to the configured provider; its credentials must be in the agent's environment.")
	return exitOK, nil
}
