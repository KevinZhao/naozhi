# RFC: cron run adoption —— 升级不再吞掉正在跑的任务

- Issue: #2712（Epic H #2546 的 Phase 1；Phase 0 是 #2691）
- 状态: 草案 v2（v1 经两路对抗评审后重写；**方向被改掉了**，不是细节修补）
- 影响面: `internal/cli`（reconnect 时扣住结果）、`internal/cron`（in-flight 标记 + 启动收养）。无 wire 协议变化，无配置变化

## 0. v1 → v2：方向从"拉"改成"推"

v1 的整个方案建立在一句话上：「收养的零件已经全在，缺的只是谁在等结果」——**这句话是错的**。两路评审各自独立证明：等到 cron 启动时再去要结果，那时候结果已经不存在了。

| v1 的说法 | 评审发现（我逐条复核过） | 处理 |
|---|---|---|
| 「result 的正文没有被丢弃，它进了正常事件管线」 | `process_event_format.go:188-196` 的 `result` 分支**只产出 `Type`+`Cost`**，注释明写"copying ev.Result into Summary/Detail would duplicate the bubble"。正文只活在瞬时的 `clievent.Event.Result` 里，经 `deliverEvent` 非阻塞投进 1024 槽 `eventCh`（`process_readloop.go:725`）；没有 `Send` 在跑就没人排空，下一次 `Send` 的 `drainStaleEvents` 直接丢掉。**唯一的持久副本是刻意无正文的。** | 方案本体重写：结果必须在**抵达的那一刻**被扣住（§3） |
| 「收养必须在 reconnect 到第一个 result 之间的窗口里挂上观察者，这个窗口有多大是最脆的一点」 | **没有窗口。** `SpawnReconnect` 在 `wrapper.go:638` 置 flag、`:641` 就 `startReadLoop()` 然后才 return，CAS（`process_readloop.go:620`）可以在调用方执行下一条语句之前就打完。而 cron 要到 `main.go:321` 的 `WireSchedulers` 才存在，reconnect 在 `main.go:229`——中间隔着 JSONL 历史加载、transcriber/project/MCP 初始化、HTTP wireup | 判定：v1 §6.2 不是"脆"，是**已经坏了**。flag 必须从"被消费的一次性开关"变成"被填充的结果盒"（§3.1） |
| §3.2 分支 2「结果已经在 replay 里：取出并按 success 终结」 | `DrainReplay` 每次 Reconnect 只能取一次（`wrapper.go:598`），`replays` 是 `reconnectShims` 的**局部变量**，只喂 linker 走查与一条日志计数，然后丢弃；明确**没有**注入 `EventLog`（`router_shim.go:520-524`）。cron 收养时没有任何 accessor | 分支 2 改为在 `SpawnReconnect` 内**当场扣住**（§3.2），不再指望后来去捞 |
| §3.5「收养在注册 cron 条目之前占 gate」 | 不可能。`Start()` 在 `s.mu` 内 :517 入表、:542 `s.cron.Start()`、**然后**才 :570 起 goroutine 跑 reconcile——收养相对第一次 tick 不只是晚，是**无序**。且不能简单提前：`reconcileRunInflight` 卡在 `s.jobStillExists`（`run_inflight_marker.go:143`），:469 锁块之前 `s.jobs` 是空的，**每个标记都会被当孤儿丢掉** | 拆成同步 claim + 异步 await（§4） |
| §3.2「标记直到终态才删」 | `run_inflight_marker.go:134-137` 先删是**刻意的自愈**。reconcile 的 goroutine（`scheduler.go:570-574`）**没有 `recover()`**（对比 `scheduler_inflight.go:24-27` 有）。留着标记 = 收养一 panic 就崩进程、launchd 重启、标记还在 → **无界崩溃循环**，标记结构里没有 attempt/boot/age 任何上界 | 改为**有界重试**：标记记 attempt，第二次开机不再收养（§5） |
| §3.1「`CostBefore` 落盘，收养后差值归属」 | 差值在边界上**不可比**：`CostTotals` 读 `ManagedSession.spent`（per-object），收养出来的 session 由 `adoptLiveShimLocked`（`router_shim.go:249-259`）重建、`lastCumulative` 为零，于是重连后第一次 cumulative 上报被**整笔算作增量**——`after - CostBefore` 会把这个 CLI session 的全部历史都记到这次 run 上。另外 `costledger.Totals`（`delta.go:142-146`）**一个 json tag 都没有**，字段改名会静默清零磁盘上的标记 | **删掉 `CostBefore`**。收养的 run 记"成本未知"，不记一个错数字（§6） |
| §3.1「标记加 `SessionKey`」 | 写入点在 `scheduler_run.go:342`，:354 的注释明写 "key is filled in after execPrepareSpawn derives it"——那时 key 还不存在，要么第二次原子写、要么挪写入点 | **不需要这个字段**：key 是 `sessionkey.CronKey(jobID)`（`key.go:39`，jobID 的纯函数，fresh 模式同样）。收养侧自己推导。评审这条不成立，已复核 |
| §3.4「`SessionRouter` 加第四个方法」 | `SessionRouter` 有 ~15 个实现（`wireup/cron_router_adapter.go:63` + 20 个 cron 测试 fake，7 个靠嵌入 `reapRouter` 继承） | 改用**能力断言**，`CostReporter`（`agent_opts.go:67`）是现成先例：1 个生产实现、0 个测试改动（§7） |
| （未提及） | **drift 关停会先杀掉要收养的 shim**：`classifyShimState` → `shimStateDrift` → `shutdownShimViaReconnect`（`router_shim.go:388-395`），发生在 `main.go:229`，比 cron 存在更早。一次同时改了 model/effort/extra_args 的 `naozhi upgrade` 正好命中 v1 §5 的验收场景 | 列为**明确的非目标**并写进验收前提（§8.2） |
| （未提及） | gate 拿在 `execAcquireSlot` 之外会与周边簿记失同步：丢 CAS 的 tick 走 `emitOverlapSkipped`（`scheduler_run.go:428-435`）→ 收养期间每 tick 一条 overlap-skip；`RunID`/`Phase` 由 `execPopulateInflight` 填、gauge 与释放在 `runScaffold`/`runFinalizer`（`:313-318`），绕过去就是 `rangeRunningSessionIDs` 看不见 + 永久卡死 | 收养必须复用这三者，不自己拿 gate（§4.2） |
| §6.1「`onTurnDone` 的所有权没核到底」 | 核到底了：两条 reconnect 路径都是 router 持有（`router_shim.go:457`、`router_lifecycle.go:940`，都是 `r.notifyChange`），`SetOnTurnDone`（`process.go:534`）是**直接覆盖** | 「不碰 `onTurnDone`」从偏好升级为**硬约束**（§3.3） |

评审还指出 `onTurnDone` 从四个站点触发（readLoop unwind / no-output 死亡 / CAS / kill），语义是"turn 不再在跑"而非"有结果"——只挂在 CAS 上的观察者会漏掉 CLI 退出与被杀。§3.4 因此把"没有结果地结束"做成显式的第二种终态。

## 1. 要解决什么

一次 `naozhi upgrade` 会吞掉正在跑的 cron 任务。Phase 0（#2691）只做到"事后可见"：`run_inflight_marker.go:55` 在 run 开始时写标记，`reconcileRunInflight`（`:112`）在启动时把每个残留标记**读 → 删 → 记一条 `canceled` + `ErrClassInterrupted`**。

于是操作员看到"这次运行被中断了"，但那次运行的**实际结果被丢掉了**——而 CLI 进程往往还活着：shim 的存在意义就是 outlive naozhi。

**v2 的核心认识**：CLI 还活着、结果也确实回来了，丢掉它的不是 shim 也不是 cron，是 naozhi 自己——重连时没有任何人声明要这个结果，于是它被投进一个没人排空的 channel。所以第一块工作在 `internal/cli`，与 cron 无关。

## 2. 分两个 PR

| PR | 内容 | 依赖 |
|---|---|---|
| **A（本 PR）** | `internal/cli`：reconnect 时扣住这一轮的结果，让"谁完成了我的 run"在几分钟后仍可回答 | 无。**不碰 scheduler 锁，与 #2711 无交集** |
| B | `internal/cron`：标记有界重试 + 同步 claim / 异步 await + 能力断言 | PR A、#2711 步 3（`runGate`）、#2740 决议 |

分开的理由不是工作量，是**可证伪性**：PR A 有一条独立的、今天就能观察到的缺陷（重连期抵达的 result 正文无处可查），可以单独验收；PR B 的验收必须端到端重启，不该和它绑在一个 PR 里。

## 3. PR A：`adoptedTurn` 结果闩

### 3.1 为什么是"闩"而不是"等待器"

v1 要的是 `AwaitResult(ctx)`——一个后来者去等。但结果**可能在后来者存在之前就已经到了**（§0 第 2 行），所以必须反过来：**结果一到就地扣住，谁来问都能拿到**。

```go
// adoptedTurn latches the outcome of a turn that was already in flight when this
// process reconnected to a surviving shim. The Send that started the turn ran in
// a previous naozhi process, so no in-process caller owns the result — and
// nothing else keeps it: deliverEvent's non-blocking handoff drops it when no
// Send is draining eventCh, and the only durable copy (ring.EventLog) is
// deliberately turn-boundary metadata with no text. The latch is what makes
// "how did my run end?" answerable by a caller wired up long after the answer
// arrived.
type adoptedTurn struct {
	armed  atomic.Bool
	done   chan struct{}   // closed exactly once, by resolve
	settle sync.Once
	mu     sync.Mutex
	out    AdoptedOutcome
}
```

### 3.2 armed 的三种时机（全在 `SpawnReconnect` 内）

`isMidTurn` 现在把"最后一个 replay 事件是不是 result"算出来**又丢掉那个 result**。改成一次判定给出两个答案：

```go
// midTurn 与 finished 由构造互斥；两者出自同一次反向走查，因为它们是同一个事实：
// 判定"最后一个有意义的事件是 result"的那一刻，走查手上正握着那个 result。
func reconnectVerdict(replays []shim.ServerMsg, proto Protocol) (midTurn bool, finished *clievent.Event)
```

于是：

| replay 尾部 | `armed` | 闩的状态 |
|---|---|---|
| 非 result（CLI 还在跑） | true | 空，等 readLoop 填 |
| result（停机期间就跑完了） | true | **立刻用 replay 里的 result 填好** |
| 什么都没有 | false | 不武装；问它的人得到"无可收养" |

第二行就是 v1 的分支 2，成本从"新增一条 replay 保留路径"降到"少丢一个已经在手上的值"。

### 3.3 填充点：复用现有 CAS，不新增语义

`process_readloop.go:620` 的 `reconnectedMidTurn.CompareAndSwap(true, false)` **一字不改**——它负责的 Running→Ready 状态转换必须保持一次性，那是 #1778 的修复。闩在同一个分支里被填充：

```go
if ev.Type == "result" && p.reconnectedMidTurn.CompareAndSwap(true, false) {
	p.adopted.resolveResult(ev)   // 新增一行；下面的状态转换与 onTurnDone 原样
	…
}
```

三条硬约束：

- **不碰 `onTurnDone`**：router 在两条 reconnect 路径上都持有它，`SetOnTurnDone` 是直接覆盖，抢了就会静默掉 dashboard 的状态广播（§0 末行）。闩与它正交。
- **与 `Send` 互斥是免费的**：`owners := p.onTurnResult(); if len(owners) > 0 { …; return false }`（`process_readloop.go:571-584`）在 CAS **之前** return。有 `Send` 认领的 result 根本到不了闩。这不是我设计的互斥，是既有结构给的——但正因如此，**填充点必须留在 CAS 分支内**，挪到 `deliverEvent` 就会破坏它。
- **`armed` 与 `reconnectedMidTurn` 同时置位、由同一个 `if` 决定**，不允许出现"armed 了但 CAS 不会来"的组合。

### 3.4 没有结果地结束，也是一种终态

`onTurnDone` 的四个触发点里三个不带结果（readLoop unwind / no-output 死亡 / kill）。闩必须在这些路径上也被 resolve，否则问它的人会挂到 ctx 超时——而"CLI 死了"是个立刻可知的事实，不该等。

漏斗选 `readLoop` 的 defer：它在每一种退出上都跑（含 panic recover），是唯一的全覆盖点。

```go
func (p *Process) AdoptedOutcome(ctx context.Context) (AdoptedOutcome, error)
// 未 armed          → ErrNoAdoptableTurn（立即，不等）
// 已填充            → 立即返回
// armed 但未填充     → 等 result / CLI 退出 / ctx
```

`AdoptedOutcome` 带 `Ended` 字段区分 `"result"` 与 `"cli_exited"`：调用方据此决定记 success 还是 interrupted，而不是从空文本猜。

### 3.5 PR A 的验收与变异

- 重连时 turn 还在跑 → 迟到的 result **正文**可从闩取出（今天取不到，只有 Type+Cost）；
- 重连时 turn 已跑完 → 闩里直接就是 replay 里的那个 result；
- 重连时无 backlog → `ErrNoAdoptableTurn`，不阻塞；
- CLI 重连后直接死 → 闩以 `cli_exited` resolve，等待方**不挂**；
- 有 `Send` 在跑时的 result → **不进闩**（互斥），`Send` 照常拿到；
- `reconnectedMidTurn` 的 Running→Ready 与 `onTurnDone` 行为**逐字不变**（既有测试全绿即为证）。

变异实验（每条必须有测试变红）：
1. 填充点从 CAS 分支挪到 `deliverEvent` 顶部 → 「有 Send 时不进闩」必须红；
2. `reconnectVerdict` 的 `finished` 恒返回 nil → 「已跑完」那例必须红；
3. 去掉 readLoop defer 里的 resolve → 「CLI 死后不挂」必须红（用短 ctx 断言 `err` 是 `cli_exited` 而不是 `DeadlineExceeded`）；
4. `armed` 在未 midTurn 时也置 true → 「无 backlog 立即报错」必须红。

## 4. PR B：cron 侧（本 PR 不实现，先定形状）

### 4.1 启动顺序：同步 claim + 异步 await

`Start()` 的可达不变式是「**在 job map 填好之后、`s.cron.Start()`（`scheduler.go:542`）之前**」。这确实让 claim 变成同步的，与 :566-569 "async so it doesn't block Start" 的理由冲突——所以拆开：

- **claim（同步）**：`ReadDir` + 每个标记一次 gate CAS + 判分支。有界，且不碰 run store——:566 那条理由防的是 run store IO 与 per-job ReadDir，不是这个。
- **await（异步）**：等结果、写历史行、删标记。原样放在既有的 `gcWG` goroutine 里，**外加 `recover()`**（`scheduler_inflight.go:24` 是现成写法）。

### 4.2 gate 必须走既有三件套

不自己 `jobGateLock`：CAS 之后要走 `execPopulateInflight` 填 `RunID`/`Phase`（否则 `rangeRunningSessionIDs`（`scheduler_inflight.go:135`）看不见这次收养），gauge 与释放走 `runScaffold`/`runFinalizer`（`scheduler_run.go:313-318`）。收养期间被 tick 撞上会记 `emitOverlapSkipped`——这是**正确行为**（真的有一次在跑），但每 tick 一条日志，需要降噪或接受。

### 4.3 标记：有界重试，而不是留到终态

```go
// Attempts counts boots that tried to adopt this marker. Phase 0 removes the
// marker BEFORE recording precisely so a bad marker cannot be re-read every
// boot; adoption needs it to survive one boot, so the bound moves into the
// marker instead of disappearing.
Attempts int `json:"attempts,omitempty"`
```

`Attempts >= 1` 的标记不再收养，直接走今天的 interrupted 路径并删除。于是最坏情况是"多活一次开机"，而不是无界崩溃循环。

### 4.4 同一 job 两个标记

判据不用 `started_at_ms` 最大（评审指出两个都可能是陈的），用「**哪一个的 key 对应着活的可收养 turn**」——key 是 per-job 的，所以最多一个能命中；其余记 interrupted。

### 4.5 能力断言，不加第四个方法

```go
// InFlightAdopter is asserted on the router, not added to SessionRouter: that
// interface has ~15 implementations (one production, the rest cron test fakes).
// A fake that does not implement this degrades to "nothing to adopt", which is
// exactly the documented behaviour for a marker from an older naozhi.
type InFlightAdopter interface {
	AdoptInFlight(key string) (InFlightRun, bool)
}
```

`CostReporter`（`agent_opts.go:67`，断言点 `scheduler_run.go:695`）是同一手法的先例。

## 5. 明确的非目标

- **不收养 sandbox run**：`sandbox_pending.go` 自有重启对账（方向相反：它要关掉 microVM）。
- **不做跨 CLI 进程的收养**：CLI 死了就是 interrupted。shim 活着是前提。
- **不对抗 drift 关停**：一次同时改了 model/effort/extra_args 的升级会在 `main.go:229` 关掉 mid-turn 的 shim（`router_shim.go:388-395`），收养随之落到 interrupted。这是**正确的**——argv 真的变了——但它是 §8 验收场景的前提条件，必须在验收步骤里写明"不改这三项"，否则测试会以为收养坏了。已记为 §9.1。
- **不修成本归属**：见 §6。
- **不改 notify 时机**：收养完成后照常发通知。
- **不引入新持久化格式**：仍是 `runinflight/<runID>.json`，只加 `attempts`（旧标记缺字段 → 视为 0，与今天行为一致，无需迁移）。

## 6. 成本归属：明确记为未知

`CostBefore` 从方案里删掉。理由是它不可能对：收养出来的 session 由 `adoptLiveShimLocked` 重建、`lastCumulative` 为零，重连后第一次 cumulative 上报会被整笔当成增量，`after - CostBefore` 于是把整个 CLI session 的历史记到这一次 run 上。**一个错的成本数字比没有数字坏得多**——它会进 ledger、进报表，而且无从发现。

收养的 run 记成本为零并带一个显式标记（字段名待定，PR B 定）。要真正修，得让 `adoptLiveShimLocked` 从磁盘恢复 `lastCumulative`，那是 cost ledger + #2545 的范围，**另开 issue**（§9.4）。

## 7. 怎么证明 PR B 有效（形状先定）

验收（issue 已定）：**一次 `naozhi upgrade` 期间跑着的 cron run 在重启后正常完成并记 success。**

端到端做法（隔离实例，不碰线上；spare port + 临时 HOME + 临时 store）：配一个 cron 任务，CLI 用一个会睡 60s 再输出结果的 stub；手动触发，等 `runinflight/` 出现标记；重启该实例（**不改 model/effort/extra_args**，见 §5）；断言该 run 最终以 success 落进 `runs/<jobID>/`，正文是 stub 的输出，`duration_ms` 跨越重启，`runinflight/` 清空。

变异要求：把 `Attempts` 的上界去掉 → 必须有测试证明崩溃循环（注入一个会 panic 的收养，断言第二次开机记 interrupted 而不是再 panic 一次）。

## 8. 仍然悬空、需要 owner 决定的

1. **§5 的 drift 关停**：要不要给收养一个 interlock（"这个 key 正在 mid-turn，drift 关停推迟到本轮结束"）？不做的话，改了 model 的升级就一定吞掉这次 run——**与 issue 验收的措辞有张力**（验收说的是"一次 upgrade"，没说"不改配置的 upgrade"）。倾向不做：argv 变了还接着跑等于用旧配置跑，更坏。请拍。
2. **§4.2 的 overlap-skip 噪声**：收养期间每 tick 一条 `emitOverlapSkipped`。降噪（收养期间抑制）还是接受？
3. **成本未知的表达方式**：ledger 里记 0 + 标记，还是干脆不写 ledger 行？后者更诚实，但会让"每个 run 一行"的不变量出洞。
4. **`lastCumulative` 的持久化**（§6 的真正修法）需要另开 issue，本 RFC 只标注。
