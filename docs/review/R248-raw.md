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
- [ ] **R248-GO-4 — Hub.Shutdown wiredLinkers=nil 在 clientWG.Wait 之前（P3）** [REFACTOR]: `internal/server/wshub.go:485-493` 顺序为 wiredLinkers=nil → clientWG.Wait。一个 in-flight readPump 调 handleSubscribe → completeSubscribe → maybeWireLinkerTailer 可能在 wiredLinkers 已被清空后到达；现有 nil-guard `if h.wiredLinkers == nil { return }` 处理之，但语义微妙（"shutdown 后不再接受新 wiring" 而非 "wiring 仍可用直到 wait 完成"）。方案 A：把 wiredLinkers=nil 移到 clientWG.Wait 之后，与 sendWG 屏障对齐。方案 B：注释强化 "clientWG 内的 maybeWireLinkerTailer 调用必须容忍 wiredLinkers==nil；Shutdown 提前 nil 是为加速 GC"。当前能跑但 review 不直观。 → #371
- [ ] **R248-GO-5 — wiredLinkers map[interface]struct{} dedup 跨 dynamic-type 假阳性（P3）** [REFACTOR]: `internal/server/wshub.go:193` 接口键 dedup 走 (dynamic type, pointer value)。当前 *cli.SubagentLinker 是唯一实现，1 process : 1 linker pointer，dedup 等价 pointer-keyed。未来若两后端的 AgentLinker 实现碰巧持相同 unsafe.Pointer 但 dynamic-type 不同（罕见），dedup 仍正确（type 区分）；但若两实例不同 dynamic-type 共享同一 vtable，map 视为不同 key 导致重复 OnResolve 注册。方案：当前是假设性风险，cli 只产 *cli.SubagentLinker。注释加 "interface 键 dedup 依赖 (T,P) 元组；多后端共存场景需重新评估"。或改 map[any]struct{} 显式标注。 → #372

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

- [ ] **R248-ARCH-1 — Capabilities interface 内聚弱：必需 + 可选混在一起（P1）** [REFACTOR]: `internal/dispatch/capabilities.go:34` Send 是 contract-required（panic）、Takeover/ReplyFooter 是 contract-optional（默认值），三方法 lifetime + failure-mode 都不同被强行打包。方案：拆 `MessageSender`（必需）+ `TakeoverHook` / `ReplyFooterHook`（可选 1-method interface），或保持 Capabilities 但 Send 拎到 DispatcherConfig.Sender 单独字段（required）。 → #373
- [ ] **R248-ARCH-3 — DispatcherConfig.SendFn 等 Deprecated 字段无清理路径（P2）** [REFACTOR]: `internal/dispatch/dispatch.go:212-244` 同时暴露 Capabilities + 3 个 Deprecated *Fn，godoc 说 transition 但没指明何时摘除。naozhi 的 sessionSendLegacy 已固化 5 轮 review。方案：加 R-DEPRECATED-DISPATCH-FN tracker 含具体清理触发；或在本批把测试 caller 改 closureCapabilities literal，直接删 Deprecated 字段。 → #374
- [ ] **R248-ARCH-4 — AgentLinker interface 4 方法偏宽 + cli.LinkInfo 跨包泄漏（P2）** [REFACTOR]: `internal/session/agentlink/agentlink.go:33-50` interface 在 session/agentlink 子包但 import cli + 返 cli.LinkInfo — 依赖反转没真正发生。OnResolve(taskID, toolUseID, internalAgentID) 是 Claude-CLI-specific 签名，ACP backend 无法实现 noop（只能传空字符串假装）。方案：(a) LinkInfo 复制到 agentlink；(b) OnResolve 改 OnAgentReady(AgentID) + Lookup(AgentID)；(c) 拆 Resolver / Notifier / PathProvider 三小接口。 → #375
- [ ] **R248-ARCH-5 — AgentLinker interface 放 session 子包方向不对（P2）** [REFACTOR]: `internal/session/agentlink/agentlink.go` Go 风格"接口放 consumer 处定义"。唯一 producer cli.SubagentLinker、唯一 consumer server，session 包内 interface 不被使用 — 当前是第三方位置，server import path 比直接定义在 server 还长。方案：移到 internal/server/agentlink.go consumer-local；*cli.SubagentLinker 隐式满足。 → cosmetic
- [ ] **R248-ARCH-6 — Hub struct 28+ 字段 god-struct 仍存（P2）** [REFACTOR]: `internal/server/wshub.go:51-194` PR #327 split 标榜 god-object 拆分但只拆文件没拆 struct。当前 split 让继续抽 BroadcastDispatcher / SubscriberRegistry 等 struct 更难（method receiver 都是 *Hub）。方案：wshub.go 头部加 NEEDS-DESIGN 明确"下一阶段：抽 BroadcastDispatcher / SubscriberRegistry / SendCoordinator 三个 struct"。 → #376
- [ ] **R248-ARCH-7 — wshub.go SetScheduler/SetUploadStore/SetScratchPool setter 滞留（P2）** [REFACTOR]: `internal/server/wshub.go:308-321` split 把 5 类职责拆出 5 文件，但 3 个 setter 留 wshub.go；cronHubOps interface 定义（line 303）唯一 caller 是 broadcast/send 路径。方案：搬到 wshub_send.go 或新建 wshub_lifecycle.go；或注释明确 "wshub.go = struct + ctor + lifecycle，handlers 在 wshub_*.go"。 → cosmetic
- [ ] **R248-ARCH-8 — PR #330 commit oversold 关闭 R242-GO-10 / R242-ARCH-17 等（P2）** [REFACTOR]: `internal/server/wshub.go:86-88` 仍自陈 "NEEDS-DESIGN R242-GO-10: 改抽 MessageEnqueuer interface"。PR #330 抽 Capabilities 同时声称关闭一组 ARCH 条目但 dashboard send 路径（Hub.queue 字段）仍直接引 *dispatch.MessageQueue。方案：要么删 commit msg 的"同时关闭"清单，要么补 follow-up PR 把 Hub.queue 抽成 MessageEnqueuer interface（Enqueue/Discard/Mode/CollectDelay/DoneOrDrain/ShouldNotify 6 方法）。 → #377
- [ ] **R248-ARCH-9 — dispatchCapabilities + Hub.sendWithBroadcast 双层 nil-fallback 职责模糊（P3）** [REFACTOR]: `internal/server/send.go:644-670` Capabilities 层 panic（生产语义）vs Hub 层 hub==nil 悄悄降级 sess.Send（headless 语义），同 send 调用在不同 wiring 下行为差异极大但无显式 mode 字段。方案：headless 应显式声明 HeadlessCapabilities 而非 Server.sendWithBroadcast 内部隐式判断。 → #379
- [~] **R248-ARCH-10 — spawningKeys close-before-delete 靠 godoc 注释而非类型保证（P3）** [REFACTOR]: `internal/session/router_lifecycle.go:540-559` lock-order-by-convention 脆弱模式。方案：封私有方法 `r.markSpawnDoneLocked(key)` (caller 持锁)，单点维护两步顺序 — 类型不能强保证但调用方至少不能搞错局部顺序。

### 代码质量 / godoc / 命名

- [x] **R248-CR-1 — capabilities.go 注释/代码比 1:1（P1）** [REFACTOR]: `internal/dispatch/capabilities.go:1-91` 类型 godoc 含 60+ 行混杂 review-history 与契约说明，超过实际 type+method 代码量。 *(已实施：Capabilities interface godoc 25→9 行；NoopCapabilities 14→6 行；保留契约说明 + R243-ARCH-10 + R248-ARCH-2 anchor。)*
- [x] **R248-CR-2 — wshub.go wiredLinkers 单字段堆叠 5 个 review 编号（P1）** [REFACTOR]: `internal/server/wshub.go:175-194` 注释提 R201/R230/R231/R233B/R239 等价于 git log 摘要。 *(已实施：godoc 18→9 行；保留 R201-CRIT-2 (历史 leak fix) + R239-ARCH-I (interface 化等价性) 两 anchor，其他历史编号删除。)*
- [x] **R248-CR-3 — dispatchCapabilities 类型名与 dispatch.Capabilities interface 易混淆（P1）** [REFACTOR]: `internal/server/send.go:644` capabilities.go godoc line 24 已用了 "serverCapabilities" — 文档与实际类型名不一致。 *(PR #343 已实施：dispatchCapabilities → serverCaps；server/send.go + server.go + dispatch/capabilities.go godoc 同步对齐。)*
- [~] **R248-CR-4 — agentlink.go 包级 godoc 17 行 + 三段 Anchor 提 4 个编号（P2）** [REFACTOR]: `internal/session/agentlink/agentlink.go:1-17` 包文档读者多数 IDE hover 看不下。方案：留前 5 行（接口存在意义 + 谁实现），Anchor 段移到 docs/TODO.md。
- [ ] **R248-CR-5 — AgentLinker.Query vs QueryOrResolveFast 命名差异不表达行为（P2）** [REFACTOR]: `internal/session/agentlink/agentlink.go:33-50` 两方法返回 (LinkInfo, bool) 签名一样，仅靠 godoc 区分语义。方案：改 Lookup（缓存只读）+ Resolve（含 stat fallback）。 → cosmetic
- [x] **R248-CR-6 — NoopCapabilities.Send panic 契约埋在 type docstring 中段（P2）** [REFACTOR]: `internal/dispatch/capabilities.go:65-83` 方法本身只有 "see type docstring"，IDE go-to-definition 跳方法时只看 method godoc，panic 契约对 hover 隐形。方案：把 "panics with msg X，原因 Y" 完整写在方法 godoc 上方。 *(已实现：capabilities.go:60-62 Send method godoc 完整说明 panic 原因 / Takeover/ReplyFooter 各有 godoc)*
- [ ] **R248-CR-7 — wshub_agent.go vs wshub_subscribe.go handleAgentSubscribe 文件归属（P2）** [REFACTOR]: `internal/server/wshub_agent.go` 包含 (a) maybeWireLinkerTailer 内部 wiring + (b) enrichSnapshot + (c) handleAgentSubscribe WS handler 三种职责。方案：把 (c) 并入 wshub_subscribe.go（同 ValidateSessionKey 入口模式），wshub_agent.go 留 wiring + tailer 桥。 → cosmetic
- [x] **R248-CR-8 — dispatchCapabilities 三个方法各 3 行 forward 缺 godoc 解释（P3）** [REFACTOR]: `internal/server/send.go:649-669` 应说明"为什么不用 method value &c.s.sendWithBroadcast" — *Server 是接口受体，method value 会对每次调用 alloc funcval。 *(已实施：serverCaps type godoc 加 "WHY METHODS, NOT METHOD-VALUE CLOSURES (R248-CR-8)" 段说明 funcval / receiver-box / 测试可换 fake 三点；Send/Takeover/ReplyFooter 三方法各加 1 行 anchor 指回 type godoc 避免重复。R248-CR-8。)*

### 性能（已审，全部确认无回归 — 见 Reviewer 3 报告）

- 4 PR 整体 perf-中性偏正：interface dispatch 开销可忽略（per-turn 调用 1-3 次 vs 100ms-30s LLM I/O）；dispatchCapabilities 单指针 inline 无 boxing；closureCapabilities 仅 deprecated 路径多 1 跳；PR #329 实质是性能优化（去 20ms tick + 减 *time.Timer alloc）；PR #330 Dispatcher 结构体净减 8 字节；PR #331 wiredLinkers 接口 dedup 仍按 (itab, ptr) 工作。3 项 P3 均建议先 profile 再动。

