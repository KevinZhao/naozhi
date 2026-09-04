// clock.go: minimal Clock abstraction for cron lifecycle timestamps (#643).
// Only the run-finish path (finishRun's endedAt + the synthetic-skipped
// startedAt) reads through it, so a fake clock pins DurationMS without
// sleeping; other time.Now() sites may migrate onto it incrementally.
package cron

import "time"

// cronClock is the time source the scheduler reads for lifecycle timestamps.
// Production wiring uses realClock (time.Now); tests inject a fake to pin a
// deterministic now without sleeping.
type cronClock interface {
	Now() time.Time
}

// realClock is the production time source: a stateless time.Now() wrapper.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// defaultClock is the shared real-time clock installed by NewScheduler.
var defaultClock cronClock = realClock{}

// now returns the scheduler's current time via its injected clock, falling
// back to wall-clock time when the clock was never wired (zero-value
// Scheduler) rather than nil-panicking.
func (s *Scheduler) now() time.Time {
	if s == nil || s.clock == nil {
		return time.Now()
	}
	return s.clock.Now()
}
