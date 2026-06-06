package server

import (
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/metrics"
)

// fakeServerMetrics is a counting stand-in for the production expvar-backed
// observer, used to assert that a server/wshub code path bumped the right
// counter without scraping /api/debug/vars. Safe for concurrent use because
// the real call sites fire from request / goroutine paths.
type fakeServerMetrics struct {
	mu                    sync.Mutex
	panicRecovered        int
	wsAuthFail            int
	wsAuthFailRateLimited int
	wsAuthFailInvalid     int
}

func (f *fakeServerMetrics) PanicRecovered() {
	f.mu.Lock()
	f.panicRecovered++
	f.mu.Unlock()
}

func (f *fakeServerMetrics) WSAuthFail() {
	f.mu.Lock()
	f.wsAuthFail++
	f.mu.Unlock()
}

func (f *fakeServerMetrics) WSAuthFailRateLimited() {
	f.mu.Lock()
	f.wsAuthFailRateLimited++
	f.mu.Unlock()
}

func (f *fakeServerMetrics) WSAuthFailInvalidToken() {
	f.mu.Lock()
	f.wsAuthFailInvalid++
	f.mu.Unlock()
}

func (f *fakeServerMetrics) snapshot() (panicN, fail, rl, invalid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.panicRecovered, f.wsAuthFail, f.wsAuthFailRateLimited, f.wsAuthFailInvalid
}

// withServerMetrics swaps the package-level observer for the duration of a
// test and restores the prior value on cleanup. Tests MUST NOT run in parallel
// while using this — serverMetrics is process-global.
func withServerMetrics(t *testing.T, obs serverMetricsObserver) {
	t.Helper()
	prev := serverMetrics
	serverMetrics = obs
	t.Cleanup(func() { serverMetrics = prev })
}

// TestServerMetricsObserver_FakeRecordsEvents verifies the seam is injectable:
// calls routed through the package-level serverMetrics land on the swapped
// observer rather than the expvar globals. This is the behaviour the wshub /
// wsclient call sites rely on for #582.
func TestServerMetricsObserver_FakeRecordsEvents(t *testing.T) {
	fake := &fakeServerMetrics{}
	withServerMetrics(t, fake)

	serverMetrics.PanicRecovered()
	serverMetrics.PanicRecovered()
	serverMetrics.WSAuthFail()
	serverMetrics.WSAuthFailRateLimited()
	serverMetrics.WSAuthFailInvalidToken()

	panicN, fail, rl, invalid := fake.snapshot()
	if panicN != 2 {
		t.Errorf("PanicRecovered: got %d, want 2", panicN)
	}
	if fail != 1 {
		t.Errorf("WSAuthFail: got %d, want 1", fail)
	}
	if rl != 1 {
		t.Errorf("WSAuthFailRateLimited: got %d, want 1", rl)
	}
	if invalid != 1 {
		t.Errorf("WSAuthFailInvalidToken: got %d, want 1", invalid)
	}
}

// TestServerMetricsObserver_DefaultForwardsToExpvar verifies the production
// observer (expvarServerMetrics) actually moves the underlying expvar globals,
// so swapping to the seam did not silently sever the wiring to
// /api/debug/vars. Each assertion takes a before/after delta so the test does
// not assume a zero starting value (other tests in the package may have
// already bumped the process-global counters).
func TestServerMetricsObserver_DefaultForwardsToExpvar(t *testing.T) {
	// Exercise the production default explicitly even if another test left a
	// fake installed.
	withServerMetrics(t, expvarServerMetrics{})

	before := struct{ panicN, fail, rl, invalid int64 }{
		panicN:  metrics.PanicRecoveredTotal.Value(),
		fail:    metrics.WSAuthFailTotal.Value(),
		rl:      metrics.WSAuthFailRateLimitedTotal.Value(),
		invalid: metrics.WSAuthFailInvalidTokenTotal.Value(),
	}

	serverMetrics.PanicRecovered()
	serverMetrics.WSAuthFail()
	serverMetrics.WSAuthFailRateLimited()
	serverMetrics.WSAuthFailInvalidToken()

	if got := metrics.PanicRecoveredTotal.Value() - before.panicN; got != 1 {
		t.Errorf("PanicRecoveredTotal delta: got %d, want 1", got)
	}
	if got := metrics.WSAuthFailTotal.Value() - before.fail; got != 1 {
		t.Errorf("WSAuthFailTotal delta: got %d, want 1", got)
	}
	if got := metrics.WSAuthFailRateLimitedTotal.Value() - before.rl; got != 1 {
		t.Errorf("WSAuthFailRateLimitedTotal delta: got %d, want 1", got)
	}
	if got := metrics.WSAuthFailInvalidTokenTotal.Value() - before.invalid; got != 1 {
		t.Errorf("WSAuthFailInvalidTokenTotal delta: got %d, want 1", got)
	}
}
