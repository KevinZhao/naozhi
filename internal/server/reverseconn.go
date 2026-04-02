package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/reverse"
)

type reverseResult struct {
	result json.RawMessage
	err    error
}

// ReverseNodeConn is the primary-side representation of a reverse-connected node.
// It implements NodeConn by forwarding calls over the reverse WebSocket connection.
type ReverseNodeConn struct {
	id          string
	displayName string

	writeMu sync.Mutex
	conn    *websocket.Conn

	pendingMu sync.Mutex
	pending   map[string]chan reverseResult // req_id → waiting caller
	reqSeq    atomic.Int64

	subMu sync.Mutex
	subs  map[string][]*wsClient // session key → local browser clients

	statusMu sync.RWMutex
	status   string // "ok" | "connecting" | "error"

	done    chan struct{}
	closed  bool
	closeMu sync.Mutex
}

func newReverseNodeConn(id, displayName string, conn *websocket.Conn) *ReverseNodeConn {
	return &ReverseNodeConn{
		id:          id,
		displayName: displayName,
		conn:        conn,
		pending:     make(map[string]chan reverseResult),
		subs:        make(map[string][]*wsClient),
		status:      "ok",
		done:        make(chan struct{}),
	}
}

func (c *ReverseNodeConn) NodeID() string      { return c.id }
func (c *ReverseNodeConn) DisplayName() string { return c.displayName }

func (c *ReverseNodeConn) Status() string {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.status
}

func (c *ReverseNodeConn) Close() {
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

func (c *ReverseNodeConn) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(v)
}

// rpc sends a request to the remote node and waits for the response.
func (c *ReverseNodeConn) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	reqID := strconv.FormatInt(c.reqSeq.Add(1), 10)
	ch := make(chan reverseResult, 1)

	c.pendingMu.Lock()
	c.pending[reqID] = ch
	c.pendingMu.Unlock()

	if err := c.writeJSON(reverse.ReverseMsg{
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

func (c *ReverseNodeConn) FetchSessions(ctx context.Context) ([]map[string]any, error) {
	raw, err := c.rpc(ctx, "fetch_sessions", nil)
	if err != nil {
		return nil, err
	}
	var result []map[string]any
	return result, json.Unmarshal(raw, &result)
}

func (c *ReverseNodeConn) FetchProjects(ctx context.Context) ([]map[string]any, error) {
	raw, err := c.rpc(ctx, "fetch_projects", nil)
	if err != nil {
		return nil, err
	}
	var result []map[string]any
	return result, json.Unmarshal(raw, &result)
}

func (c *ReverseNodeConn) FetchDiscovered(ctx context.Context) ([]map[string]any, error) {
	raw, err := c.rpc(ctx, "fetch_discovered", nil)
	if err != nil {
		return nil, err
	}
	var result []map[string]any
	return result, json.Unmarshal(raw, &result)
}

func (c *ReverseNodeConn) FetchDiscoveredPreview(ctx context.Context, sessionID string) ([]cli.EventEntry, error) {
	raw, err := c.rpc(ctx, "fetch_discovered_preview", map[string]string{"session_id": sessionID})
	if err != nil {
		return nil, err
	}
	var result []cli.EventEntry
	return result, json.Unmarshal(raw, &result)
}

func (c *ReverseNodeConn) FetchEvents(ctx context.Context, key string, after int64) ([]cli.EventEntry, error) {
	raw, err := c.rpc(ctx, "fetch_events", map[string]any{"key": key, "after": after})
	if err != nil {
		return nil, err
	}
	var result []cli.EventEntry
	return result, json.Unmarshal(raw, &result)
}

func (c *ReverseNodeConn) Send(ctx context.Context, key, text string) error {
	_, err := c.rpc(ctx, "send", map[string]string{"key": key, "text": text})
	return err
}

func (c *ReverseNodeConn) ProxyTakeover(ctx context.Context, pid int, sessionID, cwd string, procStart uint64) error {
	_, err := c.rpc(ctx, "takeover", map[string]any{
		"pid": pid, "session_id": sessionID, "cwd": cwd, "proc_start_time": procStart,
	})
	return err
}

func (c *ReverseNodeConn) ProxyRestartPlanner(ctx context.Context, projectName string) error {
	_, err := c.rpc(ctx, "restart_planner", map[string]string{"project_name": projectName})
	return err
}

func (c *ReverseNodeConn) ProxyUpdateConfig(ctx context.Context, projectName string, cfg json.RawMessage) error {
	_, err := c.rpc(ctx, "update_config", map[string]any{"project_name": projectName, "config": cfg})
	return err
}

func (c *ReverseNodeConn) Subscribe(cl *wsClient, key string, after int64) {
	c.subMu.Lock()
	alreadySub := len(c.subs[key]) > 0
	c.subs[key] = append(c.subs[key], cl)
	if !alreadySub {
		c.subMu.Unlock()
		// First subscriber: tell remote to start pushing events
		c.writeJSON(reverse.ReverseMsg{Type: "subscribe", Key: key, After: after}) //nolint
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
			cl.sendJSON(wsServerMsg{Type: "subscribed", Key: key, Node: c.id})
			if len(entries) > 0 {
				cl.sendJSON(wsServerMsg{Type: "history", Key: key, Node: c.id, Events: entries})
			}
		}()
	}
}

func (c *ReverseNodeConn) Unsubscribe(cl *wsClient, key string) {
	c.subMu.Lock()
	clients := c.subs[key]
	for i, existing := range clients {
		if existing == cl {
			c.subs[key] = append(clients[:i], clients[i+1:]...)
			break
		}
	}
	empty := len(c.subs[key]) == 0
	if empty {
		delete(c.subs, key)
	}
	c.subMu.Unlock()

	if empty {
		c.writeJSON(reverse.ReverseMsg{Type: "unsubscribe", Key: key}) //nolint
	}
	cl.sendJSON(wsServerMsg{Type: "unsubscribed", Key: key, Node: c.id})
}

func (c *ReverseNodeConn) RemoveClient(cl *wsClient) {
	c.subMu.Lock()
	var emptyKeys []string
	for key, clients := range c.subs {
		for i, existing := range clients {
			if existing == cl {
				c.subs[key] = append(clients[:i], clients[i+1:]...)
				break
			}
		}
		if len(c.subs[key]) == 0 {
			delete(c.subs, key)
			emptyKeys = append(emptyKeys, key)
		}
	}
	c.subMu.Unlock()

	for _, key := range emptyKeys {
		c.writeJSON(reverse.ReverseMsg{Type: "unsubscribe", Key: key}) //nolint
	}
}

func (c *ReverseNodeConn) readLoop() {
	defer c.markDisconnected()

	for {
		var msg reverse.ReverseMsg
		if err := c.conn.ReadJSON(&msg); err != nil {
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
					err = fmt.Errorf("%s", msg.Error)
				}
				ch <- reverseResult{msg.Result, err}
			}

		case "event":
			c.subMu.Lock()
			clients := make([]*wsClient, len(c.subs[msg.Key]))
			copy(clients, c.subs[msg.Key])
			c.subMu.Unlock()
			out := wsServerMsg{Type: "event", Key: msg.Key, Event: msg.Event, Node: c.id}
			for _, cl := range clients {
				cl.sendJSON(out)
			}

		case "session_state":
			c.subMu.Lock()
			clients := make([]*wsClient, len(c.subs[msg.Key]))
			copy(clients, c.subs[msg.Key])
			c.subMu.Unlock()
			out := wsServerMsg{Type: "session_state", Key: msg.Key, State: msg.State, Node: c.id}
			for _, cl := range clients {
				cl.sendJSON(out)
			}

		case "subscribed":
			// Remote confirmed subscription; notify all waiting clients.
			c.subMu.Lock()
			clients := make([]*wsClient, len(c.subs[msg.Key]))
			copy(clients, c.subs[msg.Key])
			c.subMu.Unlock()
			out := wsServerMsg{Type: "subscribed", Key: msg.Key, Node: c.id}
			for _, cl := range clients {
				cl.sendJSON(out)
			}

		case "subscribe_error":
			// Remote could not find the session
			c.subMu.Lock()
			clients := make([]*wsClient, len(c.subs[msg.Key]))
			copy(clients, c.subs[msg.Key])
			delete(c.subs, msg.Key)
			c.subMu.Unlock()
			out := wsServerMsg{Type: "error", Key: msg.Key, Node: c.id, Error: msg.Error}
			for _, cl := range clients {
				cl.sendJSON(out)
			}
		}
	}
}

func (c *ReverseNodeConn) markDisconnected() {
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
