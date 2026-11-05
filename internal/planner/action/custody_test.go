package action

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/planner/custody"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/task"
)

func sidecarMeta(key string) sidecar.Meta { return sidecar.Meta{Key: key} }

type fakeCustody struct {
	on         map[string]bool
	check      error
	settled    bool
	setErr     error
	deliverErr error
	sets       []string
	delivered  []string
}

func (f *fakeCustody) On(key string) bool { return f.on[key] }
func (f *fakeCustody) Check(key string) error {
	if !f.on[key] {
		return custody.ErrOff
	}
	return f.check
}
func (f *fakeCustody) Settled(string) bool { return f.settled }
func (f *fakeCustody) Set(key string, on bool, by string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.sets = append(f.sets, key+"="+map[bool]string{true: "on", false: "off"}[on]+"/"+by)
	f.on[key] = on
	return nil
}
func (f *fakeCustody) Deliver(key string, reason custody.Reason, text string) (int, error) {
	if f.deliverErr != nil {
		return 0, f.deliverErr
	}
	f.delivered = append(f.delivered, key+"|"+string(reason)+"|"+text)
	return len(f.delivered), nil
}
func (f *fakeCustody) Name(key string) string { return "名字 " + key }

const dashKey = "dashboard:direct:u:general"

// A custody block switches a session's custody as the Planner's, or sends
// the Planner's go-ahead through custody; what custody refuses is said in
// the conversation and the receipt, a key that names no session as a bad
// block, and an outside turn runs none.
func TestCustodyBlock(t *testing.T) {
	on := block(`{"type":"custody","key":"` + dashKey + `","on":true}`)
	say := block(`{"type":"custody","key":"` + dashKey + `","text":"把剩下的两个测试也跑完"}`)
	cases := []struct {
		name       string
		reply      string
		setErr     error
		deliverErr error
		check      error
		outside    string
		nilCustody bool
		sets       []string
		delivered  []string
		note       string // a substring of the only note; "" for none
	}{
		{name: "on", reply: on, sets: []string{dashKey + "=on/planner"}},
		{name: "off", reply: block(`{"type":"custody","key":"` + dashKey + `","on":false}`), sets: []string{dashKey + "=off/planner"}},
		{name: "go on", reply: say, delivered: []string{dashKey + "|Planner 继续|把剩下的两个测试也跑完"}},
		{name: "on a stopped lineage", reply: on, check: errors.Join(custody.ErrStopped), sets: []string{dashKey + "=on/planner"}, note: "要用户在会话里重新打开"},
		{name: "no such session", reply: on, setErr: custody.ErrNoSession, note: "动作块格式不对，没有执行：找不到会话"},
		{name: "refused", reply: on, setErr: custody.ErrRefused, note: "不能托管"},
		{name: "user nearby", reply: say, deliverErr: custody.ErrNearby, note: "用户正在看这个会话"},
		{name: "off session", reply: say, deliverErr: custody.ErrOff, note: "不在托管中"},
		{name: "paused", reply: say, deliverErr: custody.ErrPaused, note: "暂停了托管"},
		{name: "stopped", reply: say, deliverErr: custody.ErrStopped, note: "已经交给用户"},
		{name: "not wired", reply: say, nilCustody: true, note: "托管没有接通"},
		{name: "scout turn", reply: say, outside: "巡检", note: "巡检触发的回合不能改托管"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, _, _, _ := newRunner(t)
			fc := &fakeCustody{on: map[string]bool{}, setErr: c.setErr, deliverErr: c.deliverErr, check: c.check}
			if !c.nilCustody {
				r.cfg.Custody = fc
			}
			r.cfg.Outside = func() string { return c.outside }
			r.Result(r.cfg.Planner, "好的。\n"+c.reply)
			r.handle(<-r.ch)
			if strings.Join(fc.sets, ",") != strings.Join(c.sets, ",") || strings.Join(fc.delivered, ",") != strings.Join(c.delivered, ",") {
				t.Errorf("sets %q delivered %q, want %q %q", fc.sets, fc.delivered, c.sets, c.delivered)
			}
			switch {
			case c.note == "" && len(g.notes) != 0:
				t.Errorf("notes = %q, want none", g.notes)
			case c.note != "" && (len(g.notes) != 1 || !strings.Contains(g.notes[0], c.note)):
				t.Errorf("notes = %q, want one with %q", g.notes, c.note)
			}
			if receipts := len(g.unrun) + len(g.refused); (c.note != "") != (receipts == 1) {
				t.Errorf("receipts unrun %q refused %q", g.unrun, g.refused)
			}
		})
	}
}

// A session in custody whose turn ended with nothing pending gets the
// Planner a receipt to judge it (E, A); it waits while the user is at the
// session or the Planner is busy, and none comes for a session out of
// custody, one still pending something, or a turn a later one replaced. A
// restart checks a turn end it cut short.
func TestCustodyEnded(t *testing.T) {
	next := func(t *testing.T, r *Runner) (ended, bool) {
		t.Helper()
		select {
		case e := <-r.endCh:
			return e, true
		case <-time.After(200 * time.Millisecond):
			return ended{}, false
		}
	}
	setup := func(t *testing.T) (*Runner, *fakeGateway, *fakeCustody) {
		r, g, _, _, _ := newRunner(t)
		r.grace = 0
		fc := &fakeCustody{on: map[string]bool{dashKey: true}, settled: true}
		r.cfg.Custody = fc
		if err := r.cfg.Sessions.Put(sidecarMeta(dashKey)); err != nil {
			t.Fatal(err)
		}
		return r, g, fc
	}
	t.Run("told", func(t *testing.T) {
		r, g, fc := setup(t)
		fc.check = custody.ErrNearby
		busy := false
		r.cfg.PlannerBusy = func() bool { return busy }
		r.Result(dashKey, "改好了一半，剩下的明天再说")
		e, ok := next(t, r)
		if !ok {
			t.Fatal("not queued")
		}
		r.checkEnded(e) // the user is at the session: waits
		if e, ok = next(t, r); !ok || len(g.told) != 0 {
			t.Fatalf("while nearby: requeued %v told %q", ok, g.told)
		}
		fc.check, busy = nil, true
		r.checkEnded(e) // the Planner is busy: waits
		if e, ok = next(t, r); !ok || len(g.told) != 0 {
			t.Fatalf("while busy: requeued %v told %q", ok, g.told)
		}
		busy = false
		r.checkEnded(e)
		if len(g.told) != 1 || !strings.Contains(g.told[0], "名字 "+dashKey+"，托管中") || !strings.Contains(g.told[0], "改好了一半") ||
			!strings.Contains(g.told[0], custodyFormat(dashKey)) {
			t.Fatalf("told = %q", g.told)
		}
		if m, _ := r.cfg.Sessions.Get(dashKey); m.Turn == nil || m.Turn.Check {
			t.Errorf("turn = %+v, want checked", m.Turn)
		}
	})
	for _, c := range []struct {
		name string
		prep func(r *Runner, fc *fakeCustody)
	}{
		{"out of custody", func(r *Runner, fc *fakeCustody) { fc.on[dashKey] = false }},
		{"pending", func(r *Runner, fc *fakeCustody) { fc.settled = false }},
		{"stopped", func(r *Runner, fc *fakeCustody) { fc.check = custody.ErrStopped }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, g, fc := setup(t)
			c.prep(r, fc)
			r.Result(dashKey, "x")
			if e, ok := next(t, r); ok {
				r.checkEnded(e)
			}
			if len(g.told) != 0 {
				t.Errorf("told = %q", g.told)
			}
		})
	}
	t.Run("a later turn ended", func(t *testing.T) {
		r, g, _ := setup(t)
		now := time.Now()
		r.cfg.Now = func() time.Time { return now }
		r.Result(dashKey, "第一轮")
		first, _ := next(t, r)
		now = now.Add(time.Minute)
		r.Result(dashKey, "第二轮")
		second, _ := next(t, r)
		r.checkEnded(first)
		r.checkEnded(second)
		if len(g.told) != 1 || !strings.Contains(g.told[0], "第二轮") {
			t.Errorf("told = %q, want the second turn only", g.told)
		}
	})
	t.Run("restart", func(t *testing.T) {
		r, g, fc := setup(t)
		r.Result(dashKey, "做到一半")
		cfg := r.cfg
		r2 := New(cfg)
		r2.grace = 0
		r2.recheck()
		e, ok := next(t, r2)
		if !ok {
			t.Fatal("the restart did not check the turn end")
		}
		r2.checkEnded(e)
		if len(g.told) != 1 || !strings.Contains(g.told[0], "做到一半") || fc.on[dashKey] != true {
			t.Errorf("told = %q", g.told)
		}
	})
}

// A dispatched session in custody that ended a turn unreported gets the
// custody block spelled out beside the status one; HandOff gives the
// Planner a choice the gateway would not make, unless custody is not
// allowed for the session.
func TestCustodyTaskReceiptAndHandOff(t *testing.T) {
	r, g, _, side, dir := newRunner(t)
	r.grace = 0
	r.cfg.Tasks = task.New(task.Config{Sessions: side})
	r.cfg.Reports = func(time.Time, time.Time) []string { return nil }
	fc := &fakeCustody{on: map[string]bool{}, settled: true}
	r.cfg.Custody = fc
	r.Handle(block(`{"type":"dispatch","workspace":"` + filepath.Base(dir) + `","title":"iso 跑测试","model":"","message":"跑一遍测试"}`))
	key := g.spawned[0]
	if m, _ := side.Get(key); m.Custody == nil || !m.Custody.On || m.Custody.By != custody.ByAuto {
		t.Errorf("dispatched custody = %+v, want on by auto", m.Custody)
	}
	fc.on[key] = true
	g.told = nil
	r.Result(key, "跑了一半")
	r.checkEnded(<-r.endCh)
	if len(g.told) != 1 || !strings.Contains(g.told[0], statusFormat(key)) || !strings.Contains(g.told[0], custodyFormat(key)) {
		t.Fatalf("told = %q", g.told)
	}

	g.told = nil
	if err := r.HandOff(key, "在问用户：先修哪个？选项：A / B"); err != nil || len(g.told) != 1 || !strings.Contains(g.told[0], "先修哪个") {
		t.Errorf("HandOff: err=%v told=%q", err, g.told)
	}
	if !strings.Contains(g.told[0], "一律用 ask 问用户") {
		t.Errorf("HandOff receipt lacks the irreversible rule: %q", g.told[0])
	}
	if err := r.HandOff(dashKey, "x"); !errors.Is(err, custody.ErrOff) || len(g.told) != 1 {
		t.Errorf("HandOff out of custody: err=%v told=%q", err, g.told)
	}
	if err := r.HandOff(key, "在问用户：现在 push 吗？选项：push / 先不"); !errors.Is(err, custody.ErrIrreversible) || len(g.told) != 1 {
		t.Errorf("HandOff of an irreversible choice: err=%v told=%q", err, g.told)
	}
}

// The format a receipt spells out parses as written, with only text to
// fill in.
func TestCustodyFormatParses(t *testing.T) {
	text := strings.ReplaceAll(strings.TrimPrefix(custodyFormat(dashKey), "格式："), "…", "继续")
	b := Parse(text)
	if len(b) != 1 || b[0].Err != nil || b[0].Custody == nil || b[0].Custody.Key != dashKey || b[0].Custody.Text != "继续" {
		t.Fatalf("Parse(%q) = %+v", text, b)
	}
}
