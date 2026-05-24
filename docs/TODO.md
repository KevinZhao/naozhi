# TODO

> 最后更新 2026-05-24 Round 236 —— 深度 5-agent 并行 code review 第 46 轮：8 处直接修落地（cron Stop 等 GC goroutine 退出 [R236-GO-01] / cron addJobAcquiringLock persist 失败回滚 entry [R236-GO-10] / cron UpdateJob 注册失败回滚 schedule 字段 [R236-QA-08] / cron loadJobs Lstat 拒绝符号链接 CWE-59 [R236-SEC-01] / cron trimJobLocked sort 与 diskListNewestFirst 对齐 [R236-QA-01] / cron validateSchedule 拒绝 interval<=0 [R236-QA-07] / server send.go 删除入口冗余 BroadcastSessionsUpdate 全量扇出 [R236-PERF-01]，加 R236-SEC-12 经核对已在 c337d68 修复故关闭）+ 本轮 NEEDS-DESIGN 归档 ~30 项见 Round 236 节。
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
- [ ] **R57-ARCH-001 — `Cleanup` 双 pass `loadProcess()` 在非 exempt 占多数场景下更慢（LOW）**: go-reviewer 指出 R56 的 "先 count candidates 再 allocate" 优化在 exempt 少的部署上反而多一次 loadProcess 扫描。idle plan 部署（每 5 分钟 tick 一次）无实际差异；需真实 profiling 数据决定是否回滚或改 single-pass count-then-grow。
- [ ] **R54-CONCUR-001 — `router.ReconnectShims` reconcile 期运行时的 `sess.ReattachProcessNoCallback` 无 sendMu 保护（继承自 R51-CONCUR-002 未决）**: 本轮复核确认仍未决。方案见 R51-CONCUR-002。
- [ ] **R52-CONCUR-004 — `shim reconnect` 后 `sess.persistedHistory` 未重新注入新 `proc.eventLog`（MED）**: `Router.reconnectShims` 在 `storeProcess`（`ReattachProcessNoCallback`）前调 `proc.InjectHistory(histEntries)`，但 `histEntries` 是从 `discovery.LoadHistory` 读的 JSONL 文件，**不是** `sess.persistedHistory`。两者大部分一致，但 `persistedHistory` 可能包含仅在内存里的 user prompt / interim 状态（R49/R47 曾多次浮现）。新 proc.eventLog 缺少这些条目时，`EventEntriesSince` 的 "proc != nil → proc.EventEntries" 快路径可能少返回若干历史。
- [ ] **R51-CONCUR-002 — `reconnectShims` 周期调用期间 `ReattachProcessNoCallback` 无 sendMu 保护（HIGH）**: `StartShimReconcileLoop` 每 30s 调一次 `reconnectShims`，调用点在持 `r.mu.Lock()` 时 `sess.ReattachProcessNoCallback(proc, sessionID)`；该函数 docstring 明确标注"调用者必须保证 Send() 不在飞行中"（safety constraint）。启动阶段 OK，但运行期 reconcile 不满足该假设 — `ManagedSession.Send()` 可能持 sendMu 执行旧进程。`storeProcess` 会原子替换活跃进程指针，并 `deathReason.Store("")` 清除 Send() 刚写入的 timeout 死因。逻辑 race（非 data race），Send() 拿的是旧 process 指针仍可写回结果。
- [ ] **R51-CONCUR-005 — 并发 `shim.Manager.Reconnect` 对同 key 晚胜者 Close 早胜者 handle，session 误死（MED）**: `Reconnect` 在 `m.mu` 外建立 TCP 连接（10s 超时），然后在 `m.mu.Lock()` 下插入 `m.shims[key]`。两路并发 Reconnect 分别建立连接后，晚胜者关闭早胜者 handle；但早胜者 handle 可能已被 Router `reconnectShims` 传给 `Process`，`Process.shimConn` 被 Close 导致 readLoop 退出、session 标为 Dead。
- [ ] **R37-REL1 — `MessageQueue.TryAcquire` + `Release` 不会触发 drain**: Dashboard/WS 路径用 Guard 接口，但若同期 IM 入口 Enqueue 了消息（enqueued=true），Release 不会触发 DoneOrDrain，消息永久搁浅直到下次 Enqueue 再成为 owner。属于 Guard/Queue 混用的根本限制。
- [ ] **R33-UX1 — `dashboard.js renderSidebar` 每次 sessions_update 全量 innerHTML 重绘**: 20 sessions × 1 update/s 情况下浏览器全侧边栏 reflow；已缓存 `allSessionsCache` 但未做 diff。scrollTop 保持已在 RNEW-UX-016 关闭时落地（rAF 恢复），剩余工作 = DOM diff + active-card 跨重绘保留 + `allSessionsCache` 一致性（`syncSidebarSelectionWithActive`/`removeSessionCard` 路径若做 in-place 修改可能让后续 update 看到不一致前缀）。合并 R34-UX1。
- [ ] **R31-REL3 — `moveToShimsCgroup` 依赖 runtime sudo + 未校验 CLIPID**: 现状用 `sudo busctl`/`sudo tee`，CLIPID 取自 shim JSON 直接入参；若 shim 被劫持可通过伪造 CLIPID 把任意进程挪入 scope。
- [~] **R30-DES1 — 需架构决策（2026-04-29 Round 112 评估降级）**：本轮尝试在 `execute()` 入口加 `stopCtx.Err()` 守卫覆盖 fresh + persistent 两种模式，但这与 Round 95 的设计意图冲突（Round 95 明确将 persistent 模式的 ctx 取消委托给 Router.Shutdown，`TestCRON3_PersistentModeUnaffectedByGuard` 把此行为作为测试护栏）。fresh 分支的 stopCtx.Err() 守卫（`scheduler.go:1260`）已覆盖最危险的"fresh → Reset → 孤立 CLI"路径。persistent 模式的真正修复需要架构级协调：要么把 Router.Shutdown 和 Scheduler.Stop 串联锁定（需 S11 级决策），要么在 GetOrCreate 路径里加 shutdown-awareness（改动面大）。当前降级，等 S11 整体方案落地后重开。
- [ ] **R29-DES1 — `drainStaleEvents` push-back + goto drain 可吞 interrupted result 事件**: 本轮新发现的 invariant 冲突。在 interrupted/interruptedRun 分支的 for 循环中，若事件顺序为 `[old_nonresult, new_event, old_result]`，读到 `new_event` 后 push-back + `goto drain`，接着 drain 到 `old_result` 时因 `recvAt < cutoff` 被丢弃。interrupted 语义要求 settle 窗口必须拿到 old_result，否则下一 turn 迟到的 result 会污染结果。

## Round 236 — 5-agent 并行 code review 第 46 轮（2026-05-24）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描，共 88 项发现。本轮直接修 8 处（cron Stop 等 GC goroutine 退出 [R236-GO-01] / cron addJobAcquiringLock persist 失败回滚 entry [R236-GO-10] / cron UpdateJob 注册失败回滚 schedule 字段 [R236-QA-08] / cron loadJobs Lstat 拒绝符号链接 CWE-59 [R236-SEC-01] / cron trimJobLocked sort 与 diskListNewestFirst 对齐 [R236-QA-01] / cron validateSchedule 拒绝 interval<=0 [R236-QA-07] / server send.go 删除入口冗余 BroadcastSessionsUpdate 全量扇出 [R236-PERF-01]，外加 R236-SEC-12 在 c337d68 已修复故关闭）。下方为本轮新发现且不适合直接修的 NEEDS-DESIGN 条目。

### 安全（剩余）

- [ ] **R236-SEC-02 — 主 dashboard CSP 仍含 `'unsafe-inline'`（P1）**: `internal/server/dashboard.go:486` 主页 CSP `script-src 'self' 'unsafe-inline'`，登录页（dashboard_auth.go:236）已用 SHA-256 hash 白名单消除 unsafe-inline。任何 DOM-XSS 入口都可升级为完整会话劫持。方案：内联脚本改 nonce/hash CSP 或外联到 dashboard.js。Breaking：否，但需逐脚本测试。
- [ ] **R236-SEC-03 — requestHost 与 clientIP 信任点方向相反（P1）**: `internal/server/csrf.go:29-30` 取 X-Forwarded-Host **第一个**值，`internal/netutil/clientip.go:38` 取 X-Forwarded-For **最后一个**值。trustedProxy=true 部署下，攻击者可用可控 X-Forwarded-Host 绕过 CSRF Origin 比对。方案：requestHost 也改取最后一个（与 ALB/CloudFront 追加语义一致）。Breaking：否（仅 trustedProxy=true）。
- [ ] **R236-SEC-04 — diskListNewestFirst 仅靠 `e.Type()` 检测 symlink（P2）**: 某些文件系统（部分 tmpfs / NFS）`d_type==DT_UNKNOWN` 时 `e.Type()` 返 0 而非 ModeSymlink，trimJobLocked 通过文件名直 Remove 可能删 symlink 目标。方案：对 `.json` 条目额外 `e.Info()` 或 `os.Lstat` 双重确认。Breaking：否。
- [ ] **R236-SEC-05 — JSONL transcript prefix 检查双侧加分隔符（P2）**: `internal/server/dashboard_cron_transcript.go:270-272` `resolved+sep` 与 `resolvedRoot+sep` 比较，与 validateWorkspace/workDirUnderRoot 模式不一致，未来误改易引入路径遍历。方案：统一为 `!strings.HasPrefix(resolved, root+sep) && resolved != root`。Breaking：否。
- [ ] **R236-SEC-06 — nz_anon cookie WS 升级时未签名验证（P2）**: `internal/server/wshub.go:420-435` dashToken=="" 模式下从 nz_anon cookie 读取 uploadOwner，但 cookie 值用户可控。攻击者可伪造 nz_anon 串获取共享 NAT 下其他用户上传文件。方案：HMAC 签名（参照 cookieMAC）或在注释明确单用户假设。Breaking：否。
- [ ] **R236-SEC-07 — cron prompt 持久化路径未拦截 bidi 字符（P2）**: `internal/cron/scheduler.go:2143-2144` 调 ResolveAgent 后，cleanText 经 sanitiseRunResult 写入 IM 通知，bidi（U+202A-202E/2066-2069）可翻转 IM 渲染。方案：loadJobs 的 containsCronC0 加 bidi/LS/PS 检查。Breaking：否。
- [ ] **R236-SEC-08 — handleList 1Hz 全量返回完整 prompt（P2）**: `internal/server/dashboard_cron.go:446-454` 50 jobs × 8 KiB = 每秒 400 KiB，token 泄漏后单次 GET 即拉走所有 prompt。方案：列表截断 256 字节，详情走 GET /api/cron/{id}；前端搜索改服务端。Breaking：是（前端需调整）。
- [ ] **R236-SEC-10 — cookie_secret WriteFile 失败仅 slog.Warn 静默降级（P2）**: `internal/server/server.go:295-354` MkdirAll/WriteFile 失败 fallthrough 用内存 secret，导致每次重启所有会话失效。方案：升级为 slog.Error 或返回 fatal error。Breaking：是（启动行为）。
- [ ] **R236-SEC-11 — moveToShimsCgroup busctl args 缺 scope 名字符集校验（P3）**: `internal/shim/manager_linux.go:106-130` shimPID 当前来源安全，但 buildBusctlArgs 缺 scope 名正则验证，未来重构有注入隐患。方案：scope name 正则 `[a-zA-Z0-9\-.]` 验证。Breaking：否。
- [ ] **R236-SEC-13 — handleRunTranscript run.WorkDir 未做 C0/bidi 验证（P3）**: `internal/server/dashboard_cron_transcript.go:239` ClaudeProjectSlug(run.WorkDir) 编码若不完全转义路径分隔符可能产生 ../。方案：调 ClaudeProjectSlug 前对 WorkDir 加 utf8.ValidString + IsLogInjectionRune 检查。Breaking：否。
- [ ] **R236-SEC-14 — 主 dashboard CSP `img-src` 含 `data:`（P3）**: `internal/server/dashboard.go:486` 在 R236-SEC-02 unsafe-inline 存在前提下，data: URI 可被用作 XSS 数据外泄通道。方案：与 R236-SEC-02 一并升级 CSP 时收紧 img-src 为 `'self' blob:`。Breaking：否（需确认前端无合法 data: 图片）。
- [ ] **R236-SEC-15 — notifyTarget chunk × retry 复合超时可能超 30s 预算（P3）**: `internal/cron/scheduler.go:2845-2879` ReplyWithRetry 内部超时未必使用 replyCtx 剩余时间。方案：限制最大 chunk 数（如 5）+ 确认 ReplyWithRetry 使用传入 ctx。Breaking：否。

### Go 正确性（剩余）

- [ ] **R236-GO-02 — cacheGet 释放-重取 TOCTOU 漏掉刚 Append 的 run（P1）**: `internal/cron/runstore.go:340-370` warmCache 完成前另一 Append 的 cacheHeadPush 因 warm==false 是 no-op，warmCache 的 disk snapshot 不含该 run，Recent 漏返直到下次 Append。方案：warmCache 在 jobLock 持有期间完成 disk read 与 entry.warm=true。Breaking：否。
- [ ] **R236-GO-03 — trimJobLocked 锁契约靠注释而非编译期断言（P1）**: `internal/cron/runstore.go:628` "caller must hold jobLock" 仅注释。方案：debug build 加 TryLock 断言 + 收拢调用点为 trimJobUnderLock。Breaking：否。
- [ ] **R236-GO-04 — DeleteJobByID persist 失败但 in-memory 已删 → 复用 ID 时继承旧 runs 目录（P2）**: `internal/cron/scheduler.go:1202-1226`。方案：runStore.DeleteJob 移到 ErrPersistFailed 检查前；或 trimAll 加 orphan 检测。Breaking：否。
- [ ] **R236-GO-05 — Stop deadline 命中后 triggerWG.Wait goroutine 永久泄漏（P2）**: `internal/cron/scheduler.go:971-980` 注释自陈"intentional orphan"。方案：scheduler 加 stopCh + TriggerNow goroutine select stopCh 短路。Breaking：否。
- [ ] **R236-GO-07 — sendCtx 使用 context.Background 让 Send 不被 stopCtx 取消（P2）**: `internal/cron/scheduler.go:2259` Stop 返回后 cron job Send 可继续 jobTimeout（默认 5min），可能持已被 Router.Shutdown 回收的 session。方案：sendCtx 父 ctx 改 stopCtx + 增加 execWG.Wait。Breaking：否。
- [ ] **R236-GO-08 — Append jobLock 持锁期间 trimJobLocked 做磁盘 I/O 阻塞并发 Recent（P2）**: `internal/cron/runstore.go:228-235` 慢盘上 200ms+ 阻塞。方案：trimJobLocked 拆两阶段（锁内决名单，锁外 Remove）。Breaking：否。
- [ ] **R236-GO-09 — runDeadlineWatchdog InterruptViaControl 阻塞导致 finishRun 永不调用（P2）**: `internal/cron/scheduler.go:2003-2013` 后续触发因 inflight.running=true 全跳过。方案：watchdog 内 InterruptViaControl 加 3s 超时。Breaking：否。
- [ ] **R236-GO-11 — cacheHeadPush O(N) memmove 在 entry.mu 下阻塞 cacheGet（P2）**: `internal/cron/runstore.go:302-325`。方案：改 ring buffer (固定数组 + head 指针) O(1) 头插尾删（与 R230C-GO-13/R233-PERF-2 同根因，主条目跟踪）。Breaking：否。

### 性能（剩余）

- [ ] **R236-PERF-02 — KnownSessionIDs 每次调用全量扫 200 runs/job 无缓存（P1）**: `internal/cron/scheduler.go:472-511` cold 状态下 50 jobs × 200 runs = 10k ReadFile 在单 HTTP 请求路径，O(hundreds ms) latency spike。方案：atomic.Pointer 缓存 + recordResult/deleteJob 主动失效；或 IsExcluded 改查 LastSessionID + inflight 跳过历史。Breaking：否。
- [ ] **R236-PERF-04 — eventlog Append 单条路径每次分配 `[]EventEntry{e}`（P2）**: `internal/cli/eventlog.go:790-888`，PersistSink 签名要求 slice。同根因 R230C-PERF-2 主条目跟踪。Breaking：是（接口签名变化）。
- [ ] **R236-PERF-06 — handleSubscribe 每次扫 maxWSConns 个 client 计 per-key 订阅数（P2）**: `internal/server/wshub.go:621-656` 在 h.mu.Lock 下，subscribe 阻塞所有广播 RLock。方案：subscriberCounts map 维护 O(1) 计数。同根因 R230C-PERF-4，但可重新评估。Breaking：否。
- [ ] **R236-PERF-07 — diskListNewestFirst pagination 路径全量 ReadFile bypass 缓存（P2）**: `internal/cron/runstore.go:468-545` 200 runs × ReadFile = 6.4MB syscall。方案：mtime 排序后 binary search before cutoff，跳过不需要的 ReadFile。Breaking：否。
- [ ] **R236-PERF-08 — handleList 50 jobs 串行调 RecentRuns 各持 entry.mu（P2）**: `internal/server/dashboard_cron.go:510`。方案：BatchRecentRuns(jobIDs, n) 单次遍历或 bounded goroutine 并发。Breaking：否（新 API）。
- [ ] **R236-PERF-09 — warmCache jobLock 持锁期间 200×ReadFile + Unmarshal（P2）**: `internal/cron/runstore.go:376-392` 多 job warm 起手时 50 个 I/O 重 goroutine 占用线程。方案：先 meta scan 列文件名（短锁）+ 锁外 ReadFile + 二次锁写 entry.runs。Breaking：否。
- [ ] **R236-PERF-10 — eventlog notifySubscribers map iter vs slice（P2）**: `internal/cli/eventlog.go:1039-1051` 500 sessions × 5 events/s = 2500 RLock+map iter/s。方案：改 `[]*subscriber` slice + swap-and-shrink unsub。Breaking：否。
- [ ] **R236-PERF-11 — ListAllJobsWithNextRun 每 entry 独立 robfig.Entry() 锁（P2）**: `internal/cron/scheduler.go:1125-1146`。方案：一次 cron.Entries() + entryID→Next map。Breaking：否。
- [ ] **R236-PERF-12 — trimJobLocked 重复 ReadDir+Sort 与 diskListNewestFirst 不复用（P2）**: `internal/cron/runstore.go:628-707`。方案：cache.warm 时直接用 cache 算"是否需要 trim"，绝大多数情况跳过 ReadDir。Breaking：否。
- [ ] **R236-PERF-13 — Snapshot 含 SetModel 写副作用（P2）**: `internal/session/managed.go:1151-1275` 每次 dashboard 1Hz × N tabs 触发 atomic store。方案：model 同步移到 readLoop / Send 结束，Snapshot 改纯读。Breaking：否。

### 代码质量 / 架构（剩余，挑选高价值）

- [ ] **R236-QA-03 — UpdateJob 在 s.mu.Lock 下调 cron.Add/Remove 可能锁顺序倒置（P1）**: `internal/cron/scheduler.go:1383-1397` 与 ListAllJobsWithNextRun 注释揭示的锁顺序冲突。pauseJobLocked/resumeJobLocked 同样存在。方案：研究 robfig/cron v3 锁语义；不安全则参照 ListAllJobsWithNextRun 模式释锁后操作 cron。Breaking：否。
- [ ] **R236-QA-05 — sysession DaemonRun ErrorClass 缺 Canceled 枚举（P2）**: `internal/sysession/run.go:152` State=canceled 但 ErrorClass="" 与成功无法区分。方案：新增 DaemonErrorClassCanceled 枚举值。Breaking：否（wire format）。
- [ ] **R236-QA-09 — auto_titler 手写 insertion sort 维护风险（P2）**: `internal/sysession/auto_titler.go:280-284` `>=` 一改即死循环。方案：slices.SortFunc + batchPerTick ≤100 上限。Breaking：否。
- [ ] **R236-QA-13 — readJSONWithRetry 不区分 file-missing vs JSON-invalid（P2）**: `cmd/naozhi/main.go:97-112` 损坏 settings.json 仅 Warn。方案：errors.Is(fs.ErrNotExist) 走 Warn 其余走 Error 提示运维。Breaking：否。
- [ ] **R236-QA-16 — loadJobs 缺 Schedule/WorkDir 字段验证（P3）**: `internal/cron/store.go:109-153` 仅校 ID/Prompt/Title/Backend；超长 Schedule + 非 UTF-8 WorkDir 进 slog 可 log-injection。方案：与现有 prompt/title 验证对齐加 len + utf8.ValidString。Breaking：否。
- [ ] **R236-QA-17 — sysession runner argv 硬编 "-p --output-format text --setting-sources" 与 cli 包参数集脱钩（P3）**: `internal/sysession/runner.go:119-131`。方案：抽 runnerImplBaseArgs 包级常量 + 注释同步责任。Breaking：否。
- [ ] **R236-QA-20 — isNaozhiCallbackHook 用 strings.Contains "127." 误报合法 URL（P3）**: `cmd/naozhi/main.go:364-384`。方案：正则匹配 IP 前缀或确认 host:port token 格式。Breaking：否。
- [ ] **R236-ARCH-06 — dispatch.Dispatcher 仍持具体 *cron.Scheduler 而非 interface（P1, is_localized=true）**: `internal/dispatch/dispatch.go:67-71` 与 R235-ARCH-3 同根因，本条提供具体方案：`internal/dispatch/cron_consumer.go` 定义 ~8 method CronScheduler interface + contract test。Breaking：否（接口断言迁移）。
- [ ] **R236-ARCH-07 — cron.scheduler 直 import platform 绕过 dispatch 中文化/replyError 计数（P1）**: `internal/cron/scheduler.go:25` 第三条出口管线。本条提供窄方案：dispatch.Notifier interface 注入到 scheduler 替代 platforms map（比 SendOrchestrator RFC 改动面小 90%）。同根因 R230C-ARCH-3 主条目跟踪。
- [ ] **R236-ARCH-12 — Dispatcher closure-pattern sendFn/takeoverFn 缺编译期非 nil 保证（P2, is_localized=true）**: `internal/dispatch/dispatch.go:125-209`。方案：抽 SessionSender interface（与 takeoverFn 同类）+ NewDispatcher 必填字段。Breaking：是（dispatchConfig 字段类型）。
- [ ] **R236-ARCH-19 — jobSnapshot 字段集靠注释手工维护，无 contract test（P3, is_localized=true）**: `internal/cron/scheduler.go:1801-1830` 新加 Job 字段不会自动同步进 snapshot，executeOpt 路径有读 *Job 风险。方案：grep contract test 锁定 executeOpt 不读 *Job 字段；或函数内 shadow `_ = (*Job)(nil)`。Breaking：否。

## Round 235 — 5-agent 并行 code review 第 45 轮（2026-05-23）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描，本轮直接修 18 处（见顶部摘要 R235-CR-1/2/5/6/10/11/12/13、R235-SEC-1/3/4/5/6/7/8/9、R235-GO-1/5/7、R235-PERF-17/20）。下方为本轮新发现且不适合直接修的条目（破坏兼容 / 跨包重构 / 需 RFC / 方案不唯一）。

### 架构（高优先）— 本轮新发现

- [ ] **R235-ARCH-1 — `internal/cli/backend` 反向 import 父包 `internal/cli`（P1）**：backend.Profile.NewProtocol 直接构造 `cli.ClaudeProtocol` / `cli.ACPProtocol`，导致 cli 想用 backend.Get 必须 blank-import 或硬编码 wrapper.go 的 `backendDisplayName` / `isKnownBackendID` switch（自陈债）。建议把 backend 提升为兄弟包 `internal/clibackend` 或 `internal/agentprofile`，反转为 cli→backend；最小路径是 backend 仅暴露 Profile 元数据（DisplayName/DefaultBinary/DetectInProc/HistoryDir/CostUnit/Features），把 NewProtocol 反向到 cli 包内（`cli.RegisterProtocolFactory`）。Breaking: 是（仅 internal）。
- [ ] **R235-ARCH-2 — cron / sysession 各自定义 RunState / TriggerKind / ErrorClass（P1）**：两套字符串枚举语义高度重叠（succeeded/failed/timed_out/canceled、scheduled/manual、validation/upstream/timeout/panic）。注释自陈"Mirrors cron.RunState semantics"。建议 `internal/runschema` leaf 包共享 `type State string` / `type TriggerKind string` / `type ErrorClass string`，cron/sysession 改 alias。dashboard.js 也只剩一组常量。Breaking: 否（type alias 源码兼容）。
- [ ] **R235-ARCH-3 — `dispatch.Dispatcher.scheduler` 仍持具体 `*cron.Scheduler`（P1）**：cron→session 已用 SessionRouter interface 反转，server→session 用 HubRouter，唯独 dispatch→cron 没抽 consumer interface。建议 `dispatch/cron_consumer.go` 定义 `CronScheduler` interface（AddJob / NextRun / ListJobs / DeleteJob / PauseJob / ResumeJob 6 method），contract_test 加 `var _ dispatch.CronScheduler = (*cron.Scheduler)(nil)`。
- [ ] **R235-ARCH-4 — `cli.Wrapper.ShimManager` 是导出可变 `*shim.Manager`，protocol/transport 未拆分（P1）**：注释自陈"R230-ARCH-13 / R231-ARCH-7 已知债"。sysession.Runner 已走绕过 shim 的旁路 → transport 抽象缺失。建议 `cli.Transport` interface（StartSession / Reconnect / Close），ShimManager 适配为它的实现；新增 `WithTransport` option。Breaking: 是（cmd/naozhi/main.go 一处真消费方）。
- [ ] **R235-ARCH-5..30 — 见各 reviewer 报告**：包含 config 反向 import session/project（ARCH-5）、3 个 workspace 概念名共享（ARCH-6）、platform.QuestionItem 与 cli.AskQuestionItem 双向手抄（ARCH-7）、feishu→transcribe 直依（ARCH-8）、cron / dispatch 各有 prompt/schedule 校验二级实现（ARCH-9）、Router struct 字段 30+（ARCH-10）、cli 包 67 文件混杂（ARCH-11）、contract_test 未覆盖 dispatch.CronScheduler / sysession.Manager（ARCH-12）、session 通过 blank import 触发 history backend init（ARCH-13）、SessionGuard / MessageQueue 运行时 either-or（ARCH-14）、server 13 个 *Handlers god struct（ARCH-15）、cli.Process 字段导出与并发约束注释冲突（ARCH-16）、eventlog/schema 未承担 single source of truth（ARCH-17）、dispatch.DispatcherConfig.Router 字段类型 *session.Router（ARCH-18）、router.Version 同时承载 data + render（ARCH-19）、process.go 1500+ 行字段 60+（ARCH-20）、discovery 同时依赖 cli + cli/backend（ARCH-21）、replyTagForBackend sync.Once 兜底掩盖 wireup 时序（ARCH-22）、Reactor / QuestionCardSender type-assertion 模式（ARCH-23）、scheduler.go 2400+ 行混合多职责（ARCH-24）、sysession.Runner 硬编码 claude bin（ARCH-25）、Wrapper.Spawn 100 行 protocol+transport 混杂（ARCH-26）、validateWorkspace / cron.workDirUnderRoot 重复（ARCH-27）、cli 包同住 image/thumbnail/askquestion/todo DTO（ARCH-28）、ManagedSession.process 直接持 *cli.Process（ARCH-29）、缺 wireup 集中包（ARCH-30）。所有 ARCH 类条目均为方案不唯一 / 跨模块改动，按 Round 节存档供未来 RFC 引用。

### Go 正确性 / 性能（合并到现有跟踪）

- [~] **R235-GO-2 — `runStore.warmCache` 签名暗示返错却 always nil**：实际逻辑正确（fallback 到 diskListNewestFirst），文档与签名分叉但行为安全；判定 P3 cosmetic，归到 R231-PERF-1 主条目跟踪。
- [ ] **R235-GO-3 — `cron.Stop()` 包装 goroutine 在 deadline-hit 路径永久泄漏**：注释 R222-GO-10 已承认 intentional orphan；非 production 影响，仅 goroutine-leak 检测器在 test 中报泄漏。需要 RFC 决定是否在 godoc 中显式记录这是 by-design 的 leak，或加 ctx 信号让 goroutine 退出。
- [ ] **R235-GO-4 — `buildExcerptFromHistory` 跨行截断后 EXCERPT marker 检测失效（P1）**：512 bytes/line 截断可能把 `---BEGIN CONVERSATION EXCERPT---` 切到两行，使 ReplaceAll 检测不到完整 marker。建议在 `buildExcerpt` 内逐行处理阶段就替换 marker，而非仅末尾一次性扫描。改动 sysession/auto_titler.go 内部，无 breaking。
- [ ] **R235-PERF-1 — `Protocol.ReadEvent(line string)` 强制 string→[]byte 堆拷贝（P1）**：`shimMsg.Line` 是 `string` 是根因，两处改动需同步：shim 协议字段改 `json.RawMessage`，ReadEvent 签名改 `[]byte`。同根因主条目持续跟踪 R231-PERF-1。Breaking: 是（shim wire format + Protocol interface）。
- [ ] **R235-PERF-2 — `shimWriter` 写出热路径 string 双拷贝（P1）**：`shimClientMsg.Line` 同样的根因；与 R235-PERF-1 同批 ship。
- [ ] **R235-PERF-3 / R235-CR-11 — `cacheHeadPush` 改 ring buffer**：当前 O(N) 头插，改 head 指针 + 定长数组 → O(1)。本轮已修注释，正式 ring buffer 改造留 NEEDS-DESIGN。
- [ ] **R235-PERF-4 / R235-PERF-11 — `IsExcluded` / `KnownSessionIDs` 高频调用无 memoize**：建议 atomic.Pointer[map] + TTL 30s 缓存，仅在 recordResultP0 / 删除 job 时无效化。归 R231-PERF-7 主条目。
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
- [ ] **R235-CR-8 — scheduler.go 3038 行**：拆 `scheduler_exec.go` / `scheduler_notify.go` / `scheduler_persist.go`（同 R235-ARCH-24 路线）。
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
- [ ] **R234-ARCH-8 — `server.Hub` 直 import dispatch+cron+cli+project+session（P2）**：典型 god hub。建议拆 `WSTransport`（仅 conn lifecycle）+ `WSCoordinator`（业务），需独立 RFC。
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

- [ ] **R234-GO-1 — runstore.cacheHeadPush O(N) memmove 仍未修（P1）**：`append+copy` 对 keepCount=200 slice 触发 200-element memmove 持 entry.mu 期间执行；1Hz×50 jobs = 250 次/s cache-line 污染。建议 `recentCacheEntry` 改 ring buffer（head int + buf [keepCount]CronRunSummary），head/tail O(1) prepend。同根因主条目 R233-PERF-2。
- [ ] **R234-GO-3 — scheduler.go:778 `go trimAll` goroutine 无 WaitGroup（P1）**：Stop 不等待此 goroutine 退出，半删 runs 目录残留 / 重启并发 trimAll 可能与 per-job lock 之外的 ReadDir+Remove 出现窗口。建议给 `trimAll` 加 `s.gcWG sync.WaitGroup`，Stop 先 gcWG.Wait()（带短超时）+ 传 ctx 让 trimAll 内每个 jobID 循环检查 `ctx.Err()`。
- [ ] **R234-GO-4 — runstore.cacheGet 双锁窗口（P2）**：第一次释放 entry.mu 后 warmCache 在 entry.mu.Lock 前另一 Append 已 cacheHeadPush no-op + trimJobLocked 触发 cacheTrimAfterDisk no-op，warmCache 再读磁盘可能漏掉刚 Append 条目。建议在 warmCache 内 entry.mu.Lock 前先 jobLock.Lock（已有此模式）。
- [ ] **R234-GO-5 — sysession.Manager.Stop wg.Wait goroutine 无 WG 跟踪（P2）**：osExit(2) 兜底当前安全（进程死亡），但若测试替换 osExit 为 panic-recovery，goroutine 永久阻塞。建议加注释 `// goroutine intentionally abandoned — osExit terminates the process` 或改为外层 select 不再包 goroutine。
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
- [ ] **R233B-PERF-2 — shimWriter.Write 快速路径 `string(data[:len(data)-1])` 强制拷贝（P1）**: 每 WriteMessage 都过该路径。方案：shimClientMsg.Line 改 json.RawMessage 或加 lineBytes 字段 + custom MarshalJSON。Breaking：否。
- [ ] **R233B-PERF-3 — runstore cacheHeadPush 仍是 O(N) 左移（P1，独立于 R232-PERF-8 的 trim 优化）**: keepCount=200 时每次 Append 200-element memcopy，1Hz×50 jobs ≈ 50 次/s。方案：换 ring buffer / container/ring，head 指针 O(1)；同时缩短 entry.mu 持锁时间（与 cacheGet 双锁路径）。Breaking：否（私有实现）。
- [ ] **R233B-PERF-4 — readLoop 每行做两次 json.Unmarshal（外层 shimMsg + 内层 ReadEvent）（P2）**: 第一次解 `{"type","line"}` 协议帧，第二次解嵌入 claude 事件。方案：shimMsg.Line 改 json.RawMessage 直传 ReadEvent；或外层手写字节扫描 type 分支。配合 R233-PERF-1 一次解决。Breaking：否。
- [ ] **R233B-PERF-5 — KnownSessionIDs 无缓存，loadHistorySessions 每 120s 触发一次 O(jobs×200 copy)（P2）**: Scheduler 加 30s TTL 缓存或随 historyCache 一并缓存即可。Breaking：否。
#### 安全（P1/P2）

- [~] **R233B-SEC-1 — dashboard CSP 含 unsafe-inline script-src 与 style-src（归档 2026-05-23）**: 同 R231-SEC-2 / R233-SEC-1 一并归档，本批 PR
- [ ] **R233B-SEC-2 — Feishu VerificationToken-only 模式缺少 body HMAC（P1）**: token 泄露即可伪造任意事件体。方案：要么强制 EncryptKey；要么把"VerificationToken-only"明确标 deprecated 并加运行时启动告警。Breaking：是（运维侧）。
- [ ] **R233B-SEC-3 — project_files serveRender CSP 含 `allow-scripts + 'unsafe-inline' 'unsafe-eval'`（P2）**: 当前 octet-stream + attachment 头让脚本不会执行，但与未来 Content-Type 改回 text/html 一行就会暴雷。方案：移除 allow-scripts，依赖 blob opaque origin；或保留 allow-scripts 但补单测保证 Content-Type 永远是 octet-stream。Breaking：否。
- [ ] **R233B-SEC-4 — publicTmpProject 把 /tmp 暴露给认证用户（P2）**: 多用户机器上 /tmp 可能含其他服务 socket/证书。方案：文档明确"单用户部署"前提；或 publicTmpAllowedExtensions 白名单。Breaking：否。
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

- [ ] **R232-ARCH-1 — internal/cron/scheduler.go 2870 行 / 67 函数 god file（P1）**: scheduler.go 已汇集 CAS gate / jitter / preflight / watchdog / recordResult 双 / persist seq gate / deliverNotice / redactPaths / slogPrintfLogger 全部职责。任何修改都要在 ~3000 行内追锁顺序。方案：参考 router-split RFC 拆 5–6 个文件（lifecycle / jobs / execute / finish / persist / core）。Breaking：否（纯文件移动）。合并 R231-ARCH-N。
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
- [ ] **R231-ARCH-5 — Hub 与 Router god-object 双胞胎共同导致 Channel Adapter 抽象塌陷（P1）**: server.Hub 退化为第二个 Router（同时持 router/scheduler/scratchPool/queue/dedup/uploadStore/auth/tailers/nodes）；webhook 进来后路径从"Adapter→Router→Wrapper"变成"Adapter→{Hub/Server/Dispatcher} 三方共享状态"。方案：抽 WSEventBus / nodeRegistry / SendCoordinator 子聚合。
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
- [ ] **R231-PERF-9 — eventlog Append 单元素 sink 路径仍有 EventEntry 大 struct copy（P2，与本轮 sink-nil 早返回互补）**: 480+ B struct + slice header 都逃逸；本轮已加 sink-nil 早返回但 sink-attached 路径仍有 alloc。方案：invokePersistSinkOne 单条专路或 EventEntry 字段瘦身。Breaking：是（ring buffer 重新 benchmark）。
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
- [ ] **R230B-ARCH-17 — EventLog cli/persist/schema 三层职责切分不清（P2）**: cli/eventlog.go 同时持内存层 + replayPhase 状态机 + sanitize 节奏。方案：cli 退化为内存接口，持久化语义下沉 persist。Breaking：是。
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
- [ ] **R229-SEC-6 — Dashboard CSP `script-src 'unsafe-inline'`（P2）**: `dashboard.go:390` 主页面允许任意内联 script，登录页已用 SHA-256 hash 严格 CSP。方案：迁移到 nonce 模式，把 dashboard.html 内联 onclick 等事件外移。Breaking：是（前端较大改造）。
- [ ] **R229-SEC-8 — per-token WS 连接数无上限（P2）**: `wshub.go:307` 单 token 持有者可建 500 个 WS 连接绕过 maxSubscribersPerKey=20。方案：WS 升级时按 cookie MAC 或 IP 桶检查 per-token 连接 cap（如 20）。Breaking：否。
- [ ] **R229-SEC-9 — sandbox CSP `style-src 'unsafe-inline'`（P3）**: `project_files.go:730/928` 渲染沙箱允许内联 style，存在 CSS exfiltration 风险。方案：替换为 nonce/hash。Breaking：是（依赖内联 CSS 的报告渲染失败）。
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

- [ ] **R226-SEC-2 — Dashboard 主页 CSP `script-src 'self' 'unsafe-inline'` 完全打掉 XSS 防护（P1）**: 任何注入到 dashboard HTML 或同源 JS 文件的脚本都能直接执行；登录页已用 hash 收敛了，主页应迁移到 nonce 或 per-script SHA-256。Breaking：需把 dashboard.html 内联事件 handler 全部抽到外部文件 + 加 nonce/hash。涉及 `internal/server/dashboard.go:389` + 全部内联脚本审计。
- [ ] **R226-SEC-3 — 反向 node 在 `ws://`（无 TLS）下传 token 仅 `slog.Warn` 不阻断（P2）**: token 在第一条 WS 消息明文，passive 观察者可截获并冒充 node。方案：primary 端 `/ws-node` 加 `require_tls: true` 配置，默认 reject 非 wss 升级，除非显式 `insecure: true`。Breaking：现有 `ws://` 部署需加配置或迁 `wss://`。`internal/upstream/connector.go:210` + `internal/node/reverseserver.go:155`。
- [ ] **R226-SEC-9 — Sandbox CSP `style-src 'unsafe-inline'`（P3）**: `serveRender`/`serveRaw` 仍允许 inline CSS，CSS exfiltration 攻击面在沙箱内仍存。方案：迁 nonce 或 hash。`internal/server/project_files.go:708,905`。

### 性能 — 本轮新发现

- [ ] **R226-PERF-1 — `Protocol.ReadEvent(line string)` 每事件 `[]byte(line)` 堆拷贝（P1，封 R67-PERF-1 实施分支）**: 5-50 ev/s × N session 的强制 alloc。方案：Protocol 接口签名 `ReadEvent([]byte)`，shimMsg.Line 改 `json.RawMessage`。Breaking：是（接口）。
- [ ] **R226-PERF-4 — ACP `agent_message_chunk` 每 chunk 一次 `json.Unmarshal`（P2）**: kiro streaming 高频路径，500 unmarshal/s 仅此一处。方案：手写 byte-scan 提取 `"text":"..."` value，跳过 reflect。`internal/cli/protocol_acp.go:517`。
- [ ] **R226-PERF-5 — `eventlog.Append` 单条调用每次造 `[]EventEntry{e}` slice（P2）**: PersistSink 接口允许 retain slice 故复用受限。方案：加 `AppendOne(e)` 快路径或单元数组池。`internal/cli/eventlog.go:660`。
- [~] **R226-PERF-6 — `EventLog.applyEntryStateLocked` task 事件线性扫 turnAgents/bgAgents（P3）**: 多路 subagent 场景（>8 并行）双重 O(n)。方案：当 `len > 8` 时建 `map[string]int` 索引。`internal/cli/eventlog.go:405`。 — 评估后不实施（typical turnAgents len 1-3，result/user 事件已自动重置；threshold-based map 需 4 个同步映射 cover ToolUseID/TaskID × turn/bg，维护成本远高于收益；P3 + 无 >8 subagent 实测案例），本批 PR #164
- [ ] **R226-PERF-10 — `process_shim_io.shimWriter.Write` fast path `string(data[:len-1])` 拷贝（P3，封 R71-PERF-H1）**: shimClientMsg.Line 改 `json.RawMessage`。

### 代码质量 — 本轮新发现

- [ ] **R226-CR-7 — `RegisterForResume` / `RegisterCronStubWithChain` 用 `r.CLIName/Version` 在多 backend 部署下显示错误（P1）**: 这两条路径只看默认 wrapper；router_core.go loadStore 已正确走 `wrapperFor(entry.Backend)`。方案：加 `backend string` 参数 → `wrapperFor`。Breaking：caller API。`internal/session/router_discovery.go:259,362`。
- [~] **R226-CR-10 — `cron/scheduler.executeOpt` 320 行 7 失败分支（归档 2026-05-23）**: R229-CR-1 / R230-CQ-9 / R232-ARCH-12 同根因多轮重申。统一收敛到 R232-ARCH-12 跟踪。
- [~] **R226-CR-11 — `cron/scheduler.go` 2739 行单文件无拆分计划（归档 2026-05-23）**: R230-CQ-14 / R232-ARCH-1 同根因多轮重申。统一收敛到 R232-ARCH-1（5–6 文件 lifecycle/jobs/execute/finish/persist/core 拆分方案）跟踪。
- [~] **R226-CR-12 — `wshub.go` 1785 行 + `feishu.go` 1461 行无拆分计划（归档 2026-05-23）**: R224-ARCH-14 / R230B-ARCH-26（feishu 4 文件 transport/wire/outbound/capability 拆分） / R230-ARCH-15（server 90+ 文件按子包拆分） 同根因多轮重申。统一收敛到 R230B-ARCH-26 + R224-ARCH-14 跟踪，跨文件重构需 ADR。

### 架构 — 本轮新发现

- [ ] **R226-ARCH-1 — `server` 包成"上帝包"（P1）**: 92 个 .go + 12 子 handler，承担路由+UI+业务编排+Hub+nodeCache。建议：薄壳 server + 拆 `internal/server/api/{cron,project,scratch,discovery,...}` 子包；Hub/nodeCache 走显式注入。需 RFC。
- [~] **R226-ARCH-2 — `KeyResolver` 在 main/server/dispatch 三处独立构造（归档 2026-05-23）**: 实地核查 cmd/naozhi/main.go:834 + internal/server/server.go:551 + internal/dispatch/dispatch.go:195 三处仍各自 `session.NewKeyResolver(agents, project.NewDataSource(...))`。R230-ARCH-9 / R224-ARCH-12 / R229-ARCH-5 / R216-ARCH-3 同根因多轮重申。统一收敛到 R230-ARCH-9（projectMgr.Resolver() 单点暴露方案）跟踪，跨包构造签名变更 NEEDS-DESIGN。
- [ ] **R226-ARCH-3 — dispatch `sendFn` 接 `*session.ManagedSession + cli.SendResult` 破坏分层（P1）**: dispatch 名义有 `SessionRouter` 接口但发送闭包绕过。方案：定义 dispatch.Sender 接口，server 实现，dispatch 不再 import cli/session 具体类型。
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
- [ ] **R225-PERF-8 — `shimWriter.Write` fast path `string(data[:len-1])` 强制 byte→string copy（P2）**: process_shim_io.go:54 每条 stdin 一次必要 alloc。方案：`shimClientMsg.Line` 改 `[]byte` + 自定义 Marshaler，或在已知 data 不被改时用 `unsafe.String`。
- [~] **R225-PERF-9 — `wshub.eventPushLoop` 同一 session 多 WS 各自 marshal（归档 2026-05-23）**: 同 R230B-PERF-1 / R219-PERF-1 主条目跟踪。eventPushLoop 是 per-subscription 独立 goroutine 各持 lastTime 游标；两个订阅者可能在不同 lastTime 上请求不同 entry slice，无法简单一次 marshal 共享 byte 引用。需统一时间游标——RFC 级改造。本批 PR 归档。
- [~] **R225-PERF-10 — `marshalPooled` 每次 copy 一份独立 backing（归档 2026-05-23）**: session_state 字符串 enum 集合小但 Reason 字段任意（含 err 文本），LRU key 空间不可控；本批检查 broadcast 路径已有 doBroadcastSessionsUpdate 一次 marshal 多次 SendRaw 收敛热路径。本批 PR 归档。
- [~] **R225-PERF-14 — `wsclient.sweepSubGenExpiredLocked` 在 hub 写锁下扫 map（归档 2026-05-23）**: c.subGen map 与 c.subscriptions map 共用 h.mu 保护是显式契约（wsclient.go:127 注释说明）；移到 client-local mutex 需要 2 层锁协议；扫描 map 上限是 maxSubscribersPerClient=50，bounded scan 不是热路径。本批 PR 归档。
- [~] **R225-PERF-17 — `TruncateRunes(string, ...)` 无字节快检（P3，误报关闭 2026-05-20）**: reviewer 提议 `len(s) <= maxRunes*4` 短路其实方向反了——UTF-8 每 rune 1-4 字节意味着 byte 长度 ≤ rune 数*4 是 rune 数的**上界**而非下界，全 ASCII `len=200, maxRunes=50` 时 byte=200 ≤ 200 但 runes=200 > 50，加这种快检会漏截。当前 `len(s) <= maxRunes` 快检已与 `TruncateRunesBytes` 一致，无可优化空间。

### 代码质量 — 本轮新发现

- [ ] **R225-CR-1 — `cli/detect.knownBackends` 与 `backend.Profile` 注册表双轨（P1）**: detect.go:43-46 静态硬编码 Protocol 字符串；与 `Profile.Capabilities().StreamJSON` 不同步会导致 dashboard 误判。方案：`DetectBackendsCtx` 从 `backend.All()` 派生，删 knownBackends。Breaking：否。
- [ ] **R225-CR-2 — `cli.detectCLI` 仍硬编码 `if backend == "kiro"`（P1）**: wrapper.go:157 与 `Profile.DefaultBinary` 重复。方案：detectCLI 调 `backend.Get(backend).DefaultBinary`，删除内联 switch。Breaking：否。
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
- [ ] **R224-GO-6 — `shim/server.go SetReadDeadline` 错误 nolint 静默吞（P2）**: `:654, :680` SetReadDeadline 失败 nolint:errcheck 直接吞；conn 已关闭时后续 ReadBytes 无 deadline 阻塞 goroutine 泄漏；deadline 清除失败时 post-auth 读立即 timeout 踢掉合法客户端。方案：失败时显式关闭 conn 并 return。
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

- [ ] **R222-SEC-1 — Dashboard 主 CSP 仍含 `'unsafe-inline'` script-src/style-src（P1）**: `dashboard.go:384` 主应用 HTML 的 CSP 含 `'unsafe-inline'` 让 XSS 防护实际失效；login 页面已用 hash-based CSP 但主应用未跟。修复需把 dashboard.js 内联段拆出或为每段加 hash，是 frontend 改造；兼 SEC-NEW-4（CDN 在 script-src 但 unsafe-inline 旁路所有 nonce/hash）。涉及：`internal/server/dashboard.go:384`，`internal/server/static/dashboard.js`。
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
- [ ] **R222-ARCH-6 — Hub god-object 苗头（35+ 方法 / 1700 行）（P2）**: 拆 Hub + SubscriptionRegistry + MessageBroker。
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
- [ ] **R217-PERF-4 — `eventlog.go:640` 单元素 `[]EventEntry{e}` heap alloc**: sink 契约允许 retain，需先调整契约或 sink 实现拷贝才能用 stack array。
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
- [ ] **R217-CR-4 — `Hub god struct 36 字段 / `node.Conn` 18+ 方法巨型接口**: 子聚合拆分。
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

- [ ] **R214-SEC-2 — dashboard CSP 保留 `'unsafe-inline'`**: 主 dashboard CSP 对 script-src / style-src 都开 unsafe-inline；登录页已用 hash-based CSP。与 RNEW-SEC-003 合并跟踪。

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

- [ ] **RNEW-SEC-003 — CSP `script-src` 含 `'unsafe-inline'` + `connect-src ws: wss:`（扩展 R172-SEC-H2）**: `dashboard.go:307` CSP 放开 inline script + 明文 ws 连任意目标；CDN 脚本 integrity 已补，但 CSS link integrity 在 link 标签后才 setAttribute，浏览器接受（时序 OK）。根因仍在 10008 行 JS 的 80+ inline onclick（RNEW-UX-006）。
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

- [ ] **R172-SEC-H2 CSP 去 `unsafe-inline`**（Round 172 发现）: `internal/server/dashboard.go:301` 的 `script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net` 让 XSS 没有最后一道 CSP 防线。根因是 `eventHtml` 生成的 HTML 字符串里大量 inline `onclick="..."` handlers。
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

- [ ] **R230C-PERF-2 — eventlog.Append 单条路径每次分配 `[]EventEntry{e}`（P1）**: `internal/cli/eventlog.go:736` 字面切片在 atomic.Pointer sink 调用上逃逸到堆，5-50 events/s × N session 持续 alloc。方案：EventLog 字段缓冲 `sinkOneBuf [1]EventEntry`（Append 已串行）替代 sync.Pool。Breaking：否。
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