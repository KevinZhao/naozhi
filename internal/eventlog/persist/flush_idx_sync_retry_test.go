package persist

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// TestFlush_RetainsPendingIdxOnSyncError pins R20260605B-CORR-12 (#1816):
// flush() must run the idx durability barrier (idxWriter.Sync) BEFORE it
// discards the in-memory retry buffer (pendingIdx) and advances the stride
// cursor. If a transient idx fsync error occurs, pendingIdx must survive
// (and dirty stay true) so the next flush re-appends and re-fsyncs the
// same entries. The old code cleared pendingIdx first, leaving the idx
// bytes stranded in page cache and driving recovery to truncate
// already-fsynced log records on a crash.
func TestFlush_RetainsPendingIdxOnSyncError(t *testing.T) {
	p, dir := newTestPersister(t, func(o *Options) {
		o.IdxStride = 4
	})
	const key = "fk"
	sink := p.SinkFor(key)
	// Write a handful of records, then flush to land the header durably
	// and clear initial pending state.
	for i := 0; i < 3; i++ {
		sink([]Entry{{JSON: []byte(`{"type":"user","detail":"x"}`), TimeMS: int64(1700000000000 + i)}}, false)
	}
	fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := p.Flush(fctx); err != nil {
		fcancel()
		t.Fatalf("flush: %v", err)
	}
	fcancel()

	// Stop run so the test goroutine exclusively owns the writer.
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := p.Stop(sctx); err != nil {
		scancel()
		t.Fatalf("stop: %v", err)
	}
	scancel()

	logPath := filepath.Join(dir, KeyHash(key)+logExt)
	idxPath := filepath.Join(dir, KeyHash(key)+idxExt)
	rec, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	idxW, err := NewIdxWriter(idxPath, 0o600)
	if err != nil {
		t.Fatalf("open idx: %v", err)
	}
	w := &perKeyWriter{
		key:        key,
		stem:       KeyHash(key),
		logFile:    logFile,
		logBuf:     acquireLogBuf(logFile),
		idxWriter:  idxW,
		logPath:    logPath,
		idxPath:    idxPath,
		nextSeq:    rec.NextSeq,
		bytes:      rec.LogSize,
		pendingIdx: make([]schema.IdxEntry, 0, 16),
	}
	t.Cleanup(func() { _ = w.close() })

	// Stage one dirty record with a pending idx entry, mirroring what
	// handleBatch does on the live path.
	body := []byte("3\n{}\n")
	n, err := WriteRecordRaw(w.logBuf, body)
	if err != nil {
		t.Fatalf("write record: %v", err)
	}
	w.pendingIdx = append(w.pendingIdx, schema.IdxEntry{
		Seq: w.nextSeq, ByteOff: w.bytes, Len: int32(n), TimeMS: 42,
	})
	w.bytes += n
	w.nextSeq++
	// Mirrors handleBatch: entriesSinceIdxWrite is NOT advanced per entry.
	// It stays at the start-of-batch phase (0 here) until flush() advances
	// it modulo stride after a durable idx sync.
	w.dirty = true

	idxSizeBefore := idxFileSize(t, idxPath)

	// Inject a transient idx fsync fault.
	injected := errors.New("injected idx fsync EIO")
	idxW.syncFailHook = func() error { return injected }

	err = w.flush(p)
	if err == nil {
		t.Fatalf("flush: expected idx sync error, got nil")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("flush: err=%v, want wrapped %v", err, injected)
	}

	// THE FIX: pendingIdx retained, dirty still true — the retry source
	// must not have been discarded ahead of the durability barrier.
	if len(w.pendingIdx) != 1 {
		t.Fatalf("pendingIdx len=%d after sync failure, want 1 (retry buffer must survive)",
			len(w.pendingIdx))
	}
	if !w.dirty {
		t.Fatalf("w.dirty=false after sync failure, want true (flush must retry)")
	}
	if w.entriesSinceIdxWrite != 0 {
		t.Fatalf("entriesSinceIdxWrite=%d after sync failure, want 0 (cursor must not advance)",
			w.entriesSinceIdxWrite)
	}

	// Clear the fault and retry: the same idx entry must now be appended
	// and durably synced.
	idxW.syncFailHook = nil
	if err := w.flush(p); err != nil {
		t.Fatalf("retry flush: unexpected error: %v", err)
	}
	if len(w.pendingIdx) != 0 {
		t.Fatalf("pendingIdx len=%d after successful retry, want 0", len(w.pendingIdx))
	}
	if w.dirty {
		t.Fatalf("w.dirty=true after successful retry, want false")
	}
	if got := idxFileSize(t, idxPath); got <= idxSizeBefore {
		t.Fatalf("idx file did not grow after retry flush (before=%d after=%d); "+
			"the pending entry was lost", idxSizeBefore, got)
	}
}

func idxFileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat idx: %v", err)
	}
	return fi.Size()
}
