package server

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// resubscribeMaxAttempts × defaultResubscribeInterval (60s) is the wait budget for
// resubscribeEvents; it covers a `claude` CLI cold start (worst case 30-45s),
// after which the flap is permanent and the client's reconnect loop takes over.
// Split into attempts so the loop body (generation check, ctx / client-done
// fan-out) runs at a 5s heartbeat instead of blocking the whole window.
const (
	resubscribeMaxAttempts     = 12
	defaultResubscribeInterval = 5 * time.Second
)

// maxHistoryPushEntries is the per-frame chunk size of a WS "history" push:
// a full-ring catch-up (500 entries, ~100 KB) goes out as several ordered
// frames instead of one, so no single frame hogs the client's send buffer.
const maxHistoryPushEntries = 50

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
	// Single-subscriber fast path (#944): with one tab every notify advances
	// lastTime so the cache always misses; skip the sync.Map + mutex round-trip.
	// count != 1 falls through to the cached path.
	if h.singleSubscriber(key) {
		return marshalPooled(wsproto.NewHistory(wsproto.History{Key: key, Events: entries}))
	}
	data, _, err := h.historyMarshalCache.getOrMarshal(key, lastTime, entries, func() ([]byte, error) {
		return marshalPooled(wsproto.NewHistory(wsproto.History{Key: key, Events: entries}))
	})
	return data, err
}

// singleSubscriber reports whether key has exactly one subscriber, so only
// the strict single-tab case skips the shared marshal cache. A stale verdict
// only changes which path marshals this push, never the bytes.
func (h *Hub) singleSubscriber(key string) bool {
	return h.subs.singleSubscriber(key)
}

// eventPushLoop is the per-subscription pump that reads EventLog notifications
// and streams entries to the WS client. It owns exactly one clientWG slot for
// its lifetime (Add in completeSubscribe before go; Done in the deferred func).
//
// CLIENTWG CONTRACT: when resubscribeEvents swaps `sess` for a new process's
// session the loop keeps running in this goroutine, so there is NO extra
// Add(1): the tracked lifetime is the goroutine, and resubscribeEvents swaps
// the new unsub into the registry so Shutdown's drain sees it.
// Anyone splitting the resubscribe path into a new goroutine MUST Add(1) for it
// and Done from its own defer, or Shutdown's clientWG.Wait hangs / panics.
func (h *Hub) eventPushLoop(c *wsClient, key string, gen uint64, notify <-chan struct{}, sess *session.ManagedSession, csr *clievent.SinceCursor) {
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
			// all subs; otherwise the registry keeps this key subscribed
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
// SinceCursor and writes them to c, in order, as "history" frames of at most
// maxHistoryPushEntries. Returns (alive, buf) — the caller must exit when alive
// is false (the client closed mid-drain) and retain buf for the next wave.
//
// The cursor advances per frame and only once the frame is enqueued: a marshal
// error or a send dropped on a full buffer stops the wave with the rest still
// above the watermark, so the next notify retries from there (#3008). The
// inclusive watermark query + UUID dedup keep same-millisecond entries split
// across waves or frames from being lost or resent (#2402).
func (h *Hub) backfillSubscriberEvents(c *wsClient, key string, sess *session.ManagedSession, csr *clievent.SinceCursor, buf []clievent.EventEntry) (bool, []clievent.EventEntry) {
	// buf[:0] lets both the dead-session and live-process paths reuse capacity
	// across notify waves (#1740); entries are consumed synchronously below and
	// never retained. QueryAfter re-admits the watermark millisecond; Filter
	// drops already-delivered UUIDs in place, so the backing array survives.
	entries := sess.EventEntriesSinceAppend(buf[:0], csr.QueryAfter())
	fetched := entries
	entries = csr.Filter(entries)
	// Chunks are sub-slices of entries, but `fetched` is what is returned so
	// the buffer keeps its full capacity across waves.
	for len(entries) > 0 {
		select {
		case <-c.done:
			return false, fetched
		default:
		}
		chunk := entries[:min(len(entries), maxHistoryPushEntries)]
		// The marshal-cache fingerprint keys on the pre-advance watermark, so
		// lock-step tabs still coalesce onto one marshal per chunk.
		data, err := h.marshalHistoryFrame(key, csr.Watermark(), chunk)
		if err != nil {
			return true, fetched
		}
		if !c.trySendRaw(data) {
			return true, fetched
		}
		csr.Advance(chunk)
		entries = entries[len(chunk):]
	}
	return true, fetched
}

// resubscribeEvents waits for a new process to be attached to the session and
// re-subscribes to its EventLog. Returns (ok, currentSession). ok is false if
// the client disconnects, the wait times out (resubscribeMaxAttempts ×
// resubscribeInterval, 60s by default), or a newer subscription has taken over this
// key (generation mismatch).
func (h *Hub) resubscribeEvents(c *wsClient, key string, gen uint64, notify *<-chan struct{}) (bool, *session.ManagedSession) {
	// Timer.Reset reuses one timer across iterations instead of a Ticker + its
	// goroutine; client flap can trigger N simultaneous calls.
	timer := time.NewTimer(h.resubscribeInterval)
	defer timer.Stop()

	for i := range resubscribeMaxAttempts {
		if i > 0 {
			timer.Reset(h.resubscribeInterval)
		}
		select {
		case <-c.done:
			return false, nil
		case <-h.ctx.Done():
			return false, nil
		case <-timer.C:
		}

		// Bail out once the client is gone or a newer subscription
		// (handleSubscribe) has taken over.
		if currentGen, ok := h.subs.generation(c, key); !ok || currentGen != gen {
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

		// swap re-checks the generation under the registry's lock and hands
		// the old unsub back to run after release (lock order: registry →
		// EventLog.subMu).
		oldUnsub, ok := h.subs.swap(c, key, gen, unsub)
		if !ok {
			unsub()
			return false, nil
		}
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
	// it stops counting toward the per-connection cap. The stale unsub runs
	// after the registry's lock is released; the last subscriber leaving
	// drops the marshal cache slot, as handleUnsubscribe does.
	staleUnsub, dropCache := h.subs.expire(c, key, time.Now().UnixNano())
	if staleUnsub != nil {
		staleUnsub()
	}
	if dropCache {
		h.historyMarshalCache.drop(key)
	}
	c.SendJSON(wsproto.NewSessionState(wsproto.SessionState{Key: key, State: "ready", Reason: "subscription_timeout"}))
	return false, nil
}
