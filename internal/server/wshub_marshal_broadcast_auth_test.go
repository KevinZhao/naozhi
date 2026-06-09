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

// TestMarshalBroadcastAuth_ZeroAuthClients_NoPanic verifies R20260608133928-PERF-1:
// when NewHub has no authenticated clients, BroadcastCronRunStarted must not
// panic and must not deliver any frame. Uses a production hub (authClients != nil)
// so the fast-path is exercised.
func TestMarshalBroadcastAuth_ZeroAuthClients_NoPanic(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	// No clients registered — authClients exists but is empty.
	// Must not panic.
	hub.BroadcastCronRunStarted("aaaa", "bbbb", time.Now(), "manual", "", false)
}

// TestMarshalBroadcastAuth_ZeroAuthClients_NoSendRaw confirms that with zero
// authenticated clients BroadcastCronRunStarted does not attempt to deliver
// any frame (the SendRaw path is never reached).
func TestMarshalBroadcastAuth_ZeroAuthClients_NoSendRaw(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	// Register an unauthenticated client — authClients still empty.
	c := &wsClient{hub: hub, send: make(chan []byte, 4), done: make(chan struct{})}
	hub.mu.Lock()
	hub.clients[c] = struct{}{}
	hub.mu.Unlock()

	hub.BroadcastCronRunStarted("cccc", "dddd", time.Now(), "manual", "", false)

	select {
	case <-c.send:
		t.Fatal("unauthenticated client received a frame; should not have")
	default:
		// expected: no frame sent
	}
}

// TestMarshalBroadcastAuth_WithAuthClient_Delivers verifies that when at least
// one authenticated client is present the fast-path does not fire and the frame
// is delivered normally.
func TestMarshalBroadcastAuth_WithAuthClient_Delivers(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	c := &wsClient{hub: hub, send: make(chan []byte, 8), done: make(chan struct{})}
	c.authenticated.Store(true)
	registerSub(hub, c, "")

	hub.BroadcastCronRunStarted("eeee", "ffff", time.Now(), "manual", "", false)

	data, ok := recvRaw(t, c)
	if !ok {
		t.Fatal("authenticated client received no frame")
	}
	var msg cronRunStartedMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Type != "cron_run_started" {
		t.Errorf("Type = %q, want cron_run_started", msg.Type)
	}
}
