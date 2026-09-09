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
	_, hs := newTestServerHS(&mockPlatform{})
	if hs.scratchH == nil {
		t.Fatal("scratch handler not wired by registerDashboard")
	}
	if !hs.scratchH.RouterIsWired() {
		t.Fatal("scratch handler router field is nil — wiring regression")
	}
	// Compile-time interface-satisfaction guard: *session.Router must
	// satisfy ScratchRouter. The blank-assign here forces the compiler
	// to check the assignment at build time; if a method drops, this
	// test file fails to compile rather than failing at runtime in a
	// less-obvious place.
	var _ ScratchRouter = (*session.Router)(nil)
}

// TestSendHandler_ReachesRouterOnlyThroughEngine replaces the former
// TestSendHandler_RouterFieldIsSendRouter. #566 gave SendHandler its own
// SendRouter view; #2551 then made the handler ALSO write through
// engine.router, so one *session.Router was held twice — the shape #2551 was
// filed against. #2632 removed the field: the handler's only path to the
// router is the engine, and production wiring must hand it the Hub's engine so
// the HTTP and WS send paths observe the same router / workspace table.
func TestSendHandler_ReachesRouterOnlyThroughEngine(t *testing.T) {
	t.Parallel()
	s, hs := newTestServerHS(&mockPlatform{})
	if hs.sendH == nil {
		t.Fatal("send handler not wired by registerDashboard")
	}
	if hs.sendH.engine == nil {
		t.Fatal("send handler engine is nil — wiring regression")
	}
	if hs.sendH.engine != s.hub.engine {
		t.Fatal("send handler engine is not the Hub's engine — HTTP and WS sends would see different state")
	}
	// The engine's router view is what the HTTP path now reads through.
	var _ sendEngineRouter = (*session.Router)(nil)
}
