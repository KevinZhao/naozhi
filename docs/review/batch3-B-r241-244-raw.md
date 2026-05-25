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
- [ ] **R244-CR-P3-1 — static_cron_history_redesign_test magic constant 4000/2500 字节窗口**: 函数超出窗口时 silent 跳过 assertion。方案：用 strings.Index 定位 `}\n` 函数末尾代替固定窗口。 → #1062

### Architecture（剩余）

- [ ] **R244-ARCH-1 — 缺统一 LifecycleManager 抽象 [REFACTOR]**: cron + sysession + 未来 cron-skill-binding/planner-auto-start/system-session 各 stopCtx + budget + leak 策略。当前 shutdown ordering 仅在 main.go 隐式编码。方案：抽 `internal/lifecycle.Component { Start; Stop; Drain }` + 显式依赖图。
- [ ] **R244-ARCH-2 — eventlog PersistSink 闭包绕过 metrics/tracing；caller 不知背压状况 [REFACTOR]**: persister.go:243-274 OnDrop fire-and-forget。方案：升 PersistSink 为 interface 暴露 Pressure() float64 / accept bool。 → #1057
- [ ] **R244-ARCH-3 — 配置验证双源不一致 + 无 startup fail-fast hook [REFACTOR]**: cron applyDefaults vs sysession inline if。方案：`internal/config/validator.Validate(cfg) []ValidationIssue` 单一入口。
- [ ] **R244-ARCH-4 — wireup history-only blank import 不覆盖 cron daemons / platforms / backends 的 plug-in 注册 [REFACTOR]**: 方案：统一 `Registry[T]` 模式。 → #1058
- [ ] **R244-ARCH-5 — 三套独立 keyed persistence 抽象（runs/events/jsonl/attachments）反复 reinvent atomic write/trim/cache [REFACTOR]**: 方案：抽 `internal/persistence.KeyedStore[K,V]` 模板。 → discarded:dup-of-#509
- [ ] **R244-ARCH-6 — SessionRouter interface 缺 stub-removed 反向通知导致字符串 prefix coupling [BREAKING-LOCAL]**: scheduler.go:72-90 cron→session 通过 `cron:` 字符串 prefix 隐式约定。方案：CronKey/IsCronKey 移到 cron 包导出 + KeyKind enum。Breaking：是。 → #1059
- [ ] **R244-ARCH-7 — sysession Stop osExit(2) vs cron budget+leak policy divergence 缺架构决策机制 [REFACTOR]**: 方案：lifecycle.LeakPolicy enum {ForceExit, BudgetThenLeak, BlockForever}。 → #1060
- [ ] **R244-ARCH-8 — 三种 callback 注册风格 + cron godoc startup-only 无强制 [REFACTOR]**: 方案：抽 `internal/eventbus.Subscribe[E](handler) Unsubscribe`。 → #1061
- [ ] **R244-ARCH-9 — 三套独立持久化 Run record schema 无 SchemaVersion + 无 migration 钩子 [REFACTOR]**: 方案：所有 persisted struct 加 SchemaVersion uint16 + migrate(v, raw) 钩子。 → discarded:dup-of-#843
- [ ] **R244-ARCH-10 — cron 排除逻辑（KnownSessionIDs IsExcluded）未抽象到通用 sessionfilter [REFACTOR]**: 方案：`ExcluderRegistry { Register(name, fn); Lookup(sessionID) []ExcludeReason }`。 → #1051
- [ ] **R244-ARCH-11 — cron SetOnExecute/RunStarted/RunEnded single-channel callback 反模式 [REFACTOR]**: 方案：`chan Event` + 多订阅者 fanout / OpenTelemetry Event。 → discarded:dup-of-#508
- [ ] **R244-ARCH-12 — eventlog WriterAlive 健康协议 ad-hoc 各组件自定义 [REFACTOR]**: 方案：`internal/health.Probe` 各子系统注册 + /health 端点 fanout。 → #1052
- [ ] **R244-ARCH-13 — cron 同时存在 ID-based / plat+chat-based 双套 mutator API 重复 5 阶段 [REFACTOR]**: 方案：`JobMutator interface { Apply(j *Job) error }` 单 runMutation 封装。 → #1053
- [ ] **R244-ARCH-14 — executeOpt 单函数 344 行同时承担 8 个职责 [REFACTOR]**: 方案：`type runStep func(*runCtx) error` + pipeline 切分 ≤30 行/步。 → discarded:dup-of-#423
- [ ] **R244-ARCH-15 — 锁层级仅 godoc 描述无运行时检测 [REFACTOR]**: 方案：`internal/lockorder.Acquire(name)` goroutine-local stack。
- [ ] **R244-ARCH-16 — 超时常量散落 var 顶部不可统一调优/查阅 [REFACTOR]**: 方案：`internal/timeouts` 统一注册表 + startup 打印 + dashboard 显示。 → #1054
- [ ] **R244-ARCH-18 — sysession registry slice 字面量与 history blank import 风格不统一 [REFACTOR]**: 方案：第二个 daemon 落地前先统一 Registry pattern。 → #1055
- [ ] **R244-ARCH-19 — addJobAcquiringLock → registerStubFromJob 锁外副作用模式无 helper 强制 [REFACTOR]**: 方案：`withRouterCallback(mutate, postUnlock)` 模板 + lint 阻断违反。 → #1056

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

- [x] **R243-PERF-1 [REFACTOR]** `internal/cron/runstore.go:316/253` `Append` 在 `jobLock` 内多次调 `time.Now()`；建议 lock 前捕获一次 `now` 传下游，与 eventlog Persister 模式对齐。
- [ ] **R243-PERF-2 [REFACTOR]** `internal/cron/scheduler.go:453-458` `IsExcluded` 每次 spawn 重建 jobs×200 KnownSessionIDs map；与 dashboard 30s 快照独立。建议 atomic.Pointer[map] + 30s TTL 缓存。
- [ ] **R243-PERF-3 [REPEAT-25]** `internal/cron/scheduler.go:2340` `executeOpt` 内 `slog.With(4 attr)` 每次 cron 执行新建 logger；与 PERF-1 ReadEvent alloc 同模式第 25 次（cron 变体）。
- [x] **R243-PERF-4 [REFACTOR]** `internal/cron/runstore.go:353-355` `cacheHeadPush` `append` + `copy` 做 O(N) shift；改定长 ring buffer head/tail 指针 O(1) insert。
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

## Triage Outcomes (2026-05-25, triage-findings skill)

> Per-anchor decisions from batch3-B triage. See per-finding annotations inline above.

### Bucket A — GitHub issues opened

R241: #465 SEC-4 / #466 SEC-5 / #467 SEC-6 / #468 SEC-8 / #469 SEC-9 / #470 SEC-10 / #472 SEC-13 / #473 SEC-14 / #474 SEC-15 / #477 PERF-3 / #478 PERF-5 / #480 PERF-6 / #481 PERF-7 / #482 PERF-9 / #486 CR-6 / #487 CR-7 / #488 GO-2(+GO-3) / #490 GO-4 / #505 ARCH-1 / #506 ARCH-3 / #508 ARCH-4 / #509 ARCH-5 / #510 ARCH-6 / #511 ARCH-7 / #512 ARCH-8 / #515 ARCH-9 / #517 ARCH-10 / #519 ARCH-11 / #520 ARCH-12 / #521 ARCH-13 / #523 ARCH-14 / #524 ARCH-15

R242: #548 GO-3 / #550 GO-4 / #552 GO-5 / #554 GO-6 / #555 GO-9 / #556 GO-7 / #558 GO-8 / #574 GO-13 / #575 GO-14 / #587 GO-16 / #605 SEC-1 / #607 SEC-2 / #634 SEC-6 / #635 SEC-7 / #636 SEC-8 / #638 SEC-10 / #640 SEC-11 / #642 SEC-12 / #645 SEC-13 / #649 SEC-14 / #651 SEC-15 / #663 PERF-1 / #664 PERF-2 / #666 PERF-3 / #669 PERF-4-5-6-11 / #671 PERF-7 / #672 PERF-9 / #674 PERF-10 / #675 PERF-12 / #678 PERF-13 / #680 PERF-14 / #691 CR-3 / #693 CR-5 / #694 CR-7 / #695 CR-8 / #696 CR-11 / #704 CR-13 / #706 CR-14 / #718 ARCH-1 / #720 ARCH-2 / #721 ARCH-3 / #725 ARCH-7 / #727 ARCH-9 / #731 ARCH-10 / #733 ARCH-11 / #753 ARCH-12 / #754 ARCH-13 / #756 ARCH-14 / #758 ARCH-15 / #760 ARCH-16 / #762 ARCH-19 / #763 ARCH-20 / #765 ARCH-21 / #766 ARCH-22 / #767 ARCH-23 / #768 ARCH-26 / #770 ARCH-27 / #772 ARCH-28 / #774 ARCH-29

R243: #781 GO-9 / #788 SEC-6 / #789 SEC-7 / #795 SEC-9 / #797 SEC-11 / #798 SEC-12 / #799 SEC-14 / #800 SEC-15 / #810 PERF-5 / #812 PERF-7 / #813 PERF-8 / #814 PERF-9 / #816 PERF-11 / #817 PERF-12 / #822 CR-P2-4 / #823 CR-P3-4 / #835 ARCH-3 / #837 ARCH-6 / #838 ARCH-7 / #839 ARCH-8 / #841 ARCH-13 / #843 ARCH-14 / #845 ARCH-15 / #850 ARCH-18 / #864 ARCH-20 / #866 ARCH-21 / #869 ARCH-24 / #870 ARCH-25

R244: #877 SEC-P1-3 / #879 SEC-P1-4 / #886 SEC-P2-1 / #887 SEC-P2-2 / #888 SEC-P2-3 / #889 SEC-P2-5 / #897 SEC-P3-3 / #898 SEC-P3-4 / #899 SEC-P3-5 / #901 SEC-P3-6 / #909 GO-P2-2 / #911 GO-P2-3 / **(backfill 2026-05-25)** #1051 ARCH-10 / #1052 ARCH-12 / #1053 ARCH-13 / #1054 ARCH-16 / #1055 ARCH-18 / #1056 ARCH-19 / #1057 ARCH-2 / #1058 ARCH-4 / #1059 ARCH-6 / #1060 ARCH-7 / #1061 ARCH-8 / #1062 CR-P3-1

### Bucket B — Cosmetic backlog (godoc/comment-only)

R242-GO-15, R242-GO-17, R242-GO-18, R242-GO-20, R242-CR-12 / R244-GO-P1-1 (#908), R244-GO-P2-4 (#912), R244-GO-P3-1, R244-GO-P3-2 — appended to docs/cosmetic-backlog.md.

### Bucket C — Discarded

**Already-fixed (per raw `[x]` annotations):**
- R244-SEC-P2-4 (KaTeX/Mermaid SRI shipped via R243-SEC-4)
- R244-SEC-P3-1 (pprof/expvar gate shipped)
- R243-PERF-1 (Append time.Now lock-front shipped)
- R243-PERF-4 (cacheHeadPush ring buffer shipped)
- R243-PERF-10 (flush stride aliasing godoc shipped)
- R243-SEC-4 (CSP require-sri-for shipped)
- R243-SEC-8 (SetJobPrompt validateCronPrompt shipped via limits.go ValidatePromptStrict)
- R242-GO-11 (DevMode panic→slog.Error shipped 8f6e73b)
- R242-ARCH-25 (ResetChat broadcast godoc shipped — locking pattern intentional)
- R242-ARCH-18 (auto_chain combinedExcluderHeld shared shipped)
- R241-PERF-8 (saveStore sessionToStoreEntry helper shipped — F4 cron-fix-F4)

**Stale (already addressed):**
- R241-SEC-16 (#475 closed) — LimitReader Seek replaced by lr.N≤0 check via R243-GO-4

**Tracker dups (closed with cross-reference):**
- R242-GO-1 (#526 closed → dup of #423 RNEW-003 executeOpt split)
- R242-GO-10 (#559 closed → dup of #377 R248-ARCH-8 Hub.queue concrete type)
- R242-GO-12 (#560 closed → dup of #430 R176-ARCH-M2 processIface god-interface)
- R242-SEC-3 (#608 closed → dup of #436 R172-SEC-H1 DOMPurify)
- R242-SEC-5 (#618 closed → dup of #470 R241-SEC-10 cookie revocation)
- R243-SEC-1 (#782 closed → dup of #436 DOMPurify)
- R243-SEC-2 → also dup of #436 (not opened, marked discarded)
- R243-SEC-3 → dup of #605 (R242-SEC-1 CSP unsafe-inline)
- R243-PERF-14 (#818 closed → dup of #555 R242-GO-9 findByPrefix)
- R243-ARCH-16 (#847 closed → dup of #506 R241-ARCH-3 maps.Clone)
- R243-ARCH-19 (#852 closed → dup of #753 R242-ARCH-12 lockorder)
- R243-ARCH-23 (#868 closed → dup of #423 executeOpt split)
- R244-SEC-P1-5 (#880 closed → dup of #466 R241-SEC-5 serveRender CSP)
- R244-SEC-P3-2 (#896 closed → dup of #634 R242-SEC-6 __public_tmp__ opt-in)
- R244-ARCH-1 → dup of #839 R243-ARCH-8 lifecycle.Manager (not opened, intent-of-class)
- R244-ARCH-3 → dup of #506 R241-ARCH-3 (maps.Clone immutable map cluster)
- R244-ARCH-15 → dup of #753 R242-ARCH-12 lockorder cluster
- R242-ARCH-4 → dup of #430 processIface god-interface
- R242-ARCH-5 → dup of #508 R241-ARCH-4 SchedulerListener cluster
- R242-ARCH-6 → dup of #460 R248-ARCH-6 router/Hub god-struct cluster
- R242-ARCH-30 → explicitly NEEDS-DESIGN with analysis showing acceptable; not actionable

**Already-fixed false positives (raw file pre-block):**
- R244 SEC P1 误报 (X-Frame-Options/Referrer-Policy/Permissions-Policy / NotifyChatID 256-cap / SanitizeForLog rune-aware / anonCookie HttpOnly+SameSite / workDirUnderRoot 二次校验 / storeDirOnce permission)

**Rate-limit blocked (secondary rate-limit hit at R244 ARCH cluster) — BACKFILLED 2026-05-25:**
- R244-ARCH-2 (eventlog PersistSink interface) → #1057
- R244-ARCH-4 (wireup history-only blank import → unified Registry) → #1058
- R244-ARCH-5 (KeyedStore template — same root R241-ARCH-5 #509) → discarded:dup-of-#509
- R244-ARCH-6 (cron-prefix coupling KeyKind enum) → #1059
- R244-ARCH-7 (sysession Stop osExit vs cron leak policy) → #1060
- R244-ARCH-8 (callback registration unification) → #1061
- R244-ARCH-9 (SchemaVersion + migrate hooks — same as #843 R243-ARCH-14) → discarded:dup-of-#843
- R244-ARCH-10 (ExcluderRegistry abstraction) → #1051
- R244-ARCH-11 (SchedulerListener — dup of #508) → discarded:dup-of-#508
- R244-ARCH-12 (health.Probe protocol) → #1052
- R244-ARCH-13 (cron mutator interface — JobMutator) → #1053
- R244-ARCH-14 (executeOpt 8-step pipeline — dup of #423) → discarded:dup-of-#423
- R244-ARCH-16 (timeouts registry) → #1054
- R244-ARCH-18 (sysession registry slice) → #1055
- R244-ARCH-19 (lock-outside-callback helper) → #1056
- R244-CR-P3-1 (test magic-byte window — verify-via-strings.Index) → #1062

Backfill summary: 12 issues opened (#1051-#1062 contiguous-ish, interleaved with batch3-C R240 issues), 4 discarded as dup-of-existing per the inline annotations. Re-grep verified all 12 still real on master at backfill time.

