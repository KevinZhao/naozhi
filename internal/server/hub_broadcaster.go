package server

import "github.com/naozhi/naozhi/internal/runtelemetry"

// hubBroadcaster implements runtelemetry.Broadcaster against *Hub.
// Cron and sysession both register one at construction so their run lifecycle
// events fan out through a single seam (#1723). It used to translate each
// event into a per-subsystem frame via a Subsystem switch; with the unified
// run_started / run_ended frames (#2540) the Hub encodes the event directly,
// and the only per-subsystem decision left is the wire policy below.
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
	b.h.BroadcastRunStarted(ev)
}

func (b hubBroadcaster) BroadcastRunEnded(ev runtelemetry.RunEndedEvent) {
	if b.h == nil {
		return
	}
	// SECURITY: ErrorMsg deliberately dropped for sysession
	// (docs/rfc/system-session.md §9.4) — daemon errors can echo prompt
	// fragments, and broadcasting them to every authenticated dashboard
	// client is cross-tenant leakage. cron emits ErrorMsg post-redact.
	if ev.Subsystem == runtelemetry.SubsystemSysession {
		ev.ErrorMsg = ""
	}
	b.h.BroadcastRunEnded(ev)
}
