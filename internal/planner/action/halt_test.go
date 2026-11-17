package action

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/bgtask"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/custody"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/task"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

func TestHaltCancelParse(t *testing.T) {
	long := strings.Repeat("字", textRunes+1)
	for _, c := range []struct{ raw, err string }{
		{`{"type":"halt","task":"task:ab12","why":"用户说先停下"}`, ""},
		{`{"type":"halt","task":"feishu:direct:alice:general","why":"用户说停"}`, ""},
		{`{"type":"cancel","task":"task:ab12","why":"用户说不用做了"}`, ""},
		{`{"type":"halt","task":" ","why":"w"}`, "缺少 task"},
		{`{"type":"halt","task":"task:ab12","why":" "}`, "缺少 why"},
		{`{"type":"cancel","task":"task:ab12","why":"` + long + `"}`, "why 超过"},
		{`{"type":"halt","task":"task:ab12","why":"w","key":"task:ab12"}`, "字段"},
		{`{"type":"cancel","task":"task:ab12","why":"w","state":"done"}`, "字段"},
		{`{"type":"cancel","task":"dashboard:direct:u:general","why":"w"}`, "task: key"},
		{`{"type":"nope"}`, "halt、cancel"},
	} {
		b := Parse(block(c.raw))
		if len(b) != 1 {
			t.Fatalf("%s: %d blocks", c.raw, len(b))
		}
		ok := b[0].Halt != nil || b[0].Cancel != nil
		switch {
		case c.err == "" && (b[0].Err != nil || !ok):
			t.Errorf("%s: %v", c.raw, b[0].Err)
		case c.err != "" && (b[0].Err == nil || !strings.Contains(b[0].Err.Error(), c.err) || ok):
			t.Errorf("%s: err %v, want one with %q and no block", c.raw, b[0].Err, c.err)
		}
	}
}

// haltRig is a runner with a dispatched task, custody faked, the task
// board real, and the process stops and the board's stops recorded.
type haltRig struct {
	r       *Runner
	g       *fakeGateway
	inbox   *notify.Notifier
	side    *sidecar.Store
	fc      *fakeCustody
	key     string
	stopped []string // StopProcess calls
	boardSt []string // task.Config.Stop calls, key|promises
	asking  map[string]bool
}

func newHaltRig(t *testing.T) *haltRig {
	t.Helper()
	r, g, inbox, side, dir := newRunner(t)
	h := &haltRig{r: r, g: g, inbox: inbox, side: side, fc: &fakeCustody{on: map[string]bool{}, settled: true}, asking: map[string]bool{}}
	proms := promise.New(promise.Config{})
	r.cfg.Promises = proms
	r.cfg.Tasks = task.New(task.Config{Sessions: side, Promises: proms, Stop: func(key, _ string, promises []string) {
		h.boardSt = append(h.boardSt, key+"|"+strings.Join(promises, ","))
	}})
	r.cfg.Custody = h.fc
	r.cfg.StopProcess = func(key string) { h.stopped = append(h.stopped, key) }
	r.cfg.Asking = func(key string) bool { return h.asking[key] }
	r.Handle(block(`{"type":"dispatch","workspace":"` + filepath.Base(dir) + `","title":"iso 跑基准","model":"","message":"跑基准"}`))
	if len(g.spawned) != 1 {
		t.Fatalf("spawned = %q", g.spawned)
	}
	h.key = g.spawned[0]
	h.fc.on[h.key] = true
	g.told, g.notes = nil, nil
	return h
}

// run has the Planner reply text in a turn of words (none: not the
// user's) and outside ("" for none).
func (h *haltRig) run(text string, outside string, words ...Words) {
	h.r.cfg.Words = func() []Words { return words }
	h.r.cfg.Outside = func() string { return outside }
	h.r.Result(h.r.cfg.Planner, text)
	h.r.handle(<-h.r.ch)
}

// halt and cancel run only in a turn the user started: their message, or
// their answer to a question. A receipt, a reminder, custody, the receipt
// of blocks not run, a scout run, a meeting brief and a lookup's answer
// are refused by the turn's source, said in the receipt.
func TestHaltCancelTurnSources(t *testing.T) {
	stop := Words{Text: "先把基准停了，那个不用做了", At: time.Now()}
	answer := Words{Text: "停", Question: "基准还跑吗？", At: time.Now()}
	cases := []struct {
		name    string
		words   []Words
		outside string
		unrun   bool // the turn answers a receipt of blocks not run
		runs    bool
		receipt string // in the one receipt, "" for none
	}{
		{name: "the user's message", words: []Words{stop}, runs: true},
		{name: "the user's answer", words: []Words{answer}, runs: true},
		{name: "a receipt", receipt: notUserStop},
		{name: "a reminder", receipt: notUserStop},
		{name: "custody", receipt: notUserStop},
		{name: "the receipt of blocks not run", unrun: true},
		{name: "a scout run", outside: "巡检", receipt: "halt 只在用户让你停的那一轮有效"},
		{name: "a meeting brief", outside: "会前简报", receipt: "cancel 只在用户让你停的那一轮有效"},
		{name: "a lookup's answer", outside: "网页查询结果", receipt: "这一轮由网页查询结果触发"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHaltRig(t)
			h.r.cfg.UnrunTurn = func() bool { return c.unrun }
			h.run("好的。\n"+block(`{"type":"halt","task":"`+h.key+`","why":"用户说先停"}`)+"\n"+
				block(`{"type":"cancel","task":"`+h.key+`","why":"用户说不用做了"}`), c.outside, c.words...)
			m, _ := h.side.Get(h.key)
			ran := len(h.fc.halts) == 1 && len(h.boardSt) == 1 && m.Mark != nil
			if ran != c.runs || !c.runs && (len(h.fc.halts)+len(h.stopped)+len(h.boardSt) != 0 || m.Mark != nil) {
				t.Fatalf("halts %q stops %q board %q mark %+v, want run=%v", h.fc.halts, h.stopped, h.boardSt, m.Mark, c.runs)
			}
			receipts := append(slices.Clone(h.g.unrun), h.g.refused...)
			switch {
			case c.receipt == "" && len(receipts) != 0:
				t.Errorf("receipts = %q, want none", receipts)
			case c.receipt != "" && (len(receipts) != 1 || !strings.Contains(receipts[0], c.receipt) || strings.Count(receipts[0], "\n- ") != 2):
				t.Errorf("receipts = %q, want one naming both blocks with %q", receipts, c.receipt)
			}
			if c.outside != "" && len(h.g.refused) != 1 {
				t.Errorf("an outside turn's receipt not held to its limits: refused %q", h.g.refused)
			}
			if !c.runs && !c.unrun && len(h.g.notes) != 2 {
				t.Errorf("notes = %q, want one per block", h.g.notes)
			}
		})
	}
}

// A halt stops the session's custody, with the user's reason, and closes
// its process; its task's state is left as it is. A session the user
// opened, an IM chat among them, may be halted too.
func TestHaltStopsCustodyAndProcess(t *testing.T) {
	const im = "feishu:direct:alice:general"
	h := newHaltRig(t)
	h.run(block(`{"type":"halt","task":"`+h.key+`","why":"用户说先停下"}`)+"\n"+block(`{"type":"halt","task":"`+im+`","why":"用户说这个也停"}`), "", said)
	if want := []string{h.key + "|用户说先停下", im + "|用户说这个也停"}; !slices.Equal(h.fc.halts, want) {
		t.Errorf("halts = %q, want %q", h.fc.halts, want)
	}
	if want := []string{h.key, im}; !slices.Equal(h.stopped, want) {
		t.Errorf("processes stopped = %q, want %q", h.stopped, want)
	}
	if m, _ := h.side.Get(h.key); m.Mark != nil || len(h.boardSt) != 0 {
		t.Errorf("halt changed the task: mark %+v board %q", m.Mark, h.boardSt)
	}
	if len(h.g.notes)+len(h.g.unrun)+len(h.g.told) != 0 {
		t.Errorf("notes %q unrun %q told %q, want none: the card says it", h.g.notes, h.g.unrun, h.g.told)
	}
}

// What a halt does not stop: the Planner, a cron run, a scratch session or
// a daemon (refused as a bad block), a remote node's session (custody finds
// none), a task put away (nothing runs); a session waiting on the user's
// answer keeps its process, its custody stopped.
func TestHaltTargets(t *testing.T) {
	cases := []struct {
		name    string
		key     string // "" is the dispatched task
		setup   func(h *haltRig)
		halted  bool // custody stopped
		closed  bool // process closed
		receipt string
		bad     bool // refused as a bad block
	}{
		{name: "the Planner", key: sessionkey.PlannerMainKey, receipt: "不能叫停", bad: true},
		{name: "a cron run", key: "cron:j1", receipt: "不能叫停", bad: true},
		{name: "a scratch session", key: "scratch:s1", receipt: "不能叫停", bad: true},
		{name: "a daemon", key: "sys:attachment-gc", receipt: "不能叫停", bad: true},
		{name: "a scout run", key: "scout:r1", receipt: "不能叫停", bad: true},
		{name: "a remote session", key: "dashboard:direct:u:general", setup: func(h *haltRig) { h.fc.haltErr = custody.ErrNoSession },
			receipt: "远程节点的会话由那个节点的 Planner 叫停", bad: true},
		{name: "put away", setup: func(h *haltRig) {
			if _, err := h.side.Update(h.key, func(m *sidecar.Meta) bool { m.Reap = &sidecar.Reap{At: time.Now()}; return true }); err != nil {
				panic(err)
			}
		}, receipt: "已经收起，没有在做事，不用叫停"},
		{name: "asking the user", setup: func(h *haltRig) { h.asking[h.key] = true }, halted: true,
			receipt: "它已经在等用户回答，网关只停了托管，没有关进程"},
		{name: "need_user open", setup: func(h *haltRig) {
			h.inbox.Add(notify.Item{Kind: notify.KindQuestion, Unit: h.key, Title: "要你决定", Source: ReportSource(h.key)})
		}, halted: true, receipt: "它已经在等用户回答"},
		// The user answered its need_user question in the session: the item
		// stays open until the turn that answer started ends, the session is
		// at work on it, and a halt closes it.
		{name: "need_user answered", setup: func(h *haltRig) {
			h.inbox.Add(notify.Item{Kind: notify.KindQuestion, Unit: h.key, Title: "要你决定", Source: ReportSource(h.key)})
			answeredAt := time.Now().Add(time.Second)
			h.r.cfg.UserSent = func(key string, since time.Time) bool { return key == h.key && answeredAt.After(since) }
		}, halted: true, closed: true},
		// Asked again after that answer: the open item is the new question.
		{name: "need_user asked again", setup: func(h *haltRig) {
			h.inbox.Add(notify.Item{Kind: notify.KindQuestion, Unit: h.key, Title: "要你决定", Source: ReportSource(h.key)})
			answeredAt := time.Now().Add(time.Second)
			h.r.cfg.UserSent = func(key string, since time.Time) bool { return key == h.key && answeredAt.After(since) }
			if _, err := h.side.Update(h.key, func(m *sidecar.Meta) bool {
				m.Report = &sidecar.Report{State: ReportNeedUser, Summary: "还要你定", At: answeredAt.Add(time.Second)}
				return true
			}); err != nil {
				panic(err)
			}
		}, halted: true, receipt: "它已经在等用户回答"},
		{name: "running", halted: true, closed: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHaltRig(t)
			key := c.key
			if key == "" {
				key = h.key
			}
			if c.setup != nil {
				c.setup(h)
			}
			h.run(block(`{"type":"halt","task":"`+key+`","why":"用户说停"}`), "", said)
			if got := len(h.fc.halts) == 1; got != c.halted {
				t.Errorf("halts = %q, want halted=%v", h.fc.halts, c.halted)
			}
			if got := len(h.stopped) == 1; got != c.closed {
				t.Errorf("processes stopped = %q, want closed=%v", h.stopped, c.closed)
			}
			switch {
			case c.receipt == "" && len(h.g.unrun) != 0:
				t.Errorf("unrun = %q, want none", h.g.unrun)
			case c.receipt != "" && (len(h.g.unrun) != 1 || !strings.Contains(h.g.unrun[0], c.receipt)):
				t.Errorf("unrun = %q, want one with %q", h.g.unrun, c.receipt)
			}
			if bad := len(h.g.notes) == 1 && strings.Contains(h.g.notes[0], "格式不对"); bad != c.bad {
				t.Errorf("notes = %q, want bad=%v", h.g.notes, c.bad)
			}
		})
	}
}

// A cancel is the user's stop in 任务 (Board.Stop): the task marked done
// by the user, 已停止, the promises citing it closed, its process stopped
// through task.Config.Stop, and custody may not reopen it after. A task
// done already with nothing running needs none; one that does not exist
// is a bad block.
func TestCancel(t *testing.T) {
	t.Run("stops as the user's", func(t *testing.T) {
		h := newHaltRig(t)
		p, err := h.r.cfg.Promises.Add(promise.Promise{Text: "基准跑完告诉用户", Task: h.key})
		if err != nil {
			t.Fatal(err)
		}
		h.run(block(`{"type":"cancel","task":"`+h.key+`","why":"用户说不用做了"}`), "", said)
		m, _ := h.side.Get(h.key)
		if m.Mark == nil || task.State(m.Mark.State) != task.Done || task.By(m.Mark.By) != task.ByUser || m.Mark.Why != task.StoppedWhy {
			t.Fatalf("mark = %+v, want done by the user, %s", m.Mark, task.StoppedWhy)
		}
		if want := []string{h.key + "|" + p.ID}; !slices.Equal(h.boardSt, want) {
			t.Errorf("board stops = %q, want %q", h.boardSt, want)
		}
		if open, _ := h.r.cfg.Promises.List(0); len(open) != 0 {
			t.Errorf("open promises = %+v, want the citing one closed", open)
		}
		if got := h.r.closedByUser(h.key); got != "停掉" {
			t.Errorf("closedByUser = %q, want 停掉", got)
		}
		if len(h.g.notes)+len(h.g.unrun) != 0 || len(h.fc.halts) != 0 {
			t.Errorf("notes %q unrun %q halts %q, want none", h.g.notes, h.g.unrun, h.fc.halts)
		}
	})
	t.Run("done and idle", func(t *testing.T) {
		h := newHaltRig(t)
		if err := h.r.cfg.Tasks.Set(h.key, task.Done, task.ByPlanner, "汇报完成"); err != nil {
			t.Fatal(err)
		}
		h.run(block(`{"type":"cancel","task":"`+h.key+`","why":"用户说不用做了"}`), "", said)
		if len(h.boardSt) != 0 || len(h.g.unrun) != 1 || !strings.Contains(h.g.unrun[0], "已经是已完成，没有在跑，不用停止") {
			t.Errorf("board %q unrun %q", h.boardSt, h.g.unrun)
		}
	})
	t.Run("done and still running", func(t *testing.T) {
		h := newHaltRig(t)
		h.fc.settled = false
		if err := h.r.cfg.Tasks.Set(h.key, task.Done, task.ByPlanner, "汇报完成"); err != nil {
			t.Fatal(err)
		}
		h.run(block(`{"type":"cancel","task":"`+h.key+`","why":"用户说停"}`), "", said)
		if len(h.boardSt) != 1 || len(h.g.unrun) != 0 {
			t.Errorf("board %q unrun %q, want stopped", h.boardSt, h.g.unrun)
		}
	})
	t.Run("no such task", func(t *testing.T) {
		h := newHaltRig(t)
		h.run(block(`{"type":"cancel","task":"task:nope","why":"用户说停"}`), "", said)
		if len(h.boardSt) != 0 || len(h.g.notes) != 1 || !strings.Contains(h.g.notes[0], "找不到任务") {
			t.Errorf("board %q notes %q", h.boardSt, h.g.notes)
		}
	})
}

// A session whose custody stopped (halted, or at a limit) gets no receipt
// of a turn that ended unreported, the turn the halt cut included; a
// report it wrote still reaches the Planner.
func TestNoUnreportedReceiptWhenStopped(t *testing.T) {
	h := newHaltRig(t)
	h.r.grace, h.r.reportGrace = 0, 0
	h.r.cfg.Reports = func(time.Time, time.Time) []string { return nil }
	h.r.Result(h.key, "跑了一半")
	h.r.checkEnded(<-h.r.endCh)
	if len(h.g.told) != 1 {
		t.Fatalf("before the halt: told = %q, want the receipt", h.g.told)
	}
	h.g.told = nil
	h.r.cfg.Words = func() []Words { return []Words{said} }
	h.run(block(`{"type":"status","task":"`+h.key+`","state":"doing","why":"还在跑"}`)+"\n"+
		block(`{"type":"halt","task":"`+h.key+`","why":"用户说先停下"}`), "", said)
	h.g.told = nil
	h.r.doingAt = map[string]time.Time{} // the doing block's quiet window is not what is tested
	h.r.Result(h.key, "被打断了")
	h.r.checkEnded(<-h.r.endCh)
	if len(h.g.told) != 0 {
		t.Errorf("halted: told = %q, want no receipt", h.g.told)
	}
	h.r.Gone(h.key, "stopped")
	h.r.checkEnded(<-h.r.endCh)
	if len(h.g.told) != 0 {
		t.Errorf("halted, process gone: told = %q, want no receipt", h.g.told)
	}
	h.r.Result(h.key, "停在这里。\n```naozhi-report\n{\"state\":\"blocked\",\"summary\":\"被叫停，基准跑了一半\"}\n```")
	h.r.checkEnded(<-h.r.endCh)
	if len(h.g.told) != 1 || !strings.Contains(h.g.told[0], "被叫停，基准跑了一半") {
		t.Errorf("a report after the halt: told = %q, want its line", h.g.told)
	}
}

// A turn end held on background work still running, then the session
// halted and its process stopped: the CLI's end (Gone) wakes the hold, and
// no "之后进程被停止，不是正常结束" receipt goes to the Planner, the stop
// being the user's. Without the halt the same wake is told, so the check
// is reached.
func TestNoGoneReceiptWhenHalted(t *testing.T) {
	for _, halted := range []bool{false, true} {
		t.Run(map[bool]string{false: "not halted", true: "halted"}[halted], func(t *testing.T) {
			h := newHaltRig(t)
			h.r.grace, h.r.reportGrace = 0, 0
			now := time.Now()
			h.r.cfg.Now = func() time.Time { return now }
			var wakes int
			h.r.after = func(d time.Duration, f func()) {
				if d > 0 {
					wakes++
					return
				}
				f()
			}
			open := true
			h.r.cfg.Background = func(string) bgtask.State {
				if !open {
					return bgtask.State{}
				}
				return bgtask.State{Tasks: []bgtask.Task{{ID: "b1", Kind: bgtask.KindBash, Last: now}}}.At(now)
			}
			h.r.Result(h.key, "基准在后台跑，跑完再看")
			h.r.checkEnded(<-h.r.endCh)
			if len(h.g.told) != 0 || wakes != 1 {
				t.Fatalf("told %q wakes %d; want the turn end held", h.g.told, wakes)
			}
			if halted {
				h.run(block(`{"type":"halt","task":"`+h.key+`","why":"用户说先停下"}`), "", said)
				if len(h.stopped) != 1 || h.stopped[0] != h.key {
					t.Fatalf("stopped = %q, want the process closed", h.stopped)
				}
				h.g.told = nil
			}
			open = false
			now = now.Add(time.Minute)
			h.r.Gone(h.key, "stopped")
			h.r.checkEnded(<-h.r.endCh)
			switch {
			case halted && len(h.g.told) != 0:
				t.Errorf("halted, process gone: told = %q, want no receipt", h.g.told)
			case !halted && (len(h.g.told) != 1 || !strings.Contains(h.g.told[0], "进程被停止，后台任务随之停止，不是正常结束")):
				t.Errorf("not halted, process gone: told = %q, want the receipt", h.g.told)
			}
		})
	}
}

// The fake's halt error passes through as the receipt says it.
func TestHaltCustodyError(t *testing.T) {
	h := newHaltRig(t)
	h.fc.haltErr = errors.New("盘满了")
	h.run(block(`{"type":"halt","task":"`+h.key+`","why":"用户说停"}`), "", said)
	if len(h.stopped) != 0 || len(h.g.unrun) != 1 || !strings.Contains(h.g.unrun[0], "没有叫停（盘满了）") {
		t.Errorf("stopped %q unrun %q", h.stopped, h.g.unrun)
	}
}
