package claudejsonl

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// stubSession is a cli.HistorySessionView for the factory tests.
type stubSession struct {
	key, ws, sid string
	chain        []string
}

func (s *stubSession) SessionKey() string         { return s.key }
func (s *stubSession) Workspace() string          { return s.ws }
func (s *stubSession) SessionID() string          { return s.sid }
func (s *stubSession) SnapshotChainIDs() []string { return s.chain }

// TestFactory_ClaudeWithEmptyDirReturnsNoop pins the factory's
// degradation rule: an empty ClaudeDir means "no on-disk source
// available", so the factory must yield cli.NoopHistorySource — never
// a *Source wrapping an empty path.
func TestFactory_ClaudeWithEmptyDirReturnsNoop(t *testing.T) {
	t.Parallel()
	got := factory(&stubSession{}, cli.HistoryWiring{ClaudeDir: ""})
	if _, ok := got.(cli.NoopHistorySource); !ok {
		t.Errorf("empty ClaudeDir factory returned %T; want cli.NoopHistorySource", got)
	}
}

// TestFactory_ClaudeReturnsClaudejsonlSource pins the happy path —
// when ClaudeDir is set, the factory must return a *Source wired with
// the supplied workspace and chain accessor. A future refactor that
// accidentally hard-codes a different chain accessor (e.g. closing
// over the wrong session view) would break the dashboard's
// pagination chain reads, so anchor the type and chain wiring here.
func TestFactory_ClaudeReturnsClaudejsonlSource(t *testing.T) {
	t.Parallel()
	sess := &stubSession{
		key:   "feishu:direct:alice:general",
		ws:    "/tmp/ws",
		sid:   "sess-1",
		chain: []string{"sess-old", "sess-1"},
	}
	got := factory(sess, cli.HistoryWiring{ClaudeDir: "/claude/dir"})
	src, ok := got.(*Source)
	if !ok {
		t.Fatalf("factory(claude, claudeDir set) = %T; want *Source", got)
	}
	if src.claudeDir != "/claude/dir" {
		t.Errorf("Source.claudeDir = %q; want /claude/dir", src.claudeDir)
	}
	if src.cwd != sess.ws {
		t.Errorf("Source.cwd = %q; want %q", src.cwd, sess.ws)
	}
	// Invoking the chain function should hit our stub.
	if src.chainIDs == nil {
		t.Fatal("Source.chainIDs callback is nil")
	}
	chain := src.chainIDs()
	if len(chain) != len(sess.chain) || chain[0] != sess.chain[0] || chain[1] != sess.chain[1] {
		t.Errorf("chain callback returned %v; want %v", chain, sess.chain)
	}
}

// TestInit_RegistersClaudeBackend confirms the package-level init()
// registered "claude" with cli.RegisterHistoryFactory. Without this
// registration, NewWrapper(... "claude" ...) would never wire a
// history.Source and the dashboard would silently lose Claude JSONL
// fallback after upgrade.
func TestInit_RegistersClaudeBackend(t *testing.T) {
	t.Parallel()
	w := cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "claude")
	src := w.NewHistorySource(&stubSession{ws: "/tmp"}, cli.HistoryWiring{ClaudeDir: "/claude/dir"})
	if _, ok := src.(*Source); !ok {
		t.Errorf("wrapper(claude).NewHistorySource = %T; want *Source — init() registration regressed", src)
	}
}
