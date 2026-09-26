package server

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// manualDebounceTimer stands in for the debouncer's *time.Timer: it fires
// only when the test says so. Stop reports whether it was still pending,
// exactly like time.Timer.Stop, which is the bit the wg accounting rests on.
type manualDebounceTimer struct {
	mu      sync.Mutex
	pending bool
	resets  int
}

func (m *manualDebounceTimer) Stop() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	was := m.pending
	m.pending = false
	return was
}

func (m *manualDebounceTimer) Reset(time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	was := m.pending
	m.pending = true
	m.resets++
	return was
}

// expire marks the timer as fired (Stop now reports false) and reports
// whether it had been pending; the caller then runs the callback.
func (m *manualDebounceTimer) expire() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	was := m.pending
	m.pending = false
	return was
}

func (m *manualDebounceTimer) resetCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resets
}

type debounceRig struct {
	d     *debouncer
	timer *manualDebounceTimer
	wg    *sync.WaitGroup
	fires atomic.Int32
	now   time.Time
}

func newDebounceRig() *debounceRig {
	r := &debounceRig{wg: &sync.WaitGroup{}, timer: &manualDebounceTimer{}, now: time.Unix(1_700_000_000, 0)}
	r.d = &debouncer{
		wg:       r.wg,
		fire:     func() { r.fires.Add(1) },
		interval: debounceInterval,
		maxDelay: maxDebounceDelay,
		now:      func() time.Time { return r.now },
		timer:    r.timer,
	}
	return r
}

// fireTimer runs the callback the way the runtime would once the timer fires.
func (r *debounceRig) fireTimer(t *testing.T) {
	t.Helper()
	if !r.timer.expire() {
		t.Fatal("timer fired while not pending")
	}
	r.d.onTimer()
}

// waitReturns requires the wg to be balanced: Wait returns promptly.
func waitReturns(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("wg.Wait did not return: a slot was taken and never released")
	}
}

func TestDebouncer_BurstCoalescesIntoOneFire(t *testing.T) {
	r := newDebounceRig()
	for i := 0; i < 5; i++ {
		r.d.trigger()
		r.now = r.now.Add(10 * time.Millisecond)
	}
	if got := r.fires.Load(); got != 0 {
		t.Fatalf("fired %d times before the timer did", got)
	}
	r.fireTimer(t)
	if got := r.fires.Load(); got != 1 {
		t.Errorf("fires = %d, want the burst coalesced into 1", got)
	}
	waitReturns(t, r.wg)

	// The window is closed again: the next trigger opens a fresh one.
	r.d.trigger()
	r.fireTimer(t)
	if got := r.fires.Load(); got != 2 {
		t.Errorf("fires = %d after a second window, want 2", got)
	}
	waitReturns(t, r.wg)
}

func TestDebouncer_HardCapStopsExtendingTheWindow(t *testing.T) {
	r := newDebounceRig()
	r.d.trigger() // opens the window: one Reset
	r.now = r.now.Add(maxDebounceDelay - time.Millisecond)
	r.d.trigger() // still inside the cap: extends
	extended := r.timer.resetCount()
	if extended != 2 {
		t.Fatalf("resets = %d inside the cap, want 2", extended)
	}
	r.now = r.now.Add(time.Millisecond)
	r.d.trigger() // at the cap: must not extend again
	if got := r.timer.resetCount(); got != extended {
		t.Errorf("a trigger at the %v cap extended the window (resets %d -> %d)", maxDebounceDelay, extended, got)
	}
	r.fireTimer(t)
	waitReturns(t, r.wg)
}

func TestDebouncer_CloseReleasesAPendingWindowAndStopsFiring(t *testing.T) {
	r := newDebounceRig()
	r.d.trigger()
	r.d.close()
	waitReturns(t, r.wg) // the cancelled window gave its slot back

	r.d.trigger()
	if got := r.timer.resetCount(); got != 1 {
		t.Errorf("a trigger after close re-armed the timer (resets = %d)", got)
	}
	waitReturns(t, r.wg) // and took no new slot
	if got := r.fires.Load(); got != 0 {
		t.Errorf("fired %d times across a close", got)
	}
}

// TestDebouncer_CallbackLateAfterCloseDoesNotFire: a timer that fired just
// before close still runs its callback afterwards; it releases its slot but
// must not broadcast past Shutdown.
func TestDebouncer_CallbackLateAfterCloseDoesNotFire(t *testing.T) {
	r := newDebounceRig()
	r.d.trigger()
	if !r.timer.expire() {
		t.Fatal("timer was not pending")
	}
	r.d.close() // Stop reports false: the slot stays with the callback
	r.d.onTimer()
	if got := r.fires.Load(); got != 0 {
		t.Errorf("a callback running after close fired %d times", got)
	}
	waitReturns(t, r.wg)
}

// TestDebouncer_CloseRacingARunningCallbackStaysBalanced: once the timer has
// fired, its callback owns the slot. Whichever of close and the callback takes
// the lock first, the slot is released exactly once — a second Done panics the
// WaitGroup, a missing one hangs Shutdown's Wait.
func TestDebouncer_CloseRacingARunningCallbackStaysBalanced(t *testing.T) {
	for i := 0; i < 200; i++ {
		r := newDebounceRig()
		r.d.trigger()
		if !r.timer.expire() { // fired: Stop now reports false
			t.Fatal("timer was not pending")
		}
		var running sync.WaitGroup
		running.Add(1)
		go func() {
			defer running.Done()
			r.d.onTimer()
		}()
		r.d.close()
		running.Wait()
		waitReturns(t, r.wg)
		if got := r.fires.Load(); got > 1 {
			t.Fatalf("fired %d times", got)
		}
	}
}

// TestDebouncer_TriggerWhileCallbackRunsDoesNotReschedule: a timer that has
// fired must not be Reset, or it would run a second time with no wg slot.
func TestDebouncer_TriggerWhileCallbackRunsDoesNotReschedule(t *testing.T) {
	r := newDebounceRig()
	r.d.trigger()
	if !r.timer.expire() {
		t.Fatal("timer was not pending")
	}
	before := r.timer.resetCount()
	r.d.trigger() // callback due but not yet run
	if got := r.timer.resetCount(); got != before {
		t.Errorf("a trigger after the timer fired re-armed it (resets %d -> %d)", before, got)
	}
	r.d.onTimer()
	waitReturns(t, r.wg)

	r.d.trigger() // the callback cleared armed: a new window opens
	if got := r.timer.resetCount(); got != before+1 {
		t.Errorf("resets = %d after the callback, want a fresh window (%d)", got, before+1)
	}
	r.fireTimer(t)
	waitReturns(t, r.wg)
}

// TestDebouncer_OpeningAWindowAllocatesNothing: the timer is allocated once and
// re-armed with Reset; sessions_update triggers are frequent.
func TestDebouncer_OpeningAWindowAllocatesNothing(t *testing.T) {
	var wg sync.WaitGroup
	d := newDebouncer(&wg, func() {})
	d.interval = time.Hour // never actually fires during the run
	allocs := testing.AllocsPerRun(200, func() {
		d.trigger()
		// Cancel the window by hand so the next run opens a fresh one.
		d.mu.Lock()
		if d.timer.Stop() {
			d.armed = false
			wg.Done()
		}
		d.mu.Unlock()
	})
	if allocs != 0 {
		t.Errorf("opening a debounce window allocates %.1f times, want 0", allocs)
	}
}

// TestDebouncer_RealTimerFires covers the production wiring the other tests
// replace: newDebouncer's own timer runs onTimer, which fires and releases
// the slot.
func TestDebouncer_RealTimerFires(t *testing.T) {
	var wg sync.WaitGroup
	fired := make(chan struct{}, 1)
	d := newDebouncer(&wg, func() { fired <- struct{}{} })
	d.interval = time.Millisecond
	d.trigger()
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the real timer never fired")
	}
	waitReturns(t, &wg)
}
