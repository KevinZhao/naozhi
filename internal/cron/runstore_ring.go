package cron

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// recentCacheEntry is the cached newest-first snapshot for one job, stored in
// a fixed-capacity ring (cap = runStore.keepCount) so cacheHeadPush — called
// on every Append — is O(1) by rotating head instead of shifting data.
//
// Logical view: newest-first slice of length count, index 0 newest.
// Physically ring[head] is the newest and ring[(head+count-1)%cap] the oldest;
// ringRead / ringSnapshot translate logical → physical.
type recentCacheEntry struct {
	mu sync.RWMutex
	// ring is the fixed-capacity backing array. cap(ring) == runStore.keepCount
	// after the first warm pass; nil before warm.
	ring []CronRunSummary
	// head is the ring index of the newest entry. Undefined when count == 0.
	head int
	// count is the populated length (0 ≤ count ≤ cap(ring)).
	count int
	warm  bool // false until first warm() pass; List/Recent will lazy-warm
	// runIDs is the set of RunIDs currently in the ring, giving cacheHeadPush an
	// O(1) dedup check (#1517). Maintained in lockstep with the ring under
	// entry.mu by ringSeed / ringPushHead; nil until the first ringSeed. Needed
	// because a warmCache ringSeed can interleave between an Append's
	// WriteFileAtomic and its cacheHeadPush, seeding a RunID ahead of its push.
	runIDs map[string]struct{}
	// capZeroWarned gates the cap=0 self-heal warning to once per entry (a
	// package-level once would let one entry silence all others).
	capZeroWarned atomic.Bool
}

// warnRingCapZero fires the cap=0 self-heal slog.Warn at most once per
// recentCacheEntry (atomic CAS on e.capZeroWarned).
func warnRingCapZero(e *recentCacheEntry, site string) {
	if e.capZeroWarned.CompareAndSwap(false, true) {
		slog.Warn("cron runstore: recentCache ring cap=0 on read; self-healing to empty (ringSeed bypass regression?)",
			"site", site)
	}
}

// ringRead returns the i-th newest entry (0 = newest). Caller holds entry.mu
// and must ensure 0 ≤ i < entry.count.
func (e *recentCacheEntry) ringRead(i int) CronRunSummary {
	// Defensive against cap(ring)==0 with count>0 (a path that bypassed ringSeed):
	// avoids a divide-by-zero panic, mirroring cacheHeadPush's reseed self-heal.
	if cap(e.ring) == 0 {
		// Warn once so the silent self-heal is auditable (#979).
		warnRingCapZero(e, "ringRead")
		return CronRunSummary{}
	}
	return e.ring[(e.head+i)%cap(e.ring)]
}

// ringSnapshot returns a fresh newest-first slice of up to limit entries.
// Caller holds entry.mu. limit ≤ 0 or limit > count returns count entries.
func (e *recentCacheEntry) ringSnapshot(limit int) []CronRunSummary {
	// Guard cap=0 + count>0 regression, plus the count==0 no-alloc fast path.
	if cap(e.ring) == 0 || e.count == 0 {
		// Only cap=0 despite a populated count is the bypass regression; count==0 stays silent.
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
	e.head = (e.head - 1 + c) % c
	if e.count == 0 {
		// Keep len(ring) == cap(ring) so plain index assignment works regardless of count.
		e.ring = e.ring[:c]
	}
	if e.count == c {
		// Ring full: the slot being overwritten holds the evicted oldest entry;
		// drop it from the dedup index (#1517).
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
		// Zero trailing slots so entries beyond count don't pin strings / sub-slices.
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
	// Rebuild the RunID dedup index from the seeded rows (#1517).
	if e.runIDs == nil {
		e.runIDs = make(map[string]struct{}, n)
	} else {
		clear(e.runIDs)
	}
	for i := 0; i < n; i++ {
		e.runIDs[e.ring[i].RunID] = struct{}{}
	}
}
