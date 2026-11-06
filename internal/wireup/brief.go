package wireup

import (
	"time"

	"github.com/naozhi/naozhi/internal/cron"
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
