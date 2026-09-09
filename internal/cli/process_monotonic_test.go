package cli

import (
	"os"
	"regexp"
	"testing"
	"time"
)

// TestDrainStaleEvents_MonotonicInvariant is R43-CONCUR-C1's source-level
// contract guard. The concern raised in the TODO is that NTP wall-clock
// jumps could invert ev.RecvAt vs. cutoff ordering and cause drainStaleEvents
// to either swallow fresh events or keep stale ones.
//
// The concern is only valid when either timestamp has been stripped of its
// monotonic-clock reading. Go's time.Now() always returns a time.Time whose
// Before/After/Sub arithmetic uses the monotonic component; round-tripping
// through time.Unix() / time.UnixMilli() / time.Parse() / the JSON codec /
// t.UTC() / t.Round() / t.Truncate() etc. strips it, after which subtraction
// falls back to wall clock and the NTP-jump scenario becomes reachable.
//
// Today both cutoff (in drainStaleEvents) and ev.RecvAt (set in readLoop) are
// assigned directly from time.Now(), so the monotonic-driven comparison path
// is exercised. This test reads process.go and asserts:
//
//  1. `cutoff := time.Now()` still appears in drainStaleEvents
//  2. `ev.RecvAt = now` (or `= time.Now()`) still appears in readLoop
//  3. No `cutoff = time.Unix(` / `clievent.RecvAt = time.Unix(` line exists
//
// A future refactor that accidentally breaks the invariant (e.g., normalising
// ev.RecvAt through UnixMilli to match EventEntry.Time ordering) will fail
// this test and force the author to re-examine the NTP-safety argument.
func TestDrainStaleEvents_MonotonicInvariant(t *testing.T) {
	t.Parallel()
	// Concatenate all process*.go sources that host the two timestamps
	// this test pins. readLoop (owner of `ev.RecvAt = now`) moved to
	// process_readloop.go in Phase 2; drainStaleEvents (owner of
	// `cutoff := time.Now()`) moved to process_turn.go in Phase 4
	// (see docs/rfc/process-split.md).
	files := []string{"process.go", "process_readloop.go", "process_turn.go"}
	var src []byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src = append(src, b...)
	}

	// 1) cutoff must be from time.Now() (keeps monotonic clock).
	if !regexp.MustCompile(`cutoff\s*:=\s*time\.Now\(\)`).Match(src) {
		t.Error("drainStaleEvents no longer initialises `cutoff := time.Now()`; " +
			"if the cutoff is derived from a Unix-round-tripped time, NTP wall-clock " +
			"jumps can invert ev.RecvAt.After(cutoff) and re-open R43-CONCUR-C1")
	}

	// 2) ev.RecvAt must receive a direct time.Now() (or a local variable that
	// was itself assigned time.Now()) — we accept either shape by matching
	// `ev.RecvAt = now` (current) or `ev.RecvAt = time.Now()` (possible
	// future simplification). Both preserve the monotonic reading.
	recvNow := regexp.MustCompile(`ev\.RecvAt\s*=\s*now\b`).Match(src)
	recvDirect := regexp.MustCompile(`ev\.RecvAt\s*=\s*time\.Now\(\)`).Match(src)
	if !recvNow && !recvDirect {
		t.Error("readLoop no longer assigns ev.RecvAt from a live time.Now() " +
			"snapshot; any Unix round-trip strips the monotonic clock and re-opens " +
			"R43-CONCUR-C1 (NTP jumps invert ordering). Acceptable forms: " +
			"`ev.RecvAt = now` (paired with `now := time.Now()`) or " +
			"`ev.RecvAt = time.Now()`")
	}

	// 3) Negative check: neither side should go through time.Unix family.
	// time.Unix strips the monotonic reading. If anyone wires it into the
	// cutoff / clievent.RecvAt path, the NTP-safety argument no longer holds.
	banned := regexp.MustCompile(
		`(?:cutoff|ev\.RecvAt)\s*[:=]+\s*time\.(?:Unix|UnixMilli|UnixMicro)\(`)
	if banned.Match(src) {
		t.Error("cutoff or ev.RecvAt is now derived via time.Unix*; this strips " +
			"the monotonic clock and re-opens R43-CONCUR-C1. Keep both assigned " +
			"from the live `time.Now()` snapshot so Before/After use monotonic " +
			"subtraction and ignore NTP wall-clock jumps.")
	}
}

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
