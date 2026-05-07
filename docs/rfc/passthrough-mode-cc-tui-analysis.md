# CC TUI Mid-Turn 机制代码级分析

> **范围**: 基于 claude-code 源码 v2.1.126 对比我们之前实测数据的交叉验证
>
> **结论**: 之前实测看到的行为是 CC 代码里刻意设计的路径，不是协议副作用。naozhi 可以完全复刻这个机制。

## 1. 核心数据结构

**`src/utils/messageQueueManager.ts`** — 进程级单例 command 队列：

```typescript
const commandQueue: QueuedCommand[] = []
```

三种优先级：`'now' | 'next' | 'later'`；FIFO within priority。`'now'` 优先级的消息到达会触发**当前 turn 立即中断**（`subscribeToCommandQueue` 回调调 `abortController.abort('interrupt')`，print.ts:1859-1863）。

## 2. Stdin User Message 的进队列路径

**`src/cli/print.ts:4055-4122`**：

```typescript
for await (const message of structuredIO.structuredInput) {
  // ... control_request / control_response 处理 ...
  if (message.type !== 'user') continue
  // 去重检查
  enqueue({
    mode: 'prompt' as const,
    value: await resolveAndPrepend(message, message.message.content),
    uuid: message.uuid,
    priority: message.priority,  // 来自 stdin payload，默认 'next'
  })
  void run()
}
```

stdin 来的 `{type:"user",message:{content:[...]}}` 被直接 enqueue 到全局 commandQueue，优先级默认 `'next'`。**没有走任何 control_request 通道**。

## 3. Turn 启动时的合并消费（对应我们 T3/T6 实测"合并"现象）

**`src/cli/print.ts:1934-1961`**（`drainCommandQueue` 内）：

```typescript
while ((command = dequeue(isMainThread))) {
  // ...
  const batch: QueuedCommand[] = [command]
  if (command.mode === 'prompt') {
    while (canBatchWith(command, peek(isMainThread))) {
      batch.push(dequeue(isMainThread)!)
    }
    if (batch.length > 1) {
      command = {
        ...command,
        value: joinPromptValues(batch.map(c => c.value)),
        uuid: batch.findLast(c => c.uuid)?.uuid ?? command.uuid,
      }
    }
  }
  // batch 合并成一条 value 送进 ask() 启动新 turn
}
```

**这就是我们 T3/T6 看到的**：`turn 启动前 stdin 堆积多条 → drainCommandQueue 一次性 dequeue + joinPromptValues 合并成一条 prompt`。

## 4. Turn 进行中的 Mid-Turn 注入（关键发现）

**`src/query.ts:1535-1643`**（tool_use 完成后、递归进入下一轮 LLM 调用前）：

```typescript
// Be careful to do this after tool calls are done, because the API
// will error if we interleave tool_result messages with regular user messages.

const queuedCommandsSnapshot = getCommandsByMaxPriority(
  sleepRan ? 'later' : 'next',
).filter(cmd => {
  if (isSlashCommand(cmd)) return false
  if (isMainThread) return cmd.agentId === undefined
  return cmd.mode === 'task-notification' && cmd.agentId === currentAgentId
})

for await (const attachment of getAttachmentMessages(
  null, updatedToolUseContext, null,
  queuedCommandsSnapshot,
  [...messagesForQuery, ...assistantMessages, ...toolResults],
  querySource,
)) {
  yield attachment
  toolResults.push(attachment)
}

// Remove only commands that were actually consumed as attachments.
const consumedCommands = queuedCommandsSnapshot.filter(
  cmd => cmd.mode === 'prompt' || cmd.mode === 'task-notification',
)
if (consumedCommands.length > 0) {
  // ...
  removeFromQueue(consumedCommands)
}
```

**关键时机**：这段代码位于 tool_result 全部收集完成后，下一个 API 调用发起前。正好是"模型还没开始下一轮推理"的窗口。

## 5. Mid-Turn 消息的实际 API Payload 构造

**`src/utils/attachments.ts:1046-1083`** (`getQueuedCommandAttachments`)：

```typescript
return {
  type: 'queued_command' as const,
  prompt,  // 用户发的原始文本或 content blocks
  source_uuid: _.uuid,
  // ...
}
```

**`src/utils/messages.ts:3739-3795`** (`queued_command` → UserMessage)：

```typescript
case 'queued_command': {
  // ...
  return wrapMessagesInSystemReminder([
    createUserMessage({
      content: wrapCommandText(String(attachment.prompt), origin),
      // ...
    }),
  ])
}
```

**`src/utils/messages.ts:5496-5512`** (`wrapCommandText`)：

```typescript
case 'human':
case undefined:
default:
  return `The user sent a new message while you were working:\n${raw}\n\nIMPORTANT: After completing your current task, you MUST address the user's message above. Do not ignore it.`
```

**`src/utils/messages.ts:3097-3099`** (`wrapInSystemReminder`)：

```typescript
export function wrapInSystemReminder(content: string): string {
  return `<system-reminder>\n${content}\n</system-reminder>`
}
```

### 最终模型看到的 user message 内容

用户在 mid-turn 发了一句 `别动 production` ，模型实际收到的是：

```
<system-reminder>
The user sent a new message while you were working:
别动 production

IMPORTANT: After completing your current task, you MUST address the user's message above. Do not ignore it.
</system-reminder>
```

## 6. 这解释了我们实测的所有现象

### T1 (带 bash tool) — 被判 prompt injection

我们 naozhi 走的是 **stream-json 直连 CLI**，和 `print.ts` 完全一样的 enqueue 路径。所以我们的 mid-turn 消息**也**会被 `wrapInSystemReminder` + `wrapCommandText` 包成 `<system-reminder>The user sent a new message while you were working:...`。

T1 模型的 thinking 原文："system-reminder that says the user sent a new message..." — **完全对应上这个 wrap 模板**，证实我们的注入确实走了和 CC TUI 一模一样的路径。

模型拒绝执行不是因为路径不对，而是 CC 的模型对 `<system-reminder>` 里**对抗性指令**（撤销原任务）保留怀疑。

### T2 (纯生成) — 第一 turn 不受影响，转下一 turn

纯生成 turn 里**没有 tool_use**，`query.ts:1535` 的 drain 点永远不会执行（没有 tool_result 要处理就递归返回）。所以注入的消息只能等下一次 `drainCommandQueue`（print.ts:1934）被合并启动，就是我们看到的"下一个 turn 独立执行"。

### T3/T6 (连发) — 合并

多条消息在 turn 启动前堆到 commandQueue；`drainCommandQueue` 的 `canBatchWith` 循环把它们合并成一条 prompt value。num_turns=1 是因为合并后变成一次 ask() 调用。

### T5 (interrupt) — 不丢 pending

`control_request` 的 `interrupt` subtype 只 `abortController.abort()`（print.ts:2842），**不碰** commandQueue。被 abort 的 turn 退出后，`drainCommandQueue` 会继续处理剩余的队列项。和我们 T5 观察到的"第二条 msg 紧接作为新 turn 执行"完全对应。

### T9 (bash 中注入"加 EXTRA") — 并入当前 turn 成功

- bash 20s 开始跑（tool_use）
- 注入在 bash 执行期间到达 commandQueue
- bash tool_result 回来时，`query.ts:1535` 的 drain 点触发
- 注入的消息包成 `<system-reminder>The user sent a new message...:\n 加个 EXTRA` 作为额外的 user content block 塞进同一 API 请求
- 模型下一轮 LLM 推理看到这条消息 → 采纳（因为是追加而非撤销）

### A1 ("Also say HELLO") — 同 T9 机制

Same path as T9. 成功是因为内容非对抗性，模型愿意照做。

### C1/C2 ("忘掉笑话改 BLUE") — 同路径但被模型拒

Same wrap + 同时机注入，但 wrap 出来的 `<system-reminder>` 包着一个"撤销并替换"指令，CC 模型在 tool_result 之后看到这种风格的 system-reminder 会判定为注入。**这是模型层面的启发式，和协议层无关**。

## 7. 时机决定成功率的真实机制

用户（你）的直觉"**时机决定成功率**"是对的。精确机制是：

| 注入时机 | 结果 |
|---|---|
| Turn 启动前 stdin 堆积 | `drainCommandQueue` 合并 → 同一 turn 单次 LLM call |
| Tool_use 执行中到达 | 等到 tool_result 回来时被 `query.ts:1535` drain 走，作为 `<system-reminder>` user block 塞进下一轮 LLM call，**进入当前 turn** |
| 纯生成 turn 中到达 | 当前 turn 没有 tool_result 触发 drain → 等当前 turn 结束 → 下一个 turn 独立处理 |
| Turn 结束后到达 | 下一个 turn 独立处理 |

所以：
- **有 tool 的 turn** 天然提供 mid-turn 注入窗口
- **纯生成 turn** 无法 mid-turn（必须等下一 turn）
- 这就是为什么 CC TUI "有时候要等一个 tool 调用结束"——**它确实在等下一次 tool_result → drain 窗口**

## 8. naozhi 复刻的可行性

**完全可行**。我们实测证实 CLI 已经在做这件事；naozhi 只需要做三件事：

1. **Stdin 直通**：新消息立即写入 CLI stdin，不等 turn 结束（当前 Collect/Interrupt 都等）
2. **不做 naozhi 侧合并**：合并已经由 CLI 的 `drainCommandQueue` 做了，再多做一层等同于污染 CLI 自己的合并判断
3. **消化 CLI 可能产出的多个 result**：直通 + CLI 内部队列意味着一段时间内可能 emit 多个 `result` 事件，naozhi 要 FIFO 匹配回发送者

### 和 CC TUI 体感对齐的行为

- **Mid-turn 成功率** = 1:1 匹配 CC TUI（同代码路径）
- **合并逻辑** = 1:1 匹配 CC TUI（CLI 内部处理）
- **Interrupt 语义** = 1:1 匹配（`/stop` 触发 abortController，不丢队列）

### 和 CC TUI 的 UX 差异（naozhi 做不到的）

- CC TUI 允许用户在输入时看到 "已排队 N 条" 和 editable preview（`popAllEditable`）
- CC TUI 的 `'now'` 优先级机制（紧急消息立刻 abort 当前 turn）—— 需要 stream-json 侧暴露 priority 字段，**目前 stdin payload schema 支持**：`enqueue(..., priority: message.priority)` (print.ts:4108)
- CC TUI 的 `popAllEditable` UP-箭头回取消息编辑 — 无对应

## 9. 实操建议

按这个分析，**实现 passthrough 不需要改 CLI 并发模型**也不需要做节流。改动量比之前 RFC 估算的小很多：

1. `dispatch/msgqueue.go` 的"排队暂存"逻辑整体删掉
2. 新消息到达 → 直接 `sess.Send(...)` 不再走 `Enqueue/ownerLoop`
3. `Process.Send` 的 `State==Running` 检查放开 → 允许并发 Send；readLoop 按 FIFO 投 result
4. 保留 `InterruptViaControl` 作 `/stop`（语义正好对应 CC TUI 的 abortController）
5. 如果想利用 `'now'` 优先级实现"紧急消息立即打断"，stdin payload 里加 `priority: 'now'`（CLI 已识别）

额外福利：stdin payload 支持 `priority` 字段（print.ts:4108），所以 naozhi 可以把用户命令分两级：
- **`/urgent 别动 production`** → `priority: 'now'` → CLI 自动 abort 当前 turn
- 普通消息 → `priority: 'next'` → 进 CLI 排队，tool 边界时并入

这直接对应到之前讨论的"/amend" / "/stop" 显式方案，而且是 CC 原生支持的。

## 10. 对之前 RFC 的修正

之前写的 RFC (`passthrough-mode.md`) 提的"naozhi 侧节流避免 CLI 合并"**是错误的假设**。合并是 CC 故意的优化（同 turn 启动前的多条 prompt 合并为单次 API 调用省 token），不应该绕过。

正确的 passthrough 定位：

| 维度 | 旧 RFC 提法 | 修正后 |
|---|---|---|
| naozhi 职责 | 做 FIFO + 节流保护 CLI 不合并 | **不节流**，透传即可；CLI 自己做合并 |
| Process.Send 并发 | `pendingSends` FIFO 匹配 | 同，但动机不同（匹配 CLI 多 result 输出） |
| Interrupt | 退出消息路径 | 保留但重定位为 `/stop` / `/urgent` |
| "Mid-turn 引用" | 不承诺 | **承诺 tool 边界注入**（CC 行为一致），纯生成 turn 不承诺 |
| 用户 UX | 每条消息独立回复 | 同 |
| 合并行为 | 手动做 | CC 自己做 |

## 11. 下一步：验证 naozhi 实测是否命中同一机制

最后一个验证：**用 `--include-partial-messages` 或某种 debug flag 抓到 CLI 发出的 API 请求 payload**，确认我们之前实测里 CLI 真的把注入包成了 `<system-reminder>`。

如果 payload 里能看到 `<system-reminder>\nThe user sent a new message while you were working:\n...` 文本，整个链路就闭环了。
