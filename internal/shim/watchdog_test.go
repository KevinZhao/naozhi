package shim

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchdog_NotStartedDoesNotFire(t *testing.T) {
	var count atomic.Int32
	w, c := newManualWatchdog(20*time.Millisecond, func() { count.Add(1) })

	c.advance(time.Hour)

	if n := len(c.armed()); n != 0 {
		t.Errorf("an unstarted watchdog armed %d timers", n)
	}
	if count.Load() != 0 || isClosed(w.Fired()) {
		t.Fatal("watchdog fired without Start()")
	}
}

func TestWatchdog_FiredChannelClosedOnFire(t *testing.T) {
	w, c := newManualWatchdog(30*time.Millisecond, nil) // nil onFire is valid
	w.Start()

	c.advance(30 * time.Millisecond)
	if !isClosed(w.Fired()) {
		t.Fatal("Fired() channel was not closed after timeout")
	}
}

func TestWatchdog_MultipleResets_FiresOnce(t *testing.T) {
	var count atomic.Int32
	w, c := newManualWatchdog(40*time.Millisecond, func() { count.Add(1) })
	w.Start()

	// Five resets 25ms apart: 125ms in total, well past a single 40ms
	// deadline, so only the resets keep it from firing.
	for i := 0; i < 5; i++ {
		c.advance(25 * time.Millisecond)
		w.Reset()
	}
	if got := count.Load(); got != 0 {
		t.Fatalf("fired %d times while resets kept arriving", got)
	}

	c.advance(40*time.Millisecond - time.Nanosecond)
	if count.Load() != 0 {
		t.Fatal("fired before 40ms had passed since the last Reset")
	}
	c.advance(time.Nanosecond)
	c.advance(time.Hour)
	if got := count.Load(); got != 1 {
		t.Errorf("expected exactly 1 fire, got %d", got)
	}
}

func TestWatchdog_StopThenStartResumes(t *testing.T) {
	var count atomic.Int32
	w, c := newManualWatchdog(50*time.Millisecond, func() { count.Add(1) })

	w.Start()
	w.Stop()
	c.advance(time.Hour)
	if got := count.Load(); got != 0 {
		t.Fatalf("expected 0 fires before second Start, got %d", got)
	}

	w.Start()
	fired := w.Fired()
	w.Start() // Start while running must not re-arm or swap the Fired() channel
	c.advance(50 * time.Millisecond)
	if got := count.Load(); got != 1 {
		t.Errorf("expected the second Start to fire once at its deadline, got %d", got)
	}
	if !isClosed(fired) {
		t.Error("the Fired() channel handed out before a repeated Start was never closed")
	}
}

// TestWatchdog_RealClockFires covers the production afterFunc every other
// test swaps out: NewWatchdog must arm a real timer for the configured timeout.
func TestWatchdog_RealClockFires(t *testing.T) {
	w := NewWatchdog(10*time.Millisecond, nil)
	start := time.Now()
	w.Start()

	select {
	case <-w.Fired():
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog on the real clock never fired")
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Errorf("fired after %v, before the 10ms timeout", elapsed)
	}
}

// TestWatchdog_GenerationCounterRaceStress runs Reset, Stop and Start from
// several goroutines against real timers short enough to fire mid-loop, so
// -race sees callbacks overlapping every entry point.
func TestWatchdog_GenerationCounterRaceStress(t *testing.T) {
	w := NewWatchdog(50*time.Microsecond, nil)
	w.Start()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				w.Reset()
				if j%200 == 0 {
					w.Stop()
					w.Start()
				}
			}
		}()
	}
	wg.Wait()
	w.Stop()
}

func TestWatchdog_FiresOnTimeout(t *testing.T) {
	fired := make(chan struct{})
	w, c := newManualWatchdog(50*time.Millisecond, func() {
		close(fired)
	})
	w.Start()

	c.advance(50 * time.Millisecond)
	if !isClosed(fired) {
		t.Fatal("watchdog did not fire at its deadline")
	}
}

func TestWatchdog_ResetPrevents(t *testing.T) {
	fired := make(chan struct{})
	w, c := newManualWatchdog(100*time.Millisecond, func() {
		close(fired)
	})
	w.Start()
	defer w.Stop()

	// Reset before timeout
	c.advance(50 * time.Millisecond)
	w.Reset()
	c.advance(50 * time.Millisecond)
	w.Reset()

	// The deadline now sits 100ms after the second Reset.
	c.advance(100*time.Millisecond - time.Nanosecond)
	if isClosed(fired) {
		t.Fatal("watchdog fired despite resets")
	}
	c.advance(time.Nanosecond)
	if !isClosed(fired) {
		t.Fatal("watchdog did not fire 100ms after the last Reset")
	}
}

func TestWatchdog_StopPrevents(t *testing.T) {
	fired := make(chan struct{})
	w, c := newManualWatchdog(50*time.Millisecond, func() {
		close(fired)
	})
	w.Start()
	w.Stop()

	c.advance(time.Hour)
	if isClosed(fired) {
		t.Fatal("watchdog fired after Stop()")
	}
}

// TestWatchdog_ResetStaleCallbackNoOp pins the generation counter: a callback
// whose timer was already dispatched when Reset ran (so Reset's Stop could not
// cancel it) must be a no-op, and only the fresh generation fires.
func TestWatchdog_ResetStaleCallbackNoOp(t *testing.T) {
	var fireCount atomic.Int32
	w, c := newManualWatchdog(30*time.Millisecond, func() {
		fireCount.Add(1)
	})
	w.Start()

	c.advance(20 * time.Millisecond)
	stale := c.armed()[0]
	w.Reset()
	// Run the superseded callback as the AfterFunc goroutine would if it had
	// already been dispatched when Reset stopped the timer.
	stale.f()
	if got := fireCount.Load(); got != 0 {
		t.Fatalf("the superseded callback fired (%d), generation counter did not block it", got)
	}

	c.advance(30 * time.Millisecond)
	if got := fireCount.Load(); got != 1 {
		t.Errorf("expected watchdog to fire exactly once, got %d", got)
	}
}

// TestWatchdog_StopInvalidatesCallback: the same race against Stop — a
// callback already dispatched when Stop ran must not fire.
func TestWatchdog_StopInvalidatesCallback(t *testing.T) {
	var fireCount atomic.Int32
	w, c := newManualWatchdog(20*time.Millisecond, func() {
		fireCount.Add(1)
	})
	w.Start()

	c.advance(10 * time.Millisecond)
	stale := c.armed()[0]
	w.Stop()
	stale.f()

	if got := fireCount.Load(); got != 0 {
		t.Errorf("expected watchdog not to fire after Stop, got %d fires", got)
	}
}
