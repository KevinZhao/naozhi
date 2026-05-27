package server

import "github.com/naozhi/naozhi/internal/runtelemetry"

// hubBroadcaster implements runtelemetry.Broadcaster against *Hub.
// Cron and sysession both register one of these at construction so
// their run lifecycle events fan out through a single seam, replacing
// the legacy SetOnExecute / SetOnRunStarted / SetOnRunEnded trio for
// cron and the per-Manager SetCallbacks pair for sysession.
//
// The dispatch is keyed on RunStartedEvent.Subsystem /
// RunEndedEvent.Subsystem so a future producer (planner, system) can
// be added by extending the switch without touching cron / sysession.
//
// Refs: docs/rfc/cron-sysession-merge.md §3.5.4.
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
		// SECURITY: ErrorMsg deliberately dropped on the wire for sysession
		// per docs/rfc/system-session.md §9.4 — daemon errors can echo
		// prompt fragments back from the LLM subprocess and broadcasting
		// that to every authenticated dashboard client constitutes
		// cross-tenant leakage. cron emits ErrorMsg post-redact.
		b.h.BroadcastDaemonRunEnded(ev.OwnerID, ev.RunID, string(ev.State),
			string(ev.ErrorClass), string(ev.Trigger), ev.DurationMS)
	}
}
