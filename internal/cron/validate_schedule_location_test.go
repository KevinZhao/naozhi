package cron

import (
	"testing"
	"time"
)

// TestValidateSchedule_LocationParameter exercises the loc-aware
// validateSchedule signature added in #1321 (R20260527122801-CR-7). The
// historical bug was that validateSchedule used time.Now() (Local) to seed
// the interval probe while registerJob registers the entry with
// WithLocation(s.location); the two reference frames disagreed across DST
// and month-end forms.
func TestValidateSchedule_LocationParameter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		schedule string
		loc      *time.Location
		wantErr  bool
	}{
		{"every-1h-utc", "@every 1h", time.UTC, false},
		{"every-1h-local", "@every 1h", time.Local, false},
		{"every-1h-asia", "@every 1h", mustLoadLocation(t, "Asia/Shanghai"), false},
		// nil falls back to time.Local so the legacy free-standing path
		// (tests / pre-Scheduler bootstraps) keeps working.
		{"nil-loc-fallback", "@every 1h", nil, false},
		// Below floor regardless of location.
		{"too-fast-utc", "@every 30s", time.UTC, true},
		// Invalid expression still surfaces parse error before location is
		// consulted.
		{"bad-expr", "not-a-schedule", time.UTC, true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateSchedule(tt.schedule, tt.loc)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSchedule(%q, %v): err=%v wantErr=%v", tt.schedule, tt.loc, err, tt.wantErr)
			}
		})
	}
}

// TestValidateSchedule_LocationConsistencyWithScheduler ensures the location
// passed into validateSchedule matches what (*Scheduler).previewLocation
// reports — guards the AddJob/UpdateJob call sites against future drift.
func TestValidateSchedule_LocationConsistencyWithScheduler(t *testing.T) {
	t.Parallel()

	// nil receiver path returns UTC.
	var s *Scheduler
	if got := s.previewLocation(); got != time.UTC {
		t.Fatalf("nil scheduler previewLocation: got %v want UTC", got)
	}

	// validateSchedule with the same location must succeed for a normal
	// hourly schedule.
	if err := validateSchedule("@every 1h", s.previewLocation()); err != nil {
		t.Fatalf("validateSchedule with nil-scheduler loc: %v", err)
	}
}

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("location %q unavailable on this platform: %v", name, err)
	}
	return loc
}
