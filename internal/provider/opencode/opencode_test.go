package opencode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/talbet/traits/internal/provider"
	"github.com/talbet/traits/internal/provider/providertest"
	"github.com/talbet/traits/internal/provider/systemone"
)

func TestDecide(t *testing.T) {
	srv, rec := providertest.Server(t, 200, "answers.json")
	m, err := New(provider.Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.Decide(context.Background(), provider.Request{
		Input:     json.RawMessage(`{"a":1}`),
		Questions: []provider.Question{{Name: "x", Type: provider.Predicate, Instructions: "X?"}},
	})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if len(res.Answers) != 1 || res.Answers[0].Probability != 0.12 || res.Model != Jev113Free {
		t.Errorf("Decide() = %#v", res)
	}
	_, path, header, _ := rec.Last()
	if path != systemone.Endpoint || header.Get("Authorization") != "" {
		t.Errorf("path %q, auth %q", path, header.Get("Authorization"))
	}
}

func TestNew(t *testing.T) {
	p, ok := provider.Lookup("opencode")
	if !ok || p.KeyEnv != KeyEnv || p.Default != Jev113Free {
		t.Fatalf("registration = %+v", p)
	}
	if _, err := p.New(provider.Config{Model: Jev113}); err == nil || !strings.Contains(err.Error(), KeyEnv) {
		t.Errorf("New(jev-1.13) without key error = %v", err)
	}
	if _, err := p.New(provider.Config{Model: "gpt"}); err == nil {
		t.Error("New() accepted an unknown model")
	}
	m, err := p.New(provider.Config{Model: Jev113, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if c := m.(*systemone.Client); c.URL != BaseURL+systemone.Endpoint {
		t.Errorf("URL = %q", c.URL)
	}
}
