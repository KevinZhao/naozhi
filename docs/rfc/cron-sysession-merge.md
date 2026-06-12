# ARCH-CRON-SYSESSION-MERGE — Cron + sysession 调度层合并

| 字段 | 值 |
| :--- | :--- |
| 状态 | Implemented v5（全部 7 phase 落地；残留项经 PR #2002/#2004/#2017 清偿，见 v5 修订历史） |
| 作者 | naozhi team |
| 创建日期 | 2026-05-26 |
| 修订日期 | 2026-06-10（v5：残留 phase 全部落地，状态升 Implemented；v4：按 master 实测订正实施状态；v3：按 H1-H17 修订；v2：按 F1-F13 + G1-G10 修订） |
| 关联代码 | `internal/cron/scheduler.go`<br/>`internal/cron/scheduler_run.go`<br/>`internal/cron/scheduler_callbacks.go`<br/>`internal/cron/scheduler_finish.go`<br/>`internal/sysession/manager.go`<br/>`internal/sysession/run.go`<br/>`internal/server/wshub_broadcast.go`<br/>`internal/server/dashboard.go:405-435`<br/>`internal/dispatch/commands.go:13`<br/>`internal/session/router_lifecycle.go:265`<br/>`internal/session/key.go` |
| 关联 RFC | `docs/rfc/consumer-interfaces.md`（同样的 IoC 思路）<br/>`docs/rfc/cron-run-history.md`（cron 终态与 RunState 来源）<br/>`docs/rfc/system-session.md`（sysession 设计） |
| 关联 issue | #1166 (runtelemetry) · #1173 (cron→session) · #1164 (dispatch→cron) · #734/#945 (executeOpt) · #1036 (RegisterBroadcaster) · #746 (SchedulerDeps) · #583 (prefix DRY，撤出 RFC) |

## 0. 修订历史

### v5 (2026-06-10) — 残留 phase 全部落地，状态升 Implemented

v4 订正后识别出的三个残留项在同日全部清偿（三个 PR 文件零交集，
按序合入 master）：

- **D-main（SchedulerDeps 部分，#746）**: ✅ landed — PR #2002。
  `internal/cron/deps.go` 新建，5 个组件依赖（Router / NotifySender /
  Agents / AgentCommands / Telemetry）从 `SchedulerConfig` 移入
  `SchedulerDeps`，`NewScheduler(cfg, deps)` 双参；AST 工具机械迁移
  106 个测试文件 ~233 处调用；新增 reflect 护栏测试钉死 cfg/deps
  边界（SchedulerConfig 不许再含接口字段）。#746 关闭成立。
- **E dispatch invert（#1164）**: ✅ landed — PR #2004。dispatch 侧
  `CronJob` 投影 + `CronCommands` 接口取代带 `*cron.Job` 签名的
  `CronScheduler` 测试 seam（#1178），server 侧薄 adapter 做翻译；
  `go list -deps` 验证 dispatch 对 internal/cron 直接+传递依赖均为
  0；新增 import 契约测试防回归。#1164 关闭成立。
- **C executeOpt helper 拆分（#734/#945）**: ✅ landed — PR #2017。
  589 行 executeOpt 拆为编排器 + 7 个 exec* helper（F1 决议：plain
  methods，无 Stage 接口）。finalizer defer / spawn-ctx defer / gauge
  配对留在编排器帧（生命周期=整个 run）；#1956/#1911 的
  Reset-before-finishRun 源序、H1 双独立 ctx 预算、H6 phase 切点全
  保留（新增 inflight_phase_test.go 钉序列，3 个既有 source-anchor
  测试零改动通过）。§6 的 cron-cr issue-rate gate 因度量停摆，按
  §3.4 自带 race gate 取代：`-race -count=10` 全包 + `-race
  -count=100` 定向（watchdog/p1 套件）全绿。唯一例外
  `TestSendWithWatchdog_DeadlineFiresInterrupt` 为既有 flake（干净
  master 同样复现，#2021 跟踪）。#734/#945 关闭成立。

至此 RFC 全部 phase（A1/A2/B/D-prep/D-main/E/C）落地，状态升
Implemented v5。

### v4 (2026-06-10) — 状态订正：v3 状态表与 master 实际不符

v3 的状态表（见下）把 D-main 标为 "close #1036, #746"、E 标为
"close #1164"，但 **2026-06-10 对 master 的逐项核实表明 #746 与 #1164
的交付物不存在**。v3 引用的 commit `ff93563`/`48a7492` 在开发分支上
存在（squash 合入 PR #1264 前的中间 commit），但 PR #1264 实际合入
master 的内容（merge commit `6ad825be`）只包含 A1/A2/B + D-main 的
Broadcaster 统一部分——**不含任何 dispatch 包改动，也不含 cron/deps.go**。

逐项核实结果（均可在 master 上复跑验证）：

- **A1 runtelemetry**: ✅ 已落地 — `internal/runtelemetry/` 存在
  （broadcaster.go / event.go / state.go），cron 经
  `SchedulerConfig.Telemetry` 接入；sysession 后续经 PR #1754
  （`SetTelemetry` + `atomic.Pointer[runtelemetry.Broadcaster]`，
  legacy `SetCallbacks` 已删）收敛到同一 seam。#1166 关闭成立。
- **A2 sessionkey**: ✅ 已落地 — `internal/sessionkey/` 存在，
  depguard 钉死 leaf 属性。
- **B cron 本地化**: ✅ 已落地 — `internal/cron` 不再 import
  `internal/session`（grep 确认仅剩 `internal/sessionkey`）。#1173
  关闭成立。
- **D-prep**: ✅ — §3.5.3.1 grep 证据 + legacy `cron_result` WS
  frame 已删。
- **D-main（Broadcaster 部分）**: ✅ 已落地 — 三个 legacy
  `SetOn*` setter 已删，`runtelemetry.Broadcaster` 单点注册，
  `internal/server/hub_broadcaster.go` 存在。#1036 关闭成立。
- **D-main（SchedulerDeps 部分，#746）**: ❌ **未落地** —
  `internal/cron/deps.go` 不存在；`NewScheduler` 仍是单参
  `NewScheduler(cfg SchedulerConfig)`（`scheduler.go:375`）；
  `Router`/`NotifySender`/`Telemetry`/`Agents` 等接口与 map 仍全部
  留在 `SchedulerConfig`（`scheduler_config.go`），违反 §3.5.1 自立
  的 cfg/deps 判定规则。**#746 被 close 但交付物不存在，需重开或
  开新 issue 跟踪。**
- **E dispatch invert（#1164）**: ❌ **未落地** —
  `internal/dispatch/cron_consumer.go` 不存在；§3.6 规划的
  `CronCommands`/`CronPolicy`/`CronLimits` 接口均未创建。现状是
  另一条独立路线：`dispatch.go` 内的 `CronScheduler` 接口
  （R250-ARCH-17 #1178，测试 seam），其签名带 `*cron.Job`，故
  dispatch.go 与 commands.go **仍 import internal/cron**——且
  `dispatch.go` 的注释明确写 "#1164 ... tracked separately and out
  of scope"。**#1164 被 close 但 import 边未切，需重开或开新 issue
  跟踪。**
- **C executeOpt 4-helper**: ⏸ deferred（同 v3）— `executeOpt`
  现 589 行（`scheduler_run.go:495`），自 v3 记录的 484 行继续增长。
  §6 的 cron-cr issue-rate gate 自 2026-05-26 起无 N_pre/N_post 度量
  记录，gate 实际处于无限期搁置状态；应主动决策推进 4-helper 或正式
  关闭 #734/#945。

**结论**：本 RFC 真实完成度为 A1/A2/B/D-prep + D-main 半项。两条
"close" 记录（#746/#1164）与代码事实不符，是后续排期决策的失真源，
据此把状态从 "Implemented v3" 降为 "Partially Implemented v4"。

### v3 (2026-05-26) — ~~implemented except Phase C~~（状态记录有误，见 v4）

Implementation status as of 2026-05-26 evening:

- **A1 runtelemetry**: ✅ landed (commit `f4f3066`) — close #1166
- **A2 sessionkey**: ✅ landed (commit `7e87699`)
- **B cron 本地化**: ✅ landed (commit `85cf01f`) — close #1173
- **D-prep**: ✅ evidence in §3.5.3.1 of this RFC
- **D-main**: ~~✅ landed (commit `ff93563`) — close #1036, #746~~
  （v4 订正：仅 Broadcaster/#1036 部分落地；SchedulerDeps/#746 未落地）
- **E dispatch invert**: ~~✅ landed (commit `48a7492`) — close #1164~~
  （v4 订正：未落地，cron_consumer.go 不存在，dispatch 仍 import cron）
- **C executeOpt 4-helper**: ⏸ deferred — RFC §3.4 + §6 race × 100
  gate stand; recommend a separate focused PR after Phase A+B
  cron-cr issue-rate gate (§6) clears

All landed phases passed `go test -race ./internal/cron/... ./internal/server/... ./internal/dispatch/... ./internal/session/...` on first run.

### v3 (2026-05-26)

按第三轮 review H1-H17 修订，主要变化：

- **修正 §3.4 入口伪代码 ctx 语义**（H1）— v2 让 execSpawn 返回 ctx 给 execSend
  会破坏现有 spawn/send 双独立 ctx 预算契约（R230B-GO-1）；v3 改为各自 helper
  内 defer 关闭自己的 ctx
- **修正 §4 依赖图**（H2）— Phase B 不依赖 A1（runtelemetry），只依赖 A2
  （sessionkey）；A1 仅前置 D-main
- **解决 cfg/deps 规则自相矛盾**（H3）— `context.Context` 作为生命周期标量
  显式豁免，留在 cfg
- **新增 §3.3.5 session 包内部迁移**（H4）— `auto_chain_router.go` 等内部
  调用一并迁移，alias 仅给外部 caller；Phase B 工作量 +1 day
- **§3.4 加 setPhase 切点保留契约**（H6）+ **execReleaseSlot 顺序约束**（H5）
- **§3.1 OwnerID 字符域契约**（H10）— 防 broadcaster 漏 sanitization 路径
- **§3.6 补 DispatcherConfig 改造说明**（H12）
- **§6 验证 gate 度量精确化**（H8）— 给出可执行的 gh issue list 命令
- **§9 加 metrics 兼容条款**（H13）
- **§11 删除 "System Session 重写 cron" 不实参照**（H7）
- **§4.3 D-main 估时上调到 2.5 day**（H11）— 总工作量 13-14 → 15-16 day
- **§3.3.3 adapter init() panic 文案带实际值**（H17）

### v2 (2026-05-26)

按 review F1-F13 + 二审 G1-G10 修订，主要变化：

- **删除原 Phase C 的 Stage/Pipeline 接口设计**，改成 4-helper 抽取（F1）—
  stage 接口对线性控制流是反向工程，helper 抽取等价 close #734/#945 但风险
  从中高 → 低
- **`internal/sessionkey` 从脚注升为正式 Phase A2**（F2）— 第三方包是打破
  cron/session 循环 import 的唯一干净方案；零依赖 + depguard lint
- **`cron.Session` 接口收紧到只剩 2 方法**（F3）— Send 删掉永远 nil 的
  attachments / hooks 参数
- **`OnExecute` 旧 hook 在 D 阶段彻底删除**（F4）— 不留 legacy 字段在
  SchedulerDeps 拖债务，拆 D-prep + D-main 两个 PR 推进
- **`SchedulerConfig` vs `SchedulerDeps` 立判定规则**（F5）— interface/
  func/外部组件归 deps，值类型归 cfg
- **撤回 ErrorClass 命名空间方案**（G3）— runtelemetry 不做前缀剥离，
  每个常量值就是 wire string；靠 `wire_stability_test.go` freeze
- **`InterruptOutcome` 编译期 assert 改成 init() panic**（G2）— 字面量
  数组 assert 是死代码，init panic 才能在 boot 时崩
- **D-prep 升级为附 grep 证据清单的实工作**（G1）— 不是空 PR
- **加 Phase C race-detector × 100 测试 gate**（G4）
- **新增 §10 Decision Log + §11 何时不该做**（G9/G10）

### v1 (2026-05-26)

初稿。提议 ExecPipeline + Stage 接口（12 stage + StageContext +
StageResult），意图把 484 行 executeOpt 拆成 stage 链。Review 评审拒：

- stage 接口对线性控制流是反向工程
- 闭包 / defer / ctx 难线性化（abortCh 通过局部变量串接、stubRefresh
  闭包捕获 snap、`defer cancel()` 配对）
- StageContext 退化为可变结构体而非 immutable pipeline 数据流
- 净增 ~600 LOC + 测试，风险中高

改成 v2 的 4-helper 方案，等价 close #734/#945，工作量 6 day → 2 day。

---

## 1. 动机

### 1.1 cron 是 issue 负载中心

截至 2026-05-26 open issue 519 个，按 area 分布：

| 区域 | 数量 | 占比 |
| :--- | :--- | :--- |
| **cron** | **205** | **39.5%** |
| server | 102 | 19.7% |
| cli | 53 | 10.2% |
| session | 45 | 8.7% |

`cron-cr-*` 自动 review 每天还在产新 issue。不先把 cron 包结构债清掉，
量只会涨不会降。

### 1.2 cron 与 sysession 是同形重复

两套并行的 scheduler，共享语义但实现各异：

| 维度 | `internal/cron.Scheduler` | `internal/sysession.Manager` |
| :--- | :--- | :--- |
| 终态状态机 | `RunState` (succeeded/failed/skipped/timed_out/canceled) | `DaemonRunState` 同名常量字符串 |
| 错误分类 | `ErrorClass`：session_error / send_error / canceled / overlap_skipped … | `DaemonErrorClass`：upstream / validation / timeout / canceled / panic |
| 触发来源 | `TriggerKind` (scheduled / manual / catchup) | `DaemonTriggerKind` (scheduled / manual) |
| started/ended event | `cron.RunStartedEvent` / `RunEndedEvent` | `sysession.DaemonRunStartedEvent` / `DaemonRunEndedEvent` |
| 回调注入 | `SetOnExecute / SetOnRunStarted / SetOnRunEnded` 三个 setter | `SetCallbacks(start, end)` 一次性 + atomic.Pointer holder |

`internal/server/dashboard.go:405-435` 各做一次 wiring；`wshub_broadcast.go`
各暴露一对 `BroadcastCronRun*` / `BroadcastDaemonRun*` 共 4 个方法，wire
shape 95% 一致（区别仅 `error_msg` 出与不出，daemon_run 故意不带）。

### 1.3 cron → session 的反向 import

```
internal/cron → internal/session
  - session.AgentOpts (传给 Router.GetOrCreate)
  - session.SessionStatus (返回值)
  - session.CronKey() / IsCronKey() / CronKeyPrefix
```

`SessionRouter` 接口在 cron 侧已声明（IoC 形式），但接口签名内部还是
`session.AgentOpts`/`session.ManagedSession`/`session.SessionStatus` ——
编译期依赖边没有真正切断。

### 1.4 dispatch → cron 的硬依赖

`internal/dispatch/commands.go:13` 直接 `import .../cron`，使用
`cron.NewJob`、`cron.MaxPromptBytes`、`cron.MaxIDLen`、`cron.MaxScheduleBytes`，
并三处复读 prompt/schedule 校验逻辑。

### 1.5 `executeOpt` 单函数体量

`scheduler_run.go:540-1025`，484 行体量（godoc 自陈 327→391→484，
长期增长趋势没有压制力）。

---

## 2. 设计目标

按 ROI 排序：

1. 斩断 `cron → session` 的 hard import 边
2. 斩断 `dispatch → cron` 的 hard import 边
3. 抽 `internal/runtelemetry` 公共事件层
4. `executeOpt` 抽 4 个 helper（可读性重构，**非**接口化）
5. 统一 broadcaster wiring（含彻底删 `OnExecute` legacy）
6. 抽 `SchedulerDeps` 并立 cfg/deps 边界规则

### 非目标

- 不改 Scheduler 对外语义（terminal state / metrics / cron_jobs.json wire 都不变）
- **不合并 `cron.Scheduler` 与 `sysession.Manager` 的 lifecycle**。两者 Stop
  策略（cron leak vs sysession osExit）的不一致是 Sec-LOW-2 故意设计 —
  见 `internal/sysession/manager.go:328` 的 "do not harmonise" 注释
- 不发明新的 Stage 抽象（v1 已尝试，被 review 拒）

---

## 3. 详细设计

### 3.1 新包 `internal/runtelemetry`

```go
// internal/runtelemetry/state.go
package runtelemetry

import "time"

type Subsystem string

const (
	SubsystemCron      Subsystem = "cron"
	SubsystemSysession Subsystem = "sysession"
	// Reserved (not yet emitted): SubsystemPlanner, SubsystemSystem.
)

type RunState string

const (
	RunStateSucceeded RunState = "succeeded"
	RunStateFailed    RunState = "failed"
	RunStateSkipped   RunState = "skipped"
	RunStateTimedOut  RunState = "timed_out"
	RunStateCanceled  RunState = "canceled"
)

// ErrorClass values are wire-stable. Each constant's string value IS
// the WS payload `error_class` field — no encoding/decoding step.
//
// Naming convention:
//   - Cross-subsystem (shared semantics): no prefix.
//     ("canceled", "deadline_exceeded", "panic", "")
//   - Subsystem-specific: value mirrors the existing pre-merge wire
//     string verbatim. "session_error" stays "session_error", NOT
//     "cron.session_error" — the dashboard JS already keys off these
//     literals and changing the wire is out-of-scope.
//
// Adding a new ErrorClass MUST update wire_stability_test.go to
// re-pin the freeze. Two constants with the same wire string is a
// compile-time error (enforced by the test).
type ErrorClass string

const (
	ErrClassNone             ErrorClass = ""
	ErrClassDeadlineExceeded ErrorClass = "deadline_exceeded"
	ErrClassCanceled         ErrorClass = "canceled"
	ErrClassPanic            ErrorClass = "panic"

	// cron-specific (wire values match current cron package)
	ErrClassCronSessionError       ErrorClass = "session_error"
	ErrClassCronSendError          ErrorClass = "send_error"
	ErrClassCronWorkDirUnreachable ErrorClass = "workdir_unreachable"
	ErrClassCronWorkDirOutsideRoot ErrorClass = "workdir_outside_root"
	ErrClassCronOverlapSkipped     ErrorClass = "overlap_skipped"

	// sysession-specific (wire values match current sysession package)
	ErrClassSysessionUpstream   ErrorClass = "upstream"
	ErrClassSysessionValidation ErrorClass = "validation"
)

type TriggerKind string

const (
	TriggerScheduled TriggerKind = "scheduled"
	TriggerManual    TriggerKind = "manual"
	TriggerCatchup   TriggerKind = "catchup" // reserved
)
```

```go
// internal/runtelemetry/event.go
package runtelemetry

type RunStartedEvent struct {
	Subsystem Subsystem
	// OwnerID is the producer-side identity of the run target. Its
	// character domain depends on Subsystem:
	//   - SubsystemCron       => 16-char lowercase hex (cron.generateHexID)
	//   - SubsystemSysession  => builtinDaemons registered name (compile-in)
	// Both domains are trusted (no end-user input flows here), but
	// Broadcaster implementations MUST select the right sanitiser
	// per Subsystem at the wire boundary — sanitizeHexIDForBroadcast
	// for cron, osutil.SanitizeForLog for sysession. A future
	// Subsystem (planner / system) MUST extend this contract before
	// the broadcaster gains a new branch.
	OwnerID   string
	RunID     string
	Trigger   TriggerKind
	StartedAt time.Time

	// Optional / subsystem-discretionary.
	SessionID string
	Fresh     bool
}

type RunEndedEvent struct {
	Subsystem  Subsystem
	OwnerID    string
	RunID      string
	State      RunState
	StartedAt  time.Time
	EndedAt    time.Time
	DurationMS int64
	Trigger    TriggerKind

	SessionID  string
	ErrorClass ErrorClass
	ErrorMsg   string // server-side use only — see SECURITY note in §3.5
}
```

```go
// internal/runtelemetry/broadcaster.go
package runtelemetry

// Broadcaster is the consumer-side interface that each scheduler
// (cron / sysession / future planner) registers exactly once. The
// scheduler invokes Broadcast{Started,Ended} from outside any internal
// lock; the broadcaster MUST NOT call back into the scheduler.
type Broadcaster interface {
	BroadcastRunStarted(ev RunStartedEvent)
	BroadcastRunEnded(ev RunEndedEvent)
}
```

#### 测试

- `wire_stability_test.go`：枚举字符串黄金值表，新增重复 wire 即编译失败
- `enum_complete_test.go`：reflect 列举常量数量，新增常量必须改测试

### 3.2 新包 `internal/sessionkey`

打破 cron / session 的 prefix 共享循环 import。

```go
// internal/sessionkey/key.go
//
// Package sessionkey owns the canonical key prefixes used to namespace
// router sessions across subsystems. Lives in its own package so cron /
// session / sysession / scratch can all reference the same constants
// without forming an import cycle.
//
// MUST NOT import any other internal/* package. Enforced by depguard
// (.golangci.yml).
package sessionkey

import "strings"

const (
	CronKeyPrefix    = "cron:"
	SysKeyPrefix     = "sys:"
	ScratchKeyPrefix = "scratch:"
)

func CronKey(id string) string    { return CronKeyPrefix + id }
func SysKey(id string) string     { return SysKeyPrefix + id }
func ScratchKey(id string) string { return ScratchKeyPrefix + id }

func IsCronKey(s string) bool    { return strings.HasPrefix(s, CronKeyPrefix) }
func IsSysKey(s string) bool     { return strings.HasPrefix(s, SysKeyPrefix) }
func IsScratchKey(s string) bool { return strings.HasPrefix(s, ScratchKeyPrefix) }

func CronJobIDFromKey(s string) string {
	if !IsCronKey(s) {
		return ""
	}
	return s[len(CronKeyPrefix):]
}
```

```yaml
# .golangci.yml additions
linters-settings:
  depguard:
    rules:
      sessionkey-isolated:
        files: ["internal/sessionkey/**"]
        deny:
          - pkg: github.com/naozhi/naozhi/internal
            desc: "sessionkey must remain a leaf package — adding a deeper
                   internal/* import creates a cycle with cron/session"
```

`internal/session/key.go` 改成 thin alias，**留一个 git release 周期**
后删除（见 §4 alias 跟踪 issue 模板）。

### 3.3 cron 本地化（Phase B）

#### 3.3.1 cron 侧新增

```go
// internal/cron/agent_opts.go (NEW)
package cron

import "context"

// AgentOpts is the cron-local view of session-spawn parameters.
// Keeping it local eliminates the import edge cron → session; the
// production wiring in cmd/naozhi adapts session.AgentOpts → cron.AgentOpts.
//
// Field set is INTENTIONALLY a subset of session.AgentOpts: only the
// fields cron actually consumes. New session-side fields don't ripple
// here unless cron actually needs them.
type AgentOpts struct {
	Backend          string
	Model            string
	SystemPromptFile string
	Workspace        string
	ExtraArgs        []string
	Exempt           bool
}

type SessionStatus int

const (
	SessionExisting SessionStatus = iota
	SessionResumed
	SessionNew
)

// Session is the minimum surface cron needs from a live session: send a
// turn and (when deadline fires) interrupt. Cron does not use
// attachments or hooks today; if that ever changes, add fields here
// then.  The narrow contract makes the adapter trivial.
type Session interface {
	Send(ctx context.Context, text string) (SendResult, error)
	InterruptViaControl() InterruptOutcome
}

type SendResult struct {
	Text      string
	SessionID string
}

// InterruptOutcome mirrors session.InterruptOutcome value-for-value.
// adapter cast (cron.InterruptOutcome(int(s.InterruptOutcome))) relies
// on these ordinals matching exactly. The init() panic in
// cmd/naozhi/cron_router_adapter.go pins the contract — diverging
// values crash the binary at boot, so a CI green build proves the
// ordinals still match.
type InterruptOutcome int

const (
	InterruptUnknown InterruptOutcome = iota
	InterruptSent
	InterruptNoTurn
	InterruptUnsupported
)
```

#### 3.3.2 SessionRouter 接口收紧

```go
// internal/cron/scheduler.go
type SessionRouter interface {
	RegisterCronStubWithChain(key, workspace, lastPrompt string, chainIDs []string)
	Reset(key string)
	GetOrCreate(ctx context.Context, key string, opts AgentOpts) (Session, SessionStatus, error)
}
```

`scheduler_run.go` 里独立的 `deadlineInterrupter` 接口删除 — `cron.Session`
已经覆盖 `InterruptViaControl()`。

#### 3.3.3 production adapter

```go
// cmd/naozhi/cron_router_adapter.go (NEW, ~70 LOC)
package main

import (
	"context"
	"fmt"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
)

// init pins cron.InterruptOutcome ordinals against
// session.InterruptOutcome. Diverging values crash the binary at boot
// instead of silently miscasting in the adapter. Panic message includes
// the actual ordinals so on-call can diagnose without re-running.
func init() {
	if int(cron.InterruptUnknown) != int(session.InterruptUnknown) ||
		int(cron.InterruptSent) != int(session.InterruptSent) ||
		int(cron.InterruptNoTurn) != int(session.InterruptNoTurn) ||
		int(cron.InterruptUnsupported) != int(session.InterruptUnsupported) {
		panic(fmt.Sprintf(
			"cron.InterruptOutcome ordinals diverged from session.InterruptOutcome: "+
				"Unknown(c=%d s=%d) Sent(c=%d s=%d) NoTurn(c=%d s=%d) Unsupported(c=%d s=%d) — "+
				"update cron_router_adapter.go",
			cron.InterruptUnknown, session.InterruptUnknown,
			cron.InterruptSent, session.InterruptSent,
			cron.InterruptNoTurn, session.InterruptNoTurn,
			cron.InterruptUnsupported, session.InterruptUnsupported,
		))
	}
}

type cronRouterAdapter struct{ r *session.Router }

func (a cronRouterAdapter) RegisterCronStubWithChain(key, workspace, lastPrompt string, chain []string) {
	a.r.RegisterCronStubWithChain(key, workspace, lastPrompt, chain)
}

func (a cronRouterAdapter) Reset(key string) { a.r.Reset(key) }

func (a cronRouterAdapter) GetOrCreate(ctx context.Context, key string, opts cron.AgentOpts) (cron.Session, cron.SessionStatus, error) {
	sess, st, err := a.r.GetOrCreate(ctx, key, toSessionAgentOpts(opts))
	if err != nil {
		return nil, cron.SessionStatus(st), err
	}
	return cronSessionAdapter{sess}, cron.SessionStatus(st), nil
}

// toSessionAgentOpts copies cron.AgentOpts → session.AgentOpts. ExtraArgs
// is cloned (not aliased) per session/router_lifecycle.go contract that
// callers populating AgentOpts MUST own ExtraArgs exclusively.
func toSessionAgentOpts(o cron.AgentOpts) session.AgentOpts {
	out := session.AgentOpts{
		Backend:          o.Backend,
		Model:            o.Model,
		SystemPromptFile: o.SystemPromptFile,
		Workspace:        o.Workspace,
		Exempt:           o.Exempt,
	}
	if len(o.ExtraArgs) > 0 {
		out.ExtraArgs = append([]string(nil), o.ExtraArgs...)
	}
	return out
}

type cronSessionAdapter struct{ s *session.ManagedSession }

func (c cronSessionAdapter) Send(ctx context.Context, text string) (cron.SendResult, error) {
	r, err := c.s.Send(ctx, text, nil, nil)
	return cron.SendResult{Text: r.Text, SessionID: r.SessionID}, err
}

func (c cronSessionAdapter) InterruptViaControl() cron.InterruptOutcome {
	return cron.InterruptOutcome(c.s.InterruptViaControl())
}
```

#### 3.3.4 测试责任

- `cmd/naozhi/cron_router_adapter_test.go`：`toSessionAgentOpts` ExtraArgs
  aliasing 测试（修改 cron 侧 slice 不应影响 session 侧）
- 现有 `internal/cron/*_test.go` 不退化是底线，PR 不允许改 test fixture 签名

#### 3.3.5 session 包内部迁移（H4）

`internal/session` 包内部仍在调 `IsCronKey / IsSysKey / IsScratchKey`，
不能只迁移外部 caller。Phase B 必须**一并完成包内迁移**，否则要么
保留 alias 到永远（违背 §4.4 alias 一个 release 删除的承诺），要么
session 包内部仍然自指，外部却走 sessionkey —— 出现两条路径。

调用点（grep `internal/session/*.go`）：

- `auto_chain_router.go:52, 274` —  IsCronKey/IsSysKey/IsScratchKey 三 OR
- `key.go:32-150` — 自身定义的 helpers（迁完后整个文件改 thin alias）
- `router_*.go` 中其它命名空间判断点（迁移 PR 内部 grep 全量发现）

预计影响 ~15 个调用点。Phase B 工作量含此项已上调到 5.0 day（见 §4.3）。

迁完后：
- session 包**只**在 key.go 留 alias 文件（thin re-export 给外部 caller 临时用）
- session 包内部**不**应当再有 `session.CronKey` / `session.IsCronKey` 自调
- alias 跟踪 issue 的 acceptance 加一项：`grep -rn 'CronKey\|IsCronKey' internal/session/ | grep -v 'sessionkey\.'` 应当返回 0 行（除 alias 文件本身）

### 3.4 `executeOpt` 4-helper 抽取（Phase C）

把现 484 行 `executeOpt` 切成若干段，各自抽成独立 method。**不引入 Stage
接口、不引入 StageContext 结构体、不引入 dispatch 表**。

```go
func (s *Scheduler) executeOpt(j *Job, viaTriggerNow bool) {
	if s == nil || s.router == nil {
		slog.Error("cron: router is nil; skipping run", "id", jobIDOf(j))
		return
	}
	inflight, runID, startedAt, trigger, ok := s.execAcquireSlot(j, viaTriggerNow)
	if !ok {
		// CAS miss: emitOverlapSkipped already fired RunStarted+Ended pair
		// inside execAcquireSlot. Nothing else to finish here.
		return
	}
	defer s.execReleaseSlot(inflight)

	if !s.execJitterAndRecheck(j, viaTriggerNow, runID) {
		return
	}

	snap, notifyTo, lg := s.execSnapshotAndNotify(j, runID, startedAt, trigger)

	// IMPORTANT (H1 / R230B-GO-1): execSpawn and execSend each create
	// their own ctx via context.WithTimeout(s.stopCtx, jobTimeout) and
	// each defers its own cancel. The two budgets are intentionally
	// independent — a slow GetOrCreate that consumes most of jobTimeout
	// still hands a fresh jobTimeout to Send. Do NOT thread a single ctx
	// through both helpers.
	sess, fa := s.execSpawn(j, snap, notifyTo, lg, runID, startedAt, trigger)
	if fa != nil {
		s.finishRun(*fa)
		return
	}

	result, fa := s.execSend(sess, snap, notifyTo, lg, runID, startedAt, trigger)
	if fa != nil {
		s.finishRun(*fa)
		return
	}

	s.execNotifyAndFinishSuccess(j, snap, notifyTo, result, runID, startedAt, trigger)
}
```

#### Helper 体量与职责

| Helper | 体量 | 入参 | 出参 | 职责 |
| :--- | :--- | :--- | :--- | :--- |
| `execAcquireSlot` | ~50L | `j, viaTriggerNow` | `inflight, runID, startedAt, trigger, ok` | CAS gate；CAS miss 时**自调 emitOverlapSkipped（fan out RunStarted + finishRun(skipped, skipPersist=true)）然后返回 ok=false**；CAS hit 时 populate inflight + `setPhase(PhaseQueued)` + metrics.CronRunInflight.Add(+1) |
| `execReleaseSlot` | ~10L | `inflight` | — | **顺序固定**：`inflight.reset()` → `inflight.running.Store(false)` → `metrics.CronRunInflight.Add(-1)`。R238-GO-2：reset 必须先于 CAS 释放，否则 next TriggerNow populate 会被本 defer 覆盖 |
| `execJitterAndRecheck` | ~50L | `j, viaTriggerNow, runID` | `bool` | `setPhase(PhaseJittering)` + applyJitterSched + post-jitter delete/pause check |
| `execSnapshotAndNotify` | ~30L | `j, runID, startedAt, trigger` | `snap, notifyTo, lg` | snapshotJob + resolveNotifyTarget + emitRunStarted + slog.With |
| `execSpawn` | ~120L | `j, snap, notifyTo, lg, runID, startedAt, trigger` | `sess, *finishArgs` | freshContextPreflightP0 + workDir resolve + 自建 spawnCtx (defer cancel) + `setPhase(PhaseSpawning)` + GetOrCreate + 错误分支 → finishArgs |
| `execSend` | ~130L | `sess, snap, notifyTo, lg, runID, startedAt, trigger` | `result, *finishArgs` | 自建 sendCtx + defer cancel + `setPhase(PhaseSending)` + watchdog + Send + 错误分支 → finishArgs |
| `execNotifyAndFinishSuccess` | ~30L | `j, snap, notifyTo, result, runID, startedAt, trigger` | — | finishRun + sanitiseRunResult + deliverNotice |

总计 ~380L 切成 7 个 method + 1 个 ~25L 的入口。每个方法是普通 Go method，
接 ≤7 个参数、返回值有意义，与现有 `freshContextPreflightP0(preflightArgs)`
风格完全一致。

#### Phase 切点保留契约（H6）

dashboard "running 12s | spawning" 徽章读 `runInflight.Phase`。4-helper
抽取必须**保留**以下 setPhase 调用点：

| 切点 | 所在 helper | 时机 |
| :--- | :--- | :--- |
| `PhaseQueued` | `execAcquireSlot` | populate inflight 之后立即 |
| `PhaseJittering` | `execJitterAndRecheck` | 进入 jitter sleep 之前 |
| `PhaseSpawning` | `execSpawn` | preflight 之后、GetOrCreate 之前 |
| `PhaseSending` | `execSend` | sendCtx 创建之后、Send 之前 |

漏切点 → dashboard 相位徽章缺帧。Phase C PR 必须新增 `inflight_phase_test.go`
驱动一次完整 run 并断言 4 个 phase 都按顺序出现。

#### Phase C 测试 gate

cron 重构历史上引入过 race（#476/#483/#537/#1226）。helper 抽取看似无害，
实际容易改 defer 顺序、闭包捕获、ctx 传递。

PR 必须满足：

1. `go test -race ./internal/cron/... -count=10` 全绿
2. `go test -count=100 -race ./internal/cron/run_p1_cache_test.go ./internal/cron/run_p1_integration_test.go ./internal/cron/run_deadline_watchdog_test.go` 跑 100 次全绿
3. PR 描述附 race log 摘要（最少 "ran 100x clean"）

不需要新加 helper 级单测 — helper 是私有可读性切片，外部行为由现存
集成测试覆盖。

### 3.5 SchedulerDeps + Broadcaster（Phase D）

#### 3.5.1 cfg/deps 边界规则

> **规则**：
>
> - `SchedulerConfig` 装值类型 + operator-tunable 配置（数值、超时、路径、TZ、AllowedRoot、NotifyDefault 这种值数据）
> - `SchedulerDeps` 装接口、函数、外部组件（Router、Telemetry、Platforms、Agents、AgentCmds 这种依赖）
>
> 判定：字段类型是 interface / func / map of interface → deps；其它 → cfg。
>
> 灰区（NotifyDefault 是 struct 但语义是"值配置"）→ cfg。
>
> **例外（H3）**：`context.Context` 视为生命周期标量而非依赖组件，留在 cfg。
> 这与 Go 社区惯例对齐（`http.Server.BaseContext` 也是这样做的）—— ctx
> 表达"父生命周期"而非"组件接口"。混入 deps 会让"deps 都是组件"的概念
> 不再成立，反而模糊。

#### 3.5.2 字段迁移

留在 cfg：`StorePath / MaxJobs / MaxJobsPerChat / ExecTimeout / Location / NotifyDefault / ParentCtx / AllowedRoot / JitterMax / RunsKeepCount / RunsKeepWindow`

挪到 deps：`Router / Platforms / Agents / AgentCommands` + 新增 `Telemetry`

#### 3.5.3 真正废弃 OnExecute

```go
// internal/cron/deps.go (NEW)
package cron

type SchedulerDeps struct {
	Router    SessionRouter
	Telemetry runtelemetry.Broadcaster
	Platforms map[string]platform.Platform
	Agents    map[string]AgentOpts
	AgentCmds map[string]string
}

func NewScheduler(cfg SchedulerConfig, deps SchedulerDeps) *Scheduler { ... }
```

**不**保留 `OnExecute` 字段。Phase D 拆成两个 PR：

- **D-prep**：dashboard.js 客户端 cron_result 帧调研 + 迁移确认。
  RFC merge 前先提交 grep 证据：

  ```
  $ grep -n 'cron_result' internal/server/static/dashboard.js
  $ grep -n 'cron_run_started\|cron_run_ended' internal/server/static/dashboard.js
  ```

  列出每个 cron_result 订阅点目前的语义（更新哪个 widget、和
  cron_run_ended 是否互补还是互斥）。如果 cron_result 还有不可替代的语义
  （比如它带 result 文本而 cron_run_ended 不带），D-main 不能直接删，
  必须先把那部分语义并入 cron_run_ended（schema 变更）。

- **D-main**：删除 `SetOnExecute / OnExecuteFunc / emitOverlapSkipped 里的
  OnExecute fan-out / BroadcastCronResult / cronResultMsg`。同时把
  `SetOnRunStarted/Ended` 收成 `SchedulerDeps.Telemetry`，hub 实现
  `runtelemetry.Broadcaster`。

调研已知事实（提前补在 RFC，让 D-main 评审无歧义）：

- `cron.SetOnExecute` 的唯一调用点是 `internal/server/dashboard.go:406`
  （hub broadcast 用）。`internal/dispatch` 不依赖 OnExecute；cron 的 IM
  通知路径走 `deliverNotice → notifyTarget`，与 OnExecute 解耦。
- 后端 wiring `dashboard.go:409-417` 已经把 SetOnRunStarted/Ended
  hooked up 到 hub broadcast 流。

#### 3.5.3.1 D-prep 调研证据（2026-05-26 完成）

**前端订阅点（共 1 处）**：

`internal/server/static/dashboard.js:8961-8971`：

```js
case 'cron_result':
  // P0 cron-run-history: cron_run_ended now drives the same refresh;
  // keep cron_result for legacy compat (older naozhi backends still
  // send only this).
  announce('定时任务已完成');
  fetchCronJobs().then(() => renderCronPanel()).catch(() => {});
  break;
```

注释自陈 `cron_run_ended` 已覆盖同样的 refresh 语义。

**后端 emit 点（共 1 处）**：

- `internal/server/dashboard.go:406-408`：`SetOnExecute → BroadcastCronResult`，仅成功路径触发
- `internal/server/wshub_broadcast.go:204-247`：`cronResultMsg` 类型 + `BroadcastCronResult` 方法
- 测试：`internal/server/wshub_broadcast_cron_result_sanitize_test.go`

**字段消费分析**：

| cron_result 字段 | 前端实际消费 | 已被 cron_run_ended 覆盖 |
| :--- | :--- | :--- |
| `type: "cron_result"` | ✓ dispatch case | ✓ cron_run_ended dispatch case |
| `JobID` | ✗（announce 不读、fetchCronJobs 不需要） | ✓（msg.job_id 驱动 timeline refresh） |
| `Result` 文本 | ✗ 前端不读 | N/A — drawer 走 `GET /api/cron/jobs/<id>/runs/<runID>` 异步取 |
| `Error` 文本 | ✗ 前端不读 | N/A — 同上 |

**结论：`cron_result` 整帧零不可替代语义**。`cron_run_ended` dispatch 做的事
（`cronApplyRunEnded + fetchCronJobs + renderCronPanel + timeline refresh`）严格
超集 `cron_result` 的 `announce + fetchCronJobs + renderCronPanel`。

**唯一可迁移项**：`announce('定时任务已完成')` 语音播报（AT 用户可访问性）。
迁到 `cron_run_ended` dispatch 内成功状态分支即可。

**D-prep 实现成本**：~10 LOC 改 dashboard.js（在 `cron_run_ended` 成功
状态加 `announce`，删 `cron_result` case）。**dashboard.js 实迁移可推到
D-main**（删除路径 + 迁移 announce 调用一并做）；D-prep 只交证据清单。

#### 3.5.4 hubBroadcaster 实现

```go
// internal/server/hub_broadcaster.go (NEW)
package server

import "github.com/naozhi/naozhi/internal/runtelemetry"

type hubBroadcaster struct{ h *Hub }

func (b hubBroadcaster) BroadcastRunStarted(ev runtelemetry.RunStartedEvent) {
	switch ev.Subsystem {
	case runtelemetry.SubsystemCron:
		b.h.BroadcastCronRunStarted(ev.OwnerID, ev.RunID, ev.StartedAt,
			string(ev.Trigger), ev.SessionID, ev.Fresh)
	case runtelemetry.SubsystemSysession:
		b.h.BroadcastDaemonRunStarted(ev.OwnerID, ev.RunID,
			string(ev.Trigger), ev.StartedAt)
	}
}

func (b hubBroadcaster) BroadcastRunEnded(ev runtelemetry.RunEndedEvent) {
	switch ev.Subsystem {
	case runtelemetry.SubsystemCron:
		b.h.BroadcastCronRunEnded(ev.OwnerID, ev.RunID, string(ev.State),
			ev.StartedAt, ev.EndedAt, ev.DurationMS, ev.SessionID,
			string(ev.ErrorClass), ev.ErrorMsg, string(ev.Trigger))
	case runtelemetry.SubsystemSysession:
		b.h.BroadcastDaemonRunEnded(ev.OwnerID, ev.RunID, string(ev.State),
			string(ev.ErrorClass), string(ev.Trigger), ev.DurationMS)
	}
}
```

测试：`hubBroadcaster_subsystem_test.go` 测两个 Subsystem 都路由到正确的
`BroadcastCron* / BroadcastDaemon*` 函数。

### 3.6 dispatch 反转（Phase E）

```go
// internal/dispatch/cron_consumer.go (NEW)
package dispatch

type CronCommands interface {
	AddJob(j *cron.Job) error
	DeleteJobByID(id string) error
	ListJobs(plat, chatID string) []cron.Job
	PauseJobByID(id string) error
	ResumeJobByID(id string) error
}

type CronPolicy interface {
	ValidatePromptStrict(prompt string) error
	ValidateScheduleStrict(schedule string) error
}

type CronLimits struct {
	MaxPromptBytes   int
	MaxIDLen         int
	MaxScheduleBytes int
}
```

`commands.go` 三处校验合并到 `cronPolicy.ValidatePromptStrict + ValidateScheduleStrict`。

仍然保留 `cron.Job` struct 类型在 dispatch 接口里 — 它是值类型 wire shape，
去结构化只为切 import 没必要；只切 `*cron.Scheduler` 这种长生命周期对象的
依赖。

#### 3.6.1 DispatcherConfig 改造（H12）

`internal/dispatch/dispatch.go:206` 的 `DispatcherConfig` 当前持
`Scheduler *cron.Scheduler`。Phase E 改成：

```go
type DispatcherConfig struct {
    // ... 其他字段不变 ...

    // Cron is the consumer-side interface; cmd/naozhi passes
    // *cron.Scheduler which satisfies it. Replaces Scheduler field.
    Cron        CronCommands
    CronPolicy  CronPolicy
    CronLimits  CronLimits
}
```

构造站点 `cmd/naozhi/main.go` 不需要写 adapter — `*cron.Scheduler` 直接
赋给 `CronCommands` 字段即满足接口（accept interfaces, return concrete）。
`commands.go` 内 `dispatcher.scheduler.AddJob` 改为 `dispatcher.cron.AddJob`。

注意 typed-nil 陷阱（参见 `docs/rfc/consumer-interfaces.md` v3 经验）：
```go
var sched *cron.Scheduler // nil
cfg.Cron = sched           // typed-nil interface!
if cfg.Cron != nil { ... } // 恒真，bug
```
Phase E 的 `NewDispatcher` 必须 nil-guard `cfg.Cron`，按 consumer-interfaces
RFC 的同名修复手法。

### 3.7 #583 prefix DRY — 撤出 RFC

`scheduler_jobs.go` 的 `withJobByPrefix` 已经存在并 DRY 了 ~60% 重复。
剩下三个 prefix-variant 调用的 `op` / `postCleanup` 闭包形态各不相同，
进一步 DRY 收益边际。**作为独立小 PR 单独评估，不属于本 RFC 范围**。

---

## 4. 迁移分阶段

### 4.1 Phase 列表与依赖（H2 修订）

| Phase | 改动 | LOC | 风险 | 依赖 |
| :--- | :--- | :--- | :--- | :--- |
| A1 runtelemetry | 新增包 + wire-shape 测试 | ~250 | 低 | — |
| A2 sessionkey | 新增包 + depguard | ~80 | 低 | — |
| B cron 本地化 | AgentOpts/Session 接口 + adapter + ExtraArgs clone + InterruptOutcome init() panic + **session 包内部迁移**（H4） | ~350 | 中 | **A2** |
| D-prep | dashboard.js cron_result 调研 + 可能的迁移 | ~10-100 | 低 | — |
| D-main | SchedulerDeps + 删 OnExecute | ~150 | 低 | **A1** + D-prep |
| E dispatch 反转 | CronCommands + CronPolicy + Limits + DispatcherConfig 改造 | ~250 | 低中 | B |
| C executeOpt 4-helper | 抽 7 个 method + Phase 切点测试 | ~80 净增（重构） | 低（race × 100 gate） | B |

注意：Phase B **只依赖 A2**（sessionkey），**不**依赖 A1（runtelemetry）—
B 的范围是 cron→session 反向 import，不引入事件层。A1 仅 D-main 用到。

### 4.2 依赖图

```
A2 ──→ B ──┬──→ E
           └──→ C
A1 ──┐
     ├──→ D-main
D-prep ─┘
```

**无依赖前置任务**：A1 / A2 / D-prep 三个 — 多人并行最早可同时启动。
**B 之后并行**：E / C 两个；D-main 在 A1 + D-prep 都好之后跑。
单人最短 ~15 day；多人最短 ~6 calendar day。

### 4.3 工作量（H4 + H11 修订）

| Phase | 实施 | 测试 | review | 总 |
| :--- | :--- | :--- | :--- | :--- |
| A1 | 0.5 | 0.5 | 0.5 | 1.5 |
| A2 | 0.3 | 0.3 | 0.3 | 0.9 |
| B | 2.0 | 2.0 | 1.0 | 5.0 |
| D-prep | **完成** | — | — | **已交付** (调研 + RFC §3.5.3.1) |
| D-main | 1.0 | 1.0 | 0.5 | 2.5 |
| E | 1.0 | 1.0 | 0.5 | 2.5 |
| C | 1.0 | 0.5 | 0.5 | 2.0 |
| **总** | **5.8-6.6** | **5.3-5.6** | **3.3** | **14.4-15.5** person-day |

变化原因：
- B +1 day（H4：session 包内部 ~15 处调用迁移 + 新增包内 grep CI 守门）
- D-main +1 day（H11：删 OnExecute / BroadcastCronResult / cronResultMsg
  + 改 NewScheduler 签名 + hub_broadcaster.go + 测试 = 改 4 个文件）
- D-prep 已完成（2026-05-26）— RFC §3.5.3.1 含证据；dashboard.js 仅
  ~10 LOC 改动推到 D-main 一并做（删 cron_result case + 在 cron_run_ended
  成功分支调 announce）

### 4.4 alias 跟踪

Phase B 的 `internal/session/key.go` 留 thin alias 一个 release 周期。
B PR merge 时同时开 issue：

```
title: Remove session.{CronKey, IsCronKey, ...} alias post sessionkey migration
body:
  - Migration RFC: docs/rfc/cron-sysession-merge.md Phase B
  - Migration PR: #<B PR number>
  - Target removal: <next git tag + 1 release after that tag>
  - Owner: <assignee>
  - Acceptance: grep `session.CronKey\|session.IsCronKey` returns 0
                in internal/ and cmd/, then delete alias file
```

---

## 5. 风险 & 缓解

### R1：`session.AgentOpts` 与 `cron.AgentOpts` 字段漂移

未来 session 加新字段（比如 hooks），cron adapter 漏带。

**缓解：** `cmd/naozhi/cron_router_adapter_test.go` round-trip 测试 +
`init() panic` 的 InterruptOutcome ordinal pin。

### R2：ErrorClass wire shape 不兼容

旧 dashboard 客户端 hard-code `error_class` 字符串，常量重命名后挂。

**缓解：** runtelemetry 常量值就是 wire string，cron / sysession 旧 wire
逐字保留（"session_error" / "upstream" 不变）。`wire_stability_test.go`
freeze 全集，新增重复 wire 即测试失败。

### R3：4-helper 抽取引入 race

helper 重构改 defer 顺序、闭包捕获、ctx 传递的概率。

**缓解：** Phase C 测试 gate（§3.4）—  `-race -count=100` 三个最 race-prone
测试。

### R4：sessionkey 循环 import

未来有人加 `internal/sessionkey → internal/session` import，破坏中立性。

**缓解：** depguard 从 day 0 上 lint，PR CI 失败硬挡。

### R5：sysession osExit 不能合并到 cron

合并 lifecycle 会破 Sec-LOW-2（`internal/sysession/manager.go:328`
"do not harmonise" 注释）。

**缓解：** 本 RFC 明确不动 lifecycle（§2 非目标）。

### R6：Rollback path

每个 Phase 都设计成**独立 revert**：

- **A1/A2**：纯加法，revert = 删包 + 删 import；revert 后只剩旧代码继续工作
- **B**：adapter 是 cmd/naozhi 内部组件，revert = 把 `cron.AgentOpts/Session`
  接口和 adapter 一起删，恢复 `session.AgentOpts` 直接传入
- **D-main**：revert 风险最高（删了 OnExecute），D-prep 必须先于 D-main
  一周以上发布；revert = 恢复 SetOn{Execute,Started,Ended} + OnExecute hook
- **E**：revert = 删 CronCommands/CronPolicy interface，dispatch 重新
  import cron
- **C**：纯重构，revert = git revert

PR 描述模板里写明 "Independent revert: yes / no, blockers: …"。

---

## 6. 验证 Gate

Phase A+B 上线后**两周**对比 cron-cr 自动 review 在 cron 包发现的新
issue 计数。**精确度量**（H8）：

```bash
# A+B merge SHA 记录在跟踪 issue（见 §4.4 模板）；以下 <DATE> 形如 2026-04-12
# N_pre：A+B merge 之前 14 天窗口
gh issue list --state all --limit 1000 \
  --label area:cron \
  --search 'created:>=<MERGE_DATE-14d> created:<<MERGE_DATE> in:label source:cron-cr' \
  --json number | jq 'length'

# N_post：A+B merge 之后 14 天窗口
gh issue list --state all --limit 1000 \
  --label area:cron \
  --search 'created:>=<MERGE_DATE> created:<<MERGE_DATE+14d> in:label source:cron-cr' \
  --json number | jq 'length'
```

- gate：`N_post < 0.7 × N_pre` 才推进 Phase C
- gate 度量结果在跟踪 issue 留痕，便于回查
- 如果不达到，先 root cause（cron 包结构是否真改善了？还是 cron-cr 在抓
  别的层面问题？）再决定继续

**Gate 范围：** 仅 apply 到 Phase C（executeOpt 重构）和将来"cron scheduler
真合并 sysession"的延伸提案。A/B/D/E 都是消除已确认的代码债务，不需要等
数据。

---

## 7. 直接关闭的 issue

| Issue | 关闭于 |
| :--- | :--- |
| #1166 runtelemetry 抽出 | A1 |
| #1173 cron→session 反向 | B |
| #1164 dispatch→cron 反向 | E |
| #734 executeOpt 拆 stage | C（4-helper 重构等价 close） |
| #945 executeOpt 391 行 | C |
| #1036 RegisterBroadcaster 一站 | D-main |
| #746 SchedulerDeps abstraction | D-main |
| #583 prefix DRY | 移出 RFC，独立小 PR |

---

## 8. 安全考虑

### 8.1 ErrorMsg 在 wire 中的策略

- cron 当前 broadcast 带 `error_msg`，已经走 `redactPathsInCronError +
  SanitizeForLog`
- sysession 故意不在 wire 里带 `error_msg`（`docs/rfc/system-session.md` §9.4）
- broadcaster 接口 `RunEndedEvent.ErrorMsg` 字段保留 — 由 producer 决定
  是否在 WS payload 暴露。`hubBroadcaster` 切到 cron 分支带 ErrorMsg、切到
  sysession 分支不带

### 8.2 sessionkey 包的攻击面

`sessionkey` 是 leaf 包，零外部依赖，prefix 是常量字符串 — 没有可信
输入路径，攻击面为零。

### 8.3 adapter 的输入校验

`cronRouterAdapter.GetOrCreate` 的 opts 仍要经 session 侧 `validateModel /
validateBackend` 等校验；adapter 不剥离任何 boundary check，只做形状转换。

---

## 9. 兼容性

- **wire 兼容**：cron_run_started / cron_run_ended / daemon_run_started /
  daemon_run_ended WS payload 字段全部保留，常量字符串值保留
- **API 兼容**：dashboard HTTP API（/api/cron/*、/api/system/daemons）不变
- **存储兼容**：cron_jobs.json schema 不变；runs/<jobID>/<runID>.json schema 不变
- **客户端兼容**：dashboard.js 不需要改（除非 D-prep 调研发现 cron_result
  需迁移）
- **metrics 兼容**（H13）：cron / sysession 现有 prometheus 计数器名不变。
  Phase D 的 hubBroadcaster 改造**不**触碰 `metrics.CronRunStartedTotal /
  CronRunSucceededTotal / CronRunFailedTotal / CronRunSkippedTotal /
  CronRunTimedOutTotal / CronRunCanceledTotal / CronRunEndedTotal /
  CronRunInflight` —— 这些仍由 cron 包内部 `bumpRunStateMetrics` /
  `Add(±1)` 维护。runtelemetry 不引入新 prometheus 度量，纯事件层。

---

## 10. Decision Log

| 日期 | 决定 | 理由 |
| :--- | :--- | :--- |
| 2026-05-26 | Phase C 改 4-helper（拒 stage 链） | stage 接口对线性控制流是反向工程；闭包/defer/ctx 难线性化；StageContext 退化为可变结构体 (F1) |
| 2026-05-26 | sessionkey 升 Phase A2 | session/cron 共用 prefix 必须靠中立包打破环 (F2) |
| 2026-05-26 | `Session` 接口收紧到 2 方法 | cron 永远传 nil attachments / nil hooks，复刻类型纯债务 (F3) |
| 2026-05-26 | OnExecute D 阶段彻底删除 | "保留 legacy 字段在 deps" 让"统一 wiring"卖点打折 (F4) |
| 2026-05-26 | 立 cfg vs deps 边界规则 | 简单切两半会出现"Router 在 deps、AllowedRoot 在 cfg、Notifier 在 deps、NotifyDefault 在 cfg" 的逻辑不一致 (F5) |
| 2026-05-26 | 撤回 ErrorClass.Wire() 命名空间方案 | 跨子系统重名不通用（canceled 是共享、upstream 是子系统私有），加前缀+剥离 = 三层间接；常量直接用 wire 值更朴素 (G3) |
| 2026-05-26 | InterruptOutcome 改 init() panic | 字面量 [4]struct{} assert 是死代码，不能跨包检测 ordinal 漂移 (G2) |
| 2026-05-26 | D-prep 升级为附 grep 调研的实工作 | 不能假设 dashboard.js 已迁完 cron_result → cron_run_ended (G1) |
| 2026-05-26 | Phase C 加 race × 100 gate | cron 历史上重构引入过 race；helper 抽取易改 defer 顺序 (G4) |
| 2026-05-26 | #583 prefix DRY 撤出 RFC | `withJobByPrefix` 已抽，剩余 op/postCleanup 闭包差异大，DRY 边际收益 |
| 2026-05-26 | execSpawn / execSend 各自 own ctx | 现有 R230B-GO-1 注释明确 spawn/send 双独立预算；v2 的 ctx-thread-through 写法会破坏在产 cron 超时契约 (H1) |
| 2026-05-26 | Phase B 不依赖 A1 | B 是 import 反转，不引入事件层；A1 仅 D-main 用到 (H2) |
| 2026-05-26 | context.Context 例外留 cfg | Go 社区惯例（http.Server.BaseContext）；ctx 是生命周期标量，混入 deps 会模糊"组件接口"概念 (H3) |
| 2026-05-26 | Phase B 包内迁移一并完成 | `auto_chain_router.go` 等 session 包内部仍调 IsCronKey；只迁外部 alias 永远活下去 (H4) |
| 2026-05-26 | execReleaseSlot 顺序固定 | reset() 必须先于 CAS 释放，否则 next TriggerNow populate 会被 defer 覆盖（R238-GO-2）(H5) |
| 2026-05-26 | setPhase 4 切点写进 helper 契约 | dashboard 相位徽章读 inflight.Phase；helper 抽取易漏 (H6) |
| 2026-05-26 | OwnerID 字符域按 Subsystem 分 | broadcaster 选 sanitiser 路径不同；planner 等未来 subsystem 必须先扩约束 (H10) |
| 2026-05-26 | DispatcherConfig.Scheduler 改 Cron CronCommands | E 阶段不切这字段 = dispatch 仍 hard import cron；带 typed-nil 防御 (H12) |

---

## 11. 何时不该做这个 RFC

- **如果同时有第三方 RFC 计划改造 cron.Scheduler 的核心结构**（H7：目前
  `docs/rfc/` 下 Status=Draft 的 cron 相关 RFC 仅 cron-skill-binding /
  cron-v2-polish 等增量提案，没有覆盖性范围 — 仅作未来防御）。审 RFC
  时应当先扫一遍 `docs/rfc/` Draft 文档，确保没有覆盖性 cron 重写
- **如果团队带宽不足**，A1/A2/D 是 ROI 最高的子集（消除依赖循环、统一
  wiring、删 legacy hook），可以单独发布；B/C/E 推迟
- **如果 cron-cr 自动 review 系统下线**，§6 的 gate 失效，Phase C 决策
  转为 wall-clock + 人工审阅 cron 文件压力的判断
- **如果 dispatch 重构（#373/#611 等其它 dispatch P1）先于本 RFC 启动**，
  Phase E 应推迟到那个重构完成（合并 dispatch 改动减少冲突）

---

## 12. 落地清单

每条 PR 描述附以下字段（CI 可校验）：

```
Phase: A1 / A2 / B / D-prep / D-main / E / C
Independent revert: yes / no
Blockers if reverted: <list>
Race gate (Phase C only): "ran Nx clean"
```

### 12.1 Phase 任务清单

```
[ ] A1 RFC approved
    [ ] internal/runtelemetry/state.go
    [ ] internal/runtelemetry/event.go
    [ ] internal/runtelemetry/broadcaster.go
    [ ] internal/runtelemetry/wire_stability_test.go
    [ ] internal/runtelemetry/enum_complete_test.go
    [ ] PR merged → close #1166
[ ] A2 RFC approved
    [ ] internal/sessionkey/key.go
    [ ] internal/sessionkey/key_test.go
    [ ] .golangci.yml depguard rule
    [ ] PR merged
[ ] B RFC approved (depends on A2)
    [ ] internal/cron/agent_opts.go
    [ ] internal/cron/scheduler.go SessionRouter narrowed
    [ ] internal/cron/scheduler_run.go deadlineInterrupter removed
    [ ] cmd/naozhi/cron_router_adapter.go (with init() panic)
    [ ] cmd/naozhi/cron_router_adapter_test.go (ExtraArgs aliasing)
    [ ] internal/session/key.go alias added
    [ ] internal/session/auto_chain_router.go uses sessionkey.Is*Key
    [ ] internal/session/router_*.go remaining uses migrated to sessionkey
    [ ] grep `internal/session/` for stale `session.CronKey` self-call → 0
    [ ] alias removal issue opened
    [ ] PR merged → close #1173
[x] D-prep (2026-05-26 完成)
    [x] grep evidence appended to RFC §3.5.3.1
    [x] classification: cron_result 全帧可删除，仅 announce 需迁
    [x] dashboard.js 实迁移推到 D-main（~10 LOC 简单改动）
[ ] D-main RFC approved (depends on A1, D-prep ✓)
    [ ] internal/cron/deps.go SchedulerDeps
    [ ] internal/cron/scheduler.go NewScheduler signature change
    [ ] internal/cron/scheduler_callbacks.go OnExecute removed
    [ ] internal/server/wshub_broadcast.go BroadcastCronResult / cronResultMsg removed
    [ ] internal/server/wshub_broadcast_cron_result_sanitize_test.go removed
    [ ] internal/server/static/dashboard.js: case 'cron_result' deleted
    [ ] internal/server/static/dashboard.js: announce('定时任务已完成') 迁到 cron_run_ended succeeded 分支
    [ ] internal/server/dashboard.go: SetOnExecute wiring 删除
    [ ] internal/server/hub_broadcaster.go (subsystem dispatch)
    [ ] internal/server/hub_broadcaster_subsystem_test.go
    [ ] metrics 计数器名未变（grep diff 应当 0 行）(H13)
    [ ] PR merged → close #1036, #746
[ ] E RFC approved (depends on B)
    [ ] internal/dispatch/cron_consumer.go (CronCommands / CronPolicy)
    [ ] internal/dispatch/dispatch.go DispatcherConfig.Cron + CronPolicy + CronLimits
    [ ] NewDispatcher typed-nil guard (per consumer-interfaces v3 pattern)
    [ ] internal/dispatch/commands.go uses CronCommands + CronPolicy
    [ ] PR merged → close #1164
[ ] Verification gate (§6) passes for Phase C
    [ ] N_pre / N_post gh issue list 命令记录在 gate issue
    [ ] N_post < 0.7 × N_pre
[ ] C RFC approved (depends on B, gate)
    [ ] internal/cron/scheduler_run.go execAcquireSlot (with emitOverlapSkipped on miss)
    [ ] internal/cron/scheduler_run.go execReleaseSlot (顺序固定 reset→Store→Add)
    [ ] internal/cron/scheduler_run.go execJitterAndRecheck (setPhase(Jittering))
    [ ] internal/cron/scheduler_run.go execSnapshotAndNotify
    [ ] internal/cron/scheduler_run.go execSpawn (own ctx + setPhase(Spawning))
    [ ] internal/cron/scheduler_run.go execSend (own ctx + setPhase(Sending))
    [ ] internal/cron/scheduler_run.go execNotifyAndFinishSuccess
    [ ] internal/cron/inflight_phase_test.go (4 phase 出现顺序断言)
    [ ] race × 100 gate passed (PR description)
    [ ] PR merged → close #734, #945
[ ] Post-migration
    [ ] alias removal issue resolved
    [ ] memory updated: project_cron_sysession_merge.md
```
