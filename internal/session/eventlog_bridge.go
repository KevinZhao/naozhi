// Package session — eventlog_bridge.go
//
// Eventlog tiers: ring.EventLog.ring (in-memory, lossy), persist.Persister
// spool (durable, authoritative on restart, fed via this bridge's PersistSink),
// naozhilog.Source (replay from the spool) and history/merged.Source (composed
// read over the Claude-JSONL fallback). persist.Persister is fed via the
// per-key PersistSink callback and read back via persist.Recover.
//
// This bridge is the only place EventEntry → persist.Entry conversion lives.
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"

	"github.com/naozhi/naozhi/internal/attachment/tracker"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
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

// fillPersistGaps stores the fallback turns hidden behind persist_gap records
// of local (the event-log tail tier 1 read, reaching below floor, the oldest
// injected Time) for withGapFill. No-op unless the source is a merged.Source.
func (s *ManagedSession) fillPersistGaps(ctx context.Context, local []clievent.EventEntry, floor int64) {
	ms, ok := s.loadHistorySource().(*merged.Source)
	if !ok {
		return
	}
	if gf := ms.GapFill(ctx, local, floor, maxPersistedHistory); len(gf) > 0 {
		s.gapFillCell().turns.Store(&gf)
		slog.Info("filled persist gaps from fallback history", "key", s.key, "entries", len(gf))
	}
}

// gapFillCell is the gap fill a logical session's structs share.
type gapFillCell struct {
	turns atomic.Pointer[[]clievent.EventEntry]
}

// gapFillCell returns s's cell, allocating it on first use. CompareAndSwap
// makes a concurrent first use from tier 1 and from a successor's publish
// agree on one cell.
func (s *ManagedSession) gapFillCell() *gapFillCell {
	if c := s.gapFill.Load(); c != nil {
		return c
	}
	s.gapFill.CompareAndSwap(nil, new(gapFillCell))
	return s.gapFill.Load()
}

// loadGapFill returns the stored fill turns, nil when there are none.
func (s *ManagedSession) loadGapFill() []clievent.EventEntry {
	if c := s.gapFill.Load(); c != nil {
		if gf := c.turns.Load(); gf != nil {
			return *gf
		}
	}
	return nil
}

// withGapFill splices the gap-fill turns with Time in [page[0].Time, before)
// (before <= 0: no upper bound) into a memory page by Time, after page
// entries of equal Time. The page keeps its order and its first entry, the
// caller's next cursor, so consecutive pages split the turns between them.
// A turn whose UUID the page already holds (a later InjectHistory appended
// the transcript tail, #3028) is skipped.
func (s *ManagedSession) withGapFill(page []clievent.EventEntry, before int64) []clievent.EventEntry {
	gf := s.loadGapFill()
	if len(gf) == 0 || len(page) == 0 {
		return page
	}
	lo := page[0].Time
	var onPage map[string]struct{}
	var add []clievent.EventEntry
	for _, f := range gf {
		if f.Time < lo || (before > 0 && f.Time >= before) {
			continue
		}
		if onPage == nil {
			onPage = make(map[string]struct{}, len(page))
			for _, e := range page {
				onPage[e.UUID] = struct{}{}
			}
		}
		if _, dup := onPage[f.UUID]; dup && f.UUID != "" {
			continue
		}
		add = append(add, f)
	}
	if len(add) == 0 {
		return page
	}
	out := make([]clievent.EventEntry, 0, len(page)+len(add))
	j := 0
	for _, e := range page {
		for j < len(add) && add[j].Time < e.Time {
			out = append(out, add[j])
			j++
		}
		out = append(out, e)
	}
	return append(out, add[j:]...)
}

// encodeEntry marshals one EventEntry for the persist tier. HTML escaping is
// disabled (the persist tier reads back via json.Unmarshal, which accepts
// unescaped HTML) and the Encoder's trailing '\n' is stripped, so the bytes
// match the on-disk format. A failure is logged and reported as ok=false.
func encodeEntry(e clievent.EventEntry) ([]byte, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		slog.Warn("eventlog bridge: marshal entry failed",
			"uuid", e.UUID, "type", e.Type, "err", err)
		return nil, false
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), true
}

// newEventLogSink translates a per-key persist.PersistSink (persist.Entry
// batches) into the ring.PersistSink contract (clievent.EventEntry batches);
// neither cli nor persist imports the other, so the conversion lives only here.
//
// Ordering contract (RFC §3.2.2 / attachment-refcount §3.2): this sink MUST be
// installed on ring.EventLog.SetPersistSink AFTER any pre-hook InjectHistory
// calls complete; completeSpawn is the sole production caller responsible.
// attachTracker is optional: non-replay entries with ImagePaths bump the
// attachment refcount. A marshal failure on one EventEntry does NOT abort the
// batch — the entry is logged and skipped (best-effort persist, never block).
func newEventLogSink(persisterSink persist.PersistSink, attachTracker *tracker.Tracker, keyhash string) ring.PersistSink {
	return func(entries []clievent.EventEntry, replayPhase bool) {
		if len(entries) == 0 {
			return
		}
		out := make([]persist.Entry, 0, len(entries))
		for _, e := range entries {
			raw, ok := encodeEntry(e)
			if !ok {
				continue
			}
			out = append(out, persist.Entry{JSON: raw, TimeMS: e.Time})

			// Refcount bump for every attachment path the entry carries.
			// Replay batches are excluded: replay restores from the persist
			// tier, not a fresh user reference; bumping would reset
			// LastReferencedAt and defeat refTTL expiry.
			if !replayPhase && attachTracker != nil && keyhash != "" && len(e.ImagePaths) > 0 {
				attachTracker.OnPersistedEntry(keyhash, e.ImagePaths, e.Time)
			}
		}
		if len(out) > 0 {
			persisterSink(out, replayPhase)
		}
	}
}

// newEventLogSinkOne is the ring.PersistSinkOne counterpart to newEventLogSink,
// wiring Append's single-entry fast path to the per-key persister. It shares
// newEventLogSink so the wire format and attachment-tracker behaviour are
// identical (#410).
func newEventLogSinkOne(persisterSink persist.PersistSink, attachTracker *tracker.Tracker, keyhash string) ring.PersistSinkOne {
	sink := newEventLogSink(persisterSink, attachTracker, keyhash)
	return func(e clievent.EventEntry, replayPhase bool) {
		sink([]clievent.EventEntry{e}, replayPhase)
	}
}
