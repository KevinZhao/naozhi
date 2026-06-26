# Codex Backend 接入

> **状态**: **Ready for implementation v2（源码核对；运行时实测待跑）**
>
> 协议部分最初基于 **openai/codex@main `c73296a0`**（`c73296a0f095e72dbb646909c613ae09c9459c3a`，2026-06-18）逐方法核对并 pin 版本；**v2.1 终稿编辑轮已对三个 blocker（B1/B2/B3）+ token usage + approval decision + 反向 method 全集做第二人独立 curl 复核**（见 §11.1 头注，"单人核对"已升级为"双人核对"）。naozhi 接线点已对照 **naozhi@master `cd7334ec`** 工作树核对（每处带"文件:行"证据）。
>
> ⏳ **运行时实测 V1–V12 仍待在有 codex 的环境补跑**：本机【未安装】 OpenAI Codex CLI（顶层 `~/.codex` 是另一个无关工具的目录），所以一切以官方源码与文档为准，本文不包含任何"运行时实测结果"。运行时实测是**验证而非阻塞** Phase 1 起步；Phase 1 编码可在仅有源码核对的前提下开工，仍需真机实测的清单见 §9.2（G4/G5/G6/G7）、§5.1（Features 精确值）、§10.2（V1–V12 + V_* 专项）。
>
> **作者**: naozhi team
> **创建**: 2026-05-20 · **v2 重写**: 2026-06-18 · **v2.1 终稿编辑（对抗性审查修订 + 第二人源码复核）**: 2026-06-18
> **依赖 / 前置**:
> - `internal/cli/backend/profile.go`（多 backend 注册表已就位，参见 `multi-backend.md`）
> - `internal/cli/protocol.go` `Protocol` 接口（v2 已逐方法核对真实签名，见 §6）
> - `internal/cli/protocol_acp.go`（JSON-RPC over stdio 长连接的现成骨架；可借鉴真实字段名，但 **codex 不复用其 turn 收口模型**，见 §6）
> - `internal/wireup/history_backends.go`（历史 source 真实注册点 = blank import，**不是 main.go**）
> - `docs/rfc/multi-backend.md` v2（backend.Profile 抽象与 Dashboard §8 差异化规约）
> **关联代码**: `internal/cli/backend/` · `internal/cli/protocol_acp.go` · `internal/cli/process.go` · `internal/cli/history.go` · `internal/session/router_core.go` · `internal/session/router_lifecycle.go` · `internal/wireup/history_backends.go` · `internal/history/`
> **可行性验证**: `docs/rfc/codex-backend-validation.md`（运行时实测脚本，需 codex 环境；Phase 1 不阻塞，见 §10）

---

## 0. TL;DR 与定调

OpenAI 的 Codex CLI 已经把"程序化接入"路线收敛到 **`codex app-server`**：stdio NDJSON 长连接，双向通信，Thread/Turn/Item 三层 primitive，schema 可一键生成。这条路跟 naozhi 现有的 `Process` 长生命周期 + JSON-RPC 双向框架完全契合。

接 Codex 的工作量约等于"写一个独立的 `protocol_codex.go` + 一个 `profile_codex.go` + 一个 `internal/history/codexjsonl/`"，**不复用 `ACPProtocol`**（method/schema 不同），但可以借鉴它的双向 RPC 握手骨架与自动授权骨架。

**不要走** 老的 `codex proto` 子命令（已从 CLI 完全移除）、`codex exec --json` 单次模式、`codex mcp-server` MCP 包装这三条路，理由见 §1。

### v2 相对 v1 的核心修订（一句话结论）

v1 的**选型方向全对**（走 app-server、stdio NDJSON、三层 primitive、`-c` 配置覆盖、`~/.codex` 凭据），但 v1 **猜测的具体 wire 形状错得相当系统**。源码核对后确认三个 **blocker**：

1. **`turn/start` 时序整个搞反**——它**立即返回**初始 Turn 对象，turn 推进全在 response 之后用异步通知下发；v1 把它当"长 RPC，流完才 reply"，照此实现会永远收不到终结 response。
2. **token usage 取数来源搞错**——`turn/completed` 通知**不带** usage / response_id，token 走独立通知 `thread/tokenUsage/updated`。
3. **反向审批 method 名与 decision 值全部臆造**——没有 `{method:"approve"}`、没有 `{decision:"allow"}`，照 v1 草图回包会让 server 反序列化失败。

逐条对照见 §2，分级修正清单见 §3。

---

## 1. 背景

### 1.1 Codex CLI 当前的程序化入口

| 入口 | 描述 | 是否适合 naozhi |
|---|---|---|
| `codex` (TUI) | 交互式 terminal UI | ❌ 不可程序化 |
| `codex proto` | 老的 protocol mode | ❌ **已从 CLI 完全移除**（顶层 Subcommand enum 无 Proto 变体），仅 `docs/protocol_v1.md` 留历史 |
| `codex exec --json` | 非交互单次模式，每次起进程，输出 NDJSON `ThreadEvent`（事件名点号风格 `thread.started`/`turn.completed`） | ❌ 失去长生命周期，每 turn 重启，跟 naozhi 长连接模型冲突。flag 是 `--json`（官方明示**不是** `--experimental-json`） |
| **`codex app-server`** | **stdio NDJSON，长连接，Thread/Turn/Item 三层 primitive** | ✅ **首选**（标注：子命令在 CLI 标 `[experimental]`，见 §1.2） |
| `codex mcp-server` | 把 codex 包成 MCP server | ❌ 富交互（streaming / approval）映射不进 MCP |

> 证据：`codex proto` 已移除 —— `codex-rs/cli/src/main.rs` 顶层 Subcommand enum 无 `Proto` 变体（main.rs:123-212 区间通读 + grep 无命中）。`app-server` 子命令 —— main.rs:146-147 `/// [experimental] Run the app server or related tooling. AppServer(AppServerCommand)`。

### 1.2 为什么是 app-server（以及 experimental 的诚实标注）

OpenAI 自家所有 client（VS Code 扩展、Codex Web、TUI `--remote`、macOS app）都已统一到 app-server，这是当前唯一 first-class 的程序化集成路径。但有两点必须诚实标注：

- **`app-server` 子命令在 CLI 标 `[experimental]`**（main.rs:146-147、514-535、591-682 的 `generate-ts`/`generate-json-schema` 子命令也标 experimental）。
- **大量扩展 method 需 client 在 `initialize` 时声明 `capabilities.experimentalApi:true` 才会下发**（`InitializeCapabilities.experimental_api`，v1.rs:43-47）。naozhi phase1 **只依赖稳定基础链路**（见 §0 与 §10），不依赖标 `#[experimental(...)]` 的 method。
- v1 §0/§1.2 称"OpenAI 承诺向后兼容 first-class"。**该表述降级**：main 分支 README 无该原文，且子命令标 experimental。建议改引可核验的 `generate-json-schema --experimental` 版本绑定来锚定 schema，而非依赖未核实的兼容性承诺。（minor，见 §3 m8）

而 `codex app-server` 的协议层事实（源码核对）：

- **不是严格 JSON-RPC 2.0**：源码自述 "We do not do true JSON-RPC 2.0, as we neither send nor expect the `jsonrpc`:`2.0` field"（`codex-rs/app-server-protocol/src/jsonrpc_lite.rs:1-2`）。stdio 模式每行一条 JSON + `\n`。
- **id 类型** `RequestId`（`#[serde(untagged)] enum { String(String), Integer(i64) }`，jsonrpc_lite.rs:14-22），string（UUID）与 number 都合法。
- **schema 可一键生成**：`codex app-server generate-ts --out DIR` / `generate-json-schema --out DIR`（main.rs:598-601，均 experimental，带 `--out`/`-o`；不带 `--experimental` 时生成的 schema 不含实验性条目）。
- Thread / Turn / Item 三层 primitive，session 持久化在 `~/.codex/sessions/...`，可 resume。

### 1.3 范围

**做**：
1. 在 `internal/cli/protocol_codex.go` 新增 `CodexProtocol` 实现 `cli.Protocol`（11 个方法 + 可选 `Capabilities()`/`ReadEventInto`，见 §6）
2. 在 `internal/cli/backend/profile_codex.go` 新增 `codexProfile()` 并注册到 `RegisterDefaults`
3. 新增 `internal/history/codexjsonl/`（init() 注册 + `wireup/history_backends.go` blank import），让 dashboard 历史面板能读 `~/.codex/sessions/`
4. 接通 codex sessions 目录的四点接线链（`HistoryWiring`/`RouterConfig`/`router_lifecycle.go`/`main.go`，见 §4）
5. Dashboard chip 颜色、cost 单位、能力 map 按 `multi-backend.md` §8 接进去
6. 文档：本 RFC + `codex-backend-validation.md` 实测报告（实测在有 codex 的环境补跑）

**不做**：
- 不复用 `ACPProtocol`（method 名、schema、turn 收口模型完全不同；强行复用会污染 ACP 的 Kiro/Gemini 实现）
- 不改 `internal/cli/process.go` 任何逻辑（Protocol 接口已经是抽象边界）
- 不接 `codex proto` / `codex exec` / `codex mcp-server` 三条死路
- 不做 codex 特有 UI（plan mode / collaboration mode）的精细化呈现，先用通用 tool_call 渲染，后续单独 RFC

---

## 2. 协议真相对照表

> 列含义：**RFC v1 写法** → **真实情况** → **证据（文件:行）**。凡 v1 猜错者标 **❌猜错**，部分对者标 **⚠️半对**，全对者标 **✅**。
> 所有路径相对 `codex-rs/`，pin 在 commit `c73296a0`；naozhi 路径相对仓库根。

### 2.1 传输 / 框架 / 握手

| 主题 | RFC v1 写法 | 真实情况 | 证据 |
|---|---|---|---|
| 启动命令 | `codex app-server --listen stdio://` | **✅** `codex app-server`（裸命令即 stdio）；`--listen` 默认 `DEFAULT_LISTEN_URL`（即 `stdio://`）。可选值还有 `unix://`/`ws://IP:PORT`/`off`。**整个子命令标 `[experimental]`** | `cli/src/main.rs:146-147`（experimental）、`514-535`（`listen` 默认 `DEFAULT_LISTEN_URL`） |
| `--listen stdio://`/`unix://` 字面 | 当作三种确证写法 | **⚠️半对** 这些值为合法 listen URL，但 naozhi 用裸 `codex app-server`（stdio）即可，不必显式 `--listen` | `cli/src/main.rs:526-530` |
| JSON-RPC | "JSON-RPC 2.0（省略 jsonrpc 头）" | **✅ 更精确**："not true JSON-RPC 2.0 — we neither send nor expect the `jsonrpc`:`2.0` field"。stdio=每行一条 JSON+`\n` | `app-server-protocol/src/jsonrpc_lite.rs:1-2` |
| id 类型 | "string(UUID)与 number 都允许，按 RawMessage 反序列化" | **✅** `RequestId = #[serde(untagged)] enum { String(String), Integer(i64) }` | `app-server-protocol/src/jsonrpc_lite.rs:14-22` |
| `initialize` method | `initialize` | **✅** `ClientRequest::Initialize`（无 wire 覆写 → camelCase `initialize`） | `app-server-protocol/src/protocol/common.rs:467-471` |
| `initialize` params | (草图未细列) | **真实** `InitializeParams{ clientInfo:{name,title?,version}, capabilities?:{experimentalApi:bool, requestAttestation:bool, optOutNotificationMethods?:string[]} }`，全 camelCase | `app-server-protocol/src/protocol/v1.rs:28-56` |
| `initialize` result | **❌猜错** `{ result:{ serverInfo:{...} } }` | **❌猜错·无 serverInfo**。真实 `InitializeResponse{ userAgent, codexHome, platformFamily, platformOs }`（全 camelCase） | `app-server-protocol/src/protocol/v1.rs:58-71` |
| `initialized` 通知 | 握手第三步 | **⚠️半对** 通知存在（唯一的 client→server 通知，`{"method":"initialized"}` 无 params），client 仍应发；但 **server 不回它**，**不要把"等 initialized 确认"当可阻塞步骤** | `app-server-protocol/src/protocol/common.rs:1668-1670`（`client_notification_definitions! { Initialized, }`） |
| 标准错误码 | `"Not initialized"` / `"Already initialized"` / `-32001` | **✅** -32600 InvalidRequest、-32601 MethodNotFound、-32602 InvalidParams、-32603 InternalError、-32001 Overloaded、字符串码 `"input_too_large"` | `app-server/src/error_code.rs:3-8` |

### 2.2 三层 Primitive 与生命周期 method

| 主题 | RFC v1 写法 | 真实情况 | 证据 |
|---|---|---|---|
| 三层模型 | Thread/Turn/Item | **✅** 源码与文档均确认 | `app-server-protocol/src/protocol/v2/thread_data.rs:135-208`（Thread/Turn 结构） |
| Thread 层 method | thread/start、thread/resume、thread/archive、**`thread/close`** | **❌猜错·无 `thread/close`**。真实有 80+ 个，含 thread/start、thread/resume、thread/fork、thread/archive、thread/unarchive、thread/delete、thread/unsubscribe、thread/rollback、thread/list、thread/read、thread/loaded/list…。关闭语义=`thread/unsubscribe`/`thread/archive`/`thread/delete`；`thread/closed` 只是**通知**。调 `thread/close` 得 -32601 | `app-server-protocol/src/protocol/common.rs:476-652`（method 表）、`1578`（`ThreadClosed => "thread/closed"` 通知） |
| Item kind | …、**"todo list"** | **❌猜错·无 "todo list"**，真实是 **`plan`**（配 `item/plan/delta` + `turn/plan/updated`）。`ThreadItem`（`tag="type"`,camelCase）含 UserMessage、AgentMessage、Plan、Reasoning、CommandExecution、FileChange、McpToolCall、WebSearch 等 | `app-server-protocol/src/protocol/v2/item.rs:215-410`（`pub enum ThreadItem` 各变体 + id 提取 arm） |

### 2.3 turn 生命周期（**v1 错得最重的一块**）

| 主题 | RFC v1 写法 | 真实情况 | 证据 |
|---|---|---|---|
| `turn/start` 时序 | **❌blocker** "turn/start 是长 RPC，server 流完所有 notification 后才 reply，复用 waitForResponse 长超时(5min)" | **❌猜错·blocker·立即返回** `TurnStartResponse{ turn }`，其中 `turn.status=inProgress`、`items=[]`、`completedAt=null`。turn 真实推进（turn/started → item/* → turn/completed）全在 response **之后**通过异步通知下发。**与 ACP `session/prompt`（response 即 turn 结束）恰恰相反**。若按 v1 用长超时等 turn/start 的"最终 response"会永远收不到 | `app-server-protocol/src/protocol/v2/turn.rs:67-71`（`TurnStartParams`）、`157-159`（`TurnStartResponse{ turn:Turn }`）；`thread_data.rs:188-206`（`Turn{ id, items, items_view, status, error, started_at, completed_at, duration_ms }`） |
| turn 终结信号 | turn/start 的 response | **❌猜错·`turn/completed` 通知**（`TurnCompletedNotification{ threadId, turn }`）。`ReadEvent` 必须把这条通知翻成 `done=true` | `app-server-protocol/src/protocol/common.rs:1588`（`TurnCompleted => "turn/completed"`）；`turn.rs:386-388`（`TurnCompletedNotification{ thread_id, turn }`，**无 usage/response_id**） |
| `turn/start` params.input | **❌** `input:"..."`（裸字符串） | **❌猜错·`input: Vec<UserInput>`**（数组）。`UserInput`（`tag="type"`）：`Text{text,textElements}`、`Image{url,detail?}`、`LocalImage{path,detail?}`、`Skill{name,path}`、`Mention{name,path}`。文本元素形如 `{type:"text",text:"...",textElements:[]}` | `app-server-protocol/src/protocol/v2/turn.rs:283-308`（`pub enum UserInput`） |
| `turn/start` 其它 params | (未细列) | `threadId`(必填)、可选 model/cwd/sandboxPolicy/approvalPolicy(标 experimental,nested)/effort/outputSchema 等 | `app-server-protocol/src/protocol/v2/turn.rs:67-130` |
| `turn/completed` 负载 | **❌blocker** `{ usage:{input_tokens,output_tokens,cached_input_tokens}, response_id }` | **❌猜错·blocker·`{ threadId, turn:Turn }`**。`Turn` **无 usage、无 response_id**。停止原因看 **`turn.status`**（枚举 camelCase：`completed`/`interrupted`/`failed`/`inProgress`），不是 `stopReason` 字段 | `app-server-protocol/src/protocol/v2/turn.rs:386-388`（负载）、`28-34`（`pub enum TurnStatus`）；`thread_data.rs:195`（`Turn.status: TurnStatus`） |
| token usage 来源 | turn/completed 顶层 | **❌猜错·独立通知 `thread/tokenUsage/updated`**（`ThreadTokenUsageUpdatedNotification{ threadId, turnId, tokenUsage:ThreadTokenUsage{total,last,modelContextWindow} }`）。`TokenUsageBreakdown{ totalTokens, inputTokens, cachedInputTokens, outputTokens, reasoningOutputTokens }`（camelCase，**v1 漏了 reasoning/total 两项**）。turn.rs 另有个扁平 `Usage{inputTokens,cachedInputTokens,outputTokens}` 但**未挂在** TurnCompletedNotification 上 | `app-server-protocol/src/protocol/common.rs:1585`（`ThreadTokenUsageUpdated`）；`v2/thread.rs:1278-1322`（三个 token 结构） |
| `response_id` | 假设存在，用于续接 | **❌猜错·整个 v2 协议无 response_id**。turn 标识用 `turn.id`；续接用 `thread.id`（可选 rollout path/history）。`Process.lastResponseID` 设计应删除/改记 thread.id | turn.rs/thread.rs grep 无 `response_id`；续接见 `v2/thread.rs:288-320`（`ThreadResumeParams{ thread_id, history?, path? }`，注释"Prefer using thread_id whenever possible"） |

### 2.4 item / 流式通知形状

| 主题 | RFC v1 写法 | 真实情况 | 证据 |
|---|---|---|---|
| `item/started` | **❌** `{ itemId, type:"agentMessage" }`（顶层） | **❌猜错·`{ item:ThreadItem, threadId, turnId, startedAtMs }`**。kind 判别符 `type` 在**内嵌 `item.type`**，id 在 `item.id` | `app-server-protocol/src/protocol/v2/item.rs:1111-1117`（`ItemStartedNotification{ item, thread_id, turn_id, started_at_ms }`） |
| `item/completed` | `{ itemId,...detail }` | **真实** `{ item:ThreadItem, threadId, turnId, completedAtMs }` | `app-server-protocol/src/protocol/v2/item.rs:1185-1191`（`ItemCompletedNotification`） |
| `item/agentMessage/delta` | **❌** `{ itemId, content:"Hi" }`（增量字段叫 content） | **❌猜错·`{ threadId, turnId, itemId, delta }`** —— 增量字段是 **`delta`**（不是 content）。累积=顺序拼接 delta 字符串；全文也落在 item/completed 的 agentMessage.text | `app-server-protocol/src/protocol/v2/item.rs:1207-1211`（`AgentMessageDeltaNotification{ thread_id, turn_id, item_id, delta }`） |
| `thread/started` | `{ threadId }` | **真实** `ThreadStartedNotification{ thread:Thread }` —— id 在 `thread.id` 非顶层 | `app-server-protocol/src/protocol/common.rs:1573`（`ThreadStarted`） |
| reasoning / 命令输出增量 | (未列) | reasoning 走 `item/reasoning/textDelta`/`summaryTextDelta`；命令输出走 `item/commandExecution/outputDelta` | `app-server-protocol/src/protocol/common.rs:1609`（CommandExecutionOutputDelta）、`1625-1627`（reasoning deltas） |
| 通知全集 | 列了 6 个 | **✅** 6 个全对（thread/started、turn/started、turn/completed、item/started、item/completed、item/agentMessage/delta）；另有 60+ 未列（error、thread/status/changed、**thread/tokenUsage/updated**、turn/diff/updated、turn/plan/updated、**serverRequest/resolved**、thread/compacted、warning、deprecationNotice 等） | `app-server-protocol/src/protocol/common.rs:1570-1660`（`server_notification_definitions!` 全表） |

### 2.5 反向请求 / approval（**v1 全部臆造**）

| 主题 | RFC v1 写法 | 真实情况 | 证据 |
|---|---|---|---|
| 反向请求 method | **❌blocker** `{method:"approve"}` / `serverRequest/...` | **❌猜错·blocker·没有 "approve"，没有 serverRequest/ 前缀的请求**。真实 method(v2)：`item/commandExecution/requestApproval`、`item/fileChange/requestApproval`、`item/permissions/requestApproval`、`item/tool/requestUserInput`(EXPERIMENTAL)、`mcpServer/elicitation/request`、`item/tool/call`(DynamicToolCall)、`account/chatgptAuthTokens/refresh`、`attestation/generate`；legacy(deprecated)：`applyPatchApproval`、`execCommandApproval` | `app-server-protocol/src/protocol/common.rs:1422-1486`（`server_request_definitions!`） |
| client 回应 | **❌** `{ decision:"allow" }` | **❌猜错·没有 "allow"**。命令审批回 `{ decision: CommandExecutionApprovalDecision }`，值=`accept`/`acceptForSession`/`acceptWithExecpolicyAmendment{...}`/`applyNetworkPolicyAmendment{...}`/`decline`/`cancel`（共 6 变体）。文件审批回 `{ decision: FileChangeApprovalDecision }`，值=`accept`/`acceptForSession`/`decline`/`cancel`。语义：**`decline`=拒绝但 turn 继续，`cancel`=拒绝且立即中断 turn**（源码注释逐字）。**异构响应**：permissions 回 `{permissions, scope, strictAutoReview?}`；requestUserInput 回 `{answers}`；elicitation 回 `{action,content}` | `app-server-protocol/src/protocol/v2/item.rs:45-66`（CommandExecutionApprovalDecision，含 Decline/Cancel 注释）、`93-102`（FileChangeApprovalDecision） |
| 反向请求 params | (未列) | 都带 `threadId+turnId+itemId` 三元组路由 + `startedAtMs`；命令还有可选 command/cwd/reason/availableDecisions；文件还有 reason? | `app-server-protocol/src/protocol/v2/item.rs:1309-1390`（CommandExecution/FileChange approval params + response `decision`） |
| out-of-band 解除 | (未提) | **新增** server 可发 `serverRequest/resolved` 通知（`{threadId, requestId}`）告知"该反向请求已被解除"（如 turn 状态变化）；naozhi 反向处理器**必须消费此通知取消挂起等待**，否则审批悬挂 | `app-server-protocol/src/protocol/common.rs:1614`（`ServerRequestResolved => "serverRequest/resolved"`） |

### 2.6 中断 / steer / backpressure

| 主题 | RFC v1 写法 | 真实情况 | 证据 |
|---|---|---|---|
| 中断 | `turn/interrupt` request | **✅机制对，但⚠️字段** request（非 notification），`TurnInterruptParams{ threadId, turnId }`（**两个 id 都必填**），返回空 `TurnInterruptResponse{}`，turn 以 status=interrupted 结束。**没有 ACP 的 session/cancel notification** | `app-server-protocol/src/protocol/common.rs:811-815`（method）；`v2/turn.rs:200-208`（params/response） |
| steer | Codex 独有"turn 进行中追加输入" | **✅** `turn/steer` request，`TurnSteerParams{ threadId, input:Vec<UserInput>, clientUserMessageId?, expectedTurnId(必填,与活跃 turn 不匹配则失败), additionalContext?(experimental) }`，返回 `{ turnId }`。Claude/ACP 无对应 | `app-server-protocol/src/protocol/common.rs:805-810`；`v2/turn.rs:166-194`（含 `expected_turn_id` 注释"fails when it does not match the currently active turn"） |
| backpressure 码 | `-32001 "Server overloaded; retry later."` | **✅** code 与文案**逐字精确匹配**（常量 `OVERLOADED_ERROR_CODE`）。补：仅入站**请求队列（容量 128，`CHANNEL_CAPACITY`）满**时返回；指数退避是 client 侧约定非协议强制。client 端应只按 `code==-32001` 判定（message 文案由调用点提供） | `app-server-transport/src/transport/mod.rs:50`（`OVERLOADED_ERROR_CODE = -32001`）、`237-238`（精确文案）、`22-24`（`CHANNEL_CAPACITY=128`）；`app-server/src/error_code.rs:7` |

### 2.7 配置 / 鉴权 / schema / 历史目录

| 主题 | RFC v1 写法 | 真实情况 | 证据 |
|---|---|---|---|
| `-c key=value` | `-c model=` `-c approval_policy=never` `-c sandbox_mode=workspace-write` | **✅** `-c/--config` 是全 CLI 共享 global flag，value 先按 TOML 解析、失败当字面串，key 支持 dotted path | `core/src/config/mod.rs:2196,2234`（sandbox_mode override 路径）；config override 解析逻辑见 `common` crate（具体行待 Phase 0 schema 对照确认，见 §11 待 re-pin） |
| config key 名 | model/approval_policy/sandbox_mode | **✅** 三个 key 名正确。`approval_policy` 合法值：`untrusted`/`on-request`/`never`；`sandbox_mode`：`read-only`/`workspace-write`/`danger-full-access`（均 kebab-case） | `core/src/config/mod.rs:322`（`approval_policy: Constrained<AskForApproval>`）、`2196`（`sandbox_mode: Option<SandboxMode>`） |
| 凭据文件 | `~/.codex/auth.json` | **⚠️待 re-pin** `CODEX_HOME`（默认 `~/.codex`，`find_codex_home()` 解析）确认；**但凭据文件名在 main 上是 `CODEX_HOME/.credentials.json`**（config/mod.rs:814 注释），与 v1 的 `auth.json` 不一致——auth 模块自上次研究已重组。**doctor 检查应探测 `~/.codex` 下凭据存在性而非硬编码文件名**，精确文件名 Phase 0 re-pin（见 §11） | `core/src/config/mod.rs:814`（`file: CODEX_HOME/.credentials.json`）、`861`（CODEX_HOME 默认）、`1226`（`find_codex_home()`） |
| 环境变量 | 仅 `CODEX_API_KEY` | **⚠️半对** codex 识别**两个**：`OPENAI_API_KEY` 与 `CODEX_API_KEY`（均有专用常量）。doctor 检查应接受**任一** | `login/src/lib.rs:28`（`CODEX_API_KEY_ENV_VAR`）、`36`（`OPENAI_API_KEY_ENV_VAR`） |
| Bedrock 鉴权 | **❌** "Codex 只能走 OpenAI 自家，不像 claude 走 Bedrock" | **❌猜错·major·Codex 原生支持 Amazon Bedrock**（上次研究确认 `login_with_bedrock_api_key`/`AuthMode::BedrockApiKey`/独立 `codex-aws-auth` crate）。**部署侧可探索复用现有 Bedrock 凭据体系**。⚠️精确 CLI/env 注入入口与 region 配置 Phase 0 re-pin（auth 模块已重组，见 §11） | 上次研究 pin `core/src/bedrock_api_key.rs`、`aws-auth/Cargo.toml`（本轮该路径 404，已重组，标为待 re-pin） |
| schema 生成 | `generate-ts/generate-json-schema --out DIR` | **✅** 两子命令存在（均 `[experimental]`），`--out`（短 `-o`），不带 `--experimental` 时不含实验性条目 | `cli/src/main.rs:598-601`（AppServerSubcommand GenerateTs/GenerateJsonSchema） |
| 历史落盘目录 | **⚠️** `~/.codex/sessions/`（暗示扁平） | **⚠️半对·major·按日期三级分桶**：`$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<本地ISO时间戳>-<threadId>.jsonl`（本地时区）；归档在**独立目录** `$CODEX_HOME/archived_sessions`（非 sessions/ 子目录）；**文件可能 gzip 压缩**（需透明解压，格式 Phase 0 re-pin）。HistoryDir 写 `~/.codex/sessions/` 作 doctor 显示串没问题，但 codexjsonl **不能照搬 kiro 的扁平 `<sid>.jsonl` join** | `rollout/src/recorder.rs:1416-1430`（`dir.push(year)`/`push({:02}month)`/`push({:02}day)` + `dir.join(filename)`，filename `rollout-[ISO]-[uuid].jsonl`）；`rollout/src/lib.rs:24-25`（`SESSIONS_SUBDIR="sessions"`、`ARCHIVED_SESSIONS_SUBDIR="archived_sessions"`）；`recorder.rs:1218`（`codex_home.join(ARCHIVED_SESSIONS_SUBDIR)`） |
| npm 包 / 二进制 | `@openai/codex` → `codex` | **✅** | README / npm `@openai/codex` bin 名 `codex`（上次研究 pin `README.md:32`/`cli/src/main.rs`） |

---

## 3. RFC 逐条修正清单（去重 + 分级）

> severity：**blocker**=照 v1 实现会直接跑不通；**major**=接口/method/字段名错或缺失；**minor**=措辞或可补充。每条：**原文 → 应改为**。

### 🔴 BLOCKER

**B1. turn/start 时序模型整个搞反**
- 原文(§2.4)："turn/start 是 request，server 流完所有 notification 后才 reply 这个 request；waitForResponse 支持长超时(5min)；与 Kiro ACP session/prompt 一致"。
- 应改为：`turn/start` **提交后立即返回** `TurnStartResponse{turn:{status:inProgress, items:[], completedAt:null}}`；turn 终结信号是后续独立的 **`turn/completed` 通知**（含终态 Turn）。事件流：`turn/start(req) → turn/start(resp, inProgress) → turn/started(notif) → item/*(notif) → [反向 approval 请求/响应] → turn/completed(notif, 终态)`。**与 ACP 恰恰相反**。`ReadEvent` 以 `turn/completed` 通知作 `done=true`，**不要**用长超时等 turn/start 的 response。

**B2. token usage 取数来源搞错**
- 原文(§2.6/§6)："turn/completed 带 `{usage:{input_tokens,output_tokens,cached_input_tokens}, response_id}`；据此映射 Cost/Usage"。
- 应改为：`TurnCompletedNotification` 只带 `{threadId, turn}`，**Turn 无 usage、无 response_id**。token 经独立通知 **`thread/tokenUsage/updated`** 推送，`tokenUsage.{total,last}` 各为 `TokenUsageBreakdown{totalTokens, inputTokens, cachedInputTokens, outputTokens, reasoningOutputTokens}`。`ReadEvent` 必须**单独处理这条 method** 才能拿到 token，否则 cost 列恒空。CostUnit=tokens 方向对，取数来源改。

**B3. 反向审批 method 名与 decision 值全部臆造**
- 原文(§2.4/§2.5)：server 发 `{method:"approve"}` 或 `serverRequest/...`，client 回 `{decision:"allow"}`。
- 应改为：**没有 "approve"，没有 "allow"**。反向请求 method=`item/commandExecution/requestApproval`、`item/fileChange/requestApproval`、`item/permissions/requestApproval`、`item/tool/requestUserInput`、`mcpServer/elicitation/request`、`item/tool/call`（+legacy applyPatchApproval/execCommandApproval）。decision 值=`accept`/`acceptForSession`/`decline`/`cancel`（命令多 2 个 amendment 变体）。照草图回 `{decision:"allow"}` 会让 server 反序列化失败。

### 🟠 MAJOR

**M1. initialize result 无 serverInfo** — 原文(§2.2)`result:{serverInfo}` → 真实 `InitializeResponse{userAgent, codexHome, platformFamily, platformOs}`。取 `result.serverInfo` 拿到 null。

**M2. item 通知顶层形状错** — 原文(§2.4)`item/started{itemId,type}`、`delta{itemId,content}` → 真实 `item/started{item:ThreadItem, threadId, turnId, startedAtMs}`（type/id 在 `item.type`/`item.id`）；delta 字段是 **`delta`** 非 content。

**M3. 不存在 thread/close** — 原文(§2.3) Thread 层含 `thread/close` → 删除或改为 `thread/unsubscribe`/`thread/archive`/`thread/delete`；`thread/closed` 仅通知；调 thread/close 得 -32601。

**M4. turn 停止原因字段名错** — 原文(§3/§8.1/V6)`stopReason=interrupted` → 真实 `turn.status == "interrupted"`（`TurnStatus` 枚举 completed/interrupted/failed/inProgress）。

**M5. response_id / Process.lastResponseID 无依据** — 原文(§6)"存 response_id 做续接" → 协议无 response_id；resume 用 `thread.id`（= Init 返回的 sessionID）。删除该字段或改记 thread.id。

**M6. turn/start.input 是结构化数组** — 原文(§2.2)`input:"..."` → `input: Vec<UserInput>`（tagged enum），文本元素 `{type:"text",text:"...",textElements:[]}`，不能传裸字符串。

**M7. cli.SendResult 没有 InputTokens/OutputTokens**（本地核对，已二次确认）
- 原文(§4.2)："SendResult 已有 InputTokens/OutputTokens（Gemini 也用），无需新增"。
- 应改为：**SendResult 只有 `Text/SessionID/CostUSD/MergedCount/MergedWithHead/HeadText`**；承载 codex token **必须新增字段**（见 §6.6）。证据：`internal/cli/event.go:536-545`（已读，struct 全部字段如上，无 token 分项）。`InputTokens/OutputTokens` 仅在无关的 `internal/dashboard/cron/transcript.go`（Claude usage JSON）出现。

**M8. 历史 source 注册点不是 main.go**（本地核对，已二次确认）
- 原文(§4.2)："cmd/naozhi/main.go — 历史 source 注册增 codexjsonl"。
- 应改为：注册=(a) codexjsonl 包 `init()` 调 `cli.RegisterHistoryFactory("codex", factory)`；(b) `internal/wireup/history_backends.go` 加一行 blank import。main.go **只**在 RouterConfig 填**目录路径**。证据：`internal/wireup/history_backends.go:25-26`（已读，现仅 `_ claudejsonl` + `_ kirojsonl`）、`internal/cli/history.go:107,134`（`HistoryFactoryFn`/`RegisterHistoryFactory`）。

**M9. §4.2 漏列三个目录接线文件**（本地核对，已二次确认）
- 原文：文件清单未列 `internal/cli/history.go`、`internal/session/router_core.go`、`internal/session/router_lifecycle.go`。
- 应改为：要把 codex sessions 目录喂给 factory，须仿 kiro 四点链：(1) `HistoryWiring` 加 `CodexSessionsDir`；(2) `RouterConfig` + router struct 加 `CodexSessionsDir`/`codexSessionsDir`；(3) `router_lifecycle.go:119-123` 构造 HistoryWiring 时加 `CodexSessionsDir: r.codexSessionsDir`；(4) main.go RouterConfig 加 `CodexSessionsDir`。证据：`internal/cli/history.go:68-82`（`HistoryWiring{ClaudeDir, KiroSessionsDir, EventLogDir}`，确无 Codex 字段）、`internal/session/router_core.go:697-717`（`RouterConfig` 同三字段）、`router_lifecycle.go:119-123`（HistoryWiring 构造点）。

**M10. ACP 骨架真实名字 / 无 pendingResponses map**（本地核对）
- 原文(§6)："跟 ACPProtocol 共享骨架：pendingResponses map[id]chan + waitForResponse(id,timeout)；协议级 wMu"。
- 应改为：ACPProtocol **无 pendingResponses map、无 waitForResponse 方法、无协议级 wMu**。真实用同步 `readUntilResponse(rw, expectedID int)`（只握手期，起 goroutine + `time.NewTimer(acpHandshakeTimeout=30s)`）；握手后 turn 完成 response 在 `ReadEvent` 的 `if msg.IsResponse()` 分支翻成 Event。stdin 写锁是 `Process.shimWMu`（process.go，非协议级）。RFC 应引真实名：`readUntilResponse`/`sendAndWaitResponseMsg`/`allocID`/`Process.shimWMu`。证据：`internal/cli/protocol_acp.go:1095-1199`（`readUntilResponse`）、`966-1004`（`sendAndWaitResponse`/`Msg`）、`962-964`（`allocID`）、`188-224`（struct 成员：`mu`/`nextID`/`sessionID atomic.Pointer`/`textBuf`/`thoughtBuf`/`BackendID`，**无** pendingResponses/wMu）。

**M11. WriteInterrupt 签名带不进 turn_id**（本地核对）
- 原文(§6)："WriteInterrupt 用 turn/interrupt request，比 Claude control_request 干净"。
- 应改为：`turn/interrupt` 需 `{threadId, turnId}` 两个；naozhi 接口 `WriteInterrupt(w io.Writer, requestID string) error`（protocol.go:119）只能传一个。CodexProtocol 必须**内部缓存当前活跃 thread_id+turn_id**（从 turn/start response 或 turn/started 通知取），WriteInterrupt 时用缓存值拼参；`requestID` 形参对 codex 无用（被忽略，同 ACP，参 protocol_acp.go:488 `WriteInterrupt(w, _ string)`）。未起 turn 时返 `ErrInterruptUnsupported`（protocol.go:11）回退 SIGINT。

**M12. 反向请求种类命名 / 异构响应体** — 原文(§2.5)列 `elicitationRequest`、`requestUserInput` → 精确名 `mcpServer/elicitation/request`、`item/tool/requestUserInput`(EXPERIMENTAL)，另有 item/permissions/requestApproval、item/tool/call、attestation/generate。**且响应体异构**（decision / permissions+scope / answers / action+content），phase1 自动处理必须分 method 构造，不能统一回。

**M13. Bedrock 鉴权事实**（见 §2.7）— §7/§9 应补"Codex 原生支持 Bedrock API key，部署侧可探索复用现有 Bedrock 凭据体系"。

**M14. event.go 改动面被低估**（本地核对）— §4.3 把 event.go 列入"基本不动"，但有**两处**改动：(1) 承载 token 须扩字段（同 M7）；(2) 反向请求 method 名须经 `Event.SubType` 传给 `HandleEvent` 才能分派 codex 6+ 种异构响应（ACP 单 method 不需要，codex 多 method 必需；`SubType` 字段已存在，无需新增）。§4 与 §6.6 已明确。

### 🟡 MINOR

- **m1. DetectInProc 实参是 binary basename**（本地核对）：原文"排除 app-server 之外的 codex 子命令"基于错误前提——`internal/discovery/proc_linux.go:100`、`proc_darwin.go:78` 都先 `filepath.Base(...)` 再传 predicate，函数收到的就是 `"codex"`。正确写法 `strings.Contains(bin,"codex")`（可仿 claude 排除其他 backend 名）。
- **m2. backpressure 文案别硬匹配**：-32001 码对，但 message 文案由调用点提供；client 端应只按 `code==-32001` 判定。
- **m3. initialized 不是可阻塞门控**：server 不回 initialized 通知，不要等它确认。
- **m4. id 类型补充**：client 侧建议发 UUID 字符串；响应反向请求时**原样回传 server 给的 id 类型**（RawMessage 透传需覆盖 String|Integer 两种，参 protocol_acp.go:730 `permissionResponse.ID json.RawMessage`）。
- **m5. config.go 零代码改动**：codex 不默认开启，需 `cli.backends` 显式列；**纯文档，代码零改**。证据：`internal/config/config.go:1288-1342`（`EnabledBackends()` 配置驱动）。
- **m6. codex proto 已移除**（见 §1.1）。
- **m7. exec flag 是 --json**（见 §1.1）。
- **m8. app-server "向后兼容承诺"降级**（见 §1.2）：CLI 标 `[experimental]`、扩展 method 需 `experimentalApi`。改引可核验的 generate-json-schema 版本绑定。
- **m9. -c vs thread/start 二选一**：model/approval/sandbox 也是 thread/start RPC 合法参数（更贴 per-thread 语义），`cwd` **必须**走 thread/start params（codex app-server CLI 无 cwd flag；`SpawnOptions.WorkingDir` 经 `Init` 的 cwd 形参下发，**非"naozhi 拿不到 cwd"**，见 §6.4）。-c 不算错但非最优。
- **m10. listen 字面写法补充**：补 `off` 取值；app-server 整体标 experimental。
- **m11. steer 约束**：§3 补"`expectedTurnId` 必填，需先持久化活跃 turn_id"。
- **m12. RequiredNodeCaps 命名取舍**：`codex-app-server` vs `codex` 是纯命名（naozhi 内部闭环都跑通），推荐 `codex-app-server`（对齐 kiro `acp` 协议名风格，见 §5）。
- **m13. sandbox 值以 Rust 源为准**：serde 源是 `workspace-write`/`untrusted`（kebab）；naozhi 只用 never+workspace-write，两种写法 never 都对。

---

## 4. 文件变更清单

### 4.1 新增

| 路径 | 行数估计（估计值，待实现核定） | 说明 |
|---|---|---|
| `internal/cli/protocol_codex.go` | ~700 | `CodexProtocol` 实现 `Protocol`：Name/Clone/BuildArgs/Init/WriteMessage/WriteUserMessageLocked/SupportsPriority/SupportsReplay/WriteInterrupt/ReadEvent/HandleEvent + 可选 Capabilities/ReadEventInto（见 §6） |
| `internal/cli/protocol_codex_test.go` | ~450 | 表驱动：握手、turn 立即返回+turn/completed 收口、tokenUsage 通知映射、interrupt(双 id)、**按 `ev.SubType` 分派各 approval method 的异构响应**(命令/文件 decision、permissions、elicitation)、serverRequest/resolved 取消、backpressure -32001、id String/Integer 兼容 |
| `internal/cli/backend/profile_codex.go` | ~70 | `codexProfile()`（七键取值见 §5） |
| `internal/history/codexjsonl/source.go` | **~300+**（比 kiro 复杂：日期分桶遍历 + gzip + 多 type 分派，v1 的 ~250 偏低） | 读 `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` 转 dashboard 历史 source（见 §8） |
| `internal/history/codexjsonl/source_test.go` | ~200 | fixture 驱动（含日期分桶 + threadId 后缀命中/不误读 + gzip fixture） |
| `docs/rfc/codex-backend-validation.md` | 待写 | 运行时实测脚本与日志（需 codex 环境，见 §10） |

### 4.2 修改

| 路径 | 改动 | 证据（接线先例） |
|---|---|---|
| `internal/cli/backend/profile.go` | (1) `RegisterDefaults()` 增 `Register(codexProfile())`；(2) `Profile` struct 加可选字段 `CredentialCheck func() (ok bool, detail string)`（默认 nil，claude/kiro 不设，行为零变化），供 doctor 通用消费（见 §7.1） | profile.go:232-234（现 claude+kiro）、profile.go:35-115（Profile struct，加字段） |
| `cmd/naozhi/doctor.go` | `renderBackendsSection` 渲染循环加一句通用 `if profile.CredentialCheck != nil { ... }` 消费（**零 `switch backend.ID`**），渲染 codex 凭据行；env/目录都缺显示 warn 不 fail（见 §7.1）。**从 v1 的"不动"挪到此处** | doctor.go:308-355（Profile 驱动渲染循环，无凭据钩子）、doctor_checks.go:106（checkAuth 是 dashboard token，非可复用先例） |
| `internal/wireup/history_backends.go` | 加一行 blank import `_ ".../internal/history/codexjsonl"`（**取代** v1 错写的 main.go） | history_backends.go:25-26（kiro 先例） |
| `internal/cli/history.go` | `HistoryWiring` struct 加 `CodexSessionsDir string` | history.go:68-82（ClaudeDir/KiroSessionsDir/EventLogDir 三字段先例） |
| `internal/session/router_core.go` | `RouterConfig` 加 `CodexSessionsDir`；router struct 加 `codexSessionsDir`；构造时 `codexSessionsDir: cfg.CodexSessionsDir` | router_core.go:697-717（RouterConfig）、817-822（构造点 `kiroSessionsDir: cfg.KiroSessionsDir`） |
| `internal/session/router_lifecycle.go` | 构造 `cli.HistoryWiring{...}` 时加 `CodexSessionsDir: r.codexSessionsDir` | router_lifecycle.go:119-123（HistoryWiring 构造块） |
| `cmd/naozhi/main.go` | RouterConfig 填 `CodexSessionsDir: osutil.ExpandHome("~/.codex/sessions")`（**仅目录路径，不注册 factory**） | 仿 main.go 现有 `KiroSessionsDir:` 赋值 |
| `internal/cli/event.go` | (1) 承载 codex token 新增字段（见 §6.6 (1) 三选一）；(2) 反向请求 method 名复用现有 `Event.SubType`（**无需新增字段**，见 §6.6 (2)）。**非"基本不动"** | event.go:536-545（SendResult）、105-150（EventMetadata/TaskUsage）、15（Event.SubType 现有字段） |
| `docs/rfc/README.md` | **更新** README.md:23 现有 codex 行（**非新增行**，该行已存在）：状态串 `设计提案 Draft v1（未实测）` → `Ready for implementation v2（源码核对；运行时实测待跑）`，日期与范围描述据实微调（v2 已落实，见本轮终稿） | README.md:23（现有 codex 行） |
| `docs/rfc/multi-backend.md` | §8 Dashboard 差异化规约表追加 codex 行（chip 色、cost 单位 tokens、能力 map） | — |

### 4.3 不动

- `internal/cli/protocol_acp.go` —— 不复用，不污染（仅借鉴字段名，见 §6.5）
- `internal/cli/process.go` —— Protocol 接口已经是抽象边界
- `internal/config/config.go` —— **零代码改动**（codex 不默认开启，需 `cli.backends` 显式列；纯文档说明）。证据：config.go:1288-1342。
- `internal/discovery/proc_*.go` —— `DetectInProc` 通过 Profile 注入（实参是 basename，见 m1）
- Dashboard 前端 —— `Features` map 驱动，不写 codex 专属分支

> **注**：`cmd/naozhi/doctor.go` 的 proto/caps/history/reverse-cap 行确实 Profile 驱动自动继承（注册 codexProfile 即出现），但**凭据存在性检查不能自动继承**——`renderBackendsSection`（doctor.go:308-355）是纯 `for _, b := range cfgBackends` 循环、无 per-backend 凭据钩子，现有 `checkAuth()`（doctor_checks.go:106）只查 dashboard token。故 doctor.go **已从 v1 的"不动"挪入 §4.2 修改清单**（加一句通用 `CredentialCheck` 消费，零 `switch backend.ID`），接线方案见 §7.1。

---

## 5. Profile 字段填充（七键最终取值）

参考 `internal/cli/backend/profile_kiro.go` 同形，Profile struct 字段已核对 profile.go:35-115。

| 字段 | 最终取值 | 依据 |
|---|---|---|
| `ID` | `"codex"` | |
| `DisplayName` | `"codex"`（claude 用全称、kiro 用短名，无硬规） | profile_kiro.go:20 |
| `DefaultBinary` | `"codex"` | npm `@openai/codex` bin 名 |
| `DefaultTag` | `"cdx"`（reply prefix，任意短串，与 cc/kiro 对齐） | profile_kiro.go:22 |
| `ChipColor` | `"#10a37f"`（OpenAI 绿，与 claude 紫 `#7c5cff`/kiro 橙 `#ff7a3a` 区分） | naozhi 自定（**取值非取自 kiro**；profile_kiro.go:23 只是字段模板出处，kiro 实际是橙色 `#ff7a3a`） |
| `NewProtocol` | `func(_ ProtocolDeps) cli.Protocol { return &cli.CodexProtocol{BackendID:"codex"} }` | profile_kiro.go:24-29（ProtocolDeps 不消费；BackendID 是 metric label） |
| `DetectInProc` | `func(cmdline string) bool { return strings.Contains(cmdline, "codex") }` | profile_kiro.go:30-32；**实参是 basename**，无需排除子命令（m1）。可仿 claude 排除其他 backend 名 |
| `RequiredNodeCaps` | `[]string{"codex-app-server"}`（命名取舍，对齐 kiro `acp` 风格） | profile_kiro.go:33 |
| `HistoryDir` | `"~/.codex/sessions/"`（doctor 显示串；实际遍历是日期分桶，见 §8） | rollout/src/lib.rs:24 |
| `CostUnit` | `"tokens"`（naozhi 自定，token 数显示） | profile.go:88-97 |
| `Features` | 见 §5.1 七键 | profile_kiro.go:51-59 |

注册：`profile.go` 的 `RegisterDefaults()` 加 `Register(codexProfile())`（profile.go:232-234）。

### 5.1 Features 七键（phase1 取值，精确 true/false 标"运行时实测待定"）

`askuser`/`passthrough`/`embedded_context`/`image_input`/`audio_input`/`mcp_http`/`mcp_sse`（dashboard 硬编码稳定 key，参 profile_kiro.go:51-59）。源码侧线索 + phase1 保守取值：

```go
Features: map[string]bool{
    "askuser":          false, // 走 item/tool/requestUserInput 反向请求，phase2 卡片化
    "passthrough":      false, // 无 stdin replay 回显 → 强制 collect 模式
    "embedded_context": false, // phase1 保守
    "image_input":      true,  // UserInput 接受 Image/LocalImage（见 §6.7 落地约束）
    "audio_input":      false, // realtime audio 全标 experimental
    "mcp_http":         true,  // phase1 乐观；某些 environment 可能禁用，实测确认
    "mcp_sse":          false,
},
```

> **精确值须运行时实测确认**（不能仅凭源码定，见 §10 V_*）。源码只给方向：image_input=true（turn/start 接受 image item）、audio=experimental、askuser 走反向请求、passthrough=false（无 replay）。

---

## 6. CodexProtocol 实现要点

### 6.1 必须实现的 11 个方法（对齐真实 `Protocol` 接口，已核对 protocol.go:69-141）

```go
Name() string                                                          // 返回 "codex"
Clone() Protocol                                                       // 有状态（字段集见 §6.5.1：threadID/activeTurnID/nextID/缓冲/pendingReverseReq），须返新实例
BuildArgs(opts SpawnOptions) []string                                  // ["app-server", "-c","model=...", ...]（见 §6.4）
Init(rw *JSONRW, resumeID string, cwd string) (sessionID string, err error)  // 见 §6.2
WriteMessage(w io.Writer, text string, images []ImageData) error       // → turn/start，input:Vec<UserInput>
WriteUserMessageLocked(w io.Writer, uuid, text string, images []ImageData, priority string) error  // 降级回退 WriteMessage（codex 无 uuid 回显/priority）
SupportsPriority() bool                                                 // return false
SupportsReplay() bool                                                   // return false（无 stdin replay 回显 → Collect 模式）
WriteInterrupt(w io.Writer, requestID string) error                    // → turn/interrupt{缓存的 threadId,turnId}；未起 turn 返 ErrInterruptUnsupported（见 M11）
ReadEvent(line string) (events []Event, done bool, err error)          // 见 §6.3
HandleEvent(w io.Writer, ev Event) (handled bool)                      // 按 ev.SubType（=反向请求 method 名）分派异构响应回包（见 §6.6 (2) / §6.3 / §10）
```

- 可选实现 `Capabilities() Caps`（字段名已核对 protocol.go:262-282）：`Caps{Replay:false, Priority:false, SoftInterrupt:true, StreamJSON:false}`。`SoftInterrupt=true` 因 turn/interrupt 是握手后的安全软取消（同 ACP，参 protocol_acp.go:538-540）。
- 建议（非强制）实现 `eventReaderInto.ReadEventInto(line string, buf []Event)`（分配优化，protocol.go:152-154；ACP 实现见 protocol_acp.go:549-555）。
- 编译期约束：`Protocol = ProtocolCore + ProtocolPassthroughExt`（protocol.go:242-245），方法签名一字不能差。

### 6.2 Init 控制流（对齐 turn/start 立即返回的真相）

1. 发 `initialize` request → 用 `readUntilResponse` 同步等 result（取 `codexHome` 等）。
2. 发 `initialized` notification（无 id，**不等回**，server 不回它，见 m3）。
3. `resumeID!=""` 走 `thread/resume{threadId:resumeID}`，否则 `thread/start{cwd, model?, approvalPolicy:never, sandboxPolicy:workspace-write}`。
4. 从 `response.thread.id`（`ThreadStartResponse{thread:Thread,...}` / `ThreadStartedNotification`）取 threadId，作为返回的 `sessionID`，并存入 protocol 实例字段。
   - 证据：`v2/thread.rs:153`（`ThreadStartResponse{thread:Thread, model, ...}`）；`thread_data.rs:136`（`Thread.id`）。
- **关键控制流定调（设计决策，RFC 拍板=采纳）**：Init 之后，`WriteMessage` 发 `turn/start` 后**不阻塞等 turn 结束**——turn/start 的 response（初始 Turn 对象，status=inProgress）可**同步读一次**以拿 `turn.id` 存起来（供 interrupt/steer），随后由 readLoop 经 `ReadEvent` 等 `turn/completed` 通知收口 `done=true`。即：**握手期用 `readUntilResponse`，turn 期不用**。这正是 codex 与 ACP 的根本差异——ACP "response 即 turn 结束"，codex "turn 由 notification 收口"。

### 6.3 ReadEvent 翻译层（按真实通知形状）

| 真实 method | item.type / 字段 | 映射 | 证据 |
|---|---|---|---|
| `item/agentMessage/delta` | `delta` | assistant 文本增量（累加缓冲，仿 ACP textBuf） | item.rs:1207-1211 |
| `item/started`/`item/completed` 且 `item.type=="agentMessage"` | `item` (AgentMessage.text) | assistant 文本块 | item.rs:1111-1117,1185-1191 |
| `item/started`/`item/completed` 且 `item.type=="commandExecution"/"fileChange"/"mcpToolCall"` | `item.id/command/status` | `cli.Event{ToolCall:{ID,Title,Kind,Status,InputJSON,OutputJSON}}`（仿 ACP tool_call，参 protocol_acp.go:904-945） | item.rs:255-298 |
| `item.type=="reasoning"` / `item/reasoning/textDelta` | — | thinking block（仿 ACP thoughtBuf） | common.rs:1627 |
| `thread/tokenUsage/updated` | `tokenUsage.last/total` | **token usage**（承载见 §6.6） | common.rs:1585；thread.rs:1278-1322 |
| `turn/completed` | `turn.status` | `done=true`；status=interrupted→中断 | turn.rs:386-388；TurnStatus turn.rs:28-34 |
| 各反向请求 method（有 id，见下表 6 种） | — | 返 nil slice + `Event{Type:"permission_request", SubType:<原始 method 名>, RawParams, RPCRequestID}`，交 HandleEvent 按 `ev.SubType` 分派异构响应（见 §6.6 接线缺口）。仿 ACP `permission_request` 单分支（protocol_acp.go:602-607），但 **codex 多 method 必须靠 `SubType` 区分** | common.rs:1422-1486 |
| `serverRequest/resolved`（通知） | `requestId` | 取消挂起的反向请求等待 | common.rs:1614 |
| `error` / `warning` / 未知 method | — | 静默跳过或转 error Event（向前兼容，仿 ACP default 分支） | common.rs:1570 |

### 6.4 BuildArgs / 配置（已核对 v2/turn.rs + config/mod.rs）

```go
func (p *CodexProtocol) BuildArgs(opts SpawnOptions) []string {
    args := []string{"app-server"}
    if opts.Model != "" {
        args = append(args, "-c", "model="+opts.Model)
    }
    args = append(args, "-c", "approval_policy=never")
    args = append(args, "-c", "sandbox_mode=workspace-write")
    return args
}
```

- **cwd 不经 BuildArgs**：`SpawnOptions` **有** `WorkingDir` 字段（wrapper.go:27），但 codex app-server CLI **无接收 cwd 的 flag**，故 cwd 不进 argv；它从 `Protocol.Init(rw, resumeID, cwd)` 的 `cwd` 形参（=`SpawnOptions.WorkingDir`，由 process 在 wrapper.go:644 注入）拿到，在 `thread/start.cwd`（thread.rs:52 `ThreadStartParams.cwd`）里下发。即 cwd 的来源链是 `SpawnOptions.WorkingDir → Init 的 cwd 形参 → thread/start.cwd`，不存在"拿不到 cwd"的问题。
- model/approval/sandbox **优先用 thread/start RPC 参数**（per-thread,更稳；turn/start.approval_policy 标 experimental），`-c` 是次选（全局）。
- approval_policy=`never` 抑制命令/文件审批（失败回模型），立场同 `claude --dangerously-skip-permissions`。
- sandbox_mode 合法值 `read-only`/`workspace-write`/`danger-full-access`（kebab，config/mod.rs:2196）。

### 6.5 可借鉴 ACPProtocol 的真实字段/方法（已核对 protocol_acp.go，供直接引名）

- struct 成员：`mu sync.Mutex`（只护文本缓冲）、`nextID atomic.Int64`+`allocID() int`（acp:962-964）、`sessionID atomic.Pointer[string]`+`storeSessionID`/`loadSessionID`（acp:232-243）、`textBuf`/`thoughtBuf strings.Builder`、`BackendID string`（metric label，acp:188-224）。
- 握手：`sendAndWaitResponse(rw,req)` / `sendAndWaitResponseMsg(rw,req)(*RPCMessage,error)`（acp:966-1004）→ `readUntilResponse(rw, expectedID int)`（goroutine + `time.NewTimer(acpHandshakeTimeout=30s)`，超时 `ErrACPTimeout`，acp:1095-1199）。
- 类型化错误：自定义 `ErrCodexRPC`/`ErrCodexTimeout`（仿 acp:173-179 `ErrACPRPC`/`ErrACPTimeout`），复用 `ErrInterruptUnsupported`（protocol.go:11）。
- RPC 线格式注意：naozhi `RPCRequest` struct 带 `JSONRPC string` 字段（ACP 恒填 "2.0"，acp:316），codex "neither send nor expect" —— CodexProtocol **应自定义不含 JSONRPC 字段的请求结构**（或实测确认 codex 宽松忽略，见 §10 V_jsonrpc）。
- 反向请求响应 id 透传：用 `json.RawMessage` 原样回传 server 给的 id（仿 acp:730 `permissionResponse.ID json.RawMessage`，覆盖 String|Integer）。
- **不借鉴**：pendingResponses map / waitForResponse / 协议级 wMu（**均不存在**，见 M10）。codex turn 由 notification 收口，更接近"fire-and-stream"。

#### 6.5.1 CodexProtocol struct 字段表（minor：v1 未给完整字段集 + 并发保护，开工者无需问作者）

§6.2 第 4 步说"thread.id 存入 protocol 实例字段"、M11 说"WriteInterrupt 用内部缓存的 thread_id+turn_id"，但 v1 没列全字段集与跨 goroutine 保护。完整字段表（仿 ACP 的 split-out 模式）：

| 字段 | 类型 | 读写者 / 并发策略 | 仿照 |
|---|---|---|---|
| `BackendID` | `string` | 构造期写、只读（metric label） | acp:188-224 |
| `sessionID` | `atomic.Pointer[string]` | Init 后写、ReadEvent/查询读 | acp:232-243（`storeSessionID`/`loadSessionID`） |
| `threadID` | `atomic.Pointer[string]` | Init/resume 后写、WriteMessage/WriteInterrupt 读 | 仿 sessionID 的 split-out（与 sessionID 同值，但语义分开记，resume 复用） |
| `activeTurnID` | `atomic.Pointer[string]` | **跨 goroutine**：WriteMessage（turn/start response 拿到 turn.id）写、WriteInterrupt 读、ReadEvent 在 `turn/completed` 时清空（存 nil） | **必须用 `atomic.Pointer[string]`**（仿 sessionID split-out，protocol_acp.go:205 注释），避免与 `textBuf` 的 `mu` 争用 |
| `nextID` | `atomic.Int64` + `allocID() int` | 各发请求处并发自增 | acp:962-964 |
| `textBuf` / `thoughtBuf` | `strings.Builder` | readLoop 单 goroutine 累加，`mu` 护 | acp:188-224 |
| `mu` | `sync.Mutex` | **只护 `textBuf`/`thoughtBuf` 文本缓冲**，不护 id（id 走 atomic） | acp（mu 只护文本缓冲） |
| `pendingReverseReq` | `map[string]struct{}` + 其 `sync.Mutex`（或并发安全 map） | ReadEvent 收反向请求时登记、HandleEvent 回包后删、`serverRequest/resolved` 通知到达时删（取消挂起） | 无 ACP 直接先例（ACP 单 method 不需要），codex 多反向请求 + out-of-band resolve 需要 |

> **关键并发点**：`activeTurnID` 被三个不同 goroutine 触碰（WriteMessage 写 / WriteInterrupt 读 / ReadEvent 清），**必须 `atomic.Pointer[string]`**，不能塞进 `mu` 护的区域（否则 interrupt 读会和文本累加争锁）。`stdin` 写锁不在 CodexProtocol 内——复用 `Process.shimWMu`（process.go，同 ACP，见 M10），CodexProtocol 自身不持 stdin 写锁。

### 6.6 event.go 改动面（M7/M14 后果：token 承载 + 反向请求 method 名承载）

M14 说"event.go 非基本不动"涉及**两处**新增，phase1 都要落：

**(1) token usage 承载（RFC 须二选一/三选一）**

Event 现有 `CostUSD`（event.go:19）、`TaskUsage{TotalTokens,...}`（event.go:145-147）、`EventMetadata.MeteringUsage[]{Value,Unit,UnitPlural}`（event.go:119-125），无 input/output/cached/reasoning 分项。三个候选：
- **(a)** 新增 `SendResult.InputTokens/OutputTokens/CachedInputTokens/ReasoningOutputTokens`（+ Event 对应字段）；
- (b) 复用 `EventMetadata.MeteringUsage`（语义是 metering 非 token-split，勉强）；
- **(c)** 新增 `EventMetadata.TokenUsage` 子结构（最干净，含 reasoning/total，对齐 `TokenUsageBreakdown` 五字段 totalTokens/inputTokens/cachedInputTokens/outputTokens/reasoningOutputTokens）。

**推荐 (c)**（最贴 codex 五字段语义，且 dashboard 已有 EventMetadata 渲染管线）；其次 (a)。三者都要改 event.go。

**(2) 反向请求 method 名承载（major：v1/M12 未点破的接线缺口，无需新增字段）**

ACP 只处理一种反向 method（`session/request_permission`），靠 `ev.Type=="permission_request"` 单分支即可分派（protocol_acp.go:773）；但 codex 有 **6+ 种异构反向 method**（见下表），`HandleEvent(w, ev)(handled bool)` 单签名里 `ev` 只带 `RawParams`+`RPCRequestID`，**不带"这是哪个 method"**。若不把 method 名传进去，`HandleEvent` 无法分派异构响应体。

**接线方案（采纳，零新增字段）**：`ReadEvent` 翻译反向请求时把**原始 method 名塞进现有 `Event.SubType`**（event.go:15，`json:"subtype"` 字段，现仅 claude system 帧用），`HandleEvent` 按 `switch ev.SubType` 分派。`Event.SubType` 已存在、当前不与 codex 任何路径冲突，故**不需要给 Event 加新字段**——这是 (2) 比 (1) 轻的原因。

| 反向请求 method（`ev.SubType` 取值） | 响应体（异构） | decision/字段枚举 |
|---|---|---|
| `item/commandExecution/requestApproval` | `{decision}` | `accept`/`acceptForSession`/`acceptWithExecpolicyAmendment{...}`/`applyNetworkPolicyAmendment{...}`/`decline`/`cancel`（item.rs:48-66，已 curl 核对） |
| `item/fileChange/requestApproval` | `{decision}` | `accept`/`acceptForSession`/`decline`/`cancel`（item.rs:94-102，已 curl 核对） |
| `item/permissions/requestApproval` | `{permissions, scope, strictAutoReview?}` | 异构，非 decision |
| `item/tool/requestUserInput`（EXPERIMENTAL） | `{answers}` | 异构 |
| `mcpServer/elicitation/request` | `{action, content}` | 异构 |
| `item/tool/call`（DynamicToolCall）/ legacy `applyPatchApproval`/`execCommandApproval` | 见 §2.5 | 异构 |

> phase1 自动兜底：命令/文件审批回 `{decision:"accept"}`（approval_policy=never 下通常不触发，但 `item/permissions/requestApproval` 与 `mcpServer/elicitation/request` 与 never 无关、仍可能触发，见 §9.3/V_never），异构的 permissions/elicitation/requestUserInput 回**最小放行/取消体**防悬挂（具体取值实测 V_自动授权 后定）。**单测验收**（§10.1）须覆盖"每种 approval method 回对应 decision/异构体"，按 `ev.SubType` 分派。

### 6.7 图片输入（v1 未覆盖缺口；按 Attachment.Kind 分两支）

codex `UserInput` 只接受 `Image{url}` / `LocalImage{path}`（turn.rs:289-300，path 类型为 `PathBuf`，已 curl 核对），**不接受内联 base64**。naozhi `WriteMessage(images []ImageData)` 的 `ImageData` 是 `Attachment` 别名（event.go:325），有 **`Kind` 字段**（`KindImageInline`/`KindFileRef`，event.go:296-297）。两类附件落地路径**不同**，v1 把所有图片都当内联字节走临时文件是多余的：

- **`KindFileRef`（如 PDF、已写入工作区的文件）**：`Attachment.WorkspacePath` **已是工作区内的磁盘路径**（event.go:319，project-root 相对、forward slashes）。直接拼 `LocalImage{path: <工作区内可达路径>}` 即可——文件已在 codex 的 `cwd` 下，**无需再落临时文件、无生命周期/清理问题**。
- **`KindImageInline`（内联字节）**：`Attachment.Data []byte`（event.go:311）才需要**先落地成本地临时文件**再传 `LocalImage{path}`，临时文件生命周期/清理归属见 §9.2 G3（**G3 范围收窄到仅此一支**）。

**phase1 取值对齐**：若 phase1 只想实现 file_ref 这条无生命周期负担的路径，可声明"phase1 仅 `KindFileRef` 图片走 `LocalImage`，`KindImageInline` 内联字节 phase2"，并把 `Features.image_input` 的 phase1 真值与此对齐（见 §5.1 已标"运行时实测待定"）。这样 G3 的临时文件清理问题在 phase1 可完全避开。

---

## 7. 鉴权

Codex CLI 鉴权路径（源码核对，部分待 re-pin）：
- **环境变量两个二选一**：`OPENAI_API_KEY`（主）与 `CODEX_API_KEY`（login/src/lib.rs:28,36）。doctor 检查须接受**任一**。
- **持久化凭据文件**：`CODEX_HOME/.credentials.json`（config/mod.rs:814；⚠️ 与 v1 写的 `auth.json` 不一致，auth 模块已重组，精确文件名 Phase 0 re-pin，见 §9 已收敛为结论 + §11 待 re-pin 细节）。`CODEX_HOME` 默认 `~/.codex`（config/mod.rs:861,1226）。
- **Amazon Bedrock 原生支持**（结论，见 §2.7 M13）：codex 支持 `AuthMode::BedrockApiKey`，naozhi 部署机已走 Bedrock claude，**可探索复用现有 Bedrock 凭据体系**接 codex。具体 CLI/env 注入入口与 region 配置 Phase 0 re-pin。

naozhi 落地：
- 部署文档增加"如启用 codex backend，需 export `OPENAI_API_KEY` 或 `CODEX_API_KEY`，或预先 `codex login`，或探索 Bedrock 路径"。

#### 7.1 doctor 凭据检查接线（major：补齐"这段新代码放哪、怎么接"）

`renderBackendsSection`（doctor.go:308-355）是纯 Profile 驱动的 `for _, b := range cfgBackends` 渲染循环，**没有 per-backend 凭据钩子**，且现有 `checkAuth()`（doctor_checks.go:106）只查 dashboard token（与 backend CLI 凭据无关）。要在不破坏 `profile.go` 包头"消灭 `switch backend.ID`"哲学的前提下接入 codex 凭据检查，**采纳 Profile 驱动方案**（不写 `if id=="codex"` 专属分支）：

1. **给 `backend.Profile` 加可选字段** `CredentialCheck func() (ok bool, detail string)`（默认 `nil`；claude/kiro 不设，保持现状）。
2. `codexProfile()` 设 `CredentialCheck: codexCredentialCheck`，其逻辑：`OPENAI_API_KEY` **或** `CODEX_API_KEY` env 非空 **或** `~/.codex`（`CODEX_HOME`）目录存在且非空 → `ok=true`；都缺 → `ok=false, detail="set OPENAI_API_KEY/CODEX_API_KEY or run codex login"`。**探测目录/env 存在性，不硬编码凭据文件名**（精确文件名待 re-pin，见 §9.2 G6）。
3. `renderBackendsSection` 循环里加一句通用消费：`if profile.CredentialCheck != nil { ok, detail := profile.CredentialCheck(); 渲染 "credentials: ok/warn(detail)" 一行 }`。这是"加一段 Profile 驱动消费"，**零 `switch backend.ID`**，与 §4.3 "Profile 驱动自动继承"叙述自洽。
4. **doctor 不应因凭据缺失 fail**：env/目录都缺时显示 `warn`（不阻塞启动，因为操作者可能用 Bedrock 路径或运行时再 login），仅作可见性提示。

> 因此 `cmd/naozhi/doctor.go` 与 `internal/cli/backend/profile.go` 都要**少量改动**（加字段 + 加通用消费分支），不再属于 §4.3 "不动"。已在 §4.2 修改清单补两行。Bedrock 路径下 `~/.codex` 可能为空但仍能鉴权，故 warn 而非 fail 也覆盖了这种情况。

不在本 RFC 范围：自建凭据池、企业网关代理、按 session 切 key。

---

## 8. codexjsonl 历史 source

### 8.1 实现要点（比 kiro 复杂，规模重估 ~300+ 行）

- **不能** `filepath.Join(root, sid+".jsonl")`（kiro 扁平写法，参 kirojsonl/source.go:178）。须**递归遍历 `YYYY/MM/DD` 日期分桶**，按文件名 `rollout-<ts>-<uuid>.jsonl` 匹配 threadId（recorder.rs:1416-1430）。
- **透明 gzip 解压**：文件可能压缩（格式/后缀 Phase 0 re-pin），探测 `.jsonl` vs 压缩后缀。
- 归档在**独立目录** `~/.codex/archived_sessions`（lib.rs:25），phase1 可只读活跃 `sessions/`。
- 每行 `{timestamp(RFC3339 UTC,ms), type, payload}` 按 type 分派；首行 `session_meta` 含 id/timestamp/cwd 等。user/assistant 优先取 event_msg 的 `user_message`/`agent_message`（纯文本最简）；token 取 event_msg 的 token_count 行；turn 完成认 **`task_complete`（v1 名）与 `turn_complete`（v2 alias）两者**。逐行 record→`cli.EventEntry` 映射详见 §9 G5（须读 rollout/protocol crate record 定义补全）。
- 未知 type 静默跳过（逐行容错，与 kiro/claude 一致，参 kirojsonl/source.go:321-327 default 分支）。

### 8.2 threadId→文件定位算法（major：v1 漏了"从 threadId 找到那个文件"这第一步）

kiro 把 `s.SessionID`（返回当前 sessionID 的闭包）直接喂 `New(rootDir, s.SessionID)`，`LoadBefore` 里 `filepath.Join(rootDir, sid+".jsonl")` **一步定位**（kirojsonl/source.go:178）。codex 文件名是 `rollout-<本地ISO时间戳>-<threadId>.jsonl` 且埋在 `YYYY/MM/DD` 三级分桶（§8.1），所以 codexjsonl 的 `LoadBefore(ctx, beforeMS, limit)` 收到 `sid`（=threadId，由 `thread/start` 返回、Init 透传）后，**必须先查找到对应文件**，再做 §8.1/G5 的逐行映射。查找策略（phase1 采纳）：

1. **遍历 `<sessionsDir>/YYYY/MM/DD`，匹配文件名后缀 `-<threadId>.jsonl` 或 `-<threadId>.jsonl.<gz后缀>`**（压缩后缀待 re-pin，见 G7），命中即停。threadId 是 UUID，后缀匹配足够唯一。
2. **性能兜底**：日期分桶**按目录名倒序遍历**（年→月→日 desc），新 session 通常在最近日期，可早停；若需更强保证，首次构造 Source 时**建一次 `threadId→path` map 缓存**（一次性 `filepath.WalkDir`），后续 `LoadBefore` 直接查表。phase1 可先用倒序遍历早停的朴素实现，缓存留作优化。
3. **找不到文件**返回 `nil, nil`（空历史，非错误），与 kiro `os.ErrNotExist` 分支一致（kirojsonl/source.go:180-182）。
4. **不读 `archived_sessions/`**（独立目录，phase1 只读活跃 `sessions/`）。

> 这步**纳入 §10.1 fixture 单测验收**：伪造 `sessions/2026/06/18/rollout-2026-06-18T...-<threadId>.jsonl`，断言 `LoadBefore` 能用 threadId 命中该文件并读出 entries；再伪造一个不匹配 threadId 的文件，断言不被误读。`<threadId>` 与文件名中段时间戳无关——**只认尾部 `-<threadId>.jsonl` 后缀**，不要试图从时间戳反推。

### 8.3 走自建文件解析,不走 RPC（结论,源码层定论)

**裁决：走自建文件解析**（v1 §9.2 列为"实测 V11 后定",本 v2 收敛为已决）。理由三条且全带证据：
1. RPC（thread/list 等）本质也是读同一批 jsonl（recorder.rs:1 自述 "Persist Codex session rollouts (.jsonl) so sessions can be replayed or inspected later"），不提供额外真相；
2. naozhi `history.Source` 契约是纯读/无状态/进程可不在（`HistorySource.LoadBefore`，history.go:88-89），RPC 反需 live app-server；
3. `thread/turns/list` 仍标 `#[experimental("thread/turns/list")]`（common.rs:637-638）。

列举加速可 phase2 选稳定的 `thread/list`。

### 8.4 factory / Source 契约（已核对 history.go + kirojsonl）

- factory 签名 `func(s cli.HistorySessionView, deps cli.HistoryWiring) cli.HistorySource`（history.go:107）；`deps.CodexSessionsDir==""` 时返 `cli.NoopHistorySource{}`（仿 kirojsonl/source.go:93-98）。`s.SessionID`（=threadId 闭包）传入 Source，供 §8.2 定位用。
- Source 实现单方法 `LoadBefore(ctx, beforeMS, limit) ([]EventEntry, error)`（history.go:88-89）。
- 注册：包 `init()` 调 `cli.RegisterHistoryFactory("codex", factory)`（仿 kirojsonl/source.go:86-88）+ `wireup/history_backends.go` blank import。

---

## 9. 风险与未决项

### 9.1 已收敛为结论（v1 曾列为未决，v2 源码核对后定论）

1. **历史 source 走文件 vs RPC** → **走文件**（§8.3，源码层定论，无需等 V11）。
2. **reverse-cap 命名 `codex-app-server` vs `codex`** → **`codex-app-server`**（§5 m12，纯 naozhi 内部约定，对齐 kiro `acp` 风格；与 codex 进程零字符串耦合，子节点 caps 由 `derivedCaps()` 并集自动生成）。
3. **config key 真名** → `model`/`approval_policy`/`sandbox_mode` **全部确认**（§2.7，config/mod.rs:322,2196）。
4. **Cost 列要不要现场换算 USD** → **不换**，dashboard 显示 "N tokens"（CostUnit=tokens）；如要换需引入 model price 表，超出本 RFC。
5. **凭据/env** → 两个 env 二选一（OPENAI_API_KEY/CODEX_API_KEY）+ Bedrock 原生支持（§7）。

### 9.2 仍为未决（标注清楚）

| 项 | 类型 | 说明 |
|---|---|---|
| **G1. CodexProtocol turn 控制流** | 设计决策（本 RFC §6.2 已给推荐裁决=采纳） | Init 后 turn/start 同步读一次 response 拿 turn.id 再交 readLoop；如实测发现 turn/start response 与 turn/started 通知有竞态,可改纯依赖通知。 |
| **G2. token usage 落地结构** | 设计决策（§6.6 推荐 (c)） | (a)/(b)/(c) 三选一,影响 event.go diff 面。 |
| **G3. 内联图片临时文件方案** | 实现细节（§6.7，**范围已收窄**） | **仅 `KindImageInline` 内联字节**需落临时文件（`KindFileRef` 直接用 `WorkspacePath`，无此问题）;临时文件生命周期/清理归属未定;phase1 可只做 file_ref，inline 留 phase2。 |
| **G4. thread/tokenUsage/updated 精确 JSON 样例** | 待实测/schema 对照 | 字段 camelCase 已从源码确认,线上嵌套样例未抓;写 ReadEvent 前先 `generate-json-schema` 对齐。 |
| **G5. codexjsonl 每行 record→EventEntry 逐字段映射** | 待补 | 路径布局已确证,SessionMeta/response_item 逐 kind 映射须读 rollout+protocol crate record 定义。 |
| **G6. 凭据文件精确名 + Bedrock 注入入口** | 待 re-pin | auth 模块已自上次研究重组(本轮 `bedrock_api_key.rs`/`auth.rs` 路径 404,config/mod.rs:814 提示 `.credentials.json`)。doctor 用目录/env 存在性兜底,不阻塞 Phase 1。 |
| **G7. 落盘 gzip 格式/后缀** | 待实测/re-pin | 压缩后缀须读 rollout 压缩逻辑或实测;codexjsonl 须透明解压。 |

### 9.3 风险

| 风险 | 等级 | 缓解 |
|---|---|---|
| `app-server` 标 `[experimental]`,扩展 method 需 `experimentalApi` | M | phase1 只依赖稳定基础链路(§0/§1.2);用 `generate-json-schema` 锚定 schema 版本;订阅 codex release notes |
| 多 thread 共享一个 app-server 进程的并发隔离质量未知 | M | phase1 按 1 thread/进程,跟 claude 对齐;实测 V8 后 phase2 优化 |
| naozhi 多发 `jsonrpc:"2.0"` 字段,codex 是否报错 | M | 自定义不含 JSONRPC 字段的请求结构(§6.5);或实测 V_jsonrpc 确认宽松忽略 |
| `never` 是否全屏蔽反向请求(MCP elicitation 与 approval_policy 无关) | M | phase1 自动兜底**必须覆盖 permissions/elicitation 两类**,不假设 never 全屏蔽(见 §10 V_never) |

---

## 10. 实施分期

> **核心原则**：协议 wire 形状已源码核对并 pin 版本,**Phase 1 编码可在无 codex 环境开工**;运行时实测是**独立阶段(需 codex 环境)**,验证而非阻塞 Phase 1 起步。

| Phase | 范围 | 前置 | 工时估计 |
|---|---|---|---|
| **运行时实测(需 codex 环境)** | 跑 V1–V12(见 §10.1),写 `codex-backend-validation.md`;schema 生成对照;re-pin G4/G6/G7 | 一台装有 codex CLI 的机器 | 0.5–1 day |
| **Phase 1** | `protocol_codex.go` + `profile_codex.go` + 单测;`codexjsonl` 历史 source + 四点接线链;本 RFC 维持 ready | **仅需源码核对(已完成)**;可与"运行时实测"并行 | 2–3 days |
| **Phase 2** | Dashboard chip/cost/Features 接入;反向请求 → AskUserQuestion 卡片;`turn/steer` 接 `/urgent`;列举走 thread/list | Phase 1 + 实测放开 Features 精确值 | 1–2 days |
| **Phase 3** | 生产灰度:测试群 enable codex,跑一周观察 metric/日志/反馈 | Phase 2 | 1 week |

### 10.1 Phase 1 在"仅源码核对"前提下的开工边界与验收标准

**可立即开工(无 codex 也能写+单测通过)**：
- `protocol_codex.go` 全部 11 方法骨架 + 表驱动单测(fixture 用本 RFC §2 的真实 wire 形状构造 NDJSON 行)。
- `profile_codex.go` 七键(§5) + 注册;`TestProfile_Codex_NewProtocol` 仿 `TestProfile_Kiro_NewProtocol`。
- `codexjsonl/source.go` 日期分桶遍历 + 行解析 + fixture 单测(用伪造的 `sessions/2026/06/18/rollout-*.jsonl`)。
- 四点接线链(§4.2)+ `go build ./...` 编译通过 + `internal/cli` 全测过(编译期 `Protocol = ProtocolCore + ProtocolPassthroughExt` 约束自动校验签名)。

**Phase 1 验收标准(无 codex)**：
1. `go build ./...` + `go vet ./...` 通过;`go test ./internal/cli/... ./internal/history/codexjsonl/... ./internal/cli/backend/...` 全绿。
2. 单测覆盖:握手(initialize→initialized→thread/start)、turn 立即返回 + `turn/completed` 通知收口 done=true、`thread/tokenUsage/updated` 映射、interrupt 拼双 id、**每种 approval method 按 `ev.SubType` 分派对应 decision/异构响应体**(命令 accept/decline/cancel、文件 accept/decline/cancel、permissions 回 permissions+scope、elicitation 回 action+content)、`serverRequest/resolved` 取消挂起、id String/Integer 兼容、未知 method 容忍。
3. `cmd/naozhi/doctor` 注册 codexProfile 后自动出现 codex backend 行(proto/caps/history/reverse-cap),**且 `CredentialCheck` 渲染一行 codex 凭据状态:env/目录都缺时显示 warn(不 fail)**(见 §7.1)。
4. fixture 驱动的 codexjsonl:(a) **能用 threadId 从日期分桶 `sessions/2026/06/18/rollout-...-<threadId>.jsonl` 命中文件**(§8.2),不匹配的 threadId 文件不被误读;(b) 命中后能读出 user/assistant entries。

**必须等运行时实测才能定稿/放开的(Phase 1 用保守默认占位)**：
- Features 精确 true/false(§5.1 先保守);token usage 通知到达时机/频率(先按"每次到达即覆盖 last,turn/completed 时取最新");never 下反向请求边缘触发(先一律自动兜底 accept/decline 防悬挂);jsonrpc 字段容忍度(先自定义不含 JSONRPC 字段的请求结构)。

### 10.2 运行时实测项 V1–V12(需 codex 环境,本机无法跑)

| 验证点 | 脚本/动作 | 通过条件 |
|---|---|---|
| V1 启动 | `codex app-server`(stdio) | 进程常驻,stdin 可写 |
| V2 握手 | 发 initialize → 收 InitializeResponse → 发 initialized → 发 thread/start | thread/start 返回 `thread.id`;未握手调他法得 -32600 |
| V3 单 turn | thread/start + turn/start "echo hi" | turn/start **立即返回** inProgress;随后收 `turn/completed` 通知,assistant 文本含 "hi" |
| V4 流式 | 长 prompt | `item/agentMessage/delta` 数 >= 5,`delta` 字段累积成全文 |
| V5 工具调用 | "list files" | 收到 `item.type=="commandExecution"` 的 item/started+item/completed |
| V6 interrupt | 长 prompt 中途发 `turn/interrupt{threadId,turnId}` | 收 `turn/completed`,`turn.status=="interrupted"` |
| V7 resume | thread/start → kill → 新进程 `thread/resume{threadId}` | context 恢复 |
| V8 多 thread | 一进程开 N thread | context 互不污染 |
| V9 backpressure | 高频 turn/start 灌入 | 至少触发一次 `code==-32001` |
| V10 反向请求 | 触发命令执行 approval | HandleEvent 回 `{decision:"accept"}` 后命令执行;确认 server 接受 |
| V11 持久化 | turn 后查 `~/.codex/sessions/YYYY/MM/DD/` | 文件存在(可能 gzip)且可被 codexjsonl 读取 |
| V12 schema | `codex app-server generate-json-schema --out /tmp/x` | 文件生成且 JSON 合法;与本 RFC §2 wire 形状一致 |

**实测专项(影响 Phase 2 放开,本机无 codex)**：
- **V_tokenUsage 时机**:每 item 后增量推 vs turn 末一次?turn/completed 前是否必然到达一次?(影响是否需 client 累加 last/total。)
- **V_turn/completed 频率**:是否每 turn 恰好一次?interrupt/failed 路径仍发 turn/completed 还是改发 error?(影响 done 判定。)
- **V_never 全屏蔽**:命令/文件审批 never 下不弹,但 `item/permissions/requestApproval` 与 `mcpServer/elicitation/request`(MCP 驱动,与 approval_policy 无关)是否仍触发?**phase1 自动兜底必须覆盖这两类**。
- **V_自动授权 decision**:命令/文件 decision 枚举不同;permissions/elicitation 响应体异构。phase1 是否一律 `accept`、异构的 decline/cancel 防悬挂?须实测不悬挂。
- **V_jsonrpc 字段容忍度**:多发 `jsonrpc:"2.0"` codex 宽松忽略还是报错。
- **V_落盘 gzip**:压缩格式/后缀。
- **V_Bedrock 注入入口**:CLI flag/env 入口与 region(re-pin G6)。

实测若推翻 §2 任一 blocker 级声明则改写设计;否则 Phase 1 成果直接生效。

---

## 11. 源码核对证据索引（pin commit + 路径速查）

> **版本基准**:协议 wire 形状最初 pin 在 openai/codex@main commit **`c73296a0f095e72dbb646909c613ae09c9459c3a`**(2026-06-18)。naozhi@master `cd7334ec`(工作树有未提交改动)。
> 取数方式:`curl -s https://raw.githubusercontent.com/openai/codex/main/<path>` 直读 .rs 原文 + 本地 Read。
>
> **二次独立核对(终稿编辑轮,2026-06-18)**:为把"单人源码核对"升级为"双人核对"(评审建议),终稿编辑对下列 blocker/major 级声明**重新 curl `raw.githubusercontent.com/openai/codex/main/...` 逐字复核**,均与本 RFC §2 一致:
> - **B1** `TurnStartResponse{turn:Turn}`(turn.rs:157-159,立即返回)、`TurnStatus{Completed,Interrupted,Failed,InProgress}`(turn.rs:29-33)。
> - **B2** `TurnCompletedNotification{thread_id,turn}`(turn.rs:386-389,**grep 无 usage/无 response_id**)、`TokenUsageBreakdown` 五字段 `total_tokens/input_tokens/cached_input_tokens/output_tokens/reasoning_output_tokens`(thread.rs:1312-1322)、`ThreadTokenUsage{...,model_context_window}`(thread.rs:1291-1296)。
> - **B3** `CommandExecutionApprovalDecision`(item.rs:48-66,6 变体 Accept/AcceptForSession/AcceptWithExecpolicyAmendment/ApplyNetworkPolicyAmendment/Decline/Cancel,**Decline=继续 turn、Cancel=中断 turn 的注释逐字确认**)、`FileChangeApprovalDecision`(item.rs:94-102,4 变体);全文无 `allow`/`approve` decision 值。
> - **反向 method 全集** common.rs:1422-1486(`item/commandExecution/requestApproval`/`item/fileChange/requestApproval`/`item/tool/requestUserInput`/`mcpServer/elicitation/request`/`item/permissions/requestApproval`);**通知** `thread/started`(1573)/`thread/closed`(1578)/`thread/tokenUsage/updated`(1585)/`turn/completed`(1588)/`serverRequest/resolved`(1614)。
> - **M2/M6/M11** `ItemStartedNotification{item,thread_id,turn_id,started_at_ms}`(item.rs:1111-1117)、`AgentMessageDeltaNotification{...,delta}`(item.rs:1207-1211,增量字段=`delta`)、`UserInput` 枚举(turn.rs:283-308,含 `LocalImage{path:PathBuf}`)、`TurnInterruptParams{thread_id,turn_id}`(turn.rs:200-205,两 id)。
> - **传输层** jsonrpc_lite.rs:1-2 "neither send nor expect the jsonrpc:2.0 field"、`RequestId` untagged `{String,Integer}`(16-21)。
>
> ⚠️ **核对所用 `main` HEAD 漂移说明**:二次核对当日 `git ls-remote refs/heads/main` 已是 **`56703600091d25542b60597b85d0e027799ad063`**(晚于初 pin 的 `c73296a0`)。上述声明在**新旧两个 commit 上一致**,说明这些核心 wire 形状在该区间稳定——这反而**加强**了可信度。但 `generate-json-schema` 与 §10 运行时实测仍应在**写代码时的实际 codex 版本**上 re-pin(行号可能随版本微移),不要把上面行号当跨版本永久锚点。

### 11.1 codex 协议声明 → 文件:行(每条可回溯)

| 协议声明 | 文件(相对 `codex-rs/`):行 |
|---|---|
| 非真 JSON-RPC,无 jsonrpc 字段 | `app-server-protocol/src/jsonrpc_lite.rs:1-2` |
| RequestId = untagged {String,Integer} | `app-server-protocol/src/jsonrpc_lite.rs:14-22` |
| initialize method | `app-server-protocol/src/protocol/common.rs:467-471` |
| InitializeParams(clientInfo+capabilities) | `app-server-protocol/src/protocol/v1.rs:28-56` |
| InitializeResponse(userAgent/codexHome/platformFamily/platformOs,无 serverInfo) | `app-server-protocol/src/protocol/v1.rs:58-71` |
| initialized 是唯一 client 通知 | `app-server-protocol/src/protocol/common.rs:1668-1670` |
| Thread method 表(80+,无 thread/close) | `app-server-protocol/src/protocol/common.rs:476-652` |
| thread/closed 仅通知 | `app-server-protocol/src/protocol/common.rs:1578` |
| ThreadItem 枚举(plan 非 todo) | `app-server-protocol/src/protocol/v2/item.rs:215-410` |
| TurnStartParams(input:Vec<UserInput>,threadId 必填) | `app-server-protocol/src/protocol/v2/turn.rs:67-130` |
| TurnStartResponse{turn}(立即返回) | `app-server-protocol/src/protocol/v2/turn.rs:157-159` |
| Turn{id,items,items_view,status,error,started_at,completed_at,duration_ms} | `app-server-protocol/src/protocol/v2/thread_data.rs:188-206` |
| TurnStatus{completed,interrupted,failed,inProgress} | `app-server-protocol/src/protocol/v2/turn.rs:28-34` |
| TurnCompletedNotification{thread_id,turn}(无 usage/response_id) | `app-server-protocol/src/protocol/v2/turn.rs:386-388` |
| UserInput 枚举(Text/Image/LocalImage/Skill/Mention) | `app-server-protocol/src/protocol/v2/turn.rs:283-308` |
| thread/tokenUsage/updated 通知 | `app-server-protocol/src/protocol/common.rs:1585` |
| ThreadTokenUsage{total,last,modelContextWindow} | `app-server-protocol/src/protocol/v2/thread.rs:1278-1296` |
| TokenUsageBreakdown(5 字段含 reasoning/total) | `app-server-protocol/src/protocol/v2/thread.rs:1310-1322` |
| ThreadResumeParams{thread_id,history?,path?} | `app-server-protocol/src/protocol/v2/thread.rs:288-320` |
| ItemStartedNotification{item,thread_id,turn_id,started_at_ms} | `app-server-protocol/src/protocol/v2/item.rs:1111-1117` |
| ItemCompletedNotification | `app-server-protocol/src/protocol/v2/item.rs:1185-1191` |
| AgentMessageDeltaNotification{...,delta} | `app-server-protocol/src/protocol/v2/item.rs:1207-1211` |
| thread/started 通知(thread:Thread) | `app-server-protocol/src/protocol/common.rs:1573` |
| 反向请求 method 全集(无 approve) | `app-server-protocol/src/protocol/common.rs:1422-1486` |
| CommandExecutionApprovalDecision(6 变体,Decline/Cancel 语义) | `app-server-protocol/src/protocol/v2/item.rs:45-66` |
| FileChangeApprovalDecision(4 变体) | `app-server-protocol/src/protocol/v2/item.rs:93-102` |
| command/file approval params+response(decision) | `app-server-protocol/src/protocol/v2/item.rs:1309-1390` |
| serverRequest/resolved 通知 | `app-server-protocol/src/protocol/common.rs:1614` |
| turn/interrupt method + TurnInterruptParams{thread_id,turn_id} | `common.rs:811-815` + `v2/turn.rs:200-208` |
| turn/steer + expectedTurnId 必填 | `common.rs:805-810` + `v2/turn.rs:166-194` |
| OVERLOADED_ERROR_CODE=-32001 + 精确文案 + CHANNEL_CAPACITY=128 | `app-server-transport/src/transport/mod.rs:50,237-238,22-24` |
| 标准错误码 -32600/-32601/-32602/-32603/input_too_large | `app-server/src/error_code.rs:3-8` |
| app-server 子命令 [experimental] + listen 默认 + generate-* | `cli/src/main.rs:146-147,514-535,598-601` |
| codex proto 已移除(无 Proto 变体) | `cli/src/main.rs:123-212`(通读 + grep 无命中) |
| approval_policy/sandbox_mode config key | `core/src/config/mod.rs:322,2196` |
| CODEX_HOME 默认 ~/.codex + find_codex_home | `core/src/config/mod.rs:861,1226` |
| 凭据文件 .credentials.json(⚠️ 待 re-pin,与 v1 auth.json 不一致) | `core/src/config/mod.rs:814` |
| 两个 env 常量 OPENAI_API_KEY/CODEX_API_KEY | `login/src/lib.rs:28,36` |
| sessions 日期分桶 YYYY/MM/DD + rollout- 文件名 | `rollout/src/recorder.rs:1416-1430` |
| SESSIONS_SUBDIR / ARCHIVED_SESSIONS_SUBDIR | `rollout/src/lib.rs:24-25` |
| archived_sessions 独立目录 | `rollout/src/recorder.rs:1218` |

### 11.2 naozhi 接线点 → 文件:行(本地核对)

| 接线断言 | 文件:行 |
|---|---|
| Protocol 接口 11 方法签名 | `internal/cli/protocol.go:69-141` |
| Caps struct 字段(Replay/Priority/SoftInterrupt/StreamJSON) | `internal/cli/protocol.go:262-282` |
| ProtocolCaps + Capabilities() opt-in | `internal/cli/protocol.go:290-301` |
| ErrInterruptUnsupported | `internal/cli/protocol.go:11` |
| eventReaderInto.ReadEventInto | `internal/cli/protocol.go:152-154` |
| 编译期 Protocol=Core+PassthroughExt | `internal/cli/protocol.go:242-245` |
| SendResult 字段(无 token 分项) | `internal/cli/event.go:536-545` |
| Event/EventMetadata/TaskUsage/MeteringEntry | `internal/cli/event.go:14-19,105-150` |
| ACPProtocol struct 成员(无 pendingResponses/wMu) | `internal/cli/protocol_acp.go:188-224` |
| readUntilResponse / sendAndWaitResponse(Msg) / allocID | `internal/cli/protocol_acp.go:1095-1199,966-1004,962-964` |
| WriteInterrupt(w, _ string) 忽略 requestID | `internal/cli/protocol_acp.go:488-518` |
| permission_request 反向请求处理 + RawParams/RPCRequestID | `internal/cli/protocol_acp.go:602-607,773-838` |
| permissionResponse.ID json.RawMessage 透传 | `internal/cli/protocol_acp.go:730-733` |
| HistoryWiring{ClaudeDir,KiroSessionsDir,EventLogDir}(无 Codex) | `internal/cli/history.go:68-82` |
| HistorySource.LoadBefore / HistoryFactoryFn / RegisterHistoryFactory | `internal/cli/history.go:88-89,107,134` |
| NoopHistorySource | `internal/cli/history.go:92-100` |
| RouterConfig 三目录字段 + 构造点 | `internal/session/router_core.go:697-717,817-822` |
| HistoryWiring 构造块 | `internal/session/router_lifecycle.go:119-123` |
| wireup blank import(claude+kiro) | `internal/wireup/history_backends.go:25-26` |
| Profile struct 字段 + RegisterDefaults(claude+kiro) | `internal/cli/backend/profile.go:35-115,232-234` |
| kiroProfile 全形(借鉴模板) | `internal/cli/backend/profile_kiro.go:17-61` |
| kirojsonl init()+factory+扁平 join(对照) | `internal/history/kirojsonl/source.go:86-98,178` |
| DetectInProc 实参是 basename | `internal/discovery/proc_linux.go:100` / `proc_darwin.go:78` |
| doctor renderBackendsSection Profile 驱动 + caps/HistoryDir 自动 | `cmd/naozhi/doctor.go:251,350-352,435-436` |
| EnabledBackends 配置驱动(零代码改) | `internal/config/config.go:1288-1342` |

---

## 12. 参考

- OpenAI 官方文档:
  - https://developers.openai.com/codex/app-server (app-server 协议总览)
  - https://developers.openai.com/codex/cli/reference (CLI flags)
  - https://developers.openai.com/codex/noninteractive (exec 模式,作为对比参考)
- 源码(pin commit `c73296a0`):`openai/codex/codex-rs/app-server-protocol/`、`codex-rs/rollout/`、`codex-rs/cli/`、`codex-rs/core/`(详见 §11.1)
- naozhi 内部:`docs/rfc/multi-backend.md` v2、`internal/cli/protocol_acp.go`(双向 JSON-RPC 实现参考)、`internal/history/kirojsonl/`(历史 source 模板)
