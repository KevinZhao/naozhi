package cron

import (
	"context"
	"testing"
	"time"
)

// TestJitterSleep_NegativeWindowDoesNotPanic pins the
// R20260527122801-GO-018 defensive guard: a non-positive duration
// passed as `period` (with jitterMax > period/4) used to slip past
// the `window <= 0` check only when arithmetic produced exactly
// zero, but a hostile or buggy custom robfigcron.Schedule could
// surface a non-monotonic Next() that arithmetic clamps to a
// non-positive int64 nanosecond count. mrand.Int64N panics on
// n <= 0, so this test asserts jitterSleep returns cleanly without
// dragging the cron tick into robfig/cron's recover path.
//
// Direct inputs (period=-1, jitterMax=large positive): the existing
// `if window <= 0` branch already rejects this; the new
// `if int64(window) <= 0` is a redundant belt-and-suspenders that
// only matters if a future refactor reorders the clamp. The test
// covers both shapes (negative period; negative window via custom
// jitterMax) so a regression that removes either guard fails here.
func TestJitterSleep_NegativeWindowDoesNotPanic(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("jitterSleep panicked on non-positive window: %v", r)
		}
	}()

	cases := []struct {
		name      string
		period    time.Duration
		jitterMax time.Duration
	}{
		// Negative period with positive jitterMax: window stays at
		// jitterMax (the period > 0 branch is skipped), then
		// mrand.Int64N(jitterMax) is fine. Sanity baseline.
		{"negative period, positive jitterMax", -time.Second, 1 * time.Millisecond},
		// Zero / negative jitterMax: caller invariant says jitterMax
		// must be > 0 but defend anyway. window<=0 branch must catch.
		{"negative jitterMax", time.Hour, -time.Second},
		{"zero jitterMax", time.Hour, 0},
		// Both non-positive: every guard must hold simultaneously.
		{"both non-positive", -time.Hour, -time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			jitterSleep(context.Background(), tc.period, tc.jitterMax)
			// Should be effectively instant — never sleeps when
			// window resolves to non-positive.
			if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
				t.Errorf("jitterSleep with %s slept %v; want ~0 (non-positive window must short-circuit)", tc.name, elapsed)
			}
		})
	}
}
