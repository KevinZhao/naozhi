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
	// "disconnected" for known-but-not-connected nodes. Built under a single
	// RLock so the /health handler pays one lock acquisition instead of one
	// per node via repeated NodeByID calls. R20260616-PERF-003.
	NodesStatus() map[string]string
}

// nodeRegistry is the single owner of the multi-node connection table
// (RFC docs/rfc/godstruct-extraction.md §2.2 G2 / #2192).
//
// Before G2 the live `map[string]node.Conn`, its `sync.RWMutex` and the
// configured id→displayName map were three bare fields on Server, mirrored
// by pointer into Hub and wrapped again by a read-side nodeAccessor. Lock
// discipline lived in comments ("all nodes map access must use nodesMu"),
// so any new write site that forgot the mutex was a silent data race.
// Collapsing the three fields into this type makes the mutex private:
// every read and write of the table goes through a method that takes the
// lock, and Server and Hub share one *nodeRegistry instance instead of one
// map header plus one mutex pointer (hub_shared_state_test.go pins that).
//
// Locking: `mu` guards `conns` only. `known` is deliberately NOT under the
// lock on either side: it is written exclusively during Server construction
// (newNodeRegistry seeding + SetKnown for ReverseServer.AllNodes, both
// before OnRegister/OnDeregister are installed and before any goroutine
// can reach the registry) and by single-goroutine test fixtures, then
// treated as immutable. KnownNodes returns the backing map by reference,
// as the pre-G2 accessor did, because dashboard/session samples it once
// per request without taking the lock (handlers.go "immutable snapshot").
// Taking the lock only in SetKnown would suggest a runtime-safe write path
// that does not exist; the contract is "construction-time only", enforced
// by call-site discipline, not by the mutex.
type nodeRegistry struct {
	mu    sync.RWMutex
	conns map[string]node.Conn
	known map[string]string
}

// newNodeRegistry builds a registry seeded with the configured node table.
// nil is accepted and treated as an empty table. Every seeded node is also
// recorded in the known set under its DisplayName, mirroring the pre-G2
// Server ctor loop; nodes added later via Add (reverse registrations) are
// NOT auto-promoted to known — that stays a construction-time decision
// (SetKnown) so a rogue reverse connection cannot inject a display name.
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
// Construction-time only, before the registry is shared with any other
// goroutine — intentionally lock-free to match the lock-free KnownNodes
// reader (see the locking note on nodeRegistry). Never call at runtime.
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
// alloc(n), where n is the live count observed under the read lock. alloc
// runs while the lock is held so callers can size or borrow (sync.Pool) a
// buffer from the exact count without a second lock acquisition; it is not
// invoked when the table is empty, in which case nil is returned. This is
// the single-RLock shape Hub.unregister relied on before G2
// (R46-PERF-UNREGISTER-NODES-ALLOC / R249-PERF-6): the empty short-circuit
// skips the pool round-trip in single-node deployments.
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
// returning id → status with "disconnected" for nodes that are configured
// (known) but not currently connected. R20260616-PERF-003: the prior
// /health path called NodeByID per node, taking K RLock/RUnlock cycles for
// K nodes; this collapses that into one.
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
