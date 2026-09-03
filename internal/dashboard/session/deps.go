// Package session hosts the dashboard session-management endpoints
// (/api/sessions list/events/delete/resume/interrupt + label PATCH).
// Phase 3e (server-split-phase4-design.md §6.5 Plan B) moved this from
// internal/server.
package session

import "github.com/naozhi/naozhi/internal/dashboard/contracts"

// NodeAccessor aliases the shared dashboard contract (#2285); server's
// *nodeAccessor is injected via Deps without a reverse import.
type NodeAccessor = contracts.NodeAccessor

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
