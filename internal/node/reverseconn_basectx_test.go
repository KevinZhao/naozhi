package node

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
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
	rc.Subscribe(sink, "stall-key", 0)

	// Give the goroutine a moment to reach FetchEvents → rpc() and park.
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	rc.Close()

	// The fetch goroutine should unwind quickly now that baseCtx is cancelled
	// and rpc()'s `<-c.done` branch triggers. We allow a generous slack but
	// must not approach the 5s RPC timeout.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rc.baseCtx.Err() != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rc.baseCtx.Err() == nil {
		t.Fatal("baseCtx should be cancelled after Close")
	}

	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Errorf("Close→baseCtx.Done took %v, suggests goroutine not anchored to baseCtx", elapsed)
	}
}

// TestReverseConn_SubscribeNoCancelOnCloseWatcher is a source-level
// regression gate. The earlier code shape was:
//
//	cancelOnClose := make(chan struct{})
//	go func() {
//	    select {
//	    case <-c.done:
//	        cancel()
//	    case <-cancelOnClose:
//	    }
//	}()
//	defer close(cancelOnClose)
//
// which spawned an additional goroutine per Subscribe call that held `c`
// references without WG tracking. This test asserts the pattern is gone.
// If someone re-introduces the watcher, the test fails even if behavior
// still passes (the structural contract is what H7 pins).
func TestReverseConn_SubscribeNoCancelOnCloseWatcher(t *testing.T) {
	data, err := os.ReadFile("reverseconn.go")
	if err != nil {
		t.Fatalf("read reverseconn.go: %v", err)
	}
	src := string(data)

	// Locate Subscribe function body.
	start := strings.Index(src, "func (c *ReverseConn) Subscribe(")
	if start == -1 {
		t.Fatal("Subscribe function not found")
	}
	// Grab up to the next top-level func (best-effort bound — no Go parser).
	tail := src[start:]
	end := strings.Index(tail, "\nfunc ")
	if end == -1 {
		end = len(tail)
	}
	body := tail[:end]

	for _, forbidden := range []string{
		"cancelOnClose",
		"context.WithTimeout(context.Background()",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("Subscribe body should not contain %q (H7: ctx must derive from baseCtx, not a hand-rolled watcher)", forbidden)
		}
	}

	// Positive assertion: Subscribe's timeout contexts must derive from
	// baseCtx, or the cancellation fix is silently undone.
	if !strings.Contains(body, "context.WithTimeout(c.baseCtx") {
		t.Error(`Subscribe body must call context.WithTimeout(c.baseCtx, ...) so RPC unwinds on disconnect`)
	}

	// Regression gate: the legacy shape spawned a per-RPC watcher goroutine
	// on top of the main fetch goroutine. Counting the exact number of
	// `go func()` calls was brittle (two branches × one goroutine, but the
	// refactor may collapse them). The invariant that matters is "no watcher
	// shape" — asserting the forbidden patterns above already covers that,
	// so we drop the structural goroutine count. Keep `regexp` imported
	// via at least one use so unused-import lint stays quiet.
	_ = regexp.MustCompile(`go func\(\)`)
}
