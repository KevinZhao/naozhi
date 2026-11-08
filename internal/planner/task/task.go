// Package task is the Planner page's "任务" (docs/rfc/planner.md §15.4, D85):
// the sessions the Planner dispatched (task: keys, from their sidecars) and
// the promises, in one list with one state model. A dispatched session's
// 等你 and 失败 follow its health state, so they come and go with it; 已完成
// is the Planner's judgment (its status action block) or the user's, kept in
// the sidecar; a session the user stopped is 已完成 as theirs (D91: 失败 is
// left to a process that died by itself). A retired session (gone from the
// router) keeps the failure it had, else it ended: 已完成 by itself. A promise is 待提醒 before its
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
	// Reap is a done dispatched session put away (planner/reap): its
	// worktrees and what became of them; reopening puts it back. ReapWait
	// is why a done one is not put away yet ("" when nothing holds it).
	Reap     *sidecar.Reap `json:"reap,omitempty"`
	ReapWait string        `json:"reap_wait,omitempty"`
	// HealthSince is when the session entered Health: how long its turn
	// has run while active, how long it has waited or stalled.
	HealthSince time.Time `json:"health_since,omitzero"`
	// Live is what a dispatched session still in the router is doing now,
	// for the card's status line; nil once it is gone.
	Live *Live `json:"live,omitempty"`
	// Report is a dispatched session's latest report (§15.12 ③), the
	// card's "最新结论", from the sidecar; index.json reads its own from the
	// digest's state, so a report is recorded in both. Digest says the
	// session has a digest the card can open (GET /api/sessions/digest).
	Report *sidecar.Report `json:"report,omitempty"`
	Digest bool            `json:"digest,omitempty"`
}

// liveRunes caps Live's texts: a card shows one line of them.
const liveRunes = 200

// Live is a dispatched session's state in the router. Its texts are the
// session's own output, data the page escapes.
type Live struct {
	Running bool `json:"running,omitempty"`
	// Activity is the latest tool call or thinking, Reply the start of the
	// latest reply; each one line.
	Activity   string    `json:"activity,omitempty"`
	Reply      string    `json:"reply,omitempty"`
	LastActive time.Time `json:"last_active,omitzero"`
}

// oneLine folds s to one line of at most liveRunes.
func oneLine(s string) string {
	return textutil.TruncateRunes(strings.Join(strings.Fields(s), " "), liveRunes)
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
	// Stop stops dispatched session key's work (its process) for
	// Board.Stop; promises are the IDs of the promises citing it that the
	// stop closed. nil only marks it.
	Stop func(key, title string, promises []string)
	// OnState is told of each state set on a dispatched session, after it
	// was saved (planner/reap: done puts it away, doing puts it back); nil
	// tells no one.
	OnState func(key string, state State)
	// ReapWait is why done dispatched session key is not put away yet; nil
	// says nothing.
	ReapWait func(key string) string
	// Live is the dispatched sessions in the router by key; nil leaves
	// Task.Live out.
	Live func() map[string]Live
	// Digest reports whether session key has a digest; nil says none has.
	Digest func(key string) bool
	// Now is the wall clock; nil is time.Now.
	Now func() time.Time
}

// StoppedWhy is the reason a task the user stopped is 已完成 with.
const StoppedWhy = "已停止"

// StoppedPromiseWhy is the reason a promise citing a stopped task is closed
// with.
const StoppedPromiseWhy = "任务已停止"

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
		var live map[string]Live
		if b.cfg.Live != nil {
			live = b.cfg.Live()
		}
		for _, m := range b.cfg.Sessions.List() {
			if m.Purpose != sidecar.PurposeTask {
				continue
			}
			u := units[m.Key]
			t := Task{ID: m.Key, Kind: KindSession, Title: cmp.Or(m.Title, m.Key), Created: m.Created, Health: u.State, HealthSince: u.Since, Retired: m.Retired, Reap: m.Reap, Report: m.Report}
			t.Digest = b.cfg.Digest != nil && b.cfg.Digest(m.Key)
			if l, ok := live[m.Key]; ok && m.Retired.IsZero() {
				l.Activity, l.Reply = oneLine(l.Activity), oneLine(l.Reply)
				t.Live = &l
			}
			t.State, t.By, t.Why, t.At = sessionState(m, u)
			if t.State == Done && m.Reap == nil && b.cfg.ReapWait != nil {
				t.ReapWait = b.cfg.ReapWait(m.Key)
			}
			out = append(out, t)
		}
	}
	if b.cfg.Promises != nil {
		open, closed := b.cfg.Promises.List(1 << 30)
		for _, p := range append(open, closed...) {
			t := Task{ID: p.ID, Kind: KindPromise, Title: p.Title(), Created: p.Created, Due: p.Due, CloseWhen: p.CloseWhen}
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
	return p.Title(), ok
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
	if unit == id && b.cfg.OnState != nil {
		b.cfg.OnState(id, state)
	}
	return nil
}

// Stop stops dispatched session id for the user (D88: what the Planner
// dispatches runs at once, so stopping it is the user's control): the task
// is marked 已完成 as theirs, StoppedWhy (D91: stopping is the user's
// decision, not a failure), the open promises citing it ("X 做完后
// 告诉用户结果") are done as theirs, StoppedPromiseWhy, since nothing will
// meet them now, then Config.Stop stops its process. The session stays, its
// history readable; a mark holds over whatever it does later, until someone
// sets another state.
func (b *Board) Stop(id string) error {
	if !sessionkey.IsTaskKey(id) {
		return ErrNotFound
	}
	title, ok := b.Title(id)
	if !ok {
		return ErrNotFound
	}
	if err := b.Set(id, Done, ByUser, StoppedWhy); err != nil {
		return err
	}
	var closed []string
	if b.cfg.Promises != nil {
		open, _ := b.cfg.Promises.List(0)
		for _, p := range open {
			if !p.Cites(id) {
				continue
			}
			if err := b.Set(p.ID, Done, ByUser, StoppedPromiseWhy); err != nil {
				slog.Warn("task stop: citing promise not closed", "key", id, "promise", p.ID, "err", err)
				continue
			}
			closed = append(closed, p.ID)
		}
	}
	if b.cfg.Stop != nil {
		b.cfg.Stop(id, title, closed)
	}
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
