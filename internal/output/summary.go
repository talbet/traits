package output

import (
	"fmt"
	"io"
	"strings"

	"github.com/talbet/traits/internal/check"
)

// Summary writes Markdown for a coding agent to act on: the summary sentence,
// then "Fix" and "Double-check" sections listing findings by file with their
// lines, requirement, purpose and suggested fix, then file errors. A report
// with no findings is a single line with the skipped counts, plus any errors.
func Summary(w io.Writer, r check.Report) error {
	var b strings.Builder
	if countFindings(r) == 0 {
		s := r.Skipped
		fmt.Fprintf(&b, "No findings to fix or double-check (skipped: %d gated, %d not applicable, %d compliant; %d cached).\n",
			s.Gated, s.NotApplicable, s.Compliant, s.Cached)
	} else {
		if r.Summary != "" {
			b.WriteString(r.Summary + "\n")
		}
		writeTier(&b, r, check.TierFix, "Fix")
		writeTier(&b, r, check.TierReview, "Double-check")
	}
	if len(r.Errors) > 0 {
		b.WriteString("\n## Errors\n\n")
		for _, e := range r.Errors {
			fmt.Fprintf(&b, "- `%s`: %s\n", e.Path, oneLine(e.Error))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func countFindings(r check.Report) int {
	n := 0
	for _, f := range r.Files {
		n += len(f.Findings)
	}
	return n
}

func writeTier(b *strings.Builder, r check.Report, tier check.Tier, heading string) {
	wrote := false
	for _, f := range r.Files {
		fileWrote := false
		for _, fd := range f.Findings {
			if fd.Tier != tier {
				continue
			}
			if !wrote {
				fmt.Fprintf(b, "\n## %s\n", heading)
				wrote = true
			}
			if !fileWrote {
				fmt.Fprintf(b, "\n### %s\n\n", f.Path)
				fileWrote = true
			}
			fmt.Fprintf(b, "- %s: `%s` (%s %.2f)\n", where(fd.Location), fd.Trait, fd.Classification, fd.Confidence)
			info, ok := r.Traits[fd.Trait]
			if !ok {
				continue
			}
			if info.Requirement != "" {
				fmt.Fprintf(b, "  - Requirement: %s\n", oneLine(info.Requirement))
			}
			if info.Purpose != "" {
				fmt.Fprintf(b, "  - Why: %s\n", oneLine(info.Purpose))
			}
			if info.Fix != "" {
				fmt.Fprintf(b, "  - Fix: %s\n", oneLine(info.Fix))
			}
		}
	}
}

func where(l *check.Location) string {
	switch {
	case l == nil || l.StartLine <= 0:
		return "Whole file"
	case l.EndLine <= l.StartLine:
		return fmt.Sprintf("Line %d", l.StartLine)
	default:
		return fmt.Sprintf("Lines %d-%d", l.StartLine, l.EndLine)
	}
}

// oneLine collapses runs of whitespace, including newlines, to single spaces
// so multi-line trait text stays inside its list item.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
