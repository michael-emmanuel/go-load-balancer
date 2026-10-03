// Package proxy forwards client requests to the backend chosen by the
// balancer. It owns request rewriting, transport settings and error mapping,
// and nothing about health state or selection policy.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sync/atomic"
	"time"

	"github.com/example/load-balancer/internal/balancer"
)

// Options configures the outbound transport.
type Options struct {
	// DialTimeout bounds establishing a TCP connection to a backend.
	DialTimeout time.Duration
	// ResponseHeaderTimeout bounds the wait for response headers after the
	// request has been written. It is the proxy's "backend is too slow" limit.
	ResponseHeaderTimeout time.Duration
}

// DefaultOptions returns conservative timeouts.
func DefaultOptions() Options {
	return Options{DialTimeout: 3 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
}

// Stats is a snapshot of proxy-level counters.
type Stats struct {
	TotalRequests     uint64 `json:"total_requests"`
	FailedRequests    uint64 `json:"failed_requests"`
	NoBackendRequests uint64 `json:"no_backend_requests"`
}

// Handler is an http.Handler that load-balances requests across a Pool.
type Handler struct {
	pool    *balancer.Pool
	logger  *slog.Logger
	proxies map[*balancer.Backend]*httputil.ReverseProxy // built once, read-only afterwards

	total     atomic.Uint64
	failed    atomic.Uint64
	noBackend atomic.Uint64
}

// NewHandler builds one httputil.ReverseProxy per backend, all sharing a single
// http.Transport so TCP connections to backends are pooled and reused.
func NewHandler(pool *balancer.Pool, logger *slog.Logger, opts Options) *Handler {
	transport := &http.Transport{
		// Backends are internal; never route through HTTP(S)_PROXY from the
		// environment.
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: opts.DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
	}

	h := &Handler{
		pool:    pool,
		logger:  logger,
		proxies: make(map[*balancer.Backend]*httputil.ReverseProxy, pool.Len()),
	}
	for _, b := range pool.Backends() {
		target := b.URL
		h.proxies[b] = &httputil.ReverseProxy{
			// Rewrite (preferred over the older Director) runs on a copy of the
			// request. SetURL points it at the backend and joins the paths;
			// SetXForwarded records the client address. Hop-by-hop headers are
			// stripped by ReverseProxy before Rewrite is called.
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.SetXForwarded()
			},
			Transport:    transport,
			ErrorHandler: h.errorHandler(b),
		}
	}
	return h
}

// ServeHTTP selects a backend and forwards the request to it. No lock is held
// while the request is in flight.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.total.Add(1)

	b := h.pool.Next()
	if b == nil {
		h.noBackend.Add(1)
		h.logger.Warn("no healthy backend available", "method", r.Method, "path", r.URL.Path)
		http.Error(w, "no healthy backends available", http.StatusServiceUnavailable)
		return
	}

	b.RecordRequest()
	// Only method and path are logged. Query strings and headers can carry
	// credentials and are deliberately left out.
	h.logger.Info("request routed", "method", r.Method, "path", r.URL.Path, "backend", b.URL.String())
	h.proxies[b].ServeHTTP(w, r)
}

// Stats returns a snapshot of the proxy counters.
func (h *Handler) Stats() Stats {
	return Stats{
		TotalRequests:     h.total.Load(),
		FailedRequests:    h.failed.Load(),
		NoBackendRequests: h.noBackend.Load(),
	}
}

// errorHandler maps a transport-level failure to a client response and feeds
// it back to the backend's health state (passive health checking).
func (h *Handler) errorHandler(b *balancer.Backend) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		if errors.Is(err, context.Canceled) {
			// The client went away. That says nothing about the backend, so it
			// is neither a proxy failure nor a health signal.
			h.logger.Info("client cancelled request", "path", r.URL.Path, "backend", b.URL.String())
			return
		}

		h.failed.Add(1)
		status := http.StatusBadGateway
		if isTimeout(err) {
			status = http.StatusGatewayTimeout
		}
		h.pool.ReportFailure(b, err)
		h.logger.Error("proxy error",
			"backend", b.URL.String(), "path", r.URL.Path, "status", status, "error", err)
		http.Error(w, http.StatusText(status), status)
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
