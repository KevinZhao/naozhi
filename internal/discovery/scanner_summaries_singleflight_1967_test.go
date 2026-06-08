package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestLookupSummaries_ConcurrentSameWorkspace verifies that many goroutines
// querying summaries for the same workspace concurrently all get the right
// answer. The loadSummaryIndex singleflight collapses the redundant
// stat+read+parse (PERF-10 #1967); correctness must be preserved and -race
// must stay clean.
func TestLookupSummaries_ConcurrentSameWorkspace(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	claudeDir := makeClaudeDir(t)
	cwd := "/tmp/sf-same-workspace"
	projDir := filepath.Join(claudeDir, "projects", projDirName(cwd))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	const n = 12
	sessions := make(map[string]string, n)
	want := make(map[string]string, n)
	var entries []sessionsIndexEntry
	for i := 0; i < n; i++ {
		sid := fmt.Sprintf("aaaabbbb-0000-0000-0000-%012d", i)
		summary := fmt.Sprintf("summary %d", i)
		sessions[sid] = cwd
		want[sid] = summary
		entries = append(entries, sessionsIndexEntry{SessionID: sid, Summary: summary})
	}
	writeSessionsIndex(t, projDir, sessionsIndex{OriginalPath: cwd, Entries: entries})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				got := sc.LookupSummaries(claudeDir, sessions)
				for sid, exp := range want {
					if got[sid] != exp {
						t.Errorf("LookupSummaries[%q] = %q, want %q", sid, got[sid], exp)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}

// TestLoadSummaryIndex_MissingAndParseFailure exercises loadSummaryIndex's
// not-ok paths: a nonexistent index and a malformed JSON index both report
// ok=false rather than panicking or caching garbage.
func TestLoadSummaryIndex_MissingAndParseFailure(t *testing.T) {
	t.Parallel()
	sc := NewScanner()
	dir := t.TempDir()

	if _, ok := sc.loadSummaryIndex(filepath.Join(dir, "nope.json")); ok {
		t.Error("expected ok=false for missing index file")
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := sc.loadSummaryIndex(bad); ok {
		t.Error("expected ok=false for malformed index JSON")
	}
}
