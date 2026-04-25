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
	"unicode/utf8"

	"golang.org/x/sync/singleflight"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
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

// Note: user-label validation lives in the session package
// (session.ValidateUserLabel / session.MaxUserLabelBytes) so the dashboard
// HTTP path and the reverse-RPC worker (internal/upstream) share one
// implementation. R64-GO-H3 / L1 / L2 consolidated the rules there.

// watchdogStats is the /api/sessions "watchdog" sub-object. Declared as a
// named struct (not an inline map[string]any) so json/reflect caches the
// type descriptor once and the value is stack-allocated per response,
// eliminating the per-poll 2-key map heap alloc the dashboard hot path
// used to pay. R58-PERF-F2.
type watchdogStats struct {
	NoOutputKills int64 `json:"no_output_kills"`
	TotalKills    int64 `json:"total_kills"`
}

// nodeStatusEntry is the per-node element in /api/sessions "nodes".
// Named struct (vs map[string]any{...}) eliminates N inner-map allocs and
// interface{} boxing on every 1 Hz dashboard poll. `omitempty` on
// remote_addr keeps the JSON output identical for offline / "local" rows
// that don't carry an address. R62-PERF-1.
type nodeStatusEntry struct {
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	RemoteAddr  string `json:"remote_addr,omitempty"`
}

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
	scheduler   *cron.Scheduler // optional; used by handleEvents to revive dismissed cron stubs
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

	// uptimeCache memoises the formatted uptime string at 1-second resolution.
	// handleList is hit at 1 Hz × N dashboard tabs, and
	// time.Since(startedAt).Round(time.Second).String() allocates a short
	// string on every call — roughly (N-1)/N of those allocations sit inside
	// the same 1-second bucket. Caching the string with its bucket-id (seconds
	// since start) lets all pollers within the same second reuse one alloc.
	// Races are benign: concurrent misses re-format the same value. R65-PERF-L-1.
	uptimeCache atomic.Pointer[uptimeSnapshot]

	// staticStats pre-builds the subset of /api/sessions stats fields that
	// are immutable after startup (backend, cli_name, workspace_*, system,
	// agents, watchdog). handleList shallow-copies this map on each poll
	// instead of re-building the 12+ key map literal, which avoids per-key
	// interface{} boxing and nested map allocs on the dashboard hot path.
	// Initialized once by initStaticStats() after all fields are set.
	staticStats map[string]any
	// staticStatsOnce enforces the "initStaticStats called exactly once"
	// contract structurally. A test double or future refactor that calls
	// initStaticStats twice would otherwise race with concurrent handleList
	// readers, who read staticStats without synchronisation. R61-GO-12.
	staticStatsOnce sync.Once

	// History cache (120s TTL — see cacheTTL in historySessions)
	historyCache     []discovery.RecentSession
	historyCacheTime time.Time
	historyCacheMu   sync.Mutex
	historyFlight    singleflight.Group
	// warmHistoryWg tracks the WarmHistoryCache goroutine so callers (server
	// shutdown) can wait for the background FS scan to finish before tearing
	// down h.claudeDir-dependent state. R64-GO-M1.
	warmHistoryWg sync.WaitGroup

	// Summary cache (30s TTL) — avoids re-running discovery.LookupSummaries
	// (N os.Stat + package-level lock) on every GET /api/sessions poll.
	summaryCache     map[string]string
	summaryCacheTime time.Time
	summaryCacheMu   sync.Mutex
	// summaryFlight collapses concurrent misses at the 30s TTL boundary into
	// a single LookupSummaries invocation. Before this, N simultaneous tab
	// polls that missed the cache each performed a full N×os.Stat scan over
	// the project's .claude directory — multiplied by slow network filesystems
	// this could saturate disk IO. Mirrors the historyFlight pattern.
	// R60-PERF-5.
	summaryFlight singleflight.Group
}

// GET /api/sessions
func (h *SessionHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	// Read Version() BEFORE ListSessions(). storeGen is atomic, ListSessions
	// takes an RLock: a mutation landing between List→ and →Version would
	// otherwise publish data at gen N with version N+1, and the dashboard
	// would skip the next poll (N+1 already seen) until a later mutation
	// bumps to N+2 — effectively a "stale sessions" window of up to 1 poll.
	// Reading version first makes the response's version field an ≤ bound
	// on the data's freshness, so the dashboard never skips a real change.
	//
	// The reverse race is intentional: a mutation landing between Version
	// and ListSessions produces data at gen N+1 tagged with version N. The
	// next poll sees version N+1, re-reads, and catches up — at worst one
	// poll of display lag. The alternate ordering would instead make a
	// real-change poll look like a duplicate and skip the refresh until a
	// later unrelated mutation, which operators perceive as "my send didn't
	// update the UI". version ≤ data is the safer side. R60-GO-M3.
	version := h.router.Version()
	snapshots := h.router.ListSessions()

	// Keep dead sessions in the workspace sidebar for up to 24 hours. Merge
	// the filter pass with running/ready accounting so we only walk the
	// slice once — the dashboard polls this at 1 Hz × N tabs, and a full
	// re-scan later in handleList was pure bookkeeping for state counts the
	// filter pass could have computed in-place at zero extra cost.
	cutoff24h := time.Now().Add(-24 * time.Hour).UnixMilli()
	var running, ready int
	n := 0
	for _, snap := range snapshots {
		if snap.DeathReason != "" && snap.LastActive < cutoff24h {
			continue
		}
		switch snap.State {
		case "running":
			running++
		case "ready":
			ready++
		}
		snapshots[n] = snap
		n++
	}
	snapshots = snapshots[:n]

	// Fill project field from ProjectManager
	if h.projectMgr != nil {
		// Pre-size to len(snapshots): the loop accepts at most one entry per
		// session, so the slice never grows past this bound. Starting at nil
		// made append log(N) growth-realloc through 0→1→2→4→…→n per poll,
		// visible in heap profiles on session-heavy dashboards. R60-PERF-4.
		workspaces := make([]string, 0, len(snapshots))
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

	// Shallow-copy the pre-built staticStats (backend/cli/workspace/system/
	// agents are immutable after startup) and overlay the six dynamic fields.
	// Watchdog is a stack-allocated struct (fixed 2-field shape) so we avoid
	// the per-poll map[string]any alloc the previous inline literal paid.
	// R58-PERF-F2.
	//
	// Defensive nil check: initStaticStats is called from server.New before
	// requests arrive, but a future refactor (or test double constructing
	// SessionHandlers by hand) could skip it. Treat nil as an empty map so
	// the response still carries the dynamic fields rather than panicking on
	// a missing caller-required `agents`/`system` key. R60-GO-L3.
	staticLen := len(h.staticStats)
	stats := make(map[string]any, staticLen+6)
	for k, v := range h.staticStats {
		stats[k] = v
	}
	stats["active"] = active
	stats["running"] = running
	stats["ready"] = ready
	stats["total"] = total
	stats["version"] = version
	stats["uptime"] = h.uptimeString()
	stats["watchdog"] = watchdogStats{
		NoOutputKills: h.watchdogNoOut.Load(),
		TotalKills:    h.watchdogTotal.Load(),
	}

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

	// No configured nodes at all: use simple single-node response format.
	// Pre-size resp to 3 so the optional history_sessions insert doesn't
	// trigger a bucket rehash on fresh-deployment dashboards that always
	// have JSONL history to show. R64-PERF-10.
	if len(h.nodeAccess.KnownNodes()) == 0 {
		resp := make(map[string]any, 3)
		resp["sessions"] = snapshots
		resp["stats"] = stats
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
	// nodeStatus is a map[string]nodeStatusEntry (named struct, omitempty on
	// remote_addr) instead of map[string]any{...map[string]any{...}} — the
	// prior shape paid N inner-map allocs + interface{} boxing per key on
	// every 1 Hz /api/sessions poll. Marshals identically to the JSON
	// clients expect. R62-PERF-1.
	nodeStatus := make(map[string]nodeStatusEntry, 1+len(nodesSnapshot)+len(h.nodeAccess.KnownNodes()))
	nodeStatus["local"] = nodeStatusEntry{DisplayName: localName, Status: "ok"}

	cachedSessions, cachedStatus := h.nodeCache.Sessions()
	for id, nc := range nodesSnapshot {
		status := cachedStatus[id]
		if status == "" {
			status = "ok"
		}
		nodeStatus[id] = nodeStatusEntry{
			DisplayName: nc.DisplayName(),
			Status:      status,
			RemoteAddr:  nc.RemoteAddr(),
		}
		for _, rs := range cachedSessions[id] {
			allSessions = append(allSessions, rs)
		}
	}

	// Always include all configured nodes, even when currently disconnected.
	for id, displayName := range h.nodeAccess.KnownNodes() {
		if _, connected := nodeStatus[id]; !connected {
			nodeStatus[id] = nodeStatusEntry{
				DisplayName: displayName,
				Status:      "offline",
			}
		}
	}

	// Pre-size for the 3 always-set keys + optional history_sessions to skip
	// the bucket rehash on the common "repo with JSONL history + nodes
	// configured" path. Mirrors the single-node path pattern at line ~314.
	// R65-PERF-L-3.
	resp := make(map[string]any, 4)
	resp["sessions"] = allSessions
	resp["stats"] = stats
	resp["nodes"] = nodeStatus
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
//     returning up to `limit` newest-first-then-
//     reversed (chronological) entries
//   - limit     (optional): caps the result count. Required when `before` is set;
//     optional with `after` (defaults to uncapped for
//     backwards compat); when neither `after` nor `before`
//     is given, limit controls the initial page size
//     (defaults to returning everything — legacy behaviour)
//
// Precedence: `after` wins over `before` if both are supplied (streaming
// catch-up outranks pagination). No params = full history (legacy).
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

	// Remote node proxy — forward after only (the remote protocol predates
	// before/limit). If/when FetchEventsPaginated exists, we can extend here
	// without breaking older peer binaries.
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
		// Apply page cap on the returned entries so the dashboard gets a
		// consistent-size payload even from legacy peers.
		if limit > 0 && len(entries) > limit {
			entries = entries[len(entries)-limit:]
		}
		writeJSON(w, entries)
		return
	}

	// Local
	sess := h.router.GetSession(key)
	if sess == nil && h.scheduler != nil && h.scheduler.EnsureStub(key) {
		// Cron stubs are torn down by sidebar "×". The stub is lazily rebuilt
		// on next click so polling clients (WS-down fallback) can still open
		// the panel instead of getting a permanent 404 until the next tick.
		sess = h.router.GetSession(key)
	}
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

// PATCH /api/sessions/label — update the operator-set display label for a
// session. Empty label clears any prior value.
func (h *SessionHandlers) handleSetLabel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key   string `json:"key"`
		Node  string `json:"node"`
		Label string `json:"label"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	label, err := session.ValidateUserLabel(req.Label)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Remote node proxy — forward to the node that owns the session.
	if req.Node != "" && req.Node != "local" {
		nc, ok := h.nodeAccess.LookupNode(w, req.Node)
		if !ok {
			return
		}
		updated, err := nc.ProxySetSessionLabel(r.Context(), req.Key, label)
		if err != nil {
			slog.Warn("remote set session label failed", "node", req.Node, "key", req.Key, "err", err)
			if isUnknownRPCMethodErr(err) {
				http.Error(w, "remote node needs upgrade to support this action", http.StatusConflict)
				return
			}
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		if !updated {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		// Parallel audit entry with the local-path slog.Info below so an
		// operator grepping journalctl sees every label change regardless of
		// which node owns the session. R64-GO-M3.
		slog.Info("session label updated", "node", req.Node, "key", req.Key, "label_len", len(label))
		writeJSON(w, map[string]string{"status": "ok", "label": label})
		return
	}

	if !h.router.SetUserLabel(req.Key, label) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	slog.Info("session label updated", "node", "local", "key", req.Key, "label_len", len(label))
	writeJSON(w, map[string]string{"status": "ok", "label": label})
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
	// Rune-level scan: a byte-only gate missed C1 control points (U+0080..
	// U+009F), which arrive as valid UTF-8 continuation bytes in the 0x80..
	// 0x9F range and slipped past a `< 0x20` byte check. C1 bytes in
	// last_prompt are broadcast on every /api/sessions poll and flow into
	// slog attrs, so they corrupt structured logs and terminal viewers.
	// `\t` is intentionally still allowed (prompts may contain tab) as
	// before — slog escapes tab in JSONHandler output. R65-SEC-M-3.
	if !utf8.ValidString(req.LastPrompt) {
		http.Error(w, "last_prompt is not valid utf-8", http.StatusBadRequest)
		return
	}
	for _, r := range req.LastPrompt {
		if r == 0 || (r < 0x20 && r != '\t') || (r >= 0x7F && r <= 0x9F) {
			http.Error(w, "last_prompt contains invalid control characters", http.StatusBadRequest)
			return
		}
	}

	workspace := req.Workspace
	if workspace != "" {
		wsPath, err := validateWorkspace(workspace, h.allowedRoot)
		if err != nil {
			// Decouple the client-facing message from the underlying error
			// chain so a future edit of validateWorkspace wrapping a
			// *os.PathError (e.g. with %w) cannot leak resolved filesystem
			// paths to the dashboard user. validateWorkspace already logs
			// diagnostic detail via slog. R61-SEC-10.
			slog.Warn("resume workspace validation failed", "err", err, "workspace", workspace)
			writeJSONStatus(w, http.StatusForbidden, map[string]string{"error": "invalid workspace"})
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

	// Prefer control_request over SIGINT — see Router.InterruptSessionSafe
	// for why raw SIGINT on `-p` mode is destructive.
	switch h.router.InterruptSessionSafe(req.Key) {
	case session.InterruptSent:
		slog.Info("session interrupted via HTTP", "key", req.Key)
		writeJSON(w, map[string]string{"status": "ok"})
	case session.InterruptNoSession:
		writeJSON(w, map[string]string{"status": "not_running"})
	default:
		writeJSON(w, map[string]string{"status": "not_running"})
	}
}

// historySessions returns all filesystem sessions from the last 7 days.
// Results are cached for 120 seconds (see cacheTTL below).
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
		// Re-check under lock — a prior leader could have populated the
		// cache between our expiry detection and this closure running.
		// Mirrors the double-check pattern in lookupSummariesCached so
		// tail callers at a TTL boundary don't each pay an FS scan.
		// R64-GO-M2.
		//
		// Use historyCacheTime.IsZero() (not historyCache != nil) to
		// determine cache population: an "empty-history" deployment
		// legitimately stores a nil slice on every load, which was then
		// misclassified as "not cached" and drove a redundant FS scan
		// every TTL window. R67-GO-5.
		h.historyCacheMu.Lock()
		if !h.historyCacheTime.IsZero() && time.Since(h.historyCacheTime) < cacheTTL {
			cached := h.historyCache
			h.historyCacheMu.Unlock()
			return cached, nil
		}
		h.historyCacheMu.Unlock()
		return h.loadHistorySessions(), nil
	})

	if res, ok := v.([]discovery.RecentSession); ok {
		return res
	}
	return nil
}

// uptimeSnapshot is the value cached by uptimeCache. Bucket is the integer
// number of seconds since startedAt; Str is the pre-formatted rendering at
// that resolution. Cached at 1-second resolution because the dashboard polls
// every second and all pollers within the same bucket observe the same value.
type uptimeSnapshot struct {
	Bucket int64
	Str    string
}

// uptimeString returns time.Since(startedAt).Round(time.Second).String() with
// a 1-second resolution memoisation. Concurrent misses may all format the
// same value; last-writer-wins via unconditional Store is intentional —
// losers drop their locally formatted copy (the formatted string still
// escapes to the response regardless, so no leak).
func (h *SessionHandlers) uptimeString() string {
	d := time.Since(h.startedAt).Round(time.Second)
	bucket := int64(d / time.Second)
	if cur := h.uptimeCache.Load(); cur != nil && cur.Bucket == bucket {
		return cur.Str
	}
	s := d.String()
	h.uptimeCache.Store(&uptimeSnapshot{Bucket: bucket, Str: s})
	return s
}

// initStaticStats pre-builds the immutable subset of /api/sessions stats so
// handleList only has to overlay active/running/ready/total/version/uptime/
// watchdog on each poll. Safe to call multiple times: the Once guards against
// a test double or future refactor re-running the init concurrently with
// handleList readers. R61-GO-12.
func (h *SessionHandlers) initStaticStats() {
	h.staticStatsOnce.Do(h.doInitStaticStats)
}

func (h *SessionHandlers) doInitStaticStats() {
	// Deep-copy systemInfo()'s singleton map: handleList shallow-copies
	// staticStats into a per-response map, so the "system" entry would
	// otherwise be aliased across every response. A future maintainer adding
	// a mutable system field (e.g. network counters) would then silently
	// introduce a data race across the dashboard hot path. Breaking the
	// alias at initialisation enforces the read-only contract structurally.
	sysSrc := systemInfo()
	sysCopy := make(map[string]any, len(sysSrc))
	for k, v := range sysSrc {
		sysCopy[k] = v
	}
	// Copy agentIDs for consistency with the "system" deep-copy contract.
	// String elements are immutable so today the shared backing array is
	// race-free, but baking the copy in at init time prevents a future
	// maintainer from turning agentIDs into []AgentInfo (mutable struct)
	// and introducing a cross-goroutine data race on every dashboard poll.
	// R58-GO-M2.
	agentsCopy := make([]string, len(h.agentIDs))
	copy(agentsCopy, h.agentIDs)
	h.staticStats = map[string]any{
		"backend":           h.backendTag,
		"cli_name":          h.router.CLIName(),
		"cli_version":       h.router.CLIVersion(),
		"max_procs":         h.router.MaxProcs(),
		"default_workspace": h.router.DefaultWorkspace(),
		"workspace_id":      h.workspaceID,
		"workspace_name":    h.workspaceName,
		"system":            sysCopy,
		"agents":            agentsCopy,
	}
}

// WarmHistoryCache pre-populates the history sessions cache in the background
// so that the first dashboard load does not block on a full filesystem scan.
//
// The goroutine is tracked by warmHistoryWg so WaitWarmHistory can block
// server shutdown until the FS scan finishes. Without the tracker the
// goroutine could outlive the shutdown and write h.historyCache after
// h.claudeDir-dependent state had been torn down. R64-GO-M1.
func (h *SessionHandlers) WarmHistoryCache() {
	if h.claudeDir == "" {
		return
	}
	h.warmHistoryWg.Add(1)
	go func() {
		defer h.warmHistoryWg.Done()
		h.historyFlight.Do("history", func() (any, error) {
			return h.loadHistorySessions(), nil
		})
	}()
}

// WaitWarmHistory blocks until any in-flight WarmHistoryCache goroutine
// completes. Call from server shutdown after refusing new requests to
// guarantee no background loadHistorySessions races with teardown.
func (h *SessionHandlers) WaitWarmHistory() {
	h.warmHistoryWg.Wait()
}

// lookupSummariesCached returns sessionID→summary with a 30s TTL cache.
// The cache key set (sessionID subset) may vary between calls; we store the
// full lookup result and serve cached entries that overlap with the current
// snapshot request. On miss or expiry, re-run discovery.LookupSummaries and
// merge the fresh result into the cache.
//
// Concurrent misses at the TTL boundary are collapsed via summaryFlight so
// N parallel tab polls that all see the cache as expired do not each
// perform a full N×os.Stat fan-out. R60-PERF-5.
func (h *SessionHandlers) lookupSummariesCached(snapshots []session.SessionSnapshot) map[string]string {
	const summaryTTL = 30 * time.Second

	h.summaryCacheMu.Lock()
	if h.summaryCache != nil && time.Since(h.summaryCacheTime) < summaryTTL {
		cached := h.summaryCache
		h.summaryCacheMu.Unlock()
		return cached
	}
	h.summaryCacheMu.Unlock()

	// singleflight collapses concurrent callers into one LookupSummaries
	// run. Followers get the same map the leader computed, so we also
	// avoid redundant cache-write contention. The "summary" key is a
	// fixed constant because the leader's result is cached for the
	// entire ±30s window regardless of which subset drove the miss.
	//
	// Build sessionWorkspaces *inside* the flight closure: only the leader
	// actually consumes it, so followers no longer pay an O(N sessions)
	// map alloc + copy that is immediately discarded when the flight
	// routes them to the leader's result. The leader also gets the most
	// recent router view because snapshots passed in from each follower
	// may differ slightly; the leader captures whichever caller's
	// snapshots happened to win the race, which is acceptable for a 30s
	// cache window. R61-PERF-1.
	v, _, _ := h.summaryFlight.Do("summary", func() (any, error) {
		// Re-check under lock — a prior leader could have populated the
		// cache between our expiry detection and this closure running.
		h.summaryCacheMu.Lock()
		if h.summaryCache != nil && time.Since(h.summaryCacheTime) < summaryTTL {
			cached := h.summaryCache
			h.summaryCacheMu.Unlock()
			return cached, nil
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
		return fresh, nil
	})
	if m, ok := v.(map[string]string); ok {
		return m
	}
	return nil
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
