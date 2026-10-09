package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/michael-emmanuel/go-load-balancer/internal/config"
	"github.com/michael-emmanuel/go-load-balancer/internal/server"
	"github.com/michael-emmanuel/go-load-balancer/internal/testutil"
)

type running struct {
	srv    *server.Server
	cancel context.CancelFunc
	done   chan error
	client *http.Client
}

func (r *running) url(path string) string { return "http://" + r.srv.Addr() + path }

func start(t *testing.T, mutate func(*config.Config), backends ...*testutil.Backend) *running {
	t.Helper()
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.HealthCheckInterval = 250 * time.Millisecond
	cfg.HealthCheckTimeout = 250 * time.Millisecond
	cfg.ShutdownTimeout = 5 * time.Second
	cfg.FailureThreshold, cfg.RecoveryThreshold = 1, 1
	cfg.Backends = nil
	for _, b := range backends {
		cfg.Backends = append(cfg.Backends, b.URL())
	}
	if mutate != nil {
		mutate(&cfg)
	}

	srv, err := server.New(cfg, testutil.NewLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{
		srv: srv, cancel: cancel, done: make(chan error, 1),
		client: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second},
	}
	go func() { r.done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop during cleanup")
		}
	})
	return r
}

func (r *running) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err // keep cleanup working
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func TestServesTrafficHealthAndMetrics(t *testing.T) {
	a, b := testutil.NewBackend(t, "A"), testutil.NewBackend(t, "B")
	r := start(t, nil, a, b)

	for _, want := range []string{"A", "B", "A"} {
		resp, err := r.client.Get(r.url("/"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if got := resp.Header.Get("X-Backend"); got != want {
			t.Fatalf("served by %q, want %q", got, want)
		}
	}

	resp, err := r.client.Get(r.url("/health"))
	if err != nil {
		t.Fatal(err)
	}
	var h struct {
		Status          string `json:"status"`
		HealthyBackends int    `json:"healthy_backends"`
		TotalBackends   int    `json:"total_backends"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	if resp.StatusCode != 200 || h.Status != "ok" || h.HealthyBackends != 2 || h.TotalBackends != 2 {
		t.Fatalf("health = %d %+v", resp.StatusCode, h)
	}

	resp, err = r.client.Get(r.url("/metrics"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		TotalRequests uint64 `json:"total_requests"`
		Backends      []struct {
			Requests uint64 `json:"requests"`
		} `json:"backends"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&m)
	resp.Body.Close()
	if m.TotalRequests != 3 || m.Backends[0].Requests != 2 || m.Backends[1].Requests != 1 {
		t.Fatalf("metrics = %+v", m)
	}
}

func TestHealthAndRoutingReturn503WhenAllBackendsUnhealthy(t *testing.T) {
	a := testutil.NewBackend(t, "A")
	a.SetHealthy(false)
	r := start(t, nil, a)

	for _, path := range []string{"/health", "/"} {
		resp, err := r.client.Get(r.url(path))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", path, resp.StatusCode)
		}
	}
}

func TestBackgroundChecksEjectAndReadmitBackends(t *testing.T) {
	a, b := testutil.NewBackend(t, "A"), testutil.NewBackend(t, "B")
	r := start(t, nil, a, b)

	healthy := func() int {
		resp, err := r.client.Get(r.url("/health"))
		if err != nil {
			return -1
		}
		defer resp.Body.Close()
		var h struct {
			HealthyBackends int `json:"healthy_backends"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&h)
		return h.HealthyBackends
	}

	b.SetHealthy(false)
	testutil.Eventually(t, "B ejected", func() bool { return healthy() == 1 })
	b.SetHealthy(true)
	testutil.Eventually(t, "B readmitted", func() bool { return healthy() == 2 })
}

// In-flight requests finish, new connections are refused, Run returns nil and
// only after the health goroutine has exited.
func TestGracefulShutdownDrainsInFlightRequests(t *testing.T) {
	a := testutil.NewBackend(t, "A")
	r := start(t, nil, a)

	type result struct {
		status int
		body   string
		err    error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := r.client.Get(r.url("/slow"))
		if err != nil {
			res <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		res <- result{status: resp.StatusCode, body: string(b)}
	}()

	<-a.Entered // the request is now in flight inside the backend
	r.cancel()  // SIGTERM equivalent

	testutil.Eventually(t, "listener closed", func() bool {
		c, err := net.DialTimeout("tcp", r.srv.Addr(), 100*time.Millisecond)
		if err != nil {
			return true
		}
		c.Close()
		return false
	})

	select {
	case <-r.done:
		t.Fatal("Run returned while a request was still in flight")
	default:
	}

	a.OpenGate() // let the in-flight request complete
	got := <-res
	if got.err != nil || got.status != 200 || got.body != "slow done" {
		t.Fatalf("in-flight request not drained: %+v", got)
	}
	if err := r.wait(t); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}

func TestShutdownTimeoutForcesClose(t *testing.T) {
	a := testutil.NewBackend(t, "A")
	r := start(t, func(c *config.Config) { c.ShutdownTimeout = 50 * time.Millisecond }, a)

	go func() { _, _ = r.client.Get(r.url("/slow")) }()
	<-a.Entered
	r.cancel()

	err := r.wait(t)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want deadline exceeded", err)
	}
}

// After Run returns, the health-check goroutine must be gone. Run joins it with
// a WaitGroup, so this checks the stack dump rather than relying on timing.
func TestShutdownStopsHealthCheckGoroutine(t *testing.T) {
	a := testutil.NewBackend(t, "A")
	r := start(t, nil, a)

	r.cancel()
	if err := r.wait(t); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1<<20)
	stacks := string(buf[:runtime.Stack(buf, true)])
	if strings.Contains(stacks, "balancer.(*HealthChecker).Run") {
		t.Fatalf("health-check goroutine still running after shutdown:\n%s", stacks)
	}
}
