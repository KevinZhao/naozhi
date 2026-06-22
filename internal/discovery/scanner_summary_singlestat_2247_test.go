package discovery

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestLoadSummaryIndex_SingleStatOnMiss pins #2247: a cache miss must stat the
// index file exactly once. Before the fix the fast path stat'd, missed, then
// the singleflight closure stat'd the same file a second time. The closure now
// reuses the fast-path mtime, so a cold load issues one stat.
func TestLoadSummaryIndex_SingleStatOnMiss(t *testing.T) {
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "sessions.json")
	writeSessionsIndexFile(t, idxPath, sessionsIndex{
		OriginalPath: "/w",
		Entries:      []sessionsIndexEntry{{SessionID: "sid-1", Summary: "hello"}},
	})

	var statCount int64
	orig := summaryStatFn
	summaryStatFn = func(p string) (fs.FileInfo, error) {
		atomic.AddInt64(&statCount, 1)
		return orig(p)
	}
	t.Cleanup(func() { summaryStatFn = orig })

	sc := NewScanner()

	// Cold load: cache miss must read + parse with a single stat.
	idx, ok := sc.loadSummaryIndex(idxPath)
	if !ok {
		t.Fatal("loadSummaryIndex ok=false on cold load")
	}
	if len(idx.Entries) != 1 || idx.Entries[0].Summary != "hello" {
		t.Fatalf("parsed index wrong: %+v", idx)
	}
	if got := atomic.LoadInt64(&statCount); got != 1 {
		t.Fatalf("cold miss issued %d stats, want 1 (#2247: no redundant in-flight stat)", got)
	}

	// Warm load: fresh cache hit, one stat for the mtime check, no parse.
	if _, ok := sc.loadSummaryIndex(idxPath); !ok {
		t.Fatal("loadSummaryIndex ok=false on warm load")
	}
	if got := atomic.LoadInt64(&statCount); got != 2 {
		t.Fatalf("after warm hit total stats = %d, want 2", got)
	}
}

// writeSessionsIndexFile writes a sessionsIndex to an explicit path (the
// existing writeSessionsIndex helper targets a project dir).
func writeSessionsIndexFile(t *testing.T, path string, idx sessionsIndex) {
	t.Helper()
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
}
