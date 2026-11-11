package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const fileScopeTrait = `---
name: whole-file
subject: Go files.
requirement: Files must be tidy.
purpose: Testing file-scope reads.
scope: file
---
`

// TestCheckDiffCannotReadOutsideRepo is the CI attack: a pull request whose
// diff names a path outside the checkout, or adds a symlink to a secret, must
// not get that file's content sent to the provider.
func TestCheckDiffCannotReadOutsideRepo(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		http.Error(w, "unexpected", 500)
	}))
	defer srv.Close()
	t.Setenv("TRAITS_SYSTEMONE_URL", srv.URL)

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	os.WriteFile(secret, []byte("TOP-SECRET-TOKEN\n"), 0o600)

	dir := t.TempDir()
	tdir := filepath.Join(dir, ".agents", "traits")
	os.MkdirAll(tdir, 0o755)
	os.WriteFile(filepath.Join(tdir, "whole-file.md"), []byte(fileScopeTrait), 0o644)
	if err := os.Symlink(secret, filepath.Join(dir, "link.go")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	rel, _ := filepath.Rel(dir, secret)
	rel = filepath.ToSlash(rel)

	for _, path := range []string{rel, "link.go"} {
		d := "diff --git a/" + path + " b/" + path + "\n--- a/" + path + "\n+++ b/" + path + "\n@@ -1 +1 @@\n-x\n+TOP-SECRET-TOKEN\n"
		code, _, stderr := runCLI(t, d, "check", "--diff", "--provider", "systemone", "--model", "m", "--repo", dir, "--no-cache", "--retries", "0")
		if code != exitFindings || !strings.Contains(stderr, "file-scope trait(s) skipped") {
			t.Errorf("%s: exit %d stderr %q", path, code, stderr)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 0 {
		t.Errorf("provider was called %d time(s); first body: %.200s", len(bodies), bodies[0])
	}
}

func TestWalkSkipsSymlinks(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.go"), []byte("package secret\n"), 0o600)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	if err := os.Symlink(filepath.Join(outside, "secret.go"), filepath.Join(dir, "b.go")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	files, _, errs, err := readInput(checkFlags{}, []string{dir}, nil, dir)
	if err != nil || len(errs) != 0 {
		t.Fatal(err, errs)
	}
	if len(files) != 1 || files[0].Path() != "a.go" {
		t.Errorf("files = %+v", files)
	}
}
