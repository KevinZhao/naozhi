package persist

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// DefaultIdxStride is the number of records between idx entries. 32 keeps
// 1000 records at ~32 entries × 28 B while bounding a "scan forward from
// nearest idx entry" to 31 record decodes. Per-Persister configurable via
// Options.
const DefaultIdxStride = 32

// IdxWriter is an unbuffered append-only writer over os.File; each entry is
// exactly IdxEntrySize bytes. Callers batch and Sync at the Persister layer.
type IdxWriter struct {
	f *os.File

	// batchBuf is scratch reused across AppendBatch calls so the flush hot
	// path does not allocate per flush. Owned by the Persister's single
	// writer goroutine; capacity grows toward the largest batch seen.
	batchBuf []byte

	// syncFailHook is a test-only seam: when it returns an error, Sync
	// returns it WITHOUT touching the fd (#1816). Always nil in production.
	syncFailHook func() error
}

// NewIdxWriter opens idx at path (already resolved by the caller) with
// O_APPEND so a debug tool that re-opens the idx cannot clobber in-flight
// writes.
func NewIdxWriter(path string, perm os.FileMode) (*IdxWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, perm)
	if err != nil {
		return nil, fmt.Errorf("open idx %s: %w", path, err)
	}
	return &IdxWriter{f: f}, nil
}

// Append writes one IdxEntry. Failure leaves the file possibly
// containing a partial write; Recover is responsible for aligning
// to IdxEntrySize boundaries next startup.
func (w *IdxWriter) Append(e schema.IdxEntry) error {
	var buf [schema.IdxEntrySize]byte
	schema.MarshalIdxEntry(buf[:], e)
	if _, err := w.f.Write(buf[:]); err != nil {
		return fmt.Errorf("write idx: %w", err)
	}
	return nil
}

// AppendBatch writes many entries in one syscall, reusing w.batchBuf
// (single writer goroutine, no synchronisation).
//
// Ownership: `entries` is consumed synchronously and never retained, so
// callers may alias it with a buffer they reset right after the call
// (perKeyWriter.flush's stride<=1 fast path does: kept == pending). Any
// future deferred/async consumption MUST copy `entries` first.
func (w *IdxWriter) AppendBatch(entries []schema.IdxEntry) error {
	if len(entries) == 0 {
		return nil
	}
	need := schema.IdxEntrySize * len(entries)
	if cap(w.batchBuf) < need {
		w.batchBuf = make([]byte, need)
	} else {
		w.batchBuf = w.batchBuf[:need]
	}
	for i, e := range entries {
		schema.MarshalIdxEntry(w.batchBuf[i*schema.IdxEntrySize:], e)
	}
	if _, err := w.f.Write(w.batchBuf); err != nil {
		return fmt.Errorf("write idx batch (%d entries): %w", len(entries), err)
	}
	return nil
}

// Sync forces the idx bytes from page cache to disk. The Persister's
// flush goroutine calls this AFTER log.Sync to preserve the strict
// log-then-idx ordering (see recovery.go for why).
func (w *IdxWriter) Sync() error {
	if w.syncFailHook != nil {
		if err := w.syncFailHook(); err != nil {
			return err
		}
	}
	return w.f.Sync()
}

// Truncate cuts the idx file to size bytes (startup recovery when the tail
// idx entry points past log end).
func (w *IdxWriter) Truncate(size int64) error {
	if err := w.f.Truncate(size); err != nil {
		return fmt.Errorf("truncate idx: %w", err)
	}
	// Seek to EOF so further Append starts at the truncated end.
	if _, err := w.f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek idx to end: %w", err)
	}
	return nil
}

// Size returns the current size of the idx file.
func (w *IdxWriter) Size() (int64, error) {
	fi, err := w.f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat idx: %w", err)
	}
	return fi.Size(), nil
}

// Close releases the file descriptor; it does not imply fsync, Sync first
// for durability.
func (w *IdxWriter) Close() error {
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// ReadAllIdx reads every IdxEntry from path in order; a missing file yields
// an empty (non-nil) slice. Trailing partial entries are dropped by
// rounding down to an IdxEntrySize boundary. Whole-file read is fine: a
// typical idx is < 60 KiB and rotate needs all of it anyway.
func ReadAllIdx(path string) ([]schema.IdxEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []schema.IdxEntry{}, nil
		}
		return nil, fmt.Errorf("read idx %s: %w", path, err)
	}
	return decodeIdxBytes(data), nil
}

// decodeIdxBytes is the pure decode path behind ReadAllIdx; unaligned tail
// bytes are discarded silently.
func decodeIdxBytes(data []byte) []schema.IdxEntry {
	aligned := schema.AlignIdxSize(int64(len(data)))
	count := int(aligned / schema.IdxEntrySize)
	if count == 0 {
		return []schema.IdxEntry{}
	}
	out := make([]schema.IdxEntry, count)
	for i := 0; i < count; i++ {
		e, err := schema.UnmarshalIdxEntry(
			data[i*schema.IdxEntrySize : (i+1)*schema.IdxEntrySize],
		)
		if err != nil {
			// Unreachable given the alignment (UnmarshalIdxEntry only returns
			// ErrShortIdxBuf), but recovery-critical: a future schema error
			// class must surface a log line rather than truncate silently.
			slog.Warn("event log persist: idx decode unexpected error; truncating",
				"i", i,
				"count", count,
				"err", err)
			return out[:i]
		}
		out[i] = e
	}
	return out
}

// LastIdxEntry returns the final idx entry in path, or (zero, false) when
// the file is empty / missing, without reading the whole file.
func LastIdxEntry(path string) (schema.IdxEntry, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return schema.IdxEntry{}, false, nil
		}
		return schema.IdxEntry{}, false, fmt.Errorf("open idx %s: %w", path, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return schema.IdxEntry{}, false, fmt.Errorf("stat idx %s: %w", path, err)
	}
	aligned := schema.AlignIdxSize(fi.Size())
	if aligned == 0 {
		return schema.IdxEntry{}, false, nil
	}
	e, err := schema.ReadIdxEntryAt(f, aligned-schema.IdxEntrySize)
	if err != nil {
		return schema.IdxEntry{}, false, fmt.Errorf("read last idx entry: %w", err)
	}
	return e, true, nil
}
