// Package diff parses unified diffs (git diff output) into files and hunks
// with absolute line numbers.
package diff

// Kind marks a hunk line as added, removed, or unchanged context.
type Kind byte

const (
	Context Kind = ' '
	Added   Kind = '+'
	Removed Kind = '-'
)

// Line is one line of a hunk.
type Line struct {
	Kind Kind
	// OldLine is the 1-based line number in the old file (0 for Added).
	OldLine int
	// NewLine is the 1-based line number in the new file (0 for Removed).
	NewLine int
	Text    string
}

// Hunk is one "@@" section of a file diff.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	// Header is the optional text after the closing "@@" (e.g. a function name).
	Header string
	Lines  []Line
}

// File is one file's section of a diff.
type File struct {
	// OldPath and NewPath are the paths without "a/" "b/" prefixes. A created
	// file has OldPath "/dev/null"; a deleted file has NewPath "/dev/null".
	OldPath string
	NewPath string
	Binary  bool
	Hunks   []Hunk
}

// Path is the path the change should be reported under: NewPath, or OldPath
// for a deleted file.
func (f File) Path() string {
	if f.NewPath == "/dev/null" || f.NewPath == "" {
		return f.OldPath
	}
	return f.NewPath
}

// Deleted reports whether the file was deleted.
func (f File) Deleted() bool { return f.NewPath == "/dev/null" }
