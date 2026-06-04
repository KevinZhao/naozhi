package sysession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// DaemonRunState is the terminal state of a single Tick run. It is a type
// alias to the cross-subsystem runtelemetry.RunState so cron + sysession
// share one wire vocabulary (R260528-ARCH-2 / #1363, R244-ARCH-18 / #1055
// direction). The alias keeps the sysession-local Daemon* constant names
// for call-site readability while the underlying values come from the
// single runtelemetry source of truth. Adding a new state must happen in
// runtelemetry/state.go, not here.
type DaemonRunState = runtelemetry.RunState

const (
	DaemonRunSucceeded = runtelemetry.RunStateSucceeded
	DaemonRunFailed    = runtelemetry.RunStateFailed
	DaemonRunTimedOut  = runtelemetry.RunStateTimedOut
	DaemonRunCanceled  = runtelemetry.RunStateCanceled
)

// DaemonErrorClass classifies the failure mode of a run.  Phase 1
// drives the circuit breaker (RFC §7.4):
//
//   - Validation errors do NOT trip the breaker (a single malformed
//     candidate shouldn't disable the daemon globally).
//   - Upstream errors (Runner exec failures, non-zero exit) DO trip
//     the breaker after consecutiveCLIFailureLimit consecutive hits.
//   - Timeout errors are surfaced for observability but do NOT trip the
//     breaker either — slow LLM calls happen and a transient run of
//     timeouts shouldn't shut the daemon down for hours.
//   - Panic always trips the breaker after the same limit; a daemon
//     that panics deterministically is broken.
//
// DaemonErrorClass is type-aliased to runtelemetry.ErrorClass so cron +
// sysession share one error-class type (R260528-ARCH-18 / #1379). Five of
// the six sysession values map 1:1 onto runtelemetry constants; the lone
// exception is DaemonErrorClassTimeout, whose pre-merge wire string
// "timeout" deliberately differs from runtelemetry's canonical
// ErrClassDeadlineExceeded ("deadline_exceeded"). server's
// mapSysessionErrorClass owns that normalisation before broadcast, so the
// literal stays here verbatim to preserve the existing wire shape.
type DaemonErrorClass = runtelemetry.ErrorClass

const (
	DaemonErrorClassNone                        = runtelemetry.ErrClassNone
	DaemonErrorClassValidation                  = runtelemetry.ErrClassSysessionValidation
	DaemonErrorClassUpstream                    = runtelemetry.ErrClassSysessionUpstream
	DaemonErrorClassTimeout    DaemonErrorClass = "timeout"
	DaemonErrorClassPanic                       = runtelemetry.ErrClassPanic
	// DaemonErrorClassCanceled tags runs that returned context.Canceled
	// (e.g. naozhi shutting down mid-tick or operator-driven Stop).
	// Distinct from DaemonErrorClassNone so dashboards / log analytics
	// can tell "successful tick" apart from "tick aborted by ctx" — the
	// State field already differentiates them (DaemonRunCanceled vs
	// DaemonRunSucceeded), but ErrorClass shipped over the WS wire used
	// to collapse to "" in both cases. Closes R236-QA-05.
	//
	// Like Timeout, Canceled does NOT trip the circuit breaker:  a
	// daemon returning ctx.Canceled because operator hit Stop is not a
	// daemon bug. recordRun's switch keeps it on the "no counter
	// change" branch (default case) so we don't reset success counters
	// either.
	DaemonErrorClassCanceled = runtelemetry.ErrClassCanceled
)

// DaemonTriggerKind distinguishes scheduled ticks from manual triggers.
// Phase 1 only produces "scheduled"; "manual" is reserved for the Phase 2
// dashboard "trigger now" button. Type-aliased to runtelemetry.TriggerKind
// so cron + sysession share one trigger vocabulary (R260528-ARCH-2 / #1363).
type DaemonTriggerKind = runtelemetry.TriggerKind

const (
	DaemonTriggerScheduled = runtelemetry.TriggerScheduled
	// DaemonTriggerManual is RESERVED for the Phase 2 dashboard
	// "trigger now" button. Phase 1 only produces Scheduled; no
	// production code path emits this value today, and any DaemonRun
	// observed with Trigger=manual is either a forward-compat schema
	// bump or a test fixture. Tracked in docs/TODO.md R232-CR-8.
	DaemonTriggerManual = runtelemetry.TriggerManual
)

// DaemonRun is the in-memory record of a completed Tick.  Manager keeps
// a per-daemon ring buffer (see runring.go) and exposes them via the
// /api/system/daemons read endpoint.  Phase 1 does NOT persist to disk
// — RFC §3.4.
type DaemonRun struct {
	RunID      string            `json:"run_id"`
	Name       string            `json:"name"`
	State      DaemonRunState    `json:"state"`
	Trigger    DaemonTriggerKind `json:"trigger,omitempty"`
	StartedAt  time.Time         `json:"started_at"`
	EndedAt    time.Time         `json:"ended_at,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`

	// ErrorClass is broadcast over WS but ErrorMsg is intentionally NOT
	// (see RFC §9.4):  the upstream-error path may echo prompt content
	// back through the CLI, and broadcasting that to every connected
	// dashboard would leak conversation excerpts cross-tenant.  ErrorMsg
	// is recorded server-side via slog only.
	ErrorClass DaemonErrorClass `json:"error_class,omitempty"`
	ErrorMsg   string           `json:"-"`

	// Stats carries the daemon-specific counters from TickReport.
	// Aggregated as a flat map so dashboard widgets can render without
	// knowing which daemon produced the run.
	Stats map[string]int64 `json:"stats,omitempty"`
}

// newRunID generates a 16-hex-char identifier for a single run.  Not a
// security boundary — only for log correlation / dashboard linking.
// Falls back to a deterministic prefix on rand.Read failure (extremely
// rare; only happens if /dev/urandom is unreadable, in which case the
// process has bigger problems).
func newRunID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Best-effort fallback so we always produce a non-empty ID.
		return "fallback-" + time.Now().UTC().Format("150405.000")
	}
	return hex.EncodeToString(buf[:])
}

// classifyError maps a Tick error into (DaemonRunState, DaemonErrorClass).
// Pure function so unit tests can pin the mapping without spinning up a
// Manager.
//
// Manager calls this with the result of a single Tick.  isPanic
// distinguishes panic-recovered errors (which produce a synthetic
// fmt.Errorf wrapping the panic value) from organic errors so we can
// tag them as DaemonErrorClassPanic without string-matching.
//
// Priority order matters:
//
//  1. nil → success (early out).
//  2. isPanic → panic class, regardless of any wrapped error value.
//     A daemon that captured a ctx error before panicking would
//     otherwise classify as timeout/canceled and silently slip past
//     the breaker — RFC §7.4 explicitly counts panics toward the
//     CLI-failure breaker because they indicate broken daemon code.
//  3. ctx.DeadlineExceeded / Canceled → timeout / canceled.
//  4. ErrValidation sentinel → validation (does NOT trip breaker).
//  5. Default → upstream (counts toward breaker).
func classifyError(err error, isPanic bool) (DaemonRunState, DaemonErrorClass) {
	if err == nil {
		return DaemonRunSucceeded, DaemonErrorClassNone
	}
	if isPanic {
		return DaemonRunFailed, DaemonErrorClassPanic
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return DaemonRunTimedOut, DaemonErrorClassTimeout
	}
	if errors.Is(err, context.Canceled) {
		return DaemonRunCanceled, DaemonErrorClassCanceled
	}
	// validation vs upstream is decided by the daemon (it embeds a
	// sentinel error or wraps with errValidation).  Default to upstream
	// because that's the conservative breaker-tripping classification.
	if errors.Is(err, ErrValidation) {
		return DaemonRunFailed, DaemonErrorClassValidation
	}
	return DaemonRunFailed, DaemonErrorClassUpstream
}

// ErrValidation is the sentinel daemon implementations wrap their
// validation failures with so classifyError can route them to
// DaemonErrorClassValidation (which does NOT trip the circuit breaker).
//
// Usage in a daemon:
//
//	if !looksLikeAValidTitle(title) {
//	    return report, fmt.Errorf("title rejected: %w", sysession.ErrValidation)
//	}
var ErrValidation = errors.New("sysession: validation error")
