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

// DefaultKeepRecords is how many newest records a rotate keeps (excluding
// the header). Kept above 2× the LoadLatest page size so a dashboard "load
// earlier" right after rotate does not fall through to Claude JSONL.
const DefaultKeepRecords = 1000

// rotateAfterCloseHook is a test-only seam invoked right after w.close(); a
// non-nil error drives the post-close failure path (#1815). Always nil in
// production.
var rotateAfterCloseHook func() error

// rotate performs the O(1) tail-cut: pick the oldest idx ByteOff that keeps
// at least DefaultKeepRecords entries, splice header + tail into tmp.log
// and rebased idx entries into tmp.idx, fsync both, rename over the live
// pair (POSIX atomic), SyncDir, then reopen the new pair as w's fds.
//
// Crash safety: tmp files orphaned before the renames are removed by
// SweepOrphans at startup; a crash between the two renames leaves a
// log/idx mismatch that Recover repairs (ext4 may reorder the renames —
// worst case is one recovery pass, not lost data). On failure the old
// files stay intact and the writer keeps using them.
func (p *Persister) rotate(key, stem string, w *perKeyWriter) error {
	idxEntries, err := ReadAllIdx(w.idxPath)
	if err != nil {
		return fmt.Errorf("read idx for rotate: %w", err)
	}
	if len(idxEntries) == 0 {
		return nil
	}

	cutIdx := chooseCutIndex(idxEntries, DefaultKeepRecords, p.opts.IdxStride)
	if cutIdx <= 0 {
		return nil
	}

	epoch := p.opts.Clock().UnixNano()
	tmpLog := tmpLogPath(p.opts.Dir, stem, epoch)
	tmpIdx := tmpIdxPath(p.opts.Dir, stem, epoch)

	cleanup := func() {
		_ = os.Remove(tmpLog)
		_ = os.Remove(tmpIdx)
	}

	// ----- splice log bytes -----------------------------------
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

	// Close before rename: rename-over-open is fine on POSIX, but writes
	// would keep going to the OLD inode via the old fd.
	_ = w.close()

	// w.close() nilled w.logBuf/logFile/idxWriter. handleBatch only logs a
	// rotate error and keeps the cached writer, so any failure before the
	// reassignment below must evict w or the next batch nil-derefs w.logBuf
	// and panics the run goroutine (#1815). Safe: rotate runs on the single
	// goroutine that owns p.writers.
	rotateOK := false
	defer func() {
		if !rotateOK {
			delete(p.writers, key)
		}
	}()

	if rotateAfterCloseHook != nil {
		if err := rotateAfterCloseHook(); err != nil {
			cleanup()
			return fmt.Errorf("post-close hook: %w", err)
		}
	}

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
	// close() nilled w.logBuf; rebuild it from the shared pool (#995) or the
	// next handleBatch nil-derefs it.
	w.logBuf = acquireLogBuf(logFile)
	w.idxWriter = idxWriter
	w.bytes = newLogSize
	// Seq numbers are never recycled: continue from the new file's last idx seq.
	if n := len(newIdx); n > 0 {
		w.nextSeq = newIdx[n-1].Seq + 1
	}
	w.pendingIdx = w.pendingIdx[:0]
	w.dirty = false
	w.entriesSinceIdxWrite = 0
	rotateOK = true
	slog.Info("event log persist: rotated",
		"key", key,
		"kept_entries", len(newIdx)-1, // minus header
		"new_log_size", newLogSize,
	)
	return nil
}

// chooseCutIndex returns the INDEX in idxEntries (not a seq) where the
// rewritten body starts; 0 means nothing to cut. The header (index 0) is
// never the cut point. With stride S each idx slot covers ~S records, so
// keeping `keep` records means backing off ceil(keep/S) slots from the
// end; because idx is sparse, rotate may keep up to S-1 more than `keep`
// but never fewer.
func chooseCutIndex(idxEntries []schema.IdxEntry, keep int, stride int) int {
	if len(idxEntries) <= 1 {
		return 0
	}
	if stride < 1 {
		stride = 1
	}
	slotsNeeded := (keep + stride - 1) / stride
	if slotsNeeded >= len(idxEntries)-1 {
		return 0
	}
	cutIdx := len(idxEntries) - slotsNeeded
	if cutIdx < 1 {
		return 0
	}
	return cutIdx
}

// spliceLog copies <header bytes> + <tail from cutOff..end> from src to
// dst, returning the new size and idx entries rebased to the new file.
// The tail is streamed via bufio; the old file is never slurped whole.
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
	// Explicit close on success so a flush error surfaces before the caller
	// fsyncs and renames; dstClosed guards the deferred fallback close.
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
	// headerEntry.Len is an int32 decoded from disk with no bound; a
	// bit-flipped negative value would panic make(). Reject so rotate fails
	// safely with the old files intact (#1817).
	if headerEntry.Len < 0 || int64(headerEntry.Len) > schema.MaxRecordBytes {
		return 0, nil, fmt.Errorf("bad header idx entry len: %d", headerEntry.Len)
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
	if _, err := src.Seek(cutEntry.ByteOff, io.SeekStart); err != nil {
		return 0, nil, fmt.Errorf("seek to cut: %w", err)
	}
	br := bufio.NewReaderSize(src, 64*1024)

	newIdx := make([]schema.IdxEntry, 0, len(idxEntries)-cutIdx+1)
	newIdx = append(newIdx, schema.IdxEntry{
		Seq: 0, ByteOff: 0, Len: headerEntry.Len, TimeMS: headerEntry.TimeMS,
	})

	// Idx entries past cutIdx are sorted by ByteOff, so frames are matched to
	// idx entries by tracking srcOff instead of decoding each body for
	// rec.Seq (#603). Records without an idx entry are skipped (sparse idx);
	// seq numbers are preserved, never recycled.
	nextExpectedIdxPos := cutIdx
	srcOff := cutEntry.ByteOff
	for {
		body, frameLen, err := ReadFramedBody(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, ErrPartialTail) {
				// The source is fully fsynced, so a partial tail is corruption;
				// bail rather than commit it.
				return 0, nil, fmt.Errorf("unexpected partial tail in source log at %d", dstOff)
			}
			return 0, nil, fmt.Errorf("read src frame: %w", err)
		}
		if _, err := WriteRecordRaw(dst, body); err != nil {
			ReleaseFramedBody(body)
			return 0, nil, fmt.Errorf("write tmp frame: %w", err)
		}
		// WriteRecordRaw has consumed body; return it to the pool.
		ReleaseFramedBody(body)

		// Defensive: skip idx entries pointing between record boundaries
		// (malformed or sparser-than-expected idx).
		for nextExpectedIdxPos < len(idxEntries) && idxEntries[nextExpectedIdxPos].ByteOff < srcOff {
			nextExpectedIdxPos++
		}
		// Frame matches the next idx entry: push it rebased. Seq/TimeMS from
		// the idx are authoritative; Len is the live frame length.
		if nextExpectedIdxPos < len(idxEntries) && idxEntries[nextExpectedIdxPos].ByteOff == srcOff {
			newIdx = append(newIdx, schema.IdxEntry{
				Seq:     idxEntries[nextExpectedIdxPos].Seq,
				ByteOff: dstOff,
				Len:     int32(frameLen),
				TimeMS:  idxEntries[nextExpectedIdxPos].TimeMS,
			})
			nextExpectedIdxPos++
		}
		srcOff += int64(frameLen)
		dstOff += int64(frameLen)
	}

	// Explicit Close so a flush error surfaces before the caller fsyncs.
	if cerr := dst.Close(); cerr != nil {
		return 0, nil, fmt.Errorf("close tmp log: %w", cerr)
	}
	dstClosed = true

	return dstOff, newIdx, nil
}

// writeIdxFile creates a fresh idx file populated with entries in one pass
// (rotate); IdxWriter is the append path.
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

// fsyncPath opens, syncs, and closes a file (tmp files before rename).
func fsyncPath(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
