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

// buildPopulatedWriter writes a header + n records through the live
// pipeline, flushes, then stops the persister and reopens the (log, idx)
// pair as a standalone *perKeyWriter the test goroutine owns exclusively.
// Returns the persister (whose p.writers the writer is re-inserted into,
// so eviction is observable), the writer, and its key.
func buildPopulatedWriter(t *testing.T, n int) (*Persister, *perKeyWriter, string) {
	t.Helper()
	p, dir := newTestPersister(t, func(o *Options) {
		o.IdxStride = 1             // dense idx so spliceLog has cut candidates
		o.ChannelBuffer = 16 * 1024 // wide enough that 1k+ records never drop
	})
	const key = "rk"
	sink := p.SinkFor(key)
	// Send in chunks with intermediate flushes so the bounded channel
	// drains and no batch is dropped (a dropped record would leave too
	// few idx entries for chooseCutIndex to trip).
	for i := 0; i < n; i++ {
		sink([]Entry{{JSON: []byte(`{"type":"user","detail":"x"}`), TimeMS: int64(1700000000000 + i)}}, false)
		if i%200 == 199 {
			fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := p.Flush(fctx); err != nil {
				fcancel()
				t.Fatalf("interim flush: %v", err)
			}
			fcancel()
		}
	}
	fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := p.Flush(fctx); err != nil {
		fcancel()
		t.Fatalf("flush: %v", err)
	}
	fcancel()

	// Stop run so the test goroutine is the sole owner of p.writers.
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
	p.writers[key] = w
	return p, w, key
}

// TestRotate_EvictsPoisonedWriterOnError pins R20260605B-CORR-11 (#1815):
// rotate() calls w.close() (which nils logBuf/logFile/idxWriter) BEFORE the
// renames and reopens. If any step after close() fails, rotate must NOT
// leave the poisoned writer in p.writers — otherwise the next batch/flush
// dereferences a nil *bufio.Writer and panics the (un-recovered) run
// goroutine, crashing the whole process.
//
// The post-close failure is injected via the rotateAfterCloseHook test
// seam (the issue's real triggers are ENOSPC on rename / EMFILE on reopen).
func TestRotate_EvictsPoisonedWriterOnError(t *testing.T) {
	p, w, key := buildPopulatedWriter(t, DefaultKeepRecords+50)

	if _, ok := p.writers[key]; !ok {
		t.Fatalf("setup: writer not in p.writers")
	}

	injected := errors.New("injected post-close I/O fault")
	rotateAfterCloseHook = func() error { return injected }
	t.Cleanup(func() { rotateAfterCloseHook = nil })

	err := p.rotate(key, w.stem, w)
	if err == nil {
		t.Fatalf("rotate: expected error from post-close fault, got nil")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("rotate: err=%v, want wrapped %v", err, injected)
	}

	// THE FIX: the poisoned writer must be gone from p.writers so the
	// next access rebuilds a clean writer via Recover.
	if _, ok := p.writers[key]; ok {
		t.Fatalf("rotate left poisoned writer in p.writers after error; " +
			"next batch would nil-deref logBuf and crash run goroutine")
	}
	// Confirm the writer really was poisoned (close() ran), proving the
	// eviction — not luck — is what prevents the crash.
	if w.logBuf != nil || w.logFile != nil || w.idxWriter != nil {
		t.Errorf("expected nil buffers after failed post-close rotate, "+
			"got logBuf=%v logFile=%v idxWriter=%v", w.logBuf, w.logFile, w.idxWriter)
	}
}

// TestRotate_SucceedsAndKeepsWriter is the positive control: a clean rotate
// must NOT evict the writer (the deferred guard must clear itself on
// success) and must restore live buffers.
func TestRotate_SucceedsAndKeepsWriter(t *testing.T) {
	p, w, key := buildPopulatedWriter(t, DefaultKeepRecords+50)

	if err := p.rotate(key, w.stem, w); err != nil {
		t.Fatalf("rotate: unexpected error: %v", err)
	}
	if _, ok := p.writers[key]; !ok {
		t.Fatalf("successful rotate wrongly evicted writer from p.writers")
	}
	if w.logBuf == nil {
		t.Fatalf("successful rotate left w.logBuf nil (reopen not applied)")
	}
}

// TestSpliceLog_RejectsNegativeHeaderLen pins R20260605B-CORR-13 (#1817):
// a corrupted idx header entry with a negative int32 Len must be rejected
// with an error, NOT crash via make([]byte, negative) ("makeslice: len out
// of range") on the un-recovered run goroutine.
func TestSpliceLog_RejectsNegativeHeaderLen(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.log")
	if err := os.WriteFile(srcPath, []byte("8\n{\"a\":1}\n"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dstPath := filepath.Join(dir, "dst.log")

	// Header entry with a negative Len (high-bit-set int32, the shape a
	// single bit-flip on disk produces) plus a tail entry so cutIdx=1.
	idxEntries := []schema.IdxEntry{
		{Seq: 0, ByteOff: 0, Len: -1, TimeMS: 1},
		{Seq: 1, ByteOff: 10, Len: 5, TimeMS: 2},
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("spliceLog panicked on negative header Len (bug #1817): %v", r)
		}
	}()
	_, _, err := spliceLog(srcPath, dstPath, idxEntries, 1)
	if err == nil {
		t.Fatalf("spliceLog: expected error on negative header Len, got nil")
	}
}

// TestSpliceLog_RejectsOversizeHeaderLen guards the upper bound: a header
// Len beyond MaxRecordBytes is also rejected rather than attempting a
// multi-gigabyte allocation.
func TestSpliceLog_RejectsOversizeHeaderLen(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.log")
	if err := os.WriteFile(srcPath, []byte("8\n{\"a\":1}\n"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dstPath := filepath.Join(dir, "dst.log")
	idxEntries := []schema.IdxEntry{
		{Seq: 0, ByteOff: 0, Len: int32(schema.MaxRecordBytes) /* in-range max */, TimeMS: 1},
		{Seq: 1, ByteOff: 10, Len: 5, TimeMS: 2},
	}
	// Bump just past the cap.
	idxEntries[0].Len++
	_, _, err := spliceLog(srcPath, dstPath, idxEntries, 1)
	if err == nil {
		t.Fatalf("spliceLog: expected error on oversize header Len, got nil")
	}
}
