package output

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talbet/traits/internal/check"
)

var update = flag.Bool("update", false, "rewrite golden files")

func fixture() check.Report {
	return check.Report{
		Version:  1,
		Summary:  "2 findings to fix, 1 to double-check in 2 files (3 traits, 3 asked).",
		Provider: "openai",
		Model:    "gpt-6-luna",
		Traits: map[string]check.TraitInfo{
			"colocate-small-domain-errors": {
				Requirement: "Define small domain error types in the module that raises them.",
				Purpose:     "Keeps errors discoverable\nnext to the code that raises them.",
				Fix:         "Move the error next to its only caller.",
				Path:        "colocate-small-domain-errors.md",
			},
			"no-default-export": {
				Requirement: "Use named exports; do not use `export default`.",
				Purpose:     "Named exports keep imports greppable & renames safe.",
			},
			"explain-magic-numbers": {
				Requirement: "Name or comment numeric literals other than 0 and 1.",
				Purpose:     "Readers should not have to guess what <42> means.",
				Fix:         "Introduce a named constant.",
			},
		},
		Files: []check.FileResult{
			{
				Path: "internal/parse/parse.go",
				Findings: []check.Finding{
					{
						Trait: "colocate-small-domain-errors", Tier: check.TierFix, Classification: check.Violation,
						Probabilities: map[check.Classification]float64{check.Violation: 0.91, check.Compliant: 0.06, check.NotApplicable: 0.02, check.Inconclusive: 0.01},
						Confidence:    0.91,
						Location:      &check.Location{Hunk: "h2", StartLine: 40, EndLine: 58, Probability: 0.84},
						Interpretation: "This change likely violates colocate-small-domain-errors (0.91) in lines 40-58: Define small domain error types in the module that raises them.\n" +
							"Why it matters: Keeps errors discoverable next to the code that raises them.",
					},
					{
						Trait: "explain-magic-numbers", Tier: check.TierReview, Classification: check.Inconclusive,
						Probabilities:  map[check.Classification]float64{check.Inconclusive: 0.55, check.Violation: 0.3, check.Compliant: 0.15},
						Confidence:     0.55,
						Location:       &check.Location{Hunk: "h1", StartLine: 12, EndLine: 12, Probability: 1},
						Interpretation: "Could not tell whether explain-magic-numbers is met (0.55) at line 12.",
						Cached:         true,
					},
				},
			},
			{
				Path: "src/app view.tsx",
				Findings: []check.Finding{
					{
						Trait: "no-default-export", Tier: check.TierFix, Classification: check.Violation,
						Probabilities:  map[check.Classification]float64{check.Violation: 0.88, check.Compliant: 0.12},
						Confidence:     0.88,
						Interpretation: "This file likely violates no-default-export (0.88).",
					},
				},
			},
		},
		Skipped: check.Skipped{Gated: 5, NotApplicable: 4, Compliant: 2, Cached: 1},
		Errors:  []check.FileError{{Path: "vendor/huge.js", Error: "input too large:\n120000 bytes"}},
		Usage:   check.Usage{Requests: 3, InputTokens: 5123, CachedTokens: 4096},
	}
}

func empty() check.Report {
	return check.Report{
		Version: 1, Summary: "No findings.", Provider: "openai", Model: "gpt-6-luna",
		Skipped: check.Skipped{Gated: 3, NotApplicable: 1, Compliant: 2},
	}
}

var formats = map[string]func(io.Writer, check.Report) error{
	"json":    JSON,
	"text":    Text,
	"summary": Summary,
	"sarif":   func(w io.Writer, r check.Report) error { return SARIF(w, r, "0.1.0") },
}

func TestGolden(t *testing.T) {
	ext := map[string]string{"json": "json", "text": "txt", "summary": "md", "sarif": "sarif"}
	for format, render := range formats {
		for name, r := range map[string]check.Report{"report": fixture(), "empty": empty()} {
			var b bytes.Buffer
			if err := render(&b, r); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", name+"."+ext[format]+".golden")
			if *update {
				if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b.Bytes(), want) {
				t.Errorf("%s %s differs (go test -update):\n--- got\n%s--- want\n%s", format, name, b.Bytes(), want)
			}
		}
	}
}

func TestJSONEmptySlices(t *testing.T) {
	var b bytes.Buffer
	r := empty()
	r.Files = []check.FileResult{{Path: "a.go"}}
	if err := JSON(&b, r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "null") {
		t.Errorf("JSON contains null:\n%s", b.String())
	}
	if r.Files[0].Findings != nil {
		t.Error("JSON modified its argument")
	}
}

func TestSARIFValidJSON(t *testing.T) {
	var b bytes.Buffer
	if err := SARIF(&b, fixture(), "0.1.0"); err != nil {
		t.Fatal(err)
	}
	var log struct {
		Runs []struct {
			Results []struct {
				Locations []struct {
					PhysicalLocation struct {
						Region *struct{ StartLine int } `json:"region"`
					} `json:"physicalLocation"`
				} `json:"locations"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(b.Bytes(), &log); err != nil {
		t.Fatal(err)
	}
	res := log.Runs[0].Results
	if len(res) != 3 {
		t.Fatalf("got %d results", len(res))
	}
	if res[2].Locations[0].PhysicalLocation.Region != nil {
		t.Error("finding without location has a region")
	}
}
