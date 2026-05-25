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

// historyMarshalCache is the per-session marshal coalescer.
type historyMarshalCache struct {
	mu      sync.Mutex
	entries map[string]*marshalCacheEntry
}

func newHistoryMarshalCache() *historyMarshalCache {
	return &historyMarshalCache{entries: make(map[string]*marshalCacheEntry)}
}

// slot returns (creating if needed) the per-key cache entry. Caller MUST
// take entry.mu before reading or mutating its fingerprint / data fields.
func (c *historyMarshalCache) slot(key string) *marshalCacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &marshalCacheEntry{}
		c.entries[key] = e
	}
	return e
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
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// reset clears the entire cache. Called by Hub.Shutdown so the map and any
// large cached payloads become collectable promptly.
func (c *historyMarshalCache) reset() {
	c.mu.Lock()
	c.entries = make(map[string]*marshalCacheEntry)
	c.mu.Unlock()
}
