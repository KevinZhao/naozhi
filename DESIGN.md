# Naozhi — Claude Code 即时通信网关

基于 Claude Code CLI 的轻量消息路由层，将 CC 的完整能力暴露给即时通信渠道。

## 核心思路

不重造轮子，用 Go 写一个薄薄的消息路由服务，底层直接 spawn 本机已配置好的 `claude` CLI（含 ECC 插件、claude-mem、MCP servers），将完整的 agent 能力暴露给即时通信渠道。

## 架构

```
消息渠道 (飞书/Slack/Telegram/...)
       | webhook
       v
+----------------------------------+
|         Naozhi Gateway (Go)        |
|                                  |
|  Platform Adapter                |  <- 解析各平台消息格式
|       |                          |
|  Session Router                  |  <- session key -> 进程映射 + 并发控制
|       |                          |
|  CLI Wrapper (长生命周期进程)      |  <- stdin/stdout stream-json 持久通信
|       |                          |
|  Session Pool                    |
|    +-- user:alice -> claude 进程  |
|    +-- user:bob   -> claude 进程  |
|    +-- group:xxx  -> claude 进程  |
|                                  |
|  本机 claude CLI                  |
|    + ECC plugins (skills/agents) |
|    + claude-mem (跨会话记忆)      |
|    + MCP servers (Playwright等)  |
|                                  |
|  Cron Scheduler                  |  <- 定时任务调度
+----------------------------------+
```

## stream-json 协议 (已验证)

基于 claude CLI v2.1.80 实测验证，`--input-format stream-json` 支持**长生命周期多轮对话**。

### 进程启动

```bash
claude -p \
  --output-format stream-json \
  --input-format stream-json \
  --verbose \                    # 必须，否则 stream-json 输出报错
  --model sonnet \
  --setting-sources user \       # 只加载用户级配置，跳过 project/local 级 hooks
  --dangerously-skip-permissions
```

**hooks 隔离** (已验证):

死循环根因：**插件的 Stop hooks**（claude-mem 的 summarize + ECC 的 4 个 Stop hooks）在 stream-json 模式下被注入为 user message，触发模型回复循环。

解决方案：`--setting-sources user` — 只加载用户级配置（`~/.claude/settings.json`），跳过 project/local 级别。实测结果：
- MCP servers 全部加载 (claude-mem, eks, playwright, gmail, mysql)
- 30 秒内零 Stop hook 回馈，无死循环
- ECC skills 和 agents 可用

### 输入格式 (stdin, NDJSON)

```json
{"type":"user","message":{"role":"user","content":"你的消息"}}
```

注意：`type` 必须是 `"user"`，`"user_message"` 会被静默忽略。

### 输出格式 (stdout, NDJSON)

```
{"type":"system","subtype":"init","session_id":"uuid",...}   ← 每轮开始
{"type":"assistant","message":{"role":"assistant",...}}       ← 助手回复
{"type":"result","session_id":"uuid","result":"完整文本",...}  ← 本轮结束
```

以 `type=result` 作为一轮对话完成的信号。

### 多轮行为 (已验证)

- 进程在多轮之间保持存活，stdin/stdout 持久连接
- session_id 跨轮次一致，上下文完整保留
- 每轮响应延迟约 1.5-2.0s（无冷启动）
- 关闭 stdin 后进程退出

### 已知陷阱

- **`--verbose` 是必需的**，不加会报 `stream-json requires --verbose`
- **宿主 hooks 会死循环** — 插件的 Stop hooks 在 stream-json 模式下被注入为 user message。已通过 `--setting-sources user` 解决（只加载用户级配置，跳过插件级 Stop hooks）
- **`type: "user_message"` 无效** — 被静默忽略，不报错也不回复

## 请求全链路

一条飞书消息从接收到回复的完整数据流：

```
飞书服务器
  | POST /webhook/feishu (签名 + JSON body)
  v
nginx :443
  | proxy_pass
  v
server.Server :8080
  |
  +-> [mux 路由到 feishu.RegisterRoutes 注册的 handler]
      |
      +-> feishu 内部: Verify(r)           // 签名校验, challenge 直接返回
      +-> feishu 内部: ParseEvent(r)       // -> IncomingMessage{EventID, UserID, ChatID, Text}
      +-> w.WriteHeader(200)               // 立即返回 (飞书要求 3s 内)
      |
      +-> go handler(ctx, msg)             // feishu 内部启 goroutine, 以下异步
          |
          +-> dedup.Seen(msg.EventID)      // 幂等检查
          |
          +-> router.GetOrCreate("feishu:direct:{UserID}")
          |   |
          |   +-> [已有活跃进程]  -> 返回 session
          |   +-> [无进程]       -> cli.Spawn() -> 等 init 事件 -> 返回 session
          |   +-> [进程已死]     -> cli.Spawn(resume=session_id) -> 返回 session
          |
          +-> session.Send(ctx, msg.Text)
          |   |
          |   +-> msgQueue <- text         // 入队 (串行保证)
          |   +-> process.stdin.Write(NDJSON)
          |   |   {"type":"user","message":{"role":"user","content":"..."}}
          |   |
          |   +-> process.stdout 逐行读取
          |   |   {"type":"system","subtype":"init",...}    // 每轮开始
          |   |   {"type":"assistant","thinking",...}        // 思考中 -> 推 "思考中..."
          |   |   {"type":"assistant","tool_use",...}        // 工具调用 (可能多次)
          |   |   {"type":"assistant","text",...}            // 最终文本
          |   |   {"type":"result","result":"完整文本",...}   // 本轮结束 <- 取这个
          |   |
          |   +-> return { result, firstActivityAt }
          |
          +-> feishu.Reply(ctx, OutgoingMessage{ChatID, Text: "思考中..."})
          |   // 收到第一个 thinking/tool_use 时立即发送
          |
          +-> feishu.EditMessage(ctx, msgID, finalResult)
              // 收到 result 后编辑为最终回复
              +-> [len > MaxReplyLength()] -> 分割为多条
```

关键约束：
- 飞书 webhook 要求 3s 内返回 200，否则重试。所以步骤 3a 必须立即响应，实际处理异步进行
- 同一 session 的消息必须串行（排队），避免 claude 上下文混乱
- 进程崩溃时通过 `--resume` 恢复，对用户透明 (已验证：SIGKILL 后 resume 上下文完整保留)

## 进程生命周期

claude CLI 子进程的状态机：

```
                      首条消息到达
                          |
                          v
                    +----------+
         +--------->|  Spawning |
         |          +----+-----+
         |               |
         |          init 事件收到 (获取 session_id)
         |               |
         |               v
         |          +----+-----+
         |          |  Ready   |<--------+
         |          +----+-----+         |
         |               |               |
         |          stdin 写入消息     result 收到
         |               |               |
         |               v               |
         |          +----+-----+         |
         |          | Running  |---------+
         |          +----+-----+
         |               |
         |     进程异常退出 / 超时无输出
         |               |
         |               v
  有 session_id    +-----+------+
  --resume 恢复    |   Dead     |
         |         +-----+------+
         |               |
         +---------+     | 无 session_id
                         v
                   +-----+------+
                   |  Discarded |
                   +------------+

空闲超时 (Ready 状态超过 TTL, 默认 30min):
  Ready ---> 关闭 stdin ---> 进程退出 ---> Dead (保留 session_id, 下次 --resume 恢复)
```

状态说明：
- **Spawning**: 进程启动中，等待 init 事件。超时 10s 未收到 init 则 kill
- **Ready**: 进程空闲，可接受新消息
- **Running**: 正在处理消息，等待 result。设置 watchdog: 无输出超时 120s + 总超时 300s
- **Dead**: 进程已退出，保留 session_id 供 resume
- **Discarded**: 无法恢复，丢弃

## 模块设计

### 1. CLI Wrapper (~200 行)

核心协议层，管理长生命周期的 claude CLI 子进程。

- 启动持久进程，通过 stdin/stdout 持续通信
- 使用本机 CLI (`~/.local/bin/claude`)，继承完整插件生态
- 解析 stream-json NDJSON 输出流，以 `type=result` 标记轮次完成
- 进程健康检查、异常重启、优雅关闭

```go
type Process struct {
    cmd     *exec.Cmd
    stdin   io.WriteCloser
    stdout  io.ReadCloser
    session string // claude session_id, 首次 init 事件获取
}

type CLIWrapper struct {
    CLIPath string // 默认 ~/.local/bin/claude
}

// 启动长生命周期进程
func (c *CLIWrapper) Spawn(ctx context.Context, opts SpawnOptions) (*Process, error)

// 向持久进程发送消息，返回本轮 result
func (p *Process) Send(ctx context.Context, text string) (<-chan Event, error)

// 关闭进程
func (p *Process) Close() error
```

### 2. Session Router (~300 行)

管理 session key 到长生命周期 claude 进程的映射。

- session key: `{channel}:{chatType}:{id}:{agentId}`
  - 如 `feishu:direct:alice:general`、`feishu:group:xxx:general`
  - Phase 2+: `feishu:direct:alice:code-reviewer`、`feishu:direct:alice:researcher`
- 每个 session key 绑定一个持久 claude 进程
- 空闲超时清理进程（默认 30min），回收后保留 session_id，下次 `--resume` 恢复 (~2s 冷启动)
- 并发控制：信号量限制最大活跃进程数
- 同一 session 的消息串行处理（排队）
- 持久化到 JSON 文件 (`~/.naozhi/sessions.json`)

**并发模型** (已验证: 每个 claude 进程 ~350MB RSS):

```
并发 = 同时存在的 claude 进程数 (Ready + Running)
     = 同时活跃的 IM 对话数

t4g.small (2GB):  max_procs=3, 进程占 ~1050MB, 剩余 ~950MB
t4g.medium (4GB): max_procs=8, 更从容

超出 max_procs 时新消息排队等待，不会拒绝。
```

典型场景:
```
alice 发消息    -> spawn 进程 A           [1/3]
bob 发消息      -> spawn 进程 B           [2/3]
群聊 xxx        -> spawn 进程 C           [3/3]
charlie 发消息  -> 排队等待               [queue]
alice 空闲 30min -> 进程 A 回收 (保留 session_id) [2/3]
charlie 出队    -> spawn 进程 D           [3/3]
alice 再来      -> --resume 恢复 (~2s)    [3/3 或排队]
```

```go
type SessionRouter struct {
    mu       sync.Mutex
    sessions map[string]*ManagedSession // key: session key
    maxProcs int                        // 最大并发进程数
    ttl      time.Duration
}

type ManagedSession struct {
    Key        string
    Process    *Process   // 长生命周期 claude 进程
    LastActive time.Time
    Platform   string     // "feishu", "slack"
    UserID     string
    ChatType   string     // "direct" | "group"
    msgQueue   chan string // 串行消息队列
}

// key 示例: "feishu:direct:alice", "feishu:group:xxx"
func (r *SessionRouter) GetOrCreate(key string) (*ManagedSession, error)
func (r *SessionRouter) Cleanup()
func (r *SessionRouter) Save() error   // 持久化到 sessions.json
func (r *SessionRouter) Load() error   // 启动时恢复
```

### 3. Platform Adapter (~200 行/平台)

各 IM 平台的接入层。参考 OpenClaw 的 ChannelPlugin 模式：平台以插件形式注册，声明能力，通过回调推消息。但大幅简化 — OpenClaw 有 20+ adapter 接口，Naozhi 只取核心 3 个。

```go
// --- 核心类型 ---

// Gateway 注入的回调，平台收到消息后调用
type MessageHandler func(ctx context.Context, msg IncomingMessage)

type IncomingMessage struct {
    Platform  string // "feishu", "slack", "telegram"
    EventID   string // 去重用
    UserID    string
    ChatID    string
    ChatType  string // "direct" | "group"
    Text      string
    MentionMe bool
}

type OutgoingMessage struct {
    ChatID   string
    Text     string
    ThreadID string // 可选，支持话题回复的平台使用
}

// --- 平台接口 ---

// 所有平台必须实现
type Platform interface {
    Name() string
    // 注册到 HTTP server，收到消息时调 handler
    RegisterRoutes(mux *http.ServeMux, handler MessageHandler)
    // 发送回复，返回消息 ID（用于后续编辑）
    Reply(ctx context.Context, msg OutgoingMessage) (string, error)
    // 编辑已发送的消息（用于 "思考中..." -> 最终回复）
    EditMessage(ctx context.Context, msgID string, text string) error
    // 消息长度限制 (超过则自动分割)
    MaxReplyLength() int
}

// 可选：需要后台 goroutine 的平台 (long polling, WebSocket)
type RunnablePlatform interface {
    Platform
    Start(handler MessageHandler) error
    Stop() error
}
```

**各平台差异由实现内部消化**：

| 平台 | 接收方式 | 接口类型 | 验证 | 消息限制 | 特殊能力 |
|------|---------|----------|------|---------|---------|
| 飞书 | HTTP webhook | `Platform` | 签名 + challenge | 4000 字符 | 卡片消息 |
| Slack | HTTP webhook | `Platform` | Signing secret | 4000 字符 | Block Kit |
| Telegram | Long polling / webhook | `RunnablePlatform` | Bot token | 4096 字符 | Markdown |
| Discord | WebSocket gateway | `RunnablePlatform` | Bot token | 2000 字符 | Embed |

**Gateway 启动流程**：

```go
func (s *Server) Start() {
    handler := s.buildMessageHandler() // 统一处理: 去重 -> 路由 -> session -> reply

    for _, p := range s.platforms {
        // webhook 类: 注册路由
        p.RegisterRoutes(s.mux, handler)

        // polling/ws 类: 启动后台 goroutine
        if rp, ok := p.(RunnablePlatform); ok {
            rp.Start(handler)
        }
    }
}

func (s *Server) buildMessageHandler() MessageHandler {
    return func(ctx context.Context, msg IncomingMessage) {
        if s.dedup.Seen(msg.EventID) {
            return
        }
        session, _ := s.router.GetOrCreate(msg.SessionKey())
        result, _ := session.Send(ctx, msg.Text)
        s.platforms[msg.Platform].Reply(ctx, OutgoingMessage{
            ChatID: msg.ChatID,
            Text:   result,
        })
    }
}
```

**新增平台只需**：
1. 实现 `Platform` 或 `RunnablePlatform`
2. config.yaml 加配置
3. main.go 注册

**与 OpenClaw 的取舍**：
- 采纳: 回调推消息模式、平台声明能力、outbound 分离
- 不采纳: 20+ adapter 接口、schema contribution、binding rules、streaming adapter
- 未来按需扩展: threading adapter (Slack threads)、media adapter (图片/文件)

首期实现飞书，后续按需加平台。

### 4. Cron Scheduler (可选)

定时任务调度，支持通过消息配置。

- 基于 robfig/cron 库
- 任务结果推送到指定渠道
- 持久化任务列表

### 5. HTTP Server

HTTP 入口层。webhook 路由由各 Platform 的 `RegisterRoutes` 注册，Server 只负责启动和健康检查。

```go
// 路由由 Platform.RegisterRoutes 注册:
//   feishu -> POST /webhook/feishu
//   slack  -> POST /webhook/slack
// Server 自身只注册:
//   GET /health -> {"status":"ok","sessions":3,"uptime":"2h"}

func (s *Server) Start() error {
    handler := s.buildMessageHandler()

    for _, p := range s.platforms {
        p.RegisterRoutes(s.mux, handler)
        if rp, ok := p.(RunnablePlatform); ok {
            rp.Start(handler)
        }
    }

    s.mux.HandleFunc("/health", s.handleHealth)
    return http.ListenAndServe(s.addr, s.mux)
}
```

## 项目结构

```
naozhi/
├── cmd/
│   └── naozhi/
│       └── main.go              <- 入口：加载配置、启动 HTTP server
├── internal/
│   ├── cli/
│   │   ├── wrapper.go           <- CLIWrapper: Spawn/Close
│   │   ├── process.go           <- Process: Send, stdin/stdout 管理
│   │   └── event.go             <- Event 类型定义 (init/assistant/result)
│   ├── session/
│   │   ├── router.go            <- SessionRouter: GetOrCreate/Cleanup
│   │   ├── managed.go           <- ManagedSession: 消息队列、生命周期
│   │   └── store.go             <- sessions.json 持久化
│   ├── platform/
│   │   ├── platform.go          <- Platform / RunnablePlatform 接口
│   │   ├── handler.go           <- MessageHandler + 去重逻辑
│   │   └── feishu/
│   │       ├── feishu.go        <- Platform 接口实现 (RegisterRoutes + Reply)
│   │       ├── api.go           <- 飞书 API 客户端 (发消息/上传)
│   │       └── verify.go        <- 签名校验 + challenge
│   ├── config/
│   │   └── config.go            <- YAML 配置加载
│   └── server/
│       ├── server.go            <- HTTP server + 路由
│       └── health.go            <- 健康检查
├── config.yaml                  <- 默认配置
├── go.mod
└── go.sum
```

预估行数：
- `internal/cli/`: ~200 行
- `internal/session/`: ~300 行
- `internal/platform/feishu/`: ~200 行
- `internal/server/`: ~100 行
- `internal/config/`: ~50 行
- `cmd/naozhi/main.go`: ~50 行
- **总计: ~900 行**

## 配置格式

```yaml
# ~/.naozhi/config.yaml

server:
  addr: ":8080"

cli:
  path: "~/.local/bin/claude"    # claude 二进制路径
  model: "sonnet"                # 默认模型
  args:                          # 额外 CLI 参数
    - "--dangerously-skip-permissions"

session:
  max_procs: 3                   # 最大并发 claude 进程数 (每进程 ~350MB)
  ttl: "30m"                     # 空闲 session 回收超时 (回收后保留 session_id, resume 恢复)
  watchdog:
    no_output_timeout: "120s"    # 无输出超时 (kill 进程)
    total_timeout: "300s"        # 单轮总超时
  store_path: "~/.naozhi/sessions.json"

platforms:
  feishu:
    app_id: "${IM_APP_ID}"
    app_secret: "${IM_APP_SECRET}"
    verification_token: "${IM_VERIFICATION_TOKEN}"
    encrypt_key: "${IM_ENCRYPT_KEY}"     # 可选
    max_reply_length: 4000                    # 超过则分割

log:
  level: "info"                  # debug/info/warn/error
  file: "~/.naozhi/logs/naozhi.log"  # 为空则输出 stdout
```

环境变量支持 `${VAR}` 语法，敏感信息不写入文件。

## 与 OpenClaw 架构对比 (参考分析)

分析 OpenClaw 源码，理解其设计取舍，作为 Naozhi 的参考而非模板。

### 根本差异：谁拥有 Agent Loop

这是两个架构最本质的区别：

- **OpenClaw**: 自己拥有完整的 agent loop。CLI 只是一个"哑"的 LLM 调用后端，工具被禁用 (`"Tools are disabled in this session"`)。所有智能（工具调用、记忆、上下文管理）由 OpenClaw 自己实现。
- **Naozhi**: 将整个 agent loop 委托给 claude CLI。CLI 拥有完整的工具执行、context 管理、ECC 技能。Naozhi 只是一个消息路由薄层。

```
OpenClaw 架构:                         Naozhi 架构:
+------------------+                   +------------------+
| OpenClaw Gateway |                   | Naozhi Gateway     |
| (Node.js, 大型)  |                   | (Go, <1000 行)   |
|                  |                   |                  |
| Agent Loop  <----|-- 核心智能在这里    | 消息路由          |
| Tool System      |                   | Session 管理      |
| Memory (LanceDB) |                   |                  |
| Skill Engine     |                   +--------|---------+
| Context Mgmt     |                            |
+--------|---------+                   +--------v---------+
         |                             | claude CLI       |
+--------v---------+                   | (完整 agent)     |
| claude CLI       |                   |                  |
| (禁用工具)       |                   | Agent Loop  <----|-- 核心智能在这里
| 纯 LLM 输出      |                   | ECC Skills       |
+------------------+                   | claude-mem       |
                                       | MCP Servers      |
                                       +------------------+
```

### CLI 调用方式对比

| 维度 | OpenClaw | Naozhi |
|------|---------|------|
| **进程模型** | 短生命周期 (每轮 spawn + exit) | 长生命周期 (stdin/stdout 持久) |
| **CLI 参数** | `-p --output-format json` | `-p --output-format stream-json --input-format stream-json --verbose` |
| **输入方式** | CLI 参数传 prompt (`input: "arg"`) | stdin NDJSON 流式写入 |
| **输出方式** | stdout 一次性 JSON | stdout NDJSON 流式读取 |
| **会话恢复** | `--resume {sessionId}` (每轮) | 同一进程，无需恢复 |
| **工具** | 禁用 (`"Tools are disabled"`) | 启用 (完整 ECC 工具链) |
| **权限** | `--dangerously-skip-permissions` | `--dangerously-skip-permissions` |
| **并发策略** | 串行队列 (`serialize: true`) | 每 session 串行，跨 session 并行 |
| **超时** | watchdog (无输出超时 + 总超时) | 需自建 watchdog |
| **冷启动** | 每轮 2-5s | 仅首次 2-5s，后续 1.5-2s |

### 功能层对比

| 维度 | OpenClaw | Naozhi |
|------|---------|------|
| **代码量** | ~100K+ 行 Node.js | < 1000 行 Go |
| **Agent Loop** | 自建 (auto-reply, dispatch) | 委托 claude CLI |
| **AI 模型** | 多后端 (Claude/Codex/内嵌 PI) | Claude CLI 单后端 |
| **工具系统** | 自建 (bash-tools, channel-tools) | Claude CLI 原生 (Bash/Edit/Read/...) |
| **代码能力** | 靠 skill 补 | ECC 原生 (review/build/test) |
| **记忆** | 自建 (LanceDB/SQLite + embeddings) | claude-mem (MCP) |
| **Skills** | 5400+ 社区 PI skills | ECC 精选 + 自定义 |
| **渠道** | 30+ 插件 (Slack/Telegram/Discord/...) | 首期飞书，按需扩展 |
| **消息处理** | 分块流式、typing 状态、draft 缓冲 | 等待 result 后一次发送 |
| **Session 存储** | sessions.json (文件 + 内存缓存) | sessions.json |
| **Session Key** | `agent:{id}:{channel}:{type}:{peer}` | `{channel}:{chatType}:{id}` |
| **路由** | 复杂绑定 (channel/account/peer/guild/role) | 简单映射 |
| **配置热重载** | 支持 | Phase 2+ |
| **部署** | Node 24 + daemon | Go 二进制 + systemd |

### 从 OpenClaw 学到的

1. **sessions.json 文件存储够用** — OpenClaw 500 条上限 + TTL 清理，单文件无需数据库
2. **串行化执行很重要** — 同一 session 的消息必须排队，避免上下文混乱
3. **watchdog 超时机制** — CLI 可能挂起，需要无输出超时 + 总超时双保险
4. **session key 的可扩展性** — 结构化 key 方便后续按渠道/用户/群聊维度查询

### 有意不采纳的

1. **复杂路由绑定** — Naozhi 场景简单，不需要 channel/account/peer/guild/role 多级绑定
2. **多后端 failover** — 只用 Claude，不需要模型切换和降级
3. **禁用 CLI 工具** — 这是 Naozhi 的核心优势，必须保留
4. **短生命周期进程** — 冷启动开销大且无法保持工具状态

## 关键决策

1. **Go + stream-json 长生命周期进程** — 最终选型，理由见下方
2. **为什么用 Go？** — 团队主力语言，单二进制部署，和 sleep 服务共用基础设施
3. **为什么 spawn CLI 而非直接调 API？** — CLI 包含完整 agent loop、工具执行、context 管理，自己实现成本极高
4. **为什么选长生命周期进程？** — 消除每轮冷启动延迟，保持完整工具能力（短生命周期需要禁用工具才能稳定工作）
5. **为什么 stream-json 而非 json？** — json 模式每次只能单次输出后退出，stream-json 支持持久 stdin/stdout 多轮通信。stream-json 是 CLI 官方文档化的格式，Agent SDK 内部也使用同一协议

### 选型理由

评估了三条可行路径后确定 **方案 C (Go + stream-json)**：

- **方案 A (Channels) 不可行** — Bedrock 认证下 channel notification 不投递，是技术限制非策略限制
- **方案 B (Agent SDK) 可行但延迟过高** — 每轮 ~7s (init 3s + api 2-4s)，IM 场景体验差
- **方案 C (stream-json) 延迟最优** — 后续轮均值 1.5s，4-5 倍优势
- stream-json 是 CLI 官方格式（输出端有文档），Agent SDK 内部也用同一协议，稳定性风险可控

---

## 备选方案评估记录 (2026-03-20)

Claude Code v2.1.76-2.1.80 引入了两个可能改变架构方向的新能力。经实测验证后排除，确定 Go + stream-json 方案。

### 方案 A: Channels (v2.1.80, research preview)

**原理**: Channel 是一个声明了 `claude/channel` capability 的 MCP server，可以将外部消息推进运行中的 Claude Code session。官方已内置 Telegram、Discord channel。

**架构**:
```
飞书 webhook
    |
    v
Feishu Channel (MCP server, ~100-200 行 TS)
    | notifications/claude/channel   (push 消息进 session)
    v
Claude Code session (claude --channels plugin:feishu)
    | reply tool                     (调飞书 API 回复)
    v
飞书用户
```

**核心机制** (基于官方文档分析):
- Channel 通过 MCP stdio 与 Claude Code 通信
- 推送: `mcp.notification({ method: "notifications/claude/channel", params: { content, meta } })`
- 回复: 暴露标准 MCP tool (如 `reply`)，Claude 调用后发送到平台
- 消息到达 Claude 上下文为 `<channel source="feishu" chat_id="xxx">消息内容</channel>`
- 安全: sender allowlist 防止 prompt injection

**限制**:
- 必须 claude.ai 登录（不支持 API key / Bedrock）
- Research preview，协议可能变更
- 自定义 channel 需要 `--dangerously-load-development-channels`
- 需要一个持续运行的 Claude Code session（谁启动/管理它？）
- 只有 Anthropic allowlist 中的 plugin 可以不加 development flag

**待验证**:
- [x] Channels 是否支持 Bedrock 认证 → **不支持**，见下方实测
- [ ] 长时间运行的稳定性
- [ ] 多用户并发时的 session 隔离（channel 只推进一个 session）

**Bedrock 认证实测** (2026-03-20, v2.1.80, auth=bedrock):

MCP server 可以正常连接 (`mcp=[{"name":"test-channel","status":"connected"}]`)，channel HTTP 端点可以接收 POST (`200 ok`)。但 `mcp.notification()` 推送的消息**不会到达 Claude 的上下文**。Claude 明确表示没有收到任何 `<channel>` 标签。

验证步骤:
1. 创建最小 channel MCP server (声明 `claude/channel` capability + HTTP 监听 + notification 推送)
2. 用 `--dangerously-load-development-channels server:test-channel` 启动
3. MCP server 连接成功
4. 通过 HTTP POST 触发 `mcp.notification({ method: "notifications/claude/channel", ... })`
5. HTTP 返回 200，但 Claude 的下一轮对话中没有 `<channel>` 标签出现

**结论**: 文档所说 "They require claude.ai login" 是真实的技术限制，不仅是策略限制。Channel notification 的投递机制在 Bedrock 认证下被禁用。**方案 A 在当前认证环境下不可行。**

### 方案 B: Claude Agent SDK (原 Claude Code SDK)

**背景**: SDK 近期改名为 Claude Agent SDK (TS: `@anthropic-ai/claude-agent-sdk`, Python: `claude-agent-sdk`)，反映其从 coding 工具扩展为通用 agent 框架。

**关键 breaking changes** (v0.0.x → v0.1.0+):
- 不再默认加载 Claude Code system prompt — 需显式 `systemPrompt: { type: "preset", preset: "claude_code" }`
- 不再默认读取文件系统配置 — 需显式 `settingSources: ["user", "project", "local"]`
- `ClaudeCodeOptions` → `ClaudeAgentOptions`

**实测验证** (2026-03-20, SDK v0.2.80):

SDK 捆绑了独立的 `cli.js` (12MB, Claude Code v2.1.80 的 Node.js 版本)，不使用本机 `~/.local/bin/claude` 二进制。但通过配置可以加载本机所有设置：

| 测试场景 | settingSources | systemPrompt | MCP Servers | 工具/Skills |
|---------|---------------|-------------|-------------|------------|
| 默认 (无配置) | 不加载 | SDK 默认 | 无 | 基础工具 |
| 加载用户配置 | `["user", "project", "local"]` | SDK 默认 | **全部可见** (eks, gmail, playwright, claude-mem) | 基础工具 |
| 完整 CC 模式 | `["user", "project", "local"]` | `preset: "claude_code"` | **全部可见** | **完整工具链** (Agent, Bash, Skill, Cron, LSP...) + ECC Skills |

**结论**: `settingSources: ["user", "project", "local"]` + `preset: "claude_code"` 后，SDK 加载了本机 `~/.claude/` 下的全部配置，包括:
- 所有 MCP servers (eks-mcp-server, gmail, playwright, claude-mem)
- ECC skills 和 agents
- CLAUDE.md 指令
- Hooks 配置

**这推翻了之前的假设**: "SDK 捆绑独立 CLI 二进制，无法使用本机已配置的 ECC/claude-mem/MCP" — 实际上可以。

**架构**:
```python
from claude_agent_sdk import query, ClaudeAgentOptions

# 飞书 webhook handler
async def handle_feishu_message(user_id, text):
    options = ClaudeAgentOptions(
        resume=get_session_id(user_id),          # session 恢复
        setting_sources=["user", "project", "local"],  # 加载本机配置
        system_prompt={"type": "preset", "preset": "claude_code"},
        permission_mode="bypassPermissions",
    )
    async for msg in query(prompt=text, options=options):
        if hasattr(msg, "result"):
            await feishu_reply(user_id, msg.result)
```

**优势**:
- 官方 SDK，API 稳定性有保证
- 内置 session resume/fork
- Python/TypeScript 可选
- 不依赖未公开的 stream-json 协议

**限制**:
- 非 Go（Python 需 >=3.10，或用 TypeScript）
- 捆绑自己的 CLI 二进制 (12MB)，非本机原生 claude
- 每次 query 是短生命周期调用 (spawn → process → exit)，有冷启动开销
- 需自建 session 管理和并发控制

**待验证**:
- [x] resume 模式下的冷启动延迟实测 → 见下方
- [ ] 并发多 session 的资源消耗
- [ ] hooks 在 SDK 模式下是否正常工作
- [ ] Bedrock 认证在 SDK 中的表现

**延迟实测** (2026-03-20, SDK v0.2.80 vs stream-json, model=sonnet, allowedTools=[], 同一台机器):

Agent SDK (每次 query spawn 新进程):
```
fresh-1:         6.8s  (init 3.0s + api 2.0s)
resume-1:        7.8s  (init 2.9s + api 3.1s)
resume-2 (ctx):  8.9s  (init 3.4s + api 3.7s)
fresh-2:         7.2s  (init 2.7s + api 2.7s)
resume-3:        7.0s  (init 3.1s + api 2.1s)
```

stream-json 长生命周期进程 (stdin/stdout 持久):
```
turn 1 (fresh):      1.9s
turn 2 (subsequent): 1.5s
turn 3 (subsequent): 1.3s
turn 4 (ctx check):  2.0s
turn 5 (subsequent): 1.3s
avg turn 2-5:        1.5s
```

**关键发现**:
- SDK 的 init 开销约 3s，每次 query 都要付出（spawn → 加载配置 → 恢复 session）
- resume 不比 fresh 快，反而因加载历史上下文略慢
- stream-json 首轮 1.9s，后续均值 1.5s，因为进程常驻无需重复初始化
- **SDK 每轮延迟是 stream-json 的 4-5 倍** (7s vs 1.5s)

### 三条路径对比

| 维度 | A. Channels | B. Agent SDK | C. Go + stream-json (当前) |
|------|------------|-------------|--------------------------|
| **可行性** | **❌ Bedrock 不可用** | ✓ 已验证 | ✓ 已验证 |
| **代码量** | ~100-200 行 TS | ~300-500 行 TS/Python | ~1000 行 Go |
| **协议稳定性** | research preview | 官方 SDK | CLI 官方格式，输出有文档，输入格式无文档 |
| **进程模型** | 由 CC 管理 | 短生命周期 (每次 spawn) | 长生命周期 (持久 stdin/stdout) |
| **认证** | ❌ claude.ai only | API key / Bedrock / Vertex | Bedrock (本机 CLI) |
| **语言** | TypeScript | Python / TypeScript | Go |
| **本机配置** | 完全继承 (CC 原生) | 需显式 `settingSources` | 需 `--setting-sources ""` 隔离 |
| **Session 管理** | CC 内置 (单 session) | SDK 内置 (resume/fork) | 需自建 |
| **并发** | 单 session 限制 | 需自建 | 需自建 |
| **冷启动** | 无 (CC 常驻) | **~3s/轮 (每次 spawn)** | **仅首次 1.9s, 后续 1.5s** |
| **工具能力** | 完整 CC 工具 | 完整 CC 工具 (需配置) | 完整 CC 工具 (需 `--setting-sources ""`) |
| **成熟度** | Preview | GA (v0.1.0+) | 自建 |

## 会话管理

**重置对话**：
- 用户发送 `/new` → 丢弃当前 session_id，下次消息 spawn 全新 claude 进程（无上下文）
- TTL 回收 (30min 空闲) → 进程关闭但**保留** session_id，下次消息 `--resume` 恢复上下文

两者区别：TTL 回收是"暂停"，`/new` 是"重来"。

**消息格式** (Phase 1 scope)：
- 入站：只取纯文本，@mention 去掉 bot 名字前缀，忽略图片/文件/卡片
- 出站：纯文本回复，超过 MaxReplyLength 自动分割
- 后续扩展：图片入站（claude 可以看图）、卡片回复（代码块排版）

**回复策略** (已验证 stream-json 事件流):

stream-json 在多工具任务中输出的关键事件：
```
 0s  system/init
 5s  assistant/thinking       <- 模型开始思考
 6s  assistant/tool_use       <- 调用工具 (可能多次)
17s  user/tool_result         <- 工具返回
20s  assistant/text           <- 最终文本
20s  result                   <- 轮次结束
```

Phase 1 采用**策略 4: thinking 推一次 + result 推最终**：
```
用户发消息 → 飞书回复 "思考中..." → (等待 claude 完成) → 飞书编辑为最终回复
```
- 收到第一个 `assistant/thinking` 或 `assistant/tool_use` 时，发一条"思考中..."
- 收到 `result` 时，编辑该消息为最终回复（或发新消息）
- 用户至少知道 bot 在处理，不会觉得没反应

后续可升级为策略 2（tool_use 时推详细进度："正在读取文件..."、"正在执行命令..."）

**用户鉴权**：
- 不在 Naozhi 层做鉴权，依赖飞书应用自身的可见范围设置
- 飞书管理后台控制哪些用户/部门可以看到和使用该应用

## Agent 路由

不同指令/场景路由到不同的 claude agent，每个 agent 有独立的 session 和 system prompt。

**路由规则**：

```
用户消息                          agent         说明
/review PR#123                -> code-reviewer  代码审查
/research 量子计算最新进展       -> researcher    深度调研
/ops 检查线上服务状态            -> ops           运维操作
普通消息                        -> general       默认对话
```

**实现**：
- session key 扩展为 `{platform}:{chatType}:{userId}:{agentId}`
- 不同 agent 的 session 相互独立（各自的 claude 进程和上下文）
- 同一用户可以同时有多个 agent session

**指令解析**（在 MessageHandler 中）：

```go
func resolveAgent(text string) (agentId string, cleanText string) {
    // "/review PR#123" -> ("code-reviewer", "PR#123")
    // "/research xxx"  -> ("researcher", "xxx")
    // "普通消息"        -> ("general", "普通消息")
    if strings.HasPrefix(text, "/") {
        parts := strings.SplitN(text, " ", 2)
        cmd := strings.TrimPrefix(parts[0], "/")
        rest := ""
        if len(parts) > 1 { rest = parts[1] }
        if agentId, ok := agentCommands[cmd]; ok {
            return agentId, rest
        }
    }
    return "general", text
}
```

**Agent 配置**：

```yaml
# config.yaml
agents:
  general:
    model: "sonnet"
    args: []                         # 使用默认 system prompt
  code-reviewer:
    model: "sonnet"
    args: ["--agent", "code-reviewer"]
  researcher:
    model: "opus"
    args: ["--agent", "researcher", "--effort", "max"]
  ops:
    model: "sonnet"
    args: ["--append-system-prompt", "你是运维专家，可以使用 Bash 工具检查服务状态"]

# 指令 -> agent 映射
agent_commands:
  review: "code-reviewer"
  research: "researcher"
  ops: "ops"
```

**进程模型变化**：

```
Session Pool (之前: 1 用户 = 1 进程)
+-- feishu:direct:alice:general        -> claude 进程 A (sonnet)
+-- feishu:direct:alice:code-reviewer  -> claude 进程 B (sonnet, --agent code-reviewer)
+-- feishu:direct:alice:researcher     -> claude 进程 C (opus, --agent researcher)
```

同一用户的不同 agent 是独立进程、独立上下文、可以用不同模型。
并发限制按总进程数（不按用户），超出排队。

**Phase 1 scope**: 只实现 `general` 默认 agent，不做指令路由。
**Phase 2+**: 加指令解析和多 agent 配置。

## 优雅关闭

Gateway 收到 SIGTERM 时：

```
1. 停止接受新 webhook (listener 关闭)
2. 等待所有 Running 状态的 session 完成当前轮次 (超时 30s)
3. 保存 sessions.json (所有 session_id 持久化)
4. 关闭所有 claude 进程的 stdin (进程自然退出)
5. 退出
```

重启后：从 sessions.json 恢复 session key → session_id 映射，所有进程状态为 Dead，下次消息 `--resume` 恢复。

## 可观测性

**日志关键事件** (结构化 JSON):
- `session.created` / `session.resumed` / `session.expired` / `session.reset`
- `process.spawned` / `process.crashed` / `process.killed`
- `message.received` / `message.replied` / `message.queued`
- `error.*`

**健康检查** (`GET /health`):
```json
{
  "status": "ok",
  "uptime": "2h30m",
  "sessions": { "active": 2, "total": 15 },
  "queue_depth": 0,
  "avg_latency_ms": 1800
}
```

## 测试策略

| 层 | 方式 | 说明 |
|----|------|------|
| CLI Wrapper | 集成测试 | spawn 真实 claude 进程，验证 send/result/resume |
| Session Router | 单元测试 | mock Process 接口，测 GetOrCreate/Cleanup/并发 |
| Feishu Platform | 单元测试 | mock HTTP request + mock 飞书 API |
| 端到端 | 集成测试 | mock Platform (HTTP POST 发消息 → 验证回复) |

## 风险

- **stream-json 协议变化** — CLI 官方格式，输出端有文档，但输入格式和完整事件 schema 无文档。Agent SDK 内部使用同一协议，协议稳定性与 SDK 一致
- **并发限制** — 每个 claude 进程 ~350MB RSS (已验证)。t4g.small (2GB) 建议限制 2-3 并发，超出排队
- **CLI 版本锁定** — 建议固定 CLI 版本，测试通过后再升级
- **hooks 隔离** — 已通过 `--setting-sources user` 解决。插件 Stop hooks 不加载，MCP/skills 正常
- **进程僵尸** — 需定期健康检查，清理无响应进程

## 部署架构

```
飞书服务器
  | HTTPS
  v
Route 53: naozhi.example.com (Alias -> CloudFront)
  |
  v
CloudFront (E3RXEY31SMIN3M)
  | CachingDisabled, AllMethods
  | HTTPS 终结 (*.example.com 证书, us-east-1)
  v
ALB (naozhi-alb, SG: 仅 CloudFront prefix list)
  | HTTP :80
  v
EC2 t4g.small (独立实例, 无开发凭据)
  | :8180
  v
naozhi (Go 二进制, systemd)
  | stdin/stdout stream-json
  v
claude CLI (Bedrock 直连, IAM role 认证)
```

**安全设计**:
- ALB SG 仅允许 CloudFront managed prefix list (`pl-58a04531`)，公网无法直连
- EC2 为专用实例，无 AWS AKSK、无 GitHub 凭据、无开发环境
- Bedrock 通过 IAM role + VPC endpoint 访问，无 access key

**AWS 资源**:
- EC2: t4g.small, Amazon Linux 2023, ARM64
- IAM role: `naozhi-ec2-role` (AmazonBedrockFullAccess + SSM + S3ReadOnly)
- ALB: `naozhi-alb`, internet-facing, 80+443 listeners
- CloudFront: HTTPS终结 + 缓存禁用
- VPC endpoint: bedrock-runtime (SG 需允许 naozhi EC2 SG 访问 443)

## 部署操作

**首次部署** (新 EC2):
```bash
# 1. 启动 EC2 (t4g.small, naozhi-ec2-role, 私有子网)
# 2. Bootstrap
./deploy/bootstrap.sh          # 安装 Node.js, 创建目录

# 3. 部署 claude CLI (通过 S3)
aws s3 cp claude-binary s3://naozhi-deploy-tmp/claude-cli
# 在目标机器: aws s3 cp s3://naozhi-deploy-tmp/claude-cli ~/.local/share/claude/versions/X.Y.Z

# 4. 配置飞书凭据
./deploy/setup-env.sh <instance-id>

# 5. 部署 naozhi
./deploy/deploy.sh deploy      # build + upload + install systemd + start
```

**日常更新**:
```bash
# 修改代码后
./deploy/deploy.sh deploy      # 重新 build + 推送 + 重启

# 查看状态
./deploy/deploy.sh status
./deploy/deploy.sh logs
```

**deploy/ 目录**:
```
deploy/
├── deploy.sh        <- 构建 + 上传 + 部署 (via SSM)
├── setup-env.sh     <- 配置飞书凭据
├── bootstrap.sh     <- 新机器初始化
└── naozhi.service   <- systemd 服务定义
```

## 开发计划

1. Phase 1: CLI Wrapper + 飞书 Platform — 基础消息收发 **[已完成]**
2. Phase 2: Session Router 增强 — 多轮对话 + `/new` 重置 + "思考中..." 进度
3. Phase 3: Agent 路由 — `/review`、`/research` 等指令路由到不同 agent
4. Phase 4: Cron Scheduler — 定时任务
5. Phase 5: 更多 Platform (Slack/Telegram)
