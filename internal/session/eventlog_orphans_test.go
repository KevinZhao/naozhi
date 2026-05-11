package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// makeFakeLog creates an empty .log / .idx pair under dir for the given
// session key. mtime is backdated by `age` so orphan-sweep sees it as
// stale or fresh depending on the caller's choice.
func makeFakeLog(t *testing.T, dir, key string, age time.Duration) {
	t.Helper()
	log := persist.LogPath(dir, key)
	idx := persist.IdxPath(dir, key)
	for _, p := range []string{log, idx} {
		if err := os.WriteFile(p, []byte("fake"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}
}

// TestOrphanSweep_RemovesStaleUnknown exercises the happy path: a
// <keyhash>.log with an unknown stem and mtime past the cutoff gets
// removed. Its .idx sibling should also go.
func TestOrphanSweep_RemovesStaleUnknown(t *testing.T) {
	dir := t.TempDir()
	makeFakeLog(t, dir, "ghost", orphanSweepAge+time.Hour)

	removed, err := sweepOrphanEventLogs(dir, map[string]struct{}{}, time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Both .log and .idx count as separate files.
	if removed != 2 {
		t.Errorf("removed=%d, want 2 (log+idx)", removed)
	}
	if _, err := os.Stat(persist.LogPath(dir, "ghost")); !os.IsNotExist(err) {
		t.Errorf("log not removed: err=%v", err)
	}
	if _, err := os.Stat(persist.IdxPath(dir, "ghost")); !os.IsNotExist(err) {
		t.Errorf("idx not removed: err=%v", err)
	}
}

// TestOrphanSweep_KeepsKnown confirms the happy path for known
// sessions: no matter how old the file, we never touch its log.
func TestOrphanSweep_KeepsKnown(t *testing.T) {
	dir := t.TempDir()
	makeFakeLog(t, dir, "kept", orphanSweepAge*2)

	removed, err := sweepOrphanEventLogs(dir,
		map[string]struct{}{"kept": {}}, time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed=%d, want 0 (session is known)", removed)
	}
	if _, err := os.Stat(persist.LogPath(dir, "kept")); err != nil {
		t.Errorf("known log was removed: %v", err)
	}
}

// TestOrphanSweep_KeepsRecentUnknown ensures the "operator may be
// mid-migration" safeguard — an unknown stem whose file is still
// fresh (say, last hour) must not be deleted even though the
// session table doesn't know about it.
func TestOrphanSweep_KeepsRecentUnknown(t *testing.T) {
	dir := t.TempDir()
	makeFakeLog(t, dir, "fresh-ghost", time.Hour)

	removed, err := sweepOrphanEventLogs(dir, map[string]struct{}{}, time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed=%d, want 0 (unknown but recent)", removed)
	}
	if _, err := os.Stat(persist.LogPath(dir, "fresh-ghost")); err != nil {
		t.Errorf("recent log removed prematurely: %v", err)
	}
}

// TestOrphanSweep_IgnoresTmpFiles confirms rotate-staging *.tmp.*
// files are outside the sweep's scope (they have their own sweeper
// via persist.SweepOrphans which NewPersister already calls).
func TestOrphanSweep_IgnoresTmpFiles(t *testing.T) {
	dir := t.TempDir()
	tmpName := persist.KeyHash("x") + ".tmp.1234.log"
	tmpPath := filepath.Join(dir, tmpName)
	if err := os.WriteFile(tmpPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate well past cutoff.
	when := time.Now().Add(-orphanSweepAge * 2)
	os.Chtimes(tmpPath, when, when)

	removed, err := sweepOrphanEventLogs(dir, map[string]struct{}{}, time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed=%d — tmp files should not participate in orphan sweep", removed)
	}
}

// TestOrphanSweep_IgnoresNonNaozhiFiles keeps operator-dropped
// sidecar files alive. Another naozhi version / debug tool might
// leave a README; we don't want to eat it on upgrade.
func TestOrphanSweep_IgnoresNonNaozhiFiles(t *testing.T) {
	dir := t.TempDir()
	readme := filepath.Join(dir, "README.md")
	os.WriteFile(readme, []byte("hi"), 0o600)
	// Even when very old.
	when := time.Now().Add(-orphanSweepAge * 3)
	os.Chtimes(readme, when, when)

	removed, err := sweepOrphanEventLogs(dir, map[string]struct{}{}, time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed=%d, want 0 (foreign file)", removed)
	}
	if _, err := os.Stat(readme); err != nil {
		t.Errorf("README removed: %v", err)
	}
}

// TestOrphanSweep_MissingDir returns (0, nil). A fresh deployment
// that hasn't yet created events/ must not error out.
func TestOrphanSweep_MissingDir(t *testing.T) {
	removed, err := sweepOrphanEventLogs(
		filepath.Join(t.TempDir(), "nonexistent"),
		map[string]struct{}{}, time.Now())
	if err != nil {
		t.Fatalf("missing dir: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed=%d from missing dir", removed)
	}
}

// TestOrphanSweep_EmptyDirReturnsClean exercises the no-op path
// (directory exists but has nothing we care about).
func TestOrphanSweep_EmptyDirReturnsClean(t *testing.T) {
	dir := t.TempDir()
	removed, err := sweepOrphanEventLogs(dir, nil, time.Now())
	if err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed=%d on empty dir", removed)
	}
}

// TestOrphanSweep_EmptyDirWithDisabledConfig: even with disabled
// eventLogDir (""), the helper must safely return 0 without any
// syscall spam so Router.runOrphanSweep can be called unconditionally.
func TestOrphanSweep_EmptyDirWithDisabledConfig(t *testing.T) {
	removed, err := sweepOrphanEventLogs("", nil, time.Now())
	if err != nil {
		t.Fatalf("disabled: %v", err)
	}
	if removed != 0 {
		t.Errorf("disabled sweep returned removed=%d", removed)
	}
}

// TestRouter_RunOrphanSweep_Integration: NewRouter → writes a stale
// orphan into events/ → sweep removes it. Exercises the goroutine
// + historyWg wire-up.
func TestRouter_RunOrphanSweep_Integration(t *testing.T) {
	tmp := t.TempDir()
	eventLogDir := filepath.Join(tmp, "events")
	if err := os.MkdirAll(eventLogDir, 0o700); err != nil {
		t.Fatal(err)
	}
	makeFakeLog(t, eventLogDir, "stale-ghost", orphanSweepAge*2)

	r := NewRouter(RouterConfig{
		MaxProcs:    2,
		TTL:         time.Hour,
		StorePath:   filepath.Join(tmp, "sessions.json"),
		EventLogDir: eventLogDir,
	})
	t.Cleanup(r.Shutdown)

	// NewRouter launches the sweep in a historyWg-tracked goroutine.
	// Wait for it to finish deterministically via Shutdown's drain.
	// We don't want to call Shutdown yet (that would tear down the
	// persister), so use a bounded poll on the filesystem.
	testhelper.Eventually(t, func() bool {
		_, err := os.Stat(persist.LogPath(eventLogDir, "stale-ghost"))
		return os.IsNotExist(err)
	}, 2*time.Second, "orphan sweep did not remove stale-ghost")
}
