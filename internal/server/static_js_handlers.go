package server

// static_js_handlers.go — the one handler behind every /static/ route: the
// dashboard's JS modules, its stylesheets, and the vendored libraries.

import (
	"net/http"
	"path"
	"strings"
)

// staticTypes maps the file types handleStaticAsset serves, scripts,
// stylesheets and the fonts they load, to their Content-Type.
var staticTypes = map[string]string{
	".js":    "application/javascript",
	".css":   "text/css; charset=utf-8",
	".woff2": "font/woff2",
}

// handleStaticAsset serves static/<file>, keyed by the request path below
// /static/. The asset name comes from the request, so the table lookup rejects
// anything not embedded (a path-traversal attempt resolves to a miss rather
// than a read). sw.js is served only at /sw.js, where its scope is "/".
//
// 404 when the asset is missing, its content-type + nosniff, the cache policy
// its ?v= earns (staticCacheControl), then the ETag short-circuit before the
// body.
func handleStaticAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	typ := staticTypes[path.Ext(name)]
	if typ == "" || name == "sw.js" || !hasStaticAsset(name) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", staticCacheControl(r, name))
	if serveStaticWithETag(w, r, name) {
		return
	}
	writeStaticAssetBody(w, r, name)
}
