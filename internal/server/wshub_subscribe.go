// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     subscriber block (clients / connCount / subscriberCount /
//	            clientWG / wsAuthLimiter / wsUpgradeLimiter / upgrader /
//	            dashTokenHash / cookieMAC / trustedProxy)
//	READS:      shared deps block (read-only after ctor)
//	READS-ALSO: send block (sendClosed only — close client must drain
//	            pending sends; lifecycle-coordinated)
//
// Phase 4b 起 rule 3b 升级到 AST 字段访问对账时，会校验本文件方法体
// 真的只 WRITE subscriber 块字段；当前 Phase 0b 仅 marker 存在性。
package server

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// initialHistoryDiskTimeout bounds the disk-tier walk EventLastNVisibleCtx may
// perform during a subscribe handshake. The visible-aware reader falls back to
// reverse-scanning JSONL when the in-memory ring is entirely internal events
// (a parallel agent team flooded the tail); a slow filesystem must not stall
// the WS first frame, so the disk fallback is capped at this deadline. On
// timeout the reader returns whatever it gathered so far (memory tier at
// minimum) and the dashboard's auto-page-back safety net covers the rest.
const initialHistoryDiskTimeout = 2 * time.Second

// initialVisibleHistory reads the visible-aware initial history slice for a
// subscribe handshake, bounding the disk-tier fallback with a deadline derived
// from the Hub context so shutdown still cancels it promptly.
func (h *Hub) initialVisibleHistory(sess *session.ManagedSession, limit int) []cli.EventEntry {
	target := limit
	if target <= 0 || target > session.DefaultVisibleTarget {
		// The client's INITIAL_HISTORY_LIMIT (100) is a page-size hint, not a
		// visible-bubble target; clamp the visible goal to DefaultVisibleTarget
		// so we don't over-walk disk chasing 100 visible bubbles.
		target = session.DefaultVisibleTarget
	}
	// maxTotal=0 → the reader uses its own ceiling (maxVisibleTotal == ring
	// size). Passing `limit` here would cap the walk at the client's page-size
	// hint and strand the visible bubbles that sit beyond it under an internal
	// flood — exactly the bug. The payload can grow to the ring/page ceiling,
	// which is the pre-existing maxEventsPageLimit bound anyway.
	ctx, cancel := context.WithTimeout(h.ctx, initialHistoryDiskTimeout)
	defer cancel()
	return sess.EventLastNVisibleCtx(ctx, target, 0)
}

// File: wshub_subscribe.go
//
// Subscribe / unsubscribe WS handlers extracted from wshub.go (R243-ARCH-2
// split). Owns:
//   - handleSubscribe / completeSubscribe / handleUnsubscribe (local)
//   - handleRemoteSubscribe / handleRemoteUnsubscribe (multi-node forwarding)
//   - PurgeNodeSubscriptions (notify clients on node disconnect)
//   - maxSubscribersPerKey: per-key WS-connection cap
//   - maxSubscriptionsPerClient: per-client subscription cap
//
// All Hub state used by these helpers stays on *Hub. Pure code-relocation.

// maxSubscribersPerKey caps the number of distinct WS connections that may
// be subscribed to the same session key. Without this, a single authenticated
// token can open many connections all subscribed to one session, multiplying
// every event broadcast's fan-out cost by N. 20 is comfortably above the
// realistic multi-tab / multi-device working set (R226-SEC-8).
const maxSubscribersPerKey = 20

// maxSubscriptionsPerClient caps the number of distinct session keys a single
// WS connection may subscribe to simultaneously. Bounds per-client memory
// (subscriptions map + per-key generation/snapshot bookkeeping) and limits
// fan-out cost when a single misbehaving client tries to enumerate sessions.
// 50 covers the realistic dashboard working set (active session + recent
// history + a few cron stubs) with comfortable headroom; clients that hit
// this should re-architect rather than have the cap raised. R240-CR-4.
const maxSubscriptionsPerClient = 50

func (h *Hub) handleSubscribe(c *wsClient, msg node.ClientMsg) {
	key := msg.Key
	if key == "" {
		c.SendJSON(node.ServerMsg{Type: "error", Error: "key is required"})
		return
	}
	// R175-SEC-P1: same gate the HTTP session handlers enforce
	// (R172-SEC-L2 / R175-SEC-M). Without this, a WS client can post a
	// multi-KB key containing C1 controls or bidi characters that reach
	// slog attrs (router "session not found" path), persist into the
	// per-connection c.subscriptions map, and eventually land in
	// sessions.json at shutdown. ValidateSessionKey also caps length at
	// MaxSessionKeyBytes (~520 B) — the inline loop in sessionSend only
	// rejects ASCII C0/DEL, leaving C1 / bidi / non-UTF-8 as a log-
	// injection class for the WS subscribe path.
	if err := session.ValidateSessionKey(key); err != nil {
		c.SendJSON(node.ServerMsg{Type: "error", Error: "invalid key"})
		return
	}

	// Remote node delegation
	if msg.Node != "" && msg.Node != "local" {
		h.handleRemoteSubscribe(c, msg)
		return
	}

	// Per-connection subscription cap to prevent goroutine accumulation.
	// Reserve the slot atomically under h.mu so two concurrent subscribe
	// requests at capacity N-1 cannot both pass the check and end up at N+1.
	// The reservation is a nil-unsub placeholder that completeSubscribe will
	// overwrite with the real unsub closure; if subscription setup fails
	// before that, the placeholder would leak — but downstream code always
	// writes SOME value or sends an error back to the client without
	// returning early between here and completeSubscribe.
	h.mu.Lock()
	if _, alreadySub := c.subscriptions[key]; !alreadySub && len(c.subscriptions) >= maxSubscriptionsPerClient {
		h.mu.Unlock()
		c.SendJSON(node.ServerMsg{Type: "error", Key: key, Error: "too many subscriptions"})
		return
	}
	// R226-SEC-8: per-session-key cap across all connections. A single
	// authenticated token could otherwise open many WS connections each
	// subscribed to the same key, multiplying every event fan-out by N.
	// 20 generously covers legitimate multi-tab/multi-device usage.
	//
	// R246-PERF-4 (#716): the cap check now reads h.subscriberCount[key]
	// instead of walking every connected client × subscription map. The
	// counter is maintained alongside c.subscriptions mutations under
	// h.mu so the two stay consistent. Migration archived the prior
	// "early-break loop is good enough" rationale (R230C-PERF-4) — the
	// scan was O(N_clients) on cold keys (counter near 0) because the
	// loop only breaks at the upper cap, not at the lower bound.
	_, alreadySub := c.subscriptions[key]
	// R040034-SEC-6 (#1401): gate the cap on the explicit h.enforceCaps
	// bool rather than on `subscriberCount == nil`. Production hubs go
	// through NewHub which sets enforceCaps=true and allocates the
	// counter; hand-rolled test hubs leave both fields zero and skip cap
	// enforcement. The explicit bool documents the contract at the
	// call-site so a future refactor cannot silently activate caps in
	// every test fixture by eagerly initialising the map.
	if !alreadySub && h.enforceCaps && h.subscriberCount[key] >= maxSubscribersPerKey {
		h.mu.Unlock()
		c.SendJSON(node.ServerMsg{Type: "error", Key: key, Error: "too many subscribers for key"})
		return
	}
	// Unsubscribe from previous subscription. The counter stays unchanged
	// across this branch (one delete, one re-insert at the placeholder
	// install below) so the per-key population stays consistent.
	if unsub, ok := c.subscriptions[key]; ok {
		unsub()
		delete(c.subscriptions, key)
	}
	// Reserve the slot: placeholder keeps the map-length accurate for
	// concurrent cap checks until completeSubscribe replaces it with the
	// real unsub. If we return via the "session not found" path below, we
	// clear the reservation before returning.
	c.subscriptions[key] = func() {}
	if !alreadySub && h.enforceCaps {
		h.subscriberCount[key]++
		// Keep the lock-free mirror (read by singleSubscriber off the WS
		// push hot path) in step with the map. R20260531A-PERF-1 (#1522).
		h.setSubscriberCountFast(key, h.subscriberCount[key])
	}
	h.mu.Unlock()

	sess := h.router.GetSession(key)
	if sess == nil && h.scheduler != nil && h.scheduler.EnsureStub(key) {
		// Cron stubs are torn down by sidebar "×". Rebuild lazily on click
		// so the user doesn't have to wait for the next scheduled tick to
		// re-open the panel. EnsureStub is a no-op for non-cron keys.
		sess = h.router.GetSession(key)
	}
	if sess != nil {
		h.completeSubscribe(c, key, msg, sess)
		return
	}

	// Session not found: release the placeholder reservation. Only this
	// goroutine can have installed the placeholder for this key above, and
	// since sess was nil the completeSubscribe branch cannot replace it, so
	// an unconditional delete is safe.
	h.mu.Lock()
	if _, ok := c.subscriptions[key]; ok {
		delete(c.subscriptions, key)
		h.decSubscriberCountLocked(key)
	}
	h.mu.Unlock()

	c.SendJSON(node.ServerMsg{Type: "error", Key: key, Error: "session not found"})
}

// completeSubscribe finishes a subscription once a valid session is available.
func (h *Hub) completeSubscribe(c *wsClient, key string, msg node.ClientMsg, sess *session.ManagedSession) {
	if !sess.HasProcess() {
		// No process yet (suspended/resuming). Send persisted history so the
		// client can display old messages, and reply with "subscribed" so the
		// client's _pendingSubscribeKey is properly cleared. Without this
		// response the client gets stuck and never re-subscribes when the
		// process becomes available. Release the reserved slot since there is
		// no real unsub to install; the client can always resubscribe.
		h.mu.Lock()
		if _, ok := c.subscriptions[key]; ok {
			delete(c.subscriptions, key)
			h.decSubscriberCountLocked(key)
		}
		h.mu.Unlock()

		snap := sess.Snapshot()
		c.SendJSON(node.ServerMsg{Type: "subscribed", Key: key, State: snap.State, Reason: "suspended"})

		var entries []cli.EventEntry
		switch {
		case msg.After > 0:
			entries = sess.EventEntriesSince(msg.After)
		case msg.Limit > 0:
			// Visible-aware initial page: a suspended session whose persisted
			// tail is all internal events (parallel agent team) would otherwise
			// hand the dashboard a page that renders to the blank placeholder.
			entries = h.initialVisibleHistory(sess, msg.Limit)
		default:
			entries = sess.EventLastN(0)
		}
		if len(entries) > 0 {
			c.SendJSON(node.ServerMsg{Type: "history", Key: key, Events: entries})
		}
		slog.Debug("completeSubscribe: no process, sent persisted history", "key", key, "entries", len(entries))
		return
	}
	// Fast-fail if Shutdown already fired: SubscribeEvents would otherwise
	// register a subscriber on an EventLog whose process is being torn
	// down, and the unsub callback may never run (h.ctx.Done() in the
	// push-loop arm is the downstream guard, but avoiding the subscribe
	// entirely is cleaner).
	if h.ctx.Err() != nil {
		h.mu.Lock()
		if _, ok := c.subscriptions[key]; ok {
			delete(c.subscriptions, key)
			h.decSubscriberCountLocked(key)
		}
		h.mu.Unlock()
		return
	}
	// Wire server-side tailer ensure on Linker.Resolve. Idempotent: the
	// Linker's OnResolve list accumulates per-subscribe because completeSubscribe
	// fires on every re-subscribe, but ensureTailer is guarded by the
	// (key, taskID) map and extra callback invocations are cheap no-ops on
	// an already-running tailer. The "right" place is router.spawnSession,
	// but avoiding that coupling keeps server/cli layering clean. S2-OK.
	h.maybeWireLinkerTailer(key, sess)
	notify, unsub := sess.SubscribeEvents()

	h.mu.Lock()
	// Re-check ctx under the lock: the earlier fast-fail check was racy
	// with Shutdown's h.mu-guarded subscription teardown; if Shutdown
	// acquired h.mu between the fast-fail check and this Lock, clients
	// subscriptions was niled and the first branch below handles it.
	// But Shutdown's sequence is cancel() -> h.mu.Lock() -> iterate
	// subscriptions, so ctx.Err() being set here is a strong signal that
	// Shutdown is mid-flight; decline to start a new pushLoop.
	if c.subscriptions == nil || h.ctx.Err() != nil {
		h.mu.Unlock()
		unsub()
		return
	}
	c.subscriptions[key] = unsub
	c.subGen[key]++
	gen := c.subGen[key]
	// R175-P2: the key is live again, so any pending reclamation marker for
	// this key is stale. If we left it in place, a sweep triggered mid-life
	// would delete subGen[key] out from under an active subscription and
	// the next resubscribeEvents tick would see the counter collapse back
	// toward 0, breaking the R163 takeover-detection contract.
	c.clearSubGenReleasable(key)
	// Add to clientWG BEFORE releasing h.mu. Shutdown walks h.clients under
	// h.mu to close conns, then calls clientWG.Wait; if we Add(1) after
	// releasing here, Shutdown's Wait can return before the eventPushLoop
	// goroutine ever starts, and the goroutine can then touch torn-down state.
	h.clientWG.Add(1)
	h.mu.Unlock()

	snap := sess.Snapshot()

	var entries []cli.EventEntry
	switch {
	case msg.After > 0:
		entries = sess.EventEntriesSince(msg.After)
	case msg.Limit > 0:
		// Initial subscribe asks for the last `limit` events only — this is
		// the dashboard pagination fast path. Clients walk further back via
		// HTTP /api/sessions/events?before=.. rather than resubscribing.
		//
		// Visible-aware: when a parallel agent team has filled the trailing
		// `limit` events with internal tool_use / task_progress entries, a
		// plain EventLastN(limit) returns a page that the dashboard filters
		// down to nothing and renders as the blank "该会话最近仅有 agent
		// 活动" placeholder. EventLastNVisibleCtx keeps walking (ring, then
		// disk) until the page carries real chat bubbles.
		entries = h.initialVisibleHistory(sess, msg.Limit)
	default:
		// Legacy path: send everything the log remembers. Kept so older
		// clients (and the node-to-node relay) still see full history.
		entries = sess.EventLastN(0)
	}

	slog.Debug("completeSubscribe: sending history", "key", key, "entries", len(entries), "state", snap.State)
	c.SendJSON(node.ServerMsg{Type: "subscribed", Key: key, State: snap.State})

	var lastTime int64
	if len(entries) > 0 {
		// Pooled marshal — initial history payloads can be hundreds of KB
		// (max msg.Limit entries × ~500B-4KB each). SendJSON would otherwise
		// allocate a fresh buffer per subscribe handshake; eventPushLoop
		// already uses marshalPooled for the same shape. R218-PERF-14.
		if data, err := marshalPooled(node.ServerMsg{Type: "history", Key: key, Events: entries}); err == nil {
			c.SendRaw(data)
		} else {
			slog.Warn("history marshal failed, falling back", "err", err, "key", key)
			c.SendJSON(node.ServerMsg{Type: "history", Key: key, Events: entries})
		}
		lastTime = entries[len(entries)-1].Time
	} else if snap.State == "running" {
		// Always send an (empty) history for running sessions so the client's
		// _initialSubscribe flag is consumed. Without this, the client shows a
		// blank events area until eventPushLoop delivers the first batch, which
		// can be a noticeable delay if the process just started.
		c.SendJSON(node.ServerMsg{Type: "history", Key: key, Events: []cli.EventEntry{}})
	}

	go func() {
		defer h.clientWG.Done()
		h.eventPushLoop(c, key, gen, notify, sess, lastTime)
	}()
}

func (h *Hub) handleUnsubscribe(c *wsClient, msg node.ClientMsg) {
	key := msg.Key

	// R176-SEC-P1: same gate as handleSubscribe / handleInterrupt. Without
	// this, an authenticated WS client can hand-craft a `key` containing
	// C1 / bidi / non-UTF-8 bytes that lands in the echoed
	// `{"type":"unsubscribed","key":...}` reply and any structured log
	// attr on the path. ValidateSessionKey also caps at MaxSessionKeyBytes.
	// Gate BEFORE the remote-node delegation: handleRemoteUnsubscribe reads
	// msg.Key too, so the local-only placement of this check would leave
	// the remote path unguarded.
	if err := session.ValidateSessionKey(key); err != nil {
		c.SendJSON(node.ServerMsg{Type: "error", Error: "invalid key"})
		return
	}

	// Remote node delegation
	if msg.Node != "" && msg.Node != "local" {
		h.handleRemoteUnsubscribe(c, msg)
		return
	}

	h.mu.Lock()
	dropMarshalCache := false
	if unsub, ok := c.subscriptions[key]; ok {
		unsub()
		delete(c.subscriptions, key)
		h.decSubscriberCountLocked(key)
		// Intentionally keep c.subGen[key] intact: a stale eventPushLoop from
		// this subscription may still be parked in resubscribeEvents' ticker
		// (up to 60s). Deleting subGen[key] and allowing a new subscribe to
		// reset the counter to 1 would let the stale goroutine's gen=1 match
		// the fresh subGen[key]=1 and silently resume.
		//
		// R175-P2: mark for delayed reclamation. Without this, long-lived
		// dashboard clients that flap through many session panels would grow
		// c.subGen without bound (10k+ keys over a multi-day connection was
		// observed in production telemetry). The 75s retention window is
		// longer than resubscribeEvents' worst-case 60s park so any stale
		// goroutine is guaranteed to have exited before we delete the entry.
		nowNanos := time.Now().UnixNano()
		c.markSubGenReleasable(key, nowNanos)
		c.sweepSubGenExpiredLocked(nowNanos)
		// R214-PERF-4: if no other client still subscribes to this key, drop
		// the cached "history" marshal slot so its payload (capped at
		// maxHistoryPushEntries entries; up to ~100 KB worst case) is GC'd
		// instead of pinning memory until Shutdown.
		//
		// R236-PERF-06 (#513): O(1) via the counter h.decSubscriberCountLocked
		// just decremented. After the decrement, h.subscriberCount[key] holds
		// the residual subscriber population on this key; zero (counter map
		// entry deleted) means we were the last subscriber, so the cache slot
		// is unreachable. The pre-counter implementation walked h.clients
		// here — O(N_clients) under h.mu on every dashboard tab close.
		dropMarshalCache = !h.enforceCaps || h.subscriberCount[key] == 0
	}
	h.mu.Unlock()
	if dropMarshalCache && h.historyMarshalCache != nil {
		h.historyMarshalCache.drop(key)
	}
	c.SendJSON(node.ServerMsg{Type: "unsubscribed", Key: key})
}

// decSubscriberCountLocked decrements h.subscriberCount[key] and removes the
// entry once it hits zero, so the map size mirrors the number of distinct
// keys that currently have at least one subscriber. Caller MUST hold h.mu.
//
// Defensive against pre-counter Hubs: the cap check in handleSubscribe
// short-circuits via h.enforceCaps when the counter is unwired, so this
// helper is a safe no-op rather than a panic. R246-PERF-4 (#716);
// R040034-SEC-6 (#1401) explicit-bool gate. The data-presence check
// (subscriberCount == nil) is retained alongside enforceCaps so a hand-
// rolled test fixture that populated the map directly without flipping
// enforceCaps still mutates the map correctly — the gate decides whether
// to ENFORCE the cap, not whether the map exists.
func (h *Hub) decSubscriberCountLocked(key string) {
	if h.subscriberCount == nil {
		return
	}
	n := h.subscriberCount[key]
	if n <= 1 {
		delete(h.subscriberCount, key)
		h.subscriberCountFast.Delete(key)
		return
	}
	h.subscriberCount[key] = n - 1
	h.setSubscriberCountFast(key, n-1)
}

// setSubscriberCountFast mirrors subscriberCount[key]=n into the lock-free
// subscriberCountFast map so singleSubscriber can read the per-key
// population without h.mu. Caller MUST hold h.mu (it is invoked only from
// the subscriberCount write paths). The mirror is keyed by string and holds
// *atomic.Int32 so concurrent lock-free readers get a value-consistent load
// even while a writer is updating a different key. R20260531A-PERF-1 (#1522).
func (h *Hub) setSubscriberCountFast(key string, n int) {
	if v, ok := h.subscriberCountFast.Load(key); ok {
		v.(*atomic.Int32).Store(int32(n))
		return
	}
	var ctr atomic.Int32
	ctr.Store(int32(n))
	h.subscriberCountFast.Store(key, &ctr)
}

// ─── Remote node handlers ────────────────────────────────────────────────────

func (h *Hub) handleRemoteSubscribe(c *wsClient, msg node.ClientMsg) {
	// Reject malformed node IDs BEFORE calling slog to prevent log injection
	// via ANSI/newline bytes in the attacker-controlled Node field. HTTP
	// handlers rely on LookupNode for the same guard; the WS paths bypassed
	// it because the map lookup itself does not validate.
	if !isValidNodeID(msg.Node) {
		c.SendJSON(node.ServerMsg{Type: "error", Key: msg.Key, Error: "unknown node"})
		return
	}
	conn, ok := h.lookupNode(msg.Node)
	if !ok {
		// Do not echo the client-supplied node ID in the error: a careless
		// JS consumer rendering the field via innerHTML would turn a crafted
		// node value into reflected XSS. Log internally for operator triage.
		slog.Debug("ws subscribe: unknown node", "node", msg.Node)
		c.SendJSON(node.ServerMsg{Type: "error", Key: msg.Key, Error: "unknown node"})
		return
	}
	// H6 (#435): subscribe only needs the pub-sub role, so narrow the full
	// Conn down to node.NodeSubscriber at the call boundary.
	var sub node.NodeSubscriber = conn
	sub.Subscribe(c, msg.Key, msg.After)
}

func (h *Hub) handleRemoteUnsubscribe(c *wsClient, msg node.ClientMsg) {
	if !isValidNodeID(msg.Node) {
		// Mirror the success shape so slow clients can drop state even when
		// the node ID is malformed — behaviour equivalent to "no such node".
		c.SendJSON(node.ServerMsg{Type: "unsubscribed", Key: msg.Key})
		return
	}
	conn, ok := h.lookupNode(msg.Node)
	if !ok {
		c.SendJSON(node.ServerMsg{Type: "unsubscribed", Key: msg.Key, Node: msg.Node})
		return
	}
	// H6 (#435): unsubscribe only needs the pub-sub role.
	var sub node.NodeSubscriber = conn
	sub.Unsubscribe(c, msg.Key)
}

// PurgeNodeSubscriptions notifies all browser clients that a node disconnected,
// so they can deselect stale sessions.
func (h *Hub) PurgeNodeSubscriptions(nodeID string) {
	data, err := marshalPooled(node.ServerMsg{Type: "error", Node: nodeID, Error: "node disconnected"})
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
}
