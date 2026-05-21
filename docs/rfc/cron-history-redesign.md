# Cron 面板信息架构重设计 — 历史为主 + Run 详情独立浮层

> 状态：Draft v1（提案中，待审批后切 PR）
> 日期：2026-05-21
> 范围：仅前端 dashboard。后端契约 / API / WS 不变。
> 关联 RFC（已落地）：
> - [`cron-panel-consolidation.md`](cron-panel-consolidation.md)（Round 145 抽屉化收编）
> - [`cron-panel-consolidation-ui.md`](cron-panel-consolidation-ui.md)（v2 6 段抽屉）
> - [`cron-run-history.md`](cron-run-history.md)（runs ring + 时间轴）
>
> 本 RFC 在以上方案运行 1 个月后基于真实使用反馈做信息架构修正。**不是推倒**，是把"抽屉里 6 段"重新拆成"两栏 + run-detail 浮层"，并补齐手机端缺失的设计。

---

## 0. TL;DR（一屏看完）

**当前 (v2)**：列表 → 抽屉里堆 6 段（头/概要/操作/当前/历史/run 内联展开）。
**问题**：
1. 一栏塞了"任务定义"+"执行历史"两套不对等心智
2. timeline row 内联展开 run 详情 → 长 result 把 timeline 撑爆
3. "三栏并排"在实测主区宽度下几乎进不了
4. 手机端单栏覆盖 + 没有 sheet 模式 → run 详情打开后整屏只能看一条 run

**v3 方案**：
- 桌面：**两栏**（列表 ≤ 260px ｜ 主区：任务定义摘要 + 历史时间轴），**run 详情用右滑抽屉**（≈ 480px，覆盖主区右半，不占永久空间）
- 移动：**3 级 push view**（sidebar → cron list → cron detail）+ **bottom sheet**（run 详情）
- run-detail 桌面与移动**共用同一组件**，仅 transform 方向不同
- 默认进 detail = **历史 view**（编辑是 secondary，✎ 进 inline，⤢ 进现有 modal）
- 删除 timeline inline expand
- 状态信号收敛到色条单点编码
- URL: `?cron=<id>&run=<run_id>`

预期 PR 拆分：PR-1（最小切片，run-detail sheet 共组件）→ PR-2（detail view 重构）→ PR-3（路由 / 列表升级）。

---

## 1. 背景：v2 上线后的真实问题

v2（cron-panel-consolidation-ui）2026-05-20 落地。运行约 1 个月后通过自用 + 多端实测发现 5 个根本性问题。

### 1.1 编辑器 vs 历史 ≠ 对等的两 tab

实测使用频次：
- 看历史 / 排错（高频，只读）：约 **80%**
- 立即执行 / 暂停 / 恢复（中频）：约 **15%**
- 改 prompt / cron 表达式（低频，有副作用）：约 **5%**

v2 抽屉把**任务定义概览（含 prompt 全文）**与**执行历史**等权重并列。每次进抽屉都先撞到概要 + 操作行，要看历史还得滚下去。这跟用户实际意图反着。

### 1.2 timeline 行内联展开 = 反模式

`cronTimelineRowHtml` (`dashboard.js:11533-11582`) 点击行后，prompt/result/error 直接 inject 在该 row 下面。

**实测问题**：
- result 几 KB → 整个 timeline 被一条 run 推得没法看
- 想对比"上次成功 vs 这次失败"必须反复 expand/collapse
- 嵌套 scroll：drawer-history 自己 scroll + ctr-block `<pre>` 又 scroll + 主列 scroll = 鼠标滚轮黑洞
- 长 result 没法整段复制（嵌在 row 里）
- `event.stopPropagation()` 兜住内层 click，键盘 tab 顺序断

### 1.3 "三栏并排"实际不可达

v2 §2 列了 4 档：≥1100 / 820 / 560 / <560。但 naozhi 默认 sidebar **320px**：
- 1440 屏 → main 1120 → 抽屉打开后剩 ~740（够 wide）
- 1280 屏 → main 960 → 进入 medium → 抽屉只剩 ~600
- 1080p 笔记本 + 拉宽 sidebar 到 360 → main 720 → 进入 narrow → 抽屉 ~400

**真实场景大部分卡在 medium / narrow**。v2 wide 模式的"380 列表 + 720 抽屉"基本是个理论值。

### 1.4 抽屉 4 档 ResizeObserver 切换体验断

`setupCronLayoutObserver` (`dashboard.js:12274-12317`) 在 wide/medium/narrow/single 之间切换会让抽屉**瞬间消失/重排**，没视觉过渡。拖窗口大小时尤其明显。

### 1.5 手机端只是桌面的退化

v2 §2.2 手机方案 = "抽屉 push 覆盖列表"。**没有 run 详情的浮层设计**，timeline 行点击后还是 inline expand，在手机上 result 一展开占满整屏，找不到列表上下文。

---

## 2. 设计原则（v3）

继承 v2 §0 的 6 条原则，新增 4 条：

7. **历史是主舞台**：detail view 默认呈现 = 历史时间轴。任务定义只折叠成 3 行摘要，要改才展开。
8. **Run 详情永远是浮层**：它是 detail view 的二级展开，不该占用主屏永久空间。桌面右滑、移动底部滑出。
9. **桌面/移动共组件**：run-detail sheet 一套渲染逻辑、一套 state，两端只是 transform 方向不同。
10. **不靠"三栏并排"做主架构**：双栏是稳态目标，三栏只是宽屏奖励（且不强求）。

---

## 3. 信息架构总览（v3）

```
定时任务面板
├─ 列表 (左, ≤ 260px)
│  ├─ 顶栏（标题 + 摘要 + 新建）
│  ├─ 筛选条（搜索 / chips / 排序）
│  └─ 任务行 × N（沿用 cj-row 收紧）
│
├─ 主区 = "Detail View"（右, flex:1）
│  ├─ Header（72px sticky）：任务名 + schedule + workdir + 操作行（▷ ⏸ ✎ ⋯）
│  ├─ 任务定义摘要（折叠态，3 行 prompt + meta；点 ✎ 展开 inline 编辑）
│  ├─ 当前执行（仅 running 时）
│  └─ 执行历史（时间轴 + 加载更多，主滚动区）
│
└─ Run-Detail Sheet（浮层，仅选中 run 时存在）
   ├─ 桌面：右滑抽屉 ≈ 480px，覆盖主区右半
   ├─ 移动：bottom sheet ≈ 75vh
   └─ 内容：run header + prompt / result / error / session-jump
```

**关键**：detail view 与 run-detail sheet 是 **2 层关系**，不是 v2 的 6 段平铺。

---

## 4. 桌面布局规范

### 4.1 响应式断点（修订）

按**主区宽度** + **是否有 sheet 打开**两个维度交叉：

| 主区宽度 | sheet 关闭 | sheet 打开 |
|---|---|---|
| ≥ 1100 | 列表 260 + detail (≥ 760) | 列表 260 + detail (≥ 380) + sheet 480（右滑覆盖 detail 右半） |
| 820–1099 | 列表 240 + detail (≥ 480) | 列表 240 + detail 收窄到 ≥ 280 + sheet 480 |
| 560–819 | 列表 220 + detail (≥ 280) | sheet **覆盖**整个 detail（detail 设 inert） |
| < 560 | 单栏：列表 OR detail（push 模式） | sheet 全屏覆盖 |

**简化点**：
- v2 的 4 档无 sheet 维度，导致开 sheet 后 detail 被挤压无规律。v3 显式列出"sheet 关闭/打开"两态。
- ResizeObserver 监听**主区**而非 viewport（同 v2）。
- 切档时 sheet 用 `transition: transform .25s ease`，列表/detail 用 `transition: flex-basis .2s ease` 平滑（v2 直接 flex 跳变）。

### 4.2 双栏 + 浮层 wireframe

```
┌──── main 容器 ─────────────────────────────────────────────────────┐
│ ┌─ 列表 240-260 ─┐ ┌─ Detail View ───────────────────────────┐    │
│ │                 │ │ ┌─ Header sticky ─────────────────────┐ │    │
│ │ 顶栏 / 摘要     │ │ │  cron #2 标题       [▷立即][⏸][✎][⋯]│ │    │
│ │ 搜索 / chips    │ │ │  每天 09:00 · ~/repo                │ │    │
│ │                 │ │ └─────────────────────────────────────┘ │    │
│ │ ● cron #1       │ │                                         │    │
│ │ ● cron #2 ◄─sel │ │ ┌─ 任务定义摘要 (折叠态) ────── ✎ ── ⌃┐│    │
│ │ ● cron #3       │ │ │ prompt: "检查飞书机器人..." [展开 ▾] ││    │
│ │                 │ │ │ 通知 🔔  Fresh ✓  Backend Claude    ││    │
│ │ [+ 新建]        │ │ └─────────────────────────────────────┘│    │
│ │                 │ │                                         │    │
│ │ swipe 行:       │ │ ┌─ 当前执行 (仅 running 时) ──────────┐│    │
│ │ →[运行][暂停]   │ │ │  ● 0:14 · phase send                ││    │
│ │ ←[删除]         │ │ └─────────────────────────────────────┘│    │
│ │                 │ │                                         │    │
│ │                 │ │ ┌─ 执行历史 (主滚动区) ───────────────┐│    │
│ │                 │ │ │ (32 次, 92% 成功)                   ││    │
│ │                 │ │ │ ✓ 14:30  12s            [→]         ││    │
│ │                 │ │ │ ✓ 13:30   9s            [→]         ││    │
│ │                 │ │ │ ✗ 12:30  31s ⚠         [→] ◄click ─┼┼──┐ │
│ │                 │ │ │ ✓ 11:30  11s            [→]         ││  │ │
│ │                 │ │ │ ...                                 ││  │ │
│ │                 │ │ │ [加载更多]                          ││  │ │
│ │                 │ │ └─────────────────────────────────────┘│  │ │
│ └─────────────────┘ └─────────────────────────────────────────┘  │ │
└──────────────────────────────────────────────────────────────────┘ │
                                                                       │
              ┌─ Run-Detail Sheet (右滑 ≈ 480) ────┐  ◄────────────────┘
              │ ✗ 失败 12:30  31s    [⎘][→sess][✕]│
              │ ─────────────────────────────────  │
              │ prompt                             │
              │ ─────────                          │
              │ ...                                │
              │ ─────────────────────────────────  │
              │ result                       [⎘]   │
              │ ─────────                          │
              │ ...                                │
              │ ─────────────────────────────────  │
              │ error class · 网络错误             │
              └────────────────────────────────────┘
```

### 4.3 操作位置修正

v2 把"立即执行 / 暂停 / 编辑 / 删除"放在抽屉内的 §4.3 操作行（中部）。v3 改：

- **常用 3 个**（▷ 立即执行 · ⏸/▶ 暂停 · ✎ 编辑）→ Detail Header **右上 sticky**
- **次要操作**（🗑 删除 / ⎘ 复制 ID / ⎘ 复制 JSON / 复制最近 run_id）→ Header 右上 ⋯ 菜单（右上 + 二次确认仍是 3s 倒计时，沿用 v2）

理由：滚到 history 50 条后仍能直接触发"立即执行"——v2 滚下去就够不到了。

---

## 5. 移动端设计（核心补强）

### 5.1 view 栈与 sheet

```
L1 (root): sidebar 列表 view                           [body 类: 默认]
   │
   ▼ 点闹钟图标 (openCronPanel + mobileEnterChat)
L2 (push): cron 面板 = cron 列表                        [body 类: mobile-chat-view]
   │
   ▼ 点列表行 (openCronDetail)
L3 (push): cron detail view (历史 + 摘要 + 操作)        [URL: ?cron=<id>]
   │
   ▼ 点 timeline 行 (openRunDetailSheet)
overlay: run-detail bottom sheet (75vh)                 [非 view, 不入 history 栈]
```

**入栈策略**：
- L1 → L2 → L3 三级 push（已有 `mobileEnterChat()` + popstate）
- run-detail sheet **不**入 history 栈（避免分享链接打开看到不同状态；系统返回先关 sheet 再退栈）
- sheet 打开时背景列表保持可滚（与 RFC §B1 决策一致：移动端进 detail = push view，
  sheet 是 detail 内浮层；列表已被 push 出可见区，无需锁 body scroll）。
- 焦点：进 sheet 后焦点移到 `crs-title`（programmatic, tabindex="-1"），
  关闭时焦点回 timeline 行；ESC / 下滑 / ✕ / 点 backdrop 五路关

### 5.2 三级 view 详图

```
L2: cron list                  L3: cron detail                  Sheet: run detail
┌──────────────────┐          ┌──────────────────┐            ┌──────────────────┐
│ ← 定时任务   +   │          │ ← cron #2  ▷⏸✎⋯ │            │ ━━━━━ (drag)    │
│ ━━━━━━━━━━━━━━ │          │ 每天 09:00       │            │ ✗ 失败 12:30    │
│ 搜索 [____]      │          │ ~/repo · 通知🔔   │            │ 31s · cron      │
│ [全部][需关注]   │          │ ─────────────── │            │ [⎘][→sess][✕]  │
│                  │          │ 摘要 (3 行) ✎ ⌃ │            │ ─────────────── │
│ ● cron #1        │ ──push──►│ ─────────────── │ ──open────►│ prompt           │
│ ● cron #2        │          │ 当前执行 (cond)  │ sheet      │ ───────         │
│ ● cron #3        │          │ ─────────────── │            │ "..."            │
│                  │          │ 执行历史         │            │ ─────────────── │
│ [+ 新建]         │          │ ✓ 14:30 12s ›   │            │ result    [⎘]   │
│ swipe🗑/▷⏸       │          │ ✓ 13:30  9s ›   │            │ ───────         │
│                  │          │ ✗ 12:30 31s ›◄  │ tap        │ "..."            │
└──────────────────┘          │ ✓ 11:30 11s ›   │            │ ─────────────── │
                              │ [加载更多]      │            │ error: 网络错误  │
                              └──────────────────┘            └──────────────────┘
                                                                  ↓ 下滑 / ✕ / 系统返回
```

### 5.3 移动端 timeline 行设计

v2 行高 ~36px，触屏不易点。v3 改 **≥ 56pt**：

```
左 4px 色条（错误/missed/run 不同色）
│
▼
[██●] 失败                          31s ›
[██]  5月17日 14:30 · cron
[██]  网络错误
```

- 状态点 `●` 与色条贴近（不再独立列）
- 右侧 `›` chevron 表"可点开浮层"，与 iOS 列表心智一致
- tap 整行 = open sheet；行 hover/long-press 不显示菜单（移动端无菜单语境）

### 5.4 移动端列表 swipe action

复用现有 `session-card.swiping` 体系（已用于会话删除）：

- **右滑**到 ≥ 60% → 露出 `[▷ 运行]` 或 `[⏸ 暂停]`（暂停态显恢复）
- **左滑**到 ≥ 60% → 露出 `[🗑 删除]`（红，触发后弹 3s 倒计时确认）
- 释放 < 60% 弹回
- `touch-action: pan-y` 防止纵向滚动被吃

不再加"右侧永远可见 chevron"——避免与 swipe 冲突。

### 5.5 软键盘与 sheet 协同

iOS Safari 软键盘弹出时改 visualViewport（已有 `--vv-height` 变量）：
- sheet 高度 = `min(75vh, var(--vv-height) - 80px)`
- sheet 内 `<pre>` 块用 `max-height: 40vh` + 内部 scroll，不挡 header
- 用户在 sheet 内不会聚焦输入框（sheet 是只读），所以基本不会触发软键盘

---

## 6. Run-Detail Sheet 共组件规范

桌面 / 移动两端**同一渲染函数**，差别只在容器 transform。

### 6.1 组件 API

```js
function openRunDetailSheet(jobId, runId) { ... }
function closeRunDetailSheet() { ... }
function navigateRunSheet(direction)      // direction: 'prev' (UI ↑, 时间更新) | 'next' (UI ↓, 时间更旧)
function renderRunDetailSheet()           // 渲染 sheet 内容（header + body）
```

state：
```js
const cronRunSheetState = {
  jobId: null,
  runId: null,
  open: false,
};
```

**detail / loading / error 字段刻意省略** — sheet 命中的 run 详情直接复用
`cronTimelineState[jobId].details[runId]`（v2 已有缓存）。两份 cache 会让
WS 刷新只刷一处导致漂移；单源真相更稳。fetchDetail 完成后既刷
timeline panel（更新 inline 错误指示）也刷 sheet（写入 body），形成
"一次 fetch + 两路 view" 模式。

### 6.2 容器 CSS

```css
.cron-run-sheet {
  position: fixed;
  background: var(--nz-bg-1);
  border: 1px solid var(--nz-border);
  box-shadow: 0 -8px 24px rgba(0,0,0,.5);
  z-index: 200;
  display: flex;
  flex-direction: column;
  transition: transform .25s ease;
}

/* 桌面：右滑抽屉 */
@media (min-width: 769px) {
  .cron-run-sheet {
    top: 0; right: 0; bottom: 0;
    width: min(480px, 50vw);
    transform: translateX(100%);
    border-right: none;
    border-radius: 0;
  }
  .cron-run-sheet.is-open { transform: translateX(0); }
}

/* 移动：bottom sheet */
@media (max-width: 768px) {
  .cron-run-sheet {
    left: 0; right: 0; bottom: 0;
    max-height: 75vh;
    transform: translateY(100%);
    border-radius: 16px 16px 0 0;
    border-bottom: none;
  }
  .cron-run-sheet.is-open { transform: translateY(0); }
  .cron-run-sheet::before {
    /* drag handle */
    content: ''; display: block;
    width: 36px; height: 4px;
    background: var(--nz-text-faint);
    border-radius: 2px;
    margin: 8px auto;
  }
}
```

### 6.3 内容渲染

复用 v2 `cronTimelineDetailHtml(jobId, runId, summary, detail)` (`dashboard.js:11599-11643`)：
- prompt / result / error_msg / fresh hint / session-jump chip 全部保留
- 加 sticky header（带状态点 + 时间 + 复制 + 关闭按钮）
- result `<pre>` 加复制按钮（v2 没有）

### 6.4 交互

| 操作 | 行为 |
|---|---|
| 点 timeline 行 | open sheet + 该行加 `.is-selected`（与 `.cj-row.is-active` 区分：drawer 选中态用 `is-active`，timeline 行选中态用 `is-selected`） |
| ↑ ↓（桌面）/ 左右滑（移动） | navigateRunSheet 切上一条/下一条，列表自动滚动到对应行 |
| ESC | 关 sheet（focus 回 timeline 行） |
| ✕ | 同 ESC |
| 移动端下滑超过 80px | 关 sheet（沿用 swipe handler） |
| 点遮罩（移动） | 关 sheet |
| 桌面点 detail view 空白处 | 不关（操作员可能在边对比边看 timeline） |
| 系统返回（移动） | 关 sheet 优先于退 view |

### 6.5 复用与组件边界

- **不**做成 modal（现有 `.modal-overlay` 是会议性的，sheet 是审计性的）
- **不**做成 popover（popover 太小、关掉太轻易）
- 复用现有 `.history-sheet` 的 `sheet-in` 动画 (`dashboard.html:986`)
- 焦点陷阱：进 sheet 后第一个 focusable = ✕ 按钮；Tab 在内部循环；ESC 退出还焦点

---

## 7. 状态信号收敛（确认上一轮 review）

| 信号 | v2 现状 | v3 |
|---|---|---|
| 状态色条（左 3-4px） | 错误红 / missed 琥珀 / 运行蓝 | **保留**（唯一异常视觉编码） |
| 圆点 `cj-dot` | 绿/灰二态 + 运行脉动 | **删**（色条已表达，省 1 处冗余） |
| stats 徽章 `cj-stats` | 三档色（绿/中性/红） | 改**永远中性**，只显示 N 或百分比；**异常时**用左侧色条表达（与卡片状态一致） |
| sparkline 5-dot `cj-stats-pop` | hover 才出 | **always-on** 6px 高 mini sparkline，触屏可见 |
| 错误条 `cj-error` | 行内 `display:flex` 总展开 | 默认 `-webkit-line-clamp:1`，悬浮加 `[展开]` 微按钮 |

少 1 处冗余编码，触屏多 1 项可读信息，列表噪音降一档。

---

## 8. 任务定义编辑：双入口策略

v2 编辑只能走 modal。v3 提供：

### 8.1 Inline 编辑（detail view 内）

- 摘要区右上 ✎ 按钮 → 切到 inline 模式
- prompt textarea: `min-height: 120px; max-height: 40vh`
- schedule input: 复用 frequency picker
- 通知 / fresh / workdir 同步 inline 改
- 保存按钮 sticky 顶部黄色条："有未保存改动 [保存] [丢弃]"
- ESC = 触发 dirty check（dirty 时弹确认）

### 8.2 全屏 modal（沿用 v2 cron-modal）

- inline 编辑模式下右上 `⤢ 全屏` 按钮 → 弹现有 modal，prompt 内容继承
- 适合 prompt > 500 字 / 多行 markdown 场景
- 移动端 ✎ 直接进 modal（不走 inline，因屏幕窄）

### 8.3 Dirty buffer 与 view 切换

- Inline 编辑期间切到其他 cron / 关 detail / 跨设备 WS 推送：
  - dirty 自动写 `localStorage.cronDraft.<jobId>`
  - 回来时（同 cron）自动恢复 + 显示"恢复了草稿 [保留] [丢弃]"
- WS 推送同一 cron 更新时：
  - 设 `_editLockJobId`，**不刷** detail
  - 顶部 banner："服务端有更新（来自 ...）。[查看变更] [覆盖] [丢弃我的]"

---

## 9. URL 路由与 deep link

```
/                                           # 主页（无选择）
/?cron=<id>                                 # 选中 cron，detail = 历史 view
/?cron=<id>&edit=1                          # 选中 cron + inline 编辑模式
/?cron=<id>&run=<run_id>                    # 选中 cron + run sheet 打开
```

- `run` 不入 history.pushState（避免分享带 sheet 状态）
- 刷新后 cronJobs 异步加载，先渲染 placeholder（沿用 v2 `_cronDrawerFetchedFor`），加载完匹配 URL 选中
- `edit` 跨刷新不保留（草稿走 localStorage 而非 URL）
- 进 detail 但 cron 不存在 → 占位文案 "任务已不在列表中"（沿用 v2 行为）

---

## 10. 状态机（detail view + sheet）

```
┌────────────────────┐
│  detail = closed   │
└──────┬─────────────┘
       │ 点列表行 / URL ?cron=<id>
       ▼
┌────────────────────┐
│ detail = open      │ ◄──────┐
│   tab = history    │        │ 关 sheet
│   sheet = closed   │ ────┐  │
└──────┬─────────────┘     │  │
       │ 点 ✎              │  │
       ▼                    │  │
┌────────────────────┐      │  │
│ detail = open      │      │  │
│   tab = edit       │      │  │
│   sheet = closed   │      │  │
└──────┬─────────────┘      │  │
       │ 点 timeline row    │  │
       ▼                    ▼  │
┌────────────────────┐         │
│ detail = open      │         │
│   sheet = open     │ ────────┘
│   sheet.run = X    │
└──────┬─────────────┘
       │ ↑↓ / 左右滑 (移动)
       ▼
┌────────────────────┐
│ detail = open      │
│   sheet = open     │
│   sheet.run = Y    │
└────────────────────┘
```

### 边缘 case

| 情况 | 行为 |
|---|---|
| sheet 打开时切到另一个 cron | sheet 关闭，detail 切换；不保留 sheet 状态 |
| sheet 打开时该 run 被 GC | sheet 内显示"记录不存在或已被清理"（沿用 v2 错误态） |
| edit 模式 dirty + 切 cron | 弹确认（保留草稿/丢弃） |
| WS 推送 cron_run_finished 当前选中 run | sheet 内 detail 自动刷新（detail.state 从 running → succeeded/failed） |
| 用户在 inline 编辑 + WS 推送 cron 配置变更 | 顶部 banner，不强刷 |

---

## 11. e2e / 兼容性 / 迁移

### 11.1 锚点保留

继续保留以下 class 作为 source-level 锚点（参考现有 `cronJobCardHtml.__unused`）：
- `.cron-card`（e2e/dashboard.test.js）
- `.cj-row` / `.cj-actions` / `.cc-actions`
- `#cron-list-items` / `#cron-detail-pane` / `#cron-timeline-panel`

### 11.2 删除的代码路径

- `cronTimelineRowHtml` 的 `isExpanded` 分支 + `cronTimelineDetailHtml` 调用（移到 sheet）
- `cronTimelineToggleRow` 的展开切换（改为 select-only + open sheet）
- `cronTimelineState.expanded` / `cronTimelineState.details` 状态字段（迁到 `cronRunSheetState`）

### 11.3 测试更新

- `TestDashboardJS_R110P2_CronRunNowButton` 不变（cc-actions 锚点保留）
- 新增 `TestDashboardJS_RNEW_CronRunSheetOpen`：模拟点击 timeline 行 → assert sheet `.is-open`
- 新增 `TestDashboardJS_RNEW_CronRunSheetNavigate`：↑↓ 切换、ESC 关闭
- 新增 `TestDashboardCSS_RNEW_CronSheetMobileBottom`：< 769px 时 sheet 从底部
- 新增 e2e: `cron-detail-history.spec.js`（点击行→sheet→↑↓→关）

---

## 12. 实施 PR 拆分

### PR-1（最小切片，1.5 天，独立 reviewable）

**目标**：消除 timeline inline expand → run 详情独立 sheet（桌面右抽屉/移动 bottom sheet 共组件）

具体改动：
1. 抽 `cronRunDetailSheet` 组件（HTML 模板 + open/close/navigate API）
2. 删 `cronTimelineToggleRow` 的 inline 展开分支，改为 `openRunDetailSheet(jobId, runId)`
3. timeline 行高调 ≥ 56pt（移动），状态点与色条整合
4. ↑↓ 切换选中 run，sheet 联动；ESC / 下滑关闭
5. URL: `?cron=&run=`（先不加 `edit`）
6. 加 stats 徽章 always-on sparkline

不动：编辑器、tabs、列表布局、操作位置——后续 PR 再上。

### PR-2（detail view 重构，2 天）

1. 操作行从抽屉中部移到 Header 右上 sticky bar
2. 摘要区改"折叠态默认 + ✎ inline 编辑"
3. cron-modal 沿用为"⤢ 全屏编辑"二级入口
4. dirty buffer + localStorage 草稿
5. WS edit-lock banner

### PR-3（路由 + 列表升级，1 天）

1. URL 加 `edit=1`
2. 列表 swipe action（移动）
3. 列表收紧到 240-260px（桌面）
4. 状态信号收敛（删 cj-dot / stats 徽章中性化 / cj-error clamp）

---

## 13. 与既有 RFC 的关系

- **cron-panel-consolidation.md / -ui.md (v2)**：本 RFC 是 v2 的演进，不取代 §1-3 的整体收编决策（cron 不再是 sidebar 会话）。**修正** v2 §4（抽屉 6 段平铺改为 detail view + sheet 两层）和 §2（响应式断点）。
- **cron-run-history.md**：后端契约不变（CronRun ring / WS started/ended）。前端只改时间轴行 + 详情入口。
- **cron-v2-polish.md**：sort / search / missed banner / next-run 摘要等 polish 项**全部保留**，本 RFC 不动这些。

---

## 14. 决议（2026-05-21 ✅）

| Q | 议题 | 决议 |
|---|---|---|
| Q1 | PC sheet 定位 | **`position: absolute` 仅覆盖 detail-pane**。列表常驻可见，便于"边看 sheet 边切 cron"。实现走 detail-pane 内层相对定位。 |
| Q2 | 移动端 push 深度 | **保持 3 级 view + sheet**（sheet 不入 history 栈）。与 IM 会话 push 深度一致；返回键先关 sheet 再退 view。 |
| Q3 | ↑↓ 切 run 时列表自动滚动 | **滚动**，`behavior: 'auto'`（不是 smooth）。快速连按不会"追动画"，同时保证选中行总在视野内。 |
| Q4 | stats 徽章中性化后的 attention 转移 | **归入 attention**（左色条琥珀）。filterCronJobs 的 `attention` 分支扩展到 `paused \|\| last_error \|\| missed \|\| stats.fail_rate ≥ 20%`，与现有 missed 同色。 |
| Q5 | 桌面 sheet 宽度记忆 | **PR-1 不做**。固定 480px。如 ship 后有反馈再补 PR-4 加 resize handle + localStorage。 |

---

## 15. Review 入口

- 视觉细节请直接回复 wireframe 段落 + 行号
- 状态机 / URL / WS 同步异议看 §10 / §9 / §8.3
- 移动端（最大变化）请重点 review §5 + §6
- e2e 影响请看 §11
- 按 PR-1 / PR-2 / PR-3 切，PR-1 risk 最低、用户感知最强烈，建议先批
