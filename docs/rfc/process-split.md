# RFC: cli/Process 文件分拆(ARCH-PROCESS-SPLIT)

> **状态**: Implemented (v3 — 含落地后 bisect 备注)
> **作者**: naozhi team
> **创建**: 2026-05-10
> **修订**: 2026-05-11(v3: 补 Phase 4/5 commit 不可独立 build 的 bisect 备注)
> **范围**: `internal/cli/process.go` → 拆成 7 个文件;**零语义改动**
> **关联代码**: `internal/cli/process.go`(2464 行)、`internal/cli/passthrough.go`、`internal/cli/wrapper.go`、`internal/cli/event.go`、`internal/cli/eventlog.go`、`internal/cli/subagent_link.go`
> **解锁**: `docs/TODO.md` R67-PERF-1(ReadEvent alloc)、R71-PERF-H1(shimWriter string 拷贝)、R29-DES1(drainStaleEvents push-back invariant)

---

## 0. 修订历史

### v3 — 落地后 bisect 备注 (2026-05-11)

落地后 review 发现 **Phase 4 (`7cc7d5c`) 和 Phase 5 (`d473a5b`) commit 不可独立 build**：

- Phase 4 的 `process.go` Kill() 改为调 `signalShimShutdown(p.shimPID)`，但 `signalShimShutdown` 当时未定义（working tree 有未追踪的 `process_signal_unix.go` 掩盖了编译错误，commit 没带上该文件）。
- Phase 5 继承该 unresolved 引用。
- Phase 6 (`b548fa3`) 引入 `osutil.SendShimReload` 正式修复，与 baseline `syscall.Kill(pid, SIGUSR2)` 语义等价。

**HEAD 及此后任何 commit 均可正确 build + test**。仅 `git bisect` 或 `git log --follow` 用户走到 Phase 4/5 中间点会遇到编译错误。

**Bisect 工作流**：若需要二分回溯 process-split 相关的回归，跳过 `7cc7d5c..b548fa3~1` 区间（用 `git bisect skip` 或 `git bisect start HEAD b548fa3`）；或直接以 `b548fa3`（Phase 6）为稳定锚点对比 `27da349`（Phase 前）。

RFC §8.1 "每 phase commit 独立 build 绿" 的承诺在此两个 commit 上违反，作为事实记录在此。后续重构应把跨 phase 的依赖修复压缩在同一 commit 内。

---

### v2(2026-05-11)

按 2026-05-10 对 v1 的深度 review 修正事实错误:

- **B1 修复**:`shimMsg` struct 不是 readLoop 独用 —— `internal/cli/wrapper.go:360` 也构造 `var msg shimMsg` 用于 Spawn 握手。v2 §4.2.3 把 `shimMsg` 从"读 loop 独占"改为"跨 process_readloop + wrapper 共用",但归属仍在 `process_readloop.go`(所有权明确),wrapper.go 同 package 引用无 import 问题。
- **B3 修复**:v1 §4.2.4 声称 `EventCallback` "仅 Send 的入参类型"——真实全仓消费者跨 4 个包:`cli.passthrough.go:38`、`session.managed.go:44,50,437,528`、`session.testutil.go:16,35`、`dispatch.dispatch.go` 等。v2 承认这是**跨 package 的公共类型**,仍归 `process_send.go`(主出向路径),但在 §4.2.4 注明"多包消费者需保持签名稳定"。
- **B5 修复**:v1 §6 声称 "28 个 test 文件 / process_test.go 50+ Test"——真实 **30 个** test 文件 / `process_test.go` **15 Test** / `process_extra_test.go` **49 Test**(后者才是大头)。v2 §6 数值订正。所有 30 个 test 文件确认为 `package cli`(grep `package cli_test` 零匹配)。
- **EventCallback 类型语法修正**:v1 §4.2.4 写 `type EventCallback = func(ev Event)` 是 alias 语法,真实是 `type EventCallback func(ev Event)` 无 `=`(named type,不是 alias)。
- **B2 / B4 修复**:§4.1 九行表合计 58 与附录 A 60 的差距 —— 真实 60 个顶层 func 中有 2 个未进表(`ProcessState.String` + `sendSlot.isCanceled` 归入"Lifecycle"但没单独列)。v2 §4.1 表加上它们。
- **NITs**:常量归属表(`maxStdinLineBytes` 位置)统一、commit message 约定对齐项目现有中文标签、`git log -M90%` 相似度说明改为"对 >= 500 行大块搬迁有效"(小碎片会断)。

v1 的正文已就地修订;本节记录差异。

---

## 1. 目标

把 `internal/cli/process.go` 的 **2464 行 / 60+ 方法**按职责拆到同 package 的 7 个文件里,使得:

1. 单文件不超过 ~550 行,`Process` struct + state machine 留在主文件
2. 每个文件承担**单一职责轴**(shim I/O、readLoop、Send/Interrupt、turn 协调、事件格式化、事件查询)
3. **不改任何 export API**,外部包(`internal/session`、`internal/server`、`internal/router`)无须改 import
4. **不改任何并发原语**(`mu` / `shimWMu` / `slotsMu` / 各 atomic)
5. 现有 28 个 `internal/cli/*_test.go`(其中 `process_*_test.go` 7 个)**不用改一行**
6. 每个 phase 单独 PR/commit,`go test -race` 全绿

## 2. 非目标

| 非目标 | 原因 |
|---|---|
| 改 `*cli.Process` 的字段组织 | 超出范围;字段 padding 优化另外立 ADR |
| 改 lock 契约(shimWMu→slotsMu 顺序、mu RW 分离) | 任何一处错位都会让并发 bug 潜回;保持逐字节搬 |
| 性能优化 | R67-PERF-1 / R71-PERF-H1 / R67-PERF-9 等独立跟踪;本 RFC 专注"搬" |
| 合并成 `internal/cli/process/` 子包 | 会引爆全仓 import 路径;且同 package 已足够 |
| 拆 `passthrough.go` / `wrapper.go` | 这两个已经是可接受的粒度(passthrough ≈580 行 / wrapper ≈400 行) |
| 改 `Wrapper.Spawn` / `SpawnReconnect` 签名 | 合约由外部 router 代码依赖 |
| 合并 `process_*_test.go` | 测试分轴已合理,仅微调不值得引入 diff |

## 3. 背景

### 3.1 现状

`grep -c '^' internal/cli/process.go` → **2464 行**;`grep -n '^func ' internal/cli/process.go` 得到 **60 个顶层函数**(含方法与非方法)。按语义分组后:

| 职责轴 | 函数数 | 估计行数 |
|---|---|---|
| Lifecycle 状态 + 构造 + Kill/Close/Detach + 常量错误 | 11 | ~420 |
| shim I/O(shimWriter / encode / shimSend) | 6 | ~280 |
| readLoop + heartbeat + isChanAlive | 4 | ~500 |
| Send / Interrupt / InterruptViaControl / findResultSince + buildUserEntry | 6 | ~420 |
| drainStaleEvents | 1 | ~130 |
| 事件格式化(EventEntriesFromEvent* / FormatToolInput / parseAgentInput / shortPath / formatToolDetail / logEventAt) | 9 | ~320 |
| 事件查询 + Linker 集成 + InjectHistory + 访问器 | 18 | ~270 |
| stderr 清洗 | 1 | ~110 |
| ProcessState.String + sendSlot.isCanceled | 2 | ~30 |

**合计 60 个顶层 func**(附录 A 完整行号)。表格按职责轴分组,加上 stderr 清洗和最末两行小函数,行数估计和 process.go 的 2464 总行数差异来自 import / struct 声明 / 常量块的留白。

### 3.2 为什么现在必须拆

以下 TODO 项被"文件太大读不过来"直接阻塞:

- **R67-PERF-1**:`Protocol.ReadEvent(line []byte)` 接口改造;实现触点在 readLoop 分发处(`process.go:779`)。接口改动范围跨 3 文件 ~15 行,但 reviewer 要看清 readLoop 全貌——2464 行单文件加不上 AI review context 是硬卡点。
- **R71-PERF-H1**:`shimWriter.Write` 的 `string(data[:len-1])` 拷贝。修复要同时看 `shimWriter.Write` + `encodeShimMsg` + `shimClientMsg.Line` 的 JSON 路径,分散在 3 个地方。拆成 `process_shim_io.go` 后整条 I/O 路径可一屏装下。
- **R29-DES1**:`drainStaleEvents` 的 push-back + `goto drain` 可吞 interrupted result——需要和 readLoop 的 `eventCh` 生产者协议对齐验证,两段代码相隔 860 行。

三个 item 都不是难在逻辑,是难在**阅读成本**。文件拆分属于典型的"低风险预置工程",先交付可读性红利,不动语义。

### 3.3 现有代码已示范的拆法

同 package 下其他文件已经是"按职责轴拆"的形态,证明 Go 对同 package 多文件零成本:

- `event.go` (~300 行) — `Event` / `ContentBlock` / `NewUserMessage` / `splitAttachments` / `prependFileRefHint`
- `eventlog.go` (~500 行) — `EventLog` 状态 + persist sink + 快照查询
- `passthrough.go` (~580 行) — passthrough slot 全流水线
- `subagent_link.go` (~500 行) — Linker 上下文 + 解析 + 查询

本 RFC 让 `process.go` 家族向这种风格收敛。

## 4. 拆分方案

### 4.1 文件清单

```
internal/cli/
├── process.go              # struct + lifecycle + kill/close + 常量/错误           (~470 行)
├── process_shim_io.go      # shimWriter/encodeShimMsg/shimSend 全家               (~300 行)
├── process_readloop.go     # readLoop + heartbeatLoop + isChanAlive               (~510 行)
├── process_send.go         # Send / Interrupt / InterruptViaControl / buildUserEntry  (~360 行)
├── process_turn.go         # drainStaleEvents + findResultSince + sanitizeStderrLine  (~250 行)
├── process_event_format.go # Event→EventEntry + FormatToolInput 家族              (~340 行)
└── process_event_query.go  # EventEntries*/Linker/InjectHistory/访问器            (~230 行)
```

合计 ≈ 2460 行(原始 2464 行零净增减,仅 file-header/import 复制若干次产生几十行 noise)。

### 4.2 每个文件承担的函数清单(精确到行号)

下表基于 `grep -n '^func ' internal/cli/process.go` 的输出。行号为当前 process.go 的起始行。

#### 4.2.1 `process.go`(留守)

负责:`Process` 结构体定义、lifecycle 状态机、构造/析构、常量、错误变量、小访问器。

| 当前行号 | 签名 | 说明 |
|---|---|---|
| 141 | `func (s ProcessState) String() string` | state 枚举的 Stringer |
| 327 | `func (s *sendSlot) isCanceled() bool` | sendSlot 的一行方法;和 sendSlot struct 紧挨,不拆 |
| 341 | `func (p *Process) SetSlogKey(key string)` | slog pointer 设置,一次性构造器辅助 |
| 350 | `func (p *Process) slogger() *slog.Logger` | 被 readLoop/heartbeat/Send 共用的 logger getter,全局点 |
| 382 | `func (p *Process) setDeathReason(reason string)` | 死因 CAS;被 readLoop / heartbeat / Kill / Send 共用 |
| 403 | `func (p *Process) DeathReason() string` | 死因查询 |
| 412 | `func newShimProcess(...) *Process` | 构造器 |
| 438 | `func (p *Process) shimStdinWriter() io.Writer` | 一行 getter,和 `stdinWriter` 字段绑死 |
| 610 | `func (p *Process) startReadLoop()` | goroutine fan-out;紧贴构造语义 |
| 1351 | `func (p *Process) Alive() bool` | 一行访问器 |
| 1361 | `func (p *Process) IsRunning() bool` | 一行 RLock 访问器 |
| 1630 | `func (p *Process) Kill()` | 终结器,一次性 close |
| 1681 | `func (p *Process) Close()` | 优雅终结器 |
| 1733 | `func (p *Process) Detach()` | reconnect-friendly 终结器 |
| 2092 | `func (p *Process) GetState() ProcessState` | 一行 RLock 访问器 |
| 2108 | `func (p *Process) SetOnTurnDone(fn func())` | 回调注册 |
| 2115 | `func (p *Process) GetSessionID() string` | 一行 RLock 访问器 |
| 2122 | `func (p *Process) TotalCost() float64` | 一行 atomic 读 |
| 2127 | `func (p *Process) ProtocolName() string` | 一行 |
| 2132 | `func (p *Process) PID() int` | 一行 |
| 2137 | `func (p *Process) GetTotalTimeout() time.Duration` | 一行 + fallback |
| 2350 | `func (p *Process) LastSeq() int64` | 一行 atomic 读 |

**常量/错误留守**:
- `ProcessState` 枚举(第 27-34 行)
- `DefaultNoOutputTimeout` / `DefaultTotalTimeout` / `maxScannerBufBytes` / `maxStdinLineBytes` / `lineBufShrinkThreshold`(第 36-62 行)—— 全局数值常量;保留方便 struct 阅读者对照。但 **`maxStdinLineBytes`** 和 **`lineBufShrinkThreshold`** 分别只被 shim_io 与 readloop 用,可选移动到对应文件(见 §5.2)。
- 所有 error sentinels(`ErrMessageTooLarge` 第 67 行起至 `ErrAbortedByUrgent` 第 125 行)—— 因 callers 跨 session/router/dispatch 多处 `errors.Is`,集中保留 API 表面
- `processCloseTimeout` 变量(第 139 行)—— 被 Close 独用但同时 tests 依赖;留在主文件
- `maxPendingSlots` 常量(第 118 行)—— passthrough.go 也引用,留在主文件即可
- **`shimMsg` struct**(第 162-168 行)—— 只被 readLoop 解包用,可移到 `process_readloop.go`(见 §5.2)
- `Process` struct + `sendSlot` struct + `shimWriter` struct?——`shimWriter` 挪到 shim_io;其他留主文件

#### 4.2.2 `process_shim_io.go`

负责:shim 协议的出向写路径。所有"把一个 `shimClientMsg` 放到 bufio.Writer 上"的机制集中在这里。

| 当前行号 | 签名 | 说明 |
|---|---|---|
| 450-454 | `type shimWriter struct` | 连同 struct 一起搬 |
| 456 | `func (w *shimWriter) Write(data []byte) (int, error)` | io.Writer 实现,两条 fast/slow 路径 |
| 511-515 | `type shimClientMsg struct` | 出向消息;只被此文件 + 构造处使用 |
| 523-526 | `type shimSendEnc struct` + `shimSendBufPool` var | 池化资源 |
| 548 | `func encodeShimMsg(msg shimClientMsg) (*shimSendEnc, error)` | 编码出 pooled buffer |
| 571 | `func returnShimSendEnc(se *shimSendEnc)` | 归池;必须与 `encodeShimMsg` 同文件(配对语义) |
| 578 | `func (p *Process) shimSend(msg shimClientMsg) error` | 获锁版本 |
| 596 | `func (p *Process) shimSendLocked(msg shimClientMsg) error` | 预锁版本(Kill/Close/Detach 用) |

**额外搬家**:
- `maxStdinLineBytes` 常量(原第 46 行)—— 仅 `shimWriter.Write` 引用
- `shimSendBufMaxCap` 常量(第 569 行)—— 仅 `returnShimSendEnc` 引用

#### 4.2.3 `process_readloop.go`

负责:从 shim socket 读入站消息的 goroutine 及心跳协议。

| 当前行号 | 签名 | 说明 |
|---|---|---|
| 162-168 | `type shimMsg struct` | 入向消息;readLoop 主用;**`wrapper.go:360` 也构造**用于 Spawn 握手(同 package,归属此处不妨碍 wrapper 访问) |
| 619 | `func (p *Process) readLoop()` | 440 行主循环 |
| 1058 | `func (p *Process) heartbeatLoop()` | 心跳 60 行 |
| 1605 | `func isChanAlive(done <-chan struct{}) bool` | readLoop defer 顺序不变性的 helper;与 readLoop 绑死 |

**额外搬家**:
- `lineBufShrinkThreshold` 常量(原第 61 行)—— 仅 readLoop 引用
- `maxScannerBufBytes` 常量(原第 39 行)—— 仅 readLoop 引用

**不搬**:`startReadLoop()` 留主文件(生命周期门面;且 heartbeatLoop 的 goroutine 启动也在它里面)。

#### 4.2.4 `process_send.go`

负责:用户消息出向 + 中断语义。

| 当前行号 | 签名 | 说明 |
|---|---|---|
| 1132-1133 | `type EventCallback func(ev Event)` | **跨 package 公共类型**(cli.Send / cli.SendPassthrough / session.managed.Send / session.testutil.TestProcess / dispatch / server 都消费);归 `process_send.go` 是"主出向路径"归属,不代表"仅 Send 用"。签名改动是跨包 breaking change,慎重。 |
| 1142 | `func buildUserEntry(text string, images []ImageData) EventEntry` | Send 和 SendPassthrough 共用;但 Send 是主出向路径,归这里 |
| 1200 | `func (p *Process) Send(...) (*SendResult, error)` | 150 行;legacy 发送 |
| 1368 | `func (p *Process) Interrupt()` | SIGINT 走 shim |
| 1420 | `func (p *Process) InterruptViaControl() error` | stdin control_request |

**注 1**:`buildUserEntry` 的另一个调用点是 `passthrough.go:117`;package-private 调用跨文件,同 package 无需改任何东西。
**注 2**:`findResultSince` 归到 `process_turn.go`(见 §4.2.5)——Send 调用它,但它还服务 drainStaleEvents 的"fallback from EventLog"语义,跟 turn 协调归一类更合理。

#### 4.2.5 `process_turn.go`

负责:turn 边界协调(stale 清理 / result fallback / 事件时间戳 / 日志污染防护)。

| 当前行号 | 签名 | 说明 |
|---|---|---|
| 1118 | `func (p *Process) findResultSince(afterMS int64) *SendResult` | EventLog 扫描回退;Send / drainStaleEvents 共用 |
| 1474 | `func (p *Process) drainStaleEvents(ctx context.Context) error` | interrupted turn settle + push-back |
| 2358 | `func sanitizeStderrLine(line string) string` | stderr ANSI/控制字符清洗;只被 readLoop 的 `stderr` case 引用——严格说归 readloop,但 readloop 已满;放 turn 文件作为"旁路 helper"可接受 |

**权衡**:若觉得 `sanitizeStderrLine` 跟 turn 语义无关,可改放 `process_readloop.go`,本 RFC 接受两种答案。推荐放 turn 是因为 readloop 已 ~510 行,进一步膨胀不利;且本函数无状态、与 Process 无关,放哪都不影响正确性。

**额外搬家**:`maxStderrLogLineBytes` 常量(第 2352 行)—— `sanitizeStderrLine` 独用。

#### 4.2.6 `process_event_format.go`

负责:`Event` → `EventEntry` 转换、工具输入格式化。全是纯函数(除 `logEventAt` 一个方法)。

| 当前行号 | 签名 | 说明 |
|---|---|---|
| 1750 | `func EventEntryFromEvent(ev Event) (EventEntry, bool)` | 单 entry 便捷 |
| 1762 | `func EventEntriesFromEvent(ev Event) []EventEntry` | 多 entry |
| 1769 | `func EventEntriesFromEventAt(ev Event, nowMS int64) []EventEntry` | 时间注入版 |
| 1920 | `func (p *Process) logEventAt(ev Event, nowMS int64)` | EventLog 写入包装;**唯一**一个非纯方法 |
| 1937-1943 | `type agentInput struct` | Agent tool 输入解码 |
| 1945 | `func parseAgentInput(input json.RawMessage) agentInput` | |
| 1964 | `func (a agentInput) label() string` | 被测试 lock 保护的契约 |
| 1974 | `func formatToolDetail(block ContentBlock) string` | 包装 |
| 1981 | `func shortPath(p string) string` | 路径缩写 |
| 1997 | `func FormatToolInput(toolName string, input json.RawMessage) string` | 主分派 |

#### 4.2.7 `process_event_query.go`

负责:只读查询 + Linker 集成 + 历史注入。

| 当前行号 | 签名 | 说明 |
|---|---|---|
| 2149 | `func (p *Process) InjectHistory(entries []EventEntry)` | 唤醒 Linker 的写路径;读取为主 |
| 2231 | `func (p *Process) InitLinker(cwd string)` | Linker 构造 |
| 2246 | `func (p *Process) Linker() *SubagentLinker` | 一行访问器 |
| 2255 | `func (p *Process) EventLog() *EventLog` | 一行访问器 |
| 2266 | `func (p *Process) SetCwdForLinker(cwd string)` | reconnect 路径 |
| 2288 | `func (p *Process) EventEntries() []EventEntry` | 一行透传 |
| 2293 | `func (p *Process) EventLastN(n int) []EventEntry` | 一行透传 |
| 2298 | `func (p *Process) EventEntriesSince(afterMS int64) []EventEntry` | 一行透传 |
| 2305 | `func (p *Process) EventEntriesBefore(beforeMS int64, limit int) []EventEntry` | 一行透传 |
| 2310 | `func (p *Process) LastEntryOfType(typ string) EventEntry` | 一行透传 |
| 2315 | `func (p *Process) TurnAgents() []SubagentInfo` | 一行透传 |
| 2321 | `func (p *Process) LastActivitySummary() string` | 一行透传 |
| 2332 | `func (p *Process) LastEventAt() time.Time` | 一行透传 |
| 2340 | `func (p *Process) UserTurnCount() int64` | 一行透传 |
| 2345 | `func (p *Process) SubscribeEvents() (<-chan struct{}, func())` | 一行透传 |

### 4.3 依赖图(文件级心智模型)

同 package 内无循环依赖风险(Go 同 package 自由互引)。但写的顺序("先做哪个 phase")应遵循"被依赖者先"。方向为"调用 → 被调用":

```
process.go (Process struct, 构造, kill/close)
    │
    ├──► process_shim_io.go  (shimSend / shimSendLocked)
    │         ▲
    │         │ (被 Kill/Close/Detach/Send/Interrupt/heartbeat/passthrough 调用)
    │         │
    ├──► process_readloop.go (readLoop / heartbeatLoop)
    │         │
    │         ├─► process_shim_io.go (ping/shimSend)
    │         ├─► process_event_format.go (logEventAt / EventEntriesFromEventAt)
    │         └─► process_turn.go (sanitizeStderrLine; isChanAlive 反向依赖)
    │
    ├──► process_send.go     (Send / Interrupt)
    │         │
    │         ├─► process_shim_io.go (shimSend)
    │         └─► process_turn.go   (drainStaleEvents / findResultSince)
    │
    ├──► process_turn.go     (drainStaleEvents)
    │         │
    │         └─► process.go (isChanAlive — 留主文件;OR 跟 readLoop 同文件)
    │
    ├──► process_event_format.go (纯函数 + logEventAt)
    │         │
    │         └─► internal/cli/eventlog.go (AppendBatch)
    │
    └──► process_event_query.go (读)
              │
              ├─► internal/cli/eventlog.go (EntriesSince 等)
              └─► internal/cli/subagent_link.go (Linker.Query / Resolve)
```

关键观察:**没有任何一条"新文件 → 新文件"的双向依赖**。所有新文件要么只依赖 process.go struct + 常量,要么依赖同 package 其他既有文件(eventlog / subagent_link / passthrough)。迁移可以**任意顺序**进行。

## 5. Package-private helpers 的归属

### 5.1 需要共享的符号清单

| 符号 | 类型 | 归属 | 引用点 |
|---|---|---|---|
| `*Process` struct | struct | `process.go` | 所有文件 |
| `sendSlot` struct + `isCanceled` | struct+method | `process.go` | passthrough.go + process_send.go(未来) |
| `shimWriter` struct + `Write` | struct+method | `process_shim_io.go` | process_send.go 通过 `p.shimStdinWriter()` 拿 io.Writer |
| `shimClientMsg` struct | struct | `process_shim_io.go` | 同文件 only |
| `shimMsg` struct | struct | `process_readloop.go` | 同文件 only |
| `encodeShimMsg` / `returnShimSendEnc` | func | `process_shim_io.go` | 同文件 only;**必须同文件以防 pool 生命周期断裂** |
| `shimSend` / `shimSendLocked` | method | `process_shim_io.go` | 跨文件:send / interrupt / heartbeat / kill / close / detach |
| `setDeathReason` / `DeathReason` | method | `process.go` | 跨文件:readLoop / heartbeat / Send / Interrupt |
| `slogger` | method | `process.go` | readLoop / heartbeat / Send 都要 |
| `isChanAlive` | func | `process_turn.go`(推荐)or `process_readloop.go` | drainStaleEvents 独家使用 |
| `buildUserEntry` | func | `process_send.go` | passthrough.go 也调 |
| `agentInput` struct + `label` | struct+method | `process_event_format.go` | 同文件 + tests |
| `parseAgentInput` / `formatToolDetail` / `shortPath` | func | `process_event_format.go` | EventEntriesFromEventAt 内部 |
| `sanitizeStderrLine` | func | `process_turn.go`(推荐)or `process_readloop.go` | readLoop stderr case 独家 |
| `newShimProcess` | func | `process.go` | wrapper.go 调用(第 223, 293 行)—— **cross-file contract** |

### 5.2 小常量与变量的归属

| 符号 | 位置 | 新文件 |
|---|---|---|
| `DefaultNoOutputTimeout`, `DefaultTotalTimeout` | 37-38 | `process.go`(Send 与 watchdog 默认值) |
| `maxScannerBufBytes` | 39 | `process_readloop.go` |
| `maxStdinLineBytes` | 46 | `process_shim_io.go` |
| `lineBufShrinkThreshold` | 61 | `process_readloop.go` |
| `ErrMessageTooLarge` 等 error sentinels | 67-131 | `process.go`(API 表面) |
| `processCloseTimeout` | 139 | `process.go`(Close 独用 + tests 修改) |
| `maxPendingSlots` | 118 | `process.go`(passthrough 也要) |
| `shimSendBufMaxCap` | 569 | `process_shim_io.go` |
| `maxStderrLogLineBytes` | 2352 | `process_turn.go` |
| `DeathReasonXxx` 常量块 | 360-367 | `process.go`(跨文件 + 外部调用方 match) |
| `shimSendBufPool` (var) | 528-539 | `process_shim_io.go` |

原则:**错误与 death reason 常量保持在主文件以维持 API 可见性**;**pool / 内部 buffer 大小常量跟着用它们的函数走**。

## 6. 测试文件:零改动

现有 `internal/cli/*_test.go` 清单 **30 个**,全部 `package cli`(`grep -c "^package cli_test" internal/cli/*_test.go` 零匹配),文件间移动对测试代码**完全透明**。

| 测试文件 | 核心测试对象 | 拆分后的 .go 对位 |
|---|---|---|
| `process_test.go` (**15 Test**) | shimWriter / Send / Interrupt / Close / Detach / readLoop / FormatToolInput / parseAgentInput / SanitizeStderrLine / InjectHistory / 各访问器 | 跨 7 个新文件——测试**按行为而非按文件**组织,拆源码不影响 |
| `process_extra_test.go` (**49 Test**) | process 相关全面覆盖;实际是最大的测试集中营 | 跨 7 个新文件 |
| `process_linebuf_test.go` (2 Test) | readLoop 的 lineBuf 生命周期 | `process_readloop.go` |
| `process_monotonic_test.go` (2 Test) | drainStaleEvents 时钟单调性 | `process_turn.go` |
| `process_mu_rwlock_test.go` (1 Test) | mu RWLock 并发读 | `process.go` |
| `process_slog_test.go` (3 Test) | SetSlogKey / slogger | `process.go` |
| `process_user_entry_test.go` (3 Test) | buildUserEntry | `process_send.go` |
| `process_interrupt_race_test.go` (3 Test) | Interrupt 与 drainStaleEvents 竞态 | `process_send.go` + `process_turn.go` |
| 其余 ~14 个:`askquestion_test.go` / `cli_test.go` / `detect_test.go` / `eventlog_*_test.go` / `image_test.go` / `on_turn_done_contract_test.go` / `passthrough_test.go` / `protocol_*_test.go` / `subagent_*_test.go` / `thumbnail_test.go` / `todo_test.go` / `uuid_test.go` / `wrapper_test.go` / `atomic_pointer_contract_test.go` | 对应非 process.go 源(passthrough/protocol/eventlog/linker/wrapper 等) | 不受影响 |

**内部 test helper(`image_testhelper_test.go`、`atomic_pointer_contract_test.go` 等)同 package 存在,位置不变**。

**验证脚本**:迁移每个 phase 后跑
```bash
go test -race -count=1 ./internal/cli/...
```
期望:保持现有 pass/skip 数目,无新增 fail。

## 7. 迁移步骤

每个 phase 是**一个独立 commit + PR**。按下列顺序执行最小化冲突面;实际落地允许并行——这 7 个 phase 互不阻塞(见 §4.3 依赖图)。

### Phase 1 — `process_shim_io.go`

**搬运**:`shimWriter` struct + `Write` 方法、`shimClientMsg` struct、`shimSendEnc` struct、`shimSendBufPool` var、`encodeShimMsg`、`returnShimSendEnc`、`shimSend`、`shimSendLocked`、常量 `maxStdinLineBytes`、`shimSendBufMaxCap`。

**触发点**:约 ~300 行从 process.go 移出。

**验证**:`go build ./... && go test -race -count=1 ./internal/cli/...`

**风险**:无;纯剪切粘贴。

### Phase 2 — `process_readloop.go`

**搬运**:`shimMsg` struct、`readLoop`、`heartbeatLoop`、常量 `maxScannerBufBytes`、`lineBufShrinkThreshold`。

**决策点**:`isChanAlive` 和 `sanitizeStderrLine` 暂留 process.go,等 Phase 4 一起搬到 process_turn.go。理由:避免 Phase 2 同时搬"readLoop 主体"和 readLoop 依赖的两个 helper,commit 更小更纯。

**验证**:同上。

**风险**:readLoop 的 defer 顺序 (`close(eventCh) → close(done) → CloseSubscribers → recover`) 复制时必须 byte-identical;用 diff 工具对照确认。

### Phase 3 — `process_send.go`

**搬运**:`EventCallback` 类型、`buildUserEntry`、`Send`、`Interrupt`、`InterruptViaControl`。

**不搬**:`findResultSince` 留到 Phase 4(和 drainStaleEvents 一起)。

**验证**:同上。

**风险**:`Interrupt` 和 `InterruptViaControl` 内的 `p.mu.Lock` 顺序不变,这是 R39-CONCUR1 的 invariant 点。

### Phase 4 — `process_turn.go`

**搬运**:`findResultSince`、`drainStaleEvents`、`isChanAlive`、`sanitizeStderrLine`、`maxStderrLogLineBytes` 常量。

**验证**:同上;特别注意跑 `process_monotonic_test.go` + `process_interrupt_race_test.go`——这两个覆盖 drainStaleEvents 最精妙的路径。

**风险**:`isChanAlive` 的 godoc 对 defer 顺序不变性的论述必须原样复制。此处是本 RFC 最敏感点——**也是 R29-DES1 的前置**。

### Phase 5 — `process_event_format.go`

**搬运**:`EventEntryFromEvent`、`EventEntriesFromEvent`、`EventEntriesFromEventAt`、`logEventAt`、`agentInput` struct、`parseAgentInput`、`label`、`formatToolDetail`、`shortPath`、`FormatToolInput`。

**验证**:同上;`process_test.go:TestEventEntryFromEvent` / `TestFormatToolInput` / `TestShortPath` / `TestFormatToolDetail` 覆盖。

**风险**:无;全是纯函数 + 一个方法。

### Phase 6 — `process_event_query.go`

**搬运**:`InjectHistory`、`InitLinker`、`Linker`、`EventLog`、`SetCwdForLinker`、所有 `Event*` 一行透传方法、`LastEntryOfType`、`TurnAgents`、`LastActivitySummary`、`LastEventAt`、`UserTurnCount`、`SubscribeEvents`。

**验证**:同上。

**风险**:无;全是一行透传 + 两个 Linker 方法。

### Phase 7(可选)— 回看 process.go

预期留守 400-470 行。如有零碎"感觉不对位"的函数(例如 `buildUserEntry` 若两个调用者都在别的文件,挪到 passthrough 更近——**本 RFC 不推荐**,保持"主出向路径 = process_send"的心智模型),此时做一次小修正。

### 7.1 合并顺序自由度

依赖图(§4.3)显示所有新文件**只 outbound**依赖主 process.go;因此 Phase 1-6 可以:

- 严格串行(上述推荐路径),每 phase 单 PR
- 或一次性 6 文件 PR("big bang")——只适合能安排 2 小时代码评审窗口的情况

推荐串行:cognitive load 更低,review 质量更高,conflict 概率低。

## 8. 验证准则

### 8.1 每 phase 必过

```bash
# build
go build ./...

# race test
go test -race -count=1 ./internal/cli/...

# vet
go vet ./internal/cli/...

# gofmt 零 diff(hook 自动执行)
gofmt -l internal/cli/
```

### 8.2 diff 质量门槛

预期**纯 move,零净改**。在每个 phase 的 PR 中跑:

```bash
# 净行数应为 0 或 +几十(file header + package + import)
git diff --stat HEAD~1 -- internal/cli/process.go internal/cli/process_*.go
```

若出现"被搬函数的 body 内容 diff"(不是缩进/空行),立即 abort——某个字符变了就破坏"零语义改动"承诺。

### 8.3 git blame 保真

Git 原生对同 package 内大块移动有 `-M` 选项支持 rename 检测:

```bash
git log --follow -M90% internal/cli/process_shim_io.go
```

`-M90%`:相似度 >= 90% 时识别为 rename/move。但 **Go 大块提取到同 package 新文件不是 rename,是 diff within dir** —— `-M` 在"整文件 rename"最有效;"把一个大文件切成多份"属于 partial extraction,Git 的相似度算法要求新文件和原文件的 ~90% 行重叠才能关联,而本 RFC 每个 phase 搬 300 行到 process_*.go 但 process.go 还留 2000 行,相似度**远不够**。

**实际效果**:
- **phase 搬迁量 >= 500 行时** `-M90%` 通常能命中(新文件 90%+ 内容与原文件某段一致)
- **小 phase(&lt;300 行)** blame 会断到 move commit,`git log --follow` 结果不完整

**缓解**:
- 每个 phase 搬尽量大块(Phase 1 shim_io 300 行、Phase 2 readloop 500 行、Phase 3 send 400 行)
- commit message 用项目既有中文格式:`refactor(cli): 把 shim_io 从 process.go 拆出 [move-only, no semantic change]`
- 未来 `git log -S'shimSend'` 能跳过 move commit 命中语义修改的 commit(`-S` 按新增/删除字符串 delta,move commit 的 delta 接近零)

### 8.4 运行时回归

在部署前手动验证:

1. 冷启动:`sudo systemctl restart naozhi` → dashboard 上所有 session 可见
2. 发一条带图消息 → readLoop 正常产出 event;dashboard 实时渲染
3. 发一条长 turn(10+ tool_use) → 无 event 丢失
4. 中断:dashboard 点 interrupt → 下一条消息 drainStaleEvents 正常 settle
5. shim reconnect:`sudo systemctl restart naozhi` 连发两次 → `reconnectedMidTurn` 路径工作

## 9. 风险 & 回滚

### 9.1 风险

**R1(高)—— merge conflict**
拆文件期间若并发 PR 动 process.go,每个 phase 都与其冲突。**缓解**:选择并发 PR 谷期一次性推完(6 commit,~30 分钟可完成),或在主分支冻结 process.go 写入的短窗口内做。

**R2(中)—— git blame 失真**
对不满足 `-M` 相似度阈值的碎片(比如只搬 `shortPath` 一个函数时 file 相似度 <10%),blame 会断点。缓解:每个 phase 搬的是"大块语义相关函数群",通常单文件 >200 行,blame 的相似度判断能命中。

**R3(中)—— AI review context 的副作用**
Claude / Copilot 等 AI review 需要重新建立"process.go 所在文件"的心智——反过来说,**更细的文件反而让 AI 更好读**(单文件 <550 行放得进单次 context 窗口)。净收益。

**R4(低)—— 常量/错误 accidentally duplicated**
某个常量被两个文件都 import 时,开发者可能 local-redefine。缓解:rule-of-thumb "常量放在用它的第一个 phase 文件",并在 phase 1 就把 `maxStdinLineBytes` 等独占常量搬完。

**R5(低)—— 新文件命名**
`process_shim_io.go` vs `process_shimio.go` vs `process_shim.go`——选了 `shim_io` 强调 direction。如 reviewer 偏好其他命名,在 phase 1 PR 统一定名后续 phase 跟进。

### 9.2 回滚策略

**粒度**:每 phase 独立 commit,`git revert <commit>` 即可回滚单 phase。

**整体回滚**:`git revert <phase1>..<phase6>` 一口气 6 个 revert。由于每个 phase 是纯 move,revert 也是纯 move,无语义泄漏。

**部分回滚**:假设 Phase 4 (drainStaleEvents 搬家) 触发生产 bug,`git revert` Phase 4 单独回滚;Phase 5/6(在它之上的 move)**不受影响**——因为不同文件、无语义重叠。

### 9.3 与 R67/R71/R29 的衔接

本 RFC 完成后,三个 pending review item 的 reviewer 工作空间显著变小:

| TODO item | 涉及文件(拆前) | 涉及文件(拆后) |
|---|---|---|
| R67-PERF-1 | process.go(2464) + protocol_claude.go + protocol_acp.go | process_readloop.go(510) + protocol_*.go |
| R71-PERF-H1 | process.go(2464) + shim/protocol.go | process_shim_io.go(300) + shim/protocol.go |
| R29-DES1 | process.go(2464) 的 readLoop + drainStaleEvents 相隔 860 行 | process_readloop.go(510) + process_turn.go(250),两文件互引 |

这三项的后续修复**不属于本 RFC 范围**,但本 RFC 的唯一目的就是解除阅读瓶颈让它们能推进。

## 10. 开放问题

### Q1. 是否把 passthrough.go 里的 slot 方法(`onSystemInit` / `onTurnResult` / `reapAbortedPreempted` / `discardAllPending` / `handleReplayEventLocked` 等)也纳入本次拆分?

**不。** passthrough.go 当前 ~580 行,且职责内聚(passthrough slot 语义)。拆分 process.go 和整理 passthrough.go 是两件事;后者另立 ADR。

### Q2. 是否给 `*Process` struct 做字段 padding 优化?

**不。** 性能问题归 R67-PERF / R70-PERF 系列,不属本 RFC。

### Q3. 要不要顺便给 `EventEntriesFromEventAt` 起个更短的名字?

**不。** 重命名 = API 改动,违反 §2 非目标。

### Q4. 拆分后 `process.go` 有没有可能还是太大?

当前估算 ~470 行(含 struct 定义 + 错误常量 + 生命周期方法)。如果后续 struct 继续膨胀(新增字段),可以考虑再拆出 `process_state.go` 专门放 struct + state.String + 枚举常量。本 RFC 暂不包括。

### Q5. 为什么 `isChanAlive` 不和 readLoop 同文件?

它的契约("done 先于 eventCh 关闭")本身就是 readLoop 的 defer 顺序的反射。把它放在 `process_turn.go` 让 drainStaleEvents 的"push-back 安全性"论证全部在一个文件内完成;放在 readloop 则 drainStaleEvents 必须跨文件引用。**两种答案都对**,本 RFC 选前者,PR review 可调整。

## 11. 结语

这是一个**纯机械重构**。预期 6 个小 PR,每个 PR review 时间 5-10 分钟,总工程量 2-4 小时。产出是把 2464 行单文件降到 7 个 <550 行文件,直接解锁 R67/R71/R29 三个 review item 的阅读瓶颈,不引入任何新的测试/API/并发风险。

不值得做的前提:只有"process.go 将来就只加 50 行就再也不长了"。过去六个月数据显示它月均 +200 行,**继续放任=将来必然更痛**。现在切是最便宜的时刻。

---

**附录 A:当前 60 个 `func` 行号索引**(`grep -n '^func ' internal/cli/process.go` 全量,供 reviewer 对照):

```
141 ProcessState.String
327 sendSlot.isCanceled
341 Process.SetSlogKey
350 Process.slogger
382 Process.setDeathReason
403 Process.DeathReason
412 newShimProcess
438 Process.shimStdinWriter
456 shimWriter.Write
548 encodeShimMsg
571 returnShimSendEnc
578 Process.shimSend
596 Process.shimSendLocked
610 Process.startReadLoop
619 Process.readLoop
1058 Process.heartbeatLoop
1118 Process.findResultSince
1142 buildUserEntry
1200 Process.Send
1351 Process.Alive
1361 Process.IsRunning
1368 Process.Interrupt
1420 Process.InterruptViaControl
1474 Process.drainStaleEvents
1605 isChanAlive
1630 Process.Kill
1681 Process.Close
1733 Process.Detach
1750 EventEntryFromEvent
1762 EventEntriesFromEvent
1769 EventEntriesFromEventAt
1920 Process.logEventAt
1945 parseAgentInput
1964 agentInput.label
1974 formatToolDetail
1981 shortPath
1997 FormatToolInput
2092 Process.GetState
2108 Process.SetOnTurnDone
2115 Process.GetSessionID
2122 Process.TotalCost
2127 Process.ProtocolName
2132 Process.PID
2137 Process.GetTotalTimeout
2149 Process.InjectHistory
2231 Process.InitLinker
2246 Process.Linker
2255 Process.EventLog
2266 Process.SetCwdForLinker
2288 Process.EventEntries
2293 Process.EventLastN
2298 Process.EventEntriesSince
2305 Process.EventEntriesBefore
2310 Process.LastEntryOfType
2315 Process.TurnAgents
2321 Process.LastActivitySummary
2332 Process.LastEventAt
2340 Process.UserTurnCount
2345 Process.SubscribeEvents
2350 Process.LastSeq
2358 sanitizeStderrLine
```
