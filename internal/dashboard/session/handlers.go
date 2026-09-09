package session

import (
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/naozhi/naozhi/internal/dashboard/cronview"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/osutil"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
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
//
// It holds its wiring as one Deps value rather than a field per dependency
// (#2635). Until then New copied 23 Deps fields into 23 same-named private
// fields — the same state twice, with a second set of comments to drift and a
// struct literal in tests that had to name unexported fields. Everything below
// deps is runtime state the handlers OWN (caches, single-flight groups,
// waitgroups); nothing there is injected.
type Handlers struct {
	// deps is read-only after New. The few test-only mutators live in
	// test_setters.go and are the only writers.
	deps Deps

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

// maxEventsPageLimit caps the per-request history slice so a malicious or
// confused client can't force a full ring-buffer dump via ?limit=10000.
// 500 matches maxPersistedHistory — the upper bound of anything useful.
const maxEventsPageLimit = 500

// historyScanTimeout bounds the loadHistorySessions FS walk, which runs inside
// the singleflight leader and would otherwise stall every poller on a hung
// filesystem (#2134).
const historyScanTimeout = 5 * time.Second

// Deps bundles all wiring for New so internal/server can construct a Handlers
// without access to unexported fields.
type Deps struct {
	// SnapshotEnricher is Hub.enrichSnapshot: it folds live agent-tailer state
	// into each /api/sessions snapshot. Optional (nil skips enrichment). Wired
	// here rather than through SetSnapshotEnricher afterwards (#2552) — the Hub
	// exists before these handlers do, so there is no ordering window to cover.
	SnapshotEnricher func(*sessionpkg.SessionSnapshot)

	Router     RouterView
	ProjectMgr ProjectSource
	// Scheduler is optional; HandleEvents uses it to revive dismissed cron
	// stubs (EnsureStub).
	Scheduler CronView
	// CronSessions feeds KnownSessionIDs() to the history panel; nil disables
	// filtering cron-spawned JSONLs. Kept separate from Scheduler so server.go
	// can nil either independently (#754).
	CronSessions CronView
	// SysWorkDir is sysession's transient Runner workspace; when non-empty its
	// JSONLs are hidden from the history panel (AutoTitler otherwise leaks
	// prompt fragments into "recent sessions").
	SysWorkDir  string
	ClaudeDir   string
	AllowedRoot string
	Agents      map[string]sessionpkg.AgentOpts
	// AgentIDs is precomputed once by the wiring side (Agents is immutable
	// after startup).
	AgentIDs   []string
	NodeAccess NodeAccessor
	NodeCache  NodeCacheReader

	// Static status fields (immutable after construction).
	StartedAt     time.Time
	BackendTag    string
	WorkspaceID   string
	WorkspaceName string
	// VersionTag is the build tag surfaced as sessionStats.VersionTag; empty
	// means unknown and is omitted from JSON.
	VersionTag    string
	WatchdogNoOut *atomic.Int64
	WatchdogTotal *atomic.Int64
	// RetiredStore stamps when a session left the live sidebar so history rows
	// carry retired_at (dashboard sorts by retired_at || last_active). nil
	// disables; ordering degrades to last_active only.
	RetiredStore RetiredReader
	// ValidateWS / SystemInfoFn inject server-package helpers without a
	// reverse import.
	ValidateWS   func(ws, root string) (string, error)
	SystemInfoFn func() map[string]any
	// ProjectStableKeyEnabled toggles the stableKey field in stats.projects
	// (same switch as the /api/projects list).
	ProjectStableKeyEnabled bool
}

// New constructs a Handlers from injected deps. Deps is stored as-is; the
// documentation for each dependency lives on the Deps field, not on a
// duplicate here.
func New(d Deps) *Handlers {
	return &Handlers{deps: d}
}

// ContractStats exposes the /api/sessions "stats" wire struct to the
// contract.js generator (#2539) — reflect-only; the type stays unexported.
var ContractStats any = sessionStats{}
