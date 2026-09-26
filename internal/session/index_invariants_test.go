package session

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/shim"
)

// index_invariants_test.go — the consistency of sessionStore's indices, asserted
// instead of documented (G3 #2667).
//
// sessionStore holds a record plus three indices over it:
//
//	sessions map[key]*ManagedSession   the record
//	byChat   map[chatKey]set[key]      derived from the KEY
//	keyhash  map[KeyHash(key)]key      derived from the KEY
//	idToKey  map[sessionID]key         derived from the session ID, learned later
//
// Nothing enforced their agreement. What stood in for enforcement were 54
// `// 读写:` comments naming which files touch which field plus a 763-line linter
// (tools/check-router-fields) checking that those comments matched the code — a
// mechanism #2497 measured as more expensive than what it compensates for:
// `git log -G'// 读写:' -- internal/session` showed 40 commits whose only content
// was keeping the comments current. Both are gone; this file is what replaced
// them.
//
// A comment saying "lifecycle writes this" cannot catch a lifecycle path that
// writes it WRONG. checkIndexInvariants can, and the test below drives every
// mutation path through it.

// checkIndexInvariants verifies that the table's indices agree with its
// sessions (sessiontable.Table.Check), including that every live session's ID
// resolves to it. Caller holds r.mu (or is single-threaded, as tests are).
func checkIndexInvariants(t *testing.T, r *Router, after string) {
	t.Helper()
	if problems := r.ss.Check((*ManagedSession).getSessionID); len(problems) > 0 {
		t.Errorf("index invariants broken after %s:\n  %s", after, strings.Join(problems, "\n  "))
	}
}

// newIndexTestRouter builds a router with the session table and picks
// allocated, which is what NewRouter does. Hand-built so the test drives the
// *Locked mutators directly rather than going through a spawn.
func newIndexTestRouter() *Router {
	r := &Router{ss: newSessionTable()}
	r.picks.initLocked()
	return r
}

// TestIndexInvariants_AcrossEveryMutationPath walks a session through publish →
// id-index → rename → unregister, and a second one through publish → ResetChat,
// checking the invariants after each step.
//
// These are the only paths that mutate sessions: two writes and three deletes in
// the whole package. The 457 r.ss.* accesses the issue counted are overwhelmingly
// READS, which cannot break an invariant — which is why this file exists instead
// of the transactional Update(func(tx)) the issue proposed. See the issue comment.
func TestIndexInvariants_AcrossEveryMutationPath(t *testing.T) {
	r := newIndexTestRouter()
	checkIndexInvariants(t, r, "empty router")

	const keyA = "feishu:p2p:userA:general"
	const keyB = "feishu:p2p:userB:general"
	sA := &ManagedSession{key: keyA}
	sB := &ManagedSession{key: keyB}

	r.publishSessionLocked(keyA, sA, false)
	checkIndexInvariants(t, r, "publishSessionLocked(A)")

	r.publishSessionLocked(keyB, sB, false)
	checkIndexInvariants(t, r, "publishSessionLocked(B)")

	// idToKey is learned asynchronously — the CLI reports the session ID after the
	// record already exists, which is why publishSessionLocked does not maintain
	// it. The ID must be set on the SESSION too, not just injected into the index:
	// RenameSession and unregisterSessionLocked both find the entry to move or drop
	// via s.getSessionID(). The first version of this test injected only the index
	// entry and the invariant check reported it as a stale mapping — the checker was
	// right and the fixture was wrong.
	sA.setSessionID("sess-id-A")
	r.ss.SetID("sess-id-A", keyA)
	checkIndexInvariants(t, r, "idToKey learned for A")

	const keyARenamed = "feishu:p2p:userA:renamed"
	// RenameSession takes r.mu itself.
	if !r.RenameSession(keyA, keyARenamed) {
		t.Fatal("RenameSession returned false for a live session")
	}
	checkIndexInvariants(t, r, "RenameSession(A)")
	if got := keyForID(r, "sess-id-A"); got != keyARenamed {
		t.Errorf("idToKey after rename = %q, want %q", got, keyARenamed)
	}

	r.unregisterSessionLocked(keyARenamed, sA, false)
	checkIndexInvariants(t, r, "unregisterSessionLocked(A)")
	if _, ok := r.ss.KeyForID("sess-id-A"); ok {
		t.Error("idToKey still maps the removed session's ID")
	}

	r.unregisterSessionLocked(keyB, sB, false)
	checkIndexInvariants(t, r, "unregisterSessionLocked(B)")
	if r.ss.Len() != 0 {
		t.Errorf("sessions still holds %d entries", r.ss.Len())
	}
}

// TestIndexInvariants_ResetChatDropsTheWholeChat covers the one path that does
// NOT maintain byChat per session: resetSessionLocked deletes the record and
// leaves byChat to its caller, because ResetChat drops the whole chat's set in
// one operation instead of once per session.
//
// That delegation is documented on resetSessionLocked ("Caller … is responsible
// for cleaning up r.ss.byChat"), and a documented delegation is exactly the shape
// that rots. Asserted here.
func TestIndexInvariants_ResetChatDropsTheWholeChat(t *testing.T) {
	r := newIndexTestRouter()
	const chat = "feishu:p2p:userC"
	keys := []string{chat + ":general", chat + ":agent-one", chat + ":agent-two"}
	for _, k := range keys {
		r.publishSessionLocked(k, &ManagedSession{key: k}, false)
	}
	// A session on a DIFFERENT chat must survive, or the wholesale byChat drop is
	// too wide.
	const other = "feishu:p2p:userD:general"
	r.publishSessionLocked(other, &ManagedSession{key: other}, false)
	checkIndexInvariants(t, r, "publish 3 + 1")

	r.ResetChat(chat)
	checkIndexInvariants(t, r, "ResetChat")

	for _, k := range keys {
		if _, ok := r.ss.Lookup(k); ok {
			t.Errorf("session %q survived ResetChat", k)
		}
	}
	if _, ok := r.ss.Lookup(other); !ok {
		t.Errorf("ResetChat removed %q, which is on a different chat", other)
	}
}

// TestIndexInvariants_RenameToOccupiedKeyLeavesIndicesConsistent: a rename whose
// target already exists is the case where a half-applied mutation would leave the
// indices pointing at two different records for one key.
func TestIndexInvariants_RenameToOccupiedKeyLeavesIndicesConsistent(t *testing.T) {
	r := newIndexTestRouter()
	const from = "feishu:p2p:userE:general"
	const to = "feishu:p2p:userF:general"
	r.publishSessionLocked(from, &ManagedSession{key: from}, false)
	r.publishSessionLocked(to, &ManagedSession{key: to}, false)

	r.RenameSession(from, to)
	checkIndexInvariants(t, r, "RenameSession onto an occupied key")
}

// TestIndexInvariants_DiscoveryAndShimAdoption covers the two paths the original
// probe missed. They are the ones that learn a session ID from OUTSIDE the spawn
// — a resume registration from the send path, and a live shim's state file after
// a naozhi restart — and both write idToKey, so a mistake there mis-routes a
// resume to another session (#2093) or loses the dedupe that keeps one CLI
// session from being adopted twice.
//
// The socket-dialing half of the shim reconnect is out of unit-test reach (the
// package deliberately spawns no real shims); what it does to the indices is the
// same setSessionIDIndex call adoption makes, exercised here.
func TestIndexInvariants_DiscoveryAndShimAdoption(t *testing.T) {
	r := newIndexTestRouter()

	// --- discovery: RegisterForResume publishes and indexes the ID ---
	const resumeID = "sess-id-resume"
	keyD := "dashboard:direct:2026-01-01-000000-1:proj"
	got := r.RegisterForResume(keyD, resumeID, "/tmp/ws", "prompt")
	if got != keyD {
		t.Fatalf("RegisterForResume returned %q, want %q", got, keyD)
	}
	checkIndexInvariants(t, r, "RegisterForResume")
	if mapped := keyForID(r, resumeID); mapped != keyD {
		t.Errorf("idToKey[%q] = %q, want %q — the resume dedupe reads this", resumeID, mapped, keyD)
	}

	// A second registration for the same ID must dedupe onto the first session
	// rather than publish a rival that owns the same CLI session.
	other := r.RegisterForResume("dashboard:direct:2026-01-01-000000-2:proj", resumeID, "/tmp/ws", "prompt")
	if other != keyD {
		t.Errorf("second RegisterForResume for the same id returned %q, want the existing %q", other, keyD)
	}
	checkIndexInvariants(t, r, "RegisterForResume dedupe")

	// --- shim adoption: a live shim missing from sessions.json ---
	const shimID = "sess-id-shim"
	keyS := "dashboard:direct:2026-01-01-000000-3:proj"
	r.mu.Lock()
	sess := r.adoptLiveShimLocked(shim.State{
		Key:       keyS,
		SessionID: shimID,
		Workspace: "/tmp/ws",
		Backend:   "claude",
		ShimPID:   4242,
	}, "claude", nil)
	r.mu.Unlock()
	if sess == nil {
		t.Fatal("adoptLiveShimLocked published no session")
	}
	checkIndexInvariants(t, r, "adoptLiveShimLocked")
	if mapped := keyForID(r, shimID); mapped != keyS {
		t.Errorf("idToKey[%q] = %q, want %q — an adopted shim must be resumable by its id", shimID, mapped, keyS)
	}

	// --- and the indices stay consistent when those sessions go away ---
	r.mu.Lock()
	r.unregisterSessionLocked(keyD, r.ss.Get(keyD), false)
	r.unregisterSessionLocked(keyS, r.ss.Get(keyS), false)
	r.mu.Unlock()
	// Check reports any idToKey entry left pointing at the gone sessions.
	checkIndexInvariants(t, r, "unregister after discovery + adoption")
}

// clearSessionIDIndexIfOwnedBy exists for one scenario: a respawn rotates a
// session's ID, and the cleanup of the OLD id must not delete an entry that now
// belongs to a different live session. #2093 is the same hazard from the other
// side — idToKey is not cleaned for rotated ids, so idToKey[id]=K can dangle
// while sessions[K] holds an unrelated session. An unconditional delete here
// would silently un-resume whichever session currently owns that id.
func TestClearSessionIDIndexIfOwnedBy_OnlyDeletesItsOwnEntry(t *testing.T) {
	r := newIndexTestRouter()
	const id = "sess-id-shared"
	const owner = "dashboard:direct:2026-01-01-000000-1:proj"
	const other = "dashboard:direct:2026-01-01-000000-2:proj"

	r.ss.SetID(id, owner)

	// A different key's cleanup must leave the owner's mapping alone.
	r.ss.ClearIDIfOwnedBy(id, other)
	if got := keyForID(r, id); got != owner {
		t.Errorf("idToKey[%q] = %q after another key's cleanup, want %q untouched", id, got, owner)
	}

	// The owner's own cleanup drops it.
	r.ss.ClearIDIfOwnedBy(id, owner)
	if _, ok := r.ss.KeyForID(id); ok {
		t.Errorf("idToKey[%q] survived its owner's cleanup", id)
	}

	// Sessions without an id reach it too.
	r.ss.SetID("", owner)
	if _, ok := r.ss.KeyForID(""); ok {
		t.Error("an empty session id must not be indexed")
	}
}
