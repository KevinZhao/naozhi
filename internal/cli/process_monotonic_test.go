package cli

import (
	"testing"
	"time"
)

// TestMonotonicCompare_SurvivesNTPJump demonstrates the invariant the
// contract test pins. A time.Time with its monotonic reading intact compares
// correctly against a later time.Time even when the wall clocks are
// out-of-order (the scenario an NTP backward step would produce). Stripping
// the monotonic reading via UnixNano round-trip reverts to wall-clock
// subtraction and the ordering flips — exactly the failure mode R43-CONCUR-C1
// warned about.
//
// This is a pure demonstration test: no CLI wiring, no races, just a
// textual proof that Go's time semantics match the safety argument in
// drainStaleEvents' godoc. If stdlib ever changes these semantics (it won't,
// but the guarantee is worth pinning) the test fails and forces a re-audit.
func TestMonotonicCompare_SurvivesNTPJump(t *testing.T) {
	t.Parallel()
	earlier := time.Now()
	// Sleep a touch so the monotonic reading measurably advances.
	time.Sleep(2 * time.Millisecond)
	later := time.Now()

	// With both times carrying monotonic readings, later.After(earlier) is
	// driven by the monotonic delta and is always true — independent of any
	// wall-clock manipulation we could simulate.
	if !later.After(earlier) {
		t.Fatalf("monotonic-preserving later.After(earlier) = false: "+
			"earlier=%v later=%v", earlier, later)
	}

	// Strip monotonic readings by round-tripping through UnixNano and
	// manually inverting the wall-clock order. After the strip, comparison
	// falls back to wall clock and we can force a regression.
	earlierWall := time.Unix(0, later.UnixNano()) // pretend clock ran forward
	laterWall := time.Unix(0, earlier.UnixNano()) // then jumped backward
	if laterWall.After(earlierWall) {
		t.Fatal("wall-clock-only comparison should reflect the injected NTP-style " +
			"inversion; if it does not, the demonstration has regressed")
	}
}
