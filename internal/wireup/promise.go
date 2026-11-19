package wireup

import (
	"encoding/json"
	"errors"
	"log/slog"

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
// It is a promise.Waker too: a due promise with a close condition wakes the
// Planner (Wake), which a Note never does.
type PromiseReminder struct {
	Notify *notify.Notifier
	// Note writes text into the Planner conversation as a gateway event
	// (agent.Agent.Note); nil skips it.
	Note func(text string)
	// Planner starts a Planner turn (agent.Agent.Wake); nil wakes nothing.
	Planner func(src agent.Source, sent func(int, error), blocks ...string) error
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

// promiseWake is the JSON a due wake names its promise with.
type promiseWake struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	Due       string `json:"due"`
	CloseWhen string `json:"close_when"`
}

// PromiseWakeBlock is the block a due promise wakes the Planner with: which
// promise, and that its turn is to act on it.
func PromiseWakeBlock(p promise.Promise) string {
	data, _ := json.Marshal(promiseWake{ID: p.ID, Text: p.Text, Due: p.Due.Local().Format("1/2 15:04"), CloseWhen: p.CloseWhen})
	return "<naozhi-reminder>\n" + string(data) + "\n" +
		"这条承诺到期了，网关已经提醒过用户。按承诺去做（要派活就用 dispatch）；做不了或还不到时候，就向用户说明原因和打算；完成条件满足时用 status 标 done。\n" +
		"</naozhi-reminder>"
}

// Wake implements promise.Waker. The Planner off, stopped or starting is an
// error, so the store owes the wake and tries again; a block it will never
// take is logged and given up, as is a missing Planner.
func (r PromiseReminder) Wake(p promise.Promise) error {
	if r.Planner == nil {
		return nil
	}
	err := r.Planner(agent.SourceReminder, nil, PromiseWakeBlock(p))
	if errors.Is(err, agent.ErrTooLong) || errors.Is(err, agent.ErrNoBlock) {
		slog.Warn("promise: due wake never takeable; given up", "id", p.ID, "err", err)
		return nil
	}
	return err
}

var _ promise.Waker = PromiseReminder{}

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
