package sysession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// DaemonRunState is the terminal state of a single Tick run, aliased to
// runtelemetry.RunState so cron + sysession share one wire vocabulary (#1363).
// Add new states in runtelemetry/state.go, not here.
type DaemonRunState = runtelemetry.RunState

const (
	DaemonRunSucceeded = runtelemetry.RunStateSucceeded
	DaemonRunFailed    = runtelemetry.RunStateFailed
	DaemonRunTimedOut  = runtelemetry.RunStateTimedOut
	DaemonRunCanceled  = runtelemetry.RunStateCanceled
)

// DaemonErrorClass classifies the failure mode of a run and drives the circuit
// breaker (RFC §7.4): Validation, Timeout and Canceled do NOT trip it; Upstream
// (Runner exec failure / non-zero exit) and Panic trip it after
// consecutiveCLIFailureLimit consecutive hits. Aliased to runtelemetry.ErrorClass
// (#1379). DaemonErrorClassTimeout keeps the wire literal "timeout" (vs the
// canonical "deadline_exceeded"); mapSysessionErrorClass in telemetry.go
// normalises it before broadcast.
type DaemonErrorClass = runtelemetry.ErrorClass

const (
	DaemonErrorClassNone                        = runtelemetry.ErrClassNone
	DaemonErrorClassValidation                  = runtelemetry.ErrClassSysessionValidation
	DaemonErrorClassUpstream                    = runtelemetry.ErrClassSysessionUpstream
	DaemonErrorClassTimeout    DaemonErrorClass = "timeout"
	DaemonErrorClassPanic                       = runtelemetry.ErrClassPanic
	// DaemonErrorClassCanceled tags runs that returned context.Canceled
	// (shutdown mid-tick or operator Stop). Distinct from None so dashboards
	// can tell "successful tick" from "aborted by ctx". Does NOT trip the
	// breaker and does not reset success counters (recordRun default case).
	DaemonErrorClassCanceled = runtelemetry.ErrClassCanceled
)

// DaemonTriggerKind distinguishes scheduled ticks from manual triggers;
// aliased to runtelemetry.TriggerKind (#1363).
type DaemonTriggerKind = runtelemetry.TriggerKind

const (
	DaemonTriggerScheduled = runtelemetry.TriggerScheduled
	// DaemonTriggerManual is RESERVED for the dashboard "trigger now" button;
	// no production code path emits it today.
	DaemonTriggerManual = runtelemetry.TriggerManual
)

// DaemonRun is the in-memory record of a completed Tick, kept in a per-daemon
// ring buffer (runring.go) and exposed via /api/system/daemons. Not persisted
// to disk (RFC §3.4).
type DaemonRun struct {
	RunID      string            `json:"run_id"`
	Name       string            `json:"name"`
	State      DaemonRunState    `json:"state"`
	Trigger    DaemonTriggerKind `json:"trigger,omitempty"`
	StartedAt  time.Time         `json:"started_at"`
	EndedAt    time.Time         `json:"ended_at,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`

	// ErrorClass is broadcast over WS but ErrorMsg intentionally is NOT (RFC
	// §9.4): the upstream-error path may echo prompt content and broadcasting
	// it would leak conversation excerpts cross-tenant. ErrorMsg is slog-only.
	ErrorClass DaemonErrorClass `json:"error_class,omitempty"`
	ErrorMsg   string           `json:"-"`

	// Stats carries the daemon-specific counters from TickReport as a flat map.
	Stats map[string]int64 `json:"stats,omitempty"`
}

// newRunID generates a 16-hex-char identifier for log correlation / dashboard
// linking — not a security boundary. Falls back to a time-derived prefix if
// rand.Read fails.
func newRunID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "fallback-" + time.Now().UTC().Format("150405.000")
	}
	return hex.EncodeToString(buf[:])
}

// classifyError maps a Tick error into (DaemonRunState, DaemonErrorClass).
// isPanic marks panic-recovered errors so they are tagged Panic without
// string-matching. Priority order matters:
//
//  1. nil → success.
//  2. isPanic → panic, regardless of any wrapped ctx error: RFC §7.4 counts
//     panics toward the CLI-failure breaker, so a daemon that captured a ctx
//     error before panicking must not slip past as timeout/canceled.
//  3. ctx.DeadlineExceeded / Canceled → timeout / canceled.
//  4. ErrValidation → validation (no breaker); default → upstream (breaker).
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
	// Default to upstream: the conservative breaker-tripping classification.
	if errors.Is(err, ErrValidation) {
		return DaemonRunFailed, DaemonErrorClassValidation
	}
	return DaemonRunFailed, DaemonErrorClassUpstream
}

// ErrValidation is the sentinel daemons wrap validation failures with so
// classifyError routes them to DaemonErrorClassValidation (no breaker trip):
//
//	return report, fmt.Errorf("title rejected: %w", sysession.ErrValidation)
var ErrValidation = errors.New("sysession: validation error")
