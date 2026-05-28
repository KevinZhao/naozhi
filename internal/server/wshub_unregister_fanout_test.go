package server

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
)

// TestUnregister_RemoveClientFanOutParallel pins R260528-PERF-11 (#1356):
// Hub.unregister now fans RemoveClient across nodes in parallel so a
// 5-node deployment pays max(per-node-RTT) instead of sum. We verify the
// contract by installing N stub nodes whose RemoveClient blocks for a
// fixed sleep — if the loop is still serial the wall-clock equals
// N×sleep; the parallel shape finishes in roughly the single-call wall
// time. Each stub also records its invocation count so we assert the
// call lands on every node (parallelism must not silently drop any).
func TestUnregister_RemoveClientFanOutParallel(t *testing.T) {
	const numNodes = 5
	const perCallSleep = 80 * time.Millisecond

	hub, _ := newTestHub("")
	t.Cleanup(hub.Shutdown)

	stubs := make([]*recordingFanoutNode, 0, numNodes)
	hub.nodesMu.Lock()
	if hub.nodes == nil {
		hub.nodes = map[string]node.Conn{}
	}
	for i := 0; i < numNodes; i++ {
		s := &recordingFanoutNode{
			fakeCapNode: fakeCapNode{id: "fanout"},
			sleep:       perCallSleep,
		}
		stubs = append(stubs, s)
		// Distinct keys so the map holds all N entries.
		hub.nodes[stubKey(i)] = s
	}
	hub.nodesMu.Unlock()

	c := &wsClient{}
	hub.mu.Lock()
	if hub.clients == nil {
		hub.clients = map[*wsClient]struct{}{}
	}
	hub.clients[c] = struct{}{}
	hub.mu.Unlock()

	// Parallel: max(per-node-RTT) ≈ perCallSleep.
	// Serial: N × perCallSleep = 400ms.
	// Allow 3× slack on the parallel path to absorb scheduler jitter on a
	// loaded CI box; that is still well below the serial budget.
	const slackFactor = 3
	deadline := time.Duration(slackFactor) * perCallSleep

	start := time.Now()
	hub.unregister(c)
	elapsed := time.Since(start)

	if elapsed > deadline {
		t.Fatalf("unregister took %v, want <= %v (serial fan-out regressed: %d nodes × %v)",
			elapsed, deadline, numNodes, perCallSleep)
	}

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
	hub, _ := newTestHub("")
	t.Cleanup(hub.Shutdown)

	stub := &recordingFanoutNode{
		fakeCapNode: fakeCapNode{id: "single"},
	}
	hub.nodesMu.Lock()
	if hub.nodes == nil {
		hub.nodes = map[string]node.Conn{}
	}
	hub.nodes["single"] = stub
	hub.nodesMu.Unlock()

	c := &wsClient{}
	hub.mu.Lock()
	if hub.clients == nil {
		hub.clients = map[*wsClient]struct{}{}
	}
	hub.clients[c] = struct{}{}
	hub.mu.Unlock()

	hub.unregister(c)

	if got := stub.calls.Load(); got != 1 {
		t.Errorf("single-node RemoveClient called %d times, want 1", got)
	}
}

// recordingFanoutNode embeds the full-surface fakeCapNode stub from
// select_node_for_backend_test.go and overrides only RemoveClient so
// the parallel-fan-out timing test can both record invocation counts
// and simulate per-node RTT via the configurable sleep.
type recordingFanoutNode struct {
	fakeCapNode
	calls atomic.Int64
	sleep time.Duration
}

func (r *recordingFanoutNode) RemoveClient(_ node.EventSink) {
	if r.sleep > 0 {
		time.Sleep(r.sleep)
	}
	r.calls.Add(1)
}

func stubKey(i int) string {
	// Distinct map keys per stub so all entries survive in hub.nodes.
	const tmpl = "stub-fanout-x"
	b := []byte(tmpl)
	b[len(b)-1] = byte('0' + i)
	return string(b)
}
