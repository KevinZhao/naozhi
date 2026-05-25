package server

import (
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

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
	// 20 generously covers legitimate multi-tab/multi-device usage; the
	// O(connections) scan is bounded by maxWSConns=500 and only runs on
	// subscribe (low frequency) — not on each broadcast. The inner loop
	// terminates early once count reaches the cap (R230C-PERF-4 archived
	// 2026-05-23): worst-case work is O(maxSubscribersPerKey) regardless
	// of total connection count, so the optimisation TODO described
	// ("subscriberCounts map[string]int O(1)") would only save a small
	// constant on the cold subscribe path while adding a second invariant
	// to maintain on every disconnect — not worth the bookkeeping.
	// NEEDS-DESIGN R242-GO-5: 锁内 O(N) 遍历 clients 计算 per-key 订阅数；
	// 单 dashboard 用户场景可接受，多 operator 时改增量计数器避免锁内全扫描。
	if _, alreadySub := c.subscriptions[key]; !alreadySub {
		count := 0
		for other := range h.clients {
			if other.subscriptions == nil {
				continue
			}
			if _, has := other.subscriptions[key]; has {
				count++
				if count >= maxSubscribersPerKey {
					break
				}
			}
		}
		if count >= maxSubscribersPerKey {
			h.mu.Unlock()
			c.SendJSON(node.ServerMsg{Type: "error", Key: key, Error: "too many subscribers for key"})
			return
		}
	}
	// Unsubscribe from previous subscription
	if unsub, ok := c.subscriptions[key]; ok {
		unsub()
		delete(c.subscriptions, key)
	}
	// Reserve the slot: placeholder keeps the map-length accurate for
	// concurrent cap checks until completeSubscribe replaces it with the
	// real unsub. If we return via the "session not found" path below, we
	// clear the reservation before returning.
	c.subscriptions[key] = func() {}
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
	delete(c.subscriptions, key)
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
		delete(c.subscriptions, key)
		h.mu.Unlock()

		snap := sess.Snapshot()
		c.SendJSON(node.ServerMsg{Type: "subscribed", Key: key, State: snap.State, Reason: "suspended"})

		var entries []cli.EventEntry
		switch {
		case msg.After > 0:
			entries = sess.EventEntriesSince(msg.After)
		case msg.Limit > 0:
			entries = sess.EventLastN(msg.Limit)
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
		delete(c.subscriptions, key)
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
		entries = sess.EventLastN(msg.Limit)
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
		// instead of pinning memory until Shutdown. Walk under h.mu — already
		// held — so no other client can register a subscription concurrently.
		dropMarshalCache = !h.anyOtherClientSubscribesLocked(c, key)
	}
	h.mu.Unlock()
	if dropMarshalCache && h.historyMarshalCache != nil {
		h.historyMarshalCache.drop(key)
	}
	c.SendJSON(node.ServerMsg{Type: "unsubscribed", Key: key})
}

// anyOtherClientSubscribesLocked returns true when at least one client other
// than `excluded` has a live subscription on `key`. Caller MUST hold h.mu.
//
// O(N_clients) — acceptable on the unsubscribe path because the dashboard's
// per-tab subscribe/unsubscribe rate is bounded by user navigation, not
// per-event traffic. The fan-out hot path (eventPushLoop / broadcast) does
// NOT call this helper; only handleUnsubscribe / Shutdown do, so this scan
// is off the per-event critical path.
func (h *Hub) anyOtherClientSubscribesLocked(excluded *wsClient, key string) bool {
	for other := range h.clients {
		if other == excluded {
			continue
		}
		if _, ok := other.subscriptions[key]; ok {
			return true
		}
	}
	return false
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
	h.nodesMu.RLock()
	conn, ok := h.nodes[msg.Node]
	h.nodesMu.RUnlock()
	if !ok {
		// Do not echo the client-supplied node ID in the error: a careless
		// JS consumer rendering the field via innerHTML would turn a crafted
		// node value into reflected XSS. Log internally for operator triage.
		slog.Debug("ws subscribe: unknown node", "node", msg.Node)
		c.SendJSON(node.ServerMsg{Type: "error", Key: msg.Key, Error: "unknown node"})
		return
	}
	conn.Subscribe(c, msg.Key, msg.After)
}

func (h *Hub) handleRemoteUnsubscribe(c *wsClient, msg node.ClientMsg) {
	if !isValidNodeID(msg.Node) {
		// Mirror the success shape so slow clients can drop state even when
		// the node ID is malformed — behaviour equivalent to "no such node".
		c.SendJSON(node.ServerMsg{Type: "unsubscribed", Key: msg.Key})
		return
	}
	h.nodesMu.RLock()
	conn, ok := h.nodes[msg.Node]
	h.nodesMu.RUnlock()
	if !ok {
		c.SendJSON(node.ServerMsg{Type: "unsubscribed", Key: msg.Key, Node: msg.Node})
		return
	}
	conn.Unsubscribe(c, msg.Key)
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
