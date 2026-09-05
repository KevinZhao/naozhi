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

// JSON helpers thin-wrap internal/dashboard/httputil so dashboard
// sub-packages share one encoder pool; the single SetEscapeHTML(false) site
// lives there and must not be duplicated in this package.

// marshalPooled forwards to httputil.MarshalPooled (client-side rendering
// contract documented there).
func marshalPooled(v any) ([]byte, error) { return httputil.MarshalPooled(v) }

// marshalEscaped forwards to httputil.MarshalEscaped — the HTML-safe variant
// for payloads spliced into HTML templates / innerHTML render paths.
func marshalEscaped(v any) ([]byte, error) { return httputil.MarshalEscaped(v) }

// writeJSON / writeOK / decodeJSONBody / writeJSONStatus thin-wrap httputil;
// rendering contract, cache-control headers and the DisallowUnknownFields
// mass-assignment guard are documented on the underlying helpers.
func writeJSON(w http.ResponseWriter, v any) { httputil.WriteJSON(w, v) }
func writeOK(w http.ResponseWriter)          { httputil.WriteOK(w) }
func decodeJSONBody(r *http.Request, dst any) error {
	return httputil.DecodeJSONBody(r, dst)
}
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	httputil.WriteJSONStatus(w, status, v)
}

// errEmptyJSONBody re-exports httputil.ErrEmptyJSONBody for errors.Is call sites.
var errEmptyJSONBody = httputil.ErrEmptyJSONBody

func (s *Server) registerDashboard() {
	s.hub = NewHub(HubOptions{
		Router:    s.router,
		Agents:    s.agents,
		AgentCmds: s.agentCommands,
		DashToken: s.dashboardToken,
		// Live getter, not a snapshot: RotateCookieGen must invalidate WS
		// upgrades on the next handshake (#1398).
		CookieMACFn: s.auth.CookieMAC,
		Guard:       s.sessionGuard,
		Queue:       s.msgQueue,
		Nodes:       s.nodes,
		ProjectMgr:  s.projectMgr,
		Resolver:    s.resolver,
		// Wired at construction (both built in New) so there is no
		// setter-vs-Start ordering window (#431).
		Scheduler:        s.scheduler,
		ScratchPool:      s.scratchPool,
		AllowedRoot:      s.allowedRoot,
		TrustedProxy:     s.auth.TrustedProxy,
		WSAuthLimiter:    s.auth.LoginAllow,
		WSUpgradeLimiter: s.auth.WSUpgradeAllow,
		// HandleUpgrade mints nz_anon for uploadOwner and refuses the
		// upgrade if minting fails; never falls back to clientIP (#1326).
		Auth: s.auth,
		// Parent cancel cascades to Hub goroutines even without Shutdown();
		// nil (tests bypassing Start) falls back to Background in NewHub.
		ParentCtx: s.appCtx,
	})

	// /api/sessions snapshot enrichment goes through the hub's tailer registry.
	if s.sessionH != nil {
		s.sessionH.SetSnapshotEnricher(s.hub.enrichSnapshot)
	}

	// projectH is constructed before the hub in New(); its base ctx is set here.
	if s.projectH != nil {
		s.projectH.SetBaseContext(s.hub.ctx)
	}

	// The upload-store cleanup loop is process-lifetime (appCtx), not
	// Hub-lifetime: a Hub hot-reload must not cancel it and leak temp files.
	// The hub.ctx fallback only covers tests that bypass Start (#579).
	uploads := newUploadStore()
	cleanupCtx := s.appCtx
	if cleanupCtx == nil {
		cleanupCtx = s.hub.ctx
	}
	uploads.StartCleanup(cleanupCtx)
	s.hub.SetUploadStore(uploads)
	s.sendH = &SendHandler{
		nodeAccess: s.nodes,
		hub:        s.hub,
		// SendRouter consumer view; the handler never goes via h.hub.router.*.
		router:        s.hub.router,
		uploadStore:   uploads,
		uploadLimiter: newIPLimiterWithProxy(rate.Every(6*time.Second), 10, s.auth.TrustedProxy), // 10 uploads/min per IP
		sendLimiter:   newIPLimiterWithProxy(rate.Every(2*time.Second), 30, s.auth.TrustedProxy), // 30 sends/min per IP (burst 30)
		auth:          s.auth,
		trustedProxy:  s.auth.TrustedProxy,
		orient:        s.orient,
	}

	// Scratch (ephemeral aside) API: pool built in New(); start sweeper + mount.
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

	// cron and sysession share one runtelemetry.Broadcaster; per-subsystem
	// WS payload selection happens inside hubBroadcaster.
	telemetry := newHubBroadcaster(s.hub)
	if s.scheduler != nil {
		s.scheduler.SetTelemetry(telemetry)
	}
	if s.sysessionMgr != nil {
		s.sysessionMgr.SetTelemetry(telemetry)
	}

	// Authenticated API routes
	auth := s.auth.RequireAuth
	s.mux.HandleFunc("GET /api/cli/backends", auth(s.cliH.Handle))
	// Access profiles: list returns only non-sensitive fields (never env
	// values or tokens); create is disabled (400) when ConfigPath is unset.
	s.mux.HandleFunc("GET /api/access-profiles", auth(s.accessProfilesH.HandleList))
	s.mux.HandleFunc("POST /api/access-profiles", auth(s.accessProfilesH.HandleCreate))
	// Route groups live in same-file helpers so the routes_snapshot AST gate
	// (which scans routes.go as a whole) stays stable.
	s.registerSessionRoutes(auth)
	s.registerDiscoveredRoutes(auth)
	s.registerProjectRoutes(auth)
	// Process-resource probe (RSS / goroutines / planner fan-out) that does not
	// require the loopback-only expvar surface.
	s.mux.HandleFunc("GET /api/planner/stats", auth(s.plannerH.HandleStats))
	s.mux.HandleFunc("POST /api/transcribe", auth(s.transcribeH.HandleTranscribe))
	s.registerCronRoutes(auth)
	// system-session daemons (docs/rfc/system-session.md §9.2/§9.3)
	s.mux.HandleFunc("GET /api/system/daemons", auth(s.systemH.HandleDaemons))
	s.mux.HandleFunc("POST /api/system/labels/clear-origin", auth(s.systemH.HandleClearLabelOrigin))
	// self-update (docs/rfc/dashboard-update-notice.md)
	s.mux.HandleFunc("GET /api/system/update", auth(s.systemH.HandleUpdateStatus))
	s.mux.HandleFunc("POST /api/system/update/apply", auth(s.systemH.HandleUpdateApply))
	// instance-wide UI preferences, persisted server-side (dashboard/ext/uisettings)
	s.mux.HandleFunc("GET /api/settings", auth(s.uiSettingsH.HandleGet))
	s.mux.HandleFunc("PUT /api/settings", auth(s.uiSettingsH.HandlePut))
	s.mux.HandleFunc("POST /api/auth/logout", auth(s.auth.HandleLogout))
	// pprof / expvar are auth-gated + loopback-only AND require debug_mode so a
	// leaked dashboard token cannot enumerate goroutine stacks or counters.
	// Runbook: docs/ops/pprof.md.
	if s.debugMode {
		s.registerPprof()
		s.registerExpvar()
	}
	s.registerScratchRoutes(auth)
	// memory link preview (docs/rfc/memory-link-rendering.md): serves
	// ~/.claude/projects/<scope>/memory/<slug>.md for [[slug]] hover cards.
	if s.memoryH == nil {
		s.memoryH = memory.New(resolveClaudeProjectsDir(), newIPLimiterWithProxy(memory.MemoryLimiterRate, memory.MemoryLimiterBurst, s.auth.TrustedProxy))
	}
	s.mux.HandleFunc("GET /api/memory/{slug}", auth(s.memoryH.HandleGet))

	// Installed-asset browser (docs/rfc/cc-asset-browser.md).
	s.registerAssetBrowserRoutes(auth)

	// Unauthenticated routes (login, static assets, WebSocket with own auth)
	s.mux.HandleFunc("POST /api/auth/login", s.auth.HandleLogin)
	// No-JS form-action target: a JS-disabled login submit lands in a
	// controlled drain-and-discard path instead of a raw POST /dashboard that
	// would 405 and ship the form-encoded token through body-reading
	// middleware (#800).
	s.mux.HandleFunc("POST /api/auth/noscript", s.auth.HandleLoginNoScript)
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /manifest.json", handleManifest)
	s.mux.HandleFunc("GET /sw.js", handleSW)
	// Favicon is unauthenticated so it resolves on the login page.
	s.mux.HandleFunc("GET /favicon.ico", handleFavicon)
	s.mux.HandleFunc("GET /favicon.svg", handleFavicon)
	// Dashboard JS is auth-gated: it embeds the API endpoint list and client
	// schema, a free recon surface for unauthenticated scanners. The login
	// page loads no JS from /static/ so the bootstrap is unaffected (#1328).
	s.mux.HandleFunc("GET /static/contract.js", auth(handleContractJS))
	s.mux.HandleFunc("GET /static/nz_util.js", auth(handleNzUtilJS))
	s.mux.HandleFunc("GET /static/dashboard.js", auth(handleDashboardJS))
	s.mux.HandleFunc("GET /static/cron_view.js", auth(handleCronViewJS))
	s.mux.HandleFunc("GET /static/agent_view.js", auth(handleAgentViewJS))
	s.mux.HandleFunc("GET /static/asset_browser.js", auth(handleAssetBrowserJS))
	s.mux.HandleFunc("GET /static/files_view.js", auth(handleFilesViewJS))
	s.mux.HandleFunc("GET /ws", s.hub.HandleUpgrade)
	if s.reverseNodeServer != nil {
		s.mux.Handle("GET /ws-node", s.reverseNodeServer)
	}
}

// registerSessionRoutes wires the session-CRUD route group. `auth` is the
// caller's RequireAuth wrapper so every route here stays authenticated.
func (s *Server) registerSessionRoutes(auth func(http.HandlerFunc) http.HandlerFunc) {
	s.mux.HandleFunc("GET /api/sessions", auth(s.sessionH.HandleList))
	s.mux.HandleFunc("GET /api/sessions/events", auth(s.sessionH.HandleEvents))
	s.mux.HandleFunc("GET /api/sessions/runs", auth(s.sessionH.HandleRuns))
	// Cost ledger read API (docs/rfc/cost-ledger.md §7); unit-bucketed, rate limited.
	s.mux.HandleFunc("GET /api/cost/summary", auth(s.costH.HandleSummary))
	s.mux.HandleFunc("GET /api/cost/entries", auth(s.costH.HandleEntries))
	s.mux.HandleFunc("GET /api/sessions/git", auth(s.sessionH.HandleGit))
	s.mux.HandleFunc("GET /api/sessions/agent_events", auth(s.agentEventsH.HandleAgentEvents))
	s.mux.HandleFunc("GET /api/sessions/tool_result", auth(s.agentEventsH.HandleToolResult))
	s.mux.HandleFunc("POST /api/sessions/send", auth(s.sendH.handleSend))
	s.mux.HandleFunc("POST /api/sessions/bind", auth(s.sendH.handleBind))
	s.mux.HandleFunc("POST /api/sessions/upload", auth(s.sendH.handleUpload))
	s.mux.HandleFunc("POST /api/sessions/orient", auth(s.sendH.handleOrient))
	s.mux.HandleFunc("GET /api/sessions/attachment", auth(s.sendH.handleAttachment))
	s.mux.HandleFunc("DELETE /api/sessions", auth(s.sessionH.HandleDelete))
	s.mux.HandleFunc("POST /api/sessions/resume", auth(s.sessionH.HandleResume))
	s.mux.HandleFunc("POST /api/sessions/interrupt", auth(s.sessionH.HandleInterrupt))
	s.mux.HandleFunc("PATCH /api/sessions/label", auth(s.sessionH.HandleSetLabel))
	// Per-session model/effort override (docs/rfc/dashboard-model-effort-control.md).
	s.mux.HandleFunc("POST /api/sessions/override", auth(s.sessionH.HandleOverride))
}

// registerScratchRoutes wires the scratch-drawer route group; deployments
// without a scratch pool register no scratch routes.
func (s *Server) registerScratchRoutes(auth func(http.HandlerFunc) http.HandlerFunc) {
	if s.scratchH == nil {
		return
	}
	s.mux.HandleFunc("POST /api/scratch/open", auth(s.scratchH.HandleOpen))
	s.mux.HandleFunc("POST /api/scratch/{id}/promote", auth(s.scratchH.HandlePromote))
	s.mux.HandleFunc("DELETE /api/scratch/{id}", auth(s.scratchH.HandleDelete))
}

// registerProjectRoutes wires the project route group; all handlers are
// *dashproject.Handlers methods (the *Server-owned /api/planner/stats stays
// at the call site).
func (s *Server) registerProjectRoutes(auth func(http.HandlerFunc) http.HandlerFunc) {
	s.mux.HandleFunc("GET /api/projects", auth(s.projectH.HandleList))
	s.mux.HandleFunc("GET /api/projects/config", auth(s.projectH.HandleConfigGet))
	s.mux.HandleFunc("PUT /api/projects/config", auth(s.projectH.HandleConfigPut))
	s.mux.HandleFunc("POST /api/projects/planner/restart", auth(s.projectH.HandlePlannerRestart))
	s.mux.HandleFunc("POST /api/projects/favorite", auth(s.projectH.HandleFavoriteToggle))
	s.mux.HandleFunc("POST /api/projects/files/exists", auth(s.projectH.HandleFilesExists))
	s.mux.HandleFunc("GET /api/projects/file", auth(s.projectH.HandleFileGet))
	// Workspace file browser: listing reuses HandleFileGet's path-safety;
	// upload is the only write in the file API (CSRF gated by RequireAuth on POST).
	s.mux.HandleFunc("GET /api/projects/files/list", auth(s.projectH.HandleFilesList))
	s.mux.HandleFunc("POST /api/projects/files/upload", auth(s.projectH.HandleFilesUpload))
}

// registerDiscoveredRoutes wires the discovered-session route group
// (list / preview / takeover / close).
func (s *Server) registerDiscoveredRoutes(auth func(http.HandlerFunc) http.HandlerFunc) {
	s.mux.HandleFunc("GET /api/discovered", auth(s.discoveryH.HandleList))
	s.mux.HandleFunc("GET /api/discovered/preview", auth(s.discoveryH.HandlePreview))
	s.mux.HandleFunc("POST /api/discovered/takeover", auth(s.discoveryH.HandleTakeover))
	s.mux.HandleFunc("POST /api/discovered/close", auth(s.discoveryH.HandleClose))
}

// registerCronRoutes wires the cron route group (CRUD + pause/resume/trigger/
// preview + run-history + transcript).
func (s *Server) registerCronRoutes(auth func(http.HandlerFunc) http.HandlerFunc) {
	s.mux.HandleFunc("GET /api/cron", auth(s.cronH.HandleList))
	s.mux.HandleFunc("POST /api/cron", auth(s.cronH.HandleCreate))
	s.mux.HandleFunc("PATCH /api/cron", auth(s.cronH.HandleUpdate))
	s.mux.HandleFunc("DELETE /api/cron", auth(s.cronH.HandleDelete))
	s.mux.HandleFunc("POST /api/cron/pause", auth(s.cronH.HandlePause))
	s.mux.HandleFunc("POST /api/cron/resume", auth(s.cronH.HandleResume))
	s.mux.HandleFunc("POST /api/cron/trigger", auth(s.cronH.HandleTrigger))
	s.mux.HandleFunc("GET /api/cron/preview", auth(s.cronH.HandlePreview))
	// Run history / transcript / events / snapshot share the run_id path
	// param and the same per-IP rate limit.
	s.mux.HandleFunc("GET /api/cron/runs", auth(s.cronH.HandleRunsList))
	s.mux.HandleFunc("GET /api/cron/runs/{run_id}", auth(s.cronH.HandleRunDetail))
	s.mux.HandleFunc("GET /api/cron/runs/{run_id}/transcript", auth(s.cronH.HandleRunTranscript))
	s.mux.HandleFunc("GET /api/cron/runs/{run_id}/events", auth(s.cronH.HandleRunEvents))
	s.mux.HandleFunc("GET /api/cron/runs/{run_id}/snapshot", auth(s.cronH.HandleRunSnapshot))
	// Human confirmation queue (docs/rfc/agentcore-cloud-sandbox.md §7.4);
	// confirm/replay are POSTs and replay stops the live run first.
	s.mux.HandleFunc("GET /api/cron/attention", auth(s.cronH.HandleAttentionList))
	s.mux.HandleFunc("POST /api/cron/runs/{run_id}/confirm", auth(s.cronH.HandleRunConfirm))
	s.mux.HandleFunc("POST /api/cron/runs/{run_id}/replay", auth(s.cronH.HandleRunReplay))
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if s.dashboardToken != "" && !s.auth.IsAuthenticated(r) {
		// Rate-limit unauthenticated GETs so scanners cannot hammer the login
		// renderer. In trusted-proxy mode an unresolvable client IP fails
		// closed rather than sharing one bucket, so a direct-to-origin
		// attacker cannot starve every XFF-less caller (#2120).
		if !requestHasResolvableClientIP(r, s.auth.TrustedProxy) ||
			!s.auth.UnauthDashAllow(clientIP(r, s.auth.TrustedProxy)) {
			errRespRetry(w, http.StatusTooManyRequests, "rate_limited", "too many requests", 60)
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
	// CSP: connect-src 'self' already covers same-origin ws/wss (`ws: wss:` would admit any origin); frame-src blob: = sandboxed
	// workspace .html/.svg previews; img-src data: = CSS background SVGs (no <img src="data:"> ships — TestDashboardCSP_DataImgAuditPinned);
	// font-src jsdelivr = KaTeX @font-face (fonts cannot carry SRI; the KaTeX CSS is SRI-pinned and Permissions-Policy bounds a font-parser
	// RCE), jsdelivr narrowed to /npm/ so user-content paths cannot load; object-src/base-uri 'none' + form-action 'self' close plugin,
	// <base>-re-rooting and form-exfil vectors; require-sri-for is a fail-closed forward-compat hook. Dropping 'unsafe-inline' needs nonces (#441).
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net/npm/; connect-src 'self'; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net/npm/; font-src 'self' https://cdn.jsdelivr.net/npm/; img-src 'self' data: blob:; frame-src 'self' blob:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; require-sri-for script style font")
	// HSTS only over TLS (RFC 6797 §7.2): on plain HTTP it would brick local
	// loopback access for a year. Same gate as the auth cookie Secure flag.
	if s.auth.IsSecure(r) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	// Defence in depth against a compromised CDN script: no getUserMedia etc.
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
	// COOP blocks window.opener XS-Leaks; CORP blocks cross-origin no-cors embeds.
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if serveStaticWithETag(w, r, "dashboard.html") {
		return
	}
	writeStaticAssetBody(w, r, "dashboard.html")
}

// Static asset handlers below are pure embed.FS readers with no Server state,
// so they are package-level functions rather than *Server methods.
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

// handleFavicon serves one SVG for both /favicon.ico and /favicon.svg.
func handleFavicon(w http.ResponseWriter, r *http.Request) {
	data := staticAssetBytes("favicon.svg")
	if data == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "max-age=86400")
	if serveStaticWithETag(w, r, "favicon.svg") {
		return
	}
	writeStaticAssetBody(w, r, "favicon.svg")
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
	// No Service-Worker-Allowed header: /sw.js at root already has scope "/"
	// (#1603). ETag lets the no-cache SW update checks 304 (#1771).
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

// handleContractJS serves static/contract.js (generated backend contract,
// loaded before every other script so NZ_CONTRACT exists at parse time).
func handleContractJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("contract.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "contract.js") {
		return
	}
	writeStaticAssetBody(w, r, "contract.js")
}

// handleNzUtilJS serves static/nz_util.js (shared utility layer loaded before dashboard.js).
func handleNzUtilJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("nz_util.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "nz_util.js") {
		return
	}
	writeStaticAssetBody(w, r, "nz_util.js")
}

// handleCronViewJS serves static/cron_view.js (cron view, loaded after dashboard.js).
func handleCronViewJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("cron_view.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "cron_view.js") {
		return
	}
	writeStaticAssetBody(w, r, "cron_view.js")
}

// handleAgentViewJS serves static/agent_view.js (agent-team view module).
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

// handleAssetBrowserJS serves static/asset_browser.js (docs/rfc/cc-asset-browser.md).
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

// handleFilesViewJS serves static/files_view.js (docs/rfc/workspace-file-browser.md).
func handleFilesViewJS(w http.ResponseWriter, r *http.Request) {
	if staticAssetBytes("files_view.js") == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "files_view.js") {
		return
	}
	writeStaticAssetBody(w, r, "files_view.js")
}

// buildSessionOpts resolves agent config and planner overrides for a session
// key. With a resolver it delegates to ResolveForKey; otherwise (or when the
// resolver reports ok=false, e.g. planner key whose project is gone) it falls
// back to the inline merge so a dashboard resume never fails hard on a stale
// key. Workspace is NOT overlaid for IM 4-segment keys (resume takes it from
// sessions.json); planner keys are always Exempt.
func buildSessionOpts(key string, resolver *session.KeyResolver, agents map[string]session.AgentOpts, projectMgr *project.Manager) session.AgentOpts {
	if resolver != nil {
		if opts, ok := resolver.ResolveForKey(key); ok {
			return opts
		}
	}

	parts := strings.SplitN(key, ":", 4)
	agentID := "general"
	if len(parts) == 4 {
		agentID = parts[3]
	}

	opts := agents[agentID]
	if project.IsPlannerKey(key) {
		opts.Exempt = true // planner sessions are always exempt, regardless of project config
		// Inverse of PlannerKeyFor; splitting on ':' would truncate names
		// containing ':'.
		name := strings.TrimSuffix(strings.TrimPrefix(key, "project:"), ":planner")
		if projectMgr != nil {
			if p := projectMgr.Get(name); p != nil {
				opts.Workspace = p.Path
				if m := projectMgr.EffectivePlannerModel(p); m != "" {
					opts.Model = m
				}
				if prompt := projectMgr.EffectivePlannerPrompt(p); prompt != "" {
					opts.SystemPrompt = session.JoinSystemPrompts(opts.SystemPrompt, prompt) // #2493: layered, opts is a copy
				}
			}
		}
	}
	return opts
}
