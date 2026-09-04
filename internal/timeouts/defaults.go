package timeouts

import (
	"sync"
	"testing"
	"time"
)

// Defaults bundles the canonical timeout values. A struct (rather than
// individual constants) keeps the API stable as fields are added and lets
// tests mutate a copy without locking. Obtain it via [Get].
//
//   - HTTPIdle: server-side WebSocket / SSE idle keepalive cutoff.
//   - HTTPRead: per-request read timeout for non-streaming JSON handlers.
//   - HTTPShutdown: graceful drain budget on SIGTERM.
//   - CLIClose: child-process Close grace period before SIGKILL.
//   - CLIInterrupt: interrupt-to-idle wait when stopping a streaming turn.
//   - SessionReboot: supervisor cooldown between forced restarts.
type Defaults struct {
	HTTPIdle      time.Duration
	HTTPRead      time.Duration
	HTTPShutdown  time.Duration
	CLIClose      time.Duration
	CLIInterrupt  time.Duration
	SessionReboot time.Duration
}

// canonical holds the live (overridable) defaults, exposed only via [Get] /
// [Override] so callers cannot grab a mutable pointer.
var (
	canonicalMu sync.RWMutex
	canonical   = Defaults{
		HTTPIdle:      120 * time.Second,
		HTTPRead:      15 * time.Second,
		HTTPShutdown:  10 * time.Second,
		CLIClose:      8 * time.Second,
		CLIInterrupt:  5 * time.Second,
		SessionReboot: 2 * time.Second,
	}
)

// Get returns a copy of the canonical timeouts. Production code reads it
// once at startup; tests use [Override] rather than comparing against a
// stale snapshot.
func Get() Defaults {
	canonicalMu.RLock()
	defer canonicalMu.RUnlock()
	return canonical
}

// Override mutates the canonical [Defaults] via set for the duration of t
// and registers a t.Cleanup restoring the prior value. The struct is
// process-wide, so it is only safe under t.Parallel when the test does not
// rely on the override across the parallel split.
//
//	timeouts.Override(t, func(d *timeouts.Defaults) {
//	    d.CLIClose = 50 * time.Millisecond
//	})
//
// Returns the post-override snapshot.
func Override(t *testing.T, set func(*Defaults)) Defaults {
	t.Helper()
	if set == nil {
		canonicalMu.RLock()
		defer canonicalMu.RUnlock()
		return canonical
	}
	canonicalMu.Lock()
	prior := canonical
	updated := canonical
	set(&updated)
	canonical = updated
	canonicalMu.Unlock()
	t.Cleanup(func() {
		canonicalMu.Lock()
		canonical = prior
		canonicalMu.Unlock()
	})
	return updated
}
