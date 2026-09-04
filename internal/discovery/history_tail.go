package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// tailChunkSize is the reverse-read chunk size. 256KB balances syscall count
// against read-ahead amplification; most JSONL lines are 200-2000 bytes.
const tailChunkSize = 256 * 1024

// maxTailReadBytes caps how many trailing bytes parseTail will scan, since
// stat.Size() is untrustworthy on FUSE / /proc mounts and a size=TB report
// would otherwise spin size/tailChunkSize iterations. The tail needs ~2MB
// worst-case, so 128MB is generous; beyond it a warning notes the truncation.
// A var (not const) so the budget-bounds regression test can dial it down.
var maxTailReadBytes int64 = 128 * 1024 * 1024

// LoadHistoryTailCtx reads up to `limit` recent user/assistant entries from a
// session JSONL by seeking from EOF backward and parsing only the tail (~30ms
// for a 50MB file at limit=500 versus ~1-2s for LoadHistory), returned in
// chronological order. `limit <= 0` falls back to LoadHistory. Cancellation
// is checked between chunks and lines so Shutdown can interrupt a hung NFS read.
func LoadHistoryTailCtx(ctx context.Context, claudeDir, sessionID, cwd string, limit int) ([]clievent.EventEntry, error) {
	return LoadHistoryTailBeforeCtx(ctx, claudeDir, sessionID, cwd, 0, limit)
}

// LoadHistoryTailBeforeCtx returns up to `limit` entries strictly older than
// `beforeMS` (unix ms) from the tail of the session JSONL, in chronological
// order, driving "load earlier" pagination once the in-memory ring lacks the
// page. beforeMS <= 0 means "no upper bound" (LoadHistoryTailCtx); limit <= 0
// falls back to the full-file LoadHistory.
func LoadHistoryTailBeforeCtx(ctx context.Context, claudeDir, sessionID, cwd string, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
	if limit <= 0 {
		return LoadHistory(claudeDir, sessionID, cwd)
	}

	path, err := resolveJSONLPath(claudeDir, sessionID, cwd)
	if err != nil || path == "" {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open session jsonl %s: %w", path, err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat session jsonl %s: %w", path, err)
	}
	size := stat.Size()
	if size == 0 {
		return nil, nil
	}
	// parseTail's scanBudget bounds the work even when stat.Size() lies
	// (FUSE / /proc); the real size is still passed so the reverse read
	// seeks from genuine EOF. Warn so the operator sees the truncation.
	if size > maxTailReadBytes {
		slog.Warn("history tail: capping scan window on oversize file",
			"path", path, "size", size, "cap", maxTailReadBytes)
	}

	entries, err := parseTail(ctx, f, size, beforeMS, limit)
	if err != nil {
		return nil, fmt.Errorf("parse tail %s: %w", path, err)
	}
	return entries, nil
}

// resolveJSONLPath mirrors LoadHistory's path resolution without parsing the
// file, so both entry points share the same lookup semantics.
//
// Non-UUID sessionIDs are rejected up front so an attacker-controlled
// prev-session-id cannot produce a `filepath.Join(..., "../etc/passwd.jsonl")` escape.
func resolveJSONLPath(claudeDir, sessionID, cwd string) (string, error) {
	if !IsValidSessionID(sessionID) {
		return "", nil
	}
	if cwd != "" {
		candidate := filepath.Join(claudeDir, "projects", projDirName(cwd), sessionID+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	path, err := findSessionJSONL(claudeDir, sessionID)
	if err != nil {
		return "", err
	}
	return path, nil
}

// parseTail does the reverse chunked read + parse, accumulating lines from
// newest to oldest until `limit` entries are collected or the file head is
// reached; with beforeMS > 0, entries at Time >= beforeMS are skipped without
// counting. `carry` holds a line spanning the chunk boundary and is prepended
// to the next (older) chunk; the result is reversed so callers see chronology.
func parseTail(ctx context.Context, f *os.File, size int64, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
	// Over-collect slightly: assistant lines may contribute 0 text blocks
	// (tool_use / thinking filtered out), so a small cushion avoids a
	// second pass when the newest lines are tool-heavy.
	target := limit + limit/4
	if target < limit+8 {
		target = limit + 8
	}

	var (
		entries = make([]clievent.EventEntry, 0, target)
		carry   []byte // unterminated head fragment from prior chunk
		offset  = size
		buf     = make([]byte, tailChunkSize)
	)
	// scanBudget bounds the bytes touched when stat.Size() lies (FUSE / /proc)
	// or every line is rejected; normal paths stop via len(entries) >= target
	// long before an unbounded loop could spin ~4M iterations.
	scanBudget := size
	if scanBudget > maxTailReadBytes {
		scanBudget = maxTailReadBytes
	}

	for offset > 0 && len(entries) < target && scanBudget > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		chunkSize := int64(tailChunkSize)
		if offset < chunkSize {
			chunkSize = offset
		}
		offset -= chunkSize
		scanBudget -= chunkSize

		readBuf := buf[:chunkSize]
		if _, err := f.ReadAt(readBuf, offset); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("readAt offset=%d size=%d: %w", offset, chunkSize, err)
		}

		// Join this chunk with the fragment carried from the newer chunk. When
		// offset > 0 the first line of `chunk` may be partial (its head lives
		// in an older chunk) and becomes the next carry.
		chunk := readBuf
		if len(carry) > 0 {
			joined := make([]byte, 0, len(chunk)+len(carry))
			joined = append(joined, chunk...)
			joined = append(joined, carry...)
			chunk = joined
			carry = nil
		}

		// Walk backward via LastIndexByte('\n') rather than bytes.Split, which
		// allocates a header for every line even when we exit after a few.
		end := len(chunk)
		for end > 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			nl := bytes.LastIndexByte(chunk[:end], '\n')
			lineStart := nl + 1 // 0 when no newline left in remaining prefix
			// Before the file head, the prefix before the first '\n' is a partial
			// line whose head lives in an older chunk: stash as carry, stop.
			if nl < 0 && offset > 0 {
				// Cap carry growth so a pathologically long line (corrupt JSONL
				// with MBs of base64 and no newline) cannot drive this to O(N)
				// RAM. Past the cap the prefix is dropped; it would fail
				// parseHistoryLine anyway.
				const maxCarryBytes = 4 * 1024 * 1024
				if len(carry)+end > maxCarryBytes {
					carry = nil
					break
				}
				carry = append(carry, chunk[:end]...)
				break
			}
			line := chunk[lineStart:end]
			end = nl // on next iter, walk backward past this newline
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			lineEntries, ok := parseHistoryLine(line)
			if !ok {
				continue
			}
			// parseHistoryLine returns a line's entries in chronological order;
			// walking newest→oldest, append them reversed. Entries with
			// Time >= beforeMS are newer than the cursor and skipped.
			for j := len(lineEntries) - 1; j >= 0; j-- {
				e := lineEntries[j]
				if beforeMS > 0 && e.Time >= beforeMS {
					continue
				}
				entries = append(entries, e)
				if len(entries) >= target {
					break
				}
			}
			if len(entries) >= target {
				break
			}
		}
	}

	if len(entries) == 0 {
		return nil, nil
	}

	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	// Trim to exact limit from the end (keep the newest `limit` entries).
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// userTypeGate / asstTypeGate are cheap byte substrings that reject lines
// which cannot be "user" / "assistant" records before paying for a
// json.Unmarshal (most of a multi-MB run log is tool_use / system / thinking).
// A pure fast-negative: a marker inside an unrelated string costs at most a
// wasted unmarshal, never a wrong result. Compact and spaced forms both match.
var (
	userTypeGate       = []byte(`"type":"user"`)
	userTypeGateSpaced = []byte(`"type": "user"`)
	asstTypeGate       = []byte(`"type":"assistant"`)
	asstTypeGateSpaced = []byte(`"type": "assistant"`)
)

// parseHistoryLine decodes a single JSONL line into zero or more EventEntry
// values. Returns ok=false for malformed lines so callers can skip them
// silently (matches parseJSONL's tolerance for partially-flushed tails).
func parseHistoryLine(line []byte) ([]clievent.EventEntry, bool) {
	// Fast-negative byte gate; see userTypeGate.
	if !bytes.Contains(line, userTypeGate) && !bytes.Contains(line, userTypeGateSpaced) &&
		!bytes.Contains(line, asstTypeGate) && !bytes.Contains(line, asstTypeGateSpaced) {
		return nil, false
	}

	var hl historyLine
	if err := json.Unmarshal(line, &hl); err != nil {
		slog.Debug("skip malformed tail history line", "err", err)
		return nil, false
	}
	ts := parseTimestamp(hl.Timestamp)
	// Drop records with a missing/unparseable timestamp: a Time=0 entry
	// survives the strict-< pagination filter and pins the LoadBefore cursor
	// at before=0, which degrades to a newest-tail read repeating seen
	// entries. codexjsonl/kirojsonl drop ts<=0 for the same reason.
	if ts <= 0 {
		return nil, false
	}

	switch hl.Type {
	case "user":
		var msg historyMessage
		if err := json.Unmarshal(hl.Message, &msg); err != nil {
			return nil, false
		}
		text, images := extractTextAndImages(msg.Content)
		// Drop only when there is nothing to show; an image-only turn is still
		// surfaced, and the system-injected check only applies when text exists.
		if (text == "" && len(images) == 0) || (text != "" && IsClaudeSystemInjectedText(text)) {
			return nil, false
		}
		summary := textutil.TruncateRunes(text, 120)
		detail := textutil.TruncateRunes(text, 2000)
		e := clievent.EventEntry{
			UUID:    uuidFromClaudeLine(hl, ts, "user", summary, detail),
			Time:    ts,
			Type:    "user",
			Summary: summary,
			Detail:  detail,
		}
		// ImagePaths stays empty: the Claude JSONL carries no workspace-relative
		// path, so the lightbox falls back to the thumbnail data URI. Images are
		// not part of the UUID key, so MergedSource still prefers the local copy.
		if len(images) > 0 {
			e.Images = images
		}
		return []clievent.EventEntry{e}, true

	case "assistant":
		var msg historyMessage
		if err := json.Unmarshal(hl.Message, &msg); err != nil {
			return nil, false
		}
		var blocks []historyBlock
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			return nil, false
		}
		out := make([]clievent.EventEntry, 0, len(blocks))
		for idx, b := range blocks {
			if b.Type != "text" || strings.TrimSpace(b.Text) == "" {
				continue
			}
			summary := textutil.TruncateRunes(b.Text, 120)
			detail := textutil.TruncateRunes(b.Text, 16000)
			out = append(out, clievent.EventEntry{
				UUID:    uuidFromClaudeBlock(hl, idx, ts, "text", summary, detail),
				Time:    ts,
				Type:    "text",
				Summary: summary,
				Detail:  detail,
			})
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	}
	return nil, false
}

// LoadHistoryChainTailCtx walks a prev-session-ID chain (stored oldest →
// newest) from newest to oldest and collects up to `limit` entries total,
// returning chronological order. A long-lived chat's chain can be 32 IDs;
// the reverse walk typically opens only 1-2 files instead of loading every
// JSONL to discard all but the last 500 entries.
func LoadHistoryChainTailCtx(ctx context.Context, claudeDir string, ids []string, cwd string, limit int) []clievent.EventEntry {
	if limit <= 0 || len(ids) == 0 || claudeDir == "" {
		return nil
	}

	type bucket struct {
		id      string
		entries []clievent.EventEntry
	}
	var buckets []bucket
	remaining := limit

	for i := len(ids) - 1; i >= 0 && remaining > 0; i-- {
		if err := ctx.Err(); err != nil {
			break
		}
		id := ids[i]
		if id == "" {
			continue
		}
		// Reject non-UUID IDs here too (resolveJSONLPath also does) to skip
		// the file-open path and keep the attack surface narrow.
		if !IsValidSessionID(id) {
			continue
		}
		entries, err := LoadHistoryTailCtx(ctx, claudeDir, id, cwd, remaining)
		if err != nil {
			slog.Debug("chain tail load failed", "id", id, "err", err)
			continue
		}
		if len(entries) == 0 {
			continue
		}
		buckets = append(buckets, bucket{id: id, entries: entries})
		remaining -= len(entries)
	}

	if len(buckets) == 0 {
		return nil
	}

	// Flatten buckets oldest-first so the result is chronological.
	totalLen := 0
	for _, b := range buckets {
		totalLen += len(b.entries)
	}
	out := make([]clievent.EventEntry, 0, totalLen)
	for i := len(buckets) - 1; i >= 0; i-- {
		out = append(out, buckets[i].entries...)
	}
	return out
}

// LoadHistoryChainBeforeCtx walks the chain newest→oldest and collects up to
// `limit` entries strictly older than beforeMS, for dashboard "load earlier"
// pagination once the in-memory ring is exhausted. beforeMS <= 0 degenerates
// to LoadHistoryChainTailCtx; limit <= 0 or empty ids yields nil. Entries are
// chronological within a bucket and buckets are concatenated oldest chain ID
// first; cross-bucket re-sorting across branched sessions is the caller's job.
func LoadHistoryChainBeforeCtx(ctx context.Context, claudeDir string, ids []string, cwd string, beforeMS int64, limit int) []clievent.EventEntry {
	if limit <= 0 || len(ids) == 0 || claudeDir == "" {
		return nil
	}
	if beforeMS <= 0 {
		return LoadHistoryChainTailCtx(ctx, claudeDir, ids, cwd, limit)
	}

	type bucket struct {
		id      string
		entries []clievent.EventEntry
	}
	var buckets []bucket
	remaining := limit

	for i := len(ids) - 1; i >= 0 && remaining > 0; i-- {
		if err := ctx.Err(); err != nil {
			break
		}
		id := ids[i]
		if id == "" {
			continue
		}
		if !IsValidSessionID(id) {
			continue
		}
		entries, err := LoadHistoryTailBeforeCtx(ctx, claudeDir, id, cwd, beforeMS, remaining)
		if err != nil {
			slog.Debug("chain tail before load failed", "id", id, "err", err)
			continue
		}
		if len(entries) == 0 {
			continue
		}
		buckets = append(buckets, bucket{id: id, entries: entries})
		remaining -= len(entries)
	}

	if len(buckets) == 0 {
		return nil
	}

	totalLen := 0
	for _, b := range buckets {
		totalLen += len(b.entries)
	}
	out := make([]clievent.EventEntry, 0, totalLen)
	for i := len(buckets) - 1; i >= 0; i-- {
		out = append(out, buckets[i].entries...)
	}
	return out
}
