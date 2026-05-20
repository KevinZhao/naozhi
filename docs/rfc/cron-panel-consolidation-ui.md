# Cron 面板收编 — UI/UX 详细设计

> 配套文档：[cron-panel-consolidation.md](cron-panel-consolidation.md) §4
> 日期：2026-05-20
> 范围：仅前端视觉与交互。后端契约 / API / WS 不在本文。

---

## 0. 设计原则

1. **节奏一致**：与 dashboard 现有令牌（`--nz-bg-*` / `--nz-border` / `--nz-accent` / 圆角 6–10px / `cj-row` 高密度行）零偏差，避免 cron 面板看起来像"另一个产品"。
2. **信息分级清晰**：操作员 80% 时间在扫列表（哪些跑了？哪些挂了？），15% 在看某条任务概要，5% 才会展开 run 详情。视觉权重必须严格按这个倒推。
3. **动作的代价感**：删除 / 立即执行 / 暂停 — 三个不可逆度不同的操作要有不同视觉权重，而不是四个并排的灰色按钮。
4. **不抢焦点**：cron drawer 打开不改 sidebar 选择、不切 mainShell、WS 实时事件只在抽屉内响应。
5. **键盘第一**：每一个鼠标交互都有键盘对应（Tab/Enter/Space/Esc/方向键）。
6. **降级优雅**：网络断 / 后端 5xx / 任务被并发删除 — 都有明确的视觉与文案兜底，不是空白屏。

---

## 1. 信息架构

```
定时任务面板 (1 屏)
├─ 顶栏          标题 / 摘要 / 新建按钮
├─ 筛选条        搜索 + 状态 chips + 排序
├─ 列表 (左)     任务行 × N
└─ 详情抽屉 (右) ← 点击列表行才出现，关闭后不留痕
   ├─ 抽屉头     任务名 + schedule chip + 关闭
   ├─ 概要区     时间 / 工作目录 / 通知 / fresh / 提示词 (可折叠)
   ├─ 操作行     立即执行 · 暂停/恢复 · 编辑 · 删除
   ├─ 当前执行   仅 running 时渲染
   ├─ 执行历史   时间轴 (默认显示 10 条 + 加载更多)
   └─ 底部       run 详情展开区 (在历史时间轴行内联展开)
```

### 1.1 不在抽屉里的内容

- **events scroll** — cron 不再是会话，没有 events 概念
- **input bar / 发送按钮 / 语音按钮 / 文件上传** — 抽屉是只读视图（修改走编辑模态）
- **subagents / cost bar / context bar** — 这些是会话级 metric，cron 用 CronRun 自己的统计

---

## 2. 布局规范（响应式）

> Round 2 review 修订：原断点没扣掉 sidebar (360px)。1280 屏 - 360 sidebar - 380 列表 - 1px 分割 - padding ≈ 535px，达不到原文 600。下表是按 **main 区域可用宽度**（不是浏览器宽度）算的。

四个断点，规则严格：

| Main 可用宽度 | 浏览器宽度参考（含 sidebar 360） | 列表 | 抽屉 | 行为 |
|---|---|---|---|---|
| ≥ 1100 | ≥ 1460 | 380px | 剩余 (≥ 720) | 抽屉与列表并列；列表内部独立滚动；抽屉内部独立滚动 |
| 820 – 1099 | 1180 – 1459 | 360px | 剩余 (≥ 460) | 同上，列表宽度收紧 |
| 560 – 819 | 920 – 1179 | 320px | 剩余 (≥ 240) | 抽屉宽度被挤压；当 < 460 时自动切**单栏覆盖**模式（点行 push 抽屉，与手机一致） |
| < 560 | < 920 | 100% | 100%（push 覆盖） | 列表满屏，点击行 push 抽屉到 z=1，左上角 ← 返回 |

**触发"单栏覆盖"的真正条件**是抽屉可用宽度 < 460px（不是浏览器宽度），用 `ResizeObserver` 监听 main 容器，宽度跨阈值时切换布局。这样窄屏 + sidebar 拉宽 / 用户拖窄 sidebar / 横屏手机 都能拿到一致的体验。

### 2.1 双栏布局（PC）

```
┌──────── main 区域 ─────────────────────────────────────────────────┐
│ ← 16px ──┬─────── flex:0 0 380px ──────┬── 1px border ──┬─ flex:1 │
│          │                               │                │         │
│  ┌─ 顶栏 ────────────────────────────────────────────────────────┐ │
│  │  定时任务  •  3 运行中 · 1 需关注                  [+ 新建]   │ │
│  └────────────────────────────────────────────────────────────────┘ │
│  ┌─ 筛选条 ──────────────────────────────────────────────────────┐ │
│  │ [🔍 搜索…________________]    全部 活跃 需关注    排序▾     │ │
│  └────────────────────────────────────────────────────────────────┘ │
│                                                                       │
│  ┌─ 列表 ──────────────┐ │ ┌─ 抽屉 ─────────────────────────────┐  │
│  │ ● 每日机器人巡检    │ │ │ 每日机器人巡检                  ✕ │  │
│  │   每天 09:00     ✓✓ │ │ │ 每天 09:00 · /opt/work · 通知开    │  │
│  │ ──                  │ │ │ ───────────────────────────────── │  │
│  │ ● 文档同步          │ │ │ ▷ 立即执行  ⏸ 暂停  ✎ 编辑  🗑 删 │  │
│  │   每 30m  运行中 12s│ │ │ ───────────────────────────────── │  │
│  │ ──                  │ │ │ 当前执行                           │  │
│  │ ○ 周报草稿         │ │ │  ● 运行中 · 0:14 · phase send     │  │
│  │   已暂停           │ │ │ ───────────────────────────────── │  │
│  │ ──                  │ │ │ 执行历史 (32 次, 92% 成功)         │  │
│  │ ● 财报抓取         │ │ │  ▼ 5/20 09:00  succeeded  18s     │  │
│  │   2小时前 失败    !│ │ │     prompt …                       │  │
│  │                     │ │ │     result …                       │  │
│  │                     │ │ │  ▸ 5/19 09:00  succeeded  21s     │  │
│  │                     │ │ │  ▸ 5/18 09:00  failed     —       │  │
│  │                     │ │ │  ▸ …                               │  │
│  │                     │ │ │  [加载更多]                        │  │
│  │                     │ │ │                                    │  │
│  │ (sticky)            │ │ │ (独立滚动)                         │  │
│  └─────────────────────┘ │ └────────────────────────────────────┘  │
└───────────────────────────────────────────────────────────────────────┘
```

### 2.2 单栏布局（手机）

```
列表态 (默认):                       抽屉态 (push 覆盖):
┌──────────────────┐                 ┌──────────────────┐
│ 定时任务   [+]   │                 │ ← 每日机器人巡检 │
│ 搜索 / 筛选     │                 │ schedule · cwd   │
│ ━━━━━━━━━━━━━━ │                 │ ─────────────── │
│ ● 每日机器人巡检 │ 点击行          │ ▷ 立即  ⋯       │
│   每天 09:00 ✓✓ │ ─────►          │ ─────────────── │
│ ● 文档同步      │                 │ 当前执行 …      │
│   运行中 12s    │ ◄─── ←/Esc      │ 执行历史 …      │
│ …               │                 │ …               │
└──────────────────┘                 └──────────────────┘
```

抽屉用 `position:absolute; inset:0; z-index:5; background:var(--nz-bg-0)` 覆盖列表（不是模态），保证后退按钮 / 系统返回手势能直观回列表。列表在抽屉打开时设 `inert` 防焦点穿透。

---

## 3. 列表行（沿用 cj-row，微调）

cj-row 已经做得很到位（Round 145 三态色条 + 状态点 + when 列 + stats 徽章），**本次只加一处选中态**：

### 3.1 选中态（新）

抽屉打开时，对应行获得 `.cj-row.is-active`：

```css
.cj-row.is-active{
  background:#1f2937;
  box-shadow:inset 4px 0 0 var(--nz-accent);    /* 4px 比 3px 错误条粗 1px，视觉优先级清晰 */
}
.cj-row.is-active .cj-title{color:var(--nz-text-strong);}

/* 错误 / missed 不丢失：保留 1px 内描边作为二级强调 */
.cj-row.is-active.is-error{
  box-shadow:inset 4px 0 0 var(--nz-accent), inset 0 0 0 1px rgba(218,54,51,.45);
}
.cj-row.is-active.is-missed{
  box-shadow:inset 4px 0 0 var(--nz-accent), inset 0 0 0 1px rgba(210,153,34,.45);
}
.cj-row.is-active.is-running{
  box-shadow:inset 4px 0 0 var(--nz-accent);
  background:#1c2939;
}
```

> Round 2 review 修订：原方案"4px accent"和"3px error"同走 box-shadow inset，盖住后红色完全丢失。改为：accent 4px 主色条 + 1px 内描边表达 error/missed，三态可视。

### 3.2 ⋯ 菜单瘦身

现状菜单 5 项（运行 / 编辑 / 暂停 / 复制 ID / 删除）。把"运行"挪到 ghost button 后，菜单只剩 4 项：

```
┌─────────────────┐
│ ✎ 编辑           │
│ ⏸ 暂停 / ▶ 恢复  │
│ ⎘ 复制任务 ID    │
│ ─────────────── │
│ 🗑 删除          │
└─────────────────┘
```

行点击不再走 `selectSession`，改 `openCronDetail(jobId)`；菜单项里也不再有"打开会话"——这是改动的核心隐含变化。

### 3.3 hover 行为不变

ghost `▷ 运行` 仅在 hover/focus 时浮出（已有），触屏 `@media(hover:none)` 始终显示（已有）。stats 徽章 hover popover（已有 5 状态点 + 元信息）。

---

## 4. 抽屉详细设计

抽屉是这次改动的主战场。从上到下 6 段，间隔 16–20px，每段独立可读。

### 4.1 抽屉头（约 72px，sticky 顶部）

```
┌─────────────────────────────────────────────────────────────┐
│  每日机器人巡检                              [⎘ ▾] [✕]      │  44
│  每天 09:00 (CST)  ·  /opt/work/.../check  ·  id 7f2e       │  28
└─────────────────────────────────────────────────────────────┘
```

> Round 2 review 修订：
> - 原方案"44px sticky 头"实测放不下两行（标题 18px + 行高 + chip 22px ≈ 70px）。改写实际高度。
> - "复制按钮 ⎘"原方案默认复制完整 JSON — 一键复制大对象在企业 Slack/工单里粘贴会刷屏。改为**带下拉菜单**（默认行为是复制 ID，菜单二选项是复制完整 JSON）。
> - 单行写元数据用 ` · ` 分隔，比三个独立 chip 更省空间，且与 cron-v2 风格一致。

- **任务名** 18px / weight 600 / `--nz-text-strong`，单行 ellipsis，hover full-tooltip
- **第二行元数据**（`var(--nz-text-mute)` 12px）：
  - schedule（复用 `.cj-schedule` dotted underline accent 色，点开编辑模态聚焦到 schedule 字段）
  - workdir（mono，中段省略 `/opt/work/.../check`，hover 完整路径，点击复制）
  - id（`id 7f2e` 8 字符 mono）
- **复制按钮 ⎘ ▾** 36×28 split button：主区点击 = 复制任务 ID（最常用），▾ 区点击展开菜单：
  - 复制任务 ID（`abc123ef`）
  - 复制完整任务 JSON（带 prompt / schedule / workdir / notify 全字段）
  - 复制最近一次 run_id
- **关闭 ✕** 36×36，Esc 等价；按下后焦点回上次点击的列表行

sticky 让你滚到时间轴 50 条之后，仍能看到"我在哪个任务上"+ 关闭按钮。

### 4.2 概要区（紧凑信息表，可折叠）

默认展开。一行四列 grid：

```
┌─ 概要 ──────────────────────────────────────────────── ⌃ ┐
│ 提示词                                                    │
│   ┌───────────────────────────────────────────────────┐  │
│   │ 检查所有飞书机器人状态，列出错误最多的 3 个       │  │
│   │ 服务，并给出修复建议。                            │  │
│   └───────────────────────────────────────────────────┘  │
│                                                            │
│ 通知    🔔 飞书 oc_xxxx (默认)        Fresh    ✓ 每次重置  │
│ 目录    /opt/work/health-check        Backend  Claude (CC) │
└────────────────────────────────────────────────────────────┘
```

- **提示词** 是这块的头牌，3 行 fold（4em line-height-clamp），点 `展开 ▾` 全文
- **元数据** 4 项：通知 / Fresh / 目录 / Backend，标签 `--nz-text-mute` 11px，值 13px
- **折叠按钮 ⌃ / ⌄** 节省垂直空间（在长任务列表 / 长 prompt 场景下）
- 通知关闭时显示 `🔕 关闭`，灰色

### 4.3 操作行（单行四按钮）

```
┌──────────────────────────────────────────────────────────────────┐
│  [▷ 立即执行]  [⏸ 暂停]  [✎ 编辑]                       [🗑 删除] │
└──────────────────────────────────────────────────────────────────┘
```

| 按钮 | 视觉权重 | 启用规则 | 状态文案 |
|---|---|---|---|
| **立即执行** | primary（accent 描边 + accent 文字）| 同时满足：`!paused` ∧ `!current_run` ∧ `!just_triggered`（10s 防抖） | 触发后→`触发中…` 1s spinner →`已派发 ✓` 2s →复位 |
| **暂停 / 恢复** | secondary（灰描边）| 总可用 | paused 时变 `▶ 恢复`（绿描边） |
| **编辑** | secondary | 总可用 | 打开现有 cron-modal |
| **删除** | tertiary（红字 + 透明背景），右对齐 | 总可用 | 二次确认对话框，倒计时 3s 才能 Confirm |

> Round 2 review 修订：
> - 原方案只说"paused 时禁用"，但 §5 状态机里又说 running 时按钮变 spinner — 实际有 4 种 disable 原因要分别提示，列在这里。
> - 立即执行触发后应**立即视觉反馈**（防止操作员重复点击；后端 API 是异步的，等到 cron_run_started WS 事件回来需要 200-500ms）：先 spinner 锁住，再"已派发 ✓"，再复位。
> - **删除倒计时改回 3s** 与 §7.5 一致（原文 §4.3 写 5s，§7.5 写 3s，矛盾；以更克制的 3s 为准）。

危险操作（删除）远离常规操作，符合 GitHub / Linear / Stripe 的"危险靠右下"惯例。

### 4.3.1 立即执行的 disable 矩阵

| 状态 | 按钮 | tooltip |
|---|---|---|
| 正常 | `▷ 立即执行`（启用） | — |
| paused | `▷ 立即执行`（disabled，灰）| 已暂停。请先恢复任务。 |
| 正在 running | `▷ 运行中…`（disabled，pulse） | 上一次执行尚未完成，请等待结束。 |
| 10s 内刚触发过 | `▷ 已派发 ✓`（disabled）| 刚已触发一次，请稍候。 |
| 后端 5xx 失败 | `▷ 立即执行`（启用，但显示 inline error） | (error inline 在按钮下方显示 retry 入口) |

### 4.4 当前执行段（条件渲染）

仅当 `j.current_run` 存在时显示。设计目标：**在不打断列表节奏的前提下，让用户秒看到"它现在在做什么"**。

```
┌─ 当前执行 ───────────────────────────────────────────────┐
│  ┌──── 实时卡片 ──────────────────────────────────────┐  │
│  │  ●─运行中  •  0:14  •  按计划                       │  │
│  │  ──────────────────────────────────────────────── │  │
│  │  阶段: 等待 CLI 响应                                │  │
│  │  run 7f2e1d4a  •  session 4a8c…                    │  │
│  └─────────────────────────────────────────────────────┘  │
│  [中断本次执行]                                           │
└────────────────────────────────────────────────────────────┘
```

- 蓝色 `--nz-accent` 边框 + `cjRunPulse` 1.4s 脉动（已有 keyframe）
- 大计时器 `0:14` 24px / mono / tabular-nums，每秒 tick（沿用 `cronRunningTickTimer`）
- **trigger 用中文标签**（按计划 / 手动触发 / 错过补跑），与 §7.3 对齐；不再露 `scheduled` / `manual` / `catchup` 英文枚举
- **phase 用中文友好文案**：dispatch→`已派发，等待调度`；send→`等待 CLI 响应`；waiting→`等待中`；后端没给 phase 时显示 `执行中…`（不显示 `等待中…` — 它和 waiting 重名容易混）
- run_id 显示 8 字符（与历史时间轴一致），session_id 显示 4 字符 + `…`（避免抢眼）
- **中断按钮**：见下方 4.4.1

> Round 2 review 修订：原方案直接给 SR 看 `phase: send` 这种英文枚举；中文化文案补到这里。同时 trigger 也用中文与历史时间轴 §7.3 对齐。

完成后该段动画淡出（200ms），新行追加到时间轴顶部，从视觉上形成"running → 落到历史"的连续感。

### 4.4.1 中断按钮的处理（Q2 决议）

> **决议：不渲染**（不做 disabled 占位）

理由：
- 后端 `/api/cron/runs/<run_id>/interrupt` 没实现，渲染 disabled 按钮 + tooltip "功能开发中" 是 dark pattern — 用户会反复试，最后骂"为什么明明有按钮还点不动"。
- 现有兜底：用户想中断，可以走「编辑 → 暂停」组合（暂停后下一轮调度跳过；当前 run 自然结束 by jobTimeout）。
- 当后端实现后，再加一行 conditional render，类名 + 行为已经在本节预留。

代码层面用 `if (CRON_INTERRUPT_ENABLED) { renderInterruptButton() }` feature flag 兜住，flag 默认 false。

### 4.5 执行历史段（时间轴）

直接复用 `cronTimelineHtml/RowHtml/DetailHtml` 三件套，**只改容器位置 + 微调样式**：

```css
/* 从 mainShell 顶部 panel 改为 drawer 内 section */
.cron-detail-pane .cron-timeline-panel{
  padding: 0;                           /* 与 drawer body padding 合并 */
  background: transparent;              /* 与 drawer 背景一致 */
  border: none;                         /* 用 ct-head 自己的下分割线 */
  max-height: none;                     /* drawer 自己负责滚动 */
  overflow: visible;
}
.cron-detail-pane .ct-head{
  position: sticky; top: 0;
  background: var(--nz-bg-0);
  padding: 12px 0 8px;
  border-bottom: 1px solid var(--nz-border);
  z-index: 2;
}
```

行内展开 detail 的 prompt/result code block 已经做了 max-height:240 + 内部滚动（`.ctr-block-body`），不需要改。

### 4.6 fresh-context hint 重写

现状：`ctr-fresh-hint` 里有个 chip "跳到 session"，调用 `cronTimelineJumpToSession` 切到 sidebar 上的 cron session。**收编后 sidebar 没有 cron session**，必须改。

> Round 2 review 修订：原方案的 hint 文案"这条 run 与之前的 run 共享一个 CLI 会话"对非技术运营者完全不知所云。改写更"人话"：

```
新 hint（fresh=false 模式）:
  ┌─ ⓘ 这次执行接续了上一次的对话上下文 ───────┐
  │  本任务关闭了「每次重置」，CLI 会记住之前的   │
  │  对话内容。session 4a8c…                      │
  │  [⎘ 复制 session ID]   [查看 JSONL 路径]      │
  └────────────────────────────────────────────────┘

新 hint（fresh=true 模式 — 之前没有，本期补上）:
  ┌─ 🔄 已重置上下文 ─────────────────────────────┐
  │  本任务每次执行前都会重置 CLI 会话，          │
  │  与之前的 run 互不影响。session 4a8c…         │
  └────────────────────────────────────────────────┘
```

`查看 JSONL 路径` 弹 toast 显示 `~/.claude/projects/<cwd>/<sid>.jsonl` 完整路径 + 复制按钮，给运维 / 调试用。

> 为什么补 fresh=true 的 hint：原方案只在 fresh=false 时显示，fresh=true 留空。但操作员看到 detail 里没任何上下文说明，会困惑"为啥这条 run 和 5/19 那条不连续"。两种模式都给一句话说明，体验对称。

### 4.7 空态（没有任何 run）

```
┌─ 执行历史 ──────────────────────────────────────────────┐
│                                                          │
│           ⏰                                             │
│      还没有执行记录                                      │
│      下次调度（明天 09:00）会自动触发，                  │
│      或点上面的「▷ 立即执行」试一次。                    │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

文案给两条出路（等下一次调度 / 主动触发），不让用户面对空白干瞪眼。

---

## 5. 状态机（视觉上的 10 种主状态）

> Round 2 review 修订：补 2 个原文漏掉的状态（**I. 抽屉打开后任务被并发删除** 与 **J. WS 重连**），错误降级在 §8 全表，状态机这里只保留视觉切换。

| # | 触发 | 视觉表现 |
|---|---|---|
| **A. 列表正常** | 默认 | cj-row 三色条规则不变；选中态 4px accent 实色条 |
| **B. 抽屉打开** | 点击行 | 行 `.is-active`；抽屉滑入 200ms 从 `translateX(8px)` 到 0 + opacity 0→1 |
| **C. 抽屉切换 jobId** | 点击另一行 | 抽屉**不**滑出再滑入；内容直接 swap + 标题 240ms flash；列表上**新行**也 flash 一次（旧行无动画恢复）；timeline state 切到新 jobId |
| **D. running** | WS `cron_run_started` | 列表行运行中徽章 + 抽屉头计时器 + "当前执行"段插入；列表 stats 不变（直到 ended）|
| **E. 完成** | WS `cron_run_ended` | "当前执行"段淡出 200ms；时间轴顶部 prepend 新行 + `ctr-flash` 黄色边框 1s 渐淡；列表 stats refresh |
| **F. paused** | 用户点暂停 | 列表行 `.paused` 透明度 .6；抽屉运行按钮变灰 + tooltip；恢复按钮绿色 |
| **G. 删除中** | 用户点删除 | 二次确认 modal → 倒计时 `[ 删除 (3) ]`；确认后行渐隐 200ms 移除；抽屉淡出关闭 |
| **H. 错误** | 5xx / 网络断 | 抽屉内对应段显示红色 inline error + 重试按钮；不强制关抽屉 |
| **I. 远端删除** | 抽屉打开时 fetchCronJobs 该 jobId 缺失 | 抽屉淡出 → toast `「<title>」已被删除` → 焦点回列表第一行；时间轴 state 在内存里清掉 |
| **J. WS 重连** | wsm 断后恢复连接 | 列表顶部 amber `WS 已断开 12s` 横条淡出；抽屉计时器恢复 tick；触发一次 fetchCronJobs 重对账 stats |

### 5.1 抽屉过渡动画

```css
.cron-detail-pane{
  transform: translateX(0);
  opacity: 1;
  transition: transform 200ms cubic-bezier(.2,.7,.2,1), opacity 160ms ease;
}
.cron-detail-pane.entering{transform:translateX(8px);opacity:0}
.cron-detail-pane.leaving{transform:translateX(8px);opacity:0;pointer-events:none}

@media (prefers-reduced-motion: reduce){
  .cron-detail-pane,
  .cron-detail-pane.entering,
  .cron-detail-pane.leaving{transform:none;transition:opacity 120ms ease}
}
```

**禁用动画**：`prefers-reduced-motion` 下只有 opacity 淡入；移动端 push 也走纯 opacity（避免引发眩晕）。

### 5.2 列表 / 抽屉的视觉绑定

抽屉切换 jobId 时，列表里**对应行**会快速闪一下 → 让用户视觉上把"这个抽屉是哪条任务"和列表锁定。`flash` keyframe：

```css
@keyframes cjActiveFlash{0%{background:rgba(88,166,255,.25)}100%{background:#1f2937}}
.cj-row.is-active.flashing{animation:cjActiveFlash 240ms ease-out}
```

200ms 内非常轻，不让人感觉跳。

---

## 6. 交互细节

### 6.1 鼠标流

| 操作 | 反馈 |
|---|---|
| 点击列表行（不在按钮上）| 打开抽屉 + 行 active + 闪 |
| 点击 ⋯ | popover 菜单 |
| 点击 ⋯ 之外 | popover 关闭 |
| 点击抽屉外（PC）| **不**关抽屉（避免误触；只能 ✕ 或 Esc 关）|
| 点击抽屉外（手机）| 不存在（push 模式没有"外"）|
| 点击抽屉头任务名 | 打开重命名 inline 输入（沿用 dashboard rename） |
| 点击 schedule chip | 打开编辑模态聚焦 schedule |
| 点击时间轴行 | 行内展开 detail（沿用） |
| 点击 fresh hint chip | toast 复制 session_id（旧"跳转"语义已废） |
| 点击删除 | 二次确认 modal |

### 6.2 键盘流

> Round 2 review 修订：删掉 `R / P / E / Delete` 四个字母快捷键。理由：
> 1. 抽屉的 4 个按钮已经 Tab 可达，accelerator 只省 1 次按键；
> 2. 用户切到抽屉内输入框（搜索 / 编辑 prompt）后字母键应该是输入字符，不是触发动作 — 全局监听容易误触发；
> 3. dashboard 已有 30+ 全局快捷键（`/cron`、`Alt+↑↓`、`Esc`、`Cmd+K` 等），再加 4 个 cron-only 字母键反而稀释快捷键学习曲线。

| 键 | 上下文 | 动作 |
|---|---|---|
| `↑ / ↓` | 列表行获焦 | 上下移动焦点 |
| `Home / End` | 列表行获焦 | 跳到首/末行 |
| `Enter / Space` | 列表行获焦 | 打开抽屉 |
| `Enter` | 列表 ⋯ 按钮获焦 | 打开 popover 菜单 |
| `Tab` | 抽屉获焦 | 在抽屉内 6 段间循环 |
| `Shift+Tab` | 抽屉获焦 | 反向循环 |
| `Esc` | 抽屉获焦 | 关闭抽屉 + 焦点回上次行 |
| `Esc` | 列表 ⋯ popover / 删除 modal 打开时 | 仅关 popover/modal，不动抽屉 |
| `?` | 任意（非 textarea 内） | 打开快捷键帮助（已有 keymap modal） |

### 6.3 移动端手势

| 手势 | 动作 |
|---|---|
| 列表行点击 | push 抽屉 |
| 抽屉左缘右滑（>30%）| 返回列表（系统级返回手势）|
| 抽屉头 ← 按钮 | 返回列表 |
| 抽屉内时间轴行 tap | 展开 detail |
| 抽屉内长按时间轴行 | 弹出 ⎘ 复制 run_id 菜单 |
| 列表卡 ⋯ 长按 | 弹 popover 菜单（已有 `@media(hover:none)` 触屏适配） |

### 6.4 焦点管理细则

> Round 2 review 修订：原方案"打开抽屉时焦点移到 ✕"对键盘流非常糟糕——用户 Enter 进抽屉后第一个 Tab 落到 ✕，再按一次就关掉了。改为焦点落在抽屉头**任务名**，更接近视觉重心。

- 抽屉打开时焦点移到抽屉头**任务名**所在的 `<h2>` 容器（`tabindex="-1"` programmatic focus），SR 立即朗读完整任务名
- 第一次按 Tab 进入操作行（▷ 立即执行 → ⏸ → ✎ → 🗑）
- 抽屉关闭时焦点回到列表上次激活的行（用 `lastActiveRowEl` 记录），SR 朗读"返回任务列表，X / Y 行"
- 抽屉切 jobId 时焦点保持在抽屉头任务名（避免每次都跳关闭按钮）
- 删除任务后焦点跳到列表中**该行的下一行**（如果有），其次上一行，再次空列表跳「+ 新建」按钮 — 比直接跳第一行更符合"删完看到下一个"的语义
- **抽屉打开时 sidebar 不变更焦点**（cron 与对话独立，不抢焦）

---

## 7. 文案规范

### 7.1 状态标签（`cronStateLabel` 输出）

| 后端枚举 | 中文 | 颜色 |
|---|---|---|
| `running` | 运行中 | accent |
| `succeeded` | 成功 | green |
| `failed` | 失败 | red |
| `skipped` | 跳过 | text-faint |
| `timed_out` | 超时 | amber |
| `canceled` | 取消 | purple `#a371f7` |

### 7.2 错误分类标签（`cronErrorClassLabel`）

沿用现有映射，同时给运维者一个"了解更多"链接到 `docs/ops/cron-errors.md`（待写）。

### 7.3 触发来源

| 后端 | 中文 |
|---|---|
| `scheduled` | 按计划 |
| `manual` | 手动触发 |
| `catchup` | 错过补跑 |

### 7.4 时间格式

- 当前执行计时：`m:ss`（< 1h）/ `H:mm:ss`（≥ 1h）
- 历史行紧凑时间：`5月20日 14:30`（同年）/ `2025年12月3日 14:30`（跨年）
- next_run 倒计时：`即将（< 1m）` / `2 分钟后` / `下午 3:30` / `明天 09:00` / `5月23日 09:00`
- last_run：`刚刚` / `5 分钟前` / `今天 14:30` / `昨天 09:00` / `5月19日 09:00`
- hover tooltip 全部显示绝对 ISO，sourced from `formatAbsTime`

### 7.5 危险操作文案

```
删除「每日机器人巡检」？
─────────────────────────────────────────────────
此操作将永久删除该任务及其全部 32 次执行记录，
不可撤销。

CLI 的对话历史 JSONL 文件保留在磁盘，需要时
可在终端用 `claude --resume <session_id>` 复活；
session_id 在执行历史的「详情」里能找到。

[ 取消 ]                              [ 删除 (3) ]
                                       ↑ 倒计时
```

倒计时 3s 是有意的"减速带"，不是惯例的 5s — 太长会让人不耐烦，太短挡不住误触。

> Round 2 review 修订：原文案直接给非工程操作员看 `claude --resume` 命令是越界的（运营 / PM 用户不会进终端）。改为更清楚地告诉用户"哪里能找到 session_id"+"在终端里执行"，让需要的人能找到路径，不需要的人不被吓到。

**当任务正在执行时的特殊文案**：

```
删除「每日机器人巡检」？
─────────────────────────────────────────────────
⚠ 该任务正在执行（已运行 0:14）。

删除后任务定义和 32 条历史记录立即清除，
但当前正在跑的这次执行将继续运行直到完成
（CLI 子进程不会被强行 kill）。完成结果不会
被记录到任何地方。

[ 取消 ]                              [ 删除 (3) ]
```

这是 §5 状态机 G 的细化分支，原方案漏了这一种。

---

## 8. 错误状态与降级

> Round 2 review 修订：补 4 行（401/403、429、cors、本地时钟漂移），原方案的"5xx"是个粗箩筐，逐个 status 文案差异大。同时给每个错误一个明确的"用户下一步"。

| 场景 | UI 表现 | 文案 + 下一步 |
|---|---|---|
| WS 断 < 8s | 不显示横条（避免抖动） | — |
| WS 断 ≥ 8s | 列表顶部 amber 横条 + 手动重连按钮；抽屉运行计时器停转改显示静态 `0:14（数据滞后）` | `WS 已断开 N 秒` · `[手动重连]` |
| WS 重连成功 | 横条 200ms 淡出；抽屉计时器恢复；触发一次 fetchCronJobs 对账 | `连接已恢复` toast 1.5s |
| `/api/cron` 401/403 | **不**替换列表；唤起 authModal（与 dashboard 全站策略一致） | (auth modal) |
| `/api/cron` 429 | inline 错误卡 + 重试倒计时（用 Retry-After 头） | `请求过于频繁，X 秒后重试。` |
| `/api/cron` 5xx | 列表替换为 inline 错误卡 + 重试按钮 | `加载定时任务失败 (HTTP 503)。请稍后重试，或联系管理员。` |
| `/api/cron` 网络断（无 status） | 同 5xx，但文案不同 | `网络无法连接到 naozhi 服务。检查 dashboard 网址或 VPN。` |
| 抽屉打开后任务被并发删除 | 抽屉淡出 + toast | `「每日机器人巡检」已被删除` |
| 任务正在运行时被删 | 抽屉淡出 + toast 标 amber | `任务已删除。当前执行将运行到自然结束，结果不会被记录。` |
| 立即执行 409（并发限制） | inline error 卡片在抽屉操作行下方，3s 后自动消 | `上一次执行尚未完成，请等待后再触发。` |
| 立即执行 423（paused） | inline error 同上 | `任务已暂停，请先恢复任务。` |
| 立即执行 5xx | inline error + 重试 | `触发失败 (HTTP 503)。点击重试。` |
| 时间轴 fetch 失败（首屏） | 历史段替换为 inline error + 重试按钮 | `加载执行历史失败 (HTTP 503)。点击重试。` |
| 时间轴加载更多失败 | 「加载更多」按钮变红 + 重试图标，不替换已加载行 | `加载更多失败，点击重试。` |
| run 详情 fetch 404 | 行内展开区显示 inline | `这条记录已被清理（可能因保留期到期）。` |
| run 详情 fetch 5xx | 行内展开区显示重试 | `加载详情失败 (HTTP 503)。点击重试。` |
| 本地时钟漂移 > 5min | 列表底部 amber 横条 1 次（dismissable） | `检测到本地时间与服务器相差 X 分钟，相对时间显示可能不准。` |

---

## 9. 性能 & 渲染策略

### 9.1 列表 painting

- **不重绘整个列表** 当列表 stats 变化（cron_run_started/ended）：只重绘列表那一行（`document.querySelector('.cj-row[data-cron-id="..."]')`）+ 抽屉内时间轴 head section
- 列表 < 200 条全量渲染；≥ 200 时上 IntersectionObserver virtualize（**本期不做**，列出作为后续优化项）
- **筛选/搜索 keystroke**：debounce 100ms 后 patch DOM；不重绘抽屉

### 9.2 时间轴 painting

- 抽屉打开时显示 cronJobs.recent_runs 的 10 条（已在 cronTimelineState 里）
- 加载更多每次 +50 条（沿用）
- 切 jobId 时保留 timeline state 缓存（用户切回不重 fetch）
- 离开 cron 面板（点击 header 其它按钮）时**不**清缓存（在内存里，下次回来更快）
- **缓存上限**：cronTimelineState 最多保留 20 个 jobId 的缓存（LRU），超过时丢最旧的 — 防止打开 100+ 个任务后内存涨到 50MB+

> Round 2 review 修订：原方案"不清缓存"在大型部署（500+ cron job）会让 dashboard 内存无限增长。补 LRU 上限。

### 9.3 计时器

- **统一 1 个全局 1Hz `cronRunningTickTimer`** 扫所有 running job；抽屉打开时复用该 timer 顺便重绘抽屉头计时器 — **不开第二个 timer**（原方案两个 timer 重叠浪费）
- 计时器精度 1s 已足够，不用 requestAnimationFrame
- WS 断开 ≥ 8s 时停 timer + 抽屉计时器变灰显示 `0:14（数据滞后）`
- 抽屉关闭 + 列表无 running job 时停 timer（已有逻辑：`ensureCronRunningTick`）

### 9.4 渲染抖动控制（新）

> Round 2 review 新增：原方案没考虑"高频 cron"场景（每秒一次的 healthcheck job）。每个 ended 事件触发 fetchCronJobs + renderCronList + renderCronTimelinePanel 三个动作，CPU 100% 不夸张。

- WS `cron_run_started/ended` 事件**合流**：连续 200ms 内多个事件只触发 1 次 fetchCronJobs（`debouncedFetchCronJobs`）
- 列表行重绘：`requestAnimationFrame` schedule，下一帧合并所有变更
- 抽屉时间轴 head 刷新：使用 `cronTimelineRefreshHead` 已有的 in-flight token guard（防止覆盖式 race）

---

## 10. Token 与样式扩展

### 10.1 新增 token

```css
:root{
  /* cron drawer 专用 */
  --nz-cron-drawer-bg: var(--nz-bg-0);
  --nz-cron-drawer-section-bg: var(--nz-bg-1);
  --nz-cron-drawer-section-border: var(--nz-border);
  --nz-cron-running-bg: rgba(88,166,255,.08);
  --nz-cron-running-border: var(--nz-accent);
  --nz-cron-active-row-bg: #1f2937;
}
```

### 10.2 新增 keyframes

```css
@keyframes cjActiveFlash{0%{background:rgba(88,166,255,.25)}100%{background:var(--nz-cron-active-row-bg)}}
@keyframes ctrFlashNew{0%{box-shadow:0 0 0 2px rgba(210,153,34,.6)}100%{box-shadow:0 0 0 0 rgba(210,153,34,0)}}
```

### 10.3 类名清单（新）

| 类 | 用途 |
|---|---|
| `.cron-detail-pane` | 抽屉容器 |
| `.cron-detail-pane.mobile-fullscreen` | 移动端 push 状态 |
| `.cron-drawer-header` | 抽屉头 sticky 区 |
| `.cron-drawer-summary` | 概要折叠区 |
| `.cron-drawer-summary.collapsed` | 折叠态 |
| `.cron-drawer-actions` | 4 按钮操作行 |
| `.cron-drawer-current` | 当前执行段 |
| `.cron-drawer-current-card` | 蓝边脉动卡 |
| `.cron-drawer-current-empty` | 占位（hide 时不渲染） |
| `.cron-drawer-history` | 历史段容器 |
| `.cj-row.is-active` | 列表选中态 |
| `.cj-row.is-active.flashing` | 切 jobId 闪烁瞬态 |
| `.ctr.flashing-new` | 时间轴新增行黄色 1s 渐淡 |
| `.ctr-fresh-hint .ctr-jsonl-btn` | 替换旧"跳转 session" 的 JSONL 路径按钮 |

### 10.4 删除清单

| 类 | 原因 |
|---|---|
| `.session-card.sc-cron-card` | sidebar 不再有 cron 卡 |
| `.session-card .sc-cron` | cron 徽章字段一并删 |
| `.cron-card`（v2 旧版圆角大卡） | 列表 v3 已迁，drawer 不复用 |
| `.cron-detail .cd-field/.cd-label/.cd-value/.cd-result` | v2 旧 detail 字段表，drawer 自己用 cron-drawer-summary |

---

## 11. 可访问性（WCAG 2.1 AA）

| 项 | 实现 |
|---|---|
| 颜色对比 | 所有文本 ≥ 4.5:1（已用 `--nz-text` on `--nz-bg-0/1`，对比 11:1） |
| 焦点环 | 沿用 `:focus-visible` 全局规则，2px accent + 2px offset |
| ARIA 角色 | 列表 `role="list"`，行 `role="listitem"` + `role="button"`；抽屉 `role="region" aria-label="任务详情"`；当前执行段 `role="status" aria-live="polite"` |
| 键盘陷阱 | 抽屉打开不陷阱（不是 modal）；移动端 push 时 list 加 `inert` 防 Tab 穿透 |
| 屏幕阅读器播报 | cron_run_ended 时 announce(`定时任务 X 执行 Y`)（中文 polite live region） |
| reduced-motion | drawer 过渡 / 行 flash / running pulse 全部 fallback 为静态或 opacity-only |
| 字号 | 抽屉所有文字 ≥ 12px；正文 13–14px；标题 16–18px |
| 触屏目标 | 所有可点击 ≥ 36×36px |
| 高对比模式 | 测试 Windows High Contrast：accent 边框、3px 色条、sticky 头要求都应保留可见 |

---

## 12. mockup 像素级（HTML 草稿）

为了让团队能直接喂给 designer / 前端实现，附上一段**可粘贴运行**的 HTML 草稿（只验证视觉，没绑事件）：

```html
<div class="cron-detail-pane" role="region" aria-label="定时任务详情：每日机器人巡检">
  <header class="cron-drawer-header">
    <div class="cdh-row1">
      <h2 class="cdh-title">每日机器人巡检</h2>
      <div class="cdh-actions">
        <button class="cdh-btn-icon" aria-label="复制任务 ID">⎘</button>
        <button class="cdh-btn-icon" aria-label="关闭">✕</button>
      </div>
    </div>
    <div class="cdh-row2">
      <span class="cj-schedule" role="button">每天 09:00 (Asia/Shanghai)</span>
      <span class="cdh-chip mono">/opt/work/health-check</span>
      <span class="cdh-chip mono">7f2e</span>
    </div>
  </header>

  <section class="cron-drawer-summary">
    <div class="cds-prompt">
      <label>提示词</label>
      <p class="cds-prompt-body" data-clamp="3">
        检查所有飞书机器人状态，列出错误最多的 3 个服务，
        并给出修复建议。
      </p>
    </div>
    <div class="cds-meta">
      <div><label>通知</label><span>🔔 飞书 oc_xxxx (默认)</span></div>
      <div><label>Fresh</label><span>✓ 每次重置</span></div>
      <div><label>Backend</label><span>Claude (CC)</span></div>
    </div>
  </section>

  <nav class="cron-drawer-actions" aria-label="任务操作">
    <button class="cda-btn primary">▷ 立即执行</button>
    <button class="cda-btn">⏸ 暂停</button>
    <button class="cda-btn">✎ 编辑</button>
    <button class="cda-btn danger" style="margin-left:auto">🗑 删除</button>
  </nav>

  <section class="cron-drawer-current" role="status" aria-live="polite">
    <h3>当前执行</h3>
    <div class="cdc-card">
      <div class="cdc-row1">
        <span class="ctr-dot run" aria-hidden="true"></span>
        <span class="cdc-state">运行中</span>
        <span class="cdc-clock">0:14</span>
        <span class="cdc-trigger">按计划</span>
      </div>
      <div class="cdc-row2">
        phase: <code>send</code> · 等待 CLI 响应
      </div>
      <div class="cdc-row3 mono">run 7f2e · session 4a8c</div>
    </div>
    <button class="cdc-interrupt">中断这次执行</button>
  </section>

  <section class="cron-drawer-history">
    <header class="ct-head">
      <h3>执行历史 (32 次, 92% 成功)</h3>
      <span class="ct-head-err">最近一次：失败</span>
    </header>
    <div class="ct-rows">
      <!-- 复用现有 cronTimelineRowHtml 输出 -->
    </div>
    <div class="ct-more"><button class="ct-more-btn">加载更多</button></div>
  </section>
</div>
```

---

## 13. 实施顺序（与主 RFC §6 对齐）

UI/UX 改动建议在 PR4 一次完成，不切到 5 个细 PR 里 — 抽屉是一个完整的视觉单元，分阶段会让中间态难看。后端过滤 (PR2) 和 sidebar 删 cron (PR3) 可以单独走。

### 13.1 PR4 内的 commit 拆分（建议）

1. `refactor(dashboard): cron 列表 layout 改双栏容器` — 加 `.cron-detail-pane` div，列表宽度收紧，无功能 — 站点视觉无变化
2. `feat(dashboard): cron 抽屉头 + 概要 + 操作行` — 4.1 / 4.2 / 4.3
3. `feat(dashboard): cron 抽屉内当前执行段` — 4.4
4. `refactor(dashboard): cron 时间轴从 mainShell 迁到抽屉` — 容器位置切换 + CSS 调整
5. `feat(dashboard): cron 抽屉切换动画 + 选中态` — §5
6. `feat(dashboard): cron 抽屉移动端 push + ← 返回` — §2.2

每个 commit 独立可视，方便 review 时一段段对照 mockup。

---

## 14. Open questions（Round 2 review 后决议）

| Q | 决议 | 备注 |
|---|---|---|
| Q1: 抽屉头 schedule chip 点击 — 模态 vs 迷你 popover？ | ✓ **直接打开模态** | popover 与 §4.3 编辑按钮职能重复 |
| Q2: "中断这次执行"按钮 — disabled 占位 vs 不渲染？ | ✓ **不渲染**（feature flag 关，后端实现后开） | 见 §4.4.1 |
| Q3: 抽屉头 ⎘ 复制 — ID vs 完整 JSON？ | ✓ **split button**：默认复制 ID，▾ 菜单 3 项 | 见 §4.1 |
| Q4: 删除二次确认 — modal vs inline？ | ✓ **modal**，3s 倒计时 | 与 dashboard 其它 destructive 一致；§7.5 |
| Q5: 列表选中态在抽屉关闭后保留吗？ | ✓ **不保留**（关 = 完全 reset） | — |
| Q6: 时间轴第一次加载 — 预填 vs 等 fetch？ | ✓ **预填 + 后台对账**（已有逻辑） | recent_runs 上限 10 |
| Q7（新）: 抽屉打开后切到其他面板（agent/discovery）再回来，抽屉应保留吗？ | ✓ **保留**（cron 面板是有状态视图，与 sidebar 选择独立） | 但 F5 重载不持久化 |
| Q8（新）: 列表 ⋯ 菜单和抽屉操作行的"暂停/恢复"是否会双向同步？ | ✓ **同步**（共用 `cronPause/cronResume`，乐观更新） | 任一处点暂停，列表行 + 抽屉按钮同时变化 |
| Q9（新）: 抽屉里的 prompt 长文本可以原地编辑吗？ | ✗ **不行**（必须走编辑模态） | 编辑 prompt 涉及 schedule/workdir 重新校验，原地编辑容易留半态；模态有 Cancel 兜底 |
| Q10（新）: 移动端 push 抽屉是否阻止系统手势（左滑返回）？ | ✗ **不阻止**（让浏览器手势工作） | 浏览器后退会回到上一个 dashboard 路由（无路由变化时退出 dashboard） — 用户多用 ← 按钮，手势不强求 |

---

## 15. 验收 checklist（前端视觉）

实施完成后需要在 dev 环境逐项 check：

- [ ] sidebar 在任何时刻、任何 cron 状态下，**0** 个 `cron:` 卡片
- [ ] mainShell DOM 不存在 `#cron-timeline-panel` 节点（DevTools 全局搜）
- [ ] 列表 → 点行 → 抽屉打开，列表行有 `.is-active` 实色条 + 闪一次
- [ ] 抽屉打开时主对话区 mainShell 完全不动（不切空状态、不清 events）
- [ ] 抽屉切 jobId 时不滑出再滑入，title 闪 1 次
- [ ] running 状态：列表行徽章 + 抽屉头计时器 + 当前执行卡 三处同步
- [ ] WS 完成事件：当前执行卡 200ms 淡出，时间轴顶部 prepend + 1s 黄边
- [ ] 删除：modal → 倒计时 3s → 确认 → 行渐隐 + 抽屉关 + toast
- [ ] Esc 关抽屉，焦点回上次行
- [ ] 移动端 < 640：点行 push 抽屉，← 返回，inert 列表
- [ ] reduced-motion：动画全部退化为 opacity-only 或静态
- [ ] axe-core 扫描 0 critical/serious accessibility 问题
- [ ] Lighthouse a11y score ≥ 95

---

## 16. 不做的事（本期）

- ❌ cron 列表虚拟滚动（< 200 条够用，先观察）
- ❌ 时间轴折线图（成功率随时间）— UI 噪声大，价值在另一份 RFC「cron analytics」
- ❌ run 详情侧边导航（上一条 / 下一条）— v2 polish
- ❌ 抽屉拖宽手势（PC 用户大概率用最大宽度）
- ❌ 多任务批量操作（暂停 5 个 / 删 5 个）— `/cron list` IM 命令已能批操作
- ❌ Dark / Light 主题切换 — naozhi 全局只有 dark
- ❌ run 详情导出（JSON/CSV）— 后端已支持 curl 直拉，UI 不重复造
- ❌ 中断按钮（feature flag 默认 false）— 后端 API 未实现，强行做 disabled 占位是 dark pattern；后端实现后再开

---

## 17. Round 2 Review 决议表（24 项）

> 独立审稿人视角的 issue tracker。每项标 severity（C=critical / H=high / M=medium / L=low），状态：✓ 已合 / ⏸ 决议保留 / 🔄 进行中。

### Critical（5）

| # | severity | 节 | issue | 决议 |
|---|---|---|---|---|
| R-1 | C | §2 | 响应式断点没扣 sidebar 360px，1280 屏实际 main 区域只剩 ≈ 920px | ✓ 改用 main 可用宽度断点；ResizeObserver 监听 |
| R-2 | C | §3.1 | `box-shadow inset` 同时叠 accent 4px + error 3px，红色色条被覆盖丢失 | ✓ accent 4px + error 1px 内描边二级强调 |
| R-3 | C | §4.3 | 删除倒计时 §4.3 写 5s、§7.5 写 3s 矛盾 | ✓ 全文统一 3s |
| R-4 | C | §4.3 | 立即执行只覆盖"paused 时禁用"，缺并发 / 防抖 / loading 视觉 | ✓ 新增 §4.3.1 disable 矩阵 + 1s spinner + 2s "已派发" + 10s 防抖 |
| R-5 | C | §4.4 | UI 露 `phase: send` / `scheduled` 等英文枚举，操作员看不懂 | ✓ 全部中文化文案 |

### High（8）

| # | severity | 节 | issue | 决议 |
|---|---|---|---|---|
| R-6 | H | §4.1 | 抽屉头 44px 实测装不下两行（标题 18px + chip 22px ≈ 70px） | ✓ 改写真实高度 72px |
| R-7 | H | §4.1 | "复制 ⎘ 默认完整 JSON" 在工单粘贴会刷屏 | ✓ split button：默认 ID，▾ 菜单 3 选项 |
| R-8 | H | §4.4.1 | "中断按钮 disabled 占位 + tooltip 功能开发中" 是 dark pattern | ✓ 改为不渲染（feature flag） |
| R-9 | H | §4.6 | fresh-context hint 文案"共享一个 CLI 会话"对非工程用户难懂 | ✓ 改为"接续了上一次的对话上下文"等人话表达 |
| R-10 | H | §4.6 | fresh=true 模式没有任何视觉提示，用户困惑为啥 run 间不连续 | ✓ 补 fresh=true 的 hint |
| R-11 | H | §6.4 | 抽屉打开焦点落 ✕，键盘流第一个 Tab = 关掉抽屉 | ✓ 改为焦点落抽屉头任务名 + tabindex=-1 |
| R-12 | H | §7.5 | 删除文案直接给 PM/运营看 `claude --resume` 命令越界 | ✓ 重写文案 + 加"运行中删除"特殊分支 |
| R-13 | H | §8 | "5xx" 一行带过太粗，缺 401/403/429/网络断 | ✓ 拆成 17 行细分文案，每行带"用户下一步" |

### Medium（10）

| # | severity | 节 | issue | 决议 |
|---|---|---|---|---|
| R-14 | M | §5 | 状态机漏 "I. 远端删除" 与 "J. WS 重连" 两个状态 | ✓ 状态机扩到 10 项 |
| R-15 | M | §6.2 | 4 个字母快捷键稀释 dashboard 全局快捷键学习曲线，且在 textarea 里误触发 | ✓ 删除 R/P/E/Delete，仅保留 ↑↓/Enter/Esc/Home/End |
| R-16 | M | §6.4 | 删除后焦点跳列表第一行，离用户操作位置远 | ✓ 改为下一行→上一行→新建按钮 |
| R-17 | M | §9.2 | timeline 缓存"不清除"在大型部署内存无限增长 | ✓ LRU 上限 20 个 jobId |
| R-18 | M | §9.3 | 全局 + 抽屉两个 1Hz timer 重叠浪费 | ✓ 合并为 1 个 timer |
| R-19 | M | §9.x（新） | 高频 cron（每秒一次）触发渲染抖动，CPU 100% | ✓ 新增 §9.4 渲染抖动控制：200ms debounce + rAF |
| R-20 | M | §14 | 4 个新 open question 没回答：切面板回来抽屉是否保留 / 列表与抽屉操作同步 / prompt 原地编辑 / 移动端手势 | ✓ Q7-Q10 决议落地 |
| R-21 | M | §6.3 | 移动端"长按 ⋯"和"长按时间轴行"双重长按容易冲突 | ⏸ 决议保留：实测后再调；先按现状走 |
| R-22 | M | §11 | a11y 部分缺 SR live region 文案规范（哪些事件 polite / 哪些 assertive） | ✓ 在 §17.1 补充表格 |
| R-23 | M | §12 | mockup 缺 fresh hint / paused / 移动端 push 三种关键状态 | ⏸ 决议保留：等设计师产出 Figma 后补；本 RFC 不做 |

### Low（1）

| # | severity | 节 | issue | 决议 |
|---|---|---|---|---|
| R-24 | L | §13 | 6 个 commit 拆分粒度太细，review 反而累 | ⏸ 保留 6 commit 但允许实施时合并到 3-4 个 |

---

### 17.1 SR live region 文案规范（补充 §11）

> R-22 决议产物。所有 cron 面板触发的 SR 朗读统一在这里：

| 事件 | aria-live | 文案 |
|---|---|---|
| 抽屉打开 | polite | `已打开任务详情：<title>` |
| 抽屉关闭 | polite | `已关闭任务详情` |
| 抽屉切 jobId | polite | `切换到 <new-title>` |
| running 开始 | polite | `<title> 开始执行` |
| running 结束（成功） | polite | `<title> 执行成功，用时 <duration>` |
| running 结束（失败） | assertive | `<title> 执行失败：<error_class>` |
| 任务被并发删除 | assertive | `<title> 已被其他用户删除` |
| WS 断 ≥ 8s | assertive | `WebSocket 已断开，列表数据可能滞后` |
| WS 恢复 | polite | `连接已恢复` |
| 立即执行触发 | polite | `已派发执行请求` |
| 立即执行失败 | assertive | `触发失败：<reason>` |
| 删除确认 | polite | `请确认删除操作。倒计时 3 秒后可点击删除按钮。` |
| 删除完成 | polite | `任务已删除` |

assertive 仅用于"用户可能需要立即响应"的场景（错误 / 远端变更）；其余全 polite，避免打断用户朗读流。

---

### 17.2 仍未解决的设计风险（公开记录）

> 这些不是 review 漏的，是设计本身没法在桌面 UI 层面解决的硬约束，记录在这里供未来取舍：

1. **大量 cron job + 跨节点同步**：1000+ job 时列表渲染肉眼可见的卡顿，需要 virtualize（§16 列为不做）
2. **CronRun 详情大对象**：单条 run prompt + result 各 100KB+ 的极端任务（如代码生成），detail fetch 后渲染 `<pre>` 仍可能阻塞主线程；当前 max-height:240 + 内部 scroll 缓解 80%，剩余 20% 看后续
3. **多人并发编辑同一 cron job**：当前没有任何冲突检测，B 用户保存覆盖 A 用户修改时静默无感知 — 需要 If-Match etag 机制（不在本 RFC）
4. **任务执行的"中断"语义**：cron 调度器内部用 `runningJobs sync.Map[*atomic.Bool]` 去重，没有 cancel channel，UI 加中断按钮意义有限。后端架构改造前 UI 不渲染（R-8 决议）。
5. **cron 时区**：当前 UI 假设浏览器本地时区与 cron schedule 时区一致；跨国部署下两边时区可能不同，需要在 schedule chip 旁加时区标注（如 `每天 09:00 (Asia/Shanghai)`）— §4.1 mockup 已包含但未明确说明该字段来源

---

## 18. 与 Round 1 的 diff 总结

| 类别 | Round 1 | Round 2 | 净变化 |
|---|---|---|---|
| 节数 | 16 | 18 | +2 (§17 review 决议、§18 diff 总结) |
| 状态机 | 8 | 10 | +2 (远端删除、WS 重连) |
| 错误降级条目 | 8 | 17 | +9 |
| Open Q | 6 | 10 | +4 (Q7-Q10) |
| 字母快捷键 | 4 (R/P/E/Delete) | 0 | -4 |
| 删除倒计时 | 5s vs 3s 矛盾 | 统一 3s | 矛盾消解 |
| 抽屉头高度 | 44px（不可行）| 72px（实测 OK） | +28px |
| 抽屉内 timer | 2 个 | 1 个合流 | -1 |
| timeline 缓存 | 无上限 | LRU 20 | +上限 |
| 中文化文案 | 部分（露英文枚举）| 全中文 | — |

整体 LOC：~700 → ~1100，但**没有新增任何后端依赖**，全部在前端视觉/交互层收口。
