# Cron Dashboard Redesign — 详细设计

> 状态：Draft v2 · 2026-05-22 · 作者：Kevin
> Mockup：[`cron-dashboard-mockup.html`](./cron-dashboard-mockup.html)
> 评审：见末尾「评审记录」

## 1. 背景

当前 dashboard `定时任务` 面板的两侧空间分配失衡：

- **列表区**：5 张卡片（每张 ~150 px 高）在 ~250 px 宽度下被切成多行；存在 `.cj-row` 高密度行式样式（`dashboard.html:1199-1212`）但 wide / medium 档下无规则，回退成大卡片。
- **详情区**：自上而下 = 标题 / 提示词大块 / 通知/上下文枯燥字段 / 4 按钮 / 历史列表。**用户每次最想看的"下次什么时候跑、上次成功率、失败原因"都被埋在折叠区**。
- **执行细节**：`cronTimelineDetailHtml`（`dashboard.js:11752`）目前只展示三个 `<pre>` 块（prompt / result / error），失败时无法在对话流里定位是哪一步 tool 出错。
- **Mobile**：桌面三栏并列在 375 px 屏严重受挤。

## 2. 目标 / 非目标

**目标**

1. 列表区一屏可见 **12-15 个任务**（行高 36-40 px），状态色仅异常时显示。
2. 详情区顶部 **KPI 驾驶舱**直答用户的核心三问：下次什么时候跑、最近表现如何、上次成败。
3. 执行细节升级成 **对话流 timeline**：把 stream-json 还原成 user / assistant / tool 时间线，工具调用折叠卡片可展开看 stdout/stderr，**失败的 tool 红色高亮**。
4. Mobile 友好：三栏塌缩 stack 导航，run-sheet 改 88 vh bottom-sheet。

**非目标**

- 不改 cron 调度核心 / 持久化 schema / 导航层级。
- 不引入前端框架（沿用原生 JS + `innerHTML`）。

## 3. 现状摸底

### 3.1 后端可用数据

- `internal/cron/run.go:39` `CronRun` 已含 `RunID / State / Trigger / StartedAt / EndedAt / DurationMS / SessionID / Prompt / Result / ErrorMsg`。
- `CronRun.SessionID` 指向 `~/.claude/projects/<cwd-encoded>/<session_id>.jsonl` —— 完整 stream-json 已在磁盘，dashboard 没读它。
- `cron.Job` 已有 `MaxDuration` 字段（默认 30 min）→ 实时流轮询超时上限来源。
- `cron.RunCounters` (Stats) 已累计 `Total / Succeeded / Failed`（`dashboard_cron.go:433`），可作为 KPI 成功率口径。
- 配套 HTTP 端点 `GET /api/cron/runs` / `GET /api/cron/runs/{run_id}`（`dashboard.go:354-355`），走 per-IP `runsLimiter` 60 req/min。
- `internal/discovery/history_tail.go:125` 已实现 `resolveJSONLPath` + 反向流读 `parseTail`（**P2 可直接复用**）。

### 3.2 前端可改造点

- `cronJobCardHtml`（`dashboard.js:11382`）— 单行渲染。
- `renderCronDrawer`（`dashboard.js:12306`）— 详情区四块组件。
- `cronTimelineDetailHtml`（`dashboard.js:11752`）— sheet body 内容当前是三 `<pre>`，调用方仅 `renderRunDetailSheet`（`12023`）一处（`agent_view.js` 不引用）。
- ResizeObserver 写 `.cron-detail-body[data-cron-layout]` 四档 `wide/medium/narrow/single`。`narrow/single` 已有 `.cj-row` 完整覆盖（`dashboard.html:1199-1212`），**`wide/medium` 档无 `.cj-row` 规则** → P0 仅需补这两档。

### 3.3 不动的地方

- run-sheet 桌面右滑 / 移动 bottom-sheet 的 CSS 切换（`1354-1372`）保留。
- `/api/cron/runs/{run_id}` schema 不变。
- 不改 `cron_jobs.json` / `runs/<job>/<run>.json` 持久化 schema。

## 4. 设计

### 4.1 阶段拆分

| 阶段 | 范围 | 风险 | 独立部署 |
|---|---|---|---|
| **P0** 列表行式 + 概览 chips | CSS + cronJobCardHtml + renderCronOverview | 低 | 是 |
| **P1** 详情驾驶舱 + 提示词折叠 + sticky 操作栏 | renderCronDrawer + CSS | 低 | 是 |
| **P2a** 后端 transcript 端点 | dashboard_cron.go + jsonl 解析 | 中 | 是（端点存在但前端不调） |
| **P2b** 前端 4 tab + transcript renderer | dashboard.js + dashboard.html | 中 | 是（失败回退原始日志 tab） |
| **P3** mobile | CSS `@media` + stack 导航 | 低 | 是 |

每阶段独立 PR、独立 tag、独立可回退。

### 4.2 P0 列表行式 + 概览

**渲染契约**（每行 36-40 px）：

```
[8px status dot] [name 单行省略 flex:1] [sub-line: "每天 03:00 · 明早 4h 12m"] [rate 徽章 10px] [⋯ icon]
```

- 状态色仅在 `failed / warn / running` 时着色，健康任务全灰。
- 下次时间用相对：`< 1h` imminent 蓝；`< 0` warn 橘（`逾期 53m`）；`paused` 灰。
- `cj-rate` 徽章规则：`>=99% green / 90-98% amber / <90% red`，hover tooltip 展示 `5/5`。

**CSS 改动局限**：补 `wide` 与 `medium` 两档 `.cj-row` 规则；`narrow/single` 不动。**显式 reset `.cj-when` / `.cj-stats` 的 `display: flex`**（防止全局样式污染）。

**概览条**（`.list-overview`）：

```
[全部 4][·健康 3][·需关注 1][运行中 1] ........ [最近 1h · 0 失败]
```

每个 chip 可点 → 切换过滤态。复用 `cronFilterState`（`dashboard.js:10064`），新增 `tab: 'all'|'healthy'|'attention'|'running'`，前端纯过滤。

**项目栏可折叠**（已部分存在）：进入 `定时任务` 子面板时默认折叠成 56 px icon 条，加 CSS transition。

**E2E**：`test/e2e/cron_run_sheet.test.js` 增加 `cron list overview filter` case + 行高 ≤ 48 px 断言。

### 4.3 P1 详情驾驶舱

新结构：

```
[14px header — 名称 + ⋯/✎/‖]
[KPI 驾驶舱 1×4 grid]
  ┌─下次运行 (primary 蓝)─┐ ┌成功率┐ ┌平均耗时┐ ┌上次结果┐
  │ 4h 12m              │ │ 100% │ │ 3m 15s │ │ 成功    │
  │ 明早 03:00          │ │ 5/5  │ │ 上次..│ │ 昨日   │
  └────────────────────┘ └─────┘ └──────┘ └──────┘
[提示词折叠 1 行 + "展开"]
[执行历史 — 行式列表]
[底部 sticky 操作栏 — ▶立即执行 ‖暂停 ✎编辑 ........ 🗑删除]
```

**KPI 数据来源**（口径关键）：

| KPI | 来源 | 备注 |
|---|---|---|
| 下次运行 | `cron.Job.NextRunAt` | 已有 |
| 成功率 | **`cron.Job.Stats.Succeeded / Stats.Total`** | 累计而非 200 条窗口；与 GC 解耦 |
| 平均耗时 | `CronRunSummary[]` 客户端聚合（最近 50 条） | 副标题标注「近 50 次」避免误读 |
| 上次结果 | `CronRunSummary[0]` | 已有 |

**修复评审 S6**：成功率不再客户端聚合 200 条（会随 GC 漂移），改读后端累计 `Stats`；平均耗时这种需要 trim 极端值的指标用最近 50 条窗口，并在副标题透明披露口径。

**运行中状态**（job 当前有 running run）：用蓝色 banner 替换 KPI grid：

```
[12:52 大数字]  [正在执行 · 12m 52s]      [■ 中止本次]
                [tokens / 预估剩余 / trigger]
```

`12:52` 由前端从 `started_at` + `Date.now()` 实时算，1 Hz tick。

**提示词折叠**：默认单行预览 + "展开" 按钮。点 "编辑" 跳现有编辑模态。

**底部 sticky 操作栏**：替换 `.cron-drawer-actions` 内联放置，CSS `position:sticky;bottom:0`，运行中"立即执行" disabled。

### 4.4 P2 run-detail-sheet 升级

#### 4.4.1 顶部 4 块 stats

| label | value | 来源 |
|---|---|---|
| 耗时 | `3m 44s` | `DurationMS` |
| Tokens | `14.2k` | transcript 解析（in/out 累加） |
| 工具调用 | `7` | transcript 解析 |
| Session | `a7f9b2e1 ↗` | `SessionID`，点击跳完整对话 |

Tokens / 工具调用次数依赖 P2a 端点；端点失败时这两块降级显示 `—`。

#### 4.4.2 4 个 tab

```
对话 (12) | 工具 (7) | 提示词 | 原始日志
```

- **对话**（默认）：JSONL 还原成 turn timeline。
- **工具**：仅工具调用条，按耗时/状态过滤。
- **提示词**：完整 `Prompt`（运行时 snapshot）。
- **原始日志**：保留现有 `<pre>` 三块视图，作为兜底 — **P2b 即使 transcript 端点失败仍可用**。

#### 4.4.3 后端新增 — `GET /api/cron/runs/{run_id}/transcript`

```
GET /api/cron/runs/{run_id}/transcript?job_id=<jid>&after=<turn_index>
→ 200 application/json
{
  "session_id": "a7f9b2e1...",
  "started_at": "2026-05-21T03:00:01Z",
  "ended_at":   "2026-05-21T03:03:45Z",
  "tokens": { "input": 8100, "output": 6100, "total": 14200 },
  "tool_calls": 7,
  "turns": [
    { "index": 0, "kind": "user", "ts": "...", "text": "..." },
    { "index": 1, "kind": "assistant", "ts": "...", "tokens": 1200, "text": "..." },
    { "index": 2, "kind": "tool_use", "ts": "...", "tool":"Bash", "summary":"git fetch...", "status":"ok", "duration_ms":2100, "input": {...}, "output": "..." }
  ],
  "next_index": 3,
  "truncated": false,    // 部分内容（达到上限被截）
  "fallback": ""         // "" / "raw" / "missing"
}
```

**字段语义（评审 S5）**：

- `truncated:true` —— 渲染了部分 turns，但未到末尾。
- `fallback:"raw"` —— 完全无可解析对话流，前端切原始日志 tab。
- `fallback:"missing"` —— SessionID 空或 JSONL 不存在，前端显示「无对话流可解析」并自动切原始日志 tab。
- `next_index` —— 下次增量拉取的起点（用于实时流）。

**实现位置**：`internal/server/dashboard_cron.go` 新增 `handleRunTranscript`，与 `handleRunDetail` 同 group 共享 `runsLimiter` 60 req/min。

**安全防线（强制，评审 B1-B3）**：

```go
// 1. parse runID from path, jobID from query
// 2. lookup CronRun via runStore.Get(jobID, runID)
//    → 显式断言 run.JobID == jobID（防 cross-key SSRF）
//    → 显式断言 IsValidID(run.SessionID)
// 3. WorkDir 必须 filepath.IsAbs（runStore 历史已校验，仍二次确认）
// 4. resolveJSONLPath(WorkDir, SessionID) 复用 history_tail.go:125 的工具
//    → EvalSymlinks 后 strings.HasPrefix(claudeDir+"/projects/")
// 5. 打开文件：
//    a. os.Lstat 拒非 regular file
//    b. open
//    c. fstat 二次校验 size 与 inode（防 TOCTOU rename swap）
// 6. 反向流读（复用 parseTail 模式）— 绝不 ReadFile 整文件
//    a. 从文件末尾 chunked read，bufio.Scanner Buffer 上限 64 KB（=maxTurnBytes）
//    b. 单行超 64 KB → 截断该 turn，next_index 仍前进
// 7. 用 run.StartedAt / run.EndedAt 过滤只属于当前 run 的 turns
//    （fresh=false 模式多 run 共享同一 JSONL — 评审 S3）
// 8. 总量上限：500 turns / 8 MB / 单 turn 64 KB / tool output 32 KB
//    任一触顶 → truncated:true
// 9. ANSI 颜色码剥离：tool stdout/stderr 在序列化前 regexp 清除 \x1b\[[0-9;]*m
```

**降级路径**：JSONL 文件不存在 / 解析失败 / SessionID 为空 → `fallback:"missing"` + `turns:null`，**返回 200**（不拖累整个 run-detail）。

#### 4.4.4 前端 transcript parser 与 turn 渲染

新增 `cronRunTranscriptHtml(transcript)`，渲染契约：

| turn.kind | 渲染 |
|---|---|
| `user` / `assistant` | 头像 + role + ts + tokens 增量 + `renderMd(text)` |
| `tool_use` | 折叠卡：`tool name + summary（截断）+ status pill + duration`，展开后 `input` + `output` |

**XSS 与 ANSI 防护（评审问题 4）**：

- **assistant text**：走 `renderMd`（已 esc，安全）。
- **tool input / output / error**：**强制 `<pre>` + `esc()`，不走 `renderMd`**。tool stdout 来自任意进程，含 HTML/JS 字符串注入风险。
- ANSI 码已在后端剥离（4.4.3 步骤 9），前端不再处理。

**turn 去重（评审问题 3a）**：

- 后端发 `turn.index`（JSONL 行号），前端按 index 追加，**不依赖 ts**（ts 同毫秒会碰撞）。
- 前端缓存最后一次 transcript 对象，tab 切走再切回时仅在 `turns.length` 变化或 run 状态变化时重渲染（评审问题 3b），避免 mermaid 占位 ID 漂移。

#### 4.4.5 实时流（运行中 run）

**评审 S4 修复**：放弃 1.5 s 轮询，改 **SSE**（`text/event-stream`）单连接推 diff，零轮询、不打高 QPS、多 tab 多 cron 不放大。

```
GET /api/cron/runs/{run_id}/transcript/stream?job_id=<jid>
Accept: text/event-stream
→ 200 text/event-stream
event: turn
data: { "index": 3, "kind": "tool_use", ... }

event: turn
data: { "index": 4, "kind": "assistant", ... }

event: done
data: { "state": "ok", "ended_at": "..." }
```

**降级路径**：浏览器不支持 SSE / 端点 5xx → 自动回退到 `?after=<idx>` 增量轮询，间隔 3 s（不再 1.5 s），并叠加 Page Visibility API（隐藏 tab 暂停）。

**停止条件**（评审问题 3c）：

- run state 转 `ok / error / canceled` → SSE 端发 `event: done` 后关闭，前端清 poller。
- 前端兜底：`Date.now() - run.started_at > job.max_duration` 强制停（job.MaxDuration 已在 `cron.Job` schema）。

#### 4.4.6 翻页

sheet 底部 `‹上次 / ↻重跑 / 下次›` 三按钮。复用 `recentRunsCache[jobId]` 算 prev/next。

### 4.5 P3 Mobile 适配

| 桌面 | 移动 |
|---|---|
| 三栏并列 | stack：列表 → 详情 → run-sheet 一次只显示一屏 |
| KPI 1×4 横排 | 2×2 grid（`下次` `上次` 上排吸引视线） |
| run-sheet 右滑覆盖 | 88 vh bottom-sheet，drag handle 下拉关闭 |
| tabs 4 个一行 | 横向 scroll（`overflow-x:auto`） |
| tool 卡片单行 | 命令独占第二行 |

触摸基线：cron 行 56 px、历史行 54 px、底部按钮 42 px、FAB 主按钮 44 px+。`safe-area-inset-bottom` 应用于所有 sticky 底栏。

复用现有 `cronDetailJobId` 状态信号（桌面是布局切换、移动是 view 切换），不引入新路由。

## 5. API 变更

| 端点 | 变更 |
|---|---|
| `GET /api/cron` | 不变 |
| `GET /api/cron/runs` | 不变 |
| `GET /api/cron/runs/{run_id}` | 不变 |
| **`GET /api/cron/runs/{run_id}/transcript`** | **新增**（4.4.3） |
| **`GET /api/cron/runs/{run_id}/transcript/stream`** | **新增** SSE（4.4.5） |

新端点共享 `runsLimiter` 60 req/min。SSE 端点单连接长持有，不算入轮询限频（连接计数另设 `maxStreamsPerIP=4` 防滥用）。

## 6. 数据迁移与缓存

- **不改持久化 schema**。
- 老 run JSON 文件无 SessionID（早期版本）→ transcript 端点 `fallback:"missing"`，前端切原始日志 tab。
- **缓存策略修订（评审 S2）**：现 `sw.js` 实际是 no-op，bump 它无效。改为：
  - `dashboard.html / dashboard.js / *.css` 服务端响应 `Cache-Control: no-cache, must-revalidate` + ETag → 浏览器每次 revalidate。
  - `<script src="/static/dashboard.js?v=<build-sha>">` build 期注入 build SHA → 强制 cache-bust。
  - 这一改动归到 P0（最先发布的 PR），后续阶段无需再 bump。

## 7. 测试策略

**单元 (Go)**

- `handleRunTranscript` 表驱动：正常 / 文件不存在 / SessionID 空 / WorkDir 不存在 / 文件超 8 MB / 单 turn 超 64 KB / fresh=false 多 run 混合 / symlink 攻击 / non-regular file。
- 每个降级 case 显式 assert `resp.Fallback == "raw"|"missing"` 且 `resp.Turns == nil`（评审问题 5a）。
- `-race`，覆盖 ≥ 80%。

**SSE 端点**

- 模拟 run 结束 → 客户端收到 `event: done` → 服务端关闭连接 → 客户端无残留 goroutine。
- maxStreamsPerIP 触顶时新连接 429。

**前端 UX 契约（`static_ux_contract_test.go`）**

- 4 tab DOM 存在；KPI 4 块存在；底部 sticky class；tool output 渲染走 `esc`（grep 确认无 `renderMd(turn.output)` 字样）；`fallback` 字段处理逻辑存在（防后端发了字段前端忘处理）。

**E2E（`test/e2e/cron_run_sheet.test.js`）**

- 列表行高 ≤ 48 px；点 history row 出 sheet；tab 切换；failed tool 红色 class；mobile viewport (375×812) bottom-sheet 出现；**运行中 run → 状态变 completed → SSE 关闭，无 outgoing 请求**（评审问题 5b）；SSE 失败回退轮询。

## 8. 部署 + 回退

- 走现有部署 skill（`docs/ops/naozhi-deploy-skill.md`）：build → test → `sudo systemctl restart naozhi` → smoke。
- 五阶段独立 PR、独立 tag。每阶段 `naozhi upgrade` smoke 通过后再下一步。
- 回退：`git revert <commit>` + `naozhi upgrade`。无 schema 改动，回退无痛。

## 9. 风险与权衡

| 风险 | 缓解 |
|---|---|
| JSONL 解析失败拖累 sheet | 4.4.3 fallback 三态降级，原始日志 tab 永远可用 |
| 大 JSONL（fresh=false 长积累 50-100 MB） | 反向流读 + 时间窗过滤；8 MB / 500 turns 三层硬限 |
| symlink / TOCTOU 攻击 | Lstat 拒非 regular + EvalSymlinks 前缀校验 + Fstat 二次确认 |
| tool stdout XSS | 强制 `<pre>+esc`，禁用 renderMd |
| 多 tab 多 cron 轮询打爆限流 | SSE 长连接 + Page Visibility 暂停 + 单连接 diff |
| 静态资源缓存导致新旧错位 | Cache-Control:no-cache + ETag + script `?v=<sha>` |
| KPI 成功率随 GC 漂移 | 改读后端 `Stats.Total/Succeeded` 累计字段 |

## 10. 不在本期范围

- Cron Dashboard 全新独立页（`project_cron_dashboard.md`）— 是更激进的方向。
- 改 cron 调度算法。
- AskUserQuestion 卡片样式统一。
- 多端口 / 多 backend 选择。

## 11. 落地里程碑

| Step | 输出 | 估时 |
|---|---|---|
| 0 | 设计文档（本文 v2 ✓） | — |
| 1 | P0 PR — 列表行式 + 概览 chips + Cache-Control 修订 | 2-3 h |
| 2 | P1 PR — KPI 驾驶舱 + 提示词折叠 + sticky 操作栏 | 3-4 h |
| 3 | P2a PR — handleRunTranscript（含 SSE 流式端点）+ 测试 | 5-6 h |
| 4 | P2b PR — 4 tab + transcript renderer + 实时流前端 | 5-7 h |
| 5 | P3 PR — mobile 适配 | 2-3 h |
| 6 | tag + release + smoke | 0.5 h |

合计 17-23 工时。

---

## 评审记录（v1 → v2）

### Architect 评审（阻塞 + 建议）

| ID | 类别 | 问题 | v2 处置 |
|---|---|---|---|
| B1 | 阻塞 | `os.ReadFile` 整文件读 → OOM 风险 | §4.4.3 改反向流读 + bufio.Scanner 64 KB Buffer，明确"绝不 ReadFile" |
| B2 | 阻塞 | symlink + TOCTOU 防御缺位 | §4.4.3 加 Lstat / EvalSymlinks / Fstat 二次校验 + IsValidID(SessionID) |
| B3 | 阻塞 | `run.JobID == jobID` 显式校验 | §4.4.3 步骤 2 显式 assert |
| S1 | 建议 | P2 拆 P2a + P2b 双 tag | §4.1 + §11 已拆 |
| S2 | 建议 | sw.js bump version 不够 | §6 改 Cache-Control + ETag + `?v=<sha>` |
| S3 | 建议 | fresh=false 长积累 JSONL → 反向流读 + ts 过滤 | §4.4.3 步骤 6/7 已加 |
| S4 | 建议 | 1.5 s 轮询多 tab 放大 → SSE | §4.4.5 改 SSE，降级才回退轮询 + Page Visibility |
| S5 | 建议 | truncated vs fallback 双字段 | §4.4.3 拆 `truncated:bool` + `fallback:string` 三态 |
| S6 | 建议 | KPI 成功率 200 条窗口 → Stats 累计 | §4.3 改用 `Stats.Total/Succeeded` |

### Code Reviewer 评审

| ID | 严重度 | 问题 | v2 处置 |
|---|---|---|---|
| Q4 | HIGH | tool output XSS — 必须 `<pre>+esc` | §4.4.4 显式约束 |
| Q3a | HIGH | turn 去重键 (index) | §4.4.3 schema 加 `turn.index` + §4.4.4 前端按 index 追加 |
| Q3c | MEDIUM | max_duration 数据来源 | §4.4.5 已注明 `cron.Job.MaxDuration` |
| Q5a | MEDIUM | fallback 测试缺失 | §7 显式 assert `resp.Fallback` |
| Q1 | LOW | wide 档 display reset | §4.2 已加 |
| Q3b | LOW | tab 切换 mermaid 占位 | §4.4.4 缓存 transcript，仅 length 变化时重渲染 |
| Q5b | LOW | 轮询停止 E2E | §7 加 case |

### 评审 verdict

- Architect: WARNING — 3 阻塞已在 v2 修复。
- Code Reviewer: WARNING — 2 HIGH 已在 v2 修复，P0/P1 可先行。

**v2 状态：可开工。P0/P1/P3 纯前端可先并行；P2a 后端先于 P2b 前端发布。**
