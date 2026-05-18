// Package node — registry.go: NodeMeta carries per-connection metadata
// (capabilities, display name, hostname, registration time) so server-side
// routing can answer "does this node support backend X?" without reaching
// into the connection's private fields.
//
// Reverse-connected nodes self-report Capabilities at register time
// (see ReverseMsg.Capabilities, populated in reverseserver.go). HTTPClient
// nodes always carry an empty capability set today; that matches their
// historical behaviour where the primary never knew which backends a
// pull-mode peer could host. Sprint 6b of the multi-backend RFC is the
// first consumer of this surface.
//
// Forward-compat: capability tags the local naozhi binary does not
// recognise are stored verbatim — selectNodeForBackend never asks about
// them. This keeps newer nodes interoperating with older primaries
// (advertised tags are observed but ignored), mirroring the policy in
// caps.go logUnknownCaps.
package node

import "time"

// NodeMeta describes a single connected (or recently-connected) reverse
// node. Constructed at register time; immutable for the life of the
// connection. Routing code consults HasCap to gate backend dispatch.
//
// Capabilities is the canonical lookup form (O(1)), populated from
// ReverseMsg.Capabilities at register time. We deliberately keep the
// raw slice off the public surface — every consumer wants a set
// membership check, not a list.
type NodeMeta struct {
	NodeID       string
	DisplayName  string
	Hostname     string
	Capabilities map[string]bool
	RegisteredAt time.Time
}

// HasCap reports whether the node advertised cap at register time.
// An empty cap is treated as "no requirement" and always satisfied —
// this matches backend.Profile.RequiredNodeCaps semantics where claude
// has nil/empty RequiredNodeCaps and must satisfy every node, including
// legacy peers that advertise no capabilities.
//
// nil-receiver safe so callers building a meta on the fly (tests, legacy
// HTTPClient peers without caps) need not allocate an empty map.
func (m *NodeMeta) HasCap(cap string) bool {
	if cap == "" {
		return true
	}
	if m == nil || m.Capabilities == nil {
		return false
	}
	return m.Capabilities[cap]
}

// capsFromSlice converts the wire form (ReverseMsg.Capabilities, []string)
// into the lookup form (map[string]bool). Empty / duplicate entries are
// silently coalesced. Returns nil for an empty input so callers can
// distinguish "node never advertised any caps" (legacy client) from
// "node advertised an empty list" — both legitimate, both treated the
// same by HasCap on lookup.
func capsFromSlice(caps []string) map[string]bool {
	if len(caps) == 0 {
		return nil
	}
	out := make(map[string]bool, len(caps))
	for _, c := range caps {
		if c == "" {
			continue
		}
		out[c] = true
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
