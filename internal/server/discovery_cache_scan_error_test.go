package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
)

// TestRefresh_ScanErrorKeepsPreviousSnapshot: a transient discovery.Scan
// failure used to publish an empty session list AND advance lastDirMtime, so
// the following ticks short-circuited on "dir unchanged" and the discovered
// panel stayed empty until the sessions directory was touched again. On error
// the previous snapshot must survive and lastDirMtime must not move so the
// next tick retries the full scan.
func TestRefresh_ScanErrorKeepsPreviousSnapshot(t *testing.T) {
	claudeDir, _ := mkSessionsDir(t)
	sessDir := filepath.Join(claudeDir, "sessions")
	writeSessionJSON(t, sessDir, os.Getpid(), "11111111-1111-1111-1111-111111111111", "/tmp/keep")

	dc := newDiscoveryCache(claudeDir, func() (map[int]bool, map[string]bool, map[string]bool) {
		return nil, nil, nil
	}, nil)

	dc.refresh()
	if got := dc.snapshot(); len(got) != 1 {
		t.Fatalf("precondition: want 1 discovered session, got %d", len(got))
	}
	dc.mu.RLock()
	mtimeAfterGood := dc.lastDirMtime
	dc.mu.RUnlock()
	if mtimeAfterGood.IsZero() {
		t.Fatal("precondition: lastDirMtime should be set after a good scan")
	}

	// Force the next full scan (mtime bump) and make Scan fail.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(sessDir, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	dc.scanFn = func(string, map[int]bool, map[string]bool, map[string]bool) ([]discovery.DiscoveredSession, error) {
		return nil, errors.New("transient EIO")
	}
	dc.refresh()

	if got := dc.snapshot(); len(got) != 1 {
		t.Fatalf("scan error wiped the snapshot: got %d sessions, want 1", len(got))
	}
	dc.mu.RLock()
	mtimeAfterErr := dc.lastDirMtime
	dc.mu.RUnlock()
	if !mtimeAfterErr.Equal(mtimeAfterGood) {
		t.Fatalf("scan error advanced lastDirMtime %v → %v; next tick would short-circuit forever", mtimeAfterGood, mtimeAfterErr)
	}

	// Recovery: once Scan works again the next refresh must run (not short-circuit).
	dc.scanFn = nil
	dc.refresh()
	dc.mu.RLock()
	mtimeAfterRecover := dc.lastDirMtime
	dc.mu.RUnlock()
	if !mtimeAfterRecover.Equal(future.Truncate(time.Second)) && mtimeAfterRecover.Equal(mtimeAfterGood) {
		t.Fatalf("recovered refresh did not run a full scan (lastDirMtime still %v)", mtimeAfterRecover)
	}
}
