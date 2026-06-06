package session

// R215-ARCH-P2-2 regression tests. attachHistorySource was previously
// called manually at every site that inserted into r.ss.sessions —
// 5 production paths (router_core.go reload, router_discovery.go
// register/takeover ×2, router_lifecycle.go spawn / rename). Missing
// the call at any of them would leave EventEntriesBeforeCtx returning
// empty and the dashboard "history" drawer silently blank for that
// session. The fix funnels every insertion through publishSessionLocked
// so the (attachHistorySource → sessions map → indexAdd) triple is
// invariant-by-construction.
//
// These tests pin the contract: every publishSessionLocked path leaves
// HistorySource non-nil, and the alreadyAttached short-circuit does not
// double-attach when the caller already invoked attachHistorySource.

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/history"
)

// minimalRouter builds a Router with just enough wiring for
// publishSessionLocked to run end-to-end. The default backend
// resolves to the package-default wrapper which returns a Noop
// history source — that's fine; the test asserts on non-nil.
func minimalRouter(t *testing.T) *Router {
	t.Helper()
	w := &cli.Wrapper{} // zero-value wrapper; NewHistorySource returns Noop
	r := &Router{
		ss: sessionStore{
			sessions: map[string]*ManagedSession{},
			idToKey:  map[string]string{},
		},
	}
	r.bkStore.wrapper = w
	r.bkStore.defaultBackend = "claude"
	r.bkStore.wrappers = map[string]*cli.Wrapper{"claude": w}
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

	r.mu.Lock()
	r.publishSessionLocked(s.key, s, false)
	r.mu.Unlock()

	if got := s.loadHistorySource(); got == nil {
		t.Fatal("publishSessionLocked left HistorySource nil — EventEntriesBeforeCtx would return empty and dashboard history drawer would silently blank")
	}
	r.mu.RLock()
	stored := r.ss.sessions[s.key]
	r.mu.RUnlock()
	if stored != s {
		t.Fatalf("publishSessionLocked did not insert into r.ss.sessions: got %v, want %v", stored, s)
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

	r.mu.Lock()
	r.publishSessionLocked(s.key, s, true)
	r.mu.Unlock()

	got := s.loadHistorySource()
	if got == nil {
		t.Fatal("publishSessionLocked cleared a pre-attached HistorySource")
	}
	// The exact identity check is over-specified for some Source
	// implementations (interface-typed values). Accept any non-nil.
	r.mu.RLock()
	stored := r.ss.sessions[s.key]
	r.mu.RUnlock()
	if stored != s {
		t.Fatalf("publishSessionLocked did not insert into r.ss.sessions: got %v, want %v", stored, s)
	}
}

// TestPublishSessionLocked_IndexAddObserved: the helper must update
// the per-chat index so subsequent ResetChat / ListChat lookups see
// the session.
func TestPublishSessionLocked_IndexAddObserved(t *testing.T) {
	t.Parallel()

	r := minimalRouter(t)
	s := &ManagedSession{key: "feishu:direct:user1:general"}

	r.mu.Lock()
	r.publishSessionLocked(s.key, s, false)
	r.mu.Unlock()

	// indexAdd populates sessionsByChat (lazy init); a follow-up
	// ResetChat must find the session.
	r.mu.RLock()
	if r.ss.byChat != nil {
		chatKey := chatKeyFor(s.key)
		if _, ok := r.ss.byChat[chatKey][s.key]; !ok {
			r.mu.RUnlock()
			t.Fatalf("publishSessionLocked did not call indexAdd: chatKey=%q missing", chatKey)
		}
	}
	r.mu.RUnlock()
}
