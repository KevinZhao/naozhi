package discovery

import (
	"path/filepath"
	"testing"
	"time"
)

// TestLoadHistory_CWDBypassesNegativeCache is the regression guard for the
// interactive-preview flake: a single fallback-scan miss seeds a 60s negative
// cache entry that shadows the session for the full TTL, even after the JSONL
// lands on disk. The fix is to pass the session's cwd so LoadHistory resolves
// the file via an O(1) os.Stat on the CWD-derived path — a lookup that runs
// BEFORE (and independent of) findSessionJSONL's pathCache.
//
// The test reproduces the poisoning explicitly: it forces a cwd-less miss
// first (which stores the negative entry), confirms the negative entry is
// live, then asserts a cwd-qualified LoadHistory still returns the entries.
func TestLoadHistory_CWDBypassesNegativeCache(t *testing.T) {
	// Not t.Parallel: mutates the process-wide DefaultScanner pathCache.
	claudeDir := makeClaudeDir(t)
	sessionID := "00000000-0000-0000-0000-0000cdcdcdcd"

	// The real cwd whose slug names the project dir. ClaudeProjectSlug maps
	// "/home/u/proj" → "-home-u-proj"; makeSessionJSONL needs the slug form.
	cwd := "/home/u/preview-proj"
	dirName := ClaudeProjectSlug(cwd)
	makeSessionJSONL(t, claudeDir, dirName, sessionID, []string{
		userJSONLLine("user", "hello from preview"),
	})

	ds := DefaultScanner()
	key := pathCacheKey(claudeDir, sessionID)
	// Clean any residue from a prior test so the negative-entry assertion
	// below is meaningful.
	ds.pathCacheInvalidate(key)

	// Step 1: a cwd-less lookup walks projects/ but the slug dir does not
	// match the scan's expectations in the way the handler used to call it
	// (cwd == ""). It DOES find the file via the fan-out scan here, so to
	// reproduce the poisoning deterministically we seed a fresh negative
	// entry directly — exactly what pathCacheStoreNegative does when a real
	// miss happens (card shown before flush / mid-compaction rename).
	ds.pathCacheStoreNegative(key)
	if _, ok := ds.pathCacheLookup(key); !ok {
		t.Fatal("precondition: expected a live negative cache entry")
	}

	// Step 2: with the negative entry live, a cwd-LESS LoadHistory is
	// shadowed — this is the bug. It must return empty.
	got, err := LoadHistory(claudeDir, sessionID, "")
	if err != nil {
		t.Fatalf("LoadHistory(no cwd): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("precondition: cwd-less lookup should be shadowed by negative cache, got %d entries", len(got))
	}

	// Step 3: the fix — passing cwd resolves the JSONL via the direct
	// os.Stat path, bypassing the poisoned negative cache entirely.
	got, err = LoadHistory(claudeDir, sessionID, cwd)
	if err != nil {
		t.Fatalf("LoadHistory(with cwd): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("cwd lookup should bypass negative cache and return the entry, got %d entries", len(got))
	}
	if got[0].Summary != "hello from preview" {
		t.Errorf("entry summary = %q, want %q", got[0].Summary, "hello from preview")
	}

	// The direct-stat path must NOT have mutated the negative entry into a
	// positive one (it never touches the pathCache at all). The negative
	// entry stays as-is until its TTL expires.
	ds.pathCache.RLock()
	entry := ds.pathCache.entries[key]
	ds.pathCache.RUnlock()
	if entry.path != "" {
		t.Errorf("cwd path should not have populated the pathCache, got path=%q", entry.path)
	}
	if entry.negativeUntil.Before(time.Now()) {
		t.Error("negative entry unexpectedly expired during the test")
	}
}

// TestLoadHistory_StaleCWDFallsBackToScan confirms cwd is a pure optimisation
// hint: when it points at the wrong project dir, LoadHistory degrades to the
// fan-out scan rather than returning empty. A bad hint must never narrow the
// result set.
func TestLoadHistory_StaleCWDFallsBackToScan(t *testing.T) {
	// Not t.Parallel: shares DefaultScanner with the test above.
	claudeDir := makeClaudeDir(t)
	sessionID := "00000000-0000-0000-0000-0000efefefef"

	realCWD := "/home/u/real-proj"
	dirName := ClaudeProjectSlug(realCWD)
	makeSessionJSONL(t, claudeDir, dirName, sessionID, []string{
		userJSONLLine("user", "stale-cwd prompt"),
	})

	// Ensure no stale negative entry from a previous run blocks the fallback.
	DefaultScanner().pathCacheInvalidate(pathCacheKey(claudeDir, sessionID))

	// cwd points at a dir that does not contain the JSONL; the os.Stat hint
	// misses, so LoadHistory falls back to findSessionJSONL which DOES find it.
	wrongCWD := "/home/u/some-other-proj"
	if _, err := filepath.Abs(wrongCWD); err != nil {
		t.Fatal(err)
	}
	got, err := LoadHistory(claudeDir, sessionID, wrongCWD)
	if err != nil {
		t.Fatalf("LoadHistory(stale cwd): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("stale cwd must fall back to scan and still find the entry, got %d entries", len(got))
	}
}
