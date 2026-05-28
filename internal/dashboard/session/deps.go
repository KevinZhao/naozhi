// Package session hosts the dashboard session-management endpoints
// (/api/sessions list/events/delete/resume/interrupt + label PATCH).
// Phase 3e (server-split-phase4-design.md §6.5 Plan B) moved this from
// internal/server.
package session

import (
	"net/http"
	"net/url"

	"github.com/naozhi/naozhi/internal/node"
)

// NodeAccessor is the subset of internal/server.NodeAccessor the session
// handlers use. server's *nodeAccessor satisfies this shape; we accept
// the interface so the sub-package doesn't reverse-import server.
type NodeAccessor interface {
	HasNodes() bool
	NodesSnapshot() map[string]node.Conn
	GetNode(id string) (node.Conn, bool)
	LookupNode(w http.ResponseWriter, id string) (node.Conn, bool)
	KnownNodes() map[string]string
}

// strOrFallback is a small map[string]any helper duplicated from
// internal/server/dashboard.go. Used by HandleEvents/HandleList to read
// optional string keys with a fallback. Phase 3e: kept local rather than
// reverse-importing server.
func strOrFallback(m map[string]any, key, fallback string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	v, _ := m[fallback].(string)
	return v
}

// redactGitRemoteURL strips embedded userinfo (user:password@) from a git
// remote URL. Phase 3e duplicated from internal/server/project_api.go's
// redactGitRemoteURL — once Phase 2 (PR #1437) merges, the canonical home
// becomes dashproject.RedactGitRemoteURL and this helper can be removed
// in favour of an import. Until then, keeping a local copy avoids a
// cross-PR circular dependency.
func redactGitRemoteURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return raw
	}
	if u.User != nil {
		u.User = nil
	}
	return u.String()
}
