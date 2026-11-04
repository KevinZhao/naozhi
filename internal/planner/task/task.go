// Package task is the Planner page's "任务" (docs/rfc/planner.md §15.4, D85):
// the sessions the Planner dispatched (task: keys, from their sidecars) and
// the promises, in one list with one state model. A dispatched session's
// 等你 and 失败 follow its health state, so they come and go with it; 已完成
// is the Planner's judgment (its status action block) or the user's, kept in
// the sidecar. A retired session (gone from the router) keeps the failure
// it had, else it ended: 已完成 by itself. A promise is 待提醒 before its
// due reminder, done once closed. Every state someone sets is journaled with who and why; a done
// task leaves the list Archive after it was done, its data kept.
package task

import (
	"cmp"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/promise"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Archive is how long a done task stays listed.
const Archive = 24 * time.Hour

// whyRunes caps a reason recorded with a state.
const whyRunes = 500

// State is a task's state. Values are wire-stable (sidecars, the API).
type State string

const (
	Doing   State = "doing"   // 进行中
	Waiting State = "waiting" // 等你: the session waits on the user's reply
	Failed  State = "failed"  // 失败
	Done    State = "done"    // 已完成
	Remind  State = "remind"  // 待提醒: a promise before its due reminder
)

// By is who set a state; "" is the default (a new task).
type By string

const (
	ByPlanner By = "planner" // Planner 判断
	ByUser    By = "user"    // 你标记
	ByAuto    By = "auto"    // a pure reminder done once it reminded
	ByHealth  By = "health"  // the session's health state
	ByEnded   By = "ended"   // a retired session that had not failed
)

// Kind is where a task comes from.
type Kind string

const (
	KindSession Kind = "session" // a session the Planner dispatched
	KindPromise Kind = "promise"
)

// Task is one row of the list.
type Task struct {
	// ID is the task: key or the promise ID.
	ID    string `json:"id"`
	Kind  Kind   `json:"kind"`
	Title string `json:"title"`
	State State  `json:"state"`
	By    By     `json:"by,omitempty"`
	Why   string `json:"why,omitempty"`
	// At is when State was entered (the task's creation for the default).
	At      time.Time `json:"at"`
	Created time.Time `json:"created"`
	// Due and CloseWhen are a promise's.
	Due       time.Time `json:"due,omitzero"`
	CloseWhen string    `json:"close_when,omitempty"`
	// Health is a dispatched session's health state, when the table has it;
	// Retired is when the session was first seen gone from the router.
	Health  health.State `json:"health,omitempty"`
	Retired time.Time    `json:"retired,omitzero"`
}

// Errors Set returns.
var (
	ErrNotFound = errors.New("task: no such task")
	ErrState    = errors.New("task: not a state this task can be set to")
)

// Config wires a Board; a nil source leaves its tasks out.
type Config struct {
	Sessions *sidecar.Store
	Promises *promise.Store
	Health   func() health.Snapshot
	Journal  *journal.Journal
	// Now is the wall clock; nil is time.Now.
	Now func() time.Time
}

// Board reads and sets the tasks. Safe for concurrent use.
type Board struct {
	cfg Config
}

// New returns a Board over cfg's sources.
func New(cfg Config) *Board {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Board{cfg: cfg}
}

// sessionState is a dispatched session's task state. Its own course comes
// first: while live, 等你 or 失败 as its health says, else 进行中; once
// retired, the failure it had, else 已完成 as ended. A mark holds over that:
// done or failed always, doing (reopened) unless the session entered its
// state after it.
func sessionState(m sidecar.Meta, h health.Unit) (State, By, string, time.Time) {
	st, by, why, at := Doing, By(""), "", m.Created
	switch {
	case !m.Retired.IsZero() && m.Failure != nil:
		st, by, why, at = Failed, ByHealth, m.Failure.Why, m.Failure.At
	case !m.Retired.IsZero():
		st, by, at = Done, ByEnded, m.Retired
	case h.State == health.Waiting:
		st, by, at = Waiting, ByHealth, h.Since
	case h.State == health.Failed:
		st, by, why, at = Failed, ByHealth, h.Evidence, h.Since
	}
	if mk := m.Mark; mk != nil && (State(mk.State) != Doing || by == "" || !mk.At.Before(at)) {
		return State(mk.State), By(mk.By), mk.Why, mk.At
	}
	return st, by, why, at
}

// promiseState is a promise's task state.
func promiseState(p promise.Promise, now time.Time) (State, By, string, time.Time) {
	switch {
	case p.State == promise.StateClosed:
		return Done, By(cmp.Or(p.ClosedBy, promise.SourceUser)), p.Why, p.ClosedAt
	case !p.Due.IsZero() && p.RemindedAt.IsZero() && now.Before(p.Due):
		return Remind, "", "", p.Created
	}
	return Doing, "", "", p.Created
}

// archived reports whether t is done and past Archive.
func archived(t Task, now time.Time) bool {
	return t.State == Done && now.Sub(t.At) >= Archive
}

// List returns the tasks not archived: 进行中 (等你 and 失败 first, newest
// first), then 待提醒 (earliest due first), then 已完成 (latest first).
func (b *Board) List() []Task {
	now := b.cfg.Now()
	var out []Task
	if b.cfg.Sessions != nil {
		units := map[string]health.Unit{}
		if b.cfg.Health != nil {
			for _, u := range b.cfg.Health().Units {
				units[u.ID] = u
			}
		}
		for _, m := range b.cfg.Sessions.List() {
			if m.Purpose != sidecar.PurposeTask {
				continue
			}
			u := units[m.Key]
			t := Task{ID: m.Key, Kind: KindSession, Title: cmp.Or(m.Title, m.Key), Created: m.Created, Health: u.State, Retired: m.Retired}
			t.State, t.By, t.Why, t.At = sessionState(m, u)
			out = append(out, t)
		}
	}
	if b.cfg.Promises != nil {
		open, closed := b.cfg.Promises.List(1 << 30)
		for _, p := range append(open, closed...) {
			t := Task{ID: p.ID, Kind: KindPromise, Title: p.Text, Created: p.Created, Due: p.Due, CloseWhen: p.CloseWhen}
			t.State, t.By, t.Why, t.At = promiseState(p, now)
			out = append(out, t)
		}
	}
	out = slices.DeleteFunc(out, func(t Task) bool { return archived(t, now) })
	slices.SortStableFunc(out, compare)
	return out
}

var rank = map[State]int{Waiting: 0, Failed: 1, Doing: 2, Remind: 3, Done: 4}

func compare(a, b Task) int {
	if c := cmp.Compare(rank[a.State], rank[b.State]); c != 0 {
		return c
	}
	switch a.State {
	case Remind:
		return cmp.Or(a.Due.Compare(b.Due), cmp.Compare(a.ID, b.ID))
	case Done:
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.ID, b.ID))
	}
	return cmp.Or(b.Created.Compare(a.Created), cmp.Compare(a.ID, b.ID))
}

// Title is task id's title; false when there is no such task.
func (b *Board) Title(id string) (string, bool) {
	if sessionkey.IsTaskKey(id) {
		if b.cfg.Sessions == nil {
			return "", false
		}
		m, ok := b.cfg.Sessions.Get(id)
		if !ok || m.Purpose != sidecar.PurposeTask {
			return "", false
		}
		return cmp.Or(m.Title, id), true
	}
	if b.cfg.Promises == nil {
		return "", false
	}
	p, ok := b.cfg.Promises.Get(id)
	return p.Text, ok
}

// Set sets task id to state (doing, done or failed; a promise is never
// failed) for by, with the reason why, and journals it. Setting a state the
// task is in already is no change.
func (b *Board) Set(id string, state State, by By, why string) error {
	if state != Doing && state != Done && state != Failed {
		return ErrState
	}
	why = textutil.TruncateRunes(strings.Join(strings.Fields(why), " "), whyRunes)
	unit := id
	if sessionkey.IsTaskKey(id) {
		if _, ok := b.Title(id); !ok {
			return ErrNotFound
		}
		ok, err := b.cfg.Sessions.SetMark(id, sidecar.Mark{State: string(state), By: string(by), Why: why, At: b.cfg.Now()})
		switch {
		case !ok:
			return ErrNotFound
		case err != nil:
			return err
		}
	} else {
		if b.cfg.Promises == nil {
			return ErrNotFound
		}
		var err error
		switch state {
		case Done:
			err = b.cfg.Promises.Close(id, promise.Source(by), why)
			if errors.Is(err, promise.ErrClosed) {
				return nil
			}
		case Doing:
			err = b.cfg.Promises.Reopen(id)
		default:
			return ErrState
		}
		if errors.Is(err, promise.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		unit = "promise:" + id
	}
	b.record(unit, id, state, by, why)
	return nil
}

// Observe is a health.Machine subscriber: it keeps on a dispatched session's
// sidecar the failure health judged it in, cleared when it leaves it, so a
// session that failed still shows 失败 once retired from the router.
func (b *Board) Observe(ev health.Event) {
	t := ev.Transition
	if t == nil || b.cfg.Sessions == nil || !sessionkey.IsTaskKey(t.Unit.ID) {
		return
	}
	var f *sidecar.Mark
	if t.Unit.State == health.Failed {
		f = &sidecar.Mark{State: string(Failed), By: string(ByHealth), Why: textutil.TruncateRunes(t.Unit.Evidence, whyRunes), At: t.Unit.Since}
	}
	if _, err := b.cfg.Sessions.SetFailure(t.Unit.ID, f); err != nil {
		slog.Warn("task: failure not saved", "key", t.Unit.ID, "err", err)
	}
}

// journalData is a task state's notify journal entry.
type journalData struct {
	Event string `json:"event"` // task_state
	ID    string `json:"id"`
	State State  `json:"state"`
	By    By     `json:"by"`
	Why   string `json:"why,omitempty"`
}

func (b *Board) record(unit, id string, state State, by By, why string) {
	if b.cfg.Journal == nil {
		return
	}
	data, err := json.Marshal(journalData{Event: "task_state", ID: id, State: state, By: by, Why: why})
	if err == nil {
		err = b.cfg.Journal.Append(journal.Entry{TS: b.cfg.Now(), Kind: journal.KindNotify, Unit: unit, Data: data})
	}
	if err != nil {
		slog.Warn("task: journal append failed", "id", id, "err", err)
	}
}
