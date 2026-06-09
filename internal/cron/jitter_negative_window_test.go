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
// dragging the cron tick into robfig/cron's recover path. It runs with
// a pre-cancelled ctx so the timer wait short-circuits and the elapsed
// assertion stays deterministic on loaded CI runners.
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
	// Pre-cancelled ctx: jitterSleep still computes window and rolls
	// mrand.Int64N (the panic-risk path this test guards), but the final
	// select hits ctx.Done() immediately instead of waiting out the timer.
	// This keeps the elapsed-time assertion deterministic — without it the
	// "negative period, positive jitterMax" case legitimately sleeps up to
	// jitterMax (1ms), and a busy CI runner's scheduler latency pushes the
	// wakeup past any small fixed threshold, producing a flaky failure that
	// has nothing to do with the guards under test.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			jitterSleep(ctx, tc.period, tc.jitterMax)
			// With a cancelled ctx the call must return effectively
			// instantly regardless of the rolled window.
			if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
				t.Errorf("jitterSleep with %s took %v; want ~0 (cancelled ctx must short-circuit the timer wait)", tc.name, elapsed)
			}
		})
	}
}
