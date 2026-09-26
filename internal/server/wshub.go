package server

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/agentlink"
)

// Hub manages WebSocket client connections and event subscriptions.
//
// Field-block contract: the send pipeline (engine), connection admission
// (admit) and sessions_update debouncing (debounce) are sub-objects that own
// their locks and fields. The rest are grouped into blocks — lifecycle
// (mu / ctx / cancel), subscriber, broadcast, shared deps, agent tailer,
// cache. Methods live in wshub_<block>.go and write only their own block,
// declared by a `WRITES:` / `READS-ALSO:` godoc marker
// (tools/lint-server-handlers rule 3a); NewHub / Shutdown are the
// LIFECYCLE-METHOD cross-block exemption.
type Hub struct {
	mu sync.RWMutex
	// droppedTotal is the Hub-wide count of SendRaw drops (send channel
	// full). Deliberately aggregated, not per-client: it is exported only
	// via the auth-gated /health, and all authenticated users share one trust
	// boundary. Multi-tenant auth would require moving it behind debug_mode (#1100).
	droppedTotal atomic.Int64
	clients      map[*wsClient]struct{}
	// authClients mirrors clients whose authenticated flag is true so
	// broadcastToAuthenticated skips the handshake-pending majority.
	// Guarded by authMu, nested INSIDE h.mu by the writers (register /
	// markAuthenticated / unregister / Shutdown); the broadcast read side
	// takes authMu alone and never h.mu, so there is no inverse order.
	// Nil on hand-rolled test hubs ⇒ legacy h.clients scan (#1409, #1621).
	// The pad keeps authMu off h.mu's cache line (128 bytes: Apple silicon
	// lines, x86 adjacent-line prefetch): broadcasts read-lock authMu while
	// churn write-locks h.mu (BenchmarkHubSnapshotAuthenticated/churn +17%).
	_           [128]byte
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
	// router is the HubRouter consumer subset (consumer.go) so tests can
	// inject a fake.
	router    HubRouter
	agents    map[string]session.AgentOpts
	agentCmds map[string]string
	// engine owns the send pipeline: queue / guard / send-goroutine accounting
	// used to live here as six Hub fields (#2551). Hub now only forwards to
	// it (WS handlers) and drains it (Shutdown); SendHandler holds the same
	// instance. Named engine, not send, because wsClient already has a `send`
	// channel and h.send / c.send would read alike. nil on hand-rolled test
	// hubs that skip NewHub.
	engine *sendEngine
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

	// clientWG tracks per-client pump/eventPushLoop goroutines plus the
	// debounce callback; owned by the connection lifecycle (conn.Close),
	// whereas the send goroutines are owned by sendEngine (ctx cancel + drain).
	clientWG sync.WaitGroup

	// admit owns the connection caps, rate limits and credential checks.
	admit *connAdmission

	// debounce coalesces sessions_update broadcasts; each pending fire holds
	// a clientWG slot, so Shutdown's Wait covers a late-running broadcast.
	debounce *debouncer

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
	// UploadStore resolves WS-sent file_ids. Wired here rather than through a
	// SetUploadStore call after Start (#2552): the store is built one line
	// earlier in buildServer, so there is no window where a Hub is serving
	// upgrades with an unwired store.
	UploadStore *uploadStore
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
	nodes := opts.Nodes
	if nodes == nil {
		nodes = newNodeRegistry(nil)
	}
	h := &Hub{
		clients:         make(map[*wsClient]struct{}),
		authClients:     make(map[*wsClient]struct{}),
		authClientsIdx:  make(map[*wsClient]int),
		subscriberCount: make(map[string]int),
		router:          opts.Router,
		agents:          opts.Agents,
		agentCmds:       opts.AgentCmds,
		nodes:           nodes,
		projectMgr:      opts.ProjectMgr,
		resolver:        opts.Resolver,
		scheduler:       opts.Scheduler,
		scratchPool:     opts.ScratchPool,
		allowedRoot:     opts.AllowedRoot,
		admit:           newConnAdmission(opts),
		uploadStore:     opts.UploadStore,
		ctx:             ctx,
		cancel:          cancel,
	}
	h.tailers = newTailerRegistry(h)
	h.wiredLinkers = make(map[agentlink.AgentLinker]struct{})
	h.historyMarshalCache = newHistoryMarshalCache()
	h.debounce = newDebouncer(&h.clientWG, h.doBroadcastSessionsUpdate)
	// Built last: h is now usable as the engine's sendNotifier. The engine
	// keeps its own reference to each shared dependency (see sendEngine's
	// INVARIANT note) rather than a *Hub back-pointer.
	h.engine = newSendEngine(sendEngineOpts{
		Queue:       opts.Queue,
		Guard:       opts.Guard,
		Ctx:         ctx,
		Router:      opts.Router,
		Resolver:    opts.Resolver,
		Agents:      opts.Agents,
		ProjectMgr:  opts.ProjectMgr,
		ScratchPool: opts.ScratchPool,
		Scheduler:   opts.Scheduler,
		AllowedRoot: opts.AllowedRoot,
		Notify:      h,
	})
	return h
}

func (h *Hub) register(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	// Clients pre-authenticated by deriveUploadOwner (no-token / cookie
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
				if h.dropMarshalCacheForLocked(key) {
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
		h.admit.releaseConn(1)
		// Reads c's owner under the admission lock, so a concurrent re-key
		// cannot leak the new owner's slot.
		h.admit.releaseOwnerFor(c)
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

	// Before the clients go: no broadcast may take a clientWG slot past the
	// Wait below, and a window that never fired gives its slot back here.
	h.debounce.close()

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
		h.admit.releaseConn(removed)
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

	// After clientWG.Wait: no owner-slot or send-budget caller remains.
	h.admit.close()

	// Send barrier. Position is load-bearing and drain's godoc spells out why:
	// h.cancel() and h.debounce.close() above, none of h.mu / authMu / the
	// debouncer's lock held (the drained goroutines re-enter all three
	// through sendNotifier), and before the node Close loop below.
	// wshub_shutdown_order_test.go pins the source order.
	if h.engine != nil {
		h.engine.drain()
	}

	// Nodes close last so unregister → RemoveClient and in-flight RPCs
	// cannot race a closed node.
	for _, conn := range h.nodes.Conns() {
		conn.Close()
	}
}
