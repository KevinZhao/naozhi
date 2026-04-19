package server

import (
	"net"
	"net/http"

	"github.com/naozhi/naozhi/internal/ratelimit"
	"golang.org/x/time/rate"
)

// ipLimiter is a thin server-package adapter over ratelimit.Limiter.
// It keeps the trusted-proxy flag co-located with the limiter so
// AllowRequest can resolve the real client IP without the caller
// plumbing the flag through.
type ipLimiter struct {
	inner        *ratelimit.Limiter
	trustedProxy bool
}

func newIPLimiterWithProxy(r rate.Limit, burst int, trustedProxy bool) *ipLimiter {
	return &ipLimiter{
		inner:        ratelimit.New(ratelimit.Config{Rate: r, Burst: burst}),
		trustedProxy: trustedProxy,
	}
}

// unknownIPKey is a shared bucket used when the real client IP cannot be
// resolved. Keeping unknown clients in one bucket preserves back-pressure
// against abuse while avoiding ratelimit.Allow("")'s hard reject which
// would otherwise permanently 429 legitimate callers whose RemoteAddr we
// failed to parse.
const unknownIPKey = "_unknown_"

// Allow checks the limiter for the given remoteAddr (host:port or bare IP).
func (l *ipLimiter) Allow(remoteAddr string) bool {
	ip := remoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if ip == "" {
		ip = unknownIPKey
	}
	return l.inner.Allow(ip)
}

// AllowRequest checks the limiter using the real client IP derived from r,
// honouring trustedProxy so ALB/CloudFront deployments rate-limit the
// real caller rather than the proxy's single IP.
func (l *ipLimiter) AllowRequest(r *http.Request) bool {
	ip := clientIP(r, l.trustedProxy)
	if ip == "" {
		ip = unknownIPKey
	}
	return l.inner.Allow(ip)
}
