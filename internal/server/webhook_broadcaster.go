package server

import (
	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/webhook"
)

// newWebhookBroadcaster adapts a webhook.Sender to the run-event Broadcaster.
// nil in → nil out, so runtelemetry.Tee collapses to the Hub alone when no
// webhook is set. ErrorMsg is not copied (RunEndedEvent's SECURITY note).
func newWebhookBroadcaster(s *webhook.Sender) runtelemetry.Broadcaster {
	if s == nil {
		return nil
	}
	return webhookBroadcaster{s: s}
}

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
