package session

import (
	"fmt"
	"testing"
)

// TestSetWorkspace_EvictsLRUWhenAtCapacity pins the capacity self-healing
// (#cwd-overflow): once the override map hits maxWorkspaceOverrides, a brand-new
// SetWorkspace must EVICT the least-recently-set session-less override rather
// than dropping the new write (which used to silently fall the new session back
// to defaultCWD = workspace root).
func TestSetWorkspace_EvictsLRUWhenAtCapacity(t *testing.T) {
	r := NewRouter(RouterConfig{Workspace: "/default"})

	// Fill exactly to capacity with session-less one-shot keys, set in order
	// so chat0 is least-recently-set.
	for i := 0; i < maxWorkspaceOverrides; i++ {
		r.SetWorkspace(fmt.Sprintf("dashboard:direct:k%d", i), fmt.Sprintf("/ws/%d", i))
	}
	if got := len(r.wsStore.overrides); got != maxWorkspaceOverrides {
		t.Fatalf("precondition: overrides=%d want %d", got, maxWorkspaceOverrides)
	}

	// New key at capacity → must be accepted (LRU eviction frees a slot).
	newKey := "dashboard:direct:fresh-JD"
	r.SetWorkspace(newKey, "/home/ec2-user/workspace/JD")

	if got := r.GetWorkspace(newKey); got != "/home/ec2-user/workspace/JD" {
		t.Fatalf("new override dropped at capacity: GetWorkspace(%q)=%q want the project dir (regression: silent drop)", newKey, got)
	}
	// The least-recently-set key (k0) is the victim.
	if got := r.GetWorkspace("dashboard:direct:k0"); got != "/default" {
		t.Errorf("expected k0 (oldest) evicted → resolves to default; got %q", got)
	}
	// A more-recently-set key survives.
	if got := r.GetWorkspace("dashboard:direct:k500"); got != "/ws/500" {
		t.Errorf("k500 should survive eviction; got %q", got)
	}
	// Size invariant: still bounded.
	if got := len(r.wsStore.overrides); got != maxWorkspaceOverrides {
		t.Errorf("size after eviction=%d want %d (cap held)", got, maxWorkspaceOverrides)
	}
	// seq map must not outlive its override.
	if len(r.wsStore.seq) != len(r.wsStore.overrides) {
		t.Errorf("seq map drift: seq=%d overrides=%d (must stay in lockstep)", len(r.wsStore.seq), len(r.wsStore.overrides))
	}
}

// TestSetWorkspace_NeverEvictsLiveSession ensures an override whose chat has a
// live session is protected from eviction even when it is the oldest entry.
func TestSetWorkspace_NeverEvictsLiveSession(t *testing.T) {
	r := NewRouter(RouterConfig{Workspace: "/default"})

	// chat0 is the oldest override AND has a live session.
	liveChat := "dashboard:direct:k0"
	r.SetWorkspace(liveChat, "/ws/live")
	r.ss.byChat[liveChat] = map[string]struct{}{liveChat + ":general": {}}

	for i := 1; i < maxWorkspaceOverrides; i++ {
		r.SetWorkspace(fmt.Sprintf("dashboard:direct:k%d", i), fmt.Sprintf("/ws/%d", i))
	}

	// New key at capacity: eviction must pick the oldest SESSION-LESS key
	// (k1), NOT the live chat0.
	r.SetWorkspace("dashboard:direct:fresh", "/ws/fresh")

	if got := r.GetWorkspace(liveChat); got != "/ws/live" {
		t.Errorf("live session's override was evicted: got %q want /ws/live", got)
	}
	if got := r.GetWorkspace("dashboard:direct:k1"); got != "/default" {
		t.Errorf("oldest session-less key k1 should be the victim; got %q", got)
	}
}

// TestSetWorkspace_DropsWhenAllLive verifies the DoS bound still holds: if every
// override belongs to a live chat, a new key is dropped (size never exceeds cap)
// rather than evicting an active conversation's cwd.
func TestSetWorkspace_DropsWhenAllLive(t *testing.T) {
	r := NewRouter(RouterConfig{Workspace: "/default"})
	for i := 0; i < maxWorkspaceOverrides; i++ {
		k := fmt.Sprintf("dashboard:direct:k%d", i)
		r.SetWorkspace(k, fmt.Sprintf("/ws/%d", i))
		r.ss.byChat[k] = map[string]struct{}{k + ":general": {}}
	}
	r.SetWorkspace("dashboard:direct:overflow", "/ws/overflow")

	if got := r.GetWorkspace("dashboard:direct:overflow"); got != "/default" {
		t.Errorf("with all overrides live, new key must be dropped (DoS bound): got %q want /default", got)
	}
	if got := len(r.wsStore.overrides); got != maxWorkspaceOverrides {
		t.Errorf("size=%d want %d — must never exceed cap", got, maxWorkspaceOverrides)
	}
}

// TestSetWorkspace_DiskLoadedKeysEvictedFirst pins the intended priority: keys
// without a seq entry (loaded from disk / Takeover-installed) sort as oldest, so
// the stale historical one-shot keys that overflow the map are evicted before
// any key set in this process via SetWorkspace.
func TestSetWorkspace_DiskLoadedKeysEvictedFirst(t *testing.T) {
	r := NewRouter(RouterConfig{Workspace: "/default"})

	// Simulate disk-loaded keys: present in overrides, absent from seq.
	for i := 0; i < maxWorkspaceOverrides-1; i++ {
		r.wsStore.overrides[fmt.Sprintf("dashboard:direct:disk%d", i)] = fmt.Sprintf("/disk/%d", i)
	}
	// One key set the normal way (has a seq → newest).
	seqKey := "dashboard:direct:seqd"
	r.SetWorkspace(seqKey, "/ws/seqd")
	if got := len(r.wsStore.overrides); got != maxWorkspaceOverrides {
		t.Fatalf("precondition: overrides=%d want %d", got, maxWorkspaceOverrides)
	}

	// New key forces one eviction — must hit a disk-loaded key, never the
	// seq-tracked one.
	r.SetWorkspace("dashboard:direct:fresh", "/ws/fresh")
	if got := r.GetWorkspace(seqKey); got != "/ws/seqd" {
		t.Errorf("seq-tracked key evicted before disk-loaded keys: got %q want /ws/seqd", got)
	}
}
