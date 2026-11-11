package trait

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// FuzzParse checks that Parse never panics, that an accepted trait is usable,
// and that Serialize round-trips it.
func FuzzParse(f *testing.F) {
	paths, _ := filepath.Glob("testdata/traits/*.md")
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	for _, seed := range []string{
		doc(""),
		doc("appliesTo: [\"src/**/*.{ts,tsx}\", \"[ab]?\"]\nwhen: x+\n") + "body\n",
		doc("thresholds: {fix: 0.9, review: 0.4}\nscope: file\nstatus: candidate\n"),
		"\ufeff---\r\nname: a\r\n---\r\n",
		"---\n---\n---\n",
		"---",
		"",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		tr, err := Parse("", data)
		if err != nil {
			return
		}
		fix, review := tr.Effective()
		if !(review <= fix && fix <= 1 && review > 0) {
			t.Fatalf("bad effective thresholds %v/%v", fix, review)
		}
		for _, p := range []string{"", "a.go", "src/a/b.ts", "."} {
			_ = tr.Matches(p)
		}
		_ = tr.Gate([]string{"", "x"})
		again, err := Parse("", Serialize(tr))
		if err != nil {
			t.Fatalf("reparse: %v\n%s", err, Serialize(tr))
		}
		if !reflect.DeepEqual(again, tr) {
			t.Fatalf("round trip:\ngot  %#v\nwant %#v\n%s", again, tr, Serialize(tr))
		}
	})
}
