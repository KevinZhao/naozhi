package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/runtelemetry"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/webhook"
)

// A run event the scheduler relay emits reaches a configured webhook, with
// metadata only (no error text), while the Hub still receives it.
func TestWebhookBroadcaster_ReceivesRunEvents(t *testing.T) {
	var mu sync.Mutex
	var got []webhook.Event
	rs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var ev webhook.Event
		_ = json.Unmarshal(b, &ev)
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}))
	defer rs.Close()

	sender := webhook.New([]webhook.Endpoint{{URL: rs.URL}}, webhook.WithNode("node-a"))
	relay := &runtelemetry.Relay{}
	srv := NewWithOptions(ServerOptions{
		Addr: ":0", Router: session.NewRouter(session.RouterConfig{}),
		Platforms: map[string]platform.Platform{"test": &mockPlatform{}}, Backend: "claude",
		Relays: RelayOptions{RunTelemetry: relay, Webhooks: sender},
	})
	t.Cleanup(func() { srv.hub.Shutdown(); srv.appCancel() })

	relay.BroadcastRunStarted(runtelemetry.RunStartedEvent{Subsystem: runtelemetry.SubsystemCron, OwnerID: "0123456789abcdef", RunID: "fedcba9876543210", StartedAt: time.Now()})
	relay.BroadcastRunEnded(runtelemetry.RunEndedEvent{Subsystem: runtelemetry.SubsystemCron, OwnerID: "0123456789abcdef", RunID: "fedcba9876543210", State: runtelemetry.RunStateFailed, ErrorClass: "turn_failed", ErrorMsg: "secret prompt text"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sender.Close(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("events = %d", len(got))
	}
	if got[0].Type != webhook.EventRunStarted || got[0].Node != "node-a" || got[0].Subsystem != "cron" {
		t.Fatalf("started = %+v", got[0])
	}
	if got[1].Type != webhook.EventRunEnded || got[1].State != "failed" || got[1].ErrorClass != "turn_failed" {
		t.Fatalf("ended = %+v", got[1])
	}
}

// Without a sender the relay is bound to the Hub broadcaster alone, as before.
func TestWebhookBroadcaster_NilLeavesHubOnly(t *testing.T) {
	if newWebhookBroadcaster(nil) != nil {
		t.Fatal("nil sender must yield a nil broadcaster so Tee collapses")
	}
}
