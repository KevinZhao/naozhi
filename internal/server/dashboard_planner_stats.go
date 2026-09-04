// dashboard_planner_stats.go — GET /api/planner/stats process-resource probe:
// process-wide runtime.MemStats + goroutine count + attached planner keys.
// Per-planner-CLI RSS/CPU is future work (#452). Pull-only; the dashboard
// polls it so the non-debug panel stays off the loopback-only debug surface.
// Auth: same middleware as the rest of /api/*; process-wide aggregate only,
// so not gated on debug_mode.
package server

import (
	"net/http"
	"runtime"
	"sort"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// plannerStatsResponse is the JSON wire shape for GET /api/planner/stats.
type plannerStatsResponse struct {
	// NaozhiRSSBytes is runtime.MemStats.Sys — closer to RSS than HeapAlloc
	// (includes stacks + GC metadata), though not byte-equal to `ps -o rss`.
	NaozhiRSSBytes uint64 `json:"naozhi_rss_bytes"`
	// NaozhiHeapAllocBytes is runtime.MemStats.HeapAlloc (reachable heap objects).
	NaozhiHeapAllocBytes uint64 `json:"naozhi_heap_alloc_bytes"`
	// NaozhiHeapInuseBytes is runtime.MemStats.HeapInuse; minus HeapAlloc
	// approximates unreclaimed fragmentation.
	NaozhiHeapInuseBytes uint64 `json:"naozhi_heap_inuse_bytes"`
	// Goroutines is runtime.NumGoroutine — early leak signal.
	Goroutines int `json:"goroutines"`
	// PlannerSessionsCount is the number of router sessions matching IsPlannerKey.
	PlannerSessionsCount int `json:"planner_sessions_count"`
	// PlannerKeys is the sorted set of attached planner session keys; stable
	// ordering keeps the dashboard JS diff-free across polls.
	PlannerKeys []string `json:"planner_keys"`
}

// handlePlannerStats serves GET /api/planner/stats. A Server method because
// the data is process-scoped, like handleSystemDaemons.
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
		// Same snapshot /api/sessions uses, so counts match the sidebar.
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
