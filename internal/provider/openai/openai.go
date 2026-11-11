// Package openai registers the "openai" provider: the OpenAI Decisions API
// (POST {base}/v1/decisions) with the gpt-6-luna model.
//
// The request carries the input as one user message whose content is a single
// input_json part, and the questions as an ordered array, each with its name.
// Answers come back in question order and echo the name; an answer is matched
// by name when the name is present and by position otherwise, and a mismatch
// is an error. A refusal is an error. See CONTRACT.md for which wire details
// are confirmed by OpenAI's reference and which are assumed.
package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/talbet/traits/internal/provider"
)

// BaseURL is the default API root; Endpoint is appended.
const BaseURL = "https://api.openai.com"

// Endpoint is the Decisions path.
const Endpoint = "/v1/decisions"

// Environment variables read by the provider.
const (
	KeyEnv = "OPENAI_API_KEY"
	URLEnv = "OPENAI_BASE_URL"
)

// Luna is the only model the Decisions API serves.
const Luna = "gpt-6-luna"

// MaxQuestions is the most questions one request may carry.
const MaxQuestions = 400

// MaxInputBytes is the documented size limit of an input_json part (256 KiB).
const MaxInputBytes = 256 << 10

// maxCacheKey is the documented length limit of prompt_cache_key.
const maxCacheKey = 64

func init() {
	provider.Register(provider.Provider{
		Name:    "openai",
		KeyEnv:  KeyEnv,
		URLEnv:  URLEnv,
		Models:  []string{Luna},
		Default: Luna,
		New:     New,
	})
}

// New builds a Decisions model. The API key is required; the model must be
// gpt-6-luna (or empty, meaning gpt-6-luna). cfg.BaseURL overrides BaseURL.
func New(cfg provider.Config) (provider.DecisionModel, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("openai: %s is not set", KeyEnv)
	}
	model := cfg.Model
	if model == "" {
		model = Luna
	}
	if model != Luna {
		return nil, fmt.Errorf("openai: unknown model %q (the Decisions API serves only %s)", model, Luna)
	}
	base := cfg.BaseURL
	if base == "" {
		base = BaseURL
	}
	return &Model{
		url:     strings.TrimRight(base, "/") + Endpoint,
		key:     cfg.APIKey,
		model:   model,
		timeout: cfg.Timeout,
		retries: cfg.Retries,
		client:  provider.NewHTTPClient(),
	}, nil
}

// Model is a Decisions API model.
type Model struct {
	url     string
	key     string
	model   string
	timeout time.Duration
	retries int
	client  *http.Client
}

// Limits implements provider.DecisionModel.
func (m *Model) Limits() provider.Limits {
	return provider.Limits{MaxQuestions: MaxQuestions, MaxInputBytes: MaxInputBytes}
}

// Decide implements provider.DecisionModel by asking every question in one
// request.
func (m *Model) Decide(ctx context.Context, req provider.Request) (provider.Response, error) {
	if err := provider.CheckQuestions("openai", req, MaxQuestions); err != nil {
		return provider.Response{}, err
	}
	body, err := m.requestBody(req)
	if err != nil {
		return provider.Response{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, provider.Timeout(m.timeout))
	defer cancel()
	header := http.Header{"Authorization": {"Bearer " + m.key}}
	raw, err := provider.PostJSON(ctx, m.client, "openai", m.url, header, body, m.retries)
	if err != nil {
		return provider.Response{}, err
	}
	res, err := parseResponse(raw, req.Questions)
	if err != nil {
		return provider.Response{}, &provider.Error{Provider: "openai", Err: err}
	}
	return res, nil
}

type request struct {
	Model          string     `json:"model"`
	Input          []message  `json:"input"`
	Questions      []question `json:"questions"`
	PromptCacheKey string     `json:"prompt_cache_key,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content []part `json:"content"`
}

type part struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type question struct {
	Type         string   `json:"type"`
	Name         string   `json:"name"`
	Instructions string   `json:"instructions"`
	Choices      []choice `json:"choices,omitempty"`
	Levels       []level  `json:"levels,omitempty"`
}

type choice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type level struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

func (m *Model) requestBody(req provider.Request) ([]byte, error) {
	var input bytes.Buffer
	if err := json.Compact(&input, req.Input); err != nil {
		return nil, fmt.Errorf("openai: input is not valid JSON: %w", err)
	}
	if input.Len() > MaxInputBytes {
		return nil, fmt.Errorf("openai: input is %d bytes, more than the %d an input_json part may carry", input.Len(), MaxInputBytes)
	}
	qs := make([]question, len(req.Questions))
	for i, q := range req.Questions {
		wq := question{Type: string(q.Type), Name: q.Name, Instructions: q.Instructions}
		switch q.Type {
		case provider.Predicate:
		case provider.Choice:
			for _, o := range q.Options {
				wq.Choices = append(wq.Choices, choice{Value: o.Value, Description: o.Description})
			}
		case provider.Score:
			for _, o := range q.Options {
				wq.Levels = append(wq.Levels, level{Label: o.Value, Description: o.Description})
			}
		default:
			return nil, fmt.Errorf("openai: question %q has unsupported type %q", q.Name, q.Type)
		}
		qs[i] = wq
	}
	return json.Marshal(request{
		Model:          m.model,
		Input:          []message{{Role: "user", Content: []part{{Type: "input_json", Data: input.Bytes()}}}},
		Questions:      qs,
		PromptCacheKey: CacheKey(req.CacheKey),
	})
}

// CacheKey returns key as prompt_cache_key: unchanged when it fits the
// documented 64-character limit, otherwise its SHA-256 in hex (64 characters).
func CacheKey(key string) string {
	if len(key) <= maxCacheKey {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

type response struct {
	Model   string   `json:"model"`
	Answers []answer `json:"answers"`
	Usage   *struct {
		InputTokens        int `json:"input_tokens"`
		InputTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

type answer struct {
	Type          string          `json:"type"`
	Name          *string         `json:"name"`
	Probability   *float64        `json:"probability"`
	Choice        json.RawMessage `json:"choice"`
	Score         *float64        `json:"score"`
	Confidence    *float64        `json:"confidence"`
	Probabilities json.RawMessage `json:"probabilities"`
}

func parseResponse(raw []byte, qs []provider.Question) (provider.Response, error) {
	var res response
	if err := json.Unmarshal(raw, &res); err != nil {
		return provider.Response{}, fmt.Errorf("unexpected response: %w", err)
	}
	if res.Answers == nil {
		return provider.Response{}, errors.New("unexpected response: no answers")
	}
	if len(res.Answers) != len(qs) {
		return provider.Response{}, fmt.Errorf("%d answers for %d questions", len(res.Answers), len(qs))
	}
	byName := make(map[string]int, len(res.Answers))
	for i, a := range res.Answers {
		if a.Name == nil {
			continue
		}
		if _, dup := byName[*a.Name]; dup {
			return provider.Response{}, fmt.Errorf("two answers named %q", *a.Name)
		}
		byName[*a.Name] = i
	}

	out := provider.Response{Model: res.Model, Answers: make([]provider.Answer, len(qs))}
	if res.Usage != nil {
		out.Usage.InputTokens = res.Usage.InputTokens
		if d := res.Usage.InputTokensDetails; d != nil {
			out.Usage.CachedTokens = d.CachedTokens
		}
	}
	for i, q := range qs {
		j, ok := byName[q.Name]
		if !ok {
			// Fall back to position only for an unnamed answer, so a
			// probability is never attached to the wrong question.
			if res.Answers[i].Name != nil {
				return provider.Response{}, fmt.Errorf("answer %d is named %q, want %q", i, *res.Answers[i].Name, q.Name)
			}
			j = i
		}
		a, err := mapAnswer(q, res.Answers[j])
		if err != nil {
			return provider.Response{}, err
		}
		out.Answers[i] = a
	}
	return out, nil
}

func mapAnswer(q provider.Question, a answer) (provider.Answer, error) {
	if a.Type == "refusal" {
		return provider.Answer{}, fmt.Errorf("refused question %q", q.Name)
	}
	if a.Type != string(q.Type) {
		return provider.Answer{}, fmt.Errorf("question %q: answer type %q, want %q", q.Name, a.Type, q.Type)
	}
	out := provider.Answer{Name: q.Name, Type: q.Type}
	switch q.Type {
	case provider.Predicate:
		if a.Probability == nil || !provider.ValidProbability(*a.Probability) {
			return provider.Answer{}, fmt.Errorf("question %q: missing or invalid probability", q.Name)
		}
		out.Probability = provider.RoundProbability(*a.Probability)
		return out, nil

	case provider.Choice:
		v, err := scalar(a.Choice)
		if err != nil {
			return provider.Answer{}, fmt.Errorf("question %q: choice: %w", q.Name, err)
		}
		if optionIndex(q, v) < 0 {
			return provider.Answer{}, fmt.Errorf("question %q: choice %q is not an option", q.Name, v)
		}
		out.Value = v
		if out.Probabilities, err = distribution(q, a.Probabilities, false); err != nil {
			return provider.Answer{}, fmt.Errorf("question %q: probabilities: %w", q.Name, err)
		}

	case provider.Score:
		var err error
		if out.Probabilities, err = distribution(q, a.Probabilities, true); err != nil {
			return provider.Answer{}, fmt.Errorf("question %q: probabilities: %w", q.Name, err)
		}
		switch {
		case a.Score != nil:
			if *a.Score < 0 || *a.Score > float64(len(q.Options)-1) {
				return provider.Answer{}, fmt.Errorf("question %q: score %v is out of range", q.Name, *a.Score)
			}
			out.Score = *a.Score
		case out.Probabilities != nil:
			for i, o := range q.Options {
				out.Score += float64(i) * out.Probabilities[o.Value]
			}
			out.Score = provider.RoundProbability(out.Score)
		default:
			return provider.Answer{}, fmt.Errorf("question %q: missing score", q.Name)
		}
		// Value is the most probable level; without a distribution, the
		// level nearest the score.
		if out.Probabilities != nil {
			best := -1.0
			for _, o := range q.Options {
				if p, ok := out.Probabilities[o.Value]; ok && p > best {
					out.Value, best = o.Value, p
				}
			}
		} else {
			out.Value = q.Options[int(out.Score+0.5)].Value
		}

	default:
		return provider.Answer{}, fmt.Errorf("question %q has unsupported type %q", q.Name, q.Type)
	}

	if a.Confidence != nil {
		if !provider.ValidProbability(*a.Confidence) {
			return provider.Answer{}, fmt.Errorf("question %q: invalid confidence %v", q.Name, *a.Confidence)
		}
		out.Confidence = provider.RoundProbability(*a.Confidence)
	} else {
		out.Confidence = out.Probabilities[out.Value]
	}
	return out, nil
}

// scalar decodes a choice value, which the API allows to be a string or a
// boolean, as a string.
func scalar(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return strconv.FormatBool(b), nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String(), nil
	}
	return "", fmt.Errorf("want a string or boolean, got %s", truncate(raw))
}

func optionIndex(q provider.Question, v string) int {
	for i, o := range q.Options {
		if o.Value == v {
			return i
		}
	}
	return -1
}

// entry is one element of a probabilities array. Choice entries carry value;
// score entries carry label and value (the 0-based level index).
type entry struct {
	Value       json.RawMessage `json:"value"`
	Label       *string         `json:"label"`
	Probability *float64        `json:"probability"`
}

// distribution decodes a choice or score probabilities field into a map keyed
// by option value. It accepts the documented array of entries, and also an
// object mapping option value, level label or level index to probability. A
// key that names no option, a duplicate, or an invalid probability is an
// error. An absent or null field yields nil.
func distribution(q provider.Question, raw json.RawMessage, score bool) (map[string]float64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	out := map[string]float64{}
	add := func(key string, index *int, p *float64) error {
		i := optionIndex(q, key)
		if i < 0 && score && index != nil {
			i = *index
		}
		if i < 0 || i >= len(q.Options) {
			return fmt.Errorf("unknown option %q", key)
		}
		if p == nil || !provider.ValidProbability(*p) {
			return fmt.Errorf("missing or invalid probability for %q", key)
		}
		v := q.Options[i].Value
		if _, dup := out[v]; dup {
			return fmt.Errorf("two probabilities for %q", v)
		}
		out[v] = provider.RoundProbability(*p)
		return nil
	}

	switch raw[0] {
	case '[':
		var entries []entry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, err
		}
		for _, e := range entries {
			var key string
			var index *int
			if score {
				if e.Label != nil {
					key = *e.Label
				}
				var n int
				if err := json.Unmarshal(e.Value, &n); err == nil {
					index = &n
					if key == "" {
						key = strconv.Itoa(n)
					}
				}
			} else {
				var err error
				if key, err = scalar(e.Value); err != nil {
					return nil, err
				}
			}
			if err := add(key, index, e.Probability); err != nil {
				return nil, err
			}
		}
	case '{':
		var m map[string]float64
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		for k, p := range m {
			var index *int
			if n, err := strconv.Atoi(k); err == nil {
				index = &n
			}
			if err := add(k, index, &p); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("want an array or object, got %s", truncate(raw))
	}
	return out, nil
}

func truncate(raw []byte) string {
	if len(raw) > 40 {
		return string(raw[:40]) + "..."
	}
	return string(raw)
}
