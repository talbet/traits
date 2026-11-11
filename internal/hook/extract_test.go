package hook

import (
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talbet/traits/internal/diff"
)

func payload(t *testing.T, tool string, input any) Payload {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return Payload{HookEventName: "PostToolUse", Cwd: "/repo", ToolName: tool, ToolInput: raw}
}

func disk(files map[string]string) ReadFile {
	return func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, fs.ErrNotExist
	}
}

// added returns "line:text" for the added lines of f.
func added(f diff.File) []string {
	var out []string
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Kind == diff.Added {
				out = append(out, itoa(l.NewLine)+":"+strings.TrimSpace(l.Text))
			}
		}
	}
	return out
}

func removed(f diff.File) []string {
	var out []string
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Kind == diff.Removed {
				out = append(out, l.Text)
			}
		}
	}
	return out
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func eq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func TestClaudeWrite(t *testing.T) {
	cs := Claude(payload(t, "Write", map[string]any{"file_path": "/repo/a.go", "content": "package a\n\nfunc A() {}\n"}), disk(nil))
	if len(cs) != 1 || cs[0].Abs != "/repo/a.go" || cs[0].File.OldPath != "/dev/null" {
		t.Fatalf("changes = %+v", cs)
	}
	eq(t, "added", added(cs[0].File), []string{"1:package a", "2:", "3:func A() {}"})

	// A long write is split into hunks with absolute line numbers.
	long := strings.Repeat("x\n", WholeFileHunk+10)
	cs = Claude(payload(t, "Write", map[string]any{"file_path": "b.go", "content": long}), disk(nil))
	if len(cs[0].File.Hunks) != 2 || cs[0].File.Hunks[1].NewStart != WholeFileHunk+1 || cs[0].Abs != "/repo/b.go" {
		t.Errorf("hunks = %d, second starts %d, abs %s", len(cs[0].File.Hunks), cs[0].File.Hunks[1].NewStart, cs[0].Abs)
	}
}

func TestClaudeEditLocatesLines(t *testing.T) {
	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, "line"+itoa(i))
	}
	lines[9] = "\tfmt.Println(\"debug\")" // line 10
	lines = append(lines[:11], append([]string{"\treturn nil"}, lines[11:]...)...)
	cur := strings.Join(lines, "\n") + "\n"
	p := payload(t, "Edit", map[string]any{
		"file_path":  "/repo/main.go",
		"old_string": "line10\nline11",
		"new_string": "\tfmt.Println(\"debug\")\nline11\n\treturn nil",
	})
	cs := Claude(p, disk(map[string]string{"/repo/main.go": cur}))
	if len(cs) != 1 {
		t.Fatalf("changes = %+v", cs)
	}
	f := cs[0].File
	eq(t, "added", added(f), []string{`10:fmt.Println("debug")`, "12:return nil"})
	eq(t, "removed", removed(f), []string{"line10"})
	if h := f.Hunks[0]; h.NewStart != 7 || h.Lines[0].Text != "line7" {
		t.Errorf("hunk starts at %d with %q; want 3 lines of context from line 7", h.NewStart, h.Lines[0].Text)
	}

	// The edit's text is no longer in the file: check the edit's own text,
	// without a location.
	if cs := Claude(p, disk(map[string]string{"/repo/main.go": "other\n"})); len(cs) != 1 || !cs[0].File.Unlocated {
		t.Errorf("stale edit gave %+v", cs)
	}
	// Unreadable file.
	if cs := Claude(p, disk(nil)); len(cs) != 0 {
		t.Errorf("missing file gave %+v", cs)
	}
}

func TestClaudeEditReplaceAll(t *testing.T) {
	// replace_all cannot tell its replacements from pre-existing matches,
	// so the edit's own text is checked without a location.
	cur := "a := log(1)\nb := log(2)\n"
	p := payload(t, "Edit", map[string]any{"file_path": "/repo/x.go", "old_string": "print(", "new_string": "log(", "replace_all": true})
	cs := Claude(p, disk(map[string]string{"/repo/x.go": cur}))
	if len(cs) != 1 || !cs[0].File.Unlocated {
		t.Fatalf("changes = %+v", cs)
	}
	eq(t, "added", added(cs[0].File), []string{"1:log("})
	eq(t, "removed", removed(cs[0].File), []string{"print("})
}

func TestClaudeEditDeletion(t *testing.T) {
	// new_string "" removes text: its position is gone, but the removal is
	// still checked.
	p := payload(t, "Edit", map[string]any{"file_path": "/repo/x.go", "old_string": "\tdefer cleanup()\n", "new_string": ""})
	cs := Claude(p, disk(map[string]string{"/repo/x.go": "func f() {\n}\n"}))
	if len(cs) != 1 || !cs[0].File.Unlocated {
		t.Fatalf("changes = %+v", cs)
	}
	eq(t, "removed", removed(cs[0].File), []string{"\tdefer cleanup()"})
}

func TestClaudeEditAmbiguous(t *testing.T) {
	// The new text also occurs earlier, unchanged: undoing the first match
	// would blame the wrong line, so no location is claimed.
	cur := "if a {\n\treturn nil\n}\nif b {\n\treturn nil\n}\n"
	p := payload(t, "Edit", map[string]any{"file_path": "/repo/x.go", "old_string": "\treturn err", "new_string": "\treturn nil"})
	cs := Claude(p, disk(map[string]string{"/repo/x.go": cur}))
	if len(cs) != 1 || !cs[0].File.Unlocated {
		t.Fatalf("changes = %+v", cs)
	}
	eq(t, "removed", removed(cs[0].File), []string{"\treturn err"})
}

func TestClaudeMultiEditWithDeletionKeepsOtherEdits(t *testing.T) {
	cur := "a\nB\nc\n"
	p := payload(t, "MultiEdit", map[string]any{
		"file_path": "/repo/m.txt",
		"edits": []map[string]any{
			{"old_string": "b", "new_string": "B"},
			{"old_string": "d\n", "new_string": ""},
		},
	})
	cs := Claude(p, disk(map[string]string{"/repo/m.txt": cur}))
	if len(cs) != 1 {
		t.Fatalf("changes = %+v", cs)
	}
	eq(t, "removed", removed(cs[0].File), []string{"b", "d"})
}

func TestClaudeMultiEdit(t *testing.T) {
	// Original: a b c d e f g h i j (one per line). Edit 1: b -> B1\nB2. Edit 2: B2 -> X (applies to edit 1's output). Edit 3: i -> I.
	cur := "a\nB1\nX\nc\nd\ne\nf\ng\nh\nI\nj\n"
	p := payload(t, "MultiEdit", map[string]any{
		"file_path": "/repo/m.txt",
		"edits": []map[string]any{
			{"old_string": "b", "new_string": "B1\nB2"},
			{"old_string": "B2", "new_string": "X"},
			{"old_string": "i\n", "new_string": "I\n"},
		},
	})
	cs := Claude(p, disk(map[string]string{"/repo/m.txt": cur}))
	if len(cs) != 1 {
		t.Fatal("no change")
	}
	eq(t, "added", added(cs[0].File), []string{"2:B1", "3:X", "10:I"})
	eq(t, "removed", removed(cs[0].File), []string{"b", "i"})
}

func TestClaudeIgnoresOtherTools(t *testing.T) {
	if cs := Claude(payload(t, "Bash", map[string]any{"command": "ls"}), disk(nil)); cs != nil {
		t.Errorf("%+v", cs)
	}
	if cs := Claude(Payload{ToolName: "Write", ToolInput: json.RawMessage(`"x"`)}, disk(nil)); cs != nil {
		t.Errorf("%+v", cs)
	}
}

const codexPatch = `*** Begin Patch
*** Add File: docs/new.md
+# New
+
+text
*** Update File: src/app.py
@@ def main():
     x = 1
-    print(x)
+    log(x)
+    return x
*** Delete File: old.py
*** Update File: src/rename.py
*** Move to: src/renamed.py
@@
-a = 1
+a = 2
*** End Patch`

func TestParsePatch(t *testing.T) {
	fps := ParsePatch(codexPatch)
	if len(fps) != 4 {
		t.Fatalf("files = %+v", fps)
	}
	if fps[0].Op != OpAdd || fps[0].Path != "docs/new.md" || strings.Join(fps[0].Added(), "|") != "# New||text" {
		t.Errorf("add = %+v", fps[0])
	}
	if fps[1].Op != OpUpdate || len(fps[1].Chunks) != 1 || len(fps[1].Chunks[0]) != 4 {
		t.Errorf("update = %+v", fps[1])
	}
	if fps[2].Op != OpDelete || fps[2].Path != "old.py" || len(fps[2].Chunks) != 0 {
		t.Errorf("delete = %+v", fps[2])
	}
	if fps[3].Path != "src/renamed.py" {
		t.Errorf("move = %+v", fps[3])
	}
}

func TestCodexApplyPatch(t *testing.T) {
	app := "import os\n\n\ndef main():\n    x = 1\n    log(x)\n    return x\n"
	read := disk(map[string]string{filepath.FromSlash("/repo/src/app.py"): app})
	for _, input := range []any{
		map[string]any{"command": codexPatch},
		map[string]any{"command": []string{"apply_patch", codexPatch}},
		map[string]any{"patch": codexPatch},
	} {
		cs := Codex(payload(t, "apply_patch", input), read)
		if len(cs) != 3 {
			t.Fatalf("%v: changes = %+v", input, cs)
		}
		if cs[0].Abs != filepath.FromSlash("/repo/docs/new.md") || cs[0].File.OldPath != "/dev/null" {
			t.Errorf("add = %+v", cs[0])
		}
		eq(t, "add lines", added(cs[0].File), []string{"1:# New", "2:", "3:text"})
		// Update located on disk: absolute line numbers.
		eq(t, "update added", added(cs[1].File), []string{"6:log(x)", "7:return x"})
		eq(t, "update removed", removed(cs[1].File), []string{"    print(x)"})
		// Renamed file not on disk: chunk lines numbered from 1.
		if cs[2].Abs != filepath.FromSlash("/repo/src/renamed.py") {
			t.Errorf("move abs = %s", cs[2].Abs)
		}
		eq(t, "fallback", added(cs[2].File), []string{"1:a = 2"})
	}
	if cs := Codex(payload(t, "shell", map[string]any{"command": "ls"}), read); cs != nil {
		t.Errorf("shell gave %+v", cs)
	}
	if cs := Codex(payload(t, "apply_patch", map[string]any{"command": "*** Begin Patch\n*** Delete File: a.py\n*** End Patch"}), read); len(cs) != 0 {
		t.Errorf("delete-only gave %+v", cs)
	}
}
