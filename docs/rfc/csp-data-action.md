# RFC: inline handler → data-action 委托，去 CSP `unsafe-inline`（#1980 D5）

（v2，按两轮对抗评审修订：删除幻影 onerror/onload 需求、bubble 相位、命名空间
碰撞处置、srcdoc/blob 预览回归、登录页 hash 模式复用、ratchet 口径修正。）

## 问题

`routes.go:319` CSP `script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net/npm/`。
拦路的是 JS 拼的 HTML 里 **107 处** `on*="…"` 内联事件属性（实测：onclick 79、
onkeydown 11、onchange 8、oninput 2、oncompositionend 2、drag 系 5；onsubmit
已于 #922 迁完；**onerror/onload 全部是 property 赋值形态**（`img.onerror = fn`，
dashboard.js:15198/15227-15253、10343/10416，CSP 合法，本 RFC 不碰——lightbox
fallback 链依赖置 null 手动解绑且 `new Image()` 是 detached 节点，委托收不到，
改了必回归）。dashboard.html 静态 on* 属性为 0（#922，cap=0 已 pin）；唯一内联
`<script>` 是主题引导（防 FOUC）。

传参形态（实测）：无参 37、`this.dataset` 23、`event` 19、`this`+`event` 2、
字符串插值参数 26（全部在 cron_view.js，参数为 crypto-hex job/run id 与
工作区路径；`escJs(id)` 拼 JS 字面量——本 RFC 消灭这一整类转义面）。

现存 data-action 基建（**必须吞并而非并存**）：
- `SIDEBAR_PROJECT_ACTIONS` + `#session-list` 范围 bubble 委托（dashboard.js:1221-1246）
- `CRON_MENU_ACTIONS`（cron_view.js:1343-1361，key 是 run/open/edit/delete 通用词）
- tuning chips document 委托（dashboard.js:3344）
- modal `[data-action="modal-close"]` 逐个 bind（dashboard.js:1369-1511）

`dashboard_csp_test.go` 已有迁移基建：bundle 总量 ratchet
`generatedOnclickCap = 83`（onclick 文本计数，6 文件求和，只降不升）+
`TestDashboardCSP_ScriptSrcUnsafeInlineMigrationGate`（nonce/strict-dynamic 与
unsafe-inline 互斥的原子翻转门）。

## 方案

### 1. `nz.actions` 注册表 + 分发器（nz_util.js）

```js
export const nzActions = Object.create(null);   // 无原型：防 __proto__ 取值
nz.actions = nzActions;
export function registerActions(map) {
  for (const k of Object.keys(map)) {
    if (!/^[a-z][a-z0-9-]*$/.test(k)) throw new Error('bad action key: ' + k);
    if (k in nzActions) throw new Error('duplicate action key: ' + k);
    nzActions[k] = map[k];
  }
}
const DELEGATED = ['click','keydown','change','input','compositionend',
                   'dragstart','dragover','dragleave','drop','dragend'];
for (const type of DELEGATED) {
  document.addEventListener(type, (e) => {
    const attr = type === 'click' ? 'data-action' : 'data-action-' + type;
    const el = e.target instanceof Element ? e.target.closest('[' + attr + ']') : null;
    if (!el) return;
    const fn = nzActions[el.getAttribute(attr)];
    if (typeof fn === 'function') fn(el, e);
  });   // bubble 相位（评审 2a：capture 会打穿 #session-list 的
        // long-press click 吞噬器 dashboard.js:14492 —— 长按误选会话）
}
```

- **bubble 相位**是硬约束：capture 在 document 先于一切既有 stopPropagation
  屏蔽执行。原 `onclick="event.stopPropagation();fn()"` 在 target 相位执行、
  事件到不了 document 级关闭器；委托后 handler 在 document 执行，
  `e.stopPropagation()` 不再有同等效果（同节点监听器不受其影响）。仓库有
  8 个 document 级 click 关闭器（dashboard.js:1996/3344/3507/6598/15390/16253、
  cron_view.js:1407、agent_view.js:645），各有自保（延迟注册 / mousedown
  provenance / closest 检查）——**每个转换 PR 必须对涉及 stopPropagation 的
  handler 逐个做关闭器 provenance 审计**，不是一句"逐处核对"。
- **命名空间吞并**：PR-1 把 CRON_MENU_ACTIONS（改带 `cron-menu-` 前缀避免
  通用词碰撞）、PR-2 把 SIDEBAR_PROJECT_ACTIONS / tuning chips / modal binds
  并入 nzActions，各自的局部监听器删除；`data-action` 属性全仓单一分发器
  消费，key 全局唯一由 registerActions 抛错保证。
- 分发器在 nz_util（被所有 module import，最先执行）注册；委托对元素创建
  时机免疫。静态 HTML 今后若引入带 on* 需求的资源元素须重新评估（今天为 0）。

### 2. 107 处逐类转换

- 无参（37）：`onclick="fn()"` → `data-action="fn-key"`。
- `this.dataset`（23）：handler 收 `(el, e)`，读 `el.dataset.*`——纯搬运。
- `event`（19+2）：第二参；`event.stopPropagation()` 前缀按上节审计逐处处理。
- 插值参数（26，全在 cron）：`onclick="fn(\'' + escJs(id) + '\')"` →
  `data-action="fn-key" data-id="' + escAttr(id) + '"`。**转义面收窄**：JS
  字面量上下文整类消失。硬规则：(1) `data-action` 值必须是代码字面量，静态
  ratchet 禁止 `data-action="' +` 拼接形态；(2) 插值属性一律双引号包裹
  （escAttr 不防无引号属性上下文）。
- `oncompositionend="lastCompositionEnd=Date.now()"`（2 处）：action 在
  dashboard module 作用域赋值；其 window accessor（dashboard.js 桥块）供
  e2e 探针读取，#2557 PR-E 收桥时保留此名。
- 破坏性 action（cronDelete / cronTriggerNow / sendMessage / dismissSession
  等）保持既有二次确认语义，不因迁移弱化。
- `querySelector('button[onclick="openFilePicker()"]')` 形态的选择器耦合：
  dashboard.js:2352 + e2e 5 处（multibackend round2/3/4）随 PR-2 同步改为
  data-action 选择器。

### 3. CSP 收口（PR-3）

- 主题引导脚本 hash：**复用登录页既有模式**（auth/handlers.go:237-291——
  package init 时从 embed HTML 提取内联块现算 sha256，init 自检零命中即
  panic），不手抄 hash 字面量。hash 对原始字节算，与 embed+precompress
  无冲突（不引 nonce 的决定性理由：per-response 改写摧毁预压缩缓存）。
- **CSP2/3 地雷**：script-src 出现 `'sha256-…'` 后浏览器忽略 `'unsafe-inline'`
  （与 nonce 同坑）。PR-1 先给 `TestDashboardCSP_ScriptSrcUnsafeInlineMigrationGate`
  补 `'sha256-'` 共存禁令；hash 与 unsafe-inline 删除在 PR-3 原子落地。
  PR-3 须同步翻转两个 pin 旧值的断言：该 gate 的 invariant(1) 与
  `TestDashboardCSP_JsdelivrNpmPathScoped` 的字面 header。
- **jsdelivr `/npm/` 通配收紧**（安全评审第 3 点：任何人可发 npm 包 =
  host-source 绕过面）：PR-3 把 script-src 收到精确版本 URL
  （mermaid@x/dist/mermaid.min.js、katex@x/dist/katex.min.js），style/font
  同理；懒加载是 script-inserted `<script src>` + SRI，host-source 匹配不区分
  插入方式，不受影响。`require-sri-for` 是无浏览器实现的已撤回草案（no-op），
  保留不依赖。
- 最终 `script-src 'self' 'sha256-<init 计算>' <两个精确 cdn URL>`。
  `style-src 'unsafe-inline'` 不在本 RFC 范围（D6 #2559 style= 迁 class 后处理）。

### 4. workspace HTML 预览回归（PR-3 前置调查，评审最高危项）

dashboard.js:11281-11350 文件预览。**已实测（Playwright/Chromium，本 RFC
gate 完成）**：父页 `script-src 'self'`（无 unsafe-inline）下，blob: 与
srcdoc iframe 内联脚本**均被拦**（对照组带 unsafe-inline 均执行）——
dashboard.js:11293 "blob 无继承" 注释对现行 Chromium 不成立，两条路径都会
随 PR-3 翻转而回归。

定案方案：预览改走服务端渲染响应，iframe src 直指端点，document 策略来自
**自身响应头**（网络响应不继承父页 CSP）：
- `serveRender` 已备有完整头组合（`CSP: default-src 'none'; sandbox
  allow-scripts; script-src 'unsafe-inline' …`、CORP、no-store），只因
  Firefox 直航忽略 CSP sandbox 而刻意 octet-stream+attachment。
- 新增渲染变体：`Sec-Fetch-Dest: iframe` 请求头门禁（现代浏览器导航请求
  必带；顶层直航是 `document` → 拒绝，fail-closed），通过后以 text/html +
  上述 sandbox CSP 返回。iframe sandbox 属性保留（双保险）。
- 前端 renderSandboxedBlob 的 blob/srcdoc 双路径统一替换为端点 src——顺带
  修复 WebKit blob 空白帧 bug 与 srcdoc 的 UTF-8/SVG 局限（注释自认的
  trade-off 全部消失）。

### 5. ratchet / 测试口径

- `generatedOnclickCap` 是 bundle 总量文本计数（含 `const onclick =`、
  `btn.onclick =` 等非属性形态 3 处）。转换 PR 逐步下调；归零目标改为：
  新增**属性形态守门** regex `on[a-z]+\s*=\s*["']`（全 on* 类型，不只
  onclick）计数为 0，旧 onclick cap 降到 3（非属性形态残留）后冻结并注明。
- mock-server 补发与 routes.go 单源/防漂移的 CSP header（形态：contract.js
  生成或 Go 侧导出 + drift 测试），否则 287 条 e2e 从未在 CSP 下跑过。
- 负向探针（PR-3）：页面挂 `securitypolicyviolation` 监听；注入
  `<button onclick>`（点击）、内联 `<script>`；断言 `__pwn*` 均 undefined
  **且**收到 script-src-attr / script-src-elem violation（双断言防 header
  未发出的假绿）。

### 6. PR 切分（每 PR Playwright 全绿 + 三闸门）

1. **PR-1**：分发器 + registerActions + 迁移门补 sha256 禁令 + cron_view
   52 处 + CRON_MENU_ACTIONS 吞并（cron-menu- 前缀）+ onclick cap 下调。
2. **PR-2**：dashboard 55 处 + SIDEBAR_PROJECT_ACTIONS / tuning chips /
   modal binds 吞并 + 选择器耦合修复（dashboard.js:2352 + e2e ×5）+ cap 下调。
3. **PR-3**：预览回归调查结论落地 + 主题 hash（init 模式）+ 删
   unsafe-inline + jsdelivr 精确化 + 两个断言翻转 + mock-server CSP header +
   负向探针 + 属性形态守门归零。

## 替代方案

- nonce + strict-dynamic：per-request HTML 改写与 embed+precompress 冲突；
  事件属性 nonce 也不覆盖，107 处转换省不掉。
- DOMPurify：引运行时依赖，且不治事件属性。
- unsafe-hashes：107 个属性哈希清单不可维护。

## 验收

- 属性形态 `on[a-z]+=["']` 在 static/*.js 生成串与 dashboard.html 中为 0
  （新守门测试，全类型）；`escJs(` 调用点为 0 且 nz_util 导出与 window 别名
  删除（与 #2557 PR-E 协调）；
- CSP `script-src` 无 `unsafe-inline`、jsdelivr 收精确 URL（header 契约测试
  逐字 pin，hash 由 init 计算非手抄）；
- data-action 值全部代码字面量（拼接形态 ratchet 0）；registerActions key
  唯一性 + 无原型注册表有单测；
- workspace HTML 预览在新 CSP 下实测无回归（或按 §4 方案迁移后无回归）；
- Playwright 全绿 + 负向探针双断言。
