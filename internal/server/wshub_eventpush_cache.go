// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     rate-limit/cache block (historyMarshalCache only)
//	READS:      none beyond historyMarshalCache itself; pure helper file
package server

import (
	"sync"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// marshalCacheEntry holds the most recent marshaled "history" payload for
// a session key plus the fingerprint that produced it.
type marshalCacheEntry struct {
	mu              sync.Mutex
	lastTime        int64
	latestEntryTime int64
	count           int
	firstUUID       string
	lastUUID        string
	data            []byte
}

// marshalCacheEntryPool recycles candidates that lost the LoadOrStore race on
// a cold-miss fan-out (#1397). Only never-stored entries are returned to the
// pool — entries visible to readers are never reused, since a concurrent
// slot() may still hold a pointer to them.
var marshalCacheEntryPool = sync.Pool{
	New: func() any { return &marshalCacheEntry{} },
}

// historyMarshalCache is the per-session marshal coalescer: when N dashboard
// tabs subscribe to one session, a notify wave wakes N pushLoops that would
// each marshal the identical (key, entries-tail) payload. One slot per key
// holds the fingerprint (lastTime, latestEntryTime, count, firstUUID,
// lastUUID) plus the bytes. The UUID pair distinguishes two DIFFERENT
// same-millisecond tails (#2432); entries are a chronological tail of one
// append-only log, so equal (first, last) UUIDs + count imply an identical
// slice. Entries live in a sync.Map so cache hits do not serialise behind a
// global mutex; the per-key e.mu serialises marshal-once + fingerprint update
// (#1131). Slots are dropped when the last subscriber leaves; Shutdown resets.
type historyMarshalCache struct {
	entries sync.Map // map[string]*marshalCacheEntry
}

func newHistoryMarshalCache() *historyMarshalCache {
	return &historyMarshalCache{}
}

// slot returns (creating if needed) the per-key cache entry. Caller MUST
// take entry.mu before reading or mutating its fingerprint / data fields.
// Cold misses take the candidate from marshalCacheEntryPool; losers of the
// LoadOrStore race reset and return it.
func (c *historyMarshalCache) slot(key string) *marshalCacheEntry {
	if v, ok := c.entries.Load(key); ok {
		return v.(*marshalCacheEntry)
	}
	candidate := marshalCacheEntryPool.Get().(*marshalCacheEntry)
	e, loaded := c.entries.LoadOrStore(key, candidate)
	if loaded {
		// Lost the race; reset and recycle the unused candidate.
		candidate.lastTime = 0
		candidate.latestEntryTime = 0
		candidate.count = 0
		candidate.firstUUID = ""
		candidate.lastUUID = ""
		candidate.data = nil
		marshalCacheEntryPool.Put(candidate)
	}
	return e.(*marshalCacheEntry)
}

// getOrMarshal returns the marshaled bytes for the given (key, entries) tail.
// On a fingerprint hit the cached bytes are returned and `marshal` is NOT
// called. On miss `marshal` is invoked exactly once under the per-key mutex
// and its result is cached for the rest of the fan-out wave. Returns
// (data, fromCache, err).
//
// The cached []byte is safe to hand directly to wsClient.SendRaw: SendRaw only
// enqueues the slice and writePump reads it; nothing on the WS path mutates it.
func (c *historyMarshalCache) getOrMarshal(
	key string,
	lastTime int64,
	entries []clievent.EventEntry,
	marshal func() ([]byte, error),
) (data []byte, fromCache bool, err error) {
	if len(entries) == 0 {
		// No fingerprint for an empty tail; skip the cache (callers already
		// short-circuit this, the guard keeps the helper honest).
		data, err = marshal()
		return data, false, err
	}
	latest := entries[len(entries)-1].Time
	count := len(entries)
	firstUUID := entries[0].UUID
	lastUUID := entries[len(entries)-1].UUID

	e := c.slot(key)
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.data != nil &&
		e.lastTime == lastTime &&
		e.latestEntryTime == latest &&
		e.count == count &&
		e.firstUUID == firstUUID &&
		e.lastUUID == lastUUID {
		return e.data, true, nil
	}

	data, err = marshal()
	if err != nil {
		return nil, false, err
	}
	e.lastTime = lastTime
	e.latestEntryTime = latest
	e.count = count
	e.firstUUID = firstUUID
	e.lastUUID = lastUUID
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
