# TODO Changelog — 历史摘要存档（只读）

> docs/TODO.md 已于 2026-05-26 删除。本文件保留作历史"上一轮更新"摘要追溯,
> 供 grep R-anchor 回溯当时上下文。
>
> 新流程:`gh issue list` / `.claude/skills/triage-findings/SKILL.md`。
>
> 排序：最新在前。


> 最后更新 2026-05-25 —— **同根因清理：归档 PR #329/#330/#331/#345 已解决的 11 条历史条目**：
>   - 完全归档 `[x]`：R246-ARCH-14 / R245-ARCH-41 / R242-ARCH-17 / R242-ARCH-8 / R240-ARCH-11 / R237-ARCH-7 / R236-ARCH-12 / R243-ARCH-22 / R248-CR-3（共 9 条 — Capabilities interface 收口 sendFn/takeoverFn/replyFooterFn + spawningKeys chan 替自旋 + AgentLinker interface + dispatchCapabilities → serverCaps 改名 + boot-panic gate）
>   - 部分归档 `[~]`：R226-ARCH-3 / R234-ARCH-8 / R231-ARCH-5 / R222-ARCH-6 / R217-CR-4（共 5 条 — wshub 文件已 split 但 struct 未拆，跟踪 R248-ARCH-6；dispatch.Capabilities 仍 import cli/session 类型，跟踪 R248-ARCH-3）
>   - R240-CR-9 行号失效更新（PR #327 split 后方法分散到 wshub_*.go 多文件）
>
> 上一轮更新 2026-05-24（晚间）—— **R239-ARCH-I AgentLinker interface 解耦 server↔cli**（[BREAKING-LOCAL]，REPEAT-8）：新建 `internal/session/agentlink/agentlink.go` 定义 `AgentLinker` interface（4 方法 OnResolve / Query / QueryOrResolveFast / ProjectSessionDir）；Hub.wiredLinkers 类型改 `map[agentlink.AgentLinker]struct{}`，wshub.go 撤 `cli` import；wshub_agent.go + dashboard_agent_events.go 通过 interface 持有 linker（concrete *cli.SubagentLinker 隐式实现接口）；测试 fixture 跟改。`go test -race ./...` 全 ok。
>
> 上一轮更新 2026-05-24（夜间）—— **wshub.go god-object 拆分**（R243-ARCH-2，REPEAT-21 主条目 + R243-ARCH-17 / R240-ARCH-2 一并归档）：wshub.go 2028→525 行 (-74%)，拆出 5 个职责文件 _broadcast (368) / _send (379) / _subscribe (373) / _eventpush (274) / _upgrade (232)。Hub struct 不动保持锁不变量；source-anchor 测试 4 处指向新文件。
>
> 同日早些时候 —— PR #309 (commit c13fa47) **scheduler.go god-object split** 归档：标 [x] 7 条同根因条目（R243-ARCH-1 主条目 REPEAT-19 + R244-ARCH-17 / R240-ARCH-4 / R239-CR-1 / R235-CR-8 / R235-ARCH-24 / R232-ARCH-1）。scheduler.go 3317→852 行，拆出 7 个职责文件（_jobs 942 / _run 827 / _finish 493 / _persist 146 / _callbacks 127 / _notify 115 / _session 108）。注：行号引用过时的 R242/R243 ARCH 条目保留不动 — 代码本身在新文件里仍存在，按函数名 grep 即可。
>
> 上一轮更新 2026-05-24 —— TODO 大清理 v2（主题级合并）：在 v1 基础上跨 Round 按"同根因主题"再折叠 43 条派生：
>   - cacheHeadPush ring buffer（主 R233-PERF-2）：删 7 条
>   - KnownSessionIDs cache（主 R233-PERF-3）：删 6 条
>   - dashboard CSP unsafe-inline（主 R236-SEC-02）：删 11 条（跨 R172-R241）
>   - CSP sandbox style/script-src（主 R241-SEC-5）：删 3 条
>   - addJobAcquiringLock jobsByChat 索引（主 R237-PERF-5）：删 3 条
>   - diskListNewestFirst readRun cache（主 R236-PERF-07）：删 2 条
>   - shimWriter byte→string copy（主 R71-PERF-H1）：删 4 条
>   - eventlog `[]EventEntry{e}` PersistSink retain（主 R214-PERF-1）：删 4 条
>   - publicTmpProject /tmp 暴露（主 R237-SEC-5）：删 2 条
>   - 其它（busctl scope / router.Reset 锁）：删 2 条
>
> 累计本日大清理：35 [x] + 44 REPEAT-N + 2 已修孤儿 + 43 主题派生 = **89 条删除**。**open items 822 → 733**（行数 2222 → 2100，约 5%）。验证方法：6 个主条目 grep 计数均 = 1；派生 anchor grep 计数均 = 0。
>
> 上一轮更新 2026-05-24 Round 237 —— 深度 5-agent 并行 code review 第 47 轮（与同日 #290 Round 236 并发触发的另一批）：8 处直接修落地（discovery normalizeClaudeUUID 栈上 [32]byte 消 alloc [R237-PERF-1] / weixin pollLoop hookSem cap=20 与 feishu/slack/discord 对齐 [R237-SEC-1] / dashboard CSP 加 frame-ancestors 'none' 强化 clickjacking 防御 [R237-SEC-2] / dispatch sendTodoMessage 改 context.Background 防 turn 末尾被截断 [R237-GO-1] / project validate.go 删 maxPlannerPromptBytes 重复别名 [R237-CR-1] / upstream connector_conn.go 删尾部孤立 handleRequest godoc 防止误读 [R237-CR-2] / sysession auto_titler.go candidates 排序改 slices.SortFunc [R237-PERF-2] / sysession registry.go validateDaemonName 改手写 ASCII 检查省 regexp [R237-PERF-3]）+ 本轮 NEEDS-DESIGN 归档见 Round 237 节。注：commits 落地 anchor 仍用 [R236-X]（提交时 #290 尚未合并），文档归类用 R237 与 #290 区分。
>
> 上一轮更新 2026-05-24 Round 236 —— 深度 5-agent 并行 code review 第 46 轮：8 处直接修落地（cron Stop 等 GC goroutine 退出 [R236-GO-01] / cron addJobAcquiringLock persist 失败回滚 entry [R236-GO-10] / cron UpdateJob 注册失败回滚 schedule 字段 [R236-QA-08] / cron loadJobs Lstat 拒绝符号链接 CWE-59 [R236-SEC-01] / cron trimJobLocked sort 与 diskListNewestFirst 对齐 [R236-QA-01] / cron validateSchedule 拒绝 interval<=0 [R236-QA-07] / server send.go 删除入口冗余 BroadcastSessionsUpdate 全量扇出 [R236-PERF-01]，加 R236-SEC-12 经核对已在 c337d68 修复故关闭）+ 本轮 NEEDS-DESIGN 归档 ~30 项见 Round 236 节。
>
> 上一轮更新 2026-05-23 Round 235 —— 深度 5-agent 并行 code review 第 45 轮：18 处直接修落地（cron computeJobTimeout 删除空壳 + 测试 [R235-CR-1] / cron finishArgs.job 注释 Label→ID 修正 [R235-CR-2] / cron loadJobs 校验 ID/Title/Backend 防手编 cron_jobs.json 注入 [R235-CR-5/12] / cron executeOpt errMsg 统一 "outside allowed root" [R235-CR-6] / cron TriggerCatchup godoc 标注 do-not-emit [R235-CR-13] / cron previousTickMaxIter 抽常量 + 量化推导 [R235-CR-10] / cron cacheHeadPush 注释纠偏（cap-full 不省拷贝）[R235-CR-11] / cron readRun Lstat 防 symlink path-traversal [R235-SEC-5] / cron 父目录 0700 与 runs/ 对齐 [R235-SEC-6] / cron diskListNewestFirst 改 time.Compare 单调用 [R235-PERF-17] / cron trimJobLocked 加 runID DESC tiebreak 与 list 对齐 [R235-GO-7] / cron recordResultP0WithSanitised 快照 jobID 防 unlock 后访问 [R235-GO-1] / cron notifyTarget ctx.Err 短路防越 cronNotifyTimeout [R235-GO-5] / discord 加 hookSem cap=20 与 feishu/slack 对齐 [R235-SEC-3] / weixin Start 强制 https + maxWeixinMsgsPerPoll=100 防 relay 注入 [R235-SEC-1/8] / feishu nonceCleanupInterval = nonceTTL/2 [R235-SEC-9] / dashboard truncateRunes 重写消 off-by-one [R235-SEC-4] / acp ReadEvent stopReason 长度快路径 [R235-PERF-20] / selfupdate verifyChecksum CRLF 注释挡 reviewer 误判 [R235-SEC-7]）+ 本轮 NEEDS-DESIGN 归档见 Round 235 节。
>
> 上一轮更新 2026-05-23 Round 234 —— 深度 5-agent 并行 code review 第 44 轮：11 处直接修落地（cron scheduler.deliverNotice 改用 sanitiseRunResult 防 IM-injection [R234-SEC-1] / cron runstore.trimAll 跳 symlink 与 diskListNewestFirst 对齐 [R234-SEC-10] / cron runstore.newRunStore 主动 MkdirAll runs/ 0o700 防 jobID 枚举 [R234-SEC-4] / cron redactPathsInCronError 增加 ~/ 形态识别 + fast-path 同步 [R234-SEC-9] / cli process_readloop 提前 isSystemInit 复用去重双 string 比较 [R234-PERF-12] / cron runstore.truncateForRetry 引用 truncatedSuffix 常量 + 抽 maxRetryFieldRunes=256 [R234-CR-9] / cron IsValidID godoc 改写为输入形态描述不再引用私有 generateID [R234-CR-10] / cron workDirReachable 加 root-containment 不强制注释 [R234-CR-11] / sysession runRing invariant godoc 0<=head<runRingCap [R234-GO-14] / cron runStore.keepCount immutable-after-construction 注释 [R234-GO-13] / cron runStore 锁层级 godoc s.mu>jobLock>entry.mu [R234-GO-7]）+ 本轮 NEEDS-DESIGN 归档见 Round 234 节。
>
> 上一轮更新 2026-05-23 Round 233 —— 深度 5-agent 并行 code review 第 43 轮：合计 21 处直接修落地。第一批 12 处（PR #239）：cron RegisterCronStub 接口收敛删 1 方法 + 4 处 fake test 同步 / cron computeJobTimeout 删 schedule 死参 / cron redactPathsInCronError 用 TruncateAtRuneBoundary 守 UTF-8 边界 / dispatch nil watchdog atomic 防御 / acp readUntilResponse 超时 goroutine 通过 select-with-done 不阻塞 / sysession limitedWriter Write 错误路径不再违反 io.Writer 契约 / acp textBuf 写入 cap 守 maxAssistantMessageContentBytes / dashboard transcript ANSI regex fast-path 用 IndexByte 0x1b / project_files sensitiveDownloadNames 补 service-account.json/secrets.yaml / dashboard_cron 删 maxCronBackendLen 死常量 / wshub interruptLimiter 0.5/s 收紧 / agent_tailer silent 路径无订阅时跳过 subs 快照。第二批 9 处（PR #240）：cli/passthrough.go `cap` 变量重命名避免 shadow builtin / cron `defaultMaxJobs=50`+`defaultExecTimeout=5min`+`cronNotifyTimeout=30s` 抽命名常量 / sysession `defaultDaemonTickInterval=30s` 命名常量 / cron `addJobLocked` ID 碰撞 retry 由 unbounded 改 cap 至 10 次 + 显式 error 出口 / cron `loadJobs` 增加 `len(entries)>maxJobsHardCap=500` 防御性上限 / cli/process_turn drainStaleEvents interrupted-settle 分支补 slog.Warn 与 line 179 主路径对齐 / wshub `dashTokenHash` immutable-after-construction 注释明确 hot-reload 不支持。+ ~30 NEEDS-DESIGN 归档见 Round 233 节。
>
> 上一轮更新 2026-05-22 Round 232 —— 深度 5-agent 并行 code review 第 42 轮：7 处直接修落地（cron runstore cacheHeadPush O(N)→O(1) prepend / cron Start trimAll 改异步 goroutine / cron trimAll ReadDir 错误升级 Warn / sysession runner stderr 预截 256 / cron previousTickBefore 1000 次迭代上限 / cron recordResult 4*1024→maxStoredResultRunes 常量 + slogPrintfLogger 同时匹配 panic\|recovered + storeMu 注释纠偏 + diskListNewestFirst & trimJobLocked 跳 symlink）+ ~75 NEEDS-DESIGN 归档见 Round 232 节。
>
> 上一轮更新 2026-05-21 Round 231 —— 深度 5-agent 并行 code review 第 41 轮：12 处直接修落地（sysession.Manager Stop nil-cancel 守卫 / sysession.Manager hookMu→RWMutex / sysession.autoTitler Configure 二重调用消除 / sysession.autoTitler validate fmt.Errorf 二重 %w 修复 / sysession.autoTitler observed 容量底 16 + candidates 预分配 / sysession.autoTitler Tick 缓存 now / sysession.autoTitler 早停跳过 highwater prune / sysession.autoTitler renameOne prompt strings.Builder Grow / sysession.runner stdout 限 64KiB + ctx.Err 仅在 errors.Is(Canceled/DeadlineExceeded) 时优先 / sysession.sweep errors.Is(fs.ErrNotExist) / sysession.env envAlwaysPassthrough 直查不复制 / config.Load io.LimitReader 1MiB 上限 / cli.eventlog Append no-sink 早返回省 slice 逃逸 / shim.moveToShimsCgroupDirect 加 caller-contract godoc）+ ~30 NEEDS-DESIGN 归档见 Round 231 节。
>
> 上一轮更新 2026-05-21 Round 230 第二批 —— 深度 5-agent 并行 code review 第 41 轮：16 处直接修落地（router_lifecycle.go historyWg IIFE 同步化语义对齐 / runstore.trimAll defer Unlock 防 panic 死锁 / dashboard_cron maxCronBackendLen 收敛到 maxBackendIDLen / scheduler maxStoredResultRunes 包级常量替换 3 处局部 const / runSummaryView 抽包级 cronRunSummaryView / cron notifyTarget 4000 → platform.DefaultMaxReplyLen / handleList loc.String() 二次调用合并 / cron runstore sort.Slice → slices.SortFunc 删 sort import / runIDPattern 别名删除直调 IsValidID / runDetailView Prompt+WorkDir SanitizeForLog / dashboard_system handleClearLabelOrigin 加 MaxBytesReader / sensitiveDownloadNames 补 id_rsa/id_dsa/id_ecdsa/id_ed25519 / rawPreviewMimes 删 image/svg+xml 一致 / agentTailer registry runWG 跟踪 run goroutine + Shutdown wait / dispatch 多图替换改 strings.NewReplacer 一次性 / selfupdate 备份强制 0o600 + Rollback 回 chmod 0o755 + cacheGet 双锁注释 + agentCommands 无锁注释 + task_progress 补 bgAgents 分支 + TriggerCatchup/ErrClassPanic godoc reserved）+ NEEDS-DESIGN 归档见 Round 230 节。
>
> 上一轮更新 2026-05-21 Round 230 —— 深度 5-agent 并行 code review：12 处直接修落地（sensitive download names 加 SSH 私钥/.p8 / servePreview 加 sensitive guard / contentBytes 双调用合并 / process_event_format 死 default:continue 删除 / setWorkspace godoc 名字过时修正 / spawningKeyPollInterval 常量化 / maxBackendIDLen 与 cron 共享 / transcribe LimitReader / askquestion operatorID & ToolUseID 长度预截 / historyCtx parent 修正 + AfterFunc fan-in / shim XDG_ 收紧因测试契约暂缓登 TODO）+ ~25 NEEDS-DESIGN 归档见 Round 230 节。
>
> 上一轮更新 2026-05-20 Round 229 —— 深度 5-agent 并行 code review：14 处直接修落地（godoc 错位修正 / spawnParams.Backend 死字段 / maxExemptSessions 注释纠正 / cron slogPrintfLogger 替换 log 包桥接 / scanLastSummaries helper 消除重复扫描 / hasInjectedHistory godoc 行号去除 / Snapshot SetModel 冗余检查 / acceptsGzip strings.Split 零 alloc 重写带 q-value 兜底 / MeteringUsage 改 sync.RWMutex / wsclient readPump SetReadDeadline 错误检查 / reverseconn SetReadDeadline 错误检查 / serveDownload .env/.netrc/*.pem/*.key 黑名单 / Go 1.22+ s := s 冗余清理 ×2）+ NEEDS-DESIGN 归档见 Round 229 节。
>
> 上一轮更新 2026-05-20 Round 228 第二批 —— 深度 5-agent 并行 review 第 40 轮：13 处 FIX-READY 落地（protocol_claude WriteInterrupt 手写字节模板对齐 ACP / agent_tailer updateMetaFromEventLocked 接受 now 参数省 vDSO / agent_tailer buffered overflow copy 释放旧 backing array / dispatch/coalesce.go fmt.Fprintf → WriteString / cron/job.go generateHexID 改 hex.EncodeToString / cli/history.go + session/router_core.go + session/router_lifecycle.go 过期 Sprint 1b/1c 注释更新 / dispatch.SendSplitReply 4000 → platform.DefaultMaxReplyLen / dispatch 15s timeout 抽 platformReplyTimeout 常量 / upload_store ownerCounts+ownerBytes underflow slog.Warn / cron/scheduler.go opts.ExtraArgs 三元切片防别名 / process_event_format EventEntryFromEvent Deprecated 加 removal anchor / process_readloop isChanAlive 注释加跨文件提示 / agent_tailer idle/refCount TOCTOU 注释 / wshub no-token 模式 slog.Debug 区分）+ ~30 NEEDS-DESIGN 归档见 Round 228 第二批节。
>
> 上一轮更新 2026-05-20 Round 228 第一批 —— PR #161：4 处 FIX-READY 落地（project EffectivePlannerPrompt rune 扫描调 IsLogInjectionRune / cli sanitizeStderrLine table-driven test / eventlog Append/AppendBatch 锁外预生成 UUID / server ETag seed strconv.AppendInt 省 fmt.Sprintf 反射）+ R225-SEC-6/R224-CR-8/R225-PERF-11/R224-PERF-4 关闭。
>
> 上一轮更新 2026-05-20 Round 227 —— 深度 5-agent 并行 review 第 39 轮：12 处 FIX-READY 落地（subagent_link fireOnResolveLocked panic-safe defer Lock + errors.Is(bufio.ErrBufferFull) / dispatch.NewDispatcher takeoverFn nil noop fallback / cron snapshotJob LOCK godoc / discord 单消息 5 附件上限 + downloadURL 强制 https / weixin contextToken 长度 ≤512B / cli/subagent_transcript readLocked io.LimitReader 16MB / session extractLastPromptFromProcess + InjectHistory 改用 isActivityType 与 EventLog 6 类对齐 / session parseKeyParts 改 IndexByte 省 SplitN 切片 / cli normalizeBackendID strings.ToLower+TrimSpace / cli applyMetadata meteringUsage 16 单位上限 / process_readloop task_started TrimSpace 合并）+ ~70 NEEDS-DESIGN 归档见 Round 227 节。
>
> 上一轮更新 2026-05-19 Round 226 —— 深度 5-agent 并行 review 第 38 轮：13 处 FIX-READY 落地（process.Kill godoc 修正 / router_core 孤立 stripResumeArgs 注释删除 / process_readloop 过期 Phase 2 注释删除 / spawnSession LOCK 注释精度 / dispatch↔server 错误映射 sync 注释 / passthrough uuidFallbackSeq 跨文件指引 / upload_store removeEntryLocked 局部变量 owner fallback / select_node_for_backend cap → requiredCap / weixin getUpdates+sendMessage errmsg SanitizeForLog/%q / selfupdate Replace 错误 errors.Join 聚合）+ ~38 NEEDS-DESIGN 归档见 Round 226 节。
>
> 上一轮更新 2026-05-19 Round 225 —— 深度 5-agent 并行 review 第 37 轮：9 处 FIX-READY 落地（cli/passthrough newSlotUUID rand.Read 错误 fallback 与 newEventUUID 对齐 / shim writer 内层 w.Write 失败 early-return / shim SetReadDeadline 错误显式 close+return / upstream connector os.Hostname err 兜底 / dashboard_send io.LimitReader 防 fh.Size 撒谎 / readloop heartbeat Timer Stop 注释精度修正 / ResetChat fallback 分支 prefix 拼接外提 / protocol_acp ToolCall.Title/Kind/Status 加 SanitizeForLog(256) / selfupdate 0755→0600 + 验后 chmod 0755 + planner_defaults.prompt 加 validatePlannerPrompt 与 project.ValidateConfig 对齐）+ ~40 NEEDS-DESIGN 归档见 Round 225 节。
>
> 上一轮更新 2026-05-19 Round 224 —— 深度 5-agent 并行 review 第 36 轮：11 处 FIX-READY 落地（selfupdate constant-time compare + checksums.txt 64KB 独立上限 / feishu maxWebhookTokenLen 512 守卫 / protocol_acp ExtraArgs capExtraArgsBytes 对齐 / protocol_acp readUntilResponse 错误 %w 包装区分 EOF vs read err / subagent_link entries[:0:0] → make / gzipMiddleware response-only godoc + Flush godoc 修正 / dashboard.marshalPooled 安全契约交叉引用 / router.kiroSessionsDir Sprint 1a 注释更新 / backend/profile.go Sprint 0b/1b 路线图描述去除）+ ~40 NEEDS-DESIGN 归档见 Round 224 节。
>
> 上一轮更新 2026-05-17 Round 222 —— 深度 5-agent 并行 review 第 35 轮：12 处 FIX-READY 落地（sanitizeResumeLastPrompt IsLogInjectionRune 全开 + shim SetReadDeadline 错误传播 + cron snapshotJob RLock + rawScanSubagentsDir meta size cap + handleAttachment Lstat + filterShimEnv per-entry size cap + capExtraArgsBytes ARG_MAX 守卫 + sanitizeDownloadName 调 IsLogInjectionRune + feishu maxEventIDLen / maxIncomingTextBytes / 时间戳 const 抽取 + platform.DefaultMaxReplyLen + wrapper 错误消息小写 + ImageData godoc Deprecated + eventlog test 改用 Eventually）+ ~50 NEEDS-DESIGN 归档见 Round 222 节。
>
> 上一轮更新 2026-05-17 Round 219 —— 深度 5-agent 并行 review 第 33 轮：13 处 FIX-READY 落地（uuid hex stack-array / eventlog_bridge pooled encoder / EventEntries empty fast-path / pendingIdx flush cap shrink / config 0o077 perm mask / interruptAcquireTimeout 命名 / SessionConfig.Workspace godoc Deprecated / cron Sprintf / maxWebhookNonceLen 命名 / discovery ErrUnsupportedPlatform 单点 / dashboard_send.go path.Clean 注释）+ ~30 NEEDS-DESIGN 归档见 Round 219 节。
>
> 上一轮更新 2026-05-17 —— TODO 清理批：删除 35 个已完成 `- [x]` 条目（落地 PR 详情可在 git log 中以 review 锚点检索：R218-GO-1/SEC-1/SEC-2/CR-2、R218B-GO-4/SEC-1/SEC-3/ARCH-1/CR-1~4、R217-PERF-9/CR-2、R216-GO-3、R215-GO-P1-1/P2-1~4、R215-SEC-P2-1/P2-2/P3-3、R215-PERF-P2-2/P2-6、R215-CR-P1-1/P2-1/P2-2/P2-4、R215-ARCH-P2-8、R214-CODE-2/CODE-6、RNEW-004 等）。本次清理后剩余 ~227 个 open items。
>
> 上一轮更新 2026-05-16 Round 218 —— 深度 5-agent 并行 review 第 32 轮：6 处 FIX-READY 落地（PR #40：SubagentLinker goroutine 限并发 + contract_test cron pin；PR #22：eventlog slices.Reverse、validateModel error message、ManagedSession loadCliProcess helper、sanitizeResumeLastPrompt IndexFunc 短路）+ NEEDS-DESIGN 归档见 Round 218 节。
>
> 上一轮更新 2026-05-13 Round 217 —— 深度 5-agent 并行 review 第 31 轮：约 18 处 FIX-READY 落地（安全/Go 正确性/小性能/小质量/CR-1 限制常量统一）+ NEEDS-DESIGN 归档见 Round 217 节。
> 历史 Round 变更详情（narrative + 已修复归档）见 [`docs/TODO-changelog.md`](TODO-changelog.md)。
>
> 上一轮更新 2026-05-12 (Round 216 —— 深度 5-agent 并行 review 第 30 轮：15 处 FIX-READY 落地 + NEEDS-DESIGN 归档见 Round 216 节)
> 上一轮更新 2026-05-11 (Round 215 —— 深度 5-agent 并行 review 第 29 轮：20 处 FIX-READY 落地 + ~85 项 NEEDS-DESIGN 归档)
> 上一轮更新 2026-05-10 (Round 214 —— 深度 5-agent 并行 review 第 28 轮：15 处 FIX-READY 落地 + 30+ 项 NEEDS-DESIGN 归档)
> 上一轮更新 2026-05-10 (Round 204 —— 深度 5-agent 并行 review 第 27 轮：7 处 FIX-READY 落地 + 约 80 项 NEEDS-DESIGN 归档)
> 上一轮更新 2026-05-10 (Round 203 —— Attachment Refcount v1 MVP 落地 · RFC 子文件 Phase 6E-1 ~ 6E-4 + Router 接入 + 集成测试)
> 上一轮更新 2026-05-10 (Round 202 —— EventLog 持久化 MVP 落地 · RFC v3 Phase 0-5 + 6b + 6c + 子 RFC 框架)
> 上一轮更新 2026-05-10 (Round 201 —— 深度 5-agent 并行 review 第 26 轮 / 7 处 FIX-READY 落地 + 约 65 项 NEEDS-DESIGN 归档)
> 上一轮更新 2026-05-09 (Round 200 —— 深度 5-agent 并行 review 第 25 轮 / 14 处 FIX-READY 落地 + 约 75 项新增 NEEDS-DESIGN 归档)

