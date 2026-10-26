// scheduler_jobs_preview.go: timezone helpers (previewLocation, Location).
// These do ZERO locking — they only read Scheduler.location.

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

// Location returns the timezone the scheduler uses to evaluate cron
// expressions, so the dashboard can surface it alongside next-run
// timestamps. Delegates to previewLocation so the nil→UTC / unset→Local
// policy has a single source of truth (#835).
func (s *Scheduler) Location() *time.Location {
	return s.previewLocation()
}
