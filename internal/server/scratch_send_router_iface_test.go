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
	if !srv.scratchH.RouterIsWired() {
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

