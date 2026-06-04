# RFC: lifecycle policy 接口 + Get/Fetch/Load 命名收口（deferred items）

> **状态**: Draft v1（待评审）
> **作者**: naozhi team (cron-cr)
> **创建**: 2026-06-04
> **范围**: 把分散在 cron/dispatch/upstream 三条热路径里内联的 retry/overlap/notify 生命周期决策抽成接口（#870），并统一全仓 `Get*/Fetch*/Load*` 命名（#463）；同时记录 sysession AutoTitler 改长生命周期进程（#729）为何继续 deferred。本轮**纯设计、零生产代码**。
> **关联 issue**: #870 (R243-ARCH-25), #463 (CURATED-NAMING-1), #729 (R246-PERF-14)
> **关联代码**:
> - `internal/cron/scheduler_run.go`（`executeOpt` 状态机，约 344 行；`jobSnapshot`/`execArgs` 携带 `notify*` 字段，见 ~291/~450/~494/~595–649）
> - `internal/dispatch/dispatch.go:807` `BuildHandler`（passthrough vs queue vs Guard 三分支的 overlap/coalesce 策略）
> - `internal/upstream/connector.go:245` `Run`（backoff + circuit-breaker + jitter retry 循环）
> - `internal/session/router_core.go:1687` `GetSession`、`router_lifecycle.go:285` `GetOrCreate`、`router_workspace.go:45` `GetWorkspace`、`router_backend.go:265` `GetSessionBackend`
> - `internal/cli/process.go:799` `GetSessionID`、`:776` `GetState`、`:938` `GetTotalTimeout`
> - `internal/cron/agent_opts.go:62` `type Session interface`（rename 碰撞源）
> - `internal/sysession/runner.go:250–290` `runnerImplBaseArgs` + `runnerImpl.Run`（`-p --output-format text --setting-sources ""`）
> - `docs/rfc/system-session.md` §6.1（SharedCLI 已废弃决策）

---

## 1. Background & problem

本 RFC 把三个被 triage 标 `needs-design` 的 P3 项收在一起，因为它们都属于"明知是债、但落地前必须先定边界"的同一类——且 #870 与 #729 都被显式 sequencing 在更大的 RFC 之后。

### 1.1 #870 — 三条热路径内联了 lifecycle/policy 决策

可复现地，三个 hot-path 函数各自把"什么时候重试 / 什么时候算并发冲突跳过 / 要不要通知"的策略硬编码在控制流里：

- **cron `executeOpt`**（`scheduler_run.go`，约 344 行状态机）：overlap 策略（per-jobID gate 拒绝并发运行，见 ~215）、notify 策略（`snap.notify *bool`，nil=unset，~311/~457；`deliverNotice` ~618/~643）、以及 fresh-preflight 工作目录可达/越界的 skip-then-notify 分支（~595–649）全部内联在同一函数。
- **dispatch `BuildHandler`**（`dispatch.go:807`）：overlap/coalesce 策略以 `if d.queue.Mode()==ModePassthrough` → 每消息一 goroutine、否则 `queue.Enqueue` 的 owner/non-owner 分流硬编码在 handler 闭包里。
- **upstream `connector.Run`**（`connector.go:245`）：retry 策略 = backoff 1s→30s 倍增 + `circuitBreakerThreshold` 熔断 + `JitterBackoff`，全部内联在 `for` 循环里。

**为何是问题**：issue 自标 "pre-work for the zero-downtime restart RFC"。零中断热重启（MEMORY 记录 shim RFC v3 完成、待实现）需要在 restart 窗口内**临时替换** overlap/retry 行为（例如 drain 期间 overlap 一律 skip、retry 一律不重连新 upstream）。今天这些决策没有接缝（seam），restart RFC 要改就得改三处热路径的控制流本身，diff 大且高风险。注意：截至本 RFC，仓库 `docs/rfc/` 下**尚无** restart/zero-downtime RFC 文件落盘（grep 无命中），所以 #870 的下游依赖目前只是 MEMORY 里的设计意图，接口形状还无法被真实 consumer 约束——这正是它该继续 deferred 的核心理由。

### 1.2 #463 — `Get*/Fetch*/Load*` 命名不符合 Go 惯例

Go 惯用 getter 不带 `Get` 前缀（`io.Reader.Read`，非 `GetRead`）。naozhi 现状（实测）：

- `\.GetSession|\.GetOrCreate|\.GetWorkspace|\.GetActiveCount|\.GetSessionID|\.GetCurrentBackend|\.GetSessionBackend|\.FetchSession|\.LoadHistory` 跨 `internal/` + `cmd/` 共 **165 处引用**（issue 写 "125+"，实测更多，以本 RFC 为准）。
- 方法定义侧：`Router.GetSession` / `GetOrCreate` / `GetWorkspace` / `GetSessionBackend`，`cli.Process.GetState` / `GetSessionID` / `GetTotalTimeout`，`LoadHistoryChainTail` / `LoadBefore` 等。

**为何是问题**：这是 caller-visible API rename，不满足 triage 的 "zero functional impact" 标准（issue triage notes 已点明），所以从 `docs/cosmetic-backlog.md` 升级进 issue tracker。但更关键的是存在**命名碰撞**：`internal/cron/agent_opts.go:62` 已有 `type Session interface`。若机械把 `GetSession()` rename 成 `Session()`，在任何同时引用该 `Session` 类型与持有 router 的作用域里会产生 method-vs-type 的可读性混淆（Go 允许，但 review 噪声大），必须先做碰撞审计。

### 1.3 #729 — sysession AutoTitler 每次调用 spawn `claude -p`

`runner.go:250–290`：`runnerImpl.Run` 每次 AutoTitler 调用都 `exec.CommandContext(ctx, BinPath, "-p", "--output-format", "text", "--setting-sources", "", ...)` 起一个一次性子进程。冷启动 50–100ms × N（批量时）。

**为何是问题（且为何 deferred）**：issue 提议改长生命周期 stream-json 进程，但这**直接撞** `system-session.md` §6.1 —— 该 RFC 已**明确砍掉** SharedCLI 长生命周期共享进程方案，理由是 "Reset 语义与共享自相矛盾，连锁解决跨用户 JSONL 累积、串行化未定义、残留进程"。当前 AutoTitler 频率低（P3），没有真实批量 use case 倒逼，重开 SharedCLI 等于重新引入已被论证过的伤口。

---

## 2. Goals & non-goals

### Goals
- 为 #870 定义 `RetryPolicy` / `OverlapPolicy` / `NotifyPolicy` 三个接口的**目标形状**与抽取顺序，使其成为零中断重启 RFC 的可插拔接缝。
- 为 #463 定义命名 ADR（`Get*` 去前缀、`Fetch*/Load*` 统一为 `Load*`）+ 碰撞审计清单 + PR 切分顺序 + 执行工具（`gopls rename`，不用 sed）。
- 为 #729 记录 deferred 决策与"何时重启该讨论"的明确触发条件。

### Non-goals（防 scope creep）
- **本轮不落地任何 #870 的接口抽取代码**——接口形状必须先被真实 consumer（restart RFC）约束，否则是 premature abstraction（与 `runner.go` 里 `OneshotArgs()` deferred 同款理由）。
- **本轮不执行任何 #463 rename**——165 处 + 碰撞审计 + gopls 跨包验证，不是单 PR 能安全闭环的。
- **不重开 SharedCLI**——#729 在 batch use case 成熟前不动。
- 不改 on-disk 格式（cron job JSON、session JSONL、event-log）。
- 不引入新 config flag、新依赖、新密钥/签名设施。
- 不动 `executeOpt` 的锁顺序、`per-jobID gate`、`s.mu` 持有边界等 load-bearing 并发不变量（见 §5）。

---

## 3. Alternatives considered

### #870 接口抽取

**A. 现在就抽三个接口（issue 字面提议）** — 否决。无真实 consumer 约束接口形状；restart RFC 尚未落盘（grep `docs/rfc/` 无 restart 文件）。先抽极可能选错粒度（例如把 circuit-breaker 也塞进 `RetryPolicy` 还是独立 `CircuitPolicy`？无 consumer 无法定夺），后续返工成本 > 现在内联的维护成本。

**B. 等 restart RFC 落盘、由它驱动抽取（本 RFC 选中）** — 胜出。接口由下游真实需求倒逼，粒度有依据；本 RFC 只锁定"目标形状草案 + 抽取顺序"，作为 restart RFC 的输入。

**C. 不抽接口，restart RFC 直接改三处控制流** — 否决。三处热路径都带敏感并发不变量（§5），直接改控制流的 review/回归面远大于先有接缝再 swap。

### #463 命名

**A. sed 批量替换** — 否决。`GetSession` 是子串，sed 会误伤 `GetSessionID`/`GetSessionBackend`；且无法处理跨包导出符号的引用更新与 `Session` 类型碰撞。

**B. `gopls rename` 逐符号 + 人工碰撞审计 + 分 PR（本 RFC 选中）** — 胜出。`gopls rename` 理解 Go 语义（跨包、跨文件、只改引用不改注释/字符串），可逐符号原子提交。配合碰撞审计避开 `type Session`。

**C. 维持现状 / 只加 lint 禁新增** — 部分采纳为兜底：在 rename 真正排期前，可先加 `revive`/自定义 lint 规则禁止**新增** `Get*` getter，阻止债务增长，但这本身也是独立改动，不在本轮。

### #729 AutoTitler

**A. 长生命周期 stream-json 进程（issue 提议）** — 否决，撞 §6.1。
**B. 维持 transient `claude -p`（选中）** — AutoTitler 频率低，50–100ms 冷启动可接受；保住 `--setting-sources ""` 反死循环隔离。
**C. 进程池/复用（折中）** — 记录为"batch use case 成熟后"的候选，但同样需先解决 §6.1 指出的跨用户 JSONL/串行化问题。

---

## 4. Test strategy

本轮无代码，故下列为**各项真正落地时**必须配套的测试，写在这里作为评审 checklist。

### #870 接口抽取落地时
- **Unit**：每个 policy 接口的默认实现单测——`OverlapPolicy.AllowConcurrent()` 在 running/paused/deleted 三态返回值（对照 `executeOpt` ~107/~109 现有 skip 分支）；`RetryPolicy.NextBackoff()` 的 1s→30s 倍增 + 熔断 pin（对照 `connector.go` `circuitBreakerThreshold`/`circuitBreakerBackoff`）；`NotifyPolicy.ShouldNotify()` 对 `notify *bool` nil/true/false 三态。
- **Regression（关键）**：抽取**前后**行为必须逐位等价。点名要加的 pin 测试——
  - `TestExecuteOpt_OverlapSkip_EmitsOverlapSkipped`（保住 `emitOverlapSkipped` 与 `cron_run_started/ended` 配对，见 ~504）。
  - `TestConnectorRun_BackoffSequence`（断言 backoff 序列与熔断后 pin 值不变）。
  - `TestBuildHandler_PassthroughVsQueue_Dispatch`（owner/non-owner 分流不变）。
  - 这些应在抽取 PR 之前先以**当前内联实现**为基准写绿，再做"抽取后仍绿"的等价性回归。
- **Integration**：cron end-to-end 一次 overlap 命中 + 一次 notify 投递；upstream 断连重连走完一轮熔断。

### #463 rename 落地时
- 不需要新增行为测试（纯 rename，行为不变）。
- **Regression**：每个 rename PR 必须 `go build ./... && go vet ./... && go test ./...` 全绿；新加现有缺失的 router/process 方法调用编译期就是回归网。
- **碰撞审计测试**：rename `GetSession→Session` 后，在引用 `cron.Session` 类型的包里跑 `go vet` 确认无歧义。

### #729
- N/A — 维持现状，无新测试。若未来重开，需 pin `--setting-sources ""` 仍在 argv（已有 `runner.go` godoc 契约 + 应配 `TestRunnerArgs_SettingSourcesEmpty`）。

---

## 5. Risk & rollback

### #870
- **Load-bearing 不变量（绝不能因抽取被破坏）**：
  - `executeOpt` 的 **per-jobID gate**（~215）与 **`s.mu` 持有边界**——godoc 明确 "send/notify pipeline never runs under s.mu"（~52/~281），notify 决策若被抽到 policy 对象，调用点必须仍在锁外。
  - `snapshotJob` 在 `s.mu` 下读 `j`（~421），`notify *bool` 的拷贝语义（~457–459，拷值非拷指针）不能丢。
  - cron→session/dispatch 的反向 import 方向（MEMORY cron-sysession-merge 关注点）——新接口放包位置不能制造反向依赖。
- **回滚**：接口抽取是机械等价重构，单 PR revert 即回滚。等价性回归测试（§4）是安全网。

### #463
- **风险**：165 处跨 50+ 文件，漏改→编译失败（即时暴露，低风险）；`Session` 类型碰撞→可读性退化（中风险，靠审计规避）；多人在途 PR 的 merge 冲突（中风险，靠分小 PR + 快速 merge 降低）。
- **回滚**：每个 rename 是独立原子 PR，`git revert` 单 PR。`gopls rename` 不碰运行时行为，回滚无状态残留。

### #729
- N/A — 不动代码，无风险。维持 `--setting-sources ""` 反死循环 pin（DESIGN.md §6.5）。

---

## 6. Observability

- **#870**：抽取须**保留**现有信号——`cron_run_started/ended`、`emitOverlapSkipped`、`connectorBackoffMillis` gauge、熔断 trip/reset 的 `slog.Warn/Info`。新接口**不新增** metric（行为等价的重构不应改变可观测面）。可选：为 policy 决策点加 Debug 级 log（默认关）。
- **#463**：N/A — 纯 rename 不影响 log/metric（log 字段名、metric 名是字符串字面量，gopls 不动它们）。
- **#729**：N/A — 维持现状。

---

## 7. Compatibility & migration

- **#870**：向后兼容——内部重构，无导出 API/on-disk/wire 变化。接口若需导出供 restart RFC 跨包消费，属新增导出符号，不破坏现有调用方。
- **#463**：**破坏性**于源码层（caller-visible 符号 rename），但——naozhi 是单仓应用、无外部 import 该 internal 包，故无下游兼容负担。on-disk/wire/config 全不变（rename 只动 Go 标识符）。迁移路径：分 PR 顺序见 §8。
- **#729**：完全兼容——不动。

---

## 8. Rollout plan

### 总体：本轮无 Phase 1 代码落地。

三项均为 deferred / 纯 RFC：
- **#870** sequencing 在 restart RFC 之后——接口形状须由其约束，restart RFC 尚未落盘。
- **#463** 需先批 ADR + 完成碰撞审计 + 排 PR 序，rename 走 `gopls rename`（非 sed）。本 RFC 评审通过即解锁后续执行型 PR。
- **#729** 维持 deferred，触发条件：出现真实批量 AutoTitler/sysession use case **且** §6.1 的跨用户 JSONL/串行化问题有解。

### #870 分阶段（restart RFC 落盘后启动）
1. 以当前内联实现为基准补齐等价性 pin 测试（§4）。
2. 抽 `OverlapPolicy`（最小、最独立）→ 单 PR。
3. 抽 `RetryPolicy`（connector）→ 单 PR。
4. 抽 `NotifyPolicy`（cron，注意锁外约束）→ 单 PR。
5. restart RFC 注入替换实现。

### #463 PR 切分（ADR 批准后启动）
- PR-1：碰撞审计 + （可选）lint 禁新增 `Get*`。
- PR-2：`Get*` 去前缀（`GetSession→Session` 等），逐符号 `gopls rename`，避开 `cron.Session` 碰撞（如冲突则该符号改 `SessionFor`/`Lookup` 等替代名而非裸 `Session`）。
- PR-3：`Fetch*/Load*` 统一为 `Load*`。
- 每 PR 独立 `build+vet+test` 绿、独立 merge、独立可 revert。

### Phase 1 可独立安全落地？— **否（hasLandablePhase1 = false）**

理由（保守判断）：
- #870 是**跨包接口重设计**且接口形状无真实 consumer 约束——本轮抽取属 premature abstraction，且触碰 `executeOpt` 锁/gate 等 load-bearing 并发不变量，不符合"≤150 行 / low-medium / 不破坏已 pin 不变量"门槛。
- #463 是 165 处 caller-visible rename，需先批 ADR + 碰撞审计，单轮无法安全闭环。
- #729 显式 deferred，撞已废弃的 SharedCLI 决策。

最接近"可落地"的一步（仍建议先评审）：#870 的 **step-1 等价性 pin 测试**（只加 `internal/cron` + `internal/upstream` 测试文件，不改生产代码，约 80–120 行测试代码，low risk）。但这属测试新增而非生产代码抽取，故生产代码层 Phase-1 估为 0 行。
