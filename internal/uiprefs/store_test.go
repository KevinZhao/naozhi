package uiprefs

import (
	"os"
	"path/filepath"
	"sync"
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

// concurrentSet: [R202606-GO-1] under N concurrent Set calls, the persisted
// file must agree with the final in-memory Get(). Before the fix the lock was
// released before the disk write, so the two writes could land in any order
// and a reload could read a theme that contradicts memory. With the lock held
// across marshal+WriteFileAtomic the last in-memory winner is also the last
// on-disk write, so a fresh store reading the file sees the same value.
func TestSet_ConcurrentSetMemoryAndFileConsistent(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	themes := []string{"auto", "light", "dark"}
	const goroutines = 64

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		theme := themes[i%len(themes)]
		go func() {
			defer wg.Done()
			if err := s.Set(Settings{Theme: theme}); err != nil {
				t.Errorf("Set(%q): %v", theme, err)
			}
		}()
	}
	wg.Wait()

	// The race detector (go test -race) flags the unsynchronised window if the
	// lock no longer covers the disk write. Beyond that, the on-disk document
	// must match what the running process believes is current.
	mem := s.Get().Theme
	disk := New(dir).Get().Theme
	if mem != disk {
		t.Fatalf("memory Theme = %q but on-disk Theme = %q; persist not serialised", mem, disk)
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
