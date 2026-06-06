# UI-DASHBOARD-CRON-EXTRACTION — 将 cron 视图从 dashboard.js 抽出为独立模块

| 字段 | 值 |
| :--- | :--- |
| 状态 | Draft v2（三方评审已纳入，方向 APPROVE / 实施待 §9 拍板） |
| 作者 | naozhi team |
| 创建日期 | 2026-06-06 |
| 修订日期 | 2026-06-06 |
| 关联代码 | `internal/server/static/dashboard.js`（16564 行单体，cron 占 ~3900 行）<br/>`internal/server/static/dashboard.html`（`<script>` 加载顺序 + cron 容器/CSS）<br/>`internal/server/static/asset_browser.js`（已拆出模块的范本，`window.nzAssetView`）<br/>`internal/server/static/agent_view.js`（同上） |
| 关联测试 | `internal/server/static_cron_*.go`（5 个静态契约测试，**grep dashboard.js 文本**）<br/>`internal/server/cronview_contract_test.go` |
| 关联 RFC | `docs/rfc/cc-asset-browser.md`（已拆模块的挂载范式）<br/>`docs/rfc/cron-dashboard.md` / `docs/design/cron-dashboard-redesign.md`（cron 视图本体） |
| 关联 issue | TBD |

---

## 0. 修订历史

### v2 (2026-06-06) — 三方评审后大改实施计划

经事实核查 / 架构 / 风险三方并行评审，**方向获 APPROVE，但 v1 的"单 PR 纯搬家"
实施计划被否（NEEDS-WORK）**，发现 4 个 BLOCKER（详见新增 §2.6）：

- **B1（致命）**：cron 区有 41 个 inline `onclick`/`onchange` 引用 19+ 个顶层全局
  函数，包进 IIFE 后这些函数变文件局部，inline handler 点击即 `ReferenceError`，
  整个 cron UI 按钮全哑。必须**先迁移到 `data-action` 事件委托再抽模块**。
- **B2**：cron 实时订阅逻辑（`wsm.cronLive` + `subscribeCronLive`/`onCronLive*`，
  9756–10133 行）**长在 WebSocket 内核对象里**，不在所谓"11366–15300 连续区间"，
  且核心反向调 `setCronLiveStatus()`——是双向耦合，单向模型套不上。
- **B3**：§3.3 依赖白名单从"11 个"实测应为"25+ 个"，且漏了安全敏感的 `escJs`
  （第三种转义，23 次调用，inline onclick 专用）和有状态的 `wsm`（28 次）。
- **B4**：Go 端 `//go:embed` + mux route + auth + 成对鉴权测试三处必须同步改，
  v1 §5 全漏，漏改直接 404 / 未鉴权泄露。

同时修正：§6 测试清单（误标 `cronview_contract_test.go`，漏 `static_ux_contract`/
`static_event_dom_cap`/`static_toplevel_views` 三个会 CI 红的测试）；命名空间收敛为
单一 `window.nz.{util,render,core,views}`；放弃"nz_render 前移到 dashboard.js 之前"
（会打破 `static_agent_view_test.go` 对 `eventHtml` 由 dashboard.js 导出的契约）。

被证实成立的部分：分层方向单向无环 ✅、cron 主体高内聚（§2.5 频次数据逐条核实，
事实可信度 ~85%）✅、对话渲染确被 chat/cron/agent 三处复用 ✅、defer 执行顺序有保证 ✅。

### v1 (2026-06-06) — 首版

确立"渐进式按视图剥离"的总路线。经评审补充：**第零步先抽公共基础层**
（工具函数 + 对话渲染），再抽 cron 视图。理由见 §2.5——多个视图都在复用
同一套对话/消息渲染逻辑，把它沉淀为共享层比单为 cron 暴露 `window.nzCore`
更有价值，也是 cron 能干净抽出的前提。settings 视图剥离作为后续步骤（§8）。

---

## 1. 背景与问题

`internal/server/static/dashboard.js` 已膨胀到 **16564 行 / 786KB**，远超项目
约定的「800 行/文件 max」红线，单文件已难以维护、难以并行开发、难以让 AI 工具
在有限上下文内安全修改。

dashboard 通过 `setActivityView(view)` + 全局 `activeView` 在 4 个顶层视图间路由：

```
ACTIVITY_VIEWS = ['chat', 'assets', 'cron', 'settings']
```

其中 **assets 已经基本拆出**（`asset_browser.js` → `window.nzAssetView.{show,hide}`，
`setActivityView('assets')` 时调 `window.nzAssetView.show()`）。但 **cron 与 settings
仍埋在 dashboard.js 里**。cron 是其中体量最大、最内聚、且仍在持续迭代（cron-dashboard、
cron-skill-binding、cron-sysession-merge 等多个 RFC 在途）的域，是剥离的第一优先级。

### 1.1 量化现状

| 指标 | 数值 |
| :--- | :--- |
| cron 函数数量 | ~100 个（`isCronSessionKey` … `doEditCronJob`） |
| cron 函数代码区间 | 第 **11366–15300** 行（基本连续） |
| cron 全局变量/常量 | 23 个（`cronJobs`、`cronFilterQuery`、`cronTimelineState` … 见 §3.2） |
| cron → 核心 的依赖 | **25+** 个符号（含 `escJs`/`wsm`，见 §3.3 修订表；非 v1 的 11 个） |
| 核心 → cron 的依赖 | §3.4 的 ~10 个入口 **+ 19+ 个 inline-handler 目标（B1）+ `wsm` 反向调用（B2）** |
| 预计减少 dashboard.js 行数 | **~3900 行**（16564 → ~12700，不含 9756–10133 的 `wsm.cronLive` 块） |

cron 主体高内聚（函数集中在 11366–15300 连续区间），但**并非"对外耦合很轻"**：
inline handler 暴露面（B1）、`wsm.cronLive` 双向耦合（B2）、安全转义依赖（B3）使它
需要前置改造才能干净抽出。详见 §2.6。

---

## 2. 约束（决定实现方式，不可违反）

1. **无前端构建/打包工具**。仓库无 `package.json` build / rollup / vite / esbuild。
   静态 JS 直接以 `<script defer src="/static/*.js">` 顺序加载。
   → **不能用 ES module `import`/`export`**，模块间通信只能靠 `window.*` 全局命名空间
   + `<script>` 加载顺序。

2. **严格 CSP**（见 `dashboard_csp_test.go`）：`object-src 'none'`、`base-uri` 锁定，
   新增 `<script>` 必须走 `/static/` 同源路径。
   ⚠️ 修正：CSP 本身**并不禁止** inline `onclick` HTML 属性（cron 现状就大量使用，
   见 §2.6 B1），它管的是 `<script>`/`eval`。但项目用 `GeneratedHandlerSurfaceRatchet`
   棘轮**主动收紧** inline-handler 数量（只降不升）——这是治理约束，非 CSP 硬约束。
   data-action 迁移（B1）与该棘轮同向。

3. **大量共享全局状态**被多视图共用：`selectedKey`、`sessionsData`、`nodesData`、
   `ws`（WebSocket 实例）、`allSessionsCache` 等。这些是真正的内核，**不随视图搬走**。

4. **静态契约测试直接 grep `dashboard.js` 文本**（见 §6 修订清单）。拆分后被搬走的
   符号将不再出现在 dashboard.js，相关测试扫描目标必须同步指向新文件，否则 CI 红。

5. **`.js` 走 `//go:embed` + mux + auth，非目录服务**（见 §2.6 B4）。每个新 .js 文件
   需同步改 `static_assets.go` / `routes.go` / 鉴权测试三处。

6. **行为零变更（有例外）**。结构重构为主；但 B1（data-action 迁移）与 B2（`wsm.cronLive`
   归属）会触碰事件绑定与 WS 订阅生命周期，属**受控的非纯搬家**，须配回归测试。

---

## 2.5 公共基础层：值得先抽出的工具与对话渲染（第零步）

在抽 cron 之前，先识别 dashboard.js 里**真正被多视图复用**的公共能力。它们当前
是裸全局，散落在单体里；`agent_view.js` / `asset_browser.js` 已经靠"加载顺序 + 裸
全局"在隐式依赖它们（见 `agent_view.js` 文件头的依赖清单）。把它们显式沉淀为共享
层，既是 cron 干净抽出的前提，也消除现有隐式耦合。

### 2.5.1 公共工具函数（高频、无状态、纯函数）

实测全 dashboard.js 调用频次：

| 函数 | 定义行 | 全局调用次数 | 性质 | 跨视图复用证据 |
| :--- | :--- | :--- | :--- | :--- |
| `esc` | 7708 | **182** | 纯函数（HTML 转义，XSS 核心） | chat / cron / agent / asset 全用 |
| `escAttr` | 7717 | **90** | 纯函数（属性转义） | 同上 |
| `showToast` | 7020 | **54** | 副作用（DOM toast） | 全视图 |
| `fetchJSON` | 386 | 23 | 异步（带 token 的 fetch 封装） | 全视图 |
| `trapFocus` | 7376 | 11 | DOM（模态焦点陷阱） | 所有模态 |
| `send` | — | 11 | WebSocket 发送 | chat / cron |

→ 归入 **`nz_util.js`**（或挂 `window.nzCore`）。`esc`/`escAttr` 是安全敏感点，
**全仓必须唯一一份**，任何视图复用同一实现，严禁复制（防 XSS 实现漂移）。

### 2.5.2 对话/消息渲染层（用户指出的重点：「多个界面都有对话的显示」）

这是比工具函数更有价值的发现。**对话渲染逻辑被至少三处视图共享**：

| 渲染函数 | 定义行 | 调用次数 | 谁在用 |
| :--- | :--- | :--- | :--- |
| `eventHtml` | 3423 | 11 | chat 主视图 + agent_view.js（文件头声明依赖） |
| `renderEventsWithDividers` | 3611 | 6 | chat + agent_view.js（同上） |
| `renderMd` | 7951 | 7 | chat 气泡 + **cron transcript**（第 13523 行 `crs-text md`） |
| `renderMdUncached` | 8867 | 2 | renderMd 的实现 |
| `inlineMd` | 9134 | 11 | renderMd 内联 markdown（XSS 契约点） |
| `renderEvents` / `appendEvents` / `appendEventsToContainer` | 2981/3065/12803 | 各 2–3 | chat 流式 + cron live |

**实证三处复用同一套：**
- `agent_view.js` 文件头显式声明 `Depends on globals … eventHtml,
  renderEventsWithDividers, toolVerb`——agent 视图直接复用 chat 的事件渲染。
- cron 的 `cronRunTranscriptHtml`（13482）在渲染 Claude 回合时调 `renderMd(t.text)`
  （13523 行 `<div class="crs-text md">`）——cron transcript 复用 chat 的 markdown 渲染。
- cron live 区（`repaintCronLive` → `appendEventsToContainer`）复用 chat 的事件追加。

但同时存在**部分重复造轮子**：cron 的 `cronRunTurnHtml`（13505）手写了一套
turn/tool 气泡 HTML（`crs-turn` / `crs-tool-body`），与 chat 的 `eventHtml` 是
**两套并行的对话渲染实现**——这正是单体导致的隐性发散，是公共层最该收敛的地方。

→ 归入 **`nz_render.js`**：`eventHtml` / `renderEvents*` / `appendEvents*` /
`renderMd` / `renderMdUncached` / `inlineMd` / `toolVerb`。让 chat / cron / agent
三视图共用唯一一套对话渲染，cron 的 `cronRunTurnHtml` 后续可评估收敛到 `eventHtml`
的 transcript 模式（**本 RFC 不强制收敛，仅消除「调不到」的耦合，列为 §8 后续**）。

### 2.5.3 分层后的依赖方向

```
nz_util.js     ← esc / escAttr / escJs / fetchJSON / showToast / trapFocus     （零依赖，最先加载）
   ↑
dashboard.js   ← 全局状态 / WebSocket / session 列表 / 视图路由 / 对话渲染（内核）
   ↑
cron_view.js / agent_view.js / asset_browser.js / settings_view.js            （视图层）
```

**评审修正：放弃把对话渲染前移到 dashboard.js 之前的独立 `nz_render.js`。**
原因：`static_agent_view_test.go` 硬断言 `eventHtml`/`renderEventsWithDividers`
由 dashboard.js 以 `window.eventHtml = …` 形式导出、且 agent_view.js 在 dashboard.js
**之后**加载消费。把它们前移会打破该既有契约。且 `eventHtml`/`renderEvents*` 等
**触碰 core 的 DOM 与滚动状态**（`sessionScrollPos`、`events-scroll`），并非纯函数，
前移会形成 render→core 的隐藏反向边、破坏无环声明。

→ **对话渲染暂留 dashboard.js**，cron 通过 `window.nz.render.*`（由 dashboard.js
在末尾挂载的渲染 API 视图）消费，与 agent_view.js 现状一致。第零步只外提**真正纯净
零依赖**的 `nz_util.js`（含 `escJs`）。对话渲染层的独立成文件，留待对话渲染的两套
实现收敛之后再评估（§8）。

### 2.5.4 命名空间收敛（评审建议，采纳）

v1 散落 `window.nzUtil`/`nzRender`/`nzCore`/`nzCronView` 多个顶层全局，命名不一致、
职责含糊（`nzCore` 到底是工具白名单还是内核 API？）。**收敛为单一根命名空间**：

```
window.nz = {
  util:   { esc, escAttr, escJs, fetchJSON, showToast, trapFocus },   // nz_util.js 挂载
  render: { eventHtml, renderMd, inlineMd, renderEventsWithDividers }, // dashboard.js 末尾挂载（暂不独立成文件）
  core:   { wsm, selectSession, setActivityView, confirmDialog },      // dashboard.js 内核 API
  views:  { cron, agent, asset },                                      // 各视图模块挂载
}
```

一个全局、子对象分层，命名一致、可发现性好、全局污染最小。各视图在 IIFE 内首行
按需取用（不在顶层快照解构，避免加载期时序脆弱）。

---

## 2.6 BLOCKER：实施前必须解决（三方评审产出）

⚠️ 本节是 v2 的核心。v1 把抽取描述成"纯搬家、零行为变更、单 PR"，**经评审证伪**。
下列 4 项必须在动 cron 之前处理，否则照做必崩。

### B1（致命）— inline 事件处理器在 IIFE 包裹下全部失效

实测：cron 区（11366–15300）有 **41 个 `onclick=`**，外加 `onchange`/`oninput`/
`onkeydown`，引用至少 **19 个顶层 `function` 全局名**：`createNewCronJob`、
`doCreateCronJob`、`editCronJob`、`doEditCronJob`、`cronTriggerNow`、`cronPause`、
`cronResume`、`cronDelete`、`openCronDetail`、`closeCronDetail`、`cronSelectWorkspace`、
`toggleCronWsDropdown`、`toggleCronWsCustom`、`cronTimelineLoadMore`、
`cronTimelineSelectRun`、`cronTimelineToggleShowAll`、`setCronStatusFilter`、
`cronDrawerSpecPromptToggle`、`clearCronSearch`，以及 `onchange`/`oninput` 里的
`cronNotifyOnChange`、`cronNotifyOverrideToggle`、`freqSelectMode`、`freqUpdate`、
`freqMarkTouched`、`setCronSortOrder`、`onCronSearchInput`。

inline handler 靠"函数挂在 `window` 上"解析。一旦 §5 step2 把 cron 包进
`(function(){…})()` IIFE，这些函数变文件局部，点击即 `ReferenceError`，**cron UI
每个按钮变哑**。RFC v1 §3.4 只暴露 ~9 个"供核心调用"的函数，完全没覆盖这 19+ 个。

**前置修复（独立前置 PR，先于抽取）**：把 cron 的 inline handler 迁移到
`data-action` 事件委托——代码库已有范式 `CRON_MENU_ACTIONS`（dashboard.js:12547）、
`SIDEBAR_PROJECT_ACTIONS`。迁移后 handler 走单一委托监听器（在 cron_view.js 内部
注册），不再依赖全局函数名，IIFE 包裹才安全。这步还顺带降低 CSP inline-handler
计数（与安全棘轮 `generatedOnclickCap` 同向）。

### B2 — `wsm.cronLive` 双向耦合，推翻"连续区间"前提

cron 实时订阅逻辑**不在** 11366–15300 区间，而是长在 WebSocket 内核对象 `wsm` 里：

- `wsm.cronLive` 状态对象（dashboard.js:9756，9 字段）
- `wsm.subscribeCronLive`（10064）/ `unsubscribeCronLive`（10082）/ reconnect 恢复（10115）
- `onCronLiveHistory` / `onCronLiveEvent` / `onCronLiveSessionState`（被 9912/9916/9925 分发）
- 核心 WS 处理（9878）**反向调用** cron 的 `setCronLiveStatus()`

这是真正的双向耦合，v1 的"core →(只读)→ cron"单向模型套不上。

**决策（需评审拍板）**：
- **方案 A（推荐，但非零行为变更）**：把 `cronLive` 状态 + `subscribeCronLive`/
  `unsubscribeCronLive` + `onCronLive*` 三个 handler 全部移入 cron_view.js，内核 WS
  仅保留路由分发：`if (window.nz.views.cron?.isLiveKey(msg.key)) { window.nz.views.cron.onLiveEvent(msg); }`。生命周期 / `this` 绑定 / reconnect 时序需重验 + 回归测试，
  **标注为中风险重构，不得藏在"纯搬家"叙事里**。
- **方案 B**：`wsm` 整块留核心，cron 通过 `window.nz.core.wsm` 读写——则 cron 在写
  核心状态，"只读 API"说法不成立，分层纯净性打折。

### B3 — 依赖白名单实测从 11 个扩到 25+，且漏安全敏感函数

§3.3 的 11 个白名单不完整。实测 `repaintCronLive`/`appendEventsToContainer`/
`ensureCronLiveSubscription` 等就额外用到：

| 漏掉的核心依赖 | cron 区调用 | 性质 |
| :--- | :--- | :--- |
| `wsm` | 28 | **有状态核心对象**（见 B2），非工具函数 |
| `escJs` | 23 | **第三种转义**，inline onclick 的 JS 字符串转义，**XSS 敏感**，v1 全文未提 |
| `renderMd` | 6 | 对话渲染（归 nz_render） |
| `confirmDialog` | 2 | cronDelete 用 |
| `processEventsForDisplay` / `renderEventsWithDividers` / `isInternalEvent` / `eventHtml` / `timeDividerHtml` / `lastDividerTime` / `EVENT_DIVIDER_GAP_MS` / `updateCronLiveTruncated` / `setCronLiveStatus` | 各 1–2 | 事件渲染管线 |

**处理**：§3.3 表格按"逐符号 grep 定义位置 + 形态（自由函数 / 实例方法 / 常量）"
重做，每行附行号。安全三剑客 `esc`/`escAttr`/`escJs` 并列强调"全仓唯一一份、严禁复制"。

### B4 — Go 端 embed/route/auth 三处必须同步改（v1 §5 全漏）

`<script src="/static/*.js">` 不是目录服务，而是逐文件 `//go:embed` + 手工 mux +
auth 包裹：

- `internal/server/static_assets.go`：每个新 .js 加 `//go:embed` + 进 `staticAssets` read 表（~108–120），才有 ETag/gzip/预压缩。
- `internal/server/routes.go`（~270）：每个新 .js 加 `s.mux.HandleFunc("GET /static/cron_view.js", auth(handleCronViewJS))` + handler（仿 `handleAssetBrowserJS`）。**漏 → 404，页面整个起不来**。
- `internal/server/dashboard_static_js_auth_test.go`：每个新 .js 加 token-mode 401 + no-token 200 成对测试（否则成未鉴权 recon 泄露面，违反 SEC-4 #1328/#923）。

→ §5 实施步骤已据此补全（见修订后的 §5）。

---

## 3. 模块边界分析

### 3.1 待搬出的代码（→ `cron_view.js`）

第 11366–15300 行区间内的全部 cron 函数与状态，按子域分组：

| 子域 | 代表函数 | 职责 |
| :--- | :--- | :--- |
| 会话键判定 | `isCronSessionKey` / `isCronLiveKey` / `isCronSessionFrozen` | 判断某 session key 是否属于 cron |
| 排序/过滤 | `setCronSortOrder` / `filterCronJobs` / `setCronStatusFilter` | 列表排序与筛选 |
| cron 表达式 | `parseCronToFreq` / `humanizeCron*` / `buildFreq*` | 表达式 ↔ 人类可读 ↔ 频率选择器 |
| 创建/编辑表单 | `openCronCreateModal` / `openCronEditModal` / `doCreateCronJob` / `doEditCronJob` / `buildCronWorkspaceBody*` / `collectCron*` | 任务 CRUD 表单 |
| 列表/抽屉渲染 | `renderCronList` / `renderCronPanel` / `renderCronDrawer` / `cronJobCardHtml` / `cronDrawerHtml` | 主面板渲染 |
| 时间线/运行详情 | `getCronTimelineState` / `cronTimelineHtml` / `cronTimelineFetchDetail` / `cronRunTranscriptHtml` | run 历史 timeline + transcript sheet |
| Live 订阅 | `ensureCronLiveSubscription` / `repaintCronLive` / `setCronLiveStatus` | 正在运行 job 的实时事件流 |
| 运行态/冷却 | `cronApplyRunStarted` / `cronApplyRunEnded` / `cronTriggerNow` / `cronPause` / `cronResume` / `cronDelete` | 运行生命周期 + 操作 |
| 菜单/布局 | `toggleCronMenu` / `positionCronMenu` / `setupCronLayoutObserver` | 上下文菜单 + ResizeObserver 布局 |
| 数据拉取 | `fetchCronJobs` / `cronRefetchFullJob` | API 拉取 |
| 详情导航 | `openCronPanel` / `openCronDetail` / `closeCronDetail` / `openCronSession` | 视图入口与详情开合 |

以及 23 个 cron 全局变量（§3.2）。

> 注：`CRON_LIVE_MAX_EVENTS`、`CRON_LIVE_AGENT_ONLY_HTML`（第 7640/7646 行）位于
> cron 主区间之外，但属 cron 专用常量，一并搬出。

### 3.2 待搬出的模块级状态（封进 `cron_view.js` 闭包/命名空间）

```
cronJobs, cronNotifyDefault, cronDetailJobId, _cronDrawerFetchedFor,
_cronDrawerLastActiveRow, cronFilterQuery, cronFilterStatus, cronSortOrder,
cronSortComparators, cronMenuOpenId, cronMenuOnDoc, cronMenuOnScroll,
CRON_MENU_ACTIONS, cronFrozenRuns, cronRunningTickTimer, cronTimelineState,
cronExpandedRunId, CRON_TIMELINE_FRESH_MS, _cronTimelineRefreshScheduled,
cronJustTriggered, CRON_TRIGGER_COOLDOWN_MS, cronTriggerCooldownTickTimer,
CRON_LIVE_MAX_EVENTS, CRON_LIVE_AGENT_ONLY_HTML
```

这些当前是 dashboard.js 的文件级 `let/const`。搬入 `cron_view.js` 后成为该文件的
文件级状态，**不再污染全局**（净收益：核心命名空间收缩 23 个符号）。

### 3.3 cron → 核心 依赖（修订：实测 25+ 个，非 v1 的 11 个）

⚠️ v1 此表严重偏低且有错（评审 B3）。按 cron 区（11366–15300）实际调用语法重测，
分三类。**每行须在实施前再以 `grep -n` 复核定义位置与形态**（自由函数 / 实例方法 /
常量），下表为评审实测值：

**(a) 纯工具函数 → `window.nz.util`**

| 函数 | 定义行 | cron 区调用 | 说明 |
| :--- | :--- | :--- | :--- |
| `esc` | 7708 | ~56 | HTML 转义，**XSS 敏感** |
| `escAttr` | 7717 | ~35 | 属性转义，**XSS 敏感** |
| `escJs` | 7720 | **~23** | **JS 字符串转义，inline onclick 专用，XSS 敏感（v1 漏列）** |
| `fetchJSON` | 386 | ~10 | 带 token 的 fetch |
| `showToast` | 7020 | ~7 | 提示条（注：`toast` 别名 cron 区 0 次） |
| `trapFocus` | 7376 | 2 | 模态焦点陷阱 |

**(b) 对话渲染 → `window.nz.render`（暂留 dashboard.js，见 §2.5.3）**

`renderMd`(~6)、`eventHtml`、`renderEventsWithDividers`、`processEventsForDisplay`、
`isInternalEvent`、`timeDividerHtml`、`appendEventsToContainer` 等事件渲染管线函数。

**(c) 有状态核心 / 路由 → `window.nz.core`**

| 符号 | 形态 | cron 区调用 | 说明 |
| :--- | :--- | :--- | :--- |
| `wsm` | **核心可变状态对象** | **~28** | 见 B2，**不是工具函数，不能"只读 API"**；cron live 状态住其中 |
| `selectSession` | 自由函数 | 1 | cron→session 跳转 |
| `setActivityView` | 自由函数 | 2 | 视图路由 |
| `confirmDialog` | 自由函数 | 2 | cronDelete 确认 |

> v1 列的 `byId`/`debounce`/`send` **不是可解构的自由工具函数**：`byId` 是局部
> `const byId = new Map()`（非全局函数）、`send` 是 `wsm.send` 实例方法、`debounce`
> cron 区 0 次直接调用。已从白名单剔除/修正。

> 安全红线：`esc`/`escAttr`/`escJs` 三者都是注入防护点（`escJs` 防 JS 字符串上下文
> 注入，与 `static_cron_panel_consolidation_test.go:56` 的 `onclick="…escJs(j.id)…"`
> 契约相关）。全仓**唯一一份**，cron 复用 `window.nz.util.*`，**严禁复制**（asset_browser.js
> 自带私有 `esc` 已是漂移反例，见 §8 清理项）。

### 3.4 核心 → cron 依赖（核心需调用 cron 的入口，反向暴露给核心）

| 调用点（dashboard.js 行） | 调用的 cron 函数 | 场景 |
| :--- | :--- | :--- |
| 326 `bindClick('btn-cron')` | `openCronPanel()` | 点击侧栏 cron 按钮 |
| 373 `setActivityView` | `openCronPanel()` | 视图路由进入 cron |
| 2231 | `isCronSessionKey(key)` | session 卡片渲染判定 |
| 9912/9916/9925 WS 分发 | `isCronLiveKey(msg.key)` | WebSocket 消息归类 |
| 9962 | `cronApplyRunStarted(msg)` | cron run 开始事件 |
| 9976/9977 | `cronApplyRunEnded(msg)` + `fetchCronJobs().then(renderCronPanel)` | cron run 结束事件 |
| 10131 | `ensureCronLiveSubscription()` | WS 重连后恢复订阅 |
| 10591 | `repaintCronLive()` | live 区重绘 |

**实现方式**：cron_view.js 在加载时挂载
`window.nz.views.cron = { openCronPanel, isCronSessionKey, isLiveKey, applyRunStarted, applyRunEnded, ensureLiveSubscription, repaintLive, fetchJobs, renderPanel, onLiveHistory, onLiveEvent, onLiveSessionState, setLiveStatus }`。
核心改为 `window.nz.views.cron?.xxx()` 调用，对早于 cron_view.js 加载的调用点保留
`typeof`/可选链守卫（核心已有先例，见第 10131 行）。

> ⚠️ 此表是 v1 的"~9 个供核心调用的入口"，**但它不是 cron 的全部对外暴露面**。
> 真正必须暴露的还有 B1 的 19+ 个 inline-handler 目标（迁移到 data-action 后由
> cron 内部委托监听器处理，不再需全局暴露）+ B2 的 `onCronLive*`/`setCronLiveStatus`
> （若采纳方案 A，从内核 WS 反向调用）。完整暴露面 = 本表 + B2 handler，见 §2.6。

---

## 4. 目标架构

```
dashboard.html
  <script defer src="/static/nz_util.js">       ← 第零步新增：纯工具（esc/escAttr/escJs/fetchJSON/showToast/trapFocus）
  <script defer src="/static/dashboard.js">     ← 内核：全局状态 / WebSocket / session 列表 / 视图路由 / 对话渲染
                                                    末尾挂 window.nz.{render,core}
  <script defer src="/static/agent_view.js">    ← 已拆（现状不动）
  <script defer src="/static/asset_browser.js"> ← 已拆 (window.nzAssetView)
  <script defer src="/static/cron_view.js">     ← 第一步新增 (window.nz.views.cron)
```

加载顺序：`nz_util.js` → `dashboard.js` → 各视图。`defer` 脚本按文档序、
DOMContentLoaded 前执行，顺序有保证（`static_precompress_test.go:ScriptsDeferred`
已锁 defer）。跨模块调用都在运行时回调内，core 入口保留可选链守卫兜底极早期 WS 帧。

> **对话渲染不前移**（见 §2.5.3）：它暂留 dashboard.js，由其末尾挂到
> `window.nz.render`。`nz_util.js` 是唯一前置于 dashboard.js 的新文件，因其零依赖
> 纯函数、且被 dashboard.js 自身也要用到。

契约边界：`window.nz.util`（前置公共层）、`window.nz.render` / `window.nz.core`
（dashboard.js 末尾挂载）、`window.nz.views.cron`（cron 导出，被核心调用）。

---

## 5. 实施步骤（分 4 个 PR，每个独立可回退）

⚠️ v1 的"单 PR 纯搬家"经评审否决。因 B1/B4，**必须拆成有依赖顺序的多个 PR**，
每个 PR 独立绿、独立可 revert：

**PR-0a：抽 `nz_util.js`（最低风险，先行）**
1. 新建 `internal/server/static/nz_util.js`，把 §3.3(a) 的 6 个纯工具函数
   （`esc`/`escAttr`/`escJs`/`fetchJSON`/`showToast`/`trapFocus`）搬入，挂 `window.nz.util`，
   dashboard.js 内保留薄转发（`function esc(s){return window.nz.util.esc(s)}`）或全量改引用。
2. **Go 端脚手架（B4）**：`static_assets.go` 加 `//go:embed nz_util.js` + read 表；
   `routes.go` 加 `GET /static/nz_util.js` + auth handler；`dashboard_static_js_auth_test.go`
   加 401/200 成对测试。
3. dashboard.html 在 dashboard.js **之前**加 `<script defer src="/static/nz_util.js">`。
4. 顺带收敛 asset_browser.js 自带的私有 `esc`/`fetchJSON` 到 `window.nz.util`（§8 漂移清理）。

**PR-0b：cron inline handler → data-action 事件委托（B1 前置，不抽文件）**
5. 把 cron 区 41 个 inline `onclick`/`onchange`/`oninput` 改为 `data-action="cronTriggerNow"`
   等 + 单一委托监听器（仿 `CRON_MENU_ACTIONS`/`SIDEBAR_PROJECT_ACTIONS`）。**此 PR 不
   移动任何代码到新文件**，纯就地改事件绑定方式，cron 仍在 dashboard.js。便于单独验证
   "所有 cron 按钮仍工作" + 降低 CSP inline-handler 计数。

**PR-1：抽 `cron_view.js`（核心步骤，依赖 PR-0a/0b）**
6. 新建 `cron_view.js`，把 §3.1 函数 + §3.2 状态 + §3.4 暴露面 + （若采纳 B2 方案 A）
   `wsm.cronLive` 相关 handler 剪切进 IIFE，挂 `window.nz.views.cron`。
7. dashboard.js 末尾挂 `window.nz.render`/`window.nz.core`（§3.3 b/c）；核心内对 cron 的
   调用（§3.4 + B2）改为 `window.nz.views.cron?.xxx()`。
8. **Go 端脚手架**（同 PR-0a 第 2 步，针对 cron_view.js）。
9. dashboard.html 加 `<script defer src="/static/cron_view.js">`（dashboard.js 之后）。
10. 更新 §6 全部受影响测试。

**每个 PR 收尾**：`go test ./internal/server/...` 全绿 + 手动 smoke
（PR-0b/PR-1 必测：进 cron 视图、建/停/删 job、看 timeline、live 重绘、reconnect 恢复订阅）。

> 不引入打包工具、不改 API/WS 协议、不改 cron 的 HTML 容器与 CSS。B2 方案 A 会动
> WS 订阅生命周期，**非零行为变更**，PR-1 须含 reconnect 回归测试。

---

## 6. 测试影响（CI 红线，修订）

⚠️ v1 此节漏列 3 个会 CI 红的测试、误标 1 个、错判 1 个为"不受影响"。修订后清单：

**确认 grep `dashboard.js` 文本、cron 符号搬走后需改扫描目标：**

| 测试文件 | 断言的 cron 符号（示例） |
| :--- | :--- |
| `static_cron_compact_poll_test.go` | cron 列表/轮询相关 |
| `static_cron_history_redesign_test.go` | `cronExpandedRunId`（正向）；`cronRunSheetState`（**负向**——要求"已删除、禁止再现"，方向相反，改文件时勿误判） |
| `static_cron_panel_consolidation_test.go` | `cronDetailJobId`、`onclick="…escJs(j.id)…"`（实证 inline+escJs 依赖） |
| `static_cron_schedule_test.go` | cron 表达式/调度相关 |
| **`static_ux_contract_test.go`（v1 错判为"不受影响"）** | **十余个** `function cronXxx`：`humanizeCronStepValue`、`filterCronJobs`、`cronJobCardHtml`、`cronDrawerHtml`、`cronDrawerCockpitHtml`、`cronRunTranscriptHtml`、`cronRunTurnHtml`、`setCronStatusFilter`、`onCronSearchInput`、`cronTriggerNow`、`isCronSessionKey`、`buildFreqSchedule`、`cronRunSheetSelectTab`、`cronStatsBadgeHtml` 等 |
| **`static_event_dom_cap_test.go`（v1 漏）** | `CRON_LIVE_MAX_EVENTS` + `appendEventsToContainer` 内 `bubbles > CRON_LIVE_MAX_EVENTS`（12824 行） |
| **`static_toplevel_views_contract_test.go`（v1 漏）** | `renderCronPanel` 的 `if (activeView !== 'cron') return;` + `getElementById('cron-main')`、`fetchCronJobs` |

**误标，应剔除**：`cronview_contract_test.go` 是 **Go 接口编译期契约**
（`var _ CronView = (*cron.Scheduler)(nil)`），不 grep JS 文本，与本次抽取无关。

**CSP 棘轮（不会红，但需主动维护，否则安全治理倒退）**：
- `dashboard_csp_test.go:GeneratedHandlerSurfaceRatchet`（cap=84）：PR-0b 迁走 41 个
  onclick 后 dashboard.js 计数下降仍 PASS，但**迁移后的 data-action 不再被该棘轮守护**
  ——PR-0b 应把 cap 下调到迁移后实际值（棘轮只降不升原则）。
- `StaticHandlersWiredInJS` 断言 `function openCronPanel(` 在 dashboard.js——`openCronPanel`
  在抽取范围，PR-1 须同步该断言到 cron_view.js。

**Go 鉴权测试（B4）**：每个新 .js（nz_util.js、cron_view.js）须在
`dashboard_static_js_auth_test.go` 加 token-mode 401 + no-token 200 成对用例。

**处理方案**：定位各测试读取 `static/dashboard.js` 的辅助函数，对搬走的符号改为读
对应新文件（按符号归属精确指向，不用两文件拼接）。`dashboard_csp_test.go`（CSP 头本身）
仍是"核心未破坏"的回归护栏。

---

## 7. 风险与缓解

| 风险 | 等级 | 缓解 |
| :--- | :--- | :--- |
| **B1：inline handler 在 IIFE 下全失效，cron 按钮全哑** | **致命** | PR-0b 先迁 data-action 事件委托再抽文件；smoke 必测每个按钮 |
| **B2：`wsm.cronLive` 双向耦合，搬动改 WS 订阅生命周期** | **高** | 采纳方案 A 须含 reconnect/subscribe 时序回归测试，标注非零行为变更 |
| **B3：`escJs` 等安全敏感依赖漏列，引用未定义符号崩溃** | **高** | §3.3 已重测补全；实施前逐符号 grep 复核定义形态 |
| **B4：Go embed/route/auth 漏改 → 404 / 未鉴权泄露** | **高** | §5 每个抽文件 PR 含 Go 脚手架三件套 + 鉴权测试 |
| `esc`/`escAttr`/`escJs` 被复制导致 XSS 实现漂移 | 高 | 全仓唯一一份，强制复用 `window.nz.util.*`；asset_browser 私有 esc 一并收敛 |
| 加载时序：WS 消息早于视图就绪 | 中 | core→cron 反向调用点（含 B2 的 `setCronLiveStatus`）全部加可选链守卫 |
| 漏搬函数 / 漏改引用 | 中 | 抽取后 grep cron 函数名在 dashboard.js 应仅剩 `window.nz.views.cron.*` 与守卫 |
| 契约测试扫错文件 CI 红 | 中 | §6 修订清单同 PR 修复，本地先跑 `go test ./internal/server/...` |
| 共享核心状态被误搬进 cron | 高 | §3.2 私有白名单；`selectedKey`/`ws`/`sessionsData` 明确**留核心** |
| CSP 棘轮迁移后失守 | 中 | PR-0b 下调 `GeneratedHandlerSurfaceRatchet` cap 到迁移后实际值 |

---

## 8. 后续步骤（不在本 RFC 实施范围）

- **第二步**：同模式抽 `settings_view.js`（`renderSettingsView` 及其依赖，体量远小于 cron）。
- **收敛两套对话渲染**：把 cron 的 `cronRunTurnHtml`（手写 turn/tool 气泡）收敛到
  公共层 `eventHtml` 的 transcript 模式，消除 §2.5.2 指出的并行实现。收益大但触碰
  渲染行为、影响 `crs-*` 相关 CSS 与契约测试，风险高，独立 PR 处理。
- **改造 agent_view.js 的隐式依赖**：当前它靠文件头注释声明依赖裸全局；公共层落地后
  改为显式消费 `window.nz.util` / `window.nz.render`，去掉隐式耦合。
- **收敛 asset_browser.js 的私有 `esc`/`fetchJSON`**（PR-0a 顺带，§3.3 漂移反例）。
- **cron_view.js 内部再分（已承诺第二阶段，非"是否要做"）**：cron_view.js ~3900 行
  仍超 800 行红线约 5 倍。抽出后按 §3.1 子域再切：`cron_timeline.js`（timeline/transcript/
  run 详情，最大最内聚）、`cron_form.js`（创建/编辑 + 表达式解析）、`cron_panel.js`
  （列表/抽屉/菜单/拉取）。子模块同走 `window.nz.views.cron.*` 内部约定。
- 远期可选：若视图继续增多，再评估是否引入轻量打包（esbuild）。**首期明确不做**——
  保持"零构建、直接 serve"的现有简洁性。

---

## 9. 待评审项（v2）

1. **B2 方案 A vs B**：`wsm.cronLive` 是整体移入 cron_view（方案 A，分层干净但改 WS
   订阅生命周期、需回归测试），还是留核心由 cron 经 `window.nz.core.wsm` 读写（方案 B，
   省事但 cron 写核心状态、分层纯净性打折）？**本 RFC 倾向 A**，请拍板。
2. **命名空间**：单一根 `window.nz.{util,render,core,views}`（§2.5.4，已采纳评审建议）
   是否最终确认？是否需要给旧代码留 `window.esc` 等顶层别名一段过渡期？
3. **PR 粒度**：§5 拆成 PR-0a/0b/1 三个，是否同意？还是 0a+0b 合并（都不抽文件、风险低）？
4. **`window.nz.render` 是否最终独立成 `nz_render.js`**：本 RFC 因 `static_agent_view_test.go`
   契约 + 渲染函数有 DOM 副作用，决定**暂留 dashboard.js**。待对话渲染两套实现收敛后
   （§8）再评估独立成文件。是否同意此推迟？
