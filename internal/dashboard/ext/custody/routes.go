package custody

import "github.com/naozhi/naozhi/internal/dashboard/httputil"

// Routes returns the endpoints this package owns.
//
// Patterns must stay string literals: routes_snapshot_test.go reads them from
// the AST as the anti-drift gate.
func (h *Handler) Routes() []httputil.Route {
	return []httputil.Route{
		{Pattern: "GET /api/custody", Handler: h.HandleList},
		{Pattern: "PUT /api/custody", Handler: h.HandleSet},
		{Pattern: "PUT /api/custody/pause", Handler: h.HandlePause},
		{Pattern: "DELETE /api/custody/pause", Handler: h.HandleResume},
	}
}
