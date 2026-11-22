package wireup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/action"
	"github.com/naozhi/naozhi/internal/planner/activity"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/brief"
	"github.com/naozhi/naozhi/internal/planner/front"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/planner/patrol"
	"github.com/naozhi/naozhi/internal/planner/reap"
	"github.com/naozhi/naozhi/internal/planner/scout"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/task"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/testhelper"
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
// the 7 days read, nor the patrol's own: a deep check's session, and a task
// dispatched to fix a finding.
func TestPatrolActive(t *testing.T) {
	j := patrolJournal(t)
	reg := workspace.Load("", "node1")
	root, _ := filepath.EvalSymlinks(t.TempDir())
	id := map[string]string{}
	for _, n := range []string{"chat", "task", "cron", "planner", "scout", "later", "old", "fix", "fixlater", "deep"} {
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
	// The patrol's own: a deep check's session, and the tasks dispatched to
	// fix its findings.
	rec.RecordTurn("patrol:20261125T110412-9c1e", filepath.Join(root, "deep"), activity.TurnDone)
	side := sidecar.Open(t.TempDir(), nil)
	for _, key := range []string{"task:ffffffffffff", "task:eeeeeeeeeeee"} {
		if err := side.Put(sidecar.Meta{Key: key, Purpose: sidecar.PurposeTask, Finding: "ws_ab12cd34/bug-router-nil"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := side.Put(sidecar.Meta{Key: "task:aaaaaaaaaaaa", Purpose: sidecar.PurposeTask}); err != nil {
		t.Fatal(err)
	}
	rec.RecordTurn("task:ffffffffffff", filepath.Join(root, "fix"), activity.TurnDone)
	// In no workspace at all.
	rec.RecordTurn("dashboard:direct:u:general", "", activity.TurnDone)
	now := time.Now()
	appendEntry(t, j, journal.Entry{TS: now.Add(-patrol.ActiveDays*24*time.Hour - time.Hour), WS: id["old"], Kind: journal.KindTurn, Unit: "dashboard:direct:u:general"},
		activity.Turn{Outcome: activity.TurnDone})

	active := PatrolActive(j, func() time.Time { return now }, PatrolOwnTask(side))
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
	rec.RecordTurn("task:eeeeeeeeeeee", filepath.Join(root, "fixlater"), activity.TurnDone)
	got = active()
	if _, ok := got[id["later"]]; !ok || len(got) != 3 {
		t.Errorf("after a turn observed active = %v", got)
	}
	// The map handed out is the caller's.
	delete(got, id["later"])
	if _, ok := active()[id["later"]]; !ok {
		t.Error("the caller's change reached the table")
	}
	if got := PatrolActive(nil, nil, nil)(); len(got) != 0 {
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

// TestStateChanges: a deep check's calls that change state are told from
// its events, each once, by the tool and what planner/activity classifies
// the call as; reads are none.
func TestStateChanges(t *testing.T) {
	bash := func(cmd string) clievent.EventEntry {
		in, _ := json.Marshal(map[string]string{"command": cmd})
		ev := clievent.Event{Type: "assistant", Message: &clievent.AssistantMessage{Role: "assistant",
			Content: []clievent.ContentBlock{{Type: "tool_use", ID: "tu", Name: "Bash", Input: in}}}}
		return cli.EventEntriesFromEventAt(ev, 1)[0]
	}
	reads := []clievent.EventEntry{
		bash("git log --oneline -20"), bash("gh pr list --state open"), bash("git status && git diff HEAD~1"),
		bash("git tag --list"), bash("git push --dry-run origin x"),
		{Type: clievent.KindToolUse, Tool: "Read"}, {Type: clievent.KindText, Activity: "git_push"},
	}
	if got := stateChanges(reads); got != nil {
		t.Errorf("reads told as state changes: %q", got)
	}
	writes := append(reads,
		bash("git push origin fix/x"), bash("cd /srv/repo && git push origin fix/x"), bash("gh pr merge 31 --squash"),
		bash("git tag v1.2.3"), bash("gh pr create --fill"), bash("gh release create v1.2.3"), bash("git push origin fix/x"))
	want := []string{"Bash:git_push", "Bash:pr_merge", "Bash:git_tag", "Bash:pr_create", "Bash:release"}
	if got := stateChanges(writes); !slices.Equal(got, want) {
		t.Errorf("state changes %q, want %q", got, want)
	}
}

// TestPatrolSession: the deep check's one turn runs on the key's session;
// what comes back is its reply, the tools it called and the calls that
// changed state, and the session is reset.
func TestPatrolSession(t *testing.T) {
	// A cron key: reconnect retires a one-shot key's surviving shim, and
	// the key is all PatrolSession takes from the patrol (as ScoutSession's
	// test).
	const key = "cron:3b9d1f6a8c2e4705"
	fs := session.StartFakeShimForTest(t, key, 1, []string{adoptE2EAssistant, adoptE2EPrevResult})
	r := session.NewRouter(fs.Config)
	t.Cleanup(r.Shutdown)
	r.ReconnectShimsCtx(context.Background())
	fs.Attached(t)
	type out struct {
		reply         string
		tools, writes []string
		err           error
	}
	done := make(chan out, 1)
	go func() {
		reply, tools, writes, err := PatrolSession(r)(context.Background(), key, session.AgentOpts{}, "深查")
		done <- out{reply, tools, writes, err}
	}()
	testhelper.Eventually(t, func() bool { return len(fs.Sent()) > 0 }, 5*time.Second, "the turn never reached the shim")
	fs.Emit(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu1","name":"Read","input":{"file_path":"/ws/a.go"}}]},"session_id":"s1"}`)
	fs.Emit(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu2","name":"Bash","input":{"command":"git log -3"}}]},"session_id":"s1"}`)
	fs.Emit(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu3","name":"Bash","input":{"command":"git push origin fix/x"}}]},"session_id":"s1"}`)
	fs.Emit(t, `{"type":"result","subtype":"success","result":"{\"findings\":[]}","session_id":"s1"}`)
	var got out
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the deep check's session did not return")
	}
	if got.err != nil || got.reply != `{"findings":[]}` {
		t.Fatalf("reply %q, err %v", got.reply, got.err)
	}
	if !slices.Equal(got.tools, []string{"Read", "Bash"}) || !slices.Equal(got.writes, []string{"Bash:git_push"}) {
		t.Errorf("tools %q writes %q, want Read, Bash and the push", got.tools, got.writes)
	}
	if r.SessionFor(key) != nil {
		t.Error("the session is still in the router after the deep check")
	}
}

// TestPatrolTaskOpen: a task of the patrol's holds its place in the
// workspace's cap while it is in progress or waits on the user, and gives it
// up once done, failed, stopped or gone.
func TestPatrolTaskOpen(t *testing.T) {
	side := sidecar.Open(t.TempDir(), nil)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	put := func(key string, mark *sidecar.Mark) {
		t.Helper()
		if err := side.Put(sidecar.Meta{Key: key, Purpose: sidecar.PurposeTask, Title: key, Created: now.Add(-time.Hour), Mark: mark}); err != nil {
			t.Fatal(err)
		}
	}
	put("task:doing", nil)
	put("task:done", &sidecar.Mark{State: string(task.Done), By: string(task.ByPlanner), At: now.Add(-time.Minute)})
	put("task:failed", &sidecar.Mark{State: string(task.Failed), By: string(task.ByPlanner), At: now.Add(-time.Minute)})
	put("task:reopened", &sidecar.Mark{State: string(task.Doing), By: string(task.ByPlanner), At: now.Add(-time.Minute)})
	open := PatrolTaskOpen(task.New(task.Config{Sessions: side, Now: func() time.Time { return now }}))
	for key, want := range map[string]bool{"task:doing": true, "task:reopened": true, "task:done": false, "task:failed": false, "task:gone": false, "": false} {
		if got := open(key); got != want {
			t.Errorf("open(%q) = %v, want %v", key, got, want)
		}
	}
}

// TestPatrolAlarm: a deep check that wrote opens an error in "等你".
func TestPatrolAlarm(t *testing.T) {
	n := notify.New(notify.Config{System: func(string, string) error { return nil }})
	PatrolAlarm(n)("patrol:write:r1", "项目巡查的深查会话做了写操作", "gateway：Bash:git_push；它本该只读，请检查这个仓库")
	open, _ := n.List(0)
	if len(open) != 1 || open[0].Kind != notify.KindError || open[0].Source != "patrol:write:r1" || open[0].Title != "项目巡查的深查会话做了写操作" {
		t.Errorf("inbox %+v", open)
	}
}

// TestPatrolTaskDeny: a session dispatched to fix a patrol finding cannot
// merge a pull request or cut a release by gh: the two rules are in its
// deny list at dispatch and on a respawn from its sidecar. Any other
// dispatched session does not carry them.
func TestPatrolTaskDeny(t *testing.T) {
	const finding = "ws_ab12cd34/bug-router-nil"
	merge, release := "Bash(gh pr merge:*)", "Bash(gh release create:*)"
	side := sidecar.Open("", nil)
	to := TaskOpts{Base: session.AgentOpts{PermissionsDeny: []string{"Bash(rm -rf /)"}}, Sessions: side, Planner: "planner-abc123"}
	want := append(append([]string{"Bash(rm -rf /)"}, taskDenyRules...), merge, release)

	opts, err := to.For("task:ab12", action.Spawn{Dir: "/tmp/ws", Title: "修重连", Finding: finding})
	if err != nil || !slices.Equal(opts.PermissionsDeny, want) {
		t.Fatalf("a patrol task's deny = %v, %v; want %v", opts.PermissionsDeny, err, want)
	}
	if len(to.Base.PermissionsDeny) != 1 {
		t.Errorf("base changed: %v", to.Base.PermissionsDeny)
	}
	// The rules are deny rules only: nothing of them is allowed back.
	for _, r := range opts.PermissionsAllow {
		if strings.Contains(r, "gh pr merge") || strings.Contains(r, "gh release") {
			t.Errorf("allow holds %s", r)
		}
	}
	plain, err := to.For("task:cd34", action.Spawn{Dir: "/tmp/ws", Title: "写文档"})
	if err != nil || slices.Contains(plain.PermissionsDeny, merge) || slices.Contains(plain.PermissionsDeny, release) {
		t.Fatalf("a plain task's deny = %v, %v; want neither rule", plain.PermissionsDeny, err)
	}

	// A respawn rebuilds the options from the sidecar: the same rules, or
	// the shim's recorded overlay would read as drift.
	patrolKey, plainKey := sidecar.TaskKey(), sidecar.TaskKey()
	if err := side.Put(sidecar.Meta{Key: patrolKey, Purpose: sidecar.PurposeTask, Title: "修重连", Dir: "/tmp/ws", Finding: finding}); err != nil {
		t.Fatal(err)
	}
	if err := side.Put(sidecar.Meta{Key: plainKey, Purpose: sidecar.PurposeTask, Title: "写文档", Dir: "/tmp/ws"}); err != nil {
		t.Fatal(err)
	}
	if opts, ok := to.ForKey(patrolKey); !ok || !slices.Equal(opts.PermissionsDeny, want) {
		t.Errorf("a patrol task's respawn = %v, %v", opts.PermissionsDeny, ok)
	}
	if opts, ok := to.ForKey(plainKey); !ok || slices.Contains(opts.PermissionsDeny, merge) || slices.Contains(opts.PermissionsDeny, release) {
		t.Errorf("a plain task's respawn = %v, %v", opts.PermissionsDeny, ok)
	}
	// With mcp_tools as well (the user's own dispatch of a finding): both
	// sets of rules.
	if _, err := to.For("task:ef56", action.Spawn{Dir: "/tmp/ws", Title: "t", Finding: finding, MCPTools: map[string][]string{"aws-outlook-mcp": {"email_send"}}}); err == nil {
		t.Error("mcp_tools with no cli.mcp_config went through")
	}
}

// TestPatrolCards: the user's answer on a patrol card in "通知" reaches the
// patrol (its handler), which turns the finding down and has the other
// cards about that finding closed as settled; a card about another finding
// stays. The brief gets the patrol's part in its own types.
func TestPatrolCards(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "gateway")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const ws = "ws_ab12cd34"
	one, two := patrol.Ref(ws, "bug-one"), patrol.Ref(ws, "doc-two")
	n := notify.New(notify.Config{System: func(string, string) error { return nil }})
	handed := make(chan struct{}, 1)
	var told []string
	p := patrol.New(patrol.Config{
		Dir:        filepath.Join(root, "planner", "patrol"),
		Workspaces: func() []workspace.Dir { return []workspace.Dir{{ID: ws, Name: "gateway", Path: dir}} },
		Items:      front.New("", nil),
		Exec: func(context.Context, string, string, ...string) (string, error) {
			return "", os.ErrNotExist // no git here
		},
		Session: func(context.Context, string, session.AgentOpts, string) (string, []string, []string, error) {
			return `{"findings":[
				{"id":"bug-one","title":"重连时会话为空会崩","kind":"bug","evidence":["a.go:1"],"impact":"重连即崩","fix":"先判空","size":"S","reversible":true,"confidence":"high"},
				{"id":"doc-two","title":"README 与脚本不符","kind":"doc","evidence":["a.go:1"],"impact":"装不上","fix":"改 README","size":"S","reversible":true,"confidence":"high"}]}`, nil, nil, nil
		},
		// The findings are on record once they are handed to the Planner.
		Wake:       func(func(int, error), ...string) error { handed <- struct{}{}; return nil },
		Answer:     func(text, _, words string) error { told = append(told, words+"|"+text); return nil },
		CloseCards: PatrolCloseCards(n),
	})
	t.Cleanup(p.Stop)
	n.HandleDecisions(patrol.Handler, p.DecideCard)
	if _, err := p.Trigger(ws); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handed:
	case <-time.After(10 * time.Second):
		t.Fatal("the deep check did not hand its findings over")
	}

	ask := func(source, ref string) notify.Item {
		it, _ := n.Add(notify.Item{Kind: notify.KindQuestion, Source: "ask:" + source, Title: "怎么改？",
			Decision: &notify.Decision{Handler: action.HandlerAsk, Payload: json.RawMessage(`{"question":"怎么改？","finding":"` + ref + `"}`), Actions: []notify.Action{"方案 A"}}})
		return it
	}
	notice, _ := n.Add(notify.Item{Kind: notify.KindNotice, Source: "planner:pr", Title: "PR 已备好", Decision: p.Card(one)})
	about, other := ask("r1", one), ask("r2", two)
	if notice.Decision == nil || action.AskFinding(about) != one {
		t.Fatalf("cards: %+v, %+v", notice.Decision, about.Decision)
	}

	if err := n.Decide(notice.ID, patrol.LabelSkip); err != nil {
		t.Fatal(err)
	}
	if r, _ := p.Finding(one); r.State != patrol.StateDeclined {
		t.Errorf("record %+v, want the finding turned down", r)
	}
	if got, _ := n.Item(notice.ID); got.State != notify.StateClosed || got.ClosedBy != notify.ClosedByUser || got.Decision.Answer != patrol.LabelSkip {
		t.Errorf("the card answered: %+v", got)
	}
	if got, _ := n.Item(about.ID); got.State != notify.StateClosed || got.ClosedWhy != "你已选了不做" {
		t.Errorf("the question about the same finding: %+v, want it closed as settled", got)
	}
	if got, _ := n.Item(other.ID); got.State != notify.StateOpen {
		t.Errorf("the question about another finding: %+v, want it open", got)
	}
	if len(told) != 0 {
		t.Errorf("the Planner was told %q with nothing left to do", told)
	}
	// An answer the card does not carry never reaches the patrol.
	again, _ := n.Add(notify.Item{Kind: notify.KindNotice, Source: "planner:pr2", Title: "PR 已备好", Decision: p.Card(two)})
	if err := n.Decide(again.ID, "合并"); !errors.Is(err, notify.ErrNoDecision) {
		t.Errorf("an answer the card does not carry: %v", err)
	}
	// "做" goes on to the Planner in the user's words.
	if err := n.Decide(again.ID, patrol.LabelDo); err != nil || len(told) != 1 || !strings.HasPrefix(told[0], "做|") || !strings.Contains(told[0], `"finding":"`+two+`"`) {
		t.Errorf("做 on the card: %v, told %q", err, told)
	}

	rep := BriefProjects(p)(time.Time{})
	if len(rep.PRs) != 0 || len(rep.Stopped) != 0 || !slices.Equal(rep.New, []brief.ProjectFinding{{Title: "README 与脚本不符", Kind: "doc", Workspace: "gateway"}}) {
		t.Errorf("the brief's part: %+v", rep)
	}
}

// TestPatrolAnswer: the user's choice goes to the Planner as an answer of
// theirs; a Planner that is off says so, for the patrol to keep the choice
// and go no further.
func TestPatrolAnswer(t *testing.T) {
	a := agent.New(agent.Config{})
	if err := PatrolAnswer(a, nil)("用户选了：做", "怎么处置", "做"); !errors.Is(err, agent.ErrOff) {
		t.Errorf("with the Planner off: %v, want ErrOff", err)
	}
}
