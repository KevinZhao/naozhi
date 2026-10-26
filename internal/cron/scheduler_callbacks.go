// scheduler_callbacks.go: cron-side run-event types + emit helpers +
// per-state metrics bumps. The cron-local Run{Started,Ended}Event types use
// cron's aliases of the runtelemetry enums, so the private emit helpers copy
// them into the wire runtelemetry shapes field for field; external callers
// reach the broadcast surface only through SchedulerDeps.Telemetry (RFC §3.5).

package cron

import (
	"time"

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

// emitRunStarted translates a cron-local RunStartedEvent to the shared
// runtelemetry shape and forwards through the configured broadcaster; a nil
// broadcaster (tests / no-WS) is silently dropped.
func (s *Scheduler) emitRunStarted(ev RunStartedEvent) {
	b := s.telemetry
	if b == nil {
		return
	}
	b.BroadcastRunStarted(runtelemetry.RunStartedEvent{
		Subsystem: runtelemetry.SubsystemCron,
		OwnerID:   ev.JobID,
		RunID:     ev.RunID,
		Trigger:   ev.Trigger,
		StartedAt: ev.StartedAt,
		SessionID: ev.SessionID,
		Fresh:     ev.Fresh,
	})
}

func (s *Scheduler) emitRunEnded(ev RunEndedEvent) {
	b := s.telemetry
	if b == nil {
		return
	}
	b.BroadcastRunEnded(runtelemetry.RunEndedEvent{
		Subsystem:  runtelemetry.SubsystemCron,
		OwnerID:    ev.JobID,
		RunID:      ev.RunID,
		State:      ev.State,
		StartedAt:  ev.StartedAt,
		EndedAt:    ev.EndedAt,
		DurationMS: ev.DurationMS,
		Trigger:    ev.Trigger,
		SessionID:  ev.SessionID,
		ErrorClass: ev.ErrorClass,
		ErrorMsg:   ev.ErrorMsg,
	})
}
