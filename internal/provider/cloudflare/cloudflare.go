// Package cloudflare registers the "cloudflare" provider: Cloudflare Workers
// AI, which serves the Clef decision models.
//
// Workers AI takes the System One request body at a per-model endpoint under
// the account (POST {base}/accounts/{account}/ai/run/@cf/cloudflare/{model})
// and wraps the answers in Cloudflare's {"result", "success", "errors"}
// envelope. It accepts at most 64 questions per request.
// Adapted from madxmike/gallant (MIT).
package cloudflare

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/talbet/traits/internal/provider"
	"github.com/talbet/traits/internal/provider/systemone"
)

// BaseURL is the Cloudflare API; the account and model are appended.
const BaseURL = "https://api.cloudflare.com/client/v4"

// Environment variables read by the provider.
const (
	KeyEnv     = "CLOUDFLARE_API_TOKEN"
	AccountEnv = "CLOUDFLARE_ACCOUNT_ID"
)

// MaxQuestions is the most questions Workers AI accepts in one request.
const MaxQuestions = 64

// Models served by Workers AI.
const (
	Clef      = "clef"
	ClefFlash = "clef-flash"
)

var models = []string{Clef, ClefFlash}

func init() {
	provider.Register(provider.Provider{
		Name:       "cloudflare",
		KeyEnv:     KeyEnv,
		AccountEnv: AccountEnv,
		Models:     models,
		Default:    Clef,
		New:        New,
	})
}

// New builds a Workers AI model. The API token and account id are required,
// so a missing one is reported before any request. cfg.BaseURL overrides
// BaseURL.
func New(cfg provider.Config) (provider.DecisionModel, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("cloudflare: %s is not set", KeyEnv)
	}
	if cfg.Account == "" {
		return nil, fmt.Errorf("cloudflare: %s is not set", AccountEnv)
	}
	model := cfg.Model
	if model == "" {
		model = Clef
	}
	if !slices.Contains(models, model) {
		return nil, fmt.Errorf("cloudflare: unknown model %q (want one of %s)", model, strings.Join(models, ", "))
	}
	base := cfg.BaseURL
	if base == "" {
		base = BaseURL
	}
	u := strings.TrimRight(base, "/") + "/accounts/" + url.PathEscape(cfg.Account) +
		"/ai/run/@cf/cloudflare/" + url.PathEscape(model)
	return &systemone.Client{
		Provider:     "cloudflare",
		URL:          u,
		Header:       systemone.Bearer(cfg.APIKey),
		Model:        model,
		Timeout:      cfg.Timeout,
		Retries:      cfg.Retries,
		MaxQuestions: MaxQuestions,
		Unwrap:       unwrap,
	}, nil
}

// envelope is the wrapper the Cloudflare API puts around every response.
type envelope struct {
	Result  json.RawMessage `json:"result"`
	Success bool            `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// unwrap returns the result of a successful 2xx response, or an error carrying
// Cloudflare's own messages. Non-2xx responses are reported by
// provider.PostJSON, whose body snippet carries the same messages.
func unwrap(raw []byte) ([]byte, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("unexpected response: %w", err)
	}
	if !env.Success {
		if len(env.Errors) == 0 {
			return nil, errors.New("request failed without an error message")
		}
		msgs := make([]string, len(env.Errors))
		for i, e := range env.Errors {
			msgs[i] = fmt.Sprintf("%s (code %d)", e.Message, e.Code)
		}
		return nil, errors.New(strings.Join(msgs, "; "))
	}
	if len(env.Result) == 0 || string(env.Result) == "null" {
		return nil, errors.New("response has no result")
	}
	return env.Result, nil
}
