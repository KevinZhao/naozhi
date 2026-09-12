package persist

import (
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// flush.go — when writers get flushed and closed: the periodic tick, candidate
// selection, and the flush-everything path. Extracted from persister.go (J9 of
// #2548).

func (p *Persister) flushAllLocked() error {
	// Only dirty writers form the fan-out payload (#1128).
	if len(p.writers) == 0 {
		return nil
	}
	dirtyKeys := p.scratch.allKeys[:0]
	dirtyWs := p.scratch.allWs[:0]
	for k, w := range p.writers {
		if !w.dirty {
			continue
		}
		dirtyKeys = append(dirtyKeys, k)
		dirtyWs = append(dirtyWs, w)
	}
	p.scratch.allKeys = dirtyKeys
	p.scratch.allWs = dirtyWs
	if len(dirtyWs) == 0 {
		return nil
	}
	// Parallel flush; same independence argument as shutdownAll. firstErr
	// is recorded under p.scratch.allErrMu (a field to avoid a heap escape).
	var firstErr error
	p.parallelFsync(dirtyKeys, dirtyWs, func(k string, w *perKeyWriter) {
		if err := w.flush(p); err != nil {
			p.scratch.allErrMu.Lock()
			if firstErr == nil {
				firstErr = fmt.Errorf("flush %s: %w", k, err)
			}
			p.scratch.allErrMu.Unlock()
		}
	})
	// Drop writer pointers so closed writers are not pinned until the next Flush.
	clear(dirtyWs)
	return firstErr
}

// flushCandidate gives tickFlush a stable oldest-first flush order:
// sorting by firstDirtyAt bounds worst-case flush latency to N tick
// intervals regardless of map-iteration randomness.
// flushScratch is the set of buffers the run goroutine reuses across flushes.
// It is its own type so the Persister's fields divide structurally rather than by
// a comment repeated seven times: everything outside scratch may be touched by
// other goroutines, everything inside is run-goroutine only, which is what lets
// these be reused without synchronisation.
type flushScratch struct {
	// cands is reused across tickFlush calls.
	cands []flushCandidate
	// lastCount is how many cands slots the last tick populated, so the next tick
	// clears only those (#1406).
	lastCount int
	// tickKeys / tickWs are the (key, writer) pair tickFlush hands to
	// parallelFsync (#1569).
	tickKeys []string
	tickWs   []*perKeyWriter
	// allKeys / allWs are flushAllLocked's equivalent, kept separate from the
	// tickFlush pair so the two paths never alias.
	allKeys []string
	allWs   []*perKeyWriter
	// allErrMu serialises firstErr in flushAllLocked's parallelFsync closure; a
	// field rather than a local to avoid the heap escape from capturing a local
	// mutex's address.
	allErrMu sync.Mutex
}

type flushCandidate struct {
	key string
	w   *perKeyWriter
}

func (p *Persister) tickFlush() {
	// Idle deployments hit this every FlushInterval/2; skip the walk + Clock() (#1110).
	if len(p.writers) == 0 {
		return
	}
	now := p.opts.Clock()
	cands := p.collectFlushCandidates(now)
	if len(cands) == 0 {
		return
	}
	// Fan the per-candidate flush (fsync log + idx) over the bounded pool so
	// one slow fsync does not stall every other dirty writer for the tick;
	// candidates are distinct writers, so fn touches no shared state (#1569).
	keys := p.scratch.tickKeys[:0]
	ws := p.scratch.tickWs[:0]
	for _, c := range cands {
		keys = append(keys, c.key)
		ws = append(ws, c.w)
	}
	p.scratch.tickKeys = keys
	p.scratch.tickWs = ws
	p.parallelFsync(keys, ws, func(k string, w *perKeyWriter) {
		if err := w.flush(p); err != nil {
			slog.Warn("event log persist: debounced flush failed",
				"key", k, "err", err)
		}
	})
	// Drop writer pointers so closed writers are not pinned until the next tick.
	clear(ws)
}

// collectFlushCandidates returns writers whose firstDirtyAt has aged past
// the adaptive flush interval, sorted oldest-first. Reuses p.scratch.cands;
// safe because tickFlush is the sole consumer and runs on the run goroutine.
func (p *Persister) collectFlushCandidates(now time.Time) []flushCandidate {
	// Lengthen the window as the writer-set grows to damp the sustained
	// fsync rate (50 sessions × 100 ms tick = 100 fsync/s). Computed once
	// per tick so the bucket is stable across the iteration.
	threshold := effectiveFlushInterval(p.opts.FlushInterval, len(p.writers))
	// Clear only the slots the previous tick populated so closed writers
	// can be GC'd without memzeroing a burst-grown array every tick (#1406).
	if p.scratch.lastCount > 0 {
		n := p.scratch.lastCount
		if n > cap(p.scratch.cands) {
			n = cap(p.scratch.cands)
		}
		clear(p.scratch.cands[:n])
	}
	cands := p.scratch.cands[:0]
	for k, w := range p.writers {
		if !w.dirty {
			continue
		}
		if now.Sub(w.firstDirtyAt) < threshold {
			continue
		}
		cands = append(cands, flushCandidate{key: k, w: w})
	}
	if len(cands) > 1 {
		slices.SortFunc(cands, func(a, b flushCandidate) int {
			return a.w.firstDirtyAt.Compare(b.w.firstDirtyAt)
		})
	}
	// Keep the grown backing array; remember how many slots to clear next tick.
	p.scratch.cands = cands
	p.scratch.lastCount = len(cands)
	return cands
}

// effectiveFlushInterval returns the adaptive debounce window used by
// tickFlush. Fixed buckets (≤16 → 1×, ≤64 → 1.5×, ≤256 → 2×, else 4×)
// avoid oscillation as the session count drifts across a boundary. At 50
// writers the window goes 200 → 300 ms, cutting fsync rate ~100/s → ~67/s
// without changing durability (flush still happens, just batching more).
func effectiveFlushInterval(base time.Duration, writerCount int) time.Duration {
	switch {
	case writerCount <= 16:
		return base
	case writerCount <= 64:
		return base + base/2 // 1.5×
	case writerCount <= 256:
		return base * 2
	default:
		return base * 4
	}
}

func (p *Persister) tickIdleClose() {
	if len(p.writers) == 0 {
		return
	}
	now := p.opts.Clock()
	for k, w := range p.writers {
		if w.dirty {
			continue
		}
		if now.Sub(w.lastActivity) < p.opts.IdleCloseAfter {
			continue
		}
		if err := w.close(); err != nil {
			slog.Warn("event log persist: idle close failed",
				"key", k, "err", err)
		}
		delete(p.writers, k)
	}
}

// handleBatch is the hot path: find-or-open the writer, append every
// entry, mark dirty for debounce. It NEVER fsyncs — the debounce ticker
// owns fsync so a 500-entry batch does not cause 500 fsyncs. `now` is
// captured by the caller so one clock read also covers lastDrainNS.
