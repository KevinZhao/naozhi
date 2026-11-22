package custody

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
)

// down is key's turn failed at at with class.
func down(key string, class health.ErrorClass, at time.Time) health.Unit {
	return health.Unit{ID: key, Kind: health.KindSession, State: health.Failed, ErrClass: class, Since: at, LastActive: at, TurnAt: at}
}

// up is key's turn started (ended false) or ended normally at at.
func up(key string, at time.Time, ended bool) health.Unit {
	state := health.Active
	if ended {
		state = health.Idle
	}
	return health.Unit{ID: key, Kind: health.KindSession, State: state, Since: at, LastActive: at, TurnAt: at}
}

// A network or region failure counts toward no stop (D140): its retries go
// on past MaxErrors of the class and MaxSends in all with the session making
// no progress, and the lineage's own count is neither moved nor read by
// them, so what else custody sends is limited as if they never happened. A
// lineage already at its limit is waited out the same: the failure stays
// custody's, the stop being for its next counted message.
func TestWaitedClassCountsTowardNoStop(t *testing.T) {
	for _, class := range []health.ErrorClass{health.ErrNetwork, health.ErrRegion} {
		// One short of the stop for want of progress, and right at it.
		for _, before := range []int{MaxIdle - 1, MaxIdle} {
			t.Run(fmt.Sprintf("%s after %d", class, before), func(t *testing.T) {
				r := newRig(t)
				r.dispatch("task:a")
				r.progress["task:a"] = "same"
				for range before {
					if _, err := r.e.Deliver("task:a", ReasonPlanner, "接着做"); err != nil {
						t.Fatal(err)
					}
				}
				for i := 1; i <= MaxSends+MaxErrors; i++ {
					u := down("task:a", class, r.now)
					r.units = []health.Unit{u}
					if !r.e.Covers(u) {
						t.Fatalf("failure %d not covered", i)
					}
					r.e.Tick()
					r.now = r.now.Add(waitFor(i))
					r.e.Tick()
					if len(r.sent) != before+i || r.marks[before+i-1] != fmt.Sprintf("出错重试 %d", i) {
						t.Fatalf("failure %d: sent %q %q, want its retry, numbered within the run", i, r.sent, r.marks)
					}
					r.now = r.now.Add(time.Minute)
				}
				if st := r.e.Status("task:a"); st.State != StateOn || st.Sent != before || len(r.inbox.items) != 0 {
					t.Fatalf("status = %+v, inbox %+v; want on, the lineage's count at %d, nothing left to the user", st, r.inbox.items, before)
				}
				for n := before + 1; n <= MaxIdle; n++ {
					if got, err := r.e.Deliver("task:a", ReasonPlanner, "接着做"); err != nil || got != n {
						t.Errorf("the Planner's next: n=%d err=%v, want %d", got, err, n)
					}
				}
				if _, err := r.e.Deliver("task:a", ReasonPlanner, "接着做"); !errors.Is(err, ErrStopped) {
					t.Errorf("the one past MaxIdle: %v, want the stop for want of progress", err)
				}
			})
		}
	}
}

// The retries of a class custody waits out come 1, 2, 4 and 8 minutes after
// their failures, then every WaitEvery.
func TestWaitSchedule(t *testing.T) {
	r := newRig(t)
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, wait := range want {
		r.units = []health.Unit{down(dash, health.ErrNetwork, r.now)}
		r.e.Tick()
		if m, _ := r.sessions.Get(dash); m.Retry == nil || m.Retry.Done || !m.Retry.At.Equal(r.now.Add(wait)) {
			t.Fatalf("failure %d: retry %+v, want one due in %s", i+1, m.Retry, wait)
		}
		r.now = r.now.Add(wait - time.Second)
		r.e.Tick()
		if len(r.sent) != i {
			t.Fatalf("failure %d: sent %d before its wait %s", i+1, len(r.sent), wait)
		}
		r.now = r.now.Add(time.Second)
		r.e.Tick()
		if len(r.sent) != i+1 {
			t.Fatalf("failure %d: sent %d at its wait %s", i+1, len(r.sent), wait)
		}
		r.now = r.now.Add(time.Minute)
	}
}

// A session whose turn ended normally after a network or region failure is
// the sign the cause lifted: the other sessions waiting on that class go at
// once, whoever resumed it (custody's retry, or someone else before it);
// those waiting on another class wait on, and a turn only started releases
// nothing.
func TestRecoveryReleasesTheClass(t *testing.T) {
	const a, b, region, server = "dashboard:direct:u:a", "dashboard:direct:u:b", "dashboard:direct:u:region", "dashboard:direct:u:server"
	for _, tc := range []struct {
		name   string
		resume func(r *rig) // has a resumed, its turn running
	}{
		{"custody's retry", func(r *rig) {
			r.now = r.now.Add(Backoff[0])
			r.e.Tick()
			if len(r.sent) != 1 || r.sent[0] != a+"|"+RetryText {
				r.t.Fatalf("sent %q, want a's retry", r.sent)
			}
			r.sent = nil
		}},
		{"someone else, before the retry", func(r *rig) { r.now = r.now.Add(20 * time.Second) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.failText[server] = serverError
			r.units = []health.Unit{down(a, health.ErrNetwork, r.now)}
			r.e.Tick()
			// The others fail as a resumes, so their waits are all ahead.
			tc.resume(r)
			waiting := []health.Unit{down(b, health.ErrNetwork, r.now), down(region, health.ErrRegion, r.now),
				{ID: server, Kind: health.KindSession, State: health.Failed, ErrClass: health.ErrOther, LastActive: r.now}}
			r.units = append(waiting, up(a, r.now, false))
			r.e.Tick()
			r.e.Tick()
			if len(r.sent) != 0 {
				t.Fatalf("sent %q on a turn only started", r.sent)
			}
			r.now = r.now.Add(10 * time.Second)
			r.units = append(waiting, up(a, r.now, true))
			r.e.Tick() // a recovered: b is due now
			r.e.Tick()
			if len(r.sent) != 1 || r.sent[0] != b+"|"+RetryText {
				t.Errorf("sent %q, want b's retry alone, at once", r.sent)
			}
		})
	}
}

// Six hours after the first failure of a run custody stops waiting and
// leaves the session to the user as the failure it is, saying how long it
// waited; the run is kept across a restart; the user's message starts it
// afresh.
func TestWaitHandsOverAfterMaxWait(t *testing.T) {
	r := newRig(t)
	r.progress[dash] = "same"
	start := r.now
	n := 0
	for {
		n++
		u := down(dash, health.ErrNetwork, r.now)
		r.units = []health.Unit{u}
		// Covered to the end: the item is custody's, which says how long
		// it waited.
		if !r.e.Covers(u) {
			t.Fatalf("failure %d, %s in: not covered", n, r.now.Sub(start))
		}
		r.e.Tick()
		stopped := r.e.Status(dash).State == StateStopped
		if r.now.Sub(start) >= MaxWait {
			if !stopped {
				t.Fatalf("failure %d, %s in: not handed over", n, r.now.Sub(start))
			}
			break
		}
		if stopped {
			t.Fatalf("failure %d, %s in: handed over", n, r.now.Sub(start))
		}
		if n == 10 {
			r.open()
		}
		r.now = r.now.Add(waitFor(n))
		r.e.Tick()
		if len(r.sent) != n {
			t.Fatalf("failure %d: sent %d", n, len(r.sent))
		}
		r.now = r.now.Add(time.Minute)
	}
	// 1+2+4+8 minutes and a minute to fail after each, then every 16th.
	const why = "托管等了 6 小时 11 分钟、重试了 26 次还是出错（网络断了）。发「继续」再试"
	if it := r.inbox.items; len(it) != 1 || it[0].Kind != notify.KindError || it[0].Source != "failed:"+dash || it[0].Why != why {
		t.Errorf("inbox = %+v, want the failure saying %q", it, why)
	}
	if st := r.e.Status(dash); st.Why != why {
		t.Errorf("status = %+v", st)
	}
	r.now = r.now.Add(time.Hour)
	r.e.Tick()
	if len(r.sent) != n-1 {
		t.Errorf("sent %d after the hand-over, want %d", len(r.sent), n-1)
	}

	r.lastUser[dash] = r.now
	r.now = r.now.Add(time.Minute)
	r.units = []health.Unit{down(dash, health.ErrNetwork, r.now)}
	r.e.Tick()
	r.now = r.now.Add(Backoff[0])
	r.e.Tick()
	if len(r.sent) != n || r.marks[n-1] != "出错重试 1" {
		t.Errorf("after the user's message: sent %d %q, want a new run's first retry", len(r.sent), r.marks[len(r.marks)-1])
	}
}

// A run ends only with a turn that ended normally. A failure long after the
// one before it (a retry that ran for a while, a sleep) goes on with the run,
// the gap counting waitGap toward MaxWait: it is not handed over for a night
// asleep, and its wait does not fall back to the first. One of the other
// class custody waits out goes on with it too, as does someone else's resume
// that failed again.
func TestWaitRunSpansAGap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		then   func(r *rig) // from the first failure, registered, to the time of the second
		class  health.ErrorClass
		sent   int
		waited time.Duration // the run's, at the second failure
	}{
		{"failed again at once", func(r *rig) {
			r.now = r.now.Add(Backoff[0])
			r.e.Tick()
			r.now = r.now.Add(time.Minute)
		}, health.ErrNetwork, 1, 2 * time.Minute},
		{"failed again as the other class", func(r *rig) {
			r.now = r.now.Add(Backoff[0])
			r.e.Tick()
			r.now = r.now.Add(time.Minute)
		}, health.ErrRegion, 1, 2 * time.Minute},
		{"resumed by someone else, failed again", func(r *rig) {
			r.now = r.now.Add(20 * time.Second)
			r.units = []health.Unit{up(dash, r.now, false)}
			r.e.Tick()
			r.now = r.now.Add(20 * time.Second)
		}, health.ErrNetwork, 0, 40 * time.Second},
		{"the retry ran past MaxWait", func(r *rig) {
			r.now = r.now.Add(Backoff[0])
			r.e.Tick()
			r.units = []health.Unit{up(dash, r.now, false)}
			r.now = r.now.Add(MaxWait + time.Hour)
			r.e.Tick()
		}, health.ErrNetwork, 1, waitGap},
		{"asleep past MaxWait", func(r *rig) {
			r.now = r.now.Add(MaxWait + 3*time.Hour)
			r.woke = true
			r.e.Tick() // the retry goes at once
			r.now = r.now.Add(2 * time.Minute)
		}, health.ErrNetwork, 1, waitGap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.units = []health.Unit{down(dash, health.ErrNetwork, r.now)}
			r.e.Tick()
			tc.then(r)
			if len(r.sent) != tc.sent {
				t.Fatalf("sent %q, want %d", r.sent, tc.sent)
			}
			u := down(dash, tc.class, r.now)
			r.units = []health.Unit{u}
			if !r.e.Covers(u) {
				t.Error("the second failure is not covered")
			}
			r.e.Tick()
			m, _ := r.sessions.Get(dash)
			if got := m.Retry; got == nil || got.Done || got.N != 2 || r.now.Sub(got.Since) != tc.waited || !got.At.Equal(r.now.Add(waitFor(2))) {
				t.Errorf("retry = %+v at %s, want the run's second failure, %s waited, due in %s", got, r.now, tc.waited, waitFor(2))
			}
			if st := r.e.Status(dash); st.State == StateStopped || len(r.inbox.items) != 0 {
				t.Errorf("status = %+v, inbox %+v; want waited out", st, r.inbox.items)
			}
		})
	}
}

// The time waited adds up over a run's gaps to MaxWait, each counting
// waitGap at most, however long every retry's turn ran before it failed
// again and whatever sleep fell between: the waits stay WaitEvery apart, and
// the session is handed over in the end, so none gets "继续" without end.
func TestWaitRunEndsThoughItsGapsAreLong(t *testing.T) {
	for _, tc := range []struct {
		name    string
		class   health.ErrorClass
		ran     time.Duration // each retry's turn, before it failed again
		asleep  time.Duration // the machine's sleep in place of every third wait
		retries int
		waited  string
	}{
		{name: "each retry ran 16 minutes", class: health.ErrNetwork, ran: 16 * time.Minute, retries: 14, waited: "6 小时 19 分钟"},
		{name: "each retry ran 31 minutes", class: health.ErrRegion, ran: 31 * time.Minute, retries: 12, waited: "6 小时"},
		{name: "asleep ten hours at every third wait", class: health.ErrRegion, ran: time.Minute, asleep: 10 * time.Hour,
			retries: 20, waited: "6 小时 10 分钟"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			n := 0
			for {
				n++
				u := down(dash, tc.class, r.now)
				r.units = []health.Unit{u}
				if !r.e.Covers(u) {
					t.Fatalf("failure %d not covered", n)
				}
				r.e.Tick()
				if r.e.Status(dash).State == StateStopped {
					break
				}
				if n > tc.retries {
					t.Fatalf("failure %d: not handed over after %d retries", n, len(r.sent))
				}
				if m, _ := r.sessions.Get(dash); m.Retry == nil || !m.Retry.At.Equal(r.now.Add(waitFor(n))) {
					t.Fatalf("failure %d: retry %+v, want one due in %s", n, m.Retry, waitFor(n))
				}
				if tc.asleep > 0 && n%3 == 0 {
					r.now = r.now.Add(tc.asleep)
					r.woke = true
				} else {
					r.now = r.now.Add(waitFor(n))
				}
				r.e.Tick()
				if len(r.sent) != n {
					t.Fatalf("failure %d: sent %d", n, len(r.sent))
				}
				r.units = []health.Unit{up(dash, r.now, false)}
				r.e.Tick()
				r.now = r.now.Add(tc.ran)
			}
			why := fmt.Sprintf("托管等了 %s、重试了 %d 次还是出错（%s）。发「继续」再试", tc.waited, tc.retries, ClassLabel(string(tc.class)))
			if it := r.inbox.items; len(r.sent) != tc.retries || len(it) != 1 || it[0].Source != "failed:"+dash || it[0].Why != why {
				t.Errorf("sent %d, inbox %+v; want %d retries, then the failure saying %q", len(r.sent), it, tc.retries, why)
			}
		})
	}
}

// A dispatched session that got through and whose task was marked done
// before custody saw its turn's end is retried no more, and is the sign all
// the same: the others waiting on its class go at once, and its own retry is
// dropped.
func TestRecoveryOfAFinishedTaskReleasesTheClass(t *testing.T) {
	r := newRig(t)
	r.dispatch("task:a")
	r.units = []health.Unit{down("task:a", health.ErrNetwork, r.now)}
	r.e.Tick()
	r.now = r.now.Add(Backoff[0])
	r.e.Tick()
	if len(r.sent) != 1 || r.sent[0] != "task:a|"+RetryText {
		t.Fatalf("sent %q, want the task's retry", r.sent)
	}
	r.sent = nil
	waiting := []health.Unit{down(dash, health.ErrNetwork, r.now)}
	r.units = append(waiting, up("task:a", r.now, false))
	r.e.Tick()
	r.now = r.now.Add(10 * time.Second)
	r.mark("task:a", "done")
	r.units = append(waiting, up("task:a", r.now, true))
	r.e.Tick()
	r.e.Tick()
	if len(r.sent) != 1 || r.sent[0] != dash+"|"+RetryText {
		t.Errorf("sent %q, want the waiting session's retry, at once", r.sent)
	}
	if m, _ := r.sessions.Get("task:a"); m.Retry != nil {
		t.Errorf("the finished task's retry = %+v, want none", m.Retry)
	}
}
