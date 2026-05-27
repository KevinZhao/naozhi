// select_node_for_backend.go: backend-aware reverse-node routing.
//
// This file plugs the "kiro session + node that doesn't speak ACP" hole
// in the multi-backend RFC (docs/rfc/multi-backend.md §6 — backend-aware
// dispatch): before this file, dashboards / IM channels could pick any
// reverse node from the picker and naozhi would happily forward the send
// RPC; the remote shim would then fail to spawn the kiro CLI (no `kiro`
// binary, no `acp` capability) and the user saw a generic transport
// error.
//
// Now every dispatch entry point (HTTP /api/sessions/send, WS
// handleRemoteSend, future cron remote dispatch) calls
// Server.selectNodeForBackend BEFORE forwarding. The helper:
//  1. Resolves the backend.Profile to learn its RequiredNodeCaps.
//  2. If the caller specified a node, asserts every required cap is
//     advertised by that node. Missing caps surface as a structured
//     error with backend ID + node ID + offending cap, so dashboards
//     can render an actionable message instead of a generic 500.
//  3. Returns nil + nil error when targetNode is "" — the caller
//     treats nil as "dispatch locally" (existing behaviour).
//
// Empty backendID is treated as the default backend implicitly: if
// the registry has no profile for the empty string, RequiredNodeCaps
// is nil and the node passes (legacy single-backend deployments
// don't even register a "claude" id on stored sessions).
package server

import (
	"errors"
	"fmt"

	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/node"
)

// maxBackendIDLen mirrors send.go:263's per-request cap. Used by both
// HTTP and WS dispatch entry points so a hostile client can't blow up
// JSON / slog attrs with a 4 KB backend string.
//
// R20260527122801-ARCH-8 (#1314): aligned to 64 to match
// session/router_backend.go's maxBackendBytes. The previous 32-byte
// server cap rejected legal 33–64 byte backend IDs at the dashboard /
// HTTP-send boundary even though the router's own validateBackend
// (charset+length) accepted them — so a 60-byte backend stored in
// sessions.json could be routed through the cron path but not edited
// from the dashboard. Widening the server cap to 64 closes the
// asymmetry: both layers now share the same length contract. The DoS
// concern motivating the 32-byte cap is unchanged at 64 bytes (still
// 1/64 of the 4 KB JSON-attr inflation worst case).
const maxBackendIDLen = 64

// isValidBackendID reports whether s passes the per-request charset +
// length gate shared by HTTP /api/sessions/send and WS handleRemoteSend.
// Empty is allowed (treated as "router default" by selectNodeForBackend).
// PR #119 review fix — close the asymmetry where WS path forwarded
// unvalidated msg.Backend straight into error strings.
func isValidBackendID(s string) bool {
	if len(s) > maxBackendIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// nodeLookup is the minimal surface selectNodeForBackend needs to find
// an active reverse-node connection by id. Server's nodeAccess and
// Hub's hubNodeLookup adapter both satisfy this; defining the
// interface locally keeps the helper free of an upward dependency on
// either.
type nodeLookup interface {
	GetNode(id string) (node.Conn, bool)
}

// ErrUnknownBackend is returned when a caller asks selectNodeForBackend
// to route a backend ID that has no Profile in the registry. Sentinel
// (rather than fmt.Errorf) so handlers can map it onto a 400 status
// and a stable user-facing string. Mirrors the discriminated errors
// used elsewhere in this package for the workspace gate.
var ErrUnknownBackend = errors.New("unknown backend")

// ErrNodeNotConnected is returned when the caller specified a target
// node that is not currently registered. Distinct from "node lacks
// cap" so handlers can render different messages — "node offline,
// try later" vs "node X cannot host backend Y, pick a different
// node".
var ErrNodeNotConnected = errors.New("node not connected")

// ErrNodeMissingCap is returned when the target node is connected but
// did not advertise one of the backend's RequiredNodeCaps at register
// time. Wrapped (via %w) into a richer fmt.Errorf so callers can both
// errors.Is(err, ErrNodeMissingCap) AND surface the full
// "node X lacks cap Y for backend Z" string in error UI.
var ErrNodeMissingCap = errors.New("node missing required capability")

// selectNodeForBackend resolves (targetNode, backendID) into a routing
// decision. Three outcomes:
//
//   - (nc, nil):      forward to nc (a connected reverse node).
//   - (nil, nil):     dispatch locally; targetNode was empty / "local".
//   - (nil, err):     refuse to dispatch; surface err to the user.
//
// Empty targetNode short-circuits without consulting the registry —
// local dispatch never needs a capability check (the local naozhi
// process is the source of truth for what backends it can host, via
// its wrapper map). Likewise empty backendID is allowed and treated
// as "router default"; selectNodeForBackend trusts the caller's
// upstream resolution and only enforces caps when the registry
// actually knows about the id.
//
// Capability gating is done O(profile.RequiredNodeCaps × 1) — the cap
// map on NodeMeta is hash-lookup, so this is essentially constant time
// for the tag counts we expect (≤ 3).
func selectNodeForBackend(lookup nodeLookup, targetNode, backendID string) (node.Conn, error) {
	// Local / unspecified target — nothing to gate.
	if targetNode == "" || targetNode == "local" {
		return nil, nil
	}

	nc, ok := lookup.GetNode(targetNode)
	if !ok {
		// targetNode quoted via %q because handler logs treat the value
		// as attacker-controlled (dashboard form field). %q is safe
		// against newline / control-byte injection into slog attrs.
		return nil, fmt.Errorf("%w: %q", ErrNodeNotConnected, targetNode)
	}

	// Backend ID empty (legacy session where Backend wasn't recorded) or
	// unknown — fall through with no cap requirement. Returning the
	// connected node here preserves the historical behaviour for both
	// (a) deployments that pre-date multi-backend and (b) connected
	// peers that still satisfy the default backend.
	if backendID == "" {
		return nc, nil
	}
	profile, ok := backend.Get(backendID)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownBackend, backendID)
	}

	// Walk RequiredNodeCaps deterministically (the backend.Profile
	// stores a slice, not a set, so iteration order is the registration
	// order — fine for error reporting). The first missing cap wins;
	// reporting all missing caps would help a doctor view but isn't
	// needed for the dispatch reject path.
	meta := nc.Meta()
	for _, requiredCap := range profile.RequiredNodeCaps {
		if !meta.HasCap(requiredCap) {
			return nil, fmt.Errorf(
				"%w: node %q lacks capability %q for backend %q",
				ErrNodeMissingCap, targetNode, requiredCap, backendID)
		}
	}
	return nc, nil
}

// hubNodeLookup adapts Hub.nodes (with its shared mutex) to the
// nodeLookup interface so handleRemoteSend can call
// selectNodeForBackend without needing access to Server's nodeAccess.
// Locking is done per-call (read lock); the cost is negligible vs the
// downstream RPC.
type hubNodeLookup struct{ h *Hub }

func (l hubNodeLookup) GetNode(id string) (node.Conn, bool) {
	l.h.nodesMu.RLock()
	nc, ok := l.h.nodes[id]
	l.h.nodesMu.RUnlock()
	return nc, ok
}
