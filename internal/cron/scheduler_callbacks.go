// scheduler_callbacks.go: cron-side run-event types + emit helpers +
// per-state metrics bumps.
//
// Phase D (RFC §3.5) collapsed three legacy SetOn* setters
// (SetOnExecute / SetOnRunStarted / SetOnRunEnded) and their
// atomic.Pointer storage into a single SchedulerConfig.Telemetry
// (runtelemetry.Broadcaster) injected at construction. The cron-local
// Run{Started,Ended}Event types are kept for two reasons:
//   - cron internals (executeOpt / finishRun / emitOverlapSkipped)
//     populate them with cron-specific fields (Trigger=cron.TriggerKind,
//     ErrorClass=cron.ErrorClass) before translating to the wire
//     runtelemetry.RunEndedEvent
//   - the emit helpers are private (lowercase), so external callers
//     reach the broadcast surface only through SchedulerConfig.Telemetry
//     or SetTelemetry
//
// No behaviour change vs the pre-Phase-D pipeline: per-state metrics
// still bump in finishRun, RunStarted still fires post-CAS pre-IO,
// RunEnded still fires after persistence settles.

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

// SetTelemetry installs (or replaces) the broadcaster late, after
// construction. Used by cmd/naozhi which builds Scheduler before the
// Hub exists, then injects the broadcaster once dashboard.go finishes
// wiring. Calling SetTelemetry is the only mutation path on s.telemetry
// after NewScheduler; production uses it exactly once during boot, so
// the plain assignment is uncontended.
//
// Passing nil clears the broadcaster (returns to no-broadcast mode).
func (s *Scheduler) SetTelemetry(b runtelemetry.Broadcaster) {
	s.telemetry = b
}

// emitRunStarted translates a cron-local RunStartedEvent to the shared
// runtelemetry shape and forwards through the configured broadcaster.
// nil broadcaster (tests / no-WS) is silently dropped — the metric bump
// happens unconditionally so dashboard counts stay accurate.
//
// R230C-GO-15: CronRunStartedTotal bumps here, not at the call sites,
// so the counter cannot drift from the broadcast event count when a
// new emit path lands.
func (s *Scheduler) emitRunStarted(ev RunStartedEvent) {
	metrics.CronRunStartedTotal.Add(1)
	if s.telemetry == nil {
		return
	}
	s.telemetry.BroadcastRunStarted(runtelemetry.RunStartedEvent{
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
	if s.telemetry == nil {
		return
	}
	s.telemetry.BroadcastRunEnded(runtelemetry.RunEndedEvent{
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
// transition. Mirrored in metrics.go and pinned by counter_wiring_contract_test.
func (s *Scheduler) bumpRunStateMetrics(state RunState) {
	switch state {
	case RunStateSucceeded:
		metrics.CronRunSucceededTotal.Add(1)
	case RunStateFailed:
		metrics.CronRunFailedTotal.Add(1)
	case RunStateSkipped:
		metrics.CronRunSkippedTotal.Add(1)
	case RunStateTimedOut:
		metrics.CronRunTimedOutTotal.Add(1)
	case RunStateCanceled:
		metrics.CronRunCanceledTotal.Add(1)
	}
}
