package server

import (
	"log/slog"
	"net"
	"net/http"
	pprofhandler "net/http/pprof"
	"strings"
)

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
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Defense-in-depth: even inside requireAuth, reject non-loopback
		// callers. trustedProxy mode (ALB → EC2) does NOT exempt pprof
		// from the loopback gate — a compromised ALB could otherwise
		// smuggle profiles out via forged X-Forwarded-For.
		if !isLoopbackRemote(r.RemoteAddr) {
			slog.Warn("rejecting non-loopback pprof request",
				"remote", r.RemoteAddr, "path", r.URL.Path)
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
			pprofhandler.Cmdline(w, &rr)
		case "/debug/pprof/profile":
			pprofhandler.Profile(w, &rr)
		case "/debug/pprof/symbol":
			pprofhandler.Symbol(w, &rr)
		case "/debug/pprof/trace":
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
	s.mux.HandleFunc("GET /api/debug/pprof/", s.auth.requireAuth(handler))
	// Also cover the bare /api/debug/pprof without trailing slash so
	// operators who forget the slash get a redirect rather than 404.
	s.mux.HandleFunc("GET /api/debug/pprof", s.auth.requireAuth(handler))
}

// isLoopbackRemote reports whether a net/http Request.RemoteAddr is a
// loopback address (127/8 or ::1). Uses net.SplitHostPort to tolerate
// the "host:port" shape that Go's server always sets, falling back to
// the raw string for non-standard middlewares that might strip the
// port. Returns false on any parse error so the caller treats the
// ambiguous case as "remote".
func isLoopbackRemote(remoteAddr string) bool {
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
