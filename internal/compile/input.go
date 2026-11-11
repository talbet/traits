// Package compile turns traits and diff hunks into provider questions and the
// shared input they are asked about. Its output is deterministic and
// golden-tested; any change to produced text must bump Version.
package compile

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/talbet/traits/internal/diff"
)

// Version identifies the question templates and input encoding. It is part of
// cache keys: bump it whenever any produced text changes.
const Version = "1"

// Input is the evidence for one request: one file's change, as hunks or as the
// whole post-change file. It is serialized as the request's JSON input.
type Input struct {
	Path     string `json:"path"`
	Language string `json:"language,omitempty"`
	Hunks    []Hunk `json:"hunks,omitempty"`
	// File is the whole post-change file, one "N| text" entry per line.
	File []string `json:"file,omitempty"`
	// Changed lists the changed new-file line ranges [start, end] of File.
	Changed [][2]int `json:"changed,omitempty"`
}

// Hunk is one diff hunk as sent to the model.
type Hunk struct {
	ID       string `json:"id"`
	OldStart int    `json:"oldStart"`
	NewStart int    `json:"newStart"`
	Header   string `json:"header,omitempty"`
	// Lines are "+N| text" (added, new line N), "-N| text" (removed, old
	// line N) and " N| text" (context, new line N).
	Lines []string `json:"lines"`

	// Start and End are the new-file line range of the hunk's added lines,
	// used to locate findings. They are not sent to the model.
	Start int `json:"-"`
	End   int `json:"-"`
	// RemovedOnly is true when the hunk only removes lines; Start and End
	// are then the new-file line where the removal happened.
	RemovedOnly bool `json:"-"`
}

// HunkInput builds the input for a hunk-scoped request. Hunks get IDs h1..hN
// in order.
func HunkInput(path string, hunks []diff.Hunk) Input {
	in := Input{Path: path, Language: Language(path)}
	for i, h := range hunks {
		ch := Hunk{
			ID:       "h" + strconv.Itoa(i+1),
			OldStart: h.OldStart,
			NewStart: h.NewStart,
			Header:   h.Header,
			Lines:    make([]string, 0, len(h.Lines)),
		}
		// next is the new-file line number of the next line in the hunk.
		next := h.NewStart
		removedAt := 0
		for _, l := range h.Lines {
			var n int
			switch l.Kind {
			case diff.Added:
				n = l.NewLine
				if ch.Start == 0 || n < ch.Start {
					ch.Start = n
				}
				ch.End = max(ch.End, n)
				next = n + 1
			case diff.Removed:
				n = l.OldLine
				if removedAt == 0 {
					removedAt = next
				}
			default:
				n = l.NewLine
				next = n + 1
			}
			ch.Lines = append(ch.Lines, string(rune(l.Kind))+strconv.Itoa(n)+"| "+l.Text)
		}
		if ch.Start == 0 && removedAt > 0 {
			ch.Start, ch.End, ch.RemovedOnly = removedAt, removedAt, true
		}
		in.Hunks = append(in.Hunks, ch)
	}
	return in
}

// FileInput builds the input for a file-scoped request from the post-change
// file's lines and the changed new-file line ranges.
func FileInput(path string, lines []string, changed [][2]int) Input {
	in := Input{Path: path, Language: Language(path), Changed: changed}
	in.File = make([]string, len(lines))
	for i, l := range lines {
		in.File[i] = strconv.Itoa(i+1) + "| " + l
	}
	return in
}

var languages = map[string]string{
	".go":     "go",
	".ts":     "typescript",
	".tsx":    "typescript",
	".mts":    "typescript",
	".cts":    "typescript",
	".js":     "javascript",
	".jsx":    "javascript",
	".mjs":    "javascript",
	".cjs":    "javascript",
	".py":     "python",
	".rs":     "rust",
	".rb":     "ruby",
	".swift":  "swift",
	".kt":     "kotlin",
	".kts":    "kotlin",
	".java":   "java",
	".md":     "markdown",
	".mdx":    "markdown",
	".yaml":   "yaml",
	".yml":    "yaml",
	".json":   "json",
	".toml":   "toml",
	".sh":     "shell",
	".bash":   "shell",
	".zsh":    "shell",
	".sql":    "sql",
	".c":      "c",
	".h":      "c",
	".cc":     "cpp",
	".cpp":    "cpp",
	".cxx":    "cpp",
	".hpp":    "cpp",
	".cs":     "csharp",
	".css":    "css",
	".scss":   "scss",
	".html":   "html",
	".htm":    "html",
	".vue":    "vue",
	".svelte": "svelte",
	".php":    "php",
	".ex":     "elixir",
	".exs":    "elixir",
	".scala":  "scala",
	".dart":   "dart",
	".lua":    "lua",
}

// Language returns the language name for path's extension, or "" if unknown.
func Language(path string) string {
	return languages[strings.ToLower(filepath.Ext(path))]
}
