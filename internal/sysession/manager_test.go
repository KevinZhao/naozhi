package sysession

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session"
)

// fakeRouter satisfies SystemSessionRouter for tests without dragging
// in the real router graph.
type fakeRouter struct {
	mu     sync.Mutex
	visits int
	labels map[string]struct {
		label  string
		origin string
	}
}

func newFakeRouter() *fakeRouter {
	return &fakeRouter{labels: make(map[string]struct {
		label  string
		origin string
	})}
}

func (f *fakeRouter) VisitSessions(fn func(session.SessionSnapshot) bool) {
	f.mu.Lock()
	f.visits++
	f.mu.Unlock()
	// No sessions to visit by default; tests that need data inject it.
}

func (f *fakeRouter) SetUserLabelWithOrigin(key, label, origin string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels[key] = struct {
		label  string
		origin string
	}{label, origin}
	return true
}

func (f *fakeRouter) ClearUserLabelOrigin(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.labels, key)
	return true
}

func (f *fakeRouter) RegisterSystemStub(_, _, _ string) {}

// fakeRunner returns canned text without exec'ing anything.
type fakeRunner struct {
	resp  string
	err   error
	calls atomic.Int32
}

func (f *fakeRunner) Run(ctx context.Context, _ string) (string, error) {
	f.calls.Add(1)
	if f.err != nil {
		return "", f.err
	}
	return f.resp, nil
}

// signalDaemon is a Daemon whose Tick is fully under test control.
type signalDaemon struct {
	name   string
	calls  atomic.Int32
	tickFn func(ctx context.Context, calls int32) (TickReport, error)
}

func (s *signalDaemon) Name() string        { return s.name }
func (s *signalDaemon) Description() string { return "test-only daemon" }
func (s *signalDaemon) Tick(ctx context.Context) (TickReport, error) {
	calls := s.calls.Add(1)
	if s.tickFn != nil {
		return s.tickFn(ctx, calls)
	}
	return TickReport{Acted: 1}, nil
}

// pulseTicker returns a tickerFactory whose channel is exposed so tests
// can drive ticks deterministically.  stop is a no-op (we don't simulate
// resource cleanup; we only care about correctness of run-once behaviour).
func pulseTicker() (chan time.Time, tickerFactory) {
	pulse := make(chan time.Time, 4)
	factory := func(_ time.Duration) (<-chan time.Time, func()) {
		return pulse, func() {}
	}
	return pulse, factory
}

// All Manager_* tests share the package-level builtinDaemons slice via
// withRegistry to inject test daemons.  We do NOT use t.Parallel() in
// this file so the sequential test runner is the canonical race-free
// guard — the registryTestMu inside withRegistry is belt+braces for
// callers that may add parallel subtests later.

func TestManager_DisabledIsNoOp(t *testing.T) {
	m, err := NewManager(Config{Enabled: false})
	if err != nil {
		t.Fatalf("NewManager err = %v", err)
	}
	// All methods must be safe even when disabled.
	m.Start(context.Background())
	m.Stop(context.Background())
	if got := m.Inspector(); got != nil {
		t.Errorf("disabled Inspector = %v, want nil", got)
	}
}

func TestManager_NewRequiresRouterWhenEnabled(t *testing.T) {
	// Pass through the same mutex other Manager tests use so the race
	// detector sees a happens-before edge with their builtinDaemons
	// swaps.  The replacement is identical to the original here — we
	// only want the lock acquire/release barrier.
	registryTestMu.Lock()
	defer registryTestMu.Unlock()
	_, err := NewManager(Config{Enabled: true})
	if err == nil {
		t.Error("expected error when Enabled=true but Router=nil")
	}
}

func TestManager_TickRunsAndRecords(t *testing.T) {
	pulse, tickFn := pulseTicker()

	// Replace the auto-titler factory with our signal daemon for the
	// duration of this test.  We do this by temporarily installing
	// our own one-entry registry — see the helper below.
	d := &signalDaemon{name: "auto-titler"}
	withRegistry(t, []builtinDaemonFactory{
		{Name: "auto-titler", Build: func(deps DaemonDeps) (Daemon, error) { return d, nil }},
	})

	router := newFakeRouter()
	m, err := NewManager(Config{
		Enabled:     true,
		TickTimeout: 100 * time.Millisecond,
		Router:      router,
		Daemons: map[string]DaemonRuntimeConfig{
			"auto-titler": {Enabled: true, Tick: 50 * time.Millisecond},
		},
		NewTicker: tickFn,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop(context.Background())

	// Drive one tick.  The runDaemonLoop performs an initial random
	// jitter wait first; tests that need determinism on the very first
	// tick want a tick interval small enough that the jitter is brief.
	// With Tick=50ms, jitter is in [0, 50ms).
	pulse <- time.Now()

	// Wait for tickFn to record a call.
	deadline := time.Now().Add(2 * time.Second)
	for d.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if d.calls.Load() == 0 {
		t.Fatal("daemon Tick was never invoked")
	}

	// Inspector should report at least one run after a brief wait
	// (recordRun runs in defer of runOnce).
	deadline = time.Now().Add(time.Second)
	var statuses []DaemonStatus
	for time.Now().Before(deadline) {
		statuses = m.Inspector()
		if len(statuses) > 0 && statuses[0].LastRun != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(statuses) != 1 {
		t.Fatalf("Inspector returned %d statuses, want 1", len(statuses))
	}
	if statuses[0].LastRun == nil {
		t.Fatal("LastRun was nil after first tick")
	}
	if statuses[0].LastRun.State != DaemonRunSucceeded {
		t.Errorf("LastRun.State = %q, want succeeded", statuses[0].LastRun.State)
	}
}

func TestManager_OverlappingTicksAreSkipped(t *testing.T) {
	pulse, tickFn := pulseTicker()

	// Daemon Tick blocks until released so we can stack a second tick
	// before the first finishes.  Counts how often it actually entered.
	release := make(chan struct{})
	d := &signalDaemon{
		name: "auto-titler",
		tickFn: func(ctx context.Context, _ int32) (TickReport, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return TickReport{}, ctx.Err()
			}
			return TickReport{}, nil
		},
	}
	withRegistry(t, []builtinDaemonFactory{
		{Name: "auto-titler", Build: func(deps DaemonDeps) (Daemon, error) { return d, nil }},
	})

	router := newFakeRouter()
	m, _ := NewManager(Config{
		Enabled:     true,
		TickTimeout: time.Second,
		Router:      router,
		Daemons: map[string]DaemonRuntimeConfig{
			"auto-titler": {Enabled: true, Tick: 1 * time.Millisecond},
		},
		NewTicker: tickFn,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop(context.Background())

	// Wait past the initial jitter so the first pulse is consumed.
	time.Sleep(20 * time.Millisecond)

	// First tick: starts running, blocks on release.
	pulse <- time.Now()
	// Wait for it to enter Tick.
	deadline := time.Now().Add(time.Second)
	for d.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if d.calls.Load() != 1 {
		t.Fatalf("first tick should have entered, calls = %d", d.calls.Load())
	}

	// Second tick while first is still in flight: must be skipped by
	// the CAS gate.
	pulse <- time.Now()
	time.Sleep(50 * time.Millisecond)
	if got := d.calls.Load(); got != 1 {
		t.Errorf("after overlapping pulse, calls = %d, want 1 (CAS gate must skip)", got)
	}

	close(release)

	// Drain runs cleanly so Stop doesn't hit timeout.
	deadline = time.Now().Add(time.Second)
	for m.daemons[0].inflight.Load() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
}

func TestManager_PanicRecoveredAndInflightReset(t *testing.T) {
	pulse, tickFn := pulseTicker()

	d := &signalDaemon{
		name: "auto-titler",
		tickFn: func(ctx context.Context, calls int32) (TickReport, error) {
			if calls == 1 {
				panic("kaboom")
			}
			return TickReport{}, nil
		},
	}
	withRegistry(t, []builtinDaemonFactory{
		{Name: "auto-titler", Build: func(deps DaemonDeps) (Daemon, error) { return d, nil }},
	})

	router := newFakeRouter()
	m, _ := NewManager(Config{
		Enabled:     true,
		TickTimeout: time.Second,
		Router:      router,
		Daemons: map[string]DaemonRuntimeConfig{
			"auto-titler": {Enabled: true, Tick: 1 * time.Millisecond},
		},
		NewTicker: tickFn,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop(context.Background())

	time.Sleep(20 * time.Millisecond) // past jitter
	pulse <- time.Now()
	// Wait for the panicking tick to complete.
	deadline := time.Now().Add(time.Second)
	for d.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	// Wait for inflight to reset (defer chain in runOnce).
	deadline = time.Now().Add(time.Second)
	for m.daemons[0].inflight.Load() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if m.daemons[0].inflight.Load() {
		t.Fatal("inflight stuck true after panic — defer ordering bug")
	}

	// Subsequent tick must succeed (proves the CAS gate is unblocked).
	pulse <- time.Now()
	deadline = time.Now().Add(time.Second)
	for d.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if d.calls.Load() < 2 {
		t.Errorf("second tick never ran, calls = %d", d.calls.Load())
	}
}

func TestManager_CircuitBreakerTripsOnConsecutiveCLIErrors(t *testing.T) {
	pulse, tickFn := pulseTicker()

	upstreamErr := errors.New("upstream went away")
	d := &signalDaemon{
		name: "auto-titler",
		tickFn: func(ctx context.Context, _ int32) (TickReport, error) {
			return TickReport{}, upstreamErr
		},
	}
	withRegistry(t, []builtinDaemonFactory{
		{Name: "auto-titler", Build: func(deps DaemonDeps) (Daemon, error) { return d, nil }},
	})

	router := newFakeRouter()
	m, _ := NewManager(Config{
		Enabled:     true,
		TickTimeout: time.Second,
		Router:      router,
		Daemons: map[string]DaemonRuntimeConfig{
			"auto-titler": {Enabled: true, Tick: 1 * time.Millisecond},
		},
		NewTicker: tickFn,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop(context.Background())

	time.Sleep(20 * time.Millisecond) // past jitter

	// Fire enough pulses to trip the breaker.
	for i := 0; i < consecutiveCLIFailureLimit; i++ {
		pulse <- time.Now()
		// Each tick is fast (just returns the error); allow it to
		// settle before the next pulse so they're truly sequential.
		deadline := time.Now().Add(500 * time.Millisecond)
		want := int32(i + 1)
		for d.calls.Load() < want && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
	}
	// Wait for breaker to trip (recordRun does the trip after tick returns).
	deadline := time.Now().Add(500 * time.Millisecond)
	for !m.daemons[0].disabled.Load() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !m.daemons[0].disabled.Load() {
		t.Errorf("breaker should have tripped after %d failures",
			consecutiveCLIFailureLimit)
	}

	// One more pulse — the breaker should make this a no-op.
	beforeCalls := d.calls.Load()
	pulse <- time.Now()
	time.Sleep(50 * time.Millisecond)
	if got := d.calls.Load(); got != beforeCalls {
		t.Errorf("disabled daemon called Tick again: before=%d after=%d", beforeCalls, got)
	}
}

func TestManager_ValidationDoesNotTripBreaker(t *testing.T) {
	pulse, tickFn := pulseTicker()

	d := &signalDaemon{
		name: "auto-titler",
		tickFn: func(ctx context.Context, _ int32) (TickReport, error) {
			return TickReport{}, ErrValidation
		},
	}
	withRegistry(t, []builtinDaemonFactory{
		{Name: "auto-titler", Build: func(deps DaemonDeps) (Daemon, error) { return d, nil }},
	})

	router := newFakeRouter()
	m, _ := NewManager(Config{
		Enabled:     true,
		TickTimeout: time.Second,
		Router:      router,
		Daemons: map[string]DaemonRuntimeConfig{
			"auto-titler": {Enabled: true, Tick: 1 * time.Millisecond},
		},
		NewTicker: tickFn,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop(context.Background())

	time.Sleep(20 * time.Millisecond)

	// Fire 2x the breaker limit; validation errors must NOT trip it.
	for i := 0; i < 2*consecutiveCLIFailureLimit; i++ {
		pulse <- time.Now()
		deadline := time.Now().Add(500 * time.Millisecond)
		want := int32(i + 1)
		for d.calls.Load() < want && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
	}
	if m.daemons[0].disabled.Load() {
		t.Error("validation errors should NOT trip breaker")
	}
	if got := m.daemons[0].consecutiveValidationFailures.Load(); got == 0 {
		t.Error("consecutiveValidationFailures should have grown")
	}
	if got := m.daemons[0].consecutiveCLIFailures.Load(); got != 0 {
		t.Errorf("consecutiveCLIFailures = %d, want 0 (validation must not bleed in)", got)
	}
}

// withRegistry temporarily replaces builtinDaemons with the supplied
// list and restores the original on test cleanup.  Used to inject test
// daemons without polluting other parallel tests' view (each parallel
// test gets its own t.Cleanup hook).
//
// SAFETY:  this mutates a package-level slice.  Tests using it cannot
// run in parallel WITH each other and must be careful when running
// alongside tests that read builtinDaemons indirectly (e.g.
// validateBuiltinDaemonNames).  We serialise via a package-private
// mutex and clean up in t.Cleanup.
var registryTestMu sync.Mutex

func withRegistry(t *testing.T, replacement []builtinDaemonFactory) {
	t.Helper()
	registryTestMu.Lock()
	original := builtinDaemons
	builtinDaemons = replacement
	t.Cleanup(func() {
		builtinDaemons = original
		registryTestMu.Unlock()
	})
}
