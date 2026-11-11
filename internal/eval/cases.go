// Package eval reads labelled cases for traits and scores a decision model's
// answers on them.
//
// Cases live beside a trait in <name>.cases.jsonl, one JSON object per line:
//
//	{"id":"opt","split":"dev","path":"src/x.go","diff":"@@ ...","expect":"violation","note":"opt"}
//
// with exactly one of "diff" (a unified diff, file headers optional) or
// "content" (the whole post-change file).
package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/talbet/traits/internal/diff"
	"github.com/talbet/traits/internal/trait"
)

// Expect is a case's labelled answer.
type Expect string

const (
	ExpectViolation     Expect = "violation"
	ExpectCompliant     Expect = "compliant"
	ExpectNotApplicable Expect = "not-applicable"
)

// Split separates cases used while writing a trait from those reserved for
// judging it.
type Split string

const (
	SplitDev     Split = "dev"
	SplitHoldout Split = "holdout"
)

// CasesSuffix is the extension of a trait's sidecar cases file.
const CasesSuffix = ".cases.jsonl"

// Case is one labelled example.
type Case struct {
	ID      string  `json:"id,omitempty"`
	Split   Split   `json:"split"`
	Path    string  `json:"path"`
	Diff    *string `json:"diff,omitempty"`
	Content *string `json:"content,omitempty"`
	Expect  Expect  `json:"expect"`
	Note    string  `json:"note,omitempty"`

	// Source is "file:line" for messages.
	Source string `json:"-"`
	// File is the parsed diff of a diff case (zero for content cases).
	File diff.File `json:"-"`
}

// Positive reports whether the case is labelled a violation.
func (c Case) Positive() bool { return c.Expect == ExpectViolation }

// Label is the case ID, or its source position when it has none.
func (c Case) Label() string {
	if c.ID != "" {
		return c.ID
	}
	return c.Source
}

// PostImage returns the post-change file for content cases and for diffs
// that create the file; other diffs do not carry the whole file.
func (c Case) PostImage() ([]byte, bool) {
	if c.Content != nil {
		return []byte(*c.Content), true
	}
	if c.File.OldPath != "/dev/null" {
		return nil, false
	}
	var b strings.Builder
	for _, h := range c.File.Hunks {
		for _, l := range h.Lines {
			if l.Kind == diff.Added {
				b.WriteString(l.Text)
				b.WriteByte('\n')
			}
		}
	}
	return []byte(b.String()), true
}

// CasesPath is the sidecar cases file for t, relative to the traits dir
// (t.Path with .md replaced by .cases.jsonl), in slash form.
func CasesPath(t trait.Trait) string {
	p := t.Path
	if ext := path.Ext(p); strings.EqualFold(ext, ".md") {
		p = p[:len(p)-len(ext)]
	}
	return p + CasesSuffix
}

// Parse reads cases from r. name labels errors, which carry 1-based line
// numbers; all invalid lines are reported, joined. Blank lines are skipped.
func Parse(name string, r io.Reader) ([]Case, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var cases []Case
	var errs []error
	ids := map[string]int{}
	for i, line := range strings.Split(string(data), "\n") {
		n := i + 1
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		c, err := parseLine(line)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s:%d: %w", name, n, err))
			continue
		}
		if c.ID != "" {
			if prev, dup := ids[c.ID]; dup {
				errs = append(errs, fmt.Errorf("%s:%d: duplicate id %q (also line %d)", name, n, c.ID, prev))
				continue
			}
			ids[c.ID] = n
		}
		c.Source = fmt.Sprintf("%s:%d", name, n)
		cases = append(cases, c)
	}
	return cases, errors.Join(errs...)
}

func parseLine(line string) (Case, error) {
	var c Case
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid case: %w", err)
	}
	if dec.More() {
		return c, errors.New("invalid case: more than one JSON value on the line")
	}
	switch c.Split {
	case SplitDev, SplitHoldout:
	case "":
		return c, errors.New(`missing "split" (dev or holdout)`)
	default:
		return c, fmt.Errorf(`unknown split %q (want dev or holdout)`, c.Split)
	}
	switch c.Expect {
	case ExpectViolation, ExpectCompliant, ExpectNotApplicable:
	case "":
		return c, errors.New(`missing "expect" (violation, compliant or not-applicable)`)
	default:
		return c, fmt.Errorf(`unknown expect %q (want violation, compliant or not-applicable)`, c.Expect)
	}
	c.Path = strings.TrimPrefix(strings.TrimSpace(c.Path), "./")
	if c.Path == "" {
		return c, errors.New(`empty "path"`)
	}
	switch {
	case c.Diff != nil && c.Content != nil:
		return c, errors.New(`give either "diff" or "content", not both`)
	case c.Diff == nil && c.Content == nil:
		return c, errors.New(`missing "diff" or "content"`)
	case c.Content != nil:
		if strings.TrimSpace(*c.Content) == "" {
			return c, errors.New(`empty "content"`)
		}
		return c, nil
	}
	f, err := parseDiff(c.Path, *c.Diff)
	if err != nil {
		return c, fmt.Errorf("diff: %w", err)
	}
	c.File = f
	return c, nil
}

// parseDiff parses a case's diff, adding file headers for path when the
// text is bare hunks. It must describe exactly one file, path, with at least
// one hunk.
func parseDiff(p, text string) (diff.File, error) {
	if strings.TrimSpace(text) == "" {
		return diff.File{}, errors.New("empty")
	}
	if !hasFileHeader(text) {
		text = "--- a/" + p + "\n+++ b/" + p + "\n" + text
	}
	files, err := diff.Parse(bytes.NewReader([]byte(text)))
	if err != nil {
		return diff.File{}, err
	}
	if len(files) != 1 {
		return diff.File{}, fmt.Errorf("has %d files; a case is one file", len(files))
	}
	f := files[0]
	switch {
	case f.Path() != p:
		return f, fmt.Errorf("is for %q but the case path is %q", f.Path(), p)
	case f.Binary:
		return f, errors.New("is a binary change")
	case f.Deleted():
		return f, errors.New("deletes the file; there is nothing to evaluate")
	case len(f.Hunks) == 0:
		return f, errors.New("has no hunks")
	}
	return f, nil
}

func hasFileHeader(text string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "--- ") {
			return true
		}
		if strings.HasPrefix(line, "@@ ") {
			return false
		}
	}
	return false
}
