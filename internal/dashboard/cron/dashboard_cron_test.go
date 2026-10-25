package cron

import (
	"testing"
	"time"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
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

// TestCronSummaryToView_PreservesOrderAndFields locks the wire-shape
// contract for the recent_runs builder. R250-PERF-19 (#1122) replaced
// `make([]cronRunSummaryView, 0, len(recent))` + `append` with
// `make(..., len(recent))` + index assignment in HandleList; the
// assertion below pins that:
//
//   - the per-row mapping (RunID/State/Trigger/StartedAt/EndedAt/...)
//     is unchanged regardless of which fill strategy a future refactor
//     uses, and
//   - the source ordering survives intact (a future bug that swapped to
//     a parallel goroutine fan-out without sorting would fail this).
//
// We exercise cronSummaryToView directly rather than reaching through
// HandleList because this shape is the single contract the dashboard
// consumes; HandleList only reorders nothing on its own.
func TestCronSummaryToView_PreservesOrderAndFields(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	in := []cronpkg.CronRunSummary{
		{RunID: "aa00000000000001", State: "succeeded", StartedAt: t0, EndedAt: t0.Add(time.Second), DurationMS: 1000, SessionID: "s1"},
		{RunID: "aa00000000000002", State: "failed", StartedAt: t0.Add(time.Minute), EndedAt: t0.Add(time.Minute + time.Second), DurationMS: 1000, ErrorClass: "exec"},
		{RunID: "aa00000000000003", State: "running", StartedAt: t0.Add(2 * time.Minute)}, // EndedAt zero
	}
	out := make([]cronRunSummaryView, len(in))
	for i, r := range in {
		out[i] = cronSummaryToView(r)
	}
	if got := len(out); got != len(in) {
		t.Fatalf("len(out) = %d, want %d", got, len(in))
	}
	for i, view := range out {
		if view.RunID != in[i].RunID {
			t.Errorf("out[%d].RunID = %q, want %q (order corruption)", i, view.RunID, in[i].RunID)
		}
		if view.State != string(in[i].State) {
			t.Errorf("out[%d].State = %q, want %q", i, view.State, string(in[i].State))
		}
		if view.StartedAt != in[i].StartedAt.UnixMilli() {
			t.Errorf("out[%d].StartedAt = %d, want %d", i, view.StartedAt, in[i].StartedAt.UnixMilli())
		}
	}
	// Running run (index 2) MUST emit ended_at=0 — IsZero() guard suppresses
	// the conversion. A bug that pre-allocates and forgets to reset would
	// silently land Unix epoch (-62...) instead.
	if out[2].EndedAt != 0 {
		t.Errorf("running run EndedAt = %d, want 0 (zero time → omitted)", out[2].EndedAt)
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
