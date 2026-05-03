package discovery

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// historyLine is the minimal schema for a ~/.claude/projects/.../{sessionId}.jsonl line.
type historyLine struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"` // RFC3339
	Message   json.RawMessage `json:"message"`
}

type historyMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []block
}

type historyBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`  // tool_use
	Input json.RawMessage `json:"input"` // tool_use
}

// LoadHistory finds and parses the JSONL for sessionID under claudeDir/projects/,
// returning EventEntries for user and assistant messages.
// If cwd is non-empty, the JSONL is located directly via the CWD-based path (O(1));
// an empty cwd falls back to scanning all project directories.
func LoadHistory(claudeDir, sessionID, cwd string) ([]cli.EventEntry, error) {
	var path string
	if cwd != "" {
		candidate := filepath.Join(claudeDir, "projects", projDirName(cwd), sessionID+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			path = candidate
		}
	}
	if path == "" {
		var err error
		path, err = findSessionJSONL(claudeDir, sessionID)
		if err != nil || path == "" {
			return nil, err
		}
	}
	return parseJSONL(path)
}

// findSessionJSONL searches claudeDir/projects/**/{sessionID}.jsonl.
// Package-level wrapper delegates to DefaultScanner so historical callers
// keep the same signature while gaining the pathCache. Tests that need
// isolation (or that want to exercise a fresh cold-start cache) should
// construct their own *Scanner via NewScanner() and call findSessionJSONL
// directly on it.
func findSessionJSONL(claudeDir, sessionID string) (string, error) {
	return DefaultScanner().findSessionJSONL(claudeDir, sessionID)
}

// findSessionJSONL performs the slow O(projects) fan-out scan, fronted by
// a per-Scanner pathCache. Cache semantics:
//
//   - Positive hit (path != "", cached from a prior success): os.Stat
//     the cached path; if it still exists, return immediately (1 syscall
//     vs N). If Stat fails we drop the entry and fall through to a full
//     rescan — claude CLI can rename or delete JSONL files during
//     history compaction, so the cache must self-heal.
//   - Negative hit (path == "", negativeUntil in the future): return
//     ("", nil) without touching the disk. Caps the blast radius of a
//     startup burst where 10 resume-chain goroutines look up the same
//     missing sessionID concurrently.
//   - Miss / expired negative: run the real scan and record the result
//     (positive or negative) back into the cache.
func (s *Scanner) findSessionJSONL(claudeDir, sessionID string) (string, error) {
	key := pathCacheKey(claudeDir, sessionID)

	if path, ok := s.pathCacheLookup(key); ok {
		if path == "" {
			// Cached negative verdict still fresh.
			return "", nil
		}
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		// Stale positive entry — file moved or deleted. Evict and fall
		// through to a fresh scan so the next lookup can re-learn the
		// new location (or confirm true absence).
		s.pathCacheInvalidate(key)
	}

	projectsDir := filepath.Join(claudeDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Cache the negative result so a missing ~/.claude/projects
			// dir doesn't rerun ReadDir for every subsequent lookup.
			s.pathCacheStoreNegative(key)
			return "", nil
		}
		return "", fmt.Errorf("read projects dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(projectsDir, e.Name(), sessionID+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			s.pathCacheStorePositive(key, candidate)
			return candidate, nil
		}
	}
	s.pathCacheStoreNegative(key)
	return "", nil
}

// pathCacheKey packs claudeDir + sessionID into a single map key. NUL
// byte separates the two fields so "prefix" collisions between
// similarly-named claude dirs cannot produce false hits.
func pathCacheKey(claudeDir, sessionID string) string {
	return claudeDir + "\x00" + sessionID
}

// pathCacheLookup returns (path, true) on a hit, ("", false) on a miss
// or an expired negative entry. Hot path: takes only RLock.
func (s *Scanner) pathCacheLookup(key string) (string, bool) {
	s.pathCache.RLock()
	entry, ok := s.pathCache.entries[key]
	s.pathCache.RUnlock()
	if !ok {
		return "", false
	}
	if entry.path != "" {
		return entry.path, true
	}
	if entry.negativeUntil.After(time.Now()) {
		return "", true
	}
	return "", false
}

// pathCacheStorePositive commits a successfully-resolved path. Eviction
// is skipped when the map size is within bounds; above cap we drop
// expired negative entries first (positive entries are strictly more
// valuable because each represents a full ReadDir already amortized).
func (s *Scanner) pathCacheStorePositive(key, path string) {
	s.pathCache.Lock()
	defer s.pathCache.Unlock()
	if len(s.pathCache.entries) >= pathCacheMaxEntries {
		s.evictPathCacheLocked()
	}
	s.pathCache.entries[key] = pathCacheEntry{path: path}
}

// pathCacheStoreNegative commits a "scanned and didn't find it" verdict
// with a bounded TTL so a later-created JSONL is still picked up on
// the next lookup.
func (s *Scanner) pathCacheStoreNegative(key string) {
	s.pathCache.Lock()
	defer s.pathCache.Unlock()
	if len(s.pathCache.entries) >= pathCacheMaxEntries {
		s.evictPathCacheLocked()
	}
	s.pathCache.entries[key] = pathCacheEntry{
		negativeUntil: time.Now().Add(pathCacheNegativeTTL),
	}
}

// pathCacheInvalidate drops a cached entry, used by callers that just
// observed the cached path no longer exists on disk.
func (s *Scanner) pathCacheInvalidate(key string) {
	s.pathCache.Lock()
	delete(s.pathCache.entries, key)
	s.pathCache.Unlock()
}

// evictPathCacheLocked enforces pathCacheMaxEntries. Caller MUST hold
// s.pathCache.Lock(). Strategy:
//  1. First pass drops expired negative entries — cheap wins that were
//     going to fall through to a rescan anyway.
//  2. If the map is still above cap (all entries are positive, or all
//     negatives are still fresh), drop an arbitrary slice of entries until
//     we are back under cap. Without this fallback a long-running process
//     that sees tens of thousands of distinct sessionIDs (resume chain
//     replays, dashboard queries) would grow the map without bound once
//     all 2048 slots hold positive entries. Map iteration order is
//     randomised in Go, so "arbitrary" is effectively random eviction —
//     simple, allocation-free, and good enough; LRU would require a
//     doubly-linked list that costs more than the ReadDir we'd save.
func (s *Scanner) evictPathCacheLocked() {
	now := time.Now()
	for k, v := range s.pathCache.entries {
		if v.path == "" && !v.negativeUntil.After(now) {
			delete(s.pathCache.entries, k)
		}
	}
	if len(s.pathCache.entries) < pathCacheMaxEntries {
		return
	}
	// Second pass: drop arbitrary entries (random via map iteration) until
	// we drop below cap. Leave pathCacheEvictBatch headroom so the next
	// store isn't forced to re-evict immediately.
	excess := len(s.pathCache.entries) - pathCacheMaxEntries + pathCacheEvictBatch
	for k := range s.pathCache.entries {
		if excess <= 0 {
			break
		}
		delete(s.pathCache.entries, k)
		excess--
	}
}

func parseJSONL(path string) ([]cli.EventEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	entries := make([]cli.EventEntry, 0, 128)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var hl historyLine
		if err := json.Unmarshal(line, &hl); err != nil {
			slog.Debug("skip malformed history line", "path", path, "err", err)
			continue
		}

		ts := parseTimestamp(hl.Timestamp)

		switch hl.Type {
		case "user":
			var msg historyMessage
			if err := json.Unmarshal(hl.Message, &msg); err != nil {
				continue
			}
			text := extractText(msg.Content)
			if text == "" {
				continue
			}
			entries = append(entries, cli.EventEntry{
				Time:    ts,
				Type:    "user",
				Summary: cli.TruncateRunes(text, 120),
				Detail:  cli.TruncateRunes(text, 2000),
			})

		case "assistant":
			var msg historyMessage
			if err := json.Unmarshal(hl.Message, &msg); err != nil {
				continue
			}
			var blocks []historyBlock
			if err := json.Unmarshal(msg.Content, &blocks); err != nil {
				continue
			}
			for _, b := range blocks {
				// Only parse text blocks for history display.
				// tool_use and thinking are always filtered out by the
				// dashboard (eventHtml / processEventsForDisplay) and
				// would waste slots in the 500-entry persistedHistory
				// budget, pushing visible user/text entries out.
				if b.Type != "text" || strings.TrimSpace(b.Text) == "" {
					continue
				}
				entries = append(entries, cli.EventEntry{
					Time:    ts,
					Type:    "text",
					Summary: cli.TruncateRunes(b.Text, 120),
					Detail:  cli.TruncateRunes(b.Text, 16000),
				})
			}
		}
	}
	return entries, scanner.Err()
}

// extractText handles content that is either a plain string or []block.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try array of blocks
	var blocks []historyBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func parseTimestamp(ts string) int64 {
	if ts == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return 0
		}
	}
	return t.UnixMilli()
}
