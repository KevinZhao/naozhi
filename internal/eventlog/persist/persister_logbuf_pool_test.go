package persist

import (
	"bufio"
	"bytes"
	"io"
	"testing"
)

// TestLogBufPool_AcquireRebindsToFile pins the contract that acquireLogBuf
// returns a writer wired to the caller-supplied io.Writer (in production
// the *os.File log fd) rather than the io.Discard sentinel the pool's New
// func installs. Without the Reset, the first batch routed through a
// freshly-acquired writer would silently land in /dev/null. R249-PERF-21
// (#995).
// No t.Parallel on the logBufPool tests below: they share the package-global
// pool. Concurrent Get/Put + bw.Reset/Write/Flush from sibling tests races on
// bufio.Writer's internal buf/n/wr fields (-race on macOS catches it).
func TestLogBufPool_AcquireRebindsToFile(t *testing.T) {
	// acquireLogBuf takes *os.File in production; we use a bytes.Buffer
	// stand-in by exercising the underlying Reset behaviour the helper
	// relies on. The test mirrors how perKeyWriter.close → release →
	// next writer.acquire chain rebinds the same backing array.
	var sink1, sink2 bytes.Buffer
	bw := logBufPool.Get().(*bufio.Writer)
	bw.Reset(&sink1)
	if _, err := bw.WriteString("first"); err != nil {
		t.Fatalf("write1: %v", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("flush1: %v", err)
	}
	if got := sink1.String(); got != "first" {
		t.Errorf("sink1 = %q, want %q", got, "first")
	}
	// Simulate close → release path.
	bw.Reset(io.Discard)
	logBufPool.Put(bw)

	// Second acquire from pool: must rebind to the new sink, not retain
	// the old one. This is what the production close→acquire cycle does.
	bw2 := logBufPool.Get().(*bufio.Writer)
	bw2.Reset(&sink2)
	if _, err := bw2.WriteString("second"); err != nil {
		t.Fatalf("write2: %v", err)
	}
	if err := bw2.Flush(); err != nil {
		t.Fatalf("flush2: %v", err)
	}
	if got := sink2.String(); got != "second" {
		t.Errorf("sink2 = %q, want %q", got, "second")
	}
	// Crucially: sink1 must NOT have received the second write — Reset
	// on the second acquire severs the binding. Regression here would
	// indicate the pool is handing back writers that still target the
	// previous fd.
	if got := sink1.String(); got != "first" {
		t.Errorf("sink1 leaked second write: %q", got)
	}
	// Restore pool state.
	bw2.Reset(io.Discard)
	logBufPool.Put(bw2)
}

// TestLogBufPool_BufferSize pins the 64 KiB buffer-size contract the
// pool depends on. bufio.Writer's buffer is fixed at construction, so
// any pooled instance MUST have been built via NewWriterSize(_, 64*1024)
// — otherwise releaseLogBuf would silently put a different-sized writer
// in the pool and the next acquire would observe a degraded throughput
// profile. The test reads bw.Available() against an empty buffer, which
// equals the configured capacity.
func TestLogBufPool_BufferSize(t *testing.T) {
	bw := acquireLogBuf(nil) // nil io.Writer is fine: we don't write here
	defer releaseLogBuf(bw)
	if got, want := bw.Available(), logWriteBufSize; got != want {
		t.Errorf("pooled bufio.Writer Available = %d, want %d (logWriteBufSize)", got, want)
	}
}

// TestLogBufPool_ReleaseRebindsToDiscard pins the safety check: after
// releaseLogBuf, the writer is bound to io.Discard. Without this rebind
// a leaked retained reference (e.g. if a future caller forgot to nil out
// w.logBuf after close) could continue writing through the pooled
// instance into the previous fd, which by then may have been closed and
// reassigned to an unrelated file by the runtime.
func TestLogBufPool_ReleaseRebindsToDiscard(t *testing.T) {
	var sink bytes.Buffer
	bw := logBufPool.Get().(*bufio.Writer)
	bw.Reset(&sink)
	if _, err := bw.WriteString("payload"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	releaseLogBuf(bw)
	// After release, writes routed through bw must not reach sink.
	if _, err := bw.WriteString("after-release"); err != nil {
		t.Fatalf("write after release: %v", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("flush after release: %v", err)
	}
	if got := sink.String(); got != "payload" {
		t.Errorf("sink received bytes after release: %q", got)
	}
}
