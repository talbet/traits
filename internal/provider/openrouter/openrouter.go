// Package openrouter registers the "openrouter" provider: OpenRouter's alpha
// Decisions API, which routes System One requests to decision models such as
// TypeSafe's Jev.
//
// Wire format: POST {base}/alpha/decisions (default base
// https://openrouter.ai/api) with a bearer OPENROUTER_API_KEY and the System
// One body {"model", "state", "questions": {id: {type, instructions,
// criteria}}}. Unlike /v1/systemone, state is sent as a JSON value. Answers
// are keyed by id; choice probabilities by option value and score
// probabilities by level index. A {"type": "refusal"} answer is an error.
//
// The endpoint, body and noul answers are confirmed by OpenRouter's docs; the
// choice and score answer shapes come from OpenRouter's SDK types and
// @effect/ai-openrouter 4.1.2, not from a recorded response. See CONTRACT.md.
package openrouter

import (
	"fmt"
	"strings"

	"github.com/talbet/traits/internal/provider"
	"github.com/talbet/traits/internal/provider/systemone"
)

// BaseURL is OpenRouter's API root; the alpha path is appended.
const BaseURL = "https://openrouter.ai/api"

// Endpoint is the Decisions path under BaseURL.
const Endpoint = "/alpha/decisions"

// Environment variables read by the provider.
const (
	KeyEnv = "OPENROUTER_API_KEY"
	URLEnv = "OPENROUTER_BASE_URL"
)

// DefaultModel is the model traits.sh uses.
const DefaultModel = "~typesafe/jev-latest"

func init() {
	provider.Register(provider.Provider{
		Name:    "openrouter",
		KeyEnv:  KeyEnv,
		URLEnv:  URLEnv,
		Default: DefaultModel,
		New:     New,
	})
}

// New builds an OpenRouter decision model. The API key is required. A base
// URL ending in /v1 (the chat API root) has that suffix replaced, as
// @effect/ai-openrouter does.
func New(cfg provider.Config) (provider.DecisionModel, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("openrouter: %s is not set", KeyEnv)
	}
	model := cfg.Model
	if model == "" {
		model = DefaultModel
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = BaseURL
	}
	base = strings.TrimSuffix(base, "/v1")
	return &systemone.Client{
		Provider:  "openrouter",
		URL:       base + Endpoint,
		Header:    systemone.Bearer(cfg.APIKey),
		Model:     model,
		Timeout:   cfg.Timeout,
		Retries:   cfg.Retries,
		StateJSON: true,
	}, nil
}
