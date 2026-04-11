package shim

import (
	"log/slog"
	"sync"
	"time"
)

// Watchdog monitors CLI health during disconnect.
// When enabled, if no stdout line is pushed for the configured timeout,
// the fire callback is invoked (typically to SIGKILL the CLI).
type Watchdog struct {
	mu        sync.Mutex
	timeout   time.Duration
	timer     *time.Timer
	onFire    func()
	fired     chan struct{}
	firedOnce sync.Once
	running   bool
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

// Start enables the watchdog. Called when naozhi disconnects.
func (w *Watchdog) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return
	}
	w.running = true
	w.timer = time.AfterFunc(w.timeout, func() {
		slog.Warn("shim watchdog fired: no output timeout", "timeout", w.timeout)
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		w.firedOnce.Do(func() {
			close(w.fired)
			if w.onFire != nil {
				w.onFire()
			}
		})
	})
}

// Stop disables the watchdog. Called when naozhi reconnects.
func (w *Watchdog) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.running {
		return
	}
	w.running = false
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// Reset resets the watchdog timer. Called on each stdout line from CLI.
func (w *Watchdog) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.running || w.timer == nil {
		return
	}
	w.timer.Reset(w.timeout)
}

// Fired returns a channel that is closed when the watchdog fires.
func (w *Watchdog) Fired() <-chan struct{} {
	return w.fired
}
