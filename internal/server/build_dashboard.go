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

	s.hub = NewHub(HubOptions{
		Router:    s.router,
		Agents:    hs.wiring.agents,
		AgentCmds: hs.wiring.agentCommands,
		DashToken: s.dashboardToken,
		// Live getter, not a snapshot: RotateCookieGen must invalidate WS
		// upgrades on the next handshake (#1398).
		CookieMACFn:      s.auth.CookieMAC,
		Guard:            hs.wiring.sessionGuard,
		Queue:            hs.wiring.msgQueue,
		Nodes:            s.nodes,
		ProjectMgr:       s.projectMgr,
		Resolver:         hs.wiring.resolver,
		Scheduler:        hs.wiring.scheduler,
		ScratchPool:      s.scratchPool,
		AllowedRoot:      hs.wiring.allowedRoot,
		TrustedProxy:     s.auth.TrustedProxy,
		WSAuthLimiter:    s.auth.LoginAllow,
		WSUpgradeLimiter: s.auth.WSUpgradeAllow,
		// HandleUpgrade mints nz_anon for uploadOwner and refuses the
		// upgrade if minting fails; never falls back to clientIP (#1326).
		Auth:        s.auth,
		UploadStore: s.uploadStore,
		// appCtx is created in buildServer, so the Hub is parented from birth;
		// there is no longer a window where it runs under a Background fallback
		// until Start replaces it.
		ParentCtx: s.appCtx,
	})

	hs.sendH = &SendHandler{
		nodeAccess:    s.nodes,
		engine:        s.hub.engine,
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
			Broadcaster: s.hub,
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
		hs.wiring.routerEvents.BindSessionsChanged(s.hub.BroadcastSessionsUpdate)
	}

	// cron and sysession share one runtelemetry.Broadcaster; per-subsystem
	// WS payload selection happens inside hubBroadcaster. Same note as
	// the session list: both objects come from main.go.
	telemetry := newHubBroadcaster(s.hub)
	if hs.wiring.scheduler != nil {
		hs.wiring.scheduler.SetTelemetry(telemetry)
	}
	if hs.wiring.sysessionMgr != nil {
		hs.wiring.sysessionMgr.SetTelemetry(telemetry)
	}
}
