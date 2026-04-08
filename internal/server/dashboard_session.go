package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
)

func (s *Server) handleAPISessions(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearerAuth(w, r) {
		return
	}
	snapshots := s.router.ListSessions()

	// Filter out suspended sessions from the API response.
	// They appear as resumable filesystem sessions via recent_sessions instead.
	// Only sessions with a running process are kept in the managed list.
	n := 0
	for _, snap := range snapshots {
		if snap.State == "suspended" {
			continue
		}
		snapshots[n] = snap
		n++
	}
	snapshots = snapshots[:n]

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

	// Fill summary from sessions-index.json for managed sessions
	if s.claudeDir != "" {
		sessionWorkspaces := make(map[string]string, len(snapshots))
		for _, snap := range snapshots {
			if snap.SessionID != "" && snap.Workspace != "" {
				sessionWorkspaces[snap.SessionID] = snap.Workspace
			}
		}
		summaryMap := discovery.LookupSummaries(s.claudeDir, sessionWorkspaces)
		for i := range snapshots {
			if summary := summaryMap[snapshots[i].SessionID]; summary != "" {
				snapshots[i].Summary = summary
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
		"version":           s.router.Version(),
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
	nodesSnapshot := make(map[string]node.Conn, len(s.nodes))
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
		recent, history := s.recentAndHistorySessions()
		if len(recent) > 0 {
			resp["recent_sessions"] = recent
		}
		if len(history) > 0 {
			resp["history_sessions"] = history
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
	recent, history := s.recentAndHistorySessions()
	if len(recent) > 0 {
		resp["recent_sessions"] = recent
	}
	if len(history) > 0 {
		resp["history_sessions"] = history
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
	writeJSON(w, map[string]string{"status": "ok"})
}

// recentAndHistorySessions returns filesystem sessions split into two groups:
//   - recent: last 24 hours (for resumable sidebar)
//   - history: 1-7 days (for history popover)
//
// Results are cached for 30 seconds to avoid repeated filesystem scans.
func (s *Server) recentAndHistorySessions() (recent, history []discovery.RecentSession) {
	if s.claudeDir == "" {
		return nil, nil
	}

	const cacheTTL = 30 * time.Second
	s.recentCacheMu.Lock()
	if time.Since(s.recentCacheTime) < cacheTTL {
		r, h := s.recentCache, s.historyCache
		s.recentCacheMu.Unlock()
		return r, h
	}
	s.recentCacheMu.Unlock()

	type result struct {
		recent  []discovery.RecentSession
		history []discovery.RecentSession
	}

	v, _, _ := s.recentFlight.Do("recent", func() (any, error) {
		excludeIDs := s.router.DiscoveryExcludeIDs()
		all := discovery.RecentSessions(s.claudeDir, 0, 7*24*time.Hour, excludeIDs)

		// Resolve project names in batch.
		if s.projectMgr != nil && len(all) > 0 {
			var workspaces []string
			for _, r := range all {
				workspaces = append(workspaces, r.Workspace)
			}
			wsMap := s.projectMgr.ResolveWorkspaces(workspaces)
			for i := range all {
				all[i].Project = wsMap[all[i].Workspace]
			}
		}

		// Split by time: < 24h → recent, 1-7d → history.
		cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()
		var rec, hist []discovery.RecentSession
		for _, rs := range all {
			if rs.LastActive >= cutoff {
				rec = append(rec, rs)
			} else {
				hist = append(hist, rs)
			}
		}

		s.recentCacheMu.Lock()
		s.recentCache = rec
		s.historyCache = hist
		s.recentCacheTime = time.Now()
		s.recentCacheMu.Unlock()

		return result{recent: rec, history: hist}, nil
	})

	if res, ok := v.(result); ok {
		return res.recent, res.history
	}
	return nil, nil
}
