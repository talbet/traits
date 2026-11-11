package output

import (
	"encoding/json"
	"io"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/talbet/traits/internal/check"
)

// SARIF 2.1.0 structures, limited to the fields this package writes.
type (
	sarifLog struct {
		Schema  string     `json:"$schema"`
		Version string     `json:"version"`
		Runs    []sarifRun `json:"runs"`
	}
	sarifRun struct {
		Tool    sarifTool     `json:"tool"`
		Results []sarifResult `json:"results"`
	}
	sarifTool struct {
		Driver sarifDriver `json:"driver"`
	}
	sarifDriver struct {
		Name    string      `json:"name"`
		Version string      `json:"version,omitempty"`
		Rules   []sarifRule `json:"rules"`
	}
	sarifRule struct {
		ID               string        `json:"id"`
		ShortDescription sarifMessage  `json:"shortDescription"`
		Help             *sarifMessage `json:"help,omitempty"`
	}
	sarifMessage struct {
		Text string `json:"text"`
	}
	sarifResult struct {
		RuleID     string          `json:"ruleId"`
		RuleIndex  int             `json:"ruleIndex"`
		Level      string          `json:"level"`
		Message    sarifMessage    `json:"message"`
		Locations  []sarifLocation `json:"locations"`
		Properties sarifProperties `json:"properties"`
	}
	sarifLocation struct {
		PhysicalLocation sarifPhysical `json:"physicalLocation"`
	}
	sarifPhysical struct {
		ArtifactLocation sarifArtifact `json:"artifactLocation"`
		Region           *sarifRegion  `json:"region,omitempty"`
	}
	sarifArtifact struct {
		URI string `json:"uri"`
	}
	sarifRegion struct {
		StartLine int `json:"startLine"`
		EndLine   int `json:"endLine,omitempty"`
	}
	sarifProperties struct {
		Classification check.Classification             `json:"classification"`
		Confidence     float64                          `json:"confidence"`
		Probabilities  map[check.Classification]float64 `json:"probabilities"`
	}
)

// SARIF writes r as a SARIF 2.1.0 log with one rule per trait in r.Traits and
// one result per finding: level "error" for fix, "warning" for review. A
// result has a region only when its finding has a location. File errors are
// not included.
func SARIF(w io.Writer, r check.Report, toolVersion string) error {
	names := slices.Sorted(maps.Keys(r.Traits))
	index := make(map[string]int, len(names))
	rules := make([]sarifRule, len(names))
	for i, n := range names {
		info := r.Traits[n]
		index[n] = i
		desc := oneLine(info.Requirement)
		if desc == "" {
			desc = n
		}
		rules[i] = sarifRule{ID: n, ShortDescription: sarifMessage{Text: desc}}
		var help []string
		if info.Purpose != "" {
			help = append(help, "Why: "+info.Purpose)
		}
		if info.Fix != "" {
			help = append(help, "Fix: "+info.Fix)
		}
		if len(help) > 0 {
			rules[i].Help = &sarifMessage{Text: strings.Join(help, "\n")}
		}
	}

	results := []sarifResult{}
	for _, f := range r.Files {
		for _, fd := range f.Findings {
			idx, ok := index[fd.Trait]
			if !ok {
				// A finding whose trait is missing from r.Traits still gets
				// a rule, so ruleIndex stays valid.
				idx = len(rules)
				index[fd.Trait] = idx
				rules = append(rules, sarifRule{ID: fd.Trait, ShortDescription: sarifMessage{Text: fd.Trait}})
			}
			level := "warning"
			if fd.Tier == check.TierFix {
				level = "error"
			}
			loc := sarifLocation{PhysicalLocation: sarifPhysical{
				ArtifactLocation: sarifArtifact{URI: (&url.URL{Path: f.Path}).String()},
			}}
			if l := fd.Location; l != nil && l.StartLine > 0 {
				reg := &sarifRegion{StartLine: l.StartLine}
				if l.EndLine >= l.StartLine {
					reg.EndLine = l.EndLine
				}
				loc.PhysicalLocation.Region = reg
			}
			probs := fd.Probabilities
			if probs == nil {
				probs = map[check.Classification]float64{}
			}
			results = append(results, sarifResult{
				RuleID:     fd.Trait,
				RuleIndex:  idx,
				Level:      level,
				Message:    sarifMessage{Text: fd.Interpretation},
				Locations:  []sarifLocation{loc},
				Properties: sarifProperties{Classification: fd.Classification, Confidence: fd.Confidence, Probabilities: probs},
			})
		}
	}

	log := sarifLog{
		Schema:  "https://json.schemastore.org/sarif-2.1.0.json",
		Version: "2.1.0",
		Runs: []sarifRun{{
			Tool:    sarifTool{Driver: sarifDriver{Name: "traits", Version: toolVersion, Rules: rules}},
			Results: results,
		}},
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(log)
}
