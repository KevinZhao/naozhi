package action

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/task"
)

// rigClock is the inbox's clock read off the rig's, so an ask's Created and
// the marks compare on one time line.
type rigClock struct{ now *time.Time }

func (c rigClock) Now() time.Time      { return *c.now }
func (c rigClock) Mono() time.Duration { return 0 }

// askRig is endedRunner on 11-14's clock, its inbox and task board reading
// it too, the user's own messages to the session at typed (UserTyped), and
// what is relayed recorded.
type askRig struct {
	*endRig
	typed   time.Time
	relayed *[]relayed
}

func at1114(h, m int) time.Time { return time.Date(2026, 11, 14, h, m, 0, 0, time.Local) }

func newAskRig(t *testing.T) *askRig {
	t.Helper()
	e := endedRunner(t)
	a := &askRig{endRig: e}
	e.now = at1114(15, 0)
	e.inbox = notify.New(notify.Config{Clock: rigClock{&e.now}, System: func(string, string) error { return nil }})
	e.inbox.HandleDecisions(HandlerAsk, e.r.decideAsk)
	e.r.cfg.Inbox = e.inbox
	e.r.cfg.Tasks = task.New(task.Config{Sessions: e.side, OnState: e.r.TaskState, Now: func() time.Time { return e.now }})
	e.r.cfg.UserTyped = func(key string, since time.Time) bool { return key == e.key && a.typed.After(since) }
	a.relayed = userTurn(e.r)
	e.r.cfg.RelayTarget = func(string) error { return nil }
	e.g.told = nil
	return a
}

// ask has the Planner ask q about key at the rig's now; it returns the card.
func (a *askRig) ask(key, q string, options string) notify.Item {
	a.t.Helper()
	a.r.Result(a.r.cfg.Planner, block(`{"type":"ask","question":"`+q+`","options":[`+options+`],"task":"`+key+`"}`))
	a.r.handle(<-a.r.ch)
	open, _ := a.inbox.List(0)
	for _, it := range open {
		if it.Title == q {
			return it
		}
	}
	a.t.Fatalf("no ask %q in %+v (notes %q)", q, open, a.g.notes)
	return notify.Item{}
}

func (a *askRig) item(id string) notify.Item {
	a.t.Helper()
	it, ok := a.inbox.Item(id)
	if !ok {
		a.t.Fatalf("item %s gone", id)
	}
	return it
}

func (a *askRig) wantExpired(id, why string) {
	a.t.Helper()
	it := a.item(id)
	if it.State != notify.StateClosed || it.ClosedBy != notify.ClosedByAuto || it.ClosedWhy != why {
		a.t.Fatalf("item = state %s by %s why %q, want closed by auto, %q", it.State, it.ClosedBy, it.ClosedWhy, why)
	}
}

// 11-14: the Planner asked at 15:25 what to do with the recordings; at 15:28
// the user told the task session themselves, and it was done at 15:35. The
// answer given in "等你" at 15:54 was relayed again and set the task back to
// 进行中. Now the ask closes once the user's turn ends, records why, and
// the late answer is refused: nothing relayed, the Planner not told, the
// task left done.
func TestAskExpires1114(t *testing.T) {
	a := newAskRig(t)
	a.now = at1114(15, 25)
	it := a.ask(a.key, "录音怎么处理？", `"清理掉并提交","先留着"`)

	a.now = at1114(15, 28)
	a.typed = a.now
	a.now = at1114(15, 33)
	a.end("清理掉了，已提交")
	a.wantExpired(it.ID, expiredUserSent)

	a.now = at1114(15, 35)
	if err := a.r.cfg.Tasks.Set(a.key, task.Done, task.ByPlanner, "它汇报做完了"); err != nil {
		t.Fatal(err)
	}

	a.now = at1114(15, 54)
	a.g.told = nil
	err := a.inbox.Decide(it.ID, "清理掉并提交")
	var exp notify.Expired
	if !errors.As(err, &exp) || exp.Why != expiredUserSent || !errors.Is(err, notify.ErrClosed) {
		t.Fatalf("Decide = %v, want expired: %s", err, expiredUserSent)
	}
	if len(*a.relayed) != 0 || len(a.g.told) != 0 {
		t.Errorf("relayed %q told %q, want nothing sent", *a.relayed, a.g.told)
	}
	if st := taskState(t, a.r, a.key); st != task.Done {
		t.Errorf("task state = %s, want done", st)
	}
}

// An answer that comes before anything closed the ask (the user's turn
// still running) is refused all the same, and closes it.
func TestAskExpiresOnAnswer(t *testing.T) {
	a := newAskRig(t)
	a.now = at1114(15, 25)
	it := a.ask(a.key, "录音怎么处理？", `"清理掉并提交"`)
	a.now = at1114(15, 28)
	a.typed = a.now
	a.now = at1114(15, 29)
	if err := a.inbox.Decide(it.ID, "清理掉并提交"); !errors.Is(err, notify.ErrExpired) {
		t.Fatalf("Decide = %v, want expired", err)
	}
	a.wantExpired(it.ID, expiredUserSent)
	if len(*a.relayed) != 0 || len(a.g.told) != 0 {
		t.Errorf("relayed %q told %q, want nothing sent", *a.relayed, a.g.told)
	}

	// Marked done meanwhile, with no word in the session: 任务已完成.
	a.now = at1114(16, 0)
	it = a.ask(a.key, "要不要补文档？", `"补"`)
	if _, err := a.side.Update(a.key, func(m *sidecar.Meta) bool {
		m.Mark = &sidecar.Mark{State: string(task.Done), By: string(task.ByPlanner), At: at1114(16, 5)}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	a.now = at1114(16, 10)
	if err := a.inbox.Decide(it.ID, "补"); !errors.Is(err, notify.ErrExpired) {
		t.Fatalf("Decide = %v, want expired", err)
	}
	a.wantExpired(it.ID, expiredDone)
}

// The task marked done, or put away, closes every ask naming it; a failed
// one leaves them.
func TestAskExpiresOnDoneOrPutAway(t *testing.T) {
	a := newAskRig(t)
	a.now = at1114(15, 25)
	withOpts := a.ask(a.key, "拆不拆？", `"拆","先不拆"`)
	a.now = at1114(15, 26)
	other := a.ask(a.key, "测试栈留多久？", `"一天","一周"`)
	if other.Task != a.key {
		t.Fatalf("ask = %+v, want its task kept", other)
	}
	a.now = at1114(15, 30)
	if err := a.r.cfg.Tasks.Set(a.key, task.Failed, task.ByPlanner, "卡住了"); err != nil {
		t.Fatal(err)
	}
	if a.item(withOpts.ID).State != notify.StateOpen {
		t.Fatal("a failed task closed its ask")
	}
	a.now = at1114(15, 35)
	if err := a.r.cfg.Tasks.Set(a.key, task.Done, task.ByPlanner, "做完了"); err != nil {
		t.Fatal(err)
	}
	a.wantExpired(withOpts.ID, expiredDone)
	a.wantExpired(other.ID, expiredDone)

	// An ask about a task done already is asked on purpose: open until it
	// is put away.
	a.now = at1114(16, 0)
	later := a.ask(a.key, "要不要接着做？", `"做"`)
	a.r.TaskState(a.key, task.Done)
	if a.item(later.ID).State != notify.StateOpen {
		t.Fatal("an ask about a done task closed by the mark from before it")
	}
	a.now = at1114(18, 0)
	if _, err := a.side.Update(a.key, func(m *sidecar.Meta) bool { m.Reap = &sidecar.Reap{At: a.now, SessionID: "sid-1"}; return true }); err != nil {
		t.Fatal(err)
	}
	a.r.PutAway(a.key)
	a.wantExpired(later.ID, expiredDone)
}

// What came before the ask does not close it, nor a word to another
// session; an ask about a done task is answered, relayed, and leaves the
// task done.
func TestAskNotExpired(t *testing.T) {
	a := newAskRig(t)
	a.typed = at1114(15, 0)
	if err := a.r.cfg.Tasks.Set(a.key, task.Done, task.ByPlanner, "做完了"); err != nil {
		t.Fatal(err)
	}
	a.now = at1114(15, 25)
	it := a.ask(a.key, "要不要接着做？", `"做","不做"`)
	a.now = at1114(15, 30)
	a.r.Result(dashKey, "别的会话的回合")
	a.end("这一轮没人发话")
	if a.item(it.ID).State != notify.StateOpen {
		t.Fatal("ask closed with nothing settling it")
	}
	if err := a.inbox.Decide(it.ID, "做"); err != nil {
		t.Fatal(err)
	}
	if len(*a.relayed) != 1 || (*a.relayed)[0].key != a.key {
		t.Fatalf("relayed = %q", *a.relayed)
	}
	if got := a.item(it.ID); got.ClosedBy != notify.ClosedByUser || got.ClosedWhy != "" {
		t.Errorf("item = %+v, want answered by the user", got)
	}
	if st := taskState(t, a.r, a.key); st != task.Done {
		t.Errorf("task state = %s, want done until it reports", st)
	}
}

// An ask may name a session the user opened: their message to it closes the
// ask once its turn ends.
func TestAskExpiresOnUserSession(t *testing.T) {
	a := newAskRig(t)
	var typed time.Time
	a.r.cfg.UserTyped = func(key string, since time.Time) bool { return key == dashKey && typed.After(since) }
	a.now = at1114(15, 25)
	it := a.ask(dashKey, "这个分支要合吗？", `"合"`)
	a.now = at1114(15, 28)
	typed = a.now
	a.r.Result(dashKey, "合好了")
	a.wantExpired(it.ID, expiredUserSent)
}

// The asks settled while naozhi was down close as the runner starts.
func TestAskExpiresAtStart(t *testing.T) {
	a := newAskRig(t)
	a.now = at1114(15, 25)
	it := a.ask(a.key, "录音怎么处理？", `"清理掉并提交"`)
	a.typed = at1114(15, 28)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.r.Run(ctx)
	a.wantExpired(it.ID, expiredUserSent)
}
