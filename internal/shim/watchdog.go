package shim

import (
	"log/slog"
	"sync"
	"time"
)

// Watchdog monitors CLI health during disconnect: if no stdout line is pushed
// for the configured timeout, onFire is invoked (typically SIGKILL the CLI).
//
// Generation counter: each Reset/Stop increments gen so an AfterFunc callback
// already scheduled but not yet running sees a stale generation and exits
// (a timer whose Reset/Stop returned false cannot race the new timer).
//
// The fired channel is per-Start(): once fired it stays closed for the
// consumer; the next Start() allocates a fresh channel.
type Watchdog struct {
	mu      sync.Mutex
	timeout time.Duration
	onFire  func()
	fired   chan struct{}
	running bool
	gen     int64       // incremented on every Reset/Stop to invalidate old callbacks
	timer   *time.Timer // current AfterFunc; stopped on Reset/Stop so timers do not leak
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

// scheduleTimer arms a new AfterFunc bound to the current generation, stopping
// the previous timer so idle timers are not held; gen remains the correctness
// barrier against a callback already in flight. Must be called with w.mu held.
func (w *Watchdog) scheduleTimer() {
	if w.timer != nil {
		w.timer.Stop()
	}
	currentGen := w.gen
	w.timer = time.AfterFunc(w.timeout, func() {
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
	// Snapshot under lock so a concurrent Start() reallocating w.fired cannot
	// cause a double-close.
	ch := w.fired
	w.mu.Unlock()

	slog.Warn("shim watchdog fired: no output timeout", "timeout", w.timeout)
	select {
	case <-ch:
		// already closed by a prior fire for this channel instance
	default:
		close(ch)
	}
	if w.onFire != nil {
		w.onFire()
	}
}

// Start enables the watchdog (naozhi disconnected). A fresh fired channel is
// allocated so new consumers do not observe a stale closed one.
func (w *Watchdog) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return
	}
	// Always reallocate: fireIfCurrent closes the channel it snapshotted, not
	// the current w.fired, so this is safe and avoids racing an in-flight close.
	w.fired = make(chan struct{})
	// Bump gen so an in-flight callback from a previous Start cannot match and
	// close the *new* w.fired (spurious fire for new consumers).
	w.gen++
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
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// Reset resets the watchdog timer. Called on each stdout line from CLI.
func (w *Watchdog) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.running {
		return
	}
	w.gen++           // invalidate the previously scheduled callback
	w.scheduleTimer() // schedule a fresh one with the new generation
}

// Fired returns a channel that is closed when the watchdog fires.
// Callers must re-read via Fired() after a Start() if they want to observe
// the next fire event; the returned channel is tied to the current Start().
func (w *Watchdog) Fired() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}
