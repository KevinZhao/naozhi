package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/session"
)

var validSessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

//go:embed static/dashboard.html
var dashboardHTML embed.FS

func (s *Server) registerDashboard() {
	s.hub = NewHub(s.router, s.agents, s.agentCommands, s.dashboardToken, s.sessionGuard, s.nodes)

	// Push session list changes to WS clients
	s.router.SetOnChange(func() { s.hub.BroadcastSessionsUpdate() })

	s.mux.HandleFunc("GET /api/sessions", s.handleAPISessions)
	s.mux.HandleFunc("GET /api/sessions/events", s.handleAPISessionEvents)
	s.mux.HandleFunc("POST /api/sessions/send", s.handleAPISend)
	s.mux.HandleFunc("DELETE /api/sessions", s.handleAPISessionDelete)
	s.mux.HandleFunc("GET /api/discovered", s.handleAPIDiscovered)
	s.mux.HandleFunc("GET /api/discovered/preview", s.handleAPIDiscoveredPreview)
	s.mux.HandleFunc("POST /api/discovered/takeover", s.handleAPITakeover)
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /ws", s.hub.HandleUpgrade)
}

// checkBearerAuth validates the dashboard API token. Returns true if authorized.
func (s *Server) checkBearerAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.dashboardToken == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	if strings.HasPrefix(auth, "Bearer ") && subtle.ConstantTimeCompare([]byte(token), []byte(s.dashboardToken)) == 1 {
		return true
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
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

	stats := map[string]any{
		"active":  active,
		"total":   total,
		"uptime":  time.Since(s.startedAt).Round(time.Second).String(),
		"backend": s.backendTag,
	}

	// No remote nodes: use existing response format
	if len(s.nodes) == 0 {
		resp := map[string]any{
			"sessions": snapshots,
			"stats":    stats,
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			slog.Error("encode sessions response", "err", err)
		}
		return
	}

	// Multi-node: tag local sessions and merge with remote
	allSessions := make([]any, 0, len(snapshots))
	for i := range snapshots {
		snapshots[i].Node = "local"
		allSessions = append(allSessions, snapshots[i])
	}

	nodeStatus := map[string]any{
		"local": map[string]any{"display_name": "Local", "status": "ok"},
	}

	type fetchResult struct {
		nodeID   string
		sessions []map[string]any
		err      error
	}
	ch := make(chan fetchResult, len(s.nodes))
	for id, nc := range s.nodes {
		go func(id string, nc *NodeClient) {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			sessions, err := nc.FetchSessions(ctx)
			ch <- fetchResult{id, sessions, err}
		}(id, nc)
	}
	for i := 0; i < len(s.nodes); i++ {
		res := <-ch
		nc := s.nodes[res.nodeID]
		if res.err != nil {
			slog.Warn("fetch remote sessions", "node", res.nodeID, "err", res.err)
			nodeStatus[res.nodeID] = map[string]any{
				"display_name": nc.DisplayName,
				"status":       "error",
			}
			continue
		}
		nodeStatus[res.nodeID] = map[string]any{
			"display_name": nc.DisplayName,
			"status":       "ok",
		}
		for _, rs := range res.sessions {
			rs["node"] = res.nodeID
			allSessions = append(allSessions, rs)
		}
	}

	resp := map[string]any{
		"sessions": allSessions,
		"stats":    stats,
		"nodes":    nodeStatus,
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

	// Remote node proxy
	node := r.URL.Query().Get("node")
	if node != "" && node != "local" {
		nc, ok := s.nodes[node]
		if !ok {
			http.Error(w, "unknown node: "+node, http.StatusBadRequest)
			return
		}
		var after int64
		if afterStr := r.URL.Query().Get("after"); afterStr != "" {
			var parseErr error
			after, parseErr = strconv.ParseInt(afterStr, 10, 64)
			if parseErr != nil {
				http.Error(w, "invalid after parameter", http.StatusBadRequest)
				return
			}
		}
		entries, err := nc.FetchEvents(r.Context(), key, after)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(entries); err != nil {
			slog.Error("encode remote events response", "err", err)
		}
		return
	}

	// Local
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

func (s *Server) handleAPISessionDelete(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}

	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	if !s.router.Remove(req.Key) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleAPISend(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}

	var key, text, node string
	var images []cli.ImageData

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "bad multipart form", http.StatusBadRequest)
			return
		}
		key = r.FormValue("key")
		text = r.FormValue("text")
		node = r.FormValue("node")

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
			Node string `json:"node"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		key = req.Key
		text = req.Text
		node = req.Node
	}

	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	if text == "" && len(images) == 0 {
		http.Error(w, "text or files required", http.StatusBadRequest)
		return
	}

	// Handle /clear and /new — CLI built-in doesn't work in stream-json
	trimmed := strings.TrimSpace(text)
	if trimmed == "/clear" || trimmed == "/new" {
		s.router.Reset(key)
		if s.hub != nil {
			s.hub.broadcastState(key, "dead", "user_reset")
			s.hub.BroadcastSessionsUpdate()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"key": key, "status": "reset"})
		return
	}

	// Remote node proxy
	if node != "" && node != "local" {
		nc, ok := s.nodes[node]
		if !ok {
			http.Error(w, "unknown node: "+node, http.StatusBadRequest)
			return
		}
		capturedKey, capturedText := key, text
		go func() {
			ctx := context.Background()
			if err := nc.Send(ctx, capturedKey, capturedText); err != nil {
				slog.Error("remote send", "node", node, "key", capturedKey, "err", err)
			}
		}()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "key": key}); err != nil {
			slog.Error("encode accepted response", "err", err)
		}
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

func (s *Server) handleAPIDiscovered(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	if s.claudeDir == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]any{})
		return
	}

	excludePIDs := s.router.ManagedPIDs()
	sessions, err := discovery.Scan(s.claudeDir, excludePIDs)
	if err != nil {
		slog.Warn("discovery scan", "err", err)
		sessions = nil
	}
	if sessions == nil {
		sessions = []discovery.DiscoveredSession{}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(sessions); err != nil {
		slog.Error("encode discovered response", "err", err)
	}
}

func (s *Server) handleAPIDiscoveredPreview(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" || s.claudeDir == "" || !validSessionID.MatchString(sessionID) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]any{})
		return
	}

	entries, err := discovery.LoadHistory(s.claudeDir, sessionID)
	if err != nil {
		slog.Warn("preview load history", "session_id", sessionID, "err", err)
		entries = nil
	}
	if entries == nil {
		entries = []cli.EventEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

func (s *Server) handleAPITakeover(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}

	var req struct {
		PID       int    `json:"pid"`
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.PID <= 0 || req.SessionID == "" || !validSessionID.MatchString(req.SessionID) {
		http.Error(w, "pid and session_id are required", http.StatusBadRequest)
		return
	}

	// C2 fix: verify PID is in the discovered list before killing
	if s.claudeDir != "" {
		excludePIDs := s.router.ManagedPIDs()
		discovered, _ := discovery.Scan(s.claudeDir, excludePIDs)
		pidFound := false
		for _, d := range discovered {
			if d.PID == req.PID && d.SessionID == req.SessionID {
				pidFound = true
				break
			}
		}
		if !pidFound {
			http.Error(w, "pid not found in discovered sessions", http.StatusBadRequest)
			return
		}
	}

	// Kill the original process
	if err := syscall.Kill(req.PID, syscall.SIGTERM); err != nil {
		http.Error(w, fmt.Sprintf("kill process %d: %v", req.PID, err), http.StatusBadRequest)
		return
	}

	// Wait for process to exit (up to 5 seconds)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(req.PID, 0); err != nil {
			break // process exited
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Force kill if still alive
	_ = syscall.Kill(req.PID, syscall.SIGKILL)

	// Remove stale session file so it won't reappear in discovery scans
	if s.claudeDir != "" {
		staleFile := filepath.Join(s.claudeDir, "sessions", fmt.Sprintf("%d.json", req.PID))
		_ = os.Remove(staleFile)
	}

	// Remove session lock dir from /tmp so --resume can reacquire the session.
	// Claude CLI creates /tmp/claude-{UID}/{encoded-cwd}/{sessionID}/ and doesn't
	// clean up after SIGKILL.
	if req.CWD != "" && req.SessionID != "" {
		encodedCWD := strings.ReplaceAll(req.CWD, "/", "-")
		lockDir := filepath.Join(os.TempDir(), fmt.Sprintf("claude-%d", os.Getuid()), encodedCWD, req.SessionID)
		_ = os.RemoveAll(lockDir)
	}

	// Generate session key using full CWD to avoid collisions
	// between directories with the same base name.
	cwd := req.CWD
	if cwd == "" {
		cwd = "unknown"
	}
	cwdKey := strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-")
	key := "local:takeover:" + cwdKey + ":general"

	// Takeover via router — use Background context so the spawned process
	// outlives the HTTP request.
	opts := s.agents["general"]
	sess, err := s.router.Takeover(context.Background(), key, req.SessionID, cwd, session.AgentOpts{
		Model:     opts.Model,
		ExtraArgs: opts.ExtraArgs,
	})
	if err != nil {
		http.Error(w, "takeover failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	slog.Info("session takeover", "key", key, "session_id", req.SessionID, "pid", req.PID, "cwd", cwd)

	// Load conversation history from Claude's local JSONL
	if s.claudeDir != "" {
		if entries, err := discovery.LoadHistory(s.claudeDir, req.SessionID, cwd); err == nil && len(entries) > 0 {
			sess.InjectHistory(entries)
			slog.Info("loaded session history", "key", key, "entries", len(entries))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "key": key})
}
