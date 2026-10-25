// run_telemetry.go builds the run-event relays cron, sysession, the dashboard
// Hub and outbound webhooks share (docs/rfc/outbound-webhooks.md §3.3).

package wireup

import (
	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/webhook"
)

// RunTelemetryRelays returns the relay cron and sysession are built with
// (producers) and the one the server binds the dashboard Hub to (hub); with
// no sender they are one. The webhook is bound up front because the schedulers
// start before the server and an unbound Relay drops events (#3691).
func RunTelemetryRelays(webhooks *webhook.Sender) (producers, hub *runtelemetry.Relay) {
	hub = &runtelemetry.Relay{}
	if webhooks == nil {
		return hub, hub
	}
	producers = &runtelemetry.Relay{}
	producers.Bind(runtelemetry.Tee(hub, webhookBroadcaster{s: webhooks}))
	return producers, hub
}

// webhookBroadcaster adapts a webhook.Sender to the run-event Broadcaster.
// ErrorMsg is not copied (RunEndedEvent's SECURITY note).
type webhookBroadcaster struct{ s *webhook.Sender }

func (b webhookBroadcaster) BroadcastRunStarted(ev runtelemetry.RunStartedEvent) {
	b.s.Deliver(webhook.Event{
		Type: webhook.EventRunStarted, Subsystem: string(ev.Subsystem),
		OwnerID: ev.OwnerID, RunID: ev.RunID, Trigger: string(ev.Trigger),
		SessionID: ev.SessionID, StartedAt: ev.StartedAt, Node: b.s.Node(),
	})
}

func (b webhookBroadcaster) BroadcastRunEnded(ev runtelemetry.RunEndedEvent) {
	b.s.Deliver(webhook.Event{
		Type: webhook.EventRunEnded, Subsystem: string(ev.Subsystem),
		OwnerID: ev.OwnerID, RunID: ev.RunID, State: string(ev.State), Trigger: string(ev.Trigger),
		ErrorClass: string(ev.ErrorClass), SessionID: ev.SessionID,
		StartedAt: ev.StartedAt, EndedAt: ev.EndedAt, DurationMS: ev.DurationMS, Node: b.s.Node(),
	})
}
