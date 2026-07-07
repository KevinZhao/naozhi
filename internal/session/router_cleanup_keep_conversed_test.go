package session

import (
	"testing"
	"time"
)

// TestShouldPrune_KeepsSessionWithSessionID pins the #2278 policy: a session
// that owns a Claude SessionID (i.e. the user actually conversed in it, so a
// resumable JSONL exists on disk) is NEVER auto-pruned, regardless of how far
// past pruneTTL it has idled. Only the user closing it (Reset / Remove) may
// remove it from the router. Orphans that never obtained a SessionID are still
// prunable so the map does not accumulate dead stubs.
func TestShouldPrune_KeepsSessionWithSessionID(t *testing.T) {
	r := &Router{
		ss:       sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs: 10,
		ttl:      1 * time.Minute,
		pruneTTL: 1 * time.Hour,
	}
	now := time.Now()
	aged := now.Add(-100 * time.Hour) // well past pruneTTL and the old 72h horizon

	tests := []struct {
		name      string
		proc      processIface
		sessionID string
		want      bool
	}{
		{
			name:      "dead process WITH sessionID (conversed) → keep",
			proc:      newDeadProc(),
			sessionID: "11111111-1111-1111-1111-111111111111",
			want:      false,
		},
		{
			name:      "nil process WITH sessionID (suspended, resumable) → keep",
			proc:      nil,
			sessionID: "22222222-2222-2222-2222-222222222222",
			want:      false,
		},
		{
			name:      "dead process WITHOUT sessionID (orphan) → prune",
			proc:      newDeadProc(),
			sessionID: "",
			want:      true,
		},
		{
			name:      "nil process WITHOUT sessionID (stub) → prune",
			proc:      nil,
			sessionID: "",
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &ManagedSession{key: tt.name}
			if tt.proc != nil {
				s.storeProcess(tt.proc)
			}
			if tt.sessionID != "" {
				s.setSessionID(tt.sessionID)
			}
			s.lastActive.Store(aged.UnixNano())

			if got := r.shouldPrune(s, now); got != tt.want {
				t.Errorf("shouldPrune = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCleanup_KeepsConversedSessionForever runs the full Cleanup path to
// confirm a conversed (SessionID-bearing) but long-dead session survives while
// a same-age orphan stub is pruned. This is the end-to-end guard for the
// "opened session vanished a day later" bug (#2278): the resumable card must
// still be present in r.ss.sessions after Cleanup.
func TestCleanup_KeepsConversedSessionForever(t *testing.T) {
	r := &Router{
		ss:       sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs: 10,
		ttl:      1 * time.Minute,
		pruneTTL: 1 * time.Hour,
	}
	r.bkStore.backendOverrides = map[string]string{}

	aged := time.Now().Add(-100 * time.Hour).UnixNano()

	// Conversed session: dead process (idle-reaped) but has a SessionID → keep.
	conversed := injectSession(r, "conversed", newDeadProc())
	conversed.setSessionID("33333333-3333-3333-3333-333333333333")
	conversed.lastActive.Store(aged)

	// Orphan stub: no process, no SessionID, same age → prune.
	orphan := &ManagedSession{key: "orphan"}
	orphan.lastActive.Store(aged)
	r.ss.sessions["orphan"] = orphan

	r.Cleanup()

	if _, ok := r.ss.sessions["conversed"]; !ok {
		t.Error("conversed session with SessionID must survive Cleanup regardless of age (#2278)")
	}
	if _, ok := r.ss.sessions["orphan"]; ok {
		t.Error("orphan stub without SessionID should still be pruned past pruneTTL")
	}
}
