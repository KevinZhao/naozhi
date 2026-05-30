package cron

import (
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
)

// TestConfigMaps_AtomicSnapshotNeverNil pins R249-ARCH-27 (#991): the three
// config maps are published as one immutable *cronConfigMaps behind an
// atomic.Pointer. NewScheduler must always Store a non-nil snapshot so the
// lock-free readers (notifyTarget / executeOpt) can index without a nil-ptr
// deref, and the snapshot must carry the maps the caller supplied.
func TestConfigMaps_AtomicSnapshotNeverNil(t *testing.T) {
	t.Parallel()

	agents := map[string]AgentOpts{"general": {Backend: "claude"}}
	cmds := map[string]string{"/foo": "foo-agent"}
	plats := map[string]platform.Platform{"feishu": nil}

	s := NewScheduler(SchedulerConfig{
		Router:        &fakeRouter{},
		Agents:        agents,
		AgentCommands: cmds,
		Platforms:     plats,
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
	if _, ok := cm.platforms["feishu"]; !ok {
		t.Error("snapshot lost the 'feishu' platform key")
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
// Scheduler built without any of the three maps must still return a non-nil
// snapshot whose (nil) maps index to zero values rather than panicking.
func TestConfigMaps_NilMapsTolerated(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{Router: &fakeRouter{}})
	cm := s.configMaps()
	if cm == nil {
		t.Fatal("configMaps() must be non-nil even with no maps configured")
	}
	if _, ok := cm.platforms["missing"]; ok {
		t.Error("nil platforms map should index to zero value, not a present key")
	}
	if got := cm.agentCommands["missing"]; got != "" {
		t.Errorf("nil agentCommands index = %q, want empty", got)
	}
}
