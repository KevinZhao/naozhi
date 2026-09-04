package discovery

import (
	"bufio"
	"encoding/base64"
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

// ThumbnailFn turns raw image bytes into a small JPEG data URI capped at
// maxDim pixels on the long edge, returning "" when the image cannot be
// decoded or is too large. It is an injection point (wired by
// internal/history/claudejsonl to cli.MakeThumbnail) so discovery stays a
// leaf package that does not import internal/cli. When nil, image blocks in
// history are silently skipped.
var ThumbnailFn func(data []byte, maxDim int) string

// historyThumbMaxDim matches the live-message path (process_send.go's
// buildUserEntry) so rehydrated history thumbnails render identically.
const historyThumbMaxDim = 600

// dataURIPrefix gates ThumbnailFn output: only well-formed image data URIs
// are surfaced to the dashboard, matching the live path's sanitisation.
const dataURIPrefix = "data:image/"

// historyLine is the minimal schema for a ~/.claude/projects/.../{sessionId}.jsonl line.
//
// UUID is Claude's own record identifier; naozhi adopts it as EventEntry.UUID
// so MergedSource can dedup against the naozhi-native copy of the same turn.
// When absent, DeriveLegacyUUID produces a stable fallback.
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
	Type   string          `json:"type"`
	Text   string          `json:"text"`
	Name   string          `json:"name"`             // tool_use
	Input  json.RawMessage `json:"input"`            // tool_use
	Source *imageSource    `json:"source,omitempty"` // image
}

// imageSource is the source object of a Claude content image block:
// {"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"..."}}.
type imageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // image/jpeg, image/png, ...
	Data      string `json:"data"`       // base64-encoded original bytes
}

// LoadHistory finds and parses the JSONL for sessionID under claudeDir/projects/,
// returning EventEntries for user and assistant messages.
// If cwd is non-empty, the JSONL is located directly via the CWD-based path (O(1));
// an empty cwd falls back to scanning all project directories.
func LoadHistory(claudeDir, sessionID, cwd string) ([]clievent.EventEntry, error) {
	// Path-traversal guard: reject non-UUID sessionIDs before joining into a
	// filepath (mirrors resolveJSONLPath) so "../../etc/passwd" cannot
	// escape claudeDir/projects.
	if !IsValidSessionID(sessionID) {
		return nil, nil
	}
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

// findSessionJSONL searches claudeDir/projects/**/{sessionID}.jsonl via
// DefaultScanner's pathCache. Tests needing isolation use NewScanner().
func findSessionJSONL(claudeDir, sessionID string) (string, error) {
	return DefaultScanner().findSessionJSONL(claudeDir, sessionID)
}

// findSessionJSONL performs the O(projects) fan-out scan, fronted by the
// per-Scanner pathCache. A positive hit is re-validated with one os.Stat and
// evicted on failure (claude CLI renames/deletes JSONLs during compaction);
// a fresh negative hit returns ("", nil) without touching disk so a startup
// burst of resume-chain lookups for the same missing ID shares one scan.
func (s *Scanner) findSessionJSONL(claudeDir, sessionID string) (string, error) {
	key := pathCacheKey(claudeDir, sessionID)

	if path, ok := s.pathCacheLookup(key); ok {
		if path == "" {
			return "", nil
		}
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		// Stale positive entry — file moved or deleted; evict and rescan.
		s.pathCacheInvalidate(key)
	}

	projectsDir := filepath.Join(claudeDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Cache the negative so a missing projects dir doesn't rerun ReadDir per lookup.
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

// pathCacheKey packs claudeDir + sessionID into a struct key, avoiding the
// per-call string concatenation a packed "dir\x00id" key would cost.
func pathCacheKey(claudeDir, sessionID string) pathKey {
	return pathKey{dir: claudeDir, id: sessionID}
}

// pathCacheLookup returns (path, true) on a hit, ("", false) on a miss
// or an expired negative entry. Hot path: takes only RLock.
func (s *Scanner) pathCacheLookup(key pathKey) (string, bool) {
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

// pathCacheStorePositive commits a resolved path, evicting first when the
// map is at cap.
func (s *Scanner) pathCacheStorePositive(key pathKey, path string) {
	s.pathCache.Lock()
	defer s.pathCache.Unlock()
	if len(s.pathCache.entries) >= pathCacheMaxEntries {
		s.evictPathCacheLocked()
	}
	s.pathCache.entries[key] = pathCacheEntry{path: path}
}

// pathCacheStoreNegative commits a "not found" verdict with a bounded TTL so
// a later-created JSONL is still picked up.
func (s *Scanner) pathCacheStoreNegative(key pathKey) {
	s.pathCache.Lock()
	defer s.pathCache.Unlock()
	if len(s.pathCache.entries) >= pathCacheMaxEntries {
		s.evictPathCacheLocked()
	}
	s.pathCache.entries[key] = pathCacheEntry{
		negativeUntil: time.Now().Add(pathCacheNegativeTTL),
	}
}

// pathCacheInvalidate drops an entry whose cached path has vanished from disk.
func (s *Scanner) pathCacheInvalidate(key pathKey) {
	s.pathCache.Lock()
	delete(s.pathCache.entries, key)
	s.pathCache.Unlock()
}

// evictPathCacheLocked enforces pathCacheMaxEntries. Caller MUST hold
// s.pathCache.Lock(). First pass drops expired negative entries; if still
// above cap (all positive or fresh-negative) it drops arbitrary entries —
// Go's randomised map iteration makes that effectively random eviction,
// allocation-free and cheaper than an LRU list would be.
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
	// Second pass: drop arbitrary entries until below cap, leaving
	// pathCacheEvictBatch headroom so the next store isn't forced to re-evict.
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
	// bufio.Reader, not bufio.Scanner: the CLI inlines uploaded images as
	// base64 on a single NDJSON line (5-10 MB), and Scanner would abort the
	// whole file with "token too long". readJSONLLine skips just that line;
	// its 4 MB threshold matches parseTail's maxCarryBytes so both readers
	// drop the same lines.
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

// maxJSONLLineBytes caps a JSONL line's content length (excluding '\n')
// before readJSONLLine skips it. Equal to parseTail's maxCarryBytes so the
// forward and reverse readers drop exactly the same oversized lines.
const maxJSONLLineBytes = 4 * 1024 * 1024

// readJSONLLine reads one '\n'-terminated line from r. A normal line is
// returned without its '\n' (the slice is only valid until the next read); a
// line over maxJSONLLineBytes is drained and reported as oversized=true with
// nil bytes, retaining at most ~maxJSONLLineBytes of memory; at EOF the final
// unterminated line (if any) is returned with io.EOF. Any other non-nil err
// is a real read failure.
func readJSONLLine(r *bufio.Reader) (line []byte, oversized bool, err error) {
	frag, err := r.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		// Fast path: a complete line fit in the reader's buffer (or EOF / real
		// error). frag aliases the internal buffer; hand it over without copying.
		return trimNewline(frag), false, err
	}

	// Slow path: the line exceeds the reader buffer. `content` counts bytes
	// excluding the terminator (like parseTail's maxCarryBytes) and buf only
	// grows while under the cap, so an oversized line is drained, not retained.
	// Mid-line fragments never contain '\n'; only the final one carries it.
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
			// Drain the rest of the line without retaining it; err ends as nil
			// or io.EOF, both handled by the caller.
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

// uuidFromClaudeLine returns a stable UUID for a single-entry Claude JSONL
// line: Claude's own uuid (dashes stripped to match cli.newEventUUID's 32
// hex chars, which MergedSource compares verbatim), or DeriveLegacyUUID over
// (time + type + summary + detail) when it is missing.
func uuidFromClaudeLine(hl historyLine, ts int64, typ, summary, detail string) string {
	if u := normalizeClaudeUUID(hl.UUID); u != "" {
		return u
	}
	return textutil.DeriveLegacyUUID(ts, typ, summary, detail)
}

// uuidFromClaudeBlock derives per-block identities for assistant records,
// whose line-level UUID covers all text blocks. Block 0 inherits the line
// UUID (the common single-block case); later blocks hash (line UUID + block
// index) so two text blocks at the same timestamp stay distinct. A missing
// uuid falls back to DeriveLegacyUUID over (ts + block index + summary).
func uuidFromClaudeBlock(hl historyLine, blockIndex int, ts int64, typ, summary, detail string) string {
	if u := normalizeClaudeUUID(hl.UUID); u != "" {
		if blockIndex == 0 {
			return u
		}
		return textutil.DeriveLegacyUUID(ts, typ, u+"#"+intToA(blockIndex), detail)
	}
	return textutil.DeriveLegacyUUID(ts, typ, summary+"#"+intToA(blockIndex), detail)
}

// normalizeClaudeUUID strips dashes from a Claude-style UUID so its shape
// matches cli.newEventUUID's dashless 32-char hex. Empty or non-hex input
// returns "" so the caller falls back to DeriveLegacyUUID.
func normalizeClaudeUUID(u string) string {
	if u == "" {
		return ""
	}
	// Stack buffer avoids a per-row alloc on JSONL replay.
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

// extractText handles content that is either a plain string or []block; thin
// wrapper over extractTextAndImages for callers that only want text.
func extractText(raw json.RawMessage) string {
	text, _ := extractTextAndImages(raw)
	return text
}

// extractTextAndImages decodes message content (a plain string or []block)
// into its concatenated text plus thumbnail data URIs of any image blocks;
// an image that cannot be thumbnailed is skipped without dropping the text.
func extractTextAndImages(raw json.RawMessage) (string, []string) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []historyBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil
	}
	var parts []string
	var images []string
	for _, b := range blocks {
		switch {
		case b.Type == "text" && b.Text != "":
			parts = append(parts, b.Text)
		case b.Type == "image" && b.Source != nil &&
			b.Source.Type == "base64" && b.Source.Data != "":
			thumb := thumbnailFromBase64(b.Source.Data)
			if thumb != "" {
				images = append(images, thumb)
			}
		}
	}
	return strings.Join(parts, "\n"), images
}

// thumbnailFromBase64 decodes a base64 image payload and downsamples it to a
// data URI, returning "" if ThumbnailFn is unset, the base64 is malformed, or
// the image cannot be thumbnailed.
func thumbnailFromBase64(b64 string) string {
	if ThumbnailFn == nil {
		return ""
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(data) == 0 {
		return ""
	}
	thumb := ThumbnailFn(data, historyThumbMaxDim)
	if thumb == "" || !strings.HasPrefix(thumb, dataURIPrefix) {
		return ""
	}
	return thumb
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
