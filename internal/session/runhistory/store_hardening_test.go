package runhistory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The symlink / permission hardening this store gets from internal/runlog
// (#2709) is tested there (layout_test.go); this pins the store's own counter.

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

// TestNewStore_SymlinkedRootIsDisabled pins the wiring to runlog's layout
// guard: a symlinked root must yield a disabled Store, not one that writes
// through the link.
func TestNewStore_SymlinkedRootIsDisabled(t *testing.T) {
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
	if s.Enabled() {
		t.Fatal("NewStore on a symlinked root returned an enabled Store")
	}
}
