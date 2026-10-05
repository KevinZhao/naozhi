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
// a pre-cancelled ctx so no case waits out its timer; the contract is
// "returns without panicking", not a wall-clock ceiling.
//
// Direct inputs (period=-1, jitterMax=large positive): the existing
// `if window <= 0` branch already rejects this; the new
// `if int64(window) <= 0` is a redundant belt-and-suspenders that
// only matters if a future refactor reorders the clamp. The test
// covers both shapes (negative period; negative window via custom
// jitterMax) so a regression that removes either guard fails here.
func TestJitterSleep_NegativeWindowDoesNotPanic(t *testing.T) {
	t.Parallel()

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
	// select hits ctx.Done() instead of waiting out the timer.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Recover inside the subtest: t.Run runs this closure on its own
			// goroutine, so a recover in the parent would never see the panic.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("jitterSleep panicked on non-positive window: %v", r)
				}
			}()
			jitterSleep(ctx, tc.period, tc.jitterMax)
		})
	}
}

// TestJitterSleep_CancelledCtxSkipsTimerWait pins the ctx.Done() arm of
// jitterSleep's final select. The window is 24h, so a regression that waits
// on the timer alone blocks for hours with probability ~1 - 5s/24h; the 5s
// bound only has to separate "returns" from "sleeps", not measure latency.
func TestJitterSleep_CancelledCtxSkipsTimerWait(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		jitterSleep(ctx, 0, 24*time.Hour)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("jitterSleep with a cancelled ctx still waited on its timer; the ctx.Done() arm must short-circuit it")
	}
}
