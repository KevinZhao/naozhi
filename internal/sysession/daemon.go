// Package sysession implements naozhi's built-in background daemon framework
// (docs/rfc/system-session.md).
//
//   - Daemon is a single-method ticker-driven worker (Tick(ctx) → TickReport);
//     AutoTitler is the first one.
//   - Manager runs all daemons on independent goroutines with per-daemon CAS
//     gates against overlapping ticks, panic recovery, and a ctx-bounded
//     wg.Wait shutdown.
//   - Runner execs a transient "claude -p" subprocess per LLM call; daemons
//     never share a long-lived CLI process (RFC §6.1).
//   - Built-in daemons are listed at compile time in registry.go; runtime
//     registration is forbidden.
//
// Public APIs are interfaces so cmd/naozhi and tests can substitute fakes.
package sysession

import "context"

// TickReport is the structured result of a single Tick, recorded in the ring
// buffer and surfaced via /api/system/daemons. The zero value means "ran but
// had no work to do".
type TickReport struct {
	// Examined counts candidates inspected (post-prefilter).
	Examined int
	// Acted counts candidates where the daemon produced a side-effect.
	Acted int
	// Skipped breaks down rejections by reason (e.g. "min_turns"). May be nil.
	Skipped map[string]int
}

// Daemon is the minimum contract every built-in worker implements. Manager
// guarantees no overlapping Tick for the same daemon and owns lifecycle (no
// Start/Stop hooks). Implementations must NOT call into Router during
// long-running operations like LLM calls (copy snapshots first, then call
// out without locks); must honour ctx.Done() promptly (≤ a few hundred ms)
// on shutdown; and must be idempotent so a manual "trigger now" is safe.
type Daemon interface {
	// Name returns the kebab-case daemon name (matches sys:{name}).
	// Validated against ^[a-z][a-z0-9-]{1,30}$ at Manager startup.
	Name() string

	// Description is a one-line plain-text summary for the dashboard "System" drawer.
	Description() string

	// Tick is the single unit of work, invoked on the configured cadence. ctx
	// is cancelled on Manager.Stop or when TickTimeout expires. A non-nil error
	// feeds classifyError (RFC §7.4); a partial TickReport returned alongside
	// an error is still credited to Acted/Examined.
	// ctx carries the run identity Runner calls book their cost to; derive
	// child contexts from it, never from context.Background().
	Tick(ctx context.Context) (TickReport, error)
}

// Configurable is an optional Daemon extension: Manager calls Configure once
// at startup before the first Tick. cfg is opaque to the framework; an error
// disables that single daemon, not the whole Manager.
type Configurable interface {
	Configure(cfg DaemonConfig) error
}

// DaemonConfig is the per-daemon view of sysession.Config.Daemons[name].
// Unknown fields are ignored for forward-compat.
type DaemonConfig map[string]any
