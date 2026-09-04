package server

import (
	"net/http"
	"sync"

	"github.com/naozhi/naozhi/internal/node"
)

// NodeAccessor abstracts thread-safe access to the nodes table, decoupling
// handler groups from the registry's internals. *nodeRegistry is the only
// implementation; the dashboard sub-packages (discovery / project / session /
// agentevents / cli) each declare a structural subset of this interface.
type NodeAccessor interface {
	HasNodes() bool
	NodesSnapshot() map[string]node.Conn
	NodeByID(id string) (node.Conn, bool)
	// LookupNode returns the node or writes an HTTP 400 error.
	LookupNode(w http.ResponseWriter, id string) (node.Conn, bool)
	KnownNodes() map[string]string // id → displayName, includes disconnected
	// NodesStatus returns id → connection status for every known node,
	// "disconnected" for known-but-not-connected nodes; built under one RLock.
	NodesStatus() map[string]string
}

// nodeRegistry is the single owner of the multi-node connection table;
// Server and Hub share one instance (hub_shared_state_test.go pins that).
//
// Locking: `mu` guards `conns` only. `known` is deliberately NOT under the
// lock: it is written only during Server construction (newNodeRegistry
// seeding + SetKnown, before any goroutine can reach the registry) and then
// treated as immutable; KnownNodes returns it by reference and callers read
// it without locking. The contract is enforced by call-site discipline.
type nodeRegistry struct {
	mu    sync.RWMutex
	conns map[string]node.Conn
	known map[string]string
}

// newNodeRegistry builds a registry seeded with the configured node table
// (nil = empty). Seeded nodes are also recorded as known; nodes added later
// via Add are NOT auto-promoted so a rogue reverse connection cannot inject
// a display name.
func newNodeRegistry(seed map[string]node.Conn) *nodeRegistry {
	conns := make(map[string]node.Conn, len(seed))
	known := make(map[string]string, len(seed))
	for id, nc := range seed {
		conns[id] = nc
		known[id] = nc.DisplayName()
	}
	return &nodeRegistry{conns: conns, known: known}
}

// Add registers (or replaces) the live connection for id.
func (r *nodeRegistry) Add(id string, nc node.Conn) {
	r.mu.Lock()
	r.conns[id] = nc
	r.mu.Unlock()
}

// Remove drops the live connection for id. Unknown ids are a no-op.
func (r *nodeRegistry) Remove(id string) {
	r.mu.Lock()
	delete(r.conns, id)
	r.mu.Unlock()
}

// SetKnown records a configured node's display name so it shows up as
// "disconnected" in NodesStatus / KnownNodes even before it connects.
// Construction-time only; intentionally lock-free. Never call at runtime.
func (r *nodeRegistry) SetKnown(id, displayName string) {
	r.known[id] = displayName
}

// Len returns the number of live connections.
func (r *nodeRegistry) Len() int {
	r.mu.RLock()
	n := len(r.conns)
	r.mu.RUnlock()
	return n
}

func (r *nodeRegistry) HasNodes() bool { return r.Len() > 0 }

func (r *nodeRegistry) NodesSnapshot() map[string]node.Conn {
	r.mu.RLock()
	cp := make(map[string]node.Conn, len(r.conns))
	for k, v := range r.conns {
		cp[k] = v
	}
	r.mu.RUnlock()
	return cp
}

func (r *nodeRegistry) NodeByID(id string) (node.Conn, bool) {
	r.mu.RLock()
	nc, ok := r.conns[id]
	r.mu.RUnlock()
	return nc, ok
}

// Conns returns a freshly allocated slice of every live connection.
func (r *nodeRegistry) Conns() []node.Conn {
	return r.appendConns(func(n int) []node.Conn { return make([]node.Conn, 0, n) })
}

// appendConns snapshots every live connection into the slice returned by
// alloc(n), n being the live count observed under the read lock. alloc runs
// under the lock so callers can size or borrow (sync.Pool) a buffer without
// a second acquisition; it is not invoked when the table is empty (nil returned).
func (r *nodeRegistry) appendConns(alloc func(n int) []node.Conn) []node.Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := len(r.conns)
	if n == 0 {
		return nil
	}
	out := alloc(n)
	for _, nc := range r.conns {
		out = append(out, nc)
	}
	return out
}

// maxNodeIDBytes caps the accepted node ID size so a multi-KB `node` value
// cannot amplify into log output via slog.Warn attrs.
const maxNodeIDBytes = 64

// isValidNodeID enforces the [a-zA-Z0-9._-] allowlist for node IDs, ruling
// out log-injection characters (\n, ANSI escapes, RTL overrides).
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

// LookupNode error replies use the errResp JSON envelope with a stable
// `code` so the dashboard can branch/localize without parsing English copy.
func (r *nodeRegistry) LookupNode(w http.ResponseWriter, id string) (node.Conn, bool) {
	if len(id) > maxNodeIDBytes {
		errResp(w, http.StatusBadRequest, "node_id_too_long", "node id too long")
		return nil, false
	}
	if !isValidNodeID(id) {
		errResp(w, http.StatusBadRequest, "node_id_invalid", "invalid node id")
		return nil, false
	}
	nc, ok := r.NodeByID(id)
	if !ok {
		errResp(w, http.StatusBadRequest, "node_unknown", "unknown node")
		return nil, false
	}
	return nc, true
}

// KnownNodes returns all configured node IDs and display names, including
// disconnected nodes. The returned map is immutable after Server
// construction — safe to read without locking (see nodeRegistry doc).
func (r *nodeRegistry) KnownNodes() map[string]string {
	return r.known
}

// NodesStatus snapshots the status of every known node in a single RLock,
// returning id → status with "disconnected" for known-but-unconnected nodes.
func (r *nodeRegistry) NodesStatus() map[string]string {
	out := make(map[string]string, len(r.known))
	r.mu.RLock()
	for id := range r.known {
		if nc, ok := r.conns[id]; ok {
			out[id] = nc.Status()
		} else {
			out[id] = "disconnected"
		}
	}
	r.mu.RUnlock()
	return out
}
