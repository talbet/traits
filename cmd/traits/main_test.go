package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const badTrait = `---
name: no-debug-prints
subject: Debugging output left in code.
requirement: Do not leave fmt.Println debugging statements in committed code.
purpose: Debug output pollutes logs.
appliesTo: "*.go"
when: ["Println"]
fix: Remove the print or use the logger.
---
`

const otherTrait = `---
name: ts-only
subject: TypeScript modules.
requirement: Something about TypeScript.
purpose: Testing gating.
appliesTo: "*.ts"
---
`

const sampleDiff = `diff --git a/main.go b/main.go
index 1111111..2222222 100644
--- a/main.go
+++ b/main.go
@@ -1,3 +1,4 @@
 package main

 func main() {
+	fmt.Println("here")
`

// systemOne starts a fake System One server that answers "violation" for
// questions whose instructions mention no-debug-prints, and counts requests.
func systemOne(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		var req struct {
			Questions map[string]struct {
				Type         string `json:"type"`
				Instructions string `json:"instructions"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			if strings.Contains(q.Instructions, "no-debug-prints") {
				answers[id] = map[string]any{"type": "choice", "choice": "violation", "confidence": 0.93,
					"probabilities": map[string]float64{"violation": 0.93, "compliant": 0.05, "not-applicable": 0.01, "inconclusive": 0.01}}
			} else {
				answers[id] = map[string]any{"type": "choice", "choice": "not-applicable", "confidence": 0.9,
					"probabilities": map[string]float64{"violation": 0.02, "compliant": 0.08, "not-applicable": 0.9, "inconclusive": 0}}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "answers": answers})
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func setup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tdir := filepath.Join(dir, ".agents", "traits")
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(tdir, "no-debug-prints.md"), []byte(badTrait), 0o644)
	os.WriteFile(filepath.Join(tdir, "ts-only.md"), []byte(otherTrait), 0o644)
	return dir
}

func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestCheckDiffEndToEnd(t *testing.T) {
	srv, requests := systemOne(t)
	dir := setup(t)
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)

	code, out, stderr := runCLI(t, sampleDiff, "check", "--diff", "--provider", "systemone", "--model", "fake",
		"--repo", dir, "--retries", "0")
	if code != exitFindings {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var rep struct {
		Files []struct {
			Path     string
			Findings []struct {
				Trait    string
				Tier     string
				Location struct{ StartLine int }
			}
		}
		Skipped struct{ Gated int }
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(rep.Files) != 1 || rep.Files[0].Path != "main.go" || len(rep.Files[0].Findings) != 1 {
		t.Fatalf("report = %s", out)
	}
	f := rep.Files[0].Findings[0]
	if f.Trait != "no-debug-prints" || f.Tier != "fix" || f.Location.StartLine != 4 {
		t.Errorf("finding = %+v", f)
	}
	if rep.Skipped.Gated != 1 {
		t.Errorf("gated = %d, want 1 (ts-only)", rep.Skipped.Gated)
	}
	if !strings.Contains(stderr, "1 to fix") {
		t.Errorf("stderr = %q", stderr)
	}

	// A second run is served from the cache.
	before := requests.Load()
	code, _, _ = runCLI(t, sampleDiff, "check", "--diff", "--provider", "systemone", "--model", "fake", "--repo", dir)
	if code != exitFindings || requests.Load() != before {
		t.Errorf("second run: exit %d, %d new requests", code, requests.Load()-before)
	}
}

func TestCheckFailOnNever(t *testing.T) {
	srv, _ := systemOne(t)
	dir := setup(t)
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)
	code, out, _ := runCLI(t, sampleDiff, "check", "--diff", "--provider", "systemone", "--model", "fake",
		"--repo", dir, "--fail-on", "never", "--format", "text", "--no-cache")
	if code != exitOK || !strings.HasPrefix(out, "main.go:4: fix[no-debug-prints]") {
		t.Errorf("exit %d out %q", code, out)
	}
}

func TestCheckProviderDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	dir := setup(t)
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)
	code, _, stderr := runCLI(t, sampleDiff, "check", "--diff", "--provider", "systemone", "--model", "fake",
		"--repo", dir, "--retries", "0", "--no-cache")
	if code != exitProvider || !strings.Contains(stderr, "500") {
		t.Errorf("exit %d stderr %q", code, stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"check"},
		{"check", "--diff", "x.go"},
		{"check", "--diff", "--assume-filename", "a"},
		{"check", "--format", "xml", "a"},
		{"check", "--provider", "nope", "--diff"},
		{"bogus"},
	} {
		dir := setup(t)
		code, _, _ := runCLI(t, sampleDiff, append(args, "--repo", dir)...)
		if args[0] == "bogus" {
			code, _, _ = runCLI(t, "", args...)
		}
		if code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

func TestExplain(t *testing.T) {
	dir := setup(t)
	code, out, stderr := runCLI(t, "", "explain", "--rules", filepath.Join(dir, ".agents", "traits"), "no-debug-prints")
	if code != exitOK || !strings.Contains(out, "Do not leave fmt.Println") || strings.Contains(out, "ts-only") {
		t.Errorf("exit %d out %s err %s", code, out, stderr)
	}
}

func TestCheckEmptyDiffIsClean(t *testing.T) {
	dir := setup(t)
	code, out, stderr := runCLI(t, "", "check", "--diff", "--provider", "systemone", "--model", "fake", "--repo", dir)
	if code != exitOK {
		t.Errorf("exit %d out %q stderr %q", code, out, stderr)
	}
}

func TestCheckUnreadablePathContinues(t *testing.T) {
	srv, requests := systemOne(t)
	dir := setup(t)
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)
	good := filepath.Join(dir, "main.go")
	os.WriteFile(good, []byte("package main\n\nfunc main() { fmt.Println(1) }\n"), 0o644)
	code, out, stderr := runCLI(t, "", "check", "--provider", "systemone", "--model", "fake", "--repo", dir,
		"--no-cache", "--fail-on", "never", good, filepath.Join(dir, "missing.go"))
	if code != exitFindings || requests.Load() == 0 {
		t.Fatalf("exit %d requests %d stderr %q", code, requests.Load(), stderr)
	}
	if !strings.Contains(out, `"missing.go"`) || !strings.Contains(out, "no-debug-prints") {
		t.Errorf("out = %s", out)
	}
}

func TestCheckModelFromEnvSeparatesCache(t *testing.T) {
	srv, requests := systemOne(t)
	dir := setup(t)
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)
	t.Setenv("TRAITS_MODEL", "model-a")
	runCLI(t, sampleDiff, "check", "--diff", "--provider", "systemone", "--repo", dir)
	first := requests.Load()
	t.Setenv("TRAITS_MODEL", "model-b")
	runCLI(t, sampleDiff, "check", "--diff", "--provider", "systemone", "--repo", dir)
	if first == 0 || requests.Load() == first {
		t.Errorf("requests: first run %d, total %d; a new model must not reuse cached answers", first, requests.Load())
	}
}
