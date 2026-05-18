# Multi-Backend 可行性验证报告

> **状态**: Phase 0 实测报告（不是设计文档）
> **作者**: naozhi team
> **日期**: 2026-05-18
> **环境**:
> - kiro-cli 2.3.0（`/home/ec2-user/.local/bin/kiro-cli`）
> - naozhi master HEAD: `8939437`（fix/todo-batch-20260518-0312 分支）
> - 平台: Linux 6.1.166 aarch64
> **关联 RFC**: `multi-backend.md`（本验证报告是其 §15 的支撑材料）

---

## 0. TL;DR

跑了 12 项 PoC，全部完成。结论：

- ✅ **9 项过**（含若干优于预期）
- ❌ **2 项暴露 naozhi 现有 bug**（必须先修）
- ❌ **1 项发现现有架构未实现**（reverse-node capability 路由）

---

## 1. 验证清单

| # | 主题 | 结论 | 关键证据 |
|---|---|---|---|
| V1 | ACP `session/cancel` 行为 | ✅ 过且优于预期 | cancel 是 notification（无 id）；原 prompt 立即收 `stopReason:"cancelled"`；同 sessionId 立即可继续 |
| V2 | `session/load` 持久性 | ✅ 过 | kiro 自动写 `~/.kiro/sessions/cli/<sid>.{json,jsonl}`；新进程 load 同 sid 上下文完整恢复 |
| V2.1 | Stale lock 自动恢复 | ✅ 过 | SIGKILL 后留 lock；下一进程检测 stale PID 自动接管 |
| V5 | Chunk 回放粒度 | ⚠️ 细但可处理 | 平均 2.2 字符/chunk，15 chunks/sec；建议历史用 kiro 自带 jsonl |
| V6 | reverse-node cap 路由 | ❌ 实质未实现 | `Capabilities` 字段定义了，仅 `logUnknownCaps` 打 warn，无路由决策 |
| V7 | tool_use 事件结构 | ⚠️ 暴露 bug | permission_request 暴露 `RPCMessage.ID` 类型与 optionId 硬编码错误 |
| V8 | naozhi RPCMessage.ID 类型 | ❌ 真 bug | kiro UUID 字符串 id → naozhi `*int` 反序列化报错 |
| V9 | SIGTERM 优雅退出 | ✅ 过 | 自动清 lock；jsonl flush；退出码 -15 |
| V10 | `_kiro.dev/*` 扩展事件 | 信息收集 | 4 类私有 notification（metadata / commands / subagent / session/update） |
| V11 | 多 session 并发 | ✅ 过 | 一个 acp 进程能并发 N session，context 隔离 |
| V12 | 启动延迟 | ✅ 过 | --version 3ms / init 585ms / session/new +600ms |

---

## 2. 详细记录

### V1：ACP `session/cancel` 行为

**问题**：发 cancel 后，原 prompt RPC 怎么收尾？同一 sessionId 能否复用？

#### V1a 失败实验（cancel 当 request）

```python
# probe2_cancel.py
send({"jsonrpc":"2.0","id":4,"method":"session/cancel","params":{"sessionId":sid}})
```

输出：
```
cancel(id=4) response: {"error":{"code":-32601,"message":"Method not found","data":"session/cancel"}}
prompt(id=3) response: MISSING (一直没结束，原 prompt 没被打断)
prompt(id=5) response: {"error":{"code":-32603,"message":"Internal error","data":"Prompt already in progress"}}
```

**学到**：ACP `session/cancel` 是 **notification**（无 id），不是 request。

#### V1b 正确实验（cancel 当 notification）

```python
# probe2b_cancel_notif.py — 没有 id 字段
send({"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":sid}})
```

输出：
```
[+0.00s] >>> session/cancel notif
[+0.00s] resp(id=3): {"result":{"stopReason":"cancelled"},"id":3}
[+0.00s] notif(_kiro.dev/metadata): {"turnDurationMs":2000}
[+6.00s] >>> session/prompt id=5
[+9.26s] resp(id=5): {"result":{"stopReason":"end_turn"},"id":5}
chunks before cancel: 0   (cancel 时 prompt 刚启动)
chunks after cancel:  10  (cancel 后涌出的 buffer 残留)
```

**结论**：
- cancel notification 发出后立刻收到原 prompt 的 cancelled result（< 1ms）
- 残留 chunk 约 10 个，正常 readLoop 吞掉即可
- 同 sessionId 立即接受新 prompt，无需重 session/new

---

### V2：`session/load` 持久性

**问题**：kiro 进程死掉后，下一个进程能否用相同 sessionId 复活上下文？

#### Phase 1 — 起 session 跑一轮

```python
# probe3_load.py
send({"id":3,"method":"session/prompt","params":{
    "sessionId":sid,
    "prompt":[{"type":"text","text":"请记住:我的暗号是「紫色独角兽 42」。回复一个字「好」即可。"}]}})
```

观察文件系统：

```
~/.kiro/sessions/cli/
  44737bdd-b2fd-466a-8a30-ca447e688313.lock     (61B, 含 PID + started_at)
  44737bdd-b2fd-466a-8a30-ca447e688313.json     (2414B, session metadata + state)
  44737bdd-b2fd-466a-8a30-ca447e688313.jsonl    (394B, 逐消息日志)
```

`.json` 内容（节选）：
```json
{
  "session_id": "44737bdd-b2fd-466a-8a30-ca447e688313",
  "cwd": "/tmp/kiro-poc",
  "title": "请记住:我的暗号是「紫色独角兽 42」...",
  "session_state": {
    "version": "v1",
    "conversation_metadata": { ... },
    "rts_model_state": {
      "conversation_id": "...",
      "model_info": {"model_id":"auto","context_window_tokens":200000}
    },
    "permissions": { ... },
    "agent_name": "kiro_default"
  }
}
```

`.jsonl` 内容（**这是 naozhi 历史源要读的格式**）：
```
{"version":"v1","kind":"Prompt","data":{"message_id":"...","content":[{"kind":"text","data":"请记住:我的暗号是..."}],"meta":{"timestamp":1779081689}}}
{"version":"v1","kind":"AssistantMessage","data":{"message_id":"...","content":[{"kind":"text","data":"好"}]}}
```

#### Phase 2 — 新进程 session/load

```python
send({"id":10,"method":"session/load","params":{"sessionId":sid,"cwd":"/tmp/kiro-poc","mcpServers":[]}})
```

输出：
```
load_resp: {"result":{"modes":{...},"models":{...}}}  (注意：不再带 sessionId — 复用入参的)
```

测上下文：
```python
send({"id":11,"method":"session/prompt","params":{
    "sessionId":sid,
    "prompt":[{"type":"text","text":"我之前告诉你的暗号是什么?直接回答数字。"}]}})
```

回复：
```
"好42"   ← 模型读到了之前的"暗号是 42"
```

**结论**：
- ✅ session/load 完整恢复对话历史
- ✅ load 成功后还会通过 `session/update` 把历史消息 replay 给客户端（带 `user_message_chunk` / 普通 chunk 类型）
- ⚠️ Lock 文件存在时其他进程会被拒（`Session is active in another process (PID xxx)`）

---

### V2.1：Stale lock 自动恢复

**问题**：kiro 被 SIGKILL 后留 lock 文件，下一进程能不能自己识别 stale？

```python
# probe4_stale_lock.py
os.kill(p1.pid, signal.SIGKILL)
# 也 SIGKILL 子进程（kiro-cli 是 wrapper）
for cp in child_pids:
    os.kill(int(cp), signal.SIGKILL)
```

观察：
```
Lock before kill: {"pid":4058961,"started_at":"2026-05-18T05:25:18.275613098Z"}
Lock after kill:  exists=True (内容不变)

Phase 2 load: result OK ← kiro 自己识别 stale 自动接管
```

**结论**：**naozhi 不需要写清锁逻辑**。

---

### V5：Chunk 粒度

**问题**：ACP chunk 多碎，naozhi 该不该走 chunk 级 history？

```python
# probe5_chunk_grain.py — 让 kiro 写 500 字短文
```

输出：
```
Total chunks:      309
Total text chars:  695
Duration:          20.23s
Avg chunk size:    2.2 chars
Min/Max chunk:     1/8
Chunks/sec:        15.3
First chunk:       '#'
Last chunk:        '十年。'

session/update types: {'agent_message_chunk': 309}  (这一轮没工具调用)
```

**结论**：
- 实时流式 OK（websocket 流到 dashboard 没问题）
- 历史回放走 chunk 级：1 turn = 309 行 jsonl，过细
- **走 kiro 自带 `~/.kiro/sessions/cli/<sid>.jsonl`**（按消息聚合）更优

---

### V6：reverse-node capability 路由

**审计命令**：
```bash
grep -rn 'Capabilities|HasCap|hasCap|filter.*node|select.*node|route.*node' \
    internal/node/ internal/server/nodeclient*.go
```

**结果**：仅命中 `caps.go:logUnknownCaps`（打 warn 不影响路由）和 `protocol.go:88-90`（字段定义）。

**结论**：capability 路由实质未实现。多后端 + reverse 配置出来的 kiro session 会派到任意节点，必然 spawn 失败。

---

### V7：tool_use 事件结构

#### V7a 带 `--trust-all-tools`

```
Total session/update:
  agent_message_chunk: 14
  tool_call: 1
  tool_call_update: 2
permission_request: 0   (--trust-all-tools 已经默认通过)
```

`tool_call` 样本：
```json
{
  "sessionUpdate": "tool_call",
  "toolCallId": "tooluse_ZUluXNpzXvLU872qTJIEtE",
  "title": "Running: cat /etc/hostname",
  "kind": "execute",
  "rawInput": {"command": "cat /etc/hostname"}
}
```

`tool_call_update`（completed 的）：
```json
{
  "sessionUpdate": "tool_call_update",
  "toolCallId": "...",
  "kind": "execute",
  "status": "completed",
  "title": "Running: cat /etc/hostname",
  "rawInput": {...},
  "rawOutput": {"items":[{"Json":{"exit_status":"exit status: 0","stdout":"ip-10-0-141-156..."}}]}
}
```

#### V7b 不带 `--trust-all-tools`

```json
{
  "method": "session/request_permission",
  "id": "82017692-c404-42d1-9334-ae28dfda0cee",   ← UUID 字符串！
  "params": {
    "sessionId": "...",
    "toolCall": {"toolCallId":"...","title":"Running: cat /etc/hostname"},
    "options": [
      {"optionId":"allow_once","name":"Yes","kind":"allow_once"},
      {"optionId":"allow_always","name":"Always","kind":"allow_always"},
      {"optionId":"reject_once","name":"No","kind":"reject_once"}
    ]
  }
}
```

**关键发现**：
1. id 是 **string UUID**，不是 int
2. optionId 是 **下划线** `allow_once`，不是连字符 `allow-once`

---

### V8：naozhi RPCMessage.ID 类型 bug 复现

```go
// naozhi_id_check.go
type RPCMessage struct {
    ID *int `json:"id,omitempty"`   // naozhi 当前定义
}

kiroReq := `{"jsonrpc":"2.0","id":"82017692-...","method":"session/request_permission","params":{}}`
err := json.Unmarshal([]byte(kiroReq), &msg)
// 输出：err: json: cannot unmarshal string into Go struct field RPCMessage.id of type int
```

**结论**：在 master 上跑 kiro 必崩。

#### 修复验证

```go
type RPCMessageV2 struct {
    ID json.RawMessage `json:"id,omitempty"`  // ← 改成 RawMessage
}

func (m *RPCMessageV2) IDAsInt() (int, bool) { ... }
func (m *RPCMessageV2) IDAsString() (string, bool) { ... }
```

测试 3 种输入：
```
err:<nil>  intID:0(ok=false)  strID:"82017692-..."(ok=true)  method:"session/request_permission"
err:<nil>  intID:2(ok=true)   strID:"2"(ok=true)             method:""
err:<nil>  intID:0(ok=false)  strID:""(ok=false)             method:"session/update"   (notification)
```

**结论**：RawMessage + helper accessors 兼容所有 ACP 实测形态。

---

### V9：SIGTERM 优雅退出

```python
# probe7_sigterm.py
proc.send_signal(signal.SIGTERM)
for cp in child_pids: os.kill(int(cp), signal.SIGTERM)
```

输出：
```
Before SIGTERM:
  lock exists: True
  jsonl exists: True (360B)

After exit (rc=-15):
  lock exists: False     ← kiro 自己清掉
  jsonl exists: True (360B)   ← flush 完整
  jsonl content:
    {"version":"v1","kind":"Prompt","data":{...,"data":"说 hello"}}
    {"version":"v1","kind":"AssistantMessage","data":{...,"data":"Hello! 👋 ..."}}
```

**结论**：SIGTERM 路径无需 naozhi 特殊处理。

---

### V10：`_kiro.dev/*` 扩展事件清单

跑两轮交互（一次 plan / 一次总结），统计：

```
method counts:
  session/update: 48
  (response): 4
  _kiro.dev/metadata: 3
  _kiro.dev/subagent/list_update: 1
  _kiro.dev/commands/available: 1
  _kiro.dev/session/update: 1
```

#### `_kiro.dev/metadata` 样本
```json
{
  "sessionId": "...",
  "contextUsagePercentage": 0.028499998152256012,
  "turnDurationMs": 2000,
  "meteringUsage": [{"value":0.022,"unit":"credit","unitPlural":"credits"}]
}
```
**naozhi 增益**：dashboard 上下文用量条 / cost 累加 / turn 计时器。

#### `_kiro.dev/commands/available` 样本（节选）
```json
{
  "sessionId": "...",
  "commands": [
    {"name":"/agent","description":"Select or list available agents","meta":{...}},
    {"name":"/clear","description":"Clear conversation history"},
    {"name":"/compact","description":"Compact conversation history"},
    ...
  ]
}
```
**naozhi 增益**：dashboard 显示 backend 可用 slash 命令。

#### `_kiro.dev/session/update` 样本
```json
{
  "sessionId": "...",
  "update": {
    "sessionUpdate": "tool_call_chunk",
    "toolCallId": "tooluse_uMjnawXiOcD7EIdyMtYdZC",
    "title": "todo_list",
    "kind": "other"
  }
}
```
naozhi 当前 ReadEvent default 分支会把这个返回 `Event{Type:"system",SubType:"tool_call_chunk"}`，一般不渲染。建议提级为 `assistant/tool_use` 让 dashboard 看到工具进度。

#### `_kiro.dev/subagent/list_update`
```json
{"subagents":[],"pendingStages":[]}
```
**潜在命名碰撞**：与 naozhi 自身 subagent 概念可能冲突。当前空数组无伤；未来 kiro subagent 真正用起来，要在 ACP 协议层做 namespace 隔离。

---

### V11：多 session 并发

```python
# probe8_multi_session.py — 同一进程内创建两个 session
send({"id":2,"method":"session/new",...})  → sid1
send({"id":3,"method":"session/new",...})  → sid2
# 两条不同 prompt 几乎同时发
send({"id":10,"method":"session/prompt","params":{"sessionId":sid1,"prompt":[{"type":"text","text":"我的代号是 ALPHA。说「好」。"}]}})
send({"id":11,"method":"session/prompt","params":{"sessionId":sid2,"prompt":[{"type":"text","text":"我的代号是 BRAVO。说「好」。"}]}})
```

输出：
```
sid1: 228e4914-...
sid2: d96e6d6b-...
identical? False    ← 两个独立 session

resp10 stopReason: end_turn
resp11 stopReason: end_turn

# 测隔离 — 问 sid1 它的代号
sid1 cumulative reply: 好。ALPHA
sid1 contains ALPHA: True    ← 上下文完整
sid1 contains BRAVO: False   ← 与 sid2 隔离
```

**结论**：一个 kiro acp 进程支持 N session 并发，context 完全隔离。**未来优化空间**（一个 shim 服务多 session，资源 N 倍）。本 RFC 不利用。

---

### V12：启动延迟

```python
# probe10_startup.py — 5 次取均值
```

输出：
```
run 1: init=585ms, session/new=1176ms
run 2: init=607ms, session/new=1260ms
run 3: init=572ms, session/new=1152ms
run 4: init=578ms, session/new=1165ms
run 5: init=585ms, session/new=1180ms

avg init:        585ms
avg session/new: 1187ms (cumulative)
avg new only:    601ms

kiro --version probe: 3ms avg
```

**结论**：
- 完整握手 ~1.2 秒（与 claude 相当）
- `--version` 仅 3ms — multi-backend detect 串行化无需优化（原方案的 D2 项可推迟）

---

## 3. POC 文件清单

所有脚本与原始日志保留在 `/tmp/kiro-poc/`（不入库；本报告复述关键输出）：

```
probe1_handshake.sh             # V0 最小握手
probe2_cancel.py                # V1a cancel 当 request（错的）
probe2b_cancel_notif.py         # V1b cancel 当 notification（对的）
probe3_load.py                  # V2 phase 1 + 2
probe3b_load_clean.py           # V2 重测（无 stale lock）
probe4_stale_lock.py            # V2.1
probe5_chunk_grain.py           # V5
probe6_tools.py                 # V7a 带 trust-all
probe6b_perm.py                 # V7b 不带 trust-all（暴露 bug）
probe7_sigterm.py               # V9
probe8_multi_session.py         # V11
probe9_kiro_extras.py           # V10
probe10_startup.py              # V12
naozhi_id_check.go              # V8 bug 复现
naozhi_id_v2.go                 # V8 修复方案验证
```

入库 PR 时把这些脚本归档到 `docs/rfc/multi-backend-poc/` 子目录便于复现。

---

## 4. 后续工作

实证结果已经驱动了 `multi-backend.md` v2 的修订，工时与 Sprint 划分：

| Sprint | 主要内容 | 实证依据 |
|---|---|---|
| 0a | RPCMessage.ID + permission optionId hotfix | V7b / V8 |
| 0b | backend.Profile 抽象 | — |
| 1a-1b | 散点收敛 + history 工厂化 | — |
| 1c | kirojsonl 历史源 | V2 / V9 |
| 2 | per-session ReplyTag | — |
| 3 | ACP cancel notification | V1 |
| 3b | _kiro.dev/* 私有事件 enrich | V10 |
| 4a-4d | metrics / cap 路由 / cron / validate | V6 |

Sprint 0a 是 master 现存 bug 的 hotfix，应当作为前置 PR 单独提，不依赖本 epic。

---

## 5. 风险（非验证范围但实证发现）

1. **kiro lock 文件 PID reuse**：`{"pid":xxx}` 没带 boot identity，PID 极端 reuse 时可能误判 stale。kiro 是否对此有应对未深入测；naozhi 不参与即可。
2. **`_kiro.dev/subagent/*` 命名碰撞**：与 naozhi 自身 subagent 概念可能冲突，目前 V10 实测都是空数组，无伤。kiro 真正启用 subagent 后需要协议层 namespace 隔离。
3. **chunk 频率**：15 chunks/sec，单 chunk 1-8 字符 — naozhi 当前 buffer 累加策略正确，但要确认 ManagedSession 的 lastActive / metric 写盘频率不会被 chunk 风暴拖垮。
4. **kiro 内置 todo_list / subagent 工具与 naozhi 自带工具的语义重叠**：V10 看到 kiro 有内置 `todo_list`、`subagent`，naozhi 在 dashboard 也有 todo / askuser 等概念，需要梳理"工具调用流"在两端的展示一致性。
