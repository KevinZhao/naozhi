package wireup

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/monitor/health"
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
		tools := calledTools(sess.EventLastN(scoutEventScan))
		if err == nil && res.Text == "" {
			err = errors.New("empty reply")
		}
		return res.Text, tools, err
	}
}

// calledTools is the tools entries called, in first-call order.
func calledTools(entries []clievent.EventEntry) []string {
	var out []string
	for _, e := range entries {
		if e.Type == clievent.KindToolUse && e.Tool != "" && !slices.Contains(out, e.Tool) {
			out = append(out, e.Tool)
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
