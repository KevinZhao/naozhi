// Package naozhilog implements history.Source backed by naozhi's own on-disk
// event log (internal/eventlog/persist). It is the "local" tier in
// merged.Source; claudejsonl is the fallback.
//
// Records are clievent.EventEntry JSON wrapped in the persistence envelope
// (schema.Record); the read path reverses the framing so entries can be fed
// back through ManagedSession.InjectHistory unchanged. Readers open their own
// read-only fds and tolerate partial tails. Missing / empty files are not
// errors (empty slice) so the merged fallback path is uniform; a
// schema.WireVersion mismatch is refused rather than salvaged.
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

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// Source is a history.Source backed by <keyhash>.log / <keyhash>.idx under a
// single directory, constructed per session key.
type Source struct {
	dir string
	key string
}

// New builds a Source for dir and a session key; no file access happens until
// LoadLatest / LoadBefore. dir="" disables reads (both return (nil, nil)).
func New(dir, key string) *Source {
	return &Source{dir: dir, key: key}
}

// bufReaderPool recycles the 64 KiB bufio.Reader used by decodeFrom and
// readAllEntries. Callers MUST Reset(f) before use and Reset(nil) before
// returning it so a pooled reader never pins a closed fd.
var bufReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 64*1024) },
}

// LoadLatest returns up to `limit` newest entries from the session's log, in
// chronological order. It does a full log scan + tail-cut: the log is
// rotate-capped at ~100 MiB so a scan stays under ~200 ms, and it is called
// once per session at startup to pre-fill persistedHistory. LoadBefore
// handles the hot pagination case with idx-driven seeking.
func (s *Source) LoadLatest(ctx context.Context, limit int) ([]clievent.EventEntry, error) {
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

// LoadBefore returns up to `limit` entries strictly older than beforeMS (unix
// ms), in chronological order. It seeks via the .idx sparse index and decodes
// only the tail window needed; on any idx miss (no idx, empty, drift, decode
// error) it falls back to a full scan so correctness never depends on the
// index (#1485). beforeMS <= 0 means "no upper bound" (LoadLatest).
func (s *Source) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
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
func (s *Source) loadBeforeFullScan(ctx context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
	all, err := s.readAllEntries(ctx)
	if err != nil {
		return nil, err
	}
	filtered := make([]clievent.EventEntry, 0, len(all))
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

// idxPath is the sparse-index sidecar path: <keyhash>.idx alongside the log.
func (s *Source) idxPath() string {
	return filepath.Join(s.dir, persist.KeyHash(s.key)+".idx")
}

// loadBeforeViaIdx is the idx-seek fast path. It returns (entries, true, nil)
// on success, (nil, false, nil) when the caller should fall back to a full
// scan, and (nil, false, err) only for context cancellation mid-decode.
//
// idx entries are sorted by ByteOff with weakly-monotonic TimeMS. Locate the
// first idx entry with TimeMS >= beforeMS, back up enough slots to guarantee
// the seek offset precedes the newest `limit` qualifying records, then decode
// forward keeping the last `limit` records with Time < beforeMS — the same
// result as a full scan while touching only the tail window.
func (s *Source) loadBeforeViaIdx(ctx context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, bool, error) {
	idx, err := persist.ReadAllIdx(s.idxPath())
	if err != nil || len(idx) == 0 {
		// Missing/unreadable idx → fall back (ReadAllIdx returns empty, not an
		// error, for a missing file).
		return nil, false, nil
	}

	// Binary search for the first entry with TimeMS >= beforeMS. Ties resolve
	// leftmost, which is safe: decoding starts a few records early and the
	// filter pass discards anything with Time >= beforeMS.
	boundary, _ := slices.BinarySearchFunc(idx, beforeMS, func(e schema.IdxEntry, target int64) int {
		return cmp.Compare(e.TimeMS, target)
	})
	if boundary == 0 {
		// Even the oldest indexed record is >= beforeMS; un-indexed records may
		// precede the first (sparse) idx entry, so fall back.
		return nil, false, nil
	}

	// Each idx slot covers at least one record (worst case IdxStride=1), so
	// backing up `limit + 2` slots always spans >= limit records. The stride
	// is not knowable from the idx, so assume the densest case.
	slotsBack := limit + 2
	start := boundary - slotsBack
	if start < 0 {
		// Window reaches the head of the idx; a sparse idx may omit the first
		// records, so a full scan is needed.
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

	filtered := make([]clievent.EventEntry, 0, len(entries))
	for _, e := range entries {
		if e.Time < beforeMS {
			filtered = append(filtered, e)
		}
	}
	// The window only sees records at/after seekOff, so we can only be SURE
	// we have the newest `limit` qualifying records when at least `limit`
	// landed in it. Otherwise defer to the full scan rather than under-return.
	if len(filtered) < limit {
		return nil, false, nil
	}
	filtered = filtered[len(filtered)-limit:]
	return filtered, true, nil
}

// decodeFrom opens the log, seeks to byteOff (which must be a record-frame
// boundary, as an idx ByteOff is) and decodes every record to EOF.
func (s *Source) decodeFrom(ctx context.Context, byteOff int64) ([]clievent.EventEntry, error) {
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
	out := make([]clievent.EventEntry, 0, 512)
	if err := decodeRecords(ctx, br, path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// readAllEntries decodes every record in the log (skipping the header) in log
// order. Missing file → (nil, nil). Corrupt / unsupported-version files also
// yield (nil, nil) plus a warning, never an error, so the Claude JSONL
// fallback can still run.
func (s *Source) readAllEntries(ctx context.Context) ([]clievent.EventEntry, error) {
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
	// Pre-size to the persistedHistory ring cap (~500) to avoid append doubling.
	const estEntries = 512
	out := make([]clievent.EventEntry, 0, estEntries)
	if err := decodeRecords(ctx, br, path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeRecords decodes frames from br (at a frame boundary), appending to
// *out in log order. Header / empty / undecodable records are skipped with a
// warning; an unsupported wire version resets *out and aborts so the caller
// falls back to Claude JSONL. The only error returned is context
// cancellation. Shared by readAllEntries and decodeFrom (#1485).
func decodeRecords(ctx context.Context, br *bufio.Reader, path string, out *[]clievent.EventEntry) error {
	// ctx.Err() takes a mutex; checking every 32 records keeps decode throughput up.
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
			// Other decode errors mean a corrupt file: log, stop, keep what we have.
			slog.Warn("naozhilog: decode error; truncating read",
				"path", path, "err", err)
			break
		}

		if rec.Type == schema.TypeHeader {
			continue
		}
		if len(rec.Entry) == 0 {
			continue
		}

		var entry clievent.EventEntry
		if err := json.Unmarshal(rec.Entry, &entry); err != nil {
			slog.Warn("naozhilog: entry JSON decode failed; skipping",
				"path", path, "seq", rec.Seq, "err", err)
			continue
		}
		// stampUUID runs in cli.EventLog.Append before a record reaches disk, so a
		// missing UUID flags a producer bug or a hand-edited file. Still emit it
		// (dropping would lose history) but warn: merged dedup cannot anchor it.
		if entry.UUID == "" {
			slog.Warn("naozhilog: entry missing UUID post-decode; dedup may regress",
				"path", path, "seq", rec.Seq, "time", entry.Time)
		}
		*out = append(*out, entry)
	}
	return nil
}
