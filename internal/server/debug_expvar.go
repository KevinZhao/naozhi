package server

import (
	"expvar"
	"log/slog"
	"net/http"
	"runtime"
	"sync"

	"github.com/naozhi/naozhi/internal/osutil"
)

// goroutinesPublishOnce guards expvar.Publish("goroutines") so multiple
// Server instances in one process (tests) do not trip the stdlib "Reuse of
// exported var name" panic; the gauge is process-scoped anyway.
var goroutinesPublishOnce sync.Once

// registerExpvar wires stdlib expvar's /debug/vars JSON endpoint onto the
// server mux at /api/debug/vars, gated identically to pprof: requireAuth AND
// loopback-only client check (cmdline + memstats + naozhi counters are
// fingerprinting material; trusted-proxy mode does NOT exempt expvar).
// Runbook: docs/ops/pprof.md.
func (s *Server) registerExpvar() {
	// NumGoroutine gauge: early leak signal; cheap enough to evaluate at scrape time.
	goroutinesPublishOnce.Do(func() {
		expvar.Publish("goroutines", expvar.Func(func() any { return runtime.NumGoroutine() }))
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// With no dashboard token the loopback gate (which trusts empty UDS
		// RemoteAddr) would be the only protection; refuse entirely.
		if s.dashboardToken == "" {
			slog.Warn("rejecting expvar request: no dashboard token configured",
				"path", osutil.SanitizeForLog(r.URL.Path, 256))
			http.Error(w, "expvar disabled: set a dashboard token to enable", http.StatusForbidden)
			return
		}
		// Gate on the real client IP: in trustedProxy mode RemoteAddr is the
		// proxy's loopback IP for every forwarded request.
		if !isLoopbackClient(r, s.auth.TrustedProxy) {
			// r.URL.Path is client-supplied; sanitize before it reaches slog attrs.
			slog.Warn("rejecting non-loopback expvar request",
				"remote", r.RemoteAddr, "path", osutil.SanitizeForLog(r.URL.Path, 256))
			http.Error(w, "expvar is loopback-only; SSH to the host and curl 127.0.0.1", http.StatusForbidden)
			return
		}
		expvar.Handler().ServeHTTP(w, r)
	})

	s.mux.HandleFunc("GET /api/debug/vars", s.auth.RequireAuth(handler))
}
