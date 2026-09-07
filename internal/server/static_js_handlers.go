package server

// static_js_handlers.go — the one handler behind every /static/*.js route
// (#2554).
//
// There used to be 24 of these, one per dashboard module: 17 here and 7 in
// routes.go, each a 12-line copy differing only in the filename. They were
// added one at a time as #2558 split dashboard.js into modules, and the count
// grew with the module count — the file was split off static_assets.go purely to
// keep that one under the 500-line limit. Verified byte-identical modulo the
// asset name before collapsing them (24 bodies, 1 shape).
//
// serveStaticJS is that shape, taken once. Adding a module is now a route line,
// not a route line plus a handler.
//
// Route patterns stay written out in routes.go rather than looping over a name
// table: routes_snapshot_test.go reads each pattern as a string literal, and a
// computed "GET /static/"+name would trade the anti-drift gate for a shorter
// file.

import "net/http"

// serveStaticJS returns the handler for one embedded JS module.
//
// 404 when the asset is missing (a build that dropped an embed rather than a
// silent empty body), JS content-type + nosniff, must-revalidate so a module
// edit is picked up without a hard reload, then the ETag short-circuit before
// the body.
func serveStaticJS(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if staticAssetBytes(name) == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		if serveStaticWithETag(w, r, name) {
			return
		}
		writeStaticAssetBody(w, r, name)
	}
}

// handleDashboardCSS serves static/css/<file> from the {file} path param. Not
// serveStaticJS: the asset name comes from the request, so it needs the table
// lookup to reject anything not embedded (a path-traversal attempt resolves to
// a miss rather than a read).
func handleDashboardCSS(w http.ResponseWriter, r *http.Request) {
	name := "css/" + r.PathValue("file")
	if staticAssetBytes(name) == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	if serveStaticWithETag(w, r, name) {
		return
	}
	writeStaticAssetBody(w, r, name)
}
