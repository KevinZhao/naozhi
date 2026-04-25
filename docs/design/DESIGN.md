# Naozhi — AI Agent 即时通信网关

基于 AI CLI Agent 的轻量消息路由层，通过可插拔的协议抽象（stream-json / ACP）将 agent 的完整能力暴露给即时通信渠道。

## 核心思路

不重造轮子，用 Go 写一个薄薄的消息路由服务，底层直接 spawn 本机已配置好的 AI CLI agent（Claude CLI 或 Kiro），将完整的 agent 能力暴露给即时通信渠道。

## 架构

```
消息渠道 (飞书/Slack/Discord/...)
       | webhook / websocket
       v
+----------------------------------+
|         Naozhi Gateway (Go)        |
|                                  |
|  Platform Adapter                |  <- 飞书/Slack/Discord 消息解析
|       |                          |
|  Session Router                  |  <- session key -> 进程映射 + 并发控制
|       |                          |
|  CLI Wrapper + Protocol          |  <- 可插拔协议层
|       |                          |
|  Session Pool                    |
|    +-- alice:general   -> 进程 A  |
|    +-- alice:reviewer  -> 进程 B  |
|    +-- group:xxx       -> 进程 C  |
|                                  |
|  AI CLI Backend                  |
|    claude (stream-json)          |
|    kiro   (ACP / JSON-RPC 2.0)  |
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
  --setting-sources "" \         # 跳过所有外部配置，隔离宿主 hooks 防死循环
  --dangerously-skip-permissions
```

> **安全须知: `--dangerously-skip-permissions`**
>
> 此标志授予 Claude CLI 完全的文件系统和命令执行权限，无交互确认。这是网关场景的必要条件
> （IM 消息无法触发权限弹窗），但意味着：
>
> - IM 用户输入的任何消息都会被 Claude 以服务器用户权限执行
> - Prompt injection 攻击可能导致数据泄露或系统损坏
> - `session.cwd` / `allowedRoot` 只限制 `/cd` 命令，不限制 Claude 进程本身的文件访问
>
> **缓解措施：**
> 1. 生产环境必须设置 `server.dashboard_token`
> 2. 使用低权限用户运行 naozhi（非 root）
> 3. 通过 `session.cwd` 限制默认工作目录
> 4. 考虑在容器或 VM 中运行以隔离文件系统
> 5. 配置飞书等平台的机器人可见范围，限制可发送消息的用户

**hooks 隔离** (已验证):

死循环根因：**插件的 Stop hooks**（claude-mem 的 summarize + ECC 的 4 个 Stop hooks）在 stream-json 模式下被注入为 user message，触发模型回复循环。

解决方案：`--setting-sources ""` — 跳过所有外部配置源，隔离宿主 hooks。实测结果：
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
- **宿主 hooks 会死循环** — 插件的 Stop hooks 在 stream-json 模式下被注入为 user message。已通过 `--setting-sources ""` 解决（跳过所有外部配置源）
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
         +---------+     | 无 session_id → 从 router 中清除
                         v
                      (丢弃)

空闲超时 (Ready 状态超过 TTL, 默认 30min):
  Ready ---> 关闭 stdin ---> 进程退出 ---> Dead (保留 session_id, 下次 --resume 恢复)
```

状态说明：
- **Spawning**: 进程启动中，等待 init 事件。超时 10s 未收到 init 则 kill
- **Ready**: 进程空闲，可接受新消息
- **Running**: 正在处理消息，等待 result。启用 Watchdog: 无输出超时 120s（配置默认值）+ 总超时 300s（配置默认值）。任何一个超时触发即杀死进程
- **Dead**: 进程已退出。有 session_id 的保留供 resume；无 session_id 的在 Cleanup 时清除

Watchdog 机制：
- `no_output_timeout`（默认 2min）：若连续无输出事件，杀死进程
- `total_timeout`（默认 5min）：本轮总耗时超限，杀死进程
- 两个定时器任意一个触发则认为挂起，立即终止进程
- 有新事件时重置 no_output_timeout（但不重置 total_timeout）

## 模块设计

### 1. CLI Wrapper + Protocol Layer

管理长生命周期 AI CLI 子进程，通过可插拔的 Protocol 接口适配不同后端。

- 启动持久进程，通过 stdin/stdout 持续通信
- Protocol 接口抽象消息格式、初始化握手、事件解析
- Wrapper.Spawn → Protocol.Init (握手) → startReadLoop
- 进程健康检查、watchdog 超时、优雅关闭

**Protocol 接口**:

```go
type Protocol interface {
    Name() string                                    // "stream-json" | "acp"
    BuildArgs(opts SpawnOptions) []string             // 构建 CLI 启动参数
    Init(rw *JSONRW, resumeID string) (string, error) // 协议握手 (ACP: initialize + session/new)
    WriteMessage(w io.Writer, text string) error      // 写入用户消息
    ReadEvent(line []byte) (Event, bool, error)       // 解析事件 (bool=轮次完成)
    HandleEvent(w io.Writer, ev Event) bool           // 处理内部事件 (如 ACP 权限自动授权)
}
```

**两种实现**:

| 维度 | ClaudeProtocol (stream-json) | ACPProtocol (JSON-RPC 2.0) |
|------|------------------------------|---------------------------|
| 后端 | claude CLI | kiro CLI |
| 输入 | `{"type":"user","message":...}` | `session/prompt` RPC |
| 输出 | NDJSON `type=result` | `session/update` 通知 + RPC response |
| 握手 | 无 (session ID 从 init 事件获取) | `initialize` + `session/new` or `session/load` |
| 恢复 | `--resume {sessionId}` | `session/load` RPC |
| 权限 | `--dangerously-skip-permissions` | auto-grant `session/request_permission` |
| 文本 | result 事件直接携带 | 累积 `agent_message_chunk` 文本 |

```go
type Wrapper struct {
    CLIPath  string
    Protocol Protocol
}

func (w *Wrapper) Spawn(ctx context.Context, opts SpawnOptions) (*Process, error)
func (p *Process) Send(ctx context.Context, text string, onEvent EventCallback) (*SendResult, error)
func (p *Process) Close()
func (p *Process) Kill()
```

### 2. Session Router (~300 行)

管理 session key 到长生命周期 claude 进程的映射。

- session key: `{platform}:{chatType}:{userId}:{agentId}`
  - 如 `feishu:direct:alice:general`、`feishu:group:xxx:general`
  - Agent 支持：`feishu:direct:alice:code-reviewer`、`feishu:direct:alice:researcher`
- 每个 session key 绑定一个持久 claude 进程
- 空闲超时清理进程（默认 30min），回收后保留 session_id，下次 `--resume` 恢复 (~2s 冷启动)
- 并发控制：信号量限制最大活跃进程数，超出排队
- 同一 session 的消息串行处理（排队，通过 sendMu 保护）
- 持久化到 JSON 文件 (`~/.naozhi/sessions.json`)，启动时恢复
- 关闭前等待 running 完成（超时 30s），然后保存 store

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

**并发安全性**：
- `Reset()` 释放锁后再阻塞 Close()，避免死锁
- `evictOldest()` 先从 map 中删除（设 process=nil），再释放锁后调用 Close()，保证其他 goroutine 不会选中该进程
- `getSessionID()` 在 sendMu 下读取，避免 Send() 进行中时的数据竞争

```go
type SessionRouter struct {
    mu       sync.Mutex
    sessions map[string]*ManagedSession // key: session key
    maxProcs int                        // 最大并发进程数
    ttl      time.Duration
}

type ManagedSession struct {
    Key        string
    SessionID  string               // 从 init 事件获取（下次 resume 用）
    process    *cli.Process         // 长生命周期 claude 进程
    LastActive time.Time
    sendMu     sync.Mutex           // 保护消息串行
}

// key 示例: "feishu:direct:alice:general", "feishu:group:xxx:code-reviewer"
func (r *SessionRouter) GetOrCreate(key string, opts AgentOpts) (*ManagedSession, error)
func (r *SessionRouter) Reset(key string)     // 用户 /new 时调用
func (r *SessionRouter) Cleanup()             // 清理空闲 session
func (r *SessionRouter) Shutdown()            // 优雅关闭：等待 running 完成 + 保存 store
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

| 平台 | 接收方式 | 接口类型 | 验证 | 消息限制 | 状态 |
|------|---------|----------|------|---------|------|
| 飞书 | WebSocket (默认) / HTTP webhook | `RunnablePlatform` | 签名 + challenge | 4000 字符 | **已实现** |
| Slack | Socket Mode (WebSocket) | `RunnablePlatform` | Bot/App token | 4000 字符 | **已实现** |
| Discord | WebSocket gateway | `RunnablePlatform` | Bot token | 2000 字符 | **已实现** |
| Telegram | Long polling / webhook | `RunnablePlatform` | Bot token | 4096 字符 | 计划中 |

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
  addr: ":8180"

cli:
  backend: "claude"              # "claude" (default) | "kiro"
  path: "~/.local/bin/claude"    # CLI 二进制路径
  model: "sonnet"                # 默认模型
  args:                          # 额外 CLI 参数
    - "--dangerously-skip-permissions"

session:
  max_procs: 3                   # 最大并发 claude 进程数 (每进程 ~350MB)
  ttl: "30m"                     # 空闲 session 回收超时 (回收后保留 session_id, resume 恢复)
  watchdog:
    no_output_timeout: "120s"    # 无输出超时 (kill 进程), 默认 2min
    total_timeout: "300s"        # 单轮总超时, 默认 5min
  store_path: "~/.naozhi/sessions.json"  # session 持久化路径

# Workspace 身份
workspace:
  id: "my-node"                  # 节点标识 (默认 hostname)
  name: "My Workspace"           # 显示名

# Project 管理 (可选)
projects:
  root: "/home/ec2-user/workspace"  # 项目根目录, 子目录自动成为项目

# 多节点聚合 (可选)
nodes:
  macbook:
    url: "http://10.0.0.2:8180"
    token: "shared-secret"
    display_name: "MacBook Pro"

# 反向连接 (子节点配置, 可选)
upstream:
  url: "wss://primary:8180/ws-node"
  node_id: "remote-1"
  token: "shared-secret"

# Agent 定义: agent_id -> model + args
agents:
  general:
    model: "sonnet"
    args: []
  code-reviewer:
    model: "sonnet"
    args: ["--append-system-prompt", "You are a code review expert..."]
  researcher:
    model: "opus"
    args: ["--append-system-prompt", "You are a research expert..."]

# 命令 -> agent 映射
agent_commands:
  review: "code-reviewer"
  research: "researcher"

platforms:
  feishu:
    app_id: "${IM_APP_ID}"
    app_secret: "${IM_APP_SECRET}"
    connection_mode: "webhook"        # "websocket" (default) | "webhook"
    verification_token: "${IM_VERIFICATION_TOKEN}"
    encrypt_key: "${IM_ENCRYPT_KEY}"   # 可选, 用于 v2 签名校验 (SHA-256)
    max_reply_length: 4000                    # 超过则分割

  slack:
    bot_token: "${SLACK_BOT_TOKEN}"           # xoxb- token
    app_token: "${SLACK_APP_TOKEN}"           # xapp- token for Socket Mode
    max_reply_length: 4000

  discord:
    bot_token: "${DISCORD_BOT_TOKEN}"
    max_reply_length: 2000

log:
  level: "info"                  # debug/info/warn/error
  file: "~/.naozhi/logs/naozhi.log"  # 为空则输出 stdout
```

环境变量支持 `${VAR}` 语法，敏感信息不写入文件。

**Agent 配置说明**：
- `agents`: 定义可用的 agent 及其运行时配置
  - `model`: 使用的 AI 模型（支持覆盖 cli.model）
  - `args`: 额外的 claude CLI 参数（通常用 `--append-system-prompt` 给 system prompt）
- `agent_commands`: 将用户指令映射到 agent
  - 启动时校验：所有 agent_commands 中的 agentId 必须在 agents 中定义

**Feishu 配置说明**：
- `connection_mode`: 传输模式
  - `websocket`: 使用飞书 WebSocket 网关（推荐，无需公网 IP）
  - `webhook`: HTTP webhook（需要公网 IP + HTTPS）
- `encrypt_key`: v2 签名校验密钥
  - 若配置，使用 SHA-256 校验 `X-Lark-Signature` 头
  - 若不配置，跳过签名校验

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
- [x] 长时间运行的稳定性 → N/A，方案 A 已淘汰（Bedrock 不可用）
- [x] 多用户并发时的 session 隔离 → N/A，方案 A 已淘汰

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
- [x] 并发多 session 的资源消耗 → N/A，方案 B 已淘汰（延迟 4-5 倍）
- [x] hooks 在 SDK 模式下是否正常工作 → N/A，方案 B 已淘汰
- [x] Bedrock 认证在 SDK 中的表现 → N/A，方案 B 已淘汰

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
- 同一用户可以同时有多个 agent session（如同时与 code-reviewer 和 researcher 聊天）
- 指令格式：`/{cmd} {args}`，如 `/review file.go`、`/research blockchain`
- Agent 命令无参数时返回提示：`请在指令后输入内容。`
- 指令错误时返回提示：`未知的 agent: {cmd}`
- `/new` 重置 general agent；`/new review` 重置 code-reviewer agent；`/new unknown` 返回错误

**指令解析**（在 server.buildMessageHandler 中）：

```go
// 识别指令格式和 agent 映射
agentID, cleanText := resolveAgent(text, agentCommands)
// "/review PR#123" -> ("code-reviewer", "PR#123")
// "/research xxx"  -> ("researcher", "xxx")
// "普通消息"        -> ("general", "普通消息")

// 使用 agent 配置中的 model + extra args spawn 进程
opts := agents[agentID]
sess, _ := router.GetOrCreate(key, opts)
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
    args: ["--append-system-prompt", "You are a code review expert..."]

  researcher:
    model: "opus"
    args: ["--append-system-prompt", "You are a research expert..."]

# 指令 -> agent 映射
agent_commands:
  review: "code-reviewer"
  research: "researcher"
```

**启动校验**：
- main.go 加载 config 后，遍历 agent_commands，检查每个 agent 是否在 agents map 中存在
- 若有未定义的 agent，启动失败并提示错误

**进程模型变化**：

```
Session Pool (之前: 1 用户 = 1 进程)
+-- feishu:direct:alice:general        -> claude 进程 A (sonnet)
+-- feishu:direct:alice:code-reviewer  -> claude 进程 B (sonnet, --append-system-prompt)
+-- feishu:direct:alice:researcher     -> claude 进程 C (opus, --append-system-prompt)
```

同一用户的不同 agent 是独立进程、独立上下文、可以用不同模型。
并发限制按总进程数（不按用户），超出排队。

**Phase 2+ 扩展**：
- 新增 agent 只需在 config.yaml 中配置 agents + agent_commands，无需改代码
- 支持动态 agent 添加（需要 config 热重载）

## 优雅关闭

Gateway 收到 SIGTERM/SIGINT 时：

```
1. 停止接受新 webhook (listener 关闭)
2. 等待所有 Running 状态的 session 完成当前轮次 (超时 30s)
3. 保存 sessions.json (所有 session_id 持久化)
4. 关闭所有 claude 进程的 stdin (进程自然退出)
5. 退出
```

重启后：从 sessions.json 恢复 session key → session_id 映射，所有进程状态为 Dead，下次消息 `--resume` 恢复。

实现细节：
- `Router.Shutdown()` 持有 mu 等待 running 完成
- 关闭前调用 `saveStore()` 保存 session_id 映射
- 关闭时调用 `Process.Close()` 通过关闭 stdin 让进程优雅退出
- Process.Close() 等待最多 5s，超时后强杀

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
- **hooks 隔离** — 已通过 `--setting-sources ""` 解决。插件 Stop hooks 不加载，MCP/skills 正常
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
   - Stream-json 协议
   - 长生命周期进程管理
   - 飞书 webhook 接入

2. Phase 2: Session Router 增强 **[已完成]**
   - 多轮对话 + `/new` 重置 + "思考中..." 进度提示
   - Watchdog（无输出超时 + 总超时）
   - Session 持久化到 sessions.json，支持重启恢复
   - Graceful shutdown（等待 running → 保存 → 关闭）
   - Feishu v2 签名验证 + WebSocket/Webhook 双传输
   - 并发控制（信号量 + LRU 驱逐）

3. Phase 3: Agent 路由 **[已完成]**
   - Session key 扩展: `{platform}:{chatType}:{userId}:{agentId}`
   - Agent 配置（model + extra args）+ 启动校验
   - 命令解析（`/review` → code-reviewer）

4. Phase 4: 多后端 + 多平台 **[已完成]**
   - Protocol 接口抽象: ClaudeProtocol (stream-json) + ACPProtocol (JSON-RPC 2.0 / Kiro)
   - `cli.backend` 配置切换后端
   - Slack 平台（Socket Mode）
   - Discord 平台（WebSocket gateway）
   - 飞书 WebSocket 传输模式

5. Phase 5: Cron Scheduler — 定时任务 **[已完成]**
6. Phase 6: Multi-Node — 多节点聚合 **[已完成]**
   - NodeClient HTTP 聚合 + WS Relay 实时转发
   - 反向连接 (NAT 穿越): Connector + ReverseNodeConn
7. Phase 7: Workspace → Project → Session 三层组织
   - 7.0: ProjectManager + Planner + IM 路由 **[已完成]**
   - 7.1: Discovered sessions 合并进 sidebar **[已完成]**
   - 7.2: Node 身份 (workspace.id/name) **[已完成]**
   - 7.3: 远程 projects + discovered 聚合 **[已完成]**
   - 7.4: 跨 workspace 操作 proxy

---

## Phase 7: Project (Folder) 组织

### 核心概念

**Project (项目) = 文件系统目录**。`projects_root` 下每个子目录自动成为一个项目，目录名即项目名。项目是 session 的组织单位，也是 IM 消息的默认路由目标。

```
projects_root: /home/ec2-user/workspace/
├── naozhi/                   <- Project "naozhi"
│   └── .naozhi/
│       └── project.yaml      <- 项目配置
├── daydream/                 <- Project "daydream"
│   └── .naozhi/
│       └── project.yaml
└── research/                 <- Project "research"
    └── .naozhi/
        └── project.yaml
```

**核心约束**：
- 一个 session 只属于一个 project（由 workspace 路径决定），不允许跨 project
- Session 内的 claude CLI / kiro 进程可以自由访问任意目录，project 只影响归属展示和 planner 路由
- `projects_root` 全局唯一，在 `config.yaml` 中配置

---

### Planner Agent

每个 Project 有一个专属的 **Planner** — 常驻的 claude CLI 进程，是项目的"主脑"。

**定位**：
- **IM 的默认接入点**：IM 渠道绑定到某个 project 后，消息默认路由到该 project 的 planner
- **项目感知**：workspace 固定为项目路径，可以直接读写项目文件、查看 git 状态
- **协调其他 session**：通过 dashboard API 或 cron 可以向其他 session 发消息
- **Memory 关联**：使用项目专属 memory 文件（`.naozhi/MEMORY.md`）

**Session Key 格式**：

```
project:{projectName}:planner
```

示例：`project:naozhi:planner`、`project:daydream:planner`

**共享上下文**：planner 是 project 级别的，所有绑定到该项目的 IM chat 共享同一个 planner 进程和对话上下文。这是有意为之——planner 代表整个项目的"主脑"，而非单个用户的私有会话。

**生命周期特殊规则**：
- **免 TTL**：不受空闲超时影响。Router 的 Cleanup 跳过 `exempt` session；7×TTL 的死亡裁剪同样跳过
- **不计入 `max_procs`**：Router 用整数计数器 `activeCount + pendingSpawns >= maxProcs` 判断是否超限。`exempt` session 不计入 `activeCount`，也不被 `evictOldest` 选中。planner 进程存在于 Router 的 sessions map 中，但对其他普通 session 的并发配额透明
- **自动重启**：planner 无需特殊监控。`router.GetOrCreate` 对 dead session 的现有处理（自动 `--resume` 重建进程）已经够用。IM 消息到达时直接调 `GetOrCreate("project:naozhi:planner", ...)` 即可；`EnsurePlanner` 仅用于服务启动时可选的预热
- **初始 Prompt**：通过 `--append-system-prompt` 注入，内容从 `project.yaml` 的 `planner_prompt` 字段读取（待设计具体内容）

---

### IM 路由变更

当前路由逻辑（platform + chat → session）扩展为支持 project 绑定：

```
IM 消息到达
    |
    +-> 该 chat 是否绑定了 project?
          |
          +-- 是 -> 路由到 project 的 planner session
          |         key: project:{name}:planner
          |
          +-- 否 -> 原有逻辑 (session key = platform:chatType:chatID:general)
```

**绑定方式**：

1. **命令绑定**（用户在 IM 中发送）：
   ```
   /project naozhi          <- 将当前 chat 绑定到 naozhi 项目的 planner
   /project                 <- 查看当前 chat 的 project 绑定
   /project off             <- 解绑，恢复默认路由
   ```

2. **配置绑定**（`config.yaml` 中静态配置）：
   ```yaml
   projects:
     naozhi:
       chat_bindings:
         - platform: feishu
           chat_id: "xxx"
   ```

绑定信息持久化到各项目的 `.naozhi/project.yaml`（`chat_bindings` 字段），保持项目配置自包含，项目目录迁移时绑定关系随之保留。

**Planner 内的 agent 分发**：

绑定后，普通消息默认去 planner，agent 命令仍然有效。agent session 沿用现有 key 格式，Router 设置 workspace 为项目路径：
```
/review PR#123    -> platform:chatType:chatID:code-reviewer  (workspace 设为项目路径)
普通消息          -> project:{name}:planner
```

agent session 是 per-chat 的（key 含 chatID），planner 是 per-project 的（key 不含 chatID）。

**`/new` 行为**：

在 project 绑定的 chat 中：
```
/new         <- 重置 planner 对话上下文（所有绑定此项目的 chat 共享，均受影响）
/new review  <- 只重置当前 chat 的 code-reviewer session（不影响 planner）
```

---

### 数据模型

#### Project 结构

```go
// internal/project/project.go

type Project struct {
    Name string    // 目录名 (唯一 ID)
    Path string    // 绝对路径 (/home/ec2-user/workspace/naozhi)
    Config ProjectConfig
}

type ProjectConfig struct {
    // Git 同步
    GitSync    bool   `yaml:"git_sync"`
    GitRemote  string `yaml:"git_remote"`

    // Memory
    MemoryFile string `yaml:"memory_file"` // 默认 ".naozhi/MEMORY.md"

    // Planner
    PlannerModel  string `yaml:"planner_model"`  // 默认用全局 model
    PlannerPrompt string `yaml:"planner_prompt"`  // --append-system-prompt 内容 (待设计)

    // IM 绑定 (可选，也可通过命令动态绑定)
    ChatBindings []ChatBinding `yaml:"chat_bindings"`
}

type ChatBinding struct {
    Platform string `yaml:"platform"`
    ChatID   string `yaml:"chat_id"`
    ChatType string `yaml:"chat_type"`
}
```

#### ManagedSession 变更

```go
type ManagedSession struct {
    // ... 现有字段 ...

    // exempt=true: 不计入 activeCount、不被 evictOldest 选中、不被 Cleanup TTL/7×TTL 裁剪
    // 用于 planner session，使其对普通 session 的并发配额透明
    exempt  bool
}
```

---

### ProjectManager 组件

新增 `internal/project/manager.go`：

```go
type Manager struct {
    root     string
    mu       sync.RWMutex
    projects map[string]*Project   // name -> project

    router *session.Router         // 用于 EnsurePlanner
}

// Scan 扫描 root 下所有子目录，读取 .naozhi/project.yaml
func (m *Manager) Scan() error

// Get 获取项目（不存在返回 nil）
func (m *Manager) Get(name string) *Project

// All 返回所有项目（按目录名排序）
func (m *Manager) All() []*Project

// ForWorkspace 通过 workspace 路径反查项目（用于 session 归属）
// 使用前缀匹配：/workspace/naozhi/src/components 归属于 "naozhi" 项目
func (m *Manager) ForWorkspace(path string) *Project

// EnsurePlanner 为项目预热 planner session（可选，用于服务启动时）
// IM 消息路由时直接调 router.GetOrCreate 即可，GetOrCreate 会自动恢复 dead planner
func (m *Manager) EnsurePlanner(p *Project) (*session.ManagedSession, error)

// SaveConfig 写入 project.yaml
func (m *Manager) SaveConfig(name string, cfg ProjectConfig) error

// ProjectForChat 查找 chat 绑定的 project（用于 IM 路由）
func (m *Manager) ProjectForChat(platform, chatType, chatID string) *Project
```

**与 Router 的关系**：
- ProjectManager 持有 Router 引用，调用 `router.GetOrCreate` 创建/恢复 planner session
- Planner session 在 Router 的 `sessions` map 中正常存储，`project:` 是保留命名空间前缀，不与现有 `platform:chatType:chatID:agentID` 格式冲突
- `exempt=true` 使 planner 在以下三处被跳过：`Cleanup` 的 TTL 裁剪、`Cleanup` 的 7×TTL 死亡裁剪、`evictOldest` 的候选选择
- `exempt` session 不计入 `activeCount`，普通 session 的 `max_procs` 配额不受影响

---

### Dashboard 变更

#### 侧边栏（左栏）

```
+-----------------------------+
|  Projects                   |
|                             |
|  ▼ naozhi            [⚙]  |
|    ● planner                |   <- 常驻，特殊图标，不同颜色
|    ○ feishu:code-review    |
|    ○ slack:general         |
|                             |
|  ▶ daydream          [⚙]  |   <- 折叠
|                             |
|  ▶ research          [⚙]  |   <- 折叠
|                             |
|  ── 未归属 Sessions ──       |
|    ○ discord:general       |
+-----------------------------+
```

#### Session 归属计算（后端）

`/api/sessions` 响应新增 `project` 字段：

```json
{
  "key": "feishu:1:C123:general",
  "project": "naozhi",
  "workspace": "/home/ec2-user/workspace/naozhi",
  ...
}
```

后端通过 `projectManager.ForWorkspace(session.workspace)` 填充，planner session 直接从 key 解析。

#### 项目设置面板（`[⚙]` 按钮）

```
Project: naozhi
Path:    /home/ec2-user/workspace/naozhi

[Planner]
Status:  ● running   (project:naozhi:planner)
Model:   [claude-sonnet-4-6      ▾]
Prompt:  [edit...]

[IM Bindings]
  feishu:direct:alice    [解绑]
  slack:channel:C123     [解绑]
  [+ 添加绑定]  <- 弹出表单：选择 platform + 输入 chat ID

[Git Sync]
☐ Enabled
Remote: ___________________________

[Memory]
File:   .naozhi/MEMORY.md   (14.2 KB)
```

注："+ 添加绑定"通过表单手动输入 platform 和 chat ID，或从当前在线 session 列表中选择（dashboard 可见所有 session 的 chatID）。

#### 新增 API Endpoints

| Endpoint | Method | 说明 |
|----------|--------|------|
| `/api/projects` | GET | 列出所有项目及 planner 状态 |
| `/api/projects/{name}/config` | GET/PUT | 读写 project.yaml |
| `/api/projects/{name}/planner/restart` | POST | 重启 planner |

---

#### Session 侧边栏生命周期

**原则**：侧边栏显式、可控。`Remove` = 用户想"忘掉"这个 session，必须是不可逆的；没跑过的 session 不应自己冒出来。

**数据源**（两个彼此隔离）：

1. **侧边栏** (`/api/sessions` 的 `sessions` 字段) — 只读自 `Router.sessions`，其持久化源是 `sessions.json`。
2. **历史面板** (`/api/sessions` 的 `history_sessions` 字段) — 实时扫 `~/.claude` 目录，与侧边栏互不干扰。

**进入侧边栏的三条路径**（全部由显式行为触发）：

- IM 消息首次命中 → `Router.GetOrCreate` 写入 `sessions.json`
- Dashboard 直接发送 → 同上
- 用户在历史面板点击 resume → `POST /api/sessions/resume` → `RegisterForResume` 注册到 `Router.sessions`

**离开侧边栏**：

- `DELETE /api/sessions` → `Router.Remove`：杀进程、从 map 删除、下次落盘不含此 key。**重启后不回填**。
- 若用户事后想找回来，仍可从历史面板点 resume（该 session 的 JSONL 仍在磁盘上）。

**明确不做的事**（已拒绝）：

- 启动时扫 `~/.claude` 最近 N 天 session 自动注入侧边栏。旧实现（`session/router.go` backfill 块）已删除。理由：`Remove` 之后重启又自动回来，等于"删不掉"。

---

### 配置格式变更

```yaml
# config.yaml 新增/变更字段

# Workspace 身份（本机 naozhi 实例）
workspace:
  id: "ec2-dev"                # 唯一标识（默认 hostname）
  name: "EC2 Dev"              # 展示名（默认同 id）

session:
  cwd: "/home/ec2-user/workspace"  # 原 workspace 字段，改名为 cwd（向后兼容：workspace 仍可用）

# 远程 workspace（原 nodes，workspaces 为推荐名，nodes 仍兼容）
workspaces:
  macbook:
    url: "http://192.168.1.101:8180"
    name: "MacBook Pro"

projects:
  root: "~/workspace"      # projects_root，扫描此目录下所有子目录

  # 可选：全局 planner 默认配置（可被 project.yaml 覆盖）
  planner_defaults:
    model: "claude-sonnet-4-6"
    # prompt: 待设计
```

**术语链**：`workspace` (实例) → `project` (目录) → `session` (会话)

**向后兼容**：
- `session.workspace` 仍可用，加载时自动映射到 `session.cwd`
- `nodes` 仍可用，加载时自动映射到 `workspaces`
- 不配置 `workspace.id` 时默认取 hostname

---

### 实现步骤 (Phase 7)

Phase 7 分为四个子阶段，逐步从单机 project 组织演进到跨 workspace 管理。

#### Phase 7.0: ProjectManager + Planner + IM 路由 [已完成]

- `internal/project/` 包：Scan/Get/All/ProjectForChat/ResolveWorkspaces/BindChat/UnbindChat
- ManagedSession.Exempt + Router 三处跳过 (countActive/evictOldest/Cleanup)
- `/project` 命令、planner 路由、`/cd` 互斥、`/new` 区分 planner/agent
- Dashboard 侧边栏按 project 分组，planner 置顶紫色标识
- `/api/projects`、`/api/projects/config`、`/api/projects/planner/restart` 端点

#### Phase 7.1: Discovered Sessions 合并进 Folder [待实现]

**目标**：消除 "Discovered Processes" 独立区域，所有 session（managed + discovered）按 folder 统一聚合。

**后端**：
- `DiscoveredSession` 加 `Project string` 字段
- `handleAPIDiscovered` 用 `ResolveWorkspaces` 批量解析 `cwd → project`

**前端**：
- 删除 `discovered-section` 独立区域
- `fetchSessions` + `scanDiscovered` 结果合并为统一 item 列表：
  ```
  type: "managed" | "discovered"
  project: string  // 从 project 字段或 cwd 解析
  ```
- `renderSidebar` 合并渲染：每个 folder group 内 planner 最前，managed 按 state 排，discovered 排最后
- discovered 卡片用 `◇` 图标 + "external" badge + 内联 takeover 按钮
- 点击 discovered 卡片保持现有 `previewDiscovered` 行为
- 未匹配任何 project 的 discovered session 归入 "未归属" 分组

**不改**：`/api/sessions` 和 `/api/discovered` 保持两个独立端点。

#### Phase 7.2: Node 身份 + 配置增强 [待实现]

**目标**：每个 naozhi 实例有明确身份，为跨实例聚合做基础。

**配置变更**：

```yaml
# config.yaml 新增（nodes 保持不变，向后兼容）
node:
  id: "ec2-prod"           # 本机唯一标识（默认 hostname）
  display_name: "EC2 Prod"  # 展示名（默认同 id）

# 远程实例仍用 nodes（不改名，不破坏现有配置）
nodes:
  macbook:
    url: "http://192.168.1.101:8180"
    display_name: "MacBook Pro"
```

`node.id` 是寻址基础。不配置时默认为 `os.Hostname()`。

**API 变更**：
- `GET /health` 响应加 `node_id` 和 `display_name`
- `GET /api/sessions` 的 `stats` 加 `node_id`

**前端**：
- 单 workspace（无 remote nodes）：直接显示 project groups（现有行为不变）
- 多 workspace：最外层按 node 分组，每个 node 下再按 project 分组
  ```
  🖥 EC2 Prod [●]
    ▼ naozhi/
      ● planner
      ○ feishu:general
      ◇ PID 12345
    ▶ daydream/

  💻 MacBook Pro [●]
    ▼ brain/
      ● planner
    ▶ webapp/
  ```
- 单 workspace 时自动折叠 node 层（不增加视觉层级）

#### Phase 7.3: 远程 Projects + Discovered 聚合 [待实现]

**目标**：聚合节点能看到远程实例的 projects 和 discovered sessions。

**NodeClient 扩展**：

```go
// 新增方法
func (n *NodeClient) FetchProjects(ctx) ([]ProjectInfo, error)
func (n *NodeClient) FetchDiscovered(ctx) ([]DiscoveredSession, error)
```

**nodeCache 扩展**：

```go
type nodeCache struct {
    sessions   []SessionSnapshot      // 已有
    projects   []ProjectInfo          // 新增
    discovered []DiscoveredSession    // 新增
    status     string
    fetchedAt  time.Time
}
```

缓存刷新间隔：sessions + projects 每 10s（已有频率），discovered 每 30s（更低频，因为是扫描 /proc）。

**Dashboard**：
- 远程 node 的 sessions、projects、discovered 全部合并进统一 sidebar tree
- 远程 discovered 卡片的 takeover 按钮通过 proxy 转发

#### Phase 7.4: 跨 Workspace 操作 Proxy [待实现]

**目标**：从聚合节点可以对远程实例执行完整操作。

**操作矩阵**：

| 操作 | 本机 | 远程（proxy 转发） |
|------|------|-------------------|
| 查看 sessions/events | ✅ 已有 | ✅ 已有 |
| 发送消息 | ✅ 已有 | ✅ 已有 |
| takeover discovered | ✅ 已有 | 新增 proxy |
| planner restart | ✅ 已有 | 新增 proxy |
| project config CRUD | ✅ 已有 | 新增 proxy |
| /project bind (IM) | ✅ | ❌ 只影响本机路由 |

**NodeClient 新增**：

```go
func (n *NodeClient) Takeover(ctx, pid, sessionID, cwd string) error
func (n *NodeClient) RestartPlanner(ctx, projectName string) error
func (n *NodeClient) UpdateProjectConfig(ctx, name string, cfg ProjectConfig) error
```

**不引入全局 session key**：跨 workspace 路由不改变 session key 格式。前端通过 `node` 维度区分本机和远程，API 调用时带 `node` 参数由 server 决定本地处理还是 proxy 转发（现有模式）。

---

### 补充设计细节

#### `/cd` 与 `/project` 的交互

当 chat 已绑定 project 时，planner 的 workspace 固定为项目路径，不允许通过 `/cd` 修改：

```
用户在已绑定 naozhi 的 chat 中发 /cd /tmp
  -> 回复: "当前已绑定项目 naozhi，工作目录固定为项目路径。如需切换，请先 /project off 解绑。"
```

`/cd` 只在未绑定 project 的 chat 中可用，保持现有行为不变。

#### `exempt` 标记的持久化与恢复

`storeEntry` 不新增字段。重启加载时，通过 session key 的 `project:` 前缀推导 `exempt=true`：

```go
// loadStore 后恢复 exempt 标记
if strings.HasPrefix(entry.Key, "project:") {
    session.exempt = true
}
```

#### `countActive` 跳过 exempt session

Router 的 `countActive()` 遍历 sessions 计算存活进程数，需排除 exempt session：

```go
func (r *Router) countActive() {
    count := 0
    for _, s := range r.sessions {
        if s.exempt {
            continue  // planner 不占名额
        }
        if s.process != nil && s.process.Alive() {
            count++
        }
    }
    r.activeCount = count
}
```

#### Graceful Shutdown 中的 Planner 处理

Planner 在 shutdown 时与普通 session 一致：等待 running 完成 → 保存 store → 关闭 stdin。`exempt` 只影响日常 TTL/eviction，不影响 shutdown 流程。

---

### 功能验收矩阵

以下从用户视角描述 Phase 7 实现后可验收的全部功能。每条可直接作为集成测试 case。

#### 一、Project 自动发现

| # | 场景 | 前置条件 | 操作 | 预期结果 |
|---|------|---------|------|---------|
| 1.1 | 启动时扫描 projects_root | `~/workspace/` 下有 naozhi, daydream, research 三个目录 | 启动 naozhi 服务 | `GET /api/projects` 返回 3 个 project，name 分别为 naozhi/daydream/research |
| 1.2 | 跳过非目录文件 | `~/workspace/` 下有 README.md 文件 | 启动 | `/api/projects` 不含 README.md |
| 1.3 | 跳过隐藏目录 | `~/workspace/.git` 存在 | 启动 | `/api/projects` 不含 .git |
| 1.4 | 无 project.yaml 的目录也被发现 | `~/workspace/new-project/` 存在但无 `.naozhi/project.yaml` | 启动 | `/api/projects` 包含 new-project，config 为默认值 |
| 1.5 | 有 project.yaml 的目录读取配置 | `~/workspace/naozhi/.naozhi/project.yaml` 配置了 `planner_model: opus` | 启动 | `GET /api/projects` 中 naozhi 的 planner_model 为 opus |
| 1.6 | projects_root 不存在 | config 中 `projects.root` 指向不存在的目录 | 启动 | 启动失败，错误信息包含 "projects root not found" |

#### 二、Planner 生命周期

| # | 场景 | 前置条件 | 操作 | 预期结果 |
|---|------|---------|------|---------|
| 2.1 | 首次消息触发 planner 创建 | 项目 naozhi 存在，无 planner session | IM 发消息到已绑定 naozhi 的 chat | Router 中出现 key=`project:naozhi:planner` 的 session，状态为 ready/running |
| 2.2 | Planner 不受 TTL 影响 | Planner session 空闲超过 TTL (30min) | 等待 Cleanup 执行 | Planner 仍然 alive，不被关闭 |
| 2.3 | Planner 不受 7×TTL 死亡裁剪 | Planner 进程死亡，空闲超过 7×TTL | 等待 Cleanup 执行 | Planner session 仍在 sessions map 中，未被删除 |
| 2.4 | Planner 不被 evict | `max_procs=2`，2 个普通 session 存活，新消息到达 | Router 尝试 evictOldest | 只从普通 session 中选最旧的驱逐，planner 不被选中 |
| 2.5 | Planner 不计入 activeCount | `max_procs=3`，1 个 planner + 3 个普通 session | 检查 activeCount | activeCount=3（只计普通 session），不触发 maxProcs 限制 |
| 2.6 | Planner 死亡后自动恢复 | Planner 进程意外退出 (state=dead) | IM 再发消息 | `GetOrCreate` 自动 `--resume` 恢复，对话上下文保留 |
| 2.7 | Planner 的 workspace 固定 | Planner 创建时 workspace 为项目路径 | 查看 planner process 的 CWD | 为 `/home/.../workspace/naozhi`，不受 `/cd` 影响 |
| 2.8 | Planner 使用自定义 model | `project.yaml` 中 `planner_model: opus` | Planner 被创建 | CLI 启动参数包含 `--model opus` |
| 2.9 | Planner 注入 system prompt | `project.yaml` 中 `planner_prompt: "你是 naozhi 的规划者"` | Planner 被创建 | CLI 启动参数包含 `--append-system-prompt "你是 naozhi 的规划者"` |
| 2.10 | 服务重启后 planner 恢复 | Planner 有 session_id，服务重启 | 加载 store，IM 发消息 | Planner 通过 `--resume` 恢复，上下文保留；`exempt` 从 `project:` 前缀推导 |

#### 三、IM-to-Project 路由

| # | 场景 | 前置条件 | 操作 | 预期结果 |
|---|------|---------|------|---------|
| 3.1 | `/project` 命令绑定 | 项目 naozhi 存在 | 飞书发 `/project naozhi` | 回复确认绑定；后续普通消息路由到 `project:naozhi:planner` |
| 3.2 | `/project` 查看当前绑定 | Chat 已绑定 naozhi | 飞书发 `/project` | 回复 "当前绑定: naozhi (/home/.../workspace/naozhi)" |
| 3.3 | `/project off` 解绑 | Chat 已绑定 naozhi | 飞书发 `/project off` | 回复确认解绑；后续消息恢复原有路由 (general session) |
| 3.4 | `/project` 绑定不存在的项目 | 项目 xxx 不存在 | 飞书发 `/project xxx` | 回复 "项目不存在: xxx" |
| 3.5 | 绑定后普通消息走 planner | Chat 已绑定 naozhi | 飞书发 "帮我看看 git log" | 消息发送到 `project:naozhi:planner` session |
| 3.6 | 绑定后 agent 命令仍生效 | Chat 已绑定 naozhi，`/review` 映射到 code-reviewer | 飞书发 `/review main.go` | 消息发送到 `feishu:direct:alice:code-reviewer`，workspace 为 naozhi 项目路径 |
| 3.7 | 未绑定时走原有路由 | Chat 未绑定任何 project | 飞书发普通消息 | 消息发送到 `feishu:direct:alice:general`（现有行为） |
| 3.8 | 配置绑定（静态） | `project.yaml` 中有 `chat_bindings: [{platform: feishu, chat_id: C123}]` | 启动后飞书 C123 发消息 | 消息路由到对应项目的 planner |
| 3.9 | 绑定持久化 | 通过 `/project naozhi` 绑定 | 重启服务 | 绑定仍然有效（从 `project.yaml` 的 `chat_bindings` 加载） |
| 3.10 | `/cd` 在绑定状态下被禁止 | Chat 已绑定 naozhi | 飞书发 `/cd /tmp` | 回复 "当前已绑定项目 naozhi，工作目录固定为项目路径。如需切换，请先 /project off 解绑。" |
| 3.11 | 多 chat 共享同一 planner | 飞书 Alice 和 Slack Bob 都绑定 naozhi | Alice 发消息，Bob 发消息 | 两条消息都路由到同一个 `project:naozhi:planner` session，共享上下文 |

#### 四、`/new` 在 Project 模式下的行为

| # | 场景 | 前置条件 | 操作 | 预期结果 |
|---|------|---------|------|---------|
| 4.1 | `/new` 重置 planner | Chat 绑定 naozhi，planner 有上下文 | 飞书发 `/new` | Planner session 被 Reset，下次消息创建全新进程，上下文清空 |
| 4.2 | `/new` 影响所有共享方 | Alice 和 Bob 都绑定 naozhi | Alice 发 `/new` | Planner 重置，Bob 下次发消息也是全新上下文 |
| 4.3 | `/new review` 只重置 agent | Chat 绑定 naozhi，有 code-reviewer session | 飞书发 `/new review` | 只重置 `feishu:direct:alice:code-reviewer`，planner 不受影响 |

#### 五、Dashboard 展示

| # | 场景 | 前置条件 | 操作 | 预期结果 |
|---|------|---------|------|---------|
| 5.1 | Session 按 project 分组 | 有 naozhi project + 2 个归属 session + 1 个未归属 session | 打开 dashboard | 侧边栏显示 "naozhi" 折叠组（含 planner + 2 session）+ "未归属 Sessions" 组（含 1 session） |
| 5.2 | Planner 置顶显示 | naozhi 下有 planner + code-reviewer + general | 查看 naozhi 组 | Planner 在最上面，有特殊图标/颜色标识 |
| 5.3 | Session 通过 workspace 前缀归属 | Session workspace 为 `/workspace/naozhi/src` | 查看 dashboard | 该 session 显示在 naozhi project 下 |
| 5.4 | 无 project 的 session 归到 "未归属" | Session workspace 为 `/tmp/test` | 查看 dashboard | 该 session 显示在 "未归属 Sessions" 分组 |
| 5.5 | 项目设置面板 | 点击 naozhi 的 `[⚙]` | 面板打开 | 显示 planner 状态、model、IM bindings 列表、Git Sync 开关、Memory 文件信息 |
| 5.6 | `/api/sessions` 含 project 字段 | 存在归属 naozhi 的 session | `GET /api/sessions` | 每个 session 有 `"project": "naozhi"` 或 `"project": ""`（未归属） |
| 5.7 | `/api/projects` 接口 | 有 3 个项目 | `GET /api/projects` | 返回数组，每项含 name/path/planner_status/config 字段 |

#### 六、项目配置管理

| # | 场景 | 前置条件 | 操作 | 预期结果 |
|---|------|---------|------|---------|
| 6.1 | 读取项目配置 | naozhi 有 project.yaml | `GET /api/projects/naozhi/config` | 返回 ProjectConfig JSON |
| 6.2 | 更新项目配置 | naozhi 存在 | `PUT /api/projects/naozhi/config` 带 `{"planner_model":"opus"}` | `.naozhi/project.yaml` 被更新，返回 200 |
| 6.3 | 添加 IM 绑定 | naozhi 存在 | `PUT /api/projects/naozhi/config` 添加 chat_binding | `project.yaml` 写入新绑定，后续该 chat 消息走 planner |
| 6.4 | 删除 IM 绑定 | naozhi 有一个绑定 | `PUT /api/projects/naozhi/config` 移除 binding | 绑定生效解除，该 chat 消息恢复默认路由 |
| 6.5 | 重启 planner | planner 在运行 | `POST /api/projects/naozhi/planner/restart` | 当前 planner 进程被 kill，新进程用最新配置启动 |
| 6.6 | 不存在的项目 | 无 xxx 项目 | `GET /api/projects/xxx/config` | 返回 404 |

#### 七、与现有功能兼容性

| # | 场景 | 预期结果 |
|---|------|---------|
| 7.1 | 未配置 `projects.root` | 服务正常启动，Phase 7 功能全部禁用，现有行为不受影响 |
| 7.2 | 未绑定 project 的 chat | 消息路由、`/cd`、`/new`、agent 命令全部保持现有行为 |
| 7.3 | Graceful shutdown 含 planner | Planner running 时收到 SIGTERM：等待完成 → 保存 store（含 planner session_id）→ 关闭 |
| 7.4 | 多节点聚合 | 远程 node 的 session 如有 workspace 在本机 projects_root 下，可归属到 project（尽力归属，不强制） |
| 7.5 | Cron 任务在 project 下执行 | Cron job 配置了 workspace 在项目路径下，其 session 归属到对应 project |
| 7.6 | Session 持久化含 planner | `sessions.json` 中含 `project:naozhi:planner` 条目，重启后正确恢复 exempt 标记 |

---

## 扩展设计文档

以下功能已实现，完整设计文档独立保存：

| 功能 | 设计文档 | 状态 |
|------|---------|------|
| 多节点聚合 (Direct + Reverse) | [multi-node-design.md](multi-node-design.md) | 已实现 |
| Shim 进程 (零中断热重启) | [shim-design.md](shim-design.md) | 已实现 |
| server 包拆分 | [server-split-design.md](server-split-design.md) | Phase 1-2 已完成 |
| 语音消息转写 | [voice-transcription.md](voice-transcription.md) | 已实现 |
| 部署策略 | [../ops/deployment-strategy.md](../ops/deployment-strategy.md) | 部分实现 |

未实现的设计提案：

| 功能 | 设计文档 | 状态 |
|------|---------|------|
| 消息队列策略 | [rfc/message-queue.md](rfc/message-queue.md) | RFC，待实现 |
| 自学习系统 | [rfc/learning-system.md](rfc/learning-system.md) | RFC，待实现 |
| 7.7 | `/project` 解绑后 `/cd` 恢复 | 先 `/project off` 再 `/cd /tmp`：正常工作，无报错 |
