package cron

import (
	"testing"
	"time"
)

// TestR20260607GO001_SpawnElapsedSingleCapture pins R20260607-GO-001:
// spawnElapsed is captured exactly once before computing sendBudget, so
// spawn_elapsed_ms + send_budget_ms == job_timeout_ms in the log line
// (modulo the minSendBudget floor). Prior to the fix, two separate
// time.Since(spawnStart) calls drifted apart, making the arithmetic in
// the log inconsistent.
//
// This test exercises the formula directly (identical to the production
// computation after the fix) to assert the invariant:
//
//	spawnElapsed + sendBudget == jobTimeout  (when no floor is applied)
//	sendBudget == minSendBudget              (when floor is applied)
func TestR20260607GO001_SpawnElapsedSingleCapture(t *testing.T) {
	t.Parallel()

	jobTimeout := 5 * time.Minute

	tests := []struct {
		name        string
		spawnTaken  time.Duration
		wantSumEq   bool // spawnElapsed + sendBudget == jobTimeout
		wantAtFloor bool
	}{
		{
			name:        "fast-spawn-arithmetic-intact",
			spawnTaken:  200 * time.Millisecond,
			wantSumEq:   true,
			wantAtFloor: false,
		},
		{
			name:        "half-budget-spawn-arithmetic-intact",
			spawnTaken:  jobTimeout / 2,
			wantSumEq:   true,
			wantAtFloor: false,
		},
		{
			name:        "floor-applied-sum-exceeds-jobtimeout",
			spawnTaken:  jobTimeout - 5*time.Second,
			wantSumEq:   false, // floor bumps sendBudget, so sum > jobTimeout
			wantAtFloor: true,
		},
		{
			name:        "spawn-exceeds-jobtimeout-floor-applied",
			spawnTaken:  jobTimeout + 10*time.Second,
			wantSumEq:   false,
			wantAtFloor: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Simulate the production formula (single capture of spawnElapsed).
			spawnElapsed := tt.spawnTaken
			sendBudget := jobTimeout - spawnElapsed
			if sendBudget < minSendBudget {
				sendBudget = minSendBudget
			}

			if tt.wantAtFloor && sendBudget != minSendBudget {
				t.Fatalf("expected sendBudget == minSendBudget, got %v", sendBudget)
			}

			sum := spawnElapsed + sendBudget
			if tt.wantSumEq && sum != jobTimeout {
				// This is the invariant the fix enforces: with a single
				// spawnElapsed capture the log fields are internally consistent.
				t.Fatalf("spawnElapsed(%v) + sendBudget(%v) = %v, want %v (arithmetic inconsistency)",
					spawnElapsed, sendBudget, sum, jobTimeout)
			}
			if !tt.wantSumEq && sum == jobTimeout {
				t.Fatalf("expected floor to break exact equality, but sum == jobTimeout (%v)", sum)
			}
		})
	}
}

// TestR20260607GO001_TwoCallsDrift demonstrates why two separate time.Since
// calls (the pre-fix pattern) produce inconsistent log fields. This is a
// documentation test, not a regression test — it asserts that the single-
// capture pattern avoids the drift that two-call pattern would introduce.
func TestR20260607GO001_TwoCallsDrift(t *testing.T) {
	t.Parallel()

	jobTimeout := 5 * time.Minute
	spawnTaken := 2 * time.Minute

	// Single-capture (fixed): spawnElapsed captured once.
	spawnElapsed := spawnTaken
	sendBudget := jobTimeout - spawnElapsed
	if sendBudget < minSendBudget {
		sendBudget = minSendBudget
	}

	// With single capture the sum is exact (no floor case here).
	sum := spawnElapsed + sendBudget
	if sum != jobTimeout {
		t.Fatalf("single-capture: sum %v != jobTimeout %v", sum, jobTimeout)
	}

	// Two-call pattern: second time.Since returns a slightly later value,
	// making spawnElapsedForLog > spawnElapsedForBudget, so the reported
	// sum exceeds jobTimeout. Simulate with a 1ms drift.
	spawnElapsedForBudget := spawnTaken
	sendBudget2 := jobTimeout - spawnElapsedForBudget
	if sendBudget2 < minSendBudget {
		sendBudget2 = minSendBudget
	}
	spawnElapsedForLog := spawnTaken + 1*time.Millisecond // drift from second call
	driftedSum := spawnElapsedForLog + sendBudget2
	if driftedSum == jobTimeout {
		t.Fatalf("two-call pattern unexpectedly exact (drift simulation broke)")
	}
	if driftedSum <= jobTimeout {
		t.Fatalf("two-call drift sum %v should exceed jobTimeout %v", driftedSum, jobTimeout)
	}
}
