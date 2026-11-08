package action

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/brief"
	"github.com/naozhi/naozhi/internal/planner/digest"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/task"
	"github.com/naozhi/naozhi/internal/planner/workspace"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/testhelper"
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
			r.Handle(block(`{"type":"dispatch","workspace":"` + ws + `","title":"iso 写 hello","goal":"写好 hello.md","model":"","message":"在工作区里写一个 hello.md"}`))
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
			if s := g.spawns[0]; s.Dir != dir || s.Title != "iso 写 hello" || s.Message != "在工作区里写一个 hello.md" || s.Model != "" || s.Goal != "写好 hello.md" {
				t.Errorf("spawn = %+v", s)
			}
			m, ok := side.Get(key)
			if !ok || m.Purpose != sidecar.PurposeTask || m.Parent != sessionkey.PlannerMainKey || m.Title != "iso 写 hello" || m.Dir != dir || !m.DispatchedByPlanner() || m.Created.IsZero() {
				t.Errorf("sidecar = %+v, %v", m, ok)
			}
			if !strings.Contains(g.told[0], key) || strings.Contains(g.told[0], "notify_when_idle") || !strings.Contains(g.told[0], "在回复末尾写汇报块，网关一行转给你") {
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

// TestAskLongOption: an option over optionRunes is cut to optionRunes, "…"
// included, and the question still goes out (an option of 42 runes once
// refused the whole block); the Planner is told in one line, and an ask
// with options that fit gets no receipt at all.
func TestAskLongOption(t *testing.T) {
	r, g, inbox, _, _ := newRunner(t)
	fits := strings.Repeat("好", optionRunes)
	long := strings.Repeat("长", optionRunes+5)
	r.Handle(block(`{"type":"ask","question":"选哪个？","options":["` + fits + `"]}`))
	if len(g.unrun) != 0 {
		t.Fatalf("a receipt for options that fit: %q", g.unrun)
	}
	for _, it := range func() []notify.Item { l, _ := inbox.List(0); return l }() {
		_ = inbox.Dismiss(it.ID)
	}
	r.Handle(block(`{"type":"ask","question":"选哪个？","options":["` + long + `","` + fits + `"]}`))
	it := openDecision(t, inbox, HandlerAsk)
	want := strings.Repeat("长", optionRunes-1) + "…"
	if !slices.Equal(it.Decision.Actions, []notify.Action{notify.Action(want), notify.Action(fits)}) {
		t.Errorf("actions = %q, want %q and the one that fits", it.Decision.Actions, want)
	}
	if len(g.notes) != 0 {
		t.Errorf("notes = %q, want none: the block ran", g.notes)
	}
	if len(g.unrun) != 1 || !strings.Contains(g.unrun[0], "都执行了") || !strings.Contains(g.unrun[0], "第 1 块（ask「选哪个？」）：1 个选项超过 80 字") ||
		strings.Contains(g.unrun[0], "没有执行") {
		t.Errorf("unrun = %q, want one line on the cut option", g.unrun)
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

// TestTaskEndedAcrossShiftChange: dispatched, then the shift changes (the new
// conversation holds no subscription and none of the old one's reports),
// then the session ends its turn without reporting. The gateway tells the
// Planner once a busy turn is over, and the status block it answers with
// marks the task done. A session whose report came gets no receipt.
func TestTaskEndedAcrossShiftChange(t *testing.T) {
	for _, reported := range []bool{false, true} {
		r, g, _, side, dir := newRunner(t)
		r.grace = 0
		r.cfg.Tasks = task.New(task.Config{Sessions: side})
		now := time.Now()
		r.cfg.Now = func() time.Time { return now }
		busy := true
		r.cfg.PlannerBusy = func() bool { return busy }
		var reports []string
		r.cfg.Reports = func(time.Time, time.Time) []string { return reports }
		next := func() ended {
			t.Helper()
			select {
			case e := <-r.endCh:
				return e
			case <-time.After(5 * time.Second):
				t.Fatal("turn end never queued for a check")
				return ended{}
			}
		}

		r.Handle(block(`{"type":"dispatch","workspace":"` + filepath.Base(dir) + `","title":"iso 写 hello","model":"","message":"写一个 hello.md"}`))
		if len(g.spawned) != 1 {
			t.Fatalf("spawned = %q", g.spawned)
		}
		key := g.spawned[0]
		g.told = nil
		// The shift change: nothing the runner holds goes with it.
		now = now.Add(time.Hour)
		if reported {
			reports = []string{key}
		}
		r.Result(key, "hello.md 写好了")
		r.checkEnded(next()) // the Planner is busy: checked again later
		if len(g.told) != 0 {
			t.Fatalf("told while the Planner was busy: %q", g.told)
		}
		if reported {
			select {
			case e := <-r.endCh:
				t.Errorf("reported: checked again: %+v", e)
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		busy = false
		r.checkEnded(next())
		if len(g.told) != 1 || !strings.Contains(g.told[0], key) || !strings.Contains(g.told[0], "hello.md 写好了") ||
			!strings.Contains(g.told[0], statusFormat(key)) {
			t.Fatalf("told = %q", g.told)
		}
		r.Result(key, "又一轮") // one receipt until it reports
		r.checkEnded(next())
		if len(g.told) != 1 {
			t.Errorf("a second receipt: %q", g.told)
		}
		r.Handle(block(`{"type":"status","task":"` + key + `","state":"done","why":"它说 hello.md 写好了"}`))
		if st := r.cfg.Tasks.List()[0]; st.ID != key || st.State != task.Done {
			t.Errorf("task = %+v, want done", st)
		}
	}
}

// endRig is a runner with one dispatched task whose turn ends are checked
// at once, on a clock the test moves: end ends a turn with text; held are
// the checks waiting longer than the grace (doingQuiet's end), run by fire;
// reported is what Reports returns.
type endRig struct {
	t        *testing.T
	r        *Runner
	g        *fakeGateway
	inbox    *notify.Notifier
	side     *sidecar.Store
	key      string
	now      time.Time
	held     []func()
	reported []string
}

func endedRunner(t *testing.T) *endRig {
	t.Helper()
	r, g, inbox, side, dir := newRunner(t)
	e := &endRig{t: t, r: r, g: g, inbox: inbox, side: side}
	r.grace, r.reportGrace = 0, 0
	r.after = func(d time.Duration, f func()) {
		if d > 0 {
			e.held = append(e.held, f)
			return
		}
		f()
	}
	r.cfg.Tasks = task.New(task.Config{Sessions: side, OnState: r.TaskState})
	r.cfg.Reports = func(time.Time, time.Time) []string { return e.reported }
	e.now = time.Now()
	r.cfg.Now = func() time.Time { return e.now }
	r.Handle(block(`{"type":"dispatch","workspace":"` + filepath.Base(dir) + `","title":"iso 跑测试","model":"","message":"跑一遍测试"}`))
	e.key = g.spawned[0]
	g.told = nil
	return e
}

func (e *endRig) wait(d time.Duration) { e.now = e.now.Add(d) }

// check runs the turn-end checks queued.
func (e *endRig) check() {
	for {
		select {
		case x := <-e.r.endCh:
			e.r.checkEnded(x)
		default:
			return
		}
	}
}

func (e *endRig) end(text string) {
	e.t.Helper()
	e.r.Result(e.key, text)
	e.check()
}

// receipts are the turn-end receipts told, apart from the report reminders.
func (e *endRig) receipts() []string {
	var out []string
	for _, x := range e.g.told {
		if strings.Contains(x, "这一轮已经结束") {
			out = append(out, x)
		}
	}
	return out
}

// fire runs the held checks.
func (e *endRig) fire() {
	held := e.held
	e.held = nil
	for _, f := range held {
		f()
	}
	e.check()
}

// planner handles a Planner reply whose turn ran since the previous one.
func (e *endRig) planner(text string) {
	e.r.Result(e.r.cfg.Planner, text)
	e.r.handle(<-e.r.ch)
}

func (e *endRig) doing(why string) {
	e.planner(block(`{"type":"status","task":"` + e.key + `","state":"doing","why":"` + why + `"}`))
}

// TestTaskEndedReceiptLoop: the receipt asks the Planner for a doing block,
// so a doing block answering it must not re-arm it at once, else every
// unreported turn end brings another receipt and another doing (one task
// got 21 in 80 minutes). A turn end within doingQuiet of that block waits
// for the window's end; only the latest is told then, once.
func TestTaskEndedReceiptLoop(t *testing.T) {
	e := endedRunner(t)
	e.end("改好了一半")
	if len(e.g.told) != 1 {
		t.Fatalf("told = %q", e.g.told)
	}
	for n := range 3 {
		e.wait(5 * time.Minute)
		e.doing("还没做完")
		e.wait(5 * time.Minute)
		e.end(fmt.Sprintf("第 %d 轮", n))
		if len(e.g.told) != 1 {
			t.Fatalf("round %d: a receipt within %v of the doing block: %q", n, doingQuiet, e.g.told)
		}
	}
	e.wait(doingQuiet)
	e.fire()
	if len(e.g.told) != 2 || !strings.Contains(e.g.told[1], "第 2 轮") {
		t.Fatalf("after %v: told = %q, want one receipt of the latest turn", doingQuiet, e.g.told)
	}
	e.end("再一轮")
	if len(e.g.told) != 2 {
		t.Errorf("a second receipt before any doing block: %q", e.g.told)
	}
}

// TestTaskEndedLastTurnInQuiet: a task whose last turn ends within
// doingQuiet of the doing block, and that never has another, is told once
// the window ends, so it is not left 进行中; a report meanwhile tells
// nothing.
func TestTaskEndedLastTurnInQuiet(t *testing.T) {
	for _, report := range []bool{false, true} {
		e := endedRunner(t)
		e.end("改好了一半")
		e.wait(5 * time.Minute)
		e.doing("还没做完")
		e.wait(20 * time.Minute)
		e.end("全部做完了")
		if len(e.g.told) != 1 {
			t.Fatalf("report %v: a receipt within the window: %q", report, e.g.told)
		}
		if report {
			e.reported = []string{e.key}
		}
		e.wait(doingQuiet)
		e.fire()
		if want := map[bool]int{false: 2, true: 1}[report]; len(e.g.told) != want || !report && !strings.Contains(e.g.told[1], "全部做完了") {
			t.Errorf("report %v: told = %q, want %d", report, e.g.told, want)
		}
	}
}

// TestTaskEndedQuietOneTimer: a window's many unreported turn ends hold one
// check to its end, not one each: endCh is shared and bounded, and the
// latest is the one told.
func TestTaskEndedQuietOneTimer(t *testing.T) {
	e := endedRunner(t)
	e.end("改好了一半")
	e.wait(time.Minute)
	e.doing("还没做完")
	for n := range 2 * queueLen {
		e.wait(time.Minute)
		e.end(fmt.Sprintf("第 %d 轮", n))
	}
	if len(e.held) != 1 {
		t.Fatalf("held = %d, want one check to the window's end", len(e.held))
	}
	e.wait(doingQuiet)
	e.fire()
	if got := e.receipts(); len(got) != 2 || !strings.Contains(got[1], fmt.Sprintf("第 %d 轮", 2*queueLen-1)) {
		t.Fatalf("receipts = %q, want the latest turn told once", got)
	}

	// A wake lost to a full endCh does not leave the next window unarmed.
	e.wait(time.Minute)
	e.doing("还没做完")
	e.wait(time.Minute)
	e.end("又一轮")
	e.held = nil // the wake never reaches endCh
	e.wait(doingQuiet)
	e.end("窗口过后")
	e.doing("还要再看")
	e.wait(time.Minute)
	e.end("最后一轮")
	if len(e.held) != 1 {
		t.Fatalf("held = %d after a lost wake, want the window armed again", len(e.held))
	}
	e.wait(doingQuiet)
	e.fire()
	if got := e.receipts(); len(got) != 4 || !strings.Contains(got[3], "最后一轮") {
		t.Fatalf("receipts = %q, want the last turn told at the window's end", got)
	}
}

// TestTaskEndedWhileTold: a turn that ends after the receipt, before the
// doing block answering it, is held to the window's end when it is the
// latest, so a task whose last turn it is is not left 进行中.
func TestTaskEndedWhileTold(t *testing.T) {
	e := endedRunner(t)
	e.end("测试在后台跑，跑完再看")
	e.wait(3 * time.Minute)
	e.end("测试全部通过")
	if got := e.receipts(); len(got) != 1 {
		t.Fatalf("receipts = %q", got)
	}
	e.wait(time.Minute)
	e.doing("测试还在后台跑")
	e.wait(doingQuiet)
	e.fire()
	if got := e.receipts(); len(got) != 2 || !strings.Contains(got[1], "测试全部通过") {
		t.Fatalf("receipts = %q, want the turn that ended meanwhile told at the window's end", got)
	}
}

// TestTaskEndedStaleSkipped: a turn end checked after its session ended
// another is not told: its text is stale, and the later turn's own check
// tells that one.
func TestTaskEndedStaleSkipped(t *testing.T) {
	e := endedRunner(t)
	e.r.Result(e.key, "测试在后台跑，跑完再看")
	e.wait(time.Minute)
	e.r.Result(e.key, "测试全部通过")
	e.check()
	if got := e.receipts(); len(got) != 1 || !strings.Contains(got[0], "测试全部通过") {
		t.Fatalf("receipts = %q, want only the latest turn told", got)
	}
}

// TestTaskEndedReportRearms: a report re-arms the receipt as before, its
// doing block answering it starts no quiet window, nor does a report after
// a doing block that answered a receipt: the next unreported turn end is
// told.
func TestTaskEndedReportRearms(t *testing.T) {
	t.Run("doing answers a report", func(t *testing.T) {
		e := endedRunner(t)
		e.reported = []string{e.key}
		e.end("做了一半，汇报了")
		e.doing("它说还要跑测试")
		e.reported = nil
		e.wait(10 * time.Minute)
		e.end("全部做完了")
		if len(e.g.told) != 1 || !strings.Contains(e.g.told[0], "全部做完了") || len(e.held) != 0 {
			t.Fatalf("told = %q held = %d, want the turn end told at once", e.g.told, len(e.held))
		}
	})
	t.Run("a doing block of its own", func(t *testing.T) {
		e := endedRunner(t)
		e.doing("用户说还要补文档")
		e.wait(10 * time.Minute)
		e.end("文档补好了")
		if got := e.receipts(); len(got) != 1 || len(e.held) != 0 {
			t.Fatalf("receipts = %q held = %d, want the turn end told at once", got, len(e.held))
		}
	})
	t.Run("done, then reopened", func(t *testing.T) {
		e := endedRunner(t)
		e.end("改好了一半")
		e.wait(5 * time.Minute)
		e.doing("还没做完")
		e.planner(block(`{"type":"status","task":"` + e.key + `","state":"done","why":"它说做完了"}`))
		if err := e.r.cfg.Tasks.Set(e.key, task.Doing, task.ByUser, "重新打开"); err != nil {
			t.Fatal(err)
		}
		e.wait(10 * time.Minute)
		e.end("又改了一处")
		if got := e.receipts(); len(got) != 2 || len(e.held) != 0 {
			t.Fatalf("receipts = %q held = %d, want the reopened task's turn end told at once", got, len(e.held))
		}
	})
	t.Run("done without a doing block, then reopened", func(t *testing.T) {
		e := endedRunner(t)
		e.end("改好了")
		e.planner(block(`{"type":"status","task":"` + e.key + `","state":"done","why":"它说做完了"}`))
		if err := e.r.cfg.Tasks.Set(e.key, task.Doing, task.ByUser, "重新打开"); err != nil {
			t.Fatal(err)
		}
		e.wait(10 * time.Minute)
		e.end("又改了一处")
		if got := e.receipts(); len(got) != 2 || !strings.Contains(got[1], "又改了一处") {
			t.Fatalf("receipts = %q, want the reopened task's turn end told", got)
		}
	})
	t.Run("report after the doing block", func(t *testing.T) {
		e := endedRunner(t)
		e.end("改好了一半")
		e.wait(5 * time.Minute)
		e.doing("还没做完")
		e.wait(5 * time.Minute)
		e.reported = []string{e.key}
		e.planner("收到汇报。")
		e.reported = nil
		e.wait(10 * time.Minute)
		e.end("又一轮没汇报")
		if got := e.receipts(); len(got) != 2 || !strings.Contains(got[1], "又一轮没汇报") {
			t.Fatalf("receipts = %q, want the turn after the report told", got)
		}
	})
}

// TestTaskEndedBackground: a turn that ends with a workflow or background
// task still running is no end of the task, so it gets no receipt; the turn
// that ends once that work is done, unreported, does, even within
// doingQuiet of a doing block, so the task is not left 进行中.
func TestTaskEndedBackground(t *testing.T) {
	e := endedRunner(t)
	running := true
	e.r.cfg.Background = func(k string) bool { return k == e.key && running }
	e.end("测试已在后台运行，跑完再看")
	if len(e.g.told) != 0 {
		t.Fatalf("told while background work ran: %q", e.g.told)
	}
	running = false
	e.end("测试全部通过")
	if len(e.g.told) != 1 || !strings.Contains(e.g.told[0], "测试全部通过") {
		t.Fatalf("after the background work: told = %q", e.g.told)
	}
	e.doing("还要再跑一遍")
	running = true
	e.wait(time.Minute)
	e.end("第二遍在后台跑")
	running = false
	e.wait(time.Minute)
	e.end("第二遍也通过了")
	if len(e.g.told) != 2 || !strings.Contains(e.g.told[1], "第二遍也通过了") {
		t.Fatalf("after the doing block and the background work: told = %q", e.g.told)
	}
	e.planner(block(`{"type":"status","task":"` + e.key + `","state":"done","why":"它说测试全部通过"}`))
	if st := e.r.cfg.Tasks.List()[0]; st.ID != e.key || st.State != task.Done {
		t.Errorf("task = %+v, want done", st)
	}
	if len(e.r.doingAt) != 0 {
		t.Errorf("doingAt = %v after done, want none", e.r.doingAt)
	}
}

// TestTaskEndedAcrossRestart: a turn end a restart cut short of its check
// (the grace, a busy Planner) is checked by the next run, from the sidecar,
// and only once; a checked one is not checked again.
func TestTaskEndedAcrossRestart(t *testing.T) {
	r, g, _, side, dir := newRunner(t)
	r.cfg.Tasks = task.New(task.Config{Sessions: side})
	r.cfg.Reports = func(time.Time, time.Time) []string { return nil }
	r.Handle(block(`{"type":"dispatch","workspace":"` + filepath.Base(dir) + `","title":"iso 写 hello","model":"","message":"写一个 hello.md"}`))
	key := g.spawned[0]
	g.told = nil
	r.Result(key, "hello.md 写好了") // the restart comes before its check

	cfg := r.cfg
	cfg.Sessions = sidecar.Open(g.sideDir, nil)
	cfg.Tasks = task.New(task.Config{Sessions: cfg.Sessions})
	for run := range 2 {
		r2 := New(cfg)
		r2.grace = 0
		r2.recheck()
		wait := 50 * time.Millisecond
		if run == 0 {
			wait = 5 * time.Second
		}
		select {
		case e := <-r2.endCh:
			if run > 0 {
				t.Fatalf("checked again after the receipt: %+v", e)
			}
			r2.checkEnded(e)
		case <-time.After(wait):
			if run == 0 {
				t.Fatal("the turn end the restart cut short was never checked")
			}
		}
		if len(g.told) != 1 || !strings.Contains(g.told[0], key) || !strings.Contains(g.told[0], "hello.md 写好了") {
			t.Fatalf("run %d: told = %q", run, g.told)
		}
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

// reportReply is a turn's reply that ends with a report block.
func reportReply(state, summary string) string {
	return "做了什么：跑了测试。\n没做什么：没改文档。\n\n" + reportBlock(`{"state":"`+state+`","summary":"`+summary+`"}`)
}

// withDigests gives the rig's runner a digest writer that writes at once;
// it returns the writer.
func (e *endRig) withDigests() *digest.Writer {
	e.t.Helper()
	w := digest.New(digest.Config{Dir: e.t.TempDir(), MinGap: time.Millisecond, Facts: func(string) (digest.Facts, bool) { return digest.Facts{Title: "iso 跑测试"}, true }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	// Before the directory goes: a write in flight would keep it.
	e.t.Cleanup(func() { cancel(); <-done })
	e.r.cfg.Digests = w
	return w
}

// reportEnd ends a turn with text, waits for its digest to be written when
// the runner keeps them, and runs the checks.
func (e *endRig) reportEnd(text string) {
	e.t.Helper()
	e.r.Result(e.key, text)
	if w := e.r.cfg.Digests; w != nil {
		testhelper.Eventually(e.t, func() bool { return w.Path(e.key) != "" }, 5*time.Second, "the digest is never written")
	}
	e.check()
}

// questions are the open "等你" questions of the rig's inbox.
func (e *endRig) questions() []notify.Item {
	open, _ := e.inbox.List(0)
	return slices.DeleteFunc(open, func(it notify.Item) bool { return it.Kind != notify.KindQuestion })
}

// TestTaskReportBlock: a turn that ends with a report block gets the
// Planner one line, state, summary and the digest's path, whatever the
// state, and the conversation a 📨 note; never the receipt of a turn that
// ended without a report. The sidecar keeps it for the task card, the
// digest its full text; need_user opens a question in "等你" on the
// session, closed by its next turn's end. The reply to a report line that
// leaves the task 进行中 with no status block is reminded, but not of a
// progress report.
func TestTaskReportBlock(t *testing.T) {
	cases := []struct {
		state, word string
		reminded    bool
	}{
		{"done", "完成", true},
		{"blocked", "卡住", true},
		{"need_user", "要你决定", true},
		{"progress", "进度", false},
	}
	for _, c := range cases {
		t.Run(c.state, func(t *testing.T) {
			e := endedRunner(t)
			w := e.withDigests()
			at := e.now
			e.reportEnd(reportReply(c.state, "测试全部通过。文档没动"))
			path := w.Path(e.key)
			want := e.key + "（iso 跑测试）汇报：" + c.word + " · 测试全部通过。文档没动 · 全文 " + path
			if len(e.g.told) != 1 || e.g.told[0] != want {
				t.Fatalf("told = %q, want %q", e.g.told, want)
			}
			if note := "📨 " + e.key + " 汇报：" + c.word + " · 测试全部通过。文档没动"; !slices.Contains(e.g.notes, note) {
				t.Errorf("notes = %q, want %q", e.g.notes, note)
			}
			m, _ := e.side.Get(e.key)
			if m.Report == nil || m.Report.State != c.state || m.Report.Summary != "测试全部通过。文档没动" || !m.Report.At.Equal(at) {
				t.Errorf("sidecar report = %+v", m.Report)
			}
			if m.Turn == nil || m.Turn.Check {
				t.Errorf("turn = %+v, want checked", m.Turn)
			}
			if st, _ := w.State(e.key); st.Report == nil || st.Report.Text != "做了什么：跑了测试。\n没做什么：没改文档。" || st.Report.State != c.state {
				t.Errorf("digest report = %+v", st.Report)
			}
			qs := e.questions()
			if c.state == "need_user" {
				if len(qs) != 1 || qs[0].Unit != e.key || qs[0].Source != ReportSource(e.key) || qs[0].Why != "派出的任务在等你决定" ||
					!strings.Contains(qs[0].Title, "iso 跑测试 要你决定：测试全部通过") {
					t.Errorf("questions = %+v, want one on the session", qs)
				}
			} else if len(qs) != 0 {
				t.Errorf("questions = %+v, want none", qs)
			}

			// The Planner answers with no status block.
			e.g.told = nil
			e.wait(time.Minute)
			e.planner("收到。")
			if got := len(e.g.told) == 1 && strings.Contains(e.g.told[0], "刚向你汇报，它在任务里仍是进行中"); got != c.reminded || !c.reminded && len(e.g.told) != 0 {
				t.Errorf("told after the reply = %q, want a reminder: %v", e.g.told, c.reminded)
			}
			e.planner("再一条。")
			if len(e.g.told) > 1 {
				t.Errorf("reminded again: %q", e.g.told)
			}

			// The next turn ends without one: its receipt comes, the
			// question is closed.
			e.wait(time.Minute)
			e.end("又跑了一遍")
			if got := e.receipts(); len(got) != 1 || !strings.Contains(got[0], "没有写汇报块") || strings.Contains(got[0], "格式不对") {
				t.Errorf("receipts = %q, want one of the unreported turn", got)
			}
			if qs := e.questions(); len(qs) != 0 {
				t.Errorf("questions after the next turn = %+v, want none", qs)
			}
		})
	}

	t.Run("busy Planner", func(t *testing.T) {
		e := endedRunner(t)
		busy := true
		e.r.cfg.PlannerBusy = func() bool { return busy }
		e.r.Result(e.key, reportReply("done", "写好了"))
		e.r.checkEnded(<-e.r.endCh)
		if len(e.g.told) != 0 || len(e.r.endCh) != 1 {
			t.Fatalf("told = %q, queued %d, want the line held for the busy Planner", e.g.told, len(e.r.endCh))
		}
		busy = false
		e.check()
		// No digest writer: the line points at none.
		if want := e.key + "（iso 跑测试）汇报：完成 · 写好了"; len(e.g.told) != 1 || e.g.told[0] != want {
			t.Errorf("told = %q, want %q", e.g.told, want)
		}
	})

	// A digest not written yet holds the line back, up to endGrace after
	// the turn ended; then it is told, pointing where the digest goes.
	t.Run("digest not written", func(t *testing.T) {
		e := endedRunner(t)
		w := digest.New(digest.Config{Dir: t.TempDir()})
		e.r.cfg.Digests = w
		e.r.Result(e.key, reportReply("done", "写好了"))
		e.check()
		if len(e.g.told) != 0 || len(e.held) != 1 {
			t.Fatalf("told = %q held = %d, want the line held", e.g.told, len(e.held))
		}
		e.wait(endGrace / 2)
		e.fire()
		if len(e.g.told) != 0 || len(e.held) != 1 {
			t.Fatalf("told = %q held = %d, want the line still held", e.g.told, len(e.held))
		}
		e.wait(endGrace)
		e.fire()
		if want := e.key + "（iso 跑测试）汇报：完成 · 写好了 · 全文 " + w.File(e.key); len(e.g.told) != 1 || e.g.told[0] != want {
			t.Errorf("told = %q, want %q", e.g.told, want)
		}
	})

	t.Run("done marked", func(t *testing.T) {
		e := endedRunner(t)
		e.reportEnd(reportReply("need_user", "选 A 还是 B"))
		if len(e.questions()) != 1 {
			t.Fatalf("questions = %+v", e.questions())
		}
		e.g.told = nil
		e.planner("它问的我替你记下了。\n" + block(`{"type":"status","task":"`+e.key+`","state":"done","why":"用户说不做了"}`))
		if qs := e.questions(); len(qs) != 0 {
			t.Errorf("questions after done = %+v, want none", qs)
		}
		if len(e.g.told) != 0 {
			t.Errorf("told = %q, want no reminder for a task marked done", e.g.told)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		e := endedRunner(t)
		e.end("做完了。\n" + reportBlock(`{"state":"finished","summary":"写好了"}`))
		got := e.receipts()
		if len(got) != 1 || !strings.Contains(got[0], "没有写汇报块") || !strings.Contains(got[0], "它写了汇报块但格式不对：state 只能是") {
			t.Fatalf("receipts = %q", got)
		}
		if m, _ := e.side.Get(e.key); m.Report != nil {
			t.Errorf("sidecar report = %+v, want none", m.Report)
		}
	})

	// A report ends the quiet a doing block answering a receipt started: the
	// next unreported turn end is told at once.
	t.Run("ends the quiet", func(t *testing.T) {
		e := endedRunner(t)
		e.end("改好了一半")
		e.wait(5 * time.Minute)
		e.doing("还没做完")
		e.wait(5 * time.Minute)
		e.reportEnd(reportReply("progress", "改好了大半"))
		if len(e.g.told) != 2 || !strings.Contains(e.g.told[1], "汇报：进度") {
			t.Fatalf("told = %q, want the report line at once", e.g.told)
		}
		e.planner("收到。")
		e.wait(5 * time.Minute)
		e.end("又一轮没汇报")
		if got := e.receipts(); len(got) != 2 || !strings.Contains(got[1], "又一轮没汇报") || len(e.held) != 0 {
			t.Fatalf("receipts = %q held = %d, want the turn told at once", got, len(e.held))
		}
	})
}

// TestTaskReportAcrossRestart: a turn that ended with a report and that a
// restart cut short of its check gets the report line from the next run,
// read back from the sidecar, never the receipt of an unreported turn.
func TestTaskReportAcrossRestart(t *testing.T) {
	r, g, _, side, dir := newRunner(t)
	r.cfg.Tasks = task.New(task.Config{Sessions: side})
	r.Handle(block(`{"type":"dispatch","workspace":"` + filepath.Base(dir) + `","title":"iso 写 hello","model":"","message":"写一个 hello.md"}`))
	key := g.spawned[0]
	g.told = nil
	r.Result(key, reportReply("done", "hello.md 写好了")) // the restart comes before its check

	cfg := r.cfg
	cfg.Sessions = sidecar.Open(g.sideDir, nil)
	cfg.Tasks = task.New(task.Config{Sessions: cfg.Sessions})
	r2 := New(cfg)
	r2.grace = 0
	r2.recheck()
	select {
	case e := <-r2.endCh:
		r2.checkEnded(e)
	case <-time.After(5 * time.Second):
		t.Fatal("the turn end the restart cut short was never checked")
	}
	if want := key + "（iso 写 hello）汇报：完成 · hello.md 写好了"; len(g.told) != 1 || g.told[0] != want {
		t.Fatalf("told = %q, want %q", g.told, want)
	}
	if !slices.Contains(g.notes, "📨 "+key+" 汇报：完成 · hello.md 写好了") {
		t.Errorf("notes = %q", g.notes)
	}
}

// The status block of a report line answered in the same reply leaves
// nothing to remind of.
func TestReportLineAnsweredByStatus(t *testing.T) {
	e := endedRunner(t)
	e.reportEnd(reportReply("done", "hello.md 写好了"))
	e.g.told = nil
	e.planner("核实过了。\n" + block(`{"type":"status","task":"`+e.key+`","state":"done","why":"它汇报写好了，文件在"}`))
	if len(e.g.told) != 0 {
		t.Errorf("told = %q, want none", e.g.told)
	}
	if st := e.r.cfg.Tasks.List()[0]; st.State != task.Done {
		t.Errorf("task = %+v, want done", st)
	}
}
