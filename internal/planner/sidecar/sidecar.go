// Package sidecar keeps the Planner's per-session fields beside the session
// store, one file per session: <planner root>/sessions/<sha256(key)>.json
// (docs/rfc/planner.md §3.7, §15.1). sessions.json is never touched, so an
// older naozhi reading and rewriting it cannot drop these fields.
//
// v0.2.0 records the sessions the Planner dispatches (purpose "task", parent
// planner:main), and v0.2.1 their task state as the Planner or the user set
// it and the failure health last judged them in (§15.4). v0.5.0 adds custody
// (planner/custody): its switch on any session, and on a lineage's root what
// custody sent and why it stopped. v0.7.0 adds a session's watch switch and
// the last turn that ended with the user away (§15.12 ⑥). A file outlives its session by Retention, counted from when
// the session was first seen gone, so a retired session's lineage is still
// there to read for a while.
package sidecar

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// Version is the file format this build writes.
const Version = 1

// PurposeTask is a session the Planner dispatched (§0 purpose).
const PurposeTask = "task"

// PurposeChat is a session the user opened that the gateway observes
// (§15.12 ⑥). Derived for index.json and the digests, never written: such a
// session's sidecar, when its watch switch, a result or a mark needs one,
// keeps purpose "" as custody's does.
const PurposeChat = "chat"

// Retention is how long a file outlives its session.
const Retention = 30 * 24 * time.Hour

// sweepInterval is how often Run looks for retired sessions.
const sweepInterval = time.Hour

// maxBytes caps one file's read; it holds a title and a few IDs.
const maxBytes = 64 << 10

// Meta is one session's sidecar.
type Meta struct {
	V       int    `json:"v"`
	Key     string `json:"key"`
	Purpose string `json:"purpose"`
	// Parent is the key of the session that dispatched this one.
	Parent string `json:"parent,omitempty"`
	Title  string `json:"title,omitempty"`
	// Workspace is the workspace ID the session was dispatched into; Dir is
	// the directory it ran in.
	Workspace string `json:"workspace,omitempty"`
	Dir       string `json:"dir,omitempty"`
	// Model is the model the dispatch asked for ("" = the default). Dir and
	// Model let a session whose process went away respawn as dispatched.
	Model string `json:"model,omitempty"`
	// MCPTools are the write tools the dispatch allowed this session, by
	// server (D98), kept so a respawn gets them too.
	MCPTools map[string][]string `json:"mcp_tools,omitempty"`
	// Ultracode is the dispatch's ultracode, kept for a respawn.
	Ultracode bool `json:"ultracode,omitempty"`
	// ReportBlock is set on a session dispatched on the prompt that has it
	// report in a naozhi-report block (§15.12 ③): what it sends the Planner
	// by SendMessage is a question on the way, not a report. A session
	// dispatched before (unset) still reports by SendMessage.
	ReportBlock bool      `json:"report_block,omitempty"`
	Created     time.Time `json:"created"`
	// Retired is when the session was first seen gone from the router; the
	// file is removed Retention later.
	Retired time.Time `json:"retired,omitzero"`
	// Mark is the task state the Planner or the user last set; nil leaves
	// it to the session's health (planner/task).
	Mark *Mark `json:"mark,omitempty"`
	// Failure is the failure health last judged the session in, nil once it
	// recovered: a retired session keeps it (planner/task).
	Failure *Mark `json:"failure,omitempty"`
	// Turn is the session's last turn end as planner/action saw it, kept
	// on disk so a check a restart cut short runs again.
	Turn *Turn `json:"turn,omitempty"`
	// Custody is the session's custody switch; nil leaves it to the default
	// (planner/custody).
	Custody *Custody `json:"custody,omitempty"`
	// Chain is custody's count over the lineage this session is the root of.
	Chain *Chain `json:"chain,omitempty"`
	// Retry is custody's answer to the session's last failure.
	Retry *Retry `json:"retry,omitempty"`
	// Reap is set while the session, done, is put away (planner/reap): what
	// resuming it needs and what became of its worktrees. nil once reopened,
	// or once a session is back under the key without a reopen.
	Reap *Reap `json:"reap,omitempty"`
	// LeftWorktrees are the worktrees a put away left in place (waiting on
	// their PR, or with work not committed or pushed) or had removed and
	// checked out again, when a session came back under the key without a
	// reopen: planner/reap carries them into the next put away, which
	// removes them once they may go. Empty while Reap is set.
	LeftWorktrees []Worktree `json:"left_worktrees,omitempty"`
	// Report is the session's latest report (§15.12 ③), its conclusion
	// for the task card; the full text is in its digest (planner/digest).
	Report *Report `json:"report,omitempty"`
	// SeenAt is when the user last looked at the session's result in the
	// dashboard (§15.12 ④): opened the session, expanded its task card or
	// marked every new result read. A result later than it is new
	// (planner/task).
	SeenAt time.Time `json:"seen_at,omitzero"`
	// Watch is the session's watch switch (§15.12 ⑥, D104: the gateway
	// observing a session the user opened, its digest and its card in 任务);
	// nil leaves it to the default, on (planner/custody.Engine.Watched).
	Watch *Custody `json:"watch,omitempty"`
	// ResultAt is when a watched session's turn last ended with the user
	// away from it: a result for them (§15.12 ④ ⑥), new while later than
	// SeenAt.
	ResultAt time.Time `json:"result_at,omitzero"`
	// Taint is set once the session read outside content: web pages it
	// searched or fetched, a mail's body (§7.4, §15.12 ⑨). Sticky: the
	// content stays in its context and its event log. The Planner reads its
	// digest, marked, and never its event log (planner/agent's deny rules).
	Taint *Taint `json:"taint,omitempty"`
}

// Taint kinds.
const (
	TaintWeb   = "web"
	TaintEmail = "email"
)

// TaintOf is the kind of outside content a call of tool brings in, "" for
// none: the CLI's web tools and the web search MCP servers' tools, and
// reading a mail's body.
func TaintOf(tool string) string {
	switch {
	case tool == "WebFetch" || tool == "WebSearch",
		strings.HasPrefix(tool, "mcp__exa__"), strings.HasPrefix(tool, "mcp__agentcore-websearch__"):
		return TaintWeb
	case tool == "mcp__aws-outlook-mcp__email_read":
		return TaintEmail
	}
	return ""
}

// Taint is what outside content a session read: its kinds and the tools
// that brought it in, and when it was first seen.
type Taint struct {
	At    time.Time `json:"at"`
	Kinds []string  `json:"kinds"`
	Tools []string  `json:"tools,omitempty"`
}

// Report is a session's latest report: its state (done, blocked,
// need_user, progress), its summary and when.
type Report struct {
	State   string    `json:"state"`
	Summary string    `json:"summary"`
	At      time.Time `json:"at"`
}

// Reap is a done task's session put away: when, on whose mark (By, Why),
// the session to resume, and the worktrees it worked in.
type Reap struct {
	At         time.Time  `json:"at"`
	By         string     `json:"by,omitempty"`
	Why        string     `json:"why,omitempty"`
	SessionID  string     `json:"session_id,omitempty"`
	Workspace  string     `json:"workspace,omitempty"`
	LastPrompt string     `json:"last_prompt,omitempty"`
	Worktrees  []Worktree `json:"worktrees,omitempty"`
}

// Worktree is one git worktree a put-away session worked in: its directory,
// the repository's common git directory and the branch checked out, its
// State (planner/reap's) and why, and the reason the user was last told it
// stays (so they are told once per reason).
type Worktree struct {
	Dir      string    `json:"dir"`
	GitDir   string    `json:"git_dir"`
	Branch   string    `json:"branch,omitempty"`
	State    string    `json:"state"`
	Why      string    `json:"why,omitempty"`
	At       time.Time `json:"at"`
	Notified string    `json:"notified,omitempty"`
}

// Custody is a session's custody switch: on or off, set by whom (auto, user
// or planner) and when.
type Custody struct {
	On bool      `json:"on"`
	By string    `json:"by"`
	At time.Time `json:"at"`
}

// Chain is what custody did over one lineage: the messages it sent, those
// in a row with no progress after them, the failures per error class, the
// progress seen when it last sent, and why it stopped ("" while it runs).
type Chain struct {
	Sent      int            `json:"sent,omitempty"`
	Idle      int            `json:"idle,omitempty"`
	Errors    map[string]int `json:"errors,omitempty"`
	Progress  string         `json:"progress,omitempty"`
	Stopped   string         `json:"stopped,omitempty"`
	StoppedAt time.Time      `json:"stopped_at,omitzero"`
}

// Retry is custody's answer to one failure, named by when it happened
// (Failed): the "continue" due At for its error Class, or Done once sent or
// left to the user. A failure for an expired credential waits instead for
// the credential to expire later than Cred, its expiry then: refreshed.
// Held is set once the pause held the retry back: past the pause, one
// grown stale (custody's staleAfter) is left to the user, not sent.
type Retry struct {
	Failed time.Time `json:"failed"`
	Class  string    `json:"class,omitempty"`
	At     time.Time `json:"at,omitzero"`
	Cred   time.Time `json:"cred,omitzero"`
	Held   bool      `json:"held,omitempty"`
	Done   bool      `json:"done,omitempty"`
}

// Turn is a dispatched session's turn end. While Check is set the turn is
// still to be checked for a report to the Planner, one reaching it after
// From (the previous turn's end); Text is its reply, cut short, and
// ReportErr why the report block it ended with does not parse ("" for
// none), which the receipt of a turn left unreported says.
type Turn struct {
	Ended     time.Time `json:"ended"`
	Check     bool      `json:"check,omitempty"`
	From      time.Time `json:"from,omitzero"`
	Text      string    `json:"text,omitempty"`
	ReportErr string    `json:"report_err,omitempty"`
}

// Mark is a task state set by someone: planner/task's State and By values.
type Mark struct {
	State string    `json:"state"`
	By    string    `json:"by"`
	Why   string    `json:"why,omitempty"`
	At    time.Time `json:"at"`
}

// DispatchedByPlanner reports whether the global Planner dispatched the
// session.
func (m Meta) DispatchedByPlanner() bool { return sessionkey.IsPlannerNSKey(m.Parent) }

// Store is the loaded sidecar directory. Safe for concurrent use; reads are
// served from memory.
type Store struct {
	dir string
	now func() time.Time

	mu sync.Mutex
	m  map[string]Meta
	// newer holds the keys whose file a newer naozhi wrote: read, never
	// rewritten or removed, so the fields this build does not know survive.
	newer map[string]bool
}

// ErrNewer refuses a write to a sidecar a newer naozhi wrote: it keeps the
// fields this build does not know, so it is read and never rewritten.
var ErrNewer = errors.New("sidecar: written by a newer naozhi")

// Open loads every sidecar under dir. A file that cannot be read or parsed
// is left in place and skipped; one a newer naozhi wrote is read but never
// rewritten. An empty dir keeps the store in memory.
func Open(dir string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	s := &Store{dir: dir, now: now, m: map[string]Meta{}, newer: map[string]bool{}}
	if dir == "" {
		return s
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return s
	}
	for _, p := range names {
		m, out, err := jsonfile.Load[Meta](p, jsonfile.Options{MaxBytes: maxBytes, Label: "session sidecar", Corrupt: jsonfile.LeaveCorrupt})
		switch {
		case err != nil:
			slog.Warn("session sidecar unreadable; skipped", "path", p, "err", err)
		case out != jsonfile.Parsed:
		case m.Key == "" || filepath.Base(p) != fileName(m.Key):
			slog.Warn("session sidecar does not match its name; skipped", "path", p)
		case m.V > Version:
			slog.Warn("session sidecar written by a newer naozhi; not overwriting it", "path", p, "version", m.V)
			s.m[m.Key] = m
			s.newer[m.Key] = true
		default:
			s.m[m.Key] = m
		}
	}
	return s
}

func fileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".json"
}

func (s *Store) path(key string) string {
	if s.dir == "" {
		return ""
	}
	return filepath.Join(s.dir, fileName(key))
}

// Put records m, stamping its version and, when unset, its creation time.
func (s *Store) Put(m Meta) error {
	if m.Key == "" {
		return errors.New("sidecar: empty key")
	}
	m.V = Version
	if m.Created.IsZero() {
		m.Created = s.now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.newer[m.Key] {
		return ErrNewer
	}
	if err := s.writeLocked(m); err != nil {
		return err
	}
	s.m[m.Key] = m
	return nil
}

func (s *Store) writeLocked(m Meta) error {
	p := s.path(m.Key)
	if p == "" {
		return nil
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := datadir.EnsureDir(s.dir); err != nil {
		return err
	}
	return osutil.WriteFileAtomic(p, data, 0o600)
}

// Remove drops key's sidecar, unless a newer naozhi wrote it.
func (s *Store) Remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(key)
}

func (s *Store) removeLocked(key string) {
	if s.newer[key] {
		return
	}
	delete(s.m, key)
	if p := s.path(key); p != "" {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("session sidecar not removed", "path", p, "err", err)
		}
	}
}

// Get returns key's sidecar.
func (s *Store) Get(key string) (Meta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.m[key]
	return m, ok
}

// List returns every sidecar, in no order.
func (s *Store) List() []Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Meta, 0, len(s.m))
	for _, m := range s.m {
		out = append(out, m)
	}
	return out
}

// MarkTaint records that key read outside content of kinds through tools,
// giving it a sidecar when it has none; true when the mark grew.
func (s *Store) MarkTaint(key string, kinds, tools []string) (bool, error) {
	grew := false
	err := s.Upsert(key, func(m *Meta) bool {
		t := Taint{At: s.now()}
		if m.Taint != nil {
			t = *m.Taint
			t.Kinds, t.Tools = slices.Clone(t.Kinds), slices.Clone(t.Tools)
		}
		for _, k := range kinds {
			if !slices.Contains(t.Kinds, k) {
				t.Kinds, grew = append(t.Kinds, k), true
			}
		}
		for _, tool := range tools {
			if !slices.Contains(t.Tools, tool) && len(t.Tools) < maxTaintTools {
				t.Tools, grew = append(t.Tools, tool), true
			}
		}
		if grew {
			m.Taint = &t
		}
		return grew
	})
	return grew, err
}

// maxTaintTools caps the tools a taint mark names.
const maxTaintTools = 16

// Tainted lists the keys whose sidecar carries a taint mark, sorted.
func (s *Store) Tainted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k, m := range s.m {
		if m.Taint != nil {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// SetMark records key's task state; false when key has no sidecar.
func (s *Store) SetMark(key string, mark Mark) (bool, error) {
	return s.Update(key, func(m *Meta) bool { m.Mark = &mark; return true })
}

// SetFailure records the failure health judged key in, or that it has none
// (nil); false when key has no sidecar.
func (s *Store) SetFailure(key string, f *Mark) (bool, error) {
	return s.Update(key, func(m *Meta) bool {
		if f == nil && m.Failure == nil {
			return false
		}
		m.Failure = f
		return true
	})
}

// SetTurn records key's last turn end; false when key has no sidecar.
func (s *Store) SetTurn(key string, t Turn) (bool, error) {
	return s.Update(key, func(m *Meta) bool { m.Turn = &t; return true })
}

// Checked clears the check of key's turn that ended at ended, unless a
// later turn ended since.
func (s *Store) Checked(key string, ended time.Time) error {
	_, err := s.Update(key, func(m *Meta) bool {
		if m.Turn == nil || !m.Turn.Check || !m.Turn.Ended.Equal(ended) {
			return false
		}
		m.Turn = &Turn{Ended: m.Turn.Ended}
		return true
	})
	return err
}

// Update applies fn to key's sidecar and saves it when fn reports a change;
// false when key has no sidecar. fn runs under the store's lock.
func (s *Store) Update(key string, fn func(*Meta) bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.m[key]
	if !ok {
		return false, nil
	}
	if s.newer[key] {
		return true, ErrNewer
	}
	if !fn(&m) {
		return true, nil
	}
	if err := s.writeLocked(m); err != nil {
		return true, err
	}
	s.m[key] = m
	return true, nil
}

// Upsert applies fn to key's sidecar, giving it one first (purpose "")
// when it has none, and saves it when fn reports a change: a session the
// user opened has no sidecar until a field of it needs one. fn runs under
// the store's lock; one that changes nothing writes no new file.
func (s *Store) Upsert(key string, fn func(*Meta) bool) error {
	if key == "" {
		return errors.New("sidecar: empty key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.newer[key] {
		return ErrNewer
	}
	m, ok := s.m[key]
	if !ok {
		m = Meta{V: Version, Key: key, Created: s.now()}
	}
	if !fn(&m) {
		return nil
	}
	if err := s.writeLocked(m); err != nil {
		return err
	}
	s.m[key] = m
	return nil
}

// Ensure gives key a sidecar (purpose "") when it has none and leaves one it
// has alone, in one step under the store's lock. A Get then a Put would
// replace, with an empty one, the sidecar another writer's Upsert made in
// between: the user's sessions get theirs from writers that do not share
// the caller's lock (a result, a seen mark, the Planner's status).
func (s *Store) Ensure(key string) error {
	if key == "" {
		return errors.New("sidecar: empty key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; ok {
		return nil
	}
	m := Meta{V: Version, Key: key, Created: s.now()}
	if err := s.writeLocked(m); err != nil {
		return err
	}
	s.m[key] = m
	return nil
}

// baselineFile marks a store directory whose sidecars got their seen_at
// baseline (BaselineSeen). Not *.json: Open does not read it as a sidecar.
const baselineFile = "seen-baseline"

// BaselineSeen gives, once per store directory, every sidecar with no
// seen_at the time now: they were written before naozhi kept one (v0.7.0
// and earlier, or by v0.7.0 itself, which shipped without this baseline),
// so a result they already had is no new one for the user, while one after
// the upgrade still is. A marker file keeps it to the first start: a
// sidecar written since with no seen_at was never seen, and a later start
// must not mark it so. A newer naozhi's file is left alone. Returns how
// many it set; a failed write leaves the marker unwritten, so the next
// start tries again.
func (s *Store) BaselineSeen() (int, error) {
	if s.dir == "" {
		return 0, nil
	}
	marker := filepath.Join(s.dir, baselineFile)
	if _, err := os.Stat(marker); err == nil {
		return 0, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	var errs []error
	for key, m := range s.m {
		if s.newer[key] || !m.SeenAt.IsZero() {
			continue
		}
		m.SeenAt = now
		if err := s.writeLocked(m); err != nil {
			errs = append(errs, err)
			continue
		}
		s.m[key] = m
		n++
	}
	if len(errs) > 0 {
		return n, errors.Join(errs...)
	}
	if err := datadir.EnsureDir(s.dir); err != nil {
		return n, err
	}
	return n, osutil.WriteFileAtomic(marker, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// Purpose is key's purpose and the title it was dispatched with (both ""
// without a sidecar).
func (s *Store) Purpose(key string) (purpose, title string) {
	m, _ := s.Get(key)
	return m.Purpose, m.Title
}

// Sweep marks the sidecars whose session live no longer reports as retired,
// clears the mark of one that came back, and removes those retired longer
// than Retention. A sidecar a newer naozhi wrote is left to it.
func (s *Store) Sweep(live func(key string) bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, m := range s.m {
		if s.newer[key] {
			continue
		}
		switch alive := live(key); {
		case alive && !m.Retired.IsZero():
			m.Retired = time.Time{}
		case alive:
			continue
		case m.Retired.IsZero():
			m.Retired = now
		case now.Sub(m.Retired) > Retention:
			s.removeLocked(key)
			continue
		default:
			continue
		}
		if err := s.writeLocked(m); err != nil {
			slog.Warn("session sidecar not saved", "key", key, "err", err)
		}
		s.m[key] = m
	}
}

// Run sweeps at once and then hourly until ctx ends.
func (s *Store) Run(ctx context.Context, live func(key string) bool) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		s.Sweep(live)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// TaskKey returns a fresh "task:<random ID>" key.
func TaskKey() string {
	var b [6]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return sessionkey.TaskKeyPrefix + hex.EncodeToString(b[:])
}
