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

func (a testRouterAdapter) ReserveTakeover(key string, opts sessionview.AgentOpts) (TakeoverLease, error) {
	lease, err := a.Router.ReserveTakeover(key, opts)
	if err != nil {
		return nil, err
	}
	return testLease{a.Router, lease}, nil
}

type testLease struct {
	r     *session.Router
	lease *session.TakeoverLease
}

func (l testLease) Takeover(ctx context.Context, sessionID, workspace string) (Session, error) {
	s, err := l.r.Takeover(ctx, l.lease, sessionID, workspace)
	return testSession(s), err
}

func (l testLease) Release() { l.lease.Release() }

func testSession(s *session.ManagedSession) Session {
	if s == nil {
		return nil
	}
	return s
}
