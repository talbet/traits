package diff

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// ErrNoDiff reports input that contains no unified diff at all.
var ErrNoDiff = errors.New("no diff found (expected unified diff output, e.g. from git diff)")

const devNull = "/dev/null"

// maxLineNumber bounds hunk header numbers so line arithmetic cannot overflow.
const maxLineNumber = 1 << 30

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)$`)

// Parse reads a unified diff: git diff output (with "diff --git" headers,
// extended header lines, renames, binary markers, and C-quoted paths) or a
// plain unified diff whose sections start at "---". Text before the first
// section, such as a commit header, is ignored. Hunk line counts are checked
// against their "@@" headers; a malformed diff returns an error naming the
// input line. Input without any diff returns ErrNoDiff.
func Parse(r io.Reader) ([]File, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	p := parser{lines: splitLines(string(data))}
	if err := p.run(); err != nil {
		return nil, err
	}
	if len(p.files) == 0 {
		return nil, ErrNoDiff
	}
	return p.files, nil
}

type parser struct {
	lines []string
	files []File

	// cur is the file being read, or nil outside any file section.
	cur *File
	// git is set while reading a "diff --git" section's extended header,
	// before its "---" line or first hunk.
	git bool

	// Open hunk state: remaining old/new line counts and header line number.
	hunk           *Hunk
	remOld, remNew int
	oldNo, newNo   int
	hunkLine       int
}

func (p *parser) newFile() *File {
	p.files = append(p.files, File{})
	p.cur = &p.files[len(p.files)-1]
	return p.cur
}

func (p *parser) run() error {
	for i := 0; i < len(p.lines); i++ {
		line, n := p.lines[i], i+1
		if p.remOld > 0 || p.remNew > 0 {
			if err := p.hunkLineAt(line, n); err != nil {
				return err
			}
			continue
		}
		p.hunk = nil

		switch {
		case strings.HasPrefix(line, "diff --git "):
			old, new, err := splitGitHeader(strings.TrimPrefix(line, "diff --git "))
			if err != nil {
				return fmt.Errorf("line %d: %w", n, err)
			}
			f := p.newFile()
			f.OldPath, f.NewPath = stripPrefixes(old, new)
			p.git = true

		case strings.HasPrefix(line, "diff --cc ") || strings.HasPrefix(line, "diff --combined "):
			return fmt.Errorf("line %d: combined (merge) diffs are not supported", n)

		case strings.HasPrefix(line, "--- ") && i+1 < len(p.lines) && strings.HasPrefix(p.lines[i+1], "+++ "):
			old, err := patchPath(strings.TrimPrefix(line, "--- "))
			if err != nil {
				return fmt.Errorf("line %d: %w", n, err)
			}
			new, err := patchPath(strings.TrimPrefix(p.lines[i+1], "+++ "))
			if err != nil {
				return fmt.Errorf("line %d: %w", n+1, err)
			}
			if !p.git {
				// A plain unified diff section, or the next one.
				p.newFile()
			}
			p.cur.OldPath, p.cur.NewPath = stripPrefixes(old, new)
			p.git = false
			i++

		case strings.HasPrefix(line, "@@ "):
			if p.cur == nil {
				return fmt.Errorf("line %d: hunk before any file header", n)
			}
			if err := p.openHunk(line, n); err != nil {
				return err
			}
			p.git = false

		case p.git:
			if err := p.extendedHeader(line, n); err != nil {
				return err
			}

		case strings.HasPrefix(line, "Binary files ") && strings.HasSuffix(line, " differ"):
			// A plain diff's binary marker (GNU diff -r).
			old, new, _ := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(line, "Binary files "), " differ"), " and ")
			f := p.newFile()
			f.OldPath, f.NewPath = stripPrefixes(old, new)
			f.Binary = true
			p.cur = nil

		case p.cur != nil && len(p.cur.Hunks) > 0 && line == "-- ":
			// git format-patch signature: the rest is not diff content.
			p.cur = nil

		case p.cur != nil && len(p.cur.Hunks) > 0 && line != "" && strings.ContainsRune("+- ", rune(line[0])):
			return fmt.Errorf("line %d: unexpected %q after a complete hunk (the @@ header at line %d declares fewer lines)", n, truncate(line), p.hunkLine)
		}
	}
	if p.remOld > 0 || p.remNew > 0 {
		return fmt.Errorf("line %d: unexpected end of input: hunk at line %d is missing %d old and %d new lines", len(p.lines), p.hunkLine, p.remOld, p.remNew)
	}
	return nil
}

// extendedHeader handles a line between "diff --git" and "---" or "@@".
func (p *parser) extendedHeader(line string, n int) error {
	f := p.cur
	var err error
	switch {
	case strings.HasPrefix(line, "new file mode "):
		f.OldPath = devNull
	case strings.HasPrefix(line, "deleted file mode "):
		f.NewPath = devNull
	case strings.HasPrefix(line, "rename from "):
		f.OldPath, err = unquotePath(strings.TrimPrefix(line, "rename from "))
	case strings.HasPrefix(line, "rename to "):
		f.NewPath, err = unquotePath(strings.TrimPrefix(line, "rename to "))
	case strings.HasPrefix(line, "copy from "):
		f.OldPath, err = unquotePath(strings.TrimPrefix(line, "copy from "))
	case strings.HasPrefix(line, "copy to "):
		f.NewPath, err = unquotePath(strings.TrimPrefix(line, "copy to "))
	case line == "GIT binary patch" || strings.HasPrefix(line, "Binary files ") && strings.HasSuffix(line, " differ"):
		f.Binary = true
	}
	if err != nil {
		return fmt.Errorf("line %d: %w", n, err)
	}
	return nil
}

func (p *parser) openHunk(line string, n int) error {
	m := hunkHeader.FindStringSubmatch(line)
	if m == nil {
		return fmt.Errorf("line %d: malformed hunk header %q", n, truncate(line))
	}
	var nums [4]int
	for k, s := range m[1:5] {
		if s == "" {
			nums[k] = 1 // an omitted count means one line
			continue
		}
		v, err := strconv.Atoi(s)
		if err != nil || v > maxLineNumber {
			return fmt.Errorf("line %d: hunk header number %q out of range", n, s)
		}
		nums[k] = v
	}
	h := Hunk{OldStart: nums[0], OldLines: nums[1], NewStart: nums[2], NewLines: nums[3],
		Header: strings.TrimPrefix(m[5], " ")}
	switch {
	case h.OldLines == 0 && h.NewLines == 0:
		return fmt.Errorf("line %d: hunk header declares no lines", n)
	case h.OldStart == 0 && h.OldLines > 0, h.NewStart == 0 && h.NewLines > 0:
		return fmt.Errorf("line %d: hunk header has line 0 with a non-zero count", n)
	}
	p.cur.Hunks = append(p.cur.Hunks, h)
	p.hunk = &p.cur.Hunks[len(p.cur.Hunks)-1]
	p.remOld, p.remNew = h.OldLines, h.NewLines
	p.oldNo, p.newNo = h.OldStart, h.NewStart
	p.hunkLine = n
	return nil
}

// hunkLineAt consumes one line of an open hunk.
func (p *parser) hunkLineAt(line string, n int) error {
	if line == "" {
		// Editors and mailers strip the space from empty context lines.
		line = " "
	}
	text := line[1:]
	switch line[0] {
	case ' ':
		if p.remOld == 0 || p.remNew == 0 {
			return p.overflow(n)
		}
		p.hunk.Lines = append(p.hunk.Lines, Line{Kind: Context, OldLine: p.oldNo, NewLine: p.newNo, Text: text})
		p.oldNo++
		p.newNo++
		p.remOld--
		p.remNew--
	case '-':
		if p.remOld == 0 {
			return p.overflow(n)
		}
		p.hunk.Lines = append(p.hunk.Lines, Line{Kind: Removed, OldLine: p.oldNo, Text: text})
		p.oldNo++
		p.remOld--
	case '+':
		if p.remNew == 0 {
			return p.overflow(n)
		}
		p.hunk.Lines = append(p.hunk.Lines, Line{Kind: Added, NewLine: p.newNo, Text: text})
		p.newNo++
		p.remNew--
	case '\\':
		// "\ No newline at end of file"
	default:
		return fmt.Errorf("line %d: hunk at line %d ends early: missing %d old and %d new lines", n, p.hunkLine, p.remOld, p.remNew)
	}
	return nil
}

func (p *parser) overflow(n int) error {
	return fmt.Errorf("line %d: hunk at line %d has more lines than its @@ header declares", n, p.hunkLine)
}

// splitLines splits text into lines without their "\n" or "\r\n".
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// splitGitHeader splits the paths of a "diff --git" line. Unquoted paths may
// contain spaces; they are split where both halves name the same file, or
// before " b/" when they differ (a rename, corrected later by "rename to").
func splitGitHeader(rest string) (old, new string, err error) {
	if strings.HasPrefix(rest, `"`) {
		old, tail, err := cutQuoted(rest)
		if err != nil {
			return "", "", err
		}
		new, err = unquotePath(strings.TrimPrefix(tail, " "))
		return old, new, err
	}
	if i := strings.Index(rest, ` "`); i >= 0 && strings.HasSuffix(rest, `"`) {
		new, err = unquotePath(rest[i+1:])
		return rest[:i], new, err
	}
	if len(rest)%2 == 1 {
		n := len(rest) / 2
		a, b := rest[:n], rest[n+1:]
		if rest[n] == ' ' && strings.TrimPrefix(a, "a/") == strings.TrimPrefix(b, "b/") {
			return a, b, nil
		}
	}
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[:i], rest[i+1:], nil
	}
	old, new, _ = strings.Cut(rest, " ")
	return old, new, nil
}

// patchPath extracts the path from a "---" or "+++" line's remainder: a
// C-quoted path, or everything before a tab (git appends one to paths with
// spaces; plain diffs put a timestamp after it).
func patchPath(s string) (string, error) {
	if strings.HasPrefix(s, `"`) {
		p, _, err := cutQuoted(s)
		return p, err
	}
	p, _, _ := strings.Cut(s, "\t")
	return p, nil
}

// unquotePath returns s unquoted if it is a C-quoted path, else s.
func unquotePath(s string) (string, error) {
	if !strings.HasPrefix(s, `"`) {
		return s, nil
	}
	p, _, err := cutQuoted(s)
	return p, err
}

// cutQuoted unquotes the C-style quoted string at the start of s (git's
// quoting: \" \\ \a \b \f \n \r \t \v and \ooo octal bytes) and returns the
// rest of s after the closing quote.
func cutQuoted(s string) (unquoted, rest string, err error) {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			u, err := strconv.Unquote(s[:i+1])
			if err != nil {
				return "", "", fmt.Errorf("bad quoted path %s", truncate(s[:i+1]))
			}
			return u, s[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("unterminated quoted path %s", truncate(s))
}

// stripPrefixes removes git's "a/" and "b/" prefixes when both paths carry
// them (or are /dev/null), so --no-prefix and plain diffs keep their paths.
func stripPrefixes(old, new string) (string, string) {
	okOld := old == devNull || strings.HasPrefix(old, "a/")
	okNew := new == devNull || strings.HasPrefix(new, "b/")
	if okOld && okNew {
		return strings.TrimPrefix(old, "a/"), strings.TrimPrefix(new, "b/")
	}
	return old, new
}

func truncate(s string) string {
	const max = 60
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// ChangedText returns the text of the file's added and removed lines, in
// order, for trait gating.
func (f File) ChangedText() []string {
	var out []string
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Kind != Context {
				out = append(out, l.Text)
			}
		}
	}
	return out
}
