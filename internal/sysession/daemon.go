// Package sysession implements naozhi's built-in background daemon
// framework. See docs/rfc/system-session.md for the design.
//
// At a glance:
//
//   - Daemon is a single-method ticker-driven worker (Tick(ctx) →
//     TickReport).  Each daemon owns one logical job; AutoTitler is the
//     first one (auto-renames sessions based on conversation content).
//   - Manager runs all daemons on independent goroutines, with per-daemon
//     CAS gates against overlapping ticks, panic recovery, and a hard
//     wg.Wait shutdown that is bounded by a caller-supplied ctx.
//   - Runner is the LLM-call abstraction: each call execs a transient
//     "claude -p" subprocess (= a transient system session).  Daemons
//     never share a long-lived CLI process — the v1 SharedCLI route was
//     ruled out (RFC §6.1).
//   - Built-in daemons live under internal/sysession/registry.go and are
//     all listed at compile time; runtime registration is forbidden.
//
// All public APIs are package-level interfaces so callers (notably
// cmd/naozhi/main.go and tests) can substitute fakes without dragging in
// concrete types.
package sysession

import "context"

// TickReport is the structured result a daemon hands back at the end of
// a single Tick.  Manager records it in the in-memory ring buffer and
// surfaces it via /api/system/daemons.
//
// Empty (zero-value) TickReport is allowed and means "the daemon ran but
// had no work to do" — common for AutoTitler when nothing crossed the
// minimum-turn threshold yet.
type TickReport struct {
	// Examined counts candidates the daemon inspected (post-prefilter).
	Examined int
	// Acted counts candidates where the daemon produced a side-effect
	// (e.g. AutoTitler successfully wrote a new label).
	Acted int
	// Skipped breaks down why candidates were rejected, keyed by reason
	// (e.g. "min_turns", "origin_user", "group_chat").  May be nil.
	Skipped map[string]int
}

// Daemon is the minimum contract every built-in worker implements.
//
// Daemon implementations must:
//   - Be safe to construct once at process start and call concurrently
//     from a single goroutine (Manager guarantees no overlapping Tick
//     for the same daemon — but daemons must NOT call into Router under
//     long-running operations like LLM calls; copy snapshots first, then
//     call out without locks).
//   - Honour ctx.Done():  Manager cancels ctx during shutdown and the
//     daemon must return promptly (≤ a few hundred ms).
//   - Be idempotent:  the same Tick run twice in a row should produce
//     ≤ the same set of side-effects as one run.  This is what lets us
//     add manual "trigger now" UI in Phase 2 without reworking the
//     interface.
//
// Daemon does NOT expose Start/Stop hooks.  Manager handles lifecycle.
type Daemon interface {
	// Name returns the kebab-case daemon name (matches sys:{name}).
	// Validated against ^[a-z][a-z0-9-]{1,30}$ at Manager startup.
	Name() string

	// Description is a one-line human-readable summary used by the
	// dashboard "System" drawer.  Plain text, no HTML.
	Description() string

	// Tick is the single unit of work.  Manager invokes Tick on the
	// daemon's configured cadence.  ctx is cancelled when Manager.Stop
	// is called or when the configured TickTimeout expires.
	//
	// Returning a non-nil error counts toward the failure-classification
	// counters (RFC §7.4):  errors.Is(err, context.DeadlineExceeded) is
	// classified as timeout (does NOT trigger circuit breaker); others
	// fall through error-class detection in Manager.classifyError.
	//
	// Tick may return a partial TickReport along with an error — the
	// pre-failure work is still credited to Acted/Examined for
	// observability.
	Tick(ctx context.Context) (TickReport, error)
}

// Configurable is an optional Daemon extension.  Daemons that need to
// read knobs from sysession.Config implement Configure; Manager calls
// it once at startup before the first Tick.  Daemons that don't need
// configuration simply skip this interface.
//
// cfg is opaque to the daemon framework — the daemon decodes whatever
// fields it cares about and returns an error to abort startup if the
// configuration is invalid (e.g. interval too short, missing required
// fields).  An error here disables that single daemon, not the whole
// Manager.
type Configurable interface {
	Configure(cfg DaemonConfig) error
}

// DaemonConfig is the per-daemon configuration view.  Manager builds it
// from sysession.Config.Daemons[name] before invoking Configure.  Each
// daemon decodes only the fields it understands; unknown fields are
// silently ignored (forward-compat for adding daemon knobs without
// breaking older daemons).
type DaemonConfig map[string]any
