package node

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// H7 (Round 163): ReverseConn.baseCtx unifies cancellation of in-flight
// Subscribe history fetches with connection shutdown, replacing the per-RPC
// "cancelOnClose watcher" goroutine pattern that previously held `c`
// references unbounded.

// TestReverseConn_BaseCtxCancelledOnClose locks the contract that Close()
// propagates to baseCtx. Subscribe goroutines derive their timeout contexts
// from baseCtx, so this cancellation is what replaces the old cancelOnClose
// watcher goroutine.
func TestReverseConn_BaseCtxCancelledOnClose(t *testing.T) {
	rc, _, cleanup := setupReverseConnPair(t)
	defer cleanup()

	select {
	case <-rc.baseCtx.Done():
		t.Fatal("baseCtx should not be cancelled before Close")
	default:
	}

	rc.Close()

	select {
	case <-rc.baseCtx.Done():
		if err := rc.baseCtx.Err(); err != context.Canceled {
			t.Errorf("baseCtx.Err: want Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("baseCtx did not cancel within 2s of Close")
	}
}

// TestReverseConn_BaseCtxCancelledOnMarkDisconnected covers the readLoop
// drop path (markDisconnected is called from the readLoop defer). Subscribe
// goroutines must unwind on a silent TCP drop just as they do on an explicit
// Close.
func TestReverseConn_BaseCtxCancelledOnMarkDisconnected(t *testing.T) {
	rc, _, cleanup := setupReverseConnPair(t)
	defer cleanup()

	rc.markDisconnected()

	select {
	case <-rc.baseCtx.Done():
		if err := rc.baseCtx.Err(); err != context.Canceled {
			t.Errorf("baseCtx.Err: want Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("baseCtx did not cancel within 2s of markDisconnected")
	}
}

// TestReverseConn_BaseCtxCancelIsIdempotent guards against double-cancel
// panics: Close and markDisconnected may both fire on a race (Close called
// externally while readLoop already returned). context.CancelFunc is
// documented as safe to call multiple times, but the test pins the
// expectation so a future refactor (e.g. closing a channel in place of a
// cancel func) would be flagged.
func TestReverseConn_BaseCtxCancelIsIdempotent(t *testing.T) {
	rc, _, cleanup := setupReverseConnPair(t)
	defer cleanup()

	rc.Close()
	rc.markDisconnected() // must not panic
	rc.Close()            // must not panic

	select {
	case <-rc.baseCtx.Done():
	case <-time.After(1 * time.Second):
		t.Fatal("baseCtx should have been cancelled")
	}
}

// TestReverseConn_SubscribeHistoryAbortsOnClose exercises the real Subscribe
// path: the goroutine issues FetchEvents against a remote that never replies,
// so it parks inside rpc() until either its ctx times out (5s) or baseCtx
// cancels. After Close, we expect the goroutine to unwind quickly (well
// under the 5s RPC deadline) because baseCtx is the timeout's parent.
//
// The old code used a separate watcher goroutine to funnel c.done into
// cancel; removing that goroutine while relying on baseCtx is exactly the
// fix this test protects.
func TestReverseConn_SubscribeHistoryAbortsOnClose(t *testing.T) {
	rc, wsConn, cleanup := setupReverseConnPair(t)
	defer cleanup()

	// Drain the "subscribe" frame the remote would receive, so the write
	// doesn't block in readLoop's buffer. We deliberately do NOT send a
	// response — FetchEvents will park in rpc() waiting for one.
	go func() {
		wsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			var msg ReverseMsg
			if err := wsConn.ReadJSON(&msg); err != nil {
				return
			}
		}
	}()

	sink := &mockSink{id: 1}
	rc.Subscribe(sink, "stall-key", 0, 0)

	// Give the goroutine a moment to reach FetchEvents → rpc() and park.
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	rc.Close()

	// The fetch goroutine should unwind quickly now that baseCtx is cancelled
	// and rpc()'s `<-c.done` branch triggers. We allow a generous slack but
	// must not approach the 5s RPC timeout.
	testhelper.Eventually(t, func() bool { return rc.baseCtx.Err() != nil }, 2*time.Second, "baseCtx should be cancelled after Close")

	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Errorf("Close→baseCtx.Done took %v, suggests goroutine not anchored to baseCtx", elapsed)
	}
}
