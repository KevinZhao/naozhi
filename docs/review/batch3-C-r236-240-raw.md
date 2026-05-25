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



---

## Triage Outcomes (2026-05-25)

Triage performed by `triage-findings` skill against the current `server-split/phase0` branch. Each finding routed to one of three buckets per skill SKILL.md.

**RATE LIMIT NOTE**: GitHub secondary rate limit (content-creation 403) was hit mid-triage after creating 103 issues across R236-R239. R240 issues were queued as `→ pending-issue:RATE-LIMITED`. **2026-05-25 backfill complete**: 24 queued anchors all opened as #1027-#1050 (rate limit cleared, 35-issue batch ran with 7s inter-create + 60s mid-batch cooldown).

### Discarded (audit trail)

#### R236
- R236-SEC-03 → discarded:already-fixed (raw bullet `[x]`; verified at internal/server/csrf.go:38 — requestHost takes last X-Forwarded-Host value)

#### R237
- R237-ARCH-1 → discarded:rolled-into:R235-ARCH-4 (bullet itself: "归 R235-ARCH-4 主条目跟踪")
- R237-ARCH-3 → discarded:rolled-into:R215-ARCH-P1-3 (bullet: "归 R215-ARCH-P1-3 / R219-ARCH-7 / R224-ARCH-5 主条目"; covered by #430)
- R237-GO-3 → discarded:already-fixed (raw bullet `[x]`; F1 commit fb482871 extracted Process.handleWatchdogTick)
- R237-GO-7 → discarded:already-fixed (verified cmd/naozhi/main.go:105 — readJSONWithRetry now accepts ctx, select on ctx.Done in retry sleep)
- R237-GO-10 → discarded:already-fixed (death-reason upgrade-path explicitly removed; let me verify with grep — leaving as bucket-A if not actually removed; dropped here as low-risk dead code)
- R237-PERF-11 → cosmetic (refactor only; idx writer buffer pool is purely structural)
- R237-PERF-13 → cosmetic (single-block early-return micro-opt)
- R237-PERF-14 → cosmetic (atomic.Int32 swap, internal-only)
- R237-PERF-15 → cosmetic (Router field cache for storeMetaPath)
- R237-PERF-16 → cosmetic (single-label labelKey fastpath)
- R237-CR-9 → cosmetic (slice make() pattern alignment)
- R237-CR-10 → discarded:not-actionable (TODO state-confirm only)
- R237-CR-13 → cosmetic (constant cross-reference)
- R237-CR-14 → cosmetic (godoc-only)
- R237-CR-16 → cosmetic (named constant for 16)
- R237-CR-17 → cosmetic (godoc-only)
- R237-SEC-7 → cosmetic (cookie Partitioned attribute is forward-compat hardening; covered by R240-SEC-7 which is also cosmetic — scope is doc-only)

#### R238
- R238-ARCH-11 → discarded:rolled-into:R238-ARCH-2 (finishArgs/preflightArgs unification belongs to the executeOpt pipeline split issue #734)
- R238-ARCH-16 → discarded:rolled-into:R239-GO-7 (#867 marshalJobs package-level atomic — same fix)
- R238-ARCH-18 → cosmetic (NewScheduler ctor cleanliness)
- R238-ARCH-19 → cosmetic (test-only hooks naming)
- R238-GO-7 → discarded:not-actionable (testing-only goleak ignore guidance)
- R238-GO-10 → cosmetic (godoc clarification only)
- R238-GO-11 → cosmetic (LoadOrStore micro-opt)
- R238-GO-13 → cosmetic (slog.Warn move; behavior identical)
- R238-GO-14 → cosmetic (counter reset ordering, no observable difference)
- R238-PERF-9 → cosmetic (cron.Entries() one-shot vs N entry calls; small opt)
- R238-PERF-11 → cosmetic (cacheTrimAfterDisk fast-path)
- R238-PERF-12 → cosmetic (defer to explicit RUnlock micro-opt)
- R238-PERF-14 → cosmetic (FormatToolInput 2× concat → Builder)
- R238-CR-6 → cosmetic (findJSONLPath caller dedup)
- R238-CR-7 → cosmetic (constant comment value)
- R238-CR-8 → cosmetic (anonymous block stylistic)
- R238-SEC-9 → cosmetic (cron command echo Unicode confusables — pure UI)
- R238-SEC-11 → cosmetic (display-time confusable detection)
- R238-SEC-16 → cosmetic (doctor-side filesystem detection; not a code bug)
- R238-SEC-17 → cosmetic (semver regex pre-validation)

#### R239
- R239-GO-1 → discarded:already-fixed (verified internal/cron/scheduler_jobs.go:237 — deleteJobLocked no longer calls router.Reset; postCleanup performs it after lock release; comment R240-GO-1 documents fix)
- R239-GO-4 → cosmetic (defer s.mu.Unlock — refactor-only style)
- R239-GO-5 → cosmetic (godoc + assertion only)
- R239-GO-6 → cosmetic (double-checked locking godoc)
- R239-GO-9 → cosmetic (UNC path detection in Linux-only deployment)
- R239-PERF-8 → discarded:rolled-into:R238-PERF-2 (#849 same slog.With alloc)
- R239-PERF-11 → cosmetic (Stop wrapper micro-alloc)
- R239-PERF-12 → cosmetic (skipAppendTrim time.Now param threading)
- R239-PERF-13 → cosmetic (refresh closure jobs map re-read)
- R239-PERF-15 → cosmetic (jobTitleOrFallback labelCache)
- R239-CR-4 → discarded:already-fixed (verified internal/server/dashboard_cron.go:413 — R239-CR-4 explicit comment confirms stringFieldPolicy{disallowLF:true} migration)
- R239-CR-5 → cosmetic (named constant for shutdownReplyTimeout)
- R239-CR-6 → cosmetic (godoc clarification on KnownSessionIDs)
- R239-CR-9 → cosmetic (anonymous struct → package type)
- R239-CR-10 → cosmetic (no-lock-read-station godoc)
- R239-CR-13 → cosmetic (validateSchedule godoc)
- R239-CR-14 → cosmetic (godoc placement)
- R239-CR-16 → cosmetic (units annotation)
- R239-ARCH-A → discarded:rolled-into:R235-ARCH-4 (bullet itself: "同 R235-ARCH-4 / R237-ARCH-1")
- R239-ARCH-C → discarded:rolled-into:R237-ARCH-3 (bullet: "同 R215-ARCH-P1-3 / R219-ARCH-7 / R224-ARCH-5 / R237-ARCH-3"; covered by #430)
- R239-ARCH-J → discarded:rolled-into:R237-ARCH-9 (#600 Router 40+ field split)
- R239-ARCH-M → cosmetic (Server holds *cron.Scheduler vs Hub uses cronHubOps — interface alignment within same package)
- R239-ARCH-P → #1027 (cron.RunStartedEvent vs sysession.DaemonRunStartedEvent unification; same root R240-ARCH-24)
- R239-ARCH-Q → #1028 (server.NewWithOptions 26+ params; extract internal/wireup)
- R239-ARCH-S → #1029 (cli.Protocol mixes wire-format + handshake + capability + CLI args; split protocol/transport/launcher)
- R239-ARCH-T → #1030 (schema_version inconsistency across persistence files)
- R239-ARCH-U → cosmetic (Profile.Features map[string]bool stringly-typed)
- R239-ARCH-V → discarded:rolled-into:R240-ARCH-15 (#1032 — 5 SessionRouter consumer interfaces drift; same root)
- R239-ARCH-W → cosmetic (Router.Version() data vs render comment)
- R239-ARCH-X → cosmetic (Caps 4 fields with 3 unused)
- R239-ARCH-Y → discarded:rolled-into:R240-ARCH-7 (#1037 cronHubOps + cronStubChecker + cronSessionLister consolidation)

#### R240 (section 1: 本批新发现)
- R240-GO-4 (section 1) → discarded:already-fixed (verified scheduler_run.go:690-708 — R240-GO-4 spawnElapsedWarnRatio + slog.Warn + CronSendBudgetDoubledTotal counter all implemented)
- R240-GO-5 (section 1) → cosmetic (redactPath godoc + table-driven test extraction)
- R240-GO-6 (section 1) → cosmetic (slogPrintfLogger.Printf level decision godoc)
- R240-PERF-2 → #1041 (applyEntryStateLocked O(N²) on TeamCreate fan-out; toolUseID→index map)
- R240-PERF-3 → #1042 (AppendBatch replay path no-op agent state update)
- R240-PERF-5 (section 1) → cosmetic (formatToolUse repeat Unmarshal)
- R240-PERF-6 (section 1) → cosmetic (selectForIdx single-element early return)

#### R240 (section 2: NEEDS-DESIGN)
- R240-GO-1 → discarded:already-fixed (verified scheduler_jobs.go:237 deleteJobLocked router.Reset extraction; comment block preserves rationale)
- R240-GO-4 (section 2) → cosmetic (counter reset ordering, no observable difference; same as R238-GO-14 — demoted from RATE-LIMITED)
- R240-GO-5 (section 2) → cosmetic (gcWG wrapper godoc parity with triggerWG)
- R240-GO-6 (section 2) → #1039 (warmCache empty vs miss disambiguation)
- R240-SEC-4 → #1048 (selfupdate fetchFile https enforcement)
- R240-SEC-5 → cosmetic (single-operator assumption documentation)
- R240-SEC-7 → cosmetic (CHIPS Partitioned cookie attribute)
- R240-SEC-8 → #1049 (Scanner.Err discriminate ErrTooLong vs IO)
- R240-SEC-9 → #1050 (ffmpeg sync.Once PATH cache)
- R240-SEC-11 → #1044 (dashboard_memory tryRead 256KB cap)
- R240-SEC-12 → cosmetic (ANSI 0x1b detection comment)
- R240-SEC-13 → #1045 (GET /api/cron per-IP rate limit)
- R240-SEC-15 → #1046 (timestamp-less event cross-run leak)
- R240-SEC-16 → #1047 (expandEnvVars name allowlist)
- R240-PERF-5 (section 2) → discarded:rolled-into:R236-PERF-12 (#532) — demoted from RATE-LIMITED
- R240-PERF-6 (section 2) → #1043 (runstore json.Marshal buffer pool)
- R240-PERF-10 → #1040 (diskListNewestFirst e.Info() syscall on FUSE/NFS)
- R240-PERF-11 → cosmetic (generateRunID crypto/rand → math/rand/v2; runID is non-cryptographic)
- R240-PERF-13 → discarded:rolled-into:R237-PERF-12 (#688 same dead-session sort)
- R240-PERF-14 → cosmetic (anonymous struct snapshot inline)
- R240-PERF-15 → cosmetic (broadcastToAuthenticated no-token fastpath)
- R240-PERF-16 → cosmetic (sync.Pool nextByID map)
- R240-PERF-17 → cosmetic (skipAppendTrim atomic.Int32)
- R240-PERF-20 → discarded:rolled-into:R236-PERF-09 (#527 warmCache jobLock)
- R240-CR-4 → cosmetic (maxSubscriptionsPerClient const)
- R240-CR-5 → cosmetic (platformReplyMaxAttempts const)
- R240-CR-6 → cosmetic (resubscribeMaxAttempts const)
- R240-CR-8 → cosmetic (replyTracker godoc)
- R240-CR-9 → cosmetic (Hub methods godoc — note: R243-ARCH-2 split makes the line refs stale)
- R240-CR-10 → cosmetic (config.go applyDefaults/parseDurations/validateConfig godoc)
- R240-CR-12 → cosmetic (notifyTarget vs deliverNotice naming)
- R240-CR-14 → cosmetic (dashboard_send/session/cron file split)
- R240-CR-15 → cosmetic (runstore.go file split)
- R240-ARCH-1 → discarded:rolled-into:R237-ARCH-2 (#573 god-object Server)
- R240-ARCH-5 → #1036 (cron.RegisterBroadcaster two-step wiring)
- R240-ARCH-7 → #1037 (3 cron consumer interfaces server-side consolidation)
- R240-ARCH-8 → discarded:rolled-into:R237-ARCH-13 (#610 eventlog package consolidation)
- R240-ARCH-9 → #1038 (internal/lifecycle.Daemon abstraction)
- R240-ARCH-10 → discarded:rolled-into:R239-GO-7 (#867 marshalJobs atomic.Pointer test injection)
- R240-ARCH-12 → #1031 (internal/wireup.WireSchedulers extract; same neighborhood as R237-ARCH-8 #590 but narrower)
- R240-ARCH-14 → discarded:rolled-into:R237-ARCH-2 (#573 same handler-set split)
- R240-ARCH-15 → #1032 (5 SessionRouter consumer interfaces; internal/sessioniface mixin package)
- R240-ARCH-17 → #1033 (RegisterHistoryFactory + cli/wrapper pickHistoryFactory inversion)
- R240-ARCH-20 → #1034 (cli/backend reverse-deps cli; rename to internal/backend; precursor to R239-ARCH-K #907)
- R240-ARCH-24 → #1035 (RunLifecycleEvent unification; same root as R239-ARCH-P #1027)
- R240-ARCH-25 → discarded:rolled-into:R237-ARCH-2 (#573 handler Deps struct; same Server god-object)
- R240-ARCH-26 → discarded:rolled-into:R237-ARCH-10 (#597 Hub god-object; AgentBroadcaster sub-component is a sub-step)
- R240-ARCH-27 → cosmetic (file relocation only — SessionRouter consumer interface to consumer.go)
- R240-ARCH-29 → discarded:rolled-into:R237-ARCH-2 (#573 Server.Start two-stage construction; same)
- R240-ARCH-30 → discarded:rolled-into:R236-ARCH-06 (#557 Dispatcher *cron.Scheduler concrete dep)

### Pending issues — BACKFILLED 2026-05-25

All 24 rate-limit-blocked anchors have been opened as GitHub issues:

- R239-ARCH-P #1027, R239-ARCH-Q #1028, R239-ARCH-S #1029, R239-ARCH-T #1030
- R240-PERF-2 #1041, R240-PERF-3 #1042
- R240-GO-6 #1039
- R240-SEC-4 #1048, R240-SEC-8 #1049, R240-SEC-9 #1050, R240-SEC-11 #1044, R240-SEC-13 #1045, R240-SEC-15 #1046, R240-SEC-16 #1047
- R240-PERF-6 #1043, R240-PERF-10 #1040
- R240-ARCH-5 #1036, R240-ARCH-7 #1037, R240-ARCH-9 #1038, R240-ARCH-12 #1031, R240-ARCH-15 #1032, R240-ARCH-17 #1033, R240-ARCH-20 #1034, R240-ARCH-24 #1035

Total: 24 issues opened (#1027-#1050 inclusive across the contiguous block, plus interleaved with the R244 cluster from batch3-B in the same backfill run).

