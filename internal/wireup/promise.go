package wireup

import (
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/planner/agent"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/textutil"
)

// promiseTitleRunes caps a promise's text in an inbox title.
const promiseTitleRunes = 60

// PromiseReminder is promise.Reminder: a due promise opens a "通知" item
// pushed past the daily cap (§3.6) and shows a reminder in the Planner
// conversation; the Planner or the user closing the promise closes its item.
type PromiseReminder struct {
	Notify *notify.Notifier
	// Note writes text into the Planner conversation as a gateway event
	// (agent.Agent.Note); nil skips it.
	Note func(text string)
}

func promiseSource(p promise.Promise) string { return "promise:" + p.ID }

// Due implements promise.Reminder.
func (r PromiseReminder) Due(p promise.Promise) {
	at := p.Due.Local().Format("1/2 15:04")
	if r.Notify != nil {
		why := "承诺到期 " + at
		if p.CloseWhen != "" {
			why += "；完成条件：" + p.CloseWhen
		}
		r.Notify.Add(notify.Item{Kind: notify.KindNotice, Level: notify.LevelTimely, Uncapped: true,
			Source: promiseSource(p), Title: "到点提醒：" + textutil.TruncateRunes(p.Title(), promiseTitleRunes), Why: why})
	}
	if r.Note != nil {
		r.Note("到点提醒（承诺，到期 " + at + "）：" + p.Title())
	}
}

// Closed implements promise.Reminder.
func (r PromiseReminder) Closed(p promise.Promise) {
	if r.Notify != nil {
		r.Notify.DismissSource(promiseSource(p))
	}
}

// PlannerPromises feeds the Planner's snapshot the open promises.
func PlannerPromises(s *promise.Store) func() []agent.Promise {
	return func() []agent.Promise {
		open, _ := s.List(0)
		out := make([]agent.Promise, 0, len(open))
		for _, p := range open {
			out = append(out, agent.Promise{ID: p.ID, Text: p.Text, Due: p.Due})
		}
		return out
	}
}
