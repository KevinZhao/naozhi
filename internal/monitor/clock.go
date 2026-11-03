// Package monitor holds the gateway's self-monitoring building blocks: a
// clock that exposes wall and monotonic time separately, and a detector for
// the sleep/wake jumps between them (docs/rfc/planner.md §3.8, §15.1).
package monitor

import "time"

// Clock is the time source for heartbeat and health timing. Mono stands still
// while the machine sleeps (Go's monotonic clock on darwin and linux does), so
// a sleep shows up as Now running ahead of Mono.
type Clock interface {
	Now() time.Time
	Mono() time.Duration
}

// SystemClock reads the process clocks; Mono counts from the call.
func SystemClock() Clock { return systemClock{origin: time.Now()} }

type systemClock struct{ origin time.Time }

func (systemClock) Now() time.Time        { return time.Now() }
func (c systemClock) Mono() time.Duration { return time.Since(c.origin) }

// SleepJump is how far wall time may move against monotonic time between two
// observations before the gap counts as one sleep/wake (or a wall-clock step).
const SleepJump = 2 * time.Minute

// WakeDetector reports sleep/wake jumps between successive observations. Not
// safe for concurrent use; callers serialise Woke under their own lock.
type WakeDetector struct {
	clock Clock
	skew  time.Duration // wall minus monotonic at the last observation
}

// NewWakeDetector returns a detector whose baseline is the current instant.
func NewWakeDetector(clock Clock) *WakeDetector {
	d := &WakeDetector{clock: clock}
	d.skew = d.observe()
	return d
}

func (d *WakeDetector) observe() time.Duration {
	return time.Duration(d.clock.Now().UnixNano()) - d.clock.Mono()
}

// Woke reports whether wall and monotonic time drifted apart by more than
// SleepJump since the previous call, and moves the baseline to now. Timers
// measured against a baseline taken before the jump should restart from now.
func (d *WakeDetector) Woke() bool {
	skew := d.observe()
	jump := skew - d.skew
	d.skew = skew
	return jump > SleepJump || jump < -SleepJump
}
