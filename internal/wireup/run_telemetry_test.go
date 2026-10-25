package wireup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/webhook"
)

// webhookReceiver collects the events a test endpoint was sent.
type webhookReceiver struct {
	mu  sync.Mutex
	got []webhook.Event
}

func (r *webhookReceiver) handler(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	var ev webhook.Event
	_ = json.Unmarshal(b, &ev)
	r.mu.Lock()
	r.got = append(r.got, ev)
	r.mu.Unlock()
}

func (r *webhookReceiver) events() []webhook.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]webhook.Event(nil), r.got...)
}

// hubRecorder stands in for the dashboard Hub's broadcaster.
type hubRecorder struct {
	mu    sync.Mutex
	ended []runtelemetry.RunEndedEvent
}

func (h *hubRecorder) BroadcastRunStarted(runtelemetry.RunStartedEvent) {}

func (h *hubRecorder) BroadcastRunEnded(ev runtelemetry.RunEndedEvent) {
	h.mu.Lock()
	h.ended = append(h.ended, ev)
	h.mu.Unlock()
}

func closeSender(t *testing.T, s *webhook.Sender) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Close(ctx)
}

// A run event the producers emit before the server exists — the run.ended
// cron's startup reconcile writes for a run the previous process left in
// flight — still reaches the webhook; the Hub, bound later, sees only what
// follows its bind (#3691).
func TestRunTelemetryRelays_WebhookSeesEventsBeforeHubBind(t *testing.T) {
	rcv := &webhookReceiver{}
	rs := httptest.NewServer(http.HandlerFunc(rcv.handler))
	defer rs.Close()
	sender := webhook.New([]webhook.Endpoint{{URL: rs.URL}})
	producers, hub := RunTelemetryRelays(sender)
	if producers == hub {
		t.Fatal("with a sender the producers' relay must not be the Hub's")
	}

	producers.BroadcastRunEnded(runtelemetry.RunEndedEvent{Subsystem: runtelemetry.SubsystemCron, OwnerID: "job", RunID: "restarted", State: runtelemetry.RunStateCanceled, ErrorClass: runtelemetry.ErrClassCronInterrupted})
	h := &hubRecorder{}
	hub.Bind(h)
	producers.BroadcastRunEnded(runtelemetry.RunEndedEvent{Subsystem: runtelemetry.SubsystemCron, OwnerID: "job", RunID: "live", State: runtelemetry.RunStateSucceeded})
	closeSender(t, sender)

	got := rcv.events()
	if len(got) != 2 || got[0].RunID != "restarted" || got[0].State != "canceled" || got[1].RunID != "live" {
		t.Fatalf("webhook events = %+v, want restarted then live", got)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.ended) != 1 || h.ended[0].RunID != "live" {
		t.Fatalf("hub events = %+v, want only live", h.ended)
	}
}

// Without a sender there is one relay, bound by the server as before.
func TestRunTelemetryRelays_NilSenderIsOneRelay(t *testing.T) {
	producers, hub := RunTelemetryRelays(nil)
	if producers != hub {
		t.Fatal("no sender: producers and Hub must share the relay")
	}
	hub.Bind(&hubRecorder{}) // unbound until the server binds it; would panic if bound
}

// The webhook gets run metadata only (no error text), stamped with the node.
func TestWebhookBroadcaster_MapsRunEvents(t *testing.T) {
	rcv := &webhookReceiver{}
	rs := httptest.NewServer(http.HandlerFunc(rcv.handler))
	defer rs.Close()
	sender := webhook.New([]webhook.Endpoint{{URL: rs.URL}}, webhook.WithNode("node-a"))
	producers, _ := RunTelemetryRelays(sender)

	start := time.Now()
	producers.BroadcastRunStarted(runtelemetry.RunStartedEvent{Subsystem: runtelemetry.SubsystemCron, OwnerID: "0123456789abcdef", RunID: "fedcba9876543210", StartedAt: start})
	producers.BroadcastRunEnded(runtelemetry.RunEndedEvent{Subsystem: runtelemetry.SubsystemCron, OwnerID: "0123456789abcdef", RunID: "fedcba9876543210", State: runtelemetry.RunStateFailed, ErrorClass: "turn_failed", ErrorMsg: "secret prompt text", StartedAt: start, EndedAt: start.Add(time.Second), DurationMS: 1000})
	closeSender(t, sender)

	got := rcv.events()
	if len(got) != 2 {
		t.Fatalf("events = %d", len(got))
	}
	if got[0].Type != webhook.EventRunStarted || got[0].Node != "node-a" || got[0].Subsystem != "cron" || !got[0].StartedAt.Equal(start) {
		t.Fatalf("started = %+v", got[0])
	}
	if got[1].Type != webhook.EventRunEnded || got[1].State != "failed" || got[1].ErrorClass != "turn_failed" || got[1].DurationMS != 1000 || !got[1].EndedAt.Equal(start.Add(time.Second)) {
		t.Fatalf("ended = %+v", got[1])
	}
}
