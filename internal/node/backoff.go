package node

import (
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// jitterBackoff delegates to osutil.JitterBackoff so the three reconnect
// loops (node relay, upstream connector, platform adapters) share a single
// implementation. Wrapper kept so existing call sites and backoff_test.go
// continue to compile without touching every line.
func jitterBackoff(d time.Duration) time.Duration {
	return osutil.JitterBackoff(d)
}
