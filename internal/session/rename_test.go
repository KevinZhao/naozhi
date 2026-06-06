package session

import (
	"testing"
	"time"
)

// TestRenameSession_HappyPath exercises the scratch-promote entrypoint:
// an aside key is moved to a sidebar key while the session ID, workspace,
// totalCost, userLabel, and backend are preserved.
func TestRenameSession_HappyPath(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})
	const oldKey = "scratch:abc:general:general"
	const newKey = "feishu:direct:alice:aside-general-deadbeef"

	s := &ManagedSession{key: oldKey}
	s.setWorkspace("/tmp/repo")
	s.setSessionID("sess-promote-1")
	s.SetBackend("claude")
	s.SetCLIName("claude-code")
	s.SetCLIVersion("2.0.0")
	s.SetUserLabel("")
	storeTotalCost(&s.totalCost, 1.42)
	s.lastActive.Store(time.Now().UnixNano())

	r.mu.Lock()
	r.ss.sessions[oldKey] = s
	r.indexAdd(oldKey)
	r.ss.idToKey["sess-promote-1"] = oldKey
	r.mu.Unlock()

	if !r.RenameSession(oldKey, newKey) {
		t.Fatal("RenameSession returned false")
	}
	if r.SessionFor(oldKey) != nil {
		t.Error("old key should be gone after rename")
	}
	got := r.SessionFor(newKey)
	if got == nil {
		t.Fatal("new key missing after rename")
	}
	if got.getSessionID() != "sess-promote-1" {
		t.Errorf("sessionID not preserved: %q", got.getSessionID())
	}
	if got.Workspace() != "/tmp/repo" {
		t.Errorf("workspace not preserved: %q", got.Workspace())
	}
	if gotCost := loadTotalCost(&got.totalCost); gotCost != 1.42 {
		t.Errorf("totalCost not preserved: %v", gotCost)
	}
	if got.Backend() != "claude" {
		t.Errorf("backend not preserved: %q", got.Backend())
	}
	// Reverse index must point at the new key.
	r.mu.RLock()
	idxKey := r.ss.idToKey["sess-promote-1"]
	r.mu.RUnlock()
	if idxKey != newKey {
		t.Errorf("sessionIDToKey = %q, want %q", idxKey, newKey)
	}
}

func TestRenameSession_MissingSource(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})
	if r.RenameSession("missing:key:x:y", "feishu:direct:alice:aside") {
		t.Error("RenameSession should fail when source missing")
	}
}

func TestRenameSession_CollisionRefused(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})
	const oldKey = "scratch:abc:general:general"
	const newKey = "feishu:direct:alice:general"

	r.mu.Lock()
	r.ss.sessions[oldKey] = &ManagedSession{key: oldKey}
	r.ss.sessions[newKey] = &ManagedSession{key: newKey}
	r.indexAdd(oldKey)
	r.indexAdd(newKey)
	r.mu.Unlock()

	if r.RenameSession(oldKey, newKey) {
		t.Error("RenameSession should refuse collisions")
	}
	if r.SessionFor(oldKey) == nil {
		t.Error("old key dropped on collision")
	}
}

func TestRenameSession_SameKeyRefused(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})
	if r.RenameSession("same:key:a:b", "same:key:a:b") {
		t.Error("RenameSession(same, same) should return false")
	}
}

func TestRenameSession_InvalidNewKey(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})
	const oldKey = "scratch:abc:general:general"

	r.mu.Lock()
	r.ss.sessions[oldKey] = &ManagedSession{key: oldKey}
	r.indexAdd(oldKey)
	r.mu.Unlock()

	// control byte in new key must be rejected by ValidateSessionKey.
	if r.RenameSession(oldKey, "bad:key\x00:x:y") {
		t.Error("invalid new key accepted")
	}
}

// TestRenameSession_PreservesCreatedAt locks the scratch-promote contract:
// renaming an existing session must carry its createdAt into the fresh
// ManagedSession so the sidebar row keeps its established position rather
// than getting shoved to the bottom on rename.
func TestRenameSession_PreservesCreatedAt(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})
	const oldKey = "scratch:abc:general:general"
	const newKey = "feishu:direct:alice:aside-general-deadbeef"

	stamp := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano()
	s := &ManagedSession{key: oldKey}
	s.setSessionID("sess-rename-ca")
	s.createdAt.Store(stamp)
	s.lastActive.Store(stamp + int64(time.Hour))

	r.mu.Lock()
	r.ss.sessions[oldKey] = s
	r.indexAdd(oldKey)
	r.ss.idToKey["sess-rename-ca"] = oldKey
	r.mu.Unlock()

	if !r.RenameSession(oldKey, newKey) {
		t.Fatal("RenameSession returned false")
	}
	got := r.SessionFor(newKey)
	if got == nil {
		t.Fatal("new key missing after rename")
	}
	if gotCA := got.createdAt.Load(); gotCA != stamp {
		t.Errorf("createdAt not preserved: got %d want %d", gotCA, stamp)
	}
}

// TestRenameSession_StampsCreatedAtWhenSourceUnstamped covers the legacy
// edge case: the source session somehow has createdAt == 0 (pre-feature
// store loaded with both CreatedAt and LastActive zero). Rename must stamp
// `now` rather than leaving the fresh row anchored at zero, where it would
// otherwise float to the very top of the sidebar.
func TestRenameSession_StampsCreatedAtWhenSourceUnstamped(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})
	const oldKey = "scratch:abc:general:general"
	const newKey = "feishu:direct:alice:aside-general-deadbeef"

	s := &ManagedSession{key: oldKey}
	s.setSessionID("sess-rename-zero")
	// createdAt left at 0 to simulate the pre-feature pathological case.

	r.mu.Lock()
	r.ss.sessions[oldKey] = s
	r.indexAdd(oldKey)
	r.ss.idToKey["sess-rename-zero"] = oldKey
	r.mu.Unlock()

	before := time.Now().UnixNano()
	if !r.RenameSession(oldKey, newKey) {
		t.Fatal("RenameSession returned false")
	}
	got := r.SessionFor(newKey)
	if got == nil {
		t.Fatal("new key missing after rename")
	}
	if gotCA := got.createdAt.Load(); gotCA < before {
		t.Errorf("createdAt not stamped on rename: got %d, want >= %d", gotCA, before)
	}
}
