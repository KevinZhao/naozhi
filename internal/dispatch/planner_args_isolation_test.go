package dispatch

import (
	"testing"
)

// TestPlannerArgsIsolation_ThreeArgSliceIsolatesAppend locks in R37-CONCUR1:
// the planner injection path in handleMessage uses a three-arg slice
// expression `opts.ExtraArgs[:len:len]` before appending
// `--append-system-prompt <p>`. Without the three-arg form, a plain
// `append(opts.ExtraArgs, ...)` would — when cap>len — write past the
// caller's visible length into the shared backing array owned by
// d.agents[agentID].ExtraArgs, silently contaminating every subsequent
// lookup for that agent.
//
// This test is a contract check on the Go slice semantics we rely on.
// It does NOT exercise handleMessage end-to-end (that requires a full
// Dispatcher + router + platform stack); instead it reproduces the exact
// pattern used at dispatch.go:~213 and verifies the alias safety.
// If a future refactor drops the three-arg expression, `shared` below
// would mutate and the assertion fails.
func TestPlannerArgsIsolation_ThreeArgSliceIsolatesAppend(t *testing.T) {
	t.Parallel()
	// Simulate d.agents[agentID].ExtraArgs — a slice with spare capacity,
	// as produced by any append-driven construction or slice-of-literal.
	shared := make([]string, 2, 8)
	shared[0] = "--model"
	shared[1] = "opus"

	// First caller reads the map value, gets a header that aliases the
	// shared backing array, and injects its planner prompt.
	optsA := shared
	optsA = append(optsA[:len(optsA):len(optsA)], "--append-system-prompt", "A")

	// Second caller, racing conceptually, reads the same map value after
	// the first caller has "published" its append.
	optsB := shared
	optsB = append(optsB[:len(optsB):len(optsB)], "--append-system-prompt", "B")

	// Shared must be untouched: the three-arg slice forced fresh backing
	// arrays on each append.
	if len(shared) != 2 {
		t.Fatalf("shared length changed: got %d, want 2", len(shared))
	}
	if shared[0] != "--model" || shared[1] != "opus" {
		t.Errorf("shared mutated: %v, want [--model opus]", shared)
	}

	// Per-caller slices must contain their own prompt.
	if len(optsA) != 4 || optsA[3] != "A" {
		t.Errorf("optsA = %v, want [--model opus --append-system-prompt A]", optsA)
	}
	if len(optsB) != 4 || optsB[3] != "B" {
		t.Errorf("optsB = %v, want [--model opus --append-system-prompt B]", optsB)
	}

	// And cross-contamination check: optsA and optsB must not alias.
	// If they shared a backing array, writing to optsB[3] would leak into
	// optsA[3].
	optsB[3] = "mutated"
	if optsA[3] != "A" {
		t.Errorf("optsA leaked from optsB mutation: %v — three-arg slice failed to isolate", optsA)
	}
}

// TestPlannerArgsIsolation_TwoArgAppendDoesLeak is the negative-control
// pair: it proves that without the three-arg expression, naive append
// DOES corrupt the shared source slice when cap>len. If Go ever changed
// slice semantics this would also fail, and the isolation test above
// would lose its justification. Kept as a documented tripwire.
func TestPlannerArgsIsolation_TwoArgAppendDoesLeak(t *testing.T) {
	t.Parallel()
	shared := make([]string, 2, 8)
	shared[0] = "--model"
	shared[1] = "opus"

	optsA := shared
	optsA = append(optsA, "--append-system-prompt", "A") // two-arg: unsafe

	// Shared's backing array now carries optsA's append, even though the
	// header length is still 2 — re-slicing exposes the stored values.
	peek := shared[:4]
	if peek[3] != "A" {
		t.Fatalf("expected two-arg append to write past caller's length; got %v — slice semantics changed, three-arg isolation no longer necessary", peek)
	}
}
