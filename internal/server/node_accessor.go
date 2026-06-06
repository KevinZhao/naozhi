package server

import (
	"net/http"
	"sync"

	"github.com/naozhi/naozhi/internal/node"
)

// NodeAccessor abstracts thread-safe access to the nodes map,
// decoupling handler groups from the raw nodesMu/nodes/knownNodes fields.
type NodeAccessor interface {
	HasNodes() bool
	NodesSnapshot() map[string]node.Conn
	NodeByID(id string) (node.Conn, bool)
	// LookupNode returns the node or writes an HTTP 400 error.
	LookupNode(w http.ResponseWriter, id string) (node.Conn, bool)
	KnownNodes() map[string]string // id → displayName, includes disconnected
}

// nodeAccessor implements NodeAccessor using Server's shared mutex and maps.
type nodeAccessor struct {
	mu         *sync.RWMutex
	nodes      map[string]node.Conn
	knownNodes map[string]string
}

func newNodeAccessor(mu *sync.RWMutex, nodes map[string]node.Conn, knownNodes map[string]string) *nodeAccessor {
	return &nodeAccessor{mu: mu, nodes: nodes, knownNodes: knownNodes}
}

func (a *nodeAccessor) HasNodes() bool {
	a.mu.RLock()
	n := len(a.nodes)
	a.mu.RUnlock()
	return n > 0
}

func (a *nodeAccessor) NodesSnapshot() map[string]node.Conn {
	a.mu.RLock()
	cp := make(map[string]node.Conn, len(a.nodes))
	for k, v := range a.nodes {
		cp[k] = v
	}
	a.mu.RUnlock()
	return cp
}

func (a *nodeAccessor) NodeByID(id string) (node.Conn, bool) {
	a.mu.RLock()
	nc, ok := a.nodes[id]
	a.mu.RUnlock()
	return nc, ok
}

// maxNodeIDBytes caps the accepted node ID size. Legitimate IDs are
// configured display names (typically 8–32 bytes); 64 bytes is wide enough
// for any realistic deployment. Without this cap an authenticated caller
// can post a multi-KB `node` value that lands in slog.Warn attrs and
// amplifies into megabytes of log output under sustained load.
const maxNodeIDBytes = 64

// isValidNodeID enforces the display-name allowlist used by all node IDs.
// Restricting to [a-zA-Z0-9._-] rules out log-injection characters (\n,
// ANSI escapes, Unicode RTL overrides) that would otherwise flow into
// slog.Warn attrs downstream of LookupNode. The character set mirrors the
// backend-id allowlist in send.go.
func isValidNodeID(id string) bool {
	if id == "" {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// LookupNode error replies use the unified errEnvelope JSON shape (errResp,
// R247-ARCH-3 / #612 / #451) rather than text/plain http.Error. Every caller
// is a dashboard JSON API handler (discovery / project / session /
// agentevents) whose front-end reads `body.error`; emitting JSON with a
// stable `code` lets the UI branch and localize without parsing English copy.
func (a *nodeAccessor) LookupNode(w http.ResponseWriter, id string) (node.Conn, bool) {
	if len(id) > maxNodeIDBytes {
		errResp(w, http.StatusBadRequest, "node_id_too_long", "node id too long")
		return nil, false
	}
	if !isValidNodeID(id) {
		errResp(w, http.StatusBadRequest, "node_id_invalid", "invalid node id")
		return nil, false
	}
	nc, ok := a.NodeByID(id)
	if !ok {
		errResp(w, http.StatusBadRequest, "node_unknown", "unknown node")
		return nil, false
	}
	return nc, true
}

// KnownNodes returns all configured node IDs and display names, including disconnected nodes.
// The returned map is immutable after Server construction — safe to read without locking.
func (a *nodeAccessor) KnownNodes() map[string]string {
	return a.knownNodes
}
