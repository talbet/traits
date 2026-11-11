package trait

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validFM = "name: sample\nsubject: Things.\nrequirement: Be good.\npurpose: Because.\n"

func doc(extra string) string { return "---\n" + validFM + extra + "---\n" }

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want Trait
	}{
		{
			name: "minimal with defaults",
			src:  doc(""),
			want: Trait{Name: "sample", Subject: "Things.", Requirement: "Be good.", Purpose: "Because.",
				Scope: ScopeHunk, Status: StatusActive, Path: "sample.md"},
		},
		{
			name: "string appliesTo and when",
			src:  doc("appliesTo: \"*.go\"\nwhen: \"func \\\\w+\"\nunknownKey: 3\n") + "\n\nbody text\n\n",
			want: Trait{Name: "sample", Subject: "Things.", Requirement: "Be good.", Purpose: "Because.",
				Context: "body text", AppliesTo: []string{"*.go"}, When: []string{`func \w+`},
				Scope: ScopeHunk, Status: StatusActive, Path: "sample.md"},
		},
		{
			name: "list fields and extensions",
			src: doc("appliesTo: [\"*.ts\", \"src/**\"]\nwhen:\n  - a\n  - b\nfix: Do it.\nscope: file\n" +
				"status: candidate\nthresholds: { fix: 0.9, review: 0.6 }\napplicability: Sometimes.\n"),
			want: Trait{Name: "sample", Subject: "Things.", Requirement: "Be good.", Applicability: "Sometimes.",
				Purpose: "Because.", AppliesTo: []string{"*.ts", "src/**"}, When: []string{"a", "b"}, Fix: "Do it.",
				Scope: ScopeFile, Status: StatusCandidate, Thresholds: Thresholds{Fix: 0.9, Review: 0.6}, Path: "sample.md"},
		},
		{
			name: "CRLF and BOM",
			src:  "\ufeff" + strings.ReplaceAll(doc("")+"line one\nline two\n", "\n", "\r\n"),
			want: Trait{Name: "sample", Subject: "Things.", Requirement: "Be good.", Purpose: "Because.",
				Context: "line one\nline two", Scope: ScopeHunk, Status: StatusActive, Path: "sample.md"},
		},
		{
			name: "body containing delimiter",
			src:  doc("") + "before\n---\nafter\n",
			want: Trait{Name: "sample", Subject: "Things.", Requirement: "Be good.", Purpose: "Because.",
				Context: "before\n---\nafter", Scope: ScopeHunk, Status: StatusActive, Path: "sample.md"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse("sample.md", []byte(tt.src))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, path, src, want string
	}{
		{"no frontmatter", "sample.md", "name: x\n", "missing frontmatter"},
		{"unclosed", "sample.md", "---\n" + validFM, "unclosed frontmatter"},
		{"bad yaml", "sample.md", "---\nname: [\n---\n", "invalid frontmatter"},
		{"name missing", "", "---\nsubject: s\nrequirement: r\npurpose: p\n---\n", "name is required"},
		{"name not kebab", "", strings.Replace(doc(""), "sample", "Sample_Name", 1), "not kebab-case"},
		{"name double hyphen", "", strings.Replace(doc(""), "sample", "a--b", 1), "not kebab-case"},
		{"name trait prefix", "", strings.Replace(doc(""), "sample", "trait-x", 1), `must not start with "trait-"`},
		{"name too long", "", strings.Replace(doc(""), "sample", strings.Repeat("a", 81), 1), "longer than 80"},
		{"name not stem", "other.md", doc(""), `does not match filename stem "other"`},
		{"subject blank", "", strings.Replace(doc(""), "Things.", "' '", 1), "subject is required"},
		{"requirement missing", "", strings.Replace(doc(""), "requirement: Be good.\n", "", 1), "requirement is required"},
		{"purpose blank", "", strings.Replace(doc(""), "Because.", `""`, 1), "purpose is required"},
		{"applicability blank", "", doc("applicability: ' '\n"), "applicability is blank"},
		{"bad scope", "", doc("scope: repo\n"), `scope "repo"`},
		{"bad status", "", doc("status: draft\n"), `status "draft"`},
		{"threshold above 1", "", doc("thresholds: {fix: 1.5}\n"), "thresholds.fix 1.5"},
		{"threshold negative", "", doc("thresholds: {review: -0.1}\n"), "thresholds.review -0.1"},
		{"review above fix", "", doc("thresholds: {fix: 0.6, review: 0.7}\n"), "must not exceed"},
		{"bad when", "", doc("when: \"(\"\n"), `when "("`},
		{"empty when", "", doc("when: [\"\"]\n"), "when: empty pattern"},
		{"bad glob", "", doc("appliesTo: \"[a\"\n"), `appliesTo "[a"`},
		{"unclosed brace", "", doc("appliesTo: \"{a,b\"\n"), "unclosed {"},
		{"appliesTo not string", "", doc("appliesTo: {a: b}\n"), "invalid frontmatter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.path, []byte(tt.src))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
			if tt.path != "" && !strings.HasPrefix(err.Error(), tt.path+": ") {
				t.Errorf("err %q does not name the path", err)
			}
		})
	}
}

func TestParseJoinsValidationErrors(t *testing.T) {
	_, err := Parse("", []byte("---\nname: x\nscope: nope\n---\n"))
	for _, want := range []string{"subject is required", "requirement is required", "purpose is required", `scope "nope"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want containing %q", err, want)
		}
	}
}

func TestEffective(t *testing.T) {
	tests := []struct {
		th               Thresholds
		wantFix, wantRev float64
	}{
		{Thresholds{}, 0.8, 0.5},
		{Thresholds{Fix: 0.9}, 0.9, 0.5},
		{Thresholds{Review: 0.3}, 0.8, 0.3},
		{Thresholds{Fix: 0.4}, 0.4, 0.4},
		{Thresholds{Fix: 0.7, Review: 0.6}, 0.7, 0.6},
	}
	for _, tt := range tests {
		fix, rev := Trait{Thresholds: tt.th}.Effective()
		if fix != tt.wantFix || rev != tt.wantRev {
			t.Errorf("%+v: got %v/%v, want %v/%v", tt.th, fix, rev, tt.wantFix, tt.wantRev)
		}
	}
}

func TestMatches(t *testing.T) {
	tests := []struct {
		globs []string
		path  string
		want  bool
	}{
		{nil, "anything/at/all.txt", true},
		{[]string{"*.go"}, "main.go", true},
		{[]string{"*.go"}, "cmd/traits/main.go", true}, // basename
		{[]string{"*.go"}, "main.ts", false},
		{[]string{"*.go"}, "./cmd/main.go", true},
		{[]string{"cmd/*.go"}, "cmd/main.go", true},
		{[]string{"cmd/*.go"}, "x/cmd/main.go", false}, // has "/": full path only
		{[]string{"src/**/*.ts"}, "src/a.ts", true},
		{[]string{"src/**/*.ts"}, "src/a/b/c.ts", true},
		{[]string{"src/**/*.ts"}, "lib/src/a.ts", false},
		{[]string{"src/**/*.ts"}, "src/a/b/c.tsx", false},
		{[]string{"**/test/**"}, "test/a.go", true},
		{[]string{"**/test/**"}, "a/b/test/c/d.go", true},
		{[]string{"**/test/**"}, "a/testing/c.go", false},
		{[]string{"src/**"}, "src/x/y.go", true},
		{[]string{"src/**"}, "srcx/y.go", false},
		{[]string{"**"}, "a/b/c", true},
		{[]string{"{a,b}.md"}, "a.md", true},
		{[]string{"{a,b}.md"}, "docs/b.md", true},
		{[]string{"{a,b}.md"}, "c.md", false},
		{[]string{"*.{ts,tsx}"}, "x/y.tsx", true},
		{[]string{"{src,lib}/**/*.go"}, "lib/x.go", true},
		{[]string{"{src/*.go,*.md}"}, "docs/readme.md", true},
		{[]string{"{src/*.go,*.md}"}, "x/src/a.go", false},
		{[]string{"file?.txt"}, "file1.txt", true},
		{[]string{"[ab]*.txt"}, "dir/bee.txt", true},
		{[]string{"[^ab]*.txt"}, "bee.txt", false},
		{[]string{"\\{x\\}.txt"}, "{x}.txt", true},
		{[]string{"*.md", "*.go"}, "a.go", true},
		{[]string{"[a"}, "[a", false}, // invalid never matches
	}
	for _, tt := range tests {
		if got := (Trait{AppliesTo: tt.globs}).Matches(tt.path); got != tt.want {
			t.Errorf("%q.Matches(%q) = %v, want %v", tt.globs, tt.path, got, tt.want)
		}
	}
}

func TestGate(t *testing.T) {
	tests := []struct {
		when    []string
		changed []string
		want    bool
	}{
		{nil, nil, true},
		{[]string{`class \w+Error`}, []string{"x := 1", "class ParseError extends Error {"}, true},
		{[]string{`class \w+Error`}, []string{"x := 1"}, false},
		{[]string{`^\s*import`, `useMemo`}, []string{"const a = useMemo(f)"}, true},
		{[]string{"x"}, nil, false},
		{[]string{"("}, []string{"("}, false}, // invalid never matches
	}
	for _, tt := range tests {
		if got := (Trait{When: tt.when}).Gate(tt.changed); got != tt.want {
			t.Errorf("%q.Gate(%q) = %v, want %v", tt.when, tt.changed, got, tt.want)
		}
	}
}

func TestLoad(t *testing.T) {
	traits, err := Load("testdata/traits")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tr := range traits {
		names = append(names, tr.Name)
		if tr.Path != tr.Name+".md" {
			t.Errorf("Path = %q", tr.Path)
		}
	}
	want := []string{"align-operation-module-and-export-names", "alphabetical-unordered-configuration",
		"arguments-for-cli-query-variants", "atom-first-react-state-identity"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %q", names)
	}
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func named(name string) string {
	return "---\nname: " + name + "\nsubject: s\nrequirement: r\npurpose: p\n---\n"
}

func TestLoadSkipsAndSorts(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "zeta.md", named("zeta"))
	write(t, dir, "sub/alpha.md", named("alpha"))
	write(t, dir, ".cache/broken.md", "not a trait")
	write(t, dir, "sub/.hidden.md", "not a trait")
	write(t, dir, "zeta.cases.jsonl", "{}")
	write(t, dir, "config.yaml", "x: 1")
	traits, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(traits) != 2 || traits[0].Name != "alpha" || traits[0].Path != "sub/alpha.md" || traits[1].Name != "zeta" {
		t.Errorf("got %+v", traits)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a/dup.md", named("dup"))
	write(t, dir, "b/dup.md", named("dup"))
	write(t, dir, "bad.md", "---\nname: bad\n---\n")
	write(t, dir, "good.md", named("good"))
	traits, err := Load(dir)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{`b/dup.md: duplicate trait name "dup" (also in a/dup.md)`, "bad.md: subject is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err, want)
		}
	}
	if len(traits) != 2 || traits[0].Name != "dup" || traits[1].Name != "good" {
		t.Errorf("valid traits not returned: %+v", traits)
	}
}

func TestLoadMissingDir(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope"))
	if !errors.Is(err, ErrNoDir) {
		t.Fatalf("err = %v, want ErrNoDir", err)
	}
}
