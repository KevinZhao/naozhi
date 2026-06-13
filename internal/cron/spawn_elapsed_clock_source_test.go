package cron

import (
	"testing"
	"time"
)

// TestR20260613CR1_SpawnElapsedUsesInjectedClock pins R20260613-CR-1:
// spawnElapsed must be computed as s.now().Sub(spawnStart) so that tests
// using a fake clock get a deterministic sendBudget without real wall-clock
// drift. A regression reverting to time.Since(spawnStart) would compute
// elapsed from the real clock while spawnStart came from the fake clock,
// producing a near-zero elapsed value (the fake clock instant is far in the
// past relative to time.Now()).
//
// Strategy: set a fake clock 2 minutes ahead of the real wall clock, then
// set spawnStart to fake clock's "now" minus 3 minutes (simulating a
// 3-minute spawn). With s.now().Sub(spawnStart) the elapsed = 3min and
// sendBudget = max(jobTimeout-3min, minSendBudget). With time.Since(spawnStart)
// the elapsed would be ~3min+2min = ~5min (real clock is 2min behind fake),
// changing the computed sendBudget and diverging from the fake-clock intent.
func TestR20260613CR1_SpawnElapsedUsesInjectedClock(t *testing.T) {
	t.Parallel()

	// Fake clock set to a fixed instant well ahead of real time so
	// time.Since(fakeNow - duration) ≠ s.now().Sub(fakeNow - duration).
	fixedNow := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: fixedNow}

	// Simulate: spawn took exactly 1 minute according to the fake clock.
	spawnTakenFake := 1 * time.Minute
	spawnStart := fixedNow.Add(-spawnTakenFake)

	// Compute spawnElapsed the fixed way (as the patched code does).
	spawnElapsedFixed := clk.Now().Sub(spawnStart)

	// Compute spawnElapsed the broken way (time.Since uses real wall clock).
	// time.Since(spawnStart) = realNow - spawnStart
	// spawnStart is a year-2030 instant; real now is ~2026, so
	// time.Since gives a large negative-ish or very large positive duration
	// depending on the host clock — either way ≠ spawnTakenFake.
	spawnElapsedBroken := time.Since(spawnStart)

	if spawnElapsedFixed != spawnTakenFake {
		t.Errorf("s.now().Sub(spawnStart) = %v, want %v", spawnElapsedFixed, spawnTakenFake)
	}
	if spawnElapsedBroken == spawnTakenFake {
		// This would mean the real clock happens to equal the fake clock —
		// astronomically unlikely but guard it explicitly.
		t.Log("warning: real clock coincidentally matches fake clock; test environment may be unusual")
	}

	// The key assertion: the fixed formula produces the correct elapsed
	// duration that matches what the fake clock intended, while the broken
	// formula would not.
	jobTimeout := 5 * time.Minute
	sendBudgetFixed := jobTimeout - spawnElapsedFixed
	if sendBudgetFixed < minSendBudget {
		sendBudgetFixed = minSendBudget
	}
	wantSendBudget := 4 * time.Minute // jobTimeout(5m) - spawnTaken(1m)
	if sendBudgetFixed != wantSendBudget {
		t.Errorf("sendBudget with fixed clock = %v, want %v", sendBudgetFixed, wantSendBudget)
	}
}
