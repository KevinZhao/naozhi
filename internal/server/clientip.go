package server

import (
	"net"
	"net/http"
	"strings"
)

// clientIP extracts the real client IP from the request.
// When trustedProxy is true (behind ALB/CloudFront), reads the last entry
// from X-Forwarded-For — the one appended by the trusted proxy, which cannot
// be spoofed by the client. Otherwise falls back to RemoteAddr.
func clientIP(r *http.Request, trustedProxy bool) string {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if trustedProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				ip = last
			}
		}
	}
	return ip
}
