package server

import (
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

//go:embed static/dashboard.html
var dashboardHTML embed.FS

func (s *Server) registerDashboard() {
	s.mux.HandleFunc("GET /api/sessions", s.handleAPISessions)
	s.mux.HandleFunc("GET /api/sessions/events", s.handleAPISessionEvents)
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		http.Error(w, "dashboard not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(data); err != nil {
		slog.Debug("dashboard write", "err", err)
	}
}

func (s *Server) handleAPISessions(w http.ResponseWriter, r *http.Request) {
	snapshots := s.router.ListSessions()
	active, total := s.router.Stats()

	resp := map[string]any{
		"sessions": snapshots,
		"stats": map[string]any{
			"active":  active,
			"total":   total,
			"uptime":  time.Since(s.startedAt).Round(time.Second).String(),
			"backend": s.backendTag,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("encode sessions response", "err", err)
	}
}

func (s *Server) handleAPISessionEvents(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}

	sess := s.router.GetSession(key)
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	entries := sess.EventEntries()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		slog.Error("encode events response", "err", err)
	}
}
