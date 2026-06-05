package cron

import (
	"testing"
	"time"
)

// TestParseISO8601MS_FastPathMatchesParse asserts the hand-rolled fast
// path produces the same UnixMilli as time.Parse(time.RFC3339Nano, …)
// across the canonical UTC shapes the Claude CLI emits. This is the
// correctness guard for R234-PERF-10 / #1012 — any divergence should
// fail loudly here before reaching production transcripts.
func TestParseISO8601MS_FastPathMatchesParse(t *testing.T) {
	cases := []string{
		"2026-05-26T07:16:17Z",
		"2026-05-26T07:16:17.0Z",
		"2026-05-26T07:16:17.5Z",
		"2026-05-26T07:16:17.123Z",
		"2026-05-26T07:16:17.123456Z",
		"2026-05-26T07:16:17.123456789Z",
		"1970-01-01T00:00:00Z",
		"1970-01-01T00:00:00.000000001Z",
		"2099-12-31T23:59:59.999999999Z",
		"2024-02-29T12:34:56Z", // leap year
	}
	for _, in := range cases {
		want, err := time.Parse(time.RFC3339Nano, in)
		if err != nil {
			t.Fatalf("reference Parse(%q) errored: %v", in, err)
		}
		got := parseISO8601MS(in)
		if got != want.UnixMilli() {
			t.Errorf("parseISO8601MS(%q) = %d, want %d", in, got, want.UnixMilli())
		}
		// Also exercise the fast path directly to confirm `ok=true`.
		gotFast, ok := parseISO8601MSFast(in)
		if !ok {
			t.Errorf("parseISO8601MSFast(%q) ok=false; expected canonical shape to match", in)
		}
		if gotFast != want.UnixMilli() {
			t.Errorf("parseISO8601MSFast(%q) = %d, want %d", in, gotFast, want.UnixMilli())
		}
	}
}

// TestParseISO8601MS_FastPathRejectsNonCanonical confirms the fast path
// declines non-canonical shapes (timezone offsets, lowercase markers,
// missing fields) so they fall through to time.Parse — which either
// parses them correctly OR returns 0 sentinel via the err branch.
func TestParseISO8601MS_FastPathRejectsNonCanonical(t *testing.T) {
	rejects := []string{
		"",                                // empty
		"2026-05-26T07:16:17",             // no zone
		"2026-05-26T07:16:17z",            // lowercase zone
		"2026-05-26t07:16:17Z",            // lowercase T
		"2026-05-26T07:16:17+00:00",       // offset (canonical-but-not-Z)
		"2026-05-26T07:16:17-08:00",       // offset
		"2026-05-26T07:16:17.Z",           // empty fraction
		"2026-05-26T07:16:17.1234567890Z", // 10 fraction digits
		"2026-05-26T07:16:17.12a3Z",       // non-digit in fraction
		"2026/05/26T07:16:17Z",            // wrong separator
		"26-05-26T07:16:17Z",              // 2-digit year
		"2026-5-26T07:16:17Z",             // 1-digit month
		// Out-of-range calendar/clock fields: the fast path must decline so
		// the caller falls back to time.Parse (which rejects them) instead of
		// silently normalising via time.Date.
		"2024-99-01T00:00:00Z", // month 99
		"2026-13-26T07:16:17Z", // month 13
		"2026-00-26T07:16:17Z", // month 0
		"2026-05-00T07:16:17Z", // day 0
		"2026-05-32T07:16:17Z", // day 32
		"2026-02-30T07:16:17Z", // Feb 30
		"2026-02-29T07:16:17Z", // Feb 29 non-leap year
		"2026-04-31T07:16:17Z", // Apr 31
		"2026-05-26T24:00:00Z", // hour 24
		"2026-05-26T07:60:00Z", // minute 60
		"2026-05-26T12:00:60Z", // leap second (time.Parse rejects 60)
		"2026-05-26T12:00:61Z", // second 61
	}
	for _, in := range rejects {
		if _, ok := parseISO8601MSFast(in); ok {
			t.Errorf("parseISO8601MSFast(%q) ok=true; expected fast path to decline", in)
		}
	}
}

// TestParseISO8601MS_NonCanonicalFallback confirms parseISO8601MS still
// yields a correct result for inputs that bypass the fast path but are
// valid RFC3339 (e.g. timezone offsets) — no regression vs. previous
// behaviour where every input went through time.Parse.
func TestParseISO8601MS_NonCanonicalFallback(t *testing.T) {
	cases := []string{
		"2026-05-26T07:16:17+00:00",
		"2026-05-26T15:16:17+08:00",
		"2026-05-26T07:16:17.500-05:00",
	}
	for _, in := range cases {
		want, err := time.Parse(time.RFC3339Nano, in)
		if err != nil {
			t.Fatalf("reference Parse(%q) errored: %v", in, err)
		}
		got := parseISO8601MS(in)
		if got != want.UnixMilli() {
			t.Errorf("parseISO8601MS(%q) = %d, want %d (fallback path)", in, got, want.UnixMilli())
		}
	}
}

// TestParseISO8601MS_InvalidReturnsZero asserts garbage input yields the
// 0 sentinel callers rely on to skip the time-window filter. Mirrors the
// pre-fast-path contract (time.Parse error ⇒ 0) so callers don't need to
// learn a new failure mode.
func TestParseISO8601MS_InvalidReturnsZero(t *testing.T) {
	cases := []string{
		"",
		"not-a-time",
		"2026-13-26T07:16:17Z",  // month 13 — fast path range-rejects, fallback Parse also rejects ⇒ 0
		"2026-05-26T25:00:00Z",  // hour 25
		"2026-05-26T07:16:17.Z", // empty fraction
	}
	for _, in := range cases {
		// Every input here is rejected by both the fast path's range check
		// and time.Parse, so parseISO8601MS must yield the 0 sentinel.
		if got := parseISO8601MS(in); got != 0 {
			t.Errorf("parseISO8601MS(%q) = %d, want 0", in, got)
		}
	}
}

// TestParseISO8601MS_OutOfRangeParityWithParse is the regression guard for
// #1787: previously the fast path fed out-of-range fields straight to
// time.Date, which *normalises* (month 13 → next Jan), while time.Parse
// *rejects* them. That made the fast and slow paths diverge. This asserts
// that for out-of-range canonical-shaped inputs (a) time.Parse rejects, and
// (b) the fast path declines (ok=false), so parseISO8601MS falls back and
// returns the same 0 sentinel.
func TestParseISO8601MS_OutOfRangeParityWithParse(t *testing.T) {
	cases := []string{
		"2024-99-01T00:00:00Z", // month 99
		"2026-13-26T07:16:17Z", // month 13
		"2026-00-26T07:16:17Z", // month 0
		"2026-05-00T07:16:17Z", // day 0
		"2026-05-32T07:16:17Z", // day 32
		"2026-02-30T07:16:17Z", // Feb 30
		"2026-02-29T07:16:17Z", // Feb 29 non-leap
		"2026-04-31T07:16:17Z", // Apr 31 (30-day month)
		"2026-05-26T24:00:00Z", // hour 24
		"2026-05-26T07:60:00Z", // minute 60
		"2026-05-26T12:00:60Z", // leap second — time.Parse rejects 60
		"2026-05-26T12:00:61Z", // second 61
	}
	for _, in := range cases {
		if _, err := time.Parse(time.RFC3339Nano, in); err == nil {
			t.Fatalf("reference Parse(%q) unexpectedly succeeded; test premise broken", in)
		}
		if _, ok := parseISO8601MSFast(in); ok {
			t.Errorf("parseISO8601MSFast(%q) ok=true; fast path must decline out-of-range fields", in)
		}
		if got := parseISO8601MS(in); got != 0 {
			t.Errorf("parseISO8601MS(%q) = %d, want 0 (both paths reject)", in, got)
		}
	}
}

// TestParseISO8601MS_LeapDayAccepted confirms the per-month day validation
// is leap-year aware: Feb 29 in a leap year is still accepted by both paths.
func TestParseISO8601MS_LeapDayAccepted(t *testing.T) {
	cases := []string{
		"2024-02-29T12:34:56Z", // 2024 leap year
		"2000-02-29T00:00:00Z", // 2000 leap (÷400)
		"2026-01-31T23:59:59Z", // 31-day month boundary
		"2026-04-30T23:59:59Z", // 30-day month boundary
	}
	for _, in := range cases {
		want, err := time.Parse(time.RFC3339Nano, in)
		if err != nil {
			t.Fatalf("reference Parse(%q) errored: %v", in, err)
		}
		got, ok := parseISO8601MSFast(in)
		if !ok {
			t.Errorf("parseISO8601MSFast(%q) ok=false; valid date should be accepted", in)
		}
		if got != want.UnixMilli() {
			t.Errorf("parseISO8601MSFast(%q) = %d, want %d", in, got, want.UnixMilli())
		}
	}
}
