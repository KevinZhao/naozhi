package timeouts

import (
	"sync"
	"testing"
	"time"
)

// Defaults bundles the canonical timeout values that today live as
// package-scope `var` declarations across the codebase. Returning a struct
// (rather than exporting individual constants) keeps the API stable when we
// add fields and lets tests mutate a copy without locking.
//
// Field semantics:
//
//   - HTTPIdle      — server-side WebSocket / SSE idle keepalive cutoff.
//   - HTTPRead      — per-request read timeout for non-streaming JSON
//     handlers.
//   - HTTPShutdown  — graceful drain budget on SIGTERM.
//   - CLIClose      — child-process Close grace period before SIGKILL.
//   - CLIInterrupt  — interrupt-to-idle wait when stopping a streaming
//     turn.
//   - SessionReboot — supervisor cooldown between forced restarts.
//
// All values are populated by [Defaults]; callers should not construct
// the struct directly.
type Defaults struct {
	HTTPIdle      time.Duration
	HTTPRead      time.Duration
	HTTPShutdown  time.Duration
	CLIClose      time.Duration
	CLIInterrupt  time.Duration
	SessionReboot time.Duration
}

// canonical holds the live (overridable) defaults. Exposed only via the
// package-level [Defaults] / [Override] helpers so callers cannot grab a
// mutable pointer and scribble across goroutines.
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

// Get returns the canonical timeout struct. Each call returns a fresh
// value (struct copy) so the caller can stash the result without a
// lifetime entanglement with the package mutex.
//
// Production code reads the result once at startup; tests should use
// [Override] to flip a single field for the duration of one test instead
// of comparing against a stale snapshot.
func Get() Defaults {
	canonicalMu.RLock()
	defer canonicalMu.RUnlock()
	return canonical
}

// Override replaces a single field of the canonical [Defaults] for the
// duration of t and registers a t.Cleanup to restore the prior value.
// It is safe under t.Parallel only when the test does not actually rely
// on the override sticking across the parallel split — the canonical
// struct is process-wide.
//
// The `set` callback receives a *Defaults so the caller can write
// directly without re-typing the field name in two places:
//
//	timeouts.Override(t, func(d *timeouts.Defaults) {
//	    d.CLIClose = 50 * time.Millisecond
//	})
//
// Returns the new (post-override) snapshot for convenience.
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
