package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"syscall"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/session"
)

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
		writeJSON(w, []any{})
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
			writeJSON(w, entries)
			return
		}
	}

	// Local
	if s.claudeDir == "" {
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, []any{})
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
	writeJSON(w, entries)
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
		writeJSON(w, map[string]string{"status": "accepted"})
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
	// Validate CWD against allowedRoot to prevent sessions running in arbitrary directories.
	if cwd != "unknown" && s.allowedRoot != "" {
		if _, err := validateWorkspace(cwd, s.allowedRoot); err != nil {
			http.Error(w, "cwd outside allowed root", http.StatusBadRequest)
			return
		}
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
		if s.hub != nil {
			s.hub.BroadcastSessionsUpdate()
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"status": "accepted", "key": key})
}
