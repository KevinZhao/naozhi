package server

import (
	"net"
	"net/http"

	"github.com/naozhi/naozhi/internal/netutil"
)

// clientIP is a thin wrapper around netutil.ClientIP kept so existing
// server-package call sites can keep their short name. See the netutil
// docs for semantics (trusted-proxy XFF last-hop, IP validation).
func clientIP(r *http.Request, trustedProxy bool) string {
	return netutil.ClientIP(r, trustedProxy)
}

// isLoopbackClient reports whether the request's *real* client is on the
// loopback interface, for the pprof/expvar loopback gate (R20260604-SEC-5).
//
// The naive isLoopbackRemote(r.RemoteAddr) check is correct only for direct
// listeners. When trustedProxy is true the server sits behind a reverse proxy
// (ALB/CloudFront → loopback hop), so r.RemoteAddr is ALWAYS the proxy's IP —
// typically 127.0.0.1 — and isLoopbackRemote would wave through every
// externally-originated request the proxy forwards, defeating the gate.
//
// In trustedProxy mode we instead resolve the real client IP via
// netutil.ClientIP, which reads the last X-Forwarded-For hop appended by the
// trusted proxy (not client-spoofable) and validates it. An external client's
// IP is non-loopback → rejected. A request with no usable XFF collapses to
// r.RemoteAddr; in a proxy-fronted deployment that only happens for a request
// that did NOT traverse the proxy — i.e. a direct on-host loopback connection
// (the SSH-and-curl-127.0.0.1 runbook), which is exactly what the gate means
// to allow.
//
// UDS deployments keep isLoopbackRemote's empty/"@" handling: ClientIP returns
// the empty RemoteAddr and ipStringIsLoopback treats it as loopback.
func isLoopbackClient(r *http.Request, trustedProxy bool) bool {
	if !trustedProxy {
		return isLoopbackRemote(r.RemoteAddr)
	}
	return ipStringIsLoopback(netutil.ClientIP(r, true))
}

// ipStringIsLoopback parses a bare IP (no port) and reports whether it is a
// loopback address. Empty / "@" (UDS RemoteAddr) is treated as loopback to
// match isLoopbackRemote; an unparseable string is treated as non-loopback so
// the ambiguous case fails closed.
func ipStringIsLoopback(host string) bool {
	if host == "" || host == "@" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
