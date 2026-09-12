package persist

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"sync"
)

// pools.go — the buffer/arena pools the write path reuses. Extracted from
// persister.go (J9 of #2548): they are shared by handleBatch and perKeyWriter and
// belong to neither, so keeping them at the top of the run loop's file put the
// least session-specific code first.

var recordBufPool = sync.Pool{
	New: func() any {
		// 4 KiB covers typical EventEntry JSON.
		buf := bytes.NewBuffer(make([]byte, 0, 4*1024))
		return buf
	},
}

// recordBufMaxCap caps pooled buffer size so one oversize record does not
// pin a large allocation.
const recordBufMaxCap = 64 * 1024

// putRecordBuf returns buf to the pool unless it grew past recordBufMaxCap.
func putRecordBuf(buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	if buf.Cap() > recordBufMaxCap {
		return
	}
	buf.Reset()
	recordBufPool.Put(buf)
}

// arenaSpan is one entry's byte range inside batchArena.buf; used only by
// accept()'s resolve pass.
type arenaSpan struct{ start, end int }

// batchArena bundles the pooled JSON byte buffer with the two scratch
// slices accept() needs per batch: owned (Entry headers, escape into
// batchJob.Entries) and spans (transient byte ranges). All three share
// the arena's lifetime; handleBatch returns it after the write (#1630).
type batchArena struct {
	buf   *bytes.Buffer
	owned []Entry
	spans []arenaSpan
}

// entryArenaPool backs the batch-level copy accept() makes of each
// borrowed Entry.JSON; one arena holds a whole batch and handleBatch
// returns it once written (#1524).
var entryArenaPool = sync.Pool{
	New: func() any {
		return &batchArena{
			buf:   bytes.NewBuffer(make([]byte, 0, 4*1024)),
			owned: make([]Entry, 0, 32),
			spans: make([]arenaSpan, 0, 32),
		}
	},
}

// entryArenaMaxCap caps pooled arena buffer size.
const entryArenaMaxCap = 256 * 1024

// entryArenaSliceMaxCap caps the owned/spans scratch slices so a giant
// batch (e.g. a 500-entry InjectHistory replay) is not pinned in the pool.
const entryArenaSliceMaxCap = 1024

func putEntryArena(a *batchArena) {
	if a == nil {
		return
	}
	if a.buf == nil || a.buf.Cap() > entryArenaMaxCap {
		return
	}
	a.buf.Reset()
	// Clear so persisted payloads are not pinned between batches; oversized
	// scratch goes to GC rather than the pool.
	if cap(a.owned) > entryArenaSliceMaxCap {
		a.owned = nil
	} else {
		clear(a.owned)
		a.owned = a.owned[:0]
	}
	if cap(a.spans) > entryArenaSliceMaxCap {
		a.spans = nil
	} else {
		a.spans = a.spans[:0]
	}
	entryArenaPool.Put(a)
}

// logWriteBufSize is the bufio.Writer capacity per perKeyWriter.logFile.
// 64 KiB matches ReadFramedBody's reader buffer and absorbs a typical
// framed record (1-20 KiB) without spilling to a syscall mid-frame.
const logWriteBufSize = 64 * 1024

// logBufPool reuses *bufio.Writer instances of capacity logWriteBufSize
// across perKeyWriter create/close cycles (#995). Safe because close()
// flushes before releasing and releaseLogBuf rebinds to io.Discard, so a
// pooled instance never references a closed *os.File; the next Reset(file)
// rebinds it and clears the internal err. bufio.Writer never grows.
var logBufPool = sync.Pool{
	New: func() any {
		// Bound to io.Discard; callers Reset(file) before use.
		return bufio.NewWriterSize(io.Discard, logWriteBufSize)
	},
}

// acquireLogBuf returns a pooled *bufio.Writer rebound to file.
func acquireLogBuf(file *os.File) *bufio.Writer {
	bw := logBufPool.Get().(*bufio.Writer)
	bw.Reset(file)
	return bw
}

// releaseLogBuf returns a bufio.Writer to the pool. Callers MUST have
// flushed already — the slot is rebound to io.Discard so a retained
// reference cannot double-write to the original fd.
func releaseLogBuf(bw *bufio.Writer) {
	if bw == nil {
		return
	}
	bw.Reset(io.Discard)
	logBufPool.Put(bw)
}

// Observer receives real-time counter increments from the Persister;
// implementations typically forward to expvar / Prometheus. Methods are
// called from the writer goroutine or the PersistSink closure and MUST be
// non-blocking and thread-safe.
//
// The only production implementation is eventLogMetricsObserver in
// internal/session/eventlog_metrics.go, wired via Options.Observer. A new
// persister site must pass the same instance or metrics silently fall
// through to noopObserver; this cannot be enforced at compile time (#1171).
