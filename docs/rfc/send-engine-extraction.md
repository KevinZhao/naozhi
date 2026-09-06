# ARCH-SEND-ENGINE — 抽取 `sendEngine`：解开 Hub(WS) 与 SendHandler(HTTP) 共享的 send 链

| 字段 | 值 |
| :--- | :--- |
| 状态 | Draft **v2**（2026-09-07；v2 按三路对抗性 review 重做方案，v1 有 3 处编译不过、字段账少算 1 个、测试影响面错 3 处） |
| 作者 | naozhi team |
| 创建日期 | 2026-09-07 |
| 关联代码 | `internal/server/send.go`<br/>`internal/server/send_owner_loop.go`<br/>`internal/server/wshub_send.go`<br/>`internal/server/wshub.go`<br/>`internal/server/wshub_broadcast.go`（`LegacySendInvokes` 函数体在此）<br/>`internal/server/wshub_fanout.go`（`httpSendErrorCallback` / `informationalSendErr`）<br/>`internal/server/dashboard_send.go`<br/>`internal/server/consumer.go` + `consumer_contract_test.go`<br/>`internal/server/routes.go`<br/>`internal/server/send_dispatch_adapter.go`<br/>`internal/server/shutdown_lock_order_test.go` · `wshub_shutdown_order_test.go`<br/>`tools/lint-server-handlers/exemptions.yaml` |
| 关联设计 | `docs/design/server-split-phase4-design.md` §十五 ADR-001（**支持本 RFC**）+ §五（v0.1 三子 struct 取消，**需对账，见 §0.2**） |
| 关联 RFC | `docs/rfc/godstruct-extraction.md` §2 **G3** 的 Send 子步<br/>`docs/rfc/consumer-interfaces.md`（IoC 约束：接口在消费者处声明、构造期保留具体类型、方法数 15 重议）<br/>`docs/rfc/message-queue.md` |
| 关联 issue | #2551 · #2528（Epic E）· #2195 · #2549（Epic K）· #566（R215-ARCH-P1-4 SendRouter 窄化）· #2560（E5，结构断言应落 lint 而非 reflect 测试） |

## 0. 摘要

Hub（WS 入口）与 SendHandler（HTTP 入口）共享同一条 `enqueue → guard → router → TrackSend` 链，但这条链**没有自己的类型**：状态挂在 `Hub` 上，逻辑散在 `*Hub` / `*SendHandler` 方法与自由函数里。后果是 SendHandler 必须持 `hub *Hub` 才能发消息，于是 `h.hub != nil` 出现 5 次——HTTP 层对「WS 层是否存在」做条件分支。

本 RFC 新增 `sendEngine` 类型（留在 server 包，不搬包），把 send 的**状态所有权**与 goroutine 生命周期收进去；`Hub` 与 `SendHandler` 各持一个 `*sendEngine`，互不认识。

### 0.1 v2 相对 v1 的实质改动

| # | v1 的错 | v2 的做法 |
| :--- | :--- | :--- |
| 1 | 方法体搬进 `send_engine.go` | **只改 receiver，方法留原文件**（§2.2）。理由：搬过去 ≈540-560 行 > 500 硬上限；且 `log_level_test.go:23,46` 与 `send_sanitize_contract_test.go:69` 按**文件名**读 `send.go`（其中一条是 log-injection 脱敏门），搬走会让脱敏门静默空转 |
| 2 | 迁 5 个字段，账算成 52→48（>验收的 ≤47 却没发现） | **`guard` 一起迁**（生产上只有 `send.go:423/438/447/455` 用它），6 出 1 入 → **52 → 47**，压线达标（§2.4） |
| 3 | 「零值引擎可安全构造」 | **假的，且是唯一一处朝坏方向的安全变化**：`server_validate.go:72` 是 `if allowedRoot != ""`，空 root 跳过整个包含性检查。今天 nil hub 读 `hub.allowedRoot` 必 panik = fail-closed；零值引擎变 fail-open（带 PDF + `workspace=/etc/cron.d` 会真写进去）。v2 **禁止零值引擎**（§2.7） |
| 4 | `h.router.SetWorkspace` | `SendRouter` 只有 2 个方法，**编译不过**。v2 不拓宽它（§2.6） |
| 5 | 漏 `h.hub.httpSendErrorCallback`（`dashboard_send.go:506`） | E1-b 编译不过。v2 下沉为引擎方法（§2.5） |
| 6 | 「trackMu 是叶子锁，既有序不受影响」 | **误导性**：`drain()` 等的 goroutine 会经 notify 反向进 `authMu` / `debounceMu`。放进 `debounceMu` 临界区 = 确定性死锁，`-race` 抓不到。v2 把三条前置写进 godoc + 加源序 pin（§3） |
| 7 | 测试影响面「已逐文件核实」 | 枚举轴错（用「手搓 `&Hub{}`」，漏了全部 `newTestHub()` 用户）。漏 `send_panic_notify_test.go`（4 处写 `hub.queue` + 4 处调 `handleOwnerLoopPanic`）、`ws_test.go:655`（`hub.guard`）；且把 `dashboard_send_ratelimit_msg_test.go` 误判为 nil-hub（它有真 hub 且断言 202）。v2 §6 用可复现 grep 重建 |
| 8 | 引擎复用 `HubRouter` | `HubRouter` 实测 **15** 方法（godoc 写 14），正好触到 consumer-interfaces §7.2 的重议阈值，且 godoc 自承驮着 SendHandler 的透传债。v2 给引擎 12 方法的 `sendEngineRouter`（§2.6） |
| 9 | `READS: sendEngine` | `READS:` 不是合法 marker，**fail 模式 CI 立刻红**（`rule_field_block.go:61` 只认 `WRITES:`/`READS-ALSO:`/`LIFECYCLE-METHOD`）。v2 用 `READS-ALSO:`（§7） |

**明确不做**：不搬包、不改任何对外 HTTP/WS 报文、不动 queue 语义、不改 `allowedRoot == "" 表示不限制` 这条既有策略。

### 0.2 与 `server-split-phase4-design.md` 的两处关系（v1 只引了对自己有利的那条）

- **§十五 ADR-001 支持本 RFC**：`design.md:1638-1641`「正确的未来顺序」第 1 条逐字是"在 server 包内部把 send 引擎抽成自包含单元（只依赖已接口化的 router/queue/guard）"。
- **§五 否决过"三子 struct"**（`design.md:384-393`），理由是"三 struct + 反向接口 = Java 化"「没解决根本耦合，只是把 Hub 字段切成 3 块塞回 Hub 壳里」。**同一份文档自己不自洽**，必须交代清楚区别：

| §五 否决的模式 | 本 RFC |
| :--- | :--- |
| 字段**仍在 Hub 上**，子 struct 只是 view | 字段**所有权真迁走**，Hub 上删除 |
| 反向大接口 `hubAccess`（Hub 的宽访问面） | `sendNotifier` **4 个方法**，只是广播出口 |
| 单一消费者（Hub 自己） | **两个独立消费者**（Hub / SendHandler），这正是拆的动因 |
| 无生命周期含义 | 引擎拥有 `drain()` 屏障与 send goroutine 记账 |

**擦到边的一处，必须记账**：§2.1 把 8 个共享依赖在 Hub 与引擎上**各存一份引用**。而 #2551 立案的证据恰恰是"SendHandler 同持 `hub` 与 `router` = 同一份状态的两个视图"。E1 消掉 1 处这种模式、新造 8 处。缓解：struct 注释写死"构造后只读，任何写入都是 bug"，契约测试断言两侧指向同一实例（对标 `hub_shared_state_test.go` 对 `nodes` 的做法），去重归 Epic K。

### 0.3 `droppedTotal` 的归属

`design.md:427-433` 与 `server-split-phase4-baseline.md:140` 定义的 send 块是 **6 个字段（含 `droppedTotal`）**；issue #2551 的定义是 5 个（不含）。`droppedTotal` 实际写在 `wsclient.go:190`（SendRaw 发送通道满），读在 `wshub_broadcast.go:292`，是 **WS 传输指标**，与 send 流水线无关。**本 RFC 不迁它**，并在 E1-c 把 design.md 的 send 块定义里把它移到 rate-limit/cache 块。即"Hub 不再拥有 send 块字段"只在 issue 的 5 字段定义下成立——这一点在关闭 issue 时要写明。

## 1. 侦察事实（2026-09-07 实测 master `77a82c8c`）

| 事实 | issue 声称 | 实测 | |
| :--- | :--- | :--- | :--- |
| send 链行数 | 1,678 | 461 + 367 + 851 = **1,679** | ✅ |
| `send.go`+`wshub_send.go` 的 `*Hub` 方法 | 13 | 8 + 5 = **13** | ✅ |
| `Hub` 字段 | 52 | **52** | ✅ |
| `Server` 字段 | （epic 说 52） | **52** | ✅ 两个 52 各自都对，但不是同一个东西 |
| `dashboard_send.go` 的 `h.hub != nil` | 5 | **5**（433/459/477/488/494） | ✅ |
| `h.hub` 无条件解引用 | — | **7**（391/502/506/564/581/687 + 581） | v1 记账漏 `:506` |
| `hub == nil` 守卫（epic 13） | 13 | **13**（server.go:286,388,408,612 / send.go:111 / dashboard_send.go×5 / wsclient.go:189 / agent_tailer_registry.go:107 / server_loops.go:99） | ✅ |
| `guard` 的生产使用点 | — | **仅 `send.go` 4 处**（+`wshub.go:263` 构造赋值） | send 独占 |
| `HubRouter` 方法数 | godoc 写 14 | **15** | 触重议阈值 |

**订正 3 处前提**：

1. **`wshub_send.go` 没有豁免条目**（367 行 < 500）。验收第 4 条"三条 4b 条目"实际只有 `wshub.go` 一条挂 `4b`。
2. **`send.go` 的条目是死账**：挂 `until_phase: 3f` 不是 `4b`，记 `current: 599` 实测 **461**；`main.go:261` 是 `if lines <= limit { return nil }`，在查豁免表**之前**返回，条目完全惰性 → 该删。（`dashboard_send.go` 同样漂移：记 1089 实测 851，仍 >500，只订正 `current`。）
3. **`Closes-exemption:` 的 CI 校验不存在**：`rule_stale_exemption.go:15-31` 只做 `os.Stat` 存在性判断；全仓无任何 step/脚本校验 commit trailer。而 `design.md:757` 明写"无对应 `Closes-exemption:` 行…**fail**"——这是文档对代码的虚构。本 RFC 照惯例带 trailer（人肉纪律），并把 design.md:757 的假声明列进 E1-c。
4. **`until_phase` 是 string 字段**（`main.go:56`），写成不带引号的纯数字会让 `yaml.Unmarshal` 失败 → `main.go:80-84` `os.Exit(2)`（比 violation 更硬）。所以文件里 `"5"` 带引号、`3f`/`4b` 不带。E1-c 改挂 Epic K 必须写 **`until_phase: "K"`**。

## 2. 设计

### 2.1 `sendEngine`

```go
// send_engine.go —— 只放类型 + 构造 + 出口接口 + TrackSend/drain（≈130 行）。
// 流水线方法体留在 send.go / send_owner_loop.go 原位（§2.2）。
type sendEngine struct {
    // ── 引擎独占状态（今天挂在 Hub 上，本 RFC 迁走：6 个）──
    queue   MessageEnqueuer  // nil ⇒ legacy guard 路径（typed-nil 契约 #377）
    guard   *session.Guard
    wg      sync.WaitGroup   // 原 Hub.sendWG
    trackMu sync.Mutex       // 原 Hub.sendTrackMu
    closed  bool             // 原 Hub.sendClosed（trackMu 保护）
    legacyInvokes atomic.Int64 // 原 Hub.legacySendInvokes

    // ── 构造后只读的共享依赖（Hub 侧仍保留各自引用，非 send 路径在用）──
    // 不变量：构造后不再写。任何 e.<field> = 都是 bug（Hub 与引擎必须同实例，
    // 由 send_engine_contract_test.go 断言）。去重归 Epic K #2549。
    ctx         context.Context
    router      sendEngineRouter
    resolver    *session.KeyResolver
    agents      map[string]session.AgentOpts
    projectMgr  *project.Manager
    scratchPool *session.ScratchPool
    scheduler   CronView
    allowedRoot string

    notify sendNotifier
}
```

`Hub` 侧字段名用 **`engine *sendEngine`**，不叫 `send`：同包内 `wsClient` 已有一个 `send` channel 字段（`c.send`，多处使用），`h.send` 与 `c.send` 会视觉混淆。

**为什么不用 `hub *Hub` 反向指针**：那就是 §五 否决的 `hubAccess`，且让 SendHandler 经引擎传递性地耦合回 Hub，等于换名字。**为什么不用一个大 `sendEngineDeps` 接口**：8 个依赖里 6 个是具体类型或值（`*session.Guard` / `*KeyResolver` / `*project.Manager` / `*ScratchPool` / map / string），包成接口纯样板，违反 consumer-interfaces §4.5"构造期保留具体类型"。

### 2.2 receiver 改名，方法体不动

10 个方法把 receiver 从 `(h *Hub)` 改成 `(e *sendEngine)`、体内 `h.` 改 `e.`，**留在 `send.go` / `send_owner_loop.go` 原位**：

`sendWithBroadcast` · `sendWithBroadcastPriority` · `sessionSend` · `sessionSendLegacy` · `sessionOptsFor` · `runTurn` · `runTurnPassthrough` · `autoSaveCronPrompt`（send.go）· `ownerLoop` · `handleOwnerLoopPanic`（send_owner_loop.go）

四个好处：`send_engine.go` 不会撞 500 行；`send.go` 保持 461 行（其死豁免仍可删）；三个按文件名读 `send.go` 的契约测试（`log_level_test.go:23,46`、`send_sanitize_contract_test.go:69`，后者是 R175-SEC-P1 的 log-injection 脱敏门）**完全不受影响**；diff 可逐行核对，是真正的 move-only。

体内替换只有三类，逐类穷举：`h.<迁走的字段>` → `e.<同名>`；`h.<共享依赖>` → `e.<同名>`；`h.<广播方法>` → `e.notify.<同名>`（4 个：`BroadcastSessionReady` / `BroadcastSessionsUpdate` / `broadcastState` / `broadcastSendError`）。**不合并分支、不改日志文本、不调整错误语义。**

### 2.3 WS 协议适配器保留在 Hub —— 对验收标准第 3 条的改判

验收原文是"`send.go` + `wshub_send.go` 内 `func (h *Hub)` 方法数 13 → 0"。按 §2.2，`send.go` 的 8 个 receiver 改掉后该文件的 `func (h *Hub)` **确实归零**；剩 5 个全在 `wshub_send.go`。逐个判：

| 方法 | 判定 |
| :--- | :--- |
| `lookupNode`（:35-37） | **分类本来就错**：它是 `h.nodes.NodeByID` 的一行 wrapper，和 send 无关，只是被塞进了 wshub_send.go。E1-a 移到 node registry 相关文件，让计数诚实 |
| `handleSend`（:39） · `handleInterrupt`（:154） | 纯 wsproto 适配（`wsproto.NewSendAck` ×8、`c.uploadOwnerKey()`、`h.uploadStore.TakeAll`）。HTTP 对称的那半（`SendHandler.handleSend`）从来不在引擎里 —— **保留 `*Hub` 是对称的** |
| `handleRemoteSend`（:263） · `handleRemoteInterrupt`（:189） | **不是纯适配器**：拥有 send goroutine 生命周期（`TrackSend`）、远程派发策略（`gateRemoteAccessProfile` / `selectNodeForBackend`）、post-send 广播。保留 `*Hub`，但必须经 `h.engine.TrackSend()` 交接 |

**诚实说明**：v1 给的理由"迁走会让引擎 import wsproto"在今天是**空的**——全部代码同一个 `package server`，方法搬家不产生任何包依赖边。真正的论证是两条：① HTTP / WS 两个 transport 必须对称，HTTP 侧的 handler 从来不在引擎里；② 迁走后引擎需要 `*wsClient` + `wsproto` + `uploadStore` + `node.Conn/NodeProxy` + `isValidNodeID`/`selectNodeForBackend`/`hubNodeLookup`，从"send 流水线"膨胀成"WS send 子系统"——正是 §五 骂的那件事。

**建议把验收第 3 条改判为**（并**去 issue 上改，不只写在 RFC 里**，否则关闭时对不上账）：

- (i) `grep -nE 'h\.(queue|guard|sendWG|sendTrackMu|sendClosed|legacySendInvokes)\b' internal/server/*.go` == 0（含 `_test.go`，见 §6）
- (ii) `send.go` 内 `func (h *Hub)` == 0 且 `send_engine.go` 内 `func (h *Hub)` == 0（反向也卡）
- (iii) `wshub_send.go` 内不出现 `h.queue` / `h.guard` / `sendWG`，send goroutine 一律经 `h.engine.TrackSend()`

(i)(ii) 落成 **lint 规则**而不是 markdown 里的 grep 或新 reflect 测试——`main.go:152` 明写"rule 3b (AST field_block) due Phase 4b"，4b 永不到来 = 这条规则永远欠着，E1 正是兑现它的时机；同时兄弟 issue #2560（E5）的方向恰是"结构型 gate 测试迁 lint、只留行为断言"，新加 reflect 字段扫描测试会与之相悖。

### 2.4 `Hub` 的 send 面收缩：52 → 47

6 出 1 入。保留的对外方法只有一个：

- `Hub.LegacySendInvokes()`（函数体在 **`wshub_broadcast.go:303`**，不在 wshub.go）→ 代理 `h.engine.legacyInvokes.Load()`。它是 R-LEGACY-SEND (#710) 的迁移观测口，既有测试断言 **nil 接收者返回 0**，代理保留 `if h == nil || h.engine == nil { return 0 }`。
- `Hub.TrackSend()` **删除**。调用点：`wshub_send.go:205,318` → `h.engine.TrackSend()`；`dashboard_send.go:459` → 见 §7 的分步（E1-a 阶段先 `h.hub.engine.TrackSend()`）。
- `Hub.httpSendErrorCallback()`（`wshub_fanout.go:54`）**删除**，下沉进引擎（§2.5）。

### 2.5 `sendNotifier` + 引擎自己的错误回调

```go
// consumer.go（与 HubRouter / SendRouter / ScratchRouter / HubBroadcaster 同处）
//
// sendNotifier 是 sendEngine 向 dashboard 广播的唯一出口，也是 Epic K #2549
// 把 broadcast 面切成独立对象时的 hand-off 点：K 只换实现者，引擎不动。
// 不复用 HubBroadcaster —— 后者 6 个方法里 4 个是 cron/daemon 生命周期，
// 且缺这里需要的两个非导出方法。
type sendNotifier interface {
    BroadcastSessionReady(key string)
    BroadcastSessionsUpdate()
    broadcastState(key, state, deathReason string)
    broadcastSendError(key, msg string)
}
```

`dashboard_send.go:506` 的 `h.hub.httpSendErrorCallback(key)` 下沉为引擎方法，`informationalSendErr` 过滤**必须原样保留**：

```go
// sendErrorCallback 取代 Hub.httpSendErrorCallback。informational 结果
// (ErrAbortedByUrgent / ErrSessionReset / ErrReconnectedUnknown) 被丢弃：
// 该回调向 key 的每个订阅者 fan-out，若 A 的 HTTP send 被 B 的 /urgent 打断，
// B 的 tab 会自己拆掉乐观气泡。session_state 负责收敛 UI。
func (e *sendEngine) sendErrorCallback(key string) asyncErrorFn {
    return func(err error, msg string) {
        if informationalSendErr(err) { return }
        e.notify.broadcastSendError(key, msg)
    }
}
```

**这条是 v1 最危险的漏项**：`sendNotifier` 只有裸 `broadcastSendError` 时，实施者唯一能写出的形状就是不带过滤的直调 → `/urgent` 打断、`/clear` 复位、重连未知态会广播给同 key 的每个 tab。而现有防线 `dashboard_send_async_error_test.go:134` 直接调 `hub.httpSendErrorCallback(key)`、**不经 handler**，所以 handler 侧的回归会全绿通过。E1-b 必须同时把该测试改指向 `hub.engine.sendErrorCallback(key)`。

`informationalSendErr`（`wshub_fanout.go:41`）是自由函数，同时被 `runTurnPassthrough`（`send.go:389`）用，留在原处不动。

### 2.6 `sendEngineRouter`（12 方法）与 `SendRouter` 不拓宽

引擎实际用到的 router 方法，逐个从源码点出：`GetOrCreate`(349,377) · `SessionFor`(77,182) · `ResetAndDiscardOverride`(187) · `SetWorkspace`(207) · `SetSessionBackend`(224) · `SetSessionAccessProfile`(234) · `DefaultWorkspace`(245) · `RegisterForResume`(247) · `InterruptSessionViaControl`(290) · `InterruptSessionSafe`(428) · `NotifyIdle`(456) · `Workspace`（`resolveAttachmentWorkspace`）= **12**。

不复用 `HubRouter`：它实测 **15** 方法（godoc 自己写 14），正好触到 consumer-interfaces §7.2 的重议阈值，且其 godoc 自承驮着 "*ScratchHandler / *SendHandler borrow" 的透传债——让引擎持有它只是把借用债换了个持有人。新声明 `sendEngineRouter`（12 方法，`*session.Router` 结构化满足），并在 `consumer_contract_test.go` 加两条编译期断言（该文件正是本仓的签名漂移防护机制）：

```go
var _ sendEngineRouter = (*session.Router)(nil)
var _ sendNotifier     = (*Hub)(nil)
```

**`h.router.SetWorkspace` 编译不过**（`SendRouter` 只有 `SessionFor` + `Workspace` 两个方法）。v2 **不拓宽 `SendRouter`**——拓宽会让 `handleBind` 的 override 写入口变成可被 stub 劫持的注入点，而 `sessionSend`（`send.go:207`）的同一份 `workspaceOverride` 表仍走引擎，同一张表出现两个可独立替换的注入点。`handleBind:581` 改为 `h.engine.router.SetWorkspace(...)`。

**`resolveAttachmentWorkspace` 顺手兑现 #566**：`scratch_send_router_iface_test.go:44-47` 的注释声称"`SendHandler.router` 取代了 `resolveAttachmentWorkspace` 里的 `h.hub.router.*` 透传"，但该函数今天用的就是 `hub.router`（`dashboard_send.go:151,159`）——注释是空头承诺。v2 把它改成显式参数的自由函数：

```go
func resolveAttachmentWorkspace(r SendRouter, allowedRoot, sessionKey, reqWorkspace string) (string, error)
```

HTTP 侧传 `h.router`（**终于真的走窄视图**，注释变成真的）；WS 侧传 `h.engine.router`（`sendEngineRouter` 含 `SessionFor`+`Workspace`，接口间赋值合法）。两侧同源不变（`routes.go:99 router: s.hub.router`；`wshub.go:258 router: opts.Router`）。

### 2.7 `SendHandler` 去 `hub` —— 且**禁止零值引擎**

```go
type SendHandler struct {
    nodeAccess NodeAccessor
    engine     *sendEngine  // 取代 hub *Hub；非可选，只能来自 newSendEngine
    router     SendRouter
    …（其余 6 字段不变）
}
```

| `dashboard_send.go` | 今天 | 改后 |
| :--- | :--- | :--- |
| 391 | `resolveAttachmentWorkspace(h.hub, key, workspace)` | `resolveAttachmentWorkspace(h.router, h.engine.allowedRoot, key, workspace)` |
| 433 | `if h.hub != nil { gateRemoteAccessProfile(h.hub.resolver, …) }` | `gateRemoteAccessProfile(h.engine.resolver, …)` |
| 459 | `if h.hub != nil { h.hub.TrackSend() }` | `h.engine.TrackSend()` |
| 477 | `if h.hub != nil { WithTimeout(h.hub.ctx, 60s) } else { Background(), 30s }` | `context.WithTimeout(h.engine.ctx, 60*time.Second)` |
| 488 | `if h.hub != nil { h.hub.broadcastSendError(…) }` | `h.engine.notify.broadcastSendError(…)` |
| 494 | `if h.hub != nil { h.hub.BroadcastSessionsUpdate() }` | `h.engine.notify.BroadcastSessionsUpdate()` |
| 502 | `h.hub.sessionSend(…)` | `h.engine.sessionSend(…)` |
| 506 | `h.hub.httpSendErrorCallback(key)` | `h.engine.sendErrorCallback(key)` |
| 564 · 687 | `validateWorkspace(…, h.hub.allowedRoot)` | `…, h.engine.allowedRoot` |
| 581 | `h.hub.router.SetWorkspace(…)` | `h.engine.router.SetWorkspace(…)` |

**两条订正**：

1. **477 行不是"行为收敛"**。v1 说它是；实际两个 nil-hub 测试是在 `349`（text too long）和 `374`（`node!=local && len(images)>0`）**提前返回**，从来没走到 477。真正跑 60s 分支的是 `nodeclient_test.go:341-386 TestHandleAPISend_RemoteNode`（真 hub）。无测试断言 30s 分支，删它无风险；但**风险全在 nil ctx**（见下）。
2. **433 行的 `gateRemoteAccessProfile` 无条件化也不是"收紧"**。`select_node_for_backend.go:103-111` 是 `if targetNode == "" || targetNode == "local" || resolver == nil { return nil }`——**nil resolver 本身就是 no-op**。今天跳过闸门与改后进闸门立刻返回**等价**，一个 request 都不会改判。v1 把它记成安全收益是错的，v2 记成"无行为变化"，免得后续 reviewer 误以为闸门已加强。

**禁止零值引擎**（v1 最危险的处方）。`sendEngine` **只能由 `newSendEngine` 构造**，测试要引擎就用 `newTestHub().engine`。理由是零值的四个字段各有一条真实崩溃或放行路径：

| 零值字段 | 后果 |
| :--- | :--- |
| `allowedRoot == ""` | `server_validate.go:72` 的 `if allowedRoot != ""` 跳过整个包含性检查 → **fail-open**。今天 nil hub 读 `hub.allowedRoot` 必 panic = fail-closed。这是本 RFC 唯一一处朝坏方向的安全变化，必须堵住（`dashboard_bind_test.go:29-31` 已经把这条红线写成注释） |
| `ctx == nil` | `context.WithTimeout(nil, …)` panic，且在裸 `go func`（`dashboard_send.go:467`）里——`net/http` 的 per-request recover **管不到**，整进程崩 |
| `guard == nil` | `send.go:423 e.guard.TryAcquire` → `session/guard.go:28` 的 `g.active.LoadOrStore` nil 解引用 |
| `router == nil` | `resolveAttachmentWorkspace` 的 fallback 分支（151/159）panic |
| `notify == nil`（接口） | typed-nil 与 queue 完全对称：`Notify: (*Hub)(nil)` 会让 `!= nil` 读成 true → 首次广播 nil 解引用，且发生在 owner goroutine 里被 `ownerLoop` 的 recover 吞掉 → **消息静默消失** |

`newSendEngine` 因此做三件兜底：`Ctx == nil → context.Background()`（对齐 `wshub.go` 对 `ParentCtx` 的处理）；`Notify` 做具体类型判空后 nil → `nopNotifier{}`；queue 保留 typed-nil 装箱段（#377）。

**v2→实现的一处订正**：v2 原写"`Router == nil` 直接 panic"。实测 `wshub_cookie_mac_rotation_test.go:77` 有 `NewHub(HubOptions{})`（连 Router 都不传），panic 会直接打断它。改为**不拒绝 nil Router**：那种 Hub 的 send 路径在 `GetOrCreate` 处失败，与今天 `h.router == nil` 的行为逐字一致。

**另一处实现期才暴露的坑（#377 在新边界重演）**：`sendEngineOpts.Queue` 必须是**具体类型** `*dispatch.MessageQueue`，不能是 `MessageEnqueuer`。写成接口时，`NewHub` 传 `opts.Queue`（具体 nil 指针）会在**进入 `newSendEngine` 之前**就被装箱成非 nil 接口，于是构造函数里的 `o.Queue == nil` 恒假、`e.queue` 拿到 typed nil、`send.go` 的 legacy fallback 门被静默关掉。这正是 #377 的原始故障，只是搬到了新的参数边界上。第一版实现就踩了，被 `TestNewHub_NilQueue_LeavesInterfaceFieldNil` 当场抓住——也是 consumer-interfaces.md §4.5"构造期保留具体类型"的一个具体理由。

**`allowedRoot` 的语义不变**：`"" = 不限制` 是 `server_validate.go:72` 的既有策略（生产上由配置决定），本 RFC 不改。写在这里是为了让后来者不要把某个 nil 判断"简化"成零值。

### 2.8 构造

`NewHub` 在填完 Hub 字面量后建引擎（此时 `h` 已可作 notifier）：

```go
h.engine = newSendEngine(sendEngineOpts{
    Queue: opts.Queue, Guard: opts.Guard, Ctx: ctx, Router: opts.Router,
    Resolver: opts.Resolver, Agents: opts.Agents, ProjectMgr: opts.ProjectMgr,
    ScratchPool: opts.ScratchPool, Scheduler: opts.Scheduler,
    AllowedRoot: opts.AllowedRoot, Notify: h,
})
```

`routes.go:97` 的 `hub: s.hub` → `engine: s.hub.engine`。`wshub.go:263` 的 `guard: opts.Guard` 从 Hub 字面量删除。

## 3. Shutdown 排干顺序与锁序（v1 这一节的理由是错的）

引擎拥有 `wg`/`trackMu`/`closed`，`wshub.go:750-756` 那段屏障整段变成一次调用，**位置逐字不变**：

```go
// 原：h.sendTrackMu.Lock(); h.sendClosed = true; h.sendTrackMu.Unlock(); h.sendWG.Wait()
if h.engine != nil {
    h.engine.drain()
}
```

```go
// drain 关闭 send 受理窗口并等待经 TrackSend 注册的 goroutine。
//
// 屏障语义：一次 TrackSend 落在 closed 置位的某一侧；置位后无人再 Add，
// 因此 wg.Wait 不可能被绕过。
//
// 调用前置（三条都是硬要求，违反任一即 Shutdown 永久挂死或 post-Shutdown
// 广播；-race 检测不到，因为它们不是数据竞争）：
//   1. h.cancel() 已调用。否则 wg.Wait 要等满远程 RPC 超时
//      （dashboard_send.go 的 60s / remoteNodeProxyTimeout 10s）而不是被
//      ctx 取消立即返回。
//   2. debounceClosed + debounceClosedFast 已发布（wshub.go:649-651）。
//      被等待的 goroutine 会经 sendNotifier 调 BroadcastSessionsUpdate →
//      wshub_broadcast.go:200 clientWG.Add(1)，即存在 sendWG-goroutine →
//      clientWG.Add 的跨 WaitGroup 依赖。未先关窗就 drain，会武装一个
//      在 authClientsSlice 已清空之后才跑的广播回调。
//   3. 调用方不得持 h.mu / authMu / debounceMu。被等待的 goroutine 经
//      notify 反向进入这三把锁（BroadcastSessionReady → authMu.RLock、
//      broadcastState → h.mu.RLock、BroadcastSessionsUpdate →
//      debounceMu.Lock）。把 drain 放进 debounceMu 临界区是确定性死锁：
//      在飞的 goroutine 已过 debounceClosedFast 快路、正阻塞在
//      debounceMu.Lock，而 Shutdown 持着它等 wg 归零。
//
// 必须在 nodes 关闭之前（在飞的远程 RPC 不能写已关闭的 nc.conn）。
func (e *sendEngine) drain() {
    e.trackMu.Lock()
    e.closed = true
    e.trackMu.Unlock()
    e.wg.Wait()
}
```

**v1 给的理由是错的**：v1 写"必须在 `clientWG.Wait` 之后——readPump 退出路上还会走 `handleRemoteSend`（即 TrackSend）"。这不成立：`closed` 检查已让 `TrackSend` 返回 `shuttingDown=true`，`wshub_send.go:319-322` 直接回 error ack 而不 spawn。真正的约束是上面的第 2 条。

**`drain()` 不覆盖 IM 路径**：`Server.sendWithBroadcast`（`send.go:100-123`，入口 `send_dispatch_adapter.go:24`，即全部 IM 派发）是**同步调用，从不经 `TrackSend`**。godoc 与 §5 都要写明"drain 只等经 TrackSend 注册的 goroutine"。

**幂等与二次调用**：`drain()` 调两次安全（`closed` 是 `trackMu` 下的单调 bool，`WaitGroup.Wait` 可并发调用）。生产上 `Hub.Shutdown` 只有一个调用者（`server.go:607-614` 的 once-only shutdown goroutine），`h.cancel()` 全仓只出现一次（`wshub.go:643`）。

**`h.engine != nil`** 这一个判断是给 12 个测试文件里 35 处手搓 `&Hub{}` 留的（不走 `NewHub`），与同文件既有的 `h.tailers != nil` / `h.historyMarshalCache != nil` 同类，注释写明原因。

**新增源序 pin**（比 markdown 里的 grep 有用，与既有 `TestHubShutdown_OrderingInSource` 同构）：`h.engine.drain()` 的源码 offset 必须 > `h.cancel()`、> `debounceMu.Unlock()`、> 最后一个 `h.mu.Unlock()`、> `h.clientWG.Wait()`，且 < `h.nodes.Conns()`。

**`wshubLockOrderFiles`（`shutdown_lock_order_test.go:22-30`）不加 `send_engine.go`**。读过扫描逻辑：外层循环由 `subMu\.(?:R?Lock)\(` 的命中数驱动，引擎里不会有 `subMu` → 循环体不执行 → 加进去是纸面工作；而 `hMuRe` 写死了 receiver 名 `h\.mu\.`，对 `e.` 接收者天然失明。且清单项对文件存在性硬依赖（`:83-85` 读不到即 `t.Fatalf`），单独 revert E1-a 而留 E1-c 会硬失败。E1-c 改做真正有用的一件：把 `hMuRe` 放宽成 `\b[a-z]+\.mu\.(R?)Lock\(`。

## 4. queue 生命周期归属

今天 queue 由 `HubOptions.Queue` 外部注入，`Hub` 只持引用、**从不关闭它**（`dispatch.MessageQueue` 无 Close，生命周期进程级，由 `main`/`buildServer` 持有）。**本 RFC 不改**：引擎同样只持引用。

引擎对 queue 的三类操作在 `drain()` 之后都不再发生：`Enqueue`（`sessionSend` 同步段，`TrackSend` 之前——`shuttingDown` 时**必须**先 `queue.Discard(key)` 归还所有权再返回 `sendAckBusy`，否则 key 永久 busy）、`DoneOrDrain`/`Discard`（`ownerLoop` 内，被 `wg.Wait()` 覆盖）、`Discard`（`/clear` `/new` 同步段）。

即 **queue 的所有者仍是组合根，引擎是唯一使用者**。把构造也收进引擎归 E2 #2552 之后。

## 5. 不变量

### 5.1 引擎不变量

1. **TrackSend 契约**：每个注册到 `wg` 的 goroutine 必须经 `TrackSend()` 并尊重 `shuttingDown`；禁止直接 `wg.Add(1)`。契约注释随字段迁到 `send_engine.go`。
2. **`sessionSend` 的 shuttingDown 分支**先 `queue.Discard(key)` 再返回 `sendAckBusy`。
3. **legacy fallback 门**：`e.queue == nil` 的 typed-nil 语义（#377）+ `legacyInvokes` 计数（#710）。
4. **`/clear` `/new`**：`queue.Discard` + `DiscardPassthroughPending` + `ResetAndDiscardOverride` 三件套顺序不变。
5. **panic 恢复点**：`ownerLoop` 的 recover + `handleOwnerLoopPanic`；两个远程代理 goroutine 的 recover + `serverMetrics.PanicRecovered()`。
6. **`informationalSendErr` 过滤**在 `sendErrorCallback` 里（§2.5）与 `runTurnPassthrough`（`send.go:389`）里各一份，都不能丢。
7. **附件 rollback 分界线**：`sessionSend` 受理后 `rollback = nil`（HTTP `:517`）/ `_ = wsRollback`（WS `:144`）的位置不能挪。
8. **`ctx` 永不为 nil**，且 `ctx` cancel **不是** `drain()` 的替代品——`drain` 是唯一关窗者。走 `HubOptions.ParentCtx` 的父级取消下，`e.ctx.Done()` 已关而 `e.closed` 仍 false，`TrackSend` 继续放行（这些 send 在 `GetOrCreate` 立刻失败，不是泄漏）。这是**既有行为**，但"cancel 在 Hub、closed 在引擎"让它更难看见，写进 godoc。
9. **8 个共享依赖构造后只读**，且与 Hub 侧指向同一实例。

### 5.2 HTTP 边界不变量（E1-b 的 review checklist）

v1 只列了引擎内部的不变量，而 E1-b 要在 `handleSend` 里动 8 处。下面这些一条都不能被顺手调整：

| 不变量 | 行 |
| :--- | :--- |
| `sendLimiter` 先于一切；multipart 额外过 `uploadLimiter`（注释算过账：30 req/min × 5 × 10 MB = 1.5 GB/min 灌进 CLI stdin） | 231-233 · 247-250 |
| `MaxBytesReader` 三档 22 MB / 12 MB / 2 MB；`handleBind` 64 KB | 255 · 256 · 291 · 539 |
| `rejectIfTooManyFields`（`maxMultipartFields=32`）紧跟 `ParseMultipartForm` | 261-263 |
| 内联 fan-out ≤2 + `maxFilesPerSend` **双检**（multipart 内 + 统一路径再一次） | 274-281 · 317-320 |
| `uploadOwnerOrFail` 在 `TakeAll` 之前，`ok=false` **必须 503** 而非 IP 兜底（#501/#1399） | 326-329 |
| **纯输入校验（key 非空 / `ValidateSessionKey` / `maxWSSendTextBytes`）必须早于 `TakeAll`**（`dashboard_send_validate_before_take_test.go` 钉住） | 335-352 vs 354-363 |
| `uploadStore.TakeAll` 原子性（全成或全不动） | 356 |
| `writeSendError(…, filesConsumed)` 与裸 `writeJSONStatus` 的分工（#2014：客户端据此丢过期 chip）。远程分支 `425/436/446` 用的是**不带** `filesConsumed` 的版本——今天靠"有 fileIDs ⇒ images>0 ⇒ 在 `374` 就 return"才不可达，是**脆平衡**，任何顺序调整都会变真 bug | 354 · 366 · 375 · 395 · 400 · 418 · 512 |
| 脱敏点共 6 处，一处不少：`SanitizeForLog(err,512)`（远程 ×2）· `SanitizeForLog(node,128)`（:485）· `SanitizeForLog(p.Workspace,200)`（send.go:200）· `SanitizeForLog(req.Workspace,200)`（:568，被 `dashboard_bind_test.go:252-270` 源码级钉住）· `session.SanitizeLogAttr(key)` ×3（:394 · :486 · :511） | — |
| `handleAttachment` 的穿越防线依赖 `h.engine.allowedRoot`（§2.7 的 validateWorkspace ×2 之一） | 687 · 664 · 704-741 |

## 6. 测试影响面（用可复现 grep 重建；v1 的枚举轴错了）

v1 用"手搓 `&Hub{}` 的 12 个文件"当轴，漏掉全部 `newTestHub()` 用户。正确的轴：

```
grep -rnE "hub\.(queue|guard|sendWG|sendClosed|sendTrackMu|legacySendInvokes|handleOwnerLoopPanic|sessionSend|runTurn|ownerLoop|TrackSend|autoSaveCronPrompt|httpSendErrorCallback)" internal/server/*_test.go
grep -rn "&SendHandler{" internal/server/*_test.go
```

| 文件 | 今天 | 改法 |
| :--- | :--- | :--- |
| `send_panic_notify_test.go:19,48,62,84` 写 `hub.queue`；`:32,56,72,98` 调 `hub.handleOwnerLoopPanic` | **v1 漏** | `hub.engine.queue = …` / `hub.engine.handleOwnerLoopPanic(…)` |
| `ws_test.go:655-656` `hub.guard.TryAcquire/Release` | **v1 漏** | `hub.engine.guard.…` |
| `wshub_legacy_send_invokes_test.go:25,32` `hub.queue`；`:54-56` `h.legacySendInvokes.Add` | 列了 | `hub.engine.queue`；nil-receiver 断言保留（§2.4） |
| `send_cron_prompt_autosave_test.go:60,85,99,102` `&Hub{scheduler:…}` + `autoSaveCronPrompt` | 列了，但处方错 | **不用** `&sendEngine{…}` 字面量（会重造零值坑并绕过 nopNotifier 安装）→ `newSendEngineForTest(sendEngineOpts{Scheduler: saver})` |
| `dashboard_send_async_error_test.go:134` `hub.httpSendErrorCallback(key)` | **v1 漏** | `hub.engine.sendErrorCallback(key)`（§2.5，这是那条过滤的唯一防线） |
| `dashboard_send_validate_before_take_test.go:48` `&SendHandler{uploadStore:…}`（nil hub，`:349` 返回） | 列了 | 补 `engine: hub.engine`（真引擎） |
| `dashboard_send_files_consumed_test.go:29` 同上（`:349` / `:374` 返回） | 列了 | 同上 |
| `dashboard_orient_test.go:61` `&SendHandler{…}`（nil hub，只走 handleOrient/handleUpload） | **v1 漏** | 同上（或确认不需要，写进注释） |
| `dashboard_send_ratelimit_msg_test.go:19-23` `&SendHandler{hub: hub, …}`，**有真 hub**，断言首发 202 | **v1 误判为 nil-hub** | `engine: hub.engine`。塞零值引擎会 panic：`newTestHub` 不接 Queue → `sessionSendLegacy` → nil guard |
| `nodeclient_test.go:341-386` `TestHandleAPISend_RemoteNode` | 未提 | 唯一真跑 60s 远程分支的测试，回归看它 |
| `consumer_contract_test.go` | 未提 | 加 `var _ sendEngineRouter = (*session.Router)(nil)` + `var _ sendNotifier = (*Hub)(nil)` |
| `shutdown_lock_order_test.go` | 提了但结论错 | **不加** `send_engine.go`（§3）；E1-c 放宽 `hMuRe` |
| `wshub_shutdown_order_test.go` | 未提 | 加 drain 的源序 pin（§3） |
| `log_level_test.go:23,46` · `send_sanitize_contract_test.go:69`（按文件名读 `send.go`） | 未提 | §2.2 方法不搬文件 ⇒ **无需改动**（这是 §2.2 的主要收益之一） |
| 其余 11 个手搓 `&Hub{}` 文件 | — | 不触及（`select_node_for_backend_test.go:266` 只测 `hubNodeLookup.NodeByID`，`wsclient_test.go:13` 只测 `SendRaw` 阈值） |

**新增** `send_engine_contract_test.go`（行为断言，不做 reflect 字段扫描——那归 §2.3 的 lint 规则）：① `drain()` 后 `TrackSend()` 返回 `shuttingDown=true` 且不 Add；② `-race` 下 `TrackSend` 与 `drain` 并发不逃逸；③ `drain()` 之后任何 notify 路径不再 `clientWG.Add`；④ Hub 与引擎的 8 个共享依赖同实例（对标 `hub_shared_state_test.go`）。

`-race` 必跑：`go test -race -timeout 480s ./internal/server/`（整包约 4 分钟）。

## 7. 分步 PR

| PR | 内容 | 中间态编译要点 |
| :--- | :--- | :--- |
| **E1-a** | `send_engine.go`（类型 + `newSendEngine` + `TrackSend`/`drain`，≈130 行）；`consumer.go` 加 `sendEngineRouter` + `sendNotifier` + 两条编译期断言；6 字段迁入、Hub 加 `engine`；10 个方法**只改 receiver**；`Hub.Shutdown` 调 `drain()`；`Hub.TrackSend` / `Hub.httpSendErrorCallback` 删除；`lookupNode` 移出 wshub_send.go；`send.go:112` → `s.hub.engine.sendWithBroadcast(…)` | `SendHandler` 仍持 `hub`，`dashboard_send.go` 一律走 `h.hub.engine.*` 并**保留 5 个 `if h.hub != nil` 守卫**（v1 把"删守卫"排在 E1-a 而 `h.engine` 要到 E1-b 才存在，编译不过） |
| **E1-b** | `SendHandler.hub` → `engine`；§2.7 的 11 处替换；`resolveAttachmentWorkspace` 改签名；`routes.go` 接线；6 个测试文件 | 5 处 nil 守卫在这一步归零 |
| **E1-c** | 收尾：`wshub_send.go` file-block 注释改 `READS-ALSO:`（并顺手订正两处已有漂移——它声称 WRITES `droppedTotal`，实际写在 `wsclient.go:190`；声称 `userSendLimitersMu`，**该字段已不存在**）；`wshub_subscribe.go:7` 的 `READS-ALSO: send block (sendClosed only)` 已是死注释（全包无该文件的代码引用），随字段迁移删除；`exemptions.yaml` 删 `send.go` 死条目、订正 `dashboard_send.go` 的 `current`、`wshub.go` 改挂 `until_phase: "K"`；放宽 `hMuRe`；把 §2.3 的 (i)(ii) 落成 lint 规则；订正 `design.md` §0 fact-table 的 Hub 字段数与 §五 字段块表的 `droppedTotal` 归属 | — |

`send_engine.go` **不受 `rule_field_block` 约束**（`rule_field_block.go:37` 只扫 `wshub` 前缀），且不需要——那条规则管的是 Hub 的字段块注释，引擎自己拥有字段。不为了骗覆盖率把文件命名成 `wshub_send_engine.go`。

## 8. Epic 验收贡献（E1 只兑现 1/4，先说清楚免得关 epic 时对账崩）

| Epic #2528 验收项 | E1 后 |
| :--- | :--- |
| Hub 不再拥有 send 块字段 | ✅ **勾**（在 issue 的 5 字段定义下；design.md 的 6 字段定义还剩 `droppedTotal`，见 §0.3） |
| `hub == nil` 守卫 13 → 0；wireup `init()` 2 → 0 | ❌ E1 清 5 留 8（`server.go`×4 / `send.go:111` / `wsclient.go:189` / `agent_tailer_registry.go:107` / `server_loops.go:99`），`init()` 未触及 → 归 E2 #2552。**顺手收益**：引擎抽出后 `Server` 可直接持引擎，把 `send.go:111` 那处也消掉——但那会动 headless Server 的形态（`send.go:114-116` 有一条故意 panic 的接线回归检测），留给 E2 |
| Server 字段 52 → ≤15 | ❌ E1 **不动 Server 任何字段**，0 贡献 |
| lint 规则 6 → ≤2；`until_phase` 豁免 11 → 0 | ❌ 规则数归 E4 #2554；豁免 E1-c 删 1 改挂 1，剩 10 |

issue #2551 自己的 5 条验收：第 1 条（Hub ≤47）✅、第 2 条（`SendHandler` 无 `hub`、`h.hub != nil` 归零）✅、第 3 条按 §2.3 **改判**、第 4 条按 §1 **订正**（只有 `wshub.go` 一条 4b）、第 5 条（锁序 + 排干 `-race`）✅。

## 9. 风险与回滚

| 风险 | 缓解 |
| :--- | :--- |
| 8 个共享依赖漏一个 → 生产 nil panic | `newSendEngine` 逐个记账；`sessionOptsFor` 用到的 `resolver`/`agents`/`projectMgr`/`scratchPool` 最易漏，契约测试用一次真实 `sessionSend`（stub router）覆盖 |
| `drain()` 位置被"整理"进某个临界区 → 确定性死锁 | §3 的三条前置写进 godoc + 源序 pin 测试；`-race` 抓不到这类，所以必须靠 pin |
| `sendErrorCallback` 被写成不带过滤的直调 → informational 错误 fan-out | §5.1 item 6 + 把 `dashboard_send_async_error_test.go:134` 改指引擎（唯一防线） |
| 零值引擎被后人引入 → fail-open / 进程崩 | 只暴露 `newSendEngine` / `newSendEngineForTest`；§2.7 的表写进 struct 注释 |
| 手搓 `&Hub{}` 测试 nil 引擎 | `Shutdown` 保留 `h.engine != nil`；§6 已逐文件核实 |

**回滚**：三个 PR 各自 revert 即可，无数据/协议迁移、无持久化状态变更。注意 E1-c 不要单独保留（§3 末：清单项对文件存在性硬依赖）。

## 10. 明确不做

1. **不搬包**。Phase 4b 重启是引擎边界稳定之后独立评估的事。
2. **不删 `sessionSendLegacy`**（R-LEGACY-SEND 的条件是"每个 fixture 都接 queue"）。
3. **不动 `hub == nil` 的另外 8 处**（含 `send.go:111`）→ E2 #2552。
4. **不拆 `dashboard_send.go`（851 行）** → E6 #2561。
5. **不动 Hub 的 Subscriber / Broadcast 子对象** → Epic K #2549。
6. **不改 `allowedRoot == "" 表示不限制`** 这条既有策略。
7. **不收窄 `HubRouter`**（引擎不再用它之后它可能可以瘦身，但那要核对 Hub 自身的用点）→ 记入 #2195。
