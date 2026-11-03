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
	mustAdd(t, s, Promise{Text: "later", Due: t0.Add(2 * time.Hour)})
	mustAdd(t, s, Promise{Text: "soon", Due: t0.Add(10 * time.Minute)})
	mustAdd(t, s, Promise{Text: "undated", CloseWhen: "CI 绿了"})
	closed := mustAdd(t, s, Promise{Text: "closed first", Due: t0.Add(5 * time.Minute)})
	if err := s.Close(closed.ID, SourceUser); err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		advance time.Duration
		due     []string // reminders so far
		overdue []string
	}{
		{0, nil, nil},
		{9 * time.Minute, nil, nil},
		{time.Minute, []string{"soon"}, nil},              // due exactly now
		{time.Minute, []string{"soon"}, []string{"soon"}}, // once only
		{2 * time.Hour, []string{"soon", "later"}, []string{"soon", "later"}},
	}
	for i, st := range steps {
		clock.advance(st.advance)
		s.Check()
		if !slices.Equal(r.due, st.due) {
			t.Errorf("step %d: reminded %q, want %q", i, r.due, st.due)
		}
		var overdue []string
		for _, p := range s.Overdue(clock.Now()) {
			overdue = append(overdue, p.Text)
		}
		if !slices.Equal(overdue, st.overdue) {
			t.Errorf("step %d: overdue %q, want %q", i, overdue, st.overdue)
		}
	}
	open, _ := s.List(10)
	var order []string
	for _, p := range open {
		order = append(order, p.Text)
		if p.State != StateOpen {
			t.Errorf("%q closed by its reminder", p.Text)
		}
	}
	if want := []string{"soon", "later", "undated"}; !slices.Equal(order, want) {
		t.Errorf("open order %q, want %q (due first, undated last)", order, want)
	}
}

func TestOnlyUserCloses(t *testing.T) {
	s, _, r := newTest(t, "")
	p := mustAdd(t, s, Promise{Text: "周五前整理好", Due: t0.Add(time.Hour)})
	cases := []struct {
		name string
		id   string
		by   Source
		want error
	}{
		{"planner refused", p.ID, SourcePlanner, ErrUserOnly},
		{"empty closer refused", p.ID, "", ErrUserOnly},
		{"unknown id", "pr_nope", SourceUser, ErrNotFound},
		{"user closes", p.ID, SourceUser, nil},
		{"closed twice", p.ID, SourceUser, ErrClosed},
	}
	for _, c := range cases {
		if err := s.Close(c.id, c.by); !errors.Is(err, c.want) {
			t.Errorf("%s: Close = %v, want %v", c.name, err, c.want)
		}
	}
	open, closed := s.List(10)
	if len(open) != 0 || len(closed) != 1 || closed[0].ClosedAt.IsZero() {
		t.Errorf("after close: open %d, closed %+v", len(open), closed)
	}
	if !slices.Equal(r.closed, []string{"周五前整理好"}) {
		t.Errorf("reminder told of closes %q, want the one", r.closed)
	}
}

func TestPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "promises.json")
	s, clock, r := newTest(t, path)
	due := mustAdd(t, s, Promise{Text: "明早提醒你", Due: t0.Add(time.Minute), Source: SourceUser})
	mustAdd(t, s, Promise{Text: "CI 绿了告诉你", CloseWhen: "master CI 绿"})
	gone := mustAdd(t, s, Promise{Text: "done"})
	if err := s.Close(gone.ID, SourceUser); err != nil {
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
