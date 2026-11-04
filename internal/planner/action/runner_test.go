package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/task"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

type fakeGateway struct {
	spawned  []string
	spawns   []Spawn
	spawnErr error
	notes    []string
	told     []string
	refused  []string
	sideDir  string
}

func newRunner(t *testing.T) (*Runner, *fakeGateway, *notify.Notifier, *sidecar.Store, string) {
	t.Helper()
	g := &fakeGateway{}
	inbox := notify.New(notify.Config{System: func(string, string) error { return nil }})
	reg := workspace.Load("", "node1")
	// The registry stores the resolved path (/private/var on macOS).
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	if _, ok := reg.Ensure(dir); !ok {
		t.Fatal("workspace not registered")
	}
	g.sideDir = t.TempDir()
	side := sidecar.Open(g.sideDir, nil)
	r := New(Config{
		Planner:    sessionkey.PlannerMainKey,
		Inbox:      inbox,
		Workspaces: reg,
		Sessions:   side,
		Spawn: func(_ context.Context, key string, s Spawn) error {
			if g.spawnErr != nil {
				return g.spawnErr
			}
			g.spawned, g.spawns = append(g.spawned, key), append(g.spawns, s)
			return nil
		},
		Note:        func(text string) { g.notes = append(g.notes, text) },
		Tell:        func(text string) error { g.told = append(g.told, text); return nil },
		TellRefused: func(text string) error { g.refused = append(g.refused, text); return nil },
	})
	return r, g, inbox, side, dir
}

func openDecision(t *testing.T, inbox *notify.Notifier, handler string) notify.Item {
	t.Helper()
	open, _ := inbox.List(0)
	for _, it := range open {
		if it.Decision != nil && it.Decision.Handler == handler {
			return it
		}
	}
	t.Fatalf("no open %s card in %+v", handler, open)
	return notify.Item{}
}

// A dispatch block starts one task: session in the workspace's directory at
// once, records its lineage and tells the Planner; a start that fails tells
// the Planner why and leaves no sidecar. No card is opened either way.
func TestDispatch(t *testing.T) {
	cases := []struct {
		name      string
		workspace string // "" is the registered workspace's name
		spawnErr  error
		want      string // in both the note and the receipt
		spawned   bool
	}{
		{name: "spawned", want: "已派出：iso 写 hello（task:", spawned: true},
		{name: "unknown workspace", workspace: "nowhere", want: "派出「iso 写 hello」失败"},
		// The start failing (max_procs full, ...) is relayed as said; spawnError
		// in wireup says it.
		{name: "start fails", spawnErr: errors.New("max_procs full"), want: "max_procs full"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, inbox, side, dir := newRunner(t)
			g.spawnErr = c.spawnErr
			ws := c.workspace
			if ws == "" {
				ws = filepath.Base(dir)
			}
			r.Handle(block(`{"type":"dispatch","workspace":"` + ws + `","title":"iso 写 hello","model":"","message":"在工作区里写一个 hello.md"}`))
			if len(g.notes) != 1 || !strings.Contains(g.notes[0], c.want) {
				t.Errorf("notes = %q, want %q", g.notes, c.want)
			}
			if len(g.told) != 1 || !strings.Contains(g.told[0], c.want) {
				t.Errorf("told = %q, want %q", g.told, c.want)
			}
			if open, _ := inbox.List(0); len(open) != 0 {
				t.Errorf("inbox = %+v, want no card", open)
			}
			if !c.spawned {
				if left, _ := os.ReadDir(g.sideDir); len(g.spawned) != 0 || len(left) != 0 {
					t.Errorf("spawned %q, %d sidecars left", g.spawned, len(left))
				}
				return
			}
			if len(g.spawned) != 1 || !sessionkey.IsTaskKey(g.spawned[0]) {
				t.Fatalf("spawned = %q", g.spawned)
			}
			key := g.spawned[0]
			if s := g.spawns[0]; s.Dir != dir || s.Title != "iso 写 hello" || s.Message != "在工作区里写一个 hello.md" || s.Model != "" {
				t.Errorf("spawn = %+v", s)
			}
			m, ok := side.Get(key)
			if !ok || m.Purpose != sidecar.PurposeTask || m.Parent != sessionkey.PlannerMainKey || m.Title != "iso 写 hello" || m.Dir != dir || !m.DispatchedByPlanner() || m.Created.IsZero() {
				t.Errorf("sidecar = %+v, %v", m, ok)
			}
			if !strings.Contains(g.told[0], key) || !strings.Contains(g.told[0], "notify_when_idle") {
				t.Errorf("told = %q", g.told)
			}
		})
	}
}

// A dispatch card a run before D88 left open is closed: nothing answers it.
func TestOldDispatchCardClosed(t *testing.T) {
	inbox := notify.New(notify.Config{System: func(string, string) error { return nil }})
	inbox.Add(notify.Item{Kind: notify.KindDecision, Source: "dispatch:1", Title: "派出会话：t",
		Decision: &notify.Decision{Handler: handlerDispatch, Actions: []notify.Action{"approve", "reject"}}})
	ask, _ := inbox.Add(notify.Item{Kind: notify.KindQuestion, Source: "ask:1", Title: "q",
		Decision: &notify.Decision{Handler: HandlerAsk, Actions: []notify.Action{"是"}}})
	New(Config{Inbox: inbox})
	if open, _ := inbox.List(0); len(open) != 1 || open[0].ID != ask.ID {
		t.Errorf("open = %+v, want only the question", open)
	}
}

// An ask with options is answered from its card, and the answer goes back to
// the Planner; a refused block only shows in the conversation.
func TestAskAndRefused(t *testing.T) {
	r, g, inbox, _, _ := newRunner(t)
	r.Handle("看这里：\n" + block(`{"type":"ask","question":"今天发版吗？","options":["发","不发"]}`) + "\n" + block(`{"type":"halt"}`))
	it := openDecision(t, inbox, HandlerAsk)
	if it.Kind != notify.KindQuestion || !slices.Equal(it.Decision.Actions, []notify.Action{"发", "不发"}) {
		t.Errorf("ask card = %+v", it)
	}
	if len(g.notes) != 1 || !strings.Contains(g.notes[0], "格式不对") || !strings.Contains(g.notes[0], `{"type":"halt"}`) {
		t.Errorf("notes = %q", g.notes)
	}
	if err := inbox.Decide(it.ID, "不发"); err != nil {
		t.Fatal(err)
	}
	if len(g.told) != 1 || !strings.Contains(g.told[0], "今天发版吗？") || !strings.HasSuffix(g.told[0], "不发") {
		t.Errorf("told = %q", g.told)
	}
}

// A status block sets a task's state as the Planner's judgment and says so
// in the conversation; a task that does not exist, or a state the task
// cannot take, is refused like a malformed block.
func TestStatus(t *testing.T) {
	r, g, _, side, _ := newRunner(t)
	proms := promise.New(promise.Config{})
	p, _ := proms.Add(promise.Promise{Text: "CI 绿了告诉你", CloseWhen: "master CI 绿"})
	if err := side.Put(sidecar.Meta{Key: "task:ab12", Purpose: sidecar.PurposeTask, Title: "iso 写 hello"}); err != nil {
		t.Fatal(err)
	}
	r.cfg.Tasks = task.New(task.Config{Sessions: side, Promises: proms})
	cases := []struct {
		body string
		note string
	}{
		{`{"type":"status","task":"task:ab12","state":"done","why":"它汇报 hello.md 写好了"}`, "Planner 判断任务「iso 写 hello」已完成：它汇报 hello.md 写好了"},
		{`{"type":"status","task":"` + p.ID + `","state":"done","why":"CI 绿了"}`, "Planner 判断任务「CI 绿了告诉你」已完成：CI 绿了"},
		{`{"type":"status","task":"task:nope","state":"done","why":"w"}`, `格式不对，没有执行：找不到任务 "task:nope"`},
		{`{"type":"status","task":"` + p.ID + `","state":"failed","why":"w"}`, "格式不对，没有执行：承诺没有失败状态"},
	}
	for _, c := range cases {
		r.Handle(block(c.body))
		if last := g.notes[len(g.notes)-1]; !strings.Contains(last, c.note) {
			t.Errorf("%s: note %q, want %q", c.body, last, c.note)
		}
	}
	if m, _ := side.Get("task:ab12"); m.Mark == nil || m.Mark.State != "done" || m.Mark.By != "planner" || m.Mark.Why != "它汇报 hello.md 写好了" {
		t.Errorf("mark = %+v", m.Mark)
	}
	if got, _ := proms.Get(p.ID); got.State != promise.StateClosed || got.ClosedBy != promise.SourcePlanner {
		t.Errorf("promise = %+v", got)
	}
}

// A dispatched session that reported to the Planner during a turn gets the
// Planner one receipt when the reply leaves the task 进行中 with no status
// block, or marks it done with a promise citing it open; a doing block is an
// answer. The next turn, with no new report, gets none. {P} is the citing
// promise's ID.
func TestReportWithoutStatusGetsReceipt(t *testing.T) {
	const (
		taskDoing   = `{"type":"status","task":"task:ab12","state":"doing","why":"它说还在等 CI"}`
		taskDone    = `{"type":"status","task":"task:ab12","state":"done","why":"它汇报写好了"}`
		promiseDone = `{"type":"status","task":"{P}","state":"done","why":"已告诉用户"}`
	)
	cases := []struct {
		name   string
		sender string
		reply  string
		want   string // "" for no receipt
	}{
		{"no status", "task:ab12", "做完了，hello.md 已写好。",
			"task:ab12（iso 写 hello）刚向你汇报，它在任务里仍是进行中；完成了就用 status 标 done，没完成用 doing 并说明原因（" + statusFormat("task:ab12") + "）；相关的承诺 {P} 也一并处理，task 填承诺 ID。"},
		{"task and promise", "task:ab12", "还没完。\n" + block(taskDoing) + "\n" + block(promiseDone), ""},
		{"task only", "task:ab12", "做完了。\n" + block(taskDone),
			"task:ab12（iso 写 hello）刚向你汇报；相关的承诺 {P} 还没完成，条件满足了就用 status 标 done（" + statusFormat("{P}") + "）。"},
		{"doing only", "task:ab12", "还在等 CI。\n" + block(taskDoing), ""},
		{"not dispatched", "dashboard:direct:x:general", "收到。", ""},
		// Marked done with no report in the turn: reminded once, though
		// the next reply marks it done again.
		{"done without report", "", "它做完了。\n" + block(taskDone),
			"你刚把 task:ab12（iso 写 hello）标为已完成；相关的承诺 {P} 还没完成，条件满足了就用 status 标 done（" + statusFormat("{P}") + "）。"},
		{"done and promise without report", "", "它做完了。\n" + block(taskDone) + "\n" + block(promiseDone), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, _, side, _ := newRunner(t)
			proms := promise.New(promise.Config{})
			cited, _ := proms.Add(promise.Promise{Text: "task:ab12 做完后告诉用户结果"})
			proms.Add(promise.Promise{Text: "明早提醒你看 CI"})
			if err := side.Put(sidecar.Meta{Key: "task:ab12", Purpose: sidecar.PurposeTask, Title: "iso 写 hello"}); err != nil {
				t.Fatal(err)
			}
			r.cfg.Promises, r.cfg.Tasks = proms, task.New(task.Config{Sessions: side, Promises: proms})
			now := r.last
			r.cfg.Now = func() time.Time { return now }
			reportAt := now.Add(time.Second)
			r.cfg.Reports = func(from, to time.Time) []string {
				if c.sender != "" && reportAt.After(from) && !reportAt.After(to) {
					return []string{c.sender, c.sender}
				}
				return nil
			}
			for range 2 {
				now = now.Add(2 * time.Second)
				r.Result(sessionkey.PlannerMainKey, strings.ReplaceAll(c.reply, "{P}", cited.ID))
				r.handle(<-r.ch)
			}
			var want []string
			if c.want != "" {
				want = []string{strings.ReplaceAll(c.want, "{P}", cited.ID)}
			}
			if !slices.Equal(g.told, want) {
				t.Errorf("told = %q, want %q", g.told, want)
			}
		})
	}
}

// TestStatusFormatParses: the format a receipt spells out is a block Parse
// accepts as written, with only why to fill in.
func TestStatusFormatParses(t *testing.T) {
	text := strings.ReplaceAll(strings.TrimPrefix(statusFormat("task:ab12"), "格式："), "…", "汇报说已写完")
	blocks := Parse(text)
	if len(blocks) != 1 || blocks[0].Err != nil || blocks[0].Status == nil {
		t.Fatalf("Parse(%q) = %+v", text, blocks)
	}
	if st := blocks[0].Status; st.Task != "task:ab12" || st.State != "done" {
		t.Errorf("status = %+v", st)
	}
}

// A turn outside content brought in (a scout run, a pre-meeting brief, the
// receipt of a refusal in one) runs only its promise and ask blocks: a
// dispatch or status block is refused with a line in the conversation,
// judged by the turn's source as its result arrives, and the Planner gets
// one refusal receipt for a run of such turns, so a receipt's turn refused
// again does not go round.
func TestOutsideTurnRefuses(t *testing.T) {
	r, g, inbox, _, dir := newRunner(t)
	dispatch := block(`{"type":"dispatch","workspace":"` + filepath.Base(dir) + `","title":"iso 写 hello","model":"","message":"写一个 hello.md"}`)
	status := block(`{"type":"status","task":"task:ab12","state":"done","why":"邮件说做完了"}`)
	ask := block(`{"type":"ask","question":"今天发版吗？","options":[]}`)
	const (
		noDispatch = "触发的回合不能派活：「iso 写 hello」没有派出"
		noStatus   = "触发的回合不能改任务状态：「task:ab12」没有标为 done"
	)
	steps := []struct {
		outside string
		reply   string
		spawned bool
		notes   []string // each after the outside label
		receipt bool     // a refusal receipt sent
	}{
		{outside: "巡检", reply: dispatch + "\n" + status + "\n" + ask, notes: []string{noDispatch, noStatus}, receipt: true},
		// The receipt's own turn dispatches again: refused, not told again.
		{outside: "被拒动作的回执", reply: dispatch, notes: []string{noDispatch}},
		// A turn of the user's own dispatches, and ends the run.
		{reply: dispatch, spawned: true},
		{outside: "会前简报", reply: dispatch, notes: []string{noDispatch}, receipt: true},
	}
	for i, st := range steps {
		g.spawned, g.notes, g.refused = nil, nil, nil
		r.cfg.Outside = func() string { return st.outside }
		r.Result(r.cfg.Planner, st.reply)
		r.handle(<-r.ch)
		if got := len(g.spawned) == 1; got != st.spawned {
			t.Errorf("step %d: spawned = %q, want spawned %v", i, g.spawned, st.spawned)
		}
		if st.spawned {
			continue
		}
		var want []string
		for _, n := range st.notes {
			want = append(want, st.outside+n)
		}
		if !slices.Equal(g.notes, want) {
			t.Errorf("step %d: notes = %q, want %q", i, g.notes, want)
		}
		if got := len(g.refused) == 1; got != st.receipt || (got && !strings.Contains(g.refused[0], "这一轮由"+st.outside+"触发")) {
			t.Errorf("step %d: refusal receipts = %q, want one: %v", i, g.refused, st.receipt)
		}
	}
	if open, _ := inbox.List(0); len(open) != 1 || open[0].Kind != notify.KindQuestion {
		t.Errorf("inbox = %+v, want the question", open)
	}
}
