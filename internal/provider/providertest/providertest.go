// Package providertest serves recorded provider responses from httptest
// servers for adapter tests, so no test reaches the network.
package providertest

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/talbet/traits/internal/provider"
)

// Recorded is what a Server received.
type Recorded struct {
	mu     sync.Mutex
	Method string
	Path   string
	Header http.Header
	Body   []byte
	Calls  int
}

// Last returns a copy of the most recent request.
func (r *Recorded) Last() (method, path string, header http.Header, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Method, r.Path, r.Header, r.Body
}

// Server answers every request with status and the contents of
// testdata/<fixture> and records the request.
func Server(t *testing.T, status int, fixture string) (*httptest.Server, *Recorded) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	rec := &Recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.Method, rec.Path, rec.Header, rec.Body = r.Method, r.URL.Path, r.Header.Clone(), b
		rec.Calls++
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// ProviderError fails the test unless err is a *provider.Error.
func ProviderError(t *testing.T, err error) {
	t.Helper()
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %#v (%v), want *provider.Error", err, err)
	}
}
