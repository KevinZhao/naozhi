// Package contracts holds the consumer-side dependency interfaces (and the
// tiny error predicate that travels with them) shared by the dashboard
// sub-packages under internal/dashboard.
//
// Background (#2285, R202606e-ARCH-2/3/4): when the dashboard handlers were
// split out of internal/server (server-split-phase4-design.md §6.5 Plan B),
// every sub-package re-declared the same narrow interfaces in its own deps.go
// so it would not reverse-import internal/server. IPLimiter ended up copied
// byte-for-byte in six packages and NodeAccessor in five, and the copies had
// already started to drift (agentevents lacked NodesSnapshot). This leaf
// package is the single declaration; sub-packages keep their local names via
// `type IPLimiter = contracts.IPLimiter` aliases so wiring code and test
// doubles are unaffected.
//
// Dependency direction stays one-way: contracts imports only internal/node
// (for node.Conn) and is imported by the dashboard sub-packages; it never
// imports internal/server. server's concrete *ipLimiter / *nodeAccessor
// satisfy these shapes structurally. TestDashboardContractsDeclaredOnce
// pins the single-declaration invariant.
package contracts

import (
	"net/http"
	"strings"

	"github.com/naozhi/naozhi/internal/node"
)

// IPLimiter is the subset of internal/server.ipLimiter the dashboard
// handlers use for per-IP rate limiting. Allow gates on a raw remote
// address; AllowRequest additionally honours the trusted-proxy header
// policy configured on the server-side limiter. Implementations may be
// nil-checked by callers; the interface itself carries no nil-safety.
type IPLimiter interface {
	Allow(remoteAddr string) bool
	AllowRequest(r *http.Request) bool
}

// NodeAccessor is the dashboard-facing subset of internal/server.NodeAccessor:
// thread-safe read access to the connected-node map plus the configured
// (possibly disconnected) node roster. It deliberately omits NodesStatus,
// which only the /health handler in internal/server consumes.
//
// project / session / agentevents alias this full shape. discovery and
// ext/cli keep a narrower local interface (2 and 1 methods respectively)
// because their existing test doubles implement only those methods; the
// contract test asserts every such narrow declaration is a strict subset of
// this one so the shapes cannot drift apart.
type NodeAccessor interface {
	HasNodes() bool
	NodesSnapshot() map[string]node.Conn
	NodeByID(id string) (node.Conn, bool)
	// LookupNode returns the node or writes an HTTP 400 JSON error envelope.
	LookupNode(w http.ResponseWriter, id string) (node.Conn, bool)
	KnownNodes() map[string]string // id → displayName, includes disconnected
}

// IsUnknownRPCMethodErr reports whether a remote-proxy error came from the
// peer node rejecting the RPC method name. That happens when the peer is
// running an older naozhi binary that predates a method the dashboard
// called (remove_session / interrupt_session / agent_events …) — surfacing a
// bespoke 409 lets the dashboard show a precise "upgrade the remote node"
// toast instead of a generic 502. The match is on error text because the
// reverse-RPC error is wrapped via fmt.Errorf in multiple layers and carries
// the literal "unknown method: " prefix from
// internal/upstream/connector.go's default switch branch.
func IsUnknownRPCMethodErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown method")
}
