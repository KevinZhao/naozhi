// Package memory hosts the dashboard /api/memory/{slug} endpoint that reads
// CLAUDE.md memory files from per-project state dirs. Phase 3c
// (server-split-phase4-design.md §6.5 Plan B) moved this from
// internal/server.
package memory

import "github.com/naozhi/naozhi/internal/dashboard/contracts"

// IPLimiter aliases the shared dashboard contract (#2285); server's
// *ipLimiter is injected without a reverse import.
type IPLimiter = contracts.IPLimiter
