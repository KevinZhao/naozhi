// Package memory hosts the dashboard /api/memory/{slug} endpoint that reads
// CLAUDE.md memory files from per-project state dirs. Phase 3c
// (server-split-phase4-design.md §6.5 Plan B) moved this from
// internal/server.
package memory

import "net/http"

// IPLimiter is the subset of internal/server.ipLimiter the memory handler
// uses. server's *ipLimiter satisfies this shape; we accept the interface
// so the sub-package doesn't reverse-import server.
type IPLimiter interface {
	Allow(remoteAddr string) bool
	AllowRequest(r *http.Request) bool
}
