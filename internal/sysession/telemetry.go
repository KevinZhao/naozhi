// telemetry.go: run-event emit helpers + sysession→runtelemetry enum
// translation. Manager emits runtelemetry events directly on the same
// Broadcaster seam cron uses (#1723); the broadcaster selects the daemon_run_*
// WS payload off Subsystem=SubsystemSysession and drops ErrorMsg before
// serialising (RFC §9.4).

package sysession

import (
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// SetTelemetry installs (or replaces) the broadcaster after construction; the
// server package injects it once dashboard wiring finishes. Storage is
// atomic.Pointer because SetTelemetry can race with tick goroutines already
// calling emitRun*, so the read path must be lock-free. nil clears it (also
// the default) and emit* becomes a silent no-op.
func (m *Manager) SetTelemetry(b runtelemetry.Broadcaster) {
	if b == nil {
		m.telemetry.Store(nil)
		return
	}
	bb := b
	m.telemetry.Store(&bb)
}

// loadTelemetry returns the current broadcaster or nil (atomic.Pointer wraps
// a *Broadcaster; a nil deref would panic). Lock-free.
func (m *Manager) loadTelemetry() runtelemetry.Broadcaster {
	ptr := m.telemetry.Load()
	if ptr == nil {
		return nil
	}
	return *ptr
}

// emitRunStarted broadcasts a run-started event tagged SubsystemSysession. The
// metric bump happens unconditionally so the counter cannot drift from the
// broadcast path. Fired post-CAS, pre-IO from runOnce, outside any lock.
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

// emitRunEnded broadcasts a terminal run event tagged SubsystemSysession.
// ErrorMsg is deliberately NOT forwarded so the no-leak invariant (RFC §9.4)
// is local to the producer. Metric bump is unconditional; fired from
// recordRun outside any lock.
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

// The sysession.Daemon* and runtelemetry.* enums do NOT line up 1:1 on the
// wire: a bare cast such as runtelemetry.ErrorClass(c) would leak sysession's
// "timeout" (no runtelemetry constant) verbatim to WS, where dashboard.js only
// recognises "deadline_exceeded". The explicit maps below default unknown
// inputs to a safe constant instead of minting an undefined enum.

// mapSysessionTrigger translates a sysession trigger; unknown values fall
// back to TriggerScheduled.
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

// mapSysessionRunState translates a terminal run state; unknown values
// surface as RunStateFailed.
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

// mapSysessionErrorClass translates an error class, normalising "timeout" →
// ErrClassDeadlineExceeded (the only string dashboard.js recognises); unknown
// values fall back to ErrClassNone.
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
