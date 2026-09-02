package node

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// relayResubServer is a fake remote primary for the same-client re-subscribe
// scenarios: it speaks the relay auth handshake, counts `subscribe` frames,
// serves /api/sessions/events (sendHistoryToClient's HTTP fallback) and lets
// the test inject arbitrary frames toward the relay.
type relayResubServer struct {
	srv          *httptest.Server
	subscribes   atomic.Int32
	unsubscribes atomic.Int32
	history      atomic.Int32
	connMu       sync.Mutex
	conn         *websocket.Conn
	connReady    chan struct{}
}

func newRelayResubServer(t *testing.T) *relayResubServer {
	t.Helper()
	s := &relayResubServer{connReady: make(chan struct{})}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sessions/events" {
			s.history.Add(1)
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		if r.URL.Path != "/ws" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if !authHandshake(t, conn) {
			return
		}
		s.connMu.Lock()
		s.conn = conn
		s.connMu.Unlock()
		close(s.connReady)
		for {
			var msg ClientMsg
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case "subscribe":
				s.subscribes.Add(1)
			case "unsubscribe":
				s.unsubscribes.Add(1)
			}
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// inject writes a raw frame from the fake remote toward the relay.
func (s *relayResubServer) inject(t *testing.T, v any) {
	t.Helper()
	select {
	case <-s.connReady:
	case <-time.After(3 * time.Second):
		t.Fatal("relay never connected to fake remote")
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if err := s.conn.WriteJSON(v); err != nil {
		t.Fatalf("inject: %v", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func relaySubCount(r *wsRelay, key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subs[key])
}

// TestWSRelay_Subscribe_SameClientTwice_NoDuplicateSink pins the idempotency
// half of the #2421 review finding F1: a client that re-subscribes to a key it
// already holds must not be appended a second time. Before the fix r.subs[key]
// grew to two copies of the same sink and every relayed frame was fanned out
// twice to that one browser.
func TestWSRelay_Subscribe_SameClientTwice_NoDuplicateSink(t *testing.T) {
	fake := newRelayResubServer(t)
	relay := newWSRelay(newRelayNode(fake.srv))
	defer relay.Close()

	const key = "feishu:group:resub"
	sink := &mockSink{id: 1}
	relay.Subscribe(sink, key, 0)
	waitFor(t, "first subscribe frame", func() bool { return fake.subscribes.Load() == 1 })

	relay.Subscribe(sink, key, 0)

	if n := relaySubCount(relay, key); n != 1 {
		t.Fatalf("r.subs[%q] has %d entries after same-client re-subscribe, want 1", key, n)
	}
}

// TestWSRelay_Resubscribe_AfterRemoteTimeout_ResendsSubscribe pins the
// self-healing half of F1. The remote primary's resubscribeEvents gives up
// after 60s, drops the relay's subscription and pushes
// session_state{reason:"subscription_timeout"}; the relay forwards it, the
// dashboard clears its bookkeeping and re-subscribes on the next running
// broadcast. Before the fix that re-subscribe hit the alreadySubscribed branch
// (the relay still believed the remote subscription was live), replied with
// one HTTP history page and never rebuilt the remote WS subscription — every
// later event for the session was lost.
func TestWSRelay_Resubscribe_AfterRemoteTimeout_ResendsSubscribe(t *testing.T) {
	fake := newRelayResubServer(t)
	relay := newWSRelay(newRelayNode(fake.srv))
	defer relay.Close()

	const key = "feishu:group:timeout"
	sink := &mockSink{id: 1}
	relay.Subscribe(sink, key, 0)
	waitFor(t, "first subscribe frame", func() bool { return fake.subscribes.Load() == 1 })

	// Remote dropped our subscription and told us so.
	fake.inject(t, ServerMsg{Type: "session_state", Key: key, State: "ready", Reason: "subscription_timeout"})
	waitFor(t, "timeout frame forwarded to sink", func() bool {
		for _, raw := range sink.RawMsgs() {
			if strings.Contains(string(raw), "subscription_timeout") {
				return true
			}
		}
		return false
	})

	// Dashboard re-subscribes (after=0: it reset lastEventTimeWs).
	relay.Subscribe(sink, key, 0)

	waitFor(t, "second subscribe frame after remote timeout", func() bool { return fake.subscribes.Load() == 2 })
	if n := relaySubCount(relay, key); n != 1 {
		t.Errorf("r.subs[%q] has %d entries, want 1", key, n)
	}
	// A third subscribe without another timeout must NOT churn the remote
	// again: the dropped marker is single-shot. Deterministic negative check:
	// Subscribe writes any `subscribe` frame synchronously, so an `unsubscribe`
	// written afterwards on the same WS is ordered behind it — once the fake
	// has seen the unsubscribe, every subscribe frame has been counted.
	relay.Subscribe(sink, key, 0)
	waitFor(t, "http history for plain re-subscribe", func() bool { return fake.history.Load() >= 1 })
	relay.Unsubscribe(sink, key)
	waitFor(t, "unsubscribe probe frame", func() bool { return fake.unsubscribes.Load() == 1 })
	if n := fake.subscribes.Load(); n != 2 {
		t.Errorf("remote saw %d subscribe frames, want 2 (dropped marker must be consumed by the rebuild)", n)
	}
}

// TestWSRelay_Resubscribe_WithoutTimeout_UsesHTTPHistory pins the trade-off:
// a same-client re-subscribe while the remote subscription is still live (plain
// re-click on the selected session) keeps the existing "history page over HTTP"
// path and does not re-send `subscribe`. Re-sending would make the remote tear
// down and rebuild the shared eventPushLoop and fan a fresh `subscribed` +
// Initial history frame to every other browser on the key — churn the timeout
// marker exists to avoid.
func TestWSRelay_Resubscribe_WithoutTimeout_UsesHTTPHistory(t *testing.T) {
	fake := newRelayResubServer(t)
	relay := newWSRelay(newRelayNode(fake.srv))
	defer relay.Close()

	const key = "feishu:group:reclick"
	sink := &mockSink{id: 1}
	relay.Subscribe(sink, key, 0)
	waitFor(t, "first subscribe frame", func() bool { return fake.subscribes.Load() == 1 })

	relay.Subscribe(sink, key, 0)
	waitFor(t, "http history request", func() bool { return fake.history.Load() == 1 })
	if n := relaySubCount(relay, key); n != 1 {
		t.Errorf("r.subs[%q] has %d entries, want 1", key, n)
	}
	// Ordering probe instead of a sleep: Subscribe writes any `subscribe`
	// frame synchronously before returning, and the unsubscribe below is
	// written after it on the same WS, so once the fake has read the
	// unsubscribe it has read every subscribe frame there will ever be.
	relay.Unsubscribe(sink, key)
	waitFor(t, "unsubscribe probe frame", func() bool { return fake.unsubscribes.Load() == 1 })
	if n := fake.subscribes.Load(); n != 1 {
		t.Errorf("remote saw %d subscribe frames, want 1", n)
	}
}

// TestWSRelay_TimeoutMarker_IgnoredWithoutSubscribers guards the marker map
// against growth from timeout frames for keys nobody holds any more (the
// browser unsubscribed while the frame was in flight).
func TestWSRelay_TimeoutMarker_IgnoredWithoutSubscribers(t *testing.T) {
	fake := newRelayResubServer(t)
	relay := newWSRelay(newRelayNode(fake.srv))
	defer relay.Close()

	const gone, live = "feishu:group:gone", "feishu:group:live"
	sink := &mockSink{id: 1}
	relay.Subscribe(sink, gone, 0)
	relay.Subscribe(sink, live, 0)
	waitFor(t, "subscribe frames", func() bool { return fake.subscribes.Load() == 2 })
	relay.Unsubscribe(sink, gone)

	// Timeout for a key with no local subscribers, then a probe event on the
	// live key. The relay's single readLoop processes frames in order, so once
	// the probe reaches the sink the timeout frame has been handled too.
	fake.inject(t, ServerMsg{Type: "session_state", Key: gone, State: "ready", Reason: "subscription_timeout"})
	fake.inject(t, map[string]any{"type": "event", "key": live, "event": map[string]any{"time": 1, "type": "text"}})
	waitFor(t, "probe event delivered", func() bool {
		for _, raw := range sink.RawMsgs() {
			if strings.Contains(string(raw), live) {
				return true
			}
		}
		return false
	})

	relay.mu.Lock()
	n := len(relay.remoteDropped)
	relay.mu.Unlock()
	if n != 0 {
		t.Errorf("remoteDropped has %d entries after timeout on an unheld key, want 0", n)
	}
}
