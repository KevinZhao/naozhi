package task

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
)

var t0 = time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

func TestSessionState(t *testing.T) {
	// Health entered its state at t0; a mark set at t0+1h is newer, one set
	// at t0-1h older.
	mark := func(s State, by By, d time.Duration) *sidecar.Mark {
		return &sidecar.Mark{State: string(s), By: string(by), Why: "why", At: t0.Add(d)}
	}
	failure := &sidecar.Mark{State: string(Failed), By: string(ByHealth), Why: "exit 1", At: t0.Add(-2 * time.Hour)}
	retired := t0.Add(-time.Hour)
	cases := []struct {
		name    string
		mark    *sidecar.Mark
		failure *sidecar.Mark
		retired time.Time
		health  health.State
		want    State
		by      By
	}{
		{"new", nil, nil, time.Time{}, health.Unknown, Doing, ""},
		{"running", nil, nil, time.Time{}, health.Active, Doing, ""},
		{"asking", nil, nil, time.Time{}, health.Waiting, Waiting, ByHealth},
		{"failed", nil, nil, time.Time{}, health.Failed, Failed, ByHealth},
		{"stalled is still doing", nil, nil, time.Time{}, health.Stalled, Doing, ""},
		{"recovered", nil, failure, time.Time{}, health.Idle, Doing, ""},
		{"done by the Planner holds over health", mark(Done, ByPlanner, time.Hour), nil, time.Time{}, health.Waiting, Done, ByPlanner},
		{"failed by the Planner", mark(Failed, ByPlanner, time.Hour), nil, time.Time{}, health.Idle, Failed, ByPlanner},
		{"reopened after it failed", mark(Doing, ByUser, time.Hour), nil, time.Time{}, health.Failed, Doing, ByUser},
		{"failed again after the reopen", mark(Doing, ByUser, -time.Hour), nil, time.Time{}, health.Failed, Failed, ByHealth},
		{"reopened", mark(Doing, ByUser, -time.Hour), nil, time.Time{}, health.Idle, Doing, ByUser},
		{"retired", nil, nil, retired, health.Unknown, Done, ByEnded},
		{"retired after it failed", nil, failure, retired, health.Unknown, Failed, ByHealth},
		{"retired, reopened before", mark(Doing, ByUser, -2*time.Hour), nil, retired, health.Unknown, Done, ByEnded},
		{"retired, reopened since", mark(Doing, ByUser, time.Hour), nil, retired, health.Unknown, Doing, ByUser},
		{"retired, done by the Planner", mark(Done, ByPlanner, -2*time.Hour), failure, retired, health.Unknown, Done, ByPlanner},
	}
	for _, c := range cases {
		m := sidecar.Meta{Created: t0.Add(-3 * time.Hour), Mark: c.mark, Failure: c.failure, Retired: c.retired}
		got, by, _, _ := sessionState(m, health.Unit{State: c.health, Since: t0})
		if got != c.want || by != c.by {
			t.Errorf("%s: %s by %q, want %s by %q", c.name, got, by, c.want, c.by)
		}
	}
}

func TestPromiseState(t *testing.T) {
	cases := []struct {
		name string
		p    promise.Promise
		want State
		by   By
	}{
		{"before due", promise.Promise{State: promise.StateOpen, Due: t0.Add(time.Minute)}, Remind, ""},
		{"past due, not reminded yet", promise.Promise{State: promise.StateOpen, Due: t0.Add(-time.Minute)}, Doing, ""},
		{"reminded, condition open", promise.Promise{State: promise.StateOpen, Due: t0.Add(time.Minute), RemindedAt: t0}, Doing, ""},
		{"undated", promise.Promise{State: promise.StateOpen, CloseWhen: "CI 绿"}, Doing, ""},
		{"auto done", promise.Promise{State: promise.StateClosed, ClosedBy: promise.SourceAuto, ClosedAt: t0}, Done, ByAuto},
		{"closed by the Planner", promise.Promise{State: promise.StateClosed, ClosedBy: promise.SourcePlanner, ClosedAt: t0}, Done, ByPlanner},
		{"closed by an older naozhi", promise.Promise{State: promise.StateClosed, ClosedAt: t0}, Done, ByUser},
	}
	for _, c := range cases {
		got, by, _, _ := promiseState(c.p, t0)
		if got != c.want || by != c.by {
			t.Errorf("%s: %s by %q, want %s by %q", c.name, got, by, c.want, c.by)
		}
	}
}

func TestArchived(t *testing.T) {
	cases := []struct {
		state State
		age   time.Duration
		want  bool
	}{
		{Done, Archive - time.Second, false},
		{Done, Archive, true},
		{Doing, 30 * Archive, false},
		{Failed, 30 * Archive, false},
	}
	for _, c := range cases {
		if got := archived(Task{State: c.state, At: t0.Add(-c.age)}, t0); got != c.want {
			t.Errorf("%s %v old: archived = %v, want %v", c.state, c.age, got, c.want)
		}
	}
}

// The board lists both sources by section, sets states by who and why (a
// promise is never failed, an unknown ID is refused), journals each and
// archives a done task a day later.
func TestBoard(t *testing.T) {
	now := t0
	clock := func() time.Time { return now }
	side := sidecar.Open(t.TempDir(), clock)
	for _, m := range []sidecar.Meta{
		{Key: "task:a", Purpose: sidecar.PurposeTask, Title: "iso 写 hello", Created: t0.Add(-time.Hour)},
		{Key: "task:b", Purpose: sidecar.PurposeTask, Title: "iso 跑测试", Created: t0.Add(-2 * time.Hour)},
		{Key: "dashboard:direct:x:general", Purpose: "other"},
	} {
		if err := side.Put(m); err != nil {
			t.Fatal(err)
		}
	}
	proms := promise.New(promise.Config{Now: clock})
	// Distinct creation times keep the order of the two in progress fixed.
	now = t0.Add(-time.Minute)
	later, _ := proms.Add(promise.Promise{Text: "提醒用户明早交周报", Due: t0.Add(time.Hour)})
	now = t0
	cond, _ := proms.Add(promise.Promise{Text: "CI 绿了告诉你", CloseWhen: "master CI 绿"})
	j, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := New(Config{Sessions: side, Promises: proms, Journal: j, Now: clock,
		Health: func() health.Snapshot {
			return health.Snapshot{Units: []health.Unit{{ID: "task:b", State: health.Waiting, Since: t0}}}
		}})

	ids := func() []string {
		var out []string
		for _, t := range b.List() {
			out = append(out, t.ID+"="+string(t.State))
		}
		return out
	}
	if got, want := ids(), []string{"task:b=waiting", cond.ID + "=doing", "task:a=doing", later.ID + "=remind"}; !slices.Equal(got, want) {
		t.Fatalf("list %q, want %q", got, want)
	}

	sets := []struct {
		id    string
		state State
		by    By
		want  error
	}{
		{"task:a", Done, ByPlanner, nil},
		{cond.ID, Done, ByUser, nil},
		{cond.ID, Done, ByPlanner, nil}, // done already: no change
		{later.ID, Failed, ByPlanner, ErrState},
		{"task:nope", Done, ByPlanner, ErrNotFound},
		{"dashboard:direct:x:general", Done, ByPlanner, ErrNotFound},
		{"pr_nope", Done, ByPlanner, ErrNotFound},
		{"task:a", Remind, ByUser, ErrState},
	}
	for _, s := range sets {
		if err := b.Set(s.id, s.state, s.by, "  看过\n汇报 "); !errors.Is(err, s.want) {
			t.Errorf("Set(%s, %s) = %v, want %v", s.id, s.state, err, s.want)
		}
	}
	list := b.List()
	if got, want := ids(), []string{"task:b=waiting", later.ID + "=remind", cond.ID + "=done", "task:a=done"}; !slices.Equal(got, want) {
		t.Fatalf("after sets: %q, want %q", got, want)
	}
	if a := list[3]; a.By != ByPlanner || a.Why != "看过 汇报" || !a.At.Equal(t0) {
		t.Errorf("task:a = %+v", a)
	}
	if c := list[2]; c.By != ByUser {
		t.Errorf("promise = %+v", c)
	}
	if l := list[1]; l.Title != "明早交周报" {
		t.Errorf("promise title = %q, want the lead dropped", l.Title)
	}
	entries, _ := j.Read(t0.Add(-time.Minute), t0.Add(time.Minute))
	var logged []string
	for _, e := range entries {
		if strings.Contains(string(e.Data), `"task_state"`) {
			logged = append(logged, e.Unit)
		}
	}
	if want := []string{"task:a", "promise:" + cond.ID}; !slices.Equal(logged, want) {
		t.Errorf("journaled %q, want %q", logged, want)
	}

	// Reopened, the promise is in progress again; a day after it was done
	// the session leaves the list, its sidecar kept.
	if err := b.Set(cond.ID, Doing, ByUser, ""); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(Archive)
	if got, want := ids(), []string{"task:b=waiting", cond.ID + "=doing", later.ID + "=doing"}; !slices.Equal(got, want) {
		t.Errorf("a day later: %q, want %q", got, want)
	}
	if _, ok := side.Get("task:a"); !ok {
		t.Error("archived task's sidecar dropped")
	}

	// task:b fails and then leaves the router: it stays failed once
	// retired, with health's evidence.
	b.Observe(health.Event{Transition: &health.Transition{From: health.Waiting,
		Unit: health.Unit{ID: "task:b", State: health.Failed, Evidence: "exit 1", Since: now}}})
	b.cfg.Health = func() health.Snapshot { return health.Snapshot{} }
	side.Sweep(func(string) bool { return false })
	if l := b.List(); len(l) == 0 || l[0].ID != "task:b" || l[0].State != Failed || l[0].Why != "exit 1" || l[0].Retired.IsZero() {
		t.Errorf("retired after it failed: %+v", l)
	}
}

// Stop marks a dispatched session 已完成 as the user's (D91), closes the open
// promises citing it (its key in the text or the close condition) as theirs
// and stops its turn, naming those promises; anything else is not a session
// to stop and closes nothing.
func TestStop(t *testing.T) {
	cases := []struct {
		name   string
		id     string // "{P}" is the unrelated promise's ID
		want   error
		closed []string // the promises the stop closes, by name
	}{
		{"citing promises closed", "task:a", nil, []string{"text", "close_when"}},
		{"no such task", "task:nope", ErrNotFound, nil},
		{"a promise", "{P}", ErrNotFound, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			side := sidecar.Open(t.TempDir(), func() time.Time { return t0 })
			if err := side.Put(sidecar.Meta{Key: "task:a", Purpose: sidecar.PurposeTask, Title: "iso sleep"}); err != nil {
				t.Fatal(err)
			}
			proms := promise.New(promise.Config{Now: func() time.Time { return t0 }})
			byName := map[string]promise.Promise{}
			for name, p := range map[string]promise.Promise{
				"text":       {Text: "task:a 做完后告诉用户结果"},
				"close_when": {Text: "告诉用户 sleep 的结果", CloseWhen: "task:a 汇报做完"},
				"unrelated":  {Text: "明早提醒你", Due: t0.Add(time.Hour)},
				"closed":     {Text: "task:a 开始后告诉用户"},
			} {
				byName[name], _ = proms.Add(p)
			}
			if err := proms.Close(byName["closed"].ID, promise.SourcePlanner, "已告诉"); err != nil {
				t.Fatal(err)
			}
			var stopped []string
			b := New(Config{Sessions: side, Promises: proms, Now: func() time.Time { return t0 },
				Stop: func(key, title string, closed []string) {
					stopped = append(stopped, key+"="+title+" "+strings.Join(closed, ","))
				}})

			if err := b.Stop(strings.ReplaceAll(c.id, "{P}", byName["unrelated"].ID)); !errors.Is(err, c.want) {
				t.Fatalf("Stop(%s) = %v, want %v", c.id, err, c.want)
			}
			var ids []string
			for name := range byName {
				p, _ := proms.Get(byName[name].ID)
				done := slices.Contains(c.closed, name)
				if done != (p.State == promise.StateClosed && p.ClosedBy == promise.SourceUser) {
					t.Errorf("promise %s = %+v, want closed by the stop %v", name, p, done)
				}
				if done {
					ids = append(ids, p.ID)
					if p.Why != StoppedPromiseWhy {
						t.Errorf("promise %s why = %q", name, p.Why)
					}
				}
			}
			if c.want != nil {
				if len(stopped) != 0 {
					t.Errorf("stopped %q", stopped)
				}
				return
			}
			if len(stopped) != 1 || !strings.HasPrefix(stopped[0], "task:a=iso sleep ") {
				t.Fatalf("stopped %q", stopped)
			}
			got := strings.Split(strings.TrimPrefix(stopped[0], "task:a=iso sleep "), ",")
			slices.Sort(got)
			if slices.Sort(ids); !slices.Equal(got, ids) {
				t.Errorf("stop named %q, want %q", got, ids)
			}
			l := b.List()
			if i := slices.IndexFunc(l, func(t Task) bool { return t.ID == "task:a" }); i < 0 || l[i].State != Done || l[i].By != ByUser || l[i].Why != StoppedWhy {
				t.Errorf("list = %+v, want task:a done as the user's", l)
			}
		})
	}
}

// A state set on a dispatched session is told to OnState once saved (the
// reaper's trigger), a promise's is not; a done one not put away yet lists
// why, one put away lists its reap.
func TestOnStateAndReap(t *testing.T) {
	now := t0
	sc := sidecar.Open(t.TempDir(), func() time.Time { return now })
	const key = "task:aaaaaaaaaaaa"
	if err := sc.Put(sidecar.Meta{Key: key, Purpose: sidecar.PurposeTask, Title: "t"}); err != nil {
		t.Fatal(err)
	}
	var told []string
	b := New(Config{Sessions: sc, Now: func() time.Time { return now },
		OnState:  func(k string, s State) { told = append(told, k+" "+string(s)) },
		ReapWait: func(string) string { return "有提问没回答" }})
	if err := b.Set(key, Done, ByPlanner, "完成"); err != nil {
		t.Fatal(err)
	}
	if err := b.Set(key, Doing, ByUser, ""); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(told, []string{key + " done", key + " doing"}) {
		t.Fatalf("OnState = %v", told)
	}
	if err := b.Set(key, Done, ByUser, ""); err != nil {
		t.Fatal(err)
	}
	if l := b.List(); len(l) != 1 || l[0].ReapWait != "有提问没回答" || l[0].Reap != nil {
		t.Fatalf("List = %+v", l)
	}
	if _, err := sc.Update(key, func(m *sidecar.Meta) bool { m.Reap = &sidecar.Reap{At: now, SessionID: "s"}; return true }); err != nil {
		t.Fatal(err)
	}
	if l := b.List(); len(l) != 1 || l[0].ReapWait != "" || l[0].Reap == nil || l[0].Reap.SessionID != "s" {
		t.Fatalf("List = %+v", l)
	}
}

// A dispatched session in the router lists what it is doing now, each text
// folded to one capped line, and when it entered its health state; one gone
// from the router, or retired, lists none.
func TestLive(t *testing.T) {
	sc := sidecar.Open(t.TempDir(), func() time.Time { return t0 })
	const run, gone, retired = "task:aaaaaaaaaaaa", "task:bbbbbbbbbbbb", "task:cccccccccccc"
	for _, k := range []string{run, gone, retired} {
		m := sidecar.Meta{Key: k, Purpose: sidecar.PurposeTask, Title: k}
		if k == retired {
			m.Retired = t0
		}
		if err := sc.Put(m); err != nil {
			t.Fatal(err)
		}
	}
	long := strings.Repeat("长", liveRunes+50)
	b := New(Config{Sessions: sc, Now: func() time.Time { return t0 },
		Health: func() health.Snapshot {
			return health.Snapshot{Units: []health.Unit{{ID: run, State: health.Active, Since: t0.Add(-time.Minute)}}}
		},
		Live: func() map[string]Live {
			return map[string]Live{
				run:     {Running: true, Activity: "Bash:\n  go test\t./...", Reply: long, LastActive: t0},
				retired: {Activity: "x"},
			}
		}})
	by := map[string]Task{}
	for _, tk := range b.List() {
		by[tk.ID] = tk
	}
	l := by[run].Live
	if l == nil || !l.Running || l.Activity != "Bash: go test ./..." || !l.LastActive.Equal(t0) {
		t.Fatalf("live = %+v", l)
	}
	if n := len([]rune(l.Reply)); n > liveRunes+3 || !strings.HasSuffix(l.Reply, "...") {
		t.Errorf("reply not capped: %d runes", n)
	}
	if !by[run].HealthSince.Equal(t0.Add(-time.Minute)) {
		t.Errorf("health_since = %v", by[run].HealthSince)
	}
	if by[gone].Live != nil || by[retired].Live != nil {
		t.Errorf("gone %+v, retired %+v: want no live", by[gone].Live, by[retired].Live)
	}
}

// A dispatched session's card carries its latest report from the sidecar
// and whether it has a digest to open.
func TestReportAndDigest(t *testing.T) {
	sc := sidecar.Open(t.TempDir(), func() time.Time { return t0 })
	const reported, plain = "task:aaaaaaaaaaaa", "task:bbbbbbbbbbbb"
	rep := &sidecar.Report{State: "done", Summary: "已修好。CI 绿", At: t0}
	for _, m := range []sidecar.Meta{
		{Key: reported, Purpose: sidecar.PurposeTask, Title: "a", Report: rep},
		{Key: plain, Purpose: sidecar.PurposeTask, Title: "b"},
	} {
		if err := sc.Put(m); err != nil {
			t.Fatal(err)
		}
	}
	b := New(Config{Sessions: sc, Now: func() time.Time { return t0 }, Digest: func(key string) bool { return key == reported }})
	by := map[string]Task{}
	for _, tk := range b.List() {
		by[tk.ID] = tk
	}
	if got := by[reported]; got.Report == nil || *got.Report != *rep || !got.Digest {
		t.Errorf("reported task = %+v", got)
	}
	if got := by[plain]; got.Report != nil || got.Digest {
		t.Errorf("plain task = %+v", got)
	}
	data, err := json.Marshal(by[plain])
	if err != nil {
		t.Fatal(err)
	}
	if s := string(data); strings.Contains(s, `"report"`) || strings.Contains(s, `"digest"`) {
		t.Errorf("plain task wire = %s", s)
	}
}
