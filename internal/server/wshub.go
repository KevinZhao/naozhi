package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// Hub manages WebSocket client connections and event subscriptions.
type Hub struct {
	mu          sync.RWMutex
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

	// Per-IP rate limiter for WebSocket auth attempts — prevents token brute-force
	// via repeated connect/auth/disconnect cycles that bypass HTTP login rate limits.
	wsAuthLimiter func(ip string) *rate.Limiter

	trustedProxy bool // trust X-Forwarded-For for client IP extraction
	upgrader     websocket.Upgrader

	debounceMu    sync.Mutex
	debounceTimer *time.Timer
}

// HubOptions holds configuration for a Hub.
type HubOptions struct {
	Router        *session.Router
	Agents        map[string]session.AgentOpts
	AgentCmds     map[string]string
	DashToken     string
	CookieMAC     string
	Guard         *session.Guard
	Nodes         map[string]node.Conn
	NodesMu       *sync.RWMutex
	ProjectMgr    *project.Manager
	AllowedRoot   string
	TrustedProxy  bool
	WSAuthLimiter func(ip string) *rate.Limiter
}

// Pre-marshaled static messages to avoid repeated JSON serialization.
var sessionsUpdateMsg, _ = json.Marshal(node.ServerMsg{Type: "sessions_update"})

// NewHub creates a new WebSocket hub.
func NewHub(opts HubOptions) *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		clients:       make(map[*wsClient]struct{}),
		router:        opts.Router,
		agents:        opts.Agents,
		agentCmds:     opts.AgentCmds,
		dashToken:     opts.DashToken,
		cookieMAC:     opts.CookieMAC,
		guard:         opts.Guard,
		nodes:         opts.Nodes,
		nodesMu:       opts.NodesMu,
		projectMgr:    opts.ProjectMgr,
		allowedRoot:   opts.AllowedRoot,
		trustedProxy:  opts.TrustedProxy,
		wsAuthLimiter: opts.WSAuthLimiter,
		ctx:           ctx,
		cancel:        cancel,
	}
	h.upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true // same-origin requests omit Origin
			}
			u, err := url.Parse(origin)
			if err != nil {
				return false
			}
			host := r.Host
			if h.trustedProxy {
				if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
					host = strings.SplitN(fwd, ",", 2)[0]
				}
			}
			return u.Host == host
		},
		ReadBufferSize:  8192,
		WriteBufferSize: 8192,
	}
	return h
}

// SetScheduler sets the cron scheduler for auto-saving prompts on first send.
func (h *Hub) SetScheduler(s *cron.Scheduler) { h.scheduler = s }

// HandleUpgrade upgrades an HTTP connection to WebSocket.
func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	// Reject upgrades when too many connections are open (prevent resource exhaustion
	// from unauthenticated connections allocating goroutines + channel buffers).
	h.mu.RLock()
	count := len(h.clients)
	h.mu.RUnlock()
	if count >= 500 {
		http.Error(w, "too many WebSocket connections", http.StatusServiceUnavailable)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("ws upgrade failed", "err", err)
		return
	}
	conn.SetReadLimit(512 * 1024) // 512 KB max message size
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if h.trustedProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				ip = last
			}
		}
	}
	c := &wsClient{
		conn:          conn,
		send:          make(chan []byte, 1024),
		hub:           h,
		remoteIP:      ip,
		sendLimiter:   rate.NewLimiter(rate.Every(time.Second), 5), // 5 sends/s burst, 1/s sustained
		subscriptions: make(map[string]func()),
		subGen:        make(map[string]uint64),
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
	// Per-IP rate limit to prevent brute-force via rapid connect/auth/disconnect cycles.
	if h.wsAuthLimiter != nil && !h.wsAuthLimiter(c.remoteIP).Allow() {
		c.SendJSON(node.ServerMsg{Type: "auth_fail", Error: "too many attempts"})
		return
	}
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
	if sess != nil {
		h.completeSubscribe(c, key, msg, sess)
		return
	}

	c.SendJSON(node.ServerMsg{Type: "error", Key: key, Error: "session not found"})
}

// completeSubscribe finishes a subscription once a valid session is available.
func (h *Hub) completeSubscribe(c *wsClient, key string, msg node.ClientMsg, sess *session.ManagedSession) {
	if !sess.HasProcess() {
		// No process yet (suspended/resuming). Send persisted history so the
		// client can display old messages, and reply with "subscribed" so the
		// client's _pendingSubscribeKey is properly cleared. Without this
		// response the client gets stuck and never re-subscribes when the
		// process becomes available.
		snap := sess.Snapshot()
		c.SendJSON(node.ServerMsg{Type: "subscribed", Key: key, State: snap.State})

		var entries []cli.EventEntry
		if msg.After > 0 {
			entries = sess.EventEntriesSince(msg.After)
		} else {
			entries = sess.EventLastN(100)
		}
		if len(entries) > 0 {
			c.SendJSON(node.ServerMsg{Type: "history", Key: key, Events: entries})
		}
		slog.Debug("completeSubscribe: no process, sent persisted history", "key", key, "entries", len(entries))
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
	c.subGen[key]++
	gen := c.subGen[key]
	h.mu.Unlock()

	snap := sess.Snapshot()

	var entries []cli.EventEntry
	if msg.After > 0 {
		entries = sess.EventEntriesSince(msg.After)
	} else {
		// Limit initial history to the most recent 100 events to keep
		// JSON serialization and client-side rendering fast.
		entries = sess.EventLastN(100)
	}

	slog.Debug("completeSubscribe: sending history", "key", key, "entries", len(entries), "state", snap.State)
	c.SendJSON(node.ServerMsg{Type: "subscribed", Key: key, State: snap.State})

	var lastTime int64
	if len(entries) > 0 {
		c.SendJSON(node.ServerMsg{Type: "history", Key: key, Events: entries})
		lastTime = entries[len(entries)-1].Time
	}

	go h.eventPushLoop(c, key, gen, notify, sess, lastTime)
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

	capturedID, capturedKey := msg.ID, key
	_, err := h.sessionSend(sendParams{
		Key:       key,
		Text:      msg.Text,
		Workspace: msg.Workspace,
		ResumeID:  msg.ResumeID,
	}, func(errMsg string) {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: capturedID, Status: "error", Key: capturedKey, Error: errMsg})
	})
	if err != nil {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: err.Error()})
		return
	}
	c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "accepted", Key: key})
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

func (h *Hub) eventPushLoop(c *wsClient, key string, gen uint64, notify <-chan struct{}, sess *session.ManagedSession, lastTime int64) {
	for {
		select {
		case _, ok := <-notify:
			if !ok {
				ok, newSess := h.resubscribeEvents(c, key, gen, &notify)
				if !ok {
					return
				}
				sess = newSess
				// Catch up on events we missed during the transition.
				entries := sess.EventEntriesSince(lastTime)
				if len(entries) > 0 {
					data, err := json.Marshal(node.ServerMsg{Type: "history", Key: key, Events: entries})
					if err == nil {
						c.SendRaw(data)
					}
					lastTime = entries[len(entries)-1].Time
				}
				continue
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

// resubscribeEvents waits for a new process to be attached to the session and
// re-subscribes to its EventLog. Returns (ok, currentSession). ok is false if
// the client disconnects, the wait times out (60s), or a newer subscription
// has taken over this key (generation mismatch).
func (h *Hub) resubscribeEvents(c *wsClient, key string, gen uint64, notify *<-chan struct{}) (bool, *session.ManagedSession) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range 12 {
		select {
		case <-c.done:
			return false, nil
		case <-ticker.C:
		}

		// Check if a newer subscription (from handleSubscribe) has taken over.
		h.mu.RLock()
		currentGen := c.subGen[key]
		h.mu.RUnlock()
		if currentGen != gen {
			return false, nil
		}

		// Re-check the router for the current session — spawnSession may have
		// created a new ManagedSession, replacing the old one in the map.
		currentSess := h.router.GetSession(key)
		if currentSess == nil {
			continue
		}

		newNotify, unsub := currentSess.SubscribeEvents()
		// Check if the channel is immediately closed (process still nil).
		select {
		case _, ok := <-newNotify:
			if !ok {
				// Process still nil — clean up subscriber slot and keep waiting.
				unsub()
				continue
			}
			// Process is back and has events.
		default:
			// Channel is alive (not closed) — process is back.
		}

		// Update the subscription registration for this client.
		h.mu.Lock()
		if c.subscriptions == nil {
			// Client was removed during Shutdown.
			h.mu.Unlock()
			unsub()
			return false, nil
		}
		// Final generation check under write lock to prevent TOCTOU.
		if c.subGen[key] != gen {
			h.mu.Unlock()
			unsub()
			return false, nil
		}
		if oldUnsub, exists := c.subscriptions[key]; exists {
			oldUnsub()
		}
		c.subscriptions[key] = unsub
		h.mu.Unlock()

		*notify = newNotify
		return true, currentSess
	}
	return false, nil
}

// broadcastState sends a session_state message to ALL authenticated clients.
// This mirrors BroadcastSessionReady: the "running" start is sent to everyone,
// so the final state must also reach everyone — otherwise clients not subscribed
// to this session would see a stale "running" dot in the sidebar forever.
func (h *Hub) broadcastState(key, state, reason string) {
	data, err := json.Marshal(node.ServerMsg{Type: "session_state", Key: key, State: state, Reason: reason})
	if err != nil {
		return
	}
	h.mu.RLock()
	targets := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		if c.authenticated.Load() {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		c.SendRaw(data)
	}
}

// BroadcastSessionReady sends a session_state "running" to ALL authenticated clients
// so they can auto-subscribe. Unlike broadcastState, this is not limited to already-
// subscribed clients — needed for new sessions where nobody is subscribed yet.
func (h *Hub) BroadcastSessionReady(key string) {
	data, err := json.Marshal(node.ServerMsg{Type: "session_state", Key: key, State: "running"})
	if err != nil {
		return
	}
	h.mu.RLock()
	targets := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		if c.authenticated.Load() {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		c.SendRaw(data)
	}
}

// BroadcastSessionsUpdate debounces notifications: resets a 50ms timer on each
// call; the actual broadcast fires only when no further calls arrive within the window.
func (h *Hub) BroadcastSessionsUpdate() {
	h.debounceMu.Lock()
	defer h.debounceMu.Unlock()
	if h.debounceTimer != nil {
		h.debounceTimer.Reset(50 * time.Millisecond)
		return
	}
	h.debounceTimer = time.AfterFunc(50*time.Millisecond, func() {
		h.debounceMu.Lock()
		h.debounceTimer = nil
		h.debounceMu.Unlock()
		h.doBroadcastSessionsUpdate()
	})
}

func (h *Hub) doBroadcastSessionsUpdate() {
	data := sessionsUpdateMsg
	h.mu.RLock()
	targets := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		if c.authenticated.Load() {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		c.SendRaw(data)
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
	h.mu.RLock()
	targets := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		if c.authenticated.Load() {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		c.SendRaw(data)
	}
}

// DroppedMessages returns the total number of messages dropped across all clients.
func (h *Hub) DroppedMessages() int64 {
	var total int64
	h.mu.RLock()
	for c := range h.clients {
		total += c.dropped.Load()
	}
	h.mu.RUnlock()
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

	// send_ack is deferred until nc.Send returns, so the remote session
	// is guaranteed to exist when the browser receives the ack and triggers
	// a subscribe. Sending the ack eagerly (before the RPC) caused a race
	// where the subscribe arrived at the remote before session creation.
	go func() {
		ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
		defer cancel()
		capturedID, capturedKey := msg.ID, msg.Key
		if err := nc.Send(ctx, capturedKey, msg.Text, msg.Workspace); err != nil {
			slog.Error("remote ws send failed", "node", nodeID, "key", capturedKey, "err", err)
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: capturedID, Status: "error", Key: capturedKey, Node: nodeID, Error: "remote send failed: " + err.Error()})
		} else {
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: capturedID, Status: "accepted", Key: capturedKey, Node: nodeID})
			// Refresh the remote subscription so the connector re-creates
			// its streamEvents goroutine if the previous one exited (e.g.
			// process died between the last subscribe and this send).
			nc.RefreshSubscription(capturedKey)
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
	h.mu.RLock()
	targets := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		if c.authenticated.Load() {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		c.SendRaw(data)
	}
}
