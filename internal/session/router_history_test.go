package session

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/history/kirojsonl"
	"github.com/naozhi/naozhi/internal/history/merged"
)

// instrumentedSource is a history.Source / cli.HistorySource stub used to
// verify that attachHistorySource picks the right wrapper's factory.
// Each instance is tagged with a backend ID so a session that ends up
// pointing at the wrong backend's source produces an obvious assertion
// failure instead of a silent passthrough.
type instrumentedSource struct {
	tag   string
	calls atomic.Int64
}

func (s *instrumentedSource) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]cli.EventEntry, error) {
	s.calls.Add(1)
	return nil, nil
}

// makeRoutedRouter constructs a Router with two wrappers (claude + kiro)
// and registers a unique factory per backend so attachHistorySource
// dispatching can be observed via the returned source's tag.
//
// The factory registrations use deliberately unique backend IDs
// ("claude-routed" / "kiro-routed") so they cannot collide with
// production lookups; the cli registry has no Unregister hook today
// and these IDs are leaked into the registry for the rest of the
// test binary's lifetime — harmless because no production code path
// looks them up.
//
// Tests that share the registry must NOT call t.Parallel() inside
// the same package: Go runs intra-package tests serially by default,
// so the closure-captured *instrumentedSource will be consistent
// across the test's entire body. Cross-package parallelism is fine
// because each package gets its own test binary and its own copy of
// the registry.
func makeRoutedRouter(t *testing.T, defaultBackend string) (r *Router, claudeSrc, kiroSrc *instrumentedSource) {
	t.Helper()
	claudeSrc = &instrumentedSource{tag: "claude"}
	kiroSrc = &instrumentedSource{tag: "kiro"}

	cli.RegisterHistoryFactory("claude-routed", func(s cli.HistorySessionView, deps cli.HistoryWiring) cli.HistorySource {
		return claudeSrc
	})
	cli.RegisterHistoryFactory("kiro-routed", func(s cli.HistorySessionView, deps cli.HistoryWiring) cli.HistorySource {
		return kiroSrc
	})

	r = &Router{
		sessions:        make(map[string]*ManagedSession),
		claudeDir:       "/claude/dir",
		kiroSessionsDir: "/kiro/dir",
	}
	r.bkStore.wrappers = map[string]*cli.Wrapper{
		"claude-routed": cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "claude-routed"),
		"kiro-routed":   cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "kiro-routed"),
	}
	r.bkStore.defaultBackend = defaultBackend
	r.bkStore.backendOverrides = make(map[string]string)
	r.wsStore.overrides = make(map[string]string)
	r.bkStore.wrapper = r.bkStore.wrappers[defaultBackend]
	return
}

// TestAttachHistorySource_RoutesToBackendWrapper exercises the
// multi-wrapper dispatch contract: a session whose Backend() reports
// "kiro-routed" must end up with the kiro wrapper's source, not the
// router-default claude one. Without this routing the dashboard would
// see the wrong on-disk format for every non-default backend session.
func TestAttachHistorySource_RoutesToBackendWrapper(t *testing.T) {
	r, claudeSrc, kiroSrc := makeRoutedRouter(t, "claude-routed")

	s := &ManagedSession{key: "feishu:direct:bob:general"}
	s.SetBackend("kiro-routed")
	s.setWorkspace("/tmp/ws")

	r.attachHistorySource(s)

	src := s.loadHistorySource()
	if src == nil {
		t.Fatal("attachHistorySource left source nil")
	}
	// EventLogDir is empty in the test router → fallback is installed
	// directly (no MergedSource). Dispatch the LoadBefore call to
	// confirm it lands on kiroSrc and not claudeSrc.
	if _, err := src.LoadBefore(context.Background(), 0, 10); err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if claudeSrc.calls.Load() != 0 {
		t.Errorf("claude source called %d times; want 0 (session is kiro)", claudeSrc.calls.Load())
	}
	if kiroSrc.calls.Load() != 1 {
		t.Errorf("kiro source calls = %d; want 1", kiroSrc.calls.Load())
	}
}

// TestAttachHistorySource_FallsBackToDefaultWhenSessionBackendEmpty
// pins the empty-Backend() path: a session whose backend was never
// recorded (legacy entries in sessions.json, or a freshly-restored
// session before SetBackend ran) must use the router default. The old
// dispatch fell through to claude unconditionally; the new dispatch
// must keep that behaviour for empty-backend sessions specifically.
func TestAttachHistorySource_FallsBackToDefaultWhenSessionBackendEmpty(t *testing.T) {
	r, claudeSrc, kiroSrc := makeRoutedRouter(t, "claude-routed")

	s := &ManagedSession{key: "feishu:direct:carol:general"}
	// SetBackend deliberately omitted — Backend() returns "".
	s.setWorkspace("/tmp/ws")

	r.attachHistorySource(s)

	src := s.loadHistorySource()
	if src == nil {
		t.Fatal("attachHistorySource left source nil")
	}
	if _, err := src.LoadBefore(context.Background(), 0, 10); err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if claudeSrc.calls.Load() != 1 {
		t.Errorf("claude (default) source calls = %d; want 1", claudeSrc.calls.Load())
	}
	if kiroSrc.calls.Load() != 0 {
		t.Errorf("kiro source called %d times; want 0", kiroSrc.calls.Load())
	}
}

// TestAttachHistorySource_NilWrapperUsesNoop covers the
// missing-wrapper path: a session whose backend has no registered
// wrapper falls back to the router default; if there's no default
// either, NewHistorySource on a nil receiver yields NoopHistorySource.
// The result must still be a non-nil source so EventEntriesBeforeCtx
// doesn't crash.
func TestAttachHistorySource_NilWrapperUsesNoop(t *testing.T) {
	t.Parallel()
	r := &Router{
		sessions: make(map[string]*ManagedSession),
	}
	r.bkStore.wrappers = map[string]*cli.Wrapper{}
	r.bkStore.defaultBackend = ""
	r.bkStore.backendOverrides = make(map[string]string)
	r.wsStore.overrides = make(map[string]string)
	// r.bkStore.wrapper intentionally nil.

	s := &ManagedSession{key: "feishu:direct:dave:general"}
	s.SetBackend("orphan-backend")
	s.setWorkspace("/tmp/ws")

	r.attachHistorySource(s)
	src := s.loadHistorySource()
	if src == nil {
		t.Fatal("attachHistorySource produced nil source despite nil wrapper")
	}
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Errorf("LoadBefore on noop returned err: %v", err)
	}
	if got != nil {
		t.Errorf("noop source returned %d entries; want nil", len(got))
	}
}

// TestAttachHistorySource_NoEventLogDirSkipsMerged pins the opt-out
// branch of the composition: when EventLogDir is empty, the router
// installs the backend's source directly rather than wrapping it in
// merged.Source. Wrapping would force a naozhilog read that has
// nowhere to go, masking the real source's behaviour and adding
// allocation overhead per page request.
func TestAttachHistorySource_NoEventLogDirSkipsMerged(t *testing.T) {
	r, _, _ := makeRoutedRouter(t, "claude-routed")
	// Confirm the router actually has eventLogDir empty. (Default zero
	// from the literal above.)
	if r.eventLogDir != "" {
		t.Fatalf("test setup regression: eventLogDir = %q", r.eventLogDir)
	}

	s := &ManagedSession{key: "feishu:direct:eve:general"}
	s.SetBackend("claude-routed")
	r.attachHistorySource(s)

	src := s.loadHistorySource()
	if src == nil {
		t.Fatal("source nil")
	}
	if _, isMerged := src.(*merged.Source); isMerged {
		t.Errorf("EventLogDir empty but source wrapped in merged.Source: %T", src)
	}
}

// TestAttachHistorySource_WithEventLogDirInstallsMerged is the
// complement of the previous test: when eventLogDir is set, the
// composition must wrap the backend source inside merged.Source so
// naozhilog (local tier) and the backend source (fallback tier) both
// participate in pagination. A regression here would silently hide the
// naozhilog images on the upgrade path.
func TestAttachHistorySource_WithEventLogDirInstallsMerged(t *testing.T) {
	r, _, _ := makeRoutedRouter(t, "claude-routed")
	r.eventLogDir = t.TempDir()

	s := &ManagedSession{key: "feishu:direct:frank:general"}
	s.SetBackend("claude-routed")
	r.attachHistorySource(s)

	src := s.loadHistorySource()
	if src == nil {
		t.Fatal("source nil")
	}
	m, ok := src.(*merged.Source)
	if !ok {
		t.Fatalf("EventLogDir set but source is %T; want *merged.Source", src)
	}
	if m.Local == nil {
		t.Errorf("merged.Source.Local nil; want naozhilog")
	}
	if m.Fallback == nil {
		t.Errorf("merged.Source.Fallback nil; want backend factory result")
	}
}

// TestAttachHistorySource_NilSession is a defensive guard so a future
// refactor that accidentally calls attachHistorySource(nil) doesn't
// crash. The current implementation early-returns on nil — this
// pinning test makes that contract explicit.
func TestAttachHistorySource_NilSession(t *testing.T) {
	t.Parallel()
	r := &Router{}
	// Must not panic.
	r.attachHistorySource(nil)
}

// TestRouter_KiroSessionsDirRoundTrip verifies the new RouterConfig
// field reaches the per-call HistoryWiring. Without this the
// Sprint 1c kirojsonl factory would receive an empty dir even after
// cmd-level wiring lands.
func TestRouter_KiroSessionsDirRoundTrip(t *testing.T) {
	t.Parallel()
	saw := ""
	cli.RegisterHistoryFactory("kiro-rt-probe", func(s cli.HistorySessionView, deps cli.HistoryWiring) cli.HistorySource {
		saw = deps.KiroSessionsDir
		return cli.NoopHistorySource{}
	})
	r := &Router{
		sessions:        make(map[string]*ManagedSession),
		kiroSessionsDir: "/the/kiro/dir",
	}
	r.bkStore.wrappers = map[string]*cli.Wrapper{
		"kiro-rt-probe": cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "kiro-rt-probe"),
	}
	r.bkStore.defaultBackend = "kiro-rt-probe"
	r.bkStore.backendOverrides = make(map[string]string)
	r.wsStore.overrides = make(map[string]string)
	r.bkStore.wrapper = r.bkStore.wrappers["kiro-rt-probe"]

	s := &ManagedSession{key: "feishu:direct:greta:general"}
	s.SetBackend("kiro-rt-probe")

	r.attachHistorySource(s)

	if saw != "/the/kiro/dir" {
		t.Errorf("HistoryWiring.KiroSessionsDir = %q; want /the/kiro/dir", saw)
	}
}

// TestAttachHistorySource_KiroBackendUsesKirojsonl pins the Sprint 1c
// wiring: a multi-wrapper router with a real "kiro" wrapper must end up
// with a *kirojsonl.Source as the fallback for kiro sessions, never the
// claude factory. Anchors the init()-side-effect chain (router blank
// import → cli registry → wrapper.historyFactory → attachHistorySource)
// against a regression where, e.g., the kirojsonl import got pruned
// during Sprint 1b consolidation and kiro sessions silently degraded
// to NoopHistorySource.
func TestAttachHistorySource_KiroBackendUsesKirojsonl(t *testing.T) {
	t.Parallel()
	r := &Router{
		sessions:        make(map[string]*ManagedSession),
		claudeDir:       "/claude/dir",
		kiroSessionsDir: "/kiro/sessions/cli",
	}
	r.bkStore.wrappers = map[string]*cli.Wrapper{
		"claude": cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "claude"),
		"kiro":   cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "kiro"),
	}
	r.bkStore.defaultBackend = "claude"
	r.bkStore.backendOverrides = make(map[string]string)
	r.wsStore.overrides = make(map[string]string)
	r.bkStore.wrapper = r.bkStore.wrappers["claude"]

	s := &ManagedSession{key: "feishu:direct:harry:general"}
	s.SetBackend("kiro")
	s.setWorkspace("/tmp/ws")

	r.attachHistorySource(s)

	src := s.loadHistorySource()
	if src == nil {
		t.Fatal("attachHistorySource left source nil")
	}
	// EventLogDir is empty → fallback installed directly, no merged wrapper.
	if _, ok := src.(*kirojsonl.Source); !ok {
		t.Fatalf("kiro session got %T; want *kirojsonl.Source", src)
	}
}

// Compile-time assertion that ManagedSession satisfies the cli
// HistorySessionView interface. If a future refactor renames a method
// (e.g. SessionKey → Key) the build fails here long before the
// dashboard pagination breaks at runtime.
var _ cli.HistorySessionView = (*ManagedSession)(nil)

// Compile-time guard: history.Source and cli.HistorySource are
// structurally identical, so any history.Source value also satisfies
// cli.HistorySource. attachHistorySource relies on this when assigning
// the factory result back into history.Source for the merged.Source
// composition.
var (
	_ history.Source    = cli.NoopHistorySource{}
	_ cli.HistorySource = history.Noop{}
)
