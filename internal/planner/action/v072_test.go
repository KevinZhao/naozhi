package action

import (
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/bgtask"
	"github.com/naozhi/naozhi/internal/planner/task"
)

// A task marked done whose session then reports that it waits on the user
// (or is still at it, or is stuck) goes back to 进行中, journaled with why,
// and the line to the Planner says it was marked done; a done report, or
// a done mark of the user's own, leaves the mark.
func TestDoneTaskReportReopens(t *testing.T) {
	for _, c := range []struct {
		state  string
		by     task.By
		reopen bool
		label  string
	}{
		{"need_user", task.ByPlanner, true, "（任务已标完成，网关已改回进行中）"},
		{"progress", task.ByPlanner, true, "（任务已标完成，网关已改回进行中）"},
		{"blocked", task.ByPlanner, true, "（任务已标完成，网关已改回进行中）"},
		{"done", task.ByPlanner, false, "（任务已标完成）"},
		{"need_user", task.ByUser, false, "（任务已标完成）"},
	} {
		t.Run(c.state+"/"+string(c.by), func(t *testing.T) {
			e := endedRunner(t)
			if err := e.r.cfg.Tasks.Set(e.key, task.Done, c.by, "看着做完了"); err != nil {
				t.Fatal(err)
			}
			e.wait(time.Minute)
			e.reportEnd(reportReply(c.state, "选 A 还是 B"))
			m, _ := e.side.Get(e.key)
			want := task.Done
			if c.reopen {
				want = task.Doing
			}
			if m.Mark == nil || task.State(m.Mark.State) != want {
				t.Fatalf("mark = %+v, want %s", m.Mark, want)
			}
			if c.reopen && !strings.Contains(m.Mark.Why, "标完成后又汇报") {
				t.Errorf("why = %q", m.Mark.Why)
			}
			if len(e.g.told) != 1 || !strings.Contains(e.g.told[0], c.label) {
				t.Errorf("told = %q, want %q", e.g.told, c.label)
			}
		})
	}
	// Not done: no label, nothing set.
	e := endedRunner(t)
	e.reportEnd(reportReply("need_user", "选 A 还是 B"))
	if len(e.g.told) != 1 || strings.Contains(e.g.told[0], "任务已标完成") {
		t.Errorf("told = %q", e.g.told)
	}
}

// A relay to a done session not put away yet reaches it and leaves its task
// done (§15.14 ④); what it reports next decides, even against the user's
// own done mark, since the user wrote to it after making it.
func TestRelayToDoneTaskKeepsState(t *testing.T) {
	for _, by := range []task.By{task.ByPlanner, task.ByUser} {
		t.Run(string(by), func(t *testing.T) {
			e := endedRunner(t)
			if err := e.r.cfg.Tasks.Set(e.key, task.Done, by, "做完了"); err != nil {
				t.Fatal(err)
			}
			e.wait(time.Minute)
			out := userTurn(e.r, said)
			e.r.cfg.RelayTarget = func(string) error { return nil }
			e.r.Result(e.r.cfg.Planner, block(`{"type":"relay","key":"`+e.key+`","text":"用户要它把文档也补上"}`))
			e.r.handle(<-e.r.ch)
			if len(*out) != 1 {
				t.Fatalf("relayed = %q", *out)
			}
			if m, _ := e.side.Get(e.key); m.Mark == nil || task.State(m.Mark.State) != task.Done {
				t.Fatalf("mark = %+v, want still done", m.Mark)
			}
			e.wait(time.Minute)
			e.reportEnd(reportReply("progress", "在补文档"))
			if m, _ := e.side.Get(e.key); m.Mark == nil || task.State(m.Mark.State) != task.Doing {
				t.Fatalf("mark = %+v, want doing after its progress report", m.Mark)
			}
		})
	}
}

// A check held on background work is woken by the CLI's end, not left to
// the work's quiet limit, and the receipt says the gateway reclaimed it:
// in custody and for a dispatched task.
func TestGoneWakesHeldCheck(t *testing.T) {
	t.Run("custody", func(t *testing.T) {
		r, g, fc := custodyRig(t)
		now := time.Now()
		r.cfg.Now = func() time.Time { return now }
		var wakes int
		r.after = func(d time.Duration, f func()) {
			if d > 0 {
				wakes++
				return
			}
			f()
		}
		open := true
		r.cfg.Background = func(string) bgtask.State {
			if !open {
				return bgtask.State{}
			}
			return bgtask.State{Tasks: []bgtask.Task{{ID: "b1", Kind: bgtask.KindBash, Last: now}}}.At(now)
		}
		r.Result(dashKey, "trn2 在后台跑，跑完再看")
		r.checkEnded(<-r.endCh)
		if len(g.told) != 0 || wakes != 1 {
			t.Fatalf("told %q wakes %d; want held", g.told, wakes)
		}
		open, fc.settled = false, true
		now = now.Add(40 * time.Minute)
		r.Gone(dashKey, "idle_timeout")
		r.checkEnded(<-r.endCh)
		if len(g.told) != 1 || !strings.Contains(g.told[0], "进程被网关空闲回收，后台任务随之停止") || strings.Contains(g.told[0], "正常结束了") ||
			!strings.Contains(g.told[0], "trn2 在后台跑") {
			t.Fatalf("told = %q", g.told)
		}
		// Nothing held: nothing told.
		r.Gone(dashKey, "idle_timeout")
		r.checkEnded(<-r.endCh)
		if len(g.told) != 1 {
			t.Errorf("told again: %q", g.told)
		}
	})
	t.Run("task", func(t *testing.T) {
		e := endedRunner(t)
		open := true
		e.r.cfg.Background = func(string) bgtask.State {
			if !open {
				return bgtask.State{}
			}
			return bgtask.State{Tasks: []bgtask.Task{{ID: "b1", Kind: bgtask.KindBash, Last: e.now}}}.At(e.now)
		}
		e.end("基准在后台跑")
		if len(e.g.told) != 0 || len(e.held) == 0 {
			t.Fatalf("told %q held %d; want held", e.g.told, len(e.held))
		}
		open = false
		e.wait(40 * time.Minute)
		e.r.Gone(e.key, "idle_timeout")
		e.check()
		if len(e.g.told) != 1 || !strings.Contains(e.g.told[0], "进程被网关空闲回收，后台任务随之停止，不是正常结束") {
			t.Fatalf("told = %q", e.g.told)
		}
	})
}

// A turn end held only to a doing window had no background work to kill:
// the CLI reaped in the window wakes nothing, and the receipt at the
// window's end is the plain one, not "后台任务随之停止".
func TestGoneLeavesDoingWindowHold(t *testing.T) {
	e := endedRunner(t)
	e.end("改好了一半")
	if len(e.receipts()) != 1 {
		t.Fatalf("told = %q", e.g.told)
	}
	e.wait(5 * time.Minute)
	e.doing("还没做完")
	e.wait(5 * time.Minute)
	e.end("全部做完了")
	if n := len(e.receipts()); n != 1 {
		t.Fatalf("a receipt within the doing window: %q", e.g.told)
	}
	e.wait(31 * time.Minute)
	e.r.Gone(e.key, "idle_timeout")
	e.check()
	if n := len(e.receipts()); n != 1 {
		t.Fatalf("the CLI's end woke the window's hold: %q", e.g.told)
	}
	e.wait(doingQuiet)
	e.fire()
	last := e.g.told[len(e.g.told)-1]
	if !strings.Contains(last, "这一轮已经结束") || !strings.Contains(last, "全部做完了") || strings.Contains(last, "后台任务") {
		t.Fatalf("told = %q, want the plain receipt for the window's turn", e.g.told)
	}
}

// A session in custody told of a turn end is not told again of the turns
// that woke by themselves after it (a Monitor pushing past bgtask's quiet
// limit: one receipt per push); a turn after anything sent it is.
func TestCustodySelfWokenTurnsToldOnce(t *testing.T) {
	r, g, _ := custodyRig(t)
	now := time.Now()
	r.cfg.Now = func() time.Time { return now }
	var sent time.Time
	r.cfg.Prompted = func(key string, since time.Time) bool { return sent.After(since) }
	turn := func(text string) {
		now = now.Add(time.Minute)
		r.Result(dashKey, text)
		r.checkEnded(<-r.endCh)
	}
	turn("Monitor 推送 1")
	turn("Monitor 推送 2")
	turn("Monitor 推送 3")
	if len(g.told) != 1 || !strings.Contains(g.told[0], "推送 1") {
		t.Fatalf("told = %q, want the first only", g.told)
	}
	now = now.Add(time.Minute)
	sent = now
	turn("按用户说的改好了")
	if len(g.told) != 2 || !strings.Contains(g.told[1], "按用户说的改好了") {
		t.Fatalf("told = %q, want the prompted turn told", g.told)
	}
}

// custodyRig is a runner checking dashKey in custody at once, settled.
func custodyRig(t *testing.T) (*Runner, *fakeGateway, *fakeCustody) {
	t.Helper()
	r, g, _, _, _ := newRunner(t)
	r.grace = 0
	fc := &fakeCustody{on: map[string]bool{dashKey: true}, settled: true}
	r.cfg.Custody = fc
	if err := r.cfg.Sessions.Put(sidecarMeta(dashKey)); err != nil {
		t.Fatal(err)
	}
	return r, g, fc
}
