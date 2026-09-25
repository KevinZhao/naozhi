package shim

import (
	"sort"
	"sync"
	"time"
)

// manualClock stands in for Watchdog.afterFunc: a timer fires only when
// advance carries the clock across its deadline, so a test says "the deadline
// passed" instead of sleeping past it, and "nothing fired" is checked at an
// exact instant rather than after a guessed wait.
type manualClock struct {
	mu     sync.Mutex
	now    time.Duration
	timers []*manualTimer
}

type manualTimer struct {
	c        *manualClock
	d        time.Duration // duration the watchdog asked for
	deadline time.Duration
	f        func()
	done     bool // stopped or fired
}

func (t *manualTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	live := !t.done
	t.done = true
	return live
}

// newManualWatchdog builds a Watchdog whose timers run on a manualClock.
func newManualWatchdog(timeout time.Duration, onFire func()) (*Watchdog, *manualClock) {
	c := &manualClock{}
	w := NewWatchdog(timeout, onFire)
	w.afterFunc = c.afterFunc
	return w, c
}

func (c *manualClock) afterFunc(d time.Duration, f func()) watchdogTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualTimer{c: c, d: d, deadline: c.now + d, f: f}
	c.timers = append(c.timers, t)
	return t
}

// advance moves the clock forward and runs every live timer it crosses, in
// deadline order, on the calling goroutine — the state a test would observe
// once time.AfterFunc's goroutine had run.
func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now += d
	var due []*manualTimer
	for _, t := range c.timers {
		if !t.done && t.deadline <= c.now {
			t.done = true
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	sort.SliceStable(due, func(i, j int) bool { return due[i].deadline < due[j].deadline })
	for _, t := range due {
		t.f()
	}
}

// armed returns every timer created so far, live or not, in creation order.
func (c *manualClock) armed() []*manualTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*manualTimer(nil), c.timers...)
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
