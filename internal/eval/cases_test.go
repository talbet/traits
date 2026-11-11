package eval

import (
	"strings"
	"testing"

	"github.com/talbet/traits/internal/trait"
)

func TestParseValid(t *testing.T) {
	in := `{"id":"a","split":"dev","path":"./src/x.go","diff":"@@ -1,1 +1,2 @@\n package x\n+var y = 1\n","expect":"violation","note":"n"}

{"split":"holdout","path":"src/y.go","content":"package y\n","expect":"not-applicable"}
{"split":"dev","path":"src/z.go","diff":"--- /dev/null\n+++ b/src/z.go\n@@ -0,0 +1,2 @@\n+package z\n+var q = 2\n","expect":"compliant"}
`
	cs, err := Parse("t.cases.jsonl", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 {
		t.Fatalf("got %d cases", len(cs))
	}
	a := cs[0]
	if a.Path != "src/x.go" || a.File.Path() != "src/x.go" || len(a.File.Hunks) != 1 || !a.Positive() || a.Label() != "a" {
		t.Errorf("case a = %+v", a)
	}
	if _, ok := a.PostImage(); ok {
		t.Error("a modifying diff has no post-image")
	}
	if b := cs[1]; b.Source != "t.cases.jsonl:3" || b.Label() != "t.cases.jsonl:3" || b.Split != SplitHoldout {
		t.Errorf("case b = %+v", b)
	}
	if post, ok := cs[2].PostImage(); !ok || string(post) != "package z\nvar q = 2\n" {
		t.Errorf("new-file post-image = %q, %v", post, ok)
	}
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{`{"split":"dev","path":"a.go","content":"x","expect":"bad"}`, `unknown expect "bad"`},
		{`{"split":"test","path":"a.go","content":"x","expect":"compliant"}`, `unknown split "test"`},
		{`{"path":"a.go","content":"x","expect":"compliant"}`, `missing "split"`},
		{`{"split":"dev","path":"a.go","content":"x"}`, `missing "expect"`},
		{`{"split":"dev","path":" ","content":"x","expect":"compliant"}`, `empty "path"`},
		{`{"split":"dev","path":"a.go","content":"x","diff":"@@ -1 +1 @@\n-a\n+b\n","expect":"compliant"}`, "not both"},
		{`{"split":"dev","path":"a.go","expect":"compliant"}`, `missing "diff" or "content"`},
		{`{"split":"dev","path":"a.go","content":"  ","expect":"compliant"}`, `empty "content"`},
		{`{"split":"dev","path":"a.go","content":"x","expect":"compliant","extra":1}`, "unknown field"},
		{`{"split":"dev","path":"a.go","diff":"@@ -1 +1 @@\n-a\n","expect":"compliant"}`, "diff:"},
		{`{"split":"dev","path":"a.go","diff":"--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-a\n+b\n","expect":"compliant"}`, `is for "b.go"`},
		{`{"split":"dev","path":"a.go","diff":"--- a/a.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-a\n","expect":"compliant"}`, "deletes"},
		{`{"split":"dev","path":"a.go","diff":"not a diff","expect":"compliant"}`, "diff:"},
		{`not json`, "invalid case"},
		{`{"split":"dev","path":"a.go","content":"x","expect":"compliant"} {}`, "more than one"},
	} {
		_, err := Parse("c.jsonl", strings.NewReader("\n"+tc.line+"\n"))
		if err == nil || !strings.Contains(err.Error(), "c.jsonl:2: ") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want line 2 and %q", tc.line, err, tc.want)
		}
	}
}

func TestParseReportsAllErrorsAndDuplicates(t *testing.T) {
	in := `{"id":"x","split":"dev","path":"a.go","content":"x","expect":"compliant"}
{"id":"x","split":"dev","path":"a.go","content":"y","expect":"compliant"}
{"split":"dev","path":"a.go","content":"x","expect":"nope"}
{"split":"dev","path":"a.go","content":"z","expect":"violation"}
`
	cs, err := Parse("c.jsonl", strings.NewReader(in))
	if err == nil || !strings.Contains(err.Error(), `c.jsonl:2: duplicate id "x" (also line 1)`) || !strings.Contains(err.Error(), "c.jsonl:3: ") {
		t.Errorf("err = %v", err)
	}
	if len(cs) != 2 {
		t.Errorf("valid cases = %d, want 2", len(cs))
	}
}

func TestCasesPath(t *testing.T) {
	if got := CasesPath(trait.Trait{Path: "go/no-debug.md"}); got != "go/no-debug.cases.jsonl" {
		t.Errorf("got %q", got)
	}
}
