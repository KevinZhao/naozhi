package node

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// historyCaps is what a node answering want_history subscribes advertises.
var historyCaps = []string{clievent.SchemaCap, CapSubscribeHistory}

// expectNoNodeFrame fails if the primary writes anything to the node side
// within d. It leaves ws unusable for reads, so call it last.
func expectNoNodeFrame(t *testing.T, ws *websocket.Conn, d time.Duration) {
	t.Helper()
	if err := ws.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
	var msg ReverseMsg
	if err := ws.ReadJSON(&msg); err == nil {
		t.Fatalf("node received an unexpected %q frame (method %q)", msg.Type, msg.Method)
	}
}

// TestReverseConn_SubscribeHistoryCap_PageArrivesInBand: against a node with
// the cap, the first subscribe carries limit and want_history, the node's own
// page reaches the sink with its initial/has_more flags, and no fetch_events
// pulls the same history a second time.
func TestReverseConn_SubscribeHistoryCap_PageArrivesInBand(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws, rc := f.dialCaps(t, historyCaps)
	sink := &mockSink{id: 1}

	rc.Subscribe(sink, graceKey, 0, 50)
	sub := readNodeFrame(t, ws, "subscribe")
	if sub.Key != graceKey || sub.After != 0 || sub.Limit != 50 || !sub.WantHistory {
		t.Fatalf("subscribe = {key %q after %d limit %d want_history %v}, want {%q 0 50 true}", sub.Key, sub.After, sub.Limit, sub.WantHistory, graceKey)
	}
	more := true
	if err := ws.WriteJSON(ReverseMsg{Type: "events", Key: graceKey, Initial: true, HasMore: &more,
		Events: []clievent.EventEntry{{Time: 100, UUID: "a"}, {Time: 200, UUID: "b"}}}); err != nil {
		t.Fatal(err)
	}
	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() == 1 }, 3*time.Second, "opening page")
	page := rawFrames(t, sink)[0]
	if page.Type != "history" || !page.Initial || page.HasMore == nil || !*page.HasMore || len(page.Events) != 2 {
		t.Fatalf("opening page = %+v, want an initial history of 2 events with has_more=true", page)
	}
	if got, _ := reverseWatermark(rc, graceKey); got != 200 {
		t.Fatalf("watermark = %d, want 200", got)
	}
	expectNoNodeFrame(t, ws, 200*time.Millisecond)
}

// TestReverseConn_LegacyNode_FetchesOpeningPage: a node without the cap gets
// the subscribe it always got, and the page comes from fetch_events.
func TestReverseConn_LegacyNode_FetchesOpeningPage(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws, rc := f.dial(t)
	sink := &mockSink{id: 1}

	rc.Subscribe(sink, graceKey, 0, 50)
	sub := readNodeFrame(t, ws, "subscribe")
	if sub.Limit != 0 || sub.WantHistory {
		t.Fatalf("legacy subscribe carries limit %d want_history %v, want neither", sub.Limit, sub.WantHistory)
	}
	answerFetchEvents(t, ws, []clievent.EventEntry{{Time: 100, UUID: "a"}})
	testhelper.Eventually(t, func() bool { return sink.JSONMsgCount() == 1 }, 3*time.Second, "fetched page")
	if page, ok := sink.JSONMsgs()[0].(ServerMsg); !ok || page.Type != "history" || !page.Initial || len(page.Events) != 1 {
		t.Fatalf("fetched page = %+v, want an initial history of 1 event", sink.JSONMsgs()[0])
	}
}

// TestReverseConn_CappedOpeningPage_ReportsMore: trimming an opening page to
// maxPushedHistoryEvents leaves older history behind, so has_more turns true;
// a capped live batch stays a plain batch.
func TestReverseConn_CappedOpeningPage_ReportsMore(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws, rc := f.dialCaps(t, historyCaps)
	sink := &mockSink{id: 1}
	rc.Subscribe(sink, graceKey, 0, 50)
	readNodeFrame(t, ws, "subscribe")

	events := make([]clievent.EventEntry, maxPushedHistoryEvents+1)
	for i := range events {
		events[i] = clievent.EventEntry{Time: int64(i + 1)}
	}
	noMore := false
	if err := ws.WriteJSON(ReverseMsg{Type: "events", Key: graceKey, Events: events, Initial: true, HasMore: &noMore}); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteJSON(ReverseMsg{Type: "events", Key: graceKey, Events: events}); err != nil {
		t.Fatal(err)
	}
	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() == 2 }, 3*time.Second, "both frames")
	frames := rawFrames(t, sink)
	if p := frames[0]; !p.Initial || p.HasMore == nil || !*p.HasMore || len(p.Events) != maxPushedHistoryEvents {
		t.Fatalf("capped opening page initial=%v has_more=%v len=%d, want initial, has_more=true, %d events", p.Initial, p.HasMore, len(p.Events), maxPushedHistoryEvents)
	}
	if b := frames[1]; b.Initial || b.HasMore != nil {
		t.Fatalf("capped live batch initial=%v has_more=%v, want neither", b.Initial, b.HasMore)
	}
}

// TestReverseServer_RedialWithHistoryCap_CatchUpInBand: a node that comes back
// upgraded is resubscribed with want_history from the sink's watermark; its
// catch-up arrives as a plain batch and no fetch_events is issued.
func TestReverseServer_RedialWithHistoryCap_CatchUpInBand(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws1, rc1 := f.dial(t)
	sink := &mockSink{id: 1}
	subscribeOnLink(t, rc1, ws1, sink, 300)

	ws1.Close()
	f.waitLingering(t)

	ws2, rc2 := f.dialCaps(t, historyCaps)
	sub := readNodeFrame(t, ws2, "subscribe")
	if sub.Key != graceKey || sub.After != 300 || !sub.WantHistory {
		t.Fatalf("resubscribe = {key %q after %d want_history %v}, want {%q 300 true}", sub.Key, sub.After, sub.WantHistory, graceKey)
	}
	if err := ws2.WriteJSON(ReverseMsg{Type: "events", Key: graceKey, Events: []clievent.EventEntry{{Time: 300}, {Time: 400}}}); err != nil {
		t.Fatal(err)
	}
	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() == 1 }, 3*time.Second, "catch-up batch")
	if catchUp := rawFrames(t, sink)[0]; catchUp.Type != "history" || catchUp.Initial || len(catchUp.Events) != 2 {
		t.Fatalf("catch-up = %+v, want a non-initial history of 2 events", catchUp)
	}
	if got, _ := reverseWatermark(rc2, graceKey); got != 400 {
		t.Fatalf("new link's watermark = %d, want 400", got)
	}
	expectNoNodeFrame(t, ws2, 200*time.Millisecond)
}
