package notify

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/monitor/beat"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/workspace"
)

type fakeClock struct {
	mu   sync.Mutex
	now  time.Time
	mono time.Duration
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Mono() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.mono }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now, c.mono = c.now.Add(d), c.mono+d
	c.mu.Unlock()
}

type fakeBrowser struct {
	mu     sync.Mutex
	pushes []Push
	count  Counts
}

func (b *fakeBrowser) Notify(p Push)       { b.mu.Lock(); b.pushes = append(b.pushes, p); b.mu.Unlock() }
func (b *fakeBrowser) InboxCount(c Counts) { b.mu.Lock(); b.count = c; b.mu.Unlock() }
func (b *fakeBrowser) titles() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, p := range b.pushes {
		out = append(out, p.Title)
	}
	return out
}

type fakeHealth struct{ snap health.Snapshot }

func (h *fakeHealth) Snapshot() health.Snapshot                    { return h.snap }
func (h *fakeHealth) Subscribe(func(health.Event)) (cancel func()) { return func() {} }

// local builds a time in the zone the push rules read.
func local(h, m int) time.Time { return time.Date(2026, 11, 4, h, m, 0, 0, time.Local) }

func newTest(t *testing.T, at time.Time, lay datadir.Layout) (*Notifier, *fakeClock, *fakeBrowser) {
	t.Helper()
	clock := &fakeClock{now: at}
	n := New(Config{Clock: clock, Layout: lay, System: func(string, string) error { return nil },
		DiskFree: func() (float64, error) { return 1, nil }})
	b := &fakeBrowser{}
	n.SetBrowser(b)
	return n, clock, b
}

func TestAdmit(t *testing.T) {
	cases := []struct {
		name   string
		at     time.Time
		level  Level
		paused bool
		pushed int // timely pushes already sent today
		want   bool
		reason string
	}{
		{"timely by day", local(10, 0), LevelTimely, false, 0, true, ""},
		{"urgent by day", local(10, 0), LevelUrgent, false, 0, true, ""},
		{"timely at quiet start", local(22, 30), LevelTimely, false, 0, false, "quiet_hours"},
		{"timely before quiet start", local(22, 29), LevelTimely, false, 0, true, ""},
		{"timely before quiet end", local(7, 59), LevelTimely, false, 0, false, "quiet_hours"},
		{"timely at quiet end", local(8, 0), LevelTimely, false, 0, true, ""},
		{"urgent in quiet hours", local(23, 0), LevelUrgent, false, 0, true, ""},
		{"timely under cap", local(10, 0), LevelTimely, false, DailyCap - 1, true, ""},
		{"timely at cap", local(10, 0), LevelTimely, false, DailyCap, false, "daily_cap"},
		{"urgent at cap", local(10, 0), LevelUrgent, false, DailyCap, true, ""},
		{"timely paused", local(10, 0), LevelTimely, true, 0, false, "paused"},
		{"urgent paused", local(10, 0), LevelUrgent, true, 0, false, "paused"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, _, _ := newTest(t, c.at, datadir.Layout{})
			n.st.Day, n.st.Pushed = c.at.Format(time.DateOnly), c.pushed
			if c.paused {
				n.st.PausedUntil = c.at.Add(time.Hour)
			}
			got, reason := n.admitLocked(c.level, false, c.at)
			if got != c.want || reason != c.reason {
				t.Errorf("admit = %v %q, want %v %q", got, reason, c.want, c.reason)
			}
		})
	}
}

func TestDailyCap(t *testing.T) {
	n, clock, b := newTest(t, local(9, 0), datadir.Layout{})
	for i := range DailyCap + 5 {
		n.Add(Item{Kind: KindNotice, Source: "s" + string(rune('a'+i)), Title: "timely"})
	}
	n.Add(Item{Kind: KindError, Level: LevelUrgent, Source: "u", Title: "urgent"})
	if got := len(b.titles()); got != DailyCap+1 {
		t.Fatalf("pushed %d, want the cap of %d timely plus the urgent one", got, DailyCap)
	}
	if open, _ := n.List(0); len(open) != DailyCap+6 {
		t.Errorf("open = %d, want every item kept in the inbox", len(open))
	}
	clock.advance(24 * time.Hour)
	n.Add(Item{Kind: KindNotice, Source: "next day", Title: "tomorrow"})
	if titles := b.titles(); titles[len(titles)-1] != "tomorrow" {
		t.Errorf("the cap did not reset the next day: last push %q", titles[len(titles)-1])
	}
}

func TestPause(t *testing.T) {
	lay := datadir.FromRoot(t.TempDir())
	if err := datadir.EnsureDir(lay.PlannerRoot()); err != nil {
		t.Fatal(err)
	}
	n, clock, b := newTest(t, local(10, 0), lay)
	n.Pause(EndOfToday(clock.Now()))
	n.Add(Item{Kind: KindError, Level: LevelUrgent, Source: "a", Title: "held"})
	if len(b.pushes) != 0 {
		t.Fatalf("pushed while paused: %v", b.titles())
	}
	// The pause survives a restart; this one also finds the marker the
	// first left, since it was not closed.
	n2, _, b2 := newTest(t, local(15, 0), lay)
	if !n2.crashed {
		t.Error("an unclosed run marker was not taken for an abnormal exit")
	}
	if got := n2.PausedUntil(); !got.Equal(local(24, 0)) {
		t.Errorf("paused until %v after restart, want midnight", got)
	}
	n2.Add(Item{Kind: KindNotice, Source: "b", Title: "still held"})
	if len(b2.pushes) != 0 {
		t.Fatalf("pushed while paused: %v", b2.titles())
	}
	n2.Close()
	n3, _, b3 := newTest(t, local(10, 0).Add(24*time.Hour), lay)
	if n3.crashed {
		t.Error("a clean Close was taken for an abnormal exit")
	}
	n3.Add(Item{Kind: KindNotice, Source: "c", Title: "after"})
	if got := b3.titles(); !slices.Equal(got, []string{"after"}) {
		t.Errorf("pushes after the pause = %v", got)
	}
}

func TestSources(t *testing.T) {
	n, clock, b := newTest(t, local(10, 0), datadir.Layout{})
	free := 0.5
	n.cfg.DiskFree = func() (float64, error) { return free, nil }
	h := &fakeHealth{}
	n.SetHealth(h)
	start := clock.Now()
	unit := func(id string, s health.State, since time.Time) health.Unit {
		return health.Unit{ID: id, Workspace: "ws_1", State: s, Since: since}
	}
	openSources := func() []string {
		open, _ := n.List(0)
		var out []string
		for _, it := range open {
			out = append(out, it.Source)
		}
		slices.Sort(out)
		return out
	}
	check := func(want ...string) {
		t.Helper()
		n.Check()
		if got := openSources(); !slices.Equal(got, want) {
			t.Fatalf("open = %v, want %v", got, want)
		}
	}

	// Before the first reconcile nothing is judged.
	check()
	h.snap = health.Snapshot{At: clock.Now(), Units: []health.Unit{
		unit("ask", health.Waiting, start),
		unit("wf", health.Stalled, start),
		{ID: "unowned", State: health.Stalled, Since: start},
	}}
	check("stalled:wf")
	clock.advance(AskAfter)
	h.snap.At = clock.Now()
	check("stalled:wf", "waiting:ask")

	// Unknown (just restarted) keeps the items; a known other state or a
	// unit gone closes them.
	h.snap.Units = []health.Unit{unit("ask", health.Unknown, clock.Now()), unit("wf", health.Unknown, clock.Now())}
	check("stalled:wf", "waiting:ask")
	h.snap.Units = []health.Unit{unit("ask", health.Idle, clock.Now())}
	check()
	_, closed := n.List(10)
	if len(closed) != 2 || closed[0].ClosedBy != ClosedByAuto {
		t.Fatalf("closed = %+v, want both closed automatically", closed)
	}

	// A user-closed auto item stays closed while its condition holds, and
	// comes back once it cleared and returned.
	free = 0.05
	check("disk")
	open, _ := n.List(0)
	if err := n.Dismiss(open[0].ID); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	check()
	re := New(Config{Clock: clock, DiskFree: n.cfg.DiskFree, System: func(string, string) error { return nil }})
	re.items = slices.Clone(n.items)
	re.seedDismissed()
	if re.Check(); len(re.items) != len(n.items) {
		t.Fatal("a restart reopened the dismissed disk item while the disk is still low")
	}
	free = 0.5
	clock.advance(time.Minute)
	check()
	free = 0.05
	clock.advance(time.Minute)
	check("disk")
	free = 0.5
	check()

	// Stale loops are one urgent item naming them all, closed once every
	// one beats again; notify's own is the health loop's to report.
	beats := beat.New(clock)
	n.cfg.Beats = beats
	for _, name := range []string{"health", "cron", loopName} {
		beats.Register(name, time.Minute)
	}
	clock.advance(beat.StartupGrace)
	h.snap.At = clock.Now()
	b.pushes = nil
	check(beatSource)
	if got := b.titles(); !slices.Equal(got, []string{"关键循环停了：cron、health"}) {
		t.Errorf("pushes = %v, want one naming both loops", got)
	}
	beats.Register("health", time.Minute)
	check(beatSource)
	beats.Register("cron", time.Minute)
	check()

	// Loops stalling one after another: the open item is retitled without a
	// push, and a loop stalling after the user closed it opens it again.
	beats.Register("relay", time.Minute)
	clock.advance(90 * time.Second)
	beats.Register("health", time.Minute)
	beats.Register("relay", time.Minute)
	clock.advance(40 * time.Second)
	b.pushes = nil
	beats.Register("relay", time.Minute)
	check(beatSource)
	clock.advance(90 * time.Second)
	check(beatSource)
	open, _ = n.List(0)
	if open[0].Title != "关键循环停了：cron、health" {
		t.Errorf("title = %q, want both loops", open[0].Title)
	}
	if err := n.Dismiss(open[0].ID); err != nil {
		t.Fatal(err)
	}
	check()
	clock.advance(40 * time.Second)
	check(beatSource)
	if got := b.titles(); !slices.Equal(got, []string{"关键循环停了：cron", "关键循环停了：cron、health、relay"}) {
		t.Errorf("pushes = %v, want the first loop, then all three once the closed item missed one", got)
	}
	for _, name := range []string{"health", "cron", "relay"} {
		beats.Register(name, time.Minute)
	}
	check()

	// Failures: one unit is its own error; units failing together are one
	// systemic fault, and their transitions add nothing.
	now := clock.Now()
	b.pushes = nil
	n.HandleHealth(health.Event{At: now, Transition: &health.Transition{From: health.Idle,
		Unit: health.Unit{ID: "a", Workspace: "ws_1", State: health.Failed, ErrClass: health.ErrOther, Since: now}}})
	for _, id := range []string{"b", "c"} {
		n.HandleHealth(health.Event{At: now, Transition: &health.Transition{From: health.Idle, InFault: true,
			Unit: health.Unit{ID: id, Workspace: "ws_1", State: health.Failed, ErrClass: health.ErrAuth, Since: now}}})
	}
	n.HandleHealth(health.Event{At: now, Fault: &health.Fault{Class: health.ErrAuth, Units: []string{"b", "c"}}})
	check("failed:a", "systemic:auth")
	if got := b.titles(); !slices.Equal(got, []string{"a 失败", "系统性故障：凭证 / 权限拒绝"}) {
		t.Errorf("pushes = %v", got)
	}
	// A failure of no named class says nothing beyond its title.
	if len(b.pushes) > 0 && b.pushes[0].Body != "" {
		t.Errorf("unclassified failure body = %q, want none", b.pushes[0].Body)
	}
	if b.count != (Counts{Open: 2, Badge: 2}) {
		t.Errorf("inbox counts = %+v, want 2 open errors, both on the badge", b.count)
	}
	// The fault resolved closes its item; the unit's failure stays.
	n.HandleHealth(health.Event{At: now, Fault: &health.Fault{Class: health.ErrAuth, Resolved: true}})
	check("failed:a")
}

// A failed unit's reason is its error class, or none when the class says
// nothing: never "失败 · 其他".
func TestFailedWhy(t *testing.T) {
	for _, c := range []struct {
		class health.ErrorClass
		want  string
	}{
		{health.ErrNone, ""},
		{health.ErrOther, ""},
		{health.ErrAuth, "凭证 / 权限拒绝"},
		{health.ErrRateLimit, "限流"},
	} {
		if got := failedWhy(c.class); got != c.want {
			t.Errorf("failedWhy(%q) = %q, want %q", c.class, got, c.want)
		}
	}
}

// TestRestartKeepsQuestion: a restart dates a session's wait anew; its open
// question stays open, one the user closed stays closed, and neither is
// pushed again.
func TestRestartKeepsQuestion(t *testing.T) {
	lay := datadir.FromRoot(t.TempDir())
	if err := datadir.EnsureDir(lay.PlannerRoot()); err != nil {
		t.Fatal(err)
	}
	n, clock, b := newTest(t, local(10, 0), lay)
	h := &fakeHealth{}
	n.SetHealth(h)
	waiting := func(id string) health.Unit {
		return health.Unit{ID: id, Workspace: "ws_1", State: health.Waiting, Since: clock.Now()}
	}
	h.snap = health.Snapshot{At: clock.Now(), Units: []health.Unit{waiting("a"), waiting("b")}}
	n.Check()
	clock.advance(AskAfter)
	h.snap.At = clock.Now()
	n.Check()
	open, _ := n.List(0)
	if len(open) != 2 || b.count != (Counts{Open: 2, Badge: 2}) {
		t.Fatalf("open = %d, counts %+v; want both questions, both on the badge", len(open), b.count)
	}
	for _, it := range open {
		if it.Source == "waiting:b" {
			if err := n.Dismiss(it.ID); err != nil {
				t.Fatal(err)
			}
		}
	}

	n2, _, b2 := newTest(t, clock.Now(), lay)
	n2.SetHealth(h)
	h.snap = health.Snapshot{At: clock.Now(), Units: []health.Unit{waiting("a"), waiting("b")}}
	for range 2 {
		n2.Check()
		clock.advance(AskAfter)
		h.snap.At = clock.Now()
	}
	open, closed := n2.List(10)
	if len(open) != 1 || open[0].Source != "waiting:a" || len(closed) != 1 || closed[0].Source != "waiting:b" {
		t.Fatalf("after restart open = %+v, closed = %+v", open, closed)
	}
	if len(b2.pushes) != 0 {
		t.Errorf("pushed again after restart: %v", b2.titles())
	}
}

// TestAutoCloseWithFullHistory: closing several items in one check while
// the closed history is at keepClosed prunes it without losing an item.
func TestAutoCloseWithFullHistory(t *testing.T) {
	n, clock, _ := newTest(t, local(10, 0), datadir.Layout{})
	h := &fakeHealth{}
	n.SetHealth(h)
	for i := range keepClosed {
		n.items = append(n.items, Item{ID: "old" + strconv.Itoa(i), Source: "x", State: StateClosed, ClosedAt: clock.Now()})
	}
	for _, id := range []string{"a", "b"} {
		n.items = append(n.items, Item{ID: id, Source: "waiting:" + id, State: StateOpen, AutoClose: true})
	}
	h.snap = health.Snapshot{At: clock.Now()}
	n.Check()
	open, closed := n.List(2 * keepClosed)
	if len(open) != 0 || len(closed) != keepClosed {
		t.Fatalf("open = %d, closed = %d; want 0 and %d", len(open), len(closed), keepClosed)
	}
}

func TestDecideOnce(t *testing.T) {
	n, _, _ := newTest(t, local(10, 0), datadir.Layout{})
	it, _ := n.Add(Item{Kind: KindDecision, Source: "d", Title: "派活",
		Decision: &Decision{Handler: "h", Actions: []Action{ActionApprove, ActionReject}}})
	entered, release := make(chan struct{}), make(chan struct{})
	var applied []Action
	n.HandleDecisions("h", func(_ Item, a Action) error {
		applied = append(applied, a)
		if a == ActionApprove {
			close(entered)
			<-release
		}
		return nil
	})
	done := make(chan error)
	go func() { done <- n.Decide(it.ID, ActionApprove) }()
	<-entered
	if err := n.Decide(it.ID, ActionReject); !errors.Is(err, ErrPending) {
		t.Errorf("second answer = %v, want ErrPending", err)
	}
	if err := n.Dismiss(it.ID); !errors.Is(err, ErrPending) {
		t.Errorf("dismiss while deciding = %v, want ErrPending", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := n.Decide(it.ID, ActionReject); !errors.Is(err, ErrClosed) {
		t.Errorf("answer after close = %v, want ErrClosed", err)
	}
	if !slices.Equal(applied, []Action{ActionApprove}) {
		t.Errorf("applied = %v, want only the first answer", applied)
	}
}

func TestLinkFor(t *testing.T) {
	const base = "http://127.0.0.1:8180/dashboard"
	const id = "in_abcdefghij"
	hex := strings.Repeat("0123456789abcdef", 2)
	for _, tc := range []struct {
		name, base string
		p          Push
		want       string
	}{
		{"session", base, Push{ID: id, Unit: "dashboard:direct:alice:general"}, base + "#session=dashboard:direct:alice:general&item=" + id},
		{"dashboard project", base, Push{ID: id, Unit: "dashboard:direct:2026-11-04-101530-naozhi-planner:general"},
			base + "#session=dashboard:direct:2026-11-04-101530-naozhi-planner:general&item=" + id},
		{"dashboard chinese", base, Push{ID: id, Unit: "dashboard:direct:2026-11-04-101530-周报:general"},
			base + "#session=dashboard:direct:2026-11-04-101530-%E5%91%A8%E6%8A%A5:general&item=" + id},
		{"dashboard resume", base, Push{ID: id, Unit: "dashboard:direct:r" + hex + ":general"},
			base + "#session=dashboard:direct:r" + hex + ":general&item=" + id},
		{"feishu", base, Push{ID: id, Unit: "feishu:group:oc_" + hex + ":general"},
			base + "#session=feishu:group:oc_" + hex + ":general&item=" + id},
		{"no session", base, Push{ID: "in_2"}, base + "#planner&item=in_2"},
		{"planner", base, Push{ID: "in_3", Unit: "planner:main"}, base + "#planner&item=in_3"},
		{"no id", base, Push{}, base + "#planner"},
		{"too long", base, Push{ID: "in_4", Unit: strings.Repeat("k", 200)}, base + "#planner&item=in_4"},
		{"base too long", base + "/" + strings.Repeat("b", 200), Push{ID: "in_4"}, base + "/" + strings.Repeat("b", 200)},
		{"no base", "", Push{ID: "in_5", Unit: "dashboard:direct:alice:general"}, ""},
	} {
		if got := linkFor(tc.base, tc.p); got != tc.want {
			t.Errorf("%s: linkFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The system notification's body keeps the whole link under the cap.
func TestDeliverKeepsLink(t *testing.T) {
	var body string
	done := make(chan struct{})
	n := New(Config{Link: "http://127.0.0.1:8180/dashboard", System: func(_, b string) error { body = b; close(done); return nil }})
	n.deliver(Push{ID: "in_1", Unit: "dashboard:direct:alice:general", Title: "t", Body: strings.Repeat("长", 300)})
	<-done
	got := osascriptArgs("t", body)[8]
	if want := linkFor(n.cfg.Link, Push{ID: "in_1", Unit: "dashboard:direct:alice:general"}); !strings.HasSuffix(got, " "+want) {
		t.Errorf("body = %q, want it to end with %q", got, want)
	}
}

func TestOsascriptArgs(t *testing.T) {
	title := `x" & (do shell script "touch /tmp/pwned") & "`
	body := "line1\nline2\x00\u202etail"
	args := osascriptArgs(title, body)
	if n := len(args); n != 9 || args[0] != "-e" || args[6] != "--" || args[7] != title {
		t.Fatalf("args = %q", args)
	}
	for _, a := range args[:7] {
		if strings.Contains(a, "touch") || strings.Contains(a, "line") {
			t.Fatalf("text reached the script: %q", args[:7])
		}
	}
	if got := args[8]; got != "line1 line2  tail" {
		t.Errorf("body = %q, want control and bidi runes blanked", got)
	}
	// A title that reads as an option comes after "--", so osascript takes
	// it as an argument rather than another script line.
	opt := `-eproperty p:(do shell script"id>x")-- 在等你回复`
	if args := osascriptArgs(opt, ""); slices.Index(args, "--") != 6 || args[7] != opt {
		t.Errorf("option-like title args = %q", args)
	}
	if got := osascriptArgs(strings.Repeat("长", 200), "")[7]; len([]rune(got)) > titleRunes+3 {
		t.Errorf("title not capped: %d runes", len([]rune(got)))
	}
}

// An uncapped item (a promise's reminder) is pushed past the daily cap, and
// one the quiet hours or the pause held goes out at the first check after,
// across a restart too.
func TestUncapped(t *testing.T) {
	lay := datadir.FromRoot(t.TempDir())
	if err := datadir.EnsureDir(lay.PlannerRoot()); err != nil {
		t.Fatal(err)
	}
	n, clock, b := newTest(t, local(10, 0), lay)
	n.st.Day, n.st.Pushed = local(10, 0).Format(time.DateOnly), DailyCap
	n.Add(Item{Kind: KindNotice, Source: "capped", Title: "capped"})
	n.Add(Item{Kind: KindNotice, Uncapped: true, Source: "promise:a", Title: "past the cap"})
	if got := b.titles(); !slices.Equal(got, []string{"past the cap"}) {
		t.Fatalf("pushed %q, want only the uncapped item", got)
	}

	n.Pause(clock.Now().Add(time.Hour))
	n.Add(Item{Kind: KindNotice, Uncapped: true, Source: "promise:b", Title: "after the pause"})
	clock.advance(30 * time.Minute)
	n.Check()
	clock.advance(30 * time.Minute)
	n.Check()
	n.Check()
	if got := b.titles(); !slices.Equal(got, []string{"past the cap", "after the pause"}) {
		t.Fatalf("pushed %q, want the held item once the pause ended", got)
	}

	clock.advance(12 * time.Hour) // 23:00, quiet hours
	n.Add(Item{Kind: KindNotice, Uncapped: true, Source: "promise:c", Title: "in the morning"})
	n.Check()
	if len(b.titles()) != 2 {
		t.Fatalf("pushed %q in quiet hours", b.titles())
	}
	n2, clock2, b2 := newTest(t, local(23, 30), lay)
	clock2.advance(8*time.Hour + 30*time.Minute) // 08:00
	n2.Check()
	n2.Check()
	if got := b2.titles(); !slices.Equal(got, []string{"in the morning"}) {
		t.Errorf("after a restart pushed %q, want the held item once at 08:00", got)
	}

	n2.DismissSource("promise:c")
	if open, _ := n2.List(0); slices.ContainsFunc(open, func(it Item) bool { return it.Source == "promise:c" }) {
		t.Error("DismissSource left the item open")
	}
}

// AddDaily opens a source once a day: not again the same day, open or
// closed by the user, and again the next day once the first is closed.
func TestAddDaily(t *testing.T) {
	n, clock, b := newTest(t, local(9, 0), datadir.Layout{})
	steps := []struct {
		advance time.Duration
		close   bool // close the open item first
		opened  bool
	}{
		{opened: true},
		{advance: time.Hour},
		{advance: time.Hour, close: true},
		{advance: 24 * time.Hour, opened: true},
	}
	for i, st := range steps {
		clock.advance(st.advance)
		if st.close {
			open, _ := n.List(0)
			if err := n.Dismiss(open[0].ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, opened := n.AddDaily(Item{Kind: KindNotice, Source: "planner:k", Title: "有人等你回复"}); opened != st.opened {
			t.Errorf("step %d: opened = %v, want %v", i, opened, st.opened)
		}
	}
	if got := b.titles(); len(got) != 2 {
		t.Errorf("pushes = %q, want two", got)
	}
}

// TestFailuresFirstSeenFailing: units already failed when the health machine
// first sees them are dated by their last activity and merge into no fault,
// so each is an item of its own.
func TestFailuresFirstSeenFailing(t *testing.T) {
	lay := datadir.FromRoot(t.TempDir())
	if err := datadir.EnsureDir(lay.PlannerRoot()); err != nil {
		t.Fatal(err)
	}
	n, clock, _ := newTest(t, local(10, 0), lay)
	dir := t.TempDir()
	facts := make([]health.Fact, 0, 2)
	for _, id := range []string{"cron:a", "cron:b"} {
		facts = append(facts, health.Fact{ID: id, Kind: health.KindCron, Dir: dir, Register: true,
			Failed: "last run failed", ErrText: "API Error: 403 permission_error", LastActive: clock.Now().Add(-2 * time.Hour)})
	}
	m := health.New(health.Config{Clock: clock, Workspaces: workspace.Load("", "node"),
		Collect: func(time.Time) []health.Fact { return facts }})
	n.SetHealth(m)
	var events []health.Event
	m.Subscribe(func(ev health.Event) { events = append(events, ev) })
	m.Reconcile()
	clock.advance(time.Minute)
	m.Reconcile()
	for _, ev := range events {
		if ev.Fault != nil {
			t.Errorf("fault %+v, want none", ev.Fault)
		}
		n.HandleHealth(ev)
	}
	open, _ := n.List(0)
	var got []string
	for _, it := range open {
		got = append(got, it.Source)
	}
	slices.Sort(got)
	if want := []string{"failed:cron:a", "failed:cron:b"}; !slices.Equal(got, want) {
		t.Errorf("open items = %q, want %q", got, want)
	}
}

// A failure custody will retry is not the user's yet: it opens no item, the
// others still do.
func TestHandleHealthCustody(t *testing.T) {
	n := New(Config{System: func(string, string) error { return nil }})
	n.SetCustody(func(u health.Unit) bool { return u.ID == "task:a" })
	for _, id := range []string{"task:a", "task:b"} {
		n.HandleHealth(health.Event{Transition: &health.Transition{From: health.Idle,
			Unit: health.Unit{ID: id, Workspace: "ws_1", State: health.Failed, ErrClass: health.ErrNetwork}}})
	}
	open, _ := n.List(0)
	if len(open) != 1 || open[0].Source != "failed:task:b" {
		t.Errorf("open = %+v, want task:b's failure only", open)
	}
}
