# ARCH-CONSUMER-IF — 消费端小接口替换具体 Router 指针

| 字段 | 值 |
| :--- | :--- |
| 状态 | Implemented (v3) |
| 作者 | naozhi team |
| 创建日期 | 2026-05-10 |
| 修订日期 | 2026-05-11（v3：按落地实测 + 第二轮 review 修订方法数与 typed-nil 防御） |
| 关联代码 | `internal/session/router.go`<br/>`internal/dispatch/dispatch.go:39-70`（`*Dispatcher.router`）<br/>`internal/server/wshub.go:38-140`（`*Hub.router`）<br/>`internal/upstream/connector.go:67-95`（`*Connector.router`）<br/>`internal/cron/scheduler.go:60-85`（已有参考实现） |
| 关联 RFC | `docs/rfc/key-resolver.md`（正交推进）<br/>延后：ARCH-ROUTER-SUBAGGREGATE（暂未立 ticket）<br/>延后：ARCH-SERVER-ROUTER-IF（Phase 2.5，Server/Handlers 迁移；本 RFC 非目标） |

## 0. 修订历史

### v3(2026-05-11)

落地后第二轮 review 发现：

- **B1 (dispatch) — typed-nil interface 陷阱已修**：`NewDispatcher` 把
  `cfg.Router (*session.Router)` 直接赋给接口字段会产生 typed-nil；
  `discardQueue` 的 `if d.router != nil` 变成恒真。v3 代码加 nil-guard
  让接口字段保持 untyped nil；新增 `TestNewDispatcher_NilRouterStaysUntypedNil`
  回归锁。
- **B2 (HubRouter 方法数) — RFC 正文更新为 14 方法**：v2 §2.1.2 / §3.2.2
  漏掉 `dashboard_scratch.go` / `dashboard_send.go` 里
  `h.hub.router.X()` 透传（*ScratchHandler / *SendHandler 共用 Hub
  的 router 字段句柄）。真实 HubRouter 接口是 14 方法（多了 Remove /
  RenameSession / GetWorkspace）。v3 §2.1.2 / §3.2.2 / §7.2 全部更新
  为 14 方法、§7.2 "≤15 阈值" 余量改为 1。
- **D2 (Connector 方法数表格 8 vs 9 不一致) — 已对齐 9**：§2.1.3 表列
  了 8 行 `c.router.X()` 调用 + 1 处构造期 `router.DefaultWorkspace()`
  = 接口 9 方法。v3 正文统一描述。
- **S1 (upstream.New 形参未改接口类型) — 刻意保留**：RFC v2 §4.5 约定
  "构造站点改动最小化"，让 cmd/naozhi 继续传具体指针。v3 §6.3 补
  注释说明"形参保留具体类型是 accept 的设计决策，不是遗漏"。
- **S3 (contract_test.go 盲点) — 补一行注释**：它只能防 Router-side
  漂移，不能防消费者接口 silently 删方法；v3 文件头新增文档。
- NIT：N1 方法数更新、N3 加 RFC 链接 — 轻量落盘。

### v2（2026-05-11）

按 2026-05-10 对 v1 的 reviewer findings 重写：

- **B1 / B2 / B3 修复**：重做所有方法清单。v1 的 §2.1 "Dispatcher 26 方法" 中 18 个是虚构的；"Hub 11 方法" 把 `*SessionHandlers` / `*ProjectHandlers` / `*HealthHandler` / `*CLIBackendsHandler` 的调用误算入 Hub；"Connector" 多列 3 个未用方法（Reset / Version / RegisterForResume）。v2 的清单全部来自 `grep '{d,h,c}\.router\.'` 的真实结果。
- **B7 修复**：`SetOnChange` 归 Server（`dashboard.go:273` 是 `s.router.SetOnChange`），v1 误放到 `HubNotifier`。v2 删除 `HubNotifier`；`SetOnChange` 的接口迁移归入 Phase 2.5（非本 RFC 范围）。
- **D1 采纳**：方法数塌缩后（Dispatcher 8 / Hub 11 / Connector 9），拆 3 面的成本/收益失衡。v2 按 `cron.SessionRouter` 的既有范式，三个消费者**各自定义单个接口**。
- **B4 修复**：`cron.SessionRouter` 实际 4 方法，v1 正文写 3。
- **B5 修复**：§5 测试示范改成最小可编译 snippet（或复用现有 `newTestDispatcher` 手法），同时承认 `connector_test.go:319` 已有 fetch_events 测试，改成"从 stub 迁移到 interface fake"。
- **B6 修复**：`EventEntriesSince` 方法名修正（v1 写 `EventEntries`）。
- **S3 采纳**：§7 新增 "签名漂移防护"——Phase 1 同时落地 `internal/session/contract_test.go` 绑定三个消费者接口到 `*session.Router`，把漂移检测从肉眼 review 升级成 `go test`。
- **S4 / S5 采纳**：Phase 步骤补 "audit nil-guard" 和 "audit `*_test.go` 的 `router` 字段访问"。
- **验收条目**：§11 的 grep 表达式改用 `rg` + 单引号；"cmd/naozhi ≤ 5 行" 改 "≤ 2 行"。

v1 原文已被完全重写；历史查阅 `git log`。

---

## 1. 目标 & 非目标

### 1.1 目标

1. 让 `dispatch.Dispatcher` / `server.Hub` / `upstream.Connector` 三个 Router 消费者**各自在本包内**声明一个接口，把 `router *session.Router` 字段替换为该接口类型。
2. Router（`*session.Router`）**隐式**满足这些接口（Go 的 structural typing）。不改任何方法签名、不加新方法、不改 Router 内部。
3. 让三个消费者的关键路径（Dispatcher.ownerLoop、Hub.completeSubscribe、Connector fetch_events）可用 fake 接口注入做单测，从 "起真 Router" 的 ~50-200ms 启动成本降到 µs 级。
4. 为未来 Router 拆子聚合创造零改动接入点——拆分发生时三个接口字段各自替换实现即可。

### 1.2 非目标

- **不拆 Router**。Router 仍是单一 struct；拆分属于 ARCH-ROUTER-SUBAGGREGATE。
- **不改任何方法签名**。
- **不引入新的 session 状态 / 生命周期 hook**。
- **不碰 `cron.SessionRouter`**，它已经是该模式的最早实例。
- **不迁移 Server / Handlers 的 `s.router` 访问**（`server.go` / `takeover.go` / `dashboard.go:273` `SetOnChange` / `dashboard_session.go` / `project_api.go` / `dashboard_agent_events.go` / `health.go` / `dashboard_cli.go`）。这 7 个 receiver 各持 `router *session.Router`，接口迁移面大、与 Hub 解耦不紧；独立为 **Phase 2.5 / ARCH-SERVER-ROUTER-IF**，非本 RFC 范围。
- **不使用 gomock / mockery**。接口方法数 8-11，手写 fake 更轻。
- **不引入大一统接口**。v1 讨论过后拒绝（§4.2）；v2 沿用。

---

## 2. 背景

### 2.1 真实方法清单（grep 结果）

下表由下列命令得到，**不含虚构**：

```bash
rg 'd\.router\.' internal/dispatch/      # Dispatcher
rg 'h\.router\.' internal/server/        # 所有 h.router；需按 receiver 过滤
rg 'c\.router\.' internal/upstream/      # Connector
```

#### 2.1.1 Dispatcher（`*Dispatcher.router` 字段，`internal/dispatch/dispatch.go:40`）

| 方法 | 调用点 |
| :--- | :--- |
| `GetOrCreate` | `dispatch.go:518` |
| `GetSession` | `dispatch.go:374` |
| `Reset` | `commands.go:257, 263, 310` |
| `ResetChat` | `commands.go:611` |
| `GetWorkspace` | `commands.go:106, 575` |
| `SetWorkspace` | `commands.go:610` |
| `InterruptSessionViaControl` | `dispatch.go:302, commands.go:159` |
| `NotifyIdle` | `dispatch.go:358, 405` |

**合计 8 方法**，13 处调用点。

#### 2.1.2 Hub（`*Hub.router` 字段，`internal/server/wshub.go:58`）

按 receiver 为 `*Hub` 的源文件过滤（`wshub.go` / `wshub_agent.go` / `send.go`）得到 11 个直接调用方法。另有 3 个方法来自 *ScratchHandler / *SendHandler 透传（这些 handler 持 `h.hub` 引用，通过 `h.hub.router.X()` 间接访问）—— 必须纳入 `HubRouter` 接口，否则编译断裂。

注意 `dashboard_session.go` / `project_api.go` / `health.go` / `dashboard_cli.go` 等文件里的 `h.router.` 是 `*SessionHandlers` / `*ProjectHandlers` / `*HealthHandler` 等 **不同 struct** 的 receiver，不属于 Hub（留给 ARCH-SERVER-ROUTER-IF Phase 2.5）。

| 方法 | 调用点 |
| :--- | :--- |
| `GetOrCreate` | `send.go:484, 523` |
| `GetSession` | `wshub.go:499, 504, 1058 · wshub_agent.go:133 · send.go:87, 197` |
| `Remove` | `dashboard_scratch.go:294, 301, 316` (via `h.hub.router`) |
| `RenameSession` | `dashboard_scratch.go:311` (via `h.hub.router`) |
| `ResetAndDiscardOverride` | `send.go:203` |
| `GetWorkspace` | `dashboard_send.go:329, 949` (via `hub.router` / `h.hub.router`) |
| `SetWorkspace` | `send.go:246` |
| `SetSessionBackend` | `send.go:265` |
| `DefaultWorkspace` | `send.go:277` |
| `RegisterForResume` | `send.go:279` |
| `InterruptSession` | `send.go:564` |
| `InterruptSessionSafe` | `wshub.go:836` |
| `InterruptSessionViaControl` | `send.go:326` |
| `NotifyIdle` | `send.go:385, 592` |

**合计 14 方法**，19 处调用点（含 3 处 *ScratchHandler / *SendHandler 经 Hub 透传）。

#### 2.1.3 Connector（`*Connector.router` 字段，`internal/upstream/connector.go:71`）

| 方法 | 调用点 |
| :--- | :--- |
| `GetOrCreate` | `connector.go:693` |
| `GetSession` | `connector.go:509, 630, 1089, 1110, 1128` |
| `ListSessions` | `connector.go:583` |
| `Remove` | `connector.go:1011` |
| `ResetAndRecreate` | `connector.go:959` |
| `Takeover` | `connector.go:824` |
| `InterruptSessionSafe` | `connector.go:1029` |
| `SetUserLabel` | `connector.go:1054` |

**合计 8 方法**，12 处调用点，都是运行时 `c.router.X()`。

另有构造期 `router.DefaultWorkspace()` 在 `connector.go:93` 用到（形参为 `*session.Router` 直接调用，不是 `c.router` 字段）。本 RFC 把 `DefaultWorkspace` 也纳入接口，让 Connector 完全通过接口访问 router——否则 `New()` 的签名还得保留具体类型，测试拆不干净。最终 Connector 接口 **9 方法**。

### 2.2 已有参考：`cron.SessionRouter`

`internal/cron/scheduler.go:67-85` 已经实现了此模式，**4 个方法**：

```go
type SessionRouter interface {
    RegisterCronStub(key, workspace, lastPrompt string)
    RegisterCronStubWithChain(key, workspace, lastPrompt string, chainIDs []string)
    Reset(key string)
    GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
}
```

生产传 `*session.Router` 满足接口；测试传 fake。本 RFC 把同样手法扩展到另外三个消费者。

---

## 3. 设计

### 3.1 设计原则

- **"Accept interfaces where they are used"**。接口在消费者包里声明，不在 `session` 包。
- **单一接口 / 按消费者**。每个消费者一个接口，大小对标方法清单（Dispatcher 8 / Hub 11 / Connector 9）。不拆多面子接口——方法数不足以让拆分带来的"语义清晰"收益压过"测试/构造样板增加"的代价。Connector 8-9 在 v1 就是单面；v2 让 Dispatcher 和 Hub 同构。
- **命名用领域名词**。`dispatch.SessionRouter` / `server.HubRouter` / `upstream.SessionRouter` ——遵循 `cron.SessionRouter` 的前例。命名带消费者包名做前缀避免 grep 混淆。
- **零 Router 改动**。Router 方法签名保持不变，结构隐式满足三个接口。
- **签名漂移由 `go test` 拦截**（§7.3）。

### 3.2 三个接口定义

#### 3.2.1 `internal/dispatch/consumer.go`（新文件）

```go
package dispatch

import (
    "context"

    "github.com/naozhi/naozhi/internal/session"
)

// SessionRouter is the subset of *session.Router that Dispatcher uses.
// Defined here (consumer side) so Router can evolve independently and
// tests can inject a fake without wiring a full router graph.
//
// *session.Router satisfies this interface implicitly (Go structural
// typing). Do NOT import this interface from the session package.
type SessionRouter interface {
    GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
    GetSession(key string) *session.ManagedSession
    Reset(key string)
    ResetChat(chatKeyPrefix string)
    GetWorkspace(chatKey string) string
    SetWorkspace(chatKey, path string)
    InterruptSessionViaControl(key string) session.InterruptOutcome
    NotifyIdle()
}
```

Dispatcher struct（`dispatch.go:39`）字段从 `router *session.Router` 改为 `router SessionRouter`；`DispatcherConfig.Router *session.Router` 保留，构造时直接赋给字段（同一指针隐式满足接口）。

#### 3.2.2 `internal/server/consumer.go`（新文件）

```go
package server

import (
    "context"

    "github.com/naozhi/naozhi/internal/session"
)

// HubRouter is the subset of *session.Router that *Hub uses — strictly
// the receiver *Hub, excluding *SessionHandlers / *ProjectHandlers /
// *HealthHandler / *CLIBackendsHandler and the Server struct itself,
// which are deferred to ARCH-SERVER-ROUTER-IF (non-goal of this RFC).
//
// SetOnChange is NOT here because it is called from Server
// (dashboard.go:273) not Hub. ManagedExcludeSets / ListSessions /
// Stats / Version / MaxProcs / CLIName / CLIVersion / Remove /
// SetUserLabel / DefaultBackend / BackendIDs / BackendWrapper /
// DiscoveryExcludeIDs / EventLogStats / AttachmentTrackerStats /
// CLIPath / BumpVersion / ResetAndRecreate are all used by the
// non-Hub receivers listed above, not by Hub itself.
type HubRouter interface {
    GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
    GetSession(key string) *session.ManagedSession
    ResetAndDiscardOverride(key string)
    SetWorkspace(chatKey, path string)
    SetSessionBackend(key, backend string)
    DefaultWorkspace() string
    RegisterForResume(key, sessionID, workspace, lastPrompt string) (effectiveKey string)
    InterruptSession(key string) bool
    InterruptSessionSafe(key string) session.InterruptOutcome
    InterruptSessionViaControl(key string) session.InterruptOutcome
    NotifyIdle()
}
```

`*Hub.router` 字段类型改为 `HubRouter`；`HubOptions.Router *session.Router` 保留，构造期同一指针赋给字段。

注：`HubOptions.Router` 在 v2 **保留**为具体类型是故意的——`server` 包的其他 struct（`SessionHandlers` 等）仍然持 `*session.Router`，`HubOptions` 从同一个具体指针派生接口视图更直接。Phase 2.5 迁移 Server 时可以再把 `HubOptions.Router` 也改成接口。

#### 3.2.3 `internal/upstream/consumer.go`（新文件）

```go
package upstream

import (
    "context"

    "github.com/naozhi/naozhi/internal/session"
)

// SessionRouter is the subset of *session.Router that Connector uses
// when translating primary-reverse RPC into local router operations.
type SessionRouter interface {
    GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
    GetSession(key string) *session.ManagedSession
    ListSessions() []session.SessionSnapshot
    Remove(key string) bool
    ResetAndRecreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, error)
    Takeover(ctx context.Context, key, sessionID, cwd string, opts session.AgentOpts) (*session.ManagedSession, error)
    InterruptSessionSafe(key string) session.InterruptOutcome
    SetUserLabel(key, label string) bool
    DefaultWorkspace() string
}
```

`Connector.router` 字段类型改为 `SessionRouter`；`upstream.New(...)` 的第二个形参从 `*session.Router` 改为 `upstream.SessionRouter`。`cmd/naozhi/main.go:730` 的 `upstream.New(cfg.Upstream, router, projectMgr)` 无需改动（`router` 仍然满足新签名）。

### 3.3 为什么不合并成一个跨包接口

- `dispatch.SessionRouter` / `server.HubRouter` / `upstream.SessionRouter` 三者交集只有 `GetOrCreate` + `GetSession`。强行抽 `session.Core` 子接口 → 回到 v1 §4.1 的陷阱（session 包要知道消费者）。
- 重复声明成本 = 每个接口 8-11 行；跨包接口的演化成本（任何方法签名改动都要下游同步）远高于此。
- 接口定义与消费者编辑属于同一 PR 边界，合并无收益。

### 3.4 为什么不拆子接口（放弃 v1 的 Lifecycle/Reader/Controller 拆法）

v1 §3.2.1 曾提议 Dispatcher 拆 3 面。按真实数据（§2.1.1）：

| 拟拆面 | v2 真实方法 | 数量 |
| :--- | :--- | :--- |
| Lifecycle | GetOrCreate, Reset, ResetChat | 3 |
| Reader | GetSession, GetWorkspace | 2 |
| Controller | SetWorkspace, InterruptSessionViaControl, NotifyIdle | 3 |

每面 2-3 方法的拆分意义不大，反而：

- Dispatcher struct 从 1 字段 → 3 字段
- `NewDispatcher` 构造从 1 行赋值 → 3 行
- 每个单测 fake 要独立实现 3 个接口
- `SetWorkspace` vs `GetWorkspace` 按读写分面 vs 按领域（workspace）分面哪个更对没有定论

综合 reviewer D1 的建议：**按 `cron.SessionRouter` 范式，消费者单接口**。方法数到 15+ 时再拆，触发点写入 §7.4。

---

## 4. 关键决策的 why-not 备选

### 4.1 为什么接口不放 `session` 包

见 v1 § 4.1 / v2 §3.3。方向错误 + 演化耦合。

### 4.2 为什么不共享跨消费者大接口

见 §3.3。

### 4.3 为什么方法在多个消费者接口里重复声明（GetOrCreate / GetSession 等）

允许重复。Router 作为单一具体实现是 "trust anchor"；只要它满足所有接口，消费者之间的签名漂移由 §7.3 的 `contract_test.go` 捕获。

### 4.4 为什么不使用 gomock / mockery

对 8-11 方法手写 fake 需 ~30-50 行，比维护 mock 配置轻。手写允许 "未用到的方法 panic('not used')" 的显式信号，gomock 的 `.AnyTimes()` 会稀释此信号。

### 4.5 为什么构造期保留 `DispatcherConfig.Router *session.Router` 等具体类型

消费者包**不**应该反过来强制 `cmd/naozhi` 提供接口类型——`cmd/naozhi` 已经持有具体 `*session.Router`。保留具体类型让构造站点改动最小化（`cmd/naozhi/main.go:730` 不改）。字段类型在消费者内部才是接口。

---

## 5. 测试价值兑现

### 5.1 Dispatcher `sendAndReply` 错误路径（Phase 1 示例）

`internal/dispatch/dispatch.go:518` `d.router.GetOrCreate(...)` 返回 `session.ErrMaxProcs` 时走到 `dispatch.go:531` 的用户态回复分支（"容量不足，稍后再试"）。这条分支当前几乎没有单测，因为起真 Router 构造 "满载" 状态代价大。

该包已有 `newTestDispatcher` helper（`internal/dispatch/dispatch_test.go` 顶层），只需把它的 `router` 字段类型改为接口 + 在 helper 里默认注入一个只实现必要方法的 fake：

```go
// internal/dispatch/consumer_fake_test.go（新）
package dispatch

import (
    "context"

    "github.com/naozhi/naozhi/internal/session"
)

// fakeSessionRouter 是 test-only 的 SessionRouter 实现。未设置的
// 方法都 panic，强制测试显式声明其路径依赖。
type fakeSessionRouter struct {
    getOrCreate func(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error)
    getSession  func(key string) *session.ManagedSession
}

func (f *fakeSessionRouter) GetOrCreate(ctx context.Context, key string, opts session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error) {
    if f.getOrCreate == nil {
        panic("fakeSessionRouter.GetOrCreate not configured")
    }
    return f.getOrCreate(ctx, key, opts)
}
func (f *fakeSessionRouter) GetSession(key string) *session.ManagedSession {
    if f.getSession == nil {
        return nil
    }
    return f.getSession(key)
}
func (f *fakeSessionRouter) Reset(string)                                { panic("not configured") }
func (f *fakeSessionRouter) ResetChat(string)                            { panic("not configured") }
func (f *fakeSessionRouter) GetWorkspace(string) string                  { panic("not configured") }
func (f *fakeSessionRouter) SetWorkspace(string, string)                 { panic("not configured") }
func (f *fakeSessionRouter) InterruptSessionViaControl(string) session.InterruptOutcome {
    panic("not configured")
}
func (f *fakeSessionRouter) NotifyIdle() { /* no-op OK */ }
```

新增测试示例（示意，完整测试依赖 `newTestDispatcher` 的其他必填字段，见 §7.2 的 audit 清单）：

```go
func TestSendAndReply_MaxProcsRepliesBusy(t *testing.T) {
    d := newTestDispatcher(t)
    d.router = &fakeSessionRouter{
        getOrCreate: func(context.Context, string, session.AgentOpts) (*session.ManagedSession, session.SessionStatus, error) {
            return nil, 0, session.ErrMaxProcs
        },
    }
    // ... 触发一条消息 → assert 用户收到"容量不足"文本
}
```

**以前为什么写不出来**：`*session.Router` 要 wire `cli.Wrapper` + `shim.Manager` + `eventLogPersister` + tmp dir + workspace；写一次集成测试 50-200ms。
**现在为什么能**：fake 1 方法配置、无 I/O、µs 级。

### 5.2 Hub `handleAgentSubscribe` 的 not_found 路径（Phase 2 示例）

`wshub_agent.go:133` `sess := h.router.GetSession(msg.Key)` 返回 nil 时回客户端 not_found。接口化后可注入 fake router：

```go
type fakeHubRouter struct {
    sessions map[string]*session.ManagedSession
}

func (f *fakeHubRouter) GetSession(key string) *session.ManagedSession { return f.sessions[key] }
// 其他 10 个方法 panic("not used in this test")
```

测试触达 not_found 分支、µs 级无 I/O。

### 5.3 Connector fetch_events（Phase 3 示例）

`internal/upstream/connector_test.go:319` 已有 `TestHandleRequest_FetchEvents` 测试组，使用简化的 stub router。**v2 不是从零新增测试**，是把现有测试的 stub 迁移到 `upstream.SessionRouter` 接口：

```go
// 现状（简化 stub router）
type stubRouter struct{ ... }

// Phase 3 后
type fakeSessionRouter struct{ ... }  // 实现 upstream.SessionRouter 接口
```

同时可补测 `connector.go:630` `sess := c.router.GetSession(p.Key)` 返回 nil 时的 empty-events 回复——这条分支当前未覆盖。

---

## 6. 迁移步骤

每个 Phase 独立 commit + PR。

### Phase 1 — Dispatcher（0.5 天）

1. 新增 `internal/dispatch/consumer.go`（§3.2.1）。
2. `Dispatcher.router` 字段类型改为 `SessionRouter`；`DispatcherConfig.Router *session.Router` 保留。
3. **audit `if d.router != nil` nil-guard**（`dispatch.go:373`）：接口字段仍可判 nil，但若有 fake 实现为 typed-nil，比较语义会改。Phase 1 步骤里 grep 所有 `d.router`（无 `.` 的单引用）并列出 nil-guard。
4. 新增 `internal/dispatch/consumer_fake_test.go`（§5.1）。
5. 新增 2 个以上单测覆盖 §5.1 示例路径。
6. `go build ./... && go test -race -count=1 ./internal/dispatch/...`
7. 部署 smoke：发一条 project-bound 消息 + 一条 /new + 一条 /cd。

### Phase 2 — Hub（0.5 天）

1. 新增 `internal/server/consumer.go`（§3.2.2）。
2. `Hub.router` 字段类型改为 `HubRouter`。
3. audit `wshub*.go` / `send.go` 的 `h.router` 单引用（非 `h.router.X()`）。
4. 新增 fake + 单测（§5.2）。
5. `go build ./... && go test -race -count=1 ./internal/server/...`
6. 部署 smoke：dashboard WS 订阅 + 中断 + takeover。

**明确不做的事**：

- `dashboard_session.go` / `project_api.go` / `health.go` / `dashboard_cli.go` 等 `*SessionHandlers` 等 receiver 的 `h.router` 不动。
- `server.go` / `takeover.go` / `dashboard.go` 的 `s.router` 不动。
- 这两类合计约 30 处调用——**ARCH-SERVER-ROUTER-IF 的范围**。

### Phase 3 — Connector（0.5 天）

1. 新增 `internal/upstream/consumer.go`（§3.2.3）。
2. `Connector.router` 字段类型改为 `SessionRouter`；`upstream.New` 第二形参类型改为 `SessionRouter`。
3. **audit `internal/upstream/*_test.go` 的 router 字段访问**（至少 `connector_test.go:109`、`connector_stream_events_test.go:31`）。指针比较 `c.router != router` 在接口化后仍可用（interface == concrete pointer 合法），但如果某 test 注入 typed-nil 要做等价改写。
4. 迁移 `connector_test.go:319` 的 fetch_events stub → interface fake。
5. `go build ./... && go test -race -count=1 ./internal/upstream/...`
6. 部署 smoke：多节点 primary → reverse node 的 list / send / interrupt / takeover。

### 回归 / 验证

每 Phase 后：
```bash
go build ./...
go test -race -count=1 ./...
go vet ./...
```

---

## 7. 反向依赖风险与治理

### 7.1 新 Router 方法加入流程

1. 先加到 `session.Router`（具体类型）+ 单测。
2. 消费者需要时，再加到对应消费者接口。
3. **禁止** 消费者 type-assert 回 `*session.Router`。出现 `.(*session.Router)` 的 PR 必须显式说明。

### 7.2 接口膨胀触发点

消费者接口方法数到 15 时重新评估是否拆面。当前三接口最大是 Hub 的 **14 方法**（含 *ScratchHandler / *SendHandler 透传），留 1 方法余量。若 Phase 2.5 把 `*ScratchHandler` / `*SendHandler` 的 router 访问迁到独立接口，Hub 本体会回到 11 方法、阈值余量回升到 4。

### 7.3 签名漂移防护（新增）

v1 的 §7.1 "reviewer 肉眼 catch drift" 不可靠（`*session.Router` 同时满足所有接口时编译器不告警）。v2 **Phase 1 落地**新文件：

```go
// internal/session/contract_test.go（新，在 Phase 1 同一 commit 里落地）
package session_test

import (
    "github.com/naozhi/naozhi/internal/dispatch"
    "github.com/naozhi/naozhi/internal/server"
    "github.com/naozhi/naozhi/internal/session"
    "github.com/naozhi/naozhi/internal/upstream"
)

// 编译期断言：*session.Router 必须满足三个消费者接口。
// 任一消费者接口的方法签名漂移 → 这个文件编译失败 → CI 拒绝。
var (
    _ dispatch.SessionRouter = (*session.Router)(nil)
    _ server.HubRouter       = (*session.Router)(nil)
    _ upstream.SessionRouter = (*session.Router)(nil)
)
```

注：这个文件必须放在 `session_test` 包（不是 `session`）以避免与 `session` 的导出 API 形成测试-only 循环依赖。测试包可以 import 三个消费者包。

### 7.4 "Phase 2.5" 的关系

Server / Handlers 的 `s.router` 迁移（8 处 receiver 共 ~30 处调用）是独立 RFC。完成 Phase 2 不触发 Phase 2.5；完成 Phase 2.5 不依赖 Phase 2（两个可并行做，冲突面是 server 包内部的新 consumer.go 文件）。

---

## 8. 回滚策略

1. **单 Phase revert**：`git revert <commit>`。三个 Phase 相互独立。
2. **字段级回滚**（保留 consumer.go 但 struct 字段改回具体类型）：Dispatcher 改回 `router *session.Router` + 构造期一行；consumer.go 保留不参与链接。估计 diff **~10 行**（v1 声称 "<20 行"）。
3. **整体废除**：删三个 consumer.go + struct 字段全部改回 + `contract_test.go` 删。估计 1-2 小时（v1 声称 "<2 小时"）。

---

## 9. 非目标 / 延后

| 条目 | 原因 | 后续 RFC |
| :--- | :--- | :--- |
| `session.KeyResolver`（planner/agent key 派生） | 独立重构点 | `docs/rfc/key-resolver.md` |
| Router 拆子聚合 | 本 RFC 是其前置 | ARCH-ROUTER-SUBAGGREGATE |
| Server / Handlers / takeover / dashboard `s.router` 迁移 | Phase 2.5 范围；8 个 receiver 跨 30 处调用，diff 大 | ARCH-SERVER-ROUTER-IF |
| `cron.SessionRouter` 重命名对齐 | 已工作；改名无收益 | 不计划 |
| mock 生成工具 | 手写更轻 | 暂不 |
| 大一统 umbrella interface | §3.3 否决 | 不计划 |
| 每消费者拆多面子接口 | §3.4 否决 | 不计划 |

---

## 10. 决策记录

### D1：接口放消费者包

- **Pros**：Go 惯用、依赖方向正确、单向演化。
- **Cons**：三处重复声明 GetOrCreate / GetSession。
- **Decision**：接受重复。重复 ~20 行，跨包接口演化成本更高。

### D2：每消费者单接口（v2 新决策，覆盖 v1 三拆面）

- **Pros**：struct 字段从 3 个 → 1 个；构造期 1 行赋值；fake 实现单一；对标 `cron.SessionRouter` 范式。
- **Cons**：无法"只 mock 读面"。
- **Alternatives**：按 Lifecycle / Reader / Controller 拆 3 面。
- **Decision**：单接口。方法数（8-11）不足以触发拆分。膨胀到 15 时重议。

### D3：不动 Router

- **Decision**：本 RFC 零 risk；拆 Router 另立 RFC。

### D4：允许方法在多个消费者接口重复

- **Decision**：接受重复。配合 `contract_test.go` 的编译期断言防漂移。

### D5：不引入 mock 生成

- **Decision**：手写。膨胀时重议。

### D6：Server/Handlers 迁移不在本 RFC（v2 新）

- **Decision**：Phase 2.5 独立 RFC。当前 Phase 2 只动 Hub，明确边界。

---

## 11. 成功指标

Phase 1-3 完成后：

1. `rg 'router\s+\*session\.Router' internal/dispatch/dispatch.go internal/server/wshub.go internal/upstream/connector.go` → 0 行。
2. `internal/session/contract_test.go` 编译通过（即三个消费者接口签名与 Router 一致）。
3. 三个消费者新增单测文件，每测试运行 < 10ms。
4. `cmd/naozhi/main.go` 构造点改动 ≤ 2 行（`upstream.New` 形参类型改名）。
5. 生产 smoke：IM 消息收发、/new、/workspace、重命名、中断、takeover、upstream reverse — 全部通过。
6. `go doc internal/session/Router` 的 exported 方法列表字节级一致（无意外新增 / 删除）。
