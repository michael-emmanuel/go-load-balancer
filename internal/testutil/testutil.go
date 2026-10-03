// Package testutil provides controllable fake backends for tests.
package testutil

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// NewLogger returns a logger that discards output.
func NewLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Eventually polls cond until it returns true or a generous deadline passes.
// It exists only for assertions about background goroutines; the deadline is
// a failure bound, not an expected wait.
func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met in time: %s", what)
}

// EchoResponse is the JSON body returned by a Backend for any path other than
// /health, /slow and /status/N.
type EchoResponse struct {
	Backend string      `json:"backend"`
	Method  string      `json:"method"`
	Path    string      `json:"path"`
	Query   string      `json:"query"`
	Header  http.Header `json:"header"`
	Body    string      `json:"body"`
}

// Backend is a fake upstream whose health, availability and speed are
// controlled by the test.
//
// Endpoints:
//
//	/health     200 or 503 depending on SetHealthy; blocks while SetSlowHealth(true)
//	/slow       blocks until OpenGate (or the client disconnects)
//	/status/N   responds with status N
//	anything    JSON echo of the request, plus header X-Backend
type Backend struct {
	Name string
	// Entered receives one value each time a request reaches /slow, so tests
	// can wait for "request is in flight" without sleeping.
	Entered chan struct{}

	healthy    atomic.Bool
	slowHealth atomic.Bool
	hits       atomic.Int64
	gate       chan struct{}
	gateOnce   sync.Once

	mu   sync.Mutex
	srv  *httptest.Server
	addr string
}

// NewBackend starts a fake backend that is cleaned up with the test.
func NewBackend(t testing.TB, name string) *Backend {
	t.Helper()
	b := &Backend{Name: name, Entered: make(chan struct{}, 64), gate: make(chan struct{})}
	b.healthy.Store(true)
	b.srv = httptest.NewServer(http.HandlerFunc(b.serve))
	b.addr = b.srv.Listener.Addr().String()
	t.Cleanup(func() {
		b.OpenGate() // release anything blocked so Close cannot hang
		b.Stop()
	})
	return b
}

// URL returns the backend base URL.
func (b *Backend) URL() string { return "http://" + b.addr }

// Hits returns the number of non-health requests served.
func (b *Backend) Hits() int64 { return b.hits.Load() }

// SetHealthy controls the status code returned by /health.
func (b *Backend) SetHealthy(v bool) { b.healthy.Store(v) }

// SetSlowHealth makes /health hang (until OpenGate or client disconnect).
func (b *Backend) SetSlowHealth(v bool) { b.slowHealth.Store(v) }

// OpenGate releases every request blocked on the gate, now and in the future.
func (b *Backend) OpenGate() { b.gateOnce.Do(func() { close(b.gate) }) }

// Stop shuts the server down so connections are refused (a crashed process).
func (b *Backend) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.srv != nil {
		b.srv.Close()
		b.srv = nil
	}
}

// Start restarts a stopped backend on its original address (a restarted process).
func (b *Backend) Start() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.srv != nil {
		return nil
	}
	ln, err := net.Listen("tcp", b.addr)
	if err != nil {
		return err
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(b.serve))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	b.srv = srv
	return nil
}

func (b *Backend) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Backend", b.Name)

	switch {
	case r.URL.Path == "/health":
		if b.slowHealth.Load() {
			select {
			case <-r.Context().Done():
				return
			case <-b.gate:
			}
		}
		if !b.healthy.Load() {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)

	case r.URL.Path == "/slow":
		b.hits.Add(1)
		select {
		case b.Entered <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-b.gate:
			_, _ = io.WriteString(w, "slow done")
		}

	case strings.HasPrefix(r.URL.Path, "/status/"):
		b.hits.Add(1)
		code, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/status/"))
		if err != nil {
			code = http.StatusBadRequest
		}
		w.Header().Set("X-Custom-Response", "propagated")
		w.WriteHeader(code)

	default:
		b.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(EchoResponse{
			Backend: b.Name,
			Method:  r.Method,
			Path:    r.URL.Path,
			Query:   r.URL.RawQuery,
			Header:  r.Header,
			Body:    string(body),
		})
	}
}
