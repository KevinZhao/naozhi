// Package contracts holds the consumer-side dependency interfaces (and the
// error predicate that travels with them) shared by the dashboard
// sub-packages, so each does not re-declare drifting copies to avoid
// reverse-importing internal/server (#2285); sub-packages alias them locally.
//
// contracts imports only internal/node, never internal/server; server's
// concrete types satisfy these shapes. TestDashboardContractsDeclaredOnce
// pins the single-declaration invariant.
package contracts

import (
	"net/http"
	"strings"

	"github.com/naozhi/naozhi/internal/node"
)

// IPLimiter is the per-IP rate limiter subset the dashboard handlers use.
// Allow gates on a raw remote address; AllowRequest additionally honours the
// trusted-proxy header policy. The interface carries no nil-safety.
type IPLimiter interface {
	Allow(remoteAddr string) bool
	AllowRequest(r *http.Request) bool
}

// NodeAccessor is the dashboard-facing subset of internal/server.NodeAccessor:
// thread-safe read access to the connected-node map plus the configured
// (possibly disconnected) roster. NodesStatus is omitted (only /health uses it).
// discovery and ext/cli keep narrower local interfaces for their test doubles;
// the contract test asserts each is a strict subset of this one.
type NodeAccessor interface {
	HasNodes() bool
	NodesSnapshot() map[string]node.Conn
	NodeByID(id string) (node.Conn, bool)
	// LookupNode returns the node or writes an HTTP 400 JSON error envelope.
	LookupNode(w http.ResponseWriter, id string) (node.Conn, bool)
	KnownNodes() map[string]string // id → displayName, includes disconnected
}

// IsUnknownRPCMethodErr reports whether a remote-proxy error came from the
// peer rejecting the RPC method name (older peer binary), so the dashboard
// can show an "upgrade the remote node" toast instead of a generic 502. Text
// match because the error is fmt.Errorf-wrapped through several layers.
func IsUnknownRPCMethodErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown method")
}
