// routes.go — the /api/sessions surface this package owns (#2554).
//
// These patterns used to be written out in internal/server/routes.go, which put
// the URL space of every feature in one 478-line file owned by the server
// package. They now sit next to the handlers that serve them; the server only
// applies the middleware chain (internal/dashboard/httputil.Route).
package session

import "github.com/naozhi/naozhi/internal/dashboard/httputil"

// Routes returns the endpoints this package owns.
//
// Patterns must stay string literals: routes_snapshot_test.go reads them from
// the AST as the anti-drift gate.
func (h *Handlers) Routes() []httputil.Route {
	return []httputil.Route{
		{Pattern: "GET /api/sessions", Handler: h.HandleList},
		{Pattern: "GET /api/sessions/events", Handler: h.HandleEvents},
		{Pattern: "GET /api/sessions/runs", Handler: h.HandleRuns},
		{Pattern: "GET /api/sessions/git", Handler: h.HandleGit},
		{Pattern: "DELETE /api/sessions", Handler: h.HandleDelete},
		{Pattern: "POST /api/sessions/resume", Handler: h.HandleResume},
		{Pattern: "POST /api/sessions/interrupt", Handler: h.HandleInterrupt},
		{Pattern: "PATCH /api/sessions/label", Handler: h.HandleSetLabel},
		// Per-session model/effort override (docs/rfc/dashboard-model-effort-control.md).
		{Pattern: "POST /api/sessions/override", Handler: h.HandleOverride},
	}
}
