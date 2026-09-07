package server

import (
	"log/slog"
	"net/http"
	"strings"

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

// registerDashboard registers the dashboard's routes. Construction moved to
// buildDashboard in #2552 and the goroutine starts moved to
// startDashboardLoops in #2553, so this function is registration only —
// nothing here may build a dependency or start a goroutine. That is what lets
// buildServer call it with a local handlerSet that then goes out of scope.
// startDashboardLoops starts the dashboard's background goroutines. Separate
// from registerDashboard since #2553: registration runs at construction (so the
// handlerSet can be a local), but a goroutine started at construction would
// leak its ticker if a later construction step panicked, so the starts stay in
// Start. Same rule buildDashboard already follows.
func (s *Server) startDashboardLoops() {
	// The upload-store cleanup loop is process-lifetime (appCtx), not
	// Hub-lifetime: a Hub hot-reload must not cancel it and leak temp files.
	s.uploadStore.StartCleanup(s.appCtx)

	if s.scratchPool != nil {
		s.scratchPool.StartSweeper()
	}
}

func (s *Server) registerDashboard(hs *handlerSet) {
	// Authenticated API routes
	auth := s.auth.RequireAuth
	s.mux.HandleFunc("GET /api/cli/backends", auth(hs.cliH.Handle))
	// Access profiles: list returns only non-sensitive fields (never env
	// values or tokens); create is disabled (400) when ConfigPath is unset.
	s.mux.HandleFunc("GET /api/access-profiles", auth(hs.accessProfilesH.HandleList))
	s.mux.HandleFunc("POST /api/access-profiles", auth(hs.accessProfilesH.HandleCreate))
	// Route groups live in same-file helpers so the routes_snapshot AST gate
	// (which scans routes.go as a whole) stays stable.
	s.registerSessionRoutes(hs, auth)
	s.registerDiscoveredRoutes(hs, auth)
	s.registerProjectRoutes(hs, auth)
	// Process-resource probe (RSS / goroutines / planner fan-out) that does not
	// require the loopback-only expvar surface.
	s.mux.HandleFunc("GET /api/planner/stats", auth(hs.plannerH.HandleStats))
	s.mux.HandleFunc("POST /api/transcribe", auth(hs.transcribeH.HandleTranscribe))
	s.registerCronRoutes(hs, auth)
	// system-session daemons (docs/rfc/system-session.md §9.2/§9.3)
	s.mux.HandleFunc("GET /api/system/daemons", auth(hs.systemH.HandleDaemons))
	s.mux.HandleFunc("POST /api/system/labels/clear-origin", auth(hs.systemH.HandleClearLabelOrigin))
	// self-update (docs/rfc/dashboard-update-notice.md)
	s.mux.HandleFunc("GET /api/system/update", auth(hs.systemH.HandleUpdateStatus))
	s.mux.HandleFunc("POST /api/system/update/apply", auth(hs.systemH.HandleUpdateApply))
	// instance-wide UI preferences, persisted server-side (dashboard/ext/uisettings)
	s.mux.HandleFunc("GET /api/settings", auth(hs.uiSettingsH.HandleGet))
	s.mux.HandleFunc("PUT /api/settings", auth(hs.uiSettingsH.HandlePut))
	s.mux.HandleFunc("POST /api/auth/logout", auth(s.auth.HandleLogout))
	// pprof / expvar are auth-gated + loopback-only AND require debug_mode so a
	// leaked dashboard token cannot enumerate goroutine stacks or counters.
	// Runbook: docs/ops/pprof.md.
	if s.debugMode {
		s.registerPprof()
		s.registerExpvar()
	}
	s.registerScratchRoutes(hs, auth)
	// memory link preview (docs/rfc/memory-link-rendering.md): serves
	// ~/.claude/projects/<scope>/memory/<slug>.md for [[slug]] hover cards.
	s.mux.HandleFunc("GET /api/memory/{slug}", auth(hs.memoryH.HandleGet))

	// Installed-asset browser (docs/rfc/cc-asset-browser.md).
	s.registerAssetBrowserRoutes(hs, auth)

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
	// Dashboard JS is auth-gated: it embeds the API endpoint list + client
	// schema (recon surface); the login page loads no /static/ JS (#1328).
	s.mux.HandleFunc("GET /static/css/{file}", auth(handleDashboardCSS))
	s.mux.HandleFunc("GET /static/contract.js", auth(handleContractJS))
	s.mux.HandleFunc("GET /static/nz_util.js", auth(handleNzUtilJS))
	s.mux.HandleFunc("GET /static/render_md.js", auth(handleRenderMdJS))
	s.mux.HandleFunc("GET /static/self_update.js", auth(handleSelfUpdateJS))
	s.mux.HandleFunc("GET /static/voice.js", auth(handleVoiceJS))
	s.mux.HandleFunc("GET /static/session_header.js", auth(handleSessionHeaderJS))
	s.mux.HandleFunc("GET /static/composer_files.js", auth(handleComposerFilesJS))
	s.mux.HandleFunc("GET /static/mobile_nav.js", auth(handleMobileNavJS))
	s.mux.HandleFunc("GET /static/split_view.js", auth(handleSplitViewJS))
	s.mux.HandleFunc("GET /static/system_view.js", auth(handleSystemViewJS))
	s.mux.HandleFunc("GET /static/running_banner.js", auth(handleRunningBannerJS))
	s.mux.HandleFunc("GET /static/file_refs.js", auth(handleFileRefsJS))
	s.mux.HandleFunc("GET /static/utilities.js", auth(handleUtilitiesJS))
	s.mux.HandleFunc("GET /static/discovery.js", auth(handleDiscoveryJS))
	s.mux.HandleFunc("GET /static/tuning.js", auth(handleTuningJS))
	s.mux.HandleFunc("GET /static/msg_nav.js", auth(handleMsgNavJS))
	s.mux.HandleFunc("GET /static/sidebar_project.js", auth(handleSidebarProjectJS))
	s.mux.HandleFunc("GET /static/auth_modal.js", auth(handleAuthModalJS))
	s.mux.HandleFunc("GET /static/send_message.js", auth(handleSendMessageJS))
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
func (s *Server) registerSessionRoutes(hs *handlerSet, auth func(http.HandlerFunc) http.HandlerFunc) {
	s.mux.HandleFunc("GET /api/sessions", auth(hs.sessionH.HandleList))
	s.mux.HandleFunc("GET /api/sessions/events", auth(hs.sessionH.HandleEvents))
	s.mux.HandleFunc("GET /api/sessions/runs", auth(hs.sessionH.HandleRuns))
	// Cost ledger read API (docs/rfc/cost-ledger.md §7); unit-bucketed, rate limited.
	s.mux.HandleFunc("GET /api/cost/summary", auth(hs.costH.HandleSummary))
	s.mux.HandleFunc("GET /api/cost/entries", auth(hs.costH.HandleEntries))
	s.mux.HandleFunc("GET /api/sessions/git", auth(hs.sessionH.HandleGit))
	s.mux.HandleFunc("GET /api/sessions/agent_events", auth(hs.agentEventsH.HandleAgentEvents))
	s.mux.HandleFunc("GET /api/sessions/tool_result", auth(hs.agentEventsH.HandleToolResult))
	s.mux.HandleFunc("POST /api/sessions/send", auth(hs.sendH.handleSend))
	s.mux.HandleFunc("POST /api/sessions/bind", auth(hs.sendH.handleBind))
	s.mux.HandleFunc("POST /api/sessions/upload", auth(hs.sendH.handleUpload))
	s.mux.HandleFunc("POST /api/sessions/orient", auth(hs.sendH.handleOrient))
	s.mux.HandleFunc("GET /api/sessions/attachment", auth(hs.sendH.handleAttachment))
	s.mux.HandleFunc("DELETE /api/sessions", auth(hs.sessionH.HandleDelete))
	s.mux.HandleFunc("POST /api/sessions/resume", auth(hs.sessionH.HandleResume))
	s.mux.HandleFunc("POST /api/sessions/interrupt", auth(hs.sessionH.HandleInterrupt))
	s.mux.HandleFunc("PATCH /api/sessions/label", auth(hs.sessionH.HandleSetLabel))
	// Per-session model/effort override (docs/rfc/dashboard-model-effort-control.md).
	s.mux.HandleFunc("POST /api/sessions/override", auth(hs.sessionH.HandleOverride))
}

// registerScratchRoutes wires the scratch-drawer route group; deployments
// without a scratch pool register no scratch routes.
func (s *Server) registerScratchRoutes(hs *handlerSet, auth func(http.HandlerFunc) http.HandlerFunc) {
	if hs.scratchH == nil {
		return
	}
	s.mux.HandleFunc("POST /api/scratch/open", auth(hs.scratchH.HandleOpen))
	s.mux.HandleFunc("POST /api/scratch/{id}/promote", auth(hs.scratchH.HandlePromote))
	s.mux.HandleFunc("DELETE /api/scratch/{id}", auth(hs.scratchH.HandleDelete))
}

// registerProjectRoutes wires the project route group; all handlers are
// *dashproject.Handlers methods (the *Server-owned /api/planner/stats stays
// at the call site).
func (s *Server) registerProjectRoutes(hs *handlerSet, auth func(http.HandlerFunc) http.HandlerFunc) {
	s.mux.HandleFunc("GET /api/projects", auth(hs.projectH.HandleList))
	s.mux.HandleFunc("GET /api/projects/config", auth(hs.projectH.HandleConfigGet))
	s.mux.HandleFunc("PUT /api/projects/config", auth(hs.projectH.HandleConfigPut))
	s.mux.HandleFunc("POST /api/projects/planner/restart", auth(hs.projectH.HandlePlannerRestart))
	s.mux.HandleFunc("POST /api/projects/favorite", auth(hs.projectH.HandleFavoriteToggle))
	s.mux.HandleFunc("POST /api/projects/files/exists", auth(hs.projectH.HandleFilesExists))
	s.mux.HandleFunc("GET /api/projects/file", auth(hs.projectH.HandleFileGet))
	// Workspace file browser: listing reuses HandleFileGet's path-safety;
	// upload is the only write in the file API (CSRF gated by RequireAuth on POST).
	s.mux.HandleFunc("GET /api/projects/files/list", auth(hs.projectH.HandleFilesList))
	s.mux.HandleFunc("POST /api/projects/files/upload", auth(hs.projectH.HandleFilesUpload))
}

// registerDiscoveredRoutes wires the discovered-session route group
// (list / preview / takeover / close).
func (s *Server) registerDiscoveredRoutes(hs *handlerSet, auth func(http.HandlerFunc) http.HandlerFunc) {
	s.mux.HandleFunc("GET /api/discovered", auth(hs.discoveryH.HandleList))
	s.mux.HandleFunc("GET /api/discovered/preview", auth(hs.discoveryH.HandlePreview))
	s.mux.HandleFunc("POST /api/discovered/takeover", auth(hs.discoveryH.HandleTakeover))
	s.mux.HandleFunc("POST /api/discovered/close", auth(hs.discoveryH.HandleClose))
}

// registerCronRoutes wires the cron route group (CRUD + pause/resume/trigger/
// preview + run-history + transcript).
func (s *Server) registerCronRoutes(hs *handlerSet, auth func(http.HandlerFunc) http.HandlerFunc) {
	s.mux.HandleFunc("GET /api/cron", auth(hs.cronH.HandleList))
	s.mux.HandleFunc("POST /api/cron", auth(hs.cronH.HandleCreate))
	s.mux.HandleFunc("PATCH /api/cron", auth(hs.cronH.HandleUpdate))
	s.mux.HandleFunc("DELETE /api/cron", auth(hs.cronH.HandleDelete))
	s.mux.HandleFunc("POST /api/cron/pause", auth(hs.cronH.HandlePause))
	s.mux.HandleFunc("POST /api/cron/resume", auth(hs.cronH.HandleResume))
	s.mux.HandleFunc("POST /api/cron/trigger", auth(hs.cronH.HandleTrigger))
	s.mux.HandleFunc("GET /api/cron/preview", auth(hs.cronH.HandlePreview))
	// Run history / transcript / events / snapshot share the run_id path
	// param and the same per-IP rate limit.
	s.mux.HandleFunc("GET /api/cron/runs", auth(hs.cronH.HandleRunsList))
	s.mux.HandleFunc("GET /api/cron/runs/{run_id}", auth(hs.cronH.HandleRunDetail))
	s.mux.HandleFunc("GET /api/cron/runs/{run_id}/transcript", auth(hs.cronH.HandleRunTranscript))
	s.mux.HandleFunc("GET /api/cron/runs/{run_id}/events", auth(hs.cronH.HandleRunEvents))
	s.mux.HandleFunc("GET /api/cron/runs/{run_id}/snapshot", auth(hs.cronH.HandleRunSnapshot))
	// Human confirmation queue (docs/rfc/agentcore-cloud-sandbox.md §7.4);
	// confirm/replay are POSTs and replay stops the live run first.
	s.mux.HandleFunc("GET /api/cron/attention", auth(hs.cronH.HandleAttentionList))
	s.mux.HandleFunc("POST /api/cron/runs/{run_id}/confirm", auth(hs.cronH.HandleRunConfirm))
	s.mux.HandleFunc("POST /api/cron/runs/{run_id}/replay", auth(hs.cronH.HandleRunReplay))
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
	// CSP: built at init in dashboard_csp.go (#1980) — script-src has no
	// unsafe-inline (data-action delegation + hashed theme bootstrap), cdn
	// pinned to exact versioned files; connect-src 'self' covers same-origin
	// ws/wss; frame-src blob: = sandboxed previews; style-src unsafe-inline
	// stays until D6 (#2559) migrates the generated style="" attributes.
	w.Header().Set("Content-Security-Policy", dashboardCSP)
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
