package main

import (
	"cmp"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/talbet/traits/internal/check"
	"github.com/talbet/traits/internal/diff"
	"github.com/talbet/traits/internal/state"
	"github.com/talbet/traits/internal/trait"
)

// maxRecheckFiles bounds the files re-checked in one Stop hook.
const maxRecheckFiles = 8

// runRecheck implements `traits recheck`, a Stop/SubagentStop hook: for the
// findings this session logged that have no outcome yet, it re-checks each
// file's current content against just those traits and logs "fixed" (no
// longer flagged) or "kept". It prints nothing and always exits 0.
func runRecheck(ctx context.Context, args []string, stdin io.Reader, _, stderr io.Writer) (int, error) {
	f, err := parseHookFlags("recheck", args, stderr, false)
	if err != nil {
		return exitOK, nil
	}
	failOpen(ctx, f.timeout, stderr, func(ctx context.Context) ([]byte, error) {
		return nil, recheck(ctx, f, stdin)
	})
	return exitOK, nil
}

func recheck(ctx context.Context, f hookFlags, stdin io.Reader) error {
	p, err := readPayload(stdin)
	if err != nil || p.SessionID == "" {
		return err
	}
	if p.Cwd == "" {
		p.Cwd, _ = os.Getwd()
	}
	he, err := loadHookEnv(p.Cwd, f.rules)
	if err != nil {
		return err
	}
	logPath := state.FindingsPath(he.dir)
	recs, err := state.ReadFindings(logPath)
	if err != nil {
		return err
	}
	// Open findings of this session, grouped by file.
	pending := map[string][]state.Finding{}
	var paths []string
	for _, fd := range state.Fold(recs) {
		if fd.Session != p.SessionID || fd.Outcome != "" {
			continue
		}
		if _, ok := pending[fd.Path]; !ok {
			if len(paths) == maxRecheckFiles {
				continue
			}
			paths = append(paths, fd.Path)
		}
		pending[fd.Path] = append(pending[fd.Path], fd)
	}
	if len(paths) == 0 {
		return nil
	}
	first := pending[paths[0]][0]
	model, err := openModel(cmp.Or(f.provider, first.Provider, "openai"), cmp.Or(f.model, first.Model), f.timeout, 0)
	if err != nil {
		return err
	}
	var cache check.Cache
	if c, err := state.OpenCache(filepath.Join(he.dir, ".cache")); err == nil {
		cache = c
	}
	byName := map[string]trait.Trait{}
	for _, t := range he.traits {
		byName[t.Name] = t
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		outcomes []state.Record
	)
	for _, path := range paths {
		wg.Go(func() {
			recs := recheckFile(ctx, he.root, path, pending[path], byName, model, cache)
			mu.Lock()
			outcomes = append(outcomes, recs...)
			mu.Unlock()
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return state.AppendFindings(logPath, outcomes)
}

// recheckFile returns outcome records for one file's open findings, or none
// when the file is gone or could not be checked.
func recheckFile(ctx context.Context, root, path string, open []state.Finding, byName map[string]trait.Trait, model resolvedModel, cache check.Cache) []state.Record {
	data, err := confinedReader(root)(path)
	if err != nil {
		return nil
	}
	var traits []trait.Trait
	seen := map[string]bool{}
	for _, fd := range open {
		if t, ok := byName[fd.Trait]; ok && !seen[t.Name] {
			seen[t.Name] = true
			traits = append(traits, t)
		}
	}
	if len(traits) == 0 {
		return nil // the trait was removed or renamed: no verdict
	}
	rep, err := check.Run(ctx, check.Options{
		Model:        model.DecisionModel,
		ProviderName: model.Provider,
		ModelName:    model.Name,
		Traits:       traits,
		ReadFile:     confinedReader(root),
		Cache:        cache,
	}, []diff.File{wholeFile(path, data)})
	if err != nil || len(rep.Errors) > 0 {
		return nil
	}
	flagged := map[string]bool{}
	for _, fr := range rep.Files {
		for _, fd := range fr.Findings {
			flagged[fd.Trait] = true
		}
	}
	now := time.Now().UTC()
	var out []state.Record
	for _, fd := range open {
		if !seen[fd.Trait] {
			continue
		}
		outcome := state.OutcomeFixed
		if flagged[fd.Trait] {
			outcome = state.OutcomeKept
		}
		out = append(out, state.Record{TS: now, ID: fd.ID, Session: fd.Session, Outcome: outcome})
	}
	return out
}
