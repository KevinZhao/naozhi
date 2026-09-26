package server

import (
	"encoding/json"
	"github.com/naozhi/naozhi/internal/runtelemetry"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/wsproto"
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

	hub.marshalBroadcastAuth(wsproto.NewRunStarted(wsproto.RunStarted{Subsystem: "cron", OwnerID: "abc", RunID: "def"}))

	for i, c := range []*wsClient{c1, c2} {
		data, ok := recvRaw(t, c)
		if !ok {
			t.Fatalf("client %d received no frame", i)
		}
		var msg wsproto.RunStarted
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("client %d unmarshal: %v", i, err)
		}
		if msg.Type != "run_started" {
			t.Errorf("client %d Type = %q, want run_started", i, msg.Type)
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
// when NewHub has no authenticated clients, BroadcastRunStarted must not
// panic and must not deliver any frame.
func TestMarshalBroadcastAuth_ZeroAuthClients_NoPanic(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	// No clients registered.
	// Must not panic.
	hub.BroadcastRunStarted(runtelemetry.RunStartedEvent{Subsystem: runtelemetry.SubsystemCron, OwnerID: "aaaa", RunID: "bbbb", Trigger: runtelemetry.TriggerManual, StartedAt: time.Now()})
}

// TestMarshalBroadcastAuth_ZeroAuthClients_NoSendRaw confirms that with zero
// authenticated clients BroadcastRunStarted does not attempt to deliver
// any frame (the SendRaw path is never reached).
func TestMarshalBroadcastAuth_ZeroAuthClients_NoSendRaw(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	// Register an unauthenticated client: the authenticated set stays empty.
	c := &wsClient{hub: hub, send: make(chan []byte, 4), done: make(chan struct{})}
	hub.register(c)

	hub.BroadcastRunStarted(runtelemetry.RunStartedEvent{Subsystem: runtelemetry.SubsystemCron, OwnerID: "cccc", RunID: "dddd", Trigger: runtelemetry.TriggerManual, StartedAt: time.Now()})

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

	hub.BroadcastRunStarted(runtelemetry.RunStartedEvent{Subsystem: runtelemetry.SubsystemCron, OwnerID: "eeee", RunID: "ffff", Trigger: runtelemetry.TriggerManual, StartedAt: time.Now()})

	data, ok := recvRaw(t, c)
	if !ok {
		t.Fatal("authenticated client received no frame")
	}
	var msg wsproto.RunStarted
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Type != "run_started" {
		t.Errorf("Type = %q, want run_started", msg.Type)
	}
}

// TestSnapshotAuthenticated_SingleLockWindow verifies R20260616-PERF-004 (#2141):
// the snapshot-based fast-path returns exactly the authenticated clients and the
// empty-check shares that one snapshot. Two authenticated clients must both be
// present; the snapshot is returned to the pool by the helper.
func TestSnapshotAuthenticated_SingleLockWindow(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	c1 := &wsClient{hub: hub, send: make(chan []byte, 8), done: make(chan struct{})}
	c1.authenticated.Store(true)
	c2 := &wsClient{hub: hub, send: make(chan []byte, 8), done: make(chan struct{})}
	c2.authenticated.Store(true)
	registerSub(hub, c1, "")
	registerSub(hub, c2, "")

	snapPtr, snap := hub.snapshotAuthenticated()
	if got := len(snap); got != 2 {
		t.Errorf("snapshot len = %d, want 2", got)
	}
	releaseBroadcastSnap(snapPtr, snap)
}

// TestSnapshotAuthenticated_EmptyMirror confirms the snapshot is empty when no
// authenticated clients exist, which is what drives the marshal fast-path skip.
func TestSnapshotAuthenticated_EmptyMirror(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	snapPtr, snap := hub.snapshotAuthenticated()
	if len(snap) != 0 {
		t.Errorf("snapshot len = %d, want 0", len(snap))
	}
	releaseBroadcastSnap(snapPtr, snap)
}

// TestMarshalBroadcastAuth_ConcurrentRegisterAndBroadcast stresses the broadcast
// read side against register/unregister writers to surface any race in the
// authenticated set.
// Run with -race.
func TestMarshalBroadcastAuth_ConcurrentRegisterAndBroadcast(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	const writers = 4
	const broadcasters = 4
	const iters = 200

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < broadcasters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				select {
				case <-stop:
					return
				default:
				}
				hub.BroadcastRunStarted(runtelemetry.RunStartedEvent{Subsystem: runtelemetry.SubsystemCron, OwnerID: "j", RunID: "r", Trigger: runtelemetry.TriggerManual, StartedAt: time.Now()})
			}
		}()
	}

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				c := &wsClient{hub: hub, send: make(chan []byte, 64), done: make(chan struct{})}
				c.authenticated.Store(true)
				hub.subs.add(c)
				hub.subs.remove(c)
			}
		}()
	}

	wg.Wait()
	close(stop)
}
