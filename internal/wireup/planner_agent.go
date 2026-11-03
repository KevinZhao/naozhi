package wireup

import (
	"cmp"
	"context"

	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/session"
)

// PlannerSessions is agent.Sessions over the session router.
type PlannerSessions struct{ Router *session.Router }

var _ agent.Sessions = PlannerSessions{}

func (p PlannerSessions) State(key string) agent.SessionState {
	s := p.Router.SessionFor(key)
	if s == nil {
		return agent.SessionState{}
	}
	state := s.State()
	return agent.SessionState{
		Exists: true,
		// With no process State reads "ready"; a suspended session has none.
		Alive:     s.HasProcess() && state != "dead",
		Running:   state == "running",
		SessionID: s.SessionID(),
	}
}

func (p PlannerSessions) Ensure(ctx context.Context, key string, opts session.AgentOpts) error {
	_, _, err := p.Router.GetOrCreate(ctx, key, opts)
	return err
}

func (p PlannerSessions) Recreate(ctx context.Context, key string, opts session.AgentOpts) error {
	_, err := p.Router.ResetAndRecreate(ctx, key, opts)
	return err
}

func (p PlannerSessions) Release(key string) bool {
	s := p.Router.SessionFor(key)
	if s == nil || !s.HasProcess() || s.State() == "dead" {
		return true
	}
	return s.ReleaseIdleProcess()
}

func (p PlannerSessions) Label(key string) string {
	s := p.Router.SessionFor(key)
	if s == nil {
		return ""
	}
	snap := s.Snapshot()
	return cmp.Or(snap.UserLabel, snap.Summary)
}

func (p PlannerSessions) Name(key string) string {
	s := p.Router.SessionFor(key)
	if s == nil {
		return ""
	}
	return s.CLISessionName()
}
