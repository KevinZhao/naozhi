package promise

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fakeReminder struct {
	due, closed []string
}

func (r *fakeReminder) Due(p Promise)    { r.due = append(r.due, p.Text) }
func (r *fakeReminder) Closed(p Promise) { r.closed = append(r.closed, p.Text) }

var t0 = time.Date(2026, 11, 4, 10, 0, 0, 0, time.Local)

func newTest(t *testing.T, path string) (*Store, *fakeClock, *fakeReminder) {
	t.Helper()
	clock := &fakeClock{now: t0}
	s := New(Config{Path: path, Now: clock.Now})
	r := &fakeReminder{}
	s.SetReminder(r)
	return s, clock, r
}

func mustAdd(t *testing.T, s *Store, p Promise) Promise {
	t.Helper()
	p, err := s.Add(p)
	if err != nil {
		t.Fatalf("Add(%q): %v", p.Text, err)
	}
	return p
}

func TestDueReminder(t *testing.T) {
	s, clock, r := newTest(t, "")
	mustAdd(t, s, Promise{Text: "later", Due: t0.Add(2 * time.Hour), CloseWhen: "压测跑完"})
	mustAdd(t, s, Promise{Text: "soon", Due: t0.Add(10 * time.Minute), CloseWhen: "你看过 CI"})
	mustAdd(t, s, Promise{Text: "undated", CloseWhen: "CI 绿了"})
	closed := mustAdd(t, s, Promise{Text: "closed first", Due: t0.Add(5 * time.Minute)})
	if err := s.Close(closed.ID, SourceUser, ""); err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		advance time.Duration
		due     []string // reminders so far
	}{
		{0, nil},
		{9 * time.Minute, nil},
		{time.Minute, []string{"soon"}}, // due exactly now
		{time.Minute, []string{"soon"}}, // once only
		{2 * time.Hour, []string{"soon", "later"}},
	}
	for i, st := range steps {
		clock.advance(st.advance)
		s.Check()
		if !slices.Equal(r.due, st.due) {
			t.Errorf("step %d: reminded %q, want %q", i, r.due, st.due)
		}
	}
	open, _ := s.List(10)
	var order []string
	for _, p := range open {
		order = append(order, p.Text)
	}
	if want := []string{"soon", "later", "undated"}; !slices.Equal(order, want) {
		t.Errorf("open order %q, want %q (due first, undated last; a condition keeps them open)", order, want)
	}
}

// A pure reminder (a due time, no condition beyond reminding) is done once
// it reminded, by the gateway, and keeps the reminder's inbox item: the
// reminder is told of no close.
func TestReminderOnlyAutoCloses(t *testing.T) {
	cases := []struct {
		closeWhen string
		due       bool
		done      bool
	}{
		{"", true, true},
		{"已提醒", true, true},
		{"提醒即可。", true, true},
		{"已提醒用户", true, true},
		{"提醒你即可。", true, true},
		{"已提醒用户复查 X", true, false},
		{"你看过 CI", true, false},
		{"", false, false}, // no due: nothing reminds it
	}
	for _, c := range cases {
		s, clock, r := newTest(t, "")
		p := Promise{Text: "提醒", CloseWhen: c.closeWhen}
		if c.due {
			p.Due = t0.Add(time.Minute)
		}
		p = mustAdd(t, s, p)
		clock.advance(time.Minute)
		s.Check()
		got, _ := s.Get(p.ID)
		if done := got.State == StateClosed; done != c.done {
			t.Errorf("close_when %q due %v: done = %v, want %v", c.closeWhen, c.due, done, c.done)
		}
		if c.done && (got.ClosedBy != SourceAuto || got.ClosedAt.IsZero() || len(r.closed) != 0) {
			t.Errorf("close_when %q: closed %+v, reminder told of closes %q", c.closeWhen, got, r.closed)
		}
	}
}

func TestCloseAndReopen(t *testing.T) {
	s, _, r := newTest(t, "")
	p := mustAdd(t, s, Promise{Text: "周五前整理好", Due: t0.Add(time.Hour)})
	cases := []struct {
		name string
		id   string
		by   Source
		want error
	}{
		{"auto refused", p.ID, SourceAuto, ErrInvalid},
		{"empty closer refused", p.ID, "", ErrInvalid},
		{"unknown id", "pr_nope", SourceUser, ErrNotFound},
		{"planner closes", p.ID, SourcePlanner, nil},
		{"closed twice", p.ID, SourceUser, ErrClosed},
	}
	for _, c := range cases {
		if err := s.Close(c.id, c.by, "整理好了"); !errors.Is(err, c.want) {
			t.Errorf("%s: Close = %v, want %v", c.name, err, c.want)
		}
	}
	open, closed := s.List(10)
	if len(open) != 0 || len(closed) != 1 || closed[0].ClosedAt.IsZero() || closed[0].ClosedBy != SourcePlanner || closed[0].Why != "整理好了" {
		t.Errorf("after close: open %d, closed %+v", len(open), closed)
	}
	if !slices.Equal(r.closed, []string{"周五前整理好"}) {
		t.Errorf("reminder told of closes %q, want the one", r.closed)
	}
	if err := s.Reopen(p.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(p.ID); got.State != StateOpen || !got.ClosedAt.IsZero() || got.ClosedBy != "" || got.Why != "" {
		t.Errorf("reopened %+v", got)
	}
	if err := s.Reopen("pr_nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Reopen(unknown) = %v", err)
	}
}

func TestPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "promises.json")
	s, clock, r := newTest(t, path)
	due := mustAdd(t, s, Promise{Text: "明早提醒你", Due: t0.Add(time.Minute), CloseWhen: "你看过", Source: SourceUser})
	mustAdd(t, s, Promise{Text: "CI 绿了告诉你", CloseWhen: "master CI 绿"})
	gone := mustAdd(t, s, Promise{Text: "done"})
	if err := s.Close(gone.ID, SourceUser, ""); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	s.Check()
	if len(r.due) != 1 {
		t.Fatalf("reminded %q before the restart, want one", r.due)
	}

	// A restart keeps every promise and does not remind again.
	s2, clock2, r2 := newTest(t, path)
	clock2.advance(2 * time.Minute)
	s2.Check()
	if len(r2.due) != 0 {
		t.Errorf("reminded %q again after a restart", r2.due)
	}
	open, closed := s2.List(10)
	if len(open) != 2 || len(closed) != 1 || closed[0].ID != gone.ID {
		t.Fatalf("reloaded open %+v closed %+v", open, closed)
	}
	if got := open[0]; got.ID != due.ID || got.Source != SourceUser || !got.Due.Equal(due.Due) || got.RemindedAt.IsZero() {
		t.Errorf("reloaded %+v, want %+v reminded", got, due)
	}
	if open[1].CloseWhen != "master CI 绿" || open[1].Source != SourcePlanner {
		t.Errorf("reloaded %+v", open[1])
	}

	// A file from a newer naozhi is read-only.
	newer := []byte(`{"v":99,"promises":[]}`)
	if err := os.WriteFile(path, newer, 0o600); err != nil {
		t.Fatal(err)
	}
	s3, _, _ := newTest(t, path)
	mustAdd(t, s3, Promise{Text: "kept in memory"})
	if data, _ := os.ReadFile(path); string(data) != string(newer) {
		t.Errorf("a newer file was overwritten: %s", data)
	}
}

// A promise's title is the thing itself, with the lead the Planner words it
// as about the user dropped.
func TestTitle(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"提醒用户提交周报", "提交周报"},
		{"提醒你：明早 9 点评审", "明早 9 点评审"},
		{" 帮用户 盯着 CI ", "盯着 CI"},
		{"提交周报", "提交周报"},
		{"提醒用户", "提醒用户"},
		{"用户提醒的事", "用户提醒的事"},
	} {
		if got := (Promise{Text: tc.text}).Title(); got != tc.want {
			t.Errorf("Title(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

// fakeWaker is a Reminder and Waker: err is what each wake answers.
type fakeWaker struct {
	fakeReminder
	woke []string
	err  error
}

func (w *fakeWaker) Wake(p Promise) error {
	w.woke = append(w.woke, p.ID)
	return w.err
}

// A due promise with a close condition wakes the Planner once; a pure
// reminder or an undated one does not.
func TestDueWakesPlanner(t *testing.T) {
	s, clock, _ := newTest(t, "")
	w := &fakeWaker{}
	s.SetReminder(w)
	cond := mustAdd(t, s, Promise{Text: "派出补拍", Due: t0.Add(time.Minute), CloseWhen: "补拍会话已派出"})
	only := mustAdd(t, s, Promise{Text: "交周报", Due: t0.Add(time.Minute)})
	mustAdd(t, s, Promise{Text: "盯 CI", CloseWhen: "CI 绿了"})
	clock.advance(time.Minute)
	s.Check()
	s.Check()
	if !slices.Equal(w.woke, []string{cond.ID}) {
		t.Fatalf("woke %q, want only %q", w.woke, cond.ID)
	}
	if !slices.Equal(w.due, []string{"派出补拍", "交周报"}) {
		t.Errorf("reminded %q", w.due)
	}
	if got, _ := s.Get(only.ID); got.State != StateClosed {
		t.Errorf("pure reminder %+v not done", got)
	}
	if got, _ := s.Get(cond.ID); got.WakeOwed || got.State != StateOpen {
		t.Errorf("woken promise %+v", got)
	}
}

// A wake the Planner did not take is owed: the next checks wake again,
// without reminding the user again, until one is taken; the debt is kept
// across restarts and dropped once the promise closes.
func TestDueWakeOwed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "promises.json")
	s, clock, _ := newTest(t, path)
	w := &fakeWaker{err: errors.New("已停用 Planner")}
	s.SetReminder(w)
	p := mustAdd(t, s, Promise{Text: "派出补拍", Due: t0.Add(time.Minute), CloseWhen: "补拍会话已派出"})
	clock.advance(time.Minute)
	s.Check()
	s.Check()
	if got, _ := s.Get(p.ID); !got.WakeOwed {
		t.Fatalf("failed wake not owed: %+v", got)
	}
	if len(w.woke) != 2 || len(w.due) != 1 {
		t.Fatalf("woke %q, reminded %q: want 2 wakes, 1 reminder", w.woke, w.due)
	}

	// Restart: the debt is read back and paid once the Planner takes it.
	s2 := New(Config{Path: path, Now: clock.Now})
	w2 := &fakeWaker{err: ErrInvalid}
	s2.SetReminder(w2)
	s2.Check()
	w2.err = nil
	s2.Check()
	s2.Check()
	if len(w2.woke) != 2 || len(w2.due) != 0 {
		t.Fatalf("after restart woke %q, reminded %q: want 2 wakes, no reminder", w2.woke, w2.due)
	}
	if got, _ := s2.Get(p.ID); got.WakeOwed {
		t.Errorf("taken wake still owed: %+v", got)
	}

	// Closed with a wake owed: nothing more is woken.
	q := mustAdd(t, s2, Promise{Text: "第二件", Due: clock.Now(), CloseWhen: "做完"})
	w2.err = errors.New("正在启动 Planner")
	s2.Check()
	if err := s2.Close(q.ID, SourceUser, ""); err != nil {
		t.Fatal(err)
	}
	n := len(w2.woke)
	s2.Check()
	if got, _ := s2.Get(q.ID); got.WakeOwed || len(w2.woke) != n {
		t.Errorf("closed promise %+v woken again (%d → %d)", got, n, len(w2.woke))
	}
}
