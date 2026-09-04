// cron_router_adapter.go translates between session.* and cron.* types so
// internal/cron never imports internal/session; wireup is the seam that knows
// both type universes (main → wireup → {cron, session}).

package wireup

import (
	"context"
	"fmt"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
)

// Compile-time ordinal pins: if cron.* and session.* iota values diverge the
// difference is non-zero and the uint conversion of a negative constant fails
// to compile. The init() panic below is a second guard.
const (
	// InterruptOutcome (5 values)
	_ = uint(int(cron.InterruptSent) - int(session.InterruptSent))               // compile-time pin: diverge → negative → uint overflow
	_ = uint(int(cron.InterruptNoSession) - int(session.InterruptNoSession))     // compile-time pin
	_ = uint(int(cron.InterruptNoTurn) - int(session.InterruptNoTurn))           // compile-time pin
	_ = uint(int(cron.InterruptUnsupported) - int(session.InterruptUnsupported)) // compile-time pin
	_ = uint(int(cron.InterruptError) - int(session.InterruptError))             // compile-time pin
	// SessionStatus (3 values)
	_ = uint(int(cron.SessionExisting) - int(session.SessionExisting)) // compile-time pin
	_ = uint(int(cron.SessionResumed) - int(session.SessionResumed))   // compile-time pin
	_ = uint(int(cron.SessionNew) - int(session.SessionNew))           // compile-time pin
)

// init pins cron.InterruptOutcome / cron.SessionStatus ordinals against their
// session counterparts: the adapters below cast by int without a value guard,
// so divergence would silently miscast. Panic at boot with the actual ordinals.
func init() {
	if int(cron.InterruptSent) != int(session.InterruptSent) ||
		int(cron.InterruptNoSession) != int(session.InterruptNoSession) ||
		int(cron.InterruptNoTurn) != int(session.InterruptNoTurn) ||
		int(cron.InterruptUnsupported) != int(session.InterruptUnsupported) ||
		int(cron.InterruptError) != int(session.InterruptError) {
		panic(fmt.Sprintf(
			"cron.InterruptOutcome ordinals diverged from session.InterruptOutcome: "+
				"Sent(c=%d s=%d) NoSession(c=%d s=%d) NoTurn(c=%d s=%d) Unsupported(c=%d s=%d) Error(c=%d s=%d) — "+
				"update wireup/cron_router_adapter.go",
			cron.InterruptSent, session.InterruptSent,
			cron.InterruptNoSession, session.InterruptNoSession,
			cron.InterruptNoTurn, session.InterruptNoTurn,
			cron.InterruptUnsupported, session.InterruptUnsupported,
			cron.InterruptError, session.InterruptError,
		))
	}
	if int(cron.SessionExisting) != int(session.SessionExisting) ||
		int(cron.SessionResumed) != int(session.SessionResumed) ||
		int(cron.SessionNew) != int(session.SessionNew) {
		panic(fmt.Sprintf(
			"cron.SessionStatus ordinals diverged from session.SessionStatus: "+
				"Existing(c=%d s=%d) Resumed(c=%d s=%d) New(c=%d s=%d) — "+
				"update wireup/cron_router_adapter.go",
			cron.SessionExisting, session.SessionExisting,
			cron.SessionResumed, session.SessionResumed,
			cron.SessionNew, session.SessionNew,
		))
	}
}

// cronRouterAdapter implements cron.SessionRouter against *session.Router,
// translating the cron-local types into session-side equivalents.
type cronRouterAdapter struct{ r *session.Router }

// newCronRouterAdapter wraps a live *session.Router as a cron.SessionRouter.
func newCronRouterAdapter(r *session.Router) cron.SessionRouter {
	return cronRouterAdapter{r: r}
}

// Compile-time guards so method-set drift lands here, next to the adapters.
var _ cron.SessionRouter = cronRouterAdapter{}

var _ cron.Session = cronSessionAdapter{}

func (a cronRouterAdapter) RegisterCronStubWithChain(key, workspace, lastPrompt string, chain []string) {
	a.r.RegisterCronStubWithChain(key, workspace, lastPrompt, chain)
}

func (a cronRouterAdapter) Reset(key string) { a.r.Reset(key) }

func (a cronRouterAdapter) GetOrCreate(ctx context.Context, key string, opts cron.AgentOpts) (cron.Session, cron.SessionStatus, error) {
	sess, st, err := a.r.GetOrCreate(ctx, key, toSessionAgentOpts(opts))
	if err != nil {
		return nil, 0, err
	}
	return cronSessionAdapter{sess}, cron.SessionStatus(int(st)), nil
}

// toSessionAgentOpts copies cron.AgentOpts → session.AgentOpts. ExtraArgs is
// cloned: the router treats AgentOpts.ExtraArgs as exclusively owned.
func toSessionAgentOpts(o cron.AgentOpts) session.AgentOpts {
	// AccessProfile is intentionally NOT propagated: cron.Job has no profile
	// field and resume-lock would freeze a first-run profile with no cron-side
	// UI to change it, risking a mis-charge (RFC project-access-profile P1-b).
	out := session.AgentOpts{
		Model:        o.Model,
		Workspace:    o.Workspace,
		Backend:      o.Backend,
		Effort:       o.Effort,
		SystemPrompt: o.SystemPrompt,
		Exempt:       o.Exempt,
	}
	if len(o.ExtraArgs) > 0 {
		out.ExtraArgs = append([]string(nil), o.ExtraArgs...)
	}
	return out
}

// cronSessionAdapter wraps *session.ManagedSession behind the narrow
// cron.Session contract; cron uses no attachments or per-turn callbacks.
type cronSessionAdapter struct{ s *session.ManagedSession }

func (c cronSessionAdapter) Send(ctx context.Context, text string) (cron.SendResult, error) {
	r, err := c.s.Send(ctx, text, nil, nil)
	if r == nil {
		return cron.SendResult{}, err
	}
	// CostUSD crosses the boundary so local runs persist their cost (#2280).
	return cron.SendResult{Text: r.Text, SessionID: r.SessionID, CostUSD: r.CostUSD}, err
}

// SessionID lets the cron inflight broadcast fill in the CLI session id
// mid-Send. Assumes c.s is non-nil (always constructed with a live session).
func (c cronSessionAdapter) SessionID() string {
	return c.s.SessionID()
}

func (c cronSessionAdapter) InterruptViaControl() cron.InterruptOutcome {
	return cron.InterruptOutcome(int(c.s.InterruptViaControl()))
}
