package node

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

func reverseWatermark(rc *ReverseConn, key string) (int64, bool) {
	rc.subMu.Lock()
	defer rc.subMu.Unlock()
	t, ok := rc.book.lastEvent[key]
	return t, ok
}

// readNodeFrame reads the next frame the primary wrote to the node side.
func readNodeFrame(t *testing.T, ws *websocket.Conn, wantType string) ReverseMsg {
	t.Helper()
	if err := ws.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var msg ReverseMsg
	if err := ws.ReadJSON(&msg); err != nil {
		t.Fatalf("waiting for %q frame: %v", wantType, err)
	}
	if msg.Type != wantType {
		t.Fatalf("node received %q, want %q", msg.Type, wantType)
	}
	return msg
}

func answerFetchEvents(t *testing.T, ws *websocket.Conn, entries []clievent.EventEntry) {
	t.Helper()
	req := readNodeFrame(t, ws, "request")
	if req.Method != "fetch_events" {
		t.Fatalf("request method = %q, want fetch_events", req.Method)
	}
	result, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteJSON(ReverseMsg{Type: "response", ReqID: req.ReqID, Result: result}); err != nil {
		t.Fatal(err)
	}
}

// TestReverseConn_TracksLastEventPerKey pins the watermark a resubscribe on
// a reconnected node will send: seeded by the first subscriber, advanced by
// pushed events, pushed batches and the history fetches, never regressed,
// and forgotten with the key's last sink.
func TestReverseConn_TracksLastEventPerKey(t *testing.T) {
	rc, ws, cleanup := setupReverseConnPair(t)
	defer cleanup()
	const key = "feishu:direct:u1:general"

	want := func(step string, wantT int64) {
		t.Helper()
		if got, ok := reverseWatermark(rc, key); !ok || got != wantT {
			t.Fatalf("%s: lastEvent = %d (present=%v), want %d", step, got, ok, wantT)
		}
	}

	first := &mockSink{id: 1}
	rc.Subscribe(first, key, 500, 0)
	want("first subscribe seeds", 500)
	readNodeFrame(t, ws, "subscribe")
	answerFetchEvents(t, ws, []clievent.EventEntry{{Time: 900}, {Time: 600}})
	testhelper.Eventually(t, func() bool { return len(first.JSONMsgs()) == 1 }, 3*time.Second, "first-subscribe history frame")
	want("first-subscribe history", 900)

	push := func(m ReverseMsg, rawCount int) {
		t.Helper()
		if err := ws.WriteJSON(m); err != nil {
			t.Fatal(err)
		}
		testhelper.Eventually(t, func() bool { return first.RawMsgCount() == rawCount }, 3*time.Second, "pushed frame delivered")
	}
	push(ReverseMsg{Type: "event", Key: key, Event: &clievent.EventEntry{Time: 1200}}, 1)
	want("pushed event", 1200)
	push(ReverseMsg{Type: "events", Key: key, Events: []clievent.EventEntry{{Time: 1300}, {Time: 1500}, {Time: 1400}}}, 2)
	want("pushed batch", 1500)
	push(ReverseMsg{Type: "event", Key: key, Event: &clievent.EventEntry{Time: 1000}}, 3)
	want("older event", 1500)
	push(ReverseMsg{Type: "session_state", Key: key, State: "ready"}, 4)
	want("frame without an event time", 1500)

	second := &mockSink{id: 2}
	rc.Subscribe(second, key, 100, 0)
	answerFetchEvents(t, ws, []clievent.EventEntry{{Time: 1700}})
	testhelper.Eventually(t, func() bool { return len(second.JSONMsgs()) == 2 }, 3*time.Second, "second-subscriber ack + history")
	want("second subscriber's history, older after not adopted", 1700)

	push(ReverseMsg{Type: "event", Key: "other", Event: &clievent.EventEntry{Time: 42}}, 4)
	push(ReverseMsg{Type: "event", Key: key, Event: &clievent.EventEntry{Time: 1800}}, 5)
	want("after an event for an unheld key", 1800)
	if _, ok := reverseWatermark(rc, "other"); ok {
		t.Fatal("an event for a key nobody holds created a watermark")
	}

	rc.Unsubscribe(first, key)
	want("one sink left", 1800)
	rc.Unsubscribe(second, key)
	if _, ok := reverseWatermark(rc, key); ok {
		t.Fatal("watermark survived the key's last sink")
	}
}

// TestReverseConn_SubscribeErrorForgetsWatermark: the remote refusing a key
// drops it from the book, watermark included.
func TestReverseConn_SubscribeErrorForgetsWatermark(t *testing.T) {
	rc, ws, cleanup := setupReverseConnPair(t)
	defer cleanup()
	const key = "badkey"

	sink := &mockSink{id: 1}
	rc.subMu.Lock()
	rc.book.add(sink, key, 500)
	rc.subMu.Unlock()

	if err := ws.WriteJSON(ReverseMsg{Type: "subscribe_error", Key: key, Error: "no such session"}); err != nil {
		t.Fatal(err)
	}
	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() == 1 }, 3*time.Second, "subscribe_error delivered")
	if _, ok := reverseWatermark(rc, key); ok {
		t.Fatal("subscribe_error left the key's watermark behind")
	}
}

// TestReverseConn_MarkDisconnectedKeepsBook: a dropped conn keeps its sinks
// and watermarks for a reconnect to adopt; only dropSubs, run when the grace
// window lapses, releases them.
func TestReverseConn_MarkDisconnectedKeepsBook(t *testing.T) {
	rc, _, cleanup := setupReverseConnPair(t)
	defer cleanup()
	const key = "feishu:direct:u1:general"

	rc.subMu.Lock()
	rc.book.add(&mockSink{id: 1}, key, 500)
	rc.subMu.Unlock()

	rc.markDisconnected()

	if got, ok := reverseWatermark(rc, key); !ok || got != 500 {
		t.Fatalf("markDisconnected: watermark = %d (present=%v), want 500 kept", got, ok)
	}
	rc.subMu.Lock()
	subs := len(rc.book.subs)
	rc.subMu.Unlock()
	if subs != 1 {
		t.Fatalf("markDisconnected left %d keys with sinks, want 1 kept", subs)
	}

	rc.dropSubs()
	rc.subMu.Lock()
	subs = len(rc.book.subs)
	rc.subMu.Unlock()
	if subs != 0 {
		t.Fatalf("dropSubs left %d keys with sinks", subs)
	}
	if _, ok := reverseWatermark(rc, key); ok {
		t.Fatal("dropSubs left the key's watermark behind")
	}
}
