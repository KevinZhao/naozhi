package session

import (
	"testing"

	"github.com/naozhi/naozhi/internal/shim"
)

// ---------------------------------------------------------------------------
// adoptableShimKey — #1875
// ---------------------------------------------------------------------------

// TestAdoptableShimKey pins which discovered-but-unpersisted live shims may be
// rebuilt+reconnected (adopt) versus left to the orphan-kill path. The accept
// set must mirror sessionToStoreEntry's persist rules: sys:/scratch: never
// persist, so a live shim under such a key is an anomaly we refuse to
// resurrect; malformed keys are evidence of tampering. Regular dashboard / IM
// / cron keys are adoptable — those are exactly the sessions that vanished
// when a <30s-old or session_id-less shim was misjudged as orphan.
func TestAdoptableShimKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want bool
	}{
		{"dashboard direct", "dashboard:direct:2026-06-07-150009-2-naozhi:general", true},
		{"cron", "cron:09a61c45ad4c76ba", true},
		{"feishu group", "feishu:group:oc_abc123:general", true},
		{"sys daemon rejected", "sys:autotitler", false},
		{"scratch rejected", "scratch:dashboard:direct:abc:general", false},
		{"empty rejected", "", false},
		{"malformed control byte rejected", "dashboard:direct:\x00bad:general", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := adoptableShimKey(tc.key); got != tc.want {
				t.Errorf("adoptableShimKey(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// adoptLiveShimLocked — #1875
// ---------------------------------------------------------------------------

// TestAdoptLiveShimLocked_PublishesSession verifies that rebuilding a session
// from a shim state file installs it into the router maps with the state's
// key/workspace/session_id wired through, and indexes session_id → key. This
// is the construction half of the fix: once the session is published,
// classifyShimState observes sessFound=true and routes to reconnect instead of
// shimStateOrphan.
func TestAdoptLiveShimLocked_PublishesSession(t *testing.T) {
	r := newTestRouter(3)
	r.ss.idToKey = map[string]string{}
	r.kid.ids = map[string]bool{}

	const key = "dashboard:direct:2026-06-07-150009-2-naozhi:general"
	state := shim.State{
		Key:       key,
		SessionID: "1fbbcac7-9f79-4a7b-b2bb-09f98582f699",
		Workspace: "/home/ec2-user/workspace/naozhi",
		Backend:   "claude",
		ShimPID:   3637949,
	}
	wrapper, backendID := r.wrapperFor(state.Backend)

	r.mu.Lock()
	got := r.adoptLiveShimLocked(state, backendID, wrapper)
	r.mu.Unlock()

	if got == nil {
		t.Fatal("adoptLiveShimLocked returned nil")
	}
	r.mu.Lock()
	published, ok := r.ss.sessions[key]
	mappedKey := r.ss.idToKey[state.SessionID]
	r.mu.Unlock()

	if !ok {
		t.Fatal("session not published into r.ss.sessions")
	}
	if published != got {
		t.Error("returned session differs from the published one")
	}
	if published.Workspace() != state.Workspace {
		t.Errorf("workspace = %q, want %q", published.Workspace(), state.Workspace)
	}
	if published.getSessionID() != state.SessionID {
		t.Errorf("sessionID = %q, want %q", published.getSessionID(), state.SessionID)
	}
	if mappedKey != key {
		t.Errorf("idToKey[%q] = %q, want %q", state.SessionID, mappedKey, key)
	}
}

// TestClassifyShimState_AdoptedSessionReconnects locks the post-adopt branch:
// a freshly adopted session has no process (hasLiveProc=false) and a wrapper
// exists (wrapperNil=false); with no args drift it must classify as reconnect,
// NOT skip and NOT orphan. This is the whole point of the fix — without it the
// adopted-then-classified shim would either be killed (orphan) or ignored
// (skip) and the live conversation would still be lost.
func TestClassifyShimState_AdoptedSessionReconnects(t *testing.T) {
	// (spawning=false, sessFound=true after adopt, hasLiveProc=false,
	//  wrapperNil=false, drift=false)
	if got := classifyShimState(false, true, false, false, false); got != shimStateReconnect {
		t.Fatalf("adopted session classify = %v, want shimStateReconnect", got)
	}
}

// TestAdoptLiveShimLocked_EmptySessionID covers the most important crash window:
// a shim spawned <30s before the crash never received its system/init
// session_id, so the state file's SessionID is empty. Adopt must still publish
// the session (so it reconnects rather than being killed) — it simply does not
// add a session_id → key index entry, matching restoreSessionFromEntry's
// own empty-id guard.
func TestAdoptLiveShimLocked_EmptySessionID(t *testing.T) {
	r := newTestRouter(3)
	r.ss.idToKey = map[string]string{}
	r.kid.ids = map[string]bool{}

	const key = "dashboard:direct:2026-06-07-150409-3-naozhi:general"
	state := shim.State{
		Key:       key,
		SessionID: "", // system/init never fired before the crash
		Workspace: "/home/ec2-user/workspace/naozhi",
		Backend:   "claude",
		ShimPID:   3659980,
	}
	wrapper, backendID := r.wrapperFor(state.Backend)

	r.mu.Lock()
	r.adoptLiveShimLocked(state, backendID, wrapper)
	_, ok := r.ss.sessions[key]
	idxLen := len(r.ss.idToKey)
	r.mu.Unlock()

	if !ok {
		t.Fatal("session with empty session_id must still be published for reconnect")
	}
	if idxLen != 0 {
		t.Errorf("idToKey should stay empty when session_id is empty, got %d entries", idxLen)
	}
}
