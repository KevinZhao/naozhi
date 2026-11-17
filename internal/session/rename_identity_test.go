package session

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/history/naozhilog"
)

// A promoted aside resumes on the account it ran on: the access profile and
// its pin cross the rename, so the resume lock still holds for the new key
// instead of default_access_profile taking it.
func TestRenameSession_KeepsAccessProfile(t *testing.T) {
	r := NewRouter(RouterConfig{AccessProfiles: map[string]AccessProfile{"team-bedrock": {}, "other": {}}})
	r.backends.defaultAccessProfile = "other"
	const oldKey = "scratch:abc:general:general"
	const newKey = "dashboard:direct:alice:aside-general-x1"

	s := newSessionWithID(oldKey, "sid-ap")
	s.SetAccessProfile("team-bedrock")
	s.SetAccessProfilePinned(true)
	s.SetModel("claude-opus-5-5")
	s.setCLISessionName("aside-name")
	r.ss.Update(func(tx sessTx) { tx.Put(oldKey, s) })

	if !r.RenameSession(oldKey, newKey) {
		t.Fatal("RenameSession returned false")
	}
	got := r.SessionFor(newKey)
	if got.AccessProfile() != "team-bedrock" || !got.AccessProfilePinned() {
		t.Fatalf("after rename profile=%q pinned=%v, want team-bedrock pinned", got.AccessProfile(), got.AccessProfilePinned())
	}
	if got.Model() != "claude-opus-5-5" || got.CLISessionName() != "aside-name" {
		t.Errorf("after rename model=%q name=%q, want the source's", got.Model(), got.CLISessionName())
	}
	var resolved string
	r.ss.Update(func(tx sessTx) { resolved = r.resolveSpawnParams(tx, newKey, "sid-ap", AgentOpts{}).AccessProfileID })
	if resolved != "team-bedrock" {
		t.Errorf("resume of the promoted key resolves profile %q, want team-bedrock", resolved)
	}
}

// After a promote the live process's events go to the new key's event log,
// the one the renamed session's history source and Remove read.
func TestRenameSession_MovesPersistSink(t *testing.T) {
	r, dir := newEventLogRouter(t)
	const oldKey = "scratch:abc123:general:general"
	const newKey = "dashboard:direct:alice:aside-general-x1"
	proc := newEventLogProc(newBlockingCloseProc())
	close(proc.release)
	old := installSession(t, r, oldKey, proc)
	r.hist.installPersistSink(context.Background(), old, proc, oldKey)

	if !r.RenameSession(oldKey, newKey) {
		t.Fatal("RenameSession returned false")
	}
	// A late bind for the old struct must not take the sink back.
	r.hist.installPersistSink(context.Background(), old, proc, oldKey)
	proc.log.Append(clievent.EventEntry{UUID: "after-promote", Time: time.Now().UnixMilli(), Type: "user", Summary: "after-promote"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.hist.persister.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	count := func(key string) int {
		got, err := naozhilog.New(dir, key).LoadLatest(context.Background(), 10)
		if err != nil {
			t.Fatalf("LoadLatest(%s): %v", key, err)
		}
		return len(got)
	}
	if n := count(newKey); n != 1 {
		t.Errorf("new key's log holds %d entries, want 1", n)
	}
	if n := count(oldKey); n != 0 {
		t.Errorf("old scratch key's log holds %d entries, want 0", n)
	}
}
