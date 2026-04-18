package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"syscall"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
)

// DiscoveryHandlers groups the discovered-session and takeover API endpoints.
type DiscoveryHandlers struct {
	appCtx         context.Context // server lifecycle context, used for background cleanup
	discoveryCache *discoveryCache
	nodeAccess     NodeAccessor
	nodeCache      *node.CacheManager
	claudeDir      string
	router         *session.Router
	allowedRoot    string
	defaultAgent   session.AgentOpts // agents["general"]
	broadcast      func()            // hub.BroadcastSessionsUpdate
}

// GET /api/discovered — list discovered external CLI sessions.
func (h *DiscoveryHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	sessions := h.discoveryCache.snapshot()

	// Merge remote discovered sessions
	if h.nodeAccess.HasNodes() {
		for i := range sessions {
			sessions[i].Node = "local"
		}
		cachedDiscovered := h.nodeCache.Discovered()
		allDiscovered := make([]any, 0, len(sessions))
		for _, d := range sessions {
			allDiscovered = append(allDiscovered, d)
		}
		for _, items := range cachedDiscovered {
			for _, item := range items {
				allDiscovered = append(allDiscovered, item)
			}
		}
		writeJSON(w, allDiscovered)
		return
	}

	writeJSON(w, sessions)
}

// GET /api/discovered/preview — preview a discovered session's history.
func (h *DiscoveryHandlers) handlePreview(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	nodeID := r.URL.Query().Get("node")
	if sessionID == "" || !discovery.IsValidSessionID(sessionID) {
		writeJSON(w, []any{})
		return
	}

	// Remote node — only fall through to local when nodeID is empty or "local".
	if nodeID != "" && nodeID != "local" {
		nc, ok := h.nodeAccess.GetNode(nodeID)
		if !ok {
			slog.Warn("remote discovered preview: node not connected", "node", nodeID)
			http.Error(w, "node not connected", http.StatusBadGateway)
			return
		}
		entries, err := nc.FetchDiscoveredPreview(r.Context(), sessionID)
		if err != nil {
			slog.Warn("remote discovered preview", "node", nodeID, "err", err)
			entries = nil
		}
		if entries == nil {
			entries = []cli.EventEntry{}
		}
		writeJSON(w, entries)
		return
	}

	// Local
	if h.claudeDir == "" {
		writeJSON(w, []any{})
		return
	}

	entries, err := discovery.LoadHistory(h.claudeDir, sessionID)
	if err != nil {
		slog.Warn("preview load history", "session_id", sessionID, "err", err)
		entries = nil
	}
	if entries == nil {
		entries = []cli.EventEntry{}
	}

	writeJSON(w, entries)
}

// POST /api/discovered/takeover — kill an external CLI process and resume its session.
func (h *DiscoveryHandlers) handleTakeover(w http.ResponseWriter, r *http.Request) {
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
		nc, ok := h.nodeAccess.LookupNode(w, req.Node)
		if !ok {
			return
		}
		remoteKey, err := nc.ProxyTakeover(r.Context(), req.PID, req.SessionID, req.CWD, req.ProcStartTime)
		if err != nil {
			slog.Warn("proxy takeover failed", "node", req.Node, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "accepted", "key": remoteKey, "node": req.Node})
		return
	}

	// Verify PID is in the discovered list before killing.
	// Use cache snapshot — fresh Scan() filters out dead processes.
	if h.claudeDir != "" {
		cached := h.discoveryCache.snapshot()
		pidFound := false
		for _, d := range cached {
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
	// Validate CWD against allowedRoot to prevent sessions running in arbitrary directories.
	if cwd != "unknown" {
		cwd = filepath.Clean(cwd)
		if !filepath.IsAbs(cwd) {
			http.Error(w, "cwd must be absolute path", http.StatusBadRequest)
			return
		}
		if h.allowedRoot != "" {
			if _, err := validateWorkspace(cwd, h.allowedRoot); err != nil {
				http.Error(w, "cwd outside allowed root", http.StatusBadRequest)
				return
			}
		}
	}
	cwdKey := session.SanitizeCWDKey(cwd)
	key := session.TakeoverKey(cwdKey)

	// Kill the original process.
	// Verify PID identity before sending signal (TOCTOU guard).
	if req.ProcStartTime == 0 {
		http.Error(w, "proc_start_time is required", http.StatusBadRequest)
		return
	}
	alive := osutil.PidAlive(req.PID)
	if alive {
		if !verifyProcIdentity(req.PID, req.ProcStartTime) {
			http.Error(w, "process identity changed (PID reused)", http.StatusConflict)
			return
		}
		if err := syscall.Kill(req.PID, syscall.SIGTERM); err != nil {
			if !errors.Is(err, syscall.ESRCH) {
				slog.Error("failed to terminate process", "pid", req.PID, "err", err)
				http.Error(w, "failed to terminate process", http.StatusInternalServerError)
				return
			}
		}
	}

	// Immediately remove the killed PID from the discovery cache so the
	// frontend's fetchSessions() call (triggered right after the 202 response)
	// won't see the stale entry and re-render the old card in the sidebar.
	h.discoveryCache.evictPID(req.PID)

	// Capture locals for the background goroutine.
	pid := req.PID
	sessionID := req.SessionID
	reqCWD := req.CWD
	procStartTime := req.ProcStartTime
	agentOpts := h.defaultAgent

	broadcast := h.broadcast
	claudeDir := h.claudeDir
	router := h.router

	go func() {
		// Wait, SIGKILL, and remove stale session files.
		discovery.WaitAndCleanup(h.appCtx, pid, procStartTime, claudeDir, reqCWD, sessionID)

		// Takeover via router — use Background context so the spawned process
		// outlives the HTTP request.
		_, err := router.Takeover(h.appCtx, key, sessionID, cwd, session.AgentOpts{
			Model:     agentOpts.Model,
			ExtraArgs: agentOpts.ExtraArgs,
		})
		if err != nil {
			slog.Error("session takeover failed", "key", key, "session_id", sessionID, "pid", pid, "err", err)
			if broadcast != nil {
				broadcast()
			}
			return
		}

		slog.Info("session takeover", "key", key, "session_id", sessionID, "pid", pid, "cwd", cwd)
		if broadcast != nil {
			broadcast()
		}
	}()

	writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "accepted", "key": key})
}

// POST /api/discovered/close — kill an external CLI process without resuming its session.
func (h *DiscoveryHandlers) handleClose(w http.ResponseWriter, r *http.Request) {
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
	if req.PID <= 0 {
		http.Error(w, "pid is required", http.StatusBadRequest)
		return
	}

	// Remote node proxy
	if req.Node != "" && req.Node != "local" {
		nc, ok := h.nodeAccess.LookupNode(w, req.Node)
		if !ok {
			return
		}
		if err := nc.ProxyCloseDiscovered(r.Context(), req.PID, req.SessionID, req.CWD, req.ProcStartTime); err != nil {
			slog.Warn("proxy close discovered failed", "node", req.Node, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}

	// Verify PID is in the discovered list before killing.
	// Use the cache snapshot instead of a fresh Scan(), because Scan()
	// filters out dead processes — if the process was already killed
	// externally the fresh scan won't find it, but we still need to
	// clean up the stale entry.
	if h.claudeDir == "" {
		http.Error(w, "discovery not available", http.StatusServiceUnavailable)
		return
	}
	cached := h.discoveryCache.snapshot()
	var found *discovery.DiscoveredSession
	for i := range cached {
		if cached[i].PID == req.PID {
			found = &cached[i]
			break
		}
	}
	if found == nil {
		http.Error(w, "pid not found in discovered sessions", http.StatusBadRequest)
		return
	}
	// Use the cached SessionID/CWD for cleanup so a caller cannot
	// supply a crafted value to delete arbitrary session files.
	sessionID := found.SessionID
	cwd := found.CWD

	if req.ProcStartTime == 0 {
		http.Error(w, "proc_start_time is required", http.StatusBadRequest)
		return
	}

	// If the process is already dead, skip identity check and signal —
	// just do cleanup.  Otherwise verify PID identity and send SIGTERM.
	alive := osutil.PidAlive(req.PID)
	if alive {
		if !verifyProcIdentity(req.PID, req.ProcStartTime) {
			http.Error(w, "process identity changed (PID reused)", http.StatusConflict)
			return
		}
		if err := syscall.Kill(req.PID, syscall.SIGTERM); err != nil {
			// ESRCH = process disappeared between alive check and kill — treat as success.
			if !errors.Is(err, syscall.ESRCH) {
				slog.Error("failed to terminate process", "pid", req.PID, "err", err)
				http.Error(w, "failed to terminate process", http.StatusInternalServerError)
				return
			}
		}
	} else {
		slog.Info("discovered session already dead, cleaning up", "pid", req.PID)
	}

	// Evict from cache immediately so the frontend won't see the stale entry.
	h.discoveryCache.evictPID(req.PID)

	// Background cleanup: wait for exit, SIGKILL if stuck, remove stale files.
	pid := req.PID
	procStartTime := req.ProcStartTime
	claudeDir := h.claudeDir
	broadcast := h.broadcast

	go func() {
		discovery.WaitAndCleanup(h.appCtx, pid, procStartTime, claudeDir, cwd, sessionID)
		slog.Info("discovered session closed", "pid", pid, "session_id", sessionID)
		if broadcast != nil {
			broadcast()
		}
	}()

	writeJSON(w, map[string]string{"status": "ok"})
}
