package persist

import (
	"bytes"
	"testing"
)

// BenchmarkAcceptArenaBorrow pins R20260602-PERF-7 (#1630): the per-batch
// owned/spans slice headers are borrowed from the pooled batchArena rather
// than make()'d fresh, so the steady-state borrow→resolve→return cycle
// should report 0 allocs/op once the pool has warmed (the arena buffer +
// owned/spans backing arrays are all reused across batches).
//
// The body mirrors sessionSink.accept's arena-borrow + two-pass resolve so
// the benchmark exercises exactly the hot path the issue calls out, without
// dragging in the writer goroutine / fsync that would dominate the timing
// and obscure the allocation signal.
func BenchmarkAcceptArenaBorrow(b *testing.B) {
	payloads := [][]byte{
		[]byte(`{"time":1,"uuid":"a","type":"user","summary":"AAA"}`),
		[]byte(`{"time":2,"uuid":"b","type":"assistant","summary":"BBB"}`),
		[]byte(`{"time":3,"uuid":"c","type":"user","summary":"CCC"}`),
	}
	entries := make([]Entry, len(payloads))
	for i, pl := range payloads {
		entries[i] = Entry{JSON: pl, TimeMS: int64(i + 1)}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		arena := entryArenaPool.Get().(*batchArena)
		n := len(entries)
		owned := arena.owned
		if cap(owned) >= n {
			owned = owned[:n]
		} else {
			owned = make([]Entry, n)
		}
		spans := arena.spans
		if cap(spans) >= n {
			spans = spans[:n]
		} else {
			spans = make([]arenaSpan, n)
		}
		arena.owned = owned
		arena.spans = spans
		for j, e := range entries {
			start := arena.buf.Len()
			arena.buf.Write(e.JSON)
			spans[j] = arenaSpan{start: start, end: arena.buf.Len()}
			owned[j] = Entry{TimeMS: e.TimeMS}
		}
		all := arena.buf.Bytes()
		for j := range owned {
			owned[j].JSON = all[spans[j].start:spans[j].end]
		}
		// Sanity: resolved bytes match (kept out of the hot allocation
		// path measurement but cheap; prevents the compiler eliding work).
		if !bytes.Equal(owned[0].JSON, payloads[0]) {
			b.Fatal("resolve mismatch")
		}
		putEntryArena(arena)
	}
}
