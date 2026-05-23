// Package session — eventlog_bridge.go
//
// Despite the file name "bridge", this file is the de facto fan-out hub
// of the event-log persistence pipeline: a single PersistSink installed
// per ManagedSession that routes every cli.EventEntry batch into BOTH
// the persist tier (disk) AND the attachment refcount tracker, with a
// pooled JSON encoder amortising the steady-state allocations.
//
// "Bridge" survives in the name for historical reasons — the original
// (R215-PERF-P1-1 era) implementation only translated cli.EventEntry
// batches into persist.Entry shape (one input, one output), making
// "bridge" accurate. The attachment-tracker hand-off (added in the
// refcount work, see attachment/tracker §3.2) and the encoder pool
// turned this file into a multi-sink hub. R234-ARCH-25 proposes a
// rename to event_pipeline.go + an explicit EventPipeline + []EventSink
// abstraction; that is deferred behind the broader RouterView refactor
// (R234-ARCH-3) so the fan-out shape can be expressed in the same
// session.api package the rest of the unification will land under.
//
// Read order for newcomers: newEventLogSink (entry point) → the
// per-batch loop that feeds attachment tracker first, then persists →
// the bridgeEncPool helpers at the bottom of the file.
package session

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/naozhi/naozhi/internal/attachment/tracker"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

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
		if len(entries) == 1 {
			eb := bridgeEncPool.Get().(*bridgeEncBuf)
			eb.buf.Reset()
			e := entries[0]
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
			// Copy out of the pooled buffer so caller can hold the
			// bytes past Put. PersistSink contract permits sink to
			// retain entries.
			buf := make([]byte, len(raw))
			copy(buf, raw)
			if eb.buf.Cap() <= bridgeEncMaxCap {
				bridgeEncPool.Put(eb)
			}
			var stackArr [1]persist.Entry
			out := append(stackArr[:0], persist.Entry{JSON: buf, TimeMS: e.Time})
			// Refcount bump — same guard as multi-entry path below.
			if !replayPhase && attachTracker != nil && keyhash != "" && len(e.ImagePaths) > 0 {
				attachTracker.OnPersistedEntry(keyhash, e.ImagePaths, e.Time)
			}
			persisterSink(out, replayPhase)
			return
		}

		out := make([]persist.Entry, 0, len(entries))
		eb := bridgeEncPool.Get().(*bridgeEncBuf)
		defer func() {
			if eb.buf.Cap() <= bridgeEncMaxCap {
				bridgeEncPool.Put(eb)
			}
		}()
		for _, e := range entries {
			eb.buf.Reset()
			if err := eb.enc.Encode(e); err != nil {
				slog.Warn("eventlog bridge: marshal entry failed",
					"uuid", e.UUID, "type", e.Type, "err", err)
				continue
			}
			raw := eb.buf.Bytes()
			if n := len(raw); n > 0 && raw[n-1] == '\n' {
				raw = raw[:n-1]
			}
			// Copy out of the pooled buffer so caller can hold the
			// bytes past Put. PersistSink contract permits sink to
			// retain entries.
			buf := make([]byte, len(raw))
			copy(buf, raw)
			out = append(out, persist.Entry{JSON: buf, TimeMS: e.Time})

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
		if len(out) == 0 {
			return
		}
		persisterSink(out, replayPhase)
	}
}
