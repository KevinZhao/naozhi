// Package netutil holds small network-address helpers shared across packages
// that cannot depend on each other (e.g. internal/server and internal/node).
package netutil

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP extracts the real client IP from r.
//
// When trustedProxy is true (deployed behind ALB/CloudFront) it reads the
// last entry of X-Forwarded-For — the one appended by the trusted proxy,
// which cannot be spoofed by the client. Otherwise it falls back to
// r.RemoteAddr.
//
// The last XFF entry is validated via net.ParseIP so a malformed header
// (stray whitespace, garbage token) cannot produce a bogus rate-limit key
// that would bypass per-IP limits.
//
// The XFF scan avoids strings.Split to save one []string allocation per
// request: strings.LastIndexByte + a single TrimSpace on the tail slice
// is zero-alloc on the hot path.
//
// R247-SEC-25 nuance: when trustedProxy is true but the request arrives
// without a usable XFF (header missing or unparseable last hop), this
// function falls back to r.RemoteAddr. In a strict ALB-fronted deployment
// every legitimate request is XFF-stamped, so an XFF-less request collapses
// every "real" client into the single proxy-IP bucket and degrades per-IP
// rate-limit fairness.
func ClientIP(r *http.Request, trustedProxy bool) string {
	ip, _ := clientIPInternal(r, trustedProxy)
	return ip
}

func clientIPInternal(r *http.Request, trustedProxy bool) (string, bool) {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if !trustedProxy {
		return ip, true
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		tail := xff
		if i := strings.LastIndexByte(xff, ','); i >= 0 {
			tail = xff[i+1:]
		}
		tail = strings.TrimSpace(tail)
		if tail != "" && net.ParseIP(tail) != nil {
			return tail, true
		}
	}
	// R20260605: no usable XFF. In trustedProxy mode that normally means the
	// request did not traverse the proxy and is treated as unresolvable. But
	// a loopback RemoteAddr (127.0.0.1/::1) cannot be externally routed — the
	// kernel guarantees it originated on-host — so it is the legit
	// direct-access path (SSH tunnel / local curl / port-forward), the same
	// case isLoopbackClient deliberately allows. Resolve it to its loopback IP
	// so it gets a real per-IP rate-limit key instead of a hard fail-closed
	// reject. An externally-routable RemoteAddr without usable XFF stays
	// unresolvable, preserving R244-SEC-P3-3's shared-bucket DoS guard.
	if isLoopbackIPString(ip) {
		return ip, true
	}
	return ip, false
}

// RequestHasResolvableClientIP reports whether r carries a usable per-client
// rate-limit key. In !trustedProxy mode every request has a key. In
// trustedProxy mode the request is resolvable when X-Forwarded-For carries a
// parseable last hop, OR when the request arrives directly on the loopback
// interface (no XFF, RemoteAddr is 127.0.0.1/::1) — the kernel-guaranteed
// on-host direct-access path. Single source of truth for the per-package
// copies in internal/server and internal/dashboard/auth.
func RequestHasResolvableClientIP(r *http.Request, trustedProxy bool) bool {
	_, ok := clientIPInternal(r, trustedProxy)
	return ok
}

// isLoopbackIPString parses a bare IP host (no port) and reports whether it is
// a loopback address. Empty / "@" (UDS RemoteAddr) is treated as loopback so
// filesystem-gated UDS deployments resolve; an unparseable string is treated
// as non-loopback so the ambiguous case fails closed.
func isLoopbackIPString(host string) bool {
	if host == "" || host == "@" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
