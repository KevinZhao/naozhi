package session

import "testing"

// TestRegisterForResume_LeakedIDToKeyDoesNotMisroute is the #2093 regression
// (R20260614-LB-idtokey). idToKey is not cleaned for a session's rotated/old
// session IDs when a deterministic key is deleted and then reused for an
// unrelated conversation. A resume of the OLD sessionID would then dedup into
// the unrelated live session under the reused key — cross-session bleed.
//
// Reproduces the exact reported sequence at the index level:
//  1. old session A under deterministic key K leaves idToKey[A]=K dangling
//     (rotation/delete cleaned sessions[K]'s current SID but not A);
//  2. K is reused by an unrelated session C (idToKey[C]=K, sessions[K].SID=C);
//  3. user resumes A → RegisterForResume(newKey, A, …).
//
// Pre-fix: dedup found idToKey[A]=K, saw sessions[K] exists, returned K →
// the user lands in unrelated session C. Post-fix: the found session does not
// own A in its chain, so the leaked entry is dropped and a fresh resume entry
// is created under the caller's key.
func TestRegisterForResume_LeakedIDToKeyDoesNotMisroute(t *testing.T) {
	t.Parallel()

	r := minimalRouter(t)

	const (
		reusedKey = "dashboard:pj:wshashabc:general" // deterministic ProjectStableKey
		oldSID    = "session-A-old"
		liveSID   = "session-C-unrelated"
		resumeKey = "dashboard:resume:session-A-old"
	)

	// Unrelated live session C occupies the reused deterministic key. Its
	// chain owns only liveSID — it has nothing to do with oldSID.
	unrelated := &ManagedSession{key: reusedKey}
	unrelated.setSessionID(liveSID)
	r.ss.Lock()
	r.publishSessionLocked(reusedKey, unrelated, false)
	r.ss.SetID(liveSID, reusedKey)
	// The leaked residue: oldSID still maps to the reused key even though the
	// session living there (C) never owned oldSID.
	r.ss.SetID(oldSID, reusedKey)
	r.ss.Unlock()

	// User resumes the OLD session A.
	got := r.RegisterForResume(resumeKey, oldSID, "/ws", "hello again")

	// Must NOT be routed into the unrelated session's key.
	if got == reusedKey {
		t.Fatalf("resume of old SID %q was misrouted into unrelated session at reused key %q (#2093 cross-session bleed)",
			oldSID, reusedKey)
	}
	if got != resumeKey {
		t.Fatalf("RegisterForResume = %q, want a fresh entry under %q", got, resumeKey)
	}

	// A fresh suspended session must now exist under the caller's key,
	// targeting the requested sessionID.
	r.ss.RLock()
	fresh, ok := r.ss.Lookup(resumeKey)
	r.ss.RUnlock()
	if !ok {
		t.Fatalf("no fresh session created under %q", resumeKey)
	}
	if fresh.SessionID() != oldSID {
		t.Fatalf("fresh session SID = %q, want %q", fresh.SessionID(), oldSID)
	}

	// The unrelated session must be untouched and still own its own SID.
	r.ss.RLock()
	stillThere, ok := r.ss.Lookup(reusedKey)
	r.ss.RUnlock()
	if !ok || stillThere != unrelated || stillThere.SessionID() != liveSID {
		t.Fatalf("unrelated session at %q was disturbed by the resume", reusedKey)
	}
}

// TestRegisterForResume_LegitimateChainDedupStillWorks guards the other side
// of the fix: when the found session genuinely owns the requested sessionID
// (current SID or anywhere in its rotation chain), dedup must STILL reuse it.
// The #2093 fix must not break the legitimate "resume an ID that belongs to a
// live session's prev chain" path.
func TestRegisterForResume_LegitimateChainDedupStillWorks(t *testing.T) {
	t.Parallel()

	r := minimalRouter(t)

	const (
		liveKey = "dashboard:pj:wshashxyz:general"
		curSID  = "session-current"
		prevSID = "session-rotated-prev"
	)

	// A live session whose rotation chain is [prevSID, curSID].
	live := &ManagedSession{key: liveKey, prevSessionIDs: []string{prevSID}}
	live.setSessionID(curSID)
	r.ss.Lock()
	r.publishSessionLocked(liveKey, live, false)
	r.ss.SetID(curSID, liveKey)
	r.ss.SetID(prevSID, liveKey)
	r.ss.Unlock()

	// Resuming the current SID dedups to the live key.
	if got := r.RegisterForResume("dashboard:resume:cur", curSID, "/ws", ""); got != liveKey {
		t.Fatalf("resume of current SID: got %q, want dedup to %q", got, liveKey)
	}
	// Resuming a prev-chain SID also dedups to the live key (it genuinely
	// owns that ID).
	if got := r.RegisterForResume("dashboard:resume:prev", prevSID, "/ws", ""); got != liveKey {
		t.Fatalf("resume of prev-chain SID: got %q, want dedup to %q", got, liveKey)
	}
}
