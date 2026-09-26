package server

import (
	"context"
	"log/slog"
	"time"

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

func (h *Hub) handleSubscribe(c *wsClient, msg node.ClientMsg) {
	key := msg.Key
	if key == "" {
		c.SendJSON(wsproto.NewError(wsproto.Error{Error: "key is required"}))
		return
	}
	// Same gate as the HTTP session handlers: without it a WS client can post a
	// multi-KB key with C1 controls / bidi chars that reach slog attrs, persist
	// in the subscriber registry and land in sessions.json. ValidateSessionKey also
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

	// Reserve the slot before the session lookup so two concurrent
	// subscribes at a cap cannot both pass; every path below either installs
	// the real unsub or releases the slot.
	switch h.subs.reserve(c, key) {
	case reserveGone:
		return
	case reserveClientFull:
		c.SendJSON(wsproto.NewError(wsproto.Error{Key: key, Error: "too many subscriptions"}))
		return
	case reserveKeyFull:
		c.SendJSON(wsproto.NewError(wsproto.Error{Key: key, Error: "too many subscribers for key"}))
		return
	}

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

	h.subs.release(c, key)

	c.SendJSON(wsproto.NewError(wsproto.Error{Key: key, Error: "session not found"}))
}

// completeSubscribe finishes a subscription once a valid session is available.
func (h *Hub) completeSubscribe(c *wsClient, key string, msg node.ClientMsg, sess *session.ManagedSession) {
	if !sess.HasProcess() {
		// No process yet (suspended/resuming): send persisted history plus
		// "subscribed" so the client clears _pendingSubscribeKey and can
		// re-subscribe when a process appears. Release the reserved slot since
		// there is no real unsub to install.
		h.subs.release(c, key)

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
		h.subs.release(c, key)
		return
	}
	// Idempotent: the Linker's OnResolve list accumulates per re-subscribe, but
	// ensureTailer is guarded by the (key, taskID) map so extra callbacks are
	// cheap no-ops. Wiring here (not router.spawnSession) keeps server/cli
	// layering clean.
	h.maybeWireLinkerTailer(key, sess)
	notify, unsub := sess.SubscribeEvents()

	// admit re-checks ctx under the registry's lock: Shutdown cancels ctx
	// before it drains the registry, so a subscription installed here is
	// either seen by the drain or declined. clientWG.Add(1) happens inside the
	// same critical section, so Shutdown's Wait cannot return before the
	// eventPushLoop below starts.
	gen, ok := h.subs.install(c, key, unsub, func() bool {
		if h.ctx.Err() != nil {
			return false
		}
		h.clientWG.Add(1)
		return true
	})
	if !ok {
		unsub()
		return
	}

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

	csr := clievent.NewSinceCursor() // #2402: Advance below seeds the pushLoop watermark
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

	// The last subscriber leaving drops the cached "history" marshal slot so
	// its payload is GC'd.
	dropMarshalCache := h.subs.unsubscribe(c, key, time.Now().UnixNano())
	if dropMarshalCache {
		h.historyMarshalCache.drop(key)
	}
	c.SendJSON(wsproto.NewUnsubscribed(wsproto.Unsubscribed{Key: key}))
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
