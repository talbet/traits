package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/talbet/traits/skills"
)

// runSkill implements `traits skill install`: it writes the embedded skill
// where the chosen agent looks for project skills.
func runSkill(_ context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 || args[0] != "install" {
		return exitUsage, usageError{errors.New("usage: traits skill install [--agent claude|codex|opencode] [--apply]")}
	}
	flags := flag.NewFlagSet("skill install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	agent := flags.String("agent", "claude", "claude (.claude/skills), codex or opencode (.agents/skills)")
	apply := flags.Bool("apply", false, "write the file (default: dry run)")
	repo := flags.String("repo", "", "repository root (default: the current repository)")
	if err := flags.Parse(args[1:]); err != nil {
		return exitUsage, usageError{err}
	}
	if flags.NArg() > 0 {
		return exitUsage, usageError{fmt.Errorf("unexpected argument %q", flags.Arg(0))}
	}
	var rel string
	switch *agent {
	case "claude":
		rel = filepath.Join(".claude", "skills", "traits", "SKILL.md")
	case "codex", "opencode":
		rel = filepath.Join(".agents", "skills", "traits", "SKILL.md")
	default:
		return exitUsage, usageError{fmt.Errorf("unknown --agent %q (have claude, codex, opencode)", *agent)}
	}
	path := filepath.Join(repoRoot(*repo), rel)
	want := []byte(skills.Traits)
	old, err := os.ReadFile(path)
	exists := err == nil
	switch {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return exitProvider, err
	case exists && bytes.Equal(old, want):
		fmt.Fprintf(stdout, "%s: already up to date\n", path)
		return exitOK, nil
	case !*apply:
		verb := "would create"
		if exists {
			verb = "would replace (differs from this version's skill; a backup is kept)"
		}
		fmt.Fprintf(stdout, "%s: %s. Dry run: re-run with --apply to write it.\n", path, verb)
		return exitOK, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return exitProvider, err
	}
	if exists {
		backup := path + ".bak-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(backup, old, 0o644); err != nil {
			return exitProvider, err
		}
		fmt.Fprintf(stdout, "backup: %s\n", backup)
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		return exitProvider, err
	}
	fmt.Fprintf(stdout, "%s: written\n", path)
	return exitOK, nil
}
