package proxy_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/load-balancer/internal/balancer"
	"github.com/example/load-balancer/internal/proxy"
	"github.com/example/load-balancer/internal/testutil"
)

type env struct {
	backends []*testutil.Backend
	pool     *balancer.Pool
	checker  *balancer.HealthChecker
	handler  *proxy.Handler
	lb       *httptest.Server
	client   *http.Client
}

func newEnv(t *testing.T, opts proxy.Options) *env {
	t.Helper()
	e := &env{client: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}}
	var urls []string
	for _, name := range []string{"A", "B", "C"} {
		b := testutil.NewBackend(t, name)
		e.backends = append(e.backends, b)
		urls = append(urls, b.URL())
	}
	e.start(t, urls, opts)
	e.checker.CheckAll(context.Background())
	return e
}

// start builds pool, checker, handler and the LB server for the given URLs.
func (e *env) start(t *testing.T, urls []string, opts proxy.Options) {
	t.Helper()
	pool, err := balancer.NewPool(urls, balancer.Options{FailureThreshold: 1, RecoveryThreshold: 1}, testutil.NewLogger())
	if err != nil {
		t.Fatal(err)
	}
	e.pool = pool
	e.checker = balancer.NewHealthChecker(pool, balancer.HealthCheckConfig{
		Path: "/health", Interval: time.Hour, Timeout: 100 * time.Millisecond,
	}, testutil.NewLogger())
	e.handler = proxy.NewHandler(pool, testutil.NewLogger(), opts)
	e.lb = httptest.NewServer(e.handler)
	t.Cleanup(e.lb.Close)
}

func (e *env) do(t *testing.T, method, path string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.lb.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// sequence sends n GET requests and returns the X-Backend header of each.
func (e *env) sequence(t *testing.T, n int) []string {
	t.Helper()
	var out []string
	for i := 0; i < n; i++ {
		resp := e.do(t, http.MethodGet, "/", "")
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			out = append(out, "status:"+http.StatusText(resp.StatusCode))
			continue
		}
		out = append(out, resp.Header.Get("X-Backend"))
	}
	return out
}

func assertSeq(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Scenario 1.
func TestDistributesRoundRobin(t *testing.T) {
	e := newEnv(t, proxy.DefaultOptions())
	assertSeq(t, e.sequence(t, 6), "A", "B", "C", "A", "B", "C")
}

// Scenarios 2 and 3.
func TestFailedBackendIsSkippedThenRejoins(t *testing.T) {
	e := newEnv(t, proxy.DefaultOptions())

	e.backends[1].SetHealthy(false)
	e.checker.CheckAll(context.Background())
	assertSeq(t, e.sequence(t, 4), "A", "C", "A", "C")

	e.backends[1].SetHealthy(true)
	e.checker.CheckAll(context.Background())
	got := e.sequence(t, 3)
	if !strings.Contains(strings.Join(got, ","), "B") {
		t.Fatalf("B did not receive traffic after recovery: %v", got)
	}
}

// Scenario 4.
func TestNoHealthyBackendsReturns503(t *testing.T) {
	e := newEnv(t, proxy.DefaultOptions())
	for _, b := range e.backends {
		b.SetHealthy(false)
	}
	e.checker.CheckAll(context.Background())

	resp := e.do(t, http.MethodGet, "/", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := e.handler.Stats().NoBackendRequests; got != 1 {
		t.Fatalf("no-backend counter = %d, want 1", got)
	}
}

// Scenario 5, observed end to end: the slow backend stops receiving traffic.
func TestSlowBackendIsDetectedAndExcluded(t *testing.T) {
	e := newEnv(t, proxy.DefaultOptions())
	e.backends[2].SetSlowHealth(true)
	e.checker.CheckAll(context.Background())
	assertSeq(t, e.sequence(t, 4), "A", "B", "A", "B")
}

func TestForwardsMethodPathQueryHeadersAndBody(t *testing.T) {
	e := newEnv(t, proxy.DefaultOptions())

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		body := ""
		if method != http.MethodGet && method != http.MethodDelete {
			body = "payload-" + method
		}
		req, _ := http.NewRequest(method, e.lb.URL+"/a/b?x=1&y=two&x=3", strings.NewReader(body))
		req.Header.Set("X-Request-Id", "req-42")
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var echo testutil.EchoResponse
		if err := json.NewDecoder(resp.Body).Decode(&echo); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if echo.Method != method {
			t.Errorf("method = %q, want %q", echo.Method, method)
		}
		if echo.Path != "/a/b" {
			t.Errorf("path = %q", echo.Path)
		}
		if echo.Query != "x=1&y=two&x=3" {
			t.Errorf("query = %q", echo.Query)
		}
		if echo.Body != body {
			t.Errorf("body = %q, want %q", echo.Body, body)
		}
		if got := echo.Header.Get("X-Request-Id"); got != "req-42" {
			t.Errorf("X-Request-Id = %q", got)
		}
		if echo.Header.Get("X-Forwarded-For") == "" {
			t.Error("X-Forwarded-For should be set by the proxy")
		}
	}
}

func TestStripsHopByHopHeaders(t *testing.T) {
	e := newEnv(t, proxy.DefaultOptions())
	req, _ := http.NewRequest(http.MethodGet, e.lb.URL+"/", nil)
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "should-not-reach-backend")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var echo testutil.EchoResponse
	_ = json.NewDecoder(resp.Body).Decode(&echo)
	if echo.Header.Get("X-Hop") != "" {
		t.Fatal("hop-by-hop header leaked to the backend")
	}
}

func TestPropagatesBackendStatusAndHeaders(t *testing.T) {
	e := newEnv(t, proxy.DefaultOptions())
	resp := e.do(t, http.MethodGet, "/status/418", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", resp.StatusCode)
	}
	if resp.Header.Get("X-Custom-Response") != "propagated" {
		t.Fatal("backend response header was not propagated")
	}
	// An application-level 5xx is the backend's answer, not a proxy failure.
	resp2 := e.do(t, http.MethodGet, "/status/500", "")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 passed through", resp2.StatusCode)
	}
	if e.handler.Stats().FailedRequests != 0 {
		t.Fatal("a backend 500 must not count as a proxy failure")
	}
}

// Connection refused: first request to B fails with 502 and, because the
// failure threshold is 1, B is ejected before any health check runs.
func TestConnectionRefusedReturns502AndEjectsBackend(t *testing.T) {
	e := newEnv(t, proxy.DefaultOptions())
	e.backends[1].Stop()

	// The failed request consumed ticket 1. B is then ejected, so ticket 2 maps
	// onto the two-element healthy set {A, C} as 2 mod 2 = 0, which is A.
	got := e.sequence(t, 5)
	assertSeq(t, got, "A", "status:Bad Gateway", "A", "C", "A")
	if e.pool.Backends()[1].IsAlive() {
		t.Fatal("B should have been ejected by the passive failure")
	}
	if e.handler.Stats().FailedRequests != 1 {
		t.Fatalf("failed = %d, want 1", e.handler.Stats().FailedRequests)
	}
}

// rawBackend starts a TCP server that handles each connection with fn.
func rawBackend(t *testing.T, fn func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				fn(c)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); wg.Wait() })
	return "http://" + ln.Addr().String()
}

func singleBackendEnv(t *testing.T, url string, opts proxy.Options) *env {
	e := &env{client: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}}
	pool, err := balancer.NewPool([]string{url}, balancer.Options{InitiallyHealthy: true, FailureThreshold: 1}, testutil.NewLogger())
	if err != nil {
		t.Fatal(err)
	}
	e.pool = pool
	e.handler = proxy.NewHandler(pool, testutil.NewLogger(), opts)
	e.lb = httptest.NewServer(e.handler)
	t.Cleanup(e.lb.Close)
	return e
}

func TestBackendClosesConnectionReturns502(t *testing.T) {
	url := rawBackend(t, func(c net.Conn) {
		buf := make([]byte, 1024)
		_, _ = c.Read(buf) // read the request, then hang up without answering
	})
	e := singleBackendEnv(t, url, proxy.DefaultOptions())
	resp := e.do(t, http.MethodGet, "/", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestMalformedBackendResponseReturns502(t *testing.T) {
	url := rawBackend(t, func(c net.Conn) {
		buf := make([]byte, 1024)
		_, _ = c.Read(buf)
		_, _ = io.WriteString(c, "this is not http\r\n\r\n")
	})
	e := singleBackendEnv(t, url, proxy.DefaultOptions())
	resp := e.do(t, http.MethodGet, "/", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestSlowBackendResponseReturns504(t *testing.T) {
	opts := proxy.DefaultOptions()
	opts.ResponseHeaderTimeout = 50 * time.Millisecond
	e := newEnv(t, opts)

	resp := e.do(t, http.MethodGet, "/slow", "") // routed to A, which never answers
	resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
}

// Client cancellation says nothing about backend health. The handler is called
// directly so the test is synchronous.
func TestClientCancellationDoesNotEjectBackend(t *testing.T) {
	e := newEnv(t, proxy.DefaultOptions())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)

	e.handler.ServeHTTP(httptest.NewRecorder(), req)

	if e.pool.HealthyCount() != 3 {
		t.Fatal("client cancellation must not eject a backend")
	}
	if e.handler.Stats().FailedRequests != 0 {
		t.Fatal("client cancellation must not count as a proxy failure")
	}
}

func TestConcurrentRequests(t *testing.T) {
	e := newEnv(t, proxy.DefaultOptions())
	const workers, perWorker = 20, 25 // 500 requests

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				resp, err := e.client.Get(e.lb.URL + "/")
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("status = %d", resp.StatusCode)
				}
			}
		}()
	}
	wg.Wait()

	if got := e.handler.Stats().TotalRequests; got != workers*perWorker {
		t.Fatalf("total = %d", got)
	}
	var sum int64
	for _, b := range e.backends {
		h := b.Hits()
		sum += h
		// 500 tickets over 3 backends: 166 or 167 each.
		if h < 166 || h > 167 {
			t.Errorf("backend %s served %d requests, want 166 or 167", b.Name, h)
		}
	}
	if sum != workers*perWorker {
		t.Fatalf("backends served %d, want %d", sum, workers*perWorker)
	}
}
