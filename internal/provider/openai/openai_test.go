package openai

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/talbet/traits/internal/provider"
	"github.com/talbet/traits/internal/provider/providertest"
)

var questions = []provider.Question{
	{Name: "damaged", Type: provider.Predicate, Instructions: "Does the customer report a damaged item?"},
	{Name: "department", Type: provider.Choice, Instructions: "Which team?", Options: []provider.Option{
		{Value: "billing", Description: "Payments"}, {Value: "shipping"}, {Value: "other", Description: "Anything else"},
	}},
	{Name: "severity", Type: provider.Score, Instructions: "How severe?", Options: []provider.Option{
		{Value: "Cosmetic", Description: "Looks wrong"}, {Value: "Workaround available"}, {Value: "Fully blocked", Description: "Cannot proceed"},
	}},
}

var input = json.RawMessage(`{"message": "My order arrived broken", "order": 42}`)

func model(t *testing.T, url string) provider.DecisionModel {
	t.Helper()
	m, err := New(provider.Config{APIKey: "sk-test", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func decide(t *testing.T, fixture string, status int) (provider.Response, error, *providertest.Recorded) {
	t.Helper()
	srv, rec := providertest.Server(t, status, fixture)
	res, err := model(t, srv.URL).Decide(context.Background(), provider.Request{Input: input, Questions: questions, CacheKey: "repo:a.go"})
	return res, err, rec
}

func TestDecide(t *testing.T) {
	res, err, rec := decide(t, "decision.json", 200)
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	want := provider.Response{
		Model: Luna,
		Usage: provider.Usage{InputTokens: 1830, CachedTokens: 1024},
		Answers: []provider.Answer{
			{Name: "damaged", Type: provider.Predicate, Probability: 0.95},
			{Name: "department", Type: provider.Choice, Value: "billing", Confidence: 0.93,
				Probabilities: map[string]float64{"billing": 0.95, "shipping": 0.04, "other": 0.01}},
			{Name: "severity", Type: provider.Score, Value: "Workaround available", Score: 1.1, Confidence: 0.55,
				Probabilities: map[string]float64{"Cosmetic": 0.1, "Workaround available": 0.7, "Fully blocked": 0.2}},
		},
	}
	if !reflect.DeepEqual(res, want) {
		t.Errorf("Decide() =\n%#v\nwant\n%#v", res, want)
	}

	method, path, header, body := rec.Last()
	if method != "POST" || path != Endpoint || header.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("request = %s %s auth %q", method, path, header.Get("Authorization"))
	}
	var got, wantBody any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(`{
		"model": "gpt-6-luna",
		"input": [{"role": "user", "content": [{"type": "input_json", "data": {"message": "My order arrived broken", "order": 42}}]}],
		"questions": [
			{"type": "predicate", "name": "damaged", "instructions": "Does the customer report a damaged item?"},
			{"type": "choice", "name": "department", "instructions": "Which team?", "choices": [
				{"value": "billing", "description": "Payments"}, {"value": "shipping"}, {"value": "other", "description": "Anything else"}]},
			{"type": "score", "name": "severity", "instructions": "How severe?", "levels": [
				{"label": "Cosmetic", "description": "Looks wrong"}, {"label": "Workaround available"}, {"label": "Fully blocked", "description": "Cannot proceed"}]}
		],
		"prompt_cache_key": "repo:a.go"
	}`), &wantBody)
	if !reflect.DeepEqual(got, wantBody) {
		t.Errorf("body =\n%s\nwant\n%v", body, wantBody)
	}
}

// The probabilities shape is accepted as an object map as well as the
// documented array.
func TestDecideMapProbabilities(t *testing.T) {
	res, err, _ := decide(t, "map_probabilities.json", 200)
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	want := []provider.Answer{
		{Name: "damaged", Type: provider.Predicate, Probability: 0.3},
		{Name: "department", Type: provider.Choice, Value: "shipping", Confidence: 0.8,
			Probabilities: map[string]float64{"billing": 0.1, "shipping": 0.8, "other": 0.1}},
		{Name: "severity", Type: provider.Score, Value: "Fully blocked", Score: 1.9, Confidence: 0.8,
			Probabilities: map[string]float64{"Workaround available": 0.1, "Fully blocked": 0.9}},
	}
	if !reflect.DeepEqual(res.Answers, want) {
		t.Errorf("Answers =\n%#v\nwant\n%#v", res.Answers, want)
	}
}

func TestDecideMatchesAnswers(t *testing.T) {
	res, err, _ := decide(t, "unnamed.json", 200)
	if err != nil {
		t.Fatalf("unnamed: %v", err)
	}
	if res.Answers[1].Value != "other" || res.Answers[2].Value != "Cosmetic" || res.Answers[2].Score != 0.2 {
		t.Errorf("unnamed answers = %#v", res.Answers)
	}

	res, err, _ = decide(t, "reordered.json", 200)
	if err != nil {
		t.Fatalf("reordered: %v", err)
	}
	if res.Answers[0].Name != "damaged" || res.Answers[0].Probability != 0.1 || res.Answers[2].Value != "Fully blocked" {
		t.Errorf("reordered answers = %#v", res.Answers)
	}
}

func TestDecideReportsProviderFaults(t *testing.T) {
	tests := []struct {
		fixture string
		status  int
		want    string
	}{
		{"refusal.json", 200, `refused question "department"`},
		{"mismatch.json", 200, `answer 1 is named "dept", want "department"`},
		{"too_few.json", 200, "1 answers for 3 questions"},
		{"bad_probabilities_shape.json", 200, "want an array or object"},
		{"unknown_choice.json", 200, `choice "legal" is not an option`},
		{"bad_probability.json", 200, "missing or invalid probability"},
		{"score_out_of_range.json", 200, "score 3 is out of range"},
		{"error.json", 400, `400 Bad Request: {"error": {"message": "Rate limit reached`},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			_, err, _ := decide(t, tt.fixture, tt.status)
			providertest.ProviderError(t, err)
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestDecideRejectsBadRequests(t *testing.T) {
	srv, rec := providertest.Server(t, 200, "decision.json")
	m := model(t, srv.URL)
	big := json.RawMessage(`"` + strings.Repeat("x", MaxInputBytes) + `"`)
	if _, err := m.Decide(context.Background(), provider.Request{Input: big, Questions: questions}); err == nil {
		t.Error("accepted an input over MaxInputBytes")
	}
	many := make([]provider.Question, MaxQuestions+1)
	for i := range many {
		many[i] = provider.Question{Name: "q", Type: provider.Predicate}
	}
	if _, err := m.Decide(context.Background(), provider.Request{Input: input, Questions: many}); err == nil {
		t.Error("accepted more than MaxQuestions")
	}
	if rec.Calls != 0 {
		t.Errorf("sent %d requests", rec.Calls)
	}
	if l := m.Limits(); l.MaxQuestions != MaxQuestions || l.MaxInputBytes != MaxInputBytes {
		t.Errorf("Limits() = %+v", l)
	}
}

func TestCacheKey(t *testing.T) {
	if got := CacheKey("short"); got != "short" {
		t.Errorf("CacheKey(short) = %q", got)
	}
	long := strings.Repeat("k", 65)
	if got := CacheKey(long); len(got) != 64 || got == long[:64] || got != CacheKey(long) {
		t.Errorf("CacheKey(long) = %q", got)
	}
}

func TestNew(t *testing.T) {
	p, ok := provider.Lookup("openai")
	if !ok || p.KeyEnv != KeyEnv || p.URLEnv != URLEnv || p.Default != Luna || !reflect.DeepEqual(p.Models, []string{Luna}) {
		t.Fatalf("registration = %+v", p)
	}
	if _, err := p.New(provider.Config{}); err == nil || !strings.Contains(err.Error(), KeyEnv) {
		t.Errorf("New() without key error = %v", err)
	}
	if _, err := p.New(provider.Config{APIKey: "k", Model: "gpt-5"}); err == nil {
		t.Error("New() accepted another model")
	}
	m, err := p.New(provider.Config{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if u := m.(*Model).url; u != BaseURL+Endpoint {
		t.Errorf("url = %q", u)
	}
}
