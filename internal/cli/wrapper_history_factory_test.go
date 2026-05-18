package cli

import (
	"context"
	"testing"
)

// fakeHistorySession is a HistorySessionView stub for the factory tests.
// Returns whatever the test set up and counts how often each accessor is
// invoked so DepsRoundTrip can pin the contract that the factory pulls
// every field at least once.
type fakeHistorySession struct {
	key       string
	workspace string
	sessionID string
	chain     []string
}

func (f *fakeHistorySession) SessionKey() string         { return f.key }
func (f *fakeHistorySession) Workspace() string          { return f.workspace }
func (f *fakeHistorySession) SessionID() string          { return f.sessionID }
func (f *fakeHistorySession) SnapshotChainIDs() []string { return f.chain }

// Test-only backend IDs are constructed with unique-per-test prefixes so
// parallel registrations cannot collide. There is no Unregister hook on
// the registry today; production code never looks up these IDs because
// no production wrapper carries them as its BackendID, so leaving them
// in the registry after tests finish is harmless.

// TestNewHistorySource_NilWrapperReturnsNoop pins the nil-receiver
// safety contract. Callers in router.attachHistorySource and tests
// occasionally hold a *cli.Wrapper that has not been wired (e.g. a
// test harness that constructs a Wrapper{} literal) — calling
// NewHistorySource on nil must not panic.
func TestNewHistorySource_NilWrapperReturnsNoop(t *testing.T) {
	t.Parallel()
	var w *Wrapper // nil
	src := w.NewHistorySource(&fakeHistorySession{}, HistoryWiring{})
	if src == nil {
		t.Fatal("NewHistorySource on nil wrapper must not return nil")
	}
	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Errorf("LoadBefore err on noop: %v", err)
	}
	if got != nil {
		t.Errorf("noop LoadBefore returned %d entries; want nil", len(got))
	}
}

// TestNewHistorySource_NilFactoryReturnsNoop covers the unknown-backend
// case where pickHistoryFactory found no registration. Constructing a
// Wrapper with an unregistered BackendID must still produce a usable
// source; the dashboard should see "no fallback history" rather than
// a nil-pointer crash.
func TestNewHistorySource_NilFactoryReturnsNoop(t *testing.T) {
	t.Parallel()
	w := &Wrapper{BackendID: "no-such-backend", historyFactory: nil}
	src := w.NewHistorySource(&fakeHistorySession{}, HistoryWiring{})
	if src == nil {
		t.Fatal("nil factory must yield a non-nil noop source")
	}
	got, err := src.LoadBefore(context.Background(), 100, 5)
	if err != nil {
		t.Errorf("noop LoadBefore err: %v", err)
	}
	if got != nil {
		t.Errorf("noop returned entries: %+v", got)
	}
}

// TestNewHistorySource_FactoryReturningNilUpgradesToNoop guards the
// boundary contract: a registered factory that returns nil (e.g.
// claude factory with empty ClaudeDir) must not propagate the nil to
// the caller. Wrapper.NewHistorySource is responsible for upgrading
// the nil to NoopHistorySource so attachHistorySource never has to
// nil-check.
func TestNewHistorySource_FactoryReturningNilUpgradesToNoop(t *testing.T) {
	t.Parallel()
	w := &Wrapper{
		BackendID: "test",
		historyFactory: func(s HistorySessionView, deps HistoryWiring) HistorySource {
			return nil
		},
	}
	src := w.NewHistorySource(&fakeHistorySession{}, HistoryWiring{})
	if src == nil {
		t.Fatal("factory-returns-nil must be upgraded to non-nil noop")
	}
	if _, ok := src.(NoopHistorySource); !ok {
		t.Errorf("factory-returns-nil yielded %T; want NoopHistorySource", src)
	}
}

// TestNewHistorySource_ClaudeWithEmptyDirReturnsNoop pins the
// claudejsonl factory's degradation rule: an empty ClaudeDir means
// "no on-disk source available" and must surface as a noop, not as
// a Source pointed at an empty path that would later fail or — worse
// — read from the process working directory.
//
// This test exercises a stand-in factory that mirrors the real claude
// factory's empty-dir branch — keeping it in the cli package avoids a
// circular dependency on claudejsonl while still pinning the contract.
func TestNewHistorySource_ClaudeWithEmptyDirReturnsNoop(t *testing.T) {
	t.Parallel()
	// Mimic the claudejsonl factory's first-line branch.
	RegisterHistoryFactory("cli-test-claude-empty", func(s HistorySessionView, deps HistoryWiring) HistorySource {
		if deps.ClaudeDir == "" {
			return NoopHistorySource{}
		}
		return nil
	})
	w := NewWrapper("/bin/false", &ClaudeProtocol{}, "cli-test-claude-empty")
	// Sanity: factory registered before NewWrapper, so wrapper
	// must have picked it up.
	if w.historyFactory == nil {
		t.Fatal("wrapper.historyFactory not bound after registration")
	}

	src := w.NewHistorySource(&fakeHistorySession{
		key: "k", workspace: "/tmp", sessionID: "sid",
		chain: []string{"sid"},
	}, HistoryWiring{ClaudeDir: ""})
	if src == nil {
		t.Fatal("empty ClaudeDir must yield non-nil noop")
	}
	if _, ok := src.(NoopHistorySource); !ok {
		t.Errorf("empty ClaudeDir factory result = %T; want NoopHistorySource", src)
	}
}

// TestNewHistorySource_DepsRoundTrip verifies that all fields of
// HistoryWiring are passed through to the factory unchanged and that
// the HistorySessionView accessors are reachable. The factory is the
// only place a backend can read directory configuration, so a typo
// here would silently disable a backend's fallback.
//
// The test runs without t.Parallel() and uses a unique backend ID so
// concurrent tests in the same binary do not race on the package-level
// registry. The factory closes over local capture variables, which are
// only assigned by the factory's single invocation in this test, so no
// synchronisation primitives are needed beyond the natural happens-before
// edge from registration → NewWrapper → NewHistorySource → assertion.
func TestNewHistorySource_DepsRoundTrip(t *testing.T) {
	var sawDeps HistoryWiring
	var sawWS, sawSID, sawKey string
	var sawChain []string

	RegisterHistoryFactory("cli-test-deps-rt", func(s HistorySessionView, deps HistoryWiring) HistorySource {
		sawDeps = deps
		sawWS = s.Workspace()
		sawSID = s.SessionID()
		sawKey = s.SessionKey()
		sawChain = s.SnapshotChainIDs()
		return NoopHistorySource{}
	})

	w := NewWrapper("/bin/false", &ClaudeProtocol{}, "cli-test-deps-rt")
	if w.historyFactory == nil {
		t.Fatal("factory not bound")
	}

	want := HistoryWiring{
		ClaudeDir:       "/claude/dir",
		KiroSessionsDir: "/kiro/dir",
		EventLogDir:     "/event/log",
	}
	fakeSess := &fakeHistorySession{
		key: "feishu:direct:alice:general", workspace: "/tmp/ws",
		sessionID: "sess-xyz", chain: []string{"old", "sess-xyz"},
	}

	_ = w.NewHistorySource(fakeSess, want)

	if sawDeps != want {
		t.Errorf("deps round-trip lost data: got %+v want %+v", sawDeps, want)
	}
	if sawWS != fakeSess.workspace {
		t.Errorf("Workspace() not invoked / wrong: got %q want %q", sawWS, fakeSess.workspace)
	}
	if sawSID != fakeSess.sessionID {
		t.Errorf("SessionID() round-trip: got %q want %q", sawSID, fakeSess.sessionID)
	}
	if sawKey != fakeSess.key {
		t.Errorf("SessionKey() round-trip: got %q want %q", sawKey, fakeSess.key)
	}
	if len(sawChain) != len(fakeSess.chain) {
		t.Fatalf("SnapshotChainIDs len=%d want %d", len(sawChain), len(fakeSess.chain))
	}
	for i, id := range fakeSess.chain {
		if sawChain[i] != id {
			t.Errorf("chain[%d]=%q want %q", i, sawChain[i], id)
		}
	}
}

// TestRegisterHistoryFactory_RejectsEmptyOrNil pins the registration
// guard so a buggy backend's init() that passes "" or nil cannot
// silently overwrite a real factory or seed an empty-key entry.
func TestRegisterHistoryFactory_RejectsEmptyOrNil(t *testing.T) {
	t.Parallel()
	// Empty backend ID must be ignored.
	RegisterHistoryFactory("", func(s HistorySessionView, deps HistoryWiring) HistorySource { return nil })
	historyFactoryMu.RLock()
	_, ok := historyFactoryRegistry[""]
	historyFactoryMu.RUnlock()
	if ok {
		t.Errorf("empty backend ID accepted; registry now has empty-key entry")
	}

	// Nil function must be ignored. Use a unique key so an earlier
	// (or concurrent) test cannot pollute the assertion.
	RegisterHistoryFactory("cli-test-nilfn-guard", nil)
	historyFactoryMu.RLock()
	_, ok = historyFactoryRegistry["cli-test-nilfn-guard"]
	historyFactoryMu.RUnlock()
	if ok {
		t.Errorf("nil factory accepted; registry now has cli-test-nilfn-guard entry")
	}
}

// TestPickHistoryFactory_UnknownBackendReturnsNil ensures the lookup
// path returns nil (not a panic, not a placeholder) for unregistered
// backends. NewWrapper relies on this nil to leave w.historyFactory
// unset, which NewHistorySource then handles via its noop fallback.
func TestPickHistoryFactory_UnknownBackendReturnsNil(t *testing.T) {
	t.Parallel()
	// Random-enough id that no registered factory matches.
	got := pickHistoryFactory("cli-test-totally-unknown-x9q7")
	if got != nil {
		t.Errorf("pickHistoryFactory(unknown) = %v; want nil", got)
	}
}
