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

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/osutil"
)

// maxReverseRPCResponseBytes caps one reverse-RPC response before Unmarshal:
// []map[string]any targets allocate per nested object, an easy heap-exhaustion
// primitive from a compromised node. ~10x the worst legitimate response.
const maxReverseRPCResponseBytes = 2 << 20 // 2 MiB

// maxPendingReverseRPCs caps in-flight RPCs per ReverseConn so a hung but
// TCP-alive peer cannot let polling dashboards accumulate entries before
// readLoop detects the dead connection; a poll drives ≤10 concurrent fetches.
const maxPendingReverseRPCs = 256

// maxPushedNodeStringBytes caps free-form strings in pushed messages
// (session_state.Reason, subscribe_error.Error), which skip the rpc() size
// gate and are broadcast to every subscribed browser.
const maxPushedNodeStringBytes = 512

// maxPushedHistoryEvents caps the `events` array in pushed history so a
// compromised node cannot amplify a 16 MB push N× across browser tabs; 500
// matches the dashboard page limit so legitimate replays are never truncated.
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
	// meta is the immutable register-time snapshot; never mutated, so
	// concurrent Meta() readers need no lock.
	meta NodeMeta

	writeMu sync.Mutex
	conn    *websocket.Conn

	pendingMu sync.Mutex
	pending   map[string]chan reverseResult // req_id → waiting caller
	reqSeq    atomic.Int64

	subMu sync.Mutex
	subs  map[string][]EventSink // session key → local browser clients

	// subWG tracks the detached Subscribe history-fetch goroutines so Close()
	// never returns while one could still SendJSON after teardown (#2294).
	subWG sync.WaitGroup

	statusMu sync.RWMutex
	status   string // "ok" | "connecting" | "error"

	done    chan struct{}
	closed  bool
	closeMu sync.Mutex

	// baseCtx parents in-flight Subscribe history fetches; baseCancel fires on
	// Close()/markDisconnected() so derived timeouts unwind without a
	// per-RPC watcher goroutine.
	baseCtx    context.Context
	baseCancel context.CancelFunc
}

// newReverseConnWithMeta constructs a ReverseConn after a successful
// register. caps is the wire slice (nil = legacy peer advertising nothing);
// hostname is the truncated remote label, distinct from remoteAddr.
func newReverseConnWithMeta(id, displayName, remoteAddr string, conn *websocket.Conn, caps []string, hostname string) *ReverseConn {
	baseCtx, baseCancel := context.WithCancel(context.Background())
	return &ReverseConn{
		id:          id,
		displayName: displayName,
		remoteAddr:  remoteAddr,
		meta: NodeMeta{
			NodeID:       id,
			DisplayName:  displayName,
			Hostname:     hostname,
			Capabilities: capsFromSlice(caps),
			RegisteredAt: time.Now(),
		},
		conn:       conn,
		pending:    make(map[string]chan reverseResult),
		subs:       make(map[string][]EventSink),
		status:     "ok",
		done:       make(chan struct{}),
		baseCtx:    baseCtx,
		baseCancel: baseCancel,
	}
}

func (c *ReverseConn) NodeID() string      { return c.id }
func (c *ReverseConn) DisplayName() string { return c.displayName }
func (c *ReverseConn) RemoteAddr() string  { return c.remoteAddr }

// Meta returns the immutable register-time metadata snapshot; never nil (a
// legacy peer has nil Capabilities and HasCap denies non-empty queries).
func (c *ReverseConn) Meta() *NodeMeta { return &c.meta }

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

	// Idempotent; markDisconnected may also fire it.
	c.baseCancel()
	conn.Close()

	// Returns promptly: baseCancel unwound the fetches and conn.Close stopped readLoop.
	c.subWG.Wait()
}

func (c *ReverseConn) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// A failed SetWriteDeadline means a half-closed conn; do not issue a
	// deadline-less WriteJSON that could block until TCP keepalive expires.
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
	// Fail fast rather than waste the caller's timeout on a hung peer.
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
		// Size gate at the single RPC choke point (see maxReverseRPCResponseBytes).
		if len(res.result) > maxReverseRPCResponseBytes {
			return nil, fmt.Errorf("reverse rpc response too large (%d > %d bytes)", len(res.result), maxReverseRPCResponseBytes)
		}
		return res.result, nil
	case <-ctx.Done():
		// Remove pending so a late response does not leak.
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

func (c *ReverseConn) FetchDiscoveredPreview(ctx context.Context, sessionID string) ([]clievent.EventEntry, error) {
	raw, err := c.rpc(ctx, "fetch_discovered_preview", map[string]string{"session_id": sessionID})
	if err != nil {
		return nil, err
	}
	var result []clievent.EventEntry
	return result, json.Unmarshal(raw, &result)
}

func (c *ReverseConn) FetchEvents(ctx context.Context, key string, after int64) ([]clievent.EventEntry, error) {
	raw, err := c.rpc(ctx, "fetch_events", map[string]any{"key": key, "after": after})
	if err != nil {
		return nil, err
	}
	var result []clievent.EventEntry
	return result, json.Unmarshal(raw, &result)
}

// FetchBackends relays the remote node's CLI backend manifest as raw JSON (see
// NodeFetcher.FetchBackends); an older peer replies "unknown method".
func (c *ReverseConn) FetchBackends(ctx context.Context) (json.RawMessage, error) {
	return c.rpc(ctx, "fetch_backends", nil)
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
	// Same client re-subscribing keeps one entry, or every frame would be
	// delivered twice; it still gets its `subscribed` ack + history page.
	if !containsSink(c.subs[key], cl) {
		c.subs[key] = append(c.subs[key], cl)
	}
	// Add under subMu so Close()'s subWG.Wait() never races it.
	c.subWG.Add(1)
	c.subMu.Unlock()

	if alreadySub {
		// Additional subscriber: history via RPC, derived from baseCtx so a
		// connection drop cancels it.
		go func() {
			defer c.subWG.Done()
			ctx, cancel := context.WithTimeout(c.baseCtx, 5*time.Second)
			defer cancel()

			entries, err := c.FetchEvents(ctx, key, after)
			if err != nil {
				return
			}
			cl.SendJSON(ServerMsg{Type: "subscribed", Key: key, Node: c.id})
			if len(entries) > 0 {
				cl.SendJSON(ServerMsg{Type: "history", Key: key, Node: c.id, Events: entries, Initial: true})
			}
		}()
	} else {
		// First subscriber: the sink was added above so readLoop can deliver
		// events arriving right after the write; roll back on failure.
		if err := c.writeJSON(ReverseMsg{Type: "subscribe", Key: key, After: after}); err != nil {
			slog.Warn("reverse subscribe write failed", "node", c.id, "key", key, "err", err)
			c.subMu.Lock()
			removeSub(c.subs, key, cl)
			c.subMu.Unlock()
			// No history goroutine on this path; release the token.
			c.subWG.Done()
			return
		}
		// Also fetch persisted history: the remote's streamEvents only pushes
		// on Append, which never fires for a process-less session. This frame
		// and the remote's `subscribed` ack may arrive in either order, which
		// is why the initial page is keyed on ServerMsg.Initial, not arrival.
		go func() {
			defer c.subWG.Done()
			ctx, cancel := context.WithTimeout(c.baseCtx, 5*time.Second)
			defer cancel()

			entries, err := c.FetchEvents(ctx, key, after)
			if err != nil {
				slog.Debug("reverse first-subscribe fetch events failed", "node", c.id, "key", key, "err", err)
				return
			}
			if len(entries) > 0 {
				cl.SendJSON(ServerMsg{Type: "history", Key: key, Node: c.id, Events: entries, Initial: true})
			}
		}()
	}
}

// RefreshSubscription forces the remote to re-create streamEvents for key
// after a remote send, since the previous process may have died. Best-effort:
// a racing Unsubscribe may cause a redundant subscribe the remote tolerates.
func (c *ReverseConn) RefreshSubscription(key string) {
	c.subMu.Lock()
	hasSubs := len(c.subs[key]) > 0
	c.subMu.Unlock()
	if hasSubs {
		if err := c.writeJSON(ReverseMsg{Type: "subscribe", Key: key}); err != nil {
			slog.Debug("reverseconn: refresh subscribe write failed", "node", c.id, "key", key, "err", err)
		}
	}
}

func (c *ReverseConn) Unsubscribe(cl EventSink, key string) {
	c.subMu.Lock()
	empty := removeSub(c.subs, key, cl)
	c.subMu.Unlock()

	if empty {
		if err := c.writeJSON(ReverseMsg{Type: "unsubscribe", Key: key}); err != nil {
			slog.Debug("reverseconn: unsubscribe write failed", "node", c.id, "key", key, "err", err)
		}
	}
	cl.SendJSON(ServerMsg{Type: "unsubscribed", Key: key, Node: c.id})
}

func (c *ReverseConn) RemoveClient(cl EventSink) {
	c.subMu.Lock()
	emptyKeys := removeSubAll(c.subs, cl)
	c.subMu.Unlock()

	for _, key := range emptyKeys {
		if err := c.writeJSON(ReverseMsg{Type: "unsubscribe", Key: key}); err != nil {
			slog.Debug("reverseconn: remove client unsubscribe write failed", "node", c.id, "key", key, "err", err)
		}
	}
}

// subSnapPool reuses the subscriber snapshot built on every remote event
// (dozens per second during a turn).
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

	// Clear pointers so disconnected sinks are not pinned by the pool.
	for i := range clients {
		clients[i] = nil
	}
	// Never pool an arbitrarily large backing array after a subscriber spike.
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

	// The connector pings every 30s; a 90s read deadline detects silent
	// disconnects (NAT timeout, crash without close).
	const reverseReadTimeout = 90 * time.Second
	if err := c.conn.SetReadDeadline(time.Now().Add(reverseReadTimeout)); err != nil {
		return
	}
	c.conn.SetPongHandler(func(string) error {
		// Errors surface on the next read iteration.
		_ = c.conn.SetReadDeadline(time.Now().Add(reverseReadTimeout))
		return nil
	})
	c.conn.SetPingHandler(func(appData string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(reverseReadTimeout))
		return c.conn.WriteControl(
			websocket.PongMessage, []byte(appData), time.Now().Add(time.Second),
		)
	})

	for {
		var msg ReverseMsg
		// ReadMessage + Unmarshal avoids ReadJSON's per-frame Decoder alloc.
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			slog.Debug("reverse node disconnected", "node", c.id, "err", err)
			return
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			slog.Debug("reverse node disconnected", "node", c.id, "err", err)
			return
		}
		if err := c.conn.SetReadDeadline(time.Now().Add(reverseReadTimeout)); err != nil {
			return
		}

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
					err = fmt.Errorf("node %s: %s", c.id, osutil.SanitizeForLog(msg.Error, 256))
				}
				ch <- reverseResult{msg.Result, err}
			}

		case "event":
			c.broadcastToSubs(msg.Key, ServerMsg{Type: "event", Key: msg.Key, Event: msg.Event, Node: c.id}, false)

		case "events":
			// Keep the tail (most recent) when capping.
			events := msg.Events
			if len(events) > maxPushedHistoryEvents {
				events = events[len(events)-maxPushedHistoryEvents:]
			}
			c.broadcastToSubs(msg.Key, ServerMsg{Type: "history", Key: msg.Key, Events: events, Node: c.id}, false)

		case "session_state":
			c.broadcastToSubs(msg.Key, ServerMsg{Type: "session_state", Key: msg.Key, State: msg.State, Reason: truncateLabelUTF8(msg.Reason, maxPushedNodeStringBytes), Node: c.id}, false)

		case "subscribed":
			c.broadcastToSubs(msg.Key, ServerMsg{Type: "subscribed", Key: msg.Key, Node: c.id}, false)

		case "subscribe_error":
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

	// Idempotent; unwinds in-flight history fetches like Close().
	c.baseCancel()

	// Drop sink references so disconnected browsers are not kept live for the
	// hub's 90s subscription TTL.
	c.subMu.Lock()
	clear(c.subs)
	c.subMu.Unlock()
}
