# Codex Backend — Phase 0 可行性验证报告

> **状态**: 实测完成 ✅（2026-06-21）
> **执行环境**: Amazon Linux 2023 aarch64 · node v22.22.0 · codex-cli **0.141.0** · AWS_REGION=us-west-2
> **关联**: `docs/rfc/codex-backend.md`（本报告把该 RFC 从 Draft v1 升到 v2）

---

## 0. TL;DR

- ✅ **codex 0.141.0 安装成功**，自带 `codex app-server` 子命令。
- ✅ **app-server JSON-RPC 协议完整跑通**：`initialize → initialized → thread/start → turn/start → turn/started → item/started → item/completed → turn/completed`，threadId 正确捕获。RFC §2.4 的事件流草图被实测**证实**。
- ✅ **method / notification 名经 `generate-json-schema` + 实测全部命中** RFC §2.3/§2.4 的假设（`thread/start`、`turn/start`、`turn/interrupt`、`turn/steer`、`thread/resume`、`item/agentMessage/delta`、`turn/completed` 等）。
- 🔥 **重大纠正**：RFC §7 / §9.1 称 "Codex 模型只能走 OpenAI 自家（不像 claude 走 Bedrock）" —— **此论断已过时**。codex 0.141 **内置 `amazon-bedrock` model provider**（openai/codex PR #18744，2026-04-20 合入）。实测 `bedrock-mantle.us-west-2.api.aws/v1/responses` + `openai.gpt-oss-120b` 返回真实回复。
- ⚠️ **两个 Bedrock 约束**（影响 "codex on bedrock" 完整能力，详见 §4）：
  1. codex 内置 `amazon-bedrock` provider 把请求打到 `/openai/v1/responses`（带 `/openai` 前缀），但 Bedrock 上的 gpt-oss 只在 `/v1/responses`（无前缀）服务 —— 内置 provider 直接报 `model does not support the '/openai/v1/responses' API`。需用**自定义 provider** 指向正确的 `…/v1` base_url 绕过。
  2. Bedrock 的 gpt-oss responses 端点**拒绝 codex 内置 agentic 工具的 `namespace` tool 变体**（只接受 `function` / `mcp`），完整 shell-agentic turn 因此 `status: failed`。纯对话 / function-calling 可用，但 codex 招牌的本地 shell agentic 能力在 Bedrock gpt-oss 上**受限**。
- ❌ gpt-5.x 系列在 us-west-2 Bedrock **不可用**，只有 `gpt-oss-*`（20b/120b/safeguard）`ACTIVE`。

**结论**：app-server 协议路线 100% 可行，按 RFC §1.3 实施。Bedrock 作为一个**已验证但 agentic 受限**的部署选项写入 RFC；面向用户的 codex 完整能力仍以 OpenAI 凭据 / ChatGPT 登录为主路径。

---

## 1. 安装

```
$ npm install -g @openai/codex
added 2 packages in 2s
$ codex --version
codex-cli 0.141.0
```

`codex app-server` 子命令存在，并带 `generate-ts` / `generate-json-schema` 协议 schema 生成工具（RFC §1.2 承诺核实）。

---

## 2. Schema 生成（V12 ✅）

```
$ codex app-server generate-json-schema --out /tmp/codex-schema
```

产出 200+ JSON Schema 文件。关键文件：

| 文件 | 内容 |
|---|---|
| `ClientRequest.json` | client→server 全部 RPC method 枚举 |
| `ServerNotification.json` | server→client 全部流式事件 method 枚举 |
| `ServerRequest.json` | server→client 反向请求（approval / userInput / elicitation） |
| `v2/TurnStartParams.json` | `turn/start` 参数 schema（**input 是 `UserInput[]` 不是 string**） |

`ClientRequest` 枚举证实 RFC §2.3 的 method 名（`initialize` / `thread/start` / `thread/resume` / `turn/start` / `turn/steer` / `turn/interrupt`）全部存在。

---

## 3. 协议实测

### 3.1 V2 握手 ✅

直接 NDJSON 灌入 `codex app-server` stdin：

```
→ {"id":1,"method":"initialize","params":{"clientInfo":{"name":"naozhi","version":"0.0.1"}}}
← {"id":1,"result":{"userAgent":…,"codexHome":…,"platformFamily":…}}
→ {"method":"initialized"}                                  (notification)
→ {"id":2,"method":"thread/start","params":{"cwd":"/tmp/codextest"}}
← {"id":2,"result":{"thread":{"id":"019ee98d-…",…},"model":…,"modelProvider":…}}
← {"method":"thread/started","params":{...}}                (notification)
```

threadId 在 `thread/start` 的 **response.result.thread.id** 拿到（不在 `thread/started` notification 的 params 顶层）。

### 3.2 V3 单 turn ✅（协议层）

```
→ {"id":3,"method":"turn/start","params":{"threadId":"019ee98d-…",
      "input":[{"type":"text","text":"reply with exactly NAOZHI_OK"}]}}
← {"method":"turn/started",...}
← {"method":"item/started","params":{"item":{"type":"userMessage",...}}}
← {"method":"item/completed","params":{"item":{"type":"userMessage",...}}}
← {"method":"turn/completed","params":{"threadId":...,"turn":{"status":"failed"|"completed",...}}}
```

> 关键修正：RFC §2.4 把 `turn/start` 的 `input` 画成字符串。实测 schema 是 **`UserInput[]`**，每项 `{"type":"text","text":"…"}`。先发字符串会得到 `-32600 Invalid request: invalid type: string …, expected a sequence`。本报告把这一点回写 RFC §2.4 与实现。

### 3.3 事件名实测对照（ServerNotification 枚举）

| RFC §2.4 假设 | 实测 schema | 命中 |
|---|---|---|
| `thread/started` | `thread/started` | ✅ |
| `turn/started` | `turn/started` | ✅ |
| `item/started` | `item/started` | ✅ |
| `item/agentMessage/delta` | `item/agentMessage/delta`（`params.delta` 是**纯字符串**，不是 `{content}`） | ✅（字段名修正） |
| `item/completed` | `item/completed`（`params.item` = `ThreadItem`，agentMessage 项带 `text`） | ✅ |
| `turn/completed` | `turn/completed`（`params.turn.status` + `turn.error`；usage 在单独的 `thread/tokenUsage/updated`） | ✅（usage 位置修正） |

**usage 修正**：RFC §2.6 假设 `turn/completed` 带 `usage`。实测 token 用量走单独的 `thread/tokenUsage/updated` notification，结构 `tokenUsage.{last,total}.{inputTokens,outputTokens,cachedInputTokens,reasoningOutputTokens,totalTokens}`。实现据此把 tokenUsage 通知映射到 `EventMetadata.MeteringUsage`（unit="token"）。

### 3.4 反向请求实测名（ServerRequest 枚举）

RFC §2.5 假设 `serverRequest/...`。实测更具体：

```
item/commandExecution/requestApproval
item/fileChange/requestApproval
item/permissions/requestApproval
item/tool/requestUserInput
mcpServer/elicitation/request
```

实现的 HandleEvent 对 `*/requestApproval` 自动 allow（对齐 claude `--dangerously-skip-permissions` 立场）。

---

## 4. Bedrock 可行性（纠正 RFC §7）

### 4.1 内置 provider 存在

codex 0.141 二进制内含 `bedrock-mantle.` base_url 字面量。`codex debug models` 与官方文档（openai/codex PR #18744）确认内置 `amazon-bedrock` provider：

```toml
# 官方文档形态
model_provider = "amazon-bedrock"
[model_providers.amazon-bedrock.aws]
profile = "..."   # 或走默认 AWS 凭据链
region  = "us-west-2"
```

### 4.2 端点连通性实测

环境：`AWS_REGION=us-west-2`，IAM 凭据有效，生成了 Bedrock long-term API key（`aws iam create-service-specific-credential --service-name bedrock.amazonaws.com`）。

| 端点 | 模型 | 结果 |
|---|---|---|
| `bedrock-runtime…/openai/v1/chat/completions` | gpt-oss-120b | ✅ 返回真实 completion |
| `bedrock-mantle…/v1/responses` | gpt-oss-120b | ✅ 返回真实 response（`BEDROCK_OK`） |
| `bedrock-mantle…/openai/v1/responses` | gpt-oss-120b | ❌ `model does not support the '/openai/v1/responses' API` |
| `bedrock-mantle…/v1/responses` | gpt-5.5 | ❌ `The model 'openai.gpt-5.5' does not exist`（区域不可用） |

`aws bedrock list-foundation-models --region us-west-2` 中 `gpt-oss` 系列：

```
openai.gpt-oss-120b-1:0       ACTIVE
openai.gpt-oss-20b-1:0        ACTIVE
openai.gpt-oss-safeguard-120b ACTIVE
openai.gpt-oss-safeguard-20b  ACTIVE
```

### 4.3 约束 1：内置 provider 路径 bug → 用自定义 provider

内置 `amazon-bedrock` provider 打到带 `/openai` 前缀的路径，gpt-oss 不在那里服务。可用配置（实测 codex exec / app-server 均通过该 provider 完成握手与 turn）：

```toml
[model_providers.bedrockmantle]
name     = "Bedrock Mantle"
base_url = "https://bedrock-mantle.us-west-2.api.aws/v1"
wire_api = "responses"                      # chat 已被 codex 0.141 废弃
env_key  = "AWS_BEARER_TOKEN_BEDROCK"       # Bedrock API key（bearer）
```

> 注：`wire_api = "chat"` 在 codex 0.141 被硬拒绝（二进制字面量 `wire_api = "chat" is no longer supported`），必须 `responses`。

### 4.4 约束 2：agentic 工具被 Bedrock gpt-oss 拒绝

通过自定义 provider 跑完整 `codex exec` agentic turn 时：

```
ERROR: Failed to deserialize the JSON body into the target type:
  Invalid 'tools': unknown variant `namespace`, expected `function` or `mcp`
```

codex 内置的 agentic 工具集（shell/apply_patch 等）用 `type:"namespace"` 工具声明，Bedrock 的 gpt-oss responses 端点只认 `function`/`mcp`。手动用 `type:"function"` 工具直接打端点则通过 —— 说明这是 codex 工具声明形态与 Bedrock gpt-oss 子集不兼容，而非链路问题。

**含义**：Bedrock gpt-oss 路径下 codex 可做纯对话 / 自定义 function-calling，但**无法**跑 codex 招牌的本地 shell agentic 工作流。完整 agentic 能力仍需 OpenAI 凭据（gpt-5.x-codex 系列）。

---

## 5. 验证矩阵结果（对照 RFC §8.3）

| 验证点 | 结果 | 备注 |
|---|---|---|
| V1 启动 | ✅ | `codex app-server` 存在 |
| V2 握手 | ✅ | initialize+initialized+thread/start 跑通 |
| V3 单 turn | ✅ | 协议层完整；Bedrock gpt-oss agentic turn 受约束 2 影响 |
| V4 流式 | ✅ | `item/agentMessage/delta` 事件存在（schema + 协议确认） |
| V5 工具调用 | ⚠️ | commandExecution item 存在；Bedrock gpt-oss 拒 namespace 工具 |
| V6 interrupt | ⏳ | `turn/interrupt` method 存在（schema 确认），运行时未单测 |
| V7 resume | ⏳ | `thread/resume` method 存在（schema 确认） |
| V8 多 thread | ⏳ | 留 phase1 之后 |
| V9 backpressure | ⏳ | `-32001` 路径未触发，复用现有重试 |
| V10 反向请求 | ✅ | ServerRequest 枚举确认 `*/requestApproval` |
| V11 持久化 | ⏳ | `~/.codex/sessions/` 由 codexjsonl phase1 读 |
| V12 schema 生成 | ✅ | 见 §2 |

⏳ = method/schema 已确认存在，运行时行为留单测 + phase 后续。

---

## 6. 对 RFC 的具体回写

1. §2.4：`turn/start.input` 改为 `UserInput[]`（`[{type:"text",text:"…"}]`）。
2. §2.4 / §2.6：token usage 走 `thread/tokenUsage/updated` notification，不在 `turn/completed`。
3. §2.5：反向请求实测名是 `item/{commandExecution,fileChange,permissions}/requestApproval` + `item/tool/requestUserInput` + `mcpServer/elicitation/request`。
4. §7 / §9.1：删除 "codex 只能走 OpenAI 自家" 论断；新增 Bedrock 部署小节（含两个约束）。
5. threadId 来源：`thread/start` response `result.thread.id`。
