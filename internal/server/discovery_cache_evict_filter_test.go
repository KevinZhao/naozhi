package server

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSessionJSON writes a ~/.claude/sessions/{pid}.json record that
// discovery.Scan will pick up for a live PID. The session ID must be a
// valid UUID so IsValidSessionID accepts it.
func writeSessionJSON(t *testing.T, sessDir string, pid int, sessionID, cwd string) {
	t.Helper()
	// startedAt within the no-JSONL grace window so Scan keeps the session
	// even though we don't write a projects/ JSONL for it.
	body := fmt.Sprintf(
		`{"pid":%d,"sessionId":%q,"cwd":%q,"startedAt":%d,"kind":"managed","entrypoint":"cli"}`,
		pid, sessionID, cwd, time.Now().UnixMilli())
	if err := os.WriteFile(filepath.Join(sessDir, fmt.Sprintf("%d.json", pid)), []byte(body), 0o644); err != nil {
		t.Fatalf("write session json: %v", err)
	}
}

// TestRefresh_FullScanFiltersEvictedPID pins R20260614-PERF-009 (#2123):
// the refactored refresh() — which now computes the evictedPIDs expiry
// sweep + O(N) sessions filter against a lock-free snapshot and holds the
// write lock only for the final publish — must still drop a recently
// evicted PID from the published full-scan result.
func TestRefresh_FullScanFiltersEvictedPID(t *testing.T) {
	claudeDir, _ := mkSessionsDir(t)
	sessDir := filepath.Join(claudeDir, "sessions")

	self := os.Getpid()
	evicted := os.Getppid()
	if evicted <= 0 || evicted == self {
		t.Skip("need a distinct live parent PID for the evicted slot")
	}

	writeSessionJSON(t, sessDir, self, "11111111-1111-1111-1111-111111111111", "/tmp/keep")
	writeSessionJSON(t, sessDir, evicted, "22222222-2222-2222-2222-222222222222", "/tmp/drop")

	dc := newDiscoveryCache(claudeDir, func() (map[int]bool, map[string]bool, map[string]bool) {
		return nil, nil, nil
	}, nil)

	// lastDirMtime stays zero so tryShortCircuit bails → full scan runs.
	dc.mu.Lock()
	dc.evictedPIDs[evicted] = time.Now()
	dc.mu.Unlock()

	dc.refresh()

	var sawSelf, sawEvicted bool
	for _, s := range dc.snapshot() {
		switch s.PID {
		case self:
			sawSelf = true
		case evicted:
			sawEvicted = true
		}
	}
	if sawEvicted {
		t.Errorf("evicted PID %d was not filtered out of the full-scan publish", evicted)
	}
	if !sawSelf {
		t.Errorf("live self PID %d missing from publish — filter dropped too much", self)
	}
}

// TestRefresh_FullScanExpiresOldEvictions pins that an evictedPIDs entry
// older than the 60s grace is swept from the live map by refresh() and no
// longer filters its PID. Verifies the expiry sweep still mutates the
// shared dc.evictedPIDs map under the publish lock after the refactor.
func TestRefresh_FullScanExpiresOldEvictions(t *testing.T) {
	claudeDir, _ := mkSessionsDir(t)
	sessDir := filepath.Join(claudeDir, "sessions")

	self := os.Getpid()
	writeSessionJSON(t, sessDir, self, "33333333-3333-3333-3333-333333333333", "/tmp/keep")

	dc := newDiscoveryCache(claudeDir, func() (map[int]bool, map[string]bool, map[string]bool) {
		return nil, nil, nil
	}, nil)

	// Seed a stale eviction (well past the 60s grace) for some unrelated PID.
	stalePID := 999999
	dc.mu.Lock()
	dc.evictedPIDs[stalePID] = time.Now().Add(-5 * time.Minute)
	dc.mu.Unlock()

	dc.refresh()

	dc.mu.RLock()
	_, stillThere := dc.evictedPIDs[stalePID]
	dc.mu.RUnlock()
	if stillThere {
		t.Errorf("stale eviction for PID %d was not swept by refresh()", stalePID)
	}
}
