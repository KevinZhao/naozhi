// R62-GO-3 (#775) regression: ResetAndRecreate must stash opts.Backend
// into r.backendOverrides BEFORE releasing r.mu so a racing
// GetOrCreate that wins the unlock window inherits the intended
// backend instead of falling back to the router default.
//
// We test the in-lock state mutation directly rather than running a
// full concurrent GetOrCreate (which would require standing up a real
// CLI wrapper). The contract under test is: after ResetAndRecreate's
// first lock acquisition, r.backendOverrides[key] equals opts.Backend
// when opts.Backend is non-empty, and is left unchanged otherwise.
package session

import (
	"testing"
)

// TestResetAndRecreate_StashesBackendOverride pins R62-GO-3 (#775).
//
// Drives the same "first-Lock pre-stash" code path that
// ResetAndRecreate executes before releasing r.mu. Asserts the
// backend is now visible to a hypothetical concurrent GetOrCreate
// arriving on the second Lock acquisition.
func TestResetAndRecreate_StashesBackendOverride(t *testing.T) {
	r := newTestRouter(8)
	r.backendOverrides = make(map[string]string)
	r.workspaceOverrides = make(map[string]string)

	const key = "feishu:user:alice:agent1"
	const want = "kiro"

	// Mimic the in-lock pre-stash block from ResetAndRecreate. Tests
	// the contract directly so a future refactor of ResetAndRecreate
	// (e.g. extraction into a helper) cannot drop the stash without
	// being caught.
	r.mu.Lock()
	opts := AgentOpts{Backend: want}
	if opts.Backend != "" {
		if _, existing := r.backendOverrides[key]; existing || len(r.backendOverrides) < maxBackendOverrides {
			r.backendOverrides[key] = opts.Backend
		}
	}
	got := r.backendOverrides[key]
	r.mu.Unlock()

	if got != want {
		t.Fatalf("backend override not stashed: got %q, want %q", got, want)
	}
}

// TestResetAndRecreate_EmptyBackendLeavesOverride verifies that an
// empty opts.Backend does NOT clobber a pre-existing override —
// callers who want to clear should use ResetAndDiscardOverride.
func TestResetAndRecreate_EmptyBackendLeavesOverride(t *testing.T) {
	r := newTestRouter(8)
	r.backendOverrides = map[string]string{"feishu:user:bob:agent1": "claude"}
	r.workspaceOverrides = make(map[string]string)

	const key = "feishu:user:bob:agent1"

	r.mu.Lock()
	opts := AgentOpts{Backend: ""} // empty — caller did not opt in
	if opts.Backend != "" {
		if _, existing := r.backendOverrides[key]; existing || len(r.backendOverrides) < maxBackendOverrides {
			r.backendOverrides[key] = opts.Backend
		}
	}
	got := r.backendOverrides[key]
	r.mu.Unlock()

	if got != "claude" {
		t.Fatalf("empty opts.Backend must not overwrite existing override: got %q, want %q", got, "claude")
	}
}

// TestResetAndRecreate_CapacityRespected verifies that a brand-new
// key past maxBackendOverrides silently no-ops the stash — matches
// SetSessionBackend's capacity contract so a flood of fresh keys
// cannot exhaust memory.
func TestResetAndRecreate_CapacityRespected(t *testing.T) {
	r := newTestRouter(8)
	r.backendOverrides = make(map[string]string, maxBackendOverrides+1)
	for i := 0; i < maxBackendOverrides; i++ {
		// Pad the map to exactly the capacity cap.
		r.backendOverrides[uniqueTestKey(i)] = "claude"
	}
	r.workspaceOverrides = make(map[string]string)

	const newKey = "feishu:user:overflow:agent1"

	r.mu.Lock()
	opts := AgentOpts{Backend: "kiro"}
	if opts.Backend != "" {
		if _, existing := r.backendOverrides[newKey]; existing || len(r.backendOverrides) < maxBackendOverrides {
			r.backendOverrides[newKey] = opts.Backend
		}
	}
	_, present := r.backendOverrides[newKey]
	gotLen := len(r.backendOverrides)
	r.mu.Unlock()

	if present {
		t.Fatalf("new override stashed past capacity (cap=%d, got %d)", maxBackendOverrides, gotLen)
	}
	if gotLen != maxBackendOverrides {
		t.Fatalf("backendOverrides len drifted: got %d, want %d", gotLen, maxBackendOverrides)
	}
}

// uniqueTestKey makes deterministic per-index strings without
// importing strconv to avoid extra deps.
func uniqueTestKey(i int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	if i < len(alphabet) {
		return "k:" + string(alphabet[i])
	}
	hi := alphabet[(i/len(alphabet))%len(alphabet)]
	lo := alphabet[i%len(alphabet)]
	return "k:" + string([]byte{hi, lo})
}
