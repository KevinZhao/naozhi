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
	"golang.org/x/time/rate"

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
// Field-block contract (server-split-phase4-design.md v0.6.1 §五 / baseline §3):
// **47 fields organized in 7 blocks** (v0.6.1 校准；v0.4 baseline 写 37 漏数 10 个).
// Phase 4 抽包 to internal/wshub/ keeps the struct intact; Phase 5 considers
// Hub.allowedRoot merge with Server.allowedRoot via NewHub Options.
// Field → file access map below; new fields MUST update this map AND
// docs/design/server-split-phase4-design.md §五 simultaneously (v0.6.1 §0
// 修订纪律 1: PR description must declare any §0 fact-table change).
//
//	Block                Field                         读写文件
//	─────────────────────────────────────────────────────────────────────
//	lifecycle (3)        mu                            wshub.go, wsclient.go,
//	                                                   wshub_broadcast.go,
//	                                                   wshub_eventpush.go,
//	                                                   wshub_subscribe.go
//	                     ctx                           wshub.go, wshub_eventpush.go,
//	                                                   wshub_send.go, wshub_subscribe.go
//	                     cancel                        wshub.go (Shutdown only)
//	subscriber (10)      clients                       wshub.go, wshub_broadcast.go,
//	                                                   wshub_subscribe.go, wshub_upgrade.go
//	                     connCount                     wshub.go, wshub_upgrade.go
//	                     subscriberCount               wshub.go, wshub_subscribe.go
//	                                                   (R246-PERF-4 #716 cap counter)
//	                     clientWG                      wshub.go, wshub_broadcast.go,
//	                                                   wshub_subscribe.go, wshub_upgrade.go
//	                     wsAuthLimiter                 wshub_upgrade.go
//	                     wsUpgradeLimiter              wshub_upgrade.go
//	                     upgrader                      wshub.go, wshub_upgrade.go
//	                     dashTokenHash                 wshub.go, wshub_upgrade.go
//	                     cookieMAC                     wshub_upgrade.go
//	                     trustedProxy                  wshub.go, wshub_upgrade.go
//	broadcast (6)        debounceMu                    wshub.go, wshub_broadcast.go
//	                     debounceTimer                 wshub.go, wshub_broadcast.go
//	                     debounceFirst                 wshub_broadcast.go
//	                     debounceClosed                wshub.go, wshub_broadcast.go
//	                     debounceClosedFast            wshub_broadcast.go
//	                     debounceFire                  wshub.go (ctor),
//	                                                   wshub_broadcast.go
//	send (6)             queue                         wshub.go (ctor); via wsclient
//	                                                   READ-ALSO from wshub_subscribe.go
//	                                                   (close client → drain pending)
//	                     sendWG                        wshub.go (TrackSend / Shutdown)
//	                     sendTrackMu                   wshub.go (TrackSend / Shutdown)
//	                     sendClosed                    wshub.go (TrackSend / Shutdown)
//	                     droppedTotal                  wshub_broadcast.go
//	                     legacySendInvokes             wshub_send.go (R-LEGACY-SEND #710)
//	shared deps (14)     router                        consumer.go, wshub_agent.go,
//	                                                   wshub_eventpush.go, wshub_send.go,
//	                                                   wshub_subscribe.go
//	                     agents                        wshub.go (ctor); via wsclient
//	                     agentCmds                     wshub.go (ctor); via wsclient
//	                     dashToken                     wshub_upgrade.go
//	                     guard                         wshub.go (ctor); via wsclient
//	                     nodes                         wshub.go, wshub_send.go,
//	                                                   wshub_subscribe.go
//	                     nodesMu                       wshub.go, wshub_send.go,
//	                                                   wshub_subscribe.go
//	                     projectMgr                    wshub.go (ctor); via wsclient
//	                     resolver                      wshub.go (ctor); via wsclient
//	                     scheduler                     wshub.go, wshub_subscribe.go
//	                     uploadStore                   wshub.go, wshub_send.go
//	                     scratchPool                   wshub.go (ctor); via wsclient.
//	                                                   Phase 5 v0.6.1 §6.7 钉死：
//	                                                   sweeper 自管 / 接 main ctx /
//	                                                   Server 不再持字段
//	                     allowedRoot                   wshub.go (ctor); via wsclient.
//	                                                   Phase 5 merges with
//	                                                   Server.allowedRoot via NewHub
//	                                                   Options.
//	                     auth                          wshub.go, wshub_upgrade.go
//	agent tailer (3)     tailers                       wshub.go, wshub_agent.go
//	                     wiredLinkersMu                wshub.go, wshub_agent.go
//	                     wiredLinkers                  wshub.go, wshub_agent.go
//	rate-limit/cache (5) historyMarshalCache           wshub.go (ctor),
//	                                                   wshub_eventpush.go,
//	                                                   wshub_subscribe.go,
//	                                                   wshub_eventpush_cache.go
//	                     userSendLimitersMu            wshub_send.go (per-user rate)
//	                     userSendLimiters              wshub_send.go
//	                     connCountByOwnerMu            wshub_upgrade.go (per-owner cap)
//	                     connCountByOwner              wshub_upgrade.go
//
// 加法核对：3 + 10 + 6 + 6 + 14 + 3 + 5 = 47 ✓
//
// Phase 4 抽到 internal/wshub/ 后，方法严格按文件分块（hub_broadcast.go
// 只 WRITE broadcast block + READ shared deps；其余同理）。CI lint
// rule 3 (field_block) 验证此约束。lifecycle 块跨块写豁免（v0.6.1 §五）：
// NewHub / Shutdown / Start 用 LIFECYCLE-METHOD godoc 关键词显式标注。
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
	//
	// Granularity contract (R250-SEC-11 / #1100): this counter is
	// intentionally process-wide / Hub-aggregated, NOT per-client. An
	// authenticated dashboard tab that triggers its OWN SendRaw drops by
	// stalling its WS read can observe DroppedMessages() advance, which
	// in principle gives a 1-bit side-channel for "did anyone else's
	// broadcast also drop in this window?". Mitigation in this codebase
	// is the auth gate on /health (the only HTTP exporter) plus the
	// fact that each authenticated user already shares the same trust
	// boundary as the Hub itself — there is no "untrusted authenticated
	// peer" tier. If a future deployment introduces multi-tenant auth
	// (different users on different shards) this counter MUST move
	// behind a debug-only flag (e.g. /api/debug/vars when debug_mode=
	// true) before being surfaced to per-user JSON. Document at the
	// field rather than at /health so the granularity decision survives
	// a /health rewrite.
	droppedTotal atomic.Int64
	// legacySendInvokes counts how many times sessionSend fell through to
	// sessionSendLegacy (the deprecated pre-MessageQueue branch documented
	// in send.go). The counter unblocks R-LEGACY-SEND (#710) by giving
	// operators and CI a numeric handle on test fixtures still missing a
	// real MessageQueue: a green steady-state production deployment must
	// observe this counter at zero, while migrations land one fixture at a
	// time and watch the counter monotonically drop towards zero in tests.
	// Lock-free atomic so the hot send path stays uncontested.
	legacySendInvokes atomic.Int64
	clients           map[*wsClient]struct{}
	// subscriberCount tracks per-key subscriber count for the
	// maxSubscribersPerKey cap (R246-PERF-4 / #716). Without this, the
	// cap check in handleSubscribe scanned every connected client × every
	// subscription per call (O(N_clients) map lookups under h.mu —
	// 500 conns at the worst case). The counter collapses the check to an
	// O(1) map lookup. Mutated under h.mu alongside c.subscriptions so
	// the two stay consistent. Read-only outside of subscribe/unsubscribe
	// paths; cleared on Shutdown.
	subscriberCount map[string]int
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
	// (sessionSend → SetJobPrompt). Typed as the narrow CronView
	// interface (defined in dashboard_session.go) instead of
	// *cron.Scheduler, so server's coupling to cron is the small
	// CronView method-set, not the full 60+ method scheduler surface.
	// R232-ARCH-7 / R242-ARCH-13 (#754) — was the file-local cronHubOps
	// interface; collapsed into the package-level CronView shared with
	// SessionHandlers. *cron.Scheduler satisfies CronView implicitly.
	scheduler   CronView
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
	// auth lets HandleUpgrade mint a per-browser nz_anon cookie in
	// no-token mode (R20260527122801-SEC-2 / #1326) so the WS upload
	// owner key matches the HTTP path's per-browser bucket instead of
	// falling back to client IP — which co-NAT clients share, allowing
	// a sibling tenant to claim the victim's TakeAll uploads. Optional:
	// older test harnesses that wire NewHub without HubOptions.Auth
	// retain the legacy IP-fallback shape (those Hubs do not run
	// uploadStore so the fallback is not security-relevant).
	auth     *AuthHandlers
	upgrader websocket.Upgrader

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
	// debounceClosedFast mirrors debounceClosed as an atomic flag so post-
	// shutdown callers can short-circuit BEFORE acquiring debounceMu. During
	// process teardown a flurry of producers (router, cron, dashboard send,
	// scratch, etc.) may race a Shutdown() in flight — without this fast
	// path each one serialises on debounceMu just to read the bool and
	// return. Writes happen exclusively under debounceMu so the strict
	// invariant ("flag set ⇒ no new clientWG.Add(1)") is preserved: once
	// Shutdown has flipped the flag, every subsequent caller bails out
	// before Add. The mutex-guarded `debounceClosed` field stays as the
	// authoritative state for code paths that already hold the lock; the
	// atomic is a duplicate read-side accelerator only. R246-PERF-9 / #723.
	debounceClosedFast atomic.Bool
	// debounceFire is the AfterFunc callback assigned once at NewHub. The
	// previous BroadcastSessionsUpdate created a fresh closure literal on
	// every call (high-frequency sidebar refresh path), incurring per-call
	// heap alloc for the captured `h` pointer + func object. R239-PERF-6.
	debounceFire func()

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

	// historyMarshalCache coalesces eventPushLoop's per-subscriber JSON
	// marshal of the "history" frame so N dashboard tabs on one session
	// pay one marshalPooled per notify wave instead of N. R214-PERF-4.
	// Wired in NewHub; cleared on the last unsubscribe per key
	// (handleUnsubscribe / completeSubscribe race-recovery) and on
	// Hub.Shutdown. See wshub_eventpush_cache.go for the fingerprint /
	// fan-out contract.
	historyMarshalCache *historyMarshalCache

	// userSendLimitersMu + userSendLimiters bucket the WS "send" budget by
	// uploadOwner (cookie-MAC / token-hash / IP fallback) instead of
	// per-connection so a single user holding N tabs cannot multiply the
	// burst budget N×. The wsClient.sendLimiter is still the per-conn
	// floor — both must Allow() before the message is processed.
	// R244-SEC-P2-3 / #888.
	userSendLimitersMu sync.Mutex
	userSendLimiters   map[string]*rate.Limiter

	// connCountByOwnerMu + connCountByOwner enforce a per-uploadOwner WS
	// connection sub-cap (maxConnsPerOwner) on top of the global
	// maxWSConns ceiling. Without it a single token holder can monopolise
	// the entire WS pool — R229-SEC-8 / #1022. Increment happens at
	// HandleUpgrade after owner derivation; decrement at unregister so
	// reconnects free the slot deterministically. uploadOwner == ""
	// (anonymous no-token-mode pre-cookie) skips the per-owner cap.
	connCountByOwnerMu sync.Mutex
	connCountByOwner   map[string]int
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
	// Auth, when non-nil, is used by HandleUpgrade to mint a per-browser
	// nz_anon cookie in no-token mode so WS upload owner derivation
	// mirrors the HTTP path. R20260527122801-SEC-2 / #1326.
	Auth *AuthHandlers
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
		subscriberCount:  make(map[string]int),
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
		auth:             opts.Auth,
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
	h.historyMarshalCache = newHistoryMarshalCache()
	// R244-SEC-P2-3 / #888: per-uploadOwner send-budget map. Initialised
	// here so allowSendForOwner can lookup-or-create without a sync.Once.
	h.userSendLimiters = make(map[string]*rate.Limiter)
	// R229-SEC-8 / #1022: per-uploadOwner connection counter so a single
	// token cannot monopolise the global maxWSConns pool.
	h.connCountByOwner = make(map[string]int)
	// R239-PERF-6: pre-bind the AfterFunc callback so BroadcastSessionsUpdate
	// (high-frequency sidebar refresh path) reuses one heap-allocated closure
	// for the lifetime of the Hub instead of allocating a fresh one per call.
	h.debounceFire = func() {
		defer h.clientWG.Done()
		h.debounceMu.Lock()
		h.debounceTimer = nil
		closed := h.debounceClosed
		h.debounceMu.Unlock()
		if closed {
			return
		}
		h.doBroadcastSessionsUpdate()
	}
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

// SetScheduler sets the cron scheduler for auto-saving prompts on first send.
// Accepts the concrete *cron.Scheduler (production wiring) — the field type
// is the narrower CronView interface so the Hub never sees the rest of the
// scheduler API. R242-ARCH-13 (#754): the previous file-local cronHubOps
// interface has been collapsed into the package-level CronView (see
// dashboard_session.go) shared with SessionHandlers — fewer micro-interfaces
// to learn, identical method-set against *cron.Scheduler.
func (h *Hub) SetScheduler(s *cron.Scheduler) { h.scheduler = s }

// SetUploadStore wires the upload store used by WS sends to resolve file_ids
// that were pre-uploaded via POST /api/sessions/upload.
func (h *Hub) SetUploadStore(s *uploadStore) { h.uploadStore = s }

// SetScratchPool wires the ephemeral-session pool so sessionOptsFor can
// resolve AgentOpts for scratch keys without touching the sidebar-visible
// router state.
func (h *Hub) SetScratchPool(p *session.ScratchPool) { h.scratchPool = p }

// allowSendForOwner returns whether the per-user (uploadOwner-keyed) send
// budget admits another "send" message. The per-connection
// wsClient.sendLimiter still gates first; this is the per-user ceiling
// that prevents N tabs from multiplying the 5 sends/s burst by N. Owner
// = "" (anonymous, no-token mode pre-cookie) skips the per-user gate to
// keep the legacy single-user path unchanged. Hand-built hubs that
// bypass NewHub leave userSendLimiters nil; the nil-guard preserves
// their behaviour. R244-SEC-P2-3 / #888.
//
// Budget mirrors the per-conn shape (rate.Every(time.Second), burst=5)
// so a legitimate single-tab user observes no behavioural change. With
// N tabs, the per-user bucket caps the aggregate at the same 5 burst /
// 1 sustained sps regardless of tab count, while the per-conn floor
// still limits a single rogue tab.
func (h *Hub) allowSendForOwner(owner string) bool {
	if h == nil || owner == "" {
		return true
	}
	h.userSendLimitersMu.Lock()
	if h.userSendLimiters == nil {
		h.userSendLimitersMu.Unlock()
		return true
	}
	lim, ok := h.userSendLimiters[owner]
	if !ok {
		lim = rate.NewLimiter(rate.Every(time.Second), 5)
		h.userSendLimiters[owner] = lim
	}
	h.userSendLimitersMu.Unlock()
	return lim.Allow()
}

func (h *Hub) register(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) unregister(c *wsClient) {
	// R249-PERF-23 (#938): the per-key unsub closures (eventLog.Unsubscribe,
	// scheduler.Unsubscribe, …) each acquire their own mutex; calling them
	// inside h.mu serialised every disconnect with len(c.subscriptions)
	// closure invocations, which on heavy-tab clients (50 subs) added
	// 10-50µs of lock-hold per disconnect. Snapshot the closures while
	// holding h.mu (the map mutation must be atomic with decSubscriberCount-
	// Locked), then release h.mu before invoking them.
	//
	// Lock-order: the surviving h.mu critical section only mutates h.clients
	// + h.subscriberCount. The post-lock closures take their own mutexes;
	// none of those mutexes is acquired anywhere with h.mu held in the
	// reverse direction (see Hub.Shutdown's documented invariant), so
	// releasing h.mu before invoking unsub is safe.
	h.mu.Lock()
	removed := false
	var unsubs []func()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		if n := len(c.subscriptions); n > 0 {
			unsubs = make([]func(), 0, n)
			for key, unsub := range c.subscriptions {
				unsubs = append(unsubs, unsub)
				h.decSubscriberCountLocked(key)
			}
		}
		c.subscriptions = nil
		removed = true
	}
	h.mu.Unlock()
	for _, unsub := range unsubs {
		unsub()
	}
	if removed {
		// Release the connCount slot reserved at upgrade time. Guarded on
		// `removed` so a double-unregister (stale close path) cannot leak
		// the counter into negative territory.
		h.connCount.Add(-1)
		// R229-SEC-8 / #1022: free the per-uploadOwner sub-cap slot too.
		h.releaseOwnerSlot(c.uploadOwner)
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
	//
	// R249-PERF-6 (#927): for multi-node deployments the snapshot slice is
	// reused across disconnects via unregisterNodesPool so the steady-state
	// reconnect path drops the per-disconnect `make([]node.Conn, 0, n)`
	// allocation visible in heap profiles.
	h.nodesMu.RLock()
	if len(h.nodes) == 0 {
		h.nodesMu.RUnlock()
		return
	}
	nodesPtr := unregisterNodesPool.Get().(*[]node.Conn)
	nodes := (*nodesPtr)[:0]
	if cap(nodes) < len(h.nodes) {
		nodes = make([]node.Conn, 0, len(h.nodes))
	}
	for _, conn := range h.nodes {
		nodes = append(nodes, conn)
	}
	h.nodesMu.RUnlock()

	for _, conn := range nodes {
		conn.RemoveClient(c)
	}
	// Clear pointer references before returning to the pool so disconnected
	// node.Conn instances stay GC-eligible. Reset length to zero so the next
	// borrower sees an empty slice.
	for i := range nodes {
		nodes[i] = nil
	}
	*nodesPtr = nodes[:0]
	unregisterNodesPool.Put(nodesPtr)
}

// unregisterNodesPool reuses the []node.Conn snapshot slice that
// Hub.unregister builds while holding nodesMu so the multi-node disconnect
// path drops one heap allocation per disconnect. The pool stores pointers
// rather than slices directly so Pool.Put avoids the *[]T → []T copy that
// makes go vet's "Put argument allocates" warning fire. R249-PERF-6 (#927).
var unregisterNodesPool = sync.Pool{
	New: func() any {
		s := make([]node.Conn, 0, 4)
		return &s
	},
}

// maxWSConns caps simultaneous WebSocket upgrades. Exposed here so the
// per-tick broadcast pool (below) stays sized to the real deployment
// envelope instead of a hand-picked 256 that silently disables pooling
// whenever connCount grows past it.
const maxWSConns = 500

// maxConnsPerOwner is the per-uploadOwner sub-cap. Sized so a single
// power-user with browser tabs across multiple devices, plus a CLI/IDE
// integration or two, fits comfortably while still preventing a single
// stolen token from monopolising the maxWSConns global pool.
// R229-SEC-8 / #1022.
const maxConnsPerOwner = 20

// reserveOwnerSlot atomically increments the per-uploadOwner connection
// counter, returning false when the sub-cap (maxConnsPerOwner) is
// already exhausted. The caller is responsible for pairing every
// successful reserve with a releaseOwnerSlot when the connection is
// torn down. Owner == "" (legacy single-user, anonymous pre-cookie)
// always succeeds without bumping the map. R229-SEC-8 / #1022.
func (h *Hub) reserveOwnerSlot(owner string) bool {
	if h == nil || owner == "" {
		return true
	}
	h.connCountByOwnerMu.Lock()
	defer h.connCountByOwnerMu.Unlock()
	if h.connCountByOwner == nil {
		return true
	}
	if h.connCountByOwner[owner] >= maxConnsPerOwner {
		return false
	}
	h.connCountByOwner[owner]++
	return true
}

// releaseOwnerSlot decrements the per-uploadOwner connection counter
// reserved by reserveOwnerSlot. Removes the map entry when the count
// reaches zero so the map stays bounded to the active-user set rather
// than the lifetime-user set. R229-SEC-8 / #1022.
func (h *Hub) releaseOwnerSlot(owner string) {
	if h == nil || owner == "" {
		return
	}
	h.connCountByOwnerMu.Lock()
	defer h.connCountByOwnerMu.Unlock()
	if h.connCountByOwner == nil {
		return
	}
	n := h.connCountByOwner[owner]
	if n <= 1 {
		delete(h.connCountByOwner, owner)
		return
	}
	h.connCountByOwner[owner] = n - 1
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
	// new WG slot past our upcoming clientWG.Wait. The atomic mirror is
	// flipped inside the critical section so its publish happens-before
	// any later Stop()/Reset, keeping the existing R37-CONCUR3 ordering
	// (callers can also observe it lock-free via the atomic-only fast path).
	h.debounceClosed = true
	h.debounceClosedFast.Store(true)
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
	// R249-PERF-24 (#939): mirror unregister's lock-split pattern. The
	// per-key unsub closures (eventLog.Unsubscribe / scheduler.Unsubscribe /
	// …) each take their own mutex; calling them inside h.mu while iterating
	// h.clients serialised every per-client unsub closure — N clients × M
	// subscriptions worth of foreign-mutex acquisitions — under the Hub-wide
	// lock. Snapshot conns + every client's unsubs while holding h.mu (so
	// the map deletes stay atomic with the per-key counter clear), then
	// release h.mu before invoking the closures. Cold path, but the same
	// argument that retired the inline walk in unregister applies here:
	// the post-lock invocation can't deadlock against any other Hub mutex
	// because no closure path acquires h.mu in reverse (see Hub.Shutdown
	// invariant documented at unregister R249-PERF-23 / #938).
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
	// Bulk-clear the per-key counter — every subscriber map was just niled
	// above so the per-key counts are mechanically zero. R246-PERF-4 (#716).
	for k := range h.subscriberCount {
		delete(h.subscriberCount, k)
	}
	h.mu.Unlock()
	for _, unsub := range unsubs {
		unsub()
	}
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

	// R214-PERF-4: drop any cached "history" marshal payloads so the buffers
	// (up to capHistoryBatch entries each) become collectable promptly. Safe
	// after clientWG.Wait — no eventPushLoop will call getOrMarshal again.
	if h.historyMarshalCache != nil {
		h.historyMarshalCache.reset()
	}

	// R244-SEC-P2-3 / #888: drop the per-uploadOwner limiter map so the
	// rate.Limiter values can be GC'd after Hub teardown (test harnesses
	// that build and tear down many Hubs in one process).
	h.userSendLimitersMu.Lock()
	h.userSendLimiters = nil
	h.userSendLimitersMu.Unlock()

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
