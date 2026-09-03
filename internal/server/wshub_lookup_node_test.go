package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// TestHub_LookupNode pins ARCH4 (#384): the single by-ID read of the
// Server-shared node registry is funnelled through Hub.lookupNode. A hit returns
// the registered Conn; a miss returns (nil, false). hubNodeLookup.GetNode and
// the WS remote interrupt/subscribe/unsubscribe paths all defer to this helper,
// so locking the behaviour here guards every caller.
func TestHub_LookupNode(t *testing.T) {
	t.Parallel()

	router := session.NewRouter(session.RouterConfig{})
	guard := session.NewGuard()
	nodes := newNodeRegistry(map[string]node.Conn{
		"node-a": &fakeCapNode{id: "node-a"},
	})

	hub := NewHub(HubOptions{
		Router: router,
		Guard:  guard,
		Nodes:  nodes,
	})

	got, ok := hub.lookupNode("node-a")
	if !ok {
		t.Fatal("lookupNode(node-a) miss; want hit")
	}
	if got.NodeID() != "node-a" {
		t.Fatalf("lookupNode returned NodeID %q, want node-a", got.NodeID())
	}

	if _, ok := hub.lookupNode("absent"); ok {
		t.Fatal("lookupNode(absent) hit; want miss")
	}

	// hubNodeLookup.NodeByID must delegate to the same helper.
	if _, ok := (hubNodeLookup{h: hub}).NodeByID("node-a"); !ok {
		t.Fatal("hubNodeLookup.NodeByID(node-a) miss; want hit")
	}
}
