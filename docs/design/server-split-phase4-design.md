# server 包拆分 Phase 4 设计稿

> **状态**：设计稿 v0.4（2026-05-25），已应用 v0.1/v0.2/v0.3 共 8 轮独立 reviewer 反馈，全部字段加法实地核对到 baseline。**Phase 0 实施前等最后一次 sign-off**。
>
> **配套数据**：[server-split-phase4-baseline.md](server-split-phase4-baseline.md) — 真实 baseline（v0.2 数字粗估，v0.3 全部用实测替换）。
>
> **背景**：[server-split-design v1](server-split-design.md) 的 Phase 1-2 已完成（抽出 `internal/node` / `internal/dispatch`），Phase 3 完成（`sessionGuard` → `internal/session/guard.Guard`）。当时设计目标 server 包瘦身到 ~3117 行；**两年后实际膨胀到 17143 行 / 40 文件 / 50 路由**，新增 cron-dashboard / project-files / scratch / memory / agent-events / cli-backends / upload-store / agent-tailer / sysession 等 8+ 板块。本设计是 Phase 4 的二次拆分。
>
> **本次拆分的真正任务**不仅是把代码搬出去，更是 **建立长效防膨胀机制**——避免两年后再次出现同款症状。

---

## 一、现状盘点

> 数据采集见 [`server-split-phase4-baseline.md`](server-split-phase4-baseline.md)（2026-05-25）。
> v0.2 给的"17143 行 / 40 文件 / Server 28+ 字段 / Hub 28+ 字段"是粗估值，本节用 baseline 实测数据替换。

```
server 包行数:  含测试 38836 行 / 111 文件
              不含测试 17156 行 / 40 文件

> 800 行的非测试文件（6 个，linter 硬上限超线）：
  dashboard_session.go        1514
  dashboard_cron.go           1418
  server.go                   1309
  project_files.go            1302
  dashboard_send.go           1214
  dashboard_cron_transcript.go 909

500-800 行（4 个）：
  dashboard.go                 731
  send.go                      699
  agent_tailer.go              655
  wshub.go                     514

Server struct: 47 字段（baseline 实测，13 个是 12 个 handler 引用）
Hub struct:    37 字段（baseline 实测，按职责分 5 块见 §五）
路由注册总数: 50 条（dashboard.go::registerDashboard 单点）
Hub.queue 字段：直接耦合 *dispatch.MessageQueue（R242-GO-10 拖了 5 轮）

90 天活跃度（影响 ROI 决策）：
  仓库总 commit 306 / 涉 server 包 160（52.3%）
  70% 的 server 包 commit 改 ≥ 2 个文件
  最热文件 wshub.go / dashboard_cron.go 各 27 次改动
```

handler-group 化第一轮（`auth/cronH/sessionH/projectH/discoveryH/transcribeH/sendH/cliH/scratchH/memoryH/agentEventsH/healthH`）已完成，但 12 个 handler-struct 仍然 **持有 Server 字段引用** 或被 Server 注入若干字段，本质上是把"god struct"折叠成"god struct + 12 个 view"，没真正解耦——**这是 v0.1 reviewer 一致指出的"换汤不换药"风险，v0.2 整改的核心**。

---

## 二、设计目标（钉死，可量化）

| # | 目标 | 量化指标 | 验证方法 |
|---|---|---|---|
| 1 | server 包瘦身 | ≤ 5000 行（不含测试）/ ≤ 15 个非测试文件 | `wc -l $(ls internal/server/*.go \| grep -v _test.go)` |
| 2 | Server struct 字段瘦身 | **47 → ≤ 12** 字段（减 74%） | `awk '/^type Server struct/,/^}$/' internal/server/server.go \| grep -E '^\s+[a-zA-Z_]+ ' \| wc -l` |
| 3 | Hub 字段整理 | **37 字段维持不变**（按 §五 5 块分组组织；本次不删字段，仅整理） | 同上脚本针对 wshub.go |
| 4 | 跨包依赖减耦合 | 每个 PR 减少 `s.X` 跨包字段引用数 ≥ 5 | `grep -c "deps.\|s\." per-PR diff 报告` |
| 5 | Hub.queue 接口化 | 关闭 R242-GO-10；接口拆 3 个 ≤ 3 方法 | `var _ wshub.MessageEnqueuer = (*MessageQueue)(nil)` |
| 6 | 零行为变更 | 50 路由路径不变；WS 协议字段不变 | `routes_snapshot_test.go` golden 对齐 |
| 7 | 防止重新膨胀 | AST linter + 文件大小硬上限 + 文档化包契约 | CI gate |
| 8 | 渐进推进 | 单 PR ≤ 1500 行；任一 phase 中断不阻塞后续 | PR diff stat |

### 不做什么

- ❌ 不引入 DI 框架（wire/fx）
- ❌ 不重写 dashboard handler 业务逻辑（搬家不动逻辑）
- ❌ 不动 `internal/dispatch` / `internal/node`（已稳定 4 周+）
- ❌ 不动 WS 协议（线协议字段、消息类型保持二进制兼容）
- ❌ 不动 50 条 HTTP 路由路径（dashboard.html 与 IM 客户端零改动）
- ❌ 不拆 Hub 为三子 struct（v0.1 计划取消，详见 §五）
- ❌ 不改 `/health` 拆分（R247-ARCH-1，正交，另起 PR）
- ❌ 不动 `scratchPool` 归属（保留在 Server）

---

## 三、切分总图

```
拆分前：
  internal/server/                 17143 行 / 40 文件
    ├── server.go (1309) god-ctor + struct
    ├── wshub*.go (~3000) WS hub god-struct
    ├── dashboard*.go (~10000) 12 个 handler-group
    ├── project_files.go (1302) 项目文件 IO
    ├── send.go / agent_tailer.go / upload_store.go / discovery_cache.go
    └── 安全/认证: csrf / clientip / ip_limiter / debug_*

拆分后：
  internal/server/                 ~3500 行 / ≤ 15 文件
    ├── server.go         核心 struct (≤ 12 字段) + Start/Shutdown
    ├── routes.go         50 条路由注册（来源：dashboard.go 注册块）
    ├── middleware.go     auth/csrf/gzip/ip_limiter wrappers
    ├── debug.go          /api/debug/{pprof,vars}
    ├── workspace.go      validateWorkspace + sentinels（被多处复用）
    └── handlers/         (sub-package, 见下) — 通过 routes.go 注入

  internal/dashboard/              ~7000 行  按业务子域抽 6 个独立子包
    ├── auth/             ~600  登录/cookie/CSRF
    ├── session/          ~3500 sessions/events/send/upload/attachment/agent-tailer
    ├── cron/             ~2700 cron CRUD/runs/transcript
    ├── project/          ~1700 project_api + project_files
    ├── discovery/        ~600  discovered list/preview/takeover/close
    └── ext/              ~1400 scratch/memory/agent-events/cli-backends/transcribe/system

  internal/wshub/                  ~3500 行  WebSocket Hub（保单 struct）
    ├── hub.go            Hub struct（字段按 §五分组）+ ctor + Shutdown
    ├── hub_broadcast.go  广播 / debounce 路径方法
    ├── hub_subscribe.go  订阅 / 注册 / 取消订阅方法
    ├── hub_send.go       发送 / 队列 / sendWithBroadcast 方法
    ├── client.go         wsClient (单连接读写泵)
    ├── upgrade.go        HTTP upgrade + auth gate
    ├── eventpush.go      事件推送循环
    ├── tailer.go         agent_subscribe/agent_unsubscribe (和 agent_tailer.go 强耦合，跟着搬)
    ├── consumer.go       HubRouter / cronHubOps / scratchOps interfaces (consumer-side)
    └── types.go          MessageEnqueuer / MessageQueueControl / MessageQueueStats interfaces

  embed.FS                         ~700  HTML/JS/manifest/sw 跟着 server 入口（不抽子包）

总计：~14000 行（vs 拆分前 17156 减 18%；server 包从 17156 → 3500 减 80%）。
减少来自重复样板抽公共 + 跨包接口去 dead-import；不允许"顺便"重命名 / 重构。
```

---

## 四、Handler 与 Hub 解耦策略（**v0.2 核心整改**）

### 4.1 Deps struct 接口化（**v0.3 收紧：双轨策略 + 跨包契约文档**）

> **v0.2 反馈**：6 子包 × 3-4 接口 = 20+ 个本地接口，接口加方法时实现侧反向注释靠人维护必腐烂；跨方法序列契约（如 Enqueue → ShouldNotify）单接口 godoc 表达不了。
>
> **v0.3 双轨策略**：不一刀切"全接口化"；按使用频度区分。

每个 dashboard 子包定义自己的 `Deps`。Deps 字段按下面双轨规则选择：

#### 4.1.1 双轨硬名单（**v0.4 N10 整改：消除"≤ 6 方法"模糊判据**）

> **v0.3 错点**："高频 / 低频" + "≤ 6 方法" 是模糊表述，code review 时 reviewer 之间无统一判据。v0.4 改为**枚举式硬名单**。

##### 直接持具体类型（白名单 5 个 — 高频、方法多、跨子包稳定）

```
*session.Router       — 50+ 方法；Phase 4-5 内不动
*cron.Scheduler       — 60+ 方法；多子包共享
*project.Manager      — 30+ 方法；多子包共享
*session.KeyResolver  — Phase 4 实测 dashboard/wshub 都依赖
*wshub.Hub            — 大型对象；只有 broadcaster / send 等 ≤ 3 方法的视图通过接口隔离
```

##### 必须本地接口化（黑名单 — 子包私有窄面 ≤ 3 方法）

```
broadcaster                  — dashboard/cron 等用于 BroadcastSessionsUpdate 1 方法
pathLocator                  — dashboard/discovery 用于路径查询 ≤ 3 方法
sessionWriter                — dashboard/session/send 子包用于 Send 1 方法
... (Phase 1-3 期间各子包按需添加)
```

##### 中间地带（≤ 6 方法但不在白名单）

由 PR reviewer 单条决定（必须给理由），例如某子包只用 `*cron.Scheduler` 的 2 个方法，可以选：

- **A**：仍持 `*cron.Scheduler`（享受未来加方法不改子包的便利；但 mock 测试必须 `embed *cron.Scheduler`）
- **B**：定义本地接口（隔离窄面；mock 简单；但 cron 加方法时需评估子包是否要扩接口）

reviewer 在 PR 中指明选 A 或 B + 理由。

##### 不一刀切的理由

6 子包 × 3-4 个本地接口 = 20+ 接口面，碎片化反而比直接持有具体类型更难维护（v0.2 reviewer #2 + v0.3 reviewer #1 一致提）。白名单优先具体类型，黑名单仅限子包私有窄面。

#### 4.1.2 模板

```go
// internal/dashboard/cron/deps.go
package dashboardcron

// 高频跨包：直接持具体类型，不引入小接口
type Deps struct {
    Scheduler   *cron.Scheduler              // 高频，方法多，直接持
    Router      *session.Router              // 高频，直接持
    Hub         broadcaster                  // 低频窄面，本地接口
    AllowedRoot string
    Resolver    *session.KeyResolver
}

// broadcaster: Hub.BroadcastSessionsUpdate 是这里唯一用到的方法
type broadcaster interface {
    BroadcastSessionsUpdate()
}

type Handlers struct{ deps Deps }
func New(deps Deps) *Handlers { return &Handlers{deps: deps} }
```

测试 mock 用 embedded mock（不需写完整接口）：

```go
type mockScheduler struct {
    *cron.Scheduler          // 嵌入真 type；零方法
    listFn   func() []cron.Job
}
func (m *mockScheduler) List() []cron.Job { return m.listFn() }
```

#### 4.1.3 跨方法序列契约：放设计文档不放 godoc

某些接口的方法间有时序约束（v0.2 reviewer #2 提到 `MessageQueue.Enqueue` 返回 `isOwner=true` 后必须**驱动到完成**才能再 Enqueue 同 key）。这种 cross-method invariant 单 godoc 行表达不了。

**整改**：新建 [`docs/design/server-consumer-contracts.md`](server-consumer-contracts.md)，对每个跨包接口写：

- 哪些 consumer 包持有
- 跨方法时序契约（如 "Enqueue 返回 isOwner=true 后调用方必须保证 DoneOrDrain 最终被调用"）
- 哪些方法可以演化（加参数 / 加方法）/ 哪些方法是 stable
- 实现侧 godoc 反向引用此文档锚点

实现侧 godoc 模板：

```go
// internal/dispatch/msgqueue.go

// MessageQueue ...
//
// satisfies: wshub.MessageEnqueuer (see docs/design/server-consumer-contracts.md#message-queue)
//
// Cross-method contract: Enqueue returning isOwner=true requires the
// caller to eventually invoke DoneOrDrain for the same key, otherwise
// subsequent Enqueue calls for that key block forever.
type MessageQueue struct { ... }
```

AST linter rule 4（§六.2.0.4）扫所有 `// satisfies:` 注释，对账消费侧接口实际方法集是否被 godoc 中提到的 type 实现 — 漂移就 fail。这把"反向注释靠人维护"改成 CI 自动验证。

### 4.2 PR-level 减耦合 KPI

每个 phase 的 PR description 必须报：

```
跨包字段引用数变化：
  Before: grep -c "func .*\*cron\.Scheduler" internal/server/dashboard_cron.go = X
  After:  同 grep on internal/dashboard/cron/handlers.go = Y
  Reduction: X - Y >= 5
```

**Y < X - 5 的 PR 不予合并** —— 这是防止"挪了文件没减耦合"的硬卡口。

### 4.3 不搬业务逻辑、只搬位置（核心规约）

每个 phase 的 PR 必须满足：

```bash
# Before / After 对每个 handler 跑这条 diff，差异必须 ≤ 包路径修改 + receiver 改名
git diff phase-N..phase-N-1 -- '*.go' | grep -vE "^[+-]package |^[+-]import " | wc -l
# 期望：每个 handler ≤ 5 行实质变化
```

**例外**——以下变化不计入"行为变化"，但必须在 PR description 单独列出：

- 引用路径变更（`s.scheduler.X()` → `h.deps.Scheduler.X()`）
- 类型重命名（`*Server` receiver → `*Handlers`）
- import 列表调整
- godoc 锚点同步

任何"顺便修个 bug"或"重命名内部函数"必须**另起 PR**，不在拆分窗口里做。

### 4.4 `MessageEnqueuer` 单接口（**v0.3 收回 v0.2 三接口拆分**）

> v0.2 提出按 ISP 拆 `MessageEnqueuer` / `MessageQueueControl` / `MessageQueueStats` 三接口。
>
> v0.2 reviewer #2 指出 ISP 假题：Hub.sendWithBroadcast 实际同时持有三个字段（`sender + qctrl + qstats`），三连传 `*dispatch.MessageQueue` —— ISP 没真发挥，反而增加学习成本（3 个名词）和 mock 难度（mock 必须实现 3 接口）。

#### v0.3 方案：单接口 + cross-method 契约

> **v0.4 实测修订**：2026-05-25 grep `h\.queue\.[A-Z]` 在所有 wshub*.go 中实际只出现 5 个方法（Enqueue / DoneOrDrain / Discard / Mode / CollectDelay）；`ShouldNotify` 仅在 `dispatch` 包内部被调用，Hub 不需要。接口面按实测裁剪到 5 方法，避免"interface 写得宽但实测无人用"的 ISP 噪音。
>
> **签名按实际 dispatch.MessageQueue 方法集对齐**（不是设计稿之前粗估的 `Enqueue(key, text, payload any)` 那种简化签名）：

```go
// internal/wshub/types.go

// MessageEnqueuer is the *dispatch.MessageQueue subset Hub depends on.
//
// satisfies-by: *dispatch.MessageQueue (internal/dispatch/msgqueue.go)
//
// Cross-method contract: see docs/design/server-consumer-contracts.md#message-queue
type MessageEnqueuer interface {
    Enqueue(key string, msg dispatch.QueuedMsg) (isOwner, enqueued, shouldInterrupt bool, gen uint64)
    DoneOrDrain(key string, gen uint64) []dispatch.QueuedMsg
    Discard(key string)
    Mode() dispatch.QueueMode
    CollectDelay() time.Duration
}
```

> **未来扩展**：如果某次 phase 改动让 Hub 直接调用 `ShouldNotify` / `TryAcquire` / `Release` 等，接口加方法 + 实现侧编译期 `var _` 绑定保证不漂移。

dispatch 侧单条绑定：

```go
// internal/dispatch/msgqueue.go
var _ wshub.MessageEnqueuer = (*MessageQueue)(nil)
```

#### 测试 mock 单独提供子接口（仅测试可见）

```go
// internal/wshub/testhelper/enqueuer.go (build tag: test)
type EnqueuerMock interface {
    Enqueue(key, text string, payload any) (queued bool, depth int)
}
// fakeEnqueuer 用 embedded *dispatch.MessageQueue + override Enqueue
```

生产代码不接触 testhelper 包；mock 写起来照样省事。这样 ISP 收益（mock 简单）保留，生产代码（注入一个字段不是三个）干净。

---

## 五、Hub 单 struct + 方法分文件（**v0.1 三子 struct 方案取消**）

### 决策依据

v0.1 提出 `BroadcastDispatcher / SubscriberRegistry / SendCoordinator` 三子 struct + `hubAccess` 反向访问接口。三路 reviewer 的核心反对意见：

1. **共享 `clients` map 的锁方案"运行时观察"** = 把死锁风险延期，不是设计
2. **三 struct + 反向接口 = Java 化**（idiom reviewer），Go 更倾向 struct composition + 方法分组
3. **没解决根本耦合**，只是把 Hub 字段切成 3 块塞回 Hub 壳里

### v0.2 方案

**Hub 保持单个 struct**，但：

1. **字段按职责物理分组**，使用注释分块：

```go
type Hub struct {
    // ── lifecycle ──────────────────────────────────────
    mu     sync.RWMutex
    ctx    context.Context
    cancel context.CancelFunc

    // ── subscriber registry (RW: hub_subscribe.go) ─────
    clients          map[*wsClient]struct{}
    connCount        atomic.Int64
    clientWG         sync.WaitGroup
    wsAuthLimiter    func(ip string) bool
    wsUpgradeLimiter func(ip string) bool
    upgrader         websocket.Upgrader
    dashTokenHash    [32]byte
    cookieMAC        string

    // ── broadcast (RW: hub_broadcast.go) ───────────────
    debounceMu     sync.Mutex
    debounceTimer  *time.Timer
    debounceFirst  time.Time
    debounceClosed bool

    // ── send / queue (RW: hub_send.go) ─────────────────
    sender       MessageEnqueuer
    qctrl        MessageQueueControl
    qstats       MessageQueueStats
    sendWG       sync.WaitGroup
    sendTrackMu  sync.Mutex
    sendClosed   bool
    droppedTotal atomic.Int64

    // ── shared dependencies (read-only after ctor) ─────
    router       HubRouter
    agents       map[string]session.AgentOpts
    agentCmds    map[string]string
    dashToken    string                            // v0.4 补：与 dashTokenHash 对照用
    nodes        map[string]node.Conn
    nodesMu      *sync.RWMutex
    projectMgr   *project.Manager
    resolver     *session.KeyResolver
    scheduler    cronHubOps
    scratchPool  scratchOps
    uploadStore  uploadOps
    guard        *session.Guard
    allowedRoot  string
    trustedProxy bool                              // v0.4 补：HTTP upgrade IP 提取信任配置

    // ── agent tailer subsystem ─────────────────────────
    tailers        *tailerRegistry
    wiredLinkersMu sync.Mutex
    wiredLinkers   map[agentlink.AgentLinker]struct{}
}
```

2. **方法严格按文件分组**：

```
hub.go            Hub struct + NewHub + Shutdown
hub_subscribe.go  Register/Unregister/HandleUpgrade/AuthGate（仅访问 subscriber 字段块）
hub_broadcast.go  BroadcastSessionsUpdate/scheduleDebounce（仅访问 broadcast 字段块）
hub_send.go       SendWithBroadcast/TrackSend/sessionSendLegacy（仅访问 send 字段块）
hub_eventpush.go  事件推送循环
hub_agent.go      agent_subscribe/agent_unsubscribe（含 tailer 协调）
```

每个文件头 godoc 声明（**v0.3 增补**：明示跨块只读豁免）：

```go
// Package wshub: subscriber registry methods.
//
// Field-block contract:
//   WRITES:  subscriber-registry block (clients/connCount/clientWG/
//            wsAuth*/upgrader/dashTokenHash/cookieMAC)
//   READS:   shared-deps block (read-only after ctor)
//   READS-ALSO: send block (`sendClosed` only, for Shutdown coordination)
//
// AST linter rule 3 (file_block.go) parses this header and verifies
// each method's field access matches. Adding a field access not listed
// here fails CI.
```

**v0.3 跨块调用豁免**：现实中 hub_subscribe.go 关闭 client 时确实需要 drain pending sends（跨 send 块）。godoc 头允许声明 `READS-ALSO` 为只读跨块访问；linter 据此放行。**写跨块永远禁止**——必须把方法搬到对应文件，或拆成两个方法各文件一份。

### 锁方案钉死

**保持 `Hub.mu` 单锁**保护 `clients` map（subscriber 与 broadcast 共用读路径）。理由：

- Phase 4 是位置搬迁，**不动并发模型** —— 减少风险面
- `clients` 操作的 hot path 是 broadcast 的读（90%），Register/Unregister 写很少
- v0.1 提到的"锁分离"留作 Phase 6 优化（如果运行时验证显示有竞争）

**Phase 4 不变更锁结构** = 不引入新死锁路径。这条比 v0.1 "运行时观察" 更负责。

---

## 六、Phase 切分（**v0.3：方案 B 顺序 / 11 个 PR / 9-10 周**）

### 6.0 PR 切分原则

- ≤ 1500 行 / PR（reviewer 友好阈值）
- 单 PR 自洽：build / test / vet / gofmt / routes_snapshot 全绿
- 序列依赖最小化：3a/3b/3c 之间无依赖；3e 依赖 3a-3d 全部完成（共享 dashboard 子包入口）

### 6.1 Phase 表

| Phase | 范围 | 行数估计 | 破坏面 | 依赖 |
|---|---|---|---|---|
| **0** | 准备：godoc + interface 定义 + AST linter + routes_snapshot_test.go + 文件大小 lint + 包契约文档 | -50 / +500 | 极低 | — |
| **1** | 抽 `internal/dashboard/cron/`（含 cron / cron-transcript / 21 个 cron-related view types） | -2330 / +2400 | 中 | Phase 0 |
| **2** | 抽 `internal/dashboard/project/`（project_api + project_files） | -1690 / +1750 | 中 | Phase 0 |
| **3a** | 抽 `internal/dashboard/auth/`（dashboard_auth + csrf） | ~600 | 低 | Phase 0 |
| **3b** | 抽 `internal/dashboard/discovery/`（dashboard_discovered + takeover） | ~450 | 低 | Phase 0 |
| **3c** | 抽 `internal/dashboard/ext/scratch + memory` | ~700 | 低 | Phase 0 |
| **3d** | 抽 `internal/dashboard/ext/agent_events + cli + transcribe + system` | ~700 | 低 | Phase 0 |
| **3e** | 抽 `internal/dashboard/session/{list,events,interrupt,label}` | ~1000 | 中 | 3a-3d |
| **3f** | 抽 `internal/dashboard/session/{send,upload,attachment,agent_tailer,upload_store}` | ~1500 | **高**（含 send 路径） | 3e |
| **4** | 抽 `internal/wshub/` 整包（hub*.go + wsclient + agent_tailer 跟随） | -3500 / +3550 | **高** | **Phase 0**（推荐方案 B：Phase 4 先于 Phase 1，让 dashboard 子包从一开始就 import wshub 包）|
| **5** | Server struct 字段瘦身到 ≤ 12 + Hub 字段分组定型 + 删 god setter | -300 / +400 | 中 | Phase 4 |

总 PR 数：**11 个**（Phase 0 + 1 + 2 + 3a + 3b + 3c + 3d + 3e + 3f + 4 + 5）。

按团队 1-2 人 / 1 PR/2 天节奏，**约 9-10 周完成**（含 review + merge + 7 天观察期）— 比 v0.2 估算的 6-8 周更现实，见 baseline.md §8。

#### 合并顺序

v0.2 写"Phase 4 可与 1-3 并行" — 措辞误。Phase 1-3 引用 `*server.Hub`，Phase 4 把 Hub 搬到 wshub 包，merge 时必然冲突。**v0.3 改写**：

- ❌ **方案 A（v0.2 顺序）**：1 → 2 → 3a-3f → 4 → 5。Phase 1-3 期间引用 `*server.Hub`，Phase 4 时全部改 import → 大量 mechanical churn
- ✅ **方案 B（v0.3 推荐）**：**Phase 0 → Phase 4 → Phase 1 → Phase 2 → Phase 3a-3f → Phase 5**。Phase 4 先把 Hub 搬到 wshub 包，dashboard 子包从一开始就 import 正确路径

方案 B 的代价是 Phase 4 风险（最大刀，3500 行）前移；收益是后续 6 个 PR 的 import 路径稳定，不再因 wshub 抽包二次改动。

"代码 review 可并行" 与 "merge 必须串行" 是两件事 — 6 个 dashboard 子包（Phase 1/2/3a-3f）的代码 **review 阶段** 可重叠（不同 reviewer 不同 PR），但 merge 必须严格按方案 B 串行。

### 6.2 Phase 0：准备（独立 PR，**1 周**）

包含 6 个独立交付物：

#### 0.1 字段读写注解
- `Server struct` 28 字段加 `// 读写: <files>` 注释
- `Hub struct` 28 字段加 `// 读写: <files>` 注释（按 §五字段分组组织）

#### 0.2 接口定义
- `internal/wshub/types.go`：`MessageEnqueuer` / `MessageQueueControl` / `MessageQueueStats`（先放 server 包等 Phase 4 一起搬）
- `dispatch.MessageQueue` 三连 `var _` 绑定

#### 0.3 routes_snapshot_test.go（修订版）
v0.2 改 snapshot 内容为 `method + path + handlerTypeName`：

```go
// internal/server/routes_snapshot_test.go
type routeEntry struct {
    Method      string `json:"method"`
    Path        string `json:"path"`
    HandlerType string `json:"handler_type"` // "*dashboardcron.Handlers" 等，type 而非 method
}

func TestRoutesSnapshot(t *testing.T) {
    s := newTestServer(t)
    routes := extractRoutes(s.mux)
    sort.Slice(routes, func(i, j int) bool { ... })
    got, _ := json.MarshalIndent(routes, "", "  ")
    golden := readFile(t, "testdata/routes.golden.json")
    if !bytes.Equal(got, golden) {
        t.Fatalf("routes drifted; want\n%s\ngot\n%s", golden, got)
    }
}
```

handler 内部函数名重构不破 snapshot；只有 type 改名或路由路径改变才破。

#### 0.4 AST-based linter（**v0.3 重写：阶段化 gate + baseline 豁免**）

⚠️ **v0.2 致命缺陷**：6 个文件已超 800 行硬上限，linter Phase 0 启用 `fail` 模式立刻全红，Phase 0 PR 无法独立合入。v0.3 改阶段化方案：

| 阶段 | linter 模式 | 文件大小规则 |
|---|---|---|
| Phase 0 | **warn**（CI 输出但不卡 PR） | 现有超线文件冻结到 `tools/lint-server-handlers/exemptions.yaml` baseline 清单；新增文件走硬上限 |
| Phase 1-4 | **warn**（同上） | 每 phase merge 后自动从豁免清单删除已不超线的文件；新增文件仍走硬上限 |
| Phase 5 完工后 | **fail** | 豁免清单必须为空才算 Phase 5 验收通过 |

linter 实现位置 `tools/lint-server-handlers/`：

```
main.go              入口（CLI）+ SARIF 输出
rules/handle_decl.go 规则 1：禁止新增 func (s *Server) handle* （新文件 / 新函数）
rules/file_size.go   规则 2：文件大小超限（按 internal/server/ 500 / dashboard/* 800）
rules/field_block.go 规则 3：hub_*.go 方法访问字段对账（v0.3 新增，见 §五）
rules/iface_match.go 规则 4：实现侧反向注释 vs 消费侧本地接口对账（v0.3 新增，见 §四.1）
exemptions.yaml      Phase 0 baseline 冻结的超线文件清单（每个条目带 until_phase）
```

`exemptions.yaml` 模板（**v0.4 N9 整改：until_phase 显式标到位**）：

```yaml
file_size:
  - path: internal/server/dashboard_session.go
    current: 1514
    limit: 800
    until_phase: 3e        # Phase 3e 抽完 dashboard/session/{list,events,interrupt,label}
                           # 后此条目应被该 PR 删除
  - path: internal/server/dashboard_cron.go
    current: 1418
    limit: 800
    until_phase: 1
  - path: internal/server/server.go
    current: 1309
    limit: 800
    until_phase: 5
  - path: internal/server/project_files.go
    current: 1302
    limit: 800
    until_phase: 2
  - path: internal/server/dashboard_send.go
    current: 1214
    limit: 800
    until_phase: 3f
  - path: internal/server/dashboard_cron_transcript.go
    current: 909
    limit: 800
    until_phase: 1
  # 总 6 个 > 800 文件
```

每个 phase 的 PR 必须**减少**或**清空**对应豁免条目。豁免清单**只在 Phase 0 一次性建立**，后续 phase 不允许新增条目。

集成进 `make lint` + `.github/workflows/lint.yml`，CI 走 `mode=warn`；新增文件硬卡 fail。

**Phase 0 / Phase 4 阶段化交付（v0.4 N8 整改）**：

| 规则 | Phase 0 | Phase 4（merge 前） | Phase 5 |
|---|---|---|---|
| Rule 1 handle_decl | ✅ 必交付 | — | mode=fail |
| Rule 2 file_size | ✅ 必交付 + exemptions baseline | — | exemptions 必须空 |
| Rule 3 field_block | 框架（伪实现 OK） | ✅ **必完整实现**（§五字段块约束 = Phase 4 实现契约，没有 rule 3 强制就靠口审） | mode=fail |
| Rule 4 iface_match | 框架（伪实现 OK） | ✅ **必完整实现**（§四.1 反向注释 = Phase 1+ 实现契约） | mode=fail |

> **v0.3 错点**：rules 3-4 标 "Phase 4-5 期间补全"，但 §五 / §四.1 已经把它们当 Phase 1+ 的实现契约。v0.4 改为：rule 3 必须在 **Phase 4 PR merge 前**补完；rule 4 必须在 **Phase 1 PR merge 前**补完。Phase 0 交付的是骨架（能跑、能 SARIF 输出），实现完整规则在对应 phase 前完成。

#### 0.5 三份配套文档（**v0.4 整合 N4：Phase 0 必交付**）

**0.5.1 `docs/design/server-packages-contract.md`** — 包契约
```
internal/server/                 入口；只放 routes/middleware/debug；≤ 5000 行 / ≤ 15 文件
internal/dashboard/<domain>/     handler 子包；按 §四.1 双轨规则；≤ 1500 行/包
internal/wshub/                  WebSocket hub；保单 struct；字段分组 + 方法分文件
```

每个 PR 必须在 description 引用此文档相关条款。

**0.5.2 `docs/design/server-consumer-contracts.md`** — 跨包接口契约
对每个跨包接口（`MessageEnqueuer` / `broadcaster` / 子包私有 view 接口）写：
- 哪些 consumer 包持有
- 跨方法时序契约（如 `Enqueue` 返回 `isOwner=true` 后必须 `DoneOrDrain`）
- 哪些方法可演化（加参数 / 加方法）/ 哪些方法 stable
- 实现侧 godoc 反向引用此文档锚点

linter rule 4（Phase 4 前补全）扫 `// satisfies:` 注释比对此文档。

**0.5.3 `docs/ops/phase4-smoke-test.md`** — 一致冒烟 checklist
取代"55 次手工冒烟"非确定性。模板：
```markdown
## Smoke Test Checklist (Phase X PR)
### Dashboard
- [ ] 登录（200 + cookie 设置成功）— 截图保存到 `~/.naozhi/smoke/<phase>/login.png`
- [ ] sessions 列表加载 < 1s — 网络面板时长截图
- [ ] /cron 面板 CRUD 一轮 — 创建/暂停/恢复/触发 截图
- [ ] WebSocket 订阅事件流（/ws subscribe → 看 send_ack）— 控制台截图
- [ ] interrupt + 关闭 session — 不报错截图

### IM
- [ ] 飞书发一条消息 → 收到回复
- [ ] /cron list 命令 → 收到 cron 列表
- [ ] /project xxx 命令 → workspace 切换成功

PR description 必须附 `~/.naozhi/smoke/<phase>/` 下截图链接（gist 或 PR comment 上传）。
```

#### 0.6 baseline 数据
跑一次记录到 commit message：

```bash
go test -race -count=1 ./... 2>&1 | tail -3        # 耗时基线
wc -l internal/server/*.go | tail -1                # 17143 行基线
go test -race -count=1 ./... | grep -c "PASS"       # pass 数基线
```

后续每个 phase merge 前必须证明：耗时增加 < 10%、pass 数不减。

#### 0.7 验收
- [ ] `go build ./...`
- [ ] `go test -race -count=1 ./...` 全绿
- [ ] AST linter 在 master 上跑无误报
- [ ] routes_snapshot golden 与当前 mux 完全对齐

### 6.3 Phase 1-2：抽 cron / project

**Commit 策略**（沿用 [router-split-design](router-split-design.md) §commit 策略行数分档）：

- > 500 行 → **强烈推荐双 commit**（commit a 纯机械迁移、commit b godoc/gofmt polish）
- 300-500 行 → 双 commit 可选
- < 300 行 → 单 commit 一次到位

按本设计 Phase 1（~2400）/ Phase 2（~1750）/ Phase 4（~3550）必须双 commit；Phase 3a-3f 视具体行数决定。

每个 phase 必须满足：

- PR description 给跨包字段引用减少数（≥ 5）
- `routes_snapshot_test.go` 通过（route 路径不变）
- `go test -race -count=2 ./internal/dashboard/<domain>/...`
- 手工冒烟（详见 §九）

### 6.4 Phase 3a-3f：抽剩 9 个 handler-group

每 PR 单独 review；3a-3d 互不依赖可并发推进；3e/3f 串行。

### 6.5 Phase 4：抽 `internal/wshub/`

整包搬迁；保持 §五的"单 struct + 方法分文件"结构。**特别关注**：

- Hub Shutdown 协调链路：`ctx cancel → debounceClosed = true → drain sendWG → close clients → wait clientWG`
- `hub_concurrency_test.go`（**Phase 4 必须新增**）：
  - 模拟 broadcast 中触发 Shutdown
  - 模拟 Register 与 Shutdown 并发
  - 跑 `-race -count=100`

### 6.6 Phase 5：Server 字段瘦身（47 → 12）

#### 47 个字段去向（v0.4 重新逐字段实地对账）

> **v0.3 错点**：把 `version / cookieMAC / wsAuthLimiter / wsUpgradeLimiter` 当 Server 字段——实际 cookieMAC/wsAuth* 在 Hub，versionTag 在 SessionHandlers。"删除 5"实际写了 6 项（noOutputTimeout/totalTimeout 重复计入）。v0.4 按 baseline.md §2 实测重对账：

##### 保留 12 个（HTTP 入口 5 + 核心依赖 4 + 多节点 3）

```
HTTP 入口   addr / mux / startedAt / onReady / appCtx
核心依赖    router / scheduler / hub / projectMgr
多节点      nodes / nodesMu / reverseNodeServer
```

##### 搬走 35 个

| 去向 | 个数 | 字段 |
|---|---|---|
| `routes.go` 局部变量（构造期，注册完丢弃） | 13 | 12 handler-group + nodeAccess |
| `NewHub` Options 注入 | 9 | dedup / sessionGuard / msgQueue / agents / agentCommands / dashboardToken / allowedRoot / noOutputTimeout / totalTimeout |
| `dashboard/*` 子包持有 | 5 | claudeDir（discovery）/ workspaceName（公共）/ discoveryCache（discovery）/ scratchPool（ext）/ sysessionMgr（ext） |
| server 包内重组到独立文件 | 3 | debugMode（→ server/debug.go）/ resolver（→ routes.go 局部）/ nodeCache（→ server/nodecache.go） |
| `metrics` 包 | 2 | watchdogNoOutputKills / watchdogTotalKills |
| **待评估删除**（v0.4 N7 改"待评估"，Phase 5 写之前必须 grep 实装依赖 + 写小设计文档） | 3 | platforms（疑似 routes 注册期局部）/ backendTag（dispatch.BackendTag() 派生路径）/ knownNodes（合 nodes map） |

**加法核对**：13 + 9 + 5 + 3 + 2 + 3 = **35** ✓ ；保留 12 + 搬走 35 = **47** ✓

##### Hub 字段不动

baseline §3 实测的 Hub 37 字段（含 cookieMAC / wsAuthLimiter / wsUpgradeLimiter）**不在 47 个 Server 字段范围**。Phase 5 不动 Hub 字段；Hub 的 setter 删除见下节。

#### 删 god setter

v0.1 提到的 `SetScheduler / SetUploadStore / SetScratchPool` 三个 setter 实际位置在 **`internal/server/wshub.go`** 的 `Hub` 上（不是 Server，v0.2 措辞误）。Phase 5 改 `NewHub Options` 一次性注入，删除三个 setter。

#### 验证

```bash
# Phase 5 完工 gate
awk '/^type Server struct/,/^}$/' internal/server/server.go | grep -cE '^\s+[a-zA-Z_]+ '   # 必须 ≤ 12
grep -E "func \(h \*Hub\) Set" internal/server/wshub.go | wc -l                              # 必须 == 0
```

---

## 七、回滚预案（**v0.2 新增**）

### 7.1 单 Phase 回滚

每个 Phase 合并时打 git tag（**v0.3 改 prefix 防与发版冲突**——线上发版 tag 是 `v0.0.20` 形式）：

```
server-split-baseline          Phase 0 之前
server-split-phase0            Phase 0 完成
server-split-phase4            Phase 4 完成（按方案 B 顺序）
server-split-phase1
server-split-phase2
server-split-phase3a ... 3f
server-split-final             Phase 5 完成
```

```bash
# 单 phase 回滚（出问题在 N 时）
git revert v0.1-phase{N-1}..v0.1-phase{N}

# 或：直接 reset 到上个 tag（保留 master 历史）
git checkout master
git reset --hard v0.1-phase{N-1}     # 仅在紧急 incident 用
```

### 7.2 部分回滚的边界

**不支持**单独回滚 Phase 3 中的某个子 PR（如 3c 已经 merged，3d 未发生）。原因：
- 3c 引入了 `internal/dashboard/ext` 子包入口，3d 复用同入口
- 部分回滚会留悬空 import

**整改**：3a/3b/3c/3d 必须**全部到位再做 3e**。3e 出问题时，回滚 3e 是干净的（独立子包 session/）。

### 7.3 7 天观察期 SLA（**v0.3 新增**）

每个 Phase merge 到 master + 部署到生产后，**强制 7 天观察期**：

- 期间不接下个 Phase 的 PR merge（PR 可以提、可以 review，但不合）
- 7 天内发现问题 → 单 phase revert 不冲突（因为下个 phase 还没合）
- 7 天后再发现问题 → 必须 forward fix（写新代码修，不 revert）

**例外**：连续 phase 之间有强 import 依赖时（如 Phase 1 → 2 都属于 `internal/dashboard/` 拆分），观察期可以重叠（PR 都已 merge 的情况下并行观察）。

观察期内监控指标：

- `/health` 200 率 ≥ 99.99%
- WebSocket 连接错误率 ≤ baseline + 0.1%
- dashboard 页加载 P95 ≤ baseline × 1.1
- `cli_session_spawn_total{result="error"}` rate 不增

任一指标超线 → 立即 revert + 写 incident report。

### 7.4 跨版本兼容

- **本次拆分不涉及外部 API 变更**（路由路径不变 / WS 协议字段不变）
- 旧客户端（飞书/dashboard.html）连新 binary 100% 兼容
- 新 binary 连旧持久化文件（sessions.json / events/）100% 兼容
- **不需要 feature flag**

---

## 八、灰度部署 runbook（**v0.2 新增**）

### 8.1 每个 Phase 的发布流程

参照 [naozhi-deploy-skill](../ops/naozhi-deploy-skill.md)：

1. **本地验证**：`make build && make test-race`
2. **Staging 部署**：build → 部署到 staging EC2 → 跑 24h
3. **Canary**：部署到 1 台测试机（10% 流量）→ 观察 1h
4. **全量**：合入 main + tag → `naozhi upgrade` 到所有节点
5. **观察期**：48h 内不接新 phase；观察 `/health.eventlog.dropped_total` / `/health.attachment_tracker.dropped_total` / WS connection error rate

### 8.2 Phase 4 (wshub 抽包) 特殊说明

WebSocket Hub 抽包过程中可能短暂断开 dashboard WS 连接。发布前：

- 邮件 / 群通知运维 + 主用户 "Phase 4 发布期间 dashboard 可能闪断 1-2 秒"
- 选凌晨低峰窗口（UTC 18:00 = 北京 02:00）
- 发布后 30 分钟内观察 dashboard 重连日志，确认无 client 卡死

### 8.3 4 小时冻结窗口

参照 router-split RFC v4 的成熟做法：

- **Phase 1 / 4 / 5（大刀）**需要冻结窗口；
- 推前在群里通知："今晚 X 点起 4 小时窗口，期间 server 包 / wshub 不要新提 PR；遇到紧急 fix 喊我"
- 4 小时内推完合入；超时则暂停于最近 phase 完成处，第二天续推
- **Phase 2 / 3a-3f 不需要冻结**（每 PR 1500 行内，正常 review）

---

## 九、CI / 验收 / 防膨胀（**v0.2 整合**）

### 9.1 每 Phase 验收清单

```
- [ ] go build ./...
- [ ] go test -race -count=1 ./... 全绿
- [ ] go vet ./...
- [ ] gofmt -l internal/ 输出空
- [ ] AST linter 通过
- [ ] routes_snapshot_test.go 通过（除非显式更新 golden）
- [ ] PR description 给出跨包字段引用减少数 ≥ 5
- [ ] 手工冒烟（按 [docs/ops/phase4-smoke-test.md](../ops/phase4-smoke-test.md) 一致 checklist 跑；PR 附截图链接）
```

Phase 4 / 5 增加：

```
- [ ] hub_concurrency_test.go -race -count=100 通过
- [ ] go test -race -count=2 ./internal/wshub/...
```

最终 Phase 5 后（验证总目标达成）：

```
- [ ] wc -l internal/server/*.go | tail -1   ≤ 5000
- [ ] Server 字段数 ≤ 12（脚本验证）
- [ ] Hub 字段数 ≤ 30 且按 §五字段块分组组织（脚本验证）
- [ ] grep -r "*session.Router" internal/dashboard/  返回 0
       （子包不应直接持有 *Router，应是接口）
```

### 9.2 长效防膨胀（CI 卡口）

在 `.github/workflows/lint.yml` 添加：

```yaml
- name: AST linter (server packages)
  run: go run ./tools/lint-server-handlers/

- name: File size budget
  run: |
    err=0
    for f in internal/server/*.go; do
      lines=$(wc -l < "$f")
      [ "$lines" -gt 500 ] && echo "$f: $lines > 500" && err=1
    done
    for f in internal/dashboard/*/*.go; do
      lines=$(wc -l < "$f")
      [ "$lines" -gt 800 ] && echo "$f: $lines > 800" && err=1
    done
    exit $err
```

超线 PR 必须改设计或拆 PR，**不允许 Phase 1+ 新增豁免**（豁免清单只在 Phase 0 一次性建立，其后只能减不能增）。Phase 5 验收 gate：豁免清单空。

### 9.3 CI 时间预算

baseline `go test -race -count=1 ./...` ≈ 300s。Phase 4 完成后预期：

- 包数从 27 → 35（+8 dashboard 子包 + wshub）
- 每包独立编译：CI 串行 race test ≤ 450s（GitHub runner 600s 超时，留余量）
- 若超 450s：拆 `make test-fast`（去 race）+ `make test-full`（含 race）两层；CI 默认跑 fast，nightly 跑 full

---

## 十、风险与缓解（v0.2 重排）

| 风险 | 概率 | 缓解 |
|---|---|---|
| Hub Shutdown 在 wshub 抽包后死锁 | 高 | Phase 4 必加 `hub_concurrency_test.go -race -count=100`；Drain(ctx) 5s 超时 |
| handler 隐式依赖 Server 字段被遗漏 | 高 | Phase 0 字段读写注解；Deps 接口化强制每个引用进 Deps；编译期错误立刻暴露 |
| 50 路由路径漂移 | 低 | `routes_snapshot_test.go` golden gate |
| 跨 phase 合并冲突 | 高 | 每 PR ≤ 1500 行 / 1-2 天合一个；冻结期不接 dashboard 新功能 PR |
| Phase 4 / 5 生产 dashboard 闪断 | 中 | §八.2 凌晨低峰发布 + 主动通知 + 重连观察 |
| 抽完 2 年再膨胀 | 中（历史已发生） | AST linter + 文件大小硬上限 + 包契约文档（§九.2）|
| 单 phase 出问题难独立回滚 | 中 | tag 策略 + 3a-3d 独立可回滚 / 3e-3f 串行依赖说明（§七）|
| reviewer 节奏拖慢整体 | 中 | 11 个 PR 每个 ≤ 1500 行，单 PR review ≤ 30 分钟；3a-3d 可并发 review |
| CI 时间超 600s | 低 | §九.3 fast/full 双轨方案 |

---

## 十一、成本-收益论证（v0.2 新增 / **v0.3 钉 ROI gate**）

> 工程 reviewer 反馈：v0.1 没算清楚机会成本。
> v0.3 reviewer 反馈：v0.2 数字与 baseline 倒装（先承诺再采数据）。本节为**承诺范围**；Phase 0 后必须基于真实 baseline 重新评估。

### 成本

- **工时**：1-2 人 × 6-8 周 = ~60-120 人天
- **机会成本**：拆分窗口期 dashboard / cron 两热区接新功能必须接受合并冲突或排期排到 Phase 5 之后。**保守估计延期 4-8 周**新功能上线
- **CI 时间**：从 ~300s → ~450s（详见 §九.3），单次 PR 等待时间 +50%
- **学习成本**：6 个 dashboard 子包 + wshub 包，新人加入熟悉成本 +1 天

### 收益（量化）

| 项 | 现状 | Phase 5 后 | 量化收益 |
|---|---|---|---|
| server 包大小 | 17143 行 | ≤ 5000 行 | -71% |
| 单文件 1000+ 行数量 | 5 个 | 0 个 | 解 god file |
| Server struct 字段 | 28+ | ≤ 12 | -57% 跨包字段引用 |
| 跨包小接口数量 | ~3 个（HubRouter / cronHubOps / 等） | ~12 个（每 dashboard 子包 2-3 个） | 测试 mock 难度 -60% |
| 并行开发上限 | 1（任意 dashboard PR 撞同一文件） | 6+（6 个独立子包 + wshub） | 并行度 6× |
| dashboard 新功能 PR 平均冲突时间 | 30-90 min（实测过去 3 个月） | < 10 min | 月省 ~5-10 工程小时 |
| AST linter 防膨胀 | 无 | 强制 | 阻断"再膨胀 5×"的历史复演 |

### 何时这个 trade-off 反转

- 如果未来 6 个月内 dashboard 不接新功能（项目转向纯后端） → 收益打折，应推迟 Phase 4
- 如果团队从 1-2 人扩到 5+ 人 → 并行开发收益翻倍，应提前 Phase 4

### Phase 0 后 ROI gate（**v0.4 改时序解锁**）

> **v0.3 时序冲突**：方案 B 要 Phase 4 紧跟 Phase 0；ROI gate 要 14 天后才决策——两者不能同时成立。
>
> **v0.4 解法**：开发可重叠（Phase 4 PR 可以**review 不 merge**），merge 必须等 ROI gate 通过。

#### 时间轴

```
T+0  Phase 0 PR merge
T+0~T+14d  baseline 采集（同时 Phase 4 PR 可以开 + review）
T+14d  ROI gate 决策点
       通过 → Phase 4 merge → Phase 1 准备 → ...
       不通过 → Phase 4 PR close（保留 branch 备查）；Phase 1-5 进 NEEDS-DESIGN
```

#### 决策 gate（baseline.md §4 / §9 回填实测后跑）

- ✅ **dashboard 月均跨文件冲突时间 ≥ 15 分钟** AND **月均并行 PR 数 ≥ 3** → 启动 Phase 4 merge → Phase 1
- ⚠️ **冲突 < 15 分钟** OR **并行 PR < 3** → **关闭 Phase 4 PR，Phase 1-5 推迟**；Phase 0 的 lint gate（防新文件膨胀）保留作为长期治理
- ❌ **冲突 < 5 分钟** AND **并行 PR < 2** → 取消整个 Phase 1-5；说明 ROI 假设根本不成立，Phase 0 投入算"防膨胀工具"独立交付

#### "Phase 0 是否值得"反推

如果 ROI gate 不达标会 close Phase 4 PR — 那 Phase 0 的投入是不是浪费？

**不是**：Phase 0 4 项交付物中，**至少 2 项独立有价值**（即便 Phase 1-5 全推迟）：

1. **AST linter rules 1-2**（防新文件膨胀）— 独立有价值，永远能用
2. **`docs/design/server-packages-contract.md`** 包契约文档 — 即便不拆分，也是 server 包不再膨胀的认知底座
3. ⚠ rules 3-4 + cross-method 契约文档 — 这两项依赖 Phase 4-5；ROI 不达标时不写
4. ⚠ routes_snapshot — 依赖后续 phase 才有意义；不达标时维持但不主动维护

所以 Phase 0 的最小可独立交付 = rules 1-2 + 包契约文档 + 字段读写注解 + baseline 采集机制。这 4 项即便 Phase 1-5 全黄，也值得做。

#### 写进 PR description

Phase 0 PR description 必须含两条：

```
## ROI gate 触发条件
- 本 PR merge 后 14 天内，跑 baseline.md §4 §9 数据采集脚本
- 14 天后开 followup issue「Phase 4 ROI gate 决策」附实测数据
- 不达标时本 PR 仍保留（rules 1-2 + 包契约 + 注解 + 采集机制是长期治理）

## 后续 PR 依赖
- Phase 4 PR 可在本 PR merge 后立即 review，但 merge 必须等 ROI gate 通过
```

### 替代方案对比

| 方案 | 成本 | 收益 | 评估 |
|---|---|---|---|
| **本设计**（11 PR / 6-8 周） | 中 | 高 | ✅ 推荐 |
| **不拆，加 lint 限单文件 800 行** | 极低 | 中（防新增膨胀但旧债不还） | 退而求其次 |
| **一次性大重构（1 PR 13000 行）** | 极高（review 不过来） | 高 | ❌ 拒绝 |
| **更激进拆分**（每路由一 handler 文件） | 高 | 低（路由表碎片化） | ❌ 拒绝 |

**结论**：本设计是 ROI 最优解，但**前提是项目仍在主动开发 dashboard 功能**。Phase 0 完成后再次确认开发节奏，决定是否启动 Phase 1。

---

## 十二、不解决的问题（明示遗留）

1. **dashboard handler 与业务逻辑仍混在一起**：本设计只搬位置不重构。若将来要做 i18n / 错误统一 envelope（R247-ARCH-2/3），另起 PR。
2. **Hub 锁分离**留作 Phase 6 优化：本次保持 `Hub.mu` 单锁不变。仅当运行时 profile 显示 `clients` 锁竞争 > 阈值时再拆。
3. **`scratchPool` 留 Server**：因 Server.Shutdown 要 stop sweeper；移到 Hub 会让 wshub 包多依赖一个 `*session.ScratchPool` 类型。
4. **`/health` 拆分**（R247-ARCH-1）和这次正交，建议 Phase 6 再做。
5. **dispatch / cron 二次清理**：dispatch 已稳定，本次不动。`cron.Scheduler` 60+ 方法的 god struct 留作另起 PR。

---

## 十三、评审历史与签字

| 版本 | 日期 | 主要修订 |
|---|---|---|
| v0.1 | 2026-05-25 | 初稿；5 phase；Hub 三子 struct；50 行 lint gate |
| v0.2 | 2026-05-25 | 三路 reviewer 反馈整合：Hub 改单 struct + 方法分文件；Deps 强制接口化 + 减耦合 KPI；MessageEnqueuer 拆 3；AST linter；routes_snapshot 改 type；Phase 3 切 6 PR；新增回滚 / 灰度 / CI 预算三章 |
| v0.3 | 2026-05-25 | v0.2 三路再 review：实测 baseline 替换粗估数字（Server 47 字段 / Hub 37 字段 / 17156 行）；linter 阶段化 gate + 豁免清单；Deps 双轨策略（高频具体 / 低频接口）+ cross-method 契约文档；MessageEnqueuer 收回单接口；Phase 5 字段去向表；setter 对象修正到 Hub；7 天观察期 SLA；ROI Phase 0 后 gate；tag prefix 改 server-split-* |
| v0.4 | 2026-05-25 | v0.3 第四轮 reviewer 抓出 6 项真错 + 4 项过乐观：47 字段去向表实地重对账（删除"version/cookieMAC/wsAuthLimiter/wsUpgradeLimiter"误归 Server 字段；删除项从 5 项重置为 3 项"待评估"+实付 2 项搬走）；§五 Hub 字段示例补 dashToken / trustedProxy；ROI gate 时序解锁（Phase 4 review 不 merge / 14d 后 merge）；三份配套文档纳入 Phase 0 deliverable；linter rule 3 提前到 Phase 4 前 / rule 4 提前到 Phase 1 前；exemptions.yaml until_phase 显式化；双轨硬名单（白 5 / 黑 3-4）取代"≤ 6 方法"模糊表述 |

### v0.2 整改追溯（按 reviewer 反馈映射）

| reviewer 反馈 | v0.2 修订位置 |
|---|---|
| 阻断 1：Hub 锁方案"运行时观察"不算设计 | §五：保单 struct + 单锁不变 + 字段分组；锁分离留 Phase 6 |
| 阻断 2：Deps 换汤不换药 | §四.1：强制接口化；§四.2：减耦合 KPI |
| 阻断 3：长效治理缺失 | §九.2：AST linter + 文件大小硬上限 + 包契约文档 |
| 阻断 4：Phase 3 单 PR 5650 行违反节奏 | §六：切 3a/3b/3c/3d/3e/3f 共 6 PR，每 ≤ 1500 行 |
| 阻断 5：回滚 / 灰度 / CI 预算缺失 | §七 / §八 / §九.3 三章 |
| 工程 reviewer：机会成本未算清 | §十一：成本-收益量化 + 替代方案对比 + 反转条件 |

### v0.4 整改追溯（按 v0.3 第 4 轮 reviewer 反馈映射）

| reviewer 反馈 | v0.4 修订位置 |
|---|---|
| N1：删除项 5 实写 6 / noOutputTimeout/totalTimeout 重复计入 | §六.6 重写字段去向表；删除项重置为 3"待评估"+实付 2 搬 NewHub Options |
| N2：§五 Hub 字段示例缺 dashToken / trustedProxy | §五代码块补这 2 字段；shared deps 块改 14 字段 |
| N3：ROI gate 时序与方案 B 冲突 | §十一新增"时间轴"小节：Phase 4 PR review 不 merge / 14d 后再 merge |
| N4：三份配套文档不存在但被引用 | §六.2.0.5 拆 0.5.1/0.5.2/0.5.3 三份文档纳入 Phase 0 deliverable |
| N5：cookieMAC/wsAuthLimiter/wsUpgradeLimiter 不在 Server 字段 | §六.6 字段去向表清除这 3 项；标"Hub 字段不动"附注 |
| N6：versionTag 字段名错位 | §六.6 删除"version"列入保留 12（version 在 SessionHandlers 不在 Server）|
| N7：删除 4 字段过乐观（backendTag/allowedRoot/reverseNodeServer/knownNodes）| §六.6 改 3 项"待评估"+ Phase 5 PR 写前必须 grep 实装依赖 + 写小设计文档 |
| N8：linter rules 3-4 工作量低估 | §六.2.0.4 阶段化交付表：rule 3 → Phase 4 前 / rule 4 → Phase 1 前 |
| N9：豁免清单 until_phase 不显式 | exemptions.yaml 模板加 until_phase 字段；6 个超线文件每个标到具体 phase |
| N10：双轨"≤ 6 方法"模糊判据 | §四.1.1 改硬名单：白 5（具体类型）/ 黑 3-4（私有接口）/ 中间地带 PR reviewer 决定 |

### v0.3 整改追溯（按 v0.2 reviewer 反馈映射）

| reviewer 反馈 | v0.3 修订位置 |
|---|---|
| H1：baseline 数字粗估错（Server 28+ 实际 47） | §一全节用 baseline.md 实测替换；§二目标改 47→12 |
| H2：linter 启用即全红的死锁 | §六.2.0.4 阶段化 mode (warn→fail)；豁免清单 yaml；rules 1-2 Phase 0 必交付 |
| H3：接口碎片 + 反向注释靠人维护 | §四.1 双轨策略（高频具体类型 / 低频接口）；§四.1.3 cross-method 契约文档；linter rule 4 自动验证反向注释 |
| H4：Phase 4 与 1-3 "并行" 措辞错 | §六.1 改为方案 B（Phase 4 先于 Phase 1）；review 可重叠 / merge 严格串行 |
| H5：Phase 5 setter 对象在 Hub 不在 Server | §六.6 整改 |
| H6：47→12 字段去向不明 | §六.6 字段去向表（保 12 / 搬 35）|
| H7：字段块跨方法调用问题 | §五 增 READS-ALSO 跨块只读豁免；写跨块仍禁止 |
| H8：回滚预案对运行时失败无解 | §七.3 7 天观察期 SLA 新增 |
| H9：手工冒烟 55 次非确定性 | §九.1 改为引用 phase4-smoke-test.md 一致 checklist |
| H10：ROI 数字 baseline 倒装 | §十一 改"承诺范围" + 新增 Phase 0 后 ROI gate |
| H11：tag prefix 与发版冲突 | §七.1 改 `server-split-*` prefix |
| H12：static/ 子包没人抽 | §三改 embed.FS 跟随 server 入口不单独抽 |
| H13：MessageEnqueuer 三接口 ISP 假题 | §四.4 收回单接口；mock 走 testhelper 子接口 |
| MessageEnqueuer 6 方法过宽 | §四.4：拆 3 接口（Enqueuer / Control / Stats） |
| routes_snapshot 函数名脆弱 | §六.2.0.3：改 handlerTypeName |
| 跨包接口 godoc 不可见 | §四.1：实现侧反向注释 |
| AST linter 取代 git grep | §六.2.0.4 |
| Hub 三子 struct Java 化 | §五：取消三 struct 方案 |

---

## 十四、参考

- [docs/design/server-split-design.md](server-split-design.md) — Phase 1-3 设计（已完成）
- [docs/design/router-split-design.md](router-split-design.md) — router 同款拆分方法论参考
- [docs/ops/naozhi-deploy-skill.md](../ops/naozhi-deploy-skill.md) — 部署 playbook
- [feedback_dev_workflow_skill](../../.claude/skills/dev-workflow) — 项目级开发流程
