package server

import (
	"testing"
)

// TestHubSharesServerNodesState codifies the Server↔Hub shared-state contract
// flagged in R260528-ARCH-22 / #1381 in its post-G2 form (RFC
// docs/rfc/godstruct-extraction.md §2.2 / #2192): the Hub is not a fully
// self-contained sub-component — it deliberately shares Server's node
// registry so multi-node connection bookkeeping done on either side (reverse
// OnRegister/OnDeregister on the Server side, lookupNode / unregister fan-out
// / Shutdown close on the Hub side) observes one table under one lock.
//
// Pre-G2 this test asserted two things separately — same map header and same
// *sync.RWMutex — because Server carried a bare map + mutex and mirrored both
// into the Hub by pointer. The registry owns both now, so the invariant
// collapses to pointer identity on the single *nodeRegistry instance. A
// future refactor that hands the Hub its own registry (the #1381 "Hub
// independent struct" direction) fails loudly here instead of silently
// splitting the table and racing reverse registrations against WS lookups.
func TestHubSharesServerNodesState(t *testing.T) {
	s := newTestServer(&mockPlatform{})
	if s.hub == nil {
		t.Fatal("registerDashboard did not wire a Hub")
	}
	if s.nodes == nil {
		t.Fatal("Server.nodes registry is nil; NewWithOptions must always construct one")
	}
	if s.hub.nodes != s.nodes {
		t.Errorf("Hub.nodes = %p, want Server.nodes = %p (registry instance must be shared)",
			s.hub.nodes, s.nodes)
	}

	// Behavioural half of the contract: a write on the Server side (what the
	// reverse-node OnRegister hook does) is visible through the Hub's seam
	// without any extra plumbing, and vice versa for removal.
	stub := &fakeCapNode{id: "shared-node"}
	s.nodes.Add("shared-node", stub)
	if got, ok := s.hub.lookupNode("shared-node"); !ok || got != stub {
		t.Errorf("Hub.lookupNode after Server-side Add: ok=%v conn=%v, want the added stub", ok, got)
	}
	s.nodes.Remove("shared-node")
	if _, ok := s.hub.lookupNode("shared-node"); ok {
		t.Error("Hub.lookupNode still hits after Server-side Remove")
	}
}

// TestHubScratchPoolWiredFromServer asserts the other shared handle named in
// #1381: when Server owns a scratchPool it is the exact instance the Hub uses
// for ephemeral-session opts resolution. Skips when the bare test server has no
// pool (constructed only on the New()/Start() path), keeping the assertion
// honest rather than vacuous.
func TestHubScratchPoolWiredFromServer(t *testing.T) {
	s := newTestServer(&mockPlatform{})
	if s.scratchPool == nil {
		t.Skip("bare test server has no scratchPool wired")
	}
	if s.hub.scratchPool != s.scratchPool {
		t.Errorf("Hub.scratchPool = %p, want Server.scratchPool = %p (pool must be shared)",
			s.hub.scratchPool, s.scratchPool)
	}
}
