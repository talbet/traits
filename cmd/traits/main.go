// Command traits checks changes against a project's traits with a decision
// model. See `traits help`.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/talbet/traits/internal/check"
	"github.com/talbet/traits/internal/compile"
	"github.com/talbet/traits/internal/diff"
	"github.com/talbet/traits/internal/output"
	"github.com/talbet/traits/internal/provider"
	_ "github.com/talbet/traits/internal/provider/all"
	"github.com/talbet/traits/internal/state"
	"github.com/talbet/traits/internal/trait"
)

// version is set by the release build.
var version = "dev"

//go:embed help/usage.txt
var usageText string

// Exit statuses.
const (
	exitOK        = 0
	exitFindings  = 1
	exitUsage     = 2
	exitProvider  = 3
	exitInterrupt = 130
)

// usageError marks a command line that cannot be used.
type usageError struct{ error }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	var err error
	code := exitOK
	switch args[0] {
	case "check":
		code, err = runCheck(ctx, args[1:], stdin, stdout, stderr)
	case "explain":
		err = runExplain(args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "traits", version)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
	default:
		err = usageError{fmt.Errorf("unknown command %q", args[0])}
	}
	if err == nil {
		return code
	}
	fmt.Fprintln(stderr, "traits:", err)
	var ue usageError
	var pe *provider.Error
	switch {
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.As(err, &ue):
		return exitUsage
	case ctx.Err() != nil:
		return exitInterrupt
	case errors.As(err, &pe):
		return exitProvider
	}
	return exitProvider
}

type checkFlags struct {
	diff           bool
	assumeFilename string
	rules          string
	provider       string
	model          string
	format         string
	failOn         string
	jobs           int
	timeout        time.Duration
	retries        int
	repo           string
	noCache        bool
}

func runCheck(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	var f checkFlags
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&f.diff, "diff", false, "check a unified diff read from standard input")
	fs.StringVar(&f.assumeFilename, "assume-filename", "", "check one file read from standard input under this name")
	fs.StringVar(&f.rules, "rules", env("TRAITS_DIR", ""), "traits directory (default .agents/traits at the repository root)")
	fs.StringVar(&f.provider, "provider", env("TRAITS_PROVIDER", "openai"), "decision-model provider")
	fs.StringVar(&f.model, "model", env("TRAITS_MODEL", ""), "model name (default: the provider's)")
	fs.StringVar(&f.format, "format", "json", "output format: json, text, summary, sarif")
	fs.StringVar(&f.failOn, "fail-on", "fix", "exit 1 when a finding of this tier exists: fix, review, never")
	fs.IntVar(&f.jobs, "jobs", 8, "files to check concurrently")
	fs.DurationVar(&f.timeout, "timeout", 30*time.Second, "per-request timeout, including retries")
	fs.IntVar(&f.retries, "retries", 2, "retries on rate limits and server errors")
	fs.StringVar(&f.repo, "repo", "", "repository root for reading post-change files (default: the current repository)")
	fs.BoolVar(&f.noCache, "no-cache", false, "do not read or write the answer cache")
	if err := fs.Parse(args); err != nil {
		return exitUsage, usageError{err}
	}
	if err := validateCheckFlags(f, fs.Args()); err != nil {
		return exitUsage, usageError{err}
	}

	root := repoRoot(f.repo)
	dir := f.rules
	if dir == "" {
		dir = filepath.Join(root, ".agents", "traits")
	}
	traits, err := trait.Load(dir)
	if err != nil {
		if errors.Is(err, trait.ErrNoDir) {
			fmt.Fprintf(stderr, "traits: warning: no traits directory at %s; nothing to check\n", dir)
			return exitOK, nil
		}
		return exitUsage, usageError{err}
	}

	p, ok := provider.Lookup(f.provider)
	if !ok {
		return exitUsage, usageError{fmt.Errorf("unknown provider %q (have %s)", f.provider, strings.Join(provider.Names(), ", "))}
	}
	modelName := f.model
	if modelName == "" {
		modelName = p.Default
	}
	model, err := p.New(provider.Config{
		APIKey:  os.Getenv(p.KeyEnv),
		BaseURL: envIf(p.URLEnv),
		Account: envIf(p.AccountEnv),
		Model:   modelName,
		Timeout: f.timeout,
		Retries: f.retries,
	})
	if err != nil {
		return exitUsage, usageError{err}
	}

	files, readFile, inputErrs, err := readInput(f, fs.Args(), stdin, root)
	if err != nil {
		return exitUsage, usageError{err}
	}

	opts := check.Options{
		Model:        model,
		ProviderName: p.Name,
		ModelName:    modelName,
		Traits:       traits,
		Jobs:         f.jobs,
		ReadFile:     readFile,
	}
	if !f.noCache {
		if c, err := state.OpenCache(filepath.Join(dir, ".cache")); err == nil {
			opts.Cache = c
		}
	}

	rep, err := check.Run(ctx, opts, files)
	if err != nil {
		return exitProvider, err
	}
	if len(inputErrs) > 0 {
		rep.Errors = append(inputErrs, rep.Errors...)
	}
	if err := render(stdout, f.format, rep); err != nil {
		return exitProvider, err
	}
	if f.format != "summary" {
		fmt.Fprintln(stderr, rep.Summary)
	}
	for _, e := range rep.Errors {
		fmt.Fprintf(stderr, "traits: %s: %s\n", e.Path, e.Error)
	}
	if failed(rep, f.failOn) || len(rep.Errors) > 0 {
		return exitFindings, nil
	}
	return exitOK, nil
}

func validateCheckFlags(f checkFlags, args []string) error {
	switch {
	case f.diff && f.assumeFilename != "":
		return errors.New("--diff and --assume-filename are mutually exclusive")
	case (f.diff || f.assumeFilename != "") && len(args) > 0:
		return errors.New("--diff and --assume-filename read standard input and take no paths")
	case !f.diff && f.assumeFilename == "" && len(args) == 0:
		return errors.New("nothing to check: give paths, --diff, or --assume-filename")
	case f.jobs < 1:
		return errors.New("--jobs must be at least 1")
	case f.retries < 0:
		return errors.New("--retries must not be negative")
	}
	switch f.format {
	case "json", "text", "summary", "sarif":
	default:
		return fmt.Errorf("unknown --format %q", f.format)
	}
	switch f.failOn {
	case "fix", "review", "never":
	default:
		return fmt.Errorf("unknown --fail-on %q", f.failOn)
	}
	return nil
}

// wholeFileHunk is how many lines of a whole file go in one synthetic hunk
// when checking files rather than a diff.
const wholeFileHunk = 150

// readInput returns the files to check, a reader for post-change content,
// and per-path errors for paths that could not be read; those are reported
// with the other file errors rather than stopping the run.
func readInput(f checkFlags, args []string, stdin io.Reader, root string) ([]diff.File, func(string) ([]byte, error), []check.FileError, error) {
	readRepo := func(p string) ([]byte, error) { return os.ReadFile(filepath.Join(root, filepath.FromSlash(p))) }
	switch {
	case f.diff:
		files, err := diff.Parse(stdin)
		return files, readRepo, nil, err
	case f.assumeFilename != "":
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, nil, nil, err
		}
		name := filepath.ToSlash(f.assumeFilename)
		read := func(p string) ([]byte, error) {
			if p == name {
				return data, nil
			}
			return nil, fs.ErrNotExist
		}
		return []diff.File{wholeFile(name, data)}, read, nil, nil
	}
	var files []diff.File
	var errs []check.FileError
	contents := map[string][]byte{}
	for _, a := range args {
		_ = filepath.WalkDir(a, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				errs = append(errs, check.FileError{Path: relPath(root, p), Error: err.Error()})
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if p != a && strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				errs = append(errs, check.FileError{Path: relPath(root, p), Error: err.Error()})
				return nil
			}
			if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
				return nil // binary
			}
			name := relPath(root, p)
			contents[name] = data
			files = append(files, wholeFile(name, data))
			return nil
		})
	}
	read := func(p string) ([]byte, error) {
		if d, ok := contents[p]; ok {
			return d, nil
		}
		return nil, fs.ErrNotExist
	}
	return files, read, errs, nil
}

// wholeFile presents a file's content as a new file, split into hunks of
// wholeFileHunk lines so large files can be batched.
func wholeFile(name string, data []byte) diff.File {
	f := diff.File{OldPath: "/dev/null", NewPath: name}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return f
	}
	lines := strings.Split(text, "\n")
	for start := 0; start < len(lines); start += wholeFileHunk {
		end := min(start+wholeFileHunk, len(lines))
		h := diff.Hunk{NewStart: start + 1, NewLines: end - start}
		for i := start; i < end; i++ {
			h.Lines = append(h.Lines, diff.Line{Kind: diff.Added, NewLine: i + 1, Text: strings.TrimSuffix(lines[i], "\r")})
		}
		f.Hunks = append(f.Hunks, h)
	}
	return f
}

func relPath(root, p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	if r, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(p)
}

// repoRoot returns dir if set, else the nearest ancestor of the working
// directory containing .git, else the working directory.
func repoRoot(dir string) string {
	if dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			return abs
		}
		return dir
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	for d := wd; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return wd
		}
	}
}

func render(w io.Writer, format string, r check.Report) error {
	switch format {
	case "text":
		return output.Text(w, r)
	case "summary":
		return output.Summary(w, r)
	case "sarif":
		return output.SARIF(w, r, version)
	}
	return output.JSON(w, r)
}

func failed(r check.Report, failOn string) bool {
	for _, f := range r.Files {
		for _, fd := range f.Findings {
			if failOn == "review" || (failOn == "fix" && fd.Tier == check.TierFix) {
				return true
			}
		}
	}
	return false
}

func runExplain(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rules := fs.String("rules", env("TRAITS_DIR", ""), "traits directory")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	dir := *rules
	if dir == "" {
		dir = filepath.Join(repoRoot(""), ".agents", "traits")
	}
	traits, err := trait.Load(dir)
	if err != nil {
		return usageError{err}
	}
	type option struct {
		Value       string `json:"value"`
		Description string `json:"description"`
	}
	type question struct {
		Name         string   `json:"name"`
		Type         string   `json:"type"`
		Instructions string   `json:"instructions"`
		Options      []option `json:"options"`
	}
	byName := map[string]trait.Trait{}
	for _, t := range traits {
		byName[t.Name] = t
	}
	names := fs.Args()
	if len(names) == 0 {
		for _, t := range traits {
			names = append(names, t.Name)
		}
	}
	qs := []question{}
	for _, n := range names {
		t, ok := byName[n]
		if !ok {
			return usageError{fmt.Errorf("no trait named %q", n)}
		}
		q := compile.TraitQuestion(t)
		out := question{Name: q.Name, Type: string(q.Type), Instructions: q.Instructions}
		for _, o := range q.Options {
			out.Options = append(out.Options, option(o))
		}
		qs = append(qs, out)
	}
	out := map[string]any{"compilerVersion": compile.Version, "questions": qs}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envIf(name string) string {
	if name == "" {
		return ""
	}
	return os.Getenv(name)
}
