// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     rate-limit/cache block (historyMarshalCache only)
//	READS:      none beyond historyMarshalCache itself; pure helper file
//
// 此文件实装 historyMarshalCache 的 sync.Map 抽象——单字段块持有，无
// cross-block dependency。Phase 4b rule 3b 对账时是最简单的一类。
package server

import (
	"sync"

	"github.com/naozhi/naozhi/internal/cli"
)

// File: wshub_eventpush_cache.go
//
// historyMarshalCache coalesces the per-subscriber JSON marshal in
// eventPushLoop. R214-PERF-4: when N dashboard tabs are subscribed to the
// same session, an EventLog notify wakes all N pushLoops and each one
// independently called marshalPooled on what is, in steady state, the
// identical (key, entries-tail) payload. With 5+ tabs the marshal CPU
// dominated the WS broadcast cost on a single hot session.
//
// Design: single-slot cache per session key. Entry holds a fingerprint of
// (lastTime, latestEntryTime, count) plus the marshaled bytes. eventPushLoop
// asks the cache via getOrMarshal; on hit the bytes are returned without a
// second marshalPooled. On miss (or fingerprint mismatch from a slow
// subscriber lagging behind the head) the caller marshals once and stores.
//
// Bounded by # concurrently subscribed session keys; entries dropped when
// the last subscriber for the key unsubscribes (Hub.dropHistoryMarshalCache).
// Hub.Shutdown clears the whole map. Memory per entry ≤ 1 capHistoryBatch
// payload (~50 entries × ~2 KB ≈ 100 KB worst case; typical ~5 KB).
//
// Concurrency: getOrMarshal takes the per-key mutex for the cache slot only
// during fingerprint check and update. The marshal itself runs under that
// mutex so a single goroutine produces the bytes that the rest of the
// fan-out reuses; without this, two simultaneous misses could race and
// each pay the marshal cost. The mutex is per-key, not global, so unrelated
// sessions never contend.

// marshalCacheEntry holds the most recent marshaled "history" payload for
// a session key plus the fingerprint that produced it.
type marshalCacheEntry struct {
	mu              sync.Mutex
	lastTime        int64
	latestEntryTime int64
	count           int
	data            []byte
}

// marshalCacheEntryPool recycles loser allocations on the cold-miss path.
// R040034-GO-11 (#1397): when a fan-out wave hits a missing key with N
// goroutines, only one of them can win the LoadOrStore; the rest were
// previously throwing their freshly allocated *marshalCacheEntry onto the
// GC. The pool lets losers return their entry for the next cold miss.
// Note: only entries that were never stored (lost the LoadOrStore race)
// are returned to the pool — entries that became visible to readers are
// never reused, since a concurrent slot() may still hold a pointer to them.
var marshalCacheEntryPool = sync.Pool{
	New: func() any { return &marshalCacheEntry{} },
}

// historyMarshalCache is the per-session marshal coalescer.
//
// R250-PERF-28 (#1131): the entries map lives in a sync.Map so the
// hot fan-out path (one notify wave wakes N pushLoops on the same
// session) does not serialise every cache-hit subscriber behind a
// single top-level mutex. The per-key *marshalCacheEntry is the
// real synchronisation unit — its embedded e.mu still serialises
// the marshal-once / fingerprint-update step inside getOrMarshal,
// while sync.Map handles the map-insertion race for new keys
// without forcing readers to wait. Pre-existing reset() / drop()
// continue to share the same sync.Map.
type historyMarshalCache struct {
	entries sync.Map // map[string]*marshalCacheEntry
}

func newHistoryMarshalCache() *historyMarshalCache {
	return &historyMarshalCache{}
}

// slot returns (creating if needed) the per-key cache entry. Caller MUST
// take entry.mu before reading or mutating its fingerprint / data fields.
//
// R250-PERF-28 (#1131): sync.Map's LoadOrStore lets repeated cache hits
// for the same key skip the lock-on-read penalty the prior plain-map +
// top-level mutex paid.
//
// R040034-GO-11 (#1397): on a cold-miss fan-out (N goroutines racing on
// the same missing key) we now grab the candidate from marshalCacheEntryPool
// instead of `new`-ing every time. Losers (LoadOrStore returned an existing
// entry) reset and return their candidate to the pool for the next cold
// miss. This keeps the steady-state alloc rate at ~1 entry per distinct
// session key rather than ~N per cold-miss wave.
func (c *historyMarshalCache) slot(key string) *marshalCacheEntry {
	if v, ok := c.entries.Load(key); ok {
		return v.(*marshalCacheEntry)
	}
	candidate := marshalCacheEntryPool.Get().(*marshalCacheEntry)
	e, loaded := c.entries.LoadOrStore(key, candidate)
	if loaded {
		// Lost the race; recycle the unused candidate. Reset is cheap and
		// guards against accidental reuse of stale fingerprint/data fields
		// if a future code path leaks state into a freshly-Got entry.
		candidate.lastTime = 0
		candidate.latestEntryTime = 0
		candidate.count = 0
		candidate.data = nil
		marshalCacheEntryPool.Put(candidate)
	}
	return e.(*marshalCacheEntry)
}

// getOrMarshal returns the marshaled bytes for the given (key, entries) tail.
// On a fingerprint hit the cached bytes are returned and `marshal` is NOT
// called. On miss `marshal` is invoked exactly once under the per-key mutex
// and its result is cached for subsequent fan-out members of the same notify
// wave. Returns (data, fromCache, err) — fromCache is true when the bytes
// originated from a previous getOrMarshal call.
//
// The cached []byte is safe to hand directly to wsClient.SendRaw: SendRaw
// only enqueues the slice into the per-client send channel and the writePump
// reads it; nothing on the WS path mutates the buffer. Concurrent SendRaw
// callers across different clients all read the same slice header.
func (c *historyMarshalCache) getOrMarshal(
	key string,
	lastTime int64,
	entries []cli.EventEntry,
	marshal func() ([]byte, error),
) (data []byte, fromCache bool, err error) {
	if len(entries) == 0 {
		// No fingerprint can be computed from an empty tail; skip the cache
		// entirely. eventPushLoop already short-circuits this case before
		// calling getOrMarshal, but the guard keeps the helper honest if a
		// future caller forgets.
		data, err = marshal()
		return data, false, err
	}
	latest := entries[len(entries)-1].Time
	count := len(entries)

	e := c.slot(key)
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.data != nil &&
		e.lastTime == lastTime &&
		e.latestEntryTime == latest &&
		e.count == count {
		return e.data, true, nil
	}

	data, err = marshal()
	if err != nil {
		return nil, false, err
	}
	e.lastTime = lastTime
	e.latestEntryTime = latest
	e.count = count
	e.data = data
	return data, false, nil
}

// drop releases the cache slot for the given key. Called when the last
// subscriber for the key unsubscribes (best-effort: a concurrent re-subscribe
// will simply repopulate the slot on its first miss).
func (c *historyMarshalCache) drop(key string) {
	c.entries.Delete(key)
}

// reset clears the entire cache. Called by Hub.Shutdown so the map and any
// large cached payloads become collectable promptly.
//
// sync.Map's Range is documented as a snapshot-style iteration that may
// observe concurrent stores — for shutdown the goal is "drop everything we
// can see right now" so a Delete inside Range is safe and matches Go's
// own examples.
func (c *historyMarshalCache) reset() {
	c.entries.Range(func(k, _ any) bool {
		c.entries.Delete(k)
		return true
	})
}
