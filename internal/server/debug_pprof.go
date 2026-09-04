package server

import (
	"log/slog"
	"net"
	"net/http"
	pprofhandler "net/http/pprof"
	"strconv"
	"strings"

	"github.com/naozhi/naozhi/internal/osutil"
)

// parsePositiveSeconds parses a `seconds=` pprof query parameter; returns 0
// for any malformed input so the caller rewrites it to the 30s default.
func parsePositiveSeconds(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// registerPprof wires Go's standard net/http/pprof handlers onto the
// server mux, gated by two independent defenses: requireAuth (goroutine
// stacks may contain secrets) AND a loopback-only client check (live
// profiling is a DoS lever even with a leaked token). `/api/debug/pprof/*`
// is mapped onto the stdlib `/debug/pprof/*` shape by stripping the prefix.
// Runbook: docs/ops/pprof.md.
func (s *Server) registerPprof() {
	if s.dashboardToken == "" {
		slog.Warn("pprof enabled without dashboard token: loopback callers can access profiles without authentication; set server.dashboard_token or disable debug_mode when not profiling")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// With no dashboard token requireAuth is a no-op and the loopback
		// gate (which trusts empty/"@" UDS RemoteAddr) would be the sole
		// protection; refuse pprof entirely in that mode.
		if s.dashboardToken == "" {
			slog.Warn("rejecting pprof request: no dashboard token configured",
				"path", osutil.SanitizeForLog(r.URL.Path, 256))
			http.Error(w, "pprof disabled: set a dashboard token to enable profiling", http.StatusForbidden)
			return
		}
		// Gate on the real client IP, not r.RemoteAddr: in trustedProxy mode
		// RemoteAddr is the proxy's loopback IP for every forwarded request.
		if !isLoopbackClient(r, s.auth.TrustedProxy) {
			slog.Warn("rejecting non-loopback pprof request",
				"remote", r.RemoteAddr, "path", osutil.SanitizeForLog(r.URL.Path, 256))
			http.Error(w, "pprof is loopback-only; SSH to the host and curl 127.0.0.1", http.StatusForbidden)
			return
		}

		// Shallow copy so the original request stays untouched for middleware.
		rr := *r
		newURL := *r.URL
		newURL.Path = strings.TrimPrefix(r.URL.Path, "/api")
		rr.URL = &newURL

		switch newURL.Path {
		case "/debug/pprof/cmdline":
			// Disabled: cmdline leaks --config path and flag-based secrets.
			http.Error(w, "cmdline pprof disabled; read /proc/<pid>/cmdline locally", http.StatusForbidden)
			return
		case "/debug/pprof/profile":
			pprofhandler.Profile(w, &rr)
		case "/debug/pprof/symbol":
			pprofhandler.Symbol(w, &rr)
		case "/debug/pprof/trace":
			// Bound trace duration at 30s (stdlib has no cap); rewrite when
			// missing, non-positive, or above the cap.
			if v := newURL.Query().Get("seconds"); v == "" {
				q := newURL.Query()
				q.Set("seconds", "30")
				newURL.RawQuery = q.Encode()
				rr.URL = &newURL
			} else if n := parsePositiveSeconds(v); n <= 0 || n > 30 {
				q := newURL.Query()
				q.Set("seconds", "30")
				newURL.RawQuery = q.Encode()
				rr.URL = &newURL
			}
			pprofhandler.Trace(w, &rr)
		default:
			// Index covers the listing and every named profile (heap, goroutine, ...).
			pprofhandler.Index(w, &rr)
		}
	})

	s.mux.HandleFunc("GET /api/debug/pprof/", s.auth.RequireAuth(handler))
	// Bare path too, so a forgotten slash gets a redirect rather than 404.
	s.mux.HandleFunc("GET /api/debug/pprof", s.auth.RequireAuth(handler))
}

// isLoopbackRemote reports whether a net/http Request.RemoteAddr is a
// loopback address (127/8 or ::1). Returns false on any parse error so the
// ambiguous case is treated as "remote". Empty / "@" RemoteAddr (Unix domain
// socket listener) counts as loopback: the kernel already enforced local-only
// access via socket-path permissions.
func isLoopbackRemote(remoteAddr string) bool {
	if remoteAddr == "" || remoteAddr == "@" {
		return true
	}
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
