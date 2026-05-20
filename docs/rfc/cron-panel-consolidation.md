# Cron 执行视图收编到「定时任务」面板 — 设计 RFC

> **状态：设计提案（待评审 → 实施）**
> **日期：2026-05-20**
>
> 关联 RFC：
> - `cron-run-history.md`（已实施）— CronRun 模型、API、WS 事件、时间轴 UI 已就绪
> - `consumer-interfaces.md` — RouterCore / SessionRouter consumer 边界
>
> 关联代码：
> - `internal/server/static/dashboard.html`（cron-detail / cron-timeline-panel 样式）
> - `internal/server/static/dashboard.js`（cronVisibleKeys / openCronSession / renderCronPanel / renderCronTimelineForSession）
> - `internal/session/router.go`（RegisterCronStub / RegisterCronStubWithChain）
> - `internal/server/dashboard_session.go`（/api/sessions 列表）

---

## 0. 动机

cron-run-history 上线后，定时任务的"当前执行 + 执行历史"事实上分散在两个屏：

| 信息 | 位置 | 问题 |
|---|---|---|
| 任务列表 + 成功率徽章 + hover tooltip | `openCronPanel()` 主区 | OK |
| 任务详情 + 执行历史时间轴 + run 详情 | 切到 `selectedKey='cron:<id>'` 的 session 视图（`renderMainShell` → `cron-timeline-panel` mount 在 events 上方） | 与对话主交互区共用 mainShell；点击 cron 卡 = 强制离开主对话 |
| sidebar 上的 cron session | 用 `cronVisibleKeys` 白名单按需放行（创建 / 显式打开时入白名单） | 容器与 IM/dashboard 真实对话混用，影响 sidebar 信号噪比；× 按钮语义诡异（"从侧栏隐藏" ≠ "删任务"） |

操作员的真实心智模型：**"定时任务"是一个独立功能区，不是一种特殊会话**。它不应该：
- 占 sidebar 卡槽（即使可隐藏）
- 抢走主对话区（点 cron 行就把当前对话清掉）
- 复用 events scroll（cron 历史不是 events，是 CronRun 列表）

## 1. 目标

1. **cron session 永不出现在 sidebar**。删除 `cronVisibleKeys` 白名单机制（含创建后顺手放行、× 移除等所有路径）。
2. **cron 当前执行状态 + 执行历史全部在「定时任务」面板内**：列表行 + 行内详情抽屉一体，不再借用 mainShell。
3. **dashboard 主交互区只关心人类对话**（IM / 普通 dashboard / scratch / quick），与 cron 完全脱耦。
4. **无功能退化**：用户能看到的信息（运行中 Xs、最近 5 次状态点、完整历史时间轴、单 run 详情、fresh-context 提示）一条不少。
5. **后端 API 不动**（cron-run-history 已落的 `/api/cron`、`/api/cron/runs[/:run_id]`、WS `cron_run_*` 事件全部复用）。

## 2. 非目标

- ❌ 改 cron 调度内核（robfig/cron 不动）
- ❌ 改 CronRun 数据模型 / 持久化 / API
- ❌ 改 IM 渠道 `/cron` 命令
- ❌ 飞书等外部回复路径（cron 完成的 IM 通知不变）
- ❌ 收编 IM 主动推送的 cron 完成通知到面板（保留现有 announce + badge）

## 3. 现状梳理（按操作路径走一遍）

```
点击 header ⏰ btn-cron
  → openCronPanel()                           dashboard.js:10732
  → renderCronPanel()                         dashboard.js ~11886
  → renderCronList()                          dashboard.js:11747
  → cronJobCardHtml() 每行                    dashboard.js:11238
       click → openCronSession(jobId)         dashboard.js:11911
                ├─ markCronSessionVisible('cron:<id>')   ← ❌ 把 cron 推进 sidebar
                ├─ ensureSidebarRowExists                ← ❌
                └─ selectSession('cron:<id>')             ← ❌ 抢走 mainShell
                    → renderMainShell()                  dashboard.js:2160
                       └─ 插入 #cron-timeline-panel      dashboard.js:2193
                          → renderCronTimelineForSession dashboard.js:11382
                             ├─ cronTimelineHtml         dashboard.js:11421
                             ├─ cronTimelineRowHtml      dashboard.js:11461
                             └─ cronTimelineDetailHtml   dashboard.js:11527
```

`cronTimelineHtml` 三件套是干净的纯函数，输出一段 HTML，不感知 mainShell — **这是收编的最大友军：函数级零改动**。

底层关键点：
- `session.RegisterCronStub` 让每个 cron job 在 sessions.json 里登记一条 stub session（key=`cron:<id>`），目的是让 IM 渠道触发的 cron 执行能复用 session 路由 / 资源 cleanup / fresh-context 重启逻辑。
- `/api/sessions` 当前会**把这些 stub 一并返回**，前端 `cronVisibleKeys` 仅在前端层面过滤；属于"前端兜底，后端漏"。

## 4. 设计

### 4.1 顶层结构

cron 面板从「单层列表」升级为「列表 + 行内抽屉」：

```
┌──────────── main 区域 ───────────────────────────────────────────────┐
│ 定时任务                                                  [+ 新建]   │
│ ┌──────────── 搜索 / 状态筛选 / 排序 ──────────────────────────────┐ │
│ │ search │ all/active/attention │ next/last/title                  │ │
│ └─────────────────────────────────────────────────────────────────┘ │
│                                                                      │
│ ┌─ 列表（左/上） ──────────────┐ ┌─ 详情抽屉（右/下） ──────────────┐│
│ │ ● job-A · 每天 09:00         │ │ ▸ job-A — 每天 09:00              ││
│ │   运行中 12s   100%          │ │   schedule chip · workdir · prompt││
│ │ ──                            │ │   notify / fresh-context 状态     ││
│ │ ● job-B · @every 30m  92%   │ │   [立即执行] [暂停] [编辑] [删除] ││
│ │   imminent 02:14             │ │   ─────────────────────────────── ││
│ │ ──                            │ │   当前执行 (running):              ││
│ │ ○ job-C · 已暂停              │ │     ▷ run_id 7f2e · 已运行 12s    ││
│ │ ──                            │ │       已 dispatch · phase: send   ││
│ │ ● job-D · 错误                │ │   ─────────────────────────────── ││
│ │   2小时前 上次失败            │ │   执行历史 (32 次, 92% 成功)       ││
│ │                              │ │     ▼ 5/20 09:00  succeeded 18s  ││
│ │                              │ │        prompt / result / fresh    ││
│ │                              │ │     ▸ 5/19 09:00  succeeded 21s  ││
│ │                              │ │     ▸ 5/18 09:00  failed     —   ││
│ │                              │ │     ▸ ……                          ││
│ │                              │ │     [加载更多]                    ││
│ └──────────────────────────────┘ └──────────────────────────────────┘│
└──────────────────────────────────────────────────────────────────────┘
```

PC（≥ 920px）：列表占左 380–420px，抽屉占右剩余宽度，sticky；列表行点击在抽屉里展开同一 jobId。
移动端（< 720px）：抽屉以"全屏推入"方式覆盖列表（push 而非 modal — 避免遮罩遮住任务概览），左上角箭头返回列表。

为什么不用「行内手风琴展开」：
- 时间轴在窄列里塞不下 prompt/result code block，会反复横向滚
- 多行同时展开会让列表节奏崩
- 抽屉与「点开就看到全部信息」的预期一致，且不要求 cron 面板抢走 mainShell

### 4.2 sidebar / mainShell 完全摘除 cron

| 路径 | 改动 |
|---|---|
| `cronVisibleKeys` Set + `markCronSessionVisible` + `ensureSidebarRowExists` | **整段删除** |
| `dismissSession` 的 `isCronSessionKey(key)` 分支 | **删除**（cron 永远不进 sidebar，所以不会有 dismiss 入口） |
| `sessionCardHtml` 的 `isCron` / `sc-cron-card` / `cronBadge` | **删除** |
| `renderSessions` 里 `visibleItems` 的 cron filter | **删除**（应在数据源剪掉，不在视图层兜底） |
| `renderMainShell` 里 `isCronSessionKey(selectedKey) ? '<div class="cron-timeline-panel" ...>'` | **删除** |
| `renderMainShell` 后的 mount 钩子 `if (isCronSessionKey(selectedKey)) renderCronTimelineForSession(...)` | **删除** |
| ws dispatch 里 `cron_run_started/ended` 的 "如果 selectedKey === 'cron:<id>' 刷新 timeline" 判断 | 改为**"如果 cron drawer 当前对该 jobId open 则刷新"** |
| `openCronSession(jobId)` | 重命名为 `openCronDetail(jobId)`，**不再** `selectSession`，只设 `cronDetailJobId = jobId` 并触发 `renderCronPanel()` |
| `doCreateCronJob` 创建后的 `selectSession` + `markCronSessionVisible` | 改为 `openCronDetail(data.id)` |

### 4.3 后端：cron stub 是否仍要登记 session？

cron-run-history 在前端基础上铺好了。但**后端 `RegisterCronStub` 会让 `cron:<id>` 进 sessions.json 并被 `/api/sessions` 返回**。前端隐藏只是兜底，更彻底的做法是后端不漏：

**方案 A（最小改动，本 RFC 默认采纳）**：保留 stub 登记（cron 调度仍依赖 session 路由），但 `/api/sessions` 列表层把 `cron:` 前缀过滤掉。前端不再做兜底。
- 改一个文件：`internal/server/dashboard_session.go` 列表 handler 加 `if strings.HasPrefix(s.Key, session.CronKeyPrefix) { continue }`。
- 单元测试：mock 一组含 `cron:abc` + `feishu:...` 的 store，断言响应只剩后者。
- 风险：极低。WS subscribe / events fetch 仍可按 `cron:<id>` 工作（运维 / 调试用），但 UI 不再渲染。

**方案 B（更彻底，不在本 RFC 范围）**：cron 完全不走 RegisterCronStub。CronRun 已经独立持久化 prompt/result/error；fresh=false 模式只剩 "JSONL append 到同一 session_id" 这一项还需 router 层支持，cron 自己持有该 session 句柄即可。
- 涉及面：router_discovery / DiscoveryExcludeIDs / cron.executeOpt 重写、event log 协作语义、shim reconnect 边界全要重画。
- 决策：列入「Round 230 后续 ARCH」追踪条目，**本 RFC 不做**。

### 4.4 cron drawer 内容结构

drawer 里垂直栈四块，全部从已有数据源 / 函数复用：

1. **任务概要**（已有 `cronJobs[i]` 字段，无需新 API）
   - 标题（user-set 或 prompt 截断）+ 状态点 + schedule chip
   - workdir + notify + fresh-context + last_error_class
   - 操作行：`▷ 立即执行` `⏸ 暂停 / ▶ 恢复` `✎ 编辑` `🗑 删除`

2. **当前执行状态**（仅当 `j.current_run` 存在时渲染）
   - "运行中 Xs"实时计时（沿用 `formatRunningElapsed` + `cronRunningTickTimer`）
   - run_id 短 ID + phase（`dispatch / send / waiting`）
   - 触发来源（scheduled / manual / catchup）
   - `[查看实时输出]`：可选二期，链到 events scroll readonly view。MVP 不做。

3. **执行历史时间轴**（**直接挂 `cronTimelineHtml` 输出**）
   - 容器从 mainShell 上方迁出 → drawer 内固定 section
   - 时间轴自身的「行点击展开 detail / 加载更多 / fresh hint」逻辑零改动
   - `cronTimelineJumpToSession` 现在跳到 sidebar session — 收编后 sidebar 没有 cron 卡了，改为 toast 显示完整 session_id 并提供"复制"按钮

4. **空态**：从未跑过的 job 显示「下次调度或点击运行触发首次执行」，沿用现有文案

### 4.5 状态机

cron drawer 的可见性新增一对状态：

```
cronDetailJobId: string | null     // null = 抽屉关闭
cronDetailMobileFull: bool         // 仅移动端，抽屉是否全屏推入

action                       state transition
─────────────────────────────────────────────────────────────────
点击列表行 jobId             cronDetailJobId = jobId
点击 drawer 关闭按钮          cronDetailJobId = null
点击 → 不同 jobId             cronDetailJobId = newId（不动画过渡，直接换）
fetchCronJobs 后 jobId 不存在 cronDetailJobId = null + toast "任务已被删除"
切走 cron 面板（header click） cronDetailJobId 保留（回来还在那条）
F5 / 重新登录                 cronDetailJobId = null（不持久化）
WS cron_run_started/ended    若 msg.job_id === cronDetailJobId → 刷新 timeline；否则不动 drawer
```

`cronTimelineState[jobId]` 仍按现有 TTL 30s 失效逻辑工作；切 jobId 后旧 drawer 的 timeline state 不清（用户切回还能复用）。

### 4.6 WS 事件路由

| 事件 | 现状 | 改后 |
|---|---|---|
| `cron_run_started` | `cronApplyRunStarted` 改 cronJobs[i] + 若 selectedKey === 'cron:<id>' 进 timeline 刷新 | `cronApplyRunStarted` + 若 `cronDetailJobId === msg.job_id` 在 drawer 内刷新；不再看 selectedKey |
| `cron_run_ended` | 同上 + `cronTimelineRefreshHead` | 同上；`cronTimelineRefreshHead` 改条件 |
| `cron_result`（legacy） | announce + fetchCronJobs | 不变 |
| `cron_jobs_updated`（如有） | renderCronPanel | 不变 |

`cron_*` 事件不再依赖 `selectedKey`，意味着用户在主对话窗里聊天时，cron 完成同样会在 cron drawer 里实时滚动 — 但 mainShell 不会被动一下。

### 4.7 视觉细节（CSS）

新增 / 调整：
- `.cron-detail-pane`（drawer 容器，PC 右侧 sticky / 移动端 absolute fullscreen）
- `.cron-detail-header`（标题 + 关闭按钮 + schedule chip）
- `.cron-detail-actions`（4 个操作按钮 horizontal scroll on narrow）
- `.cron-detail-current`（current_run section，pulse 动画沿用 `cjRunPulse`）
- 复用：`.cron-timeline-panel`（从 mainShell 迁到 drawer，max-height 改 `min(60vh, calc(100% - 200px))`，去掉 `border-bottom`，背景对齐 drawer）

删除：
- `.sc-cron-card`（sidebar cron 卡的特殊样式，不再有这种卡）
- `.cron-card`（v2 旧版圆角大卡 — 列表已经全改 v3 `.cj-row`，这套样式只剩 cron drawer header 还在调用，迁过去后整批删）

### 4.8 a11y / 键盘

- 列表行：`role="button" tabindex="0"`，Enter / Space 打开 drawer（已有）
- drawer 关闭：Esc，焦点回列表上次点击行
- drawer 内时间轴行：Enter 展开 detail（已有 cronTimelineToggleRow 路径）
- 移动端推入 drawer 时，列表不接受焦点（`inert` attribute）

## 5. 改动量估算

| 文件 | 净增减 | 说明 |
|---|---|---|
| `internal/server/static/dashboard.js` | -180 / +320 ≈ +140 行 | 删 cronVisibleKeys 整套；renderCronPanel 改 layout；新增 drawer 渲染；ws 路由切条件 |
| `internal/server/static/dashboard.html`（CSS） | -40 / +80 ≈ +40 行 | drawer / detail 样式 |
| `internal/server/dashboard_session.go` | +5 行 | `/api/sessions` 过滤 `cron:` 前缀 |
| `internal/server/dashboard_session_test.go` | +30 行 | 列表过滤单测 |
| `docs/rfc/cron-panel-consolidation.md` | +新文件 | 本 RFC |
| `docs/rfc/README.md` | +1 行 | 索引登记 |

**不改后端 cron API / WS / model**，复用面 ≈ 90%。

## 6. 迁移步骤（建议 PR 拆分）

```
PR1  [doc]   本 RFC 落盘 + README 索引                      
PR2  [be]    /api/sessions 过滤 cron: 前缀 + 单测            
PR3  [fe-1]  删 cronVisibleKeys / sidebar cron 路径 / mainShell timeline 占位
            （此时点 cron 列表行短暂"无反应"——drawer 还没接上）
PR4  [fe-2]  cron 面板 layout 改双栏 + drawer 容器 + 复用 timeline 函数
PR5  [fe-3]  WS cron_run_* 事件路由从 selectedKey 切到 cronDetailJobId
PR6  [css]   清理 .sc-cron-card / 旧 .cron-card 样式残留
PR7  [test]  e2e: 创建 cron → 列表见行 → 点行见 drawer → 触发执行 → 看 running →
            完成后历史增加一行 → 删任务 drawer 自动关
```

PR3-PR5 可一并发，体感更连贯；分开是为了便于 review。PR2 单独发以免把 fe 改动卡住。

## 7. 测试矩阵

| 场景 | 期望 |
|---|---|
| 创建 cron | drawer 自动打开新建那条 |
| 点击列表行 | drawer 切到该 jobId；列表无视觉跳动 |
| 当前 drawer 打开时 cron_run_started 该 jobId | drawer 内"当前执行"section 出现 + 列表行徽章变运行中 |
| 当前 drawer 打开时 cron_run_started **其他** jobId | drawer 不动；列表行徽章变运行中 |
| 删任务 | drawer 关 + toast |
| F5 重载 | 重回 cron 面板，drawer 关，列表正常 |
| sidebar | 永远看不到 cron 行（即使 cron 任务正在跑） |
| `/api/sessions` 直接 curl | 不返回任何 `cron:<id>` 条目 |
| 移动端窄屏 | 点击行 → drawer 全屏推入 + 返回箭头 |
| 已有 e2e `TestDashboardJS_R110P2_CronRunNowButton` | 仍 pass（断言的是源码字面量，不依赖 mainShell） |

## 8. 风险与回退

| 风险 | 缓解 |
|---|---|
| 老用户已经习惯"点 cron 行进 events 视图看实时输出" | drawer 的"当前执行" section 二期可加 `[查看实时输出]` chip 串到 events readonly；MVP 公告里说明"实时输出已并入面板，events 视图不再用于 cron" |
| `RegisterCronStub` 留着，但前端永远看不到 → 数据 leak | 已在 §4.3 方案 A 收口：`/api/sessions` 列表层过滤；WS subscribe 路径仍可按需访问 |
| cron drawer 内 timeline + current_run 信息密度过高 | 本 RFC 默认垂直栈 + drawer 自身 max-height 60vh + 内部 sub-scroll；二期视情况加折叠 |
| 收编后某些 IM 调试场景丢失（运维想看 cron stub 在 sessions.json 里的状态） | sessions.json 文件本身仍然有 `cron:<id>` stub；只是 dashboard 不渲染。`naozhi doctor` / 直接读文件仍可见 |
| 回退成本 | 每个 PR 单独 revert 即可；PR2 后端过滤可单独 revert，PR3-5 前端整组回退 |

## 9. 决策记录

- **Q: 为什么不把 cron 也变成 sidebar 的"项目"分组（像 favorite 那样）？**
  A: 项目分组本质是 IM/对话语义的归类。cron 是不同维度的对象（调度规则 + 历史 run 集合，不是对话）。强行塞进会让操作员把 sidebar 当成"什么都能放的导航"，反而更乱。

- **Q: 抽屉关闭后 cronDetailJobId 是否持久化？**
  A: 不持久。F5 / 重连 / 切 tab 回来都重置为 null。理由：cron 详情不是"上次工作进度"，是即时查询行为；持久化反而让用户误以为有 unread 状态。

- **Q: drawer 里的 `[立即执行]` 按钮按下后，cron run 完成的 IM 通知会不会因为 mainShell 不切到 cron session 而看不见？**
  A: IM 通知本身走飞书等渠道，跟 dashboard 视图无关。dashboard 内的提示是 announce + cron-badge + drawer 内 timeline 滚出新行；不依赖 mainShell。

- **Q: 现有的 `cron_result` legacy 事件还要不要兼容？**
  A: 要。老 naozhi 后端只发这个；处理路径 `fetchCronJobs + renderCronPanel` 不动，drawer 通过 cronJobs 重渲染自动同步。

## 10. 验收标准

- [ ] sidebar 在任何状态下都看不到 `cron:` 前缀的会话
- [ ] mainShell 不再含 `#cron-timeline-panel` DOM
- [ ] `/api/sessions` 不返回 cron stub
- [ ] 「定时任务」面板内可完成所有 cron 操作（看列表 / 看当前执行 / 看历史 / 看 run 详情 / 跳 session_id 提示 / 触发执行 / 暂停 / 编辑 / 删除）
- [ ] 已有的 cron-run-history e2e + cron-v2-polish e2e 全 pass
- [ ] 老 IM `/cron list/add/del` 行为不变
