package diff

import (
	"os"
	"testing"
)

// TestParseRealGit parses output captured from git 2.56 (`git diff --cached -M`)
// covering paths with spaces (trailing-tab headers), C-quoted non-ASCII
// paths, a rename, a delete, a new file, a binary file and a missing final
// newline.
func TestParseRealGit(t *testing.T) {
	f, err := os.Open("testdata/real-git.diff")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	files, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		old, new string
		binary   bool
		hunks    int
	}{
		{"foo bar.go", "foo bar.go", false, 1},
		{"gone.go", "/dev/null", false, 1},
		{"img.bin", "img.bin", true, 0},
		{"/dev/null", "new file.go", false, 1},
		{"old.txt", "new.txt", false, 1},
		{"nonl.txt", "nonl.txt", false, 1},
		{"ünï.go", "ünï.go", false, 1},
	}
	if len(files) != len(want) {
		t.Fatalf("got %d files, want %d: %+v", len(files), len(want), files)
	}
	for i, w := range want {
		g := files[i]
		if g.OldPath != w.old || g.NewPath != w.new || g.Binary != w.binary || len(g.Hunks) != w.hunks {
			t.Errorf("file %d = {%q %q binary=%v hunks=%d}, want %+v", i, g.OldPath, g.NewPath, g.Binary, len(g.Hunks), w)
		}
	}
	h := files[0].Hunks[0]
	if h.NewLines != 4 || h.Lines[2].Text != "B" || h.Lines[2].NewLine != 2 || h.Lines[4].NewLine != 4 {
		t.Errorf("foo bar.go hunk = %+v", h)
	}
	if got := files[5].Hunks[0]; len(got.Lines) != 4 {
		t.Errorf("nonl.txt lines = %+v", got.Lines)
	}
}
