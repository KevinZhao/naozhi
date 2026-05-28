// Package project hosts the dashboard endpoints for project list / config /
// favorites / planner-restart and the project-files I/O endpoints. Phase 2
// (server-split-phase4-design.md §6.5 Plan B) moved these from
// internal/server.
//
// Cross-cutting helpers below are duplicated locally rather than reverse-
// imported from internal/server, keeping the dependency direction one-way.
package project

import (
	"net/http"

	"github.com/naozhi/naozhi/internal/node"
)

// NodeAccessor is the subset of internal/server.NodeAccessor that this
// package needs. Defined locally as a small interface (per "accept
// interfaces" idiom) so server's *nodeAccessor (which already satisfies
// this shape) can be passed in via Deps without reverse-importing server.
type NodeAccessor interface {
	HasNodes() bool
	NodesSnapshot() map[string]node.Conn
	GetNode(id string) (node.Conn, bool)
	LookupNode(w http.ResponseWriter, id string) (node.Conn, bool)
	KnownNodes() map[string]string
}

// IPLimiter is the subset of internal/server.ipLimiter the project handlers
// use. server's *ipLimiter satisfies this shape; we accept the interface so
// the sub-package doesn't reverse-import server. Both Allow methods are
// nil-safe via the calling sites' explicit nil-checks.
type IPLimiter interface {
	Allow(remoteAddr string) bool
	AllowRequest(r *http.Request) bool
}

// withMaxBytes wraps r.Body with http.MaxBytesReader. Duplicated from
// internal/server/middleware.go (3-line helper) to keep this package
// self-contained. R20260527122801-SEC-1 (#1325) is enforced at every
// handler call site by passing httputil.MaxRequestBodyBytes as the cap.
func withMaxBytes(w http.ResponseWriter, r *http.Request, n int64) *http.Request {
	r.Body = http.MaxBytesReader(w, r.Body, n)
	return r
}
