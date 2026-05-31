package server

import (
	"encoding/json"
	"testing"
	"time"
)

// recvRaw drains one raw frame from a captured client's send channel.
func recvRaw(t *testing.T, c *wsClient) ([]byte, bool) {
	t.Helper()
	select {
	case data := <-c.send:
		return data, true
	case <-time.After(time.Second):
		return nil, false
	}
}

// TestMarshalBroadcastAuth_FansOutToAllAuthenticated locks the R243-ARCH-15
// (#845) de-dup: the shared marshalBroadcastAuth tail must deliver the marshaled
// frame to EVERY authenticated client, exactly mirroring the per-call sites it
// replaced (broadcastState / BroadcastSessionReady / cron+daemon run events).
func TestMarshalBroadcastAuth_FansOutToAllAuthenticated(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	c1 := &wsClient{hub: hub, send: make(chan []byte, 8), done: make(chan struct{})}
	c1.authenticated.Store(true)
	c2 := &wsClient{hub: hub, send: make(chan []byte, 8), done: make(chan struct{})}
	c2.authenticated.Store(true)
	registerSub(hub, c1, "")
	registerSub(hub, c2, "")

	hub.marshalBroadcastAuth(cronRunStartedMsg{Type: "cron_run_started", JobID: "abc", RunID: "def"})

	for i, c := range []*wsClient{c1, c2} {
		data, ok := recvRaw(t, c)
		if !ok {
			t.Fatalf("client %d received no frame", i)
		}
		var msg cronRunStartedMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("client %d unmarshal: %v", i, err)
		}
		if msg.Type != "cron_run_started" {
			t.Errorf("client %d Type = %q, want cron_run_started", i, msg.Type)
		}
	}
}

// TestBroadcastSessionReady_ViaMarshalHelper verifies the refactored
// BroadcastSessionReady still emits a session_state running frame through the
// shared helper. Guards against a regression where the helper swallows the
// frame for a wire struct it cannot marshal.
func TestBroadcastSessionReady_ViaMarshalHelper(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	c, out := newCapturedClient(t, hub)
	registerSub(hub, c, "")

	hub.BroadcastSessionReady("feishu:p2p:bob")

	msg, ok := recvMsg(t, out)
	if !ok {
		t.Fatal("client received no frame")
	}
	if msg.Type != "session_state" {
		t.Fatalf("Type = %q, want session_state", msg.Type)
	}
	if msg.Key != "feishu:p2p:bob" || msg.State != "running" {
		t.Errorf("Key/State = %q/%q, want feishu:p2p:bob/running", msg.Key, msg.State)
	}
}
