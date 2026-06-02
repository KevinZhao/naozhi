package server

import (
	"net/http"
	"strings"
)

// apiV1Prefix is the canonical versioned API prefix. RNEW-ARCH-401 (#425):
// the 20+ dashboard/node endpoints historically mounted only at unversioned
// `/api/*`, so any field rename was a silent break for external IM / node
// consumers that had no stable contract to pin against. This is the first
// concrete migration slice from the issue's proposal — "Mount routes at
// /api/v1/ aliasing existing /api/" — implemented as a thin request-path
// rewrite so the 20+ registrations and the *_shape_test.go / contract regex
// gates stay byte-for-byte untouched.
const apiV1Prefix = "/api/v1/"

// withAPIVersionAlias rewrites an inbound `/api/v1/<rest>` request path to the
// existing `/api/<rest>` route before the mux matches it. Consumers can now
// pin the explicit `/api/v1/` prefix and get the identical handler, giving us
// a stable seam to evolve `/api/v2/` against later without re-touching every
// HandleFunc call site.
//
// Scope of the rewrite is deliberately narrow:
//
//   - Only the literal `/api/v1/` prefix is rewritten. `/api/...` keeps
//     working unchanged (backwards compatibility for current dashboard JS and
//     already-deployed nodes), and unrelated paths (`/ws`, `/dashboard`,
//     `/static/...`, `/health`) are passed through verbatim.
//   - `/api/v1` with no trailing slash is left alone — it is not a registered
//     route, so the mux returns its normal 404 rather than us synthesising a
//     surprising redirect.
//   - The rewrite mutates only r.URL.Path (what the ServeMux matches on).
//     r.URL.RawPath / RequestURI are left intact so logging + trace middleware
//     still record the version the client actually called.
//
// Wired as an inner middleware (below withTraceID/gzip) in server.go so the
// alias is transparent to every downstream handler.
func withAPIVersionAlias(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, apiV1Prefix) {
			// Build the unversioned path: "/api/v1/sessions" -> "/api/sessions".
			// TrimPrefix removes "/api/v1/" leaving "sessions"; re-prepend
			// "/api/" so the existing mux entry matches.
			rest := strings.TrimPrefix(r.URL.Path, apiV1Prefix)
			r.URL.Path = "/api/" + rest
		}
		next.ServeHTTP(w, r)
	})
}
