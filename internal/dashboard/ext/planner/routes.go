// routes.go — the /api/planner surface this package owns (#2554).
package planner

import "github.com/naozhi/naozhi/internal/dashboard/httputil"

// Routes returns the endpoints this package owns: the global Planner's
// status, its runtime stop switch, its message entry, the morning briefs,
// the gateway's notes in its conversation and a scout run on demand.
//
// Patterns must stay string literals: routes_snapshot_test.go reads them from
// the AST as the anti-drift gate.
func (h *Handler) Routes() []httputil.Route {
	return []httputil.Route{
		{Pattern: "GET /api/planner", Handler: h.HandleStatus},
		{Pattern: "POST /api/planner/state", Handler: h.HandleState},
		{Pattern: "POST /api/planner/send", Handler: h.HandleSend},
		{Pattern: "GET /api/planner/briefs", Handler: h.HandleBriefs},
		{Pattern: "GET /api/planner/notes", Handler: h.HandleNotes},
		{Pattern: "POST /api/scout/run", Handler: h.HandleScoutRun},
	}
}
