package compile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/talbet/traits/internal/diff"
	"github.com/talbet/traits/internal/provider"
	"github.com/talbet/traits/internal/trait"
)

var update = flag.Bool("update", false, "rewrite golden files")

var hunkTrait = trait.Trait{
	Name:          "colocate-small-domain-errors",
	Subject:       "Error types defined for a single domain module",
	Requirement:   "Define small domain error types in the module that raises them, not in a shared errors file.",
	Applicability: "Only errors used by one module; shared errors may live in a common package.",
	Purpose:       "Keeps errors discoverable next to the code that raises them.",
	Context:       "\nGood:\n\n```go\n// parse.go\nvar ErrEmpty = errors.New(\"empty\")\n```\n",
	Fix:           "Move the error next to its only caller.",
	AppliesTo:     []string{"**/*.go"},
}

var fileTrait = trait.Trait{
	Name:        "no-default-export",
	Subject:     "TypeScript modules",
	Requirement: "Use named exports; do not use `export default`.",
	Purpose:     "Named exports keep imports greppable & renames safe.",
	Scope:       trait.ScopeFile,
}

func fixtureHunks() []diff.Hunk {
	return []diff.Hunk{
		{
			OldStart: 10, OldLines: 4, NewStart: 10, NewLines: 5, Header: "func Parse(s string) error {",
			Lines: []diff.Line{
				{Kind: diff.Context, OldLine: 10, NewLine: 10, Text: "\tif s == \"\" {"},
				{Kind: diff.Removed, OldLine: 11, Text: "\t\treturn errEmpty"},
				{Kind: diff.Added, NewLine: 11, Text: "\t\treturn errors.ErrEmpty"},
				{Kind: diff.Added, NewLine: 12, Text: "\t\t// was local"},
				{Kind: diff.Context, OldLine: 12, NewLine: 13, Text: "\t}"},
				{Kind: diff.Context, OldLine: 13, NewLine: 14, Text: "\treturn nil"},
			},
		},
		{
			OldStart: 40, OldLines: 3, NewStart: 41, NewLines: 2,
			Lines: []diff.Line{
				{Kind: diff.Context, OldLine: 40, NewLine: 41, Text: ""},
				{Kind: diff.Removed, OldLine: 41, Text: "var errEmpty = errors.New(\"empty\")"},
				{Kind: diff.Context, OldLine: 42, NewLine: 42, Text: "// end"},
			},
		},
	}
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func question(q provider.Question) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\ntype: %s\noptions:\n", q.Name, q.Type)
	for _, o := range q.Options {
		fmt.Fprintf(&b, "  %s: %s\n", o.Value, o.Description)
	}
	fmt.Fprintf(&b, "instructions:\n%s\n", q.Instructions)
	return b.String()
}

// goldens returns every golden output by file name.
func goldens(t *testing.T) map[string]string {
	in := HunkInput("internal/parse/parse.go", fixtureHunks())
	return map[string]string{
		"trait_hunk.golden": question(TraitQuestion(hunkTrait)),
		"trait_file.golden": question(TraitQuestion(fileTrait)),
		"locate.golden":     question(LocateQuestion(hunkTrait, in)),
		"hunk_input.golden": marshal(t, in),
		"file_input.golden": marshal(t, FileInput("src/app.tsx", []string{"import x from \"x\"", "", "export default x"}, [][2]int{{3, 3}})),
	}
}

func TestGolden(t *testing.T) {
	for name, got := range goldens(t) {
		path := filepath.Join("testdata", name)
		if *update {
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s differs (run go test -update after bumping Version):\n--- got\n%s--- want\n%s", name, got, want)
		}
	}
}

// TestVersion fails when produced text changes without a Version bump.
// testdata/version.golden records the Version and a hash of all golden
// outputs; -update refuses to change the hash under the same Version.
func TestVersion(t *testing.T) {
	g := goldens(t)
	h := sha256.New()
	for _, n := range slices.Sorted(maps.Keys(g)) {
		fmt.Fprintf(h, "%s\n%s", n, g[n])
	}
	got := fmt.Sprintf("version %s sha256 %s\n", Version, hex.EncodeToString(h.Sum(nil)))

	path := filepath.Join("testdata", "version.golden")
	want, err := os.ReadFile(path)
	if err != nil && !(*update && os.IsNotExist(err)) {
		t.Fatal(err)
	}
	if got == string(want) {
		return
	}
	prefix := "version " + Version + " "
	if bytes.HasPrefix(want, []byte(prefix)) {
		t.Fatalf("compiled output changed but Version is still %q: bump compile.Version, then run go test -update", Version)
	}
	if !*update {
		t.Fatalf("version.golden is for another Version: run go test -update\n got %s want %s", got, want)
	}
	if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHunkInputRanges(t *testing.T) {
	in := HunkInput("a.go", fixtureHunks())
	h1, h2 := in.Hunks[0], in.Hunks[1]
	if h1.ID != "h1" || h1.Start != 11 || h1.End != 12 || h1.RemovedOnly {
		t.Errorf("h1 = %+v", h1)
	}
	if h2.ID != "h2" || h2.Start != 42 || h2.End != 42 || !h2.RemovedOnly {
		t.Errorf("h2 = %+v", h2)
	}
	if got := h1.Lines[1]; got != "-11| \t\treturn errEmpty" {
		t.Errorf("removed line = %q", got)
	}
}

func TestDeterministic(t *testing.T) {
	a, b := goldens(t), goldens(t)
	for n := range a {
		if a[n] != b[n] {
			t.Errorf("%s not deterministic", n)
		}
	}
}

func TestLanguage(t *testing.T) {
	for path, want := range map[string]string{
		"a/b.go": "go", "x.TSX": "typescript", "README.md": "markdown",
		"run.sh": "shell", "Makefile": "", "a.unknown": "",
	} {
		if got := Language(path); got != want {
			t.Errorf("Language(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestTraitJSONOmitsExtensions(t *testing.T) {
	q := TraitQuestion(hunkTrait)
	for _, s := range []string{"Move the error", "appliesTo", `"fix"`} {
		if strings.Contains(q.Instructions, s) {
			t.Errorf("instructions contain %q", s)
		}
	}
}
