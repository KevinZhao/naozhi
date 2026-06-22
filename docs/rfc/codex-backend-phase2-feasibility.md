# Codex Backend — Phase 2 Feature 可行性分析

> **状态**: 可行性分析（含 codex 0.141.0 实测探针，2026-06-21）；**embedded_context 已实现**（§6）；**§7/§8 二次实测后结论翻转**
> **前置**: `codex-backend.md` v2 + `codex-backend-validation.md`（Phase 1 已交付 PR #2216）
> **范围**: 三个 claude 专属 UX Feature 在 codex backend 的落地评估。
>
> **⚠️ 结论翻转（2026-06-21 二次深度实测）**：初判 passthrough=可行/askuser=阻塞。实测后**对调**——passthrough 的 `turn/steer` 在 Bedrock gpt-5.5 上 ACK 成功但模型不采纳（silent drop，比 Collect 更差）→**推迟**；askuser 的 `requestUserInput` schema 已完整捕获→**可实现**。详见 §7、§8。

---

## 0. 背景

Phase 1 后，codex 与 kiro 的 Feature map **逐位相同**（7 项），三个 `false` 是所有非-claude（JSON-RPC）后端的共同空缺，非 codex 独有：

| Feature | claude | kiro | codex | 本文结论 |
|---|---|---|---|---|
| askuser | ✅ | ❌ | ❌ | **HARD**（阻塞式 RPC，需 pending 表）→ 推迟 |
| passthrough | ✅ | ❌ | ❌ | **MODERATE**（实测原语齐备）→ 第二做 |
| embedded_context | ✅ | ❌ | ❌ | **EASY/MODERATE**（structured mention）→ 第一做 |
| image_input / mcp_http | ✅ | ✅ | ✅ | 已支持 |
| audio_input / mcp_sse | ✅ | ❌ | ❌ | 跨后端共同空缺，不在本文 |

翻 `true` 前必须实现底层管线，否则 dashboard 显示点了不工作的控件（`featureForCurrent(name)` 门控见 `internal/server/static/dashboard.js`）。

---

## 1. embedded_context（@file mention）— **EASY/MODERATE，第一做**

### claude 参考
- **引用式，naozhi 不读不内联文件**：`@path` token 原样混在 prompt 文本里传给 CLI，claude 自己解析。dashboard `dashboard.js:~3940` 仅是发送门控（不支持的后端 block），非 UI 控件。

### codex 机制（实测）
- **探针结果**：`codex exec "...@context.txt..."` —— codex **不原生内联** `@path`，而是**用 shell 工具**（`rg`/`sed`）agentic 地读文件后回答。答对了，但走 tool-use 路径，非 claude 式静态内联。
- **结构化 mention 可用**：`UserInput` 有 `mention` 变体 `{type:"mention", name, path}`（schema stable union，非 UNSTABLE）。codex 服务端解析路径（变体无 content 字段 → 服务端内联）。

### 落地方案
- **方案 A（引用式，零协议码）**：直接翻 `embedded_context=true`，靠 codex 的 shell 工具 agentic 读 `@path`。**风险**：仅在 agentic 模式（sandbox 允许读文件）下工作；纯对话/受限沙箱下 `@path` 无效 → 与 claude 的"任何模式都内联"语义不符。
- **方案 B（结构化 mention，~30-60 LoC，推荐）**：`WriteMessage`/`WriteUserMessageLocked` 解析 `@path` token，额外发一个 `{type:"mention", name, path}` UserInput 条目，codex 服务端解析。naozhi 仍不读文件。需确认 `path` 是绝对还是 cwd-相对。

### 估时 / 风险
- ~0.5–1 天（含一次实测确认 mention path 语义）。风险低，隔离在 `protocol_codex.go` WriteMessage + profile flag。

---

## 2. passthrough（/urgent 抢占 + 多消息并发）— **MODERATE，第二做**

### claude 参考
- 硬依赖 `SupportsReplay()`（`protocol.go:106`；`passthrough.go:55` 无则提前返回）。每个 `Send` 分配带 128-bit hex uuid 的 `sendSlot`（`passthrough.go:27`），claude 用 `--replay-user-messages` 回显 `{type:"user",isReplay:true,uuid}`，`handleReplayEventLocked`（`passthrough.go:356`）按 uuid 匹配 slot。`/urgent`→`priority:"now"`（`send.go:69`）。
- codex 当前 `SupportsReplay/SupportsPriority` 均 false → 落 Collect 模式。

### codex 机制（**已实测确认**）
- ✅ **`clientUserMessageId` → `item.clientId` 完整 round-trip**（探针：发 `clientUserMessageId="naozhi-slot-ABC123"`，`item/started type=userMessage` 回 `"clientId":"naozhi-slot-ABC123"` 逐字一致）。这正是 naozhi slot-matching 的前提。
- ✅ **`turn/steer` mid-turn 可用**（探针：turn 进行中发 `turn/steer` 带 `expectedTurnId`，返回成功 `{turnId}` 无错；`slot-1`+`slot-2` 两个 clientId 都回显）。
- ✅ `turn/started` 携带 `turnId`，供 steer 的 `expectedTurnId` 前置条件。

### 落地方案（**适配器，非重设计**）
1. `WriteUserMessageLocked` 把 slot uuid 写进 `clientUserMessageId`。
2. `ReadEvent` 拦截 `item/started type=userMessage`（当前被跳过），按 `item.clientId` 匹配 slot（类比 `handleReplayEventLocked`）。
3. 追踪活跃 `turnId`（来自 `turn/started`）；mid-turn 走 `turn/steer`，无活跃 turn 回退 `turn/start`。
4. 翻 `SupportsReplay`/`Caps.Replay`=true。

### 估时 / 风险
- ~3–4 天，新增 ~200-300 LoC + 测试。
- **风险**：① `expectedTurnId` 竞态（读 turnId 与发 steer 之间 turn 可能完成 → steer 报错，需回退）；② codex **无 `priority:"now"` 通道** → `/urgent` 语义变成"append 到当前 turn"而非 claude 的"抢占+重排"，产品行为不完全一致，可能需要把 `/urgent` 与通用多消息 passthrough 分开门控；③ 多 steer 是否**合并成一个 result**（claude 的 merged-replay sweep 依赖此）未实测，落地前需补一次 capture。

---

## 3. askuser（AskUserQuestion 卡片）— **HARD，推迟**

### claude 参考
- **观察式/fire-and-forget**：claude `-p` 自动 inject `is_error:true` tool_result ~3ms 后 turn 正常结束（`protocol_claude.go:502`）。naozhi 从不写 tool_result。用户答案作为**下一轮 user 消息**回流。RFC `docs/rfc/askuser-question.md`。Event 形状 `cli.AskQuestion`（`clievent/types.go`）。

### codex 机制
- `CodexProtocol.ReadEvent`（`protocol_codex.go:343`）当前**显式丢弃** `requestUserInput`（返回 nil 防 turn 挂起）。
- RFC §2.5 记 0.141 运行时观察到 `item/tool/requestUserInput` 反向请求，但**这是阻塞式 server→client 请求**（server 等响应），与 claude 的 fire-and-forget 根本不同。
- **未决**：`requestUserInput` 的请求+响应 schema 在当前 schema dump 中缺失（只见 `[UNSTABLE]` guardian + mcp-elicitation）。0.141 的方法名可能已 stale，落地前必须 live capture 真实请求体+响应体。

### 落地难点
- 阻塞式 RPC vs "答案作为下一轮消息"语义错配，会**重新引入** claude RFC 删掉的 pending-request + TTL 状态表：捕获 `requestId` → 持有到用户答 → 响应**那个特定 RPC**。若用户不答，codex turn 挂起到自身超时（比 claude 自动解析差），需 TTL 自动 decline。

### 估时 / 风险
- ~2–3 天**起**（若响应 schema 简单稳定），新增 ~150-250 LoC。**风险高**：schema 未验证 + 可能落到 `[UNSTABLE]` guardian 路径。**结论：推迟，先 live 捕获真实方法名+响应体再排期。**

---

## 4. 推荐顺序

1. **embedded_context**（~0.5-1d，EASY）—— 隔离、低风险，方案 B 结构化 mention。
2. **passthrough**（~3-4d，MODERATE）—— 实测原语齐备（clientId round-trip + steer），适配器复用现有 slot 机制；落地前补一次"多 steer 是否合并 result"的 capture。
3. **askuser**（推迟，HARD/部分 BLOCKED）—— 阻塞式 RPC 重引入 pending/TTL 机制，且请求/响应 schema 未验证；先 live 捕获再排期。

> 每个特性建议独立 PR（独立可回滚）。passthrough/embedded_context 的底层若做通用，等于同时惠及 kiro（同为非-replay 后端）。

## 5. 实测探针留痕（2026-06-21，codex 0.141 + gpt-5.5 @ Bedrock）
- embedded_context：见 §6 的扩充探针矩阵。
- passthrough：`clientUserMessageId` 逐字 round-trip 为 `item.clientId`；`turn/steer` mid-turn 成功返回 `{turnId}`，双 slot clientId 均回显。
- askuser：`requestUserInput` 响应 schema 未捕获（推迟前置任务）。

---

## 6. embedded_context 实现（2026-06-21，方案 A）

### 6.1 决策性探针矩阵

§1 的初判（"倾向结构化 mention"）被进一步探针**推翻**——结构化 mention 并不预内联：

| 探针 | 沙箱 | 结果 |
|---|---|---|
| 单独 `mention` UserInput（cwd-相对 path） | read-only | ❌ 未解析（"unknown"） |
| 单独 `mention`（绝对 path） | read-only | ❌ 未解析 |
| `text`+`text_elements`+`mention`（TUI 真实线格式） | read-only | ❌ "can't determine without reading the file" |
| **纯 `text` 含 `@path`** | workspace-write | ✅ codex 解析 `@path` 并发 `commandExecution` 读文件（agentic） |

**结论**：codex 的 `@path` 不是 claude 式静态内联，而是**靠 agentic shell 工具读取**，且只在沙箱允许读时生效。结构化 `mention` UserInput 在 0.141 下对内联无帮助（probe 全部未解析），故**不发** mention 条目——那是我自己探针无法证明有益的投机复杂度。

### 6.2 落地（最小正确改动）

- `internal/cli/backend/profile_codex.go`：`Features["embedded_context"] = true`，附诚实注释说明语义差异（agentic 读 vs 静态内联，取决于沙箱）。
- **无协议码改动**：`@path` 已随 `text` 透传进 `turn/start` 的 text UserInput（`CodexProtocol.WriteMessage` 原样写文本）。dashboard 的 `featureForCurrent('embedded_context')` 闸门（dashboard.js:4136）只要求"后端能从 prompt 内读文件路径"，codex 满足。
- 测试：`TestCodexProtocol_WriteMessage_AtMentionVerbatim`（`@path` 逐字进 text UserInput）+ profile_test 断言 `embedded_context=true` / askuser·passthrough 仍 false。

### 6.3 与 claude 的诚实差异

| | claude | codex |
|---|---|---|
| 机制 | CLI 静态内联文件内容进 prompt | agentic shell 工具读取 |
| 纯对话/read-only 沙箱 | ✅ 总能内联 | ⚠️ 读不到（需沙箱许可） |
| naozhi 侧代码 | 纯透传 | 纯透传（零文件读取，零新增安全面） |

弱于 claude 的静态保证，但匹配 dashboard 契约且零安全面。若未来要 claude 式强保证，可走方案 B（naozhi 服务端内联），但会引入路径穿越/大小限制/workspace confine 安全面，留作独立 RFC。

---

## 7. passthrough — **推迟（实测否定）**

§2 初判"原语齐备、适配器可行"。但 §2 只验证了 `clientUserMessageId` round-trip 与 `turn/steer` 的 **ACK**，没验证 steer 对模型输出的**实际效果**。2026-06-21 补做 3 组效果探针（Bedrock gpt-5.5）：

| 探针 | turn 内容 | steer 内容 | 结果 |
|---|---|---|---|
| 1 | "10 段罗马帝国史"（长输出） | "IGNORE…只输出 PIVOT 并停" | steer ACK 成功；**输出未 pivot**，继续写罗马史，35s 未完成 |
| 2 | "慢数 1-8" | "数完后说 PIVOT" | 单 turn 5.5s 完成；**无 PIVOT**，无第二 turn |
| 3 | "跑 sleep 8 的 bash 再报告"（tool 占用 9s） | turn 刚 started 即 steer "末尾加 PIVOT" | steer ACK 成功 `{turnId}`；turn 9.2s 完成；**无 PIVOT** |

**结论**：`turn/steer` 在 Bedrock gpt-5.5 上**返回成功但模型不采纳 steered 输入**——是 silent no-op。naozhi 若据此开 passthrough，会出现：用户发 `/urgent` 或并发消息 → naozhi ACK、round-trip slot、显示已发 → 但 codex 实际丢弃。**比现有 Collect 模式更差**（Collect 可靠排队+下一 turn 投递，不丢消息）。

**决策：不 ship。** `passthrough` 保持 false，codex 继续走 Collect 模式（可靠）。

未决/可能复活的条件：① 非-Bedrock（OpenAI 直连 gpt-5.x-codex）下 steer 是否生效未测——若生效可做 per-provider 门控；② codex 后续版本明确 steer 语义。届时重测再议。

> 教训：协议原语的"ACK 成功"≠"语义生效"。验证可行性必须测**端到端效果**，不能止于 RPC 返回码。

---

## 8. askuser — **可实现（schema 已捕获）** 

§3 初判"BLOCKED：requestUserInput schema 未验证"。2026-06-21 live 捕获补全（codex 0.141，`experimentalFeature/list` + 实跑）：

### 8.1 触发前提
- 反向请求方法名确认为 **`item/tool/requestUserInput`**（`ServerRequest.json` 注册表 index[2]）。
- **默认关闭**，必须 `--enable default_mode_request_user_input`（`-c features.default_mode_request_user_input=true`）。RFC 旧猜名 `request_user_input` 错误（server 启动即 `Unknown feature flag` 退出）。
- 另有 stable 且默认开的 `mcpServer/elicitation/request`（gated `tool_call_mcp_elicitation`），form/url schema 不同，非本卡片路径。

### 8.2 请求 schema（实测样本）
```json
{ "method":"item/tool/requestUserInput", "id":0, "params":{
  "threadId":"...", "turnId":"...", "itemId":"call_0",
  "questions":[{ "id":"deploy_target", "header":"Deploy",
    "question":"What do you want to deploy?", "isOther":true, "isSecret":false,
    "options":[{"label":"API server (Recommended)","description":"..."}, ...] }],
  "autoResolutionMs":null }}
```
- question 必填 `id`/`header`/`question`；可选 `isOther`(允许自由文本)/`isSecret`(掩码)/`options`(null⇒自由文本题)。
- option 只有 `label`+`description`，**label 即回传值**（无独立 value）。`questions` 是数组（一卡多问）。

### 8.3 响应 schema（实测解锁 turn）
```json
{ "id":0, "result":{ "answers":{ "<questionId>":{ "answers":["<选中的 label>"] } } } }
```
- `result.answers` 是**按 question id 键控的对象**（非数组）；值 `{answers:[label,...]}`（数组支持多选）。
- **校验宽松不报错**：错误/空 `result` 不会 JSON-RPC error，而是被当"用户未答"→模型**重发同一 requestUserInput**（观测到 id=1 重试）。故必须回**正确的 answers 键控结构**，否则死循环。

### 8.4 阻塞性（实测确认）
- `turn/start` 立即返回 result，但 **turn 不 completed** 直到反向请求被回答。挂起 12s 无 `turn/completed`；回答后才出 `item/agentMessage/delta` + `turn/completed`，并伴随 `serverRequest/resolved {requestId}`。
- requestUserInput **不**带自己的 item/started/completed，只作为 reverse RPC（keyed by itemId=call_0）出现。

### 8.5 与 claude askuser 的根本差异（实现难点）
claude 在 `-p` 下 **fire-and-forget**（~3ms 自注 `is_error:true` tool_result，turn 正常结束，答案作**下一轮 user 消息**回流）。`askuser-question.md` §"推翻的原设计假设"**明确删掉了** pendingQuestion 表/TTL/阻塞等待。

codex **真阻塞**——这些机制要**全部加回来**：
- ReadEvent 解析 `item/tool/requestUserInput` → `cli.AskQuestion`（itemId=ToolUseID，questions→Items，options.label→Opt）。
- pending 表持有 `requestId`+`itemId`+TTL（autoResolutionMs 可驱动）。
- 用户答案**不是发新 turn**，而是回 `{result:{answers:{qid:{answers:[label]}}}}` 到那条阻塞 RPC（HandleEvent 或新写回路径）。
- 用户不答 → 必须 TTL 自动回一个 decline-shaped answer 防 turn 永挂 + 模型重发死循环。

### 8.6 落地估时 / 风险
- ~2-3 天。中风险：`default_mode_request_user_input` 标 `underDevelopment`（schema 可能变）；阻塞语义要求 naozhi dispatch 持 turn 开（与 Collect/passthrough 的 turn 生命周期假设不同）。
- 建议：独立 PR；先在 BuildArgs 加 flag + ReadEvent 解析 + pending 表 + 回写路径，TTL 兜底必须有。

### 8.7 推荐顺序更新（取代 §4）
1. ✅ embedded_context（已交付 PR #2219）
2. **askuser**（可实现，schema 齐备，~2-3d，独立 PR）
3. ~~passthrough~~ → **推迟**（§7，Bedrock steer 实测无效）
