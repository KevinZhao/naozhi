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
	s.totalCost = 1.42
	s.lastActive.Store(time.Now().UnixNano())

	r.mu.Lock()
	r.sessions[oldKey] = s
	r.indexAdd(oldKey)
	r.sessionIDToKey["sess-promote-1"] = oldKey
	r.mu.Unlock()

	if !r.RenameSession(oldKey, newKey) {
		t.Fatal("RenameSession returned false")
	}
	if r.GetSession(oldKey) != nil {
		t.Error("old key should be gone after rename")
	}
	got := r.GetSession(newKey)
	if got == nil {
		t.Fatal("new key missing after rename")
	}
	if got.getSessionID() != "sess-promote-1" {
		t.Errorf("sessionID not preserved: %q", got.getSessionID())
	}
	if got.Workspace() != "/tmp/repo" {
		t.Errorf("workspace not preserved: %q", got.Workspace())
	}
	if got.totalCost != 1.42 {
		t.Errorf("totalCost not preserved: %v", got.totalCost)
	}
	if got.Backend() != "claude" {
		t.Errorf("backend not preserved: %q", got.Backend())
	}
	// Reverse index must point at the new key.
	r.mu.RLock()
	idxKey := r.sessionIDToKey["sess-promote-1"]
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
	r.sessions[oldKey] = &ManagedSession{key: oldKey}
	r.sessions[newKey] = &ManagedSession{key: newKey}
	r.indexAdd(oldKey)
	r.indexAdd(newKey)
	r.mu.Unlock()

	if r.RenameSession(oldKey, newKey) {
		t.Error("RenameSession should refuse collisions")
	}
	if r.GetSession(oldKey) == nil {
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
	r.sessions[oldKey] = &ManagedSession{key: oldKey}
	r.indexAdd(oldKey)
	r.mu.Unlock()

	// control byte in new key must be rejected by ValidateSessionKey.
	if r.RenameSession(oldKey, "bad:key\x00:x:y") {
		t.Error("invalid new key accepted")
	}
}
