package server

import (
	"net"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ipLimiter provides per-IP rate limiting with automatic eviction.
type ipLimiter struct {
	mu      sync.Mutex
	entries map[string]*ipLimiterEntry
	r       rate.Limit
	burst   int
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

// Allow checks the rate limiter for the given remoteAddr (host:port or bare IP).
func (l *ipLimiter) Allow(remoteAddr string) bool {
	ip := remoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}

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
