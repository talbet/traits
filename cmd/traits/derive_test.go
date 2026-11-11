package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

// deriveDecisionServer is a fake System One server for derive. answer gets
// the question instructions and returns the choice; it is installed as the
// systemone provider's URL.
func deriveDecisionServer(t *testing.T, answer func(instructions string) string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]struct {
				Instructions string `json:"instructions"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			answers[id] = map[string]any{"type": "choice", "choice": answer(q.Instructions), "confidence": 0.9}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "answers": answers})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)
}

// deriveByQuestion answers decompose with decompose, relate questions about a
// stored trait named in relate with that answer (others distinct), and
// coverage with "complete".
func deriveByQuestion(decompose string, relate map[string]string) func(string) string {
	return func(in string) string {
		switch {
		case strings.Contains(in, "should be separate traits"):
			return decompose
		case strings.HasPrefix(in, "Determine the relationship"):
			for name, v := range relate {
				if strings.Contains(in, `{"name":"`+name+`"`) {
					return v
				}
			}
			return "distinct"
		}
		return "complete"
	}
}

// deriveChatServer is a fake chat-completions server returning content, and
// counting requests; it is installed as the LLM.
func deriveChatServer(t *testing.T, content string) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "stop", "message": map[string]any{"content": content}}}})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TRAITS_LLM_BASE_URL", srv.URL+"/v1")
	t.Setenv("TRAITS_LLM_MODEL", "fake")
	return &n
}

const deriveProposal = `---
name: early-returns
subject: Functions with nested conditionals.
requirement: Return early instead of nesting the happy path.
purpose: Flat code is easier to read.
---
`

// deriveSetup returns a traits dir holding the check tests' traits and a
// proposal file, with no LLM configured.
func deriveSetup(t *testing.T) (dir, proposal string) {
	t.Helper()
	t.Setenv("TRAITS_LLM_MODEL", "")
	t.Setenv("TRAITS_DIR", "")
	dir = filepath.Join(setup(t), ".agents", "traits")
	proposal = filepath.Join(t.TempDir(), "proposal.md")
	os.WriteFile(proposal, []byte(deriveProposal), 0o644)
	return dir, proposal
}

var derivePlanLine = regexp.MustCompile(`(?m)^plan ([0-9a-f]{12})$`)

func derivePlanID(t *testing.T, out string) string {
	t.Helper()
	m := derivePlanLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no plan id in:\n%s", out)
	}
	return m[1]
}

func deriveArgs(args ...string) []string {
	return append([]string{"derive", "--provider", "systemone", "--model", "fake", "--retries", "0"}, args...)
}

func TestDeriveCreateAndApprove(t *testing.T) {
	dir, proposal := deriveSetup(t)
	deriveDecisionServer(t, deriveByQuestion("atomic", nil))

	// Flags after the positional argument are accepted.
	code, out, stderr := runCLI(t, "", deriveArgs(proposal, "--rules", dir)...)
	if code != exitOK || !strings.Contains(out, "created early-returns") || !strings.Contains(out, "--approve") {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	id := derivePlanID(t, out)
	if _, err := os.Stat(filepath.Join(dir, "early-returns.md")); err == nil {
		t.Fatal("plan wrote a file")
	}
	code, out, stderr = runCLI(t, "", "derive", "--rules", dir, "--approve", id)
	if code != exitOK || !strings.Contains(out, "created early-returns") {
		t.Fatalf("approve: exit %d\n%s\n%s", code, out, stderr)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "early-returns.md")); err != nil || string(data) != deriveProposal {
		t.Errorf("written %q err %v", data, err)
	}
	// The saved plan is consumed.
	if code, _, _ := runCLI(t, "", "derive", "--rules", dir, "--approve", id); code != exitUsage {
		t.Errorf("second approve: exit %d", code)
	}
}

func TestDeriveStdinJSON(t *testing.T) {
	dir, _ := deriveSetup(t)
	deriveDecisionServer(t, deriveByQuestion("atomic", map[string]string{"no-debug-prints": "covered"}))
	code, out, stderr := runCLI(t, deriveProposal, deriveArgs("--rules", dir, "--format", "json", "-")...)
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	var res struct {
		PlanID string
		Items  []struct {
			Outcome string
			Targets []string
		}
		Changes []any
		Approve string
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(res.Items) != 1 || res.Items[0].Outcome != "covered" || res.Items[0].Targets[0] != "no-debug-prints" ||
		len(res.Changes) != 0 || !strings.Contains(res.Approve, res.PlanID) {
		t.Errorf("result = %+v", res)
	}
}

func TestDeriveConflictNeedsResolve(t *testing.T) {
	dir, proposal := deriveSetup(t)
	deriveDecisionServer(t, deriveByQuestion("atomic", map[string]string{"ts-only": "conflict"}))
	code, out, _ := runCLI(t, "", deriveArgs("--rules", dir, proposal)...)
	if code != exitFindings || !strings.Contains(out, "needs attention") || !strings.Contains(out, "--resolve ts-only=") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	id := derivePlanID(t, out)
	code, _, stderr := runCLI(t, "", "derive", "--rules", dir, "--approve", id)
	if code != exitFindings || !strings.Contains(stderr, "ts-only") {
		t.Errorf("unresolved approve: exit %d %s", code, stderr)
	}
	if code, _, _ := runCLI(t, "", "derive", "--rules", dir, "--approve", id, "--resolve", "ts-only=maybe"); code != exitUsage {
		t.Errorf("bad resolution: exit %d", code)
	}
	code, out, stderr = runCLI(t, "", "derive", "--rules", dir, "--approve", id, "--resolve", "ts-only=replace")
	if code != exitOK || !strings.Contains(out, "deleted ts-only") || !strings.Contains(out, "created early-returns") {
		t.Fatalf("resolved approve: exit %d\n%s\n%s", code, out, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "ts-only.md")); err == nil {
		t.Error("replaced trait still exists")
	}
}

func TestDeriveStaleStore(t *testing.T) {
	dir, proposal := deriveSetup(t)
	deriveDecisionServer(t, deriveByQuestion("atomic", nil))
	_, out, _ := runCLI(t, "", deriveArgs("--rules", dir, proposal)...)
	id := derivePlanID(t, out)
	os.Remove(filepath.Join(dir, "ts-only.md"))
	code, _, stderr := runCLI(t, "", "derive", "--rules", dir, "--approve", id)
	if code != exitFindings || !strings.Contains(stderr, "traits changed since the plan; re-run derive") {
		t.Errorf("exit %d stderr %q", code, stderr)
	}
}

func TestDeriveNeedsLLM(t *testing.T) {
	dir, proposal := deriveSetup(t)
	deriveDecisionServer(t, deriveByQuestion("decomposable", nil))
	code, _, stderr := runCLI(t, "", deriveArgs("--rules", dir, "--from-feedback", "use early returns")...)
	if code != exitUsage || !strings.Contains(stderr, "TRAITS_LLM_MODEL") {
		t.Errorf("feedback: exit %d %s", code, stderr)
	}
	code, _, stderr = runCLI(t, "", deriveArgs("--rules", dir, proposal)...)
	if code != exitUsage || !strings.Contains(stderr, "split it") {
		t.Errorf("decomposable: exit %d %s", code, stderr)
	}
}

func TestDeriveFromFeedbackApprovesSavedDraft(t *testing.T) {
	dir, _ := deriveSetup(t)
	deriveDecisionServer(t, deriveByQuestion("atomic", nil))
	calls := deriveChatServer(t, `{"name":"early-returns","subject":"Functions.","requirement":"Return early.","applicability":null,"purpose":"Readability.","context":null}`)
	diffFile := filepath.Join(t.TempDir(), "fix.diff")
	os.WriteFile(diffFile, []byte("-if ok {\n+if !ok { return }\n"), 0o644)

	code, out, stderr := runCLI(t, "", deriveArgs("--rules", dir, "--from-feedback", "use early returns", "--diff-file", diffFile)...)
	if code != exitOK || !strings.Contains(out, "```diff") {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	id := derivePlanID(t, out)
	before := calls.Load()
	code, out, stderr = runCLI(t, "", "derive", "--rules", dir, "--from-feedback", "use early returns", "--approve", id)
	if code != exitOK || calls.Load() != before {
		t.Fatalf("approve: exit %d, %d new LLM calls\n%s\n%s", code, calls.Load()-before, out, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "early-returns.md"))
	if err != nil || !strings.Contains(string(data), "+if !ok { return }") {
		t.Errorf("written %q err %v", data, err)
	}
}

func TestDeriveUsageErrors(t *testing.T) {
	dir, proposal := deriveSetup(t)
	for _, args := range [][]string{
		{"derive", "--rules", dir},
		{"derive", "--rules", dir, proposal, "--from-feedback", "x"},
		{"derive", "--rules", dir, "--diff-file", "x", proposal},
		{"derive", "--rules", dir, "--approve", "../../etc"},
		{"derive", "--rules", dir, "--format", "xml", proposal},
		{"derive", "--rules", dir, "a.md", "b.md"},
	} {
		if code, _, _ := runCLI(t, "", args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}
