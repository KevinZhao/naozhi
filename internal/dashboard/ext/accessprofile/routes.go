// routes.go — the /api/access-profiles surface this package owns (#2554).
//
// These patterns used to be written out in internal/server/routes.go, which put
// the URL space of every feature in one 478-line file owned by the server
// package. They now sit next to the handlers that serve them; the server only
// applies the middleware chain (internal/dashboard/httputil.Route).
package accessprofile

import "github.com/naozhi/naozhi/internal/dashboard/httputil"

// Routes returns the endpoints this package owns. List returns only
// non-sensitive fields (never env values or tokens); create is disabled (400)
// when ConfigOptions.Path is unset.
//
// Patterns must stay string literals: routes_snapshot_test.go reads them from
// the AST as the anti-drift gate.
func (h *Handler) Routes() []httputil.Route {
	return []httputil.Route{
		{Pattern: "GET /api/access-profiles", Handler: h.HandleList},
		{Pattern: "POST /api/access-profiles", Handler: h.HandleCreate},
	}
}
