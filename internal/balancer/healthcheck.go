package balancer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// HealthCheckConfig configures active health checking.
type HealthCheckConfig struct {
	// Path is requested with GET on every backend, for example "/health".
	Path string
	// Interval is the time between check rounds.
	Interval time.Duration
	// Timeout bounds each individual probe.
	Timeout time.Duration
}

// HealthChecker actively probes every backend in a Pool and feeds the results
// into each backend's health state. It owns no goroutines until Run is called,
// and Run owns exactly one (plus short-lived per-probe goroutines that finish
// before each round ends).
type HealthChecker struct {
	pool   *Pool
	cfg    HealthCheckConfig
	client *http.Client
	logger *slog.Logger
}

// NewHealthChecker builds a checker for pool.
func NewHealthChecker(pool *Pool, cfg HealthCheckConfig, logger *slog.Logger) *HealthChecker {
	return &HealthChecker{
		pool: pool,
		cfg:  cfg,
		client: &http.Client{
			// Keep-alives are disabled on purpose: every probe opens a fresh
			// TCP connection, so a backend whose accept path is broken cannot
			// hide behind a pooled connection that still happens to work.
			Transport: &http.Transport{DisableKeepAlives: true},
			// A redirect is not a healthy answer; do not follow it.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		logger: logger,
	}
}

// Run performs a check round every Interval until ctx is cancelled, then
// returns. The caller is expected to run it in its own goroutine and to wait
// for it to return during shutdown. Rounds never overlap: a slow round delays
// the next tick instead of stacking goroutines.
func (h *HealthChecker) Run(ctx context.Context) {
	ticker := time.NewTicker(h.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.CheckAll(ctx)
		}
	}
}

// CheckAll probes every backend concurrently and returns when all probes have
// finished. Each probe is bounded by Timeout, so CheckAll is bounded too.
// It is exported so startup code and tests can run a round synchronously.
func (h *HealthChecker) CheckAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, b := range h.pool.backends {
		wg.Add(1)
		go func(b *Backend) {
			defer wg.Done()
			h.checkOne(ctx, b)
		}(b)
	}
	wg.Wait()
}

func (h *HealthChecker) checkOne(ctx context.Context, b *Backend) {
	err := h.probe(ctx, b)
	if ctx.Err() != nil {
		// The parent context ended (shutdown). The probe failed because we
		// cancelled it, not because the backend is unhealthy, so record nothing.
		return
	}
	if err != nil {
		b.RecordFailure(err)
		return
	}
	b.RecordSuccess()
}

// probe issues one GET to the backend's health path. Any transport error,
// timeout, or non-2xx status is a failure.
func (h *HealthChecker) probe(ctx context.Context, b *Backend) error {
	ctx, cancel := context.WithTimeout(ctx, h.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL.JoinPath(h.cfg.Path).String(), nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection closes cleanly.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}
	return nil
}
