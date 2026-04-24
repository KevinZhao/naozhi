package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// maxResumeLastPromptBytes caps the last_prompt field on /api/sessions/resume.
// The body-level MaxBytesReader is 1 MiB; this field-level cap prevents a
// megabyte-scale string from being persisted on the session and then echoed
// to every dashboard client on each /api/sessions poll.
const maxResumeLastPromptBytes = 2 * 1024

// isUnknownRPCMethodErr reports whether a remote-proxy error came from the
// peer node rejecting the RPC method name. That happens when the peer is
// running an older naozhi binary that predates remove_session /
// interrupt_session — surfacing a bespoke 409 lets the dashboard show a
// precise "upgrade the remote node" toast instead of a generic 502. The
// match is on error text because the reverse-RPC error is wrapped via
// fmt.Errorf in multiple layers and carries the literal "unknown method: "
// prefix from internal/upstream/connector.go's default switch branch.
func isUnknownRPCMethodErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown method")
}

// SessionHandlers groups the session list, events, delete, and resume API endpoints.
type SessionHandlers struct {
	router      *session.Router
	projectMgr  *project.Manager
	claudeDir   string
	allowedRoot string
	agents      map[string]session.AgentOpts
	// agentIDs is the precomputed list of agent IDs surfaced in /api/sessions.
	// Built once at construction (agents map is immutable after startup) so the
	// dashboard poll handler avoids allocating + filling this slice on each hit.
	agentIDs   []string
	nodeAccess NodeAccessor
	nodeCache  *node.CacheManager

	// Static status fields (immutable after construction)
	startedAt     time.Time
	backendTag    string
	workspaceID   string
	workspaceName string
	watchdogNoOut *atomic.Int64
	watchdogTotal *atomic.Int64

	// History cache (30s TTL)
	historyCache     []discovery.RecentSession
	historyCacheTime time.Time
	historyCacheMu   sync.Mutex
	historyFlight    singleflight.Group

	// Summary cache (30s TTL) — avoids re-running discovery.LookupSummaries
	// (N os.Stat + package-level lock) on every GET /api/sessions poll.
	summaryCache     map[string]string
	summaryCacheTime time.Time
	summaryCacheMu   sync.Mutex
}

// GET /api/sessions
func (h *SessionHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	snapshots := h.router.ListSessions()

	// Keep dead sessions in the workspace sidebar for up to 24 hours.
	cutoff24h := time.Now().Add(-24 * time.Hour).UnixMilli()
	n := 0
	for _, snap := range snapshots {
		if snap.DeathReason != "" && snap.LastActive < cutoff24h {
			continue
		}
		snapshots[n] = snap
		n++
	}
	snapshots = snapshots[:n]

	// Fill project field from ProjectManager
	if h.projectMgr != nil {
		var workspaces []string
		for i := range snapshots {
			if !project.IsPlannerKey(snapshots[i].Key) && snapshots[i].Workspace != "" {
				workspaces = append(workspaces, snapshots[i].Workspace)
			}
		}
		wsMap := h.projectMgr.ResolveWorkspaces(workspaces)

		for i := range snapshots {
			if project.IsPlannerKey(snapshots[i].Key) {
				// Planner keys look like "planner:{name}:{agent}". SplitN
				// allocates a []string per poll; use IndexByte twice for
				// zero-alloc extraction of the middle segment.
				key := snapshots[i].Key
				const plannerPrefix = "planner:"
				if len(key) > len(plannerPrefix) {
					rest := key[len(plannerPrefix):]
					if j := strings.IndexByte(rest, ':'); j > 0 {
						snapshots[i].Project = rest[:j]
						snapshots[i].IsPlanner = true
					}
				}
			} else if name := wsMap[snapshots[i].Workspace]; name != "" {
				snapshots[i].Project = name
			}
		}
	}

	// Fill summary from sessions-index.json for managed sessions
	if h.claudeDir != "" {
		summaryMap := h.lookupSummariesCached(snapshots)
		for i := range snapshots {
			if summary := summaryMap[snapshots[i].SessionID]; summary != "" {
				snapshots[i].Summary = summary
			}
		}
	}

	active, total := h.router.Stats()

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
		"version":           h.router.Version(),
		"uptime":            time.Since(h.startedAt).Round(time.Second).String(),
		"backend":           h.backendTag,
		"cli_name":          h.router.CLIName(),
		"cli_version":       h.router.CLIVersion(),
		"max_procs":         h.router.MaxProcs(),
		"default_workspace": h.router.DefaultWorkspace(),
		"workspace_id":      h.workspaceID,
		"workspace_name":    h.workspaceName,
		"system":            systemInfo(),
		"watchdog": map[string]any{
			"no_output_kills": h.watchdogNoOut.Load(),
			"total_kills":     h.watchdogTotal.Load(),
		},
	}

	// Include available agent IDs for dashboard session creation. Cached at
	// construction (agents map is immutable after startup) to skip the make +
	// fill on every poll. See SessionHandlers.agentIDs.
	stats["agents"] = h.agentIDs

	// Include project list for dashboard sidebar rendering.
	// Pre-allocate the outer slice so the append loop doesn't trigger log(N)
	// growth reallocs on projects-heavy dashboards.
	var projectList []map[string]any
	if h.projectMgr != nil {
		projects := h.projectMgr.All()
		projectList = make([]map[string]any, 0, len(projects))
		for _, p := range projects {
			projectList = append(projectList, map[string]any{
				"name":     p.Name,
				"path":     p.Path,
				"node":     "local",
				"favorite": p.Config.Favorite,
				// Strip embedded userinfo (PAT) before handing the URL to any
				// dashboard client. Round 46 redacted /api/projects but missed
				// this path — /api/sessions is polled every few seconds, so
				// the leak is actually larger here.
				"git_remote_url": redactGitRemoteURL(p.GitRemoteURL),
				"github":         p.IsGitHub,
			})
		}
	}
	// Merge remote projects (always, even without a local project manager)
	if h.nodeAccess.HasNodes() {
		cachedProjects := h.nodeCache.Projects()
		for _, items := range cachedProjects {
			for _, item := range items {
				name := strOrFallback(item, "name", "Name")
				path := strOrFallback(item, "path", "Path")
				nd, _ := item["node"].(string)
				if name == "" {
					continue
				}
				entry := map[string]any{"name": name, "path": path, "node": nd}
				if v, ok := item["favorite"].(bool); ok {
					entry["favorite"] = v
				}
				// Remote node may be running an older binary that hasn't
				// redacted the URL yet — always run the redactor on data
				// forwarded via the node cache so credentials never leak
				// even if a peer node is behind on patches.
				if v, ok := item["git_remote_url"].(string); ok && v != "" {
					entry["git_remote_url"] = redactGitRemoteURL(v)
				}
				if v, ok := item["github"].(bool); ok {
					entry["github"] = v
				}
				projectList = append(projectList, entry)
			}
		}
	}
	if len(projectList) > 0 {
		stats["projects"] = projectList
	}

	// Take a snapshot of nodes under lock for thread-safe access
	nodesSnapshot := h.nodeAccess.NodesSnapshot()

	// No configured nodes at all: use simple single-node response format
	if len(h.nodeAccess.KnownNodes()) == 0 {
		resp := map[string]any{
			"sessions": snapshots,
			"stats":    stats,
		}
		history := h.historySessions()
		if len(history) > 0 {
			resp["history_sessions"] = history
		}
		writeJSON(w, resp)
		return
	}

	// Multi-node: tag local sessions and merge with cached remote sessions
	allSessions := make([]any, 0, len(snapshots))
	for i := range snapshots {
		snapshots[i].Node = "local"
		allSessions = append(allSessions, snapshots[i])
	}

	localName := h.workspaceName
	if localName == "" {
		localName = "Local"
	}
	nodeStatus := map[string]any{
		"local": map[string]any{"display_name": localName, "status": "ok"},
	}

	cachedSessions, cachedStatus := h.nodeCache.Sessions()
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
	for id, displayName := range h.nodeAccess.KnownNodes() {
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
	history := h.historySessions()
	if len(history) > 0 {
		resp["history_sessions"] = history
	}
	writeJSON(w, resp)
}

// maxEventsPageLimit caps the per-request history slice so a malicious or
// confused client can't force a full ring-buffer dump via ?limit=10000.
// 500 matches maxPersistedHistory — the upper bound of anything useful.
const maxEventsPageLimit = 500

// GET /api/sessions/events
//
// Query parameters:
//   - key       (required): session key
//   - node      (optional): remote node ID (proxy to that node)
//   - after     (optional, ms): incremental fetch — entries with Time > after
//   - before    (optional, ms): pagination fetch — entries with Time < before,
//     returning up to `limit` in chronological order
//   - limit     (optional): caps the result count. Clamped at maxEventsPageLimit.
//
// Precedence: `after` wins over `before` (streaming catch-up outranks
// pagination). No params = full history (legacy behaviour).
func (h *SessionHandlers) handleEvents(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}

	q := r.URL.Query()
	afterStr := q.Get("after")
	beforeStr := q.Get("before")
	limitStr := q.Get("limit")

	var (
		after  int64
		before int64
		limit  int
	)
	if afterStr != "" {
		v, err := strconv.ParseInt(afterStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid after parameter", http.StatusBadRequest)
			return
		}
		after = v
	}
	if beforeStr != "" {
		v, err := strconv.ParseInt(beforeStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid before parameter", http.StatusBadRequest)
			return
		}
		before = v
	}
	if limitStr != "" {
		v, err := strconv.Atoi(limitStr)
		if err != nil || v < 0 {
			http.Error(w, "invalid limit parameter", http.StatusBadRequest)
			return
		}
		if v > maxEventsPageLimit {
			v = maxEventsPageLimit
		}
		limit = v
	}

	// Remote node proxy — forward `after` only (the remote protocol predates
	// before/limit). Cap the returned slice locally so the dashboard gets a
	// consistent-size payload even from legacy peers.
	nodeID := q.Get("node")
	if nodeID != "" && nodeID != "local" {
		nc, ok := h.nodeAccess.LookupNode(w, nodeID)
		if !ok {
			return
		}
		entries, err := nc.FetchEvents(r.Context(), key, after)
		if err != nil {
			slog.Warn("remote fetch events failed", "node", nodeID, "key", key, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		if limit > 0 && len(entries) > limit {
			entries = entries[len(entries)-limit:]
		}
		writeJSON(w, entries)
		return
	}

	// Local
	sess := h.router.GetSession(key)
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var entries []cli.EventEntry
	switch {
	case afterStr != "":
		entries = sess.EventEntriesSince(after)
		if limit > 0 && len(entries) > limit {
			// Preserve the newest on a full catch-up so the client doesn't
			// miss events it just streamed through.
			entries = entries[len(entries)-limit:]
		}
	case beforeStr != "" || limit > 0:
		pageLimit := limit
		if pageLimit == 0 {
			pageLimit = maxEventsPageLimit
		}
		entries = sess.EventEntriesBefore(before, pageLimit)
	default:
		entries = sess.EventEntries()
	}

	writeJSON(w, entries)
}

// DELETE /api/sessions
func (h *SessionHandlers) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key  string `json:"key"`
		Node string `json:"node"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	// Remote node proxy
	if req.Node != "" && req.Node != "local" {
		nc, ok := h.nodeAccess.LookupNode(w, req.Node)
		if !ok {
			return
		}
		removed, err := nc.ProxyRemoveSession(r.Context(), req.Key)
		if err != nil {
			slog.Warn("remote remove session failed", "node", req.Node, "key", req.Key, "err", err)
			if isUnknownRPCMethodErr(err) {
				// Peer is running an older binary without remove_session
				// support; return 409 + explicit body so the dashboard can
				// show a specific "upgrade needed" message instead of the
				// generic "remove failed". 409 (Conflict) signals the
				// request was valid but the peer cannot fulfill it.
				http.Error(w, "remote node needs upgrade to support this action", http.StatusConflict)
				return
			}
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		if !removed {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}

	if !h.router.Remove(req.Key) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	writeJSON(w, map[string]string{"status": "ok"})
}

// POST /api/sessions/resume
func (h *SessionHandlers) handleResume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID  string `json:"session_id"`
		Workspace  string `json:"workspace"`
		LastPrompt string `json:"last_prompt"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}
	if !discovery.IsValidSessionID(req.SessionID) {
		http.Error(w, "invalid session_id", http.StatusBadRequest)
		return
	}
	// Bound last_prompt so a single resume request can't ship a megabyte-scale
	// string that is then broadcast on every /api/sessions poll. Control chars
	// would also inject into structured slog JSONHandler output.
	if len(req.LastPrompt) > maxResumeLastPromptBytes {
		http.Error(w, "last_prompt too long", http.StatusBadRequest)
		return
	}
	for i := 0; i < len(req.LastPrompt); i++ {
		c := req.LastPrompt[i]
		if c == 0 || (c < 0x20 && c != '\t') || c == 0x7f {
			http.Error(w, "last_prompt contains invalid control characters", http.StatusBadRequest)
			return
		}
	}

	workspace := req.Workspace
	if workspace != "" {
		wsPath, err := validateWorkspace(workspace, h.allowedRoot)
		if err != nil {
			writeJSONStatus(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		workspace = wsPath
	}
	if workspace == "" {
		workspace = h.router.DefaultWorkspace()
	}

	var rb [8]byte
	if _, err := rand.Read(rb[:]); err != nil {
		// crypto/rand failures are pathologically rare (kernel entropy
		// pool gone, exhausted FDs), but without a log operators cannot
		// distinguish "resume failed" from other 500s.
		slog.Error("resume register: generate key failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	key := "dashboard:direct:r" + hex.EncodeToString(rb[:]) + ":general"
	effectiveKey := h.router.RegisterForResume(key, req.SessionID, workspace, req.LastPrompt)

	writeJSON(w, map[string]string{"status": "ok", "key": effectiveKey})
}

// POST /api/sessions/interrupt
func (h *SessionHandlers) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key  string `json:"key"`
		Node string `json:"node"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	// Remote node proxy
	if req.Node != "" && req.Node != "local" {
		nc, ok := h.nodeAccess.LookupNode(w, req.Node)
		if !ok {
			return
		}
		interrupted, err := nc.ProxyInterruptSession(r.Context(), req.Key)
		if err != nil {
			slog.Warn("remote interrupt session failed", "node", req.Node, "key", req.Key, "err", err)
			if isUnknownRPCMethodErr(err) {
				http.Error(w, "remote node needs upgrade to support this action", http.StatusConflict)
				return
			}
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		if interrupted {
			slog.Info("remote session interrupted via HTTP", "node", req.Node, "key", req.Key)
			writeJSON(w, map[string]string{"status": "ok"})
		} else {
			writeJSON(w, map[string]string{"status": "not_running"})
		}
		return
	}

	ok := h.router.InterruptSession(req.Key)
	if ok {
		slog.Info("session interrupted via HTTP", "key", req.Key)
		writeJSON(w, map[string]string{"status": "ok"})
	} else {
		writeJSON(w, map[string]string{"status": "not_running"})
	}
}

// historySessions returns all filesystem sessions from the last 7 days.
// Results are cached for 30 seconds.
func (h *SessionHandlers) historySessions() []discovery.RecentSession {
	if h.claudeDir == "" {
		return nil
	}

	const cacheTTL = 120 * time.Second
	h.historyCacheMu.Lock()
	if time.Since(h.historyCacheTime) < cacheTTL {
		cached := h.historyCache
		h.historyCacheMu.Unlock()
		return cached
	}
	h.historyCacheMu.Unlock()

	v, _, _ := h.historyFlight.Do("history", func() (any, error) {
		return h.loadHistorySessions(), nil
	})

	if res, ok := v.([]discovery.RecentSession); ok {
		return res
	}
	return nil
}

// WarmHistoryCache pre-populates the history sessions cache in the background
// so that the first dashboard load does not block on a full filesystem scan.
func (h *SessionHandlers) WarmHistoryCache() {
	if h.claudeDir == "" {
		return
	}
	go func() {
		h.historyFlight.Do("history", func() (any, error) {
			return h.loadHistorySessions(), nil
		})
	}()
}

// lookupSummariesCached returns sessionID→summary with a 30s TTL cache.
// The cache key set (sessionID subset) may vary between calls; we store the
// full lookup result and serve cached entries that overlap with the current
// snapshot request. On miss or expiry, re-run discovery.LookupSummaries and
// merge the fresh result into the cache.
func (h *SessionHandlers) lookupSummariesCached(snapshots []session.SessionSnapshot) map[string]string {
	const summaryTTL = 30 * time.Second

	h.summaryCacheMu.Lock()
	if h.summaryCache != nil && time.Since(h.summaryCacheTime) < summaryTTL {
		cached := h.summaryCache
		h.summaryCacheMu.Unlock()
		return cached
	}
	h.summaryCacheMu.Unlock()

	sessionWorkspaces := make(map[string]string, len(snapshots))
	for _, snap := range snapshots {
		if snap.SessionID != "" && snap.Workspace != "" {
			sessionWorkspaces[snap.SessionID] = snap.Workspace
		}
	}
	fresh := discovery.LookupSummaries(h.claudeDir, sessionWorkspaces)

	h.summaryCacheMu.Lock()
	h.summaryCache = fresh
	h.summaryCacheTime = time.Now()
	h.summaryCacheMu.Unlock()
	return fresh
}

func (h *SessionHandlers) loadHistorySessions() []discovery.RecentSession {
	excludeIDs := h.router.DiscoveryExcludeIDs()
	all := discovery.RecentSessions(h.claudeDir, 200, 7*24*time.Hour, excludeIDs)

	// Resolve project names in batch.
	if h.projectMgr != nil && len(all) > 0 {
		workspaces := make([]string, 0, len(all))
		for _, rs := range all {
			workspaces = append(workspaces, rs.Workspace)
		}
		wsMap := h.projectMgr.ResolveWorkspaces(workspaces)
		for i := range all {
			all[i].Project = wsMap[all[i].Workspace]
		}
	}

	h.historyCacheMu.Lock()
	h.historyCache = all
	h.historyCacheTime = time.Now()
	h.historyCacheMu.Unlock()

	return all
}
