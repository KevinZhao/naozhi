package wireup

import (
	"testing"
	"time"

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
