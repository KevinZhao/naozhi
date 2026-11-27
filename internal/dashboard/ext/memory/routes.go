// routes.go — the /api/memory surface this package owns (#2554).
//
// These patterns used to be written out in internal/server/routes.go, which put
// the URL space of every feature in one 478-line file owned by the server
// package. They now sit next to the handlers that serve them; the server only
// applies the middleware chain (internal/dashboard/httputil.Route).
package memory

import "github.com/naozhi/naozhi/internal/dashboard/httputil"

// Routes returns the endpoints this package owns. Serves
// ~/.claude/projects/<scope>/memory/<slug>.md for [[slug]] hover cards
// (docs/rfc/memory-link-rendering.md), and the settings view's 懂你 section
// (planner RFC §15.15 ⑦) under its own prefix, since any word after
// /api/memory/ is already a valid slug. Only the two GETs are reverse-node
// api routes (node.IsAPIRoute); PUT and DELETE also refuse any request that
// did not come straight from loopback (directLoopback).
//
// Patterns must stay string literals: routes_snapshot_test.go reads them from
// the AST as the anti-drift gate.
func (h *Handler) Routes() []httputil.Route {
	return []httputil.Route{
		{Pattern: "GET /api/memory/{slug}", Handler: h.HandleGet},
		{Pattern: "GET /api/memories", Handler: h.HandleList},
		{Pattern: "GET /api/memories/file", Handler: h.HandleFile},
		{Pattern: "PUT /api/memories/file", Handler: h.HandlePut},
		{Pattern: "DELETE /api/memories/file", Handler: h.HandleDelete},
	}
}
