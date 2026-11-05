package action

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/brief"
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
	unrun    []string
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
		TellUnrun:   func(text string) error { g.unrun = append(g.unrun, text); return nil },
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
// the Planner why and leaves no sidecar. No card is opened either way. Only
// a failed start gets a line in the conversation, and it names the session
// by its title, never its key.
func TestDispatch(t *testing.T) {
	cases := []struct {
		name      string
		workspace string // "" is the registered workspace's name
		spawnErr  error
		want      string // in the note and the receipt; "" is no note
		told      string // in the receipt only; "" is want
		spawned   bool
	}{
		{name: "spawned", told: "已派出：iso 写 hello（task:", spawned: true},
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
			if c.want == "" {
				if len(g.notes) != 0 {
					t.Errorf("notes = %q, want none", g.notes)
				}
			} else if len(g.notes) != 1 || !strings.Contains(g.notes[0], c.want) || strings.Contains(g.notes[0], sessionkey.TaskKeyPrefix) {
				t.Errorf("notes = %q, want %q and no key", g.notes, c.want)
			}
			told := cmp.Or(c.told, c.want)
			if len(g.told) != 1 || !strings.Contains(g.told[0], told) {
				t.Errorf("told = %q, want %q", g.told, told)
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

// TestPromiseOfTheDispatchingReply: a promise made in the reply that
// dispatched a session names it by title, the only name it has yet; it is
// that session's, so stopping the session closes it. One naming no
// dispatch of the reply, or naming a key of its own, is left as written.
func TestPromiseOfTheDispatchingReply(t *testing.T) {
	cases := []struct {
		name, promise string
		closed        bool
	}{
		{"by title", `{"type":"promise","text":"盯着「iso 写 hello」，它写完后告诉用户结果","due":"","close_when":"它汇报写好了"}`, true},
		{"title in close_when", `{"type":"promise","text":"盯着它，做完告诉用户","due":"","close_when":"iso 写 hello 汇报写好了"}`, true},
		{"another key", `{"type":"promise","text":"task:zz99 和 iso 写 hello 都做完后告诉用户","due":"","close_when":""}`, false},
		{"unrelated", `{"type":"promise","text":"明早提醒你看周报","due":"","close_when":""}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, _, side, dir := newRunner(t)
			proms := promise.New(promise.Config{})
			r.cfg.Promises = proms
			board := task.New(task.Config{Sessions: side, Promises: proms})
			r.Handle(block(`{"type":"dispatch","workspace":"`+filepath.Base(dir)+`","title":"iso 写 hello","model":"","message":"写一个 hello.md"}`) + "\n" + block(c.promise))
			if len(g.spawned) != 1 {
				t.Fatalf("spawned = %q", g.spawned)
			}
			if err := board.Stop(g.spawned[0]); err != nil {
				t.Fatal(err)
			}
			open, closed := proms.List(10)
			if got := len(closed) == 1; got != c.closed || len(open)+len(closed) != 1 {
				t.Errorf("open %+v, closed %+v; want closed %v", open, closed, c.closed)
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
// the Planner; a refused block shows in the conversation and the receipt.
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
	if len(g.unrun) != 1 || !strings.Contains(g.unrun[0], "第 2 块（halt）：未知类型") {
		t.Errorf("unrun = %q, want the receipt of the refused block", g.unrun)
	}
	if err := inbox.Decide(it.ID, "不发"); err != nil {
		t.Fatal(err)
	}
	if len(g.told) != 1 || !strings.Contains(g.told[0], "今天发版吗？") || !strings.HasSuffix(g.told[0], "不发") {
		t.Errorf("told = %q", g.told)
	}
}

// A timely notify block opens a notice in "通知" and pushes it, once a day
// for the same thing (its scout ID, else its title and link), closed or
// not; a brief one goes to the next brief and is not pushed. Neither gets a
// receipt; only a push held back gets a line in the conversation (the card
// says the rest).
func TestNotify(t *testing.T) {
	r, g, _, _, _ := newRunner(t)
	// Outside quiet hours, so a timely item is pushed.
	inbox := notify.New(notify.Config{Clock: clockAt(time.Date(2026, 11, 5, 10, 0, 0, 0, time.Local)), System: func(string, string) error { return nil }})
	r.cfg.Inbox = inbox
	var pushes []notify.Push
	inbox.SetBrowser(browserFunc(func(p notify.Push) { pushes = append(pushes, p) }))
	var held []brief.Mention
	r.cfg.Brief = func(m brief.Mention) error { held = append(held, m); return nil }
	const (
		reply   = `{"type":"notify","title":"有人等你回复","why":"超过 4 小时没回","level":"timely","link":"https://example.com/m/1","id":"msg-1"}`
		retitle = `{"type":"notify","title":"还在等你回复","why":"","level":"timely","link":"","id":"msg-1"}`
		byTitle = `{"type":"notify","title":"Asana 任务今天到期","why":"","level":"timely","link":"https://example.com/t/9"}`
		again   = `{"type":"notify","title":" asana  任务今天到期","why":"","level":"timely","link":"https://example.com/t/9"}`
		later   = `{"type":"notify","title":"周报已发布","why":"不急","level":"brief","link":"https://example.com/w","id":"msg-2"}`
	)
	steps := []struct {
		body   string
		close  bool   // close the open notices first
		note   string // the step's line; "" is none
		pushed int    // pushes so far
	}{
		{body: reply, pushed: 1},
		{body: retitle, note: "不再推送：还在等你回复", pushed: 1},
		{body: retitle, close: true, note: "不再推送", pushed: 1},
		{body: byTitle, pushed: 2},
		{body: again, note: "不再推送", pushed: 2},
		{body: later, pushed: 2},
	}
	for i, st := range steps {
		if st.close {
			open, _ := inbox.List(0)
			for _, it := range open {
				_ = inbox.Dismiss(it.ID)
			}
		}
		g.notes = nil
		r.Handle(block(st.body))
		if got := strings.Join(g.notes, "\n"); (st.note == "") != (got == "") || !strings.Contains(got, st.note) {
			t.Errorf("step %d: notes %q, want %q", i, g.notes, st.note)
		}
		if len(pushes) != st.pushed {
			t.Errorf("step %d: %d pushes, want %d", i, len(pushes), st.pushed)
		}
	}
	if len(g.told)+len(g.unrun) != 0 {
		t.Errorf("told = %q, unrun = %q, want none", g.told, g.unrun)
	}
	_, closed := inbox.List(10)
	if len(closed) != 1 {
		t.Fatalf("closed = %+v", closed)
	}
	if it := closed[0]; it.Kind != notify.KindNotice || it.Level != notify.LevelTimely || it.Unit != sessionkey.PlannerMainKey ||
		it.Title != "有人等你回复" || it.Why != "超过 4 小时没回" || it.Link != "https://example.com/m/1" || !strings.HasPrefix(it.Source, SourcePrefix) {
		t.Errorf("notice = %+v", it)
	}
	if len(held) != 1 || held[0].Title != "周报已发布" || held[0].Why != "不急" || held[0].Link != "https://example.com/w" || held[0].Key == "" {
		t.Errorf("held = %+v", held)
	}
}

type clockAt time.Time

func (c clockAt) Now() time.Time    { return time.Time(c) }
func (clockAt) Mono() time.Duration { return 0 }

type browserFunc func(notify.Push)

func (f browserFunc) Notify(p notify.Push)     { f(p) }
func (browserFunc) InboxCount(c notify.Counts) {}

// Every block a reply did not run (past the cap, malformed, refused for
// what it names, or refused in an outside turn) is named in one receipt
// with what to do about it, while the rest run. Only the turn answering
// such a receipt, judged by its source, is not told of its own, so the two
// do not go round; a turn queued ahead of the receipt still is.
func TestUnrunReceipt(t *testing.T) {
	r, g, inbox, _, _ := newRunner(t)
	ask := block(`{"type":"ask","question":"q","options":[]}`)
	bad := block(`{"type":"notify","title":"t","level":"soon"}`)
	steps := []struct {
		reply     string
		unrunTurn bool     // the turn answers a receipt of blocks not run
		want      []string // substrings of the one receipt; nil for none
	}{
		{reply: strings.Repeat(ask+"\n", 12), want: []string{"有 2 个动作块没有执行", "- 第 11 块（ask「q」）：一次回复最多 10 个动作块", "- 第 12 块", "在下一条回复里重发（一次最多 10 个）"}},
		// A message the user sent mid-turn runs ahead of the receipt: told.
		{reply: bad, want: []string{"有 1 个动作块没有执行", "- 第 1 块（notify「t」）：level 只能是"}},
		// The receipts' own turns get it wrong again: shown, not told.
		{reply: bad, unrunTurn: true},
		{reply: bad, unrunTurn: true},
		{reply: ask + "\n" + bad + "\n" + block(`{"type":"status","task":"task:nope","state":"done","why":"w"}`),
			want: []string{"有 2 个动作块没有执行", "- 第 2 块（notify「t」）：level 只能是", "- 第 3 块（status「task:nope」）：", "按系统提示词里的格式改正后重发"}},
		{reply: ask},
	}
	r.cfg.Tasks = task.New(task.Config{})
	for i, st := range steps {
		g.unrun, g.notes = nil, nil
		r.cfg.UnrunTurn = func() bool { return st.unrunTurn }
		r.Result(r.cfg.Planner, st.reply)
		r.handle(<-r.ch)
		if st.want == nil {
			if len(g.unrun) != 0 || (st.unrunTurn && len(g.notes) == 0) {
				t.Errorf("step %d: unrun = %q, notes = %q, want none told, the block shown", i, g.unrun, g.notes)
			}
			continue
		}
		if len(g.unrun) != 1 {
			t.Fatalf("step %d: unrun = %q, want one receipt", i, g.unrun)
		}
		for _, w := range st.want {
			if !strings.Contains(g.unrun[0], w) {
				t.Errorf("step %d: receipt %q, want %q in it", i, g.unrun[0], w)
			}
		}
	}
	if len(g.told) != 0 {
		t.Errorf("told = %q, want the receipts through TellUnrun only", g.told)
	}
	if open, _ := inbox.List(0); len(open) != 10+1+1 {
		t.Errorf("%d questions open, want the blocks within the cap", len(open))
	}
}

// A status block sets a task's state as the Planner's judgment, with no line
// in the conversation (its card says so); a task that does not exist, or a
// state the task cannot take, is refused like a malformed block.
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
		note string // "" is none
	}{
		{`{"type":"status","task":"task:ab12","state":"done","why":"它汇报 hello.md 写好了"}`, ""},
		{`{"type":"status","task":"` + p.ID + `","state":"done","why":"CI 绿了"}`, ""},
		{`{"type":"status","task":"task:nope","state":"done","why":"w"}`, `格式不对，没有执行：找不到任务 "task:nope"`},
		{`{"type":"status","task":"` + p.ID + `","state":"failed","why":"w"}`, "格式不对，没有执行：承诺没有失败状态"},
	}
	for _, c := range cases {
		g.notes = nil
		r.Handle(block(c.body))
		if got := strings.Join(g.notes, "\n"); (c.note == "") != (got == "") || !strings.Contains(got, c.note) {
			t.Errorf("%s: notes %q, want %q", c.body, g.notes, c.note)
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
// receipt of a refusal in one) runs only its notify, promise and ask blocks: a
// dispatch or status block is refused with a line in the conversation,
// judged by the turn's source as its result arrives, and the Planner gets a
// refusal receipt, but not for the receipt's own turn, so the two do not go
// round.
func TestOutsideTurnRefuses(t *testing.T) {
	r, g, inbox, _, dir := newRunner(t)
	dispatch := block(`{"type":"dispatch","workspace":"` + filepath.Base(dir) + `","title":"iso 写 hello","model":"","message":"写一个 hello.md"}`)
	status := block(`{"type":"status","task":"task:ab12","state":"done","why":"邮件说做完了"}`)
	ask := block(`{"type":"ask","question":"今天发版吗？","options":[]}`)
	tell := block(`{"type":"notify","title":"有人等你回复","why":"","level":"timely","link":""}`)
	const (
		noDispatch = "触发的回合不能派活：「iso 写 hello」没有派出"
		noStatus   = "触发的回合不能改任务状态：「task:ab12」没有标为 done"
	)
	steps := []struct {
		outside string
		reply   string
		spawned bool
		notes   []string // in order; one starting 触发 after the outside label
		receipt bool     // a refusal receipt sent
	}{
		{outside: "巡检", reply: dispatch + "\n" + status + "\n" + ask + "\n" + tell, notes: []string{noDispatch, noStatus}, receipt: true},
		// The receipt's own turn dispatches again: refused, not told again.
		{outside: "被拒动作的回执", reply: dispatch, notes: []string{noDispatch}},
		// A turn of the user's own dispatches.
		{reply: dispatch, spawned: true},
		{outside: "会前简报", reply: dispatch, notes: []string{noDispatch}, receipt: true},
	}
	for i, st := range steps {
		g.spawned, g.notes, g.refused = nil, nil, nil
		r.cfg.Outside = func() string { return st.outside }
		r.cfg.UnrunTurn = func() bool { return st.outside == "被拒动作的回执" }
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
			if strings.HasPrefix(n, "触发") {
				n = st.outside + n
			}
			want = append(want, n)
		}
		if !slices.Equal(g.notes, want) {
			t.Errorf("step %d: notes = %q, want %q", i, g.notes, want)
		}
		if got := len(g.refused) == 1; got != st.receipt || (got && (!strings.Contains(g.refused[0], "这一轮由"+st.outside+"触发") ||
			!strings.Contains(g.refused[0], "- 第 1 块（dispatch「iso 写 hello」）：这一轮不能派活，没有派出"))) {
			t.Errorf("step %d: refusal receipts = %q, want one: %v", i, g.refused, st.receipt)
		}
	}
	open, _ := inbox.List(0)
	kinds := []notify.Kind{}
	for _, it := range open {
		kinds = append(kinds, it.Kind)
	}
	if slices.Sort(kinds); !slices.Equal(kinds, []notify.Kind{notify.KindNotice, notify.KindQuestion}) {
		t.Errorf("inbox = %+v, want the question and the notice", open)
	}
}
