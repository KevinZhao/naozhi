// Package agentevents hosts the dashboard /api/sessions/agent_events +
// /api/sessions/tool_result endpoints. Phase 3d
// (server-split-phase4-design.md §6.5 Plan B) moved this from
// internal/server.
package agentevents

import "github.com/naozhi/naozhi/internal/dashboard/contracts"

// NodeAccessor aliases the shared dashboard contract (#2285). This package
// previously declared a 4-method subset (no NodesSnapshot); aliasing the full
// shape costs nothing because server's *nodeAccessor already implements it
// and this package has no NodeAccessor test doubles.
type NodeAccessor = contracts.NodeAccessor
