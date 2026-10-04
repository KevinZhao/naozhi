package persist

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// sink.go — the ingest path: a session's PersistSink hands entries to the run
// goroutine, which turns one batch into records on disk. Extracted from
// persister.go (J9 of #2548); accept and handleBatch are the two halves of one
// story and were 640 lines apart.

type sessionSink struct {
	p    *Persister
	key  string
	stem string
	// pendingGap counts entries this sink dropped since its last successful
	// hand-off. The drop site cannot leave a durable mark itself — the channel
	// is full, and pushing a marker into the same channel would drop with the
	// batch — so the NEXT batch that does get through carries the count, and
	// handleBatch writes a gap record in front of it (gap.go). Reading
	// events/<key> then distinguishes "no messages in this span" from "a batch
	// was dropped here", which droppedCnt (process memory, reset on restart)
	// and one Warn line (log retention) never could.
	pendingGap atomic.Int64
}

func (s *sessionSink) accept(entries []Entry, replayPhase bool) {
	p := s.p
	if p.closed.Load() {
		return
	}
	if replayPhase {
		p.replayLeakCnt.Add(int64(len(entries)))
		p.opts.Observer.OnReplayLeak(len(entries))
		// Log rather than panic so the ordering bug is observable via
		// replayLeakCnt / OnReplayLeak without crashing the process.
		slog.Error("event log persist: replay-phase entries reached sink",
			"key", s.key, "count", len(entries),
			"dev_mode", p.opts.DevMode)
		return
	}
	if len(entries) == 0 {
		return
	}
	// Swap-and-restore: if THIS batch also drops, the count goes back (plus
	// the batch) rather than vanishing.
	gapN := s.pendingGap.Swap(0)

	// Entry.JSON and the entries slice are borrowed — the producer may reuse
	// them as soon as accept returns — so copy every body into one pooled
	// arena and materialise our own headers (#1524). Two passes: append all
	// bytes first (the buffer may grow and move), then resolve sub-slices.
	// owned/spans come from the same arena rather than make() (#1630).
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
	for i, e := range entries {
		start := arena.buf.Len()
		arena.buf.Write(e.JSON)
		spans[i] = arenaSpan{start: start, end: arena.buf.Len()}
		owned[i] = Entry{TimeMS: e.TimeMS}
	}
	all := arena.buf.Bytes()
	for i := range owned {
		owned[i].JSON = all[spans[i].start:spans[i].end]
	}
	job := batchJob{Key: s.key, Stem: s.stem, Entries: owned, arena: arena, gapN: gapN}
	select {
	case p.in <- job:
	default:
		putEntryArena(arena)
		p.droppedCnt.Add(int64(len(entries)))
		// The swapped-out gap count rides back in plus this batch: the drop
		// tally survives until a batch finally gets through and materialises
		// the gap record on disk.
		s.pendingGap.Add(gapN + int64(len(entries)))
		p.opts.Observer.OnDrop(len(entries))
		// channel_used distinguishes a wedged writer from an instantaneous
		// burst overrun (#1184).
		slog.Warn("event log persist: channel full; dropping batch",
			"key", s.key, "count", len(entries),
			"channel_used", len(p.in),
			"channel_cap", cap(p.in))
	}
}

// DropKey closes any open writer for key, then removes its log + idx
// files. Safe from any goroutine; waits for the writer goroutine to
// acknowledge the drop.
func (p *Persister) handleBatch(job batchJob, now time.Time) {
	// Stem mid-removal: defer into the per-stem FIFO instead of blocking on
	// the unlink. The deferred job keeps its arena (the replaying handleBatch
	// returns it), so return WITHOUT the defer below. Overflow past the cap
	// takes the channel-full drop path (#1848).
	if ds, ok := p.dropping[job.Stem]; ok {
		if len(ds.pending) >= droppingPendingMaxBatches {
			p.dropDeferral(ds, job)
			return
		}
		ds.pending = append(ds.pending, job)
		return
	}

	// Return the arena owning this batch's Entry.JSON once every entry is
	// handled; the defer covers every early return. nil-safe (#1524).
	defer putEntryArena(job.arena)
	w, err := p.writerFor(job.Key, job.Stem)
	if err != nil {
		p.holdGap(job.Key, gapTally{n: job.gapN, cause: gapChannelFull})
		p.noteRunDrop(job.Key, len(job.Entries), gapWriteFailed)
		p.noteFailure(job.Key, "open writer", err)
		return
	}

	// One pooled buffer for the whole batch amortises json's encodeState alloc.
	encBuf := recordBufPool.Get().(*bytes.Buffer)
	defer putRecordBuf(encBuf)
	// One Record reused per entry: MarshalRecordInto only reads it
	// synchronously and never retains the pointer (#2088).
	var rec schema.Record
	if err := p.writeGapRecord(job, w, encBuf, &rec); err != nil {
		p.dropRestAndRetire(job.Key, w, len(job.Entries), "write gap record", err)
		return
	}
	var written int
	for i, e := range job.Entries {
		err := p.appendRecord(w, encBuf, &rec, e)
		switch {
		case err == nil:
			written++
		case errors.Is(err, errMarshalRecord):
			// Over-size / malformed — count and drop just this entry.
			p.malformedCnt.Add(1)
			p.opts.Observer.OnMalformed()
			slog.Warn("event log persist: marshal entry failed",
				"key", job.Key, "seq", w.nextSeq, "err", err)
		case recordRejected(err):
			p.droppedCnt.Add(1)
			p.opts.Observer.OnDrop(1)
			slog.Warn("event log persist: write entry failed",
				"key", job.Key, "seq", w.nextSeq, "err", err)
		default:
			// An I/O error is latched in logBuf, so the rest of the batch would
			// fail the same way: drop it and retire the writer (failure.go).
			p.dropRestAndRetire(job.Key, w, len(job.Entries)-i, "write entry", err)
			return
		}
	}
	if written > 0 {
		p.writtenCnt.Add(int64(written))
		p.opts.Observer.OnWrite(written)
	}
	if !w.dirty {
		w.dirty = true
		w.firstDirtyAt = now
	}
	w.lastActivity = now

	// Rotate after the whole batch so its records are never split across
	// old/new files.
	if w.bytes >= p.opts.MaxFileBytes {
		if err := w.flush(p); err != nil {
			p.settleFlush(job.Key, w, "pre-rotate flush", err)
		} else if err := p.rotate(job.Key, job.Stem, w); err != nil {
			slog.Warn("event log persist: rotate failed",
				"key", job.Key, "err", err)
		}
	}
}

// errMarshalRecord marks an entry appendRecord could not frame, as opposed to
// one whose framed bytes could not be written.
var errMarshalRecord = errors.New("frame entry")

// recordRejected reports a write error that refused one record before any
// byte reached logBuf, leaving the writer usable.
func recordRejected(err error) bool {
	return errors.Is(err, ErrEmptyBody) || errors.Is(err, schema.ErrRecordTooLarge)
}

// appendRecord frames e as w's next record and writes it into w.logBuf. It
// always writes through logBuf, never straight to logFile: bytes written to
// the fd would land out of order relative to anything still buffered. The idx
// entry is held in pendingIdx until fsync time to keep log-before-idx
// ordering (see recovery.go).
func (p *Persister) appendRecord(w *perKeyWriter, encBuf *bytes.Buffer, rec *schema.Record, e Entry) error {
	rec.V = schema.WireVersion
	rec.Seq = w.nextSeq
	rec.Type = schema.TypeEntry
	rec.Entry = json.RawMessage(e.JSON)
	encBuf.Reset()
	body, err := schema.MarshalRecordInto(encBuf, rec)
	if err != nil {
		return fmt.Errorf("%w: %w", errMarshalRecord, err)
	}
	n, err := WriteRecordRaw(w.logBuf, body)
	if err != nil {
		return err
	}
	// entriesSinceIdxWrite is NOT advanced here: it is the stride-cycle phase
	// of pendingIdx[0], advanced by flush() only after a durable idx sync.
	w.pendingIdx = append(w.pendingIdx, schema.IdxEntry{
		Seq:     w.nextSeq,
		ByteOff: w.bytes,
		Len:     int32(n),
		TimeMS:  e.TimeMS,
	})
	w.bytes += n
	w.nextSeq++
	return nil
}

// writeGapRecord writes key's pending gap, if any, as the record in front of
// job's entries, and moves the tally onto w until a flush makes it durable.
// Only an I/O error is returned; on any failure the tally stays pending.
func (p *Persister) writeGapRecord(job batchJob, w *perKeyWriter, encBuf *bytes.Buffer, rec *schema.Record) error {
	t := p.runGap[job.Key]
	t.add(gapTally{n: job.gapN, cause: gapChannelFull})
	if t.n == 0 {
		return nil
	}
	if len(job.Entries) == 0 {
		p.runGap[job.Key] = t
		return nil
	}
	body, err := gapRecordJSON(t, job.Entries[0].TimeMS)
	if err == nil {
		err = p.appendRecord(w, encBuf, rec, Entry{TimeMS: job.Entries[0].TimeMS, JSON: body})
	}
	if err != nil {
		p.runGap[job.Key] = t
		if errors.Is(err, errMarshalRecord) || recordRejected(err) {
			slog.Warn("event log persist: gap record rejected",
				"key", job.Key, "dropped", t.n, "err", err)
			return nil
		}
		return err
	}
	delete(p.runGap, job.Key)
	w.carriedGap.add(t)
	w.carriedGapRecs++
	return nil
}

// dropDeferral drops a batch for a stem whose deferral FIFO is full. The
// lost events come after every deferred batch, so the tally waits on ds and
// joins key's gap only once those batches are replayed.
func (p *Persister) dropDeferral(ds *dropState, job batchJob) {
	putEntryArena(job.arena)
	n := len(job.Entries)
	p.droppedCnt.Add(int64(n))
	p.opts.Observer.OnDrop(n)
	ds.gapKey = job.Key
	ds.gap.add(gapTally{n: job.gapN + int64(n), cause: gapChannelFull})
	slog.Warn("event log persist: dropping-stem pending cap reached; dropping batch",
		"key", job.Key, "stem", job.Stem, "count", n,
		"pending", len(ds.pending))
}

// writerFor returns an open perKeyWriter for key, creating or
// recovering the file pair on first access.
