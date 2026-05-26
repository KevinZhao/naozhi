package server

import (
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
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/textutil"
)

// maxResumeLastPromptBytes caps the last_prompt field on /api/sessions/resume.
// The body-level MaxBytesReader is 1 MiB; this field-level cap prevents a
// megabyte-scale string from being persisted on the session and then echoed
// to every dashboard client on each /api/sessions poll.
const maxResumeLastPromptBytes = 2 * 1024

// sanitizeResumeLastPrompt strips injection-prone bytes from a resume
// last_prompt before it reaches slog attrs or /api/sessions broadcasts,
// while preserving tab (operators paste tab-delimited snippets and slog
// JSONHandler escapes tab safely).
//
// Mirrors osutil.SanitizeForLog except for the tab carve-out. Inlined here
// because the tab allowance is a dashboard-specific relaxation — ordinary
// log attrs should keep the stricter rule.
func sanitizeResumeLastPrompt(s string, maxLen int) string {
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
		// Truncate at a rune boundary so we never split a multi-byte UTF-8
		// codepoint — the result feeds into sessions.json and the dashboard
		// UI, where invalid UTF-8 surfaces as garbled glyphs.
		mapped = mapped[:textutil.TruncateAtRuneBoundary(mapped, maxLen)]
	}
	return mapped
}

// Note: user-label validation lives in the session package
// (session.ValidateUserLabel / session.MaxUserLabelBytes) so the dashboard
// HTTP path and the reverse-RPC worker (internal/upstream) share one
// implementation. R64-GO-H3 / L1 / L2 consolidated the rules there.

// workspaceFallbackName returns the folder name to display as a session's
// sidebar group when the workspace is not registered with ProjectManager.
// Returns an empty string for inputs that are empty, root ("/"), or resolve
// to "." — these cannot produce a meaningful group label so the frontend
// falls back to the generic catch-all instead.
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

// watchdogStats is the /api/sessions "watchdog" sub-object. Declared as a
// named struct (not an inline map[string]any) so json/reflect caches the
// type descriptor once and the value is stack-allocated per response,
// eliminating the per-poll 2-key map heap alloc the dashboard hot path
// used to pay. R58-PERF-F2.
type watchdogStats struct {
	NoOutputKills int64 `json:"no_output_kills"`
	TotalKills    int64 `json:"total_kills"`
}

// sessionStatsStatic holds the subset of /api/sessions.stats fields that are
// immutable after server startup. Pre-built once by initStaticStats and then
// embedded (by value) into sessionStats on every poll — the copy is a
// fixed-size struct on the stack, not a 9-key map clone with per-key
// interface{} boxing like the previous map[string]any implementation.
// Embedding keeps the JSON output flat (all fields promoted to top-level of
// the "stats" object), preserving byte-identical shape with the prior
// map-based response for dashboard.js and any curl/monitoring consumers.
//
// `system` stays a map[string]any to reuse initStaticStats's deep-copy path
// (the systemInfo() singleton map is process-wide and must not alias into
// per-response allocations; see initStaticStats comments). Keeping the field
// typed as a map preserves that contract while still collapsing the rest of
// the stats object to a struct. R70-PERF-H1 / R68-PERF-H3 / R59-PERF-001 /
// R51-PERF-005 / R49-PERF-STATS-STRUCT / R43-PERF-P43-1 / R54-PERF-001
// (all the same underlying hot-path alloc).
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

// sessionStats is the full "stats" sub-object returned from GET /api/sessions.
// Prior code built this as a 12+ key map[string]any literal on every poll;
// this named struct holds the static subset by anonymous embed (JSON fields
// promote flat) and the dynamic counters + version + uptime + watchdog
// inline, with `projects` omitempty when the dashboard has no configured
// projects. Marshals byte-identically to the prior map shape so dashboard.js
// consumers (stats.agents / stats.default_workspace / stats.projects /
// stats.cli_name / stats.cli_version / stats.workspace_id / stats.workspace_name
// / stats.system / stats.version) see the same keys in the same order.
type sessionStats struct {
	sessionStatsStatic
	Active  int    `json:"active"`
	Running int    `json:"running"`
	Ready   int    `json:"ready"`
	Total   int    `json:"total"`
	Version uint64 `json:"version"`
	// VersionTag is the naozhi build tag (e.g. `git describe` output).
	// Surfaced separately from the uint64 `version` counter (which tracks
	// session-store mutations) so dashboard.js can render a footer like
	// "naozhi v1.2.3-dirty · dark" without conflating with the poll-version
	// field. Omitempty preserves the legacy wire shape when the ldflag is
	// unset (e.g. `go run` without -X, or `go build` without Makefile).
	VersionTag string             `json:"version_tag,omitempty"`
	Uptime     string             `json:"uptime"`
	Watchdog   watchdogStats      `json:"watchdog"`
	Projects   []projectListEntry `json:"projects,omitempty"`
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

// sessionListLocalResp is the /api/sessions response shape for single-node
// deployments (no remote nodes configured). Replaces a per-poll
// `make(map[string]any, 3)` + 3 string-keyed boxed assignments — a struct
// literal marshals byte-identically (sessions / stats / history_sessions
// keys in the same order as the prior map iteration order Go fixed for
// json.Marshal at v1.12+) while skipping the map bucket alloc + 3
// interface{} boxes per request. `history_sessions` is omitempty so
// deployments without JSONL history serialize the same 2-key object as
// before. R226-PERF-7.
type sessionListLocalResp struct {
	Sessions        []session.SessionSnapshot `json:"sessions"`
	Stats           sessionStats              `json:"stats"`
	HistorySessions []discovery.RecentSession `json:"history_sessions,omitempty"`
}

// sessionListMultiResp is the /api/sessions response shape for multi-node
// deployments (>=1 configured remote node). `Sessions` is []any because
// the multi-node merge concatenates local SessionSnapshot values with
// remote node session entries (arbitrary map[string]any decoded from
// peer JSON). `Nodes` has no omitempty: this struct is only used when
// the node map is populated, so the field is always present in the
// JSON output, matching the prior `resp["nodes"] = nodeStatus`
// unconditional assignment. `HistorySessions` keeps omitempty for the
// same reason as the single-node variant. R226-PERF-7.
type sessionListMultiResp struct {
	Sessions        []any                      `json:"sessions"`
	Stats           sessionStats               `json:"stats"`
	Nodes           map[string]nodeStatusEntry `json:"nodes"`
	HistorySessions []discovery.RecentSession  `json:"history_sessions,omitempty"`
}

// projectListEntry is the per-project element in /api/sessions "stats.projects".
// Named struct (vs map[string]any{6 keys}) eliminates P inner-map allocs and
// 6×P interface{} boxing ops per 1 Hz dashboard poll. `omitempty` tags
// preserve the previous JSON shape: local rows without a git remote, or
// remote-cached rows that didn't round-trip favorite/github, simply drop
// those keys instead of emitting false/"". dashboard.js consumes
// name/path/node/favorite/git_remote_url/github via `p.favorite`, `p.name`,
// etc. — all six are bool-or-string so struct marshaling is byte-equivalent
// to the prior map literal. R70-PERF-M1 / R67-PERF-2 (struct variant).
type projectListEntry struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Node         string `json:"node"`
	Favorite     bool   `json:"favorite,omitempty"`
	GitRemoteURL string `json:"git_remote_url,omitempty"`
	GitHub       bool   `json:"github,omitempty"`
	// CreatedAt anchors the project's sidebar order: the dashboard sorts
	// projects by this value ascending so newly-added folders always land at
	// the bottom of their tier. unix ms.
	CreatedAt int64 `json:"created_at,omitempty"`
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

// CronView is the consolidated narrow consumer interface the server
// package needs from *cron.Scheduler. R242-ARCH-13 (#754) collapses three
// previously-separate single-method shapes — cronHubOps (EnsureStub +
// SetJobPrompt, used by the Hub's auto-save-prompt path), cronStubChecker
// (EnsureStub, used by SessionHandlers.handleEvents to revive dismissed
// cron stubs) and cronSessionLister (KnownSessionIDs, used by
// loadHistorySessions to hide cron-spawned JSONLs from the catch-all
// history panel) — into one interface so reviewers and test authors only
// have to learn one shape.
//
// *cron.Scheduler satisfies CronView implicitly. Defined in the server
// package (not in cron) so server's coupling to cron stays at the three
// methods we actually call rather than the full Scheduler API. Lineage:
// R228-ARCH-17 (cronStubChecker) → R232-ARCH-7 (cronHubOps) → R245-ARCH
// (cronSessionLister) → R242-ARCH-13 (CronView).
type CronView interface {
	EnsureStub(key string) bool
	SetJobPrompt(jobID, prompt string) error
	KnownSessionIDs() map[string]bool
}

// historyFilter is the discovery.RecentSessionsFilter loadHistorySessions
// constructs each scan.  Snapshots the cron-known set + sys workspace
// once per call so the in-loop predicate is O(1) per session.
type historyFilter struct {
	skipWorkspace string          // sys-sessions absolute path; "" disables
	skipSessions  map[string]bool // cron known IDs; nil disables
}

func (f historyFilter) SkipWorkspace(ws string) bool {
	return f.skipWorkspace != "" && ws == f.skipWorkspace
}

func (f historyFilter) SkipSessionID(sid string) bool {
	return f.skipSessions != nil && f.skipSessions[sid]
}

// SessionHandlers groups the session list, events, delete, and resume API endpoints.
type SessionHandlers struct {
	router     *session.Router
	projectMgr *project.Manager
	scheduler  CronView // optional; used by handleEvents to revive dismissed cron stubs (EnsureStub)
	// cronSessions is the optional Scheduler-side view consulted when
	// building the history panel via KnownSessionIDs(). When nil, cron-spawned
	// JSONLs are NOT filtered from history (degraded behaviour matches pre-R245).
	// The underlying type is *cron.Scheduler in production; tests may inject
	// a stub. R245-ARCH (cron+sys hide-from-history).
	//
	// scheduler and cronSessions remain two separate CronView fields rather
	// than a single shared one because production wiring (server.go) must
	// be allowed to nil either independently — e.g. to disable history
	// filtering while keeping stub revival, or vice versa. Both are typed
	// CronView so a single concrete *cron.Scheduler can satisfy both.
	// R242-ARCH-13 (#754).
	cronSessions CronView
	// sysWorkDir is the absolute filesystem path used by sysession's
	// transient claude -p Runner.  When non-empty, every JSONL under
	// this workspace path is hidden from the history panel — AutoTitler
	// otherwise leaks prompt fragments into the catch-all "recent
	// sessions" list. Empty (typical in tests / disabled sysession)
	// degrades cleanly. R245-ARCH.
	sysWorkDir  string
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
	// versionTag is the naozhi build tag piped into sessionStats.VersionTag
	// on every poll. Immutable after construction. Empty means "unknown"
	// (e.g. `go run` with no -X main.version ldflag) and is omitted from
	// the JSON response via omitempty.
	versionTag    string
	watchdogNoOut *atomic.Int64
	watchdogTotal *atomic.Int64

	// snapshotEnricher is an optional hook wired from server.go to
	// Hub.enrichSnapshot so SubagentInfo rows in /api/sessions responses
	// carry the tailer-side LastTool / ToolUses / DurationMS that never
	// appear in the parent stream. nil in tests that don't build a Hub.
	snapshotEnricher func(*session.SessionSnapshot)

	// uptimeCache memoises the formatted uptime string at 1-second resolution.
	// handleList is hit at 1 Hz × N dashboard tabs, and
	// time.Since(startedAt).Round(time.Second).String() allocates a short
	// string on every call — roughly (N-1)/N of those allocations sit inside
	// the same 1-second bucket. Caching the string with its bucket-id (seconds
	// since start) lets all pollers within the same second reuse one alloc.
	// Races are benign: concurrent misses re-format the same value. R65-PERF-L-1.
	uptimeCache atomic.Pointer[uptimeSnapshot]

	// projectListCache memoises the projectList slice built in handleList at
	// 1-second resolution, sharing one rebuild across N dashboard tabs polling
	// at 1 Hz. Each tab opening adds (len(projects) ≤ ~50) projectListEntry
	// allocations + redactGitRemoteURL calls per second; with the cache N tabs
	// collapse to 1 rebuild/s instead of N. The cached slice is read-only —
	// handleList copies the header into stats.Projects, never mutating it —
	// so multiple readers can safely share the same backing array within a
	// bucket. Misses re-build identically; last-writer-wins via Store is
	// intentional (the formatted slice still escapes to the response).
	//
	// 1s resolution is chosen over a Manager-version invalidation hook because
	// (a) project mutations are minute-scale (operator clicks vs poll Hz), so
	// 1s lag is invisible to humans; (b) versioning project.Manager would
	// touch a package outside this file's domain. R247-PERF-15 [REPEAT-3].
	projectListCache atomic.Pointer[projectListSnapshot]

	// staticStats pre-builds the subset of /api/sessions stats fields that
	// are immutable after startup (backend, cli_name, workspace_*, system,
	// agents). handleList copies this struct by value on each poll instead
	// of rebuilding a 9-key map literal — a struct copy is a single
	// stack-local memmove vs per-key interface{} boxing + map bucket alloc.
	// Initialized once by initStaticStats() after all fields are set.
	// Round 79 upgrade from map[string]any → named struct.
	staticStats sessionStatsStatic
	// staticStatsOnce enforces the "initStaticStats called exactly once"
	// contract structurally. A test double or future refactor that calls
	// initStaticStats twice would otherwise race with concurrent handleList
	// readers, who read staticStats without synchronisation. R61-GO-12.
	staticStatsOnce sync.Once

	// History cache (120s TTL — see cacheTTL in historySessions).
	//
	// ALIASING CONTRACT (R62-GO-5): cache hits return the slice *header* only,
	// not a copy. Multiple readers end up with slice values that alias the same
	// backing array, which is race-free in Go — the backing array is allocated
	// fresh by loadHistorySessions() (a `make + append` pipeline that discards
	// the old backing array on TTL expiry), not mutated in place. Concurrent
	// readers may observe the array alive past the mutex release because Go's
	// GC keeps it reachable through every slice header still referencing it.
	//
	// The invariant writers MUST preserve: ANY refresh path (loadHistorySessions,
	// WarmHistoryCache, future features) must assign a freshly allocated slice
	// to h.historyCache, never mutate the existing backing array via
	// append-in-place on a header already handed out. Shallow copy before any
	// such mutation. Breaking this invariant produces cross-reader data
	// corruption indistinguishable from a classic data race.
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

	// retiredStore stamps the unix-ms instant a session left the live
	// sidebar (Router.Reset / Router.Remove) so loadHistorySessions can
	// emit retired_at on each RecentSession. The dashboard then sorts the
	// history popover by retired_at || last_active, putting the most
	// recently closed panel on top regardless of when its JSONL was last
	// written. nil disables the feature; the response degrades to
	// last_active-only ordering. See discovery.RetiredStore godoc.
	retiredStore *discovery.RetiredStore
}

// GET /api/sessions
//
// R246-CR-002 split (#736): handleList previously combined cutoff filter +
// state count + project mapping + summary lookup + stats build + node merge
// + JSON shape selection in one ~300 line function. The body now orchestrates
// focused helpers; each helper is independently testable and the per-helper
// docstring states its mutation contract:
//   - filterAndCountSnapshots — sidebar cutoff + scratch/cron/sys filter +
//     running/ready counts in a single pass
//   - fillProjectAndSummary  — workspace → project name + summaries-index
//     lookup; mutates snapshots in place
//   - buildSessionStats      — typed stats payload (no map[string]any boxing)
//   - buildLocalResp         — single-node JSON shape
//   - buildMultiNodeResp     — multi-node merge (live + cached) JSON shape
//
// Performance comments + race anchors stay on the helpers rather than this
// orchestrator so reviewers don't have to hold the whole pipeline in their
// head while reading a single concern.
func (h *SessionHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	// R246-PERF-15 (#726): read snapshots and storeGen in a single
	// r.mu.RLock epoch via ListSessionsWithVersion. The pre-existing
	// two-call pattern (Version → ListSessions) intentionally chose
	// version-first ordering as the "version ≤ data" safer side per
	// R60-GO-M3 — under that ordering a mutation landing between the
	// two reads produced data at gen N+1 tagged with version N, which
	// the next poll (seeing N+1) would re-fetch and catch up on. The
	// new tuple closes the window entirely: response.version is
	// exactly the version that produced the snapshot slice, so the
	// dashboard's version-gate neither skips a refresh nor repeats a
	// render.
	snapshots, version := h.router.ListSessionsWithVersion()

	// Capture once so downstream cutoff / uptime bucket computations share a
	// single vDSO call rather than the 2 previously paid per poll. R67-PERF-4.
	now := time.Now()

	snapshots, running, ready := filterAndCountSnapshots(snapshots, now)

	// Overlay tailer-side agent metrics (RFC v4 §3.5.4). No-op when the
	// hub tailer registry is empty or hasn't been wired — safe for tests
	// that build SessionHandlers without a Hub.
	if h.snapshotEnricher != nil {
		for i := range snapshots {
			h.snapshotEnricher(&snapshots[i])
		}
	}

	h.fillProjectAndSummary(snapshots)

	stats := h.buildSessionStats(now, version, running, ready)

	// KnownNodes returns an immutable snapshot without acquiring the
	// nodeAccess lock; NodesSnapshot does both. Single-node deployments
	// (the common case) have len(knownNodes)==0 and never need the live
	// snapshot — check KnownNodes first and short-circuit before paying
	// the NodesSnapshot RLock + map alloc.
	knownNodes := h.nodeAccess.KnownNodes()

	if len(knownNodes) == 0 {
		writeJSON(w, h.buildLocalResp(snapshots, stats))
		return
	}

	writeJSON(w, h.buildMultiNodeResp(snapshots, stats, knownNodes))
}

// filterAndCountSnapshots walks the router snapshot exactly once to:
//
//  1. evict dead sessions whose LastActive is older than 24h (sidebar TTL),
//  2. count running / ready sessions across ALL surviving entries (so the
//     maxProcs pressure indicator stays correct even for scratch / cron /
//     sys sessions that don't show up in the sidebar),
//  3. drop scratch / cron / sys keys from the returned slice — those
//     surfaces own dedicated dashboard panels (drawer / 「定时任务」 /
//     System) and must not duplicate-render in the sidebar.
//
// The function compacts in place: the returned slice aliases the input
// header but with len shrunk to the number of sidebar-eligible entries,
// so callers must not retain the original header after this call.
//
// R246-CR-002 split (#736): previously inlined into handleList; the merged
// filter+count pass (rather than two walks) was deliberate for hot-path
// alloc reasons and the performance contract is preserved here.
func filterAndCountSnapshots(snapshots []session.SessionSnapshot, now time.Time) ([]session.SessionSnapshot, int, int) {
	cutoff24h := now.Add(-24 * time.Hour).UnixMilli()
	var running, ready int
	n := 0
	for _, snap := range snapshots {
		if snap.DeathReason != "" && snap.LastActive < cutoff24h {
			continue
		}
		// Always count running/ready first so maxProcs pressure stays visible
		// in stats regardless of whether the session is sidebar-eligible.
		switch snap.State {
		case "running":
			running++
		case "ready":
			ready++
		}
		// Scratch (ephemeral aside) sessions own a CLI process and therefore
		// show up in router.ListSessions, but the drawer UX treats them as
		// private to one dashboard tab. Cron flows through the dedicated
		// 「定时任务」panel (cron-panel-consolidation RFC), and sys: daemons
		// are naozhi-internal infrastructure surfaced via the System drawer
		// (docs/rfc/system-session.md §9.2). None of them belong in the
		// sidebar listing.
		if session.IsScratchKey(snap.Key) || session.IsCronKey(snap.Key) || session.IsSysKey(snap.Key) {
			continue
		}
		snapshots[n] = snap
		n++
	}
	return snapshots[:n], running, ready
}

// workspacesPool reuses the []string scratch slice fillProjectAndSummary
// + loadHistorySessions hand to ProjectManager.ResolveWorkspaces on every
// /api/sessions poll (1 Hz × N tabs) and every history scan. R217-PERF-10
// (#616): the previous per-call `make([]string, 0, len(snapshots))` showed
// up in heap profiles on session-heavy dashboards. ResolveWorkspaces
// reads the header inside its own RLock and never retains the backing
// array, so a pool entry is safe to recycle once the call returns.
//
// Each pool entry is a *[]string so the runtime can elide the per-Get
// alloc on the typed pointer wrapper too — directly pooling []string
// would still alloc a new header on every Put because slice values are
// non-pointer. The slice we hand out has cap >= the requested size and
// len reset to 0; callers append fresh data and Put back the same
// pointer. A grown slice (cap > 4096) is dropped on Put so a single
// pathological request cannot inflate every pool entry's footprint.
//
// Concurrency: sync.Pool is safe; the per-tab calls never share a
// borrowed slice. The cap-bounding contract on Put ensures the pool's
// steady-state working-set stays bounded by the typical session count.
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
		// Grow once to the request size + slack rather than letting
		// append's geometric growth stamp out a fresh backing array on
		// each call. Bounded by the snapshot length so a deployment with
		// thousands of sessions does not over-allocate.
		s = make([]string, 0, want)
	} else {
		s = s[:0]
	}
	*p = s
	return p
}

// returnWorkspaces hands the recycled slice back to the pool. Slices
// whose backing array has been grown past the cap-bounding threshold are
// dropped so a single oversized poll cannot inflate every pool entry's
// retained footprint.
func returnWorkspaces(p *[]string) {
	if p == nil {
		return
	}
	const maxRetainCap = 4096
	if cap(*p) > maxRetainCap {
		// Drop the oversized backing array; the pool will allocate a
		// fresh small one on next Get via the New func above.
		return
	}
	// Clear element references so the pool does not keep the workspace
	// strings live past the request. Strings are interned by Go's
	// compiler for short literals but workspace paths are dynamically
	// constructed and would otherwise be GC-pinned via the pool.
	s := *p
	for i := range s {
		s[i] = ""
	}
	*p = s[:0]
	workspacesPool.Put(p)
}

// fillProjectAndSummary stamps each snapshot with its project name (from
// ProjectManager + planner-key fallback) and any persisted Summary lookup
// from sessions-index.json. Mutates snapshots in place.
//
// Splitting this out of handleList lets tests exercise project-name
// resolution against a stub ProjectManager without spinning up the full
// dashboard handler. R246-CR-002 (#736).
func (h *SessionHandlers) fillProjectAndSummary(snapshots []session.SessionSnapshot) {
	if h.projectMgr != nil {
		// Borrow a recycled []string scratch buffer to feed
		// ResolveWorkspaces. R217-PERF-10 (#616): the previous per-call
		// `make([]string, 0, len(snapshots))` showed up in heap profiles
		// on session-heavy dashboards (1 Hz × N tabs). ResolveWorkspaces
		// reads the header inside its own RLock and never retains the
		// backing array, so the pool entry is safe to recycle on return.
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
				// Planner keys are "project:{name}:planner". Extract the
				// middle segment with two IndexByte calls to avoid the
				// []string alloc from SplitN.
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
				// Fallback for unregistered workspaces: show the folder name
				// so sessions that are not bound to a ProjectManager project
				// still land in a meaningful sidebar group instead of "Other".
				// ProjectFallback signals the frontend to include the
				// workspace path in the group key so two different folders
				// with the same basename (e.g. /a/tmp and /b/tmp) do not
				// collapse into one group.
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

// buildSessionStats assembles the typed sessionStats payload that ships in
// the GET /api/sessions response. The named-struct copy avoids the
// map[string]any-style boxing the prior implementation paid on every 1 Hz
// poll. R70-PERF-H1 / R68-PERF-H3 / R59-PERF-001 / R51-PERF-005 /
// R49-PERF-STATS-STRUCT / R43-PERF-P43-1 / R54-PERF-001. Split out per
// R246-CR-002 (#736).
func (h *SessionHandlers) buildSessionStats(now time.Time, version uint64, running, ready int) sessionStats {
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
	if projectList := h.buildProjectList(now); len(projectList) > 0 {
		stats.Projects = projectList
	}
	return stats
}

// buildProjectList returns the dashboard sidebar's "Projects" panel data —
// local projects (cached at 1s buckets via projectListLocalAt) plus any
// remote-node projects forwarded through the node cache.
//
// Pre-allocate the outer slice so the append loop doesn't trigger log(N)
// growth reallocs on projects-heavy dashboards. Entries are projectListEntry
// named-struct values (not map[string]any) so the hot 1 Hz poll path skips
// the inner-map + interface{} boxing overhead. R70-PERF-M1.
//
// R247-PERF-15 [REPEAT-3]: collapse N dashboard tabs polling at 1 Hz into
// one rebuild/sec via projectListCache. The 1s bucket is invisible to
// human operators (project CRUD is minute-scale) and avoids touching the
// project package with a version hook. The cached slice is read-only —
// see projectListSnapshot godoc for the alias contract that keeps
// concurrent reads race-free. Split out per R246-CR-002 (#736).
func (h *SessionHandlers) buildProjectList(now time.Time) []projectListEntry {
	var projectList []projectListEntry
	if h.projectMgr != nil {
		projectList = h.projectListLocalAt(now)
	}
	// Merge remote projects (always, even without a local project manager).
	// When we will append remote rows onto the cached local slice we MUST
	// detach the cache first: projectListLocalAt returns the cached header
	// (alias contract), so an append that fits the existing capacity would
	// silently mutate every other reader's view. Building the merged slice
	// fresh keeps the cached entry untouched. R247-PERF-15.
	if !h.nodeAccess.HasNodes() {
		return projectList
	}
	cachedProjects := h.nodeCache.Projects()
	var remoteCount int
	for _, items := range cachedProjects {
		remoteCount += len(items)
	}
	if remoteCount > 0 {
		merged := make([]projectListEntry, len(projectList), len(projectList)+remoteCount)
		copy(merged, projectList)
		projectList = merged
	}
	for _, items := range cachedProjects {
		for _, item := range items {
			name := strOrFallback(item, "name", "Name")
			path := strOrFallback(item, "path", "Path")
			nd, _ := item["node"].(string)
			if name == "" {
				continue
			}
			entry := projectListEntry{Name: name, Path: path, Node: nd}
			if v, ok := item["favorite"].(bool); ok {
				entry.Favorite = v
			}
			// Remote node may be running an older binary that hasn't
			// redacted the URL yet — always run the redactor on data
			// forwarded via the node cache so credentials never leak
			// even if a peer node is behind on patches.
			if v, ok := item["git_remote_url"].(string); ok && v != "" {
				entry.GitRemoteURL = redactGitRemoteURL(v)
			}
			if v, ok := item["github"].(bool); ok {
				entry.GitHub = v
			}
			// JSON numbers decode as float64 from map[string]any. Pull
			// remote-node CreatedAt the same way; pre-feature peers won't
			// emit the key, so the zero-value fallback keeps their
			// projects at the very top of the sidebar (oldest by
			// definition) until they upgrade and self-stamp.
			if v, ok := item["created_at"].(float64); ok {
				entry.CreatedAt = int64(v)
			}
			projectList = append(projectList, entry)
		}
	}
	return projectList
}

// buildLocalResp constructs the single-node /api/sessions JSON shape.
//
// Use a named struct (sessionListLocalResp) instead of map[string]any
// so the 1 Hz dashboard poll skips the map-bucket alloc + interface{}
// boxing on every request. JSON output is byte-identical to the prior
// map literal because the field tags + omitempty preserve key order
// and the optional history_sessions semantics. R226-PERF-7. Split out
// per R246-CR-002 (#736).
func (h *SessionHandlers) buildLocalResp(snapshots []session.SessionSnapshot, stats sessionStats) sessionListLocalResp {
	resp := sessionListLocalResp{
		Sessions: snapshots,
		Stats:    stats,
	}
	if history := h.historySessions(); len(history) > 0 {
		resp.HistorySessions = history
	}
	return resp
}

// buildMultiNodeResp constructs the multi-node /api/sessions JSON shape.
// Local sessions are tagged with Node="local"; remote-node sessions and
// connection status are merged from the node cache + live nodesSnapshot.
//
// Use a named struct (sessionListMultiResp) instead of map[string]any
// so the multi-node hot path mirrors the single-node optimisation: no
// map-bucket alloc, no interface{} boxing of sessions/stats/nodes on
// every 1 Hz poll. JSON output stays byte-identical because the
// field tags preserve key names and history_sessions keeps omitempty.
// R226-PERF-7. Split out per R246-CR-002 (#736).
func (h *SessionHandlers) buildMultiNodeResp(snapshots []session.SessionSnapshot, stats sessionStats, knownNodes map[string]string) sessionListMultiResp {
	// Multi-node path: now we actually need the live nodesSnapshot for
	// connection status + fill-in. This acquires the nodeAccess lock.
	nodesSnapshot := h.nodeAccess.NodesSnapshot()

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
	// R172-SEC-L2: same validation the reverse-RPC fetch_events handler
	// enforces at the connector edge (internal/upstream/connector.go).
	// Without this gate an authenticated operator could post a multi-KB
	// key that lands in slog attrs on the "session not found" path or
	// embeds control bytes that corrupt log pipelines. ValidateSessionKey
	// also implicitly caps length at MaxSessionKeyBytes (~520 B).
	if err := session.ValidateSessionKey(key); err != nil {
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
		// EventEntriesBeforeCtx falls back to the backend's history.Source
		// (JSONL for claude) when the in-memory log no longer contains entries
		// older than `before`. The request context propagates into disk I/O
		// so a client-cancelled fetch unblocks the reverse JSONL scan on a
		// slow filesystem. Non-claude backends receive a noop Source and
		// behave exactly like the legacy memory-only path.
		entries = sess.EventEntriesBeforeCtx(r.Context(), before, pageLimit)
	default:
		entries = sess.EventEntries()
	}

	writeJSON(w, entries)
}

// DELETE /api/sessions accepts two input shapes for the session key:
//
//   - Query string:   DELETE /api/sessions?key=<k>&node=<n>   (REST-idiomatic)
//   - JSON body:      DELETE /api/sessions  {key, node}        (legacy)
//
// Query wins when `key` is present there — lets scripted users do
// `curl -X DELETE .../api/sessions?key=X` without crafting a body, which
// some HTTP clients (curl -G, fetch()) make awkward. The legacy JSON body
// path is preserved because the dashboard frontend and existing tests use
// it; a flag-day migration would gain nothing over this additive change.
// Both paths converge on the same validation + routing logic below.
func (h *SessionHandlers) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key  string `json:"key"`
		Node string `json:"node"`
	}
	if q := r.URL.Query(); q.Get("key") != "" {
		req.Key = q.Get("key")
		req.Node = q.Get("node")
		// Drain + close body (http.Server will close it for us, but
		// unreading it could confuse some middleware). MaxBytesReader
		// still applies to defend against trailer-bomb.
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		if err := decodeJSONBody(r, &req); err != nil || req.Key == "" {
			http.Error(w, "key is required (pass ?key=... or JSON body)", http.StatusBadRequest)
			return
		}
	}
	// R175-SEC-M: same gate handleEvents already runs (R172-SEC-L2). Without
	// it an authenticated operator could post a multi-KB key that reaches the
	// "remote remove session failed" slog.Warn attr (line below) or embeds
	// control bytes that corrupt log pipelines. ValidateSessionKey also caps
	// length at MaxSessionKeyBytes (~520 B).
	if err := session.ValidateSessionKey(req.Key); err != nil {
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
		writeOK(w)
		return
	}

	if !h.router.Remove(req.Key) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	writeOK(w)
}

// PATCH /api/sessions/label — update the operator-set display label for a
// session. Empty label clears any prior value.
func (h *SessionHandlers) handleSetLabel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key   string `json:"key"`
		Node  string `json:"node"`
		Label string `json:"label"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := decodeJSONBody(r, &req); err != nil || req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	// R175-SEC-M: gate req.Key before it reaches slog attrs (remote failure
	// path below logs both node + key) or router lookups. Same policy as
	// handleEvents / handleDelete.
	if err := session.ValidateSessionKey(req.Key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
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
			// R246-SEC-14 (#820): wrap node + key through SanitizeLogAttr
			// before slog encodes them. ValidateSessionKey already rejects
			// the bidi / C0 / C1 / zero-width classes that fragment slog
			// attrs, but `req.Node` only travels through nodeAccess.LookupNode
			// (which validates against the discovery directory, not the byte
			// class) — a future node-id format change could re-open the gap.
			// Aligning with the dispatch/commands.go:51 pattern keeps the
			// audit-log surface uniform, so a regression in either validator
			// cannot smuggle log-fragmentation bytes past slog's TextHandler.
			//
			// R246-SEC-14 (REPEAT-3, #820): the upstream node's err.Error()
			// can echo attacker-influenced bytes verbatim — a malicious
			// remote naozhi build could embed CR/LF or bidi runes in its
			// RPC error string and fragment our local slog audit trail.
			// Wrapping err.Error() through SanitizeLogAttr closes that
			// hole; the upstream wrapper text already includes "unknown
			// method:" / "rpc:" / etc. so legitimate diagnostic content
			// survives sanitisation (only control + bidi + C1 are
			// stripped).
			slog.Warn("remote set session label failed",
				"node", session.SanitizeLogAttr(req.Node),
				"key", session.SanitizeLogAttr(req.Key),
				"err", session.SanitizeLogAttr(err.Error()))
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
		// R246-SEC-14 (#820): defence-in-depth sanitiser on node + key,
		// matches the warn-path branch above.
		slog.Info("session label updated",
			"node", session.SanitizeLogAttr(req.Node),
			"key", session.SanitizeLogAttr(req.Key),
			"label_len", len(label))
		// Don't echo label — it is attacker-influenced text. Validation already
		// ensured it is safe in storage, but reflecting user input in an HTTP
		// body is a latent reflected-XSS vector if any future caller renders
		// the response via innerHTML. Client patches its cache from its own
		// optimistic value, not from the response.
		writeOK(w)
		return
	}

	if !h.router.SetUserLabel(req.Key, label) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// R246-SEC-14 (#820): SanitizeLogAttr on key matches the remote path so
	// the audit-log byte class is uniform regardless of which branch fired.
	slog.Info("session label updated", "node", "local",
		"key", session.SanitizeLogAttr(req.Key), "label_len", len(label))
	// Don't echo label — reflected-XSS precaution matches the remote-path
	// above. Client patches its cache from its own optimistic value.
	writeOK(w)
}

// POST /api/sessions/resume
func (h *SessionHandlers) handleResume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID  string `json:"session_id"`
		Workspace  string `json:"workspace"`
		LastPrompt string `json:"last_prompt"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := decodeJSONBody(r, &req); err != nil || req.SessionID == "" {
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
	// Invalid UTF-8 is still rejected — a bad encoding usually indicates a
	// buggy client and carries no safe sanitization.
	if !utf8.ValidString(req.LastPrompt) {
		http.Error(w, "last_prompt is not valid utf-8", http.StatusBadRequest)
		return
	}
	// Control / bidi / LS-PS bytes are sanitized instead of rejected. The
	// prior policy (R65-SEC-M-3) returned 400 to block slog-injection via
	// `/api/sessions` broadcasts. sanitizeResumeLastPrompt replaces the
	// dangerous class with "_" — the injection surface still closed,
	// and unlike a hard reject, sanitization lets sessions whose CLI
	// JSONL contains CLI-injected control bytes (e.g. PDF upload
	// notifications emitting U+0085 NEL) still resume from the history
	// pane. Tab is preserved (operators paste tab-delimited snippets
	// and slog JSONHandler escapes tab). last_prompt is display/log-only,
	// so lossy mapping on the rest of the class is acceptable.
	req.LastPrompt = sanitizeResumeLastPrompt(req.LastPrompt, maxResumeLastPromptBytes)

	workspace := req.Workspace
	if workspace != "" {
		wsPath, err := validateWorkspace(workspace, h.allowedRoot)
		if err != nil {
			// Decouple the client-facing message from the underlying error
			// chain so a future edit of validateWorkspace wrapping a
			// *os.PathError (e.g. with %w) cannot leak resolved filesystem
			// paths to the dashboard user. validateWorkspace already logs
			// diagnostic detail via slog. R61-SEC-10.
			// R179-SEC-1: sanitize the workspace before it lands in slog attrs
			// — authenticated callers can slip bidi/C1/newline bytes past the
			// structural path check. Mirrors the send.go (R175-SEC-P1) gate.
			slog.Warn("resume workspace validation failed", "err", err, "workspace", osutil.SanitizeForLog(workspace, 256))
			writeJSONStatus(w, http.StatusForbidden, map[string]string{"error": "invalid workspace"})
			return
		}
		workspace = wsPath
	}
	if workspace == "" {
		workspace = h.router.DefaultWorkspace()
	}

	// R247-SEC-24 / R246-SEC-5: resume key entropy widened from 8 → 16
	// bytes (64 → 128 bits) so the random tail matches anonCookie / upload
	// IDs and the rest of the codebase's 128-bit short-id budget. The
	// previous 64-bit tail had a birthday-bound (~2^32 IDs before
	// collision) that, while comfortably above realistic resume volume,
	// was inconsistent with sibling code and would have eventually been
	// flagged by another review round; align here to retire the audit
	// item permanently.
	var rb [16]byte
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
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := decodeJSONBody(r, &req); err != nil || req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	// R175-SEC-M: gate req.Key before it reaches slog attrs / router lookup.
	// Same policy as handleEvents / handleDelete / handleSetLabel.
	if err := session.ValidateSessionKey(req.Key); err != nil {
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
			if isUnknownRPCMethodErr(err) {
				http.Error(w, "remote node needs upgrade to support this action", http.StatusConflict)
				return
			}
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		if interrupted {
			slog.Info("remote session interrupted via HTTP", "node", req.Node, "key", req.Key)
			writeOK(w)
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
		writeOK(w)
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

// projectListSnapshot caches the local projectList slice build inside
// handleList at 1-second granularity. Bucket is unix-seconds at the time
// of build; a new bucket triggers a rebuild on the first miss.
//
// READ-ONLY CONTRACT: handleList reads Entries via the slice header only
// (no append, no element mutation) and copies the header into the response
// struct, which then JSON-encodes into the per-request buffer. Multiple
// concurrent readers therefore alias the same backing array — race-free
// because writers ALWAYS install a freshly built slice, never mutate in
// place. R247-PERF-15 [REPEAT-3].
type projectListSnapshot struct {
	Bucket  int64
	Entries []projectListEntry
}

// uptimeStringAt returns time.Since(startedAt).Round(time.Second).String()
// with a 1-second resolution memoisation. handleList captures time.Now()
// once at the top of the request so cutoff24h and the per-session uptime
// share a single vDSO call. Concurrent misses may all format the same
// value; last-writer-wins via unconditional Store is intentional — losers
// drop their locally formatted copy (the formatted string still escapes
// to the response regardless, so no leak). R67-PERF-4.
func (h *SessionHandlers) uptimeStringAt(now time.Time) string {
	d := now.Sub(h.startedAt).Round(time.Second)
	bucket := int64(d / time.Second)
	if cur := h.uptimeCache.Load(); cur != nil && cur.Bucket == bucket {
		return cur.Str
	}
	s := d.String()
	h.uptimeCache.Store(&uptimeSnapshot{Bucket: bucket, Str: s})
	return s
}

// projectListLocalAt returns the local projectListEntry slice with 1-second
// cache resolution. The returned slice is shared READ-ONLY across concurrent
// callers in the same bucket; any caller that intends to append must copy
// first (handleList does this in the remote-merge branch). h.projectMgr
// MUST be non-nil — callers gate on that check before invoking. R247-PERF-15
// [REPEAT-3].
//
// Cache races are benign: two pollers crossing a bucket boundary may each
// rebuild and Store; whichever writes last wins, the loser's locally
// computed slice is GC'd as soon as the response encodes. Critically, both
// rebuilds produce identical content (Manager.All takes a read lock and
// returns sorted snapshots) so observers cannot see torn data even if they
// hold an old header concurrent with the new Store.
func (h *SessionHandlers) projectListLocalAt(now time.Time) []projectListEntry {
	bucket := now.Unix()
	if cur := h.projectListCache.Load(); cur != nil && cur.Bucket == bucket {
		return cur.Entries
	}
	projects := h.projectMgr.All()
	entries := make([]projectListEntry, 0, len(projects))
	for _, p := range projects {
		entries = append(entries, projectListEntry{
			Name:      p.Name,
			Path:      p.Path,
			Node:      "local",
			Favorite:  p.Config.Favorite,
			CreatedAt: p.Config.CreatedAt,
			// Strip embedded userinfo (PAT) before handing the URL to any
			// dashboard client. Round 46 redacted /api/projects but missed
			// this path — /api/sessions is polled every few seconds, so
			// the leak is actually larger here.
			GitRemoteURL: redactGitRemoteURL(p.GitRemoteURL),
			GitHub:       p.IsGitHub,
		})
	}
	h.projectListCache.Store(&projectListSnapshot{Bucket: bucket, Entries: entries})
	return entries
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
	// Deep-copy systemInfo()'s singleton map: handleList copies the
	// sessionStatsStatic struct by value on each poll, but the System map
	// field is a reference type — without the deep copy here every poll
	// response would alias the singleton. A future maintainer adding a
	// mutable system field (e.g. network counters) would then silently
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

// InvalidateHistoryCache forces the next /api/sessions poll to repopulate
// historyCache from disk instead of serving the up-to-120s cached slice.
// Wired into Router.SetOnKeyRetired so a Reset/Remove that just retired a
// session-key surfaces the underlying jsonl in the history popover within
// one poll, instead of being hidden for up to two minutes by the TTL.
func (h *SessionHandlers) InvalidateHistoryCache() {
	h.historyCacheMu.Lock()
	h.historyCache = nil
	h.historyCacheTime = time.Time{}
	h.historyCacheMu.Unlock()
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

// RecordRetired stamps the retirement instant for sessionID into the
// retired-store and invalidates the history cache so the new ordering
// shows up on the next dashboard poll. No-op when the store is not
// configured (tests, deployments without StateDir). sessionID may be
// empty when the session was retired before the CLI returned a UUID;
// the store ignores empty IDs internally.
func (h *SessionHandlers) RecordRetired(sessionID string) {
	if h.retiredStore == nil || sessionID == "" {
		return
	}
	h.retiredStore.MarkRetired(sessionID, time.Now())
	h.InvalidateHistoryCache()
}

// FlushRetiredStore writes any pending retired-at marks to disk. Called
// from server shutdown so the final retirement event survives a restart.
// No-op when the store is not configured. Errors surface via slog inside
// the store's Save path; this method swallows them so shutdown does not
// fail the parent.
func (h *SessionHandlers) FlushRetiredStore() {
	if h.retiredStore == nil {
		return
	}
	if err := h.retiredStore.Save(); err != nil {
		slog.Warn("flush retired store failed", "err", err)
	}
}

func (h *SessionHandlers) loadHistorySessions() []discovery.RecentSession {
	excludeIDs := h.router.DiscoveryExcludeIDs()

	// R245-ARCH: hide cron-spawned and sys-session JSONLs from the
	// catch-all history panel.  Both have dedicated UI surfaces (cron
	// panel / system drawer) — leaking them here also exposed
	// AutoTitler prompt fragments because sys workdir lives under
	// ~/.claude/projects like a regular workspace.
	//
	// Snapshot Scheduler.KnownSessionIDs once per scan: it walks all
	// jobs × runStore.Recent which can be O(jobs × 200) and we don't
	// want to re-pay that cost per RecentSession candidate.
	filter := historyFilter{skipWorkspace: h.sysWorkDir}
	if h.cronSessions != nil {
		filter.skipSessions = h.cronSessions.KnownSessionIDs()
	}
	all := discovery.RecentSessions(h.claudeDir, 200, 7*24*time.Hour, excludeIDs, filter)

	// Resolve project names in batch.  R217-PERF-10 (#616): borrow the
	// pooled []string scratch (same pool fillProjectAndSummary uses) so
	// the history-scan path also stops paying a per-call workspaces alloc
	// every time the 120s history TTL expires.
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

	// Stamp retired_at from the in-memory store for sessions whose
	// retirement instant we recorded (Router.Reset / Router.Remove).
	// Snapshot once and look each entry up so the inner loop is O(N)
	// against an already-sorted slice instead of N retiredStore.Get
	// mutex acquires.
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

	h.historyCacheMu.Lock()
	h.historyCache = all
	h.historyCacheTime = time.Now()
	h.historyCacheMu.Unlock()

	return all
}
