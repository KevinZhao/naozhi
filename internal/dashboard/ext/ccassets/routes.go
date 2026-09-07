// routes.go — the /api/cc/assets surface this package owns (#2554).
//
// These patterns used to be written out in internal/server/routes.go, which put
// the URL space of every feature in one 478-line file owned by the server
// package. They now sit next to the handlers that serve them; the server only
// applies the middleware chain (internal/dashboard/httputil.Route).
package ccassets

import "github.com/naozhi/naozhi/internal/dashboard/httputil"

// Routes returns the endpoints this package owns. The read-only installed-asset
// browser (docs/rfc/cc-asset-browser.md).
//
// Patterns must stay string literals: routes_snapshot_test.go reads them from
// the AST as the anti-drift gate.
func (h *Handler) Routes() []httputil.Route {
	return []httputil.Route{
		{Pattern: "GET /api/cc/assets", Handler: h.HandleList},
		{Pattern: "GET /api/cc/assets/raw", Handler: h.HandleRaw},
	}
}
