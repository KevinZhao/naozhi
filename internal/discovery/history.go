package discovery

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// historyLine is the minimal schema for a ~/.claude/projects/.../{sessionId}.jsonl line.
//
// UUID is Claude's own record identifier, stable across re-reads of the
// same file. naozhi adopts it verbatim as EventEntry.UUID so MergedSource
// can dedup a Claude-JSONL-derived entry against the naozhi-native
// persisted copy of the same turn. When UUID is absent (rare — some
// older CLI versions omit it on assistant records), DeriveLegacyUUID
// produces a stable fallback from (time + summary + detail).
type historyLine struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"` // RFC3339
	UUID      string          `json:"uuid"`
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
func LoadHistory(claudeDir, sessionID, cwd string) ([]clievent.EventEntry, error) {
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

func parseJSONL(path string) ([]clievent.EventEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	entries := make([]clievent.EventEntry, 0, 128)
	// bufio.Reader (not bufio.Scanner): the Claude CLI inlines uploaded
	// images as base64 into a single NDJSON line, so one user turn with a
	// few screenshots can balloon past 5-10 MB. bufio.Scanner aborts the
	// WHOLE file with "token too long" on the first such line, blanking the
	// dashboard history. readJSONLLine instead skips just the oversized
	// line (its inline-image payload carries no text worth displaying) and
	// keeps parsing the rest. The 4 MB threshold matches parseTail's
	// maxCarryBytes so both read paths agree on which lines they drop.
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, oversized, rerr := readJSONLLine(r)
		if len(line) > 0 && !oversized {
			if parsed, ok := parseHistoryLine(line); ok {
				entries = append(entries, parsed...)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return entries, nil
			}
			return entries, rerr
		}
	}
}

// maxJSONLLineBytes caps a single JSONL line's content length (excluding the
// terminating '\n') before readJSONLLine treats it as unparseable junk and
// skips it. Equal to parseTail's maxCarryBytes and measured the same way
// (newline-free content), so the forward (preview) and reverse (sidebar
// pagination) readers drop the exact same oversized lines — typically a user
// turn with inline base64 images, which contributes no display text anyway.
const maxJSONLLineBytes = 4 * 1024 * 1024

// readJSONLLine reads one '\n'-terminated line from r.
//
//   - Normal line: returns the line bytes (without the trailing '\n'),
//     oversized=false. The slice is only valid until the next read — callers
//     (parseHistoryLine) must consume it before reading again.
//   - Oversized line (total > maxJSONLLineBytes): the remainder of the line
//     is drained and discarded, returns oversized=true with nil bytes.
//     Retained memory is bounded at ~maxJSONLLineBytes — once the running
//     total crosses the cap we stop appending to buf, so a 50 MB line costs
//     at most one cap's worth of buffer, never the line's full length.
//   - At EOF: returns the final unterminated line (if any) plus io.EOF.
//
// err is io.EOF once the file is exhausted (possibly alongside a final
// line); any other non-nil err is a real read failure.
func readJSONLLine(r *bufio.Reader) (line []byte, oversized bool, err error) {
	frag, err := r.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		// Fast path: a complete line fit in the reader's buffer (or we hit
		// EOF / a real error). frag aliases the reader's internal buffer —
		// trim the newline and hand it straight to the caller; no copy.
		return trimNewline(frag), false, err
	}

	// Slow path: the line is longer than the reader buffer. Track the
	// content length (excluding the line-terminating '\n') in `content` —
	// this is what the threshold is measured against, matching parseTail's
	// maxCarryBytes which also counts newline-free content, so the forward
	// and reverse readers drop the exact same lines. Only retain bytes in
	// `buf` while still under the cap: once content crosses maxJSONLLineBytes
	// the line is doomed to be skipped, so we stop growing buf and just drain
	// to the newline — peak retained memory stays ~maxJSONLLineBytes instead
	// of the 2× a doubling append would reach on a multi-MB line.
	//
	// A mid-line fragment (ErrBufferFull) never contains '\n', so all its
	// bytes are content; only the final fragment carries the terminator,
	// which trimNewline strips before counting.
	content := len(frag)
	buf := append([]byte(nil), frag...)
	for {
		frag, err = r.ReadSlice('\n')
		full := errors.Is(err, bufio.ErrBufferFull)
		if full {
			content += len(frag)
		} else {
			content += len(trimNewline(frag))
		}
		if content <= maxJSONLLineBytes {
			buf = append(buf, frag...)
		}
		if !full {
			break
		}
		if content > maxJSONLLineBytes {
			// Drain the rest of this line without retaining it. err lands on
			// nil (newline consumed, more lines may follow) or io.EOF (file
			// ended mid-oversized-line); the caller treats both correctly.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			return nil, true, err
		}
	}
	if content > maxJSONLLineBytes {
		return nil, true, err
	}
	return trimNewline(buf), false, err
}

// trimNewline drops a single trailing '\n' (and a preceding '\r') so callers
// see the same line bytes a bufio.Scanner would have produced.
func trimNewline(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n := len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	return b
}

// uuidFromClaudeLine returns a stable UUID for a single-entry
// Claude JSONL line (user records today). Prefers Claude's own uuid
// field; falls back to DeriveLegacyUUID keyed on (time + type +
// summary + detail) when the line predates uuid or the field is
// missing.
//
// Output shape matches cli.newEventUUID (32 lowercase hex chars):
// when Claude supplies a dash-separated UUID we strip dashes so
// both code paths produce the same width (MergedSource compares
// the string verbatim).
func uuidFromClaudeLine(hl historyLine, ts int64, typ, summary, detail string) string {
	if u := normalizeClaudeUUID(hl.UUID); u != "" {
		return u
	}
	return textutil.DeriveLegacyUUID(ts, typ, summary, detail)
}

// uuidFromClaudeBlock handles assistant records whose line-level UUID
// covers ALL text blocks collectively. When the JSONL line has a
// uuid, we derive per-block identities by hashing (line UUID +
// block index) so two text blocks at the same timestamp stay
// distinguishable. Missing uuid falls back to DeriveLegacyUUID over
// (ts + block index + summary) — the index keeps distinct blocks in
// the same line from collapsing.
func uuidFromClaudeBlock(hl historyLine, blockIndex int, ts int64, typ, summary, detail string) string {
	if u := normalizeClaudeUUID(hl.UUID); u != "" {
		// For multi-block assistant lines, first block inherits the
		// line UUID directly (the common case — 1 text block per
		// line). Subsequent blocks rehash so they don't collide.
		if blockIndex == 0 {
			return u
		}
		return textutil.DeriveLegacyUUID(ts, typ, u+"#"+intToA(blockIndex), detail)
	}
	return textutil.DeriveLegacyUUID(ts, typ, summary+"#"+intToA(blockIndex), detail)
}

// normalizeClaudeUUID strips dashes from a Claude-style UUID so its
// shape matches cli.newEventUUID's dashless 32-char hex. An empty
// input returns empty. Non-hex input returns empty so the caller
// falls back to DeriveLegacyUUID rather than produce a garbage key.
func normalizeClaudeUUID(u string) string {
	if u == "" {
		return ""
	}
	// R236-PERF-1: stack [32]byte avoids per-row alloc on JSONL replay.
	var b [32]byte
	n := 0
	for i := 0; i < len(u); i++ {
		c := u[i]
		if c == '-' {
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return ""
		}
		if n >= 32 {
			return ""
		}
		// Lowercase ASCII hex.
		if c >= 'A' && c <= 'F' {
			c = c + 32
		}
		b[n] = c
		n++
	}
	if n != 32 {
		return ""
	}
	return string(b[:])
}

// intToA is a minimal int-to-string without pulling strconv just
// for the block-index formatter. Values here are small (< 10).
func intToA(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	if neg {
		out = append([]byte{'-'}, out...)
	}
	return string(out)
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
