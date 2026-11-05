package wireup

import (
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
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
