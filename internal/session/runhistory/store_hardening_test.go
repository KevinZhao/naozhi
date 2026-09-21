package runhistory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These four pin the hardening the store GAINED by adopting internal/runlog
// (#2709). Before that it did a bare MkdirAll + WriteFileAtomic, so each of
// these scenarios silently wrote through — the cron store had learned every one
// of them from an incident and this store had simply never been told.

// TestNewStore_SymlinkedRootDisablesPersistence: a pre-created
// `<dataDir>/session-runs -> /tmp/x` would land every record outside the data
// dir, and MkdirAll does not error on a symlink-to-directory.
func TestNewStore_SymlinkedRootDisablesPersistence(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "session-runs")
	if err := os.Symlink(elsewhere, root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	s := NewStore(root, 5, time.Hour)
	defer s.Close()
	s.Append(SessionRun{SessionKey: "dashboard:direct:a", RunID: "aaaa1111", StartedAt: time.Now()})

	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Errorf("wrote %d entries through the symlinked root", len(ents))
	}
	if got := s.Recent("dashboard:direct:a", 0); len(got) != 0 {
		t.Errorf("a disabled store must serve nothing, got %d runs", len(got))
	}
}

// TestNewStore_TightensLooseRootPerm: a 0755 root lets any other OS user on the
// box enumerate session-key hashes and read transcript excerpts out of the
// records.
func TestNewStore_TightensLooseRootPerm(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "session-runs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewStore(root, 5, time.Hour)
	defer s.Close()

	fi, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("root mode = %v, want 0700", perm)
	}
}

// TestAppend_RefusesSymlinkedSessionDir is the same guard one level down, and
// it must hold on the SECOND append too: the ensured-marker fast path cannot be
// allowed to wave through a directory swapped for a symlink after the first
// write.
func TestAppend_RefusesSymlinkedSessionDir(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "session-runs")
	s := NewStore(root, 5, time.Hour)
	defer s.Close()

	const key = "dashboard:direct:b"
	s.Append(SessionRun{SessionKey: key, RunID: "bbbb1111", StartedAt: time.Now()})
	if got := s.Recent(key, 0); len(got) != 1 {
		t.Fatalf("first append: %d runs cached, want 1", len(got))
	}
	// Find the session's real directory and swap it for a symlink.
	ents, err := os.ReadDir(root)
	if err != nil || len(ents) != 1 {
		t.Fatalf("expected one session dir under the root: %v %d", err, len(ents))
	}
	dir := filepath.Join(root, ents[0].Name())
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	s.Append(SessionRun{SessionKey: key, RunID: "bbbb2222", StartedAt: time.Now()})

	if written, _ := os.ReadDir(elsewhere); len(written) != 0 {
		t.Errorf("second append wrote %d entries through the swapped-in symlink", len(written))
	}
}

// TestWriteFailedTotals_CountsDroppedRecords: Append cannot fail the user's
// turn, so a lost record is invisible without a counter. Before the adoption
// the failure was a Warn line and nothing else.
func TestWriteFailedTotals_CountsDroppedRecords(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "session-runs")
	s := NewStore(root, 5, time.Hour)
	defer s.Close()

	const key = "dashboard:direct:c"
	// Pre-create the session dir read-only so the atomic write cannot land.
	s.Append(SessionRun{SessionKey: key, RunID: "cccc1111", StartedAt: time.Now()})
	ents, err := os.ReadDir(root)
	if err != nil || len(ents) != 1 {
		t.Fatalf("expected one session dir: %v %d", err, len(ents))
	}
	dir := filepath.Join(root, ents[0].Name())
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()

	s.Append(SessionRun{SessionKey: key, RunID: "cccc2222", StartedAt: time.Now()})

	// The skip has to key off whether the write really failed, not off the
	// counter: keying it on the counter makes "failed but never counted" —
	// exactly the regression this test exists for — look like a skip.
	if _, err := os.Stat(filepath.Join(dir, "cccc2222.json")); err == nil {
		t.Skip("write into a 0500 dir succeeded (running as root?); counter path not exercised")
	}
	full, other := s.WriteFailedTotals()
	if other != 1 || full != 0 {
		t.Errorf("counters = (diskFull=%d, other=%d), want (0,1) — a dropped record with no counter is invisible", full, other)
	}
}
