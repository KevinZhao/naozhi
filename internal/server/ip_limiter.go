package server

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ipLimiter provides per-IP rate limiting with automatic eviction.
type ipLimiter struct {
	mu           sync.Mutex
	entries      map[string]*ipLimiterEntry
	r            rate.Limit
	burst        int
	trustedProxy bool
}

type ipLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newIPLimiter(r rate.Limit, burst int) *ipLimiter {
	return &ipLimiter{
		entries: make(map[string]*ipLimiterEntry),
		r:       r,
		burst:   burst,
	}
}

func newIPLimiterWithProxy(r rate.Limit, burst int, trustedProxy bool) *ipLimiter {
	return &ipLimiter{
		entries:      make(map[string]*ipLimiterEntry),
		r:            r,
		burst:        burst,
		trustedProxy: trustedProxy,
	}
}

// Allow checks the rate limiter for the given remoteAddr (host:port or bare IP).
func (l *ipLimiter) Allow(remoteAddr string) bool {
	ip := remoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	return l.allowIP(ip)
}

// AllowRequest checks the rate limiter using the real client IP derived from r.
// Respects l.trustedProxy to read X-Forwarded-For when behind ALB/CloudFront.
func (l *ipLimiter) AllowRequest(r *http.Request) bool {
	return l.allowIP(clientIP(r, l.trustedProxy))
}

// allowIP is the shared implementation that operates on a resolved bare IP string.
func (l *ipLimiter) allowIP(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if e, ok := l.entries[ip]; ok {
		e.lastSeen = time.Now()
		return e.limiter.Allow()
	}

	// Evict stale entries when map is large.
	const maxEntries = 1000
	if len(l.entries) >= maxEntries {
		cutoff := time.Now().Add(-10 * time.Minute)
		for k, e := range l.entries {
			if e.lastSeen.Before(cutoff) {
				delete(l.entries, k)
			}
		}
	}

	limiter := rate.NewLimiter(l.r, l.burst)
	if len(l.entries) < maxEntries {
		l.entries[ip] = &ipLimiterEntry{limiter: limiter, lastSeen: time.Now()}
	}
	return limiter.Allow()
}
