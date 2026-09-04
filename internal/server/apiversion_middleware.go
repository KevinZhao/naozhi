package server

import (
	"net/http"
	"strings"
)

// apiV1Prefix is the canonical versioned API prefix, aliased onto the
// unversioned `/api/*` routes so external consumers have a stable contract (#425).
const apiV1Prefix = "/api/v1/"

// withAPIVersionAlias rewrites an inbound `/api/v1/<rest>` request path to the
// existing `/api/<rest>` route before the mux matches it.
//
// Only the literal `/api/v1/` prefix is rewritten; `/api/...` and unrelated
// paths pass through, and `/api/v1` without a trailing slash 404s normally.
// Only r.URL.Path is mutated — RawPath / RequestURI keep the version the
// client actually called for logging.
func withAPIVersionAlias(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, apiV1Prefix) {
			// "/api/v1/sessions" -> "/api/sessions".
			rest := strings.TrimPrefix(r.URL.Path, apiV1Prefix)
			r.URL.Path = "/api/" + rest
		}
		next.ServeHTTP(w, r)
	})
}
