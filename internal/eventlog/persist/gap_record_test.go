package persist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// TestGapRecord_DroppedBatchLeavesDurableMark is #2664's core claim: after a
// channel-full drop, the NEXT batch that gets through carries a gap record in
// front, so a reader of events/<key> can tell "no messages in this span" from
// "a batch was dropped here". Before this the only evidence was a process-
// memory counter (reset on restart) and one Warn line (log retention).
func TestGapRecord_DroppedBatchLeavesDurableMark(t *testing.T) {
	t.Parallel()
	p, dir := newTestPersister(t)
	const key = "feishu:p2p:gap-test"
	s := &sessionSink{p: p, key: key, stem: KeyHash(key)}

	// Simulate the drop bookkeeping the channel-full path performs (driving a
	// real overflow would race the drain loop; the pendingGap contract is what
	// the durable mark depends on and is what this pins).
	s.pendingGap.Add(37)

	s.accept([]Entry{
		{TimeMS: 1700000000000, JSON: []byte(`{"time":1700000000000,"type":"text","summary":"after the gap"}`)},
	}, false)
	if err := p.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	recs := readAllRecords(t, LogPath(dir, key))
	var gapIdx, textIdx = -1, -1
	var gapBody []byte
	for i, r := range recs {
		if len(r.Entry) == 0 {
			continue
		}
		if bytes.Contains(r.Entry, []byte(gapEntryType)) {
			gapIdx, gapBody = i, r.Entry
		}
		if bytes.Contains(r.Entry, []byte("after the gap")) {
			textIdx = i
		}
	}
	if gapIdx < 0 {
		t.Fatal("no gap record on disk after a drop; the reader cannot see the loss")
	}
	if textIdx < 0 || gapIdx > textIdx {
		t.Fatalf("gap record at %d must precede the batch that carried it (text at %d)", gapIdx, textIdx)
	}
	var gap clievent.EventEntry
	if err := json.Unmarshal(gapBody, &gap); err != nil {
		t.Fatalf("gap record does not parse as an EventEntry: %v\n%s", err, gapBody)
	}
	if gap.Type != gapEntryType {
		t.Errorf("Type = %q", gap.Type)
	}
	if !strings.Contains(gap.Detail, "dropped=37") {
		t.Errorf("Detail = %q, want the machine-readable count", gap.Detail)
	}
	if gap.Time != 1700000000000 {
		t.Errorf("Time = %d, want the carrying batch's first timestamp — the gap spans up to that instant", gap.Time)
	}
	// No uuid on purpose: sharing Time with its batch, an empty UUID is what
	// keeps the record first under merged's (Time, UUID) order (gapEntryJSON).
	if gap.UUID != "" || bytes.Contains(gapBody, []byte(`"uuid"`)) {
		t.Errorf("gap record carries a uuid; a hex id would sort it among its own batch:\n%s", gapBody)
	}
	// And the counter is consumed: a second batch must NOT repeat the gap.
	s.accept([]Entry{
		{TimeMS: 1700000001000, JSON: []byte(`{"time":1700000001000,"type":"text","summary":"later"}`)},
	}, false)
	if err := p.Flush(context.Background()); err != nil {
		t.Fatalf("Flush 2: %v", err)
	}
	count := 0
	for _, r := range readAllRecords(t, LogPath(dir, key)) {
		if len(r.Entry) > 0 && bytes.Contains(r.Entry, []byte(gapEntryType)) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("gap records = %d, want exactly 1 — the mark must not repeat once written", count)
	}
}

// TestGapRecord_DropDuringCarryRestoresCount: if the batch carrying the gap is
// itself dropped, the swapped-out count must ride back (plus the new batch) —
// otherwise the tally vanishes and the eventual gap record under-reports.
func TestGapRecord_DropDuringCarryRestoresCount(t *testing.T) {
	t.Parallel()
	// ChannelBuffer 0 is not allowed; use 1 and pre-fill it so accept's
	// non-blocking send loses.
	p, _ := newTestPersister(t, func(o *Options) { o.ChannelBuffer = 1 })
	const key = "feishu:p2p:gap-refill"
	s := &sessionSink{p: p, key: key, stem: KeyHash(key)}

	// Own the channel state outright: Stop() makes the drain goroutine exit
	// (wg.Wait guarantees it is gone), then the closed latch is flipped back so
	// accept still runs. A stuffed channel alone is not enough — the drain can
	// dequeue the blocker between the stuff and the accept, the non-blocking
	// send then wins, and the assertion reads a consumed counter.
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	p.closed.Store(false)
	// Restore the latch before the constructor's cleanup calls Stop again —
	// its CompareAndSwap must lose, or closeCh gets closed twice.
	defer p.closed.Store(true)
	p.in <- batchJob{Key: "other", Stem: KeyHash("other")} // fills the cap-1 channel; nothing drains it now

	s.pendingGap.Add(5)
	s.accept([]Entry{{TimeMS: 1, JSON: []byte(`{"time":1,"type":"text"}`)}}, false)

	if got := s.pendingGap.Load(); got != 6 {
		t.Fatalf("pendingGap after drop-during-carry = %d, want 6 (5 restored + 1 new)", got)
	}
}

// TestGapEntryShape_MatchesEventEntry pins the hand-built JSON to the real
// EventEntry tags: persist deliberately does not import clievent in
// production code, so the linkage lives here, in the test, where the import
// is free — a renamed tag on either side fails this before it silently
// produces gap records nobody can parse.
func TestGapEntryShape_MatchesEventEntry(t *testing.T) {
	t.Parallel()
	src := gapEntryJSON{Time: 42, Type: gapEntryType, Summary: "s", Detail: "d"}
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	var round clievent.EventEntry
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	if round.Time != 42 || round.Type != gapEntryType || round.Summary != "s" || round.Detail != "d" {
		t.Errorf("round-trip lost fields: %+v", round)
	}
}

// TestGapEntryType_IsRegisteredKind: the gap record's type is spelled by
// schema (persist stays clear of clievent in production code), so this is
// where it is held to clievent's kind registry — the dashboard and the
// history readers (history/merged compares clievent.KindPersistGap) only
// recognise registered kinds.
func TestGapEntryType_IsRegisteredKind(t *testing.T) {
	t.Parallel()
	if !clievent.IsKnownKind(gapEntryType) || gapEntryType != clievent.KindPersistGap {
		t.Errorf("gapEntryType = %q, want the registered clievent.KindPersistGap (%q)", gapEntryType, clievent.KindPersistGap)
	}
}

// blockOpen makes writerFor fail for key until the returned func runs: a
// directory where the log file belongs fails Recover's truncate and the open.
func blockOpen(t *testing.T, dir, key string) (unblock func()) {
	t.Helper()
	path := LogPath(dir, key)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir over the log path: %v", err)
	}
	return func() {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove the directory over the log path: %v", err)
		}
	}
}

// TestGapRecord_OpenWriterFailureCarriesGap: a batch the run goroutine drops
// because the key's files will not open must leave a gap record once they do,
// and so must the channel-full count that batch was carrying.
func TestGapRecord_OpenWriterFailureCarriesGap(t *testing.T) {
	t.Parallel()
	p, dir := newTestPersister(t)
	const key = "feishu:p2p:gap-open-fail"
	s := &sessionSink{p: p, key: key, stem: KeyHash(key)}
	unblock := blockOpen(t, dir, key)

	s.pendingGap.Add(5)
	s.accept([]Entry{entry(t, 1700000001000, "lost1"), entry(t, 1700000001001, "lost2")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := p.Stats().Dropped; got != 2 {
		t.Fatalf("Stats().Dropped = %d, want 2 for the batch whose writer would not open", got)
	}

	unblock()
	s.accept([]Entry{entry(t, 1700000002000, "after")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush after the open succeeds: %v", err)
	}
	assertRecoveredLog(t, p, dir, key,
		"gap:dropped=7 reason=persist_channel_full+persist_write_failed", "after")
}

// TestGapRecord_RetiredGapRecordIsRestored: a gap record that reached the log
// buffer is not yet on disk. When the flush fails and the writer is retired,
// its count must come back for the next gap record, and the record itself
// must not be counted as a dropped event.
func TestGapRecord_RetiredGapRecordIsRestored(t *testing.T) {
	p, dir := newTestPersister(t, func(o *Options) { o.FlushInterval = time.Hour })
	full := installFullDisk(t, dir)
	const key = "feishu:p2p:gap-retired"
	s := &sessionSink{p: p, key: key, stem: KeyHash(key)}

	s.accept([]Entry{entry(t, 1700000001000, "before")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush before the outage: %v", err)
	}

	full.Store(true)
	s.pendingGap.Add(3)
	s.accept([]Entry{entry(t, 1700000002000, "during")}, false)
	if err := flushOrFail(t, p); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Flush on a full disk: err=%v, want ENOSPC", err)
	}
	if got := p.Stats().Dropped; got != 1 {
		t.Errorf("Stats().Dropped = %d, want 1: the lost gap record is bookkeeping, not an event", got)
	}

	full.Store(false)
	s.accept([]Entry{entry(t, 1700000003000, "after")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush after the disk freed up: %v", err)
	}
	assertRecoveredLog(t, p, dir, key,
		"before", "gap:dropped=4 reason=persist_channel_full+persist_write_failed", "after")
}

// TestGapRecord_DroppingStemCapCountsGap: a batch dropped because its stem's
// deferral FIFO is full leaves a gap record behind the batches that were
// deferred ahead of it, not in front of them.
func TestGapRecord_DroppingStemCapCountsGap(t *testing.T) {
	release, started := blockingRemoveHook(t)
	p, dir := newTestPersister(t, func(o *Options) { o.ChannelBuffer = 2 * droppingPendingMaxBatches })
	const key = "feishu:p2p:gap-dropping-cap"
	sink := p.SinkFor(key)
	sink([]Entry{entry(t, 1700000000000, "seed")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	dropErr := make(chan error, 1)
	go func() { dropErr <- p.DropKey(ctx, key) }()
	<-started
	want := make([]string, 0, droppingPendingMaxBatches+2)
	for i := 0; i < droppingPendingMaxBatches; i++ {
		u := fmt.Sprintf("deferred-%d", i)
		sink([]Entry{entry(t, 1700000001000+int64(i), u)}, false)
		want = append(want, u)
	}
	sink([]Entry{entry(t, 1700000002000, "lost1"), entry(t, 1700000002001, "lost2"), entry(t, 1700000002002, "lost3")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush while the stem is dropping: %v", err)
	}
	if got := p.Stats().Dropped; got != 3 {
		t.Fatalf("Stats().Dropped = %d, want 3 for the batch past the deferral cap", got)
	}
	close(release)
	if err := <-dropErr; err != nil {
		t.Fatalf("DropKey: %v", err)
	}
	// opCh is FIFO: this Flush lands after opDropDone, so the deferred batches
	// are on disk before "after" is sent.
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush after the drop: %v", err)
	}
	sink([]Entry{entry(t, 1700000003000, "after")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	want = append(want, "gap:dropped=3 reason=persist_channel_full", "after")
	assertRecoveredLog(t, p, dir, key, want...)
}

// TestGapRecord_DropKeyClearsRunGap: DropKey removes the files a pending gap
// would have marked, so a recreated log must not open with a stale gap.
func TestGapRecord_DropKeyClearsRunGap(t *testing.T) {
	t.Parallel()
	p, dir := newTestPersister(t)
	const key = "feishu:p2p:gap-dropkey"
	sink := p.SinkFor(key)
	blockOpen(t, dir, key)
	sink([]Entry{entry(t, 1700000001000, "lost")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	// removeKeyFiles takes the empty directory over the log path with it.
	if err := p.DropKey(ctx, key); err != nil {
		t.Fatalf("DropKey: %v", err)
	}
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush after the drop: %v", err)
	}
	sink([]Entry{entry(t, 1700000002000, "fresh")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	assertRecoveredLog(t, p, dir, key, "fresh")
}

// TestGapRecord_FlushedGapIsNotRepeated: once a flush makes a gap record
// durable its count is spent; a later outage must report only its own loss.
func TestGapRecord_FlushedGapIsNotRepeated(t *testing.T) {
	p, dir := newTestPersister(t, func(o *Options) { o.FlushInterval = time.Hour })
	full := installFullDisk(t, dir)
	const key = "feishu:p2p:gap-flushed"
	s := &sessionSink{p: p, key: key, stem: KeyHash(key)}

	s.pendingGap.Add(2)
	s.accept([]Entry{entry(t, 1700000001000, "first")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	full.Store(true)
	s.accept([]Entry{entry(t, 1700000002000, "during")}, false)
	if err := flushOrFail(t, p); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Flush on a full disk: err=%v, want ENOSPC", err)
	}
	full.Store(false)
	s.accept([]Entry{entry(t, 1700000003000, "after")}, false)
	assertRecoveredLog(t, p, dir, key,
		"gap:dropped=2 reason=persist_channel_full", "first",
		"gap:dropped=1 reason=persist_write_failed", "after")
}

// TestGapRecord_GapWriteFailureKeepsTally: when writing the gap record itself
// hits the full disk, its count must survive along with the batch it fronted.
func TestGapRecord_GapWriteFailureKeepsTally(t *testing.T) {
	p, dir := newTestPersister(t, func(o *Options) { o.FlushInterval = time.Hour })
	full := installFullDisk(t, dir)
	const key = "feishu:p2p:gap-write-fail"
	s := &sessionSink{p: p, key: key, stem: KeyHash(key)}
	s.accept([]Entry{entry(t, 1700000001000, "before")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// A filler record (seq 2) that leaves 4 bytes of the log buffer free: it
	// stays buffered, and the gap record written next has to spill.
	framed := func(pad int) (Entry, int64) {
		e := Entry{TimeMS: 1700000002000, JSON: []byte(`{"uuid":"filler","summary":"` + strings.Repeat("x", pad) + `"}`)}
		body, err := schema.MarshalRecordInto(new(bytes.Buffer), &schema.Record{V: schema.WireVersion, Seq: 2, Type: schema.TypeEntry, Entry: e.JSON})
		if err != nil {
			t.Fatal(err)
		}
		n, err := WriteRecordRaw(io.Discard, body)
		if err != nil {
			t.Fatal(err)
		}
		return e, n
	}
	// Two steps: the frame's length prefix gains digits as the body grows.
	const target = logWriteBufSize - 4
	_, n := framed(0)
	pad := target - int(n)
	_, n = framed(pad)
	filler, n := framed(pad - (int(n) - target))
	if n != target {
		t.Fatalf("filler frames to %d bytes, want %d", n, target)
	}

	full.Store(true)
	s.accept([]Entry{filler}, false)
	s.pendingGap.Add(2)
	s.accept([]Entry{entry(t, 1700000003000, "fronted")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush: %v, want nil (the ingest path already retired the writer)", err)
	}
	if got := p.Stats().Dropped; got != 2 {
		t.Errorf("Stats().Dropped = %d, want 2 (filler and fronted)", got)
	}
	full.Store(false)
	s.accept([]Entry{entry(t, 1700000004000, "after")}, false)
	assertRecoveredLog(t, p, dir, key,
		"before", "gap:dropped=4 reason=persist_channel_full+persist_write_failed", "after")
}

// TestGapRecord_IdxSyncFailureRestoresGap: a gap record is durable only once
// the idx fsync succeeds. When that fsync fails the carried count must not be
// spent: retiring the writer hands it back for key's next gap record.
func TestGapRecord_IdxSyncFailureRestoresGap(t *testing.T) {
	p, dir := newTestPersister(t, func(o *Options) { o.IdxStride = 1 })
	const key = "feishu:p2p:gap-idx-sync"
	sink := p.SinkFor(key)
	sink([]Entry{entry(t, 1700000001000, "before")}, false)
	if err := flushOrFail(t, p); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The run goroutine is gone, so the test owns a hand-built writer.
	logPath := LogPath(dir, key)
	idxPath := filepath.Join(dir, KeyHash(key)+idxExt)
	rec, err := Recover(logPath, idxPath)
	if err != nil {
		t.Fatalf("Recover: %v", err)
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
		key: key, stem: KeyHash(key),
		logFile: logFile, logBuf: acquireLogBuf(logFile), idxWriter: idxW,
		logPath: logPath, idxPath: idxPath,
		nextSeq: rec.NextSeq, bytes: rec.LogSize,
	}
	t.Cleanup(func() { _ = w.close() })

	p.runGap[key] = gapTally{n: 3, cause: gapChannelFull}
	job := batchJob{Key: key, Stem: KeyHash(key), Entries: []Entry{entry(t, 1700000002000, "fronted")}}
	if err := p.writeGapRecord(job, w, new(bytes.Buffer), new(schema.Record)); err != nil {
		t.Fatalf("writeGapRecord: %v", err)
	}
	if err := p.appendRecord(w, new(bytes.Buffer), new(schema.Record), job.Entries[0]); err != nil {
		t.Fatalf("appendRecord: %v", err)
	}
	w.dirty = true

	idxW.syncFailHook = func() error { return errors.New("injected idx fsync EIO") }
	err = w.flush(p)
	if err == nil || !w.poisoned {
		t.Fatalf("flush: err=%v poisoned=%v, want the injected idx fsync error and a poisoned writer", err, w.poisoned)
	}
	p.settleFlush(key, w, "flush", err)
	got := p.runGap[key]
	if got.n != 4 || got.cause != gapChannelFull|gapWriteFailed {
		t.Errorf("runGap[%q] = %+v, want n=4 (3 carried + fronted) with both causes", key, got)
	}
}
