package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/dashboard/discovery"
	"github.com/naozhi/naozhi/internal/dashboard/ext/accessprofile"
	"github.com/naozhi/naozhi/internal/dashboard/ext/agentevents"
	"github.com/naozhi/naozhi/internal/dashboard/ext/cli"
	"github.com/naozhi/naozhi/internal/dashboard/ext/planner"
	"github.com/naozhi/naozhi/internal/dashboard/ext/uisettings"
	dashsession "github.com/naozhi/naozhi/internal/dashboard/session"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
	"github.com/naozhi/naozhi/internal/uiprefs"
)

const defaultDedupCapacity = 10000

// Server is the HTTP entry point for Naozhi.
//
// Each field carries `// 读写: <files>` naming the non-test files in this
// package that access it via the `s.X` receiver path (method definitions on
// the field's type are out of scope). New fields MUST add the annotation.
// The struct is intentionally flat: routes.go and the routes_snapshot_test.go
// AST contract match the `s.<handlerField>.<method>` selector shape, so the
// role-grouped dividers below are the cognitive map (#2197).
type Server struct {
	// ── HTTP entry ─────────────────────────────────────
	addr      string         // 读写: server.go
	mux       *http.ServeMux // 读写: debug_expvar.go, debug_pprof.go, routes.go, server.go
	startedAt time.Time      // 读写: server.go
	onReady   func()         // 读写: server.go (called after listener is bound)
	// appCtx is the process-lifetime context every background loop, the Hub
	// and the upload-store cleaner hang off. Created in buildServer (#2552) so
	// construction no longer has to wait for Start: Start links its own ctx to
	// appCancel instead of minting a second context.
	appCtx    context.Context    // 读写: build_dashboard.go, build_dispatch.go, routes.go, server.go (HubOptions.ParentCtx)
	appCancel context.CancelFunc // 读写: server.go (Start's linker + the Serve-error path)
	logger    *slog.Logger       // 读写: server.go

	// uploadStore holds pre-uploaded attachments until the matching send
	// consumes them; shared by the Hub (WS file_ids) and SendHandler (HTTP).
	// Built in buildDashboard, cleanup loop started in registerDashboard.
	uploadStore *uploadStore // 读写: build_dashboard.go, routes.go

	// ── core deps ──────────────────────────────────────
	router     *session.Router  // 读写: build_dashboard.go, build_dispatch.go, send_dispatch_adapter.go, server.go, server_loops.go, takeover.go
	scheduler  cronScheduler    // 读写: build_dashboard.go, build_dispatch.go, server.go (narrowed to the cronScheduler consumer view, #1648)
	hub        *Hub             // 读写: build_dashboard.go, routes.go, send.go, server.go, server_loops.go (WebSocket hub)
	projectMgr *project.Manager // 读写: build_dashboard.go, build_dispatch.go, server.go, server_loops.go

	// ── multi-node ─────────────────────────────────────
	nodes             *nodeRegistry       // 读写: build_dashboard.go, server.go (single owner of the node table; same instance as Hub.nodes)
	reverseNodeServer *node.ReverseServer // 读写: routes.go, server.go

	// ── dashboard / API handler groups ─────────────────
	auth       *auth.Handlers        // 读写: build_dashboard.go, dashboard_ccassets.go, debug_expvar.go, debug_pprof.go, routes.go, server.go
	discoveryH *discovery.Handlers   // 读写: server.go
	sessionH   *dashsession.Handlers // 读写: server.go, server_loops.go
	healthH    *HealthHandler        // 读写: routes.go, server.go

	// ── send / dispatch wiring ─────────────────────────
	dispatcher      *dispatch.Dispatcher         // 读写: server.go (ctor builds; Start only calls BuildHandler)
	dedup           *platform.Dedup              // 读写: build_dispatch.go, server.go (ctor only)
	sessionGuard    *session.Guard               // 读写: build_dashboard.go, build_dispatch.go, server.go
	msgQueue        *dispatch.MessageQueue       // 读写: build_dashboard.go, build_dispatch.go, server.go
	agents          map[string]session.AgentOpts // 读写: build_dashboard.go, build_dispatch.go, server.go
	agentCommands   map[string]string            // 读写: build_dashboard.go, build_dispatch.go, server.go
	dashboardToken  string                       // 读写: build_dashboard.go, debug_expvar.go, debug_pprof.go, routes.go, server.go
	allowedRoot     string                       // 读写: build_dashboard.go, build_dispatch.go, server.go (also Hub.allowedRoot)
	noOutputTimeout time.Duration                // 读写: build_dispatch.go, server.go (timeout error messages)
	totalTimeout    time.Duration                // 读写: build_dispatch.go, server.go

	// ── on-disk paths / caches / sysession ─────────────
	claudeDir      string               // 读写: build_dispatch.go, server.go, takeover.go
	discoveryCache *discoveryCache      // 读写: server.go (background-cached local discovery results)
	scratchPool    *session.ScratchPool // 读写: build_dashboard.go, routes.go, server.go (ephemeral aside sessions for preview drawer)
	sysessionMgr   *sysession.Manager   // 读写: build_dashboard.go, server.go (system-daemon Tick scheduling)
	orient         *orientConfig        // 读: build_dashboard.go, server.go (image auto-orientation; nil = feature off)

	// ── modes / resolver / node cache ──────────────────
	debugMode bool                 // 读写: routes.go, server.go (gates /api/debug/pprof and /api/debug/vars)
	resolver  *session.KeyResolver // 读写: build_dashboard.go, build_dispatch.go, server.go (session-key → opts derivation)
	nodeCache *node.CacheManager   // 读写: server.go (background-cached remote node data)

	// ── watchdog counters ──────────────────────────────
	// watchdog holds the no-output / total watchdog-kill counters exposed via
	// /health and /api/sessions; dispatch increments them via noOutPtr()/totalPtr().
	// 读写: build_dispatch.go, server.go
	watchdog watchdogCounters

	// shutdownComplete closes once Start's shutdown goroutine has drained
	// in-flight HTTP requests; the process shutdown sequencer blocks on it
	// before router.Shutdown(). 读写: server.go (ctor + Start + accessor)
	shutdownComplete chan struct{}

	// platforms wires each IM channel's webhook + outbound sender at
	// routes-registration time. 读写: build_dispatch.go, server.go
	platforms map[string]platform.Platform
}

// replyTagForBackend resolves a backend ID ("claude" / "kiro") to the short
// tag dispatch appends to outbound IM replies; unknown ids return "" so the
// footer is skipped. Empty id means "claude" so stores predating the Backend
// field keep their "[cc]" footer (docs/rfc/multi-backend.md §7). The
// once-guard lazily registers defaults for tests that skip main's
// backend.RegisterDefaults().
func replyTagForBackend(id string) string {
	replyTagForBackendOnce.Do(func() {
		if len(backend.All()) == 0 {
			backend.RegisterDefaults()
		}
	})
	if id == "" {
		id = "claude"
	}
	if p, ok := backend.Get(id); ok {
		return p.DefaultTag
	}
	return ""
}

var replyTagForBackendOnce sync.Once

// log returns the injected component logger (ServerOptions.Logger) or
// slog.Default(). New structured logging in this package should go through it.
func (s *Server) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// RotateDashboardSessions invalidates every outstanding dashboard auth cookie
// without a restart by bumping the auth generation mixed into the cookie HMAC;
// both the HTTP cookie path and the WS upgrade path read the live MAC. Safe
// from any goroutine (#595).
func (s *Server) RotateDashboardSessions() {
	if s.auth == nil {
		return
	}
	s.auth.RotateCookieGen()
	s.log().Info("dashboard auth sessions rotated; outstanding cookies invalidated",
		"reason", "rotate_dashboard_sessions")
}

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
	// interface wrapping nil, or every `s.scheduler != nil` guard would fire.
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

	// Single owner of the live node table; nil opts.Nodes ⇒ empty table.
	nodes := newNodeRegistry(opts.Nodes)

	// allowed_root is the one directory-traversal guard for dashboard /cd,
	// cron WorkDir and takeover CWD. Empty is the legitimate single-user
	// default, so boot is not failed (`naozhi doctor` hard-fails instead);
	// token-protected + network-reachable escalates to Error so alerting
	// pipelines that ignore Warn still see it (#658).
	if opts.AllowedRoot == "" {
		slog.Warn("server.allowed_root is unset; dashboard /cd, cron WorkDir, and takeover CWD accept any absolute path — set allowed_root in config.yaml to restrict")
		if opts.DashboardToken != "" && isPlaintextPublicAddr(opts.Addr) {
			slog.Error("allowed_root unset on a token-protected, network-reachable dashboard — any authenticated user can set cron WorkDir to /etc or other system paths and let the CLI write there. Set server.allowed_root before exposing this listener; `naozhi doctor` will hard-fail this configuration.",
				"addr", opts.Addr,
			)
		}
	}

	cookieSecret := loadOrCreateCookieSecret(opts.StateDir)
	// cookieGen is mixed into the auth-cookie HMAC so every restart yields a
	// fresh MAC even on a shared stateDir. CSPRNG-seeded: a time-based seed is
	// reconstructible from /health uptime, letting an attacker holding
	// token+secret forge a cookie valid across restarts (#595, #437).
	cookieGen := auth.RandomCookieGen()

	// One KeyResolver shared by dispatcher, hub and ProjectHandlers;
	// NewDataSource returns untyped nil when projectMgr is nil.
	resolver := session.NewKeyResolver(agents, project.NewDataSource(opts.ProjectManager))

	s := &Server{
		addr:             addr,
		mux:              http.NewServeMux(),
		shutdownComplete: make(chan struct{}),
		platforms:        platforms,
		router:           router,
		dedup:            platform.NewDedup(defaultDedupCapacity),
		sessionGuard:     session.NewGuard(),
		msgQueue: dispatch.NewMessageQueueWithMode(
			opts.Queue.MaxDepth,
			opts.Queue.CollectDelay,
			dispatch.ParseQueueMode(opts.Queue.Mode),
		),
		startedAt:       time.Now(),
		logger:          opts.Logger,
		agents:          agents,
		agentCommands:   agentCommands,
		scheduler:       scheduler,
		claudeDir:       claudeDir,
		allowedRoot:     opts.AllowedRoot,
		noOutputTimeout: opts.NoOutputTimeout,
		totalTimeout:    opts.TotalTimeout,
		dashboardToken:  opts.DashboardToken,
		debugMode:       opts.DebugMode,
		onReady:         opts.OnReady,
		projectMgr:      opts.ProjectManager,
		resolver:        resolver,
		nodes:           nodes,
		sysessionMgr:    opts.Sysession.Manager,
		orient:          buildOrientConfig(opts),

		// auth stays on Server: debug_expvar / debug_pprof / ccassets wrap
		// through it and RotateDashboardSessions reaches for it at runtime.
		// Handler-group literals live in build_handlers.go (limiter rationale there).
		auth: buildAuthHandlers(opts, cookieSecret, cookieGen),
	}

	// hs carries the mount-only handlers from here to route registration and
	// then goes out of scope (#2553, handler_set.go). Only the four lifecycle
	// participants get copied onto Server.
	hs := &handlerSet{
		systemH:  buildSystemHandlers(opts, router),
		plannerH: planner.New(planner.Deps{Router: router}),
		// Empty StateDir yields an in-memory prefs store (no persistence).
		uiSettingsH: uisettings.New(uiprefs.New(opts.StateDir)),
		// Empty ConfigPath keeps the create endpoint disabled (400).
		accessProfilesH: accessprofile.New(router, opts.Config.Path, opts.Config.AccessProfileSecretsDir),
		cronH:           buildCronHandlers(opts, claudeDir),
		transcribeH:     buildTranscribeHandler(opts),
	}

	// The process-lifetime context is created HERE, not in Start (#2552).
	// Everything downstream (Hub, upload-store cleaner, background loops) hangs
	// off it, so construction no longer has to wait for a Start-time context;
	// Start links its own ctx into appCancel rather than minting a second one.
	s.appCtx, s.appCancel = context.WithCancel(context.Background())

	// Scratch pool ahead of the Hub: HubOptions.ScratchPool needs it and it
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
		s.hub.BroadcastSessionsUpdate,
	)

	s.discoveryCache = newDiscoveryCache(claudeDir, s.router.ManagedExcludeSets, opts.ProjectManager)

	hs.discoveryH = buildDiscoveryHandlers(opts, claudeDir, s.discoveryCache, s.nodes, s.nodeCache, s.hub.BroadcastSessionsUpdate, s.appCtx)
	hs.projectH = buildProjectHandlers(opts, resolver, s.nodes, s.nodeCache, s.hub.ctx)
	agentIDs := agentIDList(agents)
	hs.costH = buildCostHandlers(opts, router)
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
		routerView = router
	}
	hs.sessionH = dashsession.New(dashsession.Deps{
		// /api/sessions snapshot enrichment goes through the hub's tailer registry.
		SnapshotEnricher: s.hub.enrichSnapshot,
		Router:           routerView,
		ProjectMgr:       projectSrc,
		Scheduler:        scheduler,
		CronSessions:     scheduler,
		SysWorkDir:       opts.Sysession.WorkDir,
		ClaudeDir:        claudeDir,
		AllowedRoot:      opts.AllowedRoot,
		Agents:           agents,
		AgentIDs:         agentIDs,
		NodeAccess:       s.nodes,
		NodeCache:        s.nodeCache,
		StartedAt:        s.startedAt,
		BackendTag:       tag,
		WorkspaceID:      opts.WorkspaceID,
		WorkspaceName:    opts.WorkspaceName,
		VersionTag:       opts.Version,
		WatchdogNoOut:    s.watchdog.noOutPtr(),
		WatchdogTotal:    s.watchdog.totalPtr(),
		RetiredStore:     retiredReader,
		ValidateWS:       validateWorkspace,
		SystemInfoFn:     systemInfo,

		ProjectStableKeyEnabled: opts.ProjectStableKeyEnabled,
	})
	hs.sessionH.InitStaticStats()
	hs.sessionH.WarmHistoryCache()
	// Router.Reset/Remove hook (LRU eviction deliberately does not fire it),
	// registered once AFTER sessionH exists so the fan-out is never
	// half-wired while WarmHistoryCache runs: msgQueue.Cleanup frees the
	// per-session FIFO entry; InvalidateHistoryCache makes the retired
	// session visible to the history popover within one poll.
	{
		msgCleanup := s.msgQueue.Cleanup
		sessionH := hs.sessionH
		router.SetOnKeyRetired(func(key string) {
			msgCleanup(key)
			sessionH.InvalidateHistoryCache()
		})
	}

	router.SetOnSessionRetired(func(_ string, sessionID string) {
		hs.sessionH.RecordRetired(sessionID)
	})
	hs.agentEventsH = agentevents.New(agentevents.Deps{
		Router:     router,
		NodeAccess: s.nodes,
	})

	// StartupCtx lets SIGTERM during startup abort the --version probe.
	startupCtx := opts.StartupCtx
	if startupCtx == nil {
		startupCtx = context.Background()
	}
	// /api/cli/backends?node=<id> proxies the manifest to a remote node.
	hs.cliH = cli.NewCLIBackendsHandlerCtx(startupCtx, router, s.nodes)

	// Dispatcher is built HERE, not in Start (#2633); see build_dispatch.go.
	// Building it before healthH lets the metrics closure be a constructor
	// argument rather than a field back-filled from Start.
	s.dispatcher = s.buildDispatcher()

	platNames := platformNameSet(platforms)
	s.healthH = &HealthHandler{
		dispatcherMetrics:  s.dispatcher.Metrics,
		router:             router,
		auth:               s.auth,
		startedAt:          s.startedAt,
		workspaceID:        opts.WorkspaceID,
		workspaceName:      opts.WorkspaceName,
		version:            opts.Version,
		noOutputTimeout:    opts.NoOutputTimeout,
		totalTimeout:       opts.TotalTimeout,
		noOutputTimeoutStr: opts.NoOutputTimeout.String(),
		totalTimeoutStr:    opts.TotalTimeout.String(),
		watchdogNoOut:      s.watchdog.noOutPtr(),
		watchdogTotal:      s.watchdog.totalPtr(),
		nodeAccess:         s.nodes,
		configSHA256:       opts.Config.SHA256,
		configLoadedAt:     opts.Config.LoadedAt,
		configPath:         opts.Config.Path,
		platforms:          platNames,
		platformsStatus:    platformStatusMap(platNames),
		platformCaps:       platform.CapabilityMatrix(platforms),
		hubDropped:         s.hub.DroppedMessages,
	}

	if opts.ReverseNodeServer != nil {
		s.reverseNodeServer = opts.ReverseNodeServer
		for id, displayName := range opts.ReverseNodeServer.AllNodes() {
			s.nodes.SetKnown(id, displayName)
		}
		opts.ReverseNodeServer.OnRegister = func(id string, rc *node.ReverseConn) {
			s.nodes.Add(id, rc)
			go s.nodeCache.RefreshFor(id) // RefreshFor calls onChange → BroadcastSessionsUpdate
		}
		opts.ReverseNodeServer.OnDeregister = func(id string) {
			s.nodes.Remove(id)
			s.nodeCache.PurgeNode(id)
			s.hub.PurgeNodeSubscriptions(id)
			s.hub.BroadcastSessionsUpdate()
		}
	}

	hs.checkLimiters(s.scheduler != nil)

	// Server keeps the four handlers that outlive registration (see
	// handler_set.go for why each one does).
	s.sessionH = hs.sessionH
	s.discoveryH = hs.discoveryH

	// Routes are registered HERE, not in Start (#2553): hs can only be a local
	// if nothing after construction needs it. Start keeps the goroutine starts
	// (startDashboardLoops) and the routes that need the dispatcher.
	s.registerDashboard(hs)

	return s, hs
}

// listenTCP is the listener factory Start binds with; a package var so tests
// can inject a listener whose Accept fails post-bind.
var listenTCP = net.Listen

// Start registers routes and begins serving.
func (s *Server) Start(ctx context.Context) error {
	// Early-return error paths run before the shutdown goroutine (the sole
	// closer of shutdownComplete) exists; the process shutdown sequencer
	// blocks on ShutdownComplete() unconditionally, so close it here unless
	// the goroutine took ownership. close() must happen exactly once.
	shutdownClosed := false
	defer func() {
		if !shutdownClosed {
			close(s.shutdownComplete)
		}
	}()
	// appCancel is deferred FIRST (#2633) so every return path — including
	// the platform-start error below, which used to return before this defer
	// existed — tears down the appCtx tree (Hub, dispatcher StopCtx, loops).
	// Idempotent, so the Serve-error path's explicit call below is harmless.
	defer s.appCancel()

	// The dispatcher exists since buildServerWithHandlers; Start only turns
	// it into the platform-facing handler.
	handler := s.dispatcher.BuildHandler()

	var startedPlatforms []platform.RunnablePlatform
	for _, p := range s.platforms {
		p.RegisterRoutes(s.mux, handler)
		slog.Info("platform registered", "name", p.Name())

		if rp, ok := p.(platform.RunnablePlatform); ok {
			if err := rp.Start(handler); err != nil {
				// Roll back already-started platforms; log stop failures so a
				// dangling websocket holding the process open is visible.
				for _, sp := range startedPlatforms {
					if stopErr := sp.Stop(); stopErr != nil {
						slog.Warn("platform rollback stop failed",
							"name", sp.Name(), "err", stopErr)
					}
				}
				return fmt.Errorf("start platform %s: %w", p.Name(), err)
			}
			startedPlatforms = append(startedPlatforms, rp)
		}
	}

	// s.appCtx (created in buildServer) is the single cancel source for every
	// background loop AND the shutdown goroutine. Start does not mint a second
	// context; it links the caller's ctx into appCancel so SIGTERM still
	// cascades, while a srv.Serve error can cancel it directly. Without this
	// the Serve-error path would leave loops alive and discoveryCache.Wait()
	// blocked forever.
	//
	// The linker exits on either side, so it cannot outlive the Server.
	go func() {
		select {
		case <-ctx.Done():
			s.appCancel()
		case <-s.appCtx.Done():
		}
	}()

	s.startDashboardLoops()
	s.nodeCache.StartLoop(s.appCtx)
	s.discoveryCache.startLoop(s.appCtx)
	s.startProjectScanLoop(s.appCtx)
	// Token-protected dashboard over plaintext with no trusted proxy: tokens
	// and cookies are sniffable. trustedProxy=true asserts TLS terminates upstream.
	if s.dashboardToken != "" && !s.auth.TrustedProxy && isPlaintextPublicAddr(s.addr) {
		slog.Warn(plaintextDashboardTokenWarning, "addr", s.addr)
	}
	// No-auth on a publicly reachable address makes every /api/* world-reachable.
	if shouldWarnNoTokenOpen(s.dashboardToken, s.addr, s.auth.TrustedProxy) {
		slog.Warn(noTokenOpenWarning,
			"addr", s.addr,
			"trusted_proxy", s.auth.TrustedProxy,
		)
	} else if s.dashboardToken == "" {
		// Loopback + no token is the local-dev path, but an accidentally
		// cleared token must still be visible in the startup journal.
		slog.Warn("dashboard token not configured; all API callers accepted without authentication",
			"addr", s.addr,
		)
	}
	// /ws-node carries node tokens; plaintext on a public bind lets a sniffer
	// impersonate the remote node.
	if shouldWarnReverseNodePlaintext(s.reverseNodeServer != nil, s.auth.TrustedProxy, s.addr) {
		slog.Warn(reverseNodePlaintextWarning,
			"addr", s.addr,
		)
	}
	// With trustedProxy every per-IP gate trusts the last XFF hop, so the proxy
	// MUST drop-and-replace client-supplied XFF (ALB/CloudFront default; nginx
	// real_ip_recursive + allowlist) or `X-Forwarded-For: <victim>, <attacker>`
	// lands in the victim's bucket. Info level: the upstream contract is
	// unverifiable from here (#848).
	if s.auth.TrustedProxy {
		slog.Info(trustedProxyXFFReminder, "addr", s.addr)
	}
	// Effective turn timeouts are logged so operators can confirm them from
	// journalctl (#1054).
	slog.Info("server starting",
		"addr", s.addr,
		"no_output_timeout", s.noOutputTimeout,
		"total_timeout", s.totalTimeout,
	)

	ln, err := listenTCP("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}

	// Middleware order: withTraceID outermost so every request (auth or not)
	// carries X-Request-ID before gzip mutates the writer; withAPIVersionAlias
	// innermost so /api/v1/<rest> is rewritten just before mux matching while
	// trace-id + gzip observe the original path (#677, #425).
	srv := &http.Server{
		Handler:           withTraceID(gzipMiddleware(withAPIVersionAlias(s.mux))),
		ReadHeaderTimeout: 5 * time.Second, // Slowloris defense
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Well below the 1 MB default so unauthenticated clients cannot force
		// megabyte header buffering; 64 KB is ample for cookies + X-Forwarded-*.
		MaxHeaderBytes: 64 * 1024,
	}

	// Notify caller that the listener is bound and ready to accept connections.
	if s.onReady != nil {
		s.onReady()
	}

	if s.sessionH != nil && s.sessionH.RetiredStorePresent() {
		go s.runRetiredStoreFlusher(s.appCtx)
	}

	// Channel allocated at construction so ShutdownComplete() may be read
	// before Start runs. From here the goroutine below is the sole closer.
	shutdownComplete := s.shutdownComplete
	shutdownClosed = true
	go func() {
		<-s.appCtx.Done()
		slog.Info("shutting down server")

		// Shutdown WebSocket hub (non-nil since buildServer, #2552)
		s.hub.Shutdown()

		if s.scratchPool != nil {
			s.scratchPool.Stop()
		}

		// Drain WarmHistoryCache before claudeDir-dependent state goes away,
		// then flush the retired-store so retirements between ticks survive.
		if s.sessionH != nil {
			s.sessionH.WaitWarmHistory()
			s.sessionH.FlushRetiredStore()
		}

		// Discovery refresh goroutine must exit before projectMgr state is torn down.
		if s.discoveryCache != nil {
			s.discoveryCache.Wait()
		}

		// Stop RunnablePlatforms (e.g. WebSocket connections)
		for _, p := range s.platforms {
			if rp, ok := p.(platform.RunnablePlatform); ok {
				if err := rp.Stop(); err != nil {
					slog.Error("stop platform", "name", p.Name(), "err", err)
				}
			}
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), session.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown error", "err", err)
		}
		// No new requests after srv.Shutdown; drain parked takeover/close
		// goroutines before signalling completion.
		if s.discoveryH != nil {
			s.discoveryH.Wait()
		}
		close(shutdownComplete)
	}()

	err = srv.Serve(ln)
	// On a non-shutdown Serve error the parent ctx may never cancel; cancel
	// appCtx so the shutdown goroutine drains and closes shutdownComplete.
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.appCancel()
		<-shutdownComplete
		return err
	}
	// Wait for the shutdown goroutine to finish draining connections.
	select {
	case <-shutdownComplete:
	case <-s.appCtx.Done():
		<-shutdownComplete
	}
	return err
}

// ShutdownComplete returns a channel that closes once Start's shutdown
// goroutine has drained in-flight HTTP requests (or Start returned an error
// early). The process-level shutdown sequencer must block on it before
// router.Shutdown(), otherwise an in-flight handler can observe a
// half-cleaned session map. It never closes if Start is never invoked.
func (s *Server) ShutdownComplete() <-chan struct{} {
	return s.shutdownComplete
}

// retiredStoreFlushInterval bounds retirements lost on a hard kill while
// keeping fsync churn modest.
const retiredStoreFlushInterval = 30 * time.Second

// Prune cutoff is 2× the 7-day history window so entries that just left the
// popover are not raced; RetiredStore.Prune's cap handles pathological volume.
const (
	retiredStorePruneInterval = 6 * time.Hour
	retiredStorePruneCutoff   = 14 * 24 * time.Hour
)

// plaintextDashboardTokenWarning is logged when a token-protected dashboard
// is served over plaintext HTTP with no trusted proxy. Named so tests can
// pin the exact text.
const plaintextDashboardTokenWarning = "dashboard token served over plaintext HTTP with no trusted proxy: " +
	"bearer tokens and session cookies may be sniffed; authenticated /health responses " +
	"also leak workspace_id, node status, version, and watchdog counters in the clear. " +
	"Terminate TLS upstream and set server.trusted_proxy=true, " +
	"or bind to 127.0.0.1 for local-only access."

// noTokenOpenWarning is logged when dashboard_token is unset on a publicly
// reachable bind. Named so tests can pin the exact text.
const noTokenOpenWarning = "no dashboard_token configured on a non-loopback bind: " +
	"the ENTIRE dashboard API is open to any caller. " +
	"Anyone reaching this port can send messages to sessions, read workspace files under allowed_root, " +
	"alter cron schedules, and trigger transcription. Also: uploadOwner falls back to client IP, " +
	"so users sharing a NAT / LAN / egress gateway can see each other's inline uploads. " +
	"Either set server.dashboard_token, bind to 127.0.0.1 for single-user use, " +
	"or set server.trusted_proxy=true with an upstream that enforces access control."
