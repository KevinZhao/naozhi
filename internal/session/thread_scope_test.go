package session

import "testing"

// A thread's session key ("slack:group:C1#tT1:general") belongs to its chat
// ("slack:group:C1") for everything chat-level: the workspace override, /cd's
// reset, and the project binding.
const (
	scopeChat      = "slack:group:C1"
	scopeThreadKey = "slack:group:C1#tT1:general"
	scopeChatKey   = "slack:group:C1:general"
)

// TestThreadSession_SpawnsInItsChatsWorkspace: a thread session spawns in the
// chat's /cd override, and reading or setting the thread's chat key reaches
// the chat's override.
func TestThreadSession_SpawnsInItsChatsWorkspace(t *testing.T) {
	r := newWorkspaceTestRouter("/default", map[string]string{scopeChat: "/chat/ws"})
	var sp spawnParams
	r.ss.Update(func(tx sessTx) {
		sp = r.resolveSpawnParams(tx, scopeThreadKey, "", AgentOpts{})
	})
	if sp.Workspace != "/chat/ws" {
		t.Errorf("thread spawn workspace = %q, want the chat's /chat/ws", sp.Workspace)
	}
	if got := r.Workspace("slack:group:C1#tT1"); got != "/chat/ws" {
		t.Errorf("Workspace(thread chat key) = %q, want the chat's /chat/ws", got)
	}
	r.SetWorkspace("slack:group:C1#tT2", "/set/from/thread")
	if got := r.Workspace(scopeChat); got != "/set/from/thread" {
		t.Errorf("after SetWorkspace(thread chat key) the chat's workspace = %q, want /set/from/thread", got)
	}
}

// TestResetAndDiscardOverride_ThreadKeepsTheChatsOverride: resetting a
// thread's session leaves the chat's /cd alone; resetting the chat's own
// session still discards it.
func TestResetAndDiscardOverride_ThreadKeepsTheChatsOverride(t *testing.T) {
	r := newTestRouter(4)
	r.defaultCWD = "/default"
	injectSession(r, scopeThreadKey, newIdleProc())
	injectSession(r, scopeChatKey, newIdleProc())
	r.SetWorkspace(scopeChat, "/chat/ws")

	r.ResetAndDiscardOverride(scopeThreadKey)
	if got := r.Workspace(scopeChat); got != "/chat/ws" {
		t.Fatalf("after resetting the thread the chat's workspace = %q, want /chat/ws kept", got)
	}
	if r.SessionFor(scopeThreadKey) != nil {
		t.Error("the thread's session survived its reset")
	}
	r.ResetAndDiscardOverride(scopeChatKey)
	if got := r.Workspace(scopeChat); got != "/default" {
		t.Errorf("after resetting the chat's session its workspace = %q, want /default", got)
	}
}

// TestResetChatAndSetWorkspace_ResetsTheChatsThreads: /cd in a chat resets
// its threads' sessions too, and no other chat's.
func TestResetChatAndSetWorkspace_ResetsTheChatsThreads(t *testing.T) {
	r := newTestRouter(4)
	thread, chat, other := newIdleProc(), newIdleProc(), newIdleProc()
	injectSession(r, scopeThreadKey, thread)
	injectSession(r, scopeChatKey, chat)
	injectSession(r, "slack:group:C2#tT1:general", other)

	r.ResetChatAndSetWorkspace(scopeChat, t.TempDir())
	if thread.Alive() || chat.Alive() {
		t.Errorf("after /cd: thread alive %v, chat alive %v; want both reset", thread.Alive(), chat.Alive())
	}
	if !other.Alive() {
		t.Error("/cd in C1 reset a thread of C2")
	}
}

// TestKeyResolver_ScopedChatUsesTheChatsBinding: the project binding of a
// thread is its chat's. general goes to the shared planner; another agent
// keeps the thread's own key with the project's workspace and profile.
func TestKeyResolver_ScopedChatUsesTheChatsBinding(t *testing.T) {
	bound := ProjectBinding{Bound: true, Name: "demo", WorkspaceDir: "/proj", AccessProfile: "pinned"}
	r := NewKeyResolver(map[string]AgentOpts{"general": {}, "reviewer": {}}, &fakeDataSource{
		byChat: map[string]ProjectBinding{scopeChat: bound},
		byName: map[string]ProjectBinding{"demo": bound},
	})
	if key, _ := r.ResolveForChat("slack", "group", "C1#tT1", "general"); key != "project:demo:planner" {
		t.Errorf("ResolveForChat(thread, general) = %q, want the planner", key)
	}
	if key := r.KeyForChat("slack", "group", "C1#tT1", "general"); key != "project:demo:planner" {
		t.Errorf("KeyForChat(thread, general) = %q, want the planner", key)
	}
	key, opts := r.ResolveForChat("slack", "group", "C1#tT1", "reviewer")
	if key != "slack:group:C1#tT1:reviewer" || opts.Workspace != "/proj" || opts.AccessProfile != "pinned" {
		t.Errorf("ResolveForChat(thread, reviewer) = %q workspace %q profile %q, want the thread's key in /proj on pinned",
			key, opts.Workspace, opts.AccessProfile)
	}
	if got := r.AccessProfileForKey("slack:group:C1#tT1:reviewer"); got != "pinned" {
		t.Errorf("AccessProfileForKey(thread key) = %q, want the project's pinned", got)
	}
	if key, _ := r.ResolveForChat("slack", "group", "C9#tT1", "general"); key != "slack:group:C9#tT1:general" {
		t.Errorf("ResolveForChat(unbound thread) = %q, want the thread's own key", key)
	}
}
