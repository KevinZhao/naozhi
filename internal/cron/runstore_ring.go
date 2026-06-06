package cron

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// recentCacheEntry is the cached newest-first snapshot for one job.
//
// R242-GO-8 / R235-PERF-3 / R233-PERF-2: storage is a fixed-capacity ring
// buffer (cap = runStore.keepCount, typically 200). cacheHeadPush is the
// hot path — every Append calls it, so pre-historical implementations
// that did `append + copy` shifted up to keepCount-1 entries per push
// (O(N) per Append). The ring lets us land each push in O(1) by
// rotating `head` backwards instead of moving data.
//
// Logical view: newest-first slice of length `count`, where index 0 is
// the newest entry. Physically: `ring[head]` is the newest, `ring[(head
// + count - 1) % cap(ring)]` is the oldest. ringRead / ringSnapshot
// translate logical → physical for all consumers.
type recentCacheEntry struct {
	mu sync.Mutex
	// ring is the fixed-capacity backing array. cap(ring) == runStore.keepCount
	// after the first warm pass; nil before warm.
	ring []CronRunSummary
	// head is the ring index of the newest entry. Undefined when count == 0.
	head int
	// count is the populated length (0 ≤ count ≤ cap(ring)).
	count int
	warm  bool // false until first warm() pass; List/Recent will lazy-warm
	// appendsSinceTrim counts Append calls since the last full trimJobLocked
	// pass. Used by skipAppendTrim to batch ReadDir-driven trims when the
	// cache shows we're well under keepCount. Reset to 0 by Append after
	// calling trimJobLocked. R232-PERF-8.
	appendsSinceTrim int
	// runIDs is the set of RunIDs currently present in the ring. cacheHeadPush
	// consults it for an O(1) duplicate check instead of an O(count) linear
	// ring scan on every Append (#1517). It is maintained in lockstep with the
	// ring under entry.mu: ringSeed rebuilds it, ringPushHead inserts the new
	// RunID (and deletes the evicted oldest when the ring is full). The dedup
	// is needed because a warmCache ringSeed can interleave between an Append's
	// WriteFileAtomic and its matching cacheHeadPush (R20260527122801-PERF-4 /
	// #1335), seeding a RunID ahead of its own late push. nil until the first
	// ringSeed allocates it.
	runIDs map[string]struct{}
	// capZeroWarned is set on the first cap=0 self-heal observation so the
	// warn fires exactly once per entry rather than once per process.
	// R20260602-CR-4: per-entry instead of a package-level sync.Once so
	// parallel tests each get their own warn gate and cannot silence one
	// another.
	capZeroWarned atomic.Bool
}

// warnRingCapZero fires a slog.Warn for the cap=0 self-heal path. It is
// rate-limited per recentCacheEntry via e.capZeroWarned (atomic CAS) so
// each entry warns at most once, independent of other entries. This is
// R20260602-CR-4: the previous package-level sync.Once silenced warnings
// for all subsequent entries once the first triggered, hiding the
// regression in parallel-test and multi-job scenarios.
func warnRingCapZero(e *recentCacheEntry, site string) {
	if e.capZeroWarned.CompareAndSwap(false, true) {
		slog.Warn("cron runstore: recentCache ring cap=0 on read; self-healing to empty (ringSeed bypass regression?)",
			"site", site)
	}
}

// ringRead returns the i-th newest entry (0 = newest). Caller holds entry.mu
// and must ensure 0 ≤ i < entry.count.
func (e *recentCacheEntry) ringRead(i int) CronRunSummary {
	// R247-GO-4: defensive against cap(ring)==0 with count>0 — same self-heal
	// philosophy as cacheHeadPush's `cap(entry.ring) != s.keepCount` reseed.
	// Avoids integer divide-by-zero panic on a regression path that bypasses
	// ringSeed (e.g. an unwarmed entry mutated by future code). [BREAKING-LOCAL]
	if cap(e.ring) == 0 {
		// R249-ARCH-13 (#979): warn once so the silent self-heal is auditable.
		warnRingCapZero(e, "ringRead")
		return CronRunSummary{}
	}
	return e.ring[(e.head+i)%cap(e.ring)]
}

// ringSnapshot returns a fresh newest-first slice of up to limit entries.
// Caller holds entry.mu. limit ≤ 0 or limit > count returns count entries.
func (e *recentCacheEntry) ringSnapshot(limit int) []CronRunSummary {
	// R247-GO-4: see ringRead — guard cap=0 + count>0 regression and the
	// degenerate count==0 fast path (no allocation needed). [BREAKING-LOCAL]
	if cap(e.ring) == 0 || e.count == 0 {
		// R249-ARCH-13 (#979): warn once when cap=0 *despite* a populated
		// count — that is the bypass regression. The count==0 case is the
		// benign empty-cache fast path and stays silent.
		if cap(e.ring) == 0 && e.count > 0 {
			warnRingCapZero(e, "ringSnapshot")
		}
		return nil
	}
	if limit <= 0 || limit > e.count {
		limit = e.count
	}
	out := make([]CronRunSummary, limit)
	c := cap(e.ring)
	// Two contiguous segments: head..min(head+limit, c) and 0..wrap.
	first := limit
	if e.head+first > c {
		first = c - e.head
	}
	copy(out, e.ring[e.head:e.head+first])
	if first < limit {
		copy(out[first:], e.ring[:limit-first])
	}
	return out
}

// ringPushHead inserts summary at the newest end in O(1). Caller holds
// entry.mu and entry.ring is allocated (cap > 0).
func (e *recentCacheEntry) ringPushHead(summary CronRunSummary) {
	c := cap(e.ring)
	// Move head one slot backwards, wrapping around. After this, the
	// freshly written summary is the newest entry.
	e.head = (e.head - 1 + c) % c
	if e.count == 0 {
		// First push into an empty ring: ensure ring length covers head.
		// We keep len(ring) == cap(ring) so plain index assignment works
		// regardless of count.
		e.ring = e.ring[:c]
	}
	if e.count == c {
		// Ring full: the slot we're about to overwrite holds the oldest
		// entry, which is being evicted. Drop it from the RunID set so the
		// O(1) dedup index (#1517) stays in lockstep with the ring.
		if e.runIDs != nil {
			delete(e.runIDs, e.ring[e.head].RunID)
		}
	}
	e.ring[e.head] = summary
	if e.count < c {
		e.count++
	}
	if e.runIDs != nil {
		e.runIDs[summary.RunID] = struct{}{}
	}
}

// ringSeed populates the ring from a newest-first source slice. Caller
// holds entry.mu. Used by warmCache and cacheTrimAfterDisk to install a
// fresh snapshot. cap is set to keepCount so future pushes never realloc.
func (e *recentCacheEntry) ringSeed(rows []CronRunSummary, keepCount int) {
	if cap(e.ring) != keepCount {
		e.ring = make([]CronRunSummary, keepCount)
	} else {
		e.ring = e.ring[:keepCount]
		// Zero out trailing slots so old entries beyond count don't pin
		// strings / sub-slices (avoid leaking RAM through a smaller seed).
		// R20260606-PERF-6: use clear() (Go 1.21+) instead of a per-element
		// loop — a single memclr over the tail sub-slice rather than N
		// separate GC-visible assignments.
		if len(rows) < keepCount {
			clear(e.ring[len(rows):])
		}
	}
	n := len(rows)
	if n > keepCount {
		n = keepCount
	}
	copy(e.ring[:n], rows[:n])
	e.head = 0
	e.count = n
	// Rebuild the RunID dedup index (#1517) from the freshly-seeded rows so
	// the next cacheHeadPush can do an O(1) membership test against this
	// snapshot instead of an O(count) linear scan.
	if e.runIDs == nil {
		e.runIDs = make(map[string]struct{}, n)
	} else {
		clear(e.runIDs)
	}
	for i := 0; i < n; i++ {
		e.runIDs[e.ring[i].RunID] = struct{}{}
	}
}
