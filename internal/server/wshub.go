package server

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// Hub manages WebSocket client connections and event subscriptions. Its
// state lives in sub-objects that own their locks and fields: subs (who is
// connected, authenticated and subscribed), admit (connection caps, rate
// limits, credentials), engine (the send pipeline), debounce
// (sessions_update coalescing) and tailers (agent JSONL tailing). The Hub
// wires them together and orders their teardown in Shutdown.
type Hub struct {
	// subs is who is connected, authenticated and subscribed to what.
	subs *subscriberRegistry
	// droppedTotal is the Hub-wide count of SendRaw drops (send channel
	// full). Deliberately aggregated, not per-client: it is exported only
	// via the auth-gated /health, and all authenticated users share one trust
	// boundary. Multi-tenant auth would require moving it behind debug_mode (#1100).
	droppedTotal atomic.Int64
	// router is the HubRouter consumer subset (consumer.go) so tests can
	// inject a fake.
	router HubRouter
	// engine owns the send pipeline; SendHandler holds the same instance.
	// Named engine, not send, because wsClient already has a `send` channel
	// and h.send / c.send would read alike.
	engine *sendEngine
	// nodes is the same registry instance as Server.nodes (pinned by
	// hub_shared_state_test.go); the registry owns its mutex.
	nodes *nodeRegistry
	// resolver centralises session key → opts derivation; nil keeps the
	// inline fallback for tests.
	resolver *session.KeyResolver
	// scheduler is the narrow CronView hook: stub revival on re-subscribe
	// and first-prompt auto-save. Nil keeps both dormant.
	scheduler   CronView
	uploadStore *uploadStore    // optional, for resolving WS-sent file_ids
	ctx         context.Context // cancelled on Shutdown to stop in-flight sends
	cancel      context.CancelFunc

	// resubscribeInterval is how often a push loop whose process went away
	// looks for a new one (defaultResubscribeInterval; tests shorten it).
	resubscribeInterval time.Duration

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

// NewHub creates a new WebSocket hub. h.ctx derives from opts.ParentCtx (Background when nil) so a parent
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
		subs:        newSubscriberRegistry(),
		router:      opts.Router,
		nodes:       nodes,
		resolver:    opts.Resolver,
		scheduler:   opts.Scheduler,
		admit:       newConnAdmission(opts),
		uploadStore: opts.UploadStore,
		ctx:         ctx,
		cancel:      cancel,

		resubscribeInterval: defaultResubscribeInterval,
	}
	h.tailers = newTailerRegistry(h, opts.AllowedRoot)
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
	h.subs.add(c)
}

func (h *Hub) unregister(c *wsClient) {
	// The closures take EventLog locks; the registry hands them back so they
	// run with its lock released.
	unsubs, dropKeys, removed := h.subs.remove(c)
	for _, unsub := range unsubs {
		unsub()
	}
	// Keys left with no subscriber drop their historyMarshalCache slot, as
	// handleUnsubscribe does.
	if len(dropKeys) > 0 {
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
		h.tailers.detachClient(c)
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

// Shutdown closes all WebSocket client connections and relays.
//
// LOCK ORDER CONTRACT: unsub closures invoked from here and unregister take
// eventLog.subMu; they are invoked after the registry's lock is released, and
// no EventLog callback (notifySubscribers, eventPushLoop) may take that lock
// while holding subMu. Breaking this is an ABBA deadlock that surfaces as systemd
// TimeoutStopSec + SIGKILL (shutdown_lock_order_test.go).
func (h *Hub) Shutdown() {
	h.cancel() // cancel in-flight send goroutines

	// Before the clients go: no broadcast may take a clientWG slot past the
	// Wait below, and a window that never fired gives its slot back here.
	h.debounce.close()

	// Close client conns first, then wait for pumps/eventPushLoop, so
	// node/router teardown cannot race unregister → RemoveClient. drain hands
	// the unsub closures back so they run with the registry's lock released.
	clients, unsubs := h.subs.drain()
	for _, unsub := range unsubs {
		unsub()
	}
	// unregister releases a slot only for a client still registered, and
	// drain just emptied the registry, so release the slots here.
	if len(clients) > 0 {
		h.admit.releaseConn(len(clients))
	}
	conns := make([]*websocket.Conn, 0, len(clients))
	for _, c := range clients {
		if c.conn != nil {
			conns = append(conns, c.conn)
		}
	}

	for _, conn := range conns {
		conn.Close()
	}

	// After closing conns so in-flight pollOnce iterations finish against a
	// closed client (SendRaw drops gracefully).
	h.tailers.Shutdown()

	// clientWG.Wait MUST precede releasing the wired linkers: an in-flight
	// completeSubscribe → maybeWireLinkerTailer would otherwise find the set
	// gone and silently drop a wiring. After Wait no client goroutine remains.
	h.clientWG.Wait()
	h.tailers.releaseLinkers()

	// Safe after clientWG.Wait — no eventPushLoop calls getOrMarshal again.
	h.historyMarshalCache.reset()

	// After clientWG.Wait: no owner-slot or send-budget caller remains.
	h.admit.close()

	// Send barrier. Position is load-bearing and drain's godoc spells out why:
	// h.cancel() and h.debounce.close() above, none of the registry's locks
	// or the debouncer's held (the drained goroutines re-enter them
	// through sendNotifier), and before the node Close loop below.
	// wshub_shutdown_order_test.go pins the source order.
	h.engine.drain()

	// Nodes close last so unregister → RemoveClient and in-flight RPCs
	// cannot race a closed node.
	for _, conn := range h.nodes.Conns() {
		conn.Close()
	}
}
