package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/session"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// Hub manages WebSocket client connections and event subscriptions.
type Hub struct {
	mu        sync.Mutex
	clients   map[*wsClient]struct{}
	router    *session.Router
	agents    map[string]session.AgentOpts
	agentCmds map[string]string
	dashToken string
	guard     *sessionGuard
	nodes     map[string]*NodeClient
	relays    map[string]*wsRelay // node ID -> relay (lazy)
}

// NewHub creates a new WebSocket hub.
func NewHub(router *session.Router, agents map[string]session.AgentOpts, agentCmds map[string]string, dashToken string, guard *sessionGuard, nodes map[string]*NodeClient) *Hub {
	return &Hub{
		clients:   make(map[*wsClient]struct{}),
		router:    router,
		agents:    agents,
		agentCmds: agentCmds,
		dashToken: dashToken,
		guard:     guard,
		nodes:     nodes,
		relays:    make(map[string]*wsRelay),
	}
}

// HandleUpgrade upgrades an HTTP connection to WebSocket.
func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("ws upgrade failed", "err", err)
		return
	}
	c := &wsClient{
		conn:          conn,
		send:          make(chan []byte, 256),
		hub:           h,
		subscriptions: make(map[string]func()),
		done:          make(chan struct{}),
	}
	if h.dashToken == "" {
		c.authenticated.Store(true)
	}
	h.register(c)
	go c.writePump()
	go c.readPump()
}

func (h *Hub) register(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) unregister(c *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		for _, unsub := range c.subscriptions {
			unsub()
		}
		c.subscriptions = nil
	}
	// Collect relays to clean up (release hub lock before calling relay methods)
	relays := make([]*wsRelay, 0, len(h.relays))
	for _, relay := range h.relays {
		relays = append(relays, relay)
	}
	h.mu.Unlock()

	for _, relay := range relays {
		relay.RemoveClient(c)
	}
}

func (h *Hub) handleAuth(c *wsClient, msg wsClientMsg) {
	if h.dashToken == "" || msg.Token == h.dashToken {
		c.authenticated.Store(true)
		c.sendJSON(wsServerMsg{Type: "auth_ok"})
	} else {
		c.sendJSON(wsServerMsg{Type: "auth_fail", Error: "invalid token"})
	}
}

func (h *Hub) handleSubscribe(c *wsClient, msg wsClientMsg) {
	key := msg.Key
	if key == "" {
		c.sendJSON(wsServerMsg{Type: "error", Error: "key is required"})
		return
	}

	// Remote node delegation
	if msg.Node != "" && msg.Node != "local" {
		h.handleRemoteSubscribe(c, msg)
		return
	}

	// Unsubscribe from previous subscription under lock
	h.mu.Lock()
	if unsub, ok := c.subscriptions[key]; ok {
		unsub()
		delete(c.subscriptions, key)
	}
	h.mu.Unlock()

	sess := h.router.GetSession(key)
	if sess == nil {
		c.sendJSON(wsServerMsg{Type: "error", Key: key, Error: "session not found"})
		return
	}

	notify, unsub := sess.SubscribeEvents()

	h.mu.Lock()
	if c.subscriptions == nil {
		// Client was removed during Shutdown
		h.mu.Unlock()
		unsub()
		return
	}
	c.subscriptions[key] = unsub
	h.mu.Unlock()

	snap := sess.Snapshot()

	entries := sess.EventEntries()
	if msg.After > 0 {
		entries = sess.EventEntriesSince(msg.After)
	}

	c.sendJSON(wsServerMsg{Type: "subscribed", Key: key, State: snap.State})

	var lastTime int64
	if len(entries) > 0 {
		c.sendJSON(wsServerMsg{Type: "history", Key: key, Events: entries})
		lastTime = entries[len(entries)-1].Time
	}

	go h.eventPushLoop(c, key, notify, sess, lastTime)
}

func (h *Hub) handleUnsubscribe(c *wsClient, msg wsClientMsg) {
	key := msg.Key

	// Remote node delegation
	if msg.Node != "" && msg.Node != "local" {
		h.handleRemoteUnsubscribe(c, msg)
		return
	}

	h.mu.Lock()
	if unsub, ok := c.subscriptions[key]; ok {
		unsub()
		delete(c.subscriptions, key)
	}
	h.mu.Unlock()
	c.sendJSON(wsServerMsg{Type: "unsubscribed", Key: key})
}

func (h *Hub) handleSend(c *wsClient, msg wsClientMsg) {
	// Remote node delegation
	if msg.Node != "" && msg.Node != "local" {
		h.handleRemoteSend(c, msg)
		return
	}

	key := msg.Key
	if key == "" {
		c.sendJSON(wsServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "key is required"})
		return
	}
	if msg.Text == "" {
		c.sendJSON(wsServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "text is required"})
		return
	}

	// Handle /clear from dashboard — CLI built-in doesn't work in stream-json
	trimmed := strings.TrimSpace(msg.Text)
	if trimmed == "/clear" || trimmed == "/new" {
		h.router.Reset(key)
		c.sendJSON(wsServerMsg{Type: "send_ack", ID: msg.ID, Status: "accepted", Key: key})
		h.broadcastState(key, "dead", "user_reset")
		h.BroadcastSessionsUpdate()
		return
	}

	if !h.guard.TryAcquire(key) {
		c.sendJSON(wsServerMsg{Type: "send_ack", ID: msg.ID, Status: "busy", Key: key})
		return
	}

	c.sendJSON(wsServerMsg{Type: "send_ack", ID: msg.ID, Status: "accepted", Key: key})

	go func() {
		defer h.guard.Release(key)

		// Notify subscribers: running
		h.broadcastState(key, "running", "")
		h.BroadcastSessionsUpdate()

		ctx := context.Background()
		parts := strings.SplitN(key, ":", 4)
		agentID := "general"
		if len(parts) == 4 {
			agentID = parts[3]
		}

		opts := h.agents[agentID]
		sess, _, err := h.router.GetOrCreate(ctx, key, opts)
		if err != nil {
			slog.Error("ws send: get session", "key", key, "err", err)
			return
		}

		if _, err := sess.Send(ctx, msg.Text, nil, nil); err != nil {
			slog.Error("ws send: send", "key", key, "err", err)
		}

		// Notify subscribers: ready (or dead if process died)
		sess2 := h.router.GetSession(key)
		if sess2 != nil {
			snap := sess2.Snapshot()
			h.broadcastState(key, snap.State, snap.DeathReason)
		} else {
			h.broadcastState(key, "ready", "")
		}
		h.BroadcastSessionsUpdate()
	}()
}

func (h *Hub) handleInterrupt(c *wsClient, msg wsClientMsg) {
	key := msg.Key
	if key == "" {
		c.sendJSON(wsServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "error", Error: "key is required"})
		return
	}

	ok := h.router.InterruptSession(key)
	if ok {
		slog.Info("session interrupted via dashboard", "key", key)
		c.sendJSON(wsServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "ok", Key: key})
	} else {
		c.sendJSON(wsServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "not_running", Key: key})
	}
}

func (h *Hub) eventPushLoop(c *wsClient, key string, notify <-chan struct{}, sess *session.ManagedSession, lastTime int64) {
	for {
		select {
		case _, ok := <-notify:
			if !ok {
				return
			}
			entries := sess.EventEntriesSince(lastTime)
			for i := range entries {
				select {
				case <-c.done:
					return
				default:
				}
				c.sendJSON(wsServerMsg{Type: "event", Key: key, Event: &entries[i]})
				if entries[i].Time > lastTime {
					lastTime = entries[i].Time
				}
			}
		case <-c.done:
			return
		}
	}
}

// broadcastState sends a session_state message to all clients subscribed to the given key.
func (h *Hub) broadcastState(key, state, reason string) {
	msg := wsServerMsg{Type: "session_state", Key: key, State: state, Reason: reason}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if _, ok := c.subscriptions[key]; ok {
			c.sendJSON(msg)
		}
	}
}

// BroadcastSessionsUpdate notifies all connected WS clients that the session list changed.
func (h *Hub) BroadcastSessionsUpdate() {
	msg := wsServerMsg{Type: "sessions_update"}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.authenticated.Load() {
			c.sendJSON(msg)
		}
	}
}

// Shutdown closes all WebSocket client connections and relays.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	// Close all relays
	for id, relay := range h.relays {
		relay.Close()
		delete(h.relays, id)
	}
	for c := range h.clients {
		for _, unsub := range c.subscriptions {
			unsub()
		}
		c.subscriptions = nil
		if c.conn != nil {
			c.conn.Close()
		}
		delete(h.clients, c)
	}
	h.mu.Unlock()
}

// ─── Remote node handlers ────────────────────────────────────────────────────

func (h *Hub) handleRemoteSubscribe(c *wsClient, msg wsClientMsg) {
	nodeID := msg.Node
	h.mu.Lock()
	nc, ok := h.nodes[nodeID]
	if !ok {
		h.mu.Unlock()
		c.sendJSON(wsServerMsg{Type: "error", Key: msg.Key, Error: "unknown node: " + nodeID})
		return
	}
	relay, exists := h.relays[nodeID]
	if !exists {
		relay = newWSRelay(nc, h)
		h.relays[nodeID] = relay
	}
	h.mu.Unlock()

	relay.Subscribe(c, msg.Key, msg.After)
}

func (h *Hub) handleRemoteUnsubscribe(c *wsClient, msg wsClientMsg) {
	nodeID := msg.Node
	h.mu.Lock()
	relay, exists := h.relays[nodeID]
	h.mu.Unlock()
	if !exists {
		c.sendJSON(wsServerMsg{Type: "unsubscribed", Key: msg.Key, Node: nodeID})
		return
	}
	relay.Unsubscribe(c, msg.Key)
}

func (h *Hub) handleRemoteSend(c *wsClient, msg wsClientMsg) {
	nodeID := msg.Node
	h.mu.Lock()
	nc, ok := h.nodes[nodeID]
	h.mu.Unlock()

	if !ok {
		c.sendJSON(wsServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "unknown node: " + nodeID})
		return
	}

	c.sendJSON(wsServerMsg{Type: "send_ack", ID: msg.ID, Status: "accepted", Key: msg.Key, Node: nodeID})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := nc.Send(ctx, msg.Key, msg.Text); err != nil {
			slog.Error("remote ws send failed", "node", nodeID, "key", msg.Key, "err", err)
		}
	}()
}
