package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// TestReleaseFilesParse checks that the GitHub Action, workflows and
// goreleaser config are valid YAML with the expected top-level shape.
func TestReleaseFilesParse(t *testing.T) {
	root := filepath.Join("..", "..")
	load := func(rel string) map[string]any {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := yaml.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		return m
	}

	action := load("action.yml")
	inputs, _ := action["inputs"].(map[string]any)
	for _, in := range []string{"version", "provider", "model", "fail-on", "format", "base-ref", "upload-sarif"} {
		if _, ok := inputs[in]; !ok {
			t.Errorf("action.yml lacks input %q", in)
		}
	}
	if f, _ := inputs["format"].(map[string]any); f["default"] != "sarif" {
		t.Errorf("format default = %v", f["default"])
	}
	runs, _ := action["runs"].(map[string]any)
	steps, _ := runs["steps"].([]any)
	if runs["using"] != "composite" || len(steps) == 0 {
		t.Fatalf("runs = %v", runs)
	}
	var uploads, diffs bool
	for _, s := range steps {
		step, _ := s.(map[string]any)
		if step["uses"] == nil && step["shell"] == nil {
			t.Errorf("composite step without shell: %v", step["name"])
		}
		if u, _ := step["uses"].(string); strings.HasPrefix(u, "github/codeql-action/upload-sarif@") {
			uploads = true
		}
		if r, _ := step["run"].(string); strings.Contains(r, `git diff --no-color "$base...HEAD" | traits check --diff`) {
			diffs = true
		}
	}
	if !uploads || !diffs {
		t.Errorf("action steps: upload-sarif %v, diff|check %v", uploads, diffs)
	}

	for _, wf := range []string{".github/workflows/ci.yml", ".github/workflows/release.yml"} {
		m := load(wf)
		// YAML 1.1 parsers may read the "on" key as a boolean; accept either.
		_, on := m["on"]
		_, onTrue := m["true"]
		jobs, _ := m["jobs"].(map[string]any)
		if (!on && !onTrue) || len(jobs) == 0 {
			t.Errorf("%s: on %v jobs %v", wf, on || onTrue, jobs)
		}
	}

	gr := load(".goreleaser.yaml")
	builds, _ := gr["builds"].([]any)
	b, _ := builds[0].(map[string]any)
	ldflags, _ := b["ldflags"].([]any)
	if len(ldflags) == 0 || !strings.Contains(ldflags[0].(string), "-X main.version={{.Version}}") {
		t.Errorf("ldflags = %v", ldflags)
	}
	if gr["checksum"] == nil {
		t.Error("no checksum section")
	}
}
