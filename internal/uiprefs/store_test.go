package uiprefs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/datadir"
)

// newDefaults: a fresh store with no file on disk reports the default theme.
func TestNew_DefaultsWhenNoFile(t *testing.T) {
	s := New(t.TempDir())
	if got := s.Get().Theme; got != defaultTheme {
		t.Fatalf("Theme = %q, want default %q", got, defaultTheme)
	}
}

// roundTrip: Set persists to disk and a second store over the same dir loads it.
func TestSet_PersistsAcrossStores(t *testing.T) {
	dir := t.TempDir()
	s1 := New(dir)
	if err := s1.Set(Settings{Theme: "dark"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := s1.Get().Theme; got != "dark" {
		t.Fatalf("in-memory Theme = %q, want dark", got)
	}

	// File exists at the documented path.
	if _, err := os.Stat(datadir.UISettingsPath(dir)); err != nil {
		t.Fatalf("ui-settings.json not written: %v", err)
	}

	s2 := New(dir)
	if got := s2.Get().Theme; got != "dark" {
		t.Fatalf("reloaded Theme = %q, want dark", got)
	}
}

// normalize: an unknown theme is coerced to the default both in memory and on
// the persisted document a later store reads.
func TestSet_NormalizesUnknownTheme(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.Set(Settings{Theme: "chartreuse"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := s.Get().Theme; got != defaultTheme {
		t.Fatalf("Theme = %q, want normalized %q", got, defaultTheme)
	}
	if got := New(dir).Get().Theme; got != defaultTheme {
		t.Fatalf("reloaded Theme = %q, want normalized %q", got, defaultTheme)
	}
}

// emptyDir: an empty StateDir yields an in-memory store — Set succeeds, Get
// reflects it, but nothing is written (no path to write to).
func TestEmptyStateDir_InMemoryOnly(t *testing.T) {
	s := New("")
	if err := s.Set(Settings{Theme: "light"}); err != nil {
		t.Fatalf("Set on in-memory store: %v", err)
	}
	if got := s.Get().Theme; got != "light" {
		t.Fatalf("Theme = %q, want light", got)
	}
}

// corruptFile: a garbage file is tolerated — the store falls back to defaults
// rather than failing construction.
func TestNew_ToleratesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(datadir.UISettingsPath(dir), []byte("{not json"), 0600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	s := New(dir)
	if got := s.Get().Theme; got != defaultTheme {
		t.Fatalf("Theme = %q, want default %q after corrupt file", got, defaultTheme)
	}
	// A subsequent Set must overwrite the corrupt bytes with valid JSON.
	if err := s.Set(Settings{Theme: "dark"}); err != nil {
		t.Fatalf("Set after corrupt: %v", err)
	}
	if got := New(dir).Get().Theme; got != "dark" {
		t.Fatalf("reloaded Theme = %q, want dark", got)
	}
}

// oversizeFile: a file beyond the cap is ignored in favour of defaults.
func TestNew_IgnoresOversizeFile(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, maxFileBytes+1)
	for i := range big {
		big[i] = ' '
	}
	if err := os.WriteFile(datadir.UISettingsPath(dir), big, 0600); err != nil {
		t.Fatalf("seed oversize file: %v", err)
	}
	if got := New(dir).Get().Theme; got != defaultTheme {
		t.Fatalf("Theme = %q, want default after oversize file", got)
	}
}

// fileMode: the persisted file is 0600 (owner-only), matching the other
// state sidecars.
func TestSet_FileMode0600(t *testing.T) {
	dir := t.TempDir()
	if err := New(dir).Set(Settings{Theme: "dark"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "ui-settings.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("file mode = %v, want 0600", perm)
	}
}
