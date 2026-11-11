package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/talbet/traits/internal/check"
	"github.com/talbet/traits/internal/diff"
	"github.com/talbet/traits/internal/state"
)

// logFindings appends rep's findings to the findings log of the traits
// directory. Logging never affects the command, so errors are ignored. A
// check run inside CI (CI or GITHUB_ACTIONS set) is logged as source "ci".
func logFindings(traitsDir string, rep check.Report, files []diff.File, source, session string) {
	if source == state.SourceCheck && (os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "") {
		source = state.SourceCI
	}
	_ = state.AppendFindings(state.FindingsPath(traitsDir), findingRecords(rep, files, source, session, time.Now().UTC()))
}

func findingRecords(rep check.Report, files []diff.File, source, session string, now time.Time) []state.Record {
	var recs []state.Record
	for _, fr := range rep.Files {
		for _, fd := range fr.Findings {
			r := state.Record{
				TS:             now,
				Path:           fr.Path,
				Trait:          fd.Trait,
				Tier:           string(fd.Tier),
				Classification: string(fd.Classification),
				Probabilities:  map[string]float64{},
				Provider:       rep.Provider,
				Model:          rep.Model,
				Source:         source,
				Session:        session,
			}
			for k, v := range fd.Probabilities {
				r.Probabilities[string(k)] = v
			}
			text := ""
			if l := fd.Location; l != nil && l.StartLine > 0 {
				r.StartLine, r.EndLine = l.StartLine, max(l.EndLine, l.StartLine)
				text = linesAt(files, fr.Path, r.StartLine, r.EndLine)
			}
			r.ID = state.FindingID(fr.Path, fd.Trait, text)
			recs = append(recs, r)
		}
	}
	return recs
}

// linesAt returns the new-file text of lines start..end of path as found in
// files' hunks, or "Lstart-end" when those lines are not in the diff.
func linesAt(files []diff.File, path string, start, end int) string {
	var b strings.Builder
	for _, f := range files {
		if f.Path() != path {
			continue
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Kind != diff.Removed && l.NewLine >= start && l.NewLine <= end {
					b.WriteString(strings.TrimSpace(l.Text))
					b.WriteByte('\n')
				}
			}
		}
	}
	if b.Len() == 0 {
		return fmt.Sprintf("L%d-%d", start, end)
	}
	return b.String()
}

func runFindings(_ context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("findings", flag.ContinueOnError)
	fs.SetOutput(stderr)
	summary := fs.Bool("summary", false, "per-trait counts: flagged, fixed, kept, unknown, keep rate")
	format := fs.String("format", "text", "output format: text or json")
	rules := fs.String("rules", env("TRAITS_DIR", ""), "traits directory")
	repo := fs.String("repo", "", "repository root")
	if err := fs.Parse(args); err != nil {
		return exitUsage, usageError{err}
	}
	if fs.NArg() > 0 {
		return exitUsage, usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	if *format != "text" && *format != "json" {
		return exitUsage, usageError{fmt.Errorf("unknown --format %q", *format)}
	}
	recs, err := state.ReadFindings(state.FindingsPath(traitsDir(*rules, repoRoot(*repo))))
	if err != nil {
		return exitProvider, err
	}
	folded := state.Fold(recs)
	if *summary {
		sums := state.Summarize(folded)
		if *format == "json" {
			return exitOK, writeJSON(stdout, sums)
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "TRAIT\tFLAGGED\tFIXED\tKEPT\tUNKNOWN\tKEEP-RATE")
		for _, s := range sums {
			rate := "-"
			if s.Fixed+s.Kept > 0 {
				rate = fmt.Sprintf("%.0f%%", 100*s.KeepRate)
			}
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\n", s.Trait, s.Flagged, s.Fixed, s.Kept, s.Unknown, rate)
		}
		return exitOK, tw.Flush()
	}
	if *format == "json" {
		if folded == nil {
			folded = []state.Finding{}
		}
		return exitOK, writeJSON(stdout, folded)
	}
	for _, f := range folded {
		where := f.Path
		if f.StartLine > 0 {
			where += fmt.Sprintf(":%d", f.StartLine)
		}
		outcome := f.Outcome
		if outcome == "" {
			outcome = "unknown"
		}
		fmt.Fprintf(stdout, "%s %s %s[%s] %s (%s)\n", f.TS.Format(time.RFC3339), where, f.Tier, f.Trait, outcome, f.Source)
	}
	return exitOK, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
