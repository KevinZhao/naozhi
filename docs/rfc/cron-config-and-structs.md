# RFC: Cron Config 与 Job/Scheduler 结构形态整治（合并）

> **状态**: Draft v1（待评审）
> **作者**: naozhi team (cron-cr)
> **创建**: 2026-06-04
> **范围**: 收敛 `internal/cron` 五个长期挂账的结构/配置/拆分诉求为一份共享 RFC，确定哪些本轮只评审、哪些可独立落地，并钉死所有 load-bearing 不变量与 pin 测试。
> **关联 issue**: #776, #764, #1282, #1278, #837
> **关联代码**:
> - `internal/cron/scheduler.go`（`SchedulerConfig` line 161；`Scheduler` struct line 312；`NewScheduler` line 1081）
> - `internal/cron/job.go`（`Job` struct line 121-229；`JobInit`/`NewJobFull` line 51-107；`JobRunCounters` line 423）
> - `internal/cron/agent_opts.go`（`AgentOpts`/`Session`/`SessionStatus`/`InterruptOutcome` 全文，cron-local 并行类型）
> - `internal/cron/scheduler_finish.go`（`finishArgs` line 130-172；`finishRun` line 184）
> - `internal/cron/runstore.go`（`runStore.Append`，CronRun 历史落盘）
> - `internal/cron/{scheduler.go, scheduler_jobs.go, scheduler_run.go}`（>1300 行的三个文件，#1282）
> - `internal/wireup/schedulers.go` line 191（唯一生产 `SchedulerConfig{}` 构造点）
> - `internal/wireup/cron_router_adapter.go` line 38/62（`InterruptOutcome` 编译期 + init() 双重 ordinal pin）

---

## 1. Background & problem

本 RFC 把五个独立 issue 合并处理，因为它们都触及同一组 `internal/cron` 的“结构形态”问题，单独动任何一个都会与其它产生 diff 冲突或不变量耦合。逐项陈述可复现症状与现状证据：

### #776 — `SchedulerConfig` 25 字段，呼吁 functional options
- 现状：`internal/cron/scheduler.go:161` 起的 `SchedulerConfig` 有约 22 个导出字段（Router/Platforms/Agents/AgentCommands/Telemetry/StorePath/MaxJobs/MaxJobsPerChat/ExecTimeout/Location/NotifyDefault/ParentCtx/AllowedRoot/JitterMax/RunsKeepCount/RunsKeepWindow/SlowThreshold/AllowNilRouter…）。
- “症状”性质：这**不是运行时 bug**。`NewScheduler(cfg SchedulerConfig)` 按值接收、不保留 cfg（line 1081，R245-GO-6 已钉死 ParentCtx 只 derive-only）。所有“可选”字段已是 zero-value-fallback 的 idiomatic optional（`MaxJobsPerChat<=0` → default、`SlowThreshold<=0` → default、`AllowedRoot==""` → 关闭检查）。
- 真正的问题是 **API 约定未被显式批准**：struct-config 与 functional-options 在 Go 里是两种合法风格，本仓库其它构造器（见 `~/.claude/rules/golang/patterns.md` 的 `Option func(*Server)` 范式）未统一。25 字段的 struct literal 在阅读时无法区分“必填 vs 可选”，且新增字段时无法在编译期强制调用方关注。
- **规模事实（修正 triage 的估计）**：`grep` 全仓库 `SchedulerConfig{` 共 **213 处**，但其中**生产代码仅 1 处**（`internal/wireup/schedulers.go:191`）；其余 212 处全部在 `_test.go`（`internal/cron/*_test.go` 约 70 个文件 + `internal/dashboard/cron/*_test.go` + `internal/dispatch`/`internal/server` 测试）。triage 所说“~150+ call sites”在量级上正确，但**几乎全是测试夹具**——这直接决定了 functional-options 迁移的成本是“改 200+ 测试夹具”，而非“改 200 个生产路径”。

### #764 — `Job` god-struct 混合三类关注点
- 现状：`internal/cron/job.go:121-229` 的 `Job` 把三类语义混在一个结构里：
  1. **wire schema / 操作员意图**（持久化进 `cron_jobs.json`）：`ID/Schedule/Prompt/Title/WorkDir/Backend/Notify*/FreshContext/Paused`。
  2. **runtime-only 状态**（`json:"-"` 等价，注释明确 “not persisted”）：`entryID`、`cachedPeriod`、`cachedSched`（line 208/218/228）。
  3. **last-run 结果 / IM 元数据**（持久化，但语义上是“执行产物”而非“配置”）：`LastResult/LastRunAt/LastError/LastSessionID/LastErrorClass/RunCounters` + `Platform/ChatID/ChatType/CreatedBy`。
- 问题：(a) 每个字段都带逐字段 `cron_jobs.json` 向后兼容契约（line 134/143/195/203 的注释），任何拆分都必须保留 JSON 反序列化形态；(b) runtime 字段（cachedSched 等）通过 `registerJob` 写入、`HasMissedScheduleCached` 读取，与持久字段共享同一 `*Job` 指针在 `s.mu` 下被并发读写——拆结构会改变锁覆盖面；(c) `JobInit`/`NewJobFull`（line 51-107）已经是“收敛构造点”的部分尝试，但只覆盖创建期字段，last-run 字段仍由 `finishRun` 直接 mutate。

### #1282 — 四个 cron 文件 > 1300 行
- 现状（实测）：`scheduler.go` 2084、`scheduler_jobs.go` 2216、`scheduler_run.go` 2007、`runstore.go` 2464 行。均超出 `~/.claude/rules/common/coding-style.md` 的 “800 max”。
- 问题：纯机械可拆，但**高 churn**——cron 是全仓库测试最密集的包（`internal/cron/` 下约 160 个 `_test.go`），任何文件移动都会与并行分支（worktrees 里可见 7+ 条 cron 分支）产生大面积 rebase 冲突。

### #1278 — cron-local 并行类型 + ordinal init pin
- 现状：`internal/cron/agent_opts.go` 定义 `AgentOpts`/`Session`/`SessionStatus`/`InterruptOutcome`/`SendResult`，是 `internal/session` 对应类型的**手工镜像**，用于切断 cron→session 反向 import 边（R20260527122801-ARCH-1 已消除最后一条反向 import）。
- load-bearing 不变量：`SessionStatus`（3 值）与 `InterruptOutcome`（5 值）依赖 **iota ordinal 与 session 侧逐值对齐**，因为 `cmd`/`wireup` 适配器用数值 cast `cron.InterruptOutcome(c.s.InterruptViaControl())` 翻译。`internal/wireup/cron_router_adapter.go:38` 有编译期 pin（`uint(int(cron.X)-int(session.X))` 发散即下溢），line 62 有 `init()` panic 双重保护。
- 问题：**这不是“待修 bug”，而是“待文档化的契约”**。issue 的诉求是把“为什么有两套并行类型、改哪边要同步哪边、pin 在哪”写成可评审的设计记录，避免未来有人“简化”掉镜像类型或删掉 pin。当前知识只散落在 agent_opts.go 与 adapter 的注释里。

### #837 — `finishRun` 三写 Saga
- 现状：`scheduler_finish.go:184` 的 `finishRun` 在一次终态里做三类写：(1) `recordTerminalResult` 写 `cron_jobs.json` 的 Job 字段并返回 `jobPersistOK`（line 221）；(2) 仅当 `jobPersistOK==true` 才 `runStore.Append` 写 `runs/<jobID>/<runID>.json`（line 272）；(3) metrics bump + WS broadcast。
- **已有的保护**：`jobPersistOK` gate（line 219-258 的长注释，R249-ARCH-28 / #992）已把 divergence 钳到唯一安全方向（cron_jobs.json 领先一条、runs/ 缺最新一条 = over-report 可自愈），且由 `TestPersistOrdering_RunsNeverDivergeAheadOfJob`（`persist_ordering_runs_divergence_test.go:24`）+ `TestR242ARCH10_FinishRunPersistsBeforeEmit`（`finishrun_persist_before_emit_test.go:114`）双向钉死。
- 问题：issue 提出“是否要 Outbox/事务”。但**当前没有未覆盖的具体故障**被证明会造成 under-report 或不可恢复状态。本 RFC 必须先陈述“构建 Outbox 之前要先证明的具体未覆盖故障”，否则就是在没有缺陷证据的前提下引入分布式事务复杂度。

---

## 2. Goals & non-goals

### Goals
- G1：为 #776 给出一个**显式裁决**：是否把 `SchedulerConfig` 迁到 functional options，还是**正式批准保留 struct-config 现状**为本仓库的 cron 构造约定。
- G2：为 #764 设计一个 `Job` 关注点分离方案，**严格保持 `cron_jobs.json` 逐字段 wire 兼容**，并明确 runtime 字段的锁覆盖不变。
- G3：为 #1282 给出文件拆分的目标布局与拆分顺序（先于内容重构，纯移动）。
- G4：为 #1278 把 cron-local 并行类型的存在理由、同步规则、pin 机制写成可评审契约（文档化，非代码改动）。
- G5：为 #837 陈述“构建 Outbox 之前必须先证明的具体未覆盖故障”，并定义在此之前**只允许新增观测/回归测试**。

### Non-goals（防 scope creep）
- N1：**不**在本轮引入新的磁盘格式版本号 / schema migration（`cron_jobs.json` 当前是无版本号的逐字段向后兼容，引入 version 字段是独立 RFC）。
- N2：**不**实现 Outbox / WAL / 两阶段提交（#837 本轮只到“证明缺陷”阶段）。
- N3：**不**删除或合并 cron-local 并行类型（#1278），也不动 ordinal pin——只做文档化。
- N4：**不**改 cron→session 的依赖方向（已由 R20260527122801-ARCH-1 消除反向 import，保持现状）。
- N5：**不**在 #1282 拆分中**同时**做任何行为重构（拆分 PR 必须是 `git mv` + 包内可见性不变的纯搬运，零行为 diff）。
- N6：**不**触碰 `cmd/naozhi` 与 `wireup` 适配器的翻译逻辑（除非 G2 的 Job 拆分强制要求，而设计目标是不要求）。

---

## 3. Alternatives considered

### #776 配置形态

- **方案 A（选中）：批准保留 struct-config，仅做“收敛构造点”的小幅整治。**
  理由：(1) 生产侧只有 1 个构造点，functional-options 的核心收益（“可选字段在编译期可见、必填字段强制”）对单一生产调用点价值低；(2) 212 个测试夹具迁移到 `NewScheduler(WithRouter(...), WithStorePath(...), ...)` 是纯成本、零行为收益、且会与 7+ 并行 cron worktree 大面积冲突；(3) 现状字段已是 idiomatic optional，R245-GO-6 已保证 cfg 按值不泄漏。胜出点：成本/收益比最优，且“正式批准”本身就关闭了 issue（约定被显式记录，未来不再反复提）。
- **方案 B：全量 functional options。** 拒绝：~212 测试夹具 churn，且把唯一生产构造点从“一眼可读的字段表”变成“一串 With 调用”，可读性净损。
- **方案 C：必填字段走位置参数 + 可选走 options（`NewScheduler(router, storePath, opts...)`）。** 拒绝：Router 在测试里常为 nil（`AllowNilRouter` 路径），StorePath 在内存测试里常为空——把它们提成必填位置参数反而和现有测试模式冲突。

### #764 Job 拆分

- **方案 A（选中，仅设计本轮不落地）：三段式 embedded 拆分，保 JSON 形态。**
  把 `Job` 内部重组为：`JobSpec`（wire 配置）+ `JobRunResult`（last-run 持久产物）+ 非导出 runtime 块（entryID/cachedSched/cachedPeriod）。通过**嵌入（embedding）而非嵌套**保持顶层 JSON 字段名不变（embedded struct 的 `json` tag 提升到顶层），从而 `cron_jobs.json` 逐字段反序列化形态零变化。胜出点：是唯一能同时满足“关注点分离”与“N1 不引入 schema migration”的方案。
- **方案 B：拆成完全独立的 `JobConfig` / `JobState` 两个 map（`map[id]*JobConfig` + `map[id]*JobState`）。** 拒绝：会把 `finishRun` 当前在 `s.mu` 下对单一 `*Job` 的原子更新拆成两 map 写，引入新的一致性窗口——与 #837 的 divergence 目标背道而驰。
- **方案 C：维持现状，仅补 godoc 分区注释。** 部分采纳为 fallback：若评审认为 embedding 的间接层不值，则至少落地“字段分区注释 + 字段访问 lint”而不动结构（见 §8 Phase 选项）。

### #837 持久化

- **方案 A（选中）：先证明缺陷，再决定。** 本轮只补“失败注入”回归测试覆盖 `recordTerminalResult` 成功但 `runStore.Append` 失败的具体路径，确认当前 over-report-only 不变量在该故障下仍成立；若证明成立则**不建 Outbox**。
- **方案 B：直接建 Outbox（先写 intent log，再 apply）。** 拒绝：在没有 under-report 缺陷证据时引入 WAL，违反 N2，且 `jobPersistOK` gate + 两个 pin 测试已覆盖已知的 divergence 方向。

---

## 4. Test strategy

- **#776（若 Phase 1 落地小幅整治）**：新增 `scheduler_config_construction_test.go`，断言 `NewScheduler` 在 zero-value 可选字段下的 fallback（`MaxJobsPerChat<=0`→default、`SlowThreshold<=0`→default、`AllowedRoot==""`→检查关闭、`Location==nil`→`time.Local`）——把现状 idiomatic-optional 约定钉成回归测试。回归防护：`go build ./...` + 既有 213 处构造点全绿即证明无签名破坏。
- **#764（仅设计）**：拆分落地时必须先有 round-trip 测试：`job_wire_format_test.go`（已存在）+ `job_snapshot_contract_test.go`（已存在）必须在 embedding 重构前后产出**逐字节相同**的 `cron_jobs.json`。新增 `job_embedding_jsontag_test.go`：对一个填满所有字段的 `Job` marshal，断言 JSON key 集合与重构前快照完全一致（防 embedding tag 提升出错）。
- **#1282（纯移动）**：无新测试；判据是 `git mv` 后 `go test ./internal/cron/... -race` 全绿且 diff 仅为文件归属变化（`git log --follow` 可追溯）。
- **#1278（文档化）**：保留并依赖既有 pin：`cron_router_adapter.go:38` 编译期 pin + line 62 `init()` panic。建议新增 `agent_opts_parity_doc_test.go`：用反射断言 `cron.SessionStatus` 常量数量与 `session.SessionStatus` 一致（当前只 pin 了首值，未 pin 总数），作为 #1278 的“可执行文档”。
- **#837（先证明缺陷）**：新增 `finishrun_runstore_append_fail_test.go`：注入 `recordTerminalResult` 成功 + `runStore.Append` 失败，断言 (a) Job 字段已落盘、(b) metrics 已 bump、(c) 不发生 panic、(d) 下次 run 能重新对齐——即确认 over-report-only 不变量在此故障下成立。依赖既有 `TestPersistOrdering_RunsNeverDivergeAheadOfJob` 作为反向保证。

---

## 5. Risk & rollback

- **Load-bearing 并发/锁不变量**：
  - `s.mu` 同步覆盖 `jobs / chatJobCount / jobsByChat / sortedJobIDs` 四件套（scheduler.go:312-357 的字段分区注释）——#764 拆 Job 时**不得**把任何 runtime 字段移出 `*Job`，否则 `s.mu` 的覆盖语义破裂。pin：`config_maps_atomic_test.go`、`sorted_job_ids_test.go`、`updatejob_lockorder_test.go`、`pause_lockorder_test.go`。
  - `cachedSched`/`cachedPeriod` 由 `registerJob` 写、`HasMissedScheduleCached` 读，二者都在持锁路径——拆结构若改变指针身份会让缓存失效。pin：`cached_period_test.go`、`workdir_cache_test.go`。
- **On-disk 不变量**：`cron_jobs.json` 无版本号、逐字段 omitempty 向后兼容（job.go 多处注释）。pin：`job_wire_format_test.go`、`job_snapshot_contract_test.go`、`store_test.go`。
- **finishRun 三写顺序**：jobPersistOK gate（finish.go:219）+ Append 在 gate 之后（line 272）是 over-report-only 的结构性保证。pin：`persist_ordering_runs_divergence_test.go`、`finishrun_persist_before_emit_test.go`、`finishrun_nil_finalizer_test.go`。
- **ordinal pin**：`InterruptOutcome`/`SessionStatus` 与 session 侧 iota 对齐，由 adapter 编译期 + init() 双 pin。#1278 文档化**不得**删除这两个 pin。
- **回滚**：本轮唯一落地候选（§8 Phase 1）是 #776 的 zero-value-fallback 回归测试 + 字段分区注释——纯增量、零生产逻辑改动，回滚 = `git revert` 单个 commit，无 on-disk / 接口影响。所有结构性改动（#764/#1282/#837 Outbox）均不在本轮落地，故无运行时回滚面。

---

## 6. Observability

- 本 RFC 本轮（Phase 1）**不新增** log/metric/dashboard——只读测试 + 注释。
- 未来 #837 若真证明出 under-report 缺陷并建 Outbox：需新增 `metrics.CronRunStoreAppendFailedTotal`（区别于现有 `cron_run_*_total`）+ 一条 `cron runstore append failed (job persisted, run record dropped)` warn，使当前“静默 over-report”可观测。本轮仅记录此为未来项，不实现。
- 未来 #764 拆分落地时建议加一条 debug 计数 `sortedJobIDs` 与 `jobs` 漂移命中 fallback 的次数（marshalJobsLocked 的现有 hint-fallback，scheduler.go:349-356），以便观测 embedding 是否引入新漂移——同样属未来项。

---

## 7. Compatibility & migration

- **#776**：方案 A 零迁移——`NewScheduler(SchedulerConfig)` 签名不变。若评审选方案 B（functional options），需提供 `NewScheduler(SchedulerConfig)` 的 deprecated shim 一个 release 周期，再批量改测试；本 RFC 不推荐。
- **#764**：embedding 方案设计目标即“`cron_jobs.json` 逐字节兼容”——旧文件反序列化进 embedded 结构后顶层 key 不变；新写出的文件 key 集合不变。**无 migration step**（这正是选 embedding 而非独立 map 的原因，见 §3-A）。
- **#1282**：纯文件移动，包名 `cron` 不变，导出符号不变——对 `internal/cron` 的所有消费者（dashboard/dispatch/server/wireup）零影响。
- **#1278**：纯文档 + 一个 parity 测试，无格式/接口/flag 变化。
- **#837**：本轮无格式变化。未来 Outbox 若引入会需要 `runs/` 下的 intent log 目录——届时独立 RFC 处理 migration。
- **config flag**：本 RFC 不引入任何新 config flag。

---

## 8. Rollout plan

### 本轮裁决：hasLandablePhase1 = **false（结构性部分）/ true（仅 #776 注释+回归测试这一极小子集）**

绝大多数工作（#764 god-object 拆分、#1278 跨包并行类型契约重设计、#1282 高 churn 文件移动、#837 Outbox）**本轮只评审、不落地代码**，原因：
- #764 拆 Job 触碰 `s.mu` 锁覆盖面与 `cron_jobs.json` wire 形态，属“on-disk schema 形态 + 并发不变量”改动，必须先经评审。
- #1282 是高 churn 文件移动，需在并行 worktree 收敛后单独排期，否则 rebase 冲突成本失控。
- #1278 是跨包类型契约，删/改任一并行类型或 pin 都需架构评审。
- #837 在没有 under-report 缺陷证据前不得引入 Outbox（N2）。

### 唯一可独立安全落地的 Phase 1（low risk）

**内容**：把 #776 的“struct-config 现状 = 已批准约定”固化为可执行回归 + 在 `SchedulerConfig` 顶部加“必填 vs 可选”分区注释；并为 #1278 加 `SessionStatus` 常量计数 parity 测试（可执行文档）。

**确切文件级改动清单**：
1. `internal/cron/scheduler.go` — 在 `SchedulerConfig`（line 161）docstring 顶部加“必填：Router(或 AllowNilRouter)、StorePath；其余可选且 zero-value fallback”分区注释（约 10 行注释，零逻辑）。
2. 新增 `internal/cron/scheduler_config_construction_test.go` — table-driven 断言四个 zero-value fallback（MaxJobsPerChat / SlowThreshold / AllowedRoot / Location）（约 70 行）。
3. 新增 `internal/cron/agent_opts_parity_doc_test.go` — 反射断言 cron 侧 `SessionStatus`/`InterruptOutcome` 常量数量；含 session 侧对照（约 40 行；若需 import session 会触碰反向 import 红线，则改为在 `internal/wireup` 包内放置，约 40 行）。

**预估生产代码行数**：~10 行（仅 scheduler.go 注释，无逻辑）；测试 ~110 行。`phase1EstLines` 取 10（生产逻辑实质为 0，注释为主）。

**风险**：low — 不改任何运行时逻辑、不动结构、不动 on-disk 格式、不删任何 pin。回滚 = revert 单 commit。

### 后续阶段（均需先过评审，本轮不落地）
- Phase 2（#1282）：纯 `git mv` 文件拆分，待并行 worktree 收敛后排期。
- Phase 3（#764）：embedding 三段式拆分 + wire round-trip 测试。
- Phase 4（#1278）：若评审决定收敛并行类型则单列；默认仅文档化。
- Phase 5（#837）：先落 §4 的 append-fail 注入测试证明现状不变量；仅在证出 under-report 缺陷后才进入 Outbox 设计。
