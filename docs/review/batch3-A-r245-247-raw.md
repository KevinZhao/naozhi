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
- [x] **R247-GO-7 — Stop() gcWG.Wait wrapper goroutine 漏写 contract（P1）** [REPEAT-2]: `internal/cron/scheduler.go:694-705` 与 triggerWG 同 leak 模式但 CONTRACT 块只覆盖 triggerWG。方案：扩展同一 CONTRACT 注释或让 trimAll 观测 stopCtx。 *(已实施：Stop() CONTRACT 段加 gcWG.Wait wrapper goroutine 同 triggerWG 同 intentional-orphan 语义说明；R44 + R247-GO-7 anchor 共存。)*
- [ ] **R247-GO-8 — Append path-traversal 防御深度（P2）** [BREAKING-LOCAL]: `internal/cron/runstore.go:282-339` IsValidID 已防 hex-only 输入，但写路径无 filepath.Rel 校验。方案：MkdirAll 前 `filepath.Rel(s.root, dir)` 校验，与 readRun Lstat 对齐。
- [ ] **R247-GO-10 — EnsureStub 总返 true 不反映 register 失败（P2）** [REFACTOR]: `internal/cron/scheduler.go:617-643` registerStubByValue 不返 error，调用方误以为 stub 已注册。方案：让 RegisterCronStubWithChain 返 error 一路上抛。
- [~] **R247-GO-11 — resetRouterStub 缺 nil-receiver 防御（P2）** [BREAKING-LOCAL]: `internal/cron/scheduler.go:783-788` 与 sibling StartedAt/KnownSessionIDs 不一致；测试构造部分 Scheduler 调 DeleteJobByID 会 NPE。方案：开头 `if s == nil { return }`。
- [ ] **R247-GO-12 — runDeadlineWatchdog 每 tick spawn goroutine（P2）** [REFACTOR]: `internal/cron/scheduler_run.go:621-630` 50 jobs × 1Hz = 50 goroutine/秒 启停，99% case 无效。方案：高 jobTimeout 跳过 watchdog 或共享 watchdog goroutine。
- [ ] **R247-GO-13 — AddJob 10 collision retry 日志 flood（P3）** [BREAKING-LOCAL]: `internal/cron/scheduler_jobs.go:88-109` mock generator 死循环时刷 10 行 log。方案：仅 i==0 记 Warn 或检测确定性 generator 提前 bail。
- [ ] **R247-GO-15 — saveMarshaledSeq 失败时 lastSavedSeq 未更新注释缺失（P3）** [BREAKING-LOCAL]: `internal/cron/scheduler_persist.go:101-135` 当前正确但缺 godoc 解释。方案：注释解释为何不 bump。
- [ ] **R247-GO-16 — spawn budget 警告阈值 jobTimeout/2 噪音（P3）** [BREAKING-LOCAL]: `internal/cron/scheduler_run.go:606-613` cold-start fresh-context preflight 触发误报。方案：阈值 → jobTimeout 或区分 fresh vs hot。
- [ ] **R247-GO-17 — cacheGet R241-CR-6 注释失效（P3）** [REPEAT-2]: `internal/cron/runstore.go:449-489` 注释说 "always sets warm=true" 但 R247-GO-6 race 后不成立。方案：与 R247-GO-6 一并修或更新注释。

### 安全（剩余）

- [ ] **R247-SEC-4 — agent_view.js inline onclick attr 嵌 escAttr（P1）** [REFACTOR]: `internal/server/static/agent_view.js:69` `onclick="...switchTo(\\'" + escAttr(a.taskId) + "\\')"` —— escAttr 仅 HTML-escape 不处理 JS string；当前 a.taskId 由 server `agentTaskIDRe ^[a-z0-9]{1,32}$` 兜底但 sink 错误层。方案：addEventListener + dataset，移除 inline onclick。
- [ ] **R247-SEC-5 — selfupdate.fetchFile 初始 URL 无显式 https 断言（P2）** [REFACTOR]: `internal/selfupdate/selfupdate.go:313` 仅 CheckRedirect 内卡 https。方案：req.URL.Scheme!="https" 早拒，与 redirect 路径对齐。
- [ ] **R247-SEC-6 — transcribe ffmpeg 无 wall-clock 上限（P2）** [BREAKING-LOCAL]: `internal/transcribe/convert.go:104` 仅靠外层 ctx；构造 audio 可长时间占 transcribeSemCap=3 槽。方案：argv 加 `-t 600` 解码上限。
- [ ] **R247-SEC-8 — uploadOwner crypto/rand 失败回退 clientIP（P2）** [REPEAT-2]: `internal/server/dashboard_send.go:140-148` 与 R246-SEC-8 同根因不同 site。方案：失败返 503。
- [ ] **R247-SEC-10 — isSensitiveDownloadName 不卡父目录段（P2）** [BREAKING-LOCAL]: `internal/server/project_files.go:1212` `secrets/db.yaml` `.ssh/foo` 不命中 basename。方案：sensitivePathSegments allowlist。
- [ ] **R247-SEC-11 — parseAttachmentFile 用 declared Content-Type 决定 size cap（P2）** [REPEAT-2]: `internal/server/dashboard_send.go:172` 与 R246-SEC-9 同根因不同 fork；io.ReadAll 在 magic-byte 复核前已 buffer 32MB。方案：magic-byte 优先读 head 决定 cap。
- [ ] **R247-SEC-12 — eventlog persister MkdirAll 0700 不修复 existing dir mode（P2）** [REFACTOR]: `internal/eventlog/persist/persister.go:202` 攻击者预创建 0755/0777 父目录可绕过 0700 contract。方案：MkdirAll 后 os.Chmod 校正或 Lstat 检查 mode != 0700 拒启。
- [ ] **R247-SEC-13 — cron runstore MkdirAll 0700 同 mode 漏修（P2）** [REFACTOR]: `internal/cron/runstore.go:236-301` 同上。
- [ ] **R247-SEC-14 — attachment store MkdirAll 0700 同 mode 漏修（P2）** [REFACTOR]: `internal/attachment/store.go:232` workspace 共享场景下他 UID 可预创建。
- [ ] **R247-SEC-15 — mintAnonCookie 30d MaxAge 永不轮换（P2）** [BREAKING-LOCAL]: `internal/server/dashboard_send.go:39-52` token 模式切换 / 服务重启不清。方案：缩到 7d 或登录态变化时强制 expire。
- [~] **R247-SEC-17 — cookieMAC 缺 cookie-rotation（P2）** [REPEAT-3]: `internal/server/dashboard_auth.go:117-121` 与 R243-SEC-13/R245-SEC-2/R242-SEC-5 同根因；HMAC 输入未含 ts/nonce。方案：MAC 输入加 cookie-gen ts。
- [ ] **R247-SEC-18 — collectTranscripts 直拼 alternative.Transcript（P3）** [REFACTOR]: `internal/transcribe/transcribe.go:194` 未过 SanitizeForLog/IsLogInjectionRune。方案：与 cron sanitiseRunResult 对齐过滤 bidi/C1。
- [~] **R247-SEC-21 — cliAvailable os.Stat 暴露二进制路径（P3）** [REFACTOR]: `internal/server/health.go:283-289` 认证后 token 窃取者可探主机布局。方案：返常量 boolean 不区分 IO 类型。
- [ ] **R247-SEC-22 — reverseUpgrader.CheckOrigin 仅靠 Origin 缺失判 m2m（P3）** [REPEAT-3]: `internal/node/reverseserver.go:69-73` 反代剥 Origin 场景下 browser-XSS 端可凑无 Origin 请求。方案：r.TLS != nil 强制或 explicit insecure_node 配置。
- [ ] **R247-SEC-23 — CSP font-src https://cdn.jsdelivr.net 无 SRI（P3）** [REPEAT-3]: `internal/server/dashboard.go:503` 与 R246-SEC-10 同根因；KaTeX woff2 走同信任链。方案：vendored //go:embed 或 require-sri-for font。
- [~] **R247-SEC-24 — resume key var rb [8]byte 64-bit 熵（P3）** [REPEAT-2]: `internal/server/dashboard_session.go:1052` 与 R246-SEC-5 同根因不同 site。方案：16 字节。
- [ ] **R247-SEC-25 — netutil clientIP trustedProxy XFF 缺失回退 RemoteAddr（P3）** [REPEAT-3]: `internal/netutil/clientip.go:25-46` 所有 client 折一桶。方案：trustedProxy=true 且 XFF 空时返 400。

### 性能（剩余）

- [ ] **R247-PERF-3 — KnownSessionIDs 每次重建 jobs×200 map（P1）** [REPEAT-2]: `internal/cron/scheduler_session.go:68-108` 与 R245-PERF-2/R242-PERF-7 同根因仍未消除。方案：atomic.Pointer[snapshot] + 30s TTL，finishRun/DeleteJob 主动失效。
- [ ] **R247-PERF-4 — ListAllJobsWithNextRun 每次 4 个 slice/map alloc（P1）** [REFACTOR]: `internal/cron/scheduler_jobs.go:184-213` dashboard 1Hz poll。方案：sync.Pool 复用 pairs/result，maps.Clear+复用 nextByID。
- [ ] **R247-PERF-5 — proc_linux fmt.Sprintf("/proc/%d/...") + strings.Fields（P1）** [REFACTOR]: `internal/discovery/proc_linux.go:27,39,56` 每 PID 反射拼接 + 整 string copy。方案：strconv.Itoa builder + byte-level scan。
- [ ] **R247-PERF-6 — handleList 每个 project alloc map[string]any 8 字段（P1）** [REFACTOR]: `internal/server/project_api.go:89-134` dashboard 多 tab。方案：命名 struct 化，redactGitRemoteURL 缓存到 *Project 字段。
- [ ] **R247-PERF-7 — replyText fmt.Sprintf "[Cron %s] %s"（P2）** [REFACTOR]: `internal/cron/scheduler_run.go:709` 已大字符串再拼 reflect format。方案：strings.Builder。
- [ ] **R247-PERF-8 — deliverNotice "[Cron %s]" 三处 fmt.Sprintf（P2）** [REFACTOR]: `internal/cron/scheduler_run.go:254,567,671`。方案：抽 helper + Builder。
- [ ] **R247-PERF-9 — diskListNewestFirst 串行 readRun（P2）** [REPEAT-3]: `internal/cron/runstore.go:617-631` 与 R246-PERF-2 同根因。方案：recentCacheEntry 缓存 sorted slice + 8 worker pool 并行解码。
- [ ] **R247-PERF-10 — Append jobLock 期间 MkdirAll+Marshal+WriteFileAtomic（P2）** [REPEAT-2]: `internal/cron/runstore.go:282-339` 每条 syscall。方案：json.Marshal pool + sync.Once-per-jobID MkdirAll。
- [ ] **R247-PERF-11 — marshalJobsLocked 每次 mutation 全表 sort+Marshal（P2）** [REPEAT-2]: `internal/cron/scheduler_persist.go:45-58` 50 jobs ≈ 100KB write × 每 finishRun。方案：Encoder + bytes.Buffer pool。
- [ ] **R247-PERF-14 — eventlog Subscribe 每次 alloc subscriber（P2）** [REFACTOR]: `internal/cli/eventlog.go:1141-1166` dashboard 高频 reconnect 形成分配峰。方案：close-once 限制改 broadcast cond。
- [ ] **R247-PERF-16 — RecentSessions 无 prealloc（P2）** [REFACTOR]: `internal/discovery/recent.go:84-178` 7day×多 project 规模可观。方案：make 估上限。
- [ ] **R247-PERF-17 — protocol_acp base64.EncodeToString 全 alloc（P2）** [REFACTOR]: `internal/cli/protocol_acp.go:322-339` 多图 turn 浪费。方案：base64.StdEncoding.AppendEncode 写 pre-grown buffer。
- [ ] **R247-PERF-19 — recentFromParsedIndex jsonlMtimes map 重建（P2）** [REFACTOR]: `internal/discovery/recent.go:329-356` 已 sorted slice 可二分。方案：sort.Search 替 map。
- [~] **R247-PERF-20 — Tick highwater 全量拷贝（P2）** [REFACTOR]: `internal/sysession/auto_titler.go:181-194` 多数 key 当 tick 不访问。方案：atomic.Pointer[map] CoW。
- [ ] **R247-PERF-21 — buildUserEntry 每图 spawn goroutine（P3）** [REFACTOR]: `internal/cli/process_send.go:51-76` cap 4 sem 但仍 8KB stack × N。方案：worker pool。
- [ ] **R247-PERF-23 — Enqueue 队列满 O(N) memmove（P3）** [REPEAT-2]: `internal/dispatch/msgqueue.go:184-208` MaxDepth=16 拷贝 15。方案：环形 buffer。
- [ ] **R247-PERF-24 — workDirUnderRoot 每 execute EvalSymlinks（P3）** [REFACTOR]: `internal/cron/scheduler.go:177-189` 长寿命下重复 syscall。方案：TTL 缓存。
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
- [~] **R247-CR-10 — registerJob AddFunc closure 与 executeIfNotDeletedOrPaused 同源（P2）** [REPEAT-3]: `internal/cron/scheduler_jobs.go:843-862`。方案：closure 直调 executeIfNotDeletedOrPaused。
- [x] **R247-CR-11 — strHeap/timeHeap helper reset 路径反成噪音（P2）** [REFACTOR]: `internal/cron/runinflight.go:64-74,91-95` 与 R246-CR-011 同根因；本轮新发现 reset 不分配。方案：删 helper 或 cross-reference。 *(已实施：strHeap → boxString / timeHeap → boxTime；godoc 跟改但保留 escape analysis 历史；11 处调用点 internal/cron/runinflight.go + internal/cron/scheduler_run.go 全量切换。R246-CR-011 同根因。)*
- [ ] **R247-CR-12 — runs 限额 const 散落（P2）** [REFACTOR]: `internal/cron/runstore.go:170-187` 大小写不统一。方案：集中到 limits.go。
- [ ] **R247-CR-14 — recordResultP0WithSanitised 64 行 P0 命名已无意义（P2）** [REFACTOR]: `internal/cron/scheduler_finish.go:330-394`。方案：改名 recordTerminalResult；rollback 抽 prevSnapshot struct。
- [ ] **R247-CR-15 — recordResultP0WithSanitised P0 后缀历史 noise（P2）** [REFACTOR]: `internal/cron/scheduler_finish.go:301-329`。方案：与 R247-CR-14 一并改名。
- [~] **R247-CR-18 — gcWaitBudget 包级 mutable var 测试 racy（P3）** [REPEAT-3]: `internal/cron/scheduler.go:655,661` 与 R246-CR-012 同根因。方案：const + WithStopBudget(d) helper。
- [ ] **R247-CR-19 — marshalJobs atomic.Pointer test seam 通过 init() 装载（P3）** [REFACTOR]: `internal/cron/scheduler_persist.go:32-37` 与 R242-CR-5 同根因。方案：build tag testonly 或字段 DI。
- [ ] **R247-CR-20 — runs 限额 magic number 推导散落（P3）** [REFACTOR]: `internal/cron/runstore.go:170-187`。方案：集中注释块 + 推导公式。
- [x] **R247-CR-22 — maxJobsHardCap=500 等 const 缺 benchmark 引用（P3）** [REFACTOR]: `internal/cron/scheduler.go:283-294`。方案：链到 cron-v2-polish.md。 *(已实施：maxJobsHardCap / defaultMaxJobs / defaultExecTimeout / DefaultMaxJobsPerChat 四个 const godoc 末尾加 cron-v2-polish.md RFC 引用。)*
- [x] **R247-CR-23 — slogPrintfLogger panic/recovered 字符串扫描负面陈述（P3）** [REPEAT-3]: `internal/cron/scheduler.go:807-815` 与 R246-CR-016 同根因。方案：抽命名常量 + godoc。 *(已实施：scheduler.go:920-947 抽 cronPanicMarker / cronRecoveredMarker 命名 const + godoc 解释为何同时 match 两个 marker（"panic" 是当前 robfig/cron 实际文案，"recovered" 是 upstream-stability fallback）；reviewer 调措辞只需改一处。)*
- [x] **R247-CR-24 — executeIfNotDeletedOrPaused godoc 历史 review 引用（P3）** [REFACTOR]: `internal/cron/scheduler_run.go:38-49`。方案：去 review code 仅留行为说明。 *(已实施：godoc 重写为只留行为契约 + 锁顺序约束，删除历史 review 编号 noise；contract test 锚点（如 CRON3/4）保留。)*
- [ ] **R247-CR-25 — 历史 review 编号注释累计 40+ 处（P3）** [REFACTOR]: `internal/cron/scheduler_run.go,scheduler.go` 多处。方案：归档时同步删除注释或加 docs/COMMENT_CONVENTIONS.md。
- [ ] **R247-CR-27 — Append truncate 三字段注释不对称（P3）** [REFACTOR]: `internal/cron/runstore.go:280-339`。方案：抽 shrinkOversizeRun helper。
- [ ] **R247-CR-29 — TriggerNow 60 行 + 3 goroutine 分支（P3）** [REPEAT-3]: `internal/cron/scheduler_jobs.go:780-833`。方案：合并单 goroutine + 内部 if。注意：trigger_now_wg_done_test.go 的 CRON4 结构契约硬性要求"3 个 go func() + 3 个 defer Done"；要落地本 TODO 必须先调整该 test 表达新契约（"恰好 1 个 go func() 且包含 defer Done"），是 BREAKING-LOCAL 跨 test+impl，不适合 hourly pick。
- [x] **R247-CR-30 — IsExcluded godoc 与实现 cost 不一致（P3）** [REFACTOR]: `internal/cron/scheduler_session.go:40-46`。方案：godoc 标注 O(jobs × recentCap) + 推 KnownSessionIDs cache。 *(已实施：godoc 拆两段，cost 段明示 O(jobs × recentCap) per call 并指向 R247-PERF-3 长期 TTL-cache fix；hot-path callers 走 KnownSessionIDs() snapshot 复用。)*

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
- [x] **R246-CR-010 [P3] [REFACTOR] — `internal/cron/scheduler.go:138-142` 类型块仅声明回调类型却放在 var/const 旁**: R245 已新建 scheduler_callbacks.go。建议：把 OnRunStartedFunc/OnRunEndedFunc/OnExecuteFunc 类型也搬到 scheduler_callbacks.go。 *(已实施：OnRunStartedFunc/OnRunEndedFunc 类型从 scheduler.go 搬到 scheduler_callbacks.go 与 OnExecuteFunc 同住；scheduler.go 仅留 RunStartedEvent/RunEndedEvent struct + atomic.Pointer 字段。)*
- [x] **R246-CR-011 [P3] [REFACTOR] — `internal/cron/runinflight.go:64-74` strHeap/timeHeap helper 命名误导**: 名字暗示"分配 heap"但实际只是命名 local + 取地址，escape 与否仍由编译器决定。建议：改名为 boxString/boxTime 或加备注，避免读者误以为强制 heap。 *(已实施：strHeap → boxString / timeHeap → boxTime；godoc 跟改但保留 escape analysis 历史；11 处调用点 internal/cron/runinflight.go + internal/cron/scheduler_run.go 全量切换。R246-CR-011 同根因。)*
- [x] **R246-CR-012 [P3] [REPEAT-N] — `internal/cron/scheduler.go:655,661` `var stopBudget = 30*time.Second` 与 `gcWaitBudget` 包级 var**: 注释说改 var 是测试可调，但 mutable var 易被多测试并发改写。建议：const + WithStopBudget(d) test helper（依赖注入）。 *(已实现 via R247-CR-18 commit d1bd6a7：defaultStopBudget/defaultGCWaitBudget const + WithStopBudget(d) helper 单点维护，stop_budget_test 改用 helper)*
- [ ] **R246-CR-013 [P2] [REFACTOR] — `internal/cron/scheduler_finish.go:281-299` emitOverlapSkipped 只在 1 处调用且 18 行**: 跨包/跨文件复用并不存在。建议：合并到 executeOpt CAS 失败分支，或加 godoc 明确"future caller will reuse"。
- [ ] **R246-CR-014 [P3] [REFACTOR] — `internal/cron/scheduler_run.go:165-202` preflightArgs 字段顺序导致 padding**: 8B/120B/16B/8B/32B/16B/24B/16B 混排，64-bit 平台多 8-16 bytes。建议：按 size DESC 重排（snap → notifyTo → startedAt → 16B strings → ptrs）。
- [x] **R246-CR-015 [P3] [REFACTOR] — `internal/cron/runstore.go:163-188` const 块混排不同语义**: User-configurable defaults 与 Hard limits 混在一组。建议：拆两组并加注释。 *(已实施：const 块拆 user-configurable defaults / hard limits 两组，每组前加分组 godoc 解释可调性差异。)*
- [x] **R246-CR-016 [P3] [REFACTOR] — `internal/cron/scheduler.go:801-816` slogPrintfLogger strings.Contains "panic"/"recovered" 字符串扫描脆弱**: robfig/cron v3 PrintfLogger 措辞调整就降级。建议：抽 panicMarker/recoveredMarker 命名常量便于一处改。 *(已实施 via R247-CR-23：cronPanicMarker / cronRecoveredMarker 命名 const + godoc 解释 upstream-stability 双 marker 策略，同根因合并跟踪。)*
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
- [x] **R245-PERF-3 [REFACTOR R233-PERF-2 / R243-PERF-4 主条目] — `internal/cron/runstore.go:353-355` cacheHeadPush O(N) memmove**: 建议：定长 ring buffer。
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


## Discarded

> Triage 2026-05-25 by `triage-findings` skill (batch3 group A: R245+R246+R247, 190 items).
> Audit trail for items not promoted to GitHub issue. Reasons follow the skill's bucket-C taxonomy.

### Already-fixed (verified absent in current code)

- R245-CR-007 → already implemented (CreatedAt UTC, raw bullet marked [x])
- R245-CR-001 → already implemented (serviceUser fail-fast, raw [x])
- R245-CR-003 → already implemented (feishu remote field, raw [x])
- R245-CR-005 → already implemented (NewCLIBackendsHandler Deprecated, raw [x])
- R245-GO-1 → already implemented (resumeJobLocked err wrap, raw [x])
- R245-GO-5 → already implemented (CAS sessionID Store(nil), raw [x])
- R245-CR-2 → already implemented (EndedAt omitempty, raw [x])
- R245-GO-2 → already-fixed (SetJobPrompt now uses IIFE+defer per scheduler_jobs.go:606)
- R245-GO-3 → already-fixed (DeleteJob/PauseJob/ResumeJob: defer Unlock present scheduler_jobs.go:67,310,449)
- R245-GO-8 → already-fixed (assertJobLockHeld removed; -race covers)
- R245-PERF-2 → covered by issue cluster #528 (R247-PERF-3 KnownSessionIDs cache)
- R245-PERF-3 → already implemented (cacheHeadPush ring buffer, raw [x])
- R245-PERF-5 → covered by #549 (R247-PERF-10 cluster)
- R245-PERF-6 → covered by #528 (same root cause as R247-PERF-3)
- R245-PERF-8 → already-fixed via fde8f67 (raw [~] notes "F1 [REPEAT-2]")
- R245-PERF-10 → covered by #540 (R247-PERF-9 cluster)
- R245-PERF-11 → covered by #529 (R247-PERF-4 cluster)
- R245-PERF-16 → covered by #529 (R247-PERF-4 cluster)
- R245-SEC-3 → already implemented (transcript flatten sanitizeWireText, R246-SEC-1 direct fix)
- R245-SEC-8 → raw [~] explicitly says skip (Lstat error verified covered)
- R245-SEC-11 → raw [~]; BinPath PATH race verified handled by NewRunner abs-path
- R245-SEC-15 → raw [~]; cliPath IsAbs confirmed in current cli/wrapper.go
- R245-ARCH-26..50 individual items: clusters merged into #913 (26+27), #892 (39+44+50), #891 cluster (36+37+49), #913, #882, #883, #884; ARCH-28 (ManagedSession) into #805 cluster, ARCH-30 (processIface) dup of #430, ARCH-31 (main 622L) dup of #396, ARCH-34 (Clock abstraction) into #643, ARCH-35 (WorkspaceStore split) into #883, ARCH-40 (Server 30+ fields) into #738, ARCH-47 (cross-module error mapping) into #611
- R245-ARCH-41,48 not present in raw (numbering gaps)
- R246-CR-010 → already implemented (raw [x])
- R246-CR-011 → already implemented (raw [x])
- R246-CR-012 → already implemented via R247-CR-18 commit d1bd6a7 (raw [x])
- R246-CR-015 → already implemented (raw [x])
- R246-CR-016 → already implemented (raw [x] via R247-CR-23)
- R246-GO-1 → already-fixed (TriggerNow entryID read holds RLock, scheduler_jobs.go:798-816 + comment cites this fix)
- R246-GO-13 → already implemented (raw [x])
- R246-GO-4 → covered by #549 (R247-PERF-10 cluster)
- R246-GO-6 → covered by #476 (R247-GO-5 cluster)
- R246-GO-11 → covered by #528 (R247-PERF-3 cluster)
- R246-GO-17 → raw [~] confirms downstream not appending; merged into #549 (Append/jobLock cluster)
- R246-PERF-2 → covered by #540 (R247-PERF-9 cluster)
- R246-PERF-5 → covered by #553 (R247-PERF-14 cluster)
- R246-PERF-6 → covered by #549 (R247-PERF-10 cluster)
- R246-PERF-8 → covered by #540 (R247-PERF-9 cluster)
- R246-PERF-10 → cosmetic (raw [~]; connector.marshalResult comment-level only)
- R246-PERF-11 → covered by #551 (R247-PERF-11 cluster)
- R246-PERF-12 → covered by #553 (R247-PERF-14 cluster)
- R246-PERF-13 → covered by #570 (R247-PERF-23 cluster)
- R246-PERF-14 → opened as standalone #729
- R246-PERF-16 → already-fixed via fde8f67 (raw [x] EntriesAppend/LastNAppend buffer-reuse)
- R246-SEC-1 → direct fix landed (transcript flatten sanitizeWireText)
- R246-SEC-2 → direct fix landed (serveRaw isSensitiveDownloadName guard)
- R246-SEC-3 → already implemented (raw [x] handleSystemDaemons writeJSON)
- R246-SEC-4 → raw [~] no underlying claim (silent --resume drop verification handled)
- R246-SEC-8 → covered by #501 (R247-SEC-8 cluster)
- R246-SEC-9 → covered by #503 (R247-SEC-11 cluster)
- R246-SEC-10 → covered by #518 (R247-SEC-23 cluster)
- R246-ARCH-4 → marked [REPEAT-19~21]; not visible in raw bullet list (avoided by reviewer)
- R246-ARCH-5 → raw F4 partial fix (sysession side); cron+server sides remain in #913 R245-ARCH-26 family
- R246-ARCH-8 → already implemented (raw [x] testutil.go release build tag + contract test)
- R246-ARCH-13 → covered by #659 (R247-ARCH-16 cluster)
- R246-ARCH-14 not present in raw (numbering gap)
- R246-ARCH-15 → covered by #776 (R246-ARCH-2 cluster ServerOptions)
- R246-ARCH-16 → covered by #622 (R247-ARCH-6 cluster)
- R246-ARCH-17 → covered by #632 (R247-ARCH-10 cluster)
- R246-ARCH-18 → covered by #599 (R247-CR-19 cluster)
- R247-PERF-1 → direct fix landed (framing.go double-free)
- R247-PERF-2 → not present in raw (numbering gap)
- R247-PERF-12,13,15,18,22,25 → numbering gaps in raw
- R247-GO-1 → direct fix landed (freshSnap pre-store removal)
- R247-GO-2 → direct fix landed (ErrAmbiguousPrefix sentinel)
- R247-GO-3 → direct fix landed (ListJobs make([]Job,0,len))
- R247-GO-4 → direct fix landed (ringRead/ringSnapshot cap=0 guard)
- R247-GO-7 → already implemented (raw [x])
- R247-GO-9,14 → numbering gaps in raw
- R247-GO-11 → already implemented (raw [~]; resetRouterStub nil-check at scheduler.go:908)
- R247-SEC-1 → direct fix landed but reverted 2026-05-25 (CDN→origin link incompat); raw bullet documents revert
- R247-SEC-2 → direct fix landed (handleTrigger writeLimiter)
- R247-SEC-3 → direct fix landed (handlePreview writeLimiter reuse)
- R247-SEC-7,9,16,19,20 → numbering gaps in raw
- R247-SEC-10 → already implemented (sensitivePathSegments allowlist confirmed in project_files.go:1276)
- R247-SEC-17 → already implemented via cookieGen mechanism (raw [~]; partial — hot-reload bump opened as #826)
- R247-SEC-21 → raw [~]; low-impact sufficient guard already exists
- R247-SEC-22 → already implemented (TLS != nil + loopback gate at reverseserver.go:89-92)
- R247-SEC-24 → covered by #807 (R246-SEC-5 cluster)
- R247-CR-3 → dup of #423 [RNEW-003] executeOpt 390L cluster
- R247-CR-5 → covered by #538 (R247-PERF-7 cluster)
- R247-CR-7 → raw [~]; cosmetic-only setter-helper merged to backlog by skill convention (3 setters near-identical, generic helper PR small but no functional impact)
- R247-CR-9,13,16,17,21,26,28 → numbering gaps in raw
- R247-CR-10 → raw [~]; closure refactor cosmetic; backlog
- R247-CR-11 → already implemented (raw [x])
- R247-CR-18 → already implemented (raw [~] via d1bd6a7 stop_budget_test helper)
- R247-CR-22 → already implemented (raw [x])
- R247-CR-23 → already implemented (raw [x])
- R247-CR-24 → already implemented (raw [x])
- R247-CR-30 → already implemented (raw [x])
- R247-PERF-20 → raw [~]; auto_titler tick highwater fix is small follow-up; backlog
- R247-PERF-26 → raw [~]; persister tickFlush map iter ordering; backlog
- R247-ARCH-13 → covered by #631 (R247-ARCH-8 cluster)
- R247-ARCH-14 → covered by #611 (R247-ARCH-2 cluster)
- R247-ARCH-19 → covered by #631 (R247-ARCH-9 cluster i18n)
- R247-ARCH-21 → covered by #660 (R247-ARCH-17 cluster)

### Cosmetic (zero runtime impact, moved to docs/cosmetic-backlog.md)

- R245-CR-008, R246-GO-21, R246-GO-22, R246-PERF-10, R245-PERF-12, R245-PERF-18,
  R247-GO-15, R247-GO-16, R247-GO-17, R247-CR-6, R247-CR-7, R247-CR-8, R247-CR-10,
  R247-CR-25, R247-CR-27, R247-PERF-20, R247-PERF-26

### Discard reason summary (190 items)

- Direct-fix landed (raw [x]): 25
- Already-fixed in code (verified by grep): 14
- Cluster merged into another anchor's issue: 51
- Cosmetic moved to backlog: 17
- Numbering gaps in raw (no bullet): 19
- Promoted to GitHub issue: 64

