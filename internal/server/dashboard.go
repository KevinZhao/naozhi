package server

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sysession"
)

//go:embed static/dashboard.html
var dashboardHTML embed.FS

//go:embed static/manifest.json
var manifestJSON embed.FS

//go:embed static/sw.js
var swJS embed.FS

//go:embed static/dashboard.js
var dashboardJS embed.FS

//go:embed static/agent_view.js
var agentViewJS embed.FS

const authCookieName = "naozhi_auth"

// staticAssetETags holds precomputed strong-form ETags for embedded dashboard
// assets. cron-dashboard-redesign P0 §6 — combined with the existing
// `Cache-Control: no-cache, must-revalidate`, ETag enables a 304 fast-path so
// browsers actually skip body bytes when content hasn't changed (no-cache
// alone forces every load to re-download, hurting both latency and bandwidth).
//
// Because the asset bytes are baked in at build time via //go:embed, the
// SHA-256 hash never changes during a process's lifetime; computing it once at
// init is sufficient. Strong ETag form ("hex") is fine since byte equality is
// the actual semantics (no transformations).
var staticAssetETags = func() map[string]string {
	hash := func(b []byte) string {
		s := sha256.Sum256(b)
		return `"` + hex.EncodeToString(s[:16]) + `"`
	}
	out := map[string]string{}
	if b, err := dashboardHTML.ReadFile("static/dashboard.html"); err == nil {
		out["dashboard.html"] = hash(b)
	}
	if b, err := dashboardJS.ReadFile("static/dashboard.js"); err == nil {
		out["dashboard.js"] = hash(b)
	}
	if b, err := agentViewJS.ReadFile("static/agent_view.js"); err == nil {
		out["agent_view.js"] = hash(b)
	}
	return out
}()

// serveStaticWithETag writes the asset, attaching its precomputed ETag and
// honouring If-None-Match for a 304 fast-path. Returns true when a 304 was
// served so the caller can skip writing the body. Other security headers
// (CSP/COOP/etc.) are still set by the caller before this point.
func serveStaticWithETag(w http.ResponseWriter, r *http.Request, assetKey string) bool {
	tag := staticAssetETags[assetKey]
	if tag == "" {
		return false
	}
	w.Header().Set("ETag", tag)
	if match := r.Header.Get("If-None-Match"); match != "" {
		// Multiple If-None-Match values are comma-separated; do a simple
		// substring check rather than full RFC 7232 list parsing — our tag
		// is unique enough that a substring hit ≈ a real match. Edge case
		// (`*` wildcard) intentionally accepted: client wants any tag, so
		// 304 is correct.
		if match == "*" || strings.Contains(match, tag) {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}

// jsonEncBuf pairs a pooled bytes.Buffer with a json.Encoder bound to it.
// Reused by writeJSON/writeJSONStatus so hot dashboard poll paths do not
// allocate one encoder per HTTP response. Mirrors the shimSendBufPool idiom
// in internal/cli/process.go.
type jsonEncBuf struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

var jsonEncPool = sync.Pool{
	New: func() any {
		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		return &jsonEncBuf{buf: buf, enc: enc}
	},
}

// jsonEncBufMaxCap caps the buffer we return to the pool so a one-off large
// response (e.g. 2MB sessions snapshot) does not permanently pin that capacity.
const jsonEncBufMaxCap = 256 * 1024

func getJSONEnc() *jsonEncBuf {
	e := jsonEncPool.Get().(*jsonEncBuf)
	e.buf.Reset()
	return e
}

func putJSONEnc(e *jsonEncBuf) {
	if e.buf.Cap() > jsonEncBufMaxCap {
		return
	}
	jsonEncPool.Put(e)
}

// marshalPooled marshals v via the pooled encoder and copies the result into a
// fresh []byte. Callers who would otherwise call json.Marshal on a hot path
// (WS event fanout, session_state broadcasts) use this to avoid the per-call
// encodeState allocation. Returned slice is safe to share/outlive the pool.
//
// HTML escaping is disabled on the pooled encoder (see jsonEncPool); the same
// CLIENT-SIDE CONTRACT documented on writeJSON applies to any string field
// carried over a marshalPooled-encoded message: clients MUST render strings
// via textContent (or DOMPurify) and never assign them to innerHTML.
func marshalPooled(v any) ([]byte, error) {
	e := getJSONEnc()
	defer putJSONEnc(e)
	if err := e.enc.Encode(v); err != nil {
		return nil, err
	}
	raw := e.buf.Bytes()
	if n := len(raw); n > 0 && raw[n-1] == '\n' {
		raw = raw[:n-1]
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out, nil
}

// writeJSON sets the Content-Type header and encodes v as JSON to w.
// Logs errors at debug level since HTTP write failures are common after
// client disconnects, but JSON marshal failures indicate bugs.
// For non-200 status codes, use writeJSONStatus instead.
//
// HTML escaping is disabled so dashboard responses preserve `<`, `>`, `&`
// literally — every client consumer uses `textContent` or structured
// rendering, and the default escape just bloats responses and makes raw
// API output (curl / log dumps) hard to diff.
//
// CLIENT-SIDE CONTRACT (R71-SEC-L1): because SetEscapeHTML is disabled,
// any string field in the response that carries content controllable by the
// CLI / tool output / workspace files (e.g. `content` in servePreview,
// `last_prompt`, `summary`, `detail`) MUST be rendered through `textContent`
// in dashboard.js, OR — if rich rendering is required — passed through
// DOMPurify / a whitelist renderer before any innerHTML assignment. A future
// consumer that adds `el.innerHTML = resp.content` without DOMPurify would
// immediately become a stored-XSS vector (file contents are user-writable
// via `/api/sessions/send` + tool writes). When introducing a new response
// field destined for innerHTML, route it through a dedicated helper or the
// CSP `sandbox` iframe path instead of relaxing this rule.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// X-Content-Type-Options: nosniff prevents legacy browsers from MIME-sniffing
	// JSON responses as HTML/JS. Cheap defence-in-depth against any future path
	// that accidentally produces HTML-looking content via SetEscapeHTML(false).
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Cache-Control: no-store prevents shared proxies / browser back-forward
	// cache from retaining dashboard JSON responses (last_prompt / PID / cost /
	// auth cookie ack). Responses are authenticated per-request and carry
	// time-varying session state, so any intermediary cache is a correctness
	// hazard — a second user on the same cache would see the first user's
	// snapshot. R58-PERF-001.
	w.Header().Set("Cache-Control", "no-store")
	e := getJSONEnc()
	defer putJSONEnc(e)
	if err := e.enc.Encode(v); err != nil {
		slog.Debug("write json response", "err", err)
		return
	}
	if _, err := w.Write(e.buf.Bytes()); err != nil {
		slog.Debug("write json response", "err", err)
	}
}

// jsonOKBody is the pre-marshaled body for the common `{"status":"ok"}`
// acknowledgement reply. 20+ dashboard endpoints used to allocate a
// `map[string]string{"status":"ok"}` + run it through the JSON encoder on every
// success response; those hot paths now call writeOK which just copies these
// bytes verbatim (plus a trailing `\n` to match the encoder's NDJSON framing).
// R64-PERF-M4.
var jsonOKBody = []byte("{\"status\":\"ok\"}\n")

// writeOK writes the pre-marshaled `{"status":"ok"}` body with the same headers
// as writeJSON. Use this in preference to writeJSON for fixed ack replies so
// success paths skip the pooled encoder dance entirely.
func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(jsonOKBody); err != nil {
		slog.Debug("write json response", "err", err)
	}
}

// decodeJSONBody reads r.Body into memory and unmarshals it into dst.
//
// Callers MUST have wrapped r.Body with http.MaxBytesReader beforehand so an
// oversize client cannot force unbounded io.ReadAll; the 15+ JSON POST
// handlers in this package all follow that pattern. RNEW-PERF-001: compared
// with json.NewDecoder(r.Body).Decode(dst), this variant avoids the 4 KiB
// bufio.Reader the stdlib Decoder wraps around every request body — bodies
// are already ≤ a few MiB and fit comfortably in a single []byte.
//
// Error semantics match Decoder.Decode closely: unmarshal errors, empty
// body (io.EOF equivalent → json.Unmarshal returns "unexpected end of JSON
// input"), and MaxBytesError all surface as a single error value the
// caller can log/return as 400. Callers that previously wrote specific
// 413 responses from MaxBytesReader must still check errors.As against
// *http.MaxBytesError; they already do today.
func decodeJSONBody(r *http.Request, dst any) error {
	// net/http closes the body after the handler returns, but closing here
	// is still correct for future non-HTTP callers (test mocks, potential
	// reverse-RPC adapters) and keeps the body-lifecycle contract local to
	// this helper.
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		// Distinguishing empty-body from malformed-JSON lets handlers emit
		// a more actionable 400 than the default "unexpected end of JSON
		// input" that json.Unmarshal would otherwise produce.
		return errEmptyJSONBody
	}
	return json.Unmarshal(body, dst)
}

// errEmptyJSONBody is returned by decodeJSONBody when the request has a zero-
// length body. Callers can errors.Is against it to emit a specific message
// instead of the generic JSON parse error.
var errEmptyJSONBody = errors.New("empty request body")

// writeJSONStatus is like writeJSON but writes a non-200 HTTP status code.
// Content-Type must be set before WriteHeader, so this helper ensures
// the correct ordering: Set header → WriteHeader → Encode body.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	e := getJSONEnc()
	defer putJSONEnc(e)
	if err := e.enc.Encode(v); err != nil {
		slog.Debug("write json response", "err", err)
		return
	}
	if _, err := w.Write(e.buf.Bytes()); err != nil {
		slog.Debug("write json response", "err", err)
	}
}

func (s *Server) registerDashboard() {
	s.hub = NewHub(HubOptions{
		Router:           s.router,
		Agents:           s.agents,
		AgentCmds:        s.agentCommands,
		DashToken:        s.dashboardToken,
		CookieMAC:        s.auth.cookieMAC(),
		Guard:            s.sessionGuard,
		Queue:            s.msgQueue,
		Nodes:            s.nodes,
		NodesMu:          &s.nodesMu,
		ProjectMgr:       s.projectMgr,
		Resolver:         s.resolver,
		AllowedRoot:      s.allowedRoot,
		TrustedProxy:     s.auth.trustedProxy,
		WSAuthLimiter:    s.auth.loginAllow,
		WSUpgradeLimiter: s.auth.wsUpgradeAllow,
		// Forward the application-level ctx so a parent cancel cascades
		// to Hub goroutines even when Shutdown() is not explicitly
		// invoked (CTX1). Zero value in pure-unit tests that bypass
		// Start() is harmless — NewHub falls back to Background().
		ParentCtx: s.appCtx,
	})
	s.hub.SetScheduler(s.scheduler)

	// Route /api/sessions snapshot enrichment through the hub's tailer
	// registry now that both exist. RFC v4 agent-team-ui §3.5.4.
	if s.sessionH != nil {
		s.sessionH.snapshotEnricher = s.hub.enrichSnapshot
	}

	// Wire sendH now that hub exists
	uploads := newUploadStore()
	uploads.StartCleanup(s.hub.ctx)
	s.hub.SetUploadStore(uploads)
	s.sendH = &SendHandler{
		nodeAccess:    s.nodeAccess,
		hub:           s.hub,
		uploadStore:   uploads,
		uploadLimiter: newIPLimiterWithProxy(rate.Every(6*time.Second), 10, s.auth.trustedProxy), // 10 uploads/min per IP
		sendLimiter:   newIPLimiterWithProxy(rate.Every(2*time.Second), 30, s.auth.trustedProxy), // 30 sends/min per IP (burst 30)
		auth:          s.auth,
		trustedProxy:  s.auth.trustedProxy,
	}

	// Scratch (ephemeral aside) API. Pool was constructed in New(); wire it
	// through the hub (for sessionOptsFor lookup) and start the TTL sweeper.
	if s.scratchPool != nil {
		s.hub.SetScratchPool(s.scratchPool)
		s.scratchPool.StartSweeper()
		s.scratchH = &ScratchHandler{
			hub:       s.hub,
			pool:      s.scratchPool,
			openLimit: newIPLimiterWithProxy(rate.Every(12*time.Second), 5, s.auth.trustedProxy), // 5 opens/min per IP
			agents:    s.agents,
		}
	}

	// Push session list changes to WS clients
	s.router.SetOnChange(func() { s.hub.BroadcastSessionsUpdate() })

	// Push cron execution results to WS clients. cron_result is preserved
	// for backward compatibility; new clients should subscribe to the
	// run-started / run-ended pair (P0 cron-run-history) which covers every
	// terminal state including skipped and canceled.
	if s.scheduler != nil {
		s.scheduler.SetOnExecute(func(jobID, result, errMsg string) {
			s.hub.BroadcastCronResult(jobID, result, errMsg)
		})
		s.scheduler.SetOnRunStarted(func(ev cron.RunStartedEvent) {
			s.hub.BroadcastCronRunStarted(ev.JobID, ev.RunID, ev.StartedAt,
				string(ev.Trigger), ev.SessionID, ev.Fresh)
		})
		s.scheduler.SetOnRunEnded(func(ev cron.RunEndedEvent) {
			s.hub.BroadcastCronRunEnded(ev.JobID, ev.RunID, string(ev.State),
				ev.StartedAt, ev.EndedAt, ev.DurationMS, ev.SessionID,
				string(ev.ErrorClass), ev.ErrorMsg, string(ev.Trigger))
		})
	}

	// Push system-daemon Run events to WS clients (RFC §9.4).
	// Manager.SetCallbacks accepts wiring after Manager.Start so this is
	// safe even when sysession was constructed in main.go before the Hub
	// existed.
	if s.sysessionMgr != nil {
		s.sysessionMgr.SetCallbacks(
			func(ev sysession.DaemonRunStartedEvent) {
				s.hub.BroadcastDaemonRunStarted(ev.Name, ev.RunID, string(ev.Trigger), ev.StartedAt)
			},
			func(ev sysession.DaemonRunEndedEvent) {
				s.hub.BroadcastDaemonRunEnded(ev.Name, ev.RunID,
					string(ev.State), string(ev.ErrorClass),
					string(ev.Trigger), ev.DurationMS)
			},
		)
	}

	// Authenticated API routes
	auth := s.auth.requireAuth
	s.mux.HandleFunc("GET /api/cli/backends", auth(s.cliH.handle))
	s.mux.HandleFunc("GET /api/sessions", auth(s.sessionH.handleList))
	s.mux.HandleFunc("GET /api/sessions/events", auth(s.sessionH.handleEvents))
	s.mux.HandleFunc("GET /api/sessions/agent_events", auth(s.agentEventsH.handleAgentEvents))
	s.mux.HandleFunc("GET /api/sessions/tool_result", auth(s.agentEventsH.handleToolResult))
	s.mux.HandleFunc("POST /api/sessions/send", auth(s.sendH.handleSend))
	s.mux.HandleFunc("POST /api/sessions/upload", auth(s.sendH.handleUpload))
	s.mux.HandleFunc("GET /api/sessions/attachment", auth(s.sendH.handleAttachment))
	s.mux.HandleFunc("DELETE /api/sessions", auth(s.sessionH.handleDelete))
	s.mux.HandleFunc("POST /api/sessions/resume", auth(s.sessionH.handleResume))
	s.mux.HandleFunc("POST /api/sessions/interrupt", auth(s.sessionH.handleInterrupt))
	s.mux.HandleFunc("PATCH /api/sessions/label", auth(s.sessionH.handleSetLabel))
	s.mux.HandleFunc("GET /api/discovered", auth(s.discoveryH.handleList))
	s.mux.HandleFunc("GET /api/discovered/preview", auth(s.discoveryH.handlePreview))
	s.mux.HandleFunc("POST /api/discovered/takeover", auth(s.discoveryH.handleTakeover))
	s.mux.HandleFunc("POST /api/discovered/close", auth(s.discoveryH.handleClose))
	s.mux.HandleFunc("GET /api/projects", auth(s.projectH.handleList))
	s.mux.HandleFunc("GET /api/projects/config", auth(s.projectH.handleConfigGet))
	s.mux.HandleFunc("PUT /api/projects/config", auth(s.projectH.handleConfigPut))
	s.mux.HandleFunc("POST /api/projects/planner/restart", auth(s.projectH.handlePlannerRestart))
	s.mux.HandleFunc("POST /api/projects/favorite", auth(s.projectH.handleFavoriteToggle))
	s.mux.HandleFunc("POST /api/projects/files/exists", auth(s.projectH.handleFilesExists))
	s.mux.HandleFunc("GET /api/projects/file", auth(s.projectH.handleFileGet))
	s.mux.HandleFunc("POST /api/transcribe", auth(s.transcribeH.handleTranscribe))
	s.mux.HandleFunc("GET /api/cron", auth(s.cronH.handleList))
	s.mux.HandleFunc("POST /api/cron", auth(s.cronH.handleCreate))
	s.mux.HandleFunc("PATCH /api/cron", auth(s.cronH.handleUpdate))
	s.mux.HandleFunc("DELETE /api/cron", auth(s.cronH.handleDelete))
	s.mux.HandleFunc("POST /api/cron/pause", auth(s.cronH.handlePause))
	s.mux.HandleFunc("POST /api/cron/resume", auth(s.cronH.handleResume))
	s.mux.HandleFunc("POST /api/cron/trigger", auth(s.cronH.handleTrigger))
	s.mux.HandleFunc("GET /api/cron/preview", auth(s.cronH.handlePreview))
	// P1 cron-run-history: per-job execution history.
	s.mux.HandleFunc("GET /api/cron/runs", auth(s.cronH.handleRunsList))
	s.mux.HandleFunc("GET /api/cron/runs/{run_id}", auth(s.cronH.handleRunDetail))
	// cron-dashboard-redesign P2a §4.4.3 — transcript endpoint. Path
	// param mirrors handleRunDetail; same per-IP rate limit applies.
	s.mux.HandleFunc("GET /api/cron/runs/{run_id}/transcript", auth(s.cronH.handleRunTranscript))
	// system-session daemons (docs/rfc/system-session.md §9.2/§9.3)
	s.mux.HandleFunc("GET /api/system/daemons", auth(s.handleSystemDaemons))
	s.mux.HandleFunc("POST /api/system/labels/clear-origin", auth(s.handleClearLabelOrigin))
	s.mux.HandleFunc("POST /api/auth/logout", auth(s.auth.handleLogout))
	// pprof debug endpoints: auth-gated + loopback-only. Registered via
	// a package-local helper that wraps the stdlib net/http/pprof
	// handlers. See internal/server/debug_pprof.go + docs/ops/pprof.md.
	s.registerPprof()
	// expvar /debug/vars: same auth + loopback posture as pprof. Exposes
	// the five naozhi_* counters from internal/metrics plus stdlib
	// cmdline/memstats. OBS2.
	s.registerExpvar()
	if s.scratchH != nil {
		s.mux.HandleFunc("POST /api/scratch/open", auth(s.scratchH.handleOpen))
		s.mux.HandleFunc("POST /api/scratch/{id}/promote", auth(s.scratchH.handlePromote))
		s.mux.HandleFunc("DELETE /api/scratch/{id}", auth(s.scratchH.handleDelete))
	}
	// memory link preview (docs/rfc/memory-link-rendering.md): exposes
	// ~/.claude/projects/<scope>/memory/<slug>.md to the dashboard inlineMd
	// renderer so [[slug]] tokens become hover-previewable cards.
	if s.memoryH == nil {
		s.memoryH = NewMemoryHandler(s.auth.trustedProxy)
	}
	s.mux.HandleFunc("GET /api/memory/{slug}", auth(s.memoryH.handleGet))

	// Unauthenticated routes (login, static assets, WebSocket with own auth)
	s.mux.HandleFunc("POST /api/auth/login", s.auth.handleLogin)
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /manifest.json", s.handleManifest)
	s.mux.HandleFunc("GET /sw.js", s.handleSW)
	s.mux.HandleFunc("GET /static/dashboard.js", s.handleDashboardJS)
	s.mux.HandleFunc("GET /static/agent_view.js", s.handleAgentViewJS)
	s.mux.HandleFunc("GET /ws", s.hub.HandleUpgrade)
	if s.reverseNodeServer != nil {
		s.mux.Handle("GET /ws-node", s.reverseNodeServer)
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if s.dashboardToken != "" && !s.auth.isAuthenticated(r) {
		// R230C-SEC-12: rate-limit unauthenticated GETs so a scanner cannot
		// repeatedly hammer the login template renderer (CSP+HTML+cookie
		// crypto path) and fingerprint deployments. Same 60/min×20 burst
		// budget as wsUpgradeLimiter accommodates real users (tab-reload,
		// mobile-wake, multiple browser windows) while limiting sustained
		// abuse. Authenticated users are unaffected.
		if !s.auth.unauthDashAllow(clientIP(r, s.auth.trustedProxy)) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		s.auth.serveLoginPage(w)
		return
	}
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
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
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; connect-src 'self'; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; font-src 'self' https://cdn.jsdelivr.net; img-src 'self' data: blob:; frame-src 'self' blob:")
	// HSTS is only meaningful over TLS (RFC 6797 §7.2). Sending it on plain
	// HTTP would still be honoured by browsers and can brick local HTTP
	// loopback access for a year. Gate on the same isSecure() helper the
	// auth cookie Secure flag uses, so behaviour is consistent.
	if s.auth.isSecure(r) {
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
	if _, err := w.Write(data); err != nil {
		slog.Debug("dashboard write", "err", err)
	}
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	data, err := manifestJSON.ReadFile("static/manifest.json")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "max-age=3600")
	if _, err := w.Write(data); err != nil {
		slog.Debug("manifest write", "err", err)
	}
}

func (s *Server) handleSW(w http.ResponseWriter, r *http.Request) {
	data, err := swJS.ReadFile("static/sw.js")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Service-Worker-Allowed", "/")
	if _, err := w.Write(data); err != nil {
		slog.Debug("sw write", "err", err)
	}
}

func (s *Server) handleDashboardJS(w http.ResponseWriter, r *http.Request) {
	data, err := dashboardJS.ReadFile("static/dashboard.js")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "dashboard.js") {
		return
	}
	if _, err := w.Write(data); err != nil {
		slog.Debug("dashboard js write", "err", err)
	}
}

// handleAgentViewJS serves static/agent_view.js — the RFC v4 agent-team-ui
// dashboard module. Mirrors handleDashboardJS for caching/CSP headers so
// the two scripts behave identically in the browser cache.
func (s *Server) handleAgentViewJS(w http.ResponseWriter, r *http.Request) {
	data, err := agentViewJS.ReadFile("static/agent_view.js")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, "agent_view.js") {
		return
	}
	if _, err := w.Write(data); err != nil {
		slog.Debug("agent_view js write", "err", err)
	}
}

// strOrFallback extracts a string from a map, trying the primary key first then the fallback.
// Used to handle remote nodes that may send Go-default JSON keys (e.g. "Name") instead of
// tagged lowercase keys (e.g. "name").
func strOrFallback(m map[string]any, key, fallback string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	v, _ := m[fallback].(string)
	return v
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
		if projectMgr != nil {
			pParts := strings.SplitN(key, ":", 3)
			if len(pParts) == 3 {
				if p := projectMgr.Get(pParts[1]); p != nil {
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
	}
	return opts
}
