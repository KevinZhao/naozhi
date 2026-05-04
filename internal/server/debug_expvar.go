package server

import (
	"expvar"
	"log/slog"
	"net/http"

	"github.com/naozhi/naozhi/internal/osutil"
)

// registerExpvar wires stdlib expvar's /debug/vars JSON endpoint onto the
// server mux, gated identically to pprof:
//
//  1. requireAuth (bearer token / signed cookie) — same middleware that
//     protects /api/*. Prevents anonymous operators / crawlers from reading
//     process-internal counters.
//  2. loopback-only remote address check. expvar output includes cmdline +
//     memstats — combined with naozhi's own counters (session counts, CLI
//     spawns, auth failures), this is plenty of fingerprinting material
//     for an attacker mapping an environment. A compromised ALB must not
//     be able to smuggle this out via forged X-Forwarded-For; trusted-proxy
//     mode does NOT exempt expvar from the loopback gate.
//
// Path is /api/debug/vars (prefix-matches the rest of /api/debug/*). The
// expvar handler is shaped for the stdlib's own `/debug/vars` registration
// pattern; both URLs point to the same http.Handler instance.
//
// See docs/ops/pprof.md for the equivalent runbook — expvar has no pprof
// CPU / heap cost but exposes the same sensitivity class.
func (s *Server) registerExpvar() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRemote(r.RemoteAddr) {
			// R186-SEC-L1: r.URL.Path is URL-decoded from the client-supplied
			// request line; it can carry bidi / C1 / LS/PS code points that
			// corrupt downstream terminal log viewers. Sanitize before it
			// reaches slog attrs. r.RemoteAddr is formatted by net/http from
			// a net.Listener-provided net.Addr and cannot be attacker-shaped.
			slog.Warn("rejecting non-loopback expvar request",
				"remote", r.RemoteAddr, "path", osutil.SanitizeForLog(r.URL.Path, 256))
			http.Error(w, "expvar is loopback-only; SSH to the host and curl 127.0.0.1", http.StatusForbidden)
			return
		}
		expvar.Handler().ServeHTTP(w, r)
	})

	s.mux.HandleFunc("GET /api/debug/vars", s.auth.requireAuth(handler))
}
