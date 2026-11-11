package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talbet/traits/internal/compile"
	"github.com/talbet/traits/internal/diff"
	"github.com/talbet/traits/internal/provider"
	"github.com/talbet/traits/internal/trait"
)

// recording is one live exchange saved under testdata/live. The request body
// is saved without headers, so no credential is ever written.
type recording struct {
	Recorded  string              `json:"recorded"`
	Questions []provider.Question `json:"questions"`
	Request   json.RawMessage     `json:"request"`
	Response  json.RawMessage     `json:"response"`
}

type recorder struct {
	base     http.RoundTripper
	req, res []byte
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		r.req, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(r.req))
	}
	resp, err := r.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	r.res, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(r.res))
	return resp, nil
}

const liveTrait = `---
name: no-debug-prints
subject: Debugging output left in code.
requirement: Do not leave fmt.Println debugging statements in committed code.
purpose: Debug output pollutes logs and leaks internals.
appliesTo: "*.go"
---
`

const liveTrait2 = `---
name: wrap-errors-with-context
subject: Errors returned from Go functions.
requirement: When returning an error from a call, wrap it with fmt.Errorf and %w, adding what was being done.
purpose: Bare errors lose the context needed to debug failures.
appliesTo: "*.go"
---
`

const liveDiff = `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -3,6 +3,7 @@ import "fmt"
 func load(path string) ([]byte, error) {
 	data, err := os.ReadFile(path)
 	if err != nil {
+		fmt.Println("load failed", path)
 		return nil, err
 	}
 	return data, nil
@@ -20,5 +21,6 @@ func main() {
 	cfg, err := load("config.json")
 	if err != nil {
 		log.Fatal(err)
 	}
+	fmt.Println("config:", string(cfg))
 }
`

// liveCases are the exchanges recorded by TestRecordLive.
func liveCases(t *testing.T) map[string]provider.Request {
	t.Helper()
	a, err := trait.Parse("no-debug-prints.md", []byte(liveTrait))
	if err != nil {
		t.Fatal(err)
	}
	b, err := trait.Parse("wrap-errors-with-context.md", []byte(liveTrait2))
	if err != nil {
		t.Fatal(err)
	}
	files, err := diff.Parse(strings.NewReader(liveDiff))
	if err != nil {
		t.Fatal(err)
	}
	in := compile.HunkInput("main.go", files[0].Hunks)
	raw, _ := json.Marshal(in)
	return map[string]provider.Request{
		"traits": {Input: raw, CacheKey: "traits-live-fixture", Questions: []provider.Question{
			compile.TraitQuestion(a), compile.TraitQuestion(b),
		}},
		"locate": {Input: raw, CacheKey: "traits-live-fixture", Questions: []provider.Question{
			compile.LocateQuestion(a, in),
		}},
		"shapes": {Input: raw, Questions: []provider.Question{
			{Name: "adds_print/stdout", Type: provider.Predicate, Instructions: "Does the change add a call that prints to standard output?"},
			{Name: "risk", Type: provider.Score, Instructions: "How risky is this change to ship?", Options: []provider.Option{
				{Value: "low", Description: "Cosmetic or logging only."},
				{Value: "medium", Description: "Could change behaviour in rare cases."},
				{Value: "high", Description: "Likely to break callers."},
			}},
		}},
	}
}

// TestRecordLive makes real, paid Decisions API calls and rewrites
// testdata/live. It runs only with TRAITS_RECORD_LIVE=1 and OPENAI_API_KEY.
func TestRecordLive(t *testing.T) {
	if os.Getenv("TRAITS_RECORD_LIVE") != "1" {
		t.Skip("set TRAITS_RECORD_LIVE=1 and OPENAI_API_KEY to record live fixtures")
	}
	dm, err := New(provider.Config{APIKey: os.Getenv("OPENAI_API_KEY"), Model: Luna, Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	m := dm.(*Model)
	rec := &recorder{base: http.DefaultTransport}
	m.client.Transport = rec
	if err := os.MkdirAll("testdata/live", 0o755); err != nil {
		t.Fatal(err)
	}
	for name, req := range liveCases(t) {
		rec.req, rec.res = nil, nil
		_, derr := m.Decide(context.Background(), req)
		if rec.res == nil {
			t.Fatalf("%s: no response: %v", name, derr)
		}
		out, _ := json.MarshalIndent(recording{
			Recorded:  time.Now().UTC().Format(time.RFC3339),
			Questions: req.Questions,
			Request:   rec.req,
			Response:  rec.res,
		}, "", "  ")
		if err := os.WriteFile(filepath.Join("testdata/live", name+".json"), append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if derr != nil {
			t.Errorf("%s: adapter rejected the live response: %v", name, derr)
		}
	}
}

// TestReplayLive parses every recorded live response with the adapter.
func TestReplayLive(t *testing.T) {
	paths, _ := filepath.Glob("testdata/live/*.json")
	if len(paths) == 0 {
		t.Skip("no live recordings")
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var r recording
			if err := json.Unmarshal(data, &r); err != nil {
				t.Fatal(err)
			}
			res, err := parseResponse(r.Response, r.Questions)
			if err != nil {
				t.Fatalf("adapter rejects recorded response: %v", err)
			}
			if len(res.Answers) != len(r.Questions) || res.Usage.InputTokens == 0 {
				t.Fatalf("response = %+v", res)
			}
			for i, a := range res.Answers {
				q := r.Questions[i]
				if a.Name != q.Name || a.Type != q.Type {
					t.Errorf("answer %d = %+v for question %s/%s", i, a, q.Name, q.Type)
				}
				if q.Type != provider.Predicate {
					var sum float64
					for _, v := range a.Probabilities {
						sum += v
					}
					if len(a.Probabilities) != len(q.Options) || sum < 0.98 || sum > 1.02 {
						t.Errorf("%s: probabilities %v (sum %.3f)", q.Name, a.Probabilities, sum)
					}
				}
			}
		})
	}
}
