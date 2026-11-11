package diff

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func parse(t *testing.T, src string) []File {
	t.Helper()
	files, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return files
}

const modify = `commit 1234
Author: x

    message --- with dashes

diff --git a/main.go b/main.go
index 1111111..2222222 100644
--- a/main.go
+++ b/main.go
@@ -1,4 +1,5 @@ package main
 package main
-func a() {}
+func b() {}
+func c() {}
 
 // end
@@ -10 +11 @@
-x
+y
diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -3,0 +4,2 @@
+one
+two
`

func TestParseModifyMultipleFilesAndHunks(t *testing.T) {
	files := parse(t, modify)
	want := []File{
		{OldPath: "main.go", NewPath: "main.go", Hunks: []Hunk{
			{OldStart: 1, OldLines: 4, NewStart: 1, NewLines: 5, Header: "package main", Lines: []Line{
				{Context, 1, 1, "package main"},
				{Removed, 2, 0, "func a() {}"},
				{Added, 0, 2, "func b() {}"},
				{Added, 0, 3, "func c() {}"},
				{Context, 3, 4, ""},
				{Context, 4, 5, "// end"},
			}},
			{OldStart: 10, OldLines: 1, NewStart: 11, NewLines: 1, Lines: []Line{
				{Removed, 10, 0, "x"},
				{Added, 0, 11, "y"},
			}},
		}},
		{OldPath: "README.md", NewPath: "README.md", Hunks: []Hunk{
			{OldStart: 3, OldLines: 0, NewStart: 4, NewLines: 2, Lines: []Line{
				{Added, 0, 4, "one"},
				{Added, 0, 5, "two"},
			}},
		}},
	}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("got  %+v\nwant %+v", files, want)
	}
	if got := files[0].ChangedText(); !reflect.DeepEqual(got, []string{"func a() {}", "func b() {}", "func c() {}", "x", "y"}) {
		t.Errorf("ChangedText = %q", got)
	}
}

func TestParsePaths(t *testing.T) {
	tests := []struct {
		name, src       string
		old, new        string
		binary, deleted bool
		hunks           int
	}{
		{"new file", "diff --git a/n.go b/n.go\nnew file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ b/n.go\n@@ -0,0 +1 @@\n+x\n",
			"/dev/null", "n.go", false, false, 1},
		{"empty new file", "diff --git a/e.txt b/e.txt\nnew file mode 100644\nindex 0000000..e69de29\n",
			"/dev/null", "e.txt", false, false, 0},
		{"delete", "diff --git a/d.go b/d.go\ndeleted file mode 100644\n--- a/d.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-a\n-b\n",
			"d.go", "/dev/null", false, true, 1},
		{"pure rename", "diff --git a/old name.go b/new name.go\nsimilarity index 100%\nrename from old name.go\nrename to new name.go\n",
			"old name.go", "new name.go", false, false, 0},
		{"rename with edit", "diff --git a/x/a.go b/y/b.go\nsimilarity index 90%\nrename from x/a.go\nrename to y/b.go\n--- a/x/a.go\n+++ b/y/b.go\n@@ -1 +1 @@\n-a\n+b\n",
			"x/a.go", "y/b.go", false, false, 1},
		{"binary", "diff --git a/img.png b/img.png\nindex 1..2 100644\nBinary files a/img.png and b/img.png differ\n",
			"img.png", "img.png", true, false, 0},
		{"git binary patch", "diff --git a/b.bin b/b.bin\nnew file mode 100644\nGIT binary patch\nliteral 3\nKcmZQzU|;|M00aO5\n\nliteral 0\nHcmV?d00001\n\n",
			"/dev/null", "b.bin", true, false, 0},
		{"spaces", "diff --git a/foo bar.go b/foo bar.go\n--- a/foo bar.go\t\n+++ b/foo bar.go\t\n@@ -1 +1 @@\n-a\n+b\n",
			"foo bar.go", "foo bar.go", false, false, 1},
		{"spaces header only", "diff --git a/foo b/bar.go b/foo b/bar.go\nold mode 100644\nnew mode 100755\n",
			"foo b/bar.go", "foo b/bar.go", false, false, 0},
		{"quoted", "diff --git \"a/t\\tab.go\" \"b/t\\tab.go\"\n--- \"a/t\\tab.go\"\n+++ \"b/t\\tab.go\"\n@@ -1 +1 @@\n-a\n+b\n",
			"t\tab.go", "t\tab.go", false, false, 1},
		{"quoted octal utf8", "diff --git \"a/\\303\\274.go\" \"b/\\303\\274.go\"\nnew file mode 100644\n--- /dev/null\n+++ \"b/\\303\\274.go\"\n@@ -0,0 +1 @@\n+x\n",
			"/dev/null", "ü.go", false, false, 1},
		{"quoted rename", "diff --git \"a/q\\\"x\" b/plain\nrename from \"q\\\"x\"\nrename to plain\n",
			"q\"x", "plain", false, false, 0},
		{"no prefix", "diff --git main.go main.go\n--- main.go\n+++ main.go\n@@ -1 +1 @@\n-a\n+b\n",
			"main.go", "main.go", false, false, 1},
		{"plain unified", "--- old/f.txt\t2024-01-01 00:00:00\n+++ new/f.txt\t2024-01-02 00:00:00\n@@ -1,2 +1,2 @@\n a\n-b\n+c\n",
			"old/f.txt", "new/f.txt", false, false, 1},
		{"plain with a/ b/", "--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-b\n+c\n",
			"f.txt", "f.txt", false, false, 1},
		{"plain binary", "Binary files a/x.bin and b/x.bin differ\n", "x.bin", "x.bin", true, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := parse(t, tt.src)
			if len(files) != 1 {
				t.Fatalf("got %d files: %+v", len(files), files)
			}
			f := files[0]
			if f.OldPath != tt.old || f.NewPath != tt.new || f.Binary != tt.binary || f.Deleted() != tt.deleted || len(f.Hunks) != tt.hunks {
				t.Errorf("got old=%q new=%q binary=%v deleted=%v hunks=%d", f.OldPath, f.NewPath, f.Binary, f.Deleted(), len(f.Hunks))
			}
		})
	}
}

func TestPath(t *testing.T) {
	if p := (File{OldPath: "a", NewPath: devNull}).Path(); p != "a" {
		t.Errorf("deleted Path = %q", p)
	}
	if p := (File{OldPath: devNull, NewPath: "b"}).Path(); p != "b" {
		t.Errorf("new Path = %q", p)
	}
}

func TestParseCRLFAndNoNewline(t *testing.T) {
	src := "diff --git a/w.txt b/w.txt\r\n--- a/w.txt\r\n+++ b/w.txt\r\n@@ -1,2 +1,2 @@\r\n a\r\n-b\r\n\\ No newline at end of file\r\n+c\r\n\\ No newline at end of file\r\n"
	files := parse(t, src)
	want := []Line{{Context, 1, 1, "a"}, {Removed, 2, 0, "b"}, {Added, 0, 2, "c"}}
	if got := files[0].Hunks[0].Lines; !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestParseTolerated(t *testing.T) {
	tests := []struct{ name, src string }{
		{"stripped empty context line", "--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n a\n\n-b\n+c\n"},
		{"format-patch signature", "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-b\n+c\n-- \n2.43.0\n\n"},
		{"trailing blank line", "--- a/f\n+++ b/f\n@@ -1 +1 @@\n-b\n+c\n\n"},
		{"diff -ru noise", "diff -ru a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-b\n+c\nOnly in b: g\n"},
		{"removed line that looks like a header", "--- a/f\n+++ b/f\n@@ -1,2 +1 @@\n--- x\n-+++ y\n+z\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := parse(t, tt.src)
			if len(files) != 1 || len(files[0].Hunks) != 1 {
				t.Fatalf("got %+v", files)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct{ name, src, want string }{
		{"too few lines", "--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n a\n-b\n+c\ndiff --git a/g b/g\n", "line 7: hunk at line 3 ends early"},
		{"truncated at eof", "--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n a\n", "line 4: unexpected end of input"},
		{"too many added", "--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n+c\n", "line 6: unexpected \"+c\" after a complete hunk"},
		{"too many in hunk", "--- a/f\n+++ b/f\n@@ -1 +1,2 @@\n-a\n-b\n", "line 5: hunk at line 3 has more lines"},
		{"malformed header", "--- a/f\n+++ b/f\n@@ -x +1 @@\n", "line 3: malformed hunk header"},
		{"zero counts", "--- a/f\n+++ b/f\n@@ -1,0 +1,0 @@\n", "line 3: hunk header declares no lines"},
		{"line zero", "--- a/f\n+++ b/f\n@@ -0,1 +1 @@\n-a\n+b\n", "line 3: hunk header has line 0"},
		{"huge number", "--- a/f\n+++ b/f\n@@ -99999999999999999999 +1 @@\n", "out of range"},
		{"hunk without file", "@@ -1 +1 @@\n-a\n+b\n", "line 1: hunk before any file header"},
		{"bad quote", "diff --git \"a/x b/x\n", "line 1: unterminated quoted path"},
		{"combined", "diff --cc f\n", "combined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.src))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseNoDiff(t *testing.T) {
	for _, src := range []string{"", " \n\n", "\t"} {
		if files, err := Parse(strings.NewReader(src)); err != nil || len(files) != 0 {
			t.Errorf("Parse(%q) = %v, %v; want no files, no error", src, files, err)
		}
	}
	for _, src := range []string{"hello\nworld\n", "commit abc\n\n    msg\n"} {
		if _, err := Parse(strings.NewReader(src)); !errors.Is(err, ErrNoDiff) {
			t.Errorf("Parse(%q) err = %v, want ErrNoDiff", src, err)
		}
	}
}

// format renders files as a unified diff Parse accepts.
func format(files []File) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "--- %s\n+++ %s\n", f.OldPath, f.NewPath)
		for _, h := range f.Hunks {
			fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
			for _, l := range h.Lines {
				fmt.Fprintf(&b, "%c%s\n", l.Kind, l.Text)
			}
		}
	}
	return b.String()
}

// apply rebuilds the new content from old and f's hunks.
func apply(old string, f File) []string {
	a := splitContent(old)
	var out []string
	next := 0 // index into a of the first line not yet copied
	for _, h := range f.Hunks {
		first := h.OldStart - 1
		if h.OldLines == 0 {
			first = h.OldStart
		}
		out = append(out, a[next:first]...)
		next = first
		for _, l := range h.Lines {
			switch l.Kind {
			case Context:
				out = append(out, l.Text)
				next++
			case Removed:
				next++
			case Added:
				out = append(out, l.Text)
			}
		}
	}
	return append(out, a[next:]...)
}

func TestSynthetic(t *testing.T) {
	tests := []struct {
		name, old, new string
		context        int
		hunks          int
	}{
		{"identical", "a\nb\n", "a\nb\n", 3, 0},
		{"new file", "", "a\nb\n", 3, 1},
		{"emptied", "a\nb\n", "", 3, 1},
		{"middle edit", "1\n2\n3\n4\n5\n6\n7\n8\n9\n", "1\n2\n3\n4\nX\n6\n7\n8\n9\n", 2, 1},
		{"two far edits", "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n", "X\n2\n3\n4\n5\n6\n7\n8\n9\nY\n", 2, 2},
		{"two near edits merge", "1\n2\n3\n4\n5\n6\n", "X\n2\n3\n4\n5\nY\n", 2, 1},
		{"insert", "a\nc\n", "a\nb\nc\n", 0, 1},
		{"delete", "a\nb\nc\n", "a\nc\n", 0, 1},
		{"interleaved", "a\nb\nc\nd\ne\n", "b\nx\nc\ne\ny\n", 1, 1},
		{"negative context", "a\nb\n", "a\nc\n", -1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := Synthetic("p.go", tt.old, tt.new, tt.context)
			if f.OldPath != "p.go" || f.NewPath != "p.go" {
				t.Errorf("paths %q %q", f.OldPath, f.NewPath)
			}
			if len(f.Hunks) != tt.hunks {
				t.Errorf("hunks = %d, want %d: %+v", len(f.Hunks), tt.hunks, f.Hunks)
			}
			checkSynthetic(t, f, tt.old, tt.new, max(tt.context, 0))
		})
	}
}

func TestSyntheticExactHunk(t *testing.T) {
	f := Synthetic("p", "1\n2\n3\n4\n5\n", "1\n2\nX\n4\n5\n", 1)
	want := []Hunk{{OldStart: 2, OldLines: 3, NewStart: 2, NewLines: 3, Lines: []Line{
		{Context, 2, 2, "2"}, {Removed, 3, 0, "3"}, {Added, 0, 3, "X"}, {Context, 4, 4, "4"},
	}}}
	if !reflect.DeepEqual(f.Hunks, want) {
		t.Errorf("got %+v", f.Hunks)
	}
}

func TestSyntheticLargeFallsBack(t *testing.T) {
	var a, b strings.Builder
	for i := range 3000 {
		fmt.Fprintf(&a, "a%d\n", i)
		fmt.Fprintf(&b, "b%d\n", i)
	}
	f := Synthetic("big", "same\n"+a.String()+"end\n", "same\n"+b.String()+"end\n", 1)
	if len(f.Hunks) != 1 || len(f.Hunks[0].Lines) != 6002 {
		t.Fatalf("hunks %d", len(f.Hunks))
	}
	checkSynthetic(t, f, "same\n"+a.String()+"end\n", "same\n"+b.String()+"end\n", 1)
}

// checkSynthetic verifies f reproduces new from old, survives a format/Parse
// round trip, and keeps at most context lines around changes.
func checkSynthetic(t *testing.T, f File, old, new string, context int) {
	t.Helper()
	if got, want := apply(old, f), splitContent(new); !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
		t.Fatalf("apply = %q, want %q", got, want)
	}
	checkLineNumbers(t, f)
	for _, h := range f.Hunks {
		lead := 0
		for lead < len(h.Lines) && h.Lines[lead].Kind == Context {
			lead++
		}
		if lead > context {
			t.Errorf("hunk has %d leading context lines, want <= %d", lead, context)
		}
	}
	if len(f.Hunks) == 0 {
		return
	}
	again, err := Parse(strings.NewReader(format([]File{f})))
	if err != nil {
		t.Fatalf("reparse: %v\n%s", err, format([]File{f}))
	}
	if !reflect.DeepEqual(again[0].Hunks, f.Hunks) {
		t.Errorf("reparse differs:\ngot  %+v\nwant %+v", again[0].Hunks, f.Hunks)
	}
}

// checkLineNumbers verifies that each hunk's lines match its header counts
// and are numbered consecutively from its start lines.
func checkLineNumbers(t *testing.T, f File) {
	t.Helper()
	for hi, h := range f.Hunks {
		oldNo, newNo := h.OldStart, h.NewStart
		var nOld, nNew int
		for _, l := range h.Lines {
			switch l.Kind {
			case Context:
				if l.OldLine != oldNo || l.NewLine != newNo {
					t.Fatalf("hunk %d: context line numbered %d/%d, want %d/%d", hi, l.OldLine, l.NewLine, oldNo, newNo)
				}
				oldNo++
				newNo++
				nOld++
				nNew++
			case Removed:
				if l.OldLine != oldNo || l.NewLine != 0 {
					t.Fatalf("hunk %d: removed line numbered %d/%d, want %d/0", hi, l.OldLine, l.NewLine, oldNo)
				}
				oldNo++
				nOld++
			case Added:
				if l.NewLine != newNo || l.OldLine != 0 {
					t.Fatalf("hunk %d: added line numbered %d/%d, want 0/%d", hi, l.OldLine, l.NewLine, newNo)
				}
				newNo++
				nNew++
			default:
				t.Fatalf("hunk %d: bad kind %q", hi, l.Kind)
			}
		}
		if nOld != h.OldLines || nNew != h.NewLines {
			t.Fatalf("hunk %d: %d/%d lines, header says %d/%d", hi, nOld, nNew, h.OldLines, h.NewLines)
		}
		if (h.OldLines > 0 && h.OldStart < 1) || (h.NewLines > 0 && h.NewStart < 1) {
			t.Fatalf("hunk %d: start 0 with lines", hi)
		}
	}
}
