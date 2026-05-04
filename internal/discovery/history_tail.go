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

	"github.com/naozhi/naozhi/internal/cli"
)

// tailChunkSize is the size of each reverse-read chunk. 256KB balances
// syscall count against read-ahead amplification for typical SSD/NFS.
// Most JSONL lines are 200-2000 bytes, so one chunk covers ~100-1000 lines.
const tailChunkSize = 256 * 1024

// maxTailReadBytes caps how many trailing bytes parseTail will scan.
// `stat.Size()` returns unbounded values on FUSE / /proc and other special
// file systems; without a cap a malformed mount reporting size=TB would
// drive parseTail to spin `size / tailChunkSize` iterations against a file
// that may only yield a handful of real bytes per ReadAt. The tail-read
// mode only ever needs the last ~limit JSONL entries (default 500 ×
// ~4KB/entry = 2MB worst-case for the data we care about), so 128MB of
// scan budget is already 60× the realistic need and leaves generous room
// for lines that pad with huge tool-use payloads. Beyond this cap we seek
// from (size - maxTailReadBytes) and log a warning so the operator sees
// the truncation. R54-SEC-006 defence-in-depth.
//
// Declared var (not const) so the budget-bounds regression test can dial
// it down without waiting for 512 chunks of io.EOF responses under -race.
var maxTailReadBytes int64 = 128 * 1024 * 1024

// LoadHistoryTail reads up to `limit` recent user/assistant entries from
// a session JSONL by seeking from EOF backward and parsing only the tail.
// Stops as soon as the limit is reached or the file head is hit.
//
// Returns entries in chronological order (oldest → newest), matching
// the shape of LoadHistory.
//
// Unlike LoadHistory which scans the whole file even when InjectHistory
// will discard everything but the last 500 entries, this function touches
// only the bytes needed. For a 50MB JSONL with limit=500, typical cost
// drops from ~1-2s to ~30ms.
//
// `limit <= 0` falls back to the legacy full-file LoadHistory behaviour.
func LoadHistoryTail(claudeDir, sessionID, cwd string, limit int) ([]cli.EventEntry, error) {
	return LoadHistoryTailCtx(context.Background(), claudeDir, sessionID, cwd, limit)
}

// LoadHistoryTailCtx is the context-aware variant. Cancellation is checked
// between chunks and between parsed lines so a hung NFS read can still be
// interrupted by Shutdown (subject to the underlying ReadAt's own blocking).
func LoadHistoryTailCtx(ctx context.Context, claudeDir, sessionID, cwd string, limit int) ([]cli.EventEntry, error) {
	return LoadHistoryTailBeforeCtx(ctx, claudeDir, sessionID, cwd, 0, limit)
}

// LoadHistoryTailBeforeCtx returns up to `limit` entries strictly older than
// `beforeMS` (unix ms) from the tail of the session JSONL, in chronological
// order. Drives "load earlier" pagination when the in-memory ring no longer
// contains the requested page.
//
// beforeMS <= 0 is treated as "no upper bound" and is equivalent to
// LoadHistoryTailCtx — the function falls through to the plain tail read.
//
// limit <= 0 falls back to the full-file LoadHistory for parity with the
// legacy LoadHistoryTailCtx contract.
func LoadHistoryTailBeforeCtx(ctx context.Context, claudeDir, sessionID, cwd string, beforeMS int64, limit int) ([]cli.EventEntry, error) {
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
	// Note the size when it exceeds maxTailReadBytes: parseTail's internal
	// byte budget (see scanBudget variable) bounds total scan work even when
	// stat.Size() returns an untrustworthy value (FUSE / /proc / corrupted
	// mount). We still pass the real size so the reverse-read seeks from
	// genuine EOF — the budget stops the loop before it can iterate 1TB /
	// 256KB = 4M times against a misbehaving mount. R54-SEC-006.
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

// resolveJSONLPath mirrors LoadHistory's path-resolution logic without
// parsing the file. Exposed as a helper so both LoadHistory and
// LoadHistoryTail share the exact same lookup semantics.
//
// Rejects non-UUID sessionIDs up front so a caller that threads
// attacker-controlled prev-session-id chain values through this helper
// cannot produce a `filepath.Join(..., "../etc/passwd.jsonl")` escape.
// Production callers validate IDs before storage, but this defence-in-depth
// prevents a regression from a future caller skipping validation.
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

// parseTail does the reverse chunked read + parse. It keeps accumulating
// lines from newest to oldest until `limit` decoded entries are collected
// or the file head is reached.
//
// When beforeMS > 0, entries whose Time >= beforeMS are skipped (newer than
// the pagination cursor) without counting against `limit` or `target`. The
// caller sees only strictly-older entries. A beforeMS of 0 disables the
// filter.
//
// Internal structure:
//   - `carry` holds bytes from the previous (newer) chunk that belong to
//     a line spanning the chunk boundary; it is prepended to the current
//     chunk before splitting.
//   - lines inside a chunk are walked from newest to oldest; each decoded
//     line becomes 0-N entries (user = 1, assistant = 0-N text blocks).
//   - the final result is reversed in-place so callers see chronological
//     order (oldest → newest), matching LoadHistory.
func parseTail(ctx context.Context, f *os.File, size int64, beforeMS int64, limit int) ([]cli.EventEntry, error) {
	// Over-collect slightly: assistant lines may contribute 0 text blocks
	// (tool_use / thinking filtered out), so a small cushion avoids a
	// second pass when the newest lines are tool-heavy.
	target := limit + limit/4
	if target < limit+8 {
		target = limit + 8
	}

	var (
		entries []cli.EventEntry // collected newest-first
		carry   []byte           // unterminated head fragment from prior chunk
		offset  = size
		buf     = make([]byte, tailChunkSize)
	)
	// scanBudget bounds how many trailing bytes the reverse-read will touch.
	// Normal paths terminate via `len(entries) >= target` long before this
	// cap, so the budget only matters when stat.Size() lies (FUSE / /proc
	// files claiming TB+ sizes) or parseHistoryLine rejects every line (file
	// has no JSONL structure). Without it a misbehaving mount could pin this
	// function on size / tailChunkSize = ~4M iterations. R54-SEC-006.
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

		// Join this chunk with the fragment carried over from the newer chunk.
		// When offset > 0, the first line of `chunk` is potentially a partial
		// line (head of a line whose tail lives in an even older chunk); we
		// stash it as the new carry for the next iteration.
		chunk := readBuf
		if len(carry) > 0 {
			joined := make([]byte, 0, len(chunk)+len(carry))
			joined = append(joined, chunk...)
			joined = append(joined, carry...)
			chunk = joined
			carry = nil
		}

		// Walk backward through chunk via LastIndexByte('\n'), avoiding the
		// O(lines) slice allocation that bytes.Split would produce (each
		// 256KB chunk holds 100-1000 lines; Split pre-allocates the full
		// [][]byte header array even when we early-exit after the first
		// few matches). Line bounds inside `chunk` are [lineStart, end).
		end := len(chunk)
		for end > 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			nl := bytes.LastIndexByte(chunk[:end], '\n')
			lineStart := nl + 1 // 0 when no newline left in remaining prefix
			// If we haven't reached the file head yet, the prefix before the
			// first '\n' is a partial line whose head lives in an older
			// chunk — stash it as carry and stop walking this chunk.
			if nl < 0 && offset > 0 {
				// Cap carry growth so a pathologically long single line
				// (e.g. a corrupt JSONL file with no newlines across MBs
				// of base64 output) cannot drive this function to O(N)
				// RAM. When the cap is hit we treat the remaining prefix
				// as unparseable and reset carry — the line would fail
				// parseHistoryLine downstream anyway, so dropping it here
				// is equivalent to discarding at parse time without the
				// memory blow-up. R58-PERF-F5.
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
			// parseHistoryLine returns entries in chronological order for
			// a single line (a single JSONL record). Since we're walking
			// newest→oldest, prepend the batch by reversing internally.
			// Entries with Time >= beforeMS are skipped silently (they're
			// newer than the pagination cursor) without counting against
			// the collection target. beforeMS == 0 disables the filter.
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

	// Reverse to chronological order.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	// Trim to exact limit from the end (keep the newest `limit` entries).
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// parseHistoryLine decodes a single JSONL line into zero or more EventEntry
// values. Returns ok=false for malformed lines so callers can skip them
// silently (matches parseJSONL's tolerance for partially-flushed tails).
func parseHistoryLine(line []byte) ([]cli.EventEntry, bool) {
	var hl historyLine
	if err := json.Unmarshal(line, &hl); err != nil {
		slog.Debug("skip malformed tail history line", "err", err)
		return nil, false
	}
	ts := parseTimestamp(hl.Timestamp)

	switch hl.Type {
	case "user":
		var msg historyMessage
		if err := json.Unmarshal(hl.Message, &msg); err != nil {
			return nil, false
		}
		text := extractText(msg.Content)
		if text == "" {
			return nil, false
		}
		return []cli.EventEntry{{
			Time:    ts,
			Type:    "user",
			Summary: cli.TruncateRunes(text, 120),
			Detail:  cli.TruncateRunes(text, 2000),
		}}, true

	case "assistant":
		var msg historyMessage
		if err := json.Unmarshal(hl.Message, &msg); err != nil {
			return nil, false
		}
		var blocks []historyBlock
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			return nil, false
		}
		out := make([]cli.EventEntry, 0, len(blocks))
		for _, b := range blocks {
			if b.Type != "text" || strings.TrimSpace(b.Text) == "" {
				continue
			}
			out = append(out, cli.EventEntry{
				Time:    ts,
				Type:    "text",
				Summary: cli.TruncateRunes(b.Text, 120),
				Detail:  cli.TruncateRunes(b.Text, 16000),
			})
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	}
	return nil, false
}

// LoadHistoryChainTail walks a prev-session-ID chain (in order oldest → newest
// as stored) from newest to oldest and collects up to `limit` entries total,
// stopping as soon as the budget is exhausted. Returns entries in
// chronological order.
//
// Motivation: on long-lived chats the chain can be 32 IDs long. Loading
// every JSONL only to discard all but the last 500 entries is wasteful.
// Walking in reverse and stopping when the budget is met typically opens
// only 1-2 files for a normal session.
func LoadHistoryChainTail(claudeDir string, ids []string, cwd string, limit int) []cli.EventEntry {
	return LoadHistoryChainTailCtx(context.Background(), claudeDir, ids, cwd, limit)
}

// LoadHistoryChainTailCtx is the context-aware variant.
func LoadHistoryChainTailCtx(ctx context.Context, claudeDir string, ids []string, cwd string, limit int) []cli.EventEntry {
	if limit <= 0 || len(ids) == 0 || claudeDir == "" {
		return nil
	}

	// Collect per-ID entry slices in reverse-walk order. Concatenating all
	// ids+flatten at the end is O(total) which matches the legacy behaviour.
	type bucket struct {
		id      string
		entries []cli.EventEntry
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
		// Skip non-UUID IDs defensively — resolveJSONLPath also rejects
		// them, but this saves an unnecessary file-open syscall path and
		// keeps any future caller from accidentally widening the attack
		// surface.
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

	// Buckets are in reverse walk order (newest chain ID first). Flatten in
	// the opposite direction so the final slice is chronological.
	totalLen := 0
	for _, b := range buckets {
		totalLen += len(b.entries)
	}
	out := make([]cli.EventEntry, 0, totalLen)
	for i := len(buckets) - 1; i >= 0; i-- {
		out = append(out, buckets[i].entries...)
	}
	return out
}

// LoadHistoryChainBeforeCtx walks the chain newest→oldest and collects up to
// `limit` entries strictly older than beforeMS. Used by dashboard "load
// earlier" pagination when the in-memory ring has been exhausted.
//
// beforeMS <= 0 degenerates to LoadHistoryChainTailCtx semantics (no upper
// bound, return newest `limit`). limit <= 0 or empty ids yields nil.
//
// Returns entries in chronological order. Entries' Time values are not
// re-sorted across bucket boundaries — the caller owns final ordering if
// it merges results with other sources. Within a bucket, chronological
// order is preserved by parseTail. Between buckets, the natural JSONL
// timestamps typically already monotonically decrease toward older chain
// IDs; tie-breaking across branched sessions is the caller's concern.
func LoadHistoryChainBeforeCtx(ctx context.Context, claudeDir string, ids []string, cwd string, beforeMS int64, limit int) []cli.EventEntry {
	if limit <= 0 || len(ids) == 0 || claudeDir == "" {
		return nil
	}
	if beforeMS <= 0 {
		return LoadHistoryChainTailCtx(ctx, claudeDir, ids, cwd, limit)
	}

	type bucket struct {
		id      string
		entries []cli.EventEntry
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
	out := make([]cli.EventEntry, 0, totalLen)
	for i := len(buckets) - 1; i >= 0; i-- {
		out = append(out, buckets[i].entries...)
	}
	return out
}
