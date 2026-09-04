// Package session — eventlog_bridge.go
//
// Eventlog tiers: cli.EventLog.ring (in-memory, lossy), persist.Persister
// spool (durable, authoritative on restart, fed via this bridge's PersistSink),
// naozhilog.Source (replay from the spool) and history/merged.Source (composed
// read over the Claude-JSONL fallback). persist.Persister deliberately does not
// implement eventlog/api.EventStore; it is fed via the per-key PersistSink
// callback and read back via persist.Recover (#1570).
//
// This bridge is the only place EventEntry → persist.Entry conversion lives.
package session

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/naozhi/naozhi/internal/attachment/tracker"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/history/merged"
	"github.com/naozhi/naozhi/internal/history/naozhilog"
)

// newEventLogLocalSource builds the naozhi-native event-log history source for
// a session key (tier-1 of the history stack). It is NOT backend-specific: the
// naozhilog spool is written for every backend via the persist sink, so the
// constructor lives here — the single place the naozhilog import edge is
// allowed (#403, #567).
func newEventLogLocalSource(eventLogDir, key string) *naozhilog.Source {
	return naozhilog.New(eventLogDir, key)
}

// mergeWithEventLog composes the event-log local tier in front of a
// backend-provided fallback. Empty eventLogDir opts out and returns the
// fallback unchanged. A nil fallback is replaced by history.Noop so the merged
// read path stays safe.
func mergeWithEventLog(eventLogDir, key string, fallback history.Source) history.Source {
	if fallback == nil {
		fallback = history.Noop{}
	}
	if eventLogDir == "" {
		return fallback
	}
	return &merged.Source{
		Local:    newEventLogLocalSource(eventLogDir, key),
		Fallback: fallback,
	}
}

// bridgeEncBuf pools a bytes.Buffer + json.Encoder pair so the bridge hot path
// (≥5 events/s × N sessions) avoids the encodeState allocation json.Marshal
// performs per call. Mirrors jsonEncPool in internal/server/dashboard.go.
type bridgeEncBuf struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

var bridgeEncPool = sync.Pool{
	New: func() any {
		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)
		// The persist tier reads back via json.Unmarshal, which accepts
		// unescaped HTML; disabling escape avoids needless byte expansion.
		enc.SetEscapeHTML(false)
		return &bridgeEncBuf{buf: buf, enc: enc}
	},
}

// bridgeEncMaxCap caps buffer reuse so a one-off oversized event does not
// permanently pin a large heap buffer.
const bridgeEncMaxCap = 64 * 1024

// span records a [start,end) byte range inside the shared pooled encode buffer
// for one EventEntry plus that entry's TimeMS. Package-scoped so batchScratch
// can carry a reusable []span across AppendBatch calls (#1629, #1619).
type span struct {
	start, end int
	timeMS     int64
}

// batchScratch pools the two per-AppendBatch helper slices so their backing
// arrays survive across calls instead of escaping to the heap per batch
// (#1629). Mirrors the bridgeEncPool idiom.
type batchScratch struct {
	out   []persist.Entry
	spans []span
}

var batchScratchPool = sync.Pool{
	New: func() any { return &batchScratch{} },
}

// batchScratchMaxCap caps slice reuse so a one-off huge batch does not pin
// large backing arrays in the pool forever (same rationale as bridgeEncMaxCap).
const batchScratchMaxCap = 4096

// newEventLogSink translates a per-key persist.PersistSink (persist.Entry
// batches) into the cli.PersistSink contract (clievent.EventEntry batches);
// neither cli nor persist imports the other, so the conversion lives only here.
//
// Ordering contract (RFC §3.2.2 / attachment-refcount §3.2): this sink MUST be
// installed on cli.EventLog.SetPersistSink AFTER any pre-hook InjectHistory
// calls complete; spawnSession is the sole production caller responsible.
// attachTracker is optional: non-replay entries with ImagePaths bump the
// attachment refcount. A marshal failure on one EventEntry does NOT abort the
// batch — the entry is logged and skipped (best-effort persist, never block).
func newEventLogSink(persisterSink persist.PersistSink, attachTracker *tracker.Tracker, keyhash string) cli.PersistSink {
	return func(entries []clievent.EventEntry, replayPhase bool) {
		if len(entries) == 0 {
			return
		}

		// Single-entry fast path shares persistOneEntry with the
		// cli.PersistSinkOne path so marshal / refcount logic lives in one
		// place (#410). The bytes and slice header DO escape because
		// persisterSink retains entries; a byte-slice pool would need a
		// copy-on-take re-contract of every sink, so none is attempted.
		if len(entries) == 1 {
			persistOneEntry(persisterSink, attachTracker, keyhash, entries[0], replayPhase)
			return
		}

		// Helper slices come from a pool (#1629), reset to [:0] and returned
		// at every exit path. `out`'s persist.Entry values alias the pooled
		// encode buffer, but persisterSink copies the bytes into its own arena
		// synchronously, so both may be recycled once it returns.
		bs := batchScratchPool.Get().(*batchScratch)
		out := bs.out[:0]
		eb := bridgeEncPool.Get().(*bridgeEncBuf)
		// Explicit Put before each return (no defer) keeps the hot path cheap.
		// All entries are encoded into ONE pooled buffer without Resetting
		// between them; persist gets a borrowed sub-slice per entry (#1524).
		// Offsets are resolved after the encode pass because the buffer may
		// grow (and move) mid-loop.
		eb.buf.Reset()
		spans := bs.spans[:0]
		for _, e := range entries {
			start := eb.buf.Len()
			if err := eb.enc.Encode(e); err != nil {
				slog.Warn("eventlog bridge: marshal entry failed",
					"uuid", e.UUID, "type", e.Type, "err", err)
				eb.buf.Truncate(start) // drop the partial encode
				continue
			}
			end := eb.buf.Len()
			// json.Encoder appends a trailing '\n'; strip it from the span.
			if end > start && eb.buf.Bytes()[end-1] == '\n' {
				end--
			}
			spans = append(spans, span{start: start, end: end, timeMS: e.Time})

			// Refcount bump for every attachment path the entry carries.
			// Replay batches are excluded: replay restores from the persist
			// tier, not a fresh user reference; bumping would reset
			// LastReferencedAt and defeat refTTL expiry.
			if !replayPhase && attachTracker != nil && keyhash != "" && len(e.ImagePaths) > 0 {
				attachTracker.OnPersistedEntry(keyhash, e.ImagePaths, e.Time)
			}
		}
		// Scratch return is inlined at both exits (a captured closure would
		// heap-allocate per batch). The possibly-reallocated headers are
		// written back so grown capacity is kept; slices wider than
		// batchScratchMaxCap are left for GC instead of pinning an outsized array.
		if len(spans) == 0 {
			if cap(out) <= batchScratchMaxCap && cap(spans) <= batchScratchMaxCap {
				bs.out = out[:0]
				bs.spans = spans[:0]
				batchScratchPool.Put(bs)
			}
			if eb.buf.Cap() <= bridgeEncMaxCap {
				bridgeEncPool.Put(eb)
			}
			return
		}
		all := eb.buf.Bytes()
		for _, sp := range spans {
			out = append(out, persist.Entry{JSON: all[sp.start:sp.end], TimeMS: sp.timeMS})
		}
		// persisterSink copies the borrowed bytes synchronously, so eb and the
		// scratch slices may be returned only AFTER it returns (out's JSON
		// fields alias eb.buf).
		persisterSink(out, replayPhase)
		if cap(out) <= batchScratchMaxCap && cap(spans) <= batchScratchMaxCap {
			bs.out = out[:0]
			bs.spans = spans[:0]
			batchScratchPool.Put(bs)
		}
		if eb.buf.Cap() <= bridgeEncMaxCap {
			bridgeEncPool.Put(eb)
		}
	}
}

// persistOneEntry marshals a single EventEntry through bridgeEncPool and
// forwards it to persisterSink. Shared by newEventLogSink's len==1 fast path
// and newEventLogSinkOne so the encode/copy/refcount logic lives in exactly one
// place (#410).
func persistOneEntry(persisterSink persist.PersistSink, attachTracker *tracker.Tracker, keyhash string, e clievent.EventEntry, replayPhase bool) {
	eb := bridgeEncPool.Get().(*bridgeEncBuf)
	eb.buf.Reset()
	if err := eb.enc.Encode(e); err != nil {
		slog.Warn("eventlog bridge: marshal entry failed",
			"uuid", e.UUID, "type", e.Type, "err", err)
		if eb.buf.Cap() <= bridgeEncMaxCap {
			bridgeEncPool.Put(eb)
		}
		return
	}
	raw := eb.buf.Bytes()
	if n := len(raw); n > 0 && raw[n-1] == '\n' {
		raw = raw[:n-1]
	}
	// persisterSink copies the bytes it retains (pooled per-batch arena), so
	// it gets the borrowed encoder slice directly (#1524). eb is returned to
	// the pool only AFTER persisterSink returns, because raw aliases eb.buf.
	var stackArr [1]persist.Entry
	out := append(stackArr[:0], persist.Entry{JSON: raw, TimeMS: e.Time})
	if !replayPhase && attachTracker != nil && keyhash != "" && len(e.ImagePaths) > 0 {
		attachTracker.OnPersistedEntry(keyhash, e.ImagePaths, e.Time)
	}
	persisterSink(out, replayPhase)
	if eb.buf.Cap() <= bridgeEncMaxCap {
		bridgeEncPool.Put(eb)
	}
}

// newEventLogSinkOne is the cli.PersistSinkOne counterpart to newEventLogSink,
// wiring Append's single-entry fast path to the per-key persister without a
// `[]EventEntry{e}` slice literal. Both paths share persistOneEntry so the wire
// format and attachment-tracker behaviour are identical (#410).
func newEventLogSinkOne(persisterSink persist.PersistSink, attachTracker *tracker.Tracker, keyhash string) cli.PersistSinkOne {
	return func(e clievent.EventEntry, replayPhase bool) {
		persistOneEntry(persisterSink, attachTracker, keyhash, e, replayPhase)
	}
}
