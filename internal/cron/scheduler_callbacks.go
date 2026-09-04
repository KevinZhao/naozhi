// scheduler_callbacks.go: cron-side run-event types + emit helpers +
// per-state metrics bumps. The cron-local Run{Started,Ended}Event types carry
// cron-specific fields (Trigger=cron.TriggerKind, ErrorClass=cron.ErrorClass)
// and are translated to the wire runtelemetry shapes inside the private emit
// helpers, so external callers reach the broadcast surface only through
// SchedulerDeps.Telemetry or SetTelemetry (RFC §3.5).

package cron

import (
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// RunStartedEvent is the cron-local payload for run-started. Translated
// to runtelemetry.RunStartedEvent inside emitRunStarted before reaching
// the broadcaster.
type RunStartedEvent struct {
	JobID     string
	RunID     string
	StartedAt time.Time
	Trigger   TriggerKind
	SessionID string // 可能为空：CAS 之后立刻广播时 GetOrCreate 还没跑
	Fresh     bool
}

// RunEndedEvent is the cron-local payload for run-ended. Translated to
// runtelemetry.RunEndedEvent inside emitRunEnded.
type RunEndedEvent struct {
	JobID      string
	RunID      string
	State      RunState
	StartedAt  time.Time
	EndedAt    time.Time
	DurationMS int64
	SessionID  string
	ErrorClass ErrorClass
	ErrorMsg   string
	Trigger    TriggerKind
}

// SetTelemetry installs (or replaces) the broadcaster after construction:
// cmd/naozhi builds the Scheduler before the Hub exists and injects once
// dashboard.go finishes wiring. Storage is atomic.Pointer because wiring
// goroutines can call this while cron tick goroutines are already inside
// emitRunStarted / emitRunEnded. Passing nil clears the broadcaster.
func (s *Scheduler) SetTelemetry(b runtelemetry.Broadcaster) {
	if b == nil {
		s.telemetry.Store(nil)
		return
	}
	bb := b
	s.telemetry.Store(&bb)
}

// loadTelemetry returns the current broadcaster or nil. Centralised so
// the deref dance (atomic.Pointer wraps a *Broadcaster, dereferencing
// can panic on nil) lives in one place.
func (s *Scheduler) loadTelemetry() runtelemetry.Broadcaster {
	ptr := s.telemetry.Load()
	if ptr == nil {
		return nil
	}
	return *ptr
}

// emitRunStarted translates a cron-local RunStartedEvent to the shared
// runtelemetry shape and forwards through the configured broadcaster; a nil
// broadcaster (tests / no-WS) is silently dropped. CronRunStartedTotal bumps
// here, not at call sites, so the counter cannot drift from the broadcast
// event count when a new emit path lands.
func (s *Scheduler) emitRunStarted(ev RunStartedEvent) {
	metrics.CronRunStartedTotal.Add(1)
	b := s.loadTelemetry()
	if b == nil {
		return
	}
	b.BroadcastRunStarted(runtelemetry.RunStartedEvent{
		Subsystem: runtelemetry.SubsystemCron,
		OwnerID:   ev.JobID,
		RunID:     ev.RunID,
		Trigger:   runtelemetry.TriggerKind(ev.Trigger),
		StartedAt: ev.StartedAt,
		SessionID: ev.SessionID,
		Fresh:     ev.Fresh,
	})
}

func (s *Scheduler) emitRunEnded(ev RunEndedEvent) {
	b := s.loadTelemetry()
	if b == nil {
		return
	}
	b.BroadcastRunEnded(runtelemetry.RunEndedEvent{
		Subsystem:  runtelemetry.SubsystemCron,
		OwnerID:    ev.JobID,
		RunID:      ev.RunID,
		State:      runtelemetry.RunState(ev.State),
		StartedAt:  ev.StartedAt,
		EndedAt:    ev.EndedAt,
		DurationMS: ev.DurationMS,
		Trigger:    runtelemetry.TriggerKind(ev.Trigger),
		SessionID:  ev.SessionID,
		ErrorClass: runtelemetry.ErrorClass(ev.ErrorClass),
		ErrorMsg:   ev.ErrorMsg,
	})
}

// bumpRunStateMetrics increments the per-state counter for the terminal
// transition. It is the SINGLE owner of every per-state counter — the generic
// CronRun<State>Total family AND the sandbox-specific CronSandboxRun*Total
// pair (gated by sandbox, i.e. finishArgs.sandbox). Callers must never bump
// these counters directly; run_metrics_owner_contract_test.go pins that.
// Owning the whole state→counter mapping here makes double-count / missing-
// counter drift structurally impossible (#2173).
func (s *Scheduler) bumpRunStateMetrics(state RunState, sandbox bool) {
	switch state {
	case RunStateSucceeded:
		metrics.CronRunSucceededTotal.Add(1)
	case RunStateFailed:
		metrics.CronRunFailedTotal.Add(1)
		if sandbox {
			metrics.CronSandboxRunFailedTotal.Add(1)
		}
	case RunStateSkipped:
		metrics.CronRunSkippedTotal.Add(1)
	case RunStateTimedOut:
		metrics.CronRunTimedOutTotal.Add(1)
		if sandbox {
			// Dedicated bucket so failure-only alerts still see sandbox
			// deadlines without double-counting into CronSandboxRunFailedTotal (#2091).
			metrics.CronSandboxRunTimedOutTotal.Add(1)
		}
	case RunStateCanceled:
		metrics.CronRunCanceledTotal.Add(1)
	}
}
