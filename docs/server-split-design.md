# server 包拆分设计

## 现状

`internal/server/` 是典型的 God Object：8370 行、30 个文件、依赖 11/12 个 internal 包。
`Server` 结构体持有 30+ 字段，承担 HTTP 路由、WebSocket 中继、节点管理、Dashboard API、消息分发、认证、缓存等全部职责。

### 文件分布 (按职责)

```
server.go          416  ── 核心 struct + 启动
dashboard.go       163  ── 路由注册 + 静态文件
dashboard_auth.go  148  ── 登录/认证
dashboard_session.go 323 ── session API
dashboard_cron.go  233  ── cron API
dashboard_discovered.go 220 ── 发现 API
dashboard_send.go  212  ── 发送 API
dashboard_transcribe.go 76 ── 转写 API
dispatch.go        301  ── IM 消息分发
dispatch_commands.go 392 ── 斜杠命令
send.go             85  ── sendWithBroadcast
takeover.go         88  ── session 接管
project_api.go     218  ── project API
health.go          139  ── 健康检查
status.go          147  ── IM 状态格式化
image.go            78  ── 图片路径
wshub.go           564  ── WebSocket hub
wsclient.go        142  ── WebSocket 客户端
wshandler.go        32  ── WS 消息类型
wsrelay.go         292  ── WS 中继 (HTTP node)
nodeconn.go         79  ── NodeConn 接口
nodeclient.go      268  ── HTTP 节点客户端
nodecache_mgr.go   216  ── 节点缓存
reverseserver.go   129  ── 反向连接服务端
reverseconn.go     356  ── 反向连接
discovery_cache.go  90  ── 发现缓存
```

### 核心问题

1. **Server struct 是所有 handler 的唯一接收者** — 每个 dashboard handler 都通过 `s.xxx` 访问所需字段，添加新功能必然扩充 Server
2. **node 层耦合 WS 层** — `NodeConn.Subscribe()` 接受 `wsEventSink`，node 和 ws 无法独立存在
3. **dashboard handler 和 IM dispatch 共享发送路径** — `sendWithBroadcast` 同时被两条链路使用

---

## 设计原则

- **不过度拆分** — Go 的惯例是大包优于深嵌套。只提取有清晰接口边界的模块
- **不引入新依赖** — 拆分不应增加外部包
- **保持可增量执行** — 每个 phase 独立可编译、可测试、可 review
- **拆分标准** — 模块有独立的责任域、清晰的接口、>=200 行代码

---

## Phase 1: 提取 `internal/node/` (节点管理层)

**动机**: node 相关代码 ~1400 行，有清晰接口边界 (`NodeConn`)，两个实现（HTTP / 反向连接），一个缓存，一个中继。是最独立的子系统。

### 移动的文件

| 原文件 | 目标 | 行数 |
|---|---|---|
| `nodeconn.go` | `internal/node/conn.go` | 79 |
| `nodeclient.go` | `internal/node/httpclient.go` | 268 |
| `wsrelay.go` | `internal/node/relay.go` | 292 |
| `nodecache_mgr.go` | `internal/node/cache.go` | 216 |
| `reverseserver.go` | `internal/node/reverseserver.go` | 129 |
| `reverseconn.go` | `internal/node/reverseconn.go` | 356 |
| `wshandler.go` | `internal/node/protocol.go` | 32 |

**同步合并**: `internal/reverse/proto.go` (26 行) 并入 `internal/node/protocol.go`，消除 `internal/reverse/` 包。

**合计: ~1398 行**

### 线协议类型归属

`wsServerMsg` 和 `wsClientMsg` 定义在 `wshandler.go`（32 行），被以下文件使用：

- `wsrelay.go` — 14 处构造 `wsServerMsg`/`wsClientMsg`
- `reverseconn.go` — 7 处构造 `wsServerMsg`
- `wsclient.go` — 5 处构造 `wsServerMsg`
- `wshub.go` — 多处构造 `wsServerMsg`

这些类型本质是 node 和 dashboard 共享的 **wire protocol**。将其定义在 `node` 包中，
server 侧的 `wsclient.go` / `wshub.go` 反过来 import `node.ServerMsg` / `node.ClientMsg`。

这比在 node 内部定义 `relayMsg` 再做转换层简单得多——后者需要在 14+ 处增加类型映射代码。

同理，`reverse.ReverseMsg` (26 行) 也随之合入 `internal/node/protocol.go`，
`internal/upstream/connector.go` 改为 import `node.ReverseMsg`。

### 接口变更

```go
package node

// EventSink 从 wsEventSink 提升为公开接口，解耦 node 层和 WS 层。
// server 侧的 wsClient 实现此接口（方法名大写化: sendJSON → SendJSON, sendRaw → SendRaw）。
type EventSink interface {
    SendJSON(v any)
    SendRaw(data []byte)
}

// Conn 从 NodeConn 改名，去掉 Node 前缀 (node.Conn 已自解释)
type Conn interface {
    ID() string
    DisplayName() string
    RemoteAddr() string
    Status() string

    FetchSessions(ctx context.Context) ([]map[string]any, error)
    FetchProjects(ctx context.Context) ([]map[string]any, error)
    FetchDiscovered(ctx context.Context) ([]map[string]any, error)
    FetchDiscoveredPreview(ctx context.Context, sessionID string) ([]cli.EventEntry, error)
    FetchEvents(ctx context.Context, key string, after int64) ([]cli.EventEntry, error)
    Send(ctx context.Context, key, text, workspace string) error

    ProxyTakeover(ctx context.Context, pid int, sessionID, cwd string, procStart uint64) error
    ProxyRestartPlanner(ctx context.Context, projectName string) error
    ProxyUpdateConfig(ctx context.Context, projectName string, cfg json.RawMessage) error

    Subscribe(c EventSink, key string, after int64)
    Unsubscribe(c EventSink, key string)
    RemoveClient(c EventSink)

    Close()
}

// ServerMsg 从 wsServerMsg 改名并导出
type ServerMsg struct { ... }

// ClientMsg 从 wsClientMsg 改名并导出
type ClientMsg struct { ... }

// ReverseMsg 从 reverse.ReverseMsg 合并而来
type ReverseMsg struct { ... }
```

### server 侧变更

```go
// server.go 中
import "github.com/naozhi/naozhi/internal/node"

type Server struct {
    // ...
    nodes    map[string]node.Conn   // was: map[string]NodeConn
    nodeCache *node.CacheManager    // was: *NodeCacheManager
    // ...
}
```

`wshub.go` / `wsclient.go`:
- `wsServerMsg` → `node.ServerMsg`，`wsClientMsg` → `node.ClientMsg`
- `wsClient.sendJSON()` → `wsClient.SendJSON()`，`wsClient.sendRaw()` → `wsClient.SendRaw()`

`cmd/naozhi/main.go`:
- `server.NodeConn` → `node.Conn`
- `server.NewNodeClient` → `node.NewHTTPClient`
- `server.ReverseNodeServer` → `node.ReverseServer`
- `server.NewReverseNodeServer` → `node.NewReverseServer`

### 额外细节

- **`wsUpgrader` 全局变量**: 当前在 `wshub.go:22` 定义，`reverseserver.go:44` 共用。
  拆分后 `reverseserver.go` 在 node 包内自建 `websocket.Upgrader` 实例，不再跨包共享。
- **`removeSub` / `removeSubAll`**: 随 `nodeconn.go` 一起迁移到 node 包，无外部调用者。

### 风险

- `wsclient.go` 方法名大写化 (`sendJSON` → `SendJSON`) 影响 `wshub.go` 中所有调用点 (~20 处)。
  机械替换，但需仔细验证无遗漏。
- `internal/upstream/connector.go` 原来 import `internal/reverse`，改为 import `internal/node`。
  upstream 新增 node 依赖，依赖方向合理（upstream 是 node 的客户端角色）。

---

## Phase 2: 提取 `internal/dispatch/` (IM 消息分发)

**动机**: dispatch 逻辑处理所有 IM 平台来消息的路由、命令解析。与 dashboard API 无关。

### 前置步骤: image.go 先行移动

`dispatch.go:164` 调用 `extractImagePaths()`，`dispatch.go:169` 调用 `mimeFromPath()`。
这两个函数定义在 `image.go`（78 行），是纯函数、零依赖。

**Phase 2 开始前**，先将 `image.go` → `internal/cli/image.go`（改 `package cli`），
同步移动 `image_test.go` → `internal/cli/image_test.go`。
dispatch 通过 `cli.ExtractImagePaths()` / `cli.MimeFromPath()` 调用。

### 移动的文件

| 原文件 | 目标 | 行数 |
|---|---|---|
| `dispatch.go` | `internal/dispatch/dispatch.go` | 301 |
| `dispatch_commands.go` | `internal/dispatch/commands.go` | 392 |
| `status.go` | `internal/dispatch/status.go` | 147 |

**合计: ~840 行**

**注意: `takeover.go` 留在 server 包**。原因：`verifyProcIdentity()` 和 `killAndCleanupClaude()` 同时被
`dispatch.go`（IM 自动接管）和 `dashboard_discovered.go:177`（Dashboard 手动接管）调用。
如果移到 dispatch，server 需要反向 import dispatch，虽然不成环但违背"dispatch 被 server 调用"的单向依赖关系。
将 takeover 作为共享 utility 保留在 server 包中，dispatch 通过回调使用。

### Dispatcher 结构体设计

当前所有 dispatch 函数都是 `func (s *Server)` 方法，访问 12 个 Server 字段。
移到新包后不能再做 Server 方法，需要一个 `Dispatcher` 结构体持有这些字段：

```go
package dispatch

// Dispatcher 持有 IM 消息分发所需的全部依赖。
// 取代原来散布在 Server 方法上的 dispatch 逻辑。
type Dispatcher struct {
    Router        *session.Router
    Platforms     map[string]platform.Platform
    Agents        map[string]session.AgentOpts
    AgentCommands map[string]string
    Scheduler     *cron.Scheduler
    ProjectMgr    *project.Manager
    Guard         SessionGuard
    Dedup         *platform.Dedup
    AllowedRoot   string
    ClaudeDir     string
    BackendTag    string

    // 回调: 避免 dispatch → server 循环依赖
    SendFn    func(ctx context.Context, key string, sess *session.ManagedSession,
                   text string, images []cli.ImageData, onEvent cli.EventCallback) (*cli.SendResult, error)
    TakeoverFn func(ctx context.Context, chatKey, key string, opts session.AgentOpts) bool

    // Watchdog 配置 (用于超时错误消息)
    NoOutputTimeout time.Duration
    TotalTimeout    time.Duration

    // Watchdog kill 计数器 (原子操作)
    WatchdogNoOutputKills *atomic.Int64
    WatchdogTotalKills    *atomic.Int64
}

// SessionGuard 接口化，避免依赖 server 包的具体实现。
// server 包中的 sessionGuard 结构体实现此接口。
type SessionGuard interface {
    TryAcquire(key string) bool
    ShouldSendWait(key string) bool
    Release(key string)
}

// BuildHandler 返回 platform.MessageHandler，取代 Server.buildMessageHandler()。
// 内部创建 Dispatcher 实例并返回其处理方法。
func (d *Dispatcher) BuildHandler() platform.MessageHandler

// 以下方法原为 Server 方法，改为 Dispatcher 方法:
// - d.dispatchCommand(...)       原 s.dispatchCommand(...)
// - d.handleHelpCommand(...)     原 s.handleHelpCommand(...)
// - d.handleNewCommand(...)      原 s.handleNewCommand(...)
// - d.handleCronCommand(...)     原 s.handleCronCommand(...)
// - d.handleProjectCommand(...)  原 s.handleProjectCommand(...)
// - d.handleCdCommand(...)       原 s.handleCdCommand(...)
// - d.sendSplitReply(...)        原 s.sendSplitReply(...)
```

### takeover 的回调注入

`tryAutoTakeover` 原为 Server 方法，移到 dispatch 后通过 `TakeoverFn` 回调注入：

```go
// server.go 中构造 Dispatcher 时
dispatcher := &dispatch.Dispatcher{
    // ...
    TakeoverFn: s.tryAutoTakeover,  // takeover.go 仍在 server 包
    SendFn:     s.sendWithBroadcast,
}
```

这样 takeover.go 留在 server 包，dispatch 和 dashboard_discovered 都能使用，无跨包问题。

### server 侧变更

```go
// server.go Start() 中
d := &dispatch.Dispatcher{
    Router:                s.router,
    Platforms:             s.platforms,
    Agents:                s.agents,
    AgentCommands:         s.agentCommands,
    Scheduler:             s.scheduler,
    ProjectMgr:            s.projectMgr,
    Guard:                 s.sessionGuard,
    Dedup:                 s.dedup,
    AllowedRoot:           s.allowedRoot,
    ClaudeDir:             s.claudeDir,
    BackendTag:            s.backendTag,
    SendFn:                s.sendWithBroadcast,
    TakeoverFn:            s.tryAutoTakeover,
    NoOutputTimeout:       s.noOutputTimeout,
    TotalTimeout:          s.totalTimeout,
    WatchdogNoOutputKills: &s.watchdogNoOutputKills,
    WatchdogTotalKills:    &s.watchdogTotalKills,
}
handler := d.BuildHandler()
```

### 合并超小包

Phase 2 同步完成：
- `internal/pathutil/expand.go` (17 行) → `internal/config/expand.go`
  （`ExpandHome` 仅在 config 和 main.go 中使用）

**注意: `internal/routing/` 不合并到 dispatch**。原因：dispatch 依赖 cron（`*cron.Scheduler`），
cron 依赖 routing（`routing.ResolveAgent`）。如果 routing 合入 dispatch，会形成
`dispatch → cron → dispatch` 循环依赖。routing 保持独立（20 行）是防止此循环的最简方案。

### 测试迁移

`server_test.go` (642 行) 中涉及 dispatch 的测试需迁移：

| 测试 | 调用次数 | 目标 |
|---|---|---|
| `buildMessageHandler` 相关 | 11 处 | 迁移到 `internal/dispatch/dispatch_test.go` |
| `sendSplitReply` 相关 | 5 处 | 迁移到 `internal/dispatch/dispatch_test.go` |
| `parseCronAdd` 相关 | 1 处 | 迁移到 `internal/dispatch/commands_test.go` |
| `formatEventLine` / `appendStatusLine` | status_test.go | 整体迁移到 `internal/dispatch/status_test.go` |

迁移后测试构造 `Dispatcher` 实例而非 `Server` 实例，mock 依赖更轻量。

### 风险

- **方法签名批量修改**: 所有 `func (s *Server)` 改为 `func (d *Dispatcher)`，
  涉及 ~15 个方法。机械替换但需逐个验证字段映射。
- **`sendSplitReply` 依赖 platform 接口**: 需确认 dispatch 包能 import platform 包（可以，无循环）。

---

## Phase 3: 整理剩余 server 包

Phase 1-2 完成后，server 包剩余：

```
server.go           416  ── 核心 (可瘦身)
dashboard.go        163  ── 路由注册
dashboard_auth.go   148  ── 认证
dashboard_session.go 323 ── session API
dashboard_cron.go   233  ── cron API
dashboard_discovered.go 220 ── 发现 API
dashboard_send.go   212  ── 发送 API
dashboard_transcribe.go 76 ── 转写 API
project_api.go      218  ── project API
send.go              85  ── sendWithBroadcast
takeover.go          88  ── session 接管 (保留)
wshub.go            564  ── WebSocket hub
wsclient.go         142  ── WebSocket 客户端
health.go           139  ── 健康检查
discovery_cache.go   90  ── 发现缓存 (保留)
```

**~3117 行** — 从 8370 降到 3117，减少 63%。

这个体量对于一个 HTTP server 包来说是合理的。dashboard handler 都是 HTTP 处理函数，
紧密依赖 Server 字段，强行拆出去只会增加样板代码。

### Phase 3 做的是瘦身而非拆分

1. **`sessionGuard` 移到 `internal/session/guard.go`**
   它保护的是 session 级并发，语义上属于 session 包。
   导出类型 `session.Guard`，包含全部 4 个方法：
   - `TryAcquire(key string) bool`
   - `ShouldSendWait(key string) bool`
   - `Release(key string)`
   - `AcquireTimeout(key string, timeout time.Duration) bool`
   dispatch 包通过 `dispatch.SessionGuard` 接口使用（仅含 TryAcquire/ShouldSendWait/Release），
   server 包的 wshub.go 和 dashboard_send.go 直接使用 `*session.Guard` 具体类型（需要 AcquireTimeout）。

2. **`pathutil` 合并到 `config`** — 已在 Phase 2 完成，Phase 3 不再涉及。

3. **`Server` struct 字段清理**
   Phase 1-2 后自然消除 ~6 个字段（nodes 相关类型改为 node.Conn，dispatch 相关字段移走）。

### 不做的移动

- **`discoveryCache` 留在 server 包** — 它依赖 `project.Manager`。如果移到 discovery 包，
  discovery 就会新增 project 依赖，打破 discovery 作为"纯 IO 扫描层"的定位。
  90 行的缓存逻辑是 dashboard API 的关注点，留在 server 合理。

- **`image.go` 在 Phase 2 前置步骤中已移到 cli** — Phase 3 不再涉及。

---

## 拆分后目录结构

```
internal/
├── cli/            3226  CLI wrapper + protocol + image 工具 (原 3148 + image.go 78)
├── config/          619  配置加载 + ExpandHome (原 602 + pathutil 17)
├── cron/           1005  定时任务
├── discovery/       907  session 发现 (不变)
├── dispatch/        840  IM 消息分发 + 命令 + 状态格式化
├── node/           1398  远程节点 + 线协议 + 反向连接 + 缓存 (含 reverse 26 + wshandler 32)
├── platform/       3598  平台适配器
├── project/         430  项目管理
├── routing/          20  agent 命令路由 (保留，防 dispatch↔cron 循环)
├── server/         3117  HTTP server + dashboard API + WebSocket hub + takeover + discoveryCache
├── session/        2855  session 管理 + guard (原 2755 + sessionGuard ~100)
├── transcribe/      558  语音转写
└── upstream/        486  上行节点连接
```

### 依赖图 (拆分后)

```
cmd/naozhi/main.go
  ├─ config
  ├─ cli
  ├─ session ─── cli, discovery
  ├─ node ────── cli, config
  ├─ dispatch ── cli, session, platform, cron, routing, discovery
  ├─ server ──── cli, session, node, dispatch, cron, platform, project, discovery, transcribe
  ├─ upstream ── cli, session, project, discovery, node
  ├─ platform ── transcribe
  ├─ cron ────── session, platform, routing
  └─ routing ─── (零依赖)
```

server 的 import 从 11 个降到 9 个。关键改善是 node 和 dispatch 成为独立可测试的模块。
无循环依赖：server → {node, dispatch}，dispatch → {session, platform, cron, routing}，
cron → {session, platform, routing}，node → {cli, config}。routing 保持零依赖，同时被 dispatch 和 cron 使用。

---

## 执行顺序

| Phase | 范围 | 行数影响 | 破坏面 | 依赖 |
|---|---|---|---|---|
| 1 | `internal/node/` + 合并 reverse | -1398 | 中 (接口改名 + 类型迁移) | 无 |
| 2 | `internal/dispatch/` + image 前移 + pathutil→config | -935 | 高 (方法接收者重写) | Phase 1 完成 |
| 3 | sessionGuard→session | -100 | 低 | Phase 1-2 完成 |

每个 Phase 结束后 `go build ./...` 和 `go test ./...` 必须通过。

---

## 不做的事情

- **不拆 dashboard handler 到子包** — 7 个 handler 都是 `func(s *Server)` 方法，拆出去需要定义一个巨大的 interface 或传 10+ 参数，收益为负
- **不拆 WebSocket hub 到子包** — hub 和 dashboard API 共享认证、订阅、广播，拆分会制造循环依赖
- **不移 discoveryCache 到 discovery** — 会让 discovery 依赖 project，打破其纯 IO 层定位
- **不合并 routing 到 dispatch** — dispatch 依赖 cron，cron 依赖 routing；合并会造成 dispatch↔cron 循环
- **不引入 DI 框架** — 项目规模不需要 wire/fx，手动注入足够
- **不做一次性大重构** — 分 3 个独立 PR，每个可独立 review 和回滚
