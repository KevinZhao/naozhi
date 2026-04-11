# Session Persistence via Shim Process

> **Status: RFC v3 (未实现)**
>
> 解决 naozhi 服务重启时 claude CLI 进程被杀导致 in-flight 响应丢失的问题。

## 1. 问题

### 1.1 根因

naozhi 和 claude CLI 之间通过**匿名 pipe** 通信。pipe 是内核对象，没有名字，不存在于文件系统，只靠两端进程的 fd 存在。

naozhi 退出时：内核关闭 naozhi 持有的所有 fd → stdin pipe 的 write end 关闭 → claude CLI 读到 EOF → 退出。

**pipe 不可重连。** 新启动的 naozhi 进程无法"接上"旧的 pipe。

### 1.2 影响

| 场景 | 影响 |
|------|------|
| Agent team 会话 (5-30+ min) | 重启几乎 100% 打断活跃 session，30 分钟工作白费 + 重复 API 费 |
| 频繁部署验证修复 | `TimeoutStopSec=300` 迫使每次等 5 分钟，或放弃等待强杀 |
| `--resume` 冷启动 2-7s | 恢复对话上下文但不恢复工作进度，agent 要从头跑 |
| Cron 长任务 | 被中断后需从头重跑 |

### 1.3 为什么 `--resume` 不够

`--resume` 恢复的是**对话上下文**（谁说了什么），不是**工作进度**（做到哪一步了）。一个 30 分钟的 agent 任务中途被杀，resume 后 agent 看到之前的对话但必须重新执行所有工具调用、文件编辑、测试运行。

### 1.4 目标

- naozhi 重启时，claude CLI 进程**继续运行**
- naozhi 重启后，**自动重连**到存活的 CLI 进程
- in-flight 响应**不丢失**：断连期间的 stdout 事件被缓冲，重连后回放

### 1.5 不做

- 不做跨机器的 session 迁移
- 不做 shim 集群/多 shim 协调
- 不替代 `--resume` 机制（shim 崩溃时 `--resume` 仍然是兜底）

---

## 2. 架构

### 2.1 核心思路

**pipe 不可重连，所以需要一个不会死的进程持有 pipe，对外暴露可重连的 unix socket。**

这个进程就是 shim。它是 naozhi 架构的基础设施，不是可选功能。

```
naozhi → unix socket → shim (setsid, 独立存活) → pipe → claude CLI
```

**没有"直连模式"。** 每个 CLI session 都通过 shim。所有环境（开发、生产）行为一致。

naozhi 重启时 shim 和 CLI 继续运行。naozhi 重启后自动发现并重连 shim。

### 2.2 为什么 shim 而非其他方案

| 方案 | 不可行原因 |
|------|-----------|
| setsid 让 CLI 独立 | CLI stdin pipe 的 write end 在 naozhi，naozhi 死 → pipe 断 → CLI 收 EOF |
| 命名 pipe (FIFO) | open() 阻塞、缓冲极小 (64KB)、claude CLI 不支持指定 FIFO |
| fd 传递 (SCM_RIGHTS) | 旧 naozhi 必须活着才能传 fd，crash/SIGKILL 时不可用 |
| tmux / screen | PTY 模型注入终端控制序列，破坏 NDJSON 协议 |

shim 的本质与 tmux 相同：一个持久进程持有 I/O fd，对外暴露可重连的通道。区别是 tmux 用 PTY + 终端，shim 用 pipe + unix socket。

### 2.3 架构图

```
┌─ session 层 (不变) ────────────────────────────────┐
│  Router                                             │
│    └─ ManagedSession                                │
│         └─ process  processIface                    │
│                        │                            │
└────────────────────────┼────────────────────────────┘
                         │
┌─ cli 层 ───────────────┼────────────────────────────┐
│                        ▼                            │
│                   *cli.Process                      │
│                   ┌──────────────────────┐          │
│                   │ shimConn (net.Conn)  │── unix   │
│                   │ protocol Protocol    │   socket  │
│                   │ eventLog *EventLog   │          │
│                   │ Send() / readLoop()  │          │
│                   └──────────────────────┘          │
│                                                     │
│  ShimManager                                        │
│    ├─ startShim(key, opts) → shimConn               │
│    ├─ reconnectShim(key) → shimConn                 │
│    └─ discoverShims() → []ShimState                 │
│                                                     │
│  没有 ProcessBackend 接口                            │
│  没有 DirectBackend                                  │
│  没有 mode 字段 / if-else 分支                       │
│  只有一条代码路径                                    │
└─────────────────────────────────────────────────────┘
          │ unix socket (authenticated)
          ▼
┌─ shim 进程 (始终 setsid) ───────────────────────────┐
│  ┌─────────────────────────────────┐                │
│  │ socketListener (umask 0177)     │                │
│  │ authToken (crypto/rand 32B)     │                │
│  │ ringBuffer (10k lines / 50MB)   │                │
│  │ shimWatchdog (30min no-output)  │                │
│  │ stateFile (0600)                │                │
│  │                                 │                │
│  │ cliProcess:                     │                │
│  │   cmd + stdin + stdout + stderr │── pipe         │
│  └─────────────────────────────────┘                │
└─────────────────────────────────────────────────────┘
          │ stdin/stdout/stderr pipes
          ▼
      claude CLI / kiro

所有环境行为一致:
  naozhi 退出 → shim + CLI 继续运行
  naozhi 重启 → 发现 shim → 重连 → 回放缓冲事件 → 无缝恢复
  idle_timeout 后无重连 → shim 自动退出并清理
```

### 2.4 为什么不区分 attached / detached

shim **始终 setsid**（独立 session leader），无论什么环境：

- **开发时**：你改代码重启 naozhi → shim 活着 → 重连 → 会话不丢 → 调试更快
- **生产时**：部署重启 naozhi → shim 活着 → 重连 → 零中断

两种场景收益完全一样。区分模式只增加复杂度而无收益。`idle_timeout` 兜底清理即可。

### 2.5 为什么不保留直连模式

曾考虑三种架构方案：

| | 方案 A: ProcessBackend | 方案 B: ShimProcess | 方案 C: Shim-only |
|---|---|---|---|
| 核心思路 | Process 内部加抽象层，两种后端 | processIface 两个实现 | 始终走 shim，一条路径 |
| 过渡期代码 | DirectBackend + 接口 | Process + ShimProcess 共存 | **无** |
| 终态代码路径 | 1 条 (删 DirectBackend 后) | 1 条 (删 Process 后) | **始终 1 条** |
| 分支逻辑 | if backend == shim | if type == ShimProcess | **无** |
| Process 改动 | 每个方法都改 | 零 | 一次性改为接 shim |

方案 A/B 都在维护一个终将消亡的直连路径——shim 稳定后直连模式变死代码。方案 C 没有过渡期产物，shim 就是架构本身。

开发路径：先独立建 shim（naozhi 零改动），shim 验证通过后一步到位改 Process 接 shim。

---

## 3. Shim 进程设计

### 3.1 启动方式

Shim 作为 naozhi 二进制的子命令运行：

```bash
naozhi shim run \
  --key "feishu:d:alice:general" \
  --socket "/run/user/1000/naozhi/shim-abc123.sock" \
  --state-file "~/.naozhi/shims/abc123.json" \
  --buffer-size 10000 \
  --idle-timeout 4h \
  --cli-path /usr/local/bin/claude \
  --cli-arg '-p' --cli-arg '--output-format' --cli-arg 'stream-json' \
  --cli-arg '--input-format' --cli-arg 'stream-json' \
  --cli-arg '--verbose' --cli-arg '--setting-sources' --cli-arg '' \
  --cli-arg '--dangerously-skip-permissions' --cli-arg '--model' --cli-arg 'sonnet' \
  --cwd /home/user/project
```

CLI args 使用 `--cli-arg` 重复 flag（每个 arg 一个），而非逗号分隔（避免含逗号的值被错误分割）。

naozhi 通过 `exec.Command` 启动 shim：

```go
cmd.SysProcAttr = &syscall.SysProcAttr{
    Setsid: true,  // 始终：创建新 session，独立存活
}
cmd.Env = os.Environ()
```

shim 启动后：
1. 设置信号处理（见 3.2）
2. 创建 unix socket listener（umask 0177，权限 0600）
3. 启动 claude CLI 子进程（Setpgid=true）
4. 生成 auth token（crypto/rand 32 字节）
5. 写入 state file（0600）
6. 向 stdout 输出 ready：`{"status":"ready","pid":12345,"token":"base64..."}`
7. 关闭 stdin/stdout/stderr（脱离 parent）

naozhi 读到 ready 行后，用 token 连接 unix socket。

### 3.2 信号处理

| 信号 | Shim 行为 |
|------|-----------|
| SIGHUP | 忽略 |
| SIGPIPE | 忽略 |
| SIGTERM | 开启 30s 重连宽限期；naozhi 重连则取消退出，否则优雅关闭 |
| SIGINT | 同 SIGTERM |
| SIGUSR2 | 立即优雅关闭（应急停止） |

30s 宽限期平衡了：
- systemd restart naozhi 时 shim 不被误杀（naozhi 2-5s 内重连）
- 紧急情况可 `kill <pid>` 在 30s 内停止

### 3.3 Ring Buffer

```go
type ringBuffer struct {
    lines    [][]byte
    head     int
    count    int
    maxLines int   // 默认 10000
    maxBytes int64 // 默认 50MB
    curBytes int64
    seq      int64 // 全局递增序号
}
```

双重限制（行数 + 字节），任一达到时丢弃最旧行。重连时按 seq 回放。

### 3.4 Shim Watchdog

naozhi 断连期间，shim 自行监控 CLI 健康：

- **连接中**：watchdog 由 naozhi 侧 Process.Send() 管理
- **断连后**：shim 启用 30min 无输出 watchdog，超时则 SIGKILL CLI
- **重连后**：shim watchdog 停止

### 3.5 State File

路径：`~/.naozhi/shims/<key-hash>.json`，权限 **0600**。

```json
{
    "version": 1,
    "shim_pid": 12345,
    "cli_pid": 23456,
    "socket": "/run/user/1000/naozhi/shim-a1b2c3d4.sock",
    "auth_token": "base64-encoded-32-bytes",
    "key": "feishu:d:alice:general",
    "session_id": "sess_abc123",
    "workspace": "/home/user/project",
    "cli_args": ["-p", "--output-format", "stream-json", "..."],
    "cli_alive": true,
    "started_at": "2026-04-10T01:00:00Z",
    "last_connected_at": "2026-04-10T02:30:00Z",
    "buffer_count": 42
}
```

naozhi 连接前验证：socket 路径在预期目录内 + shim_pid 存活 + /proc/pid/exe 正确。

### 3.6 Socket 路径与认证

优先 `XDG_RUNTIME_DIR`（`/run/user/<uid>/naozhi/`），回退 `~/.naozhi/run/`。目录 0700。

Socket 创建前 `Umask(0177)` 防 TOCTOU 竞态。

认证双重验证：
1. `SO_PEERCRED` 校验 UID
2. attach 握手中 `auth_token` + `subtle.ConstantTimeCompare`

---

## 4. 通信协议

### 4.1 概述

unix socket 上使用 NDJSON（每行一个 JSON 对象），与 stream-json 风格一致。

### 4.2 握手

```
naozhi → shim:  {"type":"attach","last_seq":0,"token":"base64-32-bytes"}

                认证失败:
shim → naozhi:  {"type":"auth_failed"}

                认证成功:
shim → naozhi:  {"type":"hello","shim_pid":12345,"cli_pid":23456,"cli_alive":true,
                 "session_id":"sess_abc","buffer_seq_start":100,"buffer_seq_end":142,
                 "protocol_version":1}

                回放缓冲:
shim → naozhi:  {"type":"replay","seq":101,"line":"<raw stdout NDJSON>"}
                ...
shim → naozhi:  {"type":"replay_done","count":42}

                CLI 已退出:
shim → naozhi:  {"type":"cli_exited","code":0,"signal":""}
```

`last_seq=0` 回放所有缓冲。非零值只回放 seq > last_seq 的行。

### 4.3 正常操作

**naozhi → shim:**

| type | 说明 |
|------|------|
| `write` | 写入 CLI stdin（字段 `line`: 原始 NDJSON） |
| `interrupt` | SIGINT 到 CLI 进程组 |
| `close_stdin` | 关闭 CLI stdin |
| `kill` | SIGKILL CLI 进程组 |
| `ping` | 心跳 |
| `shutdown` | 关闭 shim（close stdin → 等 5s → kill → 退出） |
| `detach` | 断开连接，shim 继续运行 |

**shim → naozhi:**

| type | 说明 |
|------|------|
| `stdout` | CLI stdout 行（字段 `seq`, `line`） |
| `stderr` | CLI stderr 行（字段 `line`） |
| `cli_exited` | CLI 退出（字段 `code`, `signal`） |
| `pong` | 心跳响应（字段 `cli_alive`, `buffered`） |
| `error` | shim 错误（字段 `msg`） |

### 4.4 心跳

naozhi 每 30s ping，连续 3 次无 pong 标记 session dead。
shim 5min 无消息标记 disconnected。

---

## 5. naozhi 侧集成

### 5.1 Process 改造

Process 不再直接持有 `*exec.Cmd`、`stdin`、`scanner`。改为持有 `shimConn`（unix socket 连接）：

```go
type Process struct {
    shimConn net.Conn      // 到 shim 的 unix socket
    shimR    *bufio.Reader
    shimW    *bufio.Writer
    shimWMu  sync.Mutex
    protocol Protocol

    SessionID string
    State     ProcessState
    mu        sync.Mutex

    eventCh  chan Event
    done     chan struct{}
    killCh   chan struct{}
    killOnce sync.Once

    noOutputTimeout time.Duration
    totalTimeout    time.Duration

    eventLog  *EventLog
    totalCost float64

    lastSeq int64  // 最后收到的 shim seq，重连时用
}
```

**readLoop** 从 shim socket 读取 NDJSON，解析 shim 协议消息，提取 `stdout` 类型中的 `line` 字段，交给 `protocol.ReadEvent()` 解析为 Event：

```go
func (p *Process) readLoop() {
    defer close(p.eventCh)
    defer close(p.done)

    for {
        line, err := p.shimR.ReadBytes('\n')
        if err != nil { break }

        var msg shimMsg
        json.Unmarshal(line, &msg)

        switch msg.Type {
        case "stdout":
            p.lastSeq = msg.Seq
            ev, _, _ := p.protocol.ReadEvent([]byte(msg.Line))
            if ev.Type == "" { continue }
            if p.protocol.HandleEvent(p.shimStdinWriter(), ev) { continue }
            select {
            case p.eventCh <- ev:
            case <-p.killCh:
                return
            }
        case "stderr":
            slog.Debug("cli stderr", "line", msg.Line)
        case "cli_exited":
            slog.Info("CLI exited via shim", "code", msg.Code)
            break
        }
    }
    // ...
}
```

**shimStdinWriter** 返回一个 `io.Writer`，写入时封装为 shim `write` 消息：

```go
func (p *Process) shimStdinWriter() io.Writer {
    return &shimWriter{p: p}
}

type shimWriter struct{ p *Process }

func (w *shimWriter) Write(data []byte) (int, error) {
    // Protocol.WriteMessage 写出完整 NDJSON 行
    // 逐行封装为 shim write 命令
    // ...
}
```

**Kill / Close / Interrupt** 改为发送 shim 协议命令：

```go
func (p *Process) Kill() {
    p.killOnce.Do(func() {
        close(p.killCh)
        p.shimSend(shimMsg{Type: "kill"})
    })
    // 不需要 cmd.Wait()、stderrDone 等 —— shim 负责
}

func (p *Process) Interrupt() {
    p.shimSend(shimMsg{Type: "interrupt"})
}

func (p *Process) Close() {
    p.shimSend(shimMsg{Type: "close_stdin"})
    // 等待 done 或超时后 Kill
}
```

### 5.2 Wrapper 改造

`Wrapper.Spawn()` 改为：启动 shim → 等 ready → 连接 socket → 协议握手 → 返回 Process：

```go
func (w *Wrapper) Spawn(ctx context.Context, opts SpawnOptions) (*Process, error) {
    // 1. 启动 shim 子进程 (setsid)
    shimProc, token := w.shimManager.StartShim(ctx, key, opts)

    // 2. 连接 shim unix socket
    conn := connectShim(shimProc.SocketPath, token)

    // 3. 创建 Process
    proc := &Process{shimConn: conn, protocol: w.Protocol.Clone(), ...}

    // 4. 协议握手 (stream-json: no-op; ACP: initialize + session/new)
    rw := &JSONRW{W: proc.shimStdinWriter(), R: &shimLineReader{proc}}
    sessionID, _ := proto.Init(rw, opts.ResumeID)

    // 5. 启动 readLoop
    proc.startReadLoop()
    return proc, nil
}
```

### 5.3 ShimManager

```go
type ShimManager struct {
    stateDir    string
    socketDir   string
    cliPath     string
    idleTimeout time.Duration
    maxShims    int
}

// StartShim 启动新 shim 进程
func (m *ShimManager) StartShim(ctx context.Context, key string, opts SpawnOptions) (*ShimInfo, error)

// Reconnect 连接已存在的 shim（重启后）
func (m *ShimManager) Reconnect(ctx context.Context, key string) (net.Conn, *ShimHello, error)

// Discover 扫描 state files 发现存活 shim
func (m *ShimManager) Discover() ([]ShimState, error)

// StopAll 停止所有 shim
func (m *ShimManager) StopAll(ctx context.Context)
```

### 5.4 Router 重连

`Router.reconnectShims()` 在 `loadStore()` 之后运行：

1. `ShimManager.Discover()` 扫描 state files
2. 验证每个 shim：PID 存活 + socket 可连 + CLI args 一致（不一致则 shutdown 旧 shim）
3. 连接 socket，接收 hello + 回放事件
4. 回放事件直接注入 `eventLog`（不走 eventCh，避免死锁）
5. 创建 Process，调用 `ManagedSession.ReattachProcess()` 注入
6. 更新 `activeCount` + `storeGen` + `notifyChange`

### 5.5 Shutdown 流程

```
SIGTERM → 等 Running Send() 返回 → saveStore
       → 对所有 shim 发 detach（断连但不停 shim）
       → 退出
```

shim 检测到断连后进入 Disconnected 状态，继续缓冲事件。
naozhi 重启后自动重连。

显式停止所有 shim：`naozhi shim stop --all`

---

## 6. 配置

```yaml
session:
  shim:
    buffer_size: 10000               # ring buffer 行数上限
    max_buffer_bytes: "50MB"         # ring buffer 字节上限
    idle_timeout: "4h"               # 无 naozhi 连接后自动退出
    disconnect_watchdog: "30m"       # 断连时 CLI 无输出超时
    max_shims: 6                     # 最大 shim 数量
    state_dir: "~/.naozhi/shims"
```

没有 `enabled` 开关。shim 是架构，不是功能。

---

## 7. systemd 配置

```ini
[Service]
ExecStart=/usr/local/bin/naozhi
Restart=always
RestartSec=2
KillMode=process            # 只杀 naozhi 主进程，shim 不受影响
TimeoutStopSec=30           # 不需要等 CLI 完成，只等 detach
Environment=HOME=/home/ec2-user
WorkingDirectory=/home/ec2-user
```

`TimeoutStopSec` 从 300s 降到 30s。

---

## 8. 文件变更清单

| 文件 | 操作 | 内容 |
|------|------|------|
| `internal/shim/protocol.go` | **NEW** | shim 协议消息类型定义 |
| `internal/shim/state.go` | **NEW** | State file 读写 (0600) |
| `internal/shim/buffer.go` | **NEW** | Ring buffer (行数 + 字节双限制) |
| `internal/shim/watchdog.go` | **NEW** | 断连 watchdog |
| `internal/shim/manager.go` | **NEW** | ShimManager (启动/发现/重连) |
| `internal/shim/*_test.go` | **NEW** | 各组件测试 |
| `cmd/naozhi/shim.go` | **NEW** | `naozhi shim run/stop/list` 子命令 |
| `cmd/naozhi/shim_run.go` | **NEW** | shim 进程主循环 |
| `internal/cli/process.go` | **MODIFY** | cmd/stdin/scanner → shimConn |
| `internal/cli/wrapper.go` | **MODIFY** | Spawn 改为启动 shim |
| `internal/session/managed.go` | **MODIFY** | 增加 ReattachProcess() |
| `internal/session/router.go` | **MODIFY** | 增加 reconnectShims() |
| `internal/config/config.go` | **MODIFY** | 增加 ShimConfig |
| `cmd/naozhi/main.go` | **MODIFY** | 初始化 ShimManager |

---

## 9. 实现阶段

```
Phase 1: 建 shim 进程 (naozhi 零改动，独立组件)
  ├─ internal/shim/ (protocol, buffer, state, watchdog)
  ├─ cmd/naozhi/shim_run.go (主循环)
  ├─ cmd/naozhi/shim.go (子命令)
  └─ 独立测试: naozhi shim run + 手写客户端验证

Phase 2: 改 Process 接 shim (一步到位)
  ├─ process.go (shimConn 替代 cmd/stdin/scanner)
  ├─ wrapper.go (Spawn 启动 shim)
  ├─ config.go (ShimConfig)
  └─ main.go (ShimManager 初始化)

Phase 3: Router 重连 + 端到端验证
  ├─ router.go (reconnectShims, ReattachProcess)
  ├─ managed.go (ReattachProcess)
  ├─ systemd unit 更新
  └─ 端到端: 发长任务 → 重启 naozhi → 自动重连 → 收到完整结果
```

Phase 1 是纯增量（新文件），naozhi 现有功能不受影响。
Phase 2 是 Process 重写，此时 naozhi 暂时不可用，直到改造完成。
Phase 3 是闭环验证。

---

## 10. 边界情况

### 10.1 shim 启动时旧 socket 残留

shim 启动前 `net.Dial` 检测旧 socket。可连接 → 旧 shim 存活，报错退出。不可连接 → 删除残留文件。

### 10.2 naozhi 重连时 CLI 已退出

hello 消息包含 `cli_alive: false`。naozhi 回放缓冲中残留事件，然后 shutdown shim，session 标记 Dead，下次消息 `--resume`。

### 10.3 配置变更后重连

reconnectShims 比较 state file 中的 `cli_args` 与当前配置。不一致 → shutdown 旧 shim，下次消息用新配置创建。

### 10.4 shim 本身崩溃

CLI 的 stdin pipe 读端关闭 → CLI 收 EOF 退出。naozhi 连接 shim 失败 → 清理 state file → session 回退到 `--resume`。

### 10.5 多个 naozhi 竞争同一 shim

shim 同一时刻只接受一个连接。新连接踢掉旧连接（last-writer-wins）。

---

## 11. 安全

| 措施 | 说明 |
|------|------|
| Socket umask 0177 | 创建时即 0600，无 TOCTOU 窗口 |
| Socket 目录 0700 | 额外隔离 |
| SO_PEERCRED | 验证连接方 UID |
| auth_token | 32 字节 crypto/rand + subtle.ConstantTimeCompare |
| State file 0600 | 防读取 token |
| socket 路径验证 | 必须在预期目录内，防 state file 篡改 |
| /proc/pid/exe 检查 | 确认 shim PID 是 naozhi 二进制 |
| max_shims | 防资源耗尽 |
| Ring buffer 字节上限 | 50MB 防内存爆炸 |
| SIGTERM 30s 宽限期 | 可管理，不会成为不可杀进程 |

---

## 12. 设计演化记录

### v1 → v2: 架构 review

修复了 3 个 CRITICAL（eventCh 死锁、nil stdin panic、ACP HandleEvent panic）、6 个 HIGH（Kill/Close nil cmd、无 setProcess 方法、activeCount 不更新等）。引入 ProcessBackend 接口 + DirectBackend / ShimBackend 双模式。

### v2 → v3: 架构简化

经过进一步 review，决定放弃 ProcessBackend 双模式方案，原因：

1. **终态是 shim-only** — 直连模式终将消亡。ProcessBackend/DirectBackend 是过渡期产物
2. **不需要区分 attached/detached** — shim 始终 setsid，所有环境行为一致
3. **不需要脚手架过渡** — shim 是独立进程，可在 naozhi 外独立开发测试，完成后一步到位改 Process
4. **processIface 已是正确边界** — 上层 ManagedSession/Router 完全通过接口工作，不关心底层 I/O

v3 = 先建 shim，再改 Process，没有中间态。
