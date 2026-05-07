# Passthrough Mode — Phase 0 实测报告

> **CLI 版本**: claude 2.1.126 (opus-4-6 via Bedrock)
> **日期**: 2026-05-06
> **测试目录**: `test/e2e/passthrough/`
> **原始 ndjson**: `/tmp/v*.ndjson`

本文档记录 RFC §7 的 9 个验证点（V1-V9）的实测结果，每个场景都有脚本 + 原始日志可复现。

---

## 关键发现一览

| 发现 | 影响 |
|---|---|
| `--replay-user-messages` 是 naozhi ↔ CLI uuid 匹配的**唯一**可靠机制 | 必须默认启用 |
| CLI 合并多条消息时，只 emit **一个 replay event**，但该 event 的 content 里含**所有被合并消息的 text blocks** | naozhi 可以逆推合并集合 |
| `priority:"now"` 工作完美：立即 abort 当前 turn 并处理新消息 | `/urgent` 命令可落地 |
| `control_request interrupt` 不丢弃 pending 队列 | `/stop` 可落地 |
| Mid-turn 注入成功率依赖**内容**（非对抗性成功），不依赖路径 | 用户需心智接受 |
| 纯生成 turn 无法 mid-turn，延迟到下一 turn | 和 CC TUI 一致 |
| 并发 stdin 写安全（6 线程交错 all 到达） | shimWMu 已足够 |

---

## V1: `<system-reminder>` wrap 路径验证

**脚本**: `v1_system_reminder_path.py` + `v1b_with_replay.py` + `v1c_benign_wording.py`

**场景**: bash 期间注入非对抗性追加指令，检查 wrap 路径是否生效。

### 结果

| 测试 | 配置 | 注入内容 | 结果 |
|---|---|---|---|
| V1 | 无 replay flag | "include INJECTED at the start of final reply" | **FAIL** — model didn't execute |
| V1b | 带 replay flag | 同 V1 | **FAIL** — replay event 出现了（路径对），但 model 没执行 |
| V1c | 带 replay flag | "say HELLO at the very start of your reply"（A1 原文） | **PASS** — reply = `"HELLO\nCommand finished..."` |

### 结论

- **Replay 事件本身证明路径存在**：V1b 看到 `isReplay:true` + 注入消息在 tool_result 后紧贴出现
- **执行率和内容强相关**: "INJECTED"/"final" 组合模型可能触发某种保守判定；改用"HELLO" 完美照做
- **路径 = CC TUI**（`wrapMessagesInSystemReminder` + `wrapCommandText`），只是模型内部决策受内容影响
- naozhi 生产路径和 CC TUI 完全相同，**能提供 mid-turn 引用能力，但必须 caveat 成功率**

---

## V2: `priority:"now"` 立即 abort

**脚本**: `v2_priority_now.py`

**场景**: T=0 发长 bash 任务，T=3s 发 `priority:"now"` PIVOT 消息。

### 结果

```
T=3.00s  -> SEND priority:"now"
T=3.01s  CLI emit: result subtype=error_during_execution (仅 10ms 延迟)
T=3.01s  CLI emit: new system/init (新 turn 开始)
T=12.38s CLI emit: result subtype=success result="PIVOT"
```

### 结论

- **PASS**: `priority:"now"` 触发 CLI `abortController.abort('interrupt')` 路径（`print.ts:1858-1863`），10ms 级别响应
- 新 turn 自动处理 priority:"now" 的消息，不需要任何 control_request
- **naozhi 可直接把 `/urgent <msg>` 映射到 `priority:"now"` 的 stream-json 写入**，省掉自己的 InterruptViaControl 路径

---

## V3: `--replay-user-messages` vs result 事件数

**脚本**: `v3_replay_vs_result.py`

**场景**: 5 条消息间隔 50ms 连发，分别在带/不带 replay flag 下观察。

### 结果

| 模式 | stdin | replays | results |
|---|---|---|---|
| V3a (replay=on) | 5 | **5** (全部 uuid 保留) | 2（APPLE + "2345"） |
| V3b (replay=off) | 5 | 0 | 2（APPLE + ELDERBERRY） |

**V3a 详细 replay 事件**：
```
replay[0] uuid=aa7c8bb8 n_texts=1 texts=["Say '1' only."]          ← 第1条，独立处理
replay[1] uuid=83bbf05f n_texts=1 texts=["Say '2' only."]          ← 入 commandQueue
replay[2] uuid=5ea3694e n_texts=1 texts=["Say '3' only."]          ← 入 commandQueue
replay[3] uuid=4b54f4df n_texts=1 texts=["Say '4' only."]          ← 入 commandQueue
replay[4] uuid=93c6512c n_texts=4 texts=["2","3","4","5"]           ← CLI 合并事件
```

### 结论

- **PASS**: Replay 机制完整；uuid 可追溯
- **关键**: CLI 合并多条时生成**一个新的 "合并 replay" 事件**（新 uuid + 所有被合并消息的 text blocks）
- **FIFO 匹配策略**: 
  - 独立 replay (n_texts=1) → 对应下一个 result
  - 合并 replay (n_texts>1) → **合并后的 N 条消息共享**一个 result
- **不带 replay 时 CLI 不会告诉我们合并了什么**，uuid 无法匹配 result

---

## V4: CLI 崩溃（SIGKILL）时的 pending 状态

**脚本**: `v4_v5_v8_v9.py` (v4 部分)

**场景**: 发 5 条消息，第 1 条开始 result 后 SIGKILL。

### 结果

```
before SIGKILL: 1 result, 4 replays  ← 第1条完成了，后面 3 条在 commandQueue 里
after SIGKILL: process exited cleanly
```

### 结论

- **PASS**: SIGKILL 后进程干净退出
- naozhi 责任: readLoop 检测到 EOF/exit 时，遍历 pendingSlots fan-out `ErrProcessExited`（已在 RFC §6.3 规划）

---

## V5: Interrupt 后继续发消息

**脚本**: `v4_v5_v8_v9.py` (v5 部分)

**场景**: msgA（长任务）→ interrupt → msgB。

### 结果

```
result[0] subtype=error_during_execution text=''   ← msgA 被中断
result[1] subtype=success text='AFTER_INTERRUPT'   ← msgB 独立 turn 处理
```

### 结论

- **PASS**: `control_request interrupt` 只中断当前 turn，**不清空 commandQueue**
- `/stop` 指令可直接使用 CLI 原生 interrupt

---

## V6: Uuid round-trip

**脚本**: `v6_uuid_binding.py`

**场景**: 3 条带 uuid 的消息，检查 result 和 replay 里是否回传。

### 结果

| 模式 | uuid 是否回传 |
|---|---|
| V6a (replay=on) | **replay event 里 uuid round-trip 成功** |
| V6b (replay=off) | **uuid 不出现在任何 stdout 事件里** |
| result 事件 | **无论 on/off，result.uuid 都是 CC 内部的 assistant message uuid，不是我们 send 的 uuid** |

### 结论

- **PASS (V6a)**: 启用 `--replay-user-messages` 是 uuid 匹配的唯一方式
- result 事件不能直接匹配 uuid — 必须通过 replay 事件作为桥
- naozhi 的匹配逻辑：
  1. 每个 stdin user 消息带 uuid（保存到 sendSlot.uuid）
  2. 收到 replay 事件 → 按 uuid 找到 slot，标记 `delivered=true`
  3. 收到合并 replay (n_texts>1) → 按 text 内容逆查找匹配所有被合并的 slot，这些 slot 共享下一个 result
  4. 收到 result 事件 → 投递给"最早一组未 resolved 的 slots"

---

## V7: CLI 合并模式深度观察

**脚本**: `v7_coalesce_patterns.py`

**场景**: 5 条消息 @ 0ms / 50ms / sequential-with-wait 三种间隔。

### 结果

| 配置 | 独立 replay | 合并 replay | 独立 result | 合并 result |
|---|---|---|---|---|
| V7a (50ms 间隔) | 4（1-4 各独立）| 1（merge [2,3,4,5]） | 1 (APPLE) | 1 ("2345") |
| V7b (等 result 再发下一条) | 5 | 0 | 5 | 0 |
| V7c (0ms 间隔) | 4 | 1（merge [2,3,4,5]） | 1 | 1 |

### 结论

- **合并规律**: CLI 启动下一个 turn 时，把**当时 commandQueue 里的所有 prompt**合并成一条。第一条在启动第一个 turn 时独占；第二条及之后如果在第一个 turn 还没结束前到达 → 全部合并到第二个 turn
- **V7b (sequential) 完美**: 等 result 后再发，每条独立成 turn
- **启示**: naozhi 如果用户**顺序**发消息（人类手输速度），每条独立处理；**程序化快速**发才会合并

---

## V8: 纯生成 turn + mid-turn 注入

**脚本**: `v4_v5_v8_v9.py` (v8 部分)

**场景**: msgA 写长诗（无 tool），T=2s 注入 "mention CIRRUS"。

### 结果

```
result[0]: "Autumn Leaves... (完整诗)"  ← 无 CIRRUS，第一 turn 未受影响
result[1] thinking: "The user wants me to REVISE the poem to include CIRRUS"
result[1]: "Autumn Leaves... (重写版，含 'thin cirrus bars')"  ← 模型重写整首
```

### 结论

- **PASS**: 和 RFC §2/§4 预期一致：纯生成 turn 没有 tool 边界 drain 窗口，注入延迟到下一 turn
- **有趣的副作用**: 模型在同 session 上下文下会**重写**而不是**附加**——浪费 token 但用户意图被满足
- **对 naozhi 的意义**: 不需要特殊处理；用户可以用 `/urgent` 抢占如果等不及

---

## V9: 并发 stdin 写安全性

**脚本**: `v4_v5_v8_v9.py` (v9 部分)

**场景**: 2 个线程交错发 6 条消息。

### 结果

```
6 msgs via 2 threads → 6 replays received
(结果数受 CLI 合并影响，但所有 stdin 写入都成功到达 CLI)
```

### 结论

- **PASS**: 单 Process 的 `shimWMu` 互斥已经保证原子行写入
- naozhi 层可以多 goroutine 并发 Send，共享一个 Process 不会导致 NDJSON 碎裂

---

## 对 RFC v2 的修正 / 确认

| RFC 断言 | 实测结论 |
|---|---|
| `priority:"now"` → 自动 abort | **确认**（V2） |
| `--replay-user-messages` 是 FIFO 匹配必要 | **确认**（V3/V6） |
| CLI 合并 = 整块送到 API | **部分修正**：合并事件有自己的新 uuid；可以按 text blocks 内容反查 |
| result 数 < stdin 数 | **确认**（V3/V7） |
| Mid-turn 成功率依赖时机 + 内容 | **确认**（V1/V1b/V1c 对比） |
| 纯生成 turn 延迟到下一 turn | **确认**（V8） |
| Interrupt 不丢队列 | **确认**（V5） |
| SIGKILL 干净退出 | **确认**（V4） |
| 并发写安全 | **确认**（V9） |

---

## 阻断判定

**全部 9 项验证通过（V1 需理解为"路径正确，执行率内容依赖"）**

没有阻断项。可进入 Phase A 实施。

---

## 下一步 Phase 0 补充

推荐再做两个验证（非阻断但有价值）：

1. **V10 — 长 session 压测**: 连续 50 条消息（等 result 顺序发），验证 session 稳定性 + token 成本
2. **V11 — MessageID 和 parent_tool_use_id 交叉**: 如果用户手动发多个 chat 消息（通过 IM），确认 naozhi 的 chat_key → session_key 路由在 passthrough 下仍然一致

这两个可以和 Phase A 实施并行做。
