// Package custody is the gateway watching sessions for the user
// (docs/rfc/planner.md §5.4, narrowed in v0.5.0 by D93). It only decides
// whether to reply for the user; it never stops a session's own actions.
//
// A session's custody is a switch in its sidecar: on by default for a session
// the Planner dispatched and for one with a workflow running, off for the
// rest; the user flips it from the session header, the Planner with its
// custody action block. Every message custody sends goes through Deliver: it
// carries the prefix "⚙ 托管 · <reason> · 第 n/8 次", is counted against the
// lineage's limits (3 in a row with no progress, 8 in all), is held back while
// the user is at the session (viewing it, or sent it a message in the last
// NearbyFor), and none goes out while custody is paused. A lineage is a
// session and its sidecar descendants; each dispatch starts its own (the
// Planner is in none), and the Planner is told not to dispatch around a
// stopped one. Custody runs only with the Planner on.
//
// One trigger lives here (L0, deterministic): a session whose health turned
// failed with a retryable error class gets "继续" after a backoff, and is left
// to the user after MaxErrors failures of a class. A turn that ended normally
// goes to the Planner (planner/action's receipts); nothing here reads a
// reply's text or calls a model. State lives in the sidecars (the switch, and
// on a lineage's root what custody did) and in custody.json in the Planner
// state root (the pause), so a restart keeps it.
package custody

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/apierr"
	"github.com/naozhi/naozhi/internal/cliinfo"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/monitor/health"
	"github.com/naozhi/naozhi/internal/notify"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/planner/note"
	"github.com/naozhi/naozhi/internal/planner/sidecar"
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
	// maxLineage bounds the walk up a lineage.
	maxLineage = 32
	// stateBytes caps custody.json.
	stateBytes = 4 << 10
)

// Backoff is the wait before the retry of a class's n-th failure.
var Backoff = [MaxErrors - 1]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}

// RetryText is what a retry says after its prefix.
const RetryText = "继续"

// Reason is why custody sends a message, as its prefix says it.
type Reason string

const (
	ReasonRetry     Reason = "出错重试"
	ReasonSuggested Reason = "按建议执行"
	ReasonPlanner   Reason = "Planner 继续"
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
	ErrDisabled = errors.New("Planner 没有开，托管不可用")
	// ErrIrreversible: what custody would answer for the user cannot be
	// undone (Irreversible), so it is the user's.
	ErrIrreversible = errors.New("涉及不可逆的操作，不替你回答")
)

// Prefix opens the n-th message custody sends over a lineage for reason.
func Prefix(reason Reason, n int) string {
	return fmt.Sprintf("⚙ 托管 · %s · 第 %d/%d 次", reason, n, MaxSends)
}

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
	// session whose process went away resumes (server.Server.SubmitCustody).
	Send    func(key, text string) error
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
	// Path is custody.json; "" keeps the pause in memory.
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
	mu       sync.Mutex
	paused   bool
	pausedAt time.Time
	// viewing are each session's viewers (dashboard connections), by when
	// they last reported it.
	viewing map[string]map[string]time.Time
	// deferred are the sessions whose holding back is noted, until custody
	// sends or the user leaves.
	deferred map[string]bool
}

// state is custody.json.
type state struct {
	V      int       `json:"v"`
	Paused bool      `json:"paused"`
	At     time.Time `json:"at,omitzero"`
}

// New returns an Engine with the pause custody.json kept.
func New(cfg Config) *Engine {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	e := &Engine{cfg: cfg, poke: make(chan struct{}, 1), viewing: map[string]map[string]time.Time{}, deferred: map[string]bool{}}
	if cfg.Path != "" {
		st, out, err := jsonfile.Load[state](cfg.Path, jsonfile.Options{MaxBytes: stateBytes, Label: "custody state", Corrupt: jsonfile.LeaveCorrupt})
		switch {
		case err != nil:
			slog.Warn("custody state unreadable; custody paused until it is fixed", "err", err)
			e.paused = true
		case out == jsonfile.Parsed:
			e.paused, e.pausedAt = st.Paused, st.At
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
// to answer.
func (e *Engine) HandleHealth(health.Event) { e.Poke() }

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

// on is a session's switch: as set, else on for a session the Planner
// dispatched (a dispatch before custody existed included), else off.
func on(key string, m sidecar.Meta, ok bool) bool {
	switch {
	case ok && m.Custody != nil:
		return m.Custody.On
	case ok && sessionkey.IsTaskKey(key) && m.DispatchedByPlanner():
		return true
	}
	return false
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
	var dropped *sidecar.Retry
	if _, err := e.cfg.Sessions.Update(key, func(m *sidecar.Meta) bool {
		if m.Custody != nil && m.Custody.On == enable && by == ByAuto {
			return false
		}
		changed = m.Custody == nil || m.Custody.On != enable
		m.Custody = &sidecar.Custody{On: enable, By: by, At: now}
		m.Retry, dropped = switchRetry(m.Retry, enable, changed)
		return true
	}); err != nil {
		return err
	}
	if dropped != nil {
		e.leaveLocked(key, dropped.Class, "托管已关，没有自动重试")
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
	if changed || by == ByUser {
		event := "off"
		if enable {
			event = "on"
		}
		e.record(key, note.CustodyData{Event: event, By: by})
	}
	return nil
}

// switchRetry is a session's retry r after its custody was switched:
// turning it off drops a retry still due (returned, for the failure to go to
// the user); turning it on from off forgets one already answered, so the
// failure is looked at afresh. A retry due stays when custody stays on.
func switchRetry(r *sidecar.Retry, enable, changed bool) (kept, dropped *sidecar.Retry) {
	switch {
	case r == nil:
		return nil, nil
	case !enable && !r.Done:
		done := *r
		done.Done = true
		return &done, r
	case enable && changed && r.Done:
		return nil, nil
	}
	return r, nil
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

// Paused reports whether custody is paused, and since when.
func (e *Engine) Paused() (bool, time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.paused, e.pausedAt
}

// SetPaused pauses or resumes custody for every session; a turn running
// goes on, and a failure whose retry was due is left to the user. It is
// kept across restarts.
func (e *Engine) SetPaused(paused bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	at := time.Time{}
	if paused {
		at = e.now()
	}
	if e.cfg.Path != "" {
		data, err := json.Marshal(state{V: 1, Paused: paused, At: at})
		if err != nil {
			return err
		}
		if err := datadir.EnsureDir(filepath.Dir(e.cfg.Path)); err != nil {
			return err
		}
		if err := osutil.WriteFileAtomic(e.cfg.Path, data, 0o600); err != nil {
			return err
		}
	}
	e.paused, e.pausedAt = paused, at
	event := "resume"
	if paused {
		event = "pause"
	}
	e.record("", note.CustodyData{Event: event})
	// Resumed, the retries due go out; paused, they go to the user.
	e.Poke()
	return nil
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

// checkLocked is Check short of the user being nearby.
func (e *Engine) checkLocked(key string) error {
	if !e.On(key) {
		return ErrOff
	}
	if e.paused {
		return ErrPaused
	}
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

// Deliver sends text to key as custody's message for reason, after the
// prefix, and returns its count over the lineage. It sends nothing while
// custody is off, paused or stopped, or while the user is at the session
// (ErrNearby, noted in the session once); and it stops the lineage, leaving
// it to the user, at MaxIdle messages in a row with no progress or MaxSends
// in all.
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
// retry answers, so a stop leaves that failure to the user.
func (e *Engine) deliverLocked(key string, reason Reason, text, class string) (int, error) {
	if err := e.checkLocked(key); err != nil {
		return 0, err
	}
	if e.nearbyLocked(key) {
		if !e.deferred[key] {
			e.deferred[key] = true
			e.record(key, note.CustodyData{Event: "deferred", Reason: "你在看"})
		}
		return 0, ErrNearby
	}
	root := e.root(key)
	c := e.chain(root)
	progress := e.progress(key)
	if why := limit(c, progress); why != "" {
		if class != "" {
			why += "（最近一次出错：" + ClassLabel(class) + "）"
		}
		return 0, e.stopLocked(root, key, c, why, class != "")
	}
	if progress != c.Progress {
		c.Idle = 0
	}
	n := c.Sent + 1
	if err := e.cfg.Send(key, Prefix(reason, n)+"\n"+strings.TrimSpace(text)); err != nil {
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
		return fmt.Sprintf("这条任务链已自动发了 %d 次", MaxSends)
	case progress == c.Progress && c.Idle >= MaxIdle:
		return fmt.Sprintf("连续 %d 次没有进展", MaxIdle)
	}
	return ""
}

// stopLocked stops the lineage at root for why and leaves key to the user,
// so they know it is theirs again: as the failure custody gave up on when
// failed (the item the inbox opens for a failure), else as a "托管已停"
// question under custody:<root>.
func (e *Engine) stopLocked(root, key string, c sidecar.Chain, why string, failed bool) error {
	c.Stopped, c.StoppedAt = why, e.now()
	e.saveChainLocked(root, c)
	e.record(key, note.CustodyData{Event: "stopped", Reason: why})
	switch {
	case failed:
		e.leaveLocked(key, "", why+"，等你接手")
	case e.cfg.Inbox != nil:
		e.cfg.Inbox.Add(notify.Item{Kind: notify.KindQuestion, Level: notify.LevelTimely, Unit: key,
			Source: "custody:" + root, Title: e.Name(key) + " 托管已停", Why: why + "，等你接手"})
	}
	return fmt.Errorf("%w：%s", ErrStopped, why)
}

// leaveLocked leaves key's failure to the user: the item the inbox opens for
// a failure custody does not take (the same source, so one already open
// stands), why being the class's label and then why custody did not retry.
func (e *Engine) leaveLocked(key, class, why string) {
	if e.cfg.Inbox == nil {
		return
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
	switch {
	case errors.Is(err, ErrPaused):
		return "托管已暂停，没有自动重试"
	case errors.Is(err, ErrStopped):
		return "托管已停，没有自动重试"
	}
	return "托管已关，没有自动重试"
}

// Tick keeps the workflow switches (on for a session with a workflow
// running and no switch set, back to the default once its workflows are
// over), and answers the failures: a new one is classified and gets a retry
// scheduled (or is left to the user), a due retry is sent, and one whose
// session recovered meanwhile is dropped.
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
	e.mu.Lock()
	e.pruneLocked()
	e.mu.Unlock()
	if e.cfg.Health == nil {
		return
	}
	for _, u := range e.cfg.Health().Units {
		if u.Kind == health.KindSession && e.eligible(u.ID) == nil {
			e.answer(u)
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

// answer handles unit u's failure, if any. A failure of a session in
// custody that custody does not retry is left to the user here when the
// inbox may not have it: it sees only health's transitions, and a retry
// that failed again may be none.
func (e *Engine) answer(u health.Unit) {
	key := u.ID
	m, _ := e.cfg.Sessions.Get(key)
	r := m.Retry
	if u.State != health.Failed {
		if r != nil && !r.Done {
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
	if !e.On(key) {
		return // not custody's: Covers did not claim it, the inbox has it
	}
	class, retry := Classify(u, e.failText(key))
	e.mu.Lock()
	defer e.mu.Unlock()
	next := &sidecar.Retry{Failed: id, Class: class, Done: true}
	defer func() { e.setRetryLocked(key, next) }()
	if !retry {
		if u.Since.Before(u.LastActive) {
			// Active again since health judged it failed (a retry ran):
			// no transition took this failure to the inbox.
			e.leaveLocked(key, class, "")
		}
		return
	}
	if err := e.checkLocked(key); err != nil {
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
		why = fmt.Sprintf("同一类错误累计 %d 次（%s）", n, ClassLabel(class))
	} else if l := limit(c, e.progress(key)); l != "" {
		// The retry would be stopped when due: stop now, not after the
		// backoff, and as the failure it is.
		why = l + "（最近一次出错：" + ClassLabel(class) + "）"
	}
	if why != "" {
		_ = e.stopLocked(root, key, c, why, true)
		return
	}
	e.saveChainLocked(root, c)
	next.At, next.Done = e.now().Add(Backoff[n-1]), false
	e.record(key, note.CustodyData{Event: "retry", Reason: ClassLabel(class), N: n})
}

// retryDue sends key's retry r once due. Custody turned off, paused or
// stopped since it took the failure leaves the failure to the user at once,
// not when custody may send again.
func (e *Engine) retryDue(key string, r sidecar.Retry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkLocked(key); err != nil {
		e.leaveLocked(key, r.Class, notRetried(err))
		r.Done = true
		e.setRetryLocked(key, &r)
		return
	}
	if e.now().Before(r.At) {
		return
	}
	switch _, err := e.deliverLocked(key, ReasonRetry, RetryText, r.Class); {
	case err == nil, errors.Is(err, ErrStopped):
		r.Done = true
		e.setRetryLocked(key, &r)
	default:
		// The user nearby, or the send refused: tried again next tick.
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

// Covers reports whether custody will retry unit u's failure, so it is not
// the user's yet: the inbox opens no item for it (custody opens one when it
// gives up).
func (e *Engine) Covers(u health.Unit) bool {
	if e.cfg.Sessions == nil || u.Kind != health.KindSession || u.State != health.Failed || e.eligible(u.ID) != nil {
		return false
	}
	class, retry := Classify(u, e.failText(u.ID))
	if !retry {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.checkLocked(u.ID) != nil {
		return false
	}
	if m, _ := e.cfg.Sessions.Get(u.ID); m.Retry != nil && m.Retry.Failed.Equal(failedAt(u)) {
		return !m.Retry.Done
	}
	c := e.chain(e.root(u.ID))
	return c.Errors[class]+1 < MaxErrors && limit(c, e.progress(u.ID)) == ""
}

// Error classes custody tells apart beyond health's: a server-side error
// (5xx, overloaded) and a timeout (an API timeout, the watchdog's kill).
const (
	ClassServer  = "server"
	ClassTimeout = "timeout"
)

// serverRe finds a 5xx status in an API error.
var serverRe = regexp.MustCompile(`\b5(0[0-4]|29)\b`)

// serverPhrases name a server-side error without a status.
var serverPhrases = []string{"internal server error", "internalserverexception", "service unavailable", "serviceunavailable",
	"bad gateway", "gateway timeout", "overloaded"}

// Classify returns u's failure's error class and whether custody retries it:
// network, region, a server error or a timeout are retried; auth, an
// expired credential, throttling (429), a refusal, a full disk, an explicit
// denial and anything unrecognised are the user's. text is the failure's
// text, read for what health does not classify.
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
	return cmp.Or(string(u.ErrClass), string(health.ErrOther)), false
}

// irreversibleRe names what cannot be undone once custody answers for the
// user (D93): pushing, merging, releasing or tagging, sending outside,
// deleting a remote, a branch or a resource, deploying and paying.
var irreversibleRe = regexp.MustCompile(`(?i)\b(push|merge|release|deploy)|合并|发版|打\s*tag|git\s+tag|发邮件|发消息|对外发送|删除远程|删除分支|删除资源|部署|付款|支付`)

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
	}
	return notify.ClassLabel(health.ErrorClass(class))
}

// Status is key's custody.
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
	case !st.On:
	case c.Stopped != "":
		st.State, st.Why = StateStopped, c.Stopped
	case e.paused:
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
		if st := e.Status(m.Key); st.On {
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
