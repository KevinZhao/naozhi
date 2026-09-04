package server

import (
	"net"
	"net/http"
	"time"

	"github.com/naozhi/naozhi/internal/netutil"
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

// defaultIPLimiterMaxKeys / defaultIPLimiterTTL pin the LRU cap + idle TTL
// for newIPLimiterWithProxy, aligned with the auth limiters: a small LRU lets
// an IP flood evict legitimately rate-limited entries. ~1.2 MiB worst case
// per bucket (#473).
const (
	defaultIPLimiterMaxKeys = 10_000
	defaultIPLimiterTTL     = time.Hour
)

func newIPLimiterWithProxy(r rate.Limit, burst int, trustedProxy bool) *ipLimiter {
	return &ipLimiter{
		inner: ratelimit.New(ratelimit.Config{
			Rate:    r,
			Burst:   burst,
			MaxKeys: defaultIPLimiterMaxKeys,
			TTL:     defaultIPLimiterTTL,
		}),
		trustedProxy: trustedProxy,
	}
}

// newIPLimiterWithCap is a sibling of newIPLimiterWithProxy that pins the
// underlying MaxKeys (LRU cap) and TTL explicitly for endpoints facing
// DDoS-class abuse (#636). Pass MaxKeys=0/TTL=0 for ratelimit defaults.
func newIPLimiterWithCap(r rate.Limit, burst, maxKeys int, ttl time.Duration, trustedProxy bool) *ipLimiter {
	return &ipLimiter{
		inner: ratelimit.New(ratelimit.Config{
			Rate:    r,
			Burst:   burst,
			MaxKeys: maxKeys,
			TTL:     ttl,
		}),
		trustedProxy: trustedProxy,
	}
}

// unknownIPKey is a shared bucket used when the real client IP cannot be
// resolved, preserving back-pressure without ratelimit.Allow("")'s hard
// reject. In trustedProxy mode one XFF-less flood (e.g. bypassing the proxy)
// would starve every other XFF-less caller through this bucket, so
// AllowRequest fails closed before reaching it (requestHasResolvableClientIP).
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
// honouring trustedProxy so proxied deployments rate-limit the real caller.
//
// When trustedProxy=true and X-Forwarded-For is absent or unparseable it
// fails closed (false) instead of sharing the unknownIPKey bucket: such a
// request is a proxy misconfiguration or a direct-to-origin attacker, and one
// source must not starve every other XFF-less caller. Callers wanting a 400
// rather than 429 can pre-check requestHasResolvableClientIP.
func (l *ipLimiter) AllowRequest(r *http.Request) bool {
	if !requestHasResolvableClientIP(r, l.trustedProxy) {
		return false
	}
	ip := clientIP(r, l.trustedProxy)
	if ip == "" {
		ip = unknownIPKey
	}
	return l.inner.Allow(ip)
}

// requestHasResolvableClientIP reports whether r carries a usable per-client
// rate-limit key. In !trustedProxy mode every request has one; in
// trustedProxy mode only an XFF-carrying request or a loopback direct
// connection is resolvable, an externally-routable XFF-less request is not.
func requestHasResolvableClientIP(r *http.Request, trustedProxy bool) bool {
	return netutil.RequestHasResolvableClientIP(r, trustedProxy)
}
