package server

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/agentlink"
)

// Pre-encoded WS frames for messages whose body never varies. SendJSON would
// otherwise reflect-marshal `node.ServerMsg{Type: "auth_ok"}` (and similar)
// on every authentication or ping reply. Since these are emitted on the auth
// hot path (one per connection) and the pong path (every keepalive), the
// pre-encode collapses them to a single shared []byte. R218-PERF-15.
//
// R229-PERF-4: extended to cover the most common error responses (auth gate
// failures + per-client rate-limit notices) emitted from readPump's switch.
// These were marshalled via SendJSON on every readPump iteration that fell
// through to the not-authenticated / rate-limited branch — a steady-state
// cost on connections that get throttled. Frame strings must stay byte-for-
// byte identical to `node.ServerMsg{Type: "error", Error: "..."}` JSON
// output (no field reordering: ServerMsg defines Type before Error).
//
// R241-SEC-13 [SIMPLE]: store as immutable string constants and convert to
// []byte at each send site so a future code path that calls `append` on the
// shared buffer cannot race with concurrent send goroutines reading the same
// underlying array. The per-send `[]byte(...)` conversion is a simple memcpy
// of a 20-50 byte payload — negligible against the WebSocket write itself,
// and never returns the package-level frame to a caller that could mutate it.
const (
	wsAuthOkMsg          = `{"type":"auth_ok"}`
	wsPongMsg            = `{"type":"pong"}`
	wsAuthFailInvalidMsg = `{"type":"auth_fail","error":"invalid token"}`
	wsErrNotAuthMsg      = `{"type":"error","error":"not authenticated"}`
	wsErrRateLimitedMsg  = `{"type":"error","error":"rate limited"}`
)

// Hub manages WebSocket client connections and event subscriptions.
//
// Field-block contract (server-split-phase4-design.md §五 / baseline §3):
// 37 fields organized in 6 blocks. Phase 4 抽包 to internal/wshub/ keeps
// the struct intact; Phase 5 considers Hub.allowedRoot merge with
// Server.allowedRoot via NewHub Options. Field → file access map below;
// new fields MUST add a "// 读写:" inline comment AND update this map.
//
//	Block             Field                         读写文件
//	─────────────────────────────────────────────────────────────────────
//	lifecycle (3)     mu                            wshub.go, wsclient.go,
//	                                                wshub_broadcast.go,
//	                                                wshub_eventpush.go,
//	                                                wshub_subscribe.go
//	                  ctx                           wshub.go, wshub_eventpush.go,
//	                                                wshub_send.go, wshub_subscribe.go
//	                  cancel                        wshub.go (Shutdown only)
//	subscriber (8)    clients                       wshub.go, wshub_broadcast.go,
//	                                                wshub_subscribe.go, wshub_upgrade.go
//	                  connCount                     wshub.go, wshub_upgrade.go
//	                  clientWG                      wshub.go, wshub_broadcast.go,
//	                                                wshub_subscribe.go, wshub_upgrade.go
//	                  wsAuthLimiter                 wshub_upgrade.go
//	                  wsUpgradeLimiter              wshub_upgrade.go
//	                  upgrader                      wshub.go, wshub_upgrade.go
//	                  dashTokenHash                 wshub.go, wshub_upgrade.go
//	                  cookieMAC                     wshub_upgrade.go
//	broadcast (4)     debounceMu                    wshub.go, wshub_broadcast.go
//	                  debounceTimer                 wshub.go, wshub_broadcast.go
//	                  debounceFirst                 wshub_broadcast.go
//	                  debounceClosed                wshub.go, wshub_broadcast.go
//	send (5)          queue                         wshub.go (ctor); via wsclient
//	                                                READ-ALSO from wshub_subscribe.go
//	                                                (close client → drain pending)
//	                  sendWG                        wshub.go (TrackSend / Shutdown)
//	                  sendTrackMu                   wshub.go (TrackSend / Shutdown)
//	                  sendClosed                    wshub.go (TrackSend / Shutdown)
//	                  droppedTotal                  wshub_broadcast.go
//	shared deps (14)  router                        consumer.go, wshub_agent.go,
//	                                                wshub_eventpush.go, wshub_send.go,
//	                                                wshub_subscribe.go
//	                  agents                        wshub.go (ctor); via wsclient
//	                  agentCmds                     wshub.go (ctor); via wsclient
//	                  dashToken                     wshub_upgrade.go
//	                  guard                         wshub.go (ctor); via wsclient
//	                  nodes                         wshub.go, wshub_send.go,
//	                                                wshub_subscribe.go
//	                  nodesMu                       wshub.go, wshub_send.go,
//	                                                wshub_subscribe.go
//	                  projectMgr                    wshub.go (ctor); via wsclient
//	                  resolver                      wshub.go (ctor); via wsclient
//	                  scheduler                     wshub.go, wshub_subscribe.go
//	                  uploadStore                   wshub.go, wshub_send.go
//	                  scratchPool                   wshub.go (ctor); via wsclient
//	                  allowedRoot                   wshub.go (ctor); via wsclient.
//	                                                Phase 5 merges with
//	                                                Server.allowedRoot via NewHub
//	                                                Options.
//	                  trustedProxy                  wshub.go, wshub_upgrade.go
//	agent tailer (3)  tailers                       wshub.go, wshub_agent.go
//	                  wiredLinkersMu                wshub.go, wshub_agent.go
//	                  wiredLinkers                  wshub.go, wshub_agent.go
//
// Phase 4 抽到 internal/wshub/ 后，方法严格按文件分块（hub_broadcast.go
// 只 WRITE broadcast block + READ shared deps；其余同理）。CI lint
// rule 3 (field_block) 验证此约束。
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
	// router is the HubRouter subset (consumer.go). *session.Router
	// satisfies this interface implicitly; kept as an interface so
	// tests can inject a fake and a future Router sub-aggregation
	// can swap implementations without touching Hub internals.
	router    HubRouter
	agents    map[string]session.AgentOpts
	agentCmds map[string]string
	dashToken string
	// dashTokenHash is sha256(dashToken), precomputed at NewHub for
	// constant-time auth comparison. Immutable after construction:
	// hot-reloading dashToken at runtime is not supported and would
	// silently leave this hash stale — restart naozhi to rotate.
	dashTokenHash [32]byte
	cookieMAC     string // HMAC-derived cookie value (different from dashToken)
	guard         *session.Guard
	// NEEDS-DESIGN R242-GO-10: 与其他 Hub 依赖一致改抽 MessageEnqueuer interface；
	// 当前直接耦合 *dispatch.MessageQueue 具体类型。
	queue      *dispatch.MessageQueue // per-key FIFO queue for dashboard sends
	nodes      map[string]node.Conn
	nodesMu    *sync.RWMutex // shared with Server.nodesMu — all nodes map access must use this
	projectMgr *project.Manager
	// resolver centralises session key → opts derivation; used by
	// sessionOptsFor / buildSessionOpts. Nil keeps legacy fallback
	// wiring for tests that don't construct a resolver.
	resolver *session.KeyResolver
	// scheduler is the optional cron-side hook the Hub needs for two
	// things: (a) reviving a dismissed cron stub when a dashboard tab
	// re-subscribes (handleSubscribe → EnsureStub) and (b) auto-saving
	// the user's first prompt as the cron job's permanent prompt
	// (sessionSend → SetJobPrompt). R232-ARCH-7: typed as the narrow
	// cronHubOps interface (defined in this file) instead of
	// *cron.Scheduler, so server's coupling to cron is the 2 methods
	// it actually uses, not the full 60+ method scheduler surface.
	// *cron.Scheduler satisfies cronHubOps implicitly.
	scheduler   cronHubOps
	uploadStore *uploadStore // optional, for resolving WS-sent file_ids
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
	//
	// CONTRACT (R218B-ARCH-1): every code path that registers a goroutine on
	// sendWG MUST go through TrackSend(). A direct h.sendWG.Add(1) bypasses
	// the sendClosed gate and can race Shutdown.Wait into returning while the
	// goroutine is still alive — at which point it may dereference router /
	// nodes / clients maps that Shutdown has already torn down. If you add a
	// new background-send entry point, route it through TrackSend and respect
	// the shuttingDown bool to abort cleanly.
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
	//
	// R191-SEC-M2 split: wsAuthLimiter gates the *inner* `auth` WS message
	// (direct credential test, reuses loginLimiter). wsUpgradeLimiter gates
	// the upgrade handshake itself, which can fire legitimately on tab-reload
	// / mobile-wake without any credential test — a looser budget prevents
	// the two paths from DoS'ing each other via a shared bucket.
	wsAuthLimiter    func(ip string) bool
	wsUpgradeLimiter func(ip string) bool

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

	// tailers owns the agentTailer registry backing agent_subscribe / agent_
	// unsubscribe WS flows (RFC v4 agent-team-ui §3.5.4). Initialised by
	// NewHub so the nil-guard at each call site can simplify to a presence
	// check. Shutdown tears it down alongside other background loops.
	tailers *tailerRegistry

	// wiredLinkersMu + wiredLinkers dedup OnResolve / task_done callback
	// registration so re-subscribes on reconnect don't pile up duplicate
	// callbacks. Per-Hub (not package-level) so multi-Hub tests don't share
	// state; Shutdown nils the map so AgentLinker references can be GC'd —
	// the package-level leak this replaces was R201-CRIT-2.
	//
	// Interface map keys dedup on the (dynamic type, dynamic value) tuple,
	// equivalent to the prior *cli.SubagentLinker pointer-keyed map under
	// the current 1:1 (cli.Process → *SubagentLinker) producer. R239-ARCH-I.
	//
	// R248-GO-5 (issue #372): when a second AgentLinker implementation lands
	// (ACP / Gemini / etc.), the dedup contract becomes *type-aware*. Two
	// linkers that share an underlying pointer value but differ in dynamic
	// type are stored as separate keys (correct: they are observably
	// different objects, register their own callbacks, and must dedup
	// independently). The subtler case is a future helper that wraps an
	// AgentLinker in a thin adapter without changing identity — the
	// adapter's dynamic type would create a duplicate key for the same
	// underlying linker, double-firing OnResolve. New backend authors
	// MUST verify their producer satisfies the 1:1 invariant
	// (one canonical AgentLinker per cli.Process / equivalent owner) and
	// either dedup at the producer or pass the canonical reference here.
	// If multi-backend wrapping becomes routine, switch the key to a
	// stable identity field (e.g. linker.ProjectSessionDir() once it is
	// stable per linker lifetime) and document the new invariant here.
	wiredLinkersMu sync.Mutex
	wiredLinkers   map[agentlink.AgentLinker]struct{}
}

// HubOptions holds configuration for a Hub.
type HubOptions struct {
	Router     *session.Router
	Agents     map[string]session.AgentOpts
	AgentCmds  map[string]string
	DashToken  string
	CookieMAC  string
	Guard      *session.Guard
	Queue      *dispatch.MessageQueue
	Nodes      map[string]node.Conn
	NodesMu    *sync.RWMutex
	ProjectMgr *project.Manager
	// Resolver, when non-nil, centralises session-key → opts derivation
	// for sessionOptsFor / buildSessionOpts. Wired by server.Start so
	// WS subscribe / send paths share the same planner-binding
	// precedence as the IM dispatch path. Nil falls back to the legacy
	// inlined merge.
	Resolver         *session.KeyResolver
	AllowedRoot      string
	TrustedProxy     bool
	WSAuthLimiter    func(ip string) bool
	WSUpgradeLimiter func(ip string) bool
	// ParentCtx is the application-level context whose cancellation must
	// propagate to the Hub. When set, NewHub derives h.ctx via
	// context.WithCancel(ParentCtx) so that parent-ctx cancel tears down
	// send/push goroutines even if Shutdown() is not explicitly called
	// (e.g. a future panic-early-exit path in main that forgets Shutdown).
	// Nil falls back to context.Background() to preserve legacy behaviour
	// for tests and headless wiring. CTX1 (Round 167).
	ParentCtx context.Context
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
		clients:          make(map[*wsClient]struct{}),
		router:           opts.Router,
		agents:           opts.Agents,
		agentCmds:        opts.AgentCmds,
		dashToken:        opts.DashToken,
		cookieMAC:        opts.CookieMAC,
		guard:            opts.Guard,
		queue:            opts.Queue,
		nodes:            opts.Nodes,
		nodesMu:          opts.NodesMu,
		projectMgr:       opts.ProjectMgr,
		resolver:         opts.Resolver,
		allowedRoot:      opts.AllowedRoot,
		trustedProxy:     opts.TrustedProxy,
		wsAuthLimiter:    opts.WSAuthLimiter,
		wsUpgradeLimiter: opts.WSUpgradeLimiter,
		ctx:              ctx,
		cancel:           cancel,
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
	if opts.DashToken != "" {
		// Precompute sha256 so handleAuth only hashes the inbound token,
		// halving the per-WS-auth hash work. R230-PERF-11.
		h.dashTokenHash = sha256.Sum256([]byte(opts.DashToken))
	}
	h.tailers = newTailerRegistry(h)
	h.wiredLinkers = make(map[agentlink.AgentLinker]struct{})
	// R219-CR-11 / R229-CR-4: a nil queue makes every WS send fall through to
	// sessionSendLegacy (the deprecated guard path). Test harnesses and
	// headless tools deliberately wire a nil Queue, but a production Hub
	// constructed without one silently loses the dispatch queue's
	// rate-limiting / collect-window / passthrough modes — emit slog.Error
	// (not Warn) at construction so an operator triaging journalctl sees
	// the misconfiguration with the same severity as the resulting
	// per-message degradation. R229-CR-4 raises this from Warn to Error to
	// advance the R-LEGACY-SEND removal: once every test fixture wires a
	// real MessageQueue, this branch can become a hard fatal at construction
	// and sessionSendLegacy can be deleted.
	if opts.Queue == nil {
		slog.Error("server: Hub constructed without MessageQueue; falling back to legacy guard path (dispatch queue features disabled, R-LEGACY-SEND blocker)")
	}
	return h
}

// cronHubOps is the narrow consumer interface the Hub needs from
// *cron.Scheduler. Defined here (and not in cron) so server's coupling
// to the scheduler stays at the two methods we actually call, and tests
// can inject a fake without depending on the full Scheduler. R232-ARCH-7
// extension of the cronStubChecker pattern from R228-ARCH-17.
type cronHubOps interface {
	EnsureStub(key string) bool
	SetJobPrompt(jobID, prompt string) error
}

// SetScheduler sets the cron scheduler for auto-saving prompts on first send.
// Accepts the concrete *cron.Scheduler (production wiring) — the field type
// is the narrower cronHubOps interface so the Hub never sees the rest of the
// scheduler API.
func (h *Hub) SetScheduler(s *cron.Scheduler) { h.scheduler = s }

// SetUploadStore wires the upload store used by WS sends to resolve file_ids
// that were pre-uploaded via POST /api/sessions/upload.
func (h *Hub) SetUploadStore(s *uploadStore) { h.uploadStore = s }

// SetScratchPool wires the ephemeral-session pool so sessionOptsFor can
// resolve AgentOpts for scratch keys without touching the sidebar-visible
// router state.
func (h *Hub) SetScratchPool(p *session.ScratchPool) { h.scratchPool = p }
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
		// Drop any agent_subscribe refs this client was holding so refCount
		// stays accurate — otherwise an abrupt disconnect (mobile sleep)
		// would leave the tailer in broadcasting mode forever, wedging a
		// slot in the 50-tailer cap.
		if h.tailers != nil {
			h.tailers.detachClient(c)
		}
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

// maxWSConns caps simultaneous WebSocket upgrades. Exposed here so the
// per-tick broadcast pool (below) stays sized to the real deployment
// envelope instead of a hand-picked 256 that silently disables pooling
// whenever connCount grows past it.
const maxWSConns = 500

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

	// Stop all agent tailers so their ticker goroutines do not race the
	// client closure below (a tailer SendJSON to a half-closed wsClient
	// would be logged as a drop and bump droppedTotal, but is otherwise
	// harmless). Done AFTER closing the conns so in-flight pollOnce calls
	// finish their single iteration against a subscriber set that includes
	// the closed client; SendRaw's select-default drops gracefully.
	if h.tailers != nil {
		h.tailers.Shutdown()
	}

	// Now that conns are closed, pumps will observe the read/write error
	// and exit their loops; eventPushLoop sees h.ctx.Done() or c.done.
	// Wait bounds the shutdown on explicit goroutine lifecycle rather than
	// on the parent context timeout alone.
	//
	// Issue #371: clientWG.Wait MUST come before nil-ing wiredLinkers. An
	// in-flight readPump can be partway through handleSubscribe →
	// completeSubscribe → maybeWireLinkerTailer; if we nil the map first,
	// that goroutine takes the "Hub shutting down — skip" branch and
	// silently drops a wiring it would otherwise have completed. Swapping
	// the order makes "wiredLinkers == nil" mean exactly "no client
	// goroutines remain, no further wiring is possible" — which is what
	// the nil-guard at wshub_agent.go:81 was always meant to express.
	h.clientWG.Wait()

	// Release wiredLinkers so the underlying linker objects can be GC'd —
	// previously this was a process-lifetime package-level leak. Safe to
	// drop the lock here because clientWG.Wait above has already drained
	// every goroutine that might call maybeWireLinkerTailer; the mutex
	// only guards against concurrent map writes from those goroutines.
	h.wiredLinkersMu.Lock()
	h.wiredLinkers = nil
	h.wiredLinkersMu.Unlock()

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
