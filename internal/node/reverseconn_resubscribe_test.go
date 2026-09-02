package node

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestReverseConn_Subscribe_SameClientTwice_NoDuplicateSink is the ReverseConn
// twin of TestWSRelay_Subscribe_SameClientTwice_NoDuplicateSink (#2421 review
// F1). Re-subscribing an EventSink that already holds the key must not append
// it a second time — otherwise broadcastToSubs delivers every relayed frame
// twice to the same browser. The re-subscribing client still gets its
// `subscribed` ack + history page through the additional-subscriber RPC path.
func TestReverseConn_Subscribe_SameClientTwice_NoDuplicateSink(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	var subscribes atomic.Int32
	go func() {
		wsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			var msg ReverseMsg
			if err := wsConn.ReadJSON(&msg); err != nil {
				return
			}
			switch {
			case msg.Type == "subscribe":
				subscribes.Add(1)
			case msg.Type == "request" && msg.Method == "fetch_events":
				_ = wsConn.WriteJSON(ReverseMsg{Type: "response", ReqID: msg.ReqID, Result: []byte(`[]`)})
			}
		}
	}()

	const key = "feishu:group:rc-resub"
	sink := &mockSink{id: 1}
	rc.Subscribe(sink, key, 0)
	waitFor(t, "first subscribe frame", func() bool { return subscribes.Load() == 1 })

	rc.Subscribe(sink, key, 0)

	rc.subMu.Lock()
	n := len(rc.subs[key])
	rc.subMu.Unlock()
	if n != 1 {
		t.Fatalf("c.subs[%q] has %d entries after same-client re-subscribe, want 1", key, n)
	}
	// The re-subscriber still receives its ack through the RPC path.
	waitFor(t, "subscribed ack for re-subscriber", func() bool {
		for _, m := range sink.JSONMsgs() {
			if sm, ok := m.(ServerMsg); ok && sm.Type == "subscribed" && sm.Key == key {
				return true
			}
		}
		return false
	})
	if n := subscribes.Load(); n != 1 {
		t.Errorf("remote saw %d subscribe frames, want 1", n)
	}
}
