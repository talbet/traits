package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talbet/traits/internal/provider"
)

func TestURL(t *testing.T) {
	for base, want := range map[string]string{
		"":                              "https://api.openai.com/v1/chat/completions",
		"http://h:1":                    "http://h:1/v1/chat/completions",
		"http://h:1/":                   "http://h:1/v1/chat/completions",
		"http://h:1/v1":                 "http://h:1/v1/chat/completions",
		"https://openrouter.ai/api/v1/": "https://openrouter.ai/api/v1/chat/completions",
	} {
		if got := URL(base); got != want {
			t.Errorf("URL(%q) = %q, want %q", base, got, want)
		}
	}
}

// chatServer answers every request with content, recording the last request.
func chatServer(t *testing.T, status int, reply string) (*httptest.Server, *map[string]any, *http.Header) {
	t.Helper()
	var got map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		hdr = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &hdr
}

func choice(content string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content}}}})
	return string(b)
}

func TestGenerate(t *testing.T) {
	srv, got, hdr := chatServer(t, 200, choice(`{"a": 1}`))
	c := &Client{BaseURL: srv.URL + "/v1/", Model: "m", APIKey: "k"}
	schema := json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"}},"required":["a"],"additionalProperties":false}`)
	out, err := c.Generate(context.Background(), "sys", "usr", "thing", schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"a":1}` {
		t.Errorf("out = %s", out)
	}
	if hdr.Get("Authorization") != "Bearer k" {
		t.Errorf("auth = %q", hdr.Get("Authorization"))
	}
	req := *got
	if req["model"] != "m" {
		t.Errorf("model = %v", req["model"])
	}
	msgs := req["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["content"] != "usr" {
		t.Errorf("messages = %v", msgs)
	}
	rf := req["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["name"] != "thing" || js["strict"] != true || js["schema"].(map[string]any)["type"] != "object" {
		t.Errorf("response_format = %v", rf)
	}
}

func TestGenerateFencedContent(t *testing.T) {
	srv, _, _ := chatServer(t, 200, choice("```json\n{\"a\":2}\n```"))
	c := &Client{BaseURL: srv.URL, Model: "m"}
	out, err := c.Generate(context.Background(), "s", "u", "x", json.RawMessage(`{}`))
	if err != nil || string(out) != `{"a":2}` {
		t.Errorf("out %s err %v", out, err)
	}
}

func TestGenerateErrors(t *testing.T) {
	refusal, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"refusal": "no"}}}})
	cut, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "length", "message": map[string]any{"content": `{"a":`}}}})
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"array":   {200, choice(`[1,2]`), "not a JSON object"},
		"text":    {200, choice(`hello`), "not a JSON object"},
		"null":    {200, choice(`null`), "not a JSON object"},
		"refusal": {200, string(refusal), "refused"},
		"cut":     {200, string(cut), "cut off"},
		"empty":   {200, `{"choices":[]}`, "no choices"},
		"http":    {400, `{"error":"bad schema"}`, "bad schema"},
	} {
		srv, _, _ := chatServer(t, tc.status, tc.body)
		c := &Client{BaseURL: srv.URL, Model: "m"}
		_, err := c.Generate(context.Background(), "s", "u", "x", json.RawMessage(`{}`))
		var pe *provider.Error
		if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.As(err, &pe) {
			t.Errorf("%s: err = %v, want *provider.Error containing %q", name, err, tc.want)
		}
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(ModelEnv, "")
	t.Setenv(KeyEnv, "")
	t.Setenv(OpenAIKeyEnv, "ok")
	if _, err := FromEnv(); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no model: err = %v", err)
	}
	t.Setenv(ModelEnv, "gpt")
	c, err := FromEnv()
	if err != nil || c.Model != "gpt" || c.APIKey != "ok" {
		t.Fatalf("c = %+v err %v", c, err)
	}
	t.Setenv(KeyEnv, "tk")
	if c, _ := FromEnv(); c.APIKey != "tk" {
		t.Errorf("TRAITS_LLM_API_KEY should win, got %q", c.APIKey)
	}
}

func TestFromEnvNeverSendsOpenAIKeyElsewhere(t *testing.T) {
	t.Setenv(ModelEnv, "m")
	t.Setenv(KeyEnv, "")
	t.Setenv(OpenAIKeyEnv, "sk-openai")
	for base, want := range map[string]string{
		"":                               "sk-openai",
		"https://api.openai.com":         "sk-openai",
		"https://api.openai.com/v1":      "sk-openai",
		"https://openrouter.ai/api":      "",
		"http://api.openai.com":          "",
		"https://api.openai.com.evil.io": "",
	} {
		t.Setenv(BaseURLEnv, base)
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.APIKey != want {
			t.Errorf("base %q: key %q, want %q", base, c.APIKey, want)
		}
	}
}
