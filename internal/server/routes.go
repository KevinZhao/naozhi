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
	// Authenticated API routes. Every dashboard sub-package declares its own
	// patterns (its routes.go) and mountRoutes applies the API chain, so this
	// function no longer decides any feature's URL space (#2554).
	s.mountRoutes(hs.cliH.Routes())
	s.mountRoutes(hs.accessProfilesH.Routes())
	s.mountRoutes(hs.sessionH.Routes())
	s.mountRoutes(hs.costH.Routes())
	s.mountRoutes(hs.agentEventsH.Routes())
	s.mountRoutes(hs.sendH.Routes())
	s.mountRoutes(hs.discoveryH.Routes())
	s.mountRoutes(hs.projectH.Routes())
	s.mountRoutes(hs.plannerH.Routes())
	s.mountRoutes(hs.transcribeH.Routes())
	s.mountRoutes(hs.cronH.Routes())
	s.mountRoutes(hs.systemH.Routes())
	s.mountRoutes(hs.uiSettingsH.Routes())
	s.mountRoutes(hs.memoryH.Routes())
	s.mountRoutes(hs.ccAssetsH.Routes())
	// scratchH is nil when the ephemeral-aside feature is off.
	if hs.scratchH != nil {
		s.mountRoutes(hs.scratchH.Routes())
	}
	s.mux.HandleFunc("POST /api/auth/logout", s.apiChain()(s.auth.HandleLogout))
	// pprof / expvar are auth-gated + loopback-only AND require debug_mode so a
	// leaked dashboard token cannot enumerate goroutine stacks or counters.
	// Runbook: docs/ops/pprof.md.
	if s.debugMode {
		s.registerPprof()
		s.registerExpvar()
	}

	// Server-owned routes: the dashboard shell, static assets and the WS
	// upgrade. These stay here because they are not a feature's API surface —
	// no dashboard sub-package owns /dashboard or /static/*.
	auth := s.apiChain()
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

// registerScratchRoutes wires the scratch-drawer route group; deployments
// without a scratch pool register no scratch routes.

// registerProjectRoutes wires the project route group; all handlers are
// *dashproject.Handlers methods (the *Server-owned /api/planner/stats stays
// at the call site).

// registerDiscoveredRoutes wires the discovered-session route group
// (list / preview / takeover / close).

// registerCronRoutes wires the cron route group (CRUD + pause/resume/trigger/
// preview + run-history + transcript).

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

// mountRoutes registers a sub-package's declared routes, applying the API
// middleware chain to every non-public one (#2554).
//
// This is the ONLY place a dashboard sub-package route reaches the mux. A
// sub-package hands over data (httputil.Route) and never touches s.mux, so
// "this one route missed the auth wrapper" is not a mistake that can be made
// here — which is why the api_route_owner / handle_decl lint rules that used to
// reconstruct this boundary from ASTs can go.
func (s *Server) mountRoutes(routes []httputil.Route) {
	for _, rt := range routes {
		h := rt.Handler
		if !rt.Public {
			h = s.apiChain()(h)
		}
		s.mux.HandleFunc(rt.Pattern, h)
	}
}

// apiChain is the middleware every authenticated /api/ route passes through.
// Named and returned as one value so there is a single answer to "what guards
// the API", rather than a RequireAuth reference threaded through six helpers.
func (s *Server) apiChain() httputil.Middleware {
	return s.auth.RequireAuth
}

// Routes returns the send/upload/attachment surface. SendHandler lives in this
// package (it reaches sendEngine internals), so its routes are declared here
// rather than in a dashboard sub-package — but they go through the same
// mountRoutes chain as everything else (#2554).
func (h *SendHandler) Routes() []httputil.Route {
	return []httputil.Route{
		{Pattern: "POST /api/sessions/send", Handler: h.handleSend},
		{Pattern: "POST /api/sessions/bind", Handler: h.handleBind},
		{Pattern: "POST /api/sessions/upload", Handler: h.handleUpload},
		{Pattern: "POST /api/sessions/orient", Handler: h.handleOrient},
		{Pattern: "GET /api/sessions/attachment", Handler: h.handleAttachment},
	}
}
