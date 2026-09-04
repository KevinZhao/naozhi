// Package memory hosts the dashboard /api/memory/{slug} endpoint that reads
// CLAUDE.md memory files from per-project state dirs.
package memory

import "github.com/naozhi/naozhi/internal/dashboard/contracts"

// IPLimiter aliases the shared dashboard contract (#2285).
type IPLimiter = contracts.IPLimiter
