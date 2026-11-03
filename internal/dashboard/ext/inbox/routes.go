package inbox

import "github.com/naozhi/naozhi/internal/dashboard/httputil"

// Routes returns the endpoints this package owns.
//
// Patterns must stay string literals: routes_snapshot_test.go reads them from
// the AST as the anti-drift gate.
func (h *Handler) Routes() []httputil.Route {
	return []httputil.Route{
		{Pattern: "GET /api/inbox", Handler: h.HandleList},
		{Pattern: "POST /api/inbox/close", Handler: h.HandleClose},
		{Pattern: "POST /api/inbox/decide", Handler: h.HandleDecide},
		{Pattern: "PUT /api/inbox/pause", Handler: h.HandlePause},
		{Pattern: "DELETE /api/inbox/pause", Handler: h.HandleResume},
	}
}
