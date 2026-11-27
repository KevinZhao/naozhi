package server

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// TestUnregister_RemoveClientFanOut: Hub.unregister tells every remote
// node to drop the client (#1356), so no node keeps a ghost subscription.
// One node takes the no-goroutine fast path; several fan out.
func TestUnregister_RemoveClientFanOut(t *testing.T) {
	for _, numNodes := range []int{1, 5} {
		t.Run(fmt.Sprint(numNodes), func(t *testing.T) {
			hub, _ := newTestHub(t, "")
			t.Cleanup(hub.Shutdown)

			stubs := make([]*recordingFanoutNode, 0, numNodes)
			for i := 0; i < numNodes; i++ {
				s := &recordingFanoutNode{fakeCapNode: fakeCapNode{id: "fanout"}}
				stubs = append(stubs, s)
				hub.nodes.Add(fmt.Sprintf("stub-%d", i), s)
			}

			c := &wsClient{}
			hub.register(c)
			hub.unregister(c)

			for i, s := range stubs {
				if got := s.calls.Load(); got != 1 {
					t.Errorf("stub %d: RemoveClient called %d times, want 1", i, got)
				}
			}
		})
	}
}

// recordingFanoutNode embeds the full-surface fakeCapNode stub and overrides
// only RemoveClient to record invocation counts.
type recordingFanoutNode struct {
	fakeCapNode
	calls atomic.Int64
}

func (r *recordingFanoutNode) RemoveClient(_ node.EventSink) {
	r.calls.Add(1)
}
