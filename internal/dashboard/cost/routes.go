// routes.go — the /api/cost surface this package owns (#2554).
//
// These patterns used to be written out in internal/server/routes.go, which put
// the URL space of every feature in one 478-line file owned by the server
// package. They now sit next to the handlers that serve them; the server only
// applies the middleware chain (internal/dashboard/httputil.Route).
package cost

import "github.com/naozhi/naozhi/internal/dashboard/httputil"

// Routes returns the endpoints this package owns. Cost ledger read API
// (docs/rfc/cost-ledger.md §7): unit-bucketed and rate limited.
//
// Patterns must stay string literals: routes_snapshot_test.go reads them from
// the AST as the anti-drift gate.
func (h *Handlers) Routes() []httputil.Route {
	return []httputil.Route{
		{Pattern: "GET /api/cost/summary", Handler: h.HandleSummary},
		{Pattern: "GET /api/cost/entries", Handler: h.HandleEntries},
	}
}
