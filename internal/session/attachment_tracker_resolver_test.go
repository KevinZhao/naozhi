package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// addIndexedSession wires a session into r.ss.sessions + the keyhash index the
// way publishSessionLocked/indexAdd do, without the full spawn machinery.
func addIndexedSession(r *Router, key, workspace string) *ManagedSession {
	s := &ManagedSession{key: key}
	s.setWorkspace(workspace)
	r.ss.Put(key, s)
	return s
}

// TestWorkspaceResolver_IndexFastPath pins #1646: the resolver must resolve a
// keyhash to the right session's workspace via the O(1) keyhashToKey index.
func TestWorkspaceResolver_IndexFastPath(t *testing.T) {
	r := &Router{
		ss: newSessionTable(),
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

}

// TestWorkspaceResolver_EmptyAndUnknown covers the contract edges: empty
// keyhash and a hash with no matching session both return "".
func TestWorkspaceResolver_EmptyAndUnknown(t *testing.T) {
	r := &Router{
		ss: newSessionTable(),
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
