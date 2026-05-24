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
//
// R244-SEC-P3-3 [REPEAT-3]: in trustedProxy mode the unknown bucket is a
// shared-tenancy DoS amplifier — a single attacker who bypasses the proxy
// (hitting the origin directly with no X-Forwarded-For) shares the same
// bucket as every other XFF-less probe, so a small flood from one source
// can starve the bucket and 429 every other XFF-less caller. The fix
// surface is at the request boundary, not here: see AllowRequest +
// requestHasResolvableClientIP. Allow(remoteAddr) is unchanged because
// its only caller (auth pre-check by parsed IP) has already done the
// resolution work.
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
//
// R244-SEC-P3-3 [REPEAT-3]: when trustedProxy=true and X-Forwarded-For is
// absent or unparseable, AllowRequest rejects (returns false) rather than
// rate-limiting against the shared unknownIPKey bucket. Two reasons:
//
//  1. In trustedProxy mode every legitimate request MUST traverse the
//     trusted proxy, which always appends a client IP to XFF. A request
//     missing XFF is either a proxy misconfiguration or an attacker who
//     reached the origin directly — both should be denied, not throttled.
//  2. The shared unknownIPKey bucket would otherwise let a single
//     attacker burn the bucket and starve every other XFF-less probe.
//
// Callers that want to surface the misconfiguration as a 400 (rather than
// the limiter's default 429) can pre-check via requestHasResolvableClientIP.
// Existing call sites that map AllowRequest=false → 429 keep working: the
// new branch fails closed instead of silently sharing the bucket.
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
// rate-limit key. In !trustedProxy mode every request has a key (RemoteAddr
// is set by net/http even for UDS via the listener fallback). In trustedProxy
// mode the request is only resolvable when X-Forwarded-For carries at least
// one parseable IP — the same gate clientIP applies before adopting the XFF
// tail. Exposed at package level so handlers wanting to return 400 (rather
// than the limiter's 429) on a misconfigured proxy can short-circuit.
func requestHasResolvableClientIP(r *http.Request, trustedProxy bool) bool {
	if !trustedProxy {
		// RemoteAddr is set by http.Server for every accepted connection;
		// even UDS deployments reach this branch with empty / "@" — the
		// rate limiter treats those as unknownIPKey, which is acceptable
		// because the kernel has already filesystem-gated UDS access.
		return true
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return false
	}
	tail := xff
	if i := lastComma(xff); i >= 0 {
		tail = xff[i+1:]
	}
	tail = trimSpace(tail)
	return tail != "" && net.ParseIP(tail) != nil
}

// lastComma / trimSpace are tiny inlinable helpers kept local to avoid
// pulling strings into ip_limiter.go for one function. Mirrors the
// netutil.ClientIP zero-alloc XFF parse so behaviour stays identical.
func lastComma(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ',' {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
