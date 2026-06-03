// Package session — eventlog_bridge.go
//
// NEEDS-DESIGN (R243-ARCH-12, REPEAT-5): three eventlog tiers shadow
// each other today.
//
//   - cli.EventLog.ring — in-memory bounded ring shared with the WS
//     subscriber tier. Pure RAM, lossy on process exit.
//   - persist.Persister.spool — per-key durable spool that lands on
//     disk via this bridge's PersistSink. Authoritative on restart.
//   - naozhilog.Source.replay — read-side replay that re-hydrates the
//     ring from the spool when sessions reattach (history panel,
//     dashboard rewind).
//
// Each tier owns its own append/read/subscribe primitives; the four
// concrete backends (memory ring, persist spool, naozhilog source,
// scratch event store) each expose a slightly different API even
// though their conceptual contract is identical: "append, read by
// range, subscribe to tail."
//
// The unification plan tracked under R243-ARCH-12 is to publish a
// single `EventStore` interface in (likely) internal/eventlog/api/:
//
//	type EventStore interface {
//	    Append(ctx, []EventEntry) error
//	    Read(ctx, ReadRange) ([]EventEntry, error)
//	    Subscribe(ctx, SubFilter) (<-chan EventEntry, error)
//	}
//
// plus a central registry that the four backends register with. This
// bridge stays in place — its job is exactly the EventEntry⇄persist
// .Entry hop — but the cli/persist/naozhilog import edges collapse to
// "everyone implements EventStore" and the session layer can swap the
// backend (e.g. for a tests-only no-disk mode) without re-plumbing
// the spawnSession site. The migration is staged because each tier
// has accumulated its own performance hot path (see R215-PERF-P1-1
// pooling below, R228-PERF-1 single-entry fast path, R240-PERF-4
// escape analysis); a naive interface-everywhere refactor would
// regress those without an evals pass.
//
// Until the api/ subpackage lands, the bridge contract here is the
// only place EventEntry → persist.Entry conversion lives. Adding new
// backends should follow the registry path, not bolt on alongside.
package session

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/naozhi/naozhi/internal/attachment/tracker"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/history"
	"github.com/naozhi/naozhi/internal/history/merged"
	"github.com/naozhi/naozhi/internal/history/naozhilog"
)

// newEventLogLocalSource builds the naozhi-native, in-process event-log
// history source for a session key. This is tier-1 of the history stack
// and is NOT backend-specific: the naozhilog spool is written for every
// backend (claude / kiro / future) via the eventlog persist sink above,
// so the constructor lives here in the bridge — the single place the
// naozhilog import edge is allowed — rather than scattered across
// router_core.go (background loader) and router_lifecycle.go
// (attachHistorySource). Consolidating the two call sites here is the
// minimal step of R214-ARCH-3 / R215-ARCH-P1-5 (#403, #567): the generic
// session layer no longer hand-builds backend history sources inline.
func newEventLogLocalSource(eventLogDir, key string) *naozhilog.Source {
	return naozhilog.New(eventLogDir, key)
}

// mergeWithEventLog composes the naozhi event-log local tier in front of
// a backend-provided fallback source. When eventLogDir is empty the
// event-log tier is opted out and the fallback is returned unchanged, so
// callers get a single source without branching on eventLogDir at the
// call site. fallback may be nil; the caller is responsible for replacing
// a nil fallback with history.Noop before invoking when it wants the
// "never nil" guarantee, but mergeWithEventLog itself tolerates nil by
// substituting history.Noop so the merged source's read path stays safe.
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

// bridgeEncBuf pools a bytes.Buffer + json.Encoder pair so eventlog
// bridge hot path (≥5 events/s × N sessions) avoids the encodeState
// allocation that json.Marshal performs each call. Mirrors the
// jsonEncPool idiom in internal/server/dashboard.go.
// R215-PERF-P1-1: replaces per-EventEntry json.Marshal reflection
// path with pooled encoder to drop the heaviest steady-state alloc
// in the persist sink closure.
type bridgeEncBuf struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

var bridgeEncPool = sync.Pool{
	New: func() any {
		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)
		// Match cli.EventEntry JSON shape: persist tier reads back via
		// json.Unmarshal which already accepts unescaped HTML chars,
		// and disabling escape avoids needless byte expansion.
		enc.SetEscapeHTML(false)
		return &bridgeEncBuf{buf: buf, enc: enc}
	},
}

// bridgeEncMaxCap caps buffer reuse so a one-off oversized event does
// not permanently pin large heap.
const bridgeEncMaxCap = 64 * 1024

// span records a [start,end) byte range inside the shared pooled encode
// buffer for one EventEntry. Hoisted to package scope (was a func-local
// type) so batchScratch can carry a reusable []span across AppendBatch
// calls. R20260602-PERF-2 (#1629).
type span struct{ start, end int }

// batchScratch pools the three per-AppendBatch helper slices the
// multi-entry sink path used to `make` on every call (out / spans / times).
// In steady state (≥5 events/s × N sessions) those three heap allocations
// escaped per call; carrying them in a sync.Pool keeps the backing arrays
// alive across calls and reuses their capacity. Mirrors the bridgeEncPool
// idiom right above. R20260602-PERF-2 (#1629).
type batchScratch struct {
	out   []persist.Entry
	spans []span
	times []int64
}

var batchScratchPool = sync.Pool{
	New: func() any { return &batchScratch{} },
}

// batchScratchMaxCap caps slice reuse so a one-off huge batch does not pin
// large backing arrays in the pool forever (same rationale as
// bridgeEncMaxCap for the encode buffer). A batch wider than this is served
// once and dropped on return instead of being recycled.
const batchScratchMaxCap = 4096

// newEventLogSink translates a per-key persist.PersistSink (which
// accepts persist.Entry batches) into the cli.PersistSink contract
// (which accepts cli.EventEntry batches).
//
// Two packages meet here precisely because neither cli nor persist
// imports the other — schema.Record.Entry is json.RawMessage, so
// the hop is always "cli marshals EventEntry → persist writes raw
// bytes". This helper is the only place the conversion lives so a
// future change to cli.EventEntry's JSON shape doesn't force every
// session call site to re-bridge.
//
// Ordering contract (RFC §3.2.2 / attachment-refcount §3.2): this
// sink MUST be installed on cli.EventLog.SetPersistSink AFTER any
// pre-hook InjectHistory calls complete. spawnSession is the sole
// production caller responsible for the ordering; router tests anchor
// that ordering in CI.
//
// attachTracker is optional: when non-nil, every non-replay
// EventEntry that carries ImagePaths is handed to
// tracker.OnPersistedEntry so the attachment refcount stays in
// sync with disk. A nil tracker disables refcount bumps — the
// event log persistence still runs. Passing keyhash up front
// spares the tracker's resolver from re-hashing on every call.
//
// Failure mode: a marshal failure on a single EventEntry does NOT
// abort the batch — the failing entry is logged and skipped. This
// matches the "best-effort persist, never block Append" policy:
// losing one event to a rare JSON encoding pathology is preferable
// to dropping the entire batch (which would otherwise include many
// valid siblings).
func newEventLogSink(persisterSink persist.PersistSink, attachTracker *tracker.Tracker, keyhash string) cli.PersistSink {
	return func(entries []cli.EventEntry, replayPhase bool) {
		if len(entries) == 0 {
			return
		}

		// R228-PERF-1: single-entry fast path avoids a heap allocation
		// for the 1-cap []persist.Entry slice. A stack-local [1]persist.Entry
		// array keeps the backing array on the stack; slicing it to [:0]
		// lets the compiler prove the escape is bounded to this frame.
		// marshal/copy/refcount semantics are identical to the loop below.
		//
		// R240-PERF-4 (validated 2026-05-24, cron-fix-F4): `go build
		// -gcflags=-m` confirms `make([]byte, len(raw))` (line 102) and
		// `append(stackArr[:0], ...)` (line 108) DO escape — persisterSink
		// is documented to retain entries (it pumps them into the per-key
		// persist tier's batch buffer), so neither the bytes nor the slice
		// header can stay on the stack. A byte-slice sync.Pool would only
		// pay off if PersistSink were re-contracted to copy-on-take; that
		// breaks every existing sink implementation. Logged here so a
		// future re-evaluator does not repeat the -gcflags walk.
		if len(entries) == 1 {
			// Delegate to the single-entry helper so the marshal /
			// refcount logic lives in exactly one place. Both this
			// branch and the cli.PersistSinkOne fast path
			// (newEventLogSinkOne) call the same helper, so a future
			// schema tweak only has to land in one place. (#410)
			persistOneEntry(persisterSink, attachTracker, keyhash, entries[0], replayPhase)
			return
		}

		// R20260602-PERF-2 (#1629): borrow the three helper slices from a
		// pool instead of make-ing them per call. They are reset to [:0]
		// (keeping their capacity) and returned at every exit path below.
		// All three are consumed entirely before persisterSink returns —
		// `out`'s persist.Entry values alias the pooled encode buffer, but
		// the slice header itself is fully drained by persisterSink (it
		// copies the bytes into its own arena, see comment below), so the
		// backing array is free to recycle once persisterSink returns.
		bs := batchScratchPool.Get().(*batchScratch)
		out := bs.out[:0]
		eb := bridgeEncPool.Get().(*bridgeEncBuf)
		// R240-PERF-7: explicit Put before each return path avoids the
		// ~10ns/call defer frame setup cost on the multi-entry hot path
		// (5-20 entries × N sessions × ≥5/s). The pool-cap guard is the
		// same as the single-entry fast path above.
		//
		// R20260531A-PERF-3 (#1524): the persist sink now copies the
		// bytes it retains (it owns a pooled per-batch arena), so the
		// bridge no longer pays a make([]byte)+copy per entry. We encode
		// every entry into ONE pooled buffer (without Resetting between
		// entries) and hand persist a borrowed sub-slice per entry. The
		// sub-slices stay valid until persisterSink returns, after which
		// eb goes back to the pool. Offsets are resolved after the encode
		// pass because the buffer may grow (and move) mid-loop.
		eb.buf.Reset()
		spans := bs.spans[:0]
		times := bs.times[:0]
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
			spans = append(spans, span{start: start, end: end})
			times = append(times, e.Time)

			// Refcount bump for every attachment path the entry
			// carries. Replay batches are excluded — replay happens
			// because the session is being restored from the persist
			// tier, not because a user just re-referenced the
			// attachment. Bumping on replay would reset
			// LastReferencedAt and defeat the refTTL expiry logic.
			if !replayPhase && attachTracker != nil && keyhash != "" && len(e.ImagePaths) > 0 {
				attachTracker.OnPersistedEntry(keyhash, e.ImagePaths, e.Time)
			}
		}
		// putScratch returns the three helper slices to the pool, writing the
		// (possibly reallocated) headers back first so a grown capacity is
		// preserved for the next call. Slices wider than batchScratchMaxCap
		// are dropped (left for GC) instead of pinning an outsized array.
		putScratch := func() {
			if cap(out) <= batchScratchMaxCap && cap(spans) <= batchScratchMaxCap && cap(times) <= batchScratchMaxCap {
				bs.out = out[:0]
				bs.spans = spans[:0]
				bs.times = times[:0]
				batchScratchPool.Put(bs)
			}
		}
		if len(spans) == 0 {
			putScratch()
			if eb.buf.Cap() <= bridgeEncMaxCap {
				bridgeEncPool.Put(eb)
			}
			return
		}
		all := eb.buf.Bytes()
		for i, sp := range spans {
			out = append(out, persist.Entry{JSON: all[sp.start:sp.end], TimeMS: times[i]})
		}
		// persisterSink copies the borrowed bytes synchronously (it owns a
		// pooled arena), so eb and the scratch slices are safe to return only
		// AFTER it returns (out's persist.Entry JSON fields alias eb.buf).
		persisterSink(out, replayPhase)
		putScratch()
		if eb.buf.Cap() <= bridgeEncMaxCap {
			bridgeEncPool.Put(eb)
		}
	}
}

// persistOneEntry marshals a single EventEntry through bridgeEncPool and
// forwards it to persisterSink. Shared between newEventLogSink's
// len(entries)==1 fast path and newEventLogSinkOne's by-value fast path
// so the encode/copy/refcount logic lives in exactly one place. Behaviour
// (marshal failure handling, pool-cap guard, attachment refcount bump,
// stack-allocated [1]persist.Entry slice) matches the inline code that
// previously lived in newEventLogSink — extracting it here changes no
// semantics, only call shape. (#410)
func persistOneEntry(persisterSink persist.PersistSink, attachTracker *tracker.Tracker, keyhash string, e cli.EventEntry, replayPhase bool) {
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
	// R20260531A-PERF-3 (#1524): persisterSink copies the bytes it
	// retains (it owns a pooled per-batch arena), so we hand it the
	// borrowed pooled-encoder slice directly instead of make([]byte)+copy.
	// eb is returned to the pool only AFTER persisterSink returns, because
	// raw aliases eb.buf's backing array until then.
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

// newEventLogSinkOne is the cli.PersistSinkOne counterpart to
// newEventLogSink. Wires Append's single-entry fast path directly to the
// per-key persister without the `[]EventEntry{e}` slice literal that the
// legacy slice contract required. AppendBatch continues to use the slice
// sink built by newEventLogSink — the two share persistOneEntry's
// marshal/refcount logic so the wire format and attachment-tracker
// behaviour are byte-identical between the two dispatch paths. (#410)
func newEventLogSinkOne(persisterSink persist.PersistSink, attachTracker *tracker.Tracker, keyhash string) cli.PersistSinkOne {
	return func(e cli.EventEntry, replayPhase bool) {
		persistOneEntry(persisterSink, attachTracker, keyhash, e, replayPhase)
	}
}
