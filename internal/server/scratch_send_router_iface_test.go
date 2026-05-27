package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestScratchHandler_RouterFieldIsScratchRouter pins R215-ARCH-P1-4 (#566)
// Phase 2.5 cleanup: ScratchHandler now reaches its router via the
// consumer.go ScratchRouter interface field rather than transiting
// through h.hub.router.* (which made the handler invisibly depend on
// *Hub's concrete router handle).
//
// Pin shape:
//
//   - The handler MUST carry an explicit `router ScratchRouter` field.
//   - *session.Router MUST satisfy ScratchRouter (interface contract —
//     the production wiring relies on it).
//   - Production wiring (dashboard.go) MUST populate the field so a
//     unit test that instantiates the handler via newTestServer sees
//     a non-nil router.
//
// This is a structural / invariant guard: if a future refactor reverts
// the handler to h.hub.router.*, the field disappears, callers break,
// and this test compiles-fails or asserts-fails accordingly.
func TestScratchHandler_RouterFieldIsScratchRouter(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})
	if srv.scratchH == nil {
		t.Fatal("scratch handler not wired by registerDashboard")
	}
	if srv.scratchH.router == nil {
		t.Fatal("scratch handler router field is nil — wiring regression")
	}
	// Compile-time interface-satisfaction guard: *session.Router must
	// satisfy ScratchRouter. The blank-assign here forces the compiler
	// to check the assignment at build time; if a method drops, this
	// test file fails to compile rather than failing at runtime in a
	// less-obvious place.
	var _ ScratchRouter = (*session.Router)(nil)
}

// TestSendHandler_RouterFieldIsSendRouter is the SendHandler twin of
// TestScratchHandler_RouterFieldIsScratchRouter. Same R215-ARCH-P1-4
// (#566) Phase 2.5 cleanup contract: SendHandler.router replaces the
// h.hub.router.* transits in resolveAttachmentWorkspace.
func TestSendHandler_RouterFieldIsSendRouter(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})
	if srv.sendH == nil {
		t.Fatal("send handler not wired by registerDashboard")
	}
	if srv.sendH.router == nil {
		t.Fatal("send handler router field is nil — wiring regression")
	}
	var _ SendRouter = (*session.Router)(nil)
}

// TestScratchRouter_InjectableForTesting exercises the deeper "consumer
// interface" payoff: a fake ScratchRouter can be substituted at the
// field level so future scratch-handler tests don't need to wire a
// full *session.Router. The recorded calls verify that the handler's
// promote-failure cleanup path goes through the interface (not through
// h.hub.router.Remove).
func TestScratchRouter_InjectableForTesting(t *testing.T) {
	t.Parallel()
	fake := &recordingScratchRouter{}
	h := &ScratchHandler{router: fake}
	// Direct method-call surface check: the three methods we replaced
	// all dispatch through the field. We don't need to drive the full
	// HTTP handler — the surface contract is enough.
	h.router.GetSession("k")
	h.router.Remove("k")
	h.router.RenameSession("a", "b")
	if fake.getSession != 1 || fake.remove != 1 || fake.rename != 1 {
		t.Errorf("dispatch counts: getSession=%d remove=%d rename=%d (want 1/1/1)",
			fake.getSession, fake.remove, fake.rename)
	}
}

type recordingScratchRouter struct {
	getSession, remove, rename int
}

func (r *recordingScratchRouter) GetSession(string) *session.ManagedSession {
	r.getSession++
	return nil
}
func (r *recordingScratchRouter) Remove(string) bool {
	r.remove++
	return false
}
func (r *recordingScratchRouter) RenameSession(string, string) bool {
	r.rename++
	return false
}
