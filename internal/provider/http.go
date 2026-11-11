package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxResponseBytes bounds how much of a response body PostJSON reads, so a
// misbehaving endpoint cannot exhaust memory.
const MaxResponseBytes = 4 << 20

// DefaultTimeout bounds a request, including retries, when Config.Timeout is
// zero.
const DefaultTimeout = 30 * time.Second

// firstBackoff is the wait before the first retry when the server sends no
// Retry-After; each later retry doubles it.
var firstBackoff = 500 * time.Millisecond

// NewHTTPClient returns a client for provider requests. It has no timeout of
// its own (each request is bounded by its context) and refuses to follow a
// redirect to a different host, so credentials never leave the configured
// server.
func NewHTTPClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("refusing redirect to another host (%s)", req.URL.Host)
			}
			return nil
		},
	}
}

// HTTPError is a non-2xx response. Snippet is a short, single-line excerpt of
// the body, so the server's own explanation reaches the user.
type HTTPError struct {
	StatusCode int
	Status     string
	Snippet    string
	// RetryAfter is the server's Retry-After hint in seconds, or zero.
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	if e.Snippet == "" {
		return e.Status
	}
	return e.Status + ": " + e.Snippet
}

// PostJSON posts body to url and returns the response body. It retries up to
// retries times on 429 and 5xx responses, waiting for Retry-After (seconds)
// when the server sends it and otherwise 500ms doubling per attempt, and never
// waits past ctx's deadline. Every failure is a *Error naming name and url.
func PostJSON(ctx context.Context, client *http.Client, name, url string, header http.Header, body []byte, retries int) ([]byte, error) {
	if client == nil {
		client = NewHTTPClient()
	}
	fail := func(err error) error {
		return &Error{Provider: name, Err: fmt.Errorf("%s: %w", url, err)}
	}
	for attempt := 0; ; attempt++ {
		raw, err := postOnce(ctx, client, url, header, body)
		if err == nil {
			return raw, nil
		}
		var he *HTTPError
		if !errors.As(err, &he) || !retryable(he.StatusCode) || attempt >= retries {
			return nil, fail(err)
		}
		wait := he.RetryAfter
		if wait <= 0 {
			wait = firstBackoff << attempt
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait {
			// Waiting would outlast the deadline; report the response we have.
			return nil, fail(err)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fail(err)
		case <-t.C:
		}
	}
}

func postOnce(ctx context.Context, client *http.Client, url string, header http.Header, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, requestCause(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Snippet:    snippet(resp.Body),
			RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
		}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", requestCause(err))
	}
	if len(raw) > MaxResponseBytes {
		return nil, fmt.Errorf("response is larger than %d bytes", MaxResponseBytes)
	}
	return raw, nil
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// retryAfter parses a Retry-After header given in seconds. An HTTP date or a
// malformed value yields zero, which falls back to exponential backoff.
func retryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// requestCause strips the method-and-URL wrapper net/http puts around a
// transport failure, so the diagnostic does not name the URL twice.
// Adapted from madxmike/gallant (MIT).
func requestCause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// snippet reads a short, single-line excerpt of an error body, bounded so a
// large or HTML error page cannot flood the terminal.
// Adapted from madxmike/gallant (MIT).
func snippet(body io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(body, 512))
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// Timeout returns d, or DefaultTimeout when d is zero or negative.
func Timeout(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultTimeout
	}
	return d
}

// RoundProbability trims binary floating-point noise from a reported or
// derived probability, so 0.9400000000000001 reads as 0.94.
// Adapted from madxmike/gallant (MIT).
func RoundProbability(p float64) float64 {
	return math.Round(p*1e4) / 1e4
}

// CheckQuestions reports a request with no questions, or with more than max
// questions when max is positive. The error is about the request, not the
// provider, so it is not a *Error.
func CheckQuestions(name string, req Request, max int) error {
	if len(req.Questions) == 0 {
		return fmt.Errorf("%s: request has no questions", name)
	}
	if max > 0 && len(req.Questions) > max {
		return fmt.Errorf("%s: %d questions, more than the %d one request may carry", name, len(req.Questions), max)
	}
	return nil
}

// ValidProbability reports whether p is a finite number in [0, 1].
func ValidProbability(p float64) bool {
	return !math.IsNaN(p) && p >= 0 && p <= 1
}
