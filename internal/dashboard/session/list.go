package session

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/project"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// HandleList serves GET /api/sessions. It orchestrates focused helpers —
// filterAndCountSnapshots, fillProjectAndSummary, buildSessionStats,
// buildLocalResp / buildMultiNodeResp — each of which documents its own
// mutation contract (#736).
func (h *Handlers) HandleList(w http.ResponseWriter, r *http.Request) {
	// Conditional GET (#1916): the ETag folds in storeGen plus a history
	// fingerprint (cache epoch + length) — every input that can change the
	// SINGLE-NODE body. Multi-node responses also depend on live node status
	// with no version hook, so they always rebuild. Clients that omit
	// If-None-Match always get a full 200 with the ETag set.
	knownNodes := h.deps.NodeAccess.KnownNodes()
	singleNode := len(knownNodes) == 0

	// sinceVersion is the storeGen the client last rendered; 0 (absent or
	// unparseable) forces a full build.
	clientETag := r.Header.Get("If-None-Match")
	sinceVersion := parseETagVersion(clientETag)

	snapshots, version, changed := h.deps.Router.ListSessionsIfChanged(sinceVersion)

	var etag string
	if singleNode {
		// Warm the history cache BEFORE fingerprinting so the ETag reflects the
		// exact history slice buildLocalResp embeds (its own historySessions()
		// call then hits the same epoch). Steady state is a wait-free TTL hit.
		h.historySessions()
		etag = h.sessionsListETag(version)
		// Snapshots unchanged AND full validator matches → nothing in the
		// single-node body moved; 304 and skip the rebuild.
		if !changed && clientETag != "" && clientETag == etag {
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	// changed==false with no 304 (multi-node, first poll, or history moved
	// while storeGen held) still needs a body. ListSessionsWithVersion keeps
	// (snapshots, version) in one r.mu.RLock epoch (#726).
	if !changed {
		snapshots, version = h.deps.Router.ListSessionsWithVersion()
		if singleNode {
			etag = h.sessionsListETag(version)
		}
	}

	// Captured once so cutoff / uptime bucket share a single vDSO call.
	now := time.Now()

	snapshots, running, ready := filterAndCountSnapshots(snapshots, now)

	// Overlay tailer-side agent metrics; no-op when no Hub is wired (tests).
	if h.deps.SnapshotEnricher != nil {
		for i := range snapshots {
			h.deps.SnapshotEnricher(&snapshots[i])
		}
	}

	h.fillProjectAndSummary(snapshots)

	stats := h.buildSessionStats(now, version, running, ready)

	// KnownNodes was sampled once at the top (immutable snapshot, no lock).
	// Stamp the ETag so the next poll's If-None-Match can 304.
	if singleNode {
		w.Header().Set("ETag", etag)
		httputil.WriteJSON(w, h.buildLocalResp(snapshots, stats))
		return
	}

	httputil.WriteJSON(w, h.buildMultiNodeResp(snapshots, stats, knownNodes))
}

// parseETagVersion extracts the storeGen version from a sessionsListETag
// validator (`"v<N>-h<E>-n<L>"`). Lenient: anything unparseable (weak
// validator, future format, absent header) yields 0, forcing a rebuild. The
// history suffix only participates in the full-string equality check.
func parseETagVersion(etag string) uint64 {
	etag = strings.TrimPrefix(etag, "W/")
	etag = strings.Trim(etag, `"`)
	if !strings.HasPrefix(etag, "v") {
		return 0
	}
	rest := etag[1:]
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		rest = rest[:i]
	}
	v, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// sessionsListETag derives the single-node /api/sessions validator from the
// storeGen version plus the history cache fingerprint (epoch nanos + length) —
// exactly the inputs that vary the body. Stats fields are static or derived
// from the same snapshots/version; projects/uptime move at coarser resolution.
// A re-scan yielding identical history still bumps the epoch: a harmless
// missed optimisation, never a stale read.
func (h *Handlers) sessionsListETag(version uint64) string {
	historyEpoch := h.historyCacheTimeUnixNano.Load()
	h.historyCacheMu.RLock()
	historyLen := len(h.historyCache)
	h.historyCacheMu.RUnlock()
	// Quoted opaque validator per RFC 7232 §2.3.
	return `"v` + strconv.FormatUint(version, 10) +
		`-h` + strconv.FormatInt(historyEpoch, 10) +
		`-n` + strconv.Itoa(historyLen) + `"`
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
	return stats
}

// buildLocalResp constructs the single-node /api/sessions JSON shape.
func (h *Handlers) buildLocalResp(snapshots []sessionpkg.SessionSnapshot, stats sessionStats) sessionListLocalResp {
	resp := sessionListLocalResp{
		Sessions: snapshots,
		Stats:    stats,
	}
	if history := h.historySessions(); len(history) > 0 {
		resp.HistorySessions = history
	}
	return resp
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
	for id, displayName := range knownNodes {
		if _, connected := nodeStatus[id]; !connected {
			nodeStatus[id] = nodeStatusEntry{
				DisplayName: displayName,
				Status:      "offline",
			}
		}
	}

	resp := sessionListMultiResp{
		Sessions: allSessions,
		Stats:    stats,
		Nodes:    nodeStatus,
	}
	if history := h.historySessions(); len(history) > 0 {
		resp.HistorySessions = history
	}
	return resp
}
