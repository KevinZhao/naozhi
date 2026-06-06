// Package naozhilog implements history.Source backed by naozhi's
// own on-disk event log (internal/eventlog/persist). It is the
// "local" tier in MergedSource; the "fallback" tier is the
// Claude-CLI JSONL reader in internal/history/claudejsonl.
//
// Data flow (read path):
//
//	session.Router.EventEntriesBeforeCtx
//	  → merged.Source.LoadBefore
//	    → naozhilog.Source.LoadBefore  (this file)
//	      → persist.ReadAllIdx + log frame decode
//	    → claudejsonl.Source.LoadBefore  (fallback)
//
// naozhilog stores cli.EventEntry records in their persistence-layer
// envelope (schema.Record wrapping EventEntry JSON). The read path
// reverses the framing, decodes the envelope, and returns
// cli.EventEntry slices to callers so they can be fed back through
// ManagedSession.InjectHistory without further translation.
//
// Design constraints (from RFC §3.4):
//   - Concurrency-safe with the writer side: readers open their own
//     read-only fds and tolerate partial tails via the framing layer.
//   - Missing / empty files are NOT errors; they return empty slices
//     so the MergedSource fallback path is uniform.
//   - Version-mismatched files (schema.WireVersion drift) are
//     REFUSED, not salvaged, so upgrades can't silently parse a
//     subset of the new format.
package naozhilog

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// Source is an in-package history.Source backed by
// <keyhash>.log / <keyhash>.idx under a single directory. Constructed
// per-session so LoadLatest / LoadBefore share the same key→path
// resolution without a parameter each time.
type Source struct {
	dir string
	key string
}

// New builds a Source for dir and a specific session key. The key
// is used to derive the <keyhash>.log path; no file access happens
// until LoadLatest / LoadBefore is called.
//
// dir="" is a valid configuration — it disables reads entirely so a
// naozhi deployment that opts out of event-log persistence can keep
// using router.attachHistorySource unchanged. Both LoadLatest and
// LoadBefore return (nil, nil) for a disabled source.
func New(dir, key string) *Source {
	return &Source{dir: dir, key: key}
}

// bufReaderPool recycles the 64 KiB bufio.Reader used by the two log-decode
// paths (decodeFrom / readAllEntries). LoadLatest/LoadBefore previously
// allocated a fresh 64 KiB backing buffer on every call; pooling amortises
// that across the hot history-load path. R20260603-PERF-4.
//
// Callers MUST Reset(f) before use and Reset(nil) before returning a reader
// to the pool — the latter drops the *os.File reference so a pooled reader
// can never pin a closed fd.
var bufReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 64*1024) },
}

// LoadLatest returns up to `limit` newest entries from the session's
// log file, in chronological order.
//
// Implementation:
//  1. Use the idx sparse index's last-entry TimeMS as a seed "before"
//     value, then walk backwards through idx + log bytes. In practice
//     for LoadLatest (limit ≈ 500) we just do a full log scan +
//     tail-cut — the log caps at ~100 MiB rotate-protected, so a full
//     scan is still < 200ms on ext4/gp3. Keeping the code simple
//     beats a marginal speedup here; LoadBefore handles the hot
//     pagination case with idx-driven seeking.
//  2. Decode every record; keep `limit` newest.
//
// Called once per session at startup by session.Router to pre-fill
// persistedHistory, so its performance budget is "20-50ms per
// session × number of resurrected sessions" — acceptable.
func (s *Source) LoadLatest(ctx context.Context, limit int) ([]cli.EventEntry, error) {
	if s == nil || s.dir == "" || s.key == "" || limit <= 0 {
		return nil, nil
	}
	all, err := s.readAllEntries(ctx)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

// LoadBefore returns up to `limit` entries strictly older than
// beforeMS (unix ms), in chronological order. Used by the dashboard
// "load earlier" pagination path via MergedSource.LoadBefore.
//
// Implementation (R20260530-PERF-1 / #1485):
//   - Use the .idx sparse index to seek to a byte offset close to
//     beforeMS, then decode forward only the tail window needed to fill
//     `limit` — avoiding the previous O(pages × full-file) rescan that
//     ignored the idx entirely. The dashboard "load earlier" path pages
//     backward on suspended/dead sessions, so each page used to re-scan
//     and re-decode the whole <keyhash>.log.
//   - On any idx miss (no idx file, empty idx, idx/log drift, decode
//     error) the method falls back to the full-scan path so correctness
//     never depends on the index being present or current.
//
// beforeMS <= 0 is treated as "no upper bound" and falls through to
// LoadLatest's behavior.
func (s *Source) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]cli.EventEntry, error) {
	if s == nil || s.dir == "" || s.key == "" || limit <= 0 {
		return nil, nil
	}
	if beforeMS <= 0 {
		return s.LoadLatest(ctx, limit)
	}
	// Fast path: idx-guided seek + bounded forward decode.
	if out, ok, err := s.loadBeforeViaIdx(ctx, beforeMS, limit); err != nil {
		return nil, err
	} else if ok {
		return out, nil
	}
	// Fallback: full scan + filter.
	return s.loadBeforeFullScan(ctx, beforeMS, limit)
}

// loadBeforeFullScan is the index-free baseline: decode the whole log,
// filter to Time < beforeMS, keep the newest `limit`.
func (s *Source) loadBeforeFullScan(ctx context.Context, beforeMS int64, limit int) ([]cli.EventEntry, error) {
	all, err := s.readAllEntries(ctx)
	if err != nil {
		return nil, err
	}
	filtered := make([]cli.EventEntry, 0, len(all))
	for _, e := range all {
		if e.Time < beforeMS {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	return filtered, nil
}

// idxPath composes the sparse-index sidecar path for this source's key.
// persist.IdxPath was removed as dead code (DEADCODE-13), so we derive it
// the same way the package's own tests do: <keyhash>.idx alongside the log.
func (s *Source) idxPath() string {
	return filepath.Join(s.dir, persist.KeyHash(s.key)+".idx")
}

// loadBeforeViaIdx implements the idx-seek fast path. It returns
// (entries, true, nil) on success, (nil, false, nil) to signal the caller
// should fall back to a full scan, and (nil, false, err) only on a context
// cancellation surfaced mid-decode.
//
// Algorithm: idx entries are sorted by ByteOff with weakly-monotonic
// TimeMS (persist stamps each record now.UnixMilli() in append order).
// We locate the first idx entry whose TimeMS >= beforeMS — every record
// we want lives at or before its predecessor — then back up far enough
// (limit/stride + 1 idx slots) to guarantee the seek offset precedes the
// newest `limit` qualifying records. Decoding forward from that offset
// and keeping the last `limit` records with Time < beforeMS yields the
// exact same result as the full scan while touching only the tail window.
func (s *Source) loadBeforeViaIdx(ctx context.Context, beforeMS int64, limit int) ([]cli.EventEntry, bool, error) {
	idx, err := persist.ReadAllIdx(s.idxPath())
	if err != nil || len(idx) == 0 {
		// Missing/unreadable idx → fall back. ReadAllIdx returns an empty
		// slice (not error) for a missing file.
		return nil, false, nil
	}

	// Find the first idx entry at/after beforeMS. idx is ByteOff-ordered
	// with weakly-monotonic TimeMS. R112714-PERF-12: use binary search
	// (O(log n)) instead of the previous linear reverse scan (O(n)).
	// slices.BinarySearchFunc returns the first position where the
	// comparator is not negative, i.e. the first entry with TimeMS >=
	// beforeMS. On weakly-monotonic data this finds the correct boundary
	// (ties resolve to the leftmost tie, which is strictly safe: we may
	// start decoding a few records earlier than strictly necessary, which
	// is always correct — the decode-filter pass discards any record with
	// Time >= beforeMS regardless).
	boundary, _ := slices.BinarySearchFunc(idx, beforeMS, func(e schema.IdxEntry, target int64) int {
		return cmp.Compare(e.TimeMS, target)
	})
	if boundary == 0 {
		// Even the oldest indexed record is already >= beforeMS. There may
		// still be un-indexed records before the first idx entry (the idx
		// is sparse and its first non-header entry can be record #stride),
		// so fall back rather than risk returning an empty page wrongly.
		return nil, false, nil
	}

	// Back up enough idx slots that the seek offset is guaranteed to sit
	// before the newest `limit` qualifying records. Each idx slot covers at
	// least one record (worst case: every record indexed, IdxStride=1), so
	// backing up `limit + 2` slots always spans >= limit records. The stride
	// is per-Persister configurable and not knowable from the idx alone, so
	// we assume the densest case for correctness; a sparser real stride just
	// means we seek further back than strictly necessary (still bounded by
	// the small idx).
	slotsBack := limit + 2
	start := boundary - slotsBack
	if start < 0 {
		// Window reaches the head of the idx; the first idx entry may not
		// be the first record (sparse), so a full scan is needed to be
		// sure we don't drop the oldest qualifying records.
		return nil, false, nil
	}
	seekOff := idx[start].ByteOff

	entries, err := s.decodeFrom(ctx, seekOff)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, false, err
		}
		// Any decode/open failure → fall back to the full scan path.
		return nil, false, nil
	}

	filtered := make([]cli.EventEntry, 0, len(entries))
	for _, e := range entries {
		if e.Time < beforeMS {
			filtered = append(filtered, e)
		}
	}
	// Correctness guard: the seek window only sees records at/after seekOff.
	// We can only be SURE we captured the newest `limit` qualifying records
	// when at least `limit` of them landed in the window — then the newest
	// `limit` are unambiguously these (everything newer than them was
	// scanned to EOF). If fewer than `limit` qualify here, older candidates
	// may exist before seekOff, so defer to the full scan rather than
	// under-return. This makes idx purely a performance fast path that never
	// changes the result.
	if len(filtered) < limit {
		return nil, false, nil
	}
	filtered = filtered[len(filtered)-limit:]
	return filtered, true, nil
}

// decodeFrom opens the log, seeks to byteOff, and decodes every record
// from there to EOF into cli.EventEntry values (chronological order).
// byteOff must fall on a record-frame boundary (it always does when it
// comes from an idx entry's ByteOff).
func (s *Source) decodeFrom(ctx context.Context, byteOff int64) ([]cli.EventEntry, error) {
	path := persist.LogPath(s.dir, s.key)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open naozhi log %s: %w", path, err)
	}
	defer f.Close()

	if _, err := f.Seek(byteOff, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek naozhi log %s to %d: %w", path, byteOff, err)
	}

	br := bufReaderPool.Get().(*bufio.Reader)
	br.Reset(f)
	defer func() {
		br.Reset(nil) // release the *os.File so the pooled reader pins no fd
		bufReaderPool.Put(br)
	}()
	out := make([]cli.EventEntry, 0, 512)
	if err := decodeRecords(ctx, br, path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// readAllEntries decodes every record from the log file (skipping
// the header) and returns the resulting EventEntry slice in log
// order (= chronological, by construction of the persist layer).
//
// Missing file → (nil, nil). Corrupt / unsupported-version file →
// (nil, nil) + slog.Warn; the caller falls back to Claude JSONL.
// We never surface the error to the caller because that would
// prevent the fallback from running.
func (s *Source) readAllEntries(ctx context.Context) ([]cli.EventEntry, error) {
	path := persist.LogPath(s.dir, s.key)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open naozhi log %s: %w", path, err)
	}
	defer f.Close()

	br := bufReaderPool.Get().(*bufio.Reader)
	br.Reset(f)
	defer func() {
		br.Reset(nil) // release the *os.File so the pooled reader pins no fd
		bufReaderPool.Put(br)
	}()
	// Pre-allocate to the in-memory ring upper bound so the per-record
	// append loop (up to ~500 entries) avoids the repeated doubling
	// reallocations of a nil-start slice. 512 ≈ persistedHistory ring cap
	// (LoadLatest's limit ≈ 500); over-shooting reads still grow normally.
	// R20260530-PERF-3.
	const estEntries = 512
	out := make([]cli.EventEntry, 0, estEntries)
	if err := decodeRecords(ctx, br, path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeRecords decodes log frames from br (positioned at a frame
// boundary), appending the decoded cli.EventEntry values to *out in log
// (chronological) order. Header / empty / undecodable records are skipped
// with a warning; an unsupported wire version aborts the whole read and
// resets *out so the caller falls back to the Claude JSONL source. The
// only error returned is a context cancellation; every on-disk fault is
// handled internally so the merged-source fallback can still run.
//
// Shared by readAllEntries (whole-file) and decodeFrom (idx-seek window)
// so the framing / skip / UUID-warn contract stays identical across both
// read paths. R20260530-PERF-1 (#1485).
func decodeRecords(ctx context.Context, br *bufio.Reader, path string, out *[]cli.EventEntry) error {
	// R112714-PERF-7: check ctx.Err() every 32 records instead of on every
	// iteration. ctx.Err() acquires a mutex internally; at decode throughput
	// the per-record cost is measurable on long files.
	const ctxCheckInterval = 32
	var n int
	for {
		n++
		if n%ctxCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		rec, err := persist.ReadRecord(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, persist.ErrPartialTail) {
				// Writer is mid-write; stop gracefully.
				break
			}
			if errors.Is(err, schema.ErrUnsupportedVersion) {
				slog.Warn("naozhilog: unsupported wire version; skipping file",
					"path", path, "err", err)
				*out = (*out)[:0]
				return nil
			}
			// Any other decode error means the file is corrupt.
			// Log and stop; return what we have so partial reads
			// still provide value.
			slog.Warn("naozhilog: decode error; truncating read",
				"path", path, "err", err)
			break
		}

		// Skip header record.
		if rec.Type == schema.TypeHeader {
			continue
		}
		if len(rec.Entry) == 0 {
			continue
		}

		var entry cli.EventEntry
		if err := json.Unmarshal(rec.Entry, &entry); err != nil {
			slog.Warn("naozhilog: entry JSON decode failed; skipping",
				"path", path, "seq", rec.Seq, "err", err)
			continue
		}
		// Every persisted entry must have a UUID (stampUUID runs in
		// cli.EventLog.Append before the record reaches disk). A
		// missing UUID post-decode flags a producer bug or a
		// hand-edited file; we still emit the entry because dropping
		// it would lose history, but MergedSource dedup cannot anchor
		// against it, so log a warning once per file.
		if entry.UUID == "" {
			slog.Warn("naozhilog: entry missing UUID post-decode; dedup may regress",
				"path", path, "seq", rec.Seq, "time", entry.Time)
		}
		*out = append(*out, entry)
	}
	return nil
}
