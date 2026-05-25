## Round 235 — 5-agent 并行 code review 第 45 轮（2026-05-23）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描，本轮直接修 18 处（见顶部摘要 R235-CR-1/2/5/6/10/11/12/13、R235-SEC-1/3/4/5/6/7/8/9、R235-GO-1/5/7、R235-PERF-17/20）。下方为本轮新发现且不适合直接修的条目（破坏兼容 / 跨包重构 / 需 RFC / 方案不唯一）。

### 架构（高优先）— 本轮新发现

- [ ] **R235-ARCH-1 — `internal/cli/backend` 反向 import 父包 `internal/cli`（P1）**：backend.Profile.NewProtocol 直接构造 `cli.ClaudeProtocol` / `cli.ACPProtocol`，导致 cli 想用 backend.Get 必须 blank-import 或硬编码 wrapper.go 的 `backendDisplayName` / `isKnownBackendID` switch（自陈债）。建议把 backend 提升为兄弟包 `internal/clibackend` 或 `internal/agentprofile`，反转为 cli→backend；最小路径是 backend 仅暴露 Profile 元数据（DisplayName/DefaultBinary/DetectInProc/HistoryDir/CostUnit/Features），把 NewProtocol 反向到 cli 包内（`cli.RegisterProtocolFactory`）。Breaking: 是（仅 internal）。 → discarded [dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)]
- [ ] **R235-ARCH-2 — cron / sysession 各自定义 RunState / TriggerKind / ErrorClass（P1）**：两套字符串枚举语义高度重叠（succeeded/failed/timed_out/canceled、scheduled/manual、validation/upstream/timeout/panic）。注释自陈"Mirrors cron.RunState semantics"。建议 `internal/runschema` leaf 包共享 `type State string` / `type TriggerKind string` / `type ErrorClass string`，cron/sysession 改 alias。dashboard.js 也只剩一组常量。Breaking: 否（type alias 源码兼容）。 → cosmetic [new-B: runschema leaf-pkg]
- [ ] **R235-ARCH-3 — `dispatch.Dispatcher.scheduler` 仍持具体 `*cron.Scheduler`（P1）**：cron→session 已用 SessionRouter interface 反转，server→session 用 HubRouter，唯独 dispatch→cron 没抽 consumer interface。建议 `dispatch/cron_consumer.go` 定义 `CronScheduler` interface（AddJob / NextRun / ListJobs / DeleteJob / PauseJob / ResumeJob 6 method），contract_test 加 `var _ dispatch.CronScheduler = (*cron.Scheduler)(nil)`。 → discarded [dup of #457 [ARCH-DISP-1 DispatcherConfig concrete]]
- [ ] **R235-ARCH-4 — `cli.Wrapper.ShimManager` 是导出可变 `*shim.Manager`，protocol/transport 未拆分（P1）**：注释自陈"R230-ARCH-13 / R231-ARCH-7 已知债"。sysession.Runner 已走绕过 shim 的旁路 → transport 抽象缺失。建议 `cli.Transport` interface（StartSession / Reconnect / Close），ShimManager 适配为它的实现；新增 `WithTransport` option。Breaking: 是（cmd/naozhi/main.go 一处真消费方）。 → discarded [dup of #729 [R246-PERF-14 sysession runner subprocess] (recurring multi-round)]
- [ ] **R235-ARCH-5..30 — 见各 reviewer 报告**：包含 config 反向 import session/project（ARCH-5）、3 个 workspace 概念名共享（ARCH-6）、platform.QuestionItem 与 cli.AskQuestionItem 双向手抄（ARCH-7）、feishu→transcribe 直依（ARCH-8）、cron / dispatch 各有 prompt/schedule 校验二级实现（ARCH-9）、Router struct 字段 30+（ARCH-10）、cli 包 67 文件混杂（ARCH-11）、contract_test 未覆盖 dispatch.CronScheduler / sysession.Manager（ARCH-12）、session 通过 blank import 触发 history backend init（ARCH-13）、SessionGuard / MessageQueue 运行时 either-or（ARCH-14）、server 13 个 *Handlers god struct（ARCH-15）、cli.Process 字段导出与并发约束注释冲突（ARCH-16）、eventlog/schema 未承担 single source of truth（ARCH-17）、dispatch.DispatcherConfig.Router 字段类型 *session.Router（ARCH-18）、router.Version 同时承载 data + render（ARCH-19）、process.go 1500+ 行字段 60+（ARCH-20）、discovery 同时依赖 cli + cli/backend（ARCH-21）、replyTagForBackend sync.Once 兜底掩盖 wireup 时序（ARCH-22）、Reactor / QuestionCardSender type-assertion 模式（ARCH-23）、~~scheduler.go 2400+ 行混合多职责（ARCH-24）✓ 已修：PR #309 6-stage split scheduler.go 至 852 行 + 7 职责文件~~、sysession.Runner 硬编码 claude bin（ARCH-25）、Wrapper.Spawn 100 行 protocol+transport 混杂（ARCH-26）、validateWorkspace / cron.workDirUnderRoot 重复（ARCH-27）、cli 包同住 image/thumbnail/askquestion/todo DTO（ARCH-28）、ManagedSession.process 直接持 *cli.Process（ARCH-29）、缺 wireup 集中包（ARCH-30）。所有 ARCH 类条目均为方案不唯一 / 跨模块改动，按 Round 节存档供未来 RFC 引用。

### Go 正确性 / 性能（合并到现有跟踪）

- [~] **R235-GO-2 — `runStore.warmCache` 签名暗示返错却 always nil**：实际逻辑正确（fallback 到 diskListNewestFirst），文档与签名分叉但行为安全；判定 P3 cosmetic，归到 R231-PERF-1 主条目跟踪。
- [ ] **R235-GO-3 — `cron.Stop()` 包装 goroutine 在 deadline-hit 路径永久泄漏**：注释 R222-GO-10 已承认 intentional orphan；非 production 影响，仅 goroutine-leak 检测器在 test 中报泄漏。需要 RFC 决定是否在 godoc 中显式记录这是 by-design 的 leak，或加 ctx 信号让 goroutine 退出。 → cosmetic [new-B: Stop() goroutine leak godoc]
- [ ] **R235-GO-4 — `buildExcerptFromHistory` 跨行截断后 EXCERPT marker 检测失效（P1）**：512 bytes/line 截断可能把 `---BEGIN CONVERSATION EXCERPT---` 切到两行，使 ReplaceAll 检测不到完整 marker。建议在 `buildExcerpt` 内逐行处理阶段就替换 marker，而非仅末尾一次性扫描。改动 sysession/auto_titler.go 内部，无 breaking。 → #1004 [new-A: buildExcerpt marker]
- [ ] **R235-PERF-1 — `Protocol.ReadEvent(line string)` 强制 string→[]byte 堆拷贝（P1）**：`shimMsg.Line` 是 `string` 是根因，两处改动需同步：shim 协议字段改 `json.RawMessage`，ReadEvent 签名改 `[]byte`。同根因主条目持续跟踪 R231-PERF-1。Breaking: 是（shim wire format + Protocol interface）。 → discarded [dup of #461 [CLI-PERF-1]]
- [ ] **R235-PERF-7 — `linker.Resolve` 每 task_started spawn 裸 goroutine**：8 并发上限已生效，但 worker pool + 任务队列收益更大；归 R230-PERF-3 主条目。 → discarded [dup of #644 [R218B-GO-3 readLoop linker.Resolve goroutine no ctx] (recurring multi-round)]
- [ ] **R235-PERF-8 — `permission_request` 路径在 readLoop 同步走 json.Unmarshal+Marshal+Write**：建议响应改非阻塞 reply channel + writeLoop。 → #1013 [new-A: permission_request reply chan async]
- [ ] **R235-PERF-9 — `marshalJobsLocked` 全量 SortFunc + json.Marshal 在 s.mu 内**：建议 marshal 移出锁（先快照、释锁、再 marshal）。 → discarded [dup of #482/#675/#551 marshalJobsLocked]
- [ ] **R235-PERF-10 — `sanitizeStderrLine` 慢路径每行 alloc strings.Builder**：sync.Pool 化。 → #1015 [new-A: sanitizeStderrLine pool]
- [ ] **R235-PERF-12 — `applyMetadata` O(N²) merge**：改 map keyed by Unit。 → #1011 [new-A: meteringUsage map]
- [ ] **R235-PERF-13~16/18/19 — 杂项 alloc 优化**：redactPathsInCronError pool / EventEntry struct copy 延迟 / buildUserEntry 单图快路径 / textBuf 容量回收 / addJobAcquiringLock per-chat O(1) 计数 / runDeadlineWatchdog goroutine pool。

### 安全（NEEDS-DESIGN）

- [ ] **R235-SEC-2 — `transcriptTurn.Input` 是 `json.RawMessage` 经 SetEscapeHTML(false) encoder（P1）**：当前 dashboard JS 用 textContent 安全，但服务端无强制；建议 `b.Input` 通过 HTML-escaping JSON encoder 重新编码后再赋给 Input any。改动需评估 dashboard 是否依赖 raw bytes 顺序。 → discarded [dup of #461 [CLI-PERF-1]]

### 代码质量（NEEDS-DESIGN）

- [ ] **R235-CR-3 — `ErrClassPausedConcurrent` 常量定义后从未发出**：`registerJob` 闭包并发暂停分支静默 return 不调 finishRun，使 dashboard 永远收不到 paused_concurrent 状态。建议要么发 emitOverlapSkipped(ErrClassPausedConcurrent)，要么在常量旁加注释 + 删除。需要决定 dashboard UX 是否需要这个状态。 → #1001 [new-A: ErrClassPausedConcurrent dead]
- [ ] **R235-CR-4 — `XxxByID`/`Xxx` 6 对方法 60 行重复（DeleteJob/PauseJob/ResumeJob × ByID/byPrefix）**：抽 `deleteJobAfterLookup` / `pauseJobAfterLookup` / `resumeJobAfterLookup` helper。无 breaking 但改动 ~120 行。 → cosmetic [new-B: cosmetic]
- [ ] **R235-CR-7 — `emitRunEnded` 无 godoc 与 emitRunStarted 不对称**：补 godoc 说明 CronRunEndedTotal 在 finishRun 末尾 bump 而非函数内部，防止维护者错误对齐造成 double-count。 → cosmetic [new-B: emitRunEnded godoc]
- [ ] **R235-CR-9 — `skipAppendTrim` 三处 appendsSinceTrim=0 重置语义不齐**：注释解释或对齐到"只在真正触发 trim 时才重置"。 → cosmetic [new-B: skipAppendTrim semantics]
- [ ] **R235-CR-14 — `validateSchedule` 与 `PreviewSchedule` 重复 cronParser.Parse + 两次 sched.Next**：复用 `schedulePeriod`。 → cosmetic [new-B: validateSchedule dup]
- [ ] **R235-CR-15 — diskListNewestFirst 同秒 mtime tie-break 用 runID（hex）不反映时间顺序**：注释说明，或在 list 路径用 StartedAt 二次排序（成本高）。 → cosmetic [new-B: diskListNewestFirst tie-break]

## Round 234 — 5-agent 并行 code review 第 44 轮（2026-05-23）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描，本轮直接修 11 处（见顶部摘要 R234-SEC-1/-4/-9/-10、R234-PERF-12、R234-CR-9/-10/-11、R234-GO-7/-13/-14）。下方为本轮新发现且不适合直接修的条目（破坏兼容 / 跨包重构 / 需 RFC / 方案不唯一）。

### 架构（高优先）— 本轮新发现

- [ ] **R234-ARCH-1 — `dispatch` 同时直依 `cli` + `cron` + `session` + `project`（P1）**：dispatch 既是 IM↔session 中间层又把 cli.EventCallback / ImageData / SendResult 当裸类型在签名中传，commands.go 反向调 cron.Scheduler。建议在 dispatch 内定义 `SendResult` / `Event` / `Image` 最小 DTO，由 session 层做 cli↔dispatch 翻译；commands 的 cron 操作走 `CronAdmin` interface 注入。改动 ~6 文件 ~150 行，无运行时 breaking。 → discarded [dup of #457 [ARCH-DISP-1] DispatcherConfig concrete (recurring multi-round)]
- [ ] **R234-ARCH-2 — `cli.wrapper` 反向 import `internal/shim`（P1）**：cli 是协议+子进程层，shim 是 cgroup 边车；当前 cli→shim 的导入让 cli 无法在没有 shim 的环境编译/单测，shim 协议演进绑死 cli 协议。建议把 shim 抽象为 `cli.LauncherSpawner` interface，具体 `shim.Launcher` 在 session 层注入。需 R231-ARCH-1（runner 旁路 wrapper）落地之后再做以避免改两次签名。 → discarded [dup of #729 [R246-PERF-14 sysession runner subprocess] (recurring multi-round)]
- [ ] **R234-ARCH-3 — `sysession` 与 `cron` 各自定义 RouterView，DTO 散在两包（P1）**：cron `SessionRouter` interface (scheduler.go:72) + sysession `SystemSessionRouter` (router.go:25) 形成两套 router 适配器，都把 cli.EventEntry 当公共契约。建议在 `internal/session/api`（新子包，无 cli 依赖）放统一 `RouterView` + DTO，cron/sysession/scratchPool/quick session 共用。改动 ~10 文件，长期收益大。 → discarded [dup of #720 [R242-ARCH-2 exempt session pool] + #432 [R176-ARCH-N4 ManagedSession state machine] (recurring multi-round)]
- [ ] **R234-ARCH-4 — `internal/session/contract_test.go` 反向 import dispatch+server+cron+upstream（P1）**：即使 _test.go 文件，contract test 引用 dispatch/server/cron/upstream 让 session 包成为依赖图根+叶。建议把 contract_test 移到 `internal/integration/session_contract_test` 独立包。 → cosmetic [new-B: contract_test relocate]
- [~] **R234-ARCH-5 — server.send / dashboard_send / dispatch.SendSplitReply 三套发送路径（归档 2026-05-23）**：明确归到 R230-ARCH-2 SendOrchestrator 主条目；本轮仅记录 server.send 是第三条独立发送路径（reverse-node + WS push）作为未来 SendOrchestrator 设计输入。本批 PR 关闭子症状条目。
- [ ] **R234-ARCH-6 — Shutdown 顺序无形式化合约（P1）**：cron.Scheduler.Stop / sysessionMgr.Stop / router.Shutdown 在 server.Shutdown 序列里位置不明。建议写 ADR `docs/rfc/shutdown-order.md` + 各组件导出 `WaitForExit(ctx) error`，由 server 串行 wait。 → cosmetic [new-B: shutdown-order ADR]
- [ ] **R234-ARCH-7 — `cron.SessionRouter.GetOrCreate` 暴露完整 *ManagedSession（P2）**：调度器只需 Send，但接口暴露 50+ 方法的 ManagedSession，cli 接口塌陷同款。建议进一步收敛为 `Sender` 接口。 → discarded [dup of #752 [R238-ARCH-7] (recurring multi-round)]
- [~] **R234-ARCH-8 — `server.Hub` 直 import dispatch+cron+cli+project+session（P2）**：典型 god hub。建议拆 `WSTransport`（仅 conn lifecycle）+ `WSCoordinator`（业务），需独立 RFC。 *(PR #327 R243-ARCH-2 已 6-stage split：wshub.go 2028→525 行，方法分到 _broadcast/_send/_subscribe/_eventpush/_upgrade 5 文件。但 Hub struct 未拆 — 28+ 字段保持集中以维持锁不变量；进一步抽 BroadcastDispatcher / SubscriberRegistry 子 struct 跟踪到 R248-ARCH-6。)*
- [ ] **R234-ARCH-9 — Channel adapter 越权读 router 状态：dashboard_session.go / dashboard_discovered.go 直 import cli+session（P2）**：缺 wire DTO 一层，cli.EventEntry 字段改名会自动 break dashboard JSON shape。建议 `internal/server/api/dto` 定义 SessionDTO/EventDTO，~20 文件。 → cosmetic [new-B: HubRouter narrow]
- [ ] **R234-ARCH-10 — dispatch ↔ platform 双向：platform webhook 回调 leak dispatch.IncomingMessage / Reply 内部类型（P2）**：建议在 `internal/platform` 定义最小 wire types，dispatch 提供 `From(IncomingMessage)` 适配。 → cosmetic [new-B: dispatch↔platform DTO]
- [ ] **R234-ARCH-11 — 命名空间策略碎片化：sys: / cron: / scratch: / kiro: / chatKeyPrefix 5 类前缀无中央注册表（P2）**：建议 `internal/session/namespace.go` 集中 `NamespacePrefix` enum + `ParseKey(s)`，CI 加 grep test 拒绝裸字符串。前置 R233-ARCH-5。 → discarded [dup of #728 [R222-ARCH-10 ManagedSession semantic tags] (recurring multi-round)]
- [ ] **R234-ARCH-12 — Protocol/Sink/Tailer 三大事件接口契约未文档化（P2）**：建议新建 `docs/rfc/event-pipeline-contracts.md` + `internal/cli/protocoltest` 共享测试套件（类似 fstest.MapFS）。 → cosmetic [new-B: event-pipeline-contracts ADR]
- [ ] **R234-ARCH-13 — server/agent_tailer 直 import cli，IO 路径绕过 router（P2）**：建议 tailer 走 `router.Tail(key) <-chan EventDTO`，router 内部决定从哪源取（persisted vs live）。 → cosmetic [new-B: agent_tailer router.Tail]
- [ ] **R234-ARCH-14 — buildServer 初始化顺序 fragile（P2）**：建议抽 `serverDeps` struct + `buildCore`/`buildHandlers`/`buildHub`/`wire` 分步。 → cosmetic [new-B: buildServer order]
- [~] **R234-ARCH-15 — sysession.runner 旁路 cli wrapper 但 auto_titler 仍要读 cli.EventEntry（归档 2026-05-23）**：明确归到 R234-ARCH-3 的统一 RouterView 方案；待 R234-ARCH-3 落地时一并解决，子症状条目本批 PR 关闭。
- [ ] **R234-ARCH-16 — server/discovery_cache.go 越层依赖 project + cli（P3）**：建议 router 暴露 `ExcludedSessionIDs() iter.Seq[string]`，discovery 自治。~30 行。 → cosmetic [new-B: discovery_cache router.ExcludedSessionIDs]
- [ ] **R234-ARCH-17 — dispatch.consumer.go 引用 *session.ManagedSession 而非更窄 SessionHandle（P3）**：建议 `dispatch.SessionHandle interface { Send(...); Key() string }`。 → cosmetic [new-B: SessionHandle narrow]
- [ ] **R234-ARCH-18 — internal/session/testutil.go 不带 _test 后缀 + 无 build tag（P3）**：可能被生产二进制 link。建议 rename 为 testutil_test.go 或加 `//go:build testing`。 → #1017 [new-A: testutil.go production-link risk]
- [~] **R234-ARCH-19 — internal/dispatch/dispatch_test.go 单文件 import cli/cron/platform/session 4 个生产包（归档 2026-05-23）**：R234-ARCH-4 子症状；与 contract_test 重定位方案一起处理，本批 PR 关闭子条目。
- [ ] **R234-ARCH-20 — wshub_agent.go 仅依赖 session 但被并入 server 包导致编译图污染（P3）**：建议挪到 `internal/server/wshub` 子包。 → cosmetic [new-B: agent_tailer router.Tail]
- [ ] **R234-ARCH-21 — 跨包共享 limits 常量风格不一（cron/limits.go vs server/server.go vs dispatch/dispatch.go）（P3）**：建议集中到 `internal/limits`。 → cosmetic [new-B: minor arch tidies]
- [ ] **R234-ARCH-22 — `internal/cli/backend` 同时被 server 与 session 直接 import（P3）**：建议拆 `backend.Profile`（值类型）vs `backend.Launcher`（行为，仅 cli 用）。 → cosmetic [new-B: backend.Profile vs Launcher split]
- [ ] **R234-ARCH-23 — cron 走 router 而 quick session 走 dispatch.SendSplitReply 路径不一致（P3）**：建议 ADR `docs/rfc/cron-vs-dispatch-paths.md` 或归 R230-ARCH-2 SendOrchestrator。 → discarded [dup of #459 [ARCH-SVR-1 send-with-broadcast]]
- [ ] **R234-ARCH-24 — server/upload_store.go import cli 仅为 cli.ImageData（P3）**：建议 upload_store 输出自身 `Blob struct{Data []byte; MIME string}`，dashboard_send 转 cli.ImageData。 → cosmetic [new-B: upload_store decoupling]
- [ ] **R234-ARCH-25 — session/eventlog_bridge.go 是事实上的 fan-out hub 但命名 bridge（P3）**：建议 rename 为 event_pipeline.go + 引入 `EventPipeline` + `[]EventSink`。 → cosmetic [new-B: eventlog_bridge rename]

### Go 正确性 / 并发 — 本轮新发现

- [ ] **R234-GO-3 — scheduler.go:778 `go trimAll` goroutine 无 WaitGroup（P1）**：Stop 不等待此 goroutine 退出，半删 runs 目录残留 / 重启并发 trimAll 可能与 per-job lock 之外的 ReadDir+Remove 出现窗口。建议给 `trimAll` 加 `s.gcWG sync.WaitGroup`，Stop 先 gcWG.Wait()（带短超时）+ 传 ctx 让 trimAll 内每个 jobID 循环检查 `ctx.Err()`。 → #1019 [new-A: trimAll WaitGroup]
- [ ] **R234-GO-4 — runstore.cacheGet 双锁窗口（P2）**：第一次释放 entry.mu 后 warmCache 在 entry.mu.Lock 前另一 Append 已 cacheHeadPush no-op + trimJobLocked 触发 cacheTrimAfterDisk no-op，warmCache 再读磁盘可能漏掉刚 Append 条目。建议在 warmCache 内 entry.mu.Lock 前先 jobLock.Lock（已有此模式）。 → discarded [dup of #556 [R242-GO-8] cacheHeadPush O(N)]
- [ ] **R234-GO-8 — runstore.diskListNewestFirst 不区分 mtime-only 与 full-parse 路径（P2）**：warmCache 走也会 ReadFile 全部文件。建议拆 `diskListMtime`（只 ReadDir+stat+sort）和 `diskReadSummaries`（batch ReadFile）。 → discarded [dup of #796 / #522 diskListNewestFirst mtime]
- [ ] **R234-GO-10 — runstore.trimJobLocked sort 用 cmp.Compare(UnixNano) 而非 time.Compare（P3）**：边界精度 + 风格。建议 `slices.SortFunc(items, func(a,b) int { return b.mtime.Compare(a.mtime) })`。 → cosmetic [new-B: cmp.Compare→time.Compare]
- [~] **R234-GO-15 — trimAll goroutine 无 ctx 传播 Stop 无法中断（归档 2026-05-23）**：归 R234-GO-3 同主条目跟踪（gcWG.Wait + ctx.Err 一起做），本批 PR 关闭子条目。

### 安全 — 本轮新发现

- [ ] **R234-SEC-2 — `handleTrigger` 无 per-IP 速率限制（P1）**：`POST /api/cron/trigger` 每次触发 `session.GetOrCreate + sess.Send`，无任何限流；持有 token 的脚本可在秒级把 maxJobs=500 全部触发耗尽 shim/cgroup 资源。建议新增 `triggerLimiter *ipLimiter`（如 `rate.Every(2s), burst=3`）。 → discarded [dup of #691 [R242-CR-3 GET /api/cron 1Hz no rate limiter] (recurring multi-round)]
- [ ] **R234-SEC-3 — `runsLimiter` 共享 list/detail/transcript 三端点 IO 代价 100x 不对称（P2）**：transcript 走 8MB JSONL+Scanner，list 走 cache。建议 transcript 单独 `transcriptLimiter`（`rate.Every(5s), burst=5`）。 → discarded [dup of #691 [R242-CR-3 GET /api/cron 1Hz no rate limiter] (recurring multi-round)]
- [ ] **R234-SEC-5 — transcriptResponse Input json.RawMessage 透传未脱敏（P2）**：tool_use.input 含 Bash 命令明文，可能含 API 密钥/DSN。建议对 command/file_path/url 做 200 字符截断，或移除 Input 仅留 Summary。**dashboard JS breaking**。 → discarded [dup of #461 [CLI-PERF-1]]
- [ ] **R234-SEC-6 — `handleList` 返回所有 job 的 LastResult/Prompt 全量轮询带宽放大（P2）**：50 jobs × (8KB prompt + 4KB result + 5×summary) ≈ 1MB/req × 1Hz = 1MB/s。建议 list 返回截断 prompt（1KB），detail 接口返回全量；或加 server-side `?search=`。**dashboard JS fuzzy-search 需迁移**。 → discarded [dup of #671 [R242-PERF-7 KnownSessionIDs rebuilds map] (recurring multi-round)]
- [ ] **R234-SEC-7 — `Job.LastResult` 落盘无 secret-pattern 过滤（P2）**：claude 输出可能含明文 sk-ant-/ghp_/AKIA token。建议 `recordResultP0WithSanitised` 增加可配置黑名单 + 类似 `isSensitiveDownloadName` 的后处理。 → #1006 [new-A: cron LastResult secret filter]
- [ ] **R234-SEC-8 — `flattenJSONLEvent` tool_use.Input 字段无大小守卫（P3）**：500 turns × 256KB/line = 128MB 序列化输出。建议 `len(b.Input) > maxToolInputBytes`（64KB）截断为 `[truncated]` 或置空。 → #1018 [new-A: tool_use input cap]
### 性能 — 本轮新发现

- [~] **R234-PERF-1 — runstore.cacheHeadPush O(N) memmove（归档 2026-05-23）**：归 R234-GO-1 / R233-PERF-2 同主条目（ring buffer 改造），本批 PR 关闭子条目。
- [~] **R234-PERF-2 — shimWriter.Write fast-path string(data[:len-1]) heap-copy（归档 2026-05-23）**：每次 stdin write 把 4KB payload 拷贝到 string 仅为 shimClientMsg.Line string 字段；归 R71-PERF-H1 主条目跟踪（需 shim 协议 compat 评估），本批 PR 关闭子条目。
- [~] **R234-PERF-3 — protocol_claude.ReadEvent json.Unmarshal([]byte(line),...)（归档 2026-05-23）**：阻塞在 shim 协议 bump，归 R231-PERF-1 主条目跟踪，本批 PR 关闭子条目。
- [~] **R234-PERF-4 — KnownSessionIDs 无 TTL 缓存（归档 2026-05-23）**：归 R233-PERF-3 同主条目跟踪（atomic.Pointer[knownSessionIDsCache] 30s TTL + finishRun/DeleteJob 失效），本批 PR 关闭子条目。
- [~] **R234-PERF-5 — TranscriptReader.readLocked 每 200ms 重 open/seek/read/close（归档 2026-05-23）**：归 R233-PERF-4 主条目跟踪（keep *os.File + Seek/ReadAt + inode 变更才重开），本批 PR 关闭子条目。
- [~] **R234-PERF-9 — runstore.skipAppendTrim time.Now 在 fast-exit 之前（归档 2026-05-23，已核实为误报）**：实际 line 254 已先做 `len(entry.runs) > 0` 守卫，time.Now 在 fast-path 之后；review 误读控制流。本批 PR 关闭。
- [ ] **R234-PERF-10 — parseTranscriptTime 每行 RFC3339Nano 解析 ~300ns（P2）**：250 line/s × 300ns = 75µs/s。建议 hand-parse 整数字段或 ParseInLocation+UTC 缓存。 → #1012 [new-A: parseTranscriptTime perf]
- [ ] **R234-PERF-13 — readShimLine 错误漏 cap drain 路径（P3）**：bufio chunk 临时切片漏。 → #1014 [new-A: readShimLine drain]
- [ ] **R234-PERF-14 — runstore.warmCache 持 entry.mu 做 ReadDir+N×ReadFile 阻塞 dashboard 冷启动（P3）**：建议 warm 异步，首次 Recent miss 立即返空切片，后台 populate。 → discarded [dup of #527 / #871 warmCache lock]
- [ ] **R234-PERF-15 — agent_tailer pollOnce 200ms ticker 对 refCount==0 silent tailer 仍 open/close（P3）**：建议 silent + size-unchanged 时 backoff 到 2s。 → discarded [dup of #865 [R245-PERF-15] agent_tailer 200ms tick]
- [ ] **R234-PERF-16 — protocol_claude.extractAskQuestion 每 assistant 事件全 block 扫描（P3）**：建议 `strings.Contains(rawContent, "AskUserQuestion")` 早 short circuit。 → #1008 [new-A: extractAskQuestion early-circuit]

### 代码质量 — 本轮新发现

- [ ] **R234-CR-1 — runstore.truncateForRetry 与 scheduler.sanitiseRunResult 共享 truncate-with-suffix 但分散两文件（P2）**：本轮已修 truncatedSuffix 字面量统一（R234-CR-9 直接修）；后续可将 truncate-with-suffix helper 移到 limits.go 单点共享，让两个 caller 都引用。 → cosmetic [new-B: truncateForRetry helper]
- [ ] **R234-CR-2 — workDirUnderRoot/workDirReachable 在 cron + server 各一份（P2）**：建议移到 `internal/osutil` 或新 `internal/fsutil` 共享。本轮已为 workDirReachable 加 root-containment 不强制注释（R234-CR-11）。 → cosmetic [new-B: workDirUnderRoot dedup]
- [ ] **R234-CR-3 — generateID/generateRunID 都委派 generateHexID 三个名字一个函数（P2）**：建议要么收敛为单一 generateID 要么明确分歧意图。 → cosmetic [new-B: generateID alias]
- [ ] **R234-CR-4 — DeleteJob/PauseJob/ResumeJob ByPrefix vs ByID 6 方法 lock/persist/save 模板重复（P2）**：建议 `mutateJob(id string, fn func(*Job) error)` helper 内置生命周期。 → cosmetic [new-B: mutateJob helper]
- [ ] **R234-CR-5 — `var stopBudget` package-level mutable global 仅为测试注入（P2）**：建议移到 `SchedulerConfig.StopBudget` field + applyDefaults。 → cosmetic [new-B: stopBudget global]
- [ ] **R234-CR-6 — finishRun 双 sanitise 层级不透明（P3）**：建议 recordResultP0WithSanitised 加注释说明哪些 caller 已 pre-sanitise；或拆 `WithSanitised` / `Raw` 两变体。 → cosmetic [new-B: finishRun sanitise godoc]
### 架构（高优先）— 本轮新发现

- [~] **R233-ARCH-1 — sysession.Runner 完全旁路 CLI Wrapper 抽象（归档 2026-05-23）**: R230-ARCH-1 / R231-ARCH-1 同根因，统一跟踪到 R231-ARCH-1，本批 PR
- [~] **R233-ARCH-2 — cli.Protocol 对 ACP 等无 SubagentLinker 后端抽象塌陷（归档 2026-05-23）**: R219-ARCH-3 / R224-ARCH-3 / R231-ARCH-6 / R233B-ARCH-3 同根因，统一跟踪到 R231-ARCH-6，本批 PR
- [~] **R233-ARCH-3 — dispatch.NotifyTarget / cron.notifyTarget / hub.sendWithBroadcast 三套消息出口（归档 2026-05-23）**: R219-ARCH-8 / R224-ARCH-4 / R230B-ARCH-3 / R231-ARCH-2 / R232-ARCH-9 同根因，统一跟踪到 R232-ARCH-9，本批 PR
- [~] **R233-ARCH-4 — quick session 与 IM 入口走两套 sendAndReply（归档 2026-05-23）**: R230-ARCH-2 / R231-ARCH-2 同根因，统一跟踪到 R230-ARCH-2，本批 PR
- [ ] **R233-ARCH-5 — server.handleQuickSession / scratch drawer 在 router.sessions 之外又长出第二条 lifetime 协议（P2）**: ScratchPool.Close → router.Remove(key) + OptsForKey 在 sweep 之前 Touch，多处不变量耦合在注释里。方案：把 ScratchPool 实现的 ManagedSession lifecycle 接口提到独立 RFC，加 `Router.NotifyScratchExpired` hook。Breaking：否。 → discarded [dup of #720 [R242-ARCH-2 exempt session pool] + #432 [R176-ARCH-N4 ManagedSession state machine] (recurring multi-round)]

### Go 正确性 / 并发 — 本轮新发现

- [ ] **R233-GO-3 — executeOpt runInflight 6 处 Store(&local) 强制 heap escape（P2）**: 每次 cron run 6 个变量逃逸到 heap，每条 run 多 6 次小对象分配。方案：runInflight struct 内 `atomic.Pointer[string]` 字段改为 mutex 保护的直接 value，dashboard 读频率低 lock 成本可接受。Breaking：否。 → discarded [dup of #742 [R238-ARCH-3] runInflight atomic.Pointer]

### 安全 — 本轮新发现

- [~] **R233-SEC-1 — Dashboard CSP 仍 unsafe-inline（多轮 NEEDS-DESIGN 归档 2026-05-23）**: R226-SEC-2 / R227-SEC-9 / R228-SEC-1 / R229-SEC-6 / R230-SEC-1 / R231-SEC-2 / R233B-SEC-1 同根因；前端模板大改造（dashboard.html 80+ 内联 onclick 移到外部 JS 或迁 nonce/hash CSP）。统一收敛到主条目 R231-SEC-2。本批 PR
- [~] **R233-SEC-2 — ExtraArgs 进 CLI argv 仍无 flag allowlist（多轮 NEEDS-DESIGN 归档 2026-05-23）**: R217-SEC-1 / R219-SEC-1 / R225-SEC-1 / R227-SEC-1 / R229-SEC-1 / R231-SEC-4 同根因；Breaking。统一收敛到主条目 R231-SEC-4，本批 PR
- [~] **R233-SEC-3 — allowed_root 未配 + 公网 token 部署只 Warn（多轮 NEEDS-DESIGN 归档 2026-05-23）**: R226-SEC-6 / R227-SEC-3 / R229-SEC-3 / R231-SEC-3 同根因。统一收敛到主条目 R231-SEC-3，本批 PR
- [ ] **R233-SEC-4 — 飞书签名失败前未 dedup nonce（P2）**: 攻击者可 5 分钟窗口内用同 ts+nonce 暴力试不同 body。需谨慎—插入位置改变会打破 challenge 验证流。方案：在 signature verify 之前先 reserve nonce，失败也保留。Breaking：否（行为变化）。 → discarded [dup of #441 [H2 CSP unsafe-inline]]
- [ ] **R233-SEC-7 — 同 IP 可保留 60 个未认证 WS 连接（P2）**: maxWSConns=500 全局，无 per-IP 未认证 cap。方案：未认证 WS 连接 per-IP 20 上限。Breaking：否。 → discarded [dup of #691 [R242-CR-3 GET /api/cron 1Hz no rate limiter] (recurring multi-round)]
- [~] **R233-SEC-8 — /static/dashboard.js 未鉴权且无 SRI（归档 2026-05-23）**: R230-SEC-3 / R231-SEC-11 同根因。统一收敛到 R231-SEC-11，本批 PR
- [~] **R233-SEC-9 — backend ID charset 在 cron CRUD vs WS path 不对齐（P2）**: cron 走 `[a-z0-9_-]`，WS 路径走 `[a-zA-Z0-9_.-]`。方案：抽统一 validateBackendID。Breaking：是（操作员若用 uppercase/dot backend ID）。继承 R232-SEC-5。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
### 性能 — 本轮新发现

- [~] **R233-PERF-1 — ClaudeProtocol/ACPProtocol ReadEvent 每次 byte 复制（归档 2026-05-23）**: 同 R231-PERF-1 一并归档，本批 PR
- [ ] **R233-PERF-2 — runStore.cacheHeadPush 仍 O(N) memmove（P1）**: keepCount=200 每次 Append 触发 200 struct copy shift，持 jobLock 期间执行。方案：改 ring buffer。Breaking：否。 → discarded [dup of #556 [R242-GO-8] cacheHeadPush O(N)]
- [ ] **R233-PERF-3 — KnownSessionIDs 历史面板每次 1Hz 全量遍历 jobs × Recent(200)（P1）**: 50 job × 200 row × ~100B = 1MB 数据移动每秒。方案：scheduler 缓存 atomic.Pointer[map] 由 finishRun/DeleteJob 失效。Breaking：否。 → discarded [dup of #529 + #671 + #592 (recurring multi-round)]
- [ ] **R233-PERF-4 — TranscriptReader.readLocked 每次 Tail open+ReadAll+close（P2）**: 50 tailer × 5/s = 250 syscall/s。方案：持久化 *os.File，每 Tail 只 ReadAt(offset)，inode 变更时重开。Breaking：否。 → #1025 [recurring: transcript reader fd reuse]
- [ ] **R233-PERF-5 — flattenJSONLEvent 每行 unmarshal 到 map[string]any（P2）**: 整 JSON 反射，只用首个 key。方案：改 transcriptContentBlock 6 字段 struct。Breaking：否。 → #1010 [new-A: flattenJSONLEvent struct unmarshal]
- [ ] **R233-PERF-6 — readRun 每个 .json 文件 os.ReadFile cold path（P2）**: 100 文件 × open+stat+alloc+read+close。方案：diskListNewestFirst 仅返回 mtime+runID summary，defer body 到 Get。Breaking：否。 → cosmetic [new-B: diskListNewestFirst tie-break]
- [ ] **R233-PERF-7 — agentTailer 200ms ticker buffered 超 500 时 append([]EventEntry(nil), …) 复制 500 条（P3）**: 每 poll 150KB 内存 copy。方案：ring buffer。Breaking：否。 → discarded [dup of #865 [R245-PERF-15] agent_tailer 200ms tick]
- [~] **R233-PERF-8 — sysession.Manager hookMu RWMutex 改 atomic.Pointer 收益评估（P3）**: 每 tick 两次 RLock 对单写多读场景边际收益小。RWMutex Go 1.21+ 已无锁化常见路径。可选优化，先标记。Breaking：否。 — 评估关闭（已归档 2026-05-23 复核）：godoc 已落地决策——`internal/sysession/manager.go:150-155` hookMu 段落显式说明 RWMutex 选择理由：reads 每 Tick 两次（run start + run end）+ writes 仅 SetCallbacks 一次；RWMutex 让并发 Tick 并行 RLock 无锁竞争。改 atomic.Pointer[func] 需双字段（onRunStarted + onRunEnded），增加一次 atomic.Pointer.Store 协调成本，且 Go 1.21+ RWMutex 在 RLock 路径已是 atomic CAS（src/sync/rwmutex.go），benchmark 收益可忽略。本批 PR。

### 代码质量 — 本轮新发现

- [~] **R233-CR-1 — 4 个独立 fake test router struct（误报关闭 2026-05-23）**: 复核 4 个 fake 服务于不同测试关注点而非"几近重复"——fakeRouter (run_p0_test.go) 携带 configurable error 字段；jitterStubRouter (jitter_test.go) 是 minimal stub 用于 jitter 路径；backendCapturingRouter (scheduler_backend_test.go) 记录 AgentOpts.Backend 用于 backend-routing 测试；fakeSessionRouter (session_router_test.go) 是 full session-router fake。三个共有方法（RegisterCronStubWithChain/Reset/GetOrCreate）表面机械重复，但消费者期望各自不同（错误注入 / no-op / 字段捕获 / 完整生命周期），合并到 option-style 统一 fake 会引入 wrapper/builder 否定 net-positive 收益。归档关闭，本批 PR
- [~] **R233-CR-2 — TriggerCatchup/ErrClassPanic/DaemonTriggerManual 仍 export 但无外部消费者（P3，关闭归档 2026-05-23）**: 同根因 R232-CR-8 已落地——三处 godoc 各加 RESERVED 警告，明确"forward-compat schema 占位"语义；无生产 caller，无测试 pin，但保留 export 让未来填充实现时不破坏 value contract。string value（"catchup"/"panic"/"manual"）外部若 string-match 也能识别。本批 PR 关闭归档
### Round 233 第二批补充（PR #240 review 发现）

#### Go 正确性 / 并发（P1）

- [ ] **R233B-GO-1 — runinflight setPhase/setSessionID 把参数指针存入 atomic.Pointer（P1）**: `r.phase.Store(&phase)` 存的是参数局部变量地址；同样 setSessionID 存 `&id`；executeOpt 1898-1910 里 `ph := PhaseQueued; inflight.phase.Store(&ph)` 也是同模式。当前 Go 编译器会把这些值 escape 到堆上是安全的，但模式依赖 escape 分析；建议用 helper 拷贝到稳定 heap 槽或改 `atomic.Value` + string。Breaking：否（包内）。 → discarded [dup of #742 [R238-ARCH-3] runInflight atomic.Pointer]
- [~] **R233B-GO-2 — cron Scheduler.Stop deadline.C 共享 timer（误报关闭 2026-05-23）**: 复核 scheduler.go:884-928，第一个 select 通过 deadlineHit=true 标志记录"timer 已 fired"；第二个 select 由 `if !deadlineHit` 外层 gate 守护，仅在 deadline.C 尚未 fired 时才进入，即第二个 select 内 `case <-deadline.C` 仍保持 active 而非 drained。R222-GO-10 + 这层 deadlineHit gate 已经覆盖了"timer 已耗 + triggerWG.Wait 不就绪"的边角——deadline.C 不会被双重消费，第二段不会永久阻塞。godoc 注释（line 879-915）已明示该不变量。归档关闭，本批 PR

#### 性能（P1/P2）

- [~] **R233B-PERF-1 — Protocol.ReadEvent 接受 string（归档 2026-05-23）**: R67-PERF-1 / R226-PERF-1 / R231-PERF-1 / R232-PERF-1 / R233-PERF-1 / R233B-PERF-1 跨 ~30 轮重申。Breaking 接口签名改造；统一跟踪 R231-PERF-1。本批 PR
- [ ] **R233B-PERF-4 — readLoop 每行做两次 json.Unmarshal（外层 shimMsg + 内层 ReadEvent）（P2）**: 第一次解 `{"type","line"}` 协议帧，第二次解嵌入 claude 事件。方案：shimMsg.Line 改 json.RawMessage 直传 ReadEvent；或外层手写字节扫描 type 分支。配合 R233-PERF-1 一次解决。Breaking：否。 → discarded [dup of #461 [CLI-PERF-1]]
#### 安全（P1/P2）

- [~] **R233B-SEC-1 — dashboard CSP 含 unsafe-inline script-src 与 style-src（归档 2026-05-23）**: 同 R231-SEC-2 / R233-SEC-1 一并归档，本批 PR
- [ ] **R233B-SEC-2 — Feishu VerificationToken-only 模式缺少 body HMAC（P1）**: token 泄露即可伪造任意事件体。方案：要么强制 EncryptKey；要么把"VerificationToken-only"明确标 deprecated 并加运行时启动告警。Breaking：是（运维侧）。 → discarded [dup of #382/#877 feishu token-only HMAC]
#### 架构（P1/P2）

- [~] **R233B-ARCH-1 — internal/cli 包成 9 合一 god package（P1）**: 63 文件 30+ 导出类型；其它 87 个文件靠它做"通用类型库"。方案：拆 cli/process / cli/event / cli/imaging / cli/transcript（protocol 已成形）；或抽 EventEntry/ImageData/AskQuestion 到 internal/clitypes。Breaking：是（import 路径）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R233B-ARCH-2 — *session.Router 80+ 方法 god struct（P1）**: 拆 6 文件但锁/字段共享；HubRouter/SessionRouter 多套 narrow interface 各取一片。方案：拆 SessionStore/Spawner/DiscoveryAdapter/ShimReconcileLoop/CleanupLoop 多 struct 组合在 Router 里。Breaking：否（聚合接口不变）。 → discarded [dup of #383 [ARCH2 Router god-object]]
- [~] **R233B-ARCH-3 — server.agent_tailer + wshub 直接持 *cli.SubagentLinker / *cli.TranscriptReader 指针（P1）**: server→cli 偷越层访问。方案：SubagentLinker 暴露收敛到 session.ManagedSession.SubscribeAgentEvents(taskID, fn) 回调式 API。Breaking：是（server tailer 改造）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R233B-ARCH-4 — sysession.auto_titler 直 import internal/cli 取 EventEntry（P1）**: daemon 层不应依赖 process 类型。方案：把 EventEntry 抽到 internal/eventlog/schema 或 internal/clitypes。Breaking：否（仅 import 整理）。 → discarded [dup of #626 [R217-ARCH-3] discovery imports cli for EventEntry (recurring multi-round)]
- [ ] **R233B-ARCH-5 — cli.HistorySource vs history.Source 名存实亡 interface（P2）**: 仅为防 import cycle 而存在的零适配 interface。方案：抽公共类型后 cli.HistorySource 删除。Breaking：是。 → cosmetic [new-B: history.Source unify]
- [ ] **R233B-ARCH-6 — 三处独立 LoadJSON/SaveJSON 但只 10 个调 osutil.AtomicWrite（P2）**: cron/store + session/store + 其它直接 os.WriteFile。方案：抽 internal/storage 包统一 LoadJSON/SaveJSONAtomic（含 corrupt rename + size cap + fsync）。Breaking：否。 → #1023 [recurring: sessions.meta.json atomic]
- [ ] **R233B-ARCH-7 — config 反向 import internal/session 拿 AgentOpts（P2）**: 让 config 失去叶子节点资格。方案：config 用独立 AgentConfig 类型，session 在构造时翻译。Breaking：否。 → cosmetic [new-B: config import session AgentOpts]
- [ ] **R233B-ARCH-8 — server 包 60+ 文件未分子包（P2）**: dashboard_*.go 已逻辑分组但物理同包。方案：拆 server/dashboard / server/ws 子包。Breaking：否。 → discarded [dup of #387 [ARCH1 server package]]
- [ ] **R233B-ARCH-9 — internal/upstream 把 discovery + cli.EventEntry 拉进 import 图（P2）**: upstream 应是纯传输层，但通过 SetDiscoverFunc/SetPreviewFunc 接收 cli 类型。方案：discover/preview JSON 构造移到 server，传 RPC handler map 给 upstream。Breaking：否。 → discarded [dup of #626 [R217-ARCH-3] discovery imports cli for EventEntry (recurring multi-round)]

#### 代码质量（P2/P3）

- [~] **R233B-CR-1 — recordResult 死代码双轨（归档 2026-05-23）**: 同根因 R230C-CR-1 已落地——recordResult 已删除（~85 行），persist_failure_test.go 改调 recordResultP0WithSanitised(j, result, errMsg, sessionID, errClass, state) 6 参数签名，全 race test 通过；R232-ARCH-2 / R220-GO-1 历史脚注（scheduler.go:2506）保留作为反向追踪锚点。归档关闭，本批 PR
- [~] **R233B-CR-6 — runRing.Snapshot() 无生产调用者（误报关闭 2026-05-23）**: 复核 internal/cron/ 全包扫描无 runRing 类型 / 无 runring_test.go 文件 / scheduler.go 中"per-job ring"是 runstore 的命名说法（newest-first slice 而非 ring buffer）。条目所指代码不存在，归档关闭，本批 PR
- [~] **R233B-CR-7 — skipAppendTrim 三条件无单测（误报关闭 2026-05-23）**: 复核 internal/cron/runstore_test.go:633+ 已经存在 dedicated table-driven test（包括 happy path + 三个边界条件：cold cache / not warm / appendsSinceTrim 达 batch / keepCount 接近 / oldest EndedAt 出 keepWindow），并测试 skipAppendTrim 对未知 jobID 的 fallback 返回 false (line 752)。条目所述缺失的测试实际已存在；归档关闭，本批 PR
- [~] **R233B-CR-8 — sysession.runOnce panic-recovery → CAS-release 无单测（误报关闭 2026-05-23）**: 复核 internal/sysession/manager_test.go:273 已有 `TestManager_PanicRecoveredAndInflightReset` 完整覆盖：(1) 注入 panicking Tick → 等待 inflight 复位 → 断言 false (line 317)；(2) 后续 tick pulse → 验证第二次 tick 真的能跑（CAS gate 不卡，line 327）。条目描述基于过时观察，归档关闭，本批 PR

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描发现 ~95 条。本轮直接修 7 处（cacheHeadPush O(N)→O(1) prepend / trimAll Start 改异步 goroutine / trimAll ReadDir 错误升级 Warn / sysession.runner stderr 预截 256 / cron previousTickBefore 1000 次迭代上限 / cron recordResult 4*1024 改 maxStoredResultRunes 常量 / cron slogPrintfLogger 同时匹配 panic|recovered + storeMu 注释从 saveJobs→saveMarshaledSeq + diskListNewestFirst & trimJobLocked 跳过 symlink）。
> 以下是需设计决策、破坏兼容、跨包重构、或方案不唯一不适合本轮直接修的条目。

### 架构（高优先）— 本轮新发现

- [~] **R232-ARCH-3 — cron 的 SessionRouter 接口仍声明 RegisterCronStub + RegisterCronStubWithChain（误报关闭 2026-05-23）**: 复核 cron/scheduler.go:72-90 SessionRouter 接口现仅声明 RegisterCronStubWithChain（line 82，无双方法），condition 已落地。session 包仍保留 RegisterCronStub 公开方法但仅给测试 / upstream test 用，移除会破坏 ~10 测试 — 留作独立测试重构。本批 PR
- [ ] **R232-ARCH-5 — 28 个 contract test 用 os.ReadFile + 字符串/正则 pin 源代码（P1）**: notify_background_ctx_test / debounce_contract_test / on_turn_done_contract_test 等把 gofmt + 注释 + 标识符当 API。方案：抽到行为级断言；真正必须 source-pin 的统一放 `internal/contract/` 加 README。Breaking：否（重构 test）。 → cosmetic [new-B: 28 contract tests source-pin]
- [ ] **R232-ARCH-6 — 5 个独立 *Router 消费者接口 + 2 个临时 cronStubChecker/cronSessionLister（P2）**: cron / dispatch / server.HubRouter / sysession.SystemSessionRouter / upstream.SessionRouter 重叠严重。方案：合并到 `internal/session/iface` 子包按 Lifecycle/Reader/Lookup 三细分接口。Breaking：否。 → cosmetic [new-B: HubRouter narrow]
- [ ] **R232-ARCH-8 — dispatch 直 import internal/cron 持 *cron.Scheduler（P2）**: dispatch 已有 SessionRouter 消费者接口模式，cron 这一边却走具体类型，policy 不一致。方案：定义 dispatch.CronScheduler interface 子集（AddJob/ListJobs/...）。Breaking：否。 → discarded [dup of #457 [ARCH-DISP-1 DispatcherConfig concrete]]
- [ ] **R232-ARCH-9 — cron 包直 import internal/platform 自承 channel adapter 职责（P2）**: cron 的 platforms map + ReplyWithRetry/SplitText/footer 与 dispatch 平行实现。方案：引入 dispatch.Notifier 接口注入 scheduler；cron 不再 import platform。Breaking：否（构造时 wiring 调整）。 → discarded [dup of #670 [R219-ARCH-8 cron.scheduler holds platforms map] (recurring multi-round)]
- [ ] **R232-ARCH-11 — NotifyPolicy 隐式三态（P2）**: cron Job.Notify *bool 三态 + Platforms+NotifyDefault+per-job target 4 条优先级容易翻车（IM 创建默认回源 chat / dashboard 创建默认 silent）。方案：改 enum NotifyPolicy 显式建模。Breaking：是（cron_jobs.json schema 迁移）。 → cosmetic [new-B: NotifyPolicy enum]
- [~] **R232-ARCH-12 — executeOpt 316 行单函数（多轮 NEEDS-DESIGN 归档 2026-05-23）**: R226-CR-10 / R229-CR-1 / R230-CQ-9 多轮重申。executeStep interface（preflight/spawn/send/finalize 4 步）拆解需把 stubRefresh closure / sendCtx / abortResult / watchdog 通道编织成 step 状态机，~600 行新结构 + 受影响 ~25 测试（jitter / fresh_shutdown / persist_failure / run_p0 / run_p1）。统一收敛到 R232-ARCH-1 god-file 拆分 RFC（lifecycle / jobs / execute / finish / persist / core 5–6 文件）跟踪。归档关闭，本批 PR
### Go 正确性 / 并发 — 本轮新发现

- [~] **R232-GO-1 — protocol_acp.go readUntilResponse 超时 goroutine 永久泄漏（归档 2026-05-23）**: 同 R224-GO-2 主跟踪。生产 shim path 已用 SetReadDeadline pulse 让 ReadBytes 立即返回 EOF；非 shim path 仅启动单元测试命中，注释已显式标记。本批 PR。
### 性能 — 本轮新发现

- [~] **R232-PERF-1 — protocol_acp parseSessionUpdate 每 token 双 Unmarshal（多轮 NEEDS-DESIGN 归档 2026-05-23）**: agent_message_chunk 分支已 typed-decode (ACPTextContent 2 字段)，热路径每帧 1 reflect 调用。彻底合并需 ACPUpdateDetail schema 改造（content 直接 inline ACPTextContent vs RawMessage）— Breaking 协议解析层。归档 NEEDS-DESIGN，本批 PR
- [~] **R232-PERF-4 — wshub.BroadcastSessionsUpdate AfterFunc 重复分配 timer（归档 2026-05-23）**: 实地复核：debounce 路径核心不变量是 `h.debounceTimer = nil` 由 AfterFunc callback 在 debounceMu 内清零（wshub.go:1424-1426），下一个 caller 据此判断是否要 `clientWG.Add(1)`。改为 NewTimer+Reset 复用模式要重写 callback ↔ caller 的 nil-flag 互锁机制（callback 不能 nil-out 共享 timer），与 Shutdown 的 `clientWG.Done()` paired-Stop 计数也要重排。debounce 50ms 窗口 + 500ms hard cap，AfterFunc 分配最高 ~20Hz，cold path 收益小风险大。"核心 broadcast 路径不动"约束下归档。本批 PR
- [~] **R232-PERF-6 — subagent_transcript map[string]any decode 每 block 多 alloc（误报关闭 2026-05-23）**: 同根因 R230B-PERF-4 已落地（mapAssistantLine + mapUserLine 已切到 typed transcriptAssistantBlock / transcriptUserBlock）。归档关闭，本批 PR
- [~] **R232-PERF-10 — cacheTrimAfterDisk EndedAt vs trimJobLocked mtime 时间源不一致（同根因 R230B-CR-4 已落地 2026-05-23）**: R230B-CR-4 已通过 godoc 锚点解决——cacheTrimAfterDisk godoc 加段落显式分析 mtime vs EndedAt 偏差窗口（典型 <10ms，pathological <1s），下一次 1Hz 拉取会 re-warm 抹平差异；统一到 mtime 需 250 syscall/s 或 +320KB cache，成本不划算，godoc-only resolution 锁定。归档关闭，本批 PR

### 安全 — 本轮新发现

- [ ] **R232-SEC-2 — 4 条 serve* 路径独立 TOCTOU（P2，扩展 R231-SEC-5）**: handleFileGet Lstat 后 serveRender/Preview/Raw/Download 各 open 一次。方案：handleFileGet 一次 OpenFile 拿 fd 传子函数；下游 Fstat 比 inode。Breaking：否。 → discarded [dup of #655 [R219-SEC-2] (recurring multi-round)]
- [ ] **R232-SEC-3 — feishu transport_hook 签名失败前 nonce 未入库（P2）**: HMAC 失败提前返回时 timestamp 窗口内可换 nonce 重放。方案：失败时也写 nonce 或先 nonce 去重再签名校验。Breaking：否。 → discarded [dup of #441 [H2 CSP unsafe-inline]]
- [ ] **R232-SEC-7 — JFIF+PDF 双容器绕过 PDF 检测以 KindImageInline 进入（P2）**: 方案：增二次魔数检测拒绝嵌套 PDF。Breaking：否。 → #1002 [new-A: PDF in-image detection]
### 代码质量 / 重构 — 本轮新发现

- [~] **R232-CR-3 — emitOverlapSkipped 发 back-to-back started→ended（设计意图归档 2026-05-23）**: 复核 scheduler.go:2429-2464 emitOverlapSkipped godoc 已明示设计意图——双事件发射故意为之，让 subscriber state machines 不丢失"started"锚点 (R233B-CR-2 引用)。状态字段 RunStateSkipped + ErrClassOverlapSkipped 让 dashboard 渲染为 no-op pill 而非 run timeline；synthetic RunID + StartedAt + skipPersist=true 避免污染 runs/<id>/。WS schema 加 Skipped bool 是 breaking 改造，且当前 ErrClass + State 已可推出该语义。归档关闭，本批 PR
- [~] **R232-CR-13 — dispatch unit test 走真 session.Router（归档 2026-05-23）**: 实地核查 `internal/dispatch/dispatch_test.go:126` `newTestDispatcher` 仍 `session.NewRouter(session.RouterConfig{MaxProcs: 10})`，全文件 50+ 处 `newTestDispatcher` 调用确认整体是集成测试，不是单元测试。方案需为 dispatch 引入 SessionRouter fake 接口，跨包重构面较大；P3 优先级 + 现有集成测试覆盖 send/queue/dedup 整链路有正面价值，作为长期改进项归档跟踪。
- [~] **R232-CR-14 — agent_tailer.attach 锁外逐条 SendJSON（NEEDS-DESIGN 归档 2026-05-23）**: 提议 `agent_history` 新 ServerMsg.Type 是 WS schema 加 type 即 breaking 客户端解析；attach 路径每 subscribe 1 次 cold path；既有 buffered 列表 ≤500 + SendJSON 已经异步入队 c.send chan 不阻塞 hub 锁。本批 PR 归档。
## Round 231 — 5-agent 并行 review 第 41 轮（2026-05-21）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描发现 ~80 条。本轮直接修 12 处（顶部摘要）。
> 以下是需设计决策、破坏兼容、跨包重构、或方案不唯一不适合本轮直接修的条目。

### 架构（高优先）— 本轮新发现

- [~] **R231-ARCH-1 — sysession.Runner 直 exec 旁路 CLI Wrapper 三层抽象（P1）**: `internal/sysession/runner.go` 自拼 `-p` argv、自 filterEnv、自 setting-sources，与 `cli.Wrapper.Spawn` 完全平行。新增 backend（Gemini ACP）必须在此再实现一遍。区别于 R230-ARCH-1（仅指出绕 backend.Profile），本条强调它把 CLI Wrapper 整层短路。方案：把 `RunOneShot` 抽进 `backend.Profile`，或让 Runner 走 `cli.Wrapper.Spawn(--collect-mode)`。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R231-ARCH-2 — 消息出口四套并行管线（P1，扩展 R230-ARCH-2/6）**: dispatch.sendAndReply / server.Hub.sendWithBroadcast / cron.scheduler.executeOpt / upstream.connector_rpc 四套 send 路径。cron 直持 platforms map、绕开 dispatch.replyText/queue/dedup；upstream 直调 sess.Send 绕开 MessageQueue/usermsg。Channel Adapter 不再是消息出口的唯一抽象。方案：抽 `internal/turnrunner` 或扩 `dispatch.Dispatcher` 为唯一 Send 协调器。 → discarded [dup of #459 [ARCH-SVR-1 send-with-broadcast]]
- [ ] **R231-ARCH-3 — session/router_core.go 顶部 blank-import history backend（P1）**: `claudejsonl/kirojsonl/naozhilog` 三个包的 init() 注入。注释自承"Sprint 1b 将合并到 wireup 包"。session 包想成为 backend-agnostic 的话必须迁出。方案：抽 `internal/wireup` 显式 `RegisterDefaults()` 由 `cmd/naozhi` 调用。Breaking：no（机械迁移）。 → discarded [dup of #458 [ARCH-SESS-1 history backend]]
- [ ] **R231-ARCH-4 — Router god-object（60+ 方法 / 24+ 字段）（P1）**: 单结构体覆盖 7 大职责，5 处消费方手工裁剪 Reader/Writer 接口已出现 NotifyIdle/SetUserLabelWithOrigin 不对称（R230-ARCH-3）。方案：facet 化 `Router.Lifecycle()` / `Backends()` / `Stubs()` / `Overrides()`，每 facet 对应稳定接口。 → discarded [dup of #383 [ARCH2 Router god-object]]
- [~] **R231-ARCH-5 — Hub 与 Router god-object 双胞胎共同导致 Channel Adapter 抽象塌陷（P1）**: server.Hub 退化为第二个 Router（同时持 router/scheduler/scratchPool/queue/dedup/uploadStore/auth/tailers/nodes）；webhook 进来后路径从"Adapter→Router→Wrapper"变成"Adapter→{Hub/Server/Dispatcher} 三方共享状态"。方案：抽 WSEventBus / nodeRegistry / SendCoordinator 子聚合。 *(部分实施：PR #327 已 6-stage split wshub.go 文件层级；Router 端 PR #309 同 6-stage split scheduler.go；但 struct-level 的 SendCoordinator / SubscriberRegistry / WSEventBus 子聚合仍未抽出 — 跟踪到 R248-ARCH-6。)*
- [~] **R231-ARCH-6 — server.Hub.wiredLinkers 直持 cli.SubagentLinker（P1）**: Channel Adapter / 上层 server 强耦合 cli 包内部领域类型；ACP 等"无 SubagentLinker 概念"的 backend 上线时整条 agent-team UI 链路要么硬编码空实现要么走 nil 分支。方案：在 session 或 internal/agentlink 包定义 AgentLinker / AgentIntrospector 接口。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-ARCH-7 — cli.Wrapper.ShimManager 公开可变字段（P1）**: ShimManager 本应是进程级 singleton；multi-backend 部署 router 持 `wrappers map[string]*cli.Wrapper`，每 Wrapper 一个 ShimManager 副本（R230-ARCH-13 / R219-ARCH-4）。方案：定义 cli.Transport interface，shim/direct-exec 各一实现；Wrapper 拆 immutable BackendProfile + 共享 Transport。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R231-ARCH-8 — cli.Protocol 接口过宽（P2）**: 9 方法含 stream-json 专属能力 WriteUserMessageLocked / WriteInterrupt / SupportsPriority / SupportsReplay。ACP 这些方法必然 noop 或返回 ErrInterruptUnsupported。方案：缩为核心 7 方法 + passthrough/interrupt 下沉为可选 PassthroughExt / InterruptExt（type-assert）。Breaking：是。 → discarded [dup of #428 [RNEW-ARCH-404 cli.Caps] (recurring multi-round)]
- [ ] **R231-ARCH-9 — workspaceOverrides 与 sessions.json 双 JSON 分离（P2）**: 独立 dirty bit / gen counter / atomic write，部分失败导致重启后 session 引用了不在 overrides.json 的 chat workspace，无 reconciliation 路径（R219-ARCH-9）。方案：合成单文件 atomic write 或启动期一致性扫描修复。Breaking：是（store schema migration）。 → discarded [dup of #673 [R219-ARCH-9] workspaceOverrides]
- [ ] **R231-ARCH-10 — backend.Profile 注册表承诺与 wrapper.go 硬编码 switch 落差（P2）**: `cli/wrapper.go` `backendDisplayName` / `detectCLI` 仍硬编码 switch on "kiro" / "claude"，DESIGN.md L280 承诺已部分兑现但未到位。方案：通过 `backend.LookupProfile(id).DisplayName/.DefaultBinary` 获取，删除硬编码 switch；测试加 contract 锁。 → discarded [dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)]
- [ ] **R231-ARCH-11 — NewRouter 构造期副作用阻碍可测性（P2）**: NewRouter ~360 行内 load knownIDs / load workspaceOverrides / load sessions.json / 启动 N goroutine 异步加载 history / runOrphanSweep / startAttachmentTracker。测试无法单独构造 router 而不触发磁盘 IO + goroutine（R230-CQ-10）。方案：拆 `NewRouter`（仅 init 字段）+ `Router.Start(ctx)`。Breaking：是（构造方需迁移）。 → discarded [dup of #383 [ARCH2 Router god-object]]

### 安全 — 本轮新发现

- [~] **R231-SEC-1 — sysession.Runner 直 exec 不走 shimEnvAllowedPrefixes 白名单（归档 2026-05-23）**: 与 R231-ARCH-1 同根因（一旦 Runner 走 cli.Wrapper.Spawn 自动继承 shimEnvAllowedPrefixes + capExtraArgsBytes）。归并到 R231-ARCH-1 跟踪，本批 PR
- [~] **R231-SEC-2 — Dashboard 主页面 CSP `script-src 'unsafe-inline'`（P1，R229-SEC-6/R230-SEC-1 重申未修）**: 主页面允许任意内联 script，若 workspace 文件被污染且触发 XSS sink 即可执行任意 JS 窃取 session cookie。方案：迁 nonce/hash 模式，将 dashboard.html 内联 onclick 等事件外移为外部 JS。Breaking：是（前端模板需要重构）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-3 — `allowed_root` 缺失时不阻断公网监听启动（P1，R229-SEC-3 重申未修）**: dashboard_token 非空且监听非 loopback 但 allowed_root 为空时仅 Warn，认证用户可设 cron work_dir=/etc 让 CLI 向系统目录写文件。方案：fatal 启动失败 + naozhi doctor 加 HIGH 级别检查。Breaking：是（已部署但未配置 allowed_root 的部署需要迁移）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-4 — ExtraArgs 无 flag 允许列表（P1，R219-SEC-1/R229-SEC-1 重申未修）**: `protocol_claude.go:77` `args = append(args, opts.ExtraArgs...)`，dashboard-authenticated 用户可注入 `--mcp-config` / `--add-dir` 等改变 CLI 行为。方案：BuildArgs 加 flag 允许列表。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-5 — `serveRender/servePreview/serveRaw/serveDownload` Lstat 后再次 os.Open 的 inode-swap TOCTOU（P1，R219-SEC-2/R229-SEC-2 重申未修）**: 每个 mode 独立的 os.Open 都是新窗口。方案：handleFileGet 使用 OpenFile 拿到 fd，下游直接消费 fd；或加 Fstat 验证 inode。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-6 — sessions.meta.json 非原子写（P2，R230-SEC-4 重申未修）**: `internal/session/store.go` 用单次 os.WriteFile 而非 osutil.WriteFileAtomic，部分写失败时半截 JSON 导致重启后 session 历史不可用。方案：改用 osutil.WriteFileAtomic。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-7 — XDG_ 前缀过宽放行（P2，R230-SEC-2 重申未修）**: shim/manager.go 放行 `XDG_*` 整族，理论上可重定向 CLI 配置/数据查找路径。方案：精确白名单 XDG_RUNTIME_DIR= / XDG_CACHE_HOME= / XDG_STATE_HOME=。Breaking：是（contract 测试需更新）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R231-SEC-8 — feishu webhook 仅靠 plaintext VerificationToken（P2）**: nonce 已强制；但 v1 仅 VerificationToken 模式下 token 是 plaintext shared secret，泄漏后 5 分钟 replay 窗口内自由重放。方案：强制要求配 EncryptKey 或将 token-only 标 deprecated 并 startup Warn 提升级别。 → discarded [dup of #441 [H2 CSP unsafe-inline]]
- [~] **R231-SEC-9 — 单 token 可建 500 个 WS（P2，R229-SEC-8 重申未修）**: maxConnectionsPerServer=500 但无 per-token/per-cookie-bucket 子上限。方案：WS 升级时按 cookie MAC 或 Bearer SHA-256 设 per-token 上限（如 20）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-10 — 反向 Node 连接通过 `ws://` 明文（P2，R229-SEC-5 重申未修）**: 部署环境未有 TLS 卸载代理则 token 中间人截获。方案：`/ws-node` handler 检查 r.TLS + 可信 X-Forwarded-Proto:https，无则拒绝 Upgrade，或显式豁免 + 文档。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R231-SEC-11 — `/static/dashboard.js` 不走 requireAuth（P2，R230-SEC-3 重申未修）**: 中间人可替换 JS 文件后客户端窃取 dashboard token。方案：dashboard_token 非空时对静态 JS 端点加 requireAuth。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
### 性能 — 本轮新发现

- [~] **R231-PERF-1 — Protocol.ReadEvent string→[]byte 反向拷贝（多轮 NEEDS-DESIGN 归档 2026-05-23）**: R67-PERF-1 起跨 ~30 轮 review 重申，方案 Breaking 接口签名改造。50 sess × 50 evt/s 量级是已知开销但非紧急。同根因 R232-PERF-1 / R233-PERF-1 / R233B-PERF-1 / R67-PERF-1 / R226-PERF-1 一并归档 NEEDS-DESIGN，本批 PR
- [ ] **R231-PERF-2 — ACP parseSessionUpdate 双 Unmarshal（P1）**: agent_message_chunk 分支每帧两次 json.Unmarshal，kiro 每 token 一帧最热路径。方案：bytes.Contains 快速判断或合并为一次解析。 → #1020 [recurring: ACP parseSessionUpdate]
- [~] **R231-PERF-4 — ACPProtocol.textBuf 锁竞争（误报关闭 2026-05-23）**: 实地复核 protocol_acp.go textBuf 是 ACP 单 reader（readLoop）路径串行写入 + WriteMessage turn boundary Reset 的设计；mu 仅覆盖 textBuf 本身，sessionID 已分离到 atomic.Pointer 后 mu 的争用窗口被收窄。lock-free 改造需重设计 acp turn boundary 协议，不在简单修范围。归档关闭，本批 PR。
- [~] **R231-PERF-6 — BroadcastSessionsUpdate AfterFunc 创建新 timer（NEEDS-DESIGN 归档 2026-05-23）**: wshub.go:1430-1431 已有 timer.Stop()+Reset 复用路径覆盖密集分支；AfterFunc 仅在 quiet→active 转换时分配新 timer——稀疏路径。预分配 NewTimer 需重 Shutdown 协议（drain timer + clientWG.Done 时序），改造成本远超 alloc 收益。本批 PR 归档。
- [ ] **R231-PERF-7 — ACP readUntilResponse 每握手 3 次 goroutine + 3 chan alloc（P2）**: 握手 3 次 = 9 次。方案：握手 goroutine 提升为长寿命，仅在握手阶段循环；或 done chan→atomic.Bool。
- [ ] **R231-PERF-8 — Cleanup 在 r.mu 内做整 sessions map copy（P2）**: O(N) 拉长持锁时间。方案需保持 saveStore 的稳定 snapshot 语义（不能拆 keys → 释放锁 → 再 RLock 因为竞态），或者转为 RCU/COW snapshot。需独立设计。 → discarded [dup of #411 [R214-PERF-2 Snapshot alloc]]
### 代码质量 — 本轮新发现

- [ ] **R231-CQ-1 — claude reconnect 路径双注入（P1，PR #202 复盘）**: `router_shim.go:439` 直接 `proc.InjectHistory(histEntries)`，随后 `ReattachProcessNoCallback` 调 `attachProcessAndSnapshotPersisted` 把 `sess.persistedHistory`（已由 tier1/tier2 异步 goroutine 通过 `sess.InjectHistory` 填充）snapshot 再次注入同一 proc。两批高度重叠时 EventLog 翻倍。方案：line 439 改为 `sess.InjectHistory(histEntries)` 走 persistedHistory + seededLen 流向，与 kiro 路径行为一致。需对照 #202 PR 测试与 EventLog 去重逻辑验证。 → #1005 [new-A: claude reconnect double-inject EventLog]
### Go 正确性 — 本轮新发现

## Round 230 — 5-agent 并行 code review（2026-05-21）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）跑了全仓 review。本轮 12 处直接修已落 PR；以下为 breaking / 跨模块 / 需设计决策的发现登记追踪。

### Architecture（架构债）

- [~] **R230-ARCH-1 — sysession.Runner 直 exec `claude -p` 旁路 cli.Wrapper（P1）**: `internal/sysession/runner.go` 每次 Run 起新 claude -p 子进程，绕过 backend 选择 / shimEnvAllowedPrefixes / `--setting-sources ""` / ARG_MAX 守卫。新增 backend (Gemini) 需在此重做一遍。方案：`backend.Profile.RunOneShot(ctx, prompt) (string, error)` 抽接口由 Runner 复用。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230-ARCH-2 — Hub.ownerLoop / runTurn 与 dispatch.Dispatcher.ownerLoop / sendAndReply 几乎逐行重复（P1）**: `internal/server/send.go` 与 `internal/dispatch/dispatch.go` 各一份 collect-timer + drain + Discard/recover 实现，"对齐 dispatch.ownerLoop" 注释自承。方案：抽 `TurnRunner` 或把 dashboard 走 LocalChannel 适配器纳入 dispatch。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230-ARCH-3 — 五个 SessionRouter 子集接口手工对齐易漂移（P1）**: dispatch / cron / sysession / server.HubRouter / upstream 各持一份，已出现 NotifyIdle / SetUserLabelWithOrigin 不对称。方案：在 session 包合成 `Reader/Writer/Lifecycle` 三联，消费方按需 compose。Breaking：否（接口断言迁移）。 → discarded [dup of #457 [ARCH-DISP-1 DispatcherConfig concrete]]
- [ ] **R230-ARCH-4 — session.Router 60+ 方法跨 7 大职责（P1）**: 每消费方都能触达全量。方案：facet 化 `Router.Backends() / Lifecycle() / Stubs()`。Breaking：否（增量 facet）。 → discarded [dup of #383 [ARCH2 Router god-object]]
- [ ] **R230-ARCH-5 — server.Hub 45 方法 24 字段第二 Router（P2）**: 同时持 router / scheduler / scratchPool / queue / dedup / uploadStore / auth / tailers / nodes，nodesMu 与 Server 共指针为耦合 smell。方案：抽 WSEventBus / SendCoordinator / AgentTailerSet / nodeRegistry。Breaking：是。 → discarded [dup of #376 [R248-ARCH-6 Hub god]]
- [~] **R230-ARCH-6 — upstream 反向 RPC 第三套 send 管线（P1）**: `connector_rpc.go` 直 `sess.Send`，绕过 MessageQueue / dedup / usermsg / replyError 计数，反向流量在监控里不可见。方案：让 upstream 走共享 TurnRunner / Dispatcher.Send。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230-ARCH-7 — 错误→用户消息映射有 3 处偏序（P2）**: `usermsg.ForSendError` 是规范源，但 dispatch.sendAndReply 仍内联 ErrMaxProcs / ErrMaxExemptSessions / 超时分支，apierr.localizeAPIError 是第四套。方案：超时参数注入 usermsg 或 dispatch 侧 helper，dispatch 内联 switch 收敛。Breaking：否。 → cosmetic [new-B: errmap registry]
- [ ] **R230-ARCH-9 — KeyResolver 三处独立实例（P2）**: cmd/naozhi/main.go upstreamResolver / Server.resolver / Dispatcher fallback 三份缓存，project 变更后异步漂移。方案：projectMgr 暴露 Resolver() 单点。Breaking：是（构造签名）。 → discarded [dup of #604 [R237-ARCH-12] KeyResolver shared]
- [ ] **R230-ARCH-10 — plannerKey* 在 session/key.go 与 project 包重复（P2）**: 注释自承"hardcoded test assertion synced"。方案：抽 `internal/keys` 中性包，两侧 import。Breaking：否。 → cosmetic [new-B: plannerKey shared]
- [ ] **R230-ARCH-11 — dashboard.js PLATFORM_ORIGINS 硬编 IM 列表（P2）**: 新增平台需 4 处同步：adapter / main.go initPlatforms / dashboard.js / dashboard.html CSS。方案：`GET /api/platforms` 返回 `{id, displayName}[]`，前端启动时 hydrate。Breaking：是（前端模板）。 → #1021 [recurring: /api/platforms hydrate]
- [~] **R230-ARCH-12 — Dispatcher 实例只在 main.go 串到 Feishu，Hub 仅借 MessageQueue 引用（归档 2026-05-23）**: 实地核查 `internal/server/server.go:778` `dispatch.NewDispatcher` 实际在 server 包构造（不在 main.go），条目原描述位置不准。但根问题"dashboard send 与 IM send 两套抽象层"与 R230-ARCH-2 / R230-ARCH-6 / R231-ARCH-2 同根因（消息出口管线分裂），统一收敛到主条目 R231-ARCH-2 跟踪。
- [ ] **R230-ARCH-14 — internal/session/router_core.go 用 blank import 注册 history factory（P2）**: Sprint 1b 注释自承"将合并到 wireup 包"。任何 Router 测试都触发全局 registry 改动。方案：`internal/wireup/wireup.go` + 显式 RegisterDefaults() 在 main.go 调。Breaking：否（迁移）。 → discarded [dup of #458 [ARCH-SESS-1 history backend]]
- [ ] **R230-ARCH-15 — internal/server 90+ Go 文件单包（P3）**: 按 handler-group struct 切分而非 Go package 边界。方案：下次触动 auth/cron/scratch handlers 时迁子包。Breaking：是（import path）。 → discarded [dup of #387 [ARCH1 server package]]
- [ ] **R230-ARCH-16 — Router.spawnSession 直接调 *cli.Wrapper.Spawn（P3）**: cli.Wrapper 是 struct 非 interface，使 Router 单元测试需 panicSafeSpawn 替身。方案：`cli.Spawner interface { Spawn(ctx, opts) (Process, error) }`。Breaking：否。 → cosmetic [new-B: cli.Spawner interface]
- [ ] **R230-ARCH-17 — internal/cli 65+ 文件混 protocol/process/eventlog（P3）**: 同 R230-ARCH-15 同性质。方案：subpackage 化 cli/protocol cli/eventlog cli/subagent。Breaking：是。 → discarded [dup of #387 [ARCH1 server package]]
- [ ] **R230-ARCH-18 — Router.StartCleanupLoop / StartShimReconcileLoop 由 main.go 各起 goroutine（P2）**: Router 既不是完整 Run 服务也不是纯被动 struct，未来调用方易漏 Tick。方案：合并 `Router.Run(ctx)` 一次启动所有循环。Breaking：是。 → discarded [dup of #699 [R222-GO-1 sendCtx Background] (recurring multi-round)]
- [ ] **R230-ARCH-19 — Cron / Sysession / dashboard 各自一套 stub session 注册策略（P3）**: RegisterCronStub / RegisterCronStubWithChain / RegisterSystemStub 三方法 + exemptKeyPrefixes 链式 HasPrefix。方案：`StubKind` enum + 单一 RegisterStub。Breaking：是。 → cosmetic [new-B: StubKind enum]
- [ ] **R230-ARCH-20 — Server ↔ Hub 共享 nodesMu *sync.RWMutex 别名（P2）**: 表明 nodes 应独立 owner（`*node.Registry`）而非任一方持。方案：抽 nodeRegistry，Server 与 Hub 按 interface 消费。Breaking：是（构造）。 → cosmetic [new-B: nodeRegistry]

### Code quality（剩余）

- [ ] **R230-CQ-2 — sendAndReply 241 行 5+ 职责（P2）**: 内含 5s 超时 nested defer，复杂度高。方案：抽 buildReplyContext / handleSendResult。Breaking：否。 → discarded [dup of #656 [R219-CR-7] sendAndReply split]
- [~] **R230-CQ-4 — processIface.GetState/GetSessionID 命名违 Go 风格（P2 R219-CR-9 重申）**: 12 处调用点 + 两个 fakes 需机械重命名。方案：State()/SessionID()。Breaking：是（interface 改名）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230-CQ-6 — validateCronTitle 单独实现 UTF-8 + C0 + IsLogInjectionRune（P2）（评估关闭 2026-05-23）**: 与 validateStringField 三重扫描重复。方案：stringFieldPolicy 加 singleLineError bool。Breaking：否。 — 评估：dashboard_cron.go:364-368 已有显式 godoc 解释**为什么不接入** `validateStringField`：把单行专用错误消息分支（"title must be a single line" vs "contains invalid control characters"）和 rune-级 vs byte-级长度计量混入 stringFieldPolicy 会反向把 4 个 cron 验证器都污染成"如果支持单行就额外提示"的样板。已显式决策不做，本批 PR 关闭归档。
- [~] **R230-CQ-8 — reconnectShims case 内 90+ 行内联（P2 R229-CR-3 重申）**: 仍未抽 processDiscoveredShim 子函数。Breaking：否。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230-CQ-9 — cron.executeOpt 329 行 7+ 错误分支（P2 R229-CR-1 重申）**: handleSendError / deliverAndRecord 抽取。Breaking：否。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230-CQ-10 — NewRouter 359 行内联三阶段初始化（P2 R229-CR-2 重申）**: newRouterRestoreSessions / newRouterStartHistoryLoads 抽取。Breaking：否。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230-CQ-14 — cron/scheduler.go 2745 行单文件无拆分计划（P3 R226-CR-11 重申）**: 建议先建 scheduler_job.go / scheduler_run.go / scheduler_notify.go 骨架。Breaking：否。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
### Security（剩余 — 本轮新发现）

- [~] **R230-SEC-1 — Dashboard CSP `script-src 'unsafe-inline'`（P1 R229-SEC-6 重申）**: 主页面 CSP 仍允许任意 inline；登录页已用 SHA-256 hash 严格 CSP。方案：迁 nonce 模式，把 dashboard.html 内联 onclick 等事件外移；KaTeX/mermaid 通过 createElement+SRI 注入已不依赖 unsafe-inline。Breaking：是（前端较大改造）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230-SEC-2 — shim XDG_ env 前缀过宽（P2）**: `shim/manager.go:900` `XDG_` 前缀放行 XDG_CONFIG_HOME / XDG_CONFIG_DIRS / XDG_DATA_DIRS，理论上可重定向 CLI 配置/数据查找。当前测试契约（manager_test.go:517）显式允许 XDG_CONFIG_HOME，调整需同步重写测试。方案：精确 `XDG_RUNTIME_DIR=` `XDG_CACHE_HOME=` `XDG_STATE_HOME=`，剔除 CONFIG/DATA。Breaking：是（测试契约 + 部署期依赖 XDG_CONFIG_HOME 转发的运维方）。 → discarded [dup of #724 [R222-ARCH-9 env probing scattered] (recurring multi-round)]
- [ ] **R230-SEC-3 — Dashboard JS 静态资源未鉴权（P2）**: `/static/dashboard.js` 与 `/static/agent_view.js` 不走 requireAuth，HTTP 部署下中间人可替换。方案：dashboardToken 非空时对 JS 端点也加 requireAuth；或文档化 TLS 必备。Breaking：否（鉴权后浏览器需先认证才能加载，与 SPA 流程一致）。 → #1024 [recurring: static-JS auth]
### Go / Concurrency（剩余 — 本轮新发现）

### Performance（剩余 — 本轮新发现）

## Round 230B — 5-agent 并行 code review（PR #198）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）跑了全仓 review。本轮 17 处直接修已落本轮 PR；以下为非 breaking 但需要更大改动 / 需设计决策 / 跨模块的发现，登记追踪。

### Security（剩余）

- [ ] **R230B-SEC-2 — Backend ID charset 三处不一致（P2）**: `dashboard_cron.go:198` (`[a-z0-9_-]`) vs `select_node_for_backend.go:46` WS (`[a-zA-Z0-9_.-]`) vs `send.go:266` HTTP (`[a-z0-9_-]`)。本轮已收敛 maxCronBackendLen → maxBackendIDLen 但 charset 策略未统一。方案：决定是否允许大写 + `.`，统一到包级 `isValidBackendID` 一处。Breaking：是（如现有 backend ID 含大写或 `.`）。 → #1003 [new-A: backend ID charset unify]
- [ ] **R230B-SEC-3 — `cli.backends[*].args` 缺 flag 允许列表（P3）**: `validateArgvStrings` 已拒控制字节但允许任意 `--flag`。方案：与 R229-SEC-1 同批引入 flag allowlist。Breaking：是。 → discarded [dup of #653 [R219-SEC-1] (recurring multi-round)]
- [ ] **R230B-SEC-5 — dashboard CSP `img-src data:` 防御缺口（P3）**: 数据 URI 允许 SVG-with-script + 外发 GET 探针。方案：移除 `data:`；审计 dashboard.js 改用 blob URL。Breaking：是。 → discarded [dup of #441 [H2 CSP unsafe-inline]]

### Go 正确性 / 并发（剩余）

- [ ] **R230B-GO-2 — `subagent_link.Resolve` retry sleep 不响应 ctx 取消（P2）**: `subagent_link.go:294/332` 重试循环 `time.Sleep` 不察 ctx；router_shim.go:398 `go linker.Resolve` bare goroutine。方案：Resolve 加 ctx 参数 + select stop signal。Breaking：是（接口签名 + 调用方）。 → discarded [dup of #644 [R218B-GO-3 readLoop linker.Resolve goroutine no ctx] (recurring multi-round)]
- [ ] **R230B-GO-3 — `recordResultP0WithSanitised` / `recordResult` mu Unlock 非 deferred（P2）**: 多个 early-return 各自手动 Unlock，未来插早返路径易遗漏。方案：拆 stateMutate（持锁）+ stateCommit（锁外 save/fn 调用）两阶段。Breaking：否（内部重构）。 → cosmetic [new-B: recordResult deferred Unlock]
- [~] **R230B-GO-4 — `Hub.handleSubscribe` O(N) maxSubscribersPerKey 扫描（归档 2026-05-23）**: 已部分修复——R230C-PERF-4 引入 early termination 当 count 到达 maxSubscribersPerKey 即 break，worst-case O(20) 而非 O(maxWSConns=500)；handleSubscribe 是冷路径不在每事件扇出。维护单独 counter map 需在 disconnect 路径 +/- 引入第二个不变量，收益小风险大。本批 PR 归档。
### Performance（剩余）

- [~] **R230B-PERF-1 — `wshub.eventPushLoop` 同 session N 个 tab 各自 marshalPooled（P1）**: `wshub.go:1099` 同批事件 N tab 时 marshal 成本 O(N)。方案：marshal once → SendRaw 字节 fan-out。Breaking：否。R219-PERF-1 / R225-PERF-9 重申。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230B-PERF-2 — `Snapshot` `proc.TurnAgents()` 始终 alloc（P2）**: count==0 已短路，count>0 时仍 make+copy。方案：`TurnAgentsBuf(dst)` 接受 caller slice 复用，或 SessionSnapshot 内嵌固定 4-元数组。Breaking：否。R225-PERF-6 重申。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230B-PERF-3 — `ListSessions` SessionSnapshot slice 1Hz 持续分配（P2）**: 50 sessions × 1 Hz × N tab。方案：handleList 加 storeGen 缓存或 sync.Pool 池化结果。Breaking：否。R229-PERF-10 重申。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230B-PERF-5 — `subagent_transcript.readLocked` 每次 open+seek+ReadAll（P2）**: 50 tailer × 1s = 50 syscall/s。方案：保持 fd open + offset 增量；inotify 选项后续讨论。Breaking：否。 — 归档 2026-05-23 (superseded by R233-PERF-4)：R232-PERF-3 已加 readBuf 复用消除 ReadAll 增长复制；剩余 open/close-per-poll 由 R233-PERF-4 持久化 fd + ReadAt + inode 失效统一规划，本条不再独立跟踪；本批 subagent_transcript.go 加 godoc 锚点
- [ ] **R230B-PERF-6 — `eventlog_bridge` 单条快路径仍 copy raw bytes（P2）**: bridge 即使 single entry 仍 make+copy。方案：核对 Persister 留持契约，能 zero-copy 则免拷。Breaking：否（需仔细审 contract）。 → cosmetic [new-B: eventlog_bridge zero-copy]
- [~] **R230B-PERF-8 — `notifySubscribers` map iteration vs slice（P3）**: subCount==1 极常见，map range 不必要。方案：count==1 fast path 直接取 + count<=4 时 slice 存储。Breaking：否。 — 评估归档 2026-05-23：Go runtime mapiterinit+mapiternext on 1-bucket map ~tens of ns，RLock/RUnlock 是 either way 都付的成本主导项；slice 存储破坏 Subscribe/Unsubscribe + closeOnce 契约，net gain sub-percent，不划算。eventlog.go notifySubscribers godoc 锚点说明决策；若未来 5000+ session dashboard 重连成为热点，方向是 ring buffer 替换 map 而非 micro-branch，本批 PR

### Code 质量（剩余）

### Architecture（剩余 P1，需设计）

- [~] **R230B-ARCH-1 — `session` 包硬编码 import 4 个 backend-specific history 包（P1）**: `router_core.go:18-32` blank-import claudejsonl/kirojsonl 触发 init 注册，session 是协议无关层假设破裂。方案：cli.Wrapper.NewHistorySource 工厂封装；session 仅依 history.Source 接口。Breaking：是（~20 callsite）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230B-ARCH-2 — Hub/Server 半构造对象反模式（P1）**: 8+ 处 `if h.scheduler != nil` 守卫散落；Set* setter 注入顺序硬编码。方案：HubOptions 一次性装配 + null-object fallback。Breaking：否（内部重排）。立即可落地（~30 行）。 → discarded [dup of dup of #431 [R176-ARCH-M3 Hub HubOptions one-shot] (recurring multi-round)]
- [ ] **R230B-ARCH-3 — `cron.scheduler` 越层直接持 platform map + SplitText（P1）**: 与 dispatch 平行第二条 IM 出站路径，错误处理重复。方案：注入 Notifier interface。Breaking：否（cron 内部 + 兼容 fallback）。 → discarded [dup of #670 [R219-ARCH-8 cron.scheduler holds platforms map] (recurring multi-round)]
- [~] **R230B-ARCH-4 — `cli.Protocol` 接口塌陷为 stream-json 专属（P1）**: 8 方法中 4 个 ACP 必 noop/panic。方案：拆核心 Protocol + PassthroughExt 可选接口。Breaking：是（cli.Protocol 导出 + 6 处 type-assert）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230B-ARCH-5 — `cli.EventEntry` 已塌陷为跨层 DTO（P1）**: 26+ 包 import cli.EventEntry。方案：迁到叶子包 internal/event 零依赖。Breaking：是（接口签名）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230B-ARCH-6 — `sessions.json` + `workspace-overrides.json` 双 atomic write 不一致（P1）**: 部分失败下重启出现孤立 override。方案：合并 schema 单文件原子写或启动期一致性扫描。Breaking：是（schema migration）。立即可落：启动期扫描 ~30 行。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230B-ARCH-7 — `server` 包 backend fan-out 失控（P1）**: server.go 单文件 import 10 个 internal 包，是"god 包"伪装。方案：抽 internal/app 装配包，server 还原为纯 HTTP handler。Breaking：否（内部）。 → cosmetic [new-B: events.Bus]
- [~] **R230B-ARCH-8 — `cli.Wrapper` 持 `*shim.Manager` 单例假设破裂（P1）**: multi-backend 部署每 Wrapper 一份 ShimManager 副本可能撞同一系统资源。方案：BackendProfile + 共享 ShimManager 注入。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230B-ARCH-9 — Hub 持 `*cli.SubagentLinker` 内部对象指针（P1）**: 跨包内部对象暴露。方案：定义 AgentIntrospector interface。立即可落地（~50 行）。Breaking：否。 → discarded [dup of #376 [R248-ARCH-6 Hub god]]
- [ ] **R230B-ARCH-10 — server / dispatch / cron 三处持 `map[string]platform.Platform`（P1）**: 每加新平台改 3 处。方案：抽 `platform.Registry` 类型聚合 nil 守卫 + fallback 文案。Breaking：否。立即可落地（~80 行）。 → cosmetic [new-B: platform.Registry unify]
- [ ] **R230B-ARCH-11 — `cli` 包内 7 子领域并存（P2）**: ShimManager / SubagentLinker / EventLog / Wrapper / Protocol / passthrough / Process。方案：拆 cli/{process,protocol,eventlog,passthrough,subagent} 子包。Breaking：否。 → discarded [dup of #375 [R248-ARCH-4 AgentLinker]]
- [ ] **R230B-ARCH-12 — `ScratchPool` 应是 server 关注（P2）**: 实现在 session 包但唯一 caller 在 server。方案：搬到 server 或让 session.Router 自管 ephemeral 池。Breaking：是。 → discarded [dup of #720 [R242-ARCH-2 exempt session pool] + #432 [R176-ARCH-N4 ManagedSession state machine] (recurring multi-round)]
- [ ] **R230B-ARCH-13 — `discovery.DefaultScanner` package singleton（P2）**: 阻碍多租户隔离。方案：RouterConfig.HistoryScanner 字段 + nil fallback。立即可落地（~20 行）。Breaking：否。 → discarded [dup of #439 [R172-ARCH-D9 discovery scanner]]
- [ ] **R230B-ARCH-14 — `cron.notifyTarget` 错错误未走 usermsg 中文映射（P2）**: 仅 dispatch 和 server.send 用 usermsg.ForSendError；cron 出错只 slog.Warn 不回写 chat。方案：扩展 usermsg.ForCronError 或 dispatch 收回错误映射独占。Breaking：否。 → discarded [dup of #670 [R219-ARCH-8 cron.scheduler holds platforms map] (recurring multi-round)]
- [ ] **R230B-ARCH-15 — `processIface` 30+ 方法 god interface（P2）**: 方案：拆 ProcessLifecycle / EventSource / ProcessSender。Breaking：是。 → discarded [dup of #430 [R176-ARCH-M2 processIface]]
- [ ] **R230B-ARCH-16 — Dashboard 装配顺序硬编码多处（P2）**: dashboard.go SetXxx 顺序 + cmd/naozhi/main.go 各自构造。方案：抽 internal/app/wire.go 单点拓扑。Breaking：否。立即可落地。 → cosmetic [new-B: dashboard wiring central]
- [ ] **R230B-ARCH-18 — `--dangerously-skip-permissions` hardcode 在 Protocol（P2）**: 多用户/多 chat 无法 per-session 切权限。方案：SpawnOptions.PermissionMode 枚举。立即可落地（~30 行）。Breaking：否。 → discarded [dup of #531 [R215-SEC-P1-1] skip-permissions hardcode]
- [ ] **R230B-ARCH-19 — validateStringField 三重扫描（UTF-8+C0+Bidi）重复（P2）**: cron 路径已抽 helper，feishu/project planner 仍各自重复。方案：textutil.ValidateText(s, policy) 统一。Breaking：否。 → cosmetic [new-B: validateText unify]
- [ ] **R230B-ARCH-20 — node 反向 RPC 协议三处定义（P2）**: node/protocol.go / connector / wshub 各自手写编解码。方案：node/rpcprotocol 子包统一。Breaking：否。 → cosmetic [new-B: node rpcprotocol unify]
- [ ] **R230B-ARCH-23 — selfupdate 无回滚 / 健康检查 hook（P3）**: panic 后只能 ssh 手动回退。方案：systemd 启动 30s 内 self-call /health 失败自动 .prev 回退。Breaking：否。 → cosmetic [new-B: selfupdate rollback]
- [ ] **R230B-ARCH-24 — Server struct 30+ 字段 god object（P3）**: 已抽 *Handlers struct 但仍持每个指针。方案：mountAuth(mux)/mountCron(mux) 子构造，Server 退成 Listener+middleware+Mux 容器。Breaking：否。 → discarded [dup of dup of #387 (recurring multi-round)]
- [~] **R230B-ARCH-25 — EventLog SetPersistSink 时序契约靠 metric 兜底（误报关闭 2026-05-23）**: 复核 cli/eventlog.go:309-330 现状已通过 godoc + sinkReady atomic.Bool 双 stage 完整说明协议：sinkReady 初始 false，所有 Append/AppendBatch 在 false 期间标 replayPhase=true，Persister 主动 drop。SetPersistSink 同时 Store sink 指针 + sinkReady=true。godoc 已显式 "SetPersistSink must run AFTER InjectHistory" 双向校验。条目描述过时；归档关闭，本批 PR
- [ ] **R230B-ARCH-26 — feishu.go 1000+ 行 + 各平台拆分粒度不一致（P3）**: 方案：每平台拆 4 文件 transport/wire/outbound/capability。Breaking：否。 → discarded [dup of #409 [R214-ARCH-13 feishu.go split]]
## Round 229 — 5-agent 并行 code review（2026-05-20）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）跑了全仓 review。14 处直接修已落本轮 PR；以下条目为非 breaking 但需要更大改动 / 需设计决策 / 跨模块的发现，登记追踪。

### Security（剩余）

- [ ] **R229-SEC-1 — ExtraArgs flag 注入未受限（P1）**: `protocol_claude.go:77` / `protocol_acp.go:158` 直接拼接 `opts.ExtraArgs` 到 argv，无 flag 允许列表。受信认证用户可注入 `--mcp-config` / `--add-dir /etc` / `--skip-permissions` 等危险 flag。方案：BackendProfile 声明 flag allowlist；任何以 `--` 开头且不在白名单的 ExtraArgs 元素拒绝并 Warn。Breaking：是（依赖任意 ExtraArgs 的运维方需迁移到允许列表）。 → discarded [dup of #653 [R219-SEC-1] (recurring multi-round)]
- [ ] **R229-SEC-2 — serveRender TOCTOU inode-swap（P1）**: `project_files.go:683` 在 `os.Lstat(resolved)` 后再 `serveRender → os.Open(resolved)`，攻击者可通过 Claude Write 工具在窗口内创建符号链接指向 `/etc/passwd`。方案：`handleFileGet` 直接 OpenFile 拿 fd 传入 serveRender，或 Open 后 Fstat 比对 inode。Breaking：否（内部重构）。 → discarded [dup of #655 [R219-SEC-2] (recurring multi-round)]
- [ ] **R229-SEC-3 — allowed_root 缺失不阻断启动（P1）**: `server.go:513` 公网监听 + dashboard_token 配置时，`allowed_root` 为空只 Warn 启动。认证用户可设 cron `work_dir=/etc` 让 CLI 写系统文件。方案：dashboard_token 非空 + 监听非纯 loopback 时 fatal 启动失败 + naozhi doctor HIGH 级别检查。Breaking：是（现有公网部署需补 allowed_root）。 → discarded [dup of #658 [R237-SEC-9] (recurring multi-round)]
- [ ] **R229-SEC-5 — ws:// node 连接明文传输 token（P2）**: `reverseserver.go:150` 反向 node 第一条消息明文携带 token。方案：`/ws-node` handler 在 r.TLS==nil 且无可信 X-Forwarded-Proto: https 时拒绝 Upgrade，或加 insecure_node 显式豁免 flag。Breaking：是。 → #1026 [recurring: ws-node TLS]
- [ ] **R229-SEC-8 — per-token WS 连接数无上限（P2）**: `wshub.go:307` 单 token 持有者可建 500 个 WS 连接绕过 maxSubscribersPerKey=20。方案：WS 升级时按 cookie MAC 或 IP 桶检查 per-token 连接 cap（如 20）。Breaking：否。 → #1022 [recurring: per-token WS cap]
- [ ] **R229-SEC-12 — CDN allowlist 与 SRI 配合不足（P3）**: dashboard CSP `script-src` 含 `https://cdn.jsdelivr.net`，SRI 失败时 CSP 仍允许加载。方案：迁 nonce 模式后从 script-src 移除 CDN 域名。Breaking：是（与 R229-SEC-6 合并）。 → discarded [dup of #441 [H2 CSP unsafe-inline]]

### Go 正确性 / 并发（剩余）

- [~] **R229-GO-1 — ReattachProcessNoCallback 清 deathReason 与 mapSendError Store 存在 logical race（已文档化 2026-05-23）**: managed.go:477-487 ReattachProcessNoCallback godoc 已显式声明 SAFETY CONSTRAINT："this function must only be called when Send() cannot be in flight for this session"。调用者契约已明文锁定，本批 PR 归档。
- [~] **R229-GO-2 — Snapshot() 包含读侧写副作用（已文档化 2026-05-23）**: managed.go Snapshot godoc + R226-CR-13 内联注释已标注为有意决策；与 R230C-CR-Diag、R226-CR-13 主跟踪。SnapshotReadOnly 拆分需 RFC（spawnSession 显式 mirror 比 pull-based 镜像更脆弱）。本批 PR 归档为已文档化。
- [~] **R229-GO-5 — InjectHistory lastPrompt 仅"为空才设"（误报关闭 2026-05-23）**: 实地复核 managed.go InjectHistory 已有 `if loadAtomicString(&s.lastPrompt) == ""` 守卫，仅在为空时才 Store；若 Send 已先写入非空值，InjectHistory 直接跳过 — 与 TODO 担忧的方向相反（不会用 stale 替换 fresh）。godoc 已显式说明 "benign TOCTOU"。归档关闭，本批 PR。

### Performance（剩余）

- [ ] **R229-PERF-1 — Protocol.ReadEvent string→[]byte 双 copy（P1）**: 每个 stream 事件分配 1 个 []byte（line size，50 B–200 KB）。方案：Protocol.ReadEvent 签名改 []byte，shimMsg.Line 改 json.RawMessage 同步消除中间 string 拷贝。Breaking：是（Protocol 接口变更，所有实现 + fakes 更新）。 → discarded [dup of #461 [CLI-PERF-1]]
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

- [ ] **R229-ARCH-1 — internal/server 已成 god package（P1）**: 80+ 文件、Server 持 17+ 子 handler。docs/design/server-split-design.md Phase 3 未推进。方案：拆 internal/wshub + internal/api 子包。Breaking：否（包内重组）。 → discarded [dup of #387 [ARCH1 server package]]
- [ ] **R229-ARCH-2 — internal/cli 越界承担 EventLog/image/SubagentLinker/history 工厂（P1）**: 21 处 session→cli 反向 import。方案：上移 EventLog/SubagentLinker 到 session/eventlog（已有 bridge 雏形），image/thumbnail 到 internal/attachment，history 到独立子包，cli 回归 Process+Protocol+Wrapper。Breaking：否。 → discarded [dup of #375 [R248-ARCH-4 AgentLinker]]
- [~] **R229-ARCH-3 — Router 单聚合根承载 6 大职责（P1）**: 文件级拆了，struct 仍持 30+ 字段 4 把锁、shutdown_lock_order_test 即证明易死锁。方案：拆 SessionStore + Lifecycle + DiscoveryService + ShimReconciler 四子组件。Breaking：是（外部引用 *session.Router 多）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R229-ARCH-4 — ManagedSession 65 方法 + 隐式语义标签（P1）**: Exempt/Stub/Scratch/Paused 通过 process==nil + key 前缀推导。方案：拆 SessionMeta（持久化）+ LiveSession（运行时）+ 显式 tag enum。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R229-ARCH-5 — KeyResolver/server/dashboard 双路径未收拢（P1）**: server.go buildSessionOpts legacy fallback + dispatch fallback resolver 并存，contract test 已证明。方案：删除 fallback，让 KeyResolver 唯一入口。Breaking：否（内部）。 → cosmetic [new-B: KeyResolver fallback removal]
- [ ] **R229-ARCH-6 — Channel Adapter 能力鸭子类型散落（P1）**: SupportsInterimMessages/AsReactor/AsQuestionCardSender/PermanentError 等可选接口持续增长，新增 LINE/Telegram 困难。方案：参照 cli.Caps 引入 PlatformCaps 聚合。Breaking：否（向后兼容）。 → cosmetic [new-B: Platform Caps]
- [ ] **R229-ARCH-7 — main.go 持 settings.json 重写 / hooks 过滤 / env 过滤业务逻辑（P1）**: 方案：抽 internal/claudesettings 子包独立可测。Breaking：否。 → cosmetic [new-B: BackendProfile.PrepareEnv]
- [ ] **R229-ARCH-8 — Dispatch DispatcherConfig 仍依赖 *session.Router 具体类型（P2）**: 接口化只完成一半。方案：与 R229-ARCH-4 配套切到 LiveSession 接口。Breaking：否。 → discarded [dup of #457 [ARCH-DISP-1 DispatcherConfig concrete]]
- [~] **R229-ARCH-9 — Hub 承担"send + broadcast" 越界（归档 2026-05-23）**: 实地核查 `internal/server/server.go:802` `SendFn: s.sendWithBroadcast` 仍是 dispatch 反向依赖 Hub 的注入点。同根因 R230-ARCH-2 / R231-ARCH-2 / R232-ARCH-9（消息出口三/四套管线）多轮重申。统一收敛到 R231-ARCH-2 跟踪，跨包重构 NEEDS-DESIGN。
- [ ] **R229-ARCH-10 — reservedKeyPrefixes 散在 4 文件（P2）**: cron/project/scratch 前缀属性表分散。方案：session/reserved_keys.go 单点表 (Prefix, Exempt, Persisted, SidebarVisible)。Breaking：否。 → discarded [dup of #728 [R222-ARCH-10 ManagedSession semantic tags] (recurring multi-round)]
- [ ] **R229-ARCH-11 — Send 路径 Dashboard/IM/Cron 三流水线规则不同（P2）**: queue/guard/broadcast 编排各异。方案：抽 SessionInvoker（参 docs/rfc/message-queue.md）。Breaking：否。 → cosmetic [new-B: SessionInvoker abstraction]
- [ ] **R229-ARCH-12 — shim 边界在 cli.Wrapper / cli.Process / session/router_shim 三处反复横跳（P2）**: 方案：shim 包提供 ShimSession 高层 API 收拢启动+reconnect+send+close。Breaking：否。 → cosmetic [new-B: shim ShimSession high-level]
- [ ] **R229-ARCH-13 — agents 字典向 6 个组件并行传递（P2）**: 方案：KeyResolver 持有，其他从 resolver 拿。Breaking：否。 → cosmetic [new-B: agents dict via KeyResolver]
- [ ] **R229-ARCH-14 — Server.New + NewWithOptions 双构造器仍并存（P3）**: ~20 test call sites 拖累。方案：批量改测试一次性删除 New。Breaking：否（内部）。 → cosmetic [new-B: Server.New + NewWithOptions removal]
- [ ] **R229-ARCH-15 — Process.Send / SendPassthrough 双路径不变量难维护（P3）**: 方案：legacy Send 视为 passthrough N=1 退化形式收拢，需评估 ACP 协议在 N=1 下行为。Breaking：否。 → cosmetic [new-B: Process.Send/SendPassthrough collapse]
- [ ] **R229-ARCH-16 — ScratchPool 与 router.sessions 双池（P2）**: 所有迭代必须遍历两侧或 IsScratchKey 判断。方案：scratch 进 sessions map + Tag=Scratch 显式。Breaking：否。 → discarded [dup of #720 [R242-ARCH-2 exempt session pool] + #432 [R176-ARCH-N4 ManagedSession state machine] (recurring multi-round)]
- [ ] **R229-ARCH-17 — KnownIDs/SessionIDToKey/Sessions/Workspace/Backend Overrides 5 个 map 同步维护（P3）**: 方案：抽 SessionIndexes struct + 一锁。Breaking：否。 → cosmetic [new-B: SessionIndexes struct]
- [ ] **R229-ARCH-18 — discovery/history/eventlog 三套读历史路径（P3）**: 方案：抽 HistoryService 单一入口归一化。Breaking：否。 → cosmetic [new-B: HistoryService unify]
- [ ] **R229-ARCH-19 — feishu MentionMe 仍 loose 实现（P3）**: 方案：platform 协议 MentionMatch 强制精确 self-id 匹配。Breaking：是（feishu 行为变化，需 release note）。 → #1009 [new-A: feishu MentionMe loose match]
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

- [ ] **R226-SEC-3 — 反向 node 在 `ws://`（无 TLS）下传 token 仅 `slog.Warn` 不阻断（P2）**: token 在第一条 WS 消息明文，passive 观察者可截获并冒充 node。方案：primary 端 `/ws-node` 加 `require_tls: true` 配置，默认 reject 非 wss 升级，除非显式 `insecure: true`。Breaking：现有 `ws://` 部署需加配置或迁 `wss://`。`internal/upstream/connector.go:210` + `internal/node/reverseserver.go:155`。 → #1026 [recurring: ws-node TLS]

### 性能 — 本轮新发现

- [ ] **R226-PERF-1 — `Protocol.ReadEvent(line string)` 每事件 `[]byte(line)` 堆拷贝（P1，封 R67-PERF-1 实施分支）**: 5-50 ev/s × N session 的强制 alloc。方案：Protocol 接口签名 `ReadEvent([]byte)`，shimMsg.Line 改 `json.RawMessage`。Breaking：是（接口）。 → discarded [dup of #461 [CLI-PERF-1]]
- [ ] **R226-PERF-4 — ACP `agent_message_chunk` 每 chunk 一次 `json.Unmarshal`（P2）**: kiro streaming 高频路径，500 unmarshal/s 仅此一处。方案：手写 byte-scan 提取 `"text":"..."` value，跳过 reflect。`internal/cli/protocol_acp.go:517`。 → #1020 [recurring: ACP parseSessionUpdate]
- [~] **R226-PERF-6 — `EventLog.applyEntryStateLocked` task 事件线性扫 turnAgents/bgAgents（P3）**: 多路 subagent 场景（>8 并行）双重 O(n)。方案：当 `len > 8` 时建 `map[string]int` 索引。`internal/cli/eventlog.go:405`。 — 评估后不实施（typical turnAgents len 1-3，result/user 事件已自动重置；threshold-based map 需 4 个同步映射 cover ToolUseID/TaskID × turn/bg，维护成本远高于收益；P3 + 无 >8 subagent 实测案例），本批 PR #164

### 代码质量 — 本轮新发现

- [ ] **R226-CR-7 — `RegisterForResume` / `RegisterCronStubWithChain` 用 `r.CLIName/Version` 在多 backend 部署下显示错误（P1）**: 这两条路径只看默认 wrapper；router_core.go loadStore 已正确走 `wrapperFor(entry.Backend)`。方案：加 `backend string` 参数 → `wrapperFor`。Breaking：caller API。`internal/session/router_discovery.go:259,362`。 → cosmetic [new-B: StubKind enum]
- [~] **R226-CR-10 — `cron/scheduler.executeOpt` 320 行 7 失败分支（归档 2026-05-23）**: R229-CR-1 / R230-CQ-9 / R232-ARCH-12 同根因多轮重申。统一收敛到 R232-ARCH-12 跟踪。
- [~] **R226-CR-11 — `cron/scheduler.go` 2739 行单文件无拆分计划（归档 2026-05-23）**: R230-CQ-14 / R232-ARCH-1 同根因多轮重申。统一收敛到 R232-ARCH-1（5–6 文件 lifecycle/jobs/execute/finish/persist/core 拆分方案）跟踪。
- [~] **R226-CR-12 — `wshub.go` 1785 行 + `feishu.go` 1461 行无拆分计划（归档 2026-05-23）**: R224-ARCH-14 / R230B-ARCH-26（feishu 4 文件 transport/wire/outbound/capability 拆分） / R230-ARCH-15（server 90+ 文件按子包拆分） 同根因多轮重申。统一收敛到 R230B-ARCH-26 + R224-ARCH-14 跟踪，跨文件重构需 ADR。

### 架构 — 本轮新发现

- [ ] **R226-ARCH-1 — `server` 包成"上帝包"（P1）**: 92 个 .go + 12 子 handler，承担路由+UI+业务编排+Hub+nodeCache。建议：薄壳 server + 拆 `internal/server/api/{cron,project,scratch,discovery,...}` 子包；Hub/nodeCache 走显式注入。需 RFC。 → discarded [dup of #387 [ARCH1 server package]]
- [~] **R226-ARCH-2 — `KeyResolver` 在 main/server/dispatch 三处独立构造（归档 2026-05-23）**: 实地核查 cmd/naozhi/main.go:834 + internal/server/server.go:551 + internal/dispatch/dispatch.go:195 三处仍各自 `session.NewKeyResolver(agents, project.NewDataSource(...))`。R230-ARCH-9 / R224-ARCH-12 / R229-ARCH-5 / R216-ARCH-3 同根因多轮重申。统一收敛到 R230-ARCH-9（projectMgr.Resolver() 单点暴露方案）跟踪，跨包构造签名变更 NEEDS-DESIGN。
- [~] **R226-ARCH-3 — dispatch `sendFn` 接 `*session.ManagedSession + cli.SendResult` 破坏分层（P1）**: dispatch 名义有 `SessionRouter` 接口但发送闭包绕过。 *(PR #330 R243-ARCH-10 已部分实施：sendFn 收口到 dispatch.Capabilities.Send interface，server.serverCaps 实现 — closure 已被 interface 替代。但**依然导入 cli/session 具体类型**（cli.ImageData / cli.EventCallback / *cli.SendResult / *session.ManagedSession 仍在 Capabilities.Send 签名里）— 跨包分层完整解耦需 dispatch.SendRequest/SendResult 自定义 DTO 才能切干净，跟踪到独立 NEEDS-DESIGN R248-ARCH-3 Deprecated 字段清理或 follow-up RFC。)*
- [ ] **R226-ARCH-4 — `session.Router` 30+ 字段（P2，封 R225-ARCH-* 重申）**: 注解块边界天然拆分点（processPool/sessionStore/historyAttacher/shimReconciler/attachmentTracker）。需独立 RFC。 → discarded [dup of #383 [ARCH2 Router god-object]]
- [ ] **R226-ARCH-5 — Platform 包间多份"形似独立"实现（P2）**: maxIncomingTextBytes / SafeHTTPClient / Start-Stop 模板 / messageID 编解码各家重复。方案：`platform.BasePlatform` + `SafeHTTPClient(opts)` + `MessageRefCodec`。 → cosmetic [new-B: platform BasePlatform helpers]
- [ ] **R226-ARCH-6 — `workspace`/`workspaces`/`Session.cwd` 别名 + 无热重载（P2）**: 已有 deprecated warn 但仍接受双形；高频字段（agents/projects/cron）需 SIGHUP 重载。Breaking：v2 schema。 → cosmetic [new-B: workspace alias hot-reload]
- [ ] **R226-ARCH-7 — 状态分散到 7 处 store（P2）**: sessions.json / event log / knownIDs / project.yaml / cron store / shim state / scratch 各自 dirty + throttle，崩溃恢复语义不对齐。方案：先画恢复矩阵契约文档；再统一 `state.Store` facade。 → cosmetic [new-B: state.Store facade]
- [ ] **R226-ARCH-8 — `dispatch.replyTracker` 含 editLoop / todoLoop / askquestion 卡片+文字 fallback 全在 dispatch.go 内（P2）**: 抽 `internal/dispatch/imreply` 子包。 → discarded [dup-recurrent: replyTracker split]
- [ ] **R226-ARCH-9 — backend 维度配置散落 5 处（P2）**: Profile + main backendModels/backendExtraArgs map + replyTagForBackend + ReplyFooterFn fallback + StartShimWithBackend + reverse capability 字段。方案：Profile 当真正注册中心，model/args defaults/tag/shim hint 全进 Profile。 → discarded [dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)]
- [ ] **R226-ARCH-10 — `TestProcess` 暴露 30+ 公有可写字段（P2）**: 新加 processIface 方法即破坏 mock。方案：拆细 procRunner/procReader/procIntrospector 子接口，InjectSession 改 builder 模式。 → discarded [dup of #430 [R176-ARCH-M2 processIface]]
- [ ] **R226-ARCH-11 — exempt 命名空间（cron:/project:/scratch:/quick:）共享标记但归属各异（P2）**: 每加一个内部子系统改 4 处过滤。方案：`KeyKind enum` + `Policy{Persist, Exempt, SidebarVisible, Sweepable}` 表，router 用查表代替 if-prefix 链。 → discarded [dup of #728 [R222-ARCH-10 ManagedSession semantic tags] (recurring multi-round)]
- [ ] **R226-ARCH-12 — 错误→用户中文消息没注册式映射（P2）**: 新加 sentinel 必须改 dispatch + server 两处。方案：`errmap.UserMessage(err) (msg, severity)` 注册表，各包注册自己的 sentinel。 → cosmetic [new-B: errmap registry]
- [ ] **R226-ARCH-13 — `cmd/naozhi/main.go` 含 ~390 行 settings.json hook/env 过滤逻辑（P3）**: 影响测试覆盖。方案：迁 `internal/cli/settingsoverride`。 → discarded [dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)]
- [ ] **R226-ARCH-14 — `cli` 包同时含 protocol/process/eventlog/history/shim io/image/thumbnail（P3）**: image/thumbnail/uuid/todo 抽到 `internal/cli/payload`；event log 迁 `internal/eventlog`（已有 persist 子包）。 → cosmetic [new-B: cli image/thumbnail submodule]
- [ ] **R226-ARCH-15 — 各 mutation 路径手动 fan-out `s.hub.BroadcastSessionsUpdate()`（P3）**: 隐式事件总线无显式 publish API。方案：`events.Bus` + 订阅模式。 → discarded [dup of #875 / #723 / #777 BroadcastSessionsUpdate]
- [ ] **R226-ARCH-16 — panic recovery 在 dispatch/platform/cli/session 四处独立写（P3）**: metric 名也分。方案：`osutil.RecoverWith(label, ...)` helper。 → cosmetic [new-B: panic recovery unify]

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

- [ ] **R225-GO-2 — `cli.Resolve` retry sleep 的 ctx 取消（P2 R224-GO-1 子分支）**: subagent_link.go:289/327 `time.Sleep(retryInterval)` 在 SIGTERM 期间无法被取消；与 R224-GO-1 同批接 ctx。Breaking：Resolve 签名加 ctx 参数。 → discarded [dup of #644 [R218B-GO-3 readLoop linker.Resolve goroutine no ctx] (recurring multi-round)]
- [ ] **R225-GO-3 — `process_event_query.InjectHistory` 裸 `go linker.Resolve(...)` 无 ctx（P2 R224-GO-3 同源第三分支）**: process_event_query.go:61 与 router_shim.go reconnect 同型；`wallclock` 取的是 `e.Time` 是对的，但 SIGTERM 期间 goroutine 仍不可中止。同 R225-GO-2 一并接 ctx。 → discarded [dup of #644 [R218B-GO-3 readLoop linker.Resolve goroutine no ctx] (recurring multi-round)]
- [ ] **R225-GO-4 — `Router.Remove` / `dropEventLogForKey` 用 `context.Background()` + 独立 timeout 而非传入 ctx（P2）**: router_cleanup.go:97 `Remove` 路径上 7s 内不可被 SIGTERM 取消，shutdown tail latency 加重。Breaking：Remove 签名加 ctx。 → discarded [dup of #699 [R222-GO-1 sendCtx Background] (recurring multi-round)]
- [~] **R225-GO-7 — `cli.Process.Kill` 在持 shimWMu 下调 closeShimConn，与 Detach 顺序不一致（误报关闭 2026-05-20）**: 复核 process.go:519-535 (Kill) 与 :617-634 (Detach) 后两者均为 `shimWMu.Lock → SetWriteDeadline → shimSendLocked → closeShimConn → Unlock` 同一模式；两个函数都在持 shimWMu 期间调 closeShimConn，并非"Detach 是 Unlock 后 close"。closeShimConn 走 sync.Once 守护的 net.Conn.Close（R219-GO-3 落地），最坏延迟仅一次系统调用，与 SetWriteDeadline+shimSendLocked 同量级。R227-GO-8 同根因一并关闭。本批 PR #169

### 安全 — 本轮新发现

- [~] **R225-SEC-1 — `cli.Wrapper.BuildArgs ExtraArgs` 缺 flag 允许列表（P1 R219-SEC-1 重申）**: 认证 dashboard 用户可以通过 ExtraArgs 注入 `--mcp-config`/`--add-dir`/`--skip-permissions` 等改变 CLI 行为的参数。capExtraArgsBytes 仅查字节长度。方案：维护 flag denylist（或 allowlist），在 BuildArgs 之前过滤；Breaking：依赖任意 ExtraArgs 的 ops 需迁移。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R225-SEC-2 — `shim.moveToShimsCgroup` 不验 CLIPID 是否真的是 shim 子进程（P1 R219-SEC-5 重申）**: handle.Hello.CLIPID 来自 shim 自报，naozhi 直接 sudo busctl 把任意 PID 移入 cgroup（可能是 sshd / pid=1）。方案：读 `/proc/<CLIPID>/status` 验 PPid == cmd.Process.Pid。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R225-SEC-4 — `selfupdate.checksums.txt` 未做 GPG/cosign 签名验证（P3 R224-SEC-1 第④项长期）**: GitHub Releases CDN 同时被妥协时 hash 与 binary 都可被换。方案：release 流程对 checksums.txt cosign 签名 + verifyChecksum 前置签名校验。 → discarded [dup-recurrent: selfupdate hardening (multi-round)]

### 性能 — 本轮新发现

- [ ] **R225-PERF-1 — `Protocol.ReadEvent(line string)` 接口签名导致 `[]byte(line)` 强制堆 copy（P1 R67-PERF-1 ACP 分支补）**: protocol_claude.go:174 / protocol_acp.go:296 都 `json.Unmarshal([]byte(line), ...)`；最热路径每事件一次 alloc。方案：接口签名改 `ReadEvent(line []byte)`，readLoop 把 trimmed 直接传入。Breaking：是（接口）。 → discarded [dup of #461 [CLI-PERF-1]]
- [ ] **R225-PERF-2 — `process_readloop` `system/task_started` 无背压裸 `go linker.Resolve`（P1）**: process_readloop.go:393 多 sub-agent 并发启动时短时间产生大量 goroutine。方案：用 buffered channel 信号量 / 工作池限并发；与 R224-GO-1 信号量改造统筹。 → discarded [dup of #644 [R218B-GO-3 readLoop linker.Resolve goroutine no ctx] (recurring multi-round)]
- [ ] **R225-PERF-4 — `applyMetadata meteringUsage merge` 用 slice O(n×m)（P2）**: process.go:717-745 meteringMu 锁内字符串 Unit 等比；MeteringUsage() 每读一次 make+copy。方案：`map[string]*MeteringEntry` 内部存储；Snapshot 路径缓存空 case。 → #1011 [new-A: meteringUsage map]
- [ ] **R225-PERF-6 — `Snapshot SubagentInfo` slice copy（P2）**: managed.go TurnAgents 即便 turnAgentCount 已快速短路，Snapshot 中其他分配仍存在；评估 SubagentInfo slice sync.Pool。 → discarded [dup of #411 [R214-PERF-2 Snapshot alloc]]
- [ ] **R225-PERF-7 — `protocol_acp.readUntilResponse` 每次握手起独立 goroutine + channel（P2）**: protocol_acp.go:677 改预先 SetReadDeadline 让 ReadLine 自然超时返回，省掉 goroutine + channel + pulse；含 R224-GO-2 同位修改。 → #1000 [new-A: ACP readUntilResponse]
- [~] **R225-PERF-9 — `wshub.eventPushLoop` 同一 session 多 WS 各自 marshal（归档 2026-05-23）**: 同 R230B-PERF-1 / R219-PERF-1 主条目跟踪。eventPushLoop 是 per-subscription 独立 goroutine 各持 lastTime 游标；两个订阅者可能在不同 lastTime 上请求不同 entry slice，无法简单一次 marshal 共享 byte 引用。需统一时间游标——RFC 级改造。本批 PR 归档。
- [~] **R225-PERF-10 — `marshalPooled` 每次 copy 一份独立 backing（归档 2026-05-23）**: session_state 字符串 enum 集合小但 Reason 字段任意（含 err 文本），LRU key 空间不可控；本批检查 broadcast 路径已有 doBroadcastSessionsUpdate 一次 marshal 多次 SendRaw 收敛热路径。本批 PR 归档。
- [~] **R225-PERF-14 — `wsclient.sweepSubGenExpiredLocked` 在 hub 写锁下扫 map（归档 2026-05-23）**: c.subGen map 与 c.subscriptions map 共用 h.mu 保护是显式契约（wsclient.go:127 注释说明）；移到 client-local mutex 需要 2 层锁协议；扫描 map 上限是 maxSubscribersPerClient=50，bounded scan 不是热路径。本批 PR 归档。
- [~] **R225-PERF-17 — `TruncateRunes(string, ...)` 无字节快检（P3，误报关闭 2026-05-20）**: reviewer 提议 `len(s) <= maxRunes*4` 短路其实方向反了——UTF-8 每 rune 1-4 字节意味着 byte 长度 ≤ rune 数*4 是 rune 数的**上界**而非下界，全 ASCII `len=200, maxRunes=50` 时 byte=200 ≤ 200 但 runes=200 > 50，加这种快检会漏截。当前 `len(s) <= maxRunes` 快检已与 `TruncateRunesBytes` 一致，无可优化空间。

### 代码质量 — 本轮新发现

- [ ] **R225-CR-1 — `cli/detect.knownBackends` 与 `backend.Profile` 注册表双轨（P1）**: detect.go:43-46 静态硬编码 Protocol 字符串；与 `Profile.Capabilities().StreamJSON` 不同步会导致 dashboard 误判。方案：`DetectBackendsCtx` 从 `backend.All()` 派生，删 knownBackends。Breaking：否。 → discarded [dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)]
- [ ] **R225-CR-5 — `backendDisplayName / normalizeBackendID` 与 `backend.Profile.DisplayName/ID` 重复（P2 R224-ARCH-1 同源）**: cli/wrapper.go:75-95；新加 backend 改四处。方案：合并到 `backend.Get` 的 DisplayName/ID 字段。 → discarded [dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)]

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

- [ ] **R227-PERF-2 — `ACPProtocol.ReadEvent` 对 agent_message_chunk 路径双 unmarshal（P1）**: 整行 unmarshal 为 RPCMessage 后每个分支再 unmarshal Params/Result。流式文本场景两次全量 JSON 解析。方案：method 短路检查后 lazy-unmarshal。 → discarded [dup of #461 [CLI-PERF-1]]
- [ ] **R227-PERF-3 — `eventlog_bridge.newEventLogSink` per-entry make+copy（P1）**: 5-50 events/s × N session 的小对象 GC 压力。方案：合并为 batch make+copy。涉及 `internal/session/eventlog_bridge.go:98`。 → discarded [dup of dup of #410 [R214-PERF-1 PersistSink single-entry] (recurring multi-round)]
- [~] **R227-PERF-4 — `wsClient.SendJSON(v)` 调 json.Marshal 每次分配 encodeState（误报关闭 2026-05-20）**: 复核后确认 encoding/json 内部 sync.Pool 已 pool encodeState (`encode.go:312-322` newEncodeState/freeEncodeState)，naozhi 加一层 sync.Pool 仅多一次 allocator round-trip 反而更贵。R229-PERF-4 已通过预 marshal 静态帧（wsErrNotAuthMsg/wsErrRateLimitedMsg 等）覆盖了真热的小响应路径。本批 PR #187 关闭归档。
- [ ] **R227-PERF-5 — `WriteUserMessageLocked` json.Marshal encodeState alloc（P2）**: 用户 prompt 发送频率不高但每次 alloc。方案：sync.Pool 复用 bytes.Buffer + Encoder。 → cosmetic [new-B: WriteUserMessageLocked alloc]
- [ ] **R227-PERF-6 — `Cleanup` + `saveIfDirty` 每次写锁内 map clone 3 份（P2）**: 50 session × 30s 间隔每分钟 2 次 O(n) clone。方案：传 []*ManagedSession 切片，配合 listRefsPool。 → cosmetic [new-B: Cleanup map clone]
- [ ] **R227-PERF-8 — `BroadcastSessionsUpdate` time.AfterFunc 每次 alloc（P2）**: 高频 notify 下每次 timer + WG 开销。方案：Hub 持久 *Timer + Reset。 → discarded [dup of #376 [R248-ARCH-6 Hub god]]
- [~] **R227-PERF-9 — `EventLog.Append` invokePersistSink 单条 slice 逃逸（P2）**: 5-50/s × N session 热路径。方案：EventLog 内置 [1]EventEntry scratch + sync.Pool。涉及 `internal/cli/eventlog.go:660`。 — 评估关闭（已归档 2026-05-23 复核）：同根因 R215-PERF-P2-1 / R217-PERF-4 / R219-PERF-4 / R222-PERF-8 / R226-PERF-5 / R228-PERF-7 / R229-PERF-5 / R230C-PERF-2，已统一收敛到 R230C-PERF-2。`internal/cli/eventlog.go:803-811` godoc 显式锚定 PersistSink 契约——sink 可保留 slice 跨 return，所以 [1]EventEntry stack scratch 无可避免地通过 atomic.Pointer-loaded fn ptr escape；sync.Pool 只是把 alloc 换成 Get/Put 开销（48B payload 收益不显）。生产热路径已被 R230-PERF-1 sink-nil 早返回覆盖。本批 PR。
- [~] **R227-PERF-11 — `EventLog.Append` storeAtomicString 每次 *string alloc（评估关闭 2026-05-20）**: 条目自标"降级仅观察"。复核 textutil.StoreAtomicString 已用 atomic.Pointer.Load + 字符串相等短路（同值不重新 Store *string），命中率 ~99%（lastActivitySummary 在同 tool_use 持续时不变）。sync.Pool[string] 在 textutil 是叶子包做不到 lock-free 复用，且分支预测器对短路命中早已优化。本批 PR #168 关闭归档。
- [~] **R227-PERF-12 — `ACPProtocol.parseSessionUpdate` tool_call/tool_call_update 分支双 alloc（P2）**: AssistantMessage ptr + ContentBlock slice。方案：tool name 直接存 ToolCall.Title；ContentBlock 改 [1]ContentBlock + count。 — 评估关闭（已归档 2026-05-23 复核）：Message.Content 被多处读（`internal/cli/askquestion_test.go:51` `internal/dispatch/status_test.go` 等通过 `Content[0].Name` 提取 tool_use name），删除 Message 字段需跨包重写消费者；[1]ContentBlock 改 fixed array 失去 slice header 灵活性，与 [`*AssistantMessage` 含 nil 表示空] 契约冲突。tool_call 分支频率为每 turn 几十次（远低于 stdout 热路径），单次 alloc 成本被 ACP RPC 序列化掩盖。godoc 已通过 `event.go:78-79` 锚定 ToolUseID 优于 Message.Content[].Name 的提取方式。本批 PR。
- [~] **R227-PERF-16 — `EventEntriesSince` dead-session 分支全量扫描+stable sort（误报关闭 2026-05-20）**: 条目自标"500 entry stable sort < 1µs，可接受"——dead-session 重订阅是低频路径（tab reload），微秒级开销远低于其他热路径。InjectHistory 端排序也会让 replay 与 live append 之间的语义需要重新定义。本批 PR #187 关闭归档。
- [ ] **R227-PERF-17 — `shim.ServerMsg.MarshalLine` 每次 json.Marshal alloc（P3）**: shim binary 独立。方案：sync.Pool[bufEnc]。**降级**：shim 独立 binary，不影响主进程，单独 PR 处理。 → cosmetic [new-B: shim MarshalLine pool (deferred)]
- [ ] **R227-PERF-18 — `eventPushLoop` EventEntriesSince per-goroutine 独立 slice（P3）**: 50 订阅 tab × 同 session 各自分配。方案：扩展 EntriesSinceInto(dst) 接口接受 caller-owned buffer。Breaking。 → cosmetic [new-B: EventEntries Since dead]
- [~] **R227-PERF-19 — `Cleanup` Pass 1 candidates 用 time.Time 而非 lastActiveNS int64（误报关闭 2026-05-20）**: 条目自标"传染性大，ROI 低"——Cleanup 是 30s tick 的低频路径，time.Time 接口更直观且与 LastActive() 公开 API 对齐；改 int64 会让 candidate slice 类型与 ManagedSession.LastActive() 返回签名分裂，下游若拿到 candidate slice 需各自 time.Unix 还原，传染面广。本批 PR #187 关闭归档。

### 代码质量 — 本轮新发现

- [~] **R227-CR-5 — `dispatch.sendAndReply` 250+ 行 5+ 职责（P2）**: 错误处理、生命周期通知、事件跟踪、结果解析、图片读取、AskQuestion 抑制、文字分割。方案：拆 buildSendContext / handleGetOrCreateError / handleSendError / deliverResult。涉及 `internal/dispatch/dispatch.go:527`。重申 R219-CR-7。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR

### 架构 — 本轮新发现

- [ ] **R227-ARCH-1 — `session.Router` god-package：27 字段、80 方法跨 10 文件（P1）**: router-split refactor 只切了文件没切类型。方案：进一步拆 (*coreState, *lifecycleManager, *shimReconciler, *cleanupSweeper, *backendRegistry) 子结构 + facade，或承认现实合回 router.go。 → discarded [dup of #383 [ARCH2 Router god-object]]
- [ ] **R227-ARCH-2 — `cli.Process` god-struct：50+ 字段同一 RWMutex（P1）**: shimIO/turnState/procMeta/passthrough/heartbeat/watchdog/linker/快照同住一锁命名空间。方案：分 shimIO + turnState + procMeta 三子组件，Process 缩为组合体。 → discarded [dup of #430 [R176-ARCH-M2 processIface]]
- [ ] **R227-ARCH-3 — `history.Source` 与 `cli.HistorySource` 双接口结构同形（P1）**: cli 不能 import history 又要 history factory 注册，新建 internal/wireup（或 historywire）包统一管 history factory 注册解套。 → cosmetic [new-B: history.Source unify]
- [ ] **R227-ARCH-4 — `cli.backend.Profile` 与 `cli.detect.knownBackends + normalizeBackendID` 双源（P1）**: backend 元信息双轨；新加 backend 三处同步。方案：Profile 加 Aliases/IsDefault；knownBackends 从 backend.All() 派生。 → discarded [dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)]
- [~] **R227-ARCH-5 — 4 个 consumer-side SessionRouter 接口农场（P2）**: dispatch/cron/upstream/server 各自声明，server 内多数 handler 仍裸 *session.Router。方案：抽 session.RouterCore 基础接口 + RouterReader 子集。重申 R215-ARCH-P2-4。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R227-ARCH-6 — shim 协议三套版本号（ProtocolVersion/stateVersion/SchemaVersion）无升级矩阵（P2）**: zero-downtime restart 后变定时炸弹。方案：写 docs/rfc/shim-versioning.md 合并到唯一 ProtocolEpoch + 兼容矩阵 contract test。 → #1016 [new-A: shim version matrix RFC]
- [ ] **R227-ARCH-7 — sessions.json sidecar / EventLog per-record / shim state inline / cron 各一套 schema 版本机制（P2）**: 新 store 作者每次重新决定。方案：抽 internal/storefmt 包定义 VersionedFile + future-version 处理枚举。 → cosmetic [new-B: storefmt.VersionedFile]
- [ ] **R227-ARCH-8 — 4 个 platform adapter 各自 httpClient/hookSem/dedup/SanitizeForLog（P2）**: 同类 SSRF/cap 修复在 4 处反复打。方案：抽 platform/transport 公共组件（SafeHTTPClient/InboundDispatcher/OutboundRetryWithBackoff）。 → cosmetic [new-B: platform/transport common]
- [ ] **R227-ARCH-9 — `cli` 包反向依赖 history 概念，HistorySessionView 唯一实现是 *session.ManagedSession（P2）**: 抽象塌陷反例。方案：与 R227-ARCH-3 合并修。 → cosmetic [new-B: cli history reverse-dependency]
- [ ] **R227-ARCH-10 — Protocol 能力查询走 SupportsX 与 Capabilities() Caps 双轨（P2）**: 新能力两路径下游可漂移。方案：收敛到 Capabilities() 单方法，删 SupportsX。Breaking。 → cosmetic [new-B: Platform Caps]
- [ ] **R227-ARCH-11 — cli 包 import metrics 14 处隐式全局（P2）**: 单测要 mock metrics。方案：MetricsSink interface + DI，默认 noop。 → cosmetic [new-B: MetricsSink interface]
- [ ] **R227-ARCH-12 — `processIface` 30+ 方法逼近抽象塌陷（P3）**: testutil fake 200+ 行。方案：拆 processSender/processInspector/processLifecycle 三角色。 → discarded [dup of #430 [R176-ARCH-M2 processIface]]
- [ ] **R227-ARCH-13 — naozhilog/claudejsonl/kirojsonl 三 history reader 同算法独立维护（P3）**: ctxCheckEvery / limit shrink / ENOENT 各自实现。方案：抽 history/internal/scan ReverseScan 原语。 → discarded [dup of #458 [ARCH-SESS-1 history backend]]
- [ ] **R227-ARCH-14 — dispatch 与 platform 之间 type assertion 探测能力（P3）**: 加新平台能力 N×M 矩阵分支。方案：与 R227-ARCH-10 同——platform 加 Capabilities() Caps。 → cosmetic [new-B: Platform Caps]
- [ ] **R227-ARCH-15 — `eventlog_bridge` 在 cli↔persist 中介中做 EventEntry→Entry marshal（P3）**: 序列化责任在中间人违反"数据生产方负责"。方案：cli.EventEntry.MarshalForPersist() 方法 + bridge 简化。 → cosmetic [new-B: eventlog_bridge MarshalForPersist]
- [ ] **R227-ARCH-16 — `claudejsonl/kirojsonl` 通过 init 注册 + session blank import 隐式生命周期（P3）**: factory 没注册导致 NoopHistorySource 的 bug 要查 4 文件。方案：与 R227-ARCH-3 合并；显式调 wireup.RegisterX()。 → discarded [dup of #458 [ARCH-SESS-1 history backend]]
- [ ] **R227-ARCH-17 — 30+ interface 半数单实现（P3）**: HistorySessionView/AgentIntrospector/deadlineInterrupter 等单实现接口未替代。方案：写 docs/CONTRIBUTING-interfaces.md 三条规则。 → discarded [dup of #375 [R248-ARCH-4 AgentLinker]]
- [ ] **R227-ARCH-18 — contract test 边界三类含义混用（P3）**: type assertion / 行为契约 / 内存模型断言用同一 *_contract_test.go 名。方案：拆 *_iface_assert / *_behaviour / *_invariant 命名。 → cosmetic [new-B: contract_test naming convention]
- [ ] **R227-ARCH-20 — `dispatch.passthroughCtxKey/urgentCtxKey` 用 ctx.Value 跨包传 boolean（P3）**: Go 反模式（ctx.Value 应仅用于请求级元数据）。方案：定义 dispatch.SendOptions struct 或 functional options。Breaking。 → cosmetic [new-B: ctx.Value antipattern]

### 配置 / 测试 — 本轮新发现

- [ ] **R227-TEST-2 — `cli.detectVersion` 用 context.Background()（P3）**: SIGTERM 期间 --version probe 等满 5s。方案：NewWrapper 接 ctx 参数（Breaking，3-5 个调用点）。 → cosmetic [new-B: detectVersion ctx]

## Round 228 — 5-agent 并行 review 第 40 轮（2026-05-20 第二批）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 90 条发现。
> 14 处 FIX-READY 已落地本轮 PR（详情见顶部摘要）。下面是需设计决策、breaking、跨包重构、或方案不唯一不适合本轮直接修的条目。

### Go 正确性 — 本轮新发现

- [ ] **R228-GO-3 — `reconnectShims` replay goroutine 无 ctx 绑定（P2）**: 重放段对每个 `task_started` 启动裸 goroutine 调 `linker.Resolve`，SIGTERM 时延迟 shutdown。方案：与 R227-GO-2 / R225-GO-2 合并，Resolve 接 ctx。涉及 `internal/session/router_shim.go:361-394`。 → discarded [dup of #644 [R218B-GO-3 readLoop linker.Resolve goroutine no ctx] (recurring multi-round)]

### 安全 — 本轮新发现

- [~] **R228-SEC-1 — Dashboard 主页 CSP `script-src 'unsafe-inline'`（P2 R227-SEC-9 重申）**: 主页用 `'unsafe-inline'` 而 login 页已用 hash-based CSP 收敛。方案：为响应生成 nonce + 内联 `<script>` 注入 nonce；或迁移内联脚本到外部文件。Breaking：需改 HTML 模板。涉及 `internal/server/dashboard.go:390`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R228-SEC-2 — `serveRender` `os.Open(resolved)` 二次打开 inode-swap TOCTOU（P1 R227-SEC-2 重申）**: `handleFileGet` Lstat 后 `serveRender` 再次 Open，存在窗口可被符号链接替换。方案：在 Lstat 之后立即 Open 拿 fd 传入下游函数；或用 `f.Stat()` 比对 inode。涉及 `internal/server/project_files.go:590,684,762,856`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R228-SEC-4 — `/health` 端点无 rate limiting（P3 R227-SEC-10 重申）**: 未认证响应含 version 字段可 fingerprint。方案：per-IP rate limiter 60/min，或把 version 移到认证区段。涉及 `internal/server/server.go:809`、`internal/server/health.go:169`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R228-SEC-5 — WS Hub 同一 session key 订阅数量无 per-key 上限（P3 R227-SEC-11 重申）**: 单 token 开 100 个 WS 各订同 key 触发 100 路 fan-out。方案：维护 `keySubCount map[string]int` + 阈值（如 20）。涉及 `internal/server/wshub.go`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R228-SEC-6 — `serveRender`/`serveRaw` sandbox CSP `style-src 'unsafe-inline'`（P3 R226-SEC-9 重申）**: CSS-based exfiltration 攻击面。方案：nonce 化或去掉 unsafe-inline。涉及 `internal/server/project_files.go:731,~905`。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR

### 性能 — 本轮新发现

- [ ] **R228-PERF-3 — `subagent_transcript.readLocked` 每次 `os.Open` 不复用 fd（P2）**: 每 200ms × 50 active tailer = 250 open/close fd/s。方案：缓存 `*os.File`，Tail 用 Seek 复用，inode 变化时重 Open。涉及 `internal/cli/subagent_transcript.go:63-88`。 → #1025 [recurring: transcript reader fd reuse]
- [~] **R228-PERF-7 — `EventLog.Append` `[]EventEntry{e}` 字面量 heap escape（P3 R219-PERF-4 具体修法方向）**: 单条 slice 字面量逃逸。方案：先 `-gcflags=-m` 验证再决定栈数组+切片或 sync.Pool。涉及 `internal/cli/eventlog.go:703`。 — NEEDS-DESIGN 归档 2026-05-23（与 R219-PERF-4 / R222-PERF-8 / R215-PERF-P2-1 同根因；R230-PERF-1 sink-nil 早返回已覆盖生产热路径，sink-attached 路径的 slice 字面量受 PersistSink 保留契约约束结构性必需；本批 eventlog.go 加 godoc 锚点）

### 代码质量 — 本轮新发现

### 架构 — 本轮新发现

- [ ] **R228-ARCH-1 — `session` 包既通过 `cli.Wrapper.ShimManager` 又直接调 `shim.SocketPath/KeyHash/WaitSocketGone` 双重接入（P1）**: 抽象塌陷。方案：把三个 shim 调用收进 `cli.Wrapper.WaitSessionShimGone(key)`。涉及 `internal/session/router_lifecycle.go:1115-1116` + `router_shim.go:27,57-72,146-180`。 → discarded [dup of #729 [R246-PERF-14 sysession runner subprocess] (recurring multi-round)]
- [ ] **R228-ARCH-2 — `cli.Wrapper.ShimManager` 公开字段穿透到 session（P1）**: 应代理 `Discover/Reconnect` 等方法。Breaking（公开字段消失）。涉及 `internal/cli/wrapper.go:38` + `internal/session/router_backend.go:151`。 → discarded [dup of #729 [R246-PERF-14 sysession runner subprocess] (recurring multi-round)]
- [ ] **R228-ARCH-3 — `server/wshub` 直接持 `*cli.SubagentLinker` 指针长寿命缓存（P1 与 RFC v4 phase 3+ TODO 同根）**: Linker 重建后旧 map key 残留为 GC root。方案：session 层暴露 `WireLinkerOnce(key, ...)` API，把指针弱引用封进 session 包。涉及 `internal/server/wshub.go:165` + `internal/server/dashboard_agent_events.go:66,72,80`。 → discarded [dup of #375 [R248-ARCH-4 AgentLinker]]
- [ ] **R228-ARCH-4 — `cli.AskQuestion`/Item/Opt 与 `platform.QuestionCard`/Item/Option 双套结构体（P2）**: dispatch 手工字段拷贝，加字段易漏。方案：抽到共享包（如新建 `internal/askq` 或 `internal/eventlog/schema`）。涉及 `internal/cli/event.go:141-166` + `internal/platform/platform.go:108-141`。 → discarded [dup of #626 [R217-ARCH-3] discovery imports cli for EventEntry (recurring multi-round)]
- [ ] **R228-ARCH-7 — `processIface` 32-method 胖接口 + 内部强转回 `*cli.Process`（P2）**: 抽象漏了。方案：要么删 interface 直接用 `*cli.Process`；要么拆成 3 个小接口。需设计决策。涉及 `internal/session/managed.go:33-102` + `router_lifecycle.go:829`。 → discarded [dup of #430 [R176-ARCH-M2 processIface]]
- [ ] **R228-ARCH-8 — 4 个 platform adapter 各自 `var fooHTTPClient` SSRF-defense client（P2）**: 4 份近一致的 redirect+TLS 1.2 floor client。方案：`internal/platform.NewSafeHTTPClient(timeout)` helper。涉及 feishu/discord/weixin/slack 各自顶部 var。 → cosmetic [new-B: platform/transport common]
- [ ] **R228-ARCH-12 — `cron.SchedulerConfig` 直接持 `session.AgentOpts` + `platform.Platform`（P2）**: cron 字段调整波及 cron。方案：cron 加自己的 JobNotifier interface + JobAgentOpts 局部类型。涉及 `internal/cron/scheduler.go:100-101,213-214`。 → discarded [dup of #670 [R219-ARCH-8 cron.scheduler holds platforms map] (recurring multi-round)]
- [ ] **R228-ARCH-13 — `cli.HistoryFactoryFn` registry blank import 在 session 包（P2）**: 触发点已迁到 `cli.NewWrapper` 但 import 列表残留在 session。方案：移到 cli/wrapper.go 或 cmd/naozhi/main.go。涉及 `internal/session/router_core.go:21-32`。 → discarded [dup of #458 [ARCH-SESS-1 history backend]]
- [ ] **R228-ARCH-18 — `dispatch.Dispatcher.projectMgr` 仅用于 slash-command UX 但持整个 `*project.Manager`（P3）**: 30+ 方法面。方案：内部 1-method interface 注入。涉及 `internal/dispatch/dispatch.go:56`。 → cosmetic [new-B: Dispatcher.projectMgr narrow]

## Round 230C — PR #198 详细归档 NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共 82 条发现，PR #198 同批的另一组细化条目（编号集 C 区分 line 211 的核心 R230 节与 line 277 的 R230B 节）。
> 以下条目为非 breaking 但需要更大改动 / 需设计决策 / 跨模块的发现，登记追踪。

### Security（剩余）

- [ ] **R230C-SEC-7 — dashboard_token 8-15 char 公网监听只 Warn（P2）**: `cmd/naozhi/main.go:938` 公网部署时 8 字节 token 仍允许启动只 slog.Warn。方案：监听非 loopback 时最小长度提升到 16 + slog.Error+os.Exit(1)，或加 entropy 估算拒字典词。Breaking：是（部分公网部署需调长 token）。 → discarded [dup of #618 [R242-SEC-5 cookie revocation] (recurring multi-round)]
- [~] **R230C-SEC-10 — cron notify_chat_id 发送时未再次校验（多轮 NEEDS-DESIGN 归档 2026-05-23）**: validateNotifyTarget 当前在 internal/server/dashboard_cron.go，cron 包重新调用会引入 cron→server 反向 import 违反 layer。彻底方案需把 validateNotifyTarget 提到 internal/platform 或 internal/cron 共享 helper（涉及 utf8 / size cap / platform allowlist 多约束）。当前防御深度：写路径（CRUD）已校验；读路径仅在 jobs file 被外部篡改时绕过——allowed_root + 0o600 文件权限是首道防线。归档 NEEDS-DESIGN，本批 PR
### Go 正确性 / 并发（剩余）

- [~] **R230C-GO-4 — spawnSession inline history load 同步 IIFE 包了 historyWg（误报关闭 2026-05-23）**: 实地复核 loadResumeHistoryOnSpawn godoc 已明确意图："Synchronous — runs on the spawnSession caller goroutine. The historyWg Add/Done dance still tracks the call so Shutdown.Wait can drain in-flight loads"。historyWg 是独立 WaitGroup（非 sessionsWg），让 Shutdown 等历史加载完成。归档关闭，本批 PR。
- [ ] **R230C-GO-7 — executeOpt sendCtx 用 context.Background 让 5h 任务无法 shutdown 期取消（P2）**: `internal/cron/scheduler.go:1955` 5h execTimeout 的 cron 任务 stopCtx fire 后仍跑满。方案：deadline watchdog 扩展为 stopCtx 触发也调 InterruptViaControl，或 sendCtx 改用 `context.WithTimeout(s.stopCtx, jobTimeout+grace)`。Breaking：否。 → discarded [dup of #699 [R222-GO-1 sendCtx Background] (recurring multi-round)]
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

- [ ] **R230C-ARCH-1 — DESIGN.md 第 5 节 HTTP Server 描述与现实严重失真（P1）**: `docs/design/DESIGN.md:492-516` 说 server 只注册 /health；实际 92 个文件 / 60+ 路由。方案：DESIGN.md 加"实际 vs 理想"对比 + ADR 链接 server-split RFC。Breaking：否。 → discarded [dup of #387 [ARCH1 server package]]
- [~] **R230C-ARCH-2 — dispatch/cron/upstream/sysession 各自定义 SessionRouter 接口（P1）**: 4 包同名 `SessionRouter` 各持不同方法集。方案：约定 `XxxSessionRouter` 命名空间 + `internal/session/contracts/` 集中接口约束。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230C-ARCH-3 — cron Scheduler 直持 platforms map 越层调 Reply（P1）**: cron 已悄悄成为第二个 dispatch（持 Agents map / 复刻 retry+SplitText）。方案：cron 内禁用 platform.Platform，强制走 dispatch facade。Breaking：是（与 R219-ARCH-8 合并）。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [~] **R230C-ARCH-4 — processIface god interface 拆分优先级（P1）**: `internal/session/managed.go:35-102` 30+ 方法含 dashboard-only 字段。方案：先剥 dashboard 字段到 ProcessIntrospector，process 核心剩 8-10 方法。Breaking：是。 — 多轮 NEEDS-DESIGN 归档 2026-05-23（同根因主条目跟踪），本批 PR
- [ ] **R230C-ARCH-5 — 保留 key 命名空间策略表分散在 5 处（P1）**: cron:/project:/scratch:/sys: 在 reservedKeyPrefixes/exemptKeyPrefixes/saveStore/Cleanup/Sidebar 各列。方案：`KeyKind enum + Policy struct` 单一事实源 + vet 阻断。Breaking：否。 → discarded [dup of #728 [R222-ARCH-10 ManagedSession semantic tags] (recurring multi-round)]
- [ ] **R230C-ARCH-9 — Platform caps 抽象不到位（P2）**: `cli.ProtocolCaps` 已聚合，但 Platform 仍混用 `MaxReplyLength` 值 / `SupportsInterimMessages` bool / `AsReactor` 接口断言三种返回风格。方案：`PlatformCaps` 结构体聚合（与 R229-ARCH-6 合并）。Breaking：是。 → cosmetic [new-B: Platform Caps]
- [ ] **R230C-ARCH-10 — Hub 三 setter vs SysessionMgr 字段注入风格不一（P2）**: `wshub.go:282-291` SetScheduler/SetUploadStore/SetScratchPool 是 setter；ServerOptions.SysessionManager 是字段。方案：四个一律走 HubOptions required 字段。Breaking：否（与 R219-ARCH-5 合并）。 → discarded [dup of #720 [R242-ARCH-2 exempt session pool] + #432 [R176-ARCH-N4 ManagedSession state machine] (recurring multi-round)]
- [ ] **R230C-ARCH-11 — Session mode 4 态文档 vs 实际 7+ 态无单一权威类型（P2）**: `cli.ProcessState` 4 态 + ManagedSession.exempt + key 前缀派生 stub + process==nil 派生 paused。方案：`session.SessionMode enum {Active, Stub, Paused, Scratch, Exempt}` 正交叠加 + 类型化 transitions。Breaking：是。 → discarded [dup of #432 [ManagedSession state machine]]
- [ ] **R230C-ARCH-13 — scratchPool 与 router.sessions 双池强迫 Hub 分流（P2）**: `internal/session/scratch.go` + `dashboard_scratch.go` 每加一个 scratch 操作两路径都得复刻。方案：合到 sessions map + Tag=Scratch 或文档化双池约束。Breaking：是。 → discarded [dup of #720 [R242-ARCH-2 exempt session pool] + #432 [R176-ARCH-N4 ManagedSession state machine] (recurring multi-round)]
- [ ] **R230C-ARCH-14 — 文件化状态多实例并发写无 flock（P2）**: 6 个独立 atomic write store 假设单实例独占。方案：state file 加 `flock(LOCK_EX) + writer_pid + writer_host + generation`。Breaking：是。 → cosmetic [new-B: state-file flock]
- [ ] **R230C-ARCH-15 — main.go ~390 行 backend-specific settings.json 重写（P2）**: kiro backend 不需要。方案：抽 `BackendProfile.PrepareEnv()`，main 只调 profile。Breaking：否（与 R229-ARCH-7 合并）。 → cosmetic [new-B: BackendProfile.PrepareEnv]



## Discarded (auto-generated by triage-findings 2026-05-25)

Items below were dropped during triage. See annotations on individual bullets for routing.

### R225 discarded
- R225-CR-1: dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)
- R225-CR-5: dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)
- R225-GO-2: dup of #644 [R218B-GO-3 readLoop linker.Resolve goroutine no ctx] (recurring multi-round)
- R225-GO-3: dup of #644 [R218B-GO-3 readLoop linker.Resolve goroutine no ctx] (recurring multi-round)
- R225-GO-4: dup of #699 [R222-GO-1 sendCtx Background] (recurring multi-round)
- R225-PERF-1: dup of #461 [CLI-PERF-1]
- R225-PERF-2: dup of #644 [R218B-GO-3 readLoop linker.Resolve goroutine no ctx] (recurring multi-round)
- R225-PERF-6: dup of #411 [R214-PERF-2 Snapshot alloc]
- R225-SEC-4: dup-recurrent: selfupdate hardening (multi-round)

### R226 discarded
- R226-ARCH-1: dup of #387 [ARCH1 server package]
- R226-ARCH-10: dup of #430 [R176-ARCH-M2 processIface]
- R226-ARCH-11: dup of #728 [R222-ARCH-10 ManagedSession semantic tags] (recurring multi-round)
- R226-ARCH-13: dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)
- R226-ARCH-15: dup of #875 / #723 / #777 BroadcastSessionsUpdate
- R226-ARCH-4: dup of #383 [ARCH2 Router god-object]
- R226-ARCH-8: dup-recurrent: replyTracker split
- R226-ARCH-9: dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)
- R226-PERF-1: dup of #461 [CLI-PERF-1]

### R227 discarded
- R227-ARCH-1: dup of #383 [ARCH2 Router god-object]
- R227-ARCH-12: dup of #430 [R176-ARCH-M2 processIface]
- R227-ARCH-13: dup of #458 [ARCH-SESS-1 history backend]
- R227-ARCH-16: dup of #458 [ARCH-SESS-1 history backend]
- R227-ARCH-17: dup of #375 [R248-ARCH-4 AgentLinker]
- R227-ARCH-2: dup of #430 [R176-ARCH-M2 processIface]
- R227-ARCH-4: dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)
- R227-PERF-2: dup of #461 [CLI-PERF-1]
- R227-PERF-3: dup of dup of #410 [R214-PERF-1 PersistSink single-entry] (recurring multi-round)
- R227-PERF-8: dup of #376 [R248-ARCH-6 Hub god]

### R228 discarded
- R228-ARCH-1: dup of #729 [R246-PERF-14 sysession runner subprocess] (recurring multi-round)
- R228-ARCH-12: dup of #670 [R219-ARCH-8 cron.scheduler holds platforms map] (recurring multi-round)
- R228-ARCH-13: dup of #458 [ARCH-SESS-1 history backend]
- R228-ARCH-2: dup of #729 [R246-PERF-14 sysession runner subprocess] (recurring multi-round)
- R228-ARCH-3: dup of #375 [R248-ARCH-4 AgentLinker]
- R228-ARCH-4: dup of #626 [R217-ARCH-3] discovery imports cli for EventEntry (recurring multi-round)
- R228-ARCH-7: dup of #430 [R176-ARCH-M2 processIface]
- R228-GO-3: dup of #644 [R218B-GO-3 readLoop linker.Resolve goroutine no ctx] (recurring multi-round)

### R229 discarded
- R229-ARCH-1: dup of #387 [ARCH1 server package]
- R229-ARCH-10: dup of #728 [R222-ARCH-10 ManagedSession semantic tags] (recurring multi-round)
- R229-ARCH-16: dup of #720 [R242-ARCH-2 exempt session pool] + #432 [R176-ARCH-N4 ManagedSession state machine] (recurring multi-round)
- R229-ARCH-2: dup of #375 [R248-ARCH-4 AgentLinker]
- R229-ARCH-8: dup of #457 [ARCH-DISP-1 DispatcherConfig concrete]
- R229-PERF-1: dup of #461 [CLI-PERF-1]
- R229-SEC-1: dup of #653 [R219-SEC-1] (recurring multi-round)
- R229-SEC-12: dup of #441 [H2 CSP unsafe-inline]
- R229-SEC-2: dup of #655 [R219-SEC-2] (recurring multi-round)
- R229-SEC-3: dup of #658 [R237-SEC-9] (recurring multi-round)

### R230 discarded
- R230-ARCH-14: dup of #458 [ARCH-SESS-1 history backend]
- R230-ARCH-15: dup of #387 [ARCH1 server package]
- R230-ARCH-17: dup of #387 [ARCH1 server package]
- R230-ARCH-18: dup of #699 [R222-GO-1 sendCtx Background] (recurring multi-round)
- R230-ARCH-3: dup of #457 [ARCH-DISP-1 DispatcherConfig concrete]
- R230-ARCH-4: dup of #383 [ARCH2 Router god-object]
- R230-ARCH-5: dup of #376 [R248-ARCH-6 Hub god]
- R230-ARCH-9: dup of #604 [R237-ARCH-12] KeyResolver shared
- R230-CQ-2: dup of #656 [R219-CR-7] sendAndReply split
- R230-SEC-2: dup of #724 [R222-ARCH-9 env probing scattered] (recurring multi-round)

### R230B discarded
- R230B-ARCH-11: dup of #375 [R248-ARCH-4 AgentLinker]
- R230B-ARCH-12: dup of #720 [R242-ARCH-2 exempt session pool] + #432 [R176-ARCH-N4 ManagedSession state machine] (recurring multi-round)
- R230B-ARCH-13: dup of #439 [R172-ARCH-D9 discovery scanner]
- R230B-ARCH-14: dup of #670 [R219-ARCH-8 cron.scheduler holds platforms map] (recurring multi-round)
- R230B-ARCH-15: dup of #430 [R176-ARCH-M2 processIface]
- R230B-ARCH-18: dup of #531 [R215-SEC-P1-1] skip-permissions hardcode
- R230B-ARCH-2: dup of dup of #431 [R176-ARCH-M3 Hub HubOptions one-shot] (recurring multi-round)
- R230B-ARCH-24: dup of dup of #387 (recurring multi-round)
- R230B-ARCH-26: dup of #409 [R214-ARCH-13 feishu.go split]
- R230B-ARCH-3: dup of #670 [R219-ARCH-8 cron.scheduler holds platforms map] (recurring multi-round)
- R230B-ARCH-9: dup of #376 [R248-ARCH-6 Hub god]
- R230B-GO-2: dup of #644 [R218B-GO-3 readLoop linker.Resolve goroutine no ctx] (recurring multi-round)
- R230B-SEC-3: dup of #653 [R219-SEC-1] (recurring multi-round)
- R230B-SEC-5: dup of #441 [H2 CSP unsafe-inline]

### R230C discarded
- R230C-ARCH-1: dup of #387 [ARCH1 server package]
- R230C-ARCH-10: dup of #720 [R242-ARCH-2 exempt session pool] + #432 [R176-ARCH-N4 ManagedSession state machine] (recurring multi-round)
- R230C-ARCH-11: dup of #432 [ManagedSession state machine]
- R230C-ARCH-13: dup of #720 [R242-ARCH-2 exempt session pool] + #432 [R176-ARCH-N4 ManagedSession state machine] (recurring multi-round)
- R230C-ARCH-5: dup of #728 [R222-ARCH-10 ManagedSession semantic tags] (recurring multi-round)
- R230C-GO-7: dup of #699 [R222-GO-1 sendCtx Background] (recurring multi-round)
- R230C-SEC-7: dup of #618 [R242-SEC-5 cookie revocation] (recurring multi-round)

### R231 discarded
- R231-ARCH-10: dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)
- R231-ARCH-11: dup of #383 [ARCH2 Router god-object]
- R231-ARCH-2: dup of #459 [ARCH-SVR-1 send-with-broadcast]
- R231-ARCH-3: dup of #458 [ARCH-SESS-1 history backend]
- R231-ARCH-4: dup of #383 [ARCH2 Router god-object]
- R231-ARCH-8: dup of #428 [RNEW-ARCH-404 cli.Caps] (recurring multi-round)
- R231-ARCH-9: dup of #673 [R219-ARCH-9] workspaceOverrides
- R231-PERF-8: dup of #411 [R214-PERF-2 Snapshot alloc]
- R231-SEC-8: dup of #441 [H2 CSP unsafe-inline]

### R232 discarded
- R232-ARCH-8: dup of #457 [ARCH-DISP-1 DispatcherConfig concrete]
- R232-ARCH-9: dup of #670 [R219-ARCH-8 cron.scheduler holds platforms map] (recurring multi-round)
- R232-SEC-2: dup of #655 [R219-SEC-2] (recurring multi-round)
- R232-SEC-3: dup of #441 [H2 CSP unsafe-inline]

### R233 discarded
- R233-ARCH-5: dup of #720 [R242-ARCH-2 exempt session pool] + #432 [R176-ARCH-N4 ManagedSession state machine] (recurring multi-round)
- R233-GO-3: dup of #742 [R238-ARCH-3] runInflight atomic.Pointer
- R233-PERF-2: dup of #556 [R242-GO-8] cacheHeadPush O(N)
- R233-PERF-3: dup of #529 + #671 + #592 (recurring multi-round)
- R233-PERF-7: dup of #865 [R245-PERF-15] agent_tailer 200ms tick
- R233-SEC-4: dup of #441 [H2 CSP unsafe-inline]
- R233-SEC-7: dup of #691 [R242-CR-3 GET /api/cron 1Hz no rate limiter] (recurring multi-round)

### R233B discarded
- R233B-ARCH-2: dup of #383 [ARCH2 Router god-object]
- R233B-ARCH-4: dup of #626 [R217-ARCH-3] discovery imports cli for EventEntry (recurring multi-round)
- R233B-ARCH-8: dup of #387 [ARCH1 server package]
- R233B-ARCH-9: dup of #626 [R217-ARCH-3] discovery imports cli for EventEntry (recurring multi-round)
- R233B-GO-1: dup of #742 [R238-ARCH-3] runInflight atomic.Pointer
- R233B-PERF-4: dup of #461 [CLI-PERF-1]
- R233B-SEC-2: dup of #382/#877 feishu token-only HMAC

### R234 discarded
- R234-ARCH-1: dup of #457 [ARCH-DISP-1] DispatcherConfig concrete (recurring multi-round)
- R234-ARCH-11: dup of #728 [R222-ARCH-10 ManagedSession semantic tags] (recurring multi-round)
- R234-ARCH-2: dup of #729 [R246-PERF-14 sysession runner subprocess] (recurring multi-round)
- R234-ARCH-23: dup of #459 [ARCH-SVR-1 send-with-broadcast]
- R234-ARCH-3: dup of #720 [R242-ARCH-2 exempt session pool] + #432 [R176-ARCH-N4 ManagedSession state machine] (recurring multi-round)
- R234-ARCH-7: dup of #752 [R238-ARCH-7] (recurring multi-round)
- R234-GO-4: dup of #556 [R242-GO-8] cacheHeadPush O(N)
- R234-GO-8: dup of #796 / #522 diskListNewestFirst mtime
- R234-PERF-14: dup of #527 / #871 warmCache lock
- R234-PERF-15: dup of #865 [R245-PERF-15] agent_tailer 200ms tick
- R234-SEC-2: dup of #691 [R242-CR-3 GET /api/cron 1Hz no rate limiter] (recurring multi-round)
- R234-SEC-3: dup of #691 [R242-CR-3 GET /api/cron 1Hz no rate limiter] (recurring multi-round)
- R234-SEC-5: dup of #461 [CLI-PERF-1]
- R234-SEC-6: dup of #671 [R242-PERF-7 KnownSessionIDs rebuilds map] (recurring multi-round)

### R235 discarded
- R235-ARCH-1: dup of #408 [R214-ARCH-12 backend.Register] (recurring multi-round)
- R235-ARCH-3: dup of #457 [ARCH-DISP-1 DispatcherConfig concrete]
- R235-ARCH-4: dup of #729 [R246-PERF-14 sysession runner subprocess] (recurring multi-round)
- R235-PERF-1: dup of #461 [CLI-PERF-1]
- R235-PERF-7: dup of #644 [R218B-GO-3 readLoop linker.Resolve goroutine no ctx] (recurring multi-round)
- R235-PERF-9: dup of #482/#675/#551 marshalJobsLocked
- R235-SEC-2: dup of #461 [CLI-PERF-1]

