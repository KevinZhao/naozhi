// build_dashboard.go — the dashboard half of the composition root (#2552):
// Hub, upload store, SendHandler, scratch / memory handlers and the
// run-telemetry broadcaster are constructed here, so s.hub is non-nil for the
// Server's whole life. This file CONSTRUCTS; registerDashboard REGISTERS
// routes and STARTS goroutines. Nothing here may start a goroutine or bind a
// listener — a construction failure must not leak a ticker.
package server

import (
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/dashboard/ext/memory"
	"github.com/naozhi/naozhi/internal/dashboard/ext/scratch"
	"github.com/naozhi/naozhi/internal/turn"
)

// buildDashboard constructs the WebSocket hub and the handlers that depend on
// it. Called from buildServer after the Server literal and s.scratchPool exist;
// every dependency it reads is set by then. The mount-only handlers land on hs
// (#2553) rather than on Server.
func (s *Server) buildDashboard(hs *handlerSet) {
	// The upload store comes first so it can be passed into NewHub instead of
	// being pushed in with SetUploadStore afterwards. Its cleanup loop is
	// started (not created) in registerDashboard against appCtx.
	s.uploadStore = newUploadStore()

	s.hub = s.buildWSStack(hs.wiring)

	hs.sendH = &SendHandler{
		nodeAccess:    s.nodes,
		engine:        hs.wiring.engine,
		uploadStore:   s.uploadStore,
		uploadLimiter: newIPLimiterWithProxy(rate.Every(6*time.Second), 10, s.auth.TrustedProxy), // 10 uploads/min per IP
		sendLimiter:   newIPLimiterWithProxy(rate.Every(2*time.Second), 30, s.auth.TrustedProxy), // 30 sends/min per IP (burst 30)
		auth:          s.auth,
		trustedProxy:  s.auth.TrustedProxy,
		orient:        hs.wiring.orient,
	}

	// Scratch (ephemeral aside) API: pool built in buildServer; the sweeper
	// goroutine starts in registerDashboard.
	if s.scratchPool != nil {
		hs.scratchH = scratch.New(scratch.Deps{
			Broadcaster: hs.wiring.bcast,
			Router:      scratchRouter{s.hub.router},
			Pool:        s.scratchPool,
			OpenLimit:   newIPLimiterWithProxy(rate.Every(12*time.Second), 5, s.auth.TrustedProxy),
			Agents:      hs.wiring.agents,
		})
	}

	// Installed-asset browser (dashboard_ccassets.go).
	if hs.ccAssetsH == nil {
		hs.ccAssetsH = s.buildAssetBrowser()
	}

	// memory link preview (docs/rfc/memory-link-rendering.md).
	if hs.memoryH == nil {
		hs.memoryH = memory.New(resolveClaudeProjectsDir(), newIPLimiterWithProxy(memory.MemoryLimiterRate, memory.MemoryLimiterBurst, s.auth.TrustedProxy))
	}

	// Push session list changes to WS clients. The router is built in
	// cmd/naozhi/main.go before the Hub can exist, so it was handed the relay
	// and the Hub binds to it here — at construction time, so no request can
	// be served by a Hub the router does not yet reach.
	if hs.wiring.routerEvents != nil {
		hs.wiring.routerEvents.BindSessionsChanged(hs.wiring.bcast.BroadcastSessionsUpdate)
	}

	// cron and sysession share one relay, built in main.go before the Hub;
	// per-subsystem WS payload selection happens inside hubBroadcaster.
	if hs.wiring.runTelemetry != nil {
		hs.wiring.runTelemetry.Bind(newHubBroadcaster(hs.wiring.bcast))
	}
}

// buildWSStack builds the WebSocket stack in dependency order: the
// broadcaster (owns the subscriber registry), the turn.Orchestrator every
// entry's turns run on (its turnSender notifies the broadcaster), the send
// engine (submits to the Orchestrator) and the Hub (uses the engine and the
// broadcaster). w keeps the broadcaster, the Orchestrator and the engine for
// the other build steps, so nothing reaches them back through the Hub.
func (s *Server) buildWSStack(w *wiring) *Hub {
	w.bcast = newWSBroadcaster(newSubscriberRegistry())
	// A nil *session.Router boxed into the interface would read non-nil.
	var router turnRouter
	if s.router != nil {
		router = s.router
	}
	w.turns = turn.New(w.queue, turnSender{router: router, notify: w.bcast, prompts: w.scheduler})
	w.engine = newSendEngine(sendEngineOpts{
		Turns:       w.turns,
		Ctx:         s.appCtx,
		Router:      s.router,
		Resolver:    w.resolver,
		Agents:      w.agents,
		ProjectMgr:  s.projectMgr,
		ScratchPool: s.scratchPool,
		AllowedRoot: w.allowedRoot,
		Notify:      w.bcast,
	})
	return NewHub(HubOptions{
		Router:    s.router,
		DashToken: s.dashboardToken,
		// Live getter, not a snapshot: RotateCookieGen must invalidate WS
		// upgrades on the next handshake (#1398).
		CookieMACFn:      s.auth.CookieMAC,
		Nodes:            s.nodes,
		Resolver:         w.resolver,
		Scheduler:        w.scheduler,
		AllowedRoot:      w.projectsRoot,
		TrustedProxy:     s.auth.TrustedProxy,
		WSAuthLimiter:    s.auth.LoginAllow,
		WSUpgradeLimiter: s.auth.WSUpgradeAllow,
		// HandleUpgrade mints nz_anon for uploadOwner and refuses the
		// upgrade if minting fails; never falls back to clientIP (#1326).
		Auth:        s.auth,
		UploadStore: s.uploadStore,
		// appCtx is created in buildServer, so the Hub is parented from birth.
		ParentCtx:   s.appCtx,
		Engine:      w.engine,
		Broadcaster: w.bcast,
	})
}
