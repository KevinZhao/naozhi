package wireup

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/planner/patrol"
	"github.com/naozhi/naozhi/internal/planner/reap"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

func patrolJournal(t *testing.T) *journal.Journal {
	t.Helper()
	j, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func appendEntry(t *testing.T, j *journal.Journal, e journal.Entry, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	e.Data = raw
	if err := j.Append(e); err != nil {
		t.Fatal(err)
	}
}

// PatrolActive: a workspace counts as having had a session by the turns of
// the user's sessions and the dispatched ones, those written before the
// first call (read from the journal) and those after (observed); a cron
// run's, the Planner's and the scout's turns do not count, nor a turn past
// the 7 days read.
func TestPatrolActive(t *testing.T) {
	j := patrolJournal(t)
	reg := workspace.Load("", "node1")
	root, _ := filepath.EvalSymlinks(t.TempDir())
	id := map[string]string{}
	for _, n := range []string{"chat", "task", "cron", "planner", "scout", "later", "old"} {
		ws, ok := reg.Ensure(filepath.Join(root, n))
		if !ok {
			t.Fatal("workspace not registered")
		}
		id[n] = ws.ID
	}
	rec := activity.NewRecorder(j, reg)
	rec.RecordTurn("dashboard:direct:u:general", filepath.Join(root, "chat"), activity.TurnDone)
	rec.RecordTurn("task:aaaaaaaaaaaa", filepath.Join(root, "task", "sub"), activity.TurnFailed)
	rec.RecordTurn("cron:job1", filepath.Join(root, "cron"), activity.TurnDone)
	rec.RecordTurn("planner:main", filepath.Join(root, "planner"), activity.TurnDone)
	rec.RecordTurn("scout:20261123T080000-abcd", filepath.Join(root, "scout"), activity.TurnDone)
	// In no workspace at all.
	rec.RecordTurn("dashboard:direct:u:general", "", activity.TurnDone)
	now := time.Now()
	appendEntry(t, j, journal.Entry{TS: now.Add(-patrol.ActiveDays*24*time.Hour - time.Hour), WS: id["old"], Kind: journal.KindTurn, Unit: "dashboard:direct:u:general"},
		activity.Turn{Outcome: activity.TurnDone})

	active := PatrolActive(j, func() time.Time { return now })
	got := active()
	var have []string
	for n, ws := range id {
		if at, ok := got[ws]; ok {
			have = append(have, n)
			if at.After(time.Now()) || at.Before(now.Add(-time.Minute)) {
				t.Errorf("%s last active at %v", n, at)
			}
		}
	}
	slices.Sort(have)
	if want := []string{"chat", "task"}; !slices.Equal(have, want) {
		t.Fatalf("active = %v, want %v", have, want)
	}

	rec.RecordTurn("feishu:direct:alice:general", filepath.Join(root, "later"), activity.TurnDone)
	rec.RecordTurn("cron:job1", filepath.Join(root, "cron"), activity.TurnDone)
	got = active()
	if _, ok := got[id["later"]]; !ok || len(got) != 3 {
		t.Errorf("after a turn observed active = %v", got)
	}
	// The map handed out is the caller's.
	delete(got, id["later"])
	if _, ok := active()[id["later"]]; !ok {
		t.Error("the caller's change reached the table")
	}
	if got := PatrolActive(nil, nil)(); len(got) != 0 {
		t.Errorf("with no journal active = %v", got)
	}
}

// PatrolSelf counts naozhi's own run from the journal: the scout's runs and
// the failed ones (a pre-meeting brief is not a run), the Planner's blocks
// not run, custody stopping, and the starts after a crash, in the window
// asked for.
func TestPatrolSelf(t *testing.T) {
	j := patrolJournal(t)
	now := time.Now()
	since := now.Add(-time.Hour)
	type run struct {
		Type    string `json:"type"`
		Trigger string `json:"trigger,omitempty"`
		State   string `json:"state"`
	}
	text := func(s string) any { return map[string]string{"text": s} }
	at := journal.Entry{TS: now.Add(-time.Minute)}
	with := func(kind journal.Kind, unit string) journal.Entry {
		e := at
		e.Kind, e.Unit = kind, unit
		return e
	}
	appendEntry(t, j, with(journal.KindActivity, scout.UnitID), run{Type: scout.ActivityType, State: scout.StateOK})
	appendEntry(t, j, with(journal.KindActivity, scout.UnitID), run{Type: scout.ActivityType, State: scout.StateFailed})
	appendEntry(t, j, with(journal.KindActivity, scout.UnitID), run{Type: scout.ActivityType, State: scout.StateWaiting})
	appendEntry(t, j, with(journal.KindActivity, scout.UnitID), run{Type: scout.ActivityType, Trigger: scout.TriggerMeeting, State: scout.StateFailed})
	// Another unit's activity, and a failed run before the window.
	appendEntry(t, j, with(journal.KindActivity, "task:aaaaaaaaaaaa"), run{Type: "pr_create", State: scout.StateFailed})
	appendEntry(t, j, journal.Entry{TS: since.Add(-time.Minute), Kind: journal.KindActivity, Unit: scout.UnitID}, run{Type: scout.ActivityType, State: scout.StateFailed})

	appendEntry(t, j, with(journal.KindDialog, agent.Key), text("动作块格式不对，没有执行：未知类型。原文：{}"))
	appendEntry(t, j, with(journal.KindDialog, agent.Key), text("动作块没有执行：一次回复最多 10 个动作块。原文：{}"))
	appendEntry(t, j, with(journal.KindDialog, agent.Key), text("巡检触发的回合不能派活：「x」没有派出"))
	appendEntry(t, j, with(journal.KindDialog, "task:aaaaaaaaaaaa"), text("动作块没有执行：不是 Planner 的对话"))

	appendEntry(t, j, with(journal.KindCustody, "task:aaaaaaaaaaaa"), note.CustodyData{Event: note.CustodyStopped, Reason: "连续 3 次没有进展"})
	appendEntry(t, j, with(journal.KindCustody, "task:aaaaaaaaaaaa"), note.CustodyData{Event: "sent", N: 1})

	// The inbox journals the items it opens: the start after a crash is
	// one, by its source.
	inbox := notify.New(notify.Config{System: func(string, string) error { return nil }, Journal: j})
	crash := notify.CrashSource + now.Format(time.RFC3339)
	inbox.Add(notify.Item{Kind: notify.KindNotice, Level: notify.LevelTimely, Source: crash, Title: "naozhi 上次异常退出"})
	inbox.Add(notify.Item{Kind: notify.KindNotice, Level: notify.LevelTimely, Source: "disk", Title: "磁盘快满了"})
	// The user closing the notice is journaled with the same source: it is
	// not another crash.
	inbox.DismissSource(crash)

	got := PatrolSelf(j)(since, time.Now().Add(time.Minute))
	if want := (patrol.Self{ScoutRuns: 3, ScoutFailed: 1, Unrun: 2, CustodyStopped: 1, Crashes: 1}); got != want {
		t.Errorf("Self = %+v, want %+v", got, want)
	}
	if got := PatrolSelf(j)(since.Add(-time.Hour), since.Add(-30*time.Second)); got != (patrol.Self{ScoutRuns: 1, ScoutFailed: 1}) {
		t.Errorf("the hour before: %+v", got)
	}
	if PatrolSelf(nil) != nil {
		t.Error("PatrolSelf with no journal is not nil")
	}
}

// PatrolWorktrees lists the worktrees a put-away left in place (waiting on
// their pull request, or kept for work not committed or pushed), of a
// session put away and of one back under its key; those removed or gone are
// not left.
func TestPatrolWorktrees(t *testing.T) {
	side := sidecar.Open(t.TempDir(), nil)
	wt := func(name, state, why string) sidecar.Worktree {
		return sidecar.Worktree{Dir: "/repo/.claude/worktrees/" + name, GitDir: "/repo/.git", Branch: name, State: state, Why: why}
	}
	if err := side.Put(sidecar.Meta{Key: "task:aaaaaaaaaaaa", Reap: &sidecar.Reap{
		Worktrees: []sidecar.Worktree{wt("pending", reap.WTPending, "PR 还开着"), wt("removed", reap.WTRemoved, "")}}}); err != nil {
		t.Fatal(err)
	}
	if err := side.Put(sidecar.Meta{Key: "task:bbbbbbbbbbbb",
		LeftWorktrees: []sidecar.Worktree{wt("kept", reap.WTKept, "有未提交的改动"), wt("gone", reap.WTGone, "")}}); err != nil {
		t.Fatal(err)
	}
	if err := side.Put(sidecar.Meta{Key: "task:cccccccccccc"}); err != nil {
		t.Fatal(err)
	}
	got := PatrolWorktrees(side)()
	slices.SortFunc(got, func(a, b patrol.Worktree) int { return cmpString(a.Dir, b.Dir) })
	want := []patrol.Worktree{
		{Dir: "/repo/.claude/worktrees/kept", GitDir: "/repo/.git", Why: "有未提交的改动"},
		{Dir: "/repo/.claude/worktrees/pending", GitDir: "/repo/.git", Why: "PR 还开着"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("worktrees = %+v, want %+v", got, want)
	}
	if PatrolWorktrees(nil) != nil {
		t.Error("PatrolWorktrees with no sidecars is not nil")
	}
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// PatrolRoutines lists the cron jobs with failures in a row under their
// workspace, with the title the snapshot shows; one the scheduler paused
// for them counts by the count that paused it, one the user paused is left
// out, as is one that last succeeded.
func TestPatrolRoutines(t *testing.T) {
	sched := cron.NewScheduler(cron.SchedulerConfig{AllowNilRouter: true}, cron.SchedulerDeps{})
	reg := workspace.Load("", "node1")
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	ws, ok := reg.Ensure(dir)
	if !ok {
		t.Fatal("workspace not registered")
	}
	for _, j := range []*cron.Job{
		{Title: "内网模型动态", WorkDir: filepath.Join(dir, "sub"), ConsecutiveFailures: 2},
		{Prompt: "查账单更新\n第一行写结论", ConsecutiveFailures: 5, Paused: true, PausedReason: cron.PausedReasonAutoFailures},
		{Title: "后端故障停的", ConsecutiveFailures: 1, TransientFailures: 40, Paused: true, PausedReason: cron.PausedReasonAutoTransient},
		{Title: "用户停的", ConsecutiveFailures: 4, Paused: true},
		{Title: "好的"},
	} {
		j.Schedule = "0 9 * * *"
		if j.Prompt == "" {
			j.Prompt = "p"
		}
		if err := sched.AddJob(j); err != nil {
			t.Fatal(err)
		}
	}
	got := PatrolRoutines(sched, reg)()
	slices.SortFunc(got, func(a, b patrol.Routine) int { return cmpString(a.Title, b.Title) })
	for i := range got {
		// The scheduler names the job.
		if job, ok := sched.GetJob(got[i].ID); !ok || routineTitle(job) != got[i].Title {
			t.Errorf("routine %+v is not the job its ID names", got[i])
		}
		got[i].ID = ""
	}
	want := []patrol.Routine{
		{Title: "内网模型动态", Workspace: ws.ID, Failures: 2},
		{Title: "后端故障停的", Failures: 40, AutoPaused: true},
		{Title: "查账单更新", Failures: 5, AutoPaused: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("routines = %+v, want %+v", got, want)
	}
	if got := PatrolRoutines(sched, nil)(); len(got) != 3 || got[0].Workspace != "" && got[1].Workspace != "" && got[2].Workspace != "" {
		t.Errorf("with no registry routines = %+v", got)
	}
	if PatrolRoutines(nil, reg) != nil {
		t.Error("PatrolRoutines with no scheduler is not nil")
	}
}
