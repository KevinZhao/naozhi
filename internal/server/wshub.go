package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // same-origin requests omit Origin
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	},
	ReadBufferSize:  8192,
	WriteBufferSize: 8192,
}

// Hub manages WebSocket client connections and event subscriptions.
type Hub struct {
	mu         sync.Mutex
	clients    map[*wsClient]struct{}
	router     *session.Router
	agents     map[string]session.AgentOpts
	agentCmds  map[string]string
	dashToken  string
	guard      *sessionGuard
	nodes      map[string]NodeConn
	nodesMu    *sync.RWMutex // shared with Server.nodesMu — all nodes map access must use this
	projectMgr *project.Manager
	ctx        context.Context // cancelled on Shutdown to stop in-flight sends
	cancel     context.CancelFunc

	debounceMu    sync.Mutex
	debounceTimer *time.Timer
}

// NewHub creates a new WebSocket hub.
func NewHub(router *session.Router, agents map[string]session.AgentOpts, agentCmds map[string]string, dashToken string, guard *sessionGuard, nodes map[string]NodeConn, nodesMu *sync.RWMutex, projectMgr *project.Manager) *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{
		clients:    make(map[*wsClient]struct{}),
		router:     router,
		agents:     agents,
		agentCmds:  agentCmds,
		dashToken:  dashToken,
		guard:      guard,
		nodes:      nodes,
		nodesMu:    nodesMu,
		projectMgr: projectMgr,
		ctx:        ctx,
		cancel:     cancel,
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
	} else if cookie, err := r.Cookie(authCookieName); err == nil {
		if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(h.dashToken)) == 1 {
			c.authenticated.Store(true)
		}
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
	h.mu.Unlock()

	// Snapshot nodes under nodesMu to avoid data race
	h.nodesMu.RLock()
	nodes := make([]NodeConn, 0, len(h.nodes))
	for _, conn := range h.nodes {
		nodes = append(nodes, conn)
	}
	h.nodesMu.RUnlock()

	for _, conn := range nodes {
		conn.RemoveClient(c)
	}
}

func (h *Hub) handleAuth(c *wsClient, msg wsClientMsg) {
	if h.dashToken == "" || subtle.ConstantTimeCompare([]byte(msg.Token), []byte(h.dashToken)) == 1 {
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
		h.BroadcastSessionsUpdate()
		return
	}

	// Set workspace override for new dashboard sessions
	if msg.Workspace != "" {
		if idx := strings.LastIndexByte(key, ':'); idx >= 0 {
			chatKey := key[:idx]
			h.router.SetWorkspace(chatKey, msg.Workspace)
		}
	}

	if !h.guard.TryAcquire(key) {
		c.sendJSON(wsServerMsg{Type: "send_ack", ID: msg.ID, Status: "busy", Key: key})
		return
	}

	c.sendJSON(wsServerMsg{Type: "send_ack", ID: msg.ID, Status: "accepted", Key: key})

	capturedText := msg.Text
	go func() {
		defer h.guard.Release(key)

		ctx := h.ctx
		opts := buildSessionOpts(key, h.agents, h.projectMgr)
		sess, _, err := h.router.GetOrCreate(ctx, key, opts)
		if err != nil {
			slog.Error("ws send: get session", "key", key, "err", err)
			c.sendJSON(wsServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Key: key, Error: err.Error()})
			return
		}

		// Notify AFTER session exists so clients can auto-subscribe.
		// For new sessions, spawnSession already called BroadcastSessionsUpdate
		// via notifyChange; for existing sessions we need this explicit call.
		h.broadcastState(key, "running", "")
		h.BroadcastSessionsUpdate()

		if _, err := sess.Send(ctx, capturedText, nil, nil); err != nil {
			slog.Error("ws send: send", "key", key, "err", err)
		}

		// Notify subscribers: ready (or suspended if process died)
		sess2 := h.router.GetSession(key)
		if sess2 != nil {
			snap := sess2.Snapshot()
			h.broadcastState(key, snap.State, snap.DeathReason)
		}
		// If session was removed (e.g. concurrent Reset), sessions_update
		// will cause the frontend to drop it from the sidebar.
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
				data, err := json.Marshal(wsServerMsg{Type: "event", Key: key, Event: &entries[i]})
				if err != nil {
					continue
				}
				c.sendRaw(data)
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
	data, err := json.Marshal(wsServerMsg{Type: "session_state", Key: key, State: state, Reason: reason})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if _, ok := c.subscriptions[key]; ok {
			c.sendRaw(data)
		}
	}
}

// BroadcastSessionsUpdate debounces notifications: resets a 200ms timer on each
// call; the actual broadcast fires only when no further calls arrive within the window.
func (h *Hub) BroadcastSessionsUpdate() {
	h.debounceMu.Lock()
	defer h.debounceMu.Unlock()
	if h.debounceTimer != nil {
		h.debounceTimer.Reset(200 * time.Millisecond)
		return
	}
	h.debounceTimer = time.AfterFunc(200*time.Millisecond, func() {
		h.debounceMu.Lock()
		h.debounceTimer = nil
		h.debounceMu.Unlock()
		h.doBroadcastSessionsUpdate()
	})
}

func (h *Hub) doBroadcastSessionsUpdate() {
	data, err := json.Marshal(wsServerMsg{Type: "sessions_update"})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.authenticated.Load() {
			c.sendRaw(data)
		}
	}
}

// BroadcastCronResult notifies all connected WS clients that a cron job completed.
func (h *Hub) BroadcastCronResult(jobID, _, _ string) {
	data, err := json.Marshal(map[string]string{
		"type":   "cron_result",
		"job_id": jobID,
	})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.authenticated.Load() {
			c.sendRaw(data)
		}
	}
}

// Shutdown closes all WebSocket client connections and relays.
func (h *Hub) Shutdown() {
	h.cancel() // cancel in-flight send goroutines

	// Stop debounce timer
	h.debounceMu.Lock()
	if h.debounceTimer != nil {
		h.debounceTimer.Stop()
		h.debounceTimer = nil
	}
	h.debounceMu.Unlock()

	// Close node connections under nodesMu
	h.nodesMu.RLock()
	nodeConns := make([]NodeConn, 0, len(h.nodes))
	for _, conn := range h.nodes {
		nodeConns = append(nodeConns, conn)
	}
	h.nodesMu.RUnlock()
	for _, conn := range nodeConns {
		conn.Close()
	}

	// Close client connections
	h.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(h.clients))
	for c := range h.clients {
		for _, unsub := range c.subscriptions {
			unsub()
		}
		c.subscriptions = nil
		if c.conn != nil {
			conns = append(conns, c.conn)
		}
		delete(h.clients, c)
	}
	h.mu.Unlock()

	for _, conn := range conns {
		conn.Close()
	}
}

// ─── Remote node handlers ────────────────────────────────────────────────────

func (h *Hub) handleRemoteSubscribe(c *wsClient, msg wsClientMsg) {
	h.nodesMu.RLock()
	conn, ok := h.nodes[msg.Node]
	h.nodesMu.RUnlock()
	if !ok {
		c.sendJSON(wsServerMsg{Type: "error", Key: msg.Key, Error: "unknown node: " + msg.Node})
		return
	}
	conn.Subscribe(c, msg.Key, msg.After)
}

func (h *Hub) handleRemoteUnsubscribe(c *wsClient, msg wsClientMsg) {
	h.nodesMu.RLock()
	conn, ok := h.nodes[msg.Node]
	h.nodesMu.RUnlock()
	if !ok {
		c.sendJSON(wsServerMsg{Type: "unsubscribed", Key: msg.Key, Node: msg.Node})
		return
	}
	conn.Unsubscribe(c, msg.Key)
}

func (h *Hub) handleRemoteSend(c *wsClient, msg wsClientMsg) {
	nodeID := msg.Node
	h.nodesMu.RLock()
	nc, ok := h.nodes[nodeID]
	h.nodesMu.RUnlock()

	if !ok {
		c.sendJSON(wsServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "unknown node: " + nodeID})
		return
	}

	c.sendJSON(wsServerMsg{Type: "send_ack", ID: msg.ID, Status: "accepted", Key: msg.Key, Node: nodeID})

	go func() {
		ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
		defer cancel()
		if err := nc.Send(ctx, msg.Key, msg.Text, msg.Workspace); err != nil {
			slog.Error("remote ws send failed", "node", nodeID, "key", msg.Key, "err", err)
		}
		h.BroadcastSessionsUpdate()
	}()
}

// PurgeNodeSubscriptions notifies all browser clients that a node disconnected,
// so they can deselect stale sessions.
func (h *Hub) PurgeNodeSubscriptions(nodeID string) {
	data, err := json.Marshal(wsServerMsg{Type: "error", Node: nodeID, Error: "node disconnected"})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.authenticated.Load() {
			c.sendRaw(data)
		}
	}
}
