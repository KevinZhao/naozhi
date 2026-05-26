package persist

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// DefaultIdxStride is the number of records between successive idx
// entries. 32 is a compromise:
//   - Sparse enough that 1000 records cost ~32 idx entries × 28 bytes
//     = 896 bytes, dwarfed by the 40 MiB worst-case log.
//   - Dense enough that "scan forward from nearest idx entry to find
//     seq=S" costs at most 31 record decodes.
//
// The stride is per-Persister configurable (see Options); this
// constant documents the default.
const DefaultIdxStride = 32

// IdxWriter is a thin append-only writer on top of os.File. Each
// AppendEntry call writes exactly IdxEntrySize bytes. No buffering —
// callers batch at the Persister layer and Sync() on fsync boundaries.
type IdxWriter struct {
	f *os.File

	// batchBuf is a scratch slice reused across AppendBatch calls so the
	// flush hot path (~200ms cadence × N sessions) does not allocate a
	// fresh `make([]byte, IdxEntrySize*len(entries))` per flush. The
	// slice is owned exclusively by the Persister's single writer
	// goroutine — every call to AppendBatch resets it via [:0] and
	// re-appends. Capacity grows monotonically toward the largest
	// observed batch and persists for the writer's lifetime, which is
	// fine: each entry is 28 B and a typical idx batch is ≤32 entries
	// (DefaultIdxStride window) ≈ 896 B. R237-PERF-11.
	batchBuf []byte
}

// NewIdxWriter opens idx at the given path in append mode. Callers
// (Recover / Persister) supply an already-resolved path; this helper
// does not compose it.
//
// The file is opened O_APPEND to let concurrent crash-recovery tools
// that re-open the idx not clobber in-flight writes. Production has
// only one writer, but defense in depth matters when someone attaches
// a debug utility.
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

// AppendBatch writes many entries in one syscall. Used by rotate's
// reindex path where we write ~1000 entries back-to-back. Saves 999
// syscalls vs Append per-entry.
//
// The marshal scratch (`w.batchBuf`) is reused across calls — a single
// Persister writer goroutine owns this writer, so no synchronisation
// is required. R237-PERF-11.
//
// Slice ownership contract (R243-PERF-10): `entries` is consumed
// synchronously — marshalled into w.batchBuf and written before this
// method returns. AppendBatch does NOT retain a reference past return,
// so callers may safely alias the input with a buffer they reset (e.g.
// pendingIdx[:0]) immediately after the call. Any future change that
// defers consumption (e.g. async write) MUST first defensively copy
// `entries`, otherwise perKeyWriter.flush's stride<=1 fast-path —
// where `kept == pending` and the caller resets pendingIdx to []—
// would corrupt previously-queued entries.
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
	return w.f.Sync()
}

// Truncate cuts the idx file to size bytes. Used by:
//   - Startup recovery when the tail idx entry points past log end.
//   - Rotate when rebuilding the idx into a fresh file via tmp rename
//     (not via Truncate — rotate uses a new file then renames).
func (w *IdxWriter) Truncate(size int64) error {
	if err := w.f.Truncate(size); err != nil {
		return fmt.Errorf("truncate idx: %w", err)
	}
	// Seek back to EOF so further Append starts at the truncated end,
	// not at a stale offset.
	if _, err := w.f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek idx to end: %w", err)
	}
	return nil
}

// Size returns the current size of the idx file. Used by recovery to
// decide whether a partial trailing entry needs aligning.
func (w *IdxWriter) Size() (int64, error) {
	fi, err := w.f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat idx: %w", err)
	}
	return fi.Size(), nil
}

// Close releases the file descriptor. Callers should Sync before
// Close if they want durability guarantees; Close alone does not
// imply fsync.
func (w *IdxWriter) Close() error {
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// ReadAllIdx reads every IdxEntry from the file at path, in order.
// Returns an empty slice (not nil) when the file doesn't exist.
// Tolerates trailing partial entries by rounding size down to the
// nearest IdxEntrySize boundary — startup recovery is expected to
// call Align() afterwards to match that rounding on disk too.
//
// "Read all" is appropriate here because:
//   - Typical idx has <= 2000 entries (500 records / 1-record stride
//     for small files; 500/32 ≈ 16 for normal ones); < 60 KiB.
//   - Rotate needs to walk the whole thing anyway to pick a cut
//     point, so streaming wouldn't save anything.
//   - Startup reads once per session at boot, not a hot path.
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

// decodeIdxBytes is the pure decode path shared by ReadAllIdx and any
// future in-memory tests. Truncated tail bytes (size % 28 != 0) are
// discarded silently; callers that want the exact boundary position
// should prefer schema.AlignIdxSize on the file's Stat size.
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
			// Cannot happen given the alignment — schema.UnmarshalIdxEntry
			// only returns ErrShortIdxBuf, which we pre-checked. Keep
			// the error path explicit so future edits to schema can't
			// silently introduce a new error class. R20260526-CR-020:
			// since this is recovery-critical the path MUST surface a
			// log line on the way out — silent truncation would mask a
			// schema-evolution bug or an on-disk corruption that an
			// operator needs to investigate before recovery completes.
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

// LastIdxEntry returns the final idx entry in path, or (zero, false)
// when the file is empty / doesn't exist. Used by recovery's "is idx
// ahead of log" check without requiring a full read.
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
