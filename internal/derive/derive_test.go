package derive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/talbet/traits/internal/llm"
	"github.com/talbet/traits/internal/provider/systemone"
	"github.com/talbet/traits/internal/trait"
)

// judge answers one decision question. kind is "decompose", "relate" or
// "coverage"; proposal is the proposed trait's name; existing is the stored
// trait embedded in the instructions ("" for decompose).
type judge func(kind, proposal, existing string) string

// decisionServer is a fake System One server that answers with j, and
// counts requests.
func decisionServer(t *testing.T, j judge, maxQuestions int) (*systemone.Client, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
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
		if maxQuestions > 0 && len(req.Questions) > maxQuestions {
			http.Error(w, "too many questions", 400)
			return
		}
		var state struct {
			ProposedTrait struct{ Name string } `json:"proposedTrait"`
		}
		if err := json.Unmarshal([]byte(req.State), &state); err != nil {
			http.Error(w, "state: "+err.Error(), 400)
			return
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			kind, existing := "decompose", ""
			switch {
			case strings.HasPrefix(q.Instructions, relateIntro):
				kind = "relate"
			case strings.HasPrefix(q.Instructions, coverageIntro):
				kind = "coverage"
			}
			if kind != "decompose" {
				var e struct{ Name string }
				_ = json.Unmarshal([]byte(strings.SplitN(q.Instructions, "\n", 3)[1]), &e)
				existing = e.Name
			}
			answers[id] = map[string]any{"type": "choice", "choice": j(kind, state.ProposedTrait.Name, existing), "confidence": 0.9}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "answers": answers})
	}))
	t.Cleanup(srv.Close)
	return &systemone.Client{Provider: "fake", URL: srv.URL, Model: "fake", MaxQuestions: maxQuestions}, &n
}

// rules builds a judge from per-(proposal, existing) answers; unlisted pairs
// are distinct and complete, and proposals are atomic unless listed under
// decompose.
func rules(relate map[string]string, coverage map[string]string, decompose ...string) judge {
	return func(kind, p, e string) string {
		switch kind {
		case "decompose":
			for _, d := range decompose {
				if d == p {
					return "decomposable"
				}
			}
			return "atomic"
		case "relate":
			if v, ok := relate[p+"|"+e]; ok {
				return v
			}
			return "distinct"
		}
		if v, ok := coverage[p+"|"+e]; ok {
			return v
		}
		return "complete"
	}
}

// chatServer is a fake chat-completions server answering by schema name.
func chatServer(t *testing.T, answer func(schema, user string) string) *llm.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
			Format   struct {
				JSONSchema struct{ Name string } `json:"json_schema"`
			} `json:"response_format"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		content := answer(req.Format.JSONSchema.Name, req.Messages[len(req.Messages)-1].Content)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "stop", "message": map[string]any{"content": content}}}})
	}))
	t.Cleanup(srv.Close)
	return &llm.Client{BaseURL: srv.URL, Model: "fake"}
}

func mk(name, req string) trait.Trait {
	t, err := trait.Parse("", trait.Serialize(trait.Trait{Name: name, Subject: "Code.", Requirement: req, Purpose: "Because."}))
	if err != nil {
		panic(err)
	}
	return t
}

// store writes traits into a new directory and returns it with the loaded
// traits.
func store(t *testing.T, traits ...trait.Trait) (string, []trait.Trait) {
	t.Helper()
	dir := t.TempDir()
	for _, tr := range traits {
		p := tr.Path
		if p == "" {
			p = tr.Name + ".md"
		}
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, trait.Serialize(tr), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := trait.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, loaded
}

func derive(t *testing.T, j judge, lm LanguageModel, dir string, existing []trait.Trait, p trait.Trait) Plan {
	t.Helper()
	dm, _ := decisionServer(t, j, 0)
	plan, err := Derive(context.Background(), dm, lm, existing, p)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestCreated(t *testing.T) {
	dir, existing := store(t, mk("a", "Rule A."))
	p := mk("b", "Rule B.")
	plan := derive(t, rules(nil, nil), nil, dir, existing, p)
	if len(plan.Items) != 1 || plan.Items[0].Outcome != OutcomeCreated || len(plan.ID) != 12 {
		t.Fatalf("plan = %+v", plan)
	}
	if err := Apply(dir, plan, plan.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, err := trait.Parse("b.md", must(os.ReadFile(filepath.Join(dir, "b.md"))))
	if err != nil || got.Requirement != "Rule B." {
		t.Errorf("written %+v err %v", got, err)
	}
	// The written file changed the store, so the same plan is now stale.
	if err := Apply(dir, plan, plan.ID, nil); !errors.Is(err, ErrStale) {
		t.Errorf("reapply: %v", err)
	}
}

func TestCoveredComplete(t *testing.T) {
	dir, existing := store(t, mk("a", "Rule A."))
	plan := derive(t, rules(map[string]string{"b|a": "covered"}, nil), nil, dir, existing, mk("b", "Rule A again."))
	it := plan.Items[0]
	if it.Outcome != OutcomeCovered || !reflect.DeepEqual(it.Targets, []string{"a"}) || it.Relations[0].Coverage != Complete {
		t.Fatalf("item = %+v", it)
	}
	if b := plan.Blocked(nil); len(b) != 0 {
		t.Errorf("blocked: %v", b)
	}
	if err := Apply(dir, plan, plan.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.md")); err == nil {
		t.Error("covered proposal was written")
	}
}

func TestCoverageGap(t *testing.T) {
	dir, existing := store(t, mk("a", "Rule A."))
	plan := derive(t, rules(map[string]string{"b|a": "covered"}, map[string]string{"b|a": "incomplete"}), nil, dir, existing, mk("b", "Rule A and more."))
	if plan.Items[0].Outcome != OutcomeCoverageGap || len(plan.Blocked(nil)) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	if err := Apply(dir, plan, plan.ID, nil); !errors.Is(err, ErrNeedsAttention) {
		t.Errorf("apply: %v", err)
	}
}

func TestClarificationKeepsExtensions(t *testing.T) {
	x := mk("errors-local", "Keep errors local.")
	x.AppliesTo, x.When, x.Fix, x.Scope, x.Thresholds = []string{"*.ts"}, []string{"Error"}, "Move it.", trait.ScopeFile, trait.Thresholds{Fix: 0.9}
	x.Path = "sub/errors-local.md"
	dir, existing := store(t, x, mk("other", "Other."))
	p := mk("clearer-errors", "Define small error types beside the operations that throw them.")
	p.Fix = "Put it next to the thrower."
	p.Context = "Example."
	plan := derive(t, rules(map[string]string{"clearer-errors|errors-local": "clarification"}, nil), nil, dir, existing, p)
	it := plan.Items[0]
	if it.Outcome != OutcomeClarification || it.Result == nil {
		t.Fatalf("item = %+v", it)
	}
	if err := Apply(dir, plan, plan.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, err := trait.Parse("sub/errors-local.md", must(os.ReadFile(filepath.Join(dir, "sub", "errors-local.md"))))
	if err != nil {
		t.Fatal(err)
	}
	want := x
	want.Requirement, want.Fix, want.Context = p.Requirement, p.Fix, p.Context
	if !reflect.DeepEqual(got, want) {
		t.Errorf("updated\n got %+v\nwant %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "clearer-errors.md")); err == nil {
		t.Error("clarification created a new file")
	}
}

func TestConflictNeedsResolve(t *testing.T) {
	j := rules(map[string]string{"b|a": "conflict"}, nil)
	p := mk("b", "Never A.")

	dir, existing := store(t, mk("a", "Always A."))
	plan := derive(t, j, nil, dir, existing, p)
	if plan.Items[0].Outcome != OutcomeConflict || len(plan.Blocked(nil)) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	if err := Apply(dir, plan, plan.ID, nil); !errors.Is(err, ErrNeedsAttention) {
		t.Errorf("unresolved apply: %v", err)
	}
	for _, bad := range []map[string]string{{"a": "maybe"}, {"zzz": "keep"}} {
		if err := Apply(dir, plan, plan.ID, bad); err == nil {
			t.Errorf("resolutions %v accepted", bad)
		}
	}

	// keep: nothing changes.
	if err := Apply(dir, plan, plan.ID, map[string]string{"a": Keep}); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, "a.md")

	// both: the proposal is added.
	if err := Apply(dir, plan, plan.ID, map[string]string{"a": Both}); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, "a.md", "b.md")

	// replace: a is removed, b written.
	dir, existing = store(t, mk("a", "Always A."))
	plan = derive(t, j, nil, dir, existing, p)
	if err := Apply(dir, plan, plan.ID, map[string]string{"a": Replace}); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, "b.md")
}

func TestDecomposableWithoutLLM(t *testing.T) {
	dir, existing := store(t)
	dm, _ := decisionServer(t, rules(nil, nil, "two-rules"), 0)
	_, err := Derive(context.Background(), dm, nil, existing, mk("two-rules", "Do X. Also do Y."))
	if !errors.Is(err, ErrNeedLLM) || !strings.Contains(err.Error(), "split it") {
		t.Errorf("err = %v", err)
	}
	_ = dir
}

func TestDecomposableWithLLM(t *testing.T) {
	lm := chatServer(t, func(schema, user string) string {
		if schema != "traits" || !strings.Contains(user, "Do X. Also do Y.") {
			return `{"traits":[]}`
		}
		return `{"traits":[
{"name":"do-x","subject":"Code.","requirement":"Do X.","applicability":null,"purpose":"X matters.","context":null},
{"name":"do-y","subject":"Code.","requirement":"Do Y.","applicability":"Only in tests.","purpose":"Y matters.","context":"Example."}]}`
	})
	p := mk("two-rules", "Do X. Also do Y.")
	p.AppliesTo, p.Fix = []string{"*.go"}, "Fix both."
	dir, existing := store(t, mk("a", "Rule A."))
	plan := derive(t, rules(nil, nil, "two-rules"), lm, dir, existing, p)
	if len(plan.Items) != 2 || plan.Items[0].Outcome != OutcomeCreated || plan.Items[1].Outcome != OutcomeCreated {
		t.Fatalf("plan = %+v", plan)
	}
	if err := Apply(dir, plan, plan.ID, nil); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, "a.md", "do-x.md", "do-y.md")
	y, err := trait.Parse("do-y.md", must(os.ReadFile(filepath.Join(dir, "do-y.md"))))
	if err != nil || y.Applicability != "Only in tests." || !reflect.DeepEqual(y.AppliesTo, []string{"*.go"}) || y.Fix != "" {
		t.Errorf("do-y = %+v err %v", y, err)
	}
}

func TestDecomposeRejectsBadLLMTraits(t *testing.T) {
	lm := chatServer(t, func(string, string) string {
		return `{"traits":[{"name":"Bad Name","subject":"s","requirement":"r","applicability":null,"purpose":"p","context":null},
{"name":"ok","subject":"s","requirement":"r","applicability":null,"purpose":"p","context":null}]}`
	})
	dm, _ := decisionServer(t, rules(nil, nil, "x"), 0)
	if _, err := Decompose(context.Background(), dm, lm, mk("x", "r")); err == nil || !strings.Contains(err.Error(), "kebab-case") {
		t.Errorf("err = %v", err)
	}
}

func TestDraft(t *testing.T) {
	var gotUser string
	lm := chatServer(t, func(schema, user string) string {
		gotUser = user
		return `{"name":"prefer-early-returns","subject":"Functions with nested conditionals.","requirement":"Return early instead of nesting the happy path.","applicability":null,"purpose":"Flat code is easier to read.","context":"Guard clauses first."}`
	})
	diff := "-if ok {\n-  run()\n-}\n+if !ok {\n+  return\n+}\n+run()\n"
	tr, err := Draft(context.Background(), lm, "please use early returns", diff)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Name != "prefer-early-returns" || tr.Applicability != "" {
		t.Errorf("trait = %+v", tr)
	}
	if !strings.HasPrefix(tr.Context, "Guard clauses first.\n\n```diff\n-if ok {") || !strings.HasSuffix(tr.Context, "+run()\n```") {
		t.Errorf("context = %q", tr.Context)
	}
	var in map[string]string
	if err := json.Unmarshal([]byte(gotUser), &in); err != nil || in["feedback"] != "please use early returns" || in["diff"] != diff {
		t.Errorf("user message = %q", gotUser)
	}
	if _, err := Draft(context.Background(), nil, "x", ""); !errors.Is(err, ErrNeedLLM) {
		t.Errorf("nil LLM: %v", err)
	}
}

func TestFenceOutrunsBackticks(t *testing.T) {
	if got := fence("diff", "+```go"); got != "````diff\n+```go\n````" {
		t.Errorf("fence = %q", got)
	}
}

func TestPlanIDMismatchAfterStoreChange(t *testing.T) {
	dir, existing := store(t, mk("a", "Rule A."))
	plan := derive(t, rules(nil, nil), nil, dir, existing, mk("b", "Rule B."))
	if err := Apply(dir, plan, "000000000000", nil); !errors.Is(err, ErrStale) {
		t.Errorf("wrong id: %v", err)
	}
	changed := mk("a", "Rule A, edited.")
	os.WriteFile(filepath.Join(dir, "a.md"), trait.Serialize(changed), 0o644)
	if err := Apply(dir, plan, plan.ID, nil); !errors.Is(err, ErrStale) {
		t.Errorf("changed store: %v", err)
	}
	assertFiles(t, dir, "a.md")
}

func TestNameClash(t *testing.T) {
	dir, existing := store(t, mk("a", "Rule A."))
	dm, _ := decisionServer(t, rules(nil, nil), 0)
	_, err := Derive(context.Background(), dm, nil, existing, mk("a", "Something unrelated."))
	if !errors.Is(err, ErrNameClash) {
		t.Errorf("err = %v", err)
	}
	// A file appearing at the target outside the store (here a directory,
	// which Load skips) is never overwritten.
	plan := derive(t, rules(nil, nil), nil, dir, existing, mk("b", "Rule B."))
	os.Mkdir(filepath.Join(dir, "b.md"), 0o755)
	if err := Apply(dir, plan, plan.ID, nil); !errors.Is(err, ErrNameClash) {
		t.Errorf("apply over existing path: %v", err)
	}
}

func TestRelateBatchesByMaxQuestions(t *testing.T) {
	dir, existing := store(t, mk("a", "A."), mk("b", "B."), mk("c", "C."))
	_ = dir
	dm, n := decisionServer(t, rules(map[string]string{"p|a": "covered", "p|c": "covered"}, nil), 2)
	rels, err := Relate(context.Background(), dm, existing, mk("p", "P."))
	if err != nil {
		t.Fatal(err)
	}
	// 3 relate questions in 2 requests, 2 coverage questions in 1.
	if n.Load() != 3 {
		t.Errorf("requests = %d, want 3", n.Load())
	}
	if rels[0].Relationship != Covered || rels[0].Coverage != Complete || rels[1].Relationship != Distinct || rels[1].Coverage != "" {
		t.Errorf("relations = %+v", rels)
	}
}

func TestPlanJSONRoundTrip(t *testing.T) {
	x := mk("a", "Rule A.")
	x.AppliesTo = []string{"*.go"}
	dir, existing := store(t, x)
	plan := derive(t, rules(map[string]string{"b|a": "clarification"}, nil), nil, dir, existing, mk("b", "Rule A, clearer."))
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var back Plan
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, plan) || PlanID(existing, back.Items) != plan.ID {
		t.Errorf("round trip differs:\n%+v\n%+v", back, plan)
	}
}

func assertFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	var got []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() {
			got = append(got, e.Name())
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
