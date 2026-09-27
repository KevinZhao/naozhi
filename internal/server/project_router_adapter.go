package server

import (
	"context"

	dashproject "github.com/naozhi/naozhi/internal/dashboard/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// Compile-time guards so method-set drift lands here, next to the adapter.
var (
	_ dashproject.RouterView         = projectRouter{}
	_ dashproject.PlannerSession     = (*session.ManagedSession)(nil)
	_ dashproject.PlannerKeyResolver = (*session.KeyResolver)(nil)
)

// projectRouter adapts *session.Router to dashproject.RouterView, so the
// project package never names the concrete session type. BumpVersion is
// promoted unchanged.
type projectRouter struct{ *session.Router }

// SessionFor converts a missing session to a nil interface: a nil
// *ManagedSession inside a non-nil interface would pass the project list's
// `sess != nil` check and be dereferenced.
func (p projectRouter) SessionFor(key string) dashproject.PlannerSession {
	if s := p.Router.SessionFor(key); s != nil {
		return s
	}
	return nil
}

func (p projectRouter) ResetAndRecreate(ctx context.Context, key string, opts sessionview.AgentOpts) error {
	_, err := p.Router.ResetAndRecreate(ctx, key, opts)
	return err
}
