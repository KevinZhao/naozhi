package node

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestEnsureConnected_WaiterSeesClosed verifies the waiter re-lock branch in
// ensureConnected returns "relay closed" when Close() raced with an in-flight
// dial and tore down the relay while a conn was still stored. Without the
// r.closed guard the waiter would observe r.conn != nil and hand the caller a
// connection on a relay being shut down. [R202606f-GO-002]
func TestEnsureConnected_WaiterSeesClosed(t *testing.T) {
	r := &wsRelay{}

	// Enter the waiter branch: not closed, no conn yet, a dial in progress.
	ch := make(chan struct{})
	r.mu.Lock()
	r.connReady = ch
	r.mu.Unlock()

	errCh := make(chan error, 1)
	go func() { errCh <- r.ensureConnected() }()

	// Let the waiter reach <-ch (it released r.mu before blocking).
	// Poll the lock to confirm the waiter is parked on the channel rather
	// than holding the mutex.
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		parked := r.connReady == ch
		r.mu.Unlock()
		if parked || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Simulate Close() winning the race during the dial: mark closed and
	// (as the racing connect could have) leave a conn stored.
	r.mu.Lock()
	r.closed = true
	r.conn = &websocket.Conn{} // sentinel; never used
	r.mu.Unlock()
	close(ch) // unblock the waiter

	select {
	case err := <-errCh:
		if err == nil || err.Error() != "relay closed" {
			t.Fatalf("waiter on closed relay: got err %v, want \"relay closed\"", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ensureConnected waiter did not return")
	}
}
