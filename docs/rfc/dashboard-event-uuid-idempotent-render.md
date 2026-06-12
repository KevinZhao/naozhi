---
title: Dashboard 事件渲染 uuid 幂等（消除重启后 user 消息重复气泡）
status: Draft v1 (2026-06-12)
author: Kevin
---

# RFC: Dashboard 事件渲染 uuid 幂等

## 背景

upgrade 触发 systemd 重启后，dashboard 会把同一条 user 消息显示成**两条相同气泡**（中间无 bot 回复）。

### 根因（已实测坐实）

- 后端事件日志只落了**一条** user entry。复现样本 `~/.naozhi/events/e3a2b23251c62e2954b0213066a884dc.log` 中 `"升级到最新版本"` 的 user entry 仅 `seq:1`，uuid `e989da1a...`，唯一。
- 所以**不是后端重复 dispatch**（`platform.Dedup` 那条链对 dashboard 不适用——dashboard 走 WS subscribe + 事件回放，不经过 `internal/platform/dedup.go`）。**纯前端把同一条 user 事件渲染了两遍**。

前端渲染 user 消息有三条互不去重的路径：

1. **乐观气泡**（`dashboard.js:3919-3940`，`onSend`）——本地即时造，标 `optimistic-msg` class，**无 uuid**。
2. **实时推送 `onEvent`**（`dashboard.js:10658-10696`）——eventPushLoop 推来真实 user 事件时渲染一条，并移除乐观气泡。**但不更新 `lastRenderedEventTime` 游标**。
3. **历史回放 `onHistory`**（`dashboard.js:10516-10543`）——重订阅时回放历史，仅靠 `e.time <= lastRenderedEventTime` 时间游标去重。

重启触发序列：

1. 进程 dead → revived，`onSessionState`（`dashboard.js:10841-10850`，case 2 `wasDead && !msg.reason`）触发重订阅，置 `wsm.lastEventTimeWs = 0` 后 `subscribe`。
2. 历史回放里那条 user 事件再次到达。
3. 此时乐观气泡已被步骤 2 的 `onEvent` 移除（移除只发生一次），且 `onEvent` 未推进 `lastRenderedEventTime`，时间游标拦不住 → 同一 uuid 的 user 事件被画第二遍。

### 关键事实

- 后端 entry 自带稳定 uuid：`internal/cli/event.go:346` 的 `UUID` 字段，由 `internal/cli/uuid.go` crypto/rand 生成，注释明确称其为 entry 的 "authoritative identity"。
- WS 帧透传 entry 原文：`internal/eventlog/schema/record.go:73` 的 `Entry json.RawMessage` 原样下发，**前端 event 对象本就携带 uuid**。
- 但 `eventHtml`（`dashboard.js:3464`）**根本不渲染 uuid**——前端拿到了权威身份却没用它去重。

这就是本 RFC 要补的 gap：让 uuid 成为前端渲染的幂等键。

## 目标 / 非目标

**目标**
- 同一 uuid 的事件无论经由乐观气泡、`onEvent` 实时推送、`onHistory` 历史回放到达多少次，DOM 里**至多一条**。
- 治本，覆盖所有重订阅/回放叠加场景（重启、dead→revived、suspended→running、网络抖动重连），不止当前这一个触发器。
- 乐观气泡能被真实事件**原地替换**而非"先移除靠时序对账"。

**非目标**
- 不改后端 dispatch / `platform.Dedup` / 事件落盘格式（uuid 已存在，无需新增字段）。
- 不改 cron-live 那套独立的 `cronLive.*` 订阅状态机（它有自己的 `lastEventTimeMs` 接续逻辑；本 RFC 仅在其复用 `eventHtml` 处顺带受益，不改其流程）。
- 不重写 `lastRenderedEventTime` / 乐观气泡的整体生命周期——只在其上叠加 uuid 幂等作为权威去重层。

## 整体架构

引入一个**渲染期 uuid 幂等层**：所有把事件写进 `#events-scroll` 的入口，渲染前先查该 uuid 是否已在 DOM；已存在则跳过（或原地更新）。

幂等键来源优先级：
1. `e.uuid`（后端权威 id，绝大多数事件都有）。
2. 无 uuid 的事件（乐观气泡、个别 CLI 合成事件）走旧路径，不参与 uuid 去重——见"乐观气泡对账"。

DOM 是唯一真相源（single source of truth）：用 `data-uuid` 属性把 uuid 写到每个 `.event` 元素上，去重查询直接 `el.querySelector('[data-uuid="..."]')`。**不引入独立的 JS Set**——独立 Set 会与 `trimEventsScroll`（DOM 上限裁剪 #398）、session 切换、`innerHTML` 全量重渲产生第二真相源，正是当前 bug 的同类病根。DOM 裁掉的元素其 uuid 自然从去重集合消失，语义自洽。

## 前端契约

### 1. `eventHtml` 输出 `data-uuid`

`eventHtml`（`dashboard.js:3464`）在最外层 `.event` 元素上输出 `data-uuid="<esc(e.uuid)>"`。

- uuid 来自 crypto/rand hex，但仍过 `esc()`，保持"渲染入口一律转义"的不变量（防御未来 uuid 源变化）。
- `e.uuid` 缺失时不输出该属性（属性缺失 ≠ `data-uuid=""`，便于区分"无 uuid 事件"与"uuid 为空串"）。

### 2. 统一去重 helper

```js
// 已在 DOM 中存在该 uuid 的事件元素？
function eventAlreadyRendered(scrollEl, uuid) {
  if (!uuid) return false;               // 无 uuid → 不参与 uuid 去重
  return !!scrollEl.querySelector('.event[data-uuid="' + cssEscape(uuid) + '"]');
}
```

- `cssEscape`：uuid 是 hex，本无需转义，但用 `CSS.escape`（或等价 polyfill）守住选择器注入边界，与项目"边界一律防御"风格一致。

### 3. 去重只施加于 `user` 事件（实测收敛的关键决策）

实测样本 `~/.naozhi/events/420b3dc932107eabcdeb5215585316de.log`：`type:text` 共 8271 条、去重 uuid 7155 个，**586 个 uuid 重复出现**（同 uuid、同 time、内容相同——是流式重写/回放重复落盘机制，不是内容增长）。

结论：**通用"存在即跳过"会冻结流式 text 输出，绝不可行。** 而本 bug 的两条气泡只发生在 **user 事件**（乐观气泡移除后、`lastRenderedEventTime` 游标未推进时漏网）。user 事件内容不可变、不参与流式重写。

因此 uuid 去重**只施加于 `ev.type === 'user'`**：

| 入口 | 当前行为 | 改造 |
|---|---|---|
| `onEvent`（10658） | 直接 append，user 事件移除乐观气泡 | **仅当 `ev.type === 'user'`** 时 append 前 `if (eventAlreadyRendered(el, ev.uuid)) return;`；并推进 `lastRenderedEventTime`（修游标不前进的 bug，双保险）。非 user 类型完全不变 |
| `onHistory` 增量分支（10526） | 仅 `e.time <= lastRenderedEventTime` 去重 | 循环内**仅对 `e.type === 'user'`** 加 `if (eventAlreadyRendered(el, e.uuid)) { ...认领乐观气泡...; return; }`。非 user 类型维持 `lastRenderedEventTime` 时间游标去重不动 |
| `onHistory` initial 分支（10468） | `el.innerHTML = html` 全量替换 | initial 全量重建天然无重复，无需查重；`renderEventsWithDividers` 产出的 HTML 已带 `data-uuid`，重建后 DOM 即携带去重所需属性 |

非 user 类型（text/tool_use/tool_result/todo/...）**完全不接入 uuid 去重**，行为零变化——这把回归面压到最小，且 `data-uuid` 属性对它们只是惰性附带（未来若需对 tool 类做原地更新可再扩展，不在本 RFC 范围）。

### 4. 乐观气泡对账（原地替换）

当前"乐观气泡无 uuid，真实 user 事件到达时 `querySelector('.optimistic-msg')` 移除"是脆弱的——移除与渲染分两步、靠时序，重订阅回放时第二条就漏网。

改为**原地认领（claim）**：
- 真实 user 事件在 `onEvent` / `onHistory` 到达时，若 DOM 里存在 `.optimistic-msg` 且其文本与该事件 `detail` 一致，则**给这条乐观气泡补写 `data-uuid` 并去掉 `optimistic-msg` class**（认领为正式气泡），而非"删旧 + 画新"。
- 认领后，后续同 uuid 的回放被 `eventAlreadyRendered` 直接拦下。
- 找不到匹配乐观气泡（如重订阅时乐观气泡所在 DOM 已被 `innerHTML` 重建清掉）则走正常 append + uuid 去重。

文本匹配仅作"认领哪条乐观气泡"的弱关联；**去重权威键始终是 uuid**，文本不一致也不会重复（uuid 去重兜底）。

## 后端契约

**无改动。** uuid 已在 entry 中（`cli/event.go:346`）并经 `record.go:73` 的 `json.RawMessage` 透传至前端。本 RFC 是纯前端修复。

需在实现期 grep 复核一处：确认所有进入 `#events-scroll` 的事件其 `uuid` 字段在前端 event 对象上确实可读（`processEventsForDisplay` 不会剥离 uuid）。`processEventsForDisplay`（`dashboard.js:9517`）若做了字段裁剪需保留 uuid。

## 实现清单

1. `eventHtml` 最外层 `.event` 输出 `data-uuid`（缺失则不输出）。
2. 新增 `eventAlreadyRendered(scrollEl, uuid)` + `cssEscape` 守卫。
3. `onEvent`：append 前查重；补推进 `lastRenderedEventTime`。
4. `onHistory` 增量分支：循环内 per-event 查重。
5. 乐观气泡改"认领"语义：`onSend` 仍造无 uuid 的 `optimistic-msg`；真实事件到达时原地补 `data-uuid` + 去 class。
6. 复核 `processEventsForDisplay` 保留 uuid 字段。
7. 复核 cron-live 复用 `eventHtml` 的渲染处（`dashboard.js:11198` 等）不因新增 `data-uuid` 属性回归（它有独立去重，新增属性应无害）。

## 测试计划（CI 必须，对应 testing 规则）

复现型用例（核心，直接锁死本 bug）：
- **T1 重订阅回放不重复**：乐观气泡 → `onEvent` 渲染真实 user 事件（uuid=U）→ 模拟 dead→revived 重订阅 → `onHistory` 回放含 uuid=U 的历史 → 断言 `#events-scroll` 内 `[data-uuid="U"]` 的 `.event.user` **恰好 1 个**。这是当前线上 bug 的精确回归用例。
- **T2 实时+历史交错**：`onEvent` 推 U1，`onHistory` 回放 [U1,U2] → 断言 U1 不重复、U2 出现一次。

幂等层单元用例：
- **T3** `eventAlreadyRendered`：DOM 有/无该 uuid、空 uuid、含特殊字符 uuid（CSS.escape 边界）。
- **T4 乐观气泡认领**：发送造 `optimistic-msg`（无 uuid）→ 真实事件到达 → 断言同一 DOM 节点被补 `data-uuid` 且 `optimistic-msg` class 移除，节点总数不变（认领而非新增）。
- **T5 无 uuid 事件**：CLI 合成事件无 uuid → 不参与 uuid 去重、正常渲染（不被误吞）。

回归保护：
- **T6** initial `onHistory` 全量重建后每个 `.event` 带 `data-uuid`（除无 uuid 事件）。
- **T7** `trimEventsScroll` 裁剪后被裁元素 uuid 不再命中 `eventAlreadyRendered`（DOM 即真相源，无悬挂 Set）。

测试形态沿用 dashboard 现有前端测试套路（DOM 断言 / jsdom 或 e2e）。实现期确认项目里 dashboard JS 的既有测试入口（`internal/server/static_*_test.go` 契约测试 / playwright e2e）并对齐。

## 风险与未决

- **R1 `processEventsForDisplay` 可能丢 uuid**：若该函数对事件做了重组/合并（如把多个 chunk 合成一条 text），合成产物的 uuid 归属需定义。缓解：text/assistant 合并产物取首个 chunk 的 uuid（或最后一个，实现期定一致规则）；user 事件不参与合并，不受影响——而 bug 只发生在 user 事件，风险可控。
- **R2 同一 uuid 内容更新（已实测定案）**：实测确认流式 text 事件确实同 uuid 多次落盘（样本 586 个重复 uuid）。**定案：uuid 去重只施加于 `user` 事件**（见 §3），其余类型不接入，因此不存在"冻结流式输出"风险。无需 `eventAlreadyRendered` 后按类型分流——调用点本身就只在 user 分支调用它。
- **R3 cron-live 复用**：cron 面板复用 `eventHtml`，新增 `data-uuid` 属性预期无害（它走 `cronLive.*` 独立去重），但需 T7 类用例或手测确认无双重去重冲突。
- **R4 乐观气泡跨 session 残留**：认领逻辑需限定在当前 `selectedKey` 的 `#events-scroll` 内，避免误认领切走的会话的乐观气泡。`onEvent`/`onHistory` 开头已有 `msg.key !== selectedKey` 早退，沿用即可。

## 不做的事

- 不给前端引入全局 uuid Set / Map（第二真相源，与 DOM 裁剪/重建冲突，正是病根同类）。
- 不改后端事件格式、不新增字段。
- 不重写乐观气泡或 `lastRenderedEventTime` 整体机制——仅叠加 uuid 幂等作为权威层，并顺手修 `onEvent` 不推进游标的小 bug。
- 不处理飞书侧的"重启后重投递 + `platform.Dedup` 重启丢失"——那是**独立的另一个 bug**（同样由 `stop-sigterm timed out → SIGKILL` 触发，但机理不同），应单开 RFC/issue 跟踪，不在本 RFC 范围。
