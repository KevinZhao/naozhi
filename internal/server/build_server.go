package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	dashsession "github.com/naozhi/naozhi/internal/dashboard/session"
	"github.com/naozhi/naozhi/internal/discovery"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/dashboard/ext/accessprofile"
	"github.com/naozhi/naozhi/internal/dashboard/ext/agentevents"
	"github.com/naozhi/naozhi/internal/dashboard/ext/cli"
	"github.com/naozhi/naozhi/internal/dashboard/ext/planner"
	"github.com/naozhi/naozhi/internal/dashboard/ext/uisettings"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
	"github.com/naozhi/naozhi/internal/uiprefs"
)

// NewWithOptions constructs a Server from a ServerOptions value (the only
// public constructor). Required: opts.Router non-nil; opts.Addr set for the
// listener to bind. Other fields tolerate zero values.
func NewWithOptions(opts ServerOptions) *Server {
	return buildServer(opts)
}

// buildServer is the construction path behind NewWithOptions. Do not add a
// positional `func New(addr string, ...)` constructor (#614; pinned by
// TestServerNew_NotReintroduced).
func buildServer(opts ServerOptions) *Server {
	s, _ := buildServerWithHandlers(opts)
	return s
}

// buildServerWithHandlers is buildServer plus the handlerSet it built. #2553
// made the set a construction local — production discards it after route
// registration — but in-package tests that drive a handler method directly
// (sendH.handleAttachment, scratchH.HandleOpen, …) need the instance the Server
// was actually wired with, and rebuilding those dependencies per test would
// test a different object. This is the seam for that, and the only reason it
// returns two values.
func buildServerWithHandlers(opts ServerOptions) (*Server, *handlerSet) {
	addr := opts.Addr
	router := opts.Router
	platforms := opts.Platforms
	agents := opts.Agents
	agentCommands := opts.AgentCommands
	// A nil *cron.Scheduler must become a nil interface, not a non-nil
	// interface wrapping nil, or every `scheduler != nil` guard would fire.
	var scheduler cronScheduler
	if opts.Scheduler != nil {
		scheduler = opts.Scheduler
	}
	defaultBackend := opts.Backend
	// Fallback footer tag for sessions whose Backend() is empty; per-session
	// ReplyFooterFn reads session.Backend() at reply time.
	defaultTag := replyTagForBackend(defaultBackend)
	tag := defaultTag
	// "" when UserHomeDir fails; downstream sites nil-check claudeDir.
	claudeDir := resolveClaudeDir()

	// Single owner of the live node table; nil opts.Remote.Nodes ⇒ empty table.
	nodes := newNodeRegistry(opts.Remote.Nodes)

	warnUnsetAllowedRoot(opts)

	cookieSecret := loadOrCreateCookieSecret(opts.StateDir)
	// cookieGen is mixed into the auth-cookie HMAC so every restart yields a
	// fresh MAC even on a shared stateDir. CSPRNG-seeded: a time-based seed is
	// reconstructible from /health uptime, letting an attacker holding
	// token+secret forge a cookie valid across restarts (#595, #437).
	cookieGen := auth.RandomCookieGen()

	// One KeyResolver shared by dispatcher, hub and ProjectHandlers;
	// NewDataSource returns untyped nil when projectMgr is nil.
	resolver := session.NewKeyResolver(agents, project.NewDataSource(opts.ProjectManager))
	if sched := opts.Scheduler; sched != nil {
		// A cron run spawns on the profile of the agent its prompt routes to
		// (the scheduler was built from these same maps); the remote gate must see it.
		resolver = resolver.WithCronAccessProfile(func(jobID string) string {
			j, ok := sched.GetJob(jobID)
			if !ok {
				return ""
			}
			agentID, _ := session.ResolveAgent(j.Prompt, agentCommands)
			return agents[agentID].AccessProfile
		})
	}

	// Dependencies only the build steps below read: they reach the dispatcher,
	// the Hub and the handlers through hs.wiring and are not kept on Server.
	w := &wiring{
		dedup: platform.NewDedup(defaultDedupCapacity),
		queue: turn.QueueOptions{
			MaxDepth:     opts.Queue.MaxDepth,
			CollectDelay: opts.Queue.CollectDelay,
			Mode:         turn.ParseMode(opts.Queue.Mode),
		},
		startedAt:     time.Now(),
		agents:        agents,
		agentCommands: agentCommands,
		allowedRoot:   opts.AllowedRoot,
		imAccess:      opts.IMAccess,
		imLimits:      opts.IMLimits,
		debugMode:     opts.Features.Debug,
		resolver:      resolver,
		sysessionMgr:  opts.Sysession.Manager,
		orient:        buildOrientConfig(opts),
		scheduler:     scheduler,
		routerEvents:  opts.Relays.Router,
		runTelemetry:  opts.Relays.RunTelemetry,
	}

	s := &Server{
		addr:             addr,
		mux:              http.NewServeMux(),
		shutdownComplete: make(chan struct{}),
		platforms:        platforms,
		router:           router,
		logger:           opts.Logger,
		claudeDir:        claudeDir,
		noOutputTimeout:  opts.Watchdog.NoOutput,
		totalTimeout:     opts.Watchdog.Total,
		dashboardToken:   opts.DashboardToken,
		onReady:          opts.Lifecycle.OnReady,
		projectMgr:       opts.ProjectManager,
		nodes:            nodes,

		// auth stays on Server: debug_expvar / debug_pprof / ccassets wrap
		// through it and RotateDashboardSessions reaches for it at runtime.
		// Handler-group literals live in build_handlers.go (limiter rationale there).
		auth: buildAuthHandlers(opts, cookieSecret, cookieGen),
	}

	// hs carries the mount-only handlers from here to route registration and
	// then goes out of scope (#2553, handler_set.go). Only the four lifecycle
	// participants get copied onto Server.
	hs := &handlerSet{
		wiring:   w,
		systemH:  buildSystemHandlers(opts, router),
		plannerH: planner.New(planner.Deps{Router: router}),
		// Empty StateDir yields an in-memory prefs store (no persistence).
		uiSettingsH: uisettings.New(uiprefs.New(opts.StateDir)),
		// Empty ConfigPath keeps the create endpoint disabled (400).
		accessProfilesH: accessprofile.New(router.Backends(), opts.Config.Path, opts.Config.AccessProfileSecretsDir),
		cronH:           buildCronHandlers(opts, claudeDir),
		transcribeH:     buildTranscribeHandler(opts),
	}

	// The process-lifetime context is created HERE, not in Start (#2552).
	// Everything downstream (Hub, upload-store cleaner, background loops) hangs
	// off it, so construction no longer has to wait for a Start-time context;
	// Start links its own ctx into appCancel rather than minting a second one.
	s.appCtx, s.appCancel = context.WithCancel(context.Background())

	// Scratch pool ahead of the Hub: the send engine needs it and it
	// only depends on the router. The "scratch:" prefix keeps entries off the
	// sidebar and out of sessions.json; the sweeper goroutine starts in
	// registerDashboard so an early failure does not leak the ticker.
	s.scratchPool = session.NewScratchPool(router, session.DefaultScratchMax, session.DefaultScratchTTL)

	// Hub + the handlers that depend on it (build_dashboard.go). After this
	// line s.hub is non-nil for the Server's whole life, which is what lets the
	// call sites below drop their `if s.hub != nil` guards.
	s.buildDashboard(hs)

	// Retired-store load is best-effort (parse error ⇒ empty store); it is
	// persisted only when StateDir is configured.
	retiredStore, retiredErr := buildRetiredStoreWithErr(opts.StateDir)
	if retiredErr != nil {
		slog.Warn("retired store load failed (degrades to last_active sort)", "err", retiredErr)
	}

	s.nodeCache = node.NewCacheManager(
		func() map[string]node.Conn {
			return s.nodes.NodesSnapshot()
		},
		w.bcast.BroadcastSessionsUpdate,
	)

	s.discoveryCache = newDiscoveryCache(claudeDir, s.router.ManagedExcludeSets, opts.ProjectManager)

	hs.discoveryH = buildDiscoveryHandlers(opts, claudeDir, s.discoveryCache, s.nodes, s.nodeCache, w.bcast.BroadcastSessionsUpdate, s.appCtx)
	hs.projectH = buildProjectHandlers(opts, resolver, s.nodes, s.nodeCache, s.hub.ctx)
	agentIDs := agentIDList(agents)
	hs.costH = buildCostHandlers(opts, router)
	hs.sessionH = buildSessionHandlers(opts, s, w, retiredStore, agentIDs, tag)
	hs.agentEventsH = agentevents.New(agentevents.Deps{
		Router:     router,
		NodeAccess: s.nodes,
	})

	// StartupCtx lets SIGTERM during startup abort the --version probe.
	startupCtx := opts.Lifecycle.StartupCtx
	if startupCtx == nil {
		startupCtx = context.Background()
	}
	// /api/cli/backends?node=<id> proxies the manifest to a remote node.
	hs.cliH = cli.NewCLIBackendsHandlerCtx(startupCtx, router, s.nodes)

	// Dispatcher is built HERE, not in Start (#2633); see build_dispatch.go.
	// Building it before healthH lets the metrics closure be a constructor
	// argument rather than a field back-filled from Start.
	s.dispatcher = s.buildDispatcher(w)

	hs.healthH = buildHealthHandler(opts, s, w)

	s.attachReverseNodeServer(opts.Remote.ReverseServer)

	hs.checkLimiters(w.scheduler != nil)

	// Server keeps the handlers that outlive registration (see
	// handler_set.go for why each one does).
	s.sessionH = hs.sessionH
	s.discoveryH = hs.discoveryH

	// Routes are registered HERE, not in Start (#2553): hs can only be a local
	// if nothing after construction needs it. Start keeps the goroutine starts
	// (startDashboardLoops) and the routes that need the dispatcher.
	s.registerDashboard(hs)

	return s, hs
}

// warnUnsetAllowedRoot reports an unset allowed_root, the one
// directory-traversal guard for dashboard /cd, cron WorkDir and takeover CWD.
// Empty is the legitimate single-user default, so boot is not failed (`naozhi
// doctor` hard-fails instead); token-protected + network-reachable escalates to
// Error so alerting pipelines that ignore Warn still see it (#658).
func warnUnsetAllowedRoot(opts ServerOptions) {
	if opts.AllowedRoot == "" {
		slog.Warn("server.allowed_root is unset; dashboard /cd, cron WorkDir, and takeover CWD accept any absolute path — set allowed_root in config.yaml to restrict")
		if opts.DashboardToken != "" && isPlaintextPublicAddr(opts.Addr) {
			slog.Error("allowed_root unset on a token-protected, network-reachable dashboard — any authenticated user can set cron WorkDir to /etc or other system paths and let the CLI write there. Set server.allowed_root before exposing this listener; `naozhi doctor` will hard-fail this configuration.",
				"addr", opts.Addr,
			)
		}
	}
}

// buildSessionHandlers builds the /api/sessions handlers and registers the
// router hooks that keep them current. The hooks go on once, after the
// handlers exist, so the fan-out is never half-wired while WarmHistoryCache
// runs.
func buildSessionHandlers(opts ServerOptions, s *Server, w *wiring, retiredStore *discovery.RetiredStore, agentIDs []string, tag string) *dashsession.Handlers {
	router := s.router
	// Typed-nil unwrap before the interface boxing (#2561). dashsession's Deps
	// fields became consumer-side interfaces, and dashsession nil-guards three
	// of them (projectMgr ×3, retiredStore ×5, router ×1). Assigning a nil
	// *project.Manager straight into an interface field makes `!= nil` read TRUE
	// and the next call dereferences nil — which is exactly what happened when
	// this conversion was first attempted, and it is the same class as the
	// MessageEnqueuer hazard in #377. The concrete type is only visible here, so
	// the unwrap has to live at the wiring site.
	var projectSrc dashsession.ProjectSource
	if opts.ProjectManager != nil {
		projectSrc = opts.ProjectManager
	}
	var retiredReader dashsession.RetiredReader
	if retiredStore != nil {
		retiredReader = retiredStore
	}
	var routerView dashsession.RouterView
	if router != nil {
		routerView = sessionRouterView{router}
	}
	sessionH := dashsession.New(dashsession.Deps{
		// /api/sessions snapshot enrichment goes through the hub's tailer registry.
		SnapshotEnricher: s.hub.enrichSnapshot,
		Router:           routerView,
		ProjectMgr:       projectSrc,
		Scheduler:        w.scheduler,
		CronSessions:     w.scheduler,
		SysWorkDir:       opts.Sysession.WorkDir,
		ClaudeDir:        s.claudeDir,
		AllowedRoot:      opts.AllowedRoot,
		Agents:           w.agents,
		AgentIDs:         agentIDs,
		NodeAccess:       s.nodes,
		NodeCache:        s.nodeCache,
		StartedAt:        w.startedAt,
		BackendTag:       tag,
		WorkspaceID:      opts.Identity.WorkspaceID,
		WorkspaceName:    opts.Identity.WorkspaceName,
		VersionTag:       opts.Identity.Version,
		WatchdogNoOut:    w.watchdog.noOutPtr(),
		WatchdogTotal:    w.watchdog.totalPtr(),
		RetiredStore:     retiredReader,
		ValidateWS:       validateWorkspace,
		SystemInfoFn:     systemInfo,

		ProjectStableKeyEnabled: opts.Features.ProjectStableKey,
	})
	sessionH.InitStaticStats()
	sessionH.WarmHistoryCache()
	// Router.Reset/Remove hook, fired on the caller's goroutine as the key
	// leaves the table (LRU eviction does not), registered once sessionH exists
	// so the fan-out is never half-wired during WarmHistoryCache: turns.Retire
	// frees the FIFO and tells its senders off-goroutine; RecordRetired stamps
	// the session and makes it visible to the history popover within one poll.
	if opts.Relays.Router != nil {
		retire, appCtx := w.turns.Retire, s.appCtx
		opts.Relays.Router.BindKeyRetired(func(key, sessionID string) {
			retire(appCtx, key)
			sessionH.RecordRetired(sessionID)
		})
	}
	return sessionH
}

// buildHealthHandler builds the server-owned probes (/health, /livez,
// /readyz). The dispatcher exists by now, so its metrics closure is a
// constructor argument (#2633).
func buildHealthHandler(opts ServerOptions, s *Server, w *wiring) *HealthHandler {
	return &HealthHandler{
		dispatcherMetrics:  s.dispatcher.Metrics,
		router:             s.router,
		auth:               s.auth,
		startedAt:          w.startedAt,
		workspaceID:        opts.Identity.WorkspaceID,
		workspaceName:      opts.Identity.WorkspaceName,
		version:            opts.Identity.Version,
		noOutputTimeout:    opts.Watchdog.NoOutput,
		totalTimeout:       opts.Watchdog.Total,
		noOutputTimeoutStr: opts.Watchdog.NoOutput.String(),
		totalTimeoutStr:    opts.Watchdog.Total.String(),
		watchdogNoOut:      w.watchdog.noOutPtr(),
		watchdogTotal:      w.watchdog.totalPtr(),
		nodeAccess:         s.nodes,
		configSHA256:       opts.Config.SHA256,
		configLoadedAt:     opts.Config.LoadedAt,
		configLive:         opts.Config.Live,
		configPath:         opts.Config.Path,
		platforms:          s.platforms,
		platformCaps:       platform.CapabilityMatrix(s.platforms),
		hubDropped:         s.hub.DroppedMessages,
		// A method value on a nil *cron.Scheduler is fine: RunStoreHealth
		// reports disabled for a nil receiver and the probe omits the section.
		cronRunStore: opts.Scheduler.RunStoreHealth,
	}
}
