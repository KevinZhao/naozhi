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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

//go:embed static/dashboard.html
var dashboardHTML embed.FS

//go:embed static/manifest.json
var manifestJSON embed.FS

func (s *Server) registerDashboard() {
	s.hub = NewHub(s.router, s.agents, s.agentCommands, s.dashboardToken, s.sessionGuard, s.nodes, &s.nodesMu, s.projectMgr)

	// Push session list changes to WS clients
	s.router.SetOnChange(func() { s.hub.BroadcastSessionsUpdate() })

	// Push cron execution results to WS clients
	if s.scheduler != nil {
		s.scheduler.SetOnExecute(func(jobID, result, errMsg string) {
			s.hub.BroadcastCronResult(jobID, result, errMsg)
		})
	}

	s.mux.HandleFunc("GET /api/sessions", s.handleAPISessions)
	s.mux.HandleFunc("GET /api/sessions/events", s.handleAPISessionEvents)
	s.mux.HandleFunc("POST /api/sessions/send", s.handleAPISend)
	s.mux.HandleFunc("DELETE /api/sessions", s.handleAPISessionDelete)
	s.mux.HandleFunc("GET /api/discovered", s.handleAPIDiscovered)
	s.mux.HandleFunc("GET /api/discovered/preview", s.handleAPIDiscoveredPreview)
	s.mux.HandleFunc("POST /api/discovered/takeover", s.handleAPITakeover)
	s.mux.HandleFunc("GET /api/projects", s.handleAPIProjects)
	s.mux.HandleFunc("GET /api/projects/config", s.handleAPIProjectConfigGet)
	s.mux.HandleFunc("PUT /api/projects/config", s.handleAPIProjectConfigPut)
	s.mux.HandleFunc("POST /api/projects/planner/restart", s.handleAPIProjectPlannerRestart)
	s.mux.HandleFunc("POST /api/transcribe", s.handleAPITranscribe)
	s.mux.HandleFunc("GET /api/cron", s.handleAPICronList)
	s.mux.HandleFunc("POST /api/cron", s.handleAPICronCreate)
	s.mux.HandleFunc("DELETE /api/cron", s.handleAPICronDelete)
	s.mux.HandleFunc("POST /api/cron/pause", s.handleAPICronPause)
	s.mux.HandleFunc("POST /api/cron/resume", s.handleAPICronResume)
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /manifest.json", s.handleManifest)
	s.mux.HandleFunc("GET /ws", s.hub.HandleUpgrade)
	if s.reverseNodeServer != nil {
		s.mux.Handle("GET /ws-node", s.reverseNodeServer)
	}
}

// isAuthenticated checks auth without writing an error response. Used by
// endpoints that serve partial data to unauthenticated callers (e.g. /health).
func (s *Server) isAuthenticated(r *http.Request) bool {
	if s.dashboardToken == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	return strings.HasPrefix(auth, "Bearer ") && subtle.ConstantTimeCompare([]byte(token), []byte(s.dashboardToken)) == 1
}

// checkBearerAuth validates the dashboard API token. Returns true if authorized.
func (s *Server) checkBearerAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.isAuthenticated(r) {
		return true
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// No Bearer auth here: browser GET has no Authorization header yet.
	// JS in the page handles auth for API calls; the HTML shell is public.
	data, err := dashboardHTML.ReadFile("static/dashboard.html")
	if err != nil {
		http.Error(w, "dashboard not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; connect-src 'self' wss: ws:; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
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

func (s *Server) handleAPISessions(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	snapshots := s.router.ListSessions()

	// Fill project field from ProjectManager
	if s.projectMgr != nil {
		// Collect unique workspace paths for batch resolution (single lock acquisition)
		var workspaces []string
		for i := range snapshots {
			if !project.IsPlannerKey(snapshots[i].Key) && snapshots[i].Workspace != "" {
				workspaces = append(workspaces, snapshots[i].Workspace)
			}
		}
		wsMap := s.projectMgr.ResolveWorkspaces(workspaces)

		for i := range snapshots {
			if project.IsPlannerKey(snapshots[i].Key) {
				parts := strings.SplitN(snapshots[i].Key, ":", 3)
				if len(parts) == 3 {
					snapshots[i].Project = parts[1]
					snapshots[i].IsPlanner = true
				}
			} else if name := wsMap[snapshots[i].Workspace]; name != "" {
				snapshots[i].Project = name
			}
		}
	}

	active, total := s.router.Stats()

	var running, ready int
	for _, snap := range snapshots {
		switch snap.State {
		case "running":
			running++
		case "ready":
			ready++
		}
	}

	stats := map[string]any{
		"active":            active,
		"running":           running,
		"ready":             ready,
		"total":             total,
		"uptime":            time.Since(s.startedAt).Round(time.Second).String(),
		"backend":           s.backendTag,
		"max_procs":         s.router.MaxProcs(),
		"default_workspace": s.router.DefaultWorkspace(),
		"workspace_id":      s.workspaceID,
		"workspace_name":    s.workspaceName,
		"system":            systemInfo(),
		"watchdog": map[string]any{
			"no_output_kills": s.watchdogNoOutputKills.Load(),
			"total_kills":     s.watchdogTotalKills.Load(),
		},
	}

	// Include available agent IDs for dashboard session creation
	agentIDs := make([]string, 0, len(s.agents)+1)
	agentIDs = append(agentIDs, "general")
	for id := range s.agents {
		agentIDs = append(agentIDs, id)
	}
	stats["agents"] = agentIDs

	// Include project list for dashboard sidebar rendering
	var projectList []map[string]string
	if s.projectMgr != nil {
		projects := s.projectMgr.All()
		for _, p := range projects {
			projectList = append(projectList, map[string]string{"name": p.Name, "path": p.Path, "node": "local"})
		}
	}
	// Merge remote projects (always, even without a local project manager)
	s.nodesMu.RLock()
	hasNodes := len(s.nodes) > 0
	s.nodesMu.RUnlock()
	if hasNodes {
		cachedProjects := s.nodeCache.Projects()
		for _, items := range cachedProjects {
			for _, item := range items {
				name := strOrFallback(item, "name", "Name")
				path := strOrFallback(item, "path", "Path")
				node, _ := item["node"].(string)
				if name != "" {
					projectList = append(projectList, map[string]string{"name": name, "path": path, "node": node})
				}
			}
		}
	}
	if len(projectList) > 0 {
		stats["projects"] = projectList
	}

	// Take a snapshot of nodes under lock for thread-safe access
	s.nodesMu.RLock()
	nodesSnapshot := make(map[string]NodeConn, len(s.nodes))
	for k, v := range s.nodes {
		nodesSnapshot[k] = v
	}
	s.nodesMu.RUnlock()

	// No configured nodes at all: use simple single-node response format
	if len(s.knownNodes) == 0 {
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

	// Multi-node: tag local sessions and merge with cached remote sessions
	allSessions := make([]any, 0, len(snapshots))
	for i := range snapshots {
		snapshots[i].Node = "local"
		allSessions = append(allSessions, snapshots[i])
	}

	localName := s.workspaceName
	if localName == "" {
		localName = "Local"
	}
	nodeStatus := map[string]any{
		"local": map[string]any{"display_name": localName, "status": "ok"},
	}

	cachedSessions, cachedStatus := s.nodeCache.Sessions()
	for id, nc := range nodesSnapshot {
		status := cachedStatus[id]
		if status == "" {
			status = "ok"
		}
		nodeStatus[id] = map[string]any{
			"display_name": nc.DisplayName(),
			"status":       status,
			"remote_addr":  nc.RemoteAddr(),
		}
		for _, rs := range cachedSessions[id] {
			allSessions = append(allSessions, rs)
		}
	}

	// Always include all configured nodes, even when currently disconnected.
	for id, displayName := range s.knownNodes {
		if _, connected := nodeStatus[id]; !connected {
			nodeStatus[id] = map[string]any{
				"display_name": displayName,
				"status":       "offline",
			}
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
	if !s.checkBearerAuth(w, r) {
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}

	// Remote node proxy
	node := r.URL.Query().Get("node")
	if node != "" && node != "local" {
		nc, ok := s.lookupNode(w, node)
		if !ok {
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
			slog.Warn("remote fetch events failed", "node", node, "key", key, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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

	var key, text, node, workspace string
	var images []cli.ImageData

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, 55<<20) // 5 files × 10MB + form overhead
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "bad multipart form", http.StatusBadRequest)
			return
		}
		key = r.FormValue("key")
		text = r.FormValue("text")
		node = r.FormValue("node")
		workspace = r.FormValue("workspace")

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
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB limit
		var req struct {
			Key       string `json:"key"`
			Text      string `json:"text"`
			Node      string `json:"node"`
			Workspace string `json:"workspace"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		key = req.Key
		text = req.Text
		node = req.Node
		workspace = req.Workspace
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
			s.hub.BroadcastSessionsUpdate()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"key": key, "status": "reset"})
		return
	}

	// Remote node proxy
	if node != "" && node != "local" {
		nc, ok := s.lookupNode(w, node)
		if !ok {
			return
		}
		capturedKey, capturedText, capturedWorkspace := key, text, workspace
		go func() {
			ctx := context.Background()
			if err := nc.Send(ctx, capturedKey, capturedText, capturedWorkspace); err != nil {
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

	// Set workspace override for new dashboard sessions
	if workspace != "" {
		if info, err := os.Stat(workspace); err == nil && info.IsDir() {
			// Enforce same allowedRoot check as /cd command
			wsPath := filepath.Clean(workspace)
			if resolved, err := filepath.EvalSymlinks(wsPath); err == nil {
				wsPath = resolved
			}
			if s.allowedRoot != "" && wsPath != s.allowedRoot && !strings.HasPrefix(wsPath, s.allowedRoot+string(filepath.Separator)) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]string{"error": "workspace outside allowed root"})
				return
			}
			if idx := strings.LastIndexByte(key, ':'); idx >= 0 {
				chatKey := key[:idx]
				s.router.SetWorkspace(chatKey, wsPath)
			}
		}
	}

	if !s.sessionGuard.TryAcquire(key) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "session busy"}); err != nil {
			slog.Error("encode busy response", "err", err)
		}
		return
	}

	capturedText := text
	capturedImages := images
	go func() {
		defer s.sessionGuard.Release(key)

		var ctx context.Context
		if s.hub != nil {
			ctx = s.hub.ctx
		} else {
			ctx = context.Background()
		}

		opts := buildSessionOpts(key, s.agents, s.projectMgr)
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

	sessions := s.discoveryCache.snapshot()

	// Merge remote discovered sessions
	s.nodesMu.RLock()
	hasDiscoveredNodes := len(s.nodes) > 0
	s.nodesMu.RUnlock()
	if hasDiscoveredNodes {
		for i := range sessions {
			sessions[i].Node = "local"
		}
		cachedDiscovered := s.nodeCache.Discovered()
		allDiscovered := make([]any, 0, len(sessions))
		for _, d := range sessions {
			allDiscovered = append(allDiscovered, d)
		}
		for _, items := range cachedDiscovered {
			for _, item := range items {
				allDiscovered = append(allDiscovered, item)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(allDiscovered); err != nil {
			slog.Error("encode discovered response", "err", err)
		}
		return
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
	nodeID := r.URL.Query().Get("node")
	if sessionID == "" || !discovery.IsValidSessionID(sessionID) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]any{})
		return
	}

	// Remote node
	if nodeID != "" {
		s.nodesMu.RLock()
		nc := s.nodes[nodeID]
		s.nodesMu.RUnlock()
		if nc != nil {
			entries, err := nc.FetchDiscoveredPreview(r.Context(), sessionID)
			if err != nil {
				slog.Warn("remote discovered preview", "node", nodeID, "err", err)
				entries = nil
			}
			if entries == nil {
				entries = []cli.EventEntry{}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(entries)
			return
		}
	}

	// Local
	if s.claudeDir == "" {
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
		PID           int    `json:"pid"`
		SessionID     string `json:"session_id"`
		CWD           string `json:"cwd"`
		ProcStartTime uint64 `json:"proc_start_time"`
		Node          string `json:"node"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.PID <= 0 || req.SessionID == "" || !discovery.IsValidSessionID(req.SessionID) {
		http.Error(w, "pid and session_id are required", http.StatusBadRequest)
		return
	}

	// Remote node proxy
	if req.Node != "" && req.Node != "local" {
		nc, ok := s.lookupNode(w, req.Node)
		if !ok {
			return
		}
		if err := nc.ProxyTakeover(r.Context(), req.PID, req.SessionID, req.CWD, req.ProcStartTime); err != nil {
			slog.Warn("proxy takeover failed", "node", req.Node, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
		return
	}

	// C2 fix: verify PID is in the discovered list before killing
	if s.claudeDir != "" {
		excludePIDs, excludeSIDs, excludeCWDs := s.router.ManagedExcludeSets()
		discovered, _ := discovery.Scan(s.claudeDir, excludePIDs, excludeSIDs, excludeCWDs)
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

	// Compute session key before launching goroutine so we can return it immediately.
	cwd := req.CWD
	if cwd == "" {
		cwd = "unknown"
	}
	cwdKey := strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-")
	key := "local:takeover:" + cwdKey + ":general"

	// Kill the original process.
	// Verify PID identity before sending signal (TOCTOU guard).
	if req.ProcStartTime == 0 {
		http.Error(w, "proc_start_time is required", http.StatusBadRequest)
		return
	}
	if !verifyProcIdentity(req.PID, req.ProcStartTime) {
		http.Error(w, "process identity changed (PID reused)", http.StatusConflict)
		return
	}
	if err := syscall.Kill(req.PID, syscall.SIGTERM); err != nil {
		http.Error(w, fmt.Sprintf("kill process %d: %v", req.PID, err), http.StatusBadRequest)
		return
	}

	// Capture locals for the background goroutine.
	pid := req.PID
	sessionID := req.SessionID
	reqCWD := req.CWD
	procStartTime := req.ProcStartTime
	agentOpts := s.agents["general"]

	go func() {
		// Wait, SIGKILL, and remove stale session files.
		discovery.WaitAndCleanup(pid, procStartTime, s.claudeDir, reqCWD, sessionID)

		// Takeover via router — use Background context so the spawned process
		// outlives the HTTP request.
		_, err := s.router.Takeover(context.Background(), key, sessionID, cwd, session.AgentOpts{
			Model:     agentOpts.Model,
			ExtraArgs: agentOpts.ExtraArgs,
		})
		if err != nil {
			slog.Error("session takeover failed", "key", key, "session_id", sessionID, "pid", pid, "err", err)
			if s.hub != nil {
				s.hub.BroadcastSessionsUpdate()
			}
			return
		}

		slog.Info("session takeover", "key", key, "session_id", sessionID, "pid", pid, "cwd", cwd)
		s.hub.BroadcastSessionsUpdate()
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "key": key})
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

// handleAPITranscribe accepts an audio file upload and returns transcribed text.
// POST /api/transcribe  (multipart/form-data, field "audio")
func (s *Server) handleAPITranscribe(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	if s.transcriber == nil {
		http.Error(w, "transcription not configured", http.StatusNotImplemented)
		return
	}

	const maxAudioSize = 10 << 20 // 10 MB
	r.Body = http.MaxBytesReader(w, r.Body, maxAudioSize+4096)
	if err := r.ParseMultipartForm(maxAudioSize); err != nil {
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()

	files := r.MultipartForm.File["audio"]
	if len(files) == 0 {
		http.Error(w, "missing audio field", http.StatusBadRequest)
		return
	}
	fh := files[0]

	f, err := fh.Open()
	if err != nil {
		http.Error(w, "failed to read audio", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "failed to read audio", http.StatusInternalServerError)
		return
	}

	mimeType := fh.Header.Get("Content-Type")
	text, err := s.transcriber.Transcribe(r.Context(), data, mimeType)
	if err != nil {
		slog.Warn("transcribe failed", "err", err, "mime", mimeType, "size", len(data))
		http.Error(w, "transcription failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"text": text})
}

// ─── Cron API handlers ──────────────────────────────────────────────────────

// GET /api/cron — list all cron jobs (unscoped, admin view).
func (s *Server) handleAPICronList(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	if s.scheduler == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jobs": []any{}})
		return
	}

	jobs := s.scheduler.ListAllJobs()
	type cronJobView struct {
		ID             string `json:"id"`
		Schedule       string `json:"schedule"`
		Prompt         string `json:"prompt"`
		Platform       string `json:"platform"`
		ChatID         string `json:"chat_id"`
		CreatedBy      string `json:"created_by,omitempty"`
		CreatedAt      int64  `json:"created_at"`
		Paused         bool   `json:"paused"`
		NotifyPlatform string `json:"notify_platform,omitempty"`
		NotifyChatID   string `json:"notify_chat_id,omitempty"`
		LastResult     string `json:"last_result,omitempty"`
		LastRunAt      int64  `json:"last_run_at,omitempty"`
		LastError      string `json:"last_error,omitempty"`
		NextRun        int64  `json:"next_run,omitempty"`
	}
	views := make([]cronJobView, 0, len(jobs))
	for _, j := range jobs {
		v := cronJobView{
			ID:             j.ID,
			Schedule:       j.Schedule,
			Prompt:         j.Prompt,
			Platform:       j.Platform,
			ChatID:         j.ChatID,
			CreatedBy:      j.CreatedBy,
			CreatedAt:      j.CreatedAt.UnixMilli(),
			Paused:         j.Paused,
			NotifyPlatform: j.NotifyPlatform,
			NotifyChatID:   j.NotifyChatID,
			LastResult:     j.LastResult,
			LastError:      j.LastError,
		}
		if !j.LastRunAt.IsZero() {
			v.LastRunAt = j.LastRunAt.UnixMilli()
		}
		if next := s.scheduler.NextRunByID(j.ID); !next.IsZero() {
			v.NextRun = next.UnixMilli()
		}
		views = append(views, v)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jobs": views})
}

// POST /api/cron — create a new cron job from dashboard.
func (s *Server) handleAPICronCreate(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	if s.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	var req struct {
		Schedule       string `json:"schedule"`
		Prompt         string `json:"prompt"`
		NotifyPlatform string `json:"notify_platform,omitempty"`
		NotifyChatID   string `json:"notify_chat_id,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16) // 64 KB
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Schedule == "" || req.Prompt == "" {
		http.Error(w, "schedule and prompt are required", http.StatusBadRequest)
		return
	}

	job := &cron.Job{
		Schedule:       req.Schedule,
		Prompt:         req.Prompt,
		Platform:       "dashboard",
		ChatID:         "global",
		CreatedBy:      "dashboard",
		NotifyPlatform: req.NotifyPlatform,
		NotifyChatID:   req.NotifyChatID,
	}
	if err := s.scheduler.AddJob(job); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	slog.Info("cron job created via dashboard", "id", job.ID, "schedule", job.Schedule)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"id": job.ID})
}

// DELETE /api/cron?id=xxx — delete a cron job by exact ID.
func (s *Server) handleAPICronDelete(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	if s.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	j, err := s.scheduler.DeleteJobByID(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	slog.Info("cron job deleted via dashboard", "id", j.ID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// POST /api/cron/pause — pause a cron job by exact ID.
func (s *Server) handleAPICronPause(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	if s.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KB
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	if _, err := s.scheduler.PauseJobByID(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	slog.Info("cron job paused via dashboard", "id", req.ID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// POST /api/cron/resume — resume a paused cron job by exact ID.
func (s *Server) handleAPICronResume(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	if s.scheduler == nil {
		http.Error(w, "cron not configured", http.StatusNotImplemented)
		return
	}

	var req struct {
		ID string `json:"id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KB
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	if _, err := s.scheduler.ResumeJobByID(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	slog.Info("cron job resumed via dashboard", "id", req.ID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
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
