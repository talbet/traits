// Package systemone implements the System One decision protocol and registers
// the "systemone" provider.
//
// A System One server answers a state against typed questions and returns
// typed answers with probabilities instead of generated text. Ollama,
// llama.cpp, Liquid, Strands Decider, TypeSafe and OpenCode Zen (Jev) serve it
// at POST {base}/v1/systemone. Cloudflare Workers AI and OpenRouter take the
// same body at their own endpoints, so their adapters reuse Client.
//
// Wire format: {"model", "state", "questions": {id: {type, instructions,
// criteria}}} with positional ids q0, q1, ... (question names may hold
// characters a server rejects in ids). Predicate is sent as "noul"; a choice's
// criteria is a map of option value to description; a score's criteria is the
// ordered list of level descriptions. Answers come back keyed by the same ids.
//
// The "systemone" provider reads its base URL from TRAITS_SYSTEMONE_URL
// (default http://127.0.0.1:11434, a local Ollama) and an optional bearer
// token from TRAITS_SYSTEMONE_API_KEY. Servers serve different models, so it
// declares none and has no default: the model comes from --model, or from
// TRAITS_SYSTEMONE_MODEL when the flag is not given, and New fails when
// neither is set.
package systemone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/talbet/traits/internal/provider"
)

// Endpoint is the path, appended to a server's base URL, that answers
// questions about a state.
const Endpoint = "/v1/systemone"

// DefaultURL is the base URL used when TRAITS_SYSTEMONE_URL is unset: a local
// Ollama server.
const DefaultURL = "http://127.0.0.1:11434"

// Environment variables read by the "systemone" provider.
const (
	URLEnv   = "TRAITS_SYSTEMONE_URL"
	KeyEnv   = "TRAITS_SYSTEMONE_API_KEY"
	ModelEnv = "TRAITS_SYSTEMONE_MODEL"
)

func init() {
	provider.Register(provider.Provider{
		Name:   "systemone",
		KeyEnv: KeyEnv,
		URLEnv: URLEnv,
		New:    New,
	})
}

// New builds a System One model from cfg. The API key is optional; the model
// is required (cfg.Model, else TRAITS_SYSTEMONE_MODEL).
func New(cfg provider.Config) (provider.DecisionModel, error) {
	model := cfg.Model
	if model == "" {
		model = os.Getenv(ModelEnv)
	}
	if model == "" {
		return nil, fmt.Errorf("systemone: no model; pass --model or set %s", ModelEnv)
	}
	base := cfg.BaseURL
	if base == "" {
		base = DefaultURL
	}
	return &Client{
		Provider: "systemone",
		URL:      strings.TrimRight(base, "/") + Endpoint,
		Header:   Bearer(cfg.APIKey),
		Model:    model,
		Timeout:  cfg.Timeout,
		Retries:  cfg.Retries,
	}, nil
}

// Bearer returns an Authorization header for key, or nil when key is empty.
func Bearer(key string) http.Header {
	if key == "" {
		return nil
	}
	return http.Header{"Authorization": {"Bearer " + key}}
}

// Client asks a System One endpoint its questions. Its fields configure the
// variations between servers that speak the protocol.
type Client struct {
	// Provider names the provider in errors.
	Provider string
	// URL is the full endpoint URL.
	URL string
	// Header is sent with every request (authorization, typically).
	Header http.Header
	// Model is sent in the body.
	Model string
	// Timeout bounds a request including retries; zero means 30s.
	Timeout time.Duration
	// Retries is how many times to retry 429 and 5xx responses.
	Retries int
	// MaxQuestions, when positive, rejects larger requests before sending.
	MaxQuestions int
	// StateJSON sends an object, array or string Input as a JSON value
	// rather than as its compact serialization in a string.
	StateJSON bool
	// Unwrap, when set, extracts the System One response from the body (for
	// a provider envelope). Its error is reported as a provider failure.
	Unwrap func([]byte) ([]byte, error)
	// HTTP is the client used; nil means provider.NewHTTPClient().
	HTTP *http.Client
}

// Limits implements provider.DecisionModel.
func (c *Client) Limits() provider.Limits {
	return provider.Limits{MaxQuestions: c.MaxQuestions}
}

// Decide implements provider.DecisionModel by asking every question in one
// request.
func (c *Client) Decide(ctx context.Context, req provider.Request) (provider.Response, error) {
	if err := provider.CheckQuestions(c.Provider, req, c.MaxQuestions); err != nil {
		return provider.Response{}, err
	}
	body, err := c.requestBody(req)
	if err != nil {
		return provider.Response{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, provider.Timeout(c.Timeout))
	defer cancel()
	raw, err := provider.PostJSON(ctx, c.HTTP, c.Provider, c.URL, c.Header, body, c.Retries)
	if err != nil {
		return provider.Response{}, err
	}
	if c.Unwrap != nil {
		if raw, err = c.Unwrap(raw); err != nil {
			return provider.Response{}, c.fail(err)
		}
	}

	var res response
	if err := json.Unmarshal(raw, &res); err != nil {
		return provider.Response{}, c.fail(fmt.Errorf("unexpected response: %w", err))
	}
	out := provider.Response{Model: res.Model, Answers: make([]provider.Answer, len(req.Questions))}
	if out.Model == "" {
		out.Model = c.Model
	}
	if res.Usage != nil {
		out.Usage.InputTokens = res.Usage.InputTokens
	}
	for i, q := range req.Questions {
		a, ok := res.Answers[QuestionID(i)]
		if !ok {
			// A question the server did not answer would otherwise look like a
			// clean pass, so report it.
			return provider.Response{}, c.fail(fmt.Errorf("no answer for question %q", q.Name))
		}
		ans, err := MapAnswer(q, a)
		if err != nil {
			return provider.Response{}, c.fail(err)
		}
		out.Answers[i] = ans
	}
	return out, nil
}

func (c *Client) fail(err error) error {
	return &provider.Error{Provider: c.Provider, Err: err}
}

// QuestionID names the question at index i on the wire.
// Adapted from madxmike/gallant (MIT).
func QuestionID(i int) string {
	return "q" + strconv.Itoa(i)
}

func (c *Client) requestBody(req provider.Request) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, req.Input); err != nil {
		return nil, fmt.Errorf("%s: input is not valid JSON: %w", c.Provider, err)
	}
	var state any = compact.String()
	if c.StateJSON {
		switch compact.Bytes()[0] {
		case '{', '[', '"':
			state = json.RawMessage(compact.Bytes())
		}
	}
	questions := make(map[string]Question, len(req.Questions))
	for i, q := range req.Questions {
		wq, err := WireQuestion(q)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Provider, err)
		}
		questions[QuestionID(i)] = wq
	}
	return json.Marshal(Request{Model: c.Model, State: state, Questions: questions})
}

// Request is the System One request body.
type Request struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Question is one System One question. Criteria is a map of option value to
// description (choice) or the ordered level descriptions (score).
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// WireQuestion maps q to its System One form. A level with no description is
// described by its value.
func WireQuestion(q provider.Question) (Question, error) {
	switch q.Type {
	case provider.Predicate:
		return Question{Type: "noul", Instructions: q.Instructions}, nil
	case provider.Choice:
		if len(q.Options) == 0 {
			return Question{}, fmt.Errorf("choice question %q has no options", q.Name)
		}
		criteria := make(map[string]string, len(q.Options))
		for _, o := range q.Options {
			criteria[o.Value] = o.Description
		}
		return Question{Type: "choice", Instructions: q.Instructions, Criteria: criteria}, nil
	case provider.Score:
		if len(q.Options) == 0 {
			return Question{}, fmt.Errorf("score question %q has no levels", q.Name)
		}
		levels := make([]string, len(q.Options))
		for i, o := range q.Options {
			levels[i] = o.Description
			if levels[i] == "" {
				levels[i] = o.Value
			}
		}
		return Question{Type: "score", Instructions: q.Instructions, Criteria: levels}, nil
	}
	return Question{}, fmt.Errorf("question %q has unsupported type %q", q.Name, q.Type)
}

type response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   *struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
}

// Answer is one System One answer. Noul is P(true); Choice and Score are the
// most likely option and level; Probabilities is the distribution, keyed by
// option value (choice) or level index (score).
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

var wireType = map[provider.QuestionType]string{
	provider.Predicate: "noul",
	provider.Choice:    "choice",
	provider.Score:     "score",
}

// MapAnswer converts the System One answer a to q. A refusal, a type
// mismatch, an unknown option or level, or an out-of-range probability is an
// error. Probabilities are rounded to 4 decimal places.
//
// For a score with a distribution, Value is the most probable level and Score
// is the probability-weighted level index; without one, the reported score is
// used for both.
func MapAnswer(q provider.Question, a Answer) (provider.Answer, error) {
	if a.Type == "refusal" {
		return provider.Answer{}, fmt.Errorf("refused question %q", q.Name)
	}
	if a.Type != "" && a.Type != wireType[q.Type] {
		return provider.Answer{}, fmt.Errorf("question %q: answer type %q, want %q", q.Name, a.Type, wireType[q.Type])
	}
	out := provider.Answer{Name: q.Name, Type: q.Type}
	switch q.Type {
	case provider.Predicate:
		if a.Noul == nil || !provider.ValidProbability(*a.Noul) {
			return provider.Answer{}, fmt.Errorf("question %q: missing or invalid noul", q.Name)
		}
		out.Probability = provider.RoundProbability(*a.Noul)
		return out, nil

	case provider.Choice:
		known := make(map[string]bool, len(q.Options))
		for _, o := range q.Options {
			known[o.Value] = true
		}
		if !known[a.Choice] {
			return provider.Answer{}, fmt.Errorf("question %q: choice %q is not an option", q.Name, a.Choice)
		}
		out.Value = a.Choice
		if a.Probabilities != nil {
			out.Probabilities = make(map[string]float64, len(a.Probabilities))
			for k, p := range a.Probabilities {
				if !known[k] {
					return provider.Answer{}, fmt.Errorf("question %q: probability for unknown option %q", q.Name, k)
				}
				if !provider.ValidProbability(p) {
					return provider.Answer{}, fmt.Errorf("question %q: invalid probability %v", q.Name, p)
				}
				out.Probabilities[k] = provider.RoundProbability(p)
			}
		}
		if err := setConfidence(&out, q, a.Confidence); err != nil {
			return provider.Answer{}, err
		}
		return out, nil

	case provider.Score:
		if a.Probabilities != nil {
			out.Probabilities = make(map[string]float64, len(a.Probabilities))
			best, bestP, sum, weighted := -1, -1.0, 0.0, 0.0
			for k, p := range a.Probabilities {
				i, err := strconv.Atoi(k)
				if err != nil || i < 0 || i >= len(q.Options) {
					return provider.Answer{}, fmt.Errorf("question %q: probability for unknown level %q", q.Name, k)
				}
				if !provider.ValidProbability(p) {
					return provider.Answer{}, fmt.Errorf("question %q: invalid probability %v", q.Name, p)
				}
				out.Probabilities[q.Options[i].Value] = provider.RoundProbability(p)
				sum += p
				weighted += float64(i) * p
				if p > bestP || (p == bestP && i < best) {
					best, bestP = i, p
				}
			}
			if best < 0 || sum == 0 {
				return provider.Answer{}, fmt.Errorf("question %q: empty score distribution", q.Name)
			}
			out.Value = q.Options[best].Value
			out.Score = provider.RoundProbability(weighted / sum)
		} else {
			if a.Score == nil {
				return provider.Answer{}, fmt.Errorf("question %q: missing score", q.Name)
			}
			i := int(*a.Score + 0.5)
			if *a.Score < 0 || i >= len(q.Options) {
				return provider.Answer{}, fmt.Errorf("question %q: score %v is out of range", q.Name, *a.Score)
			}
			out.Value = q.Options[i].Value
			out.Score = *a.Score
		}
		if err := setConfidence(&out, q, a.Confidence); err != nil {
			return provider.Answer{}, err
		}
		return out, nil
	}
	return provider.Answer{}, fmt.Errorf("question %q has unsupported type %q", q.Name, q.Type)
}

// setConfidence records the reported confidence, or the probability of the
// chosen value when none is reported.
func setConfidence(out *provider.Answer, q provider.Question, c *float64) error {
	if c == nil {
		out.Confidence = out.Probabilities[out.Value]
		return nil
	}
	if !provider.ValidProbability(*c) {
		return fmt.Errorf("question %q: invalid confidence %v", q.Name, *c)
	}
	out.Confidence = provider.RoundProbability(*c)
	return nil
}
