// route.go — how a dashboard sub-package declares the routes it owns (#2554).
//
// Route registration used to live entirely in internal/server/routes.go: 68
// /api/* patterns written out by hand, with the auth wrapper threaded into six
// register*Routes helpers as a bare func(http.HandlerFunc) http.HandlerFunc.
// Nothing in the type system said which package owned which path, so the
// boundary was policed from outside by the api_route_owner lint rule (a few
// hundred lines of AST tooling), with handle_decl guarding the neighbouring
// edge that *Server grows no handlers of its own.
//
// Now each sub-package returns its own []Route and the server applies the
// middleware chain. Two consequences worth stating, because they are the point:
//
//  1. A route cannot be mounted without the chain. The sub-package hands over
//     data, not a registration; the server is the only thing holding a mux. So
//     "someone forgot the auth wrapper on one route" stops being possible for
//     sub-package routes. Server-owned routes in routes.go still wrap by hand,
//     which is why TestAPIUnauthenticatedRejected drives every /api/ route in
//     the golden through the mux with no credentials and expects 401.
//  2. Path ownership becomes a compile-time fact — the patterns for /api/cron
//     live in internal/dashboard/cron. api_route_owner, which reconstructed
//     this by scanning ASTs, no longer had a question to answer and was
//     deleted (#2554). handle_decl was kept: whether *Server sprouts a new
//     handler is not answered by this type, only by that rule (#2636).
//
// Patterns stay STRING LITERALS inside each package's Routes() method on
// purpose: routes_snapshot_test.go reads them from the AST, and a computed
// pattern (fmt.Sprintf, a table built at init) would make the anti-drift gate
// blind. Add a route by adding a literal, not by generating one.
package httputil

import "net/http"

// Middleware wraps a handler. The server composes one chain and applies it to
// every Route, so a sub-package cannot mount a route with a different chain —
// or none.
type Middleware func(http.HandlerFunc) http.HandlerFunc

// Route is one HTTP route a dashboard sub-package owns. Every Route is
// authenticated: there is deliberately no opt-out field. Unauthenticated
// surfaces (login, noscript form target, favicon, dashboard shell) are
// server-owned and registered directly in internal/server/routes.go, where the
// routes golden + TestAPIUnauthenticatedRejected keep the list honest. A
// `Public bool` used to live here (#2619); it had zero users and was an
// unguarded escape hatch the golden did not record, so it was removed (#2631).
type Route struct {
	// Pattern is a Go 1.22 ServeMux pattern including the method, e.g.
	// "GET /api/cron/runs/{run_id}". Must be a string literal (see file header).
	Pattern string
	// Handler is the sub-package method serving it.
	Handler http.HandlerFunc
}
