package balancer_test

import (
	"context"
	"testing"
	"time"

	"github.com/michael-emmanuel/go-load-balancer/internal/balancer"
	"github.com/michael-emmanuel/go-load-balancer/internal/testutil"
)

func setup(t *testing.T, n int, timeout time.Duration, opts balancer.Options) ([]*testutil.Backend, *balancer.Pool, *balancer.HealthChecker) {
	t.Helper()
	var fakes []*testutil.Backend
	var urls []string
	for i := 0; i < n; i++ {
		f := testutil.NewBackend(t, string(rune('A'+i)))
		fakes = append(fakes, f)
		urls = append(urls, f.URL())
	}
	pool, err := balancer.NewPool(urls, opts, testutil.NewLogger())
	if err != nil {
		t.Fatal(err)
	}
	hc := balancer.NewHealthChecker(pool, balancer.HealthCheckConfig{
		Path: "/health", Interval: 5 * time.Millisecond, Timeout: timeout,
	}, testutil.NewLogger())
	return fakes, pool, hc
}

func TestStartsUnhealthyUntilFirstCheck(t *testing.T) {
	_, pool, hc := setup(t, 3, time.Second, balancer.Options{})
	if pool.HealthyCount() != 0 {
		t.Fatal("backends must start unhealthy")
	}
	hc.CheckAll(context.Background())
	if pool.HealthyCount() != 3 {
		t.Fatalf("healthy = %d, want 3", pool.HealthyCount())
	}
}

func TestFailingBackendIsMarkedUnhealthyAndRecovers(t *testing.T) {
	fakes, pool, hc := setup(t, 3, time.Second, balancer.Options{})
	ctx := context.Background()
	hc.CheckAll(ctx)

	fakes[1].SetHealthy(false)
	hc.CheckAll(ctx)
	if pool.Backends()[1].IsAlive() || pool.HealthyCount() != 2 {
		t.Fatal("B should be unhealthy, A and C healthy")
	}

	fakes[1].SetHealthy(true)
	hc.CheckAll(ctx)
	if !pool.Backends()[1].IsAlive() || pool.HealthyCount() != 3 {
		t.Fatal("B should have recovered")
	}
}

func TestHealthCheckTimeoutMarksBackendUnhealthy(t *testing.T) {
	fakes, pool, hc := setup(t, 2, 50*time.Millisecond, balancer.Options{})
	ctx := context.Background()
	hc.CheckAll(ctx)

	fakes[0].SetSlowHealth(true)
	hc.CheckAll(ctx)

	if pool.Backends()[0].IsAlive() {
		t.Fatal("slow backend should be unhealthy after timeout")
	}
	if !pool.Backends()[1].IsAlive() {
		t.Fatal("fast backend should be unaffected")
	}
	if got := pool.Backends()[0].Status().LastError; got == "" {
		t.Fatal("last error should record the timeout")
	}
}

func TestConnectionRefusedAndRestart(t *testing.T) {
	fakes, pool, hc := setup(t, 2, time.Second, balancer.Options{})
	ctx := context.Background()
	hc.CheckAll(ctx)

	fakes[0].Stop() // process crash
	hc.CheckAll(ctx)
	if pool.Backends()[0].IsAlive() {
		t.Fatal("crashed backend should be unhealthy")
	}

	if err := fakes[0].Start(); err != nil { // process restart on the same address
		t.Fatal(err)
	}
	hc.CheckAll(ctx)
	if !pool.Backends()[0].IsAlive() {
		t.Fatal("restarted backend should rejoin")
	}
}

func TestShutdownCancellationDoesNotMarkBackendsUnhealthy(t *testing.T) {
	_, pool, hc := setup(t, 2, time.Second, balancer.Options{InitiallyHealthy: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hc.CheckAll(ctx)
	if pool.HealthyCount() != 2 {
		t.Fatal("a cancelled round must not change health state")
	}
}

func TestRunStopsWhenContextIsCancelled(t *testing.T) {
	_, _, hc := setup(t, 2, time.Second, balancer.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hc.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation (goroutine leak)")
	}
}

func TestRunDetectsFailureAndRecoveryPeriodically(t *testing.T) {
	fakes, pool, hc := setup(t, 2, time.Second, balancer.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hc.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	b := pool.Backends()[0]
	testutil.Eventually(t, "initial healthy", b.IsAlive)

	fakes[0].SetHealthy(false)
	testutil.Eventually(t, "marked unhealthy", func() bool { return !b.IsAlive() })

	fakes[0].SetHealthy(true)
	testutil.Eventually(t, "recovered", b.IsAlive)
}
