// telemetry.go: sysession-side run-event emit helpers + sysession→runtelemetry
// enum translation.
//
// #1723 Phase 1 converged sysession's run-lifecycle broadcast onto the same
// runtelemetry.Broadcaster seam cron already uses (see
// internal/cron/scheduler_callbacks.go for the sibling pattern). The previous
// design had Manager hold a pair of atomic.Pointer[onRun*Holder] callback
// fields that the server package installed via SetCallbacks, translating the
// sysession.DaemonRun*Event mirror structs into runtelemetry events inside an
// inline shim in routes.go. That shim — and the enum maps it depended on
// (formerly server.mapSysession*) — now live here so Manager produces
// runtelemetry events directly.
//
// No wire behaviour change: the broadcaster (server.hubBroadcaster) still
// selects the daemon_run_* WS payload off Subsystem=SubsystemSysession and
// still drops ErrorMsg before serialising (RFC §9.4 / Sec-LOW-2). The enum
// maps below are copied verbatim from the pre-#1723 server package so the
// timeout→deadline_exceeded normalisation and every other mapping land
// byte-for-byte on the wire as before.

package sysession

import (
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// SetTelemetry installs (or replaces) the broadcaster late, after
// construction. Used by the server package which builds the Hub after the
// Manager, then injects the broadcaster once dashboard wiring finishes.
//
// Storage is atomic.Pointer[runtelemetry.Broadcaster]: SetTelemetry can fire
// from a wiring goroutine while daemon tick goroutines are already invoking
// emitRunStarted / emitRunEnded, so the read path must be lock-free and
// race-free. Mirrors cron.Scheduler.SetTelemetry (R20260527-GO-1).
//
// Passing nil clears the broadcaster (returns to no-broadcast mode), which is
// also the default before SetTelemetry is ever called — tests and no-WS
// deployments run with a nil broadcaster and emit* is a silent no-op.
func (m *Manager) SetTelemetry(b runtelemetry.Broadcaster) {
	if b == nil {
		m.telemetry.Store(nil)
		return
	}
	bb := b
	m.telemetry.Store(&bb)
}

// loadTelemetry returns the current broadcaster or nil. Centralised so the
// deref dance (atomic.Pointer wraps a *Broadcaster; dereferencing a nil
// pointer panics) lives in one place. Lock-free; safe from any goroutine.
func (m *Manager) loadTelemetry() runtelemetry.Broadcaster {
	ptr := m.telemetry.Load()
	if ptr == nil {
		return nil
	}
	return *ptr
}

// emitRunStarted broadcasts a run-started event through the configured
// broadcaster, tagged Subsystem=SubsystemSysession. nil broadcaster (tests /
// no-WS) is silently dropped — the metric bump happens unconditionally so the
// counter cannot drift from the broadcast path (cron R230C-GO-15). Fired
// post-CAS, pre-IO from runOnce, outside any Manager-internal lock.
func (m *Manager) emitRunStarted(name, runID string, trigger DaemonTriggerKind, startedAt time.Time) {
	metrics.SysessionRunStartedTotal.Add(1)
	b := m.loadTelemetry()
	if b == nil {
		return
	}
	b.BroadcastRunStarted(runtelemetry.RunStartedEvent{
		Subsystem: runtelemetry.SubsystemSysession,
		OwnerID:   name,
		RunID:     runID,
		Trigger:   mapSysessionTrigger(trigger),
		StartedAt: startedAt,
	})
}

// emitRunEnded broadcasts a terminal run event through the configured
// broadcaster, tagged Subsystem=SubsystemSysession. ErrorMsg is deliberately
// NOT forwarded — the broadcaster drops it anyway (RFC §9.4 / Sec-LOW-2), and
// keeping it off the producer side too makes the no-leak invariant local.
// nil broadcaster is silently dropped — the metric bump happens
// unconditionally so the counter cannot drift from the broadcast path (cron
// R230C-GO-15). Fired from recordRun outside any lock.
func (m *Manager) emitRunEnded(name, runID string, state DaemonRunState, durationMS int64, errorClass DaemonErrorClass, trigger DaemonTriggerKind) {
	metrics.SysessionRunEndedTotal.Add(1)
	b := m.loadTelemetry()
	if b == nil {
		return
	}
	b.BroadcastRunEnded(runtelemetry.RunEndedEvent{
		Subsystem:  runtelemetry.SubsystemSysession,
		OwnerID:    name,
		RunID:      runID,
		State:      mapSysessionRunState(state),
		DurationMS: durationMS,
		Trigger:    mapSysessionTrigger(trigger),
		ErrorClass: mapSysessionErrorClass(errorClass),
	})
}

// sysession→runtelemetry enum translation.
//
// The two enum vocabularies (sysession.Daemon* aliases vs runtelemetry.*) do
// NOT line up 1:1 on the wire. A bare conversion such as
// runtelemetry.ErrorClass(c) silently produces an invalid enum value where the
// wire strings diverge — concretely sysession's DaemonErrorClassTimeout =
// "timeout" has no matching runtelemetry constant (the canonical timeout class
// is ErrClassDeadlineExceeded = "deadline_exceeded", which is the string
// dashboard.js's cronErrorClassLabel keys off to render "超时"). A bare cast
// would leak the raw "timeout" string to the WS wire, which the dashboard then
// renders verbatim instead of the friendly label.
//
// These explicit maps make every cross-package value a deliberate, reviewable
// decision and default unknown inputs to a safe runtelemetry constant rather
// than minting an undefined enum.

// mapSysessionTrigger translates a sysession trigger to its runtelemetry
// equivalent. Unknown values fall back to TriggerScheduled (the only
// production-emitted value today).
func mapSysessionTrigger(t DaemonTriggerKind) runtelemetry.TriggerKind {
	switch t {
	case DaemonTriggerScheduled:
		return runtelemetry.TriggerScheduled
	case DaemonTriggerManual:
		return runtelemetry.TriggerManual
	default:
		return runtelemetry.TriggerScheduled
	}
}

// mapSysessionRunState translates a terminal sysession run state to its
// runtelemetry equivalent. Unknown values fall back to RunStateFailed so an
// unexpected state is surfaced as a failure rather than a silently invalid
// enum.
func mapSysessionRunState(s DaemonRunState) runtelemetry.RunState {
	switch s {
	case DaemonRunSucceeded:
		return runtelemetry.RunStateSucceeded
	case DaemonRunFailed:
		return runtelemetry.RunStateFailed
	case DaemonRunTimedOut:
		return runtelemetry.RunStateTimedOut
	case DaemonRunCanceled:
		return runtelemetry.RunStateCanceled
	default:
		return runtelemetry.RunStateFailed
	}
}

// mapSysessionErrorClass translates a sysession error class to its
// runtelemetry equivalent. The notable normalisation is DaemonErrorClassTimeout
// ("timeout") → ErrClassDeadlineExceeded ("deadline_exceeded"): sysession and
// the rest of the system use different wire strings for the same concept, and
// only the runtelemetry canonical string is recognised by dashboard.js.
// Unknown values fall back to ErrClassNone.
func mapSysessionErrorClass(c DaemonErrorClass) runtelemetry.ErrorClass {
	switch c {
	case DaemonErrorClassNone:
		return runtelemetry.ErrClassNone
	case DaemonErrorClassValidation:
		return runtelemetry.ErrClassSysessionValidation
	case DaemonErrorClassUpstream:
		return runtelemetry.ErrClassSysessionUpstream
	case DaemonErrorClassTimeout:
		return runtelemetry.ErrClassDeadlineExceeded
	case DaemonErrorClassPanic:
		return runtelemetry.ErrClassPanic
	case DaemonErrorClassCanceled:
		return runtelemetry.ErrClassCanceled
	default:
		return runtelemetry.ErrClassNone
	}
}
