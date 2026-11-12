package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// Choosing 全局默认 puts the session on the global credentials even where
// default_access_profile names a profile, and the choice survives a restart.
func TestSwitchAccessProfile_GlobalBeatsDefaultProfile(t *testing.T) {
	r, spawned, _, opts := switchRouter(t)
	r.backends.defaultAccessProfile = "isengard"
	ctx := context.Background()
	const key = "dashboard:direct:pin:general"

	sess, _, err := r.GetOrCreate(ctx, key, AgentOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if sess.AccessProfile() != "isengard" {
		t.Fatalf("first spawn profile = %q, want the default isengard", sess.AccessProfile())
	}
	sess.setSessionID("cc-sid-pin")
	if _, err := r.SwitchAccessProfile(ctx, key, "", nil); err != nil {
		t.Fatalf("switch to global: %v", err)
	}
	if _, _, err := r.GetOrCreate(ctx, key, AgentOpts{}); err != nil {
		t.Fatal(err)
	}
	docs, so := spawned(), opts()
	if got := docs[1]["awsCredentialExport"]; got != testGlobalCredExport {
		t.Errorf("after 全局默认 awsCredentialExport = %v, want the global one", got)
	}
	if len(so[1].EnvOverlay) != 0 {
		t.Errorf("after 全局默认 env overlay = %v, want none", so[1].EnvOverlay)
	}
	cur := r.SessionFor(key)
	if cur.AccessProfile() != "" || !cur.AccessProfilePinned() {
		t.Errorf("recorded profile=%q pinned=%v, want global and pinned", cur.AccessProfile(), cur.AccessProfilePinned())
	}
	if e, _ := sessionToStoreEntry(cur); !e.AccessProfilePinned {
		t.Error("the pin is not written to the session store")
	}
}

// A pre-spawn pick of 全局默认 also beats default_access_profile.
func TestSwitchAccessProfile_PendingGlobalBeatsDefault(t *testing.T) {
	r, spawned, _, _ := switchRouter(t)
	r.backends.defaultAccessProfile = "isengard"
	ctx := context.Background()
	const key = "dashboard:direct:pin-new:general"
	if _, err := r.SwitchAccessProfile(ctx, key, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.GetOrCreate(ctx, key, AgentOpts{}); err != nil {
		t.Fatal(err)
	}
	if got := spawned()[0]["awsCredentialExport"]; got != testGlobalCredExport {
		t.Errorf("first spawn awsCredentialExport = %v, want the global one", got)
	}
}

// The header's model follows the switch at once, not the closed CLI's.
func TestSwitchAccessProfile_ModelShownAtOnce(t *testing.T) {
	r, _, _, _ := switchRouter(t)
	ctx := context.Background()
	const key = "dashboard:direct:model:general"
	sess, _, err := r.GetOrCreate(ctx, key, AgentOpts{})
	if err != nil {
		t.Fatal(err)
	}
	sess.SetModel("claude-opus-5-5[1m]")
	model := "global.anthropic.claude-fable-5-2[1m]"
	if _, err := r.SwitchAccessProfile(ctx, key, "isengard", &model); err != nil {
		t.Fatal(err)
	}
	if got := r.SessionFor(key).Snapshot().Model; got != model {
		t.Errorf("snapshot model after switch = %q, want %q", got, model)
	}
}

// Deleting a session removes its session settings file.
func TestRemove_DeletesSessionSettings(t *testing.T) {
	r, _, _, _ := switchRouter(t)
	ctx := context.Background()
	const key = "dashboard:direct:del:general"
	if _, _, err := r.GetOrCreate(ctx, key, AgentOpts{AccessProfile: "isengard"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.spawn.sessionSettingsDir, persist.KeyHash(key)+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("session settings file not written: %v", err)
	}
	if !r.Remove(key) {
		t.Fatal("Remove returned false")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("session settings file still there after Remove: %v", err)
	}
}
