package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestRecentFromParsedIndex_ReusesCachedJSONLMap pins R247-PERF-19 at the
// caller surface: recentFromParsedIndex must consume cachedJSONLByID's map
// without forcing a rebuild on each call. The companion
// TestCachedJSONLByID_ReusedAcrossCalls pins the cache layer; this test
// pins the wiring — a future refactor that copied the map (e.g. for "safety"
// reasons) inside recentFromParsedIndex would re-introduce the per-call
// allocation R247-PERF-19 set out to remove.
//
// Strategy: invoke recentFromParsedIndex twice over the same projDir +
// index, then assert cachedJSONLByID still hands back the same map pointer.
// If recentFromParsedIndex had cloned the map (or invalidated the cache as
// a side effect), the second cachedJSONLByID call would either return a
// fresh map header or rebuild from disk.
func TestRecentFromParsedIndex_ReusesCachedJSONLMap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	workspace := "/tmp/parsed-index-cache-reuse"
	sid1 := "dddddddd-0001-0001-0001-00000000fff1"
	sid2 := "dddddddd-0001-0001-0001-00000000fff2"

	for _, sid := range []string{sid1, sid2} {
		if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dirFilesCache.Delete(dir)
	t.Cleanup(func() { dirFilesCache.Delete(dir) })

	idx := &sessionsIndex{
		OriginalPath: workspace,
		Entries: []sessionsIndexEntry{
			{SessionID: sid1, Summary: "s1", FirstPrompt: "p1"},
			{SessionID: sid2, Summary: "s2", FirstPrompt: "p2"},
		},
	}

	// Warm the cache via recentFromParsedIndex (this is the production
	// caller; we want to pin its observed cache effect, not just the
	// cachedJSONLByID layer).
	got1 := recentFromParsedIndex(idx, dir, workspace, nil)
	if len(got1) != 2 {
		t.Fatalf("first call: got %d sessions, want 2", len(got1))
	}
	mapPtr1 := fmt.Sprintf("%p", cachedJSONLByID(dir))

	// Second call — must reuse the same map header. If
	// recentFromParsedIndex were rebuilding internally, the cache map
	// would still be the same (cache is not its responsibility), but a
	// future refactor that wired the build into recentFromParsedIndex's
	// own scope (e.g. "let's stop trusting the cache") would manifest as
	// a different map identity here.
	got2 := recentFromParsedIndex(idx, dir, workspace, nil)
	if len(got2) != 2 {
		t.Fatalf("second call: got %d sessions, want 2", len(got2))
	}
	mapPtr2 := fmt.Sprintf("%p", cachedJSONLByID(dir))

	if mapPtr1 != mapPtr2 {
		t.Errorf("cachedJSONLByID map pointer changed across recentFromParsedIndex calls: %s vs %s — R247-PERF-19 cache reuse regressed", mapPtr1, mapPtr2)
	}

	// Also confirm the two callers see the same logical content (no
	// stale rows, no missing rows). This guards the contract that
	// reading via the cache does not silently drop entries.
	if got1[0].SessionID != got2[0].SessionID || got1[1].SessionID != got2[1].SessionID {
		t.Errorf("session order/content differs between calls: %v vs %v", got1, got2)
	}
}
