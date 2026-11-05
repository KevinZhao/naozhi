// Package promise keeps what the Planner promised the user (a "任务" of the
// Planner page, docs/rfc/planner.md §3.6, §4.2, §15.4): "明早提醒你", "CI 绿了
// 告诉你". A promise is registered by the Planner's promise action block or
// by the user and reminds once when its due time arrives. A pure reminder
// (only a due time) is done once it reminded; any other stays open until the
// Planner judges its condition met (its status action block) or the user
// closes it, and a closed one can be reopened (D85: marked automatically,
// undoable, recorded).
//
// State is promises.json in the Planner state root, written atomically.
package promise

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
	"github.com/naozhi/naozhi/internal/planner/journal"
	"github.com/naozhi/naozhi/internal/textutil"
)

// Version is the promises.json format this build reads and writes.
const Version = 1

// Source is who registered a promise. Values are wire-stable.
type Source string

const (
	SourcePlanner Source = "planner"
	SourceUser    Source = "user" // 你
	// SourceAuto closes a pure reminder once it reminded; it never
	// registers one.
	SourceAuto Source = "auto"
)

// State is a promise's lifecycle.
type State string

const (
	StateOpen   State = "open"
	StateClosed State = "closed"
)

// Promise is one promise.
type Promise struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Due is when to remind; zero for a promise with only a close condition.
	Due time.Time `json:"due,omitzero"`
	// CloseWhen is the condition it is done on, in words.
	CloseWhen string    `json:"close_when,omitempty"`
	State     State     `json:"state"`
	Created   time.Time `json:"created"`
	ClosedAt  time.Time `json:"closed_at,omitzero"`
	// ClosedBy is who closed it ("" reads as the user), Why their reason.
	ClosedBy Source `json:"closed_by,omitempty"`
	Why      string `json:"why,omitempty"`
	Source   Source `json:"source"`
	// RemindedAt is when the due reminder went out, so it goes out once
	// across restarts.
	RemindedAt time.Time `json:"reminded_at,omitzero"`
	// Task is the dispatched session the Planner made it for in the reply
	// that dispatched it, before it could know the session's key.
	Task string `json:"task,omitempty"`
}

// Cites reports whether p waits on dispatched session key: its Task, or the
// key in its text or close condition ("task:ab12 做完后告诉用户结果").
func (p Promise) Cites(key string) bool {
	return p.Task == key || strings.Contains(p.Text, key) || strings.Contains(p.CloseWhen, key)
}

// titleLeads are the leads the Planner words a promise with as if about the
// user ("提醒用户提交周报"); Title drops them.
var titleLeads = []string{"提醒用户", "提醒您", "提醒你", "帮用户", "替用户", "告诉用户", "通知用户"}

// Title is p's text as the user reads it in 任务 and in its reminder: the
// thing itself ("提交周报"), with a lead in titleLeads and the punctuation
// after it dropped. The prompt asks for that wording; this holds when the
// model does not. A text that is only a lead stays as it is.
func (p Promise) Title() string {
	text := strings.TrimSpace(p.Text)
	for _, lead := range titleLeads {
		if rest, ok := strings.CutPrefix(text, lead); ok {
			if rest = strings.TrimLeftFunc(rest, dropsAfterLead); rest != "" {
				return rest
			}
			break
		}
	}
	return text
}

func dropsAfterLead(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) }

// Reminder takes promises to the user: the "等你" push and the Planner
// conversation.
type Reminder interface {
	// Due is called once when p's due time arrives.
	Due(p Promise)
	// Closed is called when the Planner or the user closes p; a pure
	// reminder done by its reminder keeps what the reminder opened.
	Closed(p Promise)
}

// Errors the API returns.
var (
	ErrNotFound = errors.New("promise: no such promise")
	ErrClosed   = errors.New("promise: already closed")
	ErrInvalid  = errors.New("promise: text is empty or too long")
	ErrFull     = errors.New("promise: too many open promises")
)

// Limits.
const (
	textRunes = 500
	// maxOpen bounds the open promises, so a looping Planner cannot fill
	// the file.
	maxOpen = 200
	// Closed promises are kept this long, at most keepClosed of them.
	closedRetention = 30 * 24 * time.Hour
	keepClosed      = 200
	maxBytes        = 4 << 20
	// interval is how often due times are checked.
	interval = time.Minute
)

// Config wires a Store.
type Config struct {
	// Path is promises.json; "" keeps promises in memory.
	Path    string
	Journal *journal.Journal
	// Now is the wall clock; nil is time.Now.
	Now func() time.Time
}

// Store holds the promises. Safe for concurrent use.
type Store struct {
	cfg Config

	mu       sync.Mutex
	items    []Promise
	readOnly bool // promises.json is on disk but unusable: do not overwrite it
	reminder Reminder
}

type file struct {
	V        int       `json:"v"`
	Promises []Promise `json:"promises"`
}

// New loads the promises from cfg.Path.
func New(cfg Config) *Store {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Store{cfg: cfg}
	if cfg.Path == "" {
		return s
	}
	f, out, err := jsonfile.Load[file](cfg.Path, jsonfile.Options{MaxBytes: maxBytes, Label: "planner promises"})
	switch {
	case err != nil:
		slog.Warn("planner promises unreadable; changes kept in memory only", "err", err)
		s.readOnly = true
	case out == jsonfile.Parsed && f.V > Version:
		slog.Warn("planner promises written by a newer naozhi; not overwriting them", "version", f.V)
		s.readOnly = true
	case out == jsonfile.Parsed:
		s.items = f.Promises
	}
	return s
}

// SetReminder binds the reminder exits. Call it before Run.
func (s *Store) SetReminder(r Reminder) {
	s.mu.Lock()
	s.reminder = r
	s.mu.Unlock()
}

// ParseDue reads an action block's due: RFC 3339, or "" for none.
func ParseDue(v string) (time.Time, error) {
	if v = strings.TrimSpace(v); v == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, v)
}

// Add registers p, filling in ID, State and Created; Source defaults to the
// Planner.
func (s *Store) Add(p Promise) (Promise, error) {
	p.Text = strings.TrimSpace(p.Text)
	p.CloseWhen = strings.TrimSpace(p.CloseWhen)
	if p.Text == "" || utf8.RuneCountInString(p.Text) > textRunes || utf8.RuneCountInString(p.CloseWhen) > textRunes {
		return Promise{}, ErrInvalid
	}
	p.Source = cmp.Or(p.Source, SourcePlanner)
	if p.Source != SourcePlanner && p.Source != SourceUser {
		return Promise{}, ErrInvalid
	}
	now := s.cfg.Now()
	s.mu.Lock()
	if s.openLocked() >= maxOpen {
		s.mu.Unlock()
		return Promise{}, ErrFull
	}
	p.ID, p.State, p.Created = newID(), StateOpen, now
	p.ClosedAt, p.RemindedAt, p.ClosedBy, p.Why = time.Time{}, time.Time{}, "", ""
	s.items = append(s.items, p)
	s.saveLocked(now)
	s.mu.Unlock()
	s.record(p, "promise_open")
	return p, nil
}

// List returns the open promises, the earliest due first and undated ones
// last, and up to limit closed ones, most recently closed first.
func (s *Store) List(limit int) (open, closed []Promise) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.items {
		if p.State == StateOpen {
			open = append(open, p)
		} else {
			closed = append(closed, p)
		}
	}
	slices.SortStableFunc(open, byDue)
	slices.SortStableFunc(closed, func(a, b Promise) int { return b.ClosedAt.Compare(a.ClosedAt) })
	if len(closed) > limit {
		closed = closed[:limit]
	}
	return open, closed
}

func byDue(a, b Promise) int {
	if a.Due.IsZero() != b.Due.IsZero() {
		if a.Due.IsZero() {
			return 1
		}
		return -1
	}
	return cmp.Or(a.Due.Compare(b.Due), a.Created.Compare(b.Created))
}

// Get returns the promise id.
func (s *Store) Get(id string) (Promise, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.items, func(p Promise) bool { return p.ID == id })
	if i < 0 {
		return Promise{}, false
	}
	return s.items[i], true
}

// Close closes an open promise: by is the Planner (its condition is met) or
// the user, why the reason recorded with it.
func (s *Store) Close(id string, by Source, why string) error {
	if by != SourceUser && by != SourcePlanner {
		return ErrInvalid
	}
	now := s.cfg.Now()
	s.mu.Lock()
	i := slices.IndexFunc(s.items, func(p Promise) bool { return p.ID == id })
	switch {
	case i < 0:
		s.mu.Unlock()
		return ErrNotFound
	case s.items[i].State != StateOpen:
		s.mu.Unlock()
		return ErrClosed
	}
	s.items[i].State, s.items[i].ClosedAt = StateClosed, now
	s.items[i].ClosedBy, s.items[i].Why = by, textutil.TruncateRunes(strings.TrimSpace(why), textRunes)
	p, r := s.items[i], s.reminder
	s.saveLocked(now)
	s.mu.Unlock()
	s.record(p, "promise_close")
	if r != nil {
		r.Closed(p)
	}
	return nil
}

// Reopen opens a closed promise again; it is not reminded of again. An open
// one is left as it is.
func (s *Store) Reopen(id string) error {
	now := s.cfg.Now()
	s.mu.Lock()
	i := slices.IndexFunc(s.items, func(p Promise) bool { return p.ID == id })
	if i < 0 {
		s.mu.Unlock()
		return ErrNotFound
	}
	if s.items[i].State == StateOpen {
		s.mu.Unlock()
		return nil
	}
	if s.openLocked() >= maxOpen {
		s.mu.Unlock()
		return ErrFull
	}
	p := &s.items[i]
	p.State, p.ClosedAt, p.ClosedBy, p.Why = StateOpen, time.Time{}, "", ""
	reopened := *p
	s.saveLocked(now)
	s.mu.Unlock()
	s.record(reopened, "promise_reopen")
	return nil
}

// reminderOnly reports whether p is a pure reminder: a due time and no
// condition beyond having reminded. The Planner words it freely ("已提醒用户",
// "提醒你即可。"), so who is reminded and the punctuation are dropped before
// matching; anything else left over is a condition.
func reminderOnly(p Promise) bool {
	if p.Due.IsZero() {
		return false
	}
	switch reminderWords.Replace(strings.Map(dropPunct, p.CloseWhen)) {
	case "", "已提醒", "提醒过", "提醒后", "提醒即可", "提醒到", "到点提醒":
		return true
	}
	return false
}

// reminderWords are who a reminder goes to, dropped by reminderOnly.
var reminderWords = strings.NewReplacer("用户", "", "你", "", "我", "")

func dropPunct(r rune) rune {
	if unicode.IsPunct(r) || unicode.IsSpace(r) || unicode.IsSymbol(r) {
		return -1
	}
	return r
}

// Run checks due times every interval until ctx ends.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.safeCheck()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Store) safeCheck() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("promise: check panicked", "panic", r)
		}
	}()
	s.Check()
}

// Check reminds of the open promises whose due time arrived and that were
// not reminded of yet. Without a reminder nothing is marked, so the first
// check after one is bound still reminds.
func (s *Store) Check() {
	now := s.cfg.Now()
	s.mu.Lock()
	r := s.reminder
	var due []Promise
	if r != nil {
		for _, p := range s.items {
			if p.State == StateOpen && !p.Due.IsZero() && !now.Before(p.Due) && p.RemindedAt.IsZero() {
				due = append(due, p)
			}
		}
	}
	s.mu.Unlock()
	if len(due) == 0 {
		return
	}
	slices.SortStableFunc(due, byDue)
	// Remind before marking: a crash in between reminds again on restart,
	// and the inbox keeps one open item per promise.
	for _, p := range due {
		r.Due(p)
		s.record(p, "promise_due")
	}
	var closed, done []Promise
	s.mu.Lock()
	for _, d := range due {
		i := slices.IndexFunc(s.items, func(p Promise) bool { return p.ID == d.ID })
		if i < 0 {
			continue
		}
		p := &s.items[i]
		p.RemindedAt = now
		switch {
		case p.State == StateClosed:
			closed = append(closed, *p)
		case reminderOnly(*p):
			p.State, p.ClosedAt, p.ClosedBy, p.Why = StateClosed, now, SourceAuto, "到点提醒后自动完成"
			done = append(done, *p)
		}
	}
	s.saveLocked(now)
	s.mu.Unlock()
	for _, p := range done {
		s.record(p, "promise_close")
	}
	// Closed while being reminded of: settle the reminder it just got.
	for _, p := range closed {
		r.Closed(p)
	}
}

func (s *Store) openLocked() int {
	c := 0
	for _, p := range s.items {
		if p.State == StateOpen {
			c++
		}
	}
	return c
}

// saveLocked drops closed promises past retention, then the earliest closed
// beyond keepClosed, and writes promises.json.
func (s *Store) saveLocked(now time.Time) {
	s.items = slices.DeleteFunc(s.items, func(p Promise) bool {
		return p.State == StateClosed && now.Sub(p.ClosedAt) > closedRetention
	})
	excess := len(s.items) - s.openLocked() - keepClosed
	s.items = slices.DeleteFunc(s.items, func(p Promise) bool {
		if p.State == StateClosed && excess > 0 {
			excess--
			return true
		}
		return false
	})
	if s.readOnly || s.cfg.Path == "" {
		return
	}
	data, err := json.MarshalIndent(file{V: Version, Promises: s.items}, "", "  ")
	if err == nil {
		err = osutil.WriteFileAtomic(s.cfg.Path, data, 0o600)
	}
	if err != nil {
		slog.Warn("planner promises save failed", "err", err)
	}
}

// journalData is a promise's notify journal entry.
type journalData struct {
	Event  string    `json:"event"` // promise_open | promise_close | promise_reopen | promise_due
	ID     string    `json:"id"`
	Source Source    `json:"source,omitempty"`
	Due    time.Time `json:"due,omitzero"`
	By     Source    `json:"by,omitempty"` // who closed it
	Why    string    `json:"why,omitempty"`
}

func (s *Store) record(p Promise, event string) {
	if s.cfg.Journal == nil {
		return
	}
	data, err := json.Marshal(journalData{Event: event, ID: p.ID, Source: p.Source, Due: p.Due, By: p.ClosedBy, Why: p.Why})
	if err == nil {
		err = s.cfg.Journal.Append(journal.Entry{TS: s.cfg.Now(), Kind: journal.KindNotify, Unit: "promise:" + p.ID, Data: data})
	}
	if err != nil {
		slog.Warn("promise: journal append failed", "event", event, "err", err)
	}
}

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func newID() string {
	b := []byte("pr_")
	for range 10 {
		b = append(b, idAlphabet[rand.IntN(len(idAlphabet))])
	}
	return string(b)
}
