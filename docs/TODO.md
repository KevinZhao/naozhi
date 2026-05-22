# TODO

> 最后更新 2026-05-22 Round 232 —— 深度 5-agent 并行 code review 第 42 轮：7 处直接修落地（cron runstore cacheHeadPush O(N)→O(1) prepend / cron Start trimAll 改异步 goroutine / cron trimAll ReadDir 错误升级 Warn / sysession runner stderr 预截 256 / cron previousTickBefore 1000 次迭代上限 / cron recordResult 4*1024→maxStoredResultRunes 常量 + slogPrintfLogger 同时匹配 panic\|recovered + storeMu 注释纠偏 + diskListNewestFirst & trimJobLocked 跳 symlink）+ ~75 NEEDS-DESIGN 归档见 Round 232 节。
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
- [ ] **R67-PERF-1（MED，CLI stdout 热路径）—— `ClaudeProtocol.ReadEvent` 每行 `[]byte(line)` 复制**: `ReadEvent(line string)` 收到已派生 string 再反 `[]byte` 传 `json.Unmarshal`，每行 heap alloc。5-50/s × N 活跃 session。方案：Protocol 接口改 `ReadEvent(line []byte)`，两实现（`protocol_claude.go` / `protocol_acp.go`）+ `readLoop` 调用方同步。涉及 3 个文件 ~15 行。
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

## Round 232 — 5-agent 并行 code review 第 42 轮（2026-05-22）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描发现 ~95 条。本轮直接修 7 处（cacheHeadPush O(N)→O(1) prepend / trimAll Start 改异步 goroutine / trimAll ReadDir 错误升级 Warn / sysession.runner stderr 预截 256 / cron previousTickBefore 1000 次迭代上限 / cron recordResult 4*1024 改 maxStoredResultRunes 常量 / cron slogPrintfLogger 同时匹配 panic|recovered + storeMu 注释从 saveJobs→saveMarshaledSeq + diskListNewestFirst & trimJobLocked 跳过 symlink）。
> 以下是需设计决策、破坏兼容、跨包重构、或方案不唯一不适合本轮直接修的条目。

### 架构（高优先）— 本轮新发现

- [ ] **R232-ARCH-1 — internal/cron/scheduler.go 2870 行 / 67 函数 god file（P1）**: scheduler.go 已汇集 CAS gate / jitter / preflight / watchdog / recordResult 双 / persist seq gate / deliverNotice / redactPaths / slogPrintfLogger 全部职责。任何修改都要在 ~3000 行内追锁顺序。方案：参考 router-split RFC 拆 5–6 个文件（lifecycle / jobs / execute / finish / persist / core）。Breaking：否（纯文件移动）。合并 R231-ARCH-N。
- [ ] **R232-ARCH-2 — recordResult 与 recordResultP0WithSanitised 双轨依然存在（P1）**: 生产路径只调 P0 版本，但 `persist_failure_test.go` 把死路径活成"测试桩"。两条路径 sanitize 参数易漂移（已通过 R232-CR-1 的 4*1024→maxStoredResultRunes 部分缓解，但根因是双轨）。方案：把测试改调 P0 / finishRun，再删 recordResult。Breaking：否（私有方法）。合并 R230B-SEC-1。
- [ ] **R232-ARCH-3 — cron 的 SessionRouter 接口仍声明 RegisterCronStub + RegisterCronStubWithChain（P1）**: commit b1bdff8 已合并 stub 双轨，但 cron 端消费者接口的双方法没跟进，session 包导出双方法。方案：从 cron 接口删 RegisterCronStub；session 包改 unexported 或保留单方法。Breaking：否（cron 内部接口）。
- [ ] **R232-ARCH-4 — internal/session/testutil.go 进生产 binary（P1）**: TestProcess + InjectSession helper 文件无 _test.go 后缀，绕开 spawnSession 不变量。方案：改名 `testutil_test.go` 或拆 `internal/session/sessiontest` 子包加 build tag。Breaking：否（外部测试调整 import）。
- [ ] **R232-ARCH-5 — 28 个 contract test 用 os.ReadFile + 字符串/正则 pin 源代码（P1）**: notify_background_ctx_test / debounce_contract_test / on_turn_done_contract_test 等把 gofmt + 注释 + 标识符当 API。方案：抽到行为级断言；真正必须 source-pin 的统一放 `internal/contract/` 加 README。Breaking：否（重构 test）。
- [ ] **R232-ARCH-6 — 5 个独立 *Router 消费者接口 + 2 个临时 cronStubChecker/cronSessionLister（P2）**: cron / dispatch / server.HubRouter / sysession.SystemSessionRouter / upstream.SessionRouter 重叠严重。方案：合并到 `internal/session/iface` 子包按 Lifecycle/Reader/Lookup 三细分接口。Breaking：否。
- [ ] **R232-ARCH-7 — server.wshub 同时持具体 *cron.Scheduler 与单方法 cronStubChecker 接口（P2）**: 同一依赖两套抽象并存。方案：scheduler 字段类型缩窄为 cronStubChecker；SetScheduler→SetCronStub。Breaking：否（concrete *cron.Scheduler 仍满足）。
- [ ] **R232-ARCH-8 — dispatch 直 import internal/cron 持 *cron.Scheduler（P2）**: dispatch 已有 SessionRouter 消费者接口模式，cron 这一边却走具体类型，policy 不一致。方案：定义 dispatch.CronScheduler interface 子集（AddJob/ListJobs/...）。Breaking：否。
- [ ] **R232-ARCH-9 — cron 包直 import internal/platform 自承 channel adapter 职责（P2）**: cron 的 platforms map + ReplyWithRetry/SplitText/footer 与 dispatch 平行实现。方案：引入 dispatch.Notifier 接口注入 scheduler；cron 不再 import platform。Breaking：否（构造时 wiring 调整）。
- [ ] **R232-ARCH-10 — Scheduler.Stop 写盘绕过 saveSeq gate（P2）**: marshalJobsLocked + WriteFileAtomic 直接写，可能被 in-flight saveMarshaledSeq 用旧 seq 覆盖。方案：Stop 也走 persistJobsLocked + saveMarshaledSeq。Breaking：否。
- [ ] **R232-ARCH-11 — NotifyPolicy 隐式三态（P2）**: cron Job.Notify *bool 三态 + Platforms+NotifyDefault+per-job target 4 条优先级容易翻车（IM 创建默认回源 chat / dashboard 创建默认 silent）。方案：改 enum NotifyPolicy 显式建模。Breaking：是（cron_jobs.json schema 迁移）。
- [ ] **R232-ARCH-12 — executeOpt 315 行单函数（P2）**: 一函数承担 CAS/metrics/jitter/snapshot/notify resolve/spawn/watchdog/finish/deliver/stubRefresh。方案：抽 executeStep interface（preflight/spawn/send/finalize 4 步），主流程退化为 step 串。Breaking：否。
- [ ] **R232-ARCH-13 — sysession Manager Stop 路径调 osExit(2) 与 cron Stop budget+leak 立场不一致（P3）**: 同一个 Stop() 调用链里两个子系统选不同策略。方案：sysession 改用同 cron 的 budget+leak；force-exit 决策上提到 main.go shutdown handler。Breaking：否。
- [ ] **R232-ARCH-14 — SchedulerConfig 22 字段缺乏 Defaults() helper（P3）**: 11 个可选默认型 + 行为回调通过 setter 注，调用方静态看不出哪些必填。方案：拆 SchedulerDeps + functional options 或 Defaults() helper。Breaking：是（构造签名变化，main.go 一处）。

### Go 正确性 / 并发 — 本轮新发现

- [ ] **R232-GO-1 — protocol_acp.go readUntilResponse 超时 goroutine 永久泄漏（P1）**: 非 shim reader 路径（R224-GO-2 已记）超时后 goroutine 无 SetReadDeadline 出口，每次握手超时泄漏一个。方案：JSONRW 加 SetReadDeadline 接口或 type-assert io.Closer fallback close fd。Breaking：否。
- [ ] **R232-GO-2 — historyWg.Add(1) vs historyCtx.Err() TOCTOU（P2）**: R230-GO-1 仍 open。方案：把 Add(1) 提到 ctx.Err() 检查之前，跳过分支立即 Done()。Breaking：否。
- [ ] **R232-GO-3 — sysession.Manager Stop-before-Start 边角（P2）**: stopOnce.Do 仅用 m.cancel != nil 守卫，未建立 started 标志。方案：原子 started 标志早返回。Breaking：否。
- [ ] **R232-GO-4 — limitedWriter.Write error 分支返回 len(p) 违反 io.Writer 契约（P2）**: 目前是有意为之（exec.Cmd pump 不重试），注释已说明。属"违反契约的设计取舍"，跟踪至 godoc 升级或封装。Breaking：否（行为修正风险大，跟踪不直修）。
- [ ] **R232-GO-5 — runOnce defer 顺序与注释不符（P3）**: combined defer 与 tickCtx cancel defer LIFO 实际是 cancel 先跑，注释声称相反。方案：把 tickCtx 声明提到 combined defer 之前。Breaking：否。
- [ ] **R232-GO-6 — runOnce post-Run goroutine 看到 cancel 后的 tickCtx 误分类风险（P3）**: 同 R232-GO-5。

### 性能 — 本轮新发现

- [ ] **R232-PERF-1 — protocol_acp parseSessionUpdate 每 token 双 Unmarshal（P1）**: agent_message_chunk 流式输出 kiro 高频路径双 reflect。方案：合并 ACPTextContent 解析进首次 Unmarshal。Breaking：否。
- [ ] **R232-PERF-2 — agent_tailer.pollOnce 每事件 × 每 client 重复 marshal（P1）**: 同 R230B-PERF-1 模式但 agent_tailer 路径未修。方案：扇出前 marshalPooled 一次，subscribers 用 SendRaw。Breaking：否。
- [ ] **R232-PERF-3 — subagent_transcript.readLocked 每 200ms 重 open+seek+read（P2）**: R230B-PERF-5 已记，补充 freshBytes io.ReadAll 切片不复用。方案：TranscriptReader 加 readBuf 复用 backing。Breaking：否。
- [ ] **R232-PERF-4 — wshub.BroadcastSessionsUpdate AfterFunc 重复分配 timer（P2）**: 与 heartbeatLoop 已用 NewTimer+Stop+Reset 模式不一致。方案：Hub.debounceTimer 改预分配 *time.Timer。Breaking：否。
- [ ] **R232-PERF-5 — protocol_acp.readUntilResponse 每握手分配 goroutine + 2 channel（P2）**: 方案：done 改 atomic.Bool；ch 改 var ch [1]readResult 栈上。Breaking：否。
- [ ] **R232-PERF-6 — subagent_transcript map[string]any decode 每 block 多 alloc（P2）**: 与 R230B-PERF-4 同类。方案：定义 transcriptContentBlock struct。Breaking：否。
- [ ] **R232-PERF-7 — auto_titler.buildExcerpt 合法 seed 双 rune scan（P3）**: 慢路径 utf8.ValidString check + 主 loop 重复扫。方案：合并主 loop 用 w<0 跳过非法 rune。Breaking：否。
- [ ] **R232-PERF-8 — runStore.Append 每次 trim 触发 ReadDir（P3）**: 频繁 job 远未达 keepCount 时纯开销。方案：cache len 计数 + 阈值触发。Breaking：否。
- [ ] **R232-PERF-9 — sanitiseRunResult 二次截断 ellipsis 后缀字节边界（P2）**: TruncateRunesNoEllipsis 后追加 "…[truncated]" 再 SanitizeForLog 二次截断会破坏后缀。方案：SanitizeForLog 上限 += len("…[truncated]")。Breaking：否。
- [ ] **R232-PERF-10 — cacheTrimAfterDisk EndedAt vs trimJobLocked mtime 时间源不一致（P2）**: R221-FIX-P1-3 假设只在快路径成立。方案：cache 同时存 ondisk-mtime 给 trim 用。Breaking：否。

### 安全 — 本轮新发现

- [ ] **R232-SEC-1 — .conf/.cfg/.ini 预览 + download 路径暴露凭据（P1）**: 这三种扩展常存 DSN/secret，TODO P3 R230B-SEC-4 低估风险。方案：从 previewableByExt 移除，sensitiveDownloadName 加名称模式（*db*/*database*/*secret*）。Breaking：否（预览改不可预览）。
- [ ] **R232-SEC-2 — 4 条 serve* 路径独立 TOCTOU（P2，扩展 R231-SEC-5）**: handleFileGet Lstat 后 serveRender/Preview/Raw/Download 各 open 一次。方案：handleFileGet 一次 OpenFile 拿 fd 传子函数；下游 Fstat 比 inode。Breaking：否。
- [ ] **R232-SEC-3 — feishu transport_hook 签名失败前 nonce 未入库（P2）**: HMAC 失败提前返回时 timestamp 窗口内可换 nonce 重放。方案：失败时也写 nonce 或先 nonce 去重再签名校验。Breaking：否。
- [ ] **R232-SEC-4 — sensitiveDownloadNames 漏 secrets.yaml/service-account.json/gcp-key.json（P2）**: 方案：在 sensitiveDownloadExts 加 .json 与 secret/credential/key 文件名模式结合。Breaking：否。
- [ ] **R232-SEC-5 — backend ID charset 双标 cron CRUD vs WS（P2）**: validateCronBackend [a-z0-9_-] vs isValidBackendID [a-zA-Z0-9_.-]。R230B-SEC-2 已记，本轮补充新边角：handleList 读路径不校验大写 ID 透传响应。方案：统一到单一 helper。Breaking：是。
- [ ] **R232-SEC-6 — serveRaw 透传 text/markdown MIME 不强制 attachment（P2）**: 浏览器嗅探可能渲染 HTML。方案：text/* 子类型统一强制 Content-Disposition: attachment。Breaking：否。
- [ ] **R232-SEC-7 — JFIF+PDF 双容器绕过 PDF 检测以 KindImageInline 进入（P2）**: 方案：增二次魔数检测拒绝嵌套 PDF。Breaking：否。
- [ ] **R232-SEC-8 — detectMime 隐藏文件 (.makefile) 点号拼接错误（P3）**: 无扩展名分支用 "."+base 拼接，".makefile" 被映射 octet-stream。方案：ext=="" 分支用原始 base 查表。Breaking：否。
- [ ] **R232-SEC-9 — buildLoginPageCSP 正则提取 inline script/style 错误只在运行时暴露（P3）**: 无编译期自测。方案：加 init() 自测或常量化 hash + TestLoginPage。Breaking：否。
- [ ] **R232-SEC-10 — interruptLimiter 频率高于 sendLimiter（P3）**: 15/s vs 5/s 可对单 session DoS。方案：interruptLimiter 改 rate.Every(2s) burst=2。Breaking：否。
- [ ] **R232-SEC-11 — weixin Reply MessageID 拼接含未转义 ChatID（P3）**: 方案：用 SanitizeForLog 或 url.PathEscape 处理 ChatID。Breaking：否。
- [ ] **R232-SEC-12 — protocol_claude resumeIDRe 含 . 略扩 --resume 字符集（P3）**: 方案：缩到 [A-Za-z0-9-]。Breaking：否（合法 Resume ID 不含 . 或 _）。
- [ ] **R232-SEC-13 — HTTPS+反向代理未设 trusted_proxy 时 cookie 无 Secure 标志（P3）**: 文档/doctor 缺提示。方案：doctor 加 HIGH 警告。Breaking：否。
- [ ] **R232-SEC-14 — agent_tailer.ensureTailer 未校验 jsonlPath 在 allowedRoot 下（P3）**: 方案：ensureTailer 加 allowedRoot 参数 + HasPrefix。Breaking：否。
- [ ] **R232-SEC-15 — isLoopbackRemote 未处理 UDS 空地址（P3）**: 部署在 Unix domain socket 后 pprof/expvar 拒绝访问。方案：特判空 RemoteAddr。Breaking：否。

### 代码质量 / 重构 — 本轮新发现

- [ ] **R232-CR-1 — internal/cron/store.go saveJobs 死代码（P2）**: 仅 scheduler_test.go 调，生产用 persistJobsLocked + saveMarshaledSeq。方案：测试改 json.Marshal+os.WriteFile 后删 saveJobs。Breaking：否。
- [ ] **R232-CR-2 — RunStateRunning 死枚举（P2）**: 注释说"仅 inflight 不落盘"但代码无引用。方案：删常量；inflight 状态由 runInflight.Phase 表达。Breaking：否。
- [ ] **R232-CR-3 — emitOverlapSkipped 发 back-to-back started→ended 假事件（P2）**: dashboard 短暂闪运行中气泡；SessionID/Phase 空字符串。方案：跳过 emitRunStarted 或 RunStartedEvent 加 Skipped bool。Breaking：是（WS schema）。
- [ ] **R232-CR-4 — agents map 未配 "general" 静默零值（P2）**: 缺乏防御性 log。方案：NewScheduler debug log 或注释解释。Breaking：否。
- [ ] **R232-CR-5 — computeJobTimeout schedule 参数无效（P2）**: 函数体 `_ = schedule; return maxCap`，注释"signature stability"。方案：删 schedule 参数。Breaking：是（机械重构）。
- [ ] **R232-CR-6 — auto_titler.renameOne 双层长度校验缺注释（P2）**: ValidateUserLabel 字节上限 + autoTitlerMaxTitleRunes rune 检查。方案：注释解释 16 rune 是 auto-titler 严格上限。Breaking：否。
- [ ] **R232-CR-7 — preflightResult 单字段 wrapper struct（P2）**: 方案：直接返回 (func(), bool) 二元组。Breaking：否（内部类型）。
- [ ] **R232-CR-8 — TriggerCatchup / ErrClassPanic / DaemonTriggerManual 占位常量（P3）**: 散布在导出 API 中无生产路径产生。方案：注释加警告或改 unexported。Breaking：否。
- [ ] **R232-CR-9 — JobTitleOrFallback 死导出符号（P3）**: 仅 title_test.go 用，前端实现 fallback。方案：降 unexported 或删除。Breaking：否（前端不依赖）。
- [ ] **R232-CR-10 — sysession.SweepOldJSONL 单次启动扫描而非 daemon（P3）**: 注释承诺 Phase 2 TransientSweeper 未兑现。方案：sweep.go godoc 注释明确单次语义；TODO 跟踪 Phase 2。Breaking：否。
- [ ] **R232-CR-11 — saveMarshaledSeq 注释说"Atomic CAS"实际 Load+Store（P3）**: 方案：注释改为"在 storeMu 持锁状态下 Load+Store，非 CAS"。Breaking：否（注释）。
- [ ] **R232-CR-12 — registerStub / registerStubByValue / stubChain 三 helper 结构（P3）**: pointer vs value 仅参数差异。方案：合并到一个传四个 string 的 helper。Breaking：否（私有）。
- [ ] **R232-CR-13 — dispatch unit test 走真 session.Router（P3）**: dispatch_test.go newTestDispatcher 实际是集成测试。方案：用 dispatch 自己的 SessionRouter fake。Breaking：否。
- [ ] **R232-CR-14 — agent_tailer.attach 锁外逐条 SendJSON（P2）**: 500 条 buffered replay 无批量 marshal。方案：批量 node.ServerMsg{Type: "agent_history", Events: buffered}。Breaking：否（WS schema 兼容性需查）。
- [ ] **R232-CR-15 — protocol_acp.Init session/new 分支手写 Marshal+WriteLine（P2）**: 与 initialize/load 走 sendAndWaitResponse 不一致。方案：抽 sendReq helper 或统一走 helper。Breaking：否。
- [ ] **R232-CR-16 — cli/eventlog.go fireOneTaskDoneCallback / fireTaskDoneCallbacks 重复逻辑（P2）**: 方案：fireOne 内联为 fireTaskDoneCallbacks 单元素 fast-path。Breaking：否。
- [ ] **R232-CR-17 — formatAssistantToolUseDetail map→JSON→parse round-trip（P3）**: 注释说 cold path 但 pollOnce 调用频繁。方案：保留原始 json.RawMessage 直传 FormatToolInput。Breaking：否。

## Round 231 — 5-agent 并行 review 第 41 轮（2026-05-21）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描发现 ~80 条。本轮直接修 12 处（顶部摘要）。
> 以下是需设计决策、破坏兼容、跨包重构、或方案不唯一不适合本轮直接修的条目。

### 架构（高优先）— 本轮新发现

- [ ] **R231-ARCH-1 — sysession.Runner 直 exec 旁路 CLI Wrapper 三层抽象（P1）**: `internal/sysession/runner.go` 自拼 `-p` argv、自 filterEnv、自 setting-sources，与 `cli.Wrapper.Spawn` 完全平行。新增 backend（Gemini ACP）必须在此再实现一遍。区别于 R230-ARCH-1（仅指出绕 backend.Profile），本条强调它把 CLI Wrapper 整层短路。方案：把 `RunOneShot` 抽进 `backend.Profile`，或让 Runner 走 `cli.Wrapper.Spawn(--collect-mode)`。Breaking：是。
- [ ] **R231-ARCH-2 — 消息出口四套并行管线（P1，扩展 R230-ARCH-2/6）**: dispatch.sendAndReply / server.Hub.sendWithBroadcast / cron.scheduler.executeOpt / upstream.connector_rpc 四套 send 路径。cron 直持 platforms map、绕开 dispatch.replyText/queue/dedup；upstream 直调 sess.Send 绕开 MessageQueue/usermsg。Channel Adapter 不再是消息出口的唯一抽象。方案：抽 `internal/turnrunner` 或扩 `dispatch.Dispatcher` 为唯一 Send 协调器。
- [ ] **R231-ARCH-3 — session/router_core.go 顶部 blank-import history backend（P1）**: `claudejsonl/kirojsonl/naozhilog` 三个包的 init() 注入。注释自承"Sprint 1b 将合并到 wireup 包"。session 包想成为 backend-agnostic 的话必须迁出。方案：抽 `internal/wireup` 显式 `RegisterDefaults()` 由 `cmd/naozhi` 调用。Breaking：no（机械迁移）。
- [ ] **R231-ARCH-4 — Router god-object（60+ 方法 / 24+ 字段）（P1）**: 单结构体覆盖 7 大职责，5 处消费方手工裁剪 Reader/Writer 接口已出现 NotifyIdle/SetUserLabelWithOrigin 不对称（R230-ARCH-3）。方案：facet 化 `Router.Lifecycle()` / `Backends()` / `Stubs()` / `Overrides()`，每 facet 对应稳定接口。
- [ ] **R231-ARCH-5 — Hub 与 Router god-object 双胞胎共同导致 Channel Adapter 抽象塌陷（P1）**: server.Hub 退化为第二个 Router（同时持 router/scheduler/scratchPool/queue/dedup/uploadStore/auth/tailers/nodes）；webhook 进来后路径从"Adapter→Router→Wrapper"变成"Adapter→{Hub/Server/Dispatcher} 三方共享状态"。方案：抽 WSEventBus / nodeRegistry / SendCoordinator 子聚合。
- [ ] **R231-ARCH-6 — server.Hub.wiredLinkers 直持 cli.SubagentLinker（P1）**: Channel Adapter / 上层 server 强耦合 cli 包内部领域类型；ACP 等"无 SubagentLinker 概念"的 backend 上线时整条 agent-team UI 链路要么硬编码空实现要么走 nil 分支。方案：在 session 或 internal/agentlink 包定义 AgentLinker / AgentIntrospector 接口。Breaking：是。
- [ ] **R231-ARCH-7 — cli.Wrapper.ShimManager 公开可变字段（P1）**: ShimManager 本应是进程级 singleton；multi-backend 部署 router 持 `wrappers map[string]*cli.Wrapper`，每 Wrapper 一个 ShimManager 副本（R230-ARCH-13 / R219-ARCH-4）。方案：定义 cli.Transport interface，shim/direct-exec 各一实现；Wrapper 拆 immutable BackendProfile + 共享 Transport。Breaking：是。
- [ ] **R231-ARCH-8 — cli.Protocol 接口过宽（P2）**: 9 方法含 stream-json 专属能力 WriteUserMessageLocked / WriteInterrupt / SupportsPriority / SupportsReplay。ACP 这些方法必然 noop 或返回 ErrInterruptUnsupported。方案：缩为核心 7 方法 + passthrough/interrupt 下沉为可选 PassthroughExt / InterruptExt（type-assert）。Breaking：是。
- [ ] **R231-ARCH-9 — workspaceOverrides 与 sessions.json 双 JSON 分离（P2）**: 独立 dirty bit / gen counter / atomic write，部分失败导致重启后 session 引用了不在 overrides.json 的 chat workspace，无 reconciliation 路径（R219-ARCH-9）。方案：合成单文件 atomic write 或启动期一致性扫描修复。Breaking：是（store schema migration）。
- [ ] **R231-ARCH-10 — backend.Profile 注册表承诺与 wrapper.go 硬编码 switch 落差（P2）**: `cli/wrapper.go` `backendDisplayName` / `detectCLI` 仍硬编码 switch on "kiro" / "claude"，DESIGN.md L280 承诺已部分兑现但未到位。方案：通过 `backend.LookupProfile(id).DisplayName/.DefaultBinary` 获取，删除硬编码 switch；测试加 contract 锁。
- [ ] **R231-ARCH-11 — NewRouter 构造期副作用阻碍可测性（P2）**: NewRouter ~360 行内 load knownIDs / load workspaceOverrides / load sessions.json / 启动 N goroutine 异步加载 history / runOrphanSweep / startAttachmentTracker。测试无法单独构造 router 而不触发磁盘 IO + goroutine（R230-CQ-10）。方案：拆 `NewRouter`（仅 init 字段）+ `Router.Start(ctx)`。Breaking：是（构造方需迁移）。

### 安全 — 本轮新发现

- [ ] **R231-SEC-1 — sysession.Runner 直 exec 不走 shimEnvAllowedPrefixes 精细白名单（P1，独立于 R231-ARCH-1）**: `runner.go:88` `exec.CommandContext` 调用 CLI 二进制使用 `filterEnv(EnvAllowlist)` 但语义与 `shim/manager.go:891 shimEnvAllowedPrefixes` 不一致；也不经 `capExtraArgsBytes` ARG_MAX 守卫。Daemon prompt 含用户对话摘录，错误输出可把对话片段写入 stderr → slog.Debug。方案：抽 `internal/cliexec.EnvFilter` 单实现两处共享，或让 Runner 走 cli.Wrapper 路径自动继承 shim 过滤。
- [ ] **R231-SEC-2 — Dashboard 主页面 CSP `script-src 'unsafe-inline'`（P1，R229-SEC-6/R230-SEC-1 重申未修）**: 主页面允许任意内联 script，若 workspace 文件被污染且触发 XSS sink 即可执行任意 JS 窃取 session cookie。方案：迁 nonce/hash 模式，将 dashboard.html 内联 onclick 等事件外移为外部 JS。Breaking：是（前端模板需要重构）。
- [ ] **R231-SEC-3 — `allowed_root` 缺失时不阻断公网监听启动（P1，R229-SEC-3 重申未修）**: dashboard_token 非空且监听非 loopback 但 allowed_root 为空时仅 Warn，认证用户可设 cron work_dir=/etc 让 CLI 向系统目录写文件。方案：fatal 启动失败 + naozhi doctor 加 HIGH 级别检查。Breaking：是（已部署但未配置 allowed_root 的部署需要迁移）。
- [ ] **R231-SEC-4 — ExtraArgs 无 flag 允许列表（P1，R219-SEC-1/R229-SEC-1 重申未修）**: `protocol_claude.go:77` `args = append(args, opts.ExtraArgs...)`，dashboard-authenticated 用户可注入 `--mcp-config` / `--add-dir` 等改变 CLI 行为。方案：BuildArgs 加 flag 允许列表。Breaking：是。
- [ ] **R231-SEC-5 — `serveRender/servePreview/serveRaw/serveDownload` Lstat 后再次 os.Open 的 inode-swap TOCTOU（P1，R219-SEC-2/R229-SEC-2 重申未修）**: 每个 mode 独立的 os.Open 都是新窗口。方案：handleFileGet 使用 OpenFile 拿到 fd，下游直接消费 fd；或加 Fstat 验证 inode。
- [ ] **R231-SEC-6 — sessions.meta.json 非原子写（P2，R230-SEC-4 重申未修）**: `internal/session/store.go` 用单次 os.WriteFile 而非 osutil.WriteFileAtomic，部分写失败时半截 JSON 导致重启后 session 历史不可用。方案：改用 osutil.WriteFileAtomic。
- [ ] **R231-SEC-7 — XDG_ 前缀过宽放行（P2，R230-SEC-2 重申未修）**: shim/manager.go 放行 `XDG_*` 整族，理论上可重定向 CLI 配置/数据查找路径。方案：精确白名单 XDG_RUNTIME_DIR= / XDG_CACHE_HOME= / XDG_STATE_HOME=。Breaking：是（contract 测试需更新）。
- [ ] **R231-SEC-8 — feishu webhook 仅靠 plaintext VerificationToken（P2）**: nonce 已强制；但 v1 仅 VerificationToken 模式下 token 是 plaintext shared secret，泄漏后 5 分钟 replay 窗口内自由重放。方案：强制要求配 EncryptKey 或将 token-only 标 deprecated 并 startup Warn 提升级别。
- [ ] **R231-SEC-9 — 单 token 可建 500 个 WS（P2，R229-SEC-8 重申未修）**: maxConnectionsPerServer=500 但无 per-token/per-cookie-bucket 子上限。方案：WS 升级时按 cookie MAC 或 Bearer SHA-256 设 per-token 上限（如 20）。
- [ ] **R231-SEC-10 — 反向 Node 连接通过 `ws://` 明文（P2，R229-SEC-5 重申未修）**: 部署环境未有 TLS 卸载代理则 token 中间人截获。方案：`/ws-node` handler 检查 r.TLS + 可信 X-Forwarded-Proto:https，无则拒绝 Upgrade，或显式豁免 + 文档。Breaking：是。
- [ ] **R231-SEC-11 — `/static/dashboard.js` 不走 requireAuth（P2，R230-SEC-3 重申未修）**: 中间人可替换 JS 文件后客户端窃取 dashboard token。方案：dashboard_token 非空时对静态 JS 端点加 requireAuth。
- [ ] **R231-SEC-12 — handleFileGet Lstat 后未再做 rootResolved 二次确认（P2，R230-SEC-5 重申未修）**: TOCTOU 内文件可被移到 workspace 外。方案：Lstat 后再做 HasPrefix 双重确认。

### 性能 — 本轮新发现

- [ ] **R231-PERF-1 — Protocol.ReadEvent string→[]byte 反向拷贝（P1，R67-PERF-1/R226-PERF-1 重申未落地）**: 每 stdout 事件强制 heap alloc。方案：Protocol 接口签名改 `ReadEvent([]byte)`，readLoop 直接传 trimmed []byte。Breaking：是。
- [ ] **R231-PERF-2 — ACP parseSessionUpdate 双 Unmarshal（P1）**: agent_message_chunk 分支每帧两次 json.Unmarshal，kiro 每 token 一帧最热路径。方案：bytes.Contains 快速判断或合并为一次解析。
- [ ] **R231-PERF-3 — subagent_transcript Tail 每 200ms 申请新 readBuf（P1）**: io.ReadAll 在 hot poll 路径无缓冲复用，50 tailers × 5/s = 250/s alloc。方案：TranscriptReader 加 readBuf 字段 append 复用并合理上限保留 cap。
- [ ] **R231-PERF-4 — ACPProtocol.textBuf 锁竞争（P2）**: kiro 流式输出每 token 都竞争同一把锁；ReadEvent 末尾再获取一次。方案：lock-free 积累方案 atomic.Pointer[[]string] 或 arena buf 仅 turn boundary 合并。
- [ ] **R231-PERF-5 — agent_tailer pollOnce 每事件逐 SendJSON Marshal（P2）**: 50 tailer × 5 sub × N events/200ms = 重复编码。方案：每 event 用 marshalPooled 预序列化一次再 SendRaw 分发。
- [ ] **R231-PERF-6 — BroadcastSessionsUpdate AfterFunc 创建新 timer（P2）**: 事件密集期批量分配 runtime.Timer。方案：改为 heartbeatLoop 的预分配 NewTimer + Reset 模式。
- [ ] **R231-PERF-7 — ACP readUntilResponse 每握手 3 次 goroutine + 3 chan alloc（P2）**: 握手 3 次 = 9 次。方案：握手 goroutine 提升为长寿命，仅在握手阶段循环；或 done chan→atomic.Bool。
- [ ] **R231-PERF-8 — Cleanup 在 r.mu 内做整 sessions map copy（P2）**: O(N) 拉长持锁时间。方案需保持 saveStore 的稳定 snapshot 语义（不能拆 keys → 释放锁 → 再 RLock 因为竞态），或者转为 RCU/COW snapshot。需独立设计。
- [ ] **R231-PERF-9 — eventlog Append 单元素 sink 路径仍有 EventEntry 大 struct copy（P2，与本轮 sink-nil 早返回互补）**: 480+ B struct + slice header 都逃逸；本轮已加 sink-nil 早返回但 sink-attached 路径仍有 alloc。方案：invokePersistSinkOne 单条专路或 EventEntry 字段瘦身。Breaking：是（ring buffer 重新 benchmark）。
- [ ] **R231-PERF-10 — config Load expandEnvVars(string(data)) 双 string 拷贝（P3）**: expandEnvVars 接受 []byte 可省一次。Breaking：no（仅签名调整）。

### 代码质量 — 本轮新发现

- [ ] **R231-CQ-1 — claude reconnect 路径双注入（P1，PR #202 复盘）**: `router_shim.go:439` 直接 `proc.InjectHistory(histEntries)`，随后 `ReattachProcessNoCallback` 调 `attachProcessAndSnapshotPersisted` 把 `sess.persistedHistory`（已由 tier1/tier2 异步 goroutine 通过 `sess.InjectHistory` 填充）snapshot 再次注入同一 proc。两批高度重叠时 EventLog 翻倍。方案：line 439 改为 `sess.InjectHistory(histEntries)` 走 persistedHistory + seededLen 流向，与 kiro 路径行为一致。需对照 #202 PR 测试与 EventLog 去重逻辑验证。
- [ ] **R231-CQ-2 — attachProcessAndSnapshotPersisted(nil) 语义不明（P1）**: nil 分支重置 seededLen=0 但保留 persistedHistory；下次非 nil ReattachProcess 时会全量重注入。方案：明确 nil 参数语义（detach vs clear），或 nil 分支不修改 seededLen。
- [x] **R231-CQ-3 — managed.go ReattachProcessNoCallback 中冗余 `proc != nil` 检查（P3）**: attachProcessAndSnapshotPersisted 在 proc==nil 时已 return nil，外层冗余 guard。方案：移除或加注释。 — 已修复（ReattachProcess + ReattachProcessNoCallback 两处的 `proc != nil && len(snapshot) > 0` 化简为 `len(snapshot) > 0`，并补注释说明 attachProcessAndSnapshotPersisted nil-snapshot 契约），本批 PR #210
- [x] **R231-CQ-4 — reattach_history_test.go itoa 重复（P3）**: 同 package strconv 已 import；测试自定义 itoa 违 DRY。方案：用 strconv.Itoa 或 testutil 提供 helper。 — 已修复（reattach_history_test.go 改用 strconv.Itoa，删 23 行私实现 itoa），本批 PR #210
- [ ] **R231-CQ-5 — attachProcessAndSnapshotPersisted vs adoptProcessAlreadySeeded 命名风格不对称（P3）**: 一动作+副词，一形容词描述结果。方案：统一命名风格。
- [x] **R231-CQ-6 — persistedSeededLen 字段 21 行 block 注释与函数 godoc 重复（P3）**: 方案：精简到 3-5 行 + "see attachProcessAndSnapshotPersisted" 交叉引用。 — 已修复，本批 PR #211
- [x] **R231-CQ-7 — managed.go forward 注释 "R191-GO-M1 不再相关" 与实际行为部分矛盾（P3）**: 注释说"是旧 proc 也没关系"但没解释为何旧 proc 注入无害。方案：补充"orphan proc 无 EventEntries 调用方"说明。 — 已修复，本批 PR #213
- [ ] **R231-CQ-8 — reattach_history_test 不覆盖 cap-trim 分支（P2）**: writers*perWriter=200 < maxPersistedHistory=500 触发不到 trim 路径。方案：补 trim 场景测试 + 验证无重复。
- [ ] **R231-CQ-9 — InjectHistory cap-trim seededLen=0 clamp 破坏"不重注"保证（P2）**: 当 trimmed > seededLen 时 seededLen clamp 到 0，下次 InjectHistory 全量 forward 含 proc 已见过的历史段。方案：明确 degrade-to-reseed 语义而非 no-op，调用处加 note。

### Go 正确性 — 本轮新发现

- [ ] **R231-GO-1 — managed.go old.storeProcess(nil) 在 r.mu 内未持 historyMu（P2）**: storeProcess(nil) 与并发 InjectHistory 读 loadProcess() 组成 (proc=old) vs (proc=nil) 逻辑分裂窗口。方案：改 `old.adoptProcessAlreadySeeded(nil)` 或 nil 特化路径明确文档化。
- [ ] **R231-GO-2 — RenameSession 中 fresh 在 adopt 前发布的危险窗口（P2）**: persistedSeededLen=0 期间若并发 InjectHistory 看到 fresh 会 reseed。当前 r.mu 持有保护，但应文档说明 fresh 仅在 adoptProcessAlreadySeeded 后才可安全发布。
- [ ] **R231-GO-3 — sysession.Runner sweep `os.IsNotExist` → `errors.Is(fs.ErrNotExist)` 同类（P3）**: 已修。

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
- [ ] **R219-GO-2 — `reconnectShims` replay 段 `linker.Resolve` goroutine 无 ctx 绑定（P2 重申 R218B-GO-3 未覆盖分支）**: R218B-GO-3 仅覆盖 `process_readloop.go:324`，`router.go:1469` reconnectShims 路径下的 `go linker.Resolve(...)` 同样裸 goroutine。startup 期 SIGTERM 到来时该批 Resolve 不会被取消，最多 3s 后退出延迟 shutdown。方案：和 R218B-GO-3 同步修复。涉及：`internal/session/router.go:1469`。
- [x] **R219-GO-3 — `cli/process.go:596` shimConn double-close 无 sync.Once 保护（P3）**: `Close()` 与 `Detach()` 各自 `shimWMu.Lock() + shimConn.Close()`，并发场景下 net.Conn double-close 返回 "use of closed network connection" 错误被 `_` 丢弃。无 panic 但留下误导性调试信息。方案：加 `closeOnce sync.Once`。涉及：`internal/cli/process.go:596`。 — 已修复，本批 PR #75

### 安全 — 本轮新发现

- [ ] **R219-SEC-1 — `BuildArgs` `opts.ExtraArgs` 无 flag 允许列表（P1）**: `protocol_claude.go:77` `args = append(args, opts.ExtraArgs...)`，dashboard-authenticated 用户可注入 `--mcp-config`、`--add-dir`、`--skip-permissions` 等改变 CLI 行为的 flag。区别于 R217-SEC-1（`--append-system-prompt`），本条强调 `--mcp-config` 类可加载攻击者控制 MCP 服务器定义的 flag。方案：在 BuildArgs 加 flag 允许列表，拒绝列表外以 `--` 开头的 element。Breaking：是（依赖任意 extra args 的运维方需要迁移）。
- [ ] **R219-SEC-2 — `serveRender` 在 Lstat 后第三次 os.Open 制造 inode-swap TOCTOU（P1）**: `handleFileGet` 已有 R218B-SEC-2 Lstat-after-resolve 防御，但 `mode == "render"` 走 `serveRender` 时再次 `os.Open(resolved)`，inode swap 攻击仍可绕过。方案：`serveRender` 使用 Lstat 时已 Open 的 fd 或加 `Sys().(*syscall.Stat_t).Ino` 验证。涉及：`internal/server/project_files.go:667`。
- [x] **R219-SEC-3 — `shimEnvAllowedPrefixes` 把 `ANTHROPIC_API_KEY` 泄漏到 Bedrock 部署的 shim 子进程（P2）**: `manager.go:900` 含通配前缀 `"ANTHROPIC_"`，Bedrock 部署不需要 `ANTHROPIC_API_KEY` 但仍然进 shim env，Bash tool 调用可 `env | grep ANTHROPIC` 拿到。方案：替换为显式条目 `"ANTHROPIC_AUTH_TOKEN="`、`"ANTHROPIC_MODEL="`，或 Bedrock 模式下排除 `ANTHROPIC_API_KEY`。涉及：`internal/shim/manager.go:900`。 — 已修复：PR #91 先拆 5 项显式 allowlist（API_KEY / AUTH_TOKEN / MODEL / BASE_URL / BEDROCK_BASE_URL）+ 2 条反例；PR #93 进一步把 CLAUDE_ 通配前缀也收紧为 4 项显式（CLAUDE_CODE_USE_BEDROCK / SKIP_BEDROCK_AUTH / CLAUDE_BIN / CLAUDE_MODEL），表驱动测试加 4 反例锁拒绝 ANTHROPIC_LOG/TELEMETRY_TOKEN/CLAUDE_CONFIG_DIR/TELEMETRY，同时关闭 R214-SEC-3。
- [x] **R219-SEC-4 — KaTeX CDN 加载无 SRI integrity hash（P2）**: `dashboard.js loadKatex()` 动态注入 KaTeX `<link>`/`<script>` 但无 `integrity=` SRI；CDN 被攻陷可注入恶意 `renderToString`。主 CDN 已有 SRI（CSP 注释），KaTeX 应一致。方案：在 loadKatex 动态创建的元素加 SRI 哈希。涉及：`internal/server/static/dashboard.js:2161,8651,6403`。 — 已修复（KaTeX/Mermaid 已加 sha384 SRI + crossOrigin='anonymous'，本批新增 TestDashboardJS_CDNScriptsHaveSRI 锁定契约），本批 PR #86
- [ ] **R219-SEC-5 — `moveToShimsCgroup` 用 `Hello.CLIPID` 经 sudo 把任意 PID 移入 cgroup（P2）**: `manager_linux.go:72` 把 shim 自报的 CLIPID 经 strconv.Itoa 传给 `sudo busctl`，shim 被劫持可上报任意 PID。方案：通过 `/proc/<CLIPID>/status` 验证 PPid 等于 shim 实际 PID。涉及：`internal/shim/manager_linux.go:72`。

### 性能 — 本轮新发现 / 重申

- [ ] **R219-PERF-1 — `eventPushLoop` 同 session N tab 各自 marshalPooled 独立序列化（P2 重申 R214-PERF-4）**: 同 key 多客户端 fan-out 时未做单次序列化共享。方案：在 eventPushLoop 把同 key 全部 clients 集中在 broadcast goroutine 里序列化一次再 fan-out SendRaw。
- [ ] **R219-PERF-2 — `handleList` storeGen 未变化时未短路重建 sessionWorkspaces map（P2）**: 1 Hz × N tab × N session 重建 map 浪费。方案：引入 `lastListVersion uint64 + lastListJSON []byte` 缓存，命中时直接 ResponseWriter.Write。涉及：`internal/server/dashboard_session.go:313`。
- [ ] **R219-PERF-3 — `Snapshot()` 顺序读 8 次 atomic.Pointer.Load（P2 重申 R215-ARCH-P2-7）**: 1 Hz × 10 tab × 50 session = 4000 Load/s。方案：把构造后不变字段（backend/cliName/cliVersion/userLabel）打包 `immutableBox struct` + 单次 atomic.Pointer.Load。涉及：`internal/session/managed.go:861`。
- [ ] **R219-PERF-4 — `invokePersistSinkSingle` 栈数组方案需 benchmark 证伪（P2）**: Append 单条路径 `[]EventEntry{e}` heap escape，本轮尝试用 `[1]EventEntry` 栈数组 + `s[:]` 但因 PersistSink 是 atomic.Pointer 函数指针调用，逃逸分析会强制 slice 逃逸到 heap，理论收益不确定。需 -benchmem 验证；若无效则降级为接受现状或换 sync.Pool 方案。涉及：`internal/cli/eventlog.go:640`。

### 代码质量 — 本轮新发现

- [x] **R219-CR-1 — `loadAtomicString`/`storeAtomicString` (`cli/eventlog.go`) 与 `loadStringAtomic`/`storeStringAtomic` (`session/managed.go`) 双胞胎（P2）**: 两个函数语义相同但跨包独立，命名词序还相反，新加调用易再造第三个变体。方案：抽到 `internal/syncutil` 或 `internal/textutil`，统一命名。Breaking：否。 — 已修复（textutil.LoadAtomicString/StoreAtomicString，cli/session 各自留 1 行 thin wrapper），本批 PR #76
- [x] **R219-CR-2 — `cron.runeByteOffset` 重复实现 `textutil.TruncateRunes`（P2）**: `scheduler.go:1689` 自实现 14 行 rune-counting 循环，cron 已 import session→textutil。方案：删除 `runeByteOffset`，调用 `textutil.TruncateRunes`。Breaking：否。 — 已修复（改用 textutil.TruncateRunesNoEllipsis 保留 "…[truncated]" 后缀，O(1) 长度比较检测截断），本批 PR #77
- [x] **R219-CR-3 — `feishu/askquestion.go::truncateRunes` 重复实现 `textutil.TruncateRunes`（P2）**: 8 处调用点。方案：替换为 `textutil.TruncateRunes` 或新增 `TruncateRunesSilent` 不带 ellipsis 变体。需审计 8 处对 ellipsis 的依赖。 — 已修复（新增 textutil.TruncateRunesNoEllipsis + 9 处生产调用切换 + 删 feishu 私有版 19 行 + 8 case 新表驱动测试），本批 PR #76
- [x] **R219-CR-4 — `cron.scheduler` `*ByID`/`*` 三对方法 body 重复（P2）**: DeleteJob/PauseJob/ResumeJob 与 *ByID 各 6 个共享 lock+mutate+persist+unlock body，仅 lookup 步骤不同。方案：抽 `deleteJobLocked(j *Job) (saveFunc, error)` 共享。 — 已修复（抽 deleteJobLocked / pauseJobLocked / resumeJobLocked 三 helper + 6 处 caller 收敛 lookup→mutate→persist→unlock 骨架，120 → 59+61），本批 PR #77
- [x] **R219-CR-5 — `dashboard_cron.go` 5 个 validateCron* 重复 UTF-8 + C0 + IsLogInjectionRune 三重扫描（P2）**: 任何安全策略变更需改 5 处。方案：抽 `validateStringField(s, maxBytes, allowedCtrl)` helper，每个 validator 3 行 wrapper。 — 已修复（抽 validateStringField + stringFieldPolicy 4 旋钮 helper，4 个 validate*（workdir / notify_chat_id / schedule / prompt）切换；validateCronTitle 因 \n/\r 单行错误消息保留独立），本批 PR #77
- [x] **R219-CR-6 — `cron.PreviewSchedule` 包级函数 vs `(*Scheduler).PreviewSchedule` 双轨（P2）**: 包级版用 UTC，方法版用 scheduler tz；divergence 易踩。方案：折叠 nil scheduler 守卫到方法或包级版改 unexported。 — 已修复（PreviewSchedule/PreviewScheduleN/Location 全部 nil-receiver-safe + 删除包级版 + dashboard handlePreview if/else 收敛），本批 PR #76
- [ ] **R219-CR-7 — `dispatch.sendAndReply` 241 行 5+ 职责（P2）**: 类同 R214-CODE-3 (readLoop)。方案：抽 `buildReplyContext` + `handleSendResult` helpers。
- [ ] **R219-CR-8 — `shim/server.go::handleClient` 319 行无子拆（P2）**: 4 个内联 goroutine 通过裸 channel 通信。方案：抽 `handleClientHandshake / relayStdin / relayStdout`。
- [ ] **R219-CR-9 — `processIface.GetState/GetSessionID` 违反 Go 命名约定（P2）**: 应去 `Get` 前缀。Breaking：是（接口变更，~12 处 callsite + mock 需改）。
- [x] **R219-CR-10 — `transport_hook.go:136` 注释 "base-16-ish" 与代码 0x21-0x7E 全 ASCII filter 不符（P3）**: 文档与代码漂移。方案：要么收紧 filter 到 hex，要么更正注释。 — 已修复（更正注释为 alphanumeric + 解释 0x21-0x7E 全开是有意 headroom），本批 PR #75
- [x] **R219-CR-11 — `server.sessionSendLegacy` Deprecated 但生产 nil-queue fallback 仍可达（P3）**: 注释说仅用于"未 wire MessageQueue 的测试代码路径"，但 Hub.queue == nil 时生产也命中。方案：Hub.Start() 加 nil-queue 启动 warn 或 NewWithOptions 强制 non-nil queue。 — 已修复（NewHub 在 opts.Queue==nil 时 slog.Warn，让 operator 在 journalctl 第一启动就发现配置漏洞 + 同步 sessionSendLegacy 注释指向 NewHub 警告），本批 PR #77

### 架构 — 本轮新发现 / 重申

- [ ] **R219-ARCH-1 — `cli.EventEntry` / `cli.Event` / `cli.AskQuestion` 已塌陷为跨层 DTO（P1 R217-ARCH-1 未覆盖分支）**: history.Source.LoadBefore 把"事件领域类型"硬编码 cli 包导出类型，未来迁出 cli 内部领域类型时该接口也需重写。方案：整批迁到 `internal/event`（叶子包零依赖）。Breaking：是（接口签名 + 26+ 包 import 路径变动）。
- [ ] **R219-ARCH-2 — `session/router.go:22-25` 顶部硬编码 4 个 backend-specific history 包 import（P1）**: 任意新 backend 加 history source 必须改 session 包，session 永远无法成为协议无关调度层。方案：cli.Wrapper 增 `NewHistorySource(s ManagedSession) history.Source` 工厂方法，session 只 import history.Source 接口。Breaking：是。
- [ ] **R219-ARCH-3 — `server.Hub.wiredLinkers map[*cli.SubagentLinker]struct{}` 持 cli 内部对象指针（P1 R217-ARCH-2 未覆盖分支）**: Linker 内部字段调整会让 server 的 once-only wiring 假设失效；ACP 等无 SubagentLinker 概念的 backend 上线时整条 agent-team UI 链路要么硬编码空实现要么走 nil 分支。方案：定义 `session.AgentIntrospector` interface。
- [ ] **R219-ARCH-4 — `cli.Wrapper` 公开可变字段持 `*shim.Manager`（P1 R214-ARCH-9 未覆盖分支）**: ShimManager 应是进程级单例，但 multi-backend 部署 router 持 `wrappers map[string]*cli.Wrapper` 时每 Wrapper 一个 ShimManager 副本——而 ShimManager 本应管 socket/cgroup 路径单例。方案：Wrapper 拆 immutable BackendProfile + 单例 ShimManager。
- [ ] **R219-ARCH-5 — `Hub.scheduler/uploadStore/scratchPool` 三 setter 在启动后注入造成"半构造对象"（P1）**: 8 处 `if h.scheduler != nil` / `if h.scratchPool != nil` 守卫维系隐式协议。方案：HubOptions 加这三字段，构造期 dashboard.go 重排 wiring 顺序（uploads 先于 NewHub），或以 null-object 接受 nil。
- [ ] **R219-ARCH-6 — `Protocol.WriteUserMessageLocked / WriteInterrupt / SupportsPriority/Replay` stream-json 专属能力漏到接口层（P2）**: ACP 实现必然 noop 或 panic。方案：Protocol 缩成 7 方法核心，passthrough 下沉到 `PassthroughExt` 可选 interface + type-assert。Breaking：是。
- [ ] **R219-ARCH-7 — `processIface` 30+ 方法 god 接口具体拆分建议（P2 R215-ARCH-P1-3 已登记，本轮提供具体方案）**: 拆 `ProcessLifecycle` (6方法) + `EventSource` (7方法)，stream-json 5 方法走 `loadCliProcess()` type-assert（已有先例 managed.go:973）。
- [ ] **R219-ARCH-8 — `cron.scheduler` 直持 `platforms map[string]platform.Platform` + 直调 Reply/MaxReplyLength/SplitText（P2）**: cron 越层访问 IM 出站绕过 dispatch.replyText 统一错误处理。方案：cron 持 `Notifier interface { Notify(ctx, plat, chatID, text) error }`，server 注入实现。涉及：`internal/cron/scheduler.go:1864`。
- [ ] **R219-ARCH-9 — `workspaceOverrides` 与 `sessions.json` 双 atomic write 不一致风险（P2）**: 两个独立 dirty bit/gen counter/atomic write，部分失败导致重启后 session 引用了不在 overrides.json 的 chat workspace，无 reconciliation 路径。方案：合成单文件 atomic write（sessions.json schema 加 workspace_overrides 字段）或启动期一致性检查。Breaking：是（store schema migration）。
- [ ] **R219-ARCH-10 — `--dangerously-skip-permissions` hardcode 在 Protocol 层（P2 R215-SEC-P1-1 架构视角）**: 应该是 `BackendProfile` 或 `SpawnOptions.PermissionMode` 字段。方案：`SpawnOptions.PermissionMode {Skip, Default, AutoGrant}`，Protocol.BuildArgs 据此决定参数。Breaking：否（增量字段零值兼容）。
- [ ] **R219-ARCH-11 — `discovery.DefaultScanner` package singleton 阻碍多租户隔离（P2 R172-ARCH-D9 重申）**: 自 R172 登记起未推进，server.discoveryCache 与 cmd/naozhi 都通过包级 wrapper 假设进程内单 scanner。方案：`RouterConfig.HistoryScanner *discovery.Scanner`，nil 走 DefaultScanner 兼容老路径。
- [x] **R219-ARCH-12 — DESIGN.md 实际状态机已偏离描述（P3）**: DESIGN.md 描述 4 状态 `Spawning/Ready/Running/Dead`，实现已加 Paused/Suspended/stub/scratch ephemeral/exempt 等 7+ 状态。方案：DESIGN.md 补真实状态枚举 + state diagram 或链 ADR。 — 已修复（DESIGN.md 把 4 态明确为 cli.ProcessState 进程级状态机 + 链权威定义，把 Exempt/Stub/Scratch/Paused 单列为 ManagedSession 上层"语义标签"可正交组合），本批 PR #86
- [x] **R219-ARCH-13 — DESIGN.md L292 event log 持久化"单文件 single goroutine"承诺与 per-session writer N goroutine 现实不符（P3）**: 文档与实现漂移。方案：DESIGN.md 改"per-session writer, batched fsync, 100ms flush tick"。 — 已修复（DESIGN.md L292 改"per-session 专用 writer goroutine + bufio + 100ms flush tick batched fsync"），本批 PR #86

## Round 230 — 5-agent 并行 code review（2026-05-21）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）跑了全仓 review。本轮 12 处直接修已落 PR；以下为 breaking / 跨模块 / 需设计决策的发现登记追踪。

### Architecture（架构债）

- [ ] **R230-ARCH-1 — sysession.Runner 直 exec `claude -p` 旁路 cli.Wrapper（P1）**: `internal/sysession/runner.go` 每次 Run 起新 claude -p 子进程，绕过 backend 选择 / shimEnvAllowedPrefixes / `--setting-sources ""` / ARG_MAX 守卫。新增 backend (Gemini) 需在此重做一遍。方案：`backend.Profile.RunOneShot(ctx, prompt) (string, error)` 抽接口由 Runner 复用。Breaking：是。
- [ ] **R230-ARCH-2 — Hub.ownerLoop / runTurn 与 dispatch.Dispatcher.ownerLoop / sendAndReply 几乎逐行重复（P1）**: `internal/server/send.go` 与 `internal/dispatch/dispatch.go` 各一份 collect-timer + drain + Discard/recover 实现，"对齐 dispatch.ownerLoop" 注释自承。方案：抽 `TurnRunner` 或把 dashboard 走 LocalChannel 适配器纳入 dispatch。Breaking：是。
- [ ] **R230-ARCH-3 — 五个 SessionRouter 子集接口手工对齐易漂移（P1）**: dispatch / cron / sysession / server.HubRouter / upstream 各持一份，已出现 NotifyIdle / SetUserLabelWithOrigin 不对称。方案：在 session 包合成 `Reader/Writer/Lifecycle` 三联，消费方按需 compose。Breaking：否（接口断言迁移）。
- [ ] **R230-ARCH-4 — session.Router 60+ 方法跨 7 大职责（P1）**: 每消费方都能触达全量。方案：facet 化 `Router.Backends() / Lifecycle() / Stubs()`。Breaking：否（增量 facet）。
- [ ] **R230-ARCH-5 — server.Hub 45 方法 24 字段第二 Router（P2）**: 同时持 router / scheduler / scratchPool / queue / dedup / uploadStore / auth / tailers / nodes，nodesMu 与 Server 共指针为耦合 smell。方案：抽 WSEventBus / SendCoordinator / AgentTailerSet / nodeRegistry。Breaking：是。
- [ ] **R230-ARCH-6 — upstream 反向 RPC 第三套 send 管线（P1）**: `connector_rpc.go` 直 `sess.Send`，绕过 MessageQueue / dedup / usermsg / replyError 计数，反向流量在监控里不可见。方案：让 upstream 走共享 TurnRunner / Dispatcher.Send。Breaking：是。
- [ ] **R230-ARCH-7 — 错误→用户消息映射有 3 处偏序（P2）**: `usermsg.ForSendError` 是规范源，但 dispatch.sendAndReply 仍内联 ErrMaxProcs / ErrMaxExemptSessions / 超时分支，apierr.localizeAPIError 是第四套。方案：超时参数注入 usermsg 或 dispatch 侧 helper，dispatch 内联 switch 收敛。Breaking：否。
- [ ] **R230-ARCH-8 — config 加载语义二元（P2）**: cmd/naozhi/main.go Load 一次 + per-spawn RefreshSettings 仅覆盖 ~/.claude/settings.json；其余字段须 systemctl restart。方案：写 ADR 明确"load-once 例外"清单 + 在 RouterConfig.Wrappers 等字段 godoc 标注 immutable-after-construction。Breaking：否（文档）。
- [ ] **R230-ARCH-9 — KeyResolver 三处独立实例（P2）**: cmd/naozhi/main.go upstreamResolver / Server.resolver / Dispatcher fallback 三份缓存，project 变更后异步漂移。方案：projectMgr 暴露 Resolver() 单点。Breaking：是（构造签名）。
- [ ] **R230-ARCH-10 — plannerKey* 在 session/key.go 与 project 包重复（P2）**: 注释自承"hardcoded test assertion synced"。方案：抽 `internal/keys` 中性包，两侧 import。Breaking：否。
- [ ] **R230-ARCH-11 — dashboard.js PLATFORM_ORIGINS 硬编 IM 列表（P2）**: 新增平台需 4 处同步：adapter / main.go initPlatforms / dashboard.js / dashboard.html CSS。方案：`GET /api/platforms` 返回 `{id, displayName}[]`，前端启动时 hydrate。Breaking：是（前端模板）。
- [ ] **R230-ARCH-12 — Dispatcher 实例只在 main.go 串到 Feishu，Hub 仅借 MessageQueue 引用（P2）**: dashboard send 与 IM send 实质两套抽象层。方案：Server 持 Dispatcher 由 Hub 借用，或队列 + ownerLoop 抽第三包。Breaking：否（重构）。
- [ ] **R230-ARCH-13 — cli.Wrapper 直 import internal/shim 模糊协议/传输边界（P3）**: `cli.Transport` 接口缺位，shim 是公共字段。方案：定义 cli.Transport，shim/direct-exec 各一实现。Breaking：是。
- [ ] **R230-ARCH-14 — internal/session/router_core.go 用 blank import 注册 history factory（P2）**: Sprint 1b 注释自承"将合并到 wireup 包"。任何 Router 测试都触发全局 registry 改动。方案：`internal/wireup/wireup.go` + 显式 RegisterDefaults() 在 main.go 调。Breaking：否（迁移）。
- [ ] **R230-ARCH-15 — internal/server 90+ Go 文件单包（P3）**: 按 handler-group struct 切分而非 Go package 边界。方案：下次触动 auth/cron/scratch handlers 时迁子包。Breaking：是（import path）。
- [ ] **R230-ARCH-16 — Router.spawnSession 直接调 *cli.Wrapper.Spawn（P3）**: cli.Wrapper 是 struct 非 interface，使 Router 单元测试需 panicSafeSpawn 替身。方案：`cli.Spawner interface { Spawn(ctx, opts) (Process, error) }`。Breaking：否。
- [ ] **R230-ARCH-17 — internal/cli 65+ 文件混 protocol/process/eventlog（P3）**: 同 R230-ARCH-15 同性质。方案：subpackage 化 cli/protocol cli/eventlog cli/subagent。Breaking：是。
- [ ] **R230-ARCH-18 — Router.StartCleanupLoop / StartShimReconcileLoop 由 main.go 各起 goroutine（P2）**: Router 既不是完整 Run 服务也不是纯被动 struct，未来调用方易漏 Tick。方案：合并 `Router.Run(ctx)` 一次启动所有循环。Breaking：是。
- [ ] **R230-ARCH-19 — Cron / Sysession / dashboard 各自一套 stub session 注册策略（P3）**: RegisterCronStub / RegisterCronStubWithChain / RegisterSystemStub 三方法 + exemptKeyPrefixes 链式 HasPrefix。方案：`StubKind` enum + 单一 RegisterStub。Breaking：是。
- [ ] **R230-ARCH-20 — Server ↔ Hub 共享 nodesMu *sync.RWMutex 别名（P2）**: 表明 nodes 应独立 owner（`*node.Registry`）而非任一方持。方案：抽 nodeRegistry，Server 与 Hub 按 interface 消费。Breaking：是（构造）。

### Code quality（剩余）

- [ ] **R230-CQ-1 — Hub.wiredLinkers map 键直持 *cli.SubagentLinker（P3）**: server 包直接耦合 cli 类型。方案：定义 session.AgentLinker 接口或暂改 `map[any]struct{}` 兜底。Breaking：否。
- [ ] **R230-CQ-2 — sendAndReply 241 行 5+ 职责（P2）**: 内含 5s 超时 nested defer，复杂度高。方案：抽 buildReplyContext / handleSendResult。Breaking：否。
- [ ] **R230-CQ-3 — dispatch.go log 参数遮蔽 stdlib log 包（P2）**: scheduler.go 已用 lg 命名规避，dispatch 未对齐；reactions.go logger nil 兜底不一致。方案：dispatch 三函数统一 lg；reactions.go drop nil 守卫改 slog.Default()。Breaking：否。
- [ ] **R230-CQ-4 — processIface.GetState/GetSessionID 命名违 Go 风格（P2 R219-CR-9 重申）**: 12 处调用点 + 两个 fakes 需机械重命名。方案：State()/SessionID()。Breaking：是（interface 改名）。
- [ ] **R230-CQ-5 — internal/session/testutil.go 无 build tag 进生产二进制（P2 R226-CR-14 重申）**: TestProcess 30+ 导出字段。方案：`//go:build testing` 或重命名 `_test.go`。Breaking：否（test only）。
- [ ] **R230-CQ-6 — validateCronTitle 单独实现 UTF-8 + C0 + IsLogInjectionRune（P2）**: 与 validateStringField 三重扫描重复。方案：stringFieldPolicy 加 singleLineError bool。Breaking：否。
- [ ] **R230-CQ-7 — ownerLoop drain pattern 含冗余 default: drain（P2）**: Go 1.23+ Stop 已 drain；project go.mod 1.26。方案：简化为 Stop()。Breaking：否。
- [ ] **R230-CQ-8 — reconnectShims case 内 90+ 行内联（P2 R229-CR-3 重申）**: 仍未抽 processDiscoveredShim 子函数。Breaking：否。
- [ ] **R230-CQ-9 — cron.executeOpt 329 行 7+ 错误分支（P2 R229-CR-1 重申）**: handleSendError / deliverAndRecord 抽取。Breaking：否。
- [ ] **R230-CQ-10 — NewRouter 359 行内联三阶段初始化（P2 R229-CR-2 重申）**: newRouterRestoreSessions / newRouterStartHistoryLoads 抽取。Breaking：否。
- [ ] **R230-CQ-11 — handleOwnerLoopPanic 用 slog.Error 包级 logger 缺 key/agent 富化（P3）**: dispatch.go panic 路径与 ownerLoop 主路径属性不一致。方案：参数透传 lg。Breaking：否。
- [ ] **R230-CQ-12 — backend 错误信息 3 处不一致（"invalid backend length"/"backend exceeds %d-byte limit"/"invalid backend identifier"）（P3）**: API 客户端字符串匹配会漏。方案：统一走 session.validateBackend 或共享格式化 helper。Breaking：是（外部串匹配方需迁）。
- [x] **R230-CQ-13 — rtruncByteLen 与 textutil.TruncateRunesNoEllipsis 重复（P3）**: dashboard_session.go 私实现一份。方案：统一调 textutil。Breaking：否。 — 已修复（新增 textutil.TruncateAtRuneBoundary 字节级 rune-boundary 反向截断 helper + 7 case 表驱动测试，dashboard_session.go / dashboard_transcribe.go 切换调用，删 rtruncByteLen 私实现 13 行），本批 PR #210
- [ ] **R230-CQ-14 — cron/scheduler.go 2745 行单文件无拆分计划（P3 R226-CR-11 重申）**: 建议先建 scheduler_job.go / scheduler_run.go / scheduler_notify.go 骨架。Breaking：否。
- [x] **R230-CQ-15 — Process.statusLines 无环 cap 文档与实现不一致（P3）**: 命名"pre-allocated capped ring"但实际无 trim。方案：每写入 trim 至 maxStatusLines=20。Breaking：否。 — 已修复（条目实际位于 dispatch.replyTracker.statusLines；行为正确——appendStatusLine 通过 maxStatusLines=8 + copy-to-front trim head；旧注释"pre-allocated capped ring"误导，改为精确描述 + 指向 status.go 的 trim 实现），本批 PR #210
- [x] **R230-CQ-16 — processIface godoc 描述与实际范围不符（P3）**: 注释只说 router 用，实际 wshub / server / cron 都用。方案：更新 godoc 表述实际边界。Breaking：否。 — 已修复，本批 PR #213
- [x] **R230-CQ-17 — shimManagers() 命名与 dedup 注释稍有歧义（P3）**: 实际按 *shim.Manager 指针 dedup。方案：注释精确化。Breaking：否。 — 已修复，本批 PR #213
- [x] **R230-CQ-18 — loadTotalCost/storeTotalCost 仍内联 math.Float64bits（P3）**: textutil 有 atomic string helper 但无 float64 版。方案：补 textutil.Load/StoreAtomicFloat64 或加交叉引用注释。Breaking：否。 — 已修复，本批 PR #213
- [x] **R230-CQ-19 — validateCronPrompt 对 \r 拒绝行为缺注释说明（P3）**: 与 validateCronTitle 显式 \r case 风格不一致。方案：加注释说明 prompt 不接 \r 是 JSON+UTF-8 入库约定。Breaking：否。 — 已修复，本批 PR #213

### Security（剩余 — 本轮新发现）

- [ ] **R230-SEC-1 — Dashboard CSP `script-src 'unsafe-inline'`（P1 R229-SEC-6 重申）**: 主页面 CSP 仍允许任意 inline；登录页已用 SHA-256 hash 严格 CSP。方案：迁 nonce 模式，把 dashboard.html 内联 onclick 等事件外移；KaTeX/mermaid 通过 createElement+SRI 注入已不依赖 unsafe-inline。Breaking：是（前端较大改造）。
- [ ] **R230-SEC-2 — shim XDG_ env 前缀过宽（P2）**: `shim/manager.go:900` `XDG_` 前缀放行 XDG_CONFIG_HOME / XDG_CONFIG_DIRS / XDG_DATA_DIRS，理论上可重定向 CLI 配置/数据查找。当前测试契约（manager_test.go:517）显式允许 XDG_CONFIG_HOME，调整需同步重写测试。方案：精确 `XDG_RUNTIME_DIR=` `XDG_CACHE_HOME=` `XDG_STATE_HOME=`，剔除 CONFIG/DATA。Breaking：是（测试契约 + 部署期依赖 XDG_CONFIG_HOME 转发的运维方）。
- [ ] **R230-SEC-3 — Dashboard JS 静态资源未鉴权（P2）**: `/static/dashboard.js` 与 `/static/agent_view.js` 不走 requireAuth，HTTP 部署下中间人可替换。方案：dashboardToken 非空时对 JS 端点也加 requireAuth；或文档化 TLS 必备。Breaking：否（鉴权后浏览器需先认证才能加载，与 SPA 流程一致）。
- [ ] **R230-SEC-4 — sessions.meta.json 非原子写（P3）**: store.go:225 单 WriteFile，partial write 可产生半截 JSON。方案：要么 WriteFileAtomic（额外 2 次 fsync），要么文档化"sidecar partial = unknown version 自然 fallback"。Breaking：否。
- [ ] **R230-SEC-5 — handleFileGet TOCTOU prefix 复检缺（P3）**: project_files.go:585 仅 ModeSymlink 标志检查，未在 Lstat 后再次复核 resolved 仍在 rootResolved 之下。方案：加二次 HasPrefix 防御。Breaking：否。

### Go / Concurrency（剩余 — 本轮新发现）

- [ ] **R230-GO-1 — historyWg.Add(1) 与 historyCtx.Err() 检查间存在 TOCTOU 窗口（P1）**: 新增 R229-GO-4 修复在 cancel 触发后 Add(1) 与 shutdown goroutine 内 Wait() 计数=0 仍可能竞态导致 panic。方案：mu 保护 Add 序列，或把 Add(1) 提到 Err 检查前并在跳过分支立即 Done。Breaking：否。
- [ ] **R230-GO-2 — atomic.Pointer[OnExecuteFunc] Setter 对参数取址致逃逸（P2）**: scheduler SetOnExecute/SetOnRunStarted/SetOnRunEnded 三处 `&fn` 取局部参数地址，每次调用 1 alloc。方案：包装结构体显式存指针或改 atomic.Value。Breaking：否。

### Performance（剩余 — 本轮新发现）

- [ ] **R230-PERF-1 — ACP Init / session/load / session/new 仍用 map[string]any 参数（P3）**: 已对 prompt 路径定义 acpPromptParams（R228-PERF-4），但握手三 RPC 仍 map。方案：定义 acpInitParams/acpSessionLoadParams/acpSessionNewParams 类型化。冷路径但风格不统一。Breaking：否。
- [ ] **R230-PERF-2 — process_event_format tool_result 分支整 ToolCall 复制堆分配（P3）**: `tc := *ev.ToolCall` 每次 tool_result 1 alloc ~112B。方案：`entry.ToolCall = ev.ToolCall` 直接共享指针；Append 已按值拷贝 entry，所有权安全。Breaking：否。
## Round 230B — 5-agent 并行 code review（PR #198）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）跑了全仓 review。本轮 17 处直接修已落本轮 PR；以下为非 breaking 但需要更大改动 / 需设计决策 / 跨模块的发现，登记追踪。

### Security（剩余）

- [ ] **R230B-SEC-1 — `recordResult` 死代码缺 RunCounters 更新（P2）**: `scheduler.go:2392` 仅在 persist_failure_test 中调用，与 `recordResultP0WithSanitised` 语义分叉（少 j.RunCounters.addRun + LastErrorClass 字段）。方案：删除 `recordResult` 改测试调 P0 变体。Breaking：否（测试调整）；本轮跳过因测试需要新签名 (errClass, state, 返回值) 改写。
- [ ] **R230B-SEC-2 — Backend ID charset 三处不一致（P2）**: `dashboard_cron.go:198` (`[a-z0-9_-]`) vs `select_node_for_backend.go:46` WS (`[a-zA-Z0-9_.-]`) vs `send.go:266` HTTP (`[a-z0-9_-]`)。本轮已收敛 maxCronBackendLen → maxBackendIDLen 但 charset 策略未统一。方案：决定是否允许大写 + `.`，统一到包级 `isValidBackendID` 一处。Breaking：是（如现有 backend ID 含大写或 `.`）。
- [ ] **R230B-SEC-3 — `cli.backends[*].args` 缺 flag 允许列表（P3）**: `validateArgvStrings` 已拒控制字节但允许任意 `--flag`。方案：与 R229-SEC-1 同批引入 flag allowlist。Breaking：是。
- [ ] **R230B-SEC-4 — `.conf`/`.cfg`/`.ini` previewable 可能泄漏凭据（P3）**: `project_files.go:90-99` 把这三类文件映射到 `text/plain`。方案：从 previewableByExt 移除或改 application/octet-stream 强制下载；至少加注释明确风险决策。Breaking：否（行为变化：从预览改下载）。
- [ ] **R230B-SEC-5 — dashboard CSP `img-src data:` 防御缺口（P3）**: 数据 URI 允许 SVG-with-script + 外发 GET 探针。方案：移除 `data:`；审计 dashboard.js 改用 blob URL。Breaking：是。

### Go 正确性 / 并发（剩余）

- [ ] **R230B-GO-1 — `cron.executeOpt` sendCtx 与 spawnCtx budget 不共享（P2）**: `scheduler.go:1955` sendCtx 用 `context.Background()` + jobTimeout，可能在 spawn 已耗 budget 后仍跑满 jobTimeout。总墙钟接近 2×jobTimeout。方案：sendCtx 用 `jobTimeout - time.Since(startedAt)` clamp ≥0。Breaking：否（行为变化，需评估 cron 长任务）。
- [ ] **R230B-GO-2 — `subagent_link.Resolve` retry sleep 不响应 ctx 取消（P2）**: `subagent_link.go:294/332` 重试循环 `time.Sleep` 不察 ctx；router_shim.go:398 `go linker.Resolve` bare goroutine。方案：Resolve 加 ctx 参数 + select stop signal。Breaking：是（接口签名 + 调用方）。
- [ ] **R230B-GO-3 — `recordResultP0WithSanitised` / `recordResult` mu Unlock 非 deferred（P2）**: 多个 early-return 各自手动 Unlock，未来插早返路径易遗漏。方案：拆 stateMutate（持锁）+ stateCommit（锁外 save/fn 调用）两阶段。Breaking：否（内部重构）。
- [ ] **R230B-GO-4 — `Hub.handleSubscribe` O(N) maxSubscribersPerKey 扫描在写锁下（P2）**: `wshub.go:556` Lock 期间扫所有 connections 阻塞 BroadcastSessionsUpdate。方案：先 RLock 数完再 Lock 短期 reserve；或 per-key sync.Map 计数。Breaking：否。
- [ ] **R230B-GO-5 — `eventLogPersister.Stop` 用 context.Background 而非 shutdown ctx（P3）**: `router_cleanup.go:737` 不在本轮修因 shutdown 时 historyCtx 已 cancel，会立刻 timeout 失去 5s flush 窗口。方案：增加专用 stopCtx 或 stop 阶段单独的 5s 兜底（绕过 historyCtx）。Breaking：否（设计决策）。

### Performance（剩余）

- [ ] **R230B-PERF-1 — `wshub.eventPushLoop` 同 session N 个 tab 各自 marshalPooled（P1）**: `wshub.go:1099` 同批事件 N tab 时 marshal 成本 O(N)。方案：marshal once → SendRaw 字节 fan-out。Breaking：否。R219-PERF-1 / R225-PERF-9 重申。
- [ ] **R230B-PERF-2 — `Snapshot` `proc.TurnAgents()` 始终 alloc（P2）**: count==0 已短路，count>0 时仍 make+copy。方案：`TurnAgentsBuf(dst)` 接受 caller slice 复用，或 SessionSnapshot 内嵌固定 4-元数组。Breaking：否。R225-PERF-6 重申。
- [ ] **R230B-PERF-3 — `ListSessions` SessionSnapshot slice 1Hz 持续分配（P2）**: 50 sessions × 1 Hz × N tab。方案：handleList 加 storeGen 缓存或 sync.Pool 池化结果。Breaking：否。R229-PERF-10 重申。
- [ ] **R230B-PERF-4 — `mapAssistantLine` / `mapUserLine` 用 `[]map[string]any`（P2）**: agent tailer 高频路径，map+interface boxing 比命名 struct 高 3-5×。方案：参照 process_event_format.go `ContentBlock` 形式。Breaking：否。
- [ ] **R230B-PERF-5 — `subagent_transcript.readLocked` 每次 open+seek+ReadAll（P2）**: 50 tailer × 1s = 50 syscall/s。方案：保持 fd open + offset 增量；inotify 选项后续讨论。Breaking：否。
- [ ] **R230B-PERF-6 — `eventlog_bridge` 单条快路径仍 copy raw bytes（P2）**: bridge 即使 single entry 仍 make+copy。方案：核对 Persister 留持契约，能 zero-copy 则免拷。Breaking：否（需仔细审 contract）。
- [ ] **R230B-PERF-7 — task_started Description rune scan（P3）**: `process_readloop.go:518` Description 截断已在 goroutine 启动前完成；可改 byte 上限 min(len, 2000*4) 跳过 rune 计数。Breaking：否。
- [ ] **R230B-PERF-8 — `notifySubscribers` map iteration vs slice（P3）**: subCount==1 极常见，map range 不必要。方案：count==1 fast path 直接取 + count<=4 时 slice 存储。Breaking：否。

### Code 质量（剩余）

- [ ] **R230B-CR-1 — `runIDLenLimit=64` 与 `cron.MaxIDLen` 各自定义（P3）**: 注释声明"kept separate"但未来漂移风险。方案：评估是否真要分开，否则统一。Breaking：否。
- [ ] **R230B-CR-2 — `formatTZOffset` 接受 IANA name 并展示（P3）**: name 参数实为 loc.String() 而非 zone abbr，渲染 "Asia/Shanghai (UTC+08:00)" 与 timezone_abbr 重叠。方案：统一只传 zone abbr 或重命名参数。Breaking：否（仅展示文案）。
- [ ] **R230B-CR-3 — `dashboard_cron.handleList`/`handleRunsList` map[string]any 响应（P3）**: 1Hz poll 反射 alloc。方案：定义命名 struct（参 R226-PERF-7 dashboard_session）。Breaking：否。
- [ ] **R230B-CR-4 — `trimJobLocked` 用 mtime / `cacheTrimAfterDisk` 用 StartedAt（P3）**: disk vs cache 过期判断时间源不一致，长任务+短窗口下短暂分歧。方案：选其一统一。Breaking：否。
- [ ] **R230B-CR-5 — `redactPathsInCronError` `maxErrLen=2048` + `SanitizeForLog 512` magic（P3）**: 散在两处的 cron 错误消息长度策略。方案：抽包级常量。Breaking：否。

### Architecture（剩余 P1，需设计）

- [ ] **R230B-ARCH-1 — `session` 包硬编码 import 4 个 backend-specific history 包（P1）**: `router_core.go:18-32` blank-import claudejsonl/kirojsonl 触发 init 注册，session 是协议无关层假设破裂。方案：cli.Wrapper.NewHistorySource 工厂封装；session 仅依 history.Source 接口。Breaking：是（~20 callsite）。
- [ ] **R230B-ARCH-2 — Hub/Server 半构造对象反模式（P1）**: 8+ 处 `if h.scheduler != nil` 守卫散落；Set* setter 注入顺序硬编码。方案：HubOptions 一次性装配 + null-object fallback。Breaking：否（内部重排）。立即可落地（~30 行）。
- [ ] **R230B-ARCH-3 — `cron.scheduler` 越层直接持 platform map + SplitText（P1）**: 与 dispatch 平行第二条 IM 出站路径，错误处理重复。方案：注入 Notifier interface。Breaking：否（cron 内部 + 兼容 fallback）。
- [ ] **R230B-ARCH-4 — `cli.Protocol` 接口塌陷为 stream-json 专属（P1）**: 8 方法中 4 个 ACP 必 noop/panic。方案：拆核心 Protocol + PassthroughExt 可选接口。Breaking：是（cli.Protocol 导出 + 6 处 type-assert）。
- [ ] **R230B-ARCH-5 — `cli.EventEntry` 已塌陷为跨层 DTO（P1）**: 26+ 包 import cli.EventEntry。方案：迁到叶子包 internal/event 零依赖。Breaking：是（接口签名）。
- [ ] **R230B-ARCH-6 — `sessions.json` + `workspace-overrides.json` 双 atomic write 不一致（P1）**: 部分失败下重启出现孤立 override。方案：合并 schema 单文件原子写或启动期一致性扫描。Breaking：是（schema migration）。立即可落：启动期扫描 ~30 行。
- [ ] **R230B-ARCH-7 — `server` 包 backend fan-out 失控（P1）**: server.go 单文件 import 10 个 internal 包，是"god 包"伪装。方案：抽 internal/app 装配包，server 还原为纯 HTTP handler。Breaking：否（内部）。
- [ ] **R230B-ARCH-8 — `cli.Wrapper` 持 `*shim.Manager` 单例假设破裂（P1）**: multi-backend 部署每 Wrapper 一份 ShimManager 副本可能撞同一系统资源。方案：BackendProfile + 共享 ShimManager 注入。Breaking：是。
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
- [ ] **R230B-ARCH-21 — eventlog WireVersion=1 无 forward-compat 协商（P3）**: bump 后旧 reader 整文件 fallback。方案：加 MinReadVersion。Breaking：否。立即可落地（~40 行）。
- [ ] **R230B-ARCH-22 — shim ProtocolVersion=1 硬编码无 negotiation（P3）**: 零中断热重启场景二进制不匹配 hard-fail。方案：MinSupportedVersion / MaxSupportedVersion 协商。Breaking：否。立即可落地（~30 行）。
- [ ] **R230B-ARCH-23 — selfupdate 无回滚 / 健康检查 hook（P3）**: panic 后只能 ssh 手动回退。方案：systemd 启动 30s 内 self-call /health 失败自动 .prev 回退。Breaking：否。
- [ ] **R230B-ARCH-24 — Server struct 30+ 字段 god object（P3）**: 已抽 *Handlers struct 但仍持每个指针。方案：mountAuth(mux)/mountCron(mux) 子构造，Server 退成 Listener+middleware+Mux 容器。Breaking：否。
- [ ] **R230B-ARCH-25 — EventLog SetPersistSink 时序契约靠 metric 兜底（P3）**: 方案：合并 InjectHistory+SetPersistSink 为一次性 Initialize 调用。Breaking：是。
- [ ] **R230B-ARCH-26 — feishu.go 1000+ 行 + 各平台拆分粒度不一致（P3）**: 方案：每平台拆 4 文件 transport/wire/outbound/capability。Breaking：否。
- [ ] **R230B-ARCH-27 — DESIGN.md 未区分 process / managed-session / chat 三层状态机（P3）**: 方案：三个独立状态图。Breaking：否（仅文档）。

## Round 229 — 5-agent 并行 code review（2026-05-20）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）跑了全仓 review。14 处直接修已落本轮 PR；以下条目为非 breaking 但需要更大改动 / 需设计决策 / 跨模块的发现，登记追踪。

### Security（剩余）

- [ ] **R229-SEC-1 — ExtraArgs flag 注入未受限（P1）**: `protocol_claude.go:77` / `protocol_acp.go:158` 直接拼接 `opts.ExtraArgs` 到 argv，无 flag 允许列表。受信认证用户可注入 `--mcp-config` / `--add-dir /etc` / `--skip-permissions` 等危险 flag。方案：BackendProfile 声明 flag allowlist；任何以 `--` 开头且不在白名单的 ExtraArgs 元素拒绝并 Warn。Breaking：是（依赖任意 ExtraArgs 的运维方需迁移到允许列表）。
- [ ] **R229-SEC-2 — serveRender TOCTOU inode-swap（P1）**: `project_files.go:683` 在 `os.Lstat(resolved)` 后再 `serveRender → os.Open(resolved)`，攻击者可通过 Claude Write 工具在窗口内创建符号链接指向 `/etc/passwd`。方案：`handleFileGet` 直接 OpenFile 拿 fd 传入 serveRender，或 Open 后 Fstat 比对 inode。Breaking：否（内部重构）。
- [ ] **R229-SEC-3 — allowed_root 缺失不阻断启动（P1）**: `server.go:513` 公网监听 + dashboard_token 配置时，`allowed_root` 为空只 Warn 启动。认证用户可设 cron `work_dir=/etc` 让 CLI 写系统文件。方案：dashboard_token 非空 + 监听非纯 loopback 时 fatal 启动失败 + naozhi doctor HIGH 级别检查。Breaking：是（现有公网部署需补 allowed_root）。
- [x] **R229-SEC-4 — moveToShimsCgroup 不验证 CLIPID 是否真为 shim 子进程（P2）**: `manager_linux.go:60` 用 `Hello.CLIPID` 直接调 sudo busctl StartTransientUnit。方案：使用前读 `/proc/<CLIPID>/status` 验 PPid==shim PID。Breaking：否。 — 已修复，本批 PR #190
- [ ] **R229-SEC-5 — ws:// node 连接明文传输 token（P2）**: `reverseserver.go:150` 反向 node 第一条消息明文携带 token。方案：`/ws-node` handler 在 r.TLS==nil 且无可信 X-Forwarded-Proto: https 时拒绝 Upgrade，或加 insecure_node 显式豁免 flag。Breaking：是。
- [ ] **R229-SEC-6 — Dashboard CSP `script-src 'unsafe-inline'`（P2）**: `dashboard.go:390` 主页面允许任意内联 script，登录页已用 SHA-256 hash 严格 CSP。方案：迁移到 nonce 模式，把 dashboard.html 内联 onclick 等事件外移。Breaking：是（前端较大改造）。
- [x] **R229-SEC-7 — /health 端点 version 泄漏 + 无限速（P2）**: `health.go:169` 未认证返回 git describe version 字段，公网可枚举版本。方案：将 version 移到 healthAuthSection，或对 /health 加 per-IP 60 req/min 限速。Breaking：否（移到 authSection 会改变 LB/监控可见字段，需先评估对外集成）。 — 已修复（version 移入 healthAuthSection；rate limit 留作未来增量），本批 PR #190
- [ ] **R229-SEC-8 — per-token WS 连接数无上限（P2）**: `wshub.go:307` 单 token 持有者可建 500 个 WS 连接绕过 maxSubscribersPerKey=20。方案：WS 升级时按 cookie MAC 或 IP 桶检查 per-token 连接 cap（如 20）。Breaking：否。
- [ ] **R229-SEC-9 — sandbox CSP `style-src 'unsafe-inline'`（P3）**: `project_files.go:730/928` 渲染沙箱允许内联 style，存在 CSS exfiltration 风险。方案：替换为 nonce/hash。Breaking：是（依赖内联 CSS 的报告渲染失败）。
- [x] **R229-SEC-10 — readLoop 无单事件总字节上限（P3）**: 篡改的 CLI 可发 10 MiB 嵌套 JSON 致 CPU 高占用。方案：`ReadEvent` 解析后对 ev.Message.Content 总字节数加 4 MiB 上限。Breaking：否。 — 已修复（stream-json 与 ACP 双路径加 maxAssistantMessageContentBytes=4 MiB 上限），本批 PR #190
- [x] **R229-SEC-11 — WS 认证前读限制过大（P3）**: 认证前阶段沿用框架默认大小限制。方案：Upgrade 后立即 SetReadLimit(512)，认证后扩到正常。Breaking：否。 — 已修复（readPump 入口按 authenticated.Load() 选 wsPreAuthMessageSize=512 / wsMaxMessageSize；auth case 成功后立即调高），本批 PR #174
- [ ] **R229-SEC-12 — CDN allowlist 与 SRI 配合不足（P3）**: dashboard CSP `script-src` 含 `https://cdn.jsdelivr.net`，SRI 失败时 CSP 仍允许加载。方案：迁 nonce 模式后从 script-src 移除 CDN 域名。Breaking：是（与 R229-SEC-6 合并）。
- [x] **R229-SEC-13 — EventLog/sessions.json 文件权限依赖 umask（P3）**: 默认未显式 0600，state_dir 父目录权限不当时本地他人可读对话历史。方案：osutil.WriteFileAtomic 显式 0600 + 启动期 state_dir 权限检查（参考 cookie_secret 0600 模式）。Breaking：否。 — 已修复（file 0600 已落地；naozhi doctor 见 state_dir group/world bit 时 warn），本批 PR #190

### Go 正确性 / 并发（剩余）

- [ ] **R229-GO-1 — ReattachProcessNoCallback 清 deathReason 与 mapSendError Store 存在 logical race（P1）**: `managed.go:413` 与 reconnectShims 路径下，cron Send 中飞时 reconnect 可能立即抹掉刚写入的 deathReason，损失诊断。方案：TryLock sendMu 失败时跳过清理或文档化所有调用点。Breaking：否。
- [ ] **R229-GO-2 — Snapshot() 包含读侧写副作用（P1）**: `managed.go:990` Snapshot 触发 `s.SetModel(liveModel)` mirror，违反 Snapshot 命名约定。方案：抽取 SnapshotReadOnly + 在 spawnSession/notifyChange 显式调 mirror。Breaking：是（重命名）。
- [x] **R229-GO-3 — Send → onEvent 仅在 thinking/tool_use 触发（P2）**: 纯文本 assistant 事件不触发 onEvent，cron/upstream 流式 progress 看到"假停滞"。方案：所有 assistant 事件触发或在 godoc 明确语义。Breaking：否。 — 已修复（Process.Send godoc 显式声明 onEvent 仅在 thinking/tool_use 触发，并指引订阅方走 EventLog.Subscribe 拿全量流），本批 PR #174
- [x] **R229-GO-4 — spawnSession 内 inline JSONL load 未挂 historyWg（P2）**: `router_lifecycle.go:693` 15s 历史加载可超过 30s shutdown 窗口，触碰 r.claudeDir。方案：加 r.historyWg.Add/Done + 检 r.historyCtx.Err。Breaking：否。 — 已修复，本批 PR #191
- [ ] **R229-GO-5 — InjectHistory lastPrompt/lastActivity 仅"为空才设"易被旧 JSONL 覆盖新 Send（P2）**: 启动期 500 条 JSONL replay 期间 concurrent Send 写入的最新值可能被 stale 值替代。方案：比较时间戳或始终偏向 Send 写入值。Breaking：否。
- [x] **R229-GO-6 — dropEventLogForKey/clearAttachmentTrackerRefs 用 context.Background 不继承 shutdown ctx（P2）**: shutdown 期 Remove 仍各等 2-5s。方案：传 r.historyCtx 或专用 stopCtx。Breaking：否。 — 已修复（两个 helper 改 parent 为 r.historyCtx，nil 时 fallback Background；shutdown cancel 即时传递到 DropKey / OnSessionRemoved，缩短 graceful drain），本批 PR #174
- [x] **R229-GO-7 — selfupdate.go 多处 return err 缺 fmt.Errorf 包装（P3）**: 297-414 行多处直接 return err。方案：统一加上下文 %w 包装。Breaking：否。 — 已修复（fetchFile/verifyChecksum/copyFile/SelfPath/LatestRelease 全 return err 加 fmt.Errorf 上下文 %w 包装），本批 PR #171
- [x] **R229-GO-8 — dashboard_scratch.go scratch 复制源历史用 EventEntriesBefore 而非 EventEntriesBeforeCtx（P3）**: 死进程 ring buffer 空时 promoted scratch 历史不全。方案：改用 Ctx 版走 historySource fallback。Breaking：否。 — 已修复（collectScratchContext 加 ctx 参数，handleOpen 传 r.Context()；before-window 改 sess.EventEntriesBeforeCtx 让死 session 走 disk fallback），本批 PR #171
- [x] **R229-GO-9 — StartCleanupLoop 重启循环无次数上限（P3）**: 持续 panic 时无限自重启。方案：连续失败 10 次后停跑 + Error 报警。Breaking：否。 — 已修复（公共 StartCleanupLoop 签名不变，内部抽 startCleanupLoop(ctx, interval, attempt) helper；panic 处理在 attempt+1≥cleanupLoopMaxRestarts(=10) 时打 "exceeded max restarts" Error 后 return；50s 自愈窗口覆盖偶发 panic），本批 PR #171

### Performance（剩余）

- [ ] **R229-PERF-1 — Protocol.ReadEvent string→[]byte 双 copy（P1）**: 每个 stream 事件分配 1 个 []byte（line size，50 B–200 KB）。方案：Protocol.ReadEvent 签名改 []byte，shimMsg.Line 改 json.RawMessage 同步消除中间 string 拷贝。Breaking：是（Protocol 接口变更，所有实现 + fakes 更新）。
- [x] **R229-PERF-2 — FormatToolInput 匿名 struct 命名 escape 到堆（P2）**: tool_use 事件每个调用 1 次 Unmarshal+1 次 scratch alloc。方案：包级命名 struct 替换匿名 literal。Breaking：否。 — 已修复（提 6 个 toolInputXxx 命名类型到包级，函数体内只 var s toolInputXxx，json reflect 缓存键稳定，无名字 escape；TestFormatToolInput 全表通过），本批 PR #171
- [x] **R229-PERF-3 — EventEntriesFromEventAt base 大结构体多次拷贝（P2）**: 5-block 事件 5×~240 B 栈拷贝。方案：循环内仅设变化字段。Breaking：否（需仔细处理字段重置）。 — 已修复，本批 PR #191
- [x] **R229-PERF-4 — wsclient.SendJSON 每次 json.Marshal 同样的小结构（P2）**: error/auth 类响应可预 marshal。方案：扩展 wsAuthOkMsg 模式覆盖最常见 error 响应。Breaking：否。 — 已修复（新增 wsErrNotAuthMsg/wsErrRateLimitedMsg 包级常量；readPump 4 处 not-authenticated + 2 处 rate-limited 分支改 SendRaw；TestWSPreMarshalledFrames 锁定 byte-equal 契约），本批 PR #173
- [ ] **R229-PERF-5 — EventLog.Append 单 entry 路径每次分配 1-slot 切片（P2）**: 5-50 events/s 持续分配。方案：sync.Pool of length-1 slices 或 EventLog 字段缓存。Breaking：否（注意 sink 留持契约）。
- [ ] **R229-PERF-6 — discovery.Scan 每次 O(N) os.ReadDir 调用（P2）**: 已有 promptCache/summaryCache，但 listJSONLsByMtime 未缓存。方案：(claudeDir, cwd) → mtime invalidated 缓存。Breaking：否。
- [ ] **R229-PERF-7 — SetAgentInternalID 写锁覆盖 ring buffer 反向扫描（P2）**: 8 个 sub-agent 并发 resolve 时 Append 串行化。方案：扫描阶段 RLock，需变更时升级写锁。Breaking：否。
- [x] **R229-PERF-8 — sanitizeImagesAligned 双 pass（P3）**: 当前一遍 allOK 扫 + 一遍 filter；R229 review 提议合并，但 fast-path 0 alloc 是设计意图。方案：保留双 pass 但写注释解释 fast-path；或只在 hot profile 显示 cost 时再优化。Breaking：否。 — 已修复（保留双 pass 形态 + godoc 显式说明设计权衡 + 警告"勿单 pass 化"，避免后续 review 反复提议），本批 PR #174
- [ ] **R229-PERF-9 — extractLastPromptUncached 大文件 fallback 全文扫两次（P3）**: 50 MB JSONL tail 无 prompt 时再读全文。方案：负面结果加 mtime keyed 短 TTL 缓存。Breaking：否。
- [ ] **R229-PERF-10 — ListSessions 每次 make([]SessionSnapshot)（P3）**: 1 Hz × N 客户端持续分配 ~20 KB。方案：sync.Pool 池化 slice 或流式 JSON 编码。Breaking：否。
- [x] **R229-PERF-11 — writePump 每条消息都 SetWriteDeadline + time.Now（P3）**: 高 throughput 客户端 vDSO 调用累积。方案：滚动 deadline 或定期刷新。Breaking：否。 — 已修复，本批 PR #176
- [ ] **R229-PERF-12 — Subscribe 每次 alloc subscriber+buffered chan（P3）**: 反复 tab reload 持续微分配。方案：sync.Pool 池化 subscriber。Breaking：否。

### Code 质量（剩余）

- [ ] **R229-CR-1 — cron.executeOpt 329 行高复杂度（P3）**: 单函数内含 CAS / preflight / spawn / send / classify / dispatch / record / notify 全流程。方案：抽出 executeTurn 子函数。Breaking：否。
- [ ] **R229-CR-2 — NewRouter 359 行未被 router-split 重构覆盖（P3）**: 持久化 init / restore / 异步 history 三段可拆 helper。方案：抽 newRouterRestoreSessions / newRouterStartHistoryLoads。Breaking：否。
- [ ] **R229-CR-3 — reconnectShims 350 行 + 单分支 90 行（P3）**: 已抽 classifyShimState，shimStateReconnect case 仍臃肿。方案：抽 processDiscoveredShim helper。Breaking：否。
- [x] **R229-CR-4 — sessionSendLegacy 可达且去除条件未推进（P3）**: send.go:561 仍在生产路径上。方案：升级 NewHub 缺 Queue 时的 Warn 到 Error；推进 R-LEGACY-SEND 移除条件。Breaking：否。 — 已修复（NewHub 在 opts.Queue==nil 时把 slog.Warn 升级 slog.Error 并标 R-LEGACY-SEND blocker；不阻断启动，因 test fixture 仍有部分 nil Queue，待全部迁移到真实 MessageQueue 后下一步改 fatal 即可删 sessionSendLegacy），本批 PR #171
- [x] **R229-CR-5 — sessionSendLegacy 用 InterruptSession 而非 InterruptSessionSafe（P3）**: SIGINT 终止 claude -p 损失 resume。方案：换 InterruptSessionSafe（先尝试 control_request）。Breaking：否（ACP 不变 / Claude 升级到非破坏性中断）。 — 已修复（sessionSendLegacy 改用 InterruptSessionSafe，与 wshub.handleInterrupt 等其他 dashboard 入口对齐；server 测试全绿），本批 PR #173
- [x] **R229-CR-6 — managed.go 1489 行混合 struct/方法/工具（P3）**: 方案：抽 keys_util.go（SessionKey/sanitize/SanitizeLogAttr）。Breaking：否。 — 已修复（keys_util.go 抽出 SessionKey/Sanitize*/maxKeyComponent 等），本批 PR #190
- [ ] **R229-CR-7 — dispatch.go 1281 行 replyTracker 与 Dispatcher 同居（P3）**: 方案：抽 dispatch/reply_tracker.go。Breaking：否。
- [x] **R229-CR-8 — freshContextPreflightP0 8 位置参数（P3）**: 方案：仿 finishArgs 抽 preflightArgs struct。Breaking：否（内部）。 — 已修复，本批 PR #176

### Architecture（剩余 P1，需设计）

- [ ] **R229-ARCH-1 — internal/server 已成 god package（P1）**: 80+ 文件、Server 持 17+ 子 handler。docs/design/server-split-design.md Phase 3 未推进。方案：拆 internal/wshub + internal/api 子包。Breaking：否（包内重组）。
- [ ] **R229-ARCH-2 — internal/cli 越界承担 EventLog/image/SubagentLinker/history 工厂（P1）**: 21 处 session→cli 反向 import。方案：上移 EventLog/SubagentLinker 到 session/eventlog（已有 bridge 雏形），image/thumbnail 到 internal/attachment，history 到独立子包，cli 回归 Process+Protocol+Wrapper。Breaking：否。
- [ ] **R229-ARCH-3 — Router 单聚合根承载 6 大职责（P1）**: 文件级拆了，struct 仍持 30+ 字段 4 把锁、shutdown_lock_order_test 即证明易死锁。方案：拆 SessionStore + Lifecycle + DiscoveryService + ShimReconciler 四子组件。Breaking：是（外部引用 *session.Router 多）。
- [ ] **R229-ARCH-4 — ManagedSession 65 方法 + 隐式语义标签（P1）**: Exempt/Stub/Scratch/Paused 通过 process==nil + key 前缀推导。方案：拆 SessionMeta（持久化）+ LiveSession（运行时）+ 显式 tag enum。Breaking：是。
- [ ] **R229-ARCH-5 — KeyResolver/server/dashboard 双路径未收拢（P1）**: server.go buildSessionOpts legacy fallback + dispatch fallback resolver 并存，contract test 已证明。方案：删除 fallback，让 KeyResolver 唯一入口。Breaking：否（内部）。
- [ ] **R229-ARCH-6 — Channel Adapter 能力鸭子类型散落（P1）**: SupportsInterimMessages/AsReactor/AsQuestionCardSender/PermanentError 等可选接口持续增长，新增 LINE/Telegram 困难。方案：参照 cli.Caps 引入 PlatformCaps 聚合。Breaking：否（向后兼容）。
- [ ] **R229-ARCH-7 — main.go 持 settings.json 重写 / hooks 过滤 / env 过滤业务逻辑（P1）**: 方案：抽 internal/claudesettings 子包独立可测。Breaking：否。
- [ ] **R229-ARCH-8 — Dispatch DispatcherConfig 仍依赖 *session.Router 具体类型（P2）**: 接口化只完成一半。方案：与 R229-ARCH-4 配套切到 LiveSession 接口。Breaking：否。
- [ ] **R229-ARCH-9 — Hub 承担"send + broadcast" 越界（P2）**: dispatch 通过 SendFn 反向依赖 Hub.sendWithBroadcast。方案：抽 Sender 协调对象。Breaking：否。
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
- [ ] **R229-ARCH-20 — BumpVersion 双语义（DataVersion 与 RenderVersion 混用）（P3）**: 方案：拆两计数器。Breaking：否（dashboard 加新字段，老前端忽略）。
- [ ] **R229-ARCH-21 — DESIGN.md 与实际架构 LOC 严重偏离（P3）**: 方案：DESIGN.md 增加"实际架构"章节 + 当前包间依赖图。Breaking：否。

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
- [x] **R226-SEC-4 — Slack Socket Mode handler 无并发 cap（P2）**: 每条消息无限新建 goroutine（feishu 有 `hookSem` cap=20），高频/被攻 workspace 可 OOM。方案：仿 feishu 加 `slackSem chan struct{}` 容量 20，semaphore 满时 drop 并 slog.Warn。`internal/platform/slack/slack.go:376`。 — 已修复（hookSem chan cap=20 + 非阻塞 acquire + drop 时 slog.Warn, 与 feishu transport_hook 写法一致），本批 PR #153
- [x] **R226-SEC-5 — Discord 单条消息聚合下载无 cap（P2）**: 每附件 10MB 限制，但单消息可附 10 张 = 100MB / event；高频附图轰炸放大 OOM 风险。方案：`maxDiscordAttachmentsPerMessage = 5` 上限 + 总字节 cap。`internal/platform/discord/discord.go:363`。 — 已修复（在已有 5 张 cap 之上加 32 MiB 聚合字节 cap，超额时 slog.Warn 并停止下载；`aggregateAttachmentBytesAllow` 抽出做 7 case table-driven test），本批 PR
- [ ] **R226-SEC-6 — `allowed_root` 未配置只 warn 不阻断启动（P2）**: 拥有 dashboard token 的认证用户可把 cron `work_dir` 设到任意路径（如 `/etc`），CLI 子进程拿那个 CWD 跑，Write 工具可改 `/etc/passwd`。方案：当 `dashboard_token` 已配置且监听非 loopback 时，把 warn 升级为 fatal；或 `naozhi doctor` 加高严重度检查。`internal/server/server.go:513`。
- [ ] **R226-SEC-7 — `/health` 端点无认证无限速（P3）**: 暴露 session 数 / watchdog kill 计数 / 平台名 / node 状态 / build 版本，外部 attacker 可高频枚举 infra 拓扑、估算重启时序。方案：加 per-IP rate limiter（60 req/min burst 10）。`internal/server/health.go`。
- [x] **R226-SEC-8 — `wshub` 同 session key 无订阅 cap（P3）**: 单 token 攻击者可开 1000 WS 全订同 session，每事件 fan-out N 倍 CPU/mem。方案：per-session-key 订阅 cap（如 20）。`internal/server/wshub.go`。 — 已修复（`maxSubscribersPerKey=20` 在 handleSubscribe 持 h.mu 时 O(N≤500) 扫描 hits，超额返回 "too many subscribers for key"；同步关闭重申条目 R227-SEC-11），本批 PR
- [ ] **R226-SEC-9 — Sandbox CSP `style-src 'unsafe-inline'`（P3）**: `serveRender`/`serveRaw` 仍允许 inline CSS，CSS exfiltration 攻击面在沙箱内仍存。方案：迁 nonce 或 hash。`internal/server/project_files.go:708,905`。
- [x] **R226-SEC-10 — 附件路径加 `Content-Disposition: attachment` 给 SVG/XML（P3）**: 当前 ext allowlist 已挡掉 SVG，但万一未来开放或 magic-byte sniffer 误判，仍是 defence-in-depth 缺口。方案：在 `handleAttachment` 中对 sniff 结果含 `image/svg+xml` / `application/xhtml+xml` / `text/xml` 的强制 attachment + `X-Frame-Options: DENY`。`internal/server/dashboard_send.go:1148`。 — 已修复（sniff prefix 命中 image/svg+xml / application/xhtml+xml / text/xml / application/xml 时无条件 attachment + DENY frame, defence-in-depth 不替换既有 mismatch 路径），本批 PR #153

### 性能 — 本轮新发现

- [ ] **R226-PERF-1 — `Protocol.ReadEvent(line string)` 每事件 `[]byte(line)` 堆拷贝（P1，封 R67-PERF-1 实施分支）**: 5-50 ev/s × N session 的强制 alloc。方案：Protocol 接口签名 `ReadEvent([]byte)`，shimMsg.Line 改 `json.RawMessage`。Breaking：是（接口）。
- [ ] **R226-PERF-2 — `eventlog_bridge.newEventLogSink` 每 `Append` 1 单元 slice + JSON copy（P1）**: 5 sess × 5 ev/s ≈ 25 alloc/s + ~25 KB/s GC。方案：bridge 层加 `sync.Pool[[1]persist.Entry]` 复用 + 单条快路径 `AppendOne`。涉及 PersistSink 接口。`internal/session/eventlog_bridge.go:77`。
- [x] **R226-PERF-3 — `passthrough.writeUserMessageUnderShimLock` `&captureWriter{}` 逃逸到堆（P1）**: 每条 passthrough send 2 alloc。方案：`sync.Pool[*captureWriter]`，复用 backing slice（仿 `shimSendBufPool`）。`internal/cli/passthrough.go:182`。 — 已修复（captureWriterPool sync.Pool init cap=4096 + Get 后 reset + defer Put；shimSendLocked JSON 编码已 copy 字节, 复用安全），本批 PR #153
- [ ] **R226-PERF-4 — ACP `agent_message_chunk` 每 chunk 一次 `json.Unmarshal`（P2）**: kiro streaming 高频路径，500 unmarshal/s 仅此一处。方案：手写 byte-scan 提取 `"text":"..."` value，跳过 reflect。`internal/cli/protocol_acp.go:517`。
- [ ] **R226-PERF-5 — `eventlog.Append` 单条调用每次造 `[]EventEntry{e}` slice（P2）**: PersistSink 接口允许 retain slice 故复用受限。方案：加 `AppendOne(e)` 快路径或单元数组池。`internal/cli/eventlog.go:660`。
- [~] **R226-PERF-6 — `EventLog.applyEntryStateLocked` task 事件线性扫 turnAgents/bgAgents（P3）**: 多路 subagent 场景（>8 并行）双重 O(n)。方案：当 `len > 8` 时建 `map[string]int` 索引。`internal/cli/eventlog.go:405`。 — 评估后不实施（typical turnAgents len 1-3，result/user 事件已自动重置；threshold-based map 需 4 个同步映射 cover ToolUseID/TaskID × turn/bg，维护成本远高于收益；P3 + 无 >8 subagent 实测案例），本批 PR #164
- [x] **R226-PERF-7 — `handleList` 响应仍用 `map[string]any`（P2）**: 10 tabs × 1Hz × 2 alloc = 20 map alloc/s。方案：`type sessionListResp struct {...}`。`internal/server/dashboard_session.go:535,599`。 — 已修复（新增 sessionListLocalResp/sessionListMultiResp 两个 named struct + 两个调用点切换；JSON 字节兼容；history_sessions omitempty 保留，多节点 Nodes 不带 omitempty 匹配原 unconditional 赋值），本批 PR #164
- [x] **R226-PERF-8 — `process_event_format.TodosDetailJSON` 二次 marshal（P2）**: `block.Input` → `[]TodoItem` → marshal，可直接从 `block.Input` 提 `"todos"` 字段 raw bytes。`internal/cli/process_event_format.go:173`。 — 已修复（新增 ParseTodosWithRaw 同时返回 []TodoItem + 原始 todos JSON 字节；TodoWrite 分支直接用 rawTodos 喂 entry.Detail；新增数组字面量+malformed 双契约测试），本批 PR #166
- [x] **R226-PERF-9 — `ACPProtocol.WriteInterrupt` 每次 `json.Marshal`（P2）**: 静态模板 + UUID 拼接可省反射。`internal/cli/protocol_acp.go:274`。 — 已修复（手写字节模板 + 仅 sessionId 走 json.Marshal 快路径），本批 PR #151
- [ ] **R226-PERF-10 — `process_shim_io.shimWriter.Write` fast path `string(data[:len-1])` 拷贝（P3，封 R71-PERF-H1）**: shimClientMsg.Line 改 `json.RawMessage`。
- [x] **R226-PERF-11 — `ACPProtocol.mu` 同时保护 sessionID 和 textBuf（P3）**: chunk 高频时 textBuf 锁污染 sessionID 读路径。方案：sessionID 改 `atomic.Pointer[string]`（已有 model 先例）。`internal/cli/protocol_acp.go:88`。 — 已修复，本批 PR #158

### 代码质量 — 本轮新发现

- [ ] **R226-CR-7 — `RegisterForResume` / `RegisterCronStubWithChain` 用 `r.CLIName/Version` 在多 backend 部署下显示错误（P1）**: 这两条路径只看默认 wrapper；router_core.go loadStore 已正确走 `wrapperFor(entry.Backend)`。方案：加 `backend string` 参数 → `wrapperFor`。Breaking：caller API。`internal/session/router_discovery.go:259,362`。
- [x] **R226-CR-8 — `cron/scheduler.freshContextPreflight` 死代码（P1）**: 仅由 `freshContextPreflightP0` 委托调用；测试只测 P0 版本。方案：内联 P0 → 删 wrapper + 删过期注释 "keep intact for test surface"。`internal/cron/scheduler.go:1570`。 — 已修复（删 80 行 dead function + 收编 P0 godoc），本批 PR #151
- [x] **R226-CR-9 — dispatch/server 错误→中文消息双份重复（P2，封 R215-CR-P2-1 drift）**: 已加 sync 注释，但根治需提共享函数 `UserMessageForSendError(err, noOutTimeout, totalTimeout) (msg, severity)` 到 `internal/cli/usermsg.go` 或 `internal/session/usermsg.go`，dispatch + server 同时消费。方案明确。 — 已修复（抽 internal/usermsg.ForSendError(err, key) 单一入口；server/errors_usermsg.go 退化为 wrapper；dispatch 保留 ErrNoOutputTimeout/ErrTotalTimeout 两条带配置时长的 IM 专属分支 + ErrSessionReset 早返回；contract test 改测 helper 行为），本批 PR #166
- [ ] **R226-CR-10 — `cron/scheduler.executeOpt` 320 行 7 失败分支（P2）**: 抽 `handleGetOrCreateError` / `handleSendError` / `deliverResult` helpers。`internal/cron/scheduler.go:1749-2070`。
- [ ] **R226-CR-11 — `cron/scheduler.go` 2739 行单文件无拆分计划（P2）**: 拟 `scheduler_job.go`/`scheduler_run.go`/`scheduler_notify.go` 拆分；先写 split intent 文档，分阶段做。
- [ ] **R226-CR-12 — `wshub.go` 1785 行 + `feishu.go` 1461 行无拆分计划（P3）**: wshub 至少抽 `wshub_events.go`；feishu 抽 `feishu_token.go` + `feishu_transport.go`。
- [x] **R226-CR-13 — `Snapshot()` 在 read-only 路径里写 `s.SetModel(liveModel)`（P3）**: ListSessions 池化 + side-effect 矛盾。方案：把 model 镜像写迁到 storeProcess 或 post-result hook。`internal/session/managed.go:957`。 — 已修复（保留 mirror write 同时补充 godoc 明确意图，避免未来 SnapshotReadOnly 复制路径丢副作用），本批 PR #151
- [ ] **R226-CR-14 — `session/testutil.go` 测试设施无 build constraint（P3）**: TestProcess/NewTestProcess 编进 prod binary。方案：`//go:build testing` 或迁 `testutil_test.go`（需先验证外部 test 依赖）。
- [x] **R226-CR-15 — `router_lifecycle.ResetChat` fast/fallback 双 path 共享 teardown 代码（P3）**: 抽 `resetKeyLocked(key, *ManagedSession)` helper。 — 已修复（抽 resetSessionLocked(key, &toClose, &closedActive) helper, 索引 + fallback 路径共用; 行为不变），本批 PR #153
- [x] **R226-CR-16 — `Scheduler.UpdateJob` Pause 分支没委托给 `pauseJobLocked` / `resumeJobLocked`（P3）**: pause 逻辑双份维护。 — 已修复（实际重复路径在 SetJobPrompt unpause 分支：内联 registerJob + Paused=false 改为委托 resumeJobLocked，统一恢复语义），本批 PR #157

### 架构 — 本轮新发现

- [ ] **R226-ARCH-1 — `server` 包成"上帝包"（P1）**: 92 个 .go + 12 子 handler，承担路由+UI+业务编排+Hub+nodeCache。建议：薄壳 server + 拆 `internal/server/api/{cron,project,scratch,discovery,...}` 子包；Hub/nodeCache 走显式注入。需 RFC。
- [ ] **R226-ARCH-2 — `KeyResolver` 在 main/server/dispatch 三处独立构造（P1）**: 重复 + 无编译期保证同步 agent map。方案：单例 + 应用 wiring 注入，删 dispatcher fallback。
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

- [x] **R225-GO-1 — `cli/process.go sendSlot.canceled` 普通 bool 字段在锁外被 fanoutTurnResult 读（P3）**: `process.go:337` 注释承认是 race detector 会标的"acceptable"——可直接改 `atomic.Bool` 消除注释合理性需求。改动小但触及 sendSlot 公开结构，需评估 test fixture。 — 已修复（atomic.Bool 替换裸 bool，消除 race acceptable 注释依赖），本批 PR #146
- [ ] **R225-GO-2 — `cli.Resolve` retry sleep 的 ctx 取消（P2 R224-GO-1 子分支）**: subagent_link.go:289/327 `time.Sleep(retryInterval)` 在 SIGTERM 期间无法被取消；与 R224-GO-1 同批接 ctx。Breaking：Resolve 签名加 ctx 参数。
- [ ] **R225-GO-3 — `process_event_query.InjectHistory` 裸 `go linker.Resolve(...)` 无 ctx（P2 R224-GO-3 同源第三分支）**: process_event_query.go:61 与 router_shim.go reconnect 同型；`wallclock` 取的是 `e.Time` 是对的，但 SIGTERM 期间 goroutine 仍不可中止。同 R225-GO-2 一并接 ctx。
- [ ] **R225-GO-4 — `Router.Remove` / `dropEventLogForKey` 用 `context.Background()` + 独立 timeout 而非传入 ctx（P2）**: router_cleanup.go:97 `Remove` 路径上 7s 内不可被 SIGTERM 取消，shutdown tail latency 加重。Breaking：Remove 签名加 ctx。
- [x] **R225-GO-5 — `cron.SetOnExecute / SetOnRunStarted / SetOnRunEnded` 裸 callback 字段（P2）**: scheduler.go:312 三 setter 在 s.mu 下写、`emitRun*` 在 RUnlock 后调，外部测试若直接读字段触发 race。方案：`atomic.Pointer[OnRunStartedFunc]`，与 `Router.onChange` 模式对齐。 — 已修复，本批 PR #191
- [x] **R225-GO-6 — `cli.Process pongRecv` 容量 1 在 GC pause 下可能误杀健康进程（P3）**: process.go:228 调度延迟时多个 pong 被丢，misses 误累。方案：容量提到 maxMisses+1，或心跳消费处用 drain 循环。 — 已修复，本批 PR #156
- [~] **R225-GO-7 — `cli.Process.Kill` 在持 shimWMu 下调 closeShimConn，与 Detach 顺序不一致（误报关闭 2026-05-20）**: 复核 process.go:519-535 (Kill) 与 :617-634 (Detach) 后两者均为 `shimWMu.Lock → SetWriteDeadline → shimSendLocked → closeShimConn → Unlock` 同一模式；两个函数都在持 shimWMu 期间调 closeShimConn，并非"Detach 是 Unlock 后 close"。closeShimConn 走 sync.Once 守护的 net.Conn.Close（R219-GO-3 落地），最坏延迟仅一次系统调用，与 SetWriteDeadline+shimSendLocked 同量级。R227-GO-8 同根因一并关闭。本批 PR #169
- [x] **R225-GO-8 — `cli.spawnSession` 历史加载 ctx 用 `context.Background()` 不继承 ctx（P3）**: router_lifecycle.go:678 resume + `claudeDir != ""` + 无 oldHistory 时 15s 独立 ctx；调用方 ctx 已被取消时仍会等满。方案：`context.WithTimeout(ctx, 15s)`。 — 已修复（context.WithTimeout(ctx, 15s) 派生调用方 ctx；注释补充 r.historyCtx 仍不能用的 Shutdown 路径分离原因），本批 PR #157
- [x] **R225-GO-9 — `Process.SessionID` 公开字段缺 mutex 注释（P3）**: process.go 字段定义无 `// Protected by mu` 注释，外部调用方可能绕过 GetSessionID 直接读。方案：补字段 godoc。 — 已修复（字段 godoc 补 mutex 契约），本批 PR #146

### 安全 — 本轮新发现

- [ ] **R225-SEC-1 — `cli.Wrapper.BuildArgs ExtraArgs` 缺 flag 允许列表（P1 R219-SEC-1 重申）**: 认证 dashboard 用户可以通过 ExtraArgs 注入 `--mcp-config`/`--add-dir`/`--skip-permissions` 等改变 CLI 行为的参数。capExtraArgsBytes 仅查字节长度。方案：维护 flag denylist（或 allowlist），在 BuildArgs 之前过滤；Breaking：依赖任意 ExtraArgs 的 ops 需迁移。
- [ ] **R225-SEC-2 — `shim.moveToShimsCgroup` 不验 CLIPID 是否真的是 shim 子进程（P1 R219-SEC-5 重申）**: handle.Hello.CLIPID 来自 shim 自报，naozhi 直接 sudo busctl 把任意 PID 移入 cgroup（可能是 sshd / pid=1）。方案：读 `/proc/<CLIPID>/status` 验 PPid == cmd.Process.Pid。
- [x] **R225-SEC-3 — `selfupdate.Replace` 固定 `.staging/.bak` 路径多用户竞争（P2 R224-SEC-1 第③项）**: 固定路径在多用户共享 install dir 下可被另一 UID 抢先创建。方案：`os.CreateTemp(filepath.Dir(installPath), ".naozhi-upgrade-*.staging")` rename 到位。 — 已修复（CreateTemp 随机后缀 + 测试改 Glob），本批 PR #158
- [ ] **R225-SEC-4 — `selfupdate.checksums.txt` 未做 GPG/cosign 签名验证（P3 R224-SEC-1 第④项长期）**: GitHub Releases CDN 同时被妥协时 hash 与 binary 都可被换。方案：release 流程对 checksums.txt cosign 签名 + verifyChecksum 前置签名校验。
- [x] **R225-SEC-5 — `dashboard project_files preview` 允许 `.env` 走 text/plain 预览（P3）**: previewableByExt 把 `.env` 映为 text/plain；认证用户可 `?path=.env&mode=preview` 预览敏感配置。方案：servePreview 拒高风险文件名（`.env`/`*.key`/`*.pem`），或在文档明示这是预期。 — 已修复（previewableByExt 移除 .env 条目；落到 DetectContentType → application/octet-stream 被 servePreview MIME 守卫拒；新增 TestPreviewableByExt_DoesNotIncludeDotEnv 锁契约），本批 PR #150
- [x] **R225-SEC-6 — `EffectivePlannerPrompt` 的 byte 循环不调 `osutil.IsLogInjectionRune`（P3）**: project/manager.go:344 全局默认 prompt 路径只过滤 C0 字节，bidi override 字符可绕过。方案：在过滤循环之后加 rune 扫描调 IsLogInjectionRune（与 ValidateConfig 对齐）。 — 已修复，本批 PR #158

### 性能 — 本轮新发现

- [ ] **R225-PERF-1 — `Protocol.ReadEvent(line string)` 接口签名导致 `[]byte(line)` 强制堆 copy（P1 R67-PERF-1 ACP 分支补）**: protocol_claude.go:174 / protocol_acp.go:296 都 `json.Unmarshal([]byte(line), ...)`；最热路径每事件一次 alloc。方案：接口签名改 `ReadEvent(line []byte)`，readLoop 把 trimmed 直接传入。Breaking：是（接口）。
- [ ] **R225-PERF-2 — `process_readloop` `system/task_started` 无背压裸 `go linker.Resolve`（P1）**: process_readloop.go:393 多 sub-agent 并发启动时短时间产生大量 goroutine。方案：用 buffered channel 信号量 / 工作池限并发；与 R224-GO-1 信号量改造统筹。
- [x] **R225-PERF-3 — `subagent_transcript.readLocked` `io.ReadAll(f)` 无上限（P1）**: subagent_transcript.go:81 长 session transcript 几十 MB；agent_tailer 轮询会反复 alloc。方案：`io.LimitReader` + 利用 offset seek 仅读增量。 — 已修复（R227-CR-4 落地：subagent_transcript.go:86 `maxTranscriptReadBytes = 16 * 1024 * 1024` + `io.LimitReader(f, ...)`，offset seek 已在 readLocked 顶端通过 `r.offset` / `r.tail` 保证仅读增量），本批 PR #192
- [ ] **R225-PERF-4 — `applyMetadata meteringUsage merge` 用 slice O(n×m)（P2）**: process.go:717-745 meteringMu 锁内字符串 Unit 等比；MeteringUsage() 每读一次 make+copy。方案：`map[string]*MeteringEntry` 内部存储；Snapshot 路径缓存空 case。
- [x] **R225-PERF-5 — `EventEntriesFromEvent` 双 wall clock 调用（P2）**: process_event_format.go:38 公开 API 不带 At 后缀的版本会再读一次 time.Now；统一走 EventEntriesFromEventAt 为内部唯一路径，公开版改文档化"测试专用"。 — 已修复，本批 PR #156
- [ ] **R225-PERF-6 — `Snapshot SubagentInfo` slice copy（P2）**: managed.go TurnAgents 即便 turnAgentCount 已快速短路，Snapshot 中其他分配仍存在；评估 SubagentInfo slice sync.Pool。
- [ ] **R225-PERF-7 — `protocol_acp.readUntilResponse` 每次握手起独立 goroutine + channel（P2）**: protocol_acp.go:677 改预先 SetReadDeadline 让 ReadLine 自然超时返回，省掉 goroutine + channel + pulse；含 R224-GO-2 同位修改。
- [ ] **R225-PERF-8 — `shimWriter.Write` fast path `string(data[:len-1])` 强制 byte→string copy（P2）**: process_shim_io.go:54 每条 stdin 一次必要 alloc。方案：`shimClientMsg.Line` 改 `[]byte` + 自定义 Marshaler，或在已知 data 不被改时用 `unsafe.String`。
- [ ] **R225-PERF-9 — `wshub.eventPushLoop` 同一 session 多 WS 各自 marshal（P2）**: wshub.go:1028 50 个标签页同 session 时同批事件 marshal 50 次。方案：Hub 层一次 marshal fan-out 同一 immutable []byte 引用。
- [ ] **R225-PERF-10 — `marshalPooled` 每次 copy 一份独立 backing（P2）**: dashboard.go:83 高频 broadcast 下不可避免；考虑对固定组合 session_state 做 LRU 缓存。
- [x] **R225-PERF-11 — `eventlog.Append` UUID 生成在锁内（P2）**: eventlog.go:596 Append 持 mu.Lock 期间调 stampUUID → crypto/rand 系统调用。方案：在锁外预先生成（Append 单条 + AppendBatch 循环前 in-place 全部预 stamp，caller-set UUID 保留）。 — 已修复，本批 PR #158
- [x] **R225-PERF-12 — `agent_message_chunk` `p.mu.Lock` 复用保护 textBuf（P2）**: protocol_acp.go:494 高频 chunk 与 sessionID 共享一锁；evaluate 把 textBuf 移入 readLoop 私有或独立细粒度 mu。 — 已修复（R226-PERF-11 落地：sessionID 已切到独立 atomic.Pointer[string]（protocol_acp.go:103-112），mu 当前只守 textBuf 一条字段；agent_message_chunk 与 WriteMessage / WriteInterrupt / readLoop sessionID 读不再争抢同一把锁），本批 PR #192
- [x] **R225-PERF-13 — `subagent_link.SetAgentInternalID` 持 wlock 做 O(500) 回扫（P3）**: eventlog.go:553 短时阻塞所有 Append。方案：early-exit + 限制最大回扫深度（≤50）。 — 已修复（setAgentInternalIDMaxScan = 50 上限 + foundAgent/foundTaskStart 标记两条都 backfill 后 break），本批 PR #157
- [ ] **R225-PERF-14 — `wsclient.sweepSubGenExpiredLocked` 在 hub 写锁下扫 map（P3）**: wsclient.go:143 阻塞 subscribe/unsubscribe 并发。方案：移到 client 自身轻量 mutex。
- [x] **R225-PERF-15 — `process_send.buildUserEntry` thumbnail goroutine 无并发上限（P3）**: 最多 20 个 goroutine CPU-bound JPEG encode 同时启动；建议限并发 4。 — 已修复（capacity=4 信号量 + 9 行 diff），本批 PR #153
- [x] **R225-PERF-16 — `eventlog.EntriesSince/Entries/LastN` defer Unlock 在热路径（P3）**: 高频 broadcast 下显式 Unlock 微优。 — 已修复（三个读取函数改显式 RUnlock；函数体内无非 Unlock 清理，语义不变），本批 PR #157
- [~] **R225-PERF-17 — `TruncateRunes(string, ...)` 无字节快检（P3，误报关闭 2026-05-20）**: reviewer 提议 `len(s) <= maxRunes*4` 短路其实方向反了——UTF-8 每 rune 1-4 字节意味着 byte 长度 ≤ rune 数*4 是 rune 数的**上界**而非下界，全 ASCII `len=200, maxRunes=50` 时 byte=200 ≤ 200 但 runes=200 > 50，加这种快检会漏截。当前 `len(s) <= maxRunes` 快检已与 `TruncateRunesBytes` 一致，无可优化空间。
- [x] **R225-PERF-18 — `indexAdd` map[string][]string 线性扫描去重（P3）**: router_core.go:496 改 `map[string]map[string]struct{}` O(1) 去重。 — 已修复（field 改 `map[string]map[string]struct{}` + indexAdd/indexDel 改 set 操作 + ResetChat 消费方改 `for key := range set` + init 同步；5 改动点 2 文件），本批 PR #164

### 代码质量 — 本轮新发现

- [ ] **R225-CR-1 — `cli/detect.knownBackends` 与 `backend.Profile` 注册表双轨（P1）**: detect.go:43-46 静态硬编码 Protocol 字符串；与 `Profile.Capabilities().StreamJSON` 不同步会导致 dashboard 误判。方案：`DetectBackendsCtx` 从 `backend.All()` 派生，删 knownBackends。Breaking：否。
- [ ] **R225-CR-2 — `cli.detectCLI` 仍硬编码 `if backend == "kiro"`（P1）**: wrapper.go:157 与 `Profile.DefaultBinary` 重复。方案：detectCLI 调 `backend.Get(backend).DefaultBinary`，删除内联 switch。Breaking：否。
- [x] **R225-CR-3 — `Caps.SoftInterrupt=true` 与 `WriteInterrupt` 返回 `ErrInterruptUnsupported` 语义矛盾（P1）**: protocol_acp.go:288-291 / protocol.go:86 doc string 与代码冲突；R224-ARCH-7 同源。方案：对齐语义——pre-handshake 返回 false 而非 error，或 SoftInterrupt 字段语义改为"握手完成后支持"。 — 已修复（采用方案二仅 godoc：Caps.SoftInterrupt 注释精确化"握手完成后支持"+点名 pre-handshake 仍可返回 ErrInterruptUnsupported；ACPProtocol.Capabilities() godoc 同步说明），本批 PR #166
- [x] **R225-CR-4 — `costUnitForBackend` 硬编码 switch 与 `backend.Profile` 无编译期约束（P2）**: managed.go:1400-1408 注释明示需要双改。方案：`backend.Profile.CostUnit` 字段，profile_claude/profile_kiro 各自填，session 调 `backend.MustGet(id).CostUnit`。R224-ARCH-1 同源。 — 已修复（backend.Profile 加 CostUnit 字段；profile_claude="USD" / profile_kiro="credits"；session.costUnitForBackend 改用 backend.Get 查表，empty backend 兜底"claude"，未注册 ID 返 ""；调 EnsureDefaults 让测试上下文 bootstrap），本批 PR #168
- [ ] **R225-CR-5 — `backendDisplayName / normalizeBackendID` 与 `backend.Profile.DisplayName/ID` 重复（P2 R224-ARCH-1 同源）**: cli/wrapper.go:75-95；新加 backend 改四处。方案：合并到 `backend.Get` 的 DisplayName/ID 字段。
- [x] **R225-CR-6 — `protocol_acp.go:585` TODO 锚点拼写不存在（P3）**: TODO 引 `R222-OBS-MULTIBACKEND-CODE`，TODO.md 实际是 `R222-OBS-MULTIBACKEND-LEGACY`。方案：补条目或更名注释。 — 已修复（注释从 R222-OBS-MULTIBACKEND-CODE 更名为 R222-OBS-MULTIBACKEND-LEGACY 与 R224-CR-4 引入的真实锚点对齐），本批 PR #150
- [x] **R225-CR-7 — `BackendInfo.Features` 在 DetectBackendsCtx 不填、由 dashboard handler 二次注入（P3）**: detect.go：未来调用方直接读会拿到 nil map。方案：godoc 显式标注"dashboard-only 字段"或 DetectBackendsCtx 内填。 — 已修复，本批 PR #156
- [x] **R225-CR-8 — `Snapshot.Model` 注释未提 ACP 路径填空 / spawn-time 值（P3 与 R225-ACP-MODEL-INIT 关联）**: managed.go:850 注释暗示总是准确，但 ACP `session/new` 返回的 currentModelId 当前未回填。方案：注释补"ACP 在 Init 阶段才得到真实 model"。 — 已修复，本批 PR #156
- [x] **R225-CR-9 — `normalizeBackendID` default 分支不验注册 ID（P3）**: 未注册 backend 字符串能直通到 wrapper.BackendID，错误延后到 spawn 期。方案：default 分支调 `backend.Get` 失败 slog.Warn 或 NewWrapper fail-fast。 — 已修复，本批 PR #156
- [x] **R225-CR-10 — `cli.Resolve` 路径长字符串（ev.Description）被 closure 长时间持有（P3）**: process_readloop.go:393 max 8 个 Resolve goroutine 并发持有数 KB 字符串到 sem 释放。方案：调用前 `textutil.TruncateRunes(desc, 2000)`。 — 已修复（readLoop + InjectHistory 调用前 truncate 至 2000 runes），本批 PR #151
- [x] **R225-CR-11 — `backend/profile.reset` 注释声称外部测试反射调用但 unexported 反射不可达（P3）**: profile.go:239 staticcheck U1000 未用；`cron.freshContextPreflight` 同款。方案：移到 *_test.go + 改 `resetForTest` 或 `//go:build testing`。 — 已修复（reset 在源码 + _test.go 内 0 调用者，测试已通过 withCleanRegistry helper 完整覆盖；删函数 + 同步 defaultsOnce 注释指向 withCleanRegistry。cron.freshContextPreflight 是另一根因，留作单独条目），本批 PR #150
- [x] **R225-CR-12 — `spawnSession` 函数注释承诺"持锁"但内部多次 Unlock/Lock（P2）**: router_lifecycle.go:506 让未来维护者误加嵌套调用导致死锁。方案：注释精确化 + LOCK: 批注，或重构锁生命周期由调用方负责。 — 已确认归档（router_lifecycle.go:527-531 现有 godoc 已显式标 `LOCK: enter with r.mu held. This function releases and re-acquires r.mu internally (around Spawn() and history collection) to avoid blocking other goroutines during slow protocol init (e.g. ACP handshake). Callers MUST NOT hold any other lock when invoking; the defer reacquires r.mu only.` —— 注释已精确化锁生命周期 + 调用方约束，无需额外改动），本批 PR #183
- [x] **R225-CR-13 — `readLoop` passthrough 无主 result fall-through 触发 Warn 日志（P2）**: process_readloop.go:306 `eventCh full, dropped result` 在正常路径会误报。方案：fall-through 前加 `if !p.caps.Replay` 守卫或日志降到 Debug。 — 已修复（dispatchProtocolEvent result drop 分支按 caps.Replay 分流：Replay backend 走 Debug + 增加路径说明注释；非 Replay backend 仍 Warn 保留原有可观测信号），本批 PR

### 协议正确性 — 本轮新发现（与 fix/claude-model-from-init-v2 分支强相关）

- [ ] **R225-ACP-MODEL-INIT — ACP `session/new` 返回 `result.models.currentModelId` 未回填 `Process.model`（P2）**: protocol_acp.go:168-174 解析 session/new result 时未读 currentModelId。`Snapshot.Model` 永远显示 spawn 配置值（或空）而非 kiro 实际使用的 model。方案：Init 返回值或回调把 currentModelId 传给 setModel。Breaking：否（增量字段）。

### 架构 — 本轮新发现

- [ ] **R225-ARCH-1 — `EventEntry / ImageData / EventCallback / SendResult` 跨层 DTO 塌陷（P1）**: node.ServerMsg.Events 钉住 cli 内部领域类型作为反向连接 wire 协议；server 26+ 包 import internal/cli。方案：抽叶子包 `internal/event`（零依赖），cli/server/node/dispatch 都 import 它；或 dispatch 加 DTO 转换层。Breaking：是。



> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 100 条发现。
> 11 处 FIX-READY 已落地本轮 PR（selfupdate constant-time compare + checksums.txt 64KB 上限独立 / feishu maxWebhookTokenLen 512 守卫 / protocol_acp ExtraArgs capExtraArgsBytes 对齐 claude / protocol_acp readUntilResponse 错误 %w 包装区分 EOF vs read err / subagent_link entries[:0:0] 替为 make 显式 / gzipMiddleware response-only godoc / gzip Flush godoc 修正过时 "no handler uses Flusher" / dashboard.marshalPooled 安全契约交叉引用 writeJSON CLIENT-SIDE CONTRACT / router.kiroSessionsDir Sprint 1a unwired 注释更新为 "wired in main.go" / backend/profile.go Sprint 0b/1b 路线图描述去除）。
> 以下是需设计决策、破坏兼容、跨包重构、或方案不唯一不适合本轮直接修的条目。

### Go 正确性 — 本轮新发现

- [ ] **R224-GO-1 — `cli.Resolve` 信号量 acquire 处的 Timer 资源 leak（P1 R219-GO-1 未覆盖分支）**: `subagent_link.go:263-270` Timer Stop 后未 drain `t.C`；进程 SIGTERM 时所有等 sem 的 goroutine 永久阻塞，因 select 无 ctx.Done() arm。需与 R219-GO-1 同批修复，扩大覆盖到 Timer 路径 + 两处 retry sleep（`:289` + `:327`，code-reviewer 指出）。Breaking：是（Resolve 接受 ctx）。
- [ ] **R224-GO-2 — `protocol_acp.readUntilResponse` 非 shim path goroutine 在 timeout 后仍阻塞 ReadLine（P1）**: `protocol_acp.go:635-673` 仅 shimLineReader 路径走 SetReadDeadline；非 shim path 在 ACP 握手超时后 goroutine 卡 ReadLine 直到管道 EOF。每次握手超时泄漏一个 goroutine。方案：非 shim path 也走 deadline-aware reader 或 timeout case 直接 close 底层 conn。涉及：`internal/cli/protocol_acp.go:635`。
- [ ] **R224-GO-3 — `reconnectShims` replay 段把 `time.Now().UnixMilli()` 作为 agentToolUseMS 传入 Resolve（P1）**: `router.go:1535` reconnect 路径调 `linker.Resolve(taskID, toolUseID, name, desc, time.Now().UnixMilli())`，导致 `subagent_link.go:315` `agentTS - 10s` 时间过滤在 reconnect 路径上 100% 命中所有历史条目。replay 事件应使用事件本身的 recvAt/Time 或传 0 禁用过滤。涉及：`internal/session/router.go:1535`。
- [ ] **R224-GO-4 — `subagent_link.fireOnResolveLocked` mu-release-reacquire 易死锁/panic（P2）**: `subagent_link.go:565` 持 `l.mu` write lock 时 Unlock + 跑 callback + 再 Lock，依赖 callback 不调 `linker.Resolve`（会触发写锁死锁）+ 单一 goroutine 进入此函数（否则第二个 Unlock panic 解锁未持有锁）。方案：先在 call site 拷贝 ID，Unlock 之后再 fire，整体移出锁外。
- [ ] **R224-GO-5 — `eventlog.invokePersistSink` `replay` 标志读取存在 sink Store/sinkReady Store 之间的 race window（P2）**: `eventlog.go:360` 读 `!sinkReady.Load()` 在锁外，`SetPersistSink` 先 Store sink 后 Store sinkReady（line 336-337），中间窗口内一个 entry 会被错误标记 `replay=true`。方案：SetPersistSink 顺序反转，或合并到一个 atomic.Pointer 携带 sink+ready。
- [ ] **R224-GO-6 — `shim/server.go SetReadDeadline` 错误 nolint 静默吞（P2）**: `:654, :680` SetReadDeadline 失败 nolint:errcheck 直接吞；conn 已关闭时后续 ReadBytes 无 deadline 阻塞 goroutine 泄漏；deadline 清除失败时 post-auth 读立即 timeout 踢掉合法客户端。方案：失败时显式关闭 conn 并 return。
- [ ] **R224-GO-7 — `shim/server.go writer goroutine` 内层 `w.Write(more)` 错误 nolint 吞（P2）**: `:785` 写失败后 `:790` 仍调 `flushWithDeadline()`，可能将损坏的 buf 状态 flush 出去。方案：内层 write 失败立即 return。
- [x] **R224-GO-8 — `shim/server.go resetIdleTimer` Stop() 后未 drain `idleTimer.C`（P3）**: `:457` Go &lt;1.23 toolchain 上 idle event 残留导致 reset 后立即触发空闲关闭。方案：标准 Stop+drain 模式 `if !Stop() { select { case <-C: default: } }`。 — 已修复（Stop() 返回 false 时非阻塞 drain；Go 1.26 toolchain 已自动处理，保留 drain 是 belt-and-suspenders 防御未来调用方缓存 channel），本批 PR #164

### 安全 — 本轮新发现

- [ ] **R224-SEC-1 — `selfupdate` 临时文件 0755 + 固定 staging/backup 路径，存在 fetch→verify TOCTOU + 多用户竞争（P1）**: `selfupdate.go:203` `os.OpenFile(dest, ..., 0755)` 写 binary 到 `/tmp/...` 在 verifyChecksum 之前可执行权限可写；`Replace` 用固定 `installPath + .staging/.bak` 路径，多用户 install dir 下可被其他 UID 抢先创建。方案：① 临时文件 0600 写入，verify 后 chmod；② `os.CreateTemp` 替换固定 staging path；③ verify 路径直接用已打开 fd 而非重新路径查找；④ 长期加 GPG/cosign 签名校验 checksums.txt（checksums.txt 自身未签名，CDN/release 同时被改 hash 匹配仍不可信）。涉及：`internal/selfupdate/selfupdate.go:119-203`。
- [x] **R224-SEC-2 — `verifySignature` 在 `encryptKey == ""` 直接返回 true（P2）**: `feishu.go:1263` 函数语义"空 key → 签名通过"是隐性危险：任何未来调用者忘记外围 `if EncryptKey != ""` 门控等同于跳过签名检查。方案：移除内部 return true 提前返回，调用方在外围 if 块内调并检查结果。 — 已修复（fail-closed：空 key 返回 false；唯一调用点 transport_hook.go 已外围 if 门控；feishu_test 用例 "empty encrypt key bypasses" 改为 "fails closed" 锁定新契约），本批 PR #173
- [ ] **R224-SEC-3 — `EffectivePlannerPrompt` 放行 LF/CR 与 ValidateConfig 拒绝 LF/CR 策略不一致（P2）**: `project/manager.go:357` 注释明放行 `0x0a/0x0d` 让 markdown CLAUDE.md 风格 prompt 通过；但 LF 在 stream-json NDJSON 帧中是行分隔符，注入可破坏协议帧。直接对齐拒绝会破坏现有 multi-line prompt 配置；需设计：是否把 multi-line CLAUDE.md 风格 prompt 经预处理（把 LF 替换为 ` ` 或 `\\n` literal）后才进入 argv，或拒绝 multi-line prompt 强制 operator 改用 CLAUDE.md 文件路径。
- [ ] **R224-SEC-4 — `defaults.Prompt` 全局默认 prompt 不经 project.ValidateConfig（P2）**: `manager.go:344` 全局默认通过 `validateArgvStrings` 仅查 C0 字节，但 `project.ValidateConfig` 还查 IsLogInjectionRune（C1/bidi/LS-PS）。bidi 字符可绕过到 `--append-system-prompt` argv。方案：对 `cfg.Projects.PlannerDefaults.Prompt` 同样调 `project.ValidateConfig`。
- [ ] **R224-SEC-5 — `WriteTimeout: 60s` 对大文件流式响应不足（P2）**: `server.go:870` 全局 60s 写超时；50MB raw 文件下载在慢网络可被截断。WS 走 Hijack 不受影响，但 preview/raw/download 受影响。方案：对文件路由用 `http.ResponseController.SetWriteDeadline` 扩展或抬升全局并对其他路由 per-handler 收紧。
- [ ] **R224-SEC-6 — `dashboard_auth.go` HTTP 部署仍创建 non-Secure cookie（P3）**: `dashboard_auth.go:300` `Secure: a.isSecure(r)` 在 HTTP 下创建 non-Secure cookie；启动 warn 但 cookie 仍设。考虑公网 HTTP 部署下拒绝 cookie 创建（breaking）或加强 warn 频率。

### 性能 — 本轮新发现 / 重申

- [x] **R224-PERF-1 — `eventlog.fireTaskDoneCallbacks([]pendingTaskDone{pending})` 单元素 slice literal escape（P2 R219-PERF-4 / R222-PERF-8 未覆盖分支）**: `eventlog.go:654` 与 `invokePersistSink` 单元素 slice 同款 escape，每个 task_done 事件一次额外 alloc。方案：`var buf [1]pendingTaskDone; buf[0] = pending; fire(buf[:])` 栈数组，或 `sync.Pool[[]T]`。 — 已修复（新增 fireOneTaskDoneCallback 单参数 fast path，Append firePending 分支调它，绕开 1 元素 slice literal heap escape；AppendBatch 仍走 slice 形式，因为它真累积多条），本批 PR #173
- [ ] **R224-PERF-2 — `handleList workspaces []string + wsMap map[string]string` 1 Hz × N tab 重复分配（P2 R219-PERF-2 未覆盖 inner alloc）**: `dashboard_session.go:394` storeGen 命中 in 顶层缓存之前已分配 workspaces slice + wsMap。方案：workspaces 走 sync.Pool（参考 listRefsPool R222-PERF-10 先例）；ResolveWorkspaces 接受 caller 复用 map 或缓存 wsMap 与 storeGen 绑定。
- [ ] **R224-PERF-3 — `Snapshot()` `TurnAgents()` RLock + slice copy 在 sub-agent turn 期间频繁触发（P3 R219-PERF-3 / R222-PERF-7 未覆盖）**: `managed.go:894` 当 turnAgentCount > 0 时仍进 RLock + copy；500 Snapshot/s × sub-agent turn 与 SetAgentInternalID WLock 竞争。方案：`TurnAgents()` 改 atomic.Pointer[[]Agent] copy-on-write，applyEntryStateLocked 修改时 atomic.Store 新 slice。
- [x] **R224-PERF-4 — ETag/Content-Disposition 路径上的 `fmt.Sprintf` 反射 overhead（P3）**: `dashboard_send.go:1112`/`project_files.go:592, 324, 337` 多处用 fmt.Sprintf 构造 header；ETag seed 可改 `strconv.AppendInt + 栈数组`，Content-Disposition 改 strings.Builder。 — 部分修复（ETag seed 两处都改 strconv.AppendInt 写 48B 栈缓冲，Content-Disposition 的 fmt.Sprintf 暂缓），本批 PR #160
- [x] **R224-PERF-5 — 错误信息 fmt.Sprintf 含编译期常量（P3）**: `wshub.go:791` "too many files (max %d)" 等错误 path 上的 fmt.Sprintf 完全可预先 const。方案：包级 const string 直接引用。 — 已修复（抽 `errTooManyFiles` 包级 const + 3 处调用点切换 + 编译期断言锁定字面量与 maxFilesPerSend 同步），本批 PR #141
- [ ] **R224-PERF-6 — `ACPProtocol.parseSessionUpdate` 嵌套三次 json.Unmarshal（P2）**: `protocol_acp.go:474, 476` 相比 stream-json 双解码再多一次 update.Update.Content 嵌套解码。方案：声明 Content 为 `json.RawMessage` 推迟第三次解码，或与 R222-PERF-1 / R222-PERF-3 同批做 ReadEvent([]byte) 接口改造。

### 代码质量 — 本轮新发现

- [x] **R224-CR-1 — `R220-SEC-3` (gzipMiddleware 解压请求体 → 跳过 MaxBytesReader) 是误报，应关闭（P1）**: gzipMiddleware 仅包响应 ResponseWriter，从不读 r.Body 也不解压；MaxBytesReader 与之独立。该 TODO 描述的漏洞不存在，应在 TODO.md 关闭并把 R217-SEC-3 同源条目一并关闭。本批 PR 已加 godoc 注释 "response-side only" 防止再误读。 — 已关闭（gzip.go:139-147 godoc 已明示 response-side only；同步关闭 R220-SEC-3 / R217-SEC-3），本批 PR #150
- [x] **R224-CR-2 — `R218B-GO-2` (panic handler 用 Background ctx 会挂起) 是误报，应关闭（P2）**: `dispatch.go:518` 已是 `context.WithTimeout(context.Background(), 15s)`，无法挂起；该函数必须有意从 parent ctx detach（parent 在 shutdown 已 cancel），才能给用户回 panic 错误。R218B-GO-2 / R217-GO-2 应关闭。 — 已关闭（dispatch.go:518 已锁 15s 上限；同步关闭 R218B-GO-2 / R217-GO-2 同源条目），本批 PR #150
- [x] **R224-CR-3 — `EventEntryFromEvent` Deprecated godoc 写错（P3）**: `process_event_format.go:23` 注释带 `Deprecated` 字面但不是 gopls 识别的 `// Deprecated:` 行格式（缺冒号），strike-through 不会触发。方案：改成标准 `// Deprecated: use EventEntriesFromEvent.`。注：函数仍被 process_extra_test.go 锁定行为，**不能删**，仅修注释。 — 已修复，本批 PR #141
- [x] **R224-CR-4 — `metrics/multibackend.go` 4-week 双写迁移 `R222-OBS-MULTIBACKEND-LEGACY` 锚点是死引用（P2）**: 文件头部说 "see TODO marker R222-OBS-MULTIBACKEND-LEGACY" 但 TODO.md 中无此锚点。方案：补上具体条目（列出 legacy expvar.Int 名单 + 移除前提"ops dashboards 全部迁到 labeled counter 后 4 周")。 — 已修复（multibackend.go 文件头注释改为自描述：列出 2 个 legacy mirror（CLISpawnTotal / SessionActive）+ 2 条移除前提（dashboards 全迁 + ≥4 周 production soak），跟踪锚点改为 R-METRICS-LEGACY-EXPVAR），本批 PR #141
- [x] **R224-CR-5 — `server.New` deprecated wrapper 无移除条件（P2）**: `server.go:457` Deprecated 自 Round 214 起停摆，0 生产调用，~20 test call sites 阻塞。方案：① Deprecated godoc 加显式移除条件 "remove once all *_test.go migrated"；② 或关闭 R214-CODE-4 标 "won't fix — low-value churn"。 — 已修复（采用方案 ①：Deprecated 块下追加 "Removal condition" 段，明确 `git grep -l "server.New("` 应为零才能删；同时禁止新增 positional-style 调用点。R214-CODE-4 同根因留作 *_test.go 迁移驱动），本批 PR #187
- [~] **R224-CR-6 — `process_turn.go` 文件级 ownership 注释列错位（误报关闭 2026-05-20）**: 条目自身已记「无操作，当作澄清」——`isChanAlive` 在同文件第 188-193 行确实由本文件拥有，文档与代码自洽，是 reviewer 自己的误报。本批 PR #168 关闭归档。
- [x] **R224-CR-7 — Sprint-numbered 注释批量过期（P3）**: `dispatch.go:710` "Sprint 2"、`select_node_for_backend.go:3` "Sprint 6b"、`dashboard_cron.go:354` "Sprint 6c"、`profile_kiro.go:14` "Sprint 1b reverse-node routing"（已落地于 upstream/caps.go）等多处把 Sprint 编号当时间锚点，长期看变误导。方案：扫一遍把 Sprint X 替换为功能描述或 PR 锚点。 — 已修复（dispatch.go / select_node_for_backend.go / dashboard_cron.go × 3 / profile_kiro.go × 2 共 6 处 Sprint 编号替换为功能描述或 RFC §章节锚点），本批 PR #141
- [x] **R224-CR-8 — `sanitizeStderrLine` 缺独立 unit test（P3）**: `process_turn.go` 安全敏感（log injection 防御）但仅有 readLoop 端到端覆盖。方案：补 table-driven test（normal ASCII / ANSI escape / C0 / 超长截断）。 — 已修复（process_turn_test.go 覆盖 ASCII/CSI/OSC/C0/C1/bidi/LS-PS/CJK/emoji passthrough/截断标记/UTF-8 边界），本批 PR #160
- [ ] **R224-CR-9 — `dispatch.replyTracker` 240 行 5+ 职责（P2 god object 苗头）**: edit-banner 速率限制 / todo 去重投递 / askQuestion 卡片回送 / initial-Reply lifecycle / loopWG 协调 5 类正交职责挤一对象 + 7 同步原语。方案：拆 BannerEditor / TodoStreamer / AskQuestionPoster / TurnLifecycle 4 对象，replyTracker 退化 wiring 容器。

### 架构 — 本轮新发现 / 重申

- [ ] **R224-ARCH-1 — `backend.Profile` registry 未真正用满，DisplayName/CanonicalID/CostUnit 仍三处独立 switch（P1）**: `wrapper.go:75-83`/`session.costUnitForBackend`/`cli/wrapper.go normalizeBackendID` 各自硬编码；新加 backend 必须改三处。方案：DisplayName/CanonicalID/CostUnit/DefaultCwdInheritance 全收敛 backend.Profile，cli + session 改 `backend.MustGet(id).XXX`。
- [ ] **R224-ARCH-2 — `dispatch` 直 import `internal/cron` + 持具体 `cron.Job` 类型（P1）**: `dispatch.go:16,49,121,344` + `commands.go:13,344` 把 cron 限制常量 + 增删改全绑死单一 cron 实现。方案：定义 `dispatch.CronGateway` interface，server 注入实现，dispatch 仅依赖接口；cron 限制常量改 `cron.Limits{}` 通过 gateway 暴露。
- [ ] **R224-ARCH-3 — `ManagedSession.SubagentLinker() *cli.SubagentLinker` / `AgentEventLog() *cli.EventLog` getter 暴露 cli 包内部具体类型（P1 R219-ARCH-3 未覆盖核心层）**: server / wshub_agent 拿到 cli 内部对象直接调；ACP 等无 SubagentLinker 概念的 backend 整链路硬编码 nil。方案：`session.AgentIntrospector` interface（Linker() AgentLinker / EventLog() AgentEventStream），cli.Process 实现。
- [ ] **R224-ARCH-4 — `cron` 持 `platforms map` 直调 platform.Reply 绕过 dispatch（P1 R219-ARCH-8 重申，强调 dispatch 出站层已存在却被 bypass）**: cron 走出来的 reaction / 节流 / queue ack / split 逻辑都和 IM 路径不一致。方案：抽 `dispatch.OutboundNotifier`，server 注入实现，cron 持 Notifier 而非 platforms map。
- [ ] **R224-ARCH-5 — `processIface` 30+ 方法 god + 多处 `proc.(*cli.Process)` type-assert 偷越层（P1 R215-ARCH-P1-3 加证据）**: 11 处 type assertion 是 god 接口治理失败的具体证据。方案：拆 `ProcessLifecycle` + `EventSource` + `BackendIntrospector` + 可选 `AgentIntrospector`，type-assert 路由由调用方做。
- [ ] **R224-ARCH-6 — `session/router.go:31-34` blank import + 真依赖混用（P1 R219-ARCH-2 未覆盖分支）**: `merged` / `naozhilog` 是真 import 不是 blank。R219-ARCH-2 的工厂方案不充分，必须把 merged + naozhilog 同样登记到 history factory。方案：`cli.RegisterHistoryFactory` 体系扩展或独立 `MergedSource` 工厂；blank import 集中到 cmd/naozhi/wireup.go。
- [ ] **R224-ARCH-7 — `Protocol` 接口默认 `ProtocolCaps` 反向枚举 "非 ACP → StreamJSON=true"（P1）**: `protocol_acp.go:88-108` `StreamJSON: p.Name() != "acp"` —— 新加 backend "gemini" 会被错误归类为 StreamJSON。方案：Protocol 必须实现 `Capabilities() Caps`，删除反向推断 + SupportsX 双轨；P0 先把 default 改 `StreamJSON: false` 更安全。
- [ ] **R224-ARCH-8 — `Server` 24+ 字段 / `Hub` 30+ 字段都已塌陷为新上帝包前夜（P1 R222-ARCH-6 + Server 自身未覆盖）**: scheduler/uploadStore/scratchPool 通过 setter 后注入造成半构造对象（R219-ARCH-5 已登记），新增功能继续往 Server / Hub 加。方案：Server 退化为门面，Hub 降级为 handler group 之一；新增 `internal/server/wireup` 子包做 NewServer。
- [ ] **R224-ARCH-9 — `dispatch.SendFn / TakeoverFn` 函数指针注入仅是反向依赖错觉（P2）**: 函数签名仍含 `*ManagedSession / cli.ImageData / cli.EventCallback`，dispatch 早已知道 cli 包结构。方案：要么 `session.Sender interface { Send(...) (Result, error) }` 彻底切 cli 类型，要么删 Fn 注入直接调 sess.Send。Breaking：是。
- [ ] **R224-ARCH-10 — `cli.Process` 内嵌 shim 协议字段 (`shimConn / shimR / shimW / shimWMu / shimCloseOnce / cliPID / shimPID`) 把 shim 协议焊进通用 Process（P2）**: 未来非 shim backend（in-process Go SDK / WebSocket 远端）无法复用 Process。方案：抽 `cli.Transport` interface（Read/Write/Close/PID），shim 一种实现、in-process 另一种；Process 持 Transport。Breaking：是。
- [ ] **R224-ARCH-11 — `backend.RegisterDefaults / EnsureDefaults` 无显式 wiring 顺序契约（P2）**: replyTagForBackendOnce 兜底自注册 + profile.go 警告 "不要 recover"，两套规则互相冲突。新加 backend 漏在 main 注册会触发 lazy 注册产生不同 Profile 视图。方案：把 EnsureDefaults 挂 backend 包 init()（接受不可逆 side-effect）或 main 启动加 `assertBackendRegistered`。
- [ ] **R224-ARCH-12 — `dispatch.NewDispatcher` 在 `cfg.Resolver==nil` 时 fabricate fallback resolver 双轨（P2）**: production 主 resolver vs test 各处 fallback 行为靠 code review 维持等价；KeyResolver.NewKeyResolver 签名变化时 fallback 容易漏改。方案：dispatch.NewDispatcher 把 Resolver 改必填；test 显式构造 minimal resolver 或 fabricate 上移到 server.New 顶层。
- [ ] **R224-ARCH-13 — `session/scratch.go` ScratchPool 与 managed sessions 共用 router.sessions map（P2）**: 通过 ScratchKeyPrefix 在落盘/sidebar 处过滤；store 路径热扫整 map 受 scratch 拖慢，sweep 路径要小心绕开主 cleanup。方案：scratch 走独立 `scratchSessions map`，主 sessions map 完全不知道 scratch；或抽 `internal/session/scratch` 子包。
- [ ] **R224-ARCH-14 — `dashboard_*.go` 1000-1700 行 god file（P2 R222-CR-2/3 文件级未覆盖）**: dashboard_send.go 1000+ / dashboard_session.go 1000+ / dashboard_cron.go 1700+，并行 PR 物理冲突。方案：复用 router-split-design.md 同款方法论，给 dashboard_send / dashboard_cron 各自起 split-design ADR。
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
- [x] **R222-SEC-2 — `shimEnvAllowedPrefixes` 仍含 PYTHONPATH/PYTHONHOME/CONDA_/NVM_DIR/VIRTUAL_ENV（P2）**: 类似 R220-SEC-1（GIT_）但针对 Python/conda/nvm 生态。`PYTHONPATH=` 让多用户/CI 系统上低权限可写路径里的恶意模块被 Python 子进程优先载入（Bash tool 触发 `python3` 即可）；`VIRTUAL_ENV=` 同理；`CONDA_` 通配过宽（仅需 PREFIX/DEFAULT_ENV/SHLVL 三项）；`NVM_DIR=` 暴露 nvm 树供 PATH-mismatch 攻击。方案：拆显式 allowlist；可能影响依赖 PYTHONPATH/VIRTUAL_ENV 转发的部署，需 release note。涉及：`internal/shim/manager.go:959-961`。 — 已修复（删除 PYTHONPATH/PYTHONHOME/VIRTUAL_ENV/NVM_DIR + CONDA_ 通配；保留 CONDA_PREFIX/DEFAULT_ENV/SHLVL + PYTHONDONTWRITEBYTECODE/PYTHONUNBUFFERED；manager_test.go 同步 6 个新 blocked case 锁拒），本批 PR #99
- [x] **R222-SEC-3 — 缺 `GET /api/cron/runs` 与 `GET /api/cron/runs/{id}` rate limit（P3）**: 已认证 token 被盗后可高频枚举完整 run 历史并触发持续 IO；filesExistsLimiter 模式可复用。方案：在 server.New 织入 cronRunsLimiter（如 60req/min/IP），接到 dashboard_cron handler。涉及：`internal/server/dashboard_cron.go:963,1054`。 — 已修复（CronHandlers.runsLimiter = newIPLimiterWithProxy(rate.Every(time.Second), 60, opts.TrustedProxy) 共享 bucket 防绕过 + handleRunsList/handleRunDetail 双端点限流前置守卫），本批 PR #98
- [ ] **R222-SEC-4 — `nz_anon` cookie 在无 TLS 部署下不带 Secure（P3）**: 启动期已有"未启 TLS"告警，但 cookie 自身不 fail-closed，攻击者同网络可窃取并认领 pending 上传。方案：多用户模式强制 Secure/no-TLS 启动失败。涉及：`internal/server/dashboard_send.go:44-50`。
- [x] **R222-SEC-5 — `handleAttachment` MIME 仅来自扩展名，无 magic byte 校验（P3）**: 当 attachment.sanitizeExt 之外的代码路径被加入（regression），可向 image/jpeg 头里塞非图片字节。方案：`http.DetectContentType` 比对扩展名；不一致则降级为 application/octet-stream + attachment disposition。涉及：`internal/server/dashboard_send.go:1016-1033`。 — 已修复（DetectContentType 对前 512 字节 sniff，与 ext-MIME 不一致时降级 octet-stream + Content-Disposition: attachment + X-Frame-Options: DENY；rewind 后 ServeContent 流原始字节），本批 PR #99
- [x] **R222-SEC-6 — `validateCronPrompt` LF-allowed 注释提到 `--append-system-prompt` 单行约束但策略只对 cron 安全（P3）**: 注释会误导未来把 LF-allowed 复制到 planner_prompt。方案：保留代码不改，加 contract test 锁 `project.ValidateConfig` 拒绝 PlannerPrompt 含 \n。涉及：`internal/server/dashboard_cron.go:184-188`。 — 已修复，本批 PR #96

### Go 正确性 — 跨包改动 / shutdown 协调

- [ ] **R222-GO-1 — `cron.executeOpt` 用 `context.Background()` 起 sendCtx，绕开 stopCtx 取消（P2）**: scheduler.go:1853 注释说为避免 shutdown 误记 cancel，但 Stop 后 Send 仍可阻塞 jobTimeout（最多 5 min），triggerWG.Wait 因此可能超 stopBudget。方案：sendCtx 来自 stopCtx 派生 + 短 grace；或在 wg goroutine 文档化"intentional orphan"。涉及：`internal/cron/scheduler.go:1853`。
- [x] **R222-GO-2 — `discovery/history_tail.go` 非 ctx 包装版用 `context.Background()`（P2）**: `loadHistoryTail`/`loadHistoryChainTail` 私有 wrapper 用 Background()，无法被 router historyCtx 取消，NFS 等慢 FS 上启动可能挂住。方案：调用方迁到 *Ctx 变体；删除非 ctx wrapper。涉及：`internal/discovery/history_tail.go:54,367`。 — 已修复（删除两个 Background 包装，测试改用 LoadHistoryTailCtx/LoadHistoryChainTailCtx 显式传 context.Background()），本批 PR #97
- [ ] **R222-GO-3 — `cli.SubagentLinker.fireOnResolveLocked` 释放重取 mu 让 callback 跑期间存在 nested mu-release race（P2）**: 重入安全契约靠 godoc 维系，无静态守卫；callback 若再调 linker.Query 进入嵌套路径可能死锁。方案：copy fns 后释放两锁外执行所有 callback，移除 re-lock。涉及：`internal/cli/subagent_link.go:556-568`。
- [x] **R222-GO-4 — `heartbeatLoop` pongTimer 预先 Stop+Reset 在 Go<1.23 toolchain 下可能漏 stale tick 误判 pong miss（P2）**: 注释说 Go 1.23+ 自 drain，但需校验 go.mod 最低版。方案：grep go.mod toolchain 行；若已 ≥1.23 则在注释加锁定标记。涉及：`internal/cli/process_readloop.go:506-508`。 — 已修复（go.mod 1.26.3 已 ≥1.23，注释加 LOCKED 标记 + 显式回退指引），本批 PR #96
- [x] **R222-GO-5 — `cron/runstore.go` mtime sort 在 FS 时间精度低时 ordering 不稳，pagination cutoff 可能漏数据（P3）**: 评论已承认 StartedAt 才是真源；FAT32/tmpfs 在并发 cron 完成时同秒丢序。方案：non-cached 全扫路径读 JSON 排 StartedAt；或 mtime 作 secondary key。涉及：`internal/cron/runstore.go:397`。 — 已修复（mtime 平局时按 runID（16-char 随机 hex）desc 兜底排序，跨进程跨重读确定性 secondary key，无额外 IO 开销），本批 PR #99
- [x] **R222-GO-6 — `eventlog/persist/persister.go run()` 关闭路径 drain p.in 但不 drain p.opCh，pending DropKey/Flush 调方挂直至 ctx 兜底（P3）**: 方案：closeCh case 在 shutdownAll 前 drain opCh，对每个 done channel 发 ErrPersisterClosed。涉及：`internal/eventlog/persist/persister.go:483-495`。 — 已修复（closeCh 分支 drain p.in 后再 bounded drain p.opCh，每个 pending op 的 done 通道非阻塞写 ErrPersisterClosed），本批 PR #97
- [x] **R222-GO-7 — `cli/subagent_link.go::readFirstLineMeta` ReadSlice 满 32KB 后将 partial 行丢给 Unmarshal 静默退化为 scan（P3）**: 长 thinking block 当作首行时 fast path 失败但无显式信号，性能默默掉。方案：`bufio.ErrBufferFull` 显式返 errFirstLineTooLong，调用方按 fallback 处理。涉及：`internal/cli/subagent_link.go:666-668`。 — 已修复，本批 PR #95
- [x] **R222-GO-8 — `session/router.go GetOrCreate` waiting timer drain 在 ctx.Done 分支可阻塞达 20ms（P3）**: Stop 返回 false 后 `<-waitT.C` 可能等下一 tick。方案：Reset(0) 强制立即 drain，或换 AfterFunc。涉及：`internal/session/router.go:1934-1936`。 — 已修复（go.mod ≥1.26 单调 Stop 自带 drain，去掉 if !Stop() { <-C } 旧 idiom；注释锁定 toolchain 版本契约），本批 PR #97
- [x] **R222-GO-9 — `cli/process_readloop.go readLoop` panic recover 在 close(p.done) 之前调 onTurnDone，cb 看到 p.done 仍 alive 与"进程已死"语义冲突（P3）**: 方案：recover 设 flag 而非直接调 cb，让正常 terminal 路径跑 cb；或 godoc 显式标注 cb 在 partial 状态执行。涉及：`internal/cli/process_readloop.go:66-79`。 — 已修复（采用 godoc 文档化路径：normal terminal 路径同样 cb 在 close(p.done) 之前，LIFO defer 顺序有意设计；注释明确 callbacks 不应 select p.done 作 teardown 信号，应改读 IsRunning/GetState=StateDead），本批 PR #97
- [x] **R222-GO-10 — `cron.Scheduler.Stop` triggerWG goroutine 在 deadline hit 时泄漏（P3）**: 测试场景下 stopBudget 短可触发 goroutine-leak detector。方案：用内部信号通道串联，或 gate `go func` 在 !deadlineHit。涉及：`internal/cron/scheduler.go:754-765`。 — 已修复（采用方案二"文档化 intentional orphan"：守卫块内联注释明确 wrapper goroutine 由 OS 进程退出回收 + 不加 chan-cancel 反向救赎的两条理由 + 测试侧 plumb 非 stuck deliverNotice fake 指引；纯文档化既有 contract，无行为变化），本批 PR #99

### 性能 — 协议接口或大重构

- [ ] **R222-PERF-1 — `cli.Protocol.ReadEvent(string)` 每 stdout 行做 `[]byte(line)` heap copy（P1，重申 R67-PERF-1）**: ReadEvent 接口签名为 string，内部强制再分配 []byte 给 json.Unmarshal。5-50 evt/s × N session × 50-4KB 持续 alloc。方案：接口改 `ReadEvent([]byte)`，protocol_claude/protocol_acp + readLoop 同步。Breaking：是。
- [ ] **R222-PERF-2 — `shimWriter.Write` 双路径都 `string(data[:len-1])` 拷贝到 shimClientMsg.Line（P1，重申 R71-PERF-H1）**: shimClientMsg.Line 是 string 而非 json.RawMessage；改 RawMessage 可零拷贝。Breaking：shim 协议字段类型变。涉及：`internal/cli/process_shim_io.go:54,83`。
- [ ] **R222-PERF-3 — `readLoop` 每条 stdout 行做两次完整 JSON decode（P1）**: 先 Unmarshal shimMsg（含 `Line string`），再 ReadEvent 内 Unmarshal Event。最坏 2500 double-decode/s。方案：shimMsg.Line 改 json.RawMessage 直接传 ReadEvent；包内部改不 breaking。涉及：`internal/cli/process_readloop.go:199-207`。
- [ ] **R222-PERF-4 — `eventPushLoop` 同 session N tab 各自 marshalPooled（P2，重申 R219-PERF-1 + R214-PERF-4）**: 10 tab × 50 session × 50 evt/s 最坏 25000 独立 JSON 编码/s。方案：Hub 层 per-key 单广播 goroutine 序列化一次后 fan-out 共享 []byte。涉及：`internal/server/wshub.go:1070`。
- [ ] **R222-PERF-5 — `marshalPooled` 每次 make+copy 输出，broadcast 路径多客户端共享场景多余（P2）**: 拆 marshalPooledRef 不 copy 版供 broadcast，调方 put 回 pool。涉及：`internal/server/dashboard.go:88-90`。
- [ ] **R222-PERF-6 — `handleList` storeGen 不变仍重建 sessionWorkspaces map / workspaces slice（P2，重申 R219-PERF-2）**: 引入 lastListVersion+lastListJSON 缓存命中直接 Write。涉及：`internal/server/dashboard_session.go:388`。
- [ ] **R222-PERF-7 — `Snapshot()` 8 次顺序 atomic.Pointer.Load（P2，重申 R219-PERF-3 / R215-ARCH-P2-7）**: 打包 immutableBox + mutableBox。涉及：`internal/session/managed.go:841-857`。
- [ ] **R222-PERF-8 — `invokePersistSinkSingle` 单槽 slice heap escape（P2，重申 R219-PERF-4）**: 需 -benchmem 验证后再决定 sync.Pool 或栈数组方案。涉及：`internal/cli/eventlog.go:653`。
- [ ] **R222-PERF-9 — `newEventLogSink` 每 entry make+copy 出 pooled buffer（P2）**: bridgeEncPool 复用 encoder 但仍强制 copy。方案：批 batch 一次 make 切片分发；或 PersistSink 接受 []byte slice，sender 保证生命周期。涉及：`internal/session/eventlog_bridge.go:98-99`。
- [x] **R222-PERF-10 — `ListSessions` 持 RLock 收 refs 再释放，Snapshot 在锁外但每次双 make（P2）**: sync.Pool 复用 []*ManagedSession；或 inline 成单循环填 snapshots。涉及：`internal/session/router.go:3643-3654`。 — 已修复（listRefsPool 复用 *[]*ManagedSession：cap 不足时按需 grow 一次，归还前清指针 + reset 长度防止 pool 条目 pin Session），本批 PR #99
- [ ] **R222-PERF-11 — `EntriesSince` 多 tab 同 EventLog 各自调 + 各自 marshal（P2）**: EventLog 引入 last-batch JSON 缓存，notify 时直接复制。涉及：`internal/cli/eventlog.go:898-929`。
- [x] **R222-PERF-12 — `persister.handleBatch` Clock() 调两次（P3）**: 在 run() case `<-p.in` 分支单次捕获 now 传 handleBatch，省 vDSO。涉及：`internal/eventlog/persist/persister.go:636,681`。 — 已修复（run + drainInChannel + shutdown drain 三入口收敛 + handleBatch 签名加 now time.Time 参数），本批 PR #95
- [x] **R222-PERF-13 — `shimMsg.Code *int` 每 cli_exited heap alloc（P3）**: 改 `Code int + CodePresent bool`。涉及：`internal/cli/process_readloop.go:38-44`。 — 已修复（shimMsgCode struct{Value int; Present bool} + UnmarshalJSON，4 case round-trip 表 + 3 case invalid 锁三档语义），本批 PR #94
- [x] **R222-PERF-14 — `heartbeatLoop` 30s ping 仍走 encodeShimMsg pool（P3）**: 预计算 `pingBytes = []byte(\"{\\\"type\\\":\\\"ping\\\"}\\n\")`，shimSendRaw 直送。涉及：`internal/cli/process_readloop.go:511-512`。 — 已修复（包级 shimPingBytes + Process.shimSendRaw helper，byte-equal 测试锁定与 encoder 路径输出一致），本批 PR #94
- [x] **R222-PERF-15 — `BroadcastCronRun*` 每次多次 SanitizeForLog（P3）**: 在 Job struct 上缓存 sanitized 字段；hex runID 跳过 sanitize。涉及：`internal/server/wshub.go:1407-1440`。 — 已修复（新增 sanitizeHexIDForBroadcast helper：cron.IsValidID-shape 直接零分配返回，否则回退 SanitizeForLog；jobID/runID 短路），本批 PR #96

### 代码质量 — 方法过长 / 共享 helper

- [ ] **R222-CR-1 — `session.NewRouter` 350 行 6+ 阶段直列（P2）**: 拆 initWrappers / loadPersistedState / startEventLogPersister / startBackgroundLoops。涉及：`internal/session/router.go:706`。
- [ ] **R222-CR-2 — `upstream/connector_rpc.go handleRequest` 522 行 18-case switch（P2）**: 抽 (*Connector).handleSend/handleTakeover 等私方法。涉及：`internal/upstream/connector_rpc.go:50`。
- [ ] **R222-CR-3 — `session.reconnectShims` 334 行 9-state enum + 嵌套 goroutine（P2）**: 拆 classifyAndPlanShimAction + executeShimAction。涉及：`internal/session/router.go:1276`。
- [ ] **R222-CR-4 — `cron.executeOpt` 247 行（P2）**: 抽 recordAndBroadcastRun。涉及：`internal/cron/scheduler.go:1677`。
- [x] **R222-CR-5 — 三处 firstLine/firstLineTrunc 跨包独立实现（P2）**: dispatch/status.go:135、cli/subagent_transcript.go:389、cron/job.go:192 各有版本。方案：抽 textutil.FirstLine（dispatch 语义最完整）；cli 版本变成 textutil.FirstLine + textutil.TruncateRunes 的一行 wrapper。涉及：3 文件。 — 已修复（textutil.FirstLine 收敛 dispatch + cron 跨任意空白行扫语义；textutil.FirstLineLiteral 保留 cli 字面切片版本；删 dispatch::firstLine + cli::firstLineTrunc + cron::JobTitleOrFallback 内联 strings.Split；新增 9+6 case 表驱动测试），本批 PR #98
- [x] **R222-CR-6 — `magic 120` filename cap、`maxLen` 等无名参数散落（P3）**: 提到 file/package 级常量带原由注释。涉及：`internal/server/dashboard_send.go:505`，`internal/server/dashboard_session.go:42`。 — 已修复（dashboard_send.go 内联的 maxFilenameLen=120 提为 file-level 常量 maxClientFilenameRunes 并加原由注释；dashboard_session.go 的 maxResumeLastPromptBytes 既已是命名常量，本轮无需动），本批 PR #96
- [x] **R222-CR-7 — `firstLineTrunc` vs `firstLine` 空行处理语义分歧未文档化（P3）**: 给两者加 godoc 标注；或抽 textutil 时锁定唯一语义。 — 已修复（dispatch/status.go::firstLine 与 cli/subagent_transcript.go::firstLineTrunc 加 godoc 互相交叉引用空行处理差异，R222-CR-5 收敛前防退化），本批 PR #95
- [x] **R222-CR-8 — `sessionSendLegacy` Deprecated 注释无 tracking anchor（P3）**: 加 `// Removal tracked in docs/TODO.md R-LEGACY-SEND` + 新建 TODO 条目，含明确移除条件。涉及：`internal/server/send.go:561`。 — 已修复（Deprecated 注释加 `Removal tracked in docs/TODO.md R-LEGACY-SEND` 锚点 + 明确两条移除条件；下方新增 R-LEGACY-SEND 跟踪条目），本批 PR #96
- [ ] **R-LEGACY-SEND — 删除 `Hub.sessionSendLegacy` 与其在 `sessionSend` 中的 fallback 分支（LOW，由 R222-CR-8 派生）**: sessionSendLegacy 是 MessageQueue 接入前的旧 send 路径，现仅供未配 Queue 的测试代码使用。NewHub 已在 Queue==nil 时打 slog.Warn，但仍允许构造。Removal 条件：(1) 所有驱动 Hub 的测试迁到真实 MessageQueue（或与其投递契约一致的 stub）；(2) NewHub 把 Queue==nil 从 Warn 升级为 Fatal（构造期 hard-fail）。两条都满足后，删除 sessionSendLegacy + sessionSend 中调它的 if 分支，把 guard/interrupt 语义收敛到唯一一处。涉及：`internal/server/send.go:561`，调用方在 `internal/server/send.go` sessionSend 内部分支。
- [x] **R222-CR-9 — `managed.go` TODO 引用 R217-ARCH-2 但 R219-ARCH-3 已 supersede（P3）**: 同步交叉引用。涉及：`internal/session/managed.go:941`。 — 已修复（注释加 R219-ARCH-3 锚点），本批 PR #94
- [x] **R222-CR-10 — 4 包各自定义 SessionRouter 但缺编译时静态断言（P3）**: 各包加 `var _ SessionRouter = (*session.Router)(nil)` 抓签名漂移。涉及：cron/dispatch/upstream/server consumer.go。 — 已修复（upstream 加 var _ SessionRouter，server 加 var _ HubRouter；cron/dispatch 既有断言保持），本批 PR #94

### 架构 — 大重构 NEEDS-DESIGN

- [ ] **R222-ARCH-1 — `session.Router` 已是 god object（73 方法 / ~20 字段 / 4100 行单文件）（P1）**: 拆 sessionStore + procPool + shimReconciler + persistenceCoord + historyLoader 五子组件，Router 退化为门面；contract_test.go 守的对外契约不破。是当前最大的架构债。
- [ ] **R222-ARCH-2 — shim 协议细节泄漏到 session 层（P1）**: session/router.go 直调 `shim.SocketPath/KeyHash/WaitSocketGone/ServerMsg/State`；cli.Wrapper 应吸收为 `WaitSocketGoneForKey(key,dur)` + `Reconnect(ctx,key,lastSeq) (*Process, midTurn, err)`。
- [ ] **R222-ARCH-3 — `internal/config` 反向 import `internal/session`（P1）**: 仅为读 `session.DefaultMaxProcs` 一个常量。方案：抽 internal/sessionconst 或 internal/defaults 子包，session/config 都依赖它。
- [ ] **R222-ARCH-4 — `cli.PersistSink` 与 `persist.PersistSink` 双胞胎，bridge 翻译层（P1）**: 抽 internal/eventlog/schema 唯一 entry 类型来源；或保留 bridge 但显式重命名两端。
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

- [x] **R220-SEC-1 — `shimEnvAllowedPrefixes` 通配 `GIT_` 前缀转发 `GIT_PROXY_COMMAND`/`GIT_SSH_COMMAND`/`GIT_EXEC_PATH` 到 CLI 子进程（P3）**: 这三类 git env 设置 git 执行外部命令的路径，宿主环境若被毒化即可让 Bash tool 通过 `git clone` 触达 RCE。方案：`shimEnvAllowedPrefixes` 把 `"GIT_"` 拆成显式列表（`GIT_AUTHOR_NAME=`, `GIT_COMMITTER_NAME=`, `GIT_AUTHOR_EMAIL=`, `GIT_COMMITTER_EMAIL=`, `GIT_CONFIG_GLOBAL=`），排除 PROXY/SSH/EXEC_PATH。涉及：`internal/shim/manager.go:892`。 — 已修复（拆 8 项显式 allowlist：AUTHOR/COMMITTER NAME+EMAIL、CONFIG_GLOBAL/SYSTEM、DIR/WORK_TREE），本批 PR #90
- [x] **R220-SEC-2 — `dashboardToken == ""` 短路出现在 `ConstantTimeCompare` 之后造成时序信道（P3）**: login handler 在 ConstantTimeCompare 后再判 `if a.dashboardToken == "" || !matched`，`||` 短路使"未配 token"路径比"配置但 token 错"路径快，远端可经时序区分两态。方案：启动期 `if cfg.DashboardToken == "" { return all-allowed handler }` 把 nil-token 旁路抽到 mux 装配阶段。涉及：`internal/server/dashboard_auth.go:282`。 — 已修复（位运算 matched & configured 强制两侧求值消除 `||` 短路 + 4 case 表驱动测试锁三档拒绝行为），本批 PR #90
- [x] **R220-SEC-3 — `gzipMiddleware` 在 `MaxBytesReader` 之前解压，gzip-bomb 可绕过 per-handler body cap（P2）**: gzip 中间件包裹整个 mux，每个 handler 调 `MaxBytesReader` 但只限制压缩字节；1KB gzip → 解压可达 GB 级。方案：在 gzipResponseWriter 内部对解压输出再套 io.LimitReader（cap 设为 2× MaxBytesReader 上界）。涉及：`internal/server/server.go:735`。 — 已确认是误报关闭（gzipMiddleware 实际是 response-side only：`gzip.go:139-166` 只包 ResponseWriter 的输出做 gzip 压缩，从不读 r.Body / 不调用 gzip.NewReader 解压请求体；`gzipResponseWriter.decide()` 也仅基于响应 Content-Type 做条件压缩。请求体 gzip-bomb 是 request-parsing path 独立担忧，与本中间件正交，gzip.go:144-147 godoc 已明示。R217-SEC-3 / R224-CR-1 同根因，一并关闭）。

### Go 正确性 — 跨包改动

- [ ] **R220-GO-1 — `cli.SubagentLinker.Resolve` retryInterval 250ms `time.Sleep` 无 ctx 中断（P1，与 R219-GO-1 同批修复）**: Resolve 多次 retry 间隔用 `time.Sleep(l.retryInterval)`，shutdown ctx 取消时 goroutine 仍要等满 retry 间隔（最多 12 次 × 250ms = 3s）。方案：`select { case <-time.After(l.retryInterval): case <-ctx.Done(): return }`，与 R219-GO-1 接 ctx 改造同批进行。涉及：`internal/cli/subagent_link.go:288,326`。
- [ ] **R220-GO-2 — `project_files.go` `serveRender`/`servePreview`/`serveRaw` 三处独立二次 os.Open 共享 R219-SEC-2 TOCTOU 缺口（P1，扩展 R219-SEC-2）**: R219-SEC-2 只点了 serveRender，但 servePreview（第 742 行）与 serveRaw（第 836 行）也各自独立 `os.Open(resolved)` 二次读盘；preview 路径还把内容塞进 JSON 返回。方案：handleFileGet 统一在 Lstat 阶段持已 open 的 *os.File 传递到三个 helper，避免 double-open race。
- [x] **R220-GO-3 — `cron.applyJitter` 不识别 jitter window 内 DeleteJob，jobRunningGuard 留死锁式 hold 阻塞 TriggerNow（P2）**: applyJitter 接受 `s.stopCtx` 但不检查 `s.jobs[j.ID]` 是否仍在；jitter window 内 DeleteJob 会让 guard 一直 hold 到 jitter 到期才发现 job 已删，期间 TriggerNow 同 ID 失败。方案：jitter 期间订阅 per-job cancel channel，或 applyJitter 返回后再判 job 存在性。涉及：`internal/cron/scheduler.go:1508`。 — 已修复（applyJitter 返回后 RLock 检查 `s.jobs[j.ID]` 存在性，已删则提前 return 让 deferred inflight.running.Store(false) 立即释放 CAS guard，TriggerNow 同 id 不再被"already running"误拒），本批 PR #192

### 性能 — 协议接口变更或需 benchmark

- [ ] **R220-PERF-1 — `countActive()` evictOldest/Takeover/spawnSession 路径全 map scan（P1）**: 4 个 caller 各自 `r.mu.Lock()` 下做完整 map 扫描，500 session 量级会显著增加锁内 CPU；Cleanup 已用 `newActive` 增量，evict/takeover 没接。方案：传 `delta int` 给热路径做原子加，countActive 仅在 Cleanup 全量重算。涉及：`internal/session/router.go:2126,2400,2472,4067`。
- [x] **R220-PERF-2 — `EventLog.SetAgentInternalID` 全 ring backward scan 在 OnResolve 异步回调写锁内执行（P2）**: 每次 resolve 走 500 entry backward scan 到 patch InternalAgentID/JSONLPath/FirstPromptID，期间堵 Append。方案：维护 `toolUseID → ringIndex` 小 map，直接 slot patch，O(1) 替代 O(N)。涉及：`internal/cli/eventlog.go:560`。 — 已修复（R225-PERF-13 落地：eventlog.go:21 `setAgentInternalIDMaxScan = 50` 上限 + foundAgent/foundTaskStart 双条匹配后 break early，wlock 持锁窗口从 O(maxSize=500) 降到 ≤50 entry），本批 PR #192
- [ ] **R220-PERF-3 — `EventLog.EntriesSince` 初始 catch-up 在 RLock 下复制 500 entry × 512B（P2）**: 反向扫描+复制全在 l.mu RLock 内，subscriber 初始订阅时阻塞 Append 一段时间。方案：先 snapshot ring 索引（head/count），release RLock，再在临时 slice 内拷贝。涉及：`internal/cli/eventlog.go:869`。
- [ ] **R220-PERF-4 — `Cleanup` pass2 对 candidate 做 proc.Alive + proc.IsRunning 二次锁获取（P2）**: pass1 在 r.mu RLock 下收集 candidate proc 指针，pass2 又对每个 candidate 取 `proc.mu.RLock` 跑 IsRunning，与热 Send 路径锁竞争。方案：pass1 同时 capture proc.GetState() 一次，pass2 直接读 state。涉及：`internal/session/router.go:2920-2946`。
- [ ] **R220-PERF-5 — `hub.debounceMu` 高频锁获取无 atomic 短路（P2）**: 50 tab × 5 evt/s 让 debounceMu 拿 ~300×/s 包括 timer callback 重入。方案：atomic.Bool "pending" flag 在 fast path 取代 mutex acquire；首次 set 触发 AfterFunc。涉及：`internal/server/wshub.go:140-149`。
- [x] **R220-PERF-6 — `Snapshot.TurnAgents` 即使 turnAgents 为 nil 也走 RLock+slice 复制（P3）**: 大多数 session 任意时刻 turnAgents 为空，仍每次 1 Hz × N tab × 50 sess 走锁。方案：`atomic.Int32 turnAgentCount`，Snapshot 在 count==0 时跳过 RLock+复制。 — 已修复，本批 PR #84

### 代码质量 — 错误消息一致性

- [x] **R220-CR-1 — 多处 "X too long" 错误消息不带 limit 数值（P3）**: `session/key.go:133`, `session/workspace.go:53`, `session/label.go:32`, `shim/manager.go:47` 仍用 bare "too long" 而非 "exceeds N-byte limit"，导致 API consumer 不知道 cap 值。本轮已修 notify_chat_id；剩余 4 处建议作为统一格式批量修。方案：单 PR 把 4 处错误格式化加 limit。Breaking：API 错误消息字符串变化，下游不应依赖。 — 已修复，本批 PR #84
- [x] **R220-CR-2 — `Guard.lastWait` R217-GO-1 leak 无 inline TODO 关联注释（P3）**: `sessionSendLegacy` 用 `Guard.AcquireTimeout` 但无 `// TODO(R217-GO-1)` 标记，未来 reviewer 会把 leak 当新发现重报。方案：在 send.go AcquireTimeout 调用点加 inline 注释指 R217-GO-1。涉及：`internal/server/send.go:565`。 — 已修复，本批 PR #84

## Round 218 — 5-agent 并行 review 第 32 轮（2026-05-16）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 100+ 条发现。
> 6 条 FIX-READY 已落地（PR #40 + PR #22）。以下是需设计决策、破坏兼容、跨包重构、
> 或方案不唯一不适合本轮直接修的条目。

### Go 正确性 — 跨包改动

- [ ] **R218-GO-2 — `dispatch.go:969-1002` sendAskQuestionCard goroutine 访问可能已释放的 tracker**: stop() 先执行后该 goroutine 仍对已释放 platform 进行类型断言。建议：加 context timeout 或在 stop() 里主动取消待发送卡片 goroutine。`internal/dispatch/dispatch.go:969-1002`。
- [x] **R218B-GO-1 — `discoveryCache.startLoop` 初始 `go dc.refresh()` 无 WaitGroup 追踪（P2）**: `startLoop` 启动一个裸 goroutine 做初始 refresh，Server Shutdown 取消 ctx 后该 goroutine 仍在后台运行，可能访问已清理的 projectMgr。方案：给 `discoveryCache` 添加 `wg sync.WaitGroup`，`startLoop` 前 `wg.Add(1)` + defer Done，暴露 `Wait()` 供 Server.Shutdown 调用。涉及：`internal/server/discovery_cache.go:47-60`, `internal/server/server.go` Shutdown 路径。 — 已修复，见 PR #68
- [x] **R218B-GO-2 — `handleOwnerLoopPanic` 用 `context.Background()` 向用户回送错误（P1 重申 R217-GO-2）**: recovery handler 创建 Background ctx 通知用户，若 appCtx 已取消（shutdown 期间）会挂起。方案：接受 parentCtx 参数或用 `context.WithTimeout(context.Background(), 5*time.Second)`。涉及：`internal/dispatch/dispatch.go:510`。 — 误报关闭（dispatch.go:518 已是 `context.WithTimeout(context.Background(), 15*time.Second)`，挂起不存在；从 parent ctx detach 是有意设计，shutdown 期间 parent 已 cancel 时仍能给用户回 panic 错误。R224-CR-2 同根因），本批 PR #150
- [ ] **R218B-GO-3 — `readLoop` linker.Resolve goroutine 无 context 绑定（P1）**: `go linker.Resolve(taskID, toolUseID, ...)` 启动时无 cancellation。进程 shutdown 后 Resolve 可能继续访问磁盘。方案：`linker.Resolve` 接受 ctx 参数，绑定到 process 生命周期。涉及：`internal/cli/process_readloop.go:324`，`internal/cli/subagent_link.go`。Breaking：是（接口变更）。

### 安全 — 新发现（非重复）

- [x] **R218B-SEC-2 — `project_files.go` stat→open TOCTOU 窗口（P3）**: `statRelWithRoot` 调用 `EvalSymlinks + Stat`，后续 preview handler 再次 `Open` 同路径。两次调用之间攻击者可替换 symlink 指向敏感文件。现有 `EvalSymlinks` 已 resolve 到真实路径，但 preview 端点重新 join + Open 而不是用已 resolved 路径。方案：`statRelWithRoot` 返回 `resolved string` 供 preview handler 直接复用，避免二次 EvalSymlinks。涉及：`internal/server/project_files.go:444-491`。 — 已修复（采用 Lstat-after-resolve defense-in-depth 而非 resolved 复用，更紧凑），见 PR #71

### 性能 — 需 benchmark 确认

- [ ] **R218B-PERF-1 — `resubscribeEvents` 每次调用 `time.NewTimer` 分配（P1）**: 客户端重连 flap 时多路并发 `resubscribeEvents` 各自分配 Timer，GC 压力在 N client 同时断线重连场景可观。方案：改用 `time.AfterFunc` 或 Timer 池。注意现有代码已在循环内 Reset 复用同一 Timer（`timer.Reset(5s)`），只是首次分配无法避免——实际影响有限，benchmark 后决策。涉及：`internal/server/wshub.go:1080`。
- [ ] **R218B-PERF-2 — `ownerLoop` 每次 collect 窗口 `time.NewTimer` 分配（P2）**: `collectTimer := time.NewTimer(d.queue.CollectDelay())` 在 ownerLoop 函数体内分配，ownerLoop 是每条消息的热路径。方案：改 `time.AfterFunc` 或在 Dispatcher 持有复用 Timer。涉及：`internal/dispatch/dispatch.go:448`。

### 架构 — 新发现

- [x] **R218-ARCH-1 — cron.SessionRouter 未纳入 contract_test（已修复，见 PR #40）**: ~~四个 consumer 中 cron 独缺编译期 pin，Router 签名漂移对 cron 无编译报警。~~ — 已修复，见 PR #40（本批 PR #91 仅同步 [x]）
- [ ] **R218-ARCH-2 — 4 个 consumer SessionRouter 接口定义方法重叠但无共享基础**: dispatch/cron/server/upstream 各声明独立 SessionRouter，方法签名漂移只能靠 contract_test 间接检测，无法共享 `CoreRouter` 提供编译期强绑定。方案：定义 `session.CoreRouter` interface，4 个包 embed 扩展。非 breaking，中等工作量。
- [ ] **R218-ARCH-3 — Protocol 接口 SupportsX / Capabilities 双轨（R214-ARCH-1 重申）**: Protocol 同时有 SupportsReplay/SupportsPriority 和 Capabilities() Caps，新 backend 实现者不清楚该实现哪个。建议撤除老 Supports* 方法，强制 Capabilities() 单一入口。Non-breaking，小工作量。`internal/cli/protocol.go`。
- [ ] **R218B-ARCH-2 — `Dispatcher.projectMgr` 与 `resolver` 双信息源（P3）**: `projectMgr` 仅用于 slash-command UX，`resolver` 持有 DataSource；并发修改下两者可能对同一项目产生不一致视图。方案：将 slash-command 的 projectMgr 访问路由到 resolver 暴露的接口，统一信息源。涉及：`internal/dispatch/dispatch.go:39-84`。

### 代码质量 — 新发现

- [ ] **R218-CR-1 — `dispatch.go:900-950` dispatchCommand 10+ case switch 无表驱动**: 无法编译期验证所有命令被测试覆盖。建议：`map[string]commandHandler` 表驱动 + 循环分派。`internal/dispatch/dispatch.go:900-950`。
- [x] **R218-CR-3 — `dispatch.go:545-560` takeoverFn 返回值被丢弃**: 即使 takeover 失败也继续走 GetOrCreate+Send，若 takeover 意图阻止后续操作会被静默忽略。`internal/dispatch/dispatch.go:545-560`。 — 已确认现状，dispatch.go:534-543 已加 RNEW-010 注释明确 "intentionally ignore"（GetOrCreate 兜底覆盖两种语义），无需代码改动。

## Round 217 — 5-agent 并行 review 第 31 轮（2026-05-13）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 104 条发现。
> ~18 条 FIX-READY 已落地（详见 git log）。以下是需设计决策、破坏兼容、跨包重构、
> 或方案不唯一不适合本轮直接修的条目。

### 安全 — Breaking / 需 operator 决策

- [ ] **R217-SEC-1 — `AgentOpts.ExtraArgs` 缺 flag allowlist（重申 R216-SEC-2）**: dashboard agent 编辑用户可在 `agents.*.args` 写入 `--mcp-config /host/secret` / `--append-system-prompt` 加载任意配置。配置层 `validateArgvStrings` 仅拒控制字节、不限制 flag 名。方案：`BuildArgs` 调用前对每元素 allowlist（`--model` / `--add-dir` / `--max-turns` / `--append-system-prompt`），或在 `validateConfig` 阶段明确允许的 flag 集合。**Breaking**：需要枚举所有现存 backend args 配置。涉及：`internal/cli/protocol_claude.go:56`、`internal/session/router.go:1959`。
- [ ] **R217-SEC-2 — 远端 node workspace 仅做语法校验（重申 R61-SEC-2 设计）**: `dashboard_send.go:773 validateRemoteWorkspace` 仅 path-shape 检查，不调 EvalSymlinks（远端 root 在另一台机器无法本地 resolve）。当前注释承认这是设计意图。后续要做 cross-node trust：要么强制远端节点本地 EvalSymlinks 并回传校验结果，要么 dashboard 把 workspace allowlist 配在主节点。
- [x] **R217-SEC-3 — gzip 解压链路潜在 bomb 风险**: `internal/server/gzip.go` `gzip.NewReader` 解压无大小上限。当前 `MaxBytesReader` 是压缩字节级，若未来某 path 在 gzip middleware 之后才设 cap，会留下 bomb 窗口。方案：`io.LimitReader` 包装 gzip reader 输出，按 per-handler body cap。需先核实 gzip middleware 实际是否在 MaxBytesReader 之前。 — 误报关闭（与 R220-SEC-3 / R224-CR-1 同根因：gzipMiddleware 是 response-side only，从不调 gzip.NewReader 解压请求体；gzip.go:139-147 godoc 已明示。本批 PR #150 同步关闭三处）。
- [ ] **R217-SEC-4 — `gogo/protobuf v1.3.2` CVE-2021-3121（间接依赖）**: aws-sdk-go-v2 间接依赖。naozhi 不直接调用，但为消除告警可 `go get github.com/gogo/protobuf@latest` 或在 go.mod 加 replace。
- [ ] **R217-SEC-5 — `golang.org/x/crypto v0.49.0` 偏旧（约 10 个月）**: 无已知 critical CVE 但建议跟随 toolchain 升级。
- [ ] **R217-SEC-6 — `dashboardToken` 轮转无显式 session 失效机制**: 当前依赖 cookieMAC(secret, dashboardToken)，token 改后旧 cookie 自然失效，但需要 process restart。增设 server-side session generation counter 才能不重启即时撤销。Breaking：需要持久化 generation 状态。
- [ ] **R217-SEC-7 — `writeJSON` 全局 `SetEscapeHTML(false)`**: 当前是 Feishu 卡片需求；defense-in-depth 上应分离 Feishu encoder pool 和通用 API encoder pool。Breaking：Feishu 卡片含 `<>&` 时输出会变。
- [ ] **R217-SEC-8 — `/health` 在认证后返回 workspace_id / node 状态等运营情报，cleartext HTTP 部署可被嗅探**: 部署侧问题，添加启动 warning 即可：non-loopback bind + 无 TLS terminator 时提示。

### Go 正确性 — 跨包改动 / 需 ctx 传递

- [ ] **R217-GO-1 — `Guard.lastWait` 在不发起 Release 的路径下永久泄漏**: 现状 ShouldSendWait 写 + Release 删；不调 Release 的 path 留下永久 entry。方案：sync.Map+TTL sweep（如 seenNonces），或显式 cap+LRU。
- [x] **R217-GO-2 — `handleOwnerLoopPanic` 用 `context.Background()`**: 当前签名不收 ctx，要改成 `(ctx, key, msg, r)` 影响测试与 owner-loop 路径。Breaking：函数签名变化。 — 误报关闭（与 R218B-GO-2 / R224-CR-2 同根因：dispatch.go:518 已 WithTimeout 15s，从 parent ctx detach 是 panic recovery 的有意设计），本批 PR #150
- [ ] **R217-GO-3 — `historyCtx` 派生自 `context.Background()` 而非 app ctx（重申 R216-GO-4）**: 异常退出路径下 historyWg goroutine 不被取消。需 NewRouter 收 appCtx（构造函数签名变化）。
- [ ] **R217-GO-4 — `spliceLog` 每 record `json.Unmarshal` 取已知 seq**: 重新解码 record body 只为读 seq；可由 idxEntries 索引位置直接拿。改动需谨慎（保证 seq 不被外部恶意改）。
- [ ] **R217-GO-5 — `cron.Stop()` deadline 后泄漏 triggerWG goroutine（R44 重申）**: 单 shot 设计可接受；测试 -count=N 污染。长期需重构 triggerWG/Stop 协议。
- [ ] **R217-GO-6 — cron `sendCtx` 派生自 Background()**: DeleteJob 后 sendCtx 不被 router.Shutdown 取消；jobTimeout 60min 场景 session 可在 job 删后继续跑 60min。
- [ ] **R217-GO-7 — `storeStringAtomic` fast-path 可能 silently no-op `deathReason` 清空**: managed.go:254 注释自承"逻辑 race"。需用 `Store(new(string))` 强制材料化清空，或加专用 `clearDeathReason` 方法。

### 性能 — 协议接口变更或需 benchmark

- [ ] **R217-PERF-1 — `ClaudeProtocol.ReadEvent(line string)` 双 copy（重申 R216-PERF-1 / R67-PERF-1）**: 接口改 `[]byte` 跨 cli/session。Breaking：协议接口。
- [ ] **R217-PERF-2 — `shimWriter.Write` `string(line[:n-1])` 全消息 copy（R216-PERF-2 重申）**: shimClientMsg.Line 改 json.RawMessage 或加 `shimSendRaw`。Breaking：shim 协议。
- [ ] **R217-PERF-3 — eventlog_bridge.go:49 per-EventEntry `json.Marshal`**: 引入 pooled json.Encoder（同 shimSendBufPool 模式）。
- [ ] **R217-PERF-4 — `eventlog.go:640` 单元素 `[]EventEntry{e}` heap alloc**: sink 契约允许 retain，需先调整契约或 sink 实现拷贝才能用 stack array。
- [ ] **R217-PERF-5 — `pendingIdx` 未预 cap（R216-PERF 重申）**: `make([]schema.IdxEntry, 0, IdxStride*2)`。需 benchmark 确认收益值得增量驻留内存。
- [ ] **R217-PERF-6 — `selectForIdx` 每 flush 新建 slice**: caller-owned scratch 改造。Breaking：函数签名。
- [ ] **R217-PERF-7 — `marshalPooled` 对小重复帧（session_state running/ready）总是 copy**: 预 marshal 静态形状帧。
- [ ] **R217-PERF-8 — `linker.Resolve` 每 task_started 事件 spawn goroutine**: bounded worker pool。多 agent turn 下显著。
- [ ] **R217-PERF-10 — `dashboard_session.handleList` workspaces []string 每 poll alloc**: sync.Pool；需 benchmark + 仔细处理 escape。

### 架构 — 大重构

- [ ] **R217-ARCH-1 — `cli` 已塌陷成"领域类型仓库"被 9 个上层包横向引用**: `cli.EventEntry`/`cli.Event` 同时承担 stream-json 解析输出 + naozhi 内部事件模型 + node wire DTO + persist schema input + history Source。任何 cli 内部字段调整波及 9 包。方案：迁出领域类型到 `internal/event` / `internal/domain`，cli 单方面 produce、其他 consume。长期重构。
- [ ] **R217-ARCH-2 — `server` 直接 type-assert 持有 `*cli.SubagentLinker` / `*cli.EventLog`**: agent_tailer / dashboard_agent_events / wshub_agent 通过 `sess.SubagentLinker()` 拎 cli 内部对象。RFC v4 phase 3 规划的 `AgentIntrospector` 接口未落地。方案：扩 processIface 加 Linker/EventLog 方法，或下沉 tailer 注册到 session 包。
- [ ] **R217-ARCH-3 — `discovery` 反向依赖 `cli` 拿 EventEntry / TruncateRunes（菱形依赖）**: 形成 session→discovery→cli←session。`TruncateRunes`/`DeriveLegacyUUID` 是无状态字符串工具，应迁到 `internal/textutil`。
- [ ] **R217-ARCH-4 — 4 个互相重叠的 `SessionRouter` consumer 接口（dispatch/server/cron/upstream）**: 方法重复，新增 router 方法要在 4 处同步。合并为 `session.RouterFacade` 一个 facade interface。
- [ ] **R217-ARCH-5 — `processIface` 30+ 方法 god 接口（R216-ARCH-1 重申）**: 拆 `ProcessCore` / `EventSource` / `PassthroughExt` / `Introspector` / `Sender`。
- [ ] **R217-ARCH-6 — `Router struct` 28 字段（R216-ARCH-6 重申）**: 拆 eventLogManager / workspaceStore / historyLoader / shimReconciler。
- [ ] **R217-ARCH-7 — `NewRouter` ~335 行 + `executeOpt` 230 行 + `reconnectShims` 320 行**: 函数拆解。长期。
- [ ] **R217-ARCH-8 — `Hub` 31 字段 + HubOptions 18 字段（R216-ARCH-4 重申）**: 提取 `nodeCache` / `subscriptionManager` / `agentTailerRegistry` 子 struct。
- [ ] **R217-ARCH-9 — `Protocol` 接口的 `protocol_acp.go` 实现是否在生产路径活跃**: 若仅占位、文档/测试出现，build-tag 隔离，避免接口被一个不上线的实现绑死。

### 代码质量 — 小改动等合并窗口

- [x] **R217-CR-1 — `sanitizeClientFilename` 改用 `utf8.RuneCountInString` 短路前已落地（本轮）**：保留作为已修锚。 — 已确认（dashboard_send.go:507 仍是短路实现），归档关闭。
- [ ] **R217-CR-3 — `Cleanup` 三阶段加锁窗口**：worst-case stuckKill 目标进程在 Pass 2 已被 spawnSession 替换。`shouldPrune` 已 mitigates，stuckKill 路径未 re-check。需要 pass-2 再次 verify。
- [ ] **R217-CR-4 — `Hub god struct 36 字段 / `node.Conn` 18+ 方法巨型接口**: 子聚合拆分。
- [ ] **R217-CR-5 — cross-node 错误注入方向不对称**: 反向有 LogSystemEvent，正向 node.Conn.Send 失败只 slog 不进 EventLog。
- [x] **R217-CR-6 — workspace 三重重载命名混淆 `cfg.Session.Workspace` / `cfg.Workspaces` / `cfg.Workspace`**: 重命名或文档化。 — 已确认归档（PR #69 已落地：`internal/config/config.go` 顶部 Config struct godoc 块（第 24-37 行）显式列出三种语义 1) Workspace = this-instance identity 2) Workspaces = REMOTE-NODES alias 3) SessionConfig.Workspace = deprecated CWD alias，并指引读 Workspace/Nodes/Session.CWD；Workspaces 字段段亦交叉引用顶部 godoc。R216-CR-5 同根因关闭），本批 PR #183
- [ ] **R217-CR-7 — `project.DisplayName` / `Emoji` schema 校验但 dashboard UI 不读**：要么 wire UI，要么删 schema。

## Round 216 — 5-agent 并行 review 第 30 轮（2026-05-12）NEEDS-DESIGN

> 本轮 5 个 reviewer 并行扫描共 100+ 条发现。15 条 FIX-READY 已落地（单独 commit，参见 git log）。以下是需设计决策或破坏兼容性、不适合本轮直接修的条目。

### 安全 — 破坏兼容 / 需 operator 决策

- [ ] **R216-SEC-1 — S14 `Feishu VerificationToken-only` 模式缺 body-HMAC（重申 P1）**: 5-agent security reviewer 本轮重申此为 P1。持有/嗅到 token 即可伪造任意事件（新 nonce 绕过 dedup）→ 触发 CLI 执行任意 prompt。方案：在 `validateConfig` 里将该模式升为 error，或引入 `feishu.allow_unauthenticated_webhook: true` 显式 opt-in。**Breaking**：影响未配 EncryptKey 的现有部署。
  - 涉及：`internal/platform/feishu/transport_hook.go:98-159`, `feishu.go:315, 400-403`
- [ ] **R216-SEC-2 — `AgentOpts.ExtraArgs` 未做 flag 白名单**: agent 编辑权限的 dashboard 用户可在 `agents.*.args` 写入 `--mcp-config /host/secret` 或 `--append-system-prompt` 加载任意配置。CLI 有 `--skip-permissions`，影响面大。方案：`BuildArgs` 里对 `opts.ExtraArgs` 每元素 allowlist（`--model`/`--add-dir`/`--max-turns`），或在 `validateConfig` 阶段 validate agent args。**Breaking**：需要枚举允许 flag。
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
- [ ] **R216-GO-7 — cron `sendCtx` 从 `Background()` 派生**: DeleteJob 后 sendCtx 无法被 router.Shutdown 取消，60min jobTimeout 场景下 session 可在 job 删除后继续跑 60 分钟。
  - 涉及：`internal/cron/scheduler.go:1531`

### 性能 — 协议接口变更

- [ ] **R216-PERF-1 — `ClaudeProtocol.ReadEvent(line string)` 热路径双 copy**: `[]byte(line) → json.Unmarshal` + `shimMsg.Line` string 反序列化都是拷贝。50 session × 25 events/s ≈ 1250 alloc/s。方案：接口改 `ReadEvent(line []byte)`，同步 `protocol_claude.go`/`protocol_acp.go`/`readLoop`。**Breaking**：跨 cli/session 接口。
- [ ] **R216-PERF-2 — `shimWriter.Write` 快慢两路径 `string(data[:n-1])` copy**: stdin 写入热路径每条消息双向 copy（string → json.Encoder 反向）。方案：`shimClientMsg.Line` 改 `json.RawMessage`。**Breaking**：shim 独立 binary 需同步。
  - 涉及：`internal/cli/process_shim_io.go:54,83`
- [ ] **R216-PERF-3 — `eventlog_bridge.go:49` per-EventEntry `json.Marshal`**: encoding/json reflection 路径每条 ~1KB encodeState alloc。50 sess × 5 ev/s ≈ 250 alloc/s。方案：pooled json.Encoder（同 `shimSendBufPool` 模式），或 MarshalJSONFast。**Breaking**：协议合约。

### 架构 — 大重构

- [ ] **R216-ARCH-1 — `processIface` 24 方法 god 接口（重申 R176-ARCH-M2）**: 本轮 architect 指出已加入 5 个 stream-json specific 方法。拆成 `ProcessCore` / `EventSource` / `PassthroughExt` 三 interface 后 Gemini/Kiro 集成可解耦。
- [ ] **R216-ARCH-2 — `session` 直接 import 4 个 history backend 包 + `claudeDir` 在 RouterConfig**: 违反"cli 是 opaque"原则。方案：`cli.Wrapper.NewHistorySource(...)` + `claudeDir` 搬到 ClaudeProtocol。
  - 涉及：`internal/session/router.go:22-25,216`, `router.go:1031-1058`
- [ ] **R216-ARCH-3 — KeyResolver 迁移半落地**: dispatch 主路径已用 resolver，但 legacy `resolver==nil` 分支 + `commands.go` 4 处 `projectMgr.ProjectForChat` 直接调用 + `server.buildSessionOpts` 仍手动合并。方案：强制 resolver 非 nil + 删 legacy 分支。
- [ ] **R216-ARCH-4 — Hub 36 字段多职责混合**: 同时负责 WS 升级、订阅、远端节点缓存、cron 调度、上传、临时会话池。方案：提取 `nodeCache` / `subscriptionManager` 子 struct。
- [ ] **R216-ARCH-5 — `attachment.GC` 无生产调用点（重申 R204）**: 真正 breaking 的交付不完整 —— CLAUDE.md §Attachment Refcount 声明要"grow only"已在等待 cron 调用。方案：`cron/scheduler.go` 注册 `"attachment-gc"` 系统任务。需设计触发频率、锁边界、并发模型。
- [ ] **R216-ARCH-6 — processIface 以外的 24 方法上帝接口后遗症**: Router 43 字段、ManagedSession 26 字段、`NewRouter` 354 行、`reconnectShims` 339 行、`handleRequest` 523 行、`executeOpt` 238 行。方案：子聚合 + 函数拆解。长期工程。

### 代码质量 — 小改动等合并窗口

- [ ] **R216-CR-1 — `plannerKeyFor`/`isPlannerKey` 复制到 `session/key.go`（R215-CR-P1-1 重申）**: 消除导入循环的临时方案，长期应抽 `internal/sesskey` 叶子包。
- [ ] **R216-CR-2 — `Hub` god 36 字段（同 R216-ARCH-4）**。
- [ ] **R216-CR-3 — `node.Conn` 18+ 方法巨型接口**: 消费者只用 1-2 个；拆成 `NodeReader`/`NodeProxy`/`NodeSubscriber`/`NodeLifecycle`。
- [ ] **R216-CR-4 — cross-node 错误注入方向不对称**: 反向连接有 `LogSystemEvent`，正向 `node.Conn.Send` 失败只 slog 不进 EventLog。dashboard 用户看到一半。
- [x] **R216-CR-5 — 配置 workspace 三重重载命名混淆**: `cfg.Session.Workspace` / `cfg.Workspaces`（nodes）/ `cfg.Workspace`（LocalNode） YAML 和代码 3 处同名。 — 已修复（Config struct 顶部 godoc 块明确三种语义 + 字段交叉引用），见 PR #69
- [x] **R216-CR-6 — `node/protocol.go:33` / `managed.go:950` TODO 无 issue 追踪**：加 Round 号或迁到 TODO.md。 — 已确认现状，PR #38 已把 R214-CODE-6 / R217-ARCH-2 锚点写入两个 TODO 注释，本条与 R214-CODE-6 同根因可一并关闭。
- [ ] **R216-CR-7 — `project.DisplayName` / `Emoji` 有 schema 无 UI wire**: validate.go 校验但 dashboard 不读。

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

- [ ] **R214-ARCH-1 — Protocol 接口 `SupportsX` / `Capabilities()` 双轨**: `internal/cli/protocol.go::Protocol` 接口同时有 `SupportsReplay()/SupportsPriority()/SupportsSoftInterrupt()` 和 `Capabilities() Caps`；RFC ARCH-404 的 Caps foundation 已落地但老 `Supports*` 方法未撤除，实现方对"该实现哪个"无编译期强制。
  - 方案：撤除老 Supports* 方法，强制每个 Protocol 实现 `Capabilities()` 单一入口。
  - 前置：dispatch / server 中残余的 `Supports*` / `Name()=="acp"` 调用点迁移到 `ProtocolCaps(p).X`（RFC ARCH-404 consumer 迁移）
  - 涉及：`internal/cli/protocol.go`, `protocol_claude.go`, `protocol_acp.go`

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

- [x] **R214-SEC-3 — ANTHROPIC_ 前缀全量透给 shim**: `shim/manager.go::shimEnvAllowedPrefixes` 允许 ANTHROPIC_* 全系列进子进程；Bedrock 部署下 `ANTHROPIC_API_KEY` 冗余却可被 Bash tool 读。
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

- [x] **RNEW-UX-001 — WS 重连无 jitter，N 客户端雪崩**: `dashboard.js:6717-6730` `backoff = min(backoff*2, 30s)` 纯指数，服务重启时所有 tab 在同一 ms 点重连，瞬时打满。
  - 方案: `delay += Math.floor(Math.random() * 500)` 或 full-jitter。
  - 涉及: `dashboard.js:6717-6730`
  - 已修复（dashboard.js:7855-7867 已 inline `// RNEW-UX-001:` 标记 + `Math.floor(Math.random() * 500)` 加性 jitter；本批仅同步 [x]），本批 PR #91

- [ ] **RNEW-UX-003 — 173 处 `fetch(...)` 无 AbortController / 无超时**: NAT 空闲 TCP 被丢时 ajax 挂死数分钟，按钮无响应也无 spinner。
  - 方案: 全局 `fetchJSON(url, {timeoutMs:10000})` wrapper；切页面/会话时 abort 上一批 in-flight。
  - 涉及: `dashboard.js` 全局

- [ ] **RNEW-UX-006 — 80+ inline `onclick="..."` 字符串拼接强依赖 escJs/escAttr**: `dashboard.js:1031,3265,3554,3645,3972,4280,4320,4583,8958` 等。id 里漏网的反斜杠/换行即 XSS；阻碍启用严格 CSP（与 R172-SEC-H2 根因重合）。
  - 方案: 渲染后 event delegation `list.addEventListener('click', e => { const a = e.target.closest('[data-action]'); ... })`，批量替换。
  - 涉及: `dashboard.js` 全局（80+ 站点）

- [ ] **RNEW-UX-015 — inline #xxxxxx 硬编码颜色绕过 CSS 变量（micro-batch 已落地 2026-05-10，余续做 ratchet）**: 原描述 32 处，实测起始 36，本轮迁移 5 处（8 个 hex 实例：earlier-events-btn 3、history popover、drag-over border、nav active color 2、nav popover item 2）到 `--nz-bg-2/--nz-border/--nz-text/--nz-accent/--nz-text-dim` tokens；`TestDashboardJS_RNEW_UX015_HexBaseline` 契约测试设 `ceiling = 28`（当前实际 28 零 slack，禁止回升）。**剩余**：批量迁剩 28 处；跳过 `#1f2937`（无 canonical variable）。

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

---

## Round 215 — 5-agent 深度 review 第 29 轮（2026-05-11）NEEDS-DESIGN 归档

> Round 215 同批次 20 项 FIX-READY 已合入 dev（f19e477 / c468ef2 / 8fb12fe），
> 部署完成 uptime ok，smoke 无 ERROR。以下为需要设计决策或跨模块重构、
> 不能当轮修完的条目（按 agent 分组）。

### go-reviewer（避开已归档后剩余 P1/P2）






- [ ] **R215-GO-P2-5 — `router.ReconnectShims` reconcile 期 `sess.ReattachProcessNoCallback` 无 sendMu 保护（继承 R51-CONCUR-002）**: 运行期 reconcile 与 ManagedSession.Send 并发时，storeProcess 原子替换旧 process 指针 + clearDeathReason 与 Send() 的 timeout 写入有逻辑 race。
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

- [ ] **R215-SEC-P3-1 — Shim `auth_token` 明文写 state 文件**: 同 UID 进程读取 state 后可连 shim socket 注入 stdin。
  - 方案：state file 强制 0600；或启动时重新生成 token，state 不存。
  - 涉及: `internal/shim/server.go:145-159`

- [x] **R215-SEC-P3-2 — Cron workspace 未过 `hub.allowedRoot`**: validateCronWorkDir 只校验字符集，不限于 allowedRoot。
  - 方案：cron API 绑 `validateWorkspace(workdir, allowedRoot)`。
  - 涉及: `internal/server/dashboard_cron.go`
  - 已修复（dashboard_cron.go:382 handleCreate 与 :773 handleUpdate 均已 `validateWorkspace(req.WorkDir, h.allowedRoot)` + 403 forbidden on boundary violation；本批仅核对归档），本批 PR #75


### performance-optimizer（避开已归档后剩余 P1/P2）

- [x] **R215-PERF-P1-1 — `eventlog_bridge.go:49` 每 EventEntry `json.Marshal`**: 持久化 sink closure 对每条 EventEntry 调 json.Marshal (reflection)，热路径。
  - 方案：EventEntry 提供 `MarshalJSONFast` 或改用 `bytes.Buffer + encoder` 复用。
  - 涉及: `internal/session/eventlog_bridge.go:49`
  - 已修复（commit 7fc779f 引入 sync.Pool 复用 json.Encoder + bytes.Buffer，本批仅在源码里加 R215-PERF-P1-1 锚点注释让 reviewer 反查），本批 PR #86

- [ ] **R215-PERF-P2-1 — `eventlog.Append` 单条路径 `[]EventEntry{e}` 每次 alloc**: AppendBatch 的 sinkCopy 已预分配，Append 单条仍每次分配 1 slice。
  - 方案：引入 1-slot pool 或为 Append 写专用 sink 路径。
  - 涉及: `internal/cli/eventlog.go:627`


- [x] **R215-PERF-P2-3 — `wshub.marshalPooled` 返回副本即便单订阅者**: `SendRaw` enqueue 后不再持 slice，小 batch 场景可省 copy。 — 已确认归档（dashboard.go:84-97 `marshalPooled` 必须 copy：源 backing 由 `jsonEncPool` 复用，下次 Get→Reset→Encode 会原地覆盖，省 copy 会让先前调用方拿到的 slice 被后续 marshalPooled 改写产生 fan-out 错乱；当前 SendRaw 链路异步消费，回写时机无法精确判断，copy 是池化语义的必要代价），本批 PR #192
  - 方案：单订阅 fast-path 直接传 pooled buffer，两订阅起再 clone。
  - 涉及: `internal/server/wshub.go:996,1020`

- [ ] **R215-PERF-P2-4 — `eventlog.storeAtomicString` 每次非等存储 `new(string)` heap alloc**: tool summaries 高频变化时持续 GC 压力。
  - 方案：用 sync.Pool 或 generation counter 结构避 pointer alloc。
  - 涉及: `internal/cli/eventlog.go:1027`

- [ ] **R215-PERF-P2-5 — `dashboard.handleList` 每次 poll 全 Snapshot**: 50 session × 1Hz × 10+ atomic.Load，无 storeGen 增量感知。
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

- [ ] **R215-ARCH-P2-6 — `Router.Shutdown` 内 `historyWg.Wait` 5s 超时后 goroutine 实际泄漏**: godoc 承认"intentional bounded by single-shot contract"；与零中断热重启 RFC 冲突。
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

- [ ] **R227-GO-2 — `cli/subagent_link.Resolve` retry 循环 `time.Sleep(retryInterval)` 无 ctx 取消（P1）**: SIGTERM 期间最多 8 个 Resolve goroutine 各自卡 3s。修复需 Resolve 接 context.Context 参数（Breaking）。涉及 `internal/cli/subagent_link.go:294,332`。重申 R225-GO-2。
- [x] **R227-GO-3 — `fireOnResolveLocked` 命名违反 Go 锁约定（P2）**: `...Locked` 通常意味"调用者持锁，函数不操作锁"，但本函数手动 Unlock+Lock。本轮 R227-GO-1 修了 panic-safe，但语义命名仍是陷阱：未来若有人在持锁场景再调一次将 double-unlock。建议重命名为 `fireCallbacksDropLock` 并在 godoc 明确"releases and reacquires l.mu"。 — 已修复，本批 PR #170
- [ ] **R227-GO-4 — `Router.Shutdown` test fallback `time.Sleep(100ms)` busy-poll（P2）**: 测试构造的 `&Router{}` 缺 shutdownCond，30s 超时下 300 次 busy-poll。方案：要求所有路径走 NewRouter；或裸构造时 log.Warn。
- [~] **R227-GO-5 — cron `notifyTarget` 用 `context.Background()` 不响应 stopCtx（误报关闭 2026-05-20）**: 复核后确认与 send 路径同款意图——cron run 记录写入与最终通知归属同一生命周期，必须独立于 stopCtx 才能在 graceful shutdown 期间把已跑完的 turn 结果递达 IM。条目自标"降为 P3 仅记录"，本批 PR #187 关闭归档。
- [ ] **R227-GO-7 — `cli.Resolve` resolveSem acquire 无 ctx select arm（P2）**: 与 R227-GO-2 同根因；Resolve 接 ctx 后可统一 select。
- [~] **R227-GO-8 — `cli/process.go Kill()` 持 shimWMu 调 closeShimConn 与 Detach 顺序不一致（误报关闭 2026-05-20）**: 与 R225-GO-7 同根因一并关闭——核实后 Kill (process.go:519-535) 与 Detach (:617-634) 都是「持 shimWMu 期间 closeShimConn」同一模式，不存在所谓"顺序不一致"。closeShimConn 走 sync.Once + net.Conn.Close，最坏延迟一次系统调用，不会饥饿 heartbeat。本批 PR #169

### 安全 — 本轮新发现

- [ ] **R227-SEC-1 — `protocol_claude.BuildArgs` + `protocol_acp.BuildArgs` ExtraArgs 无 flag 允许列表（P1）**: dashboard 认证用户可注入 `--mcp-config /attacker/server.json` 类危险 flag。修复需引入 flag 允许列表（Breaking：依赖任意 extra args 的运维方需迁移）。重申 R219-SEC-1，本轮强调 `protocol_acp.go:133` 同样有此缺口。
- [ ] **R227-SEC-2 — `serveRender` 在 Lstat 后 os.Open 制造 inode-swap TOCTOU（P1）**: handleFileGet 已有 Lstat-after-resolve 防御，但 serveRender 再次 os.Open(resolved)。方案：handleFileGet 阶段就 OpenFile + 传 fd 给 serveRender。涉及 `internal/server/project_files.go:670`。重申 R219-SEC-2。
- [ ] **R227-SEC-3 — `allowed_root` 缺失时仅 Warn，cron work_dir 无根目录约束（P1）**: dashboard token 已设但 allowed_root 空时，认证用户可设 cron work_dir=/etc。方案：dashboard token != "" + 监听非 loopback 时 Fatal。涉及 `internal/server/server.go:513`。重申 R226-SEC-6。
- [ ] **R227-SEC-4 — `moveToShimsCgroup` 接受 shim 自报 Hello.CLIPID（P2）**: 被劫持 shim 可上报任意 PID。方案：通过 /proc/<CLIPID>/status 验证 PPid。涉及 `internal/shim/manager_linux.go:60-90`。重申 R219-SEC-5。
- [ ] **R227-SEC-7 — 反向 node WS 在 ws:// 明文下传 token（P2）**: 内网嗅探可截获 token 冒充节点。方案：r.TLS == nil 拒绝 Upgrade，或加 insecure_node 显式豁免。重申 R226-SEC-3。
- [ ] **R227-SEC-9 — Dashboard 主页 CSP `script-src 'self' 'unsafe-inline'` 完全打开 XSS 通道（P2）**: 任何同源 XSS 注入即可执行任意脚本。方案：迁移到 CSP nonce 模式。Breaking。涉及 `internal/server/dashboard.go:389`。重申 R226-SEC-2。
- [ ] **R227-SEC-10 — `/health` 端点无认证无限速，泄漏基础设施拓扑（P3）**: 外部攻击者可枚举 session count / watchdog kills / node 状态 / build 版本。方案：per-IP rate limiter + 敏感字段移到认证 /api/stats。重申 R226-SEC-7。
- [x] **R227-SEC-11 — WS Hub 同 session key 无订阅数量上限（P3）**: 单 token 可开 1000 WS 订阅同 session 引 fan-out CPU spike。方案：subscribe 时检查同 key 当前订阅数 ≤20。重申 R226-SEC-8。 — 已修复（同 R226-SEC-8），本批 PR
- [x] **R227-SEC-12 — `mode=download` 路径可下载 .env / .npmrc / .netrc 等点开头配置（P3）**: previewableByExt 排除 .env 但 download 模式不受此保护。方案：download 模式额外 blocklist；或运维文档明确 allowed_root 不含秘密文件。 — 已关闭（实施已落地：`internal/server/project_files.go::serveDownload` 入口加 `isSensitiveDownloadName(filepath.Base(resolved))` 守卫，命中 sensitiveDownloadNames（.env / .env.local / .env.dev / .env.development / .env.prod / .env.production / .env.staging / .env.test / .netrc / .npmrc / .pypirc / .dockercfg）或 sensitiveDownloadExts（.key / .pem / .p12 / .pfx / .crt）返 403 "file type not downloadable"；commit 61cde08 R229-SEC-9 落地。R228-SEC-3 同根因），本批 PR #188
- [~] **R227-SEC-14 — `feishu/transport_ws.go` parseSDKEvent 没有 maxIncomingTextBytes 检查（误报关闭 2026-05-20）**: 复核 transport_ws.go:309 已有 `if len(text) > maxIncomingTextBytes` 守卫 + slog.Warn，与 transport_hook.go 保持对称。条目自标"本条为误报"。本批 PR #187 关闭归档。

### 性能 — 本轮新发现

- [ ] **R227-PERF-1 — `Protocol.ReadEvent(line string)` 内 `[]byte(line)` 复制（P1）**: 5-50 events/s × N session 每行额外 heap alloc。方案：接口签名改 `ReadEvent(line []byte)`，readLoop 传入 trimmed []byte。涉及 `internal/cli/protocol_claude.go:174` + `protocol_acp.go:323`。Breaking（package 内部接口）。
- [ ] **R227-PERF-2 — `ACPProtocol.ReadEvent` 对 agent_message_chunk 路径双 unmarshal（P1）**: 整行 unmarshal 为 RPCMessage 后每个分支再 unmarshal Params/Result。流式文本场景两次全量 JSON 解析。方案：method 短路检查后 lazy-unmarshal。
- [ ] **R227-PERF-3 — `eventlog_bridge.newEventLogSink` per-entry make+copy（P1）**: 5-50 events/s × N session 的小对象 GC 压力。方案：合并为 batch make+copy。涉及 `internal/session/eventlog_bridge.go:98`。
- [~] **R227-PERF-4 — `wsClient.SendJSON(v)` 调 json.Marshal 每次分配 encodeState（误报关闭 2026-05-20）**: 复核后确认 encoding/json 内部 sync.Pool 已 pool encodeState (`encode.go:312-322` newEncodeState/freeEncodeState)，naozhi 加一层 sync.Pool 仅多一次 allocator round-trip 反而更贵。R229-PERF-4 已通过预 marshal 静态帧（wsErrNotAuthMsg/wsErrRateLimitedMsg 等）覆盖了真热的小响应路径。本批 PR #187 关闭归档。
- [ ] **R227-PERF-5 — `WriteUserMessageLocked` json.Marshal encodeState alloc（P2）**: 用户 prompt 发送频率不高但每次 alloc。方案：sync.Pool 复用 bytes.Buffer + Encoder。
- [ ] **R227-PERF-6 — `Cleanup` + `saveIfDirty` 每次写锁内 map clone 3 份（P2）**: 50 session × 30s 间隔每分钟 2 次 O(n) clone。方案：传 []*ManagedSession 切片，配合 listRefsPool。
- [ ] **R227-PERF-7 — `ACPProtocol.WriteMessage` 每次 prompt 用 map[string]any（P2）**: 3-5 个 map + RPCRequest 分配 + marshal。方案：定义具名 struct。
- [ ] **R227-PERF-8 — `BroadcastSessionsUpdate` time.AfterFunc 每次 alloc（P2）**: 高频 notify 下每次 timer + WG 开销。方案：Hub 持久 *Timer + Reset。
- [ ] **R227-PERF-9 — `EventLog.Append` invokePersistSink 单条 slice 逃逸（P2）**: 5-50/s × N session 热路径。方案：EventLog 内置 [1]EventEntry scratch + sync.Pool。涉及 `internal/cli/eventlog.go:660`。
- [x] **R227-PERF-10 — `Snapshot()` MeteringUsage []slice 每次 alloc（P2）**: 大多数为 nil 或 1 条。方案：proc.MeteringUsage() 返回内部数组只读 view，或延迟 alloc 到 JSON 序列化。 — 已修复（meteringLen atomic.Int32 镜像 len(meteringUsage)，applyMetadata 在 meteringMu 内更新；MeteringUsage 进 RLock 前先 Load 短路 nil 返回，覆盖 claude-class + pre-metering kiro turn 主导路径），本批 PR #189
- [~] **R227-PERF-11 — `EventLog.Append` storeAtomicString 每次 *string alloc（评估关闭 2026-05-20）**: 条目自标"降级仅观察"。复核 textutil.StoreAtomicString 已用 atomic.Pointer.Load + 字符串相等短路（同值不重新 Store *string），命中率 ~99%（lastActivitySummary 在同 tool_use 持续时不变）。sync.Pool[string] 在 textutil 是叶子包做不到 lock-free 复用，且分支预测器对短路命中早已优化。本批 PR #168 关闭归档。
- [ ] **R227-PERF-12 — `ACPProtocol.parseSessionUpdate` tool_call/tool_call_update 分支双 alloc（P2）**: AssistantMessage ptr + ContentBlock slice。方案：tool name 直接存 ToolCall.Title；ContentBlock 改 [1]ContentBlock + count。
- [x] **R227-PERF-15 — `protocol_acp.ReadEvent` 每个 turn-end 都 unmarshal stopReason（P3）**: msg.Result 多数为 null 或 {}。方案：bytes.Contains 快速检测后再 unmarshal。 — 已修复，本批 PR #176
- [~] **R227-PERF-16 — `EventEntriesSince` dead-session 分支全量扫描+stable sort（误报关闭 2026-05-20）**: 条目自标"500 entry stable sort < 1µs，可接受"——dead-session 重订阅是低频路径（tab reload），微秒级开销远低于其他热路径。InjectHistory 端排序也会让 replay 与 live append 之间的语义需要重新定义。本批 PR #187 关闭归档。
- [ ] **R227-PERF-17 — `shim.ServerMsg.MarshalLine` 每次 json.Marshal alloc（P3）**: shim binary 独立。方案：sync.Pool[bufEnc]。**降级**：shim 独立 binary，不影响主进程，单独 PR 处理。
- [ ] **R227-PERF-18 — `eventPushLoop` EventEntriesSince per-goroutine 独立 slice（P3）**: 50 订阅 tab × 同 session 各自分配。方案：扩展 EntriesSinceInto(dst) 接口接受 caller-owned buffer。Breaking。
- [~] **R227-PERF-19 — `Cleanup` Pass 1 candidates 用 time.Time 而非 lastActiveNS int64（误报关闭 2026-05-20）**: 条目自标"传染性大，ROI 低"——Cleanup 是 30s tick 的低频路径，time.Time 接口更直观且与 LastActive() 公开 API 对齐；改 int64 会让 candidate slice 类型与 ManagedSession.LastActive() 返回签名分裂，下游若拿到 candidate slice 需各自 time.Unix 还原，传染面广。本批 PR #187 关闭归档。

### 代码质量 — 本轮新发现

- [ ] **R227-CR-2 — `internal/session/testutil.go` 无 build constraint（P2）**: TestProcess / NewTestProcess / InjectSession 进 prod binary。方案：加 `//go:build testing` 或重命名 *_test.go。重申 R226-CR-14。
- [ ] **R227-CR-5 — `dispatch.sendAndReply` 250+ 行 5+ 职责（P2）**: 错误处理、生命周期通知、事件跟踪、结果解析、图片读取、AskQuestion 抑制、文字分割。方案：拆 buildSendContext / handleGetOrCreateError / handleSendError / deliverResult。涉及 `internal/dispatch/dispatch.go:527`。重申 R219-CR-7。
- [x] **R227-CR-7 — `EventEntryFromEvent` Deprecated 但 process_extra_test.go 是唯一调用者（P3）**: 让 deprecated 函数长期存在。方案：迁测试到 EventEntriesFromEvent 后删除。 — 已修复（process_extra_test.go 两处调用迁到 EventEntriesFromEvent；router_shim.go 注释同步更名；deprecated wrapper 删除），本批 PR #166
- [x] **R227-CR-8 — `TodosDetailJSON` 二次 marshal（P2）**: ParseTodos Unmarshal 后又 Marshal。方案：直接抽取 block.Input 的 todos 字段原始字节。重申 R226-PERF-8。 — 已关闭（与 R226-PERF-8 / R228-PERF-2 同根因：`internal/cli/todo.go::ParseTodosWithRaw` 已返回 `(todos, rawTodos, ok)`，`process_event_format.go` TodoWrite 分支直接 `entry.Detail = string(rawTodos)` 不再走 `TodosDetailJSON` 二次 marshal。R226-PERF-8 落地于 PR #166，R228-PERF-2 同步关闭归档于 PR #183），本批 PR #188
- [x] **R227-CR-9 — `formatChineseDuration` 仅在 dispatch 包，cron 通知也需要（P3）**: 用户从 IM 看到中英不一致。方案：迁到 textutil/platform 公共包。 — 已修复（迁到 internal/textutil.FormatChineseDuration 公开 API；dispatch 改 import + 调用；测试随实现迁到 textutil 包），本批 PR #166
- [x] **R227-CR-13 — `TodosSummary` 用 emoji 字符（P3）**: 4 字节 emoji 在字段过短时可能截在非 emoji 边界。方案：评估 emoji 政策，必要时换 ASCII。 — 已修复（保留 emoji 但抽 5 个包级常量 todoStatusEmojiSummary/Done/Active/Pending/Unknown + todoStatusFieldSep；TodosSummary godoc 与常量块明示"下游必须按 rune 边界截断"契约 + 引用 textutil.TruncateRunes；新增 TestTodoStatusEmojiConstants_RuneBoundary 5 case 表锁单 rune + per-glyph 字节宽度 + TestTodosSummary_RuneBoundarySafe 走全分支 utf8.ValidString），本批 PR #183

### 架构 — 本轮新发现

- [ ] **R227-ARCH-1 — `session.Router` god-package：27 字段、80 方法跨 10 文件（P1）**: router-split refactor 只切了文件没切类型。方案：进一步拆 (*coreState, *lifecycleManager, *shimReconciler, *cleanupSweeper, *backendRegistry) 子结构 + facade，或承认现实合回 router.go。
- [ ] **R227-ARCH-2 — `cli.Process` god-struct：50+ 字段同一 RWMutex（P1）**: shimIO/turnState/procMeta/passthrough/heartbeat/watchdog/linker/快照同住一锁命名空间。方案：分 shimIO + turnState + procMeta 三子组件，Process 缩为组合体。
- [ ] **R227-ARCH-3 — `history.Source` 与 `cli.HistorySource` 双接口结构同形（P1）**: cli 不能 import history 又要 history factory 注册，新建 internal/wireup（或 historywire）包统一管 history factory 注册解套。
- [ ] **R227-ARCH-4 — `cli.backend.Profile` 与 `cli.detect.knownBackends + normalizeBackendID` 双源（P1）**: backend 元信息双轨；新加 backend 三处同步。方案：Profile 加 Aliases/IsDefault；knownBackends 从 backend.All() 派生。
- [ ] **R227-ARCH-5 — 4 个 consumer-side SessionRouter 接口农场（P2）**: dispatch/cron/upstream/server 各自声明，server 内多数 handler 仍裸 *session.Router。方案：抽 session.RouterCore 基础接口 + RouterReader 子集。重申 R215-ARCH-P2-4。
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
- [ ] **R227-ARCH-19 — `cli/process_*.go` 6 文件 godoc 都是 RFC "纯文件移动" 考古遗迹（P3）**: 让人误以为持续重构。方案：要么 R227-ARCH-2 完成真正切分，要么 RFC 标 ARCHIVED 后简化每文件 godoc。
- [ ] **R227-ARCH-20 — `dispatch.passthroughCtxKey/urgentCtxKey` 用 ctx.Value 跨包传 boolean（P3）**: Go 反模式（ctx.Value 应仅用于请求级元数据）。方案：定义 dispatch.SendOptions struct 或 functional options。Breaking。

### 配置 / 测试 — 本轮新发现

- [ ] **R227-CONFIG-1 — `JobUpdate.Notify` 无法 reset 回 nil（P3）**: 操作员想恢复 legacy-default 通知必须直接编辑 cron_jobs.json。方案：godoc 明确 work-around；或加 tri-state 字段。
- [ ] **R227-TEST-2 — `cli.detectVersion` 用 context.Background()（P3）**: SIGTERM 期间 --version probe 等满 5s。方案：NewWrapper 接 ctx 参数（Breaking，3-5 个调用点）。

## Round 228 — 5-agent 并行 review 第 40 轮（2026-05-20 第二批）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 90 条发现。
> 14 处 FIX-READY 已落地本轮 PR（详情见顶部摘要）。下面是需设计决策、breaking、跨包重构、或方案不唯一不适合本轮直接修的条目。

### Go 正确性 — 本轮新发现

- [x] **R228-GO-1 — `agent_tailer.DurationMS` `int64→int` 转换在 32-bit 平台溢出（P2）**: `time.Duration.Milliseconds()` 是 int64，向 `int` 转换在 32-bit Linux 上 ~24 天后会变负。方案：把 `AgentMetaPatch.DurationMS`、`SubagentInfo.DurationMS`、`node.AgentMetaPatch.DurationMS` 统一改为 int64（JSON 序列化无 breaking，但跨包字段类型变更）。涉及 `internal/server/agent_tailer.go:393` + `internal/cli/eventlog.go` + `internal/node/protocol.go`。 — 已修复（4 个跨包字段 int → int64：cli.EventEntry/SubagentInfo/TaskUsage 和 node.AgentMetaPatch；agent_tailer.go 移除 int(...) cast；JSON wire 兼容），本批 PR #185
- [x] **R228-GO-2 — `cron.AddJob` 多手动 Unlock 无 defer（P2）**: `s.mu.Lock()` 后多个手动 `s.mu.Unlock()` 提前 return（5+ 处），未来在 lock 段内插入新逻辑容易漏 Unlock。方案：refactor 为内层函数提取 + defer。涉及 `internal/cron/scheduler.go:814-856`。 — 已修复（抽 addJobLocked() 内层函数承担锁定段，defer s.mu.Unlock() 收敛所有 early-return 路径；外层 AddJob 仅做参数校验 + 锁外 save() / registerStub() 调用），本批 PR #181
- [ ] **R228-GO-3 — `reconnectShims` replay goroutine 无 ctx 绑定（P2）**: 重放段对每个 `task_started` 启动裸 goroutine 调 `linker.Resolve`，SIGTERM 时延迟 shutdown。方案：与 R227-GO-2 / R225-GO-2 合并，Resolve 接 ctx。涉及 `internal/session/router_shim.go:361-394`。

### 安全 — 本轮新发现

- [ ] **R228-SEC-1 — Dashboard 主页 CSP `script-src 'unsafe-inline'`（P2 R227-SEC-9 重申）**: 主页用 `'unsafe-inline'` 而 login 页已用 hash-based CSP 收敛。方案：为响应生成 nonce + 内联 `<script>` 注入 nonce；或迁移内联脚本到外部文件。Breaking：需改 HTML 模板。涉及 `internal/server/dashboard.go:390`。
- [ ] **R228-SEC-2 — `serveRender` `os.Open(resolved)` 二次打开 inode-swap TOCTOU（P1 R227-SEC-2 重申）**: `handleFileGet` Lstat 后 `serveRender` 再次 Open，存在窗口可被符号链接替换。方案：在 Lstat 之后立即 Open 拿 fd 传入下游函数；或用 `f.Stat()` 比对 inode。涉及 `internal/server/project_files.go:590,684,762,856`。
- [x] **R228-SEC-3 — `mode=download` 路径未 blocklist `.env`/`.npmrc`/`.netrc`/`*.pem`/`*.key`（P3 R227-SEC-12 重申）**: 认证用户可下载 workspace 内任意敏感配置。方案：`serveDownload` 入口对 `filepath.Base(path)` 做 blocklist 检查；或文档化 `allowed_root` 必须不含敏感文件。涉及 `internal/server/project_files.go`。 — 已关闭（与 R227-SEC-12 同根因，commit 61cde08 R229-SEC-9 已实施 isSensitiveDownloadName + sensitiveDownloadNames/Exts 双表 + 403 阻断），本批 PR #188
- [ ] **R228-SEC-4 — `/health` 端点无 rate limiting（P3 R227-SEC-10 重申）**: 未认证响应含 version 字段可 fingerprint。方案：per-IP rate limiter 60/min，或把 version 移到认证区段。涉及 `internal/server/server.go:809`、`internal/server/health.go:169`。
- [ ] **R228-SEC-5 — WS Hub 同一 session key 订阅数量无 per-key 上限（P3 R227-SEC-11 重申）**: 单 token 开 100 个 WS 各订同 key 触发 100 路 fan-out。方案：维护 `keySubCount map[string]int` + 阈值（如 20）。涉及 `internal/server/wshub.go`。
- [ ] **R228-SEC-6 — `serveRender`/`serveRaw` sandbox CSP `style-src 'unsafe-inline'`（P3 R226-SEC-9 重申）**: CSS-based exfiltration 攻击面。方案：nonce 化或去掉 unsafe-inline。涉及 `internal/server/project_files.go:731,~905`。

### 性能 — 本轮新发现

- [x] **R228-PERF-1 — `eventlog_bridge.newEventLogSink` 单条 Append 路径每次 `make([]persist.Entry, 0, 1)`（P1 与 R226-PERF-2 同根）**: 单条 Append 路径每次 1 entry 仍分配一个 1-cap slice。方案：栈局部 `[1]persist.Entry` 数组 + slice。涉及 `internal/session/eventlog_bridge.go:77`。 — 已修复（len(entries)==1 fast path 用 [1]persist.Entry 栈数组 + stackArr[:0]；多条路径不变；persisterSink closure escape 仍存因函数值无法静态证明 retain，但省 defer + for 控制流，为未来 PersistSink.AppendOne 单值契约打底），本批 PR #185
- [x] **R228-PERF-2 — `TodoWrite` 双 marshal（decode + recode）（P1）**: `EventEntriesFromEventAt` 对 TodoWrite 调 `ParseTodos` 再调 `TodosDetailJSON`，原始 `block.Input` 已是 `{"todos":[...]}`。方案：从 `block.Input` 提取 "todos" raw bytes 直接赋给 `entry.Detail`。涉及 `internal/cli/todo.go:40-46` + `internal/cli/process_event_format.go:178-190`。 — 已确认归档（R226-PERF-8 PR #166 已落地：todo.go:52 `ParseTodosWithRaw` 返回 `(todos, rawTodos, ok)`；process_event_format.go:165-176 在 TodoWrite 分支直接 `entry.Detail = string(rawTodos)`，不再走 `TodosDetailJSON` 二次 marshal。本批 R228-PERF-2 与 R226-PERF-8 同根因，关闭归档），本批 PR #183
- [ ] **R228-PERF-3 — `subagent_transcript.readLocked` 每次 `os.Open` 不复用 fd（P2）**: 每 200ms × 50 active tailer = 250 open/close fd/s。方案：缓存 `*os.File`，Tail 用 Seek 复用，inode 变化时重 Open。涉及 `internal/cli/subagent_transcript.go:63-88`。
- [x] **R228-PERF-4 — `protocol_acp.WriteMessage` 每条消息 `map[string]any` 逐张图 alloc（P2）**: 文本+单图常见路径 2 个 map alloc。方案：定义 `acpImageBlock` 具体结构体；prompt 预分配。涉及 `internal/cli/protocol_acp.go:234-258`。 — 已修复（acpImageSource / acpPromptBlock / acpPromptParams 三个具名 struct 替换 map[string]any + map[string]string 字面量；text block Text 用 *string 让 omitempty 在空文本仍发 `"text":""` 保字节兼容；prompt cap=len(images)+1 一次预分配；新增 protocol_acp_writemsg_test.go 4 case 锁 wire 形状），本批 PR #188
- [ ] **R228-PERF-5 — `agent_tailer.pollOnce` fan-out 时每 subscriber 各自 marshal（P2 与 R225-PERF-9 同类）**: 同一事件 N 次 marshal。方案：fan-out 前一次 `marshalPooled`，改用 `SendRaw`。涉及 `internal/server/agent_tailer.go:338-358`。
- [ ] **R228-PERF-6 — `handleList` `resp` 用 `map[string]any` 而非具体结构体（P2）**: 1 Hz poll 每次 1 个 map。方案：定义 `sessionsResponse` struct + omitempty。涉及 `internal/server/dashboard_session.go:535`。
- [ ] **R228-PERF-7 — `EventLog.Append` `[]EventEntry{e}` 字面量 heap escape（P3 R219-PERF-4 具体修法方向）**: 单条 slice 字面量逃逸。方案：先 `-gcflags=-m` 验证再决定栈数组+切片或 sync.Pool。涉及 `internal/cli/eventlog.go:703`。

### 代码质量 — 本轮新发现

- [x] **R228-CR-1 — `maxScannerBufBytes=10MB` 与 shim `maxServerLineBytes=16MB` 不一致（P2）**: 10-16MB 之间合法事件被静默丢弃。方案：加 godoc 解释 6MB headroom，或对齐到 16MB。涉及 `internal/cli/process.go:30`。 — 已修复（godoc 解释 6MB headroom：shim 自身在 16MB 拒绝行，naozhi 永远不会看到 10-16MB 的"合法但被丢"行；headroom 留给协议帧 + base64 图像 tool_result），本批 PR #169
- [x] **R228-CR-2 — `Caps.SoftInterrupt`/`Priority`/`StreamJSON` 三个字段被填但从未读（P2）**: 只有 Replay 被读。方案：删除三个 dead 字段并修 Capabilities 实现；或 godoc 标 reserved。涉及 `internal/cli/protocol.go:95-100`、`protocol_claude.go:137`、`protocol_acp.go:337`。 — 已修复（godoc 标 reserved + 指向 protocol_caps_test.go 锁定的契约；不删字段保留 forward-compat anchor），本批 PR #169
- [x] **R228-CR-3 — `isActivityType` 与 `EventLog.Append` activity 集无编译期 sync 保护（P2）**: 注释说"两边必须一起改"但无 contract test/共享函数。方案：抽 `cli.IsActivityType(t string) bool` 共享；或加 contract test。涉及 `internal/session/managed.go:1483-1488` + `internal/cli/eventlog.go:681`。 — 已修复（新增 internal/cli/event_kinds.go 暴露 IsActivityType 共享 helper + event_kinds_test.go 锁定 15 case 表；EventLog.Append/AppendBatch 与 session.scanLastSummaries 三处调用点切换；删除 session 包私有 isActivityType 19 行），本批 PR #181
- [x] **R228-CR-4 — `Process.LastEntryOfType` + `EventLog.LastEntryOfType` 导出但无 prod 调用（P3）**: 应 unexport 或加到 processIface。涉及 `internal/cli/process_event_query.go:188-191`、`internal/cli/eventlog.go:1058`。 — 已修复，本批 PR #170
- [x] **R228-CR-5 — `cron/job.JobTitleOrFallback` `[]rune(line)` heap alloc（P3）**: 与 textutil.TruncateRunes 重叠但用 `…`(U+2026)。方案：要么接受 ASCII `...` 后缀改用 textutil；要么 textutil 加 ellipsis 参数 overload。涉及 `internal/cron/job.go:205-209`。 — 已修复（改用 textutil.TruncateRunesNoEllipsis 复用 byte-level 解码 + 短路快路径；通过 truncated != line 判定后本地补 U+2026 保留卡片视觉一致性），本批 PR #169

### 架构 — 本轮新发现

- [ ] **R228-ARCH-1 — `session` 包既通过 `cli.Wrapper.ShimManager` 又直接调 `shim.SocketPath/KeyHash/WaitSocketGone` 双重接入（P1）**: 抽象塌陷。方案：把三个 shim 调用收进 `cli.Wrapper.WaitSessionShimGone(key)`。涉及 `internal/session/router_lifecycle.go:1115-1116` + `router_shim.go:27,57-72,146-180`。
- [ ] **R228-ARCH-2 — `cli.Wrapper.ShimManager` 公开字段穿透到 session（P1）**: 应代理 `Discover/Reconnect` 等方法。Breaking（公开字段消失）。涉及 `internal/cli/wrapper.go:38` + `internal/session/router_backend.go:151`。
- [ ] **R228-ARCH-3 — `server/wshub` 直接持 `*cli.SubagentLinker` 指针长寿命缓存（P1 与 RFC v4 phase 3+ TODO 同根）**: Linker 重建后旧 map key 残留为 GC root。方案：session 层暴露 `WireLinkerOnce(key, ...)` API，把指针弱引用封进 session 包。涉及 `internal/server/wshub.go:165` + `internal/server/dashboard_agent_events.go:66,72,80`。
- [ ] **R228-ARCH-4 — `cli.AskQuestion`/Item/Opt 与 `platform.QuestionCard`/Item/Option 双套结构体（P2）**: dispatch 手工字段拷贝，加字段易漏。方案：抽到共享包（如新建 `internal/askq` 或 `internal/eventlog/schema`）。涉及 `internal/cli/event.go:141-166` + `internal/platform/platform.go:108-141`。
- [ ] **R228-ARCH-5 — `cli/image.go MimeFromPath/ExtractImagePaths/safeImageDirs` 与 `platform.ImageExt` 重叠（P2）**: cli 包混入了与协议无关的 MIME/安全目录工具。方案：抽到 `internal/imageutil` 或 `internal/osutil`。涉及 `internal/cli/image.go:61-77`。
- [x] **R228-ARCH-6 — 3 份 `jitterBackoff` wrapper 全是 `osutil.JitterBackoff` 16-行 stub（P2）**: 包内私有 wrapper 无任何价值。方案：删 3 个 wrapper + 等价测试，调用方直接调 osutil。涉及 `internal/node/backoff.go`、`internal/upstream/backoff.go`、`internal/platform/platform.go:289-291`。 — 已修复，本批 PR #170
- [ ] **R228-ARCH-7 — `processIface` 32-method 胖接口 + 内部强转回 `*cli.Process`（P2）**: 抽象漏了。方案：要么删 interface 直接用 `*cli.Process`；要么拆成 3 个小接口。需设计决策。涉及 `internal/session/managed.go:33-102` + `router_lifecycle.go:829`。
- [ ] **R228-ARCH-8 — 4 个 platform adapter 各自 `var fooHTTPClient` SSRF-defense client（P2）**: 4 份近一致的 redirect+TLS 1.2 floor client。方案：`internal/platform.NewSafeHTTPClient(timeout)` helper。涉及 feishu/discord/weixin/slack 各自顶部 var。
- [x] **R228-ARCH-9 — `dispatch.MaxCoalescedTextBytes()` 被 upstream 反向调用作 RPC 入口大小限制（P2）**: upstream → dispatch 反向依赖只为复用一个常量。方案：抽到 `internal/limits` 包。涉及 `internal/upstream/connector_rpc.go:129`。 — 已修复（新增 internal/limits 叶子包持 MaxCoalescedText=4MB；dispatch.maxCoalescedTextBytes 私有 alias 等于 limits.MaxCoalescedText，hot loop 读法不变；upstream 直接 import limits 并删 dispatch import；dispatch.MaxCoalescedTextBytes() 函数无 caller 自然消失），本批 PR #186
- [x] **R228-ARCH-10 — `dispatch/commands.go` 直接构造 `cron.Job{}` 字面量（P2）**: 字段名变更 → dispatch 编译挂。方案：cron 包提供 `cron.NewJob(schedule, prompt, ctx)`。涉及 `internal/dispatch/commands.go:344-351`。 — 已修复（cron 新增 JobIMContext 小聚合体（Platform/ChatID/ChatType/CreatedBy）+ NewJob(schedule, prompt, ctx) 构造器；CreatedAt 留给 AddJob 唯一持久化入口盖戳避免 missed-schedule 漂移；dispatch/commands.go 改用 cron.NewJob），本批 PR #186
- [ ] **R228-ARCH-11 — `dispatch.SessionGuard` interface 实际不做多态分发（P2）**: `if d.queue != nil ... else d.guard ...` 是 either-or。方案：删 interface，用具体类型。涉及 `internal/dispatch/dispatch.go:23-35`。
- [ ] **R228-ARCH-12 — `cron.SchedulerConfig` 直接持 `session.AgentOpts` + `platform.Platform`（P2）**: cron 字段调整波及 cron。方案：cron 加自己的 JobNotifier interface + JobAgentOpts 局部类型。涉及 `internal/cron/scheduler.go:100-101,213-214`。
- [ ] **R228-ARCH-13 — `cli.HistoryFactoryFn` registry blank import 在 session 包（P2）**: 触发点已迁到 `cli.NewWrapper` 但 import 列表残留在 session。方案：移到 cli/wrapper.go 或 cmd/naozhi/main.go。涉及 `internal/session/router_core.go:21-32`。
- [ ] **R228-ARCH-14 — `dispatch.Dispatcher.takeoverFn`/`sendFn` closure 字段易漏 wireup（P2）**: closure-pattern 经典毛病。方案：1-method interface。Breaking：内部 wiring。涉及 `internal/dispatch/dispatch.go:82-83`。
- [x] **R228-ARCH-15 — `cli.NewWrapper` `backendDisplayName` 与 `normalizeBackendID` 顺序导致 case 不一致（P3）**: "Kiro" 走 default 显示原值，"kiro" 收敛到小写。方案：先 normalize 再 displayName。涉及 `internal/cli/wrapper.go:55-87`。 — 已修复（backendDisplayName(id) 喂规范化后的 id，新增 TestNewWrapper_DisplayNameMatchesNormalizedID 表驱动 6 case 锁契约），本批 PR #169
- [x] **R228-ARCH-16 — `parseVersionOutput` 在 wrapper.go 但属 detect 概念（P3）**: 应搬到 detect.go。涉及 `internal/cli/wrapper.go:177-187`。 — 已修复（函数 + 表驱动测试整批迁到 detect.go / detect_test.go；wrapper.go 只留 normalize/displayName/detectCLI/candidatePaths），本批 PR #169
- [x] **R228-ARCH-17 — `wshub` 持整个 `*cron.Scheduler` 仅为调 `EnsureStub`（P3）**: 耦合面 300+ 方法。方案：定义 `cronStubChecker` interface（1 method），Hub 字段改 interface。涉及 `internal/server/dashboard_session.go:713`。 — 已修复（dashboard_session.go 内定义 cronStubChecker { EnsureStub(key string) bool } + SessionHandlers.scheduler 字段类型收窄到接口；*cron.Scheduler 隐式满足，server.go 构造点零改动；goimports 自动移除 cron import。Hub.scheduler 仍是 *cron.Scheduler，因调用面 >1 方法，不在本轮范围），本批 PR #185
- [ ] **R228-ARCH-18 — `dispatch.Dispatcher.projectMgr` 仅用于 slash-command UX 但持整个 `*project.Manager`（P3）**: 30+ 方法面。方案：内部 1-method interface 注入。涉及 `internal/dispatch/dispatch.go:56`。

## Round 230C — PR #198 详细归档 NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共 82 条发现，PR #198 同批的另一组细化条目（编号集 C 区分 line 211 的核心 R230 节与 line 277 的 R230B 节）。
> 以下条目为非 breaking 但需要更大改动 / 需设计决策 / 跨模块的发现，登记追踪。

### Security（剩余）

- [ ] **R230C-SEC-7 — dashboard_token 8-15 char 公网监听只 Warn（P2）**: `cmd/naozhi/main.go:938` 公网部署时 8 字节 token 仍允许启动只 slog.Warn。方案：监听非 loopback 时最小长度提升到 16 + slog.Error+os.Exit(1)，或加 entropy 估算拒字典词。Breaking：是（部分公网部署需调长 token）。
- [ ] **R230C-SEC-10 — cron notify_chat_id 发送时未再次校验（P3）**: `internal/cron/scheduler.go` notifier 路径直接读 in-memory `j.NotifyChatID` 调 platform.Reply。`cron_jobs.json` 被篡改后绕过 validateNotifyTarget。方案：executeOpt 发 notify 前再调 validateNotifyTarget，或在 scheduler load 期 round-trip 校验一遍。Breaking：否。
- [ ] **R230C-SEC-11 — runsLimiter nil-guard 静默放行（P3）**: `internal/server/dashboard_cron.go:1029` `if h.runsLimiter != nil` 在测试桥接合理，但 `server.New` 重构漏 wire 时 silently 退化为无限速。方案：buildServer 构造 cronH 后 assert `h.runsLimiter != nil` when scheduler != nil。Breaking：否。
- [ ] **R230C-SEC-12 — GET /dashboard 未认证路径不限速（P3）**: `internal/server/dashboard.go:376` 未登录用户可无限调 `/dashboard` 触发 login 模板渲染。方案：未认证 path per-IP 30/min 限速器（与 wsUpgradeLimiter 同级）。Breaking：否。

### Go 正确性 / 并发（剩余）

- [ ] **R230C-GO-1 — SnapshotChainIDs `historyMu` 实际不保护 prevSessionIDs（P1）**: `internal/session/managed.go:368-385` reader 取 historyMu.RLock 读 `prevSessionIDs`，但 writer（registerStub / spawnSession / RenameSession）在 r.mu 下写而不持 historyMu。方案：要么写侧补 historyMu.Lock，要么把 SnapshotChainIDs 改用 r.mu.RLock 并修正注释。Breaking：否。
- [ ] **R230C-GO-4 — spawnSession inline history load 同步 IIFE 包了 historyWg（P2）**: `internal/session/router_lifecycle.go:699-711` `historyWg.Add(1)/Done()` 套在直接调用的 `func(){…}()` 上，对 Shutdown.Wait 是 noop。方案：若意图异步则补 `go`；若意图同步则删 historyWg 包装并加注释。Breaking：否。
- [ ] **R230C-GO-7 — executeOpt sendCtx 用 context.Background 让 5h 任务无法 shutdown 期取消（P2）**: `internal/cron/scheduler.go:1955` 5h execTimeout 的 cron 任务 stopCtx fire 后仍跑满。方案：deadline watchdog 扩展为 stopCtx 触发也调 InterruptViaControl，或 sendCtx 改用 `context.WithTimeout(s.stopCtx, jobTimeout+grace)`。Breaking：否。
- [ ] **R230C-GO-8 — finishRun bumpRunStateMetrics 在 persist 回滚前已计数（P2）**: `internal/cron/scheduler.go:2103` persist 失败时 in-memory job 字段回滚但 CronRunSucceededTotal 已 +1。方案：bumpRunStateMetrics 移到 recordResultP0WithSanitised 返回 ok 之后。Breaking：否。
- [ ] **R230C-GO-10 — spawnSession 用 caller ctx 而非 r.historyCtx（P3）**: `internal/session/router_lifecycle.go:702` HTTP 短超时上下文取消会让 session 历史加载半截。方案：`context.WithTimeout(r.historyCtx, 15*time.Second)`。Breaking：否。
- [ ] **R230C-GO-13 — runstore cacheHeadPush 每次 prepend O(N) copy（P3）**: `internal/cron/runstore.go:232` keepCount=200 时每个 Append 都 200-element copy。方案：改用 oldest-first 内部存储，copySummariesLocked 时反转。Breaking：否（内部）。
- [ ] **R230C-GO-15 — emitOverlapSkipped CronRunStartedTotal 与正常路径计数顺序不一致（P3）**: `internal/cron/scheduler.go:2236` vs `1805`。方案：把 `CronRunStartedTotal.Add(1)` 收敛到 emitRunStarted 内一处。Breaking：否。
- [ ] **R230C-GO-18 — registerStub 用 chainIDs 完全替换 prevSessionIDs（P2）**: `internal/session/router_discovery.go:454` 每次重新注册 stub 把 fresh_context=false 累积的多 session 链折成 1 element。方案：existing chain 非空时只追加，不替换。Breaking：否。

### Performance（剩余）

- [ ] **R230C-PERF-1 — connector_subscribe 每次 notify 都 Snapshot()（P1）**: `internal/upstream/connector_subscribe.go:74` 每事件 10 atomic.Load + SetModel 副作用，仅为读 State。方案：为 ManagedSession 提供轻量 `State() string` / `DeathReason() string`。Breaking：否（新增方法）。
- [ ] **R230C-PERF-2 — eventlog.Append 单条路径每次分配 `[]EventEntry{e}`（P1）**: `internal/cli/eventlog.go:736` 字面切片在 atomic.Pointer sink 调用上逃逸到堆，5-50 events/s × N session 持续 alloc。方案：EventLog 字段缓冲 `sinkOneBuf [1]EventEntry`（Append 已串行）替代 sync.Pool。Breaking：否。
- [ ] **R230C-PERF-4 — handleSubscribe per-key 限额线性扫描全部连接（P2）**: `internal/server/wshub.go:570` h.mu.Lock 下 O(connections × key) 扫描。方案：维护 `subscriberCounts map[string]int` O(1)。Breaking：否。
- [ ] **R230C-PERF-6 — completeSubscribe 调两次 Snapshot()（P2）**: `internal/server/wshub.go:702/720` 复用同一 snap 即可。方案：在调用点共享 snap，740 行使用包级 `var emptyEntries []cli.EventEntry`。Breaking：否。
- [ ] **R230C-PERF-7 — handleList 每次重建 projectList slice（P2）**: `internal/server/dashboard_session.go:527-576` 1Hz × N tab 持续分配，project list 实际分钟级变化。方案：`initStaticStats` 或 rescan 时缓存。Breaking：否。
- [ ] **R230C-PERF-8 — resubscribeEvents 每轮 h.mu.RLock + map read 检查 subGen（P2）**: `internal/server/wshub.go:1159` 12 iter × N 客户端瞬间死亡触发并发。方案：用已传入的 gen 参数局部变量比较，免锁。Breaking：否（内部函数）。
- [ ] **R230C-PERF-10 — connector_subscribe Snapshot+EventEntriesSince 双取 eventlog.mu（P3）**: `internal/upstream/connector_subscribe.go:60-79` 高活跃会话每事件持锁两次。方案：与 R230C-PERF-1 联合用 State() + lastState 缓存跳过无变化。Breaking：否。
- [ ] **R230C-PERF-12 — EventLog.Subscribe map 不收缩、无初始容量（P3）**: `internal/cli/eventlog.go:911-912` CloseSubscribers nil 后下次 Subscribe 1→2→4 growth rounding。方案：const subscribersMapInitCap=4 显式预分配；与 R229-PERF-12 联合实现 sync.Pool。Breaking：否。

### Code 质量（剩余）

- [ ] **R230C-CR-1 — recordResult 变成测试-only 死代码（P2）**: `internal/cron/scheduler.go:2390` 生产路径走 recordResultP0WithSanitised；recordResult 仅 persist_failure_test.go 两处调用。方案：重写测试调 P0 版后删 recordResult；或保留并明确标 test-helper-only。Breaking：否。
- [ ] **R230C-CR-2 — computeJobTimeout schedule 参数明确忽略（P2）**: `internal/cron/job.go:277` 函数签名带 schedule 参数但 `_ = schedule`。方案：删除函数 inline `s.execTimeout` 到两处 caller，或保留但去 schedule 参数。Breaking：否（unexported）。
- [ ] **R230C-CR-3 — addJobLocked 自己上锁违反 *Locked 命名约定（P2）**: `internal/cron/scheduler.go:846` 其他 `*Locked` 函数（pause/resume/persist）都遵守 caller-holds-lock。方案：重命名为 addJob，或挪出锁让 caller 持。Breaking：否。
- [ ] **R230C-CR-4 — TriggerNow entryID==0/!=0 两个 goroutine 体几乎相同（P2）**: `internal/cron/scheduler.go:1423-1488` 两条 60 行近一致路径。方案：抽 `triggerNowExecute(jobID string)` 共享。Breaking：否。
- [ ] **R230C-CR-5 — ManagedSession 三个 SessionID 访问点（getSessionID/SessionID/GetSessionID）（P2）**: `internal/session/managed.go:727-735` cli.HistorySessionView 与 processIface 各要一份。方案：godoc 集中说明三访问点关系，或合并到一个主入口。Breaking：否。
- [ ] **R230C-CR-7 — executeOpt 错误分类逻辑散在 4 个 finishRun 分支（P2）**: `internal/cron/scheduler.go:1760-2053` `(state, errClass)` 映射内联。方案：抽 `classifySendError(err) (RunState, ErrorClass)` ~15 行。Breaking：否。
- [ ] **R230C-CR-8 — registerStub vs registerStubByValue 双轨（P2）**: `internal/cron/scheduler.go:641-663` 仅参数风格不同。方案：folder 成 1-line wrapper。Breaking：否。
- [ ] **R230C-CR-11 — recordResultP0 RunCounters.addRun 不在 persist 失败时回滚（P3）**: `internal/cron/scheduler.go:2304` 计数+1 在 persist 检查前；marshal 失败后 in-memory 字段回滚但 counter 没回。方案：addRun 移到 perr==nil 分支后。Breaking：否。
- [ ] **R230C-CR-Diag — Snapshot godoc 未声明读侧 SetModel 副作用（P3）**: `internal/session/managed.go:854` 与 R226-CR-13 内联注释呼应，但方法 godoc 未提示。方案：godoc 加 "Note: side-effect mirrors live model into persisted field; see SnapshotReadOnly future variant"。Breaking：否（与 R229-GO-2 合并）。

### Architecture（剩余）

- [ ] **R230C-ARCH-1 — DESIGN.md 第 5 节 HTTP Server 描述与现实严重失真（P1）**: `docs/design/DESIGN.md:492-516` 说 server 只注册 /health；实际 92 个文件 / 60+ 路由。方案：DESIGN.md 加"实际 vs 理想"对比 + ADR 链接 server-split RFC。Breaking：否。
- [ ] **R230C-ARCH-2 — dispatch/cron/upstream/sysession 各自定义 SessionRouter 接口（P1）**: 4 包同名 `SessionRouter` 各持不同方法集。方案：约定 `XxxSessionRouter` 命名空间 + `internal/session/contracts/` 集中接口约束。Breaking：是。
- [ ] **R230C-ARCH-3 — cron Scheduler 直持 platforms map 越层调 Reply（P1）**: cron 已悄悄成为第二个 dispatch（持 Agents map / 复刻 retry+SplitText）。方案：cron 内禁用 platform.Platform，强制走 dispatch facade。Breaking：是（与 R219-ARCH-8 合并）。
- [ ] **R230C-ARCH-4 — processIface god interface 拆分优先级（P1）**: `internal/session/managed.go:35-102` 30+ 方法含 dashboard-only 字段。方案：先剥 dashboard 字段到 ProcessIntrospector，process 核心剩 8-10 方法。Breaking：是。
- [ ] **R230C-ARCH-5 — 保留 key 命名空间策略表分散在 5 处（P1）**: cron:/project:/scratch:/sys: 在 reservedKeyPrefixes/exemptKeyPrefixes/saveStore/Cleanup/Sidebar 各列。方案：`KeyKind enum + Policy struct` 单一事实源 + vet 阻断。Breaking：否。
- [ ] **R230C-ARCH-6 — 4 个 platform adapter 各自 8KiB inbound text byte cap（P2）**: feishu/slack/discord/weixin 各自 maxIncomingTextBytes=8KiB。方案：`platform.DefaultMaxIncomingBytes`（与 DefaultMaxReplyLen 同级）。Breaking：否。
- [ ] **R230C-ARCH-7 — slack/discord 各写一份 messageRef codec（P2）**: 都用 `strings.SplitN(msgID, ":", 2)`。方案：`platform.CompositeMessageID{ChatID, MsgID}` + Encode/Decode。Breaking：否。
- [ ] **R230C-ARCH-9 — Platform caps 抽象不到位（P2）**: `cli.ProtocolCaps` 已聚合，但 Platform 仍混用 `MaxReplyLength` 值 / `SupportsInterimMessages` bool / `AsReactor` 接口断言三种返回风格。方案：`PlatformCaps` 结构体聚合（与 R229-ARCH-6 合并）。Breaking：是。
- [ ] **R230C-ARCH-10 — Hub 三 setter vs SysessionMgr 字段注入风格不一（P2）**: `wshub.go:282-291` SetScheduler/SetUploadStore/SetScratchPool 是 setter；ServerOptions.SysessionManager 是字段。方案：四个一律走 HubOptions required 字段。Breaking：否（与 R219-ARCH-5 合并）。
- [ ] **R230C-ARCH-11 — Session mode 4 态文档 vs 实际 7+ 态无单一权威类型（P2）**: `cli.ProcessState` 4 态 + ManagedSession.exempt + key 前缀派生 stub + process==nil 派生 paused。方案：`session.SessionMode enum {Active, Stub, Paused, Scratch, Exempt}` 正交叠加 + 类型化 transitions。Breaking：是。
- [ ] **R230C-ARCH-13 — scratchPool 与 router.sessions 双池强迫 Hub 分流（P2）**: `internal/session/scratch.go` + `dashboard_scratch.go` 每加一个 scratch 操作两路径都得复刻。方案：合到 sessions map + Tag=Scratch 或文档化双池约束。Breaking：是。
- [ ] **R230C-ARCH-14 — 文件化状态多实例并发写无 flock（P2）**: 6 个独立 atomic write store 假设单实例独占。方案：state file 加 `flock(LOCK_EX) + writer_pid + writer_host + generation`。Breaking：是。
- [ ] **R230C-ARCH-15 — main.go ~390 行 backend-specific settings.json 重写（P2）**: kiro backend 不需要。方案：抽 `BackendProfile.PrepareEnv()`，main 只调 profile。Breaking：否（与 R229-ARCH-7 合并）。
- [ ] **R230C-ARCH-17 — DESIGN.md 缺 Backend Extension Points 节（P3）**: System Session（已有 internal/sysession）/ Cron Dashboard（Phase 5 已完成）/ Gemini CLI 集成均无章节。方案：DESIGN.md 加 "Backend Extension Points" 节列 4 接口（Profile / Protocol / HistorySource / shim hint）。Breaking：否。
- [ ] **R230C-ARCH-18 — Version 双语义（DataVersion vs RenderVersion）无 godoc（P3）**: `router_core.go:1071/1086` Version() 同名两用法。方案：godoc 加两种语义说明，pending 真正拆 counter。Breaking：否（与 R229-ARCH-20 合并）。

