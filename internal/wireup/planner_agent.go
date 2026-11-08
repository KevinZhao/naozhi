package wireup

import (
	"cmp"
	"context"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/digest"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
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

// compactBoundary is the system frame the Claude CLI reports a compaction
// of the conversation with; the event log keeps it as a system entry by
// its subtype.
const compactBoundary = "compact_boundary"

// CompactedSince reports a compaction the session's event log shows after
// t, and also one it cannot rule out: no session, or a log that no longer
// reaches back to t.
func (p PlannerSessions) CompactedSince(key string, t time.Time) bool {
	s := p.Router.SessionFor(key)
	if s == nil {
		return true
	}
	entries := s.EventEntries()
	ms := t.UnixMilli()
	if len(entries) == 0 || entries[0].Time > ms {
		return true
	}
	for _, e := range entries {
		if e.Time > ms && e.Type == clievent.KindSystem && e.Summary == compactBoundary {
			return true
		}
	}
	return false
}

// PlannerConclusion is agent.Config.Conclusion over the sidecars: the first
// sentence of a dispatched session's latest report, as its task card says
// it.
func PlannerConclusion(sidecars *sidecar.Store) func(key string) string {
	return func(key string) string {
		m, ok := sidecars.Get(key)
		if !ok || m.Report == nil {
			return ""
		}
		return digest.FirstSentence(m.Report.Summary)
	}
}
