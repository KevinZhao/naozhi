package server

import (
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

//go:embed static/dashboard.html
var dashboardHTML embed.FS

//go:embed static/manifest.json
var manifestJSON embed.FS

//go:embed static/sw.js
var swJS embed.FS

const authCookieName = "naozhi_auth"

// writeJSON encodes v as JSON to w. Logs errors at debug level since HTTP write
// failures are common after client disconnects, but JSON marshal failures indicate bugs.
func writeJSON(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
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
		Nodes:         s.nodes,
		NodesMu:       &s.nodesMu,
		ProjectMgr:    s.projectMgr,
		AllowedRoot:   s.allowedRoot,
		WSAuthLimiter: s.auth.loginLimiterFor,
	})
	s.hub.SetScheduler(s.scheduler)

	// Wire sendH now that hub exists
	s.sendH = &SendHandler{nodeAccess: s.nodeAccess, hub: s.hub}

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
	s.mux.HandleFunc("GET /api/sessions", auth(s.sessionH.handleList))
	s.mux.HandleFunc("GET /api/sessions/events", auth(s.sessionH.handleEvents))
	s.mux.HandleFunc("POST /api/sessions/send", auth(s.sendH.handleSend))
	s.mux.HandleFunc("DELETE /api/sessions", auth(s.sessionH.handleDelete))
	s.mux.HandleFunc("POST /api/sessions/resume", auth(s.sessionH.handleResume))
	s.mux.HandleFunc("GET /api/discovered", auth(s.discoveryH.handleList))
	s.mux.HandleFunc("GET /api/discovered/preview", auth(s.discoveryH.handlePreview))
	s.mux.HandleFunc("POST /api/discovered/takeover", auth(s.discoveryH.handleTakeover))
	s.mux.HandleFunc("GET /api/projects", auth(s.projectH.handleList))
	s.mux.HandleFunc("GET /api/projects/config", auth(s.projectH.handleConfigGet))
	s.mux.HandleFunc("PUT /api/projects/config", auth(s.projectH.handleConfigPut))
	s.mux.HandleFunc("POST /api/projects/planner/restart", auth(s.projectH.handlePlannerRestart))
	s.mux.HandleFunc("POST /api/transcribe", auth(s.transcribeH.handleTranscribe))
	s.mux.HandleFunc("GET /api/cron", auth(s.cronH.handleList))
	s.mux.HandleFunc("POST /api/cron", auth(s.cronH.handleCreate))
	s.mux.HandleFunc("DELETE /api/cron", auth(s.cronH.handleDelete))
	s.mux.HandleFunc("POST /api/cron/pause", auth(s.cronH.handlePause))
	s.mux.HandleFunc("POST /api/cron/resume", auth(s.cronH.handleResume))
	s.mux.HandleFunc("GET /api/cron/preview", auth(s.cronH.handlePreview))
	s.mux.HandleFunc("POST /api/auth/logout", auth(s.auth.handleLogout))

	// Unauthenticated routes (login, static assets, WebSocket with own auth)
	s.mux.HandleFunc("POST /api/auth/login", s.auth.handleLogin)
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /manifest.json", s.handleManifest)
	s.mux.HandleFunc("GET /sw.js", s.handleSW)
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
	// TODO: 'unsafe-inline' in script-src weakens XSS protection. Moving inline
	// JS to static/dashboard.js would allow removing it (nonce or 'self' only).
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; connect-src 'self' wss: ws:; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; font-src 'self' https://cdn.jsdelivr.net; img-src 'self' data: blob:")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
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
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Service-Worker-Allowed", "/")
	if _, err := w.Write(data); err != nil {
		slog.Debug("sw write", "err", err)
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
		opts.Exempt = true
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
