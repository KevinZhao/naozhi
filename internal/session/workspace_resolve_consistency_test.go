package session

import "testing"

// R245-ARCH-32 (#883): the spawn-time workspace base must be derived from
// the same resolveWorkspaceLocked path that Workspace uses, so the two
// can no longer drift. These tests pin the equivalence for the chat-level
// base tier (override-or-default), with opts/resume layered on top.

func newWorkspaceTestRouter(def string, overrides map[string]string) *Router {
	r := &Router{
		ss:         sessionStore{sessions: make(map[string]*ManagedSession)},
		defaultCWD: def,
	}
	r.wsStore.overrides = make(map[string]string)
	for k, v := range overrides {
		r.wsStore.overrides[k] = v
	}
	return r
}

func TestSpawnWorkspaceBaseMatchesGetWorkspace(t *testing.T) {
	const def = "/default/ws"
	const chatKey = "feishu:direct:user1"
	const agentKey = chatKey + ":general"

	cases := []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{
			name:      "no override falls back to default",
			overrides: nil,
			want:      def,
		},
		{
			name:      "per-chat override wins over default",
			overrides: map[string]string{chatKey: "/override/ws"},
			want:      "/override/ws",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newWorkspaceTestRouter(def, tc.overrides)

			// Workspace (the centralized resolver) view.
			gotGet := r.Workspace(chatKey)
			if gotGet != tc.want {
				t.Fatalf("Workspace = %q, want %q", gotGet, tc.want)
			}

			// Spawn-time base (no opts.Workspace, no resume) must agree.
			r.mu.Lock()
			sp := r.resolveSpawnParamsLocked(agentKey, "", AgentOpts{})
			r.mu.Unlock()
			if sp.Workspace != tc.want {
				t.Fatalf("resolveSpawnParamsLocked workspace = %q, want %q (Workspace=%q) — sources of truth drifted",
					sp.Workspace, tc.want, gotGet)
			}
		})
	}
}

func TestSpawnWorkspaceOptsOverrideWins(t *testing.T) {
	const def = "/default/ws"
	const chatKey = "feishu:direct:user1"
	const agentKey = chatKey + ":general"

	// Even with a per-chat override present, an explicit opts.Workspace
	// takes top priority (documented order: opts > per-chat > resume >
	// default).
	r := newWorkspaceTestRouter(def, map[string]string{chatKey: "/chat/override"})
	r.mu.Lock()
	sp := r.resolveSpawnParamsLocked(agentKey, "", AgentOpts{Workspace: "/opts/ws"})
	r.mu.Unlock()
	if sp.Workspace != "/opts/ws" {
		t.Fatalf("opts.Workspace should win: got %q, want /opts/ws", sp.Workspace)
	}
}
