# Cosmetic Backlog

> godoc / 命名 / 注释 / 纯结构调整建议的归档。无运行时影响的 review finding 进这里，不开 issue。
>
> 来源：`triage-findings` skill 自动 append。
>
> 处理频率：每 1-2 个月由人工批量扫一次决定是否值得一个清理 PR。

## 格式

```
- [R{ROUND}-{CAT}-{IDX}] <one-line summary> — <file>:<line>
```

## Entries

<!-- triage-findings skill 在此 append；新条目加到末尾 -->

- [R248-ARCH-5] AgentLinker interface 放 consumer-local (server pkg) 而非 session/agentlink — internal/session/agentlink/agentlink.go:1
- [R248-ARCH-7] SetScheduler/SetUploadStore/SetScratchPool 搬到 wshub_send.go 或新建 wshub_lifecycle.go — internal/server/wshub.go:369-378
- [R248-CR-5] AgentLinker.Query → Lookup, QueryOrResolveFast → Resolve — internal/session/agentlink/agentlink.go:34-40
- [R248-CR-7] handleAgentSubscribe 并入 wshub_subscribe.go (与 ValidateSessionKey 入口模式对齐) — internal/server/wshub_agent.go:127
<!-- CURATED-NAMING-1 已改提为 GitHub Issue (caller-visible API rename, 125+ call sites,
不符合 "zero functional impact" 标准；按 skill rule "godoc-AND-callers → A" 归 issue)。 -->
- [R247-GO-15] saveMarshaledSeq lastSavedSeq update godoc — internal/cron/scheduler_persist.go:101-135
- [R247-GO-16] spawn budget warn threshold jobTimeout/2 noise; comment-only adjustment — internal/cron/scheduler_run.go:606-613
- [R247-GO-17] cacheGet R241-CR-6 godoc invalidated by R247-GO-6 race; cross-reference — internal/cron/runstore.go:449-489
- [R247-CR-6] opts.ExtraArgs three-arg slice pure-defensive comment annotation — internal/cron/scheduler_run.go:485-489
- [R247-CR-8] *ByID three-entry godoc asymmetry (Pause/Resume don't delete runs) — internal/cron/scheduler_jobs.go:273-388
- [R247-CR-25] historical review-number comment cleanup (40+ sites) — internal/cron/scheduler_run.go,scheduler.go
- [R247-CR-27] Append truncate three-field comment asymmetry — internal/cron/runstore.go:280-339
- [R245-CR-008] orphan TODO referencing R239-CR-11 not in TODO.md — internal/session/managed.go:1342
- [R246-GO-22] applyJitter NewTimer/Stop comment-only future-proof — internal/cron/scheduler_run.go:721-747
- [R246-GO-21] applyDefaults pointer receiver micro-cleanup — internal/cron/scheduler.go:387-406
- [R245-PERF-12] eventlog persister handleBatch json.Encoder pool cleanup — internal/eventlog/persist/persister.go:670-711
- [R245-PERF-18] shimLineReader.ReadLine alloc verification (-gcflags=-m) — internal/cli/wrapper.go:482-499
- [R246-PERF-10] upstream connector marshalResult json.Encoder pool — internal/upstream/connector.go:331-333


<!-- batch3-D r225-r235 cosmetic-backlog appends (2026-05-25) -->
- [R226-ARCH-12] 错误→用户中文消息没注册式映射 — ?
- [R226-ARCH-14] `cli` 包同时含 protocol/process/eventlog/history/shim io/image/thumbnail — ?
- [R226-ARCH-16] panic recovery 在 dispatch/platform/cli/session 四处独立写 — ?
- [R226-ARCH-5] Platform 包间多份"形似独立"实现 — ?
- [R226-ARCH-6] `workspace`/`workspaces`/`Session.cwd` 别名 + 无热重载 — ?
- [R226-ARCH-7] 状态分散到 7 处 store — ?
- [R226-CR-7] `RegisterForResume` / `RegisterCronStubWithChain` 用 `r.CLIName/Version` 在多 backend 部署下显示错误 — internal/session/router_discovery.go
- [R227-ARCH-10] Protocol 能力查询走 SupportsX 与 Capabilities() Caps 双轨 — ?
- [R227-ARCH-11] cli 包 import metrics 14 处隐式全局 — ?
- [R227-ARCH-14] dispatch 与 platform 之间 type assertion 探测能力 — ?
- [R227-ARCH-15] `eventlog_bridge` 在 cli↔persist 中介中做 EventEntry→Entry marshal — ?
- [R227-ARCH-18] contract test 边界三类含义混用 — ?
- [R227-ARCH-20] `dispatch.passthroughCtxKey/urgentCtxKey` 用 ctx.Value 跨包传 boolean — ?
- [R227-ARCH-3] `history.Source` 与 `cli.HistorySource` 双接口结构同形 — ?
- [R227-ARCH-7] sessions.json sidecar / EventLog per-record / shim state inline / cron 各一套 schema 版本机制 — ?
- [R227-ARCH-8] 4 个 platform adapter 各自 httpClient/hookSem/dedup/SanitizeForLog — ?
- [R227-ARCH-9] `cli` 包反向依赖 history 概念，HistorySessionView 唯一实现是 *session.ManagedSession — ?
- [R227-PERF-17] `shim.ServerMsg.MarshalLine` 每次 json.Marshal alloc — ?
- [R227-PERF-18] `eventPushLoop` EventEntriesSince per-goroutine 独立 slice — ?
- [R227-PERF-5] `WriteUserMessageLocked` json.Marshal encodeState alloc — ?
- [R227-PERF-6] `Cleanup` + `saveIfDirty` 每次写锁内 map clone 3 份 — ?
- [R227-TEST-2] `cli.detectVersion` 用 context.Background() — ?
- [R228-ARCH-18] `dispatch.Dispatcher.projectMgr` 仅用于 slash-command UX 但持整个 `*project.Manager` — internal/dispatch/dispatch.go:56
- [R228-ARCH-8] 4 个 platform adapter 各自 `var fooHTTPClient` SSRF-defense client — ?
- [R229-ARCH-11] Send 路径 Dashboard/IM/Cron 三流水线规则不同 — ?
- [R229-ARCH-12] shim 边界在 cli.Wrapper / cli.Process / session/router_shim 三处反复横跳 — ?
- [R229-ARCH-13] agents 字典向 6 个组件并行传递 — ?
- [R229-ARCH-14] Server.New + NewWithOptions 双构造器仍并存 — ?
- [R229-ARCH-15] Process.Send / SendPassthrough 双路径不变量难维护 — ?
- [R229-ARCH-17] KnownIDs/SessionIDToKey/Sessions/Workspace/Backend Overrides 5 个 map 同步维护 — ?
- [R229-ARCH-18] discovery/history/eventlog 三套读历史路径 — ?
- [R229-ARCH-5] KeyResolver/server/dashboard 双路径未收拢 — ?
- [R229-ARCH-6] Channel Adapter 能力鸭子类型散落 — ?
- [R229-ARCH-7] main.go 持 settings.json 重写 / hooks 过滤 / env 过滤业务逻辑 — ?
- [R229-PERF-6] discovery.Scan 每次 O(N) os.ReadDir 调用 — ?
- [R230-ARCH-10] plannerKey* 在 session/key.go 与 project 包重复 — ?
- [R230-ARCH-16] Router.spawnSession 直接调 *cli.Wrapper.Spawn — ?
- [R230-ARCH-19] Cron / Sysession / dashboard 各自一套 stub session 注册策略 — ?
- [R230-ARCH-20] Server ↔ Hub 共享 nodesMu *sync.RWMutex 别名 — ?
- [R230-ARCH-7] 错误→用户消息映射有 3 处偏序 — ?
- [R230B-ARCH-10] server / dispatch / cron 三处持 `map[string]platform.Platform` — ?
- [R230B-ARCH-16] Dashboard 装配顺序硬编码多处 — internal/app/wire.go
- [R230B-ARCH-19] validateStringField 三重扫描（UTF-8+C0+Bidi）重复 — ?
- [R230B-ARCH-20] node 反向 RPC 协议三处定义 — ?
- [R230B-ARCH-23] selfupdate 无回滚 / 健康检查 hook — ?
- [R230B-ARCH-7] `server` 包 backend fan-out 失控 — ?
- [R230B-GO-3] `recordResultP0WithSanitised` / `recordResult` mu Unlock 非 deferred — ?
- [R230B-PERF-6] `eventlog_bridge` 单条快路径仍 copy raw bytes — ?
- [R230C-ARCH-14] 文件化状态多实例并发写无 flock — ?
- [R230C-ARCH-15] main.go ~390 行 backend-specific settings.json 重写 — ?
- [R230C-ARCH-9] Platform caps 抽象不到位 — ?
- [R232-ARCH-11] NotifyPolicy 隐式三态 — ?
- [R232-ARCH-5] 28 个 contract test 用 os.ReadFile + 字符串/正则 pin 源代码 — ?
- [R232-ARCH-6] 5 个独立 *Router 消费者接口 + 2 个临时 cronStubChecker/cronSessionLister — ?
- [R233-PERF-6] readRun 每个 .json 文件 os.ReadFile cold path — ?
- [R233B-ARCH-5] cli.HistorySource vs history.Source 名存实亡 interface — ?
- [R233B-ARCH-7] config 反向 import internal/session 拿 AgentOpts — ?
- [R234-ARCH-10] dispatch ↔ platform 双向：platform webhook 回调 leak dispatch.IncomingMessage / Reply 内部类型 — ?
- [R234-ARCH-12] Protocol/Sink/Tailer 三大事件接口契约未文档化 — ?
- [R234-ARCH-13] server/agent_tailer 直 import cli，IO 路径绕过 router — ?
- [R234-ARCH-14] buildServer 初始化顺序 fragile — ?
- [R234-ARCH-16] server/discovery_cache.go 越层依赖 project + cli — ?
- [R234-ARCH-17] dispatch.consumer.go 引用 *session.ManagedSession 而非更窄 SessionHandle — ?
- [R234-ARCH-20] wshub_agent.go 仅依赖 session 但被并入 server 包导致编译图污染 — ?
- [R234-ARCH-21] 跨包共享 limits 常量风格不一（cron/limits.go vs server/server.go vs dispatch/dispatch.go — ?
- [R234-ARCH-22] `internal/cli/backend` 同时被 server 与 session 直接 import — ?
- [R234-ARCH-24] server/upload_store.go import cli 仅为 cli.ImageData — ?
- [R234-ARCH-25] session/eventlog_bridge.go 是事实上的 fan-out hub 但命名 bridge — ?
- [R234-ARCH-4] `internal/session/contract_test.go` 反向 import dispatch+server+cron+upstream — internal/session/contract_test.go
- [R234-ARCH-6] Shutdown 顺序无形式化合约 — ?
- [R234-ARCH-9] Channel adapter 越权读 router 状态：dashboard_session.go / dashboard_discovered.go 直 import cli+session — ?
- [R234-CR-1] runstore.truncateForRetry 与 scheduler.sanitiseRunResult 共享 truncate-with-suffix 但分散两文件 — ?
- [R234-CR-2] workDirUnderRoot/workDirReachable 在 cron + server 各一份 — ?
- [R234-CR-3] generateID/generateRunID 都委派 generateHexID 三个名字一个函数 — ?
- [R234-CR-4] DeleteJob/PauseJob/ResumeJob ByPrefix vs ByID 6 方法 lock/persist/save 模板重复 — ?
- [R234-CR-5] `var stopBudget` package-level mutable global 仅为测试注入 — ?
- [R234-CR-6] finishRun 双 sanitise 层级不透明 — ?
- [R234-GO-10] runstore.trimJobLocked sort 用 cmp.Compare(UnixNano) 而非 time.Compare — ?
- [R235-ARCH-2] cron / sysession 各自定义 RunState / TriggerKind / ErrorClass — ?
- [R235-CR-14] `validateSchedule` 与 `PreviewSchedule` 重复 cronParser.Parse + 两次 sched.Next — ?
- [R235-CR-15] diskListNewestFirst 同秒 mtime tie-break 用 runID（hex）不反映时间顺序 — ?
- [R235-CR-4] `XxxByID`/`Xxx` 6 对方法 60 行重复（DeleteJob/PauseJob/ResumeJob × ByID/byPrefix — ?
- [R235-CR-7] `emitRunEnded` 无 godoc 与 emitRunStarted 不对称 — ?
- [R235-CR-9] `skipAppendTrim` 三处 appendsSinceTrim=0 重置语义不齐 — ?
- [R235-GO-3] `cron.Stop()` 包装 goroutine 在 deadline-hit 路径永久泄漏 — ?

<!-- batch3-B r241-r244 cosmetic-backlog appends (2026-05-25) -->
- [R242-GO-15] streamFromBuffer break+Writer.Close intent comment — internal/transcribe/transcribe.go:107
- [R242-GO-17] runstore.readRun Lstat-as-symlink-defense comment sharpening — internal/cron/runstore.go:586
- [R242-GO-18] Dispatcher.agents/agentCommands immutable contract godoc — internal/dispatch/dispatch.go:100
- [R242-GO-20] KnownSessionIDs jobIDs slice race window godoc — internal/cron/scheduler_jobs.go (KnownSessionIDs)
- [R242-CR-12] cron scheduler stale "(line ~1864)" comment rot — internal/cron/scheduler.go:206-212
- [R244-GO-P1-1] dashboard_cron_transcript LimitedReader.N comment precision — internal/server/dashboard_cron_transcript.go:442
- [R244-GO-P3-1] fmt.Errorf %w:%w double-wrap pattern godoc — internal/cron/scheduler.go:3200
- [R244-GO-P3-2] parseISO8601MS godoc cite RFC3339Nano vs RFC3339 — internal/server/dashboard_cron_transcript.go:696
- [R244-GO-P2-4] TestSanitizeForLog_EnforcesMaxLen mix flat + table-driven test style — internal/osutil/loginject_test.go
- [R237-PERF-11] eventlog/persist/idx.AppendBatch buffer pool — internal/eventlog/persist/idx.go
- [R237-PERF-13] discovery extractText single-block early return — internal/discovery/scanner.go
- [R237-PERF-14] runstore.skipAppendTrim atomic.Int32 — internal/cron/runstore.go:268-308
- [R237-PERF-15] session.storeMetaPath Router field cache — internal/session/managed.go
- [R237-PERF-16] metrics labelKey single-label fastpath — internal/metrics/metrics.go
- [R237-CR-9] project.UnbindAllChat slice make() pattern — internal/project/project.go
- [R237-CR-13] RingBuffer.defaultRingMaxLines vs ManagerConfig.BufferSize cross-ref constant — internal/shim/manager.go + buffer.go
- [R237-CR-14] Project.snapshotLight ChatBindings=nil godoc — internal/project/project.go
- [R237-CR-16] connector_conn.go reqSem capacity 16 named const — internal/upstream/connector_conn.go
- [R237-CR-17] cli.editLoop rateTimer godoc clarification — internal/cli/process.go
- [R237-SEC-7] auth cookie Partitioned (CHIPS) attribute — internal/server/dashboard_auth.go
- [R238-ARCH-18] NewScheduler ctor mixes 4 concerns (logging/applyDefaults/EvalSymlinks/validate-deferred) — internal/cron/scheduler.go
- [R238-ARCH-19] testing-only stopBudget/marshalJobs as production var — internal/cron/scheduler.go
- [R238-GO-10] SchedulerConfig.ExecTimeout godoc add ~2×ExecTimeout worst-case — internal/cron/scheduler.go
- [R238-GO-11] runstore.warmCache LoadOrStore micro-opt — internal/cron/runstore.go
- [R238-GO-13] applyDefaults slog.Warn move to NewScheduler — internal/cron/scheduler.go
- [R238-GO-14] runstore.skipAppendTrim reset position — internal/cron/runstore.go
- [R238-PERF-9] ListAllJobsWithNextRun cron.Entries() one-shot — internal/cron/scheduler.go
- [R238-PERF-11] runstore.cacheTrimAfterDisk fast-path — internal/cron/runstore.go
- [R238-PERF-12] session.GetSession defer→explicit RUnlock micro-opt — internal/session/managed.go
- [R238-PERF-14] cli/process_event_format.FormatToolInput Builder — internal/cli/process_event_format.go
- [R238-CR-6] discovery.scanner findJSONLPath caller dedup — internal/discovery/scanner.go
- [R238-CR-7] discovery.scanner.noJSONLGrace 5s rationale comment — internal/discovery/scanner.go
- [R238-CR-8] server.go SetOnKeyRetired anonymous block stylistic — internal/server/server.go:725-731
- [R238-SEC-9] /cron add Unicode confusables echo (UI display) — internal/dispatch/commands.go:412
- [R238-SEC-11] dashboard cookie/notify_chat_id confusable display detection — internal/server/dashboard_auth.go
- [R238-SEC-16] diskListNewestFirst overlayfs/FUSE mtime caveat doctor warning — internal/cron/runstore.go
- [R238-SEC-17] selfupdate tag semver pre-validation — internal/selfupdate/selfupdate.go
- [R239-GO-4] UpdateJob defer s.mu.Unlock refactor — internal/cron/scheduler.go:1377-1472
- [R239-GO-5] runstore.skipAppendTrim caller-contract godoc — internal/cron/runstore.go:249-290
- [R239-GO-6] runstore.cacheGet double-checked-locking godoc — internal/cron/runstore.go:346-392
- [R239-GO-9] redactPathsInCronError UNC path detection note — internal/cron/scheduler.go:2829-2898
- [R239-PERF-11] cron.Stop gcWG drain ctx-aware refactor — internal/cron/scheduler.go:936-955
- [R239-PERF-12] runstore.skipAppendTrim time.Now param threading — internal/cron/runstore.go
- [R239-PERF-13] freshContextPreflightP0 refresh closure jobs map re-read — internal/cron/scheduler.go:1975-1995
- [R239-PERF-15] snapshotJob jobTitleOrFallback labelCache — internal/cron/scheduler.go:1864-1885
- [R239-CR-5] dispatch.Shutdown 5s timeout named constant — internal/dispatch/dispatch.go:667
- [R239-CR-6] cron.IsExcluded godoc batch-caller note — internal/cron/scheduler.go:450-456
- [R239-CR-9] runDetailView anonymous struct → cronRunDetailView package type — internal/server/dashboard_cron.go:1257-1273
- [R239-CR-10] platforms/agents/agentCommands no-lock-station godoc — internal/cron/scheduler.go:205-209
- [R239-CR-13] validateSchedule godoc error contract — internal/cron/job.go:381-400
- [R239-CR-14] replyTracker godoc placement — internal/dispatch/dispatch.go:856-857
- [R239-CR-16] runstore.appendTrimBatch=10 units annotation — internal/cron/runstore.go:296-298
- [R239-ARCH-M] Server *cron.Scheduler vs Hub cronHubOps interface alignment — internal/server/server.go:55
- [R239-ARCH-U] Profile.Features map[string]bool stringly-typed — internal/cli/backend/profile.go:114
- [R239-ARCH-W] Router.Version() data vs render comment — internal/session/router_core.go:1198-1221
- [R239-ARCH-X] Caps 3-of-4 fields unused — internal/cli/protocol.go:158-178
- [R240-GO-5 sec1] redactPath table-driven test extraction — internal/cron/scheduler.go:~2864
- [R240-GO-6 sec1] slogPrintfLogger.Printf level decision godoc — internal/cron/scheduler.go:~3153
- [R240-PERF-5 sec1] formatToolUse repeat Unmarshal — internal/dispatch/status.go:79-122
- [R240-PERF-6 sec1] selectForIdx single-element early return — internal/eventlog/persist/persister.go:967-998
- [R240-GO-4 sec2] skipAppendTrim counter ordering (dup of R238-GO-14) — internal/cron/runstore.go:268-309
- [R240-GO-5 sec2] gcWG wrapper godoc parity with triggerWG — internal/cron/scheduler.go:952-963
- [R240-SEC-5] cronJobView NotifyChatID/NotifyPlatform single-operator doc — internal/server/dashboard_cron.go:247-249
- [R240-SEC-7] naozhi_auth cookie CHIPS Partitioned attribute — internal/server/dashboard_auth.go:338
- [R240-SEC-12] ANSI 0x1b detection comment — internal/server/dashboard_cron_transcript.go:83
- [R240-PERF-11] generateRunID crypto/rand → math/rand/v2 — internal/cron/scheduler.go:2153,2673
- [R240-PERF-14] recordResultP0WithSanitised value-copy snapshot — internal/cron/scheduler.go:2802-2810
- [R240-PERF-15] broadcastToAuthenticated no-token fastpath — internal/server/wshub.go:1363-1398
- [R240-PERF-16] ListAllJobsWithNextRun sync.Pool nextByID — internal/cron/scheduler.go:1196-1218
- [R240-PERF-17] skipAppendTrim atomic.Int32 — internal/cron/runstore.go:268-308
- [R240-CR-4] maxSubscriptionsPerClient const — internal/server/wshub.go:622
- [R240-CR-5] platformReplyMaxAttempts const — internal/dispatch/dispatch.go:742,869
- [R240-CR-6] resubscribeMaxAttempts/resubscribeInterval const — internal/server/wshub.go:1217
- [R240-CR-8] replyTracker 4 methods godoc — internal/dispatch/dispatch.go:956+
- [R240-CR-9] Hub methods godoc (R243-ARCH-2 split: line refs stale) — internal/server/wshub_*.go
- [R240-CR-10] config.go applyDefaults/parseDurations/validateConfig godoc — internal/config/config.go:477,551,584,1092
- [R240-CR-12] notifyTarget vs deliverNotice naming — internal/cron/scheduler.go:2943
- [R240-CR-14] dashboard_send/session/cron file split — internal/server/dashboard_*.go
- [R240-CR-15] runstore.go file split — internal/cron/runstore.go
- [R240-ARCH-27] SessionRouter consumer interface relocate to consumer.go — internal/cron/scheduler.go:65-91
