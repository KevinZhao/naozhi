package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// HandleList serves GET /api/sessions. It orchestrates focused helpers —
// filterAndCountSnapshots, fillProjectAndSummary, buildSessionStats,
// buildLocalResp / buildMultiNodeResp — each of which documents its own
// mutation contract (#736). Every response carries a content ETag
// (sessionsBodyETag); an If-None-Match naming it gets a bodyless 304.
func (h *Handlers) HandleList(w http.ResponseWriter, r *http.Request) {
	knownNodes := h.deps.NodeAccess.KnownNodes()
	// ListSessionsWithVersion keeps (snapshots, version) in one r.mu.RLock
	// epoch (#726).
	snapshots, version := h.deps.Router.ListSessionsWithVersion()

	// Captured once so cutoff / uptime bucket share a single vDSO call.
	now := time.Now()

	snapshots, running, ready := filterAndCountSnapshots(snapshots, now)
	// The table hands sessions out in map order; a fixed order is what lets
	// two polls of an unchanged table hash alike.
	slices.SortFunc(snapshots, func(a, b sessionpkg.SessionSnapshot) int { return strings.Compare(a.Key, b.Key) })

	// Overlay tailer-side agent metrics; no-op when no Hub is wired (tests).
	if h.deps.SnapshotEnricher != nil {
		for i := range snapshots {
			h.deps.SnapshotEnricher(&snapshots[i])
		}
	}

	h.fillProjectAndSummary(snapshots)

	stats := h.buildSessionStats(now, version, running, ready)

	// KnownNodes was sampled once at the top (immutable snapshot, no lock).
	// The hashed copy has uptime blanked; the body keeps it.
	var resp, hashed any
	if len(knownNodes) == 0 {
		local := h.buildLocalResp(snapshots, stats)
		resp = local
		local.Stats.Uptime = ""
		hashed = local
	} else {
		multi := h.buildMultiNodeResp(snapshots, stats, knownNodes)
		resp = multi
		multi.Stats.Uptime = ""
		hashed = multi
	}
	if etag := sessionsBodyETag(hashed); etag != "" {
		w.Header().Set("ETag", etag)
		if etagListMatches(r.Header.Get("If-None-Match"), etag) {
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	httputil.WriteJSON(w, resp)
}

// sessionsBodyETag is the validator for a /api/sessions body: the first 128
// bits of the SHA-256 of its JSON encoding. The caller blanks stats.uptime,
// the one field that moves every second, so a 304 may leave the uptime display
// stale; every other field is covered. Weak, because bodies that differ in
// uptime share it. "" when v does not encode (WriteJSON then serves the 500).
func sessionsBodyETag(v any) string {
	sum := sha256.New()
	if err := json.NewEncoder(sum).Encode(v); err != nil {
		return ""
	}
	var b [sha256.Size]byte
	return node.SessionsContentETagPrefix + hex.EncodeToString(sum.Sum(b[:0])[:16]) + `"`
}

// etagListMatches applies RFC 9110 §13.1.2 If-None-Match: `*`, or any entry
// of the comma list equal to etag under weak comparison (W/ ignored).
func etagListMatches(header, etag string) bool {
	etag = strings.TrimPrefix(etag, "W/")
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || strings.TrimPrefix(tag, "W/") == etag {
			return true
		}
	}
	return false
}

// filterAndCountSnapshots walks the router snapshot once: it counts running /
// ready across ALL entries (so maxProcs pressure includes scratch / cron / sys
// sessions) and drops scratch / cron / sys keys from the returned slice (they
// own dedicated panels). Compacts in place — the result aliases the input
// header with shrunk len, so callers must not retain the original.
//
// No 24h "dead session" cutoff (#2278): sessions stay until explicitly
// closed; idle-expired ones remain as resumable cards. `now` is retained for
// signature stability.
func filterAndCountSnapshots(snapshots []sessionpkg.SessionSnapshot, now time.Time) ([]sessionpkg.SessionSnapshot, int, int) {
	_ = now // retained for signature stability; no time-based cutoff (#2278)
	var running, ready int
	n := 0
	for _, snap := range snapshots {
		switch snap.State {
		case "running":
			running++
		case "ready":
			ready++
		}
		// Scratch, cron and sys: sessions own a CLI process so they appear in
		// ListSessions, but each has its own panel (drawer / 「定时任务」 / System
		// drawer, docs/rfc/system-session.md §9.2) and must not render here.
		if sessionkey.IsScratchKey(snap.Key) || sessionkey.IsCronKey(snap.Key) || sessionkey.IsSysKey(snap.Key) {
			continue
		}
		snapshots[n] = snap
		n++
	}
	return snapshots[:n], running, ready
}

// borrowWorkspaces returns a recycled []string with cap >= want and len
// 0. The returned slice header MUST be returned via returnWorkspaces;
// callers that escape the slice into a struct field MUST copy first.
func borrowWorkspaces(want int) *[]string {
	p := workspacesPool.Get().(*[]string)
	s := *p
	if cap(s) < want {
		// Grow once to the request size rather than letting append's geometric
		// growth allocate on each call.
		s = make([]string, 0, want)
	} else {
		s = s[:0]
	}
	*p = s
	return p
}

// returnWorkspaces hands the slice back to the pool, dropping oversized
// backing arrays so one big poll cannot inflate every entry's footprint.
func returnWorkspaces(p *[]string) {
	if p == nil {
		return
	}
	const maxRetainCap = 4096
	if cap(*p) > maxRetainCap {
		return
	}
	// Clear element references so the pool doesn't GC-pin workspace strings
	// past the request.
	s := *p
	for i := range s {
		s[i] = ""
	}
	*p = s[:0]
	workspacesPool.Put(p)
}

// fillProjectAndSummary stamps each snapshot with its project name (from
// ProjectManager + planner-key fallback) and any persisted Summary from
// sessions-index.json. Mutates snapshots in place.
func (h *Handlers) fillProjectAndSummary(snapshots []sessionpkg.SessionSnapshot) {
	if h.deps.ProjectMgr != nil {
		// Pooled scratch buffer (#616); ResolveWorkspaces never retains it.
		wsPtr := borrowWorkspaces(len(snapshots))
		defer returnWorkspaces(wsPtr)
		workspaces := *wsPtr
		for i := range snapshots {
			if !project.IsPlannerKey(snapshots[i].Key) && snapshots[i].Workspace != "" {
				workspaces = append(workspaces, snapshots[i].Workspace)
			}
		}
		*wsPtr = workspaces
		wsMap := h.deps.ProjectMgr.ResolveWorkspaces(workspaces)

		for i := range snapshots {
			if project.IsPlannerKey(snapshots[i].Key) {
				// Planner keys are "project:{name}:planner"; two IndexByte calls
				// avoid SplitN's []string alloc.
				key := snapshots[i].Key
				const plannerPrefix = "project:"
				if len(key) > len(plannerPrefix) {
					rest := key[len(plannerPrefix):]
					if j := strings.IndexByte(rest, ':'); j > 0 {
						snapshots[i].Project = rest[:j]
						snapshots[i].IsPlanner = true
					}
				}
			} else if name := wsMap[snapshots[i].Workspace]; name != "" {
				snapshots[i].Project = name
			} else if base := workspaceFallbackName(snapshots[i].Workspace); base != "" {
				// Unregistered workspace: show the folder name so the session
				// still lands in a meaningful group. ProjectFallback tells the
				// frontend to key the group by path so /a/tmp and /b/tmp don't
				// collapse together.
				snapshots[i].Project = base
				snapshots[i].ProjectFallback = true
			}
		}
	}

	// Fill summary from sessions-index.json for managed sessions
	if h.deps.ClaudeDir != "" {
		summaryMap := h.lookupSummariesCached(snapshots)
		for i := range snapshots {
			if summary := summaryMap[snapshots[i].SessionID]; summary != "" {
				snapshots[i].Summary = summary
			}
		}
	}
}

// buildSessionStats assembles the typed sessionStats payload for GET
// /api/sessions.
func (h *Handlers) buildSessionStats(now time.Time, version uint64, running, ready int) sessionStats {
	active, total := h.deps.Router.Stats()
	stats := sessionStats{
		sessionStatsStatic: h.staticStats,
		Active:             active,
		Running:            running,
		Ready:              ready,
		Total:              total,
		Version:            version,
		VersionTag:         h.deps.VersionTag,
		Uptime:             h.uptimeStringAt(now),
		Watchdog: watchdogStats{
			NoOutputKills: h.deps.WatchdogNoOut.Load(),
			TotalKills:    h.deps.WatchdogTotal.Load(),
		},
	}
	// cli_version in staticStats is the spawn-time value and goes stale after
	// a host claude upgrade; re-resolve from the live init-frame version each
	// poll (lock-free atomic read). Only overwrite when non-empty so an
	// unwired router can't blank the startup value.
	if live := h.deps.Router.CLIVersion(); live != "" {
		stats.CLIVersion = live
	}
	stats.Projects = h.buildProjectList(now)
	_, stats.HistoryTag = h.historyWithTag()
	return stats
}

// buildLocalResp constructs the single-node /api/sessions JSON shape.
func (h *Handlers) buildLocalResp(snapshots []sessionpkg.SessionSnapshot, stats sessionStats) sessionListLocalResp {
	return sessionListLocalResp{
		Sessions: snapshots,
		Stats:    stats,
	}
}

// buildMultiNodeResp constructs the multi-node /api/sessions JSON shape: local
// sessions are tagged Node="local" and merged with remote-node sessions and
// connection status from the node cache + live nodesSnapshot.
func (h *Handlers) buildMultiNodeResp(snapshots []sessionpkg.SessionSnapshot, stats sessionStats, knownNodes map[string]string) sessionListMultiResp {
	// Only the multi-node path needs the live snapshot (takes the nodeAccess lock).
	nodesSnapshot := h.deps.NodeAccess.NodesSnapshot()

	// Box *SessionSnapshot rather than the 280 B value: pointer payloads sit
	// inline in the iface, and json.Marshal output is identical (#1402).
	allSessions := make([]any, 0, len(snapshots))
	for i := range snapshots {
		snapshots[i].Node = "local"
		allSessions = append(allSessions, &snapshots[i])
	}

	localName := h.deps.WorkspaceName
	if localName == "" {
		localName = "Local"
	}
	nodeStatus := make(map[string]nodeStatusEntry, 1+len(nodesSnapshot)+len(knownNodes))
	nodeStatus["local"] = nodeStatusEntry{DisplayName: localName, Status: "ok"}

	cachedSessions, cachedStatus := h.deps.NodeCache.Sessions()
	// Node order fixes where each node's sessions land, so it must not vary
	// between polls (sessionsBodyETag).
	for _, id := range slices.Sorted(maps.Keys(nodesSnapshot)) {
		nc := nodesSnapshot[id]
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
	for id, displayName := range knownNodes {
		if _, connected := nodeStatus[id]; !connected {
			nodeStatus[id] = nodeStatusEntry{
				DisplayName: displayName,
				Status:      "offline",
			}
		}
	}

	return sessionListMultiResp{
		Sessions: allSessions,
		Stats:    stats,
		Nodes:    nodeStatus,
	}
}
