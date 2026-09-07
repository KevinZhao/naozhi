// routes.go — the /api/system surface this package owns (#2554).
//
// These patterns used to be written out in internal/server/routes.go, which put
// the URL space of every feature in one 478-line file owned by the server
// package. They now sit next to the handlers that serve them; the server only
// applies the middleware chain (internal/dashboard/httputil.Route).
package system

import "github.com/naozhi/naozhi/internal/dashboard/httputil"

// Routes returns the endpoints this package owns.
//
// Patterns must stay string literals: routes_snapshot_test.go reads them from
// the AST as the anti-drift gate.
func (h *Handlers) Routes() []httputil.Route {
	return []httputil.Route{
		// system-session daemons (docs/rfc/system-session.md §9.2/§9.3).
		{Pattern: "GET /api/system/daemons", Handler: h.HandleDaemons},
		{Pattern: "POST /api/system/labels/clear-origin", Handler: h.HandleClearLabelOrigin},
		// self-update (docs/rfc/dashboard-update-notice.md).
		{Pattern: "GET /api/system/update", Handler: h.HandleUpdateStatus},
		{Pattern: "POST /api/system/update/apply", Handler: h.HandleUpdateApply},
	}
}
