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
// for any malformed input (which then fails the `> 30` check below and gets
// rewritten to 30, the safe default).
func parsePositiveSeconds(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// registerPprof wires Go's standard net/http/pprof handlers onto the
// server mux, gated by two independent defenses:
//
//  1. requireAuth (bearer token / signed cookie — same middleware the
//     rest of /api/* uses). Without this, a misconfigured proxy could
//     expose raw profile data (including secrets in goroutine stacks).
//  2. loopback-only remote address check. Even if the dashboard token
//     leaks, remote profiling is rejected because live profiling can
//     be used as a DoS lever (CPU profile with short sampling interval
//     is expensive) and heap snapshots may leak operational context.
//
// Profiles are reached via `/api/debug/pprof/*` mapped to the stdlib
// handlers that expect `/debug/pprof/*`. The `/api` prefix is stripped
// before delegating so pprof's internal URL parsing keeps working.
//
// Operators run profiles via SSH + loopback:
//
//	ssh host curl -s -H "Authorization: Bearer $TOK" \
//	    http://127.0.0.1:8180/api/debug/pprof/heap > heap.pprof
//	go tool pprof heap.pprof
//
// See docs/ops/pprof.md for the full runbook.
func (s *Server) registerPprof() {
	// R20260603-SEC-3 (#1633): warn when pprof is enabled without a dashboard
	// token. In this mode requireAuth is a pass-through, so any process on the
	// local host can reach pprof over loopback without authentication. The
	// isLoopbackRemote gate still blocks non-loopback callers unconditionally,
	// so the exposure is limited to local processes — but goroutine dumps may
	// contain tokens and conversation context.
	// Operators should set a dashboard_token or only enable debug_mode transiently.
	if s.dashboardToken == "" {
		slog.Warn("pprof enabled without dashboard token: loopback callers can access profiles without authentication; set server.dashboard_token or disable debug_mode when not profiling")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Defense-in-depth (R20260602-SEC-10): when no dashboard token is
		// configured, requireAuth is a no-op and the loopback gate is the
		// SOLE protection. For UDS deployments isLoopbackRemote returns
		// true for empty/"@" RemoteAddr, so a future reverse-proxy-over-UDS
		// misconfig that strips RemoteAddr would expose pprof unauthenticated
		// to any same-host process. Refuse pprof entirely when there is no
		// token to enforce — operators must set DashboardToken to profile.
		if s.dashboardToken == "" {
			slog.Warn("rejecting pprof request: no dashboard token configured",
				"path", osutil.SanitizeForLog(r.URL.Path, 256))
			http.Error(w, "pprof disabled: set a dashboard token to enable profiling", http.StatusForbidden)
			return
		}
		// Defense-in-depth: even inside requireAuth, reject non-loopback
		// callers. trustedProxy mode (ALB → EC2) does NOT exempt pprof
		// from the loopback gate — a compromised ALB could otherwise
		// smuggle profiles out via forged X-Forwarded-For.
		if !isLoopbackRemote(r.RemoteAddr) {
			slog.Warn("rejecting non-loopback pprof request",
				"remote", r.RemoteAddr, "path", osutil.SanitizeForLog(r.URL.Path, 256))
			http.Error(w, "pprof is loopback-only; SSH to the host and curl 127.0.0.1", http.StatusForbidden)
			return
		}

		// Strip the /api prefix so the stdlib pprof Index handler sees
		// the /debug/pprof/... path shape it expects for split-and-
		// dispatch. Operate on a shallow copy so the original request
		// is untouched for any upstream logging/middleware state.
		rr := *r
		newURL := *r.URL
		newURL.Path = strings.TrimPrefix(r.URL.Path, "/api")
		rr.URL = &newURL

		// Dispatch on the stripped path to the specific profile handler.
		// pprof.Index handles /debug/pprof/ (listing) AND named profiles
		// (e.g. /debug/pprof/heap) via its built-in Handler(name).
		// Cmdline / Profile / Symbol / Trace have dedicated handlers.
		switch newURL.Path {
		case "/debug/pprof/cmdline":
			// Disabled: cmdline leaks --config path and any flag-based
			// secrets embedded in argv. Operators who need it can SSH
			// to the host and read /proc/self/cmdline directly. Every
			// other pprof profile is fine to expose over loopback.
			http.Error(w, "cmdline pprof disabled; read /proc/<pid>/cmdline locally", http.StatusForbidden)
			return
		case "/debug/pprof/profile":
			pprofhandler.Profile(w, &rr)
		case "/debug/pprof/symbol":
			pprofhandler.Symbol(w, &rr)
		case "/debug/pprof/trace":
			// Cap trace duration server-side: pprofhandler.Trace honours the
			// `seconds` query parameter (default 1s, no cap) so a buggy or
			// malicious loopback caller can request an indefinite trace.
			// Stdlib limits to 1s default but we further bound at 30s.
			// Rewrite when missing, non-positive, or above the 30s cap.
			// Without the `<= 0` arm, `seconds=0` and `seconds=-1`
			// would slip through unrewritten and the bound advertised
			// by the comment above wouldn't actually be enforced.
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
			// Index covers the listing at /debug/pprof/ and also every
			// dynamically-registered profile (heap, goroutine, allocs,
			// block, mutex, threadcreate, any custom ones added later).
			pprofhandler.Index(w, &rr)
		}
	})

	// Mux pattern with trailing slash registers a subtree — every path
	// beginning with /api/debug/pprof/ resolves here. The auth
	// middleware enforces bearer/cookie + same-origin guard.
	s.mux.HandleFunc("GET /api/debug/pprof/", s.auth.RequireAuth(handler))
	// Also cover the bare /api/debug/pprof without trailing slash so
	// operators who forget the slash get a redirect rather than 404.
	s.mux.HandleFunc("GET /api/debug/pprof", s.auth.RequireAuth(handler))
}

// isLoopbackRemote reports whether a net/http Request.RemoteAddr is a
// loopback address (127/8 or ::1). Uses net.SplitHostPort to tolerate
// the "host:port" shape that Go's server always sets, falling back to
// the raw string for non-standard middlewares that might strip the
// port. Returns false on any parse error so the caller treats the
// ambiguous case as "remote".
//
// Unix domain socket deployments are treated as loopback: net/http on
// a UDS listener leaves RemoteAddr empty (or "@" on Linux abstract
// sockets), and the kernel has already enforced local-only access via
// filesystem permissions on the socket path. Without this gate
// pprof/expvar would 403 on every UDS request. R232-SEC-15.
func isLoopbackRemote(remoteAddr string) bool {
	if remoteAddr == "" || remoteAddr == "@" {
		return true
	}
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	// Trim IPv6 brackets left over if SplitHostPort didn't apply.
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
