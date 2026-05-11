package server

import "testing"

// TestWSClient_SendRaw_ClosesOnDropThreshold: once cumulative drops cross
// wsDropThreshold, SendRaw closes c.done so writePump exits and the browser
// reconnects. Close is idempotent under concurrent drops (doneOnce). After
// the close, further SendRaw calls pick the <-c.done arm and return silently
// without bumping the counter, so c.dropped pins at exactly wsDropThreshold.
func TestWSClient_SendRaw_ClosesOnDropThreshold(t *testing.T) {
	t.Parallel()
	c := &wsClient{
		hub:  &Hub{},
		send: make(chan []byte, 1),
		done: make(chan struct{}),
	}
	c.send <- []byte("seed") // fill buffer; every SendRaw hits default.

	for i := 0; i < wsDropThreshold+1; i++ {
		c.SendRaw([]byte("x"))
	}
	if got := c.dropped.Load(); got != int64(wsDropThreshold) {
		t.Fatalf("c.dropped = %d, want %d", got, wsDropThreshold)
	}
	select {
	case <-c.done:
	default:
		t.Fatalf("c.done not closed after %d drops", wsDropThreshold+1)
	}
	c.SendRaw([]byte("x")) // must not panic (no double close).
}
