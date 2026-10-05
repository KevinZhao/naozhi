# RFC: 统一 cost 归口层（cost ledger）与计费精度修正

- 状态：Draft v2.1（v1 经 5 路独立评审：架构 / Go 可行性 / 性能 / 安全 / 事实核查；v2 经架构 + Go 二轮复审均 APPROVE-WITH-CHANGES；全部 BLOCKING/SHOULD-FIX 已吸收，见 §14）
- 日期：2026-09-05
- Issue：#2286 [R202606e-ARCH-6]
- 相关：#2276（per-turn delta）、#2280（本地 cron cost）、#2284（孤儿 session delta）、#2345（kiro credits）、#2355（leak-recovery 重复计费）

## 1. 背景与问题

### 1.1 现状：三套互不相通的记账

| 记账点 | 位置 | 语义 | 单位 |
|---|---|---|---|
| (a) session `costSpent` / `lastCumulativeCost` | `internal/session/managed_runmetric.go:48-92` finishRun → `runhistory.TurnCostDelta`（`runhistory/cost.go:13`） | CLI `total_cost_usd`（**进程内累计值**）差分成 per-turn delta 后累进；落盘 `session-runs/<hash>/<run>.json` 与 `sessions.json`（`store.go:194`） | USD |
| (b) cron `CronRun.CostUSD` / `SandboxRunMeta.CostUSD` | `internal/cron/run.go:50`、`cron/sandbox.go:88`、`scheduler_run.go:826`、`wireup/cron_router_adapter.go:123` | 本地 run 直接透传 `cli.SendResult.CostUSD`（**未差分的进程累计值**）；sandbox run 用 agentcore envelope 的 `total_cost_usd` | USD |
| (c) backend `Profile.CostUnit` | `profile_claude.go:32` USD / `profile_kiro.go:31` credits / `profile_codex.go:36` tokens | 仅 UI 标签 | 标签 |

没有"本机总花费""某 job 跨本地+sandbox 总花费""某模型总花费"的查询面。Dashboard Home 的「总花费」是前端对当前 live session 列表 `total_cost` 求和（`dashboard.js:9188 computeHomeStats`），删 session 即消失，不含 cron/sysession。

### 1.2 实测：CLI 到底给了什么

对线上同版本 CLI（2.1.261，Bedrock 直连）跑 `claude -p --output-format json`，result 事件（节选）：

```json
{"type":"result","total_cost_usd":0.81848,
 "usage":{"input_tokens":2,"cache_creation_input_tokens":40913,"cache_read_input_tokens":0,"output_tokens":4,
          "cache_creation":{"ephemeral_1h_input_tokens":40913,"ephemeral_5m_input_tokens":0}},
 "modelUsage":{"us.anthropic.claude-fable-5-1[1m]":{"inputTokens":2,"outputTokens":4,"cacheReadInputTokens":0,
   "cacheCreationInputTokens":40913,"webSearchRequests":0,"costUSD":0.81848,"contextWindow":1000000,
   "thinkingTokens":0,"canonicalModel":"claude-fable-5-1","provider":"bedrock","costBasis":"list"}}}
```

从 CLI 二进制提取的 schema describe 要点：

- `total_cost_usd`：query 级**累计**估算，"read the latest result rather than summing"；resume 后从 0 开始；**mid-session `/clear` 归零**；"An estimate, not a billing statement"。
- `modelUsage`：**"The correct field for token/cost accounting"**。分模型累计 token + costUSD，覆盖主循环、Task 子代理、sidechain、compaction 等所有 query 管线调用；同样 resume/`/clear` 归零。
- `usage`：**仅主循环**、per-turn，不含子代理——不适合记账。
- `costBasis`：`list`（CLI 内置价表）/ `managed`（settings `modelPricing` 合同价或倍率）/ `unknown`（**模型不在价表，costUSD 是按默认模型价猜的**）。
- 价表：Bedrock 与 1P 共用同一张 list 价表（catalog `pricing_tiers`，fable-5-1 = `tier_10_50_cache_read_0_25`：input 10 / output 50 / cache_write_1h 20 / cache_read 0.25 USD/Mtok）；无 Bedrock 专用价。校验：40913×20/1e6 + 2×10/1e6 + 4×50/1e6 = 0.81848 ✓。
- `settings.json` 的 `modelPricing.overrides / multiplier` 会改写 `total_cost_usd`（naozhi 用 `--setting-sources user` 会生效）。这是让"估算"贴近 Bedrock 合同价的**零代码杠杆**，但 naozhi 当前不记录 `costBasis`，UI 无法区分估算 / 合同价 / 猜测。
- kiro：`_kiro.dev/metadata.meteringUsage` 是 **per-turn 增量**，naozhi 在 `cli/process.go:668-691` 按 Unit 累加成**进程级累计视图**，`proc.MeteringUsage()` 返回的是累计值。codex 走同一 metering 通道，Unit=`token`。

### 1.3 精度缺陷清单（已逐条核实 file:line）

| # | 缺陷 | 证据 | 影响 |
|---|---|---|---|
| P1 | 只存 `total_cost_usd` 一个标量，丢弃 `modelUsage` | `cli/event.go:19` 只有 `CostUSD`；`event.go:464` SendResult 同；`grep -ri modelUsage internal` 零命中 | 无法分模型统计、无法识别 `costBasis=unknown` 的"猜价" |
| P2 | 本地 cron run 记的是**进程累计值**而非本 run 增量 | `wireup/cron_router_adapter.go:123` 透传 `r.CostUSD`；`scheduler_run.go:826` 直接持久化 | `fresh_context=true`（线上 6/7 job）每 run 新进程所以碰巧正确；persistent 模式（1 job）逐 run 重复计费；同一 turn 在 session 侧已算过 delta，两处口径不一致 |
| P3 | sysession Runner（AutoTitler 等）cost 完全不可见 | `sysession/runner.go:184` `-p --output-format text`，只返回 stdout | "本机总花费"漏掉一类持续开销 |
| P4 | cost 记账寄生在 run-metrics 上 | `managed_runmetric.go:49` `if rt == nil \|\| s.runStore == nil { return }`，`:88` 是 costSpent 唯一累进点 | runStore 为 nil 时 `costSpent` 永不累进 |
| P5 | 无 result 的 turn（timeout/cancel/进程死亡）成本丢失或错配 | `managed_runmetric.go:77` 仅 `result != nil` 计费 | 同进程下并入下一 turn（总额对、per-turn 错）；进程死亡则永久丢失 |
| P6 | 单位异构无字段承载；kiro credits / codex tokens 不落盘 | `SessionRun.CostUSD` 只有 USD；credits 只在 header snapshot（`managed_query.go:236`）；`protocol_acp.go` 无 CostUSD → kiro `SessionRun.CostUSD=0` | kiro/codex 消耗不进任何持久账 |
| P7 | 无 `costBasis` 记录 | 同 P1 | `dashboard.js:4016-4020` 注释因"Bedrock 常读 $0"删掉了 cost chip，是旧 CLI 行为；现已非 0 但仍是 list 估算，UI 无法诚实标注 |
| P8 | passthrough 合并/乱序 turn 的 per-turn 归属近似 | `passthrough.go:329` follower `CostUSD:0`；`TurnCostDelta` 把增长全给先到者 | 总额正确、per-turn 近似——**接受，不在本 RFC 修** |
| P9 | Home 总花费 = 前端 live session 求和 | `dashboard.js:9199` | 不含已删 session / cron / sysession |
| P10 | cron 时间轴成本小字 = 前端对**已加载** run 求和（初始种子 `recent_runs`，cap=5：`dashboard/cron/handlers.go:571`） | `cron_view.js:2335`；UI 已标注"非全量账单" | 没有任何 per-job 聚合口径；`scheduler_run.go:825` 注释里的 "monthly aggregates" 在代码中不存在 |

## 2. 目标与非目标

**目标**

- G1 单一 append-only **cost ledger**：每个计费事件一条 entry，携带 source / session_key / job_id / run_id / workspace / backend / model / provider / unit / amount / tokens / basis。
- G2 **run owner 写账**原则：谁拥有 run 生命周期谁写 ledger，一个 turn 只被记一次（§5.0）。
- G3 查询面：`GET /api/cost/summary`，按时间窗 + 维度分组，**按 unit 分桶**返回，绝不跨单位求和。
- G4 精度：记录 `modelUsage` 分模型 delta 与 `costBasis`；修 P2（cron delta 同源）、P4（记账脱离 runStore）、P6（credits/tokens 差分入账）；P3 补齐。
- G5 Dashboard：Home 花费卡改读 ledger；cron 详情增加 per-job 聚合；标注 basis。

**非目标**

- 不做 AWS Cost Explorer / CUR 对账；不做预算/告警；不做跨机汇总；不替换 CLI 定价来源（合同价走 `settings.json modelPricing`）；不修 P8。

## 3. 方案候选

| 方案 | 说明 | 结论 |
|---|---|---|
| A. 只读聚合器 | 不新增存储，查询时扫 `session-runs/` + `runs/` 求和 | ❌ sysession 无处落；runhistory `keepCount` 淘汰（`runhistory/store.go:197-203`）让总额随时间缩水；无 unit/model 字段；O(文件数) 扫描 |
| **B. 独立 ledger（选定）** | `internal/costledger`：按天分片 JSONL append-only + 内存 rollup；各 run owner 调同一 `Append` | ✅ 与 runhistory / cron runs 正交，不动其格式；"永不丢"层唯一；字段完整 |
| C. SQLite | 单文件库 | ❌ 违反"不引入外部组件、状态文件化"原则 |
| D. 扩展 `runhistory.SessionRun` | 加 tokens/model 字段并让 cron/sysession 也写 SessionRun | ❌ SessionRun 语义是"一次 session 往返"；`keepCount` 淘汰与账本"永不丢"冲突 |
| E. 复用 `runtelemetry` 事件层 | `RunEndedEvent`（`runtelemetry/event.go:40-53`）加 cost 字段 + 一个订阅者写盘 | ❌ `Subsystem` 只有 Cron/Sysession（`event.go:12-15`），没有 session 这一类；它是 WS 广播层不是存储层；ordinary session 的 turn 没有 Run 事件。**但 ledger 可作为 runtelemetry 的消费者复用 RunID/OwnerID 契约**（§5.4 / §5.5 直接引用其 RunID） |

## 4. 数据模型

```go
// internal/costledger/entry.go —— 叶子包：不 import 任何 internal/*（照抄 runtelemetry/imports_test.go 的 TestPackageIsLeaf）
type Source string // "session" | "cron_local" | "cron_sandbox" | "sysession"
type Unit   string // "USD" | "credits" | "tokens"
type Kind   string // 采集方式："turn"(CLI 累计差分) | "receipt"(sandbox 回执) | "metering"(kiro/codex) | "backfill" | "partial" | "adjust"(对账修正，可为负)
type Basis  string // CLI 定价口径："list" | "managed" | "unknown" | ""(非 claude)

type Entry struct {
    TS         time.Time    `json:"ts"`
    Source     Source       `json:"source"`
    Kind       Kind         `json:"kind"`
    SessionKey string       `json:"session_key,omitempty"`
    JobID      string       `json:"job_id,omitempty"`
    RunID      string       `json:"run_id,omitempty"`      // cron / sysession 的 runtelemetry RunID；session 的 runhistory RunID
    Workspace  string       `json:"workspace,omitempty"`   // session 的 workspace / cron 的 work_dir（basename 或 stable id，不存绝对路径）
    Backend    string       `json:"backend"`
    Unit       Unit         `json:"unit"`
    Amount     float64      `json:"amount"`                // 本 entry 增量，唯一权威金额
    Basis      Basis        `json:"basis,omitempty"`
    Models     []ModelDelta `json:"models,omitempty"`      // 分模型下钻，仅供展示/诊断，不参与 rollup 求和
}

type ModelDelta struct {
    Model      string  `json:"model"`               // canonicalModel；缺省用 key 去 [1m]
    RawModel   string  `json:"raw_model,omitempty"`
    Provider   string  `json:"provider,omitempty"`
    Basis      Basis   `json:"basis,omitempty"`
    CostUSD    float64 `json:"cost_usd"`
    Input, Output, CacheRead, CacheWrite, Thinking, WebSearch int64
}

// Cumulative 是"某进程 incarnation 的累计视图"，Delta 只记正增长（同 TurnCostDelta 规则）。
type Cumulative struct {
    USD     float64
    Models  map[string]ModelUsage   // key = raw model
    Metered map[Unit]float64        // kiro credits / codex tokens
}
func Delta(raw, prev Cumulative) (d Increment, next Cumulative)
```

约束（全部有单测）：

- `Amount` 是 **delta**，永不为累计值；`Amount<=0 && len(Models)==0` 的 entry 被 `Append` 拒绝（返回 false，不计 dropped）。唯一例外是 `Kind=adjust`：它的 `Amount` 与 `Models[]` 各字段可以为负（`Amount==0` 且无 `Models` 仍拒绝），其余 kind 的负 `Amount` 归 0。rollup 与汇总直接相加，修正自然抵消。
- `Amount` 是唯一权威；`Models[].CostUSD` 之和与 `Amount` 的偏差 > max(1%, $0.001) 时 warn（每模型去重）而**不是**断言——`total_cost_usd` 与 `modelUsage.costUSD` 是 CLI 两个独立累加器。
- `Source/Kind/Unit/Basis` 为类型化枚举；写入时校验，非法 `Basis` → `unknown`，非法 `Source/Kind/Unit` → 拒绝 + dropped 计数。
- `Model/RawModel/Provider`：来自 CLI 输出，视为不可信：长度 ≤128、合法 UTF-8、禁 C0/DEL、禁换行；违规替换为 `<invalid>` 并 warn（每 raw 值去重）。
- `Models` 上限 16 条（`cli/process.go maxMeteringUnits` 同款防御）：超出时第 16 条起合并成一条 `model="other"` 的行（`cost_usd` 与 token 相加，`basis` 取最差），分模型之和不因截断变小；`Amount` 不动（`costledger.CapModels`，sandbox 回执写 run 记录前同样合并）。
- `Source=session` 的行在 cron key（`cron:<job_id>`）上带 `JobID`（`sessionkey.CronJobIDFromKey`，其余 key 为空）：会话在 cron key 上记的账——窗口关闭后到达的迟到 result、进程结束的 partial、运行窗口外从 dashboard 手动发进该会话的 turn——都计入该 job 的 `group_by=job` 与 `job_id=` 视图（#3401），所以该视图的 `entries` 是账本记录数而非运行次数；迟到 result 与 partial 的 `RunID` 仍是 `unowned:` / `end:`，不并入已落盘的 run 记录（§5.3）。
- 一条 entry ≈ 350 B。

## 5. 精度修正设计

### 5.0 写入点归属（run owner 原则）

| Source | 写入者 | 何时 | 金额来源 |
|---|---|---|---|
| `session` | `ManagedSession.accountTurnCost`（§5.2） | 每次 `Send/SendPassthrough` 回合结束（含 leak-recovery 的第二回合，各记一条） | `Delta(proc 累计视图, lastCumulative)` |
| `cron_local` | `cron.Scheduler.finishRun` | run 终态 | cron 在所持会话上开关的 cost window 收集到的增量（§5.3） |
| `cron_sandbox` | `cron.Scheduler.finishRun` | run 终态且 `SandboxMeta.CostUSD>0` | `SandboxMeta.CostUSD`（§5.4） |
| `sysession` | `sysession.Manager.runOnce` | 每次 daemon Tick 的 Runner 调用返回 | result 帧 `total_cost_usd`（独立进程，无需差分）（§5.5） |

规则：cron 在 Send 前对所持的 `*ManagedSession` 调 `BeginCostWindow`，Send 返回处立即调 `EndCostWindow`（#3401）。窗口开着时，`accountTurnCost` 把本会话自己差分出的增量**累进 costSpent 并收进窗口，不写 ledger**——此刻 cron 是 run owner，`EndCostWindow` 返回的就是 run 的增量；窗口外的读数（Send 返回后才到的迟到 result、dashboard 对 cron key 的手动发送、进程结束 partial、沿 `successor` 链转来的花费）一律以 `Source=session` 入账，保证永不漏账。"窗口是否开着"与差分、累进在同一段 `costMu` 内判定，所以每个读数要么在 run 的增量里、要么是一条 session 行，不会两边都记、也不会两边都漏。窗口未关又被重开时，前一个窗口收集的增量没有 owner 会认领，直接以 `Source=session` 入账。唯一刻意接受的漏账：Send panic 时 `execSend` 的 defer 关窗但没有 `finishRun` 去记（run 记录本身也一并丢失，与旧门控一致）。旧的按 key 判定的 `ownedByRun` 门（run 在途期间一直持有，比 cron 的测量区间长：Send 返回到 `finalize` 之间到达的 result 与 deadline 路径 Reset 杀掉进程的 partial 两边都不记）已删除。`IsSysKey` 的 session 不经 ManagedSession（Runner 直接 exec），无此分支。passthrough 合并的 N 个 turn 只有 head 有非零增量 → 1 条 entry（P8 已接受）。

**不采用** v1 的 ctx attribution / `SendResult.CostDeltaUSD`：cron `execSend` 从 `s.stopCtx` 重建 ctx（`scheduler_run.go:679`）、`recoverLeakedToolcall` 逐字段重建 SendResult（`leak_recovery.go:104-110`），两处都会把值丢掉。

### 5.1 CLI 层：把 `modelUsage` 带出来

- `cli.Event` 新增 `ModelUsage map[string]ModelUsage \`json:"modelUsage,omitempty"\``；`ModelUsage` 字段对齐 CLI schema（inputTokens/outputTokens/cacheReadInputTokens/cacheCreationInputTokens/webSearchRequests/thinkingTokens/costUSD/canonicalModel/provider/costBasis）。
- `cli.SendResult` 新增 `ModelUsage map[string]ModelUsage`（累计快照，语义同 `CostUSD`）。`process_send.go:233`、`passthrough.go:317` head 填充；follower 零值；`leak_recovery.go:104-110` 重建时**必须拷贝**（加注释 + 测试）。
- `ClaudeProtocol.ReadEvent` 是整 Event unmarshal（`protocol_claude.go:493`，pooled `*Event`）：`encoding/json` 仅在 JSON 中出现 `modelUsage` 键时才分配 map，非 result 帧零额外分配；pool 复位 `*ev = Event{}` 已覆盖新字段。新增 `BenchmarkReadEvent_NonResultFrame` 锁定 allocs/op 与现值相等，`BenchmarkReadEvent_ResultWithModelUsage` 记录新基线。
- ACP/codex 不填 `ModelUsage`。`EventEntry.Cost` 不变。

### 5.2 session 层：统一累计快照 + 脱离 runStore

- `ManagedSession` 新增 `lastCumulative costledger.Cumulative`（受既有 `costMu` 保护），**取代** `lastModelUsage` 概念；`lastCumulativeCost atomic` 保留为其 USD 分量的持久化镜像（`store.go:76` 格式不变）。
- 生命周期与 `lastCumulativeCost` 逐点对齐：
  - respawn：`installFreshSessionLocked`（`router_lifecycle.go:859`, `:900-902`）置零 `lastCumulative`，承接 `costSpent`；
  - 同进程迁移：`RenameSession`（`router_lifecycle.go:1283`, `:1335-1336`）同时拷贝 `costSpent` 与 `lastCumulative`；
  - 被替换的会话之后还会记账（异步的进程结束 partial、迟到的 result）。respawn 提交与 rename 拷贝时，在旧会话的同一段 `costMu` 内把它链到新会话（`successor`），并把快照之后记在旧会话上的花费补给新会话；此后旧会话上的 `costSpent` / `spent` 增量沿链转给链尾的活会话（`addSpent`，逐个加锁、不嵌套）。respawn 的旧进程基线仍留在旧会话上差分；rename 的新会话跑的是同一进程，旧会话收到的读数整个交给新会话差分，不会被新会话的下一轮重算一次。账本行：旧会话自己记的行写在链尾活会话的 key 下（`ledgerKey`，同样逐个加锁），`job_id` 仍取旧会话自己的 key；respawn 前后是同一个 key，所以只有 respawn 之后新会话又被 rename 时才有区别：旧会话迟到的 partial / result 记在 rename 后的新 key 下，不会落在已消失的 scratch key 上。rename 后旧会话收到的读数由新会话记账，行写在新 key 下（只写一次）。
  - 重启恢复（shim reconnect，`router_shim.go:68-77`：CLI 是**同一 incarnation** 继续累计，这正是 `LastCumulativeCost` 要持久化的原因）：`router_core.go:825-833` 只恢复 USD 分量。`Metered` 基线 0 是正确的（`Process.meteringUsage` 是 naozhi 侧累加器，新 Process 对象归零）；`Models` 基线未知，若全额记入首 turn 会虚高，故 **restore 后首个 turn 的 `Models` 置空且跳过偏差 warn**（`Amount` 仍由 USD 差分保证正确），从第二 turn 起正常。不额外持久化 Models 基线。关停在保存 store 之前冻结记账（`costAccounting.freeze`，#3428）：CLI 在保存之后、Detach 之前报的 result（无主 turn，或 `ShutdownTimeout` 到期时仍在跑的 turn）不记，留给重启后同一 CLI 的下一个 result 按保存的基线差分；冻结会等已进入 `accountCost` 的记账做完（上限 1s），所以一个读数要么进了保存的基线（行也随 `runs.Close` 落盘），要么留给重启后，不会两边都记。代价是这段花费归到重启后下一个 run id 名下；CLI 在 naozhi 停机期间死掉时这段花费丢失，与停机期间结束的 turn 同理。
- `costMu` 保持叶子锁：其内只做差分与原子存储，**不得调用任何外部方法**（`ledger.Append`、slog 均在锁外）。
- `finishRun` 拆为两段：
  - `accountTurnCost(result *cli.SendResult) (deltaUSD float64)`：**无 `rt==nil || runStore==nil` 门控**（修 P4）。在 `costMu` 内：构造 `raw := Cumulative{USD: result.CostUSD, Models: result.ModelUsage, Metered: proc.MeteringUsage() 按 Unit}`，`d, next := costledger.Delta(raw, s.lastCumulative)`，累进 `costSpent`，存 `next`；锁外若这次读数不在 cost window 内（§5.0）则 `ledger.Append(entryFrom(d))`。`Models` 上限 16，超出截断 + warn。
  - `persistRun(rt, result, err, deltaUSD)`：原 runhistory 逻辑，保留门控，`SessionRun.CostUSD = deltaUSD`（兼容）。
  - 没有存活调用方消费的 result 经 `SetOnUnownedResult` 同样差分记一条 `Kind=turn`：CLI 自己发起的 turn（后台任务通知，#3096），以及 Send 先放弃（ctx 取消、cron deadline）后才到达的 result（#3322；落在 cron 窗口关闭之后，所以 cron key 上也由会话记账，行带该 job 的 `JobID`（§4），#3401；readLoop 在 Ready 或 Send 已放弃时记，下一次 Send 丢弃的陈旧 result 也记；passthrough 的 head slot 已取消或 orphan 时由 fan-out 记，取消前已投递未读的由 awaitSlot 收回后记；重复记账因累计差分无害）。没有 run 记录与它共用 `RunID`，所以写成 `unowned:<cli-session-id>:<id>`（取 result 帧的 session id，缺省取会话持有的；都没有时为裸 id），reconcile 据此归属。
- Unit 选择：claude → `USD`，Kind=`turn`；kiro/codex → 按 `Metered` 中有增量的 Unit 各出一条 entry（`credits` / `tokens`），Kind=`metering`。**`proc.MeteringUsage()` 是进程级累计视图**（`cli/process.go:668-691`），必须差分，不能直接取值。
- Basis：本 turn 有增量的 model 的 `costBasis` 取最差档（unknown > managed > list），缺省 `list`；首次遇到 `unknown` 的 model 名 warn 一次（内存去重 map，上限 64）。
- 新增 `ManagedSession.CostTotals() costledger.Totals`：返回 `{USD: costSpent, Metered: 各 Unit 累计, Models: 各模型累计 delta 和}`（monotonic，跨 incarnation）。
- `BeginCostWindow` / `EndCostWindow`（§5.0）：窗口只收本会话 `accountCost` 自己差分出的增量；`addSpent`（partial、successor 转发、respawn 补账）只进 costSpent，不进窗口。重复 Begin 会 warn 并重开空窗口；End 幂等。

### 5.3 cron 本地 run：与 session 同源（修 P2）

- `cron.Session` 的可选能力 `cron.CostWindow`（`BeginCostWindow()` / `EndCostWindow() costledger.Increment`，wireup adapter 转发）；没有它的会话（测试桩）退回可选的 `CostReporter`（`CostTotals()` 前后差分），两者都没有记 0。
- `execSend` 在 Send **前** 开窗口、Send **返回处立即** 关窗口（经 adapter 持有的 `*ManagedSession` 指针，**不查 router**：success/error 路径在 finishRun 之前都会 `router.Reset(key)` 或释放进程，按 key 查会读空），再加一个 defer 兜底，Send panic 也会关窗口；增量随 `runOutcome.costInc` 传给 finishRun。窗口在 Reset / 释放进程之前关闭，所以被杀进程的 partial 由会话记账（§5.6）。cron run 之间由 per-job CAS gate 互斥；同一 cron session 上来自 dashboard 的手动 turn 只靠 `sendMu` 串行，在 cron 关窗口前报出 result 的会计入该 run（可接受：与 run 共享进程上下文），其余按 §5.0 以 `Source=session` 入账。leak-recovery 两回合都在 Send 内。
- 窗口关闭后才记到的花费（迟到 result、partial）只出现在账本的 session 行里，不回写已经落盘的 `CronRun.CostUSD`：run 记录在终态写一次，账本是权威总额。
- `finishRun` 写 `CronRun.CostUSD = delta.USD`（**语义从累计值变为增量**；`fresh_context=true` 的 job 前后数值不变，persistent job 的历史值本来就错），并 `ledger.Append(Entry{Source: cron_local, Kind: turn, JobID, RunID, Workspace: job.WorkDir stable id, Backend: job.Backend, Unit/Amount 按 delta 分量各一条, Models: delta.Models})`。
- `cronSessionAdapter.Send` 不再透传 `r.CostUSD`；`cron.SendResult.CostUSD` 字段删除（仅 cron 内部使用，grep 确认无其他消费者后删）。
- 回归测试：persistent 模式连续两次 Send，进程累计 0.3 → 0.5，断言两条 `CronRun.CostUSD` 为 0.3 / 0.2（旧代码为 0.3 / 0.5 FAIL）；leak-recovery 双回合 cron run 记 delta 总和。

### 5.4 cron sandbox run

- `finishRun` 中 `ledger.Append` **放在 `scheduler_finish.go:304` 的持久化 if 之外**，只受 `cost.enabled` 控制（否则 `StorePath` 为空时 sandbox 成本全丢，是 P4 上移一层）；`dropOrphanRun`（`:331`）删 CronRun 时 ledger entry 保留（钱确实花了）。
- `Meta.CostUSD>0` 即入账（含 `FailedTransport`：microVM 里的 CLI 确实花了钱），`Kind=receipt`、`Basis=""`，entry 可带 sandbox state 供诊断；replay 是**重新执行**（`run.go:38` 新 RunID → 新 `RuntimeSessionID`，`sandbox.go:188`），另起 entry，不是双记，无需去重。
- Phase 2：agentcore envelope 增带 `modelUsage`，填 `Models`。

### 5.5 sysession Runner（修 P3）

- `runnerImplBaseArgs` 改 `--output-format json`（`runner.go:184` 注释、`runner_test.go:247/:263`、`protocol_claude.go BuildArgs` 侧注释三处同步改；`vision_runner.go:43-49` 的 stream-json argv 不动）。
- `Run` 解析 stdout 为 result 帧：返回 `result`（CLI schema 里是 string，纯文本，AutoTitler 既有 512 B line cap / 1 MiB excerpt cap 防御照常生效）。`Runner` 接口扩为 `Run(ctx, prompt) (text string, cost RunCost, err error)`（`RunCost{USD, ModelUsage}`），**写 ledger 的是 `Manager.runOnce`**（`manager.go:450` 持有 runID，也是 run owner），`Source: sysession, Kind: turn, SessionKey: "sys:<daemon>", RunID: runtelemetry RunID, Backend: "claude"`。
- `runnerStdoutCapBytes` 64 KiB → 256 KiB（JSON 信封 + `permission_denials` 等约 2–4 KiB）；解析失败（截断/非 JSON/`is_error`）**返回 error**（不能把 JSON 垃圾当标题回给 daemon）+ `SysessionRunnerParseFailTotal` 计数 + warn。
- 每次 Run 是独立 `-p` 进程，`total_cost_usd` 即 per-run 值，无需差分。

### 5.6 P5（无 result turn）— Phase 3

- `cli.Process` 在每个 assistant 帧的 `message.usage`（`model` 一并记录）上累计"影子 token 账"（`ShadowUsage`），result 帧到达即清零（其 `modelUsage` 覆盖同一批 token）。
- 记账点是进程结束，不是 Send 的错误（#3210）：`cli.Process.SetOnEnd` 在 readLoop 退出时恰好投递一次 `ProcessEnd`（接管时刻、最后一个 result 帧的接收时刻、结束时刻、session id、影子账；晚绑定的 hook 也能收到，rename 重绑不会重投）。会话在有界 goroutine 上为窗口 (max(接管时刻, 最后 result), 结束时刻] 写一条 `Kind=partial`、每个规范模型一行 `Models[]` 的 entry（只差 `[1m]` 后缀的 raw id 合并为一行，`raw_model` 取先出现的那个；`workspace` 与 `Kind=turn` 行同规则，无目录的会话记空串）（金额按下一条估算）；没有 run 记录与它共用 `RunID`，所以 `RunID` 写成 `end:<cli-session-id>:<id>`，带上这笔花费所属的 CLI session id（passthrough 进程自己不知道 session id，取会话持有的那个）。claude 的用量读 transcript：主 JSONL 加 `subagents/agent-*.jsonl` 与 `subagents/workflows/*/agent-*.jsonl`，按 `message.id` 去重、各字段取最大；主 JSONL 从 spawn 前记下的文件大小读起，子文件按 mtime 过滤。读不到 transcript（还没有 session id、非 claude backend）退回影子账（只含主循环）。这样 Send 看不到的中断也记账：stuck_running / pid_gone 杀掉的无主 turn（后台任务、workflow）、`/new` 的 `ErrSessionReset`、进程自己退出。Detach（naozhi 重启，CLI 继续跑）不记；socket 断开而 shim 进程仍活着（没收到 `cli_exited`，也不是 naozhi 的 Kill / Close）同样不记（`ProcessEnd.ShimLive`）：reconcile 循环会重新接管同一个 CLI，它下一个 result 的累计会报这批 token。partial 一律由会话记一条 `Kind=partial`，cron 窗口开着时也不计入窗口（#3401），所以 cron key 上被 Reset / 释放 / 杀掉的进程同样记账。`finishRun` 不再记 partial，一次中断只记一次；窗口从最后一个 result 起算，与 result 的累计差分不重叠。
- 金额按 CLI 实测单价估算（#3210，取代原"不估算、`Amount` 保持 0"）：naozhi 不维护价表，`costledger.RateBook` 从 CLI 自己定价过的 `Kind=turn` 行（`Models[].cost_usd` 对其 token）按模型学习单价。模型键去掉 provider 前缀（`global.`/`us.`/`anthropic.` 等）、`[1m]` 后缀、Bedrock `-vN:M`、Vertex `@日期` 与 `-YYYYMMDD`。同一模型累计 ≥8 行时对 input / output / cache_read / cache_write 四列做最小二乘，四个单价都非负且拟合总额与实测总额相差 ≤5% 才采用（CLI 的 costUSD 是 token 的线性函数，拟合能还原价表；opus-5-5 的 cache_read 是 input 的 0.05×，固定比例会偏 ~17%）；否则按固定比例 1 / 5 / 0.1 / 1.25 把实测总额摊到 token 上。启动时从最近 30 天（`cost.rollup_days` 更小时取其值：播种复用 rollup 预热的扫描）的日分片播种，此后每个 turn 的 `inc.Models` 实时喂入；`Kind=partial` 行自身的估算不回灌。
- partial entry 的 `Amount` 与每行 `cost_usd` 取估算值，`basis` 取学习样本里最差的那个；从未见过的模型只记 token、`basis` 留空（`unknown` 专指"CLI 按默认模型猜价"，这里没有任何定价），并按模型 warn 一次。估算额计入会话 `costSpent`，不计入 run-history `CostUSD`：记账发生在进程结束后，那时 run 记录已经写完（`lastCumulative` 不动：死进程不会再报这批 token）。UI 的 `kinds.partial` 计数提示"进程中断的轮次（按 CLI 实测单价估算）"。

## 6. 存储与聚合

- 目录 `<stateDir>/cost/`（`MkdirAll 0700`），文件 `YYYY-MM-DD.jsonl`（UTC 日切，`0600`，`O_WRONLY|O_APPEND|O_CREATE|O_NOFOLLOW`，打开前 `Lstat` 拒绝 symlink），每行一 Entry。
- 写路径：`Append` 非阻塞投递到有界 channel（**cap 4096**，≈ 40 s 的 100 turn/s 突发），单 worker 批量写 + 每 1 s 或 64 条 fsync（复用 `runhistory.Store` 的 AppendAsync/DropTotal 模式 `store.go:80-128`）。满 → 丢弃 + `dropped` 计数 + 每分钟一条 warn。**任何路径不阻塞 turn**。
- 内存 rollup：**只预聚合低基数维度** `(day, unit, source, backend, model, basis)`；`session_key / job_id / workspace / run_id` 维度的查询走窗口内日分片顺序扫描（700 KB/天 < 10 ms）。启动加载最近 `rollup_days`（默认 = `retention_days`，即整个保留期：低基数日聚合体积极小，全窗口 summary 不必扫文件）。
- 查询窗口：默认最大 **90 天**；`retention_days` 全窗需 `allow_full_range=1`。全窗口低基数 summary 走内存 rollup（覆盖整个保留期），高基数维度才扫日分片。
- 保留：`retention_days` 默认 400，启动 + 每日 sweep 删过期分片。
- 容量：2000 turn/天 × 350 B ≈ 700 KB/天，1 年 ≈ 250 MB；rollup 内存 O(models × days) < 1 MB。
- 崩溃一致性：尾部半行加载时跳过并 warn（同 eventlog）。

## 7. API

`GET /api/cost/summary?from=<RFC3339>&to=<RFC3339>&group_by=source|backend|model|job|session|workspace|day|basis[&session_key=|job_id=|workspace=]`

```json
{"from":"...","to":"...",
 "buckets":[{"key":"claude-fable-5-1","unit":"USD","amount":12.34,"entries":87,
             "tokens":{"input":..,"output":..,"cache_read":..,"cache_write":..}}],
 "basis":{"list":80,"managed":0,"unknown":7},
 "kinds":{"turn":84,"receipt":3},
 "dropped":0}
```

- 同一 `key` 不同 `unit` 是不同 bucket；前端按 unit 分别渲染。
- `GET /api/cost/entries?session_key=|job_id=|run_id=&from=&to=&limit=`：明细，调试/审计用。
- 校验（对照 `dashboard/cron/handlers.go:36-99 validateStringField`）：`from/to` RFC3339 解析失败 → 400，`to<from` → 400，跨度 > 90 天且无 `allow_full_range` → 400；`group_by` 白名单；`limit` ∈ [1,1000] 默认 200；`session_key/job_id/run_id/workspace` 长度 ≤256、合法 UTF-8、禁 C0/DEL、禁 log-injection runes；`dropped>0` 时响应加 `"note":"amount may be underestimated"`。
- 鉴权走既有 `auth()`；挂 `listLimiter`。可见性与 `/api/sessions` 同级（naozhi 单租户；`session_key` 含平台用户 id 与现有 sessions API 暴露面一致，不新增）。

## 8. Dashboard

- 服务概览「花费」卡：改读 `/api/cost/summary?from=<30d>&group_by=unit`（按单位分桶即够用），USD 主数字，credits 有值另起一行；hover 标注 "CLI 估算口径，非账单 / 含 N 条未知定价 / 账本丢弃 N 条"；`unknown>0 || dropped>0` 显示 ⚠；账本未加载前回退到 live session 求和并标注「累计花费」。
- cron job 详情：新增 per-job 30 天聚合（`group_by=job` 或 `job_id=` 过滤），时间轴"已加载 run 之和"小字保留（口径不同，文案已区分）。
- session header run-stats 不变。
- 前端契约测试（`static_ux_contract_test.go` 模式）锁定 unit 不混算。

## 9. 兼容与迁移

- 全部既有字段保留：`SessionRun.CostUSD`、`CronRun.CostUSD`（语义→增量，见 §5.3）、`SandboxRunMeta.CostUSD`、`SessionSnapshot.TotalCost`。
- `cli.Event`/`SendResult` 只增字段。`cron.SendResult.CostUSD` 删除（内部字段）。
- 配置（全部有默认值，越界 warn + clamp 不 fatal）：
  ```yaml
  cost:
    enabled: true        # false = 不写 ledger，/api/cost/* 返回 503
    retention_days: 400  # [1, 3650]
    rollup_days: 400     # [1, retention_days]，默认 = retention_days；启动同步加载，慢于 1s 会打 Info 日志
  ```
- 历史回填 `naozhi cost backfill [-config] [-dry-run]`：从 `session-runs/` 与 cron `runs/` 导入 `Kind=backfill`（TS=StartedAt，以 run_id 去重，超出保留期跳过）；persistent 模式 cron 旧记录是累计值，**跳过不导**；导入落在过去日期文件，需重启 naozhi 刷新内存 rollup。
- 对账 `naozhi cost reconcile [-config] [-session <cli-session-id>] [-until YYYY-MM-DD] [-claude-dir] [-write]`（#3210）：修 #3097 之前每次 `--resume` 把恢复的 cost-state 总额记进首个 turn 的历史条目，并补上从未入账的花费。**默认只打印**，`-write` 才追加；只追加 `Kind=adjust`，不改已有行，与运行中的 naozhi 并存的理由同 backfill。
  - 归属：只看 `Source=session`、claude、USD、非 cron key 的条目。CLI session 依次取 `RunID` 自带的（`end:<sid>:…` / `unowned:<sid>:…` / `reconcile:<sid>:…`）、同 `RunID` 的 session-runs 记录、只对应过一个 session id 的 key（session-runs 记录与 sessions.json 的 `prev_session_ids` 链合并后仍只有一个）；key 对应过多个 session 时，`Kind=turn` 条目（result 一到就记，这一轮的消息落在同 key 上一条非 adjust 条目之后、它之前）按这些 session 的 transcript 时间归属：窗口取 (同 key 上一条非 adjust 条目的 TS，没有则不设下限, 条目 TS+5s]，只有一个 session 在窗口里有消息（终端交互消息不算）时归它。候选 session 的 transcript 也记着它在别的 key 下（如 dashboard 历史恢复的 `r<hash>` key）跑的轮次、fork 复制的父会话行、以及下一个 session 落在 5s 余量里的首条消息，所以窗口里有两个及以上 session 的消息、都没有消息、或其中某个 session 的 transcript 读不全时不猜。session-runs 记录没写 `session_id` 的条目（#3411 之前新会话首轮的记录都这样，key 删掉后别处也查不到），按 transcript 的起头归属：候选是 `session-ids.json` 里的 session（只读，损坏时不挪走），起头取 transcript 第一条带时间戳的行，第一条带 `entrypoint` 的行是 `cli` / `claude-vscode` 的不算；只有一个候选的起头落在该 run 的 [`started_at`−5s, `ended_at`+5s] 里，且这个起头不同时落在另一条没写 `session_id` 的 run 窗口里（对方 transcript 已不在、或并发的首轮）时才归它。有候选的起头读不出来（transcript 在窗口开始后还写过）时也不猜。这类记录的起止时间也照常用于下面的轮次窗口和"首条记账前的历史"判断。都不成立的计数后跳过，并记下它所在的 (key, 日)。
  - 恢复额：cost-state 行没有时间戳，取它后面第一条带时间戳的行作为"之后的进程才能恢复它"的时刻。条目之前最后一个满足此条件的 cost-state 总额记为 r（被杀的进程不写 cost-state，连续几次 resume 会恢复同一行，所以之后每个条目都是候选）。条目自己这一轮的 transcript 用量取窗口 (max(上一轮的终点, cost-state 之前最后一个时间戳), 条目 TS]，不含终端交互消息，adjust 条目不算轮次；起点不早于 cost-state，是因为写它的进程在上一个条目之后跑的用量已在 r 里。`Kind=backfill` 的 TS 是 run 的开始，窗口改取 (TS, session-runs 记录的 `ended_at`]，没有结束时间时取到下一个非 adjust 条目的 TS（可能把下一轮也算进来，只会少标），下一轮也从这个 backfill 的 TS 起算；后面没有条目可作边界时不标记，计入"无法判定"。轮次终点：turn 是条目 TS，backfill 是 `ended_at`。条目满足 `Amount ≥ 0.98·r`、`r ≥ $0.5`，且比这一轮用量多出至少 r 的一半（基线为 0 的差分多出约 r，修好基线的 turn 约为 0；取中点是因为窗口里可能有没有条目记过的花费、条目里也可能有 transcript 看不到的花费，实测前者可达 r 的三成），就追加一条 `Amount=−r`、TS 与 key 同原条目的修正，`RunID=reconcile:<sid>:run:<原 run_id>`，已存在则跳过。`Models[]` 按 `costledger.RateKey` 把 r 的每个模型行与原条目的模型行配对取负，每行最多取到原条目该行的量；原条目或 cost-state 有一方没有 token 时改比金额（这一轮用量按 RateBook 定价）；这一轮有未学到单价的模型时不标记，计入说明里的"无法判定"条数。原条目没有 `Models[]` 时修正也不带。
  - 按天残差：对每个 UTC 日（不含 `-until` 当天及以后，默认今天；不早于账本最早一天），transcript 花费 T 取主 JSONL、`subagents/agent-*.jsonl` 与 `subagents/workflows/*/agent-*.jsonl` 按 `message.id` 去重后的用量，按 §5.6 的 RateBook 定价（样本为账本里所有 `Kind=turn` 行加各 session 的 cost-state 行）。账本同日合计（含上一步的修正）记为 L。`|T−L| > max($1, 5%·T)` 时追加一条 `Amount=T−L`、`RunID=reconcile:<sid>:day:<日期>`、TS 为当日 12:00 UTC 的修正，`Models[]` 是逐模型的 transcript 减账本。重跑时 L 已含上次的修正，所以不再追加。那一天若有归不到任何 session 的条目落在这个 session 已持有的某个 key 上（即前一步归属失败并记下的 (key, 日)），当天残差整个跳过不记——它的花费可能已经在这份 transcript 里，又已经在账本里，残差会把它再记一次。
  - 只记能证明是 naozhi 且没记在别处的花费（#3320）：`entrypoint` 为 `cli` / `claude-vscode`（交互终端，即被接管前的终端轮次）的消息不计入 T。以下情况当天残差跳过不记，只在 dry-run 表格里报天数：有消息在同一天里之后没有这个 session 的条目（记它的条目在别的日子——跨零点或跨 `-until` 的轮次——或者还没记账），记它的那一天也一起跳过；有消息早于这个 session 第一条条目所在 run 的开始（session-runs 记录的 `started_at`，没有记录时取该条目时间），即 resume 前的历史；当天有这个 CLI session 的 cron run（cron run 记录的 `session_id` 与起止时间，cron 已按自己的 Source 记过）；有消息的 `entrypoint` 既不是 `sdk-cli` 也不是交互终端（来源不明）。当天 transcript 没有可计入的消息（没有消息，或只有交互终端消息）而账本有条目时也跳过（#3302）：没有证据说明这些条目记错了，按 T=0 冲平会把 transcript 里已经找不到的花费一并抹掉；账本当天合计为负时照常补到 0（花费不会是负数，补正只会更接近真实值）；transcript 有消息、只是比账本少的日子照常记负残差。`-session` 也按全量顺序读完所有 session，fork 从父会话拷来的消息仍归父会话。
  - 跳过：找不到 transcript、子 agent 文件超过单次读取上限（用量不全）、当天有没学到单价的模型（只跳过那一天）。同一条消息只算给最早出现的 session（fork 会复制父 session 的行）。
  - 修正落在过去日期文件，需重启 naozhi 刷新内存 rollup；会话侧的 `costSpent` 不跟着改。

## 10. 可观测性

- slog：ledger append 失败 / dropped（每分钟聚合一条 warn）；`costBasis=unknown` 首见 model 名 warn（去重）；`Models` 之和偏差 warn（去重）；sysession JSON 解析失败 warn + 计数。
- metrics：`CostLedgerAppendTotal / DroppedTotal / SysessionRunnerParseFailTotal`。
- `/api/cost/summary` 带 `dropped`；doctor 面板：今日 entries、dropped（>100 红色）。
- 每条 entry 有 run_id，可与 runhistory / cron run / runtelemetry 双向核对。

## 11. 风险与回滚

| 风险 | 缓解 |
|---|---|
| `accountTurnCost` 在 sendMu/costMu 内新增 map 差分 | Models ≤16 × 8 字段；基准测试锁定 P99 < 5 µs；Append 非阻塞 |
| 双记 / 漏记 | run owner 原则：cron 窗口与会话记账在同一段 `costMu` 内二选一；集成测试：Send 内的 result 只产生 cron_local entry、无 session entry；窗口外的迟到 result 与 partial 只产生 session entry；并发读数下窗口增量 + session 行 = 最终累计；leak-recovery 两回合和 = cron delta |
| cron 前后差分被并发 turn 污染 | cron session 由 CAS gate 独占；测试锚定 |
| sysession 改 json 破坏 daemon | 解析失败返回 error（不回垃圾文本）+ 计数；`runner_test.go` 三态（正常 / 截断 / is_error）|
| `CronRun.CostUSD` 语义变化 | fresh job 数值不变；PR-2b 单独可回滚；ledger 为权威 |
| 磁盘增长 | retention sweep + 容量估算 |
| 回滚 | `cost.enabled=false` 即停写；代码回滚不影响既有存储格式 |

## 12. 测试策略

- 单测：`costledger.Delta`（乱序 / 新 model / 新 Unit / 字段回退 / 16 上限）；`Append` 拒空、枚举校验、字符串消毒；rollup / retention / 半行恢复 / symlink 拒绝；summary 每档 `group_by` + unit 分桶不混算 + 窗口上限；`TestPackageIsLeaf`。
- 集成：session finishRun → 1 条 entry；runStore=nil 时 costSpent 仍累进（P4 回归，旧代码 FAIL）；cron persistent 两次 run delta（P2 回归，旧代码 FAIL）；cron 窗口内的 turn 不写 session ledger、窗口外的迟到 result 与 partial 写 session ledger；sandbox 三种 outcome 各 1 条（CostUSD>0 时）、replay 另起 1 条；dashboard 对 cron key 在 run 窗口外发送 → `Source=session` entry；kiro metering 差分（两 turn 各 2 credits → 两条 amount=2，旧逻辑第二条会是 4）；sysession json 三态；leak-recovery 拷贝 ModelUsage。
- 基准：`BenchmarkReadEvent_NonResultFrame` allocs 不变；`BenchmarkAccountTurnCost_16Models`。
- 前端：Home 卡 / cron per-job 聚合契约测试。
- 手工：线上升级后对比 `summary(24h)` 与 `sessions.json` costSpent 增量一致；kiro 实机确认 `_kiro.dev/metadata` 先于 `session/prompt` 响应到达（否则 Metered 差分错位到下一 turn，需在 PR-2a 改为 result 后再读一次）。

## 13. 分阶段落地

| Phase | PR | 内容 | 修复 |
|---|---|---|---|
| **0** | PR-1 | `cli.Event/SendResult.ModelUsage` + leak-recovery 拷贝 + 基准；`internal/costledger`（entry/Delta/store/rollup/summary/leaf 测试） | P1 |
| **0** | PR-2a | session：`accountTurnCost` 拆分 + `lastCumulative` + `CostTotals()` + ledger 写入（**含 cron key，暂以 `Source=session` 入账**）+ kiro/codex 差分 | P4 P6 P7 |
| **0** | PR-2b | cron：`CostTotals` 前后差分 + `CronRun.CostUSD` 改增量 + cron_local/cron_sandbox 写入 + **同一 PR 内注入 `ownedByRun` 门**（门与写入同进同退，避免 2a→2b 之间或 2b 回滚期间 cron 成本真空；#3401 以 cost window 取代该门）+ 删 `cron.SendResult.CostUSD` | P2 |
| **0** | PR-3 | `/api/cost/summary|entries` + 服务概览卡 + cron per-job 聚合（config 已随 PR-2a 落地） | P9 P10 |
| 1 | PR-4 | sysession json runner + 写入（Runner 经 ctx `RunInfo` 归属到 Manager 的 runID） | P3 |
| 2 | PR-5 | `naozhi cost backfill`；rollup 覆盖整个保留期（取代月度 rollup 文件）；agentcore 回执带 modelUsage；服务概览健康条 dropped/unknown 告警 | — |
| 3 | PR-6 | 5.6 影子 token 账（进程死亡的 turn 记 `Kind=partial` entry，按 CLI 实测单价估算金额；服务概览健康条提示 partial 计数） | P5 |
| 3 | PR-7 | `naozhi cost reconcile`：`Kind=adjust`（可为负）冲回 `--resume` 恢复额，按天补齐 transcript 与账本的残差（§9） | — |

每个 PR 可独立合并与回滚：PR-1 纯新增；PR-2a 纯新增写入；PR-2b 改 cron 口径（回滚回到累计值，ledger 仍为权威）；PR-3 纯读。

## 14. v1 → v2 评审吸收记录

| 来源 | 级别 | 结论 | 处理 |
|---|---|---|---|
| Go | B1 `CostDeltaUSD` 回填不可行 | 部分成立（`result` 实为指针可回填，但 leak-recovery 重建对象会丢） | 整体放弃回填与 ctx，改 §5.0/§5.3 前后差分 |
| Go / 架构 | B2 门控悖论 | 成立 | §5.2 拆 `accountTurnCost` / `persistRun` |
| Go / 架构 | B3 kiro metering 语义 | 成立：`proc.MeteringUsage()` 是累计（`process.go:668-691`） | §5.2 纳入 `Cumulative.Metered` 差分 |
| 架构 | 未评 runtelemetry 方案 | 成立 | §3 方案 E |
| 架构 | ctx attribution 在 `execSend` 已实际丢失 | 成立 | 放弃 ctx |
| 架构 | leak-recovery 双 finishRun + cron 口径分裂 | 成立 | §5.0 run owner + 前后差分天然覆盖两回合 |
| 架构 | sandbox 写入被持久化门禁吞掉 / FailedTransport+replay 双记 | 成立 | §5.4 |
| 架构 | `Models` 之和 == `Amount` 不可维护 | 成立 | §4 改 warn 容差 |
| 架构 | Basis 混两维度 / 缺 Workspace | 成立 | §4 拆 `Kind` / 加 `Workspace` |
| 架构 | PR-2 不可独立回滚 | 成立 | §13 拆 2a/2b |
| 架构 | sysession 改 json 撞 `runner_test.go` 契约 + 64 KiB cap | 成立 | §5.5 |
| 架构 二轮 | `after` 须在 Send 返回处经指针读（Reset 先于 finishRun） | 成立 | §5.3 |
| 架构 二轮 | FailedTransport 也真花钱，replay 是重新执行非双记 | 成立（推翻 v2 §5.4） | §5.4 |
| 架构 二轮 | 2a/2b 之间 cron 成本真空 | 成立 | §13 门移到 2b |
| 架构 二轮 | sysession 写入者应是 `Manager.runOnce` | 成立 | §5.0 / §5.5 |
| Go 二轮 | "cron session 无并发 turn" 不成立：dashboard 可对 cron key 发消息（`server/send.go:407`），窗口外 turn 会漏账 | 成立 | §5.0 cost window（原 `ownedByRun` 门持有期长于 cron 的测量区间，迟到 result 与 deadline 的 partial 两边都不记；#3377 记下的这处"略变差"由 #3401 解决） |
| Go 二轮 | 重启恢复理由错（是 shim reconnect 同 incarnation）；Models 基线未知会虚高 | 成立 | §5.2 |
| Go 二轮 | `costMu` 内不得调外部方法 | 采纳 | §5.2 |
| Go 二轮 | kiro metadata 与 `session/prompt` 响应先后顺序仓内无样本 | 待实测（非阻塞） | §12 手工项 |
| 架构 | 差分函数与类型分居两包 | 成立 | 全部收进 `costledger`，session 侧做 cli→costledger 转换 |
| 性能 | 非 result 帧 map 分配 | **不成立**（`encoding/json` 键缺失不分配）但需基准锚定 | §5.1 基准 |
| 性能 | 差分无上限 | 成立 | §4 Models ≤16 |
| 性能 | channel 1024 太小 / 高基数 rollup / 400 天扫描 | 成立 | §6 cap 4096、低基数预聚合、90 天默认上限 |
| 安全 | API 校验、字符串消毒、枚举、文件权限、config 范围、warn 去重 | 成立 | §4 / §6 / §7 / §9 / §10 |
| 安全 | 多租户隔离 | 单租户，与 `/api/sessions` 同级 | §7 注明 |
| 事实 | `carryOverIdentity` 不存在 → `RenameSession`；`SandboxRunMeta` 在 `sandbox.go:88`；P10 无"月花费"概念 | 成立 | §1.1 / §1.3 / §5.2 已改 |
