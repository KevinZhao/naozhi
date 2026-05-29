# RFC: session/ManagedSession 文件分拆(ARCH-MANAGED-SPLIT)

> **状态**: Implemented (v2.3 — 5 phase 全部落地,build/vet/test-race 全绿,func 计数稳定 76)
> **作者**: naozhi team
> **创建**: 2026-05-29
> **修订**: 2026-05-29(v2.3: 落地实施;发现并修复 §6 "测试零改动" 被 2 个 source-introspecting 测试证伪)
> **范围**: `internal/session/managed.go` → 按职责拆成 6 个文件;**零语义改动**
> **关联代码**: `internal/session/managed.go`(2262 行)、`internal/session/router_lifecycle.go`、`internal/session/router_core.go`、`internal/session/store.go`、`internal/cli/process.go`
> **先例**: `docs/rfc/process-split.md`(ARCH-PROCESS-SPLIT,同一手法,已 Implemented)、`docs/design/router-split-design.md`
> **解锁**: `ProcessSender` / `ProcessEventReader` facet 拆分的后续轮次(R242-ARCH-4)、Snapshot 热路径性能项(R215-ARCH-P2-7 / R222-PERF-7)的阅读瓶颈

---

## 0. 修订历史

### v2.3(2026-05-29)— 落地实施记录(Implemented)

5 个 phase 全部落地,`internal/session/managed.go` 2262 行 → 6 文件;每 phase `go build` + `go vet` + `go test -race` 全绿,生产 func 计数稳定 **76**(managed.go 1 / identity 23 / lifecycle 11 / send 10 / history 8 / query 23),managed.go 净 **1765 行删除 + import 块收窄**,零方法体内容改动。落地后文件行数:managed.go 497 / query 686 / history 403 / send 313 / lifecycle 204 / identity 198。

**§6 "测试零改动" 被证伪 —— 这是 RFC 在 6 轮纸面 review 中均未发现的实施级问题**。存在 2 个 *source-introspecting 测试*按文件名读取源码、断言符号定义位置,文件搬迁必然破坏:

- `reattach_contract_test.go`:`os.ReadFile("managed.go")` 断言 `ReattachProcessNoCallback` 定义于此 + 遍历目录计 call site 时 `name=="managed.go"` 跳过定义文件。Phase 2 该方法移入 `managed_lifecycle.go`,改 3 处文件名字面量 + 1 处 Fatal 文案。
- `managed_interrupt_idle_race_test.go`:`os.ReadFile(".../managed.go")` 定位 `Interrupt()` body 做源码级断言。Phase 4 移入 `managed_send.go`,改 2 处。
- `process_iface_rename_plan_test.go`:`parser.ParseFile("managed.go")` 找 `processIface` 接口 —— 该接口**留守 managed.go**,故无需改(预判正确)。

修正这两个测试**未改变其断言意图**(call-site 唯一性 / isAlive 守卫 / sendCancel 契约 / Interrupt body 不变性),仅把"定义文件名"这一**布局相关**的字面量重指向新文件。本质同 round-1 ROI 分析中给 dashboard.js 标注的 `static_ux_contract_test.go` 风险:**源码自省测试把文件布局焊死进断言**。后续重构应将此类测试改为按 package AST 扫描(不绑文件名)。

**实施教训**:① `git checkout <单文件>` 会把已 cut 的 managed.go 恢复成 HEAD 全量、与已抽出文件重复 —— botched cut 须手动重删,勿用单文件 checkout 回退;② sed 提取与删除的行范围必须严格一致(含尾随空行),否则漏搬闭合括号;③ 每次 cut 后几乎必有 unused import,`go build` 即时暴露后逐个清理。

§6 正文据此订正为"**75 `_test.go` 中 2 个 source-introspecting 测试需改文件名字面量**,其余 + testutil.go 零改动"。

### v2.2(2026-05-29)— 修正 helper 归属理由(消费端范围)

第三轮 review 换维度,不再查计数而查"归属理由是否成立":对每个被搬的包私有 helper grep 了 `internal/session/` 非 test 文件的真实引用。发现 §5.1 把 *session 包级共享基础设施*误描述成 managed 局部工具:

- **E1 修复**:`loadAtomicString`/`storeAtomicString`(被 router_cleanup/discovery/lifecycle 用)、`loadTotalCost`/`storeTotalCost`(router_core/lifecycle/store.go)、`loadProcess`/`storeProcess`(**最大消费方是 router_*.go 5 文件** + store.go + testutil.go)实际跨 router 家族大量使用。文件边界不变(同包放哪都能引用),但 §5.1 归属理由从"managed 内部共享"订正为"session 包级基础设施,位置仅就近,签名/receiver 变更波及 router_*.go" —— 这对 §10 Q1 后续 facet 接口拆分是关键约束。`sortEntriesByTimeStable`/`costUnitForBackend`/`scanLastSummaries`/`extractLastPromptFromProcess` 经 grep 确认 managed.go 独家,原归属成立。
- **E3 修复**:§6 仅论证 75 个 `_test.go`,漏了 `testutil.go`(`//go:build !release`,调 `storeProcess`)。结论不变(同包零改动),补入论证集合。
- 顺修 v2.1 遗漏:§5.2 表头"全部 13 个触点"未随表体改到 14。

教训:前两轮 review 一直查"数目对不对",但 §5.1 错在"理由对不对" —— 计数正确不等于论证正确,**归属理由须用 grep 验证消费端真实范围**。

### v2.1(2026-05-29)— 修正 v2 自身的计数错误

第二轮 review 发现 v2 在修 v1 手数错误的同时,于新写的两张计数表引入同类错误(总数对、分项错):

- **D1 修复**:§5.2 historyMu 触点表标"13 个 / history(7)·lifecycle(1)·query(5)",逐 call-site 取 enclosing func 实测为 **14 个 / history(8)·lifecycle(2)·query(4)**。三个子数全错、凑巧自洽成 13;且 **lifecycle 漏列 `adoptProcessAlreadySeeded`(901)** —— 它与 `attach` 对称,在 historyMu 下置 `persistedSeededLen`。v2.1 补该方法,§4.2.3 风险注与 §9 R2 同步。
- **D2 修复**:附录"计数核对"行 `history 9·lifecycle 14·send 11·query 18` 四项错误(正确为 `8·11·10·23`),四错相互抵消使总数仍为 76。v2.1 订正为带等式 `1+23+8+11+10+23=76` 的可核对形式,并补 historyMu 各文件 grep 自检命令。

教训(与 process-split v2 同):**手数表是 RFC 的系统性弱点,凡计数必附可机械复现的 grep/awk 命令**。

### v2(2026-05-29)— 源码级深度 review 修订

对照 `internal/session/managed.go` 实际源码逐条核验 v1 断言,修正事实错误并补强论证:

- **F1 修复**:v1 §1 称"~75 个方法",附录 A 列 75 项 —— 实测 `grep -c '^func '` 为 **76 个 func**,且附录 A 漏列 5 个非 func 符号(`InterruptOutcome` type/const、`SessionSnapshot` struct、`var _ ProcessEventReader` 编译期断言、`costUnitForBackendOnce` var)。v2 §4.2 / 附录 A 补全,并显式标注 `SessionSnapshot`(导出类型)**必须留守** `managed.go`。
- **F2 修复**:v1 §4.2.6 与附录把 `String()`(行 1214)标为 `→ query`,但它是 `func (o InterruptOutcome) String()`,是中断枚举的 Stringer。v2 改归 `managed_send.go`,与 `InterruptOutcome` type+const+`InterruptViaControl` 同文件(对齐 process-split §4.2.1 `ProcessState.String` 跟 state 枚举走的先例)。
- **F3 修复**:v1 §4.1 估留守 ~520 行口径含糊。实测**第一个方法在第 420 行**,故 1-419 行(struct+3 接口+常量+box)是硬留守;再加必须留守的 `SessionSnapshot` struct(1362)。v2 厘清留守区构成。
- **F4 修复**:v1 §6 称测试"全部 `package session`"。实测 `contract_test.go` / `planner_key_contract_test.go` 是 `package session_test`(2 个外部测试包)。结论(测试零改动)不变,但论据订正为"73 内 + 2 外;外部包仅依赖导出 API,本 RFC 零 API 改动故同样不受影响"。
- **W1/W2/W3 补强**:v1 §5.2 锁不变性表漏掉 `persistedHistorySorted` 这个**跨 `managed_query.go`↔`managed_history.go` 文件**的不变性 —— `EventEntriesSince`(query)在读路径里做 RLock→Lock 升级惰性排序,写的是 `InjectHistory`(history)也写的同一标志。v2 §5.2 补该不变性、列全 13 个 historyMu 触点,§4.3 依赖图加注"query↔history 经 `persistedHistory*` 字段共享状态(非函数调用耦合)"。
- **W4 修复**:v1 §4.3/§7.1 称 Phase "任意顺序、互不阻塞" —— 该结论仅在**函数调用依赖**层面成立,但 Phase 2/3/5 经 historyMu + `persistedHistory*` 字段共享状态。v2 §7.1 改为"Phase 2/3/5 建议同一人连续做或同轮 review",收回过度乐观的并行承诺。

v1 正文已就地修订;本节记录差异。

---

## 1. 目标

把 `internal/session/managed.go` 的 **2262 行 / 76 个 func + 5 个 type·const·var**(绝大多数方法挂在单个 `ManagedSession` struct 上)按职责拆到同 package 的 6 个文件里,使得:

1. 单文件不超过 ~550 行,`ManagedSession` struct + 构造期字段 + 最贴身的访问器留在主文件
2. 每个文件承担**单一职责轴**(identity/accessor、lifecycle/process attach、send/interrupt、event/history 查询、history 注入与链、snapshot)
3. **不改任何 export API**,外部包(`internal/server`、`internal/dispatch`、`internal/cron`、`internal/upstream`)无须改 import
4. **不改任何并发原语**(`sendMu` / `historyMu` / `keyOnce` / 13 个 `atomic.Pointer`/`atomic.Int64`/`atomic.Uint64` 字段),逐字节搬迁方法体
5. 现有 75 个 `internal/session/*_test.go`(其中约 22 个直接覆盖 `ManagedSession` 方法)**不用改一行**
6. 每个 phase 单独 PR/commit,`go test -race` 全绿

## 2. 非目标

| 非目标 | 原因 |
|---|---|
| 改 `ManagedSession` 的字段组织 / padding | 超出范围;字段已为热路径精心打包(`cliIdentityBox` 三合一),另立 ADR |
| 改 lock 契约(`sendMu` 与 `historyMu` 独立、`atomic.Pointer` 读写配对) | 任何一处错位都会让 race 潜回;保持逐字节搬 |
| `processIface` god-interface 的 facet 拆分(ProcessSender/ProcessEventReader/...) | 那是**接口**拆分(R242-ARCH-4),本 RFC 只拆**文件**,两件事 |
| 把 `Get` 前缀访问器改名(`GetSessionID`→`SessionID` 等) | 重命名 = API 改动,跨包 breaking,由 `TestProcessIfaceGetterRenamePlanned` 单独跟踪 |
| 性能优化(Snapshot 单次 atomic.Load 等) | 独立跟踪;本 RFC 专注"搬" |
| 合并成 `internal/session/managed/` 子包 | 会引爆全仓 import 路径;同 package 已足够 |
| 拆 `router_*.go` 家族 | router 拆分是另一条线(见 `docs/design/router-split-design.md`),已是可接受粒度 |
| 移动 `processIface` / `ProcessSender` / `ProcessEventReader` 接口定义 | 这些是 package 级契约文档,留主文件维持可见性(见 §5) |

## 3. 背景

### 3.1 现状

`wc -l internal/session/managed.go` → **2262 行**。`grep -c '^func '` → **76 个 func**(绝大多数挂在第 238 行起的 `ManagedSession` struct 上),外加 5 个需要归位的 type·const·var(`InterruptOutcome` type+const、`SessionSnapshot` struct、`var _ ProcessEventReader` 编译期断言、`costUnitForBackendOnce`)。职责横跨:身份/访问器、CLI identity、cost、workspace、process attach/reattach、send/interrupt/passthrough、event 查询、history 注入、prev-session 链、snapshot。按语义分组:

| 职责轴 | 方法数(约) | 估计行数 |
|---|---|---|
| Identity / 一行访问器 / atomic helper(key/sessionID/workspace/label/model/backend/cliName/cliVersion/cost) | ~30 | ~360 |
| Lifecycle:process attach / reattach / adopt / load·store / isAlive / lastActive / createdAt | ~14 | ~330 |
| Send / Interrupt / InterruptViaControl(Detail) / passthrough 透传 / mapSendError | ~11 | ~340 |
| Event 查询透传(EventEntries* / EventLastN / SubscribeEvents / AgentEventLog / SubagentLinker) + sort helper | ~10 | ~280 |
| History 注入 + prev-session 链(InjectHistory / persistedHistoryBefore / Snapshot{Prev,Chain}IDs / SetPrevSessionOrigins / SnapshotPersistedHistory) | ~9 | ~430 |
| Snapshot(单方法 150 行)+ State / DeathReason / HasProcess / parseKeyParts | ~5 | ~340 |
| 包级纯函数(costUnitForBackend / scanLastSummaries / sortEntriesByTimeStable / loadAtomicString 等) | ~6 | ~120 |

**合计约 2262 行**(附录 A 完整行号)。

### 3.2 为什么现在必须拆

**churn 数据**:`git log --oneline -- internal/session/managed.go` → **53 次提交**,在仓库 2 周 / 437 commit 的全历史中位列单文件第 3。几乎每个会话相关改动都要碰这一个 2262 行文件,merge 冲突与回归风险持续累积——这是典型的"结构在和你对抗"型 churn,而非"功能在长出来"型(对比 cron 包:churn 高但已拆成 20 个内聚文件)。

**已有的接口拆分被阅读成本卡住**:文件顶部已经存在 `ProcessSender`(R242-GO-12 已落地)和 `ProcessEventReader`(R242-ARCH-4)两个 facet,godoc 明确写了"后续轮次拆 ProcessLifecycle / EventSource / Introspection"。但这些接口的实现方法散落在 2262 行单文件里、相隔数百行,reviewer 要把"哪些方法属于哪个 facet"在脑里重建——文件拆分按职责轴归位后,facet 实现就天然聚到对应文件,接口拆分的后续轮次才推得动。

**Snapshot 热路径(1 Hz × N tabs × N sessions)的多个性能项**(R215-ARCH-P2-7 / R219-PERF-3 / R222-PERF-7)都要同时看 `Snapshot`(1503-1653)+ `loadCLIIdentity`(494)+ `loadTotalCost`(464)+ 各 atomic 字段定义(238-414),跨度 1400 行,加不进单次 review context。

这些都不是难在逻辑,是难在**阅读成本**。文件拆分属于典型"低风险预置工程",先交付可读性红利,不动语义。先例 `process-split.md` 已在隔壁 `internal/cli/process.go`(2464 行 → 7 文件)验证过同一手法零回归。

### 3.3 现有代码已示范的拆法

同 package 下 `router` 家族已是"按职责轴拆"的形态,证明 Go 对同 package 多文件零成本:

- `router_core.go` / `router_lifecycle.go` / `router_cleanup.go` / `router_backend.go` / `router_discovery.go` / `router_shim.go` —— 同一个 `Router` struct 的方法按职责分到 6+ 文件
- `internal/cli/process*.go` —— `process-split.md` 落地后的 7 文件形态

本 RFC 让 `ManagedSession` 向这种风格收敛,与 router 对称。

## 4. 拆分方案

### 4.1 文件清单

```
internal/session/
├── managed.go                # ManagedSession struct + 接口契约 + SessionSnapshot + 常量    (~470 行)
├── managed_identity.go       # 一行访问器 / atomic helper / cli identity / cost / workspace  (~380 行)
├── managed_lifecycle.go      # process attach/reattach/adopt / load·store / isAlive / 时间戳  (~330 行)
├── managed_send.go           # Send / Interrupt / InterruptViaControl / passthrough 透传      (~340 行)
├── managed_history.go        # InjectHistory / prev-session 链 / persistedHistory 快照        (~440 行)
└── managed_query.go          # event 查询透传 + Snapshot + State/DeathReason + 包级纯函数      (~480 行)
```

合计 ≈ 2260 行(原始 2262 行零净增减,仅 file-header/import 复制若干次产生几十行 noise)。

### 4.2 每个文件承担的方法清单(精确到行号)

下表基于 `get_document_symbols internal/session/managed.go` 的输出。行号为当前 managed.go 的起始行。

#### 4.2.1 `managed.go`(留守)

负责:`ManagedSession` struct 定义、三个 facet 接口契约、box struct、导出快照类型、构造期常量、最贴身的 `SessionKey` 一行访问器。

**留守区构成**:第一个方法在第 **420 行**,故 1-419 行(常量 + 3 接口 + 编译期断言 + box struct + `ManagedSession` struct)是**硬留守**,不含任何可搬方法。再加夹在 query 方法群中间、但必须留守的导出 `SessionSnapshot` struct(1362)。

| 当前行号 | 符号 | 说明 |
|---|---|---|
| 24 | `maxPersistedHistory` / `maxPrevSessionIDs` 常量块 | 跨 history/lifecycle 文件引用,留主文件 |
| 53 | `type ProcessSender interface` | **package 级契约**(cron/dispatch/upstream 未来窄依赖目标);留主文件维持可见性 |
| 99 | `type ProcessEventReader interface` | 同上 |
| 137 | `type processIface interface` | god-interface,facet 拆分的锚点;留主文件 |
| 208 | `var _ ProcessEventReader = (processIface)(nil)` | 编译期子集断言;紧贴接口定义,留主文件 |
| 235 | `type processBox struct` | `atomic.Pointer[processBox]` 的载体;与 struct 紧挨 |
| 238 | `type ManagedSession struct` | 主体 |
| 417 | `type historySourceBox struct` | atomic 载体 |
| 420 | `func (s *ManagedSession) SessionKey() string` | 一行 getter,与 `key` 字段绑死,留主文件作锚 |
| 1362 | `type SessionSnapshot struct` | **导出类型**,`Snapshot()` 返回值,外部包(server/dispatch)消费 —— **必须留守**;它夹在 query 方法群中,Phase 5 搬迁时勿连带移走 |

**注**:`cliIdentityBox` struct(483)虽在留守区行号区间之后,但它紧跟 identity 读写方法,移到 `managed_identity.go`(见 §4.2.2 / §5)。

#### 4.2.2 `managed_identity.go`

负责:无状态/单字段访问器、atomic 编解码 helper、CLI identity 三合一、cost、workspace、label/model。全是 lock-free 读写。

| 当前行号 | 符号 | 说明 |
|---|---|---|
| 424 | `Workspace` / 430 `setWorkspace` / 433 `IsExempt` | 一行访问器 |
| 445 | `loadAtomicString` / 449 `storeAtomicString` | 包级 atomic helper |
| 464 | `loadTotalCost` / 475 `storeTotalCost` | 包级 atomic float64 helper |
| 483 | `cliIdentityBox` struct + 494 `loadCLIIdentity` / 509 `updateCLIIdentity` | identity 三合一(CAS loop) |
| 528 | `Backend` / 532 `SetBackend` / 540 `CLIName` / 543 `SetCLIName` / 551 `CLIVersion` / 554 `SetCLIVersion` | identity 单字段读写 |
| 562 | `UserLabel` / 571 `SetUserLabel` / 575 `LabelOrigin` / 581 `setLabelOrigin` | label |
| 585 | `Model` / 590 `SetModel` | model |
| 597 | `SetHistorySource` / 603 `loadHistorySource` | historySource atomic 读写 |

**额外搬家**:`cliIdentityBox` struct(483)与其读写方法同文件,语义内聚。

#### 4.2.3 `managed_lifecycle.go`

负责:process 的 attach / reattach / adopt、process 指针的 atomic load·store、存活判定、活跃/创建时间戳。

| 当前行号 | 符号 | 说明 |
|---|---|---|
| 805 | `loadProcess` / 818 `storeProcess` | `atomic.Pointer[processBox]` 读写;跨多文件被调,但归属 lifecycle |
| 831 | `isAlive` | 存活判定 |
| 838 | `ReattachProcess` / 873 `ReattachProcessNoCallback` | reconnect 路径 |
| 900 | `adoptProcessAlreadySeeded` | takeover 路径 |
| 926 | `attachProcessAndSnapshotPersisted` | 首次 attach + persistedHistory 快照(注意与 historyMu 的 publish/snapshot ordering) |
| 963 | `LastActive` / 968 `touchLastActive` | 时间戳 |
| 975 | `initCreatedAtIfUnset` / 984 `createdAtMillis` | createdAt 锚定 |

**风险点**:`attachProcessAndSnapshotPersisted`(926)与 `adoptProcessAlreadySeeded`(900)**都**在 `historyMu` 下置 `persistedSeededLen = len(persistedHistory)`,与 `managed_history.go` 的 `InjectHistory` 共享该不变性(lifecycle 文件因此有 2 个 historyMu 触点,见 §5.2)。两文件同 package、共享同一把锁,搬迁不影响正确性,但两处 godoc 对 publish/snapshot ordering 的论述须原样复制。

#### 4.2.4 `managed_send.go`

负责:用户消息出向、中断语义、passthrough 透传。

| 当前行号 | 符号 | 说明 |
|---|---|---|
| 1011 | `SendPassthrough` | passthrough Send 透传到 process |
| 1065 | `SupportsPassthrough` / 1075 `DiscardPassthroughPending` / 1085 `PassthroughDepth` | passthrough 生命周期透传 |
| 1096 | `mapSendError` | Send 错误归一;Send 与 SendPassthrough 共用 |
| 1113 | `Send` | 主出向路径(`sendMu` 串行化) |
| 1171 | `Interrupt` | SIGINT 透传 |
| 1189-1211 | `InterruptOutcome` type(1192)+ const 块(1194) | 中断结果枚举;与下方 String/InterruptViaControl 同轴,整组搬入 send |
| 1214 | `func (o InterruptOutcome) String()` | 枚举 Stringer(**v1 误标 →query;实为 send 轴**,对齐 process-split `ProcessState.String` 跟枚举走) |
| 1245 | `InterruptViaControl` / 1267 `InterruptViaControlDetail` | in-band control_request 中断 |

**风险点**:`Send`(1113)持 `sendMu`,`Interrupt`/`InterruptViaControl` 走 `sendCancel` atomic。锁顺序与 `mapSendError` 的错误映射须逐字节保留。`managed_interrupt_idle_race_test.go` / `interrupt_via_control_detail_test.go` 覆盖此处最精妙路径。`InterruptOutcome` 的 type+const+String 三者必须同文件(整组,勿散)。

#### 4.2.5 `managed_history.go`

负责:history 注入、prev-session 链管理、persistedHistory 快照。全部在 `historyMu` 下。

| 当前行号 | 符号 | 说明 |
|---|---|---|
| 638 | `SnapshotChainIDs` | 链 ID 快照 |
| 673 | `SetPrevSessionOrigins` | origin 平行数组维护(长度契约 + bounce-rebuild) |
| 730 | `SnapshotPrevSessionOrigins` / 753 `SnapshotPrevSessionIDs` / 769 `ReplacePrevSessionIDs` | prev-session 链读写 |
| 784 | `SnapshotPersistedHistory` | persistedHistory 防御性拷贝 |
| 1923 | `persistedHistoryBefore` | 分页 disk-tier 回退的内存层 |
| 1998 | `InjectHistory` | 170 行;唤醒路径,写 persistedHistory + 转发未 seed 的 tail |

**风险点**:`InjectHistory`(1998)是本 RFC 最敏感方法——`persistedSeededLen` / `persistedHistorySorted` 不变性、与 `attachProcessAndSnapshotPersisted` 的 ordering。`inject_history_lockheld_test.go` / `inject_history_test.go` / `reattach_history_test.go` / `prev_session_origins_test.go` 是兜底。

#### 4.2.6 `managed_query.go`

负责:只读 event 查询透传、Snapshot 聚合、状态查询、包级纯函数。

| 当前行号 | 符号 | 说明 |
|---|---|---|
| 1313 | `getSessionID` / 1323 `SessionID` / 1326 `setSessionID` | sessionID 读写(贴近 Snapshot 消费) |
| 1334 | `parseKeyParts` | key 分段(keyOnce 保护) |
| 1455 | `HasProcess` / 1465 `State` / 1477 `DeathReason` | 状态查询 |
| 1503 | `Snapshot` | **150 行**;1 Hz × N tabs 热路径,聚合所有 atomic 字段 |
| 1661 | `hasInjectedHistory` | 一行 |
| 1669 | `EventEntries` / 1707 `SubagentLinker` / 1717 `AgentEventLog` / 1726 `loadCliProcess` | 查询透传 |
| 1739 | `EventLastN` / 1791 `EventEntriesSince` / 1870 `EventEntriesBefore` / 1896 `EventEntriesBeforeCtx` | event 分页查询;**`EventEntriesSince` 内含 RLock→Lock 升级惰性排序**(见 §5.2 / §W1) |
| 1956 | `SubscribeEvents` | 订阅透传 |
| 1982 | `LogSystemEvent` | 系统事件写入 |
| 1767 | `sortEntriesByTimeStable` | 包级纯函数 |
| 2205 | `scanLastSummaries` | 包级纯函数(Snapshot 辅助) |
| 2235 | `costUnitForBackend` + 2262 `costUnitForBackendOnce` (var) | 包级纯函数 + 其 `sync.Once`;两者必须同文件 |

(`extractLastPromptFromProcess` 2175 是 `*ManagedSession` 方法,与 scanLastSummaries 同搬;`SessionID`/`getSessionID`/`setSessionID` 见 §10 Q2,本 RFC 暂放 query。)

> **`String` 归属更正**:v1 此表曾列 `1214 String (→query)` —— 已移除。该 `String()` 是 `InterruptOutcome` 的 Stringer,归 `managed_send.go`(见 §4.2.4)。

**权衡**:`Snapshot` 与 event 查询同文件,因为 Snapshot 内部就在聚合这些查询结果 + identity 字段。`sessionID` 访问器放这里(而非 identity 文件)是因为它与 `Snapshot` / `setSessionID`(首次 Send 捕获)耦合更紧。两种归属都对,PR review 可调整。

### 4.3 依赖图(文件级心智模型)

同 package 内无循环依赖风险(Go 同 package 自由互引)。方向为"调用 → 被调用":

```
managed.go (ManagedSession struct, 接口契约, 常量)
    │
    ├──► managed_identity.go  (atomic helper / identity / cost / workspace)
    │         ▲ 被几乎所有文件调用(loadAtomicString / loadCLIIdentity)
    │
    ├──► managed_lifecycle.go (loadProcess/storeProcess / attach / reattach)
    │         ├─► managed_identity.go (storeAtomicString)
    │         └─► managed_history.go  (attach 时快照 persistedHistory)
    │
    ├──► managed_send.go      (Send / Interrupt)
    │         ├─► managed_lifecycle.go (loadProcess)
    │         └─► managed_identity.go  (touchLastActive 经 lifecycle)
    │
    ├──► managed_history.go   (InjectHistory / prev-session 链)
    │         └─► managed_lifecycle.go (loadProcess 转发 tail)
    │
    └──► managed_query.go     (Snapshot / event 查询透传)
              ├─► managed_lifecycle.go (loadProcess)
              ├─► managed_identity.go  (loadCLIIdentity / loadTotalCost)
              └─► internal/cli (EventLog 透传)
```

关键观察:所有新文件的**函数调用**跨文件依赖都指向 `managed.go`(struct/常量)或 `managed_identity.go`(atomic helper)与 `managed_lifecycle.go`(loadProcess)。**没有"新文件 ↔ 新文件"的函数调用双向依赖**。

> **⚠️ 字段级耦合(函数调用图看不到)**:`EventEntriesSince`(query)与 `InjectHistory`(history)**不通过函数调用、而通过 `persistedHistory` / `persistedHistorySorted` / `persistedSeededLen` 三个字段 + `historyMu` 共享可变状态**。拆文件不切断这种耦合(同 struct 同 package),但意味着 query↔history 在状态层面双向绑定:改其一必须看其二。因此"任意顺序迁移"仅在函数调用层面成立;**Phase 2/3/5 应成对 review**(见 §7.1)。

## 5. Package-private 符号的归属

### 5.1 需要共享的符号清单

| 符号 | 类型 | 归属 | 引用点 |
|---|---|---|---|
| `ManagedSession` struct | struct | `managed.go` | 所有文件 |
| `processBox` / `historySourceBox` | struct | `managed.go` | atomic.Pointer 载体,与 struct 紧挨 |
| `cliIdentityBox` | struct | `managed_identity.go` | identity 读写方法独家配套 |
| `processIface` / `ProcessSender` / `ProcessEventReader` | interface | `managed.go` | **package 级契约文档**;留主文件维持单一可见入口 |
| `loadAtomicString` / `storeAtomicString` | func | `managed_identity.go` | **session 包级基础设施**:managed_identity/lifecycle/query + **router_cleanup.go / router_discovery.go / router_lifecycle.go** |
| `loadTotalCost` / `storeTotalCost` | func | `managed_identity.go` | **session 包级**:Snapshot(query)+ **router_core.go / router_lifecycle.go / store.go** |
| `loadProcess` / `storeProcess` | method | `managed_lifecycle.go` | **session 包级,最大消费方是 router**:send/history/query/lifecycle + **router_cleanup/discovery/lifecycle/shim.go / store.go / testutil.go(!release)** |
| `sortEntriesByTimeStable` | func | `managed_query.go` | `EventEntriesSince` 独家(已核 managed.go-only) |
| `costUnitForBackend` / `scanLastSummaries` | func | `managed_query.go` | Snapshot 辅助,managed.go-only(已核) |
| `extractLastPromptFromProcess` | method | `managed_query.go` | Snapshot 辅助,managed.go-only(已核) |

**原则**:**接口契约与 box struct(atomic 载体)保持在主文件或与其唯一消费方同文件**;**atomic 编解码 helper(`load/storeAtomicString`、`load/storeTotalCost`)与 `load/storeProcess` 是 *session 包级共享基础设施*,被 router 家族 6+ 文件 + store.go + testutil.go 跨文件依赖** —— 放 identity / lifecycle 文件仅为就近,**它们必须保持包私有可见,任何 receiver/签名变更波及上述 router_*.go 调用点**(这点对后续 §10 Q1 facet 接口拆分尤其重要);`sortEntriesByTimeStable` / `costUnitForBackend` / `scanLastSummaries` / `extractLastPromptFromProcess` 经 grep 确认 managed.go 独家,可安全跟随消费方。

### 5.2 跨文件锁不变性(必须逐字节保留)

| 锁 | 保护对象 | 跨文件触点 | 不变性 |
|---|---|---|---|
| `sendMu` | 同 session 消息串行 | `managed_send.go`(Send 持有) | 单 writer;Interrupt 不持 sendMu,走 sendCancel atomic |
| `historyMu` (RWMutex) | `persistedHistory` / `persistedSeededLen` / `persistedHistorySorted` / `prevSessionIDs` / `prevSessionOrigins` | **14 个方法,跨 3 文件**(见下表) | `persistedSeededLen <= len(persistedHistory)`;`len(prevSessionOrigins) <= len(prevSessionIDs)`;**`persistedHistorySorted==true ⇒ persistedHistory 按 Time 升序**(W1) |
| `keyOnce` | `keyPlatform/keyChatType/keyChatID/keyAgentID` | `managed_query.go`(parseKeyParts) | 一次性解析,只读 |
| 13 个 atomic 字段 | 各自字段 | identity / lifecycle / query 多文件 | 读写各经其 helper,不裸 access |

**`historyMu` 全部 14 个持锁触点**(v1 仅举 3 个,严重低估规模):

| 文件 | 方法(行号) |
|---|---|
| `managed_history.go`(8) | `SnapshotChainIDs`(639)、`SetPrevSessionOrigins`(677)、`SnapshotPrevSessionOrigins`(731)、`SnapshotPrevSessionIDs`(754)、`ReplacePrevSessionIDs`(770)、`SnapshotPersistedHistory`(785)、`persistedHistoryBefore`(1927)、`InjectHistory`(2037) |
| `managed_lifecycle.go`(2) | `adoptProcessAlreadySeeded`(901)、`attachProcessAndSnapshotPersisted`(927) |
| `managed_query.go`(4) | `hasInjectedHistory`(1662)、`EventEntries`(1674)、`EventLastN`(1744)、`EventEntriesSince`(1808) |

> **W1 — 最隐蔽的跨文件耦合**:`EventEntriesSince`(query,行 1808)在**读路径里偷偷做写** —— 当 `persistedHistorySorted==false` 时,它 `RUnlock → Lock → sortEntriesByTimeStable + 置 flag=true → Unlock → RLock`(双重检查锁升级,R040034-PERF-6 / R260528-PERF-4)。同一个 `persistedHistorySorted` 的另一个写者是 `InjectHistory`(history,eager 排序)。**这两个分处 query 与 history 两个文件的方法,通过该布尔标志 + historyMu 构成隐式协议**:flag 的语义("已排序")必须两边一致维护。拆分后改任一方,必须同时看另一方。这是比 process-split 任何一处都更需要警惕的点,**Phase 5(query)风险等级因此为「中-高」,不是「低」**。

`historyMu` 跨三文件是本拆分**唯一真正棘手**的点。三者同 package 共享同一把 `s.historyMu`,文件位置不影响锁语义——但每个方法体内的 `Lock`/`RLock`/`Unlock` 配对(尤其 `EventEntriesSince` 的升级/降级序列)须 byte-identical 复制,且 godoc 中对 ordering / flag 语义的论述原样保留。

## 6. 测试文件:近零改动(2 个 source-introspecting 测试除外)

> **v2.3 实测订正**:本节标题原为"零改动",落地后证伪。绝大多数测试确实零改动,但有 **2 个按文件名读取源码的 source-introspecting 测试**必须改文件名字面量(断言意图不变):`reattach_contract_test.go`(ReattachProcessNoCallback 定义文件 → `managed_lifecycle.go`)、`managed_interrupt_idle_race_test.go`(Interrupt 定义文件 → `managed_send.go`)。详见 §0 v2.3。

现有 `internal/session/*_test.go` 清单 **75 个**:**73 个 `package session`(内部测试)+ 2 个 `package session_test`(外部测试:`contract_test.go`、`planner_key_contract_test.go`)**。此外还有 `testutil.go`(`//go:build !release` 跨包测试工具,非 `_test.go`,调用 `storeProcess`)。除上述 2 个 source-introspecting 测试外,内部测试与 testutil.go 与被搬代码同包,文件间移动对其完全透明;外部测试仅能调用导出 API,而本 RFC **零 API 改动**,故同样不受影响。直接覆盖 `ManagedSession` 方法的约 22 个测试与拆分后 .go 的对位:

| 测试文件 | 核心测试对象 | 拆分后的 .go 对位 |
|---|---|---|
| `managed_test.go` | 综合 | 跨多文件 |
| `snapshot_*_test.go`(6 个:cli_identity / last_response / message_count / no_history_copy / normalize / setmodel_idempotent) | Snapshot | `managed_query.go` |
| `inject_history_test.go` / `inject_history_lockheld_test.go` | InjectHistory + historyMu | `managed_history.go` |
| `reattach_history_test.go` / `reattach_contract_test.go` | ReattachProcess + persistedHistory | `managed_lifecycle.go` + `managed_history.go` |
| `prev_session_origins_test.go` | SetPrevSessionOrigins 长度契约 | `managed_history.go` |
| `managed_chain_accessors_test.go` | SnapshotChainIDs / Prev* | `managed_history.go` |
| `event_entries_test.go` / `process_event_reader_test.go` | EventEntries* 透传 | `managed_query.go` |
| `interrupt_via_control_detail_test.go` / `managed_interrupt_idle_race_test.go` | Interrupt(Detail) 竞态 | `managed_send.go` |
| `has_injected_history_test.go` | hasInjectedHistory | `managed_query.go` |
| `log_system_event_test.go` | LogSystemEvent | `managed_query.go` |
| `snapshot_cli_identity_test.go` / `label_origin_test.go` / `label_test.go` | identity / label | `managed_identity.go` + `managed_query.go` |
| `atomic_pointer_contract_test.go` / `atomic_total_cost_contract_test.go` | atomic helper 契约 | `managed_identity.go` |
| `store_created_at_test.go` | createdAt | `managed_lifecycle.go` |
| `process_iface_rename_plan_test.go` | Get 前缀改名守门 | `managed.go`(接口契约) |

**验证脚本**:迁移每个 phase 后跑
```bash
go test -race -count=1 ./internal/session/...
```
期望:保持现有 pass/skip 数目,无新增 fail。

## 7. 迁移步骤

每个 phase 是**一个独立 commit + PR**。按"被依赖者先"顺序最小化冲突面。依赖图(§4.3)显示新文件只 outbound **函数调用**依赖,但 **Phase 2/3/5 经 `historyMu` + `persistedHistory*` 字段共享可变状态**(§5.2 / §4.3 字段耦合注),故它们**不是真正互相独立**:推荐由同一人连续完成、或至少同一轮 review,避免跨 phase 的锁/flag 不变性在并行 rebase 中割裂。Phase 1/4 与其余无字段耦合,可安全并行。

### Phase 1 — `managed_identity.go`
**搬运**:`Workspace`/`setWorkspace`/`IsExempt`、`loadAtomicString`/`storeAtomicString`、`loadTotalCost`/`storeTotalCost`、`cliIdentityBox` struct + `loadCLIIdentity`/`updateCLIIdentity`、`Backend`/`SetBackend`/`CLIName`/`SetCLIName`/`CLIVersion`/`SetCLIVersion`、`UserLabel`/`SetUserLabel`/`LabelOrigin`/`setLabelOrigin`、`Model`/`SetModel`、`SetHistorySource`/`loadHistorySource`。
**风险**:无;纯访问器与 atomic helper。先做这个让后续 phase 的依赖目标就位。

### Phase 2 — `managed_lifecycle.go`
**搬运**:`loadProcess`/`storeProcess`、`isAlive`、`ReattachProcess`/`ReattachProcessNoCallback`、`adoptProcessAlreadySeeded`、`attachProcessAndSnapshotPersisted`、`LastActive`/`touchLastActive`、`initCreatedAtIfUnset`/`createdAtMillis`。
**风险**:`attachProcessAndSnapshotPersisted` 的 historyMu publish/snapshot ordering godoc 须原样复制。

### Phase 3 — `managed_history.go`
**搬运**:`SnapshotChainIDs`、`SetPrevSessionOrigins`、`SnapshotPrevSessionOrigins`/`SnapshotPrevSessionIDs`/`ReplacePrevSessionIDs`、`SnapshotPersistedHistory`、`persistedHistoryBefore`、`InjectHistory`。
**风险**:`InjectHistory` 的 `persistedSeededLen`/`persistedHistorySorted` 不变性 + 长度契约。`persistedHistorySorted` 标志与 Phase 5 的 `EventEntriesSince` 跨文件共享(§5.2 W1)——**Phase 3 与 Phase 5 应成对 review**。跑 `inject_history_*_test.go` / `prev_session_origins_test.go` / `reattach_history_test.go`。

### Phase 4 — `managed_send.go`
**搬运**:`SendPassthrough`、`SupportsPassthrough`/`DiscardPassthroughPending`/`PassthroughDepth`、`mapSendError`、`Send`、`Interrupt`、`InterruptOutcome` type(1192)+const 块(1194)+`String`(1214)、`InterruptViaControl`/`InterruptViaControlDetail`。
**风险**:`sendMu` 持有边界 + `sendCancel` atomic 顺序;`InterruptOutcome` 的 type+const+String 三者整组同搬(勿散)。跑 `managed_interrupt_idle_race_test.go` / `interrupt_via_control_detail_test.go` / `interrupt_outcome_string_test.go`。

### Phase 5 — `managed_query.go`
**搬运**:`getSessionID`/`SessionID`/`setSessionID`、`parseKeyParts`、`HasProcess`/`State`/`DeathReason`、`Snapshot`、`hasInjectedHistory`、`EventEntries`/`SubagentLinker`/`AgentEventLog`/`loadCliProcess`、`EventLastN`/`EventEntriesSince`/`EventEntriesBefore`/`EventEntriesBeforeCtx`、`SubscribeEvents`、`LogSystemEvent`、`sortEntriesByTimeStable`、`extractLastPromptFromProcess`/`scanLastSummaries`/`costUnitForBackend`+`costUnitForBackendOnce`。
**不搬**:`InterruptOutcome.String`(1214)归 Phase 4 send;`SessionSnapshot` struct(1362)留守 managed.go(§4.2.1)——这两个夹在 query 行号区间内,搬迁时最易误带,务必排除。
**风险**:(a) `Snapshot` 150 行须逐字节;它读 ~10 个 atomic 字段,任一 Load 漏掉都是 silent 回归。(b) **`EventEntriesSince` 的 RLock→Lock 升级惰性排序**(§5.2 W1)与 Phase 3 `InjectHistory` 共享 `persistedHistorySorted`——**与 Phase 3 成对 review**,本 phase 风险等级「中-高」。跑全部 `snapshot_*_test.go` + `event_entries_test.go` 并发用例。

### Phase 6(可选)— 回看 `managed.go`
预期留守 ~470 行(1-419 硬留守:常量 + 3 接口 + 编译期断言 + box struct + `ManagedSession` struct;加 `SessionKey` 方法 + `SessionSnapshot` struct)。如有零碎不对位的符号做一次小修正。如 struct 字段注释仍过长,可考虑把 3 个接口契约抽到 `managed_iface.go`——**本 RFC 不推荐**,接口与它描述的 struct 同文件更利于 reviewer 对照。

### 7.1 合并顺序自由度
Phase 1(identity)先行让依赖目标就位。Phase 2/3/5 共享 historyMu 状态,**建议同一人连续完成、单轮 review 成对检查**(§4.3 / §5.2);Phase 1/4 可与其余并行。不推荐 5 文件 big-bang——字段耦合让 big-bang 的 diff 太难一次性审清。

## 8. 验证准则

### 8.1 每 phase 必过
```bash
go build ./...
go test -race -count=1 ./internal/session/...
go vet ./internal/session/...
gofmt -l internal/session/
```

### 8.2 diff 质量门槛
预期**纯 move,零净改**:
```bash
git diff --stat HEAD~1 -- internal/session/managed.go internal/session/managed_*.go
```
若出现"被搬方法 body 的内容 diff"(非缩进/空行),立即 abort——某个字符变了就破坏"零语义改动"承诺。

### 8.3 git blame 保真
与 `process-split.md` §8.3 同:`-M90%` 仅对 >= 500 行大块搬迁有效;小 phase blame 会断到 move commit。缓解:每 phase 搬尽量大块(本 RFC 每文件 330-480 行,接近阈值),commit message 用项目既有中文格式:
```
refactor(session): 把 identity 访问器从 managed.go 拆出 [move-only, no semantic change]
```
`git log -S'InjectHistory'` 仍能跳过 move commit 命中语义修改。

### 8.4 运行时回归
部署前手动验证:
1. 冷启动 `sudo systemctl restart naozhi` → dashboard 所有 session 可见、cost 不闪 $0.00(Snapshot fallback 路径)
2. 发消息 → Send 正常、event 实时渲染
3. 长 turn → EventEntriesSince 增量推送无丢失
4. 中断 → 下一条消息 InterruptViaControl 正常 settle
5. shim reconnect 后翻历史 → InjectHistory + persistedHistory 链完整
6. **并发压测(针对 W1)**:dashboard 多 tab 同时 1 Hz 轮询同一 dead session 持续 1 分钟 → `EventEntriesSince` 锁升级路径无 panic / 无 `-race` 告警

## 9. 风险 & 回滚

### 9.1 风险
**R1(高)—— merge conflict**:managed.go churn 极高(53 commit),拆分期间并发 PR 几乎必撞。**缓解**:选并发谷期一次性推完(5 commit ~30 分钟),或短窗口冻结 managed.go 写入。这是本 RFC 比 process-split 更需要协调的点。

**R2(中)—— historyMu 跨三文件 / 14 触点**:`historyMu` 持锁的 14 个方法散落 history(8)/lifecycle(2)/query(4) 三文件(§5.2 完整表)。future 改动者可能误以为锁只在一个文件而漏掉别处配对——尤其 `persistedHistorySorted` 标志在 history 的 `InjectHistory`(eager)与 query 的 `EventEntriesSince`(lazy 升级)两处维护,`persistedSeededLen` 在 history 的 `InjectHistory` 与 lifecycle 的 `attach`/`adopt` 两处置位。**缓解**:§5.2 锁不变性表 + 每个持锁方法 godoc 标注"shares s.historyMu with managed_{history,lifecycle,query}.go"。

**R3(中-高)—— EventEntriesSince 锁升级 + Snapshot 逐字节**:(a)`EventEntriesSince`(query)的 RLock→Lock→RLock 升级序列(W1)是全文件最精妙并发,搬迁时 Lock/Unlock 配对或 double-check 错一处即 race;(b)`Snapshot` 150 行读 ~10 atomic 字段,漏 Load 是 silent 回归。**缓解**:`event_entries_test.go` 并发用例 + `snapshot_*_test.go` 6 测试 + diff 门槛(§8.2)+ §8.4 第 6 条并发压测。

**R4(低)—— git blame 失真**:同 process-split R2,每 phase 搬 >330 行已接近 `-M` 阈值。

**R5(低)—— 接口契约误移**:有人把 `ProcessSender` 等接口跟着实现方法搬走。**缓解**:§5.1 明确接口留主文件;`process_iface_rename_plan_test.go` 守门 Get 前缀,任何接口签名变动会触发它。

### 9.2 回滚策略
每 phase 独立 commit,`git revert <commit>` 回滚单 phase。纯 move → revert 也纯 move,无语义泄漏。部分回滚:Phase 3(history)若触发生产 bug,单独 revert,Phase 4/5 不受影响(不同文件、无语义重叠)。

## 10. 开放问题

### Q1. 要不要顺带把 `ProcessSender`/`ProcessEventReader` 的 facet 拆分(R242-ARCH-4)一起做?
**不。** 那是接口窄化(改 caller 的依赖声明),是 API 层面的演进;本 RFC 只搬文件、零 API 改动。文件拆分完成后,facet 实现聚到 `managed_send.go`/`managed_query.go`,反而让后续接口拆分更好做。两件事分开立。

### Q2. `sessionID` 访问器该放 identity 还是 query?
本 RFC 放 `managed_query.go`(与 Snapshot / setSessionID 首次捕获耦合紧)。放 identity 也成立。PR review 可调整,不影响正确性。

### Q3. `Snapshot` 单方法 150 行,要不要拆成多个 helper?
**不。** 那是方法内重构 = 语义改动风险,违反 §2。Snapshot 的内联是 1 Hz 热路径的有意选择(R222-PERF-7)。本 RFC 只把它整体搬到 query 文件。

### Q4. 拆完 `managed.go` ~470 行是否还偏大?
主要体积是 3 个接口契约(~150 行 godoc)+ struct 字段注释(~180 行)+ `SessionSnapshot` 定义。如未来 struct 继续膨胀,可再拆 `managed_iface.go`(接口)。本 RFC 暂不含。

### Q5. 为什么不一起拆 `router_*.go`?
router 拆分是独立线(`docs/design/router-split-design.md`),`Router` 方法已分到 6+ 文件,粒度可接受。混在一起会让 diff 失焦。

## 11. 结语

这是一个**纯机械重构**,与已落地的 `process-split.md` 同手法。预期 5 个小 PR,每个 review 5-10 分钟,总工程量 2-4 小时。产出是把 2262 行单文件降到 6 个 <550 行文件,与 router 家族风格对称,直接缓解单文件第 3 高的 churn 冲突,并为 `ProcessSender`/`ProcessEventReader` facet 拆分(R242-ARCH-4)和 Snapshot 热路径性能项扫清阅读瓶颈,不引入任何新的测试/API/并发风险。

唯一比 process-split 更需要注意的两点:**R1(merge conflict)**——managed.go churn 单文件第 3,务必在并发 PR 谷期集中推完;以及 **`persistedHistory*` 字段 + historyMu 的 query↔history 跨文件耦合**(§5.2 W1)——这是 process.go 没有的结构,使本拆分不能照搬 process-split 的"任意顺序"心智模型,Phase 3/5 须成对 review。

---

**附录 A:`managed.go` 全量符号行号索引**(76 func + 5 type·const·var;供 reviewer 做"搬完没漏" checklist。`grep -c '^func '` = 76 应与本表 func 行数一致):

```
--- 留守 managed.go(行号 < 420 的 struct/接口/常量块,此处仅列方法区内的留守符号)---
208  var _ ProcessEventReader = (processIface)(nil)  (→ managed.go 编译期断言)
420  SessionKey                                      (→ managed.go 留守)
1362 type SessionSnapshot struct                     (→ managed.go 留守,导出类型,勿随 query 搬走)

--- identity ---
424  Workspace            430  setWorkspace  433  IsExempt           (→ identity)
445  loadAtomicString     449  storeAtomicString                     (→ identity)
464  loadTotalCost        475  storeTotalCost                        (→ identity)
483  type cliIdentityBox struct                                      (→ identity)
494  loadCLIIdentity      509  updateCLIIdentity                     (→ identity)
528  Backend  532 SetBackend  540 CLIName  543 SetCLIName            (→ identity)
551  CLIVersion  554 SetCLIVersion                                   (→ identity)
562  UserLabel  571 SetUserLabel  575 LabelOrigin  581 setLabelOrigin(→ identity)
585  Model  590 SetModel                                             (→ identity)
597  SetHistorySource     603  loadHistorySource                     (→ identity)

--- history ---
638  SnapshotChainIDs                                                (→ history)
673  SetPrevSessionOrigins                                           (→ history)
730  SnapshotPrevSessionOrigins  753 SnapshotPrevSessionIDs          (→ history)
769  ReplacePrevSessionIDs                                           (→ history)
784  SnapshotPersistedHistory                                        (→ history)
1923 persistedHistoryBefore                                          (→ history)
1998 InjectHistory                                                   (→ history)

--- lifecycle ---
805  loadProcess          818  storeProcess                          (→ lifecycle)
831  isAlive                                                         (→ lifecycle)
838  ReattachProcess      873  ReattachProcessNoCallback             (→ lifecycle)
900  adoptProcessAlreadySeeded                                       (→ lifecycle)
926  attachProcessAndSnapshotPersisted                               (→ lifecycle)
963  LastActive  968 touchLastActive                                 (→ lifecycle)
975  initCreatedAtIfUnset  984 createdAtMillis                       (→ lifecycle)

--- send ---
1011 SendPassthrough                                                 (→ send)
1065 SupportsPassthrough  1075 DiscardPassthroughPending             (→ send)
1085 PassthroughDepth                                                (→ send)
1096 mapSendError         1113 Send                                  (→ send)
1171 Interrupt                                                       (→ send)
1192 type InterruptOutcome int   1194 const(InterruptSent ...)       (→ send)
1214 (o InterruptOutcome) String  [v1 误标 →query,已更正]            (→ send)
1245 InterruptViaControl  1267 InterruptViaControlDetail             (→ send)

--- query ---
1313 getSessionID  1323 SessionID  1326 setSessionID                 (→ query)
1334 parseKeyParts                                                   (→ query)
1455 HasProcess  1465 State  1477 DeathReason                        (→ query)
1503 Snapshot                                                        (→ query)
1661 hasInjectedHistory                                              (→ query)
1669 EventEntries                                                    (→ query)
1707 SubagentLinker  1717 AgentEventLog  1726 loadCliProcess         (→ query)
1739 EventLastN                                                      (→ query)
1767 sortEntriesByTimeStable                                         (→ query)
1791 EventEntriesSince  [RLock→Lock 升级惰性排序,见 W1]              (→ query)
1870 EventEntriesBefore   1896 EventEntriesBeforeCtx                 (→ query)
1956 SubscribeEvents                                                 (→ query)
1982 LogSystemEvent                                                  (→ query)
2175 extractLastPromptFromProcess                                    (→ query)
2205 scanLastSummaries                                               (→ query)
2235 costUnitForBackend   2262 var costUnitForBackendOnce            (→ query)
```

**计数核对**:func 76 个 = 留守 1 · identity 23 · history 8 · lifecycle 11 · send 10 · query 23(`1+23+8+11+10+23=76`)+ type/const/var 5 个(208 断言 / 483 cliIdentityBox / 1192-1194 InterruptOutcome / 1362 SessionSnapshot / 2262 once)。reviewer 搬完后 `grep -c '^func ' internal/session/managed*.go | awk -F: '{s+=$2}END{print s}'` 应仍为 76;`grep -c 'historyMu\.\(R\)\?Lock()' internal/session/managed_{history,lifecycle,query}.go` 各文件应分别命中 history 8 / lifecycle 2 / query 4(共 14 个持锁方法的 call site 数会更多,因 EventEntriesSince 单方法有 3 处)。
