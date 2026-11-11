package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/talbet/traits/internal/trait"
)

// evalServer starts a fake System One server whose answers depend on markers
// in the input: BAD -> violation 0.9, MAYBE -> violation 0.6 (0.9 for the
// model "strict"), UNSURE -> inconclusive 0.7, otherwise compliant.
func evalServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		var req struct {
			Model     string                     `json:"model"`
			State     json.RawMessage            `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		state := string(req.State)
		var p map[string]float64
		switch {
		case strings.Contains(state, "BAD"):
			p = map[string]float64{"violation": 0.9, "compliant": 0.1}
		case strings.Contains(state, "MAYBE") && req.Model == "strict":
			p = map[string]float64{"violation": 0.9, "compliant": 0.1}
		case strings.Contains(state, "MAYBE"):
			p = map[string]float64{"violation": 0.6, "compliant": 0.4}
		case strings.Contains(state, "UNSURE"):
			p = map[string]float64{"inconclusive": 0.7, "violation": 0.2, "compliant": 0.1}
		default:
			p = map[string]float64{"compliant": 0.9, "violation": 0.05, "not-applicable": 0.05}
		}
		answers := map[string]any{}
		for id := range req.Questions {
			best, bp := "", -1.0
			for k, v := range p {
				if v > bp {
					best, bp = k, v
				}
			}
			answers[id] = map[string]any{"type": "choice", "choice": best, "confidence": bp, "probabilities": p}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "answers": answers, "usage": map[string]int{"input_tokens": 10}})
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

const debugTrait = `---
name: no-debug-prints
subject: Debugging output left in code.
requirement: Do not leave fmt.Println debugging statements in committed code.
purpose: Debug output pollutes logs.
appliesTo: "*.go"
when: ["Print"]
status: candidate
thresholds: {fix: 0.8, review: 0.5}
---
Context body.
`

const todoTrait = `---
name: no-todo
subject: TODO comments.
requirement: Do not commit TODO comments without an issue link.
purpose: TODOs rot.
status: candidate
---
`

func addLine(line string) string {
	return "@@ -1,2 +1,3 @@\n package main\n \n+" + line + "\n"
}

type tcase map[string]any

func writeCases(t *testing.T, path string, cases ...tcase) {
	t.Helper()
	var b strings.Builder
	for _, c := range cases {
		j, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// evalSetup writes the two candidate traits and their cases and returns the
// traits directory.
func evalSetup(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "traits")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "no-debug-prints.md"), []byte(debugTrait), 0o644)
	os.WriteFile(filepath.Join(dir, "no-todo.md"), []byte(todoTrait), 0o644)
	writeCases(t, filepath.Join(dir, "no-debug-prints.cases.jsonl"),
		tcase{"id": "tp", "split": "dev", "path": "a.go", "diff": addLine(`fmt.Println("BAD")`), "expect": "violation"},
		tcase{"id": "review-only", "split": "dev", "path": "a.go", "diff": addLine(`fmt.Println("MAYBE")`), "expect": "violation"},
		tcase{"id": "fp", "split": "dev", "path": "a.go", "diff": addLine(`fmt.Println("BAD") // allowed`), "expect": "compliant"},
		tcase{"id": "tn", "split": "dev", "path": "a.go", "diff": addLine(`fmt.Println("fine")`), "expect": "compliant"},
		tcase{"id": "gated-path", "split": "dev", "path": "a.ts", "diff": addLine(`console.log("BAD")`), "expect": "not-applicable"},
		tcase{"id": "gated-positive", "split": "dev", "path": "a.go", "diff": addLine(`log.Debug("BAD")`), "expect": "violation"},
		tcase{"id": "unsure", "split": "dev", "path": "b.go", "content": "package main\n\nfunc f() { fmt.Printf(\"UNSURE\") }\n", "expect": "compliant"},
		tcase{"id": "h-tp", "split": "holdout", "path": "c.go", "diff": addLine(`fmt.Println("BAD")`), "expect": "violation"},
		tcase{"id": "h-tn", "split": "holdout", "path": "c.go", "diff": addLine(`fmt.Println("ok")`), "expect": "compliant"},
	)
	writeCases(t, filepath.Join(dir, "no-todo.cases.jsonl"),
		tcase{"split": "dev", "path": "a.go", "diff": addLine(`// TODO BAD`), "expect": "violation"},
		tcase{"split": "holdout", "path": "a.go", "diff": addLine(`// TODO BAD`), "expect": "violation"},
		tcase{"split": "holdout", "path": "a.go", "diff": addLine(`// TODO(#12) BAD`), "expect": "compliant"},
	)
	return dir
}

type evalJSON struct {
	Split string
	Cases int
	Runs  []struct {
		Provider, Model string
		Traits          []traitJSON
		Overall         traitJSON
		Usage           struct{ Requests, InputTokens int }
	}
	Errors     []caseError
	Promotions []promotion
	Failed     []string
}

type ratesJSON struct {
	Threshold                             float64
	TP, FP, FN, TN                        int
	Precision, Recall, F1, FalseAlarmRate *float64
}

type traitJSON struct {
	Trait, Status                                                            string
	Cases, Positives, Negatives, Gated, GatedPositives, Errors, Inconclusive int
	InconclusiveRate                                                         *float64
	Review, Fix                                                              ratesJSON
}

func decodeEval(t *testing.T, out string) evalJSON {
	t.Helper()
	var r evalJSON
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return r
}

func near(p *float64, want float64) bool {
	return p != nil && *p > want-1e-9 && *p < want+1e-9
}

func TestEvalMetrics(t *testing.T) {
	srv, requests := evalServer(t)
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)
	dir := evalSetup(t)

	code, out, stderr := runCLI(t, "", "eval", "--rules", dir, "--provider", "systemone", "--model", "fake",
		"--trait", "no-debug-prints", "--format", "json", "--retries", "0")
	if code != exitOK {
		t.Fatalf("exit %d stderr %s", code, stderr)
	}
	r := decodeEval(t, out)
	if r.Split != "dev" || r.Cases != 7 || len(r.Runs) != 1 || len(r.Runs[0].Traits) != 1 {
		t.Fatalf("report = %s", out)
	}
	tr := r.Runs[0].Traits[0]
	if tr.Trait != "no-debug-prints" || tr.Status != "candidate" || tr.Cases != 7 || tr.Positives != 3 || tr.Negatives != 4 ||
		tr.Gated != 2 || tr.GatedPositives != 1 || tr.Errors != 0 || tr.Inconclusive != 1 || !near(tr.InconclusiveRate, 0.2) {
		t.Errorf("counts = %+v", tr)
	}
	fix := tr.Fix
	if fix.Threshold != 0.8 || fix.TP != 1 || fix.FP != 1 || fix.FN != 2 || fix.TN != 3 ||
		!near(fix.Precision, 0.5) || !near(fix.Recall, 1.0/3) || !near(fix.F1, 0.4) || !near(fix.FalseAlarmRate, 0.25) {
		t.Errorf("fix = %+v", fix)
	}
	rev := tr.Review
	if rev.Threshold != 0.5 || rev.TP != 2 || rev.FP != 1 || rev.FN != 1 || rev.TN != 3 || !near(rev.F1, 4.0/6) {
		t.Errorf("review = %+v", rev)
	}
	// Five cases were asked (two gated), one request each.
	if u := r.Runs[0].Usage; u.Requests != 5 || u.InputTokens != 50 || requests.Load() != 5 {
		t.Errorf("usage %+v, server saw %d", u, requests.Load())
	}

	// A rerun is answered from the cache, in text form.
	code, out, stderr = runCLI(t, "", "eval", "--rules", dir, "--provider", "systemone", "--model", "fake", "--trait", "no-debug-prints")
	if code != exitOK || requests.Load() != 5 {
		t.Fatalf("rerun: exit %d, %d requests, stderr %s", code, requests.Load(), stderr)
	}
	for _, want := range []string{"systemone:fake  split dev  0 request(s)", "no-debug-prints (candidate)", "2 (1 pos!)", "0.40", "PREC"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}
}

func TestEvalAllTraitsOverallAndFailUnder(t *testing.T) {
	srv, _ := evalServer(t)
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)
	dir := evalSetup(t)
	args := []string{"eval", "--rules", dir, "--provider", "systemone", "--model", "fake", "--format", "json"}

	code, out, _ := runCLI(t, "", args...)
	r := decodeEval(t, out)
	if code != exitOK || len(r.Runs[0].Traits) != 2 {
		t.Fatalf("exit %d out %s", code, out)
	}
	// no-debug-prints fix: TP1 FP1 FN2 TN3; no-todo dev: TP1.
	o := r.Runs[0].Overall
	if o.Trait != "overall" || o.Fix.TP != 2 || o.Fix.FP != 1 || o.Fix.FN != 2 || o.Fix.TN != 3 || o.Cases != 8 || !near(o.Fix.F1, 4.0/7) {
		t.Errorf("overall = %+v", o)
	}

	code, out, stderr := runCLI(t, "", append(args, "--fail-under-f1", "0.5")...)
	if r := decodeEval(t, out); code != exitFindings || len(r.Failed) != 1 || r.Failed[0] != "systemone:fake/no-debug-prints" {
		t.Errorf("fail-under 0.5: exit %d failed %v", code, r.Failed)
	}
	if !strings.Contains(stderr, "below 0.50: systemone:fake/no-debug-prints") {
		t.Errorf("stderr = %q", stderr)
	}
	if code, _, _ := runCLI(t, "", append(args, "--fail-under-f1", "0.3")...); code != exitOK {
		t.Errorf("fail-under 0.3: exit %d", code)
	}
}

func TestEvalCompare(t *testing.T) {
	srv, _ := evalServer(t)
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)
	dir := evalSetup(t)
	base := []string{"eval", "--rules", dir, "--trait", "no-debug-prints", "--compare", "systemone:fake,systemone:strict"}

	code, out, stderr := runCLI(t, "", append(base, "--format", "json")...)
	if code != exitOK {
		t.Fatalf("exit %d stderr %s", code, stderr)
	}
	r := decodeEval(t, out)
	if len(r.Runs) != 2 || r.Runs[0].Model != "fake" || r.Runs[1].Model != "strict" {
		t.Fatalf("runs = %s", out)
	}
	if f := r.Runs[0].Traits[0].Fix; !near(f.F1, 0.4) {
		t.Errorf("fake fix = %+v", f)
	}
	// strict scores MAYBE at 0.9, so it is a TP at the fix threshold.
	if f := r.Runs[1].Traits[0].Fix; f.TP != 2 || f.FN != 1 || !near(f.F1, 4.0/6) {
		t.Errorf("strict fix = %+v", f)
	}

	code, out, _ = runCLI(t, "", base...)
	lines := strings.Split(out, "\n")
	if code != exitOK || !strings.Contains(out, "systemone:fake") || !strings.Contains(out, "systemone:strict") {
		t.Fatalf("text = %s", out)
	}
	var row string
	for _, l := range lines {
		if strings.HasPrefix(l, "no-debug-prints") {
			row = l
		}
	}
	if !strings.Contains(row, "0.40 0.50/0.33 0.25") || !strings.Contains(row, "0.67 0.67/0.67 0.25") {
		t.Errorf("compare row = %q\n%s", row, out)
	}
}

func TestEvalPromote(t *testing.T) {
	srv, _ := evalServer(t)
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)
	dir := evalSetup(t)
	md := filepath.Join(dir, "no-debug-prints.md")
	args := []string{"eval", "--rules", dir, "--provider", "systemone", "--model", "fake", "--promote"}

	code, out, stderr := runCLI(t, "", args...)
	if code != exitOK {
		t.Fatalf("exit %d stderr %s", code, stderr)
	}
	if !strings.Contains(out, "would promote no-debug-prints to active (holdout F1 1.00 at fix 0.80 meets 0.90)") ||
		!strings.Contains(out, "no-todo stays candidate: holdout F1 0.67 at fix 0.80 is below 0.90") {
		t.Errorf("dry run output:\n%s", out)
	}
	if data, _ := os.ReadFile(md); string(data) != debugTrait {
		t.Errorf("dry run changed the trait:\n%s", data)
	}

	code, out, stderr = runCLI(t, "", append(args, "--yes", "--format", "json")...)
	if code != exitOK {
		t.Fatalf("exit %d stderr %s", code, stderr)
	}
	r := decodeEval(t, out)
	if r.Split != "holdout" || len(r.Promotions) != 2 || !r.Promotions[0].Written || r.Promotions[1].Promote {
		t.Errorf("promotions = %+v", r.Promotions)
	}
	ts, err := trait.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range ts {
		switch tr.Name {
		case "no-debug-prints":
			if tr.Status != trait.StatusActive || tr.Thresholds != (trait.Thresholds{Fix: 0.8, Review: 0.5}) || tr.Context != "Context body." {
				t.Errorf("promoted trait = %+v", tr)
			}
		case "no-todo":
			if tr.Status != trait.StatusCandidate {
				t.Errorf("no-todo status = %s", tr.Status)
			}
		}
	}

	// Already active: nothing left to promote but no-todo.
	_, out, _ = runCLI(t, "", args...)
	if strings.Contains(out, "promote no-debug-prints") {
		t.Errorf("active trait considered again:\n%s", out)
	}
}

func TestEvalPromoteNeedsBothLabels(t *testing.T) {
	srv, _ := evalServer(t)
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)
	dir := evalSetup(t)
	writeCases(t, filepath.Join(dir, "no-todo.cases.jsonl"),
		tcase{"split": "holdout", "path": "a.go", "diff": addLine(`// TODO BAD`), "expect": "violation"})
	_, out, _ := runCLI(t, "", "eval", "--rules", dir, "--provider", "systemone", "--model", "fake", "--promote", "--yes")
	if !strings.Contains(out, "no-todo stays candidate: needs at least 1 positive and 1 negative holdout case (has 1 and 0)") {
		t.Errorf("out:\n%s", out)
	}
}

func TestEvalInvalidCases(t *testing.T) {
	dir := evalSetup(t)
	os.WriteFile(filepath.Join(dir, "no-todo.cases.jsonl"), []byte(
		`{"split":"dev","path":"a.go","content":"x","expect":"maybe"}`+"\n"+
			`{"split":"dev","path":"a.go","expect":"violation"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "orphan.cases.jsonl"), []byte(""), 0o644)
	code, _, stderr := runCLI(t, "", "eval", "--rules", dir, "--provider", "systemone", "--model", "fake")
	if code != exitUsage || !strings.Contains(stderr, `no-todo.cases.jsonl:1: unknown expect "maybe"`) ||
		!strings.Contains(stderr, `no-todo.cases.jsonl:2: missing "diff" or "content"`) {
		t.Errorf("exit %d stderr %s", code, stderr)
	}

	os.Remove(filepath.Join(dir, "no-todo.cases.jsonl"))
	srv, _ := evalServer(t)
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)
	code, _, stderr = runCLI(t, "", "eval", "--rules", dir, "--provider", "systemone", "--model", "fake")
	if code != exitOK || !strings.Contains(stderr, "orphan.cases.jsonl has no matching trait") || !strings.Contains(stderr, "no cases file for 1 trait(s): no-todo") {
		t.Errorf("exit %d stderr %s", code, stderr)
	}
}

func TestEvalProviderDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)
	dir := evalSetup(t)
	code, _, stderr := runCLI(t, "", "eval", "--rules", dir, "--provider", "systemone", "--model", "fake", "--retries", "0", "--no-cache")
	if code != exitProvider || !strings.Contains(stderr, "500") {
		t.Errorf("exit %d stderr %q", code, stderr)
	}
}

func TestEvalUsageErrors(t *testing.T) {
	dir := evalSetup(t)
	for _, args := range [][]string{
		{"--yes"},
		{"--split", "test"},
		{"--format", "sarif"},
		{"--promote", "--split", "dev"},
		{"--promote", "--compare", "systemone:a"},
		{"--compare", "systemone:a", "--provider", "systemone"},
		{"--compare", ","},
		{"--fail-under-f1", "2"},
		{"--trait", "nope"},
		{"--provider", "nope"},
		{"stray"},
	} {
		code, _, stderr := runCLI(t, "", append([]string{"eval", "--rules", dir}, args...)...)
		if code != exitUsage {
			t.Errorf("%v: exit %d, want %d (%s)", args, code, exitUsage, stderr)
		}
	}
	if code, _, _ := runCLI(t, "", "eval", "--rules", filepath.Join(dir, "missing")); code != exitUsage {
		t.Errorf("missing dir: exit %d", code)
	}
}
