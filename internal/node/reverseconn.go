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

	c.pendingMu.Lock()
	c.pending[reqID] = ch
	c.pendingMu.Unlock()

	if err := c.writeJSON(ReverseMsg{
		Type:   "request",
		ReqID:  reqID,
		Method: method,
		Params: marshalParams(params),
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

func marshalParams(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
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

func (c *ReverseConn) ProxyTakeover(ctx context.Context, pid int, sessionID, cwd string, procStart uint64) error {
	_, err := c.rpc(ctx, "takeover", map[string]any{
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

func (c *ReverseConn) Subscribe(cl EventSink, key string, after int64) {
	c.subMu.Lock()
	alreadySub := len(c.subs[key]) > 0
	c.subs[key] = append(c.subs[key], cl)
	if !alreadySub {
		c.subMu.Unlock()
		// First subscriber: tell remote to start pushing events
		c.writeJSON(ReverseMsg{Type: "subscribe", Key: key, After: after}) //nolint
	} else {
		c.subMu.Unlock()
		// Additional subscriber: send history via RPC (non-blocking)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

func (c *ReverseConn) readLoop() {
	defer c.markDisconnected()

	for {
		var msg ReverseMsg
		if err := c.conn.ReadJSON(&msg); err != nil {
			slog.Debug("reverse node disconnected", "node", c.id, "err", err)
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
					err = fmt.Errorf("node %s: %s", c.id, msg.Error)
				}
				ch <- reverseResult{msg.Result, err}
			}

		case "event":
			c.subMu.Lock()
			clients := make([]EventSink, len(c.subs[msg.Key]))
			copy(clients, c.subs[msg.Key])
			c.subMu.Unlock()
			out := ServerMsg{Type: "event", Key: msg.Key, Event: msg.Event, Node: c.id}
			data, err := json.Marshal(out)
			if err != nil {
				continue
			}
			for _, cl := range clients {
				cl.SendRaw(data)
			}

		case "session_state":
			c.subMu.Lock()
			clients := make([]EventSink, len(c.subs[msg.Key]))
			copy(clients, c.subs[msg.Key])
			c.subMu.Unlock()
			out := ServerMsg{Type: "session_state", Key: msg.Key, State: msg.State, Node: c.id}
			data, err := json.Marshal(out)
			if err != nil {
				continue
			}
			for _, cl := range clients {
				cl.SendRaw(data)
			}

		case "subscribed":
			// Remote confirmed subscription; notify all waiting clients.
			c.subMu.Lock()
			clients := make([]EventSink, len(c.subs[msg.Key]))
			copy(clients, c.subs[msg.Key])
			c.subMu.Unlock()
			out := ServerMsg{Type: "subscribed", Key: msg.Key, Node: c.id}
			data, err := json.Marshal(out)
			if err != nil {
				continue
			}
			for _, cl := range clients {
				cl.SendRaw(data)
			}

		case "subscribe_error":
			// Remote could not find the session
			c.subMu.Lock()
			clients := make([]EventSink, len(c.subs[msg.Key]))
			copy(clients, c.subs[msg.Key])
			delete(c.subs, msg.Key)
			c.subMu.Unlock()
			out := ServerMsg{Type: "error", Key: msg.Key, Node: c.id, Error: msg.Error}
			data, err := json.Marshal(out)
			if err != nil {
				continue
			}
			for _, cl := range clients {
				cl.SendRaw(data)
			}
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
}
