package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPruneExpiredEvictionsLocked pins that the centralised pruner deletes
// only entries older than evictGrace and keeps fresh ones. Before this fix
// the prune lived inline in refresh() and never ran on the short-circuit
// path, so the evictedPIDs map could grow without bound in steady state and
// a stale entry could wrongly filter a brand-new session reusing the PID.
func TestPruneExpiredEvictionsLocked(t *testing.T) {
	t.Parallel()

	dc := newDiscoveryCache("", nil, nil)
	now := time.Now()

	dc.evictedPIDs[100] = now.Add(-evictGrace - time.Second) // expired
	dc.evictedPIDs[200] = now.Add(-evictGrace / 2)           // fresh
	dc.evictedPIDs[300] = now                                // just evicted

	dc.mu.Lock()
	dc.pruneExpiredEvictionsLocked(now)
	dc.mu.Unlock()

	if _, ok := dc.evictedPIDs[100]; ok {
		t.Errorf("expired PID 100 should have been pruned")
	}
	if _, ok := dc.evictedPIDs[200]; !ok {
		t.Errorf("fresh PID 200 (within grace) must be retained")
	}
	if _, ok := dc.evictedPIDs[300]; !ok {
		t.Errorf("just-evicted PID 300 must be retained")
	}
}

// TestTryShortCircuit_PrunesEvictionsWithoutCachedSessions pins that the
// short-circuit fast path (which is the common steady state) expires stale
// eviction entries even when there are zero cached sessions to dynamic-refresh.
// Without the fix this branch returned true without ever touching evictedPIDs.
func TestTryShortCircuit_PrunesEvictionsWithoutCachedSessions(t *testing.T) {
	t.Parallel()

	dc := newDiscoveryCache(t.TempDir(), nil, nil)
	// Seed a non-zero lastDirMtime so tryShortCircuit does not bail on the
	// "first run" guard. The sessions dir must be stat-able and unchanged.
	sessDir := filepath.Join(dc.claudeDir, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	info, err := os.Stat(sessDir)
	if err != nil {
		t.Fatalf("stat sessions: %v", err)
	}
	dc.lastDirMtime = info.ModTime()
	dc.sessions = nil // no cached sessions -> the else branch

	dc.evictedPIDs[42] = time.Now().Add(-evictGrace - time.Second)

	if !dc.tryShortCircuit() {
		t.Fatalf("tryShortCircuit should short-circuit on unchanged dir with no live PIDs")
	}
	if _, ok := dc.evictedPIDs[42]; ok {
		t.Errorf("short-circuit path must prune the expired eviction entry")
	}
}
