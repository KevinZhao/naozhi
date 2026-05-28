package cron

import (
	"net/http"
	"sync"
)

// perIPBurstNLimiter is a test-only IPLimiter that emulates the per-IP "N
// burst then deny" semantics of newIPLimiterWithProxy(rate.Every(time.Hour),
// N, false). After Phase 1 (server-split-phase4-design.md §6.5 Plan B) the
// cron tests live in this package and can't reach server's private ctor;
// this fake covers the burst-1/burst-2 cases the original tests used.
type perIPBurstNLimiter struct {
	mu    sync.Mutex
	burst int
	used  map[string]int
}

func newPerIPBurstNLimiter(burst int) *perIPBurstNLimiter {
	return &perIPBurstNLimiter{burst: burst, used: make(map[string]int)}
}

func (l *perIPBurstNLimiter) Allow(remoteAddr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used[remoteAddr] >= l.burst {
		return false
	}
	l.used[remoteAddr]++
	return true
}

func (l *perIPBurstNLimiter) AllowRequest(r *http.Request) bool {
	ip := r.RemoteAddr
	for i := len(ip) - 1; i >= 0; i-- {
		if ip[i] == ':' {
			ip = ip[:i]
			break
		}
	}
	return l.Allow(ip)
}

// alwaysAllowLimiter is the trivial pass-through used by tests that need to
// satisfy the IPLimiter interface but don't actually want to gate.
type alwaysAllowLimiter struct{}

func (alwaysAllowLimiter) Allow(string) bool               { return true }
func (alwaysAllowLimiter) AllowRequest(*http.Request) bool { return true }
