package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// TestSessionKeyForCLIName: a sender name resolves to the Claude session
// spawned with it; Kiro sessions and unknown names resolve to nothing, and a
// shared name always to the same key.
func TestSessionKeyForCLIName(t *testing.T) {
	r := mkOverlayRouter(t)
	for _, s := range []struct{ key, backend, name string }{
		{"dashboard:direct:a:general", "claude", "naozhi"},
		{"dashboard:direct:b:general", "", "fix-build"}, // "" = the default backend, claude
		{"dashboard:direct:c:general", "kiro", "kiro-ws"},
		{"feishu:direct:z:general", "claude", "dup"},
		{"feishu:direct:y:general", "claude", "dup"},
	} {
		ms := newSessionWithID(s.key, "")
		ms.SetBackend(s.backend)
		ms.setCLISessionName(s.name)
		putT(r, s.key, ms)
	}
	cases := []struct{ name, want string }{
		{"naozhi", "dashboard:direct:a:general"},
		{"fix-build", "dashboard:direct:b:general"},
		{"kiro-ws", ""},
		{"dup", "feishu:direct:y:general"},
		{"terminal-ws", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := r.SessionKeyForCLIName(tc.name); got != tc.want {
			t.Errorf("SessionKeyForCLIName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCrossSessionInboundPolicy: a chat you talk to directly spawns with
// crossSessionInbound hold, other namespaces and Kiro without it, and a
// caller's own value wins. A shim spawned before the policy, whose overlay
// carries none, still rebuilds without drift, so a restart keeps it alive.
func TestCrossSessionInboundPolicy(t *testing.T) {
	r := mkOverlayRouter(t)
	r.spawn.sessionSettingsDir = t.TempDir()
	cases := []struct {
		key  string
		opts AgentOpts
		want string
	}{
		{"dashboard:direct:p1:general", AgentOpts{}, "hold"},
		{"feishu:group:oc_1#t9:code-reviewer", AgentOpts{}, "hold"},
		{"local:takeover:Users-me-ws:general", AgentOpts{}, "hold"},
		{"cron:job1", AgentOpts{}, ""},
		{"sys:attachment-gc", AgentOpts{}, ""},
		{sessionkey.ScratchKeyPrefix + "abc123:general:general", AgentOpts{}, ""}, // scratch.go's 4-segment shape
		{"project:naozhi:planner", AgentOpts{}, ""},
		{"dashboard:direct:p2:general", AgentOpts{Backend: "kiro"}, ""},
		{"dashboard:direct:p3:general", AgentOpts{CrossSessionInbound: "accept"}, "accept"},
	}
	for _, tc := range cases {
		tc.opts.Workspace = "/ws"
		if sp := resolveT(r, tc.key, "", tc.opts); sp.Overlay.CrossSessionInbound != tc.want {
			t.Errorf("%s: crossSessionInbound = %q, want %q", tc.key, sp.Overlay.CrossSessionInbound, tc.want)
		}
	}

	key := "dashboard:direct:old:general"
	s := newSessionWithID(key, "sess-old")
	s.SetBackend("claude")
	putT(r, key, s)
	r.spawn.sessionSettingsDir = "" // spawned before the policy: no hold
	state, _ := spawnShimState(t, r, key, "", AgentOpts{Backend: "claude", Workspace: "/ws"})
	if state.SpawnOverlay.CrossSessionInbound != "" {
		t.Fatalf("pre-policy spawn holds: %+v", state.SpawnOverlay)
	}
	r.spawn.sessionSettingsDir = t.TempDir()
	wrapper, backendID := r.backends.wrapperFor("claude")
	if drift, stored, current := driftArgsFor(r).shimArgsDrift(wrapper, backendID, state, s); drift {
		t.Fatalf("pre-policy shim reads as drift\n  stored:  %v\n  current: %v", stored, current)
	}
}
