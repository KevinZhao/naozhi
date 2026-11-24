package wireup

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/bgtask"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/session"
)

// benchSession is a session in r under key whose turn ended turn ago with a
// background Bash started a minute before that.
func benchSession(r *session.Router, key string, now time.Time, turn time.Duration) (*session.ManagedSession, *session.TestProcess) {
	start := now.Add(-turn - time.Minute)
	s, p := bgSession(r, key, now.Add(-24*time.Hour),
		clievent.EventEntry{Type: clievent.KindToolUse, Tool: "Bash", Time: start.UnixMilli()},
		taskStart("b-"+key, "local_bash", "npm run bench", start),
		clievent.EventEntry{Type: clievent.KindResult, Time: now.Add(-turn).UnixMilli()})
	s.SetLastTurnAtForTest(now.Add(-turn))
	return s, p
}

// The progress notes follow this node's sessions the Planner dispatched and
// the ones the user opened that are observed, while their turn is over with
// background work open: not one whose turn runs, not one with nothing
// open, not the Planner's own, a one-shot run's, a cron run's, a daemon's
// or a scratch session, whatever the watch switch says. A task's end takes
// its session off the list.
func TestProgressList(t *testing.T) {
	r := newDigestRouter(t)
	now := time.Now()
	const chat, task, off, busy = "dashboard:direct:u:general", "task:aaaaaaaaaaaa", "dashboard:direct:off:general", "feishu:direct:u:general"
	_, chatProc := benchSession(r, chat, now, 40*time.Minute)
	benchSession(r, task, now, 40*time.Minute)
	benchSession(r, off, now, 40*time.Minute)
	_, busyProc := benchSession(r, busy, now, 40*time.Minute)
	busyProc.SetState(cli.StateRunning)
	for _, k := range []string{"cron:j1", "planner:main", "scout:r1", "reader:r1", "sys:gc", "scratch:x"} {
		benchSession(r, k, now, 40*time.Minute)
	}
	r.InjectSession("dashboard:direct:idle:general", session.NewTestProcess())
	watched := func(key string) bool { return key != off }

	keys := func() []string {
		var out []string
		for _, b := range progressList(r, watched) {
			out = append(out, b.Key)
		}
		slices.Sort(out)
		return out
	}
	if got, want := keys(), []string{chat, task}; !slices.Equal(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	for _, b := range progressList(r, watched) {
		if !b.Turn.Equal(time.UnixMilli(now.Add(-40*time.Minute).UnixMilli())) || b.State.Count() != 1 || b.State.Tasks[0].Summary != "npm run bench" {
			t.Errorf("%s = turn %v, %+v; want its turn's end and the Bash", b.Key, b.Turn, b.State)
		}
	}
	chatProc.EventLog.Append(taskDone("b-"+chat, now))
	if got, want := keys(), []string{task}; !slices.Equal(got, want) {
		t.Fatalf("after the chat's task ended listed %v, want %v", got, want)
	}
}

// ProgressNotes writes the note of a session whose turn ended 40 minutes
// ago into the journal the session's notes are read from, and writes none
// while the inbox's pause holds.
func TestProgressNotesTick(t *testing.T) {
	r := newDigestRouter(t)
	j, err := journal.Open(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	inbox := notify.New(notify.Config{System: func(string, string) error { return nil }})
	notes := SessionNotes(j, inbox, sidecar.Open(t.TempDir(), nil), r, nil, nil)
	var pushed []note.Note
	notes.SetPush(func(nt note.Note) { pushed = append(pushed, nt) })
	const chat = "dashboard:direct:u:general"
	now := time.Now()
	benchSession(r, chat, now, 40*time.Minute)
	tick := ProgressNotes(notes, r, func(string) bool { return true })

	inbox.Pause(now.Add(time.Hour))
	tick()
	if len(pushed) != 0 {
		t.Fatalf("written while paused: %+v", pushed)
	}
	inbox.Pause(time.Time{})
	tick()
	tick()
	if len(pushed) != 1 || pushed[0].Kind != note.KindProgress || pushed[0].Key != chat ||
		pushed[0].Text != "已跑 41 分钟 · 1 个后台任务：npm run bench" {
		t.Fatalf("pushed %+v, want the one progress note", pushed)
	}
	list := notes.List(chat, time.Now())
	if len(list) != 1 || list[0].ID != pushed[0].ID {
		t.Fatalf("List = %+v, want the note", list)
	}
}

// The task card's line is the progress notes' (bgtask's State.Progress),
// only while no turn runs.
func TestTaskLiveProgress(t *testing.T) {
	r := newDigestRouter(t)
	now := time.Now()
	const idle, busy = "task:aaaaaaaaaaaa", "dashboard:direct:u:general"
	benchSession(r, idle, now, 40*time.Minute)
	_, p := benchSession(r, busy, now, 40*time.Minute)
	p.SetState(cli.StateRunning)
	r.InjectSession("dashboard:direct:idle:general", session.NewTestProcess())
	live := TaskLive(r)()
	if l := live[idle]; l.Progress != "已跑 41 分钟 · 1 个后台任务：npm run bench" || l.Background == nil {
		t.Errorf("turn over: live = %+v, want the progress line", l)
	}
	if l := live[busy]; l.Progress != "" || l.Background == nil {
		t.Errorf("turn running: live = %+v, want no progress line", l)
	}
	if l := live["dashboard:direct:idle:general"]; l.Progress != "" || l.Background != nil {
		t.Errorf("nothing open: live = %+v", l)
	}
}

// A running workflow carries its agents' counts from its summary and the
// phase its board has it in; one not on the board, or with no board, names
// no phase. A paused or ended one is no work going on.
func TestRunningWorkflowsProgress(t *testing.T) {
	start := time.Now().Add(-time.Hour).UnixMilli()
	sums := []workflow.Summary{
		{TaskID: "w1", Name: "review-all", Status: workflow.StatusRunning, StartedAt: start, Counts: workflow.Counts{Total: 12, Done: 5, Failed: 1, Running: 3, Queued: 3}},
		{TaskID: "w2", Status: workflow.StatusRunning, StartedAt: start},
		{TaskID: "w3", Name: "paused", Status: workflow.StatusPaused, StartedAt: start},
		{TaskID: "w4", Name: "done", Status: workflow.StatusCompleted, StartedAt: start},
	}
	pub := workflow.NewPublished("e", []*workflow.Workflow{{TaskID: "w1", Status: workflow.StatusRunning, Phases: []workflow.Phase{
		{Index: 0, Title: "探索", Counts: workflow.Counts{Total: 5, Done: 5}},
		{Index: 1, Title: "验证", Counts: workflow.Counts{Total: 7, Running: 3, Queued: 3, Failed: 1}},
	}}}, nil)
	got := runningWorkflows(sums, pub, time.Time{})
	if len(got) != 2 || got[0].ID != "w1" || got[0].Agents != 12 || got[0].Done != 5 || got[0].Failed != 1 || got[0].Phase != "验证" ||
		got[1].ID != "w2" || got[1].Phase != "" || got[1].Agents != 0 {
		t.Fatalf("running = %+v", got)
	}
	line := bgtask.WithWorkflows(bgtask.State{}, got).Progress(time.UnixMilli(start).Add(time.Hour))
	if want := "已跑 1 小时 · 工作流 review-all：12 个子代理完成 5 个、失败 1 个，阶段「验证」 · 工作流 w2"; line != want {
		t.Errorf("Progress = %q, want %q", line, want)
	}
	if got := runningWorkflows(sums, nil, time.Time{}); len(got) != 2 || got[0].Phase != "" || got[0].Agents != 12 {
		t.Errorf("without a board: %+v", got)
	}
}
