// Package session hosts the dashboard session-management endpoints
// (/api/sessions list/events/delete/resume/interrupt + label PATCH).
package session

import "github.com/naozhi/naozhi/internal/dashboard/contracts"

// NodeAccessor aliases the shared dashboard contract (#2285); server's
// *nodeAccessor is injected via Deps without a reverse import.
type NodeAccessor = contracts.NodeAccessor

// strOrFallback reads an optional string key from a decoded map, trying key
// then fallback. Kept local rather than reverse-importing server.
func strOrFallback(m map[string]any, key, fallback string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	v, _ := m[fallback].(string)
	return v
}
