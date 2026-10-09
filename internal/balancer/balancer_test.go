package balancer_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/michael-emmanuel/go-load-balancer/internal/balancer"
	"github.com/michael-emmanuel/go-load-balancer/internal/testutil"
)

var hosts = []string{"http://a.test", "http://b.test", "http://c.test"}

func newPool(t *testing.T, opts balancer.Options) *balancer.Pool {
	t.Helper()
	p, err := balancer.NewPool(hosts, opts, testutil.NewLogger())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// pick returns the host names of the next n selections ("-" for no backend).
func pick(p *balancer.Pool, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if b := p.Next(); b != nil {
			out = append(out, b.URL.Hostname())
		} else {
			out = append(out, "-")
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRoundRobinOrder(t *testing.T) {
	p := newPool(t, balancer.Options{InitiallyHealthy: true})
	got := pick(p, 6)
	want := []string{"a.test", "b.test", "c.test", "a.test", "b.test", "c.test"}
	if !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestUnhealthyBackendIsExcluded(t *testing.T) {
	p := newPool(t, balancer.Options{InitiallyHealthy: true})
	p.Backends()[1].RecordFailure(errors.New("down")) // B

	got := pick(p, 4)
	want := []string{"a.test", "c.test", "a.test", "c.test"}
	if !equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRecoveredBackendRejoinsRotation(t *testing.T) {
	p := newPool(t, balancer.Options{InitiallyHealthy: true})
	b := p.Backends()[1]
	b.RecordFailure(errors.New("down"))
	if b.IsAlive() {
		t.Fatal("B should be unhealthy")
	}
	if !b.RecordSuccess() {
		t.Fatal("expected a recovery transition")
	}

	seen := map[string]bool{}
	for _, h := range pick(p, 3) {
		seen[h] = true
	}
	if !seen["b.test"] || len(seen) != 3 {
		t.Fatalf("B did not rejoin rotation: %v", seen)
	}
}

func TestNoHealthyBackends(t *testing.T) {
	p := newPool(t, balancer.Options{}) // fail closed: nothing healthy yet
	if b := p.Next(); b != nil {
		t.Fatalf("expected nil, got %v", b.URL)
	}
	if p.HealthyCount() != 0 {
		t.Fatalf("healthy count = %d", p.HealthyCount())
	}
}

func TestThresholdsProvideHysteresis(t *testing.T) {
	p := newPool(t, balancer.Options{InitiallyHealthy: true, FailureThreshold: 3, RecoveryThreshold: 2})
	b := p.Backends()[0]
	err := errors.New("x")

	b.RecordFailure(err)
	b.RecordFailure(err)
	if !b.IsAlive() {
		t.Fatal("two failures must not eject with threshold 3")
	}
	if !b.RecordFailure(err) || b.IsAlive() {
		t.Fatal("third failure must eject")
	}

	if b.RecordSuccess() || b.IsAlive() {
		t.Fatal("one success must not readmit with threshold 2")
	}
	b.RecordFailure(err) // a failure resets the success streak
	b.RecordSuccess()
	if b.IsAlive() {
		t.Fatal("success streak should have been reset by the failure")
	}
	if !b.RecordSuccess() || !b.IsAlive() {
		t.Fatal("two consecutive successes must readmit")
	}
}

func TestNonConsecutiveFailuresDoNotEject(t *testing.T) {
	p := newPool(t, balancer.Options{InitiallyHealthy: true, FailureThreshold: 2})
	b := p.Backends()[0]
	b.RecordFailure(errors.New("x"))
	b.RecordSuccess() // breaks the streak
	b.RecordFailure(errors.New("x"))
	if !b.IsAlive() {
		t.Fatal("non-consecutive failures must not eject")
	}
}

// With a fixed healthy set, N concurrent selections must be distributed
// exactly evenly: each ticket is unique because the counter is atomic.
func TestConcurrentSelectionIsEvenlyDistributed(t *testing.T) {
	p := newPool(t, balancer.Options{InitiallyHealthy: true})
	const workers, perWorker = 50, 60 // 3000 selections

	var mu sync.Mutex
	counts := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := map[string]int{}
			for i := 0; i < perWorker; i++ {
				local[p.Next().URL.Hostname()]++
			}
			mu.Lock()
			for k, v := range local {
				counts[k] += v
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	for _, h := range []string{"a.test", "b.test", "c.test"} {
		if counts[h] != 1000 {
			t.Fatalf("counts = %v, want 1000 each", counts)
		}
	}
}

// Run with -race: selection, health flips and status reads overlap.
func TestSelectionWhileHealthFlips(t *testing.T) {
	p := newPool(t, balancer.Options{InitiallyHealthy: true})
	b := p.Backends()[1]
	stop := make(chan struct{})
	var flipper sync.WaitGroup
	flipper.Add(1)
	go func() {
		defer flipper.Done()
		for {
			select {
			case <-stop:
				return
			default:
				b.RecordFailure(errors.New("flap"))
				b.RecordSuccess()
				_ = b.Status()
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				if got := p.Next(); got == nil {
					t.Error("A and C are always healthy; Next must not return nil")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	flipper.Wait()
}

func TestNewPoolValidation(t *testing.T) {
	cases := map[string][]string{
		"empty":      {},
		"no scheme":  {"localhost:8081"},
		"bad scheme": {"ftp://x"},
		"no host":    {"http://"},
		"duplicate":  {"http://a", "http://a"},
	}
	for name, urls := range cases {
		if _, err := balancer.NewPool(urls, balancer.Options{}, testutil.NewLogger()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
