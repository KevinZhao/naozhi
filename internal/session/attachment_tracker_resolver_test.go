package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// addIndexedSession wires a session into r.sessions + the keyhash index the
// way publishSessionLocked/indexAdd do, without the full spawn machinery.
func addIndexedSession(r *Router, key, workspace string) *ManagedSession {
	s := &ManagedSession{key: key}
	s.setWorkspace(workspace)
	r.sessions[key] = s
	r.indexAdd(key)
	return s
}

// TestWorkspaceResolver_IndexFastPath pins #1646: the resolver must resolve a
// keyhash to the right session's workspace via the O(1) keyhashToKey index.
func TestWorkspaceResolver_IndexFastPath(t *testing.T) {
	r := &Router{
		sessions:     make(map[string]*ManagedSession),
		keyhashToKey: make(map[string]string),
	}
	addIndexedSession(r, "dashboard:direct:user:a", "/ws/a")
	addIndexedSession(r, "dashboard:direct:user:b", "/ws/b")

	resolve := r.workspaceResolverForTracker()

	if got := resolve(persist.KeyHash("dashboard:direct:user:a")); got != "/ws/a" {
		t.Fatalf("resolve(a) = %q, want /ws/a", got)
	}
	if got := resolve(persist.KeyHash("dashboard:direct:user:b")); got != "/ws/b" {
		t.Fatalf("resolve(b) = %q, want /ws/b", got)
	}
	// Index must be populated (proves the fast path is live, not the scan).
	if len(r.keyhashToKey) != 2 {
		t.Fatalf("keyhashToKey size = %d, want 2", len(r.keyhashToKey))
	}
}

// TestWorkspaceResolver_EmptyAndUnknown covers the contract edges: empty
// keyhash and a hash with no matching session both return "".
func TestWorkspaceResolver_EmptyAndUnknown(t *testing.T) {
	r := &Router{
		sessions:     make(map[string]*ManagedSession),
		keyhashToKey: make(map[string]string),
	}
	addIndexedSession(r, "dashboard:direct:user:a", "/ws/a")
	resolve := r.workspaceResolverForTracker()

	if got := resolve(""); got != "" {
		t.Fatalf("resolve(\"\") = %q, want empty", got)
	}
	if got := resolve(persist.KeyHash("nope")); got != "" {
		t.Fatalf("resolve(unknown) = %q, want empty", got)
	}
}

// TestWorkspaceResolver_StaleIndexSelfHeals pins the self-healing fallback: a
// keyhashToKey entry pointing at a key no longer in r.sessions must NOT return
// a workspace for the dead key, and must still find a live session via the
// scan fallback.
func TestWorkspaceResolver_StaleIndexSelfHeals(t *testing.T) {
	r := &Router{
		sessions:     make(map[string]*ManagedSession),
		keyhashToKey: make(map[string]string),
	}
	live := addIndexedSession(r, "dashboard:direct:user:live", "/ws/live")
	_ = live

	// Simulate a delete site that removed the session but left a stale index
	// entry (e.g. a future path that bypasses indexDel).
	stale := "dashboard:direct:user:stale"
	r.keyhashToKey[persist.KeyHash(stale)] = stale // dangling: not in r.sessions

	resolve := r.workspaceResolverForTracker()

	// Stale keyhash → no session in r.sessions → resolver must return "".
	if got := resolve(persist.KeyHash(stale)); got != "" {
		t.Fatalf("resolve(stale) = %q, want empty (no dead-session workspace)", got)
	}
	// Live keyhash still resolves.
	if got := resolve(persist.KeyHash("dashboard:direct:user:live")); got != "/ws/live" {
		t.Fatalf("resolve(live) = %q, want /ws/live", got)
	}
}

// TestWorkspaceResolver_NilIndexFallsBackToScan covers test-created routers
// whose keyhashToKey is nil: the resolver must still work via the linear scan.
func TestWorkspaceResolver_NilIndexFallsBackToScan(t *testing.T) {
	r := &Router{sessions: make(map[string]*ManagedSession)} // keyhashToKey nil
	s := &ManagedSession{key: "dashboard:direct:user:a"}
	s.setWorkspace("/ws/a")
	r.sessions[s.key] = s

	resolve := r.workspaceResolverForTracker()
	if got := resolve(persist.KeyHash("dashboard:direct:user:a")); got != "/ws/a" {
		t.Fatalf("resolve(a) via scan = %q, want /ws/a", got)
	}
}

// TestIndexDel_RemovesKeyhash verifies indexDel drops the keyhash entry so the
// index stays bounded.
func TestIndexDel_RemovesKeyhash(t *testing.T) {
	r := &Router{
		sessions:     make(map[string]*ManagedSession),
		keyhashToKey: make(map[string]string),
	}
	addIndexedSession(r, "dashboard:direct:user:a", "/ws/a")
	if _, ok := r.keyhashToKey[persist.KeyHash("dashboard:direct:user:a")]; !ok {
		t.Fatal("keyhash not added")
	}
	delete(r.sessions, "dashboard:direct:user:a")
	r.indexDel("dashboard:direct:user:a")
	if _, ok := r.keyhashToKey[persist.KeyHash("dashboard:direct:user:a")]; ok {
		t.Fatal("keyhash not removed by indexDel")
	}
}
