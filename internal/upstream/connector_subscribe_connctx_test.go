package upstream

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// countStreamEventsGoroutines reports how many goroutines are currently inside
// Connector.streamEvents (the per-subscription event pump). It walks the full
// stack dump so the assertion is robust to scheduling and does not depend on
// internal channels. Used to prove the #1822 ARCH-3 guard prevents a late
// subscribe from spawning a streamEvents goroutine.
func countStreamEventsGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "upstream.(*Connector).streamEvents")
}

// TestSubscribe_AfterConnCtxCancel_NoStreamGoroutine pins the #1822 ARCH-3
// connector guard: the spawnSession stopped gate covers the three reverse-RPC
// spawn paths but NOT the read-only subscribe path, whose leak is a lingering
// streamEvents goroutine. The subscribe case now short-circuits with
// `if connCtx.Err() != nil { break }` before SessionFor/SubscribeEvents, so a
// subscribe arriving once the connection's context is already cancelled never
// spawns a streamEvents goroutine and never emits a "subscribed" ack.
//
// Determinism: we call handleConn directly with an already-cancelled ctx, so
// connCtx (= WithCancel(ctx)) is cancelled from the first instant — connCtx.Err()
// is non-nil before any message is read. handleConn does NOT close the socket on
// ctx cancel (that is runOnce's watchdog, deliberately not exercised here), so
// the read loop stays parked in ReadJSON and reads the subscribe frame the test
// server sends, hitting the guard with no race against teardown.
func TestSubscribe_AfterConnCtxCancel_NoStreamGoroutine(t *testing.T) {
	const key = "cron:arch3-late-subscribe"

	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	// Register a live, subscribable session so that WITHOUT the guard the
	// subscribe would call SubscribeEvents and spawn a streamEvents goroutine.
	r.RegisterCronStub(key, "/tmp/arch3-test", "prompt")
	if r.SessionFor(key) == nil {
		t.Fatal("setup: RegisterCronStub did not install a subscribable session")
	}

	baseline := countStreamEventsGoroutines()

	// unexpectedAck receives any "subscribed" ack — the guard must prevent it.
	unexpectedAck := make(chan string, 4)
	// serverClosed lets handleConn's ReadJSON unblock once the server hangs up.
	serverClosed := make(chan struct{})

	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer close(serverClosed)
		defer conn.Close()

		// The connector side calls handleConn directly (no register handshake),
		// so just push the subscribe frame the moment the socket is up.
		if err := conn.WriteJSON(node.ReverseMsg{Type: "subscribe", Key: key}); err != nil {
			return
		}

		// With the guard, the loop breaks silently — no "subscribed" and no
		// "subscribe_error". Surface a "subscribed" ack as a regression signal.
		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		var resp node.ReverseMsg
		if err := conn.ReadJSON(&resp); err == nil && resp.Type == "subscribed" {
			unexpectedAck <- resp.Type
		}
	})
	defer srv.Close()

	// Dial the fake server directly from the connector side and drive handleConn
	// with an already-cancelled context.
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	cfg := &Config{URL: wsURL(srv), NodeID: "n", Token: "t"}
	c := New(cfg, r, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // connCtx is born cancelled → the guard fires for any subscribe.

	hcDone := make(chan struct{})
	go func() {
		defer close(hcDone)
		_ = c.handleConn(ctx, conn)
	}()

	// Give handleConn time to read + reject the subscribe frame, then hang up so
	// ReadJSON unblocks and handleConn returns.
	time.Sleep(300 * time.Millisecond)
	_ = conn.Close()

	select {
	case <-hcDone:
	case <-time.After(handleConnDrainBudget + 2*time.Second):
		t.Fatal("handleConn did not return after the socket closed")
	}
	<-serverClosed

	select {
	case ack := <-unexpectedAck:
		t.Fatalf("received %q ack for a subscribe on an already-cancelled connCtx; ARCH-3 guard failed to short-circuit", ack)
	default:
	}

	// No streamEvents goroutine may have been spawned by the late subscribe.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := countStreamEventsGoroutines(); got <= baseline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("streamEvents goroutine count = %d, want <= baseline %d; a late subscribe spawned a leaked stream pump (#1822 ARCH-3 regression)",
				countStreamEventsGoroutines(), baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
