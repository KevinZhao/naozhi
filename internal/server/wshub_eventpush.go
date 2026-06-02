// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     rate-limit/cache block (historyMarshalCache for replay cache)
//	READS:      shared deps block (read-only after ctor) + subscriber block
//	            (clients for fanout) + lifecycle block (ctx for cancel)
//
// Phase 4b 起 rule 3b 升级到 AST 字段访问对账时，会校验本文件方法体
// 的字段访问匹配本契约；当前 Phase 0b 仅 marker 存在性。
package server

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// File: wshub_eventpush.go
//
// Per-subscription event-push loop and resubscribe helpers extracted from
// wshub.go (R243-ARCH-2 split). Owns:
//   - maxHistoryPushEntries / capHistoryBatch (history batching cap)
//   - eventPushLoop: long-lived goroutine that fans EventLog updates to a
//     subscribed wsClient, with generation gating + flap-aware resubscribe
//   - resubscribeEvents: re-attach to the post-flap EventLog without
//     dropping the WS subscription
//   - resubscribeMaxAttempts / resubscribeInterval: wait budget constants
//
// All Hub state used by these helpers stays on *Hub. Pure code-relocation.

// resubscribeMaxAttempts and resubscribeInterval together set the wait
// budget for resubscribeEvents — used when a session process flapped and
// the WS client wants to pick up the freshly attached EventLog without
// dropping the dashboard subscription.
//
// Total window = resubscribeMaxAttempts × resubscribeInterval = 12 × 5s = 60s.
// 60s comfortably covers the cold-start budget for a `claude` CLI subprocess
// (typical first-init: 5-15s; worst case with model warmup + remote git fetch:
// 30-45s). Beyond 60s we declare flap permanent and drop the WS subscription;
// the client's exponential-backoff reconnect loop takes over.
//
// The two constants are split (not a single 60s timer) so the per-iteration
// loop body — generation check, ctx fan-out, client-disconnect detection —
// runs at a 5s heartbeat instead of blocking the whole window. R240-CR-6.
const (
	resubscribeMaxAttempts = 12
	resubscribeInterval    = 5 * time.Second
)

// maxHistoryPushEntries caps a single WS "history" push. EventEntriesSince
// on an initial catch-up (lastTime=0) or after a notify backlog can return
// the full ring buffer (maxPersistedHistory=500 entries). At ~200 B per
// entry JSON-encoded, a 500-entry batch balloons to ~100 KB per push; with
// 500 active WS connections that is 50 MB of simultaneous marshal work
// blocking the hub. 50 entries matches the dashboard's paginated
// /api/sessions/events tail fetch, so older entries are still reachable
// via the `before=` path. R68-PERF-H1.
const maxHistoryPushEntries = 50

func capHistoryBatch(entries []cli.EventEntry) []cli.EventEntry {
	if len(entries) <= maxHistoryPushEntries {
		return entries
	}
	return entries[len(entries)-maxHistoryPushEntries:]
}

// marshalHistoryFrame produces the WS "history" frame bytes for the given
// session key + entries tail, coalescing the marshalPooled call across all
// eventPushLoop goroutines that are in lock-step on the same session. R214-
// PERF-4: the prior code path called marshalPooled directly inside each
// pushLoop, so N multi-tab dashboards on one session paid N reflect-marshals
// per notify wave on payloads that were byte-identical between tabs.
//
// The cache is keyed by session key; the per-key fingerprint
// (lastTime, latest entry Time, count) detects out-of-lockstep subscribers
// (e.g. a slow tab that fell behind the head and is now catching up) and
// forces a fresh marshal rather than handing back stale bytes.
//
// On cache miss the marshal runs under the per-key mutex inside
// historyMarshalCache.getOrMarshal so the first arriving subscriber pays the
// marshal cost once and the rest of the fan-out wave reuses the bytes. The
// returned []byte is safe to hand to wsClient.SendRaw concurrently from
// multiple goroutines — SendRaw enqueues a slice header into a per-client
// channel and the writePump never mutates the underlying buffer.
func (h *Hub) marshalHistoryFrame(key string, lastTime int64, entries []cli.EventEntry) ([]byte, error) {
	if h.historyMarshalCache == nil {
		// Defensive: should not happen for a Hub built via NewHub, but a
		// hand-constructed test Hub may skip the field. Fall back to the
		// uncached path so behaviour is identical to pre-R214-PERF-4.
		return marshalPooled(node.ServerMsg{Type: "history", Key: key, Events: entries})
	}
	// R249-PERF-30 (#944): single-subscriber fast path. The marshal
	// cache only pays off when ≥2 pushLoops share the same (key,
	// fingerprint) wave — for one tab on the session every notify
	// advances lastTime, so the fingerprint always misses and the
	// per-key sync.Map.Load + marshalCacheEntry.mu round-trip is pure
	// overhead. A short h.mu.RLock + map lookup is cheaper than that
	// round-trip on a hit AND avoids the slot-allocation cost on the
	// first miss for a short-lived single-tab session. When
	// singleSubscriber returns false (count != 1, or counter unwired
	// in test harnesses) we fall through to the cached path so
	// behaviour for the multi-tab fan-out case is unchanged.
	if h.singleSubscriber(key) {
		return marshalPooled(node.ServerMsg{Type: "history", Key: key, Events: entries})
	}
	data, _, err := h.historyMarshalCache.getOrMarshal(key, lastTime, entries, func() ([]byte, error) {
		return marshalPooled(node.ServerMsg{Type: "history", Key: key, Events: entries})
	})
	return data, err
}

// singleSubscriber reports whether `key` has exactly one subscriber.
// Returns false when the count is 0 (key being torn down or never
// registered) so the caller's fast-path is gated on the strict
// "single tab" definition only — multi-tab broadcasts AND the
// transient "no subscribers" window both flow through the cached
// path. R249-PERF-30 (#944).
//
// R20260531A-PERF-1 (#1522): read the lock-free subscriberCountFast
// mirror instead of taking h.mu.RLock. This call fires once per
// EventLog notify wave per subscribed client (5-50 events/s × N
// sessions), so the old RLock contended with subscribe/unsubscribe
// writers on h.mu for what is purely a fast-path heuristic. The mirror
// is maintained under h.mu by every subscriberCount mutation, so the
// lock-free load is at most one writer-critical-section stale — a wrong
// verdict only changes whether this push uses the marshal cache, never
// the emitted bytes. nil gate retained for hand-built test hubs that
// skip NewHub (subscriberCount left nil): they get the same
// "false / use cached path" behaviour as before.
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
// its entire lifetime (Add happens in completeSubscribe before go; Done runs
// in the goroutine's defer).
//
// CLIENTWG CONTRACT (R49-CONCUR-RESUBSCRIBE-CLIENTWG): when resubscribeEvents
// transparently swaps `sess` for a new process's session (the `!ok` arm
// below), the loop keeps running in the same goroutine — we do NOT Add(1)
// for the new subscription. This is correct because:
//
//  1. The lifetime being tracked is "this pushLoop goroutine", not "this
//     particular EventLog subscription". A single Add/Done pair covers
//     every successful resubscribe within the goroutine.
//  2. resubscribeEvents installs the new `unsub` into c.subscriptions[key]
//     under h.mu, replacing the stale one — so Hub.Shutdown walking
//     c.subscriptions sees the current generation's unsub without any
//     additional bookkeeping.
//  3. The unsub → notify closure ensures resubscribeEvents returns ok=false
//     on Shutdown (h.ctx.Done is checked), so the goroutine exits and
//     the single deferred Done balances the single Add.
//
// If you ever split the resubscribe path into a new goroutine (e.g. to
// parallelise multi-session fan-in), you MUST Add(1) for the new goroutine
// and Done from its own defer — otherwise Shutdown's clientWG.Wait either
// hangs (Add without Done) or panics with negative counter (Done without
// Add). The guarantee is enforced by code shape, not by assertion; a
// review that simply notes "+1 goroutine here" is insufficient without
// also updating the WG pairing.
func (h *Hub) eventPushLoop(c *wsClient, key string, gen uint64, notify <-chan struct{}, sess *session.ManagedSession, lastTime int64) {
	defer func() {
		if r := recover(); r != nil {
			// Mirror readPump: bump counter first so the panic rate is
			// visible even when stack output is truncated, then log the
			// cause at Error and the stack at Debug to avoid leaking
			// internal paths to aggregated log stores. Tag with the
			// subscription key so operators can correlate the panic
			// against a specific session fan-out.
			serverMetrics.PanicRecovered()
			slog.Error("panic in ws eventPushLoop (recovered)",
				"key", key, "panic", fmt.Sprintf("%v", r))
			slog.Debug("panic in ws eventPushLoop: stack",
				"key", key, "stack", string(debug.Stack()))
			// Without closing the connection, the panicked subscription
			// would linger: subscriptions[key]/subGen[key] stay set,
			// subscriberCount[key] is not decremented, and the per-key cap
			// (maxSubscribersPerKey) eventually traps further subscribes
			// from the same client. Closing done unblocks readPump/
			// writePump, which run unregister and tear down all subs.
			c.closeDone()
		}
	}()
	for {
		select {
		case _, ok := <-notify:
			if !ok {
				ok, newSess := h.resubscribeEvents(c, key, gen, &notify)
				if !ok {
					return
				}
				sess = newSess
				// Catch up on events missed during the resubscribe
				// transition. resubscribeEvents may consume one pending
				// notification while probing newNotify (ok=true path) — if
				// we didn't catch up unconditionally here, those events
				// would only surface on the next Append, which in an idle
				// session may be seconds or more. R246-CR-006 / #744:
				// extracted into backfillSubscriberEvents so the same
				// coalesced-marshal path serves both the post-resubscribe
				// catch-up and the regular notify drain.
				newLast, alive := h.backfillSubscriberEvents(c, key, sess, lastTime)
				if !alive {
					return
				}
				lastTime = newLast
				continue
			}
			newLast, alive := h.backfillSubscriberEvents(c, key, sess, lastTime)
			if !alive {
				return
			}
			lastTime = newLast
		case <-c.done:
			return
		case <-h.ctx.Done():
			// Hub shutdown: exit even if the client hasn't closed and the
			// subscribed notify channel is stalled. Without this arm, a
			// notify source that stops firing could park this goroutine
			// past Shutdown, with no escape until conn.Close propagates
			// through readPump — which may not happen if the socket is
			// half-open.
			return
		}
	}
}

// backfillSubscriberEvents drains EventEntriesSince(lastTime) for sess,
// marshals the batched "history" frame via the coalesced cache, and
// writes it to c. Returns (newLastTime, alive) — the caller must update
// its own lastTime with newLastTime AND exit when alive is false (the
// client closed mid-drain).
//
// R246-CR-006 / #744: previously inlined twice in eventPushLoop — once
// in the regular notify arm and once in the post-resubscribe catch-up
// branch. The two copies differed only in the c.done early-return: the
// regular arm exited the loop on close, the resubscribe arm did not. The
// helper now propagates that signal via the alive return so both call
// sites get the same eviction semantics.
//
// Capping via capHistoryBatch is mandatory: a slow client that built up
// a long backlog must not see a single multi-MB push frame that starves
// the WS send channel — the dashboard backfills older events via
// /api/sessions/events?before=. R68-PERF-H1. marshalHistoryFrame
// coalesces the JSON marshal across N pushLoops subscribed to the same
// session — multi-tab dashboards previously paid N marshals per notify
// wave for the identical (key, entries-tail) payload. R214-PERF-4.
//
// Behavior note: on marshal error the helper returns the unchanged
// lastTime (matches the regular-notify arm's `continue` path). The
// previous post-resubscribe inline branch advanced lastTime even on
// marshal error, which silently dropped events — strictly more correct
// to retry from the same lastTime on the next notify, which is what the
// helper now does for both call sites.
func (h *Hub) backfillSubscriberEvents(c *wsClient, key string, sess *session.ManagedSession, lastTime int64) (int64, bool) {
	// R112714-PERF-11: use EventEntriesSinceAppend so the dead-session path
	// (persistedHistory) can reuse a buffer. Live-session path still allocates
	// because ProcessEventReader.EventEntriesSince has no append variant
	// (adding it would touch cli.Process + all fakes — deferred). Passing nil
	// here matches the prior EventEntriesSince(nil) behaviour; callers that
	// want per-client buffer reuse can pass a pre-allocated dst instead.
	entries := sess.EventEntriesSinceAppend(nil, lastTime)
	if len(entries) == 0 {
		return lastTime, true
	}
	select {
	case <-c.done:
		return lastTime, false
	default:
	}
	entries = capHistoryBatch(entries)
	data, err := h.marshalHistoryFrame(key, lastTime, entries)
	if err != nil {
		return lastTime, true
	}
	c.SendRaw(data)
	return entries[len(entries)-1].Time, true
}

// resubscribeEvents waits for a new process to be attached to the session and
// re-subscribes to its EventLog. Returns (ok, currentSession). ok is false if
// the client disconnects, the wait times out (resubscribeMaxAttempts ×
// resubscribeInterval = 60s), or a newer subscription has taken over this
// key (generation mismatch).
func (h *Hub) resubscribeEvents(c *wsClient, key string, gen uint64, notify *<-chan struct{}) (bool, *session.ManagedSession) {
	// Timer.Reset reuses a single timer allocation across the iterations
	// instead of allocating a Ticker and its runtime goroutine; resubscribe
	// is a cold-ish path but client flap can trigger N simultaneous calls.
	// resubscribeMaxAttempts × resubscribeInterval = 60s total window —
	// see const godoc for the budget rationale.
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

		// Check if a newer subscription (from handleSubscribe) has taken over.
		//
		// R230C-PERF-8 (archive 2026-05-23): the ticket proposed dropping this
		// h.mu.RLock and "comparing against the local gen parameter directly".
		// That misreads the invariant — `gen` is the generation captured when
		// resubscribeEvents started, and `c.subGen[key]` is the *current* per-
		// client generation written by handleSubscribe under h.mu.Lock when a
		// fresh subscribe takes over the same key. The lock is the visibility
		// barrier that lets this stale-resubscribe goroutine observe the new
		// generation and bail out; without it Go memory model gives no read
		// guarantee on the map slot. Only ~12 RLock probes per resubscribe and
		// the contention is bounded by the per-client subscription map, so the
		// "免锁" optimisation would buy nothing and forfeit the supersede
		// signal that lets stale loops self-terminate. Keep as-is.
		h.mu.RLock()
		currentGen := c.subGen[key]
		h.mu.RUnlock()
		if currentGen != gen {
			return false, nil
		}

		// Re-check the router for the current session — spawnSession may have
		// created a new ManagedSession, replacing the old one in the map.
		currentSess := h.router.GetSession(key)
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

		// Update the subscription registration for this client.
		//
		// H8 (Round 163): capture the old unsub while holding h.mu but call
		// it *after* releasing the lock. The current lock order is
		// h.mu → EventLog.subMu (enforced by Hub.Shutdown's contract and the
		// shutdown_lock_order_test.go tripwire), so calling oldUnsub() under
		// h.mu is technically safe today. Releasing h.mu first removes a
		// latent hazard: if oldUnsub() were ever extended to take additional
		// locks (e.g. a future per-client audit mutex or a sub-layer WG
		// protected by h.mu), calling it under h.mu would reintroduce a
		// reverse acquisition order. Swap is a rare path (resubscribe
		// collision), so the extra unlock/relock has no measurable cost.
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

		// Re-wire the subagent linker tailer for this session: a client
		// that subscribed BEFORE the first process spawn (HasProcess==false
		// branch in completeSubscribe) never reached maybeWireLinkerTailer,
		// and the linker is created lazily on spawn. Without this call,
		// the only client subscribed via the suspended path would never
		// receive subagent transcripts from the freshly-spawned process.
		// Idempotent — guarded by wiredLinkers.
		h.maybeWireLinkerTailer(key, currentSess)

		*notify = newNotify
		return true, currentSess
	}
	// Timed out waiting for new process — notify client so the dashboard
	// can surface a "subscription expired" indicator instead of silently
	// showing stale state. Clean up the dead subscription slot so it doesn't
	// count toward the per-connection cap.
	//
	// H8 (Round 163): same lock-order precaution — snapshot oldUnsub
	// under h.mu, release the lock, then invoke it.
	h.mu.Lock()
	var staleUnsub func()
	if c.subscriptions != nil {
		if u, exists := c.subscriptions[key]; exists {
			staleUnsub = u
			delete(c.subscriptions, key)
			h.decSubscriberCountLocked(key)
		}
	}
	h.mu.Unlock()
	if staleUnsub != nil {
		staleUnsub()
	}
	c.SendJSON(node.ServerMsg{Type: "session_state", Key: key, State: "ready", Reason: "subscription_timeout"})
	return false, nil
}
