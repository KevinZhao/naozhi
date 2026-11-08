package custody

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// A failure's retry is every local session's, its switch on or off (D94):
// the user's own chat included, not held back while they are at it (the
// failure is the reply to their own message), and noted nowhere but the
// message itself; a cron run and a scratch session are one-shot and left
// alone. Turning the switch off keeps a retry due. An IM chat's reply would
// not reach the chat, so it is retried only in custody; and a failure from
// long before custody saw it (one held across an upgrade) is left as it is.
func TestRetryEveryLocalSession(t *testing.T) {
	const im = "feishu:direct:alice:general"
	for _, tc := range []struct {
		name  string
		key   string
		age   time.Duration // of the failure when custody first sees it
		setup func(r *rig)
		want  bool
	}{
		{name: "direct chat", key: dash, want: true},
		{name: "user at it", key: dash, setup: func(r *rig) { r.e.SetViewing(dash, "tab", true); r.lastUser[dash] = r.now }, want: true},
		{name: "switched off", key: "task:a", setup: func(r *rig) { r.dispatch("task:a"); r.e.Set("task:a", false, ByUser) }, want: true},
		{name: "dispatched, running", key: "task:a", setup: func(r *rig) { r.dispatch("task:a"); r.mark("task:a", "doing") }, want: true},
		{name: "dispatched, done", key: "task:a", setup: func(r *rig) { r.dispatch("task:a"); r.mark("task:a", "done") }},
		{name: "dispatched, done and put away", key: "task:a", setup: func(r *rig) {
			r.dispatch("task:a")
			r.mark("task:a", "done")
			r.update("task:a", func(m *sidecar.Meta) { m.Reap = &sidecar.Reap{At: r.now, By: "planner"} })
		}},
		{name: "dispatched, put away though not marked", key: "task:a", setup: func(r *rig) {
			r.dispatch("task:a")
			r.update("task:a", func(m *sidecar.Meta) { m.Reap = &sidecar.Reap{At: r.now, By: "planner"} })
		}},
		{name: "dispatched, done, custody on by the user", key: "task:a", setup: func(r *rig) {
			r.dispatch("task:a")
			r.mark("task:a", "done")
			r.e.Set("task:a", true, ByUser)
		}, want: true},
		{name: "cron", key: "cron:nightly"},
		{name: "scratch", key: sessionkey.ScratchKeyPrefix + "x"},
		{name: "remote", key: "remote/" + dash},
		{name: "IM chat", key: im},
		{name: "IM chat in custody", key: im, setup: func(r *rig) { r.e.Set(im, true, ByUser) }, want: true},
		{name: "project planner", key: "project:x:planner"},
		{name: "a fresh failure", key: dash, age: staleAfter, want: true},
		{name: "ten days old", key: dash, age: 10 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			if tc.setup != nil {
				tc.setup(r)
			}
			u := health.Unit{ID: tc.key, Kind: health.KindSession, State: health.Failed, ErrClass: health.ErrNetwork,
				LastActive: r.now.Add(-tc.age), Since: r.now.Add(-tc.age)}
			r.units = []health.Unit{u}
			if got := r.e.Covers(u); got != tc.want {
				t.Errorf("Covers = %v, want %v", got, tc.want)
			}
			r.e.Tick()
			r.now = r.now.Add(Backoff[0])
			r.e.Tick()
			if got := len(r.sent) == 1; got != tc.want {
				t.Errorf("sent %q, want a retry %v", r.sent, tc.want)
			}
			if got := r.custodyEvents(tc.key); tc.want && (len(got) != 1 || !strings.Contains(got[0], `"sent"`)) {
				t.Errorf("notes = %v, want only the sent one", got)
			}
		})
	}
}

// A dispatched session whose task is over (done, or stopped by the user) is
// out of custody, unless the user or the Planner set its switch since.
func TestOnFinishedTask(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mark    string
		custody *sidecar.Custody
		want    bool
	}{
		{name: "running", mark: "doing", custody: &sidecar.Custody{On: true, By: ByAuto}, want: true},
		{name: "done", mark: "done", custody: &sidecar.Custody{On: true, By: ByAuto}},
		{name: "done before custody", mark: "done"},
		{name: "failed", mark: "failed", custody: &sidecar.Custody{On: true, By: ByAuto}, want: true},
		{name: "done, on by the user", mark: "done", custody: &sidecar.Custody{On: true, By: ByUser}, want: true},
		{name: "done, on by the Planner", mark: "done", custody: &sidecar.Custody{On: true, By: ByPlanner}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			m := sidecar.Meta{Key: "task:a", Purpose: sidecar.PurposeTask, Parent: sessionkey.PlannerMainKey, Custody: tc.custody,
				Mark: &sidecar.Mark{State: tc.mark, By: "user", At: r.now}}
			if err := r.sessions.Put(m); err != nil {
				t.Fatal(err)
			}
			if got := r.e.On("task:a"); got != tc.want {
				t.Errorf("On = %v, want %v", got, tc.want)
			}
		})
	}
}

// The user's message to a stopped session takes it over: the stop and the
// count go at the next tick, the switch stays as it was. A direct session
// whose retries gave up reads stopped though off, until the user writes.
func TestTakenOver(t *testing.T) {
	r := newRig(t)
	r.dispatch("task:a")
	for range MaxIdle {
		r.e.Deliver("task:a", ReasonPlanner, "接着做")
	}
	if _, err := r.e.Deliver("task:a", ReasonPlanner, "接着做"); !errors.Is(err, ErrStopped) {
		t.Fatalf("not stopped: %v", err)
	}
	r.now = r.now.Add(time.Minute)
	r.e.Tick()
	if st := r.e.Status("task:a"); st.State != StateStopped {
		t.Fatalf("lifted with no message: %+v", st)
	}
	r.lastUser["task:a"] = r.now
	r.now = r.now.Add(NearbyFor) // past nearby, so a reply may go
	r.e.Tick()
	if st := r.e.Status("task:a"); st.State != StateOn || st.Sent != 0 || !st.On {
		t.Errorf("after the user's message: %+v, want on with a fresh count", st)
	}
	if n, err := r.e.Deliver("task:a", ReasonPlanner, "x"); n != 1 || err != nil {
		t.Errorf("after the take-over: n=%d err=%v", n, err)
	}

	r.units = []health.Unit{{ID: dash, Kind: health.KindSession, State: health.Failed, ErrClass: health.ErrNetwork}}
	r.progress[dash] = "same"
	for i := range MaxIdle + 1 {
		r.units[0].LastActive = r.now
		r.e.Tick()
		r.now = r.now.Add(Backoff[min(i, len(Backoff)-1)])
		r.e.Tick()
	}
	if st := r.e.Status(dash); st.State != StateStopped || st.On {
		t.Fatalf("direct session = %+v, want stopped and off", st)
	}
	if !slices.ContainsFunc(r.e.List(), func(s Status) bool { return s.Key == dash }) {
		t.Error("a stopped session out of custody is not listed")
	}
	r.lastUser[dash] = r.now.Add(time.Second)
	r.now = r.now.Add(time.Minute)
	r.e.Tick()
	if st := r.e.Status(dash); st.State != StateOff {
		t.Errorf("after the user's message: %+v, want off", st)
	}
}

// After the machine woke, a network error's retry is due at once; another
// class keeps its backoff.
func TestWokeRetriesNetwork(t *testing.T) {
	for _, tc := range []struct {
		class health.ErrorClass
		want  int
	}{{health.ErrNetwork, 1}, {health.ErrRegion, 0}} {
		r := newRig(t)
		r.units = []health.Unit{{ID: dash, Kind: health.KindSession, State: health.Failed, ErrClass: tc.class, LastActive: r.now}}
		r.e.Tick()
		r.now = r.now.Add(time.Second)
		r.woke = true
		r.e.Tick()
		r.e.Tick()
		if len(r.sent) != tc.want {
			t.Errorf("%s: sent %q after waking, want %d", tc.class, r.sent, tc.want)
		}
	}
}

// A failure for an expired credential waits for its refresh: the user
// hears once what to do (unless the inbox reports it with a systemic fault),
// nothing is sent while the credential expires no later, and one "继续" goes
// once it was refreshed. With no credential to watch it is the user's.
func TestCredentialRefresh(t *testing.T) {
	expired := func(r *rig) health.Unit {
		return health.Unit{ID: dash, Kind: health.KindSession, State: health.Failed, ErrClass: health.ErrCredentialExpired, LastActive: r.now}
	}
	t.Run("refreshed", func(t *testing.T) {
		r := newRig(t)
		r.cred = r.now.Add(-time.Minute)
		r.units = []health.Unit{expired(r)}
		if !r.e.Covers(r.units[0]) {
			t.Fatal("not covered")
		}
		for range 3 {
			r.e.Tick()
			r.now = r.now.Add(time.Hour)
		}
		if it := r.inbox.items; len(r.sent) != 0 || len(it) != 1 || it[0].Source != "failed:"+dash ||
			it[0].Why != "凭证过期；"+credWhy {
			t.Fatalf("before the refresh: sent %q, inbox %+v", r.sent, it)
		}
		r.cred = r.now.Add(20 * time.Hour) // mwinit
		r.e.Tick()
		r.e.Tick()
		if len(r.sent) != 1 || r.sent[0] != dash+"|"+RetryText || r.marks[0] != "凭证刷新后继续 1" {
			t.Errorf("after the refresh: sent %q %q", r.sent, r.marks)
		}
	})
	t.Run("paused meanwhile", func(t *testing.T) {
		r := newRig(t)
		r.cred = r.now.Add(-time.Minute)
		r.units = []health.Unit{expired(r)}
		r.e.Tick()
		r.until = r.now.Add(30 * time.Minute)
		r.e.Tick()
		r.now = r.until.Add(time.Minute)
		r.cred = r.now.Add(20 * time.Hour) // mwinit
		r.e.Tick()
		r.e.Tick()
		if len(r.sent) != 1 || len(r.inbox.items) != 1 || r.inbox.items[0].Why != "凭证过期；"+credWhy {
			t.Errorf("after a pause and the refresh: sent %q, inbox %+v", r.sent, r.inbox.items)
		}
	})
	t.Run("in a systemic fault", func(t *testing.T) {
		r := newRig(t)
		r.cred = r.now
		u := expired(r)
		r.units = []health.Unit{u}
		r.e.HandleHealth(health.Event{Transition: &health.Transition{Unit: u, InFault: true}})
		r.e.Tick()
		if len(r.inbox.items) != 0 {
			t.Errorf("inbox = %+v, want the fault's item only", r.inbox.items)
		}
	})
	t.Run("nothing to watch", func(t *testing.T) {
		r := newRig(t)
		if u := expired(r); r.e.Covers(u) {
			t.Error("covered with no credential to watch")
		}
	})
}

// A failure that recovered (a turn ended normally since) ends its count: a
// direct chat's failures days apart, each recovered, get a retry each and
// never stop custody; failures in a row still do.
func TestRetryCountEndsWithRecovery(t *testing.T) {
	r := newRig(t)
	for day := range MaxErrors + 2 {
		r.units = []health.Unit{{ID: dash, Kind: health.KindSession, State: health.Failed, ErrClass: health.ErrNetwork, LastActive: r.now}}
		r.e.Tick()
		r.now = r.now.Add(Backoff[0])
		r.e.Tick()
		if len(r.sent) != day+1 || r.marks[day] != "出错重试 1" {
			t.Fatalf("blip %d: sent %q %q, want one retry each, counted afresh", day+1, r.sent, r.marks)
		}
		r.units[0].State, r.units[0].LastActive = health.Idle, r.now
		r.e.Tick()
		r.now = r.now.Add(24 * time.Hour)
	}
	if st := r.e.Status(dash); st.State != StateOff || len(r.inbox.items) != 0 {
		t.Errorf("after recovered blips: %+v, inbox %+v", st, r.inbox.items)
	}

	// A dispatched session's sends still add up over its lineage.
	r.dispatch("task:a")
	r.units = []health.Unit{{ID: "task:a", Kind: health.KindSession, State: health.Failed, ErrClass: health.ErrNetwork, LastActive: r.now}}
	r.e.Tick()
	r.now = r.now.Add(Backoff[0])
	r.e.Tick()
	r.units[0].State, r.units[0].LastActive = health.Idle, r.now
	r.e.Tick()
	if st := r.e.Status("task:a"); st.Sent != 1 {
		t.Errorf("dispatched session after a recovered failure: %+v, want its send still counted", st)
	}
}

// A retry due while custody is paused waits for the pause to end and goes
// out then, at once if due by then, as its backoff has it otherwise; the
// failure opens no item meanwhile. One the pause kept past staleAfter is
// left to the user instead, as a failure that old is, a tick or not while
// the pause held (asleep from the pause on, or restarted, to the wake).
func TestRetryAcrossPause(t *testing.T) {
	const never = time.Duration(-1)
	for _, tc := range []struct {
		name               string
		pauseAt, pauseFor  time.Duration    // from the failure; pauseFor 0: no pause
		asleep             [2]time.Duration // no tick from [0] until [1], woken then
		restart            bool             // restarted as it fell asleep
		wantSent, wantLeft time.Duration    // when the retry went out, the failure was left; never
	}{
		{name: "no pause", wantSent: Backoff[0], wantLeft: never},
		{name: "paused a minute", pauseAt: 30 * time.Second, pauseFor: time.Minute, wantSent: 90 * time.Second, wantLeft: never},
		{name: "pause over before the retry is due", pauseFor: 30 * time.Second, wantSent: Backoff[0], wantLeft: never},
		{name: "failed while paused, 29 minutes", pauseFor: 29 * time.Minute, wantSent: 29 * time.Minute, wantLeft: never},
		{name: "paused past staleAfter", pauseAt: 30 * time.Second, pauseFor: time.Hour, wantSent: never, wantLeft: staleAfter + tickInterval},
		{name: "asleep from the pause past its end", pauseAt: 30 * time.Second, pauseFor: time.Hour,
			asleep: [2]time.Duration{30 * time.Second, 2 * time.Hour}, wantSent: never, wantLeft: 2 * time.Hour},
		{name: "restarted at the pause, back past its end", pauseAt: 30 * time.Second, pauseFor: time.Hour,
			asleep: [2]time.Duration{30 * time.Second, 2 * time.Hour}, restart: true, wantSent: never, wantLeft: 2 * time.Hour},
		{name: "failed while paused, asleep past staleAfter", pauseFor: 40 * time.Minute,
			asleep: [2]time.Duration{tickInterval, 50 * time.Minute}, wantSent: never, wantLeft: 50 * time.Minute},
		{name: "asleep from the pause, woken fresh", pauseAt: 30 * time.Second, pauseFor: time.Minute,
			asleep: [2]time.Duration{30 * time.Second, 10 * time.Minute}, wantSent: 10 * time.Minute, wantLeft: never},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			start := r.now
			u := health.Unit{ID: dash, Kind: health.KindSession, State: health.Failed, ErrClass: health.ErrNetwork, Since: start, LastActive: start}
			r.units = []health.Unit{u}
			sent, left := never, never
			for d := time.Duration(0); d <= 130*time.Minute; d += tickInterval {
				r.now = start.Add(d)
				if tc.pauseFor > 0 && d == tc.pauseAt {
					r.e.Pause(r.now.Add(tc.pauseFor))
				}
				if d == 0 && !r.e.Covers(u) {
					t.Fatal("not covered")
				}
				if d == tc.asleep[0] && tc.restart {
					r.open()
				}
				if d >= tc.asleep[0] && d < tc.asleep[1] {
					continue
				}
				r.woke = tc.asleep[1] > 0 && d == tc.asleep[1]
				r.e.Tick()
				if sent == never && len(r.sent) > 0 {
					sent = d
				}
				if left == never && len(r.inbox.items) > 0 {
					left = d
				}
			}
			if sent != tc.wantSent || left != tc.wantLeft || len(r.sent) > 1 {
				t.Errorf("sent %q at %v, left at %v (%+v); want sent at %v, left at %v", r.sent, sent, left, r.inbox.items, tc.wantSent, tc.wantLeft)
			}
			if left != never && (r.inbox.items[0].Source != "failed:"+dash || !strings.Contains(r.inbox.items[0].Why, "托管已暂停")) {
				t.Errorf("left as %+v", r.inbox.items[0])
			}
		})
	}
}

func (r *rig) update(key string, fn func(m *sidecar.Meta)) {
	r.t.Helper()
	if _, err := r.sessions.Update(key, func(m *sidecar.Meta) bool { fn(m); return true }); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) mark(key, state string) {
	r.t.Helper()
	r.update(key, func(m *sidecar.Meta) { m.Mark = &sidecar.Mark{State: state, By: "planner", At: r.now} })
}

// A dispatched session whose process died unexpectedly is resumed while its
// task runs (#3883); the same death once the task is done and put away (the
// reap closed it) is left alone.
func TestRetryCrashOnlyWhileTaskRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		over bool
	}{{"running", false}, {"done and put away", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.dispatch("task:a")
			if tc.over {
				r.mark("task:a", "done")
				r.update("task:a", func(m *sidecar.Meta) { m.Reap = &sidecar.Reap{At: r.now, By: "planner"} })
			}
			u := health.Unit{ID: "task:a", Kind: health.KindSession, State: health.Failed, ErrClass: health.ErrOther,
				Evidence: health.DeathEvidence + "shim_eof", LastActive: r.now, Since: r.now}
			r.units = []health.Unit{u}
			r.e.Tick()
			r.now = r.now.Add(Backoff[0])
			r.e.Tick()
			if got := len(r.sent) == 1; got == tc.over {
				t.Errorf("sent %q, want a retry %v", r.sent, !tc.over)
			}
		})
	}
}

// Blocked is what would refuse a message to a session put away whatever
// reopening it changes (its task done and its key not local): a switch set
// off by someone, the pause, a stopped lineage; a switch left to the
// default, which reads off only because the task is done, is no refusal.
func TestBlockedPutAway(t *testing.T) {
	r := newRig(t)
	key := "remote/task:a" // not local, as a session put away is not
	if err := r.sessions.Put(sidecar.Meta{Key: key, Purpose: sidecar.PurposeTask, Parent: sessionkey.PlannerMainKey,
		Mark: &sidecar.Mark{State: "done", By: "planner", At: r.now}}); err != nil {
		t.Fatal(err)
	}
	if r.e.On(key) || !errors.Is(r.e.Check(key), ErrOff) {
		t.Fatal("a done task put away reads in custody")
	}
	if err := r.e.Blocked(key); err != nil {
		t.Fatalf("default switch: Blocked = %v, want nil", err)
	}
	for _, by := range []string{ByUser, ByPlanner} {
		r.update(key, func(m *sidecar.Meta) { m.Custody = &sidecar.Custody{On: false, By: by, At: r.now} })
		if err := r.e.Blocked(key); !errors.Is(err, ErrOff) {
			t.Errorf("off by %s: Blocked = %v", by, err)
		}
	}
	r.update(key, func(m *sidecar.Meta) { m.Custody = &sidecar.Custody{On: true, By: ByUser, At: r.now} })
	if err := r.e.Blocked(key); err != nil {
		t.Errorf("on by the user: Blocked = %v", err)
	}
	r.until = r.now.Add(time.Hour)
	if err := r.e.Blocked(key); !errors.Is(err, ErrPaused) {
		t.Errorf("paused: Blocked = %v", err)
	}
	r.until = time.Time{}
	r.update(key, func(m *sidecar.Meta) {
		m.Chain = &sidecar.Chain{Sent: MaxSends, Stopped: "托管已替你发了 8 次，交还给你"}
	})
	if err := r.e.Blocked(key); !errors.Is(err, ErrStopped) {
		t.Errorf("stopped: Blocked = %v", err)
	}
}
