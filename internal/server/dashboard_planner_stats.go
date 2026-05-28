// dashboard_planner_stats.go — GET /api/planner/stats process-resource probe.
//
// Issue #452 (PLANNER-STATS-1) tracks a richer "Planner process resource
// monitor: RSS / CPU% in dashboard + metrics" feature. This file is the
// part-1 placeholder so a concrete endpoint exists for the dashboard to
// poll while the full per-planner-CLI cgroup / proc sampler is designed.
//
// Scope today:
//   - process-wide stats from runtime.MemStats (Sys / HeapAlloc /
//     HeapInuse) + runtime.NumGoroutine.
//   - count of currently-attached planner sessions (router-level), so
//     operators can correlate memory growth with planner fan-out.
//   - the resolved planner-key list, so a future drawer can render
//     per-planner rows once we wire per-process RSS.
//
// Out of scope (future part-2+):
//   - per-planner-CLI RSS / CPU%. The CLI subprocess pid is not
//     surfaced through cli.Process today; sampling /proc/<pid>/statm
//     needs a router accessor and a unix build-tagged helper.
//   - historical sampling. The endpoint is intentionally pull-only —
//     the dashboard polls and renders a sparkline locally.
//   - prometheus / metrics export. /api/debug/vars already publishes
//     goroutines + memstats for ops; this endpoint keeps the dashboard
//     polling path off the loopback-only debug surface.
//
// Auth: same `auth(...)` middleware as the rest of /api/*. The data is
// process-wide aggregate — no per-session detail an attacker could use
// to fingerprint individual users beyond what /api/sessions already
// surfaces — so we do not gate on debug_mode.
package server

import (
	"net/http"
	"runtime"
	"sort"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// plannerStatsResponse is the JSON wire shape for GET /api/planner/stats.
//
// Field naming follows the rest of /api/* (snake_case lower; explicit
// `_bytes` / `_count` suffixes so dashboard code reading the values can
// pick a unit formatter without re-reading the doc). All counts are
// uint64 to match runtime.MemStats; goroutines is int because
// runtime.NumGoroutine returns int and a >2^31 goroutine count means
// the process is wedged anyway.
type plannerStatsResponse struct {
	// NaozhiRSSBytes is runtime.MemStats.Sys — the OS-allocated bytes
	// the Go runtime currently holds. Closer to RSS than HeapAlloc
	// because it includes stack + mspan + mcache + GC metadata, which
	// /proc/<pid>/statm sums into the resident set. Not byte-equal to
	// `ps -o rss` (Sys excludes the binary text segment + executable
	// pages) but the right shape for "is naozhi growing?" alerts.
	NaozhiRSSBytes uint64 `json:"naozhi_rss_bytes"`
	// NaozhiHeapAllocBytes is runtime.MemStats.HeapAlloc — bytes of
	// reachable heap objects. Useful for spotting leaks even when the
	// runtime has not yet released memory back to the OS (HeapInuse
	// stays high after a sweep until scavenger releases pages).
	NaozhiHeapAllocBytes uint64 `json:"naozhi_heap_alloc_bytes"`
	// NaozhiHeapInuseBytes is runtime.MemStats.HeapInuse — bytes in
	// in-use heap spans. HeapInuse - HeapAlloc approximates the
	// fragmentation cost the GC has not yet reclaimed.
	NaozhiHeapInuseBytes uint64 `json:"naozhi_heap_inuse_bytes"`
	// Goroutines is runtime.NumGoroutine — early signal for leaks in
	// dispatch / wsclient / readPump. Same metric expvar publishes via
	// /api/debug/vars, surfaced here so a non-debug dashboard panel
	// does not need the loopback-only escape hatch.
	Goroutines int `json:"goroutines"`
	// PlannerSessionsCount is the number of router sessions whose key
	// matches IsPlannerKey. Per-process RSS is not yet surfaced (see
	// part-2 plan in the file header) so this is the closest the
	// dashboard can get to "how many planners are currently warm" until
	// we plumb cli.Process.Pid().
	PlannerSessionsCount int `json:"planner_sessions_count"`
	// PlannerKeys is the sorted set of planner session keys currently
	// attached to the router. Stable ordering keeps the dashboard JS
	// diff-free across polls; sorting in Go (one slice, log-N rows)
	// is cheaper than relying on the JS side to dedup across pages.
	PlannerKeys []string `json:"planner_keys"`
}

// handlePlannerStats serves GET /api/planner/stats.
//
// The handler is a `Server` method (not on `ProjectHandlers`) because
// the data it returns is process-scoped, not project-scoped — same
// shape as handleSystemDaemons in dashboard_system.go.
func (s *Server) handlePlannerStats(w http.ResponseWriter, _ *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	resp := plannerStatsResponse{
		NaozhiRSSBytes:       ms.Sys,
		NaozhiHeapAllocBytes: ms.HeapAlloc,
		NaozhiHeapInuseBytes: ms.HeapInuse,
		Goroutines:           runtime.NumGoroutine(),
		PlannerKeys:          []string{}, // explicit empty so JSON emits []
	}

	if s.router != nil {
		// ListSessions is the existing read-only snapshot the dashboard
		// already polls for /api/sessions; reusing it keeps planner
		// stats consistent with what the sidebar shows. No additional
		// router lock acquired.
		for _, snap := range s.router.ListSessions() {
			if sessionkey.IsPlannerKey(snap.Key) {
				resp.PlannerKeys = append(resp.PlannerKeys, snap.Key)
			}
		}
		sort.Strings(resp.PlannerKeys)
		resp.PlannerSessionsCount = len(resp.PlannerKeys)
	}

	writeJSON(w, resp)
}
