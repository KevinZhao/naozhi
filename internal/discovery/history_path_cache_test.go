package discovery

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestFindSessionJSONL_CachesPositiveHit: after the first successful
// resolve, a second lookup returns the cached path without re-running
// ReadDir. We verify the cache is populated; the save is observable as a
// map entry, not as a lock counter, so we check entries[key] directly.
func TestFindSessionJSONL_CachesPositiveHit(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	sessionID := "00000000-0000-0000-0000-000000000101"
	dirName := "-some-project"
	makeSessionJSONL(t, claudeDir, dirName, sessionID, []string{
		userJSONLLine("user", "cached-hit prompt"),
	})

	s := NewScanner()
	path1, err := s.findSessionJSONL(claudeDir, sessionID)
	if err != nil {
		t.Fatalf("findSessionJSONL (first): %v", err)
	}
	if path1 == "" {
		t.Fatal("expected path, got empty")
	}

	key := pathCacheKey(claudeDir, sessionID)
	entry, ok := s.pathCache.entries[key]
	if !ok {
		t.Fatal("expected cache entry after positive resolve")
	}
	if entry.path != path1 {
		t.Errorf("cache entry path = %q, want %q", entry.path, path1)
	}
	if !entry.negativeUntil.IsZero() {
		t.Errorf("positive entry should have zero negativeUntil, got %v", entry.negativeUntil)
	}

	// Second call hits cache; value identical.
	path2, err := s.findSessionJSONL(claudeDir, sessionID)
	if err != nil {
		t.Fatalf("findSessionJSONL (cached): %v", err)
	}
	if path2 != path1 {
		t.Errorf("cache returned different path: %q vs %q", path2, path1)
	}
}

// TestFindSessionJSONL_CachesNegativeHit: a missing sessionID is
// recorded with a negativeUntil so a second call within TTL does not
// re-run ReadDir. After TTL expiry the cache falls through; we cannot
// force wall-clock advancement in a test, so we verify the negative
// entry shape and a manually-aged override via the map.
func TestFindSessionJSONL_CachesNegativeHit(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	missingID := "00000000-0000-0000-0000-000000000404"

	s := NewScanner()
	path, err := s.findSessionJSONL(claudeDir, missingID)
	if err != nil {
		t.Fatalf("findSessionJSONL: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path for missing session, got %q", path)
	}

	key := pathCacheKey(claudeDir, missingID)
	entry, ok := s.pathCache.entries[key]
	if !ok {
		t.Fatal("expected negative cache entry")
	}
	if entry.path != "" {
		t.Errorf("negative entry should have empty path, got %q", entry.path)
	}
	if entry.negativeUntil.IsZero() {
		t.Error("negative entry should have non-zero negativeUntil")
	}
	if entry.negativeUntil.Before(time.Now()) {
		t.Errorf("negative TTL already expired at store time: %v", entry.negativeUntil)
	}

	// Second call within TTL hits the cache. We prove this by observing
	// that the entry was not replaced (negativeUntil unchanged).
	before := entry.negativeUntil
	_, _ = s.findSessionJSONL(claudeDir, missingID)
	if s.pathCache.entries[key].negativeUntil != before {
		t.Error("negative entry was rewritten; expected cache hit to leave it alone")
	}
}

// TestFindSessionJSONL_ExpiredNegativeFallsThrough: when the cached
// negativeUntil deadline has passed, the next lookup must run a real
// scan and, on success, flip the entry to positive.
func TestFindSessionJSONL_ExpiredNegativeFallsThrough(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	sessionID := "00000000-0000-0000-0000-000000000202"

	s := NewScanner()

	// Prime negative cache, then age it past TTL.
	_, _ = s.findSessionJSONL(claudeDir, sessionID)
	key := pathCacheKey(claudeDir, sessionID)
	s.pathCache.Lock()
	entry := s.pathCache.entries[key]
	entry.negativeUntil = time.Now().Add(-1 * time.Second)
	s.pathCache.entries[key] = entry
	s.pathCache.Unlock()

	// Now make the file available so a real scan would find it.
	dirName := "-late-arrival"
	makeSessionJSONL(t, claudeDir, dirName, sessionID, []string{
		userJSONLLine("user", "late arrival"),
	})

	path, err := s.findSessionJSONL(claudeDir, sessionID)
	if err != nil {
		t.Fatalf("findSessionJSONL: %v", err)
	}
	if path == "" {
		t.Fatal("expected expired negative to fall through and resolve new file, got empty")
	}

	gotEntry, ok := s.pathCache.entries[key]
	if !ok {
		t.Fatal("expected cache entry after successful rescan")
	}
	if gotEntry.path != path {
		t.Errorf("cache entry path = %q, want %q", gotEntry.path, path)
	}
	if !gotEntry.negativeUntil.IsZero() {
		t.Errorf("positive entry should have zero negativeUntil, got %v", gotEntry.negativeUntil)
	}
}

// TestFindSessionJSONL_StalePositiveSelfHeals: a cached path whose
// file has been deleted (e.g. claude CLI history compaction) must not
// be returned. findSessionJSONL os.Stats the cached path and, on
// failure, evicts the entry and runs a full rescan.
func TestFindSessionJSONL_StalePositiveSelfHeals(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	sessionID := "00000000-0000-0000-0000-000000000303"
	dirName := "-scratch"
	_, jsonlPath := makeSessionJSONL(t, claudeDir, dirName, sessionID, []string{
		userJSONLLine("user", "primed prompt"),
	})

	s := NewScanner()
	first, err := s.findSessionJSONL(claudeDir, sessionID)
	if err != nil {
		t.Fatalf("findSessionJSONL (first): %v", err)
	}
	if first != jsonlPath {
		t.Fatalf("first lookup path = %q, want %q", first, jsonlPath)
	}

	// Delete the file: cached path is now stale.
	if err := os.Remove(jsonlPath); err != nil {
		t.Fatalf("remove jsonl: %v", err)
	}

	got, err := s.findSessionJSONL(claudeDir, sessionID)
	if err != nil {
		t.Fatalf("findSessionJSONL (after delete): %v", err)
	}
	if got != "" {
		t.Errorf("expected empty path after file deletion, got %q", got)
	}

	// Subsequent lookup should hit the negative cache, not repeat scan.
	key := pathCacheKey(claudeDir, sessionID)
	entry := s.pathCache.entries[key]
	if entry.path != "" {
		t.Errorf("expected negative entry after self-heal, got path = %q", entry.path)
	}
	if entry.negativeUntil.IsZero() {
		t.Error("expected negative entry to carry TTL")
	}
}

// TestFindSessionJSONL_MissingProjectsDir: a claudeDir without a
// projects subdirectory (fresh install, first-run) must not error — it
// returns ("", nil) and caches the negative verdict so the common
// cold-start burst of concurrent lookups collapses to one ReadDir.
func TestFindSessionJSONL_MissingProjectsDir(t *testing.T) {
	t.Parallel()
	claudeDir := t.TempDir() // no projects/ subdir
	s := NewScanner()

	path, err := s.findSessionJSONL(claudeDir, "00000000-0000-0000-0000-000000000999")
	if err != nil {
		t.Fatalf("findSessionJSONL: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path, got %q", path)
	}

	key := pathCacheKey(claudeDir, "00000000-0000-0000-0000-000000000999")
	if _, ok := s.pathCache.entries[key]; !ok {
		t.Error("expected negative entry after missing projects dir")
	}
}

// TestFindSessionJSONL_CacheKeyNUL: pathCacheKey uses a NUL separator
// so two similarly-named claudeDirs cannot produce a false hit via
// string concatenation. This is cheap defense-in-depth — without NUL,
// "/home/alice/.claude" + "abc" and "/home/alice/.claudeabc" + ""
// would both collide on "/home/alice/.claudeabc".
func TestFindSessionJSONL_CacheKeyNUL(t *testing.T) {
	t.Parallel()
	k1 := pathCacheKey("/home/alice/.claude", "abc")
	k2 := pathCacheKey("/home/alice/.claudeabc", "")
	if k1 == k2 {
		t.Errorf("keys collided: %q == %q (NUL separator missing)", k1, k2)
	}
	// NUL must actually be present (0x00 byte), not just any separator.
	for _, k := range []string{k1, k2} {
		hasNUL := false
		for i := 0; i < len(k); i++ {
			if k[i] == 0 {
				hasNUL = true
				break
			}
		}
		if !hasNUL {
			t.Errorf("key %q missing NUL separator", k)
		}
	}
}

// TestFindSessionJSONL_Concurrent: 16 goroutines looking up the same
// sessionID must not race on the cache or produce inconsistent results.
// The race detector (`go test -race`) is the real assertion here; the
// strict check is that every caller sees the same non-empty path.
func TestFindSessionJSONL_Concurrent(t *testing.T) {
	t.Parallel()
	claudeDir := makeClaudeDir(t)
	sessionID := "00000000-0000-0000-0000-000000000b0c"
	dirName := "-concurrent"
	_, jsonlPath := makeSessionJSONL(t, claudeDir, dirName, sessionID, []string{
		userJSONLLine("user", "concurrent prompt"),
	})

	s := NewScanner()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]int{}
	)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := s.findSessionJSONL(claudeDir, sessionID)
			if err != nil {
				t.Errorf("findSessionJSONL: %v", err)
				return
			}
			mu.Lock()
			seen[p]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(seen) != 1 {
		t.Errorf("goroutines saw %d distinct results, want 1: %v", len(seen), seen)
	}
	if seen[jsonlPath] != 16 {
		t.Errorf("expected 16 hits on %q, got %v", jsonlPath, seen)
	}
}

// TestEvictPathCache_DropsOnlyExpiredNegatives: positive entries are
// strictly more valuable (each represents a paid-for ReadDir pass), so
// eviction prefers dropping expired negative entries first. When all
// entries are positive or non-expired, the cap may be exceeded by one
// until the next store — that is the documented tradeoff.
func TestEvictPathCache_DropsOnlyExpiredNegatives(t *testing.T) {
	t.Parallel()
	s := NewScanner()
	now := time.Now()

	// Hand-craft a mix of entries to check eviction policy.
	s.pathCache.entries["pos"] = pathCacheEntry{path: "/some/path"}
	s.pathCache.entries["neg-fresh"] = pathCacheEntry{negativeUntil: now.Add(30 * time.Second)}
	s.pathCache.entries["neg-stale-a"] = pathCacheEntry{negativeUntil: now.Add(-1 * time.Second)}
	s.pathCache.entries["neg-stale-b"] = pathCacheEntry{negativeUntil: now.Add(-5 * time.Second)}

	s.evictPathCacheLocked()

	if _, ok := s.pathCache.entries["pos"]; !ok {
		t.Error("positive entry evicted — must survive")
	}
	if _, ok := s.pathCache.entries["neg-fresh"]; !ok {
		t.Error("fresh negative evicted — only expired should drop")
	}
	if _, ok := s.pathCache.entries["neg-stale-a"]; ok {
		t.Error("stale negative 'neg-stale-a' should have been evicted")
	}
	if _, ok := s.pathCache.entries["neg-stale-b"]; ok {
		t.Error("stale negative 'neg-stale-b' should have been evicted")
	}
}

// TestEvictPathCache_FallsBackToArbitraryEviction pins the Round 170 fix:
// when the cache is at cap and every entry is positive (or fresh-negative),
// the first pass evicts nothing — fallback pass must drop arbitrary entries
// until we are below cap. Without the fallback, a long-running process that
// sees tens of thousands of sessionIDs would grow the map past cap since
// pathCacheStorePositive / pathCacheStoreNegative write unconditionally.
func TestEvictPathCache_FallsBackToArbitraryEviction(t *testing.T) {
	t.Parallel()
	s := NewScanner()

	// Fill with cap+1 positive entries so eviction must fire on the next
	// store. A pure in-memory test; no filesystem dependence.
	for i := 0; i <= pathCacheMaxEntries; i++ {
		key := pathCacheKey("/some/claude-dir", "sess-"+strconv.Itoa(i))
		s.pathCache.entries[key] = pathCacheEntry{path: "/resolved/" + strconv.Itoa(i)}
	}
	if len(s.pathCache.entries) <= pathCacheMaxEntries {
		t.Fatalf("setup expected >cap entries, got %d", len(s.pathCache.entries))
	}

	s.pathCache.Lock()
	s.evictPathCacheLocked()
	s.pathCache.Unlock()

	if len(s.pathCache.entries) > pathCacheMaxEntries {
		t.Errorf("after evict: map still > cap (%d > %d)", len(s.pathCache.entries), pathCacheMaxEntries)
	}
	// Fallback cushion: should be at or below cap - evictBatch + 1 after
	// the pass (strict bound: <= cap so the next store does not thrash).
	if len(s.pathCache.entries) > pathCacheMaxEntries-pathCacheEvictBatch+1 {
		t.Errorf("fallback pass did not create pathCacheEvictBatch headroom: got %d entries, expected <= %d",
			len(s.pathCache.entries), pathCacheMaxEntries-pathCacheEvictBatch+1)
	}
}

// TestLoadHistory_UsesPackageCache: the package-level LoadHistory →
// findSessionJSONL path must hit the shared DefaultScanner cache, so a
// fallback-scan price is paid at most once per sessionID per process
// lifetime. Observability: after the first call, DefaultScanner's
// pathCache contains the resolved entry.
func TestLoadHistory_UsesPackageCache(t *testing.T) {
	// Not t.Parallel: mutates DefaultScanner which is process-wide.
	claudeDir := makeClaudeDir(t)
	sessionID := "00000000-0000-0000-0000-000000000d0e"
	dirName := "-pkg-cache"
	makeSessionJSONL(t, claudeDir, dirName, sessionID, []string{
		userJSONLLine("user", "pkg cache prompt"),
	})

	if _, err := LoadHistory(claudeDir, sessionID, ""); err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}

	key := pathCacheKey(claudeDir, sessionID)
	ds := DefaultScanner()
	ds.pathCache.RLock()
	entry, ok := ds.pathCache.entries[key]
	ds.pathCache.RUnlock()
	if !ok {
		t.Fatal("expected DefaultScanner cache entry after LoadHistory fallback")
	}
	if entry.path == "" {
		t.Errorf("expected positive entry, got negative (negativeUntil=%v)", entry.negativeUntil)
	}
	expected := filepath.Join(claudeDir, "projects", dirName, sessionID+".jsonl")
	if entry.path != expected {
		t.Errorf("cached path = %q, want %q", entry.path, expected)
	}
}
