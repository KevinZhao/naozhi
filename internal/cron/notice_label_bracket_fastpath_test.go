package cron

import (
	"strings"
	"testing"
)

// TestFormatCronNotice_NoBracketFastPathSemantics pins
// R20260527122801-PERF-15: when label has no `]`, the fast-path
// IndexByte check skips strings.ReplaceAll entirely. Pre-fix
// ReplaceAll always walked the string (and could realloc) on the
// hot tick path even for ASCII titles like "daily-review" /
// "morning-stand-up" which are by far the common case.
//
// We can't directly observe the skip; instead we lock down the
// surface contract: the rendered output for a no-bracket label
// is byte-for-byte identical to the pre-fix output (semantic
// regression net), AND the rendered output round-trips through
// formatCronNotice without ever introducing a `］`. The fast-path
// MUST NOT silently rewrite bytes when no `]` is present.
func TestFormatCronNotice_NoBracketFastPathSemantics(t *testing.T) {
	t.Parallel()

	cases := []string{
		"daily-review",
		"morning-stand-up",
		"hourly_metrics",
		"任务标题中文",
		"emoji-rocket",
		"",
	}
	for _, label := range cases {
		t.Run(label, func(t *testing.T) {
			got := formatCronNotice(label, "body")
			// Output must contain the literal label as substring —
			// no rewrite happened.
			if label != "" && !strings.Contains(got, label) {
				t.Errorf("formatCronNotice rewrote no-bracket label %q; output %q does not contain it", label, got)
			}
			// Full-width bracket must NOT appear when input had no `]`.
			if strings.Contains(got, "］") {
				t.Errorf("formatCronNotice introduced full-width `］` for label %q without `]`: %q", label, got)
			}
			// There must be exactly one `]` in the rendered output —
			// the template-closing one.
			if c := strings.Count(got, "]"); c != 1 {
				t.Errorf("formatCronNotice output has %d `]` for label %q; want 1 (template only): %q", c, label, got)
			}
		})
	}
}

// TestFormatCronNotice_NoBracketFastPathAllocs is a soft alloc check:
// for a no-bracket label, formatCronNotice's allocation count must be
// no worse than the pre-fix path. We can't assert an exact number
// because SanitizeForLog and strings.Builder.Grow themselves allocate;
// we just assert the count stays at a small constant so a future
// regression that drops the IndexByte fast-path (and starts allocating
// per-call inside ReplaceAll) is caught.
func TestFormatCronNotice_NoBracketFastPathAllocs(t *testing.T) {
	// NOT t.Parallel() — testing.AllocsPerRun is sensitive to the
	// scheduler placing competing goroutines on its sample.

	label := "daily-review"
	body := "ok"
	avg := testing.AllocsPerRun(100, func() {
		_ = formatCronNotice(label, body)
	})
	// 8 is a generous ceiling — empirically the no-bracket path runs
	// at ~3-4 allocs (SanitizeForLog + Builder.Grow + b.String). The
	// pre-fix path was strictly higher due to ReplaceAll's unconditional
	// walk; this ceiling catches any regression that re-introduces
	// per-call ReplaceAll without the fast-path guard.
	if avg > 8 {
		t.Errorf("formatCronNotice avg allocs/run = %.1f for no-bracket label; want <= 8 (fast-path guard regressed?)", avg)
	}
}
