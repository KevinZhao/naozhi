// select_node_for_backend.go: backend-aware reverse-node routing.
//
// Every dispatch entry point (HTTP send, WS handleRemoteSend) calls
// selectNodeForBackend BEFORE forwarding so a node that lacks a backend's
// RequiredNodeCaps is rejected with a structured error instead of a remote
// spawn failure (docs/rfc/multi-backend.md §6).
package server

import (
	"errors"
	"fmt"

	"github.com/naozhi/naozhi/internal/backendid"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/node"
)

// maxBackendIDLen / isValidBackendID alias the shared backendid leaf package
// so every entry point enforces one length+charset contract (#1893).
const maxBackendIDLen = backendid.MaxLen

func isValidBackendID(s string) bool { return backendid.IsValid(s) }

// nodeLookup is the minimal surface selectNodeForBackend needs to find an
// active reverse-node connection by id (*nodeRegistry and hubNodeLookup).
type nodeLookup interface {
	NodeByID(id string) (node.Conn, bool)
}

// ErrUnknownBackend is returned when a caller asks selectNodeForBackend
// to route a backend ID that has no Profile in the registry. Sentinel so
// handlers can map it onto a 400.
var ErrUnknownBackend = errors.New("unknown backend")

// ErrNodeNotConnected is returned when the caller specified a target
// node that is not currently registered.
var ErrNodeNotConnected = errors.New("node not connected")

// ErrNodeMissingCap is returned when the target node is connected but
// did not advertise one of the backend's RequiredNodeCaps. Wrapped via %w
// so callers can errors.Is it and still surface the full message.
var ErrNodeMissingCap = errors.New("node missing required capability")

// selectNodeForBackend resolves (targetNode, backendID) into a routing
// decision. Three outcomes:
//
//   - (nc, nil):      forward to nc (a connected reverse node).
//   - (nil, nil):     dispatch locally; targetNode was empty / "local".
//   - (nil, err):     refuse to dispatch; surface err to the user.
//
// Local dispatch never needs a capability check. Empty backendID is
// treated as "router default" and passes with no cap requirement.
func selectNodeForBackend(lookup nodeLookup, targetNode, backendID string) (node.Conn, error) {
	if targetNode == "" || targetNode == "local" {
		return nil, nil
	}

	nc, ok := lookup.NodeByID(targetNode)
	if !ok {
		// %q: targetNode is attacker-controlled; guards log injection.
		return nil, fmt.Errorf("%w: %q", ErrNodeNotConnected, targetNode)
	}

	// Legacy session without a recorded Backend — no cap requirement.
	if backendID == "" {
		return nc, nil
	}
	profile, ok := backend.Get(backendID)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownBackend, backendID)
	}

	// First missing cap wins (slice order = registration order).
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

// ErrAccessProfileRemote is returned when a session bound to a non-default
// access profile is dispatched to a remote node. The access-profile env
// overlay (and any *_FILE secret) is host-local and never crosses the
// reverse-RPC wire, so a remote node would silently run the session on the
// wrong account; fail loud instead (RFC project-access-profile §4.5).
var ErrAccessProfileRemote = errors.New("access-profile session cannot be dispatched to a remote node (local-dispatch only)")

// accessProfileResolver is the minimal surface the remote-dispatch gate needs:
// resolve a session key to its access-profile ID. *session.KeyResolver
// satisfies it via AccessProfileForKey.
type accessProfileResolver interface {
	AccessProfileForKey(key string) string
}

// gateRemoteAccessProfile refuses remote dispatch for a key whose session
// resolves to a non-default access profile. targetNode == "" / "local" is
// always allowed (local dispatch is where the overlay works). A nil resolver
// (test harnesses without project wiring) or an empty profile is a no-op.
func gateRemoteAccessProfile(resolver accessProfileResolver, targetNode, key string) error {
	if targetNode == "" || targetNode == "local" || resolver == nil {
		return nil
	}
	if ap := resolver.AccessProfileForKey(key); ap != "" {
		return fmt.Errorf("%w: profile %q on node %q", ErrAccessProfileRemote, ap, targetNode)
	}
	return nil
}

// hubNodeLookup adapts the Hub's shared *nodeRegistry to the nodeLookup
// interface so handleRemoteSend can call selectNodeForBackend.
type hubNodeLookup struct{ h *Hub }

func (l hubNodeLookup) NodeByID(id string) (node.Conn, bool) {
	// Every by-ID read of the shared node table goes through Hub.lookupNode.
	return l.h.lookupNode(id)
}
