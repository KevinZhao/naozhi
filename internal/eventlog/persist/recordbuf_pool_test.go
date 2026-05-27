package persist

import (
	"bytes"
	"testing"
)

// TestPutRecordBuf_ResetsBeforePool pins R245-PERF-12 / R242-PERF-13: the
// putRecordBuf path MUST Reset the buffer before returning it to the pool.
// A future refactor that drops the Reset would let the next handleBatch
// Get a buffer with stale tail bytes; schema.MarshalRecordInto writes from
// position 0 but the underlying slice header would otherwise carry the
// previous record's len, and any caller that iterated buf.Bytes() (none
// today, but cheap to defend in advance) would see corruption.
func TestPutRecordBuf_ResetsBeforePool(t *testing.T) {
	t.Parallel()

	buf := bytes.NewBuffer(make([]byte, 0, 4*1024))
	buf.WriteString("stale tail bytes that must not survive Put")
	if buf.Len() == 0 {
		t.Fatal("test setup: WriteString produced empty buffer")
	}

	putRecordBuf(buf)

	if buf.Len() != 0 {
		t.Errorf("putRecordBuf left buf.Len = %d; expected Reset to zero", buf.Len())
	}
	// Cap must survive (the whole point of pooling) — a regression that
	// allocated a fresh buffer on Put would shrink cap.
	if buf.Cap() < 4*1024 {
		t.Errorf("putRecordBuf shrunk buf.Cap to %d; expected >= 4096", buf.Cap())
	}
}

// TestPutRecordBuf_NilSafe documents that putRecordBuf tolerates nil. The
// hot path always Gets first (so nil never reaches Put in production), but
// nil-safety here lets future test helpers Put unconditionally without
// fear.
func TestPutRecordBuf_NilSafe(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("putRecordBuf(nil) panicked: %v", r)
		}
	}()
	putRecordBuf(nil)
}

// TestPutRecordBuf_OversizeDropped pins the recordBufMaxCap=64KiB drop
// rule. Buffers that grew past the cap on a one-off giant record MUST NOT
// be returned to the pool, otherwise the pool would retain a multi-MB
// backing array indefinitely after a single outlier event. R245-PERF-12.
func TestPutRecordBuf_OversizeDropped(t *testing.T) {
	t.Parallel()

	// Construct a buffer whose cap exceeds recordBufMaxCap. The contents
	// are irrelevant — only the cap gates the drop.
	huge := bytes.NewBuffer(make([]byte, 0, recordBufMaxCap+1))
	huge.WriteByte('x') // any content, just to prove Reset would have run.

	// Snapshot the pool entry count by draining + counting fresh news.
	// sync.Pool gives no length introspection, so we instead Put the huge
	// buffer and then Get + check we don't see it. The pool is unordered
	// and may interleave with other tests; we Get a few times to
	// statistically rule it out.
	putRecordBuf(huge)

	for i := 0; i < 16; i++ {
		got := recordBufPool.Get().(*bytes.Buffer)
		if got == huge {
			t.Fatal("putRecordBuf retained an oversize buffer; recordBufMaxCap drop rule regressed")
		}
		// Don't put it back; we want a fresh pool state for subsequent
		// tests rather than reseeding with the test's drained entries.
	}
}

// TestRecordBufPool_NewSeedCap pins the documented 4 KiB seed capacity. A
// regression that shrank the seed (or moved to a fresh-on-every-Get
// pattern) would lose the alloc-amortisation that R245-PERF-12 / R242-
// PERF-13 introduced — handleBatch would pay an encodeState alloc per
// record again.
func TestRecordBufPool_NewSeedCap(t *testing.T) {
	t.Parallel()
	// New() is exercised when the pool is empty. We cannot reliably
	// empty the package-global pool without racing other tests, so
	// invoke the New func directly: it is a closure on the pool literal,
	// captured by exposing via Get on a fresh local pool would require
	// duplicating the literal. Instead, call the documented seed via
	// type-asserted Get and assert cap >= 4 KiB. A pool entry someone
	// else returned would also satisfy >=4 KiB (production code grows
	// the buffer; nobody shrinks it), so the assertion is monotone-safe.
	buf := recordBufPool.Get().(*bytes.Buffer)
	if buf.Cap() < 4*1024 {
		t.Errorf("recordBufPool New seed cap = %d, want >= 4096 (R245-PERF-12 amortisation)", buf.Cap())
	}
	// Return it so the next test sees a populated pool.
	putRecordBuf(buf)
}
