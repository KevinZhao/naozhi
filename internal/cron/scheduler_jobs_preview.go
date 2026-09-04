// scheduler_jobs_preview.go: schedule-preview / timezone helpers
// (previewLocation, PreviewSchedule, PreviewScheduleN, Location). These do
// ZERO locking — they only read Scheduler.location and parse cron expressions.

package cron

import (
	"time"
)

// previewLocation returns the timezone the preview helpers evaluate schedules
// in — the single decision point shared by the nil-Scheduler fallback and the
// live path: nil receiver → UTC (deterministic across machines); non-nil with
// unset location → time.Local (legacy, preserved to avoid drift in
// operator-facing tooling); configured location wins.
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
// time. Safe on a nil *Scheduler (computes in UTC for tests / dashboard
// bootstrap before the scheduler is wired).
func (s *Scheduler) PreviewSchedule(schedule string) (time.Time, error) {
	sched, err := cronParser.Parse(schedule)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(time.Now().In(s.previewLocation())), nil
}

// PreviewScheduleN returns the next n run times for a schedule expression, in
// the scheduler's configured timezone, for the dashboard's "接下来会在这些
// 时间运行" preview. Parse failure returns a validation error; n is clamped to
// a sane range by the caller. Safe on a nil *Scheduler (UTC).
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
// timestamps. Delegates to previewLocation so the nil→UTC / unset→Local
// policy has a single source of truth and preview and Location never diverge
// (#835).
func (s *Scheduler) Location() *time.Location {
	return s.previewLocation()
}
