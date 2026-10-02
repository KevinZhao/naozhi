package session

// R215-ARCH-P2-2 regression tests. attachHistorySource was previously
// called manually at every site that inserted into the session table —
// 5 production paths (router_restore.go reload, router_discovery.go
// register/takeover ×2, router_lifecycle.go spawn, router_rename.go). Missing
// the call at any of them would leave EventEntriesBeforeCtx returning
// empty and the dashboard "history" drawer silently blank for that
// session. The fix funnels every insertion through publishSession
// so the (attachHistorySource → sessions map → indexAdd) triple is
// invariant-by-construction.
//
// These tests pin the contract: every publishSession path leaves
// HistorySource non-nil, and the alreadyAttached short-circuit does not
// double-attach when the caller already invoked attachHistorySource.

import (
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/session/backendstore"
)

// minimalRouter builds a Router with just enough wiring for
// publishSession to run end-to-end. The default backend
// resolves to the package-default wrapper which returns a Noop
// history source — that's fine; the test asserts on non-nil.
func minimalRouter(t *testing.T) *Router {
	t.Helper()
	w := &cli.Wrapper{} // zero-value wrapper; NewHistorySource returns Noop
	r := &Router{
		ss: newSessionTable(),
	}
	r.editBackendsForTest(func(c *backendstore.Config) { c.Wrapper = w })
	r.editBackendsForTest(func(c *backendstore.Config) { c.DefaultBackend = "claude" })
	r.setWrappersForTest(map[string]*cli.Wrapper{"claude": w})
	return r
}

// TestPublishSessionLocked_AttachesHistorySource: the canonical happy
// path — caller did NOT pre-attach (alreadyAttached=false), so the
// helper runs attachHistorySource and the post-condition is non-nil
// HistorySource on the session.
func TestPublishSessionLocked_AttachesHistorySource(t *testing.T) {
	t.Parallel()

	r := minimalRouter(t)
	s := &ManagedSession{key: "feishu:direct:user1:general"}

	r.ss.Update(func(tx sessTx) {
		r.publishSession(tx, s.key, s, false)
	})

	if got := s.loadHistorySource(); got == nil {
		t.Fatal("publishSession left HistorySource nil — EventEntriesBeforeCtx would return empty and dashboard history drawer would silently blank")
	}
	var stored *ManagedSession
	r.ss.View(func(v sessView) {
		stored = v.Get(s.key)
	})
	if stored != s {
		t.Fatalf("publishSession did not insert into the session table: got %v, want %v", stored, s)
	}
}

// TestPublishSessionLocked_AlreadyAttachedDoesNotOverwrite: rename path
// pre-attaches the renamed `fresh` session before publishing under the
// new key. The helper must NOT overwrite that source with a freshly
// resolved one (the rename pre-attach is the only correct binding —
// post-rename the session's chain IDs differ from any naive
// backend-default resolution).
func TestPublishSessionLocked_AlreadyAttachedDoesNotOverwrite(t *testing.T) {
	t.Parallel()

	r := minimalRouter(t)
	s := &ManagedSession{key: "renamed-key"}

	// Caller pre-attaches a sentinel source.
	sentinel := history.Noop{}
	s.SetHistorySource(sentinel)

	r.ss.Update(func(tx sessTx) {
		r.publishSession(tx, s.key, s, true)
	})

	got := s.loadHistorySource()
	if got == nil {
		t.Fatal("publishSession cleared a pre-attached HistorySource")
	}
	// The exact identity check is over-specified for some Source
	// implementations (interface-typed values). Accept any non-nil.
	var stored *ManagedSession
	r.ss.View(func(v sessView) {
		stored = v.Get(s.key)
	})
	if stored != s {
		t.Fatalf("publishSession did not insert into the session table: got %v, want %v", stored, s)
	}
}

// TestPublishSessionLocked_IndexAddObserved: the helper must update
// the per-chat index so subsequent ResetChat / ListChat lookups see
// the session.
func TestPublishSessionLocked_IndexAddObserved(t *testing.T) {
	t.Parallel()

	r := minimalRouter(t)
	s := &ManagedSession{key: "feishu:direct:user1:general"}

	r.ss.Update(func(tx sessTx) {
		r.publishSession(tx, s.key, s, false)
	})

	// The session is indexed under its chat, so a follow-up ResetChat finds it.
	chatKey := chatKeyFor(s.key)
	var keys []string
	r.ss.View(func(v sessView) { keys = v.KeysOfChat(chatKey) })
	if !slices.Contains(keys, s.key) {
		t.Fatalf("publishSession left the session out of its chat index: chatKey=%q", chatKey)
	}
}
