package server

import "github.com/naozhi/naozhi/internal/runtelemetry"

// hubBroadcaster implements runtelemetry.Broadcaster against *wsBroadcaster.
// Cron and sysession both register one at construction so their run lifecycle
// events fan out through a single seam (#1723). It used to translate each
// event into a per-subsystem frame via a Subsystem switch; with the unified
// run_started / run_ended frames (#2540) the Hub encodes the event directly,
// and the only per-subsystem decision left is the wire policy below.
// Refs: docs/rfc/cron-sysession-merge.md §3.5.4.
type hubBroadcaster struct{ b *wsBroadcaster }

// newHubBroadcaster wraps the broadcaster for use as a runtelemetry.Broadcaster.
// Returns a value (not a pointer-to-pointer): the field captures once and is
// never reassigned.
func newHubBroadcaster(b *wsBroadcaster) hubBroadcaster { return hubBroadcaster{b: b} }

func (hb hubBroadcaster) BroadcastRunStarted(ev runtelemetry.RunStartedEvent) {
	if hb.b == nil {
		return
	}
	hb.b.BroadcastRunStarted(ev)
}

func (hb hubBroadcaster) BroadcastRunEnded(ev runtelemetry.RunEndedEvent) {
	if hb.b == nil {
		return
	}
	// SECURITY: ErrorMsg deliberately dropped for sysession
	// (docs/rfc/system-session.md §9.4) — daemon errors can echo prompt
	// fragments, and broadcasting them to every authenticated dashboard
	// client is cross-tenant leakage. cron emits ErrorMsg post-redact.
	if ev.Subsystem == runtelemetry.SubsystemSysession {
		ev.ErrorMsg = ""
	}
	hb.b.BroadcastRunEnded(ev)
}
