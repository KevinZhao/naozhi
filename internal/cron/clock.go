// clock.go: minimal Clock abstraction for cron lifecycle timestamps.
//
// R247-ARCH-11 / R245-ARCH-34 (#643): the scheduler reads time.Now() directly
// at ~172 sites across the package, which forces time-relevant tests to sleep
// for real wall-clock durations to observe e.g. run DurationMS or skipped-run
// EndedAt. This file introduces the smallest possible injection point — a
// cronClock interface with a real-time default — and routes the run-finish
// lifecycle path (finishRun's endedAt + the synthetic-skipped startedAt)
// through it. That is the single most test-relevant cluster: DurationMS and
// the started→ended event pair are computed here, so a fake clock lets a test
// pin a deterministic duration without sleeping.
//
// Scope is deliberately narrow: only the finish path is converted in this
// change so the diff stays reviewable and behaviour-preserving (the default
// clock calls time.Now(), byte-identical to the prior inline reads). The
// remaining sites (scheduler tick, jitter, spawn budget, runstore trim) can
// migrate incrementally onto the same interface later — the field and the
// default are now in place for them to adopt.
package cron

import "time"

// cronClock is the time source the scheduler reads for lifecycle timestamps.
// Production wiring uses realClock (time.Now); tests inject a fake to pin a
// deterministic now without sleeping.
type cronClock interface {
	Now() time.Time
}

// realClock is the production time source: a thin time.Now() wrapper with no
// state, so a single shared value is safe to reuse across all schedulers.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// defaultClock is the shared real-time clock installed by NewScheduler when no
// override is supplied. Stateless, so one value serves every scheduler.
var defaultClock cronClock = realClock{}

// now returns the scheduler's current time via its injected clock, falling
// back to wall-clock time when the clock was never wired (defensive: a Scheduler
// built through a non-constructor path or a zero value still reads real time
// rather than nil-panicking).
func (s *Scheduler) now() time.Time {
	if s == nil || s.clock == nil {
		return time.Now()
	}
	return s.clock.Now()
}
