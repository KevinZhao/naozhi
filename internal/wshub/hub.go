// Hub manages WebSocket client connections and event subscriptions.
//
// Field-block contract (server-split-phase4-design v0.6.1 §五):
// **49 fields organized in 7 blocks** (Phase 4b-hub-sync 同步 master：
// v0.6.1 47 字段 → 实测 49；新增 authClients / enforceCaps；userSendLimiters
// 类型从 map → *sync.Map）。
// Phase 4a 骨架并存：本 struct 是 server.Hub 的目标镜像。骨架阶段
// NewHub 仅初始化字段（不启动 readPump goroutine），Shutdown 完成
// lifecycle 块跨块写协调链路；其他 hub_*.go 文件含 placeholder 方法
// 满足 rule 3a marker 校验。Phase 4b 起接管实际方法实装。
//
// 加法核对：lifecycle 3 + subscriber 12 + broadcast 6 + send 6 +
// shared 14 + tailer 3 + cache 5 = 49 ✓
// （subscriber 块从 10 → 12：authClients + enforceCaps）
//
// 方法严格按文件分块（hub_broadcast.go 只 WRITE broadcast block + READ
// shared deps；其余同理）。CI lint rule 3a 验证文件 godoc 头含 WRITES
// marker；Phase 4b rule 3b 升级 AST 字段访问对账。
//
// lifecycle 块跨块写豁免（v0.6.1 §五）：NewHub / Shutdown / Start 用
// LIFECYCLE-METHOD godoc 关键词显式标注。
package wshub

import (
	"context"
	"crypto/sha256"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Hub is the Phase 4 抽包目标 struct——49 字段镜像 server.Hub（Phase 4b-hub-sync 校准）。Phase 4a
// 骨架阶段：字段定义 + NewHub 初始化 + Shutdown 协调链路。方法实装留
// Phase 4b。
type Hub struct {
	// ── lifecycle (3) ──────────────────────────────────
	mu     sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc

	// ── subscriber registry (12) ────────────────────────
	clients          map[*wsClient]struct{}
	authClients      map[*wsClient]struct{} // R260528-PERF / #1409: authenticated client mirror for broadcastToAuthenticated
	connCount        atomic.Int64
	subscriberCount  map[string]int
	clientWG         sync.WaitGroup
	wsAuthLimiter    func(ip string) bool
	wsUpgradeLimiter func(ip string) bool
	upgrader         websocket.Upgrader
	dashTokenHash    [32]byte
	cookieMAC        string
	trustedProxy     bool
	enforceCaps      bool // R260528 / #1401: explicit gate for per-key subscriber cap

	// ── broadcast (6) ──────────────────────────────────
	debounceMu         sync.Mutex
	debounceTimer      *time.Timer
	debounceFirst      time.Time
	debounceClosed     bool
	debounceClosedFast atomic.Bool
	debounceFire       func()

	// ── send / queue (6) ───────────────────────────────
	queue             MessageEnqueuer
	sendWG            sync.WaitGroup
	sendTrackMu       sync.Mutex
	sendClosed        bool
	droppedTotal      atomic.Int64
	legacySendInvokes atomic.Int64

	// ── shared dependencies (14, read-only after ctor) ─
	router      HubRouter
	agents      map[string]any // *session.AgentOpts in Phase 4b
	agentCmds   map[string]string
	dashToken   string
	guard       any            // *session.Guard in Phase 4b
	nodes       map[string]any // node.Conn in Phase 4b
	nodesMu     *sync.RWMutex
	projectMgr  any // *project.Manager in Phase 4b
	resolver    any // *session.KeyResolver in Phase 4b
	scheduler   CronView
	scratchPool ScratchOps
	uploadStore UploadOps
	allowedRoot string
	auth        Auth

	// ── agent tailer subsystem (3) ─────────────────────
	tailers        any // *tailerRegistry in Phase 4b
	wiredLinkersMu sync.Mutex
	wiredLinkers   map[any]struct{}

	// ── rate-limit / cache (5) ─────────────────────────
	historyMarshalCache     any // *historyMarshalCache in Phase 4b
	userSendLimitersStoreMu sync.Mutex
	userSendLimiters        *sync.Map // R260528-PERF-18 #1357: keyed by owner string, lock-free Allow() steady-state
	connCountByOwnerMu      sync.Mutex
	connCountByOwner        map[string]int
}

// wsClient is the Phase 4a placeholder for server.wsClient. Phase 4b
// 起从 server 包搬过来；目前是空 struct 满足 Hub.clients map key 类型
// 的编译需求。
type wsClient struct{}

// HubOptions is the constructor input for NewHub. Phase 4a 镜像
// server.HubOptions——Phase 4b 起 server.HubOptions 退役，本类型成为
// 唯一定义点。
type HubOptions struct {
	Router           HubRouter
	Agents           map[string]any
	AgentCmds        map[string]string
	DashToken        string
	CookieMAC        string
	Guard            any
	Queue            MessageEnqueuer
	Nodes            map[string]any
	NodesMu          *sync.RWMutex
	ProjectMgr       any
	Resolver         any
	AllowedRoot      string
	TrustedProxy     bool
	WSAuthLimiter    func(ip string) bool
	WSUpgradeLimiter func(ip string) bool
	Auth             Auth
	// EnforceCaps gates per-key subscriber cap enforcement (R260528 / #1401).
	// Production must keep true; test harnesses may set false to bypass
	// the maxSubscribersPerKey + dropMarshalCache early-out path.
	// Zero value (false) here is misleading—NewHub defaults to true unless
	// caller explicitly opts out via EnforceCapsExplicit pattern. For now
	// the simpler approach: NewHub always sets true, tests overwrite via
	// h.enforceCaps = false directly.
	EnforceCaps bool
	Scheduler   CronView
	ScratchPool ScratchOps
	UploadStore UploadOps
	// ParentCtx is the application-level context whose cancellation must
	// propagate to the Hub. When set, NewHub derives h.ctx via
	// context.WithCancel(ParentCtx) so that parent-ctx cancel tears down
	// send/push goroutines even if Shutdown() is not explicitly called.
	// Nil falls back to context.Background() to preserve legacy behaviour.
	ParentCtx context.Context
}

// NewHub creates a new wshub.Hub instance.
//
// LIFECYCLE-METHOD: writes ctx/cancel (lifecycle), all subscriber/broadcast/
//
//	send/shared/tailer/cache fields (initialization). Lock-order
//	documented in docs/design/server-split-phase4-design.md §五.
//
// Phase 4a 骨架：仅初始化字段，不启动 readPump / debounce / agent tailer
// 等 goroutine——这些留 Phase 4b 实装方法时一并补。骨架的 NewHub 已经能
// 通过 Shutdown 测试以验证 lifecycle 块跨块写协调链路设计。
func NewHub(opts HubOptions) *Hub {
	parent := opts.ParentCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)

	h := &Hub{
		// lifecycle
		ctx:    ctx,
		cancel: cancel,

		// subscriber
		clients:          make(map[*wsClient]struct{}),
		authClients:      make(map[*wsClient]struct{}),
		subscriberCount:  make(map[string]int),
		wsAuthLimiter:    opts.WSAuthLimiter,
		wsUpgradeLimiter: opts.WSUpgradeLimiter,
		cookieMAC:        opts.CookieMAC,
		trustedProxy:     opts.TrustedProxy,
		enforceCaps:      true, // 默认启用 cap 强制；test 可显式 opts.EnforceCaps=false 关闭

		// shared deps
		router:      opts.Router,
		agents:      opts.Agents,
		agentCmds:   opts.AgentCmds,
		dashToken:   opts.DashToken,
		guard:       opts.Guard,
		queue:       opts.Queue,
		nodes:       opts.Nodes,
		nodesMu:     opts.NodesMu,
		projectMgr:  opts.ProjectMgr,
		resolver:    opts.Resolver,
		allowedRoot: opts.AllowedRoot,
		auth:        opts.Auth,
		scheduler:   opts.Scheduler,
		scratchPool: opts.ScratchPool,
		uploadStore: opts.UploadStore,

		// rate-limit / cache
		userSendLimiters: &sync.Map{},
		connCountByOwner: make(map[string]int),

		// agent tailer
		wiredLinkers: make(map[any]struct{}),
	}

	// websocket upgrader (subscriber 块写)
	h.upgrader = websocket.Upgrader{
		CheckOrigin:     func(r *http.Request) bool { return true },
		ReadBufferSize:  8192,
		WriteBufferSize: 8192,
	}
	if opts.DashToken != "" {
		h.dashTokenHash = sha256.Sum256([]byte(opts.DashToken))
	}

	// debounceFire pre-binding (broadcast 块写)。Phase 4b 起绑定真正的
	// broadcast fanout；4a 仅占位 closure。
	h.debounceFire = func() {}

	return h
}

// Shutdown coordinates orderly teardown across all 7 field blocks.
//
// LIFECYCLE-METHOD: writes ctx/cancel (lifecycle), debounceClosed +
//
//	debounceClosedFast (broadcast), sendClosed (send), close clients map
//	(subscriber). Lock-order documented in
//	docs/design/server-split-phase4-design.md §五.
//
// Phase 4a 骨架：5 步顺序协调链路（v0.6.1 §6.5 共同特别关注）：
//
//  1. h.cancel()                          → 通知所有监听 ctx 的 goroutine
//  2. debounceClosed = true (mu)         → broadcast 不再调度新 debounce
//  3. drain sendWG (sendTrackMu)         → 等所有 send goroutine 退出
//  4. close clients map (mu)             → 关闭所有 wsClient
//  5. wait clientWG                      → 等所有 readPump 退出
//
// Phase 4b 实装方法时按本骨架的顺序细化。骨架 Shutdown 通过返回 nil
// 让 hub_concurrency_test.go 验证 NewHub + Shutdown 组合不死锁。
//
// Idempotency: 多次调用 Shutdown 必须不 panic / 不死锁——cancel 已经是
// idempotent (sync.Once 内部保护)，bool 字段写多次幂等，clients=nil
// 之后 range 0 元素 noop。
func (h *Hub) Shutdown(ctx context.Context) error {
	// Step 1: cancel ctx
	if h.cancel != nil {
		h.cancel()
	}

	// Step 2: debounce closed (broadcast 块跨块写)
	h.debounceMu.Lock()
	h.debounceClosed = true
	h.debounceMu.Unlock()
	h.debounceClosedFast.Store(true)

	// Step 3: drain sendWG (send 块跨块写)
	h.sendTrackMu.Lock()
	h.sendClosed = true
	h.sendTrackMu.Unlock()
	// Phase 4b 起 sendWG.Wait()——4a 骨架无 send goroutine，跳过。
	// 即便如此，必须 reference sendWG 字段保留写入意图。
	_ = &h.sendWG

	// Step 4: close clients (subscriber 块跨块写)
	h.mu.Lock()
	for c := range h.clients {
		// Phase 4b 起调用 c.close()——4a 骨架 wsClient 是空 struct
		_ = c
	}
	h.clients = nil
	h.mu.Unlock()

	// Step 5: wait clientWG
	_ = &h.clientWG // Phase 4b 起 clientWG.Wait()

	return nil
}
