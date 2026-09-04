// Package netutil holds small network-address helpers shared across packages
// that cannot depend on each other (e.g. internal/server and internal/node).
package netutil

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP extracts the real client IP from r. With trustedProxy (behind
// ALB/CloudFront) it takes the LAST X-Forwarded-For entry — appended by the
// trusted proxy, so the client cannot spoof it — validated via net.ParseIP so a
// malformed header cannot mint a bogus rate-limit key. Otherwise, or when no
// usable XFF is present, it falls back to r.RemoteAddr (which in an ALB-fronted
// deployment collapses XFF-less requests into the single proxy-IP bucket).
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
	// No usable XFF in trustedProxy mode normally means the request bypassed
	// the proxy → unresolvable (shared-bucket DoS guard). A loopback RemoteAddr
	// is kernel-guaranteed on-host (SSH tunnel / local curl), the same path
	// isLoopbackClient allows, so it resolves to a real per-IP key.
	if isLoopbackIPString(ip) {
		return ip, true
	}
	return ip, false
}

// RequestHasResolvableClientIP reports whether r carries a usable per-client
// rate-limit key: always in !trustedProxy mode; in trustedProxy mode when XFF
// has a parseable last hop or the request arrived directly on loopback. Single
// source of truth for internal/server and internal/dashboard/auth.
func RequestHasResolvableClientIP(r *http.Request, trustedProxy bool) bool {
	_, ok := clientIPInternal(r, trustedProxy)
	return ok
}

// isLoopbackIPString reports whether a bare IP host is loopback. Empty / "@"
// (UDS RemoteAddr) counts as loopback so filesystem-gated UDS deployments
// resolve; an unparseable string fails closed as non-loopback.
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
