// Package planner hosts the dashboard /api/planner/* endpoints.
//
//	GET /api/planner/stats  process-resource probe: runtime.MemStats +
//	                        goroutine count + attached planner keys
//
// Pull-only; the dashboard polls it so the non-debug panel stays off the
// loopback-only debug surface. Same auth middleware as the rest of /api/*;
// process-wide aggregate only, so not gated on debug_mode.
package planner

import (
	"net/http"
	"runtime"
	"sort"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Router is the consumer-side subset of *session.Router the stats probe
// reads, so the sub-package never imports internal/server.
type Router interface {
	ListSessions() []session.SessionSnapshot
}

// Deps carries what the handlers read; the server wires it once at build.
type Deps struct {
	Router Router
}

// Handlers serves the /api/planner/* endpoint family.
type Handlers struct {
	router Router
}

// New returns Handlers wired from d.
func New(d Deps) *Handlers {
	return &Handlers{router: d.Router}
}

// statsResponse is the JSON wire shape for GET /api/planner/stats.
type statsResponse struct {
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

// HandleStats serves GET /api/planner/stats.
func (h *Handlers) HandleStats(w http.ResponseWriter, _ *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	resp := statsResponse{
		NaozhiRSSBytes:       ms.Sys,
		NaozhiHeapAllocBytes: ms.HeapAlloc,
		NaozhiHeapInuseBytes: ms.HeapInuse,
		Goroutines:           runtime.NumGoroutine(),
		PlannerKeys:          []string{}, // explicit empty so JSON emits []
	}

	if h.router != nil {
		// Same snapshot /api/sessions uses, so counts match the sidebar.
		for _, snap := range h.router.ListSessions() {
			if sessionkey.IsPlannerKey(snap.Key) {
				resp.PlannerKeys = append(resp.PlannerKeys, snap.Key)
			}
		}
		sort.Strings(resp.PlannerKeys)
		resp.PlannerSessionsCount = len(resp.PlannerKeys)
	}

	httputil.WriteJSON(w, resp)
}
