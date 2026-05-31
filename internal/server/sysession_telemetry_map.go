package server

import (
	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/sysession"
)

// sysession→runtelemetry enum translation.
//
// The two packages own separate enum vocabularies (sysession.Daemon* vs
// runtelemetry.*) and the strings DO NOT line up 1:1. A bare conversion
// such as runtelemetry.ErrorClass(ev.ErrorClass) silently produces an
// invalid enum value when the wire strings diverge — concretely
// sysession's DaemonErrorClassTimeout = "timeout" has no matching
// runtelemetry constant (the canonical timeout class is
// ErrClassDeadlineExceeded = "deadline_exceeded", which is the string
// dashboard.js's cronErrorClassLabel keys off to render "超时"). The bare
// cast leaked the raw "timeout" string to the WS wire, which the
// dashboard then rendered verbatim instead of the friendly label.
//
// These explicit maps make every cross-package value a deliberate,
// reviewable decision and default unknown inputs to a safe runtelemetry
// constant rather than minting an undefined enum. Adding a sysession enum
// value forces a visible compile-time-adjacent choice here.

// mapSysessionTrigger translates a sysession trigger to its runtelemetry
// equivalent. Unknown values fall back to TriggerScheduled (the only
// production-emitted value today).
func mapSysessionTrigger(t sysession.DaemonTriggerKind) runtelemetry.TriggerKind {
	switch t {
	case sysession.DaemonTriggerScheduled:
		return runtelemetry.TriggerScheduled
	case sysession.DaemonTriggerManual:
		return runtelemetry.TriggerManual
	default:
		return runtelemetry.TriggerScheduled
	}
}

// mapSysessionRunState translates a terminal sysession run state to its
// runtelemetry equivalent. Unknown values fall back to RunStateFailed so
// an unexpected state is surfaced as a failure rather than a silently
// invalid enum.
func mapSysessionRunState(s sysession.DaemonRunState) runtelemetry.RunState {
	switch s {
	case sysession.DaemonRunSucceeded:
		return runtelemetry.RunStateSucceeded
	case sysession.DaemonRunFailed:
		return runtelemetry.RunStateFailed
	case sysession.DaemonRunTimedOut:
		return runtelemetry.RunStateTimedOut
	case sysession.DaemonRunCanceled:
		return runtelemetry.RunStateCanceled
	default:
		return runtelemetry.RunStateFailed
	}
}

// mapSysessionErrorClass translates a sysession error class to its
// runtelemetry equivalent. The notable normalisation is
// DaemonErrorClassTimeout ("timeout") → ErrClassDeadlineExceeded
// ("deadline_exceeded"): sysession and the rest of the system use
// different wire strings for the same concept, and only the runtelemetry
// canonical string is recognised by dashboard.js. Unknown values fall
// back to ErrClassNone.
func mapSysessionErrorClass(c sysession.DaemonErrorClass) runtelemetry.ErrorClass {
	switch c {
	case sysession.DaemonErrorClassNone:
		return runtelemetry.ErrClassNone
	case sysession.DaemonErrorClassValidation:
		return runtelemetry.ErrClassSysessionValidation
	case sysession.DaemonErrorClassUpstream:
		return runtelemetry.ErrClassSysessionUpstream
	case sysession.DaemonErrorClassTimeout:
		return runtelemetry.ErrClassDeadlineExceeded
	case sysession.DaemonErrorClassPanic:
		return runtelemetry.ErrClassPanic
	case sysession.DaemonErrorClassCanceled:
		return runtelemetry.ErrClassCanceled
	default:
		return runtelemetry.ErrClassNone
	}
}
