package node

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// blockingSink is a mockSink variant whose SendJSON blocks until the gate
// is released. Used to hold sendHistoryToClient inside the wg window so the
// test can verify Close() waits for the goroutine to exit.
type blockingSink struct {
	mockSink
	gate    chan struct{}
	entered chan struct{} // receives a signal when SendJSON is first called
	once    sync.Once
}

func newBlockingSink() *blockingSink {
	return &blockingSink{
		gate:    make(chan struct{}),
		entered: make(chan struct{}, 1),
	}
}

func (s *blockingSink) SendJSON(v any) {
	s.once.Do(func() {
		select {
		case s.entered <- struct{}{}:
		default:
		}
	})
	<-s.gate
	s.mockSink.SendJSON(v)
}

func (s *blockingSink) release() { close(s.gate) }

// TestWSRelay_Close_WaitsForSendHistoryGoroutine locks R184-CONC-M1: Close
// must block until sendHistoryToClient goroutines exit. Without wg tracking,
// Close returned while the goroutine was still running and could SendJSON to
// a half-closed sink.
func TestWSRelay_Close_WaitsForSendHistoryGoroutine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ws":
			upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			if !authHandshake(t, conn) {
				return
			}
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			for {
				var msg ClientMsg
				if err := conn.ReadJSON(&msg); err != nil {
					return
				}
			}
		case "/api/sessions/events":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		}
	}))
	defer srv.Close()

	node := newRelayNode(srv)
	relay := newWSRelay(node)

	first := &mockSink{id: 1}
	relay.Subscribe(first, "k", 0)
	time.Sleep(80 * time.Millisecond) // first subscriber path completes

	second := newBlockingSink()
	relay.Subscribe(second, "k", 0) // second subscriber → sendHistoryToClient goroutine

	// Wait until sendHistoryToClient has called SendJSON (i.e., the goroutine
	// is parked inside our blockingSink).
	select {
	case <-second.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("sendHistoryToClient did not reach SendJSON")
	}

	closeReturned := make(chan struct{})
	go func() {
		relay.Close()
		close(closeReturned)
	}()

	// Close must NOT return while the goroutine is parked inside SendJSON:
	// the wg is non-zero.
	select {
	case <-closeReturned:
		t.Fatal("Close() returned while sendHistoryToClient was still running")
	case <-time.After(150 * time.Millisecond):
	}

	// Release the sink → goroutine finishes → wg.Done → Close returns.
	second.release()
	select {
	case <-closeReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("Close() did not return after goroutine released")
	}
}

// TestWSRelay_Subscribe_AfterClose_NoLeak verifies Subscribe is a no-op after
// Close. Without the r.closed guard, Subscribe could call wg.Add(1) after
// Close already returned, breaking the "Add before Wait" rule next time Close
// runs (panic) or silently dispatching goroutines on a half-shut relay.
func TestWSRelay_Subscribe_AfterClose_NoLeak(t *testing.T) {
	node := NewHTTPClient("n", "http://127.0.0.1:1", "", "")
	relay := newWSRelay(node)
	relay.Close()

	sink := &mockSink{id: 1}
	relay.Subscribe(sink, "k", 0) // must not deadlock, panic, or dispatch

	msgs := sink.JSONMsgs()
	if len(msgs) == 0 {
		t.Fatal("expected error message sent to sink on post-close subscribe")
	}
	// Subscribe hits ensureConnected first which returns "relay closed" via
	// the connection-failure branch. Either of the two error phrasings is
	// acceptable; the point is the sink was notified and no goroutine ran.
	if msg, ok := msgs[0].(ServerMsg); ok {
		if msg.Type != "error" {
			t.Errorf("expected type=error on post-close subscribe, got %q", msg.Type)
		}
	}

	done := make(chan struct{})
	go func() {
		relay.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung after post-close Subscribe")
	}
}

// TestWSRelay_MultipleSecondSubscribers_AllTracked verifies N concurrent
// second-subscriber goroutines are all tracked by wg. Close must block until
// every one exits.
func TestWSRelay_MultipleSecondSubscribers_AllTracked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ws":
			upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			if !authHandshake(t, conn) {
				return
			}
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			for {
				var msg ClientMsg
				if err := conn.ReadJSON(&msg); err != nil {
					return
				}
			}
		case "/api/sessions/events":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		}
	}))
	defer srv.Close()

	node := newRelayNode(srv)
	relay := newWSRelay(node)

	first := &mockSink{id: 0}
	relay.Subscribe(first, "k", 0)
	time.Sleep(80 * time.Millisecond)

	const extra = 5
	sinks := make([]*blockingSink, extra)
	for i := 0; i < extra; i++ {
		sinks[i] = newBlockingSink()
		relay.Subscribe(sinks[i], "k", 0)
	}

	// Confirm all sinks have been entered (goroutines parked).
	entered := int32(0)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&entered) < extra {
		for _, s := range sinks {
			select {
			case <-s.entered:
				atomic.AddInt32(&entered, 1)
			default:
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&entered); got != extra {
		t.Fatalf("expected all %d second-subscriber goroutines to enter SendJSON, got %d", extra, got)
	}

	closeReturned := make(chan struct{})
	go func() {
		relay.Close()
		close(closeReturned)
	}()

	select {
	case <-closeReturned:
		t.Fatal("Close returned while history handlers were still active")
	case <-time.After(120 * time.Millisecond):
	}

	// Release all; Close must then return within a bounded window.
	for _, s := range sinks {
		s.release()
	}
	select {
	case <-closeReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after all handlers released")
	}
}

// TestWSRelay_Source_WaitGroupInvariants is a regression guard: R184-CONC-M1
// requires four source-level anchors that must stay together. Any refactor
// dropping one will fail and force a conscious decision.
func TestWSRelay_Source_WaitGroupInvariants(t *testing.T) {
	data, err := os.ReadFile("relay.go")
	if err != nil {
		t.Fatalf("read relay.go: %v", err)
	}
	src := string(data)

	// 1. wg field declared on wsRelay.
	if !regexp.MustCompile(`(?m)^\s*wg\s+sync\.WaitGroup`).MatchString(src) {
		t.Error("wsRelay must declare wg sync.WaitGroup")
	}

	// 2. Subscribe path does wg.Add(1) for the alreadySubscribed branch.
	if !regexp.MustCompile(`r\.wg\.Add\(1\)`).MatchString(src) {
		t.Error("Subscribe must call r.wg.Add(1) for second-subscriber path")
	}

	// 3. sendHistoryToClient owns the matching Done via defer.
	if !regexp.MustCompile(`(?s)func \(r \*wsRelay\) sendHistoryToClient[^{]*\{\s*defer r\.wg\.Done\(\)`).MatchString(src) {
		t.Error("sendHistoryToClient must defer r.wg.Done() as its first statement")
	}

	// 4. Close waits on wg (multi-line body scan).
	if !regexp.MustCompile(`(?s)func \(r \*wsRelay\) Close\(\)[^{]*\{.*?r\.wg\.Wait\(\)`).MatchString(src) {
		t.Error("Close must call r.wg.Wait()")
	}

	// 5. Subscribe must check r.closed under r.mu to prevent Add-after-Close.
	if !regexp.MustCompile(`(?s)func \(r \*wsRelay\) Subscribe[^{]*\{.*?r\.mu\.Lock\(\).*?if r\.closed`).MatchString(src) {
		t.Error("Subscribe must check r.closed under r.mu before wg.Add")
	}
}
