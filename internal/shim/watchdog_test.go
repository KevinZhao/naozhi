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

func TestWatchdog_StartTwiceIsSafe(t *testing.T) {
	var count atomic.Int32
	w, c := newManualWatchdog(50*time.Millisecond, func() { count.Add(1) })
	w.Start()
	fired := w.Fired()
	w.Start() // second call must be idempotent

	c.advance(50 * time.Millisecond)
	if got := count.Load(); got != 1 {
		t.Fatalf("expected 1 fire at the deadline, got %d", got)
	}
	// A second Start that re-armed would swap the channel out from under a
	// consumer that already holds it, and that consumer would never wake.
	if !isClosed(fired) {
		t.Error("the Fired() channel handed out before the second Start was never closed")
	}
	c.advance(time.Hour)
	if got := count.Load(); got != 1 {
		t.Errorf("expected exactly 1 fire, got %d", got)
	}
}

func TestWatchdog_StopTwiceIsSafe(t *testing.T) {
	var count atomic.Int32
	w, c := newManualWatchdog(50*time.Millisecond, func() { count.Add(1) })
	w.Start()
	w.Stop()
	w.Stop() // second Stop must be idempotent

	c.advance(time.Hour)
	if got := count.Load(); got != 0 {
		t.Errorf("expected 0 fires after Stop, got %d", got)
	}
}

func TestWatchdog_ResetWithoutStart_IsNoop(t *testing.T) {
	var count atomic.Int32
	w, c := newManualWatchdog(20*time.Millisecond, func() { count.Add(1) })
	w.Reset() // should be no-op since not running

	c.advance(time.Hour)
	if n := len(c.armed()); n != 0 {
		t.Errorf("Reset on a stopped watchdog armed %d timers", n)
	}
	if got := count.Load(); got != 0 {
		t.Errorf("expected 0 fires, got %d", got)
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

func TestWatchdog_FiredChannelNotClosedBeforeFire(t *testing.T) {
	w, c := newManualWatchdog(500*time.Millisecond, nil)
	w.Start()
	defer w.Stop()

	c.advance(500*time.Millisecond - time.Nanosecond)
	if isClosed(w.Fired()) {
		t.Fatal("Fired() closed before timeout")
	}
	c.advance(time.Nanosecond)
	if !isClosed(w.Fired()) {
		t.Fatal("Fired() still open at the deadline")
	}
}

func TestWatchdog_NilOnFire_DoesNotPanic(t *testing.T) {
	w, c := newManualWatchdog(20*time.Millisecond, nil)
	w.Start()

	c.advance(20 * time.Millisecond) // must not panic
	if !isClosed(w.Fired()) {
		t.Fatal("watchdog did not fire")
	}
}

func TestWatchdog_DefaultTimeout(t *testing.T) {
	// A zero timeout falls back to 30 minutes.
	var count atomic.Int32
	w, c := newManualWatchdog(0, func() { count.Add(1) })
	w.Start()
	defer w.Stop()

	if got := c.armed()[0].d; got != 30*time.Minute {
		t.Errorf("armed for %v, want the 30m default", got)
	}
	c.advance(30*time.Minute - time.Nanosecond)
	if count.Load() != 0 {
		t.Fatal("watchdog with default timeout fired early")
	}
	c.advance(time.Nanosecond)
	if got := count.Load(); got != 1 {
		t.Errorf("expected 1 fire at 30m, got %d", got)
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
	c.advance(50 * time.Millisecond)
	if got := count.Load(); got != 1 {
		t.Errorf("expected the second Start to fire once at its deadline, got %d", got)
	}
}

// TestWatchdog_ResetReplacesTimer verifies that Reset stops the previous timer
// so we do not accumulate idle runtime timers under high-frequency Reset.
func TestWatchdog_ResetReplacesTimer(t *testing.T) {
	w, _ := newManualWatchdog(50*time.Millisecond, nil)
	w.Start()
	t.Cleanup(w.Stop)

	w.mu.Lock()
	firstTimer := w.timer
	w.mu.Unlock()
	if firstTimer == nil {
		t.Fatal("Start should have allocated a timer")
	}

	w.Reset()

	w.mu.Lock()
	secondTimer := w.timer
	w.mu.Unlock()
	if secondTimer == nil {
		t.Fatal("Reset should have allocated a new timer")
	}
	if secondTimer == firstTimer {
		t.Fatal("Reset should have replaced the timer pointer")
	}
	// Stopping the original timer after Reset should already be a no-op.
	if firstTimer.Stop() {
		t.Error("first timer was still live after Reset — Reset did not Stop it")
	}
}

// TestWatchdog_StopClearsTimer verifies that Stop releases the timer pointer
// so the runtime does not retain a reference to the no-op callback.
func TestWatchdog_StopClearsTimer(t *testing.T) {
	w, _ := newManualWatchdog(50*time.Millisecond, nil)
	w.Start()
	w.Stop()

	w.mu.Lock()
	tm := w.timer
	w.mu.Unlock()
	if tm != nil {
		t.Error("Stop should have cleared the timer pointer")
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
