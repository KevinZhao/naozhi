// scheduler_jobs_preview.go: schedule-preview / timezone helpers.
//
// Holds the pure-compute preview cluster split out of scheduler_jobs.go:
// previewLocation, PreviewSchedule, PreviewScheduleN, and Location. These
// helpers do ZERO locking (no s.mu / jobLock / entry.mu) — they only read
// the already-decided Scheduler.location field and parse cron expressions
// — so they live apart from the CRUD mutation path.
//
// No behaviour change. Methods stay on *Scheduler so private fields remain
// accessible without exporting.

package cron

import (
	"time"
)

// previewLocation returns the timezone the preview helpers should evaluate
// schedules in. Centralised so the nil-Scheduler fallback (tests / dashboard
// bootstrap before scheduler wiring) and the live scheduler path share a
// single decision point. R219-CR-6.
//
//   - nil receiver → UTC (deterministic across machines, matches the godoc
//     contract historically published on the package-level PreviewSchedule)
//   - non-nil receiver with unset location → time.Local (legacy behaviour
//     when location was never configured; preserved to avoid subtle drift
//     in operator-facing tooling)
//   - configured location wins
func (s *Scheduler) previewLocation() *time.Location {
	if s == nil {
		return time.UTC
	}
	if s.location == nil {
		return time.Local
	}
	return s.location
}

// PreviewSchedule validates a schedule expression and returns the next run
// time. Safe to call on a nil *Scheduler — that path computes in UTC for
// tests / dashboard bootstrap before the scheduler is wired. R219-CR-6:
// previously a free-standing cron.PreviewSchedule existed for this nil
// fallback, and operators had to remember which surface to call; folded
// into the method so callers always invoke (*Scheduler).PreviewSchedule.
func (s *Scheduler) PreviewSchedule(schedule string) (time.Time, error) {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(time.Now().In(s.previewLocation())), nil
}

// PreviewScheduleN returns the next n run times for a schedule expression, in
// the scheduler's configured timezone. Used by the dashboard to preview what
// "接下来会在这些时间运行" looks like before a user commits to a frequency.
// Callers get a validation error on the first Parse failure; n is clamped to
// a sane range by the caller.
//
// Safe to call on a nil *Scheduler — same fallback as PreviewSchedule
// (UTC). R219-CR-6.
func (s *Scheduler) PreviewScheduleN(schedule string, n int) ([]time.Time, error) {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		n = 1
	}
	out := make([]time.Time, 0, n)
	t := time.Now().In(s.previewLocation())
	for i := 0; i < n; i++ {
		t = sched.Next(t)
		out = append(out, t)
	}
	return out, nil
}

// Location returns the timezone the scheduler uses to evaluate cron
// expressions, so the dashboard can surface it alongside preview/next-run
// timestamps.
//
// Safe to call on a nil *Scheduler: returns UTC (matches previewLocation's
// nil branch so dashboard preview / Location calls stay in agreement during
// the bootstrap window when scheduler is not yet wired). R219-CR-6.
//
// #835 (dup-code): the resolution (nil→UTC, unset→Local, else configured)
// was previously open-coded identically to previewLocation, so a future
// change to the nil/unset fallback policy had to be made in two places or
// the two timezone-decision points would silently diverge — exactly the
// "dashboard preview / Location stay in agreement" invariant this godoc
// promises. Delegate to the single source of truth.
func (s *Scheduler) Location() *time.Location {
	return s.previewLocation()
}
