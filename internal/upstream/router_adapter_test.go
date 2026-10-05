package upstream

import (
	"context"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// testRouter adapts a real *session.Router for these tests the way
// wireup.UpstreamRouter does in production. The tests cannot use that one:
// wireup imports upstream. wireup's own test covers the production adapter.
func testRouter(r *session.Router) SessionRouter { return testRouterAdapter{r} }

type testRouterAdapter struct{ *session.Router }

func (a testRouterAdapter) SessionFor(key string) Session {
	return testSession(a.Router.SessionFor(key))
}

func (a testRouterAdapter) ResetAndRecreate(ctx context.Context, key string, opts sessionview.AgentOpts) (Session, error) {
	s, err := a.Router.ResetAndRecreate(ctx, key, opts)
	return testSession(s), err
}

func (a testRouterAdapter) TakeoverPrecheck(key string) error {
	lease, err := a.Router.ReserveTakeover(key, sessionview.AgentOpts{})
	lease.Release()
	return err
}

func (a testRouterAdapter) Takeover(ctx context.Context, key, sessionID, workspace string, opts sessionview.AgentOpts) (Session, error) {
	lease, err := a.Router.ReserveTakeover(key, opts)
	if err != nil {
		return nil, err
	}
	s, err := a.Router.Takeover(ctx, lease, sessionID, workspace)
	return testSession(s), err
}

func testSession(s *session.ManagedSession) Session {
	if s == nil {
		return nil
	}
	return s
}
