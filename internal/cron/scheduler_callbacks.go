// scheduler_callbacks.go: cron run-event emit helpers + per-state metrics
// bumps. Call sites build runtelemetry.Run{Started,Ended}Event directly; the
// emit helpers stamp SubsystemCron and forward. External callers reach the
// broadcast surface only through SchedulerDeps.Telemetry (RFC §3.5).

package cron

import (
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// emitRunStarted stamps SubsystemCron on ev and forwards it through the
// configured broadcaster; a nil broadcaster (tests / no-WS) is silently
// dropped. CronRunStartedTotal bumps here, not at call sites, so the counter
// cannot drift from the broadcast event count when a new emit path lands.
func (s *Scheduler) emitRunStarted(ev runtelemetry.RunStartedEvent) {
	metrics.CronRunStartedTotal.Add(1)
	b := s.telemetry
	if b == nil {
		return
	}
	ev.Subsystem = runtelemetry.SubsystemCron
	b.BroadcastRunStarted(ev)
}

func (s *Scheduler) emitRunEnded(ev runtelemetry.RunEndedEvent) {
	b := s.telemetry
	if b == nil {
		return
	}
	ev.Subsystem = runtelemetry.SubsystemCron
	b.BroadcastRunEnded(ev)
}

// bumpRunStateMetrics increments the per-state counter for the terminal
// transition. It is the SINGLE owner of every per-state counter — the
// CronRun<State>Total family. Callers must never bump
// these counters directly; run_metrics_owner_contract_test.go pins that.
// Owning the whole state→counter mapping here makes double-count / missing-
// counter drift structurally impossible (#2173).
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
