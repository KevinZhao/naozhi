package cron

import (
	"context"
	"testing"
)

// stubNotifySender is a minimal cron.NotifySender used to assert the
// interface value survives in the atomic config snapshot. #725 replaced the
// former Platforms map[string]platform.Platform with this interface.
type stubNotifySender struct {
	lookups map[string]bool // platform name -> ok
}

func (s stubNotifySender) Lookup(name string) (PlatformReplier, bool) {
	if s.lookups[name] {
		return stubPlatformReplier{}, true
	}
	return nil, false
}

type stubPlatformReplier struct{}

func (stubPlatformReplier) MaxReplyLength() int               { return 4000 }
func (stubPlatformReplier) Split(text string, _ int) []string { return []string{text} }
func (stubPlatformReplier) Reply(context.Context, string, string) (string, error) {
	return "", nil
}

// TestConfigMaps_AtomicSnapshotNeverNil pins R249-ARCH-27 (#991): the config
// maps + the NotifySender are published as one immutable *cronConfigMaps
// behind an atomic.Pointer. NewScheduler must always Store a non-nil snapshot
// so the lock-free readers (notifyTarget / executeOpt) can index without a
// nil-ptr deref, and the snapshot must carry what the caller supplied —
// including the NotifySender interface value (#725).
func TestConfigMaps_AtomicSnapshotNeverNil(t *testing.T) {
	t.Parallel()

	agents := map[string]AgentOpts{"general": {Backend: "claude"}}
	cmds := map[string]string{"/foo": "foo-agent"}
	sender := stubNotifySender{lookups: map[string]bool{"feishu": true}}

	s := NewScheduler(SchedulerConfig{
		Router:        &fakeRouter{},
		Agents:        agents,
		AgentCommands: cmds,
		NotifySender:  sender,
	})

	cm := s.configMaps()
	if cm == nil {
		t.Fatal("configMaps() returned nil; NewScheduler must Store a non-nil snapshot")
	}
	if _, ok := cm.agents["general"]; !ok {
		t.Error("snapshot lost the 'general' agent")
	}
	if got := cm.agentCommands["/foo"]; got != "foo-agent" {
		t.Errorf("snapshot agentCommands[/foo] = %q, want foo-agent", got)
	}
	// #725: the NotifySender must survive in the same atomic snapshot as the
	// maps so notifyTarget reads it lock-free without a torn cross-field read.
	if cm.notifySender == nil {
		t.Fatal("snapshot lost the NotifySender (#725: must publish in the same atomic *cronConfigMaps)")
	}
	if _, ok := cm.notifySender.Lookup("feishu"); !ok {
		t.Error("snapshot NotifySender no longer resolves the 'feishu' platform")
	}

	// maps.Clone severs the alias: mutating the caller's map after
	// construction must not be visible through the snapshot.
	agents["general"] = AgentOpts{Backend: "tampered"}
	if cm.configMapsAgentBackend() == "tampered" {
		t.Error("snapshot aliased the caller-supplied agents map (maps.Clone expected)")
	}
}

// configMapsAgentBackend is a tiny test-local read helper so the alias check
// above does not reach into AgentOpts internals from the assertion line.
func (c *cronConfigMaps) configMapsAgentBackend() string {
	return c.agents["general"].Backend
}

// TestConfigMaps_NilMapsTolerated pins the nil-map back-compat path: a
// Scheduler built without any of the maps/sender must still return a non-nil
// snapshot whose (nil) maps index to zero values rather than panicking, and
// whose nil NotifySender is tolerated by notifyTarget (the WARN path).
func TestConfigMaps_NilMapsTolerated(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{Router: &fakeRouter{}})
	cm := s.configMaps()
	if cm == nil {
		t.Fatal("configMaps() must be non-nil even with no maps configured")
	}
	if cm.notifySender != nil {
		t.Error("nil NotifySender should round-trip as nil, not be synthesised")
	}
	if got := cm.agentCommands["missing"]; got != "" {
		t.Errorf("nil agentCommands index = %q, want empty", got)
	}
}
