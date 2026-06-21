# Codex Backend 接入

> **状态**: Draft v2（**Phase 0 已实测**，2026-06-21；详见 `codex-backend-validation.md`）
> **作者**: naozhi team
> **创建**: 2026-05-20 · **实测更新**: 2026-06-21
> **依赖 / 前置**:
> - `internal/cli/backend/profile.go`（多 backend 注册表已就位，参见 `multi-backend.md`）
> - `internal/cli/protocol.go` `Protocol` 接口
> - `internal/cli/protocol_acp.go`（JSON-RPC 2.0 over stdio 长连接的现成骨架，可作为实现参考）
> - `docs/rfc/multi-backend.md` v2（backend.Profile 抽象与 Dashboard §8 差异化规约）
> **关联代码**: `internal/cli/backend/` · `internal/cli/protocol_acp.go` · `internal/cli/wrapper.go` · `internal/history/` · `cmd/naozhi/main.go`
> **可行性验证**: ✅ `docs/rfc/codex-backend-validation.md`（codex 0.141.0 实测：协议跑通、method 名命中、Bedrock 路径连通 + 两个约束）

---

## 0. TL;DR

OpenAI 的 Codex CLI 已经把"程序化接入"路线收敛到 **`codex app-server`**：JSON-RPC 2.0 over stdio NDJSON，长连接，双向通信，schema 可生成。这条路跟 naozhi 现有的 `Process` 长生命周期 + `ACPProtocol` 双向 JSON-RPC 框架完全契合。

接 Codex 的工作量约等于"写一个独立的 `protocol_codex.go` + 一个 `profile_codex.go`"，**不复用 `ACPProtocol`**（method/schema 不同），但可以借鉴它的双向 RPC + waitForResponse + 自动授权骨架。

**不要走** 老的 `codex proto` 子命令、`codex exec --json` 单次模式、`codex mcp-server` MCP 包装这三条路，理由见 §2。

---

## 1. 背景

### 1.1 Codex CLI 当前的程序化入口

| 入口 | 描述 | 是否适合 naozhi |
|---|---|---|
| `codex` (TUI) | 交互式 terminal UI | ❌ 不可程序化 |
| `codex proto` | 老的 protocol mode：SQ/EQ NDJSON over stdio，`#[non_exhaustive]` enum | ❌ 已被 app-server 取代，schema 不稳定 |
| `codex exec --json` / `--experimental-json` | 非交互单次模式，每次起进程，输出 NDJSON `ThreadEvent` | ❌ 失去长生命周期，每 turn 重启，跟 naozhi 长连接模型冲突 |
| **`codex app-server`** | **JSON-RPC 2.0 over stdio，长连接，Thread/Turn/Item 三层 primitive** | ✅ **首选** |
| `codex mcp-server` | 把 codex 包成 MCP server | ❌ 富交互（streaming / approval）映射不进 MCP |

### 1.2 为什么必须是 app-server

OpenAI 2026-02 工程博客 *Unlocking the Codex harness: how we built the App Server* 明文表态："**Use the App Server's JSON-RPC protocol as the primary integration method ... This is the first-class integration method OpenAI will maintain going forward.**" 自家所有 client（VS Code 扩展、Codex Web、TUI `--remote`、macOS app）都已统一到 app-server。

老的 `codex proto`：
- `Op` / `EventMsg` enum 标 `#[non_exhaustive]`，schema 跟版本走不稳定
- 是 codex-rs 内部 IPC 设计，不承诺向后兼容
- `proto` 子命令文档已经从官方 reference 页淡出，只在 GitHub `codex-rs/docs/protocol_v1.md` 留存

而 `codex app-server`：
- 协议层：JSON-RPC 2.0（省略 `"jsonrpc":"2.0"` 头），stdio NDJSON 默认，可选 WebSocket / Unix socket
- 承诺**向后兼容**："older clients can safely communicate with newer server versions"
- **schema 可一键生成**：`codex app-server generate-ts --out DIR` / `generate-json-schema --out DIR`，写适配层不靠瞎猜
- Thread / Turn / Item 三层 primitive，跟 Claude / ACP 概念对得上
- session 持久化在 `~/.codex/sessions/`，可以 resume

### 1.3 范围

**做**：
1. 在 `internal/cli/protocol_codex.go` 新增 `CodexProtocol` 实现 `cli.Protocol`
2. 在 `internal/cli/backend/profile_codex.go` 新增 `codexProfile()` 并注册到 `RegisterDefaults`
3. 新增 `internal/history/codexjsonl/` 让 dashboard 历史面板能读 `~/.codex/sessions/`
4. Dashboard chip 颜色、cost 单位、能力 map 按 `multi-backend.md` §8 的规约接进去
5. 文档：本 RFC + `codex-backend-validation.md` 实测报告

**不做**：
- 不复用 `ACPProtocol`（method 名、schema 完全不同；强行复用会污染 ACP 的 Kiro/Gemini 实现）
- 不改 `internal/cli/process.go` 任何逻辑（Protocol 接口已经是抽象边界）
- 不接 `codex proto` / `codex exec` / `codex mcp-server` 三条死路
- 不做 codex 特有 UI（plan mode / collaboration mode）的精细化呈现，先用通用 tool_call 渲染，后续单独 RFC

---

## 2. 协议要点

完整 schema 由 `codex app-server generate-json-schema` 产出，本节只列对实现选型有影响的关键点。

### 2.1 启动

```bash
codex app-server                              # 默认 stdio NDJSON
codex app-server --listen stdio://            # 等价显式写法
codex app-server --listen ws://127.0.0.1:4500 # WebSocket（实验，不用）
codex app-server --listen unix://             # Unix socket（实验）
```

naozhi 用默认 stdio。

### 2.2 握手

```
client → server: { id: 1, method: "initialize", params: { clientInfo: {...} } }
server → client: { id: 1, result: { serverInfo: {...} } }
client → server: { method: "initialized" }                    // notification, 无 id
```

未握手前 server 拒绝任何其他 method（返回 `Not initialized` 错误）；重复 `initialize` 在同一连接上会返 `Already initialized`。

### 2.3 三层 Primitive

| 层级 | 含义 | 关键 method |
|---|---|---|
| **Thread** | 一段对话（持久 + 可 resume / fork / archive） | `thread/start`、`thread/resume`、`thread/archive`、`thread/close` |
| **Turn** | 一次用户输入触发的 agent 工作 | `turn/start`、`turn/interrupt`、`turn/steer` |
| **Item** | turn 内的原子事件（user/agent message、reasoning、command、file change、MCP tool call、web search、todo list） | 通过 `item/started`、`item/<kind>/delta`、`item/completed` notification 流式推送 |

### 2.4 一次完整 turn 的事件流（✅ 2026-06-21 实测，已按 schema 修正）

```
client → server: thread/start         { cwd }
server → client: { id, result: { thread: { id: "<threadId>", ... } } }   # ← threadId 在这里
server → client: thread/started       (notif)
client → server: turn/start           { threadId, input: [ { type:"text", text:"..." } ] }   # ← input 是 UserInput[]
server → client: turn/started         (notif)
server → client: item/started         (notif) { item: { type:"userMessage" | "agentMessage" | ... } }
server → client: item/agentMessage/delta (notif) { itemId, threadId, turnId, delta: "Hi" }   # ← delta 是纯 string
server → client: item/agentMessage/delta (notif) { ..., delta: "!" }
server → client: item/started         (notif) { item: { type:"commandExecution" } }
server → client: item/<kind>/requestApproval (反向请求) { ... }   # ← 见 §2.5 实测名
client → server:                      自动 allow 响应
server → client: item/completed       (notif) { item: { type:"agentMessage", text:"..." } }
server → client: thread/tokenUsage/updated (notif) { tokenUsage: { last, total } }   # ← usage 在这里，不在 turn/completed
server → client: turn/completed       (notif) { threadId, turn: { status, error } }
client → server: { id, result: { turn: { ... } } }   # turn/start 的迟到 response
```

实测修正（详见 `codex-backend-validation.md` §3）：
- **threadId** 取自 `thread/start` response 的 `result.thread.id`（非 `thread/started` notification 顶层）。
- **`turn/start.input`** 是 `UserInput[]`（`[{type:"text",text:"…"}]`），发裸字符串得 `-32600 expected a sequence`。
- **`item/agentMessage/delta.params.delta`** 是**纯字符串**，不是 `{content}`。
- **token usage** 走单独的 `thread/tokenUsage/updated` notification，不在 `turn/completed`。

注意：`turn/start` 是一个 **request**，server 流完所有 notification 后才 reply 这个 request。这种"长 RPC + 中间穿插 notification"的形态跟 Kiro ACP 的 `session/prompt` 一致，复用现有 `readUntilResponse` 模式即可。

### 2.5 反向请求（approval flow）

server 主动向 client 发 Request 要求决策。**实测的反向请求 method 名**（ServerRequest 枚举，2026-06-21）：

```
item/commandExecution/requestApproval
item/fileChange/requestApproval
item/permissions/requestApproval
item/tool/requestUserInput
mcpServer/elicitation/request
```

naozhi 的策略：
- 默认走 `approval_policy=never` + `sandbox_mode=workspace-write`，跟 claude `--dangerously-skip-permissions` 同立场
  （实测约束：`approval_policy=never` 不能配 `sandbox_mode=danger-full-access`，否则 codex 退回 read-only；workspace-write 是安全选择）
- 一旦 server 仍发了 `*/requestApproval` 反向请求（边缘 case），HandleEvent 自动 allow，落到与 ACP `session/request_permission` 同套自动授权代码

### 2.6 Cost / Usage（✅ 实测修正：走单独 notification）

token 用量**不在** `turn/completed`，而在单独的 `thread/tokenUsage/updated` notification：

```json
{
  "threadId": "...",
  "turnId": "...",
  "tokenUsage": {
    "last":  { "inputTokens": 72, "outputTokens": 20, "cachedInputTokens": 0,
               "reasoningOutputTokens": 0, "totalTokens": 92 },
    "total": { "inputTokens": ..., "outputTokens": ..., "totalTokens": ... },
    "modelContextWindow": 128000
  }
}
```

无 `total_cost_usd`。实现把 `tokenUsage.last` 映射为 `EventMetadata.MeteringUsage`（`{value: totalTokens, unit: "token"}`）+ `ContextUsagePercent`（由 totalTokens / modelContextWindow 估算）。`Profile.CostUnit = "tokens"`，dashboard cost 列显示 token 数；USD 换算由 normalize 层按 model price 处理（不在本 RFC 范围）。

### 2.7 Backpressure

server 满载时返 `-32001 "Server overloaded; retry later."`。client 必须**指数退避 + jitter** 重试。在 naozhi 这边映射为 `Process.Send` 的临时失败，复用现有重试路径。

---

## 3. 与 naozhi 现有协议对比

| 维度 | Claude stream-json | Kiro/Gemini ACP | **Codex app-server** |
|---|---|---|---|
| 协议族 | 自定义 NDJSON | JSON-RPC 2.0 | **JSON-RPC 2.0** |
| 双向通信 | ❌ CLI 单向推 | ✅ | ✅ |
| 反向请求 | 无 | `session/request_permission` | 多种（approval / elicitation / userInput） |
| 流式 chunk 名 | `assistant` 块 | `agent_message_chunk` | `item/agentMessage/delta` |
| Turn 结束 | `result` 行 | `session/prompt` response | `turn/completed` notif + `turn/start` response |
| Cancel | SIGINT | `session/cancel` notif | `turn/interrupt` request |
| 中途追加输入 | passthrough 多消息排队 | ❌ collect mode | `turn/steer` request（**新能力**） |
| Resume | `--resume <sid>` | `session/load` | `thread/resume` |
| 历史落地 | `~/.claude/projects/*.jsonl` | `~/.kiro/sessions/cli/*.jsonl` | `~/.codex/sessions/*` |
| Cost 单位 | USD | credits | tokens |
| Schema 来源 | 反向工程 | 反向工程 + 官方文档 | **`generate-json-schema --out`** |

最大的协议红利：`turn/steer` 是 Codex 独有的"turn 进行中追加输入"原语，可以原生支持 naozhi 的 `/urgent` 多消息并发，不必 fall back 到 collect mode。但本 RFC 先按 collect 落地，`turn/steer` 留作 Phase 2 优化。

---

## 4. 文件变更清单

### 4.1 新增

| 路径 | 行数估计 | 说明 |
|---|---|---|
| `internal/cli/protocol_codex.go` | ~700 | `CodexProtocol` 实现 `Protocol`：BuildArgs / Init / WriteMessage / WriteUserMessageLocked / WriteInterrupt / ReadEvent / HandleEvent / Capabilities |
| `internal/cli/protocol_codex_test.go` | ~400 | 表驱动：握手、turn 完整流、interrupt、approval 反向请求、backpressure -32001 |
| `internal/cli/backend/profile_codex.go` | ~70 | `codexProfile()`：ID/DisplayName/DefaultBinary/DefaultTag/ChipColor/HistoryDir/CostUnit/Features/NewProtocol |
| `internal/history/codexjsonl/source.go` | ~250 | 读 `~/.codex/sessions/` 转成 dashboard 历史 source |
| `internal/history/codexjsonl/source_test.go` | ~150 | fixture 驱动 |
| `docs/rfc/codex-backend.md` | 本文件 | 设计文档 |
| `docs/rfc/codex-backend-validation.md` | 待写 | Phase 0 实测脚本与日志 |

### 4.2 修改

| 路径 | 改动 |
|---|---|
| `internal/cli/backend/profile.go` | `RegisterDefaults` 增 `Register(codexProfile())` |
| `cmd/naozhi/main.go` | 历史 source 注册增 codexjsonl |
| `internal/cli/event.go` | `SendResult` 已有 InputTokens/OutputTokens（Gemini 也用），无需新增；如缺则补 |
| `internal/config/config.go` | `EnabledBackends` 默认值文档加 codex 选项（不改默认开启列表） |
| `docs/rfc/README.md` | 索引增本 RFC |
| `docs/rfc/multi-backend.md` | §8 Dashboard 差异化规约表追加 codex 行（chip 色、cost 单位 "tokens"、能力 map） |

### 4.3 不动

- `internal/cli/protocol_acp.go` —— 不复用，不污染
- `internal/cli/process.go` —— Protocol 接口已经是抽象边界
- `internal/session/router.go` —— Profile 注册即生效
- `internal/discovery/proc_*.go` —— `DetectInProc` 通过 Profile 注入
- Dashboard 前端 —— `Features` map 驱动，不写 codex 专属分支

---

## 5. Profile 字段填充

参考 `internal/cli/backend/profile_kiro.go` 同形：

| 字段 | 值 | 备注 |
|---|---|---|
| `ID` | `"codex"` | |
| `DisplayName` | `"codex"` | |
| `DefaultBinary` | `"codex"` | npm `@openai/codex` 装完叫 codex |
| `DefaultTag` | `"cdx"` | reply prefix；与 `cc` / `kiro` / `gem` 对齐 |
| `ChipColor` | `"#10a37f"` | OpenAI 品牌绿，与 claude 紫、kiro 橙、gemini 蓝区分 |
| `NewProtocol` | `func(_) cli.Protocol { return &cli.CodexProtocol{BackendID: "codex"} }` | ProtocolDeps 不消费 |
| `DetectInProc` | `strings.Contains(cmdline, "codex")` 但排除 `"app-server"` 之外的 codex 子命令以缩窄误判 | |
| `RequiredNodeCaps` | `[]string{"codex-app-server"}` | 新加的 reverse cap，等价于 `acp` 之于 kiro |
| `HistoryDir` | `"~/.codex/sessions/"` | |
| `CostUnit` | `"tokens"` | |
| `Features` | askuser=false (走 `requestUserInput` 反向请求待 phase2 卡片化), passthrough=false (phase1), embedded_context=false (phase1), image_input=true, audio_input=false, mcp_http=true, mcp_sse=false | phase1 保守值，phase2 按实测放开 |

---

## 6. CodexProtocol 实现要点

跟 `ACPProtocol` 共享的骨架（直接搬代码思路，不共享 struct）：
- `pendingResponses map[id]chan rpcResponse` + `waitForResponse(id, timeout)`
- 单 stdin 写锁 `wMu`（Process.shimWMu 之外的协议级锁）
- `BackendID` 字段用于 metric label
- `acpHandshakeTimeout` 等价物，建议初值 30s（Codex 的 model warmup 可能比 ACP 慢）
- `ErrCodexRPC` / `ErrCodexTimeout` 类型化错误，便于 dispatch 层 errors.Is

不一样的：
- **method 名全部不同**（`thread/start` / `turn/start` / `item/*`，没有 `session/*`）
- **id 类型** `string` (UUID) 与 `number` 都允许，统一按 `json.RawMessage` 反序列化（Kiro 已经踩过这坑，复用同样策略）
- **`turn/start` request 的 response 不是立即返回**，要在所有 `item/*` notif 之后才到。waitForResponse 必须支持长超时（建议 turn 级别 5min，与现有 turn timeout 对齐）
- **response_id 持久化**：`turn/completed` 带 `response_id`，下一次 `thread/resume` 可以用它做 fine-grained 续接。先存在 `Process.lastResponseID` 字段，phase2 接 shim state

`BuildArgs` 草稿：

```go
func (p *CodexProtocol) BuildArgs(opts SpawnOptions) []string {
    args := []string{"app-server"}
    // model / cwd / sandbox 通过 RPC 传，不用 CLI flag
    if opts.Model != "" {
        args = append(args, "-c", "model="+opts.Model)
    }
    args = append(args, "-c", "approval_policy=never")
    args = append(args, "-c", "sandbox_mode=workspace-write")
    return args
}
```

`Init` 草稿：

```go
func (p *CodexProtocol) Init(rw *JSONRW, resumeID, cwd string) (string, error) {
    // 1. send initialize request, wait result
    // 2. send initialized notification (no id)
    // 3. if resumeID: thread/resume; else: thread/start with cwd
    // 4. return threadId
}
```

`ReadEvent` 草稿（核心翻译层）：

```go
// raw NDJSON line → 0..N cli.Event
// - method=item/agentMessage/delta → cli.Event{Type: AssistantText, Delta: ...}
// - method=item/started type=commandExecution → cli.Event{Type: ToolCall, ...}
// - method=turn/completed → cli.Event{Type: TurnDone, Cost: ..., Usage: ...}, done=true
// - id-bearing response → 投 pendingResponses，返回 nil slice
// - method=serverRequest/* → 自动授权 / 反向请求处理，返回 nil slice
// - 未知 method → log warn，返回 nil slice（向前兼容）
```

`WriteInterrupt` 用 `turn/interrupt` request，比 Claude `control_request` 干净；`Capabilities()` 返 `Caps{Replay: false, Priority: false, SoftInterrupt: true, StreamJSON: false}`（phase1）。

---

## 7. 鉴权（✅ 实测纠正：Codex 原生支持 Bedrock）

> **Draft v1 论断作废**：v1 称 "Codex 模型只能走 OpenAI 自家（不像 claude 走 Bedrock）"。实测推翻 —— codex 0.141 **内置 `amazon-bedrock` model provider**（openai/codex PR #18744，2026-04-20 合入），naozhi 部署机的 AWS 凭据可直接驱动 codex。

Codex CLI 鉴权三条路径：

| 路径 | 适用 | naozhi 立场 |
|---|---|---|
| **Amazon Bedrock + gpt-5.5**（`AWS_BEARER_TOKEN_BEDROCK` 或 AWS 凭据链，us-east-1/2） | 复用部署机 AWS 接入 + 完整 agentic | ✅ **首选**，已验证完整 agentic（见 §7.1） |
| Amazon Bedrock + gpt-oss（us-west-2） | 纯对话 / function-calling | ⚠️ agentic 受限退路（见 §7.2） |
| `CODEX_API_KEY` / `OPENAI_API_KEY` 环境变量 | 完整 agentic | 直连 OpenAI，无区域限制 |
| `codex login` 持久化 ChatGPT 登录态 | 桌面交互 | 不适合 headless naozhi |

### 7.1 Bedrock + gpt-5.5（首选路径，2026-06-21 实测完整 agentic）

gpt-5.x 在 **us-east-1 / us-east-2** 的 `bedrock-mantle/openai/v1/responses` 上可用（responses-only，不支持 chat completions）。这恰好是 codex **内置 `amazon-bedrock` provider** 的默认路径，所以**直接用内置 provider，无需自定义**：

```toml
model_provider = "amazon-bedrock"
model = "openai.gpt-5.5"

[model_providers.amazon-bedrock.aws]
region = "us-east-2"
# 凭据走 AWS_BEARER_TOKEN_BEDROCK（Bedrock API key）或标准 AWS 凭据链
```

实测（codex 0.141.0，`codex exec` + `app-server` 两路径）：
- ✅ 纯对话返回正确（`turn/completed` status=`completed`）。
- ✅ **完整 agentic 跑通**：codex 成功调用 shell 工具（`cat probe.txt` 执行并返回正确内容），**不**触发 gpt-oss 的 `namespace` 工具被拒错误。

> 注意：`aws bedrock list-foundation-models` **不列** mantle-only 的 gpt-5.x —— 必须直接探端点确认（404=不存在；400 "does not support /v1/responses"=存在但走 `/openai/v1/responses`）。

### 7.2 Bedrock + gpt-oss（受限退路）

实测 `bedrock-mantle.us-west-2.api.aws/v1/responses` + `openai.gpt-oss-120b` 连通，但有两个约束，需用**自定义 provider**：

```toml
[model_providers.bedrockmantle]
name     = "Bedrock Mantle"
base_url = "https://bedrock-mantle.us-west-2.api.aws/v1"   # 注意：无 /openai 前缀
wire_api = "responses"                                     # chat 在 0.141 已废弃
env_key  = "AWS_BEARER_TOKEN_BEDROCK"
```

- **约束 1（路径）**：内置 `amazon-bedrock` provider 打 `/openai/v1/responses`，gpt-oss 只在 `/v1/responses` 服务 → 必须自定义 provider 指向无前缀 base_url。
- **约束 2（agentic 受限）**：Bedrock gpt-oss responses 拒绝 codex 内置 agentic 工具的 `type:"namespace"` 声明（只认 `function`/`mcp`），完整 shell-agentic turn `status:failed`。**gpt-oss 路径仅纯对话 / function-calling**。

落地前提：
- 部署文档：启用 codex backend 时择一 —— ① **Bedrock + gpt-5.5**（内置 provider，us-east-1/2，完整 agentic，推荐）；② Bedrock + gpt-oss（自定义 provider，us-west-2，agentic 受限）；③ `CODEX_API_KEY`/`codex login`。
- `cmd/naozhi/doctor.go` 增 codex 健康检查：`codex --version` 能跑 + （`~/.codex/auth.json` / `CODEX_API_KEY` / `AWS_BEARER_TOKEN_BEDROCK` 三选一）。

不在本 RFC 范围：自建凭据池、企业网关代理、按 session 切 key、Bedrock Access Gateway（BAG）代理把 namespace 工具翻译给 gpt-oss。

---

## 8. 测试策略

### 8.1 单元测试 `protocol_codex_test.go`

表驱动覆盖：
- 握手：initialize/initialized + 拒绝重复 initialize
- 完整 turn：thread/start → turn/start → 多个 item delta → turn/completed
- Interrupt：turn/interrupt 立即生效，stopReason 透传
- 反向请求：server 发 approval 请求，client 自动授权回应
- Backpressure：-32001 错误码触发重试 hint
- ID 兼容：UUID 字符串 + number 都能反序列化
- 未知 method 容忍：log warn 不崩

### 8.2 集成测试

- `cli_test.go` 加 codex backend fixture（模拟 app-server NDJSON 流）
- 历史 source 用 fixture jsonl 验证 codexjsonl 解析

### 8.3 Phase 0 实测（✅ 已完成 2026-06-21 — 详见 `codex-backend-validation.md`）

实测结果汇总（V1/V2/V3/V4/V10/V12 ✅ 通过；V5 ⚠️ Bedrock gpt-oss 受约束 2 限制；V6-V9/V11 ⏳ method/schema 已确认存在，运行时留单测 + phase 后续）：

| 验证点 | 脚本 | 通过条件 |
|---|---|---|
| V1 启动 | `codex app-server --version` | 退出码 0 |
| V2 握手 | echo initialize JSON 至 stdin | 收到 result，发 initialized 后能下发 thread/start |
| V3 单 turn | thread/start + turn/start "echo hi" | 收到 turn/completed，response 包含 "hi" |
| V4 流式 | 长 prompt 触发多 chunk | item/agentMessage/delta 数 >= 5 |
| V5 工具调用 | "list files" prompt | 收到 commandExecution item |
| V6 interrupt | 长 prompt 中途发 turn/interrupt | turn/completed stopReason=interrupted |
| V7 resume | thread/start → kill → 新进程 thread/resume 同 threadId | context 完整恢复 |
| V8 多 thread | 一个 app-server 进程开 N 个 thread | context 互不污染 |
| V9 backpressure | 高频 turn/start 灌入 | 至少触发一次 -32001 错误码 |
| V10 反向请求 | 触发命令执行 approval | 自动 allow 后命令执行成功 |
| V11 持久化 | turn 后查 `~/.codex/sessions/` | 文件存在且可被 codexjsonl 读取 |
| V12 schema 生成 | `codex app-server generate-json-schema --out /tmp/x` | 文件生成且 JSON 合法 |

协议路线（V1-V4/V10/V12）已实测通过，按 §10 进入 Phase 1 实现。Bedrock 的 agentic 约束（V5）记录在案，不阻挡 protocol/profile 落地。

---

## 9. 风险与未决项

### 9.1 风险

| 风险 | 等级 | 缓解 |
|---|---|---|
| `app-server` 仍是新接口（2025 末才推），可能有未发现的边缘 case | M | Phase 0 实测全跑过；订阅 codex GitHub release notes |
| 多 thread 共享一个 app-server 进程的并发隔离质量未知（V8） | M | phase1 仍按 1 thread / 进程，跟 claude 对齐；V8 验证后 phase2 做"一进程多 thread"优化 |
| `turn/steer` 与 naozhi `/urgent` 语义对接细节 | L | phase2 单独 RFC |
| ~~Codex 模型只能走 OpenAI 自家~~ → **已验证可走 Bedrock 且完整 agentic**（gpt-5.5 @ us-east-1/2，§7.1）；gpt-oss 路径 agentic 受限（§7.2） | L | 首选 Bedrock + gpt-5.5（内置 provider，完整 shell agentic 实测通过）；gpt-oss 仅纯对话退路 |
| Approval flow `requestUserInput` 转 AskUserQuestion 卡片的 schema 映射 | L | phase2，phase1 默认拒绝避免悬挂 |

### 9.2 未决

1. **历史 source 是该读 `~/.codex/sessions/` 还是走 `thread/list` RPC？** 倾向前者（与 claude/kiro 一致都是文件落地），实测 V11 后定。
2. **Cost 列要不要现场换算 USD？** 倾向不换，dashboard 显示 "1234 tokens"；如要换需引入 model price 表，超出本 RFC。
3. **Reverse-node capability 字符串叫 `"codex-app-server"` 还是简单 `"codex"`？** 倾向前者（明示协议版本，未来 app-server v2 可分），定稿前在 reverse-protocol RFC 中确认。

---

## 10. 实施分期

| Phase | 范围 | 工时估计 |
|---|---|---|
| **Phase 0** | 实测 V1-V12，写 `codex-backend-validation.md`；schema 生成对照 | 0.5 day |
| **Phase 1** | `protocol_codex.go` + `profile_codex.go` + 单测；`codexjsonl` 历史 source；本 RFC 升 v2 | 2-3 days |
| **Phase 2** | Dashboard chip / cost / Features 接入；反向请求 → AskUserQuestion 卡片；`turn/steer` 接 `/urgent` | 1-2 days |
| **Phase 3** | 生产灰度：在测试群 enable codex，跑一周观察 metric / 日志 / 用户反馈 | 1 week |

Phase 0 不通过则**全部推迟**，本 RFC 重写。

---

## 11. 参考

- OpenAI 官方文档：
  - https://developers.openai.com/codex/app-server （app-server 协议总览）
  - https://developers.openai.com/codex/cli/reference （CLI flags）
  - https://developers.openai.com/codex/noninteractive （exec 模式，作为对比参考）
- 工程博客（2026-02）：*Unlocking the Codex harness: how we built the App Server*
- 源码：`openai/codex/codex-rs/app-server/`、`codex-rs/protocol/src/protocol.rs`、`codex-rs/exec/src/exec_events.rs`
- naozhi 内部：`docs/rfc/multi-backend.md` v2、`internal/cli/protocol_acp.go`（双向 JSON-RPC 实现参考）
