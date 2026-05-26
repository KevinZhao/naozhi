package cron

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNewRunStore_RunsDirChmodsExisting mirrors the R238-SEC-12 (#834) /
// R238-SEC-10 (#830) defense-in-depth pattern for the runs/ root: when the
// dir already exists at a broader perm (operator pre-created it manually, or
// it carried over from a pre-fix process at 0o755), MkdirAll is a no-op on
// permissions and the broader bits persist. The Chmod follow-up clamps the
// runs/ root to 0o700 unconditionally — protecting jobID enumeration on
// shared hosts.
func TestNewRunStore_RunsDirChmodsExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron.json")
	runsRoot := filepath.Join(dir, "runs")

	// Pre-create runs/ at 0o755 — emulates a shared-host install whose
	// runs/ inherited XDG_CONFIG_HOME defaults before the security fix.
	if err := os.MkdirAll(runsRoot, 0o755); err != nil {
		t.Fatalf("pre-mkdir runs/: %v", err)
	}
	if err := os.Chmod(runsRoot, 0o755); err != nil { // defeat umask
		t.Fatalf("pre-chmod runs/: %v", err)
	}

	rs := newRunStore(storePath, 0, 0)
	if rs == nil || rs.disabled {
		t.Fatalf("expected enabled runStore, got disabled=%v", rs.disabled)
	}

	fi, err := os.Stat(runsRoot)
	if err != nil {
		t.Fatalf("stat runs/: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("runs/ perm = %o, want 0o700 (chmod must clamp pre-existing 0o755)", got)
	}
}
