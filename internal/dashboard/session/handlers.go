package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/singleflight"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/dashboard/contracts"
	"github.com/naozhi/naozhi/internal/dashboard/cronview"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/textutil"
)

// maxResumeLastPromptBytes caps the last_prompt field on /api/sessions/resume
// so a megabyte-scale string is never persisted and echoed on every poll.
const maxResumeLastPromptBytes = 2 * 1024

// SanitizeResumeLastPrompt strips injection-prone bytes from a resume
// last_prompt before it reaches slog attrs or /api/sessions broadcasts.
// Mirrors osutil.SanitizeForLog except tab is preserved (operators paste
// tab-delimited snippets; slog JSONHandler escapes tab safely).
func SanitizeResumeLastPrompt(s string, maxLen int) string {
	if s == "" {
		return s
	}
	needsClean := (maxLen > 0 && len(s) > maxLen) ||
		strings.IndexFunc(s, func(r rune) bool {
			if r == '\t' {
				return false
			}
			if r < 0x20 || r == 0x7f {
				return true
			}
			return osutil.IsLogInjectionRune(r)
		}) >= 0
	if !needsClean {
		return s
	}
	mapped := strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		if osutil.IsLogInjectionRune(r) {
			return '_'
		}
		return r
	}, s)
	if maxLen > 0 && len(mapped) > maxLen {
		// Truncate at a rune boundary: invalid UTF-8 surfaces as garbled glyphs
		// in sessions.json and the dashboard UI.
		mapped = mapped[:textutil.TruncateAtRuneBoundary(mapped, maxLen)]
	}
	return mapped
}

// workspaceFallbackName returns the folder name to display as a session's
// sidebar group when the workspace is not registered with ProjectManager.
// Empty, "/" or "." inputs yield "" so the frontend uses its catch-all.
func workspaceFallbackName(ws string) string {
	if ws == "" {
		return ""
	}
	base := filepath.Base(ws)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

// watchdogStats is the /api/sessions "watchdog" sub-object. A named struct
// (not map[string]any) keeps the per-poll value stack-allocated.
type watchdogStats struct {
	NoOutputKills int64 `json:"no_output_kills"`
	TotalKills    int64 `json:"total_kills"`
}

// sessionStatsStatic holds the /api/sessions.stats fields that are immutable
// after startup. Built once by initStaticStats and embedded by value into
// sessionStats on every poll; embedding keeps the JSON flat and byte-identical
// to the earlier map shape. System stays a map[string]any because it must be
// deep-copied from the process-wide callSystemInfo() singleton (see
// doInitStaticStats).
type sessionStatsStatic struct {
	Backend          string         `json:"backend"`
	CLIName          string         `json:"cli_name"`
	CLIVersion       string         `json:"cli_version"`
	MaxProcs         int            `json:"max_procs"`
	DefaultWorkspace string         `json:"default_workspace"`
	WorkspaceID      string         `json:"workspace_id"`
	WorkspaceName    string         `json:"workspace_name"`
	System           map[string]any `json:"system"`
	Agents           []string       `json:"agents"`
}

// sessionStats is the "stats" sub-object of GET /api/sessions. Static fields
// are promoted flat via the embed; the JSON key set is a wire contract with
// dashboard.js (stats.agents / default_workspace / projects / cli_* /
// workspace_* / system / version).
type sessionStats struct {
	sessionStatsStatic
	Active  int    `json:"active"`
	Running int    `json:"running"`
	Ready   int    `json:"ready"`
	Total   int    `json:"total"`
	Version uint64 `json:"version"`
	// VersionTag is the naozhi build tag (`git describe`), distinct from the
	// uint64 `version` store-mutation counter. omitempty keeps the wire shape
	// when the ldflag is unset.
	VersionTag string        `json:"version_tag,omitempty"`
	Uptime     string        `json:"uptime"`
	Watchdog   watchdogStats `json:"watchdog"`
	// Projects has NO omitempty: after the last project is removed the
	// dashboard must receive `projects: []` to clear its stale list.
	Projects []projectListEntry `json:"projects"`
}

// nodeStatusEntry is the per-node element in /api/sessions "nodes"; omitempty
// on remote_addr keeps offline / "local" rows byte-identical to the old map.
type nodeStatusEntry struct {
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	RemoteAddr  string `json:"remote_addr,omitempty"`
}

// sessionListLocalResp is the /api/sessions response shape for single-node
// deployments. history_sessions is omitempty so deployments without JSONL
// history serialize the same 2-key object.
type sessionListLocalResp struct {
	Sessions        []sessionpkg.SessionSnapshot `json:"sessions"`
	Stats           sessionStats                 `json:"stats"`
	HistorySessions []discovery.RecentSession    `json:"history_sessions,omitempty"`
}

// sessionListMultiResp is the /api/sessions response shape when >=1 remote node
// is configured. Sessions is []any because local SessionSnapshot values are
// merged with remote entries decoded as map[string]any. Nodes has no omitempty:
// this struct is only used when the node map is populated.
type sessionListMultiResp struct {
	Sessions        []any                      `json:"sessions"`
	Stats           sessionStats               `json:"stats"`
	Nodes           map[string]nodeStatusEntry `json:"nodes"`
	HistorySessions []discovery.RecentSession  `json:"history_sessions,omitempty"`
}

// CronView is the narrow consumer interface this package needs from
// *cron.Scheduler (EnsureStub / SetJobPrompt / KnownSessionIDs). It aliases the
// canonical definition in internal/dashboard/cronview so the shape cannot drift
// from internal/server's copy (#1536).
//
// EnsureStub returns false for three indistinguishable cases: non-cron key
// (legitimate no-op), unknown job ID, or stub registration failure. Callers
// fall through to the nil-session 404, which is correct for all three; do not
// add a reason-by-deduction branch over the bool (#772).
type CronView = cronview.CronView

// historyFilter is the discovery.RecentSessionsFilter loadHistorySessions
// constructs each scan.  Snapshots the cron-known set + sys workspace
// once per call so the in-loop predicate is O(1) per session.
type historyFilter struct {
	skipWorkspace string              // sys-sessions absolute path; "" disables
	skipSessions  map[string]struct{} // cron known IDs; nil disables. READ-ONLY: shared cache snapshot (#1544).
}

func (f historyFilter) SkipWorkspace(ws string) bool {
	return f.skipWorkspace != "" && ws == f.skipWorkspace
}

func (f historyFilter) SkipSessionID(sid string) bool {
	if f.skipSessions == nil {
		return false
	}
	_, ok := f.skipSessions[sid]
	return ok
}

// Handlers groups the session list, events, delete, and resume API endpoints.
type Handlers struct {
	router     *sessionpkg.Router
	projectMgr *project.Manager
	// projectStableKeyEnabled gates emitting projectListEntry.StableKey;
	// mirrors dashproject.Handlers.projectStableKeyEnabled.
	projectStableKeyEnabled bool
	scheduler               CronView // optional; used by HandleEvents to revive dismissed cron stubs (EnsureStub)
	// cronSessions feeds KnownSessionIDs() to the history panel; nil disables
	// filtering cron-spawned JSONLs. Kept separate from scheduler so server.go
	// can nil either independently (#754).
	cronSessions CronView
	// sysWorkDir is sysession's transient Runner workspace; when non-empty its
	// JSONLs are hidden from the history panel (AutoTitler otherwise leaks
	// prompt fragments into "recent sessions").
	sysWorkDir  string
	claudeDir   string
	allowedRoot string
	agents      map[string]sessionpkg.AgentOpts
	// agentIDs is precomputed once (agents map is immutable after startup).
	agentIDs   []string
	nodeAccess NodeAccessor
	nodeCache  *node.CacheManager

	// Static status fields (immutable after construction)
	startedAt     time.Time
	backendTag    string
	workspaceID   string
	workspaceName string
	// versionTag is the build tag surfaced as sessionStats.VersionTag; empty
	// means unknown and is omitted from JSON.
	versionTag    string
	watchdogNoOut *atomic.Int64
	watchdogTotal *atomic.Int64

	// snapshotEnricher is wired from server.go to Hub.enrichSnapshot so
	// SubagentInfo rows carry tailer-side LastTool / ToolUses / DurationMS.
	// nil in tests that don't build a Hub.
	snapshotEnricher func(*sessionpkg.SessionSnapshot)

	// uptimeCache memoises the formatted uptime string per 1-second bucket so
	// N tabs polling at 1 Hz share one alloc. Races are benign: concurrent
	// misses re-format the same value.
	uptimeCache atomic.Pointer[uptimeSnapshot]

	// projectListCache memoises the projectList slice per 1-second bucket so N
	// tabs share one rebuild. The cached slice is READ-ONLY (HandleList copies
	// the header, never mutates); misses rebuild identically and last-writer
	// wins. 1s resolution beats a Manager-version hook because project
	// mutations are minute-scale and it avoids touching project.Manager.
	projectListCache atomic.Pointer[projectListSnapshot]

	// staticStats is the immutable stats subset, copied by value per poll.
	// Initialized once by initStaticStats() after all fields are set.
	staticStats sessionStatsStatic
	// staticStatsOnce makes "initStaticStats called exactly once" structural;
	// a second call would race with HandleList readers of staticStats.
	staticStatsOnce sync.Once

	// History cache (120s TTL — see cacheTTL in historySessions).
	//
	// ALIASING CONTRACT: cache hits return the slice header only, so readers
	// alias the same backing array. This is race-free ONLY because every
	// refresh path (loadHistorySessions, WarmHistoryCache, future features)
	// assigns a freshly allocated slice to h.historyCache and never appends
	// in place on a header already handed out. Shallow copy before any such
	// mutation.
	historyCache     []discovery.RecentSession
	historyCacheTime time.Time
	// historyCacheTimeUnixNano mirrors historyCacheTime.UnixNano() so the
	// hot-path TTL check is wait-free (#1404). Writers MUST update it under
	// historyCacheMu together with historyCacheTime so fast-path readers never
	// see "fresh" before the slice is installed.
	historyCacheTimeUnixNano atomic.Int64
	historyCacheMu           sync.RWMutex
	historyFlight            singleflight.Group
	// warmHistoryWg tracks the WarmHistoryCache goroutine so server shutdown
	// can wait for the background FS scan before tearing down claudeDir state.
	warmHistoryWg sync.WaitGroup

	// Summary cache (30s TTL) — avoids re-running discovery.LookupSummaries
	// (N os.Stat + package-level lock) on every GET /api/sessions poll.
	summaryCache     map[string]string
	summaryCacheTime time.Time
	summaryCacheMu   sync.RWMutex
	// summaryFlight collapses concurrent misses at the TTL boundary into one
	// LookupSummaries (N×os.Stat) invocation; mirrors historyFlight.
	summaryFlight singleflight.Group

	// retiredStore stamps when a session left the live sidebar so history rows
	// carry retired_at (dashboard sorts by retired_at || last_active). nil
	// disables; ordering degrades to last_active only.
	retiredStore *discovery.RetiredStore

	// validateWS / systemInfoFn inject server-package helpers without a
	// reverse import.
	validateWS   func(ws, root string) (string, error)
	systemInfoFn func() map[string]any
}

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
	knownNodes := h.nodeAccess.KnownNodes()
	singleNode := len(knownNodes) == 0

	// sinceVersion is the storeGen the client last rendered; 0 (absent or
	// unparseable) forces a full build.
	clientETag := r.Header.Get("If-None-Match")
	sinceVersion := parseETagVersion(clientETag)

	snapshots, version, changed := h.router.ListSessionsIfChanged(sinceVersion)

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
		snapshots, version = h.router.ListSessionsWithVersion()
		if singleNode {
			etag = h.sessionsListETag(version)
		}
	}

	// Captured once so cutoff / uptime bucket share a single vDSO call.
	now := time.Now()

	snapshots, running, ready := filterAndCountSnapshots(snapshots, now)

	// Overlay tailer-side agent metrics; no-op when no Hub is wired (tests).
	if h.snapshotEnricher != nil {
		for i := range snapshots {
			h.snapshotEnricher(&snapshots[i])
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

// workspacesPool recycles the []string scratch that fillProjectAndSummary and
// loadHistorySessions hand to ProjectManager.ResolveWorkspaces on every poll
// (#616). ResolveWorkspaces never retains the backing array, so recycling is
// safe. Entries are *[]string so Put doesn't re-alloc a header; slices grown
// past 4096 are dropped on Put to bound the steady-state footprint.
var workspacesPool = sync.Pool{
	New: func() any {
		s := make([]string, 0, 32) // typical sidebar fits in this prefix
		return &s
	},
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
	if h.projectMgr != nil {
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
		wsMap := h.projectMgr.ResolveWorkspaces(workspaces)

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
	if h.claudeDir != "" {
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
	active, total := h.router.Stats()
	stats := sessionStats{
		sessionStatsStatic: h.staticStats,
		Active:             active,
		Running:            running,
		Ready:              ready,
		Total:              total,
		Version:            version,
		VersionTag:         h.versionTag,
		Uptime:             h.uptimeStringAt(now),
		Watchdog: watchdogStats{
			NoOutputKills: h.watchdogNoOut.Load(),
			TotalKills:    h.watchdogTotal.Load(),
		},
	}
	// cli_version in staticStats is the spawn-time value and goes stale after
	// a host claude upgrade; re-resolve from the live init-frame version each
	// poll (lock-free atomic read). Only overwrite when non-empty so an
	// unwired router can't blank the startup value.
	if live := h.router.CLIVersion(); live != "" {
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
	nodesSnapshot := h.nodeAccess.NodesSnapshot()

	// Box *SessionSnapshot rather than the 280 B value: pointer payloads sit
	// inline in the iface, and json.Marshal output is identical (#1402).
	allSessions := make([]any, 0, len(snapshots))
	for i := range snapshots {
		snapshots[i].Node = "local"
		allSessions = append(allSessions, &snapshots[i])
	}

	localName := h.workspaceName
	if localName == "" {
		localName = "Local"
	}
	nodeStatus := make(map[string]nodeStatusEntry, 1+len(nodesSnapshot)+len(knownNodes))
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

// maxEventsPageLimit caps the per-request history slice so a malicious or
// confused client can't force a full ring-buffer dump via ?limit=10000.
// 500 matches maxPersistedHistory — the upper bound of anything useful.
const maxEventsPageLimit = 500

// eventsBefore returns the entries strictly older than the `before` cursor
// (unix ms), preserving the input's chronological order. Always returns a
// non-nil slice so an exhausted page serialises as [] rather than null.
func eventsBefore(entries []clievent.EventEntry, before int64) []clievent.EventEntry {
	out := make([]clievent.EventEntry, 0, len(entries))
	for _, e := range entries {
		if e.Time < before {
			out = append(out, e)
		}
	}
	return out
}

// HandleEvents serves GET /api/sessions/events?key=&node=&after=&before=&limit=.
// `after` (ms) is an incremental fetch with Time >= after (watermark re-admitted,
// #2456; client dedups by uuid); `before` (ms) pages strictly older entries,
// newest `limit` of them in chronological order; `limit` alone sizes the
// initial page. `after` wins over `before`; no params returns full history.
func (h *Handlers) HandleEvents(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}
	// Same gate as the reverse-RPC fetch_events handler: rejects multi-KB or
	// control-byte keys before they reach slog attrs; also caps length at
	// MaxSessionKeyBytes.
	if err := sessionpkg.ValidateSessionKey(key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
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

	// Remote node proxy — the node RPC only carries `after`, so `before` /
	// `limit` pagination is emulated locally; older peers keep working.
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
		if beforeStr != "" && afterStr == "" {
			// "Load earlier" page (#2433): strictly older than the cursor, newest
			// `limit` of those, plus an authoritative has-more flag. An empty page
			// is the client's stop signal, so it must be [] with has-more=0.
			pageLimit := limit
			if pageLimit == 0 {
				pageLimit = maxEventsPageLimit
			}
			page := eventsBefore(entries, before)
			hasMore := len(page) > pageLimit
			if hasMore {
				page = page[len(page)-pageLimit:]
			}
			if hasMore {
				w.Header().Set("X-Events-Has-More", "1")
			} else {
				w.Header().Set("X-Events-Has-More", "0")
			}
			httputil.WriteJSON(w, page)
			return
		}
		// Page cap so legacy peers still yield a consistent-size payload.
		if limit > 0 && len(entries) > limit {
			entries = entries[len(entries)-limit:]
		}
		httputil.WriteJSON(w, entries)
		return
	}

	// Local
	sess := h.router.SessionFor(key)
	if sess == nil && h.scheduler != nil && h.scheduler.EnsureStub(key) {
		// Cron stubs torn down by sidebar "×" are lazily rebuilt on next click so
		// polling (WS-down) clients don't get a permanent 404 until the next tick.
		sess = h.router.SessionFor(key)
	}
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	var entries []clievent.EventEntry
	switch {
	case afterStr != "":
		entries = sess.EventEntriesSince(cli.SinceInclusive(after))
		if limit > 0 && len(entries) > limit {
			// Preserve the newest on a full catch-up so the client doesn't
			// miss events it just streamed through.
			entries = entries[len(entries)-limit:]
		}
	case beforeStr == "" && limit > 0:
		// Initial page (limit only): visible-aware read mirroring the WS
		// subscribe handshake, so a tail-N of internal-only events (agent team)
		// doesn't render the blank "该会话最近仅有 agent 活动" placeholder.
		visTarget := limit
		if visTarget > sessionpkg.DefaultVisibleTarget {
			visTarget = sessionpkg.DefaultVisibleTarget
		}
		// maxTotal=0 lets the reader use its own ceiling (ring size) so visible
		// bubbles beyond `limit` under an internal flood still surface.
		// X-Events-Has-More mirrors the WS "history" has_more field; it rides a
		// header so the bare-array body contract holds. Always set ("0"/"1") on
		// this branch; an absent header means legacy server / remote relay.
		var hasMore bool
		entries, hasMore = sess.EventInitialPageCtx(r.Context(), visTarget, 0)
		if hasMore {
			w.Header().Set("X-Events-Has-More", "1")
		} else {
			w.Header().Set("X-Events-Has-More", "0")
		}
	case beforeStr != "":
		pageLimit := limit
		if pageLimit == 0 {
			pageLimit = maxEventsPageLimit
		}
		// "Load earlier": a plain time-ordered page — the visible-aware reader
		// would skip internal events the operator is paging toward.
		// EventEntriesBeforeCtx falls back to the backend's history.Source
		// (JSONL for claude) when memory no longer holds entries older than
		// `before`; the request ctx lets a cancelled fetch unblock disk I/O.
		entries = sess.EventEntriesBeforeCtx(r.Context(), before, pageLimit)
	default:
		entries = sess.EventEntries()
	}

	httputil.WriteJSON(w, entries)
}

// HandleDelete serves DELETE /api/sessions. The key comes from the query
// string (?key=&node=, REST-idiomatic, wins when present) or the legacy JSON
// body {key, node} used by the dashboard frontend; both converge on the same
// validation + routing.
func (h *Handlers) HandleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key  string `json:"key"`
		Node string `json:"node"`
	}
	if q := r.URL.Query(); q.Get("key") != "" {
		req.Key = q.Get("key")
		req.Node = q.Get("node")
		// Drain + close body; MaxBytesReader still bounds a trailer-bomb.
		r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
		if err := httputil.DecodeJSONBody(r, &req); err != nil || req.Key == "" {
			http.Error(w, "key is required (pass ?key=... or JSON body)", http.StatusBadRequest)
			return
		}
	}
	// Same gate as HandleEvents: reject multi-KB / control-byte keys before
	// they reach the slog.Warn attr below; also caps at MaxSessionKeyBytes.
	if err := sessionpkg.ValidateSessionKey(req.Key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
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
			if contracts.IsUnknownRPCMethodErr(err) {
				// Peer runs an older binary without remove_session; 409 + explicit
				// body lets the dashboard show "upgrade needed" instead of a
				// generic failure.
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
		httputil.WriteOK(w)
		return
	}

	// RemoveAsync: the session leaves the router synchronously (200 truthfully
	// means "gone from the list, accepts no more messages") while the slow
	// teardown (proc.Close up to 8s + socket wait + cleanup) runs detached.
	if !h.router.RemoveAsync(req.Key) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	httputil.WriteOK(w)
}

// HandleSetLabel serves PATCH /api/sessions/label — the operator-set display
// label for a session. Empty label clears any prior value.
func (h *Handlers) HandleSetLabel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key   string `json:"key"`
		Node  string `json:"node"`
		Label string `json:"label"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	// Gate req.Key before it reaches slog attrs or router lookups (same policy
	// as HandleEvents / HandleDelete).
	if err := sessionpkg.ValidateSessionKey(req.Key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}

	label, err := sessionpkg.ValidateUserLabel(req.Label)
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
			// SanitizeLogAttr on node, key AND err.Error(): ValidateSessionKey
			// already rejects bidi / C0 / C1 / zero-width bytes in the key, but
			// req.Node is only validated against the discovery directory and a
			// malicious remote build can echo CR/LF or bidi runes in its RPC error
			// string, fragmenting the local slog audit trail (#820).
			slog.Warn("remote set session label failed",
				"node", sessionpkg.SanitizeLogAttr(req.Node),
				"key", sessionpkg.SanitizeLogAttr(req.Key),
				"err", sessionpkg.SanitizeLogAttr(err.Error()))
			if contracts.IsUnknownRPCMethodErr(err) {
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
		// Parallel audit entry with the local-path slog.Info so journalctl shows
		// every label change regardless of owning node; sanitised as above (#820).
		slog.Info("session label updated",
			"node", sessionpkg.SanitizeLogAttr(req.Node),
			"key", sessionpkg.SanitizeLogAttr(req.Key),
			"label_len", len(label))
		// Don't echo label — attacker-influenced text is a latent reflected-XSS
		// vector if a future caller renders the response via innerHTML. Client
		// patches its cache from its own optimistic value.
		httputil.WriteOK(w)
		return
	}

	if !h.router.SetUserLabel(req.Key, label) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// SanitizeLogAttr on key keeps the audit-log byte class uniform with the
	// remote path (#820).
	slog.Info("session label updated", "node", "local",
		"key", sessionpkg.SanitizeLogAttr(req.Key), "label_len", len(label))
	// Don't echo label — reflected-XSS precaution matches the remote-path
	// above. Client patches its cache from its own optimistic value.
	httputil.WriteOK(w)
}

// POST /api/sessions/resume
func (h *Handlers) HandleResume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID  string `json:"session_id"`
		Workspace  string `json:"workspace"`
		LastPrompt string `json:"last_prompt"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.SessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}
	if !discovery.IsValidSessionID(req.SessionID) {
		http.Error(w, "invalid session_id", http.StatusBadRequest)
		return
	}
	// Bound last_prompt: it is persisted and broadcast on every /api/sessions
	// poll, and control chars would inject into slog JSONHandler output.
	if len(req.LastPrompt) > maxResumeLastPromptBytes {
		http.Error(w, "last_prompt too long", http.StatusBadRequest)
		return
	}
	// Invalid UTF-8 is still rejected — a bad encoding usually indicates a
	// buggy client and carries no safe sanitization.
	if !utf8.ValidString(req.LastPrompt) {
		http.Error(w, "last_prompt is not valid utf-8", http.StatusBadRequest)
		return
	}
	// Control / bidi / LS-PS bytes are sanitized to "_" rather than rejected:
	// the slog-injection surface stays closed, yet sessions whose JSONL carries
	// CLI-injected control bytes (e.g. U+0085 NEL from PDF uploads) can still
	// resume from the history pane. last_prompt is display/log-only, so lossy
	// mapping is acceptable; tab is preserved.
	req.LastPrompt = SanitizeResumeLastPrompt(req.LastPrompt, maxResumeLastPromptBytes)

	workspace := req.Workspace
	if workspace != "" {
		var wsPath string
		var err error
		if h.validateWS != nil {
			wsPath, err = h.validateWS(workspace, h.allowedRoot)
		}
		if err != nil {
			// Keep the client-facing message decoupled from the error chain so a
			// wrapped *os.PathError can't leak resolved filesystem paths;
			// validateWorkspace already logs detail. Sanitize the workspace before
			// it lands in slog attrs — authenticated callers can slip bidi / C1 /
			// newline bytes past the structural path check.
			slog.Warn("resume workspace validation failed", "err", err, "workspace", osutil.SanitizeForLog(workspace, 256))
			httputil.WriteJSONStatus(w, http.StatusForbidden, map[string]string{"error": "invalid workspace"})
			return
		}
		workspace = wsPath
	}
	if workspace == "" {
		workspace = h.router.DefaultWorkspace()
	}

	// 16 random bytes (128 bits) so the resume key tail matches anonCookie /
	// upload IDs and the codebase's short-id entropy budget.
	var rb [16]byte
	if _, err := rand.Read(rb[:]); err != nil {
		// crypto/rand failures are pathologically rare; log so operators can
		// distinguish this from other 500s.
		slog.Error("resume register: generate key failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	key := "dashboard:direct:r" + hex.EncodeToString(rb[:]) + ":general"
	effectiveKey := h.router.RegisterForResume(key, req.SessionID, workspace, req.LastPrompt)

	httputil.WriteJSON(w, map[string]string{"status": "ok", "key": effectiveKey})
}

// POST /api/sessions/interrupt
func (h *Handlers) HandleInterrupt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key  string `json:"key"`
		Node string `json:"node"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, httputil.MaxRequestBodyBytes)
	if err := httputil.DecodeJSONBody(r, &req); err != nil || req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	// Gate req.Key before it reaches slog attrs / router lookup (same policy as
	// the other session handlers).
	if err := sessionpkg.ValidateSessionKey(req.Key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
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
			if contracts.IsUnknownRPCMethodErr(err) {
				http.Error(w, "remote node needs upgrade to support this action", http.StatusConflict)
				return
			}
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		if interrupted {
			slog.Info("remote session interrupted via HTTP", "node", req.Node, "key", req.Key)
			httputil.WriteOK(w)
		} else {
			httputil.WriteJSON(w, map[string]string{"status": "not_running"})
		}
		return
	}

	// Prefer control_request over SIGINT — see Router.InterruptSessionSafe
	// for why raw SIGINT on `-p` mode is destructive.
	switch h.router.InterruptSessionSafe(req.Key) {
	case sessionpkg.InterruptSent:
		slog.Info("session interrupted via HTTP", "key", req.Key)
		httputil.WriteOK(w)
	case sessionpkg.InterruptNoSession:
		httputil.WriteJSON(w, map[string]string{"status": "not_running"})
	default:
		httputil.WriteJSON(w, map[string]string{"status": "not_running"})
	}
}

// historySessions returns all filesystem sessions from the last 7 days.
// Results are cached for 120 seconds (see cacheTTL below).
func (h *Handlers) historySessions() []discovery.RecentSession {
	if h.claudeDir == "" {
		return nil
	}

	const cacheTTL = 120 * time.Second

	// Wait-free fast-path TTL check via the atomic mirror (#1404); the lock is
	// taken only when the TTL passes and we need a consistent slice header.
	if ns := h.historyCacheTimeUnixNano.Load(); ns != 0 && time.Since(time.Unix(0, ns)) < cacheTTL {
		h.historyCacheMu.RLock()
		// Re-confirm under lock — InvalidateHistoryCache could have written 0
		// between Load() and RLock().
		if !h.historyCacheTime.IsZero() && time.Since(h.historyCacheTime) < cacheTTL {
			cached := h.historyCache
			h.historyCacheMu.RUnlock()
			return cached
		}
		h.historyCacheMu.RUnlock()
	}

	v, _, _ := h.historyFlight.Do("history", func() (any, error) {
		// Re-check via the atomic — a prior leader may have populated the cache
		// before this closure ran (double-check pattern, as in
		// lookupSummariesCached). Population is judged by UnixNano != 0, not
		// historyCache != nil: an empty-history deployment legitimately caches a
		// nil slice and must not re-scan every TTL window.
		if ns := h.historyCacheTimeUnixNano.Load(); ns != 0 && time.Since(time.Unix(0, ns)) < cacheTTL {
			h.historyCacheMu.RLock()
			if !h.historyCacheTime.IsZero() && time.Since(h.historyCacheTime) < cacheTTL {
				cached := h.historyCache
				h.historyCacheMu.RUnlock()
				return cached, nil
			}
			h.historyCacheMu.RUnlock()
		}
		return h.loadHistorySessions(), nil
	})

	if res, ok := v.([]discovery.RecentSession); ok {
		return res
	}
	return nil
}

// uptimeSnapshot is the value cached by uptimeCache: Bucket is whole seconds
// since startedAt, Str the pre-formatted rendering at that resolution.
type uptimeSnapshot struct {
	Bucket int64
	Str    string
}

// uptimeStringAt returns time.Since(startedAt).Round(time.Second).String()
// memoised per 1-second bucket. Concurrent misses may format the same value;
// last-writer-wins via unconditional Store is intentional.
func (h *Handlers) uptimeStringAt(now time.Time) string {
	d := now.Sub(h.startedAt).Round(time.Second)
	bucket := int64(d / time.Second)
	if cur := h.uptimeCache.Load(); cur != nil && cur.Bucket == bucket {
		return cur.Str
	}
	s := d.String()
	h.uptimeCache.Store(&uptimeSnapshot{Bucket: bucket, Str: s})
	return s
}

// InitStaticStats pre-builds the immutable subset of /api/sessions stats so
// HandleList only overlays the dynamic counters per poll. The Once guards
// against a re-run racing with concurrent HandleList readers.
func (h *Handlers) InitStaticStats() {
	h.staticStatsOnce.Do(h.doInitStaticStats)
}

func (h *Handlers) doInitStaticStats() {
	// Deep-copy the callSystemInfo() singleton: staticStats is copied by value
	// per poll but the System map is a reference, so without this every
	// response would alias the process-wide map and any future mutable field
	// would be a data race.
	sysSrc := h.callSystemInfo()
	sysCopy := make(map[string]any, len(sysSrc))
	for k, v := range sysSrc {
		sysCopy[k] = v
	}
	// Copy agentIDs for the same read-only contract; guards against a future
	// mutable element type introducing a cross-goroutine race.
	agentsCopy := make([]string, len(h.agentIDs))
	copy(agentsCopy, h.agentIDs)
	h.staticStats = sessionStatsStatic{
		Backend:          h.backendTag,
		CLIName:          h.router.CLIName(),
		CLIVersion:       h.router.CLIVersion(),
		MaxProcs:         h.router.MaxProcs(),
		DefaultWorkspace: h.router.DefaultWorkspace(),
		WorkspaceID:      h.workspaceID,
		WorkspaceName:    h.workspaceName,
		System:           sysCopy,
		Agents:           agentsCopy,
	}
}

// WarmHistoryCache pre-populates the history cache in the background so the
// first dashboard load doesn't block on a full FS scan. The goroutine is
// tracked by warmHistoryWg so WaitWarmHistory can block shutdown until the
// scan finishes.
func (h *Handlers) WarmHistoryCache() {
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
func (h *Handlers) WaitWarmHistory() {
	h.warmHistoryWg.Wait()
}

// InvalidateHistoryCache forces the next poll to repopulate historyCache from
// disk. Wired into Router.SetOnKeyRetired so a just-retired session's jsonl
// appears in the history popover within one poll instead of up to 120s later.
func (h *Handlers) InvalidateHistoryCache() {
	h.historyCacheMu.Lock()
	h.historyCache = nil
	h.historyCacheTime = time.Time{}
	// Keep the atomic mirror in lockstep (Store=0 ⇔ time.Time{}) so wait-free
	// readers see the invalidation immediately.
	h.historyCacheTimeUnixNano.Store(0)
	h.historyCacheMu.Unlock()
}

// lookupSummariesCached returns sessionID→summary with a 30s TTL cache. The
// full lookup result is stored and served to any snapshot subset; concurrent
// misses at the TTL boundary collapse via summaryFlight so N tab polls don't
// each pay the N×os.Stat fan-out.
func (h *Handlers) lookupSummariesCached(snapshots []sessionpkg.SessionSnapshot) map[string]string {
	const summaryTTL = 30 * time.Second

	h.summaryCacheMu.RLock()
	if h.summaryCache != nil && time.Since(h.summaryCacheTime) < summaryTTL {
		cached := h.summaryCache
		h.summaryCacheMu.RUnlock()
		return cached
	}
	h.summaryCacheMu.RUnlock()

	// Fixed "summary" flight key: the leader's result is cached for the whole
	// 30s window regardless of which subset drove the miss. sessionWorkspaces is
	// built INSIDE the closure so followers don't pay an O(N) map they'd
	// discard; whichever caller's snapshots win the race is acceptable for a
	// 30s window.
	v, _, _ := h.summaryFlight.Do("summary", func() (any, error) {
		// Re-check under lock — a prior leader could have populated the
		// cache between our expiry detection and this closure running.
		h.summaryCacheMu.RLock()
		if h.summaryCache != nil && time.Since(h.summaryCacheTime) < summaryTTL {
			cached := h.summaryCache
			h.summaryCacheMu.RUnlock()
			return cached, nil
		}
		h.summaryCacheMu.RUnlock()

		// Only look up sessions that don't already carry a Summary (#1403): the
		// fill loop is a no-op on missing keys, so wire output is identical
		// while a cold miss shrinks the fan-out to O(N_new). Sized worst-case so
		// the poll right after a workspace switch doesn't pay map growth.
		sessionWorkspaces := make(map[string]string, len(snapshots))
		for _, snap := range snapshots {
			if snap.SessionID == "" || snap.Workspace == "" {
				continue
			}
			if snap.Summary != "" {
				continue
			}
			sessionWorkspaces[snap.SessionID] = snap.Workspace
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

// RecordRetired stamps the retirement instant for sessionID and invalidates
// the history cache so the new ordering shows on the next poll. No-op when
// the store is unconfigured or sessionID is empty (CLI never returned a UUID).
func (h *Handlers) RecordRetired(sessionID string) {
	if h.retiredStore == nil || sessionID == "" {
		return
	}
	h.retiredStore.MarkRetired(sessionID, time.Now())
	h.InvalidateHistoryCache()
}

// FlushRetiredStore writes pending retired-at marks to disk at server
// shutdown. No-op without a store; errors are logged, not returned, so
// shutdown doesn't fail.
func (h *Handlers) FlushRetiredStore() {
	if h.retiredStore == nil {
		return
	}
	if err := h.retiredStore.Save(); err != nil {
		slog.Warn("flush retired store failed", "err", err)
	}
}

// historyScanTimeout bounds the loadHistorySessions FS walk, which runs inside
// the singleflight leader and would otherwise stall every poller on a hung
// filesystem (#2134).
const historyScanTimeout = 5 * time.Second

func (h *Handlers) loadHistorySessions() []discovery.RecentSession {
	excludeIDs := h.router.DiscoveryExcludeIDs()

	// Hide cron-spawned and sys-session JSONLs (both have their own UI; the sys
	// workdir lives under ~/.claude/projects and would leak AutoTitler prompts).
	// KnownSessionIDs is O(jobs × 200), so snapshot it once per scan.
	filter := historyFilter{skipWorkspace: h.sysWorkDir}
	if h.cronSessions != nil {
		filter.skipSessions = h.cronSessions.KnownSessionIDs()
	}
	// Cap the walk so a slow/hung home (NFS, FUSE) can't pin the flight leader (#2134).
	ctx, cancel := context.WithTimeout(context.Background(), historyScanTimeout)
	defer cancel()
	all := discovery.RecentSessionsCtx(ctx, h.claudeDir, 200, 7*24*time.Hour, excludeIDs, filter)

	// Resolve project names in batch using the pooled scratch slice (#616).
	if h.projectMgr != nil && len(all) > 0 {
		wsPtr := borrowWorkspaces(len(all))
		workspaces := *wsPtr
		for _, rs := range all {
			workspaces = append(workspaces, rs.Workspace)
		}
		*wsPtr = workspaces
		wsMap := h.projectMgr.ResolveWorkspaces(workspaces)
		returnWorkspaces(wsPtr)
		for i := range all {
			all[i].Project = wsMap[all[i].Workspace]
		}
	}

	// Stamp retired_at from one Snapshot() so the loop is O(N) without N mutex
	// acquires.
	if h.retiredStore != nil && len(all) > 0 {
		retiredMap := h.retiredStore.Snapshot()
		if len(retiredMap) > 0 {
			for i := range all {
				if ts := retiredMap[all[i].SessionID]; ts > 0 {
					all[i].RetiredAt = ts
				}
			}
		}
	}

	now := time.Now() // outside the lock to keep vDSO off the critical section
	h.historyCacheMu.Lock()
	h.historyCache = all
	h.historyCacheTime = now
	// Mirror update under the lock so wait-free readers never see atomic-fresh
	// while h.historyCache still points at the old slice.
	h.historyCacheTimeUnixNano.Store(now.UnixNano())
	h.historyCacheMu.Unlock()

	return all
}

// callSystemInfo invokes the injected systemInfoFn. nil falls through to
// an empty map (test paths without system-probe wiring).
func (h *Handlers) callSystemInfo() map[string]any {
	if h.systemInfoFn == nil {
		return map[string]any{}
	}
	return h.systemInfoFn()
}

// Deps bundles all wiring for New so internal/server can construct a Handlers
// without access to unexported fields.
type Deps struct {
	Router        *sessionpkg.Router
	ProjectMgr    *project.Manager
	Scheduler     CronView
	CronSessions  CronView
	SysWorkDir    string
	ClaudeDir     string
	AllowedRoot   string
	Agents        map[string]sessionpkg.AgentOpts
	AgentIDs      []string
	NodeAccess    NodeAccessor
	NodeCache     *node.CacheManager
	StartedAt     time.Time
	BackendTag    string
	WorkspaceID   string
	WorkspaceName string
	VersionTag    string
	WatchdogNoOut *atomic.Int64
	WatchdogTotal *atomic.Int64
	RetiredStore  *discovery.RetiredStore
	ValidateWS    func(ws, root string) (string, error)
	SystemInfoFn  func() map[string]any
	// ProjectStableKeyEnabled toggles the stableKey field in stats.projects
	// (same switch as the /api/projects list).
	ProjectStableKeyEnabled bool
}

// New constructs a Handlers from injected deps.
func New(d Deps) *Handlers {
	return &Handlers{
		router:        d.Router,
		projectMgr:    d.ProjectMgr,
		scheduler:     d.Scheduler,
		cronSessions:  d.CronSessions,
		sysWorkDir:    d.SysWorkDir,
		claudeDir:     d.ClaudeDir,
		allowedRoot:   d.AllowedRoot,
		agents:        d.Agents,
		agentIDs:      d.AgentIDs,
		nodeAccess:    d.NodeAccess,
		nodeCache:     d.NodeCache,
		startedAt:     d.StartedAt,
		backendTag:    d.BackendTag,
		workspaceID:   d.WorkspaceID,
		workspaceName: d.WorkspaceName,
		versionTag:    d.VersionTag,
		watchdogNoOut: d.WatchdogNoOut,
		watchdogTotal: d.WatchdogTotal,
		retiredStore:  d.RetiredStore,
		validateWS:    d.ValidateWS,
		systemInfoFn:  d.SystemInfoFn,

		projectStableKeyEnabled: d.ProjectStableKeyEnabled,
	}
}

// SetSnapshotEnricher wires the optional Hub.enrichSnapshot callback
// after the Hub is constructed (registerDashboard runs after server.New
// so the hub field can't be passed in via Deps).
func (h *Handlers) SetSnapshotEnricher(fn func(*sessionpkg.SessionSnapshot)) {
	h.snapshotEnricher = fn
}

// RetiredStorePresent reports whether the RetiredStore is wired (server
// shutdown Prune gate).
func (h *Handlers) RetiredStorePresent() bool { return h.retiredStore != nil }

// PruneRetiredStore prunes entries older than cutoffMs; no-op when unwired.
func (h *Handlers) PruneRetiredStore(cutoffMs int64) {
	if h.retiredStore != nil {
		h.retiredStore.Prune(cutoffMs)
	}
}
