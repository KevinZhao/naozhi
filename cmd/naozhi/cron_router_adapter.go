// cron_router_adapter.go translates between session.* and cron.* types
// so internal/cron does not need to import internal/session.
//
// The adapter sits at the production boundary (cmd/naozhi) — every
// cron Scheduler call into the router goes through cronRouterAdapter,
// every session returned from GetOrCreate is wrapped in
// cronSessionAdapter. cron's Scheduler only ever sees cron-local types
// (cron.AgentOpts / cron.Session / cron.SessionStatus / cron.InterruptOutcome).
//
// Why this lives in cmd/naozhi rather than internal/cron: the adapter
// needs both types simultaneously, but cron must NOT import session
// (the whole point of Phase B). main is the natural seam — it owns the
// concrete *session.Router and instantiates the cron.Scheduler.
//
// Refs: docs/rfc/cron-sysession-merge.md Phase B (§3.3.3).

package main

import (
	"context"
	"fmt"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
)

// init pins cron.InterruptOutcome ordinals against
// session.InterruptOutcome. Diverging values would silently miscast in
// cronSessionAdapter.InterruptViaControl below; init() panic crashes
// the binary at boot instead. Panic message includes actual ordinals
// so on-call can diagnose without re-running.
func init() {
	if int(cron.InterruptSent) != int(session.InterruptSent) ||
		int(cron.InterruptNoSession) != int(session.InterruptNoSession) ||
		int(cron.InterruptNoTurn) != int(session.InterruptNoTurn) ||
		int(cron.InterruptUnsupported) != int(session.InterruptUnsupported) ||
		int(cron.InterruptError) != int(session.InterruptError) {
		panic(fmt.Sprintf(
			"cron.InterruptOutcome ordinals diverged from session.InterruptOutcome: "+
				"Sent(c=%d s=%d) NoSession(c=%d s=%d) NoTurn(c=%d s=%d) Unsupported(c=%d s=%d) Error(c=%d s=%d) — "+
				"update cron_router_adapter.go",
			cron.InterruptSent, session.InterruptSent,
			cron.InterruptNoSession, session.InterruptNoSession,
			cron.InterruptNoTurn, session.InterruptNoTurn,
			cron.InterruptUnsupported, session.InterruptUnsupported,
			cron.InterruptError, session.InterruptError,
		))
	}
}

// cronRouterAdapter implements cron.SessionRouter against *session.Router,
// translating the cron-local types into session-side equivalents.
type cronRouterAdapter struct{ r *session.Router }

// Compile-time guard: cronRouterAdapter must satisfy cron.SessionRouter.
// If cron.SessionRouter gains a method, this assertion makes the
// breakage land here — next to the implementation — instead of at the
// distant NewScheduler call site that takes a SessionRouter interface
// value. R249-ARCH-7 (#973): Phase B replaced "*session.Router
// satisfies cron.SessionRouter" with this adapter, so the var _ assert
// previously living on *session.Router migrated here.
var _ cron.SessionRouter = cronRouterAdapter{}

// Compile-time guard: cronSessionAdapter must satisfy cron.Session
// (Send + SessionID + InterruptViaControl). Catches drift if the
// cron.Session method set expands but the adapter forgets to forward.
var _ cron.Session = cronSessionAdapter{}

func (a cronRouterAdapter) RegisterCronStubWithChain(key, workspace, lastPrompt string, chain []string) {
	a.r.RegisterCronStubWithChain(key, workspace, lastPrompt, chain)
}

func (a cronRouterAdapter) Reset(key string) { a.r.Reset(key) }

func (a cronRouterAdapter) GetOrCreate(ctx context.Context, key string, opts cron.AgentOpts) (cron.Session, cron.SessionStatus, error) {
	sess, st, err := a.r.GetOrCreate(ctx, key, toSessionAgentOpts(opts))
	if err != nil {
		return nil, cron.SessionStatus(int(st)), err
	}
	return cronSessionAdapter{sess}, cron.SessionStatus(int(st)), nil
}

// toSessionAgentOpts copies cron.AgentOpts → session.AgentOpts.
//
// ExtraArgs is cloned (not aliased) per session/router_lifecycle.go:267
// contract: "callers populating AgentOpts to feed the router should
// treat ExtraArgs as owned exclusively by them — do NOT keep aliases
// to slices held by other goroutines (R215-ARCH-P2-8 / R37-CONCUR1)".
func toSessionAgentOpts(o cron.AgentOpts) session.AgentOpts {
	out := session.AgentOpts{
		Model:     o.Model,
		Workspace: o.Workspace,
		Backend:   o.Backend,
		Exempt:    o.Exempt,
	}
	if len(o.ExtraArgs) > 0 {
		out.ExtraArgs = append([]string(nil), o.ExtraArgs...)
	}
	return out
}

// toCronAgentOpts copies session.AgentOpts → cron.AgentOpts; used to
// build the cron.Scheduler's agents map at boot from cfg.Agents.
//
// ExtraArgs cloned identically: cron's Scheduler stores AgentOpts in a
// map that is read once-and-treated-as-immutable, but cloning here
// closes the same aliasing contract for symmetry with the runtime path.
func toCronAgentOpts(o session.AgentOpts) cron.AgentOpts {
	out := cron.AgentOpts{
		Model:     o.Model,
		Workspace: o.Workspace,
		Backend:   o.Backend,
		Exempt:    o.Exempt,
	}
	if len(o.ExtraArgs) > 0 {
		out.ExtraArgs = append([]string(nil), o.ExtraArgs...)
	}
	return out
}

// cronSessionAdapter wraps *session.ManagedSession behind the narrow
// cron.Session contract (Send + InterruptViaControl). cron does not
// use attachments or per-turn event callbacks; passing nil/nil to Send
// matches what cron has always done.
type cronSessionAdapter struct{ s *session.ManagedSession }

func (c cronSessionAdapter) Send(ctx context.Context, text string) (cron.SendResult, error) {
	r, err := c.s.Send(ctx, text, nil, nil)
	if r == nil {
		return cron.SendResult{}, err
	}
	return cron.SendResult{Text: r.Text, SessionID: r.SessionID}, err
}

// SessionID forwards to *session.ManagedSession.SessionID so the cron
// inflight broadcast can fill in the running CLI session id mid-Send.
// Mirrors fix(cron) #766 (commits 53981bf2 / 49bf32de) which the
// pre-Phase-B code reached via the *session.ManagedSession concrete
// type — the cron-local Session interface now exposes the same hook.
func (c cronSessionAdapter) SessionID() string {
	if c.s == nil {
		return ""
	}
	return c.s.SessionID()
}

func (c cronSessionAdapter) InterruptViaControl() cron.InterruptOutcome {
	return cron.InterruptOutcome(int(c.s.InterruptViaControl()))
}
