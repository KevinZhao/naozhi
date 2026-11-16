package wireup

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cron"
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
		res, err := sendTimed(ctx, sess, text)
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

// sendTimed sends text under a ctx of its own that ctx's end cuts: when
// ctx's deadline passes, the tools the session has in flight are read
// before the cut, whose interrupt closes them, and the error says what the
// session was doing (scout.TimeoutError).
func sendTimed(ctx context.Context, sess *session.ManagedSession, text string) (cron.SendResult, error) {
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	var cut *scout.TimeoutError
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			cut = stalled(sess, time.Now())
		}
		cancel()
	})
	res, err := cronSessionAdapter{sess}.Send(sctx, text)
	if !stop() {
		<-done
		if cut != nil {
			// The send saw its own ctx cancelled; the cause is ctx's deadline.
			cut.Err = ctx.Err()
			if err != nil && !errors.Is(err, context.Canceled) {
				cut.Err = fmt.Errorf("%w: %w", ctx.Err(), err)
			}
			err = cut
		}
	}
	return res, err
}

// stalled is what sess is doing at now: its tool calls in flight and how
// long it has sent nothing.
func stalled(sess *session.ManagedSession, now time.Time) *scout.TimeoutError {
	var t scout.Timeout
	for _, tool := range sess.InflightTools() {
		t.Tools = append(t.Tools, scout.TimeoutTool{Tool: cmp.Or(tool.Name, "unknown"), ElapsedSec: int(now.Sub(tool.Started) / time.Second)})
	}
	if last := sess.EventLastN(1); len(last) == 1 && last[0].Time > 0 {
		t.QuietSec = int(now.Sub(time.UnixMilli(last[0].Time)) / time.Second)
	}
	return &scout.TimeoutError{Timeout: t}
}

// ScoutWake is scout.Config.Wake (src agent.SourceScout) or Brief
// (agent.SourceBriefing) over the Planner: each block as a turn from src.
// The Planner off or stopped, or a block it will never take, is
// scout.ErrPlannerOff, so the findings count as seen; anything else (not
// started yet, busy, closing) is worth another try at the next run. A later
// block's outcome reaches sent the same way.
func ScoutWake(a *agent.Agent, src agent.Source) func(sent func(block int, err error), blocks ...string) error {
	off := func(err error) error {
		if errors.Is(err, agent.ErrOff) || errors.Is(err, agent.ErrStopped) || errors.Is(err, agent.ErrTooLong) {
			return fmt.Errorf("%w: %w", scout.ErrPlannerOff, err)
		}
		return err
	}
	return func(sent func(int, error), blocks ...string) error {
		var told func(int, error)
		if sent != nil {
			told = func(i int, err error) { sent(i, off(err)) }
		}
		return off(a.Wake(src, told, blocks...))
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
