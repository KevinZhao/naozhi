// Package notify is the "等你" inbox and the push pipeline in front of it
// (docs/rfc/planner.md §3.8, §10.1, §15.1). Gateway rules open items from
// the health state machine, the critical-loop heartbeats and the disk of the
// state root; an item is pushed to the dashboard and the macOS notification
// centre by its level, a daily cap, quiet hours and the user's pause, and
// whatever is not pushed stays in the inbox. Nothing here calls a model.
//
// State lives in the Planner state root: inbox.json (the items), notify.json
// (the pause and today's push count) and the run marker that tells the next
// start whether this one exited cleanly.
package notify

import (
	"cmp"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/monitor/beat"
	"github.com/naozhi/naozhi/internal/monitor/wake"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
	"github.com/naozhi/naozhi/internal/planner/journal"
)

// Version is the inbox.json / notify.json format this build reads and writes.
const Version = 1

// Kind is what an item asks of the user. Values are wire-stable.
type Kind string

const (
	KindQuestion Kind = "question" // 提问
	KindError    Kind = "error"    // 错误
	KindNotice   Kind = "notice"   // 通知
	// KindDecision (决定) carries a Decision the user answers with one of its
	// actions; its handler applies the answer.
	KindDecision Kind = "decision"
)

// Level is how an item may be pushed.
type Level string

const (
	// LevelUrgent is pushed at once, past the daily cap and quiet hours.
	LevelUrgent Level = "urgent"
	// LevelTimely is pushed at once within the daily cap, outside quiet hours.
	LevelTimely Level = "timely"
)

// State is an item's lifecycle.
type State string

const (
	StateOpen   State = "open"
	StateClosed State = "closed"
)

// Closer is who closed an item.
type Closer string

const (
	ClosedByUser Closer = "user" // 你
	ClosedByAuto Closer = "auto" // 自动: the condition that opened it cleared
)

// Action is an answer to a decision item.
type Action string

const (
	ActionApprove Action = "approve"
	ActionReject  Action = "reject"
)

// Item is one inbox entry.
type Item struct {
	ID    string `json:"id"`
	Kind  Kind   `json:"kind"`
	Level Level  `json:"level"`
	// Unit is the session key or unit ID the item is about ("" for the
	// gateway itself); WS its workspace ID.
	Unit  string `json:"unit,omitempty"`
	WS    string `json:"ws,omitempty"`
	Title string `json:"title"`
	// Why says why it reached the user: state and names, never tool output.
	Why     string    `json:"why"`
	Created time.Time `json:"created"`
	State   State     `json:"state"`
	// ClosedBy and ClosedAt are set once the item is closed.
	ClosedBy Closer    `json:"closed_by,omitempty"`
	ClosedAt time.Time `json:"closed_at,omitzero"`
	// Source names the condition that opened the item; at most one item per
	// source is open.
	Source string `json:"source"`
	// AutoClose items close themselves when their condition clears; the
	// others only the user closes.
	AutoClose bool      `json:"auto_close,omitempty"`
	Decision  *Decision `json:"decision,omitempty"`
}

// Decision is what a decision item asks: Payload is its handler's own
// description of the proposal, Actions the answers offered.
type Decision struct {
	Handler string          `json:"handler"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Actions []Action        `json:"actions"`
	// Answer is the action the user chose, once closed by one.
	Answer Action `json:"answer,omitempty"`
}

// DecisionFunc applies the user's answer to a decision item. An error leaves
// the item open.
type DecisionFunc func(it Item, a Action) error

// Errors the inbox API returns.
var (
	ErrNotFound   = errors.New("notify: no such item")
	ErrClosed     = errors.New("notify: item already closed")
	ErrNoDecision = errors.New("notify: not a decision item, or the action is not offered")
	ErrNoHandler  = errors.New("notify: no handler for this decision")
)

// Retention for closed items: they are kept this long, at most keepClosed
// of them, so the inbox file stays small.
const (
	closedRetention = 7 * 24 * time.Hour
	keepClosed      = 200
)

// maxBytes caps inbox.json and notify.json reads.
const maxBytes = 4 << 20

// Push is one notification as it leaves for an exit.
type Push struct {
	ID    string
	Kind  Kind
	Level Level
	Title string
	Body  string
	// Unit is the session to open when the notification is clicked.
	Unit string
}

// Browser is the dashboard exit: the WebSocket hub.
type Browser interface {
	// Notify asks open dashboards to show p if they are in the background.
	Notify(p Push)
	// InboxCount reports the number of open items.
	InboxCount(open int)
}

// Config wires a Notifier. A zero Config keeps everything in memory and
// pushes nowhere but the browser exit.
type Config struct {
	Clock wake.Clock
	// Layout is the session store's layout: inbox.json, notify.json and the
	// run marker live in its Planner root. The zero Layout keeps the inbox in
	// memory and skips the marker and the disk check.
	Layout  datadir.Layout
	Journal *journal.Journal
	Beats   *beat.Registry
	// Name returns a unit's display name ("" falls back to the unit ID).
	Name func(unit string) string
	// Link is the dashboard URL system notifications carry.
	Link string
	// System is the operating-system exit; nil uses the macOS notification
	// centre on darwin and nothing elsewhere.
	System func(title, body string) error
	// DiskFree returns the free fraction of the Planner root's volume; nil
	// uses statfs.
	DiskFree func() (float64, error)
}

// Notifier owns the inbox and the push pipeline. Safe for concurrent use;
// Run's goroutine evaluates the sources.
type Notifier struct {
	cfg    Config
	health Health
	events chan healthEvent
	// crashed is set when the previous run left its marker behind.
	crashed bool

	browser atomic.Pointer[Browser]
	// pausedUntil mirrors pushState.PausedUntil (Unix nanoseconds, 0 when
	// not paused) for the peer watchdog, which must not wait on mu.
	pausedUntil atomic.Int64
	peerAlarmed atomic.Bool

	mu        sync.Mutex
	items     []Item
	st        pushState
	readOnly  bool // inbox.json is on disk but unusable: do not overwrite it
	deciders  map[string]DecisionFunc
	condSince map[string]time.Time // Run goroutine: when a gateway condition began
}

// pushState is notify.json.
type pushState struct {
	V           int       `json:"v"`
	PausedUntil time.Time `json:"paused_until,omitzero"`
	// Day (local YYYY-MM-DD) and Pushed count today's timely pushes.
	Day    string `json:"day,omitempty"`
	Pushed int    `json:"pushed,omitempty"`
}

type inboxFile struct {
	V     int    `json:"v"`
	Items []Item `json:"items"`
}

// New loads the inbox and push state and replaces the run marker,
// remembering whether the previous run left one (an abnormal exit, pushed
// once Run starts).
func New(cfg Config) *Notifier {
	if cfg.Clock == nil {
		cfg.Clock = wake.SystemClock()
	}
	if cfg.System == nil {
		cfg.System = systemExit()
	}
	lay := cfg.Layout
	if root := lay.PlannerRoot(); cfg.DiskFree == nil && root != "" {
		cfg.DiskFree = func() (float64, error) { return diskFree(root) }
	}
	n := &Notifier{
		cfg:       cfg,
		events:    make(chan healthEvent, 256),
		deciders:  map[string]DecisionFunc{},
		condSince: map[string]time.Time{},
	}
	if lay.Root() == "" {
		return n
	}
	f, out, err := jsonfile.Load[inboxFile](lay.PlannerInboxPath(), jsonfile.Options{MaxBytes: maxBytes, Label: "planner inbox"})
	switch {
	case err != nil:
		slog.Warn("planner inbox unreadable; changes kept in memory only", "err", err)
		n.readOnly = true
	case out == jsonfile.Parsed && f.V > Version:
		slog.Warn("planner inbox written by a newer naozhi; not overwriting it", "version", f.V)
		n.readOnly = true
	case out == jsonfile.Parsed:
		n.items = f.Items
	}
	if st, out, err := jsonfile.Load[pushState](lay.PlannerPushStatePath(), jsonfile.Options{MaxBytes: maxBytes, Label: "push state"}); err == nil && out == jsonfile.Parsed && st.V <= Version {
		n.st = st
		n.pausedUntil.Store(unixNano(st.PausedUntil))
	}
	marker := lay.PlannerRunMarkerPath()
	if _, err := os.Lstat(marker); err == nil {
		n.crashed = true
	}
	if err := osutil.WriteFileAtomic(marker, []byte(cfg.Clock.Now().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		slog.Warn("planner run marker not written", "err", err)
	}
	return n
}

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// Close removes the run marker: the run ended cleanly. Call it once the
// rest of shutdown is done.
func (n *Notifier) Close() {
	if p := n.cfg.Layout.PlannerRunMarkerPath(); p != "" {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			slog.Warn("planner run marker not removed", "err", err)
		}
	}
}

// SetBrowser binds the dashboard exit.
func (n *Notifier) SetBrowser(b Browser) { n.browser.Store(&b) }

// HandleDecisions registers fn for decision items whose Decision.Handler is
// handler.
func (n *Notifier) HandleDecisions(handler string, fn DecisionFunc) {
	n.mu.Lock()
	n.deciders[handler] = fn
	n.mu.Unlock()
}

// List returns the open items, newest first, and up to limit closed ones,
// most recently closed first.
func (n *Notifier) List(limit int) (open, closed []Item) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, it := range n.items {
		if it.State == StateOpen {
			open = append(open, it)
		} else {
			closed = append(closed, it)
		}
	}
	slices.SortFunc(open, func(a, b Item) int { return b.Created.Compare(a.Created) })
	slices.SortFunc(closed, func(a, b Item) int { return b.ClosedAt.Compare(a.ClosedAt) })
	if len(closed) > limit {
		closed = closed[:limit]
	}
	return open, closed
}

// Add opens it unless an item with its Source is already open, filling in
// ID, Created and State, and pushes it by its level. It returns the open
// item for the source and whether this call opened it.
func (n *Notifier) Add(it Item) (Item, bool) {
	n.mu.Lock()
	it, opened, fx := n.addLocked(it)
	n.mu.Unlock()
	n.apply(fx)
	return it, opened
}

// Dismiss closes an open item for the user.
func (n *Notifier) Dismiss(id string) error {
	n.mu.Lock()
	i := n.indexLocked(id)
	switch {
	case i < 0:
		n.mu.Unlock()
		return ErrNotFound
	case n.items[i].State != StateOpen:
		n.mu.Unlock()
		return ErrClosed
	}
	fx := n.closeLocked(i, ClosedByUser, "")
	n.mu.Unlock()
	n.apply(fx)
	return nil
}

// Decide answers a decision item with a and closes it once its handler
// applied the answer. The handler runs outside the inbox lock; of two
// concurrent answers the first to finish wins and the other gets ErrClosed.
func (n *Notifier) Decide(id string, a Action) error {
	n.mu.Lock()
	i := n.indexLocked(id)
	if i < 0 {
		n.mu.Unlock()
		return ErrNotFound
	}
	it := n.items[i]
	if it.State != StateOpen {
		n.mu.Unlock()
		return ErrClosed
	}
	if it.Decision == nil || !slices.Contains(it.Decision.Actions, a) {
		n.mu.Unlock()
		return ErrNoDecision
	}
	fn := n.deciders[it.Decision.Handler]
	n.mu.Unlock()
	if fn == nil {
		return ErrNoHandler
	}
	if err := fn(it, a); err != nil {
		return err
	}
	n.mu.Lock()
	if i = n.indexLocked(id); i < 0 || n.items[i].State != StateOpen {
		n.mu.Unlock()
		return ErrClosed
	}
	fx := n.closeLocked(i, ClosedByUser, a)
	n.mu.Unlock()
	n.apply(fx)
	return nil
}

// PausedUntil returns when the push pause ends; zero when not paused.
func (n *Notifier) PausedUntil() time.Time {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.cfg.Clock.Now().Before(n.st.PausedUntil) {
		return time.Time{}
	}
	return n.st.PausedUntil
}

// Pause holds every push until until (a zero or past until resumes); held
// items stay in the inbox.
func (n *Notifier) Pause(until time.Time) {
	n.mu.Lock()
	if !until.After(n.cfg.Clock.Now()) {
		until = time.Time{}
	}
	n.st.PausedUntil = until
	n.pausedUntil.Store(unixNano(until))
	n.saveStateLocked()
	n.mu.Unlock()
	n.record("", notifyData{Event: "pause", Until: until})
}

// EndOfToday is the "today" pause's end: the next local midnight after now.
func EndOfToday(now time.Time) time.Time {
	y, m, d := now.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, now.Location())
}

// effects are the outputs of an inbox change, applied outside mu.
type effects struct {
	journal []journalLine
	pushes  []Push
	count   int // open items after the change; -1 when unchanged
}

type journalLine struct {
	unit string
	data notifyData
}

// notifyData is a notify journal entry's data.
type notifyData struct {
	Event    string    `json:"event"` // open | close | push | held | pause | alarm
	ID       string    `json:"id,omitempty"`
	Kind     Kind      `json:"kind,omitempty"`
	Level    Level     `json:"level,omitempty"`
	Source   string    `json:"source,omitempty"`
	ClosedBy Closer    `json:"closed_by,omitempty"`
	Answer   Action    `json:"answer,omitempty"`
	Reason   string    `json:"reason,omitempty"` // why a push was held
	Until    time.Time `json:"until,omitzero"`
}

func (n *Notifier) addLocked(it Item) (Item, bool, effects) {
	fx := effects{count: -1}
	for _, o := range n.items {
		if o.State == StateOpen && o.Source == it.Source {
			return o, false, fx
		}
	}
	now := n.cfg.Clock.Now()
	it.ID = newID()
	it.Created, it.State, it.ClosedBy, it.ClosedAt = now, StateOpen, "", time.Time{}
	it.Level = cmp.Or(it.Level, LevelTimely)
	n.items = append(n.items, it)
	n.saveLocked(now)
	fx.count = n.openLocked()
	fx.journal = append(fx.journal, journalLine{it.Unit, notifyData{Event: "open", ID: it.ID, Kind: it.Kind, Level: it.Level, Source: it.Source}})
	if ok, reason := n.admitLocked(it.Level, now); ok {
		fx.pushes = append(fx.pushes, n.pushFor(it))
		fx.journal = append(fx.journal, journalLine{it.Unit, notifyData{Event: "push", ID: it.ID, Level: it.Level}})
	} else {
		fx.journal = append(fx.journal, journalLine{it.Unit, notifyData{Event: "held", ID: it.ID, Level: it.Level, Reason: reason}})
	}
	return it, true, fx
}

func (n *Notifier) closeLocked(i int, by Closer, answer Action) effects {
	now := n.cfg.Clock.Now()
	it := &n.items[i]
	it.State, it.ClosedBy, it.ClosedAt = StateClosed, by, now
	if answer != "" {
		it.Decision.Answer = answer
	}
	d := notifyData{Event: "close", ID: it.ID, Kind: it.Kind, Source: it.Source, ClosedBy: by, Answer: answer}
	unit := it.Unit
	n.saveLocked(now)
	return effects{journal: []journalLine{{unit, d}}, count: n.openLocked()}
}

func (n *Notifier) indexLocked(id string) int {
	return slices.IndexFunc(n.items, func(it Item) bool { return it.ID == id })
}

func (n *Notifier) openLocked() int {
	c := 0
	for _, it := range n.items {
		if it.State == StateOpen {
			c++
		}
	}
	return c
}

// saveLocked drops closed items past retention, then the earliest created
// ones beyond keepClosed, and writes inbox.json.
func (n *Notifier) saveLocked(now time.Time) {
	n.items = slices.DeleteFunc(n.items, func(it Item) bool {
		return it.State == StateClosed && now.Sub(it.ClosedAt) > closedRetention
	})
	excess := len(n.items) - n.openLocked() - keepClosed
	n.items = slices.DeleteFunc(n.items, func(it Item) bool {
		if it.State == StateClosed && excess > 0 {
			excess--
			return true
		}
		return false
	})
	path := n.cfg.Layout.PlannerInboxPath()
	if n.readOnly || path == "" {
		return
	}
	data, err := json.MarshalIndent(inboxFile{V: Version, Items: n.items}, "", "  ")
	if err == nil {
		err = osutil.WriteFileAtomic(path, data, 0o600)
	}
	if err != nil {
		slog.Warn("planner inbox save failed", "err", err)
	}
}

func (n *Notifier) saveStateLocked() {
	path := n.cfg.Layout.PlannerPushStatePath()
	if path == "" {
		return
	}
	n.st.V = Version
	data, err := json.Marshal(n.st)
	if err == nil {
		err = osutil.WriteFileAtomic(path, data, 0o600)
	}
	if err != nil {
		slog.Warn("push state save failed", "err", err)
	}
}

// apply runs fx's journal writes, count report and pushes.
func (n *Notifier) apply(fx effects) {
	for _, l := range fx.journal {
		n.record(l.unit, l.data)
	}
	b := n.browser.Load()
	if fx.count >= 0 && b != nil {
		(*b).InboxCount(fx.count)
	}
	for _, p := range fx.pushes {
		n.deliver(p)
	}
}

func (n *Notifier) record(unit string, d notifyData) {
	if n.cfg.Journal == nil {
		return
	}
	data, err := json.Marshal(d)
	if err == nil {
		err = n.cfg.Journal.Append(journal.Entry{TS: n.cfg.Clock.Now(), Kind: journal.KindNotify, Unit: unit, Data: data})
	}
	if err != nil {
		slog.Warn("notify: journal append failed", "event", d.Event, "err", err)
	}
}

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func newID() string {
	b := []byte("in_")
	for range 10 {
		b = append(b, idAlphabet[rand.IntN(len(idAlphabet))])
	}
	return string(b)
}
