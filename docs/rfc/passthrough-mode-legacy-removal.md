# Passthrough 作为默认 — 遗留代码移除与 ACP Fallback 设计

> **状态**: Draft — 未开始实施，需要 review
> **创建**: 2026-05-06
> **依赖**: Phase C 已完成（`passthrough-mode-phase-c-report.md`）
> **阻塞**:
> - `/urgent` 命名 + 抽象 review（RFC §11.8）
> - IM 路径 `/urgent`/`/stop` agentID 硬编码（§11.9）
> - Phase D（IM 端到端 + ACP fallback + 压测）

## 1. 背景

Phase C 通过后 passthrough 模式已在 dashboard 路径验证完整可用。用户要求把 passthrough 作为默认开启选项并移除旧的 collect/interrupt 代码路径。

**关键矛盾**：ACP 协议（Gemini CLI / kiro 等）`SupportsReplay() == false`，**无法走 passthrough**，必须降级到 `sess.Send` 串行路径。遗留代码里恰好有 `ownerLoop` + `coalesce` + `sendMu` 组成的串行保护。**删遗留前必须先给 ACP 会话设计好新的 fallback 并发 gate**，否则默认切 passthrough 的一刻起，ACP 会话就处于"goroutine 无限堆积 + 丢 coalesce 优化"的半坏状态。

## 2. 现状盘点（证据）

### 2.1 ACP 协议能力

文件：`internal/cli/protocol_acp.go`

| 方法 | 返回 | 含义 |
|---|---|---|
| `SupportsPriority()` | `false` | 不支持 `"now"` 抢占 |
| `SupportsReplay()` | `false` | 不支持 `--replay-user-messages`；**决定性开关** |
| `WriteInterrupt(_, _)` | `ErrInterruptUnsupported` | 没有 RPC-level soft interrupt |
| `WriteUserMessageLocked` | 忽略 uuid + priority | 无 slot 模型 |

ACP turn 的中断机制只有 SIGINT（`proc.Interrupt()`）—— 杀整个回合，不是 CC 那种软中断。

### 2.2 现有降级点（已存在，验证过的）

1. `server/send.go:96-103 usePassthrough()` —— gate 函数
   ```go
   func usePassthrough(ctx context.Context, sess *session.ManagedSession) bool {
       if sess == nil || !sess.SupportsPassthrough() { return false }
       return dispatch.IsPassthrough(ctx)
   }
   ```

2. `server/send.go:74-85 sendWithBroadcastPriority` 的 switch
   ```go
   switch {
   case usePassthrough(ctx, sess):
       result, err = sess.SendPassthrough(...)   // Claude stream-json
   case priority == "now":
       sess.InterruptViaControl()                 // ACP: 返回 Unsupported
       result, err = sess.Send(...)
   default:
       result, err = sess.Send(...)               // ACP 正常落点
   }
   ```

3. `cli/passthrough.go:40` —— 硬保险
   ```go
   if !p.protocol.SupportsReplay() {
       return nil, fmt.Errorf("passthrough: protocol %s does not support replay", ...)
   }
   ```
   正常上游已降级到不了这里；留作防御性错误。

4. `session/managed.go:460-466 SupportsPassthrough()` —— 透传 process 能力

### 2.3 ACP 会话目前在 passthrough 模式下的实际行为

假设用户连发 3 条消息到一个 ACP session：

```
BuildHandler (dispatch.go:263)
  → Mode()==ModePassthrough 命中
  → go sendAndReply(WithPassthrough(ctx))        ×3 (独立 goroutine)
  → sendWithBroadcastPriority (via sendFn)
  → usePassthrough=false (ACP)
  → sess.Send (被 sendMu 序列化)                 ×3 (串行)
```

**观察结果**：
- ✅ `sendMu` 保证同一 session 不会并发 Send
- ❌ **没有 coalesce**：3 条消息变成 3 个独立 RPC turn，而不是旧 collect 下的"第 1 条独立跑 + 第 2+3 合并"
- ❌ **没有 MaxDepth 限流**：`MessageQueue.Enqueue` 根本没被调用（passthrough 分支直接 `go`），goroutine 阻塞在 `sendMu` 上无上限堆积
- ❌ **没有 panic 保护**：`ownerLoop` 的 deferred recover 不在 `sendAndReply` 里
- ❌ **没有 discardQueue on /new**：`d.discardQueue` 只清 MessageQueue.msgs，对这些"游离 goroutine"不生效

即**当前默认不切 passthrough，问题只在 dashboard；一旦切 passthrough 默认开，IM 的 ACP 会话就暴露以上所有问题**。

### 2.4 /urgent 对 ACP 的语义错位

`dispatch/commands.go:180-207 handleUrgentCommand`

```go
go d.sendAndReply(WithUrgent(WithPassthrough(ctx)), key, text, nil, ...)
```

走到 `sendWithBroadcastPriority`：
- `dispatch.IsUrgent(ctx)` → priority="now"
- `usePassthrough(ctx, sess)` → **false**（ACP）
- 进 `priority=="now"` 分支：`sess.InterruptViaControl()` → ACP 返回 `ErrInterruptUnsupported` → outcome=InterruptUnsupported（不 fatal）→ `sess.Send(...)`

**用户感知**：发 `/urgent 紧急任务`，以为会抢占当前回合，实际是**排在 sendMu 后面等当前回合跑完再跑**，和普通消息无区别。

## 3. 删除 collect/interrupt 代码对 ACP 的破坏面

| 要删的 | 对 Claude 路径 | 对 ACP 路径 |
|---|---|---|
| `MessageQueue.msgs` + `Enqueue` 排队分支 | 无影响（已不走） | 无直接影响（passthrough 下也已不走） |
| `dispatch.ownerLoop` / `server.ownerLoop` | 无影响 | **有影响** — panic recovery 没了 |
| `dispatch.CoalesceMessages` | 无影响 | **UX 回退** — 连发不合并 |
| `sessionSendLegacy`（server/send.go） | 无影响 | 无影响 — 是 tests-only 的 fallback |
| `ModeCollect` / `ModeInterrupt` | 无影响 | 无影响 |
| `sess.Send` / `runTurn` / `sendMu` | 保留 | **必须保留** — ACP 的唯一出路 |
| `ACPProtocol` 全部 | 保留 | 当然保留 |
| `InterruptViaControl` / `InterruptSession` | 保留 | 保留（/stop 入口） |

## 4. 设计：ACP Fallback 路径

目标：在 passthrough 默认开启、collect/interrupt 代码删除的前提下，ACP 会话仍有合理的：
1. **并发限流**（goroutine 不能无限堆）
2. **回合顺序语义**（sendMu 已保证，不退化）
3. **连发 UX**（coalesce 或替代方案）
4. **/urgent 明确行为**（要么抢占、要么拒绝，不能静默降级）
5. **/stop 明确行为**（保留 SIGINT-based 的 full interrupt）
6. **panic recovery**（不能因为一个 send panic 把 session 锁死）

### 4.1 三条候选路线

#### 路线 A — **"passthrough-shaped fallback"（推荐）**

把 ACP 也接入到 passthrough 的 sendSlot 模型，但在 CLI 层的 fallback 做特殊处理：

```
SendPassthrough(ACP)
  → p.protocol.SupportsReplay() == false
  → 进入 ACP fallback 分支：
    → 串行写 stdin（和 Send 一样走 sendMu 或新的 pendingQueue）
    → 等待 ACP result event（ReadEvent 里已有 Type="result" 分支）
    → 填充 SendResult
```

**优点**：
- 统一 `SendPassthrough` 单一入口，dispatch/server 两边都不用再分叉
- 自动继承 `maxPendingSlots=16` 限流（复用现成的 ErrTooManyPending）
- /urgent 的语义可以统一：ACP 不支持，返回 `ErrPriorityNotSupported`，调用方显式处理
- panic recovery 可以在 `SendPassthrough` 内部加

**缺点**：
- 要给 `Process` 加一个 "ACP-mode passthrough"，本质是两套不同行为的 sendSlot 机制
- ACP 没有 replay → 没有 "fan-out 合并"，每个 slot 都是独立 turn
- 改动面比路线 C 大

**设计要点**：
```go
// In cli/passthrough.go
func (p *Process) SendPassthrough(...) (*SendResult, error) {
    if !p.protocol.SupportsReplay() {
        return p.sendPassthroughACP(ctx, text, images, onEvent, priority)
    }
    // existing code...
}

func (p *Process) sendPassthroughACP(...) (*SendResult, error) {
    if priority == "now" {
        return nil, ErrPriorityNotSupported  // 显式拒绝
    }
    // 用 maxPendingSlots 限流（len(pending) >= 16 → ErrTooManyPending）
    // 串行执行：pendingMu → WriteMessage → 等 result event → 返回
    // 没有 replay 匹配，没有 merge fan-out，纯 FIFO 单 slot 一个 turn
}
```

#### 路线 B — **"保留 ownerLoop shell for ACP only"**

保留 `server.ownerLoop` + `dispatch.ownerLoop` 的外壳，仅当 `!sess.SupportsPassthrough()` 时才走，其余一切照旧删干净。

**优点**：
- 代码改动最小
- coalesce UX 继续生效（ACP 连发仍然合并）
- panic recovery 现成

**缺点**：
- "只为 ACP 保留"的代码注定长期无人维护会腐烂
- 两套并发模型继续并存，新人读代码依然困惑
- `MessageQueue` 的 msgs/DoneOrDrain/CollectDelay 全部需要保留，等于没删多少

#### 路线 C — **"ACP 直接拒绝并发"**

最简单：`SupportsPassthrough()==false` 的 session，`MessageQueue.MaxDepth=1`（只允许 1 个 send 在跑，第 2 个 busy 拒绝）。让用户自己重试或等。

**优点**：
- 改动最少
- 代码最清晰
- 彻底绕过并发问题

**缺点**：
- IM 用户 UX 最差（连发第 2 条直接"请稍候"）
- 没有 coalesce
- 对 Gemini CLI 这种 agent 使用体验大幅下降

### 4.2 推荐方案

**路线 A — passthrough-shaped fallback**，理由：

1. Phase C 已经验证 passthrough 的 sendSlot 模型稳定（限流、状态机、panic 清理都就位）
2. 只要 `SendPassthrough` 内部分两支，上游所有代码就统一了一个入口
3. ACP 没有 replay 的事实恰好让 fallback 实现更简单（不用做 slot ↔ replay 匹配）
4. 未来若有新协议（其他 agent CLI）也支持这个抽象

### 4.3 路线 A 的详细设计

#### 新增错误 sentinel

```go
// cli/errors.go
var (
    // ErrPriorityNotSupported is returned when a caller passes priority="now"
    // or "later" to a protocol whose SupportsPriority()==false (ACP).
    ErrPriorityNotSupported = errors.New("cli: protocol does not support priority")
)
```

#### Process.SendPassthrough 分支

```go
// cli/passthrough.go
func (p *Process) SendPassthrough(ctx, text, images, onEvent, priority) (*SendResult, error) {
    if !p.Alive() { return nil, ErrProcessExited }

    if p.protocol.SupportsReplay() {
        return p.sendPassthroughReplay(ctx, text, images, onEvent, priority)
    }
    return p.sendPassthroughSerial(ctx, text, images, onEvent, priority)
}

// sendPassthroughSerial is the ACP-shaped fallback. Semantics:
//   - FIFO: each call takes a pending slot, runs strictly in enqueue order
//   - Limit:  maxPendingSlots (same cap as replay path)
//   - No priority support (rejects "now" / "later" with ErrPriorityNotSupported)
//   - No fan-out / merge (one slot = one turn)
//   - No slot UUID (ACP has no replay event to match)
func (p *Process) sendPassthroughSerial(ctx, text, images, onEvent, priority) (*SendResult, error) {
    if priority != "" && priority != "next" {
        return nil, ErrPriorityNotSupported
    }

    // Pending-slot gate reuses the same limit + FIFO mechanic as the replay
    // path, but the slot itself waits on the CLI's natural result event
    // rather than on a UUID-matched replay. This is safe because ACP only
    // emits one `result` RPC response per `session/prompt`.

    slot := &sendSlot{
        id:        p.slotIDGen.Add(1),
        text:      text,
        onEvent:   onEvent,
        resultCh:  make(chan *SendResult, 1),
        errCh:     make(chan error, 1),
        enqueueAt: time.Now(),
    }

    p.slotsMu.Lock()
    if len(p.pendingSlots) >= maxPendingSlots {
        p.slotsMu.Unlock()
        return nil, ErrTooManyPending
    }
    p.pendingSlots = append(p.pendingSlots, slot)
    p.slotsMu.Unlock()

    // ACP 是单 stream, 回合严格串行 —— 用 sendMu 保证只有队首能写 stdin，
    // 其余 slot 自旋等待。或者改成显式 condvar。
    // ... (see §4.4 for detail on coordinator goroutine)
}
```

#### Coordinator goroutine（关键）

ACP 没法做"先把 N 条都写进 stdin 让 CLI 自己排队"，因为 `WriteMessage` 会写 `session/prompt` 阻塞等 RPC response。所以 fallback 模型实际是：

```
sendPassthroughSerial:
    1. 加 slot 到 pendingSlots（尾部）
    2. signal coordinator "有新消息"
    3. 阻塞在 slot.resultCh / errCh / ctx.Done / bail timer

coordinator goroutine:
    for each new slot in FIFO:
        write stdin (WriteMessage)
        wait for result event from readLoop
        deliver to slot.resultCh
        remove slot from pendingSlots
```

Coordinator 是 Process 启动时起的单个 goroutine（类似 readLoop），只有 ACP-mode 才起。或者复用 sendMu：让每个调用者自己拿 sendMu → 写 → 等 → 放锁。后者更简单但要把 readLoop 的 ACP result 事件路由到"当前持锁者"，可以用 `currentSerialSlot atomic.Pointer[sendSlot]`。

**简化版（推荐）**：

```go
func (p *Process) sendPassthroughSerial(ctx, text, images, onEvent, priority) (*SendResult, error) {
    if priority != "" && priority != "next" {
        return nil, ErrPriorityNotSupported
    }
    // 1. 入 FIFO 限流
    slot := newACPSlot(text, onEvent)
    if err := p.enqueueACPSlot(slot); err != nil {
        return nil, err  // ErrTooManyPending
    }

    // 2. 阻塞等到队首（condvar / channel）
    if err := slot.waitForFront(ctx); err != nil {
        p.removeACPSlot(slot)
        return nil, err
    }

    // 3. 拿 sendMu，写 stdin，注册当前 slot 为"result 接收者"
    p.sendMu.Lock()
    p.currentACPSlot.Store(slot)
    if err := p.protocol.WriteMessage(p.stdinWriter, text, images); err != nil {
        p.currentACPSlot.Store(nil)
        p.sendMu.Unlock()
        p.removeACPSlot(slot)
        return nil, err
    }

    // 4. 等 readLoop 把 result 路由过来
    select {
    case res := <-slot.resultCh:
        p.currentACPSlot.Store(nil)
        p.sendMu.Unlock()
        p.removeACPSlot(slot)
        return res, nil
    case err := <-slot.errCh:
        ...
    case <-ctx.Done():
        // ACP 没有软中断，只能 SIGINT 杀整个 agent —— 太重
        // 选项：把 ctx 超时做成"只取消 Go-side 等待"，ACP RPC 继续跑，
        //       结果到达时因为 slot 已释放被丢弃（或记到日志）
        ...
    case <-bail.C:
        ...
    }
}
```

#### readLoop 里 ACP result 的路由

```go
// Process.readLoop (extend existing ACP path)
if ev.Type == "result" && !p.protocol.SupportsReplay() {
    // ACP-mode serial passthrough: route to current holder
    if slot := p.currentACPSlot.Load(); slot != nil {
        res := &SendResult{
            Text:      ev.Result,
            SessionID: ev.SessionID,
            CostUSD:   ev.CostUSD,
            MergedCount: 1,  // 永远单条
        }
        deliverSlotResult(slot, res)
    }
    continue  // 不走 legacy eventCh
}
```

#### /urgent 的显式拒绝

上游（dispatch + server）在进 passthrough 分支前就判断：

```go
// dispatch/commands.go handleUrgentCommand
sess := d.router.GetSession(key)
if sess != nil && !sess.SupportsPassthrough() {
    d.replyText(ctx, msg, "当前会话后端（ACP）不支持紧急打断，请使用 /stop 取消当前回合后重发。", log)
    return
}
// ... existing passthrough dispatch
```

```go
// server/send.go sessionSend passthrough branch
if priority == "now" && !sess.SupportsPassthrough() {
    return false, "", fmt.Errorf("priority=now not supported on this backend")
}
```

#### /stop 保持现状
已经正常：`InterruptViaControl` 对 ACP 返回 `InterruptUnsupported`，`handleStopCommand` 回复"当前后端不支持软中断"。若用户确实要杀回合，未来可新增 `/kill` 走 `InterruptSession`（SIGINT）。**不在本次 scope**。

#### Coalesce 如何处理？

路线 A 下 coalesce **被彻底放弃**。ACP 连发 3 条就是 3 个独立 turn。理由：
- coalesce 本质是"把用户的连续上下文打包到一个 prompt"，这对 stream-json 有意义因为 CLI 会 replay 给模型看；对 ACP 就是单纯字符串拼接，对 Gemini CLI 的效果未必更好
- 保留 coalesce 就必须保留 `DoneOrDrain` / `CoalesceMessages` / `CollectDelay` 等一大坨代码，抵消删除收益
- ACP 用户实际连发频率低（命令行 agent 交互风格），UX 损失小

**若后续发现连发 UX 有问题**，可以在 `sendPassthroughSerial` 的 coordinator 层做"进入等锁前 250ms 窗口 coalesce 下一个 slot 到自己"的轻量合并，**本 RFC 不实现**。

#### panic recovery

`SendPassthrough` 外面的 goroutine（`dispatch.sendAndReply` / `server.runTurnPassthrough`）已经没有统一的 defer recover。需要在这两个入口各补一个：

```go
// dispatch/dispatch.go — 新的 safeSendAndReply
func (d *Dispatcher) safeSendAndReply(ctx, key, text, ...) {
    defer func() {
        if r := recover(); r != nil {
            d.handleDispatchPanic(key, msg, r)
        }
    }()
    d.sendAndReply(ctx, key, text, ...)
}
```

`handleOwnerLoopPanic` 的逻辑（清理 + notify user）基本可以复用，改名 `handleSendPanic` 即可。

### 4.4 MessageQueue 重构

路线 A 下，MessageQueue 降级为纯粹的 **"/new /clear + 通知限频"** 工具，不再持有消息队列本身：

```go
// 保留的 MessageQueue 表面：
type MessageQueue struct {
    mu              sync.Mutex
    dropNotifyLRU   *list.List
    dropNotifyIndex map[string]*list.Element
    // + 一个 sessions map 追踪 "is there any live send for this key"
    //   （给 /new 的 DiscardPassthroughPending 用）
}

// 保留的方法：
- NewMessageQueue() *MessageQueue                  // 简化签名
- Discard(key)                                      // 给 /new 用
- ShouldNotify(key) bool                            // 避免连续 "消息已收到" 刷屏
- // TryAcquire/Release —— 保留给 Guard 接口，否则老 tests 不过

// 删除：
- sessionQueue.msgs / gen / busy / interruptRequested
- Enqueue / DoneOrDrain
- CollectDelay / Mode / NewMessageQueueWithMode
- ModeCollect / ModeInterrupt（保留 ModePassthrough 但实际无人读）
```

或者干脆重命名：`MessageQueue` → `SendTracker`（语义更准），一次性把 API 清理干净。

### 4.5 Config 层

```yaml
# config.yaml
session:
  queue:
    # max_depth: 20          # 保留 — 映射到 Process.maxPendingSlots
    # mode: passthrough       # DEPRECATED — 读到 collect/interrupt 时 warn + 当成 passthrough
    # collect_delay: 500ms    # DEPRECATED — 读到时 warn 并忽略
```

`applyDefaults` 逻辑：
```go
switch cfg.Session.Queue.Mode {
case "", "passthrough":
    // OK
case "collect", "interrupt":
    slog.Warn("queue.mode is deprecated, treating as passthrough",
        "configured", cfg.Session.Queue.Mode)
    cfg.Session.Queue.Mode = "passthrough"
default:
    return fmt.Errorf("unknown queue.mode: %s", cfg.Session.Queue.Mode)
}
if cfg.Session.Queue.CollectDelay != "" {
    slog.Warn("queue.collect_delay is deprecated and ignored")
}
```

`max_depth` 字段保留并透传到 `maxPendingSlots`（之前是硬编码 16，现在让它可配）。

## 5. 实施顺序

**阶段 0 — 前置（本 RFC 外）**
- [ ] 锁定 `/urgent` 命名（RFC §11.8）
- [ ] 修 IM 路径 `/urgent`/`/stop` agentID 硬编码（§11.9）
- [ ] Phase D IM 路径真跑

**阶段 1 — ACP fallback 路径落地（本 RFC）**
- [ ] 新增 `ErrPriorityNotSupported`
- [ ] `Process.sendPassthroughSerial` + `currentACPSlot` 路由
- [ ] `readLoop` ACP result 路由到 serial slot
- [ ] `handleUrgentCommand` 前置 gate
- [ ] `server/send.go` priority=="now" 前置 gate
- [ ] 单元测试：ACP 连发 3 条顺序 / MaxPending 限流 / /urgent 拒绝 / /stop 仍工作
- [ ] 集成测试：真跑一个 ACP agent（Gemini CLI 或 mock）

**阶段 2 — Default 切换 + deprecation warn（低风险，独立 PR）**
- [ ] `applyDefaults` 把 `"collect"` 改成 `"passthrough"`
- [ ] collect/interrupt config 值 → deprecation warn
- [ ] 更新 config.example.yaml + README
- [ ] 生产观察 1~2 周

**阶段 3 — 删代码（依赖阶段 1 + 2 全绿）**
- [ ] 删 `dispatch/coalesce.go` + 测试
- [ ] 删 `dispatch.ownerLoop` + `handleOwnerLoopPanic`
- [ ] 删 `server.ownerLoop` + `handleOwnerLoopPanic`
- [ ] 删 `sessionSendLegacy`
- [ ] `MessageQueue` 瘦身到 `SendTracker`
- [ ] 删 `ModeCollect` / `ModeInterrupt` / `NewMessageQueueWithMode` / `CollectDelay`
- [ ] 删 `dispatch.QueuedMsg.EnqueueAt` 等 coalesce 相关字段
- [ ] 删对应测试文件
- [ ] 验证 ACP 回归没破

**阶段 4 — Config 硬清理（下个版本）**
- [ ] `queue.mode` 字段真删（之前只是 warn）
- [ ] `queue.collect_delay` 字段真删

## 6. 风险清单

| # | 风险 | 缓解 |
|---|---|---|
| R1 | ACP serial coordinator 死锁 | 加 bail timer + ctx.Done + 单元测试覆盖 deadlock 场景 |
| R2 | readLoop ACP result 路由到已经 ctx.Done 的 slot | `deliverSlotResult` 已有 canceled 检查；走 default 丢弃 |
| R3 | /urgent 被拒绝的用户体验差 | 明确中文错误信息 + /help 里标注 |
| R4 | 保留的 max_depth 不等于 CLI-mode 的 maxPendingSlots | 一次性对齐成可配置 int |
| R5 | coalesce UX 回退被用户抱怨 | 阶段 2 生产观察期间收集反馈，必要时补 §4.3 的轻量合并 |
| R6 | ACP session 在高负载下 goroutine 堆积（等 sendMu） | maxPendingSlots 限流兜底；超了返 ErrTooManyPending |
| R7 | 测试 flakiness：ACP fallback 无真实 agent 跑不了 | 写一个 mock ACP server（JSON-RPC 2.0 回显）放在 testdata/ |
| R8 | 老用户 config.yaml 里写着 `mode: collect`，静默切换让他们困惑 | deprecation warn 在启动时 log.Warn；README 改 |

## 7. 未决议问题

- **Q1**: ACP serial fallback 的 ctx.Done 语义？  
  当 IM 用户的 ctx 超时（Feishu 30s 回调？）时，ACP 回合可能还在跑。要 SIGINT 杀吗？还是悄悄丢弃结果？  
  **倾向**：悄悄丢弃（slot 标 canceled，result 到了直接丢）；SIGINT 的决定权留给显式 /stop。

- **Q2**: maxPendingSlots 是 Process 级还是 Session 级？  
  现在是 Process 级硬编码 16。ACP 回合可能比 Claude 慢得多（模型 init），16 可能偏大（goroutine 堆）或偏小。  
  **倾向**：保留 Process 级，阶段 1 实装时改成 config 可调。

- **Q3**: `MessageQueue` 改名 `SendTracker` 值不值得？  
  名字改动会污染 git blame，但当前 API 表面和"队列"已经无关。  
  **倾向**：改，顺手清理 import 语义；反正是大 PR。

- **Q4**: 是否在阶段 1 就引入新协议枚举 `PassthroughMode`（"replay" / "serial" / "none"）替换现在的 `SupportsReplay()` + `SupportsPriority()` 两个布尔？  
  **倾向**：不，本 RFC 克制住，扩展性留给下一个协议接入时再重构。

## 8. 相关文档

- `docs/rfc/passthrough-mode.md` — v2.2 主设计
- `docs/rfc/passthrough-mode-phase-c-report.md` — Phase C 实测报告
- `docs/rfc/passthrough-mode-validation.md` — CC CLI 实测数据
- `docs/rfc/message-queue.md` — 老的 collect/interrupt 队列设计（将被本 RFC 废弃）
