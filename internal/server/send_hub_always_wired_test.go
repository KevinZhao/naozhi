package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestBuildDispatcher_RequiresTurns pins buildDispatcher's construction-time
// refusal: without it a wiring that skipped buildWSStack would build a
// dispatcher whose first IM message nil-derefs at Submit.
func TestBuildDispatcher_RequiresTurns(t *testing.T) {
	t.Parallel()
	router := session.NewRouter(session.RouterConfig{})
	srv, hs := buildServerWithHandlers(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})
	t.Cleanup(srv.appCancel)

	// The Server is already built; dropping the Orchestrator from its wiring
	// now only affects the second buildDispatcher call below.
	hs.wiring.turns = nil
	defer func() {
		if recover() == nil {
			t.Fatal("buildDispatcher accepted a wiring with no Orchestrator")
		}
	}()
	srv.buildDispatcher(hs.wiring)
}
