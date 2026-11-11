package cloudflare

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
	{Name: "p", Type: provider.Predicate, Instructions: "P?"},
	{Name: "c", Type: provider.Choice, Instructions: "C?", Options: []provider.Option{{Value: "a"}, {Value: "b"}}},
}

func model(t *testing.T, url string) provider.DecisionModel {
	t.Helper()
	m, err := New(provider.Config{APIKey: "cf-tok", Account: "acct/1", BaseURL: url, Model: ClefFlash})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDecide(t *testing.T) {
	srv, rec := providertest.Server(t, 200, "success.json")
	res, err := model(t, srv.URL).Decide(context.Background(), provider.Request{Input: json.RawMessage(`"diff"`), Questions: questions})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	want := []provider.Answer{
		{Name: "p", Type: provider.Predicate, Probability: 0.87},
		{Name: "c", Type: provider.Choice, Value: "b", Confidence: 0.6, Probabilities: map[string]float64{"a": 0.4, "b": 0.6}},
	}
	if !reflect.DeepEqual(res.Answers, want) || res.Model != ClefFlash || res.Usage.InputTokens != 120 {
		t.Errorf("Decide() = %#v", res)
	}
	_, path, header, body := rec.Last()
	if path != "/accounts/acct/1/ai/run/@cf/cloudflare/clef-flash" {
		t.Errorf("path = %q", path)
	}
	if header.Get("Authorization") != "Bearer cf-tok" {
		t.Errorf("auth = %q", header.Get("Authorization"))
	}
	if !strings.Contains(string(body), `"model":"clef-flash"`) || !strings.Contains(string(body), `"state":"\"diff\""`) {
		t.Errorf("body = %s", body)
	}
}

func TestDecideReportsCloudflareErrors(t *testing.T) {
	tests := []struct {
		fixture string
		status  int
		want    string
	}{
		{"failure.json", 200, "No such model (code 5007)"},
		{"no_result.json", 200, "no result"},
		{"unauthorized.json", 401, "401 Unauthorized: " + `{"result":null,"success":false,"errors":[{"code":10000,"message":"Authentication error"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			srv, _ := providertest.Server(t, tt.status, tt.fixture)
			_, err := model(t, srv.URL).Decide(context.Background(), provider.Request{Input: json.RawMessage(`{}`), Questions: questions})
			providertest.ProviderError(t, err)
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestDecideRejectsTooManyQuestions(t *testing.T) {
	srv, rec := providertest.Server(t, 200, "success.json")
	m := model(t, srv.URL)
	if m.Limits().MaxQuestions != MaxQuestions {
		t.Errorf("Limits() = %+v", m.Limits())
	}
	qs := make([]provider.Question, MaxQuestions+1)
	for i := range qs {
		qs[i] = provider.Question{Name: "q", Type: provider.Predicate}
	}
	if _, err := m.Decide(context.Background(), provider.Request{Input: json.RawMessage(`{}`), Questions: qs}); err == nil {
		t.Fatal("Decide() accepted more than MaxQuestions")
	}
	if rec.Calls != 0 {
		t.Errorf("sent %d requests", rec.Calls)
	}
}

func TestNew(t *testing.T) {
	p, ok := provider.Lookup("cloudflare")
	if !ok || p.KeyEnv != KeyEnv || p.AccountEnv != AccountEnv || p.Default != Clef || !reflect.DeepEqual(p.Models, []string{Clef, ClefFlash}) {
		t.Fatalf("registration = %+v", p)
	}
	tests := []struct {
		cfg  provider.Config
		want string
	}{
		{provider.Config{Account: "a"}, KeyEnv},
		{provider.Config{APIKey: "k"}, AccountEnv},
		{provider.Config{APIKey: "k", Account: "a", Model: "llama"}, "unknown model"},
	}
	for _, tt := range tests {
		if _, err := p.New(tt.cfg); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("New(%+v) error = %v, want %q", tt.cfg, err, tt.want)
		}
	}
	if _, err := p.New(provider.Config{APIKey: "k", Account: "a"}); err != nil {
		t.Errorf("New() error = %v", err)
	}
}
