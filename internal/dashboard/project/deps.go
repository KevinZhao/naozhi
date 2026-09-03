// Package project hosts the dashboard endpoints for project list / config /
// favorites / planner-restart and the project-files I/O endpoints. Phase 2
// (server-split-phase4-design.md §6.5 Plan B) moved these from
// internal/server.
//
// Cross-cutting dependencies are accepted as the shared interfaces in
// internal/dashboard/contracts (#2285) rather than reverse-imported from
// internal/server, keeping the dependency direction one-way.
package project

import "github.com/naozhi/naozhi/internal/dashboard/contracts"

// NodeAccessor aliases the shared dashboard contract so server's
// *nodeAccessor can be passed in via Deps without reverse-importing server.
type NodeAccessor = contracts.NodeAccessor

// IPLimiter aliases the shared dashboard contract. Both Allow methods are
// nil-safe via the calling sites' explicit nil-checks.
type IPLimiter = contracts.IPLimiter
