// Package output renders a check.Report as JSON, one-line text, Markdown for
// a coding agent, or SARIF 2.1.0.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/talbet/traits/internal/check"
)

// JSON writes r as indented JSON. Nil slices and maps render as [] and {}.
func JSON(w io.Writer, r check.Report) error {
	r = normalize(r)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// normalize returns a copy of r with nil slices and maps made empty, so they
// encode as [] and {} rather than null. r itself is not modified.
func normalize(r check.Report) check.Report {
	if r.Traits == nil {
		r.Traits = map[string]check.TraitInfo{}
	}
	if r.Errors == nil {
		r.Errors = []check.FileError{}
	}
	files := make([]check.FileResult, len(r.Files))
	for i, f := range r.Files {
		findings := make([]check.Finding, len(f.Findings))
		for j, fd := range f.Findings {
			if fd.Probabilities == nil {
				fd.Probabilities = map[check.Classification]float64{}
			}
			findings[j] = fd
		}
		f.Findings = findings
		files[i] = f
	}
	r.Files = files
	return r
}

// Text writes one line per finding, "path:line: tier[trait] interpretation"
// (":line" omitted without a location), then one "path: error: msg" line per
// file error. Newlines in messages are escaped as \n.
func Text(w io.Writer, r check.Report) error {
	var b strings.Builder
	for _, f := range r.Files {
		for _, fd := range f.Findings {
			b.WriteString(f.Path)
			if fd.Location != nil && fd.Location.StartLine > 0 {
				b.WriteString(":" + strconv.Itoa(fd.Location.StartLine))
			}
			fmt.Fprintf(&b, ": %s[%s] %s\n", fd.Tier, fd.Trait, escapeLine(fd.Interpretation))
		}
	}
	for _, e := range r.Errors {
		fmt.Fprintf(&b, "%s: error: %s\n", e.Path, escapeLine(e.Error))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

var lineEscaper = strings.NewReplacer("\r\n", `\n`, "\n", `\n`, "\r", `\r`)

func escapeLine(s string) string { return lineEscaper.Replace(s) }
