package persist

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestAccept_CopiesBorrowedBytes pins R20260531A-PERF-3 (#1524): the
// PersistSink contract now lets producers hand over BORROWED Entry.JSON
// and reuse the backing array the moment the sink returns. accept() must
// take ownership by copying into its pooled arena before queueing the
// async batch — otherwise the producer's reuse would corrupt what lands
// on disk.
//
// We simulate the bridge's reuse pattern: a single backing array is
// encoded, passed to the sink, then immediately overwritten with the
// next "entry". If accept retained the borrowed slice instead of copying,
// the persisted record would carry the second payload.
func TestAccept_CopiesBorrowedBytes(t *testing.T) {
	p, dir := newTestPersister(t)
	sink := p.SinkFor("dashboard:direct:alice:general")

	// One reusable backing array, mirroring bridgeEncPool's buffer reuse.
	scratch := make([]byte, 0, 256)
	first := []byte(`{"time":1700000001000,"uuid":"first","type":"user","summary":"FIRST"}`)
	scratch = append(scratch[:0], first...)

	sink([]Entry{{JSON: scratch, TimeMS: 1700000001000}}, false)

	// Immediately clobber the backing array, as a producer reusing a
	// pooled buffer would on the next event.
	second := []byte(`{"time":1700000002000,"uuid":"second","type":"assistant","summary":"CLOBBER!!!"}`)
	scratch = append(scratch[:0], second...)
	_ = scratch

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	recs := readAllRecords(t, LogPath(dir, "dashboard:direct:alice:general"))
	// header + 1 entry
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (header+entry)", len(recs))
	}
	if !bytes.Contains(recs[1].Entry, []byte("FIRST")) {
		t.Errorf("persisted entry did not preserve the original borrowed bytes; accept failed to copy. entry=%q", recs[1].Entry)
	}
	if bytes.Contains(recs[1].Entry, []byte("CLOBBER")) {
		t.Errorf("persisted entry carries the producer's reused-array payload; accept retained a borrowed slice. entry=%q", recs[1].Entry)
	}
}

// TestAccept_MultiEntryBorrowedBytes is the batch variant: several
// entries share one growing backing array (the bridge encodes them into a
// single pooled buffer and hands accept sub-slices). accept must copy each
// sub-slice's bytes so post-return reuse of the buffer cannot corrupt any
// of them.
func TestAccept_MultiEntryBorrowedBytes(t *testing.T) {
	p, dir := newTestPersister(t)
	sink := p.SinkFor("dashboard:direct:bob:general")

	// Encode three entries concatenated into one arena, hand sub-slices.
	var arena bytes.Buffer
	payloads := [][]byte{
		[]byte(`{"time":1,"uuid":"a","type":"user","summary":"AAA"}`),
		[]byte(`{"time":2,"uuid":"b","type":"user","summary":"BBB"}`),
		[]byte(`{"time":3,"uuid":"c","type":"user","summary":"CCC"}`),
	}
	type span struct{ s, e int }
	spans := make([]span, len(payloads))
	for i, pl := range payloads {
		st := arena.Len()
		arena.Write(pl)
		spans[i] = span{st, arena.Len()}
	}
	all := arena.Bytes()
	entries := make([]Entry, len(payloads))
	for i := range payloads {
		entries[i] = Entry{JSON: all[spans[i].s:spans[i].e], TimeMS: int64(i + 1)}
	}

	sink(entries, false)

	// Clobber the whole arena after the sink returns.
	for i := range all {
		all[i] = 'X'
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	recs := readAllRecords(t, LogPath(dir, "dashboard:direct:bob:general"))
	if len(recs) != 4 { // header + 3 entries
		t.Fatalf("got %d records, want 4 (header+3 entries)", len(recs))
	}
	for i, want := range [][]byte{[]byte("AAA"), []byte("BBB"), []byte("CCC")} {
		if !bytes.Contains(recs[i+1].Entry, want) {
			t.Errorf("entry %d lost its original bytes (want %q); accept failed to copy. entry=%q", i, want, recs[i+1].Entry)
		}
	}
}

// TestPutEntryArena_OversizeDropped pins the entryArenaMaxCap drop rule:
// an arena grown past the cap by a one-off giant batch must NOT be pooled.
func TestPutEntryArena_OversizeDropped(t *testing.T) {
	huge := bytes.NewBuffer(make([]byte, 0, entryArenaMaxCap+1))
	huge.WriteByte('x')
	putEntryArena(huge)
	for i := 0; i < 16; i++ {
		got := entryArenaPool.Get().(*bytes.Buffer)
		if got == huge {
			t.Fatal("putEntryArena retained an oversize arena; entryArenaMaxCap drop rule regressed")
		}
	}
}

// TestPutEntryArena_NilSafe documents nil tolerance (owned-bytes callers
// queue batchJob.arena == nil).
func TestPutEntryArena_NilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("putEntryArena(nil) panicked: %v", r)
		}
	}()
	putEntryArena(nil)
}
