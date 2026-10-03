package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestBuildServer_HubAlwaysWired replaces the three Headless tests (#2634).
// ServerOptions.Headless promised a Server "wired without a dashboard Hub on
// purpose"; after #2552 no constructor path could produce one — buildDashboard
// runs unconditionally — so the flag documented a state that did not exist.
// What is worth pinning is the fact that made the flag dead: every constructed
// Server has a Hub with an engine, and wiring holds that same engine; lint
// C1/C2 rule out any other engine a build step could reach.
func TestBuildServer_HubAlwaysWired(t *testing.T) {
	t.Parallel()
	router := session.NewRouter(session.RouterConfig{})
	srv, hs := buildServerWithHandlers(ServerOptions{Addr: ":0", Router: router, Backend: "claude"})
	t.Cleanup(srv.appCancel)

	if srv.hub == nil || srv.hub.engine == nil {
		t.Fatal("buildServer produced a Server without a Hub / send engine — the hub-less mode #2634 removed has come back")
	}
	if hs.wiring.engine != srv.hub.engine {
		t.Fatal("wiring.engine is not the Hub's engine — the build steps would hand out a different pipeline than the dashboard's")
	}
}

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
