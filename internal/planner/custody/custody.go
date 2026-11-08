// Package custody is the gateway watching sessions for the user
// (docs/rfc/planner.md §5.4, narrowed in v0.5.0 by D93). It only decides
// whether to reply for the user; it never stops a session's own actions.
//
// A session's custody is a switch in its sidecar: on by default for a session
// the Planner dispatched and for one with a workflow running, off for the
// rest; the user flips it from the session header, the Planner with its
// custody action block. The switch only decides whether custody replies for
// the user (a recommended option, the Planner's go-ahead); a failure's retry
// is every local dashboard session's (D94), the user's own chats included,
// and a failure's count ends once it recovered. Every
// message custody sends goes through Deliver: its text is the message
// alone, its transcript entry carries the source custody, the reason and
// the count; it is counted against the lineage's limits (3 in a row with no
// progress, 8 in all), a reply for the user is held back while the user is
// at the session (viewing it, or sent it a message in the last NearbyFor),
// and none goes out while the one pause (暂停推送与托管, the inbox's) holds.
// A lineage is a session and its sidecar descendants; each dispatch starts
// its own (the Planner is in none), and the Planner is told not to dispatch
// around a stopped one; the user's message to a stopped one starts it
// afresh. Custody runs only with the Planner on.
//
// Two triggers live here (L0, deterministic): a session whose health turned
// failed with a retryable error class gets "继续" after a backoff (at once
// for a network error after the machine woke), and is left to the user
// after MaxErrors failures of a class; one whose credential expired gets it
// once the credential is refreshed (CredExpiry); an AskUserQuestion card
// left SuggestAfter is answered with its recommended options, handed to the
// Planner when it has none, or left to the user (suggest.go). A turn that
// ended normally goes to the Planner (planner/action's receipts); nothing
// here reads a reply's text or calls a model. State lives in the sidecars
// (the switch, and on a lineage's root what custody did), so a restart
// keeps it.
package custody

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cliinfo"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
	"github.com/naozhi/naozhi/internal/planner/task"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Limits (D93). Constants, not configuration.
const (
	// MaxSends is how many messages custody sends over one lineage.
	MaxSends = 8
	// MaxIdle is how many in a row may see no progress.
	MaxIdle = 3
	// MaxErrors is how many failures of one error class custody answers
	// before it stops: the first MaxErrors-1 get a retry.
	MaxErrors = 5
	// NearbyFor is how long after the user's message to a session custody
	// leaves it to them.
	NearbyFor = 5 * time.Minute
	// ViewFor is how long a dashboard's report that the user views a
	// session holds: the dashboard renews it sooner while the session stays
	// open, so a tab that died without saying so lapses.
	ViewFor = time.Minute
	// tickInterval is how often due retries and running workflows are
	// looked at.
	tickInterval = 15 * time.Second
	// staleAfter is how old a failure custody first sees may be and still
	// get a retry: one older failed while custody was not watching (before a
	// restart, or an upgrade that brought the user's own chats in), and no
	// one is waiting on its "继续" now.
	staleAfter = 30 * time.Minute
	// maxLineage bounds the walk up a lineage.
	maxLineage = 32
	// stateBytes caps custody.json.
	stateBytes = 4 << 10
)

// Backoff is the wait before the retry of a class's n-th failure.
var Backoff = [MaxErrors - 1]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}

// RetryText is what a retry says.
const RetryText = "继续"

// Reason is why custody sends a message, as its transcript entry shows it
// (clievent.EventEntry.CustodyReason).
type Reason string

const (
	ReasonRetry     Reason = "出错重试"
	ReasonSuggested Reason = "按建议执行"
	ReasonPlanner   Reason = "Planner 继续"
	// ReasonCredential is the retry of a failure for an expired credential,
	// once it was refreshed.
	ReasonCredential Reason = "凭证刷新后继续"
)

// Who set a session's switch (sidecar.Custody.By).
const (
	ByAuto    = "auto"
	ByUser    = "user"
	ByPlanner = "planner"
)

// Errors Deliver, Check and Set return.
var (
	ErrOff     = errors.New("托管没有开")
	ErrPaused  = errors.New("托管已暂停")
	ErrStopped = errors.New("托管已停")
	ErrNearby  = errors.New("你在看这个会话，托管暂缓")
	// ErrRefused: the Planner, a scout run and a daemon are never in custody.
	ErrRefused = errors.New("这个会话不能托管")
	// ErrNoSession: key is no session of this node (a remote node's is not
	// in custody).
	ErrNoSession = errors.New("找不到这个会话")
	// ErrDisabled: custody runs only with the Planner on (Config.Disabled).
	ErrDisabled = errors.New("托管不可用：Planner 没有开")
	// ErrIrreversible: what custody would answer for the user cannot be
	// undone (Irreversible), so it is the user's.
	ErrIrreversible = errors.New("涉及不可逆的操作，不替你回答")
)

// State is a session's custody as the header chip shows it.
type State string

const (
	StateOff      State = "off"
	StateOn       State = "on"
	StateDeferred State = "deferred" // 已暂缓: the user is at the session
	StateStopped  State = "stopped"  // 已停: a limit was reached
	StatePaused   State = "paused"   // custody is paused for every session
)

// Status is one session's custody.
type Status struct {
	Key string `json:"key"`
	On  bool   `json:"on"`
	// By and At are who last set the switch and when; empty for a default.
	By    string    `json:"by,omitempty"`
	At    time.Time `json:"at,omitzero"`
	State State     `json:"state"`
	// Why is why custody stopped.
	Why string `json:"why,omitempty"`
	// Sent is how many messages custody sent over the lineage, of Max.
	Sent int `json:"sent"`
	Max  int `json:"max"`
}

// Inbox is where custody leaves a session to the user (notify.Notifier).
type Inbox interface {
	Add(it notify.Item) (notify.Item, bool)
}

// Config wires an Engine. Sessions and Send are required for custody to do
// anything; a nil source reads as nothing (no failures, no progress).
type Config struct {
	Sessions *sidecar.Store
	// Send sends text to key the way the user's own message goes, so a
	// session whose process went away resumes, as custody's n-th message
	// over the lineage for reason (server.Server.SubmitCustody).
	Send    func(key, text string, reason Reason, n int) error
	Inbox   Inbox
	Journal *journal.Journal
	Health  func() health.Snapshot
	// Local reports whether key is a session of this node's router.
	Local func(key string) bool
	// Workflows are the local sessions with a workflow running.
	Workflows func() []string
	// LastUser is when the user last sent key a message (zero for never).
	LastUser func(key string) time.Time
	// Progress is key's progress as deterministic signals: the workspace's
	// git HEAD, its newest output, its workflows' phases, the user's last
	// message. Custody compares it, never reads it.
	Progress func(key string) string
	// FailText is the text of key's failure (its last API error or its
	// process's death detail), classified, never stored.
	FailText func(key string) string
	// Settled reports whether key's turn ended with nothing pending: not
	// running, no unanswered AskUserQuestion, no background task running.
	Settled func(key string) bool
	// Name is how the user reads key.
	Name func(key string) string
	// Events are key's latest events, at most n: its AskUserQuestion cards
	// and the messages after them (suggest.go).
	Events func(key string, n int) []clievent.EventEntry
	// HandOff gives the Planner a card with no recommended option
	// (planner/action.Runner.HandOff); nil leaves such a card to the user.
	HandOff func(key, what string) error
	// SendUser sends text to key as the user's own message: the undo of an
	// answer custody gave, which the user clicked (Undo).
	SendUser func(key, text string) error
	// PausedUntil is when the one pause ends (notify.Notifier.PausedUntil:
	// 暂停推送与托管), zero when not paused; Pause sets it
	// (notify.Notifier.Pause), so /api/custody/pause is the inbox pause's
	// alias. Without them the pause is kept in memory.
	PausedUntil func() time.Time
	Pause       func(until time.Time)
	// CredExpiry is when the credential sessions fail on expires
	// (midway.Expiry), zero for none: a failure for an expired credential
	// waits for it to expire later, refreshed, then gets one "继续". nil
	// leaves such failures to the user.
	CredExpiry func() time.Time
	// Woke reports whether the machine woke from sleep since the previous
	// call (monitor/wake.Detector.Woke); a network error's retry is due at
	// once then. Called from Tick only.
	Woke func() bool
	// Path is custody.json, where v0.5.0 kept its own pause: one found
	// paused is carried over to the one pause until the end of today.
	Path string
	Now  func() time.Time
	// Disabled leaves every session out of custody: with the Planner off
	// no one judges a turn's end, and the gateway does not start replying
	// for a user who never turned the Planner on.
	Disabled bool
}

// Engine is custody. Safe for concurrent use.
type Engine struct {
	cfg  Config
	poke chan struct{}

	// mu serializes custody's decisions and its sidecar writes.
	mu sync.Mutex
	// pausedUntil is the pause without Config.PausedUntil.
	pausedUntil time.Time
	// viewing are each session's viewers (dashboard connections), by when
	// they last reported it.
	viewing map[string]map[string]time.Time
	// deferred are the sessions whose holding back is noted, until custody
	// sends or the user leaves.
	deferred map[string]bool

	// askMu serializes answerAsks; asks are the cards it waits on, by
	// session.
	askMu sync.Mutex
	asks  map[string]*askWait

	// faultMu guards faulted: the sessions whose failure health merged into
	// a systemic fault, which the inbox reports once for all of them.
	faultMu sync.Mutex
	faulted map[string]bool
}

// state is v0.5.0's custody.json.
type state struct {
	V      int  `json:"v"`
	Paused bool `json:"paused"`
}

// New returns an Engine. A pause v0.5.0 kept in custody.json is carried
// over to the one pause, until the end of today, and the file removed.
func New(cfg Config) *Engine {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	e := &Engine{cfg: cfg, poke: make(chan struct{}, 1), viewing: map[string]map[string]time.Time{}, deferred: map[string]bool{},
		asks: map[string]*askWait{}, faulted: map[string]bool{}}
	if cfg.Path != "" {
		st, out, err := jsonfile.Load[state](cfg.Path, jsonfile.Options{MaxBytes: stateBytes, Label: "custody state", Corrupt: jsonfile.LeaveCorrupt})
		switch {
		case err != nil:
			slog.Warn("custody state unreadable; its pause not carried over", "err", err)
		case out == jsonfile.Parsed:
			if st.Paused {
				e.pause(notify.EndOfToday(e.now()))
			}
			if err := os.Remove(cfg.Path); err != nil {
				slog.Warn("custody state not removed", "err", err)
			}
		}
	}
	return e
}

func (e *Engine) now() time.Time { return e.cfg.Now() }

// Poke has the next tick run now. It never blocks.
func (e *Engine) Poke() {
	select {
	case e.poke <- struct{}{}:
	default:
	}
}

// HandleHealth is a health.Machine subscriber: a transition may be a failure
// to answer, and one that merged into a systemic fault is not reported on
// its own.
func (e *Engine) HandleHealth(ev health.Event) {
	if t := ev.Transition; t != nil {
		e.faultMu.Lock()
		if t.Unit.State == health.Failed && t.InFault {
			e.faulted[t.Unit.ID] = true
		} else {
			delete(e.faulted, t.Unit.ID)
		}
		e.faultMu.Unlock()
	}
	e.Poke()
}

func (e *Engine) inFault(key string) bool {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	return e.faulted[key]
}

// Run ticks until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		e.safely(e.Tick)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.poke:
		}
	}
}

func (e *Engine) safely(fn func()) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("custody: panicked", "panic", p)
		}
	}()
	fn()
}

// eligible refuses the keys custody never covers.
func (e *Engine) eligible(key string) error {
	switch {
	case e.cfg.Disabled:
		return ErrDisabled
	case key == "":
		return ErrNoSession
	case sessionkey.IsPlannerNSKey(key), sessionkey.IsScoutKey(key), sessionkey.IsSysKey(key):
		return ErrRefused
	case e.cfg.Local == nil || !e.cfg.Local(key):
		return ErrNoSession
	}
	return nil
}

// retryEligible refuses, beyond eligible, the keys whose failures custody
// does not retry: a cron run and a scratch session are one-shot; and a
// retry goes the dashboard's way (Config.Send), so a session whose replies
// go elsewhere (an IM chat, a project planner one may be bound to) is
// retried only with its switch on. A dispatched session is retried, its
// switch on or off (D94 D1), only while its task runs: once the task is
// done (and nobody turned custody back on since, see on) or the session was
// put away, a failure is the end of the task's process, not of a turn the
// user waits on, and a "继续" would resume a session closed on purpose.
func (e *Engine) retryEligible(key string) error {
	if err := e.eligible(key); err != nil {
		return err
	}
	if sessionkey.IsCronKey(key) || sessionkey.IsScratchKey(key) {
		return ErrRefused
	}
	if sessionkey.IsTaskKey(key) && e.taskOver(key) {
		return ErrRefused
	}
	if !dashboardSession(key) && !e.On(key) {
		return ErrRefused
	}
	return nil
}

// taskOver reports whether dispatched session key's task is over for a
// retry: put away, or marked done with its switch left to the default.
func (e *Engine) taskOver(key string) bool {
	if e.cfg.Sessions == nil {
		return false
	}
	m, ok := e.cfg.Sessions.Get(key)
	if !ok {
		return false
	}
	if m.Reap != nil {
		return true
	}
	return m.Mark != nil && task.State(m.Mark.State) == task.Done && !on(key, m, ok)
}

// dashboardSession reports whether key's replies are the dashboard's: a
// dashboard chat, a session the Planner dispatched, a taken-over CLI.
func dashboardSession(key string) bool {
	return strings.HasPrefix(key, sessionkey.DashboardPlatform+":") || sessionkey.IsTaskKey(key) || strings.HasPrefix(key, "local:")
}

// on is a session's switch: as the user or the Planner set it, else off for
// a dispatched session whose task is over (done, or stopped by the user),
// else as custody set it, else on for a session the Planner dispatched (a
// dispatch before custody existed included), else off.
func on(key string, m sidecar.Meta, ok bool) bool {
	dispatched := ok && sessionkey.IsTaskKey(key) && m.DispatchedByPlanner()
	switch {
	case !ok:
		return false
	case m.Custody != nil && m.Custody.By != ByAuto:
		return m.Custody.On
	case dispatched && m.Mark != nil && task.State(m.Mark.State) == task.Done:
		return false
	case m.Custody != nil:
		return m.Custody.On
	}
	return dispatched
}

// On reports whether key is in custody.
func (e *Engine) On(key string) bool {
	if e.cfg.Sessions == nil || e.eligible(key) != nil {
		return false
	}
	m, ok := e.cfg.Sessions.Get(key)
	return on(key, m, ok)
}

// Set turns key's custody on or off for by. The user turning it on also
// starts its lineage's count afresh and lifts a stop; the Planner's cannot,
// so it cannot talk its way past the limits.
func (e *Engine) Set(key string, enable bool, by string) error {
	if e.cfg.Sessions == nil {
		return ErrNoSession
	}
	if err := e.eligible(key); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.setLocked(key, enable, by)
}

func (e *Engine) setLocked(key string, enable bool, by string) error {
	now := e.now()
	if err := e.ensureLocked(key); err != nil {
		return err
	}
	changed := false
	if _, err := e.cfg.Sessions.Update(key, func(m *sidecar.Meta) bool {
		if m.Custody != nil && m.Custody.On == enable && by == ByAuto {
			return false
		}
		changed = m.Custody == nil || m.Custody.On != enable
		m.Custody = &sidecar.Custody{On: enable, By: by, At: now}
		return true
	}); err != nil {
		return err
	}
	if enable && by == ByUser {
		root := e.root(key)
		if err := e.ensureLocked(root); err != nil {
			return err
		}
		if _, err := e.cfg.Sessions.Update(root, func(m *sidecar.Meta) bool {
			if m.Chain == nil {
				return false
			}
			m.Chain = nil
			return true
		}); err != nil {
			return err
		}
	}
	// The user's own switch is the chip they clicked: only the Planner's and
	// custody's are noted in the session.
	if changed && by != ByUser {
		event := "off"
		if enable {
			event = "on"
		}
		e.record(key, note.CustodyData{Event: event, By: by})
	}
	return nil
}

// ensureLocked gives key a sidecar to hold custody's fields.
func (e *Engine) ensureLocked(key string) error {
	if _, ok := e.cfg.Sessions.Get(key); ok {
		return nil
	}
	return e.cfg.Sessions.Put(sidecar.Meta{Key: key})
}

// root is the top of key's lineage: up its sidecars' parents, short of the
// Planner, which dispatches lineages but is in none.
func (e *Engine) root(key string) string {
	cur := key
	for range maxLineage {
		m, ok := e.cfg.Sessions.Get(cur)
		if !ok || m.Parent == "" || m.Parent == cur || sessionkey.IsPlannerNSKey(m.Parent) {
			break
		}
		cur = m.Parent
	}
	return cur
}

func (e *Engine) chain(root string) sidecar.Chain {
	m, _ := e.cfg.Sessions.Get(root)
	if m.Chain == nil {
		return sidecar.Chain{}
	}
	c := *m.Chain
	c.Errors = maps.Clone(c.Errors)
	return c
}

func (e *Engine) saveChainLocked(root string, c sidecar.Chain) {
	if err := e.ensureLocked(root); err != nil {
		slog.Warn("custody: lineage count not saved", "key", root, "err", err)
		return
	}
	if _, err := e.cfg.Sessions.Update(root, func(m *sidecar.Meta) bool { m.Chain = &c; return true }); err != nil {
		slog.Warn("custody: lineage count not saved", "key", root, "err", err)
	}
}

// SetViewing records whether viewer (one dashboard connection) has key open
// in a visible, focused tab. A report holds for ViewFor, so the dashboard
// renews it while the tab stays on key; the user is at key while any viewer
// is.
func (e *Engine) SetViewing(key, viewer string, viewing bool) {
	e.mu.Lock()
	if viewing {
		if e.viewing[key] == nil {
			e.viewing[key] = map[string]time.Time{}
		}
		e.viewing[key][viewer] = e.now()
	} else if v := e.viewing[key]; v != nil {
		delete(v, viewer)
	}
	left := e.pruneLocked()
	e.mu.Unlock()
	if !viewing || left {
		e.Poke()
	}
}

// DropViewer forgets viewer, for every session: its connection closed.
func (e *Engine) DropViewer(viewer string) {
	e.mu.Lock()
	for _, v := range e.viewing {
		delete(v, viewer)
	}
	e.pruneLocked()
	e.mu.Unlock()
	e.Poke()
}

// pruneLocked drops the viewers whose report lapsed and the notes of
// holding back of the sessions the user left; true when one was left.
func (e *Engine) pruneLocked() bool {
	now := e.now()
	for key, v := range e.viewing {
		for viewer, at := range v {
			if now.Sub(at) >= ViewFor {
				delete(v, viewer)
			}
		}
		if len(v) == 0 {
			delete(e.viewing, key)
		}
	}
	left := false
	for key := range e.deferred {
		if !e.nearbyLocked(key) {
			delete(e.deferred, key)
			left = true
		}
	}
	return left
}

// Nearby reports whether the user is at key: viewing it, or sent it a
// message within NearbyFor (planner/reap waits on it too).
func (e *Engine) Nearby(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nearbyLocked(key)
}

// nearbyLocked reports whether the user is at key: viewing it, or sent it a
// message within NearbyFor.
func (e *Engine) nearbyLocked(key string) bool {
	now := e.now()
	for _, at := range e.viewing[key] {
		if now.Sub(at) < ViewFor {
			return true
		}
	}
	if e.cfg.LastUser == nil {
		return false
	}
	at := e.cfg.LastUser(key)
	return !at.IsZero() && now.Sub(at) < NearbyFor
}

// Paused reports whether custody is paused, and until when: the one pause,
// the inbox's (暂停推送与托管).
func (e *Engine) Paused() (bool, time.Time) {
	until := e.until()
	if !e.now().Before(until) {
		return false, time.Time{}
	}
	return true, until
}

func (e *Engine) until() time.Time {
	if e.cfg.PausedUntil != nil {
		return e.cfg.PausedUntil()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pausedUntil
}

// paused reports whether the pause holds now. The caller holds mu.
func (e *Engine) paused() bool {
	if e.cfg.PausedUntil != nil {
		return e.now().Before(e.cfg.PausedUntil())
	}
	return e.now().Before(e.pausedUntil)
}

// Pause pauses custody and pushes for every session until until (a zero or
// past until resumes): /api/custody/pause, the inbox pause's alias. A turn
// running goes on, and a failure's retry due meanwhile waits for the pause
// to end (retryDue). It is kept across restarts.
func (e *Engine) Pause(until time.Time) error {
	e.pause(until)
	// Paused, the retries pending are held from now, with no tick before
	// the machine sleeps or naozhi stops; resumed, the ones due go out.
	e.holdRetries()
	e.Poke()
	return nil
}

// holdRetries marks each retry pending held while the pause holds; one
// waiting for a credential waits on as it is.
func (e *Engine) holdRetries() {
	if e.cfg.Sessions == nil || e.cfg.Disabled {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.paused() {
		return
	}
	for _, m := range e.cfg.Sessions.List() {
		if r := m.Retry; r != nil && !r.Done && r.Cred.IsZero() && !r.Held {
			next := *r
			next.Held = true
			e.setRetryLocked(m.Key, &next)
		}
	}
}

func (e *Engine) pause(until time.Time) {
	if e.cfg.Pause != nil {
		e.cfg.Pause(until)
		return
	}
	e.mu.Lock()
	e.pausedUntil = until
	e.mu.Unlock()
}

// Check returns nil when custody may send to key now, else why not: off,
// paused, stopped (with the reason), or the user nearby.
func (e *Engine) Check(key string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkLocked(key); err != nil {
		return err
	}
	if e.nearbyLocked(key) {
		return ErrNearby
	}
	return nil
}

// Allowed reports whether custody may send to key now.
func (e *Engine) Allowed(key string) bool { return e.Check(key) == nil }

// Blocked returns why custody would not send to key whatever becomes of its
// session: custody disabled, its switch turned off by the user or the
// Planner, paused, its lineage stopped, the user at the session, or the
// lineage at a limit, which stops it as Deliver would. A dispatched session
// put away (planner/reap) reads off and not local until its task is
// reopened, so Check cannot tell these from what reopening it would change;
// a message to it is weighed by Blocked before anything reopens it, as
// reopening sets its task back to 进行中 for a message Deliver would then
// refuse.
func (e *Engine) Blocked(key string) error {
	switch {
	case e.cfg.Disabled:
		return ErrDisabled
	case e.cfg.Sessions == nil || e.cfg.Send == nil:
		return ErrOff
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if m, ok := e.cfg.Sessions.Get(key); ok && m.Custody != nil && m.Custody.By != ByAuto && !m.Custody.On {
		return ErrOff
	}
	if err := e.retryCheckLocked(key); err != nil {
		return err
	}
	if e.nearbyLocked(key) {
		return ErrNearby
	}
	root := e.root(key)
	c := e.chain(root)
	if why := limit(c, e.progress(key)); why != "" {
		return e.stopLocked(root, key, c, why, false)
	}
	return nil
}

// checkLocked is Check short of the user being nearby.
func (e *Engine) checkLocked(key string) error {
	if !e.On(key) {
		return ErrOff
	}
	return e.retryCheckLocked(key)
}

// retryCheckLocked is checkLocked for a failure's retry, which does not
// need the switch on: paused or stopped.
func (e *Engine) retryCheckLocked(key string) error {
	if e.paused() {
		return ErrPaused
	}
	return e.stoppedLocked(key)
}

// stoppedLocked returns ErrStopped, with the reason, when key's lineage
// stopped. The caller holds mu.
func (e *Engine) stoppedLocked(key string) error {
	if c := e.chain(e.root(key)); c.Stopped != "" {
		return fmt.Errorf("%w：%s", ErrStopped, c.Stopped)
	}
	return nil
}

// Settled reports whether key's turn ended with nothing pending
// (Config.Settled); false without the source.
func (e *Engine) Settled(key string) bool {
	return e.cfg.Settled != nil && e.cfg.Settled(key)
}

// Name is how the user reads key.
func (e *Engine) Name(key string) string {
	if e.cfg.Name != nil {
		if n := e.cfg.Name(key); n != "" {
			return n
		}
	}
	if m, ok := e.cfg.Sessions.Get(key); ok && m.Title != "" {
		return m.Title
	}
	return key
}

// Deliver sends text to key as custody's message for reason (the text
// alone: the transcript entry carries the reason and the count) and returns
// its count over the lineage. It sends nothing while custody is off, paused
// or stopped, or while the user is at the session (ErrNearby; the header
// chip's title says so); and it stops the lineage, leaving it to the user,
// at MaxIdle messages in a row with no progress or MaxSends in all.
func (e *Engine) Deliver(key string, reason Reason, text string) (int, error) {
	if e.cfg.Sessions == nil || e.cfg.Send == nil {
		return 0, ErrOff
	}
	if err := e.eligible(key); err != nil {
		return 0, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deliverLocked(key, reason, text, "")
}

// deliverLocked is Deliver; class names the error class of the failure a
// retry answers. A retry needs no switch on and is not held back for the
// user nearby: the failure is most likely the reply to their own last
// message. A stop leaves that failure to the user.
func (e *Engine) deliverLocked(key string, reason Reason, text, class string) (int, error) {
	if class != "" {
		if err := e.retryCheckLocked(key); err != nil {
			return 0, err
		}
	} else {
		if err := e.checkLocked(key); err != nil {
			return 0, err
		}
		if e.nearbyLocked(key) {
			e.deferred[key] = true
			return 0, ErrNearby
		}
	}
	root := e.root(key)
	c := e.chain(root)
	progress := e.progress(key)
	if why := limit(c, progress); why != "" {
		if class != "" {
			why = errStop(c, class, why)
		}
		return 0, e.stopLocked(root, key, c, why, class != "")
	}
	if progress != c.Progress {
		c.Idle = 0
	}
	n := c.Sent + 1
	if err := e.cfg.Send(key, strings.TrimSpace(text), reason, n); err != nil {
		return 0, err
	}
	c.Sent, c.Idle, c.Progress = n, c.Idle+1, progress
	e.saveChainLocked(root, c)
	delete(e.deferred, key)
	e.record(key, note.CustodyData{Event: "sent", Reason: string(reason), N: n})
	return n, nil
}

// progress is key's progress (Config.Progress).
func (e *Engine) progress(key string) string {
	if e.cfg.Progress == nil {
		return ""
	}
	return e.cfg.Progress(key)
}

// limit is why lineage c stops at custody's next message, "" when it may
// send: MaxSends in all, or MaxIdle in a row with no progress since.
func limit(c sidecar.Chain, progress string) string {
	switch {
	case c.Sent >= MaxSends:
		return fmt.Sprintf("托管已替你发了 %d 次，交还给你", MaxSends)
	case progress == c.Progress && c.Idle >= MaxIdle:
		return fmt.Sprintf("托管连续替你发了 %d 次都没推进，交还给你", MaxIdle)
	}
	return ""
}

// errStop is why lineage c stops at a failure of class, given limit's why:
// how many retries the class already got, or the limit when it got none.
func errStop(c sidecar.Chain, class, why string) string {
	if n := c.Errors[class] - 1; n > 0 {
		return fmt.Sprintf("托管重试了 %d 次还是出错（%s）。发「继续」再试，或换个说法", n, ClassLabel(class))
	}
	return why + "（出错：" + ClassLabel(class) + "）"
}

// stopLocked stops the lineage at root for why and leaves key to the user,
// once, so they know it is theirs again: as the failure custody gave up on
// when failed (the item the inbox opens for a failure), else as a question
// under custody:<root>.
func (e *Engine) stopLocked(root, key string, c sidecar.Chain, why string, failed bool) error {
	c.Stopped, c.StoppedAt = why, e.now()
	e.saveChainLocked(root, c)
	e.record(key, note.CustodyData{Event: "stopped", Reason: why})
	if e.cfg.Inbox != nil {
		it := notify.Item{Kind: notify.KindQuestion, Level: notify.LevelTimely, Unit: key,
			Source: "custody:" + root, Title: e.Name(key) + " 托管已停", Why: why}
		if failed {
			m, _ := e.cfg.Sessions.Get(key)
			it.Kind, it.Source, it.WS = notify.KindError, "failed:"+key, m.Workspace
		}
		e.cfg.Inbox.Add(it)
	}
	return fmt.Errorf("%w：%s", ErrStopped, why)
}

// leaveLocked leaves key's failure to the user: the item the inbox opens for
// a failure custody does not take (the same source, so one already open
// stands), why being the class's label and then why custody did not retry,
// or what the user does about the class (notify.FailHint).
func (e *Engine) leaveLocked(key, class, why string) {
	if e.cfg.Inbox == nil {
		return
	}
	if why == "" {
		why = notify.FailHint(health.ErrorClass(class))
	}
	if class != "" && class != string(health.ErrOther) {
		why = strings.TrimSuffix(ClassLabel(class)+"；"+why, "；")
	}
	m, _ := e.cfg.Sessions.Get(key)
	e.cfg.Inbox.Add(notify.Item{Kind: notify.KindError, Level: notify.LevelTimely, Unit: key, WS: m.Workspace,
		Source: "failed:" + key, Title: e.Name(key) + " 失败", Why: why})
}

// notRetried says why custody does not retry a failure it may not send for.
func notRetried(err error) string {
	if errors.Is(err, ErrPaused) {
		return "托管已暂停，没有自动重试"
	}
	return "托管已停，没有自动重试"
}

// credWhy is what a failure for an expired credential tells the user while
// it waits for the credential to be refreshed.
const credWhy = "刷新凭证（mwinit）后托管会替你发「继续」"

// Tick keeps the workflow switches (on for a session with a workflow
// running and no switch set, back to the default once its workflows are
// over), and answers the failures: a new one is classified and gets a retry
// scheduled (or is left to the user), a due retry is sent, and one whose
// session recovered meanwhile is dropped; and the cards waiting on the user
// are acted on once due (answerAsks). After the machine woke, the network
// errors' retries are due at once; a lineage the user sent a message to
// since it stopped starts afresh.
func (e *Engine) Tick() {
	if e.cfg.Sessions == nil || e.cfg.Disabled {
		return
	}
	if e.cfg.Workflows != nil {
		running := map[string]bool{}
		for _, key := range e.cfg.Workflows() {
			running[key] = true
			e.autoOn(key)
		}
		for _, m := range e.cfg.Sessions.List() {
			if !running[m.Key] && autoByWorkflow(m) {
				e.autoOff(m.Key)
			}
		}
	}
	if e.cfg.Woke != nil && e.cfg.Woke() {
		e.woke()
	}
	e.takenOver()
	e.mu.Lock()
	e.pruneLocked()
	e.mu.Unlock()
	if e.cfg.Health == nil {
		return
	}
	units := e.cfg.Health().Units
	for _, u := range units {
		if u.Kind == health.KindSession && e.retryEligible(u.ID) == nil {
			e.answer(u)
		}
	}
	e.answerAsks(units)
}

// woke makes the network errors' retries due now: the machine woke from
// sleep, and the network it lost with it is likely back.
func (e *Engine) woke() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for _, m := range e.cfg.Sessions.List() {
		if r := m.Retry; r != nil && !r.Done && r.Class == string(health.ErrNetwork) && r.At.After(now) {
			next := *r
			next.At = now
			e.setRetryLocked(m.Key, &next)
		}
	}
}

// takenOver starts afresh each stopped lineage the user sent a message to
// since it stopped: their message is progress (§15.9 ④), so the stop and
// the count go, the switch stays as it was.
func (e *Engine) takenOver() {
	if e.cfg.LastUser == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, m := range e.cfg.Sessions.List() {
		if e.eligible(m.Key) != nil {
			continue
		}
		root := e.root(m.Key)
		c := e.chain(root)
		if c.Stopped == "" || !e.cfg.LastUser(m.Key).After(c.StoppedAt) {
			continue
		}
		if _, err := e.cfg.Sessions.Update(root, func(m *sidecar.Meta) bool { m.Chain = nil; return true }); err != nil {
			slog.Warn("custody: a lineage the user took over not started afresh", "key", root, "err", err)
		}
	}
}

// autoOn turns custody on for key's running workflow unless someone set its
// switch; a cron run is one-shot and stays off.
func (e *Engine) autoOn(key string) {
	if sessionkey.IsCronKey(key) || e.eligible(key) != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if m, ok := e.cfg.Sessions.Get(key); ok && m.Custody != nil {
		return
	}
	if err := e.setLocked(key, true, ByAuto); err != nil {
		slog.Warn("custody: not turned on for a running workflow", "key", key, "err", err)
	}
}

// autoByWorkflow reports whether m's custody is on only for a workflow: set
// by custody itself on a session the Planner did not dispatch, with no
// retry of a failure due (the workflow's end must not drop one).
func autoByWorkflow(m sidecar.Meta) bool {
	c := m.Custody
	return c != nil && c.On && c.By == ByAuto && !sessionkey.IsTaskKey(m.Key) && (m.Retry == nil || m.Retry.Done)
}

// autoOff puts key back to the default, off, once its workflows are over:
// a session the user talks to directly is in custody only while one runs.
func (e *Engine) autoOff(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	changed := false
	if _, err := e.cfg.Sessions.Update(key, func(m *sidecar.Meta) bool {
		changed = autoByWorkflow(*m)
		if changed {
			m.Custody = nil
		}
		return changed
	}); err != nil {
		slog.Warn("custody: not turned off after its workflow", "key", key, "err", err)
		return
	}
	if changed {
		e.record(key, note.CustodyData{Event: "off", By: ByAuto})
	}
}

// failedAt names one failure: when the session was last active, so a retry
// that failed again is a new one even when health saw no transition.
func failedAt(u health.Unit) time.Time { return cmp.Or(u.LastActive, u.Since) }

// stale reports whether u's failure is older than staleAfter.
func (e *Engine) stale(u health.Unit) bool { return e.staleSince(failedAt(u)) }

// staleSince reports whether a failure at at is older than staleAfter.
func (e *Engine) staleSince(at time.Time) bool {
	return !at.IsZero() && e.now().Sub(at) > staleAfter
}

// settled reports whether u's turn ended normally: idle, or waiting on the
// user's answer.
func settled(u health.Unit) bool { return u.State == health.Idle || u.State == health.Waiting }

// answer handles unit u's failure, if any. A failure custody does not retry
// is left to the user here when the inbox may not have it: it sees only
// health's transitions, and a retry that failed again may be none. One
// older than staleAfter is left as it stands. A turn that ended normally
// since ends the failure (recovered).
func (e *Engine) answer(u health.Unit) {
	key := u.ID
	m, _ := e.cfg.Sessions.Get(key)
	r := m.Retry
	if u.State != health.Failed {
		switch {
		case r != nil && settled(u):
			e.recovered(key, *r)
		case r != nil && !r.Done:
			e.setRetry(key, nil)
		}
		return
	}
	id := failedAt(u)
	if r != nil && r.Failed.Equal(id) {
		if !r.Done {
			e.retryDue(key, *r)
		}
		return
	}
	if e.stale(u) {
		e.setRetry(key, &sidecar.Retry{Failed: id, Done: true})
		return
	}
	class, retry := Classify(u, e.failText(key))
	e.mu.Lock()
	defer e.mu.Unlock()
	next := &sidecar.Retry{Failed: id, Class: class, Done: true}
	defer func() { e.setRetryLocked(key, next) }()
	if !retry {
		if exp := e.credExpiry(class); !exp.IsZero() && e.stoppedLocked(key) == nil {
			// Covers claimed it: the user hears of it here, unless the
			// inbox reports it with the systemic fault it is part of.
			next.Cred, next.Done = exp, false
			if !e.inFault(key) {
				e.leaveLocked(key, class, credWhy)
			}
			return
		}
		if u.Since.Before(u.LastActive) {
			// Active again since health judged it failed (a retry ran):
			// no transition took this failure to the inbox.
			e.leaveLocked(key, class, "")
		}
		return
	}
	// Paused, the retry is scheduled all the same: it waits for the pause
	// to end (retryDue).
	if err := e.stoppedLocked(key); err != nil {
		e.leaveLocked(key, class, notRetried(err))
		return
	}
	root := e.root(key)
	c := e.chain(root)
	if c.Errors == nil {
		c.Errors = map[string]int{}
	}
	c.Errors[class]++
	n := c.Errors[class]
	why := ""
	if n >= MaxErrors {
		why = errStop(c, class, "")
	} else if l := limit(c, e.progress(key)); l != "" {
		// The retry would be stopped when due: stop now, not after the
		// backoff, and as the failure it is.
		why = errStop(c, class, l)
	}
	if why != "" {
		_ = e.stopLocked(root, key, c, why, true)
		return
	}
	e.saveChainLocked(root, c)
	next.At, next.Done, next.Held = e.now().Add(Backoff[n-1]), false, e.paused()
}

// recovered ends key's failure r, its turn having ended normally since: the
// retries of its class start afresh, so failures days apart, each recovered,
// never add up to a stop. A session out of custody had nothing sent but
// retries, so its whole count starts afresh too. A stopped lineage stays
// stopped: the user lifts that.
func (e *Engine) recovered(key string, r sidecar.Retry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setRetryLocked(key, nil)
	root := e.root(key)
	c := e.chain(root)
	if c.Stopped != "" || (c.Errors[r.Class] == 0 && c.Sent == 0) {
		return
	}
	if root == key && !e.On(key) {
		c = sidecar.Chain{}
	} else {
		delete(c.Errors, r.Class)
	}
	e.saveChainLocked(root, c)
}

// credExpiry is when the credential expires, for a failure of class that
// waits for its refresh: zero for any other class, or with no credential
// to watch.
func (e *Engine) credExpiry(class string) time.Time {
	if class != string(health.ErrCredentialExpired) || e.cfg.CredExpiry == nil {
		return time.Time{}
	}
	return e.cfg.CredExpiry()
}

// due reports whether retry r may go out now: past its backoff, or, for an
// expired credential, once the credential expires later than it did then,
// and not yet.
func (e *Engine) due(r sidecar.Retry) bool {
	now := e.now()
	if r.Cred.IsZero() {
		return !now.Before(r.At)
	}
	exp := e.credExpiry(r.Class)
	return exp.After(r.Cred) && exp.After(now)
}

// retryDue sends key's retry r once due. A lineage stopped since custody
// took the failure leaves it to the user at once. While custody is paused
// the retry waits, held, its backoff running on, and goes out once the
// pause ends (at once if due by then); a failure the pause held grown older
// than staleAfter is left to the user instead, during the pause or past it
// (no tick may have run at its end: the machine slept, naozhi stopped), as
// one custody first sees that old is: no one waits on its "继续" now. One
// waiting for a credential waits on, as the item it opened says, its
// "继续" due once custody may send again.
func (e *Engine) retryDue(key string, r sidecar.Retry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.stoppedLocked(key)
	switch stale := e.staleSince(r.Failed); {
	case err != nil:
	case e.paused():
		if !r.Cred.IsZero() {
			return
		}
		if !stale {
			if !r.Held {
				r.Held = true
				e.setRetryLocked(key, &r)
			}
			return
		}
		err = ErrPaused
	case r.Held && stale:
		err = ErrPaused
	}
	if err != nil {
		if !r.Cred.IsZero() {
			return
		}
		e.leaveLocked(key, r.Class, notRetried(err))
		r.Done = true
		e.setRetryLocked(key, &r)
		return
	}
	if !e.due(r) {
		return
	}
	reason := ReasonRetry
	if !r.Cred.IsZero() {
		reason = ReasonCredential
	}
	switch _, err := e.deliverLocked(key, reason, RetryText, r.Class); {
	case err == nil, errors.Is(err, ErrStopped):
		r.Done = true
		e.setRetryLocked(key, &r)
	default:
		// The send refused: tried again next tick.
	}
}

func (e *Engine) failText(key string) string {
	if e.cfg.FailText == nil {
		return ""
	}
	return e.cfg.FailText(key)
}

func (e *Engine) setRetry(key string, r *sidecar.Retry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setRetryLocked(key, r)
}

func (e *Engine) setRetryLocked(key string, r *sidecar.Retry) {
	if err := e.ensureLocked(key); err != nil {
		slog.Warn("custody: retry not saved", "key", key, "err", err)
		return
	}
	if _, err := e.cfg.Sessions.Update(key, func(m *sidecar.Meta) bool { m.Retry = r; return true }); err != nil {
		slog.Warn("custody: retry not saved", "key", key, "err", err)
	}
}

// Covers reports whether custody takes unit u's failure, so the inbox opens
// no item for it: one custody will retry (custody opens the item when it
// gives up), and one for an expired credential, which waits for its
// refresh (custody opens the item that says so). Every local session's
// failures are custody's, its switch on or off, paused too: the retry waits
// for the pause to end.
func (e *Engine) Covers(u health.Unit) bool {
	if e.cfg.Sessions == nil || u.Kind != health.KindSession || u.State != health.Failed || e.retryEligible(u.ID) != nil {
		return false
	}
	class, retry := Classify(u, e.failText(u.ID))
	if !retry && e.credExpiry(class).IsZero() {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stoppedLocked(u.ID) != nil {
		return false
	}
	if m, _ := e.cfg.Sessions.Get(u.ID); m.Retry != nil && m.Retry.Failed.Equal(failedAt(u)) {
		return !m.Retry.Done
	}
	if e.stale(u) {
		return false
	}
	if !retry {
		return true
	}
	c := e.chain(e.root(u.ID))
	return c.Errors[class]+1 < MaxErrors && limit(c, e.progress(u.ID)) == ""
}

// Error classes custody tells apart beyond health's: a server-side error
// (5xx, overloaded), a timeout (an API timeout, the watchdog's kill) and a
// crash (the process exited abnormally with nothing naming why).
const (
	ClassServer  = "server"
	ClassTimeout = "timeout"
	ClassCrash   = "crash"
)

// serverRe finds a 5xx status in an API error.
var serverRe = regexp.MustCompile(`\b5(0[0-4]|29)\b`)

// serverPhrases name a server-side error without a status.
var serverPhrases = []string{"internal server error", "internalserverexception", "service unavailable", "serviceunavailable",
	"bad gateway", "gateway timeout", "overloaded"}

// Classify returns u's failure's error class and whether custody retries it:
// network, region, a server error, a timeout or a crash (§5.4 D93: an
// abnormal exit goes on with --resume) are retried; auth, an expired
// credential, throttling (429), a refusal, a full disk, an explicit denial
// and anything else unrecognised are the user's. text is the failure's text,
// read for what health does not classify.
func Classify(u health.Unit, text string) (string, bool) {
	switch u.ErrClass {
	case health.ErrNetwork, health.ErrRegion:
		return string(u.ErrClass), true
	case health.ErrNone, health.ErrOther:
	default:
		return string(u.ErrClass), false
	}
	if u.Evidence == health.DeathEvidence+cliinfo.DeathReasonNoOutputTimeout || u.Evidence == health.DeathEvidence+cliinfo.DeathReasonTotalTimeout {
		return ClassTimeout, true
	}
	if k, ok := apierr.ClassifyError(text); ok {
		switch k {
		case apierr.KindOverloaded:
			return ClassServer, true
		case apierr.KindTimeout:
			return ClassTimeout, true
		}
	}
	lower := strings.ToLower(text)
	for _, p := range serverPhrases {
		if strings.Contains(lower, p) {
			return ClassServer, true
		}
	}
	if apierr.IsEnvelope(text) && serverRe.MatchString(lower) {
		return ClassServer, true
	}
	if strings.HasPrefix(u.Evidence, health.DeathEvidence) {
		return ClassCrash, true
	}
	return cmp.Or(string(u.ErrClass), string(health.ErrOther)), false
}

// irreversibleRe names what cannot be undone once custody answers for the
// user (D93): pushing, merging, releasing or tagging, sending outside,
// deleting a remote, a branch or a resource, deploying and paying. It also
// takes the everyday ways of saying them (推送, 发布, 上线, 合入, 删掉分支,
// publish): a word too many leaves a card to the user, a word too few lets
// custody answer what cannot be undone.
var irreversibleRe = regexp.MustCompile(`(?i)\b(push|merge|release|deploy|publish)|推送|强推|合并|合入|发版|发布|上线|打\s*tag|git\s+tag|打标签|发.{0,6}(邮件|消息)|对外发送|删[除掉].{0,6}(远程|远端|分支|资源)|部署|付款|支付`)

// Irreversible reports whether text (a question, an option, what a session
// asks for) touches what cannot be undone: custody never answers that for
// the user, the Planner included.
func Irreversible(text string) bool { return irreversibleRe.MatchString(text) }

// ClassLabel is an error class as the user reads it.
func ClassLabel(class string) string {
	switch class {
	case ClassServer:
		return "服务端错误"
	case ClassTimeout:
		return "超时"
	case ClassCrash:
		return "进程异常退出"
	}
	return notify.ClassLabel(health.ErrorClass(class))
}

// Status is key's custody. A lineage stopped by failures that kept coming
// reads stopped with the switch off too: custody retried for the user.
func (e *Engine) Status(key string) Status {
	st := Status{Key: key, State: StateOff, Max: MaxSends}
	if e.cfg.Sessions == nil || e.eligible(key) != nil {
		return st
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.cfg.Sessions.Get(key)
	st.On = on(key, m, ok)
	if m.Custody != nil {
		st.By, st.At = m.Custody.By, m.Custody.At
	}
	c := e.chain(e.root(key))
	st.Sent = c.Sent
	switch {
	case c.Stopped != "":
		st.State, st.Why = StateStopped, c.Stopped
	case !st.On:
	case e.paused():
		st.State = StatePaused
	case e.nearbyLocked(key):
		st.State = StateDeferred
	default:
		st.State = StateOn
	}
	return st
}

// List is the custody of every local session in custody, or stopped.
func (e *Engine) List() []Status {
	if e.cfg.Sessions == nil {
		return nil
	}
	var out []Status
	for _, m := range e.cfg.Sessions.List() {
		if e.eligible(m.Key) != nil {
			continue
		}
		if st := e.Status(m.Key); st.On || st.State == StateStopped {
			out = append(out, st)
		}
	}
	return out
}

// record journals what custody did on unit ("" for custody as a whole).
func (e *Engine) record(unit string, d note.CustodyData) {
	if e.cfg.Journal == nil {
		return
	}
	data, err := json.Marshal(d)
	if err == nil {
		err = e.cfg.Journal.Append(journal.Entry{TS: e.now(), Kind: journal.KindCustody, Unit: unit, Data: data})
	}
	if err != nil {
		slog.Warn("custody: not journaled", "unit", unit, "err", err)
	}
}
