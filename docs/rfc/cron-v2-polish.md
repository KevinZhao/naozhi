# Cron v2 打磨 — 设计 RFC

> **状态：设计提案（未实现）**
>
> 对比 Claude Scheduled Tasks UI 与 naozhi 现有 cron 面板的差距分析，提炼出
> 5 项增量改进。每项独立可 ship、无跨项耦合、逐条带有 rollback 路径。
>
> 关联文档：
> - 现状代码：`internal/cron/{job,scheduler,store}.go`、`internal/server/dashboard_cron.go`、`internal/server/static/dashboard.js`（cron 模块）
> - 既有 RFC：`docs/rfc/message-queue.md`（cron 执行通过 session 走消息队列）

---

## 0. 动机

Claude Scheduled Tasks 面板和 naozhi cron 面板整体方向一致（任务列表 + 频率 + 结果回传），但有 5 处 Claude 的选择在 naozhi 环境下仍然成立且值得补齐：

| 借鉴点 | 待解决的 naozhi 实际问题 |
|---|---|
| 1. Name/Description 与 Prompt 分离 | 长 prompt 在卡片列表里被截得难看；搜索只能匹配 prompt 原文 |
| 2. 调度抖动（jitter） | 同 schedule 的多 job 同秒起跑导致 CPU/API 峰值 |
| 3. "missed schedule" 显式检测 | 进程重启的空窗期错过的任务用户毫不知情 |
| 4. 排序控件 | 超过 10 job 后 `created_at desc` 固定排序找不到目标 |
| 5. Next run 视觉权重 | 最常用的"下次什么时候跑"被压在一排 meta 里 |

naozhi 已经显著领先 Claude 的地方（频率选择器 Tab 式 + 多次预览、per-job fresh context、Run now 立即触发、IM 通知目标自定义、state chips 筛选、per-chat 配额、EL 表达式高级入口）不做任何退化。

## 1. 非目标

以下事项**不在**本 RFC 范围内，避免 scope creep：

- ❌ 触发器模型改造（保留 robfig/cron v3，不引入 systemd timer / distributed cron）
- ❌ 失败重试策略（当前设计是"错过即错过 + `attention` 徽章"，改重试是独立 RFC）
- ❌ Cron 日志归档 / 运行历史（当前只留 `LastResult`/`LastError` 两个字段 + session event log）
- ❌ 模板 / 任务克隆（Title 字段是铺垫，真模板单独做）
- ❌ 跨节点 cron（shim 节点只是 CLI 子进程执行端，调度仍在 master）

## 2. 现状快速回顾

### 2.1 数据模型（`internal/cron/job.go:12`）

```go
type Job struct {
    ID, Schedule, Prompt, Platform, ChatID, ChatType, CreatedBy string
    CreatedAt            time.Time
    Paused               bool
    WorkDir              string
    NotifyPlatform       string
    NotifyChatID         string
    Notify               *bool    // tri-state
    FreshContext         bool
    LastResult, LastError string
    LastRunAt            time.Time
    entryID              robfigcron.EntryID // runtime only
}
```

### 2.2 执行路径（`internal/cron/scheduler.go:1195 execute`）

1. `jobRunningGuard` CAS 去抖并发
2. 拷贝可变字段快照
3. `resolveNotifyTarget` → 算目标 IM
4. `computeJobTimeout(schedule, cap)` — 80% of period，floor 3m
5. 如果 `FreshContext` 先 `router.Reset(cronKey)`
6. `router.GetOrCreate(...)` + `sess.Send(ctx, prompt, nil, onEvent)`
7. 成功/失败记 `LastResult` / `LastError` + 推通知

### 2.3 UI 流程（`dashboard.js`）

- `openCronPanel()` → `renderCronPanel` → `renderCronList` → `cronJobCardHtml`
- `createNewCronJob()` / `editCronJob` → modal（`renderCronModalBody` 两列 grid）
- 频率选择器 4 Tab + advanced raw cron 输入 + 5 次运行 preview
- 搜索 + 3 态状态 chip
- 卡片行：prompt / human schedule / meta 行（status/wd/notify/fresh/ran/next）/ result / actions

## 3. 提案

### 3.1 Increment A — 引入 `Job.Title` 字段

#### 3.1.1 动机

当前 `Prompt` 一字段既是"UI 显示名"也是"喂给 LLM 的提示词"。两个职责冲突：

- 卡片列表显示长 prompt 被 CSS 截断，难以区分；
- 搜索只能 substring 匹配 prompt 原文，无法为同类任务起"易记别名"；
- 未来模板/克隆功能需要一个稳定的人类可读 key。

Claude 的两字段设计（Description 单行 + Prompt 多行）正好解决这三个问题。

#### 3.1.2 数据模型改动

```go
type Job struct {
    // ... existing fields unchanged ...

    // Title 是人类可读的任务名称，用于卡片列表、搜索主 key、通知标题。
    // 为空时 UI 回退到 Prompt 的首行 trim 到 60 字符，保持向后兼容。
    // 长度上限：256 字符（加 const maxCronTitleLen）。
    Title string `json:"title,omitempty"`
}
```

**向后兼容策略**：

- 旧 `cron_jobs.json` 没有 `title` 字段 → JSON 反序列化后 `Title == ""` → UI 自动 fallback 到 prompt 首行，完全无破坏。
- 新创建的 job 在 UI 层鼓励但不强制（"Title（可选）"）。强制会让 IM 路径 `/cron add` 命令变复杂，无收益。

#### 3.1.3 Scheduler API

```go
type JobUpdate struct {
    // ... existing ...
    Title *string // nil 保持原值；""  清空（回退 fallback 逻辑）
}

// SetJobTitle 是快捷路径，等价于 UpdateJob(id, JobUpdate{Title: &t})。
// 按现有 SetJobPrompt 惯例提供。
func (s *Scheduler) SetJobTitle(id, title string) error
```

#### 3.1.4 HTTP API

- `POST /api/cron/jobs`：`{...existing, "title": "..."}`（可选）
- `PATCH /api/cron/jobs/:id`：追加 `title` 可选字段
- 后端校验：长度 ≤ 256、UTF-8 有效、不含 C0/C1 控制字符（沿用 `validateCronWorkDir` 已有 rune 扫）
- 错误返回 `invalid title` + 400，统一 handler

#### 3.1.5 UI 改动

**Modal**（创建 & 编辑共用 `renderCronModalBody`）：
- 在当前"做什么 / 什么时候 / 在哪里 / 其他设置"四格 grid 之前加一行单字段 `"名称（可选）"` + `<input id="cron-title">`，横跨两列
- 编辑时回填 `job.title`
- 空值允许直接提交

**卡片列表**（`cronJobCardHtml`）：
- `promptBlock` 上方加一行 `<div class="cc-title">{title || fallbackFromPrompt}</div>`，字号 14px、加粗
- 当 `title` 存在时，`promptBlock` 保持次级（font-size: 12px、color: var(--nz-text-mute)）
- 当 `title` 缺省时，渲染老样子（保证视觉连续性）

**搜索**（`filterCronJobs`）：
- 搜索字段集从 `[prompt, work_dir, schedule, id]` 扩到 `[title, prompt, work_dir, schedule, id]`
- `title` 放最前，匹配优先级最高（视觉上就在卡片顶部）

#### 3.1.6 测试

- `scheduler_test.go`：新增 `TestAddJob_WithTitle`、`TestUpdateJob_TitleOnly`、`TestSetJobTitle_Empty_ClearsBack`
- `dashboard_cron_test.go`：POST/PATCH 携带 `title` 的验证 case（长度 / 控制字符 / 正常）
- `dashboard.js` 单元：`filterCronJobs` 的 title 匹配断言，`cronJobCardHtml` 有无 title 两路径 snapshot

#### 3.1.7 Rollback

Title 字段自带 `omitempty`，回滚只需把相关代码删掉，`cron_jobs.json` 里的 title 字段会被 Go 默认 JSON 解码器忽略（unknown field 默认不报错）。无迁移负担。

---

### 3.2 Increment B — 调度 jitter 防并发峰值

#### 3.2.1 动机

naozhi 现状 0 抖动。场景：

- 用户建了 10 个 `@every 30m` 的 job（各自独立用途：邮件摘要、日历同步、监控扫描、代码 review…）
- 00:00、00:30、01:00 每次触发**同秒起 10 个 CLI 子进程**
- CPU/RAM 瞬间峰值；同时 10 路向 Anthropic/Bedrock 发 API 请求 → rate limit 触发 → 多 job 同时 `LastError`
- `maxProcs` 保护只会让"溢出的"失败，不 smooth

**Claude 的做法**："Scheduled tasks use a randomized delay of several minutes for server performance." 这是云端服务防惊群的标准招。自托管场景同样成立——因为**后端是同一个 CLI 子进程池**。

#### 3.2.2 设计

在 `execute(j)` 最开头（`guard.CompareAndSwap` 之后）加一个 **ctx-aware sleep**：

```go
// 执行前随机延迟 0..min(schedulerJitterMax, period/4)。
// period 很短的 job（< 20m）自动缩减抖动窗口，防止 5m job 被抖 2m
// 吃掉 40% 节奏；period 很长的 job 用完整窗口分散峰值。
// 尊重 ctx：如果 TriggerNow 在抖动期间取消，立即返回不执行。
func applyJitter(ctx context.Context, schedule string, jitterMax time.Duration) {
    if jitterMax <= 0 { return }
    period := schedulePeriod(schedule)          // 复用 computeJobTimeout 的 sched.Next 两次差值
    cap := jitterMax
    if period > 0 && period/4 < cap { cap = period / 4 }
    if cap <= 0 { return }
    d := time.Duration(rand.Int64N(int64(cap))) // 用 math/rand/v2 的 Int64N，安全性不敏感
    if d <= 0 { return }
    t := time.NewTimer(d)
    defer t.Stop()
    select {
    case <-t.C:
    case <-ctx.Done():
    }
}
```

调用位置：`execute(j)` 在取完 snapshot 拿到 `schedule` 之后、`sess.Send` 之前。

#### 3.2.3 配置

```yaml
cron:
  jitter_max: 2m          # 默认 2 分钟；0 关闭（回到当前行为）；上限硬编码 10m
```

在 `SchedulerConfig` 加 `JitterMax time.Duration`。解析处复用现有 `time.ParseDuration` 逻辑（参考 `ExecTimeout` 的解析），带默认值 `2 * time.Minute`，硬编码上限 `10 * time.Minute`（抖动比周期还长没有意义）。

#### 3.2.4 Edge cases

| 场景 | 行为 |
|---|---|
| `TriggerNow`（用户点 Run now） | 传入的 ctx 不经过 sleep，**路径旁路 jitter**。见 3.2.5。 |
| 非常短的 period（5m 最低） | cap = period/4 = 75s，不超过 2m 上限 |
| period 无法解析（手写 bad cron） | period<=0 → cap=jitterMax，兜底 2m |
| `Paused` → `Resume` 紧跟一个 scheduled tick | 正常走 jitter，符合预期（避免 resume 一堆 job 同时起） |
| 配置 `jitter_max: 0` | 直接 return，零开销，行为回退到今天 |

#### 3.2.5 TriggerNow 不抖动的实现

`execute(j)` 多加一个 `viaTriggerNow bool` 参数，`TriggerNow` 调用传 `true`，scheduled 路径（robfig 回调）传 `false`。抖动只在 `!viaTriggerNow` 时生效。

**理由**：用户点 "Run now" 是明确即时意图，抖 2 分钟是反直觉的；scheduled 路径用户不关心绝对时间点（preview 显示 09:00，实际 09:01:20 跑，用户 OK）。

#### 3.2.6 UI 提示

- `renderFreqPreview` 在预览下方追加一句 subtle 小字：`"实际会在上述时间点后 0–2 分钟内随机启动（防并发峰值，可在 config.yaml 调整）"`
- 仅在 `jitter_max > 0` 时显示；`/api/cron/config` 增加返回字段 `jitter_max_ms`，UI 读取

#### 3.2.7 测试

- `scheduler_test.go`：
  - `TestExecute_AppliesJitter`（用 fake clock / 注入 `rand` 源 + 期望 sleep 调用）
  - `TestExecute_TriggerNow_NoJitter`
  - `TestExecute_JitterRespectsCtxCancel`（Stop() 中途取消，不等 timer 耗尽）
  - `TestApplyJitter_CapClampedByPeriod`（5m 周期抖 75s not 120s）

#### 3.2.8 Rollback

- 运行时：`cron.jitter_max: 0s` 立即关闭
- 代码：删 `applyJitter` + 配置字段；无持久化状态

---

### 3.3 Increment C — Missed schedule 检测 + attention 联动

#### 3.3.1 动机

naozhi 进程重启期间（OOM 重拉、新版本发布、宿主机重启）**错过的 cron tick 不会补跑**（robfig/cron 无 catch-up 语义）。当前 UI 对此完全沉默——用户不知道任务被错过，只发现"为什么今天没收到日报"。

Claude 的处理是顶部大 banner + "Keep awake" 切换；naozhi 不是个人电脑，借鉴"显式告诉用户"的心智模型而非具体实现。

#### 3.3.2 检测算法

一个 job 被视为"错过过调度"，当且仅当：

```
expectedLastRun = 基于 schedule，从 job.CreatedAt 或上次 LastRunAt 往前算
                  的最近一次应跑时间
actualLastRun   = job.LastRunAt

missed          = actualLastRun 比 expectedLastRun 老于 period × 1.5
```

实现：

```go
func (j *Job) HasMissedSchedule(now time.Time) (missed bool, expectedAt time.Time) {
    sched, err := cronParser.Parse(j.Schedule)
    if err != nil { return false, time.Time{} }

    // 上一次应跑时间：sched 不直接给 Prev，用倒推法：
    // 从 now 往前找最大的 t 使 sched.Next(t-ε) ≤ now。
    prev := previousTickBefore(sched, now)
    if prev.IsZero() { return false, time.Time{} }

    period := sched.Next(prev).Sub(prev)
    if period <= 0 { return false, time.Time{} }

    // "从未跑过" + "应该跑过" 也算 missed
    if j.LastRunAt.IsZero() {
        return now.Sub(j.CreatedAt) > period, prev
    }
    return prev.Sub(j.LastRunAt) > period*3/2, prev
}
```

`previousTickBefore` 辅助：二分搜索或线性 500ms 步长回推，因为 robfig 只提供 `Next`，反向算成本接受（cron panel 加载时一次性计算，非高频）。

#### 3.3.3 数据字段

不进 `Job` 持久化——纯 derive，每次列出时按 `time.Now()` 实时算。避免时钟漂移和磁盘写。

API `GET /api/cron` 响应每个 job 追加：

```json
{
  "id": "...",
  "last_run_at": ...,
  "missed_since": 1714234234567,      // 首次可能跑漏的时间（毫秒）
  "missed": true                      // 冗余字段方便 UI 判断
}
```

不 missed 的 job 两个字段都缺省（省带宽）。

#### 3.3.4 UI 改动

- **顶部 banner**（`renderCronPanel` 在 `cron-filter-bar` 上方新增）：
  - 仅在存在 missed job 时显示
  - 文案：`⚠️ 有 {n} 个任务曾错过调度 — 进程重启或休眠期间未补跑。点击查看。`
  - 点击 → `setCronStatusFilter('attention')`
- **卡片徽章**：在 `cc-meta` 行中加 `<span class="cc-missed" title="上次运行比预期晚了 N">missed</span>`
- **attention 计数**：`cronBadge`（header 右上角红点）的计数纳入 missed job
- `filterCronJobs` 的 `attention` 分类扩展：`paused || last_error || missed`

#### 3.3.5 部署后第一次启动的抑制

naozhi 刚启动，所有长间隔 job 都会"missed"——这不符合意图（没人会为了重启空窗期的几分钟被 banner 轰炸）。

**抑制条件**：`now.Sub(schedulerStartedAt) < 5 * period`，即启动不到 5 个周期内不判为 missed。放到 `HasMissedSchedule` 里：

```go
if now.Sub(schedulerStartedAt) < period*5 { return false, time.Time{} }
```

`schedulerStartedAt` 加到 `Scheduler` 结构体，`Start()` 时 `time.Now()` 一次写入。

#### 3.3.6 测试

- `TestJob_HasMissedSchedule_NeverRun_StartupSuppressed`
- `TestJob_HasMissedSchedule_RecentRun_False`
- `TestJob_HasMissedSchedule_StaleLastRun_True`
- `TestJob_HasMissedSchedule_BadSchedule_False`
- `TestHTTPAPI_GET_JobsIncludesMissedField`

#### 3.3.7 Rollback

纯 derived 字段，关闭 feature 只需 UI 不 render banner；后端逻辑保留或删除都可。

---

### 3.4 Increment D — 排序控件

#### 3.4.1 动机

- 当前固定 `created_at desc`（`dashboard.js:8799`）
- 用户超过 10 job 后想按"下次运行时间"或"名称"找目标

#### 3.4.2 设计

- **前端状态**：`cronSortOrder` 取值 `created_desc` (默认) | `next_asc` | `last_desc` | `title_asc`
- **持久化**：`localStorage.setItem('nz_cron_sort', order)` — 用户偏好
- **UI**：在 `cron-list-head` 右侧（"New" 按钮旁）加一个 `<select>`，onchange 调 `setCronSortOrder(value)` → 写 localStorage + `renderCronList()`
- **纯前端**：排序不过服务端、不改 API，`renderCronList` 里新增：

```js
const compare = {
  created_desc: (a, b) => b.created_at - a.created_at,
  next_asc:     (a, b) => (a.next_run || Infinity) - (b.next_run || Infinity),
  last_desc:    (a, b) => (b.last_run_at || 0) - (a.last_run_at || 0),
  title_asc:    (a, b) => (a.title || a.prompt || '').localeCompare(b.title || b.prompt || ''),
}[cronSortOrder] || compare.created_desc;
```

#### 3.4.3 测试

`dashboard.js` 单元：`compareBy[sortOrder]` 在 mock job 数组上给出期望顺序（覆盖 4 种模式 + 缺值/空值兼容）。

#### 3.4.4 Rollback

删除 select + 写死 `compare = created_desc`。

---

### 3.5 Increment E — Next run 视觉升格

#### 3.5.1 动机

"下次何时跑"是所有 cron job 最被用户关心的字段，但目前埋在 meta 行里和 5 个其它字段并列。

#### 3.5.2 设计

`cronJobCardHtml` 渲染层：

- 把 `next: xxx` 从 `cc-meta` 中提出，升格为**独立右上角徽章**：
  ```html
  <div class="cc-next-badge">
    下次: <span class="cc-next-rel">5 分钟后</span>
    <span class="cc-next-abs">11-15 09:00</span>
  </div>
  ```
- 紧近时间高亮：next < 10 分钟，徽章加 class `imminent`（colors: `--nz-accent`）
- `paused` 的 job 徽章 hidden；`missed` 的 job 徽章改为红色 `已错过调度`

#### 3.5.3 CSS

```css
.cc-next-badge        { position: absolute; top: 10px; right: 10px; font-size: 11px; color: var(--nz-text-mute); }
.cc-next-badge.imminent { color: var(--nz-accent); font-weight: 600; }
.cc-next-badge.missed { color: var(--nz-red); }
.cron-card            { position: relative; padding-right: 90px; /* 让 badge 不压 actions */ }
```

#### 3.5.4 风险 / 测试

- 移动端卡片变窄时徽章和 actions 可能重叠 → media query `@max-width 480px` 退回 meta 行
- Snapshot test（`cronJobCardHtml` 5 种状态：idle / imminent / paused / missed / 错误）

#### 3.5.5 Rollback

纯 UI 改动，revert HTML + CSS 即可。

---

## 4. 实施顺序

按依赖 + 收益优先级：

| 顺序 | Increment | 理由 |
|---|---|---|
| 1 | **B (jitter)** | 解决真实生产 burst 风险；零依赖；~50 行代码 |
| 2 | **A (Title)** | UI 收益最大；是 C 和 D 的弱依赖（排序按 title、missed banner 显示 title） |
| 3 | **E (Next badge)** | UI 小改；依赖 A 的 title 字段做"missed 徽章显示 title" |
| 4 | **D (Sort)** | 依赖 A（title_asc 模式）；纯前端 |
| 5 | **C (Missed)** | 依赖 A（banner / 徽章显示 title）；逻辑最复杂、最小使用人群可能用不到 |

建议 5 个 increment 分 **5 个 PR / commit**，每个都可独立 ship 与 revert，不做"大爆炸"。

## 5. 验收标准

每个 increment 独立验收：

- [ ] 单元测试 `-race` 全绿
- [ ] `go vet` 无警告
- [ ] `internal/server/static_ux_contract_test.go` 加对应断言（modal 有 title 输入框 / banner 文案 / 排序控件存在）
- [ ] 手工走位：
  - 建 1 个 job，观察行为
  - 建 5 个同 schedule job，观察抖动（jitter 开关 before/after 对比日志中 `execStart`）
  - 停止 naozhi 30 分钟重启，观察 missed banner
  - 编辑 job 改 title，搜索 title 匹配
  - 切换排序模式，列表顺序随之变化

## 6. 开放问题

- [ ] **Title 对 IM 路径的意义**？`/cron add` 命令是否也支持 `name=xxx`？倾向暂不支持，IM 场景下自动 fallback 用 prompt 首行即可。
- [ ] **Jitter 是否对 cron 通知推送结果造成混乱**？通知里是否显示"延迟 N 秒"？倾向不显示（用户不需要知道 jitter 机制的细节，delivery 成功与否才是关键）。
- [ ] **Missed 检测能否用更精确的 Prev 而不是回推？** robfig/cron 是否有人写过 `Prev()` 实现？可以后续调研，初版用回推法即可，性能非瓶颈。
- [ ] **DST 边界 / 时区变更**时 missed 检测是否会误报？需要单测覆盖，初版可以接受"时区切换当日可能误报 1 次"。

## 7. 非目标再次声明

- 本 RFC **不新增** cron 底层调度器（继续 robfig）
- **不新增** 重试策略（missed 识别 ≠ 补跑）
- **不新增** 任务模板 / 克隆（Title 字段铺路，模板是下一个 RFC）
- **不动** 持久化格式以外的兼容性（新字段全部 omitempty）
