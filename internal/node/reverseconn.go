package node

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/cli"
)

// maxReverseRPCResponseBytes caps the size of a single reverse-RPC response
// payload before the primary node json.Unmarshals it. The websocket read
// limit in reverseserver.go is 16 MiB to accommodate legitimate batch
// results (full project dumps, large event ranges), but unmarshal targets
// like []map[string]any allocate one map per nested object and are an easy
// heap-exhaustion primitive from a compromised node. 2 MiB is ~10x larger
// than the worst observed legitimate response. R58-SEC-M2.
const maxReverseRPCResponseBytes = 2 << 20 // 2 MiB

// maxPendingReverseRPCs caps the in-flight reverse-RPC request map size
// per ReverseConn. Every `rpc()` call inserts one entry into `c.pending`;
// callers use 10s context timeouts so entries naturally drain, but a
// hung-but-TCP-alive peer (half-open TCP, compromised node that ACKs the
// handshake and goes silent) lets polling dashboards (CacheManager.Refresh,
// /api/sessions fanout) accumulate entries before readLoop eventually
// detects the dead connection. Capping at 256 keeps the memory bounded
// while comfortably exceeding realistic concurrent-RPC fan-out (typical
// dashboard poll drives ≤10 concurrent FetchXxx). R59-SEC-M1.
const maxPendingReverseRPCs = 256

// maxPushedNodeStringBytes caps the length of free-form string fields that
// arrive in pushed reverse-node messages (session_state.Reason,
// subscribe_error.Error). These fields skip the rpc() size gate because
// they're broadcast directly to every subscribed browser client — an
// unbounded push from a compromised node can fill each client's 256-slot
// send channel and trigger drops, degrading dashboard UX. 512 bytes fits
// any realistic operator-facing message without enabling abuse. R61-SEC-9.
const maxPushedNodeStringBytes = 512

// maxPushedHistoryEvents caps the length of the `events` array in pushed
// `events` messages from a reverse node. The reverse WS read limit is 16 MB
// (reverseserver.go after auth), and `broadcastToSubs` fan-outs to every
// subscribed browser client with a 256-slot send channel — a compromised
// node can push a 16 MB events array and amplify it N× across connected
// tabs, filling every send channel and triggering drops. 500 matches the
// dashboard's `maxEventsPageLimit` and any local EventLog ring size, so
// legitimate history replays are never truncated. R67-SEC-3.
const maxPushedHistoryEvents = 500

type reverseResult struct {
	result json.RawMessage
	err    error
}

// ReverseConn is the primary-side representation of a reverse-connected node.
// It implements Conn by forwarding calls over the reverse WebSocket connection.
type ReverseConn struct {
	id          string
	displayName string
	remoteAddr  string

	writeMu sync.Mutex
	conn    *websocket.Conn

	pendingMu sync.Mutex
	pending   map[string]chan reverseResult // req_id → waiting caller
	reqSeq    atomic.Int64

	subMu sync.Mutex
	subs  map[string][]EventSink // session key → local browser clients

	statusMu sync.RWMutex
	status   string // "ok" | "connecting" | "error"

	done    chan struct{}
	closed  bool
	closeMu sync.Mutex

	// baseCtx is the parent context for in-flight Subscribe history fetches
	// (and any future per-connection RPCs that should abort on disconnect).
	// baseCancel fires on Close()/markDisconnected() so timeout contexts
	// derived from baseCtx unwind without needing a separate "cancel on
	// c.done" watcher goroutine per RPC. H7 (Round 163).
	baseCtx    context.Context
	baseCancel context.CancelFunc
}

func newReverseConn(id, displayName, remoteAddr string, conn *websocket.Conn) *ReverseConn {
	baseCtx, baseCancel := context.WithCancel(context.Background())
	return &ReverseConn{
		id:          id,
		displayName: displayName,
		remoteAddr:  remoteAddr,
		conn:        conn,
		pending:     make(map[string]chan reverseResult),
		subs:        make(map[string][]EventSink),
		status:      "ok",
		done:        make(chan struct{}),
		baseCtx:     baseCtx,
		baseCancel:  baseCancel,
	}
}

func (c *ReverseConn) NodeID() string      { return c.id }
func (c *ReverseConn) DisplayName() string { return c.displayName }
func (c *ReverseConn) RemoteAddr() string  { return c.remoteAddr }

func (c *ReverseConn) Status() string {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.status
}

func (c *ReverseConn) Close() {
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return
	}
	c.closed = true
	close(c.done)
	conn := c.conn
	c.closeMu.Unlock()

	// Cancel baseCtx so any in-flight Subscribe history fetches unwind
	// without relying on an auxiliary watcher goroutine per RPC. Safe to
	// call multiple times; markDisconnected may also fire it.
	c.baseCancel()
	conn.Close()
}

func (c *ReverseConn) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// If SetWriteDeadline fails (conn half-closed / closed), return the
	// error instead of issuing a deadline-less WriteJSON that can block
	// until TCP keepalive expires; the caller's error path will re-dial.
	if err := c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	return c.conn.WriteJSON(v)
}

// rpc sends a request to the remote node and waits for the response.
func (c *ReverseConn) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	reqID := strconv.FormatInt(c.reqSeq.Add(1), 10)
	ch := make(chan reverseResult, 1)

	marshaledParams, err := marshalParams(params)
	if err != nil {
		return nil, err
	}

	c.pendingMu.Lock()
	// Guard against pending-map growth when the peer is slow / hung. See
	// maxPendingReverseRPCs doc for rationale. Fail fast so the caller's
	// 10s timeout isn't wasted waiting for a response that will never come.
	if len(c.pending) >= maxPendingReverseRPCs {
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("reverse rpc: too many pending requests (%d)", maxPendingReverseRPCs)
	}
	c.pending[reqID] = ch
	c.pendingMu.Unlock()

	if err := c.writeJSON(ReverseMsg{
		Type:   "request",
		ReqID:  reqID,
		Method: method,
		Params: marshaledParams,
	}); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, reqID)
		c.pendingMu.Unlock()
		return nil, err
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		// Gate response size before returning to callers that will
		// json.Unmarshal into nested map[string]any trees. A compromised
		// reverse node could otherwise send 16 MB of maximally-nested JSON
		// (the ws read limit set in reverseserver.go) and force hundreds
		// of thousands of heap allocations on the primary. 2 MiB comfortably
		// exceeds realistic FetchSessions/FetchEvents payloads and localizes
		// the exhaustion-defense at the single RPC choke point rather than
		// repeating the guard at every FetchXxx caller. R58-SEC-M2.
		if len(res.result) > maxReverseRPCResponseBytes {
			return nil, fmt.Errorf("reverse rpc response too large (%d > %d bytes)", len(res.result), maxReverseRPCResponseBytes)
		}
		return res.result, nil
	case <-ctx.Done():
		// Critical: remove pending to avoid goroutine leak when response arrives late
		c.pendingMu.Lock()
		delete(c.pending, reqID)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		c.pendingMu.Lock()
		delete(c.pending, reqID)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("node %s disconnected", c.id)
	}
}

func marshalParams(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshalParams: %w", err)
	}
	return b, nil
}

func (c *ReverseConn) FetchSessions(ctx context.Context) ([]map[string]any, error) {
	raw, err := c.rpc(ctx, "fetch_sessions", nil)
	if err != nil {
		return nil, err
	}
	var result []map[string]any
	return result, json.Unmarshal(raw, &result)
}

func (c *ReverseConn) FetchProjects(ctx context.Context) ([]map[string]any, error) {
	raw, err := c.rpc(ctx, "fetch_projects", nil)
	if err != nil {
		return nil, err
	}
	var result []map[string]any
	return result, json.Unmarshal(raw, &result)
}

func (c *ReverseConn) FetchDiscovered(ctx context.Context) ([]map[string]any, error) {
	raw, err := c.rpc(ctx, "fetch_discovered", nil)
	if err != nil {
		return nil, err
	}
	var result []map[string]any
	return result, json.Unmarshal(raw, &result)
}

func (c *ReverseConn) FetchDiscoveredPreview(ctx context.Context, sessionID string) ([]cli.EventEntry, error) {
	raw, err := c.rpc(ctx, "fetch_discovered_preview", map[string]string{"session_id": sessionID})
	if err != nil {
		return nil, err
	}
	var result []cli.EventEntry
	return result, json.Unmarshal(raw, &result)
}

func (c *ReverseConn) FetchEvents(ctx context.Context, key string, after int64) ([]cli.EventEntry, error) {
	raw, err := c.rpc(ctx, "fetch_events", map[string]any{"key": key, "after": after})
	if err != nil {
		return nil, err
	}
	var result []cli.EventEntry
	return result, json.Unmarshal(raw, &result)
}

func (c *ReverseConn) Send(ctx context.Context, key, text, workspace string) error {
	params := map[string]string{"key": key, "text": text}
	if workspace != "" {
		params["workspace"] = workspace
	}
	_, err := c.rpc(ctx, "send", params)
	return err
}

func (c *ReverseConn) ProxyTakeover(ctx context.Context, pid int, sessionID, cwd string, procStart uint64) (string, error) {
	raw, err := c.rpc(ctx, "takeover", map[string]any{
		"pid": pid, "session_id": sessionID, "cwd": cwd, "proc_start_time": procStart,
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		Key string `json:"key"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &resp); err != nil {
			return "", fmt.Errorf("takeover response: %w", err)
		}
	}
	return resp.Key, nil
}

func (c *ReverseConn) ProxyCloseDiscovered(ctx context.Context, pid int, sessionID, cwd string, procStart uint64) error {
	_, err := c.rpc(ctx, "close_discovered", map[string]any{
		"pid": pid, "session_id": sessionID, "cwd": cwd, "proc_start_time": procStart,
	})
	return err
}

func (c *ReverseConn) ProxyRestartPlanner(ctx context.Context, projectName string) error {
	_, err := c.rpc(ctx, "restart_planner", map[string]string{"project_name": projectName})
	return err
}

func (c *ReverseConn) ProxyUpdateConfig(ctx context.Context, projectName string, cfg json.RawMessage) error {
	_, err := c.rpc(ctx, "update_config", map[string]any{"project_name": projectName, "config": cfg})
	return err
}

func (c *ReverseConn) ProxySetFavorite(ctx context.Context, projectName string, favorite bool) error {
	_, err := c.rpc(ctx, "set_favorite", map[string]any{"project_name": projectName, "favorite": favorite})
	return err
}

func (c *ReverseConn) ProxyRemoveSession(ctx context.Context, key string) (bool, error) {
	raw, err := c.rpc(ctx, "remove_session", map[string]string{"key": key})
	if err != nil {
		return false, err
	}
	var resp struct {
		Removed bool `json:"removed"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &resp); err != nil {
			return false, fmt.Errorf("remove_session response: %w", err)
		}
	}
	return resp.Removed, nil
}

func (c *ReverseConn) ProxySetSessionLabel(ctx context.Context, key, label string) (bool, error) {
	raw, err := c.rpc(ctx, "set_session_label", map[string]string{"key": key, "label": label})
	if err != nil {
		return false, err
	}
	var resp struct {
		Updated bool `json:"updated"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &resp); err != nil {
			return false, fmt.Errorf("set_session_label response: %w", err)
		}
	}
	return resp.Updated, nil
}

func (c *ReverseConn) ProxyInterruptSession(ctx context.Context, key string) (bool, error) {
	raw, err := c.rpc(ctx, "interrupt_session", map[string]string{"key": key})
	if err != nil {
		return false, err
	}
	var resp struct {
		Interrupted bool `json:"interrupted"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &resp); err != nil {
			return false, fmt.Errorf("interrupt_session response: %w", err)
		}
	}
	return resp.Interrupted, nil
}

func (c *ReverseConn) Subscribe(cl EventSink, key string, after int64) {
	c.subMu.Lock()
	alreadySub := len(c.subs[key]) > 0
	c.subs[key] = append(c.subs[key], cl)
	c.subMu.Unlock()

	if alreadySub {
		// Additional subscriber: send history via RPC (non-blocking).
		// Derive the timeout from baseCtx so a connection drop mid-fetch
		// cancels the RPC through ctx cancellation — no auxiliary
		// watcher goroutine needed. H7 (Round 163).
		go func() {
			ctx, cancel := context.WithTimeout(c.baseCtx, 5*time.Second)
			defer cancel()

			entries, err := c.FetchEvents(ctx, key, after)
			if err != nil {
				return
			}
			cl.SendJSON(ServerMsg{Type: "subscribed", Key: key, Node: c.id})
			if len(entries) > 0 {
				cl.SendJSON(ServerMsg{Type: "history", Key: key, Node: c.id, Events: entries})
			}
		}()
	} else {
		// First subscriber: tell remote to start pushing events.
		// Subscriber was already added above so readLoop can deliver events
		// arriving immediately after the write. On failure, roll back.
		if err := c.writeJSON(ReverseMsg{Type: "subscribe", Key: key, After: after}); err != nil {
			slog.Warn("reverse subscribe write failed", "node", c.id, "key", key, "err", err)
			c.subMu.Lock()
			removeSub(c.subs, key, cl)
			c.subMu.Unlock()
			return
		}
		// Also fetch persisted history synchronously so that ready sessions
		// (no live process) still deliver the event log. The remote
		// connector's streamEvents only pushes on EventLog Append, which
		// never fires for a process-less session — without this fetch the
		// dashboard would subscribe and never receive any history.
		//
		// Run in a goroutine so the hub's readPump is not blocked waiting
		// for the RPC; the "subscribed" message from the remote arrives via
		// readLoop and the history message from here can arrive in either
		// order. Order doesn't matter for the client: onHistory merges by
		// key/time, and the initial page render is keyed on history arrival
		// not on subscribed arrival.
		//
		// Derive the timeout from baseCtx so a connection drop cancels the
		// RPC through ctx cancellation — no auxiliary watcher goroutine
		// needed. H7 (Round 163).
		go func() {
			ctx, cancel := context.WithTimeout(c.baseCtx, 5*time.Second)
			defer cancel()

			entries, err := c.FetchEvents(ctx, key, after)
			if err != nil {
				slog.Debug("reverse first-subscribe fetch events failed", "node", c.id, "key", key, "err", err)
				return
			}
			if len(entries) > 0 {
				cl.SendJSON(ServerMsg{Type: "history", Key: key, Node: c.id, Events: entries})
			}
		}()
	}
}

// RefreshSubscription forces the remote to re-create the streamEvents
// goroutine for key, even if a subscription already exists. This is
// needed after a remote send because the previous process (and its
// streamEvents) may have died since the last subscribe.
//
// Best-effort: a concurrent Unsubscribe between the check and the write
// may send a redundant subscribe, but the remote handles it gracefully.
func (c *ReverseConn) RefreshSubscription(key string) {
	c.subMu.Lock()
	hasSubs := len(c.subs[key]) > 0
	c.subMu.Unlock()
	if hasSubs {
		c.writeJSON(ReverseMsg{Type: "subscribe", Key: key}) //nolint
	}
}

func (c *ReverseConn) Unsubscribe(cl EventSink, key string) {
	c.subMu.Lock()
	empty := removeSub(c.subs, key, cl)
	c.subMu.Unlock()

	if empty {
		c.writeJSON(ReverseMsg{Type: "unsubscribe", Key: key}) //nolint
	}
	cl.SendJSON(ServerMsg{Type: "unsubscribed", Key: key, Node: c.id})
}

func (c *ReverseConn) RemoveClient(cl EventSink) {
	c.subMu.Lock()
	emptyKeys := removeSubAll(c.subs, cl)
	c.subMu.Unlock()

	for _, key := range emptyKeys {
		c.writeJSON(ReverseMsg{Type: "unsubscribe", Key: key}) //nolint
	}
}

// subSnapPool reuses the subscriber-snapshot slice that broadcastToSubs
// builds on every remote event. The readLoop fires broadcastToSubs for
// every incoming `event`/`events`/`session_state` message from a remote
// node — on a running Claude turn that is dozens of events per second.
// A sync.Pool avoids the per-call make([]EventSink, N) alloc. R61-PERF-3.
// Mirrors the broadcastClientSnapPool pattern in server/wshub.go.
var subSnapPool = sync.Pool{
	New: func() any {
		s := make([]EventSink, 0, 16)
		return &s
	},
}

// broadcastToSubs snapshots subscribers for key, marshals out, and sends to all.
// If deleteKey is true, the key is removed from the subscription map.
func (c *ReverseConn) broadcastToSubs(key string, out ServerMsg, deleteKey bool) {
	c.subMu.Lock()
	subs := c.subs[key]
	snapPtr := subSnapPool.Get().(*[]EventSink)
	clients := *snapPtr
	if cap(clients) < len(subs) {
		clients = make([]EventSink, len(subs))
	} else {
		clients = clients[:len(subs)]
	}
	copy(clients, subs)
	if deleteKey {
		delete(c.subs, key)
	}
	c.subMu.Unlock()

	data, err := json.Marshal(out)
	if err == nil {
		for _, cl := range clients {
			cl.SendRaw(data)
		}
	}

	// Clear pointers so disconnected EventSinks can be GC'd instead of being
	// pinned in the pooled backing array until the next Get replaces them.
	for i := range clients {
		clients[i] = nil
	}
	// Drop oversized snapshots so the pool never pins an arbitrarily large
	// backing array (e.g. after a brief spike to hundreds of subscribers).
	if cap(clients) <= 256 {
		*snapPtr = clients[:0]
		subSnapPool.Put(snapPtr)
	}
}

func (c *ReverseConn) readLoop() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in reverse readLoop", "node", c.id, "panic", r)
		}
	}()
	defer c.markDisconnected()

	// The connector sends WebSocket pings every 30s (upstream/connector.go).
	// Set a 90s read deadline so we detect silent disconnections (NAT timeout,
	// crash without clean close) rather than blocking forever on ReadJSON.
	const reverseReadTimeout = 90 * time.Second
	c.conn.SetReadDeadline(time.Now().Add(reverseReadTimeout))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(reverseReadTimeout))
		return nil
	})
	c.conn.SetPingHandler(func(appData string) error {
		c.conn.SetReadDeadline(time.Now().Add(reverseReadTimeout))
		return c.conn.WriteControl(
			websocket.PongMessage, []byte(appData), time.Now().Add(time.Second),
		)
	})

	for {
		var msg ReverseMsg
		if err := c.conn.ReadJSON(&msg); err != nil {
			slog.Debug("reverse node disconnected", "node", c.id, "err", err)
			return
		}
		c.conn.SetReadDeadline(time.Now().Add(reverseReadTimeout))

		switch msg.Type {
		case "response":
			c.pendingMu.Lock()
			ch, ok := c.pending[msg.ReqID]
			if ok {
				delete(c.pending, msg.ReqID)
			}
			c.pendingMu.Unlock()
			if ok {
				var err error
				if msg.Error != "" {
					err = fmt.Errorf("node %s: %s", c.id, msg.Error)
				}
				ch <- reverseResult{msg.Result, err}
			}

		case "event":
			c.broadcastToSubs(msg.Key, ServerMsg{Type: "event", Key: msg.Key, Event: msg.Event, Node: c.id}, false)

		case "events":
			// Cap fan-out size to prevent a compromised node amplifying a
			// 16 MB history push N× across subscribed browser tabs. Keep
			// the tail (most recent) to preserve "last N events" semantics
			// for legitimate history replays. R67-SEC-3.
			events := msg.Events
			if len(events) > maxPushedHistoryEvents {
				events = events[len(events)-maxPushedHistoryEvents:]
			}
			c.broadcastToSubs(msg.Key, ServerMsg{Type: "history", Key: msg.Key, Events: events, Node: c.id}, false)

		case "session_state":
			// Bound Reason to prevent a compromised node from flooding
			// subscribed browser clients and forcing send-channel drops.
			// R61-SEC-9.
			c.broadcastToSubs(msg.Key, ServerMsg{Type: "session_state", Key: msg.Key, State: msg.State, Reason: truncateLabelUTF8(msg.Reason, maxPushedNodeStringBytes), Node: c.id}, false)

		case "subscribed":
			c.broadcastToSubs(msg.Key, ServerMsg{Type: "subscribed", Key: msg.Key, Node: c.id}, false)

		case "subscribe_error":
			// Same cap as session_state.Reason; msg.Error reaches every
			// subscribed client on the 'error' type. R61-SEC-9.
			c.broadcastToSubs(msg.Key, ServerMsg{Type: "error", Key: msg.Key, Node: c.id, Error: truncateLabelUTF8(msg.Error, maxPushedNodeStringBytes)}, true)
		}
	}
}

func (c *ReverseConn) markDisconnected() {
	c.statusMu.Lock()
	c.status = "error"
	c.statusMu.Unlock()

	c.closeMu.Lock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	c.closeMu.Unlock()

	// Mirror Close(): cancel baseCtx so Subscribe history goroutines unwind
	// the 5s FetchEvents timeout rather than waiting it out. Idempotent.
	c.baseCancel()

	// Drop EventSink references so disconnected browser clients don't keep
	// sinks live for the hub's 90s subscription TTL.
	c.subMu.Lock()
	clear(c.subs)
	c.subMu.Unlock()
}
