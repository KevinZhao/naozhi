package dispatch

import (
	"context"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// testRouter adapts a real *session.Router for tests the way server's
// dispatchRouter does in production: a missing session is a nil interface.
type testRouter struct{ *session.Router }

func (t testRouter) GetOrCreate(ctx context.Context, key string, opts sessionview.AgentOpts) (Session, sessionview.SessionStatus, error) {
	s, status, err := t.Router.GetOrCreate(ctx, key, opts)
	if s == nil {
		return nil, status, err
	}
	return s, status, err
}

// routerOf wraps r for DispatcherConfig.Router; nil stays an untyped nil.
func routerOf(r *session.Router) SessionRouter {
	if r == nil {
		return nil
	}
	return testRouter{r}
}
