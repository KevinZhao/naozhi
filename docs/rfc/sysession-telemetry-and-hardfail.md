# RFC: sysession — adopt runtelemetry.Broadcaster seam + unify hard-fail policy

> **状态**: Draft v1（待评审）
> **作者**: naozhi team (cron-cr)
> **创建**: 2026-06-04
> **范围**: 让 sysession 收敛到 cron 已落地的 `runtelemetry.Broadcaster` 生产者范式（删掉平行的 `OnRun*` 闭包 + 镜像事件结构体 + routes.go 翻译 shim），并就 `Manager.Stop` 的 `os.Exit(2)` hard-fail 策略与 cron 的 best-effort drain 之间的分歧给出收敛方向。
> **关联 issue**: #1723, #1169, #1055
> **关联代码**:
> - `internal/runtelemetry/broadcaster.go:20`（`Broadcaster` 接口，2 方法）
> - `internal/runtelemetry/event.go:12,65`（`RunStartedEvent` / `RunEndedEvent`）
> - `internal/sysession/manager.go:88-93`（`Config.OnRunStarted/OnRunEnded`）、`:105-113`（`Config.OnHardFail`）、`:252-253`（`onRunStarted/onRunEnded atomic.Pointer[holder]`）、`:286-287`（默认 `OnHardFail = os.Exit`）、`:504-529`（Stop drain-deadline → `OnHardFail(2)`）、`:560-599`（`SetCallbacks` + `loadOnRun*`）、`:694-701` / `:809+`（emit 调用点）
> - `internal/sysession/run.go:117-135`（`DaemonRunStartedEvent` / `DaemonRunEndedEvent`）
> - `internal/sysession/hook_holders.go`（`onRunStartedHolder` / `onRunEndedHolder`）
> - `internal/sysession/registry.go:76-89`（`builtinDaemons` slice 字面量 — #1055）
> - `internal/cron/scheduler.go:170-175,516`（`Telemetry atomic.Pointer[runtelemetry.Broadcaster]`）、`internal/cron/scheduler_callbacks.go:80-134`（`loadTelemetry` + `emitRunStarted/emitRunEnded` 生产者范式）
> - `internal/server/routes.go:173-209`（`newHubBroadcaster` 接线 + sysession 的内联翻译 shim）
> - `internal/server/hub_broadcaster.go`（`hubBroadcaster` 消费者实现）

---

## 1. Background & problem

`runtelemetry` 在 cron↔sysession 调度层合并（`docs/rfc/cron-sysession-merge.md`）中被引入，作为两个调度器共享的 run 生命周期事件层。合并落地后 **cron 已完整收敛**到这个 seam，而 **sysession 只收敛了一半**——它共享了 `runtelemetry` 的*值类型*（`RunState` / `ErrorClass` / `TriggerKind` 都是 type alias，见 `run.go:20,50,78`），但 *没有* 共享生产者范式，仍维护一套平行的回调 + 事件结构。

### 1.1 #1723 — sysession 携带平行的 telemetry 管道（可复现的代码事实）

cron 的生产者范式（已落地，作为对照基准）：

- `internal/cron/scheduler.go:516`：`telemetry atomic.Pointer[runtelemetry.Broadcaster]`
- `internal/cron/scheduler_callbacks.go:83`：`loadTelemetry()` — 集中处理 `atomic.Pointer[*Broadcaster]` 的 nil-deref 舞蹈
- `internal/cron/scheduler_callbacks.go:99,116`：`emitRunStarted/emitRunEnded` — 把 cron-local 事件翻译成 `runtelemetry.RunStartedEvent{Subsystem: SubsystemCron, ...}` 后通过 broadcaster 转发；nil broadcaster 静默丢弃
- 消费侧：`internal/server/routes.go:173`：`telemetry := newHubBroadcaster(s.hub)` + `s.scheduler.SetTelemetry(telemetry)`

sysession 的平行管道（要收敛的对象）：

- `internal/sysession/manager.go:88-93`：`Config.OnRunStarted func(DaemonRunStartedEvent)` / `OnRunEnded func(DaemonRunEndedEvent)`
- `internal/sysession/manager.go:252-253`：`onRunStarted atomic.Pointer[onRunStartedHolder]` / `onRunEnded atomic.Pointer[onRunEndedHolder]`
- `internal/sysession/hook_holders.go`：`onRunStartedHolder{fn}` / `onRunEndedHolder{fn}` —— 仅为满足 `atomic.Pointer` 需要具体 pointee 类型而存在的样板
- `internal/sysession/manager.go:560-599`：`SetCallbacks` + `loadOnRunStarted/loadOnRunEnded`
- `internal/sysession/run.go:119-135`：`DaemonRunStartedEvent` / `DaemonRunEndedEvent` —— 字段逐一镜像 `runtelemetry.RunStartedEvent` / `RunEndedEvent` 的子集（`Name`↔`OwnerID`、`RunID`、`Trigger`、`StartedAt` / `State` / `DurationMS` / `ErrorClass`）

**最关键的症状**：因为 sysession 不直接生产 `runtelemetry` 事件，`routes.go:183-209` 不得不背一段内联翻译 shim——把 `sysession.DaemonRunStartedEvent` 重新打包成 `runtelemetry.RunStartedEvent{Subsystem: SubsystemSysession, OwnerID: ev.Name, ...}` 再喂给*同一个* `hubBroadcaster`。routes.go 自己的注释（`:177-181`）已经承认这是临时状态：

> "sysession.Manager keeps its own SetCallbacks API for now (its atomic.Pointer holder shape would need its own refactor); the callbacks here translate sysession.DaemonRun\*Event into runtelemetry events..."

于是同一个 `Subsystem=SubsystemSysession` 的 `OwnerID`/`Trigger`/`ErrorClass` 映射逻辑（`mapSysessionTrigger` / `mapSysessionRunState` / `mapSysessionErrorClass`）被劈成两半：值类型已经统一（alias），但映射函数仍活在 server 包，而 cron 的等价映射（`runtelemetry.TriggerKind(ev.Trigger)` 退化 no-op cast）活在 cron 包自己的 `emitRun*` 里。**这是问题**：新增一个 sysession run 字段要同时改 `run.go` 的结构体、`manager.go` 的 emit 调用点、`routes.go` 的翻译 shim 三处；而 cron 加字段只改 cron 包内两处。两个调度器对"同一个事件层"维护了不对称的接入成本，正是合并 RFC 想消灭的反向耦合的残留。

### 1.2 #1169 — hard-fail 策略发散

- sysession：`internal/sysession/manager.go:286-287` 默认 `cfg.OnHardFail = os.Exit`；`:504-529` `Stop` 在 `stopCtx` 超时（daemon 不honorctx）时调用 `m.cfg.OnHardFail(2)`，默认把**整个进程**拉下来。理由记录在 `:505-506`：force-exit 防止泄漏的 goroutine 写已拆毁的 router。
- cron：`internal/cron/scheduler.go:1744-1751` `stopWithCtx` 的 drain 是 **best-effort**——每个 drain 阶段超时只 Warn + bump 一个 budget counter，编排器本身不含预算算术，"orphaned wrapper goroutines die with the process exactly as the Stop() CONTRACT block documents"（`scheduler.go:1718`）。cron **从不** `os.Exit`。

**这是问题**：两个共生于同一进程、由 `cmd/naozhi` 编排的调度器，对"drain 超时"这一相同事件采取相反策略。如果 sysession 的 daemon 卡住触发 `os.Exit(2)`，cron 正在 drain 的 best-effort 持久化（`persistOnShutdown`，对已返回 2xx 的 mutation 是 last-write）可能被腰斩。策略发散使得 `cmd/naozhi` 的关停顺序变成一个隐式的、未文档化的不变量。

### 1.3 #1055 — 注册风格分歧

`internal/sysession/registry.go:76-89` 用一个 `var builtinDaemons = []builtinDaemonFactory{...}` slice 字面量做编译期注册；项目内其他注册点（如 history / channel adapter 的 blank-import `_ "pkg"` 自注册风格）形式不同。**这是 cosmetic/一致性问题，不是 bug**：slice 字面量风格本身工作良好，`validateBuiltinDaemonNames`（`:99`）已在 `NewManager` 做了启动期校验。它的存在只是"项目里有两种注册习惯"，需要的是一个方向决策，而不是紧急修复。

---

## 2. Goals & non-goals

### Goals
- **G1 (#1723)**：让 sysession 成为 `runtelemetry` 事件的*直接生产者*——在 `sysession.Manager` 内部持有 `runtelemetry.Broadcaster`、内部完成 `Subsystem=SubsystemSysession` 的事件构造，删除 `routes.go` 的内联翻译 shim，使 sysession 的接入成本与 cron 对称。
- **G2 (#1723)**：删除 `DaemonRunStartedEvent` / `DaemonRunEndedEvent` 镜像结构体、`onRunStartedHolder` / `onRunEndedHolder` 样板、以及 `OnRunStarted`/`OnRunEnded`/`SetCallbacks` 平行 API。
- **G3 (#1169)**：为 cron 与 sysession 的 drain-超时 hard-fail 策略给出一个**显式收敛方向**（决策 + 理由），消除隐式关停顺序依赖。
- **G4 (#1055)**：为 daemon 注册风格给出一个**方向决策**（保留 slice 字面量 vs 改 blank-import），并记录理由。

### Non-goals（防 scope creep）
- **NG1**：不改 WS wire 形状。`daemon_run_started` / `daemon_run_ended` 的 JSON payload（`hub_broadcaster.go` 的 `BroadcastDaemonRun*`）逐字节不变——本 RFC 是生产者侧重构，消费者侧 `Hub.BroadcastDaemonRun*` 与 dashboard.js 完全不动。
- **NG2**：不改 `ErrorMsg` 的安全策略（sysession 在 wire 上 drop `ErrorMsg`，见 `event.go:56-61` / `hub_broadcaster.go` SECURITY 注释）。收敛后该策略仍由 `hubBroadcaster` 的 `SubsystemSysession` 分支拥有。
- **NG3**：不把 sysession 持久化到磁盘（run history 仍是内存 ring buffer，见 `run.go:92-93`）。
- **NG4**：不重设计 `runtelemetry.Broadcaster` 接口本身、不新增 `Subsystem`。
- **NG5（关键）**：**不在本轮真正改 hard-fail 行为代码**。#1169 涉及 cron/sysession 跨包关停语义 + 已 pin 的 `os.Exit` 不变量（见 §5），属于"跨包接口重设计"，本轮只输出决策，代码留待独立评审 + 独立 PR。
- **NG6**：不引入 config flag 切换 telemetry 路径——收敛是 all-or-nothing 的内部重构，不需要灰度（见 §7）。

---

## 3. Alternatives considered

### 3.1 #1723 telemetry 收敛方向

**方案 A（选中）— 让 sysession 持有 `runtelemetry.Broadcaster`，复刻 cron 的 `SetTelemetry`/`loadTelemetry`/`emitRun*` 范式。**

`Manager` 用 `telemetry atomic.Pointer[runtelemetry.Broadcaster]` 替换 `onRunStarted/onRunEnded atomic.Pointer[holder]`；新增 `SetTelemetry(runtelemetry.Broadcaster)` + 私有 `loadTelemetry()` + `emitRunStarted/emitRunEnded`（内部构造 `Subsystem=SubsystemSysession` 事件并跑现有的 `mapSysession*` 映射）。`routes.go` 退化成一行 `s.sysessionMgr.SetTelemetry(telemetry)`，与 `s.scheduler.SetTelemetry(telemetry)` 对称。`mapSysessionTrigger/RunState/ErrorClass` 三个映射函数从 server 包搬到 sysession 包（它们消费的是 sysession 的 `Daemon*` alias 常量，本就属于 sysession 领域）。

- 优点：与 cron 范式 1:1 对称；删除 routes.go shim + 镜像结构体 + holder 样板；映射逻辑回到 owning 包；新增字段只改 sysession 包内。
- 缺点：触及 `Manager` 的并发原语（`atomic.Pointer` 换型），需要重新 pin 并发不变量（见 §5）。

**方案 B（否决）— 保留 `DaemonRunStartedEvent`，但把 routes.go 的翻译 shim 下沉成 sysession 包的导出 helper `sysession.ToRunStartedEvent(ev)`。**

- 优点：改动更小，不动 `Manager` 并发原语。
- 缺点：治标不治本——镜像结构体 + holder 样板 + `SetCallbacks` 平行 API 全部保留，sysession 仍不是直接生产者，新增字段仍要改三处。没有消灭 routes.go 注释承认的不对称。**否决**。

**方案 C（否决）— 把 sysession 的 emit 逻辑也提取进 `runtelemetry` 公共层（共享 `emit*` helper）。**

- 缺点：cron 与 sysession 的 emit 各自带不同的 metric bump（cron `:100` bump `CronRunStartedTotal`）、不同的 OwnerID 语义。强行共享 emit 会把 subsystem 分支塞进 runtelemetry，违反 broadcaster.go:6 的"producer MUST NOT call back"边界精神且超出本轮范围（属于合并 RFC 后续 phase）。**否决**。

→ **选 A**：它是唯一真正消除 #1723 描述的不对称、且有 cron 已落地实现作为逐行模板的方案。

### 3.2 #1169 hard-fail 收敛方向

**方案 A（选中作为*建议方向*，但代码不在本轮落地）— sysession 向 cron 看齐：drain 超时降级为 Warn + budget counter + 让 goroutine 随进程死，移除默认 `os.Exit(2)`。**

理由：cmd/naozhi 是单进程编排者，关停时不应由任一子系统单方面 `os.Exit`；cron 已证明 best-effort drain + "orphaned goroutine die with process" 在 systemd `TimeoutStopSec` 下是安全的。统一到这个语义后，`cmd/naozhi` 的关停顺序不再隐式依赖"谁先 exit"。

**方案 B（否决）— cron 向 sysession 看齐：drain 超时也 `os.Exit`。**

- 缺点：会破坏 cron 已 pin 的"persistOnShutdown ALWAYS runs / 已返回 2xx 的 mutation 永不丢失"契约（`scheduler.go:1660,1718`）。倒退。**否决**。

**为何代码不在本轮落地（诚实评估）**：sysession 的 `os.Exit` 路径被三个 pin 测试硬钉死——`manager_onhardfail_default_test.go`（默认必须直接指向 `os.Exit`，#1287）、`manager_stop_onhardfail_detach_test.go`（embedder no-op 契约，#585）、`manager_stop_onhardfail_panic_test.go`（recover frame，#1286）。改默认语义要重写这三个 pin + 重新论证"router 被拆毁后泄漏 goroutine 写入"这个 sysession 当初引入 force-exit 的原始风险在 best-effort 模式下如何被消除。这是跨包关停语义重设计，必须先独立评审。**本轮只给方向，hasLandablePhase1 不含 #1169。**

### 3.3 #1055 注册风格

**方案 A（选中）— 保留 slice 字面量，明确记为"sysession 标准"，不改。** 它有编译期 `validateBuiltinDaemonNames` 守卫，daemon 数量少（2 个），slice 字面量比 blank-import 自注册更易读、更易测（顺序确定性，`registry.go:69-72` 注释已说明）。blank-import 风格的价值在"插件式、跨编译单元、数量多"场景，sysession 不符合。**纯一致性问题，方向是"保持现状 + 记录决策"，零代码。**

---

## 4. Test strategy

### 4.1 现有 pin 测试（必须保持绿，作为回归护栏）
- `internal/cron/clock_inject_test.go`、`finishrun_persist_before_emit_test.go`、`run_inflight_finalize_test.go`、`scheduler_telemetry_testutil_test.go`：cron 侧 `runtelemetry.Broadcaster` 接入的现有契约测试——本 RFC 不动 cron，应零变化通过，证明 seam 本身稳定。
- `internal/runtelemetry/wire_stability_test.go`、`enum_complete_test.go`、`imports_test.go`：wire 形状 + enum 完整性，NG1 要求它们逐字节通过。
- sysession 三个 OnHardFail pin 测试（§3.2 列出）：Phase 1 *不* 触碰 hard-fail，这三个必须原样通过。

### 4.2 Phase 1（#1723 收敛）要新增/改写的测试
- **新增 `internal/sysession/manager_telemetry_test.go`**（替代隐含的 SetCallbacks 测试）：
  - `TestManager_SetTelemetry_EmitsSubsystemSysession`：注入一个 recording `runtelemetry.Broadcaster`，驱动一个 fake daemon 的一次 Tick，断言收到 1 个 `RunStartedEvent` + 1 个 `RunEndedEvent`，且 `Subsystem==SubsystemSysession`、`OwnerID==daemon.Name()`、`RunID` 两事件配对一致。
  - `TestManager_SetTelemetry_NilBroadcaster_NoPanic`：nil broadcaster 下 Tick 不 panic（对齐 cron `emitRun*` 的 nil 静默丢弃语义）。
  - `TestManager_SetTelemetry_ErrorClassMapping`：制造 upstream/timeout/validation/canceled/panic 五类 Tick 结果，断言映射后的 `runtelemetry.ErrorClass` 与迁移前 `mapSysessionErrorClass` 输出一致（特别 pin `DaemonErrorClassTimeout "timeout" → ErrClassDeadlineExceeded "deadline_exceeded"` 的归一化，见 `run.go:46-49`）。
  - `TestManager_SetTelemetry_RaceWithTick`（`-race`）：一个 goroutine 反复 `SetTelemetry(nil)/SetTelemetry(b)`，另一个反复驱动 Tick，验证 `atomic.Pointer` 换型后无 data race（复刻 cron `scheduler.go:507-511` 论证的 SetTelemetry-vs-dispatch race-free）。
- **server 包**：删除 `routes.go` 的内联翻译闭包后，新增/改写 `internal/server/*_test.go` 中的一个 wiring 测试，断言 sysession daemon Tick → `Hub.BroadcastDaemonRun*` 的端到端 wire payload 与重构前逐字段相同（NG1 回归护栏；可用现有 hub broadcast 断言夹具）。

### 4.3 防回归手段
- 全量 `go test -race ./internal/sysession/... ./internal/server/... ./internal/cron/... ./internal/runtelemetry/...`。
- `go vet ./...` + gofmt（hook 自动跑）。
- 删除 `DaemonRunStartedEvent`/`DaemonRunEndedEvent`/`onRun*Holder`/`SetCallbacks`/`OnRun*` 后，编译器即是最强回归护栏——任何遗漏引用都会编译失败。

---

## 5. Risk & rollback

### 5.1 load-bearing 不变量（点名）
- **并发**：`Manager.onRunStarted/onRunEnded atomic.Pointer[holder]`（`manager.go:252-253`）允许 `SetCallbacks` 在 `Start` 之后被 server 接线（`SetCallbacks` godoc `:561`）。换成 `telemetry atomic.Pointer[runtelemetry.Broadcaster]` 必须保持同样的"Start 后可安全 Set、与 Tick goroutine 并发读"语义——cron `scheduler.go:507-511` 已证明 `atomic.Pointer[*Broadcaster]` 的 store/load 在这个并发模式下 race-free，直接照搬其 `loadTelemetry` 的 nil-deref 处理（`scheduler_callbacks.go:83-89`）。
- **emit 调用点的锁姿态**：`manager.go:88-91` 注释承诺 emit "outside any Manager-internal lock"。`runOnce`（`:694`）的 OnRunStarted 调用在 CAS gate 之后、无锁；`recordRun`（`:809`）的 OnRunEnded 调用同样无锁。重构后 `emitRunStarted/emitRunEnded` 必须在**完全相同的调用点 + 相同锁姿态**插入，否则违反 broadcaster.go:6 的"producer MUST NOT call back into producer / no internal lock held"契约。
- **on-disk**：无。sysession run history 是内存 ring（NG3），本重构不碰任何磁盘格式。
- **wire**：`hub_broadcaster.go` 的 `BroadcastDaemonRun*` 不变（NG1），WS payload 逐字节稳定。

### 5.2 出错会 break 什么
- 若 emit 调用点错位（如挪进 `recordRun` 持锁段）：可能死锁或违反 callback 契约 → 由 `TestManager_SetTelemetry_RaceWithTick` + 现有 `m.mu` 相关测试捕获。
- 若 `ErrorClass` 映射搬迁出错：dashboard 显示错误 error class → 由 `TestManager_SetTelemetry_ErrorClassMapping` 捕获（特别是 timeout 归一化）。
- 若误删了 `OnHardFail` 相关代码：会破坏 #1169 的三个 pin 测试 → 编译/测试失败。**Phase 1 明确不碰 `OnHardFail`/`Stop`/`os.Exit`。**

### 5.3 Rollback
单 PR、纯内部重构、flag 无关 → rollback = `git revert` 该 PR。因 wire 形状不变（NG1），revert 不影响任何持久化状态或前端，无迁移回退成本。

---

## 6. Observability

- **新增 metric（可选，建议）**：对齐 cron 的 `metrics.CronRunStartedTotal`（`scheduler_callbacks.go:100`），在 sysession 的 `emitRunStarted` 加 `SysessionRunStartedTotal`，使两个调度器的 run 计数 metric 对称、且 counter 不会随 broadcast 路径漂移（cron 注释 R230C-GO-15 的同款理由）。若本轮想保持最小 diff，可记为 Phase 1.5 跟进项而非阻塞。
- **log**：现有 `slog` 调用点（panic/breaker/canceled，`manager.go:687,724,792`）全部不动。
- **dashboard**：零变化（NG1）。
- #1169 收敛（未来）将统一 drain-超时的 log 行：cron 用 Warn + budget counter，sysession 改后亦然——届时两者 grep 模式一致，利于运维。

---

## 7. Compatibility & migration

- **向后兼容**：WS wire 不变（NG1），dashboard 无需改、无需同步发布。
- **on-disk 格式**：无变化（NG3）。
- **config flag**：无新增 flag。telemetry 收敛是内部生产者重构，对外行为等价，不需要灰度开关（NG6）。`cmd/naozhi`（`main_helpers.go:420` 附近构造 `sysession.Config`）需把曾经传入的 `OnRunStarted/OnRunEnded`（若有）改为构造后 `SetTelemetry`；当前 server 是唯一接线点（`routes.go:183`），迁移面极小。
- **API 破坏**：`sysession.Config.OnRunStarted/OnRunEnded` + `Manager.SetCallbacks` + `DaemonRunStartedEvent`/`DaemonRunEndedEvent` 是 internal 包导出符号，无外部消费者（grep 仅 server + sysession 自身）。删除是包内 breaking，编译器全覆盖，无 semver 顾虑（internal/）。
- **迁移路径（#1169，未来 PR）**：移除默认 `os.Exit(2)` 属行为变更——需在该 PR 的 changelog 显式标注关停语义从"force-exit"变为"best-effort drain + 随进程退出"，并确认 systemd unit 的 `TimeoutStopSec` 足够覆盖 daemon 的 `TickTimeout`（`manager.go:60`，默认 30s）。

---

## 8. Rollout plan

### Phase 1 — #1723 telemetry seam 收敛【可独立安全落地：是】

**判定：hasLandablePhase1 = true，medium 风险，约 110–140 行生产代码。**

理由：这是一个明确的、有 cron 已落地实现作逐行模板的"适配 + 删样板"重构；不破坏任何已 pin 的 on-disk / wire 不变量（NG1/NG3）；唯一的风险面是 `Manager` 的 `atomic.Pointer` 换型，而 cron 已经证明该并发模式 race-free，可直接照搬。明确**不含** #1169（碰 `os.Exit` pin 测试，跨包关停语义）与 #1055 代码（纯决策）。

**确切文件级改动清单：**
1. `internal/sysession/manager.go`（~ -40 / +35 行）
   - 删 `Config.OnRunStarted`/`OnRunEnded`（`:88-93`）；删 `onRunStarted/onRunEnded atomic.Pointer[holder]`（`:252-253`）；删 `SetCallbacks`/`loadOnRunStarted`/`loadOnRunEnded`（`:560-599`）。
   - 加 `telemetry atomic.Pointer[runtelemetry.Broadcaster]` + `SetTelemetry(runtelemetry.Broadcaster)` + `loadTelemetry()` + `emitRunStarted/emitRunEnded`（照搬 cron `scheduler_callbacks.go:80-134` 范式）。
   - 改 `runOnce`（`:694`）与 `recordRun`（`:809`）的两个 emit 调用点，调用新 `emitRun*`（保持相同锁姿态，§5.1）。
   - `NewManager`（`:295` 附近）的初始回调接线改走 `SetTelemetry`。
2. `internal/sysession/run.go`（~ -19 行）：删 `DaemonRunStartedEvent`（`:117-124`）、`DaemonRunEndedEvent`（`:126-135`）。`DaemonRun` ring 结构（`:94-115`）保留不动（read endpoint 仍用）。
3. `internal/sysession/hook_holders.go`（删整文件，~ -28 行）：`onRunStartedHolder`/`onRunEndedHolder` 不再需要。
4. 新增 `internal/sysession/telemetry.go`（~ +45 行）：`emitRun*` + 从 server 包搬来的 `mapSysessionTrigger`/`mapSysessionRunState`/`mapSysessionErrorClass`（含 timeout→deadline_exceeded 归一化，`run.go:46-49`）。
5. `internal/server/routes.go`（~ -28 / +1 行）：删 `:177-209` 的内联翻译闭包，改为 `s.sysessionMgr.SetTelemetry(telemetry)`。
6. `internal/server/hub_broadcaster.go`：**不动**（消费侧分支保留，SubsystemSysession 分支 + ErrorMsg drop SECURITY 注释原样保留）。
7. 测试：新增 `internal/sysession/manager_telemetry_test.go`（§4.2）；删/改 server 包旧 SetCallbacks wiring 断言。

净生产代码估算：删约 105 行样板/镜像，加约 80 行（emit + map 搬迁 + SetTelemetry），**净增长 ~ -25 行，新写代码面 ~110–140 行**（用净写入面取估，见 phase1EstLines）。

**全切换，无 flag**：因对外行为等价（NG1/NG6），Phase 1 一次性切换，不需要分阶段灰度。

### Phase 2 — #1169 hard-fail 策略统一【需先独立评审，不在本轮落地】

输出 §3.2 方案 A 为建议方向（sysession 降级到 cron 的 best-effort drain），但因触及三个 `os.Exit` pin 测试（#1287/#585/#1286）与跨包关停语义，须独立 RFC 评审 + 独立 PR。本 RFC 仅记录方向与理由。

### Phase 3 — #1055 注册风格【决策即完成，零代码】

决策：保留 sysession 的 slice 字面量风格（§3.3 方案 A），记入本 RFC 作为方向裁定，关闭 issue 无需改码。
