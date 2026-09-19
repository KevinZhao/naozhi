# RFC: cron 的作业表提取、sandbox 子包化与终结路径收口

- Issue: #2711（承接 #2546 Epic H 未完成的前置）
- 状态: 草案 v2（v1 经三路对抗评审后重写；评审改掉了方案本体，不只是细节）
- 影响面: `internal/cron`；无 wire 协议变化、无配置变化。dashboard 只改一处文案（步 1）

## 0. v1 → v2 改了什么（评审结论摘要）

v1 提了一个"把四个字段和一把锁塞进 `jobTable`、保留六个锁序测试一行不改、然后 sandbox 就能提子包"的计划。三路评审各自独立否掉了其中一块：

| v1 的说法 | 评审发现 | 处理 |
|---|---|---|
| 「6 个锁序测试一行不改」 | **全部六个都直接操作 `s.mu` / 索引 `s.jobs[`**；全仓 415 处 `s.mu.` 分布在 69 个测试文件，60 个测试文件索引 `.jobs[` | 改判：步 2 的**主要成本就是测试改写**，见 §5.2；两个"外部夹逼"型测试必须**重新表达**而不是保留 |
| 「`withRLock(view)` 让锁不外泄」 | 写侧的 `lockedJobOp` 在**写锁内**跑调用方闭包（`deleteJobLocked`/`pauseJobLocked`/`resumeJobLocked`），比读侧更危险；`get(id) *Job` 交出活指针，"外部碰不到 map"是空话 | 重设 API：**不用闭包**，改数据结构描述变更 + 值拷贝快照（§3） |
| 「锁序不变」 | 我列的规则 3（持 `s.mu` 不得调 runstore）**与代码里的注释相反**：`scheduler_finish.go:99` 写的是 `s.mu > jobLock > entry.mu`，即 s.mu 是最外层 | 规则表重写（§4） |
| （未提及） | **`s.mu` 持锁跨 robfig/cron 的 channel 会合**：`registerJob`（内部 `s.cron.AddFunc`）在 `AddJob` 与 `resumeJobLocked` 的写锁内被调用 | 列为开工前必须处置的既有问题（§4.1），不能被"锁序不变"冻结 |
| 「`snapshotJobsForSaveLocked` 迁进 jobTable，只是池的取用点挪一下」 | `saveSeq.Add(1)` **在临界区内**，注释写明"Monotonic seq captured under s.mu total-orders marshals with the state they represent" | 改为 jobTable 返回值拷贝快照 + seq 留在 Scheduler（§3.2） |
| 「jobTable + runGate 解开 sandbox 子包化的前置」 | sandbox 四个文件对包内的依赖约 **26 项**，jobTable 只覆盖 6 项；`runsnapshot.go`、`tuning.go` 两处常量也是 sandbox-only 但没在搬迁集里 | 依赖清单进 §6；方案改为**只反转 `executeSandbox`**，`sandbox_pending.go` 留在 cron |
| 「引用 sandbox 的顶层文件 ≤3」 | 23 处里 16 处**不能搬**（`SandboxRunner` 是 wireup 注入的 dep、`CronRun.SandboxMeta` 是磁盘格式、7 个导出错误变量被 `internal/dashboard/cron` import…）；现实下限 ~14–16 | 判据换成"没有 `cron/sandbox` 之外的文件引用 `executeSandbox` / `sandboxPending*` / `sandboxAttention` / `SandboxRunSnapshot`"（§6.3） |
| 「移到 `executeAcquired` 分支后本地路径 byte-identical」 | sandbox 分支要用 `cleanText` 与 `opts.Model`，两者由 `execPrepareSpawn` 内的 `resolveAgent`/`cloneAgentOpts` 产出，且在**单次** `s.configMaps()` Load 之后（注释明写"Load once so both reads see the same generation"）。上移分支会拆开这次 Load 或让 sandbox 丢掉 agent-command 剥离与 model 解析 | 放弃"上移到 executeAcquired"，改为在原位反转返回值（§6.4） |
| §8.3「`skipPersist` 必须在 `runOutcome` 里有对应物」 | **已经有了**：`run_ctx.go` 早就存在 `runCtx`(:37) / `runOutcome`(:67) / `finishRunFor`(:98)，19 个字段零丢失映射，`skipPersist` 在 :79 | 删掉该节；步 5 的真实剩余工作缩小到"删一层 22 行适配 + 改两个非执行调用点"（§7） |
| （issue 的验收）「`finishRun` 调用点 = 1」 | 三个生产调用点形状本质不同（synthetic skip 无 snap/lg/inflight、restart reconciler 传 `orphanJobSnapshot`）；强行合一 = 用零值表达"别做 X"，正是 `run_ctx.go:28-33` 已经否掉的做法。且另有**两条终结路径根本不经过 `finishRun`**，"=1"低估了终结面 | 判据换成 §7.3 |

我自己复核了其中四条最要命的（`run_ctx.go` 确实已存在且 :18 的"twenty-one fields"是过期注释；`ensure_stub_lockorder_test.go` 确实按设计从外部持 `s.mu.RLock`；`registerJob` 确实在写锁内被调且内部 `s.cron.AddFunc`；`scheduler_finish.go:99` 确实写的是 `s.mu > jobLock`）。

## 1. 要解决什么

两件被反复推迟的事——`cron/sandbox` 提子包（#2546 放弃）、`finishArgs` 19 字段收口（#2691 只嵌入未替代）——都卡在"想读作业表就得先拿到半个 `Scheduler`"。实测（master 998cacee）：

| 事实 | 数字 |
|---|---|
| `s.mu.Lock()` / `RLock()` 生产站点 | 40 |
| `s.mu.` 测试引用 / 涉及文件 | **415 / 69** |
| 索引 `.jobs[` 的测试文件 | **60** |
| `jobGateLock(` / `jobInflight(` 生产调用 | 9 |
| 引用 sandbox/placement 的 cron 顶层非测试文件 | 23（#2546 时 21） |
| `finishArgs` 字段 | 19（`run_ctx.go:18` 的注释说 21，过期） |
| `finishRun` 生产调用点 | 3（`run_ctx.go:99`、`scheduler_finish.go:446`、`sandbox_pending.go:514`） |
| 不经过 `finishRun` 的终结路径 | 2（`finishOrphanRun`、`dispatchReplay`） |

## 2. 不做什么

**a. 不拆 `s.mu` 成更细的锁。** 四个字段是同一不变量的四个视图。
**b. 不把作业表做成 interface。** 唯一实现 + 唯一调用方，只增跳转（#2551/#2561 的 typed-nil 代价）。
**c. 不引入 `Update(func(tx))` 事务壳。** `internal/session` 的 G3（#2667）评估过同一提案并放弃：绝大多数访问是读，读不破坏不变量。cron 复制的是它的另一半结论——**不变量用测试断言，而不是用注释描述**。
**d. 不用任何"把锁交给调用方闭包"的 API。** 见 §0 与 §3。

## 3. `jobTable`：数据描述 + 值拷贝，不交锁

```go
// jobTable owns the job registry and its two derived indices, and its own lock.
// The API hands out DATA, never the lock and never a live *Job: every escape
// hatch that exists today (a closure under the write lock, a live pointer past
// the RLock) is a way for the invariant to be broken from outside.
type jobTable struct {
	mu           sync.RWMutex
	jobs         map[string]*Job
	chatJobCount map[chatJobKey]int
	jobsByChat   map[chatJobKey][]*Job
	sortedJobIDs []string
}
```

### 3.1 读
`exists(id) bool`、`count() int`、`countForChat(k) int`、`snapshot(id) (jobSnapshot, bool)`（**值**，不是 `*Job`）、`snapshotForChat(k) []jobSnapshot`、`ids() []string`。

已有的 `snapshotJobLocked` 正是这个形状，所以"返回值拷贝"不是新概念，是把现存做法变成唯一做法。

### 3.2 写：用数据结构，不用闭包

```go
// jobMutation describes a change to one job as DATA. The composite write paths
// today pass a closure that runs under the write lock (lockedJobOp); a struct
// cannot capture a *Scheduler, so it cannot re-enter the lock.
type jobMutation struct {
	SetPaused    *bool
	SetPrompt    *string
	SetSchedule  *string
	// … 逐个按现有 *Locked 函数的实际改动补，不留 catch-all
}

// apply mutates one job and returns the persist snapshot taken under the SAME
// lock hold, because recordTerminalResult needs exactly that atomicity
// (mutate one job + snapshot all jobs).
func (t *jobTable) apply(id string, m jobMutation) (jobsSnapshot, bool)
```

`saveSeq` 留在 `Scheduler`：它是"marshal 与状态的全序"，不属于表。调用方在同一次 `apply` 返回后自增，顺序不变。

### 3.3 `runGate`
`jobGates [N]sync.Mutex` + `runningJobs sync.Map` 提成 `runGate`（守**执行**，不是注册表）。评审确认"两把锁当前从不同时持有"是**真的**（`scheduler_run.go:429`/`sandbox_replay.go:160` 都在读表前解 gate），但也指出**没有任何东西保证它继续为真**——步 4 之后 gate 句柄会跨包传出去。§8 给出机器检查方案。

## 4. 锁序（重写）

代码里真实成立的（`scheduler_finish.go:99` + 现有测试反推）：

1. `s.mu` 是**最外层**：`s.mu > jobLock(runstore) > entry.mu`。持 `s.mu` 调 runstore 是**允许**的（v1 写反了）。
2. `jobGate` 是**叶子**（`job_gate.go:19` 明写 "Pure leaf lock"）：持 gate 可以取 `s.mu`，反之不行。
3. 持 `s.mu` 不得发 WS 广播 / 通知（`snapshot_jobs_offlock_test.go`）。

### 4.1 开工前必须处置的既有违规

**`s.mu` 持锁跨 robfig/cron 的 channel 会合。** `registerJob`（`scheduler_jobs.go:812`，内部 `s.cron.AddFunc` :817）在写锁内被调用：`AddJob` :133、`resumeJobLocked` :297、:558、:571。`AddFunc` 在 cron 运行时会向无缓冲 channel 投递并等 run loop 接收。

现状为何没炸：robfig 的 run loop 把 job 派到独立 goroutine（`go startJob`），所以它能及时回到 select 排空 channel。**但这条依赖 robfig 的内部实现**，且与 `delete_lockorder_test.go:12-15`、`pause_lockorder_test.go:9-13` 存在的理由（禁止持锁跨外部调用）直接冲突；`updatejob_lockorder_test.go:34-38` 甚至**承认**第 552 行就是这么做的、而退役的锚点测试当时还是绿的。

**决定点（请 owner 拍）**：(a) 先把 `registerJob` 移出锁（需要处理"注册成功但入表失败"的回滚序）；(b) 显式把"持 s.mu 调 cron 调度器"写进允许清单并加测试钉住。**在二者择一之前不开步 2**，否则新对象会把这条冻进去。

另一条同类：`sandbox_replay.go:44-59` / `scheduler_jobs.go:758-785` 在一次 RLock 内读 `stopped` + `jobs[id]` + `triggerWG.Add(1)`。评审指出这个复合保证**已经是幻觉**（`stopWithCtx` 从不取 `s.mu`，所以 RLock 与 `triggerWG.Wait` 之间没有任何顺序）。步 2 之前要么修、要么删掉那句注释——不能让重构悄悄丢掉一个本来就不存在的保证。

## 5. 迁移步骤

| 步 | 内容 | 风险 | 前置 |
|---|---|---|---|
| 1 | `cron_view.js` 补 `interrupted` 中文标签 + Playwright 断言 | 极低 | 无 |
| 0' | 处置 §4.1 两条既有违规（各一个小 PR） | 低–中 | 无 |
| 2 | 引入 `jobTable`（数据 API）+ 测试改写 | **高**（见 §5.2） | 0' |
| 3 | 引入 `runGate` | 低 | 2 |
| 4 | `executeSandbox` 反转 + `cron/sandbox` 子包（部分） | 中 | 3 |
| 5 | 删 `finishRunFor` 适配层 | 低（比 issue 估的小得多） | 无（可与 2 并行） |

#2712（adoption Phase 1）等步 3 完成再开始。

### 5.2 步 2 的真实成本

不是"40 个生产站点改调方法"，而是**415 处测试引用 / 69 个测试文件**。其中两类需要判断：

- **纯读 `s.jobs[id]` 做断言**（多数）：换成 `tbl.snapshot(id)`，机械。
- **外部夹逼型**（`ensure_stub_lockorder_test.go`、`scheduler_finish_deadlock_test.go`）：它们的方法就是从测试里持/放 `s.mu` 来证明被测代码的锁行为。自持锁一旦生效，这两个测试**不是失败，而是失去观察能力**（评审的原话：passes because it can no longer see the thing it was checking）。

处理方式：给 `jobTable` 加 `export_test.go` 里的测试专用夹逼口（`lockForTest()` / `rlockForTest()`），生产代码看不到。这保住了观察能力，也不给生产代码留后门。**这个决定必须写在测试文件的注释里**，否则下一个人会以为那是个 API。

`scheduler_finish_deadlock_test.go` 还有一个更麻烦的点：它注入 panic 的 `marshalJobs` 只有在 `snapshotJobsForSaveLocked` **在锁内被调用**时才会命中锁内 panic。快照挪进 `jobTable` 后，注入点不再在任何 Scheduler 可见的锁内 → 测试变空过。所以步 2 必须**同时重写这个测试的注入点**（改注入 `jobTable.apply` 的快照阶段），否则它会绿着失效。

## 6. sandbox 子包化

### 6.1 依赖清单（评审整理，逐条核过）

`sandbox.go` 需要：`s.sandbox`、`s.execTimeout`、`s.stopCtx`、`s.storePath`、`s.stateSubtree`/`mkdirStateSubtree`、`s.now`、`s.observeSuccessLatency`、`s.finishRunFor`、`s.deliverNotice`+`formatCronNotice`+`localizeNotice`、`s.writeSandboxSnapshot`、`sanitiseRunErrMsg`、`s.mu`/`s.jobs`。
`sandbox_pending.go` 需要：`s.sandboxPendingMu`/`Index`、`s.stopCtx`、`s.sandbox`、`s.mu`/`s.jobs`、`s.emitRunStarted`、`s.bumpRunStateMetrics`、`s.finishRun`。
`sandbox_replay.go` 需要：`s.mu`+`s.stopped`+`s.triggerWG` 的复合持有、`jobGateLock`+`jobInflight`、`s.snapshotJob`、`s.resolveNotifyTarget`、`emitRunStarted/Ended`、`runScaffold`+`runFinalizer`、`generateRunID`。
`sandbox_attention.go` 需要：`stateSubtree`、`mkdirStateSubtree`、`s.now` —— **唯一真能整体搬走的文件**。

合计约 26 项，`jobTable` 覆盖 6 项。**"提了作业表 sandbox 就能搬"是错的。**

漏在搬迁集外的 sandbox-only 代码：`runsnapshot.go` 全部、`tuning.go` 的 `sandboxStopTimeout`/`sandboxReconcileWorkers`、`scheduler_finish.go:167` 的 sandbox 分支。

### 6.2 `finishRun` 缺口的三个选项与选择

- (a) **下沉 `finishRun`**：它要 `s.runStore`/`appendRun`/`appendLedger`/`recordTerminalResult`/`emitRunEnded`/`bumpRunStateMetrics`/`invalidateKnownSessionsCache`/`removeRunInflightMarker`/`jobStillExists`/`s.mu`/`s.json`/`finishRunPreAppendHook` —— 等于把整个 Scheduler 减去 router 一起搬。**不是搬迁，是重写。否决。**
- (b) **回调注入**：`runCtx`/`runOutcome`/`RunState`/`ErrorClass`/`jobSnapshot`/`runFinalizer`/`runInflight` 全都得先进共享包，而 `runCtx.job *Job` 还会把 `Job` 拖下去。**代价过大。**
- (c) **反转返回值**（选这个）：`executeSandbox` 改成返回 `(runOutcome, notice)`，由原调用方在 cron 顶层收尾。`finishSandboxRunWith`（`sandbox.go:359`）已经是单一漏斗，反转成本集中在一处。代价：`reconcileSandboxPending` 从 `Start()` 起、没有调用者栈可返回，所以它**留在 cron 顶层**——`sandbox_pending.go` 不搬。

### 6.3 判据换掉

`引用 sandbox 的顶层文件 ≤3` **不可达**（16 处不能搬，现实下限 ~14–16），且它数的是注释与磁盘格式而不是耦合。换成：

> `cron/sandbox` 之外没有任何文件引用 `executeSandbox`、`sandboxPending*`、`sandboxAttention`、`SandboxRunSnapshot*`。

### 6.4 不上移分支

`executeSandbox` 的入参 `cleanText` / `opts.Model` 产自 `execPrepareSpawn` 内 `resolveAgent`/`cloneAgentOpts`，且在**单次** `s.configMaps()` Load 之后。上移到 `executeAcquired` 会拆开这次 Load（两次读可能看到不同 generation）或让 sandbox 丢掉 agent-command 剥离与 model 解析。**分支留在原位，只把它的产物从"侧出口 + 内部 finish"改成"返回值"。** issue 里"`execPrepareSpawn` 内无 sandbox 分支"这条验收随之作废，理由记在此。

### 6.5 其它必须先处理的共享状态

包级 `sandboxEventsSem`（`sandbox.go:562`，cap 8）是**进程级**而非 per-Scheduler：搬包后它就不再节流 cron 这边的任何东西。7 个导出错误变量被 `internal/dashboard/cron` 以 `cronpkg.*` import（`attention.go:127`、`update.go:204`、`runs.go:229`），`consumer.go:53-58` 的 Scheduler 方法签名不能变 → cron 需要 6 个转发壳。测试钩子 `WriteSandboxAttentionForTest`、`WriteSandboxSnapshotForTest`、`finishRunPreAppendHook` 会变成跨包。

## 7. `finishArgs`：真实剩余工作

### 7.1 已经完成的部分
`run_ctx.go` 已有 `runCtx`(10 字段) + `runOutcome`(11 字段)，`finishRunFor`(:98) 零丢失映射全部 19 个 `finishArgs` 字段，`skipPersist` 有归宿(:79)。

### 7.2 剩下的与新发现的代价
- 删 `finishRunFor` 这层 22 行适配、改两个非执行调用点。
- **反向代价**：到达 `finishRun` 的字段从 19 变成 21，其中 `notifyTo`/`key`/`lg`/`inflight` 四个在 `finishRun` 里根本不读。这是**命名收益，不是耦合收益**，RFC 如实写明。
- `appendLedger(a finishArgs)`（`cost_ledger.go:13`）是 `finishArgs` 的**第二个消费者**，签名一起变。
- `skipPersist` 实际影响**四个**子系统（sanitise 管线、`recordTerminalResult`、`runs/` 追加、metrics 门），`sandbox` 布尔另外扇出两个 metric 桶，`finalizer == nil` vs 空非 nil 是"用 nil 表达语义"的第三处——三者在收口后都要有对应物与测试。

### 7.3 判据换掉
`finishRun` 调用点 = 1 **不可达且是错的度量**（三个形状本质不同；另有两条终结路径不经过它）。换成：

> (1) `finishArgs` 类型删除；(2) 每条终结路径的入口在 `grep` 下可枚举且 ≤5；(3) `skipPersist` / `sandbox` / `finalizer==nil` 三个语义开关各有一个直接断言它的测试。

### 7.4 已知覆盖洞（步 5 开工前先补）
`cost_ledger_test.go:159` 直接调 `appendLedger(finishArgs{...})` **绕过 `finishRun`**，所以从组合里丢掉 `workDir` 会让 ledger 的 `Workspace` 变空而测试全绿。`fresh` 没有任何测试断言 `CronRun.Fresh == true`。这两个洞要先补测试，再动收口。

## 8. 怎么证明没做坏

1. **声明多重集比对**（go/parser，包级）：步 2/3/4 缺失必须为 0，新增逐个可解释。
2. **锁序测试**：不是"一行不改"，而是"**观察能力不降**"——两个夹逼测试改用 `export_test.go` 的测试专用锁口；`scheduler_finish_deadlock_test.go` 的 panic 注入点跟着快照移动。每个改动过的锁序测试都要给出"改之前它观察什么、改之后仍观察什么"的一句话说明。
3. `go test -race -count=20 ./internal/cron`（现约 29s/轮）。
4. **每步一个变异实验**，且必须能检出：漏索引更新（`apply` 不写 `jobsByChat`）、gate 与表同时持有（步 3/4）、`skipPersist` 丢位（步 5）。v1 的变异计划漏了步 5，补上。
5. **步 3/4 之后 gate 不再嵌套的机器检查**：`tools/lint-server-handlers` 是文本匹配，看不见闭包捕获与跨包持有；需要一个 `go/analysis` pass（"`jobTable` 的方法体内出现 `runGate` 调用"即报）。**这条不做，就只能靠自觉——RFC 如实写明，由 owner 决定要不要为此写 analyzer。**

## 9. 仍然悬空、需要 owner 决定的

1. §4.1 的 `registerJob` 持锁跨 cron channel：先修，还是写进允许清单？（**阻塞步 2**）
2. `sandbox_replay.go` 那个已经是幻觉的复合 RLock 保证：修还是删注释？（**阻塞步 2**）
3. §8.5 的 analyzer 要不要写？不写就接受"gate 不与表同时持有"在步 4 后没有机器保障。
4. `sandboxEventsSem` 的进程级语义在子包化后如何保留（移进子包 = 每个 Scheduler 各一份，是行为变化）。

## 10. v2 → v3：步 2 开工时的实测修正

v2 之后 owner 定了两件事：#2740（`s.mu` 持锁跨 robfig）**不是外部阻塞而是步 2 的子步**，先移出再提表；#2741 判为**删注释即可**。开工时逐条核实 v2 的前提，有五处需要改：

| v2 的说法 | 实测 | 处理 |
|---|---|---|
| §3.2「`saveSeq` 留在 `Scheduler`：它是 marshal 与状态的全序，不属于表」 | **反了**。正因为它是"snapshot 与它代表的状态的全序"，`saveSeq.Add(1)` 必须与快照在**同一次临界区**内（`scheduler_persist.go:146`、`:215` 两处都是）。锁进了表而计数器留在 Scheduler，两个快照就可能在一把不再序列化它们的锁下各取一个 seq —— 全序静默断掉 | `saveSeq` **随锁一起进 jobTable** |
| §4.1「`s.mu` 持锁跨 robfig 是既有违规，靠 robfig 内部实现才没炸」 | 更精确：`run()`（cron.go:263-279）**从不取 `runningMu`**，`startJob`（:308-314）把回调交给新 goroutine，所以 `s.mu → runningMu` **不可能成环**。代价是 **reader latency**，不是死锁。仓库 `scheduler_jobs.go:540` 已推出此结论，而同文件 :471 写了相反的话 —— 已在 #2756 修正 | 问题陈述改写；`s.cron.Entry()` 的**往返**会合已在 #2756 消除 |
| §4.1 列 `registerJob` 的违规站点含 `Start()` | `Start()` 内那处**不违规**：`s.cron.Start()` 在其后，`!c.running` 时 `Schedule` 走 `c.entries = append(...)`，**没有 channel 会合**（cron.go:168-172） | 违规站点是 **5 处**（`AddJob` 1 + `resumeJobLocked` 的三个调用者 + `UpdateJob` 2），不是 6 处 |
| §4.1「`sandbox_replay` 那个复合 RLock 保证已是幻觉，步 2 前要么修要么删注释」 | 核实确认：`stopWithCtx` 全函数体内 `s.mu.` 出现 **0 次**；且 `stopped` 是 `atomic.Bool`，在 RLock 下读它本就多余；同一文件 :119 自己写着"the earlier stopped check is stale by now"并二次检查 —— **在同一文件里自我否证** | 那把锁**移除**（它什么都不守，只让 registry 读者排队），注释改成陈述真实残留风险：`Add` 与 `Wait` 不原子，落在 `Stop` 契约已声明的"intentional orphans"桶里 |
| §5.2「步 2 的主要成本是 415 处测试引用」 | 成立，但可以**不在同一个 PR 里付**：把 `jobTable` **嵌入** `Scheduler`（而非命名字段），`s.jobs` / `s.mu` 因字段提升照常解析，560 行测试**零改动**编译通过。真正需要改的只有**结构体字面量**（Go 不允许对提升字段用字面量键）—— 实测全仓仅 **2 个文件 5 处** | 步 2 拆成 2a/2b（下） |

### 10.1 步 2 拆成 2a / 2b / 2c

- **2a（本次）**：四个字段 + 锁 + `saveSeq` 收进 `jobTable`，**嵌入** `Scheduler`；定义值语义读 API（`exists` / `count` / `countForChat` / `ids` / `liveness` / `lastSessionID`）；把**自己取锁只为一次读**的生产站点转过去；#2741 落地。零测试语义改动。
- **2b → 改序为 2d（在 2c 之后）**：74 个测试文件迁到 `export_test.go` 的测试专用口，然后**去掉嵌入**（`jobTable` 变命名字段 `tbl`）—— 封闭性只在这一步才真正被强制，而不是仅被"提供"。两个外部夹逼测试按 §8.2 的口径重新表达。改序理由：它买的是封闭性强制（hygiene），而 2c 需要的只是行为正确；预迁 63 处插入点是凭猜测付费，让 2c 自己的编译/测试失败精确指出哪些测试建了不可能的状态更便宜。
- **2c（已完成）**：robfig 单向发送已全部移出 `s.mu` —— 每个 post-start 写者走 plan（锁内，纯）→ commit（锁外）→ apply（短锁）三段，`entryMu` 覆盖**所有** entry 生命周期写者（add / pause / resume / update / delete），把窗口对并发写者互斥掉。`registerJob` 只剩 `Start()` 一个调用者（robfig 未 running，`Schedule` 是 append 不是会合），契约写进其 doc。`entryState` 三态最终**没有做成显式字段**：#2760 证明了中间态必须被处理，但互斥（entryMu）比三态枚举更小且可立即验证 —— "pending" 由"持有 entryMu 的那个写者"这一事实表示，而不是由数据表示。锁纪律的机器断言是 `TestEntryCommitNeverUnderRegistryLock`（`cronCommitHook` + `TryLock`），它是 §8.5 那个 go/analysis 提案的运行时替代。副产品：把 commit 推迟到 persist 之后，让 #1226/#1810 的整套"注册后回滚"机制（entryID 快照、出锁 Remove、rollbackEntryID 交接）失去存在理由并被删除 —— persist 失败时 entry 根本还没注册。

**为什么把 #2740 的剩余部分放到 2c 而不是 2a 之前**（owner 已采纳）：那 5 处每处都嵌在自己的 persist/rollback 顺序里（`AddJob` 的 `rollbackEntryID`、`SetJobPrompt` 的 `pauseRollbackCleanup`、`UpdateJob` 的"恢复 `prevCachedSched` 而非置 nil"）。`AddJob` 尤其危险：job 已进 `s.jobs` 而 `entryID` 仍为 0 时，并发的 `UpdateJob` 会看到 0 → 跳过 `Remove` → 注册第二个 entry → **同一 job 双触发**。关掉它需要"注册中"这个第三态，而那本就该是 `jobTable` 的 API。

### 10.2 2a 之后剩下的唯一生产逃逸口

`executeJobIDIfLive`（`scheduler_inflight.go`）必须让**活的 `*Job` 指针**逃出临界区，因为 `executeOpt` 收的是 `*Job`。这是 2a 之后生产代码里唯一一处，已在原地写明。关掉它 = 给 `executeOpt` 一个快照，属 run 管线的改动而非 registry 的，留给步 5 或独立处理。
