# TODO

> 最后更新 2026-05-17 Round 219 —— 深度 5-agent 并行 review 第 33 轮：13 处 FIX-READY 落地（uuid hex stack-array / eventlog_bridge pooled encoder / EventEntries empty fast-path / pendingIdx flush cap shrink / config 0o077 perm mask / interruptAcquireTimeout 命名 / SessionConfig.Workspace godoc Deprecated / cron Sprintf / maxWebhookNonceLen 命名 / discovery ErrUnsupportedPlatform 单点 / dashboard_send.go path.Clean 注释）+ ~30 NEEDS-DESIGN 归档见 Round 219 节。
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
- [ ] **R219-SEC-3 — `shimEnvAllowedPrefixes` 把 `ANTHROPIC_API_KEY` 泄漏到 Bedrock 部署的 shim 子进程（P2）**: `manager.go:900` 含通配前缀 `"ANTHROPIC_"`，Bedrock 部署不需要 `ANTHROPIC_API_KEY` 但仍然进 shim env，Bash tool 调用可 `env | grep ANTHROPIC` 拿到。方案：替换为显式条目 `"ANTHROPIC_AUTH_TOKEN="`、`"ANTHROPIC_MODEL="`，或 Bedrock 模式下排除 `ANTHROPIC_API_KEY`。涉及：`internal/shim/manager.go:900`。
- [ ] **R219-SEC-4 — KaTeX CDN 加载无 SRI integrity hash（P2）**: `dashboard.js loadKatex()` 动态注入 KaTeX `<link>`/`<script>` 但无 `integrity=` SRI；CDN 被攻陷可注入恶意 `renderToString`。主 CDN 已有 SRI（CSP 注释），KaTeX 应一致。方案：在 loadKatex 动态创建的元素加 SRI 哈希。涉及：`internal/server/static/dashboard.js:2161,8651,6403`。
- [ ] **R219-SEC-5 — `moveToShimsCgroup` 用 `Hello.CLIPID` 经 sudo 把任意 PID 移入 cgroup（P2）**: `manager_linux.go:72` 把 shim 自报的 CLIPID 经 strconv.Itoa 传给 `sudo busctl`，shim 被劫持可上报任意 PID。方案：通过 `/proc/<CLIPID>/status` 验证 PPid 等于 shim 实际 PID。涉及：`internal/shim/manager_linux.go:72`。

### 性能 — 本轮新发现 / 重申

- [ ] **R219-PERF-1 — `eventPushLoop` 同 session N tab 各自 marshalPooled 独立序列化（P2 重申 R214-PERF-4）**: 同 key 多客户端 fan-out 时未做单次序列化共享。方案：在 eventPushLoop 把同 key 全部 clients 集中在 broadcast goroutine 里序列化一次再 fan-out SendRaw。
- [ ] **R219-PERF-2 — `handleList` storeGen 未变化时未短路重建 sessionWorkspaces map（P2）**: 1 Hz × N tab × N session 重建 map 浪费。方案：引入 `lastListVersion uint64 + lastListJSON []byte` 缓存，命中时直接 ResponseWriter.Write。涉及：`internal/server/dashboard_session.go:313`。
- [ ] **R219-PERF-3 — `Snapshot()` 顺序读 8 次 atomic.Pointer.Load（P2 重申 R215-ARCH-P2-7）**: 1 Hz × 10 tab × 50 session = 4000 Load/s。方案：把构造后不变字段（backend/cliName/cliVersion/userLabel）打包 `immutableBox struct` + 单次 atomic.Pointer.Load。涉及：`internal/session/managed.go:861`。
- [ ] **R219-PERF-4 — `invokePersistSinkSingle` 栈数组方案需 benchmark 证伪（P2）**: Append 单条路径 `[]EventEntry{e}` heap escape，本轮尝试用 `[1]EventEntry` 栈数组 + `s[:]` 但因 PersistSink 是 atomic.Pointer 函数指针调用，逃逸分析会强制 slice 逃逸到 heap，理论收益不确定。需 -benchmem 验证；若无效则降级为接受现状或换 sync.Pool 方案。涉及：`internal/cli/eventlog.go:640`。

### 代码质量 — 本轮新发现

- [ ] **R219-CR-1 — `loadAtomicString`/`storeAtomicString` (`cli/eventlog.go`) 与 `loadStringAtomic`/`storeStringAtomic` (`session/managed.go`) 双胞胎（P2）**: 两个函数语义相同但跨包独立，命名词序还相反，新加调用易再造第三个变体。方案：抽到 `internal/syncutil` 或 `internal/textutil`，统一命名。Breaking：否。
- [ ] **R219-CR-2 — `cron.runeByteOffset` 重复实现 `textutil.TruncateRunes`（P2）**: `scheduler.go:1689` 自实现 14 行 rune-counting 循环，cron 已 import session→textutil。方案：删除 `runeByteOffset`，调用 `textutil.TruncateRunes`。Breaking：否。
- [ ] **R219-CR-3 — `feishu/askquestion.go::truncateRunes` 重复实现 `textutil.TruncateRunes`（P2）**: 8 处调用点。方案：替换为 `textutil.TruncateRunes` 或新增 `TruncateRunesSilent` 不带 ellipsis 变体。需审计 8 处对 ellipsis 的依赖。
- [ ] **R219-CR-4 — `cron.scheduler` `*ByID`/`*` 三对方法 body 重复（P2）**: DeleteJob/PauseJob/ResumeJob 与 *ByID 各 6 个共享 lock+mutate+persist+unlock body，仅 lookup 步骤不同。方案：抽 `deleteJobLocked(j *Job) (saveFunc, error)` 共享。
- [ ] **R219-CR-5 — `dashboard_cron.go` 5 个 validateCron* 重复 UTF-8 + C0 + IsLogInjectionRune 三重扫描（P2）**: 任何安全策略变更需改 5 处。方案：抽 `validateStringField(s, maxBytes, allowedCtrl)` helper，每个 validator 3 行 wrapper。
- [ ] **R219-CR-6 — `cron.PreviewSchedule` 包级函数 vs `(*Scheduler).PreviewSchedule` 双轨（P2）**: 包级版用 UTC，方法版用 scheduler tz；divergence 易踩。方案：折叠 nil scheduler 守卫到方法或包级版改 unexported。
- [ ] **R219-CR-7 — `dispatch.sendAndReply` 241 行 5+ 职责（P2）**: 类同 R214-CODE-3 (readLoop)。方案：抽 `buildReplyContext` + `handleSendResult` helpers。
- [ ] **R219-CR-8 — `shim/server.go::handleClient` 319 行无子拆（P2）**: 4 个内联 goroutine 通过裸 channel 通信。方案：抽 `handleClientHandshake / relayStdin / relayStdout`。
- [ ] **R219-CR-9 — `processIface.GetState/GetSessionID` 违反 Go 命名约定（P2）**: 应去 `Get` 前缀。Breaking：是（接口变更，~12 处 callsite + mock 需改）。
- [x] **R219-CR-10 — `transport_hook.go:136` 注释 "base-16-ish" 与代码 0x21-0x7E 全 ASCII filter 不符（P3）**: 文档与代码漂移。方案：要么收紧 filter 到 hex，要么更正注释。 — 已修复（更正注释为 alphanumeric + 解释 0x21-0x7E 全开是有意 headroom），本批 PR #75
- [ ] **R219-CR-11 — `server.sessionSendLegacy` Deprecated 但生产 nil-queue fallback 仍可达（P3）**: 注释说仅用于"未 wire MessageQueue 的测试代码路径"，但 Hub.queue == nil 时生产也命中。方案：Hub.Start() 加 nil-queue 启动 warn 或 NewWithOptions 强制 non-nil queue。

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
- [ ] **R219-ARCH-12 — DESIGN.md 实际状态机已偏离描述（P3）**: DESIGN.md 描述 4 状态 `Spawning/Ready/Running/Dead`，实现已加 Paused/Suspended/stub/scratch ephemeral/exempt 等 7+ 状态。方案：DESIGN.md 补真实状态枚举 + state diagram 或链 ADR。
- [ ] **R219-ARCH-13 — DESIGN.md L292 event log 持久化"单文件 single goroutine"承诺与 per-session writer N goroutine 现实不符（P3）**: 文档与实现漂移。方案：DESIGN.md 改"per-session writer, batched fsync, 100ms flush tick"。

## Round 218 — 5-agent 并行 review 第 32 轮（2026-05-16）NEEDS-DESIGN

> 5 reviewer（Go / 安全 / 性能 / 代码质量 / 架构）并行扫描共约 100+ 条发现。
> 6 条 FIX-READY 已落地（PR #40 + PR #22）。以下是需设计决策、破坏兼容、跨包重构、
> 或方案不唯一不适合本轮直接修的条目。

### Go 正确性 — 跨包改动

- [ ] **R218-GO-2 — `dispatch.go:969-1002` sendAskQuestionCard goroutine 访问可能已释放的 tracker**: stop() 先执行后该 goroutine 仍对已释放 platform 进行类型断言。建议：加 context timeout 或在 stop() 里主动取消待发送卡片 goroutine。`internal/dispatch/dispatch.go:969-1002`。
- [x] **R218B-GO-1 — `discoveryCache.startLoop` 初始 `go dc.refresh()` 无 WaitGroup 追踪（P2）**: `startLoop` 启动一个裸 goroutine 做初始 refresh，Server Shutdown 取消 ctx 后该 goroutine 仍在后台运行，可能访问已清理的 projectMgr。方案：给 `discoveryCache` 添加 `wg sync.WaitGroup`，`startLoop` 前 `wg.Add(1)` + defer Done，暴露 `Wait()` 供 Server.Shutdown 调用。涉及：`internal/server/discovery_cache.go:47-60`, `internal/server/server.go` Shutdown 路径。 — 已修复，见 PR #68
- [ ] **R218B-GO-2 — `handleOwnerLoopPanic` 用 `context.Background()` 向用户回送错误（P1 重申 R217-GO-2）**: recovery handler 创建 Background ctx 通知用户，若 appCtx 已取消（shutdown 期间）会挂起。方案：接受 parentCtx 参数或用 `context.WithTimeout(context.Background(), 5*time.Second)`。涉及：`internal/dispatch/dispatch.go:510`。
- [ ] **R218B-GO-3 — `readLoop` linker.Resolve goroutine 无 context 绑定（P1）**: `go linker.Resolve(taskID, toolUseID, ...)` 启动时无 cancellation。进程 shutdown 后 Resolve 可能继续访问磁盘。方案：`linker.Resolve` 接受 ctx 参数，绑定到 process 生命周期。涉及：`internal/cli/process_readloop.go:324`，`internal/cli/subagent_link.go`。Breaking：是（接口变更）。

### 安全 — 新发现（非重复）

- [x] **R218B-SEC-2 — `project_files.go` stat→open TOCTOU 窗口（P3）**: `statRelWithRoot` 调用 `EvalSymlinks + Stat`，后续 preview handler 再次 `Open` 同路径。两次调用之间攻击者可替换 symlink 指向敏感文件。现有 `EvalSymlinks` 已 resolve 到真实路径，但 preview 端点重新 join + Open 而不是用已 resolved 路径。方案：`statRelWithRoot` 返回 `resolved string` 供 preview handler 直接复用，避免二次 EvalSymlinks。涉及：`internal/server/project_files.go:444-491`。 — 已修复（采用 Lstat-after-resolve defense-in-depth 而非 resolved 复用，更紧凑），见 PR #71

### 性能 — 需 benchmark 确认

- [ ] **R218B-PERF-1 — `resubscribeEvents` 每次调用 `time.NewTimer` 分配（P1）**: 客户端重连 flap 时多路并发 `resubscribeEvents` 各自分配 Timer，GC 压力在 N client 同时断线重连场景可观。方案：改用 `time.AfterFunc` 或 Timer 池。注意现有代码已在循环内 Reset 复用同一 Timer（`timer.Reset(5s)`），只是首次分配无法避免——实际影响有限，benchmark 后决策。涉及：`internal/server/wshub.go:1080`。
- [ ] **R218B-PERF-2 — `ownerLoop` 每次 collect 窗口 `time.NewTimer` 分配（P2）**: `collectTimer := time.NewTimer(d.queue.CollectDelay())` 在 ownerLoop 函数体内分配，ownerLoop 是每条消息的热路径。方案：改 `time.AfterFunc` 或在 Dispatcher 持有复用 Timer。涉及：`internal/dispatch/dispatch.go:448`。

### 架构 — 新发现

- [ ] **R218-ARCH-1 — cron.SessionRouter 未纳入 contract_test（已修复，见 PR #40）**: ~~四个 consumer 中 cron 独缺编译期 pin，Router 签名漂移对 cron 无编译报警。~~ — 已修复，见 PR #40
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
- [ ] **R217-SEC-3 — gzip 解压链路潜在 bomb 风险**: `internal/server/gzip.go` `gzip.NewReader` 解压无大小上限。当前 `MaxBytesReader` 是压缩字节级，若未来某 path 在 gzip middleware 之后才设 cap，会留下 bomb 窗口。方案：`io.LimitReader` 包装 gzip reader 输出，按 per-handler body cap。需先核实 gzip middleware 实际是否在 MaxBytesReader 之前。
- [ ] **R217-SEC-4 — `gogo/protobuf v1.3.2` CVE-2021-3121（间接依赖）**: aws-sdk-go-v2 间接依赖。naozhi 不直接调用，但为消除告警可 `go get github.com/gogo/protobuf@latest` 或在 go.mod 加 replace。
- [ ] **R217-SEC-5 — `golang.org/x/crypto v0.49.0` 偏旧（约 10 个月）**: 无已知 critical CVE 但建议跟随 toolchain 升级。
- [ ] **R217-SEC-6 — `dashboardToken` 轮转无显式 session 失效机制**: 当前依赖 cookieMAC(secret, dashboardToken)，token 改后旧 cookie 自然失效，但需要 process restart。增设 server-side session generation counter 才能不重启即时撤销。Breaking：需要持久化 generation 状态。
- [ ] **R217-SEC-7 — `writeJSON` 全局 `SetEscapeHTML(false)`**: 当前是 Feishu 卡片需求；defense-in-depth 上应分离 Feishu encoder pool 和通用 API encoder pool。Breaking：Feishu 卡片含 `<>&` 时输出会变。
- [ ] **R217-SEC-8 — `/health` 在认证后返回 workspace_id / node 状态等运营情报，cleartext HTTP 部署可被嗅探**: 部署侧问题，添加启动 warning 即可：non-loopback bind + 无 TLS terminator 时提示。

### Go 正确性 — 跨包改动 / 需 ctx 传递

- [ ] **R217-GO-1 — `Guard.lastWait` 在不发起 Release 的路径下永久泄漏**: 现状 ShouldSendWait 写 + Release 删；不调 Release 的 path 留下永久 entry。方案：sync.Map+TTL sweep（如 seenNonces），或显式 cap+LRU。
- [ ] **R217-GO-2 — `handleOwnerLoopPanic` 用 `context.Background()`**: 当前签名不收 ctx，要改成 `(ctx, key, msg, r)` 影响测试与 owner-loop 路径。Breaking：函数签名变化。
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
- [ ] **R217-CR-6 — workspace 三重重载命名混淆 `cfg.Session.Workspace` / `cfg.Workspaces` / `cfg.Workspace`**: 重命名或文档化。
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

- [ ] **R214-SEC-3 — ANTHROPIC_ 前缀全量透给 shim**: `shim/manager.go::shimEnvAllowedPrefixes` 允许 ANTHROPIC_* 全系列进子进程；Bedrock 部署下 `ANTHROPIC_API_KEY` 冗余却可被 Bash tool 读。
  - 方案：明确排除 `ANTHROPIC_API_KEY=` 或按 backend 按需 allowlist。
  - 涉及：`internal/shim/manager.go:987-995`

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

- [ ] **RNEW-UX-001 — WS 重连无 jitter，N 客户端雪崩**: `dashboard.js:6717-6730` `backoff = min(backoff*2, 30s)` 纯指数，服务重启时所有 tab 在同一 ms 点重连，瞬时打满。
  - 方案: `delay += Math.floor(Math.random() * 500)` 或 full-jitter。
  - 涉及: `dashboard.js:6717-6730`

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

- [ ] **R215-PERF-P1-1 — `eventlog_bridge.go:49` 每 EventEntry `json.Marshal`**: 持久化 sink closure 对每条 EventEntry 调 json.Marshal (reflection)，热路径。
  - 方案：EventEntry 提供 `MarshalJSONFast` 或改用 `bytes.Buffer + encoder` 复用。
  - 涉及: `internal/session/eventlog_bridge.go:49`

- [ ] **R215-PERF-P2-1 — `eventlog.Append` 单条路径 `[]EventEntry{e}` 每次 alloc**: AppendBatch 的 sinkCopy 已预分配，Append 单条仍每次分配 1 slice。
  - 方案：引入 1-slot pool 或为 Append 写专用 sink 路径。
  - 涉及: `internal/cli/eventlog.go:627`


- [ ] **R215-PERF-P2-3 — `wshub.marshalPooled` 返回副本即便单订阅者**: `SendRaw` enqueue 后不再持 slice，小 batch 场景可省 copy。
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


