# RFC: cron runStore — 收口半成品 facade（close the half-facade）

> **状态**: Draft v1（待评审）
> **作者**: naozhi team (cron-cr)
> **创建**: 2026-06-04
> **范围**: 把 `*Scheduler` 对 `*runStore` 的访问全部收敛到一组稳定的 `*Scheduler` 方法（facade），隐藏 cache/trim/lock 内部细节；不拆包、不导出边界类型、不改 on-disk 格式。
> **关联 issue**: #509, #978
> **关联代码**:
> - `internal/cron/runstore.go`（2464 行 — 实际行数，triage 写的 1451 已过期）
> - `internal/cron/scheduler.go`（`runStore` 字段定义 + `trimAllCtx` 调用）
> - `internal/cron/scheduler_finish.go`（已有读侧 facade：`ListRuns`/`RecentRuns`/`GetRun`/`CurrentRun`；写侧 `Append`）
> - `internal/cron/scheduler_jobs.go`（`DeleteJob` + `enabled`）
> - `internal/cron/scheduler_session.go`（`RecentSessionIDs` + `enabled` + `Append`）

---

## 1. Background & problem

### 现状（可复现）

`internal/cron` 内 `*Scheduler` 持有一个未导出的 `runStore *runStore` 字段（`scheduler.go:586`）。这个 store 是 cron run 历史的全部持久化与缓存层：per-job atomic 文件写、newest-first ring cache、按 count/window 的 trim/retention，以及一整套精心设计的三层锁体系（见 `runstore.go:65`）：

```
Scheduler.s.mu  >  runStore.jobLock(jobID)  >  recentCacheEntry.mu
```

问题在于访问这个 store 的方式是**半成品 facade**：

- **读侧 4 个查询已经收口**。`scheduler_finish.go:42` 定义了导出接口 `RunHistoryReader`（`CurrentRun`/`ListRuns`/`RecentRuns`/`GetRun`），dashboard handler 经此访问，`runStore` 类型对 `server/` 不可见。这是 facade 该有的样子。
- **但写侧与生命周期侧仍直接打到 `s.runStore.<method>`**。实际生产调用点（grep 去掉注释后核实）分布在 4 个文件：
  - `s.runStore.enabled()` — `scheduler.go`, `scheduler_finish.go`, `scheduler_jobs.go`, `scheduler_session.go`（多处守卫）
  - `s.runStore.Append(&CronRun{...})` — `scheduler_finish.go:273`（终态写）、`scheduler_session.go`（注释引用，实际触发点在 finish）
  - `s.runStore.trimAllCtx(s.stopCtx, time.Now())` — `scheduler.go:1481`（冷启动 GC）
  - `s.runStore.DeleteJob(jobID)` — `scheduler_jobs.go:628`（删 job 的 postCleanup）
  - `s.runStore.RecentSessionIDs(jobID, ...)` — `scheduler_session.go:275`
  - `s.runStore.Recent(...)` / `List` / `Get` — 仍有直接点（虽然多数已走 `RunHistoryReader`）

因此 `runStore` 的方法集既不是"全私有、只经 Scheduler 方法访问"，也不是"全导出、独立子包"，而是卡在中间：**每个 scheduler 文件都要自己记得 `enabled()` 守卫、自己记得 nil 判断、自己直接知道 store 的方法名**。

### 为什么这是问题

1. **`enabled()` nil-guard 散落且易漏**。`StorePath` 为空时持久化关闭，每个调用点都得先 `if s == nil || !s.runStore.enabled()`（见 `scheduler_finish.go:87/96/106`）。读侧已统一，写侧/删除侧的守卫是各写各的，新增调用点极易漏掉 nil 检查——`scheduler_session.go:264` 注释明确写着 "RunStore is nil only in tests"，说明这个 nil 契约只靠注释维系。
2. **锁层级是 load-bearing 但靠 godoc 维系**。`runstore.go:65-69` 的层级、`Append` 的 "先 jobLock 后 entry.mu"、`*Locked` 后缀契约（`assertJobLockHeld`，`runstore.go:599`）跨 `scheduler_finish`/`scheduler_jobs`/`scheduler_session` 被多个调用方依赖。直接暴露 store 方法意味着任何新调用方都可能在错误的锁上下文里调用，例如在持 `s.mu` 时调用一个内部会取 `jobLock` 的方法。
3. **#978: `appendsSinceTrim` 把 cache 与 retention 耦合**。`runstore.go:227` 的 `appendsSinceTrim int` 计数器同时驱动两件事：ring cache 的 head-push 簿记 *和* 周期性 trim 的触发节奏（`skipAppendTrim`，`runstore.go:902`）。它在 `recentCacheEntry`（cache 概念）上，却控制 retention（GC 概念）的 cadence。这是 facade 没收口导致的内部细节外溢的一个具体样本：cache 与 trim 本应是 store 内部的两个关注点，却被一个共享计数器钉死。
4. **#509 root: 三套独立 keyed persistence 反复 reinvent**。runs / events / jsonl / attachments 各写了一份 atomic-write + trim + cache（见 `docs/review/batch3-B-r241-244-raw.md` R244-ARCH-5，已标记为 #509 的 dup）。在 runStore 边界没收紧前，无法安全地把它抽成 `internal/persistence.KeyedStore[K,V]` 模板——因为现在根本不知道"对 runStore 的合法操作集"是哪几个。**收口 facade 是抽模板的前置条件**，本 RFC 只做前置，不做模板本身。

---

## 2. Goals & non-goals

### Goals

- G1: 把对 `s.runStore` 的**全部**生产调用收敛到一组 `*Scheduler` 方法（写侧 + 生命周期侧），与已有的 `RunHistoryReader` 读侧方法风格一致。收口后，`internal/cron` 内除这组 wrapper 方法外，**没有任何文件直接写 `s.runStore.<x>`**。
- G2: 把 `enabled()` nil-guard + nil-store 守卫统一进 wrapper，消除散落的 `if s == nil || !s.runStore.enabled()` 重复。
- G3: 为 #978 记录决策方向（解耦 `appendsSinceTrim`），但**不在本 RFC 落地**（见 non-goals）。
- G4: 为 #509（KeyedStore 模板）铺路：收口后产出一份"runStore 合法操作清单"，作为未来抽模板的接口草案输入。

### Non-goals

- NG1: **不**把 runStore 提升为 `internal/cron/runs` 子包，**不**导出 `runStore`/`recentCacheEntry`/`CronRun` 等边界类型（option 2，见 §3，风险 high，单列后续 RFC）。
- NG2: **不**实现 `internal/persistence.KeyedStore[K,V]` 模板（#509 的真正解法），本轮只收口边界。
- NG3: **不**改 on-disk runs/ 目录布局、文件名、CronRun JSON schema、retention 语义（count/window 阈值不动）。
- NG4: **不**改三层锁层级、不改 `appendsSinceTrim` 的实际行为（#978 的解耦留待独立小 PR，本 RFC 仅记录方向）。
- NG5: **不**改 dashboard / server / 飞书任何调用方——它们经 `RunHistoryReader` 已隔离，收口写侧对外零可见。

---

## 3. Alternatives considered

### Option 1（选中）— in-package 收口 facade

在 `*Scheduler` 上补齐写侧/生命周期侧 wrapper（`appendRun` / `deleteJobRuns` / `recentSessionIDs` / `trimAllRuns` / `runStoreEnabled`），把散落的 `s.runStore.<x>` 全部改走 wrapper。`runStore` 仍未导出、仍在同包、锁层级与 on-disk 一字不动。

- **优点**：纯包内重构，编译边界不变，无 import cycle 风险；与已落地的 `RunHistoryReader` 读侧对称；nil/enabled 守卫一处收敛；可分文件、低行数、行为完全保持；为 #509/#978 提供"合法操作集"这一前置事实。
- **缺点**：runStore 类型仍在包内可见，纪律靠"约定 + 一个 grep 守门测试"维系，而非编译器强制。
- **为何胜出**：triage 明确判 option 1 为可落地 phase-1；它把"半成品 facade"补成"完整 facade"，恰好是 issue 标题要的，且不触碰任何 load-bearing 不变量。

### Option 2（否决）— 提升为 `internal/cron/runs` 子包 + 导出接口

把 runStore 搬到子包，导出 `runs.Store` 接口与边界类型。

- **优点**：编译器强制边界；为多 store 复用铺路最彻底。
- **缺点（致命）**：(a) `CronRun`/`CronRunSummary`/`RunInflightView` 等类型与 scheduler 双向引用，拆包极易触发 import cycle；(b) 强制导出边界类型，扩大公共 API 表面；(c) 三层锁层级中 `s.mu` 属于 Scheduler、`jobLock`/`entry.mu` 属于 store，跨包后锁协作契约更难表达且更易被误用；(d) triage 标注风险 high。**否决理由**：在边界尚未在包内收紧前跨包，是把未验证的接口直接固化成公共 API，违反"先收口再抽象"。

### Option 3（否决）— 维持现状 + 仅加文档

只在 godoc 里强调"请走 facade"。

- **缺点**：不解决散落的 nil-guard、不产出"合法操作集"、对 #509/#978 零推进。否决。

---

## 4. Test strategy

收口是行为保持型重构，测试重点是**证明零行为变化**并**防止边界再次被绕过**。

### Unit（新增）

- `TestScheduler_RunStoreFacade_DisabledIsNoop`：`StorePath` 为空时，`appendRun` / `deleteJobRuns` / `trimAllRuns` / `recentSessionIDs` 全部安全 no-op / 返回 nil，覆盖 G2 的统一守卫（替代散落的 4 处 `enabled()` 判断）。
- `TestScheduler_RunStoreFacade_NilSchedulerSafe`：`(*Scheduler)(nil)` 调每个 wrapper 不 panic（对齐现有 `scheduler_nil_safe_accessors_test.go` 的风格）。
- 收口前后对 `appendRun` 的行为等价：复用现有 `TestRunStore_AppendListRoundTrip`（`runstore_test.go:74`）作为行为锚——wrapper 不应改变写入/读回结果。

### Regression / pin（点名复用 + 1 个新守门测试）

- **新增 `TestNoDirectRunStoreAccess`（守门测试，防回归核心）**：用 `go/parser` 扫 `internal/cron/*.go`（排除 `runstore*.go`、`*_test.go`、以及定义 wrapper 的文件），断言没有任何 `s.runStore.<method>` 选择器表达式。这把"约定"升级成 CI-enforced 不变量，弥补 option 1 "纪律靠约定" 的缺点。
- 复用 `TestRunStore_ConcurrentAppendsSameJobAreSerialised`（`runstore_test.go:617`）— 证明 jobLock 串行化不被 wrapper 破坏。
- 复用 `TestRunStore_DeleteJobReclaimsJobLock`（`runstore_test.go:390`）— 证明 `deleteJobRuns` 仍回收 per-job mutex。
- 复用 `TestRunStore_TrimAllScansAllJobs`（`runstore_test.go:641`）+ `TestRunStore_TrimAllCtxCancelled`（`runstore_test.go:738`）— 证明 `trimAllRuns` 透传 ctx 取消语义不变。

### Integration

- `go test -race ./internal/cron/...` 必须全绿——这是锁层级未被破坏的主验证。
- 现有 `scheduler_*_test.go` 全套作为终态执行路径的集成回归（finishRun → appendRun → trim cadence）。

### #978 测试（仅记录，不本轮实现）

- 现有 `TestRunStore_SkipAppendTrim_Conditions`（`runstore_test.go:894`）已 pin 住 `appendsSinceTrim` 驱动 trim cadence 的行为；解耦该计数器的后续 PR 必须先让此测试以"cache 簿记"与"trim 触发"两个独立信号重写，再动实现。

---

## 5. Risk & rollback

### Load-bearing 不变量（必须保持）

| 不变量 | 锚点 | 现有 pin 测试 |
|---|---|---|
| 锁层级 `s.mu > jobLock > entry.mu` | `runstore.go:65` | `-race` 全套 + ConcurrentAppends |
| `Append` 先 jobLock 后 entry.mu | `runstore.go:854` | `TestRunStore_ConcurrentAppendsSameJobAreSerialised` |
| `*Locked` 后缀 = 调用方已持 jobLock | `assertJobLockHeld` `runstore.go:599` | 运行时 warn + race 测试 |
| DeleteJob 回收 jobLock 且 postCleanup 在 s.mu 外 | `runstore.go:1926`, `scheduler_jobs.go:904` | `TestRunStore_DeleteJobReclaimsJobLock` |
| on-disk runs/ 布局 + CronRun schema | `runstore.go` 写路径 | `TestRunStore_AppendListRoundTrip` |
| StorePath 空 = 持久化关闭 | `enabled()` `runstore.go:196` | 新增 `DisabledIsNoop` |

### 出错会 break 什么

- **最大风险**：wrapper 误改了锁上下文（例如在持 `s.mu` 的路径里调用一个内部取 `jobLock` 的方法），可能引入死锁。**缓解**：wrapper 是**纯转发**——签名、锁责任、调用顺序逐行对齐现有直接调用点，不做任何"顺手优化"；`-race` 测试 + ConcurrentAppends 为护栏。
- 漏改某个 `enabled()` 守卫导致 nil-store panic。缓解：守门测试 + `NilSchedulerSafe`。

### Rollback

- 单 PR、纯包内、无 schema/flag/格式变更 → `git revert` 即可完全回滚，无数据迁移、无残留状态。
- 因 on-disk 与锁均不变，回滚不存在"旧二进制读新数据"问题。

---

## 6. Observability

- 无新增 metric / log / dashboard。收口是内部重构，对外行为零变化。
- 现有可观测性保持：`HistoryDropTotal`（`runstore.go:152`）、`CacheStaleEvictionTotal`（`runstore.go:162`）、`WriteFailedTotals`（`runstore.go:177`）继续经各自 getter 暴露，wrapper 不拦截这些路径。
- `assertJobLockHeld` 的运行时 warn（`runstore.go:606`）保留为锁契约的 in-prod 哨兵。

---

## 7. Compatibility & migration

- **向后兼容**：完全兼容。无导出 API 变更（`RunHistoryReader` 不动），server/dashboard/飞书调用方零改动。
- **on-disk 格式**：不变（NG3）。无迁移。
- **config flag**：无新增 flag；`StorePath` 空=关闭的语义不变。
- **迁移路径**：N/A — 无需迁移，纯代码内收口。

---

## 8. Rollout plan

### Phase 1（本轮，**可独立安全落地**）— 写侧/生命周期侧收口

`hasLandablePhase1 = true`。纯包内、行为保持、不触 load-bearing 不变量、不改 on-disk/锁/schema。

**文件级改动清单（预估）**：

| 文件 | 改动 | 预估行数 |
|---|---|---|
| `internal/cron/scheduler_finish.go` | 新增写侧 wrapper `appendRun(*CronRun)` + `runStoreEnabled()`；把 `:273` 的 `s.runStore.Append` 改走 wrapper；统一 `enabled` 守卫 | ~25 |
| `internal/cron/scheduler_jobs.go` | `:628` `s.runStore.DeleteJob` → 新 wrapper `deleteJobRuns(jobID)`（含 enabled 守卫） | ~12 |
| `internal/cron/scheduler_session.go` | `:275` `s.runStore.RecentSessionIDs` → wrapper `recentSessionIDs(...)`；`:273` 守卫并入 | ~15 |
| `internal/cron/scheduler.go` | `:1481` `s.runStore.trimAllCtx` → wrapper `trimAllRuns(ctx, now)` | ~10 |
| `internal/cron/scheduler_finish.go`（或新 `scheduler_runstore_facade.go`） | wrapper 集中定义 + godoc 说明 facade 契约 | ~30 |
| `internal/cron/scheduler_runstore_facade_test.go`（新） | `DisabledIsNoop` + `NilSchedulerSafe` + `TestNoDirectRunStoreAccess` 守门 | （测试，不计入生产行数）|

**Phase 1 生产代码预估 ≈ 90 行；保守上限 ≤ 130 行。风险 low**（纯转发 + 守卫合并，无锁/格式/schema 变更，全程 `-race` 护栏）。

> 注：若评审倾向把 wrapper 集中到新文件 `scheduler_runstore_facade.go` 而非塞进 `scheduler_finish.go`，行数与风险不变，仅文件组织差异——推荐新文件以保持 `scheduler_finish.go` 聚焦 finish 路径。

### Phase 2（后续 PR，非本轮）— #978 解耦 `appendsSinceTrim`

把 cache head-push 簿记与 trim cadence 触发拆成两个独立信号，先重写 `TestRunStore_SkipAppendTrim_Conditions` 再动实现。预估 medium 风险，单列。

### Phase 3（后续 RFC，非本轮）— #509 `KeyedStore[K,V]` 模板

以 Phase 1 产出的"合法操作集"为接口草案，评估能否把 runs/events/jsonl/attachments 四套收敛到 `internal/persistence.KeyedStore`。risk high，需独立 RFC + 跨包 import cycle 评估（即 option 2 的内容下沉到那里讨论）。
