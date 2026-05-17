# Cron 执行历史与生命周期可见性 — 设计 RFC

> **状态：设计提案（待评审 → 实施）**
> **日期：2026-05-17**
> **作者：naozhi**
>
> 关联现状代码：
> - `internal/cron/{job,scheduler,store}.go`
> - `internal/server/dashboard_cron.go`
> - `internal/server/wshub.go`（`cronResultMsg` / `BroadcastCronResult`）
> - `internal/server/static/dashboard.js`（cron 面板）
>
> 关联 RFC：
> - `cron-v2-polish.md` — Increment A-E（Title/jitter/missed/sort/next-run）；其 §1 非目标节明确"Cron 日志归档 / 运行历史"留给后续 RFC，本文承接。
> - `event-log-persistence.md` — session 维度的事件持久化；本 RFC 不重复，CronRun 通过 SessionID 借用之。

---

## 0. 动机

当前 cron 模块只持久化最后一次执行的快照（`Job.LastResult` / `Job.LastError` / `Job.LastRunAt`），缺失运行时与历史可见性：

1. **不知道任务正在跑** — 调度器内部用 `runningJobs sync.Map[*atomic.Bool]` 做 CAS 去抖，外部 API/UI 完全无感知。
2. **历史只剩一条** — `recordResult` 直接覆写 LastResult；上一轮的输出没有任何方式找回。
3. **错误分类丢失** — `session_error` / `send_error` / `deadline_exceeded` / `canceled` / `workdir_unreachable` / `overlap_skipped` 全部塞进 `LastError` 字符串，UI 无法区分着色或统计。
4. **跳过事件静默** — `SkipIfStillRunning`、`jobRunningGuard` CAS=false、freshContext preflight 失败、shutdown 期间 fresh 抢跑被取消 — 全部只走 slog。
5. **触发来源不可见** — scheduled tick / TriggerNow / missed catch-up 在 UI 上不可区分。
6. **fresh=true 模式的多 session 历史无导航** — 每次 Reset 产生独立 `session_id` 与独立 JSONL 文件，dashboard 只能看到最近一条。

## 1. 非目标

- ❌ 调度器替换 / 分布式 cron（保留 robfig/cron v3）
- ❌ 失败重试策略（独立 RFC）
- ❌ 跨节点 cron
- ❌ 把 session events 全量复制到 CronRun（events 仍由 session event-log + JSONL 提供）
- ❌ Cron 模板 / 克隆（cron-v2-polish 的延续，非本 RFC）

## 2. 核心抽象：四层标识

历史展示策略与 `FreshContext` 选项强耦合，先把四层身份梳理清楚——后续所有字段定义都建立在这上面：

```
Job (jobID="abc123")             永久标识，Job 生命周期
   │ 1:1
   ▼
router key  "cron:abc123"        session 路由键，Job 级
   │ fresh=false → 1:1 共享
   │ fresh=true  → 1:N 每跑一次
   ▼
ManagedSession                   一次 CLI 子进程实例
   │ CLI 启动时 Claude 分发
   ▼
claude session_id                CLI 会话身份
   │ 严格 1:1
   ▼
~/.claude/projects/<cwd>/<session_id>.jsonl
```

### 2.1 fresh=false（保留上下文）

```
T0  spawn CLI → session_id=AAA   JSONL: AAA.jsonl  ← user1, assistant1
T1  复用 CLI                     JSONL: AAA.jsonl  ← + user2, assistant2
T2  复用                          JSONL: AAA.jsonl  ← + user3, assistant3
```

**1 Job ↔ 1 session_id ↔ 1 JSONL（不断 append）**。Claude 看到累积对话；多次 cron run 在同一份 JSONL 里靠时间窗口区分。

### 2.2 fresh=true（每次重置）

```
T0  spawn CLI #1 → AAA            JSONL: AAA.jsonl
T1  Reset → 杀 #1 → spawn #2 → BBB JSONL: BBB.jsonl   (独立)
T2  Reset → 杀 #2 → spawn #3 → CCC JSONL: CCC.jsonl
```

**1 Job ↔ N session_id ↔ N JSONL**。每条 CronRun 自带独立 SessionID，是定位独立 JSONL 的天然主键。

### 2.3 设计影响（关键）

| 关注点 | fresh=false | fresh=true |
|---|---|---|
| `CronRun.SessionID` | 大量 run 共享同一值，单字段无信息量 | 每条 run 独有，主键级 |
| 定位"第 N 次跑产生的 events" | 时间窗口 (StartedAt..EndedAt) 切共享 JSONL | 直接 SessionID 打开独立 JSONL |
| 详情页"输入/输出"展示 | 必须靠 CronRun.Prompt/Result 自身（不能完全委托 JSONL）| 也靠自身存的字段 + JSONL 任选 |
| Prompt 漂移 | 必须 snapshot — 用户改 Job.Prompt 后回看不混 | 同样必须 snapshot |
| 删除 CronRun 时是否连带删 JSONL | 不必（JSONL 共享）| **不要** — JSONL 用户可能 `claude --resume` 复活 |

## 3. 数据模型

### 3.1 `CronRun` 实体

```go
package cron

type RunState string

const (
    RunStateRunning   RunState = "running"
    RunStateSucceeded RunState = "succeeded"
    RunStateFailed    RunState = "failed"
    RunStateSkipped   RunState = "skipped"
    RunStateTimedOut  RunState = "timed_out"
    RunStateCanceled  RunState = "canceled"
)

type TriggerKind string

const (
    TriggerScheduled TriggerKind = "scheduled"
    TriggerManual    TriggerKind = "manual"   // TriggerNow
    TriggerCatchup   TriggerKind = "catchup"  // 未来 missed 重跑（保留位）
)

type ErrorClass string

const (
    ErrClassNone               ErrorClass = ""
    ErrClassSessionError       ErrorClass = "session_error"
    ErrClassSendError          ErrorClass = "send_error"
    ErrClassDeadlineExceeded   ErrorClass = "deadline_exceeded"
    ErrClassCanceled           ErrorClass = "canceled"
    ErrClassWorkDirUnreachable ErrorClass = "workdir_unreachable"
    ErrClassWorkDirOutsideRoot ErrorClass = "workdir_outside_root"
    ErrClassOverlapSkipped     ErrorClass = "overlap_skipped"
    ErrClassPausedConcurrent   ErrorClass = "paused_concurrent"
    ErrClassPanic              ErrorClass = "panic"
)

type CronRun struct {
    RunID        string      `json:"run_id"`        // 16-hex，独立于 jobID
    JobID        string      `json:"job_id"`
    State        RunState    `json:"state"`
    Trigger      TriggerKind `json:"trigger"`

    StartedAt    time.Time   `json:"started_at"`
    EndedAt      time.Time   `json:"ended_at,omitempty"`     // 终态写
    DurationMS   int64       `json:"duration_ms,omitempty"`  // 终态写

    SessionID    string      `json:"session_id,omitempty"`
    Prompt       string      `json:"prompt,omitempty"`       // 触发时刻 snapshot；不裁剪
    WorkDir      string      `json:"work_dir,omitempty"`     // snapshot
    Fresh        bool        `json:"fresh,omitempty"`        // snapshot

    Result       string      `json:"result,omitempty"`       // 截到 4K rune
    ResultBytes  int         `json:"result_bytes,omitempty"` // 原始字节（在截断前）
    ErrorClass   ErrorClass  `json:"error_class,omitempty"`
    ErrorMsg     string      `json:"error_msg,omitempty"`    // 已 redact path

    Attempt      int         `json:"attempt,omitempty"`      // 同一 schedule 周期内第几次（catchup 用，先固定 1）
    Phase        string      `json:"phase,omitempty"`        // 终态省略；运行中: queued|jittering|spawning|sending
}
```

字段约束：
- `Prompt` 不裁剪：列表 API 不返回此字段，详情 API 才返回。
- `Result`：与现行 `LastResult` 同 4K rune 上限；同时存 `ResultBytes` 让 UI 显示"完整 12.3 KB（已截断到 4 KB）"。
- `ErrorMsg`：必经 `redactPathsInCronError` + `osutil.SanitizeForLog`。
- `RunID`：crypto/rand 16-hex；与 jobID 同生成器，但分开命名空间。

### 3.2 `Job` 字段不变 + 增量

`Job.LastResult` / `LastError` / `LastRunAt` / `LastSessionID` 全部**保留**，作为快速预览缓存（list API 直接读，避免每次扫历史目录）。新增：

```go
type Job struct {
    // ... existing fields ...

    // RunCounters 是每个 job 的累计计数，进程内维护，落 cron_jobs.json。
    // 用于 list API 直接给出 stats 字段，避免扫描 runs 目录。
    RunCounters JobRunCounters `json:"run_counters,omitempty"`
}

type JobRunCounters struct {
    Total     int64 `json:"total"`
    Succeeded int64 `json:"succeeded"`
    Failed    int64 `json:"failed"`
    Skipped   int64 `json:"skipped"`
    // 平均/分位通过 EWMA 维护，避免存全量样本
    AvgMS     int64 `json:"avg_ms,omitempty"`     // EWMA α=0.2
    P95MSEst  int64 `json:"p95_ms_est,omitempty"` // P²-quantile estimator (Jain & Chlamtac)
}
```

### 3.3 内存运行态：`runInflight`

替换 `runningJobs sync.Map[jobID]*atomic.Bool`：

```go
type runInflight struct {
    RunID     string
    StartedAt time.Time
    Trigger   TriggerKind
    Phase     atomic.Value // string
    SessionID atomic.Value // string；GetOrCreate 后写
    // 兼容现有 CAS gate
    running atomic.Bool
}
```

CAS 入口 `executeOpt` 把 `running.CompareAndSwap(false, true)` 替换为同语义。`runningJobs` 改为 `sync.Map[jobID]*runInflight`。`current_run` 字段从此读。

## 4. 持久化

### 4.1 目录布局

```
<data_dir>/cron/
   cron_jobs.json              # 现有，未变
   runs/
       index.json              # 全局映射：jobID → []run_id（按时间倒序，capped）
       <jobID>/
           <run_id>.json       # 单条 CronRun
```

设计取舍：
- **每条 run 一个文件** — 高频任务（5min 一次，288/天）下，jobID 目录 ~9000 文件/月；可控。append-only 文件（如 `runs.jsonl`）会引入并发写复杂度且无随机访问。
- **index.json 是 hint** — 详情查询走 `runs/<jobID>/*.json` glob 兜底；index 落后不影响正确性，加速排序与列表分页。
- **`<data_dir>` 与 cron_jobs.json 同根** — 复用 `cron.SchedulerConfig.StorePath` 的 dirname。

### 4.2 写盘契约

| 时机 | 写什么 | 同步性 |
|---|---|---|
| executeOpt CAS 成功后 | 仅内存 runInflight；**不写盘** | n/a |
| recordResult / 跳过分支 | `runs/<jobID>/<run_id>.json` 新建 + index.json 更新 + cron_jobs.json 更新（LastRunAt/LastResult/Counters）| 异步串行（与 cron_jobs.json 同 storeMu）|

为什么 started 不写盘：进程崩溃时 inflight 丢失，不是问题——下次 boot 不会"恢复"半截 run，只会按 missed-schedule 检测追溯。

### 4.3 保留策略（与用户确认：历史保留多一些）

```go
const (
    DefaultRunsKeepCount  = 200       // 每 job 保留最近 200 条
    DefaultRunsKeepWindow = 30 * 24 * time.Hour
)
```

GC 规则：**(条数 ≤ 200) AND (年龄 ≤ 30d)**。两条件取**并集**——超出任一就删。
- 高频任务：200 条上限主导（5 min × 200 ≈ 16 小时窗口）
- 低频任务：30 天窗口主导（每天 1 次跑 30 条够看）

GC 触发：
- `recordResult` 写完 run 后，对**当前 jobID**做就地 trim（小批操作，O(增量+裁掉数)）
- 启动时一次全局扫（防进程长期未启动留下的腐烂条目）
- 不需要专门定时器（避免新增 goroutine）

GC **不**触碰 `~/.claude/projects/<cwd>/<session_id>.jsonl`：
- fresh=true 模式：用户可能 `claude --resume <session_id>` 用历史 JSONL；删除会丢用户数据。
- fresh=false 模式：JSONL 始终在用，更不能删。

### 4.4 删除 Job 时

`DeleteJobByID` 当前会 `s.router.Reset(...)` 杀 session。新增：
- 同步删除 `runs/<jobID>/`（目录递归）
- 不删 JSONL

## 5. 状态机

```
                    +-------+
                    | (no)  |
                    +-------+
                        |
              executeOpt CAS guard.Try
                        |
              ┌─────────┴──────────┐
            success              failure
              │                    │
              ▼                    ▼
        [runInflight]         CronRun{State:skipped,
        Phase:queued         ErrorClass:overlap_skipped,
              │              StartedAt=now,EndedAt=now}
              │              → broadcast skipped (无 started)
        applyJitter
        Phase:jittering
              │
        snapshotJob → resolveNotifyTarget
              │
        broadcast cron_run_started
              │
        freshContextPreflight
              │       (fail: workdir_unreachable / canceled)
              │       → CronRun{State:canceled|failed, ...}
              │       → broadcast cron_run_ended
              │
        Phase:spawning
        GetOrCreate
              │       (fail: session_error | canceled | deadline_exceeded)
              │       → 终态记录 + broadcast
              ▼
        SessionID 写入 runInflight
        Phase:sending
        sess.Send
              │       (fail: send_error | canceled | deadline_exceeded)
              ▼
        recordResult (success)
              │       State:succeeded
              ▼
        broadcast cron_run_ended
        deliverNotice (IM 通知，与 broadcast 解耦)
        GC trim
```

终态共有 5 种：`succeeded` / `failed` / `skipped` / `timed_out` / `canceled`。
- `timed_out` = `deadline_exceeded` 错误（独立终态便于统计）
- `canceled` = `context.Canceled`（shutdown / job 删除）

## 6. API

### 6.1 `GET /api/cron`（增量）

返回的每个 jobView 增量字段：

```jsonc
{
  "id": "abc123",
  // ... existing fields ...

  "current_run": {                   // 仅运行中存在
    "run_id": "f1e2...",
    "started_at": 1716000000000,
    "phase": "sending",
    "trigger": "scheduled",
    "session_id": "AAA"
  },
  "stats": {
    "total": 120,
    "succeeded": 118,
    "failed": 1,
    "skipped": 1,
    "avg_ms": 4321,
    "p95_ms": 8000
  },
  "recent_runs": [                   // 最近 5 条摘要（不含 prompt/result 全文）
    {
      "run_id": "f1e2...",
      "state": "succeeded",
      "started_at": 1716000000000,
      "duration_ms": 4321,
      "trigger": "scheduled",
      "session_id": "AAA",
      "error_class": ""
    }
  ]
}
```

### 6.2 `GET /api/cron/runs?job_id=&limit=&before=`

- `limit` 默认 50，clamp 到 [1, 200]
- `before` 是 unix-ms 时间戳，分页用；不带表示最新一页
- 响应：

```jsonc
{
  "runs": [ /* CronRun 摘要数组，按 started_at desc */ ],
  "next_before": 1716000000000  // 缺失=已到尾页
}
```

### 6.3 `GET /api/cron/runs/{run_id}`

返回单条 CronRun 全字段（含 `prompt` / `result` 全文，仍受截断上限）。

### 6.4 鉴权

复用现有 `auth(...)` middleware，与 `GET /api/cron` 同条件。

## 7. WebSocket 事件

### 7.1 现状

`cronResultMsg` 单一类型 `cron_result {type, job_id, result, error}`，前端只在末尾收到一次。

### 7.2 增量

新增两个事件，**保留 `cron_result` 兼容旧客户端**：

```jsonc
// cron_run_started
{
  "type": "cron_run_started",
  "job_id": "abc123",
  "run_id": "f1e2...",
  "started_at": 1716000000000,
  "trigger": "scheduled",
  "session_id": ""               // 此刻可能尚未拿到，先空
}

// cron_run_ended
{
  "type": "cron_run_ended",
  "job_id": "abc123",
  "run_id": "f1e2...",
  "state": "succeeded",
  "ended_at": 1716000004321,
  "duration_ms": 4321,
  "session_id": "AAA",
  "error_class": "",
  "error_msg": ""
}
```

`cron_result` 继续发送（同时间触发，等价 `ended` 的 succeeded 分支）；下个版本把前端切到 `ended` 后可移除。

无 `cron_run_progress`：session 已经在推 events，详情页订阅 cron 对应的 sessions 事件即可，不重复造。

## 8. UI

### 8.1 列表行（最小增量）

`cronJobCardHtml` 增加：
- 运行中：行右侧显示 "运行中 12s" 角标（脉动动画）
- stats badge：成功率低于 80% 时高亮 `92% (118/120)` 标签
- 错误：保留现有 inline 错误条，但展示 `error_class` 友好名而非裸 message

### 8.2 详情页时间轴（新视图）

进入 cron 详情时：
- 现有的 result/error 区块保留（last 一条快速看）
- 下方新增"执行历史"时间轴：
  - 每条 run 一行：状态点 + started_at + duration + trigger + session_id 链接
  - 状态色：succeeded 绿 / failed 红 / skipped 灰 / timed_out 橙 / canceled 紫 / running 蓝脉动
  - 点击行 → 展开看 prompt/result/error；再次点击 → 跳转 session events 面板（用 SessionID）
- 分页：滚到底加载更多（before 游标）

### 8.3 fresh=false 模式时间轴里的边界标记

session events 面板里多次 cron 触发的 user 消息混在一起。在 cron 详情页打开 events 时：
- 用 CronRun 的 `(StartedAt, EndedAt)` 对应区间高亮当前选中的那次 run
- 顶栏"上次 / 本次 / 下次"快捷跳转

### 8.4 触发历史（`recent_runs`）

在 cron 列表卡片折叠态，悬浮 stats badge 出 tooltip：最近 5 条状态气泡（绿/红/灰小圆点）。

## 9. 错误分类映射

把 `executeOpt` 现有分支映射到 `ErrorClass`，**调用点固定**避免漂移：

| 现有代码位置 | 旧 LastError 文本 | ErrorClass |
|---|---|---|
| jobRunningGuard CAS=false | (无；只 slog) | `overlap_skipped` (skipped) |
| freshContextPreflight ctx.Err | (无) | `canceled` (canceled) |
| freshContextPreflight !workDirReachable | "work_dir unreachable" | `workdir_unreachable` (failed) |
| executeOpt allowedRoot 失败 | "work_dir outside allowed_root" | `workdir_outside_root` (failed) |
| GetOrCreate err is Canceled | (无；不 record)  | `canceled` (canceled) |
| GetOrCreate err is DeadlineExceeded | "session error: ..." | `deadline_exceeded` (timed_out) |
| GetOrCreate other | "session error: ..." | `session_error` (failed) |
| Send err is Canceled | (无) | `canceled` (canceled) |
| Send err is DeadlineExceeded | "send error: ..." | `deadline_exceeded` (timed_out) |
| Send other | "send error: ..." | `send_error` (failed) |
| paused 抢跑（registerJob 闭包分支）| (无) | `paused_concurrent` (skipped) |
| 兜底 panic recover | (cron lib 处理) | `panic` (failed) |

`ErrorMsg` 仍存原文；`ErrorClass` 是机器可读分类。

## 10. Metrics

新增 expvar/指标：

```go
metrics.CronRunTotal               // counter, label: state
metrics.CronRunDurationSeconds     // histogram (无 job_id label，避免基数爆)
metrics.CronRunInflight            // gauge
```

`CronExecutionSlowTotal` 保留兼容。

## 11. 实施分阶段

### P0 — 运行态可见性（独立可 ship）

不动磁盘 schema，纯内存 + WS 改动，回滚最简单：

- [ ] `runInflight` struct 替换 `*atomic.Bool`
- [ ] `executeOpt` 各阶段写 `Phase`
- [ ] `recordResult` 拆 `ErrorClass` + `ErrorMsg`（Job.LastError 不变；新加 Job.LastErrorClass）
- [ ] WS 新增 `cron_run_started` / `cron_run_ended`，保留 `cron_result`
- [ ] list API 加 `current_run` 字段
- [ ] UI 列表行 "运行中 Xs" 角标
- [ ] metrics 三件套接入

预计 ~600 行（含测试）。

### P1 — CronRun 持久化（依赖 P0 的字段）

- [ ] `internal/cron/runstore.go`：单 run 读写、index.json、GC
- [ ] `executeOpt` 终态分支写 CronRun
- [ ] `JobRunCounters` 维护（EWMA + P²）
- [ ] `GET /api/cron/runs` + `GET /api/cron/runs/{run_id}`
- [ ] DeleteJob 同步删 `runs/<jobID>/`
- [ ] 启动时 GC 全扫一遍

预计 ~800 行。

### P2 — UI 时间轴（依赖 P1 API）

- [ ] cron 详情页时间轴组件
- [ ] 分页加载
- [ ] fresh=false 边界高亮
- [ ] stats badge tooltip + recent_runs 状态气泡

预计 ~500 行。

### P3（保留位，本 RFC 不实施）

- 跨 run 关联（catchup 链）
- 批量导出 CronRun（CSV/JSON）
- Run 比较视图（diff 两次 result）

## 12. 迁移与回滚

### 迁移
- P0 / P1 / P2 各阶段独立 PR，独立可回滚。
- `cron_jobs.json` schema 增量字段（`run_counters`）：旧 naozhi 读到忽略，新版本读到旧文件零值起步——**双向兼容**。
- `runs/` 目录不存在视为空，首次 boot 创建。

### 回滚

| 阶段 | 回滚动作 | 数据影响 |
|---|---|---|
| P0 | 还原 cron 包 + dashboard.html/js + wshub.go | 无（runInflight 仅内存）|
| P1 | 还原 P1 PR；保留 `runs/` 目录 | 无（旧版本不读，下次 P1 重新生效继续累积）|
| P2 | 还原 dashboard.js | 无（API 仍可用，只是无 UI）|

### 数据兼容
- 删除 `Job.LastResult` / `LastError` / `LastRunAt` 字段：**不做**，作为快速预览缓存继续维护。
- `runs/` 目录如手动清空：list API 的 `recent_runs` 为空，stats 归零；功能性不变。

## 13. 测试策略

### 单元
- `runstore_test.go`：写读 / 索引一致性 / GC 200+1 / GC 30d+1s / 并发写 storeMu / 损坏文件容错（rename `.corrupt.<ts>`，与 cron store 一致）
- `scheduler_run_test.go`：每个 ErrorClass 分支映射到正确 (state, ErrorClass) tuple
- `runinflight_test.go`：phase 写 / Reset 后 SessionID 更新 / DeleteJob 期间 inflight cleanup

### 集成
- 模拟 fresh=true 5 次连续触发 → 5 个 SessionID + 5 个 CronRun，每个 SessionID 在 ~/.claude/projects 存在 JSONL（用 fake claude）
- TriggerNow + scheduled tick 抢跑 → 1 succeeded + 1 overlap_skipped
- shutdown 期间 fresh 抢跑 → canceled (不写错误通知)
- WS broadcast 顺序：started → ended，client subscribe 后回放最近 5 条 ended

### E2E
- dashboard 创建 cron + TriggerNow + 看到 "运行中" → 完成 → 历史时间轴出现一条 → 点击展开看 result

## 14. 公开问答

**Q1：为什么不把 CronRun 直接合并进 cron_jobs.json？**
A：高频任务一天 ~300 run × 4KB ≈ 1.2 MB；保留 200 条 × 50 jobs ≈ 40 MB，会顶到 16 MiB 上限。runs/ 分目录后单 job 自治。

**Q2：保留窗口为什么是"200 条 AND 30 天"取并集？**
A：和用户确认"历史保留可以多一点"。条数防高频任务爆磁盘；天数照顾低频任务（避免一周一次的任务一查只剩 1 条）。

**Q3：fresh=false 模式下 SessionID 全相同，CronRun 还存这字段吗？**
A：存。允许将来用户切换 fresh 后老 run 仍然知道当时关联的是哪个 session_id。

**Q4：为什么不接 SQLite？**
A：naozhi 设计原则"不引入外部组件，所有状态文件化"（CLAUDE.md）。CronRun 单文件 < 8 KB，索引 < 64 KB；纯文件方案验证够用。

**Q5：started 不写盘，崩溃后怎么知道这次 run 跑了？**
A：不需要知道。崩溃中断的 run 不算事实——下次 boot 由 missed-schedule 检测决定是否补跑。完成的 run 才进历史。

**Q6：现有 `Job.LastSessionID` 与 `recent_runs[0].session_id` 重复吗？**
A：是。`LastSessionID` 仍负责 dashboard 侧边栏 stub 注入（`registerStub`），是早于 CronRun 的快速路径；`recent_runs[0].session_id` 是历史展示路径。两者由同一次 recordResult 写入，强一致。

---

## 附录 A：现有代码改动点清单

| 文件 | P0 | P1 | P2 |
|---|---|---|---|
| `internal/cron/job.go` | RunCounters 字段 / ErrorClass 常量 | CronRun struct / RunState | — |
| `internal/cron/scheduler.go` | runInflight / Phase 写入 / executeOpt 错误分支映射 | recordResult 写 CronRun + Counters | — |
| `internal/cron/runstore.go` | — | 新增整文件 | — |
| `internal/cron/store.go` | — | DeleteJob 连带删 runs/ | — |
| `internal/server/dashboard_cron.go` | jobView 加 current_run / stats / recent_runs / error_class | runs handlers | — |
| `internal/server/dashboard.go` | mux 路由（cron_run_started/ended channel） | mux 路由（runs API） | — |
| `internal/server/wshub.go` | BroadcastCronRunStarted/Ended | — | — |
| `internal/server/static/dashboard.js` | "运行中 Xs" 角标 + ws dispatch | — | 时间轴 + 分页 + 边界高亮 |
| `internal/server/static/dashboard.html` | 角标 CSS | — | 时间轴 CSS |
| `internal/metrics/*` | CronRunTotal / Duration / Inflight | — | — |

## 附录 B：与 cron-v2-polish.md 的关系

cron-v2-polish 五项里 Title / sort / next-run UI 已实施；missed/jitter 已实施。本 RFC 不重复任何内容，只在 §13 附录 A 表格里使用 Title 字段渲染时间轴标题。

`cron-v2-polish.md §1` 非目标节明确"Cron 日志归档 / 运行历史"留给后续 RFC——本文承接。
