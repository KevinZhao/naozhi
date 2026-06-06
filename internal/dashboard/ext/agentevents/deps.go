// Package agentevents hosts the dashboard /api/sessions/agent_events +
// /api/sessions/tool_result endpoints. Phase 3d
// (server-split-phase4-design.md §6.5 Plan B) moved this from
// internal/server.
package agentevents

import (
	"net/http"
	"strings"

	"github.com/naozhi/naozhi/internal/node"
)

// NodeAccessor is the subset of internal/server.NodeAccessor the agent
// events handlers use. server's *nodeAccessor satisfies this shape; we
// accept the interface so the sub-package doesn't reverse-import server.
type NodeAccessor interface {
	HasNodes() bool
	NodeByID(id string) (node.Conn, bool)
	LookupNode(w http.ResponseWriter, id string) (node.Conn, bool)
	KnownNodes() map[string]string
}

// isUnknownRPCMethodErr reports whether err comes from a remote node that
// doesn't yet implement an RPC method the dashboard called. Phase 3d
// duplicated from internal/server/dashboard_session.go (one-line helper)
// to keep the sub-package self-contained.
func isUnknownRPCMethodErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown method")
}
