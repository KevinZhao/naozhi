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
// alone. Turning the switch off keeps a retry due.
func TestRetryEveryLocalSession(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		setup func(r *rig)
		want  bool
	}{
		{name: "direct chat", key: dash, want: true},
		{name: "user at it", key: dash, setup: func(r *rig) { r.e.SetViewing(dash, "tab", true); r.lastUser[dash] = r.now }, want: true},
		{name: "switched off", key: "task:a", setup: func(r *rig) { r.dispatch("task:a"); r.e.Set("task:a", false, ByUser) }, want: true},
		{name: "cron", key: "cron:nightly"},
		{name: "scratch", key: sessionkey.ScratchKeyPrefix + "x"},
		{name: "remote", key: "remote/" + dash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			if tc.setup != nil {
				tc.setup(r)
			}
			u := health.Unit{ID: tc.key, Kind: health.KindSession, State: health.Failed, ErrClass: health.ErrNetwork, LastActive: r.now}
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
