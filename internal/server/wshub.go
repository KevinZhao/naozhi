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
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// Hub manages WebSocket client connections and event subscriptions.
type Hub struct {
	mu sync.RWMutex
	// connCount mirrors len(h.clients) for the unauthenticated connection
	// cap. Accessed via atomic Add with a reserve-then-check pattern so
	// the 500-connection gate is both observed and reserved in a single
	// step, closing the TOCTOU window where a burst of simultaneous
	// upgrades all observed `count < 500` under a plain RLock and then
	// landed past the cap. Over-shoot (Add then decrement) is bounded by
	// one slot per rejected upgrade and is preferred over a CAS loop
	// because Add is lock-free on all supported architectures.
	connCount   atomic.Int64
	clients     map[*wsClient]struct{}
	router      *session.Router
	agents      map[string]session.AgentOpts
	agentCmds   map[string]string
	dashToken   string
	cookieMAC   string // HMAC-derived cookie value (different from dashToken)
	guard       *session.Guard
	queue       *dispatch.MessageQueue // per-key FIFO queue for dashboard sends
	nodes       map[string]node.Conn
	nodesMu     *sync.RWMutex // shared with Server.nodesMu — all nodes map access must use this
	projectMgr  *project.Manager
	scheduler   *cron.Scheduler // optional, for cron prompt auto-save
	uploadStore *uploadStore    // optional, for resolving WS-sent file_ids
	allowedRoot string          // workspace paths must be under this root (empty = unrestricted)
	ctx         context.Context // cancelled on Shutdown to stop in-flight sends
	cancel      context.CancelFunc
	// sendWG tracks background send goroutines (ownerLoop, sessionSendLegacy)
	// so Shutdown can wait for them to exit before returning. Without this,
	// goroutines may read router/session after Shutdown tears them down.
	sendWG sync.WaitGroup

	// sendTrackMu + sendClosed serialise a late Add(1) with Shutdown's
	// Wait. External code paths (e.g. HTTP handleSend -> remote proxy) do
	// not live behind clientWG, so they need their own barrier against
	// Shutdown completing its sendWG.Wait before an Add lands. Call
	// TrackSend instead of sendWG.Add directly from those paths.
	sendTrackMu sync.Mutex
	sendClosed  bool

	// clientWG tracks per-client readPump/writePump/eventPushLoop goroutines
	// plus the debounce AfterFunc callback. Shutdown blocks on this so no
	// client-driven goroutine accesses router/nodes/clients maps after they
	// have been torn down. Tracked separately from sendWG because the
	// pump lifecycle is owned by the connection (closed via conn.Close)
	// while sendWG is owned by the send code path (canceled via ctx).
	clientWG sync.WaitGroup

	// Per-IP rate limiter for WebSocket auth attempts — prevents token brute-force
	// via repeated connect/auth/disconnect cycles that bypass HTTP login rate limits.
	// Returns true when the IP is allowed; false signals rate-limit hit.
	wsAuthLimiter func(ip string) bool

	trustedProxy bool // trust X-Forwarded-For for client IP extraction
	upgrader     websocket.Upgrader

	debounceMu    sync.Mutex
	debounceTimer *time.Timer
	debounceFirst time.Time // first trigger in the current debounce window
	// debounceClosed is set under debounceMu during Shutdown so any post-close
	// BroadcastSessionsUpdate caller does not register a new AfterFunc + Add
	// into clientWG after Shutdown has already passed its drain point. Without
	// this, a broadcast arriving between Shutdown's debounceMu release and
	// its clientWG.Wait could schedule a callback that never gets Waited on,
	// or worse, add to clientWG after Wait has already returned.
	debounceClosed bool
}

// HubOptions holds configuration for a Hub.
type HubOptions struct {
	Router        *session.Router
	Agents        map[string]session.AgentOpts
	AgentCmds     map[string]string
	DashToken     string
	CookieMAC     string
	Guard         *session.Guard
	Queue         *dispatch.MessageQueue
	Nodes         map[string]node.Conn
	NodesMu       *sync.RWMutex
	ProjectMgr    *project.Manager
	AllowedRoot   string
	TrustedProxy  bool
	WSAuthLimiter func(ip string) bool
}

// Pre-marshaled static messages to avoid repeated JSON serialization.
var sessionsUpdateMsg []byte

func init() {
	var err error
	sessionsUpdateMsg, err = json.Marshal(node.ServerMsg{Type: "sessions_update"})
	if err != nil {
		panic("sessionsUpdateMsg: " + err.Error())
	}
}

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
		queue:         opts.Queue,
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
			// When behind a trusted proxy (e.g. CloudFront), the upstream Host
			// is rewritten to the origin hostname, so r.Host no longer matches
			// the browser-visible Origin. Fall back to X-Forwarded-Host, which
			// the trusted proxy controls — trusted_proxy=true is the operator's
			// explicit statement that this header can be trusted.
			host := r.Host
			if h.trustedProxy {
				if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
					// RFC 7239 permits whitespace around commas; trim so a
					// proxy emitting "host.example , other.example" still
					// matches r.Host on the comparison below.
					host = strings.TrimSpace(strings.SplitN(fwd, ",", 2)[0])
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

// SetUploadStore wires the upload store used by WS sends to resolve file_ids
// that were pre-uploaded via POST /api/sessions/upload.
func (h *Hub) SetUploadStore(s *uploadStore) { h.uploadStore = s }

// HandleUpgrade upgrades an HTTP connection to WebSocket.
func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	// Per-IP rate limit at the upgrade boundary so a single IP cannot burn
	// through the 500 global connection budget via rapid connect/disconnect
	// cycles. Reuses the same login limiter wiring (5 attempts/min burst).
	if h.wsAuthLimiter != nil {
		// loginAllow maps "" to a shared unknown-IP bucket, so do not skip
		// the check on empty IP — that would let malformed RemoteAddr bypass
		// the per-IP budget entirely.
		if !h.wsAuthLimiter(clientIP(r, h.trustedProxy)) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
	}
	// Reject upgrades when too many connections are open (prevent resource exhaustion
	// from unauthenticated connections allocating goroutines + channel buffers).
	// Reserve the slot atomically: the previous RLock/check/unlock sequence was a
	// TOCTOU window where a concurrent burst could all observe count < cap and
	// all complete the upgrade. CAS on connCount collapses the gate into one step.
	const maxWSConns = 500
	if n := h.connCount.Add(1); n > maxWSConns {
		h.connCount.Add(-1)
		http.Error(w, "too many WebSocket connections", http.StatusServiceUnavailable)
		return
	}
	// Release the reserved slot on any pre-register failure path.
	slotReleased := false
	defer func() {
		if !slotReleased {
			h.connCount.Add(-1)
		}
	}()

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Capture origin + remote IP so operators can diagnose
		// CheckOrigin rejections or attribute floods to a specific client
		// without digging through raw request logs.
		slog.Debug("ws upgrade failed",
			"err", err,
			"remote", clientIP(r, h.trustedProxy),
			"origin", r.Header.Get("Origin"),
			"host", r.Host)
		return
	}
	// Read-limit is owned by readPump (wsMaxMessageSize). Previous code also
	// set it here with a different value (512 KB), which masked the real cap
	// since readPump re-applies 256 KB on first iteration — remove the
	// redundant setter to keep a single source of truth.
	ip := clientIP(r, h.trustedProxy)
	c := &wsClient{
		conn: conn,
		// 256 is sized for brief latency spikes; slow consumers drop rather
		// than balloon memory (per-message cap is wsMaxMessageSize = 256KB,
		// so 256 × 256KB = 64MB worst-case per client, vs 256MB at 1024).
		send:             make(chan []byte, 256),
		hub:              h,
		remoteIP:         ip,
		sendLimiter:      rate.NewLimiter(rate.Every(time.Second), 5), // 5 sends/s burst, 1/s sustained
		interruptLimiter: rate.NewLimiter(rate.Every(200*time.Millisecond), 3),
		subscriptions:    make(map[string]func()),
		subGen:           make(map[string]uint64),
		done:             make(chan struct{}),
	}
	if h.dashToken == "" {
		c.authenticated.Store(true)
		c.uploadOwner = ip // unauthenticated: owner = client IP (matches uploadOwner fallback)
	} else if cookie, err := r.Cookie(authCookieName); err == nil {
		if h.cookieMAC != "" && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(h.cookieMAC)) == 1 {
			c.authenticated.Store(true)
			// Must use the same derivation as HTTP uploadOwner so files
			// uploaded on one transport can be claimed on the other.
			c.uploadOwner = ownerKeyFromCookie(cookie.Value)
		}
	}
	// Arm clientWG BEFORE registering the client, not after. If Shutdown
	// runs between register() and Add(2), it could snapshot h.clients,
	// close the conn, observe clientWG count == 0, and return before the
	// pumps ever increment — leaving them to run past teardown and
	// use-after-free router/hub state. Add is cheap and always balanced
	// by the deferred Done() in the pump goroutines below.
	h.clientWG.Add(2)
	h.register(c)
	// Ownership of the connCount slot transfers to register/unregister:
	// mark the slot as released here so the defer on the upgrade path
	// doesn't double-decrement. unregister() will Add(-1) when this
	// client eventually disconnects.
	slotReleased = true
	go func() { defer h.clientWG.Done(); c.writePump() }()
	go func() { defer h.clientWG.Done(); c.readPump() }()
}

func (h *Hub) register(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) unregister(c *wsClient) {
	h.mu.Lock()
	removed := false
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		for _, unsub := range c.subscriptions {
			unsub()
		}
		c.subscriptions = nil
		removed = true
	}
	h.mu.Unlock()
	if removed {
		// Release the connCount slot reserved at upgrade time. Guarded on
		// `removed` so a double-unregister (stale close path) cannot leak
		// the counter into negative territory.
		h.connCount.Add(-1)
	}

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
	if h.wsAuthLimiter != nil && !h.wsAuthLimiter(c.remoteIP) {
		c.SendJSON(node.ServerMsg{Type: "auth_fail", Error: "too many attempts"})
		return
	}
	// Short-circuit when the connection is already authenticated via cookie —
	// do not touch msg.Token or run the ConstantTimeCompare so the
	// cookie-authed and token-authed paths are cleanly separated.
	if c.authenticated.Load() {
		c.SendJSON(node.ServerMsg{Type: "auth_ok"})
		return
	}
	if h.dashToken == "" || subtle.ConstantTimeCompare([]byte(msg.Token), []byte(h.dashToken)) == 1 {
		c.authenticated.Store(true)
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

	// Per-connection subscription cap to prevent goroutine accumulation.
	// Reserve the slot atomically under h.mu so two concurrent subscribe
	// requests at capacity N-1 cannot both pass the check and end up at N+1.
	// The reservation is a nil-unsub placeholder that completeSubscribe will
	// overwrite with the real unsub closure; if subscription setup fails
	// before that, the placeholder would leak — but downstream code always
	// writes SOME value or sends an error back to the client without
	// returning early between here and completeSubscribe.
	h.mu.Lock()
	if _, alreadySub := c.subscriptions[key]; !alreadySub && len(c.subscriptions) >= 50 {
		h.mu.Unlock()
		c.SendJSON(node.ServerMsg{Type: "error", Key: key, Error: "too many subscriptions"})
		return
	}
	// Unsubscribe from previous subscription
	if unsub, ok := c.subscriptions[key]; ok {
		unsub()
		delete(c.subscriptions, key)
	}
	// Reserve the slot: placeholder keeps the map-length accurate for
	// concurrent cap checks until completeSubscribe replaces it with the
	// real unsub. If we return via the "session not found" path below, we
	// clear the reservation before returning.
	c.subscriptions[key] = func() {}
	h.mu.Unlock()

	sess := h.router.GetSession(key)
	if sess != nil {
		h.completeSubscribe(c, key, msg, sess)
		return
	}

	// Session not found: release the placeholder reservation. Only this
	// goroutine can have installed the placeholder for this key above, and
	// since sess was nil the completeSubscribe branch cannot replace it, so
	// an unconditional delete is safe.
	h.mu.Lock()
	delete(c.subscriptions, key)
	h.mu.Unlock()

	c.SendJSON(node.ServerMsg{Type: "error", Key: key, Error: "session not found"})
}

// completeSubscribe finishes a subscription once a valid session is available.
func (h *Hub) completeSubscribe(c *wsClient, key string, msg node.ClientMsg, sess *session.ManagedSession) {
	if !sess.HasProcess() {
		// No process yet (suspended/resuming). Send persisted history so the
		// client can display old messages, and reply with "subscribed" so the
		// client's _pendingSubscribeKey is properly cleared. Without this
		// response the client gets stuck and never re-subscribes when the
		// process becomes available. Release the reserved slot since there is
		// no real unsub to install; the client can always resubscribe.
		h.mu.Lock()
		delete(c.subscriptions, key)
		h.mu.Unlock()

		snap := sess.Snapshot()
		c.SendJSON(node.ServerMsg{Type: "subscribed", Key: key, State: snap.State, Reason: "suspended"})

		var entries []cli.EventEntry
		if msg.After > 0 {
			entries = sess.EventEntriesSince(msg.After)
		} else {
			entries = sess.EventLastN(0)
		}
		if len(entries) > 0 {
			c.SendJSON(node.ServerMsg{Type: "history", Key: key, Events: entries})
		}
		slog.Debug("completeSubscribe: no process, sent persisted history", "key", key, "entries", len(entries))
		return
	}
	// Fast-fail if Shutdown already fired: SubscribeEvents would otherwise
	// register a subscriber on an EventLog whose process is being torn
	// down, and the unsub callback may never run (h.ctx.Done() in the
	// push-loop arm is the downstream guard, but avoiding the subscribe
	// entirely is cleaner).
	if h.ctx.Err() != nil {
		h.mu.Lock()
		delete(c.subscriptions, key)
		h.mu.Unlock()
		return
	}
	notify, unsub := sess.SubscribeEvents()

	h.mu.Lock()
	// Re-check ctx under the lock: the earlier fast-fail check was racy
	// with Shutdown's h.mu-guarded subscription teardown; if Shutdown
	// acquired h.mu between the fast-fail check and this Lock, clients
	// subscriptions was niled and the first branch below handles it.
	// But Shutdown's sequence is cancel() -> h.mu.Lock() -> iterate
	// subscriptions, so ctx.Err() being set here is a strong signal that
	// Shutdown is mid-flight; decline to start a new pushLoop.
	if c.subscriptions == nil || h.ctx.Err() != nil {
		h.mu.Unlock()
		unsub()
		return
	}
	c.subscriptions[key] = unsub
	c.subGen[key]++
	gen := c.subGen[key]
	// Add to clientWG BEFORE releasing h.mu. Shutdown walks h.clients under
	// h.mu to close conns, then calls clientWG.Wait; if we Add(1) after
	// releasing here, Shutdown's Wait can return before the eventPushLoop
	// goroutine ever starts, and the goroutine can then touch torn-down state.
	h.clientWG.Add(1)
	h.mu.Unlock()

	snap := sess.Snapshot()

	var entries []cli.EventEntry
	if msg.After > 0 {
		entries = sess.EventEntriesSince(msg.After)
	} else {
		entries = sess.EventLastN(0)
	}

	slog.Debug("completeSubscribe: sending history", "key", key, "entries", len(entries), "state", snap.State)
	c.SendJSON(node.ServerMsg{Type: "subscribed", Key: key, State: snap.State})

	var lastTime int64
	if len(entries) > 0 {
		c.SendJSON(node.ServerMsg{Type: "history", Key: key, Events: entries})
		lastTime = entries[len(entries)-1].Time
	} else if snap.State == "running" {
		// Always send an (empty) history for running sessions so the client's
		// _initialSubscribe flag is consumed. Without this, the client shows a
		// blank events area until eventPushLoop delivers the first batch, which
		// can be a noticeable delay if the process just started.
		c.SendJSON(node.ServerMsg{Type: "history", Key: key, Events: []cli.EventEntry{}})
	}

	go func() {
		defer h.clientWG.Done()
		h.eventPushLoop(c, key, gen, notify, sess, lastTime)
	}()
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
		// Intentionally keep c.subGen[key] intact: a stale eventPushLoop from
		// this subscription may still be parked in resubscribeEvents' ticker
		// (up to 60s). Deleting subGen[key] and allowing a new subscribe to
		// reset the counter to 1 would let the stale goroutine's gen=1 match
		// the fresh subGen[key]=1 and silently resume. The per-connection
		// subscription cap already bounds map growth, and the map is freed
		// wholesale when the wsClient is torn down.
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
	if msg.Text == "" && len(msg.FileIDs) == 0 {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "text or files required"})
		return
	}
	if len(msg.FileIDs) > 10 {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "too many files (max 10)"})
		return
	}

	// Resolve pre-uploaded file IDs — ownership-checked to prevent cross-user theft.
	var images []cli.ImageData
	if len(msg.FileIDs) > 0 {
		if h.uploadStore == nil {
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "uploads not configured"})
			return
		}
		for _, fid := range msg.FileIDs {
			img := h.uploadStore.Take(fid, c.uploadOwner)
			if img == nil {
				// Never echo fid (user-controlled) back in the error; log internally.
				slog.Debug("ws send: file_id not found or expired", "fid", fid)
				c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "file not found or expired"})
				return
			}
			images = append(images, *img)
		}
	}

	capturedID, capturedKey := msg.ID, key
	reset, status, err := h.sessionSend(sendParams{
		Key:       key,
		Text:      msg.Text,
		Images:    images,
		Workspace: msg.Workspace,
		ResumeID:  msg.ResumeID,
	}, func(errMsg string) {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: capturedID, Status: "error", Key: capturedKey, Error: errMsg})
	})
	if err != nil {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: err.Error()})
		return
	}
	if reset {
		// /clear or /new — HTTP path reports "reset"; keep the WS path in sync so
		// clients can uniformly distinguish reset from accepted/queued turns
		// instead of seeing an empty Status string.
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "reset", Key: key})
		return
	}
	c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: string(status), Key: key})
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
				// resubscribeEvents may consume one pending notification while
				// probing newNotify (ok=true path) — if we didn't catch-up
				// unconditionally here, those events would only surface on the
				// next Append, which in an idle session may be seconds or more.
				entries := sess.EventEntriesSince(lastTime)
				if len(entries) > 0 {
					data, err := marshalPooled(node.ServerMsg{Type: "history", Key: key, Events: entries})
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
			data, err := marshalPooled(node.ServerMsg{Type: "history", Key: key, Events: entries})
			if err != nil {
				continue
			}
			c.SendRaw(data)
			lastTime = entries[len(entries)-1].Time
		case <-c.done:
			return
		case <-h.ctx.Done():
			// Hub shutdown: exit even if the client hasn't closed and the
			// subscribed notify channel is stalled. Without this arm, a
			// notify source that stops firing could park this goroutine
			// past Shutdown, with no escape until conn.Close propagates
			// through readPump — which may not happen if the socket is
			// half-open.
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
		case <-h.ctx.Done():
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
	// Timed out waiting for new process — notify client so the dashboard
	// can surface a "subscription expired" indicator instead of silently
	// showing stale state. Clean up the dead subscription slot so it doesn't
	// count toward the per-connection cap.
	h.mu.Lock()
	if c.subscriptions != nil {
		if oldUnsub, exists := c.subscriptions[key]; exists {
			oldUnsub()
			delete(c.subscriptions, key)
		}
	}
	h.mu.Unlock()
	c.SendJSON(node.ServerMsg{Type: "session_state", Key: key, State: "ready", Reason: "subscription_timeout"})
	return false, nil
}

// broadcastClientSnapPool reuses the []*wsClient backing array across
// broadcasts so high-frequency session_state / sessions_update traffic does
// not allocate one slice per broadcast. Entries that grow beyond 256 clients
// are dropped on Put so the pool never pins a large backing array.
var broadcastClientSnapPool = sync.Pool{
	New: func() any {
		s := make([]*wsClient, 0, 32)
		return &s
	},
}

// broadcastToAuthenticated sends raw data to all authenticated WebSocket clients.
// Takes a pointer snapshot under RLock and releases the lock before the per-
// client channel sends. SendRaw itself is non-blocking, but with hundreds of
// clients a loop under RLock still serialises `register`/`unregister` behind
// every broadcast; snapshotting removes that contention amplifier and the
// backing slice is reused via sync.Pool so steady-state broadcasts are zero-alloc.
func (h *Hub) broadcastToAuthenticated(data []byte) {
	snapPtr := broadcastClientSnapPool.Get().(*[]*wsClient)
	snap := (*snapPtr)[:0]

	h.mu.RLock()
	for c := range h.clients {
		if c.authenticated.Load() {
			snap = append(snap, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range snap {
		c.SendRaw(data)
	}
	// Drop oversized snapshots so the pool never pins arbitrarily large
	// backing arrays (e.g. after a one-off broadcast to 5000 clients).
	if cap(snap) <= 256 {
		for i := range snap {
			snap[i] = nil // clear pointers so clients are GC-eligible
		}
		*snapPtr = snap[:0]
		broadcastClientSnapPool.Put(snapPtr)
	}
}

// broadcastState sends a session_state message to ALL authenticated clients.
// This mirrors BroadcastSessionReady: the "running" start is sent to everyone,
// so the final state must also reach everyone — otherwise clients not subscribed
// to this session would see a stale "running" dot in the sidebar forever.
func (h *Hub) broadcastState(key, state, reason string) {
	data, err := marshalPooled(node.ServerMsg{Type: "session_state", Key: key, State: state, Reason: reason})
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
}

// BroadcastSessionReady sends a session_state "running" to ALL authenticated clients
// so they can auto-subscribe. Unlike broadcastState, this is not limited to already-
// subscribed clients — needed for new sessions where nobody is subscribed yet.
func (h *Hub) BroadcastSessionReady(key string) {
	data, err := marshalPooled(node.ServerMsg{Type: "session_state", Key: key, State: "running"})
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
}

// BroadcastSessionsUpdate debounces notifications: resets a 50ms timer on each
// call; the actual broadcast fires only when no further calls arrive within the
// window. A 500ms hard cap on the total debounce window guarantees the update
// eventually fires even under sustained bursts, so clients never miss a refresh.
func (h *Hub) BroadcastSessionsUpdate() {
	const (
		debounceInterval = 50 * time.Millisecond
		maxDebounceDelay = 500 * time.Millisecond
	)
	h.debounceMu.Lock()
	defer h.debounceMu.Unlock()
	// Shutdown already drained the debounce WG slot; any new scheduling here
	// would either leak (callback never waited for) or race clientWG.Wait.
	if h.debounceClosed {
		return
	}
	now := time.Now()
	if h.debounceTimer != nil {
		if now.Sub(h.debounceFirst) >= maxDebounceDelay {
			// Hard cap reached — let the pending timer fire without resetting.
			return
		}
		// time.Timer.Reset on a timer whose AfterFunc already fired but whose
		// callback is still blocked on debounceMu would schedule a SECOND run
		// without a matching clientWG.Add — breaking the Shutdown Wait and
		// producing a negative clientWG count. Stop() returns false if the
		// callback already ran or is scheduled to run; in that case we treat
		// the in-flight callback as the one that will do the broadcast and
		// skip rescheduling. The callback clears debounceTimer under
		// debounceMu, so subsequent calls will start a fresh timer.
		if h.debounceTimer.Stop() {
			h.debounceTimer.Reset(debounceInterval)
		}
		return
	}
	h.debounceFirst = now
	// Track the AfterFunc callback via clientWG so Shutdown can wait for
	// any late-firing broadcast to finish touching the clients map. The
	// callback still runs even after Stop() if it had already fired and
	// was scheduled, so the tracking guards against a post-Shutdown race.
	h.clientWG.Add(1)
	h.debounceTimer = time.AfterFunc(debounceInterval, func() {
		defer h.clientWG.Done()
		h.debounceMu.Lock()
		h.debounceTimer = nil
		h.debounceMu.Unlock()
		h.doBroadcastSessionsUpdate()
	})
}

func (h *Hub) doBroadcastSessionsUpdate() {
	data := sessionsUpdateMsg
	h.broadcastToAuthenticated(data)
}

// BroadcastCronResult notifies all connected WS clients that a cron job completed.
func (h *Hub) BroadcastCronResult(jobID, result, errMsg string) {
	msg := struct {
		Type   string `json:"type"`
		JobID  string `json:"job_id"`
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}{
		Type:   "cron_result",
		JobID:  jobID,
		Result: result,
		Error:  errMsg,
	}
	data, err := marshalPooled(msg)
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
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

// TrackSend reserves a sendWG slot for a background send goroutine and
// returns a release function plus a shuttingDown flag. When shuttingDown
// is true the caller MUST abort (do not spawn the goroutine). This closes
// the window where an HTTP handler could sendWG.Add(1) after Shutdown's
// sendWG.Wait had already drained.
func (h *Hub) TrackSend() (release func(), shuttingDown bool) {
	h.sendTrackMu.Lock()
	defer h.sendTrackMu.Unlock()
	if h.sendClosed {
		return func() {}, true
	}
	h.sendWG.Add(1)
	return h.sendWG.Done, false
}

// Shutdown closes all WebSocket client connections and relays.
func (h *Hub) Shutdown() {
	h.cancel() // cancel in-flight send goroutines

	// Stop debounce timer. Any pending AfterFunc callback is tracked via
	// clientWG, so Wait below will drain callbacks that fired before Stop.
	// When Stop() returns true the callback was cancelled before running,
	// so the clientWG slot we reserved for it must be released here —
	// otherwise clientWG.Wait below would hang forever.
	h.debounceMu.Lock()
	// Block any further AfterFunc scheduling first; then drain the pending
	// timer (if any). Setting the flag before Stop() ensures a concurrent
	// BroadcastSessionsUpdate that holds debounceMu next cannot wedge a
	// new WG slot past our upcoming clientWG.Wait.
	h.debounceClosed = true
	if h.debounceTimer != nil {
		if h.debounceTimer.Stop() {
			h.clientWG.Done()
		}
		h.debounceTimer = nil
	}
	h.debounceMu.Unlock()

	// Close client connections first, then wait for their pumps/eventPushLoop
	// to exit. Closing the underlying conn triggers readPump/writePump to
	// return, which in turn calls closeDone() so eventPushLoop unblocks.
	// Without this ordering, closing node/router state before the pumps
	// exit could cause use-after-close in unregister → RemoveClient.
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

	// Now that conns are closed, pumps will observe the read/write error
	// and exit their loops; eventPushLoop sees h.ctx.Done() or c.done.
	// Wait bounds the shutdown on explicit goroutine lifecycle rather than
	// on the parent context timeout alone.
	h.clientWG.Wait()

	// Barrier: any TrackSend call that observed h.ctx.Err()==nil and was
	// about to Add(1) is racing us. Holding sendTrackMu here forces it to
	// complete either side of this line; once we mark sendClosed, any later
	// caller declines to Add. This closes the window where an HTTP handler
	// goroutine could Add(1) after sendWG.Wait has already drained below.
	h.sendTrackMu.Lock()
	h.sendClosed = true
	h.sendTrackMu.Unlock()

	// Wait for background send goroutines (ownerLoop, handleRemoteSend) to
	// exit AFTER pumps are gone. readPump can call handleRemoteSend on the
	// way out, which does sendWG.Add(1) — so Waiting before pumps drain
	// would race with a late Add that escapes the Wait.
	h.sendWG.Wait()

	// Close node connections under nodesMu after client pumps and send
	// goroutines have exited, so unregister → RemoveClient and in-flight
	// RPCs cannot race a closed node.
	h.nodesMu.RLock()
	nodeConns := make([]node.Conn, 0, len(h.nodes))
	for _, conn := range h.nodes {
		nodeConns = append(nodeConns, conn)
	}
	h.nodesMu.RUnlock()
	for _, conn := range nodeConns {
		conn.Close()
	}
}

// ─── Remote node handlers ────────────────────────────────────────────────────

func (h *Hub) handleRemoteSubscribe(c *wsClient, msg node.ClientMsg) {
	h.nodesMu.RLock()
	conn, ok := h.nodes[msg.Node]
	h.nodesMu.RUnlock()
	if !ok {
		// Do not echo the client-supplied node ID in the error: a careless
		// JS consumer rendering the field via innerHTML would turn a crafted
		// node value into reflected XSS. Log internally for operator triage.
		slog.Debug("ws subscribe: unknown node", "node", msg.Node)
		c.SendJSON(node.ServerMsg{Type: "error", Key: msg.Key, Error: "unknown node"})
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
		// Same rationale as handleRemoteSubscribe: do not reflect the raw
		// client-supplied node ID in the error body.
		slog.Debug("ws send: unknown node", "node", nodeID)
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "unknown node"})
		return
	}

	// send_ack is deferred until nc.Send returns, so the remote session
	// is guaranteed to exist when the browser receives the ack and triggers
	// a subscribe. Sending the ack eagerly (before the RPC) caused a race
	// where the subscribe arrived at the remote before session creation.
	//
	// Track via sendWG so Shutdown waits for in-flight RPC+broadcast to
	// finish before tearing down node connections and client maps. Go via
	// TrackSend so a send initiated just as Shutdown fires is refused here
	// rather than squeezing past the clientWG barrier and then hitting a
	// closed sendWG window.
	release, shuttingDown := h.TrackSend()
	if shuttingDown {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Key: msg.Key, Node: nodeID, Error: "server shutting down"})
		return
	}
	go func() {
		defer release()
		ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
		defer cancel()
		capturedID, capturedKey := msg.ID, msg.Key
		if err := nc.Send(ctx, capturedKey, msg.Text, msg.Workspace); err != nil {
			slog.Error("remote ws send failed", "node", nodeID, "key", capturedKey, "err", err)
			// Do not surface the raw err: transport-level messages can leak
			// internal host/port/auth details back to authenticated browser
			// clients. Operators still see the detail in the slog above.
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: capturedID, Status: "error", Key: capturedKey, Node: nodeID, Error: "remote send failed"})
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
	data, err := marshalPooled(node.ServerMsg{Type: "error", Node: nodeID, Error: "node disconnected"})
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
}
