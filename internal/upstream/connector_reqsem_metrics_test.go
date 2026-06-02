package upstream

// Metrics hooks on reqSem (R220-PERF-P2). The gauge/counter pair lets
// operators answer "is the primary dispatching requests faster than we
// can serve them?" without reading the source. These tests lock in the
// two guarantees that matter:
//
//  1. UpstreamReqInflight is a *gauge*: it returns to the pre-request
//     value after the request retires. A leak here would flag an ops
//     dashboard forever after a single blip.
//  2. UpstreamReqWaitTotal does NOT increment on the happy path (slot
//     available immediately). A false positive here would blur the
//     "capacity is binding" signal that the counter is designed to
//     surface. The saturated path is covered by the contention test.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/node"
)

// TestReqSem_InflightGaugeBalanced verifies that a single successful
// request takes the gauge up and back down, leaving no residue. This
// guards against a missing Add(-1) on any return path.
func TestReqSem_InflightGaugeBalanced(t *testing.T) {
	startInflight := reqSemReqInflight.Value()
	startWait := reqSemReqWaitTotal.Value()

	gotResp := make(chan struct{}, 1)
	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		handshake(t, conn)
		req := node.ReverseMsg{Type: "request", ReqID: "rq", Method: "fetch_sessions"}
		if err := conn.WriteJSON(req); err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var resp node.ReverseMsg
		if err := conn.ReadJSON(&resp); err != nil {
			return
		}
		gotResp <- struct{}{}
	})
	defer srv.Close()

	cfg := &Config{URL: wsURL(srv), NodeID: "node1", Token: "tok"}
	c := New(cfg, makeRouter(), nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()

	select {
	case <-gotResp:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for response")
	}
	cancel()
	<-done

	// After the request has fully retired the gauge must be back to its
	// starting value. Tests share package-level expvars, so we compare
	// against a captured baseline rather than literal zero. An off-by-one
	// here would be a leaked slot that eventually pins reqSem in prod.
	if got := reqSemReqInflight.Value(); got != startInflight {
		t.Errorf("UpstreamReqInflight residue: got %d, start %d", got, startInflight)
	}
	// Happy path: the non-blocking acquire succeeded so the wait counter
	// must not have moved. A bump here would be the false-positive
	// failure mode that blurs the saturation signal.
	if got := reqSemReqWaitTotal.Value(); got != startWait {
		t.Errorf("UpstreamReqWaitTotal falsely bumped on uncontended path: got %d, start %d", got, startWait)
	}
}

// TestReqSem_WaitCounterOnSaturation verifies the counter increments
// when a request actually had to block for a slot. We drive 17
// concurrent requests (one over the reqSem cap of 16) through a
// parking previewFunc so every slot stays pinned until we observe the
// wait bump, then release them for a clean shutdown.
func TestReqSem_WaitCounterOnSaturation(t *testing.T) {
	startWait := reqSemReqWaitTotal.Value()

	// Release latch: all 17 handlers block on this channel until the
	// test observes the saturation point, then unblock all at once.
	// `released` makes the cleanup close idempotent — the test happy
	// path closes explicitly, and the t.Cleanup covers the failure
	// exit so no handler goroutine leaks.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)

	const total = 17 // cap=16 → one request must wait

	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		handshake(t, conn)

		// Fire all 17 requests back-to-back. Each targets the parking
		// previewFunc; because the cap is 16, the 17th request lands
		// in the blocking `select` branch, incrementing WaitTotal.
		//
		// Empty session_id bypasses the discovery.IsValidSessionID
		// validation (line 612 checks `SessionID != ""`), letting the
		// handler short-circuit to previewFunc directly. That makes
		// the test insensitive to unrelated changes in session-id
		// format rules.
		for i := 0; i < total; i++ {
			params, _ := json.Marshal(map[string]string{})
			req := node.ReverseMsg{
				Type:   "request",
				ReqID:  fmt.Sprintf("rq-%d", i),
				Method: "fetch_discovered_preview",
				Params: params,
			}
			if err := conn.WriteJSON(req); err != nil {
				t.Logf("write request %d: %v", i, err)
				return
			}
		}

		// Swallow responses so the connector's writeJSON path unblocks.
		// We don't assert on them — the point of this test is WaitTotal.
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		for i := 0; i < total; i++ {
			var resp node.ReverseMsg
			if err := conn.ReadJSON(&resp); err != nil {
				t.Logf("read response %d: %v", i, err)
				return
			}
		}
	})
	defer srv.Close()

	cfg := &Config{URL: wsURL(srv), NodeID: "node1", Token: "tok"}
	c := New(cfg, makeRouter(), nil, nil)

	// Parking previewFunc: each call holds a reqSem slot until the
	// test closes `release`. Returning empty-array JSON keeps the
	// response path happy and avoids a marshal error masking the
	// metric assertion we actually care about.
	c.SetPreviewFunc(func(string) (json.RawMessage, error) {
		<-release
		return json.RawMessage("[]"), nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()

	// Poll WaitTotal: the 17th request blocks on the outer select, so
	// this counter must increment at least once before the deadline.
	// The polling loop is deliberately generous (5s) to stay robust on
	// CI nodes under load; the steady-state increment happens within
	// a few ms once all 17 goroutines are scheduled.
	deadline := time.Now().Add(5 * time.Second)
	observed := false
	for time.Now().Before(deadline) {
		if reqSemReqWaitTotal.Value() > startWait {
			observed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	releaseAll()
	cancel()
	<-done

	if !observed {
		t.Errorf("UpstreamReqWaitTotal did not increment under 17-concurrent saturation; start=%d now=%d",
			startWait, reqSemReqWaitTotal.Value())
	}
}
