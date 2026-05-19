# UI Round 5 — Multi-Backend Polish

**Status**: Design
**Owner**: Kevin Zhao + Claude Opus 4.7
**Date**: 2026-05-19
**Related**: PRs #127-#135 multi-backend integration; rounds 1-4 e2e suites

## 0. Background

Multi-backend (claude + kiro) 已在 master 上线。4 轮 e2e (60+ cases) 跑通后，从用户实际使用反馈收集到 7 项 UI 改进点。本文档分析每项的可行性、改造范围、风险，作为 Round 5 实施依据。

非目标：

- 不引入新 backend
- 不重写 dashboard 框架
- 不破坏单 backend deployment

## 1. 改进项总表

| # | 现象 | 用户期望 | 范围 | 优先级 |
|---|---|---|---|---|
| **R5-1** | kiro icon 是六边形（占位 SVG） | 用 kiro 官方 logo（小鬼） | dashboard.js cliIcon | P0 |
| **R5-2** | session card / 头部都显示 backend chip 重复 | session list / 顶部都不要 chip | dashboard.js sessionCardHtml + renderHeader | P0 |
| **R5-3** | 看不到当前模型 | 顶部显示 model 或 "auto"，**所有 backend 都支持** | SessionView + dashboard 头部 | P1 |
| **R5-4** | credits 显示当次值 | 显示 session 累计 | applyMetadata 累加逻辑 | P1 |
| **R5-5** | "本地 · connected" 旁红点误导 | 去掉 | dashboard.html / sidebar status | P0 |
| **R5-6** | WS reply 不自动滚底 | 明确触发场景 | dashboard.js stickEventsBottom call sites | P1 |
| **R5-7** | 顶部 ctx-bar 信息无价值 | 去掉 | dashboard.js renderHeader | P0 |

预计代码改动：

- Go: ~80 行（process.go cost accumulator + SessionView.Model 字段 + 测试）
- JS: ~150 行（cliIcon 替换 + 移除头部 chip / ctx-bar / 红点 + 加 model 显示 + scroll trigger）
- HTML/CSS: ~30 行（移红点 css + model badge 样式）

## 2. 详细设计

### R5-1: kiro 官方 logo

**现状** (`dashboard.js:1264`):

```js
if (name === 'kiro') return '<svg ...><path d="M8 1L14..." fill="#f97316"...';
```

是个**六边形占位**，并非 kiro 官方 logo。用户原话："Kiro的图标，应该采用官方的kiro图标，就是那个类似小鬼的图标"。

**调研**：

kiro 官方 logo 是 *Kiro CLI*（https://kiro.dev）的 ghost-like emblem。目前 kiro 二进制未直接暴露 logo file。两种来源：

1. 从 kiro homepage 抓 SVG（`https://kiro.dev/favicon.svg` 或 logo-mark）
2. 用现有可商用替代图标如 `mdi:ghost-outline`（Material Design Icons）

**决策**：使用现有 inline SVG 重绘 ghost-style logo，避免外部资源 + 网络依赖。配色保留橙色（与 chip 一致）。

**改造**：

```js
function cliIcon(name) {
  if (name === 'kiro') {
    // Kiro ghost-style mark — inline SVG, orange to match chip color.
    // Tracked: docs/design/ui-round5-multibackend-polish.md#r5-1
    return '<svg class="sc-cli-icon" viewBox="0 0 24 24" fill="none">...</svg>';
  }
  return '<svg ...claude logomark...';
}
```

**风险**：

- 用户可能想要的是某个特定 SVG。如有授权问题，先用占位 ghost；用户提供官方 SVG 时一行替换。
- 风险等级：低（仅视觉变更）。

**测试**：playwright 截图比对 `kiro-ui-after-send.png`。

---

### R5-2: session list / 顶部移除重复的 backend chip

**现状**：

- `dashboard.js:1319` `cardBackendChip = backendChipHtml(s.backend)` → 渲染到 sidebar 卡片
- `dashboard.js:2083` `headerBackendChip = backendChipHtml(s.backend)` → 渲染到主面板顶部

**两个位置都显示橙色 `kiro` chip**。用户原话："紫色的这个backend显示可以不需要展示在sessionlist和对话框顶部，重复了"。

**信息论分析**：

- session list 卡片已有 **cli icon** (R5-1 之后是 kiro ghost) + backend 在 dot/label 旁 — 重复
- 主面板头部已有 **kiro v2.3.0** 文字 — 重复

**决策**：

- 侧边栏卡片：**移除** `cardBackendChip`。R5-1 的 kiro icon 已传达 backend 信息。
- 主面板头部：**移除** `headerBackendChip`。`kiro v2.3.0` 文字已传达 backend + 版本。
- backendChipHtml 函数本身保留，**doctor panel 还在用**（可视化 backend 列表场景）。

**改造**：

```diff
-  const cardBackendChip = backendChipHtml(s.backend);
   const metaHtml = '<span class="sc-dot ' + dotCls + '"></span>' +
     '<span>' + esc(displayState) + '</span>' +
     nodeBadge +
-    cardBackendChip +
     originBadge + ...

-  const headerBackendChip = backendChipHtml(s.backend);
   ... (header build)
-    headerBackendChip +
```

**回归保护**：

- `multibackend.test.js: every existing session card carries a backend chip` 这条**会失败**。改成断言 `cli icon` data-attribute 含 backend ID。
- `multibackend.round3: switching ... flips chip color` 改成断言 cost unit + cli icon 切换。

**风险**：

- 用户切到多 backend 模式时，**侧边栏视觉可能区分度下降**。Mitigation: kiro icon 必须**视觉强差异化**（橙色 ghost vs 紫色 claude logomark）。
- 风险等级：中。需要 R5-1 先到位。

---

### R5-3: 当前模型显示（all backends）

**现状**：

- claude session: header 显示 `claude-code 2.1.143` — 没有 model
- kiro session: header 显示 `kiro v2.3.0` — 没有 model（实际可能是 opus 4.7）

**用户原话**："显示当前模型或者是auto模式会很方便用户识别，需要把这个功能扩展为所有backend都支持的功能"

**数据流**：

config.yaml `cli.backends[].model` → router `backendModels[id]` → spawnSession `opts.Model` → BuildArgs `--model X`. 但 SessionView 没有 model 字段。

**改造**：

1. **`Process` 加 `Model() string`**：
   ```go
   func (p *Process) Model() string { return p.model }  // p.model set at Spawn time
   ```

2. **Wrapper.Spawn 设 proc.model = opts.Model**

3. **SessionSnapshot 加字段**：
   ```go
   Model string `json:"model,omitempty"`  // resolved spawn-time model; "" when default/auto
   ```

4. **Snapshot 填充**：
   ```go
   if proc != nil { snap.Model = proc.Model() }
   ```

5. **dashboard.js 头部**：
   ```js
   const modelLabel = s.model || (s.backend === 'kiro' ? 'auto' : '');
   // 注：claude 没设 model 时 cli.path 会用 ANTHROPIC_MODEL 环境变量；
   // 显示 "" 比 "auto" 准确（claude 不叫 auto）
   ```

**显示位置**：

```
┌─────────────────────────────────────────────────────┐
│ 标题                                                 │
│ kiro v2.3.0 · claude-opus-4.7    [9.2s] [0 credits]│
└─────────────────────────────────────────────────────┘
```

**risk**:

- claude 路径下 `cli.backends[].model` 可能空 — 显示什么？决策：空时显示空（不显 "auto"，因为 claude 没这个概念）。
- 风险等级：低。

---

### R5-4: credits 累计而非单 turn

**现状**：

`applyMetadata` 在 `internal/cli/process.go:711-717`：

```go
if len(m.MeteringUsage) > 0 {
    p.meteringMu.Lock()
    p.meteringUsage = append(p.meteringUsage[:0:0], m.MeteringUsage...)  // ← 替换
    p.meteringMu.Unlock()
}
```

每次 `_kiro.dev/metadata` notification 把 metering**整个覆盖**。session-level 累计丢失。

**改造**：

按 unit (`credit`) **累加 `MeteringEntry.Value`**，保留最新单位。

```go
if len(m.MeteringUsage) > 0 {
    p.meteringMu.Lock()
    // Accumulate per-unit. kiro 报告的是单 turn 增量；session-level
    // 总计需要在 process 侧累加，因为 kiro 自己不维护。
    if p.meteringUsage == nil {
        p.meteringUsage = make([]MeteringEntry, 0, len(m.MeteringUsage))
    }
    for _, in := range m.MeteringUsage {
        var existing *MeteringEntry
        for i := range p.meteringUsage {
            if p.meteringUsage[i].Unit == in.Unit {
                existing = &p.meteringUsage[i]
                break
            }
        }
        if existing != nil {
            existing.Value += in.Value
        } else {
            p.meteringUsage = append(p.meteringUsage, in)
        }
    }
    p.meteringMu.Unlock()
}
```

**Cost 显示 (`formatCostByUnit` JS)**：

cost 来源已经是 `s.total_cost`（claude 累加 USD）。kiro session `s.total_cost` 是空（kiro 走 metering），需要从 `s.metering_usage` 派生。

```go
// In Snapshot:
if snap.CostUnit == "credits" && len(snap.MeteringUsage) > 0 {
    for _, m := range snap.MeteringUsage {
        if m.Unit == "credit" || m.Unit == "credits" {
            snap.TotalCost = m.Value
            break
        }
    }
}
```

**测试**：3-turn kiro session，每 turn `meteringUsage=[{value:0.024,unit:"credit"}]`，期望 total_cost 累加到 0.072。

**风险**：

- 重启后 metering 状态 lost（仅在 Process 内存里）。Mitigation: SessionStore 已有 TotalCost 字段，也写 metering 累计进去。
- 风险等级：中（涉及持久化）。

---

### R5-5: 移除 sidebar 顶部红点

**现状** (`dashboard.html`):

```html
<span class="sidebar-status">...
  <span class="status-dot connected"></span>...
</span>
```

**用户截图**：本地 · connected 后边有 **红色小点**。这是 css `.sc-unread` 之类 class 误用，或全局 unread badge 渲染冲突。

**调研**：

```bash
grep -n "sidebar.*unread|status.*red" dashboard.html
```

实际是 sidebar 头部右上角的 cron unread badge `🕒 1` ——但旁边有个 css `.sc-unread { background: var(--nz-red) }` ，顶部 indicator 可能复用了同 class。

**改造**：

仔细看截图发现：连接状态文字 "本地 · connected" 后面紧跟一个小红点。源头是 `.sidebar-status` div 含 `.status-dot.connected`（绿色 6px）+ 旁边一个 wsm 重连指示符（红色 unread cron + sessions）。**视觉顺序**：

```
[绿点 connected] [▼] [红色未读总数]
```

红色未读看起来像 connected 的"伴生"标记。

**决策**：把红色未读 badge 放到 sidebar 底部 cron 闹钟图标旁，不靠 connected 。

**改造**: 找到 `<span class="sidebar-status">` 并把 unread chip 从该 div 内移到右上角图标栏。

**风险**：低（纯视觉移位）。

---

### R5-6: WS reply 自动滚底（明确触发场景）

**现状**：`stickEventsBottom()` 调用点：

```
dashboard.js:2502  // result event arrival
dashboard.js:2531  // sawUser branch
dashboard.js:3296  // optimistic user bubble insert
dashboard.js:8574  // history load
dashboard.js:8604  // sawUser
dashboard.js:8735  // isUser
```

WS push 流入是经过 `onSessionEvent` → renderEvent → 触发 `stickEventsBottom`。但 user 已主动滚动到上方时，行为应**不自动滚底**（避免打断阅读）。

**触发场景定义**（新合约）：

| 场景 | 行为 | 现状 |
|---|---|---|
| 用户刚 send → optimistic bubble | **总是滚底** | ✓ 已有 |
| WS push assistant chunk + 用户**在底部** | 滚底 | ⚠️ 不一致 |
| WS push assistant chunk + 用户**已滚到上方** | **不滚底**（不打断阅读） | ⚠️ 现在会滚 |
| Turn-end result event + 用户在底部 | 滚底 | ✓ 已有 |
| Turn-end result event + 用户已滚 | **不滚底**（保持位置） | ⚠️ 不一致 |
| 切 session（select） | **滚底**（fresh view） | ✓ 已有 |
| History 翻页加载 | **不滚底**（保持滚动位置） | ✓ 已有 |

**改造**：引入 `wasAtBottom()` 探测 + conditional stick：

```js
function maybeStickBottom() {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  const slack = 80;  // px tolerance
  const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < slack;
  if (atBottom) stickEventsBottom();
}
```

WS event 处理路径：

```diff
-  stickEventsBottom();  // unconditional
+  maybeStickBottom();   // only when user is reading the latest
```

但 send-time / select-time 仍走 `stickEventsBottom()` 无条件。

**风险**：

- 误探测：用户慢慢往下滚，可能漏掉新消息。Mitigation: 加 "新消息" 浮动 pill 当 atBottom=false 时。
- 风险等级：中。需要现场测试。

---

### R5-7: 移除顶部 ctx-bar 上下文用量

**现状** (`dashboard.js:2101-2113`):

```js
let ctxBarHtml = '';
if (ctxPct > 0) {
  ctxBarHtml = '<span class="ctx-bar..."><span class="ctx-bar-fill" style="width:..."></span></span>';
}
```

并被插入到 main-header 右侧组合（与 cost / turnTimer 同行）。

**用户原话**："上下文长度这个顶部的信息，我觉得也可以不需要"

**决策**：

- 服务端 `SessionView.ContextUsagePercent` **保留**（doctor / 监控用途；未来侧边栏紧凑显示用）。
- dashboard 头部 `ctxBarHtml` **删除**。

**改造**: 一行删除 ctx-bar 注入：

```diff
-  ctxBarHtml +
   turnTimerHtml +
```

**风险**：

- 部分 e2e 断言 `.ctx-bar` 存在 — 改成断言 `s.context_usage_percent` API 数据存在。
- 风险等级：低。

## 3. 实施顺序

按依赖关系：

```
P0 (UI 仅): R5-1 (kiro icon) → R5-2 (移 chip) — 一起，避免中间态
P0 (UI 仅): R5-5 (移红点)
P0 (UI 仅): R5-7 (移 ctx-bar)
P1 (Go+UI): R5-3 (model 显示)
P1 (Go+UI): R5-4 (cost 累计)
P1 (UI):    R5-6 (scroll 触发)
```

预计 4 个 commit，每个独立可 revert：

1. `chore(ui): 移除 sidebar 红点 + 顶部 ctx-bar` (R5-5, R5-7)
2. `feat(ui): kiro 官方 logo + 移除重复 backend chip` (R5-1, R5-2)
3. `feat(session): 全 backend model 字段 + 头部显示` (R5-3)
4. `feat(acp): metering credits session 累计` (R5-4)
5. `fix(ui): WS push 仅在底部时自动滚底` (R5-6)

## 4. 验证矩阵

每条改进都加一个 e2e 用例，写在 `test/e2e/multibackend.round5.test.js`：

| Case | 预期 |
|---|---|
| kiro session card 显示 ghost-like icon (非六边形) | `path[fill="#f97316"]` count >= 2 (body+detail) |
| sidebar `.session-card .sc-backend-chip` 不存在 | `count() == 0` |
| header 区无 `.sc-backend-chip` | `count() == 0` |
| header 显示 `claude-opus-4.7` 文字 (kiro session w/ model 配置) | textContent.includes('claude-opus-4.7') |
| 3-turn kiro session 累计 credits ≈ 3 倍单 turn | `total_cost >= 0.05` |
| sidebar status 旁无红点 | `.sidebar-status .sc-unread` 不存在 |
| header 不存在 `.ctx-bar` | `count() == 0` |
| 用户滚到上方后 WS push 不滚底 | scrollTop 不变 |
| 用户在底部时 WS push 滚底 | scrollTop 跟随 scrollHeight |

## 5. 验收

5 个 commit + e2e 套件通过，`go test -race ./... + npx playwright test multibackend.round{1..5}` 全绿。

部署后人眼检查：

- [ ] kiro session card icon 是 ghost
- [ ] 头部无紫色 / 橙色 chip
- [ ] 头部 `kiro v2.3.0 · claude-opus-4.7` 二段式
- [ ] credits 多 turn 累加
- [ ] 红点没了
- [ ] ctx-bar 没了
- [ ] 滚到上方查阅历史时 WS push 不打断阅读

## 6. Out of scope

- 不改前端框架（仍是 vanilla DOM）
- 不改 WS message schema（向后兼容）
- 不动单 backend 部署的 UX
