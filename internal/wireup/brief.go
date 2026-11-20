package wireup

import (
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/brief"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// RoutineResults is brief.Config.Routines: each cron job's latest
// successful run in the window, its result compared with the run before
// (cron.Scheduler.LatestResultChange). nil without a scheduler.
func RoutineResults(sched *cron.Scheduler) func(since, until time.Time) []brief.Routine {
	if sched == nil {
		return nil
	}
	return func(since, until time.Time) []brief.Routine {
		var out []brief.Routine
		for _, jw := range sched.ListAllJobsWithNextRun() {
			if c, ok := sched.LatestResultChange(jw.Job.ID, since, until); ok {
				out = append(out, brief.Routine{Unit: sessionkey.CronKey(jw.Job.ID), At: c.EndedAt, Changed: c.Changed, FirstLine: c.FirstLine})
			}
		}
		return out
	}
}

// BriefTidy is brief.Config.Tidy: the morning tidy's block goes to the
// Planner as a turn of its own (agent.SourceMorning), followed by the
// snapshot with the lines of work. false when the Planner cannot take it
// (off, stopped, starting), and the brief goes out without waiting. nil
// without a Planner.
func BriefTidy(a *agent.Agent) func(text string) bool {
	if !a.Enabled() {
		return nil
	}
	return func(text string) bool {
		if err := a.Wake(agent.SourceMorning, nil, text); err != nil {
			slog.Info("brief: the Planner did not take the morning tidy", "err", err)
			return false
		}
		return true
	}
}
