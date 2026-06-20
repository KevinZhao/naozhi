// Package uiprefs persists the dashboard's operator-chosen presentation
// preferences (today: theme) to a single JSON file under the naozhi data
// root. Before this, the dashboard kept these only in browser localStorage,
// so the choice was lost on a browser switch, a new device, or a cache
// clear. naozhi is single-user (the dashboard auth cookie carries no
// per-session identity — internal/dashboard/auth/handlers.go), so one file
// for the whole instance is the right granularity: every browser pointed at
// this server sees the same persisted theme.
//
// The Store mirrors the minimal load/save shape of the session package's
// workspace-overrides sidecar (internal/session/store.go): json.Marshal →
// datadir.EnsureDir → osutil.WriteFileAtomic, guarded by a sync.RWMutex.
// It deliberately avoids the session store's marshal-cache / buffer-pool
// machinery — UI prefs are tiny and written at most once per operator click.
//
// Empty StateDir (test harnesses, ephemeral dev runs) degrades to an
// in-memory store: Get/Set still work for the lifetime of the process but
// nothing is persisted, matching the retired-store / cookie-secret
// degradation contract elsewhere in the server.
package uiprefs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
)

// maxFileBytes caps the on-disk file read. UI prefs are a handful of short
// string fields; 64 KiB is orders of magnitude above any legitimate payload
// and stops a corrupt/hostile file from being slurped whole into memory.
const maxFileBytes = 64 * 1024

// validThemes is the allowlist of theme values the store will persist. It is
// the single source of truth shared by the HTTP handler's validation and the
// loader's sanitisation, so a hand-edited or downgraded-version file with an
// unknown theme falls back to the default rather than being served verbatim.
var validThemes = map[string]bool{"auto": true, "light": true, "dark": true}

const defaultTheme = "auto"

// Settings is the persisted UI-preferences document. Fields are JSON-tagged
// and additive: a new preference adds a field with an omitempty tag so older
// files (missing it) decode cleanly to the zero value.
type Settings struct {
	// Theme is the dashboard color theme: "auto" (follow OS), "light", or
	// "dark". Empty/unknown values are normalised to "auto" on load.
	Theme string `json:"theme"`
}

// normalize returns a copy with out-of-range fields reset to their defaults
// so callers (and HTTP responses) never observe a value outside validThemes.
func (s Settings) normalize() Settings {
	if !validThemes[s.Theme] {
		s.Theme = defaultTheme
	}
	return s
}

// Store is a goroutine-safe holder for the instance-wide UI preferences,
// backed by <dataDir>/ui-settings.json. The zero value is not usable; build
// one with New.
type Store struct {
	path string // "" → in-memory only (no persistence)

	mu  sync.RWMutex
	cur Settings
}

// New constructs a Store backed by ui-settings.json under dataDir and loads
// any existing file. A best-effort load failure (missing file, parse error)
// is not fatal: the store starts at defaults and the next Set rewrites the
// file cleanly. An empty dataDir yields an in-memory-only store.
func New(dataDir string) *Store {
	s := &Store{
		path: datadir.UISettingsPath(dataDir),
		cur:  Settings{Theme: defaultTheme},
	}
	s.load()
	return s
}

// load reads and normalises the on-disk file into s.cur. Called once from
// New under no contention, so it takes the write lock plainly.
func (s *Store) load() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("uiprefs: load failed; using defaults", "path", s.path, "err", err)
		}
		return
	}
	if len(data) > maxFileBytes {
		slog.Warn("uiprefs: file exceeds cap; using defaults", "path", s.path, "bytes", len(data))
		return
	}
	var loaded Settings
	if err := json.Unmarshal(data, &loaded); err != nil {
		// Keep defaults rather than surfacing a parse error. We do not rename
		// the corrupt file (unlike the session store) because UI prefs carry
		// no irreplaceable state — the next Set overwrites it atomically.
		slog.Warn("uiprefs: parse failed; using defaults", "path", s.path, "err", err)
		return
	}
	s.mu.Lock()
	s.cur = loaded.normalize()
	s.mu.Unlock()
}

// Get returns the current settings (a value copy; safe to read freely).
func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Set normalises next, stores it in memory, and persists it atomically.
// The in-memory value is updated even when persistence fails or is disabled
// (empty path), so the running process stays consistent with what the
// operator just chose; the returned error reports only the persistence
// outcome.
func (s *Store) Set(next Settings) error {
	next = next.normalize()

	s.mu.Lock()
	s.cur = next
	s.mu.Unlock()

	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("marshal ui settings: %w", err)
	}
	if err := datadir.EnsureDir(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("ensure ui settings dir: %w", err)
	}
	if err := osutil.WriteFileAtomic(s.path, data, 0600); err != nil {
		return fmt.Errorf("save ui settings: %w", err)
	}
	return nil
}
