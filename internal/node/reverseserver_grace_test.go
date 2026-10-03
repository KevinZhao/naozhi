package node

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// testDeregisterGrace keeps tests that wait for OnDeregister fast.
const testDeregisterGrace = 20 * time.Millisecond

const graceKey = "feishu:direct:u1:general"

// graceFixture is a ReverseServer under test plus the channels its lifecycle
// callbacks report on.
type graceFixture struct {
	rs           *ReverseServer
	srv          *httptest.Server
	registered   chan *ReverseConn
	deregistered chan string
}

func newGraceFixture(t *testing.T, grace time.Duration) *graceFixture {
	t.Helper()
	f := &graceFixture{
		rs:           newTestReverseServer("node-1", "tok", false),
		registered:   make(chan *ReverseConn, 4),
		deregistered: make(chan string, 4),
	}
	f.rs.deregisterGrace = grace
	f.rs.OnRegister = func(_ string, rc *ReverseConn) { f.registered <- rc }
	f.rs.OnDeregister = func(id string) { f.deregistered <- id }
	mux := http.NewServeMux()
	mux.Handle("/ws-node", f.rs)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// dial registers a node link and returns it with the primary-side conn.
func (f *graceFixture) dial(t *testing.T) (*websocket.Conn, *ReverseConn) {
	t.Helper()
	return f.dialCaps(t, nil)
}

// dialCaps is dial for a node advertising caps.
func (f *graceFixture) dialCaps(t *testing.T, caps []string) (*websocket.Conn, *ReverseConn) {
	t.Helper()
	ws := dialReverseNode(t, f.srv)
	t.Cleanup(func() { ws.Close() })
	if resp := reverseAuthWithCaps(t, ws, "node-1", "tok", caps); resp.Type != "registered" {
		t.Fatalf("expected registered, got %q", resp.Type)
	}
	select {
	case rc := <-f.registered:
		return ws, rc
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for OnRegister")
		return nil, nil
	}
}

func (f *graceFixture) lingering(id string) *lingerEntry {
	f.rs.mu.RLock()
	defer f.rs.mu.RUnlock()
	return f.rs.lingering[id]
}

func (f *graceFixture) waitLingering(t *testing.T) *lingerEntry {
	t.Helper()
	testhelper.Eventually(t, func() bool { return f.lingering("node-1") != nil }, 3*time.Second, "dropped conn parked")
	return f.lingering("node-1")
}

func (f *graceFixture) expectNoDeregister(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case id := <-f.deregistered:
		t.Fatalf("unexpected OnDeregister(%q)", id)
	case <-time.After(within):
	}
}

func bookSinks(rc *ReverseConn, key string) []EventSink {
	rc.subMu.Lock()
	defer rc.subMu.Unlock()
	return append([]EventSink(nil), rc.book.subs[key]...)
}

// subscribeOnLink subscribes sink on rc and answers the history fetch the
// first subscriber triggers, so the watermark ends at newest.
func subscribeOnLink(t *testing.T, rc *ReverseConn, ws *websocket.Conn, sink EventSink, newest int64) {
	t.Helper()
	rc.Subscribe(sink, graceKey, 0, 0)
	readNodeFrame(t, ws, "subscribe")
	answerFetchEvents(t, ws, []clievent.EventEntry{{Time: newest}})
	testhelper.Eventually(t, func() bool {
		got, _ := reverseWatermark(rc, graceKey)
		return got == newest
	}, 3*time.Second, "initial history observed")
}

// rawFrames decodes what a sink received through SendRaw.
func rawFrames(t *testing.T, s *mockSink) []ServerMsg {
	t.Helper()
	var out []ServerMsg
	for _, raw := range s.RawMsgs() {
		var m ServerMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// TestReverseServer_RedialWithinGrace_KeepsSubscriptions: a node that drops
// and dials back inside the grace window is never deregistered; the new link
// is asked for the key again from the newest event the sink holds, the gap is
// replayed as a non-initial page, and live events reach the original sink.
func TestReverseServer_RedialWithinGrace_KeepsSubscriptions(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws1, rc1 := f.dial(t)
	sink := &mockSink{id: 1}
	subscribeOnLink(t, rc1, ws1, sink, 300)

	ws1.Close()
	f.waitLingering(t)

	ws2, rc2 := f.dial(t)
	sub := readNodeFrame(t, ws2, "subscribe")
	if sub.Key != graceKey || sub.After != 300 {
		t.Fatalf("resubscribe = {key %q, after %d}, want {%q, 300}", sub.Key, sub.After, graceKey)
	}
	answerFetchEvents(t, ws2, []clievent.EventEntry{{Time: 300}, {Time: 400}})
	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() == 1 }, 3*time.Second, "catch-up page")
	catchUp := rawFrames(t, sink)[0]
	if catchUp.Type != "history" || catchUp.Initial || len(catchUp.Events) != 2 {
		t.Fatalf("catch-up frame = %+v, want a non-initial history page of 2 events", catchUp)
	}

	if err := ws2.WriteJSON(ReverseMsg{Type: "event", Key: graceKey, Event: &clievent.EventEntry{Time: 500}}); err != nil {
		t.Fatal(err)
	}
	testhelper.Eventually(t, func() bool { return sink.RawMsgCount() == 2 }, 3*time.Second, "live event on the new link")
	if got, _ := reverseWatermark(rc2, graceKey); got != 500 {
		t.Fatalf("new link's watermark = %d, want 500", got)
	}
	if n := len(bookSinks(rc1, graceKey)); n != 0 {
		t.Fatalf("dropped conn still holds %d sinks after the handover", n)
	}
	if f.lingering("node-1") != nil {
		t.Fatal("parked entry survived the adoption")
	}
	f.expectNoDeregister(t, 200*time.Millisecond)
}

// TestReverseServer_GraceLapses_Deregisters: no redial means one
// OnDeregister after the window, and the parked sinks are released.
func TestReverseServer_GraceLapses_Deregisters(t *testing.T) {
	f := newGraceFixture(t, 150*time.Millisecond)
	ws1, rc1 := f.dial(t)
	subscribeOnLink(t, rc1, ws1, &mockSink{id: 1}, 300)

	dropped := time.Now()
	ws1.Close()
	select {
	case id := <-f.deregistered:
		if id != "node-1" {
			t.Fatalf("OnDeregister(%q), want node-1", id)
		}
		if waited := time.Since(dropped); waited < 150*time.Millisecond {
			t.Fatalf("OnDeregister after %v, before the grace window ended", waited)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("grace window lapsed without OnDeregister")
	}
	if n := len(bookSinks(rc1, graceKey)); n != 0 {
		t.Fatalf("expired conn still holds %d sinks", n)
	}
	if _, ok := reverseWatermark(rc1, graceKey); ok {
		t.Fatal("expired conn kept the key's watermark")
	}
	f.expectNoDeregister(t, 300*time.Millisecond)
}

// TestReverseServer_Displacement_MigratesSubscriptions: a redial that
// arrives while the old link still looks alive takes its subscriptions too.
func TestReverseServer_Displacement_MigratesSubscriptions(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws1, rc1 := f.dial(t)
	sink := &mockSink{id: 1}
	subscribeOnLink(t, rc1, ws1, sink, 300)

	ws2, rc2 := f.dial(t)
	if sub := readNodeFrame(t, ws2, "subscribe"); sub.Key != graceKey || sub.After != 300 {
		t.Fatalf("resubscribe = {key %q, after %d}, want {%q, 300}", sub.Key, sub.After, graceKey)
	}
	if got := bookSinks(rc2, graceKey); len(got) != 1 || got[0] != sink {
		t.Fatalf("new link holds %v, want the displaced link's sink", got)
	}
	f.expectNoDeregister(t, 200*time.Millisecond)
}

// TestReverseServer_RedialDisplacesHandshake_KeepsParkedSubscriptions: a
// redial that displaces a successor still mid-handshake must not overwrite
// the parked conn the successor claimed; the newest link adopts its sinks.
func TestReverseServer_RedialDisplacesHandshake_KeepsParkedSubscriptions(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	held := make(chan struct{})
	release := make(chan struct{})
	var hookCalls atomic.Int32
	f.rs.testHookBeforeAck = func(*ReverseConn) {
		if hookCalls.Add(1) == 2 {
			close(held)
			<-release
		}
	}
	ws1, rc1 := f.dial(t)
	sink := &mockSink{id: 1}
	subscribeOnLink(t, rc1, ws1, sink, 300)
	ws1.Close()
	f.waitLingering(t)

	ws2 := dialReverseNode(t, f.srv)
	t.Cleanup(func() { ws2.Close() })
	if err := ws2.WriteJSON(ReverseMsg{Type: "register", NodeID: "node-1", Token: "tok", Hostname: "h"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("link 2 never reached the ack hook")
	}

	ws3, rc3 := f.dial(t)
	close(release)
	if sub := readNodeFrame(t, ws3, "subscribe"); sub.Key != graceKey || sub.After != 300 {
		t.Fatalf("resubscribe = {key %q, after %d}, want {%q, 300}", sub.Key, sub.After, graceKey)
	}
	if got := bookSinks(rc3, graceKey); len(got) != 1 || got[0] != sink {
		t.Fatalf("newest link holds %v, want the sink parked before the handshake", got)
	}
	if f.lingering("node-1") != nil {
		t.Fatal("parked entry survived the adoption")
	}
	f.expectNoDeregister(t, 200*time.Millisecond)
}

// expectReconnecting requires sink's only frame to be the keyed refusal.
func expectReconnecting(t *testing.T, sink *mockSink, node string) {
	t.Helper()
	msgs := sink.JSONMsgs()
	if len(msgs) != 1 {
		t.Fatalf("sink got %d frames, want one error", len(msgs))
	}
	m, ok := msgs[0].(ServerMsg)
	if !ok || m.Type != "error" || m.Key != graceKey || m.Node != node || m.Error != errNodeReconnecting {
		t.Fatalf("frame = %+v, want error{%q, %q, %q}", msgs[0], graceKey, node, errNodeReconnecting)
	}
}

// TestReverseConn_SubscribeOnDroppedConn_KeyedError: a subscribe that reaches
// a conn in its grace window is refused with a keyed error the dashboard can
// retry on, instead of being dropped silently or parked without history.
func TestReverseConn_SubscribeOnDroppedConn_KeyedError(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws1, rc1 := f.dial(t)
	parked := &mockSink{id: 1}
	subscribeOnLink(t, rc1, ws1, parked, 300)
	ws1.Close()
	f.waitLingering(t)

	late := &mockSink{id: 2}
	rc1.Subscribe(late, graceKey, 0, 0)
	expectReconnecting(t, late, "node-1")
	if got := bookSinks(rc1, graceKey); len(got) != 1 || got[0] != parked {
		t.Fatalf("parked key holds %v, want only the sink from before the drop", got)
	}
}

// TestReverseConn_SubscribeWriteFails_KeyedError: the first subscribe frame
// failing to reach the node is refused the same way, on a legacy node and on
// one answering the opening page in-band.
func TestReverseConn_SubscribeWriteFails_KeyedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil); err == nil {
			c.Close()
		}
	}))
	defer srv.Close()
	for name, caps := range map[string][]string{"legacy": nil, "in-band": historyCaps} {
		t.Run(name, func(t *testing.T) {
			ws := dialReverseNode(t, srv)
			ws.Close()
			rc := newReverseConnWithMeta("node-1", "", "", ws, caps, "")

			sink := &mockSink{id: 1}
			rc.Subscribe(sink, graceKey, 0, 0)
			expectReconnecting(t, sink, "node-1")
			if n := len(bookSinks(rc, graceKey)); n != 0 {
				t.Fatalf("a failed subscribe left %d sinks behind", n)
			}
			// Not deferred: after an unbalanced subWG.Done, Close's Wait
			// would hang instead of letting the panic surface.
			rc.Close()
		})
	}
}

// TestReverseServer_SupersededLingerTimer_NoOp: a grace timer whose entry was
// claimed by a redial must not deregister or drop sinks when it fires late.
func TestReverseServer_SupersededLingerTimer_NoOp(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws1, rc1 := f.dial(t)
	subscribeOnLink(t, rc1, ws1, &mockSink{id: 1}, 300)
	ws1.Close()
	stale := f.waitLingering(t)

	f.rs.mu.Lock()
	f.rs.claimLingerLocked("node-1")
	f.rs.mu.Unlock()
	f.rs.expireLinger("node-1", stale)

	f.expectNoDeregister(t, 100*time.Millisecond)
	if n := len(bookSinks(rc1, graceKey)); n != 1 {
		t.Fatalf("stale timer left %d sinks, want the parked one", n)
	}
	if f.lingering("node-1") == nil {
		t.Fatal("stale timer removed the claimed entry")
	}
}

// TestReverseConn_HeirReceivesCallsRoutedToPredecessor: the hub keeps
// routing to the old conn until OnRegister replaces it; every subscription
// call landing in that gap must act on the new link, or a closed tab would
// stay subscribed there for good.
func TestReverseConn_HeirReceivesCallsRoutedToPredecessor(t *testing.T) {
	f := newGraceFixture(t, 5*time.Second)
	ws1, rc1 := f.dial(t)
	sink := &mockSink{id: 1}
	subscribeOnLink(t, rc1, ws1, sink, 300)
	ws1.Close()
	f.waitLingering(t)

	ws2, rc2 := f.dial(t)
	readNodeFrame(t, ws2, "subscribe")
	answerFetchEvents(t, ws2, nil)

	rc1.Unsubscribe(sink, graceKey)
	if n := len(bookSinks(rc2, graceKey)); n != 0 {
		t.Fatalf("Unsubscribe on the predecessor left %d sinks on the heir", n)
	}
	readNodeFrame(t, ws2, "unsubscribe")

	second := &mockSink{id: 2}
	rc1.Subscribe(second, graceKey, 0, 0)
	readNodeFrame(t, ws2, "subscribe")
	answerFetchEvents(t, ws2, nil)
	if got := bookSinks(rc2, graceKey); len(got) != 1 || got[0] != second {
		t.Fatalf("heir holds %v, want the sink subscribed through the predecessor", got)
	}

	rc1.RefreshSubscription(graceKey)
	readNodeFrame(t, ws2, "subscribe")

	rc1.RemoveClient(second)
	if n := len(bookSinks(rc2, graceKey)); n != 0 {
		t.Fatalf("RemoveClient on the predecessor left %d sinks on the heir", n)
	}
	if unsub := readNodeFrame(t, ws2, "unsubscribe"); unsub.Key != graceKey {
		t.Fatalf("unsubscribe key = %q, want %q", unsub.Key, graceKey)
	}
}

// TestReverseServer_DeregisterBeforeNewerRegister: a deregister the grace
// timer already decided finishes before a newer registration's OnRegister,
// so the upper layer never ends with a live node removed.
func TestReverseServer_DeregisterBeforeNewerRegister(t *testing.T) {
	f := newGraceFixture(t, 50*time.Millisecond)
	inDeregister := make(chan struct{})
	release := make(chan struct{})
	var deregisters atomic.Int32
	var mu sync.Mutex
	var order []string
	record := func(what string) {
		mu.Lock()
		order = append(order, what)
		mu.Unlock()
	}
	f.rs.OnDeregister = func(string) {
		if deregisters.Add(1) > 1 {
			return
		}
		close(inDeregister)
		<-release
		record("deregister")
	}
	f.rs.OnRegister = func(_ string, rc *ReverseConn) {
		record("register")
		f.registered <- rc
	}

	ws1, _ := f.dial(t)
	ws1.Close()
	select {
	case <-inDeregister:
	case <-time.After(3 * time.Second):
		t.Fatal("grace window lapsed without OnDeregister")
	}

	ws2 := dialReverseNode(t, f.srv)
	t.Cleanup(func() { ws2.Close() })
	if resp := reverseAuth(t, ws2, "node-1", "tok", "h"); resp.Type != "registered" {
		t.Fatalf("expected registered, got %q", resp.Type)
	}
	select {
	case <-f.registered:
		t.Fatal("OnRegister ran while the earlier OnDeregister was still in progress")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-f.registered:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for OnRegister")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(order, []string{"register", "deregister", "register"}) {
		t.Fatalf("callback order = %v, want [register deregister register]", order)
	}
}
