// Package workflows hosts the dashboard /api/sessions/workflow endpoint: one
// background workflow run of a session, as the board publishes it (RFC
// docs/rfc/workflow-dashboard.md §6.2).
package workflows

import (
	"context"

	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/dashboard/contracts"
	"github.com/naozhi/naozhi/internal/session"
)

// IPLimiter aliases the shared dashboard contract (#2285).
type IPLimiter = contracts.IPLimiter

// SessionLookup is the *session.Router surface the handler reads.
type SessionLookup interface {
	SessionFor(key string) *session.ManagedSession
}

// Board is the *session.WorkflowBoard surface the handler reads (RFC
// §5.8.1); a stub in tests stands for a board without a session.
type Board interface {
	Published() *workflow.Published
	Result(ctx context.Context, taskID string) (*workflow.ResultCache, session.ResultStatus)
}

// Deps bundles all wiring for New.
type Deps struct {
	Router  SessionLookup
	Limiter IPLimiter
}
