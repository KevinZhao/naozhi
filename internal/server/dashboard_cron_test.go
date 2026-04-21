package server

import (
	"testing"
	"time"
)

func TestFormatTZOffset(t *testing.T) {
	tests := []struct {
		name   string
		tz     string
		offset int
		want   string
	}{
		{"UTC", "UTC", 0, "UTC (UTC+00:00)"},
		{"positive_whole_hour", "Asia/Shanghai", 8 * 3600, "Asia/Shanghai (UTC+08:00)"},
		{"positive_half_hour", "Asia/Kolkata", 5*3600 + 30*60, "Asia/Kolkata (UTC+05:30)"},
		{"positive_quarter_hour", "Asia/Kathmandu", 5*3600 + 45*60, "Asia/Kathmandu (UTC+05:45)"},
		{"negative_whole_hour", "America/New_York", -5 * 3600, "America/New_York (UTC-05:00)"},
		// Regression: negative fractional offsets used to render "UTC-03:-30"
		// because the integer-mod minute component inherited the negative sign.
		{"negative_half_hour", "America/St_Johns", -(3*3600 + 30*60), "America/St_Johns (UTC-03:30)"},
		{"negative_quarter_hour", "Pacific/Marquesas", -(9*3600 + 30*60), "Pacific/Marquesas (UTC-09:30)"},
		{"positive_near_dateline", "Pacific/Kiritimati", 14 * 3600, "Pacific/Kiritimati (UTC+14:00)"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := formatTZOffset(tc.tz, tc.offset); got != tc.want {
				t.Fatalf("formatTZOffset(%q, %d) = %q, want %q", tc.tz, tc.offset, got, tc.want)
			}
		})
	}
}

// TestFormatTZOffset_MatchesStdlib verifies the helper agrees with time.Zone()
// for a live half-hour zone, so locale database changes cannot regress the
// output format without a test failure.
func TestFormatTZOffset_MatchesStdlib(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skipf("zone unavailable: %v", err)
	}
	_, offset := time.Now().In(loc).Zone()
	got := formatTZOffset(loc.String(), offset)
	want := "Asia/Kolkata (UTC+05:30)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
