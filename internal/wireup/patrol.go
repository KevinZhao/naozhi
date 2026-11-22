package wireup

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/action"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/planner/patrol"
	"github.com/naozhi/naozhi/internal/planner/reap"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

// PatrolActive is patrol.Config.Active: when each workspace's last turn of
// one of the user's sessions ended (a cron run's, the Planner's and the
// scout's turns are not the user being there). The turns from now on are
// seen as the journal writes them; those of the last patrol.ActiveDays are
// read once, at the first call, which is the patrol's first round and not
// startup. now nil is time.Now.
func PatrolActive(j *journal.Journal, now func() time.Time) func() map[string]time.Time {
	if j == nil {
		return func() map[string]time.Time { return nil }
	}
	if now == nil {
		now = time.Now
	}
	var (
		mu   sync.Mutex
		last = map[string]time.Time{}
		once sync.Once
	)
	// The observer runs on whichever goroutine appends: one map write.
	see := func(e journal.Entry) {
		if e.Kind != journal.KindTurn || e.WS == "" || !health.UserSession(e.Unit, health.KindSession) {
			return
		}
		mu.Lock()
		if e.TS.After(last[e.WS]) {
			last[e.WS] = e.TS
		}
		mu.Unlock()
	}
	j.Observe(see)
	return func() map[string]time.Time {
		once.Do(func() {
			at := now()
			entries, err := j.Read(at.Add(-patrol.ActiveDays*24*time.Hour), at.Add(time.Minute))
			if err != nil {
				slog.Warn("planner patrol: journal partly read; some workspaces may be left out", "err", err)
			}
			for _, e := range entries {
				see(e)
			}
		})
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]time.Time, len(last))
		for ws, at := range last {
			out[ws] = at
		}
		return out
	}
}

// PatrolWorktrees is patrol.Config.Worktrees: the worktrees the sidecars
// say a put-away left in place, waiting on their pull request or holding
// work not committed or pushed.
func PatrolWorktrees(sidecars *sidecar.Store) func() []patrol.Worktree {
	if sidecars == nil {
		return nil
	}
	return func() []patrol.Worktree {
		var out []patrol.Worktree
		for _, m := range sidecars.List() {
			left := m.LeftWorktrees
			if m.Reap != nil {
				left = append(left[:len(left):len(left)], m.Reap.Worktrees...)
			}
			for _, wt := range left {
				if wt.State == reap.WTPending || wt.State == reap.WTKept {
					out = append(out, patrol.Worktree{Dir: wt.Dir, GitDir: wt.GitDir, Why: wt.Why})
				}
			}
		}
		return out
	}
}

// PatrolRoutines is patrol.Config.Routines: the cron jobs with failures in
// a row, the ones the scheduler paused for them included. One the user
// paused is left out: they have seen it, and its count only clears on a
// resume. nil without a scheduler.
func PatrolRoutines(sched *cron.Scheduler, workspaces *workspace.Registry) func() []patrol.Routine {
	if sched == nil {
		return nil
	}
	return func() []patrol.Routine {
		var out []patrol.Routine
		for _, jw := range sched.ListAllJobsWithNextRun() {
			j := jw.Job
			auto := j.Paused && cron.IsAutoPausedReason(j.PausedReason)
			// A pause for transient failures leaves the streak alone; its
			// own count is what stopped the job.
			n := j.ConsecutiveFailures
			if auto {
				n = max(n, j.AutoPauseCount())
			}
			if n == 0 || j.Paused && !auto {
				continue
			}
			r := patrol.Routine{ID: j.ID, Title: routineTitle(j), Failures: n, AutoPaused: auto}
			if workspaces != nil && j.WorkDir != "" {
				if ws, ok := workspaces.Lookup(j.WorkDir); ok {
					r.Workspace = ws.ID
				}
			}
			out = append(out, r)
		}
		return out
	}
}

// PatrolSelf is patrol.Config.Self: naozhi's own run in [since, until),
// counted from the journal's lines of the scout's runs, the Planner
// conversation's lines for blocks not run, custody stopping, and the starts
// after a crash.
func PatrolSelf(j *journal.Journal) func(since, until time.Time) patrol.Self {
	if j == nil {
		return nil
	}
	return func(since, until time.Time) patrol.Self {
		entries, err := j.Read(since, until)
		if err != nil {
			slog.Warn("planner patrol: journal partly read; the week's counts may be short", "err", err)
		}
		var s patrol.Self
		for _, e := range entries {
			var d struct {
				Type    string `json:"type"`
				Trigger string `json:"trigger"`
				State   string `json:"state"`
				Text    string `json:"text"`
				Event   string `json:"event"`
				Source  string `json:"source"`
			}
			if json.Unmarshal(e.Data, &d) != nil {
				continue
			}
			switch {
			case e.Kind == journal.KindActivity && e.Unit == scout.UnitID && d.Type == scout.ActivityType && d.Trigger != scout.TriggerMeeting:
				s.ScoutRuns++
				if d.State == scout.StateFailed {
					s.ScoutFailed++
				}
			case e.Kind == journal.KindDialog && e.Unit == agent.Key && action.UnrunNote(d.Text):
				s.Unrun++
			case e.Kind == journal.KindCustody && d.Event == note.CustodyStopped:
				s.CustodyStopped++
			case e.Kind == journal.KindNotify && d.Event == "open" && strings.HasPrefix(d.Source, notify.CrashSource):
				s.Crashes++
			}
		}
		return s
	}
}
