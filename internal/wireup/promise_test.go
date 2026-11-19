package wireup

import (
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/promise"
)

// TestPromiseReminder_NoteTime pins the reminder's due time as the dashboard
// writes a point in time: M/D HH:mm.
func TestPromiseReminder_NoteTime(t *testing.T) {
	var got string
	PromiseReminder{Note: func(text string) { got = text }}.Due(promise.Promise{
		Text: "交周报", Due: time.Date(2026, 11, 4, 9, 5, 0, 0, time.Local),
	})
	if want := "到点提醒（承诺，到期 11/4 09:05）：交周报"; got != want {
		t.Fatalf("note = %q, want %q", got, want)
	}
}

// A due promise is pushed and noted as the thing itself, without the lead
// the Planner worded it with as about the user.
func TestPromiseReminderTitle(t *testing.T) {
	inbox := notify.New(notify.Config{System: func(string, string) error { return nil }})
	var notes []string
	r := PromiseReminder{Notify: inbox, Note: func(text string) { notes = append(notes, text) }}
	r.Due(promise.Promise{ID: "pr_a", Text: "提醒用户提交周报", Due: time.Now()})
	open, _ := inbox.List(0)
	if len(open) != 1 || open[0].Title != "到点提醒：提交周报" {
		t.Errorf("inbox = %+v", open)
	}
	if len(notes) != 1 || !strings.HasSuffix(notes[0], "）：提交周报") {
		t.Errorf("notes = %q", notes)
	}
}

// A due promise with a close condition wakes the Planner from
// SourceReminder with a block naming it; a pure reminder does not. A wake
// refused while the Planner is stopped is owed and taken once it resumes,
// the user reminded once.
func TestPromiseReminder_Wake(t *testing.T) {
	now := time.Date(2026, 11, 19, 16, 0, 0, 0, time.Local)
	clock := now
	s := promise.New(promise.Config{Now: func() time.Time { return clock }})
	var notes, blocks []string
	var srcs []agent.Source
	wakeErr := agent.ErrStopped
	s.SetReminder(PromiseReminder{
		Note: func(text string) { notes = append(notes, text) },
		Planner: func(src agent.Source, _ func(int, error), b ...string) error {
			srcs = append(srcs, src)
			blocks = append(blocks, b...)
			return wakeErr
		},
	})
	cond, err := s.Add(promise.Promise{Text: "派出 CPU 容量快照补拍", Due: now, CloseWhen: "补拍会话已派出"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(promise.Promise{Text: "交周报", Due: now}); err != nil {
		t.Fatal(err)
	}
	s.Check()
	if len(blocks) != 1 || len(notes) != 2 {
		t.Fatalf("blocks %q, notes %q: want one wake (the conditioned one), two reminders", blocks, notes)
	}
	clock = clock.Add(time.Minute)
	wakeErr = nil
	s.Check()
	s.Check()
	if len(blocks) != 2 || len(notes) != 2 {
		t.Fatalf("after resume blocks %d, notes %d: want the owed wake once, no new reminder", len(blocks), len(notes))
	}
	for _, src := range srcs {
		if src != agent.SourceReminder {
			t.Errorf("wake source %q, want %q", src, agent.SourceReminder)
		}
	}
	b := blocks[1]
	for _, want := range []string{"<naozhi-reminder>", cond.ID, "派出 CPU 容量快照补拍", "11/19 16:00", "补拍会话已派出", "按承诺去做", "向用户说明", "</naozhi-reminder>"} {
		if !strings.Contains(b, want) {
			t.Errorf("wake block lacks %q:\n%s", want, b)
		}
	}
}

// A block the Planner will never take is given up, not owed forever; one it
// cannot take now is owed.
func TestPromiseReminder_WakeErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		owed bool
	}{
		{agent.ErrOff, true},
		{agent.ErrStopped, true},
		{agent.ErrNotReady, true},
		{agent.ErrTooLong, false},
		{agent.ErrNoBlock, false},
		{nil, false},
	} {
		r := PromiseReminder{Planner: func(agent.Source, func(int, error), ...string) error { return c.err }}
		if got := r.Wake(promise.Promise{ID: "pr_a", Text: "x", CloseWhen: "y"}) != nil; got != c.owed {
			t.Errorf("%v: owed = %v, want %v", c.err, got, c.owed)
		}
	}
	if err := (PromiseReminder{}).Wake(promise.Promise{}); err != nil {
		t.Errorf("no Planner: %v", err)
	}
}
