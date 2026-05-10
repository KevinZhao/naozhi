# RFC: 并行 Agent/Team 可视化 + 内部过程查看

Status: Ready for implementation
Date: 2026-05-08 (v3 — second-pass review)
Author: naozhi dashboard
Scope: internal/cli + internal/session + internal/server + static/dashboard.{html,js}

## 1. 背景

Claude CLI 2.1.132 引入的并行 team 能力在 Dashboard 上只显示一句"输出中..."，
看不到每个 agent 是谁、在干什么、干到哪一步。用户要的是：

> 并行的 agent teams 工作的时候，dashboard 能同时显示多个 agent 的状态，
> 并且点击某一个 agent 能看到它的实际工作过程和状态。

### 1.1 现状代码链路（为什么只剩"输出中..."）

1. `internal/cli/process.go:1758-1769` 的 `case "Agent"` 把 `Agent` tool_use
   转成 `type=agent` 的 `EventEntry`，`TeamName + Subagent(=name) + ToolUseID`
   都正确落进 `EventLog`。
2. `internal/server/static/dashboard.js:2942-2963` 的 `case 'agent'` 正常
   把 agent push 进 `turnState.agents`；`task_started/task_progress/task_done`
   通过 `findAgentByToolUseId/TaskId` 更新状态。
3. **真正的问题**有两层：
   - **事件缺失**：team 成员在父流只出 `task_started` 和 `task_notification`，
     **没有 `task_progress` / `last_tool_name` / `usage`**（见 §1.2），所以
     agent row 的 "N calls · 2.1s" 永远是 0。
   - **UI 密度**：agent row 在 banner 里只占一行小字，父 session 的 text
     block 会把 line-1 刷成"输出中..."盖过注意力；三行 agent 看起来像没动。
4. **完全缺失的维度**：成员内部的 Read/Bash/thinking/tool_result **完全不向父
   流溢出**。

### 1.2 探针实录（team 3 agent 并行）

`/tmp/naozhi-probe` 实录一段 "TeamCreate + 3 Agent 并行 ls /tmp" 的
stream-json 流（76 行）+ 观察落盘。

**父流事件**：system.init×2 / system.task_started×7（3 Agent + 4 local_bash）
/ system.task_notification×7 / assistant×38 / user×20 / result×2。

**父流 team 相关 tool_use**：`TeamCreate` / `Agent ×3` / `SendMessage ×3` /
`TeamDelete ×5` / `Bash ×4` / `ToolSearch ×2`。

**成员在父流中的信息**（lister-1）：
```
assistant  tool_use:Agent  input={name:"lister-1", team_name:"file-listers",
                                   description:"List /tmp files (agent 1)", prompt:"..."}
           id=toolu_bdrk_01GkVR5vJY...
user       tool_result    "Spawned successfully.\nagent_id: lister-1@file-listers..."
system     task_started   task_id=t562ubj97 tool_use_id=toolu_bdrk_01GkVR5v...
                          task_type="in_process_teammate"
                          description="lister-1: You are..."
...  ← 无 task_progress / last_tool_name ...
system     task_notification  task_id=t562ubj97 status=completed
                              summary="lister-1@file-listers"
```

### 1.3 Transcript 落盘结构

```
~/.claude/projects/<encoded-cwd>/<session_uuid>/
├── <session_uuid>.jsonl                父 session（不含 sidechain）
├── tool-results/<task_id>.txt          Bash 超大输出持久化
└── subagents/
    ├── agent-<hex17>.jsonl             成员完整内部会话
    └── agent-<hex17>.meta.json         {"agentType":"lister-1"}
```

**`<encoded-cwd>` 编码规则（R7 修正）**：Claude CLI 把 cwd 里每个字符
**`[^A-Za-z0-9]`** 都替换为 `-`，**不 collapse** 连续 `-`。探针验证：

| 真实 cwd | 编码结果 |
|---|---|
| `/home/ec2-user/.claude` | `-home-ec2-user--claude` |
| `/home/ec2-user/workspace/naozhi` | `-home-ec2-user-workspace-naozhi` |
| `/tmp/a--b` | `-tmp-a--b` |
| `/tmp/a_b.c` | `-tmp-a-b-c` |
| `/tmp/spa ce` | `-tmp-spa-ce` |

**注意编码有损**：`/tmp/a-b`、`/tmp/a.b`、`/tmp/a_b` 全部映射到 `-tmp-a-b`。
RFC 用 session_uuid 子目录防串扰 + 在 Linker 里**二次校验 jsonl 首行
`sessionId` 字段 == linker.parentSessionID**，保证即使 projectDir 撞车也
不会读错 agent。

每行 jsonl schema：
```json
{"parentUuid":"...","isSidechain":true,"agentId":"<hex17>","promptId":"...",
 "type":"assistant|user|system|attachment",
 "message":{"role":"...","content":[...]},
 "uuid":"...","timestamp":"2026-05-08T12:14:40.920Z",
 "sessionId":"<parent_session_uuid>"}
```

### 1.4 ⚠️ team 成员 CLI 为每个 agent 写**两份** jsonl

**只在 `task_type=in_process_teammate`（team 成员）场景出现**。独立
sidechain（subagent_type=Explore / run_in_background）只写一份 jsonl（见
§1.7 A2）。

R1 探针：3 个 team 成员的 `subagents/` 目录有 **6 份** `agent-<hex>.jsonl`
+ 6 份 `.meta.json`。每个 agent 两份文件的差异：

| 维度 | "干净版"（A） | "完整版"（B） |
|---|---|---|
| 首行 promptId | 父 user prompt id（全 A 共享） | CLI 内部 id（全 B 共享） |
| 首次 attachment 时机 | 紧贴首行 (+2ms) | 延后 |
| system.api_error | 不记录 | 记录 |
| shutdown 交互 | 截到"报告完成" | 含后续 SendMessage |
| 行数 / size | 少 / 小 | 多 / 大（B ≥ A 恒成立） |
| mtime | 早（先停写） | 晚（持续追加） |
| `meta.json.agentType` | 相同 | 相同 |
| 跨文件节点 `uuid` | 一一对应 |

**选哪个**：**始终用完整版 B**。理由：
1. 含 api_error / shutdown 全部事件，符合"实际工作过程"。
2. mtime 追加到 agent 真正退出；tail 时机更准。
3. `size(B) ≥ size(A)` 恒成立，用 `(mtime, size)` 降序 selector 稳定。

### 1.5 字段连结可用材料

| 父流字段 | 磁盘字段 | 关系 |
|---|---|---|
| `Agent.input.name` | `meta.json.agentType` | 同一轮 team 内 name 唯一 |
| `Agent.input.description` | jsonl 首行 `<teammate-message summary=>` | 冗余校验 |
| `system.task_started.tool_use_id` | — | **无磁盘映射** |
| `system.task_started.task_id` | — | **无磁盘映射** |
| `parentSessionID`（linker 注入） | jsonl 每行 `sessionId` | **二次安全校验** |

**唯一可靠路径**：`task_id` →（同时刻父流 `Agent` tool_use 的）`name` →
`meta.json.agentType` → `(mtime desc, size desc)` 首位 `agent-<hex>.jsonl`
→ 首行读 `sessionId` 二次校验。

### 1.6 嵌套 Agent（未强制验证）

`/tmp/naozhi-probe2` 探针：要求 Agent 起二级 Agent，实际模型退化成
`Skill(skill=Agent)` 伪调用，**未能强制真正的二级 sidechain**。
subagents/ 只出现一份一级 jsonl。

**结论**：本 RFC **假设**未来真嵌套场景下，二级 jsonl 仍落在同一
`subagents/` 目录（meta.json 的 agentType + 父级 jsonl 的 parentUuid 足以
区分层级）。这是 §1.7 列出的待验证假设之一，不阻塞本轮实施（本 RFC 对
嵌套 Agent 只退化显示，不 drill-in）。

### 1.7 待验证假设清单（Phase 0 已验 A2/A3/A7；其余留 Phase 4）

| # | 假设 | Phase 0 结论 | 影响 |
|---|---|---|---|
| **A1** | 真嵌套 Agent 时二级 jsonl 仍落同目录 | 未验证（模型退化成 Skill 伪调用） | 本 RFC 不做 drill-in；若未来落别处，§3.4.1 退化为普通 tool_use 仍安全 |
| **A2** | `run_in_background=true` 的 agent 是否落盘 | ✅ **已验**：落 `subagents/agent-<hex>.jsonl` + meta.json；但**只一份**（无双版本） meta.json 多 `description` + `name` 字段 | Linker selector 的单候选路径已覆盖；meta.json 的 description 字段可供冗余校验 |
| **A3** | Shim 重连后父 session_uuid 稳定 | **有条件成立**：正常 reconnect（CLI 活、走 SpawnReconnect）→ session_id 从 shim state 透传、稳定；CLI 死后 respawn → **可能变**（router 注释 "sometimes losing resume context"） | Linker 重建策略：历史 EventEntry 持久化的 `InternalAgentID` 优先于重跑 Resolve（Phase 4 InjectHistory 路径） |
| **A4** | ACP (Gemini) 协议的 transcript 结构 | 未验证（纯 Claude 项目） | Phase 4 前按 `ProtocolName() != "stream-json"` 直接 tombstone，UI toast |
| **A5** | CLI transcript 清理触发条件 | 未验证 | 砍 410 状态码，全用 404；按"文件不存在"统一处理 |
| **A6** | parentUuid 成链无环 | 未验证 | Phase 4 若做时间轴回放再验；本 RFC 不依赖 |
| **A7** | tool-results 文件名和父流 task_id 的关系 | ✅ **已验**：**完全独立**命名空间（8-12 字符 base36）；agent 内部 jsonl **不出 `system.task_started`**，文件名是 CLI 对大输出生成的独立 id；`<persisted-output>` 文本里给**绝对路径** | endpoint 路径校验按 basename 正则 + 目录约束，不要求映射到任何 task_id |
| **A8** | 同 session 重复 spawn 同 name | 未验证 | §2.2 声明不支持；代码 warn log 监测 |

**Phase 0 新增确认**：
1. **双 jsonl 是 team-only**：只在 `task_type=in_process_teammate` 场景出现；
   独立 sidechain（subagent_type / run_in_background）只一份。**Linker 算法不变**
   （单/双候选同一套 selector），但 §1.4 文字需更精确。
2. **CLI-dead 后 session_uuid 可能变**：Phase 4 的 InjectHistory 路径必须
   直接用历史 EventEntry 里持久化的 `InternalAgentID` 重建 Linker，不要
   在新 session_uuid 下重跑 Resolve（会扫到空目录 → tombstone，丢失所有历史）。
3. **tool-results endpoint 简化**：校验 `^[a-z0-9]{1,32}\.(txt|json|log)$` +
   在 `<projectDir>/<parentSessionID>/tool-results/` 内，对 `<persisted-output>`
   正则抓绝对路径后取 basename 使用。

## 2. 目标与范围

### 2.1 目标

- 并行 team 每个 agent banner 独立成行，显示名字/状态/当前工具/耗时。
- 点击 agent row → 主事件区切换到该 agent 的内部工作流。
- 面包屑返回父 session（Esc 等价）。
- team 运行时实时增量更新（WS 推送）。
- 缺失磁盘 transcript 时 UI 降级不白屏。
- 完成 agent 仍可点击回看。

### 2.2 非目标

- TeamCreate / SendMessage / TeamDelete / TaskOutput / ScheduleWakeup /
  CronCreate 等新工具的专门 UI（走通用 tool_use 渲染）。
- Agent 级 cost 聚合（磁盘 usage 字段需全文扫描）。
- **嵌套 Agent 内部 drill-in**（agent 视图里的 Agent tool_use 退化为
  普通 tool_use，不再下钻）。
- **同 team 同 name agent**（违反 CLI 惯例）。
- **同 session 再次 spawn 同名 agent**（历史那份无法通过 task_id 找回）。
- 灰度 feature flag（用户明确要求直接上线）。
- ACP 协议下的 agent 内部查看（§1.7 A4 验证前降级到"无内部记录" toast）。

## 3. 设计

### 3.1 数据流总览

```
父 stream-json
  ├─ assistant tool_use=Agent            → EventLog "agent"
  ├─ system.task_started (teammate)      → EventLog "task_start"
  │                                        + async Linker.Resolve → SetAgentInternalID
  │                                        + on-resolve callback → silent tailer 启动
  └─ system.task_notification            → EventLog "task_done"

磁盘 subagents/agent-<hex>.jsonl (完整版)
  └─ TranscriptReader → EventEntry[]
     → HTTP /api/sessions/agent_events
     → WS {type:"agent_event", ...}

前端
  turnState.agents[i].taskId ↔ server Linker
  点击 agent row → activeAgentView=taskId → 主事件区切换
  WS agent_subscribe → 增量插入
```

### 3.2 EventEntry / SubagentInfo 字段扩展

`internal/cli/eventlog.go` EventEntry 新增 **2** 字段（SubagentSource 砍
掉，视图切换是排他的）：

```go
type EventEntry struct {
    ...existing...
    TaskType        string `json:"task_type,omitempty"`          // "in_process_teammate" | "local_bash" | ""
    InternalAgentID string `json:"internal_agent_id,omitempty"`  // "<hex17>"；异步回填
}
```

**兼容性**：TaskType 仅出现于 system subtype 派生 entry，omitempty，历史
`sessions/*.jsonl` 新旧混读无冲突；旧 naozhi 回放新文件时字段被静默忽略。

`cli.SubagentInfo` 扩展：

```go
type SubagentInfo struct {
    Name            string `json:"name"`
    Activity        string `json:"activity,omitempty"`
    Background      bool   `json:"background,omitempty"`
    // 新增：
    TaskID          string `json:"task_id,omitempty"`
    ToolUseID       string `json:"tool_use_id,omitempty"`
    TaskType        string `json:"task_type,omitempty"`
    InternalAgentID string `json:"internal_agent_id,omitempty"`
    Status          string `json:"status,omitempty"`     // "" | "spawned" | "running" | "completed" | "error"
    StartedAtMS     int64  `json:"started_at_ms,omitempty"`
    // Aggregator 注入（§3.5.4）：
    LastTool        string `json:"last_tool,omitempty"`
    LastDetail      string `json:"last_detail,omitempty"`
    ToolUses        int    `json:"tool_uses,omitempty"`
    DurationMS      int    `json:"duration_ms,omitempty"`
}
```

`applyEntryStateLocked` 扩展（R11/R12 修正）：

```go
case "agent":
    info := SubagentInfo{
        Name:       labelOrTeam(e),
        Activity:   e.Summary,
        Background: e.Background,
        ToolUseID:  e.ToolUseID,       // 新
        TaskType:   e.TaskType,        // 新（agent 事件暂无；预留）
        Status:     "spawned",
    }
    if e.Background { l.bgAgents = append(l.bgAgents, info) }
    else { l.turnAgents = append(l.turnAgents, info) }

case "task_start":
    for i := range l.turnAgents {
        if l.turnAgents[i].ToolUseID == e.ToolUseID {
            l.turnAgents[i].TaskID = e.TaskID
            l.turnAgents[i].Status = "running"
            l.turnAgents[i].StartedAtMS = e.Time
            // 注意：NOT set InternalAgentID 或 TaskType 在这里 —— Resolve
            // 是异步的，此时 e.InternalAgentID 为空串，一旦写就会覆盖
            // 后到的 SetAgentInternalID。保持空，由异步回填专写。
            break
        }
    }

case "task_done":
    for i := range l.turnAgents {
        if l.turnAgents[i].TaskID == e.TaskID {
            l.turnAgents[i].Status = nonEmpty(e.Status, "completed")
            if e.DurationMS > 0 {
                l.turnAgents[i].DurationMS = e.DurationMS  // 父流权威值
            }
            break
        }
    }
```

新方法 `EventLog.SetAgentInternalID(toolUseID, id string)` 在 l.mu 下按
ToolUseID 反查写 InternalAgentID；`UpdateAgentMeta(toolUseID, patch SubagentMetaPatch)`
批量写 LastTool/ToolUses/... 见 §3.5.4。

### 3.3 SubagentLinker

新模块 `internal/cli/subagent_link.go`。

```go
type SubagentLinker struct {
    mu              sync.RWMutex
    byTaskID        map[string]LinkInfo
    byToolUseID     map[string]LinkInfo
    resolvedTaskID  map[string]struct{}   // tombstone：已试过解（成功或失败）的 task_id
    projectDir      string                // ""=未就绪
    parentSessionID string                // ""=未就绪（init 前）

    dirCache        struct {               // §3.3.3 TTL 缓存
        at      time.Time
        entries []metaEntry
    }

    onResolve       func(taskID, toolUseID, internalAgentID string) // §3.5.4 tailer 挂载点
}

type LinkInfo struct {
    InternalAgentID string // ""=已解但无磁盘记录（tombstone）
    JSONLPath       string
    Name            string
    Resolved        bool   // 区分 "未解" vs "已解=空"
}
```

#### 3.3.1 Resolve 算法（7 步，R10 修正）

```
Input: taskID, toolUseID, name, description, agentToolUseWallclockMS
Pre:   l.projectDir != "" && l.parentSessionID != ""

# Guard: 已解过就直接返回
1. l.mu.RLock(); if info, ok := l.byTaskID[taskID]; ok { return info.InternalAgentID }

# 扫目录
2. subagentDir = <projectDir>/<parentSessionID>/subagents/
   entries := scanMetaFiles(subagentDir)   # 500 ms TTL 缓存，§3.3.3
3. filter: agentType == name

# 候选为空 → 等 CLI 写盘
4. if len(candidates) == 0:
     retry after 250 ms, up to 12 次 (3 s 宽限) → goto 2
     超时 → mark tombstone { Resolved=true, InternalAgentID="" }; fire onResolve(""); return ""

# 对每候选做 stat + 首行校验
5. for each (hex, metaPath):
     jsonlPath := subagentDir + "agent-" + hex + ".jsonl"
     st, err := os.Stat(jsonlPath)
     - 文件不存在 or size == 0: skip
     - st.ModTime() < now - 10s: skip  // 陈旧候选（同 name 二次复用）
     firstLine := readFirstLine(jsonlPath, maxBytes=32KB)
     - firstLine.sessionId != l.parentSessionID: skip  // R7 跨 projectDir 防串扰
     - parseTime(firstLine.timestamp) < agentToolUseWallclockMS - 5s: skip
       // R10：agent 启动时刻应接近父流 Agent tool_use 时刻（实测差 <200ms）
     OK → 加入 filtered

# 取首位
6. if len(filtered) == 0 → tombstone → return ""
   sort filtered by (ModTime desc, Size desc) → take [0]

# 写缓存
7. l.mu.Lock()
   info := LinkInfo{InternalAgentID: hex, JSONLPath: jsonlPath, Name: name, Resolved: true}
   l.byTaskID[taskID] = info
   l.byToolUseID[toolUseID] = info
   l.mu.Unlock()
   fire onResolve(taskID, toolUseID, hex)
   return hex
```

#### 3.3.2 projectDir 推导（R7 修正）

```go
// resolveProjectDir mirrors Claude CLI's encoding:
// replace every non-[A-Za-z0-9] character with '-', no collapse.
func resolveProjectDir(cwd string) string {
    if cwd == "" {
        return ""
    }
    claudeRoot := filepath.Join(os.Getenv("HOME"), ".claude", "projects")
    var b strings.Builder
    b.Grow(len(cwd))
    for _, r := range cwd {
        switch {
        case r >= 'a' && r <= 'z':
            b.WriteRune(r)
        case r >= 'A' && r <= 'Z':
            b.WriteRune(r)
        case r >= '0' && r <= '9':
            b.WriteRune(r)
        default:
            b.WriteByte('-')
        }
    }
    return filepath.Join(claudeRoot, b.String())
}
```

**编码有损**：`/tmp/a-b` / `/tmp/a.b` / `/tmp/a_b` → `-tmp-a-b` 同一目录。
**防御**：Resolve 必须读 jsonl 首行 `sessionId` 校验等于 parentSessionID
（§3.3.1 step 5），否则丢弃该候选。cwd 冲突下 session_uuid 撞车概率 ≈ 0。

#### 3.3.3 dirCache TTL（R9 修正）

```go
func (l *SubagentLinker) scanMetaFiles(dir string) []metaEntry {
    l.mu.Lock()
    defer l.mu.Unlock()
    if time.Since(l.dirCache.at) < 500*time.Millisecond {
        return l.dirCache.entries
    }
    entries := rawScan(dir)   // os.ReadDir + parse each .meta.json
    l.dirCache.at = time.Now()
    l.dirCache.entries = entries
    return entries
}
```

3 个 task_started 250 ms 内连发时共享一次扫描。TTL=500ms 保证"等待 CLI
写盘的轮询"能看到新 .meta.json 文件（轮询间隔 250ms，TTL 500ms 允许
旧缓存命中一次后强制刷新）。

#### 3.3.4 生命周期

- `Process.Spawn` 注入 `linker.projectDir = resolveProjectDir(opts.WorkingDir)`。
- readLoop 在首个 `system.init` 捕获 `session_id` → `linker.parentSessionID`。
  **init 前**调 Resolve 直接返回 ""。
- readLoop 在 `system.task_started`（task_type==in_process_teammate）后
  `go linker.Resolve(...)`，带 ctx=3s；主循环不阻塞。
- Resolve 完成回调：`l.onResolve(taskID, toolUseID, internalAgentID)` → tailer
  + `EventLog.SetAgentInternalID(toolUseID, id)`。
- Process 退出丢弃 Linker。InjectHistory 重放时历史 Agent/task_start 再
  跑 Resolve（best-effort；文件可能已被 CLI /new 清理，此时 tombstone）。

#### 3.3.5 锁 pattern（D5 修正）

- `byTaskID`/`byToolUseID`/`resolvedTaskID`/`dirCache` 全部受 `l.mu` 保护。
- `Resolve` 开头 RLock 做 guard 查询；中段无锁 IO；最后 Lock 写 map。
- `InternalAgentID(taskID string)` 公共读方法：RLock。
- Resolve 并发安全：多 goroutine 对同一 taskID 并发进入 step 1 guard 会
  重入一次 scan，最后的 Lock 写一次相同 LinkInfo，无冲突（幂等）。

#### 3.3.6 并发

- 同一 turn 的 3 个 task_started 并发 Resolve → 受 dirCache 保护只扫一次。
- 文件 size/stat 调用无锁并行。

### 3.4 TranscriptReader

新模块 `internal/cli/subagent_transcript.go`。

```go
type TranscriptReader struct {
    path   string
    offset int64    // 下次读取起点
    tail   []byte   // 末尾半行残片
}

func (r *TranscriptReader) Read(after int64, limit int) ([]EventEntry, error)
func (r *TranscriptReader) Tail() ([]EventEntry, error)
```

#### 3.4.1 磁盘 type → EventEntry 映射（W7/W8/W1 修正）

| 磁盘形状 | 产出 EventEntry |
|---|---|
| `user, content=string, 含<teammate-message>` | **跳过**（控制信道；prompt 和 shutdown 都走这里） |
| `user, content=string, 其他` | `Type="text", Summary=content[:120]` |
| `user, content=[{tool_result}]`, content 仅有 `tool_reference` 块 | **跳过**（ToolSearch schema 返回，对用户无价值） |
| `user, content=[{tool_result}]`, 其他 | `Type="tool_result"`, 见 §3.4.2 |
| `user, content=[{text}]` | `Type="text"` |
| `assistant, {thinking}` | `Type="thinking"` |
| `assistant, {text}` | `Type="text"` |
| `assistant, {tool_use, name=Bash/Read/Edit/...}` | `Type="tool_use", Tool=name` |
| `assistant, {tool_use, name=Agent}` | **退化为 `Type="tool_use"`**，不产 agent 行（本 RFC 不支持二级 drill-in） |
| `system, subtype=api_error` | `Type="system", Summary="api_error"` |
| 其他 `system` / `attachment` / 空 content / 空 result | 跳过 |

**判定"跳过 teammate 控制信道"的规则**：`strings.Contains(content,
"<teammate-message teammate_id=")`。探针验证：所有 teammate 控制消息都
用这个包络（prompt 和 shutdown 共用）。不用"首行"计数（W7，offset 归零时
判定会坏）。

Timestamp 用 `time.Parse(time.RFC3339Nano, e.Timestamp)` → UnixMilli；
失败退回 `time.Now().UnixMilli()`。

#### 3.4.2 tool_result 归一化

探针观察到的 content 形状：

```
string:  "<persisted-output>\nOutput too large (86.9KB). Full output saved at:
         /home/ec2-user/.claude/projects/.../tool-results/<task_id>.txt\n..."

[]any:   [{"type":"text", "text":"{...}"}]
         [{"type":"tool_reference", "tool_name":"SendMessage"}]   ← W8 整条跳过
```

```go
// 返回：summary, detail, persistedPath, skip
func flattenToolResult(c any) (string, string, string, bool) {
    switch v := c.(type) {
    case string:
        persistedPath := ""
        if strings.HasPrefix(v, "<persisted-output>") {
            persistedPath = extractPersistedPath(v)  // 正则抓 "saved at: <path>"
        }
        return firstLine(v, 120), trunc(v, 16000), persistedPath, false

    case []any:
        var b strings.Builder
        onlyRefs := true
        for _, item := range v {
            m, _ := item.(map[string]any)
            if m == nil { continue }
            switch m["type"] {
            case "text":
                onlyRefs = false
                b.WriteString(getStr(m, "text"))
            case "tool_reference":
                // 不写入 builder；若全部是 tool_reference 则 skip
            }
        }
        if onlyRefs {
            return "", "", "", true  // §3.4.1 整条跳过
        }
        s := b.String()
        return firstLine(s, 120), trunc(s, 16000), "", false
    }
    return "", "", "", true
}
```

`extractPersistedPath(s)` 返回的 path **保留原始格式**（可能是绝对路径）；
server endpoint 负责相对化 + 白名单校验。

**注意（§1.7 A7）**：`<persisted-output>` 里的 "saved at" 路径是 CLI 写
的绝对路径，server 端不能直接打开；必须 strip 前缀只保留 `tool-results/<id>.ext`。
**A7 未完全验证**，Phase 1 前需确认路径格式。

### 3.5 Server API

#### 3.5.1 HTTP 端点

```
GET /api/sessions/agent_events
    ?key=<session_key>&node=<node>&task_id=<tXXX>
    &after=<unix_ms>&limit=<int>
→ 200 [EventEntry...]            chronological
→ 202 {"status":"pending"}       Linker 未解（guard: before tombstone）
→ 404 {"error":"unknown task"}   不属于该 key / Linker 已 tombstone
→ 400                            参数非法
```

**Handler 访问链**（R8 修正）：

```go
func (h *AgentH) handleEvents(w, r) {
    key, node, taskID, after, limit := parseQuery(r)
    if err := session.ValidateSessionKey(key); err != nil { 400 }
    if !taskIDRe.MatchString(taskID) { 400 }

    sess := h.hub.router.GetSession(key)          // 按 key 定位，天然隔离
    if sess == nil { 404 }
    proc := sess.Process()
    if proc == nil { 404 }                         // 历史会话无 live Linker
    linker := proc.Linker()

    info, resolved := linker.Query(taskID)         // 公共读接口
    if !resolved { 202 }
    if info.InternalAgentID == "" { 404 }          // tombstone
    reader := cli.NewTranscriptReader(info.JSONLPath)
    events, err := reader.Read(after, limit)
    ...
    json.NewEncoder(w).Encode(events)
}
```

```
GET /api/sessions/tool_result
    ?key=<session_key>&node=<node>&path=tool-results/<id>.txt
→ 200 text/plain                 文件内容
→ 404 / 400                      非法 / 不存在
```

**Handler 访问链**（W10 修正）：

```go
func (h *AgentH) handleToolResult(w, r) {
    key, node, relPath := parseQuery(r)
    validatePath(relPath)                          // 必须以 "tool-results/" 开头
                                                   // 后缀白名单: .txt|.json|.log
                                                   // 无 "../"、无 "\0"、无绝对路径
    cleaned := path.Clean(relPath)
    if !strings.HasPrefix(cleaned, "tool-results/") { 400 }

    sess := h.hub.router.GetSession(key)
    if sess == nil { 404 }
    proc := sess.Process()
    if proc == nil { 404 }
    linker := proc.Linker()
    projectSessionDir := linker.ProjectSessionDir()   // <projectDir>/<parentSessionID>
    if projectSessionDir == "" { 404 }                // init 前

    abs := filepath.Join(projectSessionDir, filepath.FromSlash(cleaned))
    resolved, err := filepath.EvalSymlinks(abs)
    if err != nil { 404 }
    // Symlink escape defence
    toolResultsRoot := filepath.Join(projectSessionDir, "tool-results")
    if !strings.HasPrefix(resolved, toolResultsRoot+string(filepath.Separator)) { 404 }

    info, _ := os.Stat(resolved)
    if info == nil || info.IsDir() { 404 }
    if info.Size() > maxToolResultBytes { 413 }       // 16 MB 上限
    http.ServeFile(w, r, resolved)
}
```

#### 3.5.2 WebSocket 订阅

前端 → 后端：
```json
{"type":"agent_subscribe",   "key":"...", "node":"...", "task_id":"tXXX"}
{"type":"agent_unsubscribe", "key":"...", "node":"...", "task_id":"tXXX"}
```

后端 → 前端：
```json
{"type":"agent_event", "key":"...", "node":"local",
 "task_id":"tXXX", "event": {...EventEntry...}}
{"type":"agent_done", "key":"...", "node":"local",
 "task_id":"tXXX", "status":"completed|error"}
{"type":"agent_meta", "key":"...", "node":"local",
 "task_id":"tXXX", "meta": {last_tool, tool_uses, duration_ms}}
```

ACL：agent_subscribe 校验 key 属于当前 dashboard token（复用
`handleSend` pattern）。

#### 3.5.3 Tail 实现（纯轮询）

新文件 `internal/server/agent_tailer.go`。

```go
type agentTailer struct {
    key         string
    taskID      string
    toolUseID   string
    reader      *cli.TranscriptReader
    subs        map[*wsClient]struct{}    // WS 订阅者
    refCount    atomic.Int32
    stopCh      chan struct{}
    doneFired   atomic.Bool
    meta        agentMeta                 // 本地聚合状态（D4）
    lastSize    int64
    lastActive  time.Time
}

// 200 ms ticker：
//   os.Stat(path).Size > lastSize → Tail → 聚合 + 广播
//   父流 agent_done 触发 / 30 s 文件未更新 → close
```

**不引 fsnotify**：活跃 agent ≤ 50 的场景 200 ms Stat 成本可忽略，省掉
依赖/平台差异/watch 泄漏。

#### 3.5.4 aggregator（silent tailer，R13/D4 修正）

**触发**：silent tailer 在 `Linker.onResolve` 回调里启动，**不绑 task_started**
（task_started 时 InternalAgentID 还没解出，没法读文件）：

```go
linker.OnResolve(func(taskID, toolUseID, internalAgentID string) {
    if internalAgentID == "" { return }   // tombstone，不起 tailer
    jsonlPath := linker.InfoByTask(taskID).JSONLPath
    h.hub.ensureTailer(key, taskID, toolUseID, jsonlPath)
})
```

**ensureTailer** 引用计数：
- 同一 (key, taskID) 共享一个 tailer。
- refCount=0 时为 "silent"（仍跑，只做 aggregate，不广播）。
- WS agent_subscribe → refCount++，同时 tailer 转为 "broadcasting"（额外
  推 agent_event 给该 ws）。
- 同 taskID 多订阅幂等（refCount++）。
- WS agent_unsubscribe / disconnect → refCount--；refCount==0 回到 silent。
- task_done → close tailer（无论 refCount）。
- 活跃 tailer 上限 50，超限不再起 silent；HTTP 轮询路径仍工作。

**Aggregator 状态存在 tailer 本地（D4 修正）**，不回写 EventLog.turnAgents。
Snapshot 时 server 层在 `managed.go` Snapshot() 返回后做 enrich：

```go
// in internal/server/dashboard_session.go or wshub.go
func enrichSnapshot(s *session.SessionSnapshot, hub *Hub) {
    for i := range s.Subagents {
        if t := hub.tailerByTaskID(s.Key, s.Subagents[i].TaskID); t != nil {
            m := t.MetaSnapshot()
            s.Subagents[i].LastTool = m.LastTool
            s.Subagents[i].LastDetail = m.LastDetail
            s.Subagents[i].ToolUses = m.ToolUses
            if s.Subagents[i].DurationMS == 0 {  // 父流 task_done.duration 优先
                s.Subagents[i].DurationMS = m.DurationMS
            }
        }
    }
}
```

这样 `cli` 包不依赖 `server` 包；aggregator 彻底属于 server 层。

### 3.6 前端重设计

#### 3.6.1 turnState 结构

```js
let turnState = {
  parent: {
    toolCount: 0, currentTool: null, isThinking: false, isWriting: false,
    thinkingSummary: '', toolCounts: {}, toolOrder: [],
    turnStartTime: 0, timerId: null
  },
  agents: [{
    toolUseId, taskId, internalAgentID,
    name, teamName, description, background, taskType,
    status, lastTool, lastDetail, toolUses, totalTokens, durationMs,
    startedAtMs
  }],
  activeAgentView: null,         // null | taskId
  collapsedByAuto: false,        // true = 自动折叠中，点击展开后本轮不再折叠
  readyCollapseTimerId: null
};
```

**D3 提示**：现有代码有数十处 `turnState.isThinking / isWriting / currentTool
/ agents / toolCount / toolOrder / thinkingSummary / turnStartTime / timerId`
直接引用。Phase 3 需**全局替换为 `turnState.parent.*`**（agents 保持
顶层）。预计 Phase 3 改动 800-1000 行 JS（比原估 +60%）。

#### 3.6.2 事件分派（R3 修正）

| event.type | parent | agents | 备注 |
|---|---|---|---|
| user | reset | reset | turn 边界 |
| result | reset | reset | turn 边界 |
| thinking | write | skip | parent only |
| text | write | skip | parent only |
| tool_use | write | skip | 含 ToolSearch/Bash/Read/... |
| agent | write currentTool="调度 Agent" | push new row | 同步 |
| task_start | skip | update by toolUseId → status='running' | 不覆盖 InternalAgentID |
| task_done | skip | update by taskId → status='completed/error' | 父流 duration 权威 |
| todo | write | skip | parent only |
| system | skip | skip | 无视觉 |
| agent_event (WS) | skip | — | active view → appendEventToScroll；其他 → 忽略 |
| agent_meta (WS) | skip | update by taskId | lastTool/toolUses/duration |
| agent_done (WS) | skip | update by taskId → status | 冗余兜底 |

#### 3.6.3 Banner 布局

```
┌─ running-banner ─────────────────────────────────────┐
│ ● 父会话 · Edit dashboard.js              0:12       │ ← .rb-parent-row
│ ╭─ team: file-listers · 3 agents ─╮                  │
│ │ ● lister-1 · Bash ls /tmp  3 calls · 2.1s          │ ← .rb-agent-row[data-task]
│ │ ● lister-2 · Read /etc/pw  1 calls · 0.8s          │
│ │ ✓ lister-3 · done          2 calls · 1.5s          │
│ ╰─────────────────────────────────────────────────╯  │
│ ● solo-agent · thinking                              │
└───────────────────────────────────────────────────────┘
```

- 所有 row cursor:pointer + hover 高亮。
- 当前选中行加 `.active`（accent 左边框）。
- `agents.length === 0` → 退化为原单行布局。

**自动折叠（W9 修正）**：

```js
function maybeAutoCollapse() {
  const sess = sessionsData[sid(selectedKey, selectedNode)];
  if (!sess || sess.state !== 'ready') return;
  if (turnState.agents.length === 0) return;
  if (turnState.collapsedByAuto) return;  // 已折叠中
  turnState.collapsedByAuto = true;
  collapseBanner();  // 显示 "本轮 team (3) ▶" chip
}

// 触发时机：
// 1. session 进入 ready 态：updateSendButton('ready') 里 setTimeout 30s 后 maybeAutoCollapse
// 2. 任何 banner 手动交互（展开 chip / 点 agent row）：
//      turnState.collapsedByAuto = false → 本轮不再自动折叠
// 3. 下一轮 user 事件：resetTurnState 里复位 collapsedByAuto=false
```

#### 3.6.4 switchAgentView（R4/R14/D2 修正）

```js
let _switchAgentSeq = 0;
let _switchAgentRetries = 0;
const MAX_SWITCH_RETRIES = 20;   // R14: 5 秒上限（20 × 250 ms）

async function switchAgentView(taskId) {
  const seq = ++_switchAgentSeq;
  if (!taskId) _switchAgentRetries = 0;
  turnState.activeAgentView = taskId;
  turnState.collapsedByAuto = false;  // W9 展开后本轮不再折叠
  refreshBanner();

  const el = document.getElementById('events-scroll');
  el.innerHTML = '';

  if (!taskId) {
    hideAgentBreadcrumb();
    wsm.agentUnsubscribeAll(selectedKey);
    lastEventTime = 0;
    await fetchEvents(true);
    return;
  }

  showAgentBreadcrumb(taskId);
  const dispatchKey = selectedKey;
  const r = await fetch(agentEventsURL(taskId, 0, 200));
  if (seq !== _switchAgentSeq || selectedKey !== dispatchKey) return;

  if (r.status === 202) {
    _switchAgentRetries++;
    if (_switchAgentRetries >= MAX_SWITCH_RETRIES) {
      showToast('该 agent 暂无内部记录', 'warning');
      switchAgentView(null);
      return;
    }
    setTimeout(() => { if (seq === _switchAgentSeq) switchAgentView(taskId); }, 250);
    return;
  }
  if (r.status === 404) {
    showToast('该 agent 暂无内部记录', 'warning');
    switchAgentView(null);
    return;
  }
  if (!r.ok) {
    showToast('加载失败 (' + r.status + ')', 'error');
    switchAgentView(null);
    return;
  }

  const events = await r.json();
  renderAgentEvents(events);
  restoreAgentScroll(dispatchKey, taskId);  // D2：首次顶端，再进恢复
  wsm.agentSubscribe(dispatchKey, taskId);  // 内置 150 ms debounce
}

// D2 scroll policy
function restoreAgentScroll(key, taskId) {
  const k = key + '|' + taskId;
  const pos = sessionScrollPos[k];  // 复用现有机制
  const el = document.getElementById('events-scroll');
  if (!pos) el.scrollTop = 0;        // 首次：顶端
  else el.scrollTop = pos.scrollTop;  // 再进：恢复
}
```

#### 3.6.5 面包屑

```html
<div class="agent-breadcrumb" id="agent-breadcrumb" style="display:none">
  <button class="bc-back" onclick="switchAgentView(null)">← 返回父会话</button>
  <span class="bc-tag">agent</span>
  <span class="bc-name" id="bc-agent-name">lister-1</span>
  <span class="bc-team" id="bc-agent-team">file-listers</span>
  <span class="bc-stat" id="bc-agent-stat">3 calls · 运行中</span>
</div>
```

Esc：activeAgentView 非空时 `switchAgentView(null)`。

#### 3.6.6 会话切换清理

- `pickSession` → agentUnsubscribeAll + activeAgentView=null + hideAgentBreadcrumb。
- WS 断重连 → 若 activeAgentView 还有值，重新 agentSubscribe。
- 离开 agent 视图前保存 scrollTop 到 sessionScrollPos[key+'|'+taskId]。

#### 3.6.7 tool_result 渲染

- 新 case `'tool_result'`：默认**折叠**（`<details>` 或自实现）。
- 样式 `.event.tool-result`：灰底小字，左侧 `⇤` 缩进。
- 携带 `persisted_path` 时追加 "📎 打开完整输出" 按钮 → 打开
  `/api/sessions/tool_result?key=...&path=...` 新窗口。

#### 3.6.8 tool verbs 扩展

```js
const toolVerbs = {
  ...existing...,
  TeamCreate: '组队', TeamDelete: '解散', SendMessage: '发消息',
  ToolSearch: '查文档', TaskOutput: '取结果', TaskStop: '停任务',
  ScheduleWakeup: '预约', CronCreate: '新建定时',
  CronDelete: '删定时', CronList: '列定时'
};
```

### 3.7 降级矩阵

| 场景 | 降级 |
|---|---|
| Linker 3 s 超时 | tombstone → 404；UI toast "该 agent 暂无内部记录" |
| transcript 文件被 CLI 清理 | Stat 失败 → 404（砍掉 410 状态，W5.1） |
| 旧版 CLI（无 subagents/） | Resolve step 2 扫空 → tombstone |
| JSON 部分写 | TranscriptReader.tail 保留残片拼接下次 |
| WS 断连 | HTTP 轮询兜底；重连后 resubscribe |
| cwd 含罕见字符 | projectDir 编码规则通用（R7 修正）；若 CLI 改规则则 §1.7 需补验证 |
| ACP 协议无 subagents/ | §1.7 A4 验证前：全部 tombstone；验证后可能需按 ProtocolName 分支 |
| session_uuid 重连后变化 | §1.7 A3 验证前：重连后 Linker 重置 projectDir 时捕获新 uuid；历史映射丢失是可接受的 |

## 4. 安全

- **路径校验**：
  - `internalAgentID`: `^agent-[a-f0-9]{17}$`
  - `session_id`: UUID v4 正则
  - `task_id`: `^[a-z0-9]{1,32}$`（父流实测为 `t` + 8 位 base36 / `b` + 8 位）
  - `tool_result?path=`: 必须 `tool-results/` 开头 + `.(txt|json|log)` 后缀 +
    无 `..` / 无绝对路径 / filepath.EvalSymlinks 后仍在 projectSessionDir 内
- **读权限**：`~/.claude/projects/` 是 naozhi uid 私有目录，uid 边界已成立。
- **XSS/CSP**：EventEntry 经 `esc()`；tool_result Detail 同等处理；
  persistedPath 返回 text/plain + `<pre>` 包裹。
- **WS ACL**：agent_subscribe 校验 key ∈ dashboard token；复用 handleSend。
- **cwd 冲突跨目录串扰**：jsonl 首行 `sessionId` 必须 == linker.parentSessionID（§3.3.1 step 5）。
- **task_id 跨 session 撞车**：Linker 绑在 Process 上；endpoint 先按 key 定位
  session 再访问 Linker（§3.5.1 handler 访问链）。
- **资源上限**：
  - tailer ≤ 50（硬上限）；超限新 task_started 不起 silent，HTTP 轮询仍工作
  - 每 tailer stop 时 close 所有 subs channel
  - agentSubscribe 幂等（refCount）

## 5. 实施计划

### Phase 0 — 探针补漏（✅ 已完成，见 §1.7）

2026-05-08 已跑，结论：
- **A3** 正常 reconnect 稳定；CLI-dead 后可能变 → 影响 Phase 4
- **A7** tool-results 文件名独立命名空间，绝对路径在 `<persisted-output>` 文本内
- **A2 顺带** bg agent 单份 jsonl，meta.json 多 `description`/`name` 字段

其余 A1/A4/A5/A6/A8 延到 Phase 4 按需验。

### Phase 1 — 磁盘定位 + Linker + HTTP API（~750 行 + 测试）

1. `subagent_link.go` + test：7 步算法含所有 §3.3.1 edge cases +
   dirCache TTL + sessionId 校验 + parentSessionID==""/projectDir=="" 门控
2. `subagent_transcript.go` + test：映射表全路径 + flattenToolResult
   三 shape + 首行残片
3. EventEntry/SubagentInfo 字段扩展 + applyEntryStateLocked 新分支
4. `Process` 挂 Linker；`Wrapper.Spawn` 注入 projectDir；readLoop 异步 Resolve
5. `EventLog.SetAgentInternalID` + `UpdateAgentMeta`
6. `/api/sessions/agent_events` + `/api/sessions/tool_result` + 访问链
   handler + 路径白名单
7. **Phase 1 验收工具**（D1 修正）：
   - 单测 pass + `go test -race -cover ./internal/cli/... ./internal/server/...`
   - 新增 `cmd/probe/agent_events/main.go`：spawn mock CLI + mock shim →
     发 3 个 Agent tool_use + task_started + 磁盘写 transcript →
     HTTP assert 返回合理 EventEntry 列表
   - 真机手工验证：dashboard 启 team；浏览器 devtools 看 Subagents 里的
     task_id → curl agent_events + tool_result 双端点

### Phase 2 — WS + silent tailer + aggregator（~500 行 + 测试）

1. `agent_tailer.go` + refCount + silent/broadcasting 模式
2. wshub agent_subscribe/unsubscribe 分发 + 150 ms debounce
3. `Linker.OnResolve` 回调 → ensureTailer
4. `enrichSnapshot`（server 层，D4）
5. `agent_meta`/`agent_done` WS 消息

验收：devtools 看 agent_event/meta 推入；tailer 计数稳定；停订阅 tailer 回收。

### Phase 3 — 前端 UI（~800-1000 行 JS + ~80 行 CSS，D3 修正）

1. turnState.parent 拆分（全局 grep/replace）
2. 事件分派表实现（applyParentEvent/applyAgentMetaEvent 拆分）
3. Banner 多行布局 + active 类 + 折叠 chip
4. switchAgentView + 重试上限 + Esc + scroll 策略
5. tool_result 折叠渲染 + persisted_path 按钮
6. WS agentSubscribe/Unsubscribe 前端绑定 + 断重连恢复
7. 会话切换清理
8. tool verbs
9. auto-collapse timer

验收：
- 3 agent team 场景 banner 3 行独立刷新
- 点任一行看到 Bash/Read/thinking/tool_result
- Esc 回父；parent 输出不影响 agent 视图订阅
- ready 30 s 后自动折叠；用户展开后本轮不再折叠；新 turn 重置
- 完成 agent row 仍可点击回看

### Phase 4 — 回放 & 边界（~200 行）

1. InjectHistory 重放触发 Linker 重解（best-effort）
2. transcript 清理后 tombstone 行为
3. 完成 agent 列表折叠 chip 的展开/收起动画
4. 嵌套 Agent 降级（§2.2；agent 视图内 Agent tool_use 不产 agent 行）
5. §1.7 剩余假设 (A1/A2/A4/A5/A6/A8) 的实测覆盖

## 6. 风险登记

1. **CLI transcript schema 变动**：解析集中 `subagent_transcript.go`；
   encoding 集中 `resolveProjectDir`；变动改一处。
2. **name 冲突**：§2.2 不支持；warn log 观测发生频率。
3. **磁盘写延迟 > 3 s**：宽限常量可调；超时降级合理。
4. **嵌套 Agent（A1 未验证）**：本 RFC 不 drill-in；若未来二级 jsonl 落
   别处，§3.4.1 退化显示仍安全。
5. **silent tailer 资源**：50 上限 + 30 s idle 回收 + task_done 关闭。
6. **Reconnect 历史丢失（A3 未验证）**：若 session_uuid 变，老 task_id 映
   射作废；Linker 重置 tombstone；UI 降级。
7. **ACP 协议（A4 未验证）**：Phase 4 前按 ProtocolName 全链降级到
   tombstone；dashboard 不崩。

## 7. 测试策略

### 7.1 单元

- `subagent_link_test.go`：
  - 单候选 / size=0 过滤 / mtime 择最新
  - dirCache TTL 命中（2 次 Resolve 只 1 次 ReadDir）
  - sessionId 校验错 → skip
  - 首行 timestamp 比父流 Agent 早 >5 s → skip
  - 空目录 3 s 超时 → tombstone
  - projectDir/parentSessionID 为空 → 直接 ""
  - 非法 agentType / 非法 hex → skip
  - R7 编码：`/tmp/a.b` → `-tmp-a-b`
- `subagent_transcript_test.go`：
  - 所有映射路径
  - `<teammate-message>` 跳过（prompt + shutdown 两场景）
  - `tool_reference` only 跳过
  - `<persisted-output>` 识别 + extractPersistedPath
  - 部分写残片保留
  - 非法 JSON 行跳过不中断

### 7.2 集成

- `dashboard_agent_events_test.go`：200 / 202 (pre-tombstone) / 404 (tombstone) /
  400 (path/taskid 非法) / 404 (key 错) / auth
- `dashboard_tool_result_test.go`：白名单 / path traversal / 绝对路径 /
  symlink escape / 超 16 MB → 413
- `ws_agent_subscribe_test.go`：订阅 → 模拟磁盘追加 → 收到事件；
  多订阅共享 tailer；unsubscribe 回收；silent tailer 由 OnResolve 自起；
  debounce 150 ms 验证

### 7.3 端到端（手动）

1. spawn 3 agent team listing /tmp：
   - banner 3 行独立刷新
   - 点每个 agent 看到内部工具流 + tool_result 折叠
   - Esc 回父
2. 运行中刷新页：banner 恢复 + 订阅重连
3. 旧版 CLI：banner == 现状
4. 大 Bash 输出触发 persisted_path，点击打开
5. ready 30 s 自动折叠 + 展开后本轮不折叠
6. cwd 含 `.` 的项目下跑 team（验证 §3.3.2 编码）

## 8. 文件清单

**新增**（~1,650 行）：
- `internal/cli/subagent_link.go` ~250
- `internal/cli/subagent_link_test.go` ~350
- `internal/cli/subagent_transcript.go` ~300
- `internal/cli/subagent_transcript_test.go` ~350
- `internal/server/dashboard_agent_events.go` ~180
- `internal/server/dashboard_agent_events_test.go` ~220
- `internal/server/dashboard_tool_result.go` ~100
- `internal/server/dashboard_tool_result_test.go` ~100
- `internal/server/agent_tailer.go` ~250
- `internal/server/agent_tailer_test.go` ~300
- `internal/server/ws_agent_subscribe.go` ~120
- `internal/server/ws_agent_subscribe_test.go` ~250
- `cmd/probe/agent_events/main.go` ~150（D1）

**修改**（约 +900 / −80 行）：
- `internal/cli/eventlog.go`：EventEntry + SubagentInfo 扩展 +
  `SetAgentInternalID` + `UpdateAgentMeta` + `applyEntryStateLocked` 新 case
- `internal/cli/process.go`：Linker 字段 + readLoop Resolve 异步
- `internal/cli/wrapper.go`：Spawn 注入 projectDir
- `internal/session/managed.go`：Snapshot.Subagents 透传
- `internal/server/server.go`：新路由
- `internal/server/wshub.go`：agent_* 分发 + enrichSnapshot
- `internal/server/static/dashboard.js`：turnState 拆分 + banner +
  switchAgentView + 分派 + tool_result / persisted_path / 折叠
- `internal/server/static/dashboard.html`：~80 行 CSS

## 9. 开放问题

- Agent 级 cost：usage 字段聚合需全文扫描；不做。
- 导出 transcript 按钮：下一轮。
- name 冲突升级为多候选并列：等实际数据。
- 父视图加 "task_done 占位事件"：本 RFC 不加；父流 task_notification
  已给 banner 状态，父视图无需再展示。

## 10. 决策记录（vs v2 差异）

- **R7** projectDir 编码改为通用 `[^A-Za-z0-9]→'-'` + sessionId 二次校验。
- **R8** endpoint 必须按 key → session → linker 访问，从根上避免 task_id 撞车。
- **R9** Linker 加 500 ms dirCache TTL。
- **R10** mtime 窗口改为首行 timestamp vs 父流 Agent 时间 ± 5 s。
- **R11** applyEntryStateLocked 每个 case 的 SubagentInfo 填充伪码补齐。
- **R12** task_start 不写 InternalAgentID（异步回填专管）。
- **R13** silent tailer 挂在 `Linker.OnResolve` 回调，不绑 task_started。
- **R14** 前端 202 重试加上限 20 次 / 5 s。
- **R15** auto-collapse 通过 setTimeout 30 s + collapsedByAuto 标记；用户
  展开后本轮不再折叠。
- **W7** teammate 控制信道统一按字符串包络判定跳过。
- **W8** tool_reference only 的 tool_result 整条跳过。
- **W10** tool_result endpoint 访问链补全（按 key → projectSessionDir）。
- **D3** Phase 3 行数上调至 800-1000。
- **D4** aggregator 挪到 server 层（agent_tailer.go 本地状态 + enrichSnapshot）。
- **D5** Linker 锁 pattern 显式 RLock/Lock 分段。
- **砍 410**：transcript 清理走 404。
- **§1.7 新增**：8 条待验证假设，Phase 0 优先验 A3/A7。
