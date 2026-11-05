// select_node_for_backend.go: picking the node a remote send goes to and the
// backend it carries.
//
// Every dispatch entry point (HTTP send, WS handleRemoteSend) calls selectNode
// BEFORE forwarding, and remoteBackend for a send that picks a backend: the
// node's own manifest decides which backends it runs, not this binary's
// registry, so a pick the node does not offer is refused instead of dropped
// (docs/rfc/planner.md §15.5 ②).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/naozhi/naozhi/internal/backendid"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// maxBackendIDLen / isValidBackendID alias the shared backendid leaf package
// so every entry point enforces one length+charset contract (#1893).
const maxBackendIDLen = backendid.MaxLen

func isValidBackendID(s string) bool { return backendid.IsValid(s) }

// nodeLookup is the minimal surface selectNode needs to find an active node
// connection by id (*nodeRegistry and hubNodeLookup).
type nodeLookup interface {
	NodeByID(id string) (node.Conn, bool)
}

// ErrNodeNotConnected is returned when the caller specified a target
// node that is not currently registered.
var ErrNodeNotConnected = errors.New("node not connected")

// ErrNodeLacksBackend is returned when the target node's backend manifest
// does not list the picked backend, or cannot be read.
var ErrNodeLacksBackend = errors.New("远程节点未启用所选后端")

// selectNode resolves targetNode into a routing decision. Three outcomes:
//
//   - (nc, nil):      forward to nc (a connected node).
//   - (nil, nil):     dispatch locally; targetNode was empty / "local".
//   - (nil, err):     refuse to dispatch; surface err to the user.
func selectNode(lookup nodeLookup, targetNode string) (node.Conn, error) {
	if targetNode == "" || targetNode == "local" {
		return nil, nil
	}
	nc, ok := lookup.NodeByID(targetNode)
	if !ok {
		// %q: targetNode is attacker-controlled; guards log injection.
		return nil, fmt.Errorf("%w: %q", ErrNodeNotConnected, targetNode)
	}
	return nc, nil
}

// remoteBackend checks a backend pick against the node's manifest
// (fetch_backends, the payload its /api/cli/backends serves). "" is the
// node's default and needs no check. Whether the link can carry the pick is
// the Conn's own check (node.ErrSendBackendUnsupported). backendID has passed
// isValidBackendID, so it is safe in the error.
func remoteBackend(ctx context.Context, nc node.NodeFetcher, backendID string) error {
	if backendID == "" {
		return nil
	}
	raw, err := nc.FetchBackends(ctx)
	if err != nil {
		slog.Warn("remote send: node backend manifest unavailable", "backend", backendID, "err", osutil.SanitizeForLog(err.Error(), 512))
		return fmt.Errorf("%w：%s（读不到节点的后端清单）", ErrNodeLacksBackend, backendID)
	}
	var m struct {
		Backends []struct {
			ID string `json:"id"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("%w：%s（节点的后端清单无法解析）", ErrNodeLacksBackend, backendID)
	}
	for _, b := range m.Backends {
		if b.ID == backendID {
			return nil
		}
	}
	return fmt.Errorf("%w：%s", ErrNodeLacksBackend, backendID)
}

// sendPicksChecker is a Conn whose link may not carry a send's backend or
// resume pick (a ReverseConn to a node predating CapSendBackend); a direct
// HTTP node always carries them.
type sendPicksChecker interface {
	CheckSendPicks(node.SendRequest) error
}

var _ sendPicksChecker = (*node.ReverseConn)(nil)

// ErrAccessProfileRemote is returned when a session bound to a non-default
// access profile is dispatched to a remote node. The access-profile env
// overlay (and any *_FILE secret) is host-local and never crosses the
// reverse-RPC wire, so a remote node would silently run the session on the
// wrong account; fail loud instead (RFC project-access-profile §4.5).
var ErrAccessProfileRemote = errors.New("access-profile session cannot be dispatched to a remote node (local-dispatch only)")

// ErrLocalOnlySession is returned when a scratch: key is dispatched to a
// remote node. A scratch session exists only in the pool of the host that
// opened it, and its inherited access profile is invisible to the
// KeyResolver, so the profile gate below cannot vet it.
var ErrLocalOnlySession = errors.New("scratch sessions are local-only")

// accessProfileResolver is the minimal surface the remote-dispatch gate needs:
// resolve a session key to its access-profile ID. *session.KeyResolver
// satisfies it via AccessProfileForKey.
type accessProfileResolver interface {
	AccessProfileForKey(key string) string
}

// gateRemoteAccessProfile refuses remote dispatch for a scratch: key and for
// a key whose session resolves to a non-default access profile. targetNode ==
// "" / "local" is always allowed (local dispatch is where the overlay works).
// The scratch refusal needs no resolver; otherwise a nil resolver (test
// harnesses without project wiring) or an empty profile is a no-op.
func gateRemoteAccessProfile(resolver accessProfileResolver, targetNode, key string) error {
	if targetNode == "" || targetNode == "local" {
		return nil
	}
	if sessionkey.IsScratchKey(key) {
		return fmt.Errorf("%w: node %q", ErrLocalOnlySession, targetNode)
	}
	if resolver == nil {
		return nil
	}
	if ap := resolver.AccessProfileForKey(key); ap != "" {
		return fmt.Errorf("%w: profile %q on node %q", ErrAccessProfileRemote, ap, targetNode)
	}
	return nil
}

// lookupNode resolves a node ID to its Conn via the shared node registry; it
// is the Hub's only by-ID access to the node table. Callers MUST validate the
// ID with isValidNodeID first. It lived in wshub_send.go until #2551 — it is a
// node-table accessor, not part of the send pipeline, and the send chain's
// method count was misleading while it sat there.
func (h *Hub) lookupNode(id string) (node.Conn, bool) {
	return h.nodes.NodeByID(id)
}

// hubNodeLookup adapts the Hub's shared *nodeRegistry to the nodeLookup
// interface so handleRemoteSend can call selectNode.
type hubNodeLookup struct{ h *Hub }

func (l hubNodeLookup) NodeByID(id string) (node.Conn, bool) {
	// Every by-ID read of the shared node table goes through Hub.lookupNode.
	return l.h.lookupNode(id)
}
