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
	"testing"
	"time"

	"github.com/talbet/traits/internal/check"
	"github.com/talbet/traits/internal/state"
)

const hookTrait = `---
name: no-debug-prints
subject: Debugging output left in code.
requirement: Do not leave print debugging statements in committed code.
purpose: Debug output pollutes logs.
appliesTo: "*.go"
fix: Remove the print or use the logger.
---
`

// contentServer is a fake System One server that answers "violation" to the
// no-debug-prints question when the checked input contains "Println(", and
// "compliant" otherwise. delay holds each answer back (until the client
// gives up); status, when set, is returned instead of an answer.
func contentServer(t *testing.T, delay time.Duration, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State     string `json:"state"`
			Questions map[string]struct {
				Instructions string `json:"instructions"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// The body is read, so the server now notices a client giving up.
		if delay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}
		if status != 0 {
			http.Error(w, "boom", status)
			return
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			if strings.Contains(q.Instructions, "no-debug-prints") && strings.Contains(req.State, "Println(") {
				answers[id] = map[string]any{"type": "choice", "choice": "violation", "confidence": 0.95,
					"probabilities": map[string]float64{"violation": 0.95, "compliant": 0.03, "not-applicable": 0.01, "inconclusive": 0.01}}
			} else {
				answers[id] = map[string]any{"type": "choice", "choice": "compliant", "confidence": 0.9,
					"probabilities": map[string]float64{"violation": 0.03, "compliant": 0.9, "not-applicable": 0.06, "inconclusive": 0.01}}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "answers": answers})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// hookRepo makes a repository with one trait and points the provider at srv.
func hookRepo(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	tdir := filepath.Join(dir, ".agents", "traits")
	os.MkdirAll(tdir, 0o755)
	os.WriteFile(filepath.Join(tdir, "no-debug-prints.md"), []byte(hookTrait), 0o644)
	t.Setenv("TRAITS_PROVIDER", "systemone")
	t.Setenv("TRAITS_MODEL", "fake")
	t.Setenv("TRAITS_DIR", "")
	t.Setenv("TRAITS_HOOK_TIMEOUT", "")
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")
	if srv != nil {
		t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)
	}
	return dir
}

func runHookCLI(t *testing.T, stdin any, args ...string) (int, string, string) {
	t.Helper()
	raw, _ := json.Marshal(stdin)
	var out, errb bytes.Buffer
	code := run(context.Background(), args, bytes.NewReader(raw), &out, &errb)
	return code, out.String(), errb.String()
}

func writePayload(dir, rel, content, session string) map[string]any {
	abs := filepath.Join(dir, rel)
	os.MkdirAll(filepath.Dir(abs), 0o755)
	os.WriteFile(abs, []byte(content), 0o644)
	return map[string]any{
		"session_id": session, "cwd": dir, "hook_event_name": "PostToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": abs, "content": content},
	}
}

func additionalContext(t *testing.T, out string) string {
	t.Helper()
	var got struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not hook JSON: %v\n%q", err, out)
	}
	if got.HookSpecificOutput.HookEventName != "PostToolUse" {
		t.Errorf("hookEventName = %q", got.HookSpecificOutput.HookEventName)
	}
	return got.HookSpecificOutput.AdditionalContext
}

const debugGo = "package main\n\nfunc main() {\n\tfmt.Println(\"here\")\n}\n"
const cleanGo = "package main\n\nfunc main() {\n\tlog.Info(\"here\")\n}\n"

func TestHookClaudeWriteReportsFindings(t *testing.T) {
	dir := hookRepo(t, contentServer(t, 0, 0))
	code, out, stderr := runHookCLI(t, writePayload(dir, "main.go", debugGo, "s1"), "hook", "claude")
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	ctx := additionalContext(t, out)
	for _, want := range []string{"## Fix", "### main.go", "`no-debug-prints`", "Remove the print"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("additionalContext lacks %q:\n%s", want, ctx)
		}
	}
	recs, _ := state.ReadFindings(state.FindingsPath(filepath.Join(dir, ".agents", "traits")))
	if len(recs) != 1 || recs[0].Source != "hook" || recs[0].Session != "s1" || recs[0].Path != "main.go" || recs[0].Tier != "fix" || recs[0].Provider != "systemone" {
		t.Errorf("log = %+v", recs)
	}

	// A clean write prints nothing.
	code, out, _ = runHookCLI(t, writePayload(dir, "main.go", cleanGo, "s1"), "hook", "claude")
	if code != 0 || out != "" {
		t.Errorf("clean: exit %d out %q", code, out)
	}
}

func TestHookClaudeEditLocation(t *testing.T) {
	dir := hookRepo(t, contentServer(t, 0, 0))
	var b strings.Builder
	b.WriteString("package main\n\nfunc main() {\n")
	for range 20 {
		b.WriteString("\tx++\n")
	}
	b.WriteString("\tfmt.Println(x)\n}\n") // line 24
	abs := filepath.Join(dir, "main.go")
	os.WriteFile(abs, []byte(b.String()), 0o644)
	p := map[string]any{"session_id": "s2", "cwd": dir, "hook_event_name": "PostToolUse", "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": abs, "old_string": "\treturn\n}", "new_string": "\tfmt.Println(x)\n}"}}
	code, out, _ := runHookCLI(t, p, "hook", "claude")
	ctx := additionalContext(t, out)
	if code != 0 || !strings.Contains(ctx, "Lines 21-25") && !strings.Contains(ctx, "Line 24") {
		t.Errorf("exit %d context:\n%s", code, ctx)
	}
}

func TestHookCodexApplyPatch(t *testing.T) {
	dir := hookRepo(t, contentServer(t, 0, 0))
	patch := "*** Begin Patch\n*** Add File: cmd/x.go\n+package main\n+\n+func x() { fmt.Println(1) }\n*** End Patch\n"
	os.MkdirAll(filepath.Join(dir, "cmd"), 0o755)
	os.WriteFile(filepath.Join(dir, "cmd", "x.go"), []byte("package main\n\nfunc x() { fmt.Println(1) }\n"), 0o644)
	p := map[string]any{"session_id": "c1", "cwd": dir, "hook_event_name": "PostToolUse", "tool_name": "apply_patch",
		"tool_input": map[string]any{"command": patch}}
	code, out, _ := runHookCLI(t, p, "hook", "codex")
	if ctx := additionalContext(t, out); code != 0 || !strings.Contains(ctx, "### cmd/x.go") {
		t.Errorf("exit %d context:\n%s", code, ctx)
	}
}

func TestHookFailsOpen(t *testing.T) {
	cases := map[string]func(t *testing.T) (string, any){
		"provider down": func(t *testing.T) (string, any) {
			dir := hookRepo(t, contentServer(t, 0, http.StatusInternalServerError))
			return dir, writePayload(dir, "main.go", debugGo, "s")
		},
		"timeout": func(t *testing.T) (string, any) {
			dir := hookRepo(t, contentServer(t, 10*time.Second, 0))
			t.Setenv("TRAITS_HOOK_TIMEOUT", "300ms")
			return dir, writePayload(dir, "main.go", debugGo, "s")
		},
		"no traits dir": func(t *testing.T) (string, any) {
			dir := hookRepo(t, contentServer(t, 0, 0))
			os.RemoveAll(filepath.Join(dir, ".agents"))
			return dir, writePayload(dir, "main.go", debugGo, "s")
		},
		"no credentials": func(t *testing.T) (string, any) {
			dir := hookRepo(t, nil)
			t.Setenv("TRAITS_PROVIDER", "openai")
			t.Setenv("OPENAI_API_KEY", "")
			return dir, writePayload(dir, "main.go", debugGo, "s")
		},
		"file outside repo": func(t *testing.T) (string, any) {
			dir := hookRepo(t, contentServer(t, 0, 0))
			p := writePayload(t.TempDir(), "main.go", debugGo, "s")
			p["cwd"] = dir
			return dir, p
		},
		"not json": func(t *testing.T) (string, any) {
			return hookRepo(t, contentServer(t, 0, 0)), "{"
		},
		"other event": func(t *testing.T) (string, any) {
			dir := hookRepo(t, contentServer(t, 0, 0))
			p := writePayload(dir, "main.go", debugGo, "s")
			p["hook_event_name"] = "PreToolUse"
			return dir, p
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			_, p := mk(t)
			start := time.Now()
			code, out, stderr := runHookCLI(t, p, "hook", "claude")
			if code != 0 || out != "" || stderr != "" {
				t.Errorf("exit %d out %q stderr %q", code, out, stderr)
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Errorf("took %v", d)
			}
		})
	}
}

func TestRecheckFixedAndKept(t *testing.T) {
	dir := hookRepo(t, contentServer(t, 0, 0))
	tdir := filepath.Join(dir, ".agents", "traits")
	for _, f := range []string{"fixed.go", "kept.go"} {
		if _, out, _ := runHookCLI(t, writePayload(dir, f, debugGo, "s1"), "hook", "claude"); out == "" {
			t.Fatalf("%s: no finding", f)
		}
	}
	// Another session's finding is left alone.
	runHookCLI(t, writePayload(dir, "other.go", debugGo, "s2"), "hook", "claude")
	os.WriteFile(filepath.Join(dir, "fixed.go"), []byte(cleanGo), 0o644)

	stop := map[string]any{"session_id": "s1", "cwd": dir, "hook_event_name": "Stop", "stop_hook_active": false}
	code, out, stderr := runHookCLI(t, stop, "recheck")
	if code != 0 || out != "" || stderr != "" {
		t.Fatalf("exit %d out %q stderr %q", code, out, stderr)
	}
	outcomes := map[string]string{}
	recs, _ := state.ReadFindings(state.FindingsPath(tdir))
	for _, f := range state.Fold(recs) {
		outcomes[f.Path] = f.Outcome
	}
	if outcomes["fixed.go"] != "fixed" || outcomes["kept.go"] != "kept" || outcomes["other.go"] != "" {
		t.Errorf("outcomes = %v", outcomes)
	}

	// Already decided: a second recheck logs nothing new.
	before := len(recs)
	runHookCLI(t, stop, "recheck")
	if recs, _ = state.ReadFindings(state.FindingsPath(tdir)); len(recs) != before {
		t.Errorf("second recheck appended %d records", len(recs)-before)
	}

	// The summary reflects the outcomes.
	code, out, _ = runCLI(t, "", "findings", "--summary", "--repo", dir, "--format", "json")
	var sums []state.TraitSummary
	if err := json.Unmarshal([]byte(out), &sums); err != nil || code != 0 {
		t.Fatalf("exit %d %v %s", code, err, out)
	}
	if len(sums) != 1 || sums[0].Flagged != 3 || sums[0].Fixed != 1 || sums[0].Kept != 1 || sums[0].Unknown != 1 || sums[0].KeepRate != 0.5 {
		t.Errorf("summary = %+v", sums)
	}
	code, out, _ = runCLI(t, "", "findings", "--summary", "--repo", dir)
	if code != 0 || !strings.Contains(out, "no-debug-prints") || !strings.Contains(out, "50%") {
		t.Errorf("text summary:\n%s", out)
	}
	code, out, _ = runCLI(t, "", "findings", "--repo", dir)
	if code != 0 || !strings.Contains(out, "kept.go:1 fix[no-debug-prints] kept (hook)") {
		t.Errorf("findings:\n%s", out)
	}
}

func TestRecheckProviderDownLeavesFindingsOpen(t *testing.T) {
	ok := contentServer(t, 0, 0)
	dir := hookRepo(t, ok)
	runHookCLI(t, writePayload(dir, "main.go", debugGo, "s1"), "hook", "claude")
	t.Setenv("TRAITS_SYSTEMONE_URL", contentServer(t, 0, 500).URL)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(cleanGo), 0o644)
	code, out, _ := runHookCLI(t, map[string]any{"session_id": "s1", "cwd": dir}, "recheck")
	recs, _ := state.ReadFindings(state.FindingsPath(filepath.Join(dir, ".agents", "traits")))
	if code != 0 || out != "" || len(recs) != 1 {
		t.Errorf("exit %d out %q records %+v", code, out, recs)
	}
}

func TestCheckLogsFindings(t *testing.T) {
	dir := hookRepo(t, contentServer(t, 0, 0))
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(debugGo), 0o644)
	diffText := "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,3 +1,4 @@\n package main\n \n func main() {\n+\tfmt.Println(\"here\")\n"
	t.Setenv("GITHUB_ACTIONS", "true")
	code, _, stderr := runCLI(t, diffText, "check", "--diff", "--repo", dir, "--retries", "0")
	if code != exitFindings {
		t.Fatalf("exit %d %s", code, stderr)
	}
	recs, _ := state.ReadFindings(state.FindingsPath(filepath.Join(dir, ".agents", "traits")))
	if len(recs) != 1 || recs[0].Source != "ci" || recs[0].StartLine != 4 || recs[0].ID != state.FindingID("main.go", "no-debug-prints", "fmt.Println(\"here\")\n") {
		t.Errorf("log = %+v", recs)
	}
}

func TestHookContextTopN(t *testing.T) {
	fd := func(trait string, tier check.Tier, p float64) check.Finding {
		return check.Finding{Trait: trait, Tier: tier, Classification: check.Violation, Confidence: p,
			Probabilities: map[check.Classification]float64{check.Violation: p}}
	}
	rep := check.Report{Summary: "x", Files: []check.FileResult{
		{Path: "a.go", Findings: []check.Finding{fd("r1", check.TierReview, 0.6), fd("r2", check.TierReview, 0.7)}},
		{Path: "b.go", Findings: []check.Finding{fd("f1", check.TierFix, 0.85), fd("f2", check.TierFix, 0.99)}},
	}}
	got := hookContext(rep, 3)
	if !strings.Contains(got, "top 3 of 4") || strings.Contains(got, "`r1`") || !strings.Contains(got, "`r2`") || strings.Index(got, "`f2`") > strings.Index(got, "`f1`") {
		t.Errorf("context:\n%s", got)
	}
	if hookContext(check.Report{}, 5) != "" {
		t.Error("empty report gave context")
	}
}

func TestHookInstallCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	code, out, _ := runCLI(t, "", "hook", "install", "claude", "--repo", dir)
	path := filepath.Join(dir, ".claude", "settings.json")
	if code != 0 || !strings.Contains(out, "Dry run") || !strings.Contains(out, `"traits hook claude"`) {
		t.Errorf("dry run: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("dry run wrote the file")
	}
	code, _, _ = runCLI(t, "", "hook", "install", "claude", "--repo", dir, "--apply")
	if _, err := os.Stat(path); code != 0 || err != nil {
		t.Fatalf("apply: exit %d %v", code, err)
	}
	code, out, _ = runCLI(t, "", "hook", "install", "claude", "--repo", dir, "--apply")
	if code != 0 || !strings.Contains(out, "already up to date") {
		t.Errorf("rerun: %s", out)
	}
	code, _, _ = runCLI(t, "", "hook", "install", "codex", "--scope", "user", "--apply")
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".codex", "hooks.json")); code != 0 || err != nil {
		t.Errorf("codex user: exit %d %v", code, err)
	}
	if code, _, _ := runCLI(t, "", "hook", "install", "vim"); code != exitUsage {
		t.Errorf("bad agent: exit %d", code)
	}
}

func TestSkillInstall(t *testing.T) {
	dir := t.TempDir()
	code, out, _ := runCLI(t, "", "skill", "install", "--repo", dir)
	if code != 0 || !strings.Contains(out, "would create") {
		t.Errorf("dry run: %d %s", code, out)
	}
	code, _, _ = runCLI(t, "", "skill", "install", "--repo", dir, "--agent", "codex", "--apply")
	data, err := os.ReadFile(filepath.Join(dir, ".agents", "skills", "traits", "SKILL.md"))
	if code != 0 || err != nil || !strings.HasPrefix(string(data), "---\nname: traits\n") {
		t.Fatalf("apply: %d %v", code, err)
	}
	if n := strings.Count(string(data), "\n"); n >= 120 {
		t.Errorf("SKILL.md has %d lines", n)
	}
	code, out, _ = runCLI(t, "", "skill", "install", "--repo", dir, "--agent", "opencode")
	if code != 0 || !strings.Contains(out, "already up to date") {
		t.Errorf("rerun: %s", out)
	}
}
