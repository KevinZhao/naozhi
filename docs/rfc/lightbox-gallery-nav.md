# Lightbox 多图导航（gallery navigation）

状态：Draft v2（架构 + 前端双评审 approve-with-changes 后修订；blocking 全部吸收）
日期：2026-06-10
范围：dashboard 前端（`internal/server/static/dashboard.js` / `dashboard.html`），零后端改动。

## 1. 背景与问题

一条消息携带多张图片时，`eventHtml` 把它们渲染进同一个 `.event-images` 容器（dashboard.js:3616）。点开任意一张走 `openLightbox(full, thumb)`（dashboard.js:12284），lightbox 一次只装一张图：

- 看完第一张想看第二张，必须 Esc 关闭 → 回到消息 → 再点下一张缩略图。多图对比（如 UI 截图序列）体验割裂。
- 无任何位置指示（不知道当前是第几张、一共几张）。

「显示尽可能清晰/原图」这半句需求**现状已满足**：缩略图点开即加载 `/api/sessions/attachment` 原图（GC 过期自动回落缩略图，RFC attachment-refcount §3.6.3），缩放（滚轮/双指/双击/`+`/`-`）也已存在。本 RFC 只补缺失的**组内导航**，并顺手在工具栏补一对放大/缩小按钮（当前缩放只有快捷键和滚轮入口，触屏外接鼠标无滚轮场景缺可点击入口）。

## 2. 目标与非目标

**目标**
- 同一 `.event-images` 组内：左右切换（按钮 + ←/→ 键盘 + 移动端未缩放时水平 swipe）。
- 位置计数器 `n / N`（组内多于 1 张时显示，`aria-live="polite"`）。
- 切换时预加载导航方向上的相邻原图，导航不白屏。
- 工具栏新增 `+` / `−` 缩放按钮（与快捷键同步 hint）。
- `window.openLightbox(src, fallback)` 签名保持兼容（agent_view / cron_view 经 `window.eventHtml` 复用同一渲染，自动获得新行为；其他潜在调用方不破坏）。
- 缩略图点击从内联 `onclick` 迁移到委托监听（消除对 CSP `'unsafe-inline'` 的增量依赖，与 `data-lb-action` 分发风格一致）。

**非目标**
- 不跨消息/跨 event 串图（只在单条消息的图片组内导航）。
- 不做缩略图 filmstrip、不做幻灯片自动播放。
- 不改后端 attachment API、不改 EventEntry 结构。
- 不改飞书端。
- 不补全历史欠账的全部内联 onclick（仅迁移本功能触达的图片点击一处）。

## 3. 方案与备选

### 选定方案：点击时从 DOM 收集组（字符串快照）

缩略图渲染保留 `data-full` / `data-thumb`（均经 `escAttr`），**移除**内联 `onclick`，改为 lightbox IIFE 内注册一个 document 级委托监听：

```js
document.addEventListener('click', function(e){
  var t = e.target && e.target.closest && e.target.closest('.event-images img[data-full]');
  if (t) openLightboxFromThumb(t);
});
```

委托在 document 上注册一次，天然兼容轮询 `innerHTML` 重渲染（`.event-images` 节点反复销毁重建）。

```js
function openLightboxFromThumb(el){
  var box = el.closest('.event-images');
  var imgs = box ? Array.prototype.slice.call(box.querySelectorAll('img[data-full]')) : [el];
  // 点击瞬间把 dataset 字符串拷贝成快照；之后消息列表重渲染（innerHTML 整段重建）
  // 也不影响已打开的 lightbox —— items 不持有 DOM 引用。
  // indexOf 找不到（理论上不可能：el 必带 data-full）时兜底第 0 张。
  var items = imgs.map(function(i){ return {full: i.dataset.full, thumb: i.dataset.thumb}; });
  openLightboxGroup(items, Math.max(0, imgs.indexOf(el)));
}
```

lightbox IIFE 内部改造为组模型：

```js
var items=[], idx=0;
// loadWithFallback(item)：现有 onerror + naturalWidth===0 双模检测逻辑原样抽出；
// 主源 item.full，回落源 item.thumb（与现状 openLightbox(src, fallback) 的
// fallback === 缩略图 data URI 语义一一对应）。
function show(i, dir){ idx=i; reset(); loadWithFallback(items[idx]); updateNav(); preload(idx+(dir||1)); }
window.openLightboxGroup = function(list, start){ items=list; show(start); ov.classList.add('active'); /* focus 管理见下 */ };
window.openLightbox = function(src, fallback){ openLightboxGroup([{full:src, thumb:fallback}], 0); };  // 兼容壳
```

**导航入口**
- 按钮：`.lb-nav.lb-nav-prev` / `.lb-nav.lb-nav-next`，复用 `data-lb-action` 分发（点击不冒泡到 backdrop close）。首尾不环绕：到边界时设 `aria-disabled="true"` + 降透明度（不用 HTML `disabled`，保持可被读屏感知）。
- 键盘：←/→，挂在现有 keydown handler 中、editable-element 跳过逻辑之后，与 `r`/`+`/`-`/`0` 并列。
- 移动端 swipe（**触摸生命周期规约**，吸收评审 B1/B2/N1）：
  - `touchstart`（单指）：记录 `sx, sy`，并捕获 `swipeScale = scale`（**以手势开始时刻为准**——pinch 结束后 scale 是 0.5~10 间任意浮点，不能在 touchend 时再读）；置 `pinched = false`。
  - `touchstart`（双指）：`pinched = true`（本轮手势让位给捏合缩放）。
  - `touchend`：若 `!pinched && swipeScale < 1.05 && |dx| > 50 && |dx| > |dy| * 1.2` → 判定为导航 swipe：执行切图、**置 `lastTap = 0`**（抑制连续 swipe 误触 double-tap 放大）、置 `swipeHandled` 标志并在后续 click 事件中吞掉一次 backdrop close（swipe 出界到 ov 上时浏览器可能仍合成 click）。否则走原有 double-tap 检测。
  - `scale > 1.05` 时单指仍是拖拽平移，互斥成立。
- 计数器 `.lb-counter` 置于左上角（工具栏在右上角），`aria-live="polite"`；`items.length <= 1` 时 overlay 加 `.lb-single`，CSS 隐藏导航按钮与计数器。

**CSS 规约**（吸收评审 blocking：尺寸/定位/层叠显式给出）
- `.lb-nav`：`position:absolute; top:50%; transform:translateY(-50%); width:44px; height:44px;` 圆形半透明（视觉风格复用 `.lb-tool-btn` 的 `rgba(0,0,0,.55)` 底 + 白描边），`z-index:1`（与 `.lb-toolbar` 同层，互不重叠：左右居中 vs 右上角）。prev `left:14px`、next `right:14px`。44px 直接满足移动端触摸目标，无需断点内特调。
- `.lb-counter`：`position:absolute; top:18px; left:18px;` 同 hint 的胶囊样式；`pointer-events:none`。
- `.lightbox-overlay.lb-single .lb-nav, .lightbox-overlay.lb-single .lb-counter { display:none }`。

**其他行为**
- 切图即 `reset()`（缩放/平移/**旋转**归零）。注：iOS Photos 保留用户旋转，本实现选择全归零——组内截图序列通常方向一致，归零代价低且实现简单；如有反馈再迭代。
- 预加载（吸收评审 B4/N3）：仅预载**导航方向**上的一张相邻原图（开组时预载 idx+1）；用 `Set` 记录已请求 URL 去重，快速翻页不产生重复 in-flight 请求；预载 `Image` 挂 `onerror = function(){}` 吞掉 404（GC 过期附件），避免触发 RNEW-UX-002 全局 error handler 误弹 toast。
- 焦点管理（吸收评审 N3）：overlay 设 `tabindex="-1"`；打开时记录 `document.activeElement` 并 `ov.focus()`，关闭时焦点还原。现状 lightbox 无任何焦点管理，此为增量补齐。
- 同一时刻只有一个 lightbox 实例；`openLightboxGroup` 重入（已打开时再开）直接替换 `items` 并从新 start 显示——单实例模型下语义即"切换到新组"。

### 备选 A：渲染时把整组 URL JSON 塞进每个 img 的 data-group

每张图带全组数据，点击无需 DOM 查询。**否**：HTML 体积随 N² 膨胀（N 张图各带 N 项 JSON），且 escAttr 嵌套 JSON 易碎；DOM 收集法零冗余、对 agent_view 等复用方零侵入。

### 备选 B：全局收集页面上所有 .event-images 跨消息串图

类似聊天软件"查看聊天图片"。**否**：当前需求明确是"多张图片一起传入"的单消息场景；跨消息组动态变化（轮询重渲染）会让 idx 失效，复杂度不成比例。可作后续演进。

## 4. 测试策略

按 `static_ux_contract_test.go` 首部既定优先级（#388/TEST2：e2e 优先，source-grep 仅限无法行为化的不变量）：

**Playwright e2e（主体）**：新增 `test/e2e/lightbox_nav.test.js`，mock-server events 注入一条含 3 张 data-URI 图片的消息 + 一条单图消息；mock 增加 `/api/sessions/attachment` 返回真 PNG。场景：
1. 点击第 2 张缩略图 → overlay active、计数器显示 `2 / 3`、img.src 指向 attachment 原图 URL；
2. 点 next → `3 / 3` 且 next `aria-disabled="true"`；点 prev 两次 → `1 / 1` 边界 prev 禁用；
3. 键盘 ArrowRight/ArrowLeft 切图、Escape 关闭；
4. 单图消息点开 → 导航按钮与计数器不可见（`.lb-single`）；
5. 工具栏 `+`/`−` 按钮点击 → zoom hint 出现且百分比变化；
6. 桌面 + mobile-safari 两个 project 跑（playwright.config.js 既有矩阵）。

**契约测试（最小化）**：`TestDashboardJS_LightboxGalleryNav` 仅含：
- forbid-list：旧内联 `onclick="openLightbox(` 写法在 dashboard.js 中不复存在（防双轨）；
- 兼容壳：`window.openLightbox` 仍存在且委托 `openLightboxGroup`（≥2 witness：赋值语句 + 调用语句）。

**回归判定（先红后绿）**：e2e 场景 1-5 在旧代码上全部失败（无计数器/无导航按钮/无 openLightboxGroup）；契约 forbid-list 在旧代码上失败（旧写法存在）。

**手测（部署后）**：移动端真机 swipe 切换、双指捏合后单指拖拽不误触切换、pinch 后立即 swipe 不导航（swipeScale 判定）、GC 过期附件回落缩略图后导航仍可用。

## 5. 风险与回滚

- 纯前端增量，唯一行为变化点是缩略图点击路径与 lightbox IIFE。出问题 revert 单个 PR 即回到现状。
- 最大风险：触屏手势冲突。已在 §3 给出 touch 生命周期逐事件规约（swipeScale 捕获时机 / pinched 标志 / lastTap 清零 / swipeHandled 吞 click），e2e 跑 mobile-safari project，真机手测兜底。
- 预加载 404 噪声：onerror 吞掉，不触发全局 error toast（§3）。
- `window.openLightbox` 兼容壳保证任何遗漏调用方不破坏。

## 6. 可观测性

N/A — 纯展示层交互，无新增日志/指标；JS 异常由既有 RNEW-UX-002 全局 error handler 兜底上报 toast。

## 7. 兼容性与迁移

- EventEntry / attachment API / 事件日志格式零改动。
- `openLightbox(src, fallback)` 公开签名保留；`eventHtml` 由 dashboard.js 与 agent_view / cron_view 共享（window.eventHtml 单一来源），改一处全端生效。
- 旧事件（仅 data URI、无 image_paths）：data-full 即缩略图本身，导航照常工作。
- 内联 onclick → 委托监听：CSP 当前 `script-src` 含 `'unsafe-inline'`（routes.go:504），迁移不是行为必需而是方向正确——减少一处内联依赖，为将来收紧 CSP 铺路。

## 8. 上线计划

单 PR 全量切换，无 flag（纯前端、可单 PR revert、无数据迁移）。随下一次 `naozhi upgrade` 发布。

---

**设计评审记录（v1 → v2）**：`ecc:architect` 与 `ecc:typescript-reviewer` 并行评审，均 approve-with-changes。Blocking 共 6 项全部吸收：①契约测试违反 #388/TEST2 禁令 → 测试主体迁 Playwright e2e；②`.lb-nav`/`.lb-counter` CSS 规约缺失 → §3 补齐；③swipe 的 scale 须在 touchstart 捕获 + 容差 1.05；④swipe 与 double-tap 互斥（lastTap 清零）；⑤内联 onclick → document 委托；⑥预加载限界（方向性 + Set 去重 + onerror 吞 404）。非阻塞采纳：aria-live/aria-disabled、焦点管理、loadWithFallback 字段映射显式化、indexOf 兜底注释、旋转归零 trade-off 注记。轮询重渲染下字符串快照模型经 architect 核实成立（dashboard.js:2989 innerHTML 重建不影响已拷贝的 items）。
