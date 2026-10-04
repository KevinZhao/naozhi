// upstream_router.go adapts *session.Router to upstream.SessionRouter, so
// internal/upstream never imports internal/session; wireup is the seam that
// knows both (main → wireup → {upstream, session}).

package wireup

import (
	"context"

	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
	"github.com/naozhi/naozhi/internal/upstream"
)

// Compile-time guards so method-set drift lands here, next to the adapter.
var (
	_ upstream.SessionRouter   = upstreamRouter{}
	_ upstream.Session         = (*session.ManagedSession)(nil)
	_ upstream.PlannerResolver = (*session.KeyResolver)(nil)
)

// UpstreamRouter wraps a live *session.Router as an upstream.SessionRouter.
func UpstreamRouter(r *session.Router) upstream.SessionRouter { return upstreamRouter{r} }

// upstreamRouter forwards every method; the three that hand back a session
// convert it with asUpstreamSession. The rest are promoted from the embedded
// router unchanged, since their signatures speak sessionview's types already.
type upstreamRouter struct{ *session.Router }

func (u upstreamRouter) SessionFor(key string) upstream.Session {
	return asUpstreamSession(u.Router.SessionFor(key))
}

func (u upstreamRouter) ResetAndRecreate(ctx context.Context, key string, opts sessionview.AgentOpts) (upstream.Session, error) {
	s, err := u.Router.ResetAndRecreate(ctx, key, opts)
	return asUpstreamSession(s), err
}

func (u upstreamRouter) Takeover(ctx context.Context, key, sessionID, workspace string, opts sessionview.AgentOpts) (upstream.Session, error) {
	s, err := u.Router.Takeover(ctx, key, sessionID, workspace, opts)
	return asUpstreamSession(s), err
}

// asUpstreamSession converts a router session to the connector's view. A
// missing session must arrive as a nil interface: a nil *ManagedSession
// inside a non-nil interface would pass the connector's `sess == nil`
// checks and be dereferenced.
func asUpstreamSession(s *session.ManagedSession) upstream.Session {
	if s == nil {
		return nil
	}
	return s
}
