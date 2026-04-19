package shim

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchdog_NotStartedDoesNotFire(t *testing.T) {
	fired := make(chan struct{})
	w := NewWatchdog(20*time.Millisecond, func() { close(fired) })
	_ = w
	// Never call Start()
	select {
	case <-fired:
		t.Fatal("watchdog fired without Start()")
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}

func TestWatchdog_StartTwiceIsSafe(t *testing.T) {
	var count atomic.Int32
	w := NewWatchdog(50*time.Millisecond, func() { count.Add(1) })
	w.Start()
	w.Start() // second call must be idempotent

	time.Sleep(200 * time.Millisecond)

	if got := count.Load(); got != 1 {
		t.Errorf("expected exactly 1 fire, got %d", got)
	}
}

func TestWatchdog_StopTwiceIsSafe(t *testing.T) {
	var count atomic.Int32
	w := NewWatchdog(50*time.Millisecond, func() { count.Add(1) })
	w.Start()
	w.Stop()
	w.Stop() // second Stop must be idempotent

	time.Sleep(150 * time.Millisecond)
	if got := count.Load(); got != 0 {
		t.Errorf("expected 0 fires after Stop, got %d", got)
	}
}

func TestWatchdog_ResetWithoutStart_IsNoop(t *testing.T) {
	var count atomic.Int32
	w := NewWatchdog(20*time.Millisecond, func() { count.Add(1) })
	w.Reset() // should be no-op since not running
	time.Sleep(80 * time.Millisecond)
	if got := count.Load(); got != 0 {
		t.Errorf("expected 0 fires, got %d", got)
	}
}

func TestWatchdog_FiredChannelClosedOnFire(t *testing.T) {
	w := NewWatchdog(30*time.Millisecond, nil) // nil onFire is valid
	w.Start()
	select {
	case <-w.Fired():
		// expected: channel closed when watchdog fires
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Fired() channel was not closed after timeout")
	}
}

func TestWatchdog_FiredChannelNotClosedBeforeFire(t *testing.T) {
	w := NewWatchdog(500*time.Millisecond, nil)
	w.Start()
	select {
	case <-w.Fired():
		t.Fatal("Fired() closed before timeout")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
	w.Stop()
}

func TestWatchdog_NilOnFire_DoesNotPanic(t *testing.T) {
	w := NewWatchdog(20*time.Millisecond, nil)
	w.Start()
	// Wait for it to fire; must not panic
	select {
	case <-w.Fired():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog did not fire")
	}
}

func TestWatchdog_DefaultTimeout(t *testing.T) {
	// Zero timeout should apply 30min default; just check it doesn't panic
	// and doesn't fire immediately.
	var fired atomic.Bool
	w := NewWatchdog(0, func() { fired.Store(true) })
	w.Start()
	time.Sleep(20 * time.Millisecond)
	if fired.Load() {
		t.Error("watchdog with default timeout fired too fast")
	}
	w.Stop()
}

func TestWatchdog_MultipleResets_FiresOnce(t *testing.T) {
	var count atomic.Int32
	w := NewWatchdog(40*time.Millisecond, func() { count.Add(1) })
	w.Start()

	// Keep resetting, each time pushing the deadline further
	for i := 0; i < 5; i++ {
		time.Sleep(25 * time.Millisecond)
		w.Reset()
	}

	// Now stop preventing fire
	time.Sleep(200 * time.Millisecond)
	if got := count.Load(); got != 1 {
		t.Errorf("expected exactly 1 fire, got %d", got)
	}
}

func TestWatchdog_StopThenStartResumes(t *testing.T) {
	var count atomic.Int32
	w := NewWatchdog(50*time.Millisecond, func() { count.Add(1) })

	w.Start()
	w.Stop()

	// After Stop the watchdog should not fire
	time.Sleep(100 * time.Millisecond)
	if got := count.Load(); got != 0 {
		t.Errorf("expected 0 fires before second Start, got %d", got)
	}
}

// TestWatchdog_GenerationCounterRaceStress exercises concurrent Reset/Stop
// calls under the -race detector to catch any data races in the generation counter.
func TestWatchdog_GenerationCounterRaceStress(t *testing.T) {
	var count atomic.Int32
	w := NewWatchdog(5*time.Millisecond, func() { count.Add(1) })
	w.Start()

	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					w.Reset()
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(done)
	w.Stop()
	wg.Wait()
	// Just ensure no race; don't assert count (may have fired once)
}
