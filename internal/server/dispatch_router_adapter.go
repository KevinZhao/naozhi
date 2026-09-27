package server

import (
	"context"

	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// Compile-time guards so method-set drift lands here, next to the adapter.
var (
	_ dispatch.SessionRouter = dispatchRouter{}
	_ dispatch.Session       = (*session.ManagedSession)(nil)
	_ dispatch.SessionView   = (*session.ManagedSession)(nil)
	_ dispatch.KeyResolver   = (*session.KeyResolver)(nil)
)

// dispatchRouter adapts *session.Router to dispatch.SessionRouter, so the
// dispatcher never names the concrete session type. The other methods, and
// Resolver (the fallback chain's router-shared resolver), are promoted.
type dispatchRouter struct{ *session.Router }

// GetOrCreate converts a missing session to a nil interface: a nil
// *ManagedSession inside a non-nil interface would pass the dispatcher's
// checks and reach Capabilities.Send.
func (d dispatchRouter) GetOrCreate(ctx context.Context, key string, opts sessionview.AgentOpts) (dispatch.Session, sessionview.SessionStatus, error) {
	s, status, err := d.Router.GetOrCreate(ctx, key, opts)
	if s == nil {
		return nil, status, err
	}
	return s, status, err
}
