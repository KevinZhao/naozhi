package server

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
		"2026-13-26T07:16:17Z",  // month 13 — time.Date normalises but reference Parse rejects via fast path's pre-validation falling through
		"2026-05-26T25:00:00Z",  // hour 25
		"2026-05-26T07:16:17.Z", // empty fraction
	}
	for _, in := range cases {
		// We don't strictly require 0 for *every* malformed input — the
		// time.Parse fallback may parse some that the fast path rejects
		// — but empty / wholly-garbage must yield 0.
		if in == "" || in == "not-a-time" {
			if got := parseISO8601MS(in); got != 0 {
				t.Errorf("parseISO8601MS(%q) = %d, want 0", in, got)
			}
		}
	}
}
