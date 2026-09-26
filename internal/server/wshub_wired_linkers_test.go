package server

import (
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/agentlink"
	"github.com/naozhi/naozhi/internal/subagent"
)

// stubAgentLinker is a minimal AgentLinker for the wiring tests; its methods
// are no-ops because the tests only look at identity.
type stubAgentLinker struct {
	id string
}

func (s *stubAgentLinker) OnResolve(fn func(taskID, toolUseID, internalAgentID string)) {
}
func (s *stubAgentLinker) Query(taskID string) (subagent.LinkInfo, bool) {
	return subagent.LinkInfo{}, false
}
func (s *stubAgentLinker) QueryOrResolveFast(taskID string) (subagent.LinkInfo, bool) {
	return subagent.LinkInfo{}, false
}
func (s *stubAgentLinker) ProjectSessionDir() string { return "" }

// secondStubAgentLinker is structurally identical to stubAgentLinker but
// has a different dynamic type.
type secondStubAgentLinker struct {
	id string
}

func (s *secondStubAgentLinker) OnResolve(fn func(taskID, toolUseID, internalAgentID string)) {
}
func (s *secondStubAgentLinker) Query(taskID string) (subagent.LinkInfo, bool) {
	return subagent.LinkInfo{}, false
}
func (s *secondStubAgentLinker) QueryOrResolveFast(taskID string) (subagent.LinkInfo, bool) {
	return subagent.LinkInfo{}, false
}
func (s *secondStubAgentLinker) ProjectSessionDir() string { return "" }

// TestWiredLinkers_WireOnce: a linker's callbacks are installed once per
// linker. Identity is the interface key (dynamic type, value): the same
// pointer wires once, a different pointer wires again, and so does a
// different type — which is why a producer must hand over one canonical
// AgentLinker per process, or a thin adapter would double-fire OnResolve.
// After releaseLinkers nothing wires.
func TestWiredLinkers_WireOnce(t *testing.T) {
	t.Parallel()
	r := newTailerRegistry(nil, "")

	a := &stubAgentLinker{id: "a"}
	if !r.wireOnce(a) {
		t.Fatal("first wiring of a linker was refused")
	}
	if r.wireOnce(a) {
		t.Error("the same linker wired twice")
	}
	if !r.wireOnce(&stubAgentLinker{id: "b"}) {
		t.Error("a different linker of the same type was refused")
	}
	var c agentlink.AgentLinker = &secondStubAgentLinker{id: "a"}
	if !r.wireOnce(c) {
		t.Error("a linker of a different dynamic type was refused")
	}

	r.releaseLinkers()
	if r.wireOnce(&stubAgentLinker{id: "late"}) {
		t.Error("a linker wired after releaseLinkers")
	}
}

// TestTailerRegistry_RefusesPathsOutsideAllowedRoot: with allowedRoot set, a
// tailer is never started on a JSONL file outside it.
func TestTailerRegistry_RefusesPathsOutsideAllowedRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	r := newTailerRegistry(nil, root)
	outside := filepath.Join(t.TempDir(), "agent.jsonl")
	if tl, ok := r.ensureTailer("k", "task-1", "tool-1", outside); ok || tl != nil {
		t.Fatalf("ensureTailer accepted %q outside allowedRoot %q", outside, root)
	}
	if n := r.count.Load(); n != 0 {
		t.Errorf("%d tailers registered after a refused path", n)
	}

	hub := NewHub(HubOptions{Router: session.NewRouter(session.RouterConfig{}), AllowedRoot: root})
	defer hub.Shutdown()
	if hub.tailers.allowedRoot != root {
		t.Errorf("NewHub's tailers allowedRoot = %q, want HubOptions.AllowedRoot %q", hub.tailers.allowedRoot, root)
	}
}

// recordingLinker captures its OnResolve registrations and answers Query
// from a fixed table.
type recordingLinker struct {
	resolvers []func(taskID, toolUseID, internalAgentID string)
	paths     map[string]string
}

func (l *recordingLinker) OnResolve(fn func(taskID, toolUseID, internalAgentID string)) {
	l.resolvers = append(l.resolvers, fn)
}
func (l *recordingLinker) Query(taskID string) (subagent.LinkInfo, bool) {
	p, ok := l.paths[taskID]
	return subagent.LinkInfo{JSONLPath: p}, ok
}
func (l *recordingLinker) QueryOrResolveFast(taskID string) (subagent.LinkInfo, bool) {
	return l.Query(taskID)
}
func (l *recordingLinker) ProjectSessionDir() string { return "" }

type recordingTaskDone struct{ hooks int }

func (d *recordingTaskDone) SetOnAgentTaskDone(func(taskID, status string)) { d.hooks++ }

// TestWireLinker_InstallsOnceAndTailsResolvedAgents: re-subscribes wire a
// linker's callbacks once; a resolved agent with a JSONL path starts a silent
// tailer, a tombstone or an unknown task does not.
func TestWireLinker_InstallsOnceAndTailsResolvedAgents(t *testing.T) {
	hub := NewHub(HubOptions{Router: session.NewRouter(session.RouterConfig{})})
	defer hub.Shutdown()
	jsonl := filepath.Join(t.TempDir(), "agent.jsonl")
	l := &recordingLinker{paths: map[string]string{"task-1": jsonl}}
	done := &recordingTaskDone{}

	hub.wireLinker("k", l, done)
	hub.wireLinker("k", l, done) // a re-subscribe
	if len(l.resolvers) != 1 || done.hooks != 1 {
		t.Fatalf("OnResolve registered %d times, task_done hooked %d times; want 1 and 1", len(l.resolvers), done.hooks)
	}

	resolve := l.resolvers[0]
	resolve("task-1", "tool-1", "") // tombstone
	resolve("task-2", "tool-2", "agent-2")
	if n := hub.tailers.count.Load(); n != 0 {
		t.Fatalf("%d tailers after a tombstone and an unknown task, want 0", n)
	}
	resolve("task-1", "tool-1", "agent-1")
	if n := hub.tailers.count.Load(); n != 1 {
		t.Errorf("%d tailers after resolving task-1, want 1", n)
	}
}
