package server

import (
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// TestUnregister_RemoveClientFanOut: Hub.unregister tells every remote
// node to drop the client (#1356), so no node keeps a ghost subscription.
func TestUnregister_RemoveClientFanOut(t *testing.T) {
	const numNodes = 5

	hub, _ := newTestHub(t, "")
	t.Cleanup(hub.Shutdown)

	stubs := make([]*recordingFanoutNode, 0, numNodes)
	for i := 0; i < numNodes; i++ {
		s := &recordingFanoutNode{fakeCapNode: fakeCapNode{id: "fanout"}}
		stubs = append(stubs, s)
		// Distinct keys so the registry holds all N entries.
		hub.nodes.Add(stubKey(i), s)
	}

	c := &wsClient{}
	hub.register(c)
	hub.unregister(c)

	for i, s := range stubs {
		if got := s.calls.Load(); got != 1 {
			t.Errorf("stub %d: RemoveClient called %d times, want 1", i, got)
		}
	}
}

// TestUnregister_RemoveClientSingleNode pins that the fast path
// (len(nodes) == 1) still invokes RemoveClient exactly once. The
// single-node branch skips the goroutine spawn for the steady-state
// deployment where fan-out has no benefit.
func TestUnregister_RemoveClientSingleNode(t *testing.T) {
	hub, _ := newTestHub(t, "")
	t.Cleanup(hub.Shutdown)

	stub := &recordingFanoutNode{
		fakeCapNode: fakeCapNode{id: "single"},
	}
	hub.nodes.Add("single", stub)

	c := &wsClient{}
	hub.register(c)

	hub.unregister(c)

	if got := stub.calls.Load(); got != 1 {
		t.Errorf("single-node RemoveClient called %d times, want 1", got)
	}
}

// recordingFanoutNode embeds the full-surface fakeCapNode stub from
// select_node_for_backend_test.go and overrides only RemoveClient to
// record invocation counts.
type recordingFanoutNode struct {
	fakeCapNode
	calls atomic.Int64
}

func (r *recordingFanoutNode) RemoveClient(_ node.EventSink) {
	r.calls.Add(1)
}

func stubKey(i int) string {
	// Distinct map keys per stub so all entries survive in hub.nodes.
	const tmpl = "stub-fanout-x"
	b := []byte(tmpl)
	b[len(b)-1] = byte('0' + i)
	return string(b)
}
