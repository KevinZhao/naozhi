package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// wsAuthRetryAfterSeconds is the advisory "try again in N seconds" value the
// WS auth_fail rate-limit reply carries. It intentionally mirrors the
// HTTP /api/auth/login Retry-After header (60s) so the front-end can share
// one countdown helper across HTTP login and WS auth paths. The underlying
// limiter refills a token every 12s (burst=5); 60s is a conservative upper
// bound that avoids sending users into another back-to-back 429 loop.
const wsAuthRetryAfterSeconds = 60

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
	connCount atomic.Int64
	// droppedTotal counts messages dropped across all clients (send channel
	// full on SendRaw). DroppedMessages() used to scan the clients map under
	// RLock summing per-client counters, which contended with register/
	// unregister on every /health probe. An atomic counter is lock-free on
	// the hot path and monotonic — acceptable because the existing return
	// value was already eventually-consistent (per-client loads race with
	// concurrent SendRaw drops).
	droppedTotal atomic.Int64
	clients      map[*wsClient]struct{}
	router       *session.Router
	agents       map[string]session.AgentOpts
	agentCmds    map[string]string
	dashToken    string
	cookieMAC    string // HMAC-derived cookie value (different from dashToken)
	guard        *session.Guard
	queue        *dispatch.MessageQueue // per-key FIFO queue for dashboard sends
	nodes        map[string]node.Conn
	nodesMu      *sync.RWMutex // shared with Server.nodesMu — all nodes map access must use this
	projectMgr   *project.Manager
	scheduler    *cron.Scheduler // optional, for cron prompt auto-save
	uploadStore  *uploadStore    // optional, for resolving WS-sent file_ids
	// scratchPool lets sessionOptsFor resolve the inherited AgentOpts for an
	// ephemeral "scratch" key without touching the persistent agent registry.
	// Nil when the scratch feature is disabled (tests, headless mode).
	scratchPool *session.ScratchPool
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
	// ParentCtx is the application-level context whose cancellation must
	// propagate to the Hub. When set, NewHub derives h.ctx via
	// context.WithCancel(ParentCtx) so that parent-ctx cancel tears down
	// send/push goroutines even if Shutdown() is not explicitly called
	// (e.g. a future panic-early-exit path in main that forgets Shutdown).
	// Nil falls back to context.Background() to preserve legacy behaviour
	// for tests and headless wiring. CTX1 (Round 167).
	ParentCtx context.Context
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
//
// h.ctx is derived from opts.ParentCtx when set, otherwise
// context.Background() (legacy behaviour for tests and headless wiring).
// Deriving from a parent ctx means a parent cancel propagates to Hub
// goroutines even if Shutdown() is never called — closes the gap
// documented in CTX1: a future panic-early-exit path that forgets to
// call Shutdown would otherwise leak send/push goroutines. Shutdown()
// still calls h.cancel() explicitly; context.CancelFunc is idempotent
// so the two paths compose without races.
func NewHub(opts HubOptions) *Hub {
	parent := opts.ParentCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
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
		// Delegate to the shared sameOriginOK helper so WS upgrade and the
		// HTTP requireAuth CSRF gate stay in lockstep. The helper already
		// treats empty Origin as permitted (same-origin browsers omit it,
		// non-browser callers don't carry cookies), honours trustedProxy's
		// X-Forwarded-Host fallback, and rejects the opaque "null" origin.
		CheckOrigin:     func(r *http.Request) bool { return sameOriginOK(r, h.trustedProxy) },
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

// SetScratchPool wires the ephemeral-session pool so sessionOptsFor can
// resolve AgentOpts for scratch keys without touching the sidebar-visible
// router state.
func (h *Hub) SetScratchPool(p *session.ScratchPool) { h.scratchPool = p }

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
	// set it here with a different value, which masked the real cap since
	// readPump re-applies wsMaxMessageSize on first iteration — remove the
	// redundant setter to keep a single source of truth.
	ip := clientIP(r, h.trustedProxy)
	c := &wsClient{
		conn: conn,
		// Buffer holds outbound event frames (CLI output, subscription
		// updates). 256 is sized for brief latency spikes so slow consumers
		// drop rather than balloon memory. Outbound "history" batches are
		// capped at maxHistoryPushEntries in eventPushLoop (≤~50 × ~200 B =
		// ~10 KB per frame), so 256 slots × ~10 KB = ~2.5 MB worst-case
		// per-client. R68-PERF-H1.
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

	// Snapshot nodes under nodesMu to avoid data race. Single-node deployments
	// (no remote nodes configured) are the common case, so short-circuit on an
	// empty map to skip a per-disconnect `[]node.Conn{}` allocation. Mobile
	// clients that reconnect frequently made this visible in heap profiles.
	// R46-PERF-UNREGISTER-NODES-ALLOC.
	h.nodesMu.RLock()
	if len(h.nodes) == 0 {
		h.nodesMu.RUnlock()
		return
	}
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
		// Advisory RetryAfter matches the HTTP /api/auth/login 429 branch
		// (dashboard_auth.go writes Retry-After: 60) so WS and HTTP auth
		// lockouts surface identical countdowns on the front end. Clients
		// older than R110-P2 ignore the field; new clients visually gate
		// re-auth until the window elapses.
		c.SendJSON(node.ServerMsg{
			Type:       "auth_fail",
			Error:      "too many attempts",
			RetryAfter: wsAuthRetryAfterSeconds,
		})
		// OBS2: rate-limit-triggered auth_fail counts for brute-force detection
		// the same way invalid-token auth_fail does — operators watching
		// naozhi_ws_auth_fail_total should see both signals blended, and
		// Retry-After tells them whether the limiter is actively engaging.
		metrics.WSAuthFailTotal.Add(1)
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
		// Derive uploadOwner from the provided token so WS token-auth enforces
		// the same per-owner upload quota as HTTP Bearer auth. Without this,
		// a WS-token-authed client could bypass maxUploadPerOwner because
		// uploadOwner stayed "" (empty string matches every "" owner in the
		// store). Mirrors the derivation in dashboard_send.go uploadOwner().
		// R67-SEC-1.
		if c.uploadOwner == "" && msg.Token != "" {
			sum := sha256.Sum256([]byte(msg.Token))
			c.uploadOwner = hex.EncodeToString(sum[:8])
		}
		c.SendJSON(node.ServerMsg{Type: "auth_ok"})
	} else {
		c.SendJSON(node.ServerMsg{Type: "auth_fail", Error: "invalid token"})
		metrics.WSAuthFailTotal.Add(1)
	}
}

func (h *Hub) handleSubscribe(c *wsClient, msg node.ClientMsg) {
	key := msg.Key
	if key == "" {
		c.SendJSON(node.ServerMsg{Type: "error", Error: "key is required"})
		return
	}
	// R175-SEC-P1: same gate the HTTP session handlers enforce
	// (R172-SEC-L2 / R175-SEC-M). Without this, a WS client can post a
	// multi-KB key containing C1 controls or bidi characters that reach
	// slog attrs (router "session not found" path), persist into the
	// per-connection c.subscriptions map, and eventually land in
	// sessions.json at shutdown. ValidateSessionKey also caps length at
	// MaxSessionKeyBytes (~520 B) — the inline loop in sessionSend only
	// rejects ASCII C0/DEL, leaving C1 / bidi / non-UTF-8 as a log-
	// injection class for the WS subscribe path.
	if err := session.ValidateSessionKey(key); err != nil {
		c.SendJSON(node.ServerMsg{Type: "error", Error: "invalid key"})
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
	if sess == nil && h.scheduler != nil && h.scheduler.EnsureStub(key) {
		// Cron stubs are torn down by sidebar "×". Rebuild lazily on click
		// so the user doesn't have to wait for the next scheduled tick to
		// re-open the panel. EnsureStub is a no-op for non-cron keys.
		sess = h.router.GetSession(key)
	}
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
		switch {
		case msg.After > 0:
			entries = sess.EventEntriesSince(msg.After)
		case msg.Limit > 0:
			entries = sess.EventLastN(msg.Limit)
		default:
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
	switch {
	case msg.After > 0:
		entries = sess.EventEntriesSince(msg.After)
	case msg.Limit > 0:
		// Initial subscribe asks for the last `limit` events only — this is
		// the dashboard pagination fast path. Clients walk further back via
		// HTTP /api/sessions/events?before=.. rather than resubscribing.
		entries = sess.EventLastN(msg.Limit)
	default:
		// Legacy path: send everything the log remembers. Kept so older
		// clients (and the node-to-node relay) still see full history.
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

	// R176-SEC-P1: same gate as handleSubscribe / handleInterrupt. Without
	// this, an authenticated WS client can hand-craft a `key` containing
	// C1 / bidi / non-UTF-8 bytes that lands in the echoed
	// `{"type":"unsubscribed","key":...}` reply and any structured log
	// attr on the path. ValidateSessionKey also caps at MaxSessionKeyBytes.
	// Gate BEFORE the remote-node delegation: handleRemoteUnsubscribe reads
	// msg.Key too, so the local-only placement of this check would leave
	// the remote path unguarded.
	if err := session.ValidateSessionKey(key); err != nil {
		c.SendJSON(node.ServerMsg{Type: "error", Error: "invalid key"})
		return
	}

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
	// R183-SEC-M1: the remote-node delegation branch above forwards to
	// handleRemoteSend which re-enters the connector's ValidateSessionKey
	// gate; the local fast path here previously skipped that gate,
	// violating the trust-boundary policy every other ws path (subscribe
	// / unsubscribe / interrupt) already follows. An authenticated WS
	// client could land a multi-KB key containing C1/bidi/newline bytes
	// into the dispatch queue, the sessionSend log attrs, and eventually
	// sessions.json at shutdown. ValidateSessionKey also caps length at
	// MaxSessionKeyBytes (~520 B).
	if err := session.ValidateSessionKey(key); err != nil {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "invalid key"})
		return
	}
	if msg.Text == "" && len(msg.FileIDs) == 0 {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "text or files required"})
		return
	}
	// Per-field byte cap on the WS path. wsMaxMessageSize already bounds the
	// whole JSON frame, but without this inner gate an authenticated client
	// can land repeated max-size payloads into the dispatch queue; when the
	// queue drains, CoalesceMessages concatenates up to MaxDepth entries into
	// a single stdin write. maxCoalescedTextBytes backstops the merged total,
	// and maxStdinLineBytes (12 MB at the shim) is the hard ceiling.
	// R59-SEC-H1.
	if len(msg.Text) > maxWSSendTextBytes {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "text too long"})
		return
	}
	if len(msg.FileIDs) > 10 {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "too many files (max 10)"})
		return
	}

	// Resolve pre-uploaded file IDs — ownership-checked to prevent cross-user theft.
	// Atomic TakeAll: partial failure leaves the store untouched so the user
	// can retry with a fresh upload batch rather than silently losing the
	// earlier images. R37-CONCUR4.
	var images []cli.ImageData
	if len(msg.FileIDs) > 0 {
		if h.uploadStore == nil {
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "uploads not configured"})
			return
		}
		taken, err := h.uploadStore.TakeAll(msg.FileIDs, c.uploadOwner)
		if err != nil {
			// Never echo fids (user-controlled) back in the error; log internally.
			slog.Debug("ws send: one or more file_ids not found or expired", "count", len(msg.FileIDs))
			c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "file not found or expired"})
			return
		}
		images = append(images, taken...)
	}

	capturedID, capturedKey := msg.ID, key
	reset, status, err := h.sessionSend(sendParams{
		Key:       key,
		Text:      msg.Text,
		Images:    images,
		Workspace: msg.Workspace,
		ResumeID:  msg.ResumeID,
		Backend:   msg.Backend,
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
	// R175-SEC-P1: same policy as handleSubscribe / HTTP handlers. Reject
	// C1 / bidi / multi-KB keys before they land in router lookup + slog
	// attrs (both local and remote paths).
	if err := session.ValidateSessionKey(key); err != nil {
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "error", Error: "invalid key"})
		return
	}

	// Remote node delegation
	if msg.Node != "" && msg.Node != "local" {
		h.handleRemoteInterrupt(c, msg)
		return
	}

	// Prefer the non-destructive control_request path so the CLI subprocess
	// survives. Raw SIGINT via InterruptSession kills Claude `-p` outright,
	// which tears down the shim and forces a brand-new spawn on the next
	// message (losing resume context and leaking socket files). See
	// Router.InterruptSessionSafe for the full design rationale.
	switch h.router.InterruptSessionSafe(key) {
	case session.InterruptSent:
		slog.Info("session interrupted via dashboard", "key", key)
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "ok", Key: key})
	case session.InterruptNoSession:
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "not_running", Key: key})
	default:
		// control_request returned a non-terminal outcome AND the SIGINT
		// fallback also failed (e.g. session evicted mid-call). Treat as
		// not_running so the dashboard re-queries state.
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "not_running", Key: key})
	}
}

func (h *Hub) handleRemoteInterrupt(c *wsClient, msg node.ClientMsg) {
	if !isValidNodeID(msg.Node) {
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "error", Key: msg.Key, Error: "unknown node"})
		return
	}
	nodeID := msg.Node
	h.nodesMu.RLock()
	nc, ok := h.nodes[nodeID]
	h.nodesMu.RUnlock()
	if !ok {
		slog.Debug("ws interrupt: unknown node", "node", nodeID)
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "error", Key: msg.Key, Error: "unknown node"})
		return
	}

	release, shuttingDown := h.TrackSend()
	if shuttingDown {
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: msg.ID, Status: "error", Key: msg.Key, Node: nodeID, Error: "server shutting down"})
		return
	}
	go func() {
		defer release()
		capturedID, capturedKey := msg.ID, msg.Key
		// R175-SEC-P1: malformed RPC payloads from a compromised node could
		// panic inside ProxyInterruptSession's json decode path; without a
		// recover the whole naozhi service goes down, affecting every other
		// session. Mirror the ownerLoop/readLoop defensive pattern: log and
		// reply "error" so the dashboard surfaces the failure.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("remote ws interrupt goroutine panic",
					"node", nodeID, "key", capturedKey,
					"panic", r, "stack", string(debug.Stack()))
				c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: capturedID,
					Status: "error", Key: capturedKey, Node: nodeID,
					Error: "internal error"})
			}
		}()
		ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
		defer cancel()
		interrupted, err := nc.ProxyInterruptSession(ctx, capturedKey)
		if err != nil {
			slog.Error("remote ws interrupt failed", "node", nodeID, "key", capturedKey, "err", err)
			errMsg := "remote interrupt failed"
			if isUnknownRPCMethodErr(err) {
				// Explicit hint so the dashboard toast tells the operator
				// why the action is rejected instead of burying the cause.
				errMsg = "remote node needs upgrade to support this action"
			}
			c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: capturedID, Status: "error", Key: capturedKey, Node: nodeID, Error: errMsg})
			return
		}
		status := "ok"
		if !interrupted {
			status = "not_running"
		} else {
			slog.Info("remote session interrupted via dashboard", "node", nodeID, "key", capturedKey)
		}
		c.SendJSON(node.ServerMsg{Type: "interrupt_ack", ID: capturedID, Status: status, Key: capturedKey, Node: nodeID})
	}()
}

// maxHistoryPushEntries caps a single WS "history" push. EventEntriesSince
// on an initial catch-up (lastTime=0) or after a notify backlog can return
// the full ring buffer (maxPersistedHistory=500 entries). At ~200 B per
// entry JSON-encoded, a 500-entry batch balloons to ~100 KB per push; with
// 500 active WS connections that is 50 MB of simultaneous marshal work
// blocking the hub. 50 entries matches the dashboard's paginated
// /api/sessions/events tail fetch, so older entries are still reachable
// via the `before=` path. R68-PERF-H1.
const maxHistoryPushEntries = 50

func capHistoryBatch(entries []cli.EventEntry) []cli.EventEntry {
	if len(entries) <= maxHistoryPushEntries {
		return entries
	}
	return entries[len(entries)-maxHistoryPushEntries:]
}

// eventPushLoop is the per-subscription pump that reads EventLog notifications
// and streams entries to the WS client. It owns exactly one clientWG slot for
// its entire lifetime (Add happens in completeSubscribe before go; Done runs
// in the goroutine's defer).
//
// CLIENTWG CONTRACT (R49-CONCUR-RESUBSCRIBE-CLIENTWG): when resubscribeEvents
// transparently swaps `sess` for a new process's session (the `!ok` arm
// below), the loop keeps running in the same goroutine — we do NOT Add(1)
// for the new subscription. This is correct because:
//
//  1. The lifetime being tracked is "this pushLoop goroutine", not "this
//     particular EventLog subscription". A single Add/Done pair covers
//     every successful resubscribe within the goroutine.
//  2. resubscribeEvents installs the new `unsub` into c.subscriptions[key]
//     under h.mu, replacing the stale one — so Hub.Shutdown walking
//     c.subscriptions sees the current generation's unsub without any
//     additional bookkeeping.
//  3. The unsub → notify closure ensures resubscribeEvents returns ok=false
//     on Shutdown (h.ctx.Done is checked), so the goroutine exits and
//     the single deferred Done balances the single Add.
//
// If you ever split the resubscribe path into a new goroutine (e.g. to
// parallelise multi-session fan-in), you MUST Add(1) for the new goroutine
// and Done from its own defer — otherwise Shutdown's clientWG.Wait either
// hangs (Add without Done) or panics with negative counter (Done without
// Add). The guarantee is enforced by code shape, not by assertion; a
// review that simply notes "+1 goroutine here" is insufficient without
// also updating the WG pairing.
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
					entries = capHistoryBatch(entries)
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
			// Cap the batch so a slow client that built up a long backlog
			// doesn't see a single multi-MB push frame that starves the
			// WS send channel — the dashboard already backfills older
			// events via /api/sessions/events?before=. R68-PERF-H1.
			entries = capHistoryBatch(entries)
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
	// Timer.Reset reuses a single timer allocation across the 12 iterations
	// instead of allocating a Ticker and its runtime goroutine; resubscribe
	// is a cold-ish path but client flap can trigger N simultaneous calls.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	for i := range 12 {
		if i > 0 {
			timer.Reset(5 * time.Second)
		}
		select {
		case <-c.done:
			return false, nil
		case <-h.ctx.Done():
			return false, nil
		case <-timer.C:
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
		//
		// H8 (Round 163): capture the old unsub while holding h.mu but call
		// it *after* releasing the lock. The current lock order is
		// h.mu → EventLog.subMu (enforced by Hub.Shutdown's contract and the
		// shutdown_lock_order_test.go tripwire), so calling oldUnsub() under
		// h.mu is technically safe today. Releasing h.mu first removes a
		// latent hazard: if oldUnsub() were ever extended to take additional
		// locks (e.g. a future per-client audit mutex or a sub-layer WG
		// protected by h.mu), calling it under h.mu would reintroduce a
		// reverse acquisition order. Swap is a rare path (resubscribe
		// collision), so the extra unlock/relock has no measurable cost.
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
		oldUnsub := c.subscriptions[key]
		c.subscriptions[key] = unsub
		h.mu.Unlock()
		if oldUnsub != nil {
			oldUnsub()
		}

		*notify = newNotify
		return true, currentSess
	}
	// Timed out waiting for new process — notify client so the dashboard
	// can surface a "subscription expired" indicator instead of silently
	// showing stale state. Clean up the dead subscription slot so it doesn't
	// count toward the per-connection cap.
	//
	// H8 (Round 163): same lock-order precaution — snapshot oldUnsub
	// under h.mu, release the lock, then invoke it.
	h.mu.Lock()
	var staleUnsub func()
	if c.subscriptions != nil {
		if u, exists := c.subscriptions[key]; exists {
			staleUnsub = u
			delete(c.subscriptions, key)
		}
	}
	h.mu.Unlock()
	if staleUnsub != nil {
		staleUnsub()
	}
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
	// Clear *wsClient pointers so a disconnected client can be GC'd before
	// the snap slice is returned to the caller / dropped. Clearing happens
	// on both paths: for the pool-eligible path it prevents stale pointers
	// surviving in the pooled backing array until the next Get; for the
	// oversized path it releases references now instead of waiting for the
	// long-lived parent goroutine's stack frame to unwind. R59-PERF-L1.
	for i := range snap {
		snap[i] = nil
	}
	// Oversized snapshots (e.g. after a one-off broadcast to 5000 clients)
	// would pin an arbitrarily large backing array if returned to the pool.
	// Drop the slice but still return the pointer header with a fresh small
	// backing array so the pool slot is not permanently depleted — otherwise
	// a single "big broadcast" would shrink the pool by one slot until
	// process exit. R58-PERF-005.
	if cap(snap) <= 256 {
		*snapPtr = snap[:0]
	} else {
		*snapPtr = make([]*wsClient, 0, 32)
	}
	broadcastClientSnapPool.Put(snapPtr)
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

// cronResultMsg is the WS payload broadcast on cron job completion. Declared
// as a named type (not an inline anonymous struct) so json/reflect caches the
// type descriptor once across all calls.
type cronResultMsg struct {
	Type   string `json:"type"`
	JobID  string `json:"job_id,omitempty"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// BroadcastCronResult notifies all connected WS clients that a cron job completed.
func (h *Hub) BroadcastCronResult(jobID, result, errMsg string) {
	// R185-SEC-H2: scheduler generates jobID as 8-char hex today, but if a
	// future path ever surfaces a config-supplied / user-typed ID, bidi/C0
	// chars would reach the dashboard via a SetEscapeHTML(false) encoder.
	// Sanitize defensively; result/errMsg are already scrubbed at recordResult.
	data, err := marshalPooled(cronResultMsg{
		Type:   "cron_result",
		JobID:  osutil.SanitizeForLog(jobID, 64),
		Result: result,
		Error:  errMsg,
	})
	if err != nil {
		return
	}
	h.broadcastToAuthenticated(data)
}

// DroppedMessages returns the total number of messages dropped across all
// clients since the process started. Lock-free atomic load; see the struct
// field comment for why this replaced a per-client RLock scan.
func (h *Hub) DroppedMessages() int64 {
	return h.droppedTotal.Load()
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
//
// LOCK ORDER CONTRACT (R35-REL2): Shutdown acquires h.mu while iterating
// c.subscriptions to invoke per-key unsub closures. Each unsub closure
// eventually acquires eventLog.l.subMu (write lock) via
// EventLog.Unsubscribe. The ordering h.mu → eventLog.subMu is only
// deadlock-free as long as NO code path acquires eventLog.subMu first
// and then tries to acquire h.mu.
//
// Current state (2026-04-29): notifySubscribers holds subMu.RLock and
// never touches h.mu; eventPushLoop reads h.clients without h.mu (it
// holds a pointer directly). The ordering invariant therefore holds.
//
// If you add code to eventPushLoop (or any EventLog-driven callback)
// that acquires h.mu, you MUST either:
//
//	(a) release subMu before taking h.mu, or
//	(b) refactor Shutdown to snapshot c.subscriptions under h.mu and
//	    invoke the unsub closures after releasing h.mu.
//
// Breaking the invariant produces a classic ABBA deadlock on shutdown
// that shows up as systemd TimeoutStopSec expiry (30s) followed by
// SIGKILL — observable but extremely confusing to diagnose.
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
	removed := 0
	for c := range h.clients {
		for _, unsub := range c.subscriptions {
			unsub()
		}
		c.subscriptions = nil
		if c.conn != nil {
			conns = append(conns, c.conn)
		}
		delete(h.clients, c)
		removed++
	}
	h.mu.Unlock()
	// Keep connCount in sync with h.clients. conn.Close() below triggers
	// readPump/writePump exit → unregister, but unregister's decrement is
	// guarded on h.clients membership which we just cleared, so without this
	// Add(-N) connCount stays elevated until process exit. Only matters if
	// the Hub is reused (tests) but keeps maxWSConns admission accurate.
	if removed > 0 {
		h.connCount.Add(int64(-removed))
	}

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
	// Reject malformed node IDs BEFORE calling slog to prevent log injection
	// via ANSI/newline bytes in the attacker-controlled Node field. HTTP
	// handlers rely on LookupNode for the same guard; the WS paths bypassed
	// it because the map lookup itself does not validate.
	if !isValidNodeID(msg.Node) {
		c.SendJSON(node.ServerMsg{Type: "error", Key: msg.Key, Error: "unknown node"})
		return
	}
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
	if !isValidNodeID(msg.Node) {
		// Mirror the success shape so slow clients can drop state even when
		// the node ID is malformed — behaviour equivalent to "no such node".
		c.SendJSON(node.ServerMsg{Type: "unsubscribed", Key: msg.Key})
		return
	}
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
	if !isValidNodeID(msg.Node) {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Error: "unknown node"})
		return
	}
	// Syntactic workspace validation on the primary. Even though the remote
	// node runs its own EvalSymlinks check, that check uses the remote's
	// defaults; a node whose defaultWorkspace is unconfigured would pass
	// any absolute path through. Reject traversal / control-byte / oversize
	// inputs here so no primary-authenticated dashboard user can have a
	// remote node bind e.g. `/etc` as a Claude workspace. R61-SEC-2.
	if err := validateRemoteWorkspace(msg.Workspace); err != nil {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Key: msg.Key, Error: "invalid workspace"})
		return
	}
	// Enforce the same per-field text cap as handleSend. Without this gate an
	// authenticated dashboard user who targets a remote node can bypass the
	// local cap and push up to wsMaxMessageSize bytes straight to nc.Send,
	// amplifying input into the remote shim's 12 MB stdin line ceiling via
	// coalesce at the remote end. R62-SEC-1.
	if len(msg.Text) > maxWSSendTextBytes {
		c.SendJSON(node.ServerMsg{Type: "send_ack", ID: msg.ID, Status: "error", Key: msg.Key, Error: "text too long"})
		return
	}
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
		capturedID, capturedKey := msg.ID, msg.Key
		// R175-SEC-P1: same rationale as handleRemoteInterrupt — a panic
		// inside nc.Send (e.g. malformed RPC response from a compromised
		// node) would otherwise take the whole naozhi service down.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("remote ws send goroutine panic",
					"node", nodeID, "key", capturedKey,
					"panic", r, "stack", string(debug.Stack()))
				c.SendJSON(node.ServerMsg{Type: "send_ack", ID: capturedID,
					Status: "error", Key: capturedKey, Node: nodeID,
					Error: "internal error"})
			}
		}()
		ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
		defer cancel()
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
