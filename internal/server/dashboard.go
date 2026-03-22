package server

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

//go:embed static/dashboard.html
var dashboardHTML embed.FS

func (s *Server) registerDashboard() {
	s.mux.HandleFunc("GET /api/sessions", s.handleAPISessions)
	s.mux.HandleFunc("GET /api/sessions/events", s.handleAPISessionEvents)
	s.mux.HandleFunc("POST /api/sessions/send", s.handleAPISend)
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

	var entries []cli.EventEntry
	if afterStr := r.URL.Query().Get("after"); afterStr != "" {
		afterMS, err := strconv.ParseInt(afterStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid after parameter", http.StatusBadRequest)
			return
		}
		entries = sess.EventEntriesSince(afterMS)
	} else {
		entries = sess.EventEntries()
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		slog.Error("encode events response", "err", err)
	}
}

func (s *Server) handleAPISend(w http.ResponseWriter, r *http.Request) {
	// Optional bearer token auth
	if s.dashboardToken != "" {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != s.dashboardToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var key, text string
	var images []cli.ImageData

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "bad multipart form", http.StatusBadRequest)
			return
		}
		key = r.FormValue("key")
		text = r.FormValue("text")

		files := r.MultipartForm.File["files"]
		if len(files) > 5 {
			http.Error(w, "too many files (max 5)", http.StatusBadRequest)
			return
		}
		for _, fh := range files {
			if fh.Size > 10<<20 {
				http.Error(w, "file too large (max 10MB)", http.StatusBadRequest)
				return
			}
			f, err := fh.Open()
			if err != nil {
				http.Error(w, "open file: "+err.Error(), http.StatusBadRequest)
				return
			}
			data, readErr := io.ReadAll(f)
			f.Close()
			if readErr != nil {
				http.Error(w, "read file: "+readErr.Error(), http.StatusBadRequest)
				return
			}
			mime := fh.Header.Get("Content-Type")
			if !strings.HasPrefix(mime, "image/") {
				http.Error(w, "only image/* files are accepted", http.StatusBadRequest)
				return
			}
			images = append(images, cli.ImageData{Data: data, MimeType: mime})
		}
	} else {
		var req struct {
			Key  string `json:"key"`
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		key = req.Key
		text = req.Text
	}

	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	if text == "" && len(images) == 0 {
		http.Error(w, "text or files required", http.StatusBadRequest)
		return
	}

	if !s.sessionGuard.TryAcquire(key) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "session busy"}); err != nil {
			slog.Error("encode conflict response", "err", err)
		}
		return
	}

	capturedText := text
	capturedImages := images
	go func() {
		defer s.sessionGuard.Release(key)

		ctx := context.Background()

		parts := strings.SplitN(key, ":", 4)
		agentID := "general"
		if len(parts) == 4 {
			agentID = parts[3]
		}

		opts := s.agents[agentID]
		sess, _, err := s.router.GetOrCreate(ctx, key, opts)
		if err != nil {
			slog.Error("dashboard send: get session", "key", key, "err", err)
			return
		}

		if _, err := sess.Send(ctx, capturedText, capturedImages, nil); err != nil {
			slog.Error("dashboard send: send", "key", key, "err", err)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "key": key}); err != nil {
		slog.Error("encode accepted response", "err", err)
	}
}
