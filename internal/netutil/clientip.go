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
	if xff == "" {
		return ip, false
	}
	tail := xff
	if i := strings.LastIndexByte(xff, ','); i >= 0 {
		tail = xff[i+1:]
	}
	tail = strings.TrimSpace(tail)
	if tail == "" || net.ParseIP(tail) == nil {
		return ip, false
	}
	return tail, true
}
