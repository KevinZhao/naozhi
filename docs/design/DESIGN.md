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
> 6. Claude backend 可设 `cli.permission_mode: auto`，改传 `--permission-mode auto`：由 CLI 的分类器模型逐个审查工具调用，而不是全部放行（见 `config.example.yaml`）

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
  | WebSocket 长连接 (naozhi 出站连接, 无需公网 IP)
  v
feishu adapter (lark-ws SDK)
  |
  +-> [OnP2MessageReceiveV1 回调]
      |
      +-> parseSDKEvent(event)         // -> IncomingMessage{EventID, UserID, ChatID, Text}
      |
      +-> go handler(ctx, msg)         // feishu 内部启 goroutine, 以下异步
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
- 事件回调必须尽快返回，否则平台会重投。所以收到消息后立即返回，实际处理异步进行
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

状态说明（`cli.ProcessState` 进程级 4 态枚举，权威定义见 `internal/cli/process.go::ProcessState`）：
- **Spawning**: 进程启动中，等待 init 事件。超时 10s 未收到 init 则 kill
- **Ready**: 进程空闲，可接受新消息
- **Running**: 正在处理消息，等待 result。启用 Watchdog: 无输出超时 15min（配置默认值）+ 总超时 2h（配置默认值）。任何一个超时触发即杀死进程
- **Dead**: 进程已退出。有 session_id 的保留供 resume；无 session_id 的在 Cleanup 时清除

> 上述是 `cli.Process` 的进程级状态机；`session.ManagedSession` 上层另有若干**语义标签**（不是状态枚举），用来表达跨进程生命周期的 session 角色：
> - `Exempt`：planner / cron stub 类 session，不计入 maxProcs 配额（见 `AgentOpts.Exempt`）
> - `Stub`：cron 注册但尚未首次执行的占位 session（见 `RegisterCronStub`）
> - `Scratch`：dashboard 抽屉临时 session，由 key 前缀 `scratch:` 标识，永远不入 sidebar
> - `Paused / Suspended`：session 行为通过 `process == nil` + 可 resume 的 `SessionID` 表达（dead-resumable）
>
> 这些标签可正交组合（如 exempt + stub），底层进程仍走上面的 4 态机。

### 三层状态机对比（R230B-ARCH-27）

naozhi 同时管理三个层次的"状态"，常被混淆，这里列出权威拆分：

| 层 | 类型/字段 | 状态枚举 | 转换触发 | 文件位置 |
|----|----------|----------|----------|----------|
| **Process** | `cli.ProcessState` | Spawning / Ready / Running / Dead | init 事件 / stdin 写入 / result / kill / 超时 | `internal/cli/process.go::ProcessState` |
| **ManagedSession** | 多个语义标签（非枚举） | Exempt / Stub / Scratch / Paused | 由 `process==nil` + `key` 前缀 + `AgentOpts.Exempt` 推导 | `internal/session/managed.go` |
| **Chat** | 隐式（无单独类型） | Active / Idle / Removed | 用户/IM 入口产生消息 / TTL / `/new` 命令 | 由 `Router.sessions` map 存在性 + `lastActive` 表达 |

关键不变量：
- **Process** 是底层物理进程的状态机；ManagedSession 持 `process atomic.Pointer[processBox]` 引用它
- **ManagedSession** 是 router 一级的 wrapper；它的"语义标签"可正交组合（Exempt + Stub 同时存在）
- **Chat** 没有单独的 type — 它是 ManagedSession 的聚合视图（同一 chat 可能有多个 ManagedSession，每 agent 一个）

下游修改注意：改 ProcessState 枚举要同步 `process_test.go` + Watchdog；改语义标签需考虑 dashboard 渲染 + saveStore 持久化；改 chat 隐式状态机需考虑 Reset 调用面 + sessions.json schema。R230B-ARCH-27 跟踪建立独立状态图为后续 RFC 工作。

Watchdog 机制：
- `no_output_timeout`（默认 15min）：若连续无输出事件，杀死进程。工具运行期间 Claude CLI 每 30s 发一帧 `tool_progress` heartbeat，所以长工具不会触发它；它拦的是模型长时间静默（超长 thinking、超大 Write 输入）或 CLI 卡死
- `total_timeout`（默认 2h）：本轮总耗时超限，杀死进程
- 两个默认值只在 `internal/cliinfo/watchdog.go` 定义一次，`cli` 与 `config` 引用它
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
    Name() string                                                  // "stream-json" | "acp" | "codex-app-server"
    Clone() Protocol                                               // 新进程的副本（stateless 可返回 receiver）
    BuildArgs(opts SpawnOptions) []string                          // 构建 CLI 启动参数
    Init(rw *JSONRW, resumeID string, cwd string) (string, error)  // 协议握手 (ACP: initialize + session/new)
    WriteMessage(w io.Writer, text string, images []Attachment) error // 写入用户消息（含可选附件）
    WriteUserMessageLocked(w io.Writer, uuid, text string, images []Attachment, priority string) error
                                                                   // passthrough 感知写入（调用方须持 stdin 写锁）
    SupportsPriority() bool                                        // 是否透传顶层 priority 字段（/urgent 依赖）
    SupportsReplay() bool                                          // 后端是否回显 stdin user 消息（passthrough slot 匹配依赖）
    WriteInterrupt(w io.Writer, requestID string) error            // 中断当前轮次（ACP 返回 ErrInterruptUnsupported）
    ReadEvent(line string) ([]Event, bool, error)                  // 解析 NDJSON 行 → 0..n 个事件；bool 仅 advisory，
                                                                   // 轮次结束以 result/turn-end Event 为准
    HandleEvent(w io.Writer, ev Event) bool                        // 处理内部事件 (如 ACP 权限自动授权)
}
```

接口是两个 facet 的严格划分（编译期 pin）：`ProtocolCore`
（Name/Clone/BuildArgs/Init/WriteMessage/ReadEvent/HandleEvent，所有后端
均有实义实现）+ `ProtocolPassthroughExt`（WriteUserMessageLocked/
WriteInterrupt/SupportsPriority/SupportsReplay，stream-json 专属能力面，
ACP 降级实现）。热路径分配优化走可选的 `eventReaderInto` facet
（`ReadEventInto(line, buf)`，type assertion 探测），不改 `ReadEvent` 本身。
本签名与 CLAUDE.md、`internal/cli/protocol.go` 三处 lockstep 同步。

**Capabilities 聚合** (Round 209 / RNEW-ARCH-404)：

`cli.Caps` 是 Protocol 能力位的聚合结构（`Replay` / `Priority` /
`SoftInterrupt` / `StreamJSON` 等），`cli.ProtocolCaps(p)` 是统一入口：
若实现提供 `Capabilities() Caps` 方法则优先采用，否则回退到原有
`SupportsReplay()` / `SupportsPriority()` + `Name()` 派生默认值。现有
`SupportsReplay` / `SupportsPriority` 将逐步迁移到 `Caps` 字段；新增
Protocol 实现应直接提供 `Capabilities() Caps`，调用方也应优先读
`ProtocolCaps(p).X` 而非散落的 `p.SupportsX()`。这样后续新增能力位
（例如 `StreamJSON`、`SoftInterrupt`）不再需要同步扩展 Protocol 接口。

**协议实现对比**（第三个实现 `CodexProtocol`（codex app-server JSON-RPC 2.0）未列入下表，见 `docs/rfc/codex-backend.md`）:

| 维度 | ClaudeProtocol (stream-json) | ACPProtocol (JSON-RPC 2.0) |
|------|------------------------------|---------------------------|
| 后端 | claude CLI | kiro CLI |
| 输入 | `{"type":"user","message":...}` | `session/prompt` RPC |
| 输出 | NDJSON `type=result` | `session/update` 通知 + RPC response |
| 握手 | 无 (session ID 从 init 事件获取) | `initialize` + `session/new` or `session/load` |
| 恢复 | `--resume {sessionId}` | `session/load` RPC |
| 权限 | `--dangerously-skip-permissions` | auto-grant `session/request_permission`（optionId 从 request `options[].optionId` 读，**不要硬编码**） |
| 文本 | result 事件直接携带 | 累积 `agent_message_chunk` 文本（实测 ~2.2 字符/chunk，buffer 至 turn 完成才 emit） |
| 软中断 | `control_request` over stdin | `session/cancel` **notification**（无 id），原 prompt 立即回 `stopReason:"cancelled"` |
| RPC ID 类型 | n/a | **必须** `json.RawMessage`（kiro 用 UUID 字符串，不是 int） |
| 历史持久化 | `~/.claude/projects/**/*.jsonl` | `~/.kiro/sessions/cli/<sid>.{json,jsonl}`（kiro 自带，按消息聚合） |

> **多 backend 共存与切换**：见 [`docs/rfc/multi-backend.md`](../rfc/multi-backend.md)（实测报告 `multi-backend-validation.md` 已删除，需要时用 git log 找回）。
> 后端的 DisplayName / DefaultBinary / Protocol 工厂 / DetectInProc / RequiredNodeCaps 收敛到
> `internal/cli/backend.Profile` 注册表，其他模块禁止 switch backend ID。

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

#### Backend Extension Points

新增一个 backend（例如 Gemini CLI）只需要在四处显式注册，其它消费方都通过
`backend.Profile` 注册表读取，禁止 `switch backend.ID`：

1. **`backend.Profile`** (`internal/cli/backend/profile.go`)
   单一注册表，承载 backend 的所有元数据与构造钩子：

   - `ID` / `DisplayName` / `DefaultBinary` / `DefaultTag` / `ChipColor`
   - `NewProtocol(deps) cli.Protocol` —— 每次 spawn 构造新协议实例
   - `DetectInProc(cmdline)` —— 进程发现时按命令行匹配归类
   - `RequiredNodeCaps` —— 反向 node 路由的能力门（如 `["acp"]`）
   - `HistoryDir` —— 转写文件根目录（`~/...` 形式）
   - `CostUnit` —— dashboard cost cell 单位（`USD` / `credits` / 空）
   - `Features` —— UI 能力位（`askuser` / `passthrough` / `embedded_context` /
     `image_input` / `audio_input` / `mcp_http` / `mcp_sse`）

   注册流程：在 `internal/cli/backend/profile_<id>.go` 写一个返回 `Profile`
   的工厂，`RegisterDefaults` 调用 `Register(<id>Profile())`。

2. **`cli.Protocol`** (`internal/cli/protocol.go`)
   `Profile.NewProtocol` 返回的具体协议实现，负责 NDJSON / JSON-RPC 等
   线协议。每个新 backend 通常对应一个 `protocol_<id>.go`，声明
   `Capabilities() cli.Caps` 显式标注 `Replay` / `Priority` /
   `SoftInterrupt` / `StreamJSON` 等能力位（不要再依赖 `Name()` 反推默认）。
   遇到不支持中断的协议返回 `cli.ErrInterruptUnsupported`。

3. **`history.Source`** (`internal/history/`)
   Backend 私有的转写读取器。在 `internal/history/<id>jsonl/` 实现：

   - `Source.LoadHistory(ctx, dir, sid)` 把磁盘转写翻成 `cli.EventEntry` 流
   - `Source.ListSessions(ctx, dir)` 让 dashboard 历史面板枚举可恢复 session
   - 在子包 `init()` 里调 `cli.RegisterHistoryFactory(<id>, factory)`
     —— `cli.Wrapper.NewWrapper` 在 spawn 时按 BackendID 取工厂；未注册的
     backend fallback 到 `NoopHistorySource`（dashboard 显示空磁盘层）。

4. **shim hint**（可选，仅在 backend 需要 reverse-node 路由时）
   `Profile.RequiredNodeCaps` 在 `internal/server/select_node_for_backend.go`
   中作为 capability gate：候选 node 的 `cli.backend.<id>.capabilities`
   配置必须覆盖该列表才能承接此 backend 的 session。`StartShimWithBackend`
   把 backend ID 透传到 shim binary，shim 据此选 CLI 路径与启动 argv。

> 历史与展望：System Session daemon (`internal/sysession`) 与 Cron Dashboard
> 都在 backend.Profile 之外（它们消费 session，不增加 backend）；
> Gemini CLI 集成在 [`docs/rfc/multi-backend.md`](../rfc/multi-backend.md)
> 中作为下一个候选，预期只动上述四点。

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
- **Event log 持久化**（`~/.naozhi/events/<keyhash>.log` + `.idx`，见 `docs/rfc/event-log-persistence.md`）:每条 `cli.EventEntry` 由 per-session 专用 writer goroutine 按 length-prefix framing 落盘，写入经 bufio 缓冲 + 100ms flush tick 触发的 batched fsync 控制 IO 频率，保留 Images / ImagePaths / AskQuestion 等 Claude JSONL 无法还原的字段。切 session / 刷新 dashboard / 重启服务后,图片等富字段仍可见
- 关闭前等待 running 完成（超时 30s），然后保存 store

#### 保留 session-key 命名空间

除标准 IM shape 之外，以下前缀属于内部子系统的保留命名空间，不遵循
`{platform}:{chatType}:{userId}:{agentId}` 约定。新增保留前缀需同步更新
`internal/session/key.go` (`reservedKeyPrefixes`) 以及本表，并考虑是否要接入
`exemptKeyPrefixes`（TTL 豁免）、`saveStore` 过滤、Dashboard sidebar 过滤。

| 前缀 | 常量 | 所有者 | key 形状 | TTL 豁免 | 持久化 | Sidebar 可见 |
|---|---|---|---|---|---|---|
| `cron:` | `session.CronKeyPrefix` | `internal/cron` | `cron:{jobID}` | ✓ | ✓ | ✓（cron 面板） |
| `project:` | `session.ProjectKeyPrefix` | `internal/project` | `project:{name}:planner` | ✓ | ✓ | ✓（planner 行） |
| `scratch:` | `sessionkey.ScratchKeyPrefix` | `internal/session/scratch.go` | `scratch:{id}:general:{sourceAgentID}` | ✗ | ✗（ephemeral） | ✗（抽屉独立 UI） |

判定规则（`internal/session/key.go`）：
- `IsReservedNamespace(key)` — 任一保留前缀命中
- `sessionkey.IsCronKey(key)` — cron 专有路径
- `sessionkey.IsScratchKey(key)` — scratch 专有路径
- `project.IsPlannerKey(key)` — 要求 `project:` 前缀且 `:planner` 后缀（更严格）

所有前缀都带 trailing colon，避免 `cronographer:` / `projectile:` 这类子串误判。

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
| 飞书 | WebSocket 长连接 | `RunnablePlatform` | app_id + app_secret | 4000 字符 | **已实现** |
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

### 实际架构（截至 2026-05，R229-ARCH-21）

上面是 v1 设计稿的预估，实际实现已显著扩大；本节给出当前包结构与主要依赖关系，
便于新协作者建立心智图。详细的扩展点见 §模块设计 → CLI Wrapper Layer → Backend
Extension Points。

```text
cmd/naozhi/main.go                  入口装配：config → server → router → platforms → cron

internal/
├── cli/                            CLI 进程封装 + 协议层（最大子树，65+ .go）
│   ├── wrapper.go                  CLIWrapper.Spawn/Close
│   ├── process*.go                 Process: stdin/stdout/shim/event 子领域
│   ├── protocol_*.go               ClaudeProtocol (stream-json) / ACPProtocol (JSON-RPC)
│   ├── eventlog*.go                EventLog: 内存事件流 + 订阅 + persist sink
│   ├── subagent_link.go            SubagentLinker: tool_use → subagent 解析
│   └── backend/                    Profile registry (DisplayName/DefaultBinary/Caps)
├── session/                        会话路由 + 生命周期（god struct，已拆 6 文件）
│   ├── router_core/lifecycle/...   Router 主体（拆分见 R233B-ARCH-2 待续）
│   ├── managed.go                  ManagedSession: queue/dedup/历史注入
│   └── eventlog_bridge.go          EventLog → PersistSink JSONL 桥接
├── server/                         HTTP server + dashboard handler（90+ .go 单包）
│   ├── server.go                   入口装配 + middleware
│   ├── dashboard_*.go              dashboard handler 子分组
│   ├── ws.go / hub.go              dashboard WebSocket
│   ├── agent_tailer.go             subagent transcript 实时推送
│   ├── project_files.go            项目文件浏览/预览（沙箱）
│   └── health.go                   /health 端点
├── platform/                       Channel Adapter
│   ├── feishu/                     WebSocket 长连接
│   ├── slack/                      Events API + Web API
│   ├── discord/                    discordgo session
│   └── weixin/                     企业微信 webhook
├── dispatch/                       消息出口协调（去重 + replyText + queue）
├── cron/                           Cron Scheduler（CAS gate + watchdog + persist）
├── sysession/                      系统后台会话（sys: 命名空间 + AutoTitler）
├── upstream/                       node 反向协议（多机 wrapper 联动）
├── shim/                           cgroup-isolated CLI 子进程 wrapper（Linux）
├── discovery/                      claude/kiro/naozhi 历史 JSONL 扫描（factory + DefaultScanner）
├── persist/                        EventLog JSONL 留持
├── usermsg/                        错误 → 用户消息中文化映射
├── osutil/                         AtomicWrite / IsLogInjectionRune / cgroup
└── ...                             ratelimit / textutil / netutil / project / node ...
```

包间主要依赖（粗粒度，自上而下不回环；详见各包 godoc）：

```text
cmd/naozhi → server, platform/*, cron, dispatch, sysession, upstream, session
server → session, dispatch, cron, platform, upstream, project
dispatch → session, platform (registry)
cron → session (Reader), platform (Notifier)
sysession → session (sys-namespace stub registration)
upstream → session (Reader), node (reverse protocol)
session → cli (Process / EventLog / Caps), shim (manager hint), history (claude transcript via PickFactory), persist
cli → backend (Profile), persist (PersistSink), shim (protocol shape), osutil
platform → osutil
所有包 → log/slog（结构化日志）
```

实际行数对比预估：

- 预估总计 ~900 行（v1 设计稿）
- 当前 `find internal -name '*.go' | wc -l` 远超预估（cli ~65 / server ~90 / session ~30+），
  因 cron / sysession / upstream / shim / dashboard 等子系统在设计后陆续落地；
- 单文件超 1KLOC 的有 internal/cron/scheduler.go（待 R232-ARCH-1 拆分）+
  internal/cli/process_*.go 几个；其它包多保持在 200-500 行子文件粒度。

新增能力（不在 v1 设计中）：

- Cron Scheduler with persist + watchdog（`internal/cron/`）
- 多 backend 支持（claude / kiro / future gemini）via backend.Profile registry
- 反向 node 协议跨机分发（`internal/upstream/`）
- shim cgroup 隔离 + watchdog kill（`internal/shim/`，Linux only）
- System Session + AutoTitler（`internal/sysession/`）
- Dashboard WebSocket + agent_tailer（实时 subagent transcript）
- ScratchPool / 追问抽屉（短生命会话池）

后续若架构再演进，更新这节是 P3 维护项；脱离 ±20% 时应触发新一轮 sync。

## 配置格式

```yaml
# ~/.naozhi/config.yaml

server:
  addr: ":8180"

cli:
  backend: "claude"              # 默认 backend；"claude" | "kiro" | 未来扩展
  path: "~/.local/bin/claude"    # 默认 CLI 二进制路径（与 backend 字段配套）
  model: "sonnet"                # 默认模型
  args:                          # 额外 CLI 参数
    - "--dangerously-skip-permissions"

  # Multi-backend 并存（见 docs/rfc/multi-backend.md）
  # 配置 backends 数组后，dashboard / IM / cron / reverse-node 都能 per-session
  # 选 backend；不配则走 legacy 单 backend 模式（仅用上面的 cli.path / model / args）
  #backends:
  #  - id: "claude"
  #    path: "~/.local/bin/claude"
  #    model: "sonnet"
  #  - id: "kiro"
  #    path: "~/.local/bin/kiro-cli"
  #    model: "claude-sonnet-4.6"
  #    sessions_dir: "~/.kiro/sessions/cli"   # kiro 自带 jsonl 历史目录

session:
  max_procs: 3                   # 最大并发 claude 进程数 (每进程 ~350MB)
  ttl: "30m"                     # 空闲 session 回收超时 (回收后保留 session_id, resume 恢复)
  watchdog:
    no_output_timeout: "15m"     # 无输出超时 (kill 进程), 默认 15min
    total_timeout: "2h"          # 单轮总超时, 默认 2h
  store_path: "~/.naozhi/sessions.json"  # session 持久化路径

# Workspace 身份
workspace:
  id: "my-node"                  # 节点标识 (默认 hostname)
  name: "My Workspace"           # 显示名

# Project 管理 (可选)
projects:
  root: "/home/ec2-user/workspace"  # 项目根目录, 子目录自动成为项目

# 多节点聚合 (可选)
workspaces:
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
    verification_token: "${IM_VERIFICATION_TOKEN}"   # 可选, 传给长连接 SDK 的事件分发器
    encrypt_key: "${IM_ENCRYPT_KEY}"   # 可选, 同上
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
- 只走飞书 WebSocket 长连接（无需公网 IP），飞书开发者后台的事件订阅和卡片回调都要选「使用长连接接收」。
- HTTP webhook 模式已移除；残留的 `connection_mode` / `allow_insecure_webhook` 只报 `config-deprecated` WARN，不再生效。

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

## 会话管理

**重置对话**：
- 用户发送 `/new` → 丢弃当前 session_id，下次消息 spawn 全新 claude 进程（无上下文）
- TTL 回收 (30min 空闲) → 进程关闭但**保留** session_id，下次消息 `--resume` 恢复上下文

两者区别：TTL 回收是"暂停"，`/new` 是"重来"。

**Reset 的两条路径** (Round 208 / SM1)：

Router 暴露两个语义不同的 reset 入口，调用方按场景选择：

- `Router.Reset(key)` — 只清理 session 本身（杀进程、从 map 删除、下次落盘不含此 key）。
  用于 LRU eviction、空闲清理等"我只是要回收这个 session"的路径。
  `workspaceOverride` 被有意保留，下次同 key 重建 session 时仍生效。
- `Router.ResetAndDiscardOverride(key)` — 在同一把 `r.mu` 下同时清理 session
  **和** `workspaceOverrides[key]`。操作员 `/new`（以及其他"用户想彻底重来"的路径）
  必须走这一条，否则存在并发 `SetWorkspace` 把旧 override 漏进新 session 的竞态窗口。

判定规则：只要语义上是"用户想丢掉这个会话的一切状态"，就用
`ResetAndDiscardOverride`；纯粹的资源回收继续走 `Reset`。

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
- **hooks 隔离** — 已通过 `--setting-sources ""` 解决。插件 Stop hooks 不加载，skills 正常。**但 MCP 不正常**（2026-08-20 实测纠正）：`--setting-sources ""` 会连带屏蔽 `~/.claude.json` 的 `mcpServers` 段，`system/init` 的 `mcp_servers` 为空数组；settings 文件里写 `mcpServers` 键、工作目录放 `.mcp.json` 均无效。唯一入口是 `--mcp-config`，见 `docs/rfc/cli-mcp-config.md`（配置项 `cli.mcp_config`）。注意默认部署走的是 `--setting-sources user`，不受此影响；仅在开启 `naozhi_settings.enabled` 时命中
- **进程僵尸** — 需定期健康检查，清理无响应进程

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

---

#### Session 侧边栏生命周期

**原则**：侧边栏显式、可控。`Remove` = 用户想"忘掉"这个 session，必须是不可逆的；没跑过的 session 不应自己冒出来。

**数据源**（两个彼此隔离）：

1. **侧边栏** (`/api/sessions` 的 `sessions` 字段) — 只读自 `Router.sessions`，其持久化源是 `sessions.json`。
2. **历史面板** (`GET /api/sessions/history` 的 `history_sessions` 字段) — 实时扫 `~/.claude` 目录（缓存 120s），与侧边栏互不干扰。`/api/sessions` 只带 `stats.history_tag`（列表的内容哈希），前端在 tag 变化时才重新拉取列表。

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
- `session.workspace`、`nodes` 已不再读取（只报 `config-deprecated` 告警），须手动改名为 `session.cwd`、`workspaces`
- 不配置 `workspace.id` 时默认取 hostname

---

## 扩展设计文档

以下功能已实现，完整设计文档独立保存：

| 功能 | 设计文档 | 状态 |
|------|---------|------|
| 多节点聚合 (Direct + Reverse) | [multi-node-design.md](multi-node-design.md) | 已实现 |
| Reverse 协议 (wire format) | [../rfc/reverse-protocol.md](../rfc/reverse-protocol.md) | 已实现 |
| Shim 进程 (零中断热重启) | [shim-design.md](shim-design.md) | 已实现 |
| 语音消息转写 | [voice-transcription.md](voice-transcription.md) | 已实现 |
