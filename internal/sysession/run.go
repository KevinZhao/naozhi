package sysession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// DaemonRunState is the terminal state of a single Tick run.  Mirrors
// cron.RunState semantics so a future Phase 2 dashboard can reuse the
// same widget for both subsystems.
type DaemonRunState string

const (
	DaemonRunSucceeded DaemonRunState = "succeeded"
	DaemonRunFailed    DaemonRunState = "failed"
	DaemonRunTimedOut  DaemonRunState = "timed_out"
	DaemonRunCanceled  DaemonRunState = "canceled"
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
type DaemonErrorClass string

const (
	DaemonErrorClassNone       DaemonErrorClass = ""
	DaemonErrorClassValidation DaemonErrorClass = "validation"
	DaemonErrorClassUpstream   DaemonErrorClass = "upstream"
	DaemonErrorClassTimeout    DaemonErrorClass = "timeout"
	DaemonErrorClassPanic      DaemonErrorClass = "panic"
)

// DaemonTriggerKind distinguishes scheduled ticks from manual triggers.
// Phase 1 only produces "scheduled"; "manual" is reserved for the Phase 2
// dashboard "trigger now" button.
type DaemonTriggerKind string

const (
	DaemonTriggerScheduled DaemonTriggerKind = "scheduled"
	// DaemonTriggerManual is reserved for the Phase 2 dashboard
	// "trigger now" button. Phase 1 only produces Scheduled; manual
	// will be passed by the future API endpoint that calls runOnce
	// outside the ticker loop.
	//
	// R232-CR-8 WARNING: PLACEHOLDER — no producer in tree. Phase 2's
	// trigger-now endpoint will be the first writer; until then this
	// value is unreachable and any UI branch on it is dead code.
	DaemonTriggerManual DaemonTriggerKind = "manual"
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

// DaemonRunStartedEvent is published when a Tick begins (post-CAS gate).
// Dashboard subscribers update the "in-flight" indicator.
type DaemonRunStartedEvent struct {
	Name      string            `json:"name"`
	RunID     string            `json:"run_id"`
	Trigger   DaemonTriggerKind `json:"trigger,omitempty"`
	StartedAt time.Time         `json:"started_at"`
}

// DaemonRunEndedEvent is published on terminal Tick completion.
// ErrorMsg is omitted by design; subscribers must look at ErrorClass.
type DaemonRunEndedEvent struct {
	Name       string            `json:"name"`
	RunID      string            `json:"run_id"`
	State      DaemonRunState    `json:"state"`
	DurationMS int64             `json:"duration_ms,omitempty"`
	ErrorClass DaemonErrorClass  `json:"error_class,omitempty"`
	Trigger    DaemonTriggerKind `json:"trigger,omitempty"`
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
		return DaemonRunCanceled, DaemonErrorClassNone
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
