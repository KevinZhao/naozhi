package project

import (
	"net/http"
	"sync"
)

// perIPBurst1Limiter is a test-only IPLimiter that emulates the per-IP
// "1 burst then deny" semantics of newIPLimiterWithProxy(rate.Every(time.
// Hour), 1, false). Used by TestHandleFilesExists_RateLimit which formerly
// relied on server-package newIPLimiterWithProxy. After Phase 2
// (server-split-phase4-design.md §6.5 Plan B) the test runs inside this
// package and uses this fake to avoid pulling in server-package private
// constructors.
//
// Behaviour: per remoteAddr (or per r.RemoteAddr host part for AllowRequest),
// the first call returns true; subsequent calls from the same IP return
// false. A new IP gets its own bucket.
type perIPBurst1Limiter struct {
	mu   sync.Mutex
	used map[string]bool
}

func newPerIPBurst1Limiter() *perIPBurst1Limiter {
	return &perIPBurst1Limiter{used: make(map[string]bool)}
}

func (l *perIPBurst1Limiter) Allow(remoteAddr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used[remoteAddr] {
		return false
	}
	l.used[remoteAddr] = true
	return true
}

func (l *perIPBurst1Limiter) AllowRequest(r *http.Request) bool {
	ip := r.RemoteAddr
	for i := len(ip) - 1; i >= 0; i-- {
		if ip[i] == ':' {
			ip = ip[:i]
			break
		}
	}
	return l.Allow(ip)
}
