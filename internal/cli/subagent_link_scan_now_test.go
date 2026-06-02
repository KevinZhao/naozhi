package cli

import (
	"context"
	"testing"
	"time"
)

// TestScanMetaFiles_NowReuse pins [R112714-PERF-9]: scanMetaFiles snapshots
// time.Now() once and reuses it in both the RLock fast-path check and the
// write-lock double-check. This test verifies that a fresh cache hit is
// detected correctly (scan hook not called) and a stale cache miss triggers
// a rescan (scan hook called once).
func TestScanMetaFiles_NowReuse(t *testing.T) {
	t.Parallel()
	const sessionID = "abcdef01-2345-6789-abcd-ef0123456789"
	l, subagentDir := newLinkerForTest(t, sessionID)
	l.cacheTTL = 200 * time.Millisecond

	var scanCount int
	l.scanHook = func() { scanCount++ }

	now := time.Now()
	writeAgentFiles(t, subagentDir, "11223344556677889", "scan-worker", sessionID, "p1", now)
	toolUseTime := now.Add(-50 * time.Millisecond).UnixMilli()

	// First Resolve — must trigger exactly one scan (cache cold).
	info, resolved := l.Resolve(context.Background(), "t_scan1", "toolu_S1", "scan-worker", "", toolUseTime)
	if !resolved {
		t.Fatalf("first Resolve failed: %+v", info)
	}
	if scanCount != 1 {
		t.Errorf("expected 1 scan on cold cache, got %d", scanCount)
	}

	// Second Resolve with a different taskID while cache is still warm —
	// must NOT trigger another scan (TTL not expired).
	writeAgentFiles(t, subagentDir, "99887766554433221", "scan-worker", sessionID, "p2", now.Add(time.Millisecond))
	info2, resolved2 := l.Resolve(context.Background(), "t_scan2", "toolu_S2", "scan-worker", "", toolUseTime)
	if !resolved2 {
		t.Fatalf("second Resolve failed: %+v", info2)
	}
	if scanCount != 1 {
		t.Errorf("expected still 1 scan (cache warm), got %d", scanCount)
	}
}
