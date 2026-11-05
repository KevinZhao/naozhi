package wireup

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/session"
)

// scoutEventScan is how many of a scout session's events are searched for
// the tools it called.
const scoutEventScan = 2000

// ScoutSession is scout.Config.Session over the session router: a fresh
// session spawned for the run, its one turn sent and judged the way a fresh
// cron run's is (cronSessionAdapter), then reset.
func ScoutSession(router *session.Router) scout.Session {
	return func(ctx context.Context, key string, opts session.AgentOpts, text string) (string, []string, error) {
		sess, _, err := router.GetOrCreate(ctx, key, opts)
		if err != nil {
			return "", nil, err
		}
		defer router.Reset(key)
		res, err := cronSessionAdapter{sess}.Send(ctx, text)
		if ctx.Err() != nil {
			// Cut short (shutdown, or the run's timeout) mid-turn: the
			// Reset's graceful close waits seconds for a CLI that does
			// not exit on EOF mid-turn, past Scout.Stop's wait, so end
			// the CLI first; the turn's reply is not wanted either way.
			sess.Interrupt()
		}
		tools := calledTools(sess.EventLastN(scoutEventScan))
		if err == nil && res.Text == "" {
			err = errors.New("empty reply")
		}
		return res.Text, tools, err
	}
}

// ScoutWake is scout.Config.Wake (src agent.SourceScout) or Brief
// (agent.SourceBriefing) over the Planner: each block as a turn from src.
// The Planner off or stopped, or a block it will never take, is
// scout.ErrPlannerOff, so the findings count as seen; anything else (not
// started yet, busy, closing) is worth another try at the next run.
func ScoutWake(a *agent.Agent, src agent.Source) func(blocks ...string) error {
	return func(blocks ...string) error {
		err := a.Wake(src, blocks...)
		if errors.Is(err, agent.ErrOff) || errors.Is(err, agent.ErrStopped) || errors.Is(err, agent.ErrTooLong) {
			return fmt.Errorf("%w: %w", scout.ErrPlannerOff, err)
		}
		return err
	}
}

// PlannerOn is scout.Config.PlannerOn: the Planner on and not stopped.
func PlannerOn(a *agent.Agent) func() bool {
	return func() bool {
		st := a.Status()
		return st.Enabled && !st.Stopped
	}
}

// calledTools is the tools entries called, in first-call order; an
// email_read call that passed markAs is listed again with
// scout.MarkAsSuffix, so the run fails loudly.
func calledTools(entries []clievent.EventEntry) []string {
	var out []string
	for _, e := range entries {
		if e.Type != clievent.KindToolUse || e.Tool == "" {
			continue
		}
		name := e.Tool
		if e.MarkAs {
			name += scout.MarkAsSuffix
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// scoutFact is the scout's health unit, like a daemon's: its last run, and
// the failure when that run failed.
func scoutFact(st scout.Status) health.Fact {
	f := health.Fact{
		ID:       scout.UnitID,
		Kind:     health.KindSysession,
		Source:   "scout",
		Coverage: health.CoverageGateway,
		Running:  st.Running,
	}
	if r := st.Last; r != nil {
		f.LastActive = cmp.Or(r.Ended, r.Started)
		if r.State == scout.StateFailed {
			f.Failed, f.ErrText = "last scout run failed", r.Error
		}
	}
	return f
}
