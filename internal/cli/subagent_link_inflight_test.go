package cli

import (
	"testing"
)

// TestSubagentLinker_TryMarkResolveInflight_R260528_PERF_7 anchors #1354:
// the in-flight gate must (a) admit the first claim per taskID, (b) reject
// duplicates while the claim is held, (c) accept again after the slot is
// cleared, and (d) reject empty taskIDs unconditionally so the production
// `taskID == ""` short-circuit upstream stays the canonical filter.
//
// Without this gate, every replayed/duplicate task_started event for the
// same task_id during the up-to-3s Resolve grace window escapes into a
// fresh goroutine that promptly re-bails inside Resolve, paying the
// schedule + closure-allocation cost for nothing.
func TestSubagentLinker_TryMarkResolveInflight_R260528_PERF_7(t *testing.T) {
	t.Parallel()
	l := NewSubagentLinker()

	// (a) first claim wins.
	if !l.TryMarkResolveInflight("task-A") {
		t.Fatal("first claim for task-A should succeed")
	}

	// (b) second claim while first is still in-flight is rejected.
	if l.TryMarkResolveInflight("task-A") {
		t.Fatal("duplicate claim for task-A should be rejected while first is in-flight")
	}

	// Distinct taskIDs do not collide.
	if !l.TryMarkResolveInflight("task-B") {
		t.Fatal("first claim for task-B should succeed (distinct from A)")
	}

	// (c) once cleared, a follow-up claim re-succeeds (eg cache eviction
	// case where a tombstone needs a retry against fresh disk state).
	l.clearResolveInflight("task-A")
	if !l.TryMarkResolveInflight("task-A") {
		t.Fatal("post-clear re-claim for task-A should succeed")
	}

	// (d) empty taskID always rejected: the upstream `taskID == ""`
	// short-circuit is the load-bearing filter; the helper returning
	// false for empty defends against any future caller that bypasses
	// the upstream check.
	if l.TryMarkResolveInflight("") {
		t.Fatal("empty taskID claim must be rejected so empty-key entries cannot accumulate")
	}
	// Clearing an empty taskID is a no-op (no panic, no side effect).
	l.clearResolveInflight("")

	// Clearing a never-claimed key is a no-op.
	l.clearResolveInflight("task-never-claimed")
}

// TestSubagentLinker_ResolveClearsInflight_R260528_PERF_7 anchors that
// Resolve's deferred clearResolveInflight runs even on the early-return
// paths (already-resolved cache hit, missing context). The deferred
// clear is the only thing that lets a follow-up duplicate task_started
// re-claim — if it ever drops off one of those return paths, callers
// would silently lock out the taskID for the rest of the linker's
// lifetime.
func TestSubagentLinker_ResolveClearsInflight_R260528_PERF_7(t *testing.T) {
	t.Parallel()
	l := NewSubagentLinker()

	// Path 1: missing-context return (projectDir/sessionID empty).
	// Pre-claim so we can observe the deferred clear releases it.
	if !l.TryMarkResolveInflight("task-no-ctx") {
		t.Fatal("pre-claim for task-no-ctx should succeed")
	}
	// Resolve bails early because SetContext was never called; the
	// deferred clear must still fire.
	_, ok := l.Resolve(nil, "task-no-ctx", "tu1", "name", "desc", 0)
	if ok {
		t.Fatal("Resolve should return false on missing context")
	}
	// Verify the slot is clear by re-claiming.
	if !l.TryMarkResolveInflight("task-no-ctx") {
		t.Fatal("expected Resolve to clear the in-flight slot on early-return; re-claim failed")
	}
}
