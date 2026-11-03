// Package sidecar keeps the Planner's per-session fields beside the session
// store, one file per session: <planner root>/sessions/<sha256(key)>.json
// (docs/rfc/planner.md §3.7, §15.1). sessions.json is never touched, so an
// older naozhi reading and rewriting it cannot drop these fields.
//
// v0.2.0 records the sessions the Planner dispatches (purpose "task", parent
// planner:main). A file outlives its session by Retention, counted from when
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
	// Workspace is the workspace ID the session was dispatched into.
	Workspace string    `json:"workspace,omitempty"`
	Created   time.Time `json:"created"`
	// Retired is when the session was first seen gone from the router; the
	// file is removed Retention later.
	Retired time.Time `json:"retired,omitzero"`
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
}

// Open loads every sidecar under dir. A file that cannot be read or parsed
// is left in place and skipped. An empty dir keeps the store in memory.
func Open(dir string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	s := &Store{dir: dir, now: now, m: map[string]Meta{}}
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

// Remove drops key's sidecar.
func (s *Store) Remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(key)
}

func (s *Store) removeLocked(key string) {
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

// Purpose is key's purpose ("" without a sidecar).
func (s *Store) Purpose(key string) string {
	m, _ := s.Get(key)
	return m.Purpose
}

// Sweep marks the sidecars whose session live no longer reports as retired,
// clears the mark of one that came back, and removes those retired longer
// than Retention.
func (s *Store) Sweep(live func(key string) bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, m := range s.m {
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
