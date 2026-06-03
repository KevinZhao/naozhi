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
	"github.com/naozhi/naozhi/internal/cryptoutil"
	"github.com/naozhi/naozhi/internal/dashboard/auth"
	dashcron "github.com/naozhi/naozhi/internal/dashboard/cron"
	"github.com/naozhi/naozhi/internal/dashboard/discovery"
	"github.com/naozhi/naozhi/internal/dashboard/ext/agentevents"
	extccassets "github.com/naozhi/naozhi/internal/dashboard/ext/ccassets"
	"github.com/naozhi/naozhi/internal/dashboard/ext/cli"
	"github.com/naozhi/naozhi/internal/dashboard/ext/memory"
	"github.com/naozhi/naozhi/internal/dashboard/ext/scratch"
	"github.com/naozhi/naozhi/internal/dashboard/ext/transcribe"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	dashsession "github.com/naozhi/naozhi/internal/dashboard/session"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
)

const (
	defaultDedupCapacity = 10000

	// maxRequestBodyBytes is the per-handler request-body read limit applied
	// via http.MaxBytesReader. The single source of truth lives in
	// internal/dashboard/httputil so dashboard sub-packages can share the
	// limit without re-importing internal/server. Phase 3-prep
	// (server-split-phase4-design.md §6.5 Plan B).
	maxRequestBodyBytes = httputil.MaxRequestBodyBytes
)

// Server is the HTTP entry point for Naozhi.
//
// Field-block contract (server-split-phase4-design.md §五 / §六.6):
// Each field below carries `// 读写: <files>` to indicate which non-test
// files in this package access this Server-struct field via the `s.X`
// receiver path. New fields MUST add this annotation.
//
// R250-ARCH-22 (#1183): the section dividers below group fields by their
// current functional role (HTTP entry / core deps / handlers / …), NOT by a
// planned future disposition. Earlier revisions tagged each group with a
// "Phase 5: → routes.go locals / NewHub Options / metrics package" target;
// those tags had drifted (categories no longer matched reality) and were only
// load-bearing as a refactor-that-never-came. Treat this field list as the
// canonical current shape: when a field genuinely moves out, delete it here in
// the same change rather than pre-annotating an intended destination.
// Verification rule:
//
//	awk '/^type Server struct/,/^}$/' server.go | grep -cE '^\s+[a-zA-Z_]+ '
//
// must equal the field count documented in
// docs/design/server-split-phase4-baseline.md §2 (currently 48 — the
// logger field was added for R247-ARCH-4 / #620 logger injection).
//
// R250-ARCH-11 (#1174): scope clarification — the annotation tracks
// access to *this struct field*, not usage of the field's underlying
// type. So `agentEventsH *agentevents.Handler` lists only `server.go,
// dashboard.go` (the two files that read/write `s.agentEventsH`), even
// though `dashboard_agent_events.go` defines methods on
// `*AgentEventsHandlers`. Method definitions on the underlying type
// are intentionally out of scope: they don't access the Server field
// and therefore can't be invalidated by a future field-level refactor
// (renaming or moving `s.agentEventsH`). A type-usage tracker would
// be a separate verifier; this contract stays scoped to receiver-path
// reads/writes so the annotation diff stays localised when handlers
// are split out per Phase 5.
type Server struct {
	// ── HTTP entry ─────────────────────────────────────
	addr      string          // 读写: server.go
	mux       *http.ServeMux  // 读写: server.go, dashboard.go, debug_expvar.go, debug_pprof.go
	startedAt time.Time       // 读写: server.go
	onReady   func()          // 读写: server.go (called after listener is bound)
	appCtx    context.Context // 读写: server.go, dashboard.go (HubOptions.ParentCtx)
	logger    *slog.Logger    // 读写: server.go (R247-ARCH-4 #620 injected component logger; nil → slog.Default via s.log())

	// ── core deps ──────────────────────────────────────
	router     *session.Router  // 读写: server.go, dashboard.go, dashboard_system.go, send.go, takeover.go, consumer.go
	scheduler  cronScheduler    // 读写: server.go, dashboard.go, dashboard_cron.go, dashboard_cron_transcript.go, wshub.go (narrowed to the cronScheduler consumer view, #1648)
	hub        *Hub             // 读写: server.go, dashboard.go, send.go (WebSocket hub)
	projectMgr *project.Manager // 读写: server.go, dashboard.go, project_api.go, project_files.go

	// ── multi-node ─────────────────────────────────────
	nodes             map[string]node.Conn // 读写: server.go, dashboard.go
	reverseNodeServer *node.ReverseServer  // 读写: server.go, dashboard.go
	nodesMu           sync.RWMutex         // 读写: server.go, dashboard.go (shared with Hub.nodesMu)

	// ── dashboard / API handler groups ─────────────────
	auth         *auth.Handlers        // 读写: server.go, dashboard.go, debug_expvar.go, debug_pprof.go
	cronH        *dashcron.Handlers    // 读写: server.go, dashboard.go
	transcribeH  *transcribe.Handler   // 读写: dashboard.go (ctor only in server.go)
	nodeAccess   *nodeAccessor         // 读写: server.go, dashboard.go
	discoveryH   *discovery.Handlers   // 读写: server.go, dashboard.go (Phase 3b 搬到 internal/dashboard/discovery)
	projectH     *dashproject.Handlers // 读写: server.go, dashboard.go
	sessionH     *dashsession.Handlers // 读写: server.go, dashboard.go
	healthH      *HealthHandler        // 读写: server.go (ctor only)
	sendH        *SendHandler          // 读写: dashboard.go (ctor only in server.go)
	cliH         *cli.Handler          // 读写: server.go, dashboard.go
	scratchH     *scratch.Handler      // 读写: dashboard.go (ctor only in server.go)
	memoryH      *memory.Handler       // 读写: dashboard.go (ctor only in server.go)
	ccAssetsH    *extccassets.Handler  // 读写: dashboard.go (ctor only in server.go)
	agentEventsH *agentevents.Handler  // 读写: server.go, dashboard.go

	// ── send / dispatch wiring ─────────────────────────
	dedup           *platform.Dedup              // 读写: server.go (ctor only)
	sessionGuard    *session.Guard               // 读写: server.go, dashboard.go
	msgQueue        *dispatch.MessageQueue       // 读写: server.go, dashboard.go
	agents          map[string]session.AgentOpts // 读写: server.go, dashboard.go, dashboard_session.go
	agentCommands   map[string]string            // 读写: server.go, dashboard.go
	dashboardToken  string                       // 读写: server.go, dashboard.go, dashboard_auth.go
	allowedRoot     string                       // 读写: server.go, dashboard.go (also Hub.allowedRoot)
	noOutputTimeout time.Duration                // 读写: server.go (timeout error messages)
	totalTimeout    time.Duration                // 读写: server.go

	// ── on-disk paths / caches / sysession ─────────────
	claudeDir      string               // 读写: server.go, takeover.go, discovery_cache.go, dashboard_cron_transcript.go, dashboard_discovered.go, dashboard_session.go
	workspaceName  string               // 读写: server.go (ctor only; copied into SessionHandlers/HealthHandler)
	discoveryCache *discoveryCache      // 读写: server.go (background-cached local discovery results)
	scratchPool    *session.ScratchPool // 读写: server.go, dashboard.go, wshub.go (ephemeral aside sessions for preview drawer)
	sysessionMgr   *sysession.Manager   // 读写: dashboard.go, dashboard_system.go (system-daemon Tick scheduling)

	// ── modes / resolver / node cache ──────────────────
	debugMode bool                 // 读写: dashboard.go (gates /api/debug/pprof and /api/debug/vars; R244-SEC-P3-1)
	headless  bool                 // 读写: send.go (explicit no-hub mode; gates the nil-hub send fallback — R248-ARCH-9 #379)
	resolver  *session.KeyResolver // 读写: server.go, dashboard.go (session-key → opts derivation)
	nodeCache *node.CacheManager   // 读写: server.go (background-cached remote node data)

	// ── watchdog counters ──────────────────────────────
	// watchdog groups the no-output / total watchdog-kill counters into one
	// cohesive observability unit (R243-ARCH-7 / #838). Exposed via /health
	// and /api/sessions; incremented by the dispatch watchdog through the
	// *atomic.Int64 handles returned by noOutPtr()/totalPtr().
	watchdog watchdogCounters

	// shutdownComplete closes once Start's shutdown goroutine has finished
	// draining in-flight HTTP requests (srv.Shutdown returned). Exposed via
	// ShutdownComplete() so the process-level shutdown sequencer can block on
	// the real HTTP-drain barrier before tearing down router state, instead
	// of racing router.Shutdown() against handlers still observing the
	// session map. S11 / R194-COR. 读写: server.go (ctor + Start + accessor)
	shutdownComplete chan struct{}

	// platforms is read at routes-registration time (server.go) to wire each
	// IM channel's webhook + outbound sender; knownNodes maps configured node
	// IDs → display names (read at server.go:433/553). R20260603-ARCH-1: the
	// former sibling `backendTag` field was write-only (ctor-only, no receiver
	// read) and has been removed — the live reply tag flows through the local
	// `tag` var into SessionHandlers.BackendTag (server.go ctor).
	platforms  map[string]platform.Platform
	knownNodes map[string]string
}

// Workspace 验证 helpers (validateWorkspace / classifyWorkspaceErr /
// validateRemoteWorkspace / pathErrReason) + 4 个 ErrWorkspace* sentinel
// 抽到 server_validate.go (Phase 5-prep, 2026-05-28).

// loadOrCreateCookieSecret moved to server_cookie.go (Phase 5-prep, 2026-05-28).

// replyTagForBackend resolves a backend ID (e.g. "claude" / "kiro") to the
// short tag the dispatch layer appends to outbound IM replies ("cc" /
// "kiro"). Reads from the cli/backend Profile registry; unknown ids return
// "" so dispatch skips the footer rather than emitting garbled output.
//
// Empty backend ID resolves to "cc" so legacy single-backend deployments
// without the Backend field on stored sessions keep their historical
// "[cc]" footer. docs/rfc/multi-backend.md §7.
//
// Registry-not-ready path: production wires backend.RegisterDefaults() in
// cmd/naozhi/main.go before any server constructs. Tests that build a
// Server without that call would see backend.Get return false and lose the
// tag — replyTagForBackendOnce ensures a one-shot lazy registration so tests
// remain green without each having to wire the registry by hand.
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

// log returns the Server's component logger. When no logger was injected via
// ServerOptions.Logger it falls back to slog.Default(), so call sites can move
// off the bare slog.* package functions onto an injectable seam (R247-ARCH-4 /
// #620) without forcing every caller — or test — to supply one. This is the
// first concrete migration step: new structured logging in the server package
// should go through s.log() so a future change can inject a component-scoped
// (and test-swappable) logger instead of reading the process global.
func (s *Server) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// RotateDashboardSessions invalidates every outstanding dashboard auth cookie
// in real time, without a process restart. It bumps the auth handler's
// generation counter so the cookie HMAC changes; both the HTTP cookie path and
// the WS upgrade path read the live MAC (CookieMACFn: s.auth.CookieMAC), so an
// in-flight rotation propagates to every authenticated surface on the next
// request/handshake.
//
// R217-SEC-6 (#595): closes the "rotation has no explicit session
// invalidation" gap. Previously the only way to revoke outstanding cookies was
// to rotate the cookie secret and restart the process (or wait out the 24h
// MaxAge). This exposes server-side revocation that a future dashboard-token
// hot-reload / SIGHUP handler — or an operator-facing endpoint — can call to
// kick every browser back to /api/auth/login immediately. Safe to call from
// any goroutine (the underlying counter is an atomic increment).
func (s *Server) RotateDashboardSessions() {
	if s.auth == nil {
		return
	}
	s.auth.RotateCookieGen()
	s.log().Info("dashboard auth sessions rotated; outstanding cookies invalidated",
		"reason", "rotate_dashboard_sessions")
}

// New creates a new Server.
// ServerOptions lives in server_options.go (Phase 5-prep, 2026-05-28).

// NewWithOptions constructs a Server from a single ServerOptions value.
// Prefer this constructor for new call sites — it reads like a config
// literal and tolerates new fields being added without signature churn.
// The legacy New() wrapper still exists for backward compatibility.
//
// Required: opts.Router must be non-nil. opts.Addr must be set for the
// listener to bind. Other fields tolerate zero values.
func NewWithOptions(opts ServerOptions) *Server {
	return buildServer(opts)
}

// (Removed in R237-ARCH-14 / #614): the legacy positional-args
// constructor `func New(addr, router, platforms, agents, agentCommands,
// scheduler, backend, opts)` was retired once the dual-constructor pin
// in new_options_test.go was rewritten to live entirely in terms of
// NewWithOptions. The removal-condition spelled out in the prior godoc
// header (`Deprecated: use NewWithOptions`) is now satisfied — there
// are zero call sites in production (`cmd/`) and zero in tests, so the
// shim has no remaining purpose.
//
// TestServerNew_NotReintroduced (new_options_test.go) keeps drift from
// re-adding `func New(addr string, ...)` in this file by source-scan
// regression. Use NewWithOptions directly.

// buildServer is the shared construction path used by NewWithOptions.
// Kept private so the public entry point is the only way to create a
// *Server, and its contract can evolve without leaking internal assembly
// details.
func buildServer(opts ServerOptions) *Server {
	addr := opts.Addr
	router := opts.Router
	platforms := opts.Platforms
	agents := opts.Agents
	agentCommands := opts.AgentCommands
	// scheduler is boxed into the cronScheduler consumer interface (#1648).
	// A nil *cron.Scheduler must become a genuinely nil interface, not a
	// non-nil interface wrapping a nil pointer — otherwise every
	// `s.scheduler != nil` cron-enabled guard would fire for scheduler-less
	// deployments (and tests) and panic on the first method call.
	var scheduler cronScheduler
	if opts.Scheduler != nil {
		scheduler = opts.Scheduler
	}
	defaultBackend := opts.Backend
	// defaultTag is the fallback ReplyFooter tag for sessions whose
	// Backend() is empty (legacy stores predating the multi-backend Backend
	// field). docs/rfc/multi-backend.md §7.
	defaultTag := replyTagForBackend(defaultBackend)
	// tag is the legacy server-global reply footer value (flows into
	// SessionHandlers.BackendTag). Per-session ReplyFooterFn (wired below) reads
	// session.Backend() at IM-reply time so a kiro session in a claude-default
	// deployment gets [kiro] correctly.
	tag := defaultTag
	// R222-ARCH-9 / #724: env probe goes through the shared helper so the
	// "where is ~/.claude" decision lives in one place (claude_paths.go).
	// resolveClaudeDir returns "" when UserHomeDir fails, matching the
	// previous inline shape exactly — downstream sites already nil-check
	// claudeDir before joining or reading.
	claudeDir := resolveClaudeDir()

	nodes := opts.Nodes
	if nodes == nil {
		nodes = make(map[string]node.Conn)
	}
	knownNodes := make(map[string]string)
	for id, nc := range nodes {
		knownNodes[id] = nc.DisplayName()
	}

	// allowed_root is the one directory-traversal guard for dashboard /cd,
	// cron WorkDir, and handleTakeover CWD. Empty means "no restriction",
	// which is the legitimate single-user default but a real risk in
	// multi-user deployments. Surface it once at startup so operators can
	// audit their config rather than discovering the looseness via incident.
	//
	// R226-SEC-6: when both (a) a dashboard_token is configured (multi-user
	// intent) AND (b) the bind address is non-loopback (network-reachable),
	// raise the visibility from a single Warn line to two with explicit
	// "high severity" wording. Upgrading to fatal is deferred — existing
	// deployments rely on the warn-only contract and a hard-fail here
	// would break upgrades; `naozhi doctor` is the right place for the
	// fatal escalation. This pair-warn at least guarantees operators see
	// the risk before an incident, mapping onto the TODO's "升级 warn 严重度"
	// ask while preserving boot-compat.
	//
	// R237-SEC-9 / #658: the multi-user-intent + network-reachable branch
	// upgrades from Warn to Error. The single-user/loopback branch stays
	// Warn because that's the legitimate dev-laptop default. Error level
	// (a) routes to stderr in slog default text handler, (b) shows up
	// distinctly under journald PRIORITY filtering, and (c) trips alerting
	// pipelines that ignore Warn. The boot itself is intentionally not
	// failed — `naozhi doctor` remains the right place for hard-fail
	// because operators can run it standalone before exposing the listener
	// without burning a service-restart cycle on an upgrade.
	if opts.AllowedRoot == "" {
		slog.Warn("server.allowed_root is unset; dashboard /cd, cron WorkDir, and takeover CWD accept any absolute path — set allowed_root in config.yaml to restrict")
		if opts.DashboardToken != "" && isPlaintextPublicAddr(opts.Addr) {
			slog.Error("allowed_root unset on a token-protected, network-reachable dashboard — any authenticated user can set cron WorkDir to /etc or other system paths and let the CLI write there. Set server.allowed_root before exposing this listener; `naozhi doctor` will hard-fail this configuration.",
				"addr", opts.Addr,
			)
		}
	}

	cookieSecret := loadOrCreateCookieSecret(opts.StateDir)
	// R217-SEC-6 / R172-SEC-L4 (#595 / #437): cookieGen is mixed into the
	// auth-cookie HMAC alongside the dashboard token so every restart
	// produces a fresh MAC even when stateDir is shared (the common operator
	// setup). The seed was previously time.Now().UnixNano(), which is
	// predictable: an attacker who learns the process start time (exposed via
	// /health uptime, journal timestamps, or a banner) can reconstruct the
	// gen segment and — given the token + secret — forge a cookie that
	// survives any restart on the same stateDir. Seeding from a CSPRNG closes
	// that, so a captured cookie cannot be replayed against a future instance
	// even when token + secret are stable. RotateDashboardSessions can bump
	// the in-process seq counter at runtime to invalidate every outstanding
	// cookie atomically without a restart.
	cookieGen := cryptoutil.RandomCookieGen()

	// Construct KeyResolver once and share across dispatcher (wired in
	// Start), hub, and ProjectHandlers. project.NewDataSource returns
	// untyped nil when projectMgr is nil so the Resolver correctly
	// short-circuits the project-binding lookup in that mode.
	// docs/rfc/key-resolver.md Phase 4.
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
			opts.QueueMaxDepth,
			opts.QueueCollectDelay,
			dispatch.ParseQueueMode(opts.QueueMode),
		),
		startedAt:       time.Now(),
		logger:          opts.Logger,
		agents:          agents,
		agentCommands:   agentCommands,
		scheduler:       scheduler,
		claudeDir:       claudeDir,
		workspaceName:   opts.WorkspaceName,
		allowedRoot:     opts.AllowedRoot,
		noOutputTimeout: opts.NoOutputTimeout,
		totalTimeout:    opts.TotalTimeout,
		dashboardToken:  opts.DashboardToken,
		debugMode:       opts.DebugMode,
		headless:        opts.Headless,
		onReady:         opts.OnReady,
		projectMgr:      opts.ProjectManager,
		resolver:        resolver,
		nodes:           nodes,
		knownNodes:      knownNodes,
		sysessionMgr:    opts.SysessionManager,

		// Extracted handler groups (literals factored to build_handlers.go;
		// #738 / R246-CR-004). Helper docstrings carry the limiter rationale
		// that previously lived inline; buildServer keeps initialization
		// order visible while shedding ~40 LOC of struct literals.
		auth:        buildAuthHandlers(opts, cookieSecret, cookieGen),
		cronH:       buildCronHandlers(opts, claudeDir),
		transcribeH: buildTranscribeHandler(opts),
	}

	// Q1: the router's terminal-removal hook (Router.Reset/Remove; LRU
	// evictOldest deliberately does NOT fire it) is wired below — once
	// AFTER s.sessionH is constructed — so the single registration fans
	// out atomically to both cleanup riders:
	//
	//   1. msgQueue.Cleanup so the per-session FIFO map entry is truly
	//      deleted when the user resets or removes a session (/new,
	//      dashboard delete). Without this the entry is retained forever
	//      for gen-monotonicity — fine under LRU eviction (the session
	//      might return) but a slow leak when the key is never reused.
	//   2. sessionH.InvalidateHistoryCache so the history popover sees
	//      the just-retired session within one /api/sessions poll
	//      instead of being hidden by the 120s TTL.
	//
	// R238-CR-3: an earlier draft registered msgQueue.Cleanup here first
	// and overwrote the hook with the full fanout after sessionH was
	// constructed. WarmHistoryCache (a background goroutine) could
	// observe a Reset in that overwrite window with InvalidateHistoryCache
	// not yet wired. Skipping the half-wired registration removes the
	// race entirely; until the replay below, msgQueue.Cleanup is reachable
	// only via direct calls (router has no triggers active before the
	// first session/cron is started).

	// Construct the retired-store eagerly so the SessionHandlers below
	// can hold a non-nil pointer; the actual file load is best-effort
	// and a parse error here just means the store starts empty. The
	// store is only persisted when StateDir is configured — in-memory
	// only otherwise (tests, ephemeral deployments).
	retiredStore, retiredErr := buildRetiredStoreWithErr(opts.StateDir)
	if retiredErr != nil {
		slog.Warn("retired store load failed (degrades to last_active sort)", "err", retiredErr)
	}

	s.nodeAccess = newNodeAccessor(&s.nodesMu, s.nodes, s.knownNodes)

	hubBroadcast := func() {
		if s.hub != nil {
			s.hub.BroadcastSessionsUpdate()
		}
	}

	s.nodeCache = node.NewCacheManager(
		func() map[string]node.Conn {
			return s.nodeAccess.NodesSnapshot()
		},
		hubBroadcast,
	)

	s.discoveryCache = newDiscoveryCache(claudeDir, s.router.ManagedExcludeSets, opts.ProjectManager)

	// Wire extracted handler groups that depend on nodeAccess/nodeCache
	// (literals live in build_handlers.go; #738).
	s.discoveryH = buildDiscoveryHandlers(opts, claudeDir, s.discoveryCache, s.nodeAccess, s.nodeCache, hubBroadcast)
	// R247-ARCH-15 (#650): no closure here — ProjectHandlers stores
	// baseCtx as a plain field that registerDashboard wires via
	// SetBaseContext once `s.hub` exists. The two-phase construction
	// is unchanged (Hub still doesn't exist at this point); only the
	// DI shape moved from a captured closure to a direct field assign.
	s.projectH = buildProjectHandlers(opts, resolver, s.nodeAccess, s.nodeCache)
	agentIDs := agentIDList(agents)
	s.sessionH = dashsession.New(dashsession.Deps{
		Router:        router,
		ProjectMgr:    opts.ProjectManager,
		Scheduler:     scheduler,
		CronSessions:  scheduler,
		SysWorkDir:    opts.SysWorkDir,
		ClaudeDir:     claudeDir,
		AllowedRoot:   opts.AllowedRoot,
		Agents:        agents,
		AgentIDs:      agentIDs,
		NodeAccess:    s.nodeAccess,
		NodeCache:     s.nodeCache,
		StartedAt:     s.startedAt,
		BackendTag:    tag,
		WorkspaceID:   opts.WorkspaceID,
		WorkspaceName: opts.WorkspaceName,
		VersionTag:    opts.Version,
		WatchdogNoOut: s.watchdog.noOutPtr(),
		WatchdogTotal: s.watchdog.totalPtr(),
		RetiredStore:  retiredStore,
		ValidateWS:    validateWorkspace,
		SystemInfoFn:  systemInfo,
	})
	s.sessionH.InitStaticStats()
	s.sessionH.WarmHistoryCache()
	// Replay SetOnKeyRetired now that sessionH exists, fanning out to both
	// msgQueue.Cleanup and InvalidateHistoryCache. See the rationale at the
	// initial SetOnKeyRetired call earlier in New().
	{
		msgCleanup := s.msgQueue.Cleanup
		sessionH := s.sessionH
		router.SetOnKeyRetired(func(key string) {
			msgCleanup(key)
			sessionH.InvalidateHistoryCache()
		})
	}

	// Wire the session-retired hook independently of msgQueue.Cleanup
	// (which uses SetOnKeyRetired). Capturing s.sessionH in the closure
	// keeps the call zero-allocation in the steady state — atomic load
	// + 1 method call + 1 mutex op inside the store.
	router.SetOnSessionRetired(func(_ string, sessionID string) {
		s.sessionH.RecordRetired(sessionID)
	})
	s.agentEventsH = agentevents.New(agentevents.Deps{
		Router:     router,
		NodeAccess: s.nodeAccess,
	})

	// Scratch pool (ephemeral aside sessions). Bound to the same router so
	// scratches flow through the standard spawn/send/event path as managed
	// sessions; the saveStore/handleList filters on the "scratch:" prefix
	// keep them off the sidebar and out of sessions.json. The sweeper is
	// started later in registerDashboard so an early New() failure does not
	// leak the ticker goroutine.
	s.scratchPool = session.NewScratchPool(router, session.DefaultScratchMax, session.DefaultScratchTTL)
	// Thread StartupCtx into the --version probe so SIGTERM during
	// startup aborts promptly (R55-QUAL-004). Nil ctx falls back to
	// cli.NewCLIBackendsHandler's Background-derived path via the delegating
	// public ctor (keeps test/headless callers working).
	if opts.StartupCtx != nil {
		s.cliH = cli.NewCLIBackendsHandlerCtx(opts.StartupCtx, router)
	} else {
		s.cliH = cli.NewCLIBackendsHandler(router)
	}
	platNames := platformNameSet(platforms)
	s.healthH = &HealthHandler{
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
		nodeAccess:         s.nodeAccess,
		platforms:          platNames,
		hubDropped: func() int64 {
			if s.hub == nil {
				return 0
			}
			return s.hub.DroppedMessages()
		},
	}
	// sendH is wired after registerDashboard creates hub

	if opts.ReverseNodeServer != nil {
		s.reverseNodeServer = opts.ReverseNodeServer
		for id, displayName := range opts.ReverseNodeServer.AllNodes() {
			s.knownNodes[id] = displayName
		}
		opts.ReverseNodeServer.OnRegister = func(id string, rc *node.ReverseConn) {
			s.nodesMu.Lock()
			s.nodes[id] = rc
			s.nodesMu.Unlock()
			go s.nodeCache.RefreshFor(id) // RefreshFor calls onChange → BroadcastSessionsUpdate
		}
		opts.ReverseNodeServer.OnDeregister = func(id string) {
			s.nodesMu.Lock()
			delete(s.nodes, id)
			s.nodesMu.Unlock()
			s.nodeCache.PurgeNode(id)
			if s.hub != nil {
				s.hub.PurgeNodeSubscriptions(id)
				s.hub.BroadcastSessionsUpdate()
			}
		}
	}

	// R230C-SEC-11: when a scheduler is wired (cron endpoints active),
	// runsLimiter MUST be non-nil. The handlers nil-guard the limiter to
	// support test bridging that constructs a partial CronHandlers, but a
	// future server.New refactor that forgets to wire runsLimiter would
	// silently downgrade to unlimited rate. Fail-fast at construction so
	// the regression surfaces during boot rather than under attack.
	//
	// R242-CR-3: same guard for listLimiter — handleList is the cron
	// dashboard's heartbeat and the most attractive enumeration target
	// of the cron HTTP surface, so silent unlimited-rate downgrade is
	// unacceptable.
	if s.scheduler != nil && s.cronH != nil {
		if !s.cronH.HasRunsLimiter() {
			panic("server: runsLimiter must be non-nil when scheduler is wired")
		}
		if !s.cronH.HasListLimiter() {
			panic("server: listLimiter must be non-nil when scheduler is wired")
		}
		// [R247-SEC-2 / R247-SEC-3] writeLimiter gates trigger + preview;
		// silent unlimited-rate downgrade would expose CLI-spawn /
		// IM-notify amplification, so fail-fast at construction.
		if !s.cronH.HasWriteLimiter() {
			panic("server: writeLimiter must be non-nil when scheduler is wired")
		}
	}

	return s
}

// Start registers routes and begins serving.
// listenTCP is the listener factory Start uses to bind its socket. It is a
// package var (defaulting to net.Listen) purely so tests can inject a
// listener whose Accept fails post-bind, exercising the srv.Serve-error
// drain path (R20260531-GO-001) without racing a real socket close.
var listenTCP = net.Listen

func (s *Server) Start(ctx context.Context) error {
	// R030056-GO-002: Start has several early-return error paths (dispatch
	// wireup, platform Start, net.Listen) that run BEFORE the shutdown
	// goroutine — the sole closer of s.shutdownComplete — is spawned. The
	// process-level shutdown sequencer (cmd/naozhi runShutdown) blocks
	// unconditionally on ShutdownComplete() in its server-error path, so any
	// of those early returns would deadlock the whole shutdown. Guard with a
	// defer that closes the channel unless the shutdown goroutine took
	// ownership (shutdownClosed=true). close() must happen exactly once: once
	// the goroutine is spawned it becomes the only closer.
	shutdownClosed := false
	defer func() {
		if !shutdownClosed {
			close(s.shutdownComplete)
		}
	}()
	// Resolver is constructed in buildServer and reused across the
	// dispatch / hub / project-api surfaces. docs/rfc/key-resolver.md
	// Phase 4.
	d, err := dispatch.NewDispatcher(dispatch.DispatcherConfig{
		Router:                s.router,
		Platforms:             s.platforms,
		Agents:                s.agents,
		AgentCommands:         s.agentCommands,
		Scheduler:             s.scheduler,
		ProjectMgr:            s.projectMgr,
		Resolver:              s.resolver,
		Guard:                 s.sessionGuard,
		Queue:                 s.msgQueue,
		Dedup:                 s.dedup,
		AllowedRoot:           s.allowedRoot,
		ClaudeDir:             s.claudeDir,
		Capabilities:          serverCaps{s: s},
		NoOutputTimeout:       s.noOutputTimeout,
		TotalTimeout:          s.totalTimeout,
		WatchdogNoOutputKills: s.watchdog.noOutPtr(),
		WatchdogTotalKills:    s.watchdog.totalPtr(),
		// R20260527122801-CR-6 (#1320): plumb the long-lived service ctx into
		// dispatch so the passthrough send goroutine observes graceful
		// shutdown rather than ignoring SIGTERM until its 5min internal
		// totalTimeout expires. Without this, NewDispatcher falls back to
		// context.Background() which preserves the legacy "never cancels"
		// behaviour but loses the SIGTERM-aware abort.
		StopCtx: ctx,
	})
	if err != nil {
		// R250-ARCH-12: missing Send wireup is a boot-time configuration
		// fault. Surface it through Server.Start's existing error return
		// path so systemd logs the cause and the unit fails fast instead
		// of crashing on first user message.
		return fmt.Errorf("dispatch wireup: %w", err)
	}
	// Expose dispatcher counters via /health. The handler is constructed
	// earlier in New() without a dispatcher reference, so we wire the
	// closure here once the dispatcher exists.
	if s.healthH != nil {
		s.healthH.dispatcherMetrics = d.Metrics
	}
	handler := d.BuildHandler()

	var startedPlatforms []platform.RunnablePlatform
	for _, p := range s.platforms {
		p.RegisterRoutes(s.mux, handler)
		slog.Info("platform registered", "name", p.Name())

		if rp, ok := p.(platform.RunnablePlatform); ok {
			if err := rp.Start(handler); err != nil {
				// Stop already-started platforms to avoid connection leaks.
				// Log individual stop failures; a silent rollback could mask
				// a dangling websocket that holds the process open past the
				// fatal startup error we're about to return.
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

	s.mux.HandleFunc("GET /health", s.healthH.handleHealth)
	// R247-ARCH-1 (#609): K8s-style probe split. /livez is a no-deps "process
	// alive" check; /readyz gates on minimal wiring (router non-nil) without
	// the rich auth-only stats /health surfaces. Orchestrators (K8s, ECS,
	// Nomad, ALB target groups) can now point liveness at /livez and
	// readiness at /readyz without parsing the JSON shape, and a wedged
	// dependency drops the pod from rotation instead of triggering a
	// restart loop.
	s.mux.HandleFunc("GET /livez", s.healthH.handleLivez)
	s.mux.HandleFunc("GET /readyz", s.healthH.handleReadyz)
	// R20260531-GO-001: derive a cancelable child of the caller ctx and use
	// it for every long-lived background loop AND the shutdown goroutine
	// below. The shutdown goroutine is the sole closer of shutdownComplete
	// and blocks on serveCtx.Done(); on a normal SIGTERM the parent ctx
	// cancels (propagating to serveCtx), and on a srv.Serve error the
	// Serve-error path cancels serveCtx directly. Both routes wake the loops
	// (so discoveryCache.Wait() etc. return) and the shutdown goroutine,
	// which then closes shutdownComplete. Without a single cancel source the
	// loops would stay alive on the Serve-error path and the goroutine's
	// discoveryCache.Wait() would block forever, deadlocking any reader of
	// ShutdownComplete().
	serveCtx, serveCancel := context.WithCancel(ctx)
	defer serveCancel()

	s.appCtx = serveCtx
	s.discoveryH.SetAppContext(serveCtx)
	s.registerDashboard()
	s.nodeCache.StartLoop(serveCtx)
	s.discoveryCache.startLoop(serveCtx)
	s.startProjectScanLoop(serveCtx)
	// Warn if we're serving a token-protected dashboard over plaintext with no
	// trusted proxy in front — Bearer tokens and auth cookies would traverse
	// the wire in the clear, subject to passive sniffing on shared networks.
	// `trustedProxy=true` is the operator's explicit statement that TLS
	// termination happens upstream (ALB/CloudFront), in which case this
	// listener binding to plaintext loopback is fine.
	if s.dashboardToken != "" && !s.auth.TrustedProxy && isPlaintextPublicAddr(s.addr) {
		slog.Warn(plaintextDashboardTokenWarning, "addr", s.addr)
	}
	// No-auth mode on a publicly reachable address is the biggest footgun the
	// operator can step into — every /api/* endpoint becomes world-reachable.
	// Decision logic extracted to shouldWarnNoTokenOpen for unit-test coverage;
	// see R60-SEC-006 / R70-SEC-M1 in the helper's docstring.
	if shouldWarnNoTokenOpen(s.dashboardToken, s.addr, s.auth.TrustedProxy) {
		slog.Warn(noTokenOpenWarning,
			"addr", s.addr,
			"trusted_proxy", s.auth.TrustedProxy,
		)
	} else if s.dashboardToken == "" {
		// Loopback + no token is the "local dev" happy path, but if a systemd
		// unit or orchestration layer accidentally clears the token the
		// operator gets no signal that auth is off. Log once at startup so
		// journalctl shows the state regardless of reachability. R23-SEC-M5.
		slog.Warn("dashboard token not configured; all API callers accepted without authentication",
			"addr", s.addr,
		)
	}
	// /ws-node reverse-node channel sends node tokens and session payloads in
	// plaintext when the primary binds to a public HTTP address with no TLS
	// terminator upstream. Passive sniffers on the path can lift the token and
	// impersonate the remote node. Mirror the dashboard token warning so the
	// operator sees the same shape of signal in the startup journal. R176-SEC-MED.
	if shouldWarnReverseNodePlaintext(s.reverseNodeServer != nil, s.auth.TrustedProxy, s.addr) {
		slog.Warn(reverseNodePlaintextWarning,
			"addr", s.addr,
		)
	}
	// R238-SEC-15 (#848): when trustedProxy=true, every per-IP rate limiter,
	// per-IP audit slog field, and same-origin gate decision flows from the
	// last X-Forwarded-For hop. If the upstream proxy does NOT strip
	// client-supplied XFF headers before re-appending its own (or honours
	// arbitrary-depth XFF without a hop-count limit), an attacker can spoof
	// the source IP by sending `X-Forwarded-For: <victim>, <attacker>` —
	// every per-IP gate then attributes the request to the victim's bucket.
	// The fix MUST happen at the proxy (e.g. ALB/CloudFront drop-and-replace,
	// nginx `real_ip_recursive on` with a trusted-proxy allowlist) — naozhi
	// honouring the last XFF hop is by design once trustedProxy is set.
	// Surface a one-shot info-level reminder at startup so an operator
	// who flipped trustedProxy=true on a misconfigured upstream sees the
	// requirement in the boot journal rather than discovering it via a
	// rate-limiter bypass weeks later. Info-level (not Warn) because the
	// configuration itself is legitimate — the warning is about the
	// upstream contract, which we cannot verify from inside the process.
	if s.auth.TrustedProxy {
		slog.Info(trustedProxyXFFReminder, "addr", s.addr)
	}
	// R244-ARCH-16 (#1054): surface the server's effective turn timeouts at
	// startup so an operator can confirm the active values from journalctl
	// without reading config or hitting /health. These two are the only
	// operator-tunable timeouts the Server owns; other package-internal
	// intervals (debounce / poll / TTL) stay greppable in their own files.
	slog.Info("server starting",
		"addr", s.addr,
		"no_output_timeout", s.noOutputTimeout,
		"total_timeout", s.totalTimeout,
	)

	ln, err := listenTCP("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}

	// R247-ARCH-20 / #677: withTraceID wraps the entire mux so every
	// request — authenticated and unauthenticated alike — carries a
	// trace id on its ctx and echoes the same id back in the
	// X-Request-ID response header before any downstream handler runs.
	// Prior to this wiring the middleware existed in
	// traceid_middleware.go but was unreachable (no mux included it),
	// leaving its contract unenforced in production. Outermost order is
	// intentional: gzipMiddleware mutates the response writer; the
	// trace id needs to be observable to gzip's behaviour and to any
	// handler panic that fires before the body is written.
	srv := &http.Server{
		// RNEW-ARCH-401 (#425): withAPIVersionAlias is the innermost wrapper so
		// a `/api/v1/<rest>` request is rewritten to the existing `/api/<rest>`
		// route just before mux matching, while trace-id + gzip still observe
		// the original versioned path the client called.
		Handler:           withTraceID(gzipMiddleware(withAPIVersionAlias(s.mux))),
		ReadHeaderTimeout: 5 * time.Second, // Slowloris defense
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Cap header bytes well below the default 1 MB so an unauthenticated
		// client can't force us to buffer megabyte-sized headers before
		// ReadHeaderTimeout fires. 64 KB is generous for legitimate cookies
		// plus a modest number of X-Forwarded-* headers.
		MaxHeaderBytes: 64 * 1024,
	}

	// Notify caller that the listener is bound and ready to accept connections.
	if s.onReady != nil {
		s.onReady()
	}

	// Periodic flush + prune for the retired-store. 30 s is a balance
	// between "lose at most ~30 s of marks on a SIGKILL" and keeping
	// fsync churn modest under burst close-many sessions UX. Prune
	// drops entries older than 14 days (= 2× the 7-day history window)
	// so the cap pressure described in NewRetiredStoreWithCap rarely
	// matters on a normal operator. Stops via serveCtx.Done in the shutdown
	// goroutine below.
	if s.sessionH != nil && s.sessionH.RetiredStorePresent() {
		go s.runRetiredStoreFlusher(serveCtx)
	}

	// Reuse the channel allocated at construction so ShutdownComplete() (which
	// callers may read before Start runs) and the goroutine below observe the
	// same channel. S11: this is the HTTP-drain barrier the process shutdown
	// sequencer blocks on before router.Shutdown().
	shutdownComplete := s.shutdownComplete
	// The shutdown goroutine is now the sole owner/closer of shutdownComplete;
	// suppress the early-return defer above so close() happens exactly once.
	shutdownClosed = true
	go func() {
		<-serveCtx.Done()
		slog.Info("shutting down server")

		// Shutdown WebSocket hub
		if s.hub != nil {
			s.hub.Shutdown()
		}

		// Stop the scratch-pool sweeper so its ticker goroutine exits before
		// the listener teardown completes. Stop is idempotent and drains in
		// under a second in practice.
		if s.scratchPool != nil {
			s.scratchPool.Stop()
		}

		// Drain any in-flight WarmHistoryCache goroutine before tearing down
		// the rest of the server. Without this wait the background FS scan
		// could write h.historyCache after claudeDir-dependent state is gone.
		// R64-GO-M1.
		if s.sessionH != nil {
			s.sessionH.WaitWarmHistory()
			// Flush the retired-store one final time so the most recent
			// retirement event survives a restart. The periodic flusher
			// below writes every 30s; a clean shutdown that landed
			// between ticks would otherwise lose a few entries.
			s.sessionH.FlushRetiredStore()
		}

		// Wait for the initial discovery refresh goroutine (R218-GO-1).
		// ctx is already cancelled above; Wait ensures the goroutine has
		// exited before projectMgr-dependent state is torn down.
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
		// srv.Shutdown has returned, so no new requests can spawn discovery
		// takeover/close goroutines. Drain any that are still parked in
		// WaitAndCleanup before signalling shutdown complete, so they don't
		// outlive the server goroutine with no WaitGroup tracking them.
		if s.discoveryH != nil {
			s.discoveryH.Wait()
		}
		close(shutdownComplete)
	}()

	err = srv.Serve(ln)
	// If Serve failed for a non-shutdown reason (e.g. accept loop failure),
	// the parent ctx may never be cancelled — the shutdown goroutine would
	// then block forever on serveCtx.Done() and shutdownComplete would never
	// close, deadlocking any caller that reads ShutdownComplete(). Cancel
	// serveCtx here so the goroutine wakes, runs the drain sequence, and
	// closes shutdownComplete; then wait for it before returning the error.
	// R20260531-GO-001.
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		serveCancel()
		<-shutdownComplete
		return err
	}
	// Wait for the shutdown goroutine to finish draining connections.
	select {
	case <-shutdownComplete:
	case <-serveCtx.Done():
		<-shutdownComplete
	}
	return err
}

// ShutdownComplete returns a channel that closes once Start's shutdown
// goroutine has finished draining in-flight HTTP requests (srv.Shutdown
// returned). It is the synchronization barrier S11 (R194-COR) requires:
// the process-level shutdown sequencer in cmd/naozhi cancels the shared
// ctx (which triggers the HTTP drain) and then blocks on this channel
// before calling router.Shutdown(). Without the barrier, router.Shutdown
// races the drain and an in-flight GetOrCreate/Send handler can observe a
// half-cleaned session map. The channel is allocated at construction so a
// caller may obtain it before Start runs; it never closes if Start is
// never invoked. R030056-GO-002: if Start returns an error early (dispatch
// wireup, platform Start, or net.Listen — all before the shutdown goroutine
// is spawned), a defer in Start closes the channel so the process-level
// shutdown sequencer's unconditional receive does not deadlock.
func (s *Server) ShutdownComplete() <-chan struct{} {
	return s.shutdownComplete
}

// retiredStoreFlushInterval is how often runRetiredStoreFlusher writes
// the in-memory retired-store map to disk. 30 s loses at most a single
// burst of retirements on a hard kill while keeping fsync churn modest
// when an operator closes many sessions in a row.
const retiredStoreFlushInterval = 30 * time.Second

// retiredStorePruneInterval governs how often runRetiredStoreFlusher
// drops entries that fall outside the 7-day history window. A 14-day
// cutoff (= 2× the window) avoids racing the dashboard on entries that
// just dropped off the popover; the cap inside RetiredStore.Prune
// handles the pathological "operator closed thousands of sessions in
// the cutoff window" case.
const (
	retiredStorePruneInterval = 6 * time.Hour
	retiredStorePruneCutoff   = 14 * 24 * time.Hour
)

// runRetiredStoreFlusher + startProjectScanLoop + removedProjectNames moved to
// server_loops.go (Phase-3 physical split, ARCH1 / #387).

// plaintextDashboardTokenWarning is the message logged when a token-protected
// dashboard is served over plaintext HTTP with no trusted proxy. R217-SEC-8
// (#602) extends the previous inline literal to spell out the /health attack
// surface explicitly: an authenticated /health response carries workspace_id,
// node status, version, system info, watchdog counters, and (when present)
// dispatch + event-log + attachment-tracker stats. A passive sniffer on the
// wire can therefore lift not only the bearer/cookie but also a deployment
// fingerprint that aids targeting (e.g. node IDs, CLI version) without needing
// to authenticate. Named const (not inline) so tests can pin the exact text
// and a refactor that rewords one occurrence has a single source of truth.
const plaintextDashboardTokenWarning = "dashboard token served over plaintext HTTP with no trusted proxy: " +
	"bearer tokens and session cookies may be sniffed; authenticated /health responses " +
	"also leak workspace_id, node status, version, and watchdog counters in the clear. " +
	"Terminate TLS upstream and set server.trusted_proxy=true, " +
	"or bind to 127.0.0.1 for local-only access."

// noTokenOpenWarning is the message logged when the API accepts any caller
// because dashboard_token is unset on a publicly reachable bind. Exposed as
// a package-level var (not a const literal in the caller) so tests can
// assert the exact text in journal/log output without re-typing it. The
// message intentionally enumerates the concrete risks so an operator
// scrolling a startup log has enough context to act without docs lookup.
const noTokenOpenWarning = "no dashboard_token configured on a non-loopback bind: " +
	"the ENTIRE dashboard API is open to any caller. " +
	"Anyone reaching this port can send messages to sessions, read workspace files under allowed_root, " +
	"alter cron schedules, and trigger transcription. Also: uploadOwner falls back to client IP, " +
	"so users sharing a NAT / LAN / egress gateway can see each other's inline uploads. " +
	"Either set server.dashboard_token, bind to 127.0.0.1 for single-user use, " +
	"or set server.trusted_proxy=true with an upstream that enforces access control."

	// reverseNodePlaintextWarning / trustedProxyXFFReminder consts +
	// shouldWarnReverseNodePlaintext / shouldWarnNoTokenOpen / isPlaintextPublicAddr
	// helpers moved to server_warnings.go (Phase 5-prep, 2026-05-28).
