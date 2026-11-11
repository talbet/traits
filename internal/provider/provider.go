// Package provider defines the provider-neutral decision request and response
// shapes, and a registry of decision-model providers. Each adapter package
// (openai, systemone, cloudflare, openrouter) maps these to its wire format and
// registers itself from init.
package provider

import (
	"context"
	"encoding/json"
	"slices"
	"time"
)

// QuestionType is the kind of answer a question asks for.
type QuestionType string

const (
	// Predicate asks for the probability that a statement is true.
	// System One calls this "noul".
	Predicate QuestionType = "predicate"
	// Choice asks which one of a fixed set of options applies.
	Choice QuestionType = "choice"
	// Score asks which of an ordered list of levels applies.
	Score QuestionType = "score"
)

// Option is one choice of a Choice question or one level of a Score question.
type Option struct {
	Value       string
	Description string
}

// Question is one typed question about the request's shared input.
type Question struct {
	// Name is unique within a request. Adapters may send positional ids on
	// the wire but must map answers back to Name.
	Name         string
	Type         QuestionType
	Instructions string
	// Options lists choices (Choice) or levels in ascending order (Score).
	// It is empty for Predicate.
	Options []Option
}

// Request is one decision call: shared input and the questions asked about it.
type Request struct {
	// Input is the shared evidence. It is a JSON value; adapters that only
	// accept text send its compact serialization.
	Input     json.RawMessage
	Questions []Question
	// CacheKey partitions provider-side prompt caching. Optional.
	CacheKey string
}

// Answer is the provider's answer to one question.
type Answer struct {
	Name string
	Type QuestionType
	// Probability is set for Predicate: P(true).
	Probability float64
	// Value is the most likely option (Choice) or level label (Score).
	Value string
	// Score is the probability-weighted level index (Score only), 0-based.
	Score float64
	// Confidence is the provider's confidence in Value, when reported.
	Confidence float64
	// Probabilities maps every option value or level label to its
	// probability, when the provider reports a distribution.
	Probabilities map[string]float64
}

// Usage is token accounting reported by the provider, when available.
type Usage struct {
	InputTokens  int
	CachedTokens int
}

// Response holds one answer per question, in the order the questions were
// asked.
type Response struct {
	Model   string
	Answers []Answer
	Usage   Usage
}

// Limits describes what one request to a provider may carry. Zero means no
// declared limit.
type Limits struct {
	MaxQuestions  int
	MaxInputBytes int
	// Serial is true when the provider can only handle one request at a time.
	Serial bool
}

// DecisionModel answers typed questions about an input.
//
// Decide must return an answer for every question, in order, or an error. A
// refusal, a missing answer, or a malformed answer is an error, never a
// default value. Decide must return when ctx is done.
type DecisionModel interface {
	Decide(ctx context.Context, req Request) (Response, error)
	Limits() Limits
}

// Error reports a failure of the provider itself (unreachable, rejected
// request, malformed response, refusal) as opposed to a problem with one
// input. A run reports it once rather than per file.
type Error struct {
	Provider string
	Err      error
}

func (e *Error) Error() string { return e.Provider + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Config is what a provider needs to build a model. Values come from flags or
// the provider's environment variables.
type Config struct {
	APIKey  string
	BaseURL string
	Model   string
	// Account is a provider-specific account id (Cloudflare).
	Account string
	// Timeout bounds each request including retries. Zero means 30s.
	Timeout time.Duration
	// Retries is how many times to retry 429 and 5xx responses (honouring
	// Retry-After) within Timeout. Zero means no retries (hook mode).
	Retries int
}

// Provider describes a provider and how to construct its models.
type Provider struct {
	Name string
	// KeyEnv, URLEnv and AccountEnv name the environment variables read into
	// Config. Empty means unused.
	KeyEnv     string
	URLEnv     string
	AccountEnv string
	// Models are the declared model names; empty means any name is accepted.
	Models  []string
	Default string
	// New builds a model. It returns an error for an unmet requirement such as
	// a missing API key, so the problem is reported before any request.
	New func(Config) (DecisionModel, error)
}

var registry = map[string]Provider{}

// Register adds p to the registry. Adapter packages call it from init.
func Register(p Provider) { registry[p.Name] = p }

// Lookup returns the provider registered under name.
func Lookup(name string) (Provider, bool) {
	p, ok := registry[name]
	return p, ok
}

// Names returns the registered provider names, sorted.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
