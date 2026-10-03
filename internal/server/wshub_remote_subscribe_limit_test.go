package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// subscribeRecorder records the subscribe fields a remote node is handed.
type subscribeRecorder struct {
	fakeCapNode
	calls int
	after int64
	limit int
}

func (r *subscribeRecorder) Subscribe(_ node.EventSink, _ string, after int64, limit int) {
	r.calls++
	r.after, r.limit = after, limit
}

// TestHandleRemoteSubscribe_ForwardsAfterAndLimit: the browser's page-size
// hint reaches the node conn with its watermark, so a remote opening page is
// sized like a local one instead of replaying the whole log.
func TestHandleRemoteSubscribe_ForwardsAfterAndLimit(t *testing.T) {
	rec := &subscribeRecorder{fakeCapNode: fakeCapNode{id: "node1"}}
	hub := newTestHubWithNodes(t, map[string]node.Conn{"node1": rec})
	t.Cleanup(hub.Shutdown)
	c, _ := newCapturedClient(t, hub)

	hub.handleSubscribe(c, node.ClientMsg{Type: "subscribe", Key: "node1:p2p:carol", Node: "node1", After: 1234, Limit: 100})

	if rec.calls != 1 || rec.after != 1234 || rec.limit != 100 {
		t.Fatalf("node Subscribe calls=%d after=%d limit=%d, want 1 call with after=1234 limit=100", rec.calls, rec.after, rec.limit)
	}
}
