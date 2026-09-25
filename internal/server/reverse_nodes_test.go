package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// TestAttachReverseNodeServer_WiresNodeLifecycle pins the reverse-connection
// wiring that had no test before it moved out of server.go (#2792): dropping
// the known-node seeding or the deregister cleanup left the whole package
// green. Configured nodes must show up as known before they ever connect, and
// a deregistered node's live connection must leave the registry.
func TestAttachReverseNodeServer_WiresNodeLifecycle(t *testing.T) {
	s := newTestServer(&mockPlatform{})
	rs := node.NewReverseServer(map[string]node.ReverseNodeAuth{
		"n1": {Token: "tok-n1", DisplayName: "Node One"},
	}, false)

	s.attachReverseNodeServer(rs)

	if s.reverseNodeServer != rs {
		t.Fatal("reverse server not retained on the Server")
	}
	if got := s.nodes.KnownNodes()["n1"]; got != "Node One" {
		t.Errorf("known node n1 = %q, want %q — a configured node must be listed before it connects", got, "Node One")
	}
	if rs.OnRegister == nil || rs.OnDeregister == nil {
		t.Fatal("lifecycle callbacks not installed")
	}

	// A node that registered and then drops must leave the live registry.
	s.nodes.Add("n1", &fakeCapNode{id: "n1"})
	if _, ok := s.nodes.NodeByID("n1"); !ok {
		t.Fatal("precondition: n1 live")
	}
	rs.OnDeregister("n1")
	if _, ok := s.nodes.NodeByID("n1"); ok {
		t.Error("deregistered node still in the live registry")
	}
	if st := s.nodes.NodesStatus()["n1"]; st != "disconnected" {
		t.Errorf("n1 status after deregister = %q, want disconnected (still known, no longer live)", st)
	}
}

// TestAttachReverseNodeServer_NilIsNoop: most deployments configure no reverse
// server at all.
func TestAttachReverseNodeServer_NilIsNoop(t *testing.T) {
	s := newTestServer(&mockPlatform{})
	s.attachReverseNodeServer(nil)
	if s.reverseNodeServer != nil {
		t.Error("nil reverse server must leave the field nil")
	}
}
