// Package sidecar keeps the Planner's per-session fields beside the session
// store, one file per session: <planner root>/sessions/<sha256(key)>.json
// (docs/rfc/planner.md §3.7, §15.1). sessions.json is never touched, so an
// older naozhi reading and rewriting it cannot drop these fields.
//
// v0.2.0 records the sessions the Planner dispatches (purpose "task", parent
// planner:main), and v0.2.1 their task state as the Planner or the user set
// it and the failure health last judged them in (§15.4). v0.5.0 adds custody
// (planner/custody): its switch on any session, and on a lineage's root what
// custody sent and why it stopped. A file outlives its session by Retention, counted from when
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
// (§15.12 ⑥). Derived for index.json and the digests, never written: such
// a session has no sidecar of its own for it.
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
	Ultracode bool      `json:"ultracode,omitempty"`
	Created   time.Time `json:"created"`
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
	// resuming it needs and what became of its worktrees. nil once reopened.
	Reap *Reap `json:"reap,omitempty"`
	// Report is the session's latest report (§15.12 ③), its conclusion
	// for the task card; the full text is in its digest (planner/digest).
	Report *Report `json:"report,omitempty"`
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
// From (the previous turn's end); Text is its reply, cut short.
type Turn struct {
	Ended time.Time `json:"ended"`
	Check bool      `json:"check,omitempty"`
	From  time.Time `json:"from,omitzero"`
	Text  string    `json:"text,omitempty"`
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
		return errors.New("sidecar: written by a newer naozhi")
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
		return true, errors.New("sidecar: written by a newer naozhi")
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
