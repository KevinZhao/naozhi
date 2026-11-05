package wireup

import (
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/promise"
)

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
