package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsRelay maintains a persistent WS connection to a remote node
// and forwards events to local browser clients.
type wsRelay struct {
	node    *NodeClient
	hub     *Hub
	mu      sync.Mutex
	writeMu sync.Mutex // serializes writes to the WS connection
	conn    *websocket.Conn
	subs    map[string][]*wsClient // remote session key -> local clients
	done    chan struct{}
	closed  bool
}

func newWSRelay(node *NodeClient, hub *Hub) *wsRelay {
	return &wsRelay{
		node: node,
		hub:  hub,
		subs: make(map[string][]*wsClient),
		done: make(chan struct{}),
	}
}

// Subscribe subscribes a local client to a remote session key.
// Connects to the remote node on first call.
func (r *wsRelay) Subscribe(c *wsClient, key string, after int64) {
	if err := r.ensureConnected(); err != nil {
		c.sendJSON(wsServerMsg{Type: "error", Key: key, Node: r.node.ID, Error: "relay connect: " + err.Error()})
		return
	}

	r.mu.Lock()
	alreadySubscribed := len(r.subs[key]) > 0
	r.subs[key] = append(r.subs[key], c)
	r.mu.Unlock()

	if alreadySubscribed {
		// Key already subscribed on remote; send history via HTTP to just this client
		go r.sendHistoryToClient(c, key, after)
		return
	}

	// First subscriber for this key: subscribe on the remote WS
	r.writeJSON(wsClientMsg{Type: "subscribe", Key: key, After: after})
}

// Unsubscribe removes a local client from a remote session key.
func (r *wsRelay) Unsubscribe(c *wsClient, key string) {
	r.mu.Lock()
	clients := r.subs[key]
	for i, cl := range clients {
		if cl == c {
			r.subs[key] = append(clients[:i], clients[i+1:]...)
			break
		}
	}
	empty := len(r.subs[key]) == 0
	if empty {
		delete(r.subs, key)
	}
	r.mu.Unlock()

	if empty {
		r.writeJSON(wsClientMsg{Type: "unsubscribe", Key: key})
	}
	c.sendJSON(wsServerMsg{Type: "unsubscribed", Key: key, Node: r.node.ID})
}

// RemoveClient removes a client from all subscriptions (called on disconnect).
func (r *wsRelay) RemoveClient(c *wsClient) {
	r.mu.Lock()
	var emptyKeys []string
	for key, clients := range r.subs {
		for i, cl := range clients {
			if cl == c {
				r.subs[key] = append(clients[:i], clients[i+1:]...)
				break
			}
		}
		if len(r.subs[key]) == 0 {
			delete(r.subs, key)
			emptyKeys = append(emptyKeys, key)
		}
	}
	r.mu.Unlock()

	for _, key := range emptyKeys {
		r.writeJSON(wsClientMsg{Type: "unsubscribe", Key: key})
	}
}

// Close closes the WS connection and cleans up.
func (r *wsRelay) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.done)
	conn := r.conn
	r.conn = nil
	r.subs = make(map[string][]*wsClient)
	r.mu.Unlock()

	if conn != nil {
		conn.Close()
	}
}

func (r *wsRelay) ensureConnected() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fmt.Errorf("relay closed")
	}
	if r.conn != nil {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	return r.connect()
}

func (r *wsRelay) connect() error {
	wsURL := strings.Replace(r.node.URL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL += "/ws"

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", r.node.ID, err)
	}

	// Authenticate
	if err := conn.WriteJSON(wsClientMsg{Type: "auth", Token: r.node.Token}); err != nil {
		conn.Close()
		return fmt.Errorf("auth write %s: %w", r.node.ID, err)
	}
	var resp wsServerMsg
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&resp); err != nil {
		conn.Close()
		return fmt.Errorf("auth read %s: %w", r.node.ID, err)
	}
	if resp.Type != "auth_ok" {
		conn.Close()
		return fmt.Errorf("auth failed %s: %s", r.node.ID, resp.Error)
	}
	conn.SetReadDeadline(time.Time{})

	r.mu.Lock()
	if r.conn != nil {
		// Another goroutine already connected
		r.mu.Unlock()
		conn.Close()
		return nil
	}
	r.conn = conn
	r.mu.Unlock()

	go r.readLoop(conn)
	return nil
}

func (r *wsRelay) readLoop(conn *websocket.Conn) {
	defer func() {
		r.mu.Lock()
		// Only nil out if this is still the active connection
		if r.conn == conn {
			r.conn = nil
		}
		shouldReconnect := r.conn == nil && !r.closed
		r.mu.Unlock()

		conn.Close()

		if !shouldReconnect {
			return
		}
		select {
		case <-r.done:
			return
		default:
		}
		go r.reconnect()
	}()

	for {
		select {
		case <-r.done:
			return
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var msg wsServerMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		// Tag with source node
		msg.Node = r.node.ID

		r.mu.Lock()
		clients := make([]*wsClient, len(r.subs[msg.Key]))
		copy(clients, r.subs[msg.Key])
		r.mu.Unlock()

		for _, c := range clients {
			c.sendJSON(msg)
		}
	}
}

func (r *wsRelay) reconnect() {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-r.done:
			return
		case <-time.After(backoff):
		}

		if err := r.connect(); err != nil {
			slog.Warn("relay reconnect failed", "node", r.node.ID, "err", err)
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Resubscribe to all active keys
		r.mu.Lock()
		keys := make([]string, 0, len(r.subs))
		for key := range r.subs {
			if len(r.subs[key]) > 0 {
				keys = append(keys, key)
			}
		}
		r.mu.Unlock()

		for _, key := range keys {
			r.writeJSON(wsClientMsg{Type: "subscribe", Key: key})
		}
		slog.Info("relay reconnected", "node", r.node.ID, "keys", len(keys))
		return
	}
}

func (r *wsRelay) sendHistoryToClient(c *wsClient, key string, after int64) {
	c.sendJSON(wsServerMsg{Type: "subscribed", Key: key, Node: r.node.ID})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	entries, err := r.node.FetchEvents(ctx, key, after)
	if err != nil {
		slog.Warn("relay fetch history", "node", r.node.ID, "key", key, "err", err)
		return
	}
	if len(entries) > 0 {
		c.sendJSON(wsServerMsg{Type: "history", Key: key, Node: r.node.ID, Events: entries})
	}
}

func (r *wsRelay) writeJSON(v any) {
	r.mu.Lock()
	conn := r.conn
	r.mu.Unlock()
	if conn == nil {
		return
	}
	r.writeMu.Lock()
	conn.WriteJSON(v)
	r.writeMu.Unlock()
}
