package shim

import (
	"log/slog"
	"sync"
	"time"
)

// Watchdog monitors CLI health during disconnect.
// When enabled, if no stdout line is pushed for the configured timeout,
// the fire callback is invoked (typically to SIGKILL the CLI).
//
// Generation counter: each Reset/Stop increments gen so that any
// AfterFunc callback that was already scheduled but not yet running
// will see a stale generation and exit without firing. This eliminates
// the race where time.Timer.Reset returns false (timer already expired)
// and the old callback fires concurrently with the new timer.
type Watchdog struct {
	mu        sync.Mutex
	timeout   time.Duration
	onFire    func()
	fired     chan struct{}
	firedOnce sync.Once
	running   bool
	gen       int64 // incremented on every Reset/Stop to invalidate old callbacks
}

// NewWatchdog creates a watchdog with the given no-output timeout.
// onFire is called (once) when the timeout expires.
func NewWatchdog(timeout time.Duration, onFire func()) *Watchdog {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	return &Watchdog{
		timeout: timeout,
		onFire:  onFire,
		fired:   make(chan struct{}),
	}
}

// scheduleTimer creates a new AfterFunc timer bound to the current generation.
// Must be called with w.mu held.
func (w *Watchdog) scheduleTimer() {
	currentGen := w.gen
	time.AfterFunc(w.timeout, func() {
		w.fireIfCurrent(currentGen)
	})
}

// fireIfCurrent executes the fire logic only if the generation still matches,
// i.e., no Reset or Stop has superseded this callback.
func (w *Watchdog) fireIfCurrent(g int64) {
	w.mu.Lock()
	if w.gen != g || !w.running {
		w.mu.Unlock()
		return
	}
	w.running = false
	w.mu.Unlock()

	slog.Warn("shim watchdog fired: no output timeout", "timeout", w.timeout)
	w.firedOnce.Do(func() {
		close(w.fired)
		if w.onFire != nil {
			w.onFire()
		}
	})
}

// Start enables the watchdog. Called when naozhi disconnects.
func (w *Watchdog) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return
	}
	w.running = true
	w.scheduleTimer()
}

// Stop disables the watchdog. Called when naozhi reconnects.
func (w *Watchdog) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.running {
		return
	}
	w.running = false
	w.gen++ // invalidate any in-flight callback
}

// Reset resets the watchdog timer. Called on each stdout line from CLI.
func (w *Watchdog) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.running {
		return
	}
	w.gen++          // invalidate the previously scheduled callback
	w.scheduleTimer() // schedule a fresh one with the new generation
}

// Fired returns a channel that is closed when the watchdog fires.
func (w *Watchdog) Fired() <-chan struct{} {
	return w.fired
}
