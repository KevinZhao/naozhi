# RFC: runStore 抽成 internal/cron/runstore 子包(ARCH-RUNSTORE-SUBPKG)

> **状态**: **Rejected / Won't-do(评审结论:子包化 ROI 为负,见 §-1)**。文档保留作"为何不拆"的论证记录;若未来 runStore 需被 cron 包外独立复用,可重启本 RFC。
> **作者**: naozhi team
> **创建**: 2026-05-29
> **修订**: 2026-05-29(v2 修正事实;v2-final 记录 Rejected 决策)

---

## -1. 评审决策(2026-05-29):不做完整子包化

经两轮 adversarial review(§0)+ ROI 复核,**决定不执行本 RFC 的子包化主方案**。理由:

**子包化的核心收益(依赖收敛 + 独立可测)runStore 已基本具备,无需跨包重构去买:**
- Scheduler 通过**单向字段**引用 runStore,零回调;dashboard/cron 已完全通过 scheduler 公开方法访问 —— 依赖结构本就健康,runStore 不是循环源、不是耦合热点。
- 生产代码访问 runStore 私有成员**仅 1 处**(`trimAllCtx`)—— 包内无"乱碰私有实现"现象,子包边界要防的问题当前不存在。

**而 review 揭示成本高于预期:**
- 必须新建 `cronrun` leaf 破环(B4 还排除了并入 runtelemetry 的捷径)+ ErrorClass 归属决策 + 13 处测试改造 + re-export 别名维护。这是跨包重构,风险量级远高于前几次 move-only 拆分。

**结论**:`runstore.go`(1949 行)是**高内聚的单一职责**(持久化),不是混杂 god file —— "大 ≠ 该拆"。子包化用跨包重构的成本买已到手的收益,ROI 为负。

**替代去向(按推荐序)**:
1. **首选**:不动 runStore,转去做 **Server.router 接口化**(consumer-interface 剩余 40%,~30 处 `*session.Router` 具体指针 → 接口,解锁 dashboard 子包独立单测,可复用 dispatch 既有范式)。这是有真实可测性收益、踩在既定路线上的改造。
2. **仅当文件长度确实碍事时**:走 §9 同包 fallback(runstore.go → 3 个同包文件,纯 move-only 零风险),但这只解决"文件长"、不改善架构。
3. 完整子包化(下文主方案):**搁置**,除非出现"runStore 被 cron 以外的包复用"的明确需求。

以下 §0–§10 为评审时的完整论证与方案,保留备查。
> **范围**: `internal/cron/runstore.go`(1949 行) → 独立子包 `internal/cron/runstore`;**跨 package 边界,非 move-only**
> **关联代码**: `internal/cron/runstore.go`、`internal/cron/run.go`、`internal/cron/job.go`、`internal/cron/limits.go`、`internal/cron/scheduler.go`、`internal/server/wshub_broadcast.go`
> **先例**: `docs/rfc/eventlog-split.md`(ARCH-EVENTLOG-SPLIT)、`docs/rfc/managed-session-split.md`(ARCH-MANAGED-SPLIT)——**但本 RFC 与它们不同:那两个是同包 move-only,本 RFC 跨包,会改 API 表面**
> **不解锁但相关**: cron-sysession-merge Phase C(executeOpt 拆分,独立进行)

---

## 0. 修订历史

### v2(2026-05-29)— adversarial review 修正 3 处事实错误

v1 的事实盘点有一个 grep bug + 一处类型分析遗漏,经两支独立 review 队对照真实代码发现:

- **B1(grep bug,根因)**:v1 §3.1/§3.3 多处用 `grep ... internal/cron/*.go | grep -v _test.go`。`grep -v _test.go` 作用在**已无文件名的管道输出**上,完全失效,导致测试调用混入生产计数。**凡过滤测试文件必须用 `grep -L`/`--include` 或 `grep -rn ... | grep -v '_test.go:'`(冒号锚定文件名)**。
- **B2(IsValidID 严重低估)**:v1 §3.3/§D3 称 `cron.IsValidID` 包外仅 server 1 处引用。**实际 11 处**:server/wshub_broadcast.go ×1 + **dashboard/cron ×10**(handlers.go:1285/1335/1380/1437、runs.go:64/147/160、transcript.go:795/808、update.go:37)。D3 的迁移影响被低估一个量级。
- **B3(ErrCorruptRun 非零外引用)**:v1 §3.3 称包外零引用。**实际 dashboard/cron 2 处**(runs.go:166、transcript.go:364)。
- **B4(ErrorClass 不能并入 runtelemetry —— 否决 D2 的简化分支)**:v1 §4 D2-a 建议把 `ErrorClass` 并入 `runtelemetry`。但 `cron.ErrorClass`(job.go:212)含 **3 个 cron 独有的竞态常量** `ErrClassRouterMissing`/`ErrClassPausedConcurrent`/`ErrClassDeletedConcurrent`(描述 executeOpt CAS 窗口竞态,#1323/#1410),并入会污染 runtelemetry 的中立共享词汇。**必须用独立 `cron/cronrun` leaf 包承载 ErrorClass,不能塞进 runtelemetry**。

v1 的核心结论**不变**:循环依赖真实存在(已二次确认 scheduler_finish.go:227 构造 `&CronRun{}` 传 `runStore.Append`,签名 runstore.go:539),leaf 破环方向正确,生产代码访问 runStore 私有成员仍只有 1 处(`trimAllCtx`)。修正集中在迁移范围(IsValidID/ErrCorruptRun 的下游)与 leaf 包归属(ErrorClass 必须独立 leaf)。

---

## 1. 动机与定位(为什么这个不是 move-only)

`runstore.go`(1949 行)是 `internal/cron` 这个全仓最大源码包(11K 行)里**唯一真正自包含的子领域**:

- 独立锁模型:`jobLocks sync.Map`(per-job mutex),**不共用** `Scheduler.mu`
- 独立持久化:`runs/<jobID>/<runID>.json` 落盘 + ring 缓存 + trim/GC
- Scheduler 仅通过一个 `runStore *runStore` 字段单向引用它,**零回调**(不像 EventLog 有 PersistSink 反向扇出)
- dashboard/cron 完全通过 `scheduler.RecentRuns / ListRuns / GetRun` 公开方法访问,**与 runStore 实现零耦合**

但和最近三次拆分(process/managed/eventlog/router,全是同包拆文件)**本质不同**:那些 move-only 拆分符号可见性不变、可"先 assert 无损重建再写出"。本 RFC **跨 package 边界**——小写未导出符号若被 cron 主包用到就必须导出,**改变了 API 表面**,机械重建手法不适用。因此需要本 RFC 把三类决策定清后再施工。

**为什么值得做**:cron 包从 11K 行瘦身 ~2K;runStore 获得独立可测的包边界(不启动 scheduler 即可测持久化/trim/缓存);最大文件 runstore.go(1949)退出榜首。这是当前唯一"既减包体积、又真正改善依赖结构"的高 ROI 项(router-split 已落地、Phase 4b 被 ADR-001 判负 ROI)。

---

## 2. 目标与非目标

**目标**:
- `runstore.go` + 其测试迁入新包 `internal/cron/runstore/`
- 锁层级文档(`jobLock` / `assertJobLockHeld` 注释)随代码进子包
- 子包对 cron 主包**单向依赖**(import cron 取共享类型),无循环
- `go build ./...` + `go vet` + `go test -race ./internal/cron/...` + 下游(server/dashboard)全绿

**非目标**:
- **不**改任何持久化格式 / 落盘布局 / 锁语义(纯结构迁移 + 必要导出)
- **不**拆 `Scheduler` god struct(那是另一条线)
- **不**做 executeOpt 拆分(cron-sysession-merge Phase C 独立)
- **不**把 `Scheduler.mu` 下的 job 管理逻辑挪进子包(强耦合,留主包)

---

## 3. 事实盘点(可机械复现)

### 3.1 cron 主包(生产代码)对 runStore 的全部调用点

```bash
# 正确写法:冒号锚定文件名才能真正排除测试(v1 的 | grep -v _test.go 失效,见 §0 B1)
$ grep -rnE 's\.runStore\.[A-Za-z]+' internal/cron/*.go | grep -v '_test.go:' \
    | grep -oE 's\.runStore\.[A-Za-z]+' | sort | uniq -c
```

| 调用 | 生产次数 | 当前可见性 | 拆包后 |
|---|---|---|---|
| `Append` | 1(scheduler_finish.go:227) | PUB | 不变 |
| `DeleteJob` | 2(scheduler_jobs.go:696,1294) | PUB | 不变 |
| `Get`(finish:81) / `List`(finish:62) / `Recent`(finish:71) / `RecentSessionIDs`(session:273) | 各 1 | PUB | 不变 |
| `trimAllCtx` | 1(scheduler.go:1195) | **priv** | **需导出 → `TrimAllCtx`** |

**生产代码访问 runStore 私有成员的点:只有 1 处** —— `scheduler.go:1195` 的 `trimAllCtx`(此核心论断 v1/v2 一致)。所有其它私有方法(cacheGet / warmCache / trimJobLocked / scanSortedRunDir …共 ~20 个)**仅 runStore 内部自用**,拆包后保持小写不变。
> v1 此表把 `Append` 记为 3 —— grep bug 把 2 处测试调用混入(见 §0 B1)。

### 3.2 测试对私有成员的访问(决定测试改造面)

```bash
$ grep -rnoE '\.runStore\.(root|keepCount|keepWindow|disabled|enableTrimGC|trimAll)' internal/cron/*_test.go
```

| 私有成员 | 测试触点 | 文件 |
|---|---|---|
| `disabled`(字段) | 4 | delete_persist_runs_cleanup_test.go、run_p1_integration_test.go |
| `root`(字段) | 2 | delete_persist_runs_cleanup_test.go |
| `keepCount`/`keepWindow`(字段) | 4 | scheduler_test.go |
| `enableTrimGC`(字段) | 2 | run_p1_integration_test.go |
| `trimAll`(方法) | 1 | run_p1_integration_test.go |

这些是**当前在 `package cron` 内的测试**直接戳 runStore 私有字段。拆包后它们分两类处理(见 §5.3)。

### 3.3 共享类型 / helper 的归属现状

```bash
$ grep -rn '^type CronRun \|^type CronRunSummary \|^type ErrorClass\|^func IsValidID\|MaxRunRecordBytes\s*=\|^var ErrCorruptRun' internal/cron/*.go | grep -v _test.go
```

| 符号 | 定义位置 | 被谁用 | 跨包引用? |
|---|---|---|---|
| `CronRun` | run.go:39 | scheduler 构造 + runstore 持久化 | **包外零直接引用** |
| `CronRunSummary` | run.go:67 | runstore 缓存/序列化 + scheduler 转发 | **包外零直接引用** |
| `RunState`/`TriggerKind` | job.go(runtelemetry alias) | runstore + scheduler | 包外零直接引用 |
| `ErrorClass` | job.go:212 | runstore + scheduler | 包外零直接引用 |
| `MaxRunRecordBytes` | limits.go:312 | runstore(3 处) | — |
| `IsValidID` | runstore.go:335 | runstore 内部 + **包外 11 处** | **server ×1 + dashboard/cron ×10**(见下) |
| `ErrCorruptRun` | runstore.go:260 | runstore + **dashboard/cron ×2** | **非零外引用**(runs.go:166、transcript.go:364) |

`IsValidID` 包外 11 处真实调用(v2 修正,v1 仅见 1 处):
```
server/wshub_broadcast.go:410
dashboard/cron/handlers.go:1285,1335,1380,1437
dashboard/cron/runs.go:64,147,160
dashboard/cron/transcript.go:795,808
dashboard/cron/update.go:37
```
这意味着 `IsValidID` 已是 cron 包事实上的**公共 ID 校验 API**,迁移它要同步改 11 处 import(或在 cron 主包留 re-export)。

### 3.4 异味:生产文件 import testing

`runstore.go:505` 用 `testing.Testing()` 把 `assertJobLockHeld` 的锁探针挡在生产路径外——导致**生产文件 `import "testing"`**。拆包是顺手清理它的好时机(见 §6)。

---

## 4. 核心决策(本 RFC 的存在理由)

### 决策 D1:共享数据契约 `CronRun` / `CronRunSummary` 放哪?

三个选项:

| 选项 | 后果 | 评价 |
|---|---|---|
| **A. 留 cron 主包(run.go 不动)** | 子包 `runstore` import `cron` 取类型;scheduler 也用同一类型,Append(&cron.CronRun{}) 直接传 | **推荐** |
| B. 下沉进 runstore 子包 | scheduler 要 import `cron/runstore` 取 CronRun——但 scheduler 本就在 cron 主包,变成 cron→runstore 取类型 + runstore→cron 取 RunState/ErrorClass = **循环** | ❌ 循环 |
| C. 抽第三个中立包 `cron/cronrun` | 多一个包,CronRun 仅 2 个使用方,过度工程 | ❌ 过重 |

**选 A**:`CronRun`/`CronRunSummary`/`RunState`/`TriggerKind`/`ErrorClass`/`MaxRunRecordBytes` **全部留 cron 主包**。依赖方向单一:`cron/runstore` → `cron`(取类型/常量)。scheduler 持有 `*runstore.Store`,构造 `&cron.CronRun{}` 交给 `Store.Append`——类型在主包,双方都能引用,**无循环**。

> 验证无循环:cron 主包(scheduler)import `cron/runstore`(用 Store);`cron/runstore` import `cron`(用 CronRun)——这是**循环**!见 D2 如何破。

### 决策 D2:如何避免 cron ⇄ cron/runstore 循环?(关键)

D1 选 A 后暴露真问题:scheduler(在 `cron`)要用 `runstore.Store`,而 `runstore` 要用 `cron.CronRun`——**双向 import = 编译失败**。三种破法:

| 方案 | 做法 | 评价 |
|---|---|---|
| **D2-a. CronRun 等纯数据类型下沉到 leaf,两包都依赖它** | 新建 `internal/cron/cronrun`(或复用已有中立包 `internal/runtelemetry`)存放 `CronRun`/`CronRunSummary`/`RunState`/`TriggerKind`/`ErrorClass`/`MaxRunRecordBytes`;`cron` 和 `cron/runstore` 都单向依赖它 | **推荐** —— 经典 leaf-type 解法,与已落地的 `sessionkey`/`runtelemetry` 中立包同型 |
| D2-b. runstore 定义 Store 接口,cron 持接口,具体实现注入 | runstore 不 import cron,改用泛型/接口约束 CronRun;过度抽象 | ❌ 为拆包引入接口,偏离 move 性质 |
| D2-c. 不拆子包,只在主包内继续拆文件 | 放弃跨包收益,退回 move-only | 这是 §9 的 fallback |

**选 D2-a,且 leaf 必须是新建的 `internal/cron/cronrun`,不能并入 runtelemetry**(v2 修正 —— 见 §0 B4):

- `RunState`/`TriggerKind` 已是 `runtelemetry` 的 alias(job.go:175/191),这点 v1 正确。
- 但 `CronRun.ErrorClass` 字段的类型 `cron.ErrorClass`(job.go:212)含 **3 个 cron 独有竞态常量** `ErrClassRouterMissing`/`ErrClassPausedConcurrent`/`ErrClassDeletedConcurrent`(executeOpt CAS 窗口语义,#1323/#1410)。把 `ErrorClass` 并入 `runtelemetry` 会让这个本应中立、供 planner/system 未来共享的词汇表混入 cron 内部竞态——**污染 leaf 中立性**。`runtelemetry/state.go` 当前的 ErrClass 常量集**故意不含**这 3 个。
- 因此:新建 `internal/cron/cronrun` leaf,在其中定义 `ErrorClass` + 全部 12 个常量、`CronRun`/`CronRunSummary`/`MaxRunRecordBytes`/`IsValidID`;`RunState`/`TriggerKind` 继续 alias `runtelemetry`(cronrun import runtelemetry,单向)。

最终依赖(单向无环):

```
internal/runtelemetry  (RunState/TriggerKind 真身)         ← leaf
        ▲
internal/cron/cronrun  (CronRun/Summary/ErrorClass/常量/IsValidID/MaxRunRecordBytes)  ← leaf
        ▲                        ▲
internal/cron (scheduler) ──┐    │
                            ├────┤
internal/cron/runstore ─────┘    │
internal/cron ─→ internal/cron/runstore   (scheduler 持 *Store)
```

**这是本 RFC 最重的一笔,评审重点:为什么 ErrorClass 必须进新 leaf 而非 runtelemetry。**

### 决策 D3:`IsValidID` / `ErrCorruptRun` 的归属

- `IsValidID` 有 **11 处包外引用**(server ×1 + dashboard/cron ×10,见 §3.3)。它语义是纯 hex ID 校验,与持久化无关。**放 `cronrun` leaf 包**(随 D2 的 leaf 一并定义)。三种迁移策略:
  - **(b) 改全部 11 处 import 到 `cronrun.IsValidID`** —— 最干净,但触及 dashboard/cron 4 文件 + server 1 文件,Phase 1 范围扩大。
  - **(b') leaf 定义真身 + cron 主包留 `func IsValidID = cronrun.IsValidID` re-export** —— 11 处调用方零改动,零破坏;代价是 cron 主包多一行别名。**推荐**(迁移面最小,后续可渐进收口)。
  - v1 的 (a)"进 runstore 子包"已否决:IsValidID 与持久化无关,放 runstore 语义错位。
- `ErrCorruptRun` 有 **2 处包外引用**(dashboard/cron runs.go:166、transcript.go:364,v2 修正)。它是 runstore 的错误哨兵,**随 runstore 进子包导出为 `runstore.ErrCorruptRun`**,dashboard/cron 2 处改 import(或 cron 主包同样留 re-export)。

---

## 5. 目标布局与改造清单

### 5.1 包结构

```
internal/cron/
├── cronrun/                    # 新 leaf 包(D2-a,必须独立 —— 不并入 runtelemetry,见 §0 B4)
│   └── cronrun.go              # CronRun / CronRunSummary / ErrorClass + 12 常量 /
│                               #   MaxRunRecordBytes / IsValidID
│                               # import runtelemetry 取 RunState/TriggerKind alias(单向)
├── runstore/                   # 新子包:持久化 + 缓存 + trim
│   ├── store.go                # runStore→Store(导出), Append/List/Recent/Get/DeleteJob/TrimAllCtx
│   ├── cache.go                # recentCacheEntry + ring (可选二次拆)
│   ├── errors.go               # ErrCorruptRun(导出)
│   └── testing.go              # //go:build 测试 helper:NewDisabled / Root / KeepParams (替代戳私有字段)
├── scheduler*.go               # 持 *runstore.Store,构造 cronrun.CronRun
├── run.go                      # CronRun/Summary 下沉后清空;summary() 随类型进 cronrun
├── job.go                      # ErrorClass 下沉后移除该段;RunState/TriggerKind alias 留此或进 cronrun
└── ...                         # cron 主包对外留 re-export:IsValidID/ErrCorruptRun(D3-b'/D3)
```

> `cronrun` **必须**独立(B4):ErrorClass 的 3 个 cron 竞态常量不能进 runtelemetry。
> dashboard/cron 与 server 通过 cron 主包的 re-export 别名访问 IsValidID/ErrCorruptRun,迁移面降到最小。

### 5.2 导出清单(API 表面变更 —— 全部变更集中于此)

| 当前 | 拆包后 | 原因 |
|---|---|---|
| `runStore`(type) | `runstore.Store` | 跨包构造 |
| `newRunStore(...)` | `runstore.New(...)` | 跨包构造 |
| `trimAllCtx`(method) | `Store.TrimAllCtx` | scheduler.go:1195 调用 |
| `trimAll`(method) | `Store.TrimAll` 或保留 priv + 测试走 helper | 仅测试用,见 §5.3 |
| `ErrCorruptRun`(var) | `runstore.ErrCorruptRun` | 包内已用,导出供测试 |
| `IsValidID`(func) | `cronrun.IsValidID`(D3-b) | server 引用 |
| `CronRun`/`CronRunSummary` 等 | `cronrun.*`(D2-a) | 跨包共享 |
| `Append`/`List`/`Recent`/`Get`/`RecentSessionIDs`/`DeleteJob` | 不变(已 PUB) | — |
| ~20 个 priv 方法(cacheGet/warmCache/scanSortedRunDir…) | 不变(小写,内部自用) | — |

### 5.3 测试改造(§3.2 的 13 处私有访问)

测试当前在 `package cron`,拆包后 runstore 测试应迁到 `package runstore`(或 `runstore_test`)。两类处理:

- **戳私有字段**(`root`/`keepCount`/`keepWindow`/`disabled`/`enableTrimGC`,共 12 处):新增 `runstore/testing.go` 暴露受控构造器与 getter(`New(WithDisabled())`、`Store.Root()` test-only、或 `//go:build` test helper),测试改为走这些入口。**不导出生产 API 来迁就测试**。
- **戳私有方法 `trimAll`**(1 处):若 `TrimAll` 不需对 scheduler 导出(scheduler 只用 `TrimAllCtx`),则保留小写,测试改用同包(`package runstore`)直接调,或经 helper。

迁移后的 runstore 测试若仍需 cron 主包的 fixture,通过 `cronrun` leaf 拿类型即可,不反向 import cron。

### 5.4 顺带清理(D / §3.4)

`runstore.go:505` 的 `testing.Testing()` 锁探针:迁包时把 `assertJobLockHeld` 的 test-only 分支移到 `//go:build` test 文件或用构造期 flag 注入,**消除生产文件 import testing 的异味**。属顺手,不强制;若增加风险则单列后续项。

---

## 6. Phase 计划(每 phase 独立 build 绿)

| Phase | 动作 | 风险 |
|---|---|---|
| 0 | 基线:`go test -race ./internal/cron/...` 全绿存档;记录 runStore func 计数 | — |
| 1 | 建**独立** `internal/cron/cronrun` leaf 包,迁 `CronRun`/`Summary`/`summary()`/`ErrorClass`+12 常量/`MaxRunRecordBytes`/`IsValidID`(import runtelemetry 取 RunState/TriggerKind);cron 主包改内部引用 + 留 `IsValidID`/(稍后)`ErrCorruptRun` re-export 别名;`go build ./...` 绿(若走 D3-b' 别名,dashboard/server 零改动) | 中(leaf 破环,但 re-export 吸收下游) |
| 2 | 建 `internal/cron/runstore` 包,`git mv runstore.go`→`runstore/store.go`;`runStore`→`Store`、`newRunStore`→`New`、`trimAllCtx`→`TrimAllCtx`、`ErrCorruptRun` 导出;scheduler 改持 `*runstore.Store`;cron 主包 `ErrCorruptRun` re-export 别名(吸收 dashboard 2 处) | 中-高(核心一刀) |
| 3 | 迁 runstore 测试到 `package runstore`,加 `testing.go` helper,改 §3.2 的 13 处私有访问;清理 `testing.Testing()` 异味 | 中(13 触点) |
| 4 | 收尾:run.go/job.go 清理下沉后的空段;`go build ./...` + vet + `test -race ./...`(全仓,因 server+dashboard 受影响) | 低 |

> Phase 1 与 2 有依赖(2 依赖 leaf 包就位),**建议同一人连续做**。每 phase commit 独立可 build(吸取 process-split v3 "中间 commit 不可 build" 教训,跨 phase 依赖修复压在同一 commit 内)。
> **走 D3-b' re-export 别名是把 11+2 处下游改动降为 0 的关键** —— 否则 Phase 1/2 范围扩大到 dashboard/cron 5 文件 + server 1 文件。

---

## 7. 风险与缓解

| 风险 | 缓解 |
|---|---|
| **cron ⇄ runstore 循环**(最大风险) | D2-a 独立 cronrun leaf 破环;Phase 1 先落地 leaf 并 build 绿,再动 Phase 2 |
| **ErrorClass 误并入 runtelemetry 污染中立词汇**(§0 B4) | 强制 ErrorClass 进独立 cronrun leaf;runtelemetry 保持不含 cron 竞态常量 |
| `IsValidID`/`ErrCorruptRun` 漏改下游(11+2 处) | 走 D3-b' re-export 别名 → 下游零改动;否则 §3.3 已枚举全部引用点,build 即暴露 |
| 测试改造遗漏导致私有访问编译失败 | §3.2 已枚举全部 13 处;`package runstore` 编译即报 |
| 锁语义意外改变 | 非目标明确不动锁;`jobLocks`/`assertJobLockHeld` 整体迁移,`-race` ×N gate |
| `testing.Testing()` 清理引入回归 | 列为 Phase 3 可选项,若有风险拆出独立 follow-up |

---

## 8. 验收核对(可机械复现)

```bash
# (A) 无循环 import
go build ./... 2>&1 | grep -i 'import cycle' && echo FAIL || echo "no cycle"

# (B) runtelemetry 未被污染:不含 cron 竞态常量(B4 守卫)
grep -rn 'RouterMissing\|PausedConcurrent\|DeletedConcurrent' internal/runtelemetry/   # 期望空

# (C) 生产文件不再 import testing(注意冒号锚定文件名,见 §0 B1)
grep -rn '"testing"' internal/cron/runstore/*.go | grep -v '_test.go:'      # 期望空

# (D) IsValidID/ErrCorruptRun 下游:走 D3-b' 别名则 cron.X 引用仍可编译;
#     若走 (b) 全改则下游已指向 cronrun/runstore。任一方案 build 必须绿。
go build ./... && go vet ./... && go test -race ./internal/cron/... ./internal/server/... ./internal/dashboard/...
```

---

## 9. Fallback(若 D2 评审认为跨包不值)

若评审认为新增 leaf 包的协调成本 > 跨包收益,退化为 **D2-c:不拆子包,仅在 cron 主包内继续拆 runstore.go**。例如按"持久化 IO / ring 缓存 / trim+GC"把 1949 行拆成 `runstore.go` + `runstore_cache.go` + `runstore_trim.go` 三个**同包**文件——这是纯 move-only(零导出、零循环风险、零测试改造),与前几次拆分同构,可直接做。

**取舍**:子包方案得到真正的包边界(独立可测、依赖收敛),但需 leaf 包破环 + 13 处测试改造;同包拆文件零风险但只解决"文件过大",不改善依赖结构。**本 RFC 推荐子包方案,fallback 留作评审否决跨包时的保底。**

---

## 10. 附录 A:runStore 方法清单(导出决策逐条)

```
保持 PUB(已导出,scheduler 用): Append, DeleteJob, Get, List, Recent, RecentSessionIDs, WriteFailedTotals
需导出(scheduler 生产调用):    trimAllCtx → TrimAllCtx
仅测试用(走 helper,不为测试导出生产API): trimAll, + 字段 root/keepCount/keepWindow/disabled/enableTrimGC
保持 priv(runStore 内部自用):  assertJobLockHeld, cacheGet, cacheGetBefore, cacheHeadPush,
  cacheInvalidate, cacheTrimAfterDisk, diskListNewestFirst, ensureJobDir, jobLock,
  parseRunBytes, parseRunFromFile, readRun, readRunNoLstat, scanSortedRunDir,
  skipAppendTrim, trimJobLocked, trimJobUnderLock, trimSkipFromCache, warmCache, warmCacheLocked
```

对外新增导出面:
- `runstore` 子包:`Store`(原 runStore)、`New`(原 newRunStore)、`TrimAllCtx`(原 trimAllCtx)、`ErrCorruptRun`
- `cronrun` leaf 包:`CronRun`/`CronRunSummary`/`ErrorClass`+12 常量/`IsValidID`/`MaxRunRecordBytes`
- cron 主包 re-export 别名(D3-b'/D3,吸收下游 11+2 处):`IsValidID = cronrun.IsValidID`、`ErrCorruptRun = runstore.ErrCorruptRun`
