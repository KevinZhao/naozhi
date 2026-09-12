package persist

import (
	"bytes"
	"encoding/json"
	"log/slog"
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
	job := batchJob{Key: s.key, Stem: s.stem, Entries: owned, arena: arena}
	select {
	case p.in <- job:
	default:
		putEntryArena(arena)
		p.droppedCnt.Add(int64(len(entries)))
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
			putEntryArena(job.arena)
			n := len(job.Entries)
			p.droppedCnt.Add(int64(n))
			p.opts.Observer.OnDrop(n)
			slog.Warn("event log persist: dropping-stem pending cap reached; dropping batch",
				"key", job.Key, "stem", job.Stem, "count", n,
				"pending", len(ds.pending))
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
		slog.Error("event log persist: cannot open writer",
			"key", job.Key, "err", err)
		return
	}

	// One pooled buffer for the whole batch amortises json's encodeState alloc.
	encBuf := recordBufPool.Get().(*bytes.Buffer)
	defer putRecordBuf(encBuf)
	var written int
	// One stack Record reused per entry: MarshalRecordInto only reads it
	// synchronously and never retains the pointer (#2088).
	var rec schema.Record
	for _, e := range job.Entries {
		rec.V = schema.WireVersion
		rec.Seq = w.nextSeq
		rec.Type = schema.TypeEntry
		rec.Entry = json.RawMessage(e.JSON)
		encBuf.Reset()
		body, err := schema.MarshalRecordInto(encBuf, &rec)
		if err != nil {
			// Over-size / malformed — count and drop just this entry.
			p.malformedCnt.Add(1)
			p.opts.Observer.OnMalformed()
			slog.Warn("event log persist: marshal entry failed",
				"key", job.Key, "seq", w.nextSeq, "err", err)
			continue
		}
		// Always write through logBuf, never WriteRecordRaw(logFile, ...):
		// bytes written straight to the fd would land out of order relative
		// to anything still pending in the bufio buffer.
		n, err := WriteRecordRaw(w.logBuf, body)
		if err != nil {
			// Drop just this record; WriteRecordRaw writes the whole frame or
			// nothing, so file state stays consistent.
			p.droppedCnt.Add(1)
			p.opts.Observer.OnDrop(1)
			slog.Warn("event log persist: write entry failed",
				"key", job.Key, "seq", w.nextSeq, "err", err)
			continue
		}
		// Pending idx entry — we hold it until fsync time to keep
		// log-before-idx ordering (see recovery.go).
		w.pendingIdx = append(w.pendingIdx, schema.IdxEntry{
			Seq:     w.nextSeq,
			ByteOff: w.bytes,
			Len:     int32(n),
			TimeMS:  e.TimeMS,
		})
		w.bytes += n
		w.nextSeq++
		// entriesSinceIdxWrite is NOT advanced here: it is the stride-cycle
		// phase of pendingIdx[0], read by flush() as selectForIdx's start and
		// advanced by len(pendingIdx) mod stride only after a durable idx
		// sync. Advancing per entry would double-count and break alignment.
		written++
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
			slog.Warn("event log persist: pre-rotate flush failed",
				"key", job.Key, "err", err)
		} else if err := p.rotate(job.Key, job.Stem, w); err != nil {
			slog.Warn("event log persist: rotate failed",
				"key", job.Key, "err", err)
		}
	}
}

// writerFor returns an open perKeyWriter for key, creating or
// recovering the file pair on first access.
