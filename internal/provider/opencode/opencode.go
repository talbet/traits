// Package opencode registers the "opencode" provider: OpenCode Zen's System
// One gateway, which serves TypeSafe's Jev at https://opencode.ai/zen/v1/systemone.
//
// The free model (jev-1.13-free, the default) needs no key; jev-1.13 requires
// OPENCODE_API_KEY. Adapted from madxmike/gallant (MIT).
package opencode

import (
	"fmt"
	"slices"
	"strings"

	"github.com/talbet/traits/internal/provider"
	"github.com/talbet/traits/internal/provider/systemone"
)

// BaseURL is OpenCode Zen; the System One path is appended.
const BaseURL = "https://opencode.ai/zen"

// KeyEnv names the environment variable that holds the OpenCode Zen API key.
const KeyEnv = "OPENCODE_API_KEY"

// Models served by OpenCode Zen.
const (
	Jev113     = "jev-1.13"
	Jev113Free = "jev-1.13-free"
)

var models = []string{Jev113, Jev113Free}

func init() {
	provider.Register(provider.Provider{
		Name:    "opencode",
		KeyEnv:  KeyEnv,
		Models:  models,
		Default: Jev113Free,
		New:     New,
	})
}

// New builds an OpenCode Zen model. Any model but the free one requires an
// API key. cfg.BaseURL overrides BaseURL.
func New(cfg provider.Config) (provider.DecisionModel, error) {
	model := cfg.Model
	if model == "" {
		model = Jev113Free
	}
	if !slices.Contains(models, model) {
		return nil, fmt.Errorf("opencode: unknown model %q (want one of %s)", model, strings.Join(models, ", "))
	}
	if cfg.APIKey == "" && model != Jev113Free {
		return nil, fmt.Errorf("opencode: %s requires %s, which is not set", model, KeyEnv)
	}
	base := cfg.BaseURL
	if base == "" {
		base = BaseURL
	}
	return &systemone.Client{
		Provider: "opencode",
		URL:      strings.TrimRight(base, "/") + systemone.Endpoint,
		Header:   systemone.Bearer(cfg.APIKey),
		Model:    model,
		Timeout:  cfg.Timeout,
		Retries:  cfg.Retries,
	}, nil
}
