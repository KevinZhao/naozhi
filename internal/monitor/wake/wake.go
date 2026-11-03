// Package wake holds the clock the gateway's self-monitoring times against
// and detects sleep/wake jumps between its wall and monotonic readings
// (docs/rfc/planner.md §3.8, §15.1). It is a leaf so internal/monitor itself
// stays free for the ledger and health state machine, which import it and
// beat.
package wake

import "time"

// Clock is the time source for heartbeat and health timing. Mono stands still
// while the machine sleeps (Go's monotonic clock on darwin and linux does, and
// the runtime's timers run on it), so a sleep shows up as Now running ahead
// of Mono.
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

// Detector reports sleep/wake jumps between successive observations. Not safe
// for concurrent use; callers serialise Woke under their own lock.
type Detector struct {
	clock Clock
	skew  time.Duration // wall minus monotonic at the last observation
}

// NewDetector returns a detector whose baseline is the current instant.
func NewDetector(clock Clock) *Detector {
	d := &Detector{clock: clock}
	d.skew = d.observe()
	return d
}

func (d *Detector) observe() time.Duration {
	return time.Duration(d.clock.Now().UnixNano()) - d.clock.Mono()
}

// Woke reports whether wall and monotonic time drifted apart by more than
// SleepJump since the previous call, and moves the baseline to now. Timers
// measured against a baseline taken before the jump should restart from now.
func (d *Detector) Woke() bool {
	skew := d.observe()
	jump := skew - d.skew
	d.skew = skew
	return jump > SleepJump || jump < -SleepJump
}
