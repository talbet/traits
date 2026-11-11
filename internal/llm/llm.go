// Package llm is a minimal client for OpenAI-compatible chat completions with
// schema-constrained JSON output. Trait lifecycle steps that need generated
// text (drafting a trait from feedback, splitting a compound trait) use it;
// checks never do.
//
// It posts to {base}/v1/chat/completions with
// response_format {type: "json_schema", json_schema: {name, strict: true, schema}},
// which OpenAI, OpenRouter, Ollama, vLLM and llama.cpp servers accept.
//
// Configuration comes from the environment:
//
//	TRAITS_LLM_MODEL     model name (required; there is no default)
//	TRAITS_LLM_BASE_URL  server base URL (default https://api.openai.com);
//	                     a URL already ending in /v1 is used as is
//	TRAITS_LLM_API_KEY   bearer token, falling back to OPENAI_API_KEY;
//	                     optional, since local servers need none
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/talbet/traits/internal/provider"
)

// Environment variables read by FromEnv.
const (
	ModelEnv     = "TRAITS_LLM_MODEL"
	BaseURLEnv   = "TRAITS_LLM_BASE_URL"
	KeyEnv       = "TRAITS_LLM_API_KEY"
	OpenAIKeyEnv = "OPENAI_API_KEY"
)

// DefaultBaseURL is used when TRAITS_LLM_BASE_URL is unset.
const DefaultBaseURL = "https://api.openai.com"

// DefaultTimeout bounds one generation, including retries, when
// Client.Timeout is zero. Generation is slower than a decision.
const DefaultTimeout = 2 * time.Minute

// name identifies this client in errors.
const name = "llm"

// ErrNotConfigured reports that no model is configured.
var ErrNotConfigured = errors.New("no LLM configured: set " + ModelEnv + " (and " + KeyEnv + " or " + OpenAIKeyEnv + ", and " + BaseURLEnv + " for a non-OpenAI server)")

// Client generates schema-constrained JSON objects.
type Client struct {
	// BaseURL is the server base URL, with or without a trailing /v1.
	BaseURL string
	Model   string
	// APIKey is sent as a bearer token when set.
	APIKey string
	// Timeout bounds a call including retries; zero means DefaultTimeout.
	Timeout time.Duration
	// Retries is how many times to retry 429 and 5xx responses.
	Retries int
	// HTTP is the client used; nil means provider.NewHTTPClient().
	HTTP *http.Client
}

// FromEnv builds a client from the environment. It returns ErrNotConfigured
// when TRAITS_LLM_MODEL is unset.
func FromEnv() (*Client, error) {
	model := strings.TrimSpace(os.Getenv(ModelEnv))
	if model == "" {
		return nil, ErrNotConfigured
	}
	base := os.Getenv(BaseURLEnv)
	key := os.Getenv(KeyEnv)
	if key == "" && isOpenAI(base) {
		// Never send an OpenAI key to another host.
		key = os.Getenv(OpenAIKeyEnv)
	}
	return &Client{BaseURL: base, Model: model, APIKey: key, Retries: 2}, nil
}

// URL returns the chat-completions endpoint for base: base + /v1/chat/completions,
// without doubling a /v1 base already ends in.
func URL(base string) string {
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type request struct {
	Model          string         `json:"model"`
	Messages       []message      `json:"messages"`
	ResponseFormat responseFormat `json:"response_format"`
}

type response struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content *string `json:"content"`
			Refusal *string `json:"refusal"`
		} `json:"message"`
	} `json:"choices"`
}

// Generate asks the model for one JSON object matching schema, given a
// system prompt and a user message. schemaName names the schema on the wire
// (letters, digits, '_' and '-'). The returned value is a JSON object; a
// refusal, a truncated answer, or content that is not a JSON object is an
// error. Every failure is a *provider.Error.
func (c *Client) Generate(ctx context.Context, system, user, schemaName string, schema json.RawMessage) (json.RawMessage, error) {
	if c.Model == "" {
		return nil, ErrNotConfigured
	}
	if !json.Valid(schema) {
		return nil, fmt.Errorf("%s: schema %q is not valid JSON", name, schemaName)
	}
	body, err := json.Marshal(request{
		Model: c.Model,
		Messages: []message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		ResponseFormat: responseFormat{
			Type:       "json_schema",
			JSONSchema: jsonSchema{Name: schemaName, Strict: true, Schema: schema},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var header http.Header
	if c.APIKey != "" {
		header = http.Header{"Authorization": {"Bearer " + c.APIKey}}
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := provider.PostJSON(ctx, c.HTTP, name, URL(c.BaseURL), header, body, c.Retries)
	if err != nil {
		return nil, err
	}
	out, err := content(raw)
	if err != nil {
		return nil, &provider.Error{Provider: name, Err: err}
	}
	return out, nil
}

// content extracts the first choice's content and checks it is a JSON object.
func content(raw []byte) (json.RawMessage, error) {
	var res response
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("unexpected response: %w", err)
	}
	if len(res.Choices) == 0 {
		return nil, errors.New("response has no choices")
	}
	ch := res.Choices[0]
	if r := ch.Message.Refusal; r != nil && strings.TrimSpace(*r) != "" {
		return nil, fmt.Errorf("model refused: %s", strings.TrimSpace(*r))
	}
	if ch.FinishReason == "length" {
		return nil, errors.New("answer was cut off (finish_reason length)")
	}
	if ch.Message.Content == nil {
		return nil, errors.New("response has no content")
	}
	text := strings.TrimSpace(*ch.Message.Content)
	// Some servers wrap JSON in a Markdown fence despite the schema.
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("content is not a JSON object: %.200q", text)
	}
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(text)); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// isOpenAI reports whether base is unset or OpenAI's own API host, the only
// case where OPENAI_API_KEY may be sent.
func isOpenAI(base string) bool {
	if strings.TrimSpace(base) == "" {
		return true
	}
	u, err := url.Parse(base)
	return err == nil && u.Scheme == "https" && u.Hostname() == "api.openai.com"
}
