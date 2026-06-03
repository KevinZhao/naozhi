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
//	                     userSendLimiters              wshub_send.go (per-user rate; atomic.Pointer)
//	                     connCountByOwnerMu            wshub_upgrade.go (per-owner cap)
//	                     connCountByOwner              wshub_upgrade.go
//
// 加法核对：3 + 10 + 6 + 6 + 14 + 3 + 5 = 47 ✓
//
// Phase 4 抽到 internal/wshub/ 后，方法严格按文件分块（hub_broadcast.go
// 只 WRITE broadcast block + READ shared deps；其余同理）。CI lint
// rule 3 (field_block) 验证此约束。lifecycle 块跨块写豁免（v0.6.1 §五）：
// NewHub / Shutdown / Start 用 LIFECYCLE-METHOD godoc 关键词显式标注。
//
// NEEDS-DESIGN R248-ARCH-6 (#376) — struct extraction anchor:
// PR #327 split wshub.go into per-block files but kept the god-struct with
// every method on *Hub. The next modularization stage extracts three
// cohesive sub-structs out of the field-block map above (one issue each):
//
//	SubscriberRegistry  ← subscriber block (clients / connCount /
//	                      subscriberCount / clientWG) — register/unregister
//	                      fanout surface.
//	BroadcastDispatcher ← broadcast block (debounceMu/Timer/First/Closed/
//	                      ClosedFast/Fire) + droppedTotal — debounce-throttled
//	                      sessions:update fanout.
//	SendCoordinator     ← send block (queue / sendWG / sendTrackMu /
//	                      sendClosed) — owner-loop + TrackSend lifecycle.
//
// Receivers stay on *Hub until each extraction lands; this anchor exists so
// future PRs widen the same map instead of reinventing the boundary. Do NOT
// extract opportunistically — each carries its own lock-ordering contract
// (see shutdown_lock_order_test.go) and must land as a reviewed issue.
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
	// authClients mirrors clients filtered to those whose authenticated
	// flag is true. broadcastToAuthenticated iterates this smaller map
	// instead of walking every connected client × calling
	// c.authenticated.Load() per element. R040034-PERF-23 (#1409): cron's
	// run-started/ended pair fan-out at N concurrent jobs paid one
	// O(N_clients) scan per broadcast wave even though most clients
	// weren't yet authenticated (e.g. brand-new connections still in the
	// auth handshake). Updated under h.mu alongside h.clients so the two
	// stay consistent: register inserts when c.authenticated is already
	// true (the wsDeriveUploadOwner pre-auth path); markAuthenticated
	// inserts when handleAuth flips the flag on success; unregister
	// always removes regardless of state. authenticated is monotonic
	// (only ever true→true; no logout path) so we never need a "remove
	// on deauth" hook.
	//
	// R200109-PERF-4 (#1621): authClients is guarded by its own authMu
	// (a dedicated RWMutex) rather than the Hub-wide h.mu. broadcastTo-
	// Authenticated fans out on every session_state / sessions_update /
	// cron run event — N sessions × multiple events/s — and previously held
	// h.mu.RLock for the whole snapshot scan, serialising behind every
	// register / unregister / markAuthenticated h.mu.Lock. Splitting the
	// mirror onto authMu means the hot read side only contends with the
	// (cheap) authClients writes, not with the full subscription-lifecycle
	// lock. Lock-ordering: the three writers (register / markAuthenticated /
	// unregister) and Shutdown already hold h.mu when they touch authClients
	// — they nest authMu INSIDE h.mu (acquire h.mu first, then authMu). The
	// broadcast read side takes authMu ALONE and never reaches for h.mu, so
	// there is no inverse acquisition and no deadlock. The legacy fallback
	// loop (hand-rolled hubs with nil authClients) still scans h.clients
	// under h.mu because that map is h.mu-owned.
	authMu      sync.RWMutex
	authClients map[*wsClient]struct{}
	// subscriberCount tracks per-key subscriber count for the
	// maxSubscribersPerKey cap (R246-PERF-4 / #716). Without this, the
	// cap check in handleSubscribe scanned every connected client × every
	// subscription per call (O(N_clients) map lookups under h.mu —
	// 500 conns at the worst case). The counter collapses the check to an
	// O(1) map lookup. Mutated under h.mu alongside c.subscriptions so
	// the two stay consistent. Read-only outside of subscribe/unsubscribe
	// paths; cleared on Shutdown.
	subscriberCount map[string]int
	// subscriberCountFast mirrors subscriberCount as a lock-free
	// sync.Map[string]*atomic.Int32 so the WS event-push hot path
	// (singleSubscriber, called once per EventLog notify wave per
	// subscribed client) can read the per-key population WITHOUT taking
	// h.mu. R20260531A-PERF-1 (#1522): the prior singleSubscriber took
	// h.mu.RLock on every push, contending with subscribe/unsubscribe
	// writers on the same mutex at 5-50 events/s × N sessions.
	//
	// The map[string]int above stays the source of truth (it backs the
	// AST-pinned per-key cap check and the unsubscribe drop-cache gate,
	// both of which already run under h.mu). Every mutation of
	// subscriberCount goes through bumpSubscriberCountLocked /
	// decSubscriberCountLocked / the Shutdown bulk-clear, each of which
	// keeps this mirror in step under h.mu. The lock-free reader may
	// observe a value that is at most one writer-critical-section stale,
	// which is acceptable for a fast-path heuristic: a wrong "single"
	// verdict only routes a push through (or around) the marshal cache,
	// never affecting correctness — the multi-tab fan-out still produces
	// byte-identical frames either way.
	subscriberCountFast sync.Map // key string -> *atomic.Int32
	// enforceCaps is the explicit gate for the per-key subscriber cap
	// (maxSubscribersPerKey, gates wshub_subscribe.go:111 and the
	// dropMarshalCache early-out at wshub_subscribe.go:346). NewHub sets
	// it to true alongside initialising subscriberCount. Hand-rolled test
	// hubs that bypass NewHub leave it false so per-key cap and counter
	// reads short-circuit instead of firing against an uninitialised map.
	//
	// Scope is per-key only: the per-client subscription cap
	// (maxSubscriptionsPerClient, wshub_subscribe.go:86) is unconditional
	// because it counts entries in c.subscriptions which always exists on
	// any wsClient. There is no fixture path that needs to suppress the
	// per-client cap, so it does not consult enforceCaps.
	//
	// R040034-SEC-6 (#1401): the previous code keyed the same gate off
	// `subscriberCount == nil`, which read like a defensive nil-guard
	// but was actually load-bearing — a future refactor that allocated
	// subscriberCount eagerly without understanding the gating contract
	// could silently activate caps in every test fixture. The explicit
	// bool documents intent at the use-site: production wiring sets
	// enforceCaps=true; bypass-NewHub tests leave it false. Read under
	// h.mu like subscriberCount.
	enforceCaps bool
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
	// cookieMAC returns the current HMAC-derived auth cookie value
	// (different from dashToken). Stored as a getter callback rather than
	// a cached string so RotateCookieGen invalidations propagate to the
	// WS upgrade comparison without requiring a Hub rebuild — the HTTP
	// path already calls auth.CookieMAC() per request, and the WS path
	// now matches that contract. R040034-SEC-1 (#1398). When opts
	// .CookieMACFn is unset (legacy tests that pass only opts.CookieMAC),
	// NewHub installs a closure returning that constant so the existing
	// test signature keeps compiling without a getter dependency.
	cookieMAC func() string
	guard     *session.Guard
	// R242-GO-10 (#377): Hub depends on the MessageEnqueuer interface, not the
	// concrete *dispatch.MessageQueue, so the dashboard send path is decoupled
	// from dispatch internals and swappable in tests. *dispatch.MessageQueue
	// satisfies it implicitly (var _ binding in wshub_types.go). NewHub guards
	// the assignment so a nil concrete Queue stays a nil interface — the
	// `h.queue == nil` legacy-fallback gate in send.go must keep working.
	queue      MessageEnqueuer // per-key FIFO queue for dashboard sends
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
	auth     *auth.Handlers
	upgrader websocket.Upgrader

	debounceMu sync.Mutex
	// debounceTimer is allocated ONCE in NewHub (via time.AfterFunc bound to
	// debounceFire, then immediately Stopped so it starts idle). Each arming
	// re-uses it with Reset(debounceInterval) rather than allocating a fresh
	// *time.Timer per call — R200109-PERF-14 (#1624). The previous design
	// recreated the timer with time.AfterFunc on every idle→armed transition,
	// allocating a runtime timer struct on the high-frequency sidebar-refresh
	// path. The armed/idle state is now tracked by debounceArmed (below)
	// instead of timer==nil, since the timer object is now Hub-lifetime.
	// Hand-rolled hubs that skip NewHub leave debounceTimer nil and fall back
	// to the per-call time.AfterFunc path in BroadcastSessionsUpdate.
	debounceTimer *time.Timer
	// debounceArmed is the idle/armed sentinel that replaced the old
	// timer==nil check (R200109-PERF-14 #1624): true while a debounce window
	// is pending (a Reset has scheduled the fire callback and clientWG holds
	// the matching Add(1)), false once the callback has run or before the
	// first arm. Written only under debounceMu.
	debounceArmed bool
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

	// userSendLimiters buckets the WS "send" budget by uploadOwner
	// (cookie-MAC / token-hash / IP fallback) instead of per-connection so
	// a single user holding N tabs cannot multiply the burst budget N×.
	// The wsClient.sendLimiter is still the per-conn floor — both must
	// Allow() before the message is processed. R244-SEC-P2-3 / #888.
	//
	// R260528-PERF-18 (#1357): switched from `map[string]*rate.Limiter`
	// guarded by a single sync.Mutex to a sync.Map so the steady-state
	// path (limiter already exists for owner) is a lock-free Load. The
	// previous shape serialised every WS send across all owners through
	// userSendLimitersMu (removed) — at 500 conns × 5 sends/s = 2500 acquisitions/s
	// the map lookup itself was on the hot path under one mutex, blocking
	// concurrent sends from unrelated users.
	//
	// nil pointer == "not enabled" preserves the legacy nil-guard
	// behaviour for hand-built Hubs that bypass NewHub. Shutdown stores
	// nil atomically so in-flight allowSendForOwner callers either observe
	// the live map or the nil fall-through; no torn state.
	// R20260603-PERF-1: atomic.Pointer[sync.Map] replaces the RWMutex guard
	// so the hot send path (allowSendForOwner) is fully lock-free on reads.
	userSendLimiters atomic.Pointer[sync.Map] // map[string]*rate.Limiter; R20260603-PERF-1

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
	Router    *session.Router
	Agents    map[string]session.AgentOpts
	AgentCmds map[string]string
	DashToken string
	// CookieMAC is the static auth-cookie HMAC value used by tests that
	// construct a Hub without wiring AuthHandlers. Production callers
	// should set CookieMACFn instead so RotateCookieGen rotations
	// propagate to the WS comparison. R040034-SEC-1 (#1398).
	CookieMAC string
	// CookieMACFn, when non-nil, is preferred over CookieMAC: NewHub
	// stores the callback directly so each WS upgrade reads the current
	// auth.CookieMAC() value rather than a snapshot taken at server boot.
	// Without this, a future hot-reload that bumps cookieGenSeq would
	// invalidate HTTP cookies but leave WS upgrades accepting the
	// pre-rotation cookie until the process restarts. R040034-SEC-1
	// (#1398).
	CookieMACFn func() string
	Guard       *session.Guard
	Queue       *dispatch.MessageQueue
	Nodes       map[string]node.Conn
	NodesMu     *sync.RWMutex
	ProjectMgr  *project.Manager
	// Resolver, when non-nil, centralises session-key → opts derivation
	// for sessionOptsFor / buildSessionOpts. Wired by server.Start so
	// WS subscribe / send paths share the same planner-binding
	// precedence as the IM dispatch path. Nil falls back to the legacy
	// inlined merge.
	Resolver *session.KeyResolver
	// Scheduler is the optional cron-side hook (CronView) wired at
	// construction. Production callers pass s.scheduler here; nil keeps the
	// auto-save-prompt / stub-revival hooks dormant for tests. R176-ARCH-M3
	// (#431): moved into HubOptions so the prior SetScheduler post-construction
	// setter (and its call-order-vs-Start race) is gone.
	Scheduler CronView
	// ScratchPool lets sessionOptsFor resolve AgentOpts for ephemeral scratch
	// keys. Constructed in Server.New (before NewHub), so it is wired at
	// construction rather than via a post-hoc SetScratchPool setter.
	// R176-ARCH-M3 (#431).
	ScratchPool      *session.ScratchPool
	AllowedRoot      string
	TrustedProxy     bool
	WSAuthLimiter    func(ip string) bool
	WSUpgradeLimiter func(ip string) bool
	// Auth, when non-nil, is used by HandleUpgrade to mint a per-browser
	// nz_anon cookie in no-token mode so WS upload owner derivation
	// mirrors the HTTP path. R20260527122801-SEC-2 / #1326.
	Auth *auth.Handlers
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
	// Resolve cookieMAC into a getter closure. Prefer the live
	// CookieMACFn callback so RotateCookieGen rotations reach
	// wsDeriveUploadOwner without a Hub rebuild (R040034-SEC-1 / #1398).
	// Fall back to the static CookieMAC string for tests that don't wire
	// AuthHandlers; nil-safe-empty when neither is set so the WS upgrade
	// path's `h.CookieMAC() != ""` guard still rejects empty-MAC compares
	// instead of panicking on a nil callback.
	cookieMACFn := opts.CookieMACFn
	if cookieMACFn == nil {
		staticMAC := opts.CookieMAC
		cookieMACFn = func() string { return staticMAC }
	}
	h := &Hub{
		clients:          make(map[*wsClient]struct{}),
		authClients:      make(map[*wsClient]struct{}),
		subscriberCount:  make(map[string]int),
		enforceCaps:      true,
		router:           opts.Router,
		agents:           opts.Agents,
		agentCmds:        opts.AgentCmds,
		dashToken:        opts.DashToken,
		cookieMAC:        cookieMACFn,
		guard:            opts.Guard,
		nodes:            opts.Nodes,
		nodesMu:          opts.NodesMu,
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
		// Delegate to the shared auth.SameOriginOK helper so WS upgrade and the
		// HTTP requireAuth CSRF gate stay in lockstep. The helper already
		// treats empty Origin as permitted (same-origin browsers omit it,
		// non-browser callers don't carry cookies), honours trustedProxy's
		// X-Forwarded-Host fallback, and rejects the opaque "null" origin.
		CheckOrigin:     func(r *http.Request) bool { return auth.SameOriginOK(r, h.trustedProxy) },
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
	// R260528-PERF-18 (#1357): sync.Map keyed by owner string lets the
	// steady-state Allow() path skip a global mutex on every WS send.
	h.userSendLimiters.Store(&sync.Map{})
	// R229-SEC-8 / #1022: per-uploadOwner connection counter so a single
	// token cannot monopolise the global maxWSConns pool.
	h.connCountByOwner = make(map[string]int)
	// R239-PERF-6: pre-bind the AfterFunc callback so BroadcastSessionsUpdate
	// (high-frequency sidebar refresh path) reuses one heap-allocated closure
	// for the lifetime of the Hub instead of allocating a fresh one per call.
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
	// R200109-PERF-14 (#1624): pre-allocate the debounce timer ONCE here,
	// bound to the pre-bound fire callback, then Stop it so it starts idle.
	// BroadcastSessionsUpdate arms it with Reset(debounceInterval) on each
	// idle→armed transition instead of calling time.AfterFunc per call, which
	// allocated a fresh runtime *time.Timer every time the window reopened.
	// Stop() may return false if the just-created timer were already pending,
	// but AfterFunc(MaxInt64) cannot fire within this window, so draining its
	// channel is unnecessary (AfterFunc timers have no observable C). The
	// timer stays idle until the first Reset.
	h.debounceTimer = time.AfterFunc(time.Duration(math.MaxInt64), h.debounceFire)
	h.debounceTimer.Stop()
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
	} else {
		// Assign through the concrete-nil guard: storing a typed-nil
		// *dispatch.MessageQueue straight into the interface field would make
		// `h.queue == nil` (the send.go legacy-fallback gate) read false. Only
		// wrap a non-nil queue so the gate keeps its original meaning.
		h.queue = opts.Queue
	}
	return h
}

// SetUploadStore wires the upload store used by WS sends to resolve file_ids
// that were pre-uploaded via POST /api/sessions/upload. Kept as a
// post-construction setter (unlike Scheduler/ScratchPool which moved into
// HubOptions in R176-ARCH-M3 #431) because the upload store's cleanup loop is
// bound to the app-lifecycle ctx and is therefore created after the Hub exists
// — see registerDashboard's R215-ARCH-P2-3 (#579) note.
func (h *Hub) SetUploadStore(s *uploadStore) { h.uploadStore = s }

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
//
// R260528-PERF-18 (#1357): the steady-state path (limiter already exists
// for the owner) does a lock-free sync.Map.Load. The map pointer is
// guarded by an RWMutex to serialise Shutdown's nil-store with in-flight
// callers; the read-side critical section does nothing but copy the
// pointer so the lock contention is bounded by Shutdown frequency, not
// by send throughput. Previously every send acquired one process-wide
// sync.Mutex which serialised all owners' sends through a single queue.
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
	// Race: another goroutine may have created the limiter for this
	// owner in parallel. LoadOrStore returns the canonical value, so
	// the speculative NewLimiter is discarded on collision. NewLimiter
	// is a small struct alloc — far cheaper than holding a process-wide
	// mutex across the rate.NewLimiter call.
	v, _ := m.LoadOrStore(owner, rate.NewLimiter(rate.Every(time.Second), 5))
	return v.(*rate.Limiter).Allow()
}

func (h *Hub) register(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	// R040034-PERF-23 (#1409): wsDeriveUploadOwner pre-auth flips
	// c.authenticated.Store(true) BEFORE register(c) runs (no-token mode
	// or matching auth-cookie path), so any preauthenticated client must
	// land in authClients here. Token-mode clients arrive unauthenticated
	// and join via markAuthenticated when handleAuth succeeds. Hand-rolled
	// test hubs that allocate &Hub{} directly leave authClients nil; the
	// nil-guard preserves the legacy behaviour.
	if h.authClients != nil && c.authenticated.Load() {
		// R200109-PERF-4 (#1621): authClients writes nest authMu inside
		// h.mu so the broadcast read side (authMu alone) sees a consistent
		// mirror without contending on the Hub-wide lock.
		h.authMu.Lock()
		h.authClients[c] = struct{}{}
		h.authMu.Unlock()
	}
	h.mu.Unlock()
}

// markAuthenticated inserts c into the authClients mirror after handleAuth
// flips c.authenticated to true. Calling this with the auth state already
// stored is the contract — c.authenticated.Load() must be true at this
// point so broadcastToAuthenticated cannot iterate stale entries. Nil
// authClients (hand-rolled test hubs) is a defensive no-op so legacy
// fixtures continue to work; production hubs always allocate the map in
// NewHub. R040034-PERF-23 (#1409).
func (h *Hub) markAuthenticated(c *wsClient) {
	h.mu.Lock()
	// Re-check under the lock that the client is still registered: a
	// race between unregister and a delayed handleAuth (the readPump
	// arm processed an auth frame just before the conn closed) would
	// otherwise reinsert a torn-down client into authClients and pin it
	// for the next broadcast. Membership in h.clients is the single
	// source of truth for "this client is alive enough to broadcast to".
	if h.authClients != nil {
		if _, ok := h.clients[c]; ok {
			// R200109-PERF-4 (#1621): authMu nested inside h.mu.
			h.authMu.Lock()
			h.authClients[c] = struct{}{}
			h.authMu.Unlock()
		}
	}
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
		// R040034-PERF-23 (#1409): keep authClients consistent with the
		// authoritative h.clients map. delete on a missing key is a
		// well-defined no-op so the unconditional delete handles both the
		// authenticated and (rare) never-authenticated disconnect paths.
		if h.authClients != nil {
			// R200109-PERF-4 (#1621): authMu nested inside h.mu.
			h.authMu.Lock()
			delete(h.authClients, c)
			h.authMu.Unlock()
		}
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

	// R260528-PERF-11 (#1356): fan-out RemoveClient across nodes in
	// parallel so a 5-node deployment pays max(RTT) instead of sum(RTT)
	// per disconnect. WS reconnect storms (mobile sleep/wake, dashboard
	// tab swap) used to amortise N×RTT through this serial loop while
	// holding the readPump goroutine open; for N=5 nodes that is 5
	// sequential RPCs every disconnect. We block on the WaitGroup so
	// Shutdown's nodes.Close() in the documented ordering still runs
	// strictly after every in-flight RemoveClient call returns — the
	// readPump's defer calls unregister synchronously, so clientWG.Wait
	// in Shutdown still observes the full removal completing.
	//
	// Single-node deployments skip the goroutine spawn (no fan-out
	// benefit) to keep the steady-state path identical to before.
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
	// R200109-PERF-14 (#1624): the timer is now Hub-lifetime (pre-allocated
	// in NewHub), so the "is a broadcast pending?" question is answered by
	// debounceArmed, not by timer==nil. Only an armed timer holds a clientWG
	// slot worth releasing. When Stop() returns true the callback was
	// cancelled before running, so we Done() the slot it reserved; when it
	// returns false the callback already fired and its deferred Done()
	// balances the Add(). The timer object itself is left intact (not nilled)
	// so it can be reused — Stop() leaves it idle. Hand-rolled hubs that
	// never went through NewHub leave debounceTimer nil; guard for that.
	if h.debounceArmed && h.debounceTimer != nil {
		if h.debounceTimer.Stop() {
			h.clientWG.Done()
		}
		h.debounceArmed = false
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
		// R20260531A-PERF-1 (#1522): keep the lock-free mirror in step.
		h.subscriberCountFast.Delete(k)
	}
	// R040034-PERF-23 (#1409): drain authClients alongside h.clients so a
	// post-Shutdown broadcast (already racing the cancel/Wait barrier)
	// cannot iterate stale wsClient pointers. Empty map preserved (vs
	// nilling) so any straggler markAuthenticated call goes through the
	// h.clients membership check above and short-circuits on the empty
	// clients map.
	// R200109-PERF-4 (#1621): authMu nested inside h.mu (Shutdown holds
	// h.mu here); broadcast readers blocked on authMu drain to an empty
	// mirror.
	h.authMu.Lock()
	for c := range h.authClients {
		delete(h.authClients, c)
	}
	h.authMu.Unlock()
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
	// R260528-PERF-18 (#1357): the map is a sync.Map; nilling the pointer
	// via atomic.Pointer.Store serialises with in-flight allowSendForOwner
	// callers — they observe either the live map or the nil fall-through,
	// never a torn pointer. R20260603-PERF-1: RWMutex removed.
	h.userSendLimiters.Store(nil)

	// Drop the per-uploadOwner conn-count map so test harnesses that
	// build/teardown many Hubs in one process don't accumulate stale
	// owner→count entries through the Hub reference chain. Sibling to
	// userSendLimiters above; same lifecycle constraint (post clientWG.Wait,
	// no further reserveOwnerSlot/releaseOwnerSlot caller will run).
	h.connCountByOwnerMu.Lock()
	h.connCountByOwner = nil
	h.connCountByOwnerMu.Unlock()

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
