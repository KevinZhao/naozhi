// Package uiprefs persists the dashboard's operator-chosen presentation
// preferences (today: theme) to one JSON file under the naozhi data root.
// naozhi is single-user (the auth cookie carries no per-session identity),
// so one file per instance is the right granularity. Empty StateDir
// degrades to an in-memory store: Get/Set work, nothing is persisted.
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

// maxFileBytes caps the on-disk read; 64 KiB is far above any legitimate
// payload and stops a corrupt/hostile file being slurped whole.
const maxFileBytes = 64 * 1024

// validThemes is the allowlist shared by the HTTP handler's validation and
// the loader, so a hand-edited or downgraded file with an unknown theme
// falls back to the default rather than being served verbatim.
var validThemes = map[string]bool{"auto": true, "light": true, "dark": true}

const defaultTheme = "auto"

// Settings is the persisted UI-preferences document. New preferences add
// an omitempty field so older files decode cleanly to the zero value.
type Settings struct {
	// Theme is "auto" (follow OS), "light", or "dark"; unknown → "auto" on load.
	Theme string `json:"theme"`
}

// normalize returns a copy with out-of-range fields reset to defaults.
func (s Settings) normalize() Settings {
	if !validThemes[s.Theme] {
		s.Theme = defaultTheme
	}
	return s
}

// Store is a goroutine-safe holder for the instance-wide UI preferences,
// backed by <dataDir>/ui-settings.json. Build one with New.
type Store struct {
	path string // "" → in-memory only (no persistence)

	mu  sync.RWMutex
	cur Settings
}

// New constructs a Store under dataDir and loads any existing file. A load
// failure is not fatal: the store starts at defaults and the next Set
// rewrites the file. An empty dataDir yields an in-memory-only store.
func New(dataDir string) *Store {
	s := &Store{
		path: datadir.UISettingsPath(dataDir),
		cur:  Settings{Theme: defaultTheme},
	}
	s.load()
	return s
}

// load reads and normalises the on-disk file into s.cur (called once from New).
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
		// Keep defaults; the corrupt file is not renamed because UI prefs carry
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

// Set normalises next, stores it in memory, and persists it atomically. The
// in-memory value is updated even when persistence fails or is disabled;
// the returned error reports only the persistence outcome.
//
// The lock is held through the disk write so two concurrent Set calls
// cannot leave the on-disk winner different from the in-memory winner.
// Writes are operator-rare; nothing inside the critical section re-enters s.mu.
func (s *Store) Set(next Settings) error {
	next = next.normalize()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = next

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
