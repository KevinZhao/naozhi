package cron

import (
	"testing"
	"time"
)

func TestHasMissedSchedule(t *testing.T) {
	t.Parallel()
	if missed, prev := HasMissedSchedule(nil, time.Now(), time.Time{}); missed || !prev.IsZero() {
		t.Fatalf("nil job = (%v, %v), want (false, zero)", missed, prev)
	}
	now := time.Date(2026, 5, 9, 9, 15, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	const day = 24 * time.Hour
	// startedAt is far enough back to clear the 5×period startup suppression
	// unless the case says otherwise.
	cases := []struct {
		name      string
		schedule  string
		created   time.Time
		lastRun   time.Time
		startedAt time.Time
		paused    bool
		want      bool
	}{
		{"unparsable schedule is never missed", "not-a-cron", ago(time.Hour), time.Time{}, ago(10 * time.Hour), false, false},
		{"startup suppression (5×period)", "@every 30m", ago(day), time.Time{}, ago(30 * time.Minute), false, false},
		{"recent run within period*1.5", "@every 30m", ago(day), ago(20 * time.Minute), ago(10 * time.Hour), false, false},
		{"stale run", "@every 30m", ago(day), ago(3 * time.Hour), ago(10 * time.Hour), false, true},
		{"never run, created long ago", "@every 30m", ago(5 * time.Hour), time.Time{}, ago(10 * time.Hour), false, true},
		// #1979: a paused job never had the chance to run.
		{"never run, paused", "@every 30m", ago(5 * time.Hour), time.Time{}, ago(10 * time.Hour), true, false},
		// The never-run branch uses the same period*3/2 slack as the ran branch,
		// so the first jitter window is not a false positive (CR-5).
		{"never run, period plus jitter", "@every 30m", ago(30*time.Minute + 30*time.Second), time.Time{}, ago(10 * time.Hour), false, false},
		{"never run, just under slack", "@every 30m", ago(45*time.Minute - time.Minute), time.Time{}, ago(10 * time.Hour), false, false},
		{"never run, beyond slack", "@every 30m", ago(45*time.Minute + time.Minute), time.Time{}, ago(10 * time.Hour), false, true},
		{"daily, ran at the last tick", "0 9 * * *", ago(30 * day), time.Date(2026, 5, 9, 9, 0, 0, 0, time.UTC), ago(8 * day), false, false},
		{"daily, stale by three days", "0 9 * * *", ago(30 * day), ago(3 * day), ago(8 * day), false, true},
		{"daily, startup suppression", "0 9 * * *", ago(30 * day), ago(10 * day), ago(15 * time.Minute), false, false},
	}
	for _, tc := range cases {
		j := &Job{Schedule: tc.schedule, CreatedAt: tc.created, LastRunAt: tc.lastRun, Paused: tc.paused}
		missed, prev := HasMissedSchedule(j, now, tc.startedAt)
		if missed != tc.want {
			t.Errorf("%s: missed = %v, want %v", tc.name, missed, tc.want)
			continue
		}
		// A missed run reports the tick it missed, between the last run and now.
		if missed && !tc.lastRun.IsZero() && (!prev.After(tc.lastRun) || !prev.Before(now)) {
			t.Errorf("%s: prev = %v, want strictly between lastRun %v and now %v", tc.name, prev, tc.lastRun, now)
		}
	}
}
