# server 包拆分 Phase 4 设计稿

> **状态**：设计稿 v0.6.1（2026-05-28），已应用 v0.1/v0.2/v0.3/v0.4/v0.5/v0.6 共 11 轮独立 reviewer 反馈。**v0.6.1 是 v0.6 第 7 轮 review 的 9 项内部一致性修订**。**Phase 0 实施前等最后一次 sign-off**。
>
> **v0.6 摘要**：v0.5 自身 9 项不一致整改（Hub 字段在 §一/§二/§五/§九.1/§7.3 多处不同步、PR 数 11 vs 13 在 4 处不一致、§7.3 观察期算式仍用 11 phase 等）+ 同步 origin/master 实测数据（server 包从 17156→**21313** 行 / Hub 字段从 37→**47** / 超线文件从 6→**9** 个 / Phase 4 范围从 3550→**5198** 行不含测试）+ **§0 新增事实速查表**（防多处数字漂移的根治）。
>
> **v0.6.1 摘要**：v0.6 第 7 轮 review 9 项整改 — N1 baseline §3 笔误"8 块"→"7 块"；N2 §五"v0.5 新增块"误标删除；N3 §十一收益表加 800+/1000+ 行同步；**N4 exemptions.yaml limit 字段全部 800→500（critical：v0.6 模板 limit=800 与 §9.2 server 包 500 硬上限矛盾，会让 linter 误豁免）**；N5 §6.2.0.1 字段数措辞校准；N6 §7.3 加 Phase 5 → final 14 天观察期；N7 §6.0 行数例外改为 4b/4c（4a 不超线无例外）；N8 §9.1 引用 §6.5 减少重复；N9 §0 加 lint rule 6 备做承诺（Phase 0 跟随 RFC）。详见 §十三 v0.6.1 整改追溯。
>
> **配套数据**：[server-split-phase4-baseline.md](server-split-phase4-baseline.md) — 真实 baseline（v0.6 全部按 origin/master 实测重新采集）。
>
> **背景**：[server-split-design v1](server-split-design.md) 的 Phase 1-2 已完成（抽出 `internal/node` / `internal/dispatch`），Phase 3 完成（`sessionGuard` → `internal/session/guard.Guard`）。当时设计目标 server 包瘦身到 ~3117 行；**两年后实际膨胀到 21313 行 / 58 文件 / 51 路由**（v0.6 实测；v0.4 时是 17156 行 / 40 文件 / 50 路由，3 个月内又涨 24%），新增 cron-dashboard / project-files / scratch / memory / agent-events / cli-backends / upload-store / agent-tailer / sysession 等 8+ 板块。本设计是 Phase 4 的二次拆分。
>
> **本次拆分的真正任务**不仅是把代码搬出去，更是 **建立长效防膨胀机制**——避免两年后再次出现同款症状。

---

## 0. 事实速查表（**v0.6 新增**：防多处数字漂移）

> **v0.5 的痼疾**：同一事实在 §一 / §二 / §五 / §九.1 / §7.3 多处出现，单作者用 grep 同步必漏（v0.5 把 Hub 37→43 改后只同步了 4/9 处）。v0.6 钉死一份"事实速查表"，所有正文从此读。修改任一事实必须先改本表 + 全文搜索更新。
>
> 数据来源：[baseline](server-split-phase4-baseline.md) §1-§5 / §9（origin/master HEAD `44a10e8d` 2026-05-28 实测）。

| 维度 | v0.6 实测值 | v0.4 写过 | v0.5 写过 | 备注 |
|---|---|---|---|---|
| Server struct 字段 | **47** | 47 | 47 | 一致 |
| Hub struct 字段 | **47** | 37 | 43 | v0.6 同步 master：3 个月新增 4 字段（auth/subscriberCount/legacySendInvokes/debounceClosedFast）|
| Hub 字段块数 | **7** | 6 | 7 | 一致（lifecycle 3 / subscriber 10 / broadcast 6 / send 6 / shared 14 / tailer 3 / cache 5）|
| server 包行数（不含测试）| **21313** | 17156 | 17156 | v0.6 同步 master：3 个月涨 24% |
| server 包行数（含测试）| **53487** | 38836 | 38836 | v0.6 同步 master |
| server 包文件数（不含测试）| **58** | 40 | 40 | v0.6 同步 master |
| 超 800 行文件数 | **9** | 6 | 6 | v0.6 同步 master：新增 wshub.go(902) / dashboard.go(852) / agent_tailer.go(827) |
| 路由数（dashboard.go 内）| **51** | 50 | 50 | v0.6 同步 master：+1 条 |
| 路由数（server 包总）| **55** | — | — | v0.6 新增数据点 |
| Phase 4 范围（不含测试）| **5198 行** | 3500 | 3550 | v0.6 同步 master |
| Phase 4 范围（含测试）| **8476 行** | — | 6660 | v0.6 同步 master |
| Phase 5 后保留 Server 字段 | **≤ 12** | ≤ 12 | ≤ 12 | 一致 |
| Phase 5 后 Hub 字段验收目标 | **≤ 40** | ≤ 30 | ≤ 35 | v0.6 校准：从 47 字段压到 40 比"≤ 35"现实（v0.5 基于 43 字段）|
| 总 PR 数 | **13** | 11 | 13 (§6.1) / 11 (§六/§十/§十一) | v0.5 多处不一致；v0.6 全部统一为 13 |
| 节奏 | **13-15 周（含观察期）** | 9-10 周 | 13-15 周 | v0.5 起 |
| ROI gate 阈值 | **≥ 3 个文件 PRs/90d ≥ 15** | 月均冲突 ≥ 15min | 同 v0.6 | v0.5 起 |
| ROI gate 实测结果 | **6/6 文件超线（17-32 PRs/90d）→ gate 通过** | — | 同 v0.6 | v0.5 起 |
| 观察期总长（不含重叠）| **12 phase × 7 + Phase 5→final × 14 = 98 自然日** | — | 11×7=77 (§7.3) / 13×7=91 (§6.1) | v0.5 §7.3 错算；v0.6 修为 91（含 13 phase × 7）；v0.6.1 加 Phase 5 → final 14 天双倍观察期（mutex pprof 数据），共 98 天 |
| 实施起点 | **Phase 0 merge + ROI gate 当场通过** | T+14d 决策 | 同 v0.6 | v0.5 起 |
| Server.handle* 方法基线数 | **7** | — | — | v0.6 实测；exemptions.yaml `handle_baseline` 钉死 |

**修订纪律**（**v0.6 钉死，写进 §十三 评审签字**）：

1. 任何对本表的修改必须 PR description 第一行声明 `update §0 fact-table: <field>: <old> → <new>`
2. 修改本表后必须 grep 全文确认所有引用同步，单 commit 提交
3. v0.6 之后每个 phase merge 时 baseline 实测一次，对账本表
4. 本表是单一真相源——其他章节出现与本表不一致的数字，本表为准
5. **lint rule 6（v0.6.1 备做 / Phase 0 跟随 RFC）**：linter 扫所有 markdown 文件中的关键数字 token（"47 字段"/"13 PR"/"21313 行"/"≤ 40"/"13-15 周"等），与本速查表对账；漂移即 fail。**理由**：纪律 1-4 全靠人维护，正是 v0.5 痼疾的根因；rule 6 是把"约定"升级为"机器约束"的兜底。当前 v0.6 仅承诺写 RFC，实装在 v0.6.1 或 Phase 0 跟随。**承诺位置**：Phase 0 PR description 必须含 `lint-rule-6 RFC: <link>`，否则 Phase 0 不予合并

---

## 一、现状盘点

> 数据采集见 [`server-split-phase4-baseline.md`](server-split-phase4-baseline.md)（2026-05-28，origin/master HEAD `44a10e8d`）。
> v0.2 给的"17143 行 / 40 文件 / Server 28+ 字段 / Hub 28+ 字段"是粗估值；v0.4 baseline 用 17156/40/47/37 实测替换；v0.6 用 21313/58/47/47 重新同步到最新 master。

```
server 包行数:  含测试 53487 行 / 206 文件
              不含测试 21313 行 / 58 文件   ← v0.6（v0.4 时 17156 / 40 文件，3 个月涨 24%）

> 800 行的非测试文件（9 个，linter 硬上限超线 — v0.4 时仅 6 个）：
  dashboard_session.go        1713
  project_files.go            1632
  dashboard_send.go           1446
  dashboard_cron.go           1427
  dashboard_cron_transcript.go 1383
  server.go                   1334
  wshub.go                     902   ← v0.4 时 514，3 个月涨 388
  dashboard.go                 852   ← v0.4 时 731
  agent_tailer.go              827   ← v0.4 时 655，3 个月涨 172

500-800 行（2 个）：
  send.go                      703
  dashboard_auth.go            583

Server struct: 47 字段（baseline 实测，13 个是 12 个 handler 引用）
Hub struct:    47 字段（baseline §3 实测；v0.4 baseline 写 37，3 个月新增 10 字段；按职责分 7 块见 §五）
路由注册总数: 51 条（dashboard.go::registerDashboard 单点；v0.4 时 50 条）
Hub.queue 字段：直接耦合 *dispatch.MessageQueue（R242-GO-10 拖了 5 轮）

90 天活跃度（影响 ROI 决策）：
  仓库总 commit 345 / 涉 server 包 158（45.8%）
  没有 git merge commit（squash merge）→ ROI gate 用 unique PR 数（详 §十一）
```

handler-group 化第一轮（`auth/cronH/sessionH/projectH/discoveryH/transcribeH/sendH/cliH/scratchH/memoryH/agentEventsH/healthH`）已完成，但 12 个 handler-struct 仍然 **持有 Server 字段引用** 或被 Server 注入若干字段，本质上是把"god struct"折叠成"god struct + 12 个 view"，没真正解耦——**这是 v0.1 reviewer 一致指出的"换汤不换药"风险，v0.2 整改的核心**。

> **v0.6 新观察**：wshub.go 自身已超线（902 行）+ agent_tailer.go 超线（827 行），意味着 Phase 4 拆分后会**自动**清掉 2 个 baseline 豁免文件，比 v0.5 估算的 1 个（仅 wshub）更省。

---

## 二、设计目标（钉死，可量化）

| # | 目标 | 量化指标 | 验证方法 |
|---|---|---|---|
| 1 | server 包瘦身 | ≤ 5000 行（不含测试）/ ≤ 15 个非测试文件 | `wc -l $(ls internal/server/*.go \| grep -v _test.go)` |
| 2 | Server struct 字段瘦身 | **47 → ≤ 12** 字段（减 74%） | `awk '/^type Server struct/,/^}$/' internal/server/server.go \| grep -E '^\s+[a-zA-Z_]+ ' \| wc -l` |
| 3 | Hub 字段整理 | **47 字段维持不变**（v0.6 实测；v0.4 baseline 写 37 漏数 10 个；按 §五 7 块分组组织；本次不删字段，仅整理） | 同上脚本针对 wshub.go |
| 4 | 跨包依赖减耦合 | 每个 PR 减少 `s.X` 跨包字段引用数 ≥ 5 | `grep -c "deps.\|s\." per-PR diff 报告` |
| 5 | Hub.queue 接口化 | 关闭 R242-GO-10；MessageEnqueuer 单接口（5 方法）+ var _ 编译期 gate | `var _ wshub.MessageEnqueuer = (*MessageQueue)(nil)` |
| 6 | 零行为变更 | 51 路由路径不变；WS 协议字段不变 | `routes_snapshot_test.go` golden 对齐 |
| 7 | 防止重新膨胀 | AST linter rules 1-5 + 文件大小硬上限 + 文档化包契约 | CI gate |
| 8 | 渐进推进 | 单 PR ≤ 1500 行（Phase 4b/4c 例外，详 §6.5；4a 不超线无例外）；任一 phase 中断不阻塞后续 | PR diff stat |

### 不做什么

- ❌ 不引入 DI 框架（wire/fx）
- ❌ 不重写 dashboard handler 业务逻辑（搬家不动逻辑）
- ❌ 不动 `internal/dispatch` / `internal/node`（已稳定 4 周+）
- ❌ 不动 WS 协议（线协议字段、消息类型保持二进制兼容）
- ❌ 不动 51 条 HTTP 路由路径（dashboard.html 与 IM 客户端零改动）
- ❌ 不拆 Hub 为三子 struct（v0.1 计划取消，详见 §五）
- ❌ 不改 `/health` 拆分（R247-ARCH-1，正交，另起 PR）
- ❌ ~~不动 `scratchPool` 归属（保留在 Server）~~ **v0.5 推翻**：scratchPool 自管 sweeper / Server 不持字段 / NewHub Options 注入。详见 §6.7

---

## 三、切分总图

```
拆分前（v0.6 实测）：
  internal/server/                 21313 行 / 58 文件
    ├── server.go (1334) god-ctor + struct
    ├── wshub*.go + wsclient.go (~3700) WS hub god-struct（v0.4 时 ~3000）
    ├── dashboard*.go (~10000) 12 个 handler-group
    ├── project_files.go (1632) 项目文件 IO
    ├── send.go / agent_tailer.go (827) / upload_store.go / discovery_cache.go
    └── 安全/认证: csrf / clientip / ip_limiter / debug_*

拆分后：
  internal/server/                 ~3500 行 / ≤ 15 文件
    ├── server.go         核心 struct (≤ 12 字段) + Start/Shutdown
    ├── routes.go         51 条路由注册（来源：dashboard.go 注册块）
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

  internal/wshub/                  ~5200 行  WebSocket Hub（保单 struct；v0.4 时估 3500，v0.6 实测 5198）
    ├── hub.go            Hub struct（字段按 §五分组，47 字段）+ ctor + Shutdown
    ├── hub_broadcast.go  广播 / debounce 路径方法
    ├── hub_subscribe.go  订阅 / 注册 / 取消订阅方法
    ├── hub_send.go       发送 / 队列 / sendWithBroadcast 方法
    ├── client.go         wsClient (单连接读写泵)
    ├── upgrade.go        HTTP upgrade + auth gate
    ├── eventpush.go      事件推送循环
    ├── tailer.go         agent_subscribe/agent_unsubscribe (和 agent_tailer.go 强耦合，跟着搬)
    ├── consumer.go       HubRouter / cronHubOps / scratchOps interfaces (consumer-side)
    └── types.go          MessageEnqueuer interface

  embed.FS                         ~700  HTML/JS/manifest/sw 跟着 server 入口（不抽子包）

总计：~16400 行（v0.6 实测；vs 拆分前 21313 减 23%；server 包从 21313 → 3500 减 84%）。
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
    // ── lifecycle (3) ──────────────────────────────────
    mu     sync.RWMutex
    ctx    context.Context
    cancel context.CancelFunc

    // ── subscriber registry (10, RW: hub_subscribe.go) ─
    clients          map[*wsClient]struct{}
    connCount        atomic.Int64
    subscriberCount  atomic.Int64                   // v0.6 补：与 connCount 区分
    clientWG         sync.WaitGroup
    wsAuthLimiter    func(ip string) bool
    wsUpgradeLimiter func(ip string) bool
    upgrader         websocket.Upgrader
    dashTokenHash    [32]byte
    cookieMAC        string
    trustedProxy     bool                            // HTTP upgrade IP 提取信任配置

    // ── broadcast (6, RW: hub_broadcast.go) ────────────
    debounceMu         sync.Mutex
    debounceTimer      *time.Timer
    debounceFirst      time.Time
    debounceClosed     bool
    debounceClosedFast atomic.Bool                   // v0.6 补：debounce 快速关闭旗标
    debounceFire       func()                        // v0.5 补：debounce 触发回调

    // ── send / queue (6, RW: hub_send.go) ──────────────
    queue             MessageEnqueuer                // v0.5：原 sender/qctrl/qstats 三字段实测只有 queue
    sendWG            sync.WaitGroup
    sendTrackMu       sync.Mutex
    sendClosed        bool
    droppedTotal      atomic.Int64
    legacySendInvokes atomic.Int64                   // v0.6 补：legacy send 路径调用计数

    // ── shared dependencies (14, read-only after ctor) ─
    router       HubRouter
    agents       map[string]session.AgentOpts
    agentCmds    map[string]string
    dashToken    string
    nodes        map[string]node.Conn
    nodesMu      *sync.RWMutex
    projectMgr   *project.Manager
    resolver     *session.KeyResolver
    scheduler    cronHubOps
    scratchPool  scratchOps
    uploadStore  uploadOps
    guard        *session.Guard
    allowedRoot  string
    auth         *AuthHandlers                      // v0.6 补：auth handler 引用

    // ── agent tailer subsystem (3) ─────────────────────
    tailers        *tailerRegistry
    wiredLinkersMu sync.Mutex
    wiredLinkers   map[agentlink.AgentLinker]struct{}

    // ── rate-limit / cache (5, v0.5 起识别) ────────────
    historyMarshalCache *historyMarshalCache       // 历史序列化缓存
    userSendLimitersMu  sync.Mutex
    userSendLimiters    map[string]*rate.Limiter   // 按 user 维度发送限流
    connCountByOwnerMu  sync.Mutex
    connCountByOwner    map[string]int             // 按 owner 维度的连接计数
}
```

> **v0.6 修订**（同步 master 实测）：v0.5 写 43 字段（漏 4 个：auth/subscriberCount/legacySendInvokes/debounceClosedFast）；v0.6 实测 47 字段。
>
> 与 baseline §3 对齐：lifecycle 3 + subscriber 10 + broadcast 6 + send 6 + shared 14 + tailer 3 + rate-limit/cache 5 = **47**。本块表是 Phase 4 实施的字段归属契约——linter rule 3 据此对账，缺一字段都会让 Phase 4 PR 走 godoc 注释时分类不明。
>
> Phase 5 字段瘦身验收 gate（§九.1）从 v0.5 写的"≤ 35"调整为 **≤ 40**——cookieMAC ↔ dashTokenHash 合并、connCount ↔ subscriberCount ↔ connCountByOwner 三选一、debounceClosed ↔ debounceClosedFast 合并是潜在收敛路径，但保守地把 ctx/cancel/lifecycle 块 3 字段视作不可压。

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
//   WRITES:  subscriber-registry block (clients/connCount/subscriberCount/
//            clientWG/wsAuth*/upgrader/dashTokenHash/cookieMAC/trustedProxy)
//   READS:   shared-deps block (read-only after ctor)
//   READS-ALSO: send block (`sendClosed` only, for Shutdown coordination)
//
// AST linter rule 3 (file_block.go) parses this header and verifies
// each method's field access matches. Adding a field access not listed
// here fails CI.
```

**v0.3 跨块调用豁免**：现实中 hub_subscribe.go 关闭 client 时确实需要 drain pending sends（跨 send 块）。godoc 头允许声明 `READS-ALSO` 为只读跨块访问；linter 据此放行。**写跨块永远禁止**（除 lifecycle 豁免外）——必须把方法搬到对应文件，或拆成两个方法各文件一份。

**v0.5 lifecycle 块跨块写豁免**：构造 / 析构期方法天然要写多块——`NewHub()` 初始化所有字段、`Shutdown()` 必须按 `cancel ctx → debounceClosed=true → sendWG.Wait → close clients → clientWG.Wait` 五步顺序协调 broadcast、send、subscriber 三块。这是 lifecycle 的**本质职责**，不是污染。

豁免范围（钉死，不可扩）：

- ✅ `hub.go` 中的 `NewHub` / `Shutdown` / `Start`：允许写所有 7 块字段
- ❌ `hub_*.go` 任意其他方法：写跨块仍然禁止
- ⚠️ 单元测试 helper（`hub_test.go` / `testhelper/`）：与 lifecycle 同类，但需在 godoc 头声明 `LIFECYCLE-EXEMPT`，linter rule 3 据此放行

linter rule 3 实现要求：
1. 识别 `hub.go` 中归属在 lifecycle 块的方法（NewHub / Shutdown / Start / 同等签名）作为豁免对象
2. 其他文件方法默认严守"写跨块禁止"
3. 测试文件 godoc 含 `LIFECYCLE-EXEMPT` 标记时放行（限制：仅 _test.go 文件可使用此标记）

godoc 模板（lifecycle 方法）：

```go
// Shutdown coordinates orderly teardown across all 7 field blocks.
// LIFECYCLE-METHOD: writes ctx/cancel (lifecycle), debounceClosed (broadcast),
//                   sendClosed (send), close clients map (subscriber).
//                   Lock-order documented in docs/design/server-split-phase4-design.md §五.
func (h *Hub) Shutdown(ctx context.Context) error { ... }
```

**理由**：v0.4 设计完全没考虑 Shutdown 这种构造/析构期跨块协调的合法性，按字面规则会把 Shutdown 卡死在 linter rule 3。lifecycle 豁免是**真实需求**，但必须用关键词显式标注（不是默认放行任何 hub.go 方法），保留对 lifecycle 块的攻击面纪律。

### 锁方案钉死

**保持 `Hub.mu` 单锁**保护 `clients` map（subscriber 与 broadcast 共用读路径）。理由：

- Phase 4 是位置搬迁，**不动并发模型** —— 减少风险面
- `clients` 操作的 hot path 是 broadcast 的读（90%），Register/Unregister 写很少
- v0.1 提到的"锁分离"留作 Phase 6 优化（如果运行时验证显示有竞争）

**Phase 4 不变更锁结构** = 不引入新死锁路径。这条比 v0.1 "运行时观察" 更负责。

---

## 六、Phase 切分（**v0.6：方案 B 顺序 / 13 个 PR / 13-15 周（含观察期）**）

### 6.0 PR 切分原则

- ≤ 1500 行 / PR（reviewer 友好阈值；**Phase 4b（~2000）/ 4c（~1700）例外**，详 §6.5；4a 700 行不超线，无例外需要）
- 单 PR 自洽：build / test / vet / gofmt / routes_snapshot 全绿
- 双 commit 中**每个 commit 独立 build/test 绿**（详 §6.3 双 commit 与 routes_snapshot 同步契约）
- 序列依赖最小化：3a/3b/3c 之间无依赖；3e 依赖 3a-3d 全部完成（共享 dashboard 子包入口）

### 6.1 Phase 表

| Phase | 范围 | 行数估计（不含测试 / 含测试）| 破坏面 | 依赖 |
|---|---|---|---|---|
| **0** | 准备：godoc + interface 定义 + AST linter（rules 1-5）+ routes_snapshot_test.go + 文件大小 lint + 包契约文档 | -50 / +500 | 极低 | — |
| **4a** | wshub 骨架：hub.go + types.go + ctor + Shutdown + 5 文件壳（v0.5 拆 Phase 4 起；v0.6 范围按 master 重估）| ~700 / +1300 | 中 | Phase 0 |
| **4b** | wshub 方法实质搬迁：subscribe + broadcast + send + wsclient | -2000 / +2700 | **高**（含 send 路径与 broadcast 协调）| 4a |
| **4c** | wshub 收尾：agent_tailer + eventpush + hub_agent | -1700 / +2500 | 中 | 4b |
| **1** | 抽 `internal/dashboard/cron/`（含 cron / cron-transcript / 21 个 cron-related view types）| -2810 / +2400 | 中 | 4c |
| **2** | 抽 `internal/dashboard/project/`（project_api + project_files）| -1830 / +1750 | 中 | 1 |
| **3a** | 抽 `internal/dashboard/auth/`（dashboard_auth + csrf）| ~600 / +700 | 低 | 2 |
| **3b** | 抽 `internal/dashboard/discovery/`（dashboard_discovered + takeover）| ~450 / +500 | 低 | 2 |
| **3c** | 抽 `internal/dashboard/ext/scratch + memory` | ~700 / +800 | 低 | 2 |
| **3d** | 抽 `internal/dashboard/ext/agent_events + cli + transcribe + system` | ~700 / +800 | 低 | 2 |
| **3e** | 抽 `internal/dashboard/session/{list,events,interrupt,label}` | ~1000 / +1100 | 中 | 3a-3d |
| **3f** | 抽 `internal/dashboard/session/{send,upload,attachment,agent_tailer,upload_store}` | ~1500 / +1700 | **高**（含 send 路径）| 3e |
| **5** | Server struct 字段瘦身到 ≤ 12 + Hub 字段分组定型 + 删 god setter | -300 / +400 | 中 | 4c + 3f |

总 PR 数：**13 个**（Phase 0 + 4a + 4b + 4c + 1 + 2 + 3a + 3b + 3c + 3d + 3e + 3f + 5）。详见 §0 事实速查表。

> v0.6 修订：表格按 origin/master 实测重估（v0.4 时 Phase 1 范围 -2330 行；v0.6 实测 dashboard_cron + transcript 共 1427+1383=2810 行不含测试）。Phase 4a/4b/4c 行数详 §6.5。

按团队 1-2 人 / 1 PR/2 天节奏，**13-15 周完成**（v0.5 修订；v0.4 写"9-10 周"过乐观——13 PR × 7 天观察期 = 91 天纯等待，仅观察期就要 13 周；加上 review/merge/部署的实际工作时间到 13-15 周才现实）。

**v0.6.1 节奏分解**：
- 13 个 PR × 平均 review + merge + 部署 ≈ 3 工作日 = 39 工作日
- 12 个 PR × 7 天 + Phase 5 → final × 14 天（双倍）= **98 自然日**（v0.6.1 修：v0.6 写 91 漏 Phase 5 → final 双倍观察期，详 §7.3）
- 观察期重叠豁免（详 §7.3）：Phase 1↔2 / 2↔3a / 3a-3d 间 / 4b↔4c 等节省 ~14-21 天 = 2-3 周
- 实际净观察期：~77 天 ≈ 11 周
- 加上 review + merge + 部署（每 phase 2-3 工作日）：**13-15 周** total

详细分摊见 baseline.md §8。**v0.4 估算的 9-10 周仅在所有 phase 都能 100% 平稳无 incident、且观察期最大限度重叠时才成立**——把这视为"快道乐观估计"。13-15 周是"标称现实估计"。

#### 合并顺序

v0.2 写"Phase 4 可与 1-3 并行" — 措辞误。Phase 1-3 引用 `*server.Hub`，Phase 4 把 Hub 搬到 wshub 包，merge 时必然冲突。**v0.3 改写**：

- ❌ **方案 A（v0.2 顺序）**：1 → 2 → 3a-3f → 4 → 5。Phase 1-3 期间引用 `*server.Hub`，Phase 4 时全部改 import → 大量 mechanical churn
- ✅ **方案 B（v0.3 推荐 / v0.5 拆 Phase 4 微调）**：**Phase 0 → Phase 4a → 4b → 4c → Phase 1 → Phase 2 → Phase 3a-3f → Phase 5**。Phase 4 先把 Hub 搬到 wshub 包，dashboard 子包从一开始就 import 正确路径

方案 B 的代价是 Phase 4 风险（最大刀，5198 行不含测试）前移；收益是后续 6 个 PR 的 import 路径稳定，不再因 wshub 抽包二次改动。

"代码 review 可并行" 与 "merge 必须串行" 是两件事 — 6 个 dashboard 子包（Phase 1/2/3a-3f）的代码 **review 阶段** 可重叠（不同 reviewer 不同 PR），但 merge 必须严格按方案 B 串行。

### 6.2 Phase 0：准备（独立 PR，**1 周**）

包含 7 个独立交付物：

#### 0.1 字段读写注解
- `Server struct` **47 字段**加 `// 读写: <files>` 注释（v0.6 校准；v0.4 时仍沿用 v0.2 时代的"28+"粗估，v0.6 实测 47 替换；详 §0 速查表）
- `Hub struct` **47 字段**加 `// 读写: <files>` 注释（按 §五 7 块字段分组组织；v0.4 baseline 写 37 漏数 10 个，v0.6 实测 47 替换；详 §0 速查表）

#### 0.2 接口定义
- `internal/wshub/types.go`：`MessageEnqueuer`（先放 server 包等 Phase 4a 一起搬）
- `dispatch.MessageQueue` 单条 `var _` 绑定

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

#### 0.4 AST-based linter（**v0.3 重写：阶段化 gate + baseline 豁免；v0.6 整合 rules 1-5**）

⚠️ **v0.2 致命缺陷**：6 个文件已超 800 行硬上限（v0.6 实测 9 个），linter Phase 0 启用 `fail` 模式立刻全红，Phase 0 PR 无法独立合入。v0.3 改阶段化方案：

| 阶段 | linter 模式 | 文件大小规则 |
|---|---|---|
| Phase 0 | **warn**（CI 输出但不卡 PR）| 现有超线文件冻结到 `tools/lint-server-handlers/exemptions.yaml` baseline 清单；新增文件走硬上限 |
| Phase 1-4 | **warn**（同上）| 每 phase merge 后自动从豁免清单删除已不超线的文件；新增文件仍走硬上限 |
| Phase 5 完工后 | **fail** | 豁免清单必须为空才算 Phase 5 验收通过 |

linter 实现位置 `tools/lint-server-handlers/`：

```
main.go                  入口（CLI）+ SARIF 输出
rules/handle_decl.go     规则 1：禁止新增 func (s *Server) handle* （新文件 / 新函数）
rules/file_size.go       规则 2：文件大小超限（按 internal/server/ 500 / dashboard/* 800）
rules/field_block.go     规则 3a/3b：hub_*.go 方法 godoc 头 + 字段访问对账（v0.5 拆两层）
rules/iface_match.go     规则 4：实现侧反向注释 vs 消费侧本地接口对账（v0.3 新增，见 §四.1）
rules/stale_exemption.go 规则 5：豁免清单反向依赖（v0.5 新增；查 git tag 与 file_size 条目）
exemptions.yaml          Phase 0 baseline 冻结的超线文件清单（每个条目带 until_phase）
```

`exemptions.yaml` 模板（**v0.6 按 master 实测重写：11 个超线文件；limit 字段全部按 server 包 500 硬上限**）：

> **v0.6 修订（重要）**：limit 字段必须与 §9.2 的硬上限阈值匹配——`internal/server/` 包内文件硬上限 **500 行**（不是 800），`internal/dashboard/<domain>/` 子包硬上限 **800 行**。v0.5 的 yaml 模板把 limit 全写 800 是错的：CI 用 500 阈值扫 server 包文件，会把 server.go(1334) / dashboard.go(852) 等列为超线，但豁免里的 `limit=800` 让 linter 误以为"豁免到 800 即可"——实际超 500 就该报。改：本期所有条目都在 server 包内，全部 `limit: 500`；dashboard 子包尚未存在，无需豁免。

```yaml
file_size:
  # 全部 11 个超线条目均在 internal/server/ 包内，硬上限 500（不是 800）
  # 9 个 > 800 行：
  - path: internal/server/dashboard_session.go
    current: 1713
    limit: 500             # v0.6 校准：server 包硬上限 500（v0.5 模板误写 800）
    until_phase: 3e        # Phase 3e 抽完 dashboard/session/{list,events,interrupt,label}
  - path: internal/server/project_files.go
    current: 1632
    limit: 500
    until_phase: 2
  - path: internal/server/dashboard_send.go
    current: 1446
    limit: 500
    until_phase: 3f
  - path: internal/server/dashboard_cron.go
    current: 1427
    limit: 500
    until_phase: 1
  - path: internal/server/dashboard_cron_transcript.go
    current: 1383
    limit: 500
    until_phase: 1
  - path: internal/server/server.go
    current: 1334
    limit: 500
    until_phase: 5
  - path: internal/server/wshub.go        # v0.6 新增：v0.4 时 514 不超线
    current: 902
    limit: 500
    until_phase: 4b
  - path: internal/server/dashboard.go    # v0.6 新增：v0.4 时 731 不超线
    current: 852
    limit: 500
    until_phase: 5
  - path: internal/server/agent_tailer.go # v0.6 新增：v0.4 时 655 不超线
    current: 827
    limit: 500
    until_phase: 4c
  # 2 个 500-800 行（v0.6 新增豁免：超 server 500 但不超 dashboard 800）：
  # send.go 由 Phase 3f 顺带搬到 dashboard/session/；dashboard_auth.go 由 Phase 3a 搬到 dashboard/auth/
  - path: internal/server/send.go
    current: 703
    limit: 500
    until_phase: 3f
  - path: internal/server/dashboard_auth.go
    current: 583
    limit: 500
    until_phase: 3a
  # 总 11 个 > 500 行文件（其中 9 个 > 800；v0.4 时 6 个 > 800）
```

> **dashboard 子包阈值差异**：未来 `internal/dashboard/<domain>/` 硬上限是 **800 行**（不是 500），因为子包是按业务域聚合的、文件天然大；exemptions.yaml 中若有 dashboard 子包条目（v0.6 暂无），limit 应写 800。linter 实现需支持按路径前缀分别应用 500/800 阈值（rule 2 的实现已支持，详 `tools/lint-server-handlers/rules/file_size.go`）。

每个 phase 的 PR 必须**减少**或**清空**对应豁免条目。豁免清单**只在 Phase 0 一次性建立**，后续 phase 不允许新增条目。

集成进 `make lint` + `.github/workflows/lint.yml`，CI 走 `mode=warn`；新增文件硬卡 fail。

##### v0.5 反向依赖保护（**新增 rule 5: stale_exemption**）

> **v0.4 缺漏**：until_phase 只规定"该豁免在 phase N 之前有效"，但**没有强制 phase N merge 后必须删除**。如果 Phase 2 merge 了但有人忘了从 exemptions.yaml 删掉 `project_files.go` 条目，CI 不会 fail（条目仍标 `until_phase: 2`，linter 在 warn 模式下继续容忍）。豁免债越积越多，直到 Phase 5 切 fail 模式才崩。
>
> **v0.5 整改**：linter 加 rule 5「stale_exemption」反向校验。

**Rule 5 行为**（实现位置 `tools/lint-server-handlers/rules/stale_exemption.go`）：

1. 读取 `exemptions.yaml` 全部 `file_size[].until_phase`
2. 与 git tag 列表比对：若 `server-split-phase{N}` tag 已存在（即该 phase 已 merge），则 `until_phase: N` 的所有条目必须不再被引用
3. 不再被引用 = 1 of：(a) 文件已不存在；(b) 文件存在但行数已 ≤ limit；(c) 条目已从 yaml 删除
4. **(a)/(b) 而 (c) 未做 → fail**：报错"`<file>` 已满足 limit / 已不存在，但 `until_phase: <N>` 条目残留 — 请删除"

**实操示例**：

```bash
# Phase 2 merge 后，git tag 含 server-split-phase2
# 但 project_files.go 的 exemption 条目仍在
$ make lint-server
[stale_exemption] internal/server/project_files.go (until_phase: 2):
  ✗ tag server-split-phase2 已存在
  ✗ 文件不存在 → 条目应删除
  → 请编辑 tools/lint-server-handlers/exemptions.yaml 删除该条目
```

**Phase X PR commit message 约定**（**v0.5 强制**）：

每个抽包 phase 的 PR commit message 必须含 trailer：

```
Closes-exemption: internal/server/<file>.go
```

CI 校验：phase X PR 若 commit message 无对应 `Closes-exemption:` 行，且 exemptions.yaml 中 `until_phase: X` 条目未减少，**fail**。

> 这把"豁免债清理"从"自觉行为"变为"机器约束"。豁免清单只能减不能增，且减必须留 audit trail。

##### Rule 5 阶段化交付

| 阶段 | 交付 |
|---|---|
| Phase 0 | rule 5 框架实现（读 exemptions.yaml + 列文件存在性检查）|
| Phase 1 merge 前 | git tag 比对完整实装 |
| Phase 5 完工 | mode=fail；exemptions.yaml 必须空（rule 2 + rule 5 双保险）|

**Phase 0 / Phase 1 / Phase 4 阶段化交付（v0.6 整合 rule 3a/3b 拆分 + rule 5）**：

| 规则 | Phase 0 | Phase 1 前 | Phase 4a 前 | Phase 4b 前 | Phase 5 |
|---|---|---|---|---|---|
| Rule 1 handle_decl | ✅ 必交付 | — | — | — | mode=fail |
| Rule 2 file_size | ✅ 必交付 + exemptions baseline | — | — | — | exemptions 必须空 |
| Rule 3a field_block 骨架（仅检查 godoc 含 WRITES 行） | ✅ 必交付 | — | warn → 必交付 | — | mode=fail |
| Rule 3b field_block AST 对账（解析 godoc 头 + 字段访问追踪 + 与 §五 7 块对账） | — | — | — | ✅ **必完整实现**（v0.5 拆 3a/3b：复杂 AST 走 4b 前补完） | mode=fail |
| Rule 4 iface_match | 框架（伪实现 OK） | ✅ **必完整实现**（§四.1 反向注释 = Phase 1+ 实现契约） | — | — | mode=fail |
| Rule 5 stale_exemption | ✅ 框架交付 | ✅ **完整 git tag 比对** | — | — | mode=fail |

> **v0.3 错点**：rules 3-4 标 "Phase 4-5 期间补全"，但 §五 / §四.1 已经把它们当 Phase 1+ 的实现契约。v0.4 改为：rule 3 必须在 **Phase 4 PR merge 前**补完；rule 4 必须在 **Phase 1 PR merge 前**补完。Phase 0 交付的是骨架（能跑、能 SARIF 输出），实现完整规则在对应 phase 前完成。
>
> **v0.5 拆 rule 3 为 3a/3b**：v0.4 把 rule 3 整体推到"Phase 4 前必完整实现"低估了字段访问 AST 对账的复杂度（要解析每个文件 godoc 头的 WRITES/READS-ALSO + 走方法体所有 `h.<field>` 引用 + 跨方法调用追踪 + 与 §五字段归属表对账）。1 个工程师 1 周做不完，更现实是 2-3 周。
>
> v0.5 拆法：
> - **rule 3a**（仅检查 godoc 含 `WRITES:` / `READS-ALSO:` 行存在）— Phase 0 交付，简单文本扫描
> - **rule 3b**（AST 字段访问对账 + 字段归属表 + 跨方法追踪）— Phase 4b 中段（4a 完工后、4b 之前）补完
>
> 这样 Phase 0 能立刻给"忘了写 godoc 头"的反馈（最常见的疏忽），4b 真实搬迁前再启用 AST 对账（需要的时机），降低了关键路径上的实现风险。
>
> **v0.5 新增 rule 5 stale_exemption**：见上面"反向依赖保护"小节。

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

linter rule 4（Phase 1 前补全）扫 `// satisfies:` 注释比对此文档。

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
wc -l internal/server/*.go | tail -1                # 21313 行基线（v0.6 实测）
go test -race -count=1 ./... | grep -c "PASS"       # pass 数基线
```

后续每个 phase merge 前必须证明：耗时增加 < 10%、pass 数不减。

#### 0.7 验收
- [ ] `go build ./...`
- [ ] `go test -race -count=1 ./...` 全绿
- [ ] AST linter 在 master 上跑无误报（rule 1-5 全部 warn 模式启用）
- [ ] routes_snapshot golden 与当前 mux 完全对齐（51 条路由）

### 6.3 Phase 1-2 / 3a-3f：抽 dashboard 子包

**Commit 策略**（沿用 [router-split-design](router-split-design.md) §commit 策略行数分档）：

- > 500 行 → **强烈推荐双 commit**（commit a 纯机械迁移、commit b godoc/gofmt polish）
- 300-500 行 → 双 commit 可选
- < 300 行 → 单 commit 一次到位

按本设计 Phase 1（~2810 不含测试）/ Phase 2（~1830）/ Phase 4b（~2000）必须双 commit；Phase 3a-3f 视具体行数决定。

#### 双 commit 与 routes_snapshot 的同步契约（**v0.5 新增**）

> **v0.5 整改**：v0.4 §0.3 用 `HandlerType` 字段（如 `*dashboardcron.Handlers`）做 routes_snapshot golden。但双 commit 流程下，commit a 把 handler 从 `*Server` receiver 改到 `*dashboardcron.Handlers` —— `HandlerType` 字段必然变化，golden 必须更新。如果 golden 只在 commit b 更新，commit a 自身的 `routes_snapshot_test.go` 失败 —— 违反 [process-split v3 §0](../rfc/process-split.md) 已经踩过的"每 commit 独立 build 绿"教训（bisect 工具会撞墙）。

**钉死规则**：

| 场景 | golden 更新位置 | 理由 |
|---|---|---|
| 单 commit（< 300 行 PR） | 同 commit | 不存在切分问题 |
| 双 commit（≥ 500 行 PR） | **commit a** 一并更新 | commit a 必须独立 build/test 绿；commit b 仅做 godoc/gofmt（不改 type） |
| 路由路径真实变更（罕见） | 显式新增 commit c "update routes golden + reason" | 路径变更属于行为变化，需独立审查 |

**实操流程**（每个 dashboard 子包搬迁 PR）：

```bash
# commit a: 移代码 + 改 receiver + 更新 routes golden
git add internal/dashboard/cron/...
git add internal/server/dashboard_cron.go        # 删除原文件
UPDATE_GOLDEN=1 go test -run TestRoutesSnapshot ./internal/server/...
git add internal/server/testdata/routes.golden.json
git commit -m "phase 1: extract dashboard/cron (mechanical move + golden)"

# commit a 验证：必须独立绿
git stash; go build ./...; go test -race ./...; git stash pop

# commit b: godoc / gofmt polish
goimports -w internal/dashboard/cron/...
git add internal/dashboard/cron/
git commit -m "phase 1: dashboard/cron godoc + gofmt polish"
```

**反模式**（PR review 必须打回）：

- commit a 没更新 golden → commit a 单独 build 失败 → bisect 工具走到此 commit 卡住
- commit b 更新 golden → 把"机械搬迁"和"路由签名变化"耦合到同一 commit，回滚粒度变粗
- 用 `git rebase --autosquash` 后又分开 → fixup commit 顺序倒装时 golden 漂移

**linter 兜底**：`make pre-push` 检查每个 commit 独立 build/test 绿（router-split RFC v4 已有同款 hook，沿用）。

每个 phase 必须满足：

- PR description 给跨包字段引用减少数（≥ 5）
- PR commit message 含 `Closes-exemption: internal/server/<file>.go` trailer（v0.5 rule 5 强制）
- `routes_snapshot_test.go` 通过（route 路径不变）
- `go test -race -count=2 ./internal/dashboard/<domain>/...`
- 双 commit 中**每个 commit 独立 build/test 绿**（pre-push hook 强制）
- 手工冒烟（详见 §九）

### 6.4 Phase 3a-3f：抽剩 9 个 handler-group

每 PR 单独 review；3a-3d 互不依赖可并发推进；3e/3f 串行。

### 6.5 Phase 4：抽 `internal/wshub/`（**v0.5 拆 4a/4b/4c；v0.6 行数按 master 重估**）

> **v0.4 错点**：v0.4 估"-3500 / +3550 行"是非测试估算。**v0.6 实测**：`wshub*.go 5589 行（含测试）/ 3213（不含）`、`agent_tailer 1055 行`、`wsclient 455 行`、`send.go 703 行`——**Phase 4 总范围含测试 ≈ 8476 行**，不含测试 ≈ 5198 行。一刀超 §6.0 的 1500 行/PR 上限 5 倍。
>
> **v0.5 拆三刀，v0.6 行数同步**：

| 子 phase | 范围 | 行数（不含测试 / 含测试）| 风险 |
|---|---|---|---|
| **4a** | `hub.go` + `hub_types.go` + 字段块定型 + ctor + Shutdown 骨架 + 5 方法分文件壳 | ~700 / ~1300 | 中 |
| **4b** | `hub_subscribe.go` + `hub_broadcast.go` + `hub_send.go` 完整方法搬迁 + `wsclient.go` | ~2000 / ~2700 | 高（含 send 路径与 broadcast 协调）|
| **4c** | `agent_tailer.go` + `hub_agent.go` + `hub_eventpush.go` + 关联测试 | ~1700 / ~2500 | 中 |
| **4 sum** | — | ~4400 / ~6500 | — |

> 拆完后每刀 ≤ 2700 行（含测试）；4b 最大但因 send 路径完整搬迁不可再拆；4a/4c 在阈值内。**send.go 703 行单独留在 server 包**——Phase 3f 抽 dashboard/session/send 时一并搬，不属于 Phase 4 范围。

#### 4a：Hub 骨架 + 字段块定型

- 创建 `internal/wshub/` 包结构（hub.go / types.go）
- Hub struct 按 §五 7 块字段顺序排好（**47 字段**）；ctor `NewHub(opts HubOptions)` 完成
- 5 个方法分文件壳（hub_subscribe.go / hub_broadcast.go / hub_send.go / hub_agent.go / hub_eventpush.go）每个仅含 1-2 个 placeholder 方法 + godoc 头
- Shutdown 骨架（lifecycle 块跨块写）+ `hub_concurrency_test.go` 骨架（仅 1 个测试 case）
- linter **rule 3a** 启用 warn 模式校验 godoc 头存在性（v0.6 显式：用 3a 不是 3b）

**4a 验收 gate**：
- 子包 build 绿；4a merge 时 `internal/server/wshub*.go` 仍存在（旧 Hub 与新骨架并存，过渡期）
- AST linter rule 3a 对 4a 新文件 0 误报
- exemptions.yaml 中 `wshub.go (until_phase: 4b)` 仍保留（4a 是骨架，wshub.go 还没缩）

#### 4b：方法实质搬迁

- `Register / Unregister / HandleUpgrade / wsAuthGate` 等 subscriber 块方法 → 搬到 `wshub/hub_subscribe.go`
- `BroadcastSessionsUpdate / scheduleDebounce / debounceFire` 等 broadcast 块方法 → 搬到 `wshub/hub_broadcast.go`
- `SendWithBroadcast / TrackSend / sessionSendLegacy` 等 send 块方法 → 搬到 `wshub/hub_send.go`
- `wsclient.go` → 搬到 `wshub/client.go`
- 删除 `internal/server/wshub.go` / `wshub_subscribe.go` / `wshub_broadcast.go` / `wshub_send.go` / `wsclient.go`

**4b 验收 gate**：
- linter **rule 3b** 启用 warn 模式做 AST 字段访问对账（v0.6 显式）
- `hub_concurrency_test.go -race -count=100` 通过（broadcast 中触发 Shutdown / Register 与 Shutdown 并发）
- routes_snapshot 不变（Hub 是构造期对象，不直接挂路由）
- `internal/server/` 不再 import `dispatch.MessageQueue` 直接类型（Hub.queue 经 MessageEnqueuer 接口）
- exemptions.yaml 中 `wshub.go (until_phase: 4b)` 已删除（PR commit message 含 `Closes-exemption: internal/server/wshub.go`）

#### 4c：agent_tailer + eventpush 收尾

- `agent_tailer.go + agent_tailer_test.go` → `wshub/tailer.go + tailer_test.go`
- `wshub_agent.go` → `wshub/hub_agent.go`
- `wshub_eventpush*.go` → `wshub/hub_eventpush.go`
- 清理 server 包内残留 import

**4c 验收 gate**：
- linter rule 3a + 3b 切到 fail 模式（v0.6 显式：4c 后 wshub 子包是新生产代码，不再容忍违规）
- `internal/server/wshub*.go` 文件不再存在（grep 0 匹配）
- `agent_tailer.go` 不再在 server 包
- linter rule 3 对 wshub 子包内全部方法 0 违规
- exemptions.yaml 中 `agent_tailer.go (until_phase: 4c)` 已删除

#### 共同特别关注（4a/4b/4c 都要满足）

- Hub Shutdown 协调链路：`ctx cancel → debounceClosed = true → drain sendWG → close clients → wait clientWG`（lifecycle 块跨块写豁免，详 §五）
- `hub_concurrency_test.go`（**4b PR 必须新增完整测试**）：
  - 模拟 broadcast 中触发 Shutdown
  - 模拟 Register 与 Shutdown 并发
  - 模拟 send 与 broadcast 同时进行（清理跨块只读豁免实测验证）
  - 跑 `-race -count=100`
- 4a → 4b 之间**观察期 7 天独立**（4a 是骨架并存，4b 是真实搬迁，独立观察）
- 4b → 4c **观察期 4 天可重叠**（同属 wshub 抽包，无新运行时交互）
- 4c → 1 **观察期 7 天独立**（dashboard 子包从 wshub 包 import 路径稳定后才能开始）

### 6.6 Phase 5：Server 字段瘦身（47 → 12）

#### 47 个字段去向（v0.6 重新逐字段实地对账）

> v0.4 错点：把 `version / cookieMAC / wsAuthLimiter / wsUpgradeLimiter` 当 Server 字段——实际 cookieMAC/wsAuth* 在 Hub，versionTag 在 SessionHandlers。"删除 5"实际写了 6 项（noOutputTimeout/totalTimeout 重复计入）。v0.4 按 baseline.md §2 实测重对账，v0.6 同步 master：

##### 保留 12 个（HTTP 入口 5 + 核心依赖 4 + 多节点 3）

```
HTTP 入口   addr / mux / startedAt / onReady / appCtx
核心依赖    router / scheduler / hub / projectMgr
多节点      nodes / nodesMu / reverseNodeServer
```

##### 搬走 35 个

| 去向 | 个数 | 字段 |
|---|---|---|
| `routes.go` 局部变量（构造期，注册完丢弃）| 13 | 12 handler-group + nodeAccess |
| `NewHub` Options 注入 | **10** | dedup / sessionGuard / msgQueue / agents / agentCommands / dashboardToken / allowedRoot / noOutputTimeout / totalTimeout / **scratchPool**（v0.5 决议；详 §6.7）|
| `dashboard/*` 子包持有 | **4** | claudeDir（discovery）/ workspaceName（公共）/ discoveryCache（discovery）/ sysessionMgr（ext）— v0.5 移除 scratchPool |
| server 包内重组到独立文件 | 3 | debugMode（→ server/debug.go）/ resolver（→ routes.go 局部）/ nodeCache（→ server/nodecache.go）|
| `metrics` 包 | 2 | watchdogNoOutputKills / watchdogTotalKills |
| **待评估删除**（v0.4 N7 改"待评估"，Phase 5 写之前必须 grep 实装依赖 + 写小设计文档）| 3 | platforms（疑似 routes 注册期局部）/ backendTag（dispatch.BackendTag() 派生路径）/ knownNodes（合 nodes map）|

**加法核对**：13 + 10 + 4 + 3 + 2 + 3 = **35** ✓ ；保留 12 + 搬走 35 = **47** ✓（v0.6：scratchPool 在表内从"dashboard 子包"移到"NewHub Options"，总数不变；与 baseline §2 v0.6 同步表一致）

##### Hub 字段不动

baseline §3 实测的 Hub **47 字段**（v0.6 校准；v0.4 baseline 写 37 漏数 10 个）**不在 47 个 Server 字段范围**。Phase 5 不动 Hub 字段；Hub 的 setter 删除见下节。

#### 删 god setter

v0.1 提到的 `SetScheduler / SetUploadStore / SetScratchPool` 三个 setter 实际位置在 **`internal/server/wshub.go`** 的 `Hub` 上（不是 Server，v0.2 措辞误）。Phase 5 改 `NewHub Options` 一次性注入，删除三个 setter。

#### 验证

```bash
# Phase 5 完工 gate
awk '/^type Server struct/,/^}$/' internal/server/server.go | grep -cE '^\s+[a-zA-Z_]+ '   # 必须 ≤ 12
grep -E "func \(h \*Hub\) Set" internal/wshub/*.go | wc -l                                  # 必须 == 0（Phase 4c 后已搬到 wshub 子包）
```

### 6.7 `scratchPool` 三角问题（**v0.5 新增**）

> **v0.5 整改**：v0.4 §十二.3 说"scratchPool 留 Server：因为 Server.Shutdown 要 stop sweeper"，但 baseline §2 字段去向表又把它列入"dashboard/* 子包持有"5 项；同时 `internal/server/dashboard.go:388` 已存在 `s.hub.SetScratchPool(s.scratchPool)` 把同一对象注入 Hub。三处自相矛盾，Phase 5 删除 `SetScratchPool` 之后会留下"Server 持但 Hub 也要用"的 ownership 死结。
>
> **v0.5 钉死方案**：把 sweeper 启停搬进 `*session.ScratchPool` 自己的生命周期；Server 完全不持字段；Hub 通过 `NewHub Options` 一次性注入。

#### Phase 5 必做的 4 件事（v0.6 显式 main ctx 注入）

1. **`*session.ScratchPool` 自管 sweeper**（`internal/session/scratchpool.go`）
   - `NewScratchPool(ctx, ...)` **接 main ctx 参数**（v0.6 显式：来自 server.New 入参或 main goroutine 顶层 ctx）；内部 `go pool.runSweeper(ctx)`
   - `Stop()` cancel 内部 ctx 并 wait sweeper goroutine 退出（向后兼容；新代码靠 main ctx cancel 自然退出）
   - 调用方不再需要显式 `StartSweeper()`；现存的 `dashboard.go:389` 一行调用 Phase 5 PR 删除

2. **Server 不再持 `scratchPool` 字段**
   - 旧路径：`s.scratchPool = session.NewScratchPool(...)` → `s.hub.SetScratchPool(s.scratchPool)` → `s.scratchPool.StartSweeper()` + Shutdown 走 `s.scratchPool.Stop()`
   - 新路径：`pool := session.NewScratchPool(mainCtx, ...)` 局部变量 → `NewHub(HubOptions{ScratchPool: pool, ...})` 注入 → 主 ctx cancel 时 pool 自然 Stop（sweeper 监听 main ctx）

3. **Server.Shutdown 不再显式 Stop pool**
   - 由 main goroutine 的 ctx cancel 触发 pool 内部 sweeper 退出
   - 删除 `server.go:1080` 那段 `s.scratchPool.Stop()` 显式调用

4. **main ctx vs Hub.ctx 区分（v0.6 钉死）**
   - sweeper **必须监听 main ctx**（即 server.New 的 ctx 入参 / cmd/naozhi/main.go 顶层 ctx）
   - 不能监听 Hub.ctx——wireup 顺序：main ctx → server → hub.NewHub(...)，Hub.ctx 是 main ctx 的子 ctx；如果 sweeper 监听 Hub.ctx，pool 在 Hub Shutdown 后立刻死，但 server 还在 Shutdown 阶段可能调 pool（虽然新设计已经避免，但纪律上需要钉死）
   - server.New 签名：`func New(ctx context.Context, opts ServerOptions) (*Server, error)`，Server.Shutdown 不带 ctx 参数（保持 v0.4 签名不变；ctx cancel 由 main 控制）

#### 验证

```bash
# Phase 5 完工后
grep -n "scratchPool" internal/server/server.go              # 必须 == 0
grep -n "SetScratchPool" internal/wshub/*.go                 # 必须 == 0
grep -n "StartSweeper" internal/{server,session}/*.go        # 必须 == 0（接口不再暴露）
grep -n "NewScratchPool(ctx" internal/session/*.go           # 必须 ≥ 1（接 ctx 参数已落实）
```

#### 风险

- 行为变化（sweeper 启停由调用方→自管）属于 §四.3 不计入"行为变化"的"内部重组"；但需要在 Phase 5 PR description 单独列出
- 测试用例若依赖 `scratchPool.StartSweeper()` / `Stop()` 显式调用顺序，必须改为通过 main ctx cancel 验证
- main ctx vs Hub.Shutdown：sweeper 优先监听**main ctx**而非 Hub.ctx，否则 wireup 顺序倒装时 sweeper 会泄露（v0.6 已在第 4 件事钉死）

---

## 七、回滚预案（**v0.2 新增**）

### 7.1 单 Phase 回滚（**v0.6 加 4a/4b/4c tag**）

每个 Phase 合并时打 git tag（**v0.3 改 prefix 防与发版冲突**——线上发版 tag 是 `v0.0.20` 形式）：

```
server-split-baseline          Phase 0 之前
server-split-phase0            Phase 0 完成
server-split-phase4a           Phase 4a 完成（按方案 B 顺序：紧跟 Phase 0）
server-split-phase4b           Phase 4b 完成
server-split-phase4c           Phase 4c 完成
server-split-phase1
server-split-phase2
server-split-phase3a ... 3f
server-split-final             Phase 5 完成
```

> v0.6 修订：v0.4/v0.5 tag 列表只写 `server-split-phase4`，但 v0.5 已拆 4a/4b/4c。回滚时必须能精确到子刀。

```bash
# 单 phase 回滚（出问题在 N 时）
git revert server-split-phase{N-1}..server-split-phase{N}

# 或：直接 reset 到上个 tag（保留 master 历史）
git checkout master
git reset --hard server-split-phase{N-1}     # 仅在紧急 incident 用
```

### 7.2 部分回滚的边界

**不支持**单独回滚 Phase 3 中的某个子 PR（如 3c 已经 merged，3d 未发生）。原因：
- 3c 引入了 `internal/dashboard/ext` 子包入口，3d 复用同入口
- 部分回滚会留悬空 import

**整改**：3a/3b/3c/3d 必须**全部到位再做 3e**。3e 出问题时，回滚 3e 是干净的（独立子包 session/）。

**Phase 4a/4b/4c 独立回滚（v0.6 新增）**：
- 4a 是 `internal/wshub/` 骨架 + `internal/server/wshub*.go` 旧 Hub **并存**——4b 出问题可独立 reset 到 4a tag（旧 Hub 还在生产代码里，无悬空）
- 4b 抽出方法、删除旧文件——4c 出问题可独立 reset 到 4b tag（agent_tailer 还在 server 包未搬，无悬空）
- 4c 是 agent_tailer + eventpush 收尾——4c 出问题独立 reset 到 4b 也干净

### 7.3 7 天观察期 SLA（**v0.3 新增 / v0.6 重叠规则同步 13 phase**）

每个 Phase merge 到 master + 部署到生产后，**默认 7 天观察期**：

- 期间不接下个 Phase 的 PR merge（PR 可以提、可以 review，但不合）
- 7 天内发现问题 → 单 phase revert 不冲突（因为下个 phase 还没合）
- 7 天后再发现问题 → 必须 forward fix（写新代码修，不 revert）

#### v0.6 观察期重叠豁免（按风险等级分档；同步 13 phase）

> **v0.4 仅笼统说"连续 phase 强 import 依赖时观察期可重叠"**——含混。v0.5 钉死表格但仍按 11 phase 算；v0.6 同步到 13 phase（含 4a/4b/4c）。

| 当前 Phase | 下一 Phase | 重叠规则 | 理由 |
|---|---|---|---|
| Phase 0 | Phase 4a | **观察期 0**（紧跟 ROI gate 通过即开 4a）| Phase 0 无运行时变化，纯添加 lint + 文档 |
| Phase 4a | Phase 4b | **观察期 7 天，不可重叠** | 4a 是骨架并存，4b 是真实搬迁，独立观察 |
| Phase 4b | Phase 4c | **观察期重叠 4 天**（4c PR 在 4b merge 后 3 天可 merge）| 同 wshub 包内，无新运行时交互 |
| Phase 4c | Phase 1 | **观察期 7 天，不可重叠** | dashboard 子包从 wshub 包 import 路径稳定后才能开始 |
| Phase 1 | Phase 2 | **观察期重叠 4 天** | 都是 dashboard/ 子包搬迁，无运行时交互 |
| Phase 2 | Phase 3a | **观察期重叠 4 天** | 同上 |
| Phase 3a/3b/3c/3d | 互相 | **完全并行**（Phase 2 之后即可开 4 个 PR）| 4 个独立子包无依赖，独立合入独立观察 |
| Phase 3d → 3e | **不可重叠**（Phase 3e 必须等 3a-3d 全 merge + 3 天）| 3e 引用 ext 子包入口，需要 3a-3d 稳定 |
| Phase 3e → 3f | **观察期 7 天，不可重叠** | 3f 含 send 路径，高风险 |
| Phase 3f → 5 | **观察期 7 天，不可重叠** | Phase 5 是字段瘦身收尾，必须等 3f 稳定 |
| Phase 5 → final | **观察期 14 天独立（双倍）** | 收尾性 phase + §十二.2 mutex pprof 数据采集需稳定流量；v0.6 补：原 v0.5 表无此行，但 §9.1 Phase 5 验收 gate 含 mutex profile 入档要求，需要 14 天连续生产负载样本 |

**重叠豁免计算后实际节奏**：
- 不重叠总观察期：**12 phase × 7 + 1 phase × 14（final）= 98 自然日**（v0.6 修：v0.5 §7.3 写 11×7=77 但 §6.1 写 13×7=91 不一致；v0.6 加上 Phase 5 → final 双倍观察期共 98 天）
- 重叠豁免节省：4b↔4c / Phase 1↔2 / 2↔3a / 3a-3d 之间 ≈ 节省 14-21 天（4 处重叠各 ~3-4 天）
- 实际观察期：~77 天 ≈ 11 周
- 加上 review + merge + 部署（每 phase 2-3 工作日）：**13-15 周** total（详 §0 事实速查表）

**例外升级**：观察期内任一指标超线 → **立即 revert + 当前 phase 重做**，所有后续 phase 重新进入 7 天独立观察期（即清空所有重叠豁免到 phase 重启完成）。

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

- 邮件 / 群通知运维 + 主用户 "Phase 4b 发布期间 dashboard 可能闪断 1-2 秒"（v0.6 校准：4a 骨架并存不闪断；4b 实质搬迁才闪；4c 收尾基本不闪）
- 选凌晨低峰窗口（UTC 18:00 = 北京 02:00）
- 发布后 30 分钟内观察 dashboard 重连日志，确认无 client 卡死

### 8.3 4 小时冻结窗口

参照 router-split RFC v4 的成熟做法：

- **Phase 1 / 4b / 5（大刀）**需要冻结窗口（v0.6 校准：4a/4c 不需要冻结，4b 才是大刀）
- 推前在群里通知："今晚 X 点起 4 小时窗口，期间 server 包 / wshub 不要新提 PR；遇到紧急 fix 喊我"
- 4 小时内推完合入；超时则暂停于最近 phase 完成处，第二天续推
- **Phase 2 / 3a-3f / 4a / 4c 不需要冻结**（每 PR 1500 行内，正常 review）

---

## 九、CI / 验收 / 防膨胀（**v0.2 整合**）

### 9.1 每 Phase 验收清单

```
- [ ] go build ./...
- [ ] go test -race -count=1 ./... 全绿
- [ ] go vet ./...
- [ ] gofmt -l internal/ 输出空
- [ ] AST linter 通过（rule 1-5 全部 warn 或 fail，按 §6.2.0.4 阶段化交付表）
- [ ] routes_snapshot_test.go 通过（除非显式更新 golden）
- [ ] PR description 给出跨包字段引用减少数 ≥ 5
- [ ] PR commit message 含 `Closes-exemption: <file>` trailer（v0.5 rule 5 强制）
- [ ] 双 commit 中每个 commit 独立 build/test 绿（pre-push hook 强制；v0.5 §6.3）
- [ ] 手工冒烟（按 [docs/ops/phase4-smoke-test.md](../ops/phase4-smoke-test.md) 一致 checklist 跑；PR 附截图链接）
```

**Phase 4a / 4b / 4c 验收 gate** — **唯一权威列表见 §6.5 各小节末尾的"4x 验收 gate"**（v0.6 修订：避免 §9.1 与 §6.5 双处维护漂移；本节只列摘要）：

- 4a：rule 3a warn / 子包 build 绿 / exemptions wshub.go 仍保留 — 详 §6.5 §4a 验收 gate
- 4b：**rule 3b warn + race count=100 + dispatch.MessageQueue import 0 / exemptions wshub.go 删除** — 详 §6.5 §4b 验收 gate
- 4c：rule 3a/3b 切 fail / wshub*.go 0 匹配 / exemptions agent_tailer.go 删除 — 详 §6.5 §4c 验收 gate

**Phase 5 增加**：

```
- [ ] mutex profile 跑过 + 入档 docs/ops/lock-profile-2026-XX.md（v0.5 §十二.2 钉死）
- [ ] scratchPool 三角验证脚本通过（详 §6.7）
- [ ] Phase 5 → final 14 天观察期（v0.6 §7.3 加：双倍观察期，mutex profile 数据采集需稳定流量）
```

最终 Phase 5 后（验证总目标达成）：

```
- [ ] wc -l internal/server/*.go | tail -1   ≤ 5000
- [ ] Server 字段数 ≤ 12（脚本验证）
- [ ] Hub 字段数 ≤ 40 且按 §五字段块（7 块）分组组织（v0.6 校准；v0.4 写"≤ 30"基于错误的 37 baseline；v0.5 改"≤ 35"基于 43 字段；v0.6 实测 47 字段下"≤ 40"才现实）
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
| Hub Shutdown 在 wshub 抽包后死锁 | 高 | Phase 4b 必加 `hub_concurrency_test.go -race -count=100`；Drain(ctx) 5s 超时；lifecycle 块跨块写豁免（§五）|
| handler 隐式依赖 Server 字段被遗漏 | 高 | Phase 0 字段读写注解；Deps 接口化强制每个引用进 Deps；编译期错误立刻暴露 |
| 51 路由路径漂移 | 低 | `routes_snapshot_test.go` golden gate（v0.6 校准：50→51 条）|
| 跨 phase 合并冲突 | 高 | 每 PR ≤ 1500 行 / 1-2 天合一个；冻结期不接 dashboard 新功能 PR |
| Phase 4b / 5 生产 dashboard 闪断 | 中 | §八.2 凌晨低峰发布 + 主动通知 + 重连观察 |
| 抽完 2 年再膨胀 | 中（历史已发生）| AST linter rules 1-5 + 文件大小硬上限 + 包契约文档（§九.2）|
| 单 phase 出问题难独立回滚 | 中 | tag 策略（含 4a/4b/4c）+ 3a-3d 独立可回滚 / 3e-3f 串行依赖说明（§七）|
| reviewer 节奏拖慢整体 | 中 | **13 个 PR** 每个 ≤ 1500 行（4b/4c 例外，4a 不超线），单 PR review ≤ 30 分钟；3a-3d 可并发 review |
| CI 时间超 600s | 低 | §九.3 fast/full 双轨方案 |
| 豁免债积累不清 | 高（v0.5 新增）| linter rule 5 stale_exemption + Closes-exemption commit trailer |
| 业务逻辑分散到 6 子包后 i18n 改造成本放大 | 中 | §十二.1 显式声明 Phase 7 业务逻辑提取层路径 |

---

## 十一、成本-收益论证（v0.2 新增 / **v0.3 钉 ROI gate** / **v0.5 改历史可回测**）

> 工程 reviewer 反馈：v0.1 没算清楚机会成本。
> v0.3 reviewer 反馈：v0.2 数字与 baseline 倒装（先承诺再采数据）。本节为**承诺范围**。
> v0.5 reviewer 反馈：ROI gate "月均冲突时间"在 squash merge 仓库无法采集。v0.5 改用历史可回测指标。

### 成本

- **工时**：1-2 人 × 13-15 周 = ~65-150 人天（v0.5 修订）。其中纯编码 ≈ 39 工作日（v0.6 同步：13 PR × 3 工作日 = 39，比 v0.5 的 33 多 6 因为 4a/4b/4c 拆分）；其余是观察期、review、部署、incident 缓冲。v0.4 估"60-120 人天"基本对，但日历周数从"6-8 周"拉到"13-15 周"才符合 7 天观察期 × 13 个 phase 的物理时间
- **机会成本**：拆分窗口期 dashboard / cron 两热区接新功能必须接受合并冲突或排期排到 Phase 5 之后。**保守估计延期 4-8 周**新功能上线
- **CI 时间**：从 ~300s → ~450s（详见 §九.3），单次 PR 等待时间 +50%
- **学习成本**：6 个 dashboard 子包 + wshub 包，新人加入熟悉成本 +1 天

### 收益（量化；v0.6 同步 master 实测）

| 项 | 现状 | Phase 5 后 | 量化收益 |
|---|---|---|---|
| server 包大小 | **21313 行**（v0.6；v0.4 时 17143） | ≤ 5000 行 | -77%（v0.4 写 -71% 基于 17143）|
| server 包文件数（不含测试）| **58 个**（v0.6；v0.4 时 40） | ≤ 15 个 | -74% |
| 单文件 1000+ 行数量 | **6 个**（v0.6；v0.4 时 5 个） — dashboard_session/project_files/dashboard_send/dashboard_cron/dashboard_cron_transcript/server | 0 个 | 解 god file |
| 单文件 800+ 行数量 | **9 个**（v0.6；v0.4 时 6 个） — 上述 6 + wshub.go(902) / dashboard.go(852) / agent_tailer.go(827) | 0 个 | 长效防膨胀 |
| Server struct 字段 | 47 | ≤ 12 | -74% 跨包字段引用 |
| Hub struct 字段 | **47**（v0.6） | ≤ 40 | -15% |
| 跨包小接口数量 | ~3 个（HubRouter / cronHubOps / 等） | ~12 个（每 dashboard 子包 2-3 个） | 测试 mock 难度 -60% |
| 并行开发上限 | 1（任意 dashboard PR 撞同一文件） | 6+（6 个独立子包 + wshub） | 并行度 6× |
| dashboard 新功能 PR 平均冲突时间 | 30-90 min（实测过去 3 个月） | < 10 min | 月省 ~5-10 工程小时 |
| AST linter 防膨胀 | 无 | 强制（rules 1-5）| 阻断"再膨胀 5×"的历史复演 |

### 何时这个 trade-off 反转

- 如果未来 6 个月内 dashboard 不接新功能（项目转向纯后端） → 收益打折，应推迟 Phase 4
- 如果团队从 1-2 人扩到 5+ 人 → 并行开发收益翻倍，应提前 Phase 4

### Phase 0 后 ROI gate（**v0.5 改为历史可回测指标**）

> **v0.4 错点**：ROI gate 用"月均跨文件冲突时间 ≥ 15 分钟"——项目走 squash merge（无 merge commit），冲突时间在历史里**不存在**。要测这个指标只能"再等 14 天采集"，但 14 天内若没人开 dashboard PR baseline 仍是 0，导致误判推迟 Phase 1。
>
> **v0.5 解法**：改用 **历史可回测指标**（git log 直接挖），Phase 0 merge 后立刻能跑，不用等。

#### 时间轴（v0.5 简化）

```
T+0    Phase 0 PR merge
T+0    立即跑 ROI gate 历史回测（数据已存在 git log 里）
       通过 → Phase 4a PR review + merge → 4b → 4c → Phase 1 准备 → ...
       不通过 → Phase 4 PR close（保留 branch 备查）；Phase 1-5 进 NEEDS-DESIGN
```

> v0.4 的 14 天等待窗口废除——历史指标一跑就出，不需要"再观察"。

#### 决策 gate（v0.5 历史可回测指标）

**指标定义**（基于过去 90 天 git log，squash merge 模式可挖）：

```bash
# 指标 H1: 6 个热文件每个被多少独立 PR 改
for f in dashboard_cron.go dashboard_send.go dashboard_session.go \
         wshub.go project_files.go server.go; do
  prs=$(git log --since=90.days.ago --format='%s' -- "internal/server/$f" \
        | awk -F'\\(#' 'NF>1{split($NF,a,")"); print a[1]}' | sort -u | wc -l)
  echo "$prs PRs / 90d → $f"
done
```

**v0.6 实测 baseline（2026-05-28, origin/master HEAD `44a10e8d`）**：

```
31 PRs / 90d → dashboard_cron.go
17 PRs / 90d → dashboard_send.go
19 PRs / 90d → dashboard_session.go
32 PRs / 90d → wshub.go
25 PRs / 90d → project_files.go
28 PRs / 90d → server.go
```

6 个热文件每个 17-32 PR / 90d，**远超 ROI 临界点**。

**决策 gate**（v0.5）：

- ✅ **任意 ≥ 3 个 server 包文件 PRs/90d ≥ 15** → 启动 Phase 4a merge → Phase 1（**v0.6 实测 6/6 满足**，gate 立即通过）
- ⚠️ **2 个文件 ≥ 15 PRs / 90d** → Phase 1-5 推迟，Phase 0 lint gate 保留作为长期治理
- ❌ **0-1 个文件 ≥ 15 PRs / 90d** → 取消整个 Phase 1-5；Phase 0 投入算"防膨胀工具"独立交付

**为什么这个指标能替代"冲突时间"**：
- 文件被 N 个独立 PR 改 ≈ N-1 次潜在 merge 冲突机会；squash merge 抹去冲突记录但不抹去 PR 引用
- 文件越被频繁改、潜在并行冲突越大；阈值 15 = 双周一次跨 PR 改动
- 不依赖 baseline 采集窗口：Phase 0 merge 后**立即可跑**，决策不再卡 14 天等待

**仍需采集（不上 gate，仅观察）**：
- Phase 0 merge 后 14 天内观察是否有人尝试在 wshub.go / dashboard_cron.go 同时开 PR（PR 列表手工记），用于事后印证 ROI gate 假设

#### "Phase 0 是否值得"反推

如果 ROI gate 不达标会 close Phase 4 PR — 那 Phase 0 的投入是不是浪费？

**不是**：Phase 0 7 项交付物中，**至少 3 项独立有价值**（即便 Phase 1-5 全推迟）：

1. **AST linter rules 1-2 + rule 5**（防新文件膨胀 + 豁免债清理）— 独立有价值，永远能用
2. **`docs/design/server-packages-contract.md`** 包契约文档 — 即便不拆分，也是 server 包不再膨胀的认知底座
3. ⚠ rules 3-4 + cross-method 契约文档 — 这两项依赖 Phase 4-5；ROI 不达标时不写
4. ⚠ routes_snapshot — 依赖后续 phase 才有意义；不达标时维持但不主动维护
5. **§0 事实速查表 + 修订纪律**（v0.6 新增）— 独立有价值，作为后续设计稿模板

所以 Phase 0 的最小可独立交付 = rules 1-2 + rule 5 + 包契约文档 + 字段读写注解 + baseline 采集机制 + 事实速查表。这 6 项即便 Phase 1-5 全黄，也值得做。

#### 写进 PR description

Phase 0 PR description 必须含两条：

```
## ROI gate 触发条件（v0.5：历史可回测，不再等 14 天）
- 本 PR merge 后立即跑 baseline.md §4 / §9 git log 历史指标脚本
- 6 个热文件 PRs/90d 实测填入 baseline.md §9
- 通过 → 同 PR 中开 Phase 4a PR；不通过 → 本 PR 仍保留（rules 1-2 + rule 5 + 包契约 + 注解 + 速查表是长期治理）

## 后续 PR 依赖
- Phase 4a PR 在本 PR merge + ROI gate 通过后即可 merge（v0.5 取消 v0.4 的"review 不 merge"双轨）
```

### 替代方案对比

| 方案 | 成本 | 收益 | 评估 |
|---|---|---|---|
| **本设计**（13 PR / 13-15 周（含观察期））| 中 | 高 | ✅ 推荐 |
| **不拆，加 lint 限单文件 800 行** | 极低 | 中（防新增膨胀但旧债不还）| 退而求其次 |
| **一次性大重构（1 PR 21000 行）** | 极高（review 不过来）| 高 | ❌ 拒绝 |
| **更激进拆分**（每路由一 handler 文件）| 高 | 低（路由表碎片化）| ❌ 拒绝 |

**结论**：本设计是 ROI 最优解，**v0.6 实测 6/6 文件超 ROI 阈值，gate 立即通过；可立即启动 Phase 0**。本结论保留为反向兜底——若项目在 Phase 0 后突然转向纯后端方向（dashboard 功能停做），即时按 §十一 决策 gate 表第 3 档取消 Phase 1-5。

---

## 十二、不解决的问题（明示遗留）

1. **dashboard handler 与业务逻辑仍混在一起**：本设计只搬位置不重构。若将来要做 i18n / 错误统一 envelope（R247-ARCH-2/3），另起 PR。

   **v0.5 显式后果说明**：Phase 4 完成后业务逻辑分散到 6 个 dashboard 子包，i18n / envelope 改造的成本会从"在 1 个 server 包统一处理"变为"在 6 个子包同时改造"。AST linter rules 1-5 只检文件大小、字段块、接口契约，**不检业务复杂度**——子包内部的业务逻辑二次膨胀对 lint 是不可见的。

   **建议路径**（Phase 6+）：
   - 待 Phase 5 完工后开 **Phase 7 RFC：业务逻辑提取层**（`internal/dashboardlogic/` 或类似），把 handler 从"HTTP 解析 + 业务逻辑 + 响应序列化"三合一，分离为：
     - `internal/dashboard/<domain>/` 仅做 HTTP 解析 + 调用 logic + 响应序列化（thin handler）
     - `internal/dashboardlogic/<domain>/` 业务逻辑（无 net/http 依赖，可单元测试）
   - i18n / 错误 envelope 在 logic 层统一处理，6 个子包通过 helper 函数复用而非每个 handler 自己实现
   - 这是 Phase 4 物理拆分**完全没解决**的二阶债，但触发条件应该是"i18n / envelope 真要落地时"，不是 Phase 4 收工就立即上

   不在本设计范围。但**承认这条债的存在**，避免 Phase 4 收工后用户误以为"server 包架构债已清"。

2. **Hub 锁分离**留作 Phase 6 优化（**v0.5 钉死监测承诺**）：本次保持 `Hub.mu` 单锁不变。**Phase 5 完工后必须执行**以下监测：

   ```bash
   # 1. 启用 mutex profile（默认关闭）
   curl -s 'http://localhost:8080/api/debug/pprof/mutex?seconds=30' > mutex.pb.gz

   # 2. 跑 dashboard 真实负载（10 浏览器 tab × 5 分钟 + 1 cron 触发流量）

   # 3. 分析 top contended 锁
   go tool pprof -top -cum mutex.pb.gz | head -10
   ```

   **判定**：
   - `*server.Hub.mu` / `*wshub.Hub.mu` 进 top 3 → **立即开 Phase 6 RFC**（锁分离：clients map 独立 RWMutex / debounce 独立 Mutex）
   - 进 top 4-10 → 30 天内开 Phase 6 RFC
   - 不进 top 10 → Phase 6 推迟，每 90 天复查一次（写入 dashboard healthcheck cron）

   v0.4 仅说"运行时 profile 显示 > 阈值时再拆"——阈值不明、时机不定。v0.5 钉死：**Phase 5 完工那天即跑 mutex pprof + 入档 docs/ops/lock-profile-2026-XX.md，作为 Phase 6 触发的 audit trail**。这一条是 Phase 5 PR 的 必交付 项之一（PR description checklist 含 `mutex profile attached`）。

3. ~~**`scratchPool` 留 Server**~~：**v0.5 推翻**——见 §6.7「scratchPool 三角问题」。Phase 5 把 sweeper 启停搬进 `*session.ScratchPool` 自管，Server 不再持字段，Hub 通过 `NewHub Options` 注入。原 v0.4 描述"Server.Shutdown 要 stop sweeper"已不成立。
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
| v0.5 | 2026-05-28 | v0.4 第五轮深度 review 11 项整改：A. Hub 字段从 37 重数为 43（漏 6：debounceFire / historyMarshalCache / userSendLimiters* / connCountByOwner*）+ §五新增第 7 块 rate-limit/cache；B. §6.7 scratchPool 三角钉死（自管 sweeper / Server 不持字段 / NewHub Options 注入）；C. §五 lifecycle 块跨块写豁免（Shutdown/Start/NewHub 写所有块，关键词 LIFECYCLE-METHOD 标注）；D. §6.3 双 commit 与 routes_snapshot 同步契约（commit a 必须含 golden 更新）；E. ROI gate 改历史可回测指标（git log 90d unique PR 数，Phase 0 merge 后立即决策，废除 14 天等待）；F. 节奏从"9-10 周"改"13-15 周"含观察期 + §7.3 重叠豁免按风险分档；G. Phase 4 拆 4a/4b/4c（骨架/方法搬迁/收尾，每刀 ≤ 2400 行）；H. linter rule 5 stale_exemption 反向依赖保护（git tag 比对 + Closes-exemption commit trailer）；I. linter rule 3 拆 3a/3b（Phase 0 文本扫描 / Phase 4 中段 AST 对账）；J. Hub.mu 锁监测承诺（Phase 5 完工跑 mutex pprof + 入档 docs/ops/lock-profile-2026-XX.md）；K. §十二.1 显式 Phase 7 业务逻辑提取层路径声明 |
| v0.6 | 2026-05-28 | v0.5 第六轮 review 9 项整改（**多处不一致**）+ 同步到 origin/master 最新 HEAD `44a10e8d` 实测数据。N1：§一 Hub 37→47 同步；N2：PR 数 11→13 同步到 §六/§十/§十一；N3：§7.3 观察期算式 11×7=77 → 13×7=91 + 加 4a/4b/4c 行；N4：§7.1 tag 列表加 server-split-phase4a/4b/4c；N5：§9.1 Phase 4b 验收 gate 单独列出（race count=100 不再笼统挂"Phase 4"）；N6：§6.7 scratchPool 主 ctx 注入显式化（NewScratchPool(ctx, ...) 接 main ctx）；N7：§6.5 4a 验收 gate 显式用 rule 3a / 4b 用 rule 3b；N8：§6.2.0.1 字段数 28→47/47；N9：§十一结论同步 v0.5 ROI gate 立即决策。**重大事实更新**：Hub 字段 43→47（v0.5 漏 4 个：auth/subscriberCount/legacySendInvokes/debounceClosedFast）；server 包从 17156→21313 行；超线文件从 6→9 个；exemptions.yaml 加 wshub.go/dashboard.go/agent_tailer.go 三个新条目；Phase 4 范围从 3550→5198 行不含测试。**§0 新增事实速查表 + 修订纪律**——根治 v0.5 多处数字漂移痼疾 |
| v0.6.1 | 2026-05-28 | v0.6 第 7 轮 review 9 项内部一致性修订（速查表自身的副作用）：N1 baseline §3 笔误"8 块"→"7 块"（实际枚举 7 块）；N2 §五代码示例"rate-limit / cache (5, v0.5 新增块)"→"v0.5 起识别"（避免误导）；N3 §十一收益表加"server 包文件数 58→≤15"+"800+ 行 9 个"两行（与 §一对称）；**N4 critical：exemptions.yaml limit 全部 800→500**（v0.6 模板与 §9.2 硬上限矛盾会让 linter 误豁免；改正后 server 包文件 limit:500 / dashboard 子包 limit:800）+ 新增 send.go / dashboard_auth.go 两个 500-800 条目（共 11 条）；N5 §6.2.0.1 措辞校准；N6 §7.3 加 Phase 5 → final 14 天独立观察期 + 总观察期 91→98 天；N7 §6.0 行数例外改 4b/4c（4a 700 行不超线，无例外）；N8 §9.1 验收清单 Phase 4a/4b/4c 改为引用 §6.5 锚点（避免双处维护漂移）；N9 §0 加 lint rule 6 备做承诺（扫 markdown 关键数字 token 与速查表对账，Phase 0 跟随 RFC，否则纪律 1-4 全靠人维护是 v0.5 痼疾的复演）|

### v0.2 整改追溯（按 reviewer 反馈映射）

| reviewer 反馈 | v0.2 修订位置 |
|---|---|
| 阻断 1：Hub 锁方案"运行时观察"不算设计 | §五：保单 struct + 单锁不变 + 字段分组；锁分离留 Phase 6 |
| 阻断 2：Deps 换汤不换药 | §四.1：强制接口化；§四.2：减耦合 KPI |
| 阻断 3：长效治理缺失 | §九.2：AST linter + 文件大小硬上限 + 包契约文档 |
| 阻断 4：Phase 3 单 PR 5650 行违反节奏 | §六：切 3a/3b/3c/3d/3e/3f 共 6 PR，每 ≤ 1500 行 |
| 阻断 5：回滚 / 灰度 / CI 预算缺失 | §七 / §八 / §九.3 三章 |
| 工程 reviewer：机会成本未算清 | §十一：成本-收益量化 + 替代方案对比 + 反转条件 |

### v0.6.1 整改追溯（按 v0.6 第 7 轮 reviewer 反馈映射）

| reviewer 反馈 | v0.6.1 修订位置 |
|---|---|
| N1: baseline §3 标题"8 块"与实际枚举 7 块不一致 | baseline §3 line 113 改为"7 块" |
| N2: §五字段示例"v0.5 新增块"措辞误导（v0.5 已是 7 块，不是新增）| §五 line 432 改为"v0.5 起识别" |
| N3: §十一收益表只列"1000+ 行 6 个"，与 §一"800+ 9 个"不对称 | §十一收益表加 server 包文件数 58→≤15、800+ 行 9 个两行 |
| **N4 critical: exemptions.yaml limit 全部 800 与 §9.2 server 包 500 硬上限矛盾** | §0.4 yaml 模板：9 个超线条目 limit:800 → limit:500；新增 send.go(703)/dashboard_auth.go(583) 两个 500-800 条目（共 11 条），加"dashboard 子包阈值 800（不是 500）"显式说明 |
| N5: §6.2.0.1 字段数措辞反向追溯链断裂（误把 v0.2→v0.4 链接到 v0.6） | §6.2.0.1 改"v0.4 时仍沿用 v0.2 时代的 28+ 粗估，v0.6 实测 47 替换" |
| N6: §7.3 重叠豁免表无 Phase 5 → final 行（mutex pprof 数据采集需稳定流量）| §7.3 加 Phase 5 → final 14 天独立观察期行 + 总观察期 91→98 天 |
| N7: §6.0 行数例外"4a/4b/4c"中 4a 700 行不超线，例外应为 4b/4c | §6.0 / §二目标 8 / §十风险表 reviewer 节奏行：例外改 4b/4c，4a 不超线显式说明 |
| N8: §9.1 Phase 4b 验收清单与 §6.5 4b 验收 gate 双处维护风险漂移 | §9.1 Phase 4a/4b/4c 改为引用 §6.5 锚点的摘要表，详细 gate 单一来源 |
| N9: §0 修订纪律 4 条全靠人维护，是 v0.5 痼疾复演风险 | §0 加纪律 5：lint rule 6 扫 markdown 关键数字 token 与速查表对账（v0.6.1 备做 / Phase 0 跟随 RFC，承诺写进 PR description）|

### v0.6 整改追溯（按 v0.5 第 6 轮 reviewer 反馈映射）

| reviewer 反馈 | v0.6 修订位置 |
|---|---|
| **L: 同步 origin/master 最新代码** | baseline 全文重写（HEAD `44a10e8d` 2026-05-28 实测）；§一/§二/§三/§5 字段示例/§6.1 phase 表/§6.5 Phase 4 行数/§6.6 字段去向表/§9.1 验收 gate/§十一收益表全部同步 |
| **M: §0 事实速查表（防多处漂移）** | §0 新增 19 行事实速查表 + 修订纪律 4 条 |
| N1: §一现状盘点 Hub 37 漏改（应同步 §二的 43→47）| §一改为"Hub 47 字段 / 7 块" |
| N2: §六标题/§十/§十一 PR 数 11 → 13 不一致 | §六标题"v0.5：方案 B 顺序 / 11 个 PR" → "v0.6：方案 B 顺序 / 13 个 PR"；§十"11 个 PR"→"13 个 PR"；§十一替代方案表"11 PR / 13-15 周"→"13 PR / 13-15 周" |
| N3: §7.3 观察期算式仍 11×7=77 / 缺 4a/4b/4c 行 | §7.3 改 13×7=91 + 加 Phase 4a/4b/4c 重叠规则三行 |
| N4: §7.1 tag 列表没加 4a/4b/4c | §7.1 tag 列表分别加 server-split-phase4a/4b/4c + §7.2 加 4a/4b/4c 独立回滚边界 |
| N5: §9.1 Phase 4 race count=100 笼统挂"Phase 4" | §9.1 拆为"Phase 4b 增加"（race count=100）+ "Phase 5 增加"（mutex profile + scratchPool 验证）|
| N6: §6.7 主 ctx 注入未显式 | §6.7 加第 4 件事"main ctx vs Hub.ctx 区分"+ NewScratchPool(ctx, ...) 接 main ctx 显式 + 验证脚本 |
| N7: §6.5 4a 用 rule 3a 还是 3b 不明 | §6.5 4a 验收 gate 改"linter rule 3a 启用 warn 模式"；4b "rule 3b AST 对账"；4c "rule 3a + 3b 切 fail 模式" |
| N8: §6.2.0.1 字段数还是 28（v0.2 时代）| §6.2.0.1 改"Server 47 字段 / Hub 47 字段" |
| N9: §十一结论与 v0.5 立即决策矛盾 | §十一结论改"v0.6 实测 6/6 超 ROI 阈值，gate 立即通过；可立即启动 Phase 0" |

### v0.5 整改追溯（按 v0.4 第 5 轮 reviewer 反馈映射）

| reviewer 反馈 | v0.5 修订位置 |
|---|---|
| A：Hub 字段 37 漏 6 个（baseline 错） | baseline §3 重数为 43 + 新增 rate-limit/cache 块 / design §五代码示例同步 / §九.1 验收 gate"≤ 30"调整为"≤ 35" |
| B：scratchPool 三角矛盾（§十二.3 vs baseline §2 vs 代码 dashboard.go:388） | §6.7 新增「scratchPool 三角问题处理」+ §十二.3 推翻 + baseline §2 字段去向表迁移 |
| C：lifecycle 块跨块写规则未豁免 Shutdown/Start | §五新增 v0.5 lifecycle 块跨块写豁免 + LIFECYCLE-METHOD godoc 关键词 |
| D：双 commit 策略下 routes_snapshot golden 同步规则缺失 | §6.3 新增「双 commit 与 routes_snapshot 同步契约」+ pre-push hook 强制每 commit 独立 build/test 绿 |
| E：ROI gate"月均冲突时间"在 squash merge 仓库无法采集 | §十一改用 git log 90d unique PR 数指标（实测 17-32 PRs/file 全部超线）+ baseline §9 同步 |
| F：9-10 周节奏对 1-2 人团队过乐观（11 PR × 7 天 = 77 天观察期） | 改为 13-15 周（含观察期）+ §7.3 重叠豁免按风险等级分档表 |
| G：Phase 4 实际 4815 行（不含测试）超 1500/PR 上限 4 倍 | §6.5 拆 Phase 4 为 4a/4b/4c（骨架/方法搬迁/收尾）+ §6.1 phase 表从 11 PR 改 13 PR |
| H：exemptions until_phase 反向依赖未保护 | §6.2.0.4 新增 rule 5 stale_exemption + Closes-exemption commit trailer 强制 |
| I：linter rule 3 实现复杂度低估 | §6.2.0.4 拆 rule 3 为 3a（godoc 文本扫描）/ 3b（AST 字段访问对账） |
| J：§十二.2 锁分离阈值不明、时机不定 | §十二.2 钉死 Phase 5 完工跑 mutex pprof + 阈值（top 3 立刻 RFC / top 4-10 30 天内 / 不入 top 10 90 天复查） |
| K：业务逻辑分散到 6 个子包后 i18n 改造成本会被放大 | §十二.1 显式声明 Phase 7 业务逻辑提取层路径（thin handler + dashboardlogic 子包） |

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
