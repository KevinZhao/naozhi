package cron

import (
	"testing"
	"time"
)

// TestValidateSchedule covers the user syntax, the 5-minute interval floor and
// the location the interval probe runs in. The probe seeds from a fixed
// DST-quiet instant in loc (#965, #1321): seeded from time.Now() it measured a
// daily schedule across a spring-forward gap and judged it against the floor
// in a different frame than registerJob's WithLocation.
func TestValidateSchedule(t *testing.T) {
	t.Parallel()

	ny := mustLoadLocation(t, "America/New_York") // observes DST
	tests := []struct {
		name     string
		schedule string
		loc      *time.Location
		wantErr  bool
	}{
		{"every-30m", "@every 30m", time.UTC, false},
		{"daily", "@daily", time.UTC, false},
		{"hourly", "@hourly", time.UTC, false},
		{"weekdays-9am", "0 9 * * 1-5", time.UTC, false},
		{"every-5m-cron", "*/5 * * * *", time.UTC, false},
		// The seconds field is not part of the user syntax.
		{"six-field", "0 */5 * * * *", time.UTC, true},
		{"invalid", "invalid", time.UTC, true},
		{"empty", "", time.UTC, true},
		{"every-1h-local", "@every 1h", time.Local, false},
		{"every-1h-asia", "@every 1h", mustLoadLocation(t, "Asia/Shanghai"), false},
		// nil falls back to time.Local.
		{"nil-loc-fallback", "@every 1h", nil, false},
		{"too-fast-utc", "@every 30s", time.UTC, true},
		// 02:30 is exactly the US Eastern spring-forward gap.
		{"daily-0230-dst-zone", "30 2 * * *", ny, false},
		{"just-above-floor-dst-zone", "@every 6m", ny, false},
		{"just-below-floor-dst-zone", "@every 4m", ny, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateSchedule(tt.schedule, tt.loc)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSchedule(%q, %v): err=%v wantErr=%v", tt.schedule, tt.loc, err, tt.wantErr)
			}
		})
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
