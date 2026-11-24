package wireup

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/action"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/brief"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/planner/patrol"
	"github.com/naozhi/naozhi/internal/planner/reap"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/task"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/session"
)

// PatrolActive is patrol.Config.Active: when each workspace's last turn of
// one of the user's sessions ended (a cron run's, the Planner's and the
// scout's turns are not the user being there). The turns from now on are
// seen as the journal writes them; those of the last patrol.ActiveDays are
// read once, at the first call, which is the patrol's first round and not
// startup. now nil is time.Now. own (nil for none) names the sessions the
// patrol itself had dispatched (PatrolOwnTask): their turns are not the
// user being there either, or a workspace the user left would stay in scope,
// and count as changed, for as long as the patrol kept finding things to
// fix in it.
func PatrolActive(j *journal.Journal, now func() time.Time, own func(unit string) bool) func() map[string]time.Time {
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
		if e.Kind != journal.KindTurn || e.WS == "" || !health.UserSession(e.Unit, health.KindSession) || own != nil && own(e.Unit) {
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

// PatrolOwnTask reports whether session key was dispatched to fix a
// finding of the patrol's (its sidecar names one), for PatrolActive.
func PatrolOwnTask(sidecars *sidecar.Store) func(key string) bool {
	if sidecars == nil {
		return nil
	}
	return func(key string) bool {
		m, ok := sidecars.Get(key)
		return ok && m.Finding != ""
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

// PatrolSession is patrol.Config.Session over the session router, as
// ScoutSession is the scout's: a fresh session spawned for the deep check,
// its one turn sent, then reset.
func PatrolSession(router *session.Router) patrol.Session {
	return func(ctx context.Context, key string, opts session.AgentOpts, text string) (string, error) {
		sess, _, err := router.GetOrCreate(ctx, key, opts)
		if err != nil {
			return "", err
		}
		defer router.Reset(key)
		res, err := sendTimed(ctx, sess, text)
		if ctx.Err() != nil {
			// Cut short mid-turn: end the CLI before the Reset's graceful
			// close, which waits past Patrol.Stop's wait (ScoutSession).
			sess.Interrupt()
		}
		if err == nil && res.Text == "" {
			err = errors.New("empty reply")
		}
		return res.Text, err
	}
}

// PatrolWake is patrol.Config.Wake over the Planner: each block of a
// workspace's new findings as a turn from agent.SourcePatrol, the patrol
// told how each turn ended. Whatever keeps the Planner from taking them,
// the patrol keeps them for its next round.
func PatrolWake(a *agent.Agent) func(sent, ended func(batch int, err error), blocks ...string) error {
	return func(sent, ended func(int, error), blocks ...string) error {
		return a.WakeEnded(agent.SourcePatrol, sent, ended, blocks...)
	}
}

// PatrolTaskOpen is patrol.Config.TaskOpen: whether dispatched session key
// is still going in 任务. Over is one marked done or failed (by the Planner
// or the user, a stop among them), one whose session is gone, and one no
// longer listed. A failure health reports of a live session is not over:
// custody sends it on in minutes and the user may, so the task keeps its
// place in the workspace's cap and its finding, and the pull request it
// reports after going on is the finding's.
func PatrolTaskOpen(tasks *task.Board) func(key string) bool {
	return func(key string) bool {
		for _, t := range tasks.List() {
			if t.ID != key {
				continue
			}
			switch {
			case t.State == task.Done:
				return false
			case t.State == task.Failed:
				return t.By == task.ByHealth && t.Retired.IsZero()
			}
			return true
		}
		return false
	}
}

// PatrolAlarm is patrol.Config.Alarm: an error in "等你", pushed.
func PatrolAlarm(n *notify.Notifier) func(source, title, why string) {
	return func(source, title, why string) {
		n.Add(notify.Item{Kind: notify.KindError, Level: notify.LevelTimely, Source: source, Title: title, Why: why})
	}
}

// PatrolAnswer is patrol.Config.Answer over the Planner: what the user
// chose about a finding, as a turn they started whose words are the choice
// (agent.Agent.TellAnswer), so a dispatch that follows is theirs.
func PatrolAnswer(a *agent.Agent, now func() time.Time) func(text, question, words string) error {
	if now == nil {
		now = time.Now
	}
	return func(text, question, words string) error {
		return a.TellAnswer(text, agent.Words{Text: words, Question: question, At: now()})
	}
}

// PatrolCloseCards is patrol.Config.CloseCards: the open cards in "通知"
// about a finding — the notice of its pull request, the Planner's questions
// about it — close as settled elsewhere, why saying how. The card being
// answered right now is left to its answer (notify.ErrPending).
func PatrolCloseCards(n *notify.Notifier) func(ref, why string) {
	return func(ref, why string) {
		open, _ := n.List(0)
		for _, it := range open {
			if patrol.CardRef(it) == ref || action.AskFinding(it) == ref {
				if err := n.Expire(it.ID, why); err != nil && !errors.Is(err, notify.ErrPending) && !errors.Is(err, notify.ErrClosed) {
					slog.Warn("planner patrol: a finding's card not closed", "finding", ref, "err", err)
				}
			}
		}
	}
}

// BriefProjects is brief.Config.Projects over the patrol: its part of the
// morning brief, in the brief's own types.
func BriefProjects(p *patrol.Patrol) func(since time.Time) brief.ProjectPatrol {
	return func(since time.Time) brief.ProjectPatrol {
		rep := p.Brief(since)
		var out brief.ProjectPatrol
		for _, pr := range rep.PRs {
			out.PRs = append(out.PRs, brief.ProjectPR{Title: pr.Title, URL: pr.URL, CI: pr.CI, Workspace: pr.Workspace, Fresh: pr.Fresh})
		}
		for _, f := range rep.New {
			out.New = append(out.New, brief.ProjectFinding{Title: f.Title, Kind: f.Kind, Workspace: f.Workspace})
		}
		for _, k := range rep.Stopped {
			out.Stopped = append(out.Stopped, brief.ProjectStopped{Kind: k.Kind, Adopted: k.Adopted, Of: k.Of})
		}
		return out
	}
}
