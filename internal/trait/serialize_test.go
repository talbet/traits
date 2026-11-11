package trait

import (
	"reflect"
	"testing"
)

func TestSerializeRoundTrip(t *testing.T) {
	traits, err := Load("testdata/traits")
	if err != nil {
		t.Fatal(err)
	}
	traits = append(traits, Trait{
		Name: "everything", Subject: "Multi\nline\n", Requirement: "line1\nline2  \n\n",
		Applicability: "  leading: colon # hash", Purpose: "'quoted' \"too\"",
		Context: "body\n---\nmore", AppliesTo: []string{"*.go", "{a,b}/**"}, When: []string{`\bfoo`, `"q"`},
		Fix: "Fix it.", Scope: ScopeFile, Status: StatusCandidate, Thresholds: Thresholds{Review: 0.3},
		Path: "x/everything.md",
	}, Trait{
		// go-yaml's literal block style would drop these trailing spaces.
		Name: "spaces", Subject: "s", Requirement: "r", Purpose: "p", Applicability: "trailing  \n",
		Scope: ScopeHunk, Status: StatusActive, Path: "spaces.md",
	})
	for _, want := range traits {
		got, err := Parse(want.Path, Serialize(want))
		if err != nil {
			t.Fatalf("%s: %v\n%s", want.Name, err, Serialize(want))
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip %s:\ngot  %+v\nwant %+v\n%s", want.Name, got, want, Serialize(want))
		}
	}
}

func TestSerializeOrderAndOmission(t *testing.T) {
	tr := Trait{Name: "a", Subject: "s", Requirement: "r", Purpose: "p", Applicability: "x",
		Fix: "f", Scope: ScopeHunk, Status: StatusActive, Thresholds: Thresholds{Fix: 0.9}}
	want := "---\nname: a\nsubject: s\nrequirement: r\napplicability: x\npurpose: p\nfix: f\nthresholds:\n  fix: 0.9\n---\n"
	if got := string(Serialize(tr)); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
