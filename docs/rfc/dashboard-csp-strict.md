# RFC: Dashboard CSP — drop `'unsafe-inline'` via hash/externalize (and the nonce/strict-dynamic question)

> **状态**: Draft v1（待评审）
> **作者**: naozhi team (cron-cr)
> **创建**: 2026-06-04
> **范围**: 移除 `/dashboard` 响应里 script-src / style-src 的 `'unsafe-inline'`，闭合 DOM-XSS→任意脚本执行的链路；定方案（全外置 vs 每请求 nonce+strict-dynamic）、分相、回归 lint。
> **关联 issue**: #1734, #922（亦牵涉历史 #441 / #479 / #562 / #605 / #607 / #1526）
> **关联代码**:
> - `internal/server/routes.go:404-549`（`handleDashboard`：读 embed.FS、CSP 头、ETag/304 路径、写 body）
> - `internal/server/routes.go:489-522`（self-marked NEEDS-DESIGN 注释 + `script-src 'unsafe-inline'` 行）
> - `internal/server/static_assets.go:19-34`（`//go:embed` 静态资源 + `serveStaticWithETag`）
> - `internal/dashboard/auth/handlers.go:340-396`（登录页 hash-based CSP 先例 + `init()` 自检 panic + `extractInlineBlocks`/`hashInline`）
> - `internal/server/static/dashboard.html:16-26`（头部 inline theme 引导 `<script>`）、`:2491-2493`（外置 script 标签）、8 处 inline `onclick`/`onsubmit`、1 处 inline `<style>`、11 处 inline `style=`
> - `internal/server/static/dashboard.js:7410-7476`（mermaid/KaTeX 动态 `createElement('script')` 注入；~84 处生成 HTML 字符串内的 `onclick=`；~94 处 `.style.` 变更）
> - 现有 pin 测试：`internal/server/dashboard_csp_test.go`（尤其 `TestDashboardCSP_ScriptSrcUnsafeInlineMigrationGate`、`TestDashboardCSP_InlineHandlerSurfaceDoesNotGrow`）、`dashboard_csp_data_audit_test.go`、`dashboard_csp_katex_font_sri_test.go`

---

## 1. Background & problem

### 可复现症状
`/dashboard` 的每个响应都带这条 CSP（`internal/server/routes.go:522`）：

```
default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net/npm/; connect-src 'self'; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net/npm/; font-src 'self' https://cdn.jsdelivr.net/npm/; img-src 'self' data: blob:; frame-src 'self' blob:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; require-sri-for script style font
```

复现：`curl -sI http://<host>/dashboard | grep -i content-security-policy`，可见 `script-src` 与 `style-src` 都含 `'unsafe-inline'`。

### 为何是问题
`script-src 'unsafe-inline'` 让 CSP 对脚本注入几乎失效：任何把可控字符串写进 dashboard DOM 的 DOM-XSS（dashboard.js 有 ~94 处 `.style.`、大量 `innerHTML`/模板字符串拼 HTML，含 ~84 处生成的 `onclick=`），都能直接执行任意 `<script>` 或内联 handler。`base-uri 'none'`、`object-src 'none'`、`form-action 'self'`、`connect-src 'self'`、SRI 等都是围着这个洞做的纵深防御补丁——但核心洞（任意内联脚本执行）始终敞着。issue #1734（本 RFC 主题）与 #922（历史聚合 issue）即要求移除它。

代码已自标 NEEDS-DESIGN（`routes.go:489-521`）：注释明确指出 strict-dynamic+nonce 与现状的 `'unsafe-inline'` **互斥**（CSP3：一旦出现 nonce/strict-dynamic，浏览器忽略 `'unsafe-inline'`），所以不能半迁移。`TestDashboardCSP_ScriptSrcUnsafeInlineMigrationGate`（`dashboard_csp_test.go:331-382`）把这点 pin 成测试：**加 nonce/strict-dynamic 必须与删 `'unsafe-inline'` 在同一变更里发生**，否则现有内联 handler 会被静默禁用。

### 现状内联面（核实于 2026-06-04 实际代码）
- `dashboard.html:16-26`：1 个 inline `<script>`（localStorage 读 `nz_theme` 设 `data-theme`，须在首绘前同步执行以避免主题闪烁——FOUC 敏感，不能简单外置成异步 `<script src>`）。
- `dashboard.html`：1 个 inline `<style>` 块；11 处 inline `style=` 属性。
- `dashboard.html`：8 处 inline 事件 handler（`onclick`×7 + `onsubmit`×1），均在头部按钮/quick-ask 表单上。`TestDashboardCSP_InlineHandlerSurfaceDoesNotGrow` 把 `onclick` 上限 pin 在 7。
- `dashboard.js`：~84 处生成 HTML 字符串里嵌 `onclick=`（动态渲染的消息/卡片/列表行）——**这是 nonce 方案的致命点**：nonce 只覆盖 `<script>` 元素，对 inline `on*=` 属性无效（CSP 永远禁止带 nonce 的内联属性 handler）。要去 `'unsafe-inline'`，这 ~84 处必须改成 `addEventListener` / 事件委托。
- `dashboard.js:7410-7476`：mermaid/KaTeX 走 `document.createElement('script')` 动态注入带 `integrity` 的 CDN script——这是 `strict-dynamic` 的典型适配场景（已加载脚本可传播信任），但也意味着方案选择牵动 CDN allowlist。
- `agent_view.js` / `asset_browser.js`：inline `on*=` 计数为 0（无新增内联面）。

### 先例
登录页（`auth/handlers.go:356-373`）已用 **sha256 hash-based** CSP：开包时 `buildLoginPageHTML` 用 `extractInlineBlocks`+`hashInline` 把内联 `<script>`/`<style>` 算成 `'sha256-…'`，`init()`（`:347-354`）做自检——若正则抽不到块就 panic，防"登录页自己被自己 CSP 封死"的首请求才暴露的故障。这是本 RFC 优先复用的模式。

---

## 2. Goals & non-goals

### Goals
1. `/dashboard` 响应的 `script-src` 与 `style-src` 不再含 `'unsafe-inline'`，且不留"半迁移"状态（不与 nonce/strict-dynamic 并存导致静默破坏）。
2. dashboard 全功能（主题引导、头部按钮、quick-ask、消息渲染交互、mermaid/KaTeX、workspace .html 预览、图片预览）在新 CSP 下保持工作。
3. 加回归 lint/测试，防止未来新增内联 `<script>`/`on*=`/`style=` 再把洞撕开。
4. 与现有所有 pin 测试（connect-src、base-uri、form-action、object-src、frame-src blob、jsdelivr /npm/ scoping、SRI）保持一致，不回退任何一条。

### Non-goals
- **不** vendoring CDN 资产到 `//go:embed`（mermaid/KaTeX ~6MB，`routes.go:444-468` 另案跟踪）。本 RFC 保留 `https://cdn.jsdelivr.net/npm/` allowlist + SRI 不变。
- **不**收紧 `img-src data:`（`routes.go:472-488` 的 SEC-14 审计另案；`TestDashboardCSP_DataImgAuditPinned` 已 pin）。
- **不**改登录页 CSP（已是 hash-based，独立达标）。
- **不**引入服务端模板引擎/SSR 渲染管线（dashboard 是 embed.FS 静态文件 + ETag，引入模板渲染是大改）——除非选中 nonce 方案，那时再权衡（见 §3）。
- **不**改 `connect-src`、`frame-ancestors`、HSTS/COOP/CORP 等非 inline 相关 directive。
- **不**做 CSP report-only 上报后端基础设施（`report-uri`/`report-to`）作为长期方案——可作为 rollout 期临时手段（见 §6/§8）。

---

## 3. Alternatives considered

### 方案 A：全外置 + hash（选中）
把所有内联面消除或转成可静态 hash 的形态：
- **inline `<script>`（theme 引导）**：保留为内联块，但用登录页同款 `'sha256-…'` hash 授权（开包时对 `dashboard.html` 内联块算 hash，注入 script-src）。**或**移到独立 `/static/theme-boot.js`（外置 `<script src>`，被 `script-src 'self'` 覆盖）——但外置会异步化，引入主题闪烁；hash 内联保持同步执行，无 FOUC。倾向 hash。
- **inline `<style>` 块 + 11 处 `style=`**：`<style>` 块用 `'sha256-…'` hash 授权；inline `style=` 属性受 `style-src-attr` 管，hash 覆盖不了——需迁成 class 或保留 `style-src-attr 'unsafe-inline'`（见下文权衡）。
- **8 处 HTML inline `on*=` + ~84 处 JS 生成的 `onclick=`**：全部改成 `addEventListener` / 事件委托（在 dashboard.js 顶层对容器装一个 delegated click 监听，按 `data-action` 派发）。
- **mermaid/KaTeX 动态 script**：`script-src 'self' https://cdn.jsdelivr.net/npm/` 已经覆盖（动态创建的 `<script src=CDN>` 仍按 host allowlist 校验），**无需 strict-dynamic**。

胜出理由：
- 与登录页先例同构（`extractInlineBlocks`/`hashInline` 可直接复用/提取为共享包），心智负担最低。
- **hash 是静态的**，与 dashboard 的 embed.FS + ETag/304 缓存路径天然兼容——CSP 头是常量，body 不变，304 命中时无需重算（见 §5 关键约束）。
- 不需要任何"每请求生成随机值"的基础设施。
- `script-src` 可彻底去 `'unsafe-inline'`，DOM-XSS 注入的 `<script>`（hash 不匹配）与注入的 `on*=`（hash/nonce 都管不了，但 `'unsafe-inline'` 一去就全禁）双双被挡。

代价：~84 处 JS 生成 handler + 8 处 HTML handler 必须迁移，是最大工作量；这是为什么本轮 hasLandablePhase1=false。

### 方案 B：每请求 nonce + strict-dynamic（否决为主路径）
每次 `/dashboard` 生成随机 nonce，注入 `<script nonce>` 与 CSP `script-src 'nonce-…' 'strict-dynamic'`。
否决理由（致命）：
1. **与 ETag/304 缓存冲突**：`handleDashboard`（`routes.go:544`）走 `serveStaticWithETag`，命中 304 时 **body 根本不写**。nonce 必须同时出现在 body 的 `<script nonce>` 和头里——304 复用旧 body 但头里是新 nonce，必然不匹配，dashboard 直接白屏。要支持 nonce 必须**放弃静态 embed + ETag**，改成每请求渲染（模板替换 nonce 占位符）+ 重算/禁用缓存，与 §2 non-goal 冲突，且伤首字节缓存性能。
2. **nonce 管不了 inline `on*=`**：strict-dynamic 也不行。那 8+84 处 handler **仍然必须迁移**——nonce 并没省掉方案 A 的主要工作量，却额外背上 per-request 渲染与缓存重构。
3. strict-dynamic 会**忽略** host allowlist（`https://cdn.jsdelivr.net/npm/`），改为信任传播——这反而让现有 `TestDashboardCSP_JsdelivrNpmPathScoped` 的 /npm/ path-scoping 失效（host-source 被 strict-dynamic 旁路），是一次防御**降级**。

结论：nonce/strict-dynamic 对 naozhi 是负收益——既不省 handler 迁移，又破坏缓存与 CDN path-scoping。方案 A 完胜。

### 方案 C：`script-src-elem` / `style-src-attr` 细粒度拆分（部分采纳为补充）
CSP3 允许把 `script-src` 拆成 `script-src-elem`（`<script>` 元素）与 `script-src-attr`（inline `on*=`）。可用 `script-src-attr 'none'` 显式禁内联 handler、`style-src-attr 'unsafe-inline'` 临时容忍 11 处 inline `style=`。
不作主路径：浏览器兼容性参差（老 Safari 把未知 `*-attr` 回退到 `script-src`/`style-src`，可能放宽），且 naozhi 仍要迁 handler 才能让 `script-src-attr 'none'` 不破功能。**采纳点**：rollout 末期可用 `style-src-attr` 单独承接 inline `style=` 残留（见 §8 Phase 3），避免为 11 个 `style=` 卡住整条迁移。

---

## 4. Test strategy

### Unit
- `auth` 包已有的 `extractInlineBlocks`/`hashInline` 抽取为共享小包（如 `internal/csputil`）后，对其做表驱动单测：空串、单块、多块、跨标签不误匹配（沿用 `auth/handlers.go:375-382` 分离正则的理由）。
- `server` 包：新增 `buildDashboardCSP()`（把 `routes.go:522` 的常量化为构造函数），单测断言产出串不含 `'unsafe-inline'`（script/style 两处），含 theme-boot 的 `'sha256-…'`，且保留所有现存 directive。

### Integration（httptest，复用 `newTestServer(&mockPlatform{})` + `s.handleDashboard`）
- 新 `TestDashboardCSP_NoUnsafeInline`：隔离 `script-src` 与 `style-src` 两个 directive（沿用 `dashboard_csp_test.go:348-355` 的按 `;` 切分隔离法），断言均不含 `'unsafe-inline'`。
- 新 `TestDashboardCSP_ThemeBootHashAuthorized`：对 `dashboard.html:16-26` 内联块算 sha256，断言该 hash 出现在 script-src（防 HTML 改动后 hash 漂移导致主题脚本被自己 CSP 封死——对应登录页 `init()` 自检的同类保护）。建议同样加 `init()` panic 自检。
- ETag 路径：断言开/关 `If-None-Match` 两条路径下 CSP 头一致（验证 §5 约束：CSP 静态、304 仍带正确头）。

### Regression / 防回归 lint（核心，防洞重开）
- **改造 `TestDashboardCSP_ScriptSrcUnsafeInlineMigrationGate`**（`dashboard_csp_test.go:331-382`）：迁移落地时，把"必须含 `'unsafe-inline'` 且不得含 nonce"反转为"**不得**含 `'unsafe-inline'`"。这是 atomic-cut 的执行点。
- **改造/替换 `TestDashboardCSP_InlineHandlerSurfaceDoesNotGrow`**（`:264-303`）：当前 cap=7（onclick）。迁移后改为 cap=0（HTML 内联 handler 必须清零），并把扫描扩展到 dashboard.js 的生成字符串（grep `on(click|change|input|submit|...)=`），把"不得新增内联 handler"做成持续 lint。`onload=`/`onerror=`/`onfocus=`/`onmouseover=` 维持零（已有）。
- 新增对 `dashboard.html` 的 lint：除已 hash 授权的 theme-boot 块外，不得出现新的内联 `<script>`/`<style>`（数量 pin，类似登录页 `init` 自检思路）。
- 维持不变：`TestDashboardCSP_ConnectSrcSelfOnly`、`BaseURIAndFormAction`、`ObjectSrcNone`、`FrameSrcBlob`、`JsdelivrNpmPathScoped`、`dashboard_csp_data_audit_test.go`、`dashboard_csp_katex_font_sri_test.go` 全部继续通过（迁移不得回退任一）。

### E2E（Playwright，关键用户流）
- 头部按钮（sidebar search/history/new session/cron panel/sidebar toggle）点击生效。
- quick-ask 表单 Enter 起会话。
- 消息列表里动态渲染的 `onclick`（追问 ↗、卡片按钮、AskUserQuestion 选项）经 delegated handler 仍生效。
- mermaid 图、KaTeX 公式渲染（验证 CDN 动态 script 在新 script-src 下仍加载，SRI 仍 pin）。
- workspace .html 预览（frame-src blob:）+ 图片上传预览（img-src blob:）不回归。
- 浏览器 console 无 CSP violation 报错（可用 `browser_console_messages` 断言零 CSP 拒绝）。

---

## 5. Risk & rollback

### 出错会 break 什么
- **最高风险**：若删 `'unsafe-inline'` 而 handler 迁移有遗漏，对应按钮/交互静默失效（点击无反应，无报错堆栈，只有 console CSP violation）——故 E2E + console violation 断言是硬门槛。
- 若 theme-boot hash 算错/HTML 改了没重算，主题脚本被 CSP 封 → 首绘主题闪烁或 `data-theme` 不设。`init()` 自检 panic（仿登录页 `auth/handlers.go:347-354`）把这降级为进程启动期可见错误。
- strict-dynamic（若误采方案 B）会旁路 `JsdelivrNpmPathScoped` 的 /npm/ scoping——方案 A 不引入 strict-dynamic，规避此风险。

### Load-bearing 不变量与已有 pin
- **ETag/304 body-skip 不变量**（`routes.go:544` `serveStaticWithETag` 命中即 `return`，不写 body）：CSP 必须保持**与请求无关的静态串**。任何"每请求变体"（nonce）都会与此冲突——方案 A 的 hash 是静态的，安全。
- **atomic-cut 不变量**（`TestDashboardCSP_ScriptSrcUnsafeInlineMigrationGate`）：nonce/strict-dynamic 与 `'unsafe-inline'` 不得并存。本 RFC 选 hash，根本不引入 nonce，天然满足；该测试在落地时反转方向继续守门。
- **CDN /npm/ path-scoping**（`TestDashboardCSP_JsdelivrNpmPathScoped`，`dashboard_csp_test.go:204-246`）：方案 A 保留 host allowlist，不引 strict-dynamic，scoping 不变。
- **SRI 契约**（`dashboard_csp_katex_font_sri_test.go`、`TestDashboardJS_CDNScriptsHaveSRI`）：mermaid/KaTeX 动态 script 的 integrity 不动。
- **inline handler 计数 pin**（`InlineHandlerSurfaceDoesNotGrow`）：迁移期间作为进度尺，落地后改 cap=0 守门。

### Rollback
- 因为是分阶段（§8），任一阶段是独立 commit，回退即 `git revert` 该 commit。
- 最终切换（删 `'unsafe-inline'`）是单点字符串改动 + 测试反转，回退只需把 CSP 串与两个 gate 测试还原；handler 迁移（addEventListener）本身向后兼容（在 `'unsafe-inline'` 仍存在时也工作），可先合、不影响功能，降低最终切换的爆炸半径。
- rollout 期可用 `Content-Security-Policy-Report-Only` 并行投放目标 CSP（见 §6/§8），生产仍跑旧 enforce CSP，零功能风险地收集 violation。

---

## 6. Observability
- **过渡期上报**：临时加 `Content-Security-Policy-Report-Only` 头（携带目标"无 unsafe-inline"策略）+ 一个轻量 `/csp-report` 接收端点（仅 server 侧 `slog.Warn("csp_violation", "directive", …, "blocked", …)`，不落盘、不新增 on-disk 格式），用于在真删之前定位遗漏的内联面。切换完成后**移除** report-only 与端点（non-goal：不做长期上报基础设施）。
- **日志**：`buildDashboardCSP()` 的 `init()` 自检失败 → 启动 panic（高可见）；运行期无新增常态日志。
- **无新增 metric / dashboard**：CSP 是静态头，无运行期指标价值；若 report-only 端点保留期内可临时数 violation 计数，切换后删除。

---

## 7. Compatibility & migration
- **向后兼容**：CSP 头收紧只影响浏览器对页面资源的放行，不改任何 API/线协议。已登录会话、cookie、鉴权流（`IsSecure` 等）不受影响。
- **On-disk 格式**：无变更。dashboard.html/js 仍是 embed.FS 静态文件；不引入新文件格式、不迁移任何 schema。
- **Config flag**：rollout 期可加一个**仅过渡用**的 env/flag（如 `NZ_DASHBOARD_CSP_REPORT_ONLY=1`）切到 report-only 投放，便于灰度观察；最终切换后移除该 flag（不作为长期配置面）。
- **缓存**：sw.js / ETag 不变（方案 A 的 CSP 仍静态）。若 theme-boot 改为外置 `/static/theme-boot.js`（备选），需在 service worker 缓存清单纳入——倾向 hash 内联以避免触动 sw 缓存。
- **迁移路径**：handler 迁移（addEventListener）可独立、增量、在 `'unsafe-inline'` 仍在时无害落地，逐批清空内联面，最后一步才反转 CSP——见 §8。

---

## 8. Rollout plan

分阶段、最终单点切换。**本轮（this round）不落地任何生产代码（hasLandablePhase1=false）**：本 RFC 牵动 CSP + 前端 handler 重构 + 鉴权页先例复用 + 与 ETag 缓存的交互，属 triage 明确划归"需先评审"的联动改动；且最小有意义的一步（迁 ~84+8 处 handler 之首批）虽可逐步，但其设计（事件委托 schema、`data-action` 约定、是否提取 `csputil` 共享包）必须先评审定档，避免引入第二套与登录页不一致的模式。

### Phase 0（本轮产出）
本 RFC + 评审。确认方案 A（hash + 全外置 handler，不用 nonce/strict-dynamic）为主路径，确认 theme-boot 用 hash 内联、handler 用事件委托、`csputil` 是否从 `auth` 抽取共享。

### Phase 1（评审通过后，第一个可落地 PR——非本轮）
**纯前端、不动 CSP 头、不删 `'unsafe-inline'`**，因此对线上零功能风险、独立安全可合：
- 在 dashboard.js 顶层装事件委托：容器级 `click`/`submit` 监听，按元素 `data-action` 属性派发到现有函数。
- 把 dashboard.html 的 8 处 inline `on*=` 改为 `data-action`（保留按钮其余属性不变）。
- 给 dashboard.js 中**一批**生成 HTML 的 `onclick=` 改为 `data-action`（可拆多个 PR，本 Phase 取第一批，例如头部/侧栏/cron 面板相关，约 1/3）。
- 此阶段 `'unsafe-inline'` 仍在，所有交互在新旧两种绑定下都工作（向后兼容）。

文件级改动清单（Phase 1，估算）：
- `internal/server/static/dashboard.html`：8 处 `on*=`→`data-action`，约 10 行改动。
- `internal/server/static/dashboard.js`：新增委托派发器（约 40-60 行）+ 第一批 `onclick=`→`data-action` 替换（约 30-50 行）。
- 测试：扩展/更新 `TestDashboardCSP_InlineHandlerSurfaceDoesNotGrow` 把 HTML cap 7→0 并加 dashboard.js 扫描（约 30 行测试）。
- **预估生产代码 ~120-150 行**（前端 JS/HTML 为主，无 Go 生产逻辑改动）。

> 注：因 triage 已判 high risk（前端+CSP+鉴权联动），且首个 PR 仍是 ~150 行前端重构 + 改既有 pin 测试方向，**本轮不写代码**；上面清单是评审通过后 Phase 1 的执行蓝图，不是本轮交付。

### Phase 2
迁完 dashboard.js 剩余 `onclick=`（多 PR），内联 handler 计数清零。其间可选启用 `Content-Security-Policy-Report-Only`（§6）投放目标策略，确认零 violation。

### Phase 3（atomic cut，单点切换）
同一 PR 内：
- `routes.go:522`：`script-src`/`style-src` 删 `'unsafe-inline'`，加 theme-boot/`<style>` 块的 `'sha256-…'`；如需容忍残留 inline `style=`，加 `style-src-attr 'unsafe-inline'`（方案 C 补充点）或先把 11 处 `style=` 迁 class。把 CSP 常量化为 `buildDashboardCSP()` + `init()` 自检（仿登录页）。
- 反转 `TestDashboardCSP_ScriptSrcUnsafeInlineMigrationGate`（断言**不含** `'unsafe-inline'`）。
- 移除 report-only 头与临时端点/flag。
- 全套 E2E + console violation 断言通过后合并、按部署 playbook restart 验证。

---

## 附：Phase 1 可落地性结论
**hasLandablePhase1 = false**。理由：本主题是 CSP + 前端 handler 大面积重构（~84+8 处）+ 鉴权页 hash 先例复用 + 与 embed.FS/ETag 缓存的交互的联动设计，triage 已判 high risk 且"NO phase-1 code this round — pure RFC"。即便第一步（事件委托 + 首批 handler 迁移）技术上可独立合，它仍是 ~120-150 行前端改动并需反转既有 pin 测试方向，依赖评审先确认事件委托约定与 `csputil` 抽取决策，故本轮只产出 RFC，不落代码。
