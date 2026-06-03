package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestSnapshot_MessageCount_ProcNilFromPersistedHistory pins #1644: a session
// with no live process (evicted / suspended / stub) must report
// SessionSnapshot.MessageCount derived from the count of persisted "user"
// entries rather than a constant 0, otherwise AutoTitler's minUserTurns gate
// skips the session forever.
func TestSnapshot_MessageCount_ProcNilFromPersistedHistory(t *testing.T) {
	s := &ManagedSession{key: "dashboard:direct:user:general"}
	s.InjectHistory([]cli.EventEntry{
		{Time: 1, Type: "user", Summary: "q1"},
		{Time: 2, Type: "text", Summary: "a1"},
		{Time: 3, Type: "user", Summary: "q2"},
		{Time: 4, Type: "tool_use", Summary: "t"},
		{Time: 5, Type: "user", Summary: "q3"},
	})

	if got := s.persistedUserTurns.Load(); got != 3 {
		t.Fatalf("persistedUserTurns = %d, want 3", got)
	}

	snap := s.Snapshot()
	if snap.MessageCount != 3 {
		t.Fatalf("Snapshot().MessageCount = %d, want 3 (proc==nil should use persisted user turns)", snap.MessageCount)
	}
}

// TestSnapshot_MessageCount_ProcNilNoUserEntries ensures non-user entries do
// not inflate the count (a brand-new session with only system/init events
// should stay at 0 so the min-turn gate still suppresses it).
func TestSnapshot_MessageCount_ProcNilNoUserEntries(t *testing.T) {
	s := &ManagedSession{key: "dashboard:direct:user:general"}
	s.InjectHistory([]cli.EventEntry{
		{Time: 1, Type: "init", Summary: "boot"},
		{Time: 2, Type: "text", Summary: "a"},
	})

	if got := s.Snapshot().MessageCount; got != 0 {
		t.Fatalf("Snapshot().MessageCount = %d, want 0", got)
	}
}

// TestRecountPersistedUserTurns_AfterAppend verifies the cached count tracks
// incremental InjectHistory appends.
func TestRecountPersistedUserTurns_AfterAppend(t *testing.T) {
	s := &ManagedSession{key: "k"}
	s.InjectHistory([]cli.EventEntry{{Time: 1, Type: "user"}})
	if got := s.persistedUserTurns.Load(); got != 1 {
		t.Fatalf("after first append = %d, want 1", got)
	}
	s.InjectHistory([]cli.EventEntry{{Time: 2, Type: "user"}, {Time: 3, Type: "user"}})
	if got := s.persistedUserTurns.Load(); got != 3 {
		t.Fatalf("after second append = %d, want 3", got)
	}
}

// TestInstallFreshSessionLocked_RecountsPersistedUserTurns guards
// R20260603040203-CODE-002: installFreshSessionLocked must seed
// persistedUserTurns from the restored oldHistory so snapshot().MessageCount
// and the AutoTitler min-turn gate report the correct count before any new
// turn arrives. Tested via the same locking pattern the production code uses
// (struct construction + historyMu-protected recount).
func TestInstallFreshSessionLocked_RecountsPersistedUserTurns(t *testing.T) {
	t.Parallel()
	oldHistory := []cli.EventEntry{
		{Time: 1, Type: "user", Summary: "q1"},
		{Time: 2, Type: "text", Summary: "a1"},
		{Time: 3, Type: "user", Summary: "q2"},
	}

	// Replicate the construction pattern from installFreshSessionLocked:
	// create the ManagedSession, set persistedHistory, then call
	// recountPersistedUserTurnsLocked under historyMu.
	s := &ManagedSession{
		key:              "feishu:direct:alice:general",
		persistedHistory: oldHistory,
	}
	if len(oldHistory) > 0 {
		s.historyMu.Lock()
		s.recountPersistedUserTurnsLocked()
		s.historyMu.Unlock()
	}

	if got := s.persistedUserTurns.Load(); got != 2 {
		t.Errorf("persistedUserTurns = %d, want 2 (2 user entries in oldHistory)", got)
	}
	snap := s.Snapshot()
	if snap.MessageCount != 2 {
		t.Errorf("Snapshot().MessageCount = %d, want 2", snap.MessageCount)
	}
}

// TestRenameSession_RecountsPersistedUserTurns guards R20260603040203-CODE-002:
// after RenameSession the renamed session must report the correct
// persistedUserTurns count so AutoTitler's min-turn gate is not starved.
func TestRenameSession_RecountsPersistedUserTurns(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})
	const oldKey = "scratch:abc:general:general"
	const newKey = "feishu:direct:alice:aside-general-deadbeef"

	s := &ManagedSession{key: oldKey}
	// Inject history directly; InjectHistory will set persistedUserTurns on s,
	// but the rename must preserve/recount on the fresh struct.
	s.InjectHistory([]cli.EventEntry{
		{Time: 1, Type: "user", Summary: "q1"},
		{Time: 2, Type: "text", Summary: "a1"},
		{Time: 3, Type: "user", Summary: "q2"},
		{Time: 4, Type: "user", Summary: "q3"},
	})

	r.mu.Lock()
	r.sessions[oldKey] = s
	r.indexAdd(oldKey)
	r.mu.Unlock()

	if !r.RenameSession(oldKey, newKey) {
		t.Fatal("RenameSession returned false")
	}
	got := r.GetSession(newKey)
	if got == nil {
		t.Fatal("renamed session missing")
	}
	if count := got.persistedUserTurns.Load(); count != 3 {
		t.Errorf("persistedUserTurns = %d, want 3 after rename", count)
	}
	if mc := got.Snapshot().MessageCount; mc != 3 {
		t.Errorf("Snapshot().MessageCount = %d, want 3 after rename", mc)
	}
}
