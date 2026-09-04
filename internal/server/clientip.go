package server

import (
	"net"
	"net/http"

	"github.com/naozhi/naozhi/internal/netutil"
)

// clientIP is a thin wrapper around netutil.ClientIP (trusted-proxy XFF
// last-hop, IP validation).
func clientIP(r *http.Request, trustedProxy bool) string {
	return netutil.ClientIP(r, trustedProxy)
}

// isLoopbackClient reports whether the request's *real* client is on the
// loopback interface, for the pprof/expvar loopback gate.
//
// Behind a trusted proxy r.RemoteAddr is always the proxy's (loopback) IP, so
// the gate must use the XFF last hop instead; a request with no usable XFF
// collapses to RemoteAddr, which in a proxy-fronted deployment is exactly the
// direct on-host connection the gate means to allow. UDS ("" / "@") counts as
// loopback.
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
