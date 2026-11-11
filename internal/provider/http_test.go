package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func init() { firstBackoff = time.Millisecond }

// server answers each request with the next status in statuses (the last one
// repeats) and counts the calls.
func server(t *testing.T, calls *atomic.Int32, statuses []int, retryAfter, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1)) - 1
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Test") != "yes" {
			t.Errorf("headers = %v", r.Header)
		}
		status := statuses[min(n, len(statuses)-1)]
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

var testHeader = http.Header{"X-Test": {"yes"}}

func TestPostJSONRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := server(t, &calls, []int{503, 429, 200}, "", `{"ok":true}`)
	raw, err := PostJSON(context.Background(), nil, "p", srv.URL, testHeader, []byte(`{}`), 2)
	if err != nil {
		t.Fatalf("PostJSON() error = %v", err)
	}
	if string(raw) != `{"ok":true}` || calls.Load() != 3 {
		t.Errorf("raw, calls = %s, %d", raw, calls.Load())
	}
}

func TestPostJSONErrors(t *testing.T) {
	tests := []struct {
		name      string
		statuses  []int
		retries   int
		body      string
		wantCalls int32
		want      string
	}{
		{name: "no retries", statuses: []int{429}, body: `{"error":"slow down"}`, wantCalls: 1, want: `429 Too Many Requests: {"error":"slow down"}`},
		{name: "retries exhausted", statuses: []int{500}, retries: 2, wantCalls: 3, want: "500 Internal Server Error"},
		{name: "4xx is not retried", statuses: []int{400}, retries: 3, body: "bad\n  request", wantCalls: 1, want: "400 Bad Request: bad request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := server(t, &calls, tt.statuses, "", tt.body)
			_, err := PostJSON(context.Background(), nil, "p", srv.URL, testHeader, nil, tt.retries)
			var pe *Error
			if !errors.As(err, &pe) || pe.Provider != "p" {
				t.Fatalf("error = %#v, want *Error for p", err)
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.HasPrefix(err.Error(), "p: "+srv.URL+": ") {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
			if calls.Load() != tt.wantCalls {
				t.Errorf("calls = %d, want %d", calls.Load(), tt.wantCalls)
			}
		})
	}
}

// A Retry-After longer than the remaining deadline stops retrying at once.
func TestPostJSONRetryAfterBeyondDeadline(t *testing.T) {
	var calls atomic.Int32
	srv := server(t, &calls, []int{429}, "60", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := PostJSON(ctx, nil, "p", srv.URL, testHeader, nil, 5)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("error = %v, want the 429", err)
	}
	if time.Since(start) > time.Second || calls.Load() != 1 {
		t.Errorf("took %v over %d calls; want one prompt call", time.Since(start), calls.Load())
	}
}

func TestPostJSONSnippetIsBounded(t *testing.T) {
	var calls atomic.Int32
	srv := server(t, &calls, []int{502}, "", strings.Repeat("<html>\n", 200))
	_, err := PostJSON(context.Background(), nil, "p", srv.URL, testHeader, nil, 0)
	msg := err.Error()
	if strings.Contains(msg, "\n") || !strings.HasSuffix(msg, "...") {
		t.Errorf("error = %q, want a single-line truncated snippet", msg)
	}
	if i := strings.Index(msg, "Gateway: "); i < 0 || len(msg)-i-len("Gateway: ") != 203 {
		t.Errorf("snippet length wrong in %q", msg)
	}
}

func TestPostJSONResponseTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, MaxResponseBytes+1))
	}))
	defer srv.Close()
	_, err := PostJSON(context.Background(), nil, "p", srv.URL, nil, nil, 0)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("error = %v, want a size error", err)
	}
}

func TestPostJSONRefusesCrossHostRedirect(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect to another host was followed")
	}))
	defer other.Close()
	// 127.0.0.1 and localhost are different hosts to the redirect check.
	target := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := PostJSON(context.Background(), nil, "p", srv.URL, nil, []byte(`{}`), 0)
	if err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("error = %v, want a refused redirect", err)
	}
	if strings.Count(err.Error(), srv.URL) != 1 {
		t.Errorf("error = %q names the URL more than once", err)
	}
}

func TestPostJSONHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := PostJSON(ctx, nil, "p", srv.URL, nil, nil, 3)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
}

func TestHelpers(t *testing.T) {
	if got := RoundProbability(0.9400000000000001); got != 0.94 {
		t.Errorf("RoundProbability = %v", got)
	}
	if Timeout(0) != DefaultTimeout || Timeout(time.Second) != time.Second {
		t.Error("Timeout default wrong")
	}
	if err := CheckQuestions("p", Request{}, 0); err == nil {
		t.Error("CheckQuestions accepted no questions")
	}
	if err := CheckQuestions("p", Request{Questions: make([]Question, 3)}, 2); err == nil {
		t.Error("CheckQuestions accepted too many questions")
	}
	if ValidProbability(1.1) || ValidProbability(-0.1) || !ValidProbability(0) {
		t.Error("ValidProbability wrong")
	}
}
