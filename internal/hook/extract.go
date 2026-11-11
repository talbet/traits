// Package hook turns coding-agent hook payloads (Claude Code and Codex) into
// diffs to check, and installs the hooks into the agents' settings.
package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/talbet/traits/internal/diff"
)

// Payload is the part of a hook's stdin JSON that traits uses. Claude Code
// and Codex share these field names.
type Payload struct {
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	AgentID       string          `json:"agent_id"`
}

// Change is one file changed by a tool call.
type Change struct {
	// Abs is the file's absolute path.
	Abs string
	// File is the change as a diff, with absolute new-file line numbers.
	// Its paths are Abs; callers rename them to repository-relative paths.
	File diff.File
}

// ReadFile reads post-change file contents (os.ReadFile in production).
type ReadFile func(path string) ([]byte, error)

// contextLines is how many unchanged lines surround a reconstructed edit.
const contextLines = 3

// WholeFileHunk is how many lines of a written file go in one hunk.
const WholeFileHunk = 150

type edit struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// Claude extracts the changes from a Claude Code PostToolUse payload for
// Write, Edit or MultiEdit. Write's content is the whole new file. Edit and
// MultiEdit have already been applied, so the file is read from disk, the
// edits are undone in reverse to recover the old content, and the two are
// diffed; an edit whose new text is no longer in the file is dropped.
func Claude(p Payload, read ReadFile) []Change {
	var in struct {
		FilePath string  `json:"file_path"`
		Content  *string `json:"content"`
		edit
		Edits []edit `json:"edits"`
	}
	if json.Unmarshal(p.ToolInput, &in) != nil || in.FilePath == "" {
		return nil
	}
	abs := absPath(p.Cwd, in.FilePath)
	switch p.ToolName {
	case "Write":
		if in.Content == nil {
			return nil
		}
		return []Change{{Abs: abs, File: WholeFile(abs, []byte(*in.Content))}}
	case "Edit":
		return editChange(abs, []edit{in.edit}, read)
	case "MultiEdit":
		return editChange(abs, in.Edits, read)
	}
	return nil
}

func editChange(abs string, edits []edit, read ReadFile) []Change {
	data, err := read(abs)
	if err != nil {
		return nil
	}
	cur := string(data)
	old, ok := undo(cur, edits)
	if !ok {
		return nil
	}
	f := diff.Synthetic(abs, old, cur, contextLines)
	if len(f.Hunks) == 0 {
		return nil
	}
	return []Change{{Abs: abs, File: f}}
}

// undo reverses edits (applied in order) on cur. It fails when an edit's new
// text is empty or absent: its position cannot be recovered.
func undo(cur string, edits []edit) (string, bool) {
	if len(edits) == 0 {
		return "", false
	}
	s := cur
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		if e.NewString == "" || !strings.Contains(s, e.NewString) {
			return "", false
		}
		if e.ReplaceAll {
			s = strings.ReplaceAll(s, e.NewString, e.OldString)
		} else {
			s = strings.Replace(s, e.NewString, e.OldString, 1)
		}
	}
	return s, true
}

// Codex extracts the changes from a Codex PostToolUse payload for
// apply_patch (patch text in tool_input.command, or tool_input.patch).
func Codex(p Payload, read ReadFile) []Change {
	if p.ToolName != "apply_patch" && p.ToolName != "" {
		return nil
	}
	var in struct {
		Command json.RawMessage `json:"command"`
		Patch   string          `json:"patch"`
	}
	if json.Unmarshal(p.ToolInput, &in) != nil {
		return nil
	}
	patch := in.Patch
	if len(in.Command) > 0 {
		var s string
		var argv []string
		switch {
		case json.Unmarshal(in.Command, &s) == nil:
			patch = s
		case json.Unmarshal(in.Command, &argv) == nil:
			// ["apply_patch", "<patch>"] form.
			for _, a := range argv {
				if strings.Contains(a, "*** Begin Patch") {
					patch = a
				}
			}
		}
	}
	var out []Change
	for _, fp := range ParsePatch(patch) {
		abs := absPath(p.Cwd, fp.Path)
		switch fp.Op {
		case OpDelete:
			continue
		case OpAdd:
			out = append(out, Change{Abs: abs, File: WholeFile(abs, []byte(strings.Join(fp.Added(), "\n")))})
		case OpUpdate:
			if c, ok := patchChange(abs, fp, read); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

// patchChange builds an updated file's diff. With the patched file on disk
// each chunk is located in it and undone, giving absolute line numbers.
// Otherwise the chunks' lines are numbered from 1 in the order they appear.
func patchChange(abs string, fp FilePatch, read ReadFile) (Change, bool) {
	var edits []edit
	for _, c := range fp.Chunks {
		var o, n []string
		for _, l := range c {
			if l.Kind != diff.Added {
				o = append(o, l.Text)
			}
			if l.Kind != diff.Removed {
				n = append(n, l.Text)
			}
		}
		edits = append(edits, edit{OldString: strings.Join(o, "\n"), NewString: strings.Join(n, "\n")})
	}
	if data, err := read(abs); err == nil {
		cur := string(data)
		if old, ok := undo(cur, edits); ok {
			f := diff.Synthetic(abs, old, cur, contextLines)
			if len(f.Hunks) == 0 {
				return Change{}, false
			}
			return Change{Abs: abs, File: f}, true
		}
	}
	f := diff.File{OldPath: abs, NewPath: abs}
	oldNo, newNo := 1, 1
	for _, c := range fp.Chunks {
		h := diff.Hunk{OldStart: oldNo, NewStart: newNo}
		for _, l := range c {
			if l.Kind != diff.Added {
				l.OldLine = oldNo
				oldNo++
				h.OldLines++
			}
			if l.Kind != diff.Removed {
				l.NewLine = newNo
				newNo++
				h.NewLines++
			}
			h.Lines = append(h.Lines, l)
		}
		if len(h.Lines) > 0 {
			f.Hunks = append(f.Hunks, h)
		}
	}
	return Change{Abs: abs, File: f}, len(f.Hunks) > 0 && hasChange(f)
}

func hasChange(f diff.File) bool {
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Kind != diff.Context {
				return true
			}
		}
	}
	return false
}

// Patch operations.
const (
	OpAdd    = "add"
	OpUpdate = "update"
	OpDelete = "delete"
)

// FilePatch is one file's section of a Codex apply_patch patch.
type FilePatch struct {
	Op   string
	Path string // the "Move to" path for a renamed file
	// Chunks are the "@@" sections, as lines marked added, removed or
	// context (no line numbers). An added file has a single chunk.
	Chunks [][]diff.Line
}

// Added returns the patch's added lines in order.
func (fp FilePatch) Added() []string {
	var out []string
	for _, c := range fp.Chunks {
		for _, l := range c {
			if l.Kind == diff.Added {
				out = append(out, l.Text)
			}
		}
	}
	return out
}

// ParsePatch parses the Codex apply_patch format:
//
//	*** Begin Patch
//	*** Add File: path      (+ lines)
//	*** Update File: path   (optional "*** Move to: path", "@@" chunks of
//	                         " ", "-" and "+" lines)
//	*** Delete File: path
//	*** End Patch
//
// Unknown lines are ignored.
func ParsePatch(patch string) []FilePatch {
	var out []FilePatch
	var cur *FilePatch
	var chunk []diff.Line
	flushChunk := func() {
		if cur != nil && len(chunk) > 0 {
			cur.Chunks = append(cur.Chunks, chunk)
		}
		chunk = nil
	}
	flush := func() {
		flushChunk()
		if cur != nil {
			out = append(out, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n") {
		if op, path, ok := patchHeader(line); ok {
			flush()
			cur = &FilePatch{Op: op, Path: path}
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "*** Move to: "):
			cur.Path = strings.TrimSpace(strings.TrimPrefix(line, "*** Move to: "))
		case strings.HasPrefix(line, "***"):
			// "*** End Patch", "*** End of File".
		case strings.HasPrefix(line, "@@"):
			flushChunk()
		case cur.Op == OpDelete:
		case strings.HasPrefix(line, "+"):
			chunk = append(chunk, diff.Line{Kind: diff.Added, Text: line[1:]})
		case cur.Op == OpAdd:
		case strings.HasPrefix(line, "-"):
			chunk = append(chunk, diff.Line{Kind: diff.Removed, Text: line[1:]})
		case strings.HasPrefix(line, " "):
			chunk = append(chunk, diff.Line{Kind: diff.Context, Text: line[1:]})
		case line == "":
			// A blank context line whose leading space was stripped.
			if len(chunk) > 0 {
				chunk = append(chunk, diff.Line{Kind: diff.Context})
			}
		}
	}
	flush()
	for i := range out {
		// Drop trailing blank context lines picked up before the next header.
		for j, c := range out[i].Chunks {
			for len(c) > 0 && c[len(c)-1].Kind == diff.Context && c[len(c)-1].Text == "" {
				c = c[:len(c)-1]
			}
			out[i].Chunks[j] = c
		}
	}
	return out
}

func patchHeader(line string) (op, path string, ok bool) {
	for prefix, op := range map[string]string{
		"*** Add File: ":    OpAdd,
		"*** Update File: ": OpUpdate,
		"*** Delete File: ": OpDelete,
	} {
		if p, found := strings.CutPrefix(line, prefix); found {
			return op, strings.TrimSpace(p), true
		}
	}
	return "", "", false
}

// WholeFile presents content as a new file at path, split into hunks of
// WholeFileHunk lines.
func WholeFile(path string, data []byte) diff.File {
	f := diff.File{OldPath: "/dev/null", NewPath: path}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return f
	}
	lines := strings.Split(text, "\n")
	for start := 0; start < len(lines); start += WholeFileHunk {
		end := min(start+WholeFileHunk, len(lines))
		h := diff.Hunk{NewStart: start + 1, NewLines: end - start}
		for i := start; i < end; i++ {
			h.Lines = append(h.Lines, diff.Line{Kind: diff.Added, NewLine: i + 1, Text: strings.TrimSuffix(lines[i], "\r")})
		}
		f.Hunks = append(f.Hunks, h)
	}
	return f
}

func absPath(cwd, p string) string {
	if !filepath.IsAbs(p) {
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

// Relabel returns f with its paths set to rel (keeping /dev/null).
func Relabel(f diff.File, rel string) diff.File {
	if f.OldPath != "/dev/null" {
		f.OldPath = rel
	}
	if f.NewPath != "/dev/null" {
		f.NewPath = rel
	}
	return f
}
