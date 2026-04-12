# 消息队列策略设计文档

> **状态: 设计提案 (未实现)**
>
> 本文档是 Phase 8+ 的设计 RFC。当前代码仍使用 `sessionGuard` 丢弃忙碌时的新消息。

## 1. 问题背景

### 1.1 当前行为

Naozhi 使用 `sessionGuard`（基于 `sync.Map` 的 try-lock）实现"每个 session 同一时刻只处理一条消息"的并发控制。当 session 正在处理消息 A 时，用户发送的消息 B **被直接丢弃**，仅每 3 秒回复一次"正在处理上一条消息，请稍候..."。

```
dispatch.go:222-229:

  if !s.sessionGuard.TryAcquire(key) {
      if s.sessionGuard.ShouldSendWait(key) {
          p.Reply("正在处理上一条消息，请稍候...")
      }
      return   ← 消息 B 永远丢失
  }
```

### 1.2 为什么这是个问题

**场景一：连续补充信息（最常见）**

> 用户在飞书发送："帮我review这个PR"
> Claude 开始工作（读文件、分析代码，耗时 30-60 秒）
> 用户想到补充："重点看安全相关的部分"
> 用户又补充："特别是 SQL 注入"
> → 后两条消息全部丢失，Claude 完成的 review 没有用户期望的重点

这是 IM 交互的天然模式——人在发出第一条消息后会继续思考和补充。PC/手机端用户习惯多条短消息而非一条长消息。

**场景二：急于纠正**

> 用户："把数据库从 MySQL 迁移到 PostgreSQL"
> Claude 开始执行迁移（修改 schema、改代码、写迁移脚本...）
> 用户立刻意识到发错了："等等！别动 production 的配置"
> → 纠正消息丢失，Claude 可能已经改了不该改的文件

用户无法中断正在进行的操作，也无法修正指令。

**场景三：高频群聊**

> 群里绑定了一个 code-reviewer agent
> Alice 发："review service.go"
> Bob 5 秒后发："review handler.go"
> → Bob 的消息丢失，只有 Alice 的被处理

多人共用一个 agent 时，消息丢失率随活跃度线性增长。

**场景四：Cron 与手动冲突**

> Cron job 每 30 分钟执行 "检查服务状态"
> 恰好在 Cron 执行时用户发了条消息
> → 用户消息丢失，用户不知道是因为 Cron 占用了 session

### 1.3 影响程度

根据 IM 使用习惯估算：
- 单次 Claude 处理耗时 10-60 秒（读文件、工具调用、生成回复）
- 用户在等待期间发送补充消息的概率约 30-50%
- 当前所有补充消息 100% 丢失
- 用户无任何途径知道消息被丢弃（"请稍候"暗示消息会被处理，但实际不会）

---

## 2. 设计目标

### 必须实现
1. Session busy 时消息**不丢失**（在配置的队列深度内）
2. 提供合理的默认行为，零配置即可改善体验
3. 不引入额外 goroutine 或复杂的生命周期管理
4. Dashboard 的"发送消息"功能行为不变（reject-if-busy）

### 应该实现
5. 三种可配置的队列模式，覆盖不同使用场景
6. 用户能感知到消息已收到（入队确认）
7. Dashboard 显示排队深度

### 不做
8. 不做跨 session 的优先级队列（各 session 独立）
9. 不做消息持久化（进程重启丢弃排队消息是可接受的）
10. 不做用户级限流（属于鉴权系统的职责）

---

## 3. 三种队列模式详细设计

### 3.1 Collect 模式（合并）— 默认

#### 行为描述

当 session 正在处理消息时，后续消息被收集到队列中。当前 turn 完成后，**等待一个短暂的收集窗口**（默认 500ms），然后将所有排队消息**合并为一个 prompt** 发送给 CLI。

#### 适用场景

| 场景 | 为什么 collect 最合适 |
|------|----------------------|
| **连续补充信息** | "帮我review" + "重点看安全" + "特别是SQL注入" → 合并为一个完整的 review 请求，Claude 一次性理解全部意图 |
| **多条短消息替代一条长消息** | IM 用户习惯分段发送，合并后等价于一条完整消息 |
| **修正+补充混合** | "做X" + "等等，不要改Y" + "但Z可以动" → Claude 看到全部约束后统一执行 |
| **Cron 与手动冲突** | Cron 执行完后，用户消息自动作为下一 turn 处理 |

#### 合并格式

**单条排队消息**：原样发送，不加任何修饰。

**多条排队消息**：
```
[以下是用户在你处理上一条消息期间追加发送的内容]

[14:02] 帮我写个函数

[14:02] 要用Go

[14:03] 还有记得加测试
```

首行加系统提示 `[以下是用户在你处理上一条消息期间追加发送的内容]`，让 Claude 明确知道这些是追加消息而非独立请求。每条消息前加 `[HH:MM]` 时间戳（入队时间），消息间用空行分隔。图片按顺序拼接。

#### 为什么要 collect_delay（收集窗口）

没有 delay 的问题：
```
用户连发三条（间隔 200ms）：
  14:02:00.000 "帮我写个函数"     ← 正在处理中
  14:02:00.200 "要用Go"          ← 入队
  14:02:00.400 "还有记得加测试"    ← 入队

假设第一条 14:02:30 处理完成：
  → 立即取出 ["要用Go", "还有记得加测试"] 合并发送

这很好。但如果用户还在打字：
  14:02:30.500 "对了，不要用泛型"  ← 这条来晚了 500ms
  → 已经发出去了，又变成排队
```

加 500ms delay 后：
```
  14:02:30.000 第一条处理完成
  14:02:30.500 等待窗口期间收到 "对了，不要用泛型"
  14:02:30.500 窗口到期，合并 ["要用Go", "还有记得加测试", "对了，不要用泛型"]
  → 三条补充一次性发送，完整覆盖用户意图
```

500ms 对 IM 场景几乎无感（用户刚看到上一条回复），但能捕获 IM 用户"打完一条马上打下一条"的行为模式。

#### 时序图

```
时间轴 →
────────────────────────────────────────────────────────────────

用户      │ 发A ──────────────────── 发B ─── 发C ──── 发D ──────
          │                                                     
Naozhi    │ Enqueue(A)                Enqueue(B) Enqueue(C) Enqueue(D)
          │ isOwner=true              入队       入队        入队
          │ ↓                         ↓          ↓           
          │ sendAndReply(A)           回复"消息已收到，         
          │ ├ GetOrCreate             待完成后一并处理"        
          │ ├ sess.Send(A)            (限速3秒一次)           
          │ ├ ... Claude 工作中 ...                            
          │ ├ 回复A的结果给用户                                
          │ ↓                                                   
          │ sleep(500ms)  ←收集窗口                 Enqueue(D) 入队
          │ ↓                                                   
          │ DoneOrDrain → [B, C, D]                             
          │ coalesce → "[14:02] B\n\n[14:02] C\n\n[14:03] D"   
          │ ↓                                                   
          │ sendAndReply(合并消息)                               
          │ ├ sess.Send(合并后的文本)                            
          │ ├ ... Claude 处理合并请求 ...                       
          │ ├ 回复结果给用户                                    
          │ ↓                                                   
          │ sleep(500ms)                                        
          │ DoneOrDrain → nil                                   
          │ → 释放 ownership，退出循环                          
```

#### 优点
- **最自然的 IM 交互模型**：用户不需要把所有信息组织成一条消息
- **减少 Claude API 调用次数**：N 条补充消息 = 1 次 API turn（省成本）
- **Claude 看到完整上下文后做更好的决策**：而不是分别处理碎片信息
- **零配置即改善体验**：作为默认模式，比当前"丢弃"好很多

#### 局限
- 排队消息的回复有延迟（需要等当前 turn 完成 + collect_delay）
- 合并后 Claude 可能把多个请求混在一个回复里
- 不适合"紧急取消"场景（无法中断当前执行）

---

### 3.2 Interrupt 模式（中断）

#### 行为描述

当 session 正在处理消息时，新消息触发 **SIGINT**（软中断），CLI 中止当前生成，发出 partial result。然后**合并所有排队消息**为一个 prompt 发送（与 collect 相同的合并逻辑，但不等 collect_delay）。

与 collect 模式的区别仅在于：interrupt **主动中断当前 turn**，而不是等它自然完成。

为什么不只取最后一条？因为用户可能连发 "别动 production" + "只迁移 staging"，两条都是关键约束，丢弃任一都可能导致错误操作。

#### 适用场景

| 场景 | 为什么 interrupt 最合适 |
|------|------------------------|
| **紧急纠正** | "迁移数据库" → "等等别动！" → 立即中断迁移操作 |
| **改变方向** | "用React写" → "算了还是用Vue" → 不等 React 版本写完 |
| **探索式对话** | 用户快速尝试不同 prompt，不想等每个都完成 |
| **演示/展示** | 需要快速响应，不想等上一个长任务跑完 |

#### SoftInterrupt vs 现有 Interrupt

现有 `Interrupt()` 方法（managed.go:147-164）做两件事：
1. **取消 Send context** → Process.Send() 进入 `ctx.Done()` 分支 → `Kill()` 杀死进程
2. **发送 SIGINT** → 进程收到信号

这太激进了——进程被杀死后需要重新 spawn + `--resume`，有冷启动开销。

新增 `SoftInterrupt()` 只做第 2 步：
1. **只发送 SIGINT** → CLI 收到信号后中止当前生成
2. **不取消 context** → Process.Send() 继续等待 eventCh
3. **CLI 发出 partial result event** → Send() 正常返回
4. **进程存活** → 下一条消息直接复用，无冷启动

```go
func (s *ManagedSession) SoftInterrupt() bool {
    proc := s.process
    if proc == nil || !proc.IsRunning() {
        return false
    }
    proc.Interrupt()  // SIGINT to process group, no context cancel
    return true
}
```

#### interrupted flag 的作用

防止对同一个 turn 发送多次 SIGINT：

```
A 正在处理中
B 到达 → shouldInterrupt=true → 发 SIGINT → interrupted=true
C 到达 → shouldInterrupt=false（interrupted 已为 true）→ 不再发 SIGINT
D 到达 → shouldInterrupt=false → 不再发 SIGINT

A 返回 partial result
DoneOrDrain → [B, C, D]
coalesce → 合并 B+C+D 为一个 prompt（与 collect 相同逻辑）
interrupted 重置为 false
处理合并后的消息
```

#### 时序图

```
时间轴 →
────────────────────────────────────────────────────────────────

用户      │ 发A ──────── 发B("算了别做了") ─── 发C("改做X") ───
          │                                                     
Naozhi    │ Enqueue(A)   Enqueue(B)              Enqueue(C)
          │ isOwner=true  入队                    入队
          │ ↓             shouldInterrupt=true    shouldInterrupt=false
          │ sendAndReply(A)                       (interrupted已为true)
          │ ├ sess.Send(A)                        
          │ │  ← SoftInterrupt() → SIGINT                       
          │ │  ← CLI 中止生成，发出 partial result               
          │ ├ Send() 正常返回（partial result）                  
          │ ├ 回复 A 的 partial result 给用户                    
          │ ↓                                                   
          │ DoneOrDrain → [B, C]                                
          │ coalesce → 合并 B+C                                  
          │ ↓                                                   
          │ sendAndReply(合并消息)                               
          │ ├ sess.Send("改做X" + 之前的约束)                    
          │ ├ ... Claude 处理"改做X" ...                        
          │ ├ 回复 C 的结果给用户                               
          │ ↓                                                   
          │ DoneOrDrain → nil → 释放 ownership                  
```

#### 优点
- **最快响应用户意图变化**：不需要等当前 turn 跑完
- **进程复用**：SoftInterrupt 不杀进程，无冷启动开销
- **自然的"取消"语义**：用户发新消息 = 取消旧的
- **适合交互式探索**：快速迭代 prompt
- **不丢信息**：所有排队消息合并发送，关键约束不会丢失

#### 局限
- 依赖 CLI 正确处理 SIGINT 并发出 partial result（需要实测验证）
- 如果 CLI 不响应 SIGINT，需要等 no_output_timeout（2分钟）兜底
- partial result 可能是截断的、不完整的回复
- 被中断的 turn 浪费了已消耗的 tokens

#### 风险：SIGINT 行为验证

Claude CLI 在 stream-json 模式下收到 SIGINT 的行为需要实测确认：
- **预期**：中止当前生成，发出 `type=result` 事件（可能带部分结果）
- **风险**：如果 CLI 不发 result 事件，Send() 会一直阻塞直到 no_output_timeout
- **兜底**：watchdog 2 分钟后杀进程，owner loop 继续处理下一条消息（通过 resume 恢复）
- **未来改进**：可增加 10 秒的 "interrupt timeout"，比 no_output_timeout 短

---

### 3.3 Followup 模式（顺序）

#### 行为描述

消息按到达顺序排队，**逐条处理**，每条消息都获得独立的完整回复。

#### 适用场景

| 场景 | 为什么 followup 最合适 |
|------|------------------------|
| **多人群聊** | Alice 和 Bob 各发一条，两条都应该被处理且各自收到回复 |
| **独立任务序列** | "review A.go" + "review B.go" — 两个独立任务，合并反而混乱 |
| **Cron + 手动** | Cron 任务和用户消息都是独立请求，应该逐个处理 |
| **审计需求** | 每条消息对应一个明确的回复，方便追溯 |

#### 时序图

```
时间轴 →
────────────────────────────────────────────────────────────────

用户      │ 发A ────────── 发B ─── 发C ─────────────────────────
          │                                                     
Naozhi    │ Enqueue(A)    Enqueue(B) Enqueue(C)
          │ isOwner=true   入队       入队
          │ ↓              回复"消息已排队(#1)" 回复"消息已排队(#2)"
          │ sendAndReply(A)                                     
          │ ├ sess.Send(A)                                      
          │ ├ ... Claude 处理 A ...                             
          │ ├ 回复 A 的结果给用户                               
          │ ↓                                                   
          │ DoneOrDequeueOne → B                                
          │ sendAndReply(B)                                     
          │ ├ sess.Send(B)                                      
          │ ├ ... Claude 处理 B ...                             
          │ ├ 回复 B 的结果给用户                               
          │ ↓                                                   
          │ DoneOrDequeueOne → C                                
          │ sendAndReply(C)                                     
          │ ├ sess.Send(C)                                      
          │ ├ ... Claude 处理 C ...                             
          │ ├ 回复 C 的结果给用户                               
          │ ↓                                                   
          │ DoneOrDequeueOne → empty → 释放 ownership           
```

#### 优点
- **每条消息都有明确回复**：最可预测的行为
- **适合多人场景**：每个人的请求都被处理
- **最简单的心智模型**：先到先处理，像排队一样
- **无信息丢失**：不像 interrupt 会丢弃中间消息

#### 局限
- **最慢**：N 条消息 = N 次 CLI turn，等待时间线性增长
- **最贵**：每条消息独立消耗 tokens
- **补充信息效果差**："帮我review" + "重点看安全" 会变成两次独立请求
- **可能产生重复工作**：Claude 不知道后面还有相关消息

---

## 4. 模式对比与推荐

### 4.1 横向对比

| 维度 | Collect | Interrupt | Followup |
|------|---------|-----------|----------|
| **消息保留** | 全部保留，合并发送 | 全部保留，中断后合并发送 | 全部保留，逐条处理 |
| **响应延迟** | 等当前 turn + 500ms | 中断后立即处理 | 等前面所有消息处理完 |
| **API 成本** | 最低（N→1） | 中等（产生废 partial + 合并 turn） | 最高（N→N） |
| **适合场景** | 补充信息、日常使用 | 纠正、探索、演示 | 多人群聊、独立任务 |
| **信息丢失** | 无 | 无（合并发送） | 无 |
| **实现复杂度** | 中（需要合并逻辑） | 高（需要 SoftInterrupt） | 低（简单 FIFO） |

### 4.2 推荐配置

**个人 1v1 对话（最常见）**：
```yaml
session:
  queue:
    mode: collect          # 合并补充信息
    max_depth: 20
    collect_delay: 500ms
```

**团队群聊 / 多人共用**：
```yaml
session:
  queue:
    mode: followup         # 每人的消息都处理
    max_depth: 10
    collect_delay: 0s      # followup 模式不需要 delay
```

**交互式开发 / 演示**：
```yaml
session:
  queue:
    mode: interrupt        # 快速迭代
    max_depth: 5
    collect_delay: 0s      # interrupt 模式不需要 delay
```

---

## 5. 用户体验设计

### 5.1 入队确认消息

当消息被排队而非立即处理时，用户应该知道消息没有丢失。按模式发送不同的确认文案：

| 模式 | 确认文案 | 触发条件 |
|------|---------|---------|
| collect | "消息已收到，待当前回复完成后一并处理。" | 每 3 秒最多一次 |
| interrupt | "正在中断当前任务以处理新消息..." | 每 3 秒最多一次 |
| followup | "消息已排队 (#2)，将依次处理。" | 每 3 秒最多一次，含队列位置 |

3 秒限速是为了避免用户连发 10 条消息时收到 10 条确认。

### 5.2 队列满时的行为

当排队消息达到 `max_depth` 时，**丢弃最旧的排队消息**，新消息入队。

回复用户："消息队列已满，最早的排队消息已被替换。"（仅在丢弃发生时通知一次）

### 5.3 /new 重置时清空队列

用户发送 `/new` 时，除了重置 session，还应该**清空该 session 的排队消息**。否则旧的排队消息会在新 session 中被处理，语义不连贯。

### 5.4 Dashboard 展示

Session 列表中增加 `queue_depth` 字段，Dashboard UI 在 session 卡片上显示排队数量（如 "Queue: 3"），帮助运维人员了解负载情况。

---

## 6. 核心实现：Owner Goroutine 模式

### 6.1 为什么不用独立的消费者 goroutine

常见队列实现是"生产者入队 + 独立消费者 goroutine 出队"。不采用这种方式的原因：

| 问题 | 说明 |
|------|------|
| **生命周期管理** | 每个 session 需要一个 goroutine，session 创建/销毁时要启停 goroutine |
| **goroutine 泄漏风险** | session 被 /new 重置、TTL 过期、进程崩溃时 goroutine 可能泄漏 |
| **空闲资源浪费** | 大多数 session 大部分时间是空闲的，goroutine 白白占用内存 |
| **测试复杂度** | 异步消费者让测试难以确定性断言"什么时候消息被处理" |

### 6.2 Owner Goroutine 模式如何工作

利用已有的 goroutine——平台回调 handler 已经为每条消息分配了 goroutine：

```
Message A 的 goroutine:
  1. Enqueue(key, A) → isOwner=true（我是 owner）
  2. sendAndReply(A)     ← 处理消息 A
  3. DoneOrDrain(key)    ← 检查队列
  4. 有消息 → 合并 → sendAndReply(合并)  ← 处理排队消息
  5. DoneOrDrain(key)    ← 再次检查
  6. 无消息 → 释放 ownership → return

Message B 的 goroutine（A 处理期间到达）:
  1. Enqueue(key, B) → isOwner=false（不是 owner）
  2. 发送"消息已收到"确认
  3. return   ← goroutine 结束

Message C 的 goroutine（A 处理期间到达）:
  1. Enqueue(key, C) → isOwner=false
  2. return   ← goroutine 结束
```

关键点：
- **零额外 goroutine**：owner 就是第一条消息的 handler goroutine
- **自然退出**：队列空时 owner 退出，goroutine 结束
- **无生命周期问题**：不需要管理 goroutine 的创建和销毁

### 6.3 原子性保证

`DoneOrDrain` 方法必须是原子的：

```go
func (q *messageQueue) DoneOrDrain(key string) []queuedMsg {
    q.mu.Lock()
    defer q.mu.Unlock()

    sq := q.queues[key]
    if sq == nil || len(sq.msgs) == 0 {
        // 队列空 → 释放 ownership
        if sq != nil {
            sq.busy = false
        }
        delete(q.queues, key)
        return nil
    }

    // 队列非空 → 取出全部，保持 ownership
    msgs := sq.msgs
    sq.msgs = nil
    sq.interrupted = false
    return msgs
}
```

如果检查和释放不在同一把锁内：

```
Owner goroutine:              Enqueuer goroutine:
  检查队列 → 空
                               Enqueue(msg) → 入队成功
  释放 ownership               isOwner=false（busy 还是 true）
  → msg 永远不会被处理！       → return
```

原子操作消除了这个窗口。

---

## 7. 配置

```yaml
session:
  queue:
    # 队列模式
    # - collect:   合并排队消息为一个 prompt（默认）
    # - interrupt:  中断当前 turn，处理最新消息
    # - followup:   逐条顺序处理
    mode: collect

    # 最大排队深度。超出时丢弃最旧的排队消息。
    # 设为 0 表示不排队（退化为当前行为：丢弃 + "请稍候"）
    max_depth: 20

    # collect 模式下，当前 turn 完成后等待更多消息的时间。
    # 较长的值能捕获更多连续消息，但增加回复延迟。
    # interrupt 和 followup 模式下此值被忽略。
    collect_delay: 500ms
```

### 配置验证
- `mode` 必须是 `collect`、`interrupt`、`followup` 之一
- `max_depth` >= 0（0 = 禁用队列，退化为当前行为：丢弃 + "请稍候"）
- `collect_delay` >= 0（0 = 不等待）

### 默认值
- `mode`: `"collect"`
- `max_depth`: `20`
- `collect_delay`: `"500ms"`

### max_depth=0 的退化行为

当 `max_depth=0` 时，`Enqueue()` 在 session busy 的情况下不入队、直接返回 `(false, false)`。行为与当前 `sessionGuard.TryAcquire()` 完全一致：消息丢弃 + 限速"请稍候"。这为不想引入队列的部署提供了零风险回退路径。

```go
func (q *messageQueue) Enqueue(key string, msg queuedMsg) (isOwner, shouldInterrupt bool) {
    q.mu.Lock()
    defer q.mu.Unlock()
    sq := q.getOrCreate(key)
    if !sq.busy {
        sq.busy = true
        return true, false  // 成为 owner
    }
    // max_depth=0: 不排队，退化为丢弃
    if q.maxDepth <= 0 {
        return false, false
    }
    // 正常入队逻辑...
}
```

---

## 8. 边界情况

### 8.1 进程崩溃 / 超时

**场景**：owner goroutine 在处理消息 A 时，CLI 进程被 watchdog 杀死（no_output_timeout 或 total_timeout）。

**处理**：
1. `sendAndReply(A)` 返回错误（ErrNoOutputTimeout / ErrTotalTimeout）
2. 回复用户超时错误消息
3. Owner loop **不退出**，继续执行 `DoneOrDrain`
4. 如果有排队消息，`sendAndReply` 调用 `GetOrCreate`
5. `GetOrCreate` 检测到进程死亡 → 自动 `--resume` 创建新进程
6. 排队消息在新进程中被处理

**结果**：排队消息不会因为进程崩溃而丢失。

### 8.2 max_procs 耗尽

**场景**：所有 CLI 进程槽位都满了（默认 3 个），新 session 的消息入队。

**处理**：
- 排队消息不受 max_procs 限制（入队不需要进程）
- owner loop 在 `sendAndReply` 中调用 `GetOrCreate`
- `GetOrCreate` 尝试 evict 最旧的 idle session
- 如果仍然满 → 返回 ErrMaxProcs → 回复用户"处理已满"
- owner loop 继续 drain，下次可能有空位

### 8.3 Server 关闭

**场景**：收到 SIGTERM，server 开始 graceful shutdown。

**处理**：
1. Context 被取消
2. owner loop 中 `sess.Send()` 返回 `ctx.Err()`
3. `sendAndReply` 返回错误
4. `DoneOrDrain` 释放 ownership
5. 排队消息被丢弃（可接受——重启后用户会重新发送）

### 8.4 Dashboard 发送与 IM 队列冲突

**场景**：IM 用户有排队消息，Dashboard 操作员也想发消息。

**处理**：
- Dashboard 使用 `TryAcquire` → 返回 false（session busy）→ 返回 409
- 不影响 IM 队列
- 这是正确的行为：Dashboard 操作员看到 session 正在运行，应该等待

### 8.5 同一 chat 多个 agent 的队列隔离

**场景**：用户在同一个 chat 中发 "帮我review"（→ general agent）和 "/research 查查API" （→ research agent）。

**处理**：
- 队列 key = session key = `"platform:chatType:chatID:agentID"`
- general 和 research 是不同的 key → 不同的队列
- 两者完全独立，互不影响

### 8.6 排队消息的 Context 生命周期

**场景**：用户消息 B 通过飞书 webhook 到达，handler goroutine 的 ctx 在 webhook 超时后被取消。但 B 被入队，稍后由 A 的 goroutine 处理。

**处理**：
- 入队的消息不携带原始 ctx
- owner loop 中 `sendAndReply` 使用 server 的 lifecycle context
- 只有 server shutdown 才会取消这个 context
- **实现要求**：`Server` 结构体需新增 `ctx context.Context` 字段，在 `Start()` 时保存

### 8.7 平台 Handler 与 Owner 生命周期

**前提假设**：所有平台的 message handler 都在独立 goroutine 中运行（不受 webhook 响应超时限制）。

验证：
- **飞书 WebSocket**：消息回调已在独立 goroutine（transport_ws.go 的 onMessage）
- **飞书 Webhook**：handler 在 `go handler(msg)` 中异步执行，HTTP handler 立即返回 200
- **Slack Socket Mode**：Bolt SDK 回调在独立 goroutine
- **Discord Gateway**：消息事件回调在独立 goroutine
- **微信 iLink**：长轮询回调在独立 goroutine

因此 owner goroutine 可以安全地长时间运行（处理 N 条排队消息），不会阻塞平台的 HTTP 响应或 WebSocket 接收。

### 8.8 Followup 模式下 Owner 长时间运行

**场景**：排队了 20 条消息，owner goroutine 顺序处理所有 20 条，总计可能 30-100 分钟。

**处理**：
- 功能上无问题（goroutine 无超时限制）
- 日志需要可追溯：owner loop 每次处理新消息时更新 slog context

```go
for i := 0; ; i++ {
    log := slog.With("key", current.key, "queue_pos", i)
    log.Info("processing queued message", "text_len", len(current.text))
    s.sendAndReply(ctx, current, i == 0)
    // ...
}
```

### 8.9 Cron 消息不会与用户消息合并

Cron job 的 session key 包含 Cron 自己的 chatID（`cron:{jobID}` 或 job 绑定的 chatID + agentID），与用户的 session key 不同。因此 Cron 消息和用户消息**在不同的队列中**，不存在合并问题。

如果 Cron 和用户碰巧使用同一个 session key（例如 Cron 配置了用户 chatID），则按 queue mode 正常处理——这是运维配置的选择，不是 bug。

---

## 9. 与现有机制的交互

### 9.1 与 sendMu 的关系

`ManagedSession.sendMu` 在 session 层序列化消息发送。messageQueue 在 server 层控制并发。两者职责不同：

| 层级 | 机制 | 职责 |
|------|------|------|
| Server 层 | messageQueue (busy flag) | 决定"消息是入队还是处理" |
| Session 层 | sendMu (mutex) | 保证"同一 session 的 Send() 不并发" |

两者是 belt + suspenders 关系。正常情况下 messageQueue 已经保证不会并发 Send()，sendMu 是最后一道防线。

### 9.2 与 Dedup 的关系

Dedup 在 messageQueue 之前执行（dispatch.go:23）。重复消息在入队前就被过滤。

### 9.3 与 Cron 的关系

Cron job 使用自己的 session key（包含 job 绑定的 chatID + agentID）。通常 Cron 和用户消息在不同的队列中，互不影响（见 8.9）。

### 9.4 /stop 命令（新增）

`/stop` 是一个与 queue mode 正交的命令：在任何模式下，用户发 `/stop` 都应该**中断当前 turn，不提交新 prompt**。

行为：
1. 调用 `session.SoftInterrupt()`（或 `Interrupt()` 如果需要杀进程）
2. 清空该 session 的排队消息（`Discard(key)`）
3. 回复"已中断当前任务。"

与 interrupt mode 的区别：
- `/stop` 是显式用户命令，仅中断，不发新消息
- interrupt mode 是"新消息自动触发中断"，隐式行为

实现：在 dispatch.go 的命令解析部分（与 /new、/cd 同级）增加 `/stop` 处理。

---

## 10. 文件变更清单

| 文件 | 操作 | 变更内容 |
|------|------|---------|
| `internal/server/msgqueue.go` | **NEW** | messageQueue 类型，Enqueue/DoneOrDrain/TryAcquire/Release 等方法 |
| `internal/server/msgqueue_test.go` | **NEW** | 队列单元测试，含 `-race` 并发测试 |
| `internal/server/coalesce.go` | **NEW** | coalesceMessages() 合并逻辑 |
| `internal/server/coalesce_test.go` | **NEW** | 合并格式测试 |
| `internal/server/dispatch.go` | **MODIFY** | 提取 sendAndReply()；替换 sessionGuard 为 msgQueue；新增 processOwnerLoop()；新增 /stop 命令 |
| `internal/server/server.go` | **MODIFY** | 删除 sessionGuard 类型和字段；新增 msgQueue 字段和 ctx 字段；更新构造函数 |
| `internal/session/managed.go` | **MODIFY** | 新增 SoftInterrupt() 方法 |
| `internal/session/router.go` | **MODIFY** | 新增 SoftInterruptSession() 方法 |
| `internal/config/config.go` | **MODIFY** | 新增 QueueConfig 结构体和解析 |
| `internal/server/dashboard.go` | **MODIFY** | sessionGuard 引用改为 msgQueue |
| `internal/server/wshub.go` | **MODIFY** | guard 字段类型更新 |
| `internal/server/dashboard_test.go` | **MODIFY** | 更新测试中的 sessionGuard 引用 |

---

## 11. 实现阶段

```
Phase 1: msgqueue.go + tests           ┐
Phase 2: config.go QueueConfig          ├── 可并行开发
Phase 3: managed.go SoftInterrupt       │
Phase 4: coalesce.go + tests            ┘
                                        │
                                        ▼
Phase 5: dispatch.go 重构               ← 核心集成（依赖 1-4 全部完成）
                                        │
                                        ▼
Phase 6: server.go/dashboard/hub 重命名  ← 机械性替换
                                        │
                                        ▼
Phase 7: Dashboard queue depth 展示      ← 可选增强
```

---

## 12. 验证计划

### 自动化测试
- `go test -race ./internal/server/...` — 队列、合并的单元和并发测试
- `go test -race ./internal/session/...` — SoftInterrupt 测试
- `go vet ./...` && `go build ./...` — 编译和静态分析

### 手动测试矩阵

| 测试场景 | Collect | Interrupt | Followup |
|----------|---------|-----------|----------|
| 单条消息（无排队） | 行为与当前完全一致 | 同左 | 同左 |
| 连发 3 条消息 | 合并为 1 个 prompt | 中断 + 合并 3 条发送 | 逐条处理，3 个回复 |
| 处理中 /new | 队列清空，session 重置 | 同左 | 同左 |
| 处理中 /stop | 中断当前 turn，队列清空 | 同左 | 同左 |
| 进程超时后重试 | 超时错误 → resume → 处理排队消息 | 同左 | 同左 |
| max_depth 溢出 | 最旧消息丢弃 | 同左 | 同左 |
| max_depth=0 | 退化为当前行为（丢弃） | 同左 | 同左 |
| Dashboard 并发发送 | 返回 409 | 同左 | 同左 |
| SIGINT 后进程存活 | N/A | 验证 PID 不变 | N/A |

### SIGINT 行为验证（Interrupt 模式专项）

在合并前需要验证 Claude CLI 的 SIGINT 行为：

```bash
# 启动 stream-json 进程
claude -p --output-format stream-json --input-format stream-json --verbose

# 发送消息触发长任务
{"type":"user","message":{"role":"user","content":"写一个 1000 行的程序"}}

# 等 CLI 开始输出后发送 SIGINT
kill -INT <pid>

# 观察：
# 1. CLI 是否发出 type=result 事件？
# 2. result 内容是 partial 还是空？
# 3. 进程是否存活？
# 4. 能否继续发送下一条消息？
```

如果 CLI 不发 result 事件，需要在 Process.Send() 中增加 SIGINT 专用的短超时（10 秒），超时后回退到 Kill()。
