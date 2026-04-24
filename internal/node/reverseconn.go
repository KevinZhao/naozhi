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
}

func newReverseConn(id, displayName, remoteAddr string, conn *websocket.Conn) *ReverseConn {
	return &ReverseConn{
		id:          id,
		displayName: displayName,
		remoteAddr:  remoteAddr,
		conn:        conn,
		pending:     make(map[string]chan reverseResult),
		subs:        make(map[string][]EventSink),
		status:      "ok",
		done:        make(chan struct{}),
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

	conn.Close()
}

func (c *ReverseConn) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
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
		return res.result, res.err
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
		// Anchor the timeout to the connection's done channel so a drop
		// mid-fetch cancels the RPC instead of running to completion then
		// writing to a potentially closed EventSink.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cancelOnClose := make(chan struct{})
			go func() {
				select {
				case <-c.done:
					cancel()
				case <-cancelOnClose:
				}
			}()
			defer close(cancelOnClose)

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
		}
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

// broadcastToSubs snapshots subscribers for key, marshals out, and sends to all.
// If deleteKey is true, the key is removed from the subscription map.
func (c *ReverseConn) broadcastToSubs(key string, out ServerMsg, deleteKey bool) {
	c.subMu.Lock()
	clients := make([]EventSink, len(c.subs[key]))
	copy(clients, c.subs[key])
	if deleteKey {
		delete(c.subs, key)
	}
	c.subMu.Unlock()

	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	for _, cl := range clients {
		cl.SendRaw(data)
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
			c.broadcastToSubs(msg.Key, ServerMsg{Type: "history", Key: msg.Key, Events: msg.Events, Node: c.id}, false)

		case "session_state":
			c.broadcastToSubs(msg.Key, ServerMsg{Type: "session_state", Key: msg.Key, State: msg.State, Reason: msg.Reason, Node: c.id}, false)

		case "subscribed":
			c.broadcastToSubs(msg.Key, ServerMsg{Type: "subscribed", Key: msg.Key, Node: c.id}, false)

		case "subscribe_error":
			c.broadcastToSubs(msg.Key, ServerMsg{Type: "error", Key: msg.Key, Node: c.id, Error: msg.Error}, true)
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

	// Drop EventSink references so disconnected browser clients don't keep
	// sinks live for the hub's 90s subscription TTL.
	c.subMu.Lock()
	clear(c.subs)
	c.subMu.Unlock()
}
