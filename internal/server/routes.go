package server

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/dashboard/ext/memory"
	"github.com/naozhi/naozhi/internal/dashboard/ext/scratch"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// embed.FS 静态资源 5 变量 + staticAssetETags init + serveStaticWithETag
// helper 抽到 static_assets.go (Phase 5-prep, 2026-05-28).

// Phase 3-prep (server-split-phase4-design.md §6.5 Plan B):
// the JSON-encoder pool, marshalPooled / marshalEscaped, and the
// SetEscapeHTML(false) literal all moved to internal/dashboard/httputil so
// future dashboard sub-packages (discovery / cron / project / auth ...) can
// share them without re-importing internal/server. The thin wrappers below
// keep server-package call sites working unchanged. The R245-SEC-13 contract
// (SetEscapeHTML(false) lives at exactly one site) is now pinned by
// TestSetEscapeHTMLFalseScopedToPackage in the new package, plus the
// in-server TestSetEscapeHTMLFalse_ScopedToWriteJSONHelper scan asserts the
// literal is now ABSENT from internal/server entirely.

// marshalPooled forwards to httputil.MarshalPooled. See that helper for the
// CLIENT-SIDE rendering contract carried over the wire format.
func marshalPooled(v any) ([]byte, error) { return httputil.MarshalPooled(v) }

// marshalEscaped forwards to httputil.MarshalEscaped — the HTML-safe variant
// for payloads spliced into HTML templates / innerHTML render paths.
func marshalEscaped(v any) ([]byte, error) { return httputil.MarshalEscaped(v) }

// writeJSON / writeOK / decodeJSONBody / writeJSONStatus / errEmptyJSONBody
// thin-wrap httputil. The CLIENT-SIDE rendering contract (R71-SEC-L1 /
// R243-SEC-10), the cache-control headers (R58-PERF-001), and the
// DisallowUnknownFields mass-assignment guard (R20260527122801-SEC-5) all
// live in the package godoc on the underlying helper.
func writeJSON(w http.ResponseWriter, v any) { httputil.WriteJSON(w, v) }
func writeOK(w http.ResponseWriter)          { httputil.WriteOK(w) }
func decodeJSONBody(r *http.Request, dst any) error {
	return httputil.DecodeJSONBody(r, dst)
}
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	httputil.WriteJSONStatus(w, status, v)
}

// errEmptyJSONBody is the package-local re-export so existing
// `errors.Is(err, errEmptyJSONBody)` call sites compile unchanged. The actual
// sentinel is httputil.ErrEmptyJSONBody.
var errEmptyJSONBody = httputil.ErrEmptyJSONBody

func (s *Server) registerDashboard() {
	s.hub = NewHub(HubOptions{
		Router:    s.router,
		Agents:    s.agents,
		AgentCmds: s.agentCommands,
		DashToken: s.dashboardToken,
		// R040034-SEC-1 (#1398): pass the live getter rather than a
		// snapshot so a future hot-reload that invokes RotateCookieGen
		// invalidates WS upgrades on the next handshake instead of
		// continuing to accept pre-rotation cookies until restart. The
		// HTTP path already calls auth.CookieMAC() per request.
		CookieMACFn: s.auth.CookieMAC,
		Guard:       s.sessionGuard,
		Queue:       s.msgQueue,
		Nodes:       s.nodes,
		NodesMu:     &s.nodesMu,
		ProjectMgr:  s.projectMgr,
		Resolver:    s.resolver,
		// R176-ARCH-M3 (#431): Scheduler/ScratchPool wired at construction
		// instead of via post-NewHub SetX setters. Both deps are built in
		// Server.New (before registerDashboard), so passing them here removes
		// the SetScheduler/SetScratchPool call-order-vs-Start race window.
		Scheduler:        s.scheduler,
		ScratchPool:      s.scratchPool,
		AllowedRoot:      s.allowedRoot,
		TrustedProxy:     s.auth.TrustedProxy,
		WSAuthLimiter:    s.auth.LoginAllow,
		WSUpgradeLimiter: s.auth.WSUpgradeAllow,
		// R20260527122801-SEC-2 / #1326: forward AuthHandlers so
		// HandleUpgrade can mint nz_anon (and refuse upgrade if mint
		// fails) instead of falling back to clientIP for uploadOwner.
		Auth: s.auth,
		// Forward the application-level ctx so a parent cancel cascades
		// to Hub goroutines even when Shutdown() is not explicitly
		// invoked (CTX1). Zero value in pure-unit tests that bypass
		// Start() is harmless — NewHub falls back to Background().
		ParentCtx: s.appCtx,
	})

	// Route /api/sessions snapshot enrichment through the hub's tailer
	// registry now that both exist. RFC v4 agent-team-ui §3.5.4.
	if s.sessionH != nil {
		s.sessionH.SetSnapshotEnricher(s.hub.enrichSnapshot)
	}

	// R247-ARCH-15 (#650): wire ProjectHandlers' baseCtx now that
	// s.hub.ctx exists. New() constructs projectH before the hub so
	// the prior implementation captured the lookup in a closure; this
	// setter call replaces that closure-as-DI antipattern.
	if s.projectH != nil {
		s.projectH.SetBaseContext(s.hub.ctx)
	}

	// Wire sendH now that hub exists.
	//
	// R215-ARCH-P2-3 (#579): the upload-store cleanup goroutine is an
	// app-lifecycle subsystem, not a Hub-internal one — its lifetime must
	// follow the process, not the Hub's hot-reload boundary. A future Hub
	// hot-reload (drain + replace) would otherwise prematurely cancel the
	// cleanup loop and leak temp-file entries until the next process start.
	// `s.appCtx` is wired by Server.Start before registerDashboard runs (see
	// project_api_basectx_test.go's CTX1 pin); it is canceled only on full
	// process shutdown. Falling back to s.hub.ctx is unsafe (semantics drift)
	// and to context.Background is unsafe (no cancellation at all), so the
	// nil-fallback below is purely defensive against tests that bypass Start.
	uploads := newUploadStore()
	cleanupCtx := s.appCtx
	if cleanupCtx == nil {
		cleanupCtx = s.hub.ctx
	}
	uploads.StartCleanup(cleanupCtx)
	s.hub.SetUploadStore(uploads)
	s.sendH = &SendHandler{
		nodeAccess: s.nodeAccess,
		hub:        s.hub,
		// router: SendRouter consumer-interface view of *session.Router.
		// Closes the R215-ARCH-P1-4 (#566) Phase-2.5 cleanup so the handler
		// no longer reaches its router via h.hub.router.* transits.
		router:        s.hub.router,
		uploadStore:   uploads,
		uploadLimiter: newIPLimiterWithProxy(rate.Every(6*time.Second), 10, s.auth.TrustedProxy), // 10 uploads/min per IP
		sendLimiter:   newIPLimiterWithProxy(rate.Every(2*time.Second), 30, s.auth.TrustedProxy), // 30 sends/min per IP (burst 30)
		auth:          s.auth,
		trustedProxy:  s.auth.TrustedProxy,
	}

	// Scratch (ephemeral aside) API. Pool was constructed in New() and is now
	// wired into the Hub via HubOptions.ScratchPool above (R176-ARCH-M3 #431);
	// here we only start the TTL sweeper and mount the scratch handler.
	if s.scratchPool != nil {
		s.scratchPool.StartSweeper()
		s.scratchH = scratch.New(scratch.Deps{
			Broadcaster: s.hub,
			Router:      s.hub.router,
			Pool:        s.scratchPool,
			OpenLimit:   newIPLimiterWithProxy(rate.Every(12*time.Second), 5, s.auth.TrustedProxy),
			Agents:      s.agents,
		})
	}

	// Push session list changes to WS clients
	s.router.SetOnChange(func() { s.hub.BroadcastSessionsUpdate() })

	// Phase D (RFC §3.5): unified Run lifecycle wiring via
	// runtelemetry.Broadcaster. cron and sysession each register the
	// same hubBroadcaster; subsystem-specific WS payload selection
	// happens inside hubBroadcaster.Broadcast{Started,Ended}.
	//
	// The legacy cron_result frame and its SetOnExecute hook were
	// deleted in this phase — dashboard.js migrated the announce()
	// to the cron_run_ended succeeded branch. Result text, when needed,
	// is fetched via the GET /api/cron/jobs/<id>/runs/<runID> endpoint.
	telemetry := newHubBroadcaster(s.hub)
	if s.scheduler != nil {
		s.scheduler.SetTelemetry(telemetry)
	}
	// #1723 Phase 1: sysession.Manager now produces runtelemetry events
	// directly (SetTelemetry, mirroring cron's seam) instead of the legacy
	// SetCallbacks shim that translated sysession.DaemonRun*Event into
	// runtelemetry events here. The enum maps moved into the sysession
	// package; the wire shape is unchanged — the same hubBroadcaster still
	// selects the daemon_run_* payload off Subsystem=SubsystemSysession.
	if s.sysessionMgr != nil {
		s.sysessionMgr.SetTelemetry(telemetry)
	}

	// Authenticated API routes
	auth := s.auth.RequireAuth
	s.mux.HandleFunc("GET /api/cli/backends", auth(s.cliH.Handle))
	// R237-ARCH-2 (#573) / R260528-ARCH-6 (#1367) incremental slice: the
	// cohesive session-CRUD route group is extracted into its own helper so
	// registerDashboard shrinks toward the routes.go split the issues call
	// for. Behaviour is identical — the routes_snapshot AST gate scans
	// routes.go as a whole, so moving these calls into a same-file helper
	// keeps the golden snapshot byte-for-byte stable.
	s.registerSessionRoutes(auth)
	// R260528-ARCH-6 (#1367) incremental slice: the discovered-session route
	// group (list/preview/takeover/close) extracted into its own same-file
	// helper, continuing the registerDashboard god-function decomposition.
	s.registerDiscoveredRoutes(auth)
	// R237-ARCH-2 (#573) incremental slice: the project route group
	// (*dashproject.Handlers) extracted into its own same-file helper,
	// further shrinking registerDashboard's 60+ HandleFunc body. The
	// /api/planner/stats probe stays inline below because it is a *Server
	// method, not a projectH handler.
	s.registerProjectRoutes(auth)
	// Issue #452 (PLANNER-STATS-1) part-1: process-resource probe so the
	// dashboard can show RSS / goroutine / planner-fan-out trends without
	// reaching the loopback-only /api/debug/vars expvar surface. Per-CLI
	// per-process RSS is the part-2 follow-up; see dashboard_planner_stats.go.
	s.mux.HandleFunc("GET /api/planner/stats", auth(s.handlePlannerStats))
	s.mux.HandleFunc("POST /api/transcribe", auth(s.transcribeH.HandleTranscribe))
	// R260528-ARCH-6 (#1367) incremental slice: the cron route group (CRUD +
	// pause/resume/trigger/preview + run-history) is extracted into its own
	// same-file helper, the exact "拆 cron handlers" step the issue names.
	// Same-file means the routes_snapshot AST gate stays stable.
	s.registerCronRoutes(auth)
	// system-session daemons (docs/rfc/system-session.md §9.2/§9.3)
	s.mux.HandleFunc("GET /api/system/daemons", auth(s.handleSystemDaemons))
	s.mux.HandleFunc("POST /api/system/labels/clear-origin", auth(s.handleClearLabelOrigin))
	s.mux.HandleFunc("POST /api/auth/logout", auth(s.auth.HandleLogout))
	// pprof / expvar debug endpoints: auth-gated + loopback-only AND
	// gated behind server.debug_mode (default false) so a leaked dashboard
	// token cannot enumerate goroutine stacks (which embed file paths +
	// queue contents) or expvar counters at all. Operators flip
	// debug_mode=true via config.yaml only while capturing a profile, then
	// flip it back. R244-SEC-P3-1 [REPEAT-3]. See internal/server/
	// debug_pprof.go + docs/ops/pprof.md for the runbook (operators must
	// also restart with debug_mode=true).
	if s.debugMode {
		s.registerPprof()
		s.registerExpvar()
	}
	// R260528-ARCH-6 (#1367) incremental slice: the scratch-drawer route group
	// extracted into its own same-file helper (which keeps the original
	// nil-guard) to further trim registerDashboard.
	s.registerScratchRoutes(auth)
	// memory link preview (docs/rfc/memory-link-rendering.md): exposes
	// ~/.claude/projects/<scope>/memory/<slug>.md to the dashboard inlineMd
	// renderer so [[slug]] tokens become hover-previewable cards.
	if s.memoryH == nil {
		s.memoryH = memory.New(resolveClaudeProjectsDir(), newIPLimiterWithProxy(memory.MemoryLimiterRate, memory.MemoryLimiterBurst, s.auth.TrustedProxy))
	}
	s.mux.HandleFunc("GET /api/memory/{slug}", auth(s.memoryH.HandleGet))

	// Installed-asset browser (docs/rfc/cc-asset-browser.md): wiring lives in
	// dashboard_ccassets.go to keep this file from growing.
	s.registerAssetBrowserRoutes(auth)

	// Unauthenticated routes (login, static assets, WebSocket with own auth)
	s.mux.HandleFunc("POST /api/auth/login", s.auth.HandleLogin)
	// R243-SEC-15 (#800): explicit no-JS form-action target. The login page
	// posts JSON via fetch() when JavaScript is enabled; this handler exists
	// so a JS-disabled browser's submit lands in a controlled drain-and-
	// discard path instead of a raw "POST /dashboard" that would (a) get
	// 405 from the GET-only mux entry above and (b) ship the form-encoded
	// token through any logging middleware that reads request bodies.
	s.mux.HandleFunc("POST /api/auth/noscript", s.auth.HandleLoginNoScript)
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /manifest.json", handleManifest)
	s.mux.HandleFunc("GET /sw.js", handleSW)
	// R20260527122801-SEC-4 (#1328): the dashboard JS modules embed the
	// list of authenticated API endpoints, the cron polling cadence, and
	// the dashboard's client-side schema. Serving them to unauthenticated
	// scanners gave a free recon surface — pull /static/dashboard.js,
	// grep for `/api/`, fingerprint the deployment. Now gated behind
	// requireAuth so only authenticated dashboard users (or no-token-mode
	// deployments where requireAuth is a pass-through) can fetch them.
	// The login page itself loads no JS from /static/, so wrapping these
	// does not break the unauthenticated bootstrap.
	s.mux.HandleFunc("GET /static/dashboard.js", auth(handleDashboardJS))
	s.mux.HandleFunc("GET /static/agent_view.js", auth(handleAgentViewJS))
	s.mux.HandleFunc("GET /static/asset_browser.js", auth(handleAssetBrowserJS))
	s.mux.HandleFunc("GET /ws", s.hub.HandleUpgrade)
	if s.reverseNodeServer != nil {
		s.mux.Handle("GET /ws-node", s.reverseNodeServer)
	}
}

// registerSessionRoutes wires the session-CRUD route group (list / events /
// agent_events / tool_result / send / upload / attachment / delete / resume /
// interrupt / label). Split out of registerDashboard as the first concrete
// slice of the R237-ARCH-2 (#573) god-function decomposition. `auth` is the
// RequireAuth wrapper captured by the caller so every route here stays
// authenticated. Behaviour is byte-identical to the inline block it replaced.
func (s *Server) registerSessionRoutes(auth func(http.HandlerFunc) http.HandlerFunc) {
	s.mux.HandleFunc("GET /api/sessions", auth(s.sessionH.HandleList))
	s.mux.HandleFunc("GET /api/sessions/events", auth(s.sessionH.HandleEvents))
	s.mux.HandleFunc("GET /api/sessions/agent_events", auth(s.agentEventsH.HandleAgentEvents))
	s.mux.HandleFunc("GET /api/sessions/tool_result", auth(s.agentEventsH.HandleToolResult))
	s.mux.HandleFunc("POST /api/sessions/send", auth(s.sendH.handleSend))
	s.mux.HandleFunc("POST /api/sessions/upload", auth(s.sendH.handleUpload))
	s.mux.HandleFunc("GET /api/sessions/attachment", auth(s.sendH.handleAttachment))
	s.mux.HandleFunc("DELETE /api/sessions", auth(s.sessionH.HandleDelete))
	s.mux.HandleFunc("POST /api/sessions/resume", auth(s.sessionH.HandleResume))
	s.mux.HandleFunc("POST /api/sessions/interrupt", auth(s.sessionH.HandleInterrupt))
	s.mux.HandleFunc("PATCH /api/sessions/label", auth(s.sessionH.HandleSetLabel))
}

// registerScratchRoutes wires the scratch-drawer route group (open / promote /
// delete) when the scratch handler is configured. Extracted from
// registerDashboard as a further slice of the R260528-ARCH-6 (#1367)
// god-function decomposition. The nil-guard is preserved here verbatim so
// deployments without a scratch pool register no scratch routes, exactly as
// before.
func (s *Server) registerScratchRoutes(auth func(http.HandlerFunc) http.HandlerFunc) {
	if s.scratchH == nil {
		return
	}
	s.mux.HandleFunc("POST /api/scratch/open", auth(s.scratchH.HandleOpen))
	s.mux.HandleFunc("POST /api/scratch/{id}/promote", auth(s.scratchH.HandlePromote))
	s.mux.HandleFunc("DELETE /api/scratch/{id}", auth(s.scratchH.HandleDelete))
}

// registerProjectRoutes wires the project route group (list / config get+put /
// planner restart / favorite toggle / files-exists / file get). Extracted from
// registerDashboard as a further slice of the R237-ARCH-2 (#573) god-function
// decomposition. All handlers are *dashproject.Handlers methods; the
// /api/planner/stats probe is deliberately left inline at the call site
// because it is a *Server method, keeping this helper a single-owner group.
// http.ServeMux matches by distinct path, so consolidating the registrations
// here (the favorite/files routes previously sat after planner/stats) does not
// change routing behaviour.
func (s *Server) registerProjectRoutes(auth func(http.HandlerFunc) http.HandlerFunc) {
	s.mux.HandleFunc("GET /api/projects", auth(s.projectH.HandleList))
	s.mux.HandleFunc("GET /api/projects/config", auth(s.projectH.HandleConfigGet))
	s.mux.HandleFunc("PUT /api/projects/config", auth(s.projectH.HandleConfigPut))
	s.mux.HandleFunc("POST /api/projects/planner/restart", auth(s.projectH.HandlePlannerRestart))
	s.mux.HandleFunc("POST /api/projects/favorite", auth(s.projectH.HandleFavoriteToggle))
	s.mux.HandleFunc("POST /api/projects/files/exists", auth(s.projectH.HandleFilesExists))
	s.mux.HandleFunc("GET /api/projects/file", auth(s.projectH.HandleFileGet))
}

// registerDiscoveredRoutes wires the discovered-session route group
// (list / preview / takeover / close). Extracted from registerDashboard as a
// further slice of the R260528-ARCH-6 (#1367) god-package decomposition.
// `auth` is the caller's RequireAuth wrapper; behaviour is byte-identical to
// the inline block it replaced.
func (s *Server) registerDiscoveredRoutes(auth func(http.HandlerFunc) http.HandlerFunc) {
	s.mux.HandleFunc("GET /api/discovered", auth(s.discoveryH.HandleList))
	s.mux.HandleFunc("GET /api/discovered/preview", auth(s.discoveryH.HandlePreview))
	s.mux.HandleFunc("POST /api/discovered/takeover", auth(s.discoveryH.HandleTakeover))
	s.mux.HandleFunc("POST /api/discovered/close", auth(s.discoveryH.HandleClose))
}

// registerCronRoutes wires the cron route group (CRUD + pause/resume/trigger/
// preview + run-history + transcript). Split out of registerDashboard as the
// first concrete slice of the R260528-ARCH-6 (#1367) "拆 cron handlers" step.
// `auth` is the caller's RequireAuth wrapper. Behaviour is byte-identical to
// the inline block it replaced.
func (s *Server) registerCronRoutes(auth func(http.HandlerFunc) http.HandlerFunc) {
	s.mux.HandleFunc("GET /api/cron", auth(s.cronH.HandleList))
	s.mux.HandleFunc("POST /api/cron", auth(s.cronH.HandleCreate))
	s.mux.HandleFunc("PATCH /api/cron", auth(s.cronH.HandleUpdate))
	s.mux.HandleFunc("DELETE /api/cron", auth(s.cronH.HandleDelete))
	s.mux.HandleFunc("POST /api/cron/pause", auth(s.cronH.HandlePause))
	s.mux.HandleFunc("POST /api/cron/resume", auth(s.cronH.HandleResume))
	s.mux.HandleFunc("POST /api/cron/trigger", auth(s.cronH.HandleTrigger))
	s.mux.HandleFunc("GET /api/cron/preview", auth(s.cronH.HandlePreview))
	// P1 cron-run-history: per-job execution history.
	s.mux.HandleFunc("GET /api/cron/runs", auth(s.cronH.HandleRunsList))
	s.mux.HandleFunc("GET /api/cron/runs/{run_id}", auth(s.cronH.HandleRunDetail))
	// cron-dashboard-redesign P2a §4.4.3 — transcript endpoint. Path
	// param mirrors handleRunDetail; same per-IP rate limit applies.
	s.mux.HandleFunc("GET /api/cron/runs/{run_id}/transcript", auth(s.cronH.HandleRunTranscript))
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if s.dashboardToken != "" && !s.auth.IsAuthenticated(r) {
		// R230C-SEC-12: rate-limit unauthenticated GETs so a scanner cannot
		// repeatedly hammer the login template renderer (CSP+HTML+cookie
		// crypto path) and fingerprint deployments. Same 60/min×20 burst
		// budget as wsUpgradeLimiter accommodates real users (tab-reload,
		// mobile-wake, multiple browser windows) while limiting sustained
		// abuse. Authenticated users are unaffected.
		if !s.auth.UnauthDashAllow(clientIP(r, s.auth.TrustedProxy)) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		s.auth.ServeLoginPage(w, r)
		return
	}
	if staticAssetBytes("dashboard.html") == nil {
		http.Error(w, "dashboard not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	// connect-src 只保留 'self'：同源页面发起的 ws:// 与 wss:// 已由 'self'
	// 隐式覆盖（浏览器按页面 scheme 自动选）。显式写 `ws: wss:` 会放宽到
	// **任何**跨源 WebSocket 端点，为潜在 XSS/XS-Leak 外泄数据留口。
	//
	// frame-src 必须显式列 `blob:`：dashboard 工作区 .html / .svg 文件预览
	// (renderSandboxedBlob in dashboard.js) 走 fetch → Blob({type:...}) →
	// iframe.src = blob:URL 的链路（type 为 text/html 或 image/svg+xml）。
	// CSP 'self' 不匹配 `blob:` scheme，未列就会 fallback 到 default-src
	// 'self' 然后被拦掉，预览框空白。`blob:` 在 frame-src 上下文里仍是
	// opaque origin + iframe sandbox=""（零权限），三层防御
	// （serveRender octet-stream attachment / blob opaque origin /
	// sandbox 空）都未变化，安全契约保持。
	// R236-SEC-2: frame-ancestors 'none' 与 X-Frame-Options: DENY 双重防御 clickjacking；
	// 现代浏览器优先 CSP frame-ancestors，X-Frame-Options 仅作 fallback。
	//
	// R247-SEC-23 [REPEAT-3, ties to R243-SEC-4 / R246-SEC-10]: font-src
	// `https://cdn.jsdelivr.net` is required because KaTeX's CSS @font-face
	// rules pull math woff2 files from the same CDN. Subresource Integrity
	// (SRI) is *not* enforceable on @font-face fonts — neither the CSS
	// `unicode-range`/`src` syntax nor any CSP directive supports an
	// integrity hash on font assets, and the once-proposed CSP3
	// `require-sri-for font` directive was withdrawn before browser
	// implementation. Defence in depth has three layers today:
	//   (1) the KaTeX *CSS* itself is loaded with `integrity=sha384-…`
	//       (dashboard.js loadKatex), so a CDN-substituted stylesheet
	//       cannot redirect @font-face to attacker-controlled origins;
	//       a tampered woff2 byte-stream still reaches a font parser
	//       that runs in a sandbox and cannot execute script;
	//   (2) Permissions-Policy strips camera/microphone/geolocation/payment
	//       so a font-parser-RCE in the browser still cannot reach the
	//       hardware/credential surface most attackers want;
	//   (3) script-src does NOT include `cdn.jsdelivr.net` for non-SRI
	//       loads — every CDN script is pinned by integrity in JS.
	// NEEDS-DESIGN: full mitigation requires `//go:embed` of the ~6 MB
	// KaTeX woff2 bundle (~30 files spanning Math/Main/Caligraphic/Fraktur
	// /SansSerif/Script/Size/Typewriter variants × Regular/Bold/Italic
	// faces) and a CSS rewriter that strips the `https://cdn.jsdelivr.net`
	// prefix from KaTeX's bundled `katex.min.css`. That work is tracked
	// for a separate change so this CSP comment stays the authoritative
	// pointer to "why we still trust jsdelivr for fonts" until it lands.
	// `require-sri-for font` is included as a no-op forward-compatibility
	// hook: every shipping browser ignores the directive today, but if
	// any vendor revives the proposal we get integrity enforcement for
	// free without another CSP edit. R247-SEC-23 (#518) closes on this
	// pin alone; vendoring is the long-term mitigation tracked above as
	// NEEDS-DESIGN. The TestDashboardCSP_KatexFontSRIForwardCompat
	// regression test asserts the `font` token never gets dropped from
	// require-sri-for during a future CSP refactor.
	//
	// R243-SEC-4 / R244-SEC-P2-4 [REPEAT-3]: extend the same forward-compat
	// hook to `script` and `style` tokens. Today's CDN-loaded resources
	// (mermaid script, KaTeX script + stylesheet) ALL carry an `integrity`
	// attribute so the directive is currently a no-op for naozhi —
	// dashboard.js loadKatex / loadMermaid are the only call paths that
	// inject CDN <script>/<link> tags. If a future contributor adds a CDN
	// asset without `integrity=` and any browser later revives the spec,
	// the policy will fail-closed instead of silently shipping unsigned
	// CDN code. Vendoring the assets via //go:embed remains the proper
	// long-term mitigation; this is the cheap "no regression" gate while
	// that work is queued (see R247-SEC-23 NEEDS-DESIGN comment above).
	//
	// R236-SEC-14 (#562) audit: `img-src ... data:` is intentional and
	// kept. The dropdown-arrow CSS in `static/dashboard.html`
	// (`.cron-sort-select`, `.freq-mode-select`, `.freq-extra`) ships
	// inline `data:image/svg+xml` URIs as `background-image`, which the
	// CSP spec routes through `img-src` even though the syntactic context
	// is CSS. Stripping `data:` therefore breaks the cron / scheduler UI
	// dropdowns. The pre-condition of the original SEC-14 concern —
	// pairing with `'unsafe-inline'` to enable a data: exfil channel —
	// requires an attacker-injected `<img src="data:...">` element; the
	// audit (`grep -nE '<img\s[^>]*src="data:' static/`) finds zero such
	// occurrences in the shipped HTML, and the regression test
	// `TestDashboardCSP_DataImgAuditPinned` pins that absence so the
	// actual exfil precondition cannot regress. Tightening to
	// `'self' blob:` must be bundled with (a) replacing those CSS
	// data-URIs with embedded SVG files served from /static and
	// (b) eliminating `script-src 'unsafe-inline'` per R236-SEC-02 /
	// #441 — see triage note on #562 for the bundled work.
	// R242-SEC-1 / R249-SEC-9 (#605, #922) interim hardening: while the full
	// strict-dynamic+nonce migration that lets us drop `'unsafe-inline'` from
	// script-src is tracked NEEDS-DESIGN, three additive directives close real
	// attack surface today without touching the inline-handler surface:
	//   - `object-src 'none'`: explicitly forbids <object>/<embed>/<applet>
	//     plugin embeds (a legacy script-execution vector that default-src
	//     'self' would still permit for same-origin sources). The dashboard
	//     ships zero such elements, so 'none' is the strict, recommended lock.
	//   - `base-uri 'none'`: a single injected `<base href>` would otherwise
	//     re-root every relative URL (including the `/static/dashboard.js`
	//     and `/static/agent_view.js` script tags) at an attacker origin,
	//     turning any HTML-injection into script substitution even with the
	//     existing script-src allowlist. We never use a <base> element, so
	//     'none' is safe.
	//   - `form-action 'self'`: the dashboard's only <form> (quick-ask) does
	//     `event.preventDefault()` and submits via fetch, so it never POSTs
	//     to an action target; pinning to 'self' stops an injected form from
	//     exfiltrating to a foreign origin. Mirrors the login page's
	//     explicit form-action discipline (R243-SEC-15 / #800).
	// R242-SEC-2 (#607): narrow every `cdn.jsdelivr.net` source expression to
	// the `/npm/` path prefix. A CSP host-source with a trailing-slash path
	// matches only URLs whose path starts with that prefix (CSP3 §6.6.2.6
	// path-part matching), so the dashboard can still pull
	// `…/npm/mermaid@…`, `…/npm/katex@…/dist/katex.min.js`, the KaTeX
	// stylesheet, and its `@font-face` woff2 files (all under `/npm/`), while
	// the CDN scope can no longer bootstrap an arbitrary follow-on load from a
	// non-`/npm/` jsdelivr path (e.g. `/gh/<attacker>/<repo>` user content or
	// `/combine/` bundle endpoints). This shrinks the script-src CDN surface
	// without the breaking strict-dynamic+nonce migration that drops
	// `'unsafe-inline'`, which stays NEEDS-DESIGN (it is mutually exclusive
	// with the dashboard's inline-handler reliance — browsers ignore
	// `'unsafe-inline'` once `strict-dynamic` is present). require-sri-for
	// script continues to pin every CDN script to its integrity hash.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net/npm/; connect-src 'self'; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net/npm/; font-src 'self' https://cdn.jsdelivr.net/npm/; img-src 'self' data: blob:; frame-src 'self' blob:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; require-sri-for script style font")
	// HSTS is only meaningful over TLS (RFC 6797 §7.2). Sending it on plain
	// HTTP would still be honoured by browsers and can brick local HTTP
	// loopback access for a year. Gate on the same isSecure() helper the
	// auth cookie Secure flag uses, so behaviour is consistent.
	if s.auth.IsSecure(r) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	// Permissions-Policy: block camera/microphone/geolocation/payment API
	// access outright. Embedded CDN scripts (mermaid, KaTeX) are SRI-pinned
	// but defence in depth — if the CDN is ever compromised, the hostile
	// replacement still cannot silently invoke getUserMedia etc.
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
	// COOP isolates the dashboard browsing context from cross-origin popups so
	// `window.opener` leaks (Spectre / XS-Leak) cannot reach naozhi state.
	// CORP blocks other origins from embedding this HTML via <img>/<script>/
	// <iframe> no-cors fetches — complements the existing X-Frame-Options.
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if serveStaticWithETag(w, r, "dashboard.html") {
		return
	}
	writeStaticAssetBody(w, r, "dashboard.html")
}

// handleManifest / handleSW / handleDashboardJS / handleAgentViewJS serve
// the dashboard's static asset bundle (PWA manifest, service worker,
// dashboard SPA, agent-view module). All four are pure embed.FS readers
// with no Server-instance state, so they live as package-level functions
// rather than *Server methods. This drops 4 entries from the
// handle_baseline lint allow-list (8 → 4) without changing routing
// behaviour. R-static-handlers (2026-05-28).
func handleManifest(w http.ResponseWriter, r *http.Request) {
	data := staticAssetBytes("manifest.json")
	if data == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "max-age=3600")
	if serveStaticWithETag(w, r, "manifest.json") {
		return
	}
	if _, err := w.Write(data); err != nil {
		slog.Debug("manifest write", "err", err)
	}
}

func handleSW(w http.ResponseWriter, r *http.Request) {
	data := staticAssetBytes("sw.js")
	if data == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	// R20260602190132-SEC-2 (#1603): dropped the explicit
	// `Service-Worker-Allowed: /` grant. The header only ever *broadens* the
	// max SW scope above the script's own directory; since /sw.js already
	// lives at root its default scope is "/" with or without the header, so
	// the header was a redundant explicit root-scope grant that scanners
	// could read as a registration hint. dashboard.js calls
	// `navigator.serviceWorker.register('/sw.js')` with no scope option, so
	// the default "/" scope (the script's location) still applies — anonymous
	// PWA installability is unaffected.
	//
	// #1771: sw.js is served no-cache, so browsers re-request it on every SW
	// update check. Attaching the ETag lets those re-checks 304 (empty body)
	// instead of re-downloading the full script each time.
	if serveStaticWithETag(w, r, "sw.js") {
		return
	}
	if _, err := w.Write(data); err != nil {
		slog.Debug("sw write", "err", err)
	}
}

func handleDashboardJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("dashboard.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "dashboard.js") {
		return
	}
	writeStaticAssetBody(w, r, "dashboard.js")
}

// handleAgentViewJS serves static/agent_view.js — the RFC v4 agent-team-ui
// dashboard module. Mirrors handleDashboardJS for caching/CSP headers so
// the two scripts behave identically in the browser cache.
func handleAgentViewJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("agent_view.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "agent_view.js") {
		return
	}
	writeStaticAssetBody(w, r, "agent_view.js")
}

// handleAssetBrowserJS serves static/asset_browser.js — the cc-asset-browser
// dashboard module (RFC docs/rfc/cc-asset-browser.md). Mirrors handleAgentViewJS.
func handleAssetBrowserJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("asset_browser.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "asset_browser.js") {
		return
	}
	writeStaticAssetBody(w, r, "asset_browser.js")
}

// buildSessionOpts resolves agent config and planner overrides for a
// session key. When resolver is non-nil, delegates to ResolveForKey for
// the planner branch (preserving the "do not read defaults" contract of
// planner-restart semantics) and for IM-4-segment keys. Falls back to
// the legacy inlined merge otherwise, e.g. headless/test constructions
// that wire no resolver.
//
// Behaviour parity with the legacy path:
//   - IM 4-segment key → opts = agents[agentID]; Workspace NOT overlaid
//     (resume path: workspace comes from sessions.json, not fresh chat)
//   - planner key + project exists → Resolver's planner-view opts
//     (Exempt=true + Workspace + Model + --append-system-prompt)
//   - planner key + project missing → ResolveForKey returns ok=false;
//     we fall back to legacy "opts.Exempt=true, agent defaults only"
//     so the session still spawns (planner without project config is a
//     degenerate but recoverable state — e.g. project deleted between
//     sessions.json save and dashboard resume)
func buildSessionOpts(key string, resolver *session.KeyResolver, agents map[string]session.AgentOpts, projectMgr *project.Manager) session.AgentOpts {
	if resolver != nil {
		if opts, ok := resolver.ResolveForKey(key); ok {
			return opts
		}
		// ok=false for planner with missing project, or scratch/cron/
		// malformed keys. Fall through to the legacy inline merge so
		// we stay lenient — dashboard resume must never fail hard on
		// a stale key.
	}

	parts := strings.SplitN(key, ":", 4)
	agentID := "general"
	if len(parts) == 4 {
		agentID = parts[3]
	}

	opts := agents[agentID]
	if project.IsPlannerKey(key) {
		opts.Exempt = true // planner sessions are always exempt, regardless of project config
		// R20260531-QUAL-4: extract the project name by stripping the fixed
		// "project:" prefix and ":planner" suffix — the exact inverse of
		// PlannerKeyFor. SplitN(key, ":", 4)[1] would truncate a name that
		// itself contains ':' (dir "my:proj" → "my"), silently breaking
		// projectMgr.Get and leaving planner opts unconfigured.
		name := strings.TrimSuffix(strings.TrimPrefix(key, "project:"), ":planner")
		if projectMgr != nil {
			if p := projectMgr.Get(name); p != nil {
				opts.Workspace = p.Path
				if m := projectMgr.EffectivePlannerModel(p); m != "" {
					opts.Model = m
				}
				if prompt := projectMgr.EffectivePlannerPrompt(p); prompt != "" {
					opts.ExtraArgs = append(opts.ExtraArgs[:len(opts.ExtraArgs):len(opts.ExtraArgs)],
						"--append-system-prompt", prompt)
				}
			}
		}
	}
	return opts
}
