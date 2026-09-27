package session

// R70-ARCH-H3 regression tests for Router.Takeover.
//
// Takeover has three branches that previously had zero direct coverage:
//  1. Fresh key — no existing session, the spawn runs immediately.
//  2. Replace alive / dead session — existing session is closed and
//     unregistered before the re-spawn.
//  3. Concurrent-creation abort — while we release the table lock to Close() the
//     old process, another goroutine slips in a live session under the
//     same key; Takeover must abort with a specific error instead of
//     clobbering the interloper.
//
// The real spawn call at the end of Takeover fails because newTestRouter's
// wrapper points at /nonexistent/cli-binary. That's fine: these tests
// assert the side effects up to and including the spawn, not a
// successful spawn.

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// newTakeoverTestRouter builds a Router that has every map Takeover and
// the spawn touch which the older newTestRouter helper leaves nil
// (wsStore and pp are zero-value usable and need no init).
func newTakeoverTestRouter(maxProcs int) *Router {
	r := newTestRouter(maxProcs)
	stateOf(r).picks.backend = map[string]string{}
	return r
}

// TestTakeover_NewKey — calling Takeover on a key that has no existing
// session must skip the close/unregister branches entirely, still write
// the workspace override, and then proceed to spawn (which fails in tests
// but only after the override is recorded).
func TestTakeover_NewKey(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	key := "feishu:direct:user1:general"
	workspace := "/tmp/takeover-ws"

	_, err := r.Takeover(context.Background(), key, "sess-abc", workspace, AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error (nonexistent CLI), got nil")
	}
	if !strings.Contains(err.Error(), "spawn") {
		t.Errorf("error should be a spawn failure, got: %v", err)
	}

	// Workspace override must land on the chat key prefix, not the session key.
	chatKey := chatKeyFor(key)
	if got, _ := stateOf(r).workspaces.Lookup(chatKey); got != workspace {
		t.Errorf("workspaceOverrides[%q] = %q, want %q", chatKey, got, workspace)
	}
	if !stateOf(r).workspaces.Dirty() {
		t.Error("wsOverridesDirty should be set after Takeover writes override")
	}

	// No stale session should have been injected.
	if _, ok := lookupT(r, key); ok {
		t.Error("sessions[key] should be empty after failed spawn on a fresh Takeover")
	}
}

// TestTakeover_ReplacesDeadSession — when the existing session's process
// is dead, Takeover takes the else-branch that unregisters without calling
// Close, and bumps storeGen.
func TestTakeover_ReplacesDeadSession(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	key := "feishu:direct:user2:general"

	old := injectSession(r, key, newDeadProc())
	old.setSessionID("old-sess")
	setIDT(r, "old-sess", key)
	genBefore := r.ss.Gen()

	_, err := r.Takeover(context.Background(), key, "new-sess", "/tmp/ws", AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error after dead-session unregister")
	}

	if _, ok := lookupT(r, key); ok {
		t.Error("dead session should have been unregistered")
	}
	if _, ok := keyForIDT(r, "old-sess"); ok {
		t.Error("old session ID should have been removed from sessionIDToKey")
	}
	if r.ss.Gen() <= genBefore {
		t.Error("storeGen should advance when dead session is unregistered")
	}
}

// TestTakeover_ReplacesAliveSession — when the existing session's process
// is alive, Takeover enters the close-and-recheck branch: Close() is
// called on the old process while the table lock is released, then the session is
// unregistered under the re-acquired lock. the spawn fails afterward,
// but the old process must be Close()'d and the session gone.
func TestTakeover_ReplacesAliveSession(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	key := "feishu:direct:user3:general"

	oldProc := newIdleProc()
	old := injectSession(r, key, oldProc)
	old.setSessionID("old-alive-sess")
	setIDT(r, "old-alive-sess", key)

	_, err := r.Takeover(context.Background(), key, "new-sess", "/tmp/ws", AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error after alive-session replacement")
	}

	if oldProc.Alive() {
		t.Error("old alive process must be Close()'d during Takeover")
	}
	if _, ok := lookupT(r, key); ok {
		t.Error("old session should have been unregistered before re-spawn failed")
	}
}

// hookCloseProc is a fakeProcess whose Close() runs a hook. Takeover
// calls Close() while holding neither the table lock nor any per-session lock, so
// the hook can exercise the concurrent-creation race.
type hookCloseProc struct {
	*fakeProcess
	onClose func()
	once    sync.Once
}

func newHookCloseProc(onClose func()) *hookCloseProc {
	return &hookCloseProc{fakeProcess: newIdleProc(), onClose: onClose}
}

func (h *hookCloseProc) Close() {
	h.once.Do(func() {
		if h.onClose != nil {
			h.onClose()
		}
	})
	h.fakeProcess.Close()
}

// TestTakeover_ConcurrentCreationAborts — if another goroutine inserts a
// live session under the same key while Takeover has released the table lock to
// Close() the old process, Takeover must abort with an explicit error
// rather than silently unregister the interloper and spawn on top.
func TestTakeover_ConcurrentCreationAborts(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	key := "feishu:direct:user4:general"

	interloper := newIdleProc()
	// onClose runs after the table lock is released by Takeover. Inject a new live
	// session under the same key to simulate a concurrent GetOrCreate
	// winning the race.
	hook := newHookCloseProc(func() {
		var s *ManagedSession
		r.ss.Update(func(tx sessTx) {
			s = &ManagedSession{key: key}
			s.storeProcess(interloper)
			s.touchLastActive()
			tx.Put(key, s)
		})
	})
	old := injectSession(r, key, hook)
	old.setSessionID("old-sess")

	_, err := r.Takeover(context.Background(), key, "new-sess", "/tmp/ws", AgentOpts{})
	if err == nil {
		t.Fatal("expected concurrent-creation abort error, got nil")
	}
	if !strings.Contains(err.Error(), "concurrent session created") {
		t.Errorf("error should identify the concurrent race, got: %v", err)
	}

	// Interloper session must survive untouched; Takeover may not
	// clobber a live parallel session.
	cur, ok := lookupT(r, key)
	if !ok {
		t.Fatal("interloper session should still be in sessions map")
	}
	if cur.loadProcess() == nil || !cur.loadProcess().Alive() {
		t.Error("interloper's live process should survive the aborted Takeover")
	}
	if !interloper.Alive() {
		t.Error("interloper's fakeProcess should not have been Close()'d by Takeover")
	}
}

// TestTakeover_EmptyWorkspaceSkipsOverride — the guard `if chatKey != key`
// prevents writing an override for single-segment keys (or for keys where
// the caller knows no chat-scoped workspace applies). Using a key that
// equals its own chatKey exercises that guard.
func TestTakeover_EmptyWorkspaceSkipsOverride(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	// Single-segment key: chatKeyFor returns the key unchanged, so the
	// `chatKey != key` guard in Takeover must skip the override write.
	key := "singleton-key"
	if chatKeyFor(key) != key {
		t.Fatalf("test precondition: chatKeyFor(%q) should equal %q", key, key)
	}

	_, err := r.Takeover(context.Background(), key, "sess-x", "/tmp/ws", AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error, got nil")
	}
	if stateOf(r).workspaces.Len() != 0 {
		t.Errorf("workspaceOverrides should remain empty for chatKey==key, got %v",
			stateOf(r).workspaces.Snapshot())
	}
	if stateOf(r).workspaces.Dirty() {
		t.Error("wsOverridesDirty should not be set when override write is skipped")
	}
}

// TestTakeover_WorkspaceOverrideIdempotent — writing the same workspace
// twice should still succeed but must not rebump wsOverridesDirty if the
// prior value already matches (the guard inside Takeover).
func TestTakeover_WorkspaceOverrideIdempotent(t *testing.T) {
	t.Parallel()
	r := newTakeoverTestRouter(3)
	key := "feishu:direct:user5:general"
	chatKey := chatKeyFor(key)
	// Seed = disk-loaded semantics: present, not dirty.
	stateOf(r).workspaces.Seed(map[string]string{chatKey: "/tmp/existing"})

	// Same workspace: guard should see prev == workspace and skip dirty flip.
	_, err := r.Takeover(context.Background(), key, "sess-y", "/tmp/existing", AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error")
	}
	if stateOf(r).workspaces.Dirty() {
		t.Error("wsOverridesDirty should not flip when new workspace equals prior")
	}

	// Different workspace: must flip dirty.
	stateOf(r).workspaces.MarkSavedIfUnchanged(stateOf(r).workspaces.Gen())
	_, err = r.Takeover(context.Background(), key, "sess-y", "/tmp/changed", AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error")
	}
	if !stateOf(r).workspaces.Dirty() {
		t.Error("wsOverridesDirty should flip when workspace changes")
	}
	if got, _ := stateOf(r).workspaces.Lookup(chatKey); got != "/tmp/changed" {
		t.Errorf("workspaceOverrides[%q] = %q, want /tmp/changed", chatKey, got)
	}
}

// compile-time sanity: hookCloseProc satisfies processIface through its
// embedded fakeProcess. If this compiles we're good.
var _ processIface = (*hookCloseProc)(nil)
