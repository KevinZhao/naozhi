package memory

import "net/http"

// alwaysAllowLimiter is the trivial pass-through used by tests that need
// to satisfy the IPLimiter interface but not actually gate. Mirrors the
// legacy newIPLimiterWithProxy(rate.Inf, 1, false) "essentially unlimited"
// shape.
type alwaysAllowLimiter struct{}

func (alwaysAllowLimiter) Allow(string) bool               { return true }
func (alwaysAllowLimiter) AllowRequest(*http.Request) bool { return true }
