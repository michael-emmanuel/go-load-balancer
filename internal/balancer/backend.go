package balancer

import (
	"log/slog"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Backend is one upstream server together with its health state.
//
// A Backend deliberately knows nothing about request forwarding (that lives in
// package proxy) and nothing about how it is selected (that lives in Pool).
// It answers exactly two questions: "who am I?" (URL) and "should I receive
// traffic right now?" (IsAlive).
//
// Concurrency: IsAlive is called on every request and must be cheap, so the
// alive flag is an atomic.Bool that can be read without a lock. Everything
// that changes together (the flag, the consecutive-result counters, the last
// error) is written under mu so a health transition is a single atomic step.
type Backend struct {
	// URL is the immutable identity of the backend.
	URL *url.URL

	logger *slog.Logger

	failureThreshold  int
	recoveryThreshold int

	alive    atomic.Bool   // lock-free read on the hot path; written under mu
	requests atomic.Uint64 // requests routed to this backend

	mu                   sync.Mutex // guards the fields below and writes to alive
	consecutiveFailures  int
	consecutiveSuccesses int
	lastErr              error
	lastChange           time.Time
}

// Status is a point-in-time copy of a backend's state, safe to serialize.
type Status struct {
	URL                 string    `json:"url"`
	Alive               bool      `json:"alive"`
	Requests            uint64    `json:"requests"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastError           string    `json:"last_error,omitempty"`
	LastChange          time.Time `json:"last_change"`
}

func newBackend(u *url.URL, opts Options, logger *slog.Logger) *Backend {
	b := &Backend{
		URL:               u,
		logger:            logger.With("backend", u.String()),
		failureThreshold:  opts.FailureThreshold,
		recoveryThreshold: opts.RecoveryThreshold,
		lastChange:        time.Now(),
	}
	b.alive.Store(opts.InitiallyHealthy)
	return b
}

// IsAlive reports whether the backend is eligible for routing. It never blocks.
func (b *Backend) IsAlive() bool { return b.alive.Load() }

// RecordRequest counts a request routed to this backend.
func (b *Backend) RecordRequest() { b.requests.Add(1) }

// RecordSuccess reports one successful health observation. It returns true if
// this observation moved the backend from unhealthy to healthy.
//
// A backend that is already healthy only has its failure streak reset. A
// backend that is unhealthy must accumulate RecoveryThreshold consecutive
// successes before rejoining, which damps flapping.
func (b *Backend) RecordSuccess() bool {
	b.mu.Lock()
	b.consecutiveFailures = 0
	b.lastErr = nil
	if b.alive.Load() {
		b.consecutiveSuccesses = 0
		b.mu.Unlock()
		return false
	}
	b.consecutiveSuccesses++
	if b.consecutiveSuccesses < b.recoveryThreshold {
		b.mu.Unlock()
		return false
	}
	b.consecutiveSuccesses = 0
	b.alive.Store(true)
	b.lastChange = time.Now()
	b.mu.Unlock()

	// Log outside the lock so a slow log sink cannot stall other state updates.
	b.logger.Info("backend recovered")
	return true
}

// RecordFailure reports one failed observation (a failed health check or a
// failed proxied request). It returns true if this observation moved the
// backend from healthy to unhealthy.
//
// The backend is never deleted: it stays in the pool so later successful
// health checks can bring it back.
func (b *Backend) RecordFailure(err error) bool {
	b.mu.Lock()
	b.consecutiveSuccesses = 0
	b.lastErr = err
	if !b.alive.Load() {
		b.mu.Unlock()
		return false
	}
	b.consecutiveFailures++
	if b.consecutiveFailures < b.failureThreshold {
		b.mu.Unlock()
		return false
	}
	b.consecutiveFailures = 0
	b.alive.Store(false)
	b.lastChange = time.Now()
	b.mu.Unlock()

	b.logger.Warn("backend unhealthy", "error", err)
	return true
}

// Status returns a consistent snapshot of the backend's state.
func (b *Backend) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Status{
		URL:                 b.URL.String(),
		Alive:               b.alive.Load(),
		Requests:            b.requests.Load(),
		ConsecutiveFailures: b.consecutiveFailures,
		LastChange:          b.lastChange,
	}
	if b.lastErr != nil {
		s.LastError = b.lastErr.Error()
	}
	return s
}
