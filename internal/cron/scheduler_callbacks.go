// scheduler_callbacks.go: cron run-event emit helpers. Call sites build runtelemetry.Run{Started,Ended}Event directly; the
// emit helpers stamp SubsystemCron and forward. External callers reach the
// broadcast surface only through SchedulerDeps.Telemetry (RFC §3.5).

package cron

import "github.com/naozhi/naozhi/internal/runtelemetry"

// emitRunStarted stamps SubsystemCron on ev and forwards it through the
// configured broadcaster; a nil broadcaster (tests / no-WS) is silently
// dropped.
func (s *Scheduler) emitRunStarted(ev runtelemetry.RunStartedEvent) {
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
