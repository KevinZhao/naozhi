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
