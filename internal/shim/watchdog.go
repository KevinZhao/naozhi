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
//
// The fired channel is per-Start(): once a watchdog fires we keep the
// channel closed so the consumer observes it; on the next Start() a
// fresh channel is allocated so a later consumer can wait again (used
// by tests that reuse a Watchdog across scenarios; in production the
// shim exits after a fire so this reuse does not apply).
type Watchdog struct {
	mu      sync.Mutex
	timeout time.Duration
	onFire  func()
	fired   chan struct{}
	running bool
	gen     int64       // incremented on every Reset/Stop to invalidate old callbacks
	timer   *time.Timer // current in-flight AfterFunc; stopped on Reset/Stop so
	// high-frequency Reset does not leak runtime timers waiting their full duration.
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
// Stops the previous timer so the runtime does not hold idle timers; gen is
// still the correctness barrier against Stop() returning false for a callback
// already in flight.
// Must be called with w.mu held.
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
	// Snapshot the channel under lock so a concurrent Start() that reallocates
	// `w.fired` cannot make us double-close. The close is observable to the
	// consumer that was already waiting on this generation's channel.
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

// Start enables the watchdog. Called when naozhi disconnects.
// If the watchdog previously fired, a fresh `fired` channel is allocated so
// new consumers (e.g., the shim main loop after a reconnect scenario) do not
// immediately observe a stale closed channel.
func (w *Watchdog) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return
	}
	// Always allocate a fresh channel on Start. The previous select-based
	// branch that only replaced on observed close had a race: fireIfCurrent
	// snapshots `w.fired` under the lock and closes outside it, so Start()
	// could race the close and observe the old channel as still-open, skip
	// reallocation, and then Fired() would return a channel that
	// fireIfCurrent closes moments later — giving new consumers a
	// pre-closed handle for a superseded generation.
	//
	// It is safe to always reallocate because fireIfCurrent always closes
	// the channel it snapshotted, not the current `w.fired`.
	w.fired = make(chan struct{})
	// Bump the generation so any in-flight timer callback scheduled by a
	// previous Start (post-fire, pre-reallocation) sees a stale gen in
	// fireIfCurrent and exits without re-closing the fresh channel.
	// Without this, two timers with the same gen can be live simultaneously:
	// the stale one would match w.gen and close the *new* w.fired, giving
	// new consumers a spurious fire.
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
