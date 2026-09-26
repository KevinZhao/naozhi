package server

import (
	"math"
	"sync"
	"time"
)

// Coalescing window for sessions_update broadcasts: a burst of triggers fires
// once debounceInterval after the last of them, but never later than
// maxDebounceDelay after the first.
const (
	debounceInterval = 50 * time.Millisecond
	maxDebounceDelay = 500 * time.Millisecond
)

// debounceTimer is the part of *time.Timer the debouncer uses.
type debounceTimer interface {
	Stop() bool
	Reset(d time.Duration) bool
}

// debouncer coalesces bursts of triggers into one call of fire. It owns its
// lock and every field under it; nothing outside this file reads them.
//
// Every pending fire holds one slot in wg (Add when a window opens, Done when
// the callback returns or when close cancels a timer that never fired), so a
// Shutdown that closes the debouncer and then Waits on wg also waits for a
// late-running fire. After close no new slot is taken.
type debouncer struct {
	wg       *sync.WaitGroup
	fire     func()
	interval time.Duration
	maxDelay time.Duration
	now      func() time.Time

	mu     sync.Mutex
	timer  debounceTimer
	armed  bool      // a window is open and holds a wg slot
	first  time.Time // first trigger of the open window
	closed bool
}

// newDebouncer builds a debouncer whose timer is allocated once, idle, and
// re-armed with Reset, so a trigger opening a window allocates nothing.
func newDebouncer(wg *sync.WaitGroup, fire func()) *debouncer {
	d := &debouncer{
		wg:       wg,
		fire:     fire,
		interval: debounceInterval,
		maxDelay: maxDebounceDelay,
		now:      time.Now,
	}
	// AfterFunc(MaxInt64) cannot fire before the Stop, so it starts idle.
	t := time.AfterFunc(time.Duration(math.MaxInt64), d.onTimer)
	t.Stop()
	d.timer = t
	return d
}

// trigger asks for a fire. It opens a window when none is open, extends an
// open one up to maxDelay, and does nothing once closed.
func (d *debouncer) trigger() {
	// Read the clock outside the critical section.
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	if d.armed {
		if now.Sub(d.first) >= d.maxDelay {
			// Hard cap: let the pending timer fire without extending it.
			return
		}
		// A timer whose callback already started (and is waiting on mu) must
		// not be Reset: that would schedule a second run with no wg slot. Stop
		// reports false in exactly that case, and the running callback clears
		// armed so the next trigger opens a fresh window.
		if d.timer.Stop() {
			d.timer.Reset(d.interval)
		}
		return
	}
	d.first = now
	d.wg.Add(1)
	d.armed = true
	d.timer.Reset(d.interval)
}

// onTimer is the timer callback: it releases the window's wg slot on every
// path, and fires unless the debouncer closed in the meantime.
func (d *debouncer) onTimer() {
	defer d.wg.Done()
	d.mu.Lock()
	d.armed = false
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return
	}
	d.fire()
}

// close stops all future fires. A window whose timer had not fired yet is
// cancelled and its wg slot released here; a callback already running
// releases its own slot, so the count stays balanced either way.
func (d *debouncer) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.armed {
		if d.timer.Stop() {
			d.wg.Done()
		}
		d.armed = false
	}
}
