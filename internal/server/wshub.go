package server

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/agentlink"
)

// Hub manages WebSocket client connections and event subscriptions.
//
// Field-block contract: fields are grouped into 7 blocks — lifecycle
// (mu / ctx / cancel), subscriber, broadcast, send, shared deps, agent
// tailer, rate-limit/cache. Methods live in wshub_<block>.go and write only
// their own block, declared by a `WRITES:` / `READS-ALSO:` godoc marker
// (tools/lint-server-handlers rule 3a); NewHub / Shutdown are the
// LIFECYCLE-METHOD cross-block exemption. Do not extract sub-structs
// opportunistically — each block carries its own lock-ordering contract
// (shutdown_lock_order_test.go).
type Hub struct {
	mu sync.RWMutex
	// connCount mirrors len(h.clients) for the connection cap; reserved with
	// an atomic Add before the check so concurrent upgrades cannot all pass
	// `count < cap` and land past it (over-shoot is decremented).
	connCount atomic.Int64
	// droppedTotal is the Hub-wide count of SendRaw drops (send channel
	// full). Deliberately aggregated, not per-client: it is exported only
	// via the auth-gated /health, and all authenticated users share one trust
	// boundary. Multi-tenant auth would require moving it behind debug_mode (#1100).
	droppedTotal atomic.Int64
	// legacySendInvokes counts sessionSend falls-through to sessionSendLegacy
	// (nil queue); production steady state must read zero (#710).
	legacySendInvokes atomic.Int64
	clients           map[*wsClient]struct{}
	// authClients mirrors clients whose authenticated flag is true so
	// broadcastToAuthenticated skips the handshake-pending majority.
	// Guarded by authMu, nested INSIDE h.mu by the writers (register /
	// markAuthenticated / unregister / Shutdown); the broadcast read side
	// takes authMu alone and never h.mu, so there is no inverse order.
	// Nil on hand-rolled test hubs ⇒ legacy h.clients scan (#1409, #1621).
	authMu      sync.RWMutex
	authClients map[*wsClient]struct{}
	// authClientsSlice + authClientsIdx mirror authClients as a contiguous
	// slice (copy() instead of a map walk on the broadcast hot path) with an
	// index for O(1) swap-delete. Maintained under authMu by the same
	// writers; nil when authClients is nil (#2310).
	authClientsSlice []*wsClient
	authClientsIdx   map[*wsClient]int
	// subscriberCount is the per-key subscriber count backing the
	// maxSubscribersPerKey cap; mutated under h.mu with c.subscriptions,
	// cleared on Shutdown (#716).
	subscriberCount map[string]int
	// subscriberCountFast is a lock-free mirror of subscriberCount for the
	// event-push hot path (singleSubscriber). The map stays the source of
	// truth; every mutation goes through bump/decSubscriberCountLocked or the
	// Shutdown clear. A one-critical-section-stale read only affects the
	// marshal-cache routing heuristic, never correctness (#1522).
	subscriberCountFast sync.Map // key string -> *atomic.Int32
	// enforceCaps gates the per-key subscriber cap and counter reads. NewHub
	// sets it true; hand-rolled test hubs leave it false so the cap never
	// fires against an uninitialised map. The per-client cap is
	// unconditional and does not consult it. Read under h.mu (#1401).
	enforceCaps bool
	// router is the HubRouter consumer subset (consumer.go) so tests can
	// inject a fake.
	router    HubRouter
	agents    map[string]session.AgentOpts
	agentCmds map[string]string
	dashToken string
	// dashTokenHash is sha256(dashToken) for constant-time auth comparison.
	// Immutable after construction: rotating dashToken requires a restart.
	dashTokenHash [32]byte
	// cookieMAC is a getter (not a snapshot) so RotateCookieGen reaches the
	// WS upgrade comparison without a Hub rebuild, matching the HTTP path
	// (#1398). NewHub wraps a static CookieMAC when no getter is supplied.
	cookieMAC func() string
	guard     *session.Guard
	// queue is the MessageEnqueuer interface, not *dispatch.MessageQueue, so
	// tests can swap it. NewHub keeps a nil concrete queue a nil interface:
	// send.go's `h.queue == nil` legacy-fallback gate depends on it (#377).
	queue MessageEnqueuer // per-key FIFO queue for dashboard sends
	// nodes is the same registry instance as Server.nodes (pinned by
	// hub_shared_state_test.go); the registry owns its mutex.
	nodes      *nodeRegistry
	projectMgr *project.Manager
	// resolver centralises session key → opts derivation; nil keeps the
	// inline fallback for tests.
	resolver *session.KeyResolver
	// scheduler is the narrow CronView hook: stub revival on re-subscribe
	// and first-prompt auto-save. Nil keeps both dormant.
	scheduler   CronView
	uploadStore *uploadStore // optional, for resolving WS-sent file_ids
	// scratchPool resolves inherited AgentOpts for ephemeral "scratch" keys;
	// nil when the feature is disabled.
	scratchPool *session.ScratchPool
	allowedRoot string          // workspace paths must be under this root (empty = unrestricted)
	ctx         context.Context // cancelled on Shutdown to stop in-flight sends
	cancel      context.CancelFunc
	// sendWG tracks background send goroutines so Shutdown can wait for them
	// before tearing down router/session state.
	sendWG sync.WaitGroup

	// sendTrackMu + sendClosed serialise a late Add(1) with Shutdown's Wait.
	// CONTRACT: every goroutine registered on sendWG MUST go through
	// TrackSend() and honour its shuttingDown result; a direct sendWG.Add(1)
	// can outlive Shutdown and dereference torn-down maps.
	sendTrackMu sync.Mutex
	sendClosed  bool

	// clientWG tracks per-client pump/eventPushLoop goroutines plus the
	// debounce callback; owned by the connection lifecycle (conn.Close),
	// whereas sendWG is owned by the send path (ctx cancel).
	clientWG sync.WaitGroup

	// wsAuthLimiter gates the inner `auth` WS message (credential test);
	// wsUpgradeLimiter gates the handshake itself, which fires legitimately
	// on tab-reload / mobile-wake, so the two must not share a bucket.
	// Both return true when the IP is allowed.
	wsAuthLimiter    func(ip string) bool
	wsUpgradeLimiter func(ip string) bool

	trustedProxy bool // trust X-Forwarded-For for client IP extraction
	// auth lets HandleUpgrade mint a per-browser nz_anon cookie in no-token
	// mode so the WS upload owner matches the HTTP path's per-browser bucket
	// instead of the client IP, which co-NAT clients share (#1326). Optional
	// for test hubs that run no uploadStore.
	auth     *auth.Handlers
	upgrader websocket.Upgrader

	debounceMu sync.Mutex
	// debounceTimer is allocated once in NewHub and re-armed with Reset; nil
	// on hand-rolled hubs, which fall back to per-call time.AfterFunc (#1624).
	debounceTimer *time.Timer
	// debounceArmed is true while a debounce window is pending and clientWG
	// holds the matching Add(1). Written only under debounceMu.
	debounceArmed bool
	debounceFirst time.Time // first trigger in the current debounce window
	// debounceClosed (under debounceMu) stops post-Shutdown broadcasts from
	// adding to clientWG after Shutdown's drain point.
	debounceClosed bool
	// debounceClosedFast is a lock-free read-side mirror of debounceClosed;
	// written only under debounceMu so "flag set ⇒ no new clientWG.Add(1)"
	// still holds (#723).
	debounceClosedFast atomic.Bool
	// debounceFire is the AfterFunc callback bound once in NewHub.
	debounceFire func()

	// tailers is the agentTailer registry behind agent_subscribe /
	// agent_unsubscribe; initialised by NewHub, torn down in Shutdown.
	tailers *tailerRegistry

	// wiredLinkersMu + wiredLinkers dedup OnResolve / task_done callback
	// registration across re-subscribes. Keys dedup on (dynamic type,
	// value), so a producer MUST pass one canonical AgentLinker per
	// cli.Process; a thin adapter type would double-fire OnResolve (#372).
	// Shutdown nils the map so linkers can be GC'd.
	wiredLinkersMu sync.Mutex
	wiredLinkers   map[agentlink.AgentLinker]struct{}

	// historyMarshalCache lets N tabs on one session pay one "history" frame
	// marshal per notify wave; cleared on last unsubscribe per key and on
	// Shutdown (see wshub_eventpush_cache.go).
	historyMarshalCache *historyMarshalCache

	// userSendLimiters buckets the WS send budget by uploadOwner so N tabs
	// cannot multiply the per-connection burst N×; the per-conn limiter is
	// still the floor. Nil pointer ⇒ not enabled (hand-built hubs); Shutdown
	// stores nil atomically so in-flight callers see live map or nil (#888).
	userSendLimiters atomic.Pointer[sync.Map] // map[string]*rate.Limiter

	// connCountByOwner enforces the per-uploadOwner sub-cap
	// (maxConnsPerOwner) so one token holder cannot monopolise maxWSConns;
	// reserved at HandleUpgrade, released at unregister. Owner "" skips
	// the cap (#1022).
	connCountByOwnerMu sync.Mutex
	connCountByOwner   map[string]int
}

// HubOptions holds configuration for a Hub.
type HubOptions struct {
	Router    *session.Router
	Agents    map[string]session.AgentOpts
	AgentCmds map[string]string
	DashToken string
	// CookieMAC is a static auth-cookie HMAC for tests without AuthHandlers.
	CookieMAC string
	// CookieMACFn, when non-nil, is preferred over CookieMAC so each WS
	// upgrade reads the live auth.CookieMAC() and cookie rotation is not
	// bypassed via WS (#1398).
	CookieMACFn func() string
	Guard       *session.Guard
	Queue       *dispatch.MessageQueue
	// Nodes is the Server-owned node registry. Nil (bare test Hubs) gets a
	// private empty registry so every nodes access stays nil-safe.
	Nodes      *nodeRegistry
	ProjectMgr *project.Manager
	// Resolver, when non-nil, gives WS subscribe/send the same planner-binding
	// precedence as IM dispatch; nil falls back to the inline merge.
	Resolver *session.KeyResolver
	// Scheduler is the optional CronView hook; nil keeps stub revival and
	// prompt auto-save dormant.
	Scheduler CronView
	// ScratchPool resolves AgentOpts for ephemeral scratch keys.
	ScratchPool      *session.ScratchPool
	AllowedRoot      string
	TrustedProxy     bool
	WSAuthLimiter    func(ip string) bool
	WSUpgradeLimiter func(ip string) bool
	// Auth, when non-nil, lets HandleUpgrade mint a per-browser nz_anon
	// cookie in no-token mode so WS upload-owner derivation mirrors HTTP (#1326).
	Auth *auth.Handlers
	// ParentCtx, when set, parents h.ctx so an application cancel tears down
	// send/push goroutines even if Shutdown() is never called. Nil ⇒ Background.
	ParentCtx context.Context
}

// NewHub creates a new WebSocket hub (LIFECYCLE-METHOD: writes every field
// block). h.ctx derives from opts.ParentCtx (Background when nil) so a parent
// cancel reaches Hub goroutines even without Shutdown(); CancelFunc is
// idempotent so both paths compose.
func NewHub(opts HubOptions) *Hub {
	parent := opts.ParentCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	// Always install a getter so the WS upgrade path's `h.CookieMAC() != ""`
	// guard never hits a nil callback.
	cookieMACFn := opts.CookieMACFn
	if cookieMACFn == nil {
		staticMAC := opts.CookieMAC
		cookieMACFn = func() string { return staticMAC }
	}
	nodes := opts.Nodes
	if nodes == nil {
		nodes = newNodeRegistry(nil)
	}
	h := &Hub{
		clients:          make(map[*wsClient]struct{}),
		authClients:      make(map[*wsClient]struct{}),
		authClientsIdx:   make(map[*wsClient]int),
		subscriberCount:  make(map[string]int),
		enforceCaps:      true,
		router:           opts.Router,
		agents:           opts.Agents,
		agentCmds:        opts.AgentCmds,
		dashToken:        opts.DashToken,
		cookieMAC:        cookieMACFn,
		guard:            opts.Guard,
		nodes:            nodes,
		projectMgr:       opts.ProjectMgr,
		resolver:         opts.Resolver,
		scheduler:        opts.Scheduler,
		scratchPool:      opts.ScratchPool,
		allowedRoot:      opts.AllowedRoot,
		trustedProxy:     opts.TrustedProxy,
		wsAuthLimiter:    opts.WSAuthLimiter,
		wsUpgradeLimiter: opts.WSUpgradeLimiter,
		auth:             opts.Auth,
		ctx:              ctx,
		cancel:           cancel,
	}
	h.upgrader = websocket.Upgrader{
		// Shared with the HTTP CSRF gate so both stay in lockstep (empty
		// Origin permitted, "null" rejected, X-Forwarded-Host under trustedProxy).
		CheckOrigin:     func(r *http.Request) bool { return auth.SameOriginOK(r, h.trustedProxy) },
		ReadBufferSize:  8192,
		WriteBufferSize: 8192,
	}
	if opts.DashToken != "" {
		h.dashTokenHash = sha256.Sum256([]byte(opts.DashToken))
	}
	h.tailers = newTailerRegistry(h)
	h.wiredLinkers = make(map[agentlink.AgentLinker]struct{})
	h.historyMarshalCache = newHistoryMarshalCache()
	h.userSendLimiters.Store(&sync.Map{})
	h.connCountByOwner = make(map[string]int)
	// Bound once so the high-frequency BroadcastSessionsUpdate path does not
	// allocate a closure per call.
	h.debounceFire = func() {
		defer h.clientWG.Done()
		h.debounceMu.Lock()
		h.debounceArmed = false
		closed := h.debounceClosed
		h.debounceMu.Unlock()
		if closed {
			return
		}
		h.doBroadcastSessionsUpdate()
	}
	// Hub-lifetime timer, created idle; BroadcastSessionsUpdate arms it with
	// Reset. AfterFunc(MaxInt64) cannot fire before Stop, so no drain needed.
	h.debounceTimer = time.AfterFunc(time.Duration(math.MaxInt64), h.debounceFire)
	h.debounceTimer.Stop()
	// A nil queue routes every WS send through sessionSendLegacy and loses
	// the dispatch queue's rate-limit / collect-window / passthrough modes;
	// Error level so a misconfigured production Hub is visible in journalctl.
	if opts.Queue == nil {
		slog.Error("server: Hub constructed without MessageQueue; falling back to legacy guard path (dispatch queue features disabled, R-LEGACY-SEND blocker)")
	} else {
		// Only a non-nil concrete queue is boxed, so send.go's `h.queue == nil`
		// gate keeps its meaning (a typed nil would read non-nil).
		h.queue = opts.Queue
	}
	return h
}

// SetUploadStore wires the upload store WS sends use to resolve pre-uploaded
// file_ids. A setter (not a HubOptions field) because the store's cleanup
// loop is bound to the app ctx and is created after the Hub exists.
func (h *Hub) SetUploadStore(s *uploadStore) { h.uploadStore = s }

// allowSendForOwner is the per-user (uploadOwner-keyed) send ceiling that
// stops N tabs multiplying the per-connection burst; the per-conn limiter
// still gates first. The budget mirrors the per-conn shape (1/s, burst 5)
// so a single tab sees no change. Owner "" and a nil map (hand-built hubs)
// always admit (#888).
func (h *Hub) allowSendForOwner(owner string) bool {
	if h == nil || owner == "" {
		return true
	}
	m := h.userSendLimiters.Load()
	if m == nil {
		return true
	}
	if v, ok := m.Load(owner); ok {
		return v.(*rate.Limiter).Allow()
	}
	// LoadOrStore returns the canonical limiter on a concurrent create.
	v, _ := m.LoadOrStore(owner, rate.NewLimiter(rate.Every(time.Second), 5))
	return v.(*rate.Limiter).Allow()
}

func (h *Hub) register(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	// Clients pre-authenticated by wsDeriveUploadOwner (no-token / cookie
	// path) join authClients here; token-mode clients join via
	// markAuthenticated. authMu nests inside h.mu.
	if h.authClients != nil && c.authenticated.Load() {
		h.authMu.Lock()
		h.addAuthClientLocked(c)
		h.authMu.Unlock()
	}
	h.mu.Unlock()
}

// addAuthClientLocked inserts c into authClients and its slice mirror.
// Idempotent (register's pre-auth insert may be followed by
// markAuthenticated). Caller MUST hold authMu (write).
func (h *Hub) addAuthClientLocked(c *wsClient) {
	if _, ok := h.authClients[c]; ok {
		return
	}
	h.authClients[c] = struct{}{}
	if h.authClientsIdx != nil {
		h.authClientsIdx[c] = len(h.authClientsSlice)
		h.authClientsSlice = append(h.authClientsSlice, c)
	}
}

// removeAuthClientLocked deletes c from authClients and swap-deletes it from
// the slice mirror in O(1). No-op if absent. Caller MUST hold authMu (write).
func (h *Hub) removeAuthClientLocked(c *wsClient) {
	if _, ok := h.authClients[c]; !ok {
		return
	}
	delete(h.authClients, c)
	if h.authClientsIdx == nil {
		return
	}
	i, ok := h.authClientsIdx[c]
	delete(h.authClientsIdx, c)
	if !ok {
		return
	}
	last := len(h.authClientsSlice) - 1
	if i != last {
		moved := h.authClientsSlice[last]
		h.authClientsSlice[i] = moved
		h.authClientsIdx[moved] = i
	}
	h.authClientsSlice[last] = nil // let the removed client be GC'd
	h.authClientsSlice = h.authClientsSlice[:last]
}

// markAuthenticated inserts c into the authClients mirror; the caller must
// have stored c.authenticated=true first. Nil authClients is a no-op.
func (h *Hub) markAuthenticated(c *wsClient) {
	h.mu.Lock()
	// Membership in h.clients is the source of truth: a delayed handleAuth
	// racing unregister must not reinsert a torn-down client.
	if h.authClients != nil {
		if _, ok := h.clients[c]; ok {
			h.authMu.Lock()
			h.addAuthClientLocked(c)
			h.authMu.Unlock()
		}
	}
	h.mu.Unlock()
}

func (h *Hub) unregister(c *wsClient) {
	// Per-key unsub closures take their own mutexes, so they are snapshotted
	// under h.mu (map mutation must be atomic with the counter decrement) and
	// invoked after release. Safe because no closure path acquires h.mu.
	h.mu.Lock()
	removed := false
	var unsubs []func()
	// Keys whose count hits zero drop their historyMarshalCache slot after
	// h.mu is released, mirroring handleUnsubscribe (#2010).
	var dropKeys []string
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		if h.authClients != nil {
			h.authMu.Lock()
			h.removeAuthClientLocked(c)
			h.authMu.Unlock()
		}
		if n := len(c.subscriptions); n > 0 {
			unsubs = make([]func(), 0, n)
			for key, unsub := range c.subscriptions {
				unsubs = append(unsubs, unsub)
				h.decSubscriberCountLocked(key)
				if !h.enforceCaps || h.subscriberCount[key] == 0 {
					dropKeys = append(dropKeys, key)
				}
			}
		}
		c.subscriptions = nil
		removed = true
	}
	h.mu.Unlock()
	for _, unsub := range unsubs {
		unsub()
	}
	if len(dropKeys) > 0 && h.historyMarshalCache != nil {
		for _, key := range dropKeys {
			h.historyMarshalCache.drop(key)
		}
	}
	if removed {
		// Guarded on `removed` so a double-unregister cannot drive the
		// counter negative.
		h.connCount.Add(-1)
		// Reads c.uploadOwnerKey() under connCountByOwnerMu so a concurrent
		// rekeyOwnerSlot cannot leak the new owner's slot (#1808).
		h.releaseOwnerSlotForClient(c)
		// Drop agent_subscribe refs so an abrupt disconnect cannot wedge a
		// tailer slot in broadcasting mode.
		if h.tailers != nil {
			h.tailers.detachClient(c)
		}
	}

	// appendConns skips alloc on an empty table (nodesPtr stays nil) so the
	// common single-node disconnect makes no pool round-trip; the pool borrow
	// runs inside alloc so count check + fill share one lock acquisition.
	var nodesPtr *[]node.Conn
	nodes := h.nodes.appendConns(func(n int) []node.Conn {
		nodesPtr = unregisterNodesPool.Get().(*[]node.Conn)
		buf := (*nodesPtr)[:0]
		if cap(buf) < n {
			buf = make([]node.Conn, 0, n)
		}
		return buf
	})
	if nodesPtr == nil {
		return
	}

	// Parallel RemoveClient fan-out (max(RTT) not sum(RTT)); blocking on the
	// WaitGroup keeps Shutdown's nodes.Close() strictly after every in-flight
	// call because readPump's defer runs unregister synchronously (#1356).
	if len(nodes) == 1 {
		nodes[0].RemoveClient(c)
	} else {
		var wg sync.WaitGroup
		wg.Add(len(nodes))
		for _, conn := range nodes {
			conn := conn
			go func() {
				defer wg.Done()
				conn.RemoveClient(c)
			}()
		}
		wg.Wait()
	}
	// Nil out references so returned node.Conn values stay GC-eligible.
	for i := range nodes {
		nodes[i] = nil
	}
	*nodesPtr = nodes[:0]
	unregisterNodesPool.Put(nodesPtr)
}

// unregisterNodesPool reuses Hub.unregister's []node.Conn snapshot; it holds
// pointers so Pool.Put does not allocate (go vet "Put argument allocates").
var unregisterNodesPool = sync.Pool{
	New: func() any {
		s := make([]node.Conn, 0, 4)
		return &s
	},
}

// maxWSConns caps simultaneous WebSocket upgrades; the broadcast pool is
// sized from it.
const maxWSConns = 500

// maxConnsPerOwner is the per-uploadOwner sub-cap: room for one power user's
// tabs + integrations while a single stolen token cannot monopolise
// maxWSConns (#1022).
const maxConnsPerOwner = 20

// reserveOwnerSlot increments the per-uploadOwner connection counter,
// returning false at maxConnsPerOwner. Every success must be paired with a
// release at teardown. Owner "" always succeeds without bumping the map.
func (h *Hub) reserveOwnerSlot(owner string) bool {
	if h == nil || owner == "" {
		return true
	}
	h.connCountByOwnerMu.Lock()
	defer h.connCountByOwnerMu.Unlock()
	return h.reserveOwnerSlotLocked(owner)
}

// reserveOwnerSlotLocked is the connCountByOwnerMu-held body of
// reserveOwnerSlot. owner=="" is handled by the callers (it never reaches
// here from reserveOwnerSlot, and rekeyOwnerSlot guards "" explicitly).
func (h *Hub) reserveOwnerSlotLocked(owner string) bool {
	if owner == "" || h.connCountByOwner == nil {
		return true
	}
	if h.connCountByOwner[owner] >= maxConnsPerOwner {
		return false
	}
	h.connCountByOwner[owner]++
	return true
}

// releaseOwnerSlot decrements the per-uploadOwner counter, deleting the entry
// at zero so the map stays bounded to active owners.
func (h *Hub) releaseOwnerSlot(owner string) {
	if h == nil || owner == "" {
		return
	}
	h.connCountByOwnerMu.Lock()
	defer h.connCountByOwnerMu.Unlock()
	h.releaseOwnerSlotLocked(owner)
}

// releaseOwnerSlotLocked is the connCountByOwnerMu-held body of
// releaseOwnerSlot.
func (h *Hub) releaseOwnerSlotLocked(owner string) {
	if owner == "" || h.connCountByOwner == nil {
		return
	}
	n := h.connCountByOwner[owner]
	if n <= 1 {
		delete(h.connCountByOwner, owner)
		return
	}
	h.connCountByOwner[owner] = n - 1
}

// rekeyOwnerSlot moves c's per-owner slot from oldOwner to newOwner and
// publishes c.setUploadOwner(newOwner) inside the SAME connCountByOwnerMu
// critical section, so a concurrent releaseOwnerSlotForClient sees either
// the old or the new owner with its slot held, never a half-applied state
// (#1808). Returns false (nothing changed) when newOwner is at the ceiling.
func (h *Hub) rekeyOwnerSlot(c *wsClient, oldOwner, newOwner string) bool {
	if h == nil {
		c.setUploadOwner(newOwner)
		return true
	}
	h.connCountByOwnerMu.Lock()
	defer h.connCountByOwnerMu.Unlock()
	// A torn-down connection (c.done closed) must not reserve a fresh slot:
	// unregister's one-shot release has already passed and would never free it.
	select {
	case <-c.done:
		return false
	default:
	}
	h.releaseOwnerSlotLocked(oldOwner)
	if !h.reserveOwnerSlotLocked(newOwner) {
		// Re-claim the old slot so the eventual release stays balanced.
		h.reserveOwnerSlotLocked(oldOwner)
		return false
	}
	c.setUploadOwner(newOwner)
	return true
}

// releaseOwnerSlotForClient releases the slot for c's CURRENT upload owner,
// reading c.uploadOwnerKey() under connCountByOwnerMu so it cannot
// interleave with rekeyOwnerSlot.
func (h *Hub) releaseOwnerSlotForClient(c *wsClient) {
	if h == nil {
		return
	}
	h.connCountByOwnerMu.Lock()
	defer h.connCountByOwnerMu.Unlock()
	h.releaseOwnerSlotLocked(c.uploadOwnerKey())
}

// TrackSend reserves a sendWG slot for a background send goroutine and
// returns a release function plus a shuttingDown flag. When shuttingDown is
// true the caller MUST NOT spawn the goroutine.
func (h *Hub) TrackSend() (release func(), shuttingDown bool) {
	h.sendTrackMu.Lock()
	defer h.sendTrackMu.Unlock()
	if h.sendClosed {
		return func() {}, true
	}
	h.sendWG.Add(1)
	return h.sendWG.Done, false
}

// Shutdown closes all WebSocket client connections and relays
// (LIFECYCLE-METHOD: writes every field block).
//
// LOCK ORDER CONTRACT: unsub closures invoked from here and unregister take
// eventLog.subMu; they are invoked after h.mu is released, and no EventLog
// callback (notifySubscribers, eventPushLoop) may acquire h.mu while holding
// subMu. Breaking this is an ABBA deadlock that surfaces as systemd
// TimeoutStopSec + SIGKILL (shutdown_lock_order_test.go).
func (h *Hub) Shutdown() {
	h.cancel() // cancel in-flight send goroutines

	// Flag first (inside the critical section, so the atomic mirror is
	// published before any later Stop/Reset) so no concurrent broadcast can
	// add a clientWG slot past the Wait below. Only an armed timer holds a
	// slot: Stop()==true means the callback never ran, so release it here.
	h.debounceMu.Lock()
	h.debounceClosed = true
	h.debounceClosedFast.Store(true)
	if h.debounceArmed && h.debounceTimer != nil {
		if h.debounceTimer.Stop() {
			h.clientWG.Done()
		}
		h.debounceArmed = false
	}
	h.debounceMu.Unlock()

	// Close client conns first, then wait for pumps/eventPushLoop, so
	// node/router teardown cannot race unregister → RemoveClient. Unsub
	// closures are snapshotted under h.mu and invoked after release (same
	// lock-split as unregister).
	h.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(h.clients))
	var unsubs []func()
	removed := 0
	for c := range h.clients {
		if n := len(c.subscriptions); n > 0 {
			if unsubs == nil {
				unsubs = make([]func(), 0, n)
			}
			for _, unsub := range c.subscriptions {
				unsubs = append(unsubs, unsub)
			}
		}
		c.subscriptions = nil
		if c.conn != nil {
			conns = append(conns, c.conn)
		}
		delete(h.clients, c)
		removed++
	}
	// All subscriptions were just niled, so the per-key counts are zero.
	for k := range h.subscriberCount {
		delete(h.subscriberCount, k)
		h.subscriberCountFast.Delete(k)
	}
	// Drain authClients (kept empty, not nil, so a straggler
	// markAuthenticated still goes through the h.clients check) and its
	// slice mirror so post-Shutdown broadcasts cannot reach torn-down clients.
	h.authMu.Lock()
	for c := range h.authClients {
		delete(h.authClients, c)
	}
	for i := range h.authClientsSlice {
		h.authClientsSlice[i] = nil
	}
	h.authClientsSlice = h.authClientsSlice[:0]
	for c := range h.authClientsIdx {
		delete(h.authClientsIdx, c)
	}
	h.authMu.Unlock()
	h.mu.Unlock()
	for _, unsub := range unsubs {
		unsub()
	}
	// unregister's decrement is gated on h.clients membership (just
	// cleared), so release the slots here.
	if removed > 0 {
		h.connCount.Add(int64(-removed))
	}

	for _, conn := range conns {
		conn.Close()
	}

	// After closing conns so in-flight pollOnce iterations finish against a
	// closed client (SendRaw drops gracefully).
	if h.tailers != nil {
		h.tailers.Shutdown()
	}

	// clientWG.Wait MUST precede nil-ing wiredLinkers: an in-flight
	// completeSubscribe → maybeWireLinkerTailer would otherwise take the
	// "shutting down" branch and silently drop a wiring. After Wait,
	// wiredLinkers == nil means exactly "no client goroutine remains" (#371).
	h.clientWG.Wait()

	// Release wiredLinkers so linker objects can be GC'd.
	h.wiredLinkersMu.Lock()
	h.wiredLinkers = nil
	h.wiredLinkersMu.Unlock()

	// Safe after clientWG.Wait — no eventPushLoop calls getOrMarshal again.
	if h.historyMarshalCache != nil {
		h.historyMarshalCache.reset()
	}

	// Atomic nil-store: in-flight allowSendForOwner sees live map or nil.
	h.userSendLimiters.Store(nil)

	// Post clientWG.Wait no reserve/release caller remains.
	h.connCountByOwnerMu.Lock()
	h.connCountByOwner = nil
	h.connCountByOwnerMu.Unlock()

	// Barrier: a racing TrackSend completes on one side of this line; after
	// sendClosed no caller Adds, so sendWG.Wait cannot be escaped.
	h.sendTrackMu.Lock()
	h.sendClosed = true
	h.sendTrackMu.Unlock()

	// After pumps are gone: readPump may call handleRemoteSend (sendWG.Add)
	// on its way out.
	h.sendWG.Wait()

	// Nodes close last so unregister → RemoveClient and in-flight RPCs
	// cannot race a closed node.
	for _, conn := range h.nodes.Conns() {
		conn.Close()
	}
}
