// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     rate-limit/cache block (historyMarshalCache for replay cache)
//	READS:      shared deps block (read-only after ctor) + subscriber block
//	            (clients for fanout) + lifecycle block (ctx for cancel)
package server

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// resubscribeMaxAttempts × resubscribeInterval (60s) is the wait budget for
// resubscribeEvents; it covers a `claude` CLI cold start (worst case 30-45s),
// after which the flap is permanent and the client's reconnect loop takes over.
// Split into attempts so the loop body (generation check, ctx / client-done
// fan-out) runs at a 5s heartbeat instead of blocking the whole window.
const (
	resubscribeMaxAttempts = 12
	resubscribeInterval    = 5 * time.Second
)

// maxHistoryPushEntries caps a single WS "history" push so a full-ring
// catch-up (500 entries, ~100 KB) cannot fan out to every connection at once.
// 50 matches the dashboard's paginated /api/sessions/events tail fetch;
// older entries stay reachable via `before=`.
const maxHistoryPushEntries = 50

func capHistoryBatch(entries []clievent.EventEntry) []clievent.EventEntry {
	if len(entries) <= maxHistoryPushEntries {
		return entries
	}
	return entries[len(entries)-maxHistoryPushEntries:]
}

// marshalHistoryFrame produces the WS "history" frame bytes for key + entries
// tail, coalescing the marshal across all eventPushLoop goroutines in
// lock-step on the same session. The per-key fingerprint (lastTime, latest
// Time, count, first/last UUID) forces a fresh marshal for out-of-lockstep
// subscribers. The returned []byte may be handed to wsClient.SendRaw from
// multiple goroutines: SendRaw enqueues the slice and writePump never mutates it.
func (h *Hub) marshalHistoryFrame(key string, lastTime int64, entries []clievent.EventEntry) ([]byte, error) {
	// Redact credential token shapes from Summary/Detail before the bytes reach
	// the browser: this is the single serialization choke point for backfill and
	// live push, and the dashboard persists frames to IndexedDB. Redaction runs
	// inside the marshal closures so a cache HIT does not re-scan already-redacted
	// entries (#1888); it never touches Time, so the fingerprint is unaffected.
	if h.historyMarshalCache == nil {
		// Hand-constructed test Hubs may skip the field; use the uncached path.
		return marshalPooled(wsproto.NewHistory(wsproto.History{Key: key, Events: redactEntrySecrets(entries)}))
	}
	// Single-subscriber fast path (#944): with one tab every notify advances
	// lastTime so the cache always misses; skip the sync.Map + mutex round-trip.
	// count != 1 (or counter unwired in tests) falls through to the cached path.
	if h.singleSubscriber(key) {
		return marshalPooled(wsproto.NewHistory(wsproto.History{Key: key, Events: redactEntrySecrets(entries)}))
	}
	data, _, err := h.historyMarshalCache.getOrMarshal(key, lastTime, entries, func() ([]byte, error) {
		return marshalPooled(wsproto.NewHistory(wsproto.History{Key: key, Events: redactEntrySecrets(entries)}))
	})
	return data, err
}

// singleSubscriber reports whether `key` has exactly one subscriber.
// Returns false for count 0 (teardown / never registered) so only the strict
// "single tab" case takes the fast path (#944). Reads the lock-free
// subscriberCountFast mirror rather than h.mu.RLock: the mirror is updated
// under h.mu by every subscriberCount mutation, so a stale verdict only
// changes whether this push uses the marshal cache, never the bytes (#1522).
// nil subscriberCount (hand-built test hubs) yields false.
func (h *Hub) singleSubscriber(key string) bool {
	if h.subscriberCount == nil {
		return false
	}
	v, ok := h.subscriberCountFast.Load(key)
	if !ok {
		return false
	}
	return v.(*atomic.Int32).Load() == 1
}

// eventPushLoop is the per-subscription pump that reads EventLog notifications
// and streams entries to the WS client. It owns exactly one clientWG slot for
// its lifetime (Add in completeSubscribe before go; Done in the deferred func).
//
// CLIENTWG CONTRACT: when resubscribeEvents swaps `sess` for a new process's
// session the loop keeps running in this goroutine, so there is NO extra
// Add(1): the tracked lifetime is the goroutine, and resubscribeEvents installs
// the new unsub into c.subscriptions[key] under h.mu so Shutdown sees it.
// Anyone splitting the resubscribe path into a new goroutine MUST Add(1) for it
// and Done from its own defer, or Shutdown's clientWG.Wait hangs / panics.
func (h *Hub) eventPushLoop(c *wsClient, key string, gen uint64, notify <-chan struct{}, sess *session.ManagedSession, csr *cli.SinceCursor) {
	defer func() {
		if r := recover(); r != nil {
			// Mirror readPump: counter first, cause at Error, stack at Debug (avoid
			// leaking internal paths to aggregated logs); tag with the key.
			serverMetrics.PanicRecovered()
			slog.Error("panic in ws eventPushLoop (recovered)",
				"key", key, "panic", fmt.Sprintf("%v", r))
			slog.Debug("panic in ws eventPushLoop: stack",
				"key", key, "stack", string(debug.Stack()))
			// Close the connection so readPump/writePump unregister and tear down
			// all subs; otherwise subscriptions[key]/subGen/subscriberCount linger
			// and maxSubscribersPerKey eventually traps this client.
			c.closeDone()
		}
	}()
	// One buffer per goroutine, reused across notify waves by
	// backfillSubscriberEvents (#1740); the drain never retains it.
	var evBuf []clievent.EventEntry
	for {
		select {
		case _, ok := <-notify:
			if !ok {
				ok, newSess := h.resubscribeEvents(c, key, gen, &notify)
				if !ok {
					return
				}
				// Rewind on session REPLACEMENT (/new, eviction + respawn): the new
				// log's timestamps can predate the old watermark (#2402). A
				// same-session process flap keeps its watermark.
				if newSess != sess {
					csr.Reset()
				}
				sess = newSess
				// Catch up unconditionally: resubscribeEvents may have consumed one
				// pending notification while probing newNotify, and in an idle
				// session the next Append could be seconds away (#744).
				alive, b := h.backfillSubscriberEvents(c, key, sess, csr, evBuf)
				evBuf = b
				if !alive {
					return
				}
				continue
			}
			alive, b := h.backfillSubscriberEvents(c, key, sess, csr, evBuf)
			evBuf = b
			if !alive {
				return
			}
		case <-c.done:
			return
		case <-h.ctx.Done():
			// Hub shutdown: exit even if the client is open and notify is stalled
			// (a half-open socket may never propagate conn.Close via readPump).
			return
		}
	}
}

// backfillSubscriberEvents drains new entries for sess through the caller's
// SinceCursor, marshals the batched "history" frame via the coalesced cache,
// and writes it to c. Returns (alive, buf) — the caller must exit when alive
// is false (the client closed mid-drain) and retain buf for the next wave.
//
// cli.SinceCursor (inclusive watermark query + UUID dedup at the trailing
// millisecond) is what keeps same-millisecond entries landing in a LATER
// notify wave from being dropped (#2402); redeliveries that reach the client
// are absorbed by the dashboard's UUID dedup. On marshal error the cursor is
// not advanced, so the same entries are retried on the next notify.
func (h *Hub) backfillSubscriberEvents(c *wsClient, key string, sess *session.ManagedSession, csr *cli.SinceCursor, buf []clievent.EventEntry) (bool, []clievent.EventEntry) {
	// buf[:0] lets both the dead-session and live-process paths reuse capacity
	// across notify waves (#1740); entries are consumed synchronously below and
	// never retained. QueryAfter re-admits the watermark millisecond; Filter
	// drops already-delivered UUIDs in place, so the backing array survives.
	entries := sess.EventEntriesSinceAppend(buf[:0], csr.QueryAfter())
	fetched := entries
	entries = csr.Filter(entries)
	if len(entries) == 0 {
		return true, fetched
	}
	select {
	case <-c.done:
		return false, fetched
	default:
	}
	// Marshal/advance from the capped tail view but return the full `fetched`
	// slice so its capacity is preserved (a tail slice shrinks cap every call).
	capped := capHistoryBatch(entries)
	// The marshal-cache fingerprint keys on the pre-advance watermark, so
	// lock-step tabs still coalesce onto one marshal.
	data, err := h.marshalHistoryFrame(key, csr.Watermark(), capped)
	if err != nil {
		return true, fetched
	}
	c.SendRaw(data)
	csr.Advance(capped)
	return true, fetched
}

// resubscribeEvents waits for a new process to be attached to the session and
// re-subscribes to its EventLog. Returns (ok, currentSession). ok is false if
// the client disconnects, the wait times out (resubscribeMaxAttempts ×
// resubscribeInterval = 60s), or a newer subscription has taken over this
// key (generation mismatch).
func (h *Hub) resubscribeEvents(c *wsClient, key string, gen uint64, notify *<-chan struct{}) (bool, *session.ManagedSession) {
	// Timer.Reset reuses one timer across iterations instead of a Ticker + its
	// goroutine; client flap can trigger N simultaneous calls.
	timer := time.NewTimer(resubscribeInterval)
	defer timer.Stop()

	for i := range resubscribeMaxAttempts {
		if i > 0 {
			timer.Reset(resubscribeInterval)
		}
		select {
		case <-c.done:
			return false, nil
		case <-h.ctx.Done():
			return false, nil
		case <-timer.C:
		}

		// Bail out if a newer subscription (handleSubscribe) has taken over.
		// The RLock is the visibility barrier for c.subGen[key], written by
		// handleSubscribe under h.mu.Lock; do not replace it with a lock-free read.
		h.mu.RLock()
		currentGen := c.subGen[key]
		h.mu.RUnlock()
		if currentGen != gen {
			return false, nil
		}

		// Re-check the router for the current session — spawnSession may have
		// created a new ManagedSession, replacing the old one in the map.
		currentSess := h.router.SessionFor(key)
		if currentSess == nil {
			continue
		}

		newNotify, unsub := currentSess.SubscribeEvents()
		// Check if the channel is immediately closed (process still nil).
		select {
		case _, ok := <-newNotify:
			if !ok {
				// Process still nil — clean up subscriber slot and keep waiting.
				unsub()
				continue
			}
			// Process is back and has events.
		default:
			// Channel is alive (not closed) — process is back.
		}

		// Update the subscription registration. Capture the old unsub under h.mu
		// but call it AFTER releasing the lock: lock order is h.mu → EventLog.subMu
		// (shutdown_lock_order_test.go), and calling oldUnsub under h.mu would
		// reintroduce a reverse-order hazard if it ever took more locks.
		h.mu.Lock()
		if c.subscriptions == nil {
			// Client was removed during Shutdown.
			h.mu.Unlock()
			unsub()
			return false, nil
		}
		// Final generation check under write lock to prevent TOCTOU.
		if c.subGen[key] != gen {
			h.mu.Unlock()
			unsub()
			return false, nil
		}
		oldUnsub := c.subscriptions[key]
		c.subscriptions[key] = unsub
		h.mu.Unlock()
		if oldUnsub != nil {
			oldUnsub()
		}

		// Re-wire the subagent linker tailer: a client that subscribed before the
		// first spawn (HasProcess==false in completeSubscribe) never reached
		// maybeWireLinkerTailer, and the linker is created lazily on spawn.
		// Idempotent — guarded by wiredLinkers.
		h.maybeWireLinkerTailer(key, currentSess)

		*notify = newNotify
		return true, currentSess
	}
	// Timed out: tell the client so the dashboard can surface "subscription
	// expired" instead of stale state, and free the dead subscription slot so
	// it stops counting toward the per-connection cap. Same lock-order
	// precaution: snapshot oldUnsub under h.mu, release, then invoke.
	h.mu.Lock()
	var staleUnsub func()
	dropCache := false
	if c.subscriptions != nil {
		if u, exists := c.subscriptions[key]; exists {
			staleUnsub = u
			delete(c.subscriptions, key)
			h.decSubscriberCountLocked(key)
			// Mark subGen[key] for delayed reclamation, matching handleUnsubscribe;
			// otherwise the slot stays pinned for the connection lifetime (#2224).
			nowNanos := time.Now().UnixNano()
			c.markSubGenReleasable(key, nowNanos)
			c.sweepSubGenExpiredLocked(nowNanos)
			// Drop the marshal cache slot when this removed the last subscriber,
			// matching handleUnsubscribe/unregister (#2010).
			dropCache = !h.enforceCaps || h.subscriberCount[key] == 0
		}
	}
	h.mu.Unlock()
	if staleUnsub != nil {
		staleUnsub()
	}
	if dropCache && h.historyMarshalCache != nil {
		h.historyMarshalCache.drop(key)
	}
	c.SendJSON(wsproto.NewSessionState(wsproto.SessionState{Key: key, State: "ready", Reason: "subscription_timeout"}))
	return false, nil
}
