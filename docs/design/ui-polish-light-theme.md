# UI 打磨方案：简洁清爽 & 浅色主题优雅化

> 状态：设计定稿，待实施
> 起因：2026-07-31 对 dashboard 全视图截图走查（桌面/移动 × 浅色/深色，12+3 张，
> 脚本 `scripts/dashboard-screenshots.js` 同源方法）+ `dashboard.html` 样式通读。
> 结论：token 纪律、动效体系、a11y 底子好，不动；问题集中在
> **双主色打架、首页信息过载、浅色阴影过重、侧栏视觉噪音**四类。

## 目标 / 非目标

**目标**
1. 一屏之内只有一个饱和 accent 在争夺注意力（品牌 rust 与功能蓝各归其位）。
2. 首页空态回归"问点什么？"的极简心智，运维信息迁往系统/设置视图。
3. 浅色主题下浮层阴影从"黑晕"降为柔和层次。
4. 侧栏/聊天的常驻装饰降噪，信息密度提升。

**非目标**
- 不改布局架构（activity bar + sidebar + main 三栏保持）。
- 不动动效体系（`--nz-ease`、`--nz-dur-loop`、reduced-motion 全局降级）。
- 不动状态色语义（green/amber/red/purple）。
- 不处理"服务端 ui-settings 覆盖 localStorage 主题导致跨端闪烁"问题
  （已知，另开 issue 跟踪）。

---

## D1 色彩角色表（本方案的宪法）

| 角色 | 颜色 | 说明 |
|---|---|---|
| 品牌 / 主 CTA（logo、发送、新建） | rust 系（`--nz-rust*`） | 现状保持 |
| **选中/激活态**（当前会话卡、palette 高亮行、cron 选中行、activity bar 激活） | **rust 淡底 + rust 左条** | 现状是蓝橙两套，统一到 rust |
| **输入框聚焦边框** | **rust** | 现状蓝框紧挨 rust 发送按钮，是撞色主源 |
| 键盘 focus ring（`:focus-visible`） | 蓝（`--nz-focus` 不动） | 可访问性惯例，与"选中"语义区分 |
| 链接、信息文本、running 指示 | 蓝（`--nz-accent`/`--nz-blue`） | 现状保持 |
| 状态点/状态语义色 | green/amber/red/purple | 现状保持 |

新增 token（`:root`，light 块同值即可，rust ramp 已有两套主题值）：

```css
--nz-selected-bg:var(--nz-rust-subtle);
--nz-selected-bar:var(--nz-rust);
```

替换点（现状 → 改法）：
- `.cmd-palette-item.active`（dashboard.html ~1167）：`rgba(31,111,235,.2)` + inset accent 条 → 两个新 token。
- `.proj-pick li.selected`（~1153）：同上。
- `.cron-timeline-panel .ctr.is-selected`（~2001）：同上。
- `.rb-agent-row.active`（~1030）：inset accent → `--nz-selected-bar`。
- `.ab-nav.active::before`（~315）：accent 条 → `--nz-selected-bar`。
- `.session-card.active`（~551）：已是 rust，改引新 token（语义归一）。
- `.quick-ask-input:focus`（~816）：蓝边+蓝晕 → `border-color:var(--nz-rust);box-shadow:0 0 0 2px var(--nz-rust-subtle)`。聊天主输入框 focus 同步（同类规则 grep `border-color:var(--nz-blue)` 逐一判断是否为输入聚焦）。
- 用户气泡头像 `>_` 橙圈 + 浅蓝气泡底的并置：气泡底保持蓝色系（信息），头像圈改中性 `--nz-bg-2` + `--nz-text-mute` 描边，消除同框撞色。

不改：`:focus-visible` 全局规则（~280）、`button:focus-visible` 蓝晕（~281）、
running 相关的蓝（banner 文字、`.dot-new`、`--nz-status-new`）。

**对比度验证**：rust 条 on white ≈3.1:1（非文本装饰，可接受）；`--nz-rust-subtle`
底上正文仍用 `--nz-text`（≥7:1）；深色主题 rust on `#0d1117` ≈4.9:1，均过。

## D2 阴影 token 化 + 浅色主题降重

现状：39 处 `box-shadow` 全为暗主题手调的黑色高 alpha（`.4/.45/.5`），浅色下发黑。

新增 token：

```css
:root{ /* dark 默认，保持现值不重排版面 */
  --nz-shadow-1:0 1px 2px rgba(0,0,0,.2);        /* 输入框/小控件 */
  --nz-shadow-2:0 8px 24px rgba(0,0,0,.5);       /* popover / menu / stats-pop */
  --nz-shadow-3:0 16px 48px rgba(0,0,0,.5);      /* modal / palette */
  --nz-shadow-drawer:-2px 0 12px rgba(0,0,0,.4); /* 右侧抽屉 */
}
:root[data-theme="light"]{ /* auto+prefers-light 块同步 */
  --nz-shadow-1:0 1px 2px rgba(31,35,40,.06);
  --nz-shadow-2:0 4px 16px rgba(31,35,40,.12);
  --nz-shadow-3:0 12px 32px rgba(31,35,40,.14);
  --nz-shadow-drawer:-2px 0 12px rgba(31,35,40,.08);
}
```

替换清单（浮层类）：`.quick-ask-input`(~815→shadow-1)、`.fv-drawer`(~911→drawer)、
`.ctx-menu`(~1141→shadow-2)、`.cmd-palette`(~1160→shadow-3)、
`.history-popover`(~1208→shadow-2)、`.cj-menu`(~1739→shadow-2)、
`.cj-stats-pop`(~1837→shadow-2)、`.aside-drawer.nz-split-front`(~969→drawer)。

**不换**：lightbox（~855，暗场剧院效果两主题都要暗）、focus ring、inset 选中条、
`cjDotPulse`/`rb-dot-ripple` 等用 box-shadow 实现的脉冲动画。

可选加固：在 `static_style_ratchet_test.go` 增加 box-shadow 黑色字面量 ratchet
（基线=替换后剩余数），防新黑影字面量回流。

## D3 首页减负：recent-panel 只留"最近会话"

现状（`renderRecentSessionsPanel`，dashboard.js ~7955-8023）一个面板四层：
3 个带框 stat 卡 + 会话列表 + 5 行健康文本 + Backends `<details>`。
版本号/子进程数是运维信息，不该在"问点什么？"首屏。

改法：
1. `renderRecentSessionsPanel` 只渲染 title + rows（statsHtml/healthHtml/doctorHtml
   三段从首页移除）。
2. **迁移而非删除**：
   - 运行时健康（运行/就绪/uptime、claude 子进程、Backends 状态含 doctor 面板）
     → `renderSystemView()`（dashboard.js ~10569 附近的系统视图）顶部新增
     "服务概览" section。`computeHomeStats`/`buildHomeHealthLines`/
     `renderBackendsDoctorPanel` 函数原样复用，仅换调用方。
   - 版本信息（naozhi vX / claude-code vX）→ 设置页新增"关于"区（见 D8）。
   - stat 三卡（今日活跃/prompt 数/累计花费）→ 系统视图"服务概览"内，
     改为**无边框裸数字+标签**横排（去掉 `.recent-stat` 的 border/bg 嵌套）。
3. 外层 `.recent-panel`（dashboard.html ~1402）扁平化：去 `border` + `background`，
   保留 `max-width:480px` 和 title；rows hover 态不变。
4. 注意：`recent-row` 现用内联 `onclick`（dashboard.js ~7972），本次不新增内联
   handler（CSP ratchet ≤ 现值），迁移的 doctor 面板同理。

## D4 侧栏会话卡瘦身

现状：每卡 36px 高饱和 rust "爆炸" Clawd 图标（`.event.text` 同款素材，卡片侧
`.session-card` ~546 + 图标规则 ~743），信息量仅"backend=claude"，与 `● running`
状态点语义重复；卡高 ~80px。

改法：
1. 列表内 Clawd 图标从 36px 独立列 → **20px 单行内嵌**，进 meta 行；
   或（更激进）列表不放图标、仅聊天头像保留。**推荐前者**（保留品牌感）。
2. 卡片改两行结构：`title`（一行截断）/ `meta 行 = 状态点 + 状态词 + 时间 + chip`。
3. `.session-card` padding `12px 16px → 10px 14px`，`gap 12 → 10`。
4. 移动端（≤768px）时间戳与右缘齿轮重叠修复：操作图标桌面 hover 显示、
   移动端移入长按/滑动菜单（或最小改法：`.sc-time` 右侧加 `--nz-space-2` 间距）。

## D5 聊天视图常驻降噪

1. **头部 meta**（"claude-code v2.1.220 · claude-fable-5"）：默认只显示
   `backend · 模型别名`，完整版本进 `title` tooltip。
2. **输入提示行**（"Enter send · Shift+Enter newline · Esc interrupt"）：
   默认 `opacity:0`，`.input-area:focus-within` 时淡入（纯 CSS，键盘用户恰好
   在需要时看到）。
3. **running banner**（`.running-banner` ~999）：去满宽蓝底 →
   `background:transparent` + `border-bottom:1px solid var(--nz-border)`，
   spinner/dot 保留 accent 色，正文降为 `--nz-text-mute`，工具计数行保持。
   点击滚动到运行位置的功能不变。
4. Clawd 头像 36px → 28px（聊天侧适度保留，比侧栏宽容）。

## D6 新会话 palette

1. **backend 选择**（dashboard.js ~6776 原生 `<select>`）：backend ≤3 时渲染为
   分段 pill（segmented control，样式复用 `.settings-theme-opt`），>3 回退
   `<select>`。
2. **路径前缀淡化**：项目行的 mono 路径把公共 root（`projects.root`，本机
   `/Users/zhaokm/Workspace`）缩写为 `~/Workspace`，且前缀用
   `<span style="color:var(--nz-text-faint)">` 包裹，项目尾段保持正常色——
   让项目名成为唯一焦点。

## D7 资产视图小修

1. kind tabs（`.asset-kindtabs` ~386）右端截断：改 `flex-wrap:wrap`（4 个 tab
   最多两行），去掉横向滚动 + mask 渐隐。
2. 未选中资产时右栏顶部悬空的 "—" 标题：`#asset-main-head` 本有 `hidden`
   属性，排查 asset_browser.js 里 view 切换时提前 unhide 的路径，未选中保持
   `hidden`，空态文案独立居中。

## D8 设置页成型

`settings-body` 已有 720px 居中容器（~348），缺的是分组视觉与内容：
1. `.settings-sec` 卡片化：`border:1px solid var(--nz-border);border-radius:var(--nz-radius-md);padding:var(--nz-space-4);background:var(--nz-bg-1)`。
2. 新增"关于"区：naozhi 版本、claude-code 版本、backends 清单（承接 D3 迁出的
   版本信息）+ 指向系统视图的"服务概览"链接。

## D9 移动端 tab 收敛

底部 6 tab → 5：≤768px 隐藏 `#abnav-system`，设置页顶部加"系统任务"入口行；
system 的 alert 徽章降级为设置 tab 上的红点（复用 `.ab-badge`）。

## D10 历史徽章去数字

`.hdr-badge`（~1313）常驻显示历史总数（如"84"）无行动价值且扎眼。
改法：info 级（纯计数）不渲染徽章；仅 `is-alert` / `is-warn`（~2549）保留。
JS 侧找 `history-badge` 赋值点做条件渲染。

---

## PR 切分与顺序

| PR | 内容 | 层 | 风险 |
|---|---|---|---|
| A | D2 阴影 token + light 覆盖 | 纯 CSS | 极低 |
| B | D1 选中/聚焦 accent 统一 | 纯 CSS | 低（截图对比两主题） |
| C | D3 首页减负 + D8 设置承接 + D10 徽章 | JS+CSS+e2e | 中（DOM 结构变，e2e 断言需同步） |
| D | D4 侧栏卡瘦身 | JS+CSS | 中 |
| E | D5 聊天降噪 | CSS 为主 | 低 |
| F | D6 palette + D7 资产 + D9 移动端 | JS+CSS | 低（互相独立，可再拆） |

A、B 先行——都是 token 定义层，一次截图对比即可验收，且为后续 PR 提供统一语言。

## 每 PR 验证清单

1. `scripts/dashboard-screenshots.js` 前后截图（light + dark 各一轮；
   dark 需显式 `localStorage nz_theme=dark`，服务端 ui-settings 会覆盖 auto）。
2. `cd test/e2e && npx playwright test`（改 DOM 的 PR 先
   `grep -rn "recent-panel\|recent-stat\|doctor-" test/e2e/` 同步断言）。
3. `go test ./internal/server/ -run 'Ratchet|CSP|UXContract'`：
   font-size 字面量 ≤30、onclick 计数不涨、新 CSS 一律走 token
   （新阴影/选中色 token 本身符合 ratchet 精神）。
4. 对比度抽查：rust 选中底上的正文、浅色主题 rust focus 边框。

## 已确认不动的部分

动效 token（`--nz-ease`、1.5s 心跳）、CJK 字体栈、`:focus-visible` 全局规则、
reduced-motion 降级、状态色四件套、空态 `>_` 大 logo 与快速提问 CTA 结构。
