// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     subscriber block (clients / connCount / subscriberCount /
//	            clientWG / wsAuthLimiter / wsUpgradeLimiter / upgrader /
//	            dashTokenHash / cookieMAC / trustedProxy)
//	READS:      shared deps block (read-only after ctor)
//	READS-ALSO: send block (sendClosed only — close client must drain
//	            pending sends; lifecycle-coordinated)
package server

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// initialHistoryDiskTimeout bounds the disk-tier walk EventLastNVisibleCtx may
// perform during a subscribe handshake (reverse-scanning JSONL when the ring is
// all internal events) so a slow filesystem cannot stall the WS first frame.
// On timeout the reader returns what it gathered (memory tier at minimum);
// the dashboard's auto-page-back covers the rest.
const initialHistoryDiskTimeout = 2 * time.Second

// initialVisibleHistory reads the visible-aware initial history slice for a
// subscribe handshake, bounding the disk-tier fallback with a deadline derived
// from the Hub context so shutdown still cancels it promptly. The returned
// hasMore reports whether any event strictly older than the slice still exists
// (ring or disk); the dashboard mounts its "load earlier" affordance off this
// flag instead of guessing from the returned slice length.
func (h *Hub) initialVisibleHistory(sess *session.ManagedSession, limit int) ([]clievent.EventEntry, bool) {
	target := limit
	if target <= 0 || target > session.DefaultVisibleTarget {
		// The client's INITIAL_HISTORY_LIMIT (100) is a page-size hint, not a
		// visible-bubble target; clamp the visible goal to DefaultVisibleTarget
		// so we don't over-walk disk chasing 100 visible bubbles.
		target = session.DefaultVisibleTarget
	}
	// maxTotal=0 → the reader uses its own ceiling (ring size). Passing `limit`
	// would cap the walk at the page-size hint and strand visible bubbles
	// beyond it under an internal flood.
	ctx, cancel := context.WithTimeout(h.ctx, initialHistoryDiskTimeout)
	defer cancel()
	return sess.EventInitialPageCtx(ctx, target, 0)
}

// initialHasMorePtr returns a non-nil *bool only for the initial-page history
// frame (msg.Limit>0, no After cursor) — the one branch that computed hasMore.
// After-cursor catch-up and legacy full-history frames return nil so the
// has_more field is omitted and the client keeps its length-heuristic fallback
// rather than seeing a meaningless false.
func initialHasMorePtr(msg node.ClientMsg, hasMore bool) *bool {
	if msg.After > 0 || msg.Limit <= 0 {
		return nil
	}
	return &hasMore
}

// maxSubscribersPerKey caps distinct WS connections subscribed to one session
// key; otherwise a single token can multiply every event fan-out by N. 20 is
// comfortably above the realistic multi-tab / multi-device working set.
const maxSubscribersPerKey = 20

// maxSubscriptionsPerClient caps distinct session keys one WS connection may
// subscribe to, bounding per-client memory and enumeration fan-out. 50 covers
// the dashboard working set with headroom; clients hitting it should
// re-architect rather than have the cap raised.
const maxSubscriptionsPerClient = 50

func (h *Hub) handleSubscribe(c *wsClient, msg node.ClientMsg) {
	key := msg.Key
	if key == "" {
		c.SendJSON(wsproto.NewError(wsproto.Error{Error: "key is required"}))
		return
	}
	// Same gate as the HTTP session handlers: without it a WS client can post a
	// multi-KB key with C1 controls / bidi chars that reach slog attrs, persist
	// in c.subscriptions and land in sessions.json. ValidateSessionKey also
	// caps length at MaxSessionKeyBytes.
	if err := session.ValidateSessionKey(key); err != nil {
		c.SendJSON(wsproto.NewError(wsproto.Error{Error: "invalid key"}))
		return
	}

	// Remote node delegation
	if msg.Node != "" && msg.Node != "local" {
		h.handleRemoteSubscribe(c, msg)
		return
	}

	// Per-connection cap; reserve the slot atomically under h.mu so two
	// concurrent subscribes at N-1 cannot both pass. The reservation is a
	// nil-unsub placeholder that completeSubscribe overwrites; every path
	// between here and there either writes a real value or clears it.
	h.mu.Lock()
	if _, alreadySub := c.subscriptions[key]; !alreadySub && len(c.subscriptions) >= maxSubscriptionsPerClient {
		h.mu.Unlock()
		c.SendJSON(wsproto.NewError(wsproto.Error{Key: key, Error: "too many subscriptions"}))
		return
	}
	// Per-session-key cap across all connections via h.subscriberCount[key],
	// maintained alongside c.subscriptions mutations under h.mu (#716).
	_, alreadySub := c.subscriptions[key]
	// Gate on the explicit h.enforceCaps bool, not `subscriberCount == nil`:
	// NewHub sets both; hand-rolled test hubs leave both zero and skip caps,
	// and eagerly initialising the map must not silently activate them (#1401).
	if !alreadySub && h.enforceCaps && h.subscriberCount[key] >= maxSubscribersPerKey {
		h.mu.Unlock()
		c.SendJSON(wsproto.NewError(wsproto.Error{Key: key, Error: "too many subscribers for key"}))
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
		// Keep the lock-free mirror read by singleSubscriber in step (#1522).
		h.setSubscriberCountFast(key, h.subscriberCount[key])
	}
	h.mu.Unlock()

	sess := h.router.SessionFor(key)
	if sess == nil && h.scheduler != nil && h.scheduler.EnsureStub(key) {
		// Cron stubs are torn down by sidebar "×". Rebuild lazily on click
		// so the user doesn't have to wait for the next scheduled tick to
		// re-open the panel. EnsureStub is a no-op for non-cron keys.
		sess = h.router.SessionFor(key)
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

	c.SendJSON(wsproto.NewError(wsproto.Error{Key: key, Error: "session not found"}))
}

// completeSubscribe finishes a subscription once a valid session is available.
func (h *Hub) completeSubscribe(c *wsClient, key string, msg node.ClientMsg, sess *session.ManagedSession) {
	if !sess.HasProcess() {
		// No process yet (suspended/resuming): send persisted history plus
		// "subscribed" so the client clears _pendingSubscribeKey and can
		// re-subscribe when a process appears. Release the reserved slot since
		// there is no real unsub to install.
		h.mu.Lock()
		if _, ok := c.subscriptions[key]; ok {
			delete(c.subscriptions, key)
			h.decSubscriberCountLocked(key)
		}
		h.mu.Unlock()

		snap := sess.Snapshot()
		c.SendJSON(wsproto.NewSubscribed(wsproto.Subscribed{Key: key, State: snap.State, Reason: "suspended"}))

		var entries []clievent.EventEntry
		var hasMore bool
		switch {
		case msg.After > 0:
			entries = entriesSinceReconnect(sess, msg.After)
		case msg.Limit > 0:
			// Visible-aware initial page: a suspended session whose persisted
			// tail is all internal events (parallel agent team) would otherwise
			// hand the dashboard a page that renders to the blank placeholder.
			entries, hasMore = h.initialVisibleHistory(sess, msg.Limit)
		default:
			entries = sess.EventLastN(0)
		}
		if len(entries) > 0 || emptyInitialHistoryWanted(msg, snap.State) { // #2432
			c.SendJSON(wsproto.NewHistory(wsproto.History{Key: key, Events: nonNilEntries(entries), HasMore: initialHasMorePtr(msg, hasMore), Initial: true}))
		}
		slog.Debug("completeSubscribe: no process, sent persisted history", "key", key, "entries", len(entries), "has_more", hasMore)
		return
	}
	// Fast-fail if Shutdown already fired: SubscribeEvents would register on an
	// EventLog being torn down and the unsub may never run.
	if h.ctx.Err() != nil {
		h.mu.Lock()
		if _, ok := c.subscriptions[key]; ok {
			delete(c.subscriptions, key)
			h.decSubscriberCountLocked(key)
		}
		h.mu.Unlock()
		return
	}
	// Idempotent: the Linker's OnResolve list accumulates per re-subscribe, but
	// ensureTailer is guarded by the (key, taskID) map so extra callbacks are
	// cheap no-ops. Wiring here (not router.spawnSession) keeps server/cli
	// layering clean.
	h.maybeWireLinkerTailer(key, sess)
	notify, unsub := sess.SubscribeEvents()

	h.mu.Lock()
	// Re-check ctx under the lock: Shutdown's sequence is cancel() -> h.mu.Lock()
	// -> iterate subscriptions, so ctx.Err() set here means Shutdown is
	// mid-flight; decline to start a new pushLoop.
	if c.subscriptions == nil || h.ctx.Err() != nil {
		// Release the placeholder reservation, symmetric with the two sibling early
		// returns; otherwise the placeholder + inflated subscriberCount[key] would
		// count toward the caps until the client disconnects (#1806).
		if c.subscriptions != nil {
			if _, ok := c.subscriptions[key]; ok {
				delete(c.subscriptions, key)
				h.decSubscriberCountLocked(key)
			}
		}
		h.mu.Unlock()
		unsub()
		return
	}
	c.subscriptions[key] = unsub
	c.subGen[key]++
	gen := c.subGen[key]
	// The key is live again, so any pending reclamation marker is stale; a sweep
	// mid-life would delete subGen[key] under an active subscription and break
	// resubscribeEvents' takeover detection.
	c.clearSubGenReleasable(key)
	// Add to clientWG BEFORE releasing h.mu. Shutdown walks h.clients under
	// h.mu to close conns, then calls clientWG.Wait; if we Add(1) after
	// releasing here, Shutdown's Wait can return before the eventPushLoop
	// goroutine ever starts, and the goroutine can then touch torn-down state.
	h.clientWG.Add(1)
	h.mu.Unlock()

	// Balance clientWG.Add(1) if we never reach the goroutine spawn: anything
	// between here and `spawned = true` can panic, and readPump's recover then
	// unwinds via unregister without the goroutine's deferred Done(), hanging
	// Hub.Shutdown's clientWG.Wait().
	spawned := false
	defer func() {
		if !spawned {
			h.clientWG.Done()
		}
	}()

	snap := sess.Snapshot()

	var entries []clievent.EventEntry
	var hasMore bool
	switch {
	case msg.After > 0:
		entries = entriesSinceReconnect(sess, msg.After)
	case msg.Limit > 0:
		// Initial subscribe asks for the last `limit` events only; clients page
		// back via HTTP /api/sessions/events?before=. Visible-aware: when internal
		// tool_use / task_progress entries fill the tail, EventInitialPageCtx keeps
		// walking (ring, then disk) until the page carries real chat bubbles and
		// reports whether older history exists for "load earlier".
		entries, hasMore = h.initialVisibleHistory(sess, msg.Limit)
	default:
		// Legacy path: send everything the log remembers. Kept so older
		// clients (and the node-to-node relay) still see full history.
		entries = sess.EventLastN(0)
	}

	slog.Debug("completeSubscribe: sending history", "key", key, "entries", len(entries), "state", snap.State, "has_more", hasMore)
	c.SendJSON(wsproto.NewSubscribed(wsproto.Subscribed{Key: key, State: snap.State}))

	csr := cli.NewSinceCursor() // #2402: Advance below seeds the pushLoop watermark
	if len(entries) > 0 {
		// Pooled marshal: initial history payloads can be hundreds of KB.
		hm := initialHasMorePtr(msg, hasMore)
		if data, err := marshalPooled(wsproto.NewHistory(wsproto.History{Key: key, Events: entries, HasMore: hm, Initial: true})); err == nil {
			c.SendRaw(data)
		} else {
			slog.Warn("history marshal failed, falling back", "err", err, "key", key)
			c.SendJSON(wsproto.NewHistory(wsproto.History{Key: key, Events: entries, HasMore: hm, Initial: true}))
		}
		csr.Advance(entries)
	} else if emptyInitialHistoryWanted(msg, snap.State) {
		// Empty Initial frame consumes the client's _initialSubscribe flag so
		// the pane shows a placeholder instead of staying blank (#2432).
		c.SendJSON(wsproto.NewHistory(wsproto.History{Key: key, Events: []clievent.EventEntry{}, HasMore: initialHasMorePtr(msg, hasMore), Initial: true}))
	}

	spawned = true
	go func() {
		defer h.clientWG.Done()
		h.eventPushLoop(c, key, gen, notify, sess, csr)
	}()
}

func (h *Hub) handleUnsubscribe(c *wsClient, msg node.ClientMsg) {
	key := msg.Key

	// Same gate as handleSubscribe: a crafted key with C1 / bidi / non-UTF-8
	// bytes would land in the echoed "unsubscribed" reply and log attrs. Gate
	// BEFORE remote delegation since handleRemoteUnsubscribe reads msg.Key too.
	if err := session.ValidateSessionKey(key); err != nil {
		c.SendJSON(wsproto.NewError(wsproto.Error{Error: "invalid key"}))
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
		// Keep c.subGen[key] intact: a stale eventPushLoop may still be parked in
		// resubscribeEvents (up to 60s), and a fresh subscribe resetting the
		// counter to 1 would let its gen=1 silently resume. Mark for delayed
		// reclamation instead (75s retention > the 60s park) so long-lived
		// clients flapping through many panels do not grow c.subGen unbounded.
		nowNanos := time.Now().UnixNano()
		c.markSubGenReleasable(key, nowNanos)
		c.sweepSubGenExpiredLocked(nowNanos)
		// If we were the last subscriber (counter entry deleted → 0), drop the
		// cached "history" marshal slot so its payload is GC'd (#513).
		dropMarshalCache = !h.enforceCaps || h.subscriberCount[key] == 0
	}
	h.mu.Unlock()
	if dropMarshalCache && h.historyMarshalCache != nil {
		h.historyMarshalCache.drop(key)
	}
	c.SendJSON(wsproto.NewUnsubscribed(wsproto.Unsubscribed{Key: key}))
}

// decSubscriberCountLocked decrements h.subscriberCount[key] and removes the
// entry once it hits zero, so the map size mirrors the number of distinct
// keys that currently have at least one subscriber. Caller MUST hold h.mu.
//
// The nil check is retained alongside enforceCaps so a hand-rolled test
// fixture that populated the map without flipping enforceCaps still mutates
// it correctly — the gate decides whether to ENFORCE the cap, not whether
// the map exists (#1401).
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
// subscriberCountFast map so singleSubscriber can read the per-key population
// without h.mu. Caller MUST hold h.mu. Values are *atomic.Int32 so lock-free
// readers get a consistent load while a writer updates another key (#1522).
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
	// via ANSI/newline bytes in the attacker-controlled Node field.
	if !isValidNodeID(msg.Node) {
		c.SendJSON(wsproto.NewError(wsproto.Error{Key: msg.Key, Error: "unknown node"}))
		return
	}
	conn, ok := h.lookupNode(msg.Node)
	if !ok {
		// Do not echo the client-supplied node ID in the error: a careless
		// JS consumer rendering the field via innerHTML would turn a crafted
		// node value into reflected XSS. Log internally for operator triage.
		slog.Debug("ws subscribe: unknown node", "node", msg.Node)
		c.SendJSON(wsproto.NewError(wsproto.Error{Key: msg.Key, Error: "unknown node"}))
		return
	}
	// Subscribe only needs the pub-sub role; narrow to node.NodeSubscriber (#435).
	var sub node.NodeSubscriber = conn
	sub.Subscribe(c, msg.Key, msg.After)
}

func (h *Hub) handleRemoteUnsubscribe(c *wsClient, msg node.ClientMsg) {
	if !isValidNodeID(msg.Node) {
		// Mirror the success shape so slow clients can drop state even when
		// the node ID is malformed — behaviour equivalent to "no such node".
		c.SendJSON(wsproto.NewUnsubscribed(wsproto.Unsubscribed{Key: msg.Key}))
		return
	}
	conn, ok := h.lookupNode(msg.Node)
	if !ok {
		c.SendJSON(wsproto.NewUnsubscribed(wsproto.Unsubscribed{Key: msg.Key, Node: msg.Node}))
		return
	}
	// Unsubscribe only needs the pub-sub role (#435).
	var sub node.NodeSubscriber = conn
	sub.Unsubscribe(c, msg.Key)
}

// PurgeNodeSubscriptions notifies all browser clients that a node disconnected,
// so they can deselect stale sessions.
func (h *Hub) PurgeNodeSubscriptions(nodeID string) {
	data, err := marshalPooled(wsproto.NewError(wsproto.Error{Node: nodeID, Error: "node disconnected"}))
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
}
