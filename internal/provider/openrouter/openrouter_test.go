package openrouter

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/talbet/traits/internal/provider"
	"github.com/talbet/traits/internal/provider/providertest"
	"github.com/talbet/traits/internal/provider/systemone"
)

var questions = []provider.Question{
	{Name: "p", Type: provider.Predicate, Instructions: "P?"},
	{Name: "c", Type: provider.Choice, Instructions: "C?", Options: []provider.Option{{Value: "a", Description: "A"}, {Value: "b", Description: "B"}}},
	{Name: "s", Type: provider.Score, Instructions: "S?", Options: []provider.Option{{Value: "low", Description: "Low"}, {Value: "high", Description: "High"}}},
}

func model(t *testing.T, base string) provider.DecisionModel {
	t.Helper()
	m, err := New(provider.Config{APIKey: "or-key", BaseURL: base})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDecide(t *testing.T) {
	srv, rec := providertest.Server(t, 200, "answers.json")
	// A chat API root ending in /v1 is accepted, as @effect/ai-openrouter does.
	res, err := model(t, srv.URL+"/api/v1").Decide(context.Background(), provider.Request{
		Input: json.RawMessage(`{"diff":"+x"}`), Questions: questions,
	})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	want := provider.Response{
		Model: "typesafe/jev-1.13",
		Usage: provider.Usage{InputTokens: 230},
		Answers: []provider.Answer{
			{Name: "p", Type: provider.Predicate, Probability: 0.71},
			{Name: "c", Type: provider.Choice, Value: "b", Confidence: 0.55, Probabilities: map[string]float64{"a": 0.45, "b": 0.55}},
			{Name: "s", Type: provider.Score, Value: "low", Score: 0.4, Confidence: 0.6, Probabilities: map[string]float64{"low": 0.6, "high": 0.4}},
		},
	}
	if !reflect.DeepEqual(res, want) {
		t.Errorf("Decide() =\n%#v\nwant\n%#v", res, want)
	}
	_, path, header, body := rec.Last()
	if path != "/api"+Endpoint || header.Get("Authorization") != "Bearer or-key" {
		t.Errorf("path %q, auth %q", path, header.Get("Authorization"))
	}
	var got systemone.Request
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != DefaultModel || !reflect.DeepEqual(got.State, map[string]any{"diff": "+x"}) || len(got.Questions) != 3 {
		t.Errorf("body = %s", body)
	}
}

func TestDecideErrors(t *testing.T) {
	tests := []struct {
		fixture string
		status  int
		want    string
	}{
		{"refusal.json", 200, `refused question "p"`},
		{"error.json", 400, "400 Bad Request: " + `{"error":{"code":400,"message":"Model does not support decisions"}}`},
	}
	for _, tt := range tests {
		srv, _ := providertest.Server(t, tt.status, tt.fixture)
		_, err := model(t, srv.URL).Decide(context.Background(), provider.Request{Input: json.RawMessage(`{}`), Questions: questions})
		providertest.ProviderError(t, err)
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %q, want %q", tt.fixture, err, tt.want)
		}
	}
}

func TestNew(t *testing.T) {
	p, ok := provider.Lookup("openrouter")
	if !ok || p.KeyEnv != KeyEnv || p.URLEnv != URLEnv || p.Default != DefaultModel {
		t.Fatalf("registration = %+v", p)
	}
	if _, err := p.New(provider.Config{}); err == nil || !strings.Contains(err.Error(), KeyEnv) {
		t.Errorf("New() without key error = %v", err)
	}
	m, err := p.New(provider.Config{APIKey: "k", Model: "typesafe/jev-1.13"})
	if err != nil {
		t.Fatal(err)
	}
	if c := m.(*systemone.Client); c.URL != "https://openrouter.ai/api/alpha/decisions" || c.Model != "typesafe/jev-1.13" {
		t.Errorf("client = %+v", c)
	}
}
