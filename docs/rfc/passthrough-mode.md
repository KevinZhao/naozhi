# Passthrough Mode — Claude CLI 原生队列直通

> **状态**: v2.2 设计文档（v2.1 review 后修正架构核心）
>
> **v2.2 变更**:
> - §5.2 Send 改为 turn 级所有权，append+write 原子化（修正 FIFO 错乱漏洞）
> - §6.1 匹配协议改为 turn 边界聚合（修正 mid-turn drain 时 slot 漏拿 result）
> - §5.3 Protocol 接口加 SupportsPriority 能力探测（ACP 降级）
> - §5.4/§6.4 watchdog 按 turnStartedAt 计时（修正长 turn 合并时误 timeout）
> - §6.1 合并 result fan-out 区分 head/follower（修正 IM 刷屏）
> - §6.3 reconnect 用 ErrReconnectedUnknown（修正暴力 discard）
> - §5.2 readLoop 不记录 replay user 事件到 EventLog
> - §5.2 ctx cancel 用 tombstone 策略，保留 FIFO 完整性
>
> **前身**: `passthrough-mode.md.v1-deprecated`（假设 naozhi 需要节流、节流目的是避免 CLI 合并。v1 的根本错误是误解了 CC CLI 的内部队列行为）
>
> **配套文档**:
> - `passthrough-mode-cc-tui-analysis.md` — CC TUI 代码级分析
> - `passthrough-mode-validation.md` — Phase 0 实测报告（V1-V9 全部通过）
>
> **核心论点**: Claude CLI 进程内部已经实现了完整的 command queue（`commandQueue` 单例 + `priority` 字段 + tool-边界 mid-turn drain + `wrapMessagesInSystemReminder`）。naozhi 只需要做透传，不要做合并、不要做节流、不要 coalesce。

---

## 1. 设计目标

让 naozhi 的消息处理体验**尽可能对齐 Claude Code TUI**：

1. 用户在任意时刻发送的消息**立即送达 CLI**，不被 naozhi 层暂存 / 合并
2. Turn 进行中且有 tool_use 时，新消息在 tool 边界被**当前 turn 感知**（`<system-reminder>` 包装的 attachment）
3. 纯生成 turn 中的消息等下一轮 turn（CC TUI 也是这样，不是我们的缺陷）
4. 紧急指令（新增 `/urgent`）使用 CC 原生 `priority:'now'` 立即中断当前 turn
5. Turn 启动前堆积的多条消息由 **CLI 自己合并**（节省 token，同 CC TUI）
6. 每条消息独立产生 `result`，独立回复用户（每人一条气泡 vs 合并气泡）

## 2. CC CLI 已有的能力（实测 + 代码验证）

> 以下事实均在 CC 2.1.126 源码 + `/tmp/test_*.py` 实测确认。详见 `passthrough-mode-cc-tui-analysis.md`。

| 能力 | 代码位置 | 对 naozhi 的意义 |
|---|---|---|
| stdin user message 进 `commandQueue` 单例 | `src/cli/print.ts:4102` | naozhi 的 stream-json 写入直接触发入队，**不需要 control_request** |
| Turn 启动前多条 prompt 合并 | `print.ts:1934-1961` `drainCommandQueue`+`joinPromptValues` | naozhi **不要** 再做一层 coalesce |
| Mid-turn 在 tool 边界 drain | `query.ts:1535-1643` | 带 tool 的 turn 会自动感知新消息；naozhi 不做额外调度 |
| `<system-reminder>` 包装 | `utils/messages.ts:3097` + `:5496-5512` | 模型看到的内容 = `<system-reminder>\nThe user sent a new message while you were working:\n{text}\n\nIMPORTANT: ...` |
| `priority:'now'` 自动 abort | `print.ts:1858-1863` | naozhi 可以把 `/urgent` 映射到此，省掉自己的 InterruptViaControl 路径 |
| `control_request interrupt` | `print.ts:2830-2849` | **不丢 commandQueue**，实测验证；保留作为 `/stop` |
| `--replay-user-messages` | `print.ts:4074-4087` | 实测验证不影响模型输入，仅 stdout 回显 |

## 3. 非目标（明确排除）

1. **不承诺纯生成 turn 中实时引用新消息** —— CC TUI 也做不到（没有 tool 边界 = 没有 drain 窗口）
2. **不承诺对抗性 mid-turn 指令一定生效** —— 模型的 prompt-injection 启发式会拒绝撤销/替换类指令（C1/C2 实测验证）
3. **不做 naozhi 侧 coalesce** —— CLI 自己做
4. **不自动 interrupt** —— 必须用户显式 `/stop` 或 `/urgent`（避免烧已花的 token）

## 4. 行为对照表

一条长任务（30s）+ 用户在 T=2s、T=5s 连发 2 条补充消息的场景：

| 模式 | T=0 msgA | T=2 msgB | T=5 msgC | 用户看到的气泡 / reaction |
|---|---|---|---|---|
| Collect（旧） | 开始 turn A | 进 naozhi 队列 | 进 naozhi 队列 | T=30 A 独立回复；T=30.5 合并 B+C → T=55 一条合并气泡 |
| Interrupt（旧） | 开始 turn A | control_request 中断，A 失败烧 token | 进 naozhi 队列 | 中断通知气泡 + 合并 B+C 回复 |
| **Passthrough（新，无 tool）** | 立即 stdin，CLI 启动 turn A | 立即 stdin，入 CLI commandQueue | 立即 stdin，入 CLI commandQueue | T=30 A 独立回复（head 气泡）；T=30 CLI 自动 drain 合并 B+C → T=55 head 气泡（B）带 "合并 2 条" chip，C 气泡打 "↗ 合并" reaction |
| **Passthrough + tool**（msgA 含 10s bash） | 开始 turn A bash | 立即 stdin | 立即 stdin | T=10 bash 完成时 CLI mid-turn drain → B/C 作为 `<system-reminder>` 并入 turn A；T=25 result 覆盖 A/B/C 三条 slot；A 气泡正常回复 + "合并 3 条" chip，B/C 气泡打 "↗ 合并" reaction |

**关键观察**：
- passthrough 的合并和 Collect 的合并**本质都是合并**，差别在合并点（naozhi vs CLI）和模型看到的格式（纯 prompt vs `<system-reminder>`）
- Passthrough + tool 场景最有价值：mid-turn 期间的补充消息能**立即被当前 turn 感知**，不等 30s
- 所有场景下 IM 侧体验一致：head/follower 分发避免刷屏

## 5. 架构改动

### 5.1 总览

```
旧：IM → Dispatcher → msgqueue(busy+slice+coalesce) → ownerLoop(drain+coalesce) → Process.Send(mutex) → shim → CLI
新：IM → Dispatcher → throttle(并发 Send 协调) → Process.Send(可重入+FIFO) → shim → CLI(内部 commandQueue 自行处理)
```

### 5.2 `cli/process.go` 并发模型改造（核心）

目前 `Process.Send` 用 `State==Running` 做互斥（`process.go:855-870`），拒绝并发 Send。必须放开，改成 **turn 级所有权** + **append+write 原子化**。

#### 5.2.1 数据结构

```go
type sendSlot struct {
    id       uint64
    uuid     string             // naozhi 生成的 user uuid，stdin payload 里带
    text     string             // 原文，用于合并 replay 的 text 反查匹配
    images   []ImageData
    resultCh chan *SendResult   // 由 readLoop 在 turn 结束时投递
    errCh    chan error         // proc exited / reset / reconnect

    // 状态位（只在 slotsMu 下读写）
    canceled bool               // ctx.Done 设置，readLoop pop 时跳过
    replayed bool               // 见到对应 replay event 后标记（用于重复文本去重）

    enqueueAt    time.Time
    writtenAt    time.Time       // WriteMessage 成功后戳
}

type Process struct {
    // ... 现有字段 ...

    // 并发模型
    slotsMu       sync.Mutex     // 守护以下所有 slot 相关字段
    pendingSlots  []*sendSlot    // FIFO，按 stdin 写入顺序
    slotIDGen     atomic.Uint64

    // Turn 追踪 — 所有归属当前 turn 的 slot
    // 在 system.init 时初始化为空；在 replay/merge-replay 时追加；在 result 时一次性 fan-out
    currentTurnSlots []*sendSlot
    turnStartedAt    time.Time
    inTurn           bool
}
```

**关键状态机**：
- `system.init` → 新 turn 开始（`inTurn=true`, `turnStartedAt=now`, `currentTurnSlots=[]`）
- replay event (独立) → match by uuid → append 到 `currentTurnSlots`
- replay event (合并 N 条) → match by text → append N 个 slot 到 `currentTurnSlots`
- `result` → fan-out 给 `currentTurnSlots`，从 `pendingSlots` 移除这些 slot；`inTurn=false`

**不**按 replay 事件决定所有权 — 否则 mid-turn drain 会错误覆盖 turn 原有 slot（见 v2.1 review 问题 #2）。

#### 5.2.2 Send 实现

```go
func (p *Process) Send(ctx context.Context, text string, images []ImageData, onEvent EventCallback) (*SendResult, error) {
    return p.SendWithPriority(ctx, text, images, onEvent, "")
}

func (p *Process) SendWithPriority(ctx context.Context, text string, images []ImageData,
                                    onEvent EventCallback, priority string) (*SendResult, error) {

    // 快速拒绝：Process 已死 / pending 满
    if !p.Alive() {
        return nil, ErrProcessExited
    }

    slot := &sendSlot{
        id:        p.slotIDGen.Add(1),
        uuid:      uuid.NewString(),
        text:      text,
        images:    images,
        resultCh:  make(chan *SendResult, 1),
        errCh:     make(chan error, 1),
        enqueueAt: time.Now(),
    }

    // 关键：append + write 必须在同一把锁下，否则两个并发 Send 可能
    // 在 pendingSlots 顺序和 stdin 写入顺序之间发生交错，破坏 FIFO 匹配。
    // 用 shimWMu 覆盖两个操作：它本身就是 stdin 写入的互斥锁。
    p.shimWMu.Lock()
    p.slotsMu.Lock()

    if len(p.pendingSlots) >= maxPendingSlots {   // 背压
        p.slotsMu.Unlock()
        p.shimWMu.Unlock()
        return nil, ErrTooManyPending
    }
    p.pendingSlots = append(p.pendingSlots, slot)
    p.slotsMu.Unlock()

    // 写 stdin —— shimSendLocked 走 shim 协议。
    // 注意：这里已经持有 shimWMu，不要调用再拿锁的 shimSend。
    writeErr := p.protocol.WriteUserMessageLocked(p.stdinWriter, slot.uuid, text, images, priority)
    slot.writtenAt = time.Now()
    p.shimWMu.Unlock()

    if writeErr != nil {
        // 写失败：slot 从 pendingSlots 移除，因为 CLI 根本没收到
        p.removeSlotByID(slot.id)
        return nil, fmt.Errorf("write message: %w", writeErr)
    }

    // 等 result / err / ctx
    select {
    case res := <-slot.resultCh:
        return res, nil
    case err := <-slot.errCh:
        return nil, err
    case <-ctx.Done():
        // Tombstone 策略：**不**从 pendingSlots 移除
        // 原因：如果移除，CLI 的第 K 个 result 本该给 slot K，
        //       移除后 pop 会给到 K+1，破坏 FIFO 对齐。
        // 做法：标记 canceled=true；replay/result 事件仍会匹配它，
        //       但 fan-out 时发现 canceled 就丢弃这份 result（内部 drop），
        //       不会把过期的 result 返回给已经退出的 Send。
        p.slotsMu.Lock()
        slot.canceled = true
        p.slotsMu.Unlock()
        return nil, ctx.Err()
    }
}
```

#### 5.2.3 readLoop 事件处理

```go
// system.init 事件
case "system":
    if ev.SubType == "init" {
        p.slotsMu.Lock()
        p.turnStartedAt = time.Now()
        p.inTurn = true
        p.currentTurnSlots = p.currentTurnSlots[:0]
        p.slotsMu.Unlock()
    }

// user 事件（replay 或 tool_result）
case "user":
    if !ev.IsReplay {
        // tool_result / tool_use_result — 不走 slot 匹配
        return
    }
    // Replay 事件 — naozhi 用它做 slot 匹配，不记入 EventLog。
    p.slotsMu.Lock()
    texts := extractTextBlocks(ev)
    if len(texts) == 1 {
        // 独立 replay: 按 uuid 匹配
        if slot := p.findSlotByUUIDLocked(ev.UUID); slot != nil {
            slot.replayed = true
            p.currentTurnSlots = append(p.currentTurnSlots, slot)
        }
        // 匹配不到（例如 mid-turn drain 的合并入 tool_result 的情况）
        // 继续默默忽略 — 后续 turn 的 result 还是会按 currentTurnSlots 分发
    } else {
        // 合并 replay: 按 text 内容反查 + FIFO 优先去重
        matched := p.matchMergedSlotsLocked(texts)
        p.currentTurnSlots = append(p.currentTurnSlots, matched...)
    }
    p.slotsMu.Unlock()
    return   // 不走 EventLog / onEvent 路径

// result 事件（turn 结束）
case "result":
    p.slotsMu.Lock()
    owners := p.currentTurnSlots
    p.currentTurnSlots = nil
    p.inTurn = false
    // 从 pendingSlots 移除本 turn 消费的 slot
    p.removeSlotsLocked(owners)
    p.slotsMu.Unlock()

    fanoutResult(owners, ev)
```

`fanoutResult` 实现在 §6.1 详述（head/follower 分发）。

#### 5.2.4 进程死亡清理

```go
// readLoop 退出时（cli_exited / shim EOF / panic）：
p.slotsMu.Lock()
victims := append([]*sendSlot(nil), p.pendingSlots...)
p.pendingSlots = nil
p.currentTurnSlots = nil
p.slotsMu.Unlock()

for _, s := range victims {
    // canceled 的 slot 已经没有 listener，跳过避免 errCh 被阻塞发送
    if s.canceled { continue }
    select {
    case s.errCh <- ErrProcessExited:
    default:
    }
}
```

#### 5.2.5 辅助方法

```go
// matchMergedSlotsLocked —— 按 text 内容匹配，重复文本按 FIFO 优先
// Caller 必须持有 slotsMu
func (p *Process) matchMergedSlotsLocked(texts []string) []*sendSlot {
    matched := make([]*sendSlot, 0, len(texts))
    used := make(map[uint64]bool, len(texts))
    for _, text := range texts {
        for _, slot := range p.pendingSlots {
            if slot.replayed || used[slot.id] {
                continue
            }
            if slot.text == text {
                slot.replayed = true
                used[slot.id] = true
                matched = append(matched, slot)
                break
            }
        }
    }
    return matched
}

// findSlotByUUIDLocked —— O(N) 扫，N 上限为 maxPendingSlots=16，可接受
func (p *Process) findSlotByUUIDLocked(u string) *sendSlot {
    for _, s := range p.pendingSlots {
        if s.uuid == u {
            return s
        }
    }
    return nil
}

// removeSlotsLocked —— 从 pendingSlots 剔除给定 slot（保持其他顺序）
func (p *Process) removeSlotsLocked(victims []*sendSlot) {
    if len(victims) == 0 { return }
    victimSet := make(map[uint64]bool, len(victims))
    for _, v := range victims { victimSet[v.id] = true }
    kept := p.pendingSlots[:0]
    for _, s := range p.pendingSlots {
        if !victimSet[s.id] { kept = append(kept, s) }
    }
    p.pendingSlots = kept
}
```

#### 5.2.6 锁序约定

为避免死锁：

```
shimWMu  →  slotsMu        (Send 路径)
slotsMu  →  (无下游 lock)   (readLoop 路径、cancel 路径)
```

- Send 获取 `shimWMu` 后才获取 `slotsMu`；readLoop 只获取 `slotsMu` 而不碰 `shimWMu`
- 事件处理回调（`onEvent`、`fanoutResult`、log）必须在释放 `slotsMu` 之后进行

### 5.3 `cli/event.go` + `protocol_claude.go` 扩展 uuid + priority

**InputMessage schema 扩展**：

```go
// event.go
type InputMessage struct {
    Type     string       `json:"type"`
    Message  InputContent `json:"message"`
    UUID     string       `json:"uuid,omitempty"`       // naozhi 生成，用于 replay 匹配
    Priority string       `json:"priority,omitempty"`   // "now" | "next" | "later" | ""
}

func NewUserMessage(text string, images []ImageData) InputMessage { /* existing */ }

func NewUserMessageWithMeta(text string, images []ImageData, uuid, priority string) InputMessage {
    m := NewUserMessage(text, images)
    m.UUID = uuid
    m.Priority = priority
    return m
}
```

**Protocol 接口加能力探测**（多协议兼容）：

```go
// protocol.go
type Protocol interface {
    Name() string
    BuildArgs(opts SpawnOptions) []string
    Init(rw *JSONRW, workspace, resumeID string) (string, error)

    // WriteMessage 走普通优先级，uuid 可为 ""（老调用点兼容）
    WriteMessage(w io.Writer, text string, images []ImageData) error

    // WriteUserMessageLocked —— passthrough 路径专用，caller 已持 shimWMu
    // uuid 不为空时写入 payload；priority in {"", "now", "next", "later"}
    WriteUserMessageLocked(w io.Writer, uuid, text string, images []ImageData, priority string) error

    // SupportsPriority —— 返回 false 时上层调用 priority="now" 要降级为
    // InterruptViaControl + 普通 send
    SupportsPriority() bool

    // SupportsReplay —— 返回 false 时 uuid 字段无用，匹配协议退化
    SupportsReplay() bool

    // 其他现有方法...
    WriteInterrupt(w io.Writer, requestID string) error
    ReadEvent(line string) (Event, bool, error)
    HandleEvent(w io.Writer, ev Event) bool
}
```

**ClaudeProtocol 实现**：

```go
func (p *ClaudeProtocol) BuildArgs(opts SpawnOptions) []string {
    args := []string{
        "-p",
        "--output-format", "stream-json",
        "--input-format", "stream-json",
        "--verbose",
        "--replay-user-messages",              // ← 必须，passthrough 匹配依赖
        "--setting-sources", "",
        "--dangerously-skip-permissions",
    }
    // ... 其余不变
}

func (p *ClaudeProtocol) SupportsPriority() bool { return true }
func (p *ClaudeProtocol) SupportsReplay() bool   { return true }

func (p *ClaudeProtocol) WriteUserMessageLocked(w io.Writer, uuid, text string, images []ImageData, priority string) error {
    msg := NewUserMessageWithMeta(text, images, uuid, priority)
    data, err := json.Marshal(msg)
    if err != nil { return err }
    data = append(data, '\n')
    _, err = w.Write(data)
    return err
}
```

**ACPProtocol 降级**：

```go
func (p *ACPProtocol) SupportsPriority() bool { return false }
func (p *ACPProtocol) SupportsReplay() bool   { return false }

func (p *ACPProtocol) WriteUserMessageLocked(w io.Writer, uuid, text string, images []ImageData, priority string) error {
    // ACP 没有 priority / uuid 概念，忽略参数，退化到普通 send
    return p.WriteMessage(w, text, images)
}
```

**上层降级策略**（dispatch 层）：

```go
// /urgent 处理
if sess.Protocol.SupportsPriority() {
    sess.SendWithPriority(ctx, text, images, onEvent, "now")
} else {
    // ACP 等不支持 priority 的协议降级
    sess.InterruptViaControl()          // 尽力中断当前 turn
    sess.Send(ctx, text, images, onEvent) // 作为普通新消息
}
```

**Passthrough 降级到 Collect**：

当 `SupportsReplay() == false` 时，无法做 uuid 匹配和合并检测，整个 passthrough 的 FIFO 匹配无法建立。这种 Process 自动降级：

```go
// Router/Dispatcher 层判断
if !sess.Protocol.SupportsReplay() && cfg.QueueMode == "passthrough" {
    // 该 session fall back to Collect 模式
    useLegacyQueue = true
}
```

### 5.4 `dispatch/msgqueue.go` 重构为 `sendThrottle`

**删除的东西**：

- `sessionQueue.msgs []QueuedMsg` slice（不再暂存）
- `maxDepth` eviction 逻辑
- `collectDelay` + `ParseQueueMode` 返回 ModeCollect/ModeInterrupt 的代码路径
- `CoalesceMessages` 函数（整文件 `coalesce.go` 删除）
- `ownerLoop` 里的 drain 循环
- `interruptRequested` / auto-interrupt on enqueue 语义

**保留 / 改造的东西**：

- `busy` 状态：从"是否在处理单条消息"变成"是否有活跃的 Send"——**但不再互斥**（passthrough 下可以并发 Send）。仅用于 UI 显示（"正在处理"）
- `ShouldNotify` 及 dropNotify LRU：保留，用于 IM 侧 ack 抑制
- `busy` 现在由 `len(pendingSlots) > 0` 等价计算
- `Discard`: 用于 `/new`，清空所有 pendingSlots 并给每个投递 `ErrSessionReset`

**新的 `sendThrottle` 接口**：

```go
type SendThrottle struct {
    mu      sync.Mutex
    sessions map[string]*throttleState
    dropNotify ... // 保留
}

type throttleState struct {
    activeSlots int  // 统计用，UI 显示
    lastNotify  int64
}

// Throttle 不阻塞发送！只做统计 + ShouldNotify。
// 实际排队由 Process.pendingSlots 承担。
func (t *SendThrottle) Accept(key string) (acknowledged bool) { ... }
func (t *SendThrottle) Release(key string) { ... }
func (t *SendThrottle) Depth(key string) int { ... }  // 读 Process.pendingSlots 长度
```

### 5.5 `dispatch.go` / `server/send.go` 简化

当前 IM path `BuildHandler`（dispatch.go:262-317）的 Enqueue + ownerLoop 两段式流程改为"直通 + 等待"：

```go
// Direct send — 每条消息独立 goroutine；Process.Send 内部做 FIFO
go func() {
    defer d.router.NotifyIdle()
    d.sendAndReply(ctx, key, cleanText, images, agentID, opts, msg, log, true)
}()
// 立即 ack 用户（IM reaction / dashboard 乐观气泡）
d.ackReceived(ctx, msg, log)
```

**`sendAndReply` 内部对合并 result 的处理**：

```go
func (d *Dispatcher) sendAndReply(ctx, key, text, images, ...) {
    // ... GetOrCreate session ...

    result, err := d.sendFn(ctx, key, sess, text, images, tracker.onEvent)
    if err != nil {
        // 映射错误（含 ErrReconnectedUnknown / ErrSessionReset / ErrTooManyPending）
        d.replyErrorText(ctx, msg, err, log)
        return
    }

    // v2.2: 处理合并 result
    switch {
    case result.MergedCount > 1 && result.Text == "":
        // Follower slot: 模型回复已经由 head slot 发给了另一条消息
        // 不发新 bubble；给原消息打 "↗ 合并" reaction
        d.markMessageMerged(ctx, msg, result.MergedWithHead, result.MergedCount, log)
        return
    case result.MergedCount > 1 && result.Text != "":
        // Head slot: 发完整 reply + "合并 N 条" chip
        d.replyMergedHead(ctx, msg, result, log)
        return
    default:
        // 单条独立 result，正常回复
        d.replyResult(ctx, msg, result, log)
    }
}
```

**`Process.Send` 现在是完全可重入的**（passthrough 支持并发 Send），所以每条消息独立一个 goroutine、独立 result。

### 5.5.1 `onEvent` 回调的广播语义

Turn 内的 assistant/thinking/tool_use 事件，按 **turn 内所有已匹配 slot 都收到** 广播（让每个气泡都能显示 "思考中..." / 工具链状态）：

```go
// readLoop 收到 assistant/thinking 等事件时
case "assistant", "system" /* task_started 等 */:
    p.slotsMu.Lock()
    // 快照当前 turn 的 slot 列表
    targets := append([]*sendSlot(nil), p.currentTurnSlots...)
    p.slotsMu.Unlock()

    for _, slot := range targets {
        if slot.isCanceled() || slot.onEvent == nil { continue }
        slot.onEvent(ev)  // 每个 slot 的 onEvent 都触发
    }
```

IM 层可以选择只让 head slot 的 onEvent 真实更新气泡，follower 的 onEvent noop，以避免同一份 "思考中" 状态在多个 bubble 上闪烁。但这由 dispatch 层决定，Process 只负责广播。

### 5.6 `/urgent` 指令（原生 abort）

```go
// dispatch/commands.go 新增
case "/urgent":
    rest := strings.TrimSpace(strings.TrimPrefix(text, "/urgent"))
    if rest == "" {
        replyText(ctx, msg, "用法: /urgent <紧急消息>", log)
        return true
    }
    // 写 stdin priority:'now' —— CLI 会自动 abort 当前 turn
    if err := sess.SendWithPriority(ctx, rest, nil, "now"); err != nil {
        /* ... */
    }
    return true
```

### 5.7 `/stop` 指令保留（process-level 中断）

`InterruptViaControl` **保留不删**。它是 CC CLI 原生的 `control_request interrupt` subtype（process.go:1099）。

区别：
- `/urgent <msg>` → `priority:'now'` user message → CLI drain 时 abort + 立即处理该消息
- `/stop` → control_request interrupt → CLI abort 当前 turn，**不发新消息**；pendingSlots 里的后续 Send 会继续被 CLI 消费

## 6. 边界与失败处理

### 6.1 CLI 消息合并 + Fan-out 匹配协议

**问题**：实测 V3/V7 证实 CLI 会把 turn 启动前的多条 prompt 合并成一次 API 调用，只产生一个 `result`。N 个 slot 入 pendingSlots，只会出 K ≤ N 个 result。

**已确认解决方案** (V3/V6/V7): `--replay-user-messages` + uuid 绑定 + turn 边界 fan-out。

#### 6.1.1 Replay 事件的两种形态

独立 replay（1 条消息独立启动一个 turn）:
```json
{"type": "user", "uuid": "<naozhi-uuid>", "isReplay": true,
 "message": {"role": "user", "content": [{"type": "text", "text": "..."}]}}
```

合并 replay（CLI 把多条消息合并启动一个 turn）:
```json
{"type": "user", "uuid": "<CLI-生成的新 uuid>", "isReplay": true,
 "message": {"role": "user", "content": [
   {"type": "text", "text": "msg B"},
   {"type": "text", "text": "msg C"},
   {"type": "text", "text": "msg D"}
 ]}}
```

#### 6.1.2 Turn 边界聚合（关键修正）

naozhi 不在"replay 事件到达时设置 nextResultOwner"，而是**按 turn 整体聚合**：

- `system.init` → 开启新 turn，`currentTurnSlots = []`
- replay (独立) → uuid 匹配 → append 到 `currentTurnSlots`
- replay (合并) → text 反查 → append 到 `currentTurnSlots`
- mid-turn drain 的 replay → 同上（也是同一个 turn 的一部分，自然归入）
- `result` → fan-out 给 `currentTurnSlots` 所有元素

这样同一个 turn 内的 mid-turn drain replay 不会**覆盖** turn 启动时的 slot，只是**追加**。result 一次性分发所有。

#### 6.1.3 Fan-out: Head / Follower 分发

一个 result 分给 N 个 slot 时，naozhi 不希望每个 slot 的 IM 回复都 echo 一次完整文本（会刷屏 N 次）。方案：

```go
type SendResult struct {
    Text         string   // 模型完整回复，仅 Head slot 拿到
    SessionID    string
    CostUSD      float64
    // 合并场景专用 metadata
    MergedWithHead uint64   // follower 填 head slot id；head 填 0
    MergedCount    int      // follower 填本组总数；head 也填
    HeadText       string   // follower 可选：携带 head 完整文本用于关联 UI
}

func fanoutResult(owners []*sendSlot, ev Event) {
    if len(owners) == 0 {
        // Orphan result — 没有匹配到任何 slot（理论上不应发生）
        slog.Warn("passthrough: orphan result, no slot claim", "session", ev.SessionID)
        return
    }

    head := owners[0]
    mergedCount := len(owners)

    // Head 拿完整 SendResult
    headRes := &SendResult{
        Text: ev.Result, SessionID: ev.SessionID, CostUSD: ev.CostUSD,
        MergedCount: mergedCount,
    }
    deliverToSlot(head, headRes)

    // Follower 拿空 text + metadata
    for _, slot := range owners[1:] {
        folRes := &SendResult{
            Text: "",  // 空，提示上层不要重复回复
            SessionID: ev.SessionID, CostUSD: 0,
            MergedWithHead: head.id, MergedCount: mergedCount,
            HeadText: ev.Result,   // 上层若需要做气泡联动可用
        }
        deliverToSlot(slot, folRes)
    }
}

func deliverToSlot(s *sendSlot, r *SendResult) {
    // Canceled slot: drop result，避免向已离开的 Send 发
    // canceled 需要在 slotsMu 下读，但本函数已在 slotsMu 外运行 → 重读一次
    if s.isCanceled() {
        return
    }
    select {
    case s.resultCh <- r:
    default:
        // resultCh 容量 1，理论不会阻塞；真阻塞说明 Send goroutine 已走但 canceled 未设，log + drop
        slog.Warn("passthrough: resultCh full, drop", "slot_id", s.id)
    }
}
```

#### 6.1.4 IM 侧处理

dispatch 层看 `SendResult.MergedCount`：

- `MergedCount ≤ 1`：普通 reply
- `MergedCount > 1` 且 `Text != ""`（head）：正常 reply，附 "（合并 N 条消息的统一回复）" chip
- `MergedCount > 1` 且 `Text == ""`（follower）：**不**发新气泡；对原 user 气泡打一个 "↗ 合并到上一条回复" reaction

这样用户看到：
- 3 个独立的 user 气泡（对应他发的 3 条消息）
- 1 个 bot 回复气泡（对应 head）
- 另外 2 个 user 气泡上有 "↗ 合并" 的小标记

#### 6.1.5 Orphan replay / orphan result 防御

实测中偶尔可能出现：

- **Orphan replay**：收到 replay event 但 text 一条都匹配不上（例如 panic reconnect、CLI 重发旧 commandQueue）→ 该 replay 被忽略，不 append 任何 slot
- **Orphan result**：`currentTurnSlots` 为空但收到 result → slot 已被 reconnect 清空或 replay 没跟上；打 Warn，丢弃
- **漏匹配 slot**：某个 slot 的 replay 永远没到（CLI 死亡前被吞）→ 依赖 watchdog timeout 兜底（§6.4）

### 6.2 `/new` / `/clear` 的 pendingSlots 清理

```go
func (p *Process) DiscardPending(reason error) {
    p.slotsMu.Lock()
    victims := p.pendingSlots
    p.pendingSlots = nil
    p.currentTurnSlots = nil
    p.slotsMu.Unlock()

    for _, s := range victims {
        if s.isCanceled() { continue }
        select {
        case s.errCh <- reason:
        default:
        }
    }
}
```

调用场景：
- `/new` 或 `/clear` → `DiscardPending(ErrSessionReset)`
- Process Kill / Close → `DiscardPending(ErrProcessExited)`
- 配置变更强制重建 Process → `DiscardPending(ErrSessionReset)`

### 6.3 CLI 死亡 + 重连处理

#### 6.3.1 CLI 完全死亡（cli_exited / shim EOF）

- readLoop 观察到 EOF/exit → 把所有 pending slot fan-out `ErrProcessExited`
- Dispatch 层收到错误 → IM 回复"进程意外退出，请重新发送"

#### 6.3.2 naozhi 重启后 SpawnReconnect

naozhi 自己重启（non-CLI-death）重新连回活着的 shim 时，**不知道** CLI 内部 commandQueue 还有几条待处理、哪些已处理过。盲目 `ErrProcessExited` 会让用户看到"失败"，但 CLI 实际可能正常产出。

**方案**: 重连时 pendingSlots 用 `ErrReconnectedUnknown`：

```go
var ErrReconnectedUnknown = errors.New("naozhi reconnected: processing state unknown")

// SpawnReconnect 路径
func (p *Process) handleReconnect() {
    p.slotsMu.Lock()
    victims := p.pendingSlots
    p.pendingSlots = nil
    p.currentTurnSlots = nil
    p.slotsMu.Unlock()

    for _, s := range victims {
        if s.isCanceled() { continue }
        select {
        case s.errCh <- ErrReconnectedUnknown:
        default:
        }
    }
}
```

Dispatch 层映射：

- `ErrProcessExited` → IM 回复 "❌ 处理失败，请重新发送"
- `ErrReconnectedUnknown` → IM 回复 "⚠️ 系统已重启，处理状态未知。请查看历史记录或重新发送"
- `ErrSessionReset` → IM 不额外回复（用户主动 /new，已知 intent）

### 6.4 Timeout

**v2.1 设计有 bug**：按 slot enqueueAt 计时，合并场景下 follower slot 的 enqueueAt 比 turn 启动晚，但按 turn 启动前堆积规则它们属于同一个 turn；如果 turn 合理跑 200s，follower 已经被误判 timeout。

**v2.2 修正**：watchdog 按 **turnStartedAt** 计时，不是 slot enqueueAt：

```go
// Watchdog loop（Process 级别，不是 slot 级别）
func (p *Process) watchdogLoop() {
    ticker := time.NewTicker(checkInterval)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            p.slotsMu.Lock()
            if !p.inTurn {
                p.slotsMu.Unlock()
                continue
            }
            elapsed := time.Since(p.turnStartedAt)
            exceededTotal := elapsed >= p.totalTimeout

            // noOutputTimeout 仍按最后一次 stdout event 时间
            noOutput := time.Since(p.lastStdoutAt) >= p.noOutputTimeout
            p.slotsMu.Unlock()

            if exceededTotal {
                p.killAndFanout(ErrTotalTimeout)
                return
            }
            if noOutput {
                p.killAndFanout(ErrNoOutputTimeout)
                return
            }
        case <-p.done:
            return
        }
    }
}
```

对 slot 来说，`Send` 里的 select 也要有个兜底 timeout（`totalTimeout + 30s` 防 watchdog 自己 stuck）：

```go
// Send select 加一个 timer 兜底
bailout := time.NewTimer(p.totalTimeout + 30*time.Second)
defer bailout.Stop()

select {
case res := <-slot.resultCh: return res, nil
case err := <-slot.errCh:    return nil, err
case <-ctx.Done():           /* ... tombstone ... */
case <-bailout.C:
    // Watchdog 没触发，这是纯防御
    p.slotsMu.Lock()
    slot.canceled = true
    p.slotsMu.Unlock()
    return nil, ErrOrphanedSlot
}
```

### 6.5 背压

`len(pendingSlots) > maxPendingSlots`（默认 16）→ `Send` 返回 `ErrTooManyPending`，dispatch 层映射到 `sendAckBusy`。

上限选 16 的理由：
- CC TUI 内部 commandQueue 没有显式上限；实践中单用户手速 IM 很难 >5
- naozhi 侧 16 是"异常行为防护"（goroutine 泄漏、内存爆炸）而不是业务限制
- 用户触达上限时最合理的行为是看到 "正忙，请先等候" 而不是继续堆积

### 6.6 Replay event 不写入 EventLog（新）

**v2.1 遗漏**：启用 `--replay-user-messages` 后，CLI 会把每条 stdin user echo 回来。当前 `readLoop` 会把所有 user 事件记入 EventLog，会导致 Dashboard 看到每条用户消息显示两次（乐观气泡一次，replay 回显一次）。

修正：

```go
// Event 增加字段
type Event struct {
    // ... 现有字段 ...
    IsReplay bool  // 从 JSON "isReplay" 反序列化
}

// EventEntriesFromEventAt 增加规则
case "user":
    if ev.IsReplay {
        // Replay 事件仅用于 passthrough slot 匹配（在 readLoop 里处理），
        // 不写 EventLog，不触发 onEvent 广播
        return nil
    }
    // 非 replay user = tool_result —— 保留现有逻辑
    ...
```

## 7. 实测验证清单（Phase 0 — 已完成）

**状态**: 全部 9 项验证通过。详细报告见 `passthrough-mode-validation.md`，测试脚本在 `test/e2e/passthrough/`。

| ID | 场景 | 结果 | 关键发现 |
|---|---|---|---|
| V1 | bash 期间注入非对抗追加 | **PASS (V1c)** | 路径 = CC TUI；执行率取决于内容 |
| V2 | `priority:"now"` 自动 abort | **PASS** | 10ms 级响应；`/urgent` 可直接落地 |
| V3 | 5 条连发 replay/result 对照 | **PASS** | Replay 全部带 uuid；合并 = 单个 replay event 含多个 text blocks |
| V4 | SIGKILL 时进程清理 | **PASS** | 进程干净退出；naozhi 负责 fan-out slots |
| V5 | interrupt 不丢队列 | **PASS** | `control_request interrupt` 只中断当前 turn |
| V6 | uuid round-trip | **PASS (仅在 replay=on 时)** | Result 里无 naozhi uuid；必须通过 replay 桥 |
| V7 | 合并模式观察 | **PASS** | Sequential = 5/5 独立；burst 合并到一个 turn 里 |
| V8 | 纯生成 turn 注入 | **PASS** | 延迟到下一 turn；模型重写（浪费 token） |
| V9 | 并发 stdin 写 | **PASS** | shimWMu 已足够；6 线程并发不丢消息 |

**阻断项**: 无。可进入 Phase A 实施。

## 8. 迁移策略

### 8.1 配置

```yaml
queue:
  mode: collect    # 默认不变；灰度过程中逐步切 passthrough
  # or: passthrough
  max_pending: 16   # passthrough 下生效
```

### 8.2 代码灰度

1. **Phase A — CLI 层改造 + 单测**：改 `Process.Send` 为 FIFO slot 模型；**不接入 dispatch**，只跑单测验证并发 Send 正确
2. **Phase B — Dispatch throttle 重构**：新增 `SendThrottle`，`msgqueue.MessageQueue` 保留但不再是默认路径；passthrough 配置开启时走新路径
3. **Phase C — `/urgent` + `--replay-user-messages` 接入**：验证 V2 + V3 → V6 的 uuid 匹配方案落地
4. **Phase D — IM 侧 UX**：每条独立气泡显示、"已收到" reaction 对应独立 result 的 clear、错误提示文案
5. **Phase E — 灰度 + 回滚**：passthrough 跑 1 周稳定后把 `collect` 模式整块删除

### 8.3 回滚条件

- 如果 V3（replay user messages）/ V6（uuid 绑定）方案无法稳定匹配 result → pendingSlots 会漏拿 result → 退回 Collect 模式
- 如果 `priority:"now"` 不按文档生效 → `/urgent` 退化为 `/stop` + 正常 send

## 9. UX / IM 侧设计

### 9.1 用户 IM 命令

| 指令 | 行为 | 对应 CC 原生能力 |
|---|---|---|
| 普通消息 | 直通 CLI，`priority:"next"` | `enqueue({mode:'prompt', priority:'next'})` |
| `/urgent <msg>` | 直通 CLI，`priority:"now"` | CLI 自动 abort 当前 turn + 处理此消息 |
| `/stop` | 发 `control_request interrupt` | 等价 TUI 按 ESC |
| `/new` | 清空 pendingSlots + reset session | 同 |
| `/ref <msg>`（可选） | 和普通消息一样，语义上提示用户"追加上下文"；UI 层可以显示一个气泡图标 | 无，仅 UX 区分 |

### 9.2 回复行为

**独立 result（常见）**：
- 每条用户消息 → 一个独立 bot 气泡
- 消息右下角显示 `✓` reaction（用户看到"已处理"）

**合并 result（N 条消息合并到一个 turn）**：
- Head（第一条消息）→ 正常 reply + chip "（合并 N 条消息的回复）"
- Follower（后续 N-1 条）→ **不**发新气泡；原消息打 "↗ 合并到上一条回复" reaction
- 用户体感：多个问题 → 一个综合答复 + 明显的合并标记

**排队深度 chip**：
- 侧边栏 session card 显示 `待处理 N 条` 当 `pendingSlots > 1`
- 鼠标 hover 显示每条的时间戳和首 20 字

**Tool 边界 mid-turn 注入 thinking**：
- 当前 turn 的 thinking log 含 "The user sent a new message while you were working..."
- UI 可以把该 thinking 段标记为 "👀 读到了新消息"，让用户知道 mid-turn 真的生效

**错误状态 reaction**：
- `ErrProcessExited` → 消息打 ❌ + 气泡底 "进程已退出，请重发"
- `ErrReconnectedUnknown` → 消息打 ❓ + 气泡底 "系统重启，状态未知，建议重发或查看历史"
- `ErrSessionReset` → 消息打 🗑️（用户刚 /new 过，低噪）
- `ErrTooManyPending` → 气泡底 "会话队列已满 (16/16)，请等候或 /stop"

## 10. 对 v1 RFC 的修正清单

| v1 说法 | 错误原因 | v2 修正 |
|---|---|---|
| "naozhi 需要节流避免 CLI 合并" | 合并是 CC 故意的 token 优化 | naozhi 直通；合并由 CLI 做 |
| "FIFO 严格匹配 result" | CLI 可能合并多条为一个 result | replay + uuid 匹配；FIFO 兜底 |
| "不承诺 mid-turn 引用" | 实测 + 代码确认 CC 提供了 tool 边界 drain | **部分承诺**：tool 边界生效；纯生成 turn 不承诺 |
| "/stop 是唯一中断" | CC 原生支持 `priority:'now'` | 新增 `/urgent` |
| "所有消息独立 turn" | 和 CLI 合并冲突 | 承诺"独立可路由回复"，但合并时给一份综合 result 分发 |

## 11. 开放问题

### 已决（v2.1/v2.2 关闭）

1. ~~**Uuid 匹配 vs replay 绑定**~~ → replay event 作为主索引
2. ~~**合并 result 的归属**~~ → head/follower 分发模型（§6.1.3）
3. ~~**合并 replay 重复文本鲁棒性**~~ → FIFO 优先 + `replayed` 去重（§5.2.5）
4. ~~**Slot append 与 stdin write 原子性**~~ → shimWMu 同时覆盖（§5.2.6）
5. ~~**Watchdog 按 slot vs turn 计时**~~ → 按 turnStartedAt（§6.4）
6. ~~**ctx cancel 破坏 FIFO**~~ → tombstone 策略（§5.2.2）
7. ~~**Reconnect 暴力 discard**~~ → `ErrReconnectedUnknown`（§6.3.2）
8. ~~**Replay 事件污染 EventLog**~~ → `IsReplay` 字段过滤（§6.6）
9. ~~**Priority 不是所有 protocol 都支持**~~ → `SupportsPriority` 能力探测（§5.3）

### 仍需讨论

1. **`/ref` 是否需要**：如果普通消息行为已和用户预期一致，`/ref` 可能冗余。建议 Phase D 前 kill
2. **Dashboard 显示 "X 条 pending" 粒度**：是否显示每条具体内容？隐私、权限考虑（多人 IM 群聊场景）
3. **Subagent / Task 工具中的 `agentId` scoping**：`query.ts:1571-1578` 显示 subagent 有独立 drain 过滤。naozhi 目前不用，未来 planner 可能用
4. **Follower slot 的 onEvent 广播**：是只给 head 还是全 broadcast？head-only 让 thinking 展示简单；全 broadcast 让每个 bubble 都活跃。当前设计是广播到所有 slot，由 dispatch 层决定是否 noop follower
5. **`/urgent` 自带 text 为空怎么处理**：ACP 降级路径需要 `Send("", ...)`，而 ACP `Send("")` 的语义不明确。建议 v2.2 实施阶段实测 ACP 行为后决定
6. **合并 result 的 IM UX 实现成本**：`markMessageMerged` 需要各平台 reaction 接口支持；Feishu/Slack 有，Discord 也有，Weixin 不确定。退化方案：不支持 reaction 的平台 fall back 到发一条 "这条消息已合并到上一条的回复中" 小气泡
7. **单 turn 合并 follower 数上限**：极端情况下一次合并几十条消息，fan-out 给 follower 会产生几十次 reaction call。需不需要 batch？Phase D 再观察

## 12. 附录: 与 CC TUI 的能力差距

naozhi passthrough 能做到的 CC TUI 能力：
- stream-json user message 进队列、FIFO、priority
- Tool 边界 mid-turn drain → `<system-reminder>` 包装注入
- Turn 启动前合并
- `priority:'now'` 立即中断
- `control_request interrupt` 支持
- 消息 uuid 追溯

naozhi passthrough **做不到**的：
- TUI 侧 "UP 箭头取回 pending 消息编辑"（`popAllEditable`）
- TUI 侧"视觉上看到队列里有几条"（可以由 IM 层模拟显示 `Depth()`）
- CC TUI 的 `subscribeToCommandQueue` 驱动的实时 UI 更新（dashboard 可以订阅 naozhi 自己的事件流模拟）
- Task notification / orphaned permission / skill 等非 prompt 模式（naozhi 暂无对应概念）
- TUI 的 `set_permission_mode` / 模型热切换 control_request（naozhi 不需要，/agent 命令已覆盖）

## 13. 附录: 错误类型总览

| 错误 | 触发场景 | IM 文案 | Slot 清理 |
|---|---|---|---|
| `ErrProcessExited` | cli_exited / shim EOF / readLoop panic | "进程意外退出，请重新发送" | fan-out 所有 pending |
| `ErrSessionReset` | `/new` / `/clear` / Router.Reset | 无（用户已知 intent） | fan-out 所有 pending |
| `ErrReconnectedUnknown` | SpawnReconnect 路径 | "系统已重启，状态未知，请查看历史或重发" | fan-out 所有 pending |
| `ErrTooManyPending` | pendingSlots >= maxPendingSlots | "会话队列已满 (N/N)，请等候或 /stop" | 仅当前 slot 不入队 |
| `ErrTotalTimeout` | watchdog: `now - turnStartedAt >= totalTimeout` | "⏱️ 处理超时，请拆分任务" | kill + fan-out 所有 |
| `ErrNoOutputTimeout` | watchdog: `now - lastStdoutAt >= noOutputTimeout` | "⏱️ 无输出超时" | kill + fan-out 所有 |
| `ErrOrphanedSlot` | Send 兜底 timer（理论不应触发） | "内部错误" | 仅当前 slot canceled |
| `ErrMessageTooLarge` | Write 时超过 shim 单行上限 | "消息过大，请缩短或拆分" | 仅当前 slot 拒 |
| `ErrInterruptUnsupported` | ACP 等不支持 priority 的协议走 /urgent | 走降级路径（不报错，退化为 interrupt+send） | - |

## 14. v2.2 变更摘要

对比 v2.1：

| 章节 | v2.1 问题 | v2.2 修正 |
|---|---|---|
| §5.2 Send | append + write 非原子，FIFO 可错乱 | 同 shimWMu 覆盖；tombstone 策略替代 remove |
| §5.2 所有权 | 按 replay 事件设 nextResultOwner | **按 turn 边界聚合** currentTurnSlots |
| §5.3 Protocol | 假设所有协议都有 priority | 加 SupportsPriority/SupportsReplay 能力探测 |
| §5.5 Send flow | 未考虑合并 result 的 IM 分发 | head/follower 两级分发 |
| §6.1 Fan-out | 每个 slot 拿同一份完整 result | head 拿完整 text，follower 拿 metadata |
| §6.3 Reconnect | 暴力 ErrProcessExited | ErrReconnectedUnknown，UI 区分 |
| §6.4 Watchdog | 按 slot enqueueAt 计时，合并场景误 timeout | 按 turnStartedAt |
| §6.6（新增） | readLoop 把 replay 记入 EventLog，Dashboard 双写 | IsReplay 字段过滤 |

核心修正是 **turn 边界聚合**：slot 的生命周期不再跟随 replay 事件的单点决策，而是由完整的 `init → [replay...] → result` turn 包络决定。这样：
- Mid-turn drain 不会把 turn 的 slot "赶走"
- 合并场景下所有参与者都在同一个 `currentTurnSlots` 里
- result 一次 fan-out 所有归属 slot，不会漏不会多
