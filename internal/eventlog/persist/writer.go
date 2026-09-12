package persist

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
	"github.com/naozhi/naozhi/internal/osutil"
)

// writer.go — the per-key writer: opening it, flushing it, closing it, and
// removing its files. Extracted from persister.go (J9 of #2548).

func (p *Persister) dropInMemoryLocked(key string) {
	if w, ok := p.writers[key]; ok {
		if err := w.close(); err != nil {
			slog.Warn("event log persist: close on drop failed", "key", key, "err", err)
		}
		delete(p.writers, key)
	}
}

// removeKeyFiles unlinks the log + idx for stem. Runs on a goroutine
// spawned by handleOp(opDrop) so a slow os.Remove does not stall the
// batch loop; the error is forwarded to DropKey's done channel (#1284).
func (p *Persister) removeKeyFiles(stem string) error {
	logPath := filepath.Join(p.opts.Dir, stem+logExt)
	idxPath := filepath.Join(p.opts.Dir, stem+idxExt)
	var firstErr error
	if err := removeFileHook(logPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		firstErr = fmt.Errorf("remove log: %w", err)
	}
	if err := removeFileHook(idxPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		if firstErr == nil {
			firstErr = fmt.Errorf("remove idx: %w", err)
		}
	}
	return firstErr
}

// removeFileHook is the test seam for a slow/instrumented unlink (#1774).
var removeFileHook = os.Remove

func (p *Persister) writerFor(key, stem string) (*perKeyWriter, error) {
	if w, ok := p.writers[key]; ok {
		return w, nil
	}

	// handleBatch gates on p.dropping before calling here and opDropDone
	// deletes the entry before replaying, so the stem is never mid-removal
	// at this point (remove-before-recreate, #1774).

	logPath := filepath.Join(p.opts.Dir, stem+logExt)
	idxPath := filepath.Join(p.opts.Dir, stem+idxExt)

	// Recover brings the (log, idx) pair to a consistent state BEFORE
	// opening for append so the first write lands at a known-clean offset.
	rec, err := Recover(logPath, idxPath)
	if err != nil {
		return nil, fmt.Errorf("recover %s: %w", key, err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", logPath, err)
	}
	idxW, err := NewIdxWriter(idxPath, 0o600)
	if err != nil {
		logFile.Close()
		return nil, fmt.Errorf("open idx %s: %w", idxPath, err)
	}

	// Pre-size pendingIdx to two stride windows to avoid grow churn.
	pendingCap := 16
	if p.opts.IdxStride > 1 {
		pendingCap = p.opts.IdxStride * 2
	}
	now := p.opts.Clock()
	w := &perKeyWriter{
		key:          key,
		stem:         stem,
		logFile:      logFile,
		logBuf:       acquireLogBuf(logFile),
		idxWriter:    idxW,
		logPath:      logPath,
		idxPath:      idxPath,
		nextSeq:      rec.NextSeq,
		bytes:        rec.LogSize,
		pendingIdx:   make([]schema.IdxEntry, 0, pendingCap),
		lastActivity: now,
	}

	// Fresh file: emit the header at seq=0.
	if rec.LogSize == 0 && !rec.HeaderValid {
		hdr := schema.NewHeader(key, now.UnixMilli(), p.opts.Generator)
		body, mErr := schema.MarshalRecord(hdr)
		if mErr != nil {
			logFile.Close()
			idxW.Close()
			return nil, fmt.Errorf("marshal initial header: %w", mErr)
		}
		n, err := WriteRecordRaw(logFile, body)
		if err != nil {
			logFile.Close()
			idxW.Close()
			return nil, fmt.Errorf("write initial header: %w", err)
		}
		w.pendingIdx = append(w.pendingIdx, schema.IdxEntry{
			Seq: 0, ByteOff: 0, Len: int32(n), TimeMS: hdr.Header.CreatedAt,
		})
		w.bytes = n
		w.nextSeq = 1
		w.dirty = true
		w.firstDirtyAt = now

		// fsync the header now so a crash before any other write leaves a
		// valid file rather than 0 bytes.
		if err := w.flush(p); err != nil {
			logFile.Close()
			idxW.Close()
			return nil, fmt.Errorf("flush initial header: %w", err)
		}
		// SyncDir so the new file's dirent is durable.
		if err := osutil.SyncDir(p.opts.Dir); err != nil {
			slog.Warn("event log persist: SyncDir after header failed",
				"dir", p.opts.Dir, "err", err)
		}
	}

	p.writers[key] = w
	return w, nil
}

// perKeyWriter is per-session state owned exclusively by the writer
// goroutine; no mutex needed.
type perKeyWriter struct {
	key       string
	stem      string
	logFile   *os.File
	logBuf    *bufio.Writer // wraps logFile; flushed before Sync()
	idxWriter *IdxWriter
	logPath   string
	idxPath   string

	nextSeq              uint64
	bytes                int64
	pendingIdx           []schema.IdxEntry // buffered until fsync time
	idxScratch           []schema.IdxEntry // selectForIdx scratch, reused across flushes
	entriesSinceIdxWrite int

	dirty        bool
	firstDirtyAt time.Time
	lastActivity time.Time
}

// flush writes pending idx entries with strict log→idx ordering, fsyncs
// both, and clears dirty. No-op when nothing is dirty.
func (w *perKeyWriter) flush(p *Persister) error {
	if !w.dirty {
		return nil
	}
	// Phase 1: drain bufio, then fsync the log fd. Both must complete before
	// any idx write touches disk (recovery.go: idx must never run ahead of
	// log). On failure dirty stays true so the next tick retries; the bufio
	// Flush error surfaces the original stashed Write failure.
	if err := w.logBuf.Flush(); err != nil {
		return fmt.Errorf("flush log buffer: %w", err)
	}
	if err := w.logFile.Sync(); err != nil {
		return fmt.Errorf("sync log: %w", err)
	}
	p.fsyncCnt.Add(1)
	p.opts.Observer.OnFsync()

	// Phase 2: append pending idx entries, then fsync idx.
	idxAppended := false
	if len(w.pendingIdx) > 0 {
		// Sparse idx: keep the first of every stride, the header (seq=0) and
		// the last entry so recovery finds a safe edge near EOF.
		kept := selectForIdx(w.pendingIdx, p.opts.IdxStride, w.entriesSinceIdxWrite, w.idxScratch[:0])
		// Retain `kept` as scratch only when stride > 1: in the stride<=1 path
		// selectForIdx returns `pending` itself, and aliasing it into idxScratch
		// would let the next flush append into both slices over one backing
		// array, corrupting the idx.
		if p.opts.IdxStride > 1 {
			w.idxScratch = kept
		}
		// AppendBatch consumes `kept` synchronously (idx.go's slice-ownership
		// contract), so the aliasing is safe. If it ever retains `entries`,
		// the stride<=1 path must copy here.
		if err := w.idxWriter.AppendBatch(kept); err != nil {
			return fmt.Errorf("append idx batch: %w", err)
		}
		idxAppended = true
	}
	// Skip the idx fsync when nothing was appended (idx is only valid up to
	// a previously-fsynced suffix anyway). The Sync MUST run BEFORE pendingIdx
	// is discarded and the stride cursor advanced: AppendBatch only reached
	// the page cache, and clearing the retry buffer on a transient Sync error
	// stranded idx bytes that recovery later used to truncate durable log (#1816).
	if idxAppended {
		if err := w.idxWriter.Sync(); err != nil {
			return fmt.Errorf("sync idx: %w", err)
		}
		p.fsyncCnt.Add(1)
		p.opts.Observer.OnFsync()

		// Durability confirmed — safe to discard the retry buffer and advance
		// the stride cursor (mod stride keeps successive batches aligned).
		w.entriesSinceIdxWrite = (w.entriesSinceIdxWrite + len(w.pendingIdx)) % p.opts.IdxStride
		// Shrink if a one-off large batch (e.g. a 500-entry InjectHistory
		// replay) bloated cap far past the steady-state IdxStride*2, else the
		// writer pins the peak capacity for its lifetime. idxScratch grows the
		// same way (#1120) and is only assigned when stride > 1.
		if p.opts.IdxStride > 1 && cap(w.pendingIdx) > p.opts.IdxStride*4 {
			w.pendingIdx = make([]schema.IdxEntry, 0, p.opts.IdxStride*2)
		} else {
			w.pendingIdx = w.pendingIdx[:0]
		}
		if p.opts.IdxStride > 1 && cap(w.idxScratch) > p.opts.IdxStride*4 {
			w.idxScratch = make([]schema.IdxEntry, 0, p.opts.IdxStride*2)
		}
	}

	w.dirty = false
	return nil
}

// close flushes then releases fds; the writer is not reusable afterward.
// The explicit logBuf Flush covers callers that close() without a
// preceding flush() (e.g. shutdownAll after a flush error still wants
// fds released); errors here are best-effort, flush() is where they surface.
func (w *perKeyWriter) close() error {
	var firstErr error
	if w.logBuf != nil {
		if err := w.logBuf.Flush(); err != nil && firstErr == nil {
			firstErr = err
		}
		// releaseLogBuf rebinds the pooled writer to io.Discard so the nilled
		// w.logBuf cannot route writes through it later (#995).
		releaseLogBuf(w.logBuf)
		w.logBuf = nil
	}
	if w.logFile != nil {
		if err := w.logFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		w.logFile = nil
	}
	if w.idxWriter != nil {
		if err := w.idxWriter.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		w.idxWriter = nil
	}
	return firstErr
}

// selectForIdx applies the sparse-idx policy: keep the first entry, every
// stride-th entry after it relative to cursor, and always the last entry
// so recovery finds a safe edge within stride-1 records of EOF.
//
// `scratch` is caller-owned (perKeyWriter.idxScratch[:0]) and reused across
// flushes. Aliasing contract: when stride <= 1 or len(pending) == 1 the
// function returns `pending` itself. The sole caller (flush) hands the
// result to idxWriter.AppendBatch, which copies SYNCHRONOUSLY before
// returning, then resets pendingIdx[:0]. Do NOT add an async AppendBatch
// or retain the returned slice without a defensive copy.
func selectForIdx(pending []schema.IdxEntry, stride, cursor int, scratch []schema.IdxEntry) []schema.IdxEntry {
	if stride <= 1 {
		return pending
	}
	if len(pending) == 0 {
		return nil
	}
	// A lone entry is both first and last: always kept, skip the loop.
	if len(pending) == 1 {
		return pending
	}
	estCap := len(pending)/stride + 2
	var kept []schema.IdxEntry
	if cap(scratch) >= estCap {
		kept = scratch[:0]
	} else {
		kept = make([]schema.IdxEntry, 0, estCap)
	}
	for i, e := range pending {
		// Header (seq=0) is always kept.
		if e.Seq == 0 {
			kept = append(kept, e)
			continue
		}
		// Stride-aligned relative to cursor.
		if (cursor+i)%stride == 0 {
			kept = append(kept, e)
			continue
		}
		// Last entry of the batch is always kept.
		if i == len(pending)-1 {
			kept = append(kept, e)
		}
	}
	return kept
}
