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

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/node"
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
		// Behind CloudFront/ALB, r.Host is the ALB hostname while Origin
		// carries the real domain. Check X-Forwarded-Host first.
		// NOTE: In direct-access scenarios (no reverse proxy), a client can
		// forge X-Forwarded-Host to bypass origin checks. This is mitigated
		// by the auth layer (cookie or auth message required).
		host := r.Host
		if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
			host = strings.SplitN(fwd, ",", 2)[0] // take first entry only
		}
		return u.Host == host
	},
	ReadBufferSize:  8192,
	WriteBufferSize: 8192,
}

// Hub manages WebSocket client connections and event subscriptions.
type Hub struct {
	mu          sync.Mutex
	clients     map[*wsClient]struct{}
	router      *session.Router
	agents      map[string]session.AgentOpts
	agentCmds   map[string]string
	dashToken   string
	cookieMAC   string // HMAC-derived cookie value (different from dashToken)
	guard       *session.Guard
	nodes       map[string]node.Conn
	nodesMu     *sync.RWMutex // shared with Server.nodesMu — all nodes map access must use this
	projectMgr  *project.Manager
	scheduler   *cron.Scheduler // optional, for cron prompt auto-save
	allowedRoot string          // workspace paths must be under this root (empty = unrestricted)
	ctx         context.Context // cancelled on Shutdown to stop in-flight sends
	cancel      context.CancelFunc

	debounceMu    sync.Mutex
	debounceTimer *time.Timer
}

// HubOptions holds configuration for a Hub.
type HubOptions struct {
	Router      *session.Router
	Agents      map[string]session.AgentOpts
	AgentCmds   map[string]string
	DashToken   string
	CookieMAC   string
	Guard       *session.Guard
	Nodes       map[string]node.Conn
	NodesMu     *sync.RWMutex
	ProjectMgr  *project.Manager
	AllowedRoot string
}

// NewHub creates a new WebSocket hub.
func NewHub(opts HubOptions) *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{
		clients:     make(map[*wsClient]struct{}),
		router:      opts.Router,
		agents:      opts.Agents,
		agentCmds:   opts.AgentCmds,
		dashToken:   opts.DashToken,
		cookieMAC:   opts.CookieMAC,
		guard:       opts.Guard,
		nodes:       opts.Nodes,
		nodesMu:     opts.NodesMu,
		projectMgr:  opts.ProjectMgr,
		allowedRoot: opts.AllowedRoot,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// SetScheduler sets the cron scheduler for auto-saving prompts on first send.
func (h *Hub) SetScheduler(s *cron.Scheduler) { h.scheduler = s }

// HandleUpgrade upgrades an HTTP connection to WebSocket.
func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("ws upgrade failed", "err", err)
		return
	}
	conn.SetReadLimit(512 * 1024) // 512 KB max message size
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
		if h.cookieMAC != "" && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(h.cookieMAC)) == 1 {
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
	nodes := make([]node.Conn, 0, len(h.nodes))
	for _, conn := range h.nodes {
		nodes = append(nodes, conn)
	}
	h.nodesMu.RUnlock()

	for _, conn := range nodes {
		conn.RemoveClient(c)
	}
}

func (h *Hub) handleAuth(c *wsClient, msg node.ClientMsg) {
	if h.dashToken == "" || subtle.ConstantTimeCompare([]byte(msg.Token), []byte(h.dashToken)) == 1 {
		c.authenticated.Store(true)
		c.SendJSON(node.ServerMsg{Type: "auth_ok"})
	} else if c.authenticated.Load() {
		// Already pre-authenticated via cookie during upgrade — accept.
		c.SendJSON(node.ServerMsg{Type: "auth_ok"})
	} else {
		c.SendJSON(node.ServerMsg{Type: "auth_fail", Error: "invalid token"})
	}
}

func (h *Hub) handleSubscribe(c *wsClient, msg node.ClientMsg) {
	key := msg.Key
	if key == "" {
		c.SendJSON(node.ServerMsg{Type: "error", Error: "key is required"})
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
		c.SendJSON(node.ServerMsg{Type: "error", Key: key, Error: "session not found"})
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

	c.SendJSON(node.ServerMsg{Type: "subscribed", Key: key, State: snap.State})

	var lastTime int64
	if len(entries) > 0 {
		c.SendJSON(node.ServerMsg{Type: "history", Key: key, Events: entries})
		lastTime = entries[len(entries)-1].Time
	}

	go h.eventPushLoop(c, key, notify, sess, lastTime)
}

func (h *Hub) handleUnsubscribe(c *wsClient, msg node.ClientMsg) {
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
	c.SendJSON(node.ServerMsg{Type: "unsubscribed", Key: key})
}

func (h *Hub) handleSend(c *wsClient, msg node.ClientMsg) {
	// Remote node delegation
	if msg.Node != "" && msg.Node != "local" {
		h.handleRemoteSend(c, msg)
		return
	}

	key := msg.Key
	if key == "" {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "key is required"})
		return
	}
	if msg.Text == "" {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "text is required"})
		return
	}

	// Handle /clear from dashboard — CLI built-in doesn't work in stream-json
	trimmed := strings.TrimSpace(msg.Text)
	if trimmed == "/clear" || trimmed == "/new" {
		h.router.Reset(key)
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "accepted", Key: key})
		h.BroadcastSessionsUpdate()
		return
	}

	// Set workspace override for new dashboard sessions
	var validatedWorkspace string
	if msg.Workspace != "" {
		wsPath, err := validateWorkspace(msg.Workspace, h.allowedRoot)
		if err != nil {
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: err.Error()})
			return
		}
		validatedWorkspace = wsPath
		if idx := strings.LastIndexByte(key, ':'); idx >= 0 {
			chatKey := key[:idx]
			h.router.SetWorkspace(chatKey, wsPath)
		}
	}

	// Register for resume: pre-create a suspended entry so GetOrCreate
	// will --resume the specified session ID on the first message.
	if msg.ResumeID != "" && discovery.IsValidSessionID(msg.ResumeID) {
		ws := validatedWorkspace
		if ws == "" {
			ws = h.router.DefaultWorkspace()
		}
		h.router.RegisterForResume(key, msg.ResumeID, ws)
	}

	acquired := h.guard.TryAcquire(key)
	needInterrupt := !acquired

	if needInterrupt {
		// Session is running — interrupt; the goroutine below will wait for the guard
		h.router.InterruptSession(key)
		slog.Info("ws send: interrupted running session for new message", "key", key)
	}

	c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "accepted", Key: key})

	capturedText := msg.Text
	go func() {
		if needInterrupt {
			// Wait for the interrupted turn to release the guard
			if !h.guard.AcquireTimeout(h.ctx, key, 5*time.Second) {
				slog.Error("ws send: interrupt timed out", "key", key)
				c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Key: key, Error: "session busy, interrupt timed out"})
				return
			}
		}
		defer h.guard.Release(key)
		defer h.router.NotifyIdle() // wake Shutdown wait loop

		ctx := h.ctx
		opts := buildSessionOpts(key, h.agents, h.projectMgr)
		sess, _, err := h.router.GetOrCreate(ctx, key, opts)
		if err != nil {
			slog.Error("ws send: get session", "key", key, "err", err)
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Key: key, Error: err.Error()})
			return
		}

		if _, err := h.sendWithBroadcast(ctx, key, sess, capturedText, nil, nil); err != nil {
			slog.Error("ws send: send", "key", key, "err", err)
		} else if h.scheduler != nil && strings.HasPrefix(key, "cron:") {
			if err := h.scheduler.SetJobPrompt(strings.TrimPrefix(key, "cron:"), capturedText); err != nil {
				slog.Warn("ws send: set cron prompt", "key", key, "err", err)
			}
		}
	}()
}

func (h *Hub) handleInterrupt(c *wsClient, msg node.ClientMsg) {
	key := msg.Key
	if key == "" {
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "error", Error: "key is required"})
		return
	}

	ok := h.router.InterruptSession(key)
	if ok {
		slog.Info("session interrupted via dashboard", "key", key)
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "ok", Key: key})
	} else {
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "not_running", Key: key})
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
			if len(entries) == 0 {
				continue
			}
			select {
			case <-c.done:
				return
			default:
			}
			// Batch events into a single "history" message to reduce
			// per-event JSON marshaling and WebSocket frame overhead.
			data, err := json.Marshal(node.ServerMsg{Type: "history", Key: key, Events: entries})
			if err != nil {
				continue
			}
			c.SendRaw(data)
			lastTime = entries[len(entries)-1].Time
		case <-c.done:
			return
		}
	}
}

// broadcastState sends a session_state message to all clients subscribed to the given key.
func (h *Hub) broadcastState(key, state, reason string) {
	data, err := json.Marshal(node.ServerMsg{Type: "session_state", Key: key, State: state, Reason: reason})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if _, ok := c.subscriptions[key]; ok {
			c.SendRaw(data)
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
	data, err := json.Marshal(node.ServerMsg{Type: "sessions_update"})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.authenticated.Load() {
			c.SendRaw(data)
		}
	}
}

// BroadcastCronResult notifies all connected WS clients that a cron job completed.
func (h *Hub) BroadcastCronResult(jobID, result, errMsg string) {
	payload := map[string]string{
		"type":   "cron_result",
		"job_id": jobID,
	}
	if result != "" {
		payload["result"] = result
	}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.authenticated.Load() {
			c.SendRaw(data)
		}
	}
}

// DroppedMessages returns the total number of messages dropped across all clients.
func (h *Hub) DroppedMessages() int64 {
	var total int64
	h.mu.Lock()
	for c := range h.clients {
		total += c.dropped.Load()
	}
	h.mu.Unlock()
	return total
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
	nodeConns := make([]node.Conn, 0, len(h.nodes))
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

func (h *Hub) handleRemoteSubscribe(c *wsClient, msg node.ClientMsg) {
	h.nodesMu.RLock()
	conn, ok := h.nodes[msg.Node]
	h.nodesMu.RUnlock()
	if !ok {
		c.SendJSON(node.ServerMsg{Type: "error", Key: msg.Key, Error: "unknown node: " + msg.Node})
		return
	}
	conn.Subscribe(c, msg.Key, msg.After)
}

func (h *Hub) handleRemoteUnsubscribe(c *wsClient, msg node.ClientMsg) {
	h.nodesMu.RLock()
	conn, ok := h.nodes[msg.Node]
	h.nodesMu.RUnlock()
	if !ok {
		c.SendJSON(node.ServerMsg{Type: "unsubscribed", Key: msg.Key, Node: msg.Node})
		return
	}
	conn.Unsubscribe(c, msg.Key)
}

func (h *Hub) handleRemoteSend(c *wsClient, msg node.ClientMsg) {
	nodeID := msg.Node
	h.nodesMu.RLock()
	nc, ok := h.nodes[nodeID]
	h.nodesMu.RUnlock()

	if !ok {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "unknown node: " + nodeID})
		return
	}

	c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "accepted", Key: msg.Key, Node: nodeID})

	go func() {
		ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
		defer cancel()
		capturedID, capturedKey := msg.ID, msg.Key
		if err := nc.Send(ctx, msg.Key, msg.Text, msg.Workspace); err != nil {
			slog.Error("remote ws send failed", "node", nodeID, "key", msg.Key, "err", err)
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: capturedID, Status: "error", Key: capturedKey, Node: nodeID, Error: "remote send failed: " + err.Error()})
		}
		h.BroadcastSessionsUpdate()
	}()
}

// PurgeNodeSubscriptions notifies all browser clients that a node disconnected,
// so they can deselect stale sessions.
func (h *Hub) PurgeNodeSubscriptions(nodeID string) {
	data, err := json.Marshal(node.ServerMsg{Type: "error", Node: nodeID, Error: "node disconnected"})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.authenticated.Load() {
			c.SendRaw(data)
		}
	}
}
