# RFC: eventlog 子系统统一——消解三层影子与 5 包散落

> **状态**: Draft v1（待评审）
> **作者**: naozhi team (cron-cr)
> **创建**: 2026-06-04
> **范围**: 收敛 cli ring / persist spool / naozhilog replay 三层"影子"事件存储到单一 `EventStore` 契约，并把散落在 5 个包的 eventlog 状态聚合到一个 `internal/session/eventlogpipeline` 装配点
> **关联 issue**: #737（state spread across 5 packages）, #1369（cli/eventlog.go vs persist+schema vs session/bridge 三层影子）
> **关联代码**:
> - `internal/session/eventlog_bridge.go`（EventEntry⇄persist.Entry 转换 + sink 构造 + naozhilog 源构造）
> - `internal/session/eventlog_health.go`（/health.eventlog 投影）
> - `internal/session/eventlog_orphans.go`（orphan sweep）
> - `internal/session/eventlog_metrics.go`（metrics observer）
> - `internal/session/router_core.go:1016/1038/1040/1285/1297`（Persister 装配 + Tier1 加载）
> - `internal/session/router_lifecycle.go:139/1008/1016/1017`（mergeWithEventLog 装配 + SetPersistSinkPair 装配）
> - `internal/cli/eventlog.go`（in-memory ring，cli.EventLog / EventEntry / PersistSink）
> - `internal/eventlog/persist/persister.go`（on-disk spool）
> - `internal/eventlog/schema/`（wire format）
> - `internal/eventlog/api/api.go`（**已存在**的 behaviour-free 目标接口 EventStore/Appender/Reader/Subscriber）
> - `internal/history/naozhilog`（replay reader）
> - `internal/attachment/tracker`（refcount tracker，与 sink 同生命周期）
> **先例**: `docs/rfc/eventlog-split.md`（ARCH-EVENTLOG-SPLIT，Implemented v1.1 —— 已完成 cli/eventlog.go 单文件 6 拆，本 RFC 不重复该工作）

---

## 1. 背景与问题（Background & problem）

### 1.1 可复现的"三层影子"现状（#1369）

naozhi 的事件日志在概念上是单一契约——"append、按 range 读、subscribe tail"——但今天它被实现为**三个相互影子（shadow）的层**，每层各自拥有一套 append/read/subscribe 原语：

- **tier-1 内存 ring** —— `cli.EventLog.entries`（`internal/cli/eventlog.go:101-103`），纯 RAM 有界环，进程退出即丢。生产者唯一入口。
- **tier-2 持久 spool** —— `persist.Persister`（`internal/eventlog/persist/persister.go`），经 bridge 的 `PersistSink` 落盘，重启后权威。
- **tier-3 replay 读** —— `naozhilog.Source`（`internal/history/naozhilog`），会话重连时从 spool 回填 ring（history 面板、dashboard rewind）。

三者的"影子"性质有源码内自陈证据。`internal/session/eventlog_bridge.go:1-42` 的包头 NEEDS-DESIGN 注释（R243-ARCH-12, REPEAT-5）逐字写道：

> 四个具体后端（memory ring、persist spool、naozhilog source、scratch event store）各自暴露略有差异的 API，尽管它们的概念契约完全相同。

`internal/cli/eventlog.go:1-31` 的文件头（R237-ARCH-13, #610）独立记录了同一问题，并给出长期方向"rename 到 `internal/eventlog/{ring, persist, replay}` 让包名对齐数据流位置"。两处注释指向同一未落地的统一计划。

### 1.2 状态散落在 5 个包（#737）

eventlog 的**运行态与装配逻辑**横跨 5 个包，且会话层（`internal/session`）不得不手工编排所有后端：

| 关注点 | 所在 | 锚点 |
|--------|------|------|
| EventEntry⇄persist.Entry 转换 + sink 构造 | `internal/session` | `eventlog_bridge.go:187/310/348` |
| naozhilog 源构造 + merged 装配 | `internal/session` | `eventlog_bridge.go:69/81`，`router_lifecycle.go:139` |
| /health 投影 | `internal/session` | `eventlog_health.go:45/79` |
| orphan sweep | `internal/session` | `eventlog_orphans.go:57/131` |
| Persister 生命周期装配 | `internal/session` | `router_core.go:1036-1053` |
| sink 装配（SetPersistSinkPair） | `internal/session` | `router_lifecycle.go:1008-1017` |
| in-memory ring + PersistSink 契约 | `internal/cli` | `eventlog.go`（及 split 出的 5 文件） |
| on-disk writer | `internal/eventlog/persist` | `persister.go` |
| wire format | `internal/eventlog/schema` | `record.go`/`idx.go` |
| replay reader | `internal/history/naozhilog` | — |

`internal/session` 一个包就吃下了 bridge / health / orphans / metrics / router 双站点装配——它既是 cli、persist、naozhilog、tracker、merged 的**公共下游汇聚点**，又是唯一的编排者。

### 1.3 为何是问题

1. **新后端无法注入**：`internal/eventlog/api/api.go:5-15` 自陈"A new backend cannot be registry-injected; cross-tier consistency is maintained by hand."（R20260602-091302-ARCH-2, #1570）。要加 kiro/未来后端，必须在 bridge + router 双站点手工 re-plumb。
2. **跨层一致性靠手维护**：四个后端的"append/read/subscribe"语义须人工保持同形，没有编译期契约兜底。
3. **import 边耦合脆弱**：cli 既不 import persist 也不被 persist import（`eventlog.go:16-19`），转换只能挤在 session bridge 这一处；任何 cli.EventEntry JSON shape 变更都得经 bridge 中转，约束写在注释里而非类型里。
4. **review 噪声**：session 包被迫承载与"会话路由"无关的 eventlog 装配，god-package 化趋势明显。

> **已完成的前置工作**：`docs/rfc/eventlog-split.md`（Implemented v1.1）已把 cli/eventlog.go 2289 行按职责拆成 6 文件——本 RFC **不**重复文件拆分，目标是更上一层的**跨包接口收敛 + 装配点聚合**。同时 `internal/eventlog/api/api.go` 已 behaviour-free 落地目标接口，本 RFC 的核心是**让后端实现并让 bridge/router 消费这些接口**。

---

## 2. 目标与非目标（Goals & non-goals）

### 目标

- **G1**：让现有四个后端（cli ring、persist spool、naozhilog replay、merged）显式实现 `internal/eventlog/api` 的 `EventStore`（或其分面 `Appender`/`Reader`/`Subscriber`），把今天靠注释维护的"概念同形"升格为编译期契约。
- **G2**：把 session 包里 eventlog 的装配逻辑（bridge sink 构造、Persister 生命周期、merged 源装配、health 投影、orphan sweep、metrics）聚合到一个新的 `internal/session/eventlogpipeline` 子包（或等价单一装配 facade），让 `router_core.go` / `router_lifecycle.go` 只调一个装配入口，不再散落 5+ 站点。
- **G3**：保留所有已 pin 的性能热路径（§5），统一**不得**回归 bench。
- **G4**：保留所有 on-disk 格式与 /health 线格式（§7），零迁移。

### 非目标（防 scope creep）

- **NG1**：**不**做 `internal/eventlog/{ring,persist,replay}` 的物理 rename（R237-ARCH-13 长期项）。rename 牵动 ~20 个消费方的 import，单独成 RFC。
- **NG2**：**不**改 `cli.EventEntry` / `schema.Record` 的 JSON/wire shape。
- **NG3**：**不**引入 registry 运行时后端动态注册（#1570 的终态）；本 RFC 只到"后端实现接口 + 装配收敛"，registry 注入是后续轮次。
- **NG4**：**不**改任何锁粒度（`l.mu` / `subMu` / persister channel）。
- **NG5**：**不**新增 config flag、不改 on-disk schema 版本。
- **NG6**：**不**重做 eventlog-split 已完成的 cli 文件拆分。
- **NG7**：**不**碰 attachment tracker 的 refcount 算法（仅保持其与 sink 同生命周期装配）。

---

## 3. 备选方案（Alternatives considered）

### 方案 A：物理 rename `internal/eventlog/{ring,persist,replay}` 优先（R237-ARCH-13 路线）

把 cli.EventLog 物理迁出 cli 到 `internal/eventlog/ring`，naozhilog 迁到 `internal/eventlog/replay`，让包名对齐数据流。

- **优点**：包名自解释，长期最干净。
- **缺点**：cli.EventLog 被 ~20 个文件以 `cli.EventLog`/`cli.EventEntry`/`cli.PersistSink` 引用（见 eventlog-split.md §5.3）；rename 是巨型 move + 全仓 import 重写，且 cli 包内 EventLog 与 Process 紧耦合（`realProc.EventLog()`），迁出会扯出 Process 的依赖。**高风险、高噪声、不解决 #737 的装配散落**（rename 后 session 仍是手工编排者）。**否决**——留给独立 RFC。

### 方案 B：接口契约收敛 + 装配聚合（**选中**）

不动包的物理位置，分两条独立轴推进：

1. **接口轴**：四后端实现已存在的 `internal/eventlog/api.EventStore`；bridge/router 改为面向 `api` 接口而非具体指针装配。
2. **装配轴**：新建 `internal/session/eventlogpipeline`，把 bridge/health/orphans/metrics/router 装配代码迁入，router 只调单一 facade。

- **优点**：增量、可分 phase；接口轴和装配轴互不阻塞；直接解 #737（装配聚合）与 #1369（编译期契约消影子）；`api` 包已 behaviour-free 落地，第一步只是"让 cli.EventLog 通过编译期断言满足接口"，零运行时改动。
- **缺点**：包物理位置仍不对齐数据流（NG1 接受这点）；session 仍持有 pipeline 子包（但已从散落收敛为单点）。

**为何 B 胜出**：B 把"难且高风险的物理搬迁"（A）与"真正解决散落与影子的契约+装配"解耦。`api.go` 已存在且 import 周期已验证 cycle-free（cli 下游于 schema，不 import api），意味着接口轴的第一步是**纯编译期断言**，可零风险落地，立即把"概念同形"变成 CI 守得住的事实。装配轴可在接口稳定后跟进。

### 方案 C：维持现状 + 加强注释/lint

只在 bridge 写更强的注释、加 lint 规则禁止新后端绕过 bridge。

- **缺点**：不解决任何根因；#737/#1369 的"手工维护一致性"风险原样保留。**否决**。

---

## 4. 测试策略（Test strategy）

### 4.1 接口轴（G1）

- **编译期断言（unit）**：在 `internal/eventlog/api` 新增 `api_assert_test.go`，对每个后端写 `var _ api.Appender = (*cli.EventLog)(nil)` / `var _ api.Subscriber = (*cli.EventLog)(nil)` / `var _ api.Reader = (*naozhilog.Source)(nil)` 等。这是把"概念同形"钉死的最低成本守卫——任何后端漂移立刻编译失败。
  - 注意：断言测试若放在 `api` 包内会引入 `api → cli`（已有）与 `api → naozhilog` 边，须确认不成环（naozhilog 不 import api/cli-ring 反向）。若成环，断言改放 `internal/eventlog/api/assert`（独立测试包）或各后端自身的 `_test.go` 内。
- **接口 round-trip（unit）**：对满足 `EventStore` 的后端，table-driven 验证 `Append → SubscribeNew 收到 → Reader.LoadBefore 读回` 的顺序契约（oldest→newest），覆盖空、单条、批量、ring 溢出四态。

### 4.2 装配轴（G2）

- **装配 facade 单测**：`eventlogpipeline_test.go` 验证给定 `(eventLogDir, persister, tracker)` 装配出的 sink/merged-source/health 投影与迁移前逐字节一致（用现有 `eventlog_integration_test.go` 作 golden 基线对照）。
- **回归 pin（最关键）**：迁移属 move + adapter，必须证明零语义改动。沿用 eventlog-split.md §6 手法——
  - sink 字节流 pin：现有 `internal/cli/eventlog_persist_sink_test.go` / `eventlog_append_batch_*_test.go` 保持绿。
  - bridge 转换 pin：`eventlog_localsource_test.go` / `eventlog_integration_test.go` 保持绿，断言迁移后 EventEntry⇄persist.Entry 仍逐字节等价。
  - ordering pin：SetPersistSink-after-InjectHistory 的 router 测试（`router_lifecycle.go:826/839` 注释引用的 CI anchor）保持绿。
  - replay-phase pin：`replayInvokeTotal` 守卫相关测试（`eventlog.go:261-275`）保持绿。

### 4.3 防回归机制

- 每个 phase commit 独立跑 `go build ./... && go vet ./internal/... && go test -race ./internal/cli/... ./internal/session/... ./internal/eventlog/...`。
- 接口断言测试一旦加入，即成为"新后端必须实现 EventStore"的 CI 闸门——直接防止 #1369 的影子再生。
- 装配迁移**只允许 move + 同包重命名**，禁止改方法体；用 `grep -c '^func '` 前后计数核对（eventlog-split.md §6 (A) 同款机械命令）。

---

## 5. 风险与回滚（Risk & rollback）

### 5.1 出错会 break 什么

- **接口轴**：若把后端方法签名改成"适配接口"而非"接口适配现状"，会破坏 `cli.EventLog.Append(e)` / `SubscribeNew()` 的现有签名与 ~20 个消费方。**缓解**：`api` 接口已按现状方法名（`Append`/`AppendBatch`/`SubscribeNew`/`LoadBefore`）声明（见 `api.go:34-55`），后端"恰好满足"无需改签名；断言测试若编译失败说明走偏，立即停手。
- **装配轴**：sink 装配顺序错位 → replay 写回环（持久化 InjectHistory 的回放）。**load-bearing 不变量**：`SetPersistSink 必须晚于 InjectHistory`（`eventlog.go:224`、`router_lifecycle.go:839` R242-ARCH-11 #733、persister.go:218/561）。迁移时此调用序必须原样保留在 facade 内。
- **EventEntry⇄persist.Entry 转换**：bridge 的 borrowed-bytes 别名契约（`eventlog_bridge.go:218-298`，out 的 JSON 字段 alias 池化 buffer，persisterSink 同步 copy 后才可回收）极其精细，move 时整块搬迁、不得拆解。

### 5.2 load-bearing 并发/锁/on-disk 不变量与已有 pin 测试

| 不变量 | 锚点 | 已有 pin |
|--------|------|---------|
| ring 单调推进，`entries`/`head`/`count` 在 `l.mu` 下 | `eventlog.go:101-106` | cli ring pos 测试 `eventlog_agent_ring_pos_test.go` |
| `subMu` 下 "channel closed exactly once" | `eventlog.go:205-219` | `eventlog_subscribe_after_close_test.go` |
| SetPersistSink 晚于 InjectHistory（replay 守卫） | `router_lifecycle.go:839`, `persister.go:218` | router ordering 测试 + `replayInvokeTotal` |
| bridge borrowed-bytes：persisterSink 同步 copy 后才回收 pool | `eventlog_bridge.go:291-298` | `eventlog_append_batch_*_test.go`、`accept_borrowed_bytes_test.go` |
| 单条 sink 与批量 sink 字节同形（#410） | `eventlog_bridge.go:310/348` | `eventlog_persist_sink_one_test.go` |
| persister channel 深度 / drain / fsync | `persister.go` Stats | `tick_flush_*_test.go`、`parallel_fsync_test.go` |
| orphan sweep age 长于 attachment refTTL | `eventlog_orphans.go:27` | `eventlog_orphans_test.go` |
| 性能池化热路径（R215/R228/R240 PERF） | `eventlog_bridge.go:94-155` | `eventlog_bridge_scratch_pool_test.go`、`*_bench_test.go` |

### 5.3 回滚

- 每 phase 独立 commit，move-only。接口断言 phase 若回退，`git revert` 单 commit 即可（无运行时影响）。
- 装配迁移 phase 回退：因属 move，revert 即恢复原 5 站点装配；on-disk 与 wire 格式从未改动，无数据回滚。
- **禁止** `git checkout <单文件>` 回退已 move 的文件（会与新位置重复，eventlog-split.md §7 教训①同型）。

---

## 6. 可观测性（Observability）

- **无新增 metric/log（接口轴）**：纯编译期契约，运行时零变化。
- **装配轴保持现有投影逐字节不变**：
  - `/health.eventlog`（`eventlog_health.go:17` EventLogHealth）字段集不变：WriterAlive/ChannelDepth/ChannelCap/Written/Dropped/Fsyncs/Malformed/ReplayLeak/FSType/FSSupported。线格式见 `internal/server/health.go:110` healthEventLogStats，须保持 mirror。
  - `/health.attachment_tracker`（`eventlog_health.go:91`）同样不变。
  - metrics observer（`eventlog_metrics.go` eventLogMetricsObserver）随装配迁入 pipeline，但发射的指标 key/语义不变。
- **可选诊断**：可在接口断言落地时，于 startup 日志加一行 debug 级"eventstore backends: ring/persist/replay satisfy api.EventStore"，纯信息性，默认 debug 不影响生产。本 RFC 不强制。

---

## 7. 兼容性与迁移（Compatibility & migration）

- **on-disk 格式**：零改动。`<keyhash>.log` / `.idx` 的 framing、schema.Record wire version（`schema.WireVersion`/`MinReadVersion`）均不动（NG2/NG5）。重启后旧 spool 原样可读。
- **/health 线格式**：零改动（§6）。dashboard/monitor 消费方无感。
- **config flag**：无新增（NG5）。`EventLogDir`/`EventLogPersister`/`EventLogDevMode`/`EventLogGenerator` 的 router Config 字段与语义不变。
- **API 兼容**：cli.EventLog / EventEntry / PersistSink 等导出符号签名不变（接口"恰好满足"，不改签名）。~20 个消费方（session/server/dashboard/node/history/sysession）无需改。
- **迁移路径**：无运行时迁移。接口轴是 additive（加断言测试 + adapter）；装配轴是 move。两轴均可灰度落地、随时停在任一 phase。

---

## 8. 上线计划（Rollout plan）

非 flag-gated（无运行时行为变化），分 phase 按"先零风险接口断言、后装配 move"推进。每 phase 一个 commit，独立 build/vet/test-race 绿。

| Phase | 动作 | 文件级改动 | 估算行数 | 风险 | 可独立落地 |
|-------|------|-----------|---------|------|-----------|
| **1** | 接口断言：让四后端编译期满足 `api.EventStore`/分面 | 新增 `internal/eventlog/api/api_assert_test.go`（或独立 assert 测试包）；若 cli.EventLog 缺某方法则**仅在缺口处**加 thin adapter（预计无缺口——`api.go` 按现状方法名声明） | ~60-120（测试为主，零生产逻辑） | **low** | **是** |
| 2 | 文档对齐：更新 bridge/cli/persist 三处 doc.go 注释，指向 `api.EventStore` 为既成契约（不再是"proposed"） | `eventlog_bridge.go` 头注、`cli/doc.go`、`persist/doc.go`、`schema/doc.go` 注释 | ~40（纯注释） | low | 是 |
| 3 | 装配 facade 雏形：新建 `internal/session/eventlogpipeline`，先迁 health 投影（纯读、无副作用，最易隔离） | 新增 `eventlogpipeline/health.go`；`eventlog_health.go` 改为转调 | ~120 | low | 是（health 独立） |
| 4 | 迁 orphan sweep + metrics observer | `eventlogpipeline/orphans.go`、`/metrics.go`；router 转调 | ~180 | medium | 评审后 |
| 5 | 迁 bridge sink 构造 + merged 源装配（**§5.1 borrowed-bytes / ordering 强耦合**） | `eventlogpipeline/sink.go`、`/source.go`；`router_lifecycle.go` 装配转调 | ~250 | medium-high | **同轮 review** |
| 6 | 迁 Persister 生命周期装配，router 只剩单一 `pipeline.Attach(r)` 入口 | `router_core.go` 装配收敛 | ~120 | medium | 评审后 |

### Phase 1 可独立安全落地：**是**

**确切做什么**：在 `internal/eventlog/api/` 下新增编译期断言测试，对四个后端各写 `var _ api.Appender = (*cli.EventLog)(nil)`、`var _ api.Subscriber = (*cli.EventLog)(nil)`、`var _ api.Reader = (*naozhilog.Source)(nil)`、`var _ api.EventStore = ...`（凡同时满足三分面者）。先 `go build` + `go test ./internal/eventlog/api/...` 验证后端**当前签名恰好满足**这些接口（`api.go:34-55` 已按现状方法名声明，预期零缺口）。仅当某后端缺某方法时，才在该后端包内加一个 thin、零逻辑的 adapter 方法（如 ring 缺 `LoadBefore` 则薄封装现有 `EntriesBefore`），并就近补单测。

**为何 low 风险**：纯 additive 测试代码，零生产行为改动，不碰任何 §5.2 不变量；编译失败即立即暴露走偏。落地后立即获得 CI 闸门，防 #1369 影子再生。唯一需核实点是断言测试放置位置不引入 import 环（§4.1 已给出 fallback 到独立测试包的预案）。

> **注**：triage 已定调本轮为纯 RFC 产出，**不**落地 phase-1 代码。上表 Phase 1 描述的是"若落地，第一步确切做什么"及其风险评估，待评审通过后另起实施轮次。

---

## 9. 附录：本 RFC 与既有 RFC 的边界

- `docs/rfc/eventlog-split.md`（Implemented v1.1）：完成 cli/eventlog.go **文件内**职责拆分。本 RFC 接力做**跨包接口收敛 + 装配聚合**，不重叠。
- R237-ARCH-13（物理 rename）：本 RFC §2 NG1 明确不做，留独立 RFC。
- #1570 registry 运行时注入：本 RFC §2 NG3 明确不做，是本 RFC 接口轴落地后的后续终态。
