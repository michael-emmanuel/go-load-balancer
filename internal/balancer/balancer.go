// Package balancer contains the backend pool, the round-robin selection
// algorithm, and the active health checker. It performs no request
// forwarding; see package proxy for that.
package balancer

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync/atomic"
)

// Options configures backend health transitions.
type Options struct {
	// FailureThreshold is the number of consecutive failures that mark a
	// healthy backend unhealthy. Values below 1 are treated as 1.
	FailureThreshold int
	// RecoveryThreshold is the number of consecutive successful health checks
	// that return an unhealthy backend to service. Values below 1 are treated
	// as 1.
	RecoveryThreshold int
	// InitiallyHealthy controls the state before the first health check.
	// The default (false) fails closed: no traffic until a backend has proven
	// itself healthy.
	InitiallyHealthy bool
}

func (o Options) withDefaults() Options {
	if o.FailureThreshold < 1 {
		o.FailureThreshold = 1
	}
	if o.RecoveryThreshold < 1 {
		o.RecoveryThreshold = 1
	}
	return o
}

// Pool is a fixed set of backends plus the round-robin cursor.
//
// The membership of the pool never changes after NewPool returns, so the
// backends slice is read without any lock. Only two pieces of state change at
// runtime: each backend's health (see Backend) and the round-robin counter
// (an atomic).
type Pool struct {
	backends []*Backend
	next     atomic.Uint64 // monotonically increasing ticket counter
}

// NewPool validates the backend URLs and builds a pool in the given order.
func NewPool(rawURLs []string, opts Options, logger *slog.Logger) (*Pool, error) {
	if len(rawURLs) == 0 {
		return nil, errors.New("balancer: at least one backend is required")
	}
	opts = opts.withDefaults()

	seen := make(map[string]struct{}, len(rawURLs))
	backends := make([]*Backend, 0, len(rawURLs))
	for _, raw := range rawURLs {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("balancer: invalid backend %q: %w", raw, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("balancer: backend %q must be an absolute http or https URL", raw)
		}
		if _, dup := seen[u.String()]; dup {
			return nil, fmt.Errorf("balancer: duplicate backend %q", raw)
		}
		seen[u.String()] = struct{}{}

		backends = append(backends, newBackend(u, opts, logger))
		logger.Info("backend registered", "backend", u.String(), "initially_healthy", opts.InitiallyHealthy)
	}
	return &Pool{backends: backends}, nil
}

// Next returns the next healthy backend in round-robin order, or nil if no
// backend is healthy.
//
// Algorithm: take a ticket from the atomic counter, count the healthy
// backends, and pick the (ticket mod healthyCount)-th healthy backend. The
// cursor therefore advances over the healthy set only, so with A, B, C where B
// is unhealthy the sequence is A, C, A, C rather than A, C, C, A, C, C (which
// is what "skip to the next healthy one" would produce).
//
// Synchronization: the only shared write is one atomic add. No lock is held,
// and nothing is held across the proxied request. Health may change between
// the count and the walk; that is benign (see docs/concurrency.md) and handled
// by falling back to the last healthy backend seen.
//
// Cost: O(n) in the number of backends, no allocation.
func (p *Pool) Next() *Backend {
	ticket := p.next.Add(1) - 1

	healthy := 0
	for _, b := range p.backends {
		if b.IsAlive() {
			healthy++
		}
	}
	if healthy == 0 {
		return nil
	}

	target := int(ticket % uint64(healthy))
	var last *Backend
	seen := 0
	for _, b := range p.backends {
		if !b.IsAlive() {
			continue
		}
		last = b
		if seen == target {
			return b
		}
		seen++
	}
	// Health flipped between the two passes and the set shrank. Returning the
	// last healthy backend we saw is a safe, bounded-cost answer; nil means the
	// set became empty.
	return last
}

// Backends returns a copy of the backend list in configuration order.
func (p *Pool) Backends() []*Backend {
	out := make([]*Backend, len(p.backends))
	copy(out, p.backends)
	return out
}

// HealthyCount returns the number of backends currently eligible for routing.
func (p *Pool) HealthyCount() int {
	n := 0
	for _, b := range p.backends {
		if b.IsAlive() {
			n++
		}
	}
	return n
}

// Len returns the total number of configured backends.
func (p *Pool) Len() int { return len(p.backends) }

// ReportFailure lets the request path feed a passive failure observation
// (for example connection refused) into the backend's health state. It
// returns true if the backend transitioned to unhealthy.
func (p *Pool) ReportFailure(b *Backend, err error) bool {
	return b.RecordFailure(err)
}

// Statuses returns a snapshot of every backend for observability endpoints.
func (p *Pool) Statuses() []Status {
	out := make([]Status, len(p.backends))
	for i, b := range p.backends {
		out[i] = b.Status()
	}
	return out
}
