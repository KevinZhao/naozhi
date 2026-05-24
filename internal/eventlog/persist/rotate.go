package persist

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
	"github.com/naozhi/naozhi/internal/osutil"
)

// DefaultKeepRecords is how many of the newest records a rotate keeps
// in the rewritten file (not counting the header). Set deliberately
// above any caller's "visible history" — rotate_test assumes 2× the
// LoadLatest page size so a dashboard "load earlier" right after
// rotate still sees previous page content without falling through to
// Claude JSONL.
const DefaultKeepRecords = 1000

// rotate executes the O(1) tail-cut rotate documented in RFC §5.2:
//
//  1. Decide the cut: from idx entries, pick the oldest ByteOff we
//     want to keep so that at least DefaultKeepRecords entries survive
//     (header + the newest N).
//  2. Splice:
//     tmp.log = header + <bytes from cutOff..end of old log>
//     tmp.idx = <idx entries for the surviving records, rebased>
//  3. fsync tmp.log, fsync tmp.idx.
//  4. rename tmp.log → <stem>.log, rename tmp.idx → <stem>.idx
//     (POSIX atomic).
//  5. SyncDir so the directory entry flip is durable.
//  6. Reopen the new pair as this writer's backing fds.
//
// Crash safety:
//   - Any crash before step 4 leaves the tmp files orphaned; the
//     startup SweepOrphans path deletes them on next boot.
//   - A crash between the two renames would leave log renamed but
//     idx not — recovery would then see an "idx ahead of log" or
//     "log ahead of idx" mismatch and fix it. ext4 can reorder the
//     two renames; we accept the risk because the worst case is one
//     recovery pass, not lost data.
//
// Returns nil on success; on failure the old files are still intact
// and the writer keeps using them. A subsequent rotate attempt runs
// on the next batch to trip the threshold again.
func (p *Persister) rotate(key, stem string, w *perKeyWriter) error {
	idxEntries, err := ReadAllIdx(w.idxPath)
	if err != nil {
		return fmt.Errorf("read idx for rotate: %w", err)
	}
	if len(idxEntries) == 0 {
		// Empty idx means nothing to rotate (nothing persisted yet).
		// Shouldn't happen given the threshold check, but bail safely.
		return nil
	}

	// Find the cut point. idxEntries[0] is always the header (seq=0
	// by construction); we need at least DefaultKeepRecords entries
	// past the header.
	cutIdx := chooseCutIndex(idxEntries, DefaultKeepRecords, p.opts.IdxStride)
	if cutIdx <= 0 {
		// Nothing to trim (< keep records). Extend threshold avoids
		// tight-loop rotate.
		return nil
	}

	epoch := p.opts.Clock().UnixNano()
	tmpLog := tmpLogPath(p.opts.Dir, stem, epoch)
	tmpIdx := tmpIdxPath(p.opts.Dir, stem, epoch)

	// Always clean tmp files on any failure path.
	cleanup := func() {
		_ = os.Remove(tmpLog)
		_ = os.Remove(tmpIdx)
	}

	// ----- splice log bytes -----------------------------------
	// Keep the header (bytes 0..idxEntries[0].Len) and the tail
	// starting at cutEntry.ByteOff. The records between are dropped.
	newLogSize, newIdx, err := spliceLog(w.logPath, tmpLog, idxEntries, cutIdx)
	if err != nil {
		cleanup()
		return fmt.Errorf("splice log: %w", err)
	}

	// ----- write new idx --------------------------------------
	if err := writeIdxFile(tmpIdx, newIdx); err != nil {
		cleanup()
		return fmt.Errorf("write tmp idx: %w", err)
	}

	// ----- fsync both before renaming --------------------------
	if err := fsyncPath(tmpLog); err != nil {
		cleanup()
		return fmt.Errorf("fsync tmp log: %w", err)
	}
	if err := fsyncPath(tmpIdx); err != nil {
		cleanup()
		return fmt.Errorf("fsync tmp idx: %w", err)
	}

	// ----- close current fds before rename ---------------------
	// On POSIX rename-over-open is fine (the old inode is retained
	// until close), but we'd continue writing to the OLD inode via
	// the old fd. Close first so the next write opens the new file.
	_ = w.close()

	// ----- atomic rename ---------------------------------------
	if err := os.Rename(tmpLog, w.logPath); err != nil {
		cleanup()
		return fmt.Errorf("rename tmp log: %w", err)
	}
	if err := os.Rename(tmpIdx, w.idxPath); err != nil {
		// Log is committed; idx failed. Best effort: remove the log
		// (inconsistent state with no idx is worse than both missing).
		_ = os.Remove(w.logPath)
		cleanup()
		return fmt.Errorf("rename tmp idx: %w", err)
	}
	if err := osutil.SyncDir(p.opts.Dir); err != nil {
		slog.Warn("event log persist: SyncDir after rotate failed",
			"dir", p.opts.Dir, "err", err)
	}

	// ----- reopen fds against the freshly renamed files --------
	logFile, err := os.OpenFile(w.logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("reopen log post-rotate: %w", err)
	}
	idxWriter, err := NewIdxWriter(w.idxPath, 0o600)
	if err != nil {
		logFile.Close()
		return fmt.Errorf("reopen idx post-rotate: %w", err)
	}
	w.logFile = logFile
	// Rebuild the bufio wrapper against the freshly-opened fd.
	// close() nilled w.logBuf; if we skipped this the next
	// handleBatch would panic on WriteRecordRaw(w.logBuf, ...)
	// dereferencing a nil *bufio.Writer.
	w.logBuf = bufio.NewWriterSize(logFile, logWriteBufSize)
	w.idxWriter = idxWriter
	w.bytes = newLogSize
	// nextSeq keeps increasing; rotate does NOT recycle seq numbers.
	// The new file's highest idx seq + 1 is the correct next seq.
	if n := len(newIdx); n > 0 {
		w.nextSeq = newIdx[n-1].Seq + 1
	}
	w.pendingIdx = w.pendingIdx[:0]
	w.dirty = false
	w.entriesSinceIdxWrite = 0
	slog.Info("event log persist: rotated",
		"key", key,
		"kept_entries", len(newIdx)-1, // minus header
		"new_log_size", newLogSize,
	)
	return nil
}

// chooseCutIndex picks the idx slot where the rewritten file's body
// starts. It returns the INDEX in idxEntries (not a seq), so the
// caller can read cutEntry.ByteOff directly. The header (index 0) is
// never the cut point — the header is always preserved.
//
// Algorithm:
//   - Count non-header idx entries: n = len(idxEntries) - 1.
//   - If n <= keep, return 0 (nothing to cut yet — caller will skip).
//   - Otherwise pick the slot that keeps ~`keep` entries. With idx
//     stride S each idx slot covers S records on average, so to keep
//     `keep` records we back off `keep/S` idx slots from the end.
//
// Because idx is sparse, we can't cut at an arbitrary record count;
// we cut at an idx slot. That means rotate may keep slightly more
// than `keep` (the S-1 records between the chosen idx slot and the
// next one) but never fewer.
func chooseCutIndex(idxEntries []schema.IdxEntry, keep int, stride int) int {
	if len(idxEntries) <= 1 {
		return 0
	}
	if stride < 1 {
		stride = 1
	}
	// Number of idx slots we need at the tail to cover `keep` records.
	slotsNeeded := (keep + stride - 1) / stride
	if slotsNeeded >= len(idxEntries)-1 {
		return 0
	}
	// Leave slotsNeeded at the end, plus the header (idx 0).
	cutIdx := len(idxEntries) - slotsNeeded
	if cutIdx < 1 {
		return 0
	}
	return cutIdx
}

// spliceLog copies <header bytes> + <tail from cutOff..end> from
// src → dst, computing the new per-entry idx metadata as it goes.
// The returned idx entries have ByteOff rebased to the new file.
//
// Keeps memory use bounded: we never slurp the whole old file. The
// header is small (<= 1 KiB typical) and we stream the tail via a
// bufio.Reader aligned to record boundaries.
func spliceLog(srcPath, dstPath string, idxEntries []schema.IdxEntry, cutIdx int) (int64, []schema.IdxEntry, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return 0, nil, fmt.Errorf("open src log: %w", err)
	}
	defer src.Close()

	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, nil, fmt.Errorf("create tmp log: %w", err)
	}
	// Explicit close so a write-flush failure surfaces before the caller
	// fsyncs and renames the tmp file. dstClosed guards the deferred
	// fallback close on the error paths below.
	dstClosed := false
	defer func() {
		if !dstClosed {
			_ = dst.Close()
		}
	}()

	// ----- copy header record verbatim ------------------------
	headerEntry := idxEntries[0]
	if headerEntry.ByteOff != 0 || headerEntry.Seq != 0 {
		return 0, nil, fmt.Errorf("bad header idx entry: %+v", headerEntry)
	}
	hdr := make([]byte, headerEntry.Len)
	if _, err := src.ReadAt(hdr, 0); err != nil {
		return 0, nil, fmt.Errorf("read header bytes: %w", err)
	}
	if _, err := dst.Write(hdr); err != nil {
		return 0, nil, fmt.Errorf("write header: %w", err)
	}
	dstOff := int64(headerEntry.Len)

	// ----- copy tail records, rebasing idx --------------------
	cutEntry := idxEntries[cutIdx]
	// Seek to the cut point. The tail consists of one or more
	// framed records; we stream them via bufio and rewrite idx
	// entries as we go.
	if _, err := src.Seek(cutEntry.ByteOff, io.SeekStart); err != nil {
		return 0, nil, fmt.Errorf("seek to cut: %w", err)
	}
	br := bufio.NewReaderSize(src, 64*1024)

	newIdx := make([]schema.IdxEntry, 0, len(idxEntries)-cutIdx+1)
	newIdx = append(newIdx, schema.IdxEntry{
		Seq: 0, ByteOff: 0, Len: headerEntry.Len, TimeMS: headerEntry.TimeMS,
	})

	// We preserve idx seq numbers from the old file (no seq recycle).
	// However, we re-record idx entries only for the records we
	// actually see — recovery picks them up from there.
	nextExpectedIdxPos := cutIdx
	for {
		body, frameLen, err := ReadFramedBody(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, ErrPartialTail) {
				// Should not happen at this point — spliceLog is
				// running under a fully-fsynced source file. Bail
				// rather than commit a corrupted tail.
				return 0, nil, fmt.Errorf("unexpected partial tail in source log at %d", dstOff)
			}
			return 0, nil, fmt.Errorf("read src frame: %w", err)
		}
		if _, err := WriteRecordRaw(dst, body); err != nil {
			ReleaseFramedBody(body)
			return 0, nil, fmt.Errorf("write tmp frame: %w", err)
		}
		// Release before continuing the loop. WriteRecordRaw has
		// already copied body bytes into dst's bufio buffer (see
		// frameWrite contract), so reuse via pool is safe.
		ReleaseFramedBody(body)

		// Advance idx if the record we just spliced lines up with the
		// next expected idx entry. Sparse idx: not every record has
		// one; only push a new IdxEntry when there was one in the old
		// idx at this exact record.
		rec, err := schema.UnmarshalRecord(body)
		if err != nil {
			return 0, nil, fmt.Errorf("decode spliced record: %w", err)
		}
		for nextExpectedIdxPos < len(idxEntries) && idxEntries[nextExpectedIdxPos].Seq < rec.Seq {
			nextExpectedIdxPos++
		}
		if nextExpectedIdxPos < len(idxEntries) && idxEntries[nextExpectedIdxPos].Seq == rec.Seq {
			newIdx = append(newIdx, schema.IdxEntry{
				Seq:     rec.Seq,
				ByteOff: dstOff,
				Len:     int32(frameLen),
				TimeMS:  idxEntries[nextExpectedIdxPos].TimeMS,
			})
			nextExpectedIdxPos++
		}
		dstOff += int64(frameLen)
	}

	// Explicit Close before the caller fsyncs the path: a deferred close
	// would silently drop a flush error from the buffered file's kernel
	// page-cache, leaving the caller to fsync potentially-partial bytes.
	if cerr := dst.Close(); cerr != nil {
		return 0, nil, fmt.Errorf("close tmp log: %w", cerr)
	}
	dstClosed = true

	return dstOff, newIdx, nil
}

// writeIdxFile creates a fresh idx file populated with `entries`.
// Unlike IdxWriter (which appends), this is used by rotate to write
// a complete file in one pass.
func writeIdxFile(path string, entries []schema.IdxEntry) error {
	if len(entries) == 0 {
		return errors.New("persist: refusing to write empty idx")
	}
	buf := make([]byte, schema.IdxEntrySize*len(entries))
	for i, e := range entries {
		schema.MarshalIdxEntry(buf[i*schema.IdxEntrySize:], e)
	}
	return os.WriteFile(path, buf, 0o600)
}

// fsyncPath opens, syncs, and closes a file. Used by rotate to bring
// tmp files to disk before the atomic rename.
func fsyncPath(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
