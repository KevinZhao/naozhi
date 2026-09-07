// routes.go — the /api/projects surface this package owns (#2554).
//
// These patterns used to be written out in internal/server/routes.go, which put
// the URL space of every feature in one 478-line file owned by the server
// package. They now sit next to the handlers that serve them; the server only
// applies the middleware chain (internal/dashboard/httputil.Route).
package project

import "github.com/naozhi/naozhi/internal/dashboard/httputil"

// Routes returns the endpoints this package owns.
//
// Patterns must stay string literals: routes_snapshot_test.go reads them from
// the AST as the anti-drift gate.
func (h *Handlers) Routes() []httputil.Route {
	return []httputil.Route{
		{Pattern: "GET /api/projects", Handler: h.HandleList},
		{Pattern: "GET /api/projects/config", Handler: h.HandleConfigGet},
		{Pattern: "PUT /api/projects/config", Handler: h.HandleConfigPut},
		{Pattern: "POST /api/projects/planner/restart", Handler: h.HandlePlannerRestart},
		{Pattern: "POST /api/projects/favorite", Handler: h.HandleFavoriteToggle},
		{Pattern: "POST /api/projects/files/exists", Handler: h.HandleFilesExists},
		{Pattern: "GET /api/projects/file", Handler: h.HandleFileGet},
		// Workspace file browser: listing reuses HandleFileGet's path-safety;
		// upload is the only write in the file API.
		{Pattern: "GET /api/projects/files/list", Handler: h.HandleFilesList},
		{Pattern: "POST /api/projects/files/upload", Handler: h.HandleFilesUpload},
	}
}
