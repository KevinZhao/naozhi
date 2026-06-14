package session

import (
	"testing"
)

// TestRegisterForResume_StaleRotatedSID_DoesNotMisroute pins the #2093 fix:
// a deterministic key K whose SID rotated (A→C) can leave a dangling
// idToKey[A]=K. A later resume of the retired SID A must NOT dedup into the
// unrelated session that now lives at K (which carries the live SID C) — that
// would cross-route the user into a foreign conversation. Instead the dedup
// must re-validate the resolved session's current SID and, on mismatch,
// self-heal the stale entry and create a fresh resume entry for the queried
// key.
func TestRegisterForResume_StaleRotatedSID_DoesNotMisroute(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})

	const detKey = "dashboard:pj:wshash:general" // deterministic ProjectStableKey-shaped key
	const retiredSID = "sid-A"                   // user wants to resume this
	const liveSID = "sid-C"                      // session currently at detKey

	// Session C legitimately occupies detKey with its own live SID.
	sessC := &ManagedSession{key: detKey}
	sessC.setSessionID(liveSID)
	r.mu.Lock()
	r.ss.sessions[detKey] = sessC
	r.indexAdd(detKey)
	r.ss.idToKey[liveSID] = detKey
	// Dangling residue: the retired SID A still maps to detKey from a prior
	// incarnation that rotated its SID.
	r.ss.idToKey[retiredSID] = detKey
	r.mu.Unlock()

	const resumeKey = "feishu:direct:bob:general"
	got := r.RegisterForResume(resumeKey, retiredSID, "/tmp/ws", "resume A please")

	// MUST NOT route the user into the unrelated session C at detKey.
	if got == detKey {
		t.Fatalf("RegisterForResume mis-routed resume of %q into unrelated key %q (session C, live SID %q)",
			retiredSID, detKey, liveSID)
	}
	// Expect a fresh resume entry for the queried key.
	if got != resumeKey {
		t.Fatalf("RegisterForResume = %q, want fresh entry %q", got, resumeKey)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	// The stale residue must be healed: retiredSID now maps to the new key.
	if mapped := r.ss.idToKey[retiredSID]; mapped != resumeKey {
		t.Errorf("idToKey[%q] = %q, want %q (self-healed)", retiredSID, mapped, resumeKey)
	}
	// Session C and its live SID mapping must be untouched.
	if mapped := r.ss.idToKey[liveSID]; mapped != detKey {
		t.Errorf("idToKey[%q] = %q, want %q (unrelated session untouched)", liveSID, mapped, detKey)
	}
	if r.ss.sessions[detKey] != sessC {
		t.Errorf("session at %q was unexpectedly replaced", detKey)
	}
}

// TestRegisterForResume_MatchingSID_StillDedups guards against the #2093 fix
// over-correcting: when the resolved session genuinely carries the queried
// SID, dedup must still return the existing key (the original behaviour).
func TestRegisterForResume_MatchingSID_StillDedups(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})

	const key = "dashboard:pj:wshash:general"
	const sid = "sid-live"
	s := &ManagedSession{key: key}
	s.setSessionID(sid)
	r.mu.Lock()
	r.ss.sessions[key] = s
	r.indexAdd(key)
	r.ss.idToKey[sid] = key
	r.mu.Unlock()

	got := r.RegisterForResume("feishu:direct:bob:general", sid, "/tmp/ws", "p")
	if got != key {
		t.Fatalf("RegisterForResume = %q, want dedup into %q", got, key)
	}
}

// TestRegisterForResume_StaleNoSession_CleansAndCreates keeps coverage of the
// original stale-index branch (idToKey entry whose key no longer has a live
// session): it must clean the entry and create a fresh resume entry.
func TestRegisterForResume_StaleNoSession_CleansAndCreates(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})

	const sid = "sid-orphan"
	r.mu.Lock()
	r.ss.idToKey[sid] = "ghost:key:no:session" // points at a key with no session
	r.mu.Unlock()

	const resumeKey = "feishu:direct:bob:general"
	got := r.RegisterForResume(resumeKey, sid, "/tmp/ws", "p")
	if got != resumeKey {
		t.Fatalf("RegisterForResume = %q, want fresh entry %q", got, resumeKey)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if mapped := r.ss.idToKey[sid]; mapped != resumeKey {
		t.Errorf("idToKey[%q] = %q, want %q", sid, mapped, resumeKey)
	}
}
