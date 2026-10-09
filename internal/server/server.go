// Package server wires configuration, pool, health checker and proxy into a
// runnable HTTP service with a well-defined lifecycle.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/michael-emmanuel/go-load-balancer/internal/balancer"
	"github.com/michael-emmanuel/go-load-balancer/internal/config"
	"github.com/michael-emmanuel/go-load-balancer/internal/proxy"
)

// Server is the load balancer process: an HTTP listener, a backend pool, a
// background health checker and the proxy handler.
type Server struct {
	cfg     config.Config
	logger  *slog.Logger
	pool    *balancer.Pool
	checker *balancer.HealthChecker
	proxy   *proxy.Handler
	http    *http.Server
	ln      net.Listener
}

// New validates cfg, builds the components and binds the listen address.
// Binding here (rather than in Run) surfaces "address in use" immediately and
// lets callers read Addr before Run starts, which matters when listening on
// port 0 in tests.
func New(cfg config.Config, logger *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	pool, err := balancer.NewPool(cfg.Backends, balancer.Options{
		FailureThreshold:  cfg.FailureThreshold,
		RecoveryThreshold: cfg.RecoveryThreshold,
	}, logger)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("server: listen: %w", err)
	}

	s := &Server{
		cfg:    cfg,
		logger: logger,
		pool:   pool,
		checker: balancer.NewHealthChecker(pool, balancer.HealthCheckConfig{
			Path:     cfg.HealthCheckPath,
			Interval: cfg.HealthCheckInterval,
			Timeout:  cfg.HealthCheckTimeout,
		}, logger),
		proxy: proxy.NewHandler(pool, logger, proxy.DefaultOptions()),
		ln:    ln,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)   // reserved: the LB's own health
	mux.HandleFunc("/metrics", s.handleMetrics) // reserved: the LB's own counters
	mux.Handle("/", s.proxy)                    // everything else is forwarded

	s.http = &http.Server{
		Handler: mux,
		// Bounds slow-header clients (slowloris). There is deliberately no
		// WriteTimeout: it would cut off legitimately slow proxied responses.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s, nil
}

// Addr returns the bound listen address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Run serves traffic until ctx is cancelled, then shuts down gracefully.
//
// Lifecycle:
//  1. Run one synchronous health round so traffic starts with real state.
//  2. Start the background health checker and the HTTP server.
//  3. On ctx cancellation: cancel the health checker, stop accepting new
//     connections, and let in-flight requests finish for up to ShutdownTimeout
//     (http.Server.Shutdown); force-close whatever remains.
//  4. Wait for the health goroutine to exit before returning.
func (s *Server) Run(ctx context.Context) error {
	s.logger.Info("starting load balancer",
		"addr", s.Addr(), "backends", s.pool.Len(),
		"health_interval", s.cfg.HealthCheckInterval.String(),
		"health_timeout", s.cfg.HealthCheckTimeout.String())

	// Startup check. Connections that arrive meanwhile wait in the kernel's
	// accept queue and are served once Serve starts, so no request is routed
	// on unverified state.
	s.checker.CheckAll(ctx)
	s.logger.Info("initial health check complete", "healthy", s.pool.HealthyCount(), "total", s.pool.Len())

	healthCtx, stopHealth := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.checker.Run(healthCtx)
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.http.Serve(s.ln) }()

	select {
	case err := <-serveErr:
		// Serve returned without a shutdown request: a real failure.
		stopHealth()
		wg.Wait()
		return fmt.Errorf("server: serve: %w", err)
	case <-ctx.Done():
	}

	s.logger.Info("shutting down", "timeout", s.cfg.ShutdownTimeout.String())
	stopHealth() // non-blocking; the goroutine is joined below

	// The shutdown deadline must not derive from ctx, which is already done.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()

	var result error
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		s.logger.Error("graceful shutdown incomplete, forcing close", "error", err)
		_ = s.http.Close()
		result = fmt.Errorf("server: shutdown: %w", err)
	}
	<-serveErr // Serve returns http.ErrServerClosed once shutdown begins
	wg.Wait()  // health-check goroutine has exited
	s.logger.Info("shutdown complete")
	return result
}

type healthResponse struct {
	Status          string `json:"status"`
	HealthyBackends int    `json:"healthy_backends"`
	TotalBackends   int    `json:"total_backends"`
}

// handleHealth reports the load balancer's own readiness: 200 while at least
// one backend can take traffic, 503 otherwise.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := healthResponse{
		Status:          "ok",
		HealthyBackends: s.pool.HealthyCount(),
		TotalBackends:   s.pool.Len(),
	}
	code := http.StatusOK
	if resp.HealthyBackends == 0 {
		resp.Status, code = "unavailable", http.StatusServiceUnavailable
	}
	writeJSON(w, code, resp)
}

type metricsResponse struct {
	proxy.Stats
	HealthyBackends int               `json:"healthy_backends"`
	Backends        []balancer.Status `json:"backends"`
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, metricsResponse{
		Stats:           s.proxy.Stats(),
		HealthyBackends: s.pool.HealthyCount(),
		Backends:        s.pool.Statuses(),
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil && !errors.Is(err, net.ErrClosed) {
		// Headers are already sent; nothing useful can be returned to the client.
		slog.Default().Debug("write response failed", "error", err)
	}
}
