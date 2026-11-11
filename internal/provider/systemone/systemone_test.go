package systemone

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/talbet/traits/internal/provider"
	"github.com/talbet/traits/internal/provider/providertest"
)

// questions are a predicate, a choice and a score, asked as q0, q1, q2.
var questions = []provider.Question{
	{Name: "go/no-panic", Type: provider.Predicate, Instructions: "The change panics."},
	{Name: "style", Type: provider.Choice, Instructions: "Style?", Options: []provider.Option{
		{Value: "compliant", Description: "follows the guide"},
		{Value: "violation", Description: "breaks the guide"},
	}},
	{Name: "risk", Type: provider.Score, Instructions: "Risk?", Options: []provider.Option{
		{Value: "low", Description: "harmless"},
		{Value: "mid"},
		{Value: "high", Description: "dangerous"},
	}},
}

var input = json.RawMessage(`{ "file": "a.go",  "hunks": ["+x"] }`)

func newModel(t *testing.T, url string) provider.DecisionModel {
	t.Helper()
	m, err := New(provider.Config{BaseURL: url + "/", Model: "jev-1.13", APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDecide(t *testing.T) {
	srv, rec := providertest.Server(t, 200, "answers.json")
	res, err := newModel(t, srv.URL).Decide(context.Background(), provider.Request{Input: input, Questions: questions})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	want := provider.Response{
		Model: "jev-1.13",
		Usage: provider.Usage{InputTokens: 412},
		Answers: []provider.Answer{
			{Name: "go/no-panic", Type: provider.Predicate, Probability: 0.94},
			{Name: "style", Type: provider.Choice, Value: "violation", Confidence: 0.82,
				Probabilities: map[string]float64{"compliant": 0.18, "violation": 0.82}},
			{Name: "risk", Type: provider.Score, Value: "mid", Score: 1, Confidence: 0.9,
				Probabilities: map[string]float64{"low": 0.05, "mid": 0.9, "high": 0.05}},
		},
	}
	if !reflect.DeepEqual(res, want) {
		t.Errorf("Decide() =\n%#v\nwant\n%#v", res, want)
	}

	method, path, header, body := rec.Last()
	if method != "POST" || path != Endpoint || header.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("request = %s %s auth %q", method, path, header.Get("Authorization"))
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	wantBody := map[string]any{
		"model": "jev-1.13",
		"state": `{"file":"a.go","hunks":["+x"]}`,
		"questions": map[string]any{
			"q0": map[string]any{"type": "noul", "instructions": "The change panics."},
			"q1": map[string]any{"type": "choice", "instructions": "Style?", "criteria": map[string]any{
				"compliant": "follows the guide", "violation": "breaks the guide"}},
			"q2": map[string]any{"type": "score", "instructions": "Risk?", "criteria": []any{"harmless", "mid", "dangerous"}},
		},
	}
	if !reflect.DeepEqual(got, wantBody) {
		t.Errorf("body =\n%v\nwant\n%v", got, wantBody)
	}
}

func TestDecideWithoutProbabilities(t *testing.T) {
	srv, _ := providertest.Server(t, 200, "no_probabilities.json")
	res, err := newModel(t, srv.URL).Decide(context.Background(), provider.Request{Input: input, Questions: questions})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	want := []provider.Answer{
		{Name: "go/no-panic", Type: provider.Predicate, Probability: 0.2},
		{Name: "style", Type: provider.Choice, Value: "compliant"},
		{Name: "risk", Type: provider.Score, Value: "high", Score: 2, Confidence: 0.7},
	}
	if !reflect.DeepEqual(res.Answers, want) || res.Model != "jev-1.13" {
		t.Errorf("Decide() = %#v", res)
	}
}

// A refusal, a missing or malformed answer is a provider fault, never a pass.
func TestDecideReportsProviderFaults(t *testing.T) {
	tests := map[string]string{
		"missing_answer.json":  `no answer for question "style"`,
		"refusal.json":         `refused question "go/no-panic"`,
		"unknown_option.json":  `choice "maybe" is not an option`,
		"unknown_level.json":   `unknown level "7"`,
		"bad_probability.json": "invalid noul",
		"wrong_type.json":      `answer type "choice", want "noul"`,
		"malformed.json":       "unexpected response",
	}
	for fixture, want := range tests {
		t.Run(fixture, func(t *testing.T) {
			srv, _ := providertest.Server(t, 200, fixture)
			_, err := newModel(t, srv.URL).Decide(context.Background(), provider.Request{Input: input, Questions: questions})
			providertest.ProviderError(t, err)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		})
	}
}

func TestDecideHTTPError(t *testing.T) {
	srv, _ := providertest.Server(t, 404, "malformed.json")
	_, err := newModel(t, srv.URL).Decide(context.Background(), provider.Request{Input: input, Questions: questions})
	providertest.ProviderError(t, err)
	if !strings.Contains(err.Error(), "404 Not Found: not json") {
		t.Errorf("error = %q", err)
	}
}

func TestDecideRejectsBadRequests(t *testing.T) {
	srv, rec := providertest.Server(t, 200, "answers.json")
	c := &Client{Provider: "x", URL: srv.URL, Model: "m", MaxQuestions: 2}
	if _, err := c.Decide(context.Background(), provider.Request{Input: input, Questions: questions}); err == nil {
		t.Error("accepted more than MaxQuestions")
	}
	if _, err := c.Decide(context.Background(), provider.Request{Input: json.RawMessage(`{`), Questions: questions[:1]}); err == nil {
		t.Error("accepted invalid input JSON")
	}
	if rec.Calls != 0 {
		t.Errorf("sent %d requests", rec.Calls)
	}
	if got := c.Limits(); got.MaxQuestions != 2 {
		t.Errorf("Limits() = %+v", got)
	}
}

func TestStateJSON(t *testing.T) {
	srv, rec := providertest.Server(t, 200, "answers.json")
	c := &Client{Provider: "x", URL: srv.URL, Model: "m", StateJSON: true}
	if _, err := c.Decide(context.Background(), provider.Request{Input: input, Questions: questions}); err != nil {
		t.Fatal(err)
	}
	_, _, _, body := rec.Last()
	if !strings.Contains(string(body), `"state":{"file":"a.go","hunks":["+x"]}`) {
		t.Errorf("body = %s, want state as a JSON object", body)
	}
}

func TestNewAndRegistration(t *testing.T) {
	p, ok := provider.Lookup("systemone")
	if !ok || p.URLEnv != URLEnv || p.KeyEnv != KeyEnv || p.Default != "" || len(p.Models) != 0 {
		t.Fatalf("registration = %+v", p)
	}
	t.Setenv(ModelEnv, "")
	if _, err := p.New(provider.Config{}); err == nil || !strings.Contains(err.Error(), ModelEnv) {
		t.Errorf("New() without model error = %v", err)
	}
	t.Setenv(ModelEnv, "nimble")
	m, err := p.New(provider.Config{})
	if err != nil {
		t.Fatal(err)
	}
	c := m.(*Client)
	if c.Model != "nimble" || c.URL != DefaultURL+Endpoint || c.Header != nil {
		t.Errorf("client = %+v", c)
	}
}
