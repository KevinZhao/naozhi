package server

import (
	"bytes"
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

//go:embed static/dashboard.html
var dashboardHTML embed.FS

//go:embed static/manifest.json
var manifestJSON embed.FS

//go:embed static/sw.js
var swJS embed.FS

//go:embed static/dashboard.js
var dashboardJS embed.FS

const authCookieName = "naozhi_auth"

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
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// X-Content-Type-Options: nosniff prevents legacy browsers from MIME-sniffing
	// JSON responses as HTML/JS. Cheap defence-in-depth against any future path
	// that accidentally produces HTML-looking content via SetEscapeHTML(false).
	w.Header().Set("X-Content-Type-Options", "nosniff")
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

// writeJSONStatus is like writeJSON but writes a non-200 HTTP status code.
// Content-Type must be set before WriteHeader, so this helper ensures
// the correct ordering: Set header → WriteHeader → Encode body.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
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
		Router:        s.router,
		Agents:        s.agents,
		AgentCmds:     s.agentCommands,
		DashToken:     s.dashboardToken,
		CookieMAC:     s.auth.cookieMAC(),
		Guard:         s.sessionGuard,
		Queue:         s.msgQueue,
		Nodes:         s.nodes,
		NodesMu:       &s.nodesMu,
		ProjectMgr:    s.projectMgr,
		AllowedRoot:   s.allowedRoot,
		TrustedProxy:  s.auth.trustedProxy,
		WSAuthLimiter: s.auth.loginAllow,
	})
	s.hub.SetScheduler(s.scheduler)

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
		trustedProxy:  s.auth.trustedProxy,
	}

	// Push session list changes to WS clients
	s.router.SetOnChange(func() { s.hub.BroadcastSessionsUpdate() })

	// Push cron execution results to WS clients
	if s.scheduler != nil {
		s.scheduler.SetOnExecute(func(jobID, result, errMsg string) {
			s.hub.BroadcastCronResult(jobID, result, errMsg)
		})
	}

	// Authenticated API routes
	auth := s.auth.requireAuth
	s.mux.HandleFunc("GET /api/cli/backends", auth(s.cliH.handle))
	s.mux.HandleFunc("GET /api/sessions", auth(s.sessionH.handleList))
	s.mux.HandleFunc("GET /api/sessions/events", auth(s.sessionH.handleEvents))
	s.mux.HandleFunc("POST /api/sessions/send", auth(s.sendH.handleSend))
	s.mux.HandleFunc("POST /api/sessions/upload", auth(s.sendH.handleUpload))
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
	s.mux.HandleFunc("POST /api/auth/logout", auth(s.auth.handleLogout))

	// Unauthenticated routes (login, static assets, WebSocket with own auth)
	s.mux.HandleFunc("POST /api/auth/login", s.auth.handleLogin)
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /manifest.json", s.handleManifest)
	s.mux.HandleFunc("GET /sw.js", s.handleSW)
	s.mux.HandleFunc("GET /static/dashboard.js", s.handleDashboardJS)
	s.mux.HandleFunc("GET /ws", s.hub.HandleUpgrade)
	if s.reverseNodeServer != nil {
		s.mux.Handle("GET /ws-node", s.reverseNodeServer)
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if s.dashboardToken != "" && !s.auth.isAuthenticated(r) {
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
	// connect-src includes both ws: and wss:. ws: is required for local HTTP
	// development (browsers reject ws:// under a CSP that only lists wss:) while
	// wss: covers production TLS deployments. The browser automatically picks
	// the matching scheme based on page origin, so listing both does not widen
	// the attack surface for TLS users.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; connect-src 'self' ws: wss:; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; font-src 'self' https://cdn.jsdelivr.net; img-src 'self' data: blob:")
	w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
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
	if _, err := w.Write(data); err != nil {
		slog.Debug("dashboard js write", "err", err)
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

// buildSessionOpts resolves agent config and planner overrides for a session key.
func buildSessionOpts(key string, agents map[string]session.AgentOpts, projectMgr *project.Manager) session.AgentOpts {
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
