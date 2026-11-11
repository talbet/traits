package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const existing = `{
  "model": "opus",
  "permissions": {"allow": ["Bash(go test:*)"], "deny": []},
  "hooks": {
    "PostToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "echo bash", "timeout": 5}]},
      {"matcher": "Write|Edit", "hooks": [
        {"type": "command", "command": "prettier --write"},
        {"type": "command", "command": "/usr/local/bin/traits hook claude --top 3"}
      ]}
    ]
  },
  "zeta": 1.50,
  "env": {"B": "2", "A": "1"}
}
`

func TestInstallMergesIdempotentlyAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(existing), 0o600)
	cmds := Commands("claude", "")
	now := time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC)

	// Dry run writes nothing.
	res, err := Install(path, cmds, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Actions[0] != Updated || res.Actions[1] != Added {
		t.Fatalf("dry run = %+v", res.Actions)
	}
	if data, _ := os.ReadFile(path); string(data) != existing {
		t.Fatal("dry run modified the file")
	}

	res, err = Install(path, cmds, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Backup != path+".bak-20261111-093000" {
		t.Errorf("backup = %q", res.Backup)
	}
	if data, _ := os.ReadFile(res.Backup); string(data) != existing {
		t.Error("backup differs from the original")
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	// Unknown keys kept, in their original order, numbers untouched.
	order := []string{`"model"`, `"permissions"`, `"hooks"`, `"zeta": 1.50`, `"env"`, `"B"`, `"A"`}
	last := -1
	for _, k := range order {
		i := strings.Index(text, k)
		if i < last {
			t.Errorf("%s out of order in\n%s", k, text)
		}
		last = i
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode())
	}

	var got struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct {
				Command string
				Timeout int
			}
		}
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	post := got.Hooks["PostToolUse"]
	if len(post) != 3 {
		t.Fatalf("PostToolUse = %+v", post)
	}
	// The shared group keeps its other handler and matcher; ours gets its own group.
	if post[1].Matcher != "Write|Edit" || len(post[1].Hooks) != 1 || post[1].Hooks[0].Command != "prettier --write" {
		t.Errorf("shared group = %+v", post[1])
	}
	if post[2].Matcher != "Write|Edit|MultiEdit" || post[2].Hooks[0].Command != "traits hook claude" || post[2].Hooks[0].Timeout != 15 {
		t.Errorf("ours = %+v", post[2])
	}
	if stop := got.Hooks["Stop"]; len(stop) != 1 || stop[0].Matcher != "" || stop[0].Hooks[0].Command != "traits recheck" {
		t.Errorf("Stop = %+v", stop)
	}

	// Second run: nothing to do, no new backup.
	res, err = Install(path, cmds, true, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.Backup != "" || res.Actions[0] != Unchanged || res.Actions[1] != Unchanged {
		t.Errorf("rerun = %+v", res)
	}
	if after, _ := os.ReadFile(path); string(after) != text {
		t.Error("rerun changed the file")
	}

	// Pinning a provider updates our handlers in place.
	res, _ = Install(path, Commands("claude", "--provider systemone"), true, now.Add(2*time.Minute))
	data, _ = os.ReadFile(path)
	if res.Actions[0] != Updated || strings.Count(string(data), "traits hook claude") != 1 || !strings.Contains(string(data), "traits recheck --provider systemone") {
		t.Errorf("update = %+v\n%s", res.Actions, data)
	}
}

func TestInstallNewFileAndCodex(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "hooks.json")
	res, err := Install(path, Commands("codex", ""), true, time.Now())
	if err != nil || res.Backup != "" {
		t.Fatalf("%+v %v", res, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"matcher": "Edit|Write|apply_patch"`) || !strings.Contains(string(data), `"command": "traits hook codex"`) {
		t.Errorf("hooks.json =\n%s", data)
	}
}

func TestInstallRefusesBadShapes(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"array.json": `[1]`,
		"hooks.json": `{"hooks": []}`,
		"event.json": `{"hooks": {"Stop": {}}}`,
		"bad.json":   `{"a": `,
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(content), 0o644)
		if _, err := Install(p, Commands("claude", ""), true, time.Now()); err == nil {
			t.Errorf("%s: no error", name)
		}
		if data, _ := os.ReadFile(p); string(data) != content {
			t.Errorf("%s modified", name)
		}
	}
}

func TestOurs(t *testing.T) {
	for cmd, want := range map[string]bool{
		"traits hook claude":                  true,
		"/opt/bin/traits recheck --model x":   true,
		"TRAITS_PROVIDER=x traits hook codex": true,
		"'/a b/traits.exe' hook claude":       false, // quoted path with space: not ours (left alone)
		"traits check --diff":                 false,
		"other hook claude":                   false,
		"":                                    false,
	} {
		if got := ours(cmd); got != want {
			t.Errorf("ours(%q) = %v", cmd, got)
		}
	}
}
