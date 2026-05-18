# Multi-Backend (Claude + Kiro) 切换与并存

> **状态**: 设计提案 v2（**基于 2026-05-18 实测可行性验证修订**）
> **作者**: naozhi team
> **创建**: 2026-05-18
> **依赖 / 前置**:
> - `internal/cli/protocol.go`、`protocol_acp.go`、`protocol_claude.go`（已存在）
> - `internal/cli/wrapper.go`（多 backend wrappers map 已就位）
> - `internal/session/router.go` `wrapperFor` / `BackendIDs` / `BackendWrapper`（已就位）
> - `internal/config/config.go` `EnabledBackends` / `DefaultBackendID`（已就位）
> - `docs/rfc/event-log-persistence.md`（naozhilog 持久化，本 RFC 在 §3.4 复用）
> - `docs/rfc/reverse-protocol.md`（capability 字段已定义，本 RFC §6 落地路由）
> **关联代码**: `internal/cli/wrapper.go` · `internal/cli/protocol_acp.go` · `internal/cli/protocol_claude.go` · `internal/cli/detect.go` · `internal/cli/rpc_types.go` · `internal/session/router.go` · `internal/server/server.go` · `internal/dispatch/dispatch.go` · `internal/discovery/proc_*.go` · `internal/node/caps.go` · `internal/node/reverseserver.go`
> **可行性验证**: `docs/rfc/multi-backend-validation.md`（V1-V12 实测脚本与日志）

---

## 0. TL;DR

naozhi 已经把多 backend 的**基础设施**搭齐了 70%（config schema、Protocol 抽象、Wrapper 多实例、Router wrapperFor、Dashboard picker、Shim state 记录 backend、history.Source 接口）。把"切到 Kiro 也能完整工作"和"两个 backend 并存"做完整，剩下四件事：

1. **修两个真 bug**（hotfix，与切换正交，但不修 kiro 跑不通）：`RPCMessage.ID` 类型错（int → 应为 RawMessage 兼容 string），`permission optionId` 硬编码值不对（`"allow-once"` → 应从 request 的 `options[].optionId` 读取）。
2. **收敛散点**（重构）：把"backend → DisplayName / DefaultBinary / Protocol / DetectInProc / RequiredNodeCaps"的散点收敛到 `internal/cli/backend.Profile`，其他模块只调注册表，不再 switch backend ID。
3. **补五块后端功能**：`internal/history/kirojsonl/` 让 kiro 历史落地、ACP `session/cancel` 软中断、reverse-node capability 路由、per-session ReplyTag、服务端 normalize 层（统一 cost / context% / turn duration / metering）。
4. **Dashboard 全套差异化区分（§8）**：chip / cost 单位 / context 用量条 / 能力灰显 / tool_call 渲染 / slash 命令 / reverse 兼容性校验 — 26 处 UI 改动按 26 条规约逐一实现，前端工程师可并行四个 Sprint 完成。

实测一句话总结：**Kiro 的 ACP 实现质量出乎意料地好**（自带 jsonl 历史 / stale lock 自动恢复 / cancel 立即生效 / 多 session 并发），多后端的设计可以更激进，工时不增反减。

---

## 1. 背景与动机

### 1.1 当前状态

- naozhi 设计目标是**协议无关**的 IM 网关：CLI Wrapper → Session Router → Channel Adapter
- `internal/cli/protocol.go` 已抽象为 `Protocol` 接口，`ClaudeProtocol`（stream-json）和 `ACPProtocol`（JSON-RPC 2.0 Agent Client Protocol）已实现
- 主程序 `cmd/naozhi/main.go:472-541` 已经能按 `cfg.EnabledBackends()` for-loop 构造多个 wrapper，写入 `wrappers` map，传给 Router
- Dashboard `/api/cli/backends` + new-session picker 已经支持 per-session 选 backend（`dashboard.js:81`、`4682-4730`）
- Shim state 文件已记录 `Backend` 字段，重启重连能按 backend 路由

### 1.2 实证驱动的 gap 分析

2026-05-18 在本地装好 kiro-cli 2.3.0 后跑 12 项 PoC（V1-V12，详见 `multi-backend-validation.md`），结果：

| 维度 | 实证结果 | 对方案影响 |
|---|---|---|
| ACP `session/cancel` | notification 形式（无 id），原 prompt 立即回 `stopReason:"cancelled"`，同 sessionId 立即可继续 | Sprint 3 大幅简化 |
| `session/load` 持久性 | kiro 自动写 `~/.kiro/sessions/cli/<sid>.{json,jsonl}`；新进程 load 同 sid 上下文完整恢复 | naozhi 重启后能复用 kiro session |
| Stale lock 自动恢复 | kiro SIGKILL 后留 `.lock` 文件，下一进程读 PID 检测 stale 自动接管 | naozhi 不需写清锁逻辑 |
| `RPCMessage.ID` 类型 | kiro `permission_request` 的 id 是 **UUID 字符串** (`"82017692-..."`), naozhi 当前 `*int` 反序列化报错 | **必修 bug** |
| permission optionId | kiro 给的 `options[].optionId` 是 `allow_once / allow_always / reject_once`，naozhi 硬编码 `allow-once`（连字符）不被识别 | **必修 bug** |
| Chunk 粒度 | 平均 2.2 字符 / chunk，15 chunks/sec — 309 chunks/turn。naozhi 当前 buffer 累加策略正确 | 历史回放走 kiro 自带 jsonl 而非 naozhilog |
| Reverse capability 路由 | `Capabilities []string` 字段已定义，但 `reverseserver.go:291` 仅 `logUnknownCaps` 打 warn，**无路由决策**使用 | **从"补一下"升级为"实现路由"** |
| 多 session 并发 | 一个 kiro acp 进程能并发 N 个独立 session（context 隔离） | 未来优化空间，本 RFC 仍按 1:1 |
| 启动延迟 | `--version` 3ms / init 585ms / session/new 600ms | detect 串行化无需优化 |
| SIGTERM 优雅退出 | 自动清 lock，jsonl 已 flush，退出码 -15 | shim 关闭路径无特殊处理 |

### 1.3 范围

**做**：
- Backend 抽象层（`internal/cli/backend.Profile`）
- Kiro 历史源（`internal/history/kirojsonl/`）
- ACP 软中断（`session/cancel` notification）
- Per-session ReplyTag
- Reverse-node capability 路由
- Cron job backend 字段
- Metrics backend label
- Config Validate + doctor + 文档

**不做**：
- 一个 shim 多 session（V11 验证可行，但与现有 `Process.SessionID` 1:1 模型不兼容，留作 v2）
- 单 session 跨 backend 续传（session ID 格式与 history 都不互通，物理不可能）
- 同 chat 多 backend 并发（与 chat-key 派生规则冲突，留作 v2）

---

## 2. 必前置 hotfix（Sprint 0a，与本 RFC 正交）

### 2.1 Bug A: RPCMessage.ID 类型错

**文件**：`internal/cli/rpc_types.go:14-21`

**当前**：
```go
type RPCMessage struct {
    JSONRPC string          `json:"jsonrpc"`
    ID      *int            `json:"id,omitempty"`     // ❌ 锁死 int
    Method  string          `json:"method,omitempty"`
    Params  json.RawMessage `json:"params,omitempty"`
    Result  json.RawMessage `json:"result,omitempty"`
    Error   *RPCError       `json:"error,omitempty"`
}
```

**实证（V8）**：
```
kiro permission_request → {"id":"82017692-c404-42d1-9334-ae28dfda0cee", ...}
naozhi unmarshal       → "json: cannot unmarshal string into Go struct field RPCMessage.id of type int"
```

readLoop 在 `protocol_acp.go:200` `json.Unmarshal` 直接 return err，整个 ACP session 卡死。

**修复**：
```go
type RPCMessage struct {
    JSONRPC string          `json:"jsonrpc"`
    ID      json.RawMessage `json:"id,omitempty"`     // ✅ 兼容 int / string
    Method  string          `json:"method,omitempty"`
    Params  json.RawMessage `json:"params,omitempty"`
    Result  json.RawMessage `json:"result,omitempty"`
    Error   *RPCError       `json:"error,omitempty"`
}

func (m *RPCMessage) IDAsInt() (int, bool) {
    if len(m.ID) == 0 { return 0, false }
    var n int
    if err := json.Unmarshal(m.ID, &n); err == nil { return n, true }
    return 0, false
}

func (m *RPCMessage) IDAsString() (string, bool) {
    if len(m.ID) == 0 { return "", false }
    if n, ok := m.IDAsInt(); ok { return strconv.Itoa(n), true }
    var s string
    if err := json.Unmarshal(m.ID, &s); err == nil { return s, true }
    return "", false
}
```

`IsRequest` / `IsResponse` / `IsNotification` 改判 `len(m.ID) > 0`。`Event.RPCRequestID` 也要从 `int` 升级到 `string`（用 `IDAsString` 统一）。

**RPCRequest.ID 保留 int**：naozhi 自己生成的 RPC id 始终是 int（`ACPProtocol.allocID` 单调递增），不需要兼容 string。

### 2.2 Bug B: permission optionId 硬编码错值

**文件**：`internal/cli/protocol_acp.go:263-284`

**当前**：
```go
resp := permissionResponse{
    Result: permissionResult{
        Outcome: permissionOutcome{Outcome: "selected", OptionID: "allow-once"},  // ❌
    },
}
```

**实证（V7b）**：kiro 实际给的 options：
```json
"options": [
  {"optionId": "allow_once",  "name": "Yes",    "kind": "allow_once"},
  {"optionId": "allow_always","name": "Always", "kind": "allow_always"},
  {"optionId": "reject_once", "name": "No",     "kind": "reject_once"}
]
```

下划线 `allow_once`，不是连字符 `allow-once`。

**修复**：从 `permission_request.params.options` 读 `optionId`，按 `kind` 选第一个 `allow_*` 的：

```go
type permissionRequestParams struct {
    SessionID string `json:"sessionId"`
    ToolCall  struct {
        ToolCallID string `json:"toolCallId"`
        Title      string `json:"title"`
    } `json:"toolCall"`
    Options []struct {
        OptionID string `json:"optionId"`
        Name     string `json:"name"`
        Kind     string `json:"kind"`  // "allow_once" / "allow_always" / "reject_once"
    } `json:"options"`
}

func (p *ACPProtocol) HandleEvent(w io.Writer, ev Event) bool {
    if ev.Type != "permission_request" { return false }
    var params permissionRequestParams
    if err := json.Unmarshal(ev.RawParams, &params); err != nil {
        slog.Warn("acp: parse permission_request params", "err", err)
        return true
    }
    chosen := ""
    for _, o := range params.Options {
        if strings.HasPrefix(o.Kind, "allow_") {
            chosen = o.OptionID
            break
        }
    }
    if chosen == "" {
        slog.Warn("acp: no allow option in permission_request, defaulting to allow_once")
        chosen = "allow_once"
    }
    // 用 ev.RPCRequestID（string，已经 hotfix A 升级类型）回写
    ...
}
```

`Event` 结构体新增 `RawParams json.RawMessage` 字段把原始 params 透传到 HandleEvent。

### 2.3 验收

- 单元测试：`protocol_acp_id_test.go` 覆盖 string ID / int ID / 缺失 ID 三种 unmarshal
- 集成测试：fake kiro acp 模拟 permission_request，断言 naozhi 自动选 `allow_once` 并能继续 prompt

**这两个 fix 应作为单独 hotfix PR 上线**，与本 RFC 任何后续工作解耦。

---

## 3. backend.Profile 抽象

### 3.1 现状散点

当前散布在 6 个文件的 backend 知识（all switch on backend ID）：

| 文件 | 行号 | 散点内容 |
|---|---|---|
| `cli/wrapper.go` | 60-67, 122-124 | `backendDisplayName`, `detectCLI` 里的 binary 名 |
| `cli/detect.go` | 23-26 | `knownBackends` |
| `cmd/naozhi/main.go` | 478-495 | `switch b.ID { case "kiro": proto = &cli.ACPProtocol{}; ...}` |
| `discovery/proc_linux.go` | 49-65 | `detectCLIName` 字符串匹配 |
| `discovery/proc_darwin.go` | — | 同上 |
| `server/server.go` | 423-427, 476 | `if backend == "kiro" { tag = "kiro" }` |
| `session/router.go` | 1115-1121 | `attachHistorySource` 里 `case backend == "claude"` |

### 3.2 Profile 设计

新增 `internal/cli/backend/profile.go`：

```go
package backend

import (
    "github.com/naozhi/naozhi/internal/cli"
)

// Profile 是一个 backend 的完整能力描述。每个 backend 注册一份 Profile，
// 其他模块通过 backend.Get(id) 查询，禁止 switch backend ID。
type Profile struct {
    // 不可变标识
    ID            string  // "claude" | "kiro" | "gemini"
    DisplayName   string  // "claude-code" | "kiro" | "gemini-cli"
    DefaultBinary string  // 当 cli.path 未配置时的探测目标 binary 名
    DefaultTag    string  // 默认 reply tag（"cc" / "kiro" / "gem"），可被 config 覆盖

    // 协议构造
    NewProtocol func(ProtocolDeps) cli.Protocol

    // 进程发现 — 用于 discovery/proc_*.go 识别本机 CLI 进程
    DetectInProc func(cmdline string) bool

    // 反向节点路由 — 子节点必须申报这些 cap 才能承载本 backend 的 session
    // 空切片 = 不需要任何特殊 cap（claude 是这种）
    RequiredNodeCaps []string
}

type ProtocolDeps struct {
    SettingsFile    string         // claude --settings 文件
    RefreshSettings func() string  // claude 每次 spawn 重新生成 settings
}

// Registry — 显式 RegisterDefaults，禁止 init() 自注册（避免漏 import 时静默缺失）
var registry = map[string]Profile{}

func Register(p Profile) {
    if _, exists := registry[p.ID]; exists {
        panic("backend: duplicate registration of " + p.ID)
    }
    registry[p.ID] = p
}

func Get(id string) (Profile, bool) {
    p, ok := registry[id]
    return p, ok
}

func MustGet(id string) Profile {
    p, ok := registry[id]
    if !ok { panic("backend: unknown id " + id) }
    return p
}

func All() []Profile { /* sorted by registration order */ }

// RegisterDefaults 必须在 main.go 启动早期调用一次。
// 不用 init() 是为了显式控制 — 测试可以选择只注册需要的 backend。
func RegisterDefaults() {
    Register(claudeProfile())
    Register(kiroProfile())
}
```

### 3.3 注册示例

```go
// internal/cli/backend/profile_claude.go
func claudeProfile() Profile {
    return Profile{
        ID:               "claude",
        DisplayName:      "claude-code",
        DefaultBinary:    "claude",
        DefaultTag:       "cc",
        NewProtocol: func(d ProtocolDeps) cli.Protocol {
            return &cli.ClaudeProtocol{
                SettingsFile:    d.SettingsFile,
                RefreshSettings: d.RefreshSettings,
            }
        },
        DetectInProc: func(cmd string) bool {
            return strings.Contains(cmd, "claude") && !strings.Contains(cmd, "kiro")
        },
        RequiredNodeCaps: nil,  // claude 是默认能力
    }
}

// internal/cli/backend/profile_kiro.go
func kiroProfile() Profile {
    return Profile{
        ID:               "kiro",
        DisplayName:      "kiro",
        DefaultBinary:    "kiro-cli",
        DefaultTag:       "kiro",
        NewProtocol: func(d ProtocolDeps) cli.Protocol {
            return &cli.ACPProtocol{}
        },
        DetectInProc: func(cmd string) bool {
            return strings.Contains(cmd, "kiro")
        },
        RequiredNodeCaps: []string{"acp"},  // 子节点必须申报支持 acp 才能承载 kiro session
    }
}
```

### 3.4 散点收敛

| 旧位置 | 新做法 |
|---|---|
| `cli/wrapper.go:60-67` `backendDisplayName` | `backend.MustGet(id).DisplayName` |
| `cli/wrapper.go:122-124` 硬编码 `kiro-cli` | `backend.MustGet(id).DefaultBinary` |
| `cli/detect.go:23-26` `knownBackends` | `backend.All()` |
| `cmd/naozhi/main.go:478-495` switch | `backend.MustGet(b.ID).NewProtocol(deps)` |
| `discovery/proc_*.go:49-65` | for each `backend.All()`, 调 `p.DetectInProc(cmd)` |
| `server/server.go:423-427` `tag = "kiro"` | 删除全局 tag，改 per-session（§5） |
| `session/router.go:1115-1121` | `attachHistorySource` 走 `wrapper.NewHistorySource`（§4） |

### 3.5 Caps 不放进 Profile

原因：`cli.Caps`（Replay / Priority / SoftInterrupt / StreamJSON）来自 `Protocol.Capabilities()` 方法，已经是运行时事实来源。在 Profile 里再存一份会有漂移风险。需要时 `cli.ProtocolCaps(p.NewProtocol(deps))` 取即可。

---

## 4. Kiro 历史源（kirojsonl）

### 4.1 实证依据

V2 / V9 验证：kiro 在每个 session 起来时建立 `~/.kiro/sessions/cli/<sid>.{json,jsonl}`，SIGTERM 后 jsonl 已 flush，格式：

```jsonl
{"version":"v1","kind":"Prompt","data":{"message_id":"...","content":[{"kind":"text","data":"..."}],"meta":{"timestamp":...}}}
{"version":"v1","kind":"AssistantMessage","data":{"message_id":"...","content":[{"kind":"text","data":"..."}]}}
```

按消息聚合（不是按 chunk），naozhi 直接读这个就能拿到完整历史，**不依赖 chunk 级 naozhilog**。

### 4.2 设计

新增 `internal/history/kirojsonl/source.go`，模式克隆 `claudejsonl`：

```go
package kirojsonl

import (
    "context"
    "encoding/json"
    "os"
    "path/filepath"
    "github.com/naozhi/naozhi/internal/cli"
    "github.com/naozhi/naozhi/internal/history"
)

type Source struct {
    rootDir   string                  // ~/.kiro/sessions/cli
    sessionID func() string           // 当前 session 的 kiro sessionId（动态，可能 resume 切换）
}

var _ history.Source = (*Source)(nil)

func New(kiroSessionsDir string, sessionIDFn func() string) *Source {
    return &Source{rootDir: kiroSessionsDir, sessionID: sessionIDFn}
}

// LoadBefore 实现 history.Source：把 jsonl 解析成 cli.EventEntry
func (s *Source) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]cli.EventEntry, error) {
    sid := s.sessionID()
    if sid == "" { return nil, nil }
    path := filepath.Join(s.rootDir, sid+".jsonl")
    f, err := os.Open(path)
    if errors.Is(err, os.ErrNotExist) { return nil, nil }
    if err != nil { return nil, err }
    defer f.Close()
    // 解析 v1 schema → EventEntry
    // - Prompt → user EventEntry
    // - AssistantMessage → assistant EventEntry
    // - 其他 kind（tool 调用等，未来可扩）目前忽略
    ...
}
```

### 4.3 Wrapper 工厂化

为避免 `internal/session/router.go` 反向依赖 `kirojsonl` / `claudejsonl`（呼应 docs/TODO.md R219-ARCH-2），把 history 工厂下放到 `cli.Wrapper`：

```go
// internal/cli/wrapper.go
type HistorySessionView interface {
    Key() string
    Workspace() string
    SnapshotChainIDs() []string  // ManagedSession 上现有未导出的 snapshotChainIDs 升级为 public
    SessionID() string
}

type historyFactory func(s HistorySessionView, deps HistoryWiring) history.Source

type HistoryWiring struct {
    ClaudeDir       string  // ~/.claude/projects
    KiroSessionsDir string  // ~/.kiro/sessions/cli
    EventLogDir     string  // naozhi event log dir（与 kirojsonl 通过 merged.Source 合并）
}

type Wrapper struct {
    BackendID      string
    CLIPath        string
    CLIName        string
    CLIVersion     string
    Protocol       Protocol
    ShimManager    *shim.Manager
    historyFactory historyFactory  // ← 新增，NewWrapper 时按 backend 绑定
}

func (w *Wrapper) NewHistorySource(s HistorySessionView, deps HistoryWiring) history.Source {
    if w.historyFactory == nil { return history.Noop{} }
    return w.historyFactory(s, deps)
}
```

`NewWrapper` 时按 backend 绑：

```go
func NewWrapper(cliPath string, proto Protocol, backendID string) *Wrapper {
    w := &Wrapper{...}
    switch backendID {
    case "claude":
        w.historyFactory = func(s HistorySessionView, d HistoryWiring) history.Source {
            if d.ClaudeDir == "" { return history.Noop{} }
            return claudejsonl.New(d.ClaudeDir, s.Workspace(), s.SnapshotChainIDs)
        }
    case "kiro":
        w.historyFactory = func(s HistorySessionView, d HistoryWiring) history.Source {
            if d.KiroSessionsDir == "" { return history.Noop{} }
            return kirojsonl.New(d.KiroSessionsDir, s.SessionID)
        }
    }
    return w
}
```

### 4.4 Router 改造

`router.go attachHistorySource` 简化为：

```go
func (r *Router) attachHistorySource(s *ManagedSession) {
    backend := s.Backend()
    if backend == "" { backend = r.defaultBackend }
    wrapper := r.wrappers[backend]
    if wrapper == nil { wrapper = r.wrapper }
    if wrapper == nil {
        s.SetHistorySource(history.Noop{})
        return
    }
    fallback := wrapper.NewHistorySource(s, cli.HistoryWiring{
        ClaudeDir:       r.claudeDir,
        KiroSessionsDir: r.kiroSessionsDir,
        EventLogDir:     r.eventLogDir,
    })
    if r.eventLogDir == "" {
        s.SetHistorySource(fallback)
        return
    }
    s.SetHistorySource(&merged.Source{
        Local:    naozhilog.New(r.eventLogDir, s.key),
        Fallback: fallback,
    })
}
```

`Router.kiroSessionsDir` 从 config 注入（默认 `~/.kiro/sessions/cli`）。

### 4.5 验收

- 一轮 kiro session → 关 naozhi → 重启 → dashboard "load earlier" 能拉到 jsonl 里的历史
- 单测覆盖 v1 schema 解析 + 缺失 jsonl 文件 + 损坏行（partial write）
- `go test -race` 全绿

---

## 5. ACP 软中断（session/cancel notification）

### 5.1 实证依据（V1）

```
naozhi → kiro: {"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"..."}}    (notification, no id)
kiro   → naozhi: original prompt RPC 立即回 {"jsonrpc":"2.0","result":{"stopReason":"cancelled"},"id":3}
naozhi → kiro: 同 sessionId 立即发新 session/prompt id=5
kiro   → naozhi: 3.3s 后正常回 {"result":{"stopReason":"end_turn"},"id":5}
```

cancel 后 buffer 里残留约 10 个 chunk 涌出（已经在飞的网络包），naozhi 现有 readLoop 安全吃掉即可。

### 5.2 设计

`internal/cli/protocol_acp.go` `WriteInterrupt`：

```go
func (p *ACPProtocol) WriteInterrupt(w io.Writer, _ string) error {
    p.mu.Lock()
    sid := p.sessionID
    p.mu.Unlock()
    if sid == "" {
        // 还没握手完成，没法 cancel
        return ErrInterruptUnsupported
    }
    notif := map[string]any{
        "jsonrpc": "2.0",
        "method":  "session/cancel",
        "params":  map[string]any{"sessionId": sid},
    }
    data, err := json.Marshal(notif)
    if err != nil { return err }
    _, err = w.Write(append(data, '\n'))
    return err
}
```

立即返回 nil，**不需要 `Protocol.AwaitInterrupt`**。原因：原 prompt RPC 自己会发 cancelled result，readLoop 走正常 turn-complete 路径，turn 状态 ManagedSession 自然回 Ready。

### 5.3 Capabilities 升级

```go
func (p *ACPProtocol) Capabilities() Caps {
    return Caps{
        Replay: false, Priority: false,
        SoftInterrupt: true,  // ← 由 false 升级为 true，反映实际能力
        StreamJSON: false,
    }
}
```

### 5.4 cancel + 立即重发的稳态

V1 实证：cancel notification → 50ms 内收到 cancelled result → 立即发新 prompt OK。Process.Send 的 commandQueue 在 stream-json 模式下已经能处理 "interrupt 后立即 send 下一条"，ACP 走相同路径。

无需为 cancel 单独加 await/同步逻辑。

### 5.5 验收

- 单元：fake kiro 收到 cancel notification 后回 cancelled result，naozhi turn 状态正确转 Ready
- 集成：dashboard /cancel 在 kiro session 上，进程不被 SIGKILL，turn 优雅结束
- Soak：10 次 cancel/resend 循环不崩、不漏事件

---

## 6. Reverse-node Capability 路由

### 6.1 现状（V6 audit）

- `internal/node/protocol.go:88-90` 定义 `Capabilities []string` 字段
- `internal/node/caps.go:knownServerCaps` 包含 `acp`、`gemini`、`askuser`、`attach`、`scratch`
- `reverseserver.go:291` 调 `logUnknownCaps` 仅打 WARN
- 全仓 grep `HasCap` / `selectNode` / `routeByCap` / `filterNode`：**零结果**
- `ClientMsg.Backend` 字段虽然定义，但 reverse dispatch 链路未做 backend → cap 匹配

**结论**：capability 路由实质未实现。多后端 + 反向节点配置出来的 kiro session 会派到任意节点（包括不支持 acp 的），spawn 失败。

### 6.2 设计

#### 6.2.1 节点状态结构扩展

```go
// internal/node/registry.go (新增或扩展现有 Hub.nodes)
type NodeMeta struct {
    NodeID       string
    DisplayName  string
    Hostname     string
    Capabilities map[string]bool  // O(1) 查询
    RegisteredAt time.Time
}

// 注册时由 ReverseMsg.Capabilities 填充
func (m *NodeMeta) HasCap(cap string) bool {
    if cap == "" { return true }
    return m.Capabilities[cap]
}
```

#### 6.2.2 路由查询

```go
// internal/server/nodeclient.go (新增)
func (s *Server) selectNodeForBackend(targetNode, backendID string) (*node.Conn, error) {
    profile, ok := backend.Get(backendID)
    if !ok { return nil, fmt.Errorf("unknown backend %q", backendID) }

    // 显式指定 node — 仅校验 cap 是否满足
    if targetNode != "" {
        nc, ok := s.nodes[targetNode]
        if !ok { return nil, fmt.Errorf("node %q not connected", targetNode) }
        for _, cap := range profile.RequiredNodeCaps {
            if !nc.Meta().HasCap(cap) {
                return nil, fmt.Errorf("node %q lacks required cap %q for backend %q", targetNode, cap, backendID)
            }
        }
        return nc, nil
    }

    // 未指定 node — 走本地（local backend 必须有 backend 对应 wrapper）
    return nil, nil  // 调用方按 nil 走本地路径
}
```

#### 6.2.3 调用点

`server/send.go` 在派发到节点之前先调 selectNodeForBackend，把不兼容的请求 reject 而不是 silently 派失败。Dashboard `dashboard.js` 的 node 选择器在用户挑 backend 后过滤候选 node 列表（前端 UX 改进，可推迟 v2）。

### 6.3 配置不变

子节点 `upstream.capabilities` 由 RegisterDefaults 中每个 Profile 的 `RequiredNodeCaps` 推导：子节点启动时枚举 `backend.All()`，把每个 enabled profile 的 RequiredNodeCaps 并集申报。

### 6.4 验收

- 单测：双节点（一支持 acp 一不支持），dispatcher 能正确路由 kiro 请求到支持的节点
- 错误路径：用户显式选了 unsupported node + backend 组合，返回明确错误
- 兼容性：旧 client 不申报 caps 时，仅 claude session 派得过去（默认行为）

---

## 7. Per-session ReplyTag

### 7.1 现状

`internal/server/server.go:54, 423-427, 476` 把 `backendTag` 当成 server 全局字段（`"cc"` 或 `"kiro"`），`dispatch.replyFooter` 在构造时一次注入，所有 IM 回复尾部带相同 tag。

多后端共存时这是错的：用户起 claude session 收到 `[kiro]` tag 会困惑。

### 7.2 设计

#### 7.2.1 DispatcherConfig 改成函数

```go
// internal/dispatch/dispatch.go
type DispatcherConfig struct {
    ...
    // 旧：ReplyFooter string
    ReplyFooterFn func(backendID string) string  // 入参为 session 的 backend ID
}
```

`dispatch.Dispatcher.replyFooter` → `replyFooterFn`。所有调用点（IM fanout / cron 通知 / dashboard reply）取 `sess.Backend()` 传进去；缺失时传 `""`，fn 返回 default tag。

#### 7.2.2 Server 实现

```go
// server/server.go buildServer
opts.ReplyFooterFn = func(backend string) string {
    if backend == "" { backend = router.DefaultBackend() }
    if p, ok := backendpkg.Get(backend); ok { return p.DefaultTag }
    return "cc"
}
```

#### 7.2.3 可选：config 覆盖

```yaml
cli:
  backends:
    - id: "kiro"
      reply_tag: "Kiro"  # 覆盖 Profile.DefaultTag
```

`CLIBackendConfig.ReplyTag` 字段优先于 Profile.DefaultTag。零成本，建议带上。

### 7.3 Dashboard 显示

侧边栏 session 卡片右上角加 backend chip（小标签），用 `s.backend` 字段。Server-side `SessionView` 已经返回 `backend`，只是前端没渲染。

### 7.4 删全局 backendTag

删除 `server.go:54` 的 `backendTag` 字段、`server.go:423-427` 的 tag 推断、`new_options_test.go` 相关断言。`dashboard_session.go:1144` 的 `Backend: h.backendTag` 改成 `router.DefaultBackend()`（这是 stats 的"集群默认"，不是某 session 的实际 backend）。

---

## 8. Dashboard 区分 backend 的全套规约

> 这一节集中所有 dashboard 前端在多 backend 场景下需要做出的差异化处理。**每条都列出现有代码位置，明确"改" / "新增" / "保持"**，避免 PR 时漏掉某个面板。

### 8.1 设计原则

1. **单后端零变动**：当 `cli.backends` 未配置（legacy 单 backend 模式），dashboard 行为完全等同今天，picker / chip / 灰显逻辑全部隐藏。`fetchCLIBackends().backends.length <= 1` 是单后端判定的唯一来源。
2. **一切以 session 实际 backend 为准**：不要从 stats / config 推断"当前用户在用什么"，永远读 `session.backend`（已在 SessionView 返回，dashboard.js 当前未渲染）。
3. **能力差异 graceful degrade**：UI 控件按 `Caps` 灰显，不弹错误对话框。用户点了不可用按钮，给 tooltip 解释为什么；不要 silent ignore。
4. **数据源就近**：context% / cost / 模型名等 enrich 数据，kiro 直接给（`_kiro.dev/metadata`），claude 估算 — UI 直接用，不为统一做二次换算。

### 8.2 共享数据契约

后端在已有 `/api/cli/backends` 基础上扩展返回字段，dashboard 一次拉到所有差异决策需要的元信息。

```jsonc
// GET /api/cli/backends — 多后端模式返回示例
{
  "default": "claude",
  "backends": [
    {
      "id": "claude",
      "display_name": "claude-code",
      "version": "2.1.92",
      "available": true,
      "reply_tag": "cc",
      "caps": {
        "replay": true,
        "priority": true,
        "soft_interrupt": false,
        "stream_json": true
      },
      "features": {           // 用户级 feature toggles，不是协议 caps
        "askuser": true,      // AskUserQuestion 卡片
        "passthrough": true,  // /urgent + 多消息并发
        "embedded_context": true,  // @file mention
        "image_input": true,
        "audio_input": true,  // 经 Bedrock Transcribe
        "mcp_http": true,
        "mcp_sse": true
      },
      "models": null  // claude 走 cli.model，不动态枚举
    },
    {
      "id": "kiro",
      "display_name": "kiro",
      "version": "2.3.0",
      "available": true,
      "reply_tag": "kiro",
      "caps": {
        "replay": false,
        "priority": false,
        "soft_interrupt": true,
        "stream_json": false
      },
      "features": {
        "askuser": false,         // 待 V13 验证；保守默认 false
        "passthrough": false,     // 协议无 replay
        "embedded_context": false,// kiro acp 申报 false
        "image_input": true,
        "audio_input": false,     // kiro acp 申报 false
        "mcp_http": true,
        "mcp_sse": false
      },
      "models": [               // kiro 多模型，初始化时由 session/new 返回
        {"id": "auto", "name": "auto", "default": true},
        {"id": "claude-opus-4.7", "name": "claude-opus-4.7"},
        {"id": "claude-sonnet-4.6", "name": "claude-sonnet-4.6"},
        {"id": "deepseek-3.2", "name": "deepseek-3.2"},
        {"id": "minimax-m2.5", "name": "minimax-m2.5"},
        {"id": "glm-5", "name": "glm-5"},
        {"id": "qwen3-coder-next", "name": "qwen3-coder-next"}
      ],
      "commands": [             // kiro 的 _kiro.dev/commands/available 缓存
        {"name": "/agent", "description": "..."},
        {"name": "/clear", "description": "..."}
      ]
    }
  ]
}
```

`features` 是 dashboard 的**真理来源**。`caps` 给协议层用。`models` / `commands` 仅 kiro 类 backend 提供（探活时缓存）。前端按 `feat = backend.features[name] === true` 判断，**不要**在 dashboard.js 里写 `if backend === 'kiro'` 分支。

### 8.3 改动清单

| # | 区域 | 行为 | 现有代码 | 改/新 | 说明 |
|---|---|---|---|---|---|
| D1 | session 卡片右上角 | 显示 backend chip（小标签），紧邻 IM origin chip | `dashboard.js:1888-1942` `renderHeader` | 新增 | 用 `s.backend` 字段，色调跟随 reply_tag 哈希；hover tooltip 显示 backend display_name + version |
| D2 | 侧边栏 session 列表 | 每行尾追加 backend 微标（仅多后端模式） | `dashboard.js:215-280` | 新增 | 单字符或 2 字 abbrev，避免长 backend 名挤占布局 |
| D3 | new-session picker | 已实现 | `dashboard.js:4705-4727` `renderBackendPicker` | 保持 | 不可用 backend `disabled` 已支持 |
| D4 | new-session model 联动 | 切 backend 时，model 下拉重渲为该 backend 的 `backend.models[]` | 暂无（claude 单一 model） | 新增 | onchange listener 重建 model 选项；claude 选 backend 时显示 `cli.model` 单值 readonly |
| D5 | cost 列 | 当前 `effCLIName !== 'kiro'` 才显示 | `dashboard.js:1906-1909` | 改 | 改成读 `s.cost_unit`；kiro session 显示 `0.024 credits`，claude session 显示 `$0.0024`；都不为 0 时永不隐藏 |
| D6 | context 用量条 | 当前根据 token 估算 | 暂无统一组件 | 新增 | 读 `s.context_usage_percent`（kiro 直接给；claude 由 naozhi 估算并填同字段）；进度条 < 80% 绿，< 95% 黄，≥ 95% 红 |
| D7 | turn 计时器 | 当前 wall clock | 暂无 | enrich | kiro session 优先读 `_kiro.dev/metadata.turnDurationMs`，fallback wall clock |
| D8 | 模型显示 | 当前 stats 里的 `cli_name` + 版本 | `dashboard.js:1902-1904` | 改 | 多后端模式追加 `model_id`（kiro session 显示具体如 "deepseek-3.2"），单后端保持 cli_name |
| D9 | `/urgent` 按钮 | 当前永远显示 | 命令面板 | 改 | `feat.passthrough === false` 时 disabled + tooltip "kiro 不支持优先级抢占（请用 /cancel 后重发）" |
| D10 | `/cancel` 按钮 | 当前 SIGINT 路径 | 命令面板 | 不变（行为已正确） | 后端协议自动选 `session/cancel` notification 或 `control_request`；UI 只调 `/api/sessions/cancel` |
| D11 | 多消息并发 / 队列指示 | 当前 collect/passthrough/interrupt 都显示状态 | `dashboard.js` queue indicator | 改 | kiro session 强制 collect 模式，不显示 mode 切换器 |
| D12 | AskUserQuestion 卡片 | 飞书 + dashboard 双端 UI | `dashboard.js` askuser handler | 改 | `feat.askuser === false` 时不渲染卡片，回退到普通文本提问；event log 标注 "[此 backend 不支持 AskUserQuestion，已降级为文本]" |
| D13 | @文件 mention 输入 | claude 路径有 | 输入框补全 | 改 | `feat.embedded_context === false` 时禁用 @ 补全；显示 inline tooltip "kiro 请粘贴绝对路径" |
| D14 | 图片上传 | 现有 paste / upload 路径 | `dashboard.js` upload | 不变（两端都支持） | 仅在 `feat.image_input === false` 时禁用 |
| D15 | 音频上传 / 转写 | 现有路径 | upload + transcribe | 改 | `feat.audio_input === false` 时按钮 disabled，tooltip "kiro 不直接接受音频，会先转文字" — 实际 naozhi 后端已经把音频转文字再喂 prompt，所以这里仅显示提示，**不是真禁用** |
| D16 | slash 命令面板 | 当前固定 naozhi 自定义命令 | 命令面板 | 新增 | 多后端模式追加 backend 自带命令（kiro 的 `/agent /clear /compact /context /knowledge /mcp /usage` 等），分组标题 "Kiro 内置命令"；点击直接发到 prompt |
| D17 | tool_call 工具进度 | claude 走 tool_use ContentBlock | event 渲染 | 改 | kiro `tool_call` / `tool_call_update` 提供 status/rawInput/rawOutput，渲染 progress 行（pending → completed/failed）+ collapsible output；当前 `tool_call_chunk` 落入 default 分支被吞，要新增 case |
| D18 | subagent 列表 | naozhi 自身 agent-team UI | agent panel | 改 | 出现 `_kiro.dev/subagent/list_update` 时 namespace 隔离，标题加 "Kiro Subagents"；当前 V10 实测都是空数组，需要回归测试避免与 naozhi subagent 串扰 |
| D19 | History 翻页 | claudejsonl + naozhilog 已支持 | `dashboard.js:loadEarlier` | 不变 | 后端 historyFactory 已抽象，前端无感知 |
| D20 | session backend 切换（已起 session）| 当前没这功能 | — | 不做 | 物理不可能；接口层面也不暴露，避免误用 |
| D21 | new-session "favorite backend" 记忆 | 当前记 last agent | localStorage | 新增 | 记 last picked backend（key: `naozhi.lastBackend`），下次打开 picker 默认选这个 |
| D22 | doctor / 状态页 | 现有 `/health`、stats footer | server-side | 新增 | stats footer 当多后端时显示每个 backend 的 status icon（available / version / spawn count），点开显示 V11 cap 表 |
| D23 | reverse-node + backend 兼容性提示 | new-session 选 node + backend 组合 | new-session 模态 | 新增 | 用户选择 node X + backend kiro，前端校验 `node.caps` 含 `acp`；不含时 disabled + tooltip "node X 未声明支持 ACP，无法承载 kiro session" |
| D24 | error 提示文案 | 现有 `errors_usermsg.go` | server + client | 改 | 多后端模式下错误消息追加 backend ID 前缀（`[kiro] session/load failed: ...`），运维定位故障更快 |
| D25 | 通知 IM tag | 现有 reply_tag global | dispatch | 配套 §5 | dashboard 发的消息回到 IM 时尾巴 `[cc]` / `[kiro]`，与 dashboard 卡片 chip 颜色一致 |

### 8.4 颜色与无障碍

- **chip 颜色**：claude → 既有 `--nz-accent`（紫）；kiro → 新加 `--nz-accent-kiro`（橙）；新 backend 按 `id.charCodeAt(0) % palette.length` 自动取色，但允许 `cli.backends[].chip_color` 覆盖。
- **对比度**：所有 chip 文字背景对比度 ≥ 4.5:1（WCAG AA）。开发期跑一次 axe-core 校验。
- **chip 文本**：默认 backend `display_name`，若超过 8 字符截断为 `id`，再超截断到 6 字符 + `…`，hover 显示完整。
- **dark mode**：单独覆盖 chip 背景透明度，避免与已有 IM origin chip 撞色。

### 8.5 状态机：何时刷新 backend 元信息

```
启动 → fetchCLIBackends() → 缓存 60s
  ├─ 模态打开 → 命中缓存 → 直接渲染 picker
  ├─ 缓存过期 → 后台刷新（不阻塞 UI）
  └─ "刷新 backend" 按钮（仅 doctor 页）→ 强制刷新

session 创建后 → 用户切了 model（kiro 内）→ POST /api/sessions/<key>/model → 不影响 backends 缓存
session 切 backend → 不允许，UI 层就拒绝
naozhi 重启 → fetchCLIBackends 自动失败 → fallback 到上次缓存（standby 模式横幅）
```

### 8.6 测试矩阵

| 场景 | 期望行为 |
|---|---|
| 单后端（仅 claude）| dashboard 行为完全等同今天，无 picker、无 chip、无能力灰显 |
| 双后端 + 用户选 claude | UI 全功能可用，cost 显示美元 |
| 双后端 + 用户选 kiro | `/urgent` 灰显、cost 显示 credits、context 用真值、模型切换可用 |
| kiro session 上 `/cancel` | 不杀进程；turn 优雅结束；turn-complete 事件正常 |
| kiro session 上发 5 条快速消息 | 队列指示器显示 "collect mode (kiro 不支持 passthrough)" |
| node X 不支持 acp + 用户选 kiro 派 X | new-session 不允许提交，显示前置错误 |
| 多后端 + AskUserQuestion 触发 | claude session 显示卡片；kiro session 显示文本 fallback |
| 旧 store 里 backend 字段空 | wrapperFor fallback 到 default，UI 显示 default backend chip |
| backends.length===1 但是 kiro | picker 隐藏（length<=1 规则），但 chip / cost 单位 / cap 灰显仍生效 |

### 8.7 反例（**不要这么做**）

- ❌ `if (s.backend === 'kiro') hideUrgent()` — 用 `feat.passthrough` 判定
- ❌ 把 cost 单位写死成 `$` — 用 `s.cost_unit` 字段
- ❌ 一次拉所有 backend 的 model 列表常驻内存 — 仅在打开 picker 时按需查
- ❌ 用 backend.id 做 CSS class 名 — id 来自 config 可能含特殊字符；用 `data-backend` 属性 + prefix 选择器
- ❌ chip 用 emoji（如 🟪 / 🟧）— 屏幕阅读器读不出有意义文本，破坏无障碍

### 8.8 服务端 normalize 层

为支撑 §8.3 的 D5/D6/D7/D26（不让 dashboard.js 直接解析 `_kiro.dev/*` 私有 method），server 侧 SessionView 增加 4 个 normalize 字段：

```go
type SessionView struct {
    ...
    Backend             string  `json:"backend"`              // 已有
    CostUnit            string  `json:"cost_unit"`            // 新增："USD" | "credits"
    ContextUsagePercent float64 `json:"context_usage_percent"`// 新增：0-100
    TurnDurationMs      int64   `json:"turn_duration_ms"`     // 新增：上一轮耗时
    MeteringUsage       []struct {                            // 新增：kiro 计费明细
        Value float64 `json:"value"`
        Unit  string  `json:"unit"`
    } `json:"metering_usage,omitempty"`
}
```

填充规则：
- `claude` session：`CostUnit="USD"`，ContextUsagePercent 由 token estimate 算，TurnDurationMs 由 wall clock，MeteringUsage 留空
- `kiro` session：`CostUnit="credits"`，三个字段从 `_kiro.dev/metadata` 通知里读

`ACPProtocol.ReadEvent` 把 `_kiro.dev/metadata` 私有 method 提级为 `Event{Type:"metadata", ...}`，`ManagedSession` 的 setter 接收并 update 字段。**dashboard.js 永远只消费 normalize 字段**。

### 8.9 验收（GA blocker）

1. D1 / D5 / D6 三项 + §7 ReplyTag：双后端模式下 chip / cost 单位 / context 用量条 三处一致使用 session 真实 backend
2. D9 / D11 / D12 / D13：所有按 `feat.*` 灰显的控件，hover 都有解释 tooltip，**不允许 silent disabled**
3. D17 tool_call 渲染：kiro 上跑一个 `cat /etc/hostname` shell 工具，progress 行出现并 collapse 显示完整 stdout
4. D23：在双节点（一支持一不支持 acp）+ 双后端配置下，前端能拒绝错误组合
5. axe-core 跑过：所有 chip / disabled 控件无 ARIA / 对比度 violation
6. 服务端 normalize 字段（CostUnit/ContextUsagePercent/TurnDurationMs/MeteringUsage）单测覆盖 claude/kiro 两路填充

---

## 9. Cron job backend 字段

### 9.1 现状

`internal/cron/job.go` 的 Job 结构体没有 backend 字段。Cron 触发时通过 router default 决定 backend；多后端用户必然问"为什么 cron 不能选 backend"。

### 9.2 设计

```go
// internal/cron/job.go
type Job struct {
    ...
    Backend string `json:"backend,omitempty"`  // 空 = router default
}
```

调度时 `cron.Scheduler.runJob` 把 `j.Backend` 透传到 `AgentOpts.Backend`，`spawnSession` 走现有 backend 路由路径。

### 9.3 Dashboard

Cron 编辑器加 backend 下拉（与 new-session picker 同款），用 `/api/cli/backends`。后端 `dashboard_cron.go` 的 create / update job 接受 `backend` 字段并 passthrough 到 Scheduler。

### 9.4 兼容性

旧 cron 持久化（`~/.naozhi/cron_jobs.json`）没 backend 字段，反序列化得 `""`，运行时 fallback 到 default。零迁移成本。

---

## 10. Metrics backend label

### 10.1 现状

`internal/metrics/metrics.go` 的 `CLISpawnTotal` 等是全局计数器，无 backend 维度。多后端启用后，`naozhi_cli_spawn_total = 100` 看不出哪 100 是 claude 哪些是 kiro。

### 10.2 设计

将以下 metric 升级为 `expvar.Map` 或迁移到 prometheus CounterVec（取决于现有 metric 框架）：

| Metric | 现状 | 多后端后 |
|---|---|---|
| `naozhi_cli_spawn_total` | int | label: `backend` |
| `naozhi_session_active` | int | label: `backend` |
| `naozhi_protocol_rpc_error_total` | 不存在 | 新增 label: `backend, method, code` |
| `naozhi_acp_cancel_total` | 不存在 | 新增 label: `backend` |
| `naozhi_attachment_ref_*_total` | int | 不变（attachment 与 backend 无关） |

### 10.3 兼容性

如果 dashboards/alerts 已经在用旧 metric 名查询，**双写一个迁移期**（4 周）：保留旧 int + 新 vector。`docs/ops/pprof.md` 同步更新 jq 模板。

---

## 11. Config Validate + doctor

### 11.1 Validate

`config.Load` 返回前调 `Validate()`，输出诊断列表（warn 级别，不阻断启动）：

```go
func (c *Config) Validate() []ValidationDiag {
    var diags []ValidationDiag
    backends := c.EnabledBackends()
    for _, b := range backends {
        if _, ok := backend.Get(b.ID); !ok {
            diags = append(diags, ValidationDiag{
                Level: "error",
                Field: "cli.backends[" + b.ID + "]",
                Msg:   "unknown backend id; will be skipped at runtime",
                Hint:  "valid ids: " + strings.Join(backendIDs(), ", "),
            })
        }
    }
    return diags
}

type ValidationDiag struct {
    Level string  // "warn" | "error"
    Field string
    Msg   string
    Hint  string
}
```

启动时打印 diags，error 级别立即退出（避免 systemd Restart=on-failure 死循环 — 通过显式 exit code 0x42 信号 unit 文件设 `RestartPreventExitStatus=66`）。

### 11.2 doctor

`naozhi --doctor` 在现有 effective config dump 之外加一段：

```
=== CLI Backends ===
Default: claude

[claude] claude-code
  path:    /home/user/.local/bin/claude
  version: 2.1.92
  proto:   stream-json
  caps:    Replay=true Priority=true SoftInterrupt=false
  history: ~/.claude/projects/...

[kiro] kiro
  path:    /home/user/.local/bin/kiro-cli
  version: 2.3.0
  proto:   acp
  caps:    Replay=false Priority=false SoftInterrupt=true
  history: ~/.kiro/sessions/cli/

=== Reverse Nodes ===
node "macbook" caps: [acp, askuser]
  → can host: claude, kiro
node "android" caps: []
  → can host: claude (kiro requires "acp" cap)
```

---

## 12. Sprint 计划

| Sprint | 内容 | LOC（impl） | LOC（test） | 工时 |
|---|---|---|---|---|
| **0a** | hotfix RPCMessage.ID + permission optionId | 80 | 250 | 0.5 |
| 0b | `internal/cli/backend/` 包 + RegisterDefaults | 200 | 200 | 1 |
| 1a | `cli.Wrapper.NewHistorySource` 工厂化 + router.attachHistorySource 重写 | 150 | 200 | 1 |
| 1b | 收敛散点（detect / proc / wrapper / main） | 200 | 150 | 1 |
| 1c | `internal/history/kirojsonl/` | 300 | 200 | 1.5 |
| 2 | per-session ReplyTag | 80 | 100 | 1 |
| 3 | ACP `session/cancel` notification | 60 | 200 | 0.5 |
| 4 | 服务端 normalize 层（§8.8 — `_kiro.dev/metadata` → SessionView 字段） | 150 | 200 | 1 |
| **5a** | **Dashboard chip / cost unit / context bar（§8 D1/D2/D5/D6/D7/D8/D26）** | **180** | **150** | **1.5** |
| **5b** | **Dashboard 能力灰显（§8 D4/D9/D11/D12/D13/D14/D15/D21/D24）** | **160** | **150** | **1** |
| **5c** | **Dashboard tool_call 渲染 + slash 命令 + subagent 隔离（§8 D16/D17/D18）** | **200** | **150** | **1.5** |
| **5d** | **Dashboard reverse-node + backend 兼容性校验（§8 D23）+ doctor 状态页 (§8 D22)** | **120** | **100** | **1** |
| 6a | metrics backend label | 150 | 100 | 0.5 |
| 6b | reverse-node capability 路由 | 250 | 250 | 1 |
| 6c | cron job backend 字段 | 100 | 100 | 0.5 |
| 6d | Validate + doctor + 文档 | 150 + 文档 | 50 | 1 |
| **合计** | | ~2530 | ~2550 | **14.5 人/天** |

每个 Sprint 独立可上线、独立可回滚。Sprint 0a 是 hotfix，应当作为前置 PR 单独提。**Dashboard 四个 Sprint（5a/5b/5c/5d）可并行交给前端工程师**，最长链路落在 5c。建议：

1. Sprint 0a / 0b / 1a 上线 — 切到 kiro 不崩，但用户体验仍 Patchy
2. Sprint 1b / 1c / 2 / 3 / 4 上线 — kiro session 完整可用（历史 / cancel / tag / normalize），但 dashboard 还没区分
3. Sprint 5a-d 上线 — dashboard 完整 backend 区分
4. Sprint 6a-d 上线 — 运维 + 反向节点 + cron 完整支持

**Dashboard feature flag**：5a-d 期间用 `localStorage.naozhi.dashboardMultiBackend = "true"` 灰度，问题时一键回退到单 backend UI。

---

## 13. 风险与回滚

| 风险 | 触发场景 | 缓解 |
|---|---|---|
| RPCMessage.ID 改类型破坏旧测试 | 大量 `*int` 解引用 | 单独 hotfix PR + 一次性 grep 全仓改完 |
| Profile 注册顺序冲突 | 重复 ID | `Register` panic-on-dup |
| Wrapper 共享 ShimManager 不够 | kiro 想要更长 idle_timeout | v1 共享，schema 预留 `cli.backends[].shim` 字段，未来无 breaking |
| Dashboard 老 session 无 backend 字段 | Store 旧记录 `Backend == ""` | wrapperFor 已有 fallback；attachHistorySource fallback 到 default |
| Reverse-node cap 新路由破坏旧路径 | 旧子节点不申报 caps | 默认 caps 空 = 仅 claude 兼容；kiro 必须升级子节点版本才能承接 |
| Kiro stale lock 极端情况 | PID reuse — 同 PID 被另一无关进程占了 | 已实证 kiro 自己处理；naozhi 不参与 |

**回滚**：每 Sprint 是独立 PR，且 Sprint 0/1 行为零变动；如 Sprint 3（cancel）出问题，单独 revert，多后端基础设施仍可用。

---

## 14. 决策点（待 owner 确认）

1. **Sprint 0a hotfix PR 的 release timing**：现在就修（hotfix 单 PR），还是合并进多后端 epic？
   - 建议：现在就修。两个 bug 在 master 已经存在，与切换正交。
2. **ShimManager per-backend**：v1 共享 v2 拆分，还是 v1 就预留 schema？
   - 建议：v1 schema 预留 `cli.backends[].shim`，实现仍共享。30 分钟工作，避免未来 breaking。
3. **Metrics 双写期**：直接破坏老 dashboard 还是双写 4 周再删？
   - 建议：双写 2 周 + 一篇运维 changelog 通知。
4. **kiro_sessions_dir config 字段名**：`session.kiro_sessions_dir` vs `cli.backends[kiro].sessions_dir`？
   - 建议：放 `cli.backends[].sessions_dir` 通用字段（gemini-cli 未来也可能有自己的目录），默认 `~/.kiro/sessions/cli`。

---

## 15. 验收标准（GA）

1. **单后端兼容**：仅 claude 配置启动，所有现有功能行为完全不变（回归测试 100% 绿）
2. **多后端共存**：claude + kiro 同时启用，Dashboard new-session 选择器能两个 backend 都起 session 并正常对话
3. **Per-backend 历史**：kiro session 重启 naozhi 后历史完整可见（>10 turn 测试）
4. **Per-backend tag**：claude session 回复尾 `[cc]`，kiro session 回复尾 `[kiro]`
5. **Cancel**：kiro session `/cancel` 不杀进程，turn 优雅结束，立即可发下一条
6. **Reverse-node**：双节点（一支持 acp 一不支持）+ 主节点配双 backend，kiro 请求只派到支持的节点
7. **Cron**：cron job 编辑器能选 backend，kiro cron 任务正常执行
8. **Metrics**：dashboard / pprof 能按 backend label 区分指标
9. **Doctor**：`--doctor` 输出每个 backend 的完整状态
10. **Dashboard §8.9 全过**：chip / cost unit / context bar / 灰显 / tool_call 渲染 / reverse 校验 / axe-core / normalize 字段单测
11. **`go test -race -count=1 ./...` 全绿**

---

## 16. 验证依据（实测原始数据）

详见 `docs/rfc/multi-backend-validation.md`：12 项 PoC 脚本（probe1_handshake.sh ~ probe10_startup.py）+ 输出快照 + bug 复现脚本（naozhi_id_check.go）。本 RFC 任何"实证 / 实测"标注均指向该文件相应章节。
