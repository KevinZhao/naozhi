package server

import (
	"reflect"
	"testing"
)

// TestHubSharesServerNodesState codifies the Server↔Hub shared-state contract
// flagged in R260528-ARCH-22 / #1381: the Hub is not a fully self-contained
// sub-component — it deliberately shares Server's nodes map and the *sync.RWMutex
// guarding it so multi-node connection bookkeeping done on either side stays
// consistent under one lock. server.go documents this in prose ("shared with
// Hub.nodesMu"); this test turns that prose into an enforced invariant so a
// future refactor that gives the Hub its own mutex/map (the issue's proposed
// "Hub independent struct" direction) fails loudly here instead of silently
// splitting the lock and racing the nodes map.
func TestHubSharesServerNodesState(t *testing.T) {
	s := newTestServer(&mockPlatform{})
	if s.hub == nil {
		t.Fatal("registerDashboard did not wire a Hub")
	}

	// nodesMu must be the *same* mutex instance, not a copy: the Hub stores a
	// *sync.RWMutex pointing at Server.nodesMu.
	if s.hub.nodesMu != &s.nodesMu {
		t.Errorf("Hub.nodesMu = %p, want &Server.nodesMu = %p (lock must be shared)",
			s.hub.nodesMu, &s.nodesMu)
	}

	// nodes must be the same backing map header so writes on one side are
	// visible on the other.
	if reflect.ValueOf(s.hub.nodes).Pointer() != reflect.ValueOf(s.nodes).Pointer() {
		t.Error("Hub.nodes and Server.nodes are not the same map instance")
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
