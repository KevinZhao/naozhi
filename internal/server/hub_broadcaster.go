package server

import "github.com/naozhi/naozhi/internal/runtelemetry"

// hubBroadcaster implements runtelemetry.Broadcaster against *Hub.
// Cron and sysession both register one at construction so their run lifecycle
// events fan out through a single seam (#1723). Dispatch is keyed on
// RunStartedEvent.Subsystem / RunEndedEvent.Subsystem, so a new producer is
// added by extending the switch. Refs: docs/rfc/cron-sysession-merge.md §3.5.4.
type hubBroadcaster struct{ h *Hub }

// newHubBroadcaster wraps a Hub for use as a runtelemetry.Broadcaster.
// Returns a value (not a pointer-to-pointer): the hub field captures
// once and is never reassigned.
func newHubBroadcaster(h *Hub) hubBroadcaster { return hubBroadcaster{h: h} }

func (b hubBroadcaster) BroadcastRunStarted(ev runtelemetry.RunStartedEvent) {
	if b.h == nil {
		return
	}
	switch ev.Subsystem {
	case runtelemetry.SubsystemCron:
		b.h.BroadcastCronRunStarted(ev.OwnerID, ev.RunID, ev.StartedAt,
			string(ev.Trigger), ev.SessionID, ev.Fresh)
	case runtelemetry.SubsystemSysession:
		b.h.BroadcastDaemonRunStarted(ev.OwnerID, ev.RunID,
			string(ev.Trigger), ev.StartedAt)
	}
}

func (b hubBroadcaster) BroadcastRunEnded(ev runtelemetry.RunEndedEvent) {
	if b.h == nil {
		return
	}
	switch ev.Subsystem {
	case runtelemetry.SubsystemCron:
		b.h.BroadcastCronRunEnded(ev.OwnerID, ev.RunID, string(ev.State),
			ev.StartedAt, ev.EndedAt, ev.DurationMS, ev.SessionID,
			string(ev.ErrorClass), ev.ErrorMsg, string(ev.Trigger))
	case runtelemetry.SubsystemSysession:
		// SECURITY: ErrorMsg deliberately dropped for sysession
		// (docs/rfc/system-session.md §9.4) — daemon errors can echo prompt
		// fragments, and broadcasting them to every authenticated dashboard
		// client is cross-tenant leakage. cron emits ErrorMsg post-redact.
		b.h.BroadcastDaemonRunEnded(ev.OwnerID, ev.RunID, string(ev.State),
			string(ev.ErrorClass), string(ev.Trigger), ev.DurationMS)
	}
}
