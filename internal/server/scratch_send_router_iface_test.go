package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

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
