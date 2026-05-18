package kirojsonl

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// stubKiroSession is a cli.HistorySessionView for the factory tests.
// Mirrors claudejsonl/factory_test.go's stubSession but only the
// SessionID() method matters for kiro — kirojsonl never reads
// SnapshotChainIDs / Workspace.
type stubKiroSession struct {
	key, ws, sid string
	chain        []string
}

func (s *stubKiroSession) SessionKey() string         { return s.key }
func (s *stubKiroSession) Workspace() string          { return s.ws }
func (s *stubKiroSession) SessionID() string          { return s.sid }
func (s *stubKiroSession) SnapshotChainIDs() []string { return s.chain }

// TestFactory_KiroReturnsKirojsonlSource pins the happy path — when
// KiroSessionsDir is set, the factory must return a *Source wired with
// the supplied rootDir and session-ID accessor. A future refactor that
// accidentally swapped accessors (e.g. closing over Workspace instead
// of SessionID) would break the dashboard's "load earlier" path on
// kiro sessions, so anchor the wiring here.
func TestFactory_KiroReturnsKirojsonlSource(t *testing.T) {
	t.Parallel()
	sess := &stubKiroSession{
		key: "feishu:direct:alice:general",
		ws:  "/tmp/ws",
		sid: "kiro-sess-1",
	}
	got := factory(sess, cli.HistoryWiring{KiroSessionsDir: "/kiro/dir"})
	src, ok := got.(*Source)
	if !ok {
		t.Fatalf("factory(kiro, dir set) = %T; want *Source", got)
	}
	if src.rootDir != "/kiro/dir" {
		t.Errorf("Source.rootDir = %q; want /kiro/dir", src.rootDir)
	}
	if src.sessionID == nil {
		t.Fatal("Source.sessionID callback is nil")
	}
	// Invoking the callback should hit our stub.
	if got := src.sessionID(); got != sess.sid {
		t.Errorf("sessionID callback returned %q; want %q", got, sess.sid)
	}
}

// TestInit_RegistersKiroBackend confirms the package-level init()
// registered "kiro" with cli.RegisterHistoryFactory. Without this
// registration, NewWrapper(... "kiro" ...) would never wire a
// history.Source and the dashboard would silently lose kiro JSONL
// fallback after upgrade. Same role as
// claudejsonl/factory_test.go::TestInit_RegistersClaudeBackend; pinned
// here so the kiro side cannot regress independently.
func TestInit_RegistersKiroBackend(t *testing.T) {
	t.Parallel()
	w := cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "kiro")
	src := w.NewHistorySource(
		&stubKiroSession{sid: "x"},
		cli.HistoryWiring{KiroSessionsDir: "/kiro/dir"},
	)
	if _, ok := src.(*Source); !ok {
		t.Errorf("wrapper(kiro).NewHistorySource = %T; want *Source — init() registration regressed", src)
	}
}
