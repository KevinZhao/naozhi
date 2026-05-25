# TODO

> 最后更新：2026-05-25 — 详见 `docs/TODO_CHANGELOG.md`
>
> **当前规模**：Open ~1124 / Pending ~275 / Done 已搬到 `docs/TODO_ARCHIVE.md`
>
> **最近 3 轮重大动作**（详见 changelog）：
>   - **2026-05-25 R248 cluster 收尾**：14 条同根因归档（PR #348）+ R246-GO-7 jitter Paused re-check + 3 条误报归档（PR #349）
>   - **2026-05-24 4 个核心架构 refactor**：wshub.go 6-stage split (PR #327, R243-ARCH-2) + spawningKeys spin→chan (PR #329, R243-ARCH-4) + dispatch.Capabilities interface (PR #330, R243-ARCH-10) + AgentLinker interface (PR #331, R239-ARCH-I) + 5 个清理 PR (#342-#346)
>   - **2026-05-23 cron 大批改**：scheduler.go god-object split → 7 职责文件 (PR #309)

## 阅读指南

文档结构：
- **顶部 "上一轮更新"**：最近 5 轮一行摘要；完整 narrative 见 `docs/TODO-changelog.md`。
- **Round 26-82 抢救区**：从 2026-04-21 至 2026-04-27 的 57 轮 review 中抢救的未决 open items（18 条），narrative 部分已归档。
- **CRITICAL / HIGH / UX / MEDIUM / LOW / 新功能**：真实待决项按优先级分类。
- **历史归档**：`docs/TODO-changelog.md` 内含 Round 110..83 完整 narrative + 2026-04-14 ~ 2026-04-21 已修复清单（开发日志形态，供追溯）。

**若要开工新条目**，优先看 CRITICAL / HIGH 区的 `- [ ]` 标记条目，以及 Round 26-82 抢救区的未决条目。

---

## Round 26-82 历史详细条目已归档

> 2026-04-21 至 2026-04-27 期间共跑了 57 轮深度 review（Round 26-82），每轮记录当时的 "Needs Design Decision" / 已修复 / 判为误报 明细。经 Round 84（性能条目全量核实）+ Round 85（安全条目全量核实）后，这 57 轮的待决条目已全部：
> - **已实施**：代码中可 grep 验证的条目已关闭
> - **降级 / 误报**：威胁模型或量级假设与 naozhi 实际部署不匹配的条目已关闭
> - **合并跟踪**：同源条目收敛到最新轮次的锚点条目
>
> 删除这些详细条目以让 TODO 更清爽。如需查看某条历史条目的当时上下文，参见 git log：
> - Round 84 完整核实清单见本文件 `## Round 84 — 性能堵点全量核实批`
> - Round 85 完整核实清单见本文件 `## Round 85 — 安全条目全量核实批`
> - 各 Round 的原始 "已修复" 记录在 Round 的 commit message 里（grep `git log --grep='R[0-9]'`）

### 从 Round 26-82 抢救的剩余 open items

- [ ] **R71-PERF-H1（HIGH，stdout 热路径 alloc）—— `shimWriter.Write` 快慢两条路径都 `string(data[:len-1])` 拷贝**: 5-50 events/s × N session 每行约 200B-4KB heap copy 到 `shimClientMsg.Line`。方案：`shimClientMsg.Line` 由 `string` 改 `json.RawMessage`，或引入 `lineBytes []byte` 字段 + 自定义 `MarshalJSON`，`returnShimSendEnc` 前 zero 掉 slice 指针。需跨 shim 协议 revision 校对 peer 版本兼容。`internal/cli/process.go:264,293` + `internal/shim/protocol.go:10-15`
- [ ] **R67-PERF-3（MED，shim stdout 热路径）—— shim `readStdout` 双 string 转换**: `string(line)` 做 ServerMsg.Line 用 + `json.Marshal` 内再编码一次。方案：`ServerMsg` 变体字段 `json.RawMessage` 供 stdout 热路径，避 intermediate string。shim 独立 binary，不影响主进程 API。
- [ ] **R62-GO-3 — `ResetAndRecreate` 释放 + 重取 `r.mu` 窗口对 `spawnSession` opts 的竞态（MED）**: `router.go:1532-1538` 删 session 后释放 mu 调用 `proc.Close()`，再 re-Lock 调 spawnSession。此窗口内若并发 `GetOrCreate` 抢先 spawn 同 key session，其 opts 会覆盖 ResetAndRecreate 调用方的 Backend 选择，而调用方以为拿到了自己 opts 下的 session。
- [~] **R61-GO-10 — `evictOldest` 不清理 `workspaceOverrides`（降级关闭，2026-04-28 Round 108 核实）**: 本条原文把"保留 override → 新 session 继承"描述为 bug，但核实后这正是期望行为：`workspaceOverrides` 是用户**显式调 `SetWorkspace`** 设置的 per-chat 偏好（不是 eviction 的衍生状态），evict 是 LRU 资源回收，不意味着用户放弃 workspace 设定。相关证据：`Reset(key)` 路径（用户主动 `/new`）**才**按 chatKeyPrefix 清理 `workspaceOverrides` (`router.go:1228-1235`)，显式区分"用户主动重置 chat" vs "LRU 驱逐 session"。Resume 继承 workspaceOverrides 是 `spawnSession` 的第 2 优先级决策点 (`router.go:1399-1403`)，和 evict 后重建同 key 路径共享此语义。若未来需要"驱逐同时忘记 workspace"的新语义，应作为独立 UX 功能走 Remove 或新命令，不应修改 evict 的 LRU 契约。
- [~] **R57-ARCH-001 — `Cleanup` 双 pass `loadProcess()` 在非 exempt 占多数场景下更慢（LOW）**: go-reviewer 指出 R56 的 "先 count candidates 再 allocate" 优化在 exempt 少的部署上反而多一次 loadProcess 扫描。idle plan 部署（每 5 分钟 tick 一次）无实际差异；需真实 profiling 数据决定是否回滚或改 single-pass count-then-grow。 — 已实施（cron-fix-F4 2026-05-24 核实）：`router_cleanup.go:163-188` Cleanup() 已是 single-pass，candidate slice 用 `make([]cand, 0, len(r.sessions)/2+1)` 容量估算，inline 注释引 R59-GO-M1。本条目所述的"双 pass"早已在 R59 修复，TODO 残留。本批 PR 关闭。
- [ ] **R54-CONCUR-001 — `router.ReconnectShims` reconcile 期运行时的 `sess.ReattachProcessNoCallback` 无 sendMu 保护（继承自 R51-CONCUR-002 未决）**: 本轮复核确认仍未决。方案见 R51-CONCUR-002。
- [~] **R52-CONCUR-004 — `shim reconnect` 后 `sess.persistedHistory` 未重新注入新 `proc.eventLog`（MED）**: `Router.reconnectShims` 在 `storeProcess`（`ReattachProcessNoCallback`）前调 `proc.InjectHistory(histEntries)`，但 `histEntries` 是从 `discovery.LoadHistory` 读的 JSONL 文件，**不是** `sess.persistedHistory`。两者大部分一致，但 `persistedHistory` 可能包含仅在内存里的 user prompt / interim 状态（R49/R47 曾多次浮现）。新 proc.eventLog 缺少这些条目时，`EventEntriesSince` 的 "proc != nil → proc.EventEntries" 快路径可能少返回若干历史。 — 已加 inline godoc（cron-fix-F4 2026-05-24）：`router_shim.go:434-471` reconnect JSONL 注入路径已注释说明本子问题 + dashboard scrollback fallback 路径会做 merge 收敛 + 真正修复需先落 R51-CONCUR-002（sendMu 保护）以保证合并不撕裂正在进行的 Send。本条作为子项归 R51-CONCUR-002 主条目跟踪，本批 PR 关闭。
- [ ] **R51-CONCUR-002 — `reconnectShims` 周期调用期间 `ReattachProcessNoCallback` 无 sendMu 保护（HIGH）**: `StartShimReconcileLoop` 每 30s 调一次 `reconnectShims`，调用点在持 `r.mu.Lock()` 时 `sess.ReattachProcessNoCallback(proc, sessionID)`；该函数 docstring 明确标注"调用者必须保证 Send() 不在飞行中"（safety constraint）。启动阶段 OK，但运行期 reconcile 不满足该假设 — `ManagedSession.Send()` 可能持 sendMu 执行旧进程。`storeProcess` 会原子替换活跃进程指针，并 `deathReason.Store("")` 清除 Send() 刚写入的 timeout 死因。逻辑 race（非 data race），Send() 拿的是旧 process 指针仍可写回结果。
- [ ] **R51-CONCUR-005 — 并发 `shim.Manager.Reconnect` 对同 key 晚胜者 Close 早胜者 handle，session 误死（MED）**: `Reconnect` 在 `m.mu` 外建立 TCP 连接（10s 超时），然后在 `m.mu.Lock()` 下插入 `m.shims[key]`。两路并发 Reconnect 分别建立连接后，晚胜者关闭早胜者 handle；但早胜者 handle 可能已被 Router `reconnectShims` 传给 `Process`，`Process.shimConn` 被 Close 导致 readLoop 退出、session 标为 Dead。
- [ ] **R37-REL1 — `MessageQueue.TryAcquire` + `Release` 不会触发 drain**: Dashboard/WS 路径用 Guard 接口，但若同期 IM 入口 Enqueue 了消息（enqueued=true），Release 不会触发 DoneOrDrain，消息永久搁浅直到下次 Enqueue 再成为 owner。属于 Guard/Queue 混用的根本限制。
- [ ] **R33-UX1 — `dashboard.js renderSidebar` 每次 sessions_update 全量 innerHTML 重绘**: 20 sessions × 1 update/s 情况下浏览器全侧边栏 reflow；已缓存 `allSessionsCache` 但未做 diff。scrollTop 保持已在 RNEW-UX-016 关闭时落地（rAF 恢复），剩余工作 = DOM diff + active-card 跨重绘保留 + `allSessionsCache` 一致性（`syncSidebarSelectionWithActive`/`removeSessionCard` 路径若做 in-place 修改可能让后续 update 看到不一致前缀）。合并 R34-UX1。
- [ ] **R31-REL3 — `moveToShimsCgroup` 依赖 runtime sudo + 未校验 CLIPID**: 现状用 `sudo busctl`/`sudo tee`，CLIPID 取自 shim JSON 直接入参；若 shim 被劫持可通过伪造 CLIPID 把任意进程挪入 scope。
- [~] **R30-DES1 — 需架构决策（2026-04-29 Round 112 评估降级）**：本轮尝试在 `execute()` 入口加 `stopCtx.Err()` 守卫覆盖 fresh + persistent 两种模式，但这与 Round 95 的设计意图冲突（Round 95 明确将 persistent 模式的 ctx 取消委托给 Router.Shutdown，`TestCRON3_PersistentModeUnaffectedByGuard` 把此行为作为测试护栏）。fresh 分支的 stopCtx.Err() 守卫（`scheduler.go:1260`）已覆盖最危险的"fresh → Reset → 孤立 CLI"路径。persistent 模式的真正修复需要架构级协调：要么把 Router.Shutdown 和 Scheduler.Stop 串联锁定（需 S11 级决策），要么在 GetOrCreate 路径里加 shutdown-awareness（改动面大）。当前降级，等 S11 整体方案落地后重开。
- [ ] **R29-DES1 — `drainStaleEvents` push-back + goto drain 可吞 interrupted result 事件**: 本轮新发现的 invariant 冲突。在 interrupted/interruptedRun 分支的 for 循环中，若事件顺序为 `[old_nonresult, new_event, old_result]`，读到 `new_event` 后 push-back + `goto drain`，接着 drain 到 `old_result` 时因 `recvAt < cutoff` 被丢弃。interrupted 语义要求 settle 窗口必须拿到 old_result，否则下一 turn 迟到的 result 会污染结果。

## Round 248 — Go 正确性专审 PR #327/#329/#330/#331（2026-05-24）NEEDS-DESIGN

> 4 PR 累计 review：PR #327 wshub.go god-object 6-stage split；PR #329 spawningKeys map 值改 chan struct{} 即时唤醒；PR #330 dispatch.Capabilities interface 收口 closure-DI；PR #331 AgentLinker interface 解耦 server↔cli。基线 bd38498~1 → 102d8a7。聚焦：race / 死锁 / 资源泄漏 / 错误处理 / 边界条件 / lock 顺序。
>
> 总结：核心不变量正确。spawningKeys close+delete-under-r.mu 顺序确实避开了 ABA（实际上 close/delete 的相对顺序在 r.mu 保护下不可观察，注释略夸大但无害）；wshub split 中 clientWG.Add 全部位于持锁路径，sendWG 走 TrackSend 屏障；Capabilities 反向适配在 *Server 完整构造后赋值（server.go:919 在 Start 阶段，非 New），无 partial-init；AgentLinker typed-nil 陷阱在两个生产路径均有 concrete-level guard。
>
> 找到 5 项可改进项，全部低优先级 / 文档级，未发现严重正确性 bug。

### Go 正确性

- [x] **R248-GO-1 — closureCapabilities.Send c.send==nil 运行时 panic（P3）** [REFACTOR]: `internal/dispatch/capabilities.go:104-109` 当 NewDispatcher 走 closure 适配器分支但 SendFn 为 nil（用户只设了 TakeoverFn/ReplyFooterFn），首次 Send 调用才 panic。契约违规应在构造期暴露。 *(已实施：与 R248-ARCH-2 一同修复 — NewDispatcher 加 boot-panic gate 检测 closureCapabilities{send:nil} 也判 missing；test 路径用 AllowMissingSender opt-out。)*
- [x] **R248-GO-2 — NewDispatcher Capabilities 与 *Fn 同时设置静默忽略 *Fn（P3）** [REFACTOR]: `internal/dispatch/dispatch.go:286-297` `caps := cfg.Capabilities; if caps == nil { ... }` —— 若调用者同时设置 `Capabilities` 和 `SendFn`（迁移期常见误用），SendFn 静默丢失，hot-path 走 Capabilities.Send。 *(已实施：NewDispatcher 在 Capabilities 与 *Fn 同时设置时 slog.Warn 一次性日志，标识哪些 *Fn 被忽略。)*
- [x] **R248-GO-3 — spawnSession defer close-then-delete 注释略夸大（P3）** [REFACTOR]: `internal/session/router_lifecycle.go:544-548` 注释说 "close BEFORE delete so a caller dispatched between 'lock acquired' and 'delete returned' observes the closed channel from the still-present map entry, not a fresh nil"。实际上整个 defer 块在 r.mu 内，任何 reader 读 `r.spawningKeys[key]` 都需先取 r.mu，不可能观察到 close 与 delete 之间的中间态。close/delete 顺序对正确性等价（reader 已持 ch 引用，与 map 无关）。 *(已实施：注释重写为 "close-before-delete 是为可读性不是正确性；waiter 通过持有 channel 引用而非 map lookup 唤醒"，加 R248-GO-3 anchor。)*
- [ ] **R248-GO-4 — Hub.Shutdown wiredLinkers=nil 在 clientWG.Wait 之前（P3）** [REFACTOR]: `internal/server/wshub.go:485-493` 顺序为 wiredLinkers=nil → clientWG.Wait。一个 in-flight readPump 调 handleSubscribe → completeSubscribe → maybeWireLinkerTailer 可能在 wiredLinkers 已被清空后到达；现有 nil-guard `if h.wiredLinkers == nil { return }` 处理之，但语义微妙（"shutdown 后不再接受新 wiring" 而非 "wiring 仍可用直到 wait 完成"）。方案 A：把 wiredLinkers=nil 移到 clientWG.Wait 之后，与 sendWG 屏障对齐。方案 B：注释强化 "clientWG 内的 maybeWireLinkerTailer 调用必须容忍 wiredLinkers==nil；Shutdown 提前 nil 是为加速 GC"。当前能跑但 review 不直观。
- [ ] **R248-GO-5 — wiredLinkers map[interface]struct{} dedup 跨 dynamic-type 假阳性（P3）** [REFACTOR]: `internal/server/wshub.go:193` 接口键 dedup 走 (dynamic type, pointer value)。当前 *cli.SubagentLinker 是唯一实现，1 process : 1 linker pointer，dedup 等价 pointer-keyed。未来若两后端的 AgentLinker 实现碰巧持相同 unsafe.Pointer 但 dynamic-type 不同（罕见），dedup 仍正确（type 区分）；但若两实例不同 dynamic-type 共享同一 vtable，map 视为不同 key 导致重复 OnResolve 注册。方案：当前是假设性风险，cli 只产 *cli.SubagentLinker。注释加 "interface 键 dedup 依赖 (T,P) 元组；多后端共存场景需重新评估"。或改 map[any]struct{} 显式标注。

### 已确认无问题（保留观察）

- spawningKeys close+delete 在 r.mu 内，waiter 持 ch 引用 select 唤醒 — 无 ABA / 无 missed-wakeup（panic 路径也跑 defer）
- spawnSession defer 与 GetOrCreate retry 循环锁交接正确（每个 return 路径都先 r.mu.Unlock，defer 重新 Lock）
- ReconnectShims `_, spawning := r.spawningKeys[key]` 仅做 presence check，map 值类型变化无影响
- wshub split clientWG.Add/Done 平衡：upgrade.go Add(2) + 两 pump defer Done；completeSubscribe Add(1) 在 h.mu 内 + eventPushLoop defer Done；BroadcastSessionsUpdate Add(1) + AfterFunc defer Done（Stop()=true 时 Shutdown 替补 Done）
- sendWG 全走 TrackSend 屏障，sendClosed 标志位置 clientWG.Wait 之后 sendWG.Wait 之前（防 readPump 出口处的 handleRemoteSend Add(1) 漏 Wait）
- Lock order h.mu → eventLog.subMu 在 split 后未变；wshub_eventpush.go 的 resubscribeEvents oldUnsub() 已挪到 h.mu 释放后
- dispatchCapabilities{s:s} 在 Start() 时构造（server.go:919），s 已 New() 完整初始化 — 无 partial-init
- AgentLinker typed-nil：maybeWireLinkerTailer (wshub_agent.go:71) 与 linkerForSession (dashboard_agent_events.go:90-94) 都先在 *cli.SubagentLinker concrete 层 nil-check 后才 promote，handleAgentSubscribe (wshub_agent.go:159) 直接拿 concrete 不涉及 interface
- Timer.Reset 模式在 Go 1.26 安全（go.mod 1.26.3）

### 测试覆盖 / contract anchor


### 架构 / API 设计

- [ ] **R248-ARCH-1 — Capabilities interface 内聚弱：必需 + 可选混在一起（P1）** [REFACTOR]: `internal/dispatch/capabilities.go:34` Send 是 contract-required（panic）、Takeover/ReplyFooter 是 contract-optional（默认值），三方法 lifetime + failure-mode 都不同被强行打包。方案：拆 `MessageSender`（必需）+ `TakeoverHook` / `ReplyFooterHook`（可选 1-method interface），或保持 Capabilities 但 Send 拎到 DispatcherConfig.Sender 单独字段（required）。
- [ ] **R248-ARCH-3 — DispatcherConfig.SendFn 等 Deprecated 字段无清理路径（P2）** [REFACTOR]: `internal/dispatch/dispatch.go:212-244` 同时暴露 Capabilities + 3 个 Deprecated *Fn，godoc 说 transition 但没指明何时摘除。naozhi 的 sessionSendLegacy 已固化 5 轮 review。方案：加 R-DEPRECATED-DISPATCH-FN tracker 含具体清理触发；或在本批把测试 caller 改 closureCapabilities literal，直接删 Deprecated 字段。
- [ ] **R248-ARCH-4 — AgentLinker interface 4 方法偏宽 + cli.LinkInfo 跨包泄漏（P2）** [REFACTOR]: `internal/session/agentlink/agentlink.go:33-50` interface 在 session/agentlink 子包但 import cli + 返 cli.LinkInfo — 依赖反转没真正发生。OnResolve(taskID, toolUseID, internalAgentID) 是 Claude-CLI-specific 签名，ACP backend 无法实现 noop（只能传空字符串假装）。方案：(a) LinkInfo 复制到 agentlink；(b) OnResolve 改 OnAgentReady(AgentID) + Lookup(AgentID)；(c) 拆 Resolver / Notifier / PathProvider 三小接口。
- [ ] **R248-ARCH-5 — AgentLinker interface 放 session 子包方向不对（P2）** [REFACTOR]: `internal/session/agentlink/agentlink.go` Go 风格"接口放 consumer 处定义"。唯一 producer cli.SubagentLinker、唯一 consumer server，session 包内 interface 不被使用 — 当前是第三方位置，server import path 比直接定义在 server 还长。方案：移到 internal/server/agentlink.go consumer-local；*cli.SubagentLinker 隐式满足。
- [ ] **R248-ARCH-6 — Hub struct 28+ 字段 god-struct 仍存（P2）** [REFACTOR]: `internal/server/wshub.go:51-194` PR #327 split 标榜 god-object 拆分但只拆文件没拆 struct。当前 split 让继续抽 BroadcastDispatcher / SubscriberRegistry 等 struct 更难（method receiver 都是 *Hub）。方案：wshub.go 头部加 NEEDS-DESIGN 明确"下一阶段：抽 BroadcastDispatcher / SubscriberRegistry / SendCoordinator 三个 struct"。
- [ ] **R248-ARCH-7 — wshub.go SetScheduler/SetUploadStore/SetScratchPool setter 滞留（P2）** [REFACTOR]: `internal/server/wshub.go:308-321` split 把 5 类职责拆出 5 文件，但 3 个 setter 留 wshub.go；cronHubOps interface 定义（line 303）唯一 caller 是 broadcast/send 路径。方案：搬到 wshub_send.go 或新建 wshub_lifecycle.go；或注释明确 "wshub.go = struct + ctor + lifecycle，handlers 在 wshub_*.go"。
- [ ] **R248-ARCH-8 — PR #330 commit oversold 关闭 R242-GO-10 / R242-ARCH-17 等（P2）** [REFACTOR]: `internal/server/wshub.go:86-88` 仍自陈 "NEEDS-DESIGN R242-GO-10: 改抽 MessageEnqueuer interface"。PR #330 抽 Capabilities 同时声称关闭一组 ARCH 条目但 dashboard send 路径（Hub.queue 字段）仍直接引 *dispatch.MessageQueue。方案：要么删 commit msg 的"同时关闭"清单，要么补 follow-up PR 把 Hub.queue 抽成 MessageEnqueuer interface（Enqueue/Discard/Mode/CollectDelay/DoneOrDrain/ShouldNotify 6 方法）。
- [ ] **R248-ARCH-9 — dispatchCapabilities + Hub.sendWithBroadcast 双层 nil-fallback 职责模糊（P3）** [REFACTOR]: `internal/server/send.go:644-670` Capabilities 层 panic（生产语义）vs Hub 层 hub==nil 悄悄降级 sess.Send（headless 语义），同 send 调用在不同 wiring 下行为差异极大但无显式 mode 字段。方案：headless 应显式声明 HeadlessCapabilities 而非 Server.sendWithBroadcast 内部隐式判断。
- [~] **R248-ARCH-10 — spawningKeys close-before-delete 靠 godoc 注释而非类型保证（P3）** [REFACTOR]: `internal/session/router_lifecycle.go:540-559` lock-order-by-convention 脆弱模式。方案：封私有方法 `r.markSpawnDoneLocked(key)` (caller 持锁)，单点维护两步顺序 — 类型不能强保证但调用方至少不能搞错局部顺序。

### 代码质量 / godoc / 命名

- [x] **R248-CR-1 — capabilities.go 注释/代码比 1:1（P1）** [REFACTOR]: `internal/dispatch/capabilities.go:1-91` 类型 godoc 含 60+ 行混杂 review-history 与契约说明，超过实际 type+method 代码量。 *(已实施：Capabilities interface godoc 25→9 行；NoopCapabilities 14→6 行；保留契约说明 + R243-ARCH-10 + R248-ARCH-2 anchor。)*
- [x] **R248-CR-2 — wshub.go wiredLinkers 单字段堆叠 5 个 review 编号（P1）** [REFACTOR]: `internal/server/wshub.go:175-194` 注释提 R201/R230/R231/R233B/R239 等价于 git log 摘要。 *(已实施：godoc 18→9 行；保留 R201-CRIT-2 (历史 leak fix) + R239-ARCH-I (interface 化等价性) 两 anchor，其他历史编号删除。)*
- [x] **R248-CR-3 — dispatchCapabilities 类型名与 dispatch.Capabilities interface 易混淆（P1）** [REFACTOR]: `internal/server/send.go:644` capabilities.go godoc line 24 已用了 "serverCapabilities" — 文档与实际类型名不一致。 *(PR #343 已实施：dispatchCapabilities → serverCaps；server/send.go + server.go + dispatch/capabilities.go godoc 同步对齐。)*
- [~] **R248-CR-4 — agentlink.go 包级 godoc 17 行 + 三段 Anchor 提 4 个编号（P2）** [REFACTOR]: `internal/session/agentlink/agentlink.go:1-17` 包文档读者多数 IDE hover 看不下。方案：留前 5 行（接口存在意义 + 谁实现），Anchor 段移到 docs/TODO.md。
- [ ] **R248-CR-5 — AgentLinker.Query vs QueryOrResolveFast 命名差异不表达行为（P2）** [REFACTOR]: `internal/session/agentlink/agentlink.go:33-50` 两方法返回 (LinkInfo, bool) 签名一样，仅靠 godoc 区分语义。方案：改 Lookup（缓存只读）+ Resolve（含 stat fallback）。
- [x] **R248-CR-6 — NoopCapabilities.Send panic 契约埋在 type docstring 中段（P2）** [REFACTOR]: `internal/dispatch/capabilities.go:65-83` 方法本身只有 "see type docstring"，IDE go-to-definition 跳方法时只看 method godoc，panic 契约对 hover 隐形。方案：把 "panics with msg X，原因 Y" 完整写在方法 godoc 上方。 *(已实现：capabilities.go:60-62 Send method godoc 完整说明 panic 原因 / Takeover/ReplyFooter 各有 godoc)*
- [ ] **R248-CR-7 — wshub_agent.go vs wshub_subscribe.go handleAgentSubscribe 文件归属（P2）** [REFACTOR]: `internal/server/wshub_agent.go` 包含 (a) maybeWireLinkerTailer 内部 wiring + (b) enrichSnapshot + (c) handleAgentSubscribe WS handler 三种职责。方案：把 (c) 并入 wshub_subscribe.go（同 ValidateSessionKey 入口模式），wshub_agent.go 留 wiring + tailer 桥。
- [x] **R248-CR-8 — dispatchCapabilities 三个方法各 3 行 forward 缺 godoc 解释（P3）** [REFACTOR]: `internal/server/send.go:649-669` 应说明"为什么不用 method value &c.s.sendWithBroadcast" — *Server 是接口受体，method value 会对每次调用 alloc funcval。 *(已实施：serverCaps type godoc 加 "WHY METHODS, NOT METHOD-VALUE CLOSURES (R248-CR-8)" 段说明 funcval / receiver-box / 测试可换 fake 三点；Send/Takeover/ReplyFooter 三方法各加 1 行 anchor 指回 type godoc 避免重复。R248-CR-8。)*

### 性能（已审，全部确认无回归 — 见 Reviewer 3 报告）

- 4 PR 整体 perf-中性偏正：interface dispatch 开销可忽略（per-turn 调用 1-3 次 vs 100ms-30s LLM I/O）；dispatchCapabilities 单指针 inline 无 boxing；closureCapabilities 仅 deprecated 路径多 1 跳；PR #329 实质是性能优化（去 20ms tick + 减 *time.Timer alloc）；PR #330 Dispatcher 结构体净减 8 字节；PR #331 wiredLinkers 接口 dedup 仍按 (itab, ptr) 工作。3 项 P3 均建议先 profile 再动。

## Round 247 — 5-agent 并行 code review 第 57 轮（2026-05-24，紧随 Round 246）NEEDS-DESIGN

> 紧随 Round 246 的第 57 轮 5-reviewer 并行扫描（Go / 安全 / 性能 / 代码质量 / 架构）。基线 0336c19。in-flight PR=0；REPEAT 提示 ARCH-1/2/10/11 REPEAT-7~20 / 多处 PERF/SEC REPEAT-2~3 已避开（本轮专注：framing.go 双 free / freshSnap pre-snapshot store / cron findByPrefix sentinel / ListJobs nil slice / ringRead cap=0 panic / csrf scheme 不匹配 / dashboard cron trigger+preview rate limit / MkdirAll 0700 不修复 existing dir / agent_view onclick 模板 / errclass 三套并行）。
>
> 本轮直接修 8 项落地（v4 三段限额：P1-SEC 无上限×1 + 非安全简单 ≤6 共 5 + BREAKING-LOCAL ≤2 共 2）：
> - [R247-PERF-1] eventlog/persist/framing.go 删 missing-newline 路径双 ReleaseFramedBody（pool 双 free 引发后续帧 reader race）
> - [R247-GO-1] cron/scheduler_run.go 删 line 411 redundant `inflight.freshSnap.Store(j.FreshContext)` pre-snapshot 写（snap.fresh 是权威值，且 j.FreshContext 无锁读会触 -race）
> - [R247-GO-2] cron 引入 `ErrAmbiguousPrefix` sentinel + `findByPrefix` ambiguous 分支 `%w` 包装，让 dashboard handler 能 errors.Is 区分 not-found vs ambiguous
> - [R247-GO-3] (BREAKING-LOCAL) cron/scheduler_jobs.go `ListJobs` nil slice 改 make([]Job, 0, len)；空 list JSON 从 null → []
> - [R247-GO-4] (BREAKING-LOCAL) cron/runstore.go ringRead/ringSnapshot 加 cap(ring)==0 守卫防 integer divide-by-zero panic
> - [R247-SEC-1] (BREAKING-LOCAL) server/csrf.go sameOriginOK 加 scheme 比对（HTTPS 请求拒绝 `Origin: http://host`）+ `requestScheme(r, trustedProxy)` helper — **REVERTED 2026-05-25**：CDN→origin HTTP-only 链路下 `X-Forwarded-Proto=http` 强制注入，HTTPS viewer 访问 `Origin: https://host` 触发 scheme mismatch 403；auth cookie 已是 host-scoped + SameSite=Strict，scheme-match 边际安全收益 ~0。
> - [R247-SEC-2] server/dashboard_cron.go handleTrigger 加 per-IP rate limit（30 req/min，burst 6，统一 `writeLimiter` 桶）
> - [R247-SEC-3] server/dashboard_cron.go handlePreview 复用同 `writeLimiter` 桶
>
> 其余 ~80 项全登记 NEEDS-DESIGN，按 v4 分类标签 [REFACTOR]/[BREAKING-LOCAL]/[REPEAT-N]。

### Go（剩余）

- [ ] **R247-GO-5 — runDeadlineWatchdog goroutine ctx 缺 hard timeout（P1）** [BREAKING-LOCAL]: `internal/cron/scheduler_run.go:339-350` watchdog 内调 `sess.InterruptViaControl()` 若 shim 写 wedge 可阻塞，导致 wrapper goroutine 持 abortCh 槽位超过 stopBudget。方案：select{<-abortCh} 配 hard timeout 或 sendCtx-like 传入 InterruptViaControl。
- [ ] **R247-GO-6 — runstore warmCache LoadOrStore 与 cacheGet 不同 entry 实例 race（P1）** [BREAKING-LOCAL]: `internal/cron/runstore.go:494-509` 若 cacheInvalidate(DeleteJob) 在两次 LoadOrStore 之间执行，cacheGet 的 OLD entry 永远 warm=false → 返回 (nil,false) 静默 miss。方案：把 entry 指针从 cacheGet 透传给 warmCache，或 cacheGet 在 warmCache 后再 Load() 切到当前 entry。
- [ ] **R247-GO-7 — Stop() gcWG.Wait wrapper goroutine 漏写 contract（P1）** [REPEAT-2]: `internal/cron/scheduler.go:694-705` 与 triggerWG 同 leak 模式但 CONTRACT 块只覆盖 triggerWG。方案：扩展同一 CONTRACT 注释或让 trimAll 观测 stopCtx。
- [ ] **R247-GO-8 — Append path-traversal 防御深度（P2）** [BREAKING-LOCAL]: `internal/cron/runstore.go:282-339` IsValidID 已防 hex-only 输入，但写路径无 filepath.Rel 校验。方案：MkdirAll 前 `filepath.Rel(s.root, dir)` 校验，与 readRun Lstat 对齐。
- [x] **R247-GO-9 — NextRun s.cron.Entry 在 RUnlock 后读（P2）** [REPEAT-2]: `internal/cron/scheduler_jobs.go:761-770` 与 R246-GO-1 同模式不同 site；UpdateJob 的 Remove+AddFunc 可在窗口内返回 Entry{}。方案：Entry() 移到 RLock 内或 godoc 标注 best-effort。 — 解决 2026-05-25：`NextRun` 改为 `s.mu.RLock(); defer s.mu.RUnlock()` 跨整个 entryID 读 + `s.cron.Entry(entryID)` 调用；windowed UpdateJob Remove+AddFunc 不再可在 RUnlock 后窗口内让 Entry 返回零值；godoc 加 R247-GO-9 段解释 robfig/cron 内部锁与 s.mu 无 lock-order 冲突（cron 锁不回调回 scheduler）。`go test ./internal/cron/ -count=1` 全通过。
- [ ] **R247-GO-10 — EnsureStub 总返 true 不反映 register 失败（P2）** [REFACTOR]: `internal/cron/scheduler.go:617-643` registerStubByValue 不返 error，调用方误以为 stub 已注册。方案：让 RegisterCronStubWithChain 返 error 一路上抛。
- [~] **R247-GO-11 — resetRouterStub 缺 nil-receiver 防御（P2）** [BREAKING-LOCAL]: `internal/cron/scheduler.go:783-788` 与 sibling StartedAt/KnownSessionIDs 不一致；测试构造部分 Scheduler 调 DeleteJobByID 会 NPE。方案：开头 `if s == nil { return }`。
- [ ] **R247-GO-12 — runDeadlineWatchdog 每 tick spawn goroutine（P2）** [REFACTOR]: `internal/cron/scheduler_run.go:621-630` 50 jobs × 1Hz = 50 goroutine/秒 启停，99% case 无效。方案：高 jobTimeout 跳过 watchdog 或共享 watchdog goroutine。
- [ ] **R247-GO-13 — AddJob 10 collision retry 日志 flood（P3）** [BREAKING-LOCAL]: `internal/cron/scheduler_jobs.go:88-109` mock generator 死循环时刷 10 行 log。方案：仅 i==0 记 Warn 或检测确定性 generator 提前 bail。
- [x] **R247-GO-14 — Stop() 文档实际 wall-clock 35s（P3）** [BREAKING-LOCAL]: `internal/cron/scheduler.go:684-777` godoc 说 "bounded by stopBudget(30s)"，实测 + gcWaitBudget(5s) = 35s。方案：godoc 修正或共享 budget。 — F2 cron-fix 2026-05-24: godoc 改"gcWaitBudget + stopBudget = 35s default; both independent timers"，明示组合上限并交叉引用 With*Budget helper（见 R247-CR-18）。文案修正路线（不 share budget），无 caller 改动。
- [ ] **R247-GO-15 — saveMarshaledSeq 失败时 lastSavedSeq 未更新注释缺失（P3）** [BREAKING-LOCAL]: `internal/cron/scheduler_persist.go:101-135` 当前正确但缺 godoc 解释。方案：注释解释为何不 bump。
- [ ] **R247-GO-16 — spawn budget 警告阈值 jobTimeout/2 噪音（P3）** [BREAKING-LOCAL]: `internal/cron/scheduler_run.go:606-613` cold-start fresh-context preflight 触发误报。方案：阈值 → jobTimeout 或区分 fresh vs hot。
- [ ] **R247-GO-17 — cacheGet R241-CR-6 注释失效（P3）** [REPEAT-2]: `internal/cron/runstore.go:449-489` 注释说 "always sets warm=true" 但 R247-GO-6 race 后不成立。方案：与 R247-GO-6 一并修或更新注释。

### 安全（剩余）

- [ ] **R247-SEC-4 — agent_view.js inline onclick attr 嵌 escAttr（P1）** [REFACTOR]: `internal/server/static/agent_view.js:69` `onclick="...switchTo(\\'" + escAttr(a.taskId) + "\\')"` —— escAttr 仅 HTML-escape 不处理 JS string；当前 a.taskId 由 server `agentTaskIDRe ^[a-z0-9]{1,32}$` 兜底但 sink 错误层。方案：addEventListener + dataset，移除 inline onclick。
- [ ] **R247-SEC-5 — selfupdate.fetchFile 初始 URL 无显式 https 断言（P2）** [REFACTOR]: `internal/selfupdate/selfupdate.go:313` 仅 CheckRedirect 内卡 https。方案：req.URL.Scheme!="https" 早拒，与 redirect 路径对齐。
- [ ] **R247-SEC-6 — transcribe ffmpeg 无 wall-clock 上限（P2）** [BREAKING-LOCAL]: `internal/transcribe/convert.go:104` 仅靠外层 ctx；构造 audio 可长时间占 transcribeSemCap=3 槽。方案：argv 加 `-t 600` 解码上限。
- [x] **R247-SEC-7 — handleConfigPut 缺 per-IP rate limit（P2）** [REFACTOR]: `internal/server/project_api.go:159` 写盘 + WS 广播无频次墙。方案：加 ipLimiter 或 projectMgr.UpdateConfig 5/sec gate。 *(已实施：ProjectHandlers 加 configPutLimiter (5/sec, burst 5, ≈300/min) 字段；handleConfigPut 在所有工作前 AllowRequest 守门，超限返 429 + Retry-After:1。server.go 用 newIPLimiterWithProxy 串 opts.TrustedProxy 与 filesExistsLimiter 同 pattern 接入。tests 手搓 ProjectHandlers 的 nil-safe 路径未变。)*
- [ ] **R247-SEC-8 — uploadOwner crypto/rand 失败回退 clientIP（P2）** [REPEAT-2]: `internal/server/dashboard_send.go:140-148` 与 R246-SEC-8 同根因不同 site。方案：失败返 503。
- [~] **R247-SEC-9 — shortPromoteSuffix 32-bit 熵（P2）** [BREAKING-LOCAL]: `internal/server/dashboard_scratch.go:351` birthday-bound ~2^16；与 anonCookie/upload 16 byte 不齐。方案：8 字节（64-bit）。
- [ ] **R247-SEC-10 — isSensitiveDownloadName 不卡父目录段（P2）** [BREAKING-LOCAL]: `internal/server/project_files.go:1212` `secrets/db.yaml` `.ssh/foo` 不命中 basename。方案：sensitivePathSegments allowlist。
- [ ] **R247-SEC-11 — parseAttachmentFile 用 declared Content-Type 决定 size cap（P2）** [REPEAT-2]: `internal/server/dashboard_send.go:172` 与 R246-SEC-9 同根因不同 fork；io.ReadAll 在 magic-byte 复核前已 buffer 32MB。方案：magic-byte 优先读 head 决定 cap。
- [ ] **R247-SEC-12 — eventlog persister MkdirAll 0700 不修复 existing dir mode（P2）** [REFACTOR]: `internal/eventlog/persist/persister.go:202` 攻击者预创建 0755/0777 父目录可绕过 0700 contract。方案：MkdirAll 后 os.Chmod 校正或 Lstat 检查 mode != 0700 拒启。
- [ ] **R247-SEC-13 — cron runstore MkdirAll 0700 同 mode 漏修（P2）** [REFACTOR]: `internal/cron/runstore.go:236-301` 同上。
- [ ] **R247-SEC-14 — attachment store MkdirAll 0700 同 mode 漏修（P2）** [REFACTOR]: `internal/attachment/store.go:232` workspace 共享场景下他 UID 可预创建。
- [ ] **R247-SEC-15 — mintAnonCookie 30d MaxAge 永不轮换（P2）** [BREAKING-LOCAL]: `internal/server/dashboard_send.go:39-52` token 模式切换 / 服务重启不清。方案：缩到 7d 或登录态变化时强制 expire。
- [x] **R247-SEC-16 — ownerKeyFromCookie sha256[:8] 64-bit 熵（P2）** [BREAKING-LOCAL]: `internal/server/dashboard_send.go:114-120` 同 BroadcastCronResult/ETag short-hash。方案：sha256[:16]（128-bit）。 *(已实施：3 处 owner-key 派生统一改 sha256[:16] (128-bit)：dashboard_send.go ownerKeyFromCookie + Bearer 路径，wshub_upgrade.go WS token-auth 路径。test ws_test.go TestHandleAuth_WSToken_SetsUploadOwner 长度断言 16→32 同步。BroadcastCronResult 路径不含 owner-key 短哈希；project_files.go ETag 短哈希非安全语义保留 [:8]。本批限于 auth-derived owner key，与 R246-SEC-5 / R247-SEC-24 / R67-SEC-1 ≥128-bit 习语对齐。)*
- [~] **R247-SEC-17 — cookieMAC 缺 cookie-rotation（P2）** [REPEAT-3]: `internal/server/dashboard_auth.go:117-121` 与 R243-SEC-13/R245-SEC-2/R242-SEC-5 同根因；HMAC 输入未含 ts/nonce。方案：MAC 输入加 cookie-gen ts。
- [ ] **R247-SEC-18 — collectTranscripts 直拼 alternative.Transcript（P3）** [REFACTOR]: `internal/transcribe/transcribe.go:194` 未过 SanitizeForLog/IsLogInjectionRune。方案：与 cron sanitiseRunResult 对齐过滤 bidi/C1。
- [x] **R247-SEC-19 — sysession runner BinPath 未 LookPath 校验（P3）** [REPEAT-2]: `internal/sysession/runner.go:79` 与 R245-SEC-15 同根因不同位置。 *(已实施：NewRunner 在 absolute / contains-separator 分支补 os.Stat 校验，要求 regular file + executable bit；与 resolveBinPathFromEnv 的 PATH 分支同 gate；保留 symlink-following 兼容 distro 包 /usr/local/bin/claude 链。)*
- [x] **R247-SEC-20 — uploadStore.Put crypto/rand 失败 panic（P3）** [REFACTOR]: `internal/server/upload_store.go:122` 让 HTTP server 倒塌。方案：返 errUploadStoreFull + slog.Error。 *(已实施：upload_store.go Put rand.Read 失败分支已返 errUploadStoreFull + slog.Error("uploadStore Put: crypto/rand unavailable")，不再 panic；callers 已对 errUploadStoreFull 走"稍后再试"重试路径。本批补 TestUploadStorePut_NoPanicOnRandFailure_SourceContract 源码扫描契约测试 pin Put body 含 errUploadStoreFull 且不含 panic( 防回归。)*
- [~] **R247-SEC-21 — cliAvailable os.Stat 暴露二进制路径（P3）** [REFACTOR]: `internal/server/health.go:283-289` 认证后 token 窃取者可探主机布局。方案：返常量 boolean 不区分 IO 类型。
- [ ] **R247-SEC-22 — reverseUpgrader.CheckOrigin 仅靠 Origin 缺失判 m2m（P3）** [REPEAT-3]: `internal/node/reverseserver.go:69-73` 反代剥 Origin 场景下 browser-XSS 端可凑无 Origin 请求。方案：r.TLS != nil 强制或 explicit insecure_node 配置。
- [ ] **R247-SEC-23 — CSP font-src https://cdn.jsdelivr.net 无 SRI（P3）** [REPEAT-3]: `internal/server/dashboard.go:503` 与 R246-SEC-10 同根因；KaTeX woff2 走同信任链。方案：vendored //go:embed 或 require-sri-for font。
- [~] **R247-SEC-24 — resume key var rb [8]byte 64-bit 熵（P3）** [REPEAT-2]: `internal/server/dashboard_session.go:1052` 与 R246-SEC-5 同根因不同 site。方案：16 字节。
- [ ] **R247-SEC-25 — netutil clientIP trustedProxy XFF 缺失回退 RemoteAddr（P3）** [REPEAT-3]: `internal/netutil/clientip.go:25-46` 所有 client 折一桶。方案：trustedProxy=true 且 XFF 空时返 400。

### 性能（剩余）

- [x] **R247-PERF-2 — flattenAssistantEvent O(N) 前插 + 整 slice reindex（P1）** [REFACTOR]: `internal/server/dashboard_cron_transcript.go:665-676` 500 行 transcript 每 assistant event 都触发。方案：先 emit assistant 再 emit tool_use 或 prealloc + shift 一次。 → 改 two-pass：第一遍 aggregate textBuf + count tool_use 预算 cap，第二遍 emit。assistant 在前 tool_use 在后顺序+ index 由首写就位，消除 `append([]T{a}, out...)` 整 slice copy + reindex 循环。同时按精确 totalTurns prealloc 收口 R247-PERF-18 同函数 make([]T,0,2) 每行 alloc（双修一锅）。go test ./internal/server/ -run "Cron|Transcript" 通过。
- [ ] **R247-PERF-3 — KnownSessionIDs 每次重建 jobs×200 map（P1）** [REPEAT-2]: `internal/cron/scheduler_session.go:68-108` 与 R245-PERF-2/R242-PERF-7 同根因仍未消除。方案：atomic.Pointer[snapshot] + 30s TTL，finishRun/DeleteJob 主动失效。
- [ ] **R247-PERF-4 — ListAllJobsWithNextRun 每次 4 个 slice/map alloc（P1）** [REFACTOR]: `internal/cron/scheduler_jobs.go:184-213` dashboard 1Hz poll。方案：sync.Pool 复用 pairs/result，maps.Clear+复用 nextByID。
- [ ] **R247-PERF-5 — proc_linux fmt.Sprintf("/proc/%d/...") + strings.Fields（P1）** [REFACTOR]: `internal/discovery/proc_linux.go:27,39,56` 每 PID 反射拼接 + 整 string copy。方案：strconv.Itoa builder + byte-level scan。
- [ ] **R247-PERF-6 — handleList 每个 project alloc map[string]any 8 字段（P1）** [REFACTOR]: `internal/server/project_api.go:89-134` dashboard 多 tab。方案：命名 struct 化，redactGitRemoteURL 缓存到 *Project 字段。
- [ ] **R247-PERF-7 — replyText fmt.Sprintf "[Cron %s] %s"（P2）** [REFACTOR]: `internal/cron/scheduler_run.go:709` 已大字符串再拼 reflect format。方案：strings.Builder。
- [ ] **R247-PERF-8 — deliverNotice "[Cron %s]" 三处 fmt.Sprintf（P2）** [REFACTOR]: `internal/cron/scheduler_run.go:254,567,671`。方案：抽 helper + Builder。
- [ ] **R247-PERF-9 — diskListNewestFirst 串行 readRun（P2）** [REPEAT-3]: `internal/cron/runstore.go:617-631` 与 R246-PERF-2 同根因。方案：recentCacheEntry 缓存 sorted slice + 8 worker pool 并行解码。
- [ ] **R247-PERF-10 — Append jobLock 期间 MkdirAll+Marshal+WriteFileAtomic（P2）** [REPEAT-2]: `internal/cron/runstore.go:282-339` 每条 syscall。方案：json.Marshal pool + sync.Once-per-jobID MkdirAll。
- [ ] **R247-PERF-11 — marshalJobsLocked 每次 mutation 全表 sort+Marshal（P2）** [REPEAT-2]: `internal/cron/scheduler_persist.go:45-58` 50 jobs ≈ 100KB write × 每 finishRun。方案：Encoder + bytes.Buffer pool。
- [x] **R247-PERF-12 — protocol_acp WriteMessage Marshal 无 pool（P2）** [REFACTOR]: `internal/cli/protocol_acp.go:308-356,675-680` ACP turn / permissionResponse 高频。方案：镜像 shimSendBufPool。 — F1 verify-stale 2026-05-25：WriteMessage (protocol_acp.go:437) 与 HandleEvent permissionResponse (protocol_acp.go:773) 均已用 acpEncPool（acpEncBuf{buf, enc} pool 与 shimSendBufPool 镜像 + acpEncBufMaxCap 64KB 上限）。当前文件无残留 json.Marshal 在该两热路径。
- [x] **R247-PERF-13 — eventlog Entries 全 ring 拷贝 ~140KB/call（P2）** [REPEAT-3]: `internal/cli/eventlog.go:1217-1245` dashboard subscribe 路径。方案：LastN 上限或 sync.Pool slice 复用。 — F1 [REPEAT-3]: 加 EntriesAppend(dst) / LastNAppend(dst,n) / Count() 让 sync.Pool 缓存 backing array；Entries/LastN 退化为 nil-dst 包装。提供 zero-alloc 路径让 dashboard subscribe 可逐步迁移。
- [ ] **R247-PERF-14 — eventlog Subscribe 每次 alloc subscriber（P2）** [REFACTOR]: `internal/cli/eventlog.go:1141-1166` dashboard 高频 reconnect 形成分配峰。方案：close-once 限制改 broadcast cond。
- [x] **R247-PERF-15 — handleList projectList 每次 1Hz 全量 alloc（P2）** [REPEAT-3]: `internal/server/dashboard_session.go:554-572` R230C-PERF-7 已知。方案：atomic.Pointer + projectMgr 版本号失效。 *(已实施：projectListCache atomic.Pointer 1s-bucket cache + projectListLocalAt helper；remote-node 合并路径 copy + grow 先复制再 append，保证 cached read-only header 不被 mutate。1s 分辨率人类操作不可感知，省去 Manager.Version() 跨包 hook。R247-PERF-15 [REPEAT-3]。)*
- [ ] **R247-PERF-16 — RecentSessions 无 prealloc（P2）** [REFACTOR]: `internal/discovery/recent.go:84-178` 7day×多 project 规模可观。方案：make 估上限。
- [ ] **R247-PERF-17 — protocol_acp base64.EncodeToString 全 alloc（P2）** [REFACTOR]: `internal/cli/protocol_acp.go:322-339` 多图 turn 浪费。方案：base64.StdEncoding.AppendEncode 写 pre-grown buffer。
- [x] **R247-PERF-18 — flattenAssistantEvent make([]T,0,2) 每行（P2）** [REPEAT-2]: `internal/server/dashboard_cron_transcript.go:625-679` 500 行 = 500 alloc。方案：caller-provided scratch slice。 → 与 R247-PERF-2 同根因合并修复：精确 totalTurns 预算（toolUseCount + 1 if hasText），消除 `make([]T, 0, 2)` underallocate-then-grow 双 alloc，单点 make 收口。
- [ ] **R247-PERF-19 — recentFromParsedIndex jsonlMtimes map 重建（P2）** [REFACTOR]: `internal/discovery/recent.go:329-356` 已 sorted slice 可二分。方案：sort.Search 替 map。
- [~] **R247-PERF-20 — Tick highwater 全量拷贝（P2）** [REFACTOR]: `internal/sysession/auto_titler.go:181-194` 多数 key 当 tick 不访问。方案：atomic.Pointer[map] CoW。
- [ ] **R247-PERF-21 — buildUserEntry 每图 spawn goroutine（P3）** [REFACTOR]: `internal/cli/process_send.go:51-76` cap 4 sem 但仍 8KB stack × N。方案：worker pool。
- [x] **R247-PERF-22 — HandleEvent permissionResponse Atoi+Itoa 来回（P3）** [REFACTOR]: `internal/cli/protocol_acp.go:660-666`。方案：直接 json.RawMessage 写。 — 解决 2026-05-24：Atoi 仅用作 validation，原始 ev.RPCRequestID 字符串复用为 RawMessage（已是合法 JSON 数字字面量），省 1 alloc/permission；string-id 路径维持 json.Marshal 以保留 escape 处理。
- [ ] **R247-PERF-23 — Enqueue 队列满 O(N) memmove（P3）** [REPEAT-2]: `internal/dispatch/msgqueue.go:184-208` MaxDepth=16 拷贝 15。方案：环形 buffer。
- [ ] **R247-PERF-24 — workDirUnderRoot 每 execute EvalSymlinks（P3）** [REFACTOR]: `internal/cron/scheduler.go:177-189` 长寿命下重复 syscall。方案：TTL 缓存。
- [~] **R247-PERF-25 — agent_tailer Shutdown 重新 alloc map（P3）** [REFACTOR]: `internal/server/agent_tailer.go:565-578`。方案：clear(r.byTask) (go1.21+)。
- [~] **R247-PERF-26 — eventlog persister tickFlush 无序 map iter（P3）** [REFACTOR]: `internal/eventlog/persist/persister.go:629-657` 高 N session 抖动。方案：sorted dirty heap。

### 代码质量（剩余）

- [ ] **R247-CR-1 — DeleteJobByID/PauseJobByID/ResumeJobByID 三 dashboard 入口同构（P1）** [REPEAT-3]: `internal/cron/scheduler_jobs.go:273-388` ~120 行重复 closure + lock + persist 框架。方案：抽 `withJobByID(id, op, postCleanup)` helper。
- [ ] **R247-CR-2 — DeleteJob/PauseJob/ResumeJob (prefix) 与 *ByID 第二组同构（P1）** [REPEAT-3]: `internal/cron/scheduler_jobs.go:687-758` ~150 行重复。方案：lookup 抽 helper，lockedOp+持久化共用。
- [ ] **R247-CR-3 — executeOpt 仍 345 行 cyclomatic >30（P1）** [REPEAT-3]: `internal/cron/scheduler_run.go:367-711` 与 R246-CR-001 同根因仍 open；本轮新增 sendCtx 双预算 warn / abort 通道收发 / classifyExecError 进一步推高复杂度。方案：runSpawn/runSend/runFinish 切分。
- [ ] **R247-CR-4 — Scheduler.Stop() 95 行 mini-state-machine（P2）** [REFACTOR]: `internal/cron/scheduler.go:684-777`。方案：抽 waitGCDrain/waitCronStop/waitTriggerWG/persistOnShutdown 四 helper。
- [~] **R247-CR-5 — `[Cron %s]` 中文文案 3 处 hardcode（P2）** [REPEAT-3]: `internal/cron/scheduler_run.go:567,671,709`。方案：抽 const + helper formatCronNotice(label, kind, body)。
- [ ] **R247-CR-6 — opts.ExtraArgs 三参数 slice 表达式纯防御性（P2）** [REFACTOR]: `internal/cron/scheduler_run.go:485-489` cron 后续从不 append。方案：slices.Clone 或注释 "future-proof"。
- [~] **R247-CR-7 — SetOnExecute/SetOnRunStarted/SetOnRunEnded 三 setter 同构（P2）** [REPEAT-3]: `internal/cron/scheduler_callbacks.go:61-88`。方案：泛型 setCallback[T] helper。
- [ ] **R247-CR-8 — *ByID 三入口注释暗示对称但 Pause/Resume 不删 runs（P2）** [REFACTOR]: `internal/cron/scheduler_jobs.go:273-388`。方案：godoc 显式区分或 helper 接 cleanup 钩子。
- [x] **R247-CR-9 — NotifyDefault 缺 godoc 行为契约（P2）** [REFACTOR]: `internal/cron/scheduler.go:472-482` 与 Location 不一致。方案：补 godoc。 — 解决 2026-05-25 (F2)：补全 godoc（snapshot at NewScheduler / 不支持 runtime 改 / zero NotifyTarget 表示未配置 / dashboard 用 IsSet 判断）+ 加 nil-receiver 防御与 Location/StartedAt 同模式。
- [~] **R247-CR-10 — registerJob AddFunc closure 与 executeIfNotDeletedOrPaused 同源（P2）** [REPEAT-3]: `internal/cron/scheduler_jobs.go:843-862`。方案：closure 直调 executeIfNotDeletedOrPaused。
- [ ] **R247-CR-11 — strHeap/timeHeap helper reset 路径反成噪音（P2）** [REFACTOR]: `internal/cron/runinflight.go:64-74,91-95` 与 R246-CR-011 同根因；本轮新发现 reset 不分配。方案：删 helper 或 cross-reference。
- [ ] **R247-CR-12 — runs 限额 const 散落（P2）** [REFACTOR]: `internal/cron/runstore.go:170-187` 大小写不统一。方案：集中到 limits.go。
- [x] **R247-CR-13 — emitOverlapSkipped godoc 引用已归档 review（P2）** [REFACTOR]: `internal/cron/scheduler_finish.go:281-299`。方案：简化 godoc 删历史引用。 — 解决 2026-05-25 (F2)：删 R233B-CR-2 review 编号引用；重组 godoc 先说 trigger 来源（CAS-reject tick / TriggerNow losing gate）再说 dual-event 行为契约 + skipPersist 用途。
- [ ] **R247-CR-14 — recordResultP0WithSanitised 64 行 P0 命名已无意义（P2）** [REFACTOR]: `internal/cron/scheduler_finish.go:330-394`。方案：改名 recordTerminalResult；rollback 抽 prevSnapshot struct。
- [ ] **R247-CR-15 — recordResultP0WithSanitised P0 后缀历史 noise（P2）** [REFACTOR]: `internal/cron/scheduler_finish.go:301-329`。方案：与 R247-CR-14 一并改名。
- [ ] **R247-CR-16 — jobSnapshot struct 字段顺序非最优（P3）** [REFACTOR]: `internal/cron/scheduler_run.go:96-121` 64-bit 平台 padding 浪费 ~8B。方案：size DESC 重排。
- [x] **R247-CR-17 — hexIDBytes=8 命名误导（P3）** [REFACTOR]: `internal/cron/job.go:202` godoc 说 "16-char hex"。方案：改名 hexIDEntropyBytes。 — 解决 2026-05-25 (F2)：改名 hexIDBytes → hexIDEntropyBytes 钉死语义到熵源侧；godoc 显式说 "熵字节数（不是字符数）" + "8 字节 → hex.EncodeToString → 16 hex 字符"。仅内部 const，无外部 caller。
- [~] **R247-CR-18 — gcWaitBudget 包级 mutable var 测试 racy（P3）** [REPEAT-3]: `internal/cron/scheduler.go:655,661` 与 R246-CR-012 同根因。方案：const + WithStopBudget(d) helper。
- [ ] **R247-CR-19 — marshalJobs atomic.Pointer test seam 通过 init() 装载（P3）** [REFACTOR]: `internal/cron/scheduler_persist.go:32-37` 与 R242-CR-5 同根因。方案：build tag testonly 或字段 DI。
- [ ] **R247-CR-20 — runs 限额 magic number 推导散落（P3）** [REFACTOR]: `internal/cron/runstore.go:170-187`。方案：集中注释块 + 推导公式。
- [x] **R247-CR-21 — emitOverlapSkipped godoc 否定式叙述（P3）** [REFACTOR]: `internal/cron/scheduler_finish.go:269-279`。方案：改正向 "Emits start→end pair with state=Skipped"。 *(已实现：scheduler_finish.go:264-279 godoc 已正向 "runs the full RunStarted→finishRun lifecycle ... emits BOTH a RunStarted event AND drives finishRun")*
- [ ] **R247-CR-22 — maxJobsHardCap=500 等 const 缺 benchmark 引用（P3）** [REFACTOR]: `internal/cron/scheduler.go:283-294`。方案：链到 cron-v2-polish.md。
- [~] **R247-CR-23 — slogPrintfLogger panic/recovered 字符串扫描负面陈述（P3）** [REPEAT-3]: `internal/cron/scheduler.go:807-815` 与 R246-CR-016 同根因。方案：抽命名常量 + godoc。
- [ ] **R247-CR-24 — executeIfNotDeletedOrPaused godoc 历史 review 引用（P3）** [REFACTOR]: `internal/cron/scheduler_run.go:38-49`。方案：去 review code 仅留行为说明。
- [ ] **R247-CR-25 — 历史 review 编号注释累计 40+ 处（P3）** [REFACTOR]: `internal/cron/scheduler_run.go,scheduler.go` 多处。方案：归档时同步删除注释或加 docs/COMMENT_CONVENTIONS.md。
- [ ] **R247-CR-26 — containsCronC0 函数名误导（P3）** [REFACTOR]: `internal/cron/store.go:241-266` 实际还查 bidi/LS/PS。方案：改名 containsCronUnsafe。
- [ ] **R247-CR-27 — Append truncate 三字段注释不对称（P3）** [REFACTOR]: `internal/cron/runstore.go:280-339`。方案：抽 shrinkOversizeRun helper。
- [x] **R247-CR-28 — spawn budget 警告 magic factor 0.5（P3）** [REFACTOR]: `internal/cron/scheduler_run.go:606-613`。方案：const spawnElapsedWarnRatio + godoc。 — 解决 2026-05-25 (F2)：抽 spawnElapsedWarnRatio = 0.5 const + godoc 解释取值依据；改用 time.Duration(float64(jobTimeout)*ratio) 算 spawnWarnBudget；slog 消息 "cron send budget exceeds job/2" 保留不动（runbook + docs/ops/pprof.md + metrics.go godoc 都按这串 grep），新增 warn_ratio 结构化字段。
- [ ] **R247-CR-29 — TriggerNow 60 行 + 3 goroutine 分支（P3）** [REPEAT-3]: `internal/cron/scheduler_jobs.go:780-833`。方案：合并单 goroutine + 内部 if。注意：trigger_now_wg_done_test.go 的 CRON4 结构契约硬性要求"3 个 go func() + 3 个 defer Done"；要落地本 TODO 必须先调整该 test 表达新契约（"恰好 1 个 go func() 且包含 defer Done"），是 BREAKING-LOCAL 跨 test+impl，不适合 hourly pick。
- [ ] **R247-CR-30 — IsExcluded godoc 与实现 cost 不一致（P3）** [REFACTOR]: `internal/cron/scheduler_session.go:40-46`。方案：godoc 标注 O(jobs × recentCap) + 推 KnownSessionIDs cache。

### 架构（剩余）

- [ ] **R247-ARCH-1 — /health 同时承担 liveness+readiness+stats（P1）** [REFACTOR]: `internal/server/health.go:189` + `cmd/naozhi/main.go:442`。方案：拆 /livez /readyz /health；startup_phase metrics 设 readiness gate。
- [ ] **R247-ARCH-2 — 三套并行错误分类碎片（P1）** [REFACTOR]: cron.ErrorClass / sysession.DaemonErrorClass / session.mapSendError 各自演化无共享词汇；NotifyDefault 决策、metrics label、IM 文案分类只能终端 site 重复。方案：抽 internal/errclass.Class enum + Classify(err) Class 统一表。
- [ ] **R247-ARCH-3 — dashboard 错误响应 http.Error vs writeJSONStatus 混用（P1）** [BREAKING-LOCAL]: `internal/server/dashboard.go` 216 处 text/plain vs 110 处 JSON envelope。方案：errResp(w,r,httpStatus,code,msg) 统一含 trace_id+retry_after。
- [ ] **R247-ARCH-4 — slog.SetDefault 全局耦合 47 处直读（P1）** [REFACTOR]: `cmd/naozhi/main.go:444-460`；测试 SetDefault swap 阻塞 t.Parallel。方案：每包 New(...) 接 *slog.Logger 派生 + slog.With("component",...)；为 trace_id propagation 做底。
- [ ] **R247-ARCH-5 — WriteFileAtomic 4 处独立 reimplement（P1）** [REFACTOR]: shim/state.go:80-119 / eventlog/persist/rotate.go:90-120 / cron/store.go:90-110 / discovery/retired_store.go:200-230，尽管 osutil/atomicfile.go 已存在。方案：全部改用 osutil.WriteFileAtomic。
- [ ] **R247-ARCH-6 — metrics 命名约定打架（P2）** [REFACTOR]: `internal/metrics/metrics.go:54-380` _total/_inflight/_ms 用同 NewInt 类型；8 套子前缀；无 Prom 注册路径。方案：metrics.New(subsystem, name, kind) 强制后缀+前缀；切 Prom 用。
- [ ] **R247-ARCH-7 — 110/305 测试无 t.Parallel root cause（P2）** [REFACTOR]: slog.Default 全局 + time.Now 全局 + 0 testdata + SetDefault swap 锁线程。方案：禁 SetDefault 改 logger DI；testfx.NewProcess(t)/NewLogger(t) 隔离；110 测试加 lint allowlist。
- [ ] **R247-ARCH-8 — 配置验证/defaults 三源分裂（P2）** [REFACTOR]: Config.Validate / cron.applyDefaults inline / main.go parseDurationOrDefault。方案：抽 internal/config/validator.Pipeline 单入口 LoadAndValidate。
- [ ] **R247-ARCH-9 — 用户文案散布 17 文件 141 处中文字面（P2）** [REFACTOR]: dispatch/commands.go 62 处 / usermsg.go 16 等。方案：抽 internal/i18n/catalog + zh.toml 中央化。
- [ ] **R247-ARCH-10 — dispatch SendCtx detached ctx 模板 5+ 处（P2）** [REFACTOR][REPEAT-2]: 与 R244-ARCH-2 / R246-ARCH-17 同根因；R243-ARCH-11 修了一处仍未集中。方案：dispatch.NotifyCtx(parent, kind, timeout) factory。
- [ ] **R247-ARCH-11 — 0 处 Clock 抽象 172 处 time.Now（P2）** [REFACTOR]: testdata 时间相关只能 sleep。方案：clock.Clock interface 注 cron/sysession/shim 三个生命周期管理者。
- [ ] **R247-ARCH-12 — 健康探针无统一 interface（P2）** [REFACTOR]: handleHealth 手动展开 RouterStats/AttachmentTracker/EventLog。方案：health.Probe interface + Stats() ProbeStats registry fanout。
- [ ] **R247-ARCH-13 — flag/env/yaml 三层无统一优先级（P2）** [REFACTOR]: 1 个 -config flag + 17 处 os.Getenv 散布。方案：config.Layered{File,Env(prefix=NAOZHI_),Flag} + Resolve；doctor 输出生效来源。
- [ ] **R247-ARCH-14 — errors.As 仅 6 处 vs 227 处 fmt.Errorf 不带 %w（P2）** [REFACTOR]: 上层只能按 error.Error() 字符串匹配回退。方案：与 R247-ARCH-2 errclass 一并 + 关键域 typed error。
- [ ] **R247-ARCH-15 — server.ctxFunc closure-as-DI 反模式（P2）** [REFACTOR]: `internal/server/server.go:718` 5+ handler 持 ctxFunc 而非 ctx。方案：*Server 显式 baseCtx 字段 + r.Context() with timeout。
- [ ] **R247-ARCH-16 — eventcore types-only 子包阻碍反向 import（P3）** [REFACTOR]: 已登记 R246-ARCH-13 但本轮新角度：cli pkg 1.4k 行 eventlog.go 阻碍 history/persist 反向引用 EventEntry。方案：抽 internal/eventcore（types only）三家共享。
- [ ] **R247-ARCH-17 — 4+ 套独立"插件式注册表"（P3）** [REFACTOR][REPEAT-2]: sysession.builtinDaemons / cli/backend.Profile / history blank import / wireup 显式。方案：抽 internal/registry.Typed[T]；ban 包级 init() 注册。
- [ ] **R247-ARCH-18 — "看 const 实际 var" 80+ 处散落 14 包（P3）** [REFACTOR][REPEAT-2]: 与 R244-ARCH-16 同方向但本轮强调具体规模。方案：internal/timeouts.Defaults() + Override(t,key,d) 自动 cleanup。
- [ ] **R247-ARCH-19 — feishu transport 中文文案 hardcode（P3）** [REFACTOR]: `internal/platform/feishu/transport_ws.go:191,201` `[语音消息下载失败...]` 落 platform 包不能集中 dispatch/i18n。方案：platform.Reply 接 dispatch.UserMsg{Class,Args}。
- [ ] **R247-ARCH-20 — 0 处 trace_id propagation（P3）** [REFACTOR]: HTTP→cli→reply 三段无法关联日志，postmortem 只能 grep timestamp。方案：ctxutil.WithTraceID 中间件 + slog.With logger 派生。
- [ ] **R247-ARCH-21 — init() 写包级状态 4+ 处混合三类（P3）** [REFACTOR][REPEAT-2]: cron persist / claudejsonl source / kirojsonl source / dashboard_auth CSP 自检。方案：plugin → registry.Register；test seam → 字段+构造参数；启动自检 → main.go step。
- [ ] **R247-ARCH-22 — 31 处 sync.WaitGroup 各自跟踪 goroutine（P3）** [REFACTOR]: 单元测试无法验证 leak。方案：引 go.uber.org/goleak（test-only）+ cron/sysession/router 主测包 leak check。
- [ ] **R247-ARCH-23 — Config struct 装 yaml 输入+解析后 cached* 双语义（P3）** [BREAKING-LOCAL]: `internal/config/config.go:39-78` 30+ 字段含 7 cached*。方案：拆 RawConfig → Resolve() (*ResolvedConfig)。

## Round 246 — 5-agent 并行 code review 第 56 轮（2026-05-24，紧随 Round 245）NEEDS-DESIGN

> 紧随 Round 245 的第 56 轮 5-reviewer 并行扫描（Go / 安全 / 性能 / 代码质量 / 架构）。基线 c13fa47。in-flight PR=0；REPEAT 提示 ARCH-1/2/3/4 REPEAT-19~21 / SEC-1/2 REPEAT-37~46 / GO-1/2/3 REPEAT-25~40 / PERF-1/2/3/4 REPEAT-34~69 已避开（本轮在新维度：transcript sanitize 死代码 / serveRaw 凭据守卫漏 / coalesce.go time.Format alloc / preflightArgs struct alignment 等）。
>
> 本轮直接修 2 项 P1-SEC 落地（无单 PR 限额规则）：[R246-SEC-1] transcript flatten 5 处加 sanitizeWireText（让 R243-SEC-5 godoc 描述与代码一致）；[R246-SEC-2] serveRaw 加 isSensitiveDownloadName 守卫（与 servePreview/serveDownload 对齐，关闭 .env/.npmrc/id_rsa 通过 raw 模式泄露的洞）。其余 90 项全登记 NEEDS-DESIGN，按 v4 分类标签 [REFACTOR]/[BREAKING-LOCAL]/[REPEAT-N]。

### Direct fixes (R246)


### NEEDS-DESIGN (R246) — 90 项登记，按域/标签

#### 安全（SEC，12 项）

- [x] **R246-SEC-3 [P2] [BREAKING-LOCAL] — `internal/server/dashboard_system.go:35-56` handleSystemDaemons 绕过 writeJSON**: 行 39-41 / 54-55 直接 w.Write([]byte("[]"))，缺 X-Content-Type-Options / Cache-Control。同文件 handleClearLabelOrigin 行 102-103 success 路径同症。建议：替换为 writeJSON / writeOK。 → handleSystemDaemons 两条返回路径都改走 writeJSON([]DaemonStatus{}/statuses)，handleClearLabelOrigin success 改走 writeOK；wire shape `{"ok":true}` → `{"status":"ok"}` 与 /api/* 其他 mutation 端点对齐；既有测试 `[]` body assertion 通过 strings.TrimSpace 兼容 writeJSON 末尾 `\n`。
- [~] **R246-SEC-4 [P2] [REPEAT-3] — `internal/cli/protocol_claude.go:77-81` --resume argv 校验失败静默丢弃**: resumeIDRe 不匹配时不报错，沉默地不传 --resume。建议：spawn 失败前 slog.Warn 至少 1 次便于审计；或返回 error。
- [ ] **R246-SEC-5 [P2] [BREAKING-LOCAL] — `internal/server/dashboard_session.go:1052-1061` resume key 仅 64-bit 熵**: var rb [8]byte 与同 codebase 其他 ≥128 bit 不一致。建议：8B → 16B 与 anonCookie/upload 对齐。
- [ ] **R246-SEC-6 [P2] [BREAKING-LOCAL] — `internal/metrics/labeled.go:220-228` clipLabelSegment 未过滤 `|`**: labelKey 用 `|` 串接，segment 自带 `|` 时撞 key。建议：strings.ReplaceAll(v, "|", "_") 或拒绝。
- [ ] **R246-SEC-7 [P2] [REPEAT-2] — `internal/server/wshub.go:1530` BroadcastCronResult 把 result/errMsg 原文广播无 sanitize**: 与 BroadcastCronRunEnded 走 SanitizeForLog 不同，本端直接走 marshalPooled。建议：排除 Result 字段（仅传 ID + URL 让前端拉详情）或在此处再过 SanitizeForLog。
- [~] **R246-SEC-8 [P2] [REPEAT-3] — `internal/server/dashboard_send.go:147` mintAnonCookie 失败回退到原始 clientIP**: rand 失败 fallback 让 owner key 形态不一致污染 ownerCounts map。建议：fallback 也 ownerKeyFromCookie(clientIP(...)) 统一形态，或 503 重试。
- [ ] **R246-SEC-9 [P3] [BREAKING-LOCAL] — `internal/server/dashboard_send.go:172` parseAttachmentFile 信任 client Content-Type 选择 size 上限**: 32MB vs 10MB 由声明决定，magic-byte 复核在 fh.Open 后。建议：magic byte 决定 size cap，或 PDF 上传走专属端点。
- [ ] **R246-SEC-10 [P3] [REPEAT-2] — `internal/server/dashboard.go:503` CSP 含 cdn.jsdelivr.net 但缺 SRI 兜底**: 若 CDN 被劫持，CSP 仍允许加载。建议：CSP 加 `require-sri-for script style`（实验性 Chrome 79+），或迁移到本地 vendored 资产。
- [ ] **R246-SEC-11 [P3] [REPEAT-2] — `internal/server/health.go:189` /health 未认证响应未限速**: Status + Uptime 可作 fingerprint。建议：未认证分支套上 unauthDashLimiter，或把 uptime 下推到 auth-only section。
- [ ] **R246-SEC-14 [P3] [REPEAT-2] — `internal/server/dashboard_session.go:983` slog Info "session label updated" key 字段未二次 sanitize**: 仅过 ValidateSessionKey 校 C0/UTF8，bidi 漏网取决于实现。建议：log 处统一 session.SanitizeLogAttr 与 dispatch.commands.go:51 一致。

#### Go 语言（GO，22 项）

- [ ] **R246-GO-1 [P1] [BREAKING-LOCAL] — `internal/cron/scheduler_jobs.go:782-832` TriggerNow.entryID 读在 unlock 之后**: s.cron.Entry(entryID) 在 s.mu.Unlock 后调，并发 UpdateJob 改 j.entryID 后 Remove 旧 ID 致正常 schedule 误判 entry gone。建议：把 cron.Entry(entryID) 提到 unlock 前；或在 executeIfNotDeletedOrPaused 重新解析最新 entryID。
- [ ] **R246-GO-3 [P1] [BREAKING-LOCAL] — `internal/cron/scheduler_run.go:367-411` finishRun 失败时 inflight 元数据延迟清理**: defer 顺序导致 CurrentRun(jobID) 仍能 Load runInflightView{Phase:Spawning} 与 cron_run_ended 并行出现。建议：finishRun 第一行 inflight.reset() 让 broadcast 与 inflight view 一致。
- [~] **R246-GO-4 [P2] [REPEAT-34] — `internal/cron/runstore.go:282-339` Append 每次 os.MkdirAll 在热路径**: 长寿 job 每次写盘多 lstat+mkdir syscall。建议：sync.Map[jobID]struct{} 记录已确保过目录，与 storeDirOnce 思路一致。
- [ ] **R246-GO-5 [P2] [REFACTOR] — `internal/cron/scheduler.go:684-777` Stop 路径 saveJobs 失败留下"假成功"语义**: marshal 失败 return 不 save，但 stopCancel/cron.Stop 已发生，调用方拿到 void Stop() 不知数据丢失。建议：Stop() 改返回 error；或至少 slog.Error 加 "persist": "FAILED_DURING_SHUTDOWN" 增加 alert key。
- [ ] **R246-GO-6 [P1] [BREAKING-LOCAL] — `internal/cron/scheduler_run.go:629-633` abortCh 阻塞读不带 timeout**: sess.InterruptViaControl 可能阻塞，cron Stop 路径上突破 stopBudget。建议：select { case abort = <-abortCh: case <-time.After(2s): abort = abortResult{} }。
- [ ] **R246-GO-8 [P2] [REPEAT-25] — `internal/cron/scheduler_jobs.go:281-318` DeleteJobByID lock-order 未文档但实际依赖**: scheduler.go EnsureStub 文档"must-not-hold-s.mu"但无 contract test 阻止新 callers 绕过。建议：debug-only TryRLock assert，或 contract test assert 调用栈。
- [ ] **R246-GO-9 [P2] [BREAKING-LOCAL] — `internal/cron/runstore.go:425-443` cacheHeadPush 静默 no-op 时数据已 fsync 上盘**: warm=false 时 no-op，下次 cacheGet 走 warmCache 重读盘；cache miss 频率高。建议：Append 末尾若 cache 不 warm 主动 LoadOrStore 空 entry + 刚 Append 的 summary 作种子。
- [ ] **R246-GO-11 [P2] [REPEAT-34] — `internal/cron/scheduler_session.go:68-108` KnownSessionIDs 文档说"30秒 cached" 但代码没缓存**: 每次调用都触发完整扫描，500 jobs × 200 runs = 100k map ops。建议：runStore.globalKnownSet atomic.Pointer 增量；或 KnownSessionIDsCached sync.Map+TTL 真做 30s 缓存。
- [x] **R246-GO-13 [P2] [BREAKING-LOCAL] — `internal/cron/scheduler.go:683-758` Stop deadline timer 复用陷阱**: 第一次 select 命中 cronDoneCtx 后第二段 select 与 deadline.C 共用同一 timer，依赖 NewTimer 复用陷阱不保证 Go 语义。建议：第二段 select 用 time.After(stopBudget - elapsed)。 — 解决 2026-05-25 (F2)：记 stopStart，第二段 select 改用 `time.After(stopBudget - time.Since(stopStart))` 单独 derive 剩余预算；clamp 1ms floor 避免 0-duration 阻塞已就绪 triggerDone。仅 scheduler.go 单文件，外部 caller 0 影响。
- [ ] **R246-GO-15 [P3] [REPEAT-25] — `internal/cron/runinflight.go:64-73` strHeap/timeHeap 每次 Store 都 alloc**: setPhase 在 fast-path Load 比较虽避免重复 store 但 phase 值实际每次都不同。建议：phase 改用 atomic.Int32 枚举（PhaseQueued..Sending），string version 留 export 时 lookup。
- [ ] **R246-GO-16 [P2] [REPEAT-37] — `internal/cron/scheduler_jobs.go:870-890` findByPrefix 在持 s.mu.Lock 下做 O(N) scan**: 500 job 全 scan + strings.HasPrefix 写锁下，并发 dashboard 列表读直接被阻塞。建议：findByPrefix 自己拿 RLock；或维护 prefix→[]ID 小 trie。
- [~] **R246-GO-17 [P3] [REPEAT-40] — `internal/cron/scheduler_run.go:486-500` ExtraArgs clip 但 AgentOpts 其他 slice/map 未 clone**: 当前没看到下游 append 但 hardening 不全。建议：cloneAgentOpts(opts) 工具函数复制所有 slice/map。
- [ ] **R246-GO-20 [P2] [BREAKING-LOCAL] — `internal/cron/runstore.go:733-819` trimJobLocked sort+remove 在持 jobLock 阻塞 Append**: 200 entries × keepCount sort + N×Remove syscall 在 jobLock 下，并发 Append 排队。建议：Remove 阶段释放 jobLock。
- [~] **R246-GO-21 [P3] [REPEAT-40] — `internal/cron/scheduler.go:387-406` applyDefaults 每次复制 cfg.SchedulerConfig**: ~280 字节复制。建议：applyDefaults pointer receiver in-place。
- [~] **R246-GO-22 [P3] [REPEAT-34] — `internal/cron/scheduler_run.go:721-747` applyJitter NewTimer/Stop 频繁**: 当前 100 timer/min 成本可忽略，未来 5000 jobs 再优化。建议：保持现状但加注释；或 sync.Pool of *time.Timer。

#### 性能（PERF，16 项）

- [ ] **R246-PERF-2 [P1] [REFACTOR] — `internal/cron/runstore.go:556-633` diskListNewestFirst 排序后顺序 readRun 触发 N×(Lstat+ReadFile)**: 冷启动 GC + 第一次 dashboard 1Hz poll 串行 2×200 syscall+unmarshal。建议：先 mtime 过滤再 readRun，并以 8 worker 并行解码；预期 lat↓ 5-10× cold cache。
- [ ] **R246-PERF-3 [P2] [REFACTOR] — `internal/cli/process_send.go:262-281` time.NewTicker 每次 Send 启动**: 每条用户消息一次，比 NewTimer+Reset 多 runtime goroutine + chan。建议：单一 deadline timer + select default 短轮询，或共享 watchdog goroutine。
- [ ] **R246-PERF-4 [P2] [REFACTOR] — `internal/server/wshub.go:643-661` 锁内 O(connections × subscriptions) per-key 计数扫描**: 500 conn × 50 subs/conn = 25K map 查找一次锁内。建议：增量 subscriberCounts map[string]int 在 sub/unsub path 维护，subscribe 锁内 O(1)；预期 lat↓ 20×。
- [ ] **R246-PERF-5 [P2] [REFACTOR] — `internal/cli/eventlog.go:1118-1130` notifySubscribers RLock + slice scan per-Append**: 4-50 events/s × N session = 25K 调用/s。建议：subscribers 切到 atomic.Pointer[[]*subscriber] CoW 替换指针，读者完全免锁；预期 CPU↓ 5-10%。
- [ ] **R246-PERF-6 [P2] [REFACTOR] — `internal/cron/runstore.go:301-339` Append 每次 os.MkdirAll syscall**: 长寿系统每条都浪费一次 stat+mkdir。建议：per-jobID sync.Once 或 dirsCreated sync.Map；预期 syscall↓ 50%。
- [ ] **R246-PERF-8 [P2] [REPEAT-2] — `internal/cron/runstore.go:606-615` slices.SortFunc 每次 List/Recent + Trim 全量**: dashboard 1Hz poll 已加 cache，但 trim 路径每次 Append 仍可能触发。建议：当 mtime 已有序时跳过 sort（probe + linear scan）；预期 CPU↓ 30%。
- [ ] **R246-PERF-9 [P2] [REFACTOR] — `internal/server/wshub.go:1463-1509` BroadcastSessionsUpdate debounce timer + clientWG.Add(1)**: 频繁 session mutate 下高频 timer 创建。建议：复用单一长寿命 timer（Reset），减少 AfterFunc 闭包分配。
- [~] **R246-PERF-10 [P2] [REFACTOR] — `internal/upstream/connector.go:331-333` marshalResult(v any) 走 reflect path 无 pool**: connector 频繁交互每次新分配 encodeState scratch。建议：getJSONEnc/putJSONEnc 同模式；预期 alloc↓ ~1 buf/call。
- [ ] **R246-PERF-11 [P3] [REFACTOR] — `internal/cron/scheduler_persist.go:46-58` marshalJobsLocked per-mutation 全量 marshal + sort**: 50 jobs × 2KB = 100KB write。建议：lazy/debounce 持久化或 WAL 后批量 snapshot；预期 lat↓ 5-10ms/mutation。
- [ ] **R246-PERF-12 [P3] [REFACTOR] — `internal/cli/eventlog.go:1118-1130` subCount.Load short-circuit 后仍 RLock**: sub > 0 时仍每次 RLock + range。建议：subCount==1 时存 atomic.Pointer 单 subscriber 直接 send；或者 subscribers atomic.Pointer CoW 让 hot path 完全免锁。
- [ ] **R246-PERF-13 [P3] [REFACTOR] — `internal/dispatch/msgqueue.go:185-208` Enqueue 队列满 copy + 零槽 + slice shrink 是 O(N)**: MaxDepth 默认 16 但每次 evict memmove 15 个 QueuedMsg。建议：环形 buffer（head/tail 索引）O(1) 入队/出队。
- [ ] **R246-PERF-14 [P3] [REFACTOR] — `internal/sysession/runner.go:166-204` exec.CommandContext 每次启动 claude -p subprocess**: AutoTitler 频繁触发时 50-100ms 启动 × N。建议：长寿命单例 stream-json claude（与 RFC §6.1 SharedCLI 决策冲突，需重新评估批量场景）。
- [ ] **R246-PERF-15 [P3] [REFACTOR] — `internal/server/dashboard_session.go:411-412` router.Version() + ListSessions() 双 RLock**: 可在 ListSessions 内一次返回 (snapshots, version) 元组避免 race window；预期语义更清晰。
- [x] **R246-PERF-16 [P3] [REPEAT-2] — `internal/cli/eventlog.go:1217-1245` Entries()/LastN() 全 ring 拷贝走 []EventEntry**: 500 slot ring 复制是 ~200KB+ 浅拷贝。建议：返回 []*EventEntry + lifetime contract，或 sync.Pool 缓存 slice 重用 backing array。 — F1 [REPEAT-2]: 同根因 R247-PERF-13 已合并修复（fde8f67：EntriesAppend/LastNAppend buffer-reuse API）。

#### 代码质量（CR，18 项）

- [ ] **R246-CR-001 [P2] [REFACTOR] — `internal/cron/scheduler_run.go:367-711` executeOpt 仍 345 行**: 即便 R245 已切多个 helper，核心 body 仍 > 三个屏幕高度。建议：抽 runSpawn/runSend/runFinish 三段 helper，主体只做编排。
- [ ] **R246-CR-002 [P2] [REFACTOR] — `internal/server/dashboard_session.go:395` handleList 324 行**: cutoff 过滤+状态计数+项目映射+summary lookup+node merge+JSON shape 选择 6 职责。建议：filterAndCount/fillProjectAndSummary/buildLocalResp/buildMultiNodeResp 四 helper。
- [ ] **R246-CR-003 [P2] [REFACTOR] — `cmd/naozhi/main.go:401-1029` main() 629 行**: flag→config→logging→wire→signals→block 全在一个函数。建议：抽 setupLogging/buildRouter/buildHTTPServer/registerSignals helper 系列。
- [ ] **R246-CR-004 [P2] [REFACTOR] — `internal/server/server.go:517` buildServer 358 行**: 每加 dashboard handler 都要在 buildServer 多塞 30 行。建议：按 handler 域 (sessions/cron/projects/system/cli) 拆 wire 函数，每个返回 sub-mux。
- [ ] **R246-CR-005 [P2] [REFACTOR] — `internal/shim/manager.go:216` StartShimWithBackend 247 行**: 串 slot 预留→exec.Command→ready scan→token decode→connect→cgroup move→handle 替换 7 步，killAndUnblock+slotReleased 双 cleanup 标志。建议：awaitReady(ctx, stdout, deadline) + reserveSlot defer 闭包。
- [ ] **R246-CR-006 [P2] [REFACTOR] — `internal/server/wshub.go:1215` resubscribeEvents 183 行**: 连接状态机+key 校验+gen 比较+ManagedSession 拉取+event backfill+broadcast，cyclomatic > 25。建议：backfill+initial-state 段独立 backfillSubscriberEvents(c, sess)。
- [ ] **R246-CR-008 [P2] [REFACTOR] — `internal/cron/runstore.go:526-633` diskListNewestFirst pagination 语义不一致**: items 按 mtime 排序但 before cutoff 比 run.StartedAt（行 627），跨进程/重启 mtime 与 StartedAt 可能不单调。建议：cutoff 比 it.mtime；或 sort key 与 pagination key 都用 mtime。
- [ ] **R246-CR-010 [P3] [REFACTOR] — `internal/cron/scheduler.go:138-142` 类型块仅声明回调类型却放在 var/const 旁**: R245 已新建 scheduler_callbacks.go。建议：把 OnRunStartedFunc/OnRunEndedFunc/OnExecuteFunc 类型也搬到 scheduler_callbacks.go。
- [ ] **R246-CR-011 [P3] [REFACTOR] — `internal/cron/runinflight.go:64-74` strHeap/timeHeap helper 命名误导**: 名字暗示"分配 heap"但实际只是命名 local + 取地址，escape 与否仍由编译器决定。建议：改名为 boxString/boxTime 或加备注，避免读者误以为强制 heap。
- [x] **R246-CR-012 [P3] [REPEAT-N] — `internal/cron/scheduler.go:655,661` `var stopBudget = 30*time.Second` 与 `gcWaitBudget` 包级 var**: 注释说改 var 是测试可调，但 mutable var 易被多测试并发改写。建议：const + WithStopBudget(d) test helper（依赖注入）。 *(已实现 via R247-CR-18 commit d1bd6a7：defaultStopBudget/defaultGCWaitBudget const + WithStopBudget(d) helper 单点维护，stop_budget_test 改用 helper)*
- [ ] **R246-CR-013 [P2] [REFACTOR] — `internal/cron/scheduler_finish.go:281-299` emitOverlapSkipped 只在 1 处调用且 18 行**: 跨包/跨文件复用并不存在。建议：合并到 executeOpt CAS 失败分支，或加 godoc 明确"future caller will reuse"。
- [ ] **R246-CR-014 [P3] [REFACTOR] — `internal/cron/scheduler_run.go:165-202` preflightArgs 字段顺序导致 padding**: 8B/120B/16B/8B/32B/16B/24B/16B 混排，64-bit 平台多 8-16 bytes。建议：按 size DESC 重排（snap → notifyTo → startedAt → 16B strings → ptrs）。
- [ ] **R246-CR-015 [P3] [REFACTOR] — `internal/cron/runstore.go:163-188` const 块混排不同语义**: User-configurable defaults 与 Hard limits 混在一组。建议：拆两组并加注释。
- [ ] **R246-CR-016 [P3] [REFACTOR] — `internal/cron/scheduler.go:801-816` slogPrintfLogger strings.Contains "panic"/"recovered" 字符串扫描脆弱**: robfig/cron v3 PrintfLogger 措辞调整就降级。建议：抽 panicMarker/recoveredMarker 命名常量便于一处改。
- [ ] **R246-CR-017 [P2] [REFACTOR] — `internal/cron/scheduler_run.go:376-390` defer + atomic 组合阅读负担**: 三步顺序约束（reset → Store(false) → metrics.Add(-1)）只有 godoc 防御，无静态检查。建议：抽 helper inflight.releaseRun(metrics.CronRunInflight)。
- [ ] **R246-CR-018 [P3] [REPEAT-N] — `internal/cron/scheduler_jobs.go:875` findByPrefix O(N) 线性扫**: maxJobsHardCap=500 下可接受，但与 1Hz dashboard 列表 RLock 抢锁。建议：保留实现但改用 RLock 之外读：在 ListAllJobsWithNextRun snapshot map keys 后 unlock 线性扫。

#### 架构（ARCH，22 项）

- [ ] **R246-ARCH-1 [P2] [REFACTOR] — `internal/cli/history.go:84` + `internal/history/source.go:43` 双重 HistorySource 接口**: 维护两份签名，加新方法立即产生方法漂移而无编译失败。建议：在 history/types.go 移出 EventEntry（或 cli 内放定义但 history 包仅 type-alias），合并为单一 interface；当前 cycle 因 cli 持有 EventEntry，可用 type HistorySource = history.Source 别名做无破坏迁移。
- [ ] **R246-ARCH-2 [P2] [REFACTOR] — `internal/cron/scheduler.go:84-136` SchedulerConfig 25 字段 + 重新构造而非 functional options**: 测试新增字段必须改三处，Location nil → time.Local 隐含逻辑不在类型层暴露。建议：functional options cron.WithJitter(d)/WithMaxJobs(n)。RouterConfig（30+ 字段）与 ServerOptions（25+ 字段）同症。
- [ ] **R246-ARCH-3 [P2] [REFACTOR] — `internal/server/wshub.go:1463` BroadcastSessionsUpdate 散布 9+ 调用点**: 多 producer 直接调 Hub 内部方法 = 业务事件流向不清晰。建议：抽 session.SessionsBus（subscribe + publish），Hub 实现 subscriber 一处；其它包只 publish。
- [ ] **R246-ARCH-5 [P2] [REFACTOR] — `internal/cron/scheduler_callbacks.go:18` + `internal/sysession/manager.go:418` + `internal/server/server.go:684` callback 注册三模式不统一**: 同抽象三套实现 + 三套加锁 + 三种 nil-fallback。建议：统一为构造时只接受可空 fn 字段；运行时切换用 eventbus.Hook[T] 包级 helper。 *(部分推进 2026-05-25 (F4)：sysession.SetCallbacks 从 sync.RWMutex + 函数字段 改为 atomic.Pointer[holder] 双字段，与 upstream/connector.go R246-ARCH-6 同形；hookMu 删除；Tick 路径 load 从带锁 → lock-free；race test 通过。cron / server 两侧仍为原状（不在 F4 文件域），全面 eventbus.Hook[T] 收口待跨域 design 评审。)*
- [ ] **R246-ARCH-6 [P2] [REFACTOR] — `internal/upstream/connector.go:122,130` SetDiscoverFunc/SetPreviewFunc 启动时 race-prone 设值**: connector.go:114 注释自承"plain 字段 (no atomic / mutex) — 并发 SetX 与 handleRequest 是 data race"，依赖 main 单线程顺序。建议：upstream.New(..., Hooks{Discover, Preview}) 构造参数，或 atomic.Pointer。
- [ ] **R246-ARCH-7 [P3] [REFACTOR] — `internal/server/server.go` HTTP 中间件链未抽象 MaxBytesReader 在 13+ handler 内手写**: dashboard_cron.go 5 处 r.Body = http.MaxBytesReader、dashboard_send.go 4 处不同上限，新加 handler 漏 limiter 不会被编译捕获。建议：middleware.Chain{authM, maxBytes(N), gzip}.Then(handler) 或表驱动。
- [x] **R246-ARCH-8 [P3] [REFACTOR] — `internal/session/testutil.go` 通过 package session 把 TestProcess + InjectSession 编入生产二进制**: 自承"this file ships in the production binary"——是反模式。建议：创建 internal/session/sessiontest 子包加 //go:build !release，或 export Router.injectForTest 仅 sessiontest 友包能调。 *(已实施：testutil.go 已 carry `//go:build !release`，本批补 TestTestUtilHasReleaseBuildTag 契约测试 pin 第一行字面量，防回归。`go build -tags release ./internal/session/...` 验证存根从 release binary 排除。子包重新组织属 follow-up，blocker 在 testutil.go godoc Migration note。)*
- [ ] **R246-ARCH-9 [P2] [REFACTOR] — `internal/cron/scheduler.go:208` Scheduler 持有 stopCtx + stopCancel 字段**: 自承"context in struct usually anti-pattern"。同症 sysession/manager.go:156。建议：包装 robfig/cron 回调到 cronEntry struct{ ctx; fn func(context.Context) }，让 ctx 显式流过。
- [ ] **R246-ARCH-10 [P3] [REFACTOR] — `internal/dispatch/passthrough_ctx.go:12,31` 用 context.Value 跨 goroutine/包传 boolean 控制位**: dispatch sendFn 已显式 6 参数，再用 context.Value 传两个 bool 是冗余 + 隐式控制流。建议：扩签名 SendOpts{Passthrough, Urgent bool}；ctx.Value 留给真正跨 framework 边界。
- [ ] **R246-ARCH-11 [P2] [REFACTOR] — 4 个 SessionRouter interface 同名方法集错位**: cron/dispatch/server/upstream consumer 4 包，upstream 已 split Lookup/Lifecycle/Mutator 但其它三家未 split，contract_test 锚点跨 4 包同步成本高。建议：先把 upstream split 提到 session/api 子包，cron/dispatch 各 embed 子集，作为单 PR 落（与 R243-ARCH-9 协同）。
- [ ] **R246-ARCH-12 [P2] [REFACTOR] — `internal/cli/eventlog.go:1141` Process.SubscribeEvents 把 <-chan struct{} 直接暴露跨包**: subscriber 必须知道 channel 关闭语义、unsub 调用顺序、close 重入。建议：包装成 sess.Subscribe(handler func()) (cancel func())，channel 关在 eventlog 内部。
- [ ] **R246-ARCH-13 [P3] [REFACTOR] — `internal/cli` ↔ `internal/history` 反向依赖循环约束**: history 想 import cli.EventEntry 才能定 Source 但 cli 不能 import history（cycle）。建议：抽 internal/eventlog/event 子包仅持 EventEntry/EventKind 类型（无行为），cli 与 history/* 都依赖它。
- [ ] **R246-ARCH-15 [P2] [REFACTOR] — `cmd/naozhi/main.go:823-855` ServerOptions 25 字段大爆炸 + main.go 1323 行**: NewWithOptions 是 god-builder，每加上层依赖就要再扩 ServerOptions。建议：抽 wireup.Build(cfg) → *Server；ServerOptions 拆 CoreDeps + Optionals + Config 三组。
- [ ] **R246-ARCH-16 [P3] [REFACTOR] — `internal/metrics/metrics.go:54-` 全局可变 expvar 计数器单一注册表**: 19 包已耦合到中央表，metrics 反向被所有上层包导入，自己却不能 import 业务包。建议：metrics 仅暴露 Counter/Gauge interface + Register(name) Counter；具体常量名定义放回各业务包。
- [ ] **R246-ARCH-17 [P2] [REFACTOR] — `internal/dispatch/dispatch.go:1086,1160,710` + cron/scheduler_notify.go:91 重复 context.WithTimeout(context.Background())**: 4 处反复出现"投 IM 不能被父 cancel"模式。建议：抽 platform.NotifyCtx(parent) 工厂，内部统一 detach + timeout 选择。
- [ ] **R246-ARCH-18 [P3] [REFACTOR] — `internal/cron/scheduler_persist.go:32-37` 用 init() 把 json.Marshal 装进 atomic.Pointer**: production 路径加 test seam 反模式，每次保存额外 atomic.Load。建议：测试 t.Cleanup 注入失败注入器到 *Scheduler；或干脆不为 1 行 path 加 seam。
- [ ] **R246-ARCH-19 [P2] [REFACTOR] — `internal/wireup/history_backends.go` 单文件单职责包名"wireup"过窄**: cron/metrics/agents wiring 仍散落在 main.go 1323 行里，wireup 仅 27 行做 history。建议：扩展 wireup/ 接管全部 init() registration（cron schedule parser/history factory/protocol backends），main.go 仅 import _ + 调构造。
- [ ] **R246-ARCH-20 [P2] [REFACTOR] — `internal/cli/eventlog.go:313` onAgentTaskDoneFn 与 subscribers []*subscriber 双订阅模型并存**: 加更多事件类型要么塞 callback 要么扩 channel，没有统一事件分发抽象。建议：eventlog.Event sum type + 单一 Subscribe(filter EventFilter) Subscription。
- [ ] **R246-ARCH-21 [P3] [REFACTOR] — `internal/cli/wrapper.go:148` detectVersionCtx(context.Background(), cliPath) 启动路径吞 ctx**: 若 claude CLI 进程 hang 在 --version，systemctl restart 必须等 5s × N backends。建议：DetectBackends 接 ctx 入参；或 lazy-detect on first Spawn。
- [ ] **R246-ARCH-22 [P3] [REFACTOR] — `internal/session/router_core.go` Router 35+ 字段 + 4 处 consumer 直接 *session.Router 赋值给 SessionRouter interface 字段**: 任何 Router 公共方法签名变化要跨 4 包对齐 contract_test 锚点。已在 R243-ARCH-9 NEEDS-DESIGN，本轮强调"已是 4 包平方依赖、必须同 PR 切换"成本。

## Round 245 — 5-agent 并行 code review 第 55 轮（2026-05-24，紧随 Round 244）NEEDS-DESIGN

> 紧随 Round 244 的第 55 轮 5-reviewer 并行扫描（Go / 安全 / 性能 / 代码质量 / 架构）。基线 4f16034。in-flight PR=0；REPEAT 提示 ARCH-3 REPEAT-21 / ARCH-2 REPEAT-18 / ARCH-1 REPEAT-19 / ARCH-4 REPEAT-17 / ARCH-10~25 REPEAT-3~10 已避开（本轮 ARCH 全在新维度：dispatch god-package、ManagedSession god-struct facet 拆分、processIface god-interface ISP 拆、Router 内置 test hooks、Workspace 多源真相、Metrics 盲区 ×3、跨模块错误传播缺口、main() 622 行）。
>
> 本轮直接修 7 项落地（cron 4：[R245-GO-1] resumeJobLocked err wrap；[R245-GO-5] CAS sessionID Store(nil) 对齐 reset；[R245-CR-2] EndedAt 删除无效 omitempty + IsZero 注释；[R245-CR-007] CreatedAt UTC 化；server/cmd/feishu 3：[R245-CR-001] serviceUser UserHomeDir 失败 fail-fast；[R245-CR-003] feishu 三个安全 slog.Warn 加 remote 字段（P1-SEC）；[R245-CR-005] NewCLIBackendsHandler godoc 加 Deprecated 标记）。SEC-3 时序问题 grep 验证已正确（applyClaudeEnvSettings 在 shim.NewManager 前），跳过。

### Direct fixes (R245)


### NEEDS-DESIGN (R245) — 65 项登记，按域/标签

#### 安全（SEC，11 项）

- [ ] **R245-SEC-1 [BREAKING-LOCAL] — `internal/cron/runstore.go:root` runStore.root 缺路径校验 + symlink 检查**: filepath.Dir(StorePath)+"/runs" 派生 root。若 StorePath 含路径穿越（../../tmp/cron_jobs.json）则 runs 目录逃出 data dir；validateWorkspace 仅校 WorkDir，未覆盖 StorePath。建议：Scheduler.Start 加 absolute + EvalSymlinks + allowedRoot 前缀校验，与 validateWorkspace 同模式。runs 目录 Lstat 校验也应在 newRunStore 内做（R245-SEC-4 同根因合并）。
- [ ] **R245-SEC-2 [REFACTOR] — `internal/server/dashboard_auth.go:344` token rotation 不立即作废现存 cookie**: cookieMAC = HMAC(cookieSecret, dashboardToken)；hot-reload 改 token 后旧 cookie 仍有效 ≤24h。建议：MAC 输入加 cookie-gen timestamp + freshness window；或在 stateDir 维护 nonce 计数 token rotation 时递增。
- [ ] **R245-SEC-5 [BREAKING-LOCAL] — `internal/platform/feishu/transport_hook.go:101-110` token-only 模式无 replay 保护**: timestamp 头空 + EncryptKey="" + token 非空时跳过 timestamp 校验，nonce 也跳。建议：任何 auth 凭据存在时强制要求 X-Lark-Request-Timestamp + Nonce + dedup。
- [ ] **R245-SEC-7 [BREAKING-LOCAL] — `internal/server/project_files.go:48-63` __public_tmp__ 多用户场景泄漏**: 任意 auth 用户可读 /tmp 下他用户 0600 文件。建议：加 server.allow_tmp_preview 配置项默认 false，或拒绝 mode 0600 owned-by-other-uid。
- [~] **R245-SEC-8 [BREAKING-LOCAL] — `internal/cron/store.go:56-58` Lstat 非 ErrNotExist 错误未拒绝**: EACCES/ELOOP 时 fall through 到 os.Open 跟随 symlink。建议：除 ErrNotExist 外所有 Lstat 错误均直接返回。
- [ ] **R245-SEC-9 [BREAKING-LOCAL] — `internal/server/dashboard_auth.go:117-121` token 为空时仍走 MAC cookie 路径**: dashboardToken="" 时 isAuthenticated 已无条件 true，但 cookieMAC 仍计算空字符串 deterministic MAC，逻辑残留可被未来回归利用。建议：token 空时跳过 cookie 流程整段。
- [ ] **R245-SEC-10 [BREAKING-LOCAL] — `internal/server/project_files.go:808-824` serveRender CSP img-src 'self' 残留**: sandbox 下 'self' 让 rendered blob 可向 dashboard origin 发图片请求。建议：img-src 改为 `data: blob:`。
- [~] **R245-SEC-11 [BREAKING-LOCAL] — `internal/sysession/runner.go:147-150` BinPath 相对名 + PATH 时序竞态**: NewRunner 抓 r.env 后若 parent PATH 被并发 os.Setenv 改动则发生分叉。建议：NewRunner 用 exec.LookPath 在 r.env 的 PATH 下解析为绝对路径并固化到 BinPath。
- [ ] **R245-SEC-13 [REPEAT-2] — dashboard_cron Prompt 全量回 SetEscapeHTML(false) 风险**: 同 R243-SEC 群。建议：静态测试断言 SetEscapeHTML(false) 仅用于 API JSON，不用于 HTML 模板。
- [~] **R245-SEC-15 [REPEAT-2] — `internal/cli/wrapper.go:158` cliPath 来自 ExpandHome 未确认 IsAbs/regular file**: 建议：filepath.IsAbs 断言 + os.Lstat mode 校验（必须 regular + executable）。

#### Go 语言（GO，6 项）

- [ ] **R245-GO-2 [REPEAT-N R228-GO-2 同根因第 2 次] — `internal/cron/scheduler.go:1607-1651` SetJobPrompt mu.Lock 无 defer Unlock + 4 个手动出口**: 未来 early return 易死锁。建议：IIFE 包裹 + defer Unlock，与 DeleteJobByID/PauseJobByID 模式对齐。
- [ ] **R245-GO-3 [REPEAT-N R228-GO-2 同根因第 3 次] — `internal/cron/scheduler.go:1732/1761/1783` DeleteJob/PauseJob/ResumeJob 同样手动 Unlock**: persistJobsLocked 若 panic 锁永不释放。建议：同 R245-GO-2，全部统一 IIFE+defer。
- [ ] **R245-GO-4 [BREAKING-LOCAL] — `internal/cron/scheduler.go:453-458` IsExcluded 每次 spawn 全建 KnownSessionIDs map**: O(jobs×recentCap) alloc 仅做一次 lookup。建议：私有 containsSessionID 在 RLock 下 O(jobs) 扫描 + runningJobs.Range 短路；KnownSessionIDs 保留为 dashboard 路径。
- [ ] **R245-GO-6 [REFACTOR] — `internal/cron/scheduler.go:256-263` SchedulerConfig.ParentCtx 长期保留导致 ctx 持续引用**: 建议：NewScheduler 取出后 drop config.ParentCtx；godoc 标 derived-only。
- [ ] **R245-GO-8 [REFACTOR] — `internal/cron/runstore.go:188-193` assertJobLockHeld TryLock 盲区**: caller 已持锁时 helper 永远跳过；现实 -race 已覆盖。建议：删除 helper 或换 atomic ownerID 模式。
- [ ] **R245-GO-9 [REPEAT-2 R243-SEC-14] — `internal/cron/scheduler.go:562-566` cronNotifyTimeout=30s 阻塞 Stop**: Stop 期间 hung webhook 阻塞 systemd TimeoutStopSec。
- [ ] **R245-GO-10 [REPEAT-N R243-GO-9 第 4 次] — `internal/osutil/loginject.go:77-88` 超长 ASCII clean 字符串仍走 strings.Map 全扫**: 建议：clean 因超长触发时 isASCIIClean fast-path 直接 truncateAtRuneBoundary。

#### 性能（PERF，18 项）

- [ ] **R245-PERF-1 [REPEAT-N R71-PERF-H1] — `internal/cli/process_shim_io.go:58,87` shimWriter 每帧 byte→string 堆拷贝**: 建议：shimClientMsg.Line 改 json.RawMessage 零拷贝。
- [ ] **R245-PERF-2 [REFACTOR R233-PERF-3 / R242-PERF-7 主条目] — `internal/cron/scheduler.go:481-521` KnownSessionIDs 1Hz 重建**: 建议：atomic.Pointer[knownIDsSnapshot] + 30s TTL；finishRun/DeleteJob 主动失效；IsExcluded spawn 路径同步收敛。
- [ ] **R245-PERF-3 [REFACTOR R233-PERF-2 / R243-PERF-4 主条目] — `internal/cron/runstore.go:353-355` cacheHeadPush O(N) memmove**: 建议：定长 ring buffer。
- [ ] **R245-PERF-4 [REFACTOR R241-PERF-3 / R242-PERF-11] — `internal/server/dashboard_cron.go:519,559` 1Hz × N tabs handleList**: HasMissedSchedule 内 cronParser.Parse regexp 50/s + 第 559 行 time.Now().In(loc) 重复调用（行 480 已有 now）。建议：Job.parsedSchedule 缓存；559 改 now.In(loc).Zone()。
- [ ] **R245-PERF-5 [REFACTOR R240-PERF-6] — `internal/cron/runstore.go:221,235` Append json.Marshal 无 buffer pool**: 建议：sync.Pool + bytes.Buffer + json.NewEncoder（仿 bridgeEncPool）。
- [ ] **R245-PERF-6 [REFACTOR R243-PERF-2] — IsExcluded spawn 路径独立重建 map**: 同 R245-PERF-2 收敛。
- [ ] **R245-PERF-7 [REFACTOR R242-PERF-3 / R243-PERF-3] — `internal/cron/scheduler.go:2280,2340` executeOpt slog.With 每次新建 logger handler chain**: 建议：缓存到 jobSnapshot 或 inflight。
- [~] **R245-PERF-8 [REPEAT-3 R242-PERF-1] — `internal/eventlog/persist/framing.go:232` ReadFramedBody 每帧 make 新 buffer**: 建议：sync.Pool 复用 frame buffer。
- [ ] **R245-PERF-9 [REFACTOR R243-PERF-11] — `internal/cron/runstore.go:592-612` readRun 双 syscall（Lstat+ReadFile）**: 建议：抽 readRunNoLstat 从 dirent info 传 mode 跳 Lstat。
- [ ] **R245-PERF-10 [REFACTOR R243-PERF-5 / R242-PERF-9] — `internal/cron/runstore.go:483-558` diskListNewestFirst pagination 绕 cache**: 建议：recentCacheEntry 存 sorted []item 内 cache 过滤。
- [ ] **R245-PERF-11 [REFACTOR R240-PERF-16 / R242-PERF-5/6] — `internal/cron/scheduler.go:1196-1218` ListAllJobsWithNextRun 三次 alloc + handleList 内 ×50 sync.Map.Load**: 建议：sync.Pool 复用 map（maps.Clear）+ 批量快照合并。
- [~] **R245-PERF-12 [REFACTOR R242-PERF-13] — `internal/eventlog/persist/persister.go:670-711` handleBatch MarshalRecord 反射路径无 pool**: 建议：参照 bridgeEncPool 加 json.Encoder pool。
- [ ] **R245-PERF-13 [REFACTOR R243-PERF-1] — `internal/cron/runstore.go:316/253` Append 内 jobLock 期间多次 time.Now()**: 建议：lock 前捕获 now。
- [ ] **R245-PERF-14 [REFACTOR R240-PERF-17] — `internal/cron/runstore.go:268-308` skipAppendTrim 无竞争快路径仍 Lock**: 建议：appendsSinceTrim 改 atomic.Int32 + Load 快路径。
- [ ] **R245-PERF-15 [REFACTOR] — `internal/server/agent_tailer.go:390-396` pollOnce 200ms tick 每次 make 新 subs slice**: 建议：sync.Pool 复用（Put 前清零 pointer）。
- [ ] **R245-PERF-16 [REFACTOR] — `internal/server/dashboard_cron.go:548-554` handleList × RecentRuns 内层 cronSummaryToView 250 struct copy/s**: 建议：合并到 R245-PERF-11 批量快照。
- [ ] **R245-PERF-17 [REFACTOR R243-PERF-13] — `internal/cron/scheduler.go:3035-3082` redactPathsInCronError slow-path 双 alloc**: 建议：sync.Pool 复用 strings.Builder。
- [ ] **R245-PERF-18 [REFACTOR] — `internal/cli/wrapper.go:482-499` shimLineReader.ReadLine 每次 Unmarshal shimMsg**: 需 -gcflags=-m 验证是否栈分配；可能伪阳性。

#### 代码质量（CR，4 项）

- [ ] **R245-CR-002 [REFACTOR] — `cmd/naozhi/service.go:92` EvalSymlinks 静默丢错**: 建议：if resolved, err := EvalSymlinks; err == nil { binary = resolved } + 注释 fallback 语义。
- [ ] **R245-CR-004 [REFACTOR] — `internal/usermsg/usermsg.go` 无单测**: ForSendError 是规范 error→user 文本映射；新 sentinel 漏 case 静默 fall-through。建议：表驱动 contract test 仿 internal/dispatch/error_mapping_contract_test.go。
- [ ] **R245-CR-006 [REFACTOR] — `cmd/naozhi/service.go:73` fs.Parse 错误丢弃**: ExitOnError 模式下 err 永不返回，但模式改后会静默；setup.go:102 同问题。建议：err 显式处理或加 //nolint:errcheck 注释。
- [~] **R245-CR-008 [REFACTOR] — `internal/session/managed.go:1342` 孤儿 TODO 引用 R239-CR-11 不在 TODO.md**: 建议：要么补登记，要么删 TODO 注释。

#### 架构（ARCH，25 项 — 全 [REFACTOR]，本轮新维度）

- [ ] **R245-ARCH-26 [REFACTOR] — `internal/dispatch/dispatch.go:81-163` god-package: 1346 行 Dispatcher 兼任调度+命令路由+replyTracker（4 goroutine 类+6 atomic）**: 建议：拆 replyTracker 到 internal/dispatch/replytracker.go 或独立包；为多渠道并行复用铺路。
- [ ] **R245-ARCH-27 [REFACTOR] — `internal/dispatch/dispatch.go:15-23` dispatch import cli+cron+platform+project+session+usermsg 6 对等包**: 建议：抽 CronAdmin interface 注入而非直依赖 *cron.Scheduler。
- [ ] **R245-ARCH-28 [REFACTOR] — `internal/session/managed.go:120-266` ManagedSession 14 atomic.Pointer + 4 mu，~280 字段宽（god-struct）**: 建议：按 facet 拆 sessionMeta/sessionLabels/sessionCost/sessionHistory，ManagedSession 组合。
- [ ] **R245-ARCH-29 [REFACTOR] — `internal/session/router_core.go:191-436` Router 50 字段含 testHookBeforeSpawnPhase3/Phase3 production-built-in test hooks**: 建议：移到 export_test.go 或 build tag _test_hooks.go。
- [ ] **R245-ARCH-30 [REFACTOR] — `internal/session/managed.go:47-114` processIface 30+ 方法（god-interface）**: 建议：ISP 拆 ProcessLifecycle/ProcessSender/ProcessQuery/ProcessHistory 四面。
- [ ] **R245-ARCH-31 [REFACTOR] — `cmd/naozhi/main.go:401-1022` main() 622 行装载 13 子系统**: 建议：抽 bootstrap.go 与 type App struct + Bootstrap 函数；main 仅 ~80 行 signal 处理。
- [ ] **R245-ARCH-32 [REFACTOR] — Workspace 状态多份真相（Router.workspace/workspaceOverrides/ManagedSession.workspace/Process.cwd 四源）**: 建议：抽 WorkspaceResolver 集中读取优先级；ManagedSession 不缓存 workspace 字段。
- [ ] **R245-ARCH-33 [REFACTOR] — `internal/dispatch/dispatch.go:820` os.ReadFile 在 dispatcher 主路径无 abstraction**: 建议：注入 ImageReader interface 到 DispatcherConfig，测试可 mock。
- [ ] **R245-ARCH-34 [REFACTOR] — dispatch 多处 time.Now/NewTimer 直调，无 Clock 抽象**: 建议：引 Clock interface（Now/NewTimer），DispatcherConfig.Clock 默认 systemClock。
- [ ] **R245-ARCH-35 [REFACTOR] — `internal/dispatch/consumer.go:34-43` SessionRouter 接口含 GetWorkspace/SetWorkspace 与 KeyResolver 重叠**: 建议：移出独立 WorkspaceStore interface。
- [ ] **R245-ARCH-36 [REFACTOR] — Metrics 盲区 #1: dispatch 4 atomic counter 未走 metrics 包**: 建议：改用 metrics.DispatchMessageTotal expvar.Int 与 SessionCreateTotal 一致。
- [ ] **R245-ARCH-37 [REFACTOR] — Metrics 盲区 #2: MessageQueue 完全无 metrics（depth/enqueue/drain/discard/interrupt fire/coalesce）**: 建议：加 naozhi_dispatch_queue_* 系列 expvar。
- [ ] **R245-ARCH-38 [REFACTOR] — `cmd/naozhi/main.go:911-952` runShutdown 仅串行 sysMgr→scheduler→router，未显式 stop Hub/nodeCache/discoveryCache/scratchPool**: 建议：拓扑排序显式 + ShutdownPhaseMs metric。
- [ ] **R245-ARCH-39 [REFACTOR] — `internal/dispatch/dispatch.go:649-871` sendAndReply 220 行单函数 7 阶段平铺**: 建议：切 acquireSession/runTurn/renderReply/postReply 四步。
- [ ] **R245-ARCH-40 [REFACTOR] — `internal/server/server.go:45-125` Server 30+ 字段含 13 handler 子结构**: 建议：type Handlers struct{} 单字段聚合；Server 仅 router/platforms/lifecycle 三组。
- [ ] **R245-ARCH-42 [REFACTOR] — `internal/cli/process.go:163` Process struct ~60 字段 + 8 atomic + 5 mu，跨 7 文件 split 同结构**: 建议：facet 拆 processCore/processStream/processTurn 三结构。
- [ ] **R245-ARCH-43 [REFACTOR] — `cmd/naozhi/main.go:577-592` default backend probe 失败 os.Exit(1)，sysession 与 default 不同时 fallback 不可达**: 建议：buildSysessionManager 显式可选第二 backend；或配置层强约束。
- [ ] **R245-ARCH-44 [REFACTOR] — `internal/dispatch/dispatch.go:411-424` BuildHandler passthrough/queue/guard 三层 fallback 嵌套 ~200 行**: 建议：抽 GateStrategy interface 三实现；NewDispatcher 据 cfg 选一。
- [ ] **R245-ARCH-45 [REFACTOR] — dispatch SendFn/TakeoverFn 闭包注入**: 建议：补 SessionFlow interface 把 GetOrCreate+SendFn+TakeoverFn 三事打包，测试 mock 单 interface 比三 closure 更稳。
- [ ] **R245-ARCH-46 [REFACTOR] — Router NewRouter 内执行 runOrphanSweep/startAttachmentTracker/runAutoChainBackfillOnce 副作用**: 建议：构造仅构造，新增 Router.Start(ctx) 显式触发 lifecycle。
- [ ] **R245-ARCH-47 [REFACTOR] — 跨模块错误传播缺口: dispatch 与 session 各自硬编码错误→中文文案 switch**: 建议：统一 internal/usermsg/registry.go 表驱动 map[error]usermsgEntry{i18nKey, severity}；dispatch 调 usermsg.For(err)。
- [ ] **R245-ARCH-49 [REFACTOR] — Observability 盲区 #3: replyTracker editLoop/todoLoop/sendAskQuestionCard 三 goroutine 无 in-flight gauge**: 建议：naozhi_replytracker_inflight_loops expvar.Int Add(+1)/Add(-1)。
- [ ] **R245-ARCH-50 [REFACTOR] — `internal/dispatch/dispatch.go:249-308` NewDispatcher 60 行配置正常化**: 建议：抽 (*DispatcherConfig).normalize()。

## Round 244 — 5-agent 并行 code review 第 54 轮（2026-05-24，紧随 Round 243）NEEDS-DESIGN

> 紧随 Round 243 的第 54 轮 5-reviewer 并行扫描（Go / 安全 / 性能 / 代码质量 / 架构）；in-flight PR=0；REPEAT 提示 ARCH-3 REPEAT-24 / ARCH-2 REPEAT-21 / ARCH-1 REPEAT-19 / ARCH-4 REPEAT-16 / ARCH-10~25 REPEAT-4~9 已避开。
>
> 本轮直接修 3 项落地（PERF：osutil SanitizeForLog 截断 O(n²)→O(1) 改用 utf8.RuneStart [R244-GO-P3] / CR：cron rollback warn 字段 orig_err→persist_err [R244-CR-P3] / TEST：osutil loginject_test 边界断言加 content equality [R244-CR-P2]）。
>
> grep-verify 命中**多处 false positive**当场归档不进 fix queue：dashboard.go 已有 X-Frame-Options/Referrer-Policy/Permissions-Policy（SEC P1 误报）/ NotifyChatID 已有 maxNotifyChatIDLen=256 + validateStringField 验证（SEC P3 误报）/ SanitizeForLog 已 rune-aware 字节截断后回退到 valid UTF-8（SEC P3 误报）/ anonCookie 已 HttpOnly+SameSite=Strict（SEC P3 误报）/ workDirUnderRoot 已在 executeOpt 二次校验防 symlink TOCTOU（scheduler.go:2380，SEC P1 误报）/ scheduler.go storeDirOnce MkdirAll 0700 + WriteFileAtomic 0600 已收紧权限（SEC P3 误报）。

### Security（剩余 — 非误报）

- [ ] **R244-SEC-P1-1 — dashboard CSP `script-src 'unsafe-inline'` 中和 XSS 防御 [BREAKING-LOCAL]**: `internal/server/dashboard.go:488` 主 dashboard CSP 含 `'unsafe-inline'`（login page 已无），任何到达 innerHTML sink 的 stored XSS 都不被 CSP 阻断。方案：login page 同款 per-script SHA-256 hash 注入或 nonce-based CSP；需逐个内联脚本 hash 化。Breaking：是（≤2 包：dashboard.go + dashboard.html 内联脚本规整）。
- [ ] **R244-SEC-P1-2 — dashboard.js 多个 innerHTML sink 数据来自 server 字段 esc() 应用不一致 [REPEAT-3]**: `internal/server/static/dashboard.js:566/1024/2178/3591/3638/5073/5532/6833/9469` 渲染 sc-prompt/title/label 等字段。validateSessionKey 仅拒 C0/C1/bidi，不拒 `<>"` 等 HTML 元字符。方案：审计每个 innerHTML 模板字符串内插字段，强制 esc() 包装；引入 lint 规则。
- [ ] **R244-SEC-P1-3 — feishu verification_token-only 模式无 HMAC 防 replay [BREAKING-LOCAL]**: `internal/platform/feishu/feishu.go:483` 当 encrypt_key 未配置时仅校验静态 token，无内容签名；攻击者获取 token 即可重放任意 payload。方案：startup doctor 在 production 强制要求 encrypt_key；或文档化威胁模型并显式 fail-fast。Breaking：是（影响只配 token 的现有部署）。
- [ ] **R244-SEC-P1-4 — slack webhook signing secret 验证路径需审计 [BREAKING-LOCAL]**: `internal/platform/slack/slack.go` 需验证 `signingSecret == ""` 路径是否硬拒；若沿用 feishu fallback 模式则同样 replay。方案：grep + 加单元测试 pin 空 secret = reject。Breaking：是（与现有部署兼容性）。
- [ ] **R244-SEC-P1-5 — serveRender CSP `unsafe-inline` + `unsafe-eval` 双开 [BREAKING-LOCAL]**: `internal/server/project_files.go:823` serveRender 路由 CSP 含 `unsafe-eval`；当前依赖 attachment Content-Disposition + opaque blob origin 隔离。`unsafe-eval` 可去除（无需 eval/Function）。方案：移除 `unsafe-eval`；保留 `unsafe-inline`（已有威胁模型 doc）；加 iframe sandbox 回归测试。Breaking：低风险局部。
- [ ] **R244-SEC-P2-1 — upload_store 文件 MIME 验证依赖客户端 Content-Type [BREAKING-LOCAL]**: 上传文件 MIME 校验需为 byte-sniff（http.DetectContentType）覆盖客户端声明。方案：上传后立即 sniff；MIME 不在 allowlist（png/jpeg/gif/webp/pdf）则 reject。Breaking：是。
- [ ] **R244-SEC-P2-2 — project_files.previewableByExt 含 `.html`/`.htm` [BREAKING-LOCAL]**: `internal/server/project_files.go:114-115` 含 .html → text/html。servePreview 已挡，但未来回归易回归。方案：从 previewableByExt 移除 html/htm；让其走 byte-sniff。Breaking：低。
- [ ] **R244-SEC-P2-3 — wsclient sendLimiter per-conn 不是 per-user [BREAKING-LOCAL]**: send rate 5/burst 1/s 应用于每连接；同一 user 开 N 连接 burst capacity 倍增。方案：按 uploadOwner 聚合 limiter。Breaking：是。
- [x] **R244-SEC-P2-4 — cdn.jsdelivr.net 资源无 SRI 整合度校验 [REPEAT-3]**: KaTeX/Mermaid 动态 script 注入未带 integrity。方案：自托管或动态注入时附 sha256 integrity。 *(已实施：合并到 R243-SEC-4 一处修复 — KaTeX/Mermaid 当前已带 integrity=sha384，CSP 扩 `require-sri-for script style font` 锁回归。)*
- [ ] **R244-SEC-P2-5 — Scheduler.SetJobPrompt 不限 prompt 字节上限 [BREAKING-LOCAL]**: dashboard 用户可写多 MB prompt 落盘。方案：加 `len(prompt)>maxCronPromptBytes` guard。Breaking：是。
- [x] **R244-SEC-P3-1 — pprof/expvar 端点对认证用户暴露 goroutine 栈包含路径 [REPEAT-3]**: 方案：`debug_mode` flag 控制注册。 *(已实施：dashboard.go:440 `if s.debugMode { s.registerPprof(); s.registerExpvar() }` gate + Server.debugMode 字段（来自 ServerOptions.DebugMode → config.yaml `server.debug_mode`，default false）。本批补 TestPprofExpvarGatedByDebugMode 源码扫描契约测试 pin gate + exactly-one-call 防 ungated 回归。R244-SEC-P3-1 [REPEAT-3]。)*
- [ ] **R244-SEC-P3-2 — `__public_tmp__` pseudo-project 默认 enabled 暴露全 /tmp [BREAKING-LOCAL]**: 方案：operator opt-in flag。Breaking：是。
- [~] **R244-SEC-P3-3 — ip_limiter unknownIPKey 共享一桶导致 trustedProxy 配错时全用户共桶 [REPEAT-3]**: 方案：trustedProxy=true 且 X-Forwarded-For 缺失时 400。
- [~] **R244-SEC-P3-4 — git remote URL 显示 / openExternal 缺 scheme allowlist [REPEAT-3]**: 方案：window.open 前显式 startsWith 校验。
- [ ] **R244-SEC-P3-5 — weixin SHA1 token 验证无 HMAC 同 feishu replay 风险 [REPEAT-3]**: 方案：文档化 production 威胁模型。
- [ ] **R244-SEC-P3-6 — esc() 用共享 `_escEl` DOM 元素并发不安全 [BREAKING-LOCAL]**: 方案：纯字符串 replace 链替代。Breaking：低。

### Go（剩余 — 非误报）

- [ ] **R244-GO-P1-1 — dashboard_cron_transcript LimitedReader.N 截断检测注释精度问题**: `internal/server/dashboard_cron_transcript.go:442` 注释说 lr.N 跟踪 logical remaining，实际跟踪 reader-level read。方案：注释明确"lr.N≤0 表示 LimitedReader 已无字节供 scanner.Read"。
- [ ] **R244-GO-P2-2 — dashboard_cron_transcript.summariseToolInput fallback 重 marshal 浪费 alloc**: line 669 unmarshal 后仅做 key 探测；fallback 分支 line 683 `json.Marshal(obj)` 可直接 `string(input)` 给 SanitizeForLog。方案：fallback 直传 `string(input)`。
- [ ] **R244-GO-P2-3 — io.LimitedReader 直接构造 bypass io.LimitReader 类型契约**: dashboard_cron_transcript.go:383 直接 &io.LimitedReader{N: maxTranscriptBytes} 类型必须保证 int64 安全。方案：显式 int64 cast。
- [ ] **R244-GO-P2-4 — TestSanitizeForLog_EnforcesMaxLen 顶部 flat 断言 vs 子测试 t.Run 风格混用**: 方案：全部改 t.Run 统一 table-driven。
- [ ] **R244-GO-P3-1 — `fmt.Errorf("%w: %w", ...)` 多 wrap 模式 godoc 缺说明**: `internal/cron/scheduler.go:3200`。方案：godoc 注一行说明 errors.Is 与 As 路径。
- [ ] **R244-GO-P3-2 — parseISO8601MS godoc 关于 RFC3339Nano 与 RFC3339 关系需引用**: dashboard_cron_transcript.go:696。方案：补 `// (Go time.Parse treats .999... fragment as optional)`。
- [ ] **R244-CR-P3-1 — static_cron_history_redesign_test magic constant 4000/2500 字节窗口**: 函数超出窗口时 silent 跳过 assertion。方案：用 strings.Index 定位 `}\n` 函数末尾代替固定窗口。

### Architecture（剩余）

- [ ] **R244-ARCH-1 — 缺统一 LifecycleManager 抽象 [REFACTOR]**: cron + sysession + 未来 cron-skill-binding/planner-auto-start/system-session 各 stopCtx + budget + leak 策略。当前 shutdown ordering 仅在 main.go 隐式编码。方案：抽 `internal/lifecycle.Component { Start; Stop; Drain }` + 显式依赖图。
- [ ] **R244-ARCH-2 — eventlog PersistSink 闭包绕过 metrics/tracing；caller 不知背压状况 [REFACTOR]**: persister.go:243-274 OnDrop fire-and-forget。方案：升 PersistSink 为 interface 暴露 Pressure() float64 / accept bool。
- [ ] **R244-ARCH-3 — 配置验证双源不一致 + 无 startup fail-fast hook [REFACTOR]**: cron applyDefaults vs sysession inline if。方案：`internal/config/validator.Validate(cfg) []ValidationIssue` 单一入口。
- [ ] **R244-ARCH-4 — wireup history-only blank import 不覆盖 cron daemons / platforms / backends 的 plug-in 注册 [REFACTOR]**: 方案：统一 `Registry[T]` 模式。
- [ ] **R244-ARCH-5 — 三套独立 keyed persistence 抽象（runs/events/jsonl/attachments）反复 reinvent atomic write/trim/cache [REFACTOR]**: 方案：抽 `internal/persistence.KeyedStore[K,V]` 模板。
- [ ] **R244-ARCH-6 — SessionRouter interface 缺 stub-removed 反向通知导致字符串 prefix coupling [BREAKING-LOCAL]**: scheduler.go:72-90 cron→session 通过 `cron:` 字符串 prefix 隐式约定。方案：CronKey/IsCronKey 移到 cron 包导出 + KeyKind enum。Breaking：是。
- [ ] **R244-ARCH-7 — sysession Stop osExit(2) vs cron budget+leak policy divergence 缺架构决策机制 [REFACTOR]**: 方案：lifecycle.LeakPolicy enum {ForceExit, BudgetThenLeak, BlockForever}。
- [ ] **R244-ARCH-8 — 三种 callback 注册风格 + cron godoc startup-only 无强制 [REFACTOR]**: 方案：抽 `internal/eventbus.Subscribe[E](handler) Unsubscribe`。
- [ ] **R244-ARCH-9 — 三套独立持久化 Run record schema 无 SchemaVersion + 无 migration 钩子 [REFACTOR]**: 方案：所有 persisted struct 加 SchemaVersion uint16 + migrate(v, raw) 钩子。
- [ ] **R244-ARCH-10 — cron 排除逻辑（KnownSessionIDs IsExcluded）未抽象到通用 sessionfilter [REFACTOR]**: 方案：`ExcluderRegistry { Register(name, fn); Lookup(sessionID) []ExcludeReason }`。
- [ ] **R244-ARCH-11 — cron SetOnExecute/RunStarted/RunEnded single-channel callback 反模式 [REFACTOR]**: 方案：`chan Event` + 多订阅者 fanout / OpenTelemetry Event。
- [ ] **R244-ARCH-12 — eventlog WriterAlive 健康协议 ad-hoc 各组件自定义 [REFACTOR]**: 方案：`internal/health.Probe` 各子系统注册 + /health 端点 fanout。
- [ ] **R244-ARCH-13 — cron 同时存在 ID-based / plat+chat-based 双套 mutator API 重复 5 阶段 [REFACTOR]**: 方案：`JobMutator interface { Apply(j *Job) error }` 单 runMutation 封装。
- [ ] **R244-ARCH-14 — executeOpt 单函数 344 行同时承担 8 个职责 [REFACTOR]**: 方案：`type runStep func(*runCtx) error` + pipeline 切分 ≤30 行/步。
- [ ] **R244-ARCH-15 — 锁层级仅 godoc 描述无运行时检测 [REFACTOR]**: 方案：`internal/lockorder.Acquire(name)` goroutine-local stack。
- [ ] **R244-ARCH-16 — 超时常量散落 var 顶部不可统一调优/查阅 [REFACTOR]**: 方案：`internal/timeouts` 统一注册表 + startup 打印 + dashboard 显示。
- [ ] **R244-ARCH-18 — sysession registry slice 字面量与 history blank import 风格不统一 [REFACTOR]**: 方案：第二个 daemon 落地前先统一 Registry pattern。
- [ ] **R244-ARCH-19 — addJobAcquiringLock → registerStubFromJob 锁外副作用模式无 helper 强制 [REFACTOR]**: 方案：`withRouterCallback(mutate, postUnlock)` 模板 + lint 阻断违反。

## Round 243 — 5-agent 并行 code review 第 53 轮（2026-05-24，紧随 Round 242）NEEDS-DESIGN

> 紧随 Round 242 的第 53 轮 5-reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描；in-flight PR=0；REPEAT 提示 ARCH-3 已 23 次 / ARCH-2 20 次 / PERF-1 25 次 / GO-1 19 次 / SEC-1 18 次，本批避开。
>
> 本批直接修 6 处见上方 commits（osutil SanitizeForLog len==maxLen boundary 测试 [R243-CR-P3-5] / cron SetJobPrompt rollback warn 携带原始 perr [R243-GO-5] / server transcript LimitedReader.N 替代 Seek 判断截断 [R243-GO-4] BREAKING-LOCAL / server transcript system event 空 message 跳 unmarshal [R243-GO-6] / server transcript parseISO8601MS 删 RFC3339 dead-fallback [R243-CR-P3-6] / server cronCreateResp 注释指明 dashboard.js consumer [R243-CR-P3-3]）。
>
> 限额：P1-SEC 无上限本轮选 0（A/B 两条均为 markdown renderer XSS [REPEAT-1]，需 [REFACTOR] DOMPurify 集成方可收口，登记不丢弃）；非安全 ≤6 选 5（GO-5/GO-6/CR-P3-3/P3-5/P3-6）；BREAKING-LOCAL ≤2 占 1（GO-4）。下列为本批新发现且不适合直接修的 NEEDS-DESIGN 条目（保留 v4 二级分类标签）。

### Go (ecc:go-reviewer 第 53 轮)

- [~] **R243-GO-2 [REPEAT-3]** `internal/cron/scheduler.go:1316-1361/1363-1396` `DeleteJobByID/PauseJobByID` IIFE 锁外仍解引用 `j *Job`，仅读 `j.ID` 当前安全但合约不显式；与 R242-GO-3 同根因第 3 次。 — F2 db17eac: hoist jobID local + LOCKING CONTRACT godoc on Delete/Pause/Resume.
- [ ] **R243-GO-7 [REPEAT-3]** `internal/cron/scheduler.go:2240-2584` `executeOpt` 单函数 344 行（与 R242-GO-1 同根因），本轮新增 `CronSendBudgetDoubledTotal` 又叠职责。
- [ ] **R243-GO-9 [REPEAT-3]** `internal/osutil/loginject.go:94-102` `clean=false` 仅因超长时仍走 `strings.Map` 全串遍历；ASCII-clean 超长可走 `s[:maxLen]` 快路径。

### 安全 (ecc:security-reviewer 第 53 轮)

- [ ] **R243-SEC-1 [REPEAT-1]** `internal/server/static/dashboard.js:11865` `cronTimelineDetailHtml` 把 `detail.result`（claude CLI 输出）直接喂给 `renderMd` 后 innerHTML，无 DOMPurify 二次过滤。`SetEscapeHTML(false)` 让 `<>&` 透传 wire；renderMd 自身的 `esc()` 不能完全防御 mermaid/KaTeX 路径。方案：renderMd 输出走 DOMPurify。
- [ ] **R243-SEC-2 [REPEAT-1]** `internal/server/static/dashboard.js:11878,11944` `lastAssistant.text` / `t.text`（JSONL transcript 文本）同上路径，`handleRunTranscript` 路径未 SanitizeForLog。
- [ ] **R243-SEC-3 [REPEAT-2]** `internal/server/dashboard.go:488` CSP `script-src 'unsafe-inline'` 让任何 innerHTML XSS 直升 RCE；与 R242-SEC-1 同根因第 2 次。
- [x] **R243-SEC-4 [REPEAT-3]** `internal/server/static/dashboard.js:7084` mermaid SRI 在场但 KaTeX/CDN 路径未启 require-sri-for；与 R242-SEC-2 同根因第 3 次。 *(已实施：dashboard CSP 在原有 `require-sri-for font` 基础上扩展为 `require-sri-for script style font` 三 token forward-compat gate；今日所有 CDN <script>/<link> 注入（mermaid/KaTeX 走 dashboard.js loadKatex/loadMermaid，均已带 integrity=sha384-...）现 spec 下 no-op，未来回归会 fail-closed。新增 dashboard_csp_test.go 子断言锚点防回归。)*
- [ ] **R243-SEC-6 [REFACTOR]** `internal/server/dashboard_cron_transcript.go:525-527` ANSI strip 仅覆盖 CSI（`\x1b[`），OSC 序列（`\x1b]8;;url\x1b\\` hyperlink）未覆盖；当前 `<pre>+esc()` 兜底但需 defence-in-depth。
- [ ] **R243-SEC-7 [REPEAT-10]** `internal/server/dashboard_cron.go:570-579` `notify_default.ChatID` 在 list response 中暴露给所有认证 dashboard 用户；多 operator 部署的 cross-tenant data leak。
- [x] **R243-SEC-8 [REPEAT-5]** `internal/cron/scheduler.go (SetJobPrompt path)` IM 路径调 `SetJobPrompt` 时未走 `validateCronPrompt`（仅 `prompt != ""`）；dashboard 路径有完整校验。 — 解决 2026-05-25：`internal/cron/limits.go:36` 已实现 `ValidatePromptStrict` 集中策略（empty / MaxPromptBytes / utf8 / C0 控制 / IsLogInjectionRune C1·bidi·LS·PS）；`internal/cron/scheduler_jobs.go:592` `Scheduler.SetJobPrompt` 入口先调 `ValidatePromptStrict(prompt)` 再检 MaxPromptBytes 上限，IM (`/cron …` → Hub.runTurn / runTurnPassthrough → SetJobPrompt) 与 dashboard wshub 双路径同 policy；返 wrapped `ErrInvalidPrompt` sentinel 让 caller `errors.Is` 区分 ErrJobNotFound / ErrPersistFailed。
- [ ] **R243-SEC-9 [REPEAT-7]** `internal/cron/scheduler.go:628-654` `workDirUnderRoot` 当 `EvalSymlinks` + `allowedRootResolved` 缓存均失败时退化到 raw string prefix 比较；TOCTOU/symlink escape。
- [ ] **R243-SEC-11 [REFACTOR]** `internal/server/static/dashboard.js:8241-8244` 自动链接正则未剥 `>`/`]`/`"` 尾标点；`escAttr` 当前兜得住，仅信息性。
- [ ] **R243-SEC-12 [REFACTOR]** `internal/server/dashboard_cron_transcript.go:46-74` 256 KB scanner buffer × 并发请求数；single-operator 可接受，多 operator 需 token bucket。
- [ ] **R243-SEC-13 [REFACTOR]** `internal/server/dashboard_auth.go:343-351` 1 day cookie 无 rotation/revocation；多 operator 隐患。
- [ ] **R243-SEC-14 [REFACTOR]** `internal/cron/scheduler.go:566-567` `cronNotifyTimeout=30s` 走 `context.Background()` 不挂 stopCtx；hung webhook 阻 Stop 30s。
- [ ] **R243-SEC-15 [REFACTOR]** `internal/server/dashboard_auth.go:388` Login form `action="/dashboard"` POST fallback（JS disabled 场景）会把 token 写到 form body；当前 JS 拦截足够，仅 defence-in-depth。

### 性能 (ecc:performance-optimizer 第 53 轮)

- [ ] **R243-PERF-1 [REFACTOR]** `internal/cron/runstore.go:316/253` `Append` 在 `jobLock` 内多次调 `time.Now()`；建议 lock 前捕获一次 `now` 传下游，与 eventlog Persister 模式对齐。
- [ ] **R243-PERF-2 [REFACTOR]** `internal/cron/scheduler.go:453-458` `IsExcluded` 每次 spawn 重建 jobs×200 KnownSessionIDs map；与 dashboard 30s 快照独立。建议 atomic.Pointer[map] + 30s TTL 缓存。
- [ ] **R243-PERF-3 [REPEAT-25]** `internal/cron/scheduler.go:2340` `executeOpt` 内 `slog.With(4 attr)` 每次 cron 执行新建 logger；与 PERF-1 ReadEvent alloc 同模式第 25 次（cron 变体）。
- [ ] **R243-PERF-4 [REFACTOR]** `internal/cron/runstore.go:353-355` `cacheHeadPush` `append` + `copy` 做 O(N) shift；改定长 ring buffer head/tail 指针 O(1) insert。
- [ ] **R243-PERF-5 [REFACTOR]** `internal/cron/runstore.go:483-558` `diskListNewestFirst` `before≠0` 时绕 cache 直 ReadDir + Lstat；可在 cache 内尾段先扫，再 fallback disk。
- [ ] **R243-PERF-6 [REPEAT-22]** `internal/cron/scheduler.go:3165-3174` `marshalJobsLocked` 每次 mutation 全量 sort + Marshal；`finishRun` 1Hz/job × 50 jobs 累积压力。
- [ ] **R243-PERF-7 [REFACTOR]** `internal/server/static/dashboard.js:12459` `cronTimelineRefreshHead` 每个 `cron_run_ended` WS 全量 sort + innerHTML 重绘；rAF 防抖。
- [ ] **R243-PERF-8 [REPEAT-19]** `internal/server/static/dashboard.js:11383-11407` `ensureCronRunningTick` 1Hz × `renderCronPanel` 全量重建 cron list；只更新 `cj-when` 文字节点。
- [ ] **R243-PERF-9 [REPEAT-10]** `internal/cron/runstore.go:657-743` `trimJobLocked` 每次 ReadDir + Sort；`appendTrimBatch` 由 10 提至 50 或 keepCount/4。
- [x] **R243-PERF-10 [REFACTOR]** `internal/eventlog/persist/persister.go:853-1004` `flush` stride<=1 路径 `kept` 与 `pendingIdx` 共享底层 array；`AppendBatch` 必须同步消费的不变量需文档化或防御性 copy。 — F1: idx.go::AppendBatch 加 "Slice ownership contract" godoc 显式声明同步消费 + 禁 retain；persister.go::flush stride<=1 aliasing 注释扩展引用契约 + future-change 防御要求。
- [ ] **R243-PERF-11 [REFACTOR]** `internal/cron/runstore.go:592-612` `readRun` 双 syscall（Lstat + ReadFile），`diskListNewestFirst` 已通过 `e.Info()` 拿过 stat；提供 `readRunNoLstat` 快路径。
- [ ] **R243-PERF-12 [REPEAT-13]** `internal/server/static/dashboard.js:11694-11735` `cronTimelineHtml` 每次全 200 行重渲；加 identity check + data-run-id key diff。
- [~] **R243-PERF-13 [REPEAT-8]** `internal/cron/scheduler.go:3035-3082` `redactPathsInCronError` slow-path Builder + SanitizeForLog 双 alloc；sync.Pool 复用 Builder。 — F2 4a3ff2c: add redactBuilderPool with 8 KiB retention cap.
- [ ] **R243-PERF-14 [REFACTOR]** `internal/cron/scheduler.go:3120-3141` `findByPrefix` O(N) 持 `s.mu.Lock()`；`maxJobsHardCap=500` 时累积；前缀索引 map 或注释说明。

### 代码质量 (ecc:code-reviewer 第 53 轮)

- [ ] **R243-CR-P2-4 [REFACTOR]** `internal/server/dashboard_cron_transcript.go:121` `transcriptTurn.Input json.RawMessage` 与 `omitempty` 在 `null` 字面量下不会 omit；要么 `if len > 0 && string != "null"`，要么 godoc 说明 `null` 可能出现在 wire。
- [ ] **R243-CR-P3-4 [REFACTOR]** `internal/server/dashboard_cron_transcript.go:580-592` `flattenJSONLEvent` assistant text prepend + re-number 用 `append([]transcriptTurn{...}, out...)` 模式可读性差；用 index-0 insert + 单次 re-number。

### 架构 (ecc:architect 第 53 轮)

- [ ] **R243-ARCH-3 [BREAKING-LOCAL][REPEAT-24]** `internal/cron/scheduler.go:200 (s.mu)` 单 mutex 全局序列化所有 mutation 路径；改 per-job sharding `sync.Map[string]*jobShard`。
- [ ] **R243-ARCH-5 [REFACTOR][REPEAT-15]** `internal/session/managed.go:47-114` processIface 35+ 方法 god-interface；拆 ProcessLifecycle / EventSource / ProcessSender 三 facet。
- [ ] **R243-ARCH-6 [REFACTOR][REPEAT-12]** `internal/cron/scheduler.go:2649-2742` finishRun 三写隐式 Saga（cron_jobs.json + runStore.Append + emitRunEnded）；引入轻量 Outbox 单 goroutine 串行化。
- [ ] **R243-ARCH-7 [REFACTOR][REPEAT-11]** `internal/server/server.go + wshub.go + dispatch.go` watchdogNoOutputKills/TotalKills 三处共享指针 + onChange/onKeyRetired/onSessionRetired atomic.Pointer holder 手写散布；抽 `metrics/observability` 子包统一 DI。
- [ ] **R243-ARCH-8 [BREAKING-LOCAL][REPEAT-10]** `cron Stop / router Shutdown / wshub Shutdown` 三组超时各自常量、main.go 顺序耦合；引入 `lifecycle.Manager` topo-sort + 共享 budget。
- [~] **R243-ARCH-9 [REFACTOR][REPEAT-9]** `internal/{upstream,dispatch,cron,server}/consumer.go` 4 个 consumer-side SessionRouter 接口（9/8/3/14 方法）并存；抽 `internal/session/api/` 细粒度接口子包。 — upstream/consumer.go 加 NEEDS-DESIGN godoc + 拆 SessionLookup/SessionLifecycle/SessionMutator 三窄接口预备未来抽 internal/session/api/；4 包统一仍待跨包评审。
- [~] **R243-ARCH-12 [REFACTOR][REPEAT-5]** `cli.EventLog ring + persist.Persister spool + naozhilog.Source replay` 三层 eventlog 重影 + 4 backend 责任不清；抽 `EventStore interface{Append/Read/Subscribe}` + 中央 registry。 — internal/session/eventlog_bridge.go 顶部加 NEEDS-DESIGN 包级 godoc 描述 EventStore interface + 中央 registry 计划 + 各 backend 已积累的 perf 热路径迁移成本；跨包抽 api/ 仍待评审。
- [ ] **R243-ARCH-13 [REFACTOR][REPEAT-6]** `internal/cron/scheduler.go` metrics.CronRun*Total 11 处直调 + slog 33 处散布；用 internal/metrics/labeled + slog.With helper。
- [ ] **R243-ARCH-14 [BREAKING-LOCAL][REPEAT-4]** `sysession.Config / SchedulerConfig / RouterConfig` 无 schemaVersion；HotReload 无路径；引入 `config/v1/` migration 入口。
- [ ] **R243-ARCH-15 [REFACTOR][REPEAT-3]** wshub broadcast / agentTailer / scratch/cron-run-ended 三条独立 fan-out；抽 `broadcaster` topic 包，6 topic 集中订阅。
- [ ] **R243-ARCH-16 [REFACTOR][REPEAT-4]** scheduler.platforms / agents / agentCommands 等 6+ 处运行时不可变 map 仅靠 godoc；用 atomic.Pointer[map] swap-on-write 编译期固化。
- [ ] **R243-ARCH-18 [REFACTOR][REPEAT-5]** `cron recordResultP0WithSanitised + dispatch redact` 错误 sanitize/redact 散在 5+ 包；提到 errors 子包 SafeErr struct。
- [ ] **R243-ARCH-19 [REFACTOR][REPEAT-3]** cron / session / server 锁层级仅靠 godoc 维持；dev-only 引入 lockorder build tag CI 校验。
- [ ] **R243-ARCH-20 [REFACTOR][REPEAT-3]** 整 codebase 缺集成/E2E 测试（rg `^func TestFull|TestIntegration|TestE2E` 0 命中）；增 `tests/integration/` + testcontainers smoke。
- [ ] **R243-ARCH-21 [REFACTOR][REPEAT-3]** `cli.Process` 12 process_*.go 拆得过细，process_event_format Deprecated 等 TODO 长期挂；真正 split eventbus / linker 子包。
- [ ] **R243-ARCH-23 [REFACTOR][REPEAT-3]** `executeOpt` 345 行 + 嵌套 7 层；改 state machine `runStateMachine.Run`。
- [ ] **R243-ARCH-24 [REFACTOR][REPEAT-3]** WS / IM JSON struct 散在各包（sessionsUpdateMsg pre-marshal at server / RunStarted/Ended at cron / scratch at server）；抽 `internal/wireproto` 单一来源。
- [ ] **R243-ARCH-25 [REFACTOR]** （本轮新结构发现）`executeOpt` / `dispatch.BuildHandler` / `upstream.connector.Run` 三个 hot-path 嵌入 lifecycle/policy 决策；抽 RetryPolicy/OverlapPolicy/NotifyPolicy 接口便于 zero-downtime restart RFC 替换。

## Round 242 — 5-agent 并行 code review 第 52 轮（2026-05-24，紧随 Round 241）NEEDS-DESIGN

> 紧随 Round 241 的第 52 轮 5-reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描；in-flight PR=0；REPEAT 提示 ARCH-3 已 22 次 / ARCH-2 19 次 / ARCH-1 17 次，本批避开。
>
> 本批直接修 7 处见上方 commits（cli thumbnail goroutine recover panic [R242-GO-2] BREAKING-LOCAL / cron SetJobPrompt rollback error 不再 swallow [R242-CR-9] / server transcript Seek 失败标 truncated [R242-CR-1] BREAKING-LOCAL / server 删 dead `var _ = strconv.Itoa` 转义口 [R242-CR-2] REFACTOR / server cronCreateResp typed struct [R242-CR-10] REFACTOR / server transcriptTurn.Input → json.RawMessage [R242-CR-4] REFACTOR / server system event Unmarshal 加 Debug log 不再 swallow [R242-CR-15] REFACTOR）。
>
> 限额：P1-SEC 无上限本轮选 0（CSP/CDN/innerHTML 三项均需 [REFACTOR] DOMPurify + nonce 改造，登记不丢弃）；非安全 ≤6 选 5（CR-1/2/4/9/15）；BREAKING-LOCAL ≤2 占 2（GO-2 + CR-1）。下列为本批新发现且不适合直接修的 NEEDS-DESIGN 条目（保留 v4 二级分类标签）。

### Go (ecc:go-reviewer 第 52 轮)

- [ ] **R242-GO-1 [REFACTOR]** `internal/cron/scheduler.go:2180` `executeOpt` 函数 344 行混合了 CAS 门控/jitter/preflight/spawn/watchdog/notify/finishRun 七职责；任何新 error branch 需在 344 行上下文里定位正确的 stubRefresh / cancel 顺序。建议拆 `executeSetup` + `executeRun` + `executeResult` 三段。
- [ ] **R242-GO-3 [REFACTOR]** `internal/cron/scheduler.go:1283/1322/1354` `DeleteJobByID/PauseJobByID/ResumeJobByID` 三个 IIFE 模式让 `j *Job` 指针在锁外被使用，注释声称"j 为锁内拷贝"误导；`UpdateJob`（line 1529）正确做了 `*j` 值拷贝。建议三处统一 `result := *j` 在 IIFE 内值拷贝。
- [ ] **R242-GO-4 [REFACTOR]** `internal/dispatch/dispatch.go:639` `sendAndReply` 222 行多分支重复处理 shutdown-cancel ctx 替换；提取 `resolveReplyCtx(ctx) context.Context` helper。
- [ ] **R242-GO-5 [REFACTOR]** `internal/server/wshub.go:621` `handleSubscribe` 持 `h.mu.Lock()` 期间 O(N) 遍历 clients 计算 per-key 订阅数；改增量计数器避免在 Lock 内全量扫描。
- [~] **R242-GO-6 [REFACTOR]** `internal/sysession/manager.go:254` `Start.startOnce.Do` 内 `m.started.Store(true)` 顺序约束仅活在注释；用 `atomic.Pointer[context.CancelFunc]` 让 cancel 与 started 原子绑定。
- [ ] **R242-GO-7 [REFACTOR]** `internal/cron/scheduler.go:2411` watchdog 在 DeadlineExceeded 分支未检查 `abort.outcome` != succeeded；补 `slog.Warn`（与 Canceled 分支对齐）。
- [~] **R242-GO-8 [REPEAT-3]** `internal/cron/runstore.go:335` `cacheHeadPush` O(N=200) shift；与 R233-PERF-2 / R235-PERF-3 同根因，第 3 次。
- [ ] **R242-GO-9 [REFACTOR]** `internal/cron/scheduler.go:3062` `findByPrefix` 持 `s.mu.Lock()` 期间全量遍历 jobs map；加 `idPrefixIndex map[string]string` 缩短持锁时间。
- [ ] **R242-GO-10 [REFACTOR]** `internal/server/wshub.go:198` `Hub` 直接持 `*dispatch.MessageQueue` 具体类型而其他依赖走接口；抽 `MessageEnqueuer` 接口对齐风格。
- [x] **R242-GO-11 [BREAKING-LOCAL]** `internal/eventlog/persist/persister.go:255` DevMode panic 路径不可见但靠注释保证；改 `slog.Error + return` 或 `os.Exit(1)`。 — 已实现：commit 8f6e73b 改为 `slog.Error + replayLeakCnt + Observer.OnReplayLeak + return`，TestPersister_DevMode_ReplayLeakObserved pin 行为；persister.go:255 注释 "(closed)" 已就位。
- [~] **R242-GO-12 [REPEAT-5]** `internal/session/managed.go:47` processIface 30+ 方法 god-interface；与 R215/R219/R224/R230C-ARCH-4 同根因。
- [ ] **R242-GO-13 [REFACTOR]** `internal/cron/scheduler.go:2067` `freshContextPreflightP0` 错误分支 `deliverNotice` 同步调用拖延 finishRun；改 async goroutine。
- [ ] **R242-GO-14 [REFACTOR]** `internal/cron/scheduler.go:2919` `deliverNotice` 同步调用 + 独立 ctx 但被 cron tick goroutine drain 隐式约束；文档化关系。
- [ ] **R242-GO-15 [BREAKING-LOCAL]** `internal/transcribe/transcribe.go:107` `streamFromBuffer` sender break 后 fall-through 到 Writer.Close 是意图设计但无注释；加 `// break → Writer.Close handles cleanup`。
- [~] **R242-GO-16 [BREAKING-LOCAL]** `internal/sysession/manager.go:338` NewManager 直接赋 `m.onRunStarted` 不走 SetCallbacks；统一通过 SetCallbacks 写路径。
- [~] **R242-GO-17 [BREAKING-LOCAL]** `internal/cron/runstore.go:586` `readRun` Lstat-as-symlink-defense 注释不明确；改 "Lstat intentionally used (not Stat)"。
- [ ] **R242-GO-18 [BREAKING-LOCAL]** `internal/dispatch/dispatch.go:100` `Dispatcher.agents/agentCommands` immutable-after-construction 缺注释；与 `Scheduler.agents` 文档化对齐。
- [ ] **R242-GO-20 [REFACTOR]** `internal/cron/scheduler.go:478` `KnownSessionIDs` 在 `s.mu.RUnlock` 后用 jobIDs slice 遍历 runStore；race window 与 DeleteJobByID 并发存在但 acceptable，需注释说明。

### 安全 (ecc:security-reviewer 第 52 轮)

- [ ] **R242-SEC-1 [BREAKING-LOCAL]** `internal/server/dashboard.go:488` CSP `'unsafe-inline'` in `script-src` 使 XSS 防护退化；迁向 `'strict-dynamic'` + per-request nonce。
- [ ] **R242-SEC-2 [BREAKING-LOCAL]** `internal/server/dashboard.go:488` `https://cdn.jsdelivr.net` 在 `script-src` 无 strict-dynamic 范围隔离；配 `require-sri-for script` 或 nonce。
- [ ] **R242-SEC-3 [BREAKING-LOCAL]** `internal/server/static/dashboard.js:566/1024/1584/3986` 多处 `innerHTML` 拼装 server-data；`renderMd` 后无 DOMPurify pass，CSP `unsafe-inline` 兜底失效；加 DOMPurify 最终 pass 或 nonce CSP。
- [ ] **R242-SEC-5 [REFACTOR]** `internal/server/dashboard_auth.go:350` 1 day cookie 无 server-side revocation；加 per-session random nonce 存服务端，删 nonce 即吊销。
- [ ] **R242-SEC-6 [BREAKING-LOCAL]** `internal/server/project_files.go:47-63` `__public_tmp__` 让认证用户读 `/tmp` 任意文件；多租户场景需 explicit `allow_tmp_browse` config flag。
- [ ] **R242-SEC-7 [REFACTOR]** `internal/server/dashboard_memory.go:183-186` `tryRead` prefix 检查与 `EvalSymlinks` 用不同 base，潜在 race；统一用构造期已 EvalSymlinks 的 `h.projectsDir`。
- [ ] **R242-SEC-8 [REFACTOR]** `internal/server/dashboard_cron.go:445-453` `runsLimiter` 用默认 `MaxKeys=1000` 未显式设置；DDoS 时 LRU 驱逐让被 rate-limit IP 重新拿 token。
- [ ] **R242-SEC-10 [BREAKING-LOCAL]** `internal/cron/scheduler.go:2329` `filepath.Clean(snap.workDir)` 不解 symlink；用 `workDirReachable` 返回的 EvalSymlinks 路径作 `opts.Workspace`。
- [~] **R242-SEC-11 [REFACTOR]** `internal/server/dashboard_cron.go:656-661` `notify=true` 部分 field 检查用 `&&`，单边设置漏过 `validateNotifyTarget`；改 `||` 或前置完整校验。
- [ ] **R242-SEC-12 [REFACTOR]** `internal/server/dashboard_cron_transcript.go:393-426` shared-JSONL fresh=false 用秒级 ts 边界过滤，相邻 run 可能渗透；加 per-run UUID。
- [ ] **R242-SEC-13 [REFACTOR]** `internal/server/dashboard_cron_transcript.go:642` `summariseToolInput` Unmarshal `map[string]any` 后 Marshal，可能放大；加 depth/size cap。
- [~] **R242-SEC-14 [REFACTOR]** `internal/server/dashboard_cron_transcript.go:254-261` `IsLogInjectionRune` 不查 C0 控制字节；旧持久化 WorkDir 含 tab 会绕过。
- [~] **R242-SEC-15 [REFACTOR]** `internal/server/project_files.go:680` 双 EvalSymlinks 错误路径返回 404 静默；区分 `fs.ErrNotExist` vs IO 错误并 Warn。

### 性能 (ecc:performance-optimizer 第 52 轮)

- [~] **R242-PERF-1 [REPEAT-3]** `internal/eventlog/persist/framing.go:232` `ReadFramedBody` 每帧 `make([]byte, n+1)` alloc；recovery 启动几千帧均 alloc；用 sync.Pool。同 R218-PERF-10 同文件，第 3 次。
- [ ] **R242-PERF-2 [REFACTOR]** `internal/cron/scheduler.go:3209` `applyJitter` 每 tick 重 `cronParser.Parse(schedule)`；从 jobSnapshot 缓存 period。
- [ ] **R242-PERF-3 [REFACTOR]** `internal/cron/scheduler.go:2280` `slog.With(...)` 每 executeOpt 调用都新建 logger handler chain；移到 jobSnapshot/inflight。
- [ ] **R242-PERF-4 [REFACTOR]** `internal/cron/scheduler.go:508` `handleList` 对每个 non-paused job `cron.HasMissedSchedule` 内部 `cronParser.Parse` 50 parse/s；缓存 pre-parsed period。
- [ ] **R242-PERF-5 [REFACTOR]** `internal/cron/scheduler.go:478-543` `handleList` 每 job × `RecentRuns(5)` 50 次 sync.Map.Load + entry.mu.Lock；批量 ListAllJobsWithRecent。
- [ ] **R242-PERF-6 [REFACTOR]** `internal/cron/scheduler.go:484` `handleList` 每 job × `CurrentRun` 50 次 sync.Map.Load；与 PERF-5 折叠批量快照。
- [ ] **R242-PERF-7 [REPEAT-2]** `internal/server/dashboard_session.go:1390` `KnownSessionIDs` 每 1Hz tab 全量重建 jobs×200 map；30s TTL cache。同 review_perf_2026_04_20.md handleList Stat 缓存项。
- [ ] **R242-PERF-9 [REFACTOR]** `internal/cron/runstore.go:477-553` `diskListNewestFirst` before-cutoff pagination 无 cache；缓存 sorted items slice 在 recentCacheEntry。
- [ ] **R242-PERF-10 [REFACTOR]** `internal/cron/runstore.go:652-736` `trimJobLocked` 不利用 cache 快路径；warm cache 时直接根据 cache 长度判断。
- [ ] **R242-PERF-11 [REFACTOR]** `internal/cron/scheduler.go:547-549` `handleList` 重复 `time.Now().In(loc)`；用已捕获的 `now`。
- [ ] **R242-PERF-12 [REFACTOR]** `internal/cron/scheduler.go:3104-3115` `marshalJobsLocked` 每次 mutation O(N log N) sort；维护 pre-sorted ID slice 增量更新。
- [~] **R242-PERF-13 [REPEAT-2]** `internal/eventlog/persist/persister.go:670-711` `handleBatch` MarshalRecord 标准 json.Marshal 反射；镜像 bridgeEncPool 池化。同 R215-PERF-P1-1。
- [ ] **R242-PERF-14 [REFACTOR]** `internal/cron/scheduler.go:2411` `WithTimeout(Background, jobTimeout)` timer heap 压力；测试 10ms timeout 高频时明显。

### 代码质量 (ecc:code-reviewer 第 52 轮)

- [~] **R242-CR-3 [REFACTOR]** `internal/server/dashboard_cron.go:456-571` `GET /api/cron`（1 Hz poll）无 rate limiter 而 runs 有；DOS 风险；与 runsLimiter 同模式应用。
- [ ] **R242-CR-5 [REFACTOR]** `internal/cron/scheduler.go:3091-3096` `var marshalJobs atomic.Pointer` package-level mutable via init() + 测试 mutate；改 Scheduler 构造期 DI 字段。
- [ ] **R242-CR-7 [REFACTOR]** `internal/cron/runstore.go:192` `assertJobLockHeld` 用 `panic` + `TryLock` 在生产路径加锁竞争 + crash 风险；移到 `_test.go` 或 build tag。
- [ ] **R242-CR-8 [REFACTOR]** `internal/server/dashboard_cron_transcript.go:639-664` `summariseToolInput` `map[string]any` decode-and-hunt 反射成本；用目标 struct 或 byte-scan。
- [ ] **R242-CR-11 [BREAKING-LOCAL]** `internal/cron/runstore.go:277-323` `skipAppendTrim` panic 路径会跨过 `entry.mu` 获取，defer 不执行 → 锁泄漏；assertJobLockHeld 改 bool 返回。
- [~] **R242-CR-12 [REFACTOR]** `internal/cron/scheduler.go:206-212` 注释引用 "(line ~1864)" 但实际 2265/2296——comment rot；删除显式行号引用。
- [~] **R242-CR-13 [REFACTOR]** `internal/server/dashboard_cron_transcript.go` `flattenJSONLEvent` 120 行 4-5 层嵌套；提取 `flattenUserEvent/flattenAssistantEvent/flattenSystemEvent`。
- [ ] **R242-CR-14 [REFACTOR]** `internal/cron/job.go:206-215` `generateHexID` panic on crypto/rand 失败，会从 AddJob 调用栈炸到顶层；改返回 error 传播。

### 架构 (ecc:architect 第 52 轮)

- [ ] **R242-ARCH-1 [REFACTOR]** `internal/cron/scheduler.go:2287-2426` send/spawn budget 双倍 wall-clock + sendCtx 脱 stopCtx 链；引入 `sendDeadline := startedAt.Add(2*jobTimeout)` 显式封顶 + `context.AfterFunc(stopCtx, sendCancel)`。
- [~] **R242-ARCH-2 [BREAKING-LOCAL]** `internal/session/router_core.go:80` + `internal/cron/scheduler.go:580` exempt 池一桶共享 cron stub + planner + sysession，maxExemptSessions=20 易耗尽；三个子配额硬隔离或对 cron stub 走 LRU 覆盖。
- [ ] **R242-ARCH-3 [REFACTOR][REPEAT-2]** `internal/cli/wrapper.go:43` `Wrapper.ShimManager` 公开可变字段 + Protocol/Transport 耦合；引入 `cli.Transport` interface，ShimManager 改 unexported 构造期注入。
- [ ] **R242-ARCH-4 [REFACTOR]** `internal/session/managed.go:47-114` `processIface` 14 方法 god-interface；分裂为 ProcessLifecycle / EventSource / ProcessSender / Introspection 四个 facet。
- [ ] **R242-ARCH-5 [REFACTOR]** `internal/cron/scheduler.go:264-368` 三个独立 SetOn* + emitRun* 副作用；抽 `SchedulerListener` 单接口 + finishRun 集中终态分发。
- [ ] **R242-ARCH-6 [REFACTOR][REPEAT-2]** `internal/session/router_core.go:193-438` Router 38+ 字段 + 6 atomic.Pointer + 3 锁；进一步拆 `routerStore` + `routerLifecycle` + `routerExclusion`。
- [ ] **R242-ARCH-7 [REFACTOR]** `internal/cron/scheduler.go:97` cron 反向依赖 `platform` map；抽 `NotifySender` interface 由 cmd/naozhi 注入。
- [ ] **R242-ARCH-9 [REFACTOR]** `internal/cli/process.go` + `wrapper.go` shim 协议常量散布 4-5 处；抽 `cli/protocol.go::ProtocolLimits` struct。
- [ ] **R242-ARCH-10 [REFACTOR]** `internal/cron/scheduler.go:2511-2516` finishRun emitRunEnded 在 saveMarshaledSeq 前发；订阅者 fetch /api/cron 仍读旧 LastResult。事件 payload 直接带 result。
- [ ] **R242-ARCH-11 [REFACTOR]** `internal/session/router_lifecycle.go:721` SetPersistSink + InjectHistory + attachHistorySource 顺序仅靠 sinkReady 兜底；合并为 `Router.bindNewSession(*ManagedSession)`。
- [ ] **R242-ARCH-12 [REFACTOR]** `internal/cron/runstore.go:46-70` 锁层级 `s.mu > jobLock > entry.mu` 仅注释；引入 `lockorder.Track` debug-build 注解。
- [ ] **R242-ARCH-13 [REFACTOR]** `internal/server/wshub.go:111` cronHubOps + cronStubChecker + cronSessionLister 三个微接口职责重叠；整合 `server.CronView` 单接口。
- [ ] **R242-ARCH-14 [REFACTOR]** `internal/cron/scheduler.go:2125-2163` `deadlineInterrupter` 单方法接口直接依赖 `session.InterruptOutcome`；提到 internal/types 共享或 cron 镜像枚举。
- [ ] **R242-ARCH-15 [REFACTOR]** `internal/cron/scheduler.go:280-294` `runningJobs sync.Map` 永不清理；DeleteJob 调 LoadAndDelete 或 freshness epoch。
- [ ] **R242-ARCH-16 [REFACTOR]** `internal/session/router_core.go:413` excluders atomic.Pointer 启动期 nil pool fallback；cmd 装配前 SetPendingExcluders(true) blocking。
- [x] **R242-ARCH-18 [REFACTOR]** `internal/session/auto_chain_router.go:73-89` PickWorkspaceChain Phase 2/3 双 build excluder 不一致；Phase 3 复用 Phase 2 inner。 *(已实施：spawn 路径之前已抽 `snapshotCombinedExcluderLocked` 收口；本批补 backfill 路径 — 拆出 `combinedExcluderHeld`（caller-must-hold-r.mu 变体）共享 slice layout，`snapshotCombinedExcluderLocked` 变成 lock shell + 调 held，`runAutoChainBackfillOnce` Phase 3 从内联 `routerExcluder + extras` 改为 `r.combinedExcluderHeld()`；spawn 与 backfill 现共享单一 source of truth，未来 excluder 来源加在 held 内全路径自动覆盖。auto-chain 全部既有测试通过（TestRunAutoChainBackfillOnce_* / TestMaybeAttachAutoChainOnSpawn_*）。)*
- [ ] **R242-ARCH-19 [REFACTOR]** `internal/cron/runstore.go:22-32` runs/ vs cron_jobs.json 物理分离无 atomic transaction；DeleteJob 先 await persistJobsLocked 再删 runs/。
- [ ] **R242-ARCH-20 [REFACTOR]** `internal/cli/eventlog.go:469-478` PersistSink 双 atomic store sinkReady 顺序无 healthcheck；加 `replayDropTotal atomic.Int64` 在 /health 端点。
- [ ] **R242-ARCH-21 [REFACTOR]** `cmd/naozhi/main.go:919-937` 关闭顺序 sysMgr → scheduler → router 仅注释；抽 `lifecycle.Coordinator` 显式依赖图。
- [ ] **R242-ARCH-22 [REFACTOR]** `internal/cron/scheduler.go:2270-2276` emitRunStarted 在 GetOrCreate 之前发，SessionID="" 让 KnownSessionIDs 漏；推迟到 setSessionID 后。
- [ ] **R242-ARCH-23 [REFACTOR]** `internal/cron/scheduler.go:450` cron.IsExcluded 每次新建 jobs×200 map 在 spawn 路径；暴露 `LookupKnownSessionID(id) bool` 直查 set。
- [~] **R242-ARCH-24 [REFACTOR]** `internal/sysession/router.go:25-61` `EventEntriesForKey` 返回 `[]cli.EventEntry` 强依赖 cli pkg；定义本地 SystemEventEntry 镜像。 — godoc NEEDS-DESIGN 锚点已就位（router.go:61-71）：方向定义，但 deferred to R243-ARCH-12 (EventStore interface unification) 落地后再做，避免 mirror 类型短期内重写两次让 AutoTitler 测试 churn。
- [x] **R242-ARCH-25 [REFACTOR]** `internal/session/router_lifecycle.go:135-199` ResetChat shutdownCond.Broadcast 一处持锁一处释锁分裂；统一持锁广播。已加 godoc 锁定 not-mergeable：Close()必须在锁外（防 shim teardown pin Router），但 Broadcast 必须在 Close 之后（否则 IsRunning 谓词未翻转 → missed wakeup）。两段持锁是必需而非疏漏，与 evictOldest 同型。
- [ ] **R242-ARCH-26 [REFACTOR]** `internal/cron/scheduler.go:78-89` `RegisterCronStubWithChain chainIDs []string` 调用方一律传单元素；接口收敛或重命名。
- [ ] **R242-ARCH-27 [REFACTOR]** `internal/cli/process_turn.go:36` interruptedSettleWindow 500ms 与 runDeadlineWatchdog 并发未协同；化为 process 级配置 + watchdog settle 完成再清 inflight。
- [ ] **R242-ARCH-28 [REFACTOR]** `internal/server/wshub.go:108-111` `cronHubOps.EnsureStub func(string) bool` false 三义；改 `(ok bool, reason string)`。
- [ ] **R242-ARCH-29 [REFACTOR]** `internal/cron/scheduler.go:1741-1813` TriggerNow 三层嵌套 `entry.WrappedJob` 依赖 robfig 内部状态；引入 `s.cron.HasEntry(entryID)` 抽象。
- [ ] **R242-ARCH-30 [REFACTOR][NEEDS-DESIGN]** `internal/session/managed.go:120` `processBox{ p processIface }` wrapper 每次 storeProcess alloc；用 atomic.Value 或 sync.Pool。已分析（cron-fix-F4，managed.go processBox godoc 锁定结论）：atomic.Value 拒绝 inconsistent dynamic type 强制所有测试 fake 用 *cli.Process 不可接受；sync.Pool 与 loadProcess 跨 goroutine retain pointer 冲突（Hub broadcast / dispatch / cron Send）会 use-after-free，需 ref-count 才能安全。storeProcess 仅 spawn/attach/kill/`/new`/`/clear` 冷路径触发 ~分钟级，16B alloc 被周围 goroutine + lifecycle 工作量碾压。降级 NEEDS-DESIGN。

## Round 241 — 5-agent 并行 code review 第 51 轮（2026-05-24，紧随 Round 240）NEEDS-DESIGN

> 紧随 Round 240 的第 51 轮 5-reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描，无 in-flight PR 排除项；REPEAT 提示提示绝大部分 ARCH-1..ARCH-20 / CR-1..CR-13 已登记 ≥3 次，本批避开。
>
> 本批直接修 8 处见上方 commits（cmd readJSONWithRetry 用 NewTimer 避免 ctx 取消时 timer 泄漏 [R241-GO-1] / cmd 加 readJSONWithRetry ctx 取消测试 [R241-CR-1] / cron SetJobPrompt persist 失败完整 rollback Prompt+Paused [R241-CR-2] / cron 断言 SetJobPrompt rollback 效果 [R241-CR-3] / cron 清理 truncateForRetry 残留注释 [R241-CR-4] / cron 简化 NewScheduler maxJobs warn 条件 [R241-CR-5] / server serveLoginPage HSTS 仅在 TLS 时下发 [R241-SEC-1] BREAKING-LOCAL ≤2 包 / server handleList 循环外 capture now 与 StartedAt [R241-PERF-1]）。
>
> 限额：P1-SEC 无上限选 1（HSTS）；非安全 ≤6 选 6（GO-1 + CR-1..CR-5 + PERF-1 共 7，1 项算入 P1-SEC slot）；BREAKING-LOCAL ≤2 占 1（HSTS）。下列为本批新发现且不适合直接修的 NEEDS-DESIGN 条目（保留 v4 二级分类标签）。

### 安全（SEC）

- [ ] **R241-SEC-4 [SIMPLE]** `internal/server/dashboard_cron_transcript.go:263` — `discovery.ClaudeProjectSlug` 仅替 `/`→`-`，未过滤 \t \n \r 等控制字符；手编 cron_jobs.json WorkDir 含 tab 时生成奇异 filesystem path。建议在 `handleRunTranscript` 入口拒任何 byte<0x20。
- [ ] **R241-SEC-5 [SIMPLE]** `internal/server/project_files.go:791-836` — `serveRender` HTTP 响应同时附 `application/octet-stream + Content-Disposition: attachment` 与 `CSP: sandbox allow-scripts; script-src 'unsafe-inline' 'unsafe-eval'`。安全语义依赖浏览器永远不内联渲染 octet-stream，建议用 `sandbox` 不含 `allow-scripts` 的 belt-and-braces CSP。
- [ ] **R241-SEC-6 [SIMPLE]** `internal/server/dashboard_memory.go:160-170` — `tryRead` 把 `os.ReadDir` 返回的 `projectDir` name 直接 Join；name 由磁盘目录控制，含 `..` / 控制字节理论可影响最终路径。建议 lookup 在 Join 前对每个 dir name 做 regex 校验。
- [ ] **R241-SEC-8 [SIMPLE]** `internal/server/dashboard_send.go:916,990,1022` — `attachmentDirPrefix=".naozhi/attachments/"` 硬编码 forward-slash，`HasPrefix` 检查与 `filepath.Separator` 拼接不一致；当前靠 pre-clean 拒 `\\` 防 Windows，但跨平台契约不显式。建议加 godoc 注明语义或统一 `filepath.FromSlash`。
- [ ] **R241-SEC-9 [SIMPLE]** `internal/cron/store.go:50-63` — `loadJobs` 对 Lstat 非 ErrNotExist 错误仅 Warn 后继续走 `os.Open`，留 symlink-bypass 窗口（FUSE 伪 EBUSY）。建议任何非 ErrNotExist 错误硬中止。
- [ ] **R241-SEC-10 [REFACTOR]** `internal/server/dashboard_auth.go:115-120` — `cookieMAC = HMAC(secret, dashboardToken)` 是确定性的：cookie 不轮换，被盗后只能轮换 `dashboardToken` 才能 invalidate（杀所有 session）。建议 MAC 绑定 per-session nonce/issue time，需服务端保存 session map（跨 ≥3 包改动）。
- [ ] **R241-SEC-13 [SIMPLE]** `internal/server/wshub.go:51-56` — pre-encoded WS frames `wsAuthFailInvalidMsg` 等是共享 `[]byte`，未来 `append` 即 race。建议改 `string` const，每次 send 时转换。
- [ ] **R241-SEC-14 [SIMPLE]** `internal/server/ip_limiter.go:20-25` — `newIPLimiterWithProxy` 未设 `MaxKeys`/`TTL`，IP flood 下 LRU 无界（loginLimiter / wsUpgradeLimiter 已设 10000）。建议补默认 cap。
- [ ] **R241-SEC-15 [SIMPLE]** `internal/selfupdate/selfupdate.go:385-390` — `verifyChecksum` 对同一 asset 多条目仅取首条匹配，理论允许 attacker 在 checksums.txt 注入弱 hash 后续条目（被忽略，但格式不严格）。建议断言唯一匹配。
- [ ] **R241-SEC-16 [SIMPLE]** `internal/server/dashboard_cron_transcript.go:418` — `LimitReader` 截断检测靠 `f.Seek(0, SeekCurrent)`：`LimitReader` 不前推 fd，seek 反映 fd 位置而非 reader 限额，边界 case 下 `truncated` 可能误判。建议用 wrapping reader 直接置 flag。

### 性能（PERF）

- [ ] **R241-PERF-3 [SIMPLE]** `internal/server/dashboard_cron.go:443-544` — `handleList` 1Hz 轮询路径里对每个非 paused job 都跑 `cron.HasMissedSchedule`，内部含 `cronParser.Parse`（regexp NFA）+ 2 次 `sched.Next`；50 jobs/s 重复 Parse。建议 `JobWithNextRun` 预解析 schedule 或缓存到 Job 字段。
- [~] **R241-PERF-5 [SIMPLE]** `internal/cli/process_readloop.go:572` — `dispatchProtocolEvent` task_started 分支为每事件 `go linker.Resolve(...)` 并拷 8KB Description；resolveSem 限流但仍裸 goroutine 调度。建议改 worker pool。
- [ ] **R241-PERF-6 [SIMPLE]** `internal/cron/runstore.go:770-784` — `cacheTrimAfterDisk` 每次新分配 keep slice，热路径每次堆分配。建议复用底层数组（`runs[:0]` + append），仅 cap 缩减时 make。
- [ ] **R241-PERF-7 [SIMPLE]** `internal/server/dashboard_cron_transcript.go:479` — `flattenJSONLEvent` 每行 `make([]transcriptTurn, 0, 2)`；500 行 cron 日志 = 500 次堆分配。建议改 caller-provided scratch slice append。
- [x] **R241-PERF-8 [REFACTOR]** `internal/session/store.go:117-213` — `saveStore` 在 map 迭代中串行多 atomic load；建议抽 `sessionToStoreEntry` helper，逻辑不变但可独立 benchmark + 后续并行化（跨 ≥3 文件 + DI）。 — 已修（cron-fix-F4 2026-05-24）：抽出 `sessionToStoreEntry(s *ManagedSession) (storeEntry, bool)` helper，saveStore body 由 ~80 行收紧到 7 行（gather → marshal → atomic write → meta sidecar）。helper 含 CONTRACT godoc 强调 r.mu→s.historyMu 锁顺序，独立可单测/可 benchmark；后续 worker pool 并行化的纯函数前置已就位。逻辑零改变（scratch/sys-skip + sid+cost+prev clone 全量保留）。`go test ./internal/session/` 全 pass。
- [ ] **R241-PERF-9 [SIMPLE]** `internal/cron/scheduler.go:3054-3064` — `marshalJobsLocked` 每次 persistJobsLocked 都 `slices.SortFunc` 全表；50 jobs × log50 ≈ 280 比较，可忽略，但建议可在 mutation 路径维护已排 ID 列表。

### 代码质量（CR / GO）

- [~] **R241-CR-6 [SIMPLE]** `internal/cron/runstore.go:375-378` — warmCache 注释提到"warm=false on disk error"，但 R239-CR-6 之后 `warm` 总置 true，注释 stale。建议同步注释。
- [~] **R241-GO-2 [SIMPLE]** `internal/cron/scheduler.go:1278-1313` — `DeleteJobByID` IIFE 用 `j == nil` 当 not-found 哨兵；当前正确但与"j 是 *Job 可为 nil"潜在用途冲突。建议加 `found bool` 显式区分。
- [ ] **R241-GO-3 [SIMPLE]** `internal/cron/scheduler.go:1317-1346` — `PauseJobByID` 同上 nil-sentinel 模式。同 R241-GO-2 修复。
- [ ] **R241-GO-4 [SIMPLE]** `cmd/naozhi/main.go:536-538` — `RefreshSettings` 在 ctx cancel 后 mid-retry 静默返回空 settings path，operator 无 log signal。建议在 `writeClaudeSettingsOverride` 加 `ctx.Err()` 时 Warn。
- [~] **R241-CR-7 [SIMPLE]** `internal/dispatch/coalesce.go:117` — 截断 marker 用 `fmt.Fprintf` 而紧邻的 fast path 已用 WriteString 避反射。建议要么补统一注释要么改 strconv.AppendInt。

### 架构（ARCH）

- [ ] **R241-ARCH-1 [SIMPLE]** `internal/cli/wrapper.go:151-172` — `NewWrapper` 构造期同步执行 `detectVersion(cliPath)` 启 5s subprocess；纯字段赋值构造函数变成阻塞 IO。建议提到 `Probe(ctx)` 或 `LazyVersion()`。
- [~] **R241-ARCH-3 [SIMPLE]** `internal/cron/scheduler.go:204-213` — `platforms`/`agents`/`agentCommands` map godoc 说 "immutable after NewScheduler" 但 caller 持原引用可改底层 map → cron 包内 lock-free 读会 race。建议构造时 `maps.Clone` 切断引用。
- [ ] **R241-ARCH-4 [SIMPLE]** `internal/cron/scheduler.go:331-368` — `SetOnExecute / SetOnRunStarted / SetOnRunEnded` 三个独立 setter；加事件类型必改 struct + 增 setter。建议抽 `SchedulerListener` 单接口。
- [ ] **R241-ARCH-5 [BREAKING-LOCAL]** `internal/cron/scheduler.go:402-426` — runStore 半 facade：facade 方法只覆盖 List/Recent/Get，scheduler.go 多处直接访问 `s.runStore.{Append,trimAll,cacheInvalidate}`；要么 facade 闭合要么提升为 `internal/cron/runs` 子包。
- [ ] **R241-ARCH-6 [SIMPLE]** `internal/cron/scheduler.go:848-863` — `registerStubByValue / registerStubFromJob` nil-router 静默 no-op；wireup bug 不会启动期 fail。建议构造时 `panic("Router must be set")`，测试走 `WithoutRouter()` 旁路。
- [ ] **R241-ARCH-7 [SIMPLE]** `internal/cron/scheduler.go:2143-2156` — `classifyExecError` 仅覆盖 DeadlineExceeded，Canceled 由 caller `errors.Is` 提前分流；状态映射散在 6 处。建议 helper 完整覆盖让 caller 不再 if-else。
- [ ] **R241-ARCH-8 [SIMPLE]** `internal/cron/runstore.go:101-103` — `MaxRunRecordBytes` 是导出常量但 `newRunStore` 不接此参数，而 keepCount/keepWindow 都接；可调性不对称。
- [ ] **R241-ARCH-9 [BREAKING-LOCAL]** `internal/cron/scheduler.go:2329-2407` — executeOpt 内 `s.router.GetOrCreate / sess.Send / InterruptViaControl` 穿透 SessionRouter consumer interface（仅声明 GetOrCreate/Reset/RegisterCronStubWithChain），cron 直接持 *ManagedSession 调 50+ method。`deadlineInterrupter` 微接口 (line 2102) 仅覆盖 InterruptViaControl，Send 没有窄接口。建议扩展为 `JobSession` 接口收口，或承认 ManagedSession 是事实公共 API。
- [ ] **R241-ARCH-10 [SIMPLE]** `internal/cron/scheduler.go:684-744` — NewScheduler 把 13+ cfg 字段直接复制到 struct；`applyDefaults` 仅覆盖部分默认值（maxPerChat/allowedRootResolved/cronLogger 仍 inline）。建议 applyDefaults 完整覆盖让 NewScheduler 退化为字段镜像。
- [ ] **R241-ARCH-11 [SIMPLE]** `internal/cron/scheduler.go:587` — `cronSlowThreshold = 30s` 包级 const 不可配；ExecTimeout=300s 部署天天误报 slow。建议 `SchedulerConfig.SlowThreshold`。
- [ ] **R241-ARCH-12 [SIMPLE]** `internal/cron/scheduler.go:2842-2879` — `resolveNotifyTarget` 5 路决策树仅 inline if-else，调用方 / dashboard 无法解释决策原因。建议抽 `NotifyDecision{Target, Source string}` enum。
- [ ] **R241-ARCH-13 [SIMPLE]** `internal/cron/scheduler.go:2705-2723` — `emitOverlapSkipped` CAS 失败 fast-path 仍走全套 lifecycle hook + 2 次 metric bump + WS broadcast；高频 trigger 放大 hub 锁压。建议 lite-path：仅 metric + 单次 WS event。
- [ ] **R241-ARCH-14 [SIMPLE]** `internal/sysession/runner.go:118-134` — `runnerImplBaseArgs` 与 `internal/cli/protocol_claude.go:BuildArgs` 靠注释 + 人为对齐；新加 backend 又得加一份。建议抽到 `backend.Profile.OneshotArgs`。
- [ ] **R241-ARCH-15 [SIMPLE]** `internal/cron/scheduler.go:2102-2113` — `abortResult` 含 `outcome session.InterruptOutcome`，cron 反向耦合 session 公共类型。建议本地 enum `InterruptResult{Fired,Suppressed,Unsupported}`。

## Round 240 — 5-agent 并行 code review 第 50 轮（2026-05-24，与同日 Round 239 / PR #299 Round 238 并发触发）NEEDS-DESIGN

> 与同日 Round 239（下方章节）/ PR #299 Round 238 并发触发的第三批 5-reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描，覆盖 540 .go 文件。**注意 commit anchor 仍用 R239-X**（提交时 master Round 239 尚未 merge，与 master Round 239 / #299 同名但落地目标不同 — 本批改 cron/scheduler.go 三处 unlock 模式 + applyDefaults 副作用 + 错误链双%w + dispatch coalescePrefix 常量化等 11 处可立即修），文档归类用 Round 240 与 master Round 239 隔开避免双轨。
>
> 本批直接修 11 处见上方 commits（cron ResultBytes 与 Result 一致用 persistedResult 长度 [R239-CR-1] / cron DeleteJobByID/PauseJobByID/ResumeJobByID 改 defer Unlock 模式 [R239-CR-2] / cron SetJobPrompt resume 失败时回滚 Prompt 字段 [R239-CR-3] / cron applyDefaults 去除副作用 Warn 移至 NewScheduler [R239-CR-4] / cron inline truncateForRetry 一行 wrapper [R239-CR-5] / cron warmCache 删除恒 nil error 返回 [R239-CR-6] / cron ErrClassPausedConcurrent 加 reserved 注释 [R239-CR-7] / cron persistJobsLocked 错误链改双 %w 让 inner 可 introspect [R239-GO-1] / cron readRun 错误链改双 %w [R239-GO-2] / cmd readJSONWithRetry 接受 ctx 响应取消 [R239-GO-3] / dispatch coalescePrefix 常量化避免运行时 len 重算 [R239-PERF-1]）。
>
> Reviewer 自查：本批 P1-SEC 无新增（webhook 签名 / CSP / secret / SSRF / TLS / 反序列化攻击面均已闭合，CSP unsafe-inline / X-Forwarded-Host CSRF / nz_anon HMAC 等已在下方 Round 239 节再次确认未修复，本批不重复登记）。BREAKING-LOCAL 限额 ≤2，本批选 0（候选 R239-CR-2 改用 inner closure 完成 ≤1 包，未占 BREAKING-LOCAL slot）。约 50 项 [REPEAT-N] 见下方 Round 239 / R238 / R237 / 历史 ARCH-1~19 章节，本批不重复登记 anchor。下列为本批新发现且不适合直接修的 NEEDS-DESIGN 条目（保留 v4 二级分类标签）。

### Go 正确性 / 可观测性 — 本批新发现

- [~] **R240-GO-4 — `cron.Scheduler.executeOpt` `sendCtx` 双倍预算缺乏可观测性（P2）**：scheduler.go:~2349 `sendCtx` 派生自 `context.Background()`（与 R238-GO-4 同根因留 [REPEAT-N R238-GO-4]），budget 为 `jobTimeout` 独立计时；R230B-GO-1 注释承认 worst-case wall clock ~`2×jobTimeout`，但当 spawn ctx 已消耗 >50% 后再进 sendCtx 时无任何 slog 信号；运营 300s 任务的 operator 看不到这一类"翻倍执行"事件。建议在 `time.Since(startedAt) > jobTimeout/2` 时打 `slog.Warn("cron send budget exceeds job/2", ...)`，并补 expvar `cron_send_budget_doubled` counter。`[BREAKING-LOCAL]`
- [ ] **R240-GO-5 — `cron.scheduler.go:~2864 redactPathsInCronError` 无单测覆盖（P3）**：手写 70 行 byte scanner，覆盖 POSIX/Win drive/tilde-home 三种 path 形态，每个分支条件（`:` followed by space、tilde+`/`、isWin 分支）均无独立 unit table。当前依赖端到端 review。建议抽 `redactPath(in []byte) []byte` 私有函数 + table-driven test，`isWin` 分支在 Linux 部署上其实是死代码可加 `//go:build !windows` 收紧。`[REFACTOR]`
- [ ] **R240-GO-6 — `cron.scheduler.go:~3153 slogPrintfLogger.Printf` 用 strings.Contains("panic"|"recovered") 决定 log level（P3）**：依赖格式化后字符串 substring 决定 Warn vs Error，间接且无结构化字段；后续 robfig/cron 改提示文案会让等级误判。建议加 structured `"source":"robfig/cron"` 字段，level 固定 Error（生产路径只剩 recovered 才会进来）；或改用 `slog.Handler` 自定义 attrgroup。`[REFACTOR]`

### 性能 — 本批新发现

- [~] **R240-PERF-2 — `cli/eventlog.go:513-654 applyEntryStateLocked` O(N) 扫描 turnAgents/bgAgents（P2）**：`task_start`/`task_progress`/`task_done` 每事件均 `range` 切片定位 toolUseID；TeamCreate fan-out 50 subagent 时单轮 N×50 事件触发 O(N²)，eventlog 锁内 O(N²) 扫描在订阅者多时阻塞 broadcast。建议维护 `toolUseID→index` map：append agent 时更新，result/user 时整体清空。改动局限 eventlog 包但触及 turnAgents/bgAgents 字段语义。`[BREAKING-LOCAL]`
- [~] **R240-PERF-3 — `cli/eventlog.go:898-1010 AppendBatch` 历史重放路径 agent 状态更新无意义（P2）**：InjectHistory 一次 500 条 batch 在写锁内逐条调 `applyEntryStateLocked`，但历史重放不会触发 `task_done` 回调，agent state 更新对 replay 无观测价值；500 次 switch + agent 扫描在锁内放大 R240-PERF-2 的 O(N²) 问题。建议为 replay batch 加 `isReplay bool` 参数跳过 agent state 更新；或仅对 task_*/result/user 类型才调 applyEntryStateLocked。`[REFACTOR]`
- [~] **R240-PERF-5 — `dispatch/status.go:79-122 formatToolUse` 重复 json.Unmarshal（P3）**：Read/Edit/Write 三个 case 各自声明同形 `filePathInput {FilePath string}` struct 重复解码三次。建议提前一次性 Unmarshal 到 filePathInput，case 内只做格式化。`[REFACTOR]` 可下轮直接修
- [~] **R240-PERF-6 — `eventlog/persist/persister.go:967-998 selectForIdx` 单条 batch 走 stride 路径仍构造 kept slice（P3）**：`stride>1` 且 `len(pending)==1` 时仍走 estCap=2 + scratch reuse 路径，单条必然同时是 first 和 last，可直接 return pending。建议 `if len(pending) == 1 { return pending }` 早返回。`[REFACTOR]` 可下轮直接修

### 安全 — 本批新发现

- 无（R235-R238 已闭合 webhook 签名 / CSP / secret / SSRF / TLS / 反序列化主要攻击面；P1-SEC ×3 X-Forwarded-Host / nz_anon HMAC / dashboard CSP 在下方 Round 239 节再次登记，本批不重复）。

### 代码质量 / 架构 — 本批新发现

- 无新增独立 anchor。所有 CR/ARCH 项均落在历史 ARCH-1~ARCH-19 / R235-R238 / Round 239 同根因簇，本批按 v4 规则不重复登记新 ID，[REPEAT-N] 计数将在下轮统计中递增。

---
## Round 240 — 5-agent 并行 code review 第 50 轮（2026-05-24）NEEDS-DESIGN

> 5-reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描。本批直接修 6 处见上方 commits（cmd godoc 错位 [R240-CR-2] / dashboard_memory 加 EvalSymlinks 抗 symlink 穿越 [R240-SEC-1+2] / cron transcript resolvedRoot 仅 not-exist 时 fallback [R240-SEC-3] / scheduler slog 标签统一 job_id [R240-CR-1] / preflightArgs.runID 注释 8→16-char [R240-CR-3] / serveLoginPage HSTS 仅 TLS 设置 [R240-SEC-14]）。selfupdate fetchFile 强制 https 入口校验 [R240-SEC-4] 因现存 httptest 用 http:// 失败 abort，登记 NEEDS-DESIGN（需先迁 httptest 至 TLS）。下方为本轮新发现且不适合直接修的 NEEDS-DESIGN 条目（保留 v4 二级分类标签）。
>
> P1-SEC ×3（CSP / CSRF / nz_anon HMAC）本轮再次确认未修复，同 R236-SEC-02/03/06 主条目，标 [REPEAT-N] 不重复登记主条目。

### 安全（NEEDS-DESIGN）

- [ ] **R240-SEC-4 — `internal/selfupdate/selfupdate.go:313` fetchFile 入口未强制 https [REFACTOR]（P2）**：CheckRedirect 已校验后续 hop，但首次请求 URL 未校验。当前调用方均 hardcode https，但未来路径若传 http:// 则首段不受保护。本轮尝试加 `strings.HasPrefix(... "https://")` guard，但现存 selfupdate_test 用 httptest.NewServer (http://127.0.0.1) 失败。方向：先改 test 用 NewTLSServer + InsecureSkipVerify，再加入口 guard。Breaking：否（生产代码无变化，仅 test）。
- [ ] **R240-SEC-5 — `internal/server/dashboard_cron.go:247-249` cronJobView 在 GET /api/cron 列表全量返回 NotifyChatID / NotifyPlatform [REFACTOR]（P2）**：单 operator 假设下可接受，未来 multi-tenant 即 IM chat ID 泄漏到所有已认证 token。方案：明确文档化 single-operator 假设或在 multi-user 配置下 redact。Breaking：否。
- [ ] **R240-SEC-7 — `internal/server/dashboard_auth.go:338` naozhi_auth cookie 缺 Partitioned 属性 + 默认 Domain [REFACTOR]（P2）**：CHIPS 兼容前瞻；当前未设 Domain 假设 naozhi 独占 origin。方案：文档化此假设，必要时显式 `Path: "/"` 与 `Partitioned`。Breaking：否。
- [ ] **R240-SEC-8 — `internal/server/dashboard_cron_transcript.go:410` Scanner.Err 后 ErrTooLong vs IO 错误均坍塌为 truncated=true [REFACTOR]（P2）**：取证侧无法区分长行 vs 磁盘错误。方案：errors.Is(err, bufio.ErrTooLong) 分支改为 fallback="line_too_long"。Breaking：否。
- [ ] **R240-SEC-9 — `internal/transcribe/convert.go:26` lookupFFmpeg 在 sync.Once 缓存进程级 [REFACTOR]（P2）**：首次 PATH 注入即固化。方案：从 config 接 ffmpeg 绝对路径不依赖 PATH。Breaking：否（config 字段新增）。
- [ ] **R240-SEC-11 — `internal/server/dashboard_memory.go:160-187` tryRead 无尺寸上限 [REFACTOR]（P2）**：os.ReadFile 无 cap；超大 .md 重复 hover 触发 read+JSON serialize。方案：`maxMemoryFileBytes=256KB`，超出 truncated=true。Breaking：否。
- [ ] **R240-SEC-12 — `internal/server/dashboard_cron_transcript.go:83` ANSI 0x1b 检测依赖 json.Unmarshal 解码  隐式行为 [REFACTOR]（P3）**：方案：注释明确依赖。Breaking：否。
- [ ] **R240-SEC-13 — `internal/server/dashboard_cron.go:436-437` GET /api/cron 无 per-IP rate limit [REFACTOR]（P3）**：dashboard 1Hz polling，token 泄漏可枚举全量配置。方案：加 ipLimiter 与 runsLimiter 一致。Breaking：否。
- [ ] **R240-SEC-15 — `internal/server/dashboard_cron_transcript.go:397-409` 无 timestamp 事件 fresh=false 模式跨 run 泄漏 [REFACTOR]（P3）**：方案：drop 或按位置归属。Breaking：否。
- [ ] **R240-SEC-16 — `internal/config/config.go:1079-1090` expandEnvVars 无 env name 白名单 [REFACTOR]（P3）**：误填 ${ANTHROPIC_API_KEY} 等可能展开后被日志/API 反射。方案：限制 NAOZHI_ 前缀或对所有 string 字段加 containsEnvPlaceholder 检查。Breaking：否。

### Go 正确性（NEEDS-DESIGN）

- [~] **R240-GO-1 — `internal/cron/scheduler.go:1233-1241` deleteJobLocked 持 s.mu 期间调 router.Reset 锁序倒置风险 [BREAKING-LOCAL]（P1）**：router.Reset 内回调可能尝试 s.mu 写锁递归即死锁；EnsureStub godoc 已明确 must-not-hold-s.mu 与本函数自相矛盾。方案：deleteJobLocked 只做内存清理，router.Reset 移到调用方释放 s.mu 后调用。Breaking：本地（deleteJobLocked + 两 caller）。
- [ ] **R240-GO-4 — `internal/cron/runstore.go:268-309` skipAppendTrim appendsSinceTrim 增量在条件检查之前 [REFACTOR]（P2）**：batch counter 与 count-cap/window-cutoff 分支顺序混乱时计数失真。方案：++ 移到所有短路 return 之后；reset 统一在末尾。Breaking：否。
- [ ] **R240-GO-5 — `internal/cron/scheduler.go:952-963` Stop gcWG wrapper 与 triggerWG wrapper 同等泄漏注释缺失（P2）**：Stop 契约注释只覆盖 triggerWG。方案：gcWG wrapper 处补 "intentional orphan on timeout, same rationale as triggerWG"。Breaking：否（仅注释）。
- [ ] **R240-GO-6 — `internal/cron/runstore.go:395-410` warmCache + cacheGet 双锁路径在空目录下 warm=true && len(runs)==0 与 cache miss 不可区分（P2）**：caller 看到空 slice 而非 false，跳过 disk fallback。方案：cacheGet 在 warm && empty 时仍尝试 disk 或文档化"空 warm 是已知行为"。Breaking：否。

### 性能（NEEDS-DESIGN）

- [ ] **R240-PERF-5 — `internal/cron/runstore.go:662-706` trimJobLocked 每次 Append 接 ReadDir [REFACTOR]（P1）**：appendTrimBatch=10 阈值低；FUSE/NFS 下成本不可预测。方案：提高阈值或 trim 移后台 ticker。Breaking：否。
- [ ] **R240-PERF-6 — `internal/cron/runstore.go:221,235` json.Marshal 无 buffer pool [REFACTOR]（P2）**：每次 Append 分配 encodeState ~2KB。方案：sync.Pool + bytes.Buffer + json.NewEncoder（dashboard.go 已有此模式）。Breaking：否。
- [ ] **R240-PERF-10 — `internal/cron/runstore.go:487-563` diskListNewestFirst 每 .json 调 e.Info() Stat [REFACTOR]（P2）**：FUSE/NFS 下额外 syscall。方案：改 runID lex 排序消除 Info()。Breaking：否（sort 顺序变化）。
- [ ] **R240-PERF-11 — `internal/cron/scheduler.go:2153,2673` generateRunID crypto/rand.Read syscall [REFACTOR]（P2）**：1Hz × N jobs 热路径每 run ~200ns syscall。方案：math/rand/v2 + 时间戳混合，runID 非 crypto 凭证。Breaking：否。
- [ ] **R240-PERF-13 — `internal/session/managed.go:1418-1426` EventEntriesSince dead-session 路径每次 sortEntriesByTimeStable [REFACTOR]（P2）**：1Hz push 调用。方案：InjectHistory 保证有序或 backward binary search。Breaking：否。
- [ ] **R240-PERF-14 — `internal/cron/scheduler.go:2802-2810` recordResultP0WithSanitised inline anonymous struct snapshot [REFACTOR]（P3）**：方案：jSnapshot := *j 一次 value copy。Breaking：否。
- [ ] **R240-PERF-15 — `internal/server/wshub.go:1363-1398` broadcastToAuthenticated dashToken=="" 时 atomic Load 全 true 浪费 [REFACTOR]（P3）**：方案：no-token 模式 fast path 跳过 Load。Breaking：否。
- [ ] **R240-PERF-16 — `internal/cron/scheduler.go:1196-1218` ListAllJobsWithNextRun 三次 alloc [REFACTOR]（P3）**：方案：sync.Pool 复用 nextByID map（Go 1.21 maps.Clear）。Breaking：否。
- [ ] **R240-PERF-17 — `internal/cron/runstore.go:268-308` skipAppendTrim 每 Append entry.mu Lock 即使条件不满足 [REFACTOR]（P3）**：方案：appendsSinceTrim 改 atomic.Int32 减少锁频。Breaking：否。
- [ ] **R240-PERF-20 — `internal/cron/runstore.go:395-411` warmCache 持 jobLock 同步读 N 文件 [REFACTOR]（P3）**：cold start 200 jobs × 200 文件串行；用户首次 List 阻塞。方案：Start 时后台并发 warm。Breaking：否。

### 代码质量（NEEDS-DESIGN）

- [~] **R240-CR-4 — `internal/server/wshub.go:622` per-client 订阅上限 50 magic number [REFACTOR]（P2）**：与 maxWSConns/maxSubscribersPerKey 同包但未命名。方案：抽 const maxSubscriptionsPerClient = 50。Breaking：否。
- [~] **R240-CR-5 — `internal/dispatch/dispatch.go:742,869` ReplyWithRetry 重试次数 3 magic number [REFACTOR]（P2）**：两处独立修改易飘。方案：抽 const platformReplyMaxAttempts = 3。Breaking：否。
- [~] **R240-CR-6 — `internal/server/wshub.go:1217` resubscribeEvents `for i := range 12` + 5s magic numbers [REFACTOR]（P2）**：60s 总窗口未注释。方案：抽 const resubscribeMaxAttempts/resubscribeInterval。Breaking：否。
- [ ] **R240-CR-8 — `internal/dispatch/dispatch.go:956,973,1164,1317` replyTracker 4 方法缺 godoc [REFACTOR]（P2）**：onEvent 是 IM 流式核心。方案：补 godoc 标注线程约束 + 超时契约。Breaking：否。
- [ ] **R240-CR-9 — Hub 方法缺 godoc [REFACTOR]（P2）**：register/unregister/handleAuth/handleSubscribe/handleUnsubscribe/handleSend/handleInterrupt/handleRemote*/eventPushLoop/resubscribeEvents/doBroadcastSessionsUpdate/capHistoryBatch/PurgeNodeSubscriptions 等 12+ 方法缺 godoc。**注**：PR #327 R243-ARCH-2 split 后这些方法分散在 wshub_upgrade.go / wshub_subscribe.go / wshub_send.go / wshub_eventpush.go / wshub_broadcast.go — 旧行号引用 (461, 1821 等) 失效，按函数名 grep 即可。Breaking：否。
- [ ] **R240-CR-10 — `internal/config/config.go:477,551,584,1092` applyDefaults/parseDurations/validateConfig/containsEnvPlaceholder 缺 godoc [REFACTOR]（P2）**：方案：补流水线契约说明（first-error vs errors.Join）。Breaking：否。
- [ ] **R240-CR-12 — `internal/cron/scheduler.go:2943` notifyTarget vs NotifyTarget vs deliverNotice 三层命名混淆 [REFACTOR]（P3）**：方案：私有方法重命名为 sendNoticeToChat 或 sendViaPlatform。Breaking：否。
- [ ] **R240-CR-14 — `internal/server/dashboard_send.go|dashboard_session.go|dashboard_cron.go` 3 文件超 800 行 [REFACTOR]（P3）**：方案：按 endpoint 拆子文件。Breaking：否。
- [ ] **R240-CR-15 — `internal/cron/runstore.go` 856 行超上限 [REFACTOR]（P3）**：方案：拆 runstore_gc.go + runstore_cache.go。Breaking：否。

### 架构（NEEDS-DESIGN）

- [ ] **R240-ARCH-1 — `internal/server/server.go:55-100` god-object Server 30+ 异质字段 [REFACTOR]（P1）**：12 handler + router + dedup + 各 cache + watchdog。方案：抽 serverDeps + handlerSet。Breaking：否（内部）。
- [ ] **R240-ARCH-5 — `internal/server/dashboard.go:299,341` cron/sysession 钩子两步 wiring [REFACTOR]（P1）**：SetScheduler + SetOnRunStarted/Ended 分散。方案：cron.RegisterBroadcaster(b CronBroadcaster) 接口；同模式套 sysession。Breaking：否。
- [ ] **R240-ARCH-7 — `internal/server/dashboard_session.go:280` server 三个 cron 接口 cronStubChecker/cronSessionLister/cronHubOps [REFACTOR]（P2）**：方案：与 ARCH-3 合并到 cron.ServerSurface。Breaking：否。
- [ ] **R240-ARCH-8 — `internal/cli/eventlog.go` vs `internal/eventlog/persist+schema` 双 eventlog 物理包 [REFACTOR]（P2）**：方案：rename in-mem ring 到 cli.EventBuffer 或 internal/eventbuffer。Breaking：否（rename）。
- [ ] **R240-ARCH-9 — `internal/sysession/manager.go:138` Manager + Scheduler + Router 三个 lifecycle 形状不一 [REFACTOR]（P2）**：方案：抽 internal/lifecycle.Daemon + RunReporter。Breaking：否。
- [ ] **R240-ARCH-10 — `internal/cron/scheduler.go:3010` package-level init() 注册 marshalJobs atomic.Pointer [REFACTOR]（P2）**：隐藏全局；测试可见。方案：cfg.MarshalFn 字段 default json.Marshal。Breaking：否。
- [ ] **R240-ARCH-12 — `cmd/naozhi/main.go:735-789` cron + sysession wiring 命令式 60 行 blob [REFACTOR]（P2）**：顺序约束仅在 main.go 里。方案：抽 internal/wireup.WireSchedulers。Breaking：否。
- [ ] **R240-ARCH-14 — `internal/server/server.go:76-88` 12 handler 字段全具体类型 [REFACTOR]（P2）**：handler 间互依赖（SendHandler↔Hub↔ScratchHandler）。方案：抽 httpHandlerSet 接口 + 自注册。Breaking：否。
- [ ] **R240-ARCH-15 — `internal/upstream/connector.go:69` 等 5 个 "subset of session.Router" 接口 [REFACTOR]（P2）**：HubRouter 14 / dispatch 8 / cron 3 / sysession 4 / upstream 各一份；签名飘移风险。方案：internal/sessioniface 包提供 RouterReader/Mutator/Dispatcher mixin。Breaking：否。
- [ ] **R240-ARCH-17 — `internal/cli/history.go:122` RegisterHistoryFactory + cli/wrapper pickHistoryFactory 反向依赖 [REFACTOR]（P3）**：内部 cli 持 history 全局 map。方案：HistoryFactoryFn 注册迁到 internal/history。Breaking：是 (NewWrapper 签名)。
- [ ] **R240-ARCH-20 — `internal/cli/backend/profile.go:22` cli/backend 反向依赖 cli (Profile.NewProtocol returns cli.Protocol) [REFACTOR]（P3）**：cli 不能 import cli/backend 即 cycle。方案：rename 到 internal/backend 顶层。Breaking：是 (import paths)。
- [ ] **R240-ARCH-24 — `internal/cron/scheduler.go:163-187` cron RunStartedEvent vs sysession DaemonRunStartedEvent 平行复制 [REFACTOR]（P3）**：方案：抽 internal/lifecycle/RunEvent + Hub 单 BroadcastRunEvent(category, ev)。Breaking：否（wire 兼容）。
- [ ] **R240-ARCH-25 — `internal/server/server.go:76-88` handler 不可独立测试 [REFACTOR]（P3）**：构造方法私有且含跨 handler 依赖。方案：每 handler 公开 Deps struct。Breaking：否。
- [ ] **R240-ARCH-26 — `internal/server/wshub.go:179` tailers + wshub_agent 子能力嵌在 god-object [REFACTOR]（P3）**：方案：抽 AgentBroadcaster 子组件。Breaking：否。
- [ ] **R240-ARCH-27 — `internal/cron/scheduler.go:65-91` SessionRouter consumer 接口写在 scheduler.go 顶部不在 consumer.go [REFACTOR]（P3）**：与 dispatch/server/sysession/upstream 约定不一致。方案：移到 internal/cron/consumer.go。Breaking：否。
- [ ] **R240-ARCH-29 — `internal/server/dashboard.go:299,341` Server.Start 后置 wiring 两阶段构造 [REFACTOR]（P3）**：方案：把 callback wiring 全移进 NewWithOptions/buildServer。Breaking：否。
- [ ] **R240-ARCH-30 — `internal/dispatch/dispatch.go:71` Dispatcher 持具体 *cron.Scheduler [REFACTOR]（P3）**：方案：抽 dispatch.CronOps consumer 接口。Breaking：否。

## Round 239 — 5-agent 并行 code review 第 49 轮（2026-05-24，与 PR #299 Round 238 并发批）NEEDS-DESIGN

> 与同日 PR #299 Round 238 并发触发的另一批 5-reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描。本批直接修 3 处见上方 commits（cron loadJobs 校验 NotifyChatID/NotifyPlatform [R238-CR-2 anchor]，与 #299 [R238-CR-2] commit anchor 同名但目标字段不同 — #299 改的是 Lstat 错误升级，本 PR 加 NotifyChatID/NotifyPlatform 校验，二者非冲突 / cron AddJob ID 碰撞重试 slog.Warn [R238-CR-15] / cron HasMissedSchedule 单次 Parse 复用 sched 减少 dashboard 1Hz × N jobs 重复正则 [R238-PERF-2]）。文档归类用 R239 与 #299 隔开避免双轨；下方为本轮新发现且不适合直接修的 NEEDS-DESIGN 条目（保留 v4 二级分类标签）。
>
> 跳过项：F3 fix agent 因 import cycle（internal/cli/backend 反向依赖 internal/cli）跳过 R239-ARCH-K（wrapper.go backendDisplayName/detectCLI 走 Profile）；新 master 已通过 [R225-CR-2] knownBackendBinaries map 部分缓解，深度重构仍需先抽 backendreg 子包，归 [REFACTOR] 跟踪。
> P1-SEC ×3（CSRF X-Forwarded-Host / nz_anon HMAC / dashboard CSP unsafe-inline）本轮再次确认未修复，同 R236-SEC-02/03/06 主条目。

### 安全（P1，无限额）— 本轮再次确认未修复


### 安全（P2/P3）— 本轮再次确认

- [ ] **R239-SEC-5 — `internal/cron/scheduler.go:2143` ResolveAgent 后的 cleanText 到 IM 通知路径未拦截 bidi 字符**：containsCronC0 已覆盖 cron loadJobs 路径，但 scheduler.deliverNotice 路径仍走 sanitiseRunResult，未消化 bidi。方案：sanitiseRunResult 增加 bidi rune 过滤，或 deliverNotice 前调 textutil.SanitizeText。Breaking：否。

### Go 正确性（剩余）

- [ ] **R239-GO-1 — `internal/cron/scheduler.go:1200` deleteJobLocked 在 s.mu 持有期间调 router.Reset 可能导致锁序倒置（P1）**：Reset 内部取 r.mu，cron-internal → execute 路径 RegisterCronStubWithChain → notifyChange 回调可能尝试 s.mu。方案：deleteJobLocked 只做内存清理，把 router.Reset 移到调用方释放 s.mu 后调用。Breaking：否。
- [ ] **R239-GO-2 — `internal/cron/scheduler.go:2031` deadlineInterrupter 与 processIface 同名 InterruptViaControl 签名不一致（P1）[REFACTOR]**：deadlineInterrupter 返回 InterruptOutcome，processIface 返回 error；测试 stub 必须分别维护，重构风险点。方向：统一签名或在 managed.go 加专门 shim。
- [~] **R239-GO-4 — `internal/cron/scheduler.go:1377-1472` UpdateJob 多分支 return 但 s.mu 用显式 Unlock 而非 defer（P2）**：当前 line 1449 错误路径正确 Unlock，但任何后续 return 路径添加都易遗漏。方案：改 defer s.mu.Unlock() 或抽 UpdateJobLocked helper。Breaking：否。
- [~] **R239-GO-5 — `internal/cron/runstore.go:249-290` skipAppendTrim 缺"调用前必须持 jobLock"显式断言（P2）**：当前 Append 路径正确，但函数本身只取 entry.mu，未来若有新 caller 不持 jobLock 复用即引入竞态。方案：godoc 顶部加 caller-contract + 内部 race-detector friendly assertion。Breaking：否。
- [ ] **R239-GO-6 — `internal/cron/runstore.go:346-392` cacheGet 无 jobLock 保护下的 entry.warm 双 check 走 warmCache double-check locking（P2）**：当前实现正确（warmCache 内 Lock 二次校验），但 cacheGet 路径与 warmCache 之间窗口允许并发 disk read，文档未明确"刻意接受重复 disk read 换无锁"。方案：godoc 注解 double-checked locking 是有意选择。Breaking：否。
- [ ] **R239-GO-7 — `internal/cron/scheduler.go:2966-2971` marshalJobs 包级 atomic.Pointer 在 t.Parallel 测试下可被另一测试改写覆盖（P3）[BREAKING-LOCAL]**：当前测试 swap 顺序未明确；未来加并行测试即引入 flake。方案：marshalFn 注入 NewScheduler，删包级 atomic。Breaking：本地（仅 NewScheduler 签名 + 测试 helper）。
- [~] **R239-GO-9 — `internal/cron/scheduler.go:2829-2898` redactPathsInCronError isWin 检测未覆盖 UNC `\\server\share` 与 `//` 前缀（P3）**：Linux 容器内运行通常无影响；WSL / Windows mount 路径泄漏可能漏 redact。方案：注释明示 UNC 不在范围或补充 `\\` 前缀检测。Breaking：否。

### 性能（剩余）

- [ ] **R239-PERF-5 — `internal/cron/runstore.go:628-713` Append 路径双 ReadDir（warmCache + trimJobLocked）（P2）**：trimJobLocked 与 diskListNewestFirst 各自独立 ReadDir + e.Info() 排序。方案：trimJobLocked 复用 diskListNewestFirst 已排序 slice 判 trim 边界。Breaking：否。
- [ ] **R239-PERF-6 — `internal/server/wshub.go:1467` BroadcastSessionsUpdate 防抖 time.AfterFunc 每次新闭包逃逸（P2）**：高频侧边栏刷新触发新 func() alloc。方案：Hub 字段预存可重用闭包，debounceMu 下赋值。Breaking：否。
- [ ] **R239-PERF-8 — `internal/cron/scheduler.go:2183` executeOpt 内 slog.With 每次 alloc Logger Attr slice（P2）**：50 job × 1Hz 多次 With 调用累积 GC pressure。方案：单次 With + 局部复用，或 with 模板缓存。Breaking：否。
- [~] **R239-PERF-9 — `internal/cli/eventlog.go:1039-1051` notifySubscribers map[*subscriber]struct{} 迭代 hot-path 25K 次/s（500 sess × 50 events × 1 sub 平均）[BREAKING-LOCAL]**：同根因 R230C-PERF 已分析归档，但 reviewer 重新论证 []*subscriber + swap-to-end unsub 不破坏 closeOnce 语义。方案：subscribers 改 slice + subMu 下 swap-to-end shrink。Breaking：本地（subscribers 内部数据结构）。
- [ ] **R239-PERF-11 — `internal/cron/scheduler.go:936-955` Stop GC drain 用临时 channel + wrapper goroutine 等待 gcWG（P3）**：本轮 PR #299 GO-1 已把 timer 改 NewTimer 收紧；但 wrapper goroutine 与临时 channel 仍 alloc，可改 ctx 派生模式。方案：gcWG 改 ctx-aware 或维持现状（每次 Stop 仅 1 次，影响微小）。Breaking：否。
- [ ] **R239-PERF-12 — `internal/cron/runstore.go:249-290` skipAppendTrim 在 entry.mu 下独立调 time.Now（P3）**：Append 路径已有 now，可统一传入。方案：skipAppendTrim 接 now 参数避免持锁路径重复 vDSO。Breaking：否。
- [~] **R239-PERF-13 — `internal/cron/scheduler.go:1975-1995` freshContextPreflightP0 refresh 闭包每次 s.mu.RLock 重读同一 job（P3）**：snap 已含 workDir/prompt 固化值，可直接调 registerStubByValue。方案：refresh 不再读 s.jobs map。Breaking：否。
- [ ] **R239-PERF-15 — `internal/cron/scheduler.go:1864-1885` snapshotJob 内 jobTitleOrFallback → textutil.FirstLine + Truncate 在 RLock 下重复计算（P3）**：高频短周期 job 累积调用。方案：Job 加非持久化 labelCache 字段，AddJob/UpdateJob/SetJobPrompt 失效。Breaking：否。

### 代码质量（剩余）

- [~] **R239-CR-4 — `internal/server/dashboard_cron.go:369-391` validateCronTitle 与 validateCronPrompt 共享 25 行 C0+IsLogInjectionRune 扫描（P2）**：现有注释解释为何不共用 validateStringField，但底层两段扫描可加 disallowLF knob 抽到 stringFieldPolicy。方案：扩展 stringFieldPolicy + disallowLF。Breaking：否。
- [~] **R239-CR-5 — `internal/dispatch/dispatch.go:667` shutdown 路径 `5*time.Second` 与 platformReplyTimeout(15s) 不一致且未命名（P2）**：方案：抽 const shutdownReplyTimeout = 5*time.Second + 注释解释为何短于 platformReplyTimeout（ctx 已 cancel 期望快速 fallback）。Breaking：否。
- [ ] **R239-CR-6 — `internal/cron/scheduler.go:450-456` IsExcluded godoc 与 KnownSessionIDs 性能契约不一致（P2）**：godoc 自陈"auto-chain spawn 路径每次最多调一次"但函数本身每次 O(jobs × recentCap)。方案：godoc 标记 batch caller 应直接走 KnownSessionIDs。Breaking：否。
- [ ] **R239-CR-7 — `internal/cron/runinflight.go:157-181` JobRunCounters 类型与 addRun 方法所在文件名不直观（P2）**：定义在 runinflight.go 但本身是 Job 字段。方案：移到 job.go 或新建 counters.go，runinflight.go 专注 in-flight。Breaking：否。
- [ ] **R239-CR-8 — `internal/dispatch/commands.go:362-488` handleCronCommand 126 行内联 5 个 switch arm 缺 helper（P2）**：与 handleCdCommand/handleProjectCommand 不对称。方案：抽 handleCronAdd/Del/Pause/Resume helper 各自单测。Breaking：否。
- [~] **R239-CR-9 — `internal/server/dashboard_cron.go:1257-1273` runDetailView 匿名 struct 在 handler body 内声明（P2）**：与 cronRunSummaryView/cronJobView/cronRunCountersView 包级类型风格不一致；测试无法引用。方案：提升为 cronRunDetailView 包级类型。Breaking：否。
- [ ] **R239-CR-10 — `internal/cron/scheduler.go:205-209` `platforms`/`agents`/`agentCommands` immutable-after-construction 注释只在 struct 定义点，调用点 executeOpt/notifyTarget 缺无锁说明（P2）**：方案：每个无锁读站点加 "// reads without s.mu: immutable after NewScheduler" 注释。Breaking：否。
- [ ] **R239-CR-13 — `internal/cron/job.go:381-400` validateSchedule 没 godoc 错误契约（P3）**：与 schedulePeriod 缺有签名 godoc 不对称。方案：补充 "returns error if Parse fails OR interval < minCronInterval"。Breaking：否。
- [ ] **R239-CR-14 — `internal/dispatch/dispatch.go:856-857` replyTracker godoc 错位（P3）**：注释在 type 声明行后，go doc 不显示。方案：移动到 type 行前。Breaking：否。
- [ ] **R239-CR-16 — `internal/cron/runstore.go:296-298` appendTrimBatch=10 含数学推导但未标 "units: Append calls"（P3）**：方案：注释加 units 显式标记。Breaking：否。

### 架构（剩余 — 大量与历史 ARCH-* 同根因，多数已被同日 R238-ARCH-1/2/3/4 / R237-ARCH 系列覆盖）

- [ ] **R239-ARCH-A — `internal/cli/wrapper.go:43` ShimManager 是公开可变 *shim.Manager 把 protocol+transport 压成同一抽象（P1）[REFACTOR]**：同 R235-ARCH-4 / R237-ARCH-1。方向：抽 cli.Transport 接口，shim 是其一实现；cli 不再 import internal/shim。
- [ ] **R239-ARCH-C — `internal/session/managed.go:47` processIface 27 方法 god-interface（P1）[REFACTOR]**：同 R215-ARCH-P1-3 / R219-ARCH-7 / R224-ARCH-5 / R237-ARCH-3。方向：拆 ProcessSender / ProcessLifecycle / EventSource。
- [ ] **R239-ARCH-D — `internal/session/managed.go:1731-1733` ManagedSession 内回调 backend.RegisterDefaults() 触发全局注册（P1）[REFACTOR]**：业务对象懒触发全局注册，违反 main 显式 wire。方向：构造路径要求 caller 已注册。
- [ ] **R239-ARCH-E — Claude env 白/黑名单常量散落 main.go + internal/shim/manager.go:925 + internal/sysession/run.go EnvAllowlist 三份不同政策（P1）[REFACTOR]**：方向：内聚 internal/envpolicy 包。
- [ ] **R239-ARCH-F — `internal/cli/backend/profile.go:155-163` backend.Register 重复 ID 直接 panic 无运行时刷新（P1）[REFACTOR]**：多租户/测试隔离需 withCleanRegistry 黑魔法。方向：注入式 ProfileRegistry 由 main 持有。
- [ ] **R239-ARCH-G — `internal/session/key.go:145-160` plannerKeyFor "project:{name}:planner" 与 internal/project.PlannerKeyFor 双方硬编码字面量靠双向测试断言保持一致（P1）[REFACTOR]**：方向：抽 internal/keyspec 零依赖共享包。
- [ ] **R239-ARCH-H — `internal/platform/platform.go:134` Reactor / QuestionCardSender / RunnablePlatform 三个 capability interface 散在包根（P2）[REFACTOR]**：加新 capability 须改多处 AsX()。方向：Capability 注册表或 typed-nil discriminator。
  - **2026-05-24 cron-fix-F3 评估降级（[REPEAT-7] → [REPEAT-8] + [BREAKING-LOCAL]）**：尝试 server-only 局部 helper 不可行。Hub 的 linker producer 是 `sess.SubagentLinker() *cli.SubagentLinker`（`internal/session/managed.go:1318`）；即便 server 内部把 map 改成 `map[AgentLinker]struct{}` interface key，`wshub_agent.go:60/143` + `dashboard_agent_events.go:80` 这 3 处仍要先把 `*cli.SubagentLinker` 包装成 interface（且 `OnResolve/Query/QueryOrResolveFast` 三个方法签名直接出现在 server 里，意味着接口必须在 server 重写一遍 cli 的方法语义）。真正的解耦需要：(a) 新建 `internal/session/agentlink` 子包定义 `AgentLinker` 接口，(b) 把 `*cli.SubagentLinker` 改为实现该接口（cli 包 import agentlink），(c) `ManagedSession.SubagentLinker()` 返回 `agentlink.AgentLinker`，(d) server + `dashboard_agent_events_test.go` 全改用接口。跨 4 包 + 测试 fake 重写 — 已超 50 行 helper 上限，`dashboard_agent_events_test.go`（10 处 `func(k string) *cli.SubagentLinker` literal）需跟改。降级理由与 [BREAKING-LOCAL] 候选 R240-PERF-2 / R240-GO-1 一致：拆 RFC 走设计评审，不在 cron-fix 批量修中处理。同根因 R240-ARCH-19 已是 [REPEAT-N]，本次更新明确 REPEAT-8。
- [ ] **R239-ARCH-J — `internal/session/router_core.go:193-431` Router struct 含 40+ 字段（P2）[REFACTOR]**：已 split 但仍是 god-object。方向：拆 RouterStore / RouterPolicy / RouterLifecycle 子结构。
- [ ] **R239-ARCH-K — `internal/cli/wrapper.go:120-128,176-180` backendDisplayName / detectCLI 仍 switch 硬编码 [REFACTOR]**：本轮 fix agent 因 import cycle 跳过；新 master [R225-CR-2] 已通过 knownBackendBinaries map 部分缓解 detectCLI 路径但未解决核心 cycle。方向：先抽 backendreg 子包消除 cycle，再让 wrapper 调 backend.Get(id).DisplayName/DefaultBinary。
- [ ] **R239-ARCH-L — `internal/session/router_core.go:80` exemptKeyPrefixes 与 `internal/session/key.go:58` reservedKeyPrefixes 两个 prefix 列表（P2）[REFACTOR]**：同根概念两套。方向：单一 KeySpec 表。
- [ ] **R239-ARCH-M — `internal/server/server.go:55` Server 持具体 *cron.Scheduler 而 Hub 用 cronHubOps interface（P2）[REFACTOR]**：同包内两种粒度。方向：完成 ServerCronOps interface 收口。
- [ ] **R239-ARCH-N — `internal/session/router_core.go:778-792` NewRouter 内部直接构造 persist.NewPersister（P2）[REFACTOR]**：Router 同时承担 SessionStore + EventLog 持久化两个职责。方向：Persister 改注入式。
- [ ] **R239-ARCH-O — `internal/session/testutil.go:1-26` 非 _test.go 文件把 TestProcess + Router.InjectSession 编进生产 binary（P2）[REFACTOR]**：testhelper 缺 build-tag 隔离。方向：build tag testfakes。
- [ ] **R239-ARCH-P — `internal/cron/scheduler.go:155` cron.RunStartedEvent 与 `internal/sysession/run.go:89` DaemonRunStartedEvent 几乎同构（P2）[REFACTOR]**：Hub 各有 BroadcastCronRunStarted / BroadcastDaemonRunStarted。方向：统一 RunLifecycleEvent 抽象。
- [ ] **R239-ARCH-Q — `cmd/naozhi/main.go:817-841` server.NewWithOptions 接收 26+ 参数（P2）[REFACTOR]**：main 直接持 backend-specific（KiroSessionsDir/ClaudeDir/EventLogDir）组装。方向：抽 internal/app 或 wireup 包。
- [ ] **R239-ARCH-S — `internal/cli/protocol.go:69-141` Protocol 接口同时承载 wire-format + handshake + capability + CLI args（P2）[REFACTOR]**：协议层与传输/启动层职责混合。方向：protocol/transport/launcher 三剖。
- [ ] **R239-ARCH-T — sessions.json 用 sessions.meta.json sidecar 存 schema_version=1 但 cron_jobs.json / workspace_overrides.json / known_ids.json 无 schema_version（P2）[REFACTOR]**：migration 路径不一致。方向：统一 store.Meta envelope。
- [ ] **R239-ARCH-U — `internal/cli/backend/profile.go:114` Profile.Features map[string]bool 字符串当类型常量 + dashboard.js 硬编码字符串（P3）[REFACTOR]**：方向：定义枚举类型 + go:generate JS。
- [ ] **R239-ARCH-V — sysession.SystemSessionRouter / cron.SessionRouter / dispatch.SessionRouter / server.HubRouter / upstream.SessionRouter 5 份消费者接口（P3）[REFACTOR]**：方法集独立但全部隐式断言 *session.Router 满足。方向：合并验证测试到一处。
- [ ] **R239-ARCH-W — `internal/session/router_core.go:1198-1221` Version() 同时服务 data 与 render 两种版本（P3）[REFACTOR]**：注释自陈 R229-ARCH-20 待拆。
- [ ] **R239-ARCH-X — `internal/cli/protocol.go:158-178` Caps 4 字段中 3 字段无生产消费者（P3）[REFACTOR]**：接口提前演化反向放大破坏面。
- [ ] **R239-ARCH-Y — `internal/server/wshub.go:111` cronHubOps 与 dashboard_session.go:245 cronStubChecker / cronSessionLister 同包三个 cron consumer interface（P3）[REFACTOR]**：方向：cron.HubFacade 单一接口暴露给 server。

## Round 238 — 5-agent 并行 code review 第 48 轮（2026-05-24）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描。本轮直接修 8 处见上方 commits（dashboard t.tokens innerHTML XSS 强转 [R238-SEC-1] / dashboard bold-italic 排序契约注释 [R238-SEC-2] / cron Stop gc 等待用 NewTimer + 抽 gcWaitBudget 常量 [R238-GO-1] / cron inflight defer 先 reset 再 Store(false) 防 reset 撕裂下一 run [R238-GO-2] / cron DeleteJob 与 DeleteJobByID persist 失败路径下也清 runStore 防 runs/ 目录泄漏 [R238-GO-3] / eventlog idx Sync 仅在本次写过 idx 才执行省半数 fsync [R238-PERF-1] / sysession registry 错误消息去掉正则字符串 [R238-CR-1] / cron store.go Lstat 非 NotExist 错误升级 slog.Warn [R238-CR-2]）。下方为本轮新发现且不适合直接修的 NEEDS-DESIGN 条目（保留 v4 二级分类标签）。

### 架构（高优先）— 本轮新发现

- [ ] **R238-ARCH-2 — `executeOpt()` 327 行 12+ 步骤（P1）**：CAS gate / jitter / snapshot / resolve notify / preflight / GetOrCreate / watchdog / Send / classify / finishRun / deliverNotice / stubRefresh，控制流靠 5+ stubRefresh+return 分支组合，新增 ErrorClass/state 必须改 7 处。建议抽 ExecPipeline 接口：每步 Stage（CASGate/Jitter/Snapshot/Preflight/Spawn/Send/Finish），chain-of-responsibility 串联；finishArgs/preflightArgs 已是雏形。`[REFACTOR]`
- [ ] **R238-ARCH-3 — `runInflight` 6 个独立 atomic.Pointer 假装 lock-free（P1）**：runID/startedAt/phase/trigger/sessionID/freshSnap 6 字段独立 Store；snapshot() 6 次独立 Load 之间字段可能被 reset 撕裂——atomic 假装 lock-free 没有原子组语义。建议改 atomic.Pointer[runInflightView] 整体 swap：Store 时构造完整 view 一次 Pointer.Store，snapshot 一次 Load；分配数 6→1 且组内一致。`[BREAKING-LOCAL]`
- [ ] **R238-ARCH-4 — DeleteJob/PauseJob/ResumeJob 6 方法同构（P1）**：3 个 IM-prefix 版 + 3 个 ByID 版 ~25 行 copy-paste（lock+lookup+mutate+persist+unlock+save+side-effect），ErrPersistFailed 处理在 6 处重复。建议抽 mutateJob(lookup, mutate) 通用 helper；6 方法塌缩为 5 行 wrapper。`[SIMPLE]` 注：本轮已部分缓解（R238-GO-3 修了 runStore 路径泄漏），结构化拆分待 RFC。
- [ ] **R238-ARCH-5 — Scheduler 三 setter DI 散点（P1）**：onExecute/onRunStarted/onRunEnded 三个 atomic.Pointer 回调 + platforms/agents/agentCommands 三 map + NotifyDefault/MaxJobs/ExecTimeout 配置全堆 Scheduler；外界通过 3 个 SetOn* setter 注入。建议抽 SchedulerDeps 接口（Notifier/RunStartedListener/RunEndedListener/ExecListener）+ PlatformResolver / AgentResolver；cron 持单一 deps 字段；测试 fake deps。`[REFACTOR]`
- [ ] **R238-ARCH-7 — SessionRouter consumer-side 接口仅 3 方法但 GetOrCreate 返 *session.ManagedSession（P2）**：抽象塌陷——Scheduler 仍可调 sess.Send/InterruptViaControl/Reset 任意公开方法（cron 包 import session.AgentOpts/SessionStatus/InterruptOutcome/ManagedSession 4 处）。建议把 GetOrCreate 返回抽成 SessionHandle 接口（Send/InterruptViaControl/Reset/SessionID），ManagedSession 实现之；移除 cron 对 session.ManagedSession 直接依赖。`[REFACTOR]`
- [ ] **R238-ARCH-11 — finishArgs/preflightArgs/位置参数三种参数收口策略并存（P2）**：finishArgs (15 字段 bag) vs preflightArgs (8 字段 bag) vs recordResultP0WithSanitised 6 个位置参数。建议统一为 *runContext（持 job snapshot+runID+trigger+lg+stopCtx）；通过 method 调用（rctx.finish, rctx.preflight）；去位置参数。`[REFACTOR]`
- [ ] **R238-ARCH-12 — runStore 应独立子包（P2）**：830 行 + 锁层级 s.mu>jobLock>entry.mu 是独立子系统（per-job ring + recentCache + jobLocks sync.Map + skipAppendTrim batch + cacheHeadPush O(N) memmove + warmCache 释放-重取 + cacheTrimAfterDisk 时间源不一致）。建议拆 internal/cron/runs 子包：runs.Store 接口；scheduler.go 不再 import io/fs / path/filepath / encoding/json。`[REFACTOR]`
- [ ] **R238-ARCH-13 — Job god data struct（P2）**：~25 字段 = IM 元数据 / Schedule / Prompt / Notify 配置 / FreshContext 行为 / LastResult/LastError/LastRunAt/LastSessionID/LastErrorClass 状态 / RunCounters / entryID runtime；wire schema 同时是 in-memory 状态 → 加内部状态必动 wire schema。建议拆 Job 配置（Schedule/Prompt/WorkDir/Notify*/Backend/Title）和 JobState 状态；wire schema 只持配置。`[REFACTOR]`
- [ ] **R238-ARCH-14 — UpdateJob patch 模式 nil-vs-empty 语义不一致（P2）**：JobUpdate{*string, *bool} pointer-as-tristate；Notify 用 *bool 模拟 tri-state，但 ID 是 string 直接；mutate 决策分散在 12 个 if upd.X != nil 块。建议引入 JobPatch + Apply pattern：每个 patchable field 实现 Apply(*Job)；JobUpdate 收 []FieldPatch；新增字段不改 UpdateJob 体。`[BREAKING-LOCAL]`
- [ ] **R238-ARCH-15 — ErrorClass 字符串枚举与 sentinel error 两套并存（P2）**：10 ErrClass 常量 + 6 sentinel error（ErrJobNotFound/AlreadyPaused/...）；handler 在每个 endpoint 写 7-8 个 errors.Is case。建议统一为带 class 的 *cron.Error：errors.Is 通过 class 路由 HTTP code；移除 6 个 sentinel error。`[REFACTOR]`
- [ ] **R238-ARCH-16 — marshalJobs 全局 atomic.Pointer var（P2）**：测试 swap 用，把测试钩子塞到 production 全局变量。建议改 Scheduler.marshalFn 字段，构造时由 SchedulerConfig 注入；测试用 NewSchedulerForTest(WithMarshal(...)) helper。`[SIMPLE]`
- [ ] **R238-ARCH-17 — NextRun(*Job) 跨包接口 entryID 静默零值（P2）**：j.entryID 是 unexported runtime-only，外部反序列化的 *Job entryID 必为 0 → 静默返回 zero time，看起来像"下次运行未知"。建议 NextRun(jobID string) 内部 lookup；删除 *Job 参数版；ListAllJobsWithNextRun 已是 ID 路径。`[SIMPLE]`
- [ ] **R238-ARCH-18 — NewScheduler 构造期混 4 件事（P3）**：Debug log + cfg.applyDefaults 隐式 mutate Location + EvalSymlinks 预 resolve + validate 延后到 Start。建议 applyDefaults 已抽，再抽 validateConfig + setupLogging；ctor 只字段赋值；side-effect 操作移到 Start。`[SIMPLE]`
- [ ] **R238-ARCH-19 — testing-only 钩子混入 production var（P3）**：stopBudget/marshalJobs 是 var 因为测试要 shorten。建议抽到 unexported testHook struct + cron/internal/testing 子包；生产 stopBudget 用 const，testing 通过专用 SetTestHook 注入。`[SIMPLE]`
- [ ] **R238-ARCH-20 — abortResult.outcome 仍 leak session.InterruptOutcome（P3）**：runDeadlineWatchdog/deadlineInterrupter 微接口仅 cron 包内用，但 outcome 字段是 session.InterruptOutcome；fired bool 含糊（success path 也是 fired=false）。建议 abortResult 改 InterruptResult enum；cron 不再 import session.InterruptOutcome；deadlineInterrupter 改名 RunInterrupter。`[BREAKING-LOCAL]`

### Go 正确性 — 本轮新发现

- [ ] **R238-GO-4 — `Scheduler.sendCtx` 派生自 context.Background 不受 stopCtx 控制（P1）**：Router/Session 层在 Scheduler.Stop 之后被 Shutdown，而 sendCtx 的 Send 仍然向已销毁的 session 写，构成 use-after-free 类竞态。建议 sendCtx 父 context 改为 s.stopCtx，对 context.Canceled 分支走已有的 skipPersist 路径。`[BREAKING-LOCAL]`
- [ ] **R238-GO-5 — `sysession.runner.limitedWriter.Write` 内层错误后无停机标志（P2）**：内层 w.Write(chunk) 出错后静默丢弃错误并继续，后续写入仍调用内层 writer，lw.n 停止增长，cap 永不更新，Writer 进入无效写入循环。建议增加 `failed bool` 字段，内层出错后置 true，后续写入直接 return。`[SIMPLE]`
- [ ] **R238-GO-7 — `sysession.manager.Stop` wrapper goroutine 让 goleak 误报（P2）**：osExit 被 mock 为 panic 时（测试场景），panic-recover 后 wrapper goroutine 继续存活。建议在测试文档/注释里说明需要 goleak.IgnoreTopFunction，或提供 ctx-aware WaitGroup drain 工具。`[SIMPLE]`
- [ ] **R238-GO-8 — `runstore.diskListNewestFirst` before 过滤无 mtime 预筛（P2）**：先 readRun（2 次 syscall）再判断 StartedAt < before；可能读取远超 limit 个文件后全部丢弃。建议利用已有的 mtime 做粗筛（mtime >= before 的条目可跳过 readRun）。`[REFACTOR]`
- [ ] **R238-GO-9 — `executeOpt` TriggerNow 路径 panic 直传（P3）**：TriggerNow 路径不走 robfig chain Recover，panic 时 inflight defer 仍执行但 panic 向上传播到 TriggerNow goroutine 并 crash。建议在 executeIfNotDeletedOrPaused 或 executeOpt 入口增加 recover，将 panic 转为 finishRun + slog.Error。`[SIMPLE]`
- [ ] **R238-GO-10 — `SchedulerConfig.ExecTimeout` godoc 未提及最坏 wall-clock（P3）**：实际最坏是 ~2×ExecTimeout（R230B-GO-1），运维配置 systemd TimeoutStopSec 时会设置错误。建议 godoc 注明 "worst-case wall-clock per run is ~2×ExecTimeout"。`[SIMPLE]`
- [ ] **R238-GO-11 — `runstore.warmCache` 并发 LoadOrStore 多余分配（P3）**：高并发重启场景下加重 GC。建议先 Load，只在 miss 时再 LoadOrStore；或注释说明已知权衡。`[SIMPLE]`
- [ ] **R238-GO-12 — `sysession.runner` cmd.Run 失败时 stderr 未追加到 error（P3）**：dashboard 的 circuit breaker last_error 字段无法诊断原因。建议将 sanitized stderr 前 N 字节追加到返回 error 的消息里。`[SIMPLE]`
- [ ] **R238-GO-13 — `applyDefaults` 文档"Idempotent"但内含 slog.Warn 副作用（P3）**：多次调用会重复打印警告。建议将 slog.Warn 移至 NewScheduler，保持 applyDefaults 为纯函数。`[SIMPLE]`
- [ ] **R238-GO-14 — `runstore.skipAppendTrim` 重置不在成功后（P3）**：mu 持有期间 appendsSinceTrim 重置为 0，即使后续 trimJobLocked 失败（磁盘错误），重置已完成，下次需再等 appendTrimBatch 次才重试 trim。建议将重置移到 trimJobLocked 成功返回后。`[SIMPLE]`
- [ ] **R238-GO-15 — `auto_titler.buildExcerptFromHistory` 无总长度上限（P3）**：数千轮对话会在 strings.Builder 中积累大量内存，OOM 风险。建议增加 softCap（如 1 MB）或在配置项增加 max_excerpt_bytes。`[SIMPLE]`
- [ ] **R238-GO-16 — `auto_titler.highwater` map 早停下无限增长（P3）**：earlyStop=true 时跳过 prune，若 earlyStop 持续为 true（session 数超过 batchPerTick×4），highwater map 会无限增长。建议对 highwater map size 设置上限（如 maxJobsHardCap × 2），超出时强制 prune 最旧条目。`[SIMPLE]`

### 安全 — 本轮新发现

- [ ] **R238-SEC-4 — selfupdate 无加密签名验证（P2）**：仅有同 release 拉的 SHA-256；GitHub token 泄漏后攻击者可同时替换 binary + checksums.txt。建议引入 GPG/cosign 签名 + 公钥固化或引入 Sigstore 透明日志。`[REFACTOR]`
- [ ] **R238-SEC-5 — dashboard.go pooled encoder `SetEscapeHTML(false)` 无静态约束（P2）**：依赖运行期 "no innerHTML" 契约。建议把不 escape 的 encoder 限定在专用 marshal 站点（marshalForJSONLDownload 等），主路径用 escape=true 的 encoder。`[REFACTOR]`
- [ ] **R238-SEC-6 — `dashboard_cron_transcript.go:239` ClaudeProjectSlug HasPrefix 大小写敏感（P2）**：macOS HFS+ 可能 case-insensitive，partial bypass 可能。建议统一对路径做 ToLower 或显式断言文件系统大小写敏感。`[SIMPLE]`
- [ ] **R238-SEC-7 — `runstore.go:584` Lstat + 单独 ReadFile TOCTOU（P2）**：应先打开 fd 再 Fstat。建议改 OpenFile + Fstat 一致路径。`[SIMPLE]`
- [ ] **R238-SEC-8 — `cron/store.go:50` Lstat/open TOCTOU 仍残留 + corrupt-rename 无重检查（P2）**：本轮 R238-CR-2 已加 Warn 但 TOCTOU 主路径未变。建议改为 OpenFile(O_RDONLY|O_NOFOLLOW) 一步到位。`[SIMPLE]`
- [ ] **R238-SEC-9 — `dispatch/commands.go:412` /cron add 回显 Unicode homoglyphs 通过过滤可冒充系统消息（P2）**：建议加 Unicode confusable 检测（如 unicode.SimpleFold + ranges）。`[SIMPLE]`
- [ ] **R238-SEC-10 — `runs/` 父目录可能 0o755 泄漏 cron job 历史存在性（P2）**：runs/ root 0o700 但父 data dir 可能 0o755。建议父目录也 0o700 或改 ACL。`[SIMPLE]`
- [ ] **R238-SEC-11 — auth cookie / notify_chat_id 等 Unicode confusables（P3）**：dashboard 显示存在欺骗风险。建议显示前做 confusable 检测或 punycode 化。`[SIMPLE]`
- [ ] **R238-SEC-12 — `storeDirOnce` 0o700 在第一次 save 时才 fire（P3）**：进程启动到第一次 save 之间存在权限窗口。建议在 NewScheduler 启动时立即 ensure。`[SIMPLE]`
- [ ] **R238-SEC-13 — `ansiEscRe` 正则未覆盖 OSC/DCS ANSI 序列（P3）**：tool 输出中 OSC/DCS 序列不被去除。建议扩展正则覆盖 OSC (ESC ]) 与 DCS (ESC P) 直到 ST/BEL。`[SIMPLE]`
- [ ] **R238-SEC-14 — `handleUpdate` 部分 notify target（platform 设了但 chat_id 没设）静默 fall through（P3）**：建议显式 422 错误。`[SIMPLE]`
- [ ] **R238-SEC-15 — Per-IP rate limiter 在 trustedProxy=true 下可被 XFF spoof（P3）**：建议在 doctor 提示 trustedProxy + XFF 配合的注意事项。`[SIMPLE]`
- [ ] **R238-SEC-16 — `diskListNewestFirst` 依赖 mtime 在 overlayfs/FUSE 不可靠（P3）**：建议在 doctor 检测文件系统类型并提示。`[SIMPLE]`
- [ ] **R238-SEC-17 — selfupdate tag 未先做 semver 校验就嵌入 URL（P3）**：建议加 semver regex 前置校验。`[SIMPLE]`

### 性能 — 本轮新发现

- [ ] **R238-PERF-2 — `executeOpt` slog.With 每次 cron 执行都 alloc 4-attr Logger（P1）**：即使 effective log level 跳过下游也无法避免。建议构造 lazy logger：在 jitter 后、snapshotJob 前一次性构造，或改为按需附加 attrs。`[SIMPLE]`
- [ ] **R238-PERF-9 — `ListAllJobsWithNextRun` 500 jobs 顺序调 cron.Entry（P3）**：500 次 robfig 内部 lock。建议一次 cron.Entries() 拿全 + 构造 map[EntryID]time.Time。`[SIMPLE]`
- [ ] **R238-PERF-11 — `cacheTrimAfterDisk` 无 fast-path 即使无操作也 alloc（P3）**：建议加同 trimJobLocked 已有的 fast-path 守卫：`if len(entry.runs) <= s.keepCount && oldest entry inside window`，return early without alloc。`[SIMPLE]`
- [ ] **R238-PERF-12 — `session.GetSession` defer 在 subscribe hot path（P3）**：subscribe 周期 O(subs/s) 路径。建议替换 defer 为显式 RUnlock 在每个 return 前。`[SIMPLE]`
- [ ] **R238-PERF-14 — `cli/process_event_format.FormatToolInput` Grep 分支两次 string concat（P3）**：result + " " + Truncate + " in " + shortPath 两次中间分配。建议两 part 同时存在时用 strings.Builder + Grow。`[SIMPLE]`

### 代码质量 — 本轮新发现

- [ ] **R238-CR-3 — `server.go:643` 第一次 SetOnKeyRetired 被后来的覆盖（P2）**：注释承认了这个窗口但没有消除它，WarmHistoryCache 是后台 goroutine，理论上可在窗口内触发 Reset，此时 InvalidateHistoryCache 不会被调用。建议删除第一次 SetOnKeyRetired，仅保留 sessionH 初始化后的完整 fanout。`[SIMPLE]`
- [ ] **R238-CR-5 — `dispatch.sendTodoMessage` Reply 失败仅 slog.Debug（P3）**：R236-GO-1 改为 context.Background() 后，连 t.ctx 取消也不能影响此函数，失败完全无声。建议改 slog.Warn 与同文件 ask_question fallback 对齐。`[SIMPLE]`
- [ ] **R238-CR-6 — `discovery.scanner.findJSONLPath` 同一帧调两次（P3）**：line 372 存在性检查 + line 380 jsonlMtime 内部再调（含 os.Stat）。建议把 jsonlPath 向下传给 jsonlMtime 或合并为 jsonlPathAndMtime。`[REFACTOR]`
- [ ] **R238-CR-7 — `discovery.scanner.noJSONLGrace = 5s` 选值依据未说明（P3）**：注释说 "1-2s before they flush" 但常量是 5s。建议补注释 "5s covers observed worst-case CLI first-flush latency with 2x safety margin"。`[SIMPLE]`
- [ ] **R238-CR-8 — `server.go:725-731` 匿名块 {} 包裹 SetOnKeyRetired fanout 增加视觉噪音（P3）**：Go 中匿名块通常用于 switch 分支 shadowing，此处对逃逸分析无实际作用。建议去掉 {} 直接赋值，或提取为 wireOnKeyRetired(router, ...) 函数。`[REFACTOR]`

---

## Round 237 — 5-agent 并行 code review 第 47 轮（2026-05-24，与 Round 236 并发批）NEEDS-DESIGN

> 与同日 #290 Round 236 并发触发的另一批 5-reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描。本批直接修 8 处见上方 commits（commit anchors 仍写 [R236-*]，因落地时 #290 尚未合并；文档归类用 R237 与 #290 隔开）。下方为本轮新发现且不适合直接修的条目。

### 架构（高优先）— 本轮新发现

- [ ] **R237-ARCH-1 — `cli.Wrapper.ShimManager` 是导出可变 `*shim.Manager`，protocol/transport 未拆分（P1）**：与 R235-ARCH-4 同根因，本轮 architect 再次点出。建议引入 `cli.Transport` 接口（Spawn / Reconnect），shim 是其一个实现；cli 包不再 import `internal/shim`。Breaking: 是。归 R235-ARCH-4 主条目跟踪。
- [ ] **R237-ARCH-2 — `internal/server.Server` god object 持 19 个 handler/dep 字段 + `registerDashboard` 60+ HandleFunc（P1）**：每加 handler 改两处。建议拆 `Lifecycle` / `Routes` / `Container` 三层；或采用 module 模式（auth.Module() / cron.Module() …）。Breaking: 内部组装大改，外部 API 不变。
- [ ] **R237-ARCH-3 — `session.processIface` 32 方法 god-interface（P1）**：`*cli.Process` 是唯一生产实现，接口因 testutil.TestProcess 撑大。建议拆 ProcessSender / ProcessLifecycle / EventSource 三 facet。Breaking: 是。归 R215-ARCH-P1-3 / R219-ARCH-7 / R224-ARCH-5 主条目。
- [ ] **R237-ARCH-4 — discovery vs sysession 职责重叠（P1）**：项目 brief 与 R234-ARCH-3 已点出。建议设计单一 session catalog 抽象：discovery 只产出 DiscoveredSession（外部 CLI 进程）；sysession 消费 router + catalog。Breaking: 是（影响 RFC 接口）。
- [ ] **R237-ARCH-5 — metrics 反向依赖（P1）**：`internal/metrics` 是叶子包但被 7+ 核心包直接 import 写 expvar。建议改 Observer 接口注入：cli.Wrapper / shim.Manager / cron.Scheduler 收 Observer 字段，main.go 装配。Breaking: 业务包公共构造函数加参数。
- [ ] **R237-ARCH-6 — sysession.Manager.Stop 超时调 osExit(2) 让 systemd 重启进程（P1）**：单个 daemon 卡死 → 整个 naozhi 不可用。建议替换为：超时只 detach 该 daemon record，不再 broadcast；Stop 返回让上层决策。需 RFC 评估错误传播策略。
- [ ] **R237-ARCH-8 — main.go 622 行 wiring + 业务逻辑（P1）**：subcommand dispatch / settings.json filtering / Bedrock env / shim manager / wrapper × N / router / sysession / 5 platform / cron / upstream / 4 webhook / shutdown 顺序全在 main()。建议抽 `internal/bootstrap` 包：`func Run(ctx, cfg) error`。Breaking: 否（内部重组）。
- [ ] **R237-ARCH-9 — `Router` struct 字段 40+（P2）**：core/lifecycle/cleanup/discovery/shim/backend 5 文件共享同一大 struct。建议拆 SessionStore + KnownIDs + WorkspaceOverrides + BackendOverrides + AutoChainResolver。Breaking: 公共方法签名不变但锁顺序需重证。
- [ ] **R237-ARCH-10 — server.Hub 持 22+ 字段（P2）**：WS 连接管理 + 业务路由 + 子 agent 跟踪 + cron 集成都在一起。建议拆 ConnPool / Broadcaster / SendPath / AgentLinker。
- [ ] **R237-ARCH-11 — Shutdown 顺序 3 处分散表达（P2）**：main.go runShutdown / server.go ctx.Done goroutine / router shutdownOnce 三处都说"必须 X 在 Y 之前"。建议 `internal/lifecycle`：Lifecycle.Register(component)，按注册逆序关闭。归 R234-ARCH-6 主条目。
- [ ] **R237-ARCH-12 — `session.KeyResolver` 在 4 处共享但无 singleton 协调（P2）**：main.go upstream + buildServer + Dispatcher.cfg.Resolver + Hub.opts.Resolver 各持一个，agents 表配置变更不同步。建议 `*session.Router.Resolver()` 方法或 main.go 注入 singleton。
- [ ] **R237-ARCH-13 — eventlog 跨包语义同名但行为分裂（P2）**：cli/eventlog.go (in-mem) vs eventlog/persist (disk) vs history/naozhilog (replay) 三者命名相同。建议拆 `internal/eventlog/{ring,persist,replay}`。
- [ ] **R237-ARCH-14 — server.New() vs NewWithOptions() 双构造函数（P2）**：~20 个 test call sites 暂未迁移，留两个构造函数让 API 稳定性退化。建议一次性改完所有 test 站点或把 New 改为 internal/test-only 别名。Breaking: 删 New 是 breaking。

### Go 正确性 / 风格 — 本轮新发现

- [ ] **R237-GO-2 — `cli.Process.SessionID` 和 `State` 字段导出但访问需走 getter（P2）**：注释要求用 `GetSessionID()` / `GetState()` 但字段本身导出，外部包可绕过锁。建议改未导出 + 强制 method 访问。Breaking: 是（包外直接读写需更新）。
- [x] **R237-GO-3 — `cli.Process.Send()` 160 行函数（P2）**：state 管理 / 事件日志 / stale drain / watchdog ticker 多关注点混合。建议提取 `handleWatchdog(now, lastOutput, turnStart, noOutputDur, totalDur) error`。Non-breaking。 — F1 解决 2026-05-25 (commit fb482871)：抽 Process.handleWatchdogTick(now, lastOutput, turnStart, turnStartMS, noOutputDur, totalDur) (*SendResult, error) 三态返回，主 select 退化为 sr/err 透传 + Reset 一行；Kill/setDeathReason/findResultSince 调用语义不变。
- [ ] **R237-GO-4 — `dispatch.sendAndReply()` 223 行（P2）**：takeover / GetOrCreate / 错误映射 / tracker / result / image 多职责。建议提取 `handleGetOrCreateError` 等子函数。Non-breaking。
- [ ] **R237-GO-5 — `cli.dispatchProtocolEvent` 241 行（P2）**：metadata / passthrough hooks / linker / EventLog / reconnect / killCh 6 关注点。建议提取 `notifyLinker(ev, nowMS)` 与 `deliverEvent(ev, now, log) bool`。Non-breaking。
- [ ] **R237-GO-6 — `cli.shimLineReader.ReadLine` 无 ctx 检查（P2）**：shim 连续发非 stdout/cli_exited 消息时无法在 proto.Init 超时场景提前退出。建议加 `ctx context.Context` 字段。Breaking: 是（接口变更）。
- [ ] **R237-GO-7 — `cmd/naozhi/main.go:96` `readJSONWithRetry` 阻塞 main goroutine（P2）**：`time.Sleep(sleep)` 重试不响应 ctx 取消。建议加 ctx 参数 + select 替换 Sleep。Breaking: 是（调用点更新）。
- [ ] **R237-GO-10 — `process.go:411` `setDeathReason` upgrade-path 死代码（P3）**：注释 "not taken today" 自陈死代码。建议删除 upgrade-path（425-428 行）。Non-breaking。

### 安全 — 本轮新发现

- [ ] **R237-SEC-4 — `expandEnvVars` 环境变量展开后内容注入 YAML（P2）**：env 值含换行符 + 新 key 可注入任意配置。建议展开前对每个变量值做 YAML 字符串转义或单引号包裹。Breaking: 仅对含 YAML 特殊字符的现有 env 值有影响。
- [ ] **R237-SEC-5 — `publicTmpProject` 允许已认证用户读 /tmp 全量（P2）**：注释自陈"any authenticated user can read non-credential files anywhere under /tmp"。建议加 config 显式开关（默认关闭）或限制路径到 attachment 子目录。Breaking: 加配置项。
- [ ] **R237-SEC-6 — `selfupdate` exec.Command("systemctl") 用 PATH 查找（P2）**：PATH 污染场景下可能执行恶意替代。建议改绝对路径 `/usr/bin/systemctl` 或 `exec.LookPath` 缓存。Non-breaking。
- [ ] **R237-SEC-7 — auth cookie 无 Partitioned (CHIPS) 属性（P3）**：未来兼容性加固。Non-breaking。
- [ ] **R237-SEC-8 — `config.go` fd-stat 权限检查与 Lstat 不一致（P3）**：Lstat 拒绝 0o077，fd-stat 仅拒绝 0o044，对 0o650 等罕见权限存在 TOCTOU 缝隙。建议 fd-stat 同样用 0o077。Non-breaking。
- [ ] **R237-SEC-9 — `allowed_root` 为空时仅 Warn 不拒绝启动（P3）**：已认证用户可设置 cron WorkDir 为 /etc。建议升级为 `slog.Error` 或 `naozhi doctor` 强制检查。Non-breaking。

### 性能 — 本轮新发现

- [ ] **R237-PERF-5 — `cron.Scheduler.addJobAcquiringLock` per-chat 限制全量 O(N) 扫描（P1）**：持 s.mu.Lock() 期间扫 maxJobs=500 个 *Job 阻塞 TriggerNow / emitRunStarted。建议维护 `chatJobCount map[chatKey]int` 同步更新。Non-breaking。
- [ ] **R237-PERF-6 — `session.InjectHistory` 在 historyMu.Lock 持锁期间做 trim+make+copy（P1）**：启动历史回放路径 10 goroutine 并发批量 lock 与 dashboard 1Hz RLock 争用。建议 trim 移出锁外或用 sync.Pool。Non-breaking。
- [ ] **R237-PERF-7 — `discovery.scanner.Scan` 双 syscall 模式（P2）**：DirEntry.Info() + os.ReadFile() 两次 stat。建议合并为单次 OpenFile + io.LimitReader。Non-breaking。
- [ ] **R237-PERF-8 — `runstore.diskListNewestFirst` 无 mtime 预过滤（P2）**：分页查询时全量解析 JSON 再丢弃靠前的条目。建议 before 非零时先 mtime 预过滤。Non-breaking。
- [ ] **R237-PERF-9 — `Router.knownIDsOrder` 无上限（P2）**：无 cap 持续 append，过期 ID 占内存。建议设 maxKnownIDsOrder = 10000 + FIFO 截断。Non-breaking。
- [~] **R237-PERF-11 — `eventlog/persist/idx.AppendBatch` 每次分配新 buf（P2）**：默认 200ms 间隔每批 28-896 字节短命对象给 GC 增压。建议 buf 改 IdxWriter 字段复用或栈分配。Non-breaking。
- [ ] **R237-PERF-12 — `session.EventEntriesSince` dead-session 全量 sort（P2）**：1Hz × N tabs × M dead sessions。建议 persistedHistory 维护按 Time 排序的不变式 + 二分查找。Non-breaking。
- [ ] **R237-PERF-13 — discovery extractText 单 block 路径多余 alloc（P3）**：blocks 长度 1 时仍走 strings.Join。建议早返。Non-breaking。
- [ ] **R237-PERF-14 — runstore.skipAppendTrim 高频 sync.Map lookup + entry.mu lock（P3）**：建议改 jobAppendCount sync.Map[*atomic.Int32] 无锁。Non-breaking。
- [ ] **R237-PERF-15 — session.storeMetaPath 重复 filepath 计算（P3）**：建议 Router 字段缓存。Non-breaking。
- [ ] **R237-PERF-16 — metrics labelKey Pool overhead 单 label 场景（P3）**：建议 len==1 时直接返回 clipLabelSegment。Non-breaking。

### 代码质量 — 本轮新发现

- [ ] **R237-CR-3 — `shim.handleClient` 327 行 / `shim.Run` 286 行 / `upstream.handleRequest` 525 行 14-case switch（P1）**：超长函数。建议 handleClient 拆 authenticateClient/replayHistory/runCommandLoop；Run 提取 waitAfterExit；handleRequest 按 case 拆 handle&lt;Method&gt;。Non-breaking。
- [ ] **R237-CR-4 — package-level mutable var `handleConnDrainBudget` / `circuitBreakerThreshold` / `circuitBreakerBackoff` / `maxWriteLineBytes` 测试直接赋值（P1）**：`-race` 下 data race。建议 atomic.Int64/Value 或通过 ConnectorConfig 传入。Breaking: 否（atomic）/ 是（config）。
- [ ] **R237-CR-5 — shim cli_exited / watchdog.Fired 两段 select 结构完全相同（P2）**：60s exitTimer + 嵌套 reconnectTimer 逻辑逐字相同。建议提取 `waitForReattach(acceptCh, idleTimeout, reason)`。Non-breaking。
- [ ] **R237-CR-6 — upstream send/takeover/close_discovered workspace 路径验证三处重复（P2）**：EvalSymlinks + Clean + IsAbs + HasPrefix(allowedRoot)。建议 `sanitizeWorkspacePath(raw, defaultWorkspace)` 共用。Non-breaking。
- [ ] **R237-CR-8 — shim.shimLogFilePtr package-level atomic（P2）**：同进程多 shim（测试）互覆。建议改 Run 局部变量 + closure。Non-breaking。
- [ ] **R237-CR-9 — project.UnbindAllChat slice 就地复用底层数组（P2）**：单次调用无害但模式不一致。建议改 `make([]ChatBinding, 0, len(...))` 或 slices.DeleteFunc。Non-breaking。
- [ ] **R237-CR-10 — TODO `node/protocol.go:33` R214-CODE-6 状态需确认（P2）**：核实是否仍 open；若已归档则删除注释。Non-breaking。
- [ ] **R237-CR-11 — `shim.StartShimWithBackend` 208 行（P2）**：key 校验/slot 预留/argv 构造/socket 预检/进程启动/ready 解析/token 解码/连接/cgroup/map 更新 多阶段。建议提取 `buildShimArgs` + `waitForShimReady`。Non-breaking。
- [ ] **R237-CR-12 — `validateKeyForShim` 与 `session.ValidateSessionKey` 重复且靠注释同步（P3）**：建议提取到 session 包共用函数 + contract test。Non-breaking。
- [ ] **R237-CR-13 — `RingBuffer.defaultRingMaxLines` 与 ManagerConfig.BufferSize 默认值靠注释同步（P3）**：建议 manager.go 直接引用 buffer.go 常量。Non-breaking。
- [ ] **R237-CR-14 — `Project.snapshotLight` ChatBindings=nil 无注释（P3）**：建议 godoc 明确"返回 Project 不含 ChatBindings"。Non-breaking。
- [ ] **R237-CR-15 — `osutil.SyncDir` 静默吞 fs.ErrPermission（P3）**：权限错误是真实配置问题。建议返回 err 或至少 slog.Debug。Non-breaking（行为变化）。
- [ ] **R237-CR-16 — `connector_conn.go` reqSem 容量 16 裸字面量（P3）**：与 maxInflightClients 相同字面但无关联。建议 `const connectorReqSemCapacity = 16`。Non-breaking。
- [ ] **R237-CR-17 — `cli.editLoop` rateTimer 时序边界注释（P3）**：当前 1s rate-limit 实际正确（Reset(1s) 后必须读 timer.C），但代码可读性可改进。注释加"EditMessage 耗时不抵消 rate window"。Non-breaking。

## Round 236 — 5-agent 并行 code review 第 46 轮（2026-05-24）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描，共 88 项发现。本轮直接修 8 处（cron Stop 等 GC goroutine 退出 [R236-GO-01] / cron addJobAcquiringLock persist 失败回滚 entry [R236-GO-10] / cron UpdateJob 注册失败回滚 schedule 字段 [R236-QA-08] / cron loadJobs Lstat 拒绝符号链接 CWE-59 [R236-SEC-01] / cron trimJobLocked sort 与 diskListNewestFirst 对齐 [R236-QA-01] / cron validateSchedule 拒绝 interval<=0 [R236-QA-07] / server send.go 删除入口冗余 BroadcastSessionsUpdate 全量扇出 [R236-PERF-01]，外加 R236-SEC-12 在 c337d68 已修复故关闭）。下方为本轮新发现且不适合直接修的 NEEDS-DESIGN 条目。

### 安全（剩余）

- [ ] **R236-SEC-02 — 主 dashboard CSP 仍含 `'unsafe-inline'`（P1）**: `internal/server/dashboard.go:486` 主页 CSP `script-src 'self' 'unsafe-inline'`，登录页（dashboard_auth.go:236）已用 SHA-256 hash 白名单消除 unsafe-inline。任何 DOM-XSS 入口都可升级为完整会话劫持。方案：内联脚本改 nonce/hash CSP 或外联到 dashboard.js。Breaking：否，但需逐脚本测试。
- [x] **R236-SEC-03 — requestHost 与 clientIP 信任点方向相反（P1）**: `internal/server/csrf.go:29-30` 取 X-Forwarded-Host **第一个**值，`internal/netutil/clientip.go:38` 取 X-Forwarded-For **最后一个**值。trustedProxy=true 部署下，攻击者可用可控 X-Forwarded-Host 绕过 CSRF Origin 比对。方案：requestHost 也改取最后一个（与 ALB/CloudFront 追加语义一致）。Breaking：否（仅 trustedProxy=true）。 *(已实施：requestHost 取 X-Forwarded-Host 最后一个值，与 netutil.ClientIP last-XFF 语义一致；攻击者 prepend 的 attacker.com 不再绕过 CSRF Origin 比对。csrf_test.go 加 trusted_proxy_attacker_prepended 锚定攻击场景。)*
- [ ] **R236-SEC-04 — diskListNewestFirst 仅靠 `e.Type()` 检测 symlink（P2）**: 某些文件系统（部分 tmpfs / NFS）`d_type==DT_UNKNOWN` 时 `e.Type()` 返 0 而非 ModeSymlink，trimJobLocked 通过文件名直 Remove 可能删 symlink 目标。方案：对 `.json` 条目额外 `e.Info()` 或 `os.Lstat` 双重确认。Breaking：否。
- [ ] **R236-SEC-06 — nz_anon cookie WS 升级时未签名验证（P2）**: `internal/server/wshub.go:420-435` dashToken=="" 模式下从 nz_anon cookie 读取 uploadOwner，但 cookie 值用户可控。攻击者可伪造 nz_anon 串获取共享 NAT 下其他用户上传文件。方案：HMAC 签名（参照 cookieMAC）或在注释明确单用户假设。Breaking：否。
- [ ] **R236-SEC-08 — handleList 1Hz 全量返回完整 prompt（P2）**: `internal/server/dashboard_cron.go:446-454` 50 jobs × 8 KiB = 每秒 400 KiB，token 泄漏后单次 GET 即拉走所有 prompt。方案：列表截断 256 字节，详情走 GET /api/cron/{id}；前端搜索改服务端。Breaking：是（前端需调整）。
- [ ] **R236-SEC-14 — 主 dashboard CSP `img-src` 含 `data:`（P3）**: `internal/server/dashboard.go:486` 在 R236-SEC-02 unsafe-inline 存在前提下，data: URI 可被用作 XSS 数据外泄通道。方案：与 R236-SEC-02 一并升级 CSP 时收紧 img-src 为 `'self' blob:`。Breaking：否（需确认前端无合法 data: 图片）。
- [ ] **R236-SEC-15 — notifyTarget chunk × retry 复合超时可能超 30s 预算（P3）**: `internal/cron/scheduler.go:2845-2879` ReplyWithRetry 内部超时未必使用 replyCtx 剩余时间。方案：限制最大 chunk 数（如 5）+ 确认 ReplyWithRetry 使用传入 ctx。Breaking：否。

### Go 正确性（剩余）

- [ ] **R236-GO-04 — DeleteJobByID persist 失败但 in-memory 已删 → 复用 ID 时继承旧 runs 目录（P2）**: `internal/cron/scheduler.go:1202-1226`。方案：runStore.DeleteJob 移到 ErrPersistFailed 检查前；或 trimAll 加 orphan 检测。Breaking：否。
- [ ] **R236-GO-05 — Stop deadline 命中后 triggerWG.Wait goroutine 永久泄漏（P2）**: `internal/cron/scheduler.go:971-980` 注释自陈"intentional orphan"。方案：scheduler 加 stopCh + TriggerNow goroutine select stopCh 短路。Breaking：否。
- [ ] **R236-GO-07 — sendCtx 使用 context.Background 让 Send 不被 stopCtx 取消（P2）**: `internal/cron/scheduler.go:2259` Stop 返回后 cron job Send 可继续 jobTimeout（默认 5min），可能持已被 Router.Shutdown 回收的 session。方案：sendCtx 父 ctx 改 stopCtx + 增加 execWG.Wait。Breaking：否。
- [ ] **R236-GO-08 — Append jobLock 持锁期间 trimJobLocked 做磁盘 I/O 阻塞并发 Recent（P2）**: `internal/cron/runstore.go:228-235` 慢盘上 200ms+ 阻塞。方案：trimJobLocked 拆两阶段（锁内决名单，锁外 Remove）。Breaking：否。
- [ ] **R236-GO-09 — runDeadlineWatchdog InterruptViaControl 阻塞导致 finishRun 永不调用（P2）**: `internal/cron/scheduler.go:2003-2013` 后续触发因 inflight.running=true 全跳过。方案：watchdog 内 InterruptViaControl 加 3s 超时。Breaking：否。

### 性能（剩余）

- [~] **R236-PERF-04 — eventlog Append 单条路径每次分配 `[]EventEntry{e}`（P2）**: `internal/cli/eventlog.go:790-888`，PersistSink 签名要求 slice。同根因 R230C-PERF-2 主条目跟踪。Breaking：是（接口签名变化）。 — NEEDS-DESIGN 状态收敛 2026-05-24（cron-fix-F1 复核）：与 R219-PERF-4 / R222-PERF-8 / R228-PERF-7 / R227-PERF-9 / R240-PERF-7 同根因。R230-PERF-1 sink-nil 早返回已覆盖 production hot path（无 sink 时零 alloc）；sink-attached 路径的 slice literal 结构性必需（contract: sink 可保留 slice 跨 return）。godoc 锚点已在 eventlog.go:909-917；本批仅文档同步关闭主条目跟踪。
- [ ] **R236-PERF-06 — handleSubscribe 每次扫 maxWSConns 个 client 计 per-key 订阅数（P2）**: `internal/server/wshub.go:621-656` 在 h.mu.Lock 下，subscribe 阻塞所有广播 RLock。方案：subscriberCounts map 维护 O(1) 计数。同根因 R230C-PERF-4，但可重新评估。Breaking：否。
- [ ] **R236-PERF-07 — diskListNewestFirst pagination 路径全量 ReadFile bypass 缓存（P2）**: `internal/cron/runstore.go:468-545` 200 runs × ReadFile = 6.4MB syscall。方案：mtime 排序后 binary search before cutoff，跳过不需要的 ReadFile。Breaking：否。
- [ ] **R236-PERF-08 — handleList 50 jobs 串行调 RecentRuns 各持 entry.mu（P2）**: `internal/server/dashboard_cron.go:510`。方案：BatchRecentRuns(jobIDs, n) 单次遍历或 bounded goroutine 并发。Breaking：否（新 API）。
- [ ] **R236-PERF-09 — warmCache jobLock 持锁期间 200×ReadFile + Unmarshal（P2）**: `internal/cron/runstore.go:376-392` 多 job warm 起手时 50 个 I/O 重 goroutine 占用线程。方案：先 meta scan 列文件名（短锁）+ 锁外 ReadFile + 二次锁写 entry.runs。Breaking：否。
- [ ] **R236-PERF-12 — trimJobLocked 重复 ReadDir+Sort 与 diskListNewestFirst 不复用（P2）**: `internal/cron/runstore.go:628-707`。方案：cache.warm 时直接用 cache 算"是否需要 trim"，绝大多数情况跳过 ReadDir。Breaking：否。
- [ ] **R236-PERF-13 — Snapshot 含 SetModel 写副作用（P2）**: `internal/session/managed.go:1151-1275` 每次 dashboard 1Hz × N tabs 触发 atomic store。方案：model 同步移到 readLoop / Send 结束，Snapshot 改纯读。Breaking：否。

### 代码质量 / 架构（剩余，挑选高价值）

- [ ] **R236-QA-03 — UpdateJob 在 s.mu.Lock 下调 cron.Add/Remove 可能锁顺序倒置（P1）**: `internal/cron/scheduler.go:1383-1397` 与 ListAllJobsWithNextRun 注释揭示的锁顺序冲突。pauseJobLocked/resumeJobLocked 同样存在。方案：研究 robfig/cron v3 锁语义；不安全则参照 ListAllJobsWithNextRun 模式释锁后操作 cron。Breaking：否。
- [ ] **R236-QA-13 — readJSONWithRetry 不区分 file-missing vs JSON-invalid（P2）**: `cmd/naozhi/main.go:97-112` 损坏 settings.json 仅 Warn。方案：errors.Is(fs.ErrNotExist) 走 Warn 其余走 Error 提示运维。Breaking：否。
- [ ] **R236-QA-20 — isNaozhiCallbackHook 用 strings.Contains "127." 误报合法 URL（P3）**: `cmd/naozhi/main.go:364-384`。方案：正则匹配 IP 前缀或确认 host:port token 格式。Breaking：否。
- [ ] **R236-ARCH-06 — dispatch.Dispatcher 仍持具体 *cron.Scheduler 而非 interface（P1, is_localized=true）**: `internal/dispatch/dispatch.go:67-71` 与 R235-ARCH-3 同根因，本条提供具体方案：`internal/dispatch/cron_consumer.go` 定义 ~8 method CronScheduler interface + contract test。Breaking：否（接口断言迁移）。
- [ ] **R236-ARCH-07 — cron.scheduler 直 import platform 绕过 dispatch 中文化/replyError 计数（P1）**: `internal/cron/scheduler.go:25` 第三条出口管线。本条提供窄方案：dispatch.Notifier interface 注入到 scheduler 替代 platforms map（比 SendOrchestrator RFC 改动面小 90%）。同根因 R230C-ARCH-3 主条目跟踪。


## Round 235 — 5-agent 并行 code review 第 45 轮（2026-05-23）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描，本轮直接修 18 处（见顶部摘要 R235-CR-1/2/5/6/10/11/12/13、R235-SEC-1/3/4/5/6/7/8/9、R235-GO-1/5/7、R235-PERF-17/20）。下方为本轮新发现且不适合直接修的条目（破坏兼容 / 跨包重构 / 需 RFC / 方案不唯一）。

### 架构（高优先）— 本轮新发现

- [ ] **R235-ARCH-1 — `internal/cli/backend` 反向 import 父包 `internal/cli`（P1）**：backend.Profile.NewProtocol 直接构造 `cli.ClaudeProtocol` / `cli.ACPProtocol`，导致 cli 想用 backend.Get 必须 blank-import 或硬编码 wrapper.go 的 `backendDisplayName` / `isKnownBackendID` switch（自陈债）。建议把 backend 提升为兄弟包 `internal/clibackend` 或 `internal/agentprofile`，反转为 cli→backend；最小路径是 backend 仅暴露 Profile 元数据（DisplayName/DefaultBinary/DetectInProc/HistoryDir/CostUnit/Features），把 NewProtocol 反向到 cli 包内（`cli.RegisterProtocolFactory`）。Breaking: 是（仅 internal）。
- [ ] **R235-ARCH-2 — cron / sysession 各自定义 RunState / TriggerKind / ErrorClass（P1）**：两套字符串枚举语义高度重叠（succeeded/failed/timed_out/canceled、scheduled/manual、validation/upstream/timeout/panic）。注释自陈"Mirrors cron.RunState semantics"。建议 `internal/runschema` leaf 包共享 `type State string` / `type TriggerKind string` / `type ErrorClass string`，cron/sysession 改 alias。dashboard.js 也只剩一组常量。Breaking: 否（type alias 源码兼容）。
- [ ] **R235-ARCH-3 — `dispatch.Dispatcher.scheduler` 仍持具体 `*cron.Scheduler`（P1）**：cron→session 已用 SessionRouter interface 反转，server→session 用 HubRouter，唯独 dispatch→cron 没抽 consumer interface。建议 `dispatch/cron_consumer.go` 定义 `CronScheduler` interface（AddJob / NextRun / ListJobs / DeleteJob / PauseJob / ResumeJob 6 method），contract_test 加 `var _ dispatch.CronScheduler = (*cron.Scheduler)(nil)`。
- [ ] **R235-ARCH-4 — `cli.Wrapper.ShimManager` 是导出可变 `*shim.Manager`，protocol/transport 未拆分（P1）**：注释自陈"R230-ARCH-13 / R231-ARCH-7 已知债"。sysession.Runner 已走绕过 shim 的旁路 → transport 抽象缺失。建议 `cli.Transport` interface（StartSession / Reconnect / Close），ShimManager 适配为它的实现；新增 `WithTransport` option。Breaking: 是（cmd/naozhi/main.go 一处真消费方）。
- [ ] **R235-ARCH-5..30 — 见各 reviewer 报告**：包含 config 反向 import session/project（ARCH-5）、3 个 workspace 概念名共享（ARCH-6）、platform.QuestionItem 与 cli.AskQuestionItem 双向手抄（ARCH-7）、feishu→transcribe 直依（ARCH-8）、cron / dispatch 各有 prompt/schedule 校验二级实现（ARCH-9）、Router struct 字段 30+（ARCH-10）、cli 包 67 文件混杂（ARCH-11）、contract_test 未覆盖 dispatch.CronScheduler / sysession.Manager（ARCH-12）、session 通过 blank import 触发 history backend init（ARCH-13）、SessionGuard / MessageQueue 运行时 either-or（ARCH-14）、server 13 个 *Handlers god struct（ARCH-15）、cli.Process 字段导出与并发约束注释冲突（ARCH-16）、eventlog/schema 未承担 single source of truth（ARCH-17）、dispatch.DispatcherConfig.Router 字段类型 *session.Router（ARCH-18）、router.Version 同时承载 data + render（ARCH-19）、process.go 1500+ 行字段 60+（ARCH-20）、discovery 同时依赖 cli + cli/backend（ARCH-21）、replyTagForBackend sync.Once 兜底掩盖 wireup 时序（ARCH-22）、Reactor / QuestionCardSender type-assertion 模式（ARCH-23）、~~scheduler.go 2400+ 行混合多职责（ARCH-24）✓ 已修：PR #309 6-stage split scheduler.go 至 852 行 + 7 职责文件~~、sysession.Runner 硬编码 claude bin（ARCH-25）、Wrapper.Spawn 100 行 protocol+transport 混杂（ARCH-26）、validateWorkspace / cron.workDirUnderRoot 重复（ARCH-27）、cli 包同住 image/thumbnail/askquestion/todo DTO（ARCH-28）、ManagedSession.process 直接持 *cli.Process（ARCH-29）、缺 wireup 集中包（ARCH-30）。所有 ARCH 类条目均为方案不唯一 / 跨模块改动，按 Round 节存档供未来 RFC 引用。

### Go 正确性 / 性能（合并到现有跟踪）

- [~] **R235-GO-2 — `runStore.warmCache` 签名暗示返错却 always nil**：实际逻辑正确（fallback 到 diskListNewestFirst），文档与签名分叉但行为安全；判定 P3 cosmetic，归到 R231-PERF-1 主条目跟踪。
- [ ] **R235-GO-3 — `cron.Stop()` 包装 goroutine 在 deadline-hit 路径永久泄漏**：注释 R222-GO-10 已承认 intentional orphan；非 production 影响，仅 goroutine-leak 检测器在 test 中报泄漏。需要 RFC 决定是否在 godoc 中显式记录这是 by-design 的 leak，或加 ctx 信号让 goroutine 退出。
- [ ] **R235-GO-4 — `buildExcerptFromHistory` 跨行截断后 EXCERPT marker 检测失效（P1）**：512 bytes/line 截断可能把 `---BEGIN CONVERSATION EXCERPT---` 切到两行，使 ReplaceAll 检测不到完整 marker。建议在 `buildExcerpt` 内逐行处理阶段就替换 marker，而非仅末尾一次性扫描。改动 sysession/auto_titler.go 内部，无 breaking。
- [ ] **R235-PERF-1 — `Protocol.ReadEvent(line string)` 强制 string→[]byte 堆拷贝（P1）**：`shimMsg.Line` 是 `string` 是根因，两处改动需同步：shim 协议字段改 `json.RawMessage`，ReadEvent 签名改 `[]byte`。同根因主条目持续跟踪 R231-PERF-1。Breaking: 是（shim wire format + Protocol interface）。
- [ ] **R235-PERF-7 — `linker.Resolve` 每 task_started spawn 裸 goroutine**：8 并发上限已生效，但 worker pool + 任务队列收益更大；归 R230-PERF-3 主条目。
- [ ] **R235-PERF-8 — `permission_request` 路径在 readLoop 同步走 json.Unmarshal+Marshal+Write**：建议响应改非阻塞 reply channel + writeLoop。
- [ ] **R235-PERF-9 — `marshalJobsLocked` 全量 SortFunc + json.Marshal 在 s.mu 内**：建议 marshal 移出锁（先快照、释锁、再 marshal）。
- [ ] **R235-PERF-10 — `sanitizeStderrLine` 慢路径每行 alloc strings.Builder**：sync.Pool 化。
- [ ] **R235-PERF-12 — `applyMetadata` O(N²) merge**：改 map keyed by Unit。
- [ ] **R235-PERF-13~16/18/19 — 杂项 alloc 优化**：redactPathsInCronError pool / EventEntry struct copy 延迟 / buildUserEntry 单图快路径 / textBuf 容量回收 / addJobAcquiringLock per-chat O(1) 计数 / runDeadlineWatchdog goroutine pool。

### 安全（NEEDS-DESIGN）

- [ ] **R235-SEC-2 — `transcriptTurn.Input` 是 `json.RawMessage` 经 SetEscapeHTML(false) encoder（P1）**：当前 dashboard JS 用 textContent 安全，但服务端无强制；建议 `b.Input` 通过 HTML-escaping JSON encoder 重新编码后再赋给 Input any。改动需评估 dashboard 是否依赖 raw bytes 顺序。

### 代码质量（NEEDS-DESIGN）

- [ ] **R235-CR-3 — `ErrClassPausedConcurrent` 常量定义后从未发出**：`registerJob` 闭包并发暂停分支静默 return 不调 finishRun，使 dashboard 永远收不到 paused_concurrent 状态。建议要么发 emitOverlapSkipped(ErrClassPausedConcurrent)，要么在常量旁加注释 + 删除。需要决定 dashboard UX 是否需要这个状态。
- [ ] **R235-CR-4 — `XxxByID`/`Xxx` 6 对方法 60 行重复（DeleteJob/PauseJob/ResumeJob × ByID/byPrefix）**：抽 `deleteJobAfterLookup` / `pauseJobAfterLookup` / `resumeJobAfterLookup` helper。无 breaking 但改动 ~120 行。
- [ ] **R235-CR-7 — `emitRunEnded` 无 godoc 与 emitRunStarted 不对称**：补 godoc 说明 CronRunEndedTotal 在 finishRun 末尾 bump 而非函数内部，防止维护者错误对齐造成 double-count。
- [ ] **R235-CR-9 — `skipAppendTrim` 三处 appendsSinceTrim=0 重置语义不齐**：注释解释或对齐到"只在真正触发 trim 时才重置"。
- [ ] **R235-CR-14 — `validateSchedule` 与 `PreviewSchedule` 重复 cronParser.Parse + 两次 sched.Next**：复用 `schedulePeriod`。
- [ ] **R235-CR-15 — diskListNewestFirst 同秒 mtime tie-break 用 runID（hex）不反映时间顺序**：注释说明，或在 list 路径用 StartedAt 二次排序（成本高）。

## Round 234 — 5-agent 并行 code review 第 44 轮（2026-05-23）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描，本轮直接修 11 处（见顶部摘要 R234-SEC-1/-4/-9/-10、R234-PERF-12、R234-CR-9/-10/-11、R234-GO-7/-13/-14）。下方为本轮新发现且不适合直接修的条目（破坏兼容 / 跨包重构 / 需 RFC / 方案不唯一）。

### 架构（高优先）— 本轮新发现

- [ ] **R234-ARCH-1 — `dispatch` 同时直依 `cli` + `cron` + `session` + `project`（P1）**：dispatch 既是 IM↔session 中间层又把 cli.EventCallback / ImageData / SendResult 当裸类型在签名中传，commands.go 反向调 cron.Scheduler。建议在 dispatch 内定义 `SendResult` / `Event` / `Image` 最小 DTO，由 session 层做 cli↔dispatch 翻译；commands 的 cron 操作走 `CronAdmin` interface 注入。改动 ~6 文件 ~150 行，无运行时 breaking。
- [ ] **R234-ARCH-2 — `cli.wrapper` 反向 import `internal/shim`（P1）**：cli 是协议+子进程层，shim 是 cgroup 边车；当前 cli→shim 的导入让 cli 无法在没有 shim 的环境编译/单测，shim 协议演进绑死 cli 协议。建议把 shim 抽象为 `cli.LauncherSpawner` interface，具体 `shim.Launcher` 在 session 层注入。需 R231-ARCH-1（runner 旁路 wrapper）落地之后再做以避免改两次签名。
- [ ] **R234-ARCH-3 — `sysession` 与 `cron` 各自定义 RouterView，DTO 散在两包（P1）**：cron `SessionRouter` interface (scheduler.go:72) + sysession `SystemSessionRouter` (router.go:25) 形成两套 router 适配器，都把 cli.EventEntry 当公共契约。建议在 `internal/session/api`（新子包，无 cli 依赖）放统一 `RouterView` + DTO，cron/sysession/scratchPool/quick session 共用。改动 ~10 文件，长期收益大。
- [ ] **R234-ARCH-4 — `internal/session/contract_test.go` 反向 import dispatch+server+cron+upstream（P1）**：即使 _test.go 文件，contract test 引用 dispatch/server/cron/upstream 让 session 包成为依赖图根+叶。建议把 contract_test 移到 `internal/integration/session_contract_test` 独立包。
- [~] **R234-ARCH-5 — server.send / dashboard_send / dispatch.SendSplitReply 三套发送路径（归档 2026-05-23）**：明确归到 R230-ARCH-2 SendOrchestrator 主条目；本轮仅记录 server.send 是第三条独立发送路径（reverse-node + WS push）作为未来 SendOrchestrator 设计输入。本批 PR 关闭子症状条目。
- [ ] **R234-ARCH-6 — Shutdown 顺序无形式化合约（P1）**：cron.Scheduler.Stop / sysessionMgr.Stop / router.Shutdown 在 server.Shutdown 序列里位置不明。建议写 ADR `docs/rfc/shutdown-order.md` + 各组件导出 `WaitForExit(ctx) error`，由 server 串行 wait。
- [ ] **R234-ARCH-7 — `cron.SessionRouter.GetOrCreate` 暴露完整 *ManagedSession（P2）**：调度器只需 Send，但接口暴露 50+ 方法的 ManagedSession，cli 接口塌陷同款。建议进一步收敛为 `Sender` 接口。
- [~] **R234-ARCH-8 — `server.Hub` 直 import dispatch+cron+cli+project+session（P2）**：典型 god hub。建议拆 `WSTransport`（仅 conn lifecycle）+ `WSCoordinator`（业务），需独立 RFC。 *(PR #327 R243-ARCH-2 已 6-stage split：wshub.go 2028→525 行，方法分到 _broadcast/_send/_subscribe/_eventpush/_upgrade 5 文件。但 Hub struct 未拆 — 28+ 字段保持集中以维持锁不变量；进一步抽 BroadcastDispatcher / SubscriberRegistry 子 struct 跟踪到 R248-ARCH-6。)*
- [ ] **R234-ARCH-9 — Channel adapter 越权读 router 状态：dashboard_session.go / dashboard_discovered.go 直 import cli+session（P2）**：缺 wire DTO 一层，cli.EventEntry 字段改名会自动 break dashboard JSON shape。建议 `internal/server/api/dto` 定义 SessionDTO/EventDTO，~20 文件。
- [ ] **R234-ARCH-10 — dispatch ↔ platform 双向：platform webhook 回调 leak dispatch.IncomingMessage / Reply 内部类型（P2）**：建议在 `internal/platform` 定义最小 wire types，dispatch 提供 `From(IncomingMessage)` 适配。
- [ ] **R234-ARCH-11 — 命名空间策略碎片化：sys: / cron: / scratch: / kiro: / chatKeyPrefix 5 类前缀无中央注册表（P2）**：建议 `internal/session/namespace.go` 集中 `NamespacePrefix` enum + `ParseKey(s)`，CI 加 grep test 拒绝裸字符串。前置 R233-ARCH-5。
- [ ] **R234-ARCH-12 — Protocol/Sink/Tailer 三大事件接口契约未文档化（P2）**：建议新建 `docs/rfc/event-pipeline-contracts.md` + `internal/cli/protocoltest` 共享测试套件（类似 fstest.MapFS）。
- [ ] **R234-ARCH-13 — server/agent_tailer 直 import cli，IO 路径绕过 router（P2）**：建议 tailer 走 `router.Tail(key) <-chan EventDTO`，router 内部决定从哪源取（persisted vs live）。
- [ ] **R234-ARCH-14 — buildServer 初始化顺序 fragile（P2）**：建议抽 `serverDeps` struct + `buildCore`/`buildHandlers`/`buildHub`/`wire` 分步。
- [~] **R234-ARCH-15 — sysession.runner 旁路 cli wrapper 但 auto_titler 仍要读 cli.EventEntry（归档 2026-05-23）**：明确归到 R234-ARCH-3 的统一 RouterView 方案；待 R234-ARCH-3 落地时一并解决，子症状条目本批 PR 关闭。
- [ ] **R234-ARCH-16 — server/discovery_cache.go 越层依赖 project + cli（P3）**：建议 router 暴露 `ExcludedSessionIDs() iter.Seq[string]`，discovery 自治。~30 行。
- [ ] **R234-ARCH-17 — dispatch.consumer.go 引用 *session.ManagedSession 而非更窄 SessionHandle（P3）**：建议 `dispatch.SessionHandle interface { Send(...); Key() string }`。
- [ ] **R234-ARCH-18 — internal/session/testutil.go 不带 _test 后缀 + 无 build tag（P3）**：可能被生产二进制 link。建议 rename 为 testutil_test.go 或加 `//go:build testing`。
- [~] **R234-ARCH-19 — internal/dispatch/dispatch_test.go 单文件 import cli/cron/platform/session 4 个生产包（归档 2026-05-23）**：R234-ARCH-4 子症状；与 contract_test 重定位方案一起处理，本批 PR 关闭子条目。
- [ ] **R234-ARCH-20 — wshub_agent.go 仅依赖 session 但被并入 server 包导致编译图污染（P3）**：建议挪到 `internal/server/wshub` 子包。
- [ ] **R234-ARCH-21 — 跨包共享 limits 常量风格不一（cron/limits.go vs server/server.go vs dispatch/dispatch.go）（P3）**：建议集中到 `internal/limits`。
- [ ] **R234-ARCH-22 — `internal/cli/backend` 同时被 server 与 session 直接 import（P3）**：建议拆 `backend.Profile`（值类型）vs `backend.Launcher`（行为，仅 cli 用）。
- [ ] **R234-ARCH-23 — cron 走 router 而 quick session 走 dispatch.SendSplitReply 路径不一致（P3）**：建议 ADR `docs/rfc/cron-vs-dispatch-paths.md` 或归 R230-ARCH-2 SendOrchestrator。
- [ ] **R234-ARCH-24 — server/upload_store.go import cli 仅为 cli.ImageData（P3）**：建议 upload_store 输出自身 `Blob struct{Data []byte; MIME string}`，dashboard_send 转 cli.ImageData。
- [ ] **R234-ARCH-25 — session/eventlog_bridge.go 是事实上的 fan-out hub 但命名 bridge（P3）**：建议 rename 为 event_pipeline.go + 引入 `EventPipeline` + `[]EventSink`。

### Go 正确性 / 并发 — 本轮新发现

- [ ] **R234-GO-3 — scheduler.go:778 `go trimAll` goroutine 无 WaitGroup（P1）**：Stop 不等待此 goroutine 退出，半删 runs 目录残留 / 重启并发 trimAll 可能与 per-job lock 之外的 ReadDir+Remove 出现窗口。建议给 `trimAll` 加 `s.gcWG sync.WaitGroup`，Stop 先 gcWG.Wait()（带短超时）+ 传 ctx 让 trimAll 内每个 jobID 循环检查 `ctx.Err()`。
- [ ] **R234-GO-4 — runstore.cacheGet 双锁窗口（P2）**：第一次释放 entry.mu 后 warmCache 在 entry.mu.Lock 前另一 Append 已 cacheHeadPush no-op + trimJobLocked 触发 cacheTrimAfterDisk no-op，warmCache 再读磁盘可能漏掉刚 Append 条目。建议在 warmCache 内 entry.mu.Lock 前先 jobLock.Lock（已有此模式）。
- [ ] **R234-GO-8 — runstore.diskListNewestFirst 不区分 mtime-only 与 full-parse 路径（P2）**：warmCache 走也会 ReadFile 全部文件。建议拆 `diskListMtime`（只 ReadDir+stat+sort）和 `diskReadSummaries`（batch ReadFile）。
- [ ] **R234-GO-10 — runstore.trimJobLocked sort 用 cmp.Compare(UnixNano) 而非 time.Compare（P3）**：边界精度 + 风格。建议 `slices.SortFunc(items, func(a,b) int { return b.mtime.Compare(a.mtime) })`。
- [~] **R234-GO-15 — trimAll goroutine 无 ctx 传播 Stop 无法中断（归档 2026-05-23）**：归 R234-GO-3 同主条目跟踪（gcWG.Wait + ctx.Err 一起做），本批 PR 关闭子条目。

### 安全 — 本轮新发现

- [ ] **R234-SEC-2 — `handleTrigger` 无 per-IP 速率限制（P1）**：`POST /api/cron/trigger` 每次触发 `session.GetOrCreate + sess.Send`，无任何限流；持有 token 的脚本可在秒级把 maxJobs=500 全部触发耗尽 shim/cgroup 资源。建议新增 `triggerLimiter *ipLimiter`（如 `rate.Every(2s), burst=3`）。
- [ ] **R234-SEC-3 — `runsLimiter` 共享 list/detail/transcript 三端点 IO 代价 100x 不对称（P2）**：transcript 走 8MB JSONL+Scanner，list 走 cache。建议 transcript 单独 `transcriptLimiter`（`rate.Every(5s), burst=5`）。
- [ ] **R234-SEC-5 — transcriptResponse Input json.RawMessage 透传未脱敏（P2）**：tool_use.input 含 Bash 命令明文，可能含 API 密钥/DSN。建议对 command/file_path/url 做 200 字符截断，或移除 Input 仅留 Summary。**dashboard JS breaking**。
- [ ] **R234-SEC-6 — `handleList` 返回所有 job 的 LastResult/Prompt 全量轮询带宽放大（P2）**：50 jobs × (8KB prompt + 4KB result + 5×summary) ≈ 1MB/req × 1Hz = 1MB/s。建议 list 返回截断 prompt（1KB），detail 接口返回全量；或加 server-side `?search=`。**dashboard JS fuzzy-search 需迁移**。
- [ ] **R234-SEC-7 — `Job.LastResult` 落盘无 secret-pattern 过滤（P2）**：claude 输出可能含明文 sk-ant-/ghp_/AKIA token。建议 `recordResultP0WithSanitised` 增加可配置黑名单 + 类似 `isSensitiveDownloadName` 的后处理。
- [ ] **R234-SEC-8 — `flattenJSONLEvent` tool_use.Input 字段无大小守卫（P3）**：500 turns × 256KB/line = 128MB 序列化输出。建议 `len(b.Input) > maxToolInputBytes`（64KB）截断为 `[truncated]` 或置空。
### 性能 — 本轮新发现

- [~] **R234-PERF-1 — runstore.cacheHeadPush O(N) memmove（归档 2026-05-23）**：归 R234-GO-1 / R233-PERF-2 同主条目（ring buffer 改造），本批 PR 关闭子条目。
- [~] **R234-PERF-2 — shimWriter.Write fast-path string(data[:len-1]) heap-copy（归档 2026-05-23）**：每次 stdin write 把 4KB payload 拷贝到 string 仅为 shimClientMsg.Line string 字段；归 R71-PERF-H1 主条目跟踪（需 shim 协议 compat 评估），本批 PR 关闭子条目。
- [~] **R234-PERF-3 — protocol_claude.ReadEvent json.Unmarshal([]byte(line),...)（归档 2026-05-23）**：阻塞在 shim 协议 bump，归 R231-PERF-1 主条目跟踪，本批 PR 关闭子条目。
- [~] **R234-PERF-4 — KnownSessionIDs 无 TTL 缓存（归档 2026-05-23）**：归 R233-PERF-3 同主条目跟踪（atomic.Pointer[knownSessionIDsCache] 30s TTL + finishRun/DeleteJob 失效），本批 PR 关闭子条目。
- [~] **R234-PERF-5 — TranscriptReader.readLocked 每 200ms 重 open/seek/read/close（归档 2026-05-23）**：归 R233-PERF-4 主条目跟踪（keep *os.File + Seek/ReadAt + inode 变更才重开），本批 PR 关闭子条目。
- [~] **R234-PERF-9 — runstore.skipAppendTrim time.Now 在 fast-exit 之前（归档 2026-05-23，已核实为误报）**：实际 line 254 已先做 `len(entry.runs) > 0` 守卫，time.Now 在 fast-path 之后；review 误读控制流。本批 PR 关闭。
- [ ] **R234-PERF-10 — parseTranscriptTime 每行 RFC3339Nano 解析 ~300ns（P2）**：250 line/s × 300ns = 75µs/s。建议 hand-parse 整数字段或 ParseInLocation+UTC 缓存。
- [ ] **R234-PERF-13 — readShimLine 错误漏 cap drain 路径（P3）**：bufio chunk 临时切片漏。
- [ ] **R234-PERF-14 — runstore.warmCache 持 entry.mu 做 ReadDir+N×ReadFile 阻塞 dashboard 冷启动（P3）**：建议 warm 异步，首次 Recent miss 立即返空切片，后台 populate。
- [ ] **R234-PERF-15 — agent_tailer pollOnce 200ms ticker 对 refCount==0 silent tailer 仍 open/close（P3）**：建议 silent + size-unchanged 时 backoff 到 2s。
- [ ] **R234-PERF-16 — protocol_claude.extractAskQuestion 每 assistant 事件全 block 扫描（P3）**：建议 `strings.Contains(rawContent, "AskUserQuestion")` 早 short circuit。

### 代码质量 — 本轮新发现

- [ ] **R234-CR-1 — runstore.truncateForRetry 与 scheduler.sanitiseRunResult 共享 truncate-with-suffix 但分散两文件（P2）**：本轮已修 truncatedSuffix 字面量统一（R234-CR-9 直接修）；后续可将 truncate-with-suffix helper 移到 limits.go 单点共享，让两个 caller 都引用。
- [ ] **R234-CR-2 — workDirUnderRoot/workDirReachable 在 cron + server 各一份（P2）**：建议移到 `internal/osutil` 或新 `internal/fsutil` 共享。本轮已为 workDirReachable 加 root-containment 不强制注释（R234-CR-11）。
- [ ] **R234-CR-3 — generateID/generateRunID 都委派 generateHexID 三个名字一个函数（P2）**：建议要么收敛为单一 generateID 要么明确分歧意图。
- [ ] **R234-CR-4 — DeleteJob/PauseJob/ResumeJob ByPrefix vs ByID 6 方法 lock/persist/save 模板重复（P2）**：建议 `mutateJob(id string, fn func(*Job) error)` helper 内置生命周期。
- [ ] **R234-CR-5 — `var stopBudget` package-level mutable global 仅为测试注入（P2）**：建议移到 `SchedulerConfig.StopBudget` field + applyDefaults。
- [ ] **R234-CR-6 — finishRun 双 sanitise 层级不透明（P3）**：建议 recordResultP0WithSanitised 加注释说明哪些 caller 已 pre-sanitise；或拆 `WithSanitised` / `Raw` 两变体。
### 架构（高优先）— 本轮新发现

- [~] **R233-ARCH-1 — sysession.Runner 完全旁路 CLI Wrapper 抽象（归档 2026-05-23）**: R230-ARCH-1 / R231-ARCH-1 同根因，统一跟踪到 R231-ARCH-1，本批 PR
- [~] **R233-ARCH-2 — cli.Protocol 对 ACP 等无 SubagentLinker 后端抽象塌陷（归档 2026-05-23）**: R219-ARCH-3 / R224-ARCH-3 / R231-ARCH-6 / R233B-ARCH-3 同根因，统一跟踪到 R231-ARCH-6，本批 PR
- [~] **R233-ARCH-3 — dispatch.NotifyTarget / cron.notifyTarget / hub.sendWithBroadcast 三套消息出口（归档 2026-05-23）**: R219-ARCH-8 / R224-ARCH-4 / R230B-ARCH-3 / R231-ARCH-2 / R232-ARCH-9 同根因，统一跟踪到 R232-ARCH-9，本批 PR
- [~] **R233-ARCH-4 — quick session 与 IM 入口走两套 sendAndReply（归档 2026-05-23）**: R230-ARCH-2 / R231-ARCH-2 同根因，统一跟踪到 R230-ARCH-2，本批 PR
- [ ] **R233-ARCH-5 — server.handleQuickSession / scratch drawer 在 router.sessions 之外又长出第二条 lifetime 协议（P2）**: ScratchPool.Close → router.Remove(key) + OptsForKey 在 sweep 之前 Touch，多处不变量耦合在注释里。方案：把 ScratchPool 实现的 ManagedSession lifecycle 接口提到独立 RFC，加 `Router.NotifyScratchExpired` hook。Breaking：否。

### Go 正确性 / 并发 — 本轮新发现

- [ ] **R233-GO-3 — executeOpt runInflight 6 处 Store(&local) 强制 heap escape（P2）**: 每次 cron run 6 个变量逃逸到 heap，每条 run 多 6 次小对象分配。方案：runInflight struct 内 `atomic.Pointer[string]` 字段改为 mutex 保护的直接 value，dashboard 读频率低 lock 成本可接受。Breaking：否。

### 安全 — 本轮新发现

- [~] **R233-SEC-1 — Dashboard CSP 仍 unsafe-inline（多轮 NEEDS-DESIGN 归档 2026-05-23）**: R226-SEC-2 / R227-SEC-9 / R228-SEC-1 / R229-SEC-6 / R230-SEC-1 / R231-SEC-2 / R233B-SEC-1 同根因；前端模板大改造（dashboard.html 80+ 内联 onclick 移到外部 JS 或迁 nonce/hash CSP）。统一收敛到主条目 R231-SEC-2。本批 PR
- [~] **R233-SEC-2 — ExtraArgs 进 CLI argv 仍无 flag allowlist（多轮 NEEDS-DESIGN 归档 2026-05-23）**: R217-SEC-1 / R219-SEC-1 / R225-SEC-1 / R227-SEC-1 / R229-SEC-1 / R231-SEC-4 同根因；Breaking。统一收敛到主条目 R231-SEC-4，本批 PR
- [~] **R233-SEC-3 — allowed_root 未配 + 公网 token 部署只 Warn（多轮 NEEDS-DESIGN 归档 2026-05-23）**: R226-SEC-6 / R227-SEC-3 / R229-SEC-3 / R231-SEC-3 同根因。统一收敛到主条目 R231-SEC-3，本批 PR
- [ ] **R233-SEC-4 — 飞书签名失败前未 dedup nonce（P2）**: 攻击者可 5 分钟窗口内用同 ts+nonce 暴力试不同 body。需谨慎—插入位置改变会打破 challenge 验证流。方案：在 signature verify 之前先 reserve nonce，失败也保留。Breaking：否（行为变化）。
- [ ] **R233-SEC-7 — 同 IP 可保留 60 个未认证 WS 连接（P2）**: maxWSConns=500 全局，无 per-IP 未认证 cap。方案：未认证 WS 连接 per-IP 20 上限。Breaking：否。
- [~] **R233-SEC-8 — /static/dashboard.js 未鉴权且无 SRI（归档 2026-05-23）**: R230-SEC-3 / R231-SEC-11 同根因。统一收敛到 R231-SEC-11，本批 PR
- [~] **R233-SEC-9 — backend ID charset 在 cron CRUD vs WS path 不对齐（P2）**: cron 走 `[a-z0-9_-]`，WS 路径走 `[a-zA-Z0-9_.-]`。方案：抽统一 validateBackendID。Breaking：是（操作员若用 uppercase/dot backend ID）。继承 R232-SEC-5。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
### 性能 — 本轮新发现

- [~] **R233-PERF-1 — ClaudeProtocol/ACPProtocol ReadEvent 每次 byte 复制（归档 2026-05-23）**: 同 R231-PERF-1 一并归档，本批 PR
- [ ] **R233-PERF-2 — runStore.cacheHeadPush 仍 O(N) memmove（P1）**: keepCount=200 每次 Append 触发 200 struct copy shift，持 jobLock 期间执行。方案：改 ring buffer。Breaking：否。
- [ ] **R233-PERF-3 — KnownSessionIDs 历史面板每次 1Hz 全量遍历 jobs × Recent(200)（P1）**: 50 job × 200 row × ~100B = 1MB 数据移动每秒。方案：scheduler 缓存 atomic.Pointer[map] 由 finishRun/DeleteJob 失效。Breaking：否。
- [ ] **R233-PERF-4 — TranscriptReader.readLocked 每次 Tail open+ReadAll+close（P2）**: 50 tailer × 5/s = 250 syscall/s。方案：持久化 *os.File，每 Tail 只 ReadAt(offset)，inode 变更时重开。Breaking：否。
- [ ] **R233-PERF-5 — flattenJSONLEvent 每行 unmarshal 到 map[string]any（P2）**: 整 JSON 反射，只用首个 key。方案：改 transcriptContentBlock 6 字段 struct。Breaking：否。
- [ ] **R233-PERF-6 — readRun 每个 .json 文件 os.ReadFile cold path（P2）**: 100 文件 × open+stat+alloc+read+close。方案：diskListNewestFirst 仅返回 mtime+runID summary，defer body 到 Get。Breaking：否。
- [ ] **R233-PERF-7 — agentTailer 200ms ticker buffered 超 500 时 append([]EventEntry(nil), …) 复制 500 条（P3）**: 每 poll 150KB 内存 copy。方案：ring buffer。Breaking：否。
- [~] **R233-PERF-8 — sysession.Manager hookMu RWMutex 改 atomic.Pointer 收益评估（P3）**: 每 tick 两次 RLock 对单写多读场景边际收益小。RWMutex Go 1.21+ 已无锁化常见路径。可选优化，先标记。Breaking：否。 — 评估关闭（已归档 2026-05-23 复核）：godoc 已落地决策——`internal/sysession/manager.go:150-155` hookMu 段落显式说明 RWMutex 选择理由：reads 每 Tick 两次（run start + run end）+ writes 仅 SetCallbacks 一次；RWMutex 让并发 Tick 并行 RLock 无锁竞争。改 atomic.Pointer[func] 需双字段（onRunStarted + onRunEnded），增加一次 atomic.Pointer.Store 协调成本，且 Go 1.21+ RWMutex 在 RLock 路径已是 atomic CAS（src/sync/rwmutex.go），benchmark 收益可忽略。本批 PR。

### 代码质量 — 本轮新发现

- [~] **R233-CR-1 — 4 个独立 fake test router struct（误报关闭 2026-05-23）**: 复核 4 个 fake 服务于不同测试关注点而非"几近重复"——fakeRouter (run_p0_test.go) 携带 configurable error 字段；jitterStubRouter (jitter_test.go) 是 minimal stub 用于 jitter 路径；backendCapturingRouter (scheduler_backend_test.go) 记录 AgentOpts.Backend 用于 backend-routing 测试；fakeSessionRouter (session_router_test.go) 是 full session-router fake。三个共有方法（RegisterCronStubWithChain/Reset/GetOrCreate）表面机械重复，但消费者期望各自不同（错误注入 / no-op / 字段捕获 / 完整生命周期），合并到 option-style 统一 fake 会引入 wrapper/builder 否定 net-positive 收益。归档关闭，本批 PR
- [~] **R233-CR-2 — TriggerCatchup/ErrClassPanic/DaemonTriggerManual 仍 export 但无外部消费者（P3，关闭归档 2026-05-23）**: 同根因 R232-CR-8 已落地——三处 godoc 各加 RESERVED 警告，明确"forward-compat schema 占位"语义；无生产 caller，无测试 pin，但保留 export 让未来填充实现时不破坏 value contract。string value（"catchup"/"panic"/"manual"）外部若 string-match 也能识别。本批 PR 关闭归档
### Round 233 第二批补充（PR #240 review 发现）

#### Go 正确性 / 并发（P1）

- [ ] **R233B-GO-1 — runinflight setPhase/setSessionID 把参数指针存入 atomic.Pointer（P1）**: `r.phase.Store(&phase)` 存的是参数局部变量地址；同样 setSessionID 存 `&id`；executeOpt 1898-1910 里 `ph := PhaseQueued; inflight.phase.Store(&ph)` 也是同模式。当前 Go 编译器会把这些值 escape 到堆上是安全的，但模式依赖 escape 分析；建议用 helper 拷贝到稳定 heap 槽或改 `atomic.Value` + string。Breaking：否（包内）。
- [~] **R233B-GO-2 — cron Scheduler.Stop deadline.C 共享 timer（误报关闭 2026-05-23）**: 复核 scheduler.go:884-928，第一个 select 通过 deadlineHit=true 标志记录"timer 已 fired"；第二个 select 由 `if !deadlineHit` 外层 gate 守护，仅在 deadline.C 尚未 fired 时才进入，即第二个 select 内 `case <-deadline.C` 仍保持 active 而非 drained。R222-GO-10 + 这层 deadlineHit gate 已经覆盖了"timer 已耗 + triggerWG.Wait 不就绪"的边角——deadline.C 不会被双重消费，第二段不会永久阻塞。godoc 注释（line 879-915）已明示该不变量。归档关闭，本批 PR

#### 性能（P1/P2）

- [~] **R233B-PERF-1 — Protocol.ReadEvent 接受 string（归档 2026-05-23）**: R67-PERF-1 / R226-PERF-1 / R231-PERF-1 / R232-PERF-1 / R233-PERF-1 / R233B-PERF-1 跨 ~30 轮重申。Breaking 接口签名改造；统一跟踪 R231-PERF-1。本批 PR
- [ ] **R233B-PERF-4 — readLoop 每行做两次 json.Unmarshal（外层 shimMsg + 内层 ReadEvent）（P2）**: 第一次解 `{"type","line"}` 协议帧，第二次解嵌入 claude 事件。方案：shimMsg.Line 改 json.RawMessage 直传 ReadEvent；或外层手写字节扫描 type 分支。配合 R233-PERF-1 一次解决。Breaking：否。
#### 安全（P1/P2）

- [~] **R233B-SEC-1 — dashboard CSP 含 unsafe-inline script-src 与 style-src（归档 2026-05-23）**: 同 R231-SEC-2 / R233-SEC-1 一并归档，本批 PR
- [ ] **R233B-SEC-2 — Feishu VerificationToken-only 模式缺少 body HMAC（P1）**: token 泄露即可伪造任意事件体。方案：要么强制 EncryptKey；要么把"VerificationToken-only"明确标 deprecated 并加运行时启动告警。Breaking：是（运维侧）。
#### 架构（P1/P2）

- [~] **R233B-ARCH-1 — internal/cli 包成 9 合一 god package（P1）**: 63 文件 30+ 导出类型；其它 87 个文件靠它做"通用类型库"。方案：拆 cli/process / cli/event / cli/imaging / cli/transcript（protocol 已成形）；或抽 EventEntry/ImageData/AskQuestion 到 internal/clitypes。Breaking：是（import 路径）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R233B-ARCH-2 — *session.Router 80+ 方法 god struct（P1）**: 拆 6 文件但锁/字段共享；HubRouter/SessionRouter 多套 narrow interface 各取一片。方案：拆 SessionStore/Spawner/DiscoveryAdapter/ShimReconcileLoop/CleanupLoop 多 struct 组合在 Router 里。Breaking：否（聚合接口不变）。
- [~] **R233B-ARCH-3 — server.agent_tailer + wshub 直接持 *cli.SubagentLinker / *cli.TranscriptReader 指针（P1）**: server→cli 偷越层访问。方案：SubagentLinker 暴露收敛到 session.ManagedSession.SubscribeAgentEvents(taskID, fn) 回调式 API。Breaking：是（server tailer 改造）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R233B-ARCH-4 — sysession.auto_titler 直 import internal/cli 取 EventEntry（P1）**: daemon 层不应依赖 process 类型。方案：把 EventEntry 抽到 internal/eventlog/schema 或 internal/clitypes。Breaking：否（仅 import 整理）。
- [ ] **R233B-ARCH-5 — cli.HistorySource vs history.Source 名存实亡 interface（P2）**: 仅为防 import cycle 而存在的零适配 interface。方案：抽公共类型后 cli.HistorySource 删除。Breaking：是。
- [ ] **R233B-ARCH-6 — 三处独立 LoadJSON/SaveJSON 但只 10 个调 osutil.AtomicWrite（P2）**: cron/store + session/store + 其它直接 os.WriteFile。方案：抽 internal/storage 包统一 LoadJSON/SaveJSONAtomic（含 corrupt rename + size cap + fsync）。Breaking：否。
- [ ] **R233B-ARCH-7 — config 反向 import internal/session 拿 AgentOpts（P2）**: 让 config 失去叶子节点资格。方案：config 用独立 AgentConfig 类型，session 在构造时翻译。Breaking：否。
- [ ] **R233B-ARCH-8 — server 包 60+ 文件未分子包（P2）**: dashboard_*.go 已逻辑分组但物理同包。方案：拆 server/dashboard / server/ws 子包。Breaking：否。
- [ ] **R233B-ARCH-9 — internal/upstream 把 discovery + cli.EventEntry 拉进 import 图（P2）**: upstream 应是纯传输层，但通过 SetDiscoverFunc/SetPreviewFunc 接收 cli 类型。方案：discover/preview JSON 构造移到 server，传 RPC handler map 给 upstream。Breaking：否。

#### 代码质量（P2/P3）

- [~] **R233B-CR-1 — recordResult 死代码双轨（归档 2026-05-23）**: 同根因 R230C-CR-1 已落地——recordResult 已删除（~85 行），persist_failure_test.go 改调 recordResultP0WithSanitised(j, result, errMsg, sessionID, errClass, state) 6 参数签名，全 race test 通过；R232-ARCH-2 / R220-GO-1 历史脚注（scheduler.go:2506）保留作为反向追踪锚点。归档关闭，本批 PR
- [~] **R233B-CR-6 — runRing.Snapshot() 无生产调用者（误报关闭 2026-05-23）**: 复核 internal/cron/ 全包扫描无 runRing 类型 / 无 runring_test.go 文件 / scheduler.go 中"per-job ring"是 runstore 的命名说法（newest-first slice 而非 ring buffer）。条目所指代码不存在，归档关闭，本批 PR
- [~] **R233B-CR-7 — skipAppendTrim 三条件无单测（误报关闭 2026-05-23）**: 复核 internal/cron/runstore_test.go:633+ 已经存在 dedicated table-driven test（包括 happy path + 三个边界条件：cold cache / not warm / appendsSinceTrim 达 batch / keepCount 接近 / oldest EndedAt 出 keepWindow），并测试 skipAppendTrim 对未知 jobID 的 fallback 返回 false (line 752)。条目所述缺失的测试实际已存在；归档关闭，本批 PR
- [~] **R233B-CR-8 — sysession.runOnce panic-recovery → CAS-release 无单测（误报关闭 2026-05-23）**: 复核 internal/sysession/manager_test.go:273 已有 `TestManager_PanicRecoveredAndInflightReset` 完整覆盖：(1) 注入 panicking Tick → 等待 inflight 复位 → 断言 false (line 317)；(2) 后续 tick pulse → 验证第二次 tick 真的能跑（CAS gate 不卡，line 327）。条目描述基于过时观察，归档关闭，本批 PR

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描发现 ~95 条。本轮直接修 7 处（cacheHeadPush O(N)→O(1) prepend / trimAll Start 改异步 goroutine / trimAll ReadDir 错误升级 Warn / sysession.runner stderr 预截 256 / cron previousTickBefore 1000 次迭代上限 / cron recordResult 4*1024 改 maxStoredResultRunes 常量 / cron slogPrintfLogger 同时匹配 panic|recovered + storeMu 注释从 saveJobs→saveMarshaledSeq + diskListNewestFirst & trimJobLocked 跳过 symlink）。
> 以下是需设计决策、破坏兼容、跨包重构、或方案不唯一不适合本轮直接修的条目。

### 架构（高优先）— 本轮新发现

- [~] **R232-ARCH-3 — cron 的 SessionRouter 接口仍声明 RegisterCronStub + RegisterCronStubWithChain（误报关闭 2026-05-23）**: 复核 cron/scheduler.go:72-90 SessionRouter 接口现仅声明 RegisterCronStubWithChain（line 82，无双方法），condition 已落地。session 包仍保留 RegisterCronStub 公开方法但仅给测试 / upstream test 用，移除会破坏 ~10 测试 — 留作独立测试重构。本批 PR
- [ ] **R232-ARCH-5 — 28 个 contract test 用 os.ReadFile + 字符串/正则 pin 源代码（P1）**: notify_background_ctx_test / debounce_contract_test / on_turn_done_contract_test 等把 gofmt + 注释 + 标识符当 API。方案：抽到行为级断言；真正必须 source-pin 的统一放 `internal/contract/` 加 README。Breaking：否（重构 test）。
- [ ] **R232-ARCH-6 — 5 个独立 *Router 消费者接口 + 2 个临时 cronStubChecker/cronSessionLister（P2）**: cron / dispatch / server.HubRouter / sysession.SystemSessionRouter / upstream.SessionRouter 重叠严重。方案：合并到 `internal/session/iface` 子包按 Lifecycle/Reader/Lookup 三细分接口。Breaking：否。
- [ ] **R232-ARCH-8 — dispatch 直 import internal/cron 持 *cron.Scheduler（P2）**: dispatch 已有 SessionRouter 消费者接口模式，cron 这一边却走具体类型，policy 不一致。方案：定义 dispatch.CronScheduler interface 子集（AddJob/ListJobs/...）。Breaking：否。
- [ ] **R232-ARCH-9 — cron 包直 import internal/platform 自承 channel adapter 职责（P2）**: cron 的 platforms map + ReplyWithRetry/SplitText/footer 与 dispatch 平行实现。方案：引入 dispatch.Notifier 接口注入 scheduler；cron 不再 import platform。Breaking：否（构造时 wiring 调整）。
- [ ] **R232-ARCH-11 — NotifyPolicy 隐式三态（P2）**: cron Job.Notify *bool 三态 + Platforms+NotifyDefault+per-job target 4 条优先级容易翻车（IM 创建默认回源 chat / dashboard 创建默认 silent）。方案：改 enum NotifyPolicy 显式建模。Breaking：是（cron_jobs.json schema 迁移）。
- [~] **R232-ARCH-12 — executeOpt 316 行单函数（多轮 NEEDS-DESIGN 归档 2026-05-23）**: R226-CR-10 / R229-CR-1 / R230-CQ-9 多轮重申。executeStep interface（preflight/spawn/send/finalize 4 步）拆解需把 stubRefresh closure / sendCtx / abortResult / watchdog 通道编织成 step 状态机，~600 行新结构 + 受影响 ~25 测试（jitter / fresh_shutdown / persist_failure / run_p0 / run_p1）。统一收敛到 R232-ARCH-1 god-file 拆分 RFC（lifecycle / jobs / execute / finish / persist / core 5–6 文件）跟踪。归档关闭，本批 PR
### Go 正确性 / 并发 — 本轮新发现

- [~] **R232-GO-1 — protocol_acp.go readUntilResponse 超时 goroutine 永久泄漏（归档 2026-05-23）**: 同 R224-GO-2 主跟踪。生产 shim path 已用 SetReadDeadline pulse 让 ReadBytes 立即返回 EOF；非 shim path 仅启动单元测试命中，注释已显式标记。本批 PR。
### 性能 — 本轮新发现

- [~] **R232-PERF-1 — protocol_acp parseSessionUpdate 每 token 双 Unmarshal（多轮 NEEDS-DESIGN 归档 2026-05-23）**: agent_message_chunk 分支已 typed-decode (ACPTextContent 2 字段)，热路径每帧 1 reflect 调用。彻底合并需 ACPUpdateDetail schema 改造（content 直接 inline ACPTextContent vs RawMessage）— Breaking 协议解析层。归档 NEEDS-DESIGN，本批 PR
- [~] **R232-PERF-4 — wshub.BroadcastSessionsUpdate AfterFunc 重复分配 timer（归档 2026-05-23）**: 实地复核：debounce 路径核心不变量是 `h.debounceTimer = nil` 由 AfterFunc callback 在 debounceMu 内清零（wshub.go:1424-1426），下一个 caller 据此判断是否要 `clientWG.Add(1)`。改为 NewTimer+Reset 复用模式要重写 callback ↔ caller 的 nil-flag 互锁机制（callback 不能 nil-out 共享 timer），与 Shutdown 的 `clientWG.Done()` paired-Stop 计数也要重排。debounce 50ms 窗口 + 500ms hard cap，AfterFunc 分配最高 ~20Hz，cold path 收益小风险大。"核心 broadcast 路径不动"约束下归档。本批 PR
- [~] **R232-PERF-6 — subagent_transcript map[string]any decode 每 block 多 alloc（误报关闭 2026-05-23）**: 同根因 R230B-PERF-4 已落地（mapAssistantLine + mapUserLine 已切到 typed transcriptAssistantBlock / transcriptUserBlock）。归档关闭，本批 PR
- [~] **R232-PERF-10 — cacheTrimAfterDisk EndedAt vs trimJobLocked mtime 时间源不一致（同根因 R230B-CR-4 已落地 2026-05-23）**: R230B-CR-4 已通过 godoc 锚点解决——cacheTrimAfterDisk godoc 加段落显式分析 mtime vs EndedAt 偏差窗口（典型 <10ms，pathological <1s），下一次 1Hz 拉取会 re-warm 抹平差异；统一到 mtime 需 250 syscall/s 或 +320KB cache，成本不划算，godoc-only resolution 锁定。归档关闭，本批 PR

### 安全 — 本轮新发现

- [ ] **R232-SEC-2 — 4 条 serve* 路径独立 TOCTOU（P2，扩展 R231-SEC-5）**: handleFileGet Lstat 后 serveRender/Preview/Raw/Download 各 open 一次。方案：handleFileGet 一次 OpenFile 拿 fd 传子函数；下游 Fstat 比 inode。Breaking：否。
- [ ] **R232-SEC-3 — feishu transport_hook 签名失败前 nonce 未入库（P2）**: HMAC 失败提前返回时 timestamp 窗口内可换 nonce 重放。方案：失败时也写 nonce 或先 nonce 去重再签名校验。Breaking：否。
- [ ] **R232-SEC-7 — JFIF+PDF 双容器绕过 PDF 检测以 KindImageInline 进入（P2）**: 方案：增二次魔数检测拒绝嵌套 PDF。Breaking：否。
### 代码质量 / 重构 — 本轮新发现

- [~] **R232-CR-3 — emitOverlapSkipped 发 back-to-back started→ended（设计意图归档 2026-05-23）**: 复核 scheduler.go:2429-2464 emitOverlapSkipped godoc 已明示设计意图——双事件发射故意为之，让 subscriber state machines 不丢失"started"锚点 (R233B-CR-2 引用)。状态字段 RunStateSkipped + ErrClassOverlapSkipped 让 dashboard 渲染为 no-op pill 而非 run timeline；synthetic RunID + StartedAt + skipPersist=true 避免污染 runs/<id>/。WS schema 加 Skipped bool 是 breaking 改造，且当前 ErrClass + State 已可推出该语义。归档关闭，本批 PR
- [~] **R232-CR-13 — dispatch unit test 走真 session.Router（归档 2026-05-23）**: 实地核查 `internal/dispatch/dispatch_test.go:126` `newTestDispatcher` 仍 `session.NewRouter(session.RouterConfig{MaxProcs: 10})`，全文件 50+ 处 `newTestDispatcher` 调用确认整体是集成测试，不是单元测试。方案需为 dispatch 引入 SessionRouter fake 接口，跨包重构面较大；P3 优先级 + 现有集成测试覆盖 send/queue/dedup 整链路有正面价值，作为长期改进项归档跟踪。
- [~] **R232-CR-14 — agent_tailer.attach 锁外逐条 SendJSON（NEEDS-DESIGN 归档 2026-05-23）**: 提议 `agent_history` 新 ServerMsg.Type 是 WS schema 加 type 即 breaking 客户端解析；attach 路径每 subscribe 1 次 cold path；既有 buffered 列表 ≤500 + SendJSON 已经异步入队 c.send chan 不阻塞 hub 锁。本批 PR 归档。
## Round 231 — 5-agent 并行 review 第 41 轮（2026-05-21）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描发现 ~80 条。本轮直接修 12 处（顶部摘要）。
> 以下是需设计决策、破坏兼容、跨包重构、或方案不唯一不适合本轮直接修的条目。

### 架构（高优先）— 本轮新发现

- [~] **R231-ARCH-1 — sysession.Runner 直 exec 旁路 CLI Wrapper 三层抽象（P1）**: `internal/sysession/runner.go` 自拼 `-p` argv、自 filterEnv、自 setting-sources，与 `cli.Wrapper.Spawn` 完全平行。新增 backend（Gemini ACP）必须在此再实现一遍。区别于 R230-ARCH-1（仅指出绕 backend.Profile），本条强调它把 CLI Wrapper 整层短路。方案：把 `RunOneShot` 抽进 `backend.Profile`，或让 Runner 走 `cli.Wrapper.Spawn(--collect-mode)`。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R231-ARCH-2 — 消息出口四套并行管线（P1，扩展 R230-ARCH-2/6）**: dispatch.sendAndReply / server.Hub.sendWithBroadcast / cron.scheduler.executeOpt / upstream.connector_rpc 四套 send 路径。cron 直持 platforms map、绕开 dispatch.replyText/queue/dedup；upstream 直调 sess.Send 绕开 MessageQueue/usermsg。Channel Adapter 不再是消息出口的唯一抽象。方案：抽 `internal/turnrunner` 或扩 `dispatch.Dispatcher` 为唯一 Send 协调器。
- [ ] **R231-ARCH-3 — session/router_core.go 顶部 blank-import history backend（P1）**: `claudejsonl/kirojsonl/naozhilog` 三个包的 init() 注入。注释自承"Sprint 1b 将合并到 wireup 包"。session 包想成为 backend-agnostic 的话必须迁出。方案：抽 `internal/wireup` 显式 `RegisterDefaults()` 由 `cmd/naozhi` 调用。Breaking：no（机械迁移）。
- [ ] **R231-ARCH-4 — Router god-object（60+ 方法 / 24+ 字段）（P1）**: 单结构体覆盖 7 大职责，5 处消费方手工裁剪 Reader/Writer 接口已出现 NotifyIdle/SetUserLabelWithOrigin 不对称（R230-ARCH-3）。方案：facet 化 `Router.Lifecycle()` / `Backends()` / `Stubs()` / `Overrides()`，每 facet 对应稳定接口。
- [~] **R231-ARCH-5 — Hub 与 Router god-object 双胞胎共同导致 Channel Adapter 抽象塌陷（P1）**: server.Hub 退化为第二个 Router（同时持 router/scheduler/scratchPool/queue/dedup/uploadStore/auth/tailers/nodes）；webhook 进来后路径从"Adapter→Router→Wrapper"变成"Adapter→{Hub/Server/Dispatcher} 三方共享状态"。方案：抽 WSEventBus / nodeRegistry / SendCoordinator 子聚合。 *(部分实施：PR #327 已 6-stage split wshub.go 文件层级；Router 端 PR #309 同 6-stage split scheduler.go；但 struct-level 的 SendCoordinator / SubscriberRegistry / WSEventBus 子聚合仍未抽出 — 跟踪到 R248-ARCH-6。)*
- [~] **R231-ARCH-6 — server.Hub.wiredLinkers 直持 cli.SubagentLinker（P1）**: Channel Adapter / 上层 server 强耦合 cli 包内部领域类型；ACP 等"无 SubagentLinker 概念"的 backend 上线时整条 agent-team UI 链路要么硬编码空实现要么走 nil 分支。方案：在 session 或 internal/agentlink 包定义 AgentLinker / AgentIntrospector 接口。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-ARCH-7 — cli.Wrapper.ShimManager 公开可变字段（P1）**: ShimManager 本应是进程级 singleton；multi-backend 部署 router 持 `wrappers map[string]*cli.Wrapper`，每 Wrapper 一个 ShimManager 副本（R230-ARCH-13 / R219-ARCH-4）。方案：定义 cli.Transport interface，shim/direct-exec 各一实现；Wrapper 拆 immutable BackendProfile + 共享 Transport。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R231-ARCH-8 — cli.Protocol 接口过宽（P2）**: 9 方法含 stream-json 专属能力 WriteUserMessageLocked / WriteInterrupt / SupportsPriority / SupportsReplay。ACP 这些方法必然 noop 或返回 ErrInterruptUnsupported。方案：缩为核心 7 方法 + passthrough/interrupt 下沉为可选 PassthroughExt / InterruptExt（type-assert）。Breaking：是。
- [ ] **R231-ARCH-9 — workspaceOverrides 与 sessions.json 双 JSON 分离（P2）**: 独立 dirty bit / gen counter / atomic write，部分失败导致重启后 session 引用了不在 overrides.json 的 chat workspace，无 reconciliation 路径（R219-ARCH-9）。方案：合成单文件 atomic write 或启动期一致性扫描修复。Breaking：是（store schema migration）。
- [ ] **R231-ARCH-10 — backend.Profile 注册表承诺与 wrapper.go 硬编码 switch 落差（P2）**: `cli/wrapper.go` `backendDisplayName` / `detectCLI` 仍硬编码 switch on "kiro" / "claude"，DESIGN.md L280 承诺已部分兑现但未到位。方案：通过 `backend.LookupProfile(id).DisplayName/.DefaultBinary` 获取，删除硬编码 switch；测试加 contract 锁。
- [ ] **R231-ARCH-11 — NewRouter 构造期副作用阻碍可测性（P2）**: NewRouter ~360 行内 load knownIDs / load workspaceOverrides / load sessions.json / 启动 N goroutine 异步加载 history / runOrphanSweep / startAttachmentTracker。测试无法单独构造 router 而不触发磁盘 IO + goroutine（R230-CQ-10）。方案：拆 `NewRouter`（仅 init 字段）+ `Router.Start(ctx)`。Breaking：是（构造方需迁移）。

### 安全 — 本轮新发现

- [~] **R231-SEC-1 — sysession.Runner 直 exec 不走 shimEnvAllowedPrefixes 白名单（归档 2026-05-23）**: 与 R231-ARCH-1 同根因（一旦 Runner 走 cli.Wrapper.Spawn 自动继承 shimEnvAllowedPrefixes + capExtraArgsBytes）。归并到 R231-ARCH-1 跟踪，本批 PR
- [~] **R231-SEC-2 — Dashboard 主页面 CSP `script-src 'unsafe-inline'`（P1，R229-SEC-6/R230-SEC-1 重申未修）**: 主页面允许任意内联 script，若 workspace 文件被污染且触发 XSS sink 即可执行任意 JS 窃取 session cookie。方案：迁 nonce/hash 模式，将 dashboard.html 内联 onclick 等事件外移为外部 JS。Breaking：是（前端模板需要重构）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-3 — `allowed_root` 缺失时不阻断公网监听启动（P1，R229-SEC-3 重申未修）**: dashboard_token 非空且监听非 loopback 但 allowed_root 为空时仅 Warn，认证用户可设 cron work_dir=/etc 让 CLI 向系统目录写文件。方案：fatal 启动失败 + naozhi doctor 加 HIGH 级别检查。Breaking：是（已部署但未配置 allowed_root 的部署需要迁移）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-4 — ExtraArgs 无 flag 允许列表（P1，R219-SEC-1/R229-SEC-1 重申未修）**: `protocol_claude.go:77` `args = append(args, opts.ExtraArgs...)`，dashboard-authenticated 用户可注入 `--mcp-config` / `--add-dir` 等改变 CLI 行为。方案：BuildArgs 加 flag 允许列表。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-5 — `serveRender/servePreview/serveRaw/serveDownload` Lstat 后再次 os.Open 的 inode-swap TOCTOU（P1，R219-SEC-2/R229-SEC-2 重申未修）**: 每个 mode 独立的 os.Open 都是新窗口。方案：handleFileGet 使用 OpenFile 拿到 fd，下游直接消费 fd；或加 Fstat 验证 inode。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-6 — sessions.meta.json 非原子写（P2，R230-SEC-4 重申未修）**: `internal/session/store.go` 用单次 os.WriteFile 而非 osutil.WriteFileAtomic，部分写失败时半截 JSON 导致重启后 session 历史不可用。方案：改用 osutil.WriteFileAtomic。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-7 — XDG_ 前缀过宽放行（P2，R230-SEC-2 重申未修）**: shim/manager.go 放行 `XDG_*` 整族，理论上可重定向 CLI 配置/数据查找路径。方案：精确白名单 XDG_RUNTIME_DIR= / XDG_CACHE_HOME= / XDG_STATE_HOME=。Breaking：是（contract 测试需更新）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R231-SEC-8 — feishu webhook 仅靠 plaintext VerificationToken（P2）**: nonce 已强制；但 v1 仅 VerificationToken 模式下 token 是 plaintext shared secret，泄漏后 5 分钟 replay 窗口内自由重放。方案：强制要求配 EncryptKey 或将 token-only 标 deprecated 并 startup Warn 提升级别。
- [~] **R231-SEC-9 — 单 token 可建 500 个 WS（P2，R229-SEC-8 重申未修）**: maxConnectionsPerServer=500 但无 per-token/per-cookie-bucket 子上限。方案：WS 升级时按 cookie MAC 或 Bearer SHA-256 设 per-token 上限（如 20）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-10 — 反向 Node 连接通过 `ws://` 明文（P2，R229-SEC-5 重申未修）**: 部署环境未有 TLS 卸载代理则 token 中间人截获。方案：`/ws-node` handler 检查 r.TLS + 可信 X-Forwarded-Proto:https，无则拒绝 Upgrade，或显式豁免 + 文档。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-11 — `/static/dashboard.js` 不走 requireAuth（P2，R230-SEC-3 重申未修）**: 中间人可替换 JS 文件后客户端窃取 dashboard token。方案：dashboard_token 非空时对静态 JS 端点加 requireAuth。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
### 性能 — 本轮新发现

- [~] **R231-PERF-1 — Protocol.ReadEvent string→[]byte 反向拷贝（多轮 NEEDS-DESIGN 归档 2026-05-23）**: R67-PERF-1 起跨 ~30 轮 review 重申，方案 Breaking 接口签名改造。50 sess × 50 evt/s 量级是已知开销但非紧急。同根因 R232-PERF-1 / R233-PERF-1 / R233B-PERF-1 / R67-PERF-1 / R226-PERF-1 一并归档 NEEDS-DESIGN，本批 PR
- [ ] **R231-PERF-2 — ACP parseSessionUpdate 双 Unmarshal（P1）**: agent_message_chunk 分支每帧两次 json.Unmarshal，kiro 每 token 一帧最热路径。方案：bytes.Contains 快速判断或合并为一次解析。
- [~] **R231-PERF-4 — ACPProtocol.textBuf 锁竞争（误报关闭 2026-05-23）**: 实地复核 protocol_acp.go textBuf 是 ACP 单 reader（readLoop）路径串行写入 + WriteMessage turn boundary Reset 的设计；mu 仅覆盖 textBuf 本身，sessionID 已分离到 atomic.Pointer 后 mu 的争用窗口被收窄。lock-free 改造需重设计 acp turn boundary 协议，不在简单修范围。归档关闭，本批 PR。
- [~] **R231-PERF-6 — BroadcastSessionsUpdate AfterFunc 创建新 timer（NEEDS-DESIGN 归档 2026-05-23）**: wshub.go:1430-1431 已有 timer.Stop()+Reset 复用路径覆盖密集分支；AfterFunc 仅在 quiet→active 转换时分配新 timer——稀疏路径。预分配 NewTimer 需重 Shutdown 协议（drain timer + clientWG.Done 时序），改造成本远超 alloc 收益。本批 PR 归档。
- [ ] **R231-PERF-7 — ACP readUntilResponse 每握手 3 次 goroutine + 3 chan alloc（P2）**: 握手 3 次 = 9 次。方案：握手 goroutine 提升为长寿命，仅在握手阶段循环；或 done chan→atomic.Bool。
- [ ] **R231-PERF-8 — Cleanup 在 r.mu 内做整 sessions map copy（P2）**: O(N) 拉长持锁时间。方案需保持 saveStore 的稳定 snapshot 语义（不能拆 keys → 释放锁 → 再 RLock 因为竞态），或者转为 RCU/COW snapshot。需独立设计。
### 代码质量 — 本轮新发现

- [ ] **R231-CQ-1 — claude reconnect 路径双注入（P1，PR #202 复盘）**: `router_shim.go:439` 直接 `proc.InjectHistory(histEntries)`，随后 `ReattachProcessNoCallback` 调 `attachProcessAndSnapshotPersisted` 把 `sess.persistedHistory`（已由 tier1/tier2 异步 goroutine 通过 `sess.InjectHistory` 填充）snapshot 再次注入同一 proc。两批高度重叠时 EventLog 翻倍。方案：line 439 改为 `sess.InjectHistory(histEntries)` 走 persistedHistory + seededLen 流向，与 kiro 路径行为一致。需对照 #202 PR 测试与 EventLog 去重逻辑验证。
### Go 正确性 — 本轮新发现

## Round 219 — 5-agent 并行 review 第 33 轮（2026-05-17）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 100 条发现。
> 13 条 FIX-READY 已落地本轮 PR（uuid hex alloc / eventlog_bridge pooled encoder /
> EventEntriesFromEventAt empty fast-path / pendingIdx flush 后 cap shrink /
> config permission 0o077 / interruptAcquireTimeout 命名 / SessionConfig.Workspace
> godoc Deprecated / cron recordResult Sprintf / maxWebhookNonceLen 命名 /
> ErrUnsupportedPlatform 单点 / dashboard_send.go path.Clean 注释）。
> 以下是需设计决策、破坏兼容、跨包重构、或方案不唯一不适合本轮直接修的条目。

### Go 正确性 — 本轮新发现

- [ ] **R219-GO-1 — `cli.Resolve` 在 `resolveSem` 满时 acquire 无 ctx select arm（P2）**: `subagent_link.go:323` `Resolve` 通过容量 8 的 channel 限并发，但 acquire 路径 `resolveSem <- struct{}{}` 在 sem 满时无 ctx select arm，进程被 Kill 时该 goroutine 永久阻塞。方案：`select { case l.resolveSem <- ...: case <-ctx.Done(): return }`。涉及：`internal/cli/subagent_link.go:323`。Breaking：是（Resolve 接受 ctx 参数）。
- [~] **R219-GO-2 — `reconnectShims` replay 段 `linker.Resolve` goroutine 无 ctx 绑定（P2 重申 R218B-GO-3 未覆盖分支）**: R218B-GO-3 仅覆盖 `process_readloop.go:324`，`router.go:1469` reconnectShims 路径下的 `go linker.Resolve(...)` 同样裸 goroutine。startup 期 SIGTERM 到来时该批 Resolve 不会被取消，最多 3s 后退出延迟 shutdown。方案：和 R218B-GO-3 同步修复。涉及：`internal/session/router.go:1469`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR

### 安全 — 本轮新发现

- [ ] **R219-SEC-1 — `BuildArgs` `opts.ExtraArgs` 无 flag 允许列表（P1）**: `protocol_claude.go:77` `args = append(args, opts.ExtraArgs...)`，dashboard-authenticated 用户可注入 `--mcp-config`、`--add-dir`、`--skip-permissions` 等改变 CLI 行为的 flag。区别于 R217-SEC-1（`--append-system-prompt`），本条强调 `--mcp-config` 类可加载攻击者控制 MCP 服务器定义的 flag。方案：在 BuildArgs 加 flag 允许列表，拒绝列表外以 `--` 开头的 element。Breaking：是（依赖任意 extra args 的运维方需要迁移）。
- [ ] **R219-SEC-2 — `serveRender` 在 Lstat 后第三次 os.Open 制造 inode-swap TOCTOU（P1）**: `handleFileGet` 已有 R218B-SEC-2 Lstat-after-resolve 防御，但 `mode == "render"` 走 `serveRender` 时再次 `os.Open(resolved)`，inode swap 攻击仍可绕过。方案：`serveRender` 使用 Lstat 时已 Open 的 fd 或加 `Sys().(*syscall.Stat_t).Ino` 验证。涉及：`internal/server/project_files.go:667`。
### 性能 — 本轮新发现 / 重申

- [~] **R219-PERF-1 — `eventPushLoop` 同 session N tab 各自 marshalPooled 独立序列化（P2 重申 R214-PERF-4）**: 同 key 多客户端 fan-out 时未做单次序列化共享。方案：在 eventPushLoop 把同 key 全部 clients 集中在 broadcast goroutine 里序列化一次再 fan-out SendRaw。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R219-PERF-2 — `handleList` storeGen 未变化时未短路重建 sessionWorkspaces map（归档 2026-05-23）**: 同 R215-PERF-P2-5 / R222-PERF-6 / R224-PERF-2 主条目跟踪。响应内容包含动态 uptime/now 字段，每秒变化与 storeGen 无关，缓存命中无法跳过；细粒度缓存需拆分静态/动态分段，工作量超 P2 预算。本批 PR 归档。
- [~] **R219-PERF-3 — `Snapshot()` 顺序读 8 次 atomic.Pointer.Load（P2 重申 R215-ARCH-P2-7）**: 1 Hz × 10 tab × 50 session = 4000 Load/s。方案：把构造后不变字段（backend/cliName/cliVersion/userLabel）打包 `immutableBox struct` + 单次 atomic.Pointer.Load。涉及：`internal/session/managed.go:861`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R219-PERF-4 — `invokePersistSinkSingle` 栈数组方案需 benchmark 证伪（P2）**: Append 单条路径 `[]EventEntry{e}` heap escape，本轮尝试用 `[1]EventEntry` 栈数组 + `s[:]` 但因 PersistSink 是 atomic.Pointer 函数指针调用，逃逸分析会强制 slice 逃逸到 heap，理论收益不确定。需 -benchmem 验证；若无效则降级为接受现状或换 sync.Pool 方案。涉及：`internal/cli/eventlog.go:640`。 — NEEDS-DESIGN 归档 2026-05-23（与 R222-PERF-8 / R215-PERF-P2-1 / R228-PERF-7 同根因；R230-PERF-1 sink-nil 早返回已覆盖生产热路径，sink-attached 路径的 slice 字面量受 PersistSink 保留契约约束结构性必需；本批 eventlog.go 加 godoc 锚点）

### 代码质量 — 本轮新发现

- [ ] **R219-CR-7 — `dispatch.sendAndReply` 241 行 5+ 职责（P2）**: 类同 R214-CODE-3 (readLoop)。方案：抽 `buildReplyContext` + `handleSendResult` helpers。
- [ ] **R219-CR-8 — `shim/server.go::handleClient` 319 行无子拆（P2）**: 4 个内联 goroutine 通过裸 channel 通信。方案：抽 `handleClientHandshake / relayStdin / relayStdout`。
- [ ] **R219-CR-9 — `processIface.GetState/GetSessionID` 违反 Go 命名约定（P2）**: 应去 `Get` 前缀。Breaking：是（接口变更，~12 处 callsite + mock 需改）。

### 架构 — 本轮新发现 / 重申

- [~] **R219-ARCH-1 — `cli.EventEntry` / `cli.Event` / `cli.AskQuestion` 已塌陷为跨层 DTO（P1 R217-ARCH-1 未覆盖分支）**: history.Source.LoadBefore 把"事件领域类型"硬编码 cli 包导出类型，未来迁出 cli 内部领域类型时该接口也需重写。方案：整批迁到 `internal/event`（叶子包零依赖）。Breaking：是（接口签名 + 26+ 包 import 路径变动）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R219-ARCH-2 — `session/router.go:22-25` 顶部硬编码 4 个 backend-specific history 包 import（P1）**: 任意新 backend 加 history source 必须改 session 包，session 永远无法成为协议无关调度层。方案：cli.Wrapper 增 `NewHistorySource(s ManagedSession) history.Source` 工厂方法，session 只 import history.Source 接口。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R219-ARCH-3 — `server.Hub.wiredLinkers map[*cli.SubagentLinker]struct{}` 持 cli 内部对象指针（P1 R217-ARCH-2 未覆盖分支）**: Linker 内部字段调整会让 server 的 once-only wiring 假设失效；ACP 等无 SubagentLinker 概念的 backend 上线时整条 agent-team UI 链路要么硬编码空实现要么走 nil 分支。方案：定义 `session.AgentIntrospector` interface。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R219-ARCH-4 — `cli.Wrapper` 公开可变字段持 `*shim.Manager`（P1 R214-ARCH-9 未覆盖分支）**: ShimManager 应是进程级单例，但 multi-backend 部署 router 持 `wrappers map[string]*cli.Wrapper` 时每 Wrapper 一个 ShimManager 副本——而 ShimManager 本应管 socket/cgroup 路径单例。方案：Wrapper 拆 immutable BackendProfile + 单例 ShimManager。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R219-ARCH-5 — `Hub.scheduler/uploadStore/scratchPool` 三 setter 在启动后注入造成"半构造对象"（P1）**: 8 处 `if h.scheduler != nil` / `if h.scratchPool != nil` 守卫维系隐式协议。方案：HubOptions 加这三字段，构造期 dashboard.go 重排 wiring 顺序（uploads 先于 NewHub），或以 null-object 接受 nil。
- [ ] **R219-ARCH-6 — `Protocol.WriteUserMessageLocked / WriteInterrupt / SupportsPriority/Replay` stream-json 专属能力漏到接口层（P2）**: ACP 实现必然 noop 或 panic。方案：Protocol 缩成 7 方法核心，passthrough 下沉到 `PassthroughExt` 可选 interface + type-assert。Breaking：是。
- [ ] **R219-ARCH-7 — `processIface` 30+ 方法 god 接口具体拆分建议（P2 R215-ARCH-P1-3 已登记，本轮提供具体方案）**: 拆 `ProcessLifecycle` (6方法) + `EventSource` (7方法)，stream-json 5 方法走 `loadCliProcess()` type-assert（已有先例 managed.go:973）。
- [ ] **R219-ARCH-8 — `cron.scheduler` 直持 `platforms map[string]platform.Platform` + 直调 Reply/MaxReplyLength/SplitText（P2）**: cron 越层访问 IM 出站绕过 dispatch.replyText 统一错误处理。方案：cron 持 `Notifier interface { Notify(ctx, plat, chatID, text) error }`，server 注入实现。涉及：`internal/cron/scheduler.go:1864`。
- [ ] **R219-ARCH-9 — `workspaceOverrides` 与 `sessions.json` 双 atomic write 不一致风险（P2）**: 两个独立 dirty bit/gen counter/atomic write，部分失败导致重启后 session 引用了不在 overrides.json 的 chat workspace，无 reconciliation 路径。方案：合成单文件 atomic write（sessions.json schema 加 workspace_overrides 字段）或启动期一致性检查。Breaking：是（store schema migration）。
- [ ] **R219-ARCH-10 — `--dangerously-skip-permissions` hardcode 在 Protocol 层（P2 R215-SEC-P1-1 架构视角）**: 应该是 `BackendProfile` 或 `SpawnOptions.PermissionMode` 字段。方案：`SpawnOptions.PermissionMode {Skip, Default, AutoGrant}`，Protocol.BuildArgs 据此决定参数。Breaking：否（增量字段零值兼容）。
- [ ] **R219-ARCH-11 — `discovery.DefaultScanner` package singleton 阻碍多租户隔离（P2 R172-ARCH-D9 重申）**: 自 R172 登记起未推进，server.discoveryCache 与 cmd/naozhi 都通过包级 wrapper 假设进程内单 scanner。方案：`RouterConfig.HistoryScanner *discovery.Scanner`，nil 走 DefaultScanner 兼容老路径。

## Round 230 — 5-agent 并行 code review（2026-05-21）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）跑了全仓 review。本轮 12 处直接修已落 PR；以下为 breaking / 跨模块 / 需设计决策的发现登记追踪。

### Architecture（架构债）

- [~] **R230-ARCH-1 — sysession.Runner 直 exec `claude -p` 旁路 cli.Wrapper（P1）**: `internal/sysession/runner.go` 每次 Run 起新 claude -p 子进程，绕过 backend 选择 / shimEnvAllowedPrefixes / `--setting-sources ""` / ARG_MAX 守卫。新增 backend (Gemini) 需在此重做一遍。方案：`backend.Profile.RunOneShot(ctx, prompt) (string, error)` 抽接口由 Runner 复用。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230-ARCH-2 — Hub.ownerLoop / runTurn 与 dispatch.Dispatcher.ownerLoop / sendAndReply 几乎逐行重复（P1）**: `internal/server/send.go` 与 `internal/dispatch/dispatch.go` 各一份 collect-timer + drain + Discard/recover 实现，"对齐 dispatch.ownerLoop" 注释自承。方案：抽 `TurnRunner` 或把 dashboard 走 LocalChannel 适配器纳入 dispatch。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230-ARCH-3 — 五个 SessionRouter 子集接口手工对齐易漂移（P1）**: dispatch / cron / sysession / server.HubRouter / upstream 各持一份，已出现 NotifyIdle / SetUserLabelWithOrigin 不对称。方案：在 session 包合成 `Reader/Writer/Lifecycle` 三联，消费方按需 compose。Breaking：否（接口断言迁移）。
- [ ] **R230-ARCH-4 — session.Router 60+ 方法跨 7 大职责（P1）**: 每消费方都能触达全量。方案：facet 化 `Router.Backends() / Lifecycle() / Stubs()`。Breaking：否（增量 facet）。
- [ ] **R230-ARCH-5 — server.Hub 45 方法 24 字段第二 Router（P2）**: 同时持 router / scheduler / scratchPool / queue / dedup / uploadStore / auth / tailers / nodes，nodesMu 与 Server 共指针为耦合 smell。方案：抽 WSEventBus / SendCoordinator / AgentTailerSet / nodeRegistry。Breaking：是。
- [~] **R230-ARCH-6 — upstream 反向 RPC 第三套 send 管线（P1）**: `connector_rpc.go` 直 `sess.Send`，绕过 MessageQueue / dedup / usermsg / replyError 计数，反向流量在监控里不可见。方案：让 upstream 走共享 TurnRunner / Dispatcher.Send。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230-ARCH-7 — 错误→用户消息映射有 3 处偏序（P2）**: `usermsg.ForSendError` 是规范源，但 dispatch.sendAndReply 仍内联 ErrMaxProcs / ErrMaxExemptSessions / 超时分支，apierr.localizeAPIError 是第四套。方案：超时参数注入 usermsg 或 dispatch 侧 helper，dispatch 内联 switch 收敛。Breaking：否。
- [ ] **R230-ARCH-9 — KeyResolver 三处独立实例（P2）**: cmd/naozhi/main.go upstreamResolver / Server.resolver / Dispatcher fallback 三份缓存，project 变更后异步漂移。方案：projectMgr 暴露 Resolver() 单点。Breaking：是（构造签名）。
- [ ] **R230-ARCH-10 — plannerKey* 在 session/key.go 与 project 包重复（P2）**: 注释自承"hardcoded test assertion synced"。方案：抽 `internal/keys` 中性包，两侧 import。Breaking：否。
- [ ] **R230-ARCH-11 — dashboard.js PLATFORM_ORIGINS 硬编 IM 列表（P2）**: 新增平台需 4 处同步：adapter / main.go initPlatforms / dashboard.js / dashboard.html CSS。方案：`GET /api/platforms` 返回 `{id, displayName}[]`，前端启动时 hydrate。Breaking：是（前端模板）。
- [~] **R230-ARCH-12 — Dispatcher 实例只在 main.go 串到 Feishu，Hub 仅借 MessageQueue 引用（归档 2026-05-23）**: 实地核查 `internal/server/server.go:778` `dispatch.NewDispatcher` 实际在 server 包构造（不在 main.go），条目原描述位置不准。但根问题"dashboard send 与 IM send 两套抽象层"与 R230-ARCH-2 / R230-ARCH-6 / R231-ARCH-2 同根因（消息出口管线分裂），统一收敛到主条目 R231-ARCH-2 跟踪。
- [ ] **R230-ARCH-14 — internal/session/router_core.go 用 blank import 注册 history factory（P2）**: Sprint 1b 注释自承"将合并到 wireup 包"。任何 Router 测试都触发全局 registry 改动。方案：`internal/wireup/wireup.go` + 显式 RegisterDefaults() 在 main.go 调。Breaking：否（迁移）。
- [ ] **R230-ARCH-15 — internal/server 90+ Go 文件单包（P3）**: 按 handler-group struct 切分而非 Go package 边界。方案：下次触动 auth/cron/scratch handlers 时迁子包。Breaking：是（import path）。
- [ ] **R230-ARCH-16 — Router.spawnSession 直接调 *cli.Wrapper.Spawn（P3）**: cli.Wrapper 是 struct 非 interface，使 Router 单元测试需 panicSafeSpawn 替身。方案：`cli.Spawner interface { Spawn(ctx, opts) (Process, error) }`。Breaking：否。
- [ ] **R230-ARCH-17 — internal/cli 65+ 文件混 protocol/process/eventlog（P3）**: 同 R230-ARCH-15 同性质。方案：subpackage 化 cli/protocol cli/eventlog cli/subagent。Breaking：是。
- [ ] **R230-ARCH-18 — Router.StartCleanupLoop / StartShimReconcileLoop 由 main.go 各起 goroutine（P2）**: Router 既不是完整 Run 服务也不是纯被动 struct，未来调用方易漏 Tick。方案：合并 `Router.Run(ctx)` 一次启动所有循环。Breaking：是。
- [ ] **R230-ARCH-19 — Cron / Sysession / dashboard 各自一套 stub session 注册策略（P3）**: RegisterCronStub / RegisterCronStubWithChain / RegisterSystemStub 三方法 + exemptKeyPrefixes 链式 HasPrefix。方案：`StubKind` enum + 单一 RegisterStub。Breaking：是。
- [ ] **R230-ARCH-20 — Server ↔ Hub 共享 nodesMu *sync.RWMutex 别名（P2）**: 表明 nodes 应独立 owner（`*node.Registry`）而非任一方持。方案：抽 nodeRegistry，Server 与 Hub 按 interface 消费。Breaking：是（构造）。

### Code quality（剩余）

- [ ] **R230-CQ-2 — sendAndReply 241 行 5+ 职责（P2）**: 内含 5s 超时 nested defer，复杂度高。方案：抽 buildReplyContext / handleSendResult。Breaking：否。
- [~] **R230-CQ-4 — processIface.GetState/GetSessionID 命名违 Go 风格（P2 R219-CR-9 重申）**: 12 处调用点 + 两个 fakes 需机械重命名。方案：State()/SessionID()。Breaking：是（interface 改名）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230-CQ-6 — validateCronTitle 单独实现 UTF-8 + C0 + IsLogInjectionRune（P2）（评估关闭 2026-05-23）**: 与 validateStringField 三重扫描重复。方案：stringFieldPolicy 加 singleLineError bool。Breaking：否。 — 评估：dashboard_cron.go:364-368 已有显式 godoc 解释**为什么不接入** `validateStringField`：把单行专用错误消息分支（"title must be a single line" vs "contains invalid control characters"）和 rune-级 vs byte-级长度计量混入 stringFieldPolicy 会反向把 4 个 cron 验证器都污染成"如果支持单行就额外提示"的样板。已显式决策不做，本批 PR 关闭归档。
- [~] **R230-CQ-8 — reconnectShims case 内 90+ 行内联（P2 R229-CR-3 重申）**: 仍未抽 processDiscoveredShim 子函数。Breaking：否。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230-CQ-9 — cron.executeOpt 329 行 7+ 错误分支（P2 R229-CR-1 重申）**: handleSendError / deliverAndRecord 抽取。Breaking：否。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230-CQ-10 — NewRouter 359 行内联三阶段初始化（P2 R229-CR-2 重申）**: newRouterRestoreSessions / newRouterStartHistoryLoads 抽取。Breaking：否。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230-CQ-14 — cron/scheduler.go 2745 行单文件无拆分计划（P3 R226-CR-11 重申）**: 建议先建 scheduler_job.go / scheduler_run.go / scheduler_notify.go 骨架。Breaking：否。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
### Security（剩余 — 本轮新发现）

- [~] **R230-SEC-1 — Dashboard CSP `script-src 'unsafe-inline'`（P1 R229-SEC-6 重申）**: 主页面 CSP 仍允许任意 inline；登录页已用 SHA-256 hash 严格 CSP。方案：迁 nonce 模式，把 dashboard.html 内联 onclick 等事件外移；KaTeX/mermaid 通过 createElement+SRI 注入已不依赖 unsafe-inline。Breaking：是（前端较大改造）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230-SEC-2 — shim XDG_ env 前缀过宽（P2）**: `shim/manager.go:900` `XDG_` 前缀放行 XDG_CONFIG_HOME / XDG_CONFIG_DIRS / XDG_DATA_DIRS，理论上可重定向 CLI 配置/数据查找。当前测试契约（manager_test.go:517）显式允许 XDG_CONFIG_HOME，调整需同步重写测试。方案：精确 `XDG_RUNTIME_DIR=` `XDG_CACHE_HOME=` `XDG_STATE_HOME=`，剔除 CONFIG/DATA。Breaking：是（测试契约 + 部署期依赖 XDG_CONFIG_HOME 转发的运维方）。
- [ ] **R230-SEC-3 — Dashboard JS 静态资源未鉴权（P2）**: `/static/dashboard.js` 与 `/static/agent_view.js` 不走 requireAuth，HTTP 部署下中间人可替换。方案：dashboardToken 非空时对 JS 端点也加 requireAuth；或文档化 TLS 必备。Breaking：否（鉴权后浏览器需先认证才能加载，与 SPA 流程一致）。
### Go / Concurrency（剩余 — 本轮新发现）

### Performance（剩余 — 本轮新发现）

## Round 230B — 5-agent 并行 code review（PR #198）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）跑了全仓 review。本轮 17 处直接修已落本轮 PR；以下为非 breaking 但需要更大改动 / 需设计决策 / 跨模块的发现，登记追踪。

### Security（剩余）

- [ ] **R230B-SEC-2 — Backend ID charset 三处不一致（P2）**: `dashboard_cron.go:198` (`[a-z0-9_-]`) vs `select_node_for_backend.go:46` WS (`[a-zA-Z0-9_.-]`) vs `send.go:266` HTTP (`[a-z0-9_-]`)。本轮已收敛 maxCronBackendLen → maxBackendIDLen 但 charset 策略未统一。方案：决定是否允许大写 + `.`，统一到包级 `isValidBackendID` 一处。Breaking：是（如现有 backend ID 含大写或 `.`）。
- [ ] **R230B-SEC-3 — `cli.backends[*].args` 缺 flag 允许列表（P3）**: `validateArgvStrings` 已拒控制字节但允许任意 `--flag`。方案：与 R229-SEC-1 同批引入 flag allowlist。Breaking：是。
- [ ] **R230B-SEC-5 — dashboard CSP `img-src data:` 防御缺口（P3）**: 数据 URI 允许 SVG-with-script + 外发 GET 探针。方案：移除 `data:`；审计 dashboard.js 改用 blob URL。Breaking：是。

### Go 正确性 / 并发（剩余）

- [ ] **R230B-GO-2 — `subagent_link.Resolve` retry sleep 不响应 ctx 取消（P2）**: `subagent_link.go:294/332` 重试循环 `time.Sleep` 不察 ctx；router_shim.go:398 `go linker.Resolve` bare goroutine。方案：Resolve 加 ctx 参数 + select stop signal。Breaking：是（接口签名 + 调用方）。
- [ ] **R230B-GO-3 — `recordResultP0WithSanitised` / `recordResult` mu Unlock 非 deferred（P2）**: 多个 early-return 各自手动 Unlock，未来插早返路径易遗漏。方案：拆 stateMutate（持锁）+ stateCommit（锁外 save/fn 调用）两阶段。Breaking：否（内部重构）。
- [~] **R230B-GO-4 — `Hub.handleSubscribe` O(N) maxSubscribersPerKey 扫描（归档 2026-05-23）**: 已部分修复——R230C-PERF-4 引入 early termination 当 count 到达 maxSubscribersPerKey 即 break，worst-case O(20) 而非 O(maxWSConns=500)；handleSubscribe 是冷路径不在每事件扇出。维护单独 counter map 需在 disconnect 路径 +/- 引入第二个不变量，收益小风险大。本批 PR 归档。
### Performance（剩余）

- [~] **R230B-PERF-1 — `wshub.eventPushLoop` 同 session N 个 tab 各自 marshalPooled（P1）**: `wshub.go:1099` 同批事件 N tab 时 marshal 成本 O(N)。方案：marshal once → SendRaw 字节 fan-out。Breaking：否。R219-PERF-1 / R225-PERF-9 重申。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230B-PERF-2 — `Snapshot` `proc.TurnAgents()` 始终 alloc（P2）**: count==0 已短路，count>0 时仍 make+copy。方案：`TurnAgentsBuf(dst)` 接受 caller slice 复用，或 SessionSnapshot 内嵌固定 4-元数组。Breaking：否。R225-PERF-6 重申。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230B-PERF-3 — `ListSessions` SessionSnapshot slice 1Hz 持续分配（P2）**: 50 sessions × 1 Hz × N tab。方案：handleList 加 storeGen 缓存或 sync.Pool 池化结果。Breaking：否。R229-PERF-10 重申。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230B-PERF-5 — `subagent_transcript.readLocked` 每次 open+seek+ReadAll（P2）**: 50 tailer × 1s = 50 syscall/s。方案：保持 fd open + offset 增量；inotify 选项后续讨论。Breaking：否。 — 归档 2026-05-23 (superseded by R233-PERF-4)：R232-PERF-3 已加 readBuf 复用消除 ReadAll 增长复制；剩余 open/close-per-poll 由 R233-PERF-4 持久化 fd + ReadAt + inode 失效统一规划，本条不再独立跟踪；本批 subagent_transcript.go 加 godoc 锚点
- [ ] **R230B-PERF-6 — `eventlog_bridge` 单条快路径仍 copy raw bytes（P2）**: bridge 即使 single entry 仍 make+copy。方案：核对 Persister 留持契约，能 zero-copy 则免拷。Breaking：否（需仔细审 contract）。
- [~] **R230B-PERF-8 — `notifySubscribers` map iteration vs slice（P3）**: subCount==1 极常见，map range 不必要。方案：count==1 fast path 直接取 + count<=4 时 slice 存储。Breaking：否。 — 评估归档 2026-05-23：Go runtime mapiterinit+mapiternext on 1-bucket map ~tens of ns，RLock/RUnlock 是 either way 都付的成本主导项；slice 存储破坏 Subscribe/Unsubscribe + closeOnce 契约，net gain sub-percent，不划算。eventlog.go notifySubscribers godoc 锚点说明决策；若未来 5000+ session dashboard 重连成为热点，方向是 ring buffer 替换 map 而非 micro-branch，本批 PR

### Code 质量（剩余）

### Architecture（剩余 P1，需设计）

- [~] **R230B-ARCH-1 — `session` 包硬编码 import 4 个 backend-specific history 包（P1）**: `router_core.go:18-32` blank-import claudejsonl/kirojsonl 触发 init 注册，session 是协议无关层假设破裂。方案：cli.Wrapper.NewHistorySource 工厂封装；session 仅依 history.Source 接口。Breaking：是（~20 callsite）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230B-ARCH-2 — Hub/Server 半构造对象反模式（P1）**: 8+ 处 `if h.scheduler != nil` 守卫散落；Set* setter 注入顺序硬编码。方案：HubOptions 一次性装配 + null-object fallback。Breaking：否（内部重排）。立即可落地（~30 行）。
- [ ] **R230B-ARCH-3 — `cron.scheduler` 越层直接持 platform map + SplitText（P1）**: 与 dispatch 平行第二条 IM 出站路径，错误处理重复。方案：注入 Notifier interface。Breaking：否（cron 内部 + 兼容 fallback）。
- [~] **R230B-ARCH-4 — `cli.Protocol` 接口塌陷为 stream-json 专属（P1）**: 8 方法中 4 个 ACP 必 noop/panic。方案：拆核心 Protocol + PassthroughExt 可选接口。Breaking：是（cli.Protocol 导出 + 6 处 type-assert）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230B-ARCH-5 — `cli.EventEntry` 已塌陷为跨层 DTO（P1）**: 26+ 包 import cli.EventEntry。方案：迁到叶子包 internal/event 零依赖。Breaking：是（接口签名）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230B-ARCH-6 — `sessions.json` + `workspace-overrides.json` 双 atomic write 不一致（P1）**: 部分失败下重启出现孤立 override。方案：合并 schema 单文件原子写或启动期一致性扫描。Breaking：是（schema migration）。立即可落：启动期扫描 ~30 行。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230B-ARCH-7 — `server` 包 backend fan-out 失控（P1）**: server.go 单文件 import 10 个 internal 包，是"god 包"伪装。方案：抽 internal/app 装配包，server 还原为纯 HTTP handler。Breaking：否（内部）。
- [~] **R230B-ARCH-8 — `cli.Wrapper` 持 `*shim.Manager` 单例假设破裂（P1）**: multi-backend 部署每 Wrapper 一份 ShimManager 副本可能撞同一系统资源。方案：BackendProfile + 共享 ShimManager 注入。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230B-ARCH-9 — Hub 持 `*cli.SubagentLinker` 内部对象指针（P1）**: 跨包内部对象暴露。方案：定义 AgentIntrospector interface。立即可落地（~50 行）。Breaking：否。
- [ ] **R230B-ARCH-10 — server / dispatch / cron 三处持 `map[string]platform.Platform`（P1）**: 每加新平台改 3 处。方案：抽 `platform.Registry` 类型聚合 nil 守卫 + fallback 文案。Breaking：否。立即可落地（~80 行）。
- [ ] **R230B-ARCH-11 — `cli` 包内 7 子领域并存（P2）**: ShimManager / SubagentLinker / EventLog / Wrapper / Protocol / passthrough / Process。方案：拆 cli/{process,protocol,eventlog,passthrough,subagent} 子包。Breaking：否。
- [ ] **R230B-ARCH-12 — `ScratchPool` 应是 server 关注（P2）**: 实现在 session 包但唯一 caller 在 server。方案：搬到 server 或让 session.Router 自管 ephemeral 池。Breaking：是。
- [ ] **R230B-ARCH-13 — `discovery.DefaultScanner` package singleton（P2）**: 阻碍多租户隔离。方案：RouterConfig.HistoryScanner 字段 + nil fallback。立即可落地（~20 行）。Breaking：否。
- [ ] **R230B-ARCH-14 — `cron.notifyTarget` 错错误未走 usermsg 中文映射（P2）**: 仅 dispatch 和 server.send 用 usermsg.ForSendError；cron 出错只 slog.Warn 不回写 chat。方案：扩展 usermsg.ForCronError 或 dispatch 收回错误映射独占。Breaking：否。
- [ ] **R230B-ARCH-15 — `processIface` 30+ 方法 god interface（P2）**: 方案：拆 ProcessLifecycle / EventSource / ProcessSender。Breaking：是。
- [ ] **R230B-ARCH-16 — Dashboard 装配顺序硬编码多处（P2）**: dashboard.go SetXxx 顺序 + cmd/naozhi/main.go 各自构造。方案：抽 internal/app/wire.go 单点拓扑。Breaking：否。立即可落地。
- [ ] **R230B-ARCH-18 — `--dangerously-skip-permissions` hardcode 在 Protocol（P2）**: 多用户/多 chat 无法 per-session 切权限。方案：SpawnOptions.PermissionMode 枚举。立即可落地（~30 行）。Breaking：否。
- [ ] **R230B-ARCH-19 — validateStringField 三重扫描（UTF-8+C0+Bidi）重复（P2）**: cron 路径已抽 helper，feishu/project planner 仍各自重复。方案：textutil.ValidateText(s, policy) 统一。Breaking：否。
- [ ] **R230B-ARCH-20 — node 反向 RPC 协议三处定义（P2）**: node/protocol.go / connector / wshub 各自手写编解码。方案：node/rpcprotocol 子包统一。Breaking：否。
- [ ] **R230B-ARCH-23 — selfupdate 无回滚 / 健康检查 hook（P3）**: panic 后只能 ssh 手动回退。方案：systemd 启动 30s 内 self-call /health 失败自动 .prev 回退。Breaking：否。
- [ ] **R230B-ARCH-24 — Server struct 30+ 字段 god object（P3）**: 已抽 *Handlers struct 但仍持每个指针。方案：mountAuth(mux)/mountCron(mux) 子构造，Server 退成 Listener+middleware+Mux 容器。Breaking：否。
- [~] **R230B-ARCH-25 — EventLog SetPersistSink 时序契约靠 metric 兜底（误报关闭 2026-05-23）**: 复核 cli/eventlog.go:309-330 现状已通过 godoc + sinkReady atomic.Bool 双 stage 完整说明协议：sinkReady 初始 false，所有 Append/AppendBatch 在 false 期间标 replayPhase=true，Persister 主动 drop。SetPersistSink 同时 Store sink 指针 + sinkReady=true。godoc 已显式 "SetPersistSink must run AFTER InjectHistory" 双向校验。条目描述过时；归档关闭，本批 PR
- [ ] **R230B-ARCH-26 — feishu.go 1000+ 行 + 各平台拆分粒度不一致（P3）**: 方案：每平台拆 4 文件 transport/wire/outbound/capability。Breaking：否。
## Round 229 — 5-agent 并行 code review（2026-05-20）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）跑了全仓 review。14 处直接修已落本轮 PR；以下条目为非 breaking 但需要更大改动 / 需设计决策 / 跨模块的发现，登记追踪。

### Security（剩余）

- [ ] **R229-SEC-1 — ExtraArgs flag 注入未受限（P1）**: `protocol_claude.go:77` / `protocol_acp.go:158` 直接拼接 `opts.ExtraArgs` 到 argv，无 flag 允许列表。受信认证用户可注入 `--mcp-config` / `--add-dir /etc` / `--skip-permissions` 等危险 flag。方案：BackendProfile 声明 flag allowlist；任何以 `--` 开头且不在白名单的 ExtraArgs 元素拒绝并 Warn。Breaking：是（依赖任意 ExtraArgs 的运维方需迁移到允许列表）。
- [ ] **R229-SEC-2 — serveRender TOCTOU inode-swap（P1）**: `project_files.go:683` 在 `os.Lstat(resolved)` 后再 `serveRender → os.Open(resolved)`，攻击者可通过 Claude Write 工具在窗口内创建符号链接指向 `/etc/passwd`。方案：`handleFileGet` 直接 OpenFile 拿 fd 传入 serveRender，或 Open 后 Fstat 比对 inode。Breaking：否（内部重构）。
- [ ] **R229-SEC-3 — allowed_root 缺失不阻断启动（P1）**: `server.go:513` 公网监听 + dashboard_token 配置时，`allowed_root` 为空只 Warn 启动。认证用户可设 cron `work_dir=/etc` 让 CLI 写系统文件。方案：dashboard_token 非空 + 监听非纯 loopback 时 fatal 启动失败 + naozhi doctor HIGH 级别检查。Breaking：是（现有公网部署需补 allowed_root）。
- [ ] **R229-SEC-5 — ws:// node 连接明文传输 token（P2）**: `reverseserver.go:150` 反向 node 第一条消息明文携带 token。方案：`/ws-node` handler 在 r.TLS==nil 且无可信 X-Forwarded-Proto: https 时拒绝 Upgrade，或加 insecure_node 显式豁免 flag。Breaking：是。
- [ ] **R229-SEC-8 — per-token WS 连接数无上限（P2）**: `wshub.go:307` 单 token 持有者可建 500 个 WS 连接绕过 maxSubscribersPerKey=20。方案：WS 升级时按 cookie MAC 或 IP 桶检查 per-token 连接 cap（如 20）。Breaking：否。
- [ ] **R229-SEC-12 — CDN allowlist 与 SRI 配合不足（P3）**: dashboard CSP `script-src` 含 `https://cdn.jsdelivr.net`，SRI 失败时 CSP 仍允许加载。方案：迁 nonce 模式后从 script-src 移除 CDN 域名。Breaking：是（与 R229-SEC-6 合并）。

### Go 正确性 / 并发（剩余）

- [~] **R229-GO-1 — ReattachProcessNoCallback 清 deathReason 与 mapSendError Store 存在 logical race（已文档化 2026-05-23）**: managed.go:477-487 ReattachProcessNoCallback godoc 已显式声明 SAFETY CONSTRAINT："this function must only be called when Send() cannot be in flight for this session"。调用者契约已明文锁定，本批 PR 归档。
- [~] **R229-GO-2 — Snapshot() 包含读侧写副作用（已文档化 2026-05-23）**: managed.go Snapshot godoc + R226-CR-13 内联注释已标注为有意决策；与 R230C-CR-Diag、R226-CR-13 主跟踪。SnapshotReadOnly 拆分需 RFC（spawnSession 显式 mirror 比 pull-based 镜像更脆弱）。本批 PR 归档为已文档化。
- [~] **R229-GO-5 — InjectHistory lastPrompt 仅"为空才设"（误报关闭 2026-05-23）**: 实地复核 managed.go InjectHistory 已有 `if loadAtomicString(&s.lastPrompt) == ""` 守卫，仅在为空时才 Store；若 Send 已先写入非空值，InjectHistory 直接跳过 — 与 TODO 担忧的方向相反（不会用 stale 替换 fresh）。godoc 已显式说明 "benign TOCTOU"。归档关闭，本批 PR。

### Performance（剩余）

- [ ] **R229-PERF-1 — Protocol.ReadEvent string→[]byte 双 copy（P1）**: 每个 stream 事件分配 1 个 []byte（line size，50 B–200 KB）。方案：Protocol.ReadEvent 签名改 []byte，shimMsg.Line 改 json.RawMessage 同步消除中间 string 拷贝。Breaking：是（Protocol 接口变更，所有实现 + fakes 更新）。
- [~] **R229-PERF-5 — EventLog.Append 单 entry 路径每次分配 1-slot 切片（归档 2026-05-23）**: 同 R215-PERF-P2-1 / R217-PERF-4 / R219-PERF-4 / R222-PERF-8 / R226-PERF-5 / R227-PERF-9 / R228-PERF-7 / R230C-PERF-2 同根因（PersistSink 契约允许 retain slice）。生产热路径已由 R230-PERF-1 sink-nil 早返回覆盖。统一收敛到 R230C-PERF-2，本批 PR 归档。
- [ ] **R229-PERF-6 — discovery.Scan 每次 O(N) os.ReadDir 调用（P2）**: 已有 promptCache/summaryCache，但 listJSONLsByMtime 未缓存。方案：(claudeDir, cwd) → mtime invalidated 缓存。Breaking：否。
- [~] **R229-PERF-7 — SetAgentInternalID 写锁覆盖 ring buffer 反向扫描（P2）**: 8 个 sub-agent 并发 resolve 时 Append 串行化。方案：扫描阶段 RLock，需变更时升级写锁。Breaking：否。 — 评估关闭（已归档 2026-05-23 复核）：R225-PERF-13 已落地缓解——`internal/cli/eventlog.go:677-714` SetAgentInternalID 已 cap scan 在 `setAgentInternalIDMaxScan = 50`（line 21）+ 双 found 标志早 break，写锁持锁时间收敛到至多扫描 50 个 ring slot。Go RWMutex 不支持 read→write upgrade（reader/writer 之间需先 RUnlock 再 Lock），中途升级会引入 ABA：scan 阶段读到的 entry 在 RUnlock 与 Lock 间被 Append 覆盖，写入到错误 slot。当前持续写锁是正确性最优方案，cap=50 + early break 已把锁竞争降到可接受窗口。本批 PR。
- [~] **R229-PERF-9 — extractLastPromptUncached 大文件 fallback 全文扫两次（误报关闭 2026-05-23）**: 复核 discovery/scanner.go:587-594 现状：extractLastPrompt 已对所有 result 调 setCachedPrompt（line 593），无论 result 为 "" 还是非空——负面结果 mtime keyed cache 已落地。条目描述过时，归档关闭，本批 PR
### Code 质量（剩余）

- [~] **R229-CR-1 — cron.executeOpt 329 行高复杂度（归档 2026-05-23）**: R226-CR-10 / R230-CQ-9 / R232-ARCH-12 同根因多轮重申。统一收敛到 R232-ARCH-12（executeStep interface 4-step 拆解方案）跟踪。
- [~] **R229-CR-2 — NewRouter 359 行未被 router-split 重构覆盖（归档 2026-05-23）**: R230-CQ-10 / R231-ARCH-11（NewRouter + Router.Start(ctx) 拆分方案）/ R222-CR-1 同根因多轮重申。统一收敛到 R231-ARCH-11 跟踪。
- [~] **R229-CR-3 — reconnectShims 350 行 + 单分支 90 行（归档 2026-05-23）**: R230-CQ-8 / R222-CR-3 同根因多轮重申。统一收敛到 R222-CR-3（classifyAndPlanShimAction + executeShimAction 拆解方案）跟踪。
- [~] **R229-CR-7 — dispatch.go 1281 行 replyTracker 与 Dispatcher 同居（归档 2026-05-23）**: 实地核查 `internal/dispatch/dispatch.go` 当前 1256 行（条目登记后小幅缩小但没拆），replyTracker struct + 12 方法（816-1259）仍在同文件。同根因 R224-CR-9 / R226-CR-8（dispatch/imreply 子包）/ R230-CQ-2（sendAndReply 拆分）已多轮重申。统一收敛到 R224-CR-9 跟踪。

### Architecture（剩余 P1，需设计）

- [ ] **R229-ARCH-1 — internal/server 已成 god package（P1）**: 80+ 文件、Server 持 17+ 子 handler。docs/design/server-split-design.md Phase 3 未推进。方案：拆 internal/wshub + internal/api 子包。Breaking：否（包内重组）。
- [ ] **R229-ARCH-2 — internal/cli 越界承担 EventLog/image/SubagentLinker/history 工厂（P1）**: 21 处 session→cli 反向 import。方案：上移 EventLog/SubagentLinker 到 session/eventlog（已有 bridge 雏形），image/thumbnail 到 internal/attachment，history 到独立子包，cli 回归 Process+Protocol+Wrapper。Breaking：否。
- [~] **R229-ARCH-3 — Router 单聚合根承载 6 大职责（P1）**: 文件级拆了，struct 仍持 30+ 字段 4 把锁、shutdown_lock_order_test 即证明易死锁。方案：拆 SessionStore + Lifecycle + DiscoveryService + ShimReconciler 四子组件。Breaking：是（外部引用 *session.Router 多）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R229-ARCH-4 — ManagedSession 65 方法 + 隐式语义标签（P1）**: Exempt/Stub/Scratch/Paused 通过 process==nil + key 前缀推导。方案：拆 SessionMeta（持久化）+ LiveSession（运行时）+ 显式 tag enum。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R229-ARCH-5 — KeyResolver/server/dashboard 双路径未收拢（P1）**: server.go buildSessionOpts legacy fallback + dispatch fallback resolver 并存，contract test 已证明。方案：删除 fallback，让 KeyResolver 唯一入口。Breaking：否（内部）。
- [ ] **R229-ARCH-6 — Channel Adapter 能力鸭子类型散落（P1）**: SupportsInterimMessages/AsReactor/AsQuestionCardSender/PermanentError 等可选接口持续增长，新增 LINE/Telegram 困难。方案：参照 cli.Caps 引入 PlatformCaps 聚合。Breaking：否（向后兼容）。
- [ ] **R229-ARCH-7 — main.go 持 settings.json 重写 / hooks 过滤 / env 过滤业务逻辑（P1）**: 方案：抽 internal/claudesettings 子包独立可测。Breaking：否。
- [ ] **R229-ARCH-8 — Dispatch DispatcherConfig 仍依赖 *session.Router 具体类型（P2）**: 接口化只完成一半。方案：与 R229-ARCH-4 配套切到 LiveSession 接口。Breaking：否。
- [~] **R229-ARCH-9 — Hub 承担"send + broadcast" 越界（归档 2026-05-23）**: 实地核查 `internal/server/server.go:802` `SendFn: s.sendWithBroadcast` 仍是 dispatch 反向依赖 Hub 的注入点。同根因 R230-ARCH-2 / R231-ARCH-2 / R232-ARCH-9（消息出口三/四套管线）多轮重申。统一收敛到 R231-ARCH-2 跟踪，跨包重构 NEEDS-DESIGN。
- [ ] **R229-ARCH-10 — reservedKeyPrefixes 散在 4 文件（P2）**: cron/project/scratch 前缀属性表分散。方案：session/reserved_keys.go 单点表 (Prefix, Exempt, Persisted, SidebarVisible)。Breaking：否。
- [ ] **R229-ARCH-11 — Send 路径 Dashboard/IM/Cron 三流水线规则不同（P2）**: queue/guard/broadcast 编排各异。方案：抽 SessionInvoker（参 docs/rfc/message-queue.md）。Breaking：否。
- [ ] **R229-ARCH-12 — shim 边界在 cli.Wrapper / cli.Process / session/router_shim 三处反复横跳（P2）**: 方案：shim 包提供 ShimSession 高层 API 收拢启动+reconnect+send+close。Breaking：否。
- [ ] **R229-ARCH-13 — agents 字典向 6 个组件并行传递（P2）**: 方案：KeyResolver 持有，其他从 resolver 拿。Breaking：否。
- [ ] **R229-ARCH-14 — Server.New + NewWithOptions 双构造器仍并存（P3）**: ~20 test call sites 拖累。方案：批量改测试一次性删除 New。Breaking：否（内部）。
- [ ] **R229-ARCH-15 — Process.Send / SendPassthrough 双路径不变量难维护（P3）**: 方案：legacy Send 视为 passthrough N=1 退化形式收拢，需评估 ACP 协议在 N=1 下行为。Breaking：否。
- [ ] **R229-ARCH-16 — ScratchPool 与 router.sessions 双池（P2）**: 所有迭代必须遍历两侧或 IsScratchKey 判断。方案：scratch 进 sessions map + Tag=Scratch 显式。Breaking：否。
- [ ] **R229-ARCH-17 — KnownIDs/SessionIDToKey/Sessions/Workspace/Backend Overrides 5 个 map 同步维护（P3）**: 方案：抽 SessionIndexes struct + 一锁。Breaking：否。
- [ ] **R229-ARCH-18 — discovery/history/eventlog 三套读历史路径（P3）**: 方案：抽 HistoryService 单一入口归一化。Breaking：否。
- [ ] **R229-ARCH-19 — feishu MentionMe 仍 loose 实现（P3）**: 方案：platform 协议 MentionMatch 强制精确 self-id 匹配。Breaking：是（feishu 行为变化，需 release note）。
- [~] **R229-ARCH-20 — BumpVersion 双语义（DataVersion 与 RenderVersion 混用）（P3）**: 方案：拆两计数器。Breaking：否（dashboard 加新字段，老前端忽略）。 — 评估关闭（已归档 2026-05-23 复核）：godoc 已落地（`internal/session/router_core.go:1099-1116` Version() 段落 + R230C-ARCH-18 锚点；line 1120-1138 BumpVersion godoc 显式说明它是 Render version 半边，不 set storeDirty 但 advance storeGen）。代价分析：拆两计数器需 dashboard JS + Go 端双向 schema 迁移，而当前重叠成本仅是 BumpVersion 时一次 debounced saveStore（IO-cheap），ROI 不足。godoc 锁定语义后未来若 BumpVersion 写频率显著提升再拆。本批 PR。
## Round 226 — 5-agent 并行 review 第 38 轮（2026-05-19 第三批）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）再跑一轮全仓深扫。13 处 FIX-READY 已落地本轮 PR：
> - `process.Kill()` godoc 被前函数 `isChanAlive` 文段污染，删除残留首句（R226-CR-1）
> - `router_core.go:1041` 孤立 `stripResumeArgs` 注释（函数已迁 router_lifecycle.go），整段删除（R226-CR-2）
> - `process_readloop.go` 头部 Phase 2 过期注释（`isChanAlive` 已迁 process_turn.go），删除（R226-CR-3）
> - `spawnSession` godoc 显式声明 LOCK 顺序与"不得持其他锁"约束（R226-CR-4）
> - `dispatch/dispatch.go` 与 `server/errors_usermsg.go` 错误→中文消息映射加 `// keep in sync` 双向交叉引用注释（R226-CR-5）
> - `passthrough.go newSlotUUID` 注释指明 `uuidFallbackSeq` 在 `cli/uuid.go` 包级声明（R226-CR-6）
> - `upload_store.removeEntryLocked` 不再原地改写 `*uploadEntry.Owner`，改用局部变量 fallback，避免破坏调用方持有的 entry 字段不变式（R226-GO-1）
> - `select_node_for_backend.go` 局部变量 `cap` → `requiredCap` 解除 builtin 遮蔽（R226-GO-2）
> - `weixin/weixin.go getUpdates` 与 `weixin/api.go sendMessage` 的 `errmsg` 走 `osutil.SanitizeForLog(256)` / `%q`，与 feishu/slack 错误日志风格对齐，封堵 iLink relay 注入 ANSI/换行的 log 通道（R226-SEC-1）
> - `selfupdate.Replace` 在 Rename 失败时用 `errors.Join` 聚合 cleanup/restore 错误，避免 restore 失败被静默吞掉（R226-GO-3）
>
> 以下是需设计决策、破坏兼容、跨包重构、或方案不唯一不适合本轮直接修的条目。

### 安全 — 本轮新发现

- [ ] **R226-SEC-3 — 反向 node 在 `ws://`（无 TLS）下传 token 仅 `slog.Warn` 不阻断（P2）**: token 在第一条 WS 消息明文，passive 观察者可截获并冒充 node。方案：primary 端 `/ws-node` 加 `require_tls: true` 配置，默认 reject 非 wss 升级，除非显式 `insecure: true`。Breaking：现有 `ws://` 部署需加配置或迁 `wss://`。`internal/upstream/connector.go:210` + `internal/node/reverseserver.go:155`。

### 性能 — 本轮新发现

- [ ] **R226-PERF-1 — `Protocol.ReadEvent(line string)` 每事件 `[]byte(line)` 堆拷贝（P1，封 R67-PERF-1 实施分支）**: 5-50 ev/s × N session 的强制 alloc。方案：Protocol 接口签名 `ReadEvent([]byte)`，shimMsg.Line 改 `json.RawMessage`。Breaking：是（接口）。
- [ ] **R226-PERF-4 — ACP `agent_message_chunk` 每 chunk 一次 `json.Unmarshal`（P2）**: kiro streaming 高频路径，500 unmarshal/s 仅此一处。方案：手写 byte-scan 提取 `"text":"..."` value，跳过 reflect。`internal/cli/protocol_acp.go:517`。
- [~] **R226-PERF-6 — `EventLog.applyEntryStateLocked` task 事件线性扫 turnAgents/bgAgents（P3）**: 多路 subagent 场景（>8 并行）双重 O(n)。方案：当 `len > 8` 时建 `map[string]int` 索引。`internal/cli/eventlog.go:405`。 — 评估后不实施（typical turnAgents len 1-3，result/user 事件已自动重置；threshold-based map 需 4 个同步映射 cover ToolUseID/TaskID × turn/bg，维护成本远高于收益；P3 + 无 >8 subagent 实测案例），本批 PR #164

### 代码质量 — 本轮新发现

- [ ] **R226-CR-7 — `RegisterForResume` / `RegisterCronStubWithChain` 用 `r.CLIName/Version` 在多 backend 部署下显示错误（P1）**: 这两条路径只看默认 wrapper；router_core.go loadStore 已正确走 `wrapperFor(entry.Backend)`。方案：加 `backend string` 参数 → `wrapperFor`。Breaking：caller API。`internal/session/router_discovery.go:259,362`。
- [~] **R226-CR-10 — `cron/scheduler.executeOpt` 320 行 7 失败分支（归档 2026-05-23）**: R229-CR-1 / R230-CQ-9 / R232-ARCH-12 同根因多轮重申。统一收敛到 R232-ARCH-12 跟踪。
- [~] **R226-CR-11 — `cron/scheduler.go` 2739 行单文件无拆分计划（归档 2026-05-23）**: R230-CQ-14 / R232-ARCH-1 同根因多轮重申。统一收敛到 R232-ARCH-1（5–6 文件 lifecycle/jobs/execute/finish/persist/core 拆分方案）跟踪。
- [~] **R226-CR-12 — `wshub.go` 1785 行 + `feishu.go` 1461 行无拆分计划（归档 2026-05-23）**: R224-ARCH-14 / R230B-ARCH-26（feishu 4 文件 transport/wire/outbound/capability 拆分） / R230-ARCH-15（server 90+ 文件按子包拆分） 同根因多轮重申。统一收敛到 R230B-ARCH-26 + R224-ARCH-14 跟踪，跨文件重构需 ADR。

### 架构 — 本轮新发现

- [ ] **R226-ARCH-1 — `server` 包成"上帝包"（P1）**: 92 个 .go + 12 子 handler，承担路由+UI+业务编排+Hub+nodeCache。建议：薄壳 server + 拆 `internal/server/api/{cron,project,scratch,discovery,...}` 子包；Hub/nodeCache 走显式注入。需 RFC。
- [~] **R226-ARCH-2 — `KeyResolver` 在 main/server/dispatch 三处独立构造（归档 2026-05-23）**: 实地核查 cmd/naozhi/main.go:834 + internal/server/server.go:551 + internal/dispatch/dispatch.go:195 三处仍各自 `session.NewKeyResolver(agents, project.NewDataSource(...))`。R230-ARCH-9 / R224-ARCH-12 / R229-ARCH-5 / R216-ARCH-3 同根因多轮重申。统一收敛到 R230-ARCH-9（projectMgr.Resolver() 单点暴露方案）跟踪，跨包构造签名变更 NEEDS-DESIGN。
- [~] **R226-ARCH-3 — dispatch `sendFn` 接 `*session.ManagedSession + cli.SendResult` 破坏分层（P1）**: dispatch 名义有 `SessionRouter` 接口但发送闭包绕过。 *(PR #330 R243-ARCH-10 已部分实施：sendFn 收口到 dispatch.Capabilities.Send interface，server.serverCaps 实现 — closure 已被 interface 替代。但**依然导入 cli/session 具体类型**（cli.ImageData / cli.EventCallback / *cli.SendResult / *session.ManagedSession 仍在 Capabilities.Send 签名里）— 跨包分层完整解耦需 dispatch.SendRequest/SendResult 自定义 DTO 才能切干净，跟踪到独立 NEEDS-DESIGN R248-ARCH-3 Deprecated 字段清理或 follow-up RFC。)*
- [ ] **R226-ARCH-4 — `session.Router` 30+ 字段（P2，封 R225-ARCH-* 重申）**: 注解块边界天然拆分点（processPool/sessionStore/historyAttacher/shimReconciler/attachmentTracker）。需独立 RFC。
- [ ] **R226-ARCH-5 — Platform 包间多份"形似独立"实现（P2）**: maxIncomingTextBytes / SafeHTTPClient / Start-Stop 模板 / messageID 编解码各家重复。方案：`platform.BasePlatform` + `SafeHTTPClient(opts)` + `MessageRefCodec`。
- [ ] **R226-ARCH-6 — `workspace`/`workspaces`/`Session.cwd` 别名 + 无热重载（P2）**: 已有 deprecated warn 但仍接受双形；高频字段（agents/projects/cron）需 SIGHUP 重载。Breaking：v2 schema。
- [ ] **R226-ARCH-7 — 状态分散到 7 处 store（P2）**: sessions.json / event log / knownIDs / project.yaml / cron store / shim state / scratch 各自 dirty + throttle，崩溃恢复语义不对齐。方案：先画恢复矩阵契约文档；再统一 `state.Store` facade。
- [ ] **R226-ARCH-8 — `dispatch.replyTracker` 含 editLoop / todoLoop / askquestion 卡片+文字 fallback 全在 dispatch.go 内（P2）**: 抽 `internal/dispatch/imreply` 子包。
- [ ] **R226-ARCH-9 — backend 维度配置散落 5 处（P2）**: Profile + main backendModels/backendExtraArgs map + replyTagForBackend + ReplyFooterFn fallback + StartShimWithBackend + reverse capability 字段。方案：Profile 当真正注册中心，model/args defaults/tag/shim hint 全进 Profile。
- [ ] **R226-ARCH-10 — `TestProcess` 暴露 30+ 公有可写字段（P2）**: 新加 processIface 方法即破坏 mock。方案：拆细 procRunner/procReader/procIntrospector 子接口，InjectSession 改 builder 模式。
- [ ] **R226-ARCH-11 — exempt 命名空间（cron:/project:/scratch:/quick:）共享标记但归属各异（P2）**: 每加一个内部子系统改 4 处过滤。方案：`KeyKind enum` + `Policy{Persist, Exempt, SidebarVisible, Sweepable}` 表，router 用查表代替 if-prefix 链。
- [ ] **R226-ARCH-12 — 错误→用户中文消息没注册式映射（P2）**: 新加 sentinel 必须改 dispatch + server 两处。方案：`errmap.UserMessage(err) (msg, severity)` 注册表，各包注册自己的 sentinel。
- [ ] **R226-ARCH-13 — `cmd/naozhi/main.go` 含 ~390 行 settings.json hook/env 过滤逻辑（P3）**: 影响测试覆盖。方案：迁 `internal/cli/settingsoverride`。
- [ ] **R226-ARCH-14 — `cli` 包同时含 protocol/process/eventlog/history/shim io/image/thumbnail（P3）**: image/thumbnail/uuid/todo 抽到 `internal/cli/payload`；event log 迁 `internal/eventlog`（已有 persist 子包）。
- [ ] **R226-ARCH-15 — 各 mutation 路径手动 fan-out `s.hub.BroadcastSessionsUpdate()`（P3）**: 隐式事件总线无显式 publish API。方案：`events.Bus` + 订阅模式。
- [ ] **R226-ARCH-16 — panic recovery 在 dispatch/platform/cli/session 四处独立写（P3）**: metric 名也分。方案：`osutil.RecoverWith(label, ...)` helper。

## Round 225 — 5-agent 并行 review 第 37 轮（2026-05-19 第二批）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）再跑一轮全仓深扫。9 处 FIX-READY 已落地本轮 PR：
> - `cli/passthrough.newSlotUUID` `crypto/rand.Read` 错误 panic-safe fallback（与 `newEventUUID` 对齐）
> - `shim/server.go writer goroutine` 内层 `w.Write(more)` 失败 early-return（封 R224-GO-7 实施分支）
> - `shim/server.go SetReadDeadline` 错误显式 close+return（封 R224-GO-6 实施分支）
> - `upstream/connector.go` `os.Hostname` 错误 fallback "unknown" + slog.Warn
> - `server/dashboard_send.parseAttachmentFile` 加 `io.LimitReader(f, cap+1)` 防御 fh.Size 撒谎
> - `cli/process_readloop.go` heartbeat Timer.Stop+Reset 注释精度修正（Go 1.23+ 行为描述）
> - `session/router_lifecycle.ResetChat` fallback 分支 prefix 拼接外提避免每次迭代 alloc
> - `cli/protocol_acp.go` ToolCall.Title/Kind/Status 加 `osutil.SanitizeForLog(256)` 截断+清洗（dashboard chip 注入面）
> - `selfupdate.fetchFile` 0755 → 0600，`Download` 在 verifyChecksum 通过后 `os.Chmod 0755`（R224-SEC-1 第①第②项落地，固定路径竞争与 cosign 签名仍开）
> - `config.validateConfig` 新增 `validatePlannerPrompt` 对 `cfg.Projects.PlannerDefaults.Prompt` 强制 NUL/C0/DEL/C1/bidi/LS-PS 全拒，与 `project.ValidateConfig` 对齐（R224-SEC-4 落地）
>
> 以下是需设计决策、破坏兼容、跨包重构、或方案不唯一不适合本轮直接修的条目（Round 224 已登记的 R224-* 锚点不重复列出）。

### Go 正确性 — 本轮新发现

- [ ] **R225-GO-2 — `cli.Resolve` retry sleep 的 ctx 取消（P2 R224-GO-1 子分支）**: subagent_link.go:289/327 `time.Sleep(retryInterval)` 在 SIGTERM 期间无法被取消；与 R224-GO-1 同批接 ctx。Breaking：Resolve 签名加 ctx 参数。
- [ ] **R225-GO-3 — `process_event_query.InjectHistory` 裸 `go linker.Resolve(...)` 无 ctx（P2 R224-GO-3 同源第三分支）**: process_event_query.go:61 与 router_shim.go reconnect 同型；`wallclock` 取的是 `e.Time` 是对的，但 SIGTERM 期间 goroutine 仍不可中止。同 R225-GO-2 一并接 ctx。
- [ ] **R225-GO-4 — `Router.Remove` / `dropEventLogForKey` 用 `context.Background()` + 独立 timeout 而非传入 ctx（P2）**: router_cleanup.go:97 `Remove` 路径上 7s 内不可被 SIGTERM 取消，shutdown tail latency 加重。Breaking：Remove 签名加 ctx。
- [~] **R225-GO-7 — `cli.Process.Kill` 在持 shimWMu 下调 closeShimConn，与 Detach 顺序不一致（误报关闭 2026-05-20）**: 复核 process.go:519-535 (Kill) 与 :617-634 (Detach) 后两者均为 `shimWMu.Lock → SetWriteDeadline → shimSendLocked → closeShimConn → Unlock` 同一模式；两个函数都在持 shimWMu 期间调 closeShimConn，并非"Detach 是 Unlock 后 close"。closeShimConn 走 sync.Once 守护的 net.Conn.Close（R219-GO-3 落地），最坏延迟仅一次系统调用，与 SetWriteDeadline+shimSendLocked 同量级。R227-GO-8 同根因一并关闭。本批 PR #169

### 安全 — 本轮新发现

- [~] **R225-SEC-1 — `cli.Wrapper.BuildArgs ExtraArgs` 缺 flag 允许列表（P1 R219-SEC-1 重申）**: 认证 dashboard 用户可以通过 ExtraArgs 注入 `--mcp-config`/`--add-dir`/`--skip-permissions` 等改变 CLI 行为的参数。capExtraArgsBytes 仅查字节长度。方案：维护 flag denylist（或 allowlist），在 BuildArgs 之前过滤；Breaking：依赖任意 ExtraArgs 的 ops 需迁移。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R225-SEC-2 — `shim.moveToShimsCgroup` 不验 CLIPID 是否真的是 shim 子进程（P1 R219-SEC-5 重申）**: handle.Hello.CLIPID 来自 shim 自报，naozhi 直接 sudo busctl 把任意 PID 移入 cgroup（可能是 sshd / pid=1）。方案：读 `/proc/<CLIPID>/status` 验 PPid == cmd.Process.Pid。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R225-SEC-4 — `selfupdate.checksums.txt` 未做 GPG/cosign 签名验证（P3 R224-SEC-1 第④项长期）**: GitHub Releases CDN 同时被妥协时 hash 与 binary 都可被换。方案：release 流程对 checksums.txt cosign 签名 + verifyChecksum 前置签名校验。

### 性能 — 本轮新发现

- [ ] **R225-PERF-1 — `Protocol.ReadEvent(line string)` 接口签名导致 `[]byte(line)` 强制堆 copy（P1 R67-PERF-1 ACP 分支补）**: protocol_claude.go:174 / protocol_acp.go:296 都 `json.Unmarshal([]byte(line), ...)`；最热路径每事件一次 alloc。方案：接口签名改 `ReadEvent(line []byte)`，readLoop 把 trimmed 直接传入。Breaking：是（接口）。
- [ ] **R225-PERF-2 — `process_readloop` `system/task_started` 无背压裸 `go linker.Resolve`（P1）**: process_readloop.go:393 多 sub-agent 并发启动时短时间产生大量 goroutine。方案：用 buffered channel 信号量 / 工作池限并发；与 R224-GO-1 信号量改造统筹。
- [ ] **R225-PERF-4 — `applyMetadata meteringUsage merge` 用 slice O(n×m)（P2）**: process.go:717-745 meteringMu 锁内字符串 Unit 等比；MeteringUsage() 每读一次 make+copy。方案：`map[string]*MeteringEntry` 内部存储；Snapshot 路径缓存空 case。
- [ ] **R225-PERF-6 — `Snapshot SubagentInfo` slice copy（P2）**: managed.go TurnAgents 即便 turnAgentCount 已快速短路，Snapshot 中其他分配仍存在；评估 SubagentInfo slice sync.Pool。
- [ ] **R225-PERF-7 — `protocol_acp.readUntilResponse` 每次握手起独立 goroutine + channel（P2）**: protocol_acp.go:677 改预先 SetReadDeadline 让 ReadLine 自然超时返回，省掉 goroutine + channel + pulse；含 R224-GO-2 同位修改。
- [~] **R225-PERF-9 — `wshub.eventPushLoop` 同一 session 多 WS 各自 marshal（归档 2026-05-23）**: 同 R230B-PERF-1 / R219-PERF-1 主条目跟踪。eventPushLoop 是 per-subscription 独立 goroutine 各持 lastTime 游标；两个订阅者可能在不同 lastTime 上请求不同 entry slice，无法简单一次 marshal 共享 byte 引用。需统一时间游标——RFC 级改造。本批 PR 归档。
- [~] **R225-PERF-10 — `marshalPooled` 每次 copy 一份独立 backing（归档 2026-05-23）**: session_state 字符串 enum 集合小但 Reason 字段任意（含 err 文本），LRU key 空间不可控；本批检查 broadcast 路径已有 doBroadcastSessionsUpdate 一次 marshal 多次 SendRaw 收敛热路径。本批 PR 归档。
- [~] **R225-PERF-14 — `wsclient.sweepSubGenExpiredLocked` 在 hub 写锁下扫 map（归档 2026-05-23）**: c.subGen map 与 c.subscriptions map 共用 h.mu 保护是显式契约（wsclient.go:127 注释说明）；移到 client-local mutex 需要 2 层锁协议；扫描 map 上限是 maxSubscribersPerClient=50，bounded scan 不是热路径。本批 PR 归档。
- [~] **R225-PERF-17 — `TruncateRunes(string, ...)` 无字节快检（P3，误报关闭 2026-05-20）**: reviewer 提议 `len(s) <= maxRunes*4` 短路其实方向反了——UTF-8 每 rune 1-4 字节意味着 byte 长度 ≤ rune 数*4 是 rune 数的**上界**而非下界，全 ASCII `len=200, maxRunes=50` 时 byte=200 ≤ 200 但 runes=200 > 50，加这种快检会漏截。当前 `len(s) <= maxRunes` 快检已与 `TruncateRunesBytes` 一致，无可优化空间。

### 代码质量 — 本轮新发现

- [ ] **R225-CR-1 — `cli/detect.knownBackends` 与 `backend.Profile` 注册表双轨（P1）**: detect.go:43-46 静态硬编码 Protocol 字符串；与 `Profile.Capabilities().StreamJSON` 不同步会导致 dashboard 误判。方案：`DetectBackendsCtx` 从 `backend.All()` 派生，删 knownBackends。Breaking：否。
- [ ] **R225-CR-5 — `backendDisplayName / normalizeBackendID` 与 `backend.Profile.DisplayName/ID` 重复（P2 R224-ARCH-1 同源）**: cli/wrapper.go:75-95；新加 backend 改四处。方案：合并到 `backend.Get` 的 DisplayName/ID 字段。

### 协议正确性 — 本轮新发现（与 fix/claude-model-from-init-v2 分支强相关）

- [ ] **R225-ACP-MODEL-INIT — ACP `session/new` 返回 `result.models.currentModelId` 未回填 `Process.model`（P2）**: protocol_acp.go:168-174 解析 session/new result 时未读 currentModelId。`Snapshot.Model` 永远显示 spawn 配置值（或空）而非 kiro 实际使用的 model。方案：Init 返回值或回调把 currentModelId 传给 setModel。Breaking：否（增量字段）。

### 架构 — 本轮新发现

- [~] **R225-ARCH-1 — `EventEntry / ImageData / EventCallback / SendResult` 跨层 DTO 塌陷（P1）**: node.ServerMsg.Events 钉住 cli 内部领域类型作为反向连接 wire 协议；server 26+ 包 import internal/cli。方案：抽叶子包 `internal/event`（零依赖），cli/server/node/dispatch 都 import 它；或 dispatch 加 DTO 转换层。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 100 条发现。
> 11 处 FIX-READY 已落地本轮 PR（selfupdate constant-time compare + checksums.txt 64KB 上限独立 / feishu maxWebhookTokenLen 512 守卫 / protocol_acp ExtraArgs capExtraArgsBytes 对齐 claude / protocol_acp readUntilResponse 错误 %w 包装区分 EOF vs read err / subagent_link entries[:0:0] 替为 make 显式 / gzipMiddleware response-only godoc / gzip Flush godoc 修正过时 "no handler uses Flusher" / dashboard.marshalPooled 安全契约交叉引用 writeJSON CLIENT-SIDE CONTRACT / router.kiroSessionsDir Sprint 1a unwired 注释更新为 "wired in main.go" / backend/profile.go Sprint 0b/1b 路线图描述去除）。
> 以下是需设计决策、破坏兼容、跨包重构、或方案不唯一不适合本轮直接修的条目。

### Go 正确性 — 本轮新发现

- [~] **R224-GO-1 — `cli.Resolve` 信号量 acquire 处的 Timer 资源 leak（P1 R219-GO-1 未覆盖分支）**: `subagent_link.go:263-270` Timer Stop 后未 drain `t.C`；进程 SIGTERM 时所有等 sem 的 goroutine 永久阻塞，因 select 无 ctx.Done() arm。需与 R219-GO-1 同批修复，扩大覆盖到 Timer 路径 + 两处 retry sleep（`:289` + `:327`，code-reviewer 指出）。Breaking：是（Resolve 接受 ctx）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R224-GO-2 — `protocol_acp.readUntilResponse` 非 shim path goroutine 在 timeout 后仍阻塞 ReadLine（P1）**: `protocol_acp.go:635-673` 仅 shimLineReader 路径走 SetReadDeadline；非 shim path 在 ACP 握手超时后 goroutine 卡 ReadLine 直到管道 EOF。每次握手超时泄漏一个 goroutine。方案：非 shim path 也走 deadline-aware reader 或 timeout case 直接 close 底层 conn。涉及：`internal/cli/protocol_acp.go:635`。
- [ ] **R224-GO-4 — `subagent_link.fireOnResolveLocked` mu-release-reacquire 易死锁/panic（P2）**: `subagent_link.go:565` 持 `l.mu` write lock 时 Unlock + 跑 callback + 再 Lock，依赖 callback 不调 `linker.Resolve`（会触发写锁死锁）+ 单一 goroutine 进入此函数（否则第二个 Unlock panic 解锁未持有锁）。方案：先在 call site 拷贝 ID，Unlock 之后再 fire，整体移出锁外。
- [ ] **R224-GO-7 — `shim/server.go writer goroutine` 内层 `w.Write(more)` 错误 nolint 吞（P2）**: `:785` 写失败后 `:790` 仍调 `flushWithDeadline()`，可能将损坏的 buf 状态 flush 出去。方案：内层 write 失败立即 return。

### 安全 — 本轮新发现

- [ ] **R224-SEC-1 — `selfupdate` 临时文件 0755 + 固定 staging/backup 路径，存在 fetch→verify TOCTOU + 多用户竞争（P1）**: `selfupdate.go:203` `os.OpenFile(dest, ..., 0755)` 写 binary 到 `/tmp/...` 在 verifyChecksum 之前可执行权限可写；`Replace` 用固定 `installPath + .staging/.bak` 路径，多用户 install dir 下可被其他 UID 抢先创建。方案：① 临时文件 0600 写入，verify 后 chmod；② `os.CreateTemp` 替换固定 staging path；③ verify 路径直接用已打开 fd 而非重新路径查找；④ 长期加 GPG/cosign 签名校验 checksums.txt（checksums.txt 自身未签名，CDN/release 同时被改 hash 匹配仍不可信）。涉及：`internal/selfupdate/selfupdate.go:119-203`。
- [ ] **R224-SEC-3 — `EffectivePlannerPrompt` 放行 LF/CR 与 ValidateConfig 拒绝 LF/CR 策略不一致（P2）**: `project/manager.go:357` 注释明放行 `0x0a/0x0d` 让 markdown CLAUDE.md 风格 prompt 通过；但 LF 在 stream-json NDJSON 帧中是行分隔符，注入可破坏协议帧。直接对齐拒绝会破坏现有 multi-line prompt 配置；需设计：是否把 multi-line CLAUDE.md 风格 prompt 经预处理（把 LF 替换为 ` ` 或 `\\n` literal）后才进入 argv，或拒绝 multi-line prompt 强制 operator 改用 CLAUDE.md 文件路径。
- [ ] **R224-SEC-4 — `defaults.Prompt` 全局默认 prompt 不经 project.ValidateConfig（P2）**: `manager.go:344` 全局默认通过 `validateArgvStrings` 仅查 C0 字节，但 `project.ValidateConfig` 还查 IsLogInjectionRune（C1/bidi/LS-PS）。bidi 字符可绕过到 `--append-system-prompt` argv。方案：对 `cfg.Projects.PlannerDefaults.Prompt` 同样调 `project.ValidateConfig`。
- [ ] **R224-SEC-5 — `WriteTimeout: 60s` 对大文件流式响应不足（P2）**: `server.go:870` 全局 60s 写超时；50MB raw 文件下载在慢网络可被截断。WS 走 Hijack 不受影响，但 preview/raw/download 受影响。方案：对文件路由用 `http.ResponseController.SetWriteDeadline` 扩展或抬升全局并对其他路由 per-handler 收紧。
- [ ] **R224-SEC-6 — `dashboard_auth.go` HTTP 部署仍创建 non-Secure cookie（P3）**: `dashboard_auth.go:300` `Secure: a.isSecure(r)` 在 HTTP 下创建 non-Secure cookie；启动 warn 但 cookie 仍设。考虑公网 HTTP 部署下拒绝 cookie 创建（breaking）或加强 warn 频率。

### 性能 — 本轮新发现 / 重申

- [~] **R224-PERF-2 — `handleList workspaces []string + wsMap map[string]string` 1 Hz × N tab 重复分配（P2 R219-PERF-2 未覆盖 inner alloc）**: `dashboard_session.go:394` storeGen 命中 in 顶层缓存之前已分配 workspaces slice + wsMap。方案：workspaces 走 sync.Pool（参考 listRefsPool R222-PERF-10 先例）；ResolveWorkspaces 接受 caller 复用 map 或缓存 wsMap 与 storeGen 绑定。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R224-PERF-3 — `Snapshot()` `TurnAgents()` RLock + slice copy 在 sub-agent turn 期间频繁触发（P3 R219-PERF-3 / R222-PERF-7 未覆盖）**: `managed.go:894` 当 turnAgentCount > 0 时仍进 RLock + copy；500 Snapshot/s × sub-agent turn 与 SetAgentInternalID WLock 竞争。方案：`TurnAgents()` 改 atomic.Pointer[[]Agent] copy-on-write，applyEntryStateLocked 修改时 atomic.Store 新 slice。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R224-PERF-6 — `ACPProtocol.parseSessionUpdate` 嵌套三次 json.Unmarshal（P2）**: `protocol_acp.go:474, 476` 相比 stream-json 双解码再多一次 update.Update.Content 嵌套解码。方案：声明 Content 为 `json.RawMessage` 推迟第三次解码，或与 R222-PERF-1 / R222-PERF-3 同批做 ReadEvent([]byte) 接口改造。

### 代码质量 — 本轮新发现

- [~] **R224-CR-6 — `process_turn.go` 文件级 ownership 注释列错位（误报关闭 2026-05-20）**: 条目自身已记「无操作，当作澄清」——`isChanAlive` 在同文件第 188-193 行确实由本文件拥有，文档与代码自洽，是 reviewer 自己的误报。本批 PR #168 关闭归档。
- [ ] **R224-CR-9 — `dispatch.replyTracker` 240 行 5+ 职责（P2 god object 苗头）**: edit-banner 速率限制 / todo 去重投递 / askQuestion 卡片回送 / initial-Reply lifecycle / loopWG 协调 5 类正交职责挤一对象 + 7 同步原语。方案：拆 BannerEditor / TodoStreamer / AskQuestionPoster / TurnLifecycle 4 对象，replyTracker 退化 wiring 容器。

### 架构 — 本轮新发现 / 重申

- [ ] **R224-ARCH-1 — `backend.Profile` registry 未真正用满，DisplayName/CanonicalID/CostUnit 仍三处独立 switch（P1）**: `wrapper.go:75-83`/`session.costUnitForBackend`/`cli/wrapper.go normalizeBackendID` 各自硬编码；新加 backend 必须改三处。方案：DisplayName/CanonicalID/CostUnit/DefaultCwdInheritance 全收敛 backend.Profile，cli + session 改 `backend.MustGet(id).XXX`。
- [ ] **R224-ARCH-2 — `dispatch` 直 import `internal/cron` + 持具体 `cron.Job` 类型（P1）**: `dispatch.go:16,49,121,344` + `commands.go:13,344` 把 cron 限制常量 + 增删改全绑死单一 cron 实现。方案：定义 `dispatch.CronGateway` interface，server 注入实现，dispatch 仅依赖接口；cron 限制常量改 `cron.Limits{}` 通过 gateway 暴露。
- [~] **R224-ARCH-3 — `ManagedSession.SubagentLinker() *cli.SubagentLinker` / `AgentEventLog() *cli.EventLog` getter 暴露 cli 包内部具体类型（P1 R219-ARCH-3 未覆盖核心层）**: server / wshub_agent 拿到 cli 内部对象直接调；ACP 等无 SubagentLinker 概念的 backend 整链路硬编码 nil。方案：`session.AgentIntrospector` interface（Linker() AgentLinker / EventLog() AgentEventStream），cli.Process 实现。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R224-ARCH-4 — `cron` 持 `platforms map` 直调 platform.Reply 绕过 dispatch（P1 R219-ARCH-8 重申，强调 dispatch 出站层已存在却被 bypass）**: cron 走出来的 reaction / 节流 / queue ack / split 逻辑都和 IM 路径不一致。方案：抽 `dispatch.OutboundNotifier`，server 注入实现，cron 持 Notifier 而非 platforms map。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R224-ARCH-5 — `processIface` 30+ 方法 god + 多处 `proc.(*cli.Process)` type-assert 偷越层（P1 R215-ARCH-P1-3 加证据）**: 11 处 type assertion 是 god 接口治理失败的具体证据。方案：拆 `ProcessLifecycle` + `EventSource` + `BackendIntrospector` + 可选 `AgentIntrospector`，type-assert 路由由调用方做。
- [~] **R224-ARCH-6 — `session/router.go:31-34` blank import + 真依赖混用（P1 R219-ARCH-2 未覆盖分支）**: `merged` / `naozhilog` 是真 import 不是 blank。R219-ARCH-2 的工厂方案不充分，必须把 merged + naozhilog 同样登记到 history factory。方案：`cli.RegisterHistoryFactory` 体系扩展或独立 `MergedSource` 工厂；blank import 集中到 cmd/naozhi/wireup.go。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R224-ARCH-7 — `Protocol` 接口默认 `ProtocolCaps` 反向枚举 "非 ACP → StreamJSON=true"（P1）**: `protocol_acp.go:88-108` `StreamJSON: p.Name() != "acp"` —— 新加 backend "gemini" 会被错误归类为 StreamJSON。方案：Protocol 必须实现 `Capabilities() Caps`，删除反向推断 + SupportsX 双轨；P0 先把 default 改 `StreamJSON: false` 更安全。
- [~] **R224-ARCH-8 — `Server` 24+ 字段 / `Hub` 30+ 字段都已塌陷为新上帝包前夜（P1 R222-ARCH-6 + Server 自身未覆盖）**: scheduler/uploadStore/scratchPool 通过 setter 后注入造成半构造对象（R219-ARCH-5 已登记），新增功能继续往 Server / Hub 加。方案：Server 退化为门面，Hub 降级为 handler group 之一；新增 `internal/server/wireup` 子包做 NewServer。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R224-ARCH-9 — `dispatch.SendFn / TakeoverFn` 函数指针注入仅是反向依赖错觉（P2）**: 函数签名仍含 `*ManagedSession / cli.ImageData / cli.EventCallback`，dispatch 早已知道 cli 包结构。方案：要么 `session.Sender interface { Send(...) (Result, error) }` 彻底切 cli 类型，要么删 Fn 注入直接调 sess.Send。Breaking：是。
- [ ] **R224-ARCH-10 — `cli.Process` 内嵌 shim 协议字段 (`shimConn / shimR / shimW / shimWMu / shimCloseOnce / cliPID / shimPID`) 把 shim 协议焊进通用 Process（P2）**: 未来非 shim backend（in-process Go SDK / WebSocket 远端）无法复用 Process。方案：抽 `cli.Transport` interface（Read/Write/Close/PID），shim 一种实现、in-process 另一种；Process 持 Transport。Breaking：是。
- [ ] **R224-ARCH-11 — `backend.RegisterDefaults / EnsureDefaults` 无显式 wiring 顺序契约（P2）**: replyTagForBackendOnce 兜底自注册 + profile.go 警告 "不要 recover"，两套规则互相冲突。新加 backend 漏在 main 注册会触发 lazy 注册产生不同 Profile 视图。方案：把 EnsureDefaults 挂 backend 包 init()（接受不可逆 side-effect）或 main 启动加 `assertBackendRegistered`。
- [ ] **R224-ARCH-12 — `dispatch.NewDispatcher` 在 `cfg.Resolver==nil` 时 fabricate fallback resolver 双轨（P2）**: production 主 resolver vs test 各处 fallback 行为靠 code review 维持等价；KeyResolver.NewKeyResolver 签名变化时 fallback 容易漏改。方案：dispatch.NewDispatcher 把 Resolver 改必填；test 显式构造 minimal resolver 或 fabricate 上移到 server.New 顶层。
- [ ] **R224-ARCH-13 — `session/scratch.go` ScratchPool 与 managed sessions 共用 router.sessions map（P2）**: 通过 ScratchKeyPrefix 在落盘/sidebar 处过滤；store 路径热扫整 map 受 scratch 拖慢，sweep 路径要小心绕开主 cleanup。方案：scratch 走独立 `scratchSessions map`，主 sessions map 完全不知道 scratch；或抽 `internal/session/scratch` 子包。
- [~] **R224-ARCH-14 — `dashboard_*.go` 1000-1700 行 god file（P2 R222-CR-2/3 文件级未覆盖）**: dashboard_send.go 1000+ / dashboard_session.go 1000+ / dashboard_cron.go 1700+，并行 PR 物理冲突。方案：复用 router-split-design.md 同款方法论，给 dashboard_send / dashboard_cron 各自起 split-design ADR。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R224-ARCH-15 — `upstream/connector_rpc.go` 522 行 18-case switch（P2 R222-CR-2 架构层补）**: method dispatch table（map[string]Handler）+ per-method handler 文件取代 switch + 单文件 522 行。
- [ ] **R224-ARCH-16 — `platform.SupportsInterimMessages / AsReactor / AsQuestionCardSender` 隐式 plugin 协议无 capability registry（P2）**: 调用方必须知道有哪些 capability，新加 capability 要全仓库 grep。方案：`platform.Capability` 枚举 + `Platform.Capabilities() []Capability`，或 `RegisterCapabilityCheck` 让 dashboard /health 能枚举 platform 实例能力面。
- [ ] **R224-ARCH-17 — `internal/agentregistry` 单点缺失（P3）**: agent 配置在 dispatch.agents / KeyResolver.agents / router 三层各自持，hot reload 顺序不确定。方案：`internal/agentregistry`，dispatch / resolver / router 都通过 registry 读；hot reload 走 registry signal。
- [ ] **R224-ARCH-18 — `Hub.wiredLinkers` pointer-based map key 脆弱（P3 R219-ARCH-3 落地建议）**: 未来 Linker 池化复用会让 wiredLinkers 误判。方案：linker 构造时分配 chrono uuid，map key 改 string ID 与 cli 内部对象身份解耦。
- [ ] **R224-ARCH-19 — `contract_test.go` 把 4 个 consumer interface 集中绑死 `*session.Router`（P3）**: session 成事实 API 中心。方案：var _ 断言拆到各 consumer *_test.go，删除 internal/session/contract_test.go 反向链。

## Round 222 — 5-agent 并行 review 第 35 轮（2026-05-17）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 80 条发现。
> 12 条 FIX-READY 已落地本轮 PR（sanitizeResumeLastPrompt IsLogInjectionRune 全开 / shim
> SetReadDeadline 错误传播 / cron snapshotJob RLock / rawScanSubagentsDir meta size cap /
> handleAttachment Lstat / filterShimEnv per-entry size cap / capExtraArgsBytes ARG_MAX
> 守卫 / sanitizeDownloadName 调 IsLogInjectionRune / feishu maxEventIDLen+maxIncomingTextBytes
> 抽 const / platform.DefaultMaxReplyLen / feishu webhookTimestampFutureSkew+webhookTimestampMaxAge /
> wrapper 错误消息小写 / ImageData godoc Deprecated / eventlog_integration_test 改用
> testhelper.Eventually）。
> 以下是需设计决策、破坏兼容、跨包重构、或方案不唯一不适合本轮直接修的条目。

### 安全 — 需设计或部署影响

- [ ] **R222-SEC-4 — `nz_anon` cookie 在无 TLS 部署下不带 Secure（P3）**: 启动期已有"未启 TLS"告警，但 cookie 自身不 fail-closed，攻击者同网络可窃取并认领 pending 上传。方案：多用户模式强制 Secure/no-TLS 启动失败。涉及：`internal/server/dashboard_send.go:44-50`。

### Go 正确性 — 跨包改动 / shutdown 协调

- [ ] **R222-GO-1 — `cron.executeOpt` 用 `context.Background()` 起 sendCtx，绕开 stopCtx 取消（P2）**: scheduler.go:1853 注释说为避免 shutdown 误记 cancel，但 Stop 后 Send 仍可阻塞 jobTimeout（最多 5 min），triggerWG.Wait 因此可能超 stopBudget。方案：sendCtx 来自 stopCtx 派生 + 短 grace；或在 wg goroutine 文档化"intentional orphan"。涉及：`internal/cron/scheduler.go:1853`。
- [~] **R222-GO-3 — `cli.SubagentLinker.fireOnResolveLocked` 释放重取 mu 让 callback 跑期间存在 nested mu-release race（P2）**: 重入安全契约靠 godoc 维系，无静态守卫；callback 若再调 linker.Query 进入嵌套路径可能死锁。方案：copy fns 后释放两锁外执行所有 callback，移除 re-lock。涉及：`internal/cli/subagent_link.go:556-568`。 — 归档 2026-05-23：R227-GO-3 已重命名 fireCallbacksDropLock 并补 onResolveMu copy-then-drop；callback 进入 Query 走 RLock 不死锁（l.mu 已 Unlock），进入 Resolve 走自身 mu 获取顺序；本批 subagent_link.go 加 godoc 锚点说明锁分析

### 性能 — 协议接口或大重构

- [~] **R222-PERF-1 — `cli.Protocol.ReadEvent(string)` 每 stdout 行做 `[]byte(line)` heap copy（P1，重申 R67-PERF-1）**: ReadEvent 接口签名为 string，内部强制再分配 []byte 给 json.Unmarshal。5-50 evt/s × N session × 50-4KB 持续 alloc。方案：接口改 `ReadEvent([]byte)`，protocol_claude/protocol_acp + readLoop 同步。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R222-PERF-2 — `shimWriter.Write` 双路径都 `string(data[:len-1])` 拷贝到 shimClientMsg.Line（P1，重申 R71-PERF-H1）**: shimClientMsg.Line 是 string 而非 json.RawMessage；改 RawMessage 可零拷贝。Breaking：shim 协议字段类型变。涉及：`internal/cli/process_shim_io.go:54,83`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R222-PERF-3 — `readLoop` 每条 stdout 行做两次完整 JSON decode（P1）**: 先 Unmarshal shimMsg（含 `Line string`），再 ReadEvent 内 Unmarshal Event。最坏 2500 double-decode/s。方案：shimMsg.Line 改 json.RawMessage 直接传 ReadEvent；包内部改不 breaking。涉及：`internal/cli/process_readloop.go:199-207`。
- [~] **R222-PERF-4 — `eventPushLoop` 同 session N tab 各自 marshalPooled（P2，重申 R219-PERF-1 + R214-PERF-4）**: 10 tab × 50 session × 50 evt/s 最坏 25000 独立 JSON 编码/s。方案：Hub 层 per-key 单广播 goroutine 序列化一次后 fan-out 共享 []byte。涉及：`internal/server/wshub.go:1070`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R222-PERF-5 — `marshalPooled` 每次 make+copy 输出（归档 2026-05-23）**: 同 R225-PERF-10 一并跟踪。SendRaw 路径 c.send 通道排队，多 sub 同 buffer 必须保证寿命跨多个 select+conn.Write，调方 put 回 pool 时机难定。当前 copy 一份是 invariant：消息字节归 c 拥有，sub 互不影响。本批 PR 归档。
- [~] **R222-PERF-6 — `handleList` storeGen 不变仍重建 sessionWorkspaces map / workspaces slice（P2，重申 R219-PERF-2）**: 引入 lastListVersion+lastListJSON 缓存命中直接 Write。涉及：`internal/server/dashboard_session.go:388`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R222-PERF-7 — `Snapshot()` 8 次顺序 atomic.Pointer.Load（P2，重申 R219-PERF-3 / R215-ARCH-P2-7）**: 打包 immutableBox + mutableBox。涉及：`internal/session/managed.go:841-857`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R222-PERF-8 — `invokePersistSinkSingle` 单槽 slice heap escape（P2，重申 R219-PERF-4）**: 需 -benchmem 验证后再决定 sync.Pool 或栈数组方案。涉及：`internal/cli/eventlog.go:653`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R222-PERF-9 — `newEventLogSink` 每 entry make+copy 出 pooled buffer（归档 2026-05-23）**: 同 R216-PERF-3 / R217-PERF-3 / R226-PERF-2 / R227-PERF-3 / R230B-PERF-6 同根因。eventlog_bridge.go single-entry fast path 已用 bridgeEncPool 复用 encoder + buffer（R228-PERF-1）；剩余 make+copy 是 PersistSink 留持契约要求，消除拷贝需要新接口（breaking）。统一收敛到 R230B-PERF-6，本批 PR 归档。
- [ ] **R222-PERF-11 — `EntriesSince` 多 tab 同 EventLog 各自调 + 各自 marshal（P2）**: EventLog 引入 last-batch JSON 缓存，notify 时直接复制。涉及：`internal/cli/eventlog.go:898-929`。

### 代码质量 — 方法过长 / 共享 helper

- [ ] **R222-CR-1 — `session.NewRouter` 350 行 6+ 阶段直列（P2）**: 拆 initWrappers / loadPersistedState / startEventLogPersister / startBackgroundLoops。涉及：`internal/session/router.go:706`。
- [ ] **R222-CR-2 — `upstream/connector_rpc.go handleRequest` 522 行 18-case switch（P2）**: 抽 (*Connector).handleSend/handleTakeover 等私方法。涉及：`internal/upstream/connector_rpc.go:50`。
- [ ] **R222-CR-3 — `session.reconnectShims` 334 行 9-state enum + 嵌套 goroutine（P2）**: 拆 classifyAndPlanShimAction + executeShimAction。涉及：`internal/session/router.go:1276`。
- [ ] **R222-CR-4 — `cron.executeOpt` 247 行（P2）**: 抽 recordAndBroadcastRun。涉及：`internal/cron/scheduler.go:1677`。
- [ ] **R-LEGACY-SEND — 删除 `Hub.sessionSendLegacy` 与其在 `sessionSend` 中的 fallback 分支（LOW，由 R222-CR-8 派生）**: sessionSendLegacy 是 MessageQueue 接入前的旧 send 路径，现仅供未配 Queue 的测试代码使用。NewHub 已在 Queue==nil 时打 slog.Warn，但仍允许构造。Removal 条件：(1) 所有驱动 Hub 的测试迁到真实 MessageQueue（或与其投递契约一致的 stub）；(2) NewHub 把 Queue==nil 从 Warn 升级为 Fatal（构造期 hard-fail）。两条都满足后，删除 sessionSendLegacy + sessionSend 中调它的 if 分支，把 guard/interrupt 语义收敛到唯一一处。涉及：`internal/server/send.go:561`，调用方在 `internal/server/send.go` sessionSend 内部分支。

### 架构 — 大重构 NEEDS-DESIGN

- [ ] **R222-ARCH-1 — `session.Router` 已是 god object（73 方法 / ~20 字段 / 4100 行单文件）（P1）**: 拆 sessionStore + procPool + shimReconciler + persistenceCoord + historyLoader 五子组件，Router 退化为门面；contract_test.go 守的对外契约不破。是当前最大的架构债。
- [ ] **R222-ARCH-2 — shim 协议细节泄漏到 session 层（P1）**: session/router.go 直调 `shim.SocketPath/KeyHash/WaitSocketGone/ServerMsg/State`；cli.Wrapper 应吸收为 `WaitSocketGoneForKey(key,dur)` + `Reconnect(ctx,key,lastSeq) (*Process, midTurn, err)`。
- [ ] **R222-ARCH-3 — `internal/config` 反向 import `internal/session`（P1）**: 仅为读 `session.DefaultMaxProcs` 一个常量。方案：抽 internal/sessionconst 或 internal/defaults 子包，session/config 都依赖它。
- [ ] **R222-ARCH-5 — server 直接持有大量 cli.* 类型，绕过 session 抽象（P1）**: 14 个 server 文件 import cli.Attachment/ImageData/EventEntry/SubagentLinker。方案：扩展 platform.Attachment 或抽 internal/dispatch/dto。
- [~] **R222-ARCH-6 — Hub god-object 苗头（35+ 方法 / 1700 行）（P2）**: 拆 Hub + SubscriptionRegistry + MessageBroker。 *(PR #327 R243-ARCH-2 已 6-stage split：wshub.go 2028→525 行，方法分到 5 个职责文件。35+ 方法已物理分散；struct 层 SubscriptionRegistry/MessageBroker 抽出跟踪到 R248-ARCH-6。)*
- [ ] **R222-ARCH-7 — `processIface` 30+ 方法（P2）**: 按 core/status/events/agents 切分四个小接口。
- [ ] **R222-ARCH-8 — graceful shutdown 顺序约束散在 main.go + server.go + router.go（P2）**: 引入 internal/lifecycle.Coordinator + 拓扑 Stop。
- [ ] **R222-ARCH-9 — env 探测散在 5+ 处独立读取（XDG_RUNTIME_DIR / HOME / APPDATA）（P2）**: Config.Paths 子结构集中；config.Load Normalize 阶段单点探测。
- [ ] **R222-ARCH-10 — ManagedSession 语义标签隐式（exempt 字段 / Stub via RegisterCronStub / Scratch via key prefix / Paused via process==nil）（P2）**: 抽 SessionRole + SessionMode 两枚举。
- [ ] **R222-ARCH-11 — workspace 三义混用（Config.Workspace / Config.Workspaces / Session.Workspace / Router.workspace / Router.workspaceOverrides）（P2）**: 改名 NodeIdentity / RemoteNodes / DefaultCWD；YAML 字段保 alias。
- [ ] **R222-ARCH-12 — spawn 路径 workspace 决策散在 5+ 优先级点（P2）**: 抽 WorkspaceResolver（参考 KeyResolver 解耦经验）。
- [ ] **R222-ARCH-13 — eventlog 子系统散在 Router/ManagedSession/cli.EventLog/persist.Persister/attachment.Tracker 五处（P2）**: 抽 internal/session/eventlogpipeline 子包封装。
- [ ] **R222-ARCH-14 — backend 信息散在 Router 4 字段 + ManagedSession atomic + Wrapper（P3）**: type Backend struct 聚合，Router 持 backends map[string]*Backend。
- [ ] **R222-ARCH-15 — cli.Event vs cli.EventEntry vs cli.EventLog 命名空间冲突（P3）**: 改 WireEvent + LogEntry。Breaking：是（cli 包外引用多）。
- [ ] **R222-ARCH-16 — discovery 包职责不单一（pure path util + ClaudeProjectSlug + LoadHistory）（P3）**: 拆 discovery/path + discovery/proc + history/claudejsonl 合并。
- [ ] **R222-ARCH-17 — Router 持 history 子系统 ctx/wg（lifecycle 越权）（P3）**: 与 R222-ARCH-1 一并迁。

## Round 220 — 5-agent 并行 review 第 34 轮（2026-05-17）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 110 条发现。
> 9 条 FIX-READY 已落地（本批 PR：删 ListAllJobs/NextRunByID 死代码、validateCronTitle godoc、
> ManagedSession.GetLastActive→LastActive、cli/session loadAtomicString 命名统一、
> notify_chat_id 错误消息加 limit、scheduler.platforms/agents immutable 注释、
> AppendBatch nil-sink fast-path、BackendIDs 排序结果缓存、url_verification skip nonce dedup）。
> 以下是需设计决策、破坏兼容、跨包重构、或方案不唯一不适合本轮直接修的条目。

### 安全 — 需 operator 决策

### Go 正确性 — 跨包改动

- [ ] **R220-GO-1 — `cli.SubagentLinker.Resolve` retryInterval 250ms `time.Sleep` 无 ctx 中断（P1，与 R219-GO-1 同批修复）**: Resolve 多次 retry 间隔用 `time.Sleep(l.retryInterval)`，shutdown ctx 取消时 goroutine 仍要等满 retry 间隔（最多 12 次 × 250ms = 3s）。方案：`select { case <-time.After(l.retryInterval): case <-ctx.Done(): return }`，与 R219-GO-1 接 ctx 改造同批进行。涉及：`internal/cli/subagent_link.go:288,326`。
- [ ] **R220-GO-2 — `project_files.go` `serveRender`/`servePreview`/`serveRaw` 三处独立二次 os.Open 共享 R219-SEC-2 TOCTOU 缺口（P1，扩展 R219-SEC-2）**: R219-SEC-2 只点了 serveRender，但 servePreview（第 742 行）与 serveRaw（第 836 行）也各自独立 `os.Open(resolved)` 二次读盘；preview 路径还把内容塞进 JSON 返回。方案：handleFileGet 统一在 Lstat 阶段持已 open 的 *os.File 传递到三个 helper，避免 double-open race。

### 性能 — 协议接口变更或需 benchmark

- [ ] **R220-PERF-1 — `countActive()` evictOldest/Takeover/spawnSession 路径全 map scan（P1）**: 4 个 caller 各自 `r.mu.Lock()` 下做完整 map 扫描，500 session 量级会显著增加锁内 CPU；Cleanup 已用 `newActive` 增量，evict/takeover 没接。方案：传 `delta int` 给热路径做原子加，countActive 仅在 Cleanup 全量重算。涉及：`internal/session/router.go:2126,2400,2472,4067`。
- [ ] **R220-PERF-3 — `EventLog.EntriesSince` 初始 catch-up 在 RLock 下复制 500 entry × 512B（P2）**: 反向扫描+复制全在 l.mu RLock 内，subscriber 初始订阅时阻塞 Append 一段时间。方案：先 snapshot ring 索引（head/count），release RLock，再在临时 slice 内拷贝。涉及：`internal/cli/eventlog.go:869`。
- [ ] **R220-PERF-4 — `Cleanup` pass2 对 candidate 做 proc.Alive + proc.IsRunning 二次锁获取（P2）**: pass1 在 r.mu RLock 下收集 candidate proc 指针，pass2 又对每个 candidate 取 `proc.mu.RLock` 跑 IsRunning，与热 Send 路径锁竞争。方案：pass1 同时 capture proc.GetState() 一次，pass2 直接读 state。涉及：`internal/session/router.go:2920-2946`。
- [~] **R220-PERF-5 — `hub.debounceMu` 高频锁获取无 atomic 短路（归档 2026-05-23）**: BroadcastSessionsUpdate 函数体内 4 个互斥状态分支（debounceClosed / 已 timer 在跑 / 超 maxDelay / 全新 timer），都需 debounceMu 保 timer 重入与 clientWG.Add 配对；纯 atomic.Bool 不能取代 4-way 决策。300/s acquire 远不是性能瓶颈。本批 PR 归档。

### 代码质量 — 错误消息一致性

## Round 218 — 5-agent 并行 review 第 32 轮（2026-05-16）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 100+ 条发现。
> 6 条 FIX-READY 已落地（PR #40 + PR #22）。以下是需设计决策、破坏兼容、跨包重构、
> 或方案不唯一不适合本轮直接修的条目。

### Go 正确性 — 跨包改动

- [ ] **R218B-GO-3 — `readLoop` linker.Resolve goroutine 无 context 绑定（P1）**: `go linker.Resolve(taskID, toolUseID, ...)` 启动时无 cancellation。进程 shutdown 后 Resolve 可能继续访问磁盘。方案：`linker.Resolve` 接受 ctx 参数，绑定到 process 生命周期。涉及：`internal/cli/process_readloop.go:324`，`internal/cli/subagent_link.go`。Breaking：是（接口变更）。

### 安全 — 新发现（非重复）

### 性能 — 需 benchmark 确认

- [~] **R218B-PERF-1 — `resubscribeEvents` 每次调用 `time.NewTimer` 分配（归档 2026-05-23）**: 条目自身备注"实际影响有限"。wshub.go 已用单 Timer + Reset 在 12 iter 间复用，只剩 cold-path 1 alloc；池化引入 lifetime 复杂度反而高风险。本批 PR 归档。
- [~] **R218B-PERF-2 — `ownerLoop` 每次 collect 窗口 `time.NewTimer` 分配（误报关闭 2026-05-23）**: 实地复核 dispatch.go ownerLoop — collectTimer := time.NewTimer(...) 在 ownerLoop 函数体内**只分配一次**，loop 内通过 collectTimer.Reset(...) 复用。ownerLoop 是 per-session-key 协程（每 key 至多 1 个并发），不是"每条消息"的热路径。本批 PR 归档。

### 架构 — 新发现

- [ ] **R218-ARCH-2 — 4 个 consumer SessionRouter 接口定义方法重叠但无共享基础**: dispatch/cron/server/upstream 各声明独立 SessionRouter，方法签名漂移只能靠 contract_test 间接检测，无法共享 `CoreRouter` 提供编译期强绑定。方案：定义 `session.CoreRouter` interface，4 个包 embed 扩展。非 breaking，中等工作量。
- [~] **R218-ARCH-3 — Protocol 接口 SupportsX / Capabilities 双轨（R214-ARCH-1 重申）**: Protocol 同时有 SupportsReplay/SupportsPriority 和 Capabilities() Caps，新 backend 实现者不清楚该实现哪个。建议撤除老 Supports* 方法，强制 Capabilities() 单一入口。Non-breaking，小工作量。`internal/cli/protocol.go`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R218B-ARCH-2 — `Dispatcher.projectMgr` 与 `resolver` 双信息源（P3）**: `projectMgr` 仅用于 slash-command UX，`resolver` 持有 DataSource；并发修改下两者可能对同一项目产生不一致视图。方案：将 slash-command 的 projectMgr 访问路由到 resolver 暴露的接口，统一信息源。涉及：`internal/dispatch/dispatch.go:39-84`。

### 代码质量 — 新发现

## Round 217 — 5-agent 并行 review 第 31 轮（2026-05-13）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 104 条发现。
> ~18 条 FIX-READY 已落地（详见 git log）。以下是需设计决策、破坏兼容、跨包重构、
> 或方案不唯一不适合本轮直接修的条目。

### 安全 — Breaking / 需 operator 决策

- [~] **R217-SEC-1 — `AgentOpts.ExtraArgs` 缺 flag allowlist（重申 R216-SEC-2）**: dashboard agent 编辑用户可在 `agents.*.args` 写入 `--mcp-config /host/secret` / `--append-system-prompt` 加载任意配置。配置层 `validateArgvStrings` 仅拒控制字节、不限制 flag 名。方案：`BuildArgs` 调用前对每元素 allowlist（`--model` / `--add-dir` / `--max-turns` / `--append-system-prompt`），或在 `validateConfig` 阶段明确允许的 flag 集合。**Breaking**：需要枚举所有现存 backend args 配置。涉及：`internal/cli/protocol_claude.go:56`、`internal/session/router.go:1959`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R217-SEC-2 — 远端 node workspace 仅做语法校验（重申 R61-SEC-2 设计）**: `dashboard_send.go:773 validateRemoteWorkspace` 仅 path-shape 检查，不调 EvalSymlinks（远端 root 在另一台机器无法本地 resolve）。当前注释承认这是设计意图。后续要做 cross-node trust：要么强制远端节点本地 EvalSymlinks 并回传校验结果，要么 dashboard 把 workspace allowlist 配在主节点。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R217-SEC-4 — `gogo/protobuf v1.3.2` CVE-2021-3121（间接依赖）**: aws-sdk-go-v2 间接依赖。naozhi 不直接调用，但为消除告警可 `go get github.com/gogo/protobuf@latest` 或在 go.mod 加 replace。
- [ ] **R217-SEC-5 — `golang.org/x/crypto v0.49.0` 偏旧（约 10 个月）**: 无已知 critical CVE 但建议跟随 toolchain 升级。
- [ ] **R217-SEC-6 — `dashboardToken` 轮转无显式 session 失效机制**: 当前依赖 cookieMAC(secret, dashboardToken)，token 改后旧 cookie 自然失效，但需要 process restart。增设 server-side session generation counter 才能不重启即时撤销。Breaking：需要持久化 generation 状态。
- [ ] **R217-SEC-7 — `writeJSON` 全局 `SetEscapeHTML(false)`**: 当前是 Feishu 卡片需求；defense-in-depth 上应分离 Feishu encoder pool 和通用 API encoder pool。Breaking：Feishu 卡片含 `<>&` 时输出会变。
- [ ] **R217-SEC-8 — `/health` 在认证后返回 workspace_id / node 状态等运营情报，cleartext HTTP 部署可被嗅探**: 部署侧问题，添加启动 warning 即可：non-loopback bind + 无 TLS terminator 时提示。

### Go 正确性 — 跨包改动 / 需 ctx 传递

- [ ] **R217-GO-1 — `Guard.lastWait` 在不发起 Release 的路径下永久泄漏**: 现状 ShouldSendWait 写 + Release 删；不调 Release 的 path 留下永久 entry。方案：sync.Map+TTL sweep（如 seenNonces），或显式 cap+LRU。
- [~] **R217-GO-3 — `historyCtx` 派生自 `context.Background()` 而非 app ctx（重申 R216-GO-4）**: 异常退出路径下 historyWg goroutine 不被取消。需 NewRouter 收 appCtx（构造函数签名变化）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R217-GO-4 — `spliceLog` 每 record `json.Unmarshal` 取已知 seq**: 重新解码 record body 只为读 seq；可由 idxEntries 索引位置直接拿。改动需谨慎（保证 seq 不被外部恶意改）。
- [ ] **R217-GO-5 — `cron.Stop()` deadline 后泄漏 triggerWG goroutine（R44 重申）**: 单 shot 设计可接受；测试 -count=N 污染。长期需重构 triggerWG/Stop 协议。
- [~] **R217-GO-6 — cron `sendCtx` 派生自 Background()（归档 2026-05-23）**: 同 R216-GO-7 / R230B-GO-1 同根因（重复登记）。统一收敛到 R230B-GO-1 跟踪，本批 PR
- [~] **R217-GO-7 — `storeStringAtomic` fast-path 可能 silently no-op `deathReason` 清空**: managed.go:254 注释自承"逻辑 race"。需用 `Store(new(string))` 强制材料化清空，或加专用 `clearDeathReason` 方法。 — 评估关闭（已归档 2026-05-23 复核）：`internal/textutil/atomic_string.go:32-46` StoreAtomicString godoc R219-CR-1 详细论证 fast-path 安全性——last-writer-wins 语义下，若我们的 s 与已观察值相等，写入与不写入产生相同可见状态；deathReason "" 清空路径在 cli.EventLog 上下文持 l.mu 串行化，跨包路径靠 atomic.Pointer 单写者保证。configure with `internal/textutil/atomic_string_test.go:62-90` TestStoreAtomicString_ConcurrentWriters 已锁定 last-writer-wins 契约。逻辑 race 注释指的是"两个不同写者并发写不同值，最终值不确定"——这是 atomic 设计本身的语义而非 bug。本批 PR。

### 性能 — 协议接口变更或需 benchmark

- [~] **R217-PERF-1 — `ClaudeProtocol.ReadEvent(line string)` 双 copy（重申 R216-PERF-1 / R67-PERF-1）**: 接口改 `[]byte` 跨 cli/session。Breaking：协议接口。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R217-PERF-2 — `shimWriter.Write` `string(line[:n-1])` 全消息 copy（R216-PERF-2 重申）**: shimClientMsg.Line 改 json.RawMessage 或加 `shimSendRaw`。Breaking：shim 协议。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R217-PERF-3 — eventlog_bridge.go:49 per-EventEntry `json.Marshal`（归档 2026-05-23）**: 同 R216-PERF-3 同根因；R219 已落地 pooled encoder（见顶部 "eventlog_bridge pooled encoder"），剩余 single-entry raw bytes copy 由 R230B-PERF-6 跟踪。统一收敛到 R230B-PERF-6，本批 PR
- [ ] **R217-PERF-5 — `pendingIdx` 未预 cap（R216-PERF 重申）**: `make([]schema.IdxEntry, 0, IdxStride*2)`。需 benchmark 确认收益值得增量驻留内存。
- [ ] **R217-PERF-6 — `selectForIdx` 每 flush 新建 slice**: caller-owned scratch 改造。Breaking：函数签名。
- [~] **R217-PERF-7 — `marshalPooled` 对小重复帧（session_state running/ready）总是 copy（归档 2026-05-23）**: 同 R225-PERF-10 同根因（`marshalPooled` 每次 copy 一份独立 backing；高频 broadcast 下不可避免；考虑对固定组合 session_state 做 LRU 缓存）。统一收敛到 R225-PERF-10 跟踪，本批 PR
- [~] **R217-PERF-8 — `linker.Resolve` 每 task_started 事件 spawn goroutine（归档 2026-05-23）**: 同 R225-PERF-2（`process_readloop` `system/task_started` 无背压裸 `go linker.Resolve`，多 sub-agent 并发启动时短时间产生大量 goroutine）+ R230B-GO-2（Resolve 加 ctx 参数 + select stop signal）同根因。R218 已落地 `SubagentLinker goroutine 限并发`（见顶部 Round 218 摘要），但 worker pool 抽象未到位。统一收敛到 R225-PERF-2 跟踪，本批 PR
- [ ] **R217-PERF-10 — `dashboard_session.handleList` workspaces []string 每 poll alloc**: sync.Pool；需 benchmark + 仔细处理 escape。

### 架构 — 大重构

- [ ] **R217-ARCH-1 — `cli` 已塌陷成"领域类型仓库"被 9 个上层包横向引用**: `cli.EventEntry`/`cli.Event` 同时承担 stream-json 解析输出 + naozhi 内部事件模型 + node wire DTO + persist schema input + history Source。任何 cli 内部字段调整波及 9 包。方案：迁出领域类型到 `internal/event` / `internal/domain`，cli 单方面 produce、其他 consume。长期重构。
- [ ] **R217-ARCH-2 — `server` 直接 type-assert 持有 `*cli.SubagentLinker` / `*cli.EventLog`**: agent_tailer / dashboard_agent_events / wshub_agent 通过 `sess.SubagentLinker()` 拎 cli 内部对象。RFC v4 phase 3 规划的 `AgentIntrospector` 接口未落地。方案：扩 processIface 加 Linker/EventLog 方法，或下沉 tailer 注册到 session 包。
- [ ] **R217-ARCH-3 — `discovery` 反向依赖 `cli` 拿 EventEntry / TruncateRunes（菱形依赖）**: 形成 session→discovery→cli←session。`TruncateRunes`/`DeriveLegacyUUID` 是无状态字符串工具，应迁到 `internal/textutil`。
- [ ] **R217-ARCH-4 — 4 个互相重叠的 `SessionRouter` consumer 接口（dispatch/server/cron/upstream）**: 方法重复，新增 router 方法要在 4 处同步。合并为 `session.RouterFacade` 一个 facade interface。
- [~] **R217-ARCH-5 — `processIface` 30+ 方法 god 接口（R216-ARCH-1 重申）**: 拆 `ProcessCore` / `EventSource` / `PassthroughExt` / `Introspector` / `Sender`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R217-ARCH-6 — `Router struct` 28 字段（R216-ARCH-6 重申）**: 拆 eventLogManager / workspaceStore / historyLoader / shimReconciler。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R217-ARCH-7 — `NewRouter` ~335 行 + `executeOpt` 230 行 + `reconnectShims` 320 行**: 函数拆解。长期。
- [~] **R217-ARCH-8 — `Hub` 31 字段 + HubOptions 18 字段（R216-ARCH-4 重申）**: 提取 `nodeCache` / `subscriptionManager` / `agentTailerRegistry` 子 struct。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R217-ARCH-9 — `Protocol` 接口的 `protocol_acp.go` 实现是否在生产路径活跃**: 若仅占位、文档/测试出现，build-tag 隔离，避免接口被一个不上线的实现绑死。

### 代码质量 — 小改动等合并窗口

- [ ] **R217-CR-3 — `Cleanup` 三阶段加锁窗口**：worst-case stuckKill 目标进程在 Pass 2 已被 spawnSession 替换。`shouldPrune` 已 mitigates，stuckKill 路径未 re-check。需要 pass-2 再次 verify。
- [~] **R217-CR-4 — `Hub god struct 36 字段 / `node.Conn` 18+ 方法巨型接口**: 子聚合拆分。 *(PR #327 R243-ARCH-2 部分实施：wshub.go 文件 split 5 个职责文件；Hub struct 字段未拆（保锁不变量）。node.Conn 接口未动 — 跟踪 R231-ARCH-5。)*
- [ ] **R217-CR-5 — cross-node 错误注入方向不对称**: 反向有 LogSystemEvent，正向 node.Conn.Send 失败只 slog 不进 EventLog。
- [~] **R217-CR-7 — `project.DisplayName` / `Emoji` schema 校验但 dashboard UI 不读（归档 2026-05-23）**: 同根因 R216-CR-7（重复登记）+ R110-P2 父条目（"项目自定义显示名 + emoji（foundation 已落地 2026-05-10，UI 待续）"）已显式 own UI wire 工作。foundation 实地复核：`internal/project/validate.go:47-55,124-138` `ProjectConfig.DisplayName` (128 rune cap) + `ProjectConfig.Emoji` (8 rune cap) + `ProjectConfig_DisplayNameEmojiRoundtrip` / `ProjectConfig_LegacyYamlWithoutDisplayName` / `ProjectConfig_DisplayNameTooLong` 三测试已落地；`internal/server` 全包 grep `project.DisplayName / project.Emoji` 0 命中（UI/`/api/projects` 均未读）。统一收敛到 R110-P2 跟踪 UI 落地，本批 PR

## Round 216 — 5-agent 并行 review 第 30 轮（2026-05-12）NEEDS-DESIGN

> 本轮 5 个 reviewer 并行扫描共 100+ 条发现。15 条 FIX-READY 已落地（单独 commit，参见 git log）。以下是需设计决策或破坏兼容性、不适合本轮直接修的条目。

### 安全 — 破坏兼容 / 需 operator 决策

- [ ] **R216-SEC-1 — S14 `Feishu VerificationToken-only` 模式缺 body-HMAC（重申 P1）**: 5-agent security reviewer 本轮重申此为 P1。持有/嗅到 token 即可伪造任意事件（新 nonce 绕过 dedup）→ 触发 CLI 执行任意 prompt。方案：在 `validateConfig` 里将该模式升为 error，或引入 `feishu.allow_unauthenticated_webhook: true` 显式 opt-in。**Breaking**：影响未配 EncryptKey 的现有部署。
  - 涉及：`internal/platform/feishu/transport_hook.go:98-159`, `feishu.go:315, 400-403`
- [~] **R216-SEC-2 — `AgentOpts.ExtraArgs` 未做 flag 白名单（归档 2026-05-23）**: 同 R217-SEC-1 / R219-SEC-1 / R225-SEC-1 / R227-SEC-1 / R229-SEC-1 / R231-SEC-4 / R233-SEC-2 同根因多轮重申。Breaking 需枚举允许 flag。统一收敛到主条目 R231-SEC-4 跟踪，本批 PR
  - 涉及：`internal/cli/protocol_claude.go:56`, `internal/session/router.go:1923`
- [ ] **R216-SEC-3 — shim `LimitedReader` 每行 reset，累计字节无上限**: 控制 shim token 的攻击者可连续发送 16MB 行导致 shim OOM。方案：引入 session 级全局字节计数器，超限断开。
  - 涉及：`internal/shim/server.go:815`
- [ ] **R216-SEC-4 — S9 注销不撤销 cookie（重申）**: logout 仅清浏览器端 MaxAge=-1；服务端无 generation counter，被盗 cookie 24h 有效。方案：stateDir 存 cookie generation，注销递增，cookieMAC 纳入 generation。**Breaking**：现有 session 升级时需重新登录。
  - 涉及：`internal/server/dashboard_auth.go:302-313`
- [ ] **R216-SEC-5 — CLIPID 伪造进 cgroup（R31-REL3 未改）**: shim hello 消息里的 CLIPID 直接传 `sudo busctl`，未验证是否我们的 CLI binary。方案：用 `/proc/<pid>/exe` 反查验证。
  - 涉及：`internal/shim/manager.go:921-951 moveToShimsCgroup`
- [ ] **R216-SEC-6 — shim watchSocketFile 用 Stat 不用 Lstat**: 攻击者可用 symlink 欺骗使 shim 以为 socket 还存在。现有代码注释说是有意为之（便于 symlink replacement），但注释没论证安全边界。需重新评估 symlink trust 模型是否成立。
  - 涉及：`internal/shim/server.go:415`

### Go 正确性 — 需跨包改动

- [ ] **R216-GO-1 — `ReattachProcessNoCallback` 无 sendMu 保护（R51-CONCUR-002 再确认）**: reconcile 周期调用对运行中 session 发生，docstring 明确标注 "Send() 不在飞行中"，但运行期 reconcile 不满足该假设。需跨 managed.go / router.go 改 lock ordering，合并 RFC。
- [ ] **R216-GO-2 — `shim.Run()` package-level `shimLogFile *os.File` global**: 包级变量被 deferred panic handler 跨 goroutine 读取，race detector 会报。方案：改 local + closure，或 atomic.Pointer。
  - 涉及：`internal/shim/server.go:78`
- [ ] **R216-GO-4 — `ReconnectShims()` 用 `context.Background()` 启动路径**: N sessions × 15s/timeout，SIGTERM 无法取消启动阶段重连。方案：接受 appCtx 参数。
  - 涉及：`internal/session/router.go:1109-1111`
- [ ] **R216-GO-5 — cron `Stop()` deadline 后泄漏 triggerWG goroutine（R44 已归档，重申）**: 单 shot 设计内可接受；测试 `-count=N` 下会污染。长期修需重构 triggerWG 与 Stop 协议。
- [ ] **R216-GO-6 — `cmd.Wait()` zombie reaper goroutine 无 Manager 归属**: 若 StopAll 后仍在跑，race 下访问 keyHash 后状态。方案：加 sync.WaitGroup 追踪。
  - 涉及：`internal/shim/manager.go:273-277`
- [~] **R216-GO-7 — cron `sendCtx` 从 `Background()` 派生（归档 2026-05-23）**: 同 R217-GO-6 / R230B-GO-1（`cron.executeOpt` sendCtx 与 spawnCtx budget 不共享）同根因；DeleteJob 后无法 router.Shutdown 取消的修复路径与 budget clamp 同改造点。统一收敛到 R230B-GO-1 跟踪，本批 PR
  - 涉及：`internal/cron/scheduler.go:1531`

### 性能 — 协议接口变更

- [~] **R216-PERF-1 — `ClaudeProtocol.ReadEvent(line string)` 热路径双 copy（归档 2026-05-23）**: 同 R67-PERF-1 / R217-PERF-1 / R225-PERF-1 / R229-PERF-1 / R231-PERF-1 / R232-PERF-1 / R233-PERF-1 / R233B-PERF-1 跨 ~30 轮重申。Breaking 接口签名改造；统一收敛到主条目 R231-PERF-1 跟踪，本批 PR
- [~] **R216-PERF-2 — `shimWriter.Write` 快慢两路径 `string(data[:n-1])` copy（归档 2026-05-23）**: 同 R71-PERF-H1 / R217-PERF-2 / R225-PERF-8 / R233B-PERF-2 跨多轮重申。Breaking shim 协议；统一收敛到主条目 R233B-PERF-2 跟踪，本批 PR
  - 涉及：`internal/cli/process_shim_io.go:54,83`
- [~] **R216-PERF-3 — `eventlog_bridge.go:49` per-EventEntry `json.Marshal`（归档 2026-05-23）**: 同 R217-PERF-3 同根因；R219 已落地 `eventlog_bridge pooled encoder`（见顶部 Round 219 摘要 "eventlog_bridge pooled encoder"），但单条 raw bytes copy（R230B-PERF-6）仍未消除。统一收敛到主条目 R230B-PERF-6 跟踪，本批 PR

### 架构 — 大重构

- [~] **R216-ARCH-1 — `processIface` 24 方法 god 接口（重申 R176-ARCH-M2）**: 本轮 architect 指出已加入 5 个 stream-json specific 方法。拆成 `ProcessCore` / `EventSource` / `PassthroughExt` 三 interface 后 Gemini/Kiro 集成可解耦。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R216-ARCH-2 — `session` 直接 import 4 个 history backend 包 + `claudeDir` 在 RouterConfig（归档 2026-05-23）**: 同 R230B-ARCH-1（`session` 包硬编码 import 4 个 backend-specific history 包）/ R231-ARCH-3（router_core.go 顶部 blank-import history backend）同根因多轮重申。统一收敛到 R231-ARCH-3 跟踪，本批 PR
  - 涉及：`internal/session/router.go:22-25,216`, `router.go:1031-1058`
- [~] **R216-ARCH-3 — KeyResolver 迁移半落地（归档 2026-05-23）**: 同 R229-ARCH-5（KeyResolver/server/dashboard 双路径未收拢）同根因。统一收敛到 R229-ARCH-5 跟踪，本批 PR
- [~] **R216-ARCH-4 — Hub 36 字段多职责混合（归档 2026-05-23）**: 同 R217-ARCH-8（Hub 31 字段 + HubOptions 18 字段）/ R217-CR-4 / R230B-ARCH-2（Hub/Server 半构造对象反模式）/ R231-ARCH-5（Hub 与 Router god-object 双胞胎）同根因多轮重申。统一收敛到 R231-ARCH-5 跟踪，本批 PR
- [~] **R216-ARCH-5 — `attachment.GC` 无生产调用点（重申 R204）**: 真正 breaking 的交付不完整 —— CLAUDE.md §Attachment Refcount 声明要"grow only"已在等待 cron 调用。方案：`cron/scheduler.go` 注册 `"attachment-gc"` 系统任务。需设计触发频率、锁边界、并发模型。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R216-ARCH-6 — processIface 以外的 24 方法上帝接口后遗症（归档 2026-05-23）**: 同 R217-ARCH-6（Router struct 28 字段）/ R226-ARCH-4（session.Router 30+ 字段）/ R229-ARCH-3（Router 单聚合根承载 6 大职责）/ R231-ARCH-4（Router god-object 60+ 方法 / 24+ 字段）/ R233B-ARCH-2（*session.Router 80+ 方法 god struct）跨多轮重申。统一收敛到 R231-ARCH-4 跟踪，本批 PR

### 代码质量 — 小改动等合并窗口

- [ ] **R216-CR-1 — `plannerKeyFor`/`isPlannerKey` 复制到 `session/key.go`（R215-CR-P1-1 重申）**: 消除导入循环的临时方案，长期应抽 `internal/sesskey` 叶子包。
- [~] **R216-CR-2 — `Hub` god 36 字段（同 R216-ARCH-4，归档 2026-05-23）**: 重复登记，统一收敛到 R231-ARCH-5 跟踪，本批 PR
- [ ] **R216-CR-3 — `node.Conn` 18+ 方法巨型接口**: 消费者只用 1-2 个；拆成 `NodeReader`/`NodeProxy`/`NodeSubscriber`/`NodeLifecycle`。
- [ ] **R216-CR-4 — cross-node 错误注入方向不对称**: 反向连接有 `LogSystemEvent`，正向 `node.Conn.Send` 失败只 slog 不进 EventLog。dashboard 用户看到一半。
- [~] **R216-CR-7 — `project.DisplayName` / `Emoji` 有 schema 无 UI wire（归档 2026-05-23）**: 同 R217-CR-7（重复登记）+ R110-P2（已 own UI wire 工作）。统一收敛到 R110-P2 跟踪，本批 PR

## CRITICAL — 安全 (需设计决策)

### Round 194 新发现（2026-05-07）

- [ ] **SM3 — `ManagedSession.Send` sendCancel 先于 loadProcess (Round 174 发现, 2026-05-10 降级 LOW)**: `session/managed.go:336-352` 先 `s.sendCancel.Store(&cancel)` 再 `proc := s.loadProcess()`。并发 `spawnSession` 替换 process 的窄 window 里，`Interrupt()` 调 `(*cancel)()` 可能取消到错 ctx。**现状 accepted**：无数据损坏（只是 Interrupt 语义弱化 —— 旧 ctx cancel 对新 process 无副作用），window 纳秒级，高并发 Interrupt × spawn 的真实触发率未观测到；修需跨 managed.go/router.go sendMu/r.mu lock ordering 重构。留作"stable-process invariant"专项 RFC 材料。
  - 待决策：是否引入 "stable-process invariant" 让 sendCancel 绑定到 process epoch？涉及 sendMu / r.mu lock ordering，改动面跨包
  - 涉及：`internal/session/managed.go Send/Interrupt`, `internal/session/router.go spawnSession`

- [ ] **Q3 — `MessageQueue.Discard` 保留 gen 无 Cleanup 区分 (Round 174 再次确认)**: 与 Q1 同一根因。`Discard` 为让 gen 持续单调不删 `q.queues[key]`，panic/一次性 session 会永远累积。建议新增 `Cleanup(key)` 方法显式告诉 queue "这个 key 永不复用，可删"，由 `router.Reset` 调用。
  - 涉及：`internal/dispatch/msgqueue.go`

- [ ] **S14 — 飞书 VerificationToken-only 模式缺 body-HMAC (Round 174 发现)**: 无 EncryptKey 部署下，nonce dedup 只防 exact replay，攻击者嗅到一条 payload 可**新 nonce** replay 触发业务副作用。启动时已有 warn（feishu.go:315），建议升级为 block startup 或推 operator 明确 ack "I accept no-HMAC mode"。
  - 待决策：是否加 `feishu.allow_unauthenticated_webhook: true` 显式 opt-in？
  - 涉及：`internal/platform/feishu/transport_hook.go:98-159`, `feishu.go:315`

- [ ] **ARCH2 — `Router` god object 拆子聚合 (Round 174 架构师发现)**: 61 方法 / 20+ 字段，承担 spawn + store + history + shim reconcile + discovery。建议抽 `sessionStore`（persistence + knownIDs + storeGen）/ `processPool`（maxProcs/activeCount/pendingSpawns/spawningKeys）/ `shimReconciler`（ReconnectShims 相关）三子聚合，Router 退为 coordinator。
  - 前置：与 ARCH1（server 拆子包）合并规划，按"accept interface, return struct" 风格推进
  - 涉及：`internal/session/router.go` 3000+ 行, `internal/session/managed.go`

- [ ] **ARCH3 — dispatch 对 `project.Manager` 反向依赖 (Round 174 架构师发现)**: `dispatch/dispatch.go:212-243` 内联 planner session key 派生，调 `ProjectForChat / EffectivePlannerModel / EffectivePlannerPrompt`。同样逻辑散落在 server/dashboard.go, server/send.go, server/project_api.go, dispatch/commands.go —— 第二 channel 接入要重复实现。建议抽 `session.Routing`（或 `session.KeyResolver`）一处收敛。
  - 涉及：`internal/dispatch/dispatch.go`, `internal/server/{send.go,project_api.go}`, `internal/dispatch/commands.go`

- [ ] **ARCH4 — Server/Hub 共享 `*sync.RWMutex` 指针阻塞 ARCH1 Phase 3 (Round 174 架构师发现)**: `server.go:50 nodesMu sync.RWMutex`, `dashboard.go:181` 传入 `HubOptions.NodesMu`, `wshub.go:63` 存 `*sync.RWMutex`。跨包拆分 `wshub` 前必须先抽 `nodepool.Registry{mu, conns}` 接口，否则隐式耦合。
  - 前置：优先于 ARCH1 Phase 3
  - 涉及：`internal/server/server.go:50`, `internal/server/dashboard.go:181`, `internal/server/wshub.go:63`

- [ ] **ARCH5 — 配置三源 precedence 无文档 (Round 174 架构师发现)**: config.yaml / ~/.claude/settings.json env 注入 / .naozhi/ sidecar 并行生效，`config.go:30-31` 的 Nodes/Workspaces 二名也仅 inline 注释。18 个文件直接 `os.Getenv` 绕过 config 层。
  - 建议：新增 `docs/design/config-precedence.md` 每字段 precedence 表；抽 `config.Resolved` DTO 收敛 env 读取
  - 涉及：`internal/config/config.go`, `cmd/naozhi/main.go:103-120`, `internal/shim/state.go:141`, 全仓 `os.Getenv` 调用

- [ ] **ARCH6 — stream-json / store version 演进仅 advisory (Round 174 架构师发现)**: `store.go storeFormatVersion=1` 加载时只 log warn 不 migrate；`cli/protocol.go Protocol` interface 无 `SupportedVersions()`，CLI event schema 漂移只能 silently drop。
  - 建议：(A) `Protocol.SupportedVersions() []int` + Init 握手；(B) storeFormatVersion 升级为 migrator registry `map[int]func([]byte) []byte`
  - 涉及：`internal/cli/protocol.go`, `internal/cli/detect.go:24-25`, `internal/session/store.go:60-82`

- [ ] **ARCH1 — `internal/server` 拆子包 Phase 3 (Round 171 发现)**: 58 文件单包（含 test），`server.go` / `wshub.go` / `dashboard_session.go` 都超千行，handler 抽取只完成到结构体层面（Phase 1-2），包级别未拆。`*Server` 作为上帝对象被 hub/handler/debounce/upload 共享，边界靠文件名前缀约定。
  - 待决策：按 subsystem（server/ws、server/http、server/upload）拆子包，还是按生命周期阶段（startup、runtime、shutdown）拆？前者让 `static_ux_contract_test.go` 4900 行 grep 测试可局限在子包内
  - 参考：`docs/design/server-split-design.md` 的 Phase 3 章节待起草

- [ ] **TEST2 — 契约测试边界 RFC (Round 171 发现)**: `static_ux_contract_test.go` 4906 行、418 处正则断言，已形成"测试锁住实现细节"反模式：简单 DOM 重组触发数十 test fail、reviewer 疲劳 → 降低"改测试"门槛 → 测试契约严肃性被稀释
  - 待决策：定义"什么该 source-grep / 什么该 httptest / 什么该 Playwright E2E"，并决定是否允许按 Round 号拆分该文件（`static_ux_contract_test_r110.go` 等）
  - 立即可做：文件顶部加 package godoc 标注"DO NOT add more regex-based source-grep tests"，把新 UX 契约引导到 `test/e2e/`

- [ ] **DOC2 — docs/TODO.md 拆 open + changelog (Round 171 发现)**: 文件 343KB 已触 Read 工具上限，内容高度"开发日志"化 —— 真正的 open item 被每 round 500+ 字的变更说明淹没。部分 UX 条目 Round 146+ 已落地仍挂在 open list
  - 待决策：拆成 `TODO-open.md`（当前未决）+ `CHANGELOG.md`（Round 历史），还是维持合并文件但加"最近 5 轮/全史"折叠？
  - 立即可做：顶部加 `## OPEN ITEMS（真正剩余）` 30 行手写索引

- [ ] **S9 — 注销未撤销 Cookie**: `/api/logout` 路径仅清除浏览器 cookie，被窃取的 token 仍在 24h 内有效；cookie secret 持久化，重启不失效。
  - 方案: 引入 cookie generation counter（存 stateDir），注销时自增，令旧 cookie 立即失效
  - 改动面: cookie MAC 同时由 server cookie 校验、WS cookie 校验、uploadOwner 派生三处消费，加 generation 需同步这三处
  - 涉及: `server/dashboard_auth.go:147-155`, `server/wshub.go`, `server/dashboard_send.go`

- [ ] **S11 — HTTP shutdown 与 router.Shutdown 无同步屏障**: main.go 信号 goroutine 先 `scheduler.Stop()` → `router.Shutdown()`，与 `srv.Shutdown(30s)` drain 期间在途 HTTP handler 并发；在途 `GetOrCreate/Send` 可能观察到半清理 session map。当前未暴露 bug（`router.Shutdown` 返回空 session 时 handler 得到 `ErrMaxProcs` / nil），但生命周期语义不正确。
  - 待决策: 将 `router.Shutdown()` 移入 `server.Start` 的 shutdown goroutine（`srv.Shutdown` 之后），还是在 `Server` 暴露 `ShutdownComplete` channel 给调用方？
  - 涉及: `cmd/naozhi/main.go:546-566`, `internal/server/server.go:413-453`

- [ ] **X1 — shim/discovery 跨平台 build tag（方案 B 主体已落地 2026-05-10，cli 残余待续）**: `internal/shim/{manager,server}.go` 与 `internal/discovery/scanner.go` 加 `//go:build linux` + 3 个 `*_other.go` stub 文件（20+17 导出符号覆盖，共 332 行 stub）。`GOOS=darwin go build ./...` 现过；`GOOS=windows` 仅 `internal/shim/` 与 `internal/discovery/` 过，剩 `internal/cli/process.go:1660` 的 `syscall.Kill+syscall.SIGUSR2` 待后续专项（超出本 lane 范围）。CI matrix 对 darwin 已有价值。
  - 涉及: `internal/cli/process.go` 残余 Linux-only syscall, `cmd/naozhi/service.go:39`(`getent` → `os/user.Lookup`)

- [ ] **OBS1 — 可观测性增强（panic 计数已落地，余项保留）**: panic 计数部分已于 2026-05-10 Round 206 落地 —— 新增 `naozhi_panic_recovered_total` expvar 全局计数器，接入 5 处高信号 recover 站点（wsclient readPump / wshub 两处远端 WS goroutine / dispatch ownerLoop / feishu cleanupNoncesTick），`counter_wiring_contract_test.go` 锁 ≥3 覆盖，`docs/ops/pprof.md` 已同步。**剩余**：goroutine 基线告警、cron 执行延迟 histogram、handler 延迟分布、全链路 trace ID。
  - 待决策: 是否接入 OpenTelemetry / Prometheus / 纯 slog with sampling？按部署目标选轻量方案
  - 涉及: `internal/server/health.go`, `internal/dispatch/dispatch.go Metrics`, slog middleware 层

- [ ] **CLI2 — Process.Send SessionID 只在 init 首次为空时设置**: `--resume` 切换到不同 session ID 后，新 init 的 SessionID 被 `if p.SessionID == ""` guard 丢弃，result 处同样。
  - 待决策: init/result 无条件更新 SessionID 有无其它副作用？加 resume 专用 flag 控制？
  - 涉及: `internal/cli/process.go:550-557`

- [ ] **SM2 — `spawnSession` 与 `ReconnectShims` 并发可能 activeCount 漂移 1 (2026-05-10 降级 LOW)**: 启动期 `ReconnectShims` 在锁内 `activeCount++`，同时 `spawnSession` 也可能在释放/重获锁的窗口里 `activeCount++`。漂移 1 触发虚假 `ErrMaxProcs`，下次 `Cleanup()` 调用 `countActive()` 自愈。**现状 accepted**：自愈窗口已覆盖；真实触发需启动期高并发 spawn + reconnect 同 key，实测未观测。留作 `pendingSpawns` 预留重构专项。
  - 待决策: `spawnSession` 改用 `pendingSpawns` 作为预留，只在最终 `countActive()` 里计算实际 active？还是接受自愈窗口？
  - 涉及: `internal/session/router.go:527, 896`

- [ ] **TEST1 — 测试层 flaky 指标（foundation 已落地 2026-05-10，bulk 迁移待续）**: 仓内 101 处 `time.Sleep` 用于测试同步，135 处 `time.After` 短超时。**已落地**：新增 `internal/testhelper` 包，`Eventually(t, cond, timeout, msg)` + `EventuallyWithInterval` 两个 helper，spy-based 超时测试 + 表驱动 5 sub-test 覆盖；migrate 3 处示范（shim/watchdog_test / cli/process_extra_test / cli/passthrough_test）。**剩余**：~230 处 Sleep/After 分阶段迁移到 Eventually（shim + cli 优先）+ 添加 `t.Parallel()` + 大文件拆分。
  - 涉及: `internal/testhelper/` + `internal/shim/`, `internal/cli/`, `internal/upstream/`, `internal/node/` 测试文件

- [ ] **DEP2 — gogo/protobuf 间接依赖维护模式 (2026-05-10 降级跟踪)**: v1.3.2 是 CVE-2021-3121 修复版本，当前无风险，由 larksuite SDK 间接拉入，naozhi 本身无法直接升级。**现状 accepted**：RNEW-OPS-413 dependabot 已开（每周扫），出现新 CVE 即知；上游迁移 `google.golang.org/protobuf` 不归 naozhi 控制。保留作长期跟踪条目（非 action item）。

- [ ] **CQ1 — `main()` 过长（421 行）**: 无法单测，平台注册/scheduler 初始化/上游连接器全内联。
  - 待决策: 按 `initPlatforms(cfg)` / `initScheduler(cfg, router)` 等关注点拆分私有函数
  - 涉及: `cmd/naozhi/main.go`

- [ ] **CQ2 — `spawnSession` 214 行 TOCTOU 守卫两次释放锁**: 注释量大但认知负载高。
  - 待决策: 将 history 链路构建抽 `buildSessionChain(old, resumeID)` 私有函数
  - 涉及: `internal/session/router.go`

- [ ] **UX3 — 事件列表无虚拟化**: 长 session (1M+ 事件) 会 OOM 浏览器。
  - 待决策: 引入虚拟滚动库？还是滚动到底部自动分页 + IndexedDB 存老事件？
  - 涉及: `internal/server/static/dashboard.js`

- [ ] **PF1 — StartShim cgroup 路径部分成功窗口 (2026-05-10 降级 LOW)**: `connect` 成功后 `moveToShimsCgroup` 若 panic（exec 子进程异常），shim 进程已 alive 但未入 map，孤立运行。cgroup 路径当前无 panic 触发点，结构性脆弱。**现状 accepted**：busctl / sudo 子进程全走 `osutil.SanitizeForLog` + 错误返回而非 panic，实测无触发；OBS1 新增 `naozhi_panic_recovered_total` 已覆盖 panic 观测。留作 defensive-design 条目。
  - 待决策: `moveToShimsCgroup` 提前到 `connect` 前；或用 defer guard，成功插入 map 后 disarm？
  - 涉及: `internal/shim/manager.go:241-255`

- [ ] **RES2 — cron saveSnapshot 同步 I/O 阻塞 mutation 路径 (2026-05-10 降级 LOW)**: AddJob/Delete/Pause/Resume/recordResult 都同步落盘；磁盘慢时阻塞 cron goroutine 并拖住 triggerWG。**现状 accepted**：R58 已把 `WriteFileAtomic` 放到锁外（persistJobsLocked 返回 save func，caller 锁外执行），磁盘慢只阻塞触发者不影响 mutator 锁释放；recordResult 又走 `save := persistJobsLocked()` + 锁外 `save()` 模式。单 save consumer goroutine + dirty flag 属于对 cron 已不是堵点的过度工程，留作 saveIfDirty-parity 专项。
  - 待决策: 接入单 consumer save goroutine + dirty flag 模式，与 session.saveIfDirty 对齐？还是保持同步提升确定性？
  - 涉及: `internal/cron/scheduler.go:998`

- [ ] **CRON1 — `fresh_context` 下 `Reset + GetOrCreate` 非原子**: cron scheduler `execute` 在 fresh 模式先 `router.Reset(key)` 再 `router.GetOrCreate(ctx, key, opts)`，两次锁获取之间其他 cron trigger / dashboard send 若并发到达同 key 会用旧 opts 重建 session，绕过 fresh 语义。当前未暴露 bug（cron + dashboard 极少同 key），但语义薄弱。
  - 待决策: 在 Router 暴露原子 `ResetAndGetOrCreate(ctx, key, opts)`？还是文档化 cron key namespace 隔离？
  - 涉及: `internal/cron/scheduler.go:820`, `internal/session/router.go Reset/GetOrCreate`

### Round 214 — 5-agent 深度 review（2026-05-10）NEEDS-DESIGN 归档

> Round 214 同批次 22 项 FIX-READY 已合入 dev；以下列出需要设计决策或跨模块重构、不能当轮修完的条目。

- [ ] **R214-ARCH-2 — Platform 能力扩展靠 type-assertion helpers 碎片化**: `SupportsInterimMessages` / `AsReactor` / `AsQuestionCardSender` 已 3 个 helper，再加语音/卡片编辑就彻底碎片化；建议收敛为单一 `Capabilities() platform.Caps`，模仿 `cli.Caps` 已有模式。
  - 涉及：`internal/platform/platform.go`, 各 platform adapter

- [ ] **R214-ARCH-3 — Router 硬编码 backend-specific history 源**: `internal/session/router.go` import `claudejsonl` / `naozhilog` / `merged`，session 作为"协议无关调度层"持有 Claude-only 历史源；第二 backend（Gemini/Kiro）上线会让 import list 继续膨胀。
  - 方案：backend 相关 history source 构造移到 `cli.Wrapper`（或新增 `backend.Profile`），Router 只消费 `history.Source` 接口。
  - 涉及：`internal/session/router.go` import 清单

- [ ] **R214-ARCH-4 — nodepool.Registry 抽取（与 ARCH4 合并跟踪）**: Server 与 Hub 共享 `*sync.RWMutex` 指针 + nodes map；consumer-interfaces RFC 迁 Hub 到小接口但 mutex 共享未处理。
  - 方案：抽 `nodepool.Registry` 类型封装注册/快照/生命周期，Server/Hub 持 Registry 接口。
  - 合并到 ARCH4 跟踪。

- [ ] **R214-ARCH-5 — 5 个独立后台 loop 无 supervisor 协调**: `startProjectScanLoop` / `StartCleanupLoop` / `StartShimReconcileLoop` / `scheduler.Start` / `scratchPool.StartSweeper` 各自 own ticker/ctx，顺序靠 main.go 手工书写 + 注释。
  - 方案：引入 `daemon.Group`（已有 errgroup 模式），各子系统 Register 后统一 Start/Stop。
  - 涉及：`cmd/naozhi/main.go:790-794`, 5 个 loop 启动点

- [ ] **R214-ARCH-6 — lifecycle.Manager 编排启动/关闭顺序（与 S11 合并）**: 启动/关闭序列散落在 main + server.Start + hub.Shutdown + scheduler.Stop，依赖靠注释维系。
  - 合并到 S11 跟踪；作为正向结构建议。

- [ ] **R214-ARCH-7 — node.Conn 接口 27+ 方法按消费者拆**: HTTPClient 与 ReverseConn 语义差极大但共用大接口；server 多处 `node.Conn` 调用只用 1-2 方法。按消费者拆 `NodeReader / NodeProxy / NodeSubscriber / NodeLifecycle`。
  - 合并到 ARCH H6 跟踪。

- [ ] **R214-ARCH-8 — processIface 24 方法 god 接口**: session 包定义接口只为给 `*cli.Process` 打包，但 mock 需求与"不新增第二实现"冲突。要么拆 3-4 个小接口，要么退回具体指针 + 真 fake process。
  - 合并到 R176-ARCH-M2 跟踪。

- [ ] **R214-ARCH-9 — cli.Wrapper public mutable fields + ShimManager 生命周期不对等**: Wrapper 作 immutable 元数据容器更合适；ShimManager 应归 Router 管，现在耦合在 Wrapper 上。
  - 涉及：`internal/cli/wrapper.go`, `internal/session/router.go`

- [ ] **R214-ARCH-10 — statefile.Store[T] generics 抽取**: cron/session/shim 三处独立实现 atomic JSON file store，语义相似但无共享抽象。
  - 涉及：`internal/session/store.go`, `internal/cron/store.go`, `internal/shim/state.go`

- [ ] **R214-ARCH-11 — state.Layout 统一路径派生**: server 构造时直接 `os.MkdirAll / os.Stat` 拼 storeDir / claudeDir / attachmentDir / uploadDir 无 owner；未来换 state 实现（tmpfs/S3 backup）要改 6 处。
  - 方案：`state.Layout{ SessionStore, EventLog, Shims, Attachments, Uploads, CronStore }` 统一。
  - 涉及：`internal/server/server.go`, `cmd/naozhi/main.go`

- [ ] **R214-ARCH-12 — backend.Register 替代 knownBackends 包级 var**: 新 backend 靠改源码 + 重编译；Gemini 接入需要 protocol + wrapper 构造 + detect 三处改。
  - 方案：`backend.Register(info BackendInfo, newProto func() Protocol)`。
  - 涉及：`internal/cli/detect.go`

- [ ] **R214-ARCH-13 — feishu.go 单文件 1300+ 行拆子类型**: `Feishu` struct 职责过重（token/reaction/上传/卡片/nonce dedup/bot info）。
  - 方案：按职责拆 `tokenManager` / `reactionCache` / `cardBuilder` 子文件/子类型。
  - 涉及：`internal/platform/feishu/feishu.go`

- [ ] **R214-PERF-1 — PersistSink 单 entry 版本避免 1-slot slice alloc**: `cli/eventlog.go:627` 每个 Append 事件分配 `[]EventEntry{e}` 1-slot slice 传给 `invokePersistSink`；250 alloc/s 基线。
  - 方案：新增单 entry API 或传 `[1]EventEntry` 数组按地址。
  - 涉及：`internal/cli/eventlog.go::PersistSink` 契约 + `internal/session/eventlog_bridge.go`

- [ ] **R214-PERF-2 — Snapshot 不拷贝 persistedHistory**: 1 Hz × N tab dashboard poll 每次 Snapshot 全拷 `persistedHistory` (up to 500 entries ×400B = 200KB)。
  - 方案：Snapshot 改 lazy-load 或只返回摘要标量字段。
  - 涉及：`internal/session/managed.go::Snapshot`

- [ ] **R214-PERF-3 — eventlog persister 高 session 数下 fsync 未 coalesce**: `tickFlush` 遍历 writers 每个独自 Sync；50 session 每 100ms 可触 100 次 fsync。
  - 方案：fdatasync batching 或 high-session-count 下 tick 间隔自适应延长。
  - 涉及：`internal/eventlog/persist/persister.go:584-598`

- [ ] **R214-PERF-4 — eventPushLoop 每 subscriber 独立序列化**: 同 session N 个 dashboard tab，同批 entries 被序列化 N 次。
  - 方案：序列化一次 fan-out raw bytes。
  - 涉及：`internal/server/wshub.go:981,1005`

- [ ] **R214-PERF-5 — AppendBatch 持锁期内拷 500 entry slice**: InjectHistory 500 entry replay 在 l.mu 内做 ~200KB copy。
  - 方案：锁内只写 ring buffer，锁外 sink copy。
  - 涉及：`internal/cli/eventlog.go:641-726`

- [ ] **R214-PERF-6 — task_started 每事件起一 goroutine Resolve**: 5-10 parallel tasks 瞬时喷 5-10 goroutine，goroutine 生命周期几秒。
  - 方案：引入 resolve pool / work queue 限并发。
  - 涉及：`internal/cli/process.go:887-895`

- [ ] **R214-PERF-7 — newEventUUID 每次走 crypto/rand getrandom syscall**: 250 calls/s 全走 kernel。
  - 方案：per-session 单次 seed + counter/AES-CSPRNG。
  - 涉及：`internal/cli/uuid.go:27`

- [ ] **R214-SEC-1 — Weixin iLink 缺入向签名验证**: 长轮询通道只靠 outgoing bearer，任何能发 HTTP 响应的中间人（DNS/MITM）可推消息进 CLI stdin。
  - 方案：评估 iLink 是否有 HMAC；若无，文档标注 + 启动硬拒（除非 `allow_unauthenticated_weixin: true`）。
  - 涉及：`internal/platform/weixin/weixin.go`


  - 方案：明确排除 `ANTHROPIC_API_KEY=` 或按 backend 按需 allowlist。
  - 涉及：`internal/shim/manager.go:987-995`
  - 已修复（与 R219-SEC-3 同根因，9 项显式白名单替换 ANTHROPIC_/CLAUDE_ 通配前缀），本批 PR #93

- [ ] **R214-SEC-4 — ETag 8 字节 sha256 前缀可能被时间信道 probe**: `project_files.go:584` ETag 长度有限，理论上可通过 If-None-Match 猜测文件 size|mtime 重合。
  - 方案：加随机 salt 或使用内容哈希。
  - 涉及：`internal/server/project_files.go:584`

- [ ] **R214-CODE-1 — 错误→用户消息映射逻辑双重维护**: `dispatch/dispatch.go:577-613`（带 emoji / 动态时长）与 `server/errors_usermsg.go`（无 emoji）各自 switch，新 error 两处改。本轮（R214）已在 dispatch 侧补齐 `ErrMessageTooLarge`/`ErrOrphanedSlot`，未来方向是抽共享 `errmsg.UserMessage(err, noOutputTimeout, totalTimeout)`。
  - 涉及：`internal/dispatch/dispatch.go`, `internal/server/errors_usermsg.go`

- [ ] **R214-CODE-3 — readLoop 439 行圈复杂度最高**: `process.go::readLoop` 协议解析 + 状态机 + SubagentLinker + heartbeat + EOF 分类 + panic recover 全耦合。只有端到端测试覆盖。
  - 方案：抽 `handleShimMessage(msg)` + `classifyEOF(msg)` 辅助函数；与 `docs/rfc/process-split.md` 协同。
  - 涉及：`internal/cli/process.go:619-1057`

- [ ] **R214-CODE-4 — server_test.go 残余 8 处 legacy `New()` 位置参数调用**: `new_options_test.go` 专门测试 deprecated 路径，无法一键迁移；新调用站点仍需迁到 `NewWithOptions`。
  - 涉及：`internal/server/server_test.go`, `dashboard_attachment_test.go`, `dashboard_test.go`

- [ ] **R214-CODE-5 — session 生命周期日志级别调整**: `router.go` session spawned/reset/removed/expired 4 条 Info 级每用户消息触发；降 Debug 可减生产日志噪音，但影响运维审计。
  - 待决策：保留 Info 作为 audit trail 还是降 Debug 减噪？
  - 涉及：`internal/session/router.go:2264,2486,2757,2890`

- [ ] **R214-SEC-5 — Feishu VerificationToken-only body-HMAC 启动 fail-fast**: S14 已存在；本轮 security agent 再次确认建议从 warn 升级为 block startup 或显式 opt-in。
  - 合并到 S14 跟踪。

---

## HIGH

### Round 194 新发现（2026-05-07）—— Go/架构/安全高影响

- [ ] **RNEW-003 — cron `executeOpt` 200 行 / 5+ 嵌套 / 中途 ctx 切换**: `scheduler.go:1248-1458` 单函数含 snapshot validate + fresh-context reset + allowedRoot / GetOrCreate / Send / recordResult / deliverNotice 至少 6 项职责，圈复杂度 > 20；第 1427 行从 `s.stopCtx` 切到 `context.Background()`（与 RNEW-ARCH-M5 已处理的 Background sendCtx 相关）再加上 canceled 分支（RNEW-004）审计难度大。
  - 方案: 至少抽 `executeSend` + `executeGetSession`，每个函数守一 ctx 派生点。
  - 涉及: `internal/cron/scheduler.go:1248-1458`

- [ ] **RNEW-008 — `connector.handleRequest` ctx 语义不清导致未来 goroutine 泄漏**: `upstream/connector.go:454,538-541,651,771` 两个 ctx（appCtx / connCtx）混用，不同 RPC 分支对"ctx 断则 goroutine 退出"的承诺不一致。send 走 connCtx 正确，takeover 走 appCtx 是意图 —— 但无注释锁定，未来新 RPC 分支容易误用 appCtx 导致 reconnect 后 orphan。
  - 方案: 在 `handleRequest` 参数 godoc 列出规则矩阵（appCtx = "跨 reconnect 留存"，connCtx = "随 WS 断"），send 分支加 `-race` 验证 connCtx 取消在 drain budget 内。
  - 涉及: `internal/upstream/connector.go:454,538-541,651,771`

  - 方案: RNEW-UX-006 + RNEW-SEC-003 同批做：event delegation + CSP nonce + `connect-src 'self' wss:`（生产强制 TLS）。
  - 涉及: `internal/server/dashboard.go:307`, `dashboard.js` 80+ inline handler 站点

- [ ] **RNEW-ARCH-401 — HTTP/REST API 未版本化 + 无 OpenAPI 契约**: 20+ 端点裸挂 `/api/*`，payload 形状只靠 `*_shape_test.go` + `static_ux_contract_test.go` 418 处 regex 锁；外部 IM/node 字段改名是 silent break。
  - 方案: `docs/rfc/api-versioning.md` RFC + 路由挂 `/api/v1/` 别名共存 + OpenAPI YAML（swaggo/swag 或手写）替换 regex 契约。
  - 涉及: `internal/server/dashboard.go`, `server.go`, `*_shape_test.go`, `static_ux_contract_test.go`

- [ ] **RNEW-ARCH-402 — node 反向协议 ProtocolVersion + Capabilities（foundation 已落地 2026-05-10，server 端 caps-intersection 待续）**: `ReverseMsg` 加 `ProtocolVersion int,omitempty` + `Capabilities []string,omitempty` + 2 条 round-trip 测试锁定"零值 omit"契约。**剩余**：server 侧 caps 交集路由 + `docs/design/reverse-protocol.md` compat 矩阵 + 低版本字段 strip。

- [ ] **RNEW-ARCH-403 — shim↔naozhi state schema_version（foundation 已落地 2026-05-10，rejection 待续）**: `State` 在既有 `Version` 硬 gate 之外新加 `SchemaVersion int,omitempty` 作为推进式 forward-compat 标记（零读作 v1）；2 条测试锁定 round-trip 与 zero-is-v1 契约。**剩余**：`ConnectShim` major 不匹配拒连 + 日志 + `docs/design/shim-design.md` version skew 矩阵。

- [ ] **RNEW-ARCH-404 — 多 backend 能力集聚合 Caps（foundation 已落地 2026-05-10，consumer 迁移待续）**: 新增 `cli.Caps{Replay,Priority,SoftInterrupt,StreamJSON bool}` + `ProtocolCaps(p) Caps` helper；ClaudeProtocol / ACPProtocol 实现 `Capabilities()`（Claude: Replay=true,Priority=true,SoftInterrupt=false,StreamJSON=true；ACP: 反之）；4 条 table test 覆盖 default derivation / impl wins / Claude / ACP。**剩余**：dispatch + server 硬编码的 `protocol.Name()=="acp"` / `SupportsReplay()` 站点迁移到 `ProtocolCaps(p).X`。

- [ ] **R176-PERF-N2 — `shimMsg.Line` 改 `json.RawMessage` 减少每事件 1 alloc**: `readLoop` 的 `json.Unmarshal(trimmed, &msg)` 对 JSON string 值必须 copy 为 Go string（每 event 1 alloc）。5 events/s × 30 session = 150 alloc/s 稳态，可测量但非 P0。
  - 方案: `shimMsg.Line` 从 `string` 改 `json.RawMessage`，下游 `protocol.ReadEvent` 兼容 `[]byte` 入参后延迟 copy 到实际需要的路径。需先评估 ReadEvent 所有实现（ClaudeProtocol / ACPProtocol）的二进制兼容性。
  - 涉及: `internal/cli/process.go:585` (`shimMsg` struct + readLoop 调用点), `internal/cli/cli.go` (Protocol.ReadEvent 签名)

- [ ] **R176-ARCH-M2 — `ManagedSession.processIface` 24 方法 god 接口**: 涵盖 `Send/Kill/Close/State` + `EventEntriesSince/Before/LastN` + `SubscribeEvents` + `InjectHistory` + `TurnAgents` 等，违反 "small interface" 原则。`cli → session` 依赖等价全耦合，MockProcess 需实现 24 方法才能写测试。Gemini ACP 第二 backend 上线前必须拆。
  - 方案: 拆 `ProcessLifecycle` (Send/Kill/Close/State/IsAlive) + `EventSource` (EventEntriesSince/Before/LastN/SubscribeEvents) + `HistoryInjector` (InjectHistory/TurnAgents)。Managed 按需聚合；Snapshot 只依赖 EventSource。
  - 涉及: `internal/session/managed.go:31-64` (processIface 声明), `internal/cli/process.go` 实现点, ~12 个测试 mock

- [ ] **R176-ARCH-M3 — `Hub` 7 个具体依赖 + `SetScheduler/SetUploadStore/SetScratchPool` 后注入 race**: Hub 持 router/queue/projectMgr/scheduler/scratchPool/uploadStore 6-7 个具体指针 + 运行时 setter（`dashboard.go:192`），启动顺序成隐式协议：哪个 Set 在 Start 前/后决定行为正确性。Round 170+ 多次"Set 时机" race 修补都指向这个架构问题。
  - 方案: HubOptions 一次注入完毕（nil 依赖用 null-object 替代 setter）；把 "cron 保存 prompt" 从 Scheduler→Hub 的反向依赖改成 Scheduler 自订阅 hub event（`Hub → Scheduler` 降级为 `Scheduler ← Hub event`）。
  - 涉及: `internal/server/wshub.go:62-114`, `internal/server/dashboard.go:192` (SetScheduler 调用点), `internal/cron/scheduler.go` (增加 event-bus subscription)

- [ ] **R176-ARCH-N4 — `ManagedSession` 没有显式状态机**: `exempt bool` 只在构造期赋值；stub (RegisterCronStub 注册未跑过) / dead-resumable / dead-not-resumable / exempt-paused 四种语义映射到 `process==nil + sessionID 可能存在`，dashboard 要拼装多字段才能渲染当前状态。
  - 方案: 引入 `ManagedState` enum `{Stub, Alive, Suspended, Dead, Exempt}`，store 持久化 state 字段，前端单枚举映射。属 v2 store 迁移，需同步改 `saveStore` schema + 兼容性读取老格式。
  - 涉及: `internal/session/managed.go` (状态字段 + getter), `internal/session/store.go` (serialize + legacy migration), `internal/server/dashboard_session.go` (snapshot 渲染)

- [ ] **R176-ARCH-NX — 跨节点 proxy 错误上浮路径不对称**: `upstream/connector.go:559-572` reverse send 失败注入 `LogSystemEvent`，但 primary 侧 `node.Conn.Send` 的超时/网络错误只走 slog，无等价 EventLog 注入。飞书/Discord 下发失败同样只入日志。DESIGN "错误上浮" 链在 primary→remote 方向可观测，remote→primary 方向静默。
  - 方案: 先补 IM 平台 send 失败注入到对应 session EventLog（与 connector 对称），再抽 `ErrorSink` 抽象让三类 sink（session EventLog / webhook ack / dashboard banner）统一。
  - 涉及: `internal/upstream/connector.go:559-572`, `internal/server/wshub.go` remote send 错误点, IM platform 实现层

- [ ] **H9 — `dashboard.js::_statusTickTimer` 退化为纯 `updateNodeSelector` 驱动 (Round 163 发现)**: `#sidebar-status` DOM 已在侧栏底部让位迭代中删除，`updateStatusBar` 早退；`_statusTickTimer` 的 1s `setInterval(updateStatusBar, 1000)` side-effect 仅剩 `updateNodeSelector()`，而 `setState` 每次 WS 状态变化已同步调用 `updateNodeSelector()`，tick 实质是冗余。
  - 方案: 删除 `_statusTickTimer` + `_updateStatusTick` helper + setState 调用点；连动修改 `static_ux_contract_test.go::TestDashboard_R110P1_WSOutageDurationHint` 的 invariant 4
  - 风险: 当前契约测试锁 tick 机制存在，整体移除需要一次性改契约（约 15 行断言），不是 easy-win；保留便于未来接入 outage 时长提示
  - 涉及: `dashboard.js:1155-1169,6673`, `static_ux_contract_test.go:2918-2993`

- [ ] **H6 — `node.Conn` 接口 18 方法**: 混合 session 获取、pub/sub、代理操作三种职责，无法 mock。
  - 方案: 拆分为 `NodeInfo`、`NodeFetcher`、`NodeSubscriber` 三个小接口
  - 涉及: `node/conn.go`, `node/httpclient.go`, `node/reverseconn.go`, `server/wshub.go`

- [ ] **5+ 包零测试覆盖**: discovery (1415行), node (1679行), upstream (592行), project (430行)。
  - 优先补测: `cli/process.go Send()` → `discovery/scanner.go` → `router.go` 缺失方法 → `dispatch.go ownerLoop`
  - 总覆盖率: ~30%

- [ ] **R172-SEC-H1 `dashboard.js::renderMd()` → innerHTML XSS 审计**（Round 172 发现）: `eventHtml()` 对 `type=text`/`type=user` 事件调 `renderMd(raw)` 并把结果拼入 innerHTML 多处（dashboard.js:2017/6540/frag.innerHTML:1947）。后端 `SetEscapeHTML(false)` 让 LLM 产生的 `<` / `>` / `&` 原样送达前端，`renderMd` 是否全路径都通过 `esc()` / `escAttr()` 防护、是否有 DOMPurify 或等价 sanitizer 兜底，需要做一次端到端审计 + 必要时接入 DOMPurify。单用户 self-host 风险低（本机环境 + 本人 prompt），多用户或共享部署必须阻塞。
  - 方案: (1) 审计 `renderMd` / `inlineMd` / `renderTable` / `renderKatex` 所有 HTML-producing 路径，确认每段 LLM 文本都经 `esc()` 或 function-form `$&` 屏蔽；(2) 若审计发现 gap，接入 DOMPurify（CDN 已允许 cdn.jsdelivr.net）做第二层 sanitize；(3) 加一条回归测试模拟 `<img onerror=prompt()>`/`javascript:` URL 不执行。
  - 涉及: `internal/server/static/dashboard.js` renderMd 起步约 5322 行

  - 方案: (1) 把 inline `onclick="..."` 改为 `data-*` 属性 + `addEventListener` event delegation（click 代理到 `.session-card` / `.event-row` 等父节点）；(2) 去除 CSP 的 `'unsafe-inline'`，换 nonce 或 `strict-dynamic`；(3) 与 H1 同批处理，两者任何一个单独完成仍有残留漏洞。
  - 涉及: `internal/server/dashboard.go:301`, `internal/server/static/dashboard.js` 大量 inline handler 站点

- [ ] **R172-SEC-M4 `connector.go::handleRequest` err.Error() → LogSystemEvent 广播**（Round 172 发现）: `sess.LogSystemEvent("发送失败：" + err.Error())` 的 `err` 是远端/TCP 错误，可能含 bidi override / 终端 escape / C1 控制字符，经 EventLog 广播给所有 dashboard WS client，污染 journalctl + 前端显示。
  - 方案: 抽公共 `SanitizeForLog(s string) string` helper（复用 `isLogInjectionRune`），下沉到 `internal/netutil` 或 `internal/osutil`，cron validator + connector 远端 err 都走同一套过滤。
  - 涉及: `internal/upstream/connector.go:554`（及类似通过 err.Error() 写 EventLog 的所有站点）

- [ ] **R172-SEC-L4 Cookie 24h + 单 token 无轮换**（Round 172 发现）: `dashboard_auth.go:178` MaxAge=86400，Cookie 是 `dashboardToken` 的 HMAC，token 不轮换前 Cookie 永久有效 24h。单用户自托管可接受；多操作员/共享部署需要 per-login nonce 挂到 HMAC input。
  - 方案: (A) 保持现状（自托管场景不是 attack surface 首要入口）；(B) 引入 `auth.sessions.json` 存 per-login nonce，Cookie 多带一个 nonce 段；(C) 短 TTL + refresh token。待多用户场景真实出现再决策。

- [ ] **R172-ARCH-D1 `router.go` 单文件 3000 行**（Round 172 发现）: `internal/session/router.go` 混合 session map / 持久化 / shim reconnect / planner exempt / cron stub / history 加载 6 个关注点。new hire 理解曲线陡、review diff noisy。
  - 方案: mechanical split 成 `router_shim.go`（reconnectShims + spawningKeys + zombie cleanup）+ `router_history.go`（JSONL backfill + shimReconnectGraceDelay）+ `router_planner.go`（exempt countActive / Cleanup 跳过 / RegisterCronStub），不改语义、不改 API。
  - 风险: 低；契约已清晰。

- [ ] **R172-ARCH-D3 Server 包 Phase 3 拆分推进**（Round 172 发现）: `Server struct` 80+ 字段、handler group 10+ 挂在一起，Start 生命周期依赖关系看不清。既有 `server-split-design.md` Phase 1-2 完成，Phase 3 停滞。
  - 方案: 继续把 ScratchPool / ProjectMgr / NodeCache / DiscoveryCache 包成 subsystem 构造器返回——非本轮 scope，需要独立 PR 配 review。

- [ ] **R172-ARCH-D5 `reconnectShims` 并行化（与零中断热重启联动）**（Round 172 发现）: `router.go::reconnectShims` 每 shim 串行、最坏 `N × shimReconnectTimeout=15s`，100 个 shim 下启动可达 25 分钟；`Start()` 同步调用阻塞。启动 grace `shimReconnectGraceDelay=5s` 又是另一个启动期 block。这与"零中断热重启"RFC 直接相关——hot restart 要求新进程 reconnect 快到旧进程 SIGTERM 前。
  - 方案: 用 semaphore（`historyLoadConcurrency=10` 已有同模式）并行处理 shim reconnect，单 shim 仍保持 15s 超时契约。与 zero-downtime restart 方案捆绑设计。

- [ ] **R172-ARCH-D8 regex-on-source 测试边界 RFC**（Round 172 发现）: `static_ux_contract_test.go`（4906 行 418 处正则）+ `resubscribe_lock_order_test.go`（字面量 fragment 匹配）对 gofmt 宽度调整 / 注释插入 / 无语义重命名脆弱。契约测试有价值，但不应把完整 pattern 固化为字面量。
  - 方案: 定义"禁止列表优先 + 小量正向断言"规范：(1) 坏 pattern 的 regex 保留（防退化真需要）；(2) 正向 pattern 要求同时匹配 ≥2 个不同"见证"，避免单一字面量被误伤；(3) 每个 source-level 测试 ≤ 200 行（分块/分文件）。先在 `resubscribe_lock_order_test.go` 试点收敛，成果再推到 `static_ux_contract_test.go` 拆分。

- [ ] **R172-ARCH-D9 `discovery.DefaultScanner` 进程全局单例**（Round 172 发现）: `history_path_cache_test.go:356` 明确标注 "Not t.Parallel: mutates DefaultScanner which is process-wide"。生产 RouterConfig 强依赖 package-level singleton，单元测试被迫 serial。
  - 方案: `RouterConfig.HistoryScanner *discovery.Scanner`（`nil` 默认走 `DefaultScanner()` 保 back-compat），tests 注入独立实例。DefaultScanner 降级为 helper。

- [ ] **R172-ARCH-D10 `jitterBackoff` gauge 观测（第 5 候选，Round 193 归档为 NEEDS-DESIGN）**：前 4 个 counter 已全部落地 —— Round 191 `SpawnPanicRecoveredTotal` + Round 193 `Interrupt{Sent,NoTurn,Unsupported,Error}Total` / `ShimReconnectGraceBackfillTotal` / `WSAuthFail{RateLimited,InvalidToken}Total` 共 7 个新 expvar.Int。**剩余第 5 候选 `connector.jitterBackoff` 当前 backoff 值**属 gauge 语义而非 counter，与既有 `expvar.Int` 模板不匹配。需决策：(a) 是否引入 `expvar.Func` 或 `expvar.String` 承载瞬时值；(b) 多节点部署下 per-connector vs 全局汇总；(c) 瞬时 gauge vs 累积 histogram 桶分布。任一路径都会触发 metrics 包结构性扩展 + 测试策略调整，留待专题 PR。**Round 193 落地细节**：(1) `Router.InterruptSessionViaControl` 里 outcome switch per-case Inc（`router.go:2967-2981`），`InterruptNoSession` 刻意不计以保持分母语义。(2) `ShimReconnectGraceBackfillTotal` 位于 `hasInjectedHistory() == false` 之后（`router.go:779`），happy path 不触发。(3) `WSAuthFail{RateLimited,InvalidToken}Total` 在 `wshub.go:394,421` 两分支与聚合 `WSAuthFailTotal` 并列。配套测试：`metrics_test.go` 三表各加 6 新项 / `counter_wiring_contract_test.go` +8 wiring cases + `TestOBS2_InterruptCountersInOutcomeSwitch`（regex 锁 4 个 Interrupt counter Inc 位于 `switch outcome { ... }` 块内）。

- [ ] **R172-PERF-M3 `pathCacheStorePositive` cap 软上界记一笔**（Round 172 观察）: `evictPathCacheLocked` 行为是"满 cap 后下次 store 前 evict，evict 后再写入"；若 evict 首轮只删 expired negative 且第二轮随机驱逐 16 个 headroom，下次 store 前 map 短暂为 `cap - batch + 1`——cap 是"软上界"，最多超出 `pathCacheEvictBatch=16` 个。已在注释里说明，这里记录一下避免未来 reviewer 误读为 bug。
  - 无需改动；观察性条目。

---

## UX — 用户体验 (2026-04-21 项目级 UX review + 2026-04-29 Playwright 截图审查)

> 已实施: 首次访问 Onboarding Modal / localizeAPIError 中文化 / 语音+转写错误细分 (commit e14ee8c)。
> 剩余项按优先级排列，多数需要跨层设计或较大前端重构。
> 2026-04-29 Round 110 通过 Playwright 9 张截图新增 28 条发现，插入到下方各优先级段（标注 **R110**）。
> 2026-05-07 Round 194 通过 4 agent 并发 review 新增 16 条前端发现（RNEW-UX-xxx）。

### Round 194 发现（前端 UX / JS 可维护性，2026-05-07）

- [ ] **RNEW-UX-003 — 173 处 `fetch(...)` 无 AbortController / 无超时**: NAT 空闲 TCP 被丢时 ajax 挂死数分钟，按钮无响应也无 spinner。
  - 方案: 全局 `fetchJSON(url, {timeoutMs:10000})` wrapper；切页面/会话时 abort 上一批 in-flight。
  - 涉及: `dashboard.js` 全局

- [ ] **RNEW-UX-006 — 80+ inline `onclick="..."` 字符串拼接强依赖 escJs/escAttr**: `dashboard.js:1031,3265,3554,3645,3972,4280,4320,4583,8958` 等。id 里漏网的反斜杠/换行即 XSS；阻碍启用严格 CSP（与 R172-SEC-H2 根因重合）。
  - 方案: 渲染后 event delegation `list.addEventListener('click', e => { const a = e.target.closest('[data-action]'); ... })`，批量替换。
  - 涉及: `dashboard.js` 全局（80+ 站点）

- [~] **RNEW-UX-015 — inline #xxxxxx 硬编码颜色绕过 CSS 变量（大幅迁移完成 2026-05-23，剩余尾巴归档为 ratchet）**: 原描述 32 处，实测起始 36，多轮 micro-batch 迁移到 `--nz-bg-2/--nz-border/--nz-text/--nz-accent/--nz-text-dim` tokens；当前 `TestDashboardJS_RNEW_UX015_HexBaseline` 契约 `ceiling = 14`（实测 14 零 slack）—— 36 → 14 累计迁移 22 处，下行 61%。剩余 14 处多为无 canonical token 的语义色（`#1f2937` 等），后续 PR 视情况补 token；ratchet 测试已锁定不可回升，归档跟踪于 ceiling 数值。本批 PR

### Round 110 发现（Playwright 截图审查，2026-04-29）

#### P1 — 首屏可用性 / 核心任务闭环 (R110)

- [ ] **R110-P1-空闲态 Home 仪表（中部 + 顶部 stats 2/4 + 底部健康 MVP 已落地，其余需后端）** —— 三部分拆解：
  - 🟨 **顶部 stats 卡（2/4 已落地）**：Round 147 追加 `.recent-panel-stats` 2-column grid：**今日活跃会话数**（`computeHomeStats` 按 `last_active >= 本地 0 点` 累加）+ **累计花费**（sum `total_cost`，`formatHomeCost` 双精度 $0.01/$0.0001 分档）。剩余 2 项（**已处理 prompt 数** + **累计 token**）需要后端遍历 event log 或新增 `/api/stats/aggregate`，暂缓。
  - ✅ **中部"最近 5 个会话"缩略卡**：Round 146 已落地。纯前端，0 后端。HTML + mainEmptyHtml() 双站点加 `<div id="recent-sessions-panel" class="recent-panel-wrap">` 占位；新 helper `renderRecentSessionsPanel()` 读 `allSessionsCache`，`selectedKey` 为真 early-return，零会话写空 innerHTML 保持冷启动极简，否则 sort by `last_active` desc 取前 5 渲染 `.recent-row`（`.recent-dot` 复用 `--nz-status-*` token + label + timeAgo）。renderSidebar body 尾部调 `renderRecentSessionsPanel()` 与 sidebar 同步。9 CSS 规则 + 2 契约测试 `TestDashboardJS_R110P1_HomePanelMVP` + `TestDashboardHTML_R110P1_HomePanelStyles`。
  - 🟨 **底部服务健康 MVP（可派生部分已落地）**：Round 148 追加 `.recent-panel-health` strip —— 基于 /api/sessions `stats` 已吐的字段（active/running/ready/total / uptime / cli_name+version / watchdog.{total_kills,no_output_kills}）。新增纯函数 `buildHomeHealthLines(stats)` 3-tier：Line 1 计数+uptime（always）/ Line 2 CLI（有 cli_name 时）/ Line 3 watchdog 介入（kills>0 时，`kind:'warn'` 触发 amber 色）。新增 `lastStatsSnapshot` 模块缓存，fetchSessions 写入。3 CSS 规则（.recent-panel-health / .recent-health-line / .recent-health-line.warn）+ 2 契约测试 `TestDashboardJS_R110P1_HomePanelHealth` + `TestDashboardHTML_R110P1_HomePanelHealthStyles`。**剩余需后端**：claude 子进程数 / shim 连通状态 / cron 队列长度 / 状态文件大小 —— 需后端扩展 /healthz 或新 /api/stats 端点，归到独立 TODO。
  - 方案（历史原文）：顶部 stats 卡片 / 中部"最近 5 个会话"缩略卡 / 底部服务健康
  - 涉及：`internal/server/static/dashboard.html`（冷启动加占位 + 9+4 CSS 规则）/ `internal/server/static/dashboard.js`（mainEmptyHtml 加占位 + renderRecentSessionsPanel helper + computeHomeStats/formatHomeCost 纯函数 + renderSidebar 调用点）/ 后端 `/api/stats/aggregate`（待立项，覆盖 prompt/token/健康）

- [ ] **R110-P1-侧边栏行加回复摘要 (agent chip + 消息计数已落地，响应摘要仍需后端)** —— 三诉求拆解：
  - ✅ **agent chip**：Round 143 已落地。`s.agent` 字段 (`session.ManagedSession.Agent` managed.go:554) 早已通过 `/api/sessions` shipped，前端仅渲染缺失。新增纯函数 `shortAgentLabel(agent)` family 匹配（opus > sonnet > haiku 优先级 substring），空/'general' 短路返 ''，非 Anthropic 保留原串截 10 字符。`sessionCardHtml` metaHtml 在 agentBadge（.sc-agents 机器人计数 chip）前插入 modelBadge（新 .sc-agent 单数 chip，title 承载完整 `s.agent` 消歧义）。CSS `.sc-agent{color:var(--nz-text-dim)...}` 低 chrome 与 `.sc-agents{...}` 语义分离。2 条契约测试 `TestDashboardJS_R110P1_SidebarAgentChip` + `TestDashboardHTML_R110P1_SidebarAgentChipStyle` 锁定。
  - ❌ **assistant 响应 30 字摘要**：需要后端 scan events 提取最后一条 assistant message 入 ManagedSession.LastResponse 新字段（类似现有 LastPrompt 的提取路径），属跨后端侵入，暂缓。
  - ✅ **消息计数**：Round 163 已落地。**设计决策**：无须 event log 遍历 —— 直接在 `EventLog` 加 `userTurnCount atomic.Int64`，Append 里遇 `type=="user"` 自增、AppendBatch 合并为一次 `Add(N)`。`Process.UserTurnCount()` pass-through；`SessionSnapshot.MessageCount int64` omitempty 新字段；`Snapshot()` 在 `proc != nil` 时读 `proc.UserTurnCount()`，proc==nil 返 0。**语义**：cumulative turn count（累计），ring buffer 满溢后老条目被覆盖但 count 继续累加；shim 重连 → InjectHistory → AppendBatch 自动重建计数，对齐"历史值"，无归零假象；sessions.json 不存，与 LastActive 同策。前端 `msgCountBadgeHtml(n)` 纯函数 gate on `> 0` + 999+ overflow clamp，双站点（sidebar `sessionCardHtml` + main-header `renderMainShell`）同步。CSS `.sc-msg-count` 用 `--nz-text-dim` + `--nz-bg-2` + tabular-nums，与 .sc-origin 同级语义。测试：`TestEventLog_UserTurnCount_Append/AppendBatch/SurvivesRingEviction/ConcurrentAppends`（cli 包 4 条 + `-race` 并发压测）+ `TestSnapshot_MessageCount`（session 包 4 table case：proc==nil / 0 / 1 / 142）+ `TestDashboardJS_R110P1_MessageCountBadge` / `TestDashboardHTML_R110P1_MessageCountBadgeStyle`（server 包 2 条锁 helper 契约 + CSS hook）。7 测试 21/21 包 `-race` 全绿。
  - 方案（历史原文）：每行增加最后一条 assistant 响应 30 字摘要（淡色第二行），以及 agent chip（`sonnet-4.6` / `haiku`）和消息计数
  - 涉及：`internal/cli/eventlog.go`（+userTurnCount atomic.Int64 + UserTurnCount()） / `internal/cli/process.go`（+UserTurnCount pass-through） / `internal/session/managed.go`（+SessionSnapshot.MessageCount + processIface.UserTurnCount + Snapshot 填充） / `internal/session/testutil.go` + `router_test.go` / `takeover_test.go`（fakeProcess stub） / `dashboard.js`（msgCountBadgeHtml + 两站点注入） / `dashboard.html`（`.sc-msg-count{...}` CSS）

#### P2 — 信息密度 / 一致性 / 错误处理 (R110)

- [ ] **R110-P2-Cron 卡片重构**：单卡同时挤了标题 / cwd / cron 表达式 / log / 多个按钮，视觉主次不清。
  - 方案：头部 = 状态 pill + 标题 + 项目 chip；中部 = 表达式 + 人话 + next run；右侧按钮 = run now / pause / edit / delete；底部可折叠最近 5 次执行结果绿/红/黄点阵
  - 涉及：`dashboard.html` cron card 模板 / `dashboard.js`

- [ ] **R110-P2-项目自定义显示名 + emoji（foundation 已落地 2026-05-10，UI 待续）**：目录名不可改，但显示名应当可定制（支持 emoji prefix），尤其多项目场景。**已落地**：`ProjectConfig.DisplayName` / `.Emoji` 字段 + `display_name,omitempty` / `emoji,omitempty` yaml tag + `validate.go` rune-count caps (128/8) + C0/C1/bidi/LS-PS 过滤（复用 `osutil.IsLogInjectionRune`）+ 4 条 round-trip/legacy/too-long 测试。**剩余**：dashboard 列表 / 设置面板 UI + `/api/projects` 响应字段 + `/project bind` 命令参数。
  - 涉及：`internal/project/project.go` 状态文件扩字段 / dashboard 设置面板

#### P3 — 增益型功能 (R110)

- [ ] **R110-P3-消息 hover 工具栏**：现在消息右下只有极小 `↗ 追问`（scratch drawer）按钮；hover 消息时整条显示工具栏（复制 / 追问 / 重试 / 分支 / 保存）。
  - 涉及：`dashboard.js` message render + hover handler

---

> 下面是 2026-04-21 老版 UX review 剩余项（未受本轮影响，保留原文）

### P1 — 基础设施层

- [ ] **i18n 基础设施**: 约 110 条 Go 中文字面量 + 879 HTML + 245 JS 字符跨越 Go 后端 + Dashboard + IM 平台。早晚要做。
  - **设计文档**: `docs/design/i18n.md` **APPROVED v4**（2026-04-29 冻结，四轮 review 累计 74 条修复全部归档到 `docs/design/i18n-review-history.md`；v4 后结构性变更走独立 ADR 不再改主文档）
  - 推荐方案: 自写 ~500 行 Printer/Bundle/Resolver/Heuristic (YAML + embed.FS + x/text/language.Matcher) + 后端预渲染 `window.__i18n__` 给前端（`__t` 唯一标识 + 边界 regex）
  - Locale 来源: **Dashboard 链**：cookie > `?lang=` > `Accept-Language`（q-value）> config default；**IM 链**：三档置信度模型（`user` > `platform` > `heuristic`），高置信覆盖低置信；`/lang` 命令一期化作为启发式错判的自愈通道
  - 飞书 webhook 不带 locale，用"CJK 比例启发式 + /lang"兜底；Slack cache key 固定为 `team_id:user_id` 防跨 workspace 污染；Discord 有原生字段
  - 迁移路线: PR1 基础设施 → PR2 平台 UserLocale + session.Locale 弱固化 → PR3a `/lang` + `/help` 试点 → PR3b apierr → PR3c dispatch 剩余 → PR4 cron/cli → PR5a HTML 模板化 + Settings → PR5b JS 字面量 1000 行 → PR6 测试升级 + CJK 基线清零
  - CI 基线: `docs/i18n-cjk-baseline.txt` 只拦截增量，避免主干阻塞
  - 风险: YAML 漏 key (CI 脚本 diff) / user locale API 失败 (30min LRU + fallback) / 测试脆性 (`Contains` 替代全等)
  - 涉及: `internal/i18n/`（新包）, `internal/dispatch/commands.go`, `internal/dispatch/apierr.go`, `internal/platform/*`, `internal/server/static/dashboard.*`

- [ ] **错误消息后端结构化**: 目前 API/handler 错误以 `text/plain` 返回（如 `"upload rate limit exceeded"`），前端拼接时露出技术术语。
  - 方案: handler 统一返回 `{error: {code, message_zh, message_en, context?}}`；前端按 locale 选择
  - 涉及: `internal/server/dashboard_*.go`, `internal/server/static/dashboard.js`

### P1 — Dashboard UX

- [ ] **移动端会话卡片"X"按钮不可见**: hover 在触控设备无效，导致无法删除会话。
  - 方案: 长按 (≥500ms) 弹 Context Menu（删除/编辑/复制 key）；需与现有 swipe-to-delete 交互协调
  - 涉及: `internal/server/static/dashboard.js` session card render + `initSwipeDelete`

### P2 — 性能感知

- [ ] **长事件流无虚拟滚动**: 500+ 事件时 DOM 全量渲染卡顿、滚动不畅。
  - 方案: Intersection Observer 虚拟列表，可视区 + 上下各 20 条缓冲；保持 60fps
  - 涉及: `internal/server/static/dashboard.js` events render
  - 风险: 与 Markdown/Mermaid/代码块高度计算的交互

- [ ] **Planner 进程资源监控**: 长期运行 Planner 内存持续增长，无可视化 / 无手动重启。
  - 方案: Dashboard 侧边栏 Planner 卡片显示 RSS / CPU%，右键"重启 Planner"
  - 涉及: `internal/server/server.go`（暴露 `/api/planner/stats`）, dashboard.js

### P3 — 小改进

- [ ] **主题切换（浅色 / 系统跟随）**: 现仅 GitHub Dark 硬编码，部分用户需要浅色。
  - 方案: CSS variables 已用 `--nz-*`，新增 `.theme-light` 类覆盖；localStorage 记忆；Settings 菜单选择
  - 涉及: `internal/server/static/dashboard.html` CSS, dashboard.js

---

## MEDIUM

### Round 194 新发现（2026-05-07）—— 性能 / 运维 / 文档 / 测试

#### 性能

- [ ] **RNEW-PERF-003 — `renderMd` LRU cache key 用全文字符串 → 流式更新全不命中**: `dashboard.js:5678-5693, 6194-6280` 流式 LLM 输出每 event `detail` 不同，缓存从不命中；500 行回复每次流更新做全量重渲染，O(n×k)。
  - 方案: 流式 event 改增量渲染（只重渲染最后 N 行）；`running` 状态用 `textContent` 纯文本，`result` 事件后再 MD 渲染。
  - 涉及: `internal/server/static/dashboard.js:5678-5693, 6194-6280`

- [ ] **RNEW-PERF-004 — `EventLog.notifySubscribers` 在 RLock 内做 channel send（已部分缓解）**: `internal/cli/eventlog.go:728-740` `subMu.RLock` 下遍历 subscribers 做非阻塞 channel send；50 WS × 10 sub 场景下每 Append 触发 50 次 send + atomic dropped 计数。**现有缓解**：R65-PERF-M-1 已把 `subMu` 从 `sync.Mutex` 升级为 `sync.RWMutex`（多个 notify 不互斥）+ `subCount atomic.Int32` fast-path 在零订阅时完全跳过锁（`eventlog.go:729`）。剩余窗口仅在 subscriber > 0 且多 Append 并发时触发，"snapshot then unlock" 能再省锁内 send 的串行化。降级为 MEDIUM/defense-in-depth。
  - 方案: 先 RLock 下快照 subscriber slice → 释放锁 → 锁外做 channel send。
  - 涉及: `internal/cli/eventlog.go:728-740`

#### 运维

- [ ] **RNEW-OPS-415 — 状态目录磁盘告警 + 日志轮转（启动告警已落地 2026-05-10，quota + journald 待续）**: `~/.naozhi/{sessions.json, cron.json, shims/*.json, attachments/, run/, env}` 持续增长；shim state 无大小上限；journald 日志轮转靠 distro 默认而非 unit 锁定。**已落地**：`osutil.StateDirSize` walker + `stateDirWarnMB=500` 阈值 + 50k 文件扫描预算（避免巨型目录拖慢启动）+ `ErrStateDirScanTruncated` 哨兵 + 首次运行 ENOENT 静默 + `docs/ops/disk-budget.md`（44 行，列 7 路径 + 清理指引 + 跨引 RNEW-OPS-415）+ 4 条 osutil 测试。**剩余**：config.yaml `session.shim.state_dir_quota` 字段 + 硬 quota 执行 + `deploy/naozhi.service LogRateLimitIntervalSec/Burst` 绑定 journald 轮转预算。
  - 涉及: `internal/shim/manager.go`, `internal/attachment/store.go`, `deploy/naozhi.service`, `docs/ops/`

#### 架构（从 HIGH 溢出的 1 条）

---

### 代码质量

- [ ] **命名一致性: Get*/Fetch*/Load\***: `GetSession`/`GetWorkspace`/`GetState` 等应去 `Get` 前缀（Go 惯用）；`FetchEvents` 明确远程，`LoadHistory` 明确文件 I/O，已合理分工。
  - 方案: 批量去 `Get` 前缀（23 处 session.Router + 10 处 cli.Process）；保持 `Fetch*`/`Load*`
  - 改动面: 大但机械，可一次性重命名 + 更新调用点

- ~~**M1 — `cron.Scheduler` 存 `context.Context` 到 struct 字段**~~: 2026-04-20 确认为合理例外（robfig/cron 回调无 ctx 参数，需 Scheduler 持有 lifecycle ctx），代码添加注释说明，不做机械拆分。

### 架构重构 (暂缓)

> 经多次独立 review 验证，当前均无实际 bug 或开发阻碍。仅在出现相关问题时再推进。

- [ ] **P0 — Router God Object** (1761 行, 24 字段, 7+ 职责): 拆分为 `SessionStore` + `ShimReconciler` + `HistoryLoader`
- [ ] **P0 — Server 包职责过广** (22 文件, 10 内部 import): handler group 已提取，可进一步以 interface 解耦成子包
- [ ] **P1 — Dispatcher 依赖具体类型**: `Router`/`Scheduler`/`ProjectMgr` 均为具体指针，应定义消费者接口
- [ ] **P1 — session → discovery 紧耦合**: 直接调用 `discovery.LoadHistory()`，应注入 `HistoryLoader` 接口
- [ ] send-with-broadcast 流程 3 处重复 (dispatch/WS/HTTP) — 可提取 SessionSender 服务
- [ ] server 包含业务逻辑 (sessionSend/tryAutoTakeover/startProjectScanLoop) — 可下沉

### 性能优化

- [ ] `[]byte(line)` 每事件字符串拷贝 — unsafe 零拷贝或 shim 协议改造
  - 涉及: `cli/protocol_claude.go:59`
  - 备注: 需 unsafe 或协议改造，风险高于收益，暂缓

---

## LOW

- [ ] parseCronAdd 要求双引号包裹 schedule — 有意设计
- [ ] Reverse node 注册无重放保护 — TLS 下无风险
- [ ] Cookie pre-auth 绕过 `wsAuthLimiter` — 有 500 连接上限兜底
- [ ] Watchdog timer AfterFunc Reset 竞态 — fires as no-op (需 generation 机制)

---

## 新功能 (未开始)

### 访问控制
```yaml
access:
  dm_policy: "allowlist"        # open | allowlist | disabled
  group_policy: "open"          # open | allowlist | disabled
  allowed_users: ["ou_xxx"]
  allowed_chats: ["oc_xxx"]
```

### Gemini CLI 集成
ACP 协议验证通过，protocol_gemini.go 设计完成，待实现。

### 竞品能力提炼后续实现要点（2026-05-22 调研）
完整设计要点见 [`docs/design/competitor-distilled-2026-05.md`](design/competitor-distilled-2026-05.md)。
覆盖 Anthropic Cowork / AWS Quick / OpenAI Codex / OpenClaw / Hermes / OpenHuman / Manus / Genspark / MCP 生态调研，按优先级提炼 9 块能力：

- **P0** 安全基线（Hermes 八条 P0 自查 + Smart approval XML fence + Redaction 默认 ON）
- **P0** Cron `no_agent` watchdog + LLM job + delivery target 三段式
- **P1** ACP server（让 IDE 接入 naozhi）
- **P1** 多渠道 BaseAdapter 抽象 + WeCom/DingTalk
- **P1** Connector / MCP vault Phase 1（OAuth + Trust 分级 + per-channel 启用）
- **P1-P2** Self-Evolving Skills（Curator-lite → LLM 复审 fork）
- **P2** Multi-agent Kanban（SQLite WAL + worker 池化）
- **P2** OTel + 治理面（对位 Cowork 治理黑盒）
- **P3** ACP client / Memory Tree / Wide Research 扇出

后续按文档 §10 拆分为独立 RFC（`docs/rfc/security-baseline.md` / `cron-v2-no-agent.md` / `acp-server.md` / `multi-channel-adapter.md` / `connector-vault.md` / `skill-curator.md` / `kanban.md` / `otel-audit.md` 等）。

---

## Round 215 — 5-agent 深度 review 第 29 轮（2026-05-11）NEEDS-DESIGN 归档

> Round 215 同批次 20 项 FIX-READY 已合入 dev（f19e477 / c468ef2 / 8fb12fe），
> 部署完成 uptime ok，smoke 无 ERROR。以下为需要设计决策或跨模块重构、
> 不能当轮修完的条目（按 agent 分组）。

### go-reviewer（避开已归档后剩余 P1/P2）

- [~] **R215-GO-P2-5 — `router.ReconnectShims` reconcile 期 `sess.ReattachProcessNoCallback` 无 sendMu 保护（继承 R51-CONCUR-002）**: 运行期 reconcile 与 ManagedSession.Send 并发时，storeProcess 原子替换旧 process 指针 + clearDeathReason 与 Send() 的 timeout 写入有逻辑 race。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
  - 方案：加 sendMu 快照契约或显式序列化。涉及 sendMu/r.mu lock ordering。
  - 合并到 R51-CONCUR-002 跟踪。

### security-reviewer（避开已归档后剩余 P1/P2）

- [ ] **R215-SEC-P1-1 — `--dangerously-skip-permissions` 硬编码**: `protocol_claude.go:43` 每个 Claude CLI spawn 都带此 flag，授权用户可让 CLI 执行任意 shell 指令 / 读写任意文件。
  - 待决策：设配置开关（per-agent 或全局），默认保留当前行为（单用户可信部署），多租户场景 opt-out。
  - 涉及: `internal/cli/protocol_claude.go:43`

- [ ] **R215-SEC-P1-2 — Planner prompt 从 CLAUDE.md 读取后不再走 ValidateConfig**: `EffectivePlannerPrompt` 从 disk 读的 PlannerPrompt 直接塞进 `--append-system-prompt`；Claude 的 Write tool 能改 CLAUDE.md，理论上 prompt injection → 下一轮 planner spawn 用带控制字符的恶意 system prompt。
  - 方案：spawn 前对从 disk 读出的 PlannerPrompt 再跑一次 validator。
  - 涉及: `internal/project/manager.go EffectivePlannerPrompt`, `internal/server/project_api.go:352`, `internal/session/routing.go:119`

- [ ] **R215-SEC-P2-3 — 非 Linux 平台 attachment 路径校验 `path.Clean` vs `filepath.Clean` 不一致**: Linux 生产无影响；macOS/Windows 部署存在 case-insensitive / 分隔符绕过风险。
  - 方案：非 Linux 平台补 `filepath.Clean(filepath.FromSlash(relRaw))`；或文档化"Linux-only deployment"。
  - 涉及: `internal/server/dashboard_send.go:919`

- [~] **R215-SEC-P3-1 — Shim `auth_token` 明文写 state 文件**: 同 UID 进程读取 state 后可连 shim socket 注入 stdin。 — 归档 2026-05-23：威胁模型设计上"OS 账户即信任边界"，shim socket 已通过 SO_PEERCRED (peeruid_linux.go) 强制 same-UID，能读 state 0600 文件的进程同样能直连 socket，加密 token 不抬升攻击门槛；本批 state.go 加 godoc 锚点说明 accepted-risk 决策
  - 方案：state file 强制 0600；或启动时重新生成 token，state 不存。
  - 涉及: `internal/shim/server.go:145-159`

  - 方案：cron API 绑 `validateWorkspace(workdir, allowedRoot)`。
  - 涉及: `internal/server/dashboard_cron.go`
  - 已修复（dashboard_cron.go:382 handleCreate 与 :773 handleUpdate 均已 `validateWorkspace(req.WorkDir, h.allowedRoot)` + 403 forbidden on boundary violation；本批仅核对归档），本批 PR #75

### performance-optimizer（避开已归档后剩余 P1/P2）

  - 方案：EventEntry 提供 `MarshalJSONFast` 或改用 `bytes.Buffer + encoder` 复用。
  - 涉及: `internal/session/eventlog_bridge.go:49`
  - 已修复（commit 7fc779f 引入 sync.Pool 复用 json.Encoder + bytes.Buffer，本批仅在源码里加 R215-PERF-P1-1 锚点注释让 reviewer 反查），本批 PR #86

- [~] **R215-PERF-P2-1 — `eventlog.Append` 单条路径 `[]EventEntry{e}` 每次 alloc**: AppendBatch 的 sinkCopy 已预分配，Append 单条仍每次分配 1 slice。 — 归档 2026-05-23 (superseded by R230-PERF-1)：sink-nil 早返回已覆盖生产热路径（test harness / headless tools / replay phase），sink-attached 路径的 alloc 受 PersistSink 保留契约约束结构性必需；与 R219-PERF-4 / R222-PERF-8 / R228-PERF-7 同根因；本批 eventlog.go 加 godoc 锚点
  - 方案：引入 1-slot pool 或为 Append 写专用 sink 路径。
  - 涉及: `internal/cli/eventlog.go:627`

  - 方案：单订阅 fast-path 直接传 pooled buffer，两订阅起再 clone。
  - 涉及: `internal/server/wshub.go:996,1020`

- [~] **R215-PERF-P2-4 — `eventlog.storeAtomicString` 每次非等存储 `new(string)` heap alloc**: tool summaries 高频变化时持续 GC 压力。 — 归档 2026-05-23：textutil.StoreAtomicString 已有 fast-path 等值短路（覆盖稳态重复 prompt summary），实际变更时 `new(string)` heap alloc 是 atomic.Pointer[string] 结构性必需（Pointer.Store 需要 addressable 槽）；零 alloc 替代方案（atomic.Value / uintptr+intern）成本/收益不对等，且热路径是 turn 边界（低频）非每行 stdout；本批 eventlog.go 加 godoc 锚点
  - 方案：用 sync.Pool 或 generation counter 结构避 pointer alloc。
  - 涉及: `internal/cli/eventlog.go:1027`

- [~] **R215-PERF-P2-5 — `dashboard.handleList` 每次 poll 全 Snapshot（归档 2026-05-23）**: 同 R219-PERF-2 主条目跟踪：响应含动态 uptime/now，纯版本缓存命中无法跳过；router.Version() 已先读 + handleList 内已最优化。50 session × 10 atomic.Load × 1Hz = 500 atomic ops/s，远不构成瓶颈。本批 PR 归档。
  - 方案：server 端缓存 `[]SessionSnapshot`，基于 storeGen 重建。
  - 涉及: `internal/server/dashboard_session.go:307-324`

### code-reviewer（避开已归档后剩余 P1/P2）

- [ ] **R215-CR-P2-3 — dispatch/server 的 `resolver == nil` legacy fallback 双轨**: KeyResolver 创建后仍保 legacy inline，漂移已经实际出现（/urgent 一度丢 planner model/prompt）。
  - 方案：`NewKeyResolver(nil,nil)` 合法，让 Resolver 非 nil 强制；或加 CI 规则禁止 legacy 分支新增。
  - 涉及: `internal/dispatch/dispatch.go:266-295`, `internal/server/dashboard.go:475-512`

### architect（避开已归档后剩余 P1/P2）

- [ ] **R215-ARCH-P1-1 — Router 跨 ManagedSession 内部字段直读**: `Router.reconnectShims / collectPreviousHistory / RenameSession / DiscoveryExcludeIDs / RegisterCronStubWithChain` 直接访问 `sess.prevSessionIDs / persistedHistory / historyMu`。拆 Router 的前置必须先加 `SnapshotPrevIDs / ReplacePrevIDs / SnapshotPersistedHistory` accessor。
  - 涉及: `internal/session/router.go:1241-1242,1978-1988,2127-2130,2624-2625,2628,2660-2665,3654-3655,3794-3795`

- [ ] **R215-ARCH-P1-2 — `spawnSession` 跨 3 段临界区手工 pendingSpawns`++/--`**: `panicSafeSpawn` 只保护其中一段，其他 3 段若未来 refactor 引入 panic 会永久 ErrMaxProcs。
  - 方案：RAII slot token 封装 inc/dec，defer Release。
  - 涉及: `internal/session/router.go:2083-2105, 2117-2155`

- [ ] **R215-ARCH-P1-3 — `processIface` 24 方法混合 Claude-only passthrough 扩展**: `InterruptViaControl / SendPassthrough / DiscardPassthroughPending / PassthroughDepth / SupportsPassthrough` 是 stream-json 协议独有；session 包感知这些方法即协议细节上漏。
  - 方案：拆 `ProcessCore` + `PassthroughExt`（optional）+ `EventSource`，或用 `Caps.Passthrough` gate。
  - 涉及: `internal/session/managed.go:34-92`

- [ ] **R215-ARCH-P1-4 — `h.hub.router` 跨 handler 盗用具体路由器**: ScratchHandler/SendHandler 绕开 HubRouter 接口直接用具体 `*Router`。consumer.go godoc 明确标注 "Phase 2.5 cleanup" 悬空债务。
  - 方案：为这两 handler 各自定义 ScratchRouter / SendRouter consumer interface。
  - 涉及: `internal/server/dashboard_scratch.go:97,294,301,311,316`, `internal/server/dashboard_send.go:321,329,941,949`

- [ ] **R215-ARCH-P1-5 — `session` 包 import `history/claudejsonl+merged+naozhilog`**: attachHistorySource 硬编码 `switch backend == "claude"`，`RouterConfig.ClaudeDir` 是 Claude 专用配置漏进通用 session 包。
  - 方案：`cli.Wrapper.HistorySource(s)` 方法由各 backend 实现；RouterConfig.ClaudeDir 迁到 ClaudeProtocol。
  - 涉及: `internal/session/router.go:22-25, attachHistorySource:1031-1058`

- [ ] **R215-ARCH-P2-1 — Server 启动后 `SetScheduler/SetUploadStore/SetScratchPool` 三处无锁 set**: 裸指针写无 atomic.Pointer；`s.hub != nil` guard 扩散 8 处，本质是对象半构造状态漏出。
  - 方案：短期升 atomic.Pointer；长期 HubOptions 一次性注入 + null-object。
  - 涉及: `internal/server/wshub.go:239-248`, `internal/server/dashboard.go:238,249,263`

- [ ] **R215-ARCH-P2-2 — `historySource` 附着分 6 处手动调**: attachHistorySource 漏调 = EventEntriesBeforeCtx 返回空。
  - 方案：ManagedSession 构造强制注入 history.Source，或由工厂方法统一。
  - 涉及: `internal/session/router.go:825,1054-1057,2255,2679,3737,3798`

- [ ] **R215-ARCH-P2-3 — `Hub.ctx` 被 ScratchHandler/uploadStore.StartCleanup 借用当 app ctx**: 语义错位，Hub 关 ≠ app 关；未来 Hub 热重启会一起死。
  - 方案：Server 分发 `appCtx`，Hub.ctx 只 Hub 内用。
  - 涉及: `internal/server/dashboard.go:248`, `internal/server/server.go:537`

- [ ] **R215-ARCH-P2-4 — 4 个包各自定义 consumer `SessionRouter` 接口重复方法**: dispatch/cron/server/upstream 各 declare GetOrCreate/GetSession/Reset；方法签名漂移要改 4 次。
  - 方案：`session.CoreReader` + `session.CoreMutator` 中心接口，consumer 包 embed 扩展。
  - 涉及: `internal/dispatch/consumer.go:34-43`, `internal/cron/scheduler.go:67-85`, `internal/server/consumer.go:37-52`

- [ ] **R215-ARCH-P2-5 — `cron.executeOpt` 200 行内 3 个 ctx 无法 reason**: stopCtx / sendCtx(Background) / timeout 语义分歧。
  - 方案：拆 `executeFreshSpawn(stopCtx,j)` + `executeSendToSession(sess,text,timeout)`，参数单语义。
  - 涉及: `internal/cron/scheduler.go:1351-1458`

- [~] **R215-ARCH-P2-6 — Router.Shutdown 内 historyWg.Wait 5s 超时后 goroutine 实际泄漏（误报关闭 2026-05-23）**: 复核 router_cleanup.go:595-614 godoc 已完整说明 leak 是 intentional，bounded by Shutdown 单 shot 契约（process 退出回收）；零中断热重启 RFC 仍是 open design problem，需要不同的 cleanup 机制（不是这条的范畴）。归档关闭，本批 PR
  - 方案：history load goroutine 自感知 historyCtx 并 return；或 RFC 明确单 shot 语义。
  - 涉及: `internal/session/router.go:3207-3228`

- [ ] **R215-ARCH-P2-7 — `ManagedSession.Snapshot` 顺序 load 10+ atomic.Pointer**: 1Hz × N tab × N session 热路径。
  - 方案：`atomic.Pointer[snapshotBox]` 一次 load 拿全部不变字段。
  - 涉及: `internal/session/managed.go:850-910`

## Round 227 — 5-agent 并行 review 第 39 轮（2026-05-20）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 90 条发现。
> 12 条 FIX-READY 已落地本轮 PR（subagent_link panic-safe defer Lock /
> errors.Is(bufio.ErrBufferFull) / dispatch takeoverFn nil noop /
> cron snapshotJob LOCK godoc / discord 5 附件上限 + https 强制 /
> weixin contextToken 512B 长度上限 / transcript LimitReader 16MB /
> session extractLastPromptFromProcess + InjectHistory 与 EventLog 6 类
> activity 对齐 / parseKeyParts 改 IndexByte / normalizeBackendID
> ToLower+TrimSpace / meteringUsage 16 单位上限 / task_started TrimSpace
> 合并）。下面是需设计决策、breaking、跨包重构、或方案不唯一不适合本轮直接修
> 的条目。

### Go 正确性 — 本轮新发现

- [~] **R227-GO-2 — `cli/subagent_link.Resolve` retry 循环 `time.Sleep(retryInterval)` 无 ctx 取消（P1）**: SIGTERM 期间最多 8 个 Resolve goroutine 各自卡 3s。修复需 Resolve 接 context.Context 参数（Breaking）。涉及 `internal/cli/subagent_link.go:294,332`。重申 R225-GO-2。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R227-GO-5 — cron `notifyTarget` 用 `context.Background()` 不响应 stopCtx（误报关闭 2026-05-20）**: 复核后确认与 send 路径同款意图——cron run 记录写入与最终通知归属同一生命周期，必须独立于 stopCtx 才能在 graceful shutdown 期间把已跑完的 turn 结果递达 IM。条目自标"降为 P3 仅记录"，本批 PR #187 关闭归档。
- [~] **R227-GO-7 — `cli.Resolve` resolveSem acquire 无 ctx select arm（P2）**: 与 R227-GO-2 同根因；Resolve 接 ctx 后可统一 select。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R227-GO-8 — `cli/process.go Kill()` 持 shimWMu 调 closeShimConn 与 Detach 顺序不一致（误报关闭 2026-05-20）**: 与 R225-GO-7 同根因一并关闭——核实后 Kill (process.go:519-535) 与 Detach (:617-634) 都是「持 shimWMu 期间 closeShimConn」同一模式，不存在所谓"顺序不一致"。closeShimConn 走 sync.Once + net.Conn.Close，最坏延迟一次系统调用，不会饥饿 heartbeat。本批 PR #169

### 安全 — 本轮新发现

- [~] **R227-SEC-1 — `protocol_claude.BuildArgs` + `protocol_acp.BuildArgs` ExtraArgs 无 flag 允许列表（P1）**: dashboard 认证用户可注入 `--mcp-config /attacker/server.json` 类危险 flag。修复需引入 flag 允许列表（Breaking：依赖任意 extra args 的运维方需迁移）。重申 R219-SEC-1，本轮强调 `protocol_acp.go:133` 同样有此缺口。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R227-SEC-2 — `serveRender` 在 Lstat 后 os.Open 制造 inode-swap TOCTOU（P1）**: handleFileGet 已有 Lstat-after-resolve 防御，但 serveRender 再次 os.Open(resolved)。方案：handleFileGet 阶段就 OpenFile + 传 fd 给 serveRender。涉及 `internal/server/project_files.go:670`。重申 R219-SEC-2。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R227-SEC-3 — `allowed_root` 缺失时仅 Warn，cron work_dir 无根目录约束（P1）**: dashboard token 已设但 allowed_root 空时，认证用户可设 cron work_dir=/etc。方案：dashboard token != "" + 监听非 loopback 时 Fatal。涉及 `internal/server/server.go:513`。重申 R226-SEC-6。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R227-SEC-4 — `moveToShimsCgroup` 接受 shim 自报 Hello.CLIPID（P2）**: 被劫持 shim 可上报任意 PID。方案：通过 /proc/<CLIPID>/status 验证 PPid。涉及 `internal/shim/manager_linux.go:60-90`。重申 R219-SEC-5。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R227-SEC-7 — 反向 node WS 在 ws:// 明文下传 token（P2）**: 内网嗅探可截获 token 冒充节点。方案：r.TLS == nil 拒绝 Upgrade，或加 insecure_node 显式豁免。重申 R226-SEC-3。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R227-SEC-9 — Dashboard 主页 CSP `script-src 'self' 'unsafe-inline'` 完全打开 XSS 通道（P2）**: 任何同源 XSS 注入即可执行任意脚本。方案：迁移到 CSP nonce 模式。Breaking。涉及 `internal/server/dashboard.go:389`。重申 R226-SEC-2。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R227-SEC-10 — `/health` 端点无认证无限速，泄漏基础设施拓扑（P3）**: 外部攻击者可枚举 session count / watchdog kills / node 状态 / build 版本。方案：per-IP rate limiter + 敏感字段移到认证 /api/stats。重申 R226-SEC-7。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R227-SEC-14 — `feishu/transport_ws.go` parseSDKEvent 没有 maxIncomingTextBytes 检查（误报关闭 2026-05-20）**: 复核 transport_ws.go:309 已有 `if len(text) > maxIncomingTextBytes` 守卫 + slog.Warn，与 transport_hook.go 保持对称。条目自标"本条为误报"。本批 PR #187 关闭归档。

### 性能 — 本轮新发现

- [ ] **R227-PERF-2 — `ACPProtocol.ReadEvent` 对 agent_message_chunk 路径双 unmarshal（P1）**: 整行 unmarshal 为 RPCMessage 后每个分支再 unmarshal Params/Result。流式文本场景两次全量 JSON 解析。方案：method 短路检查后 lazy-unmarshal。
- [ ] **R227-PERF-3 — `eventlog_bridge.newEventLogSink` per-entry make+copy（P1）**: 5-50 events/s × N session 的小对象 GC 压力。方案：合并为 batch make+copy。涉及 `internal/session/eventlog_bridge.go:98`。
- [~] **R227-PERF-4 — `wsClient.SendJSON(v)` 调 json.Marshal 每次分配 encodeState（误报关闭 2026-05-20）**: 复核后确认 encoding/json 内部 sync.Pool 已 pool encodeState (`encode.go:312-322` newEncodeState/freeEncodeState)，naozhi 加一层 sync.Pool 仅多一次 allocator round-trip 反而更贵。R229-PERF-4 已通过预 marshal 静态帧（wsErrNotAuthMsg/wsErrRateLimitedMsg 等）覆盖了真热的小响应路径。本批 PR #187 关闭归档。
- [ ] **R227-PERF-5 — `WriteUserMessageLocked` json.Marshal encodeState alloc（P2）**: 用户 prompt 发送频率不高但每次 alloc。方案：sync.Pool 复用 bytes.Buffer + Encoder。
- [ ] **R227-PERF-6 — `Cleanup` + `saveIfDirty` 每次写锁内 map clone 3 份（P2）**: 50 session × 30s 间隔每分钟 2 次 O(n) clone。方案：传 []*ManagedSession 切片，配合 listRefsPool。
- [ ] **R227-PERF-8 — `BroadcastSessionsUpdate` time.AfterFunc 每次 alloc（P2）**: 高频 notify 下每次 timer + WG 开销。方案：Hub 持久 *Timer + Reset。
- [~] **R227-PERF-9 — `EventLog.Append` invokePersistSink 单条 slice 逃逸（P2）**: 5-50/s × N session 热路径。方案：EventLog 内置 [1]EventEntry scratch + sync.Pool。涉及 `internal/cli/eventlog.go:660`。 — 评估关闭（已归档 2026-05-23 复核）：同根因 R215-PERF-P2-1 / R217-PERF-4 / R219-PERF-4 / R222-PERF-8 / R226-PERF-5 / R228-PERF-7 / R229-PERF-5 / R230C-PERF-2，已统一收敛到 R230C-PERF-2。`internal/cli/eventlog.go:803-811` godoc 显式锚定 PersistSink 契约——sink 可保留 slice 跨 return，所以 [1]EventEntry stack scratch 无可避免地通过 atomic.Pointer-loaded fn ptr escape；sync.Pool 只是把 alloc 换成 Get/Put 开销（48B payload 收益不显）。生产热路径已被 R230-PERF-1 sink-nil 早返回覆盖。本批 PR。
- [~] **R227-PERF-11 — `EventLog.Append` storeAtomicString 每次 *string alloc（评估关闭 2026-05-20）**: 条目自标"降级仅观察"。复核 textutil.StoreAtomicString 已用 atomic.Pointer.Load + 字符串相等短路（同值不重新 Store *string），命中率 ~99%（lastActivitySummary 在同 tool_use 持续时不变）。sync.Pool[string] 在 textutil 是叶子包做不到 lock-free 复用，且分支预测器对短路命中早已优化。本批 PR #168 关闭归档。
- [~] **R227-PERF-12 — `ACPProtocol.parseSessionUpdate` tool_call/tool_call_update 分支双 alloc（P2）**: AssistantMessage ptr + ContentBlock slice。方案：tool name 直接存 ToolCall.Title；ContentBlock 改 [1]ContentBlock + count。 — 评估关闭（已归档 2026-05-23 复核）：Message.Content 被多处读（`internal/cli/askquestion_test.go:51` `internal/dispatch/status_test.go` 等通过 `Content[0].Name` 提取 tool_use name），删除 Message 字段需跨包重写消费者；[1]ContentBlock 改 fixed array 失去 slice header 灵活性，与 [`*AssistantMessage` 含 nil 表示空] 契约冲突。tool_call 分支频率为每 turn 几十次（远低于 stdout 热路径），单次 alloc 成本被 ACP RPC 序列化掩盖。godoc 已通过 `event.go:78-79` 锚定 ToolUseID 优于 Message.Content[].Name 的提取方式。本批 PR。
- [~] **R227-PERF-16 — `EventEntriesSince` dead-session 分支全量扫描+stable sort（误报关闭 2026-05-20）**: 条目自标"500 entry stable sort < 1µs，可接受"——dead-session 重订阅是低频路径（tab reload），微秒级开销远低于其他热路径。InjectHistory 端排序也会让 replay 与 live append 之间的语义需要重新定义。本批 PR #187 关闭归档。
- [ ] **R227-PERF-17 — `shim.ServerMsg.MarshalLine` 每次 json.Marshal alloc（P3）**: shim binary 独立。方案：sync.Pool[bufEnc]。**降级**：shim 独立 binary，不影响主进程，单独 PR 处理。
- [ ] **R227-PERF-18 — `eventPushLoop` EventEntriesSince per-goroutine 独立 slice（P3）**: 50 订阅 tab × 同 session 各自分配。方案：扩展 EntriesSinceInto(dst) 接口接受 caller-owned buffer。Breaking。
- [~] **R227-PERF-19 — `Cleanup` Pass 1 candidates 用 time.Time 而非 lastActiveNS int64（误报关闭 2026-05-20）**: 条目自标"传染性大，ROI 低"——Cleanup 是 30s tick 的低频路径，time.Time 接口更直观且与 LastActive() 公开 API 对齐；改 int64 会让 candidate slice 类型与 ManagedSession.LastActive() 返回签名分裂，下游若拿到 candidate slice 需各自 time.Unix 还原，传染面广。本批 PR #187 关闭归档。

### 代码质量 — 本轮新发现

- [~] **R227-CR-5 — `dispatch.sendAndReply` 250+ 行 5+ 职责（P2）**: 错误处理、生命周期通知、事件跟踪、结果解析、图片读取、AskQuestion 抑制、文字分割。方案：拆 buildSendContext / handleGetOrCreateError / handleSendError / deliverResult。涉及 `internal/dispatch/dispatch.go:527`。重申 R219-CR-7。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR

### 架构 — 本轮新发现

- [ ] **R227-ARCH-1 — `session.Router` god-package：27 字段、80 方法跨 10 文件（P1）**: router-split refactor 只切了文件没切类型。方案：进一步拆 (*coreState, *lifecycleManager, *shimReconciler, *cleanupSweeper, *backendRegistry) 子结构 + facade，或承认现实合回 router.go。
- [ ] **R227-ARCH-2 — `cli.Process` god-struct：50+ 字段同一 RWMutex（P1）**: shimIO/turnState/procMeta/passthrough/heartbeat/watchdog/linker/快照同住一锁命名空间。方案：分 shimIO + turnState + procMeta 三子组件，Process 缩为组合体。
- [ ] **R227-ARCH-3 — `history.Source` 与 `cli.HistorySource` 双接口结构同形（P1）**: cli 不能 import history 又要 history factory 注册，新建 internal/wireup（或 historywire）包统一管 history factory 注册解套。
- [ ] **R227-ARCH-4 — `cli.backend.Profile` 与 `cli.detect.knownBackends + normalizeBackendID` 双源（P1）**: backend 元信息双轨；新加 backend 三处同步。方案：Profile 加 Aliases/IsDefault；knownBackends 从 backend.All() 派生。
- [~] **R227-ARCH-5 — 4 个 consumer-side SessionRouter 接口农场（P2）**: dispatch/cron/upstream/server 各自声明，server 内多数 handler 仍裸 *session.Router。方案：抽 session.RouterCore 基础接口 + RouterReader 子集。重申 R215-ARCH-P2-4。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R227-ARCH-6 — shim 协议三套版本号（ProtocolVersion/stateVersion/SchemaVersion）无升级矩阵（P2）**: zero-downtime restart 后变定时炸弹。方案：写 docs/rfc/shim-versioning.md 合并到唯一 ProtocolEpoch + 兼容矩阵 contract test。
- [ ] **R227-ARCH-7 — sessions.json sidecar / EventLog per-record / shim state inline / cron 各一套 schema 版本机制（P2）**: 新 store 作者每次重新决定。方案：抽 internal/storefmt 包定义 VersionedFile + future-version 处理枚举。
- [ ] **R227-ARCH-8 — 4 个 platform adapter 各自 httpClient/hookSem/dedup/SanitizeForLog（P2）**: 同类 SSRF/cap 修复在 4 处反复打。方案：抽 platform/transport 公共组件（SafeHTTPClient/InboundDispatcher/OutboundRetryWithBackoff）。
- [ ] **R227-ARCH-9 — `cli` 包反向依赖 history 概念，HistorySessionView 唯一实现是 *session.ManagedSession（P2）**: 抽象塌陷反例。方案：与 R227-ARCH-3 合并修。
- [ ] **R227-ARCH-10 — Protocol 能力查询走 SupportsX 与 Capabilities() Caps 双轨（P2）**: 新能力两路径下游可漂移。方案：收敛到 Capabilities() 单方法，删 SupportsX。Breaking。
- [ ] **R227-ARCH-11 — cli 包 import metrics 14 处隐式全局（P2）**: 单测要 mock metrics。方案：MetricsSink interface + DI，默认 noop。
- [ ] **R227-ARCH-12 — `processIface` 30+ 方法逼近抽象塌陷（P3）**: testutil fake 200+ 行。方案：拆 processSender/processInspector/processLifecycle 三角色。
- [ ] **R227-ARCH-13 — naozhilog/claudejsonl/kirojsonl 三 history reader 同算法独立维护（P3）**: ctxCheckEvery / limit shrink / ENOENT 各自实现。方案：抽 history/internal/scan ReverseScan 原语。
- [ ] **R227-ARCH-14 — dispatch 与 platform 之间 type assertion 探测能力（P3）**: 加新平台能力 N×M 矩阵分支。方案：与 R227-ARCH-10 同——platform 加 Capabilities() Caps。
- [ ] **R227-ARCH-15 — `eventlog_bridge` 在 cli↔persist 中介中做 EventEntry→Entry marshal（P3）**: 序列化责任在中间人违反"数据生产方负责"。方案：cli.EventEntry.MarshalForPersist() 方法 + bridge 简化。
- [ ] **R227-ARCH-16 — `claudejsonl/kirojsonl` 通过 init 注册 + session blank import 隐式生命周期（P3）**: factory 没注册导致 NoopHistorySource 的 bug 要查 4 文件。方案：与 R227-ARCH-3 合并；显式调 wireup.RegisterX()。
- [ ] **R227-ARCH-17 — 30+ interface 半数单实现（P3）**: HistorySessionView/AgentIntrospector/deadlineInterrupter 等单实现接口未替代。方案：写 docs/CONTRIBUTING-interfaces.md 三条规则。
- [ ] **R227-ARCH-18 — contract test 边界三类含义混用（P3）**: type assertion / 行为契约 / 内存模型断言用同一 *_contract_test.go 名。方案：拆 *_iface_assert / *_behaviour / *_invariant 命名。
- [ ] **R227-ARCH-20 — `dispatch.passthroughCtxKey/urgentCtxKey` 用 ctx.Value 跨包传 boolean（P3）**: Go 反模式（ctx.Value 应仅用于请求级元数据）。方案：定义 dispatch.SendOptions struct 或 functional options。Breaking。

### 配置 / 测试 — 本轮新发现

- [ ] **R227-TEST-2 — `cli.detectVersion` 用 context.Background()（P3）**: SIGTERM 期间 --version probe 等满 5s。方案：NewWrapper 接 ctx 参数（Breaking，3-5 个调用点）。

## Round 228 — 5-agent 并行 review 第 40 轮（2026-05-20 第二批）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 90 条发现。
> 14 处 FIX-READY 已落地本轮 PR（详情见顶部摘要）。下面是需设计决策、breaking、跨包重构、或方案不唯一不适合本轮直接修的条目。

### Go 正确性 — 本轮新发现

- [ ] **R228-GO-3 — `reconnectShims` replay goroutine 无 ctx 绑定（P2）**: 重放段对每个 `task_started` 启动裸 goroutine 调 `linker.Resolve`，SIGTERM 时延迟 shutdown。方案：与 R227-GO-2 / R225-GO-2 合并，Resolve 接 ctx。涉及 `internal/session/router_shim.go:361-394`。

### 安全 — 本轮新发现

- [~] **R228-SEC-1 — Dashboard 主页 CSP `script-src 'unsafe-inline'`（P2 R227-SEC-9 重申）**: 主页用 `'unsafe-inline'` 而 login 页已用 hash-based CSP 收敛。方案：为响应生成 nonce + 内联 `<script>` 注入 nonce；或迁移内联脚本到外部文件。Breaking：需改 HTML 模板。涉及 `internal/server/dashboard.go:390`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R228-SEC-2 — `serveRender` `os.Open(resolved)` 二次打开 inode-swap TOCTOU（P1 R227-SEC-2 重申）**: `handleFileGet` Lstat 后 `serveRender` 再次 Open，存在窗口可被符号链接替换。方案：在 Lstat 之后立即 Open 拿 fd 传入下游函数；或用 `f.Stat()` 比对 inode。涉及 `internal/server/project_files.go:590,684,762,856`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R228-SEC-4 — `/health` 端点无 rate limiting（P3 R227-SEC-10 重申）**: 未认证响应含 version 字段可 fingerprint。方案：per-IP rate limiter 60/min，或把 version 移到认证区段。涉及 `internal/server/server.go:809`、`internal/server/health.go:169`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R228-SEC-5 — WS Hub 同一 session key 订阅数量无 per-key 上限（P3 R227-SEC-11 重申）**: 单 token 开 100 个 WS 各订同 key 触发 100 路 fan-out。方案：维护 `keySubCount map[string]int` + 阈值（如 20）。涉及 `internal/server/wshub.go`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R228-SEC-6 — `serveRender`/`serveRaw` sandbox CSP `style-src 'unsafe-inline'`（P3 R226-SEC-9 重申）**: CSS-based exfiltration 攻击面。方案：nonce 化或去掉 unsafe-inline。涉及 `internal/server/project_files.go:731,~905`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR

### 性能 — 本轮新发现

- [ ] **R228-PERF-3 — `subagent_transcript.readLocked` 每次 `os.Open` 不复用 fd（P2）**: 每 200ms × 50 active tailer = 250 open/close fd/s。方案：缓存 `*os.File`，Tail 用 Seek 复用，inode 变化时重 Open。涉及 `internal/cli/subagent_transcript.go:63-88`。
- [~] **R228-PERF-7 — `EventLog.Append` `[]EventEntry{e}` 字面量 heap escape（P3 R219-PERF-4 具体修法方向）**: 单条 slice 字面量逃逸。方案：先 `-gcflags=-m` 验证再决定栈数组+切片或 sync.Pool。涉及 `internal/cli/eventlog.go:703`。 — NEEDS-DESIGN 归档 2026-05-23（与 R219-PERF-4 / R222-PERF-8 / R215-PERF-P2-1 同根因；R230-PERF-1 sink-nil 早返回已覆盖生产热路径，sink-attached 路径的 slice 字面量受 PersistSink 保留契约约束结构性必需；本批 eventlog.go 加 godoc 锚点）

### 代码质量 — 本轮新发现

### 架构 — 本轮新发现

- [ ] **R228-ARCH-1 — `session` 包既通过 `cli.Wrapper.ShimManager` 又直接调 `shim.SocketPath/KeyHash/WaitSocketGone` 双重接入（P1）**: 抽象塌陷。方案：把三个 shim 调用收进 `cli.Wrapper.WaitSessionShimGone(key)`。涉及 `internal/session/router_lifecycle.go:1115-1116` + `router_shim.go:27,57-72,146-180`。
- [ ] **R228-ARCH-2 — `cli.Wrapper.ShimManager` 公开字段穿透到 session（P1）**: 应代理 `Discover/Reconnect` 等方法。Breaking（公开字段消失）。涉及 `internal/cli/wrapper.go:38` + `internal/session/router_backend.go:151`。
- [ ] **R228-ARCH-3 — `server/wshub` 直接持 `*cli.SubagentLinker` 指针长寿命缓存（P1 与 RFC v4 phase 3+ TODO 同根）**: Linker 重建后旧 map key 残留为 GC root。方案：session 层暴露 `WireLinkerOnce(key, ...)` API，把指针弱引用封进 session 包。涉及 `internal/server/wshub.go:165` + `internal/server/dashboard_agent_events.go:66,72,80`。
- [ ] **R228-ARCH-4 — `cli.AskQuestion`/Item/Opt 与 `platform.QuestionCard`/Item/Option 双套结构体（P2）**: dispatch 手工字段拷贝，加字段易漏。方案：抽到共享包（如新建 `internal/askq` 或 `internal/eventlog/schema`）。涉及 `internal/cli/event.go:141-166` + `internal/platform/platform.go:108-141`。
- [ ] **R228-ARCH-7 — `processIface` 32-method 胖接口 + 内部强转回 `*cli.Process`（P2）**: 抽象漏了。方案：要么删 interface 直接用 `*cli.Process`；要么拆成 3 个小接口。需设计决策。涉及 `internal/session/managed.go:33-102` + `router_lifecycle.go:829`。
- [ ] **R228-ARCH-8 — 4 个 platform adapter 各自 `var fooHTTPClient` SSRF-defense client（P2）**: 4 份近一致的 redirect+TLS 1.2 floor client。方案：`internal/platform.NewSafeHTTPClient(timeout)` helper。涉及 feishu/discord/weixin/slack 各自顶部 var。
- [ ] **R228-ARCH-12 — `cron.SchedulerConfig` 直接持 `session.AgentOpts` + `platform.Platform`（P2）**: cron 字段调整波及 cron。方案：cron 加自己的 JobNotifier interface + JobAgentOpts 局部类型。涉及 `internal/cron/scheduler.go:100-101,213-214`。
- [ ] **R228-ARCH-13 — `cli.HistoryFactoryFn` registry blank import 在 session 包（P2）**: 触发点已迁到 `cli.NewWrapper` 但 import 列表残留在 session。方案：移到 cli/wrapper.go 或 cmd/naozhi/main.go。涉及 `internal/session/router_core.go:21-32`。
- [ ] **R228-ARCH-18 — `dispatch.Dispatcher.projectMgr` 仅用于 slash-command UX 但持整个 `*project.Manager`（P3）**: 30+ 方法面。方案：内部 1-method interface 注入。涉及 `internal/dispatch/dispatch.go:56`。

## Round 230C — PR #198 详细归档 NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共 82 条发现，PR #198 同批的另一组细化条目（编号集 C 区分 line 211 的核心 R230 节与 line 277 的 R230B 节）。
> 以下条目为非 breaking 但需要更大改动 / 需设计决策 / 跨模块的发现，登记追踪。

### Security（剩余）

- [ ] **R230C-SEC-7 — dashboard_token 8-15 char 公网监听只 Warn（P2）**: `cmd/naozhi/main.go:938` 公网部署时 8 字节 token 仍允许启动只 slog.Warn。方案：监听非 loopback 时最小长度提升到 16 + slog.Error+os.Exit(1)，或加 entropy 估算拒字典词。Breaking：是（部分公网部署需调长 token）。
- [~] **R230C-SEC-10 — cron notify_chat_id 发送时未再次校验（多轮 NEEDS-DESIGN 归档 2026-05-23）**: validateNotifyTarget 当前在 internal/server/dashboard_cron.go，cron 包重新调用会引入 cron→server 反向 import 违反 layer。彻底方案需把 validateNotifyTarget 提到 internal/platform 或 internal/cron 共享 helper（涉及 utf8 / size cap / platform allowlist 多约束）。当前防御深度：写路径（CRUD）已校验；读路径仅在 jobs file 被外部篡改时绕过——allowed_root + 0o600 文件权限是首道防线。归档 NEEDS-DESIGN，本批 PR
### Go 正确性 / 并发（剩余）

- [~] **R230C-GO-4 — spawnSession inline history load 同步 IIFE 包了 historyWg（误报关闭 2026-05-23）**: 实地复核 loadResumeHistoryOnSpawn godoc 已明确意图："Synchronous — runs on the spawnSession caller goroutine. The historyWg Add/Done dance still tracks the call so Shutdown.Wait can drain in-flight loads"。historyWg 是独立 WaitGroup（非 sessionsWg），让 Shutdown 等历史加载完成。归档关闭，本批 PR。
- [ ] **R230C-GO-7 — executeOpt sendCtx 用 context.Background 让 5h 任务无法 shutdown 期取消（P2）**: `internal/cron/scheduler.go:1955` 5h execTimeout 的 cron 任务 stopCtx fire 后仍跑满。方案：deadline watchdog 扩展为 stopCtx 触发也调 InterruptViaControl，或 sendCtx 改用 `context.WithTimeout(s.stopCtx, jobTimeout+grace)`。Breaking：否。
- [~] **R230C-GO-13 — runstore cacheHeadPush O(N) copy（归档 2026-05-23）**: R233-PERF-2 / R233B-PERF-3 同根因 — keepCount=200 prepend 每次 200-element memmove。统一收敛到 R233-PERF-2 主条目跟踪（ring buffer / container/ring 改造）。当前 grow-in-place + copy + 索引赋值已经省了 backing array 拷贝（runstore.go:295-297）。归档关闭，本批 PR
- [~] **R230C-GO-18 — registerStub 用 chainIDs 完全替换 prevSessionIDs（误报关闭 2026-05-23）**: 实地复核 router_discovery.go registerStub — `len(chainIDs) > 0 && !slices.Equal(...)` 才替换；nil/空 chain 不会触发改写（cron_stub_test.go 已 pin 契约）。生产 cron 始终传完整链。建议方案会破坏 cron 把链替换为权威值的语义。本批 PR 归档。

### Performance（剩余）

- [~] **R230C-PERF-4 — handleSubscribe per-key 限额线性扫描全部连接（误报关闭 2026-05-23）**: 实地复核：内层 `for other := range h.clients` 在 `count >= maxSubscribersPerKey` 时已 break early（wshub.go:615-617），单次 subscribe 最坏 O(maxSubscribersPerKey)（20）而非 O(connections=500）。"O(1) subscriberCounts map" 优化要在每条 disconnect / closeClient 路径维护第二份计数表，bookkeeping 成本超过收益。godoc 已补充原地说明。本批 PR
- [~] **R230C-PERF-6 — completeSubscribe 调两次 Snapshot()（误报关闭 2026-05-23）**: 实地复核：两个 `sess.Snapshot()` 调用分别在 `!sess.HasProcess()` early-return 分支（wshub.go:674）和正常分支（wshub.go:741），互斥执行不会同请求两次。TODO 描述的"复用同一 snap"前提不成立。本批 PR
- [~] **R230C-PERF-7 — handleList 每次重建 projectList slice（归档 2026-05-23）**: 缓存 + 失效 hooks 要覆盖 project Add/Remove/SetFavorite/git-detect/node-cache merge 多条改写路径，invariant 成本超过它救下的分配。realistic 规模 ≤50 projects × ≤20 tabs ≈ 50 rebuilds/s × ≤4 KB ≈ 几百 KB/s GC churn，远低于 dashboard 自身 JSON encode 的分配。godoc 已就地说明。本批 PR
### Code 质量（剩余）

- [~] **R230C-CR-2 — computeJobTimeout schedule 参数明确忽略（误报关闭 2026-05-23）**: 复核 cron/job.go:276 现状：`func computeJobTimeout(maxCap time.Duration) time.Duration { return maxCap }`，schedule 参数已删。R232-CR-5（已落地）已机械重构删除 schedule 参数 + 调用点。条目描述过时，归档关闭，本批 PR
- [~] **R230C-CR-4 — TriggerNow entryID==0/!=0 两个 goroutine 体几乎相同（同根因 R233B-CR-3 已落地 2026-05-23）**: R233B-CR-3 已抽 executeIfNotDeletedOrPaused(jobID) helper：RLock 拿最新 *Job + paused 判断，缺失/暂停均 Debug log skip，否则 executeOpt(cur, true)。两个 goroutine 都收敛到一行调用，14 行 → 3 行。归档关闭，本批 PR
- [~] **R230C-CR-8 — registerStub vs registerStubByValue 双轨（归档 2026-05-23）**: 复核现状：registerStubByValue 是核心实现（~10 行），registerStubFromJob 已是 1-line wrapper（`s.registerStubByValue(j.ID, j.WorkDir, j.Prompt, j.LastSessionID)`），仅在持锁路径不能传 *Job 的场景由 registerStubByValue 直接调用。R232-CR-12 godoc 已写明 "三 helper 合成单个值参数版本"。两路并存是有意为之（值参数避免持锁路径误传 *Job 指针）。归档关闭，本批 PR。
- [~] **R230C-CR-11 — recordResultP0 RunCounters.addRun 不在 persist 失败时回滚（归档 2026-05-23 误报）**: 复核 scheduler.go:2535-2553 现状：prev.Counters = j.RunCounters 已在 mutate 前快照（line 2535），perr 失败分支已 `j.RunCounters = prev.Counters` 回滚（line 2553）。Counter 回滚已实现，描述误报，归档关闭，本批 PR。
### Architecture（剩余）

- [ ] **R230C-ARCH-1 — DESIGN.md 第 5 节 HTTP Server 描述与现实严重失真（P1）**: `docs/design/DESIGN.md:492-516` 说 server 只注册 /health；实际 92 个文件 / 60+ 路由。方案：DESIGN.md 加"实际 vs 理想"对比 + ADR 链接 server-split RFC。Breaking：否。
- [~] **R230C-ARCH-2 — dispatch/cron/upstream/sysession 各自定义 SessionRouter 接口（P1）**: 4 包同名 `SessionRouter` 各持不同方法集。方案：约定 `XxxSessionRouter` 命名空间 + `internal/session/contracts/` 集中接口约束。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230C-ARCH-3 — cron Scheduler 直持 platforms map 越层调 Reply（P1）**: cron 已悄悄成为第二个 dispatch（持 Agents map / 复刻 retry+SplitText）。方案：cron 内禁用 platform.Platform，强制走 dispatch facade。Breaking：是（与 R219-ARCH-8 合并）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230C-ARCH-4 — processIface god interface 拆分优先级（P1）**: `internal/session/managed.go:35-102` 30+ 方法含 dashboard-only 字段。方案：先剥 dashboard 字段到 ProcessIntrospector，process 核心剩 8-10 方法。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230C-ARCH-5 — 保留 key 命名空间策略表分散在 5 处（P1）**: cron:/project:/scratch:/sys: 在 reservedKeyPrefixes/exemptKeyPrefixes/saveStore/Cleanup/Sidebar 各列。方案：`KeyKind enum + Policy struct` 单一事实源 + vet 阻断。Breaking：否。
- [ ] **R230C-ARCH-9 — Platform caps 抽象不到位（P2）**: `cli.ProtocolCaps` 已聚合，但 Platform 仍混用 `MaxReplyLength` 值 / `SupportsInterimMessages` bool / `AsReactor` 接口断言三种返回风格。方案：`PlatformCaps` 结构体聚合（与 R229-ARCH-6 合并）。Breaking：是。
- [ ] **R230C-ARCH-10 — Hub 三 setter vs SysessionMgr 字段注入风格不一（P2）**: `wshub.go:282-291` SetScheduler/SetUploadStore/SetScratchPool 是 setter；ServerOptions.SysessionManager 是字段。方案：四个一律走 HubOptions required 字段。Breaking：否（与 R219-ARCH-5 合并）。
- [ ] **R230C-ARCH-11 — Session mode 4 态文档 vs 实际 7+ 态无单一权威类型（P2）**: `cli.ProcessState` 4 态 + ManagedSession.exempt + key 前缀派生 stub + process==nil 派生 paused。方案：`session.SessionMode enum {Active, Stub, Paused, Scratch, Exempt}` 正交叠加 + 类型化 transitions。Breaking：是。
- [ ] **R230C-ARCH-13 — scratchPool 与 router.sessions 双池强迫 Hub 分流（P2）**: `internal/session/scratch.go` + `dashboard_scratch.go` 每加一个 scratch 操作两路径都得复刻。方案：合到 sessions map + Tag=Scratch 或文档化双池约束。Breaking：是。
- [ ] **R230C-ARCH-14 — 文件化状态多实例并发写无 flock（P2）**: 6 个独立 atomic write store 假设单实例独占。方案：state file 加 `flock(LOCK_EX) + writer_pid + writer_host + generation`。Breaking：是。
- [ ] **R230C-ARCH-15 — main.go ~390 行 backend-specific settings.json 重写（P2）**: kiro backend 不需要。方案：抽 `BackendProfile.PrepareEnv()`，main 只调 profile。Breaking：否（与 R229-ARCH-7 合并）。