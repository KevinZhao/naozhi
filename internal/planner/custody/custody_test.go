package custody

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cliinfo"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

type fakeInbox struct {
	mu    sync.Mutex
	items []notify.Item
}

func (f *fakeInbox) Add(it notify.Item) (notify.Item, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, it)
	return it, true
}

// rig is an Engine over a sidecar directory and fakes for every source.
type rig struct {
	t        *testing.T
	dir      string
	now      time.Time
	sent     []string
	sendErr  error
	inbox    *fakeInbox
	journal  *journal.Journal
	units    []health.Unit
	lastUser map[string]time.Time
	progress map[string]string
	failText map[string]string
	running  []string
	sessions *sidecar.Store
	e        *Engine
}

const dash = "dashboard:direct:u:general"

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	j, err := journal.Open(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	r := &rig{t: t, dir: dir, now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC), inbox: &fakeInbox{}, journal: j,
		lastUser: map[string]time.Time{}, progress: map[string]string{}, failText: map[string]string{}}
	r.open()
	return r
}

// open (re)builds the engine over the same files, as a restart does.
func (r *rig) open() {
	r.sessions = sidecar.Open(filepath.Join(r.dir, "sessions"), func() time.Time { return r.now })
	r.e = New(Config{
		Sessions:  r.sessions,
		Send:      func(key, text string) error { r.sent = append(r.sent, key+"|"+text); return r.sendErr },
		Inbox:     r.inbox,
		Journal:   r.journal,
		Health:    func() health.Snapshot { return health.Snapshot{At: r.now, Units: r.units} },
		Local:     func(key string) bool { return !strings.HasPrefix(key, "remote/") },
		Workflows: func() []string { return r.running },
		LastUser:  func(key string) time.Time { return r.lastUser[key] },
		Progress:  func(key string) string { return r.progress[key] },
		FailText:  func(key string) string { return r.failText[key] },
		Path:      filepath.Join(r.dir, "custody.json"),
		Now:       func() time.Time { return r.now },
	})
}

func (r *rig) dispatch(key string) {
	r.t.Helper()
	if err := r.sessions.Put(sidecar.Meta{Key: key, Purpose: sidecar.PurposeTask, Parent: sessionkey.PlannerMainKey, Title: "修 bug"}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) custodyEvents(unit string) []string {
	r.t.Helper()
	entries, err := r.journal.Read(r.now.Add(-24*time.Hour), r.now.Add(time.Hour))
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Kind == journal.KindCustody && e.Unit == unit {
			out = append(out, string(e.Data))
		}
	}
	return out
}

// The switch's defaults: on for a session the Planner dispatched (one from
// before custody too), off for the rest; a set switch wins, and the Planner,
// scout runs, daemons and remote sessions are never in custody.
func TestOnAndSet(t *testing.T) {
	r := newRig(t)
	r.dispatch("task:a")
	if err := r.sessions.Put(sidecar.Meta{Key: "task:b", Purpose: sidecar.PurposeTask, Parent: "dashboard:x"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key  string
		want bool
	}{{"task:a", true}, {"task:b", false}, {dash, false}, {"cron:nightly", false}, {sessionkey.PlannerMainKey, false}} {
		if got := r.e.On(tc.key); got != tc.want {
			t.Errorf("On(%s) = %v, want %v", tc.key, got, tc.want)
		}
	}
	for _, tc := range []struct {
		key  string
		want error
	}{
		{sessionkey.PlannerMainKey, ErrRefused},
		{"sys:attachment-gc", ErrRefused},
		{sessionkey.ScoutKeyPrefix + "x", ErrRefused},
		{"remote/" + dash, ErrNoSession},
		{dash, nil},
		{"task:a", nil},
	} {
		if err := r.e.Set(tc.key, tc.key != "task:a", ByUser); !errors.Is(err, tc.want) {
			t.Errorf("Set(%s) = %v, want %v", tc.key, err, tc.want)
		}
	}
	if !r.e.On(dash) || r.e.On("task:a") {
		t.Errorf("after Set: On(dash)=%v On(task:a)=%v, want true false", r.e.On(dash), r.e.On("task:a"))
	}
	if m, _ := r.sessions.Get(dash); m.Custody == nil || m.Custody.By != ByUser || !m.Custody.At.Equal(r.now) {
		t.Errorf("dash sidecar custody = %+v", m.Custody)
	}
	if got := r.custodyEvents(dash); len(got) != 1 || !strings.Contains(got[0], `"event":"on"`) {
		t.Errorf("journal = %v", got)
	}
	r.open()
	if !r.e.On(dash) || r.e.On("task:a") {
		t.Error("the switch did not survive a restart")
	}
}

// Deliver prefixes and counts each message over the lineage; three in a row
// with no progress, or eight in all, stop the lineage and leave it to the
// user, and the user turning custody back on starts it afresh while the
// Planner's cannot.
func TestDeliverLimits(t *testing.T) {
	t.Run("no progress", func(t *testing.T) {
		r := newRig(t)
		r.dispatch("task:a")
		for i := 1; i <= MaxIdle; i++ {
			n, err := r.e.Deliver("task:a", ReasonPlanner, " 接着做 ")
			if err != nil || n != i {
				t.Fatalf("send %d: n=%d err=%v", i, n, err)
			}
		}
		if want := "task:a|⚙ 托管 · Planner 继续 · 第 1/8 次\n接着做"; r.sent[0] != want {
			t.Errorf("first message = %q, want %q", r.sent[0], want)
		}
		_, err := r.e.Deliver("task:a", ReasonPlanner, "接着做")
		if !errors.Is(err, ErrStopped) || len(r.sent) != MaxIdle {
			t.Fatalf("fourth with no progress: err=%v sent=%d", err, len(r.sent))
		}
		if len(r.inbox.items) != 1 || r.inbox.items[0].Kind != notify.KindQuestion || r.inbox.items[0].Source != "custody:task:a" {
			t.Errorf("inbox = %+v", r.inbox.items)
		}
		if st := r.e.Status("task:a"); st.State != StateStopped || st.Why != "连续 3 次没有进展" || st.Sent != 3 {
			t.Errorf("status = %+v", st)
		}
		if err := r.e.Set("task:a", true, ByPlanner); err != nil || r.e.Allowed("task:a") {
			t.Errorf("the Planner lifted the stop: err=%v allowed=%v", err, r.e.Allowed("task:a"))
		}
		r.open() // the stop survives a restart
		if r.e.Allowed("task:a") {
			t.Fatal("stop lost on restart")
		}
		if err := r.e.Set("task:a", true, ByUser); err != nil || !r.e.Allowed("task:a") {
			t.Fatalf("the user's on did not lift the stop: %v", err)
		}
		if n, err := r.e.Deliver("task:a", ReasonPlanner, "x"); n != 1 || err != nil {
			t.Errorf("after the user's on: n=%d err=%v, want a fresh count", n, err)
		}
	})
	t.Run("eight in all", func(t *testing.T) {
		r := newRig(t)
		r.dispatch("task:a")
		for i := 1; i <= MaxSends; i++ {
			r.progress["task:a"] = string(rune('a' + i)) // progress between every message
			if n, err := r.e.Deliver("task:a", ReasonRetry, RetryText); err != nil || n != i {
				t.Fatalf("send %d: n=%d err=%v", i, n, err)
			}
		}
		r.progress["task:a"] = "more"
		if _, err := r.e.Deliver("task:a", ReasonRetry, RetryText); !errors.Is(err, ErrStopped) || !strings.Contains(err.Error(), "8 次") {
			t.Fatalf("ninth: %v", err)
		}
		if !strings.HasPrefix(strings.TrimPrefix(r.sent[7], "task:a|"), "⚙ 托管 · 出错重试 · 第 8/8 次") {
			t.Errorf("eighth = %q", r.sent[7])
		}
	})
	t.Run("a lineage counts together", func(t *testing.T) {
		r := newRig(t)
		r.dispatch("task:a")
		if err := r.sessions.Put(sidecar.Meta{Key: "task:b", Purpose: sidecar.PurposeTask, Parent: "task:a"}); err != nil {
			t.Fatal(err)
		}
		r.e.Deliver("task:a", ReasonPlanner, "x")
		if err := r.e.Set("task:b", true, ByPlanner); err != nil {
			t.Fatal(err)
		}
		if n, err := r.e.Deliver("task:b", ReasonPlanner, "x"); n != 2 || err != nil {
			t.Errorf("child's first: n=%d err=%v, want 2 on the root's count", n, err)
		}
	})
	t.Run("off, refused and failed sends count nothing", func(t *testing.T) {
		r := newRig(t)
		if _, err := r.e.Deliver(dash, ReasonPlanner, "x"); !errors.Is(err, ErrOff) {
			t.Errorf("off: %v", err)
		}
		r.dispatch("task:a")
		r.sendErr = errors.New("busy")
		if _, err := r.e.Deliver("task:a", ReasonPlanner, "x"); err == nil {
			t.Error("a failed send reported sent")
		}
		r.sendErr = nil
		if n, _ := r.e.Deliver("task:a", ReasonPlanner, "x"); n != 1 {
			t.Errorf("after a failed send n = %d, want 1", n)
		}
	})
}

// While the user is at a session (viewing it, or sent it a message within
// NearbyFor) custody sends nothing and notes it once; once they leave it
// sends again. The pause holds everything and survives a restart.
func TestNearbyAndPause(t *testing.T) {
	r := newRig(t)
	r.dispatch("task:a")
	r.e.SetViewing("task:a", "tab1", true)
	for range 2 {
		if _, err := r.e.Deliver("task:a", ReasonRetry, RetryText); !errors.Is(err, ErrNearby) {
			t.Fatalf("viewing: %v", err)
		}
	}
	if st := r.e.Status("task:a"); st.State != StateDeferred {
		t.Errorf("status while viewing = %+v", st)
	}
	if got := r.custodyEvents("task:a"); len(got) != 1 || !strings.Contains(got[0], `"deferred"`) || !strings.Contains(got[0], "你在看") {
		t.Errorf("notes = %v, want one deferred", got)
	}
	r.e.SetViewing("task:a", "tab1", false)
	r.lastUser["task:a"] = r.now.Add(-NearbyFor + time.Second)
	if _, err := r.e.Deliver("task:a", ReasonRetry, RetryText); !errors.Is(err, ErrNearby) {
		t.Fatalf("a message %s ago: %v", NearbyFor-time.Second, err)
	}
	r.lastUser["task:a"] = r.now.Add(-NearbyFor)
	if _, err := r.e.Deliver("task:a", ReasonRetry, RetryText); err != nil || len(r.sent) != 1 {
		t.Fatalf("after leaving: err=%v sent=%v", err, r.sent)
	}

	if err := r.e.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	r.open()
	if p, _ := r.e.Paused(); !p {
		t.Fatal("pause lost on restart")
	}
	if _, err := r.e.Deliver("task:a", ReasonRetry, RetryText); !errors.Is(err, ErrPaused) {
		t.Errorf("paused: %v", err)
	}
	if st := r.e.Status("task:a"); st.State != StatePaused {
		t.Errorf("status while paused = %+v", st)
	}
	if err := r.e.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	if !r.e.Allowed("task:a") {
		t.Error("not allowed after resume")
	}
}

// A failure of a retryable class gets "继续" after 1, 2, 4, 8 minutes, the
// next failure being a new one when the session was active again; the fifth
// of a class is left to the user; one that is not retryable is the user's at
// once, and a session that recovered drops its retry.
func TestRetry(t *testing.T) {
	failed := func(key string, class health.ErrorClass, evidence string, active time.Time) health.Unit {
		return health.Unit{ID: key, Kind: health.KindSession, State: health.Failed, ErrClass: class, Evidence: evidence, LastActive: active}
	}
	t.Run("backoff then stop", func(t *testing.T) {
		r := newRig(t)
		r.dispatch("task:a")
		for i, wait := range Backoff {
			active := r.now
			r.units = []health.Unit{failed("task:a", health.ErrNetwork, "last turn ended in an API error", active)}
			r.progress["task:a"] = string(rune('a' + i))
			if !r.e.Covers(r.units[0]) {
				t.Fatalf("failure %d not covered", i+1)
			}
			r.e.Tick()
			r.now = r.now.Add(wait - time.Second)
			r.e.Tick()
			if len(r.sent) != i {
				t.Fatalf("failure %d: sent %d before its backoff %s", i+1, len(r.sent), wait)
			}
			r.now = r.now.Add(time.Second)
			r.e.Tick()
			r.e.Tick()
			if len(r.sent) != i+1 || !strings.HasSuffix(r.sent[i], "\n"+RetryText) || !strings.Contains(r.sent[i], "出错重试") {
				t.Fatalf("failure %d: sent %q", i+1, r.sent)
			}
			r.now = r.now.Add(time.Minute)
		}
		r.units = []health.Unit{failed("task:a", health.ErrNetwork, "", r.now)}
		if r.e.Covers(r.units[0]) {
			t.Error("the fifth failure is covered")
		}
		r.e.Tick()
		r.now = r.now.Add(time.Hour)
		r.e.Tick()
		if len(r.sent) != len(Backoff) {
			t.Errorf("sent %d, want %d", len(r.sent), len(Backoff))
		}
		if len(r.inbox.items) != 1 || r.inbox.items[0].Source != "failed:task:a" || !strings.Contains(r.inbox.items[0].Why, "同一类错误累计 5 次（网络 / DNS）") {
			t.Errorf("inbox = %+v", r.inbox.items)
		}
	})
	t.Run("recovered", func(t *testing.T) {
		r := newRig(t)
		r.dispatch("task:a")
		r.units = []health.Unit{failed("task:a", health.ErrRegion, "", r.now)}
		r.e.Tick()
		r.units = []health.Unit{{ID: "task:a", Kind: health.KindSession, State: health.Active}}
		r.now = r.now.Add(time.Hour)
		r.e.Tick()
		if m, _ := r.sessions.Get("task:a"); len(r.sent) != 0 || m.Retry != nil {
			t.Errorf("recovered: sent %v retry %+v", r.sent, m.Retry)
		}
	})
	t.Run("restart keeps a due retry", func(t *testing.T) {
		r := newRig(t)
		r.dispatch("task:a")
		r.units = []health.Unit{failed("task:a", health.ErrNetwork, "", r.now)}
		r.e.Tick()
		r.open()
		r.e.Tick() // the same failure: not counted again
		r.now = r.now.Add(time.Minute)
		r.e.Tick()
		if len(r.sent) != 1 {
			t.Errorf("sent %v after a restart", r.sent)
		}
		if m, _ := r.sessions.Get("task:a"); m.Chain == nil || m.Chain.Errors["network"] != 1 {
			t.Errorf("chain = %+v", m.Chain)
		}
	})
	for _, tc := range []struct {
		name      string
		unit      health.Unit
		text      string
		class     string
		retryable bool
	}{
		{"network", failed("task:a", health.ErrNetwork, "", time.Time{}), "", "network", true},
		{"region", failed("task:a", health.ErrRegion, "", time.Time{}), "", "region", true},
		{"5xx", failed("task:a", health.ErrOther, "last turn ended in an API error", time.Time{}), "API Error: 500 Internal error", ClassServer, true},
		{"overloaded", failed("task:a", health.ErrOther, "", time.Time{}), "API Error: 529 overloaded_error", ClassServer, true},
		{"watchdog", failed("task:a", health.ErrOther, health.DeathEvidence+cliinfo.DeathReasonNoOutputTimeout, time.Time{}), "", ClassTimeout, true},
		{"api timeout", failed("task:a", health.ErrOther, "", time.Time{}), "API Error: Request timed out", ClassTimeout, true},
		{"auth", failed("task:a", health.ErrAuth, "", time.Time{}), "API Error: 403", "auth", false},
		{"expired", failed("task:a", health.ErrCredentialExpired, "", time.Time{}), "", "credential_expired", false},
		{"throttled", failed("task:a", health.ErrRateLimit, "", time.Time{}), "API Error: 429", "rate_limit", false},
		{"refusal", failed("task:a", health.ErrRefusal, "", time.Time{}), "", "refusal", false},
		{"disk", failed("task:a", health.ErrDiskFull, "", time.Time{}), "", "disk_full", false},
		{"crash", failed("task:a", health.ErrOther, health.DeathEvidence+cliinfo.DeathReasonCLIExitedCodePrefix+"1", time.Time{}), "", ClassCrash, true},
		{"killed", failed("task:a", health.ErrNone, health.DeathEvidence+cliinfo.DeathReasonCLIExitedSignalPrefix+"killed", time.Time{}), "", ClassCrash, true},
		{"crash naming a denial", failed("task:a", health.ErrAuth, health.DeathEvidence+cliinfo.DeathReasonShimEOF, time.Time{}), "AccessDenied", "auth", false},
		{"startup failure", failed("task:a", health.ErrOther, "startup failed 3 times (other)", time.Time{}), "", "other", false},
		{"a number in plain text", failed("task:a", health.ErrOther, "", time.Time{}), "wrote 500 lines", "other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			class, retry := Classify(tc.unit, tc.text)
			if class != tc.class || retry != tc.retryable {
				t.Fatalf("Classify = %q %v, want %q %v", class, retry, tc.class, tc.retryable)
			}
			r := newRig(t)
			r.dispatch("task:a")
			tc.unit.LastActive = r.now
			r.units = []health.Unit{tc.unit}
			r.failText["task:a"] = tc.text
			if got := r.e.Covers(tc.unit); got != tc.retryable {
				t.Errorf("Covers = %v", got)
			}
			r.e.Tick()
			r.now = r.now.Add(time.Hour)
			r.e.Tick()
			if want := map[bool]int{true: 1, false: 0}[tc.retryable]; len(r.sent) != want {
				t.Errorf("sent %v, want %d", r.sent, want)
			}
		})
	}
	t.Run("off or paused", func(t *testing.T) {
		r := newRig(t)
		u := failed(dash, health.ErrNetwork, "", r.now)
		r.units = []health.Unit{u}
		if r.e.Covers(u) {
			t.Error("a session out of custody is covered")
		}
		r.dispatch("task:a")
		r.e.SetPaused(true)
		u.ID = "task:a"
		if r.e.Covers(u) {
			t.Error("covered while paused")
		}
	})
}

// A session with a workflow running is put in custody unless someone set
// its switch, and goes back to the default once its workflows are over
// (a failure's retry still due holding it); a cron run is not, and a
// dispatched session or a switch the user set stays.
func TestWorkflowAutoOn(t *testing.T) {
	r := newRig(t)
	const user, failing = "dashboard:direct:u:mine", "dashboard:direct:u:failing"
	r.dispatch("task:a")
	r.running = []string{dash, "cron:nightly", "dashboard:direct:u:off", user, failing, "task:a"}
	if err := r.e.Set("dashboard:direct:u:off", false, ByUser); err != nil {
		t.Fatal(err)
	}
	r.e.Tick()
	got := []bool{r.e.On(dash), r.e.On("cron:nightly"), r.e.On("dashboard:direct:u:off")}
	if !slices.Equal(got, []bool{true, false, false}) {
		t.Errorf("On = %v, want [true false false]", got)
	}
	if st := r.e.Status(dash); st.By != ByAuto || st.State != StateOn {
		t.Errorf("status = %+v", st)
	}
	if l := r.e.List(); len(l) != 4 || !slices.ContainsFunc(l, func(s Status) bool { return s.Key == dash }) {
		t.Errorf("List = %+v", l)
	}
	if err := r.e.Set(user, true, ByUser); err != nil {
		t.Fatal(err)
	}
	r.units = []health.Unit{{ID: failing, Kind: health.KindSession, State: health.Failed, ErrClass: health.ErrNetwork, LastActive: r.now}}
	r.e.Tick()
	r.running = nil
	r.e.Tick()
	got = []bool{r.e.On(dash), r.e.On(user), r.e.On("task:a"), r.e.On(failing)}
	if !slices.Equal(got, []bool{false, true, true, true}) {
		t.Errorf("after the workflows: On = %v, want [false true true true]", got)
	}
	r.now = r.now.Add(Backoff[0])
	r.e.Tick() // the retry goes out
	r.e.Tick()
	if len(r.sent) != 1 || r.e.On(failing) {
		t.Errorf("after its retry: sent %q, on %v", r.sent, r.e.On(failing))
	}
	if got := r.custodyEvents(dash); len(got) != 2 || !strings.Contains(got[1], `"event":"off"`) {
		t.Errorf("journal = %v", got)
	}
}

// Viewing holds per viewer: one of two tabs leaving keeps the user at the
// session, a report lapses after ViewFor, and a closed connection is
// forgotten everywhere.
func TestViewers(t *testing.T) {
	r := newRig(t)
	r.dispatch("task:a")
	r.e.SetViewing("task:a", "tab1", true)
	r.e.SetViewing("task:a", "tab2", true)
	r.e.SetViewing("task:a", "tab1", false)
	if _, err := r.e.Deliver("task:a", ReasonPlanner, "x"); !errors.Is(err, ErrNearby) {
		t.Fatalf("tab2 still viewing: %v", err)
	}
	r.now = r.now.Add(ViewFor - time.Second)
	r.e.SetViewing("task:a", "tab2", true) // renewed
	r.now = r.now.Add(ViewFor - time.Second)
	if r.e.Allowed("task:a") {
		t.Fatal("a renewed report lapsed")
	}
	r.now = r.now.Add(time.Second)
	if !r.e.Allowed("task:a") {
		t.Fatal("a report older than ViewFor still holds")
	}
	r.e.SetViewing("task:a", "tab3", true)
	r.e.DropViewer("tab3")
	if n, err := r.e.Deliver("task:a", ReasonPlanner, "x"); err != nil || n != 1 {
		t.Errorf("after the connection closed: n=%d err=%v", n, err)
	}
}

// A failure custody took is left to the user, as the failure it is, as soon
// as custody gives it up: turned off, paused, a retry that failed again with
// a class it does not retry (no health transition), or a lineage that would
// stop at the retry for want of progress.
func TestRetryLeftToUser(t *testing.T) {
	failed := func(key string, class health.ErrorClass, since, active time.Time) health.Unit {
		return health.Unit{ID: key, Kind: health.KindSession, State: health.Failed, ErrClass: class, Since: since, LastActive: active}
	}
	for _, tc := range []struct {
		name string
		then func(r *rig)
		why  string
		sent int
	}{
		{"off by the user", func(r *rig) { r.e.Set("task:a", false, ByUser) }, "托管已关", 0},
		{"off by the Planner", func(r *rig) { r.e.Set("task:a", false, ByPlanner) }, "托管已关", 0},
		{"paused", func(r *rig) { r.e.SetPaused(true) }, "托管已暂停", 0},
		{"failed again, not retryable", func(r *rig) {
			r.now = r.now.Add(time.Minute)
			r.e.Tick() // the retry goes out
			r.units = []health.Unit{failed("task:a", health.ErrAuth, r.units[0].Since, r.now)}
		}, "凭证 / 权限拒绝", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.dispatch("task:a")
			r.units = []health.Unit{failed("task:a", health.ErrNetwork, r.now, r.now)}
			if !r.e.Covers(r.units[0]) {
				t.Fatal("not covered")
			}
			r.e.Tick()
			tc.then(r)
			for range 3 {
				r.e.Tick()
				r.now = r.now.Add(10 * time.Minute)
			}
			if len(r.sent) != tc.sent {
				t.Errorf("sent %q, want %d", r.sent, tc.sent)
			}
			it := r.inbox.items
			if len(it) != 1 || it[0].Kind != notify.KindError || it[0].Source != "failed:task:a" || !strings.Contains(it[0].Why, tc.why) {
				t.Fatalf("inbox = %+v, want one failure saying %q", it, tc.why)
			}
		})
	}
	t.Run("no progress", func(t *testing.T) {
		r := newRig(t)
		r.dispatch("task:a")
		r.progress["task:a"] = "same"
		for i := range MaxIdle {
			r.units = []health.Unit{failed("task:a", health.ErrNetwork, r.now, r.now)}
			if !r.e.Covers(r.units[0]) {
				t.Fatalf("failure %d not covered", i+1)
			}
			r.e.Tick()
			r.now = r.now.Add(Backoff[i])
			r.e.Tick()
			r.now = r.now.Add(time.Minute)
		}
		u := failed("task:a", health.ErrNetwork, r.now, r.now)
		r.units = []health.Unit{u}
		if r.e.Covers(u) {
			t.Error("a failure the lineage would stop at is covered")
		}
		r.e.Tick() // stops now, not after an 8-minute backoff
		if len(r.sent) != MaxIdle {
			t.Errorf("sent %d, want %d", len(r.sent), MaxIdle)
		}
		it := r.inbox.items
		if len(it) != 1 || it[0].Kind != notify.KindError || it[0].Source != "failed:task:a" ||
			!strings.Contains(it[0].Why, "连续 3 次没有进展（最近一次出错：网络 / DNS）") {
			t.Errorf("inbox = %+v", it)
		}
		if st := r.e.Status("task:a"); st.State != StateStopped {
			t.Errorf("status = %+v", st)
		}
	})
}

// With the Planner off no session is in custody, a dispatched one included,
// and none can be put in it.
func TestDisabled(t *testing.T) {
	r := newRig(t)
	r.e.cfg.Disabled = true
	r.dispatch("task:a")
	r.running = []string{dash}
	u := health.Unit{ID: "task:a", Kind: health.KindSession, State: health.Failed, ErrClass: health.ErrNetwork, LastActive: r.now}
	r.units = []health.Unit{u}
	r.e.Tick()
	r.now = r.now.Add(time.Hour)
	r.e.Tick()
	if r.e.On("task:a") || r.e.On(dash) || r.e.Covers(u) || len(r.sent) != 0 {
		t.Errorf("custody ran: on=%v/%v covers=%v sent=%v", r.e.On("task:a"), r.e.On(dash), r.e.Covers(u), r.sent)
	}
	if err := r.e.Set(dash, true, ByUser); !errors.Is(err, ErrDisabled) {
		t.Errorf("Set = %v", err)
	}
}

func TestIrreversible(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"要 push 到远端吗？", true},
		{"Merge the PR (Recommended)", true},
		{"合并到 master", true},
		{"现在发版吗", true},
		{"打 tag v1.2.0", true},
		{"Deploy to prod", true},
		{"部署到测试环境", true},
		{"给客户发邮件", true},
		{"删除远程分支 feat/x", true},
		{"去支付", true},
		{"先跑测试（推荐）", false},
		{"改用表驱动测试", false},
	} {
		if got := Irreversible(tc.text); got != tc.want {
			t.Errorf("Irreversible(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}
