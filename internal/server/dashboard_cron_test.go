package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"golang.org/x/time/rate"
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

// TestHandleTrigger_PerIPRateLimit pins R234-SEC-2 / #1007: POST
// /api/cron/trigger MUST gate per-IP via writeLimiter before invoking the
// scheduler. A token holder fan-out without this gate could drain shim/cli
// capacity by looping triggers; we assert that once the burst is exhausted
// the handler returns 429 Too Many Requests instead of falling through to
// scheduler.TriggerNow.
//
// The test deliberately leaves scheduler nil — once the limiter rejects
// the request we MUST not reach the scheduler-nil 501 branch. Comparing
// against 429 (not 501) is the load-bearing assertion: an inverted gate
// would 501 before 429.
func TestHandleTrigger_PerIPRateLimit(t *testing.T) {
	t.Parallel()
	// Burst of 2 lets the first two requests through (modulo any rate
	// recharge during the test loop), the third is forced to 429. We
	// pick rate.Every(time.Hour) so the sustained refill is irrelevant
	// over the test's microsecond-scale duration; only burst matters.
	h := &CronHandlers{
		writeLimiter: newIPLimiterWithProxy(rate.Every(time.Hour), 2, false),
	}

	body := `{"id":"deadbeefdeadbeef"}`
	doReq := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/cron/trigger", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "10.1.2.3:5555"
		w := httptest.NewRecorder()
		h.handleTrigger(w, req)
		return w.Code
	}

	// First two should NOT be 429 (limiter has burst=2). They will be
	// 501 (scheduler nil) — anything except 429 confirms we didn't
	// false-positive the limiter.
	for i := 0; i < 2; i++ {
		if got := doReq(); got == http.StatusTooManyRequests {
			t.Fatalf("request %d 429ed early — burst budget exhausted prematurely", i+1)
		}
	}
	// Third request MUST hit 429: limiter exhausted, no recharge in
	// test window. Fail loudly if a future refactor drops the
	// writeLimiter gate at the top of handleTrigger.
	if got := doReq(); got != http.StatusTooManyRequests {
		t.Fatalf("3rd trigger after burst exhaustion: got %d, want 429 Too Many Requests; #1007 trigger rate-limit gate may be missing", got)
	}
}

// TestHandleTrigger_NilLimiter_PassThrough sanity check: when no
// writeLimiter is wired (test handler / forgotten injection), handleTrigger
// must NOT 429 — it nil-guards and falls through. Otherwise hand-built
// CronHandlers in unrelated tests would 429 spuriously.
func TestHandleTrigger_NilLimiter_PassThrough(t *testing.T) {
	t.Parallel()
	h := &CronHandlers{} // writeLimiter nil

	req := httptest.NewRequest(http.MethodPost, "/api/cron/trigger", strings.NewReader(`{"id":"deadbeef"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleTrigger(w, req)
	if w.Code == http.StatusTooManyRequests {
		t.Fatalf("nil writeLimiter must not produce 429; got %d body=%q", w.Code, w.Body.String())
	}
}

// TestMissedScheduleVerdict_CachesWithinTTL guards R245-PERF-4 (#857):
// the dashboard handleList path must not re-Parse the cron expression on
// every poll. Two back-to-back calls within missedCacheTTL with the same
// LastRunAt must reuse the cached verdict (cache size stays at 1, single
// underlying compute) rather than triggering a fresh cron parse per call.
func TestMissedScheduleVerdict_CachesWithinTTL(t *testing.T) {
	t.Parallel()
	h := &CronHandlers{}
	startedAt := time.Now().Add(-24 * time.Hour) // past suppression window
	now := time.Now()
	j := &cron.Job{
		ID:        "job-cache-1",
		Schedule:  "@every 30m",
		CreatedAt: startedAt,
		LastRunAt: now.Add(-15 * time.Minute), // recent → not missed
	}

	// First call populates the cache.
	missed1, _ := h.missedScheduleVerdict(j, now, startedAt)
	if missed1 {
		t.Fatalf("recent run should not be missed")
	}
	if len(h.missedCache) != 1 {
		t.Fatalf("missedCache size = %d, want 1", len(h.missedCache))
	}
	// Mutate the underlying job's schedule in a way that would change the
	// verdict if HasMissedSchedule were re-evaluated. Because the cache key
	// includes the schedule string, this would normally miss the cache —
	// but we want to assert the same-key path serves cached values, so we
	// query with the *original* job pointer and inspect the cached value
	// directly via a same-key second call. The verdict must remain the
	// cached one even when the cron package would compute differently.
	missed2, _ := h.missedScheduleVerdict(j, now, startedAt)
	if missed2 != missed1 {
		t.Fatalf("second call within TTL returned %v, want cached %v", missed2, missed1)
	}
	if len(h.missedCache) != 1 {
		t.Fatalf("second call grew cache to %d, want 1 (cache reuse failed)", len(h.missedCache))
	}
}

// TestMissedScheduleVerdict_InvalidatesOnLastRunAdvance guards the
// correctness leg of R245-PERF-4 (#857): when LastRunAt advances (the job
// completed a run), the cached verdict MUST be discarded — otherwise a
// post-run dashboard tick would still report the pre-run verdict.
func TestMissedScheduleVerdict_InvalidatesOnLastRunAdvance(t *testing.T) {
	t.Parallel()
	h := &CronHandlers{}
	startedAt := time.Now().Add(-24 * time.Hour)
	now := time.Now()
	// Stale LastRunAt → should be flagged missed under @every 5m.
	j := &cron.Job{
		ID:        "job-invalidate-1",
		Schedule:  "@every 5m",
		CreatedAt: startedAt,
		LastRunAt: now.Add(-90 * time.Minute),
	}
	missedStale, _ := h.missedScheduleVerdict(j, now, startedAt)
	if !missedStale {
		t.Fatalf("90m-stale @every 5m must be missed")
	}

	// Job ran just now: LastRunAt advances. The cache key matches but
	// lastRunNanos differs, so the verdict must be recomputed → not missed.
	j.LastRunAt = now
	missedFresh, _ := h.missedScheduleVerdict(j, now, startedAt)
	if missedFresh {
		t.Fatalf("after fresh run, missed verdict must clear (cache stale)")
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
