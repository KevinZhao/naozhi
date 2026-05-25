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
- [~] **R217-CR-4 — `Hub god struct 36 字段 / `node.Conn` 18+ 方法巨型接口**: 子聚合拆分。 *(PR #327 R243-ARCH-2 部分实施：wshub.go 文件 split 5 个职责文件；Hub struct 字段未拆（保锁不变量）。node.Conn 接口未动 — 跟踪 R231-ARCH-5。)*
- [ ] **R217-CR-5 — cross-node 错误注入方向不对称**: 反向有 LogSystemEvent，正向 node.Conn.Send 失败只 slog 不进 EventLog。
- [~] **R217-CR-7 — `project.DisplayName` / `Emoji` schema 校验但 dashboard UI 不读（归档 2026-05-23）**: 同根因 R216-CR-7（重复登记）+ R110-P2 父条目（"项目自定义显示名 + emoji（foundation 已落地 2026-05-10，UI 待续）"）已显式 own UI wire 工作。foundation 实地复核：`internal/project/validate.go:47-55,124-138` `ProjectConfig.DisplayName` (128 rune cap) + `ProjectConfig.Emoji` (8 rune cap) + `ProjectConfig_DisplayNameEmojiRoundtrip` / `ProjectConfig_LegacyYamlWithoutDisplayName` / `ProjectConfig_DisplayNameTooLong` 三测试已落地；`internal/server` 全包 grep `project.DisplayName / project.Emoji` 0 命中（UI/`/api/projects` 均未读）。统一收敛到 R110-P2 跟踪 UI 落地，本批 PR


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
- [~] **R222-ARCH-6 — Hub god-object 苗头（35+ 方法 / 1700 行）（P2）**: 拆 Hub + SubscriptionRegistry + MessageBroker。 *(PR #327 R243-ARCH-2 已 6-stage split：wshub.go 2028→525 行，方法分到 5 个职责文件。35+ 方法已物理分散；struct 层 SubscriptionRegistry/MessageBroker 抽出跟踪到 R248-ARCH-6。)*
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
- [~] **R57-ARCH-001 — `Cleanup` 双 pass `loadProcess()` 在非 exempt 占多数场景下更慢（LOW）**: go-reviewer 指出 R56 的 "先 count candidates 再 allocate" 优化在 exempt 少的部署上反而多一次 loadProcess 扫描。idle plan 部署（每 5 分钟 tick 一次）无实际差异；需真实 profiling 数据决定是否回滚或改 single-pass count-then-grow。 — 已实施（cron-fix-F4 2026-05-24 核实）：`router_cleanup.go:163-188` Cleanup() 已是 single-pass，candidate slice 用 `make([]cand, 0, len(r.sessions)/2+1)` 容量估算，inline 注释引 R59-GO-M1。本条目所述的"双 pass"早已在 R59 修复，TODO 残留。本批 PR 关闭。
- [ ] **R54-CONCUR-001 — `router.ReconnectShims` reconcile 期运行时的 `sess.ReattachProcessNoCallback` 无 sendMu 保护（继承自 R51-CONCUR-002 未决）**: 本轮复核确认仍未决。方案见 R51-CONCUR-002。
- [~] **R52-CONCUR-004 — `shim reconnect` 后 `sess.persistedHistory` 未重新注入新 `proc.eventLog`（MED）**: `Router.reconnectShims` 在 `storeProcess`（`ReattachProcessNoCallback`）前调 `proc.InjectHistory(histEntries)`，但 `histEntries` 是从 `discovery.LoadHistory` 读的 JSONL 文件，**不是** `sess.persistedHistory`。两者大部分一致，但 `persistedHistory` 可能包含仅在内存里的 user prompt / interim 状态（R49/R47 曾多次浮现）。新 proc.eventLog 缺少这些条目时，`EventEntriesSince` 的 "proc != nil → proc.EventEntries" 快路径可能少返回若干历史。 — 已加 inline godoc（cron-fix-F4 2026-05-24）：`router_shim.go:434-471` reconnect JSONL 注入路径已注释说明本子问题 + dashboard scrollback fallback 路径会做 merge 收敛 + 真正修复需先落 R51-CONCUR-002（sendMu 保护）以保证合并不撕裂正在进行的 Send。本条作为子项归 R51-CONCUR-002 主条目跟踪，本批 PR 关闭。
- [ ] **R51-CONCUR-002 — `reconnectShims` 周期调用期间 `ReattachProcessNoCallback` 无 sendMu 保护（HIGH）**: `StartShimReconcileLoop` 每 30s 调一次 `reconnectShims`，调用点在持 `r.mu.Lock()` 时 `sess.ReattachProcessNoCallback(proc, sessionID)`；该函数 docstring 明确标注"调用者必须保证 Send() 不在飞行中"（safety constraint）。启动阶段 OK，但运行期 reconcile 不满足该假设 — `ManagedSession.Send()` 可能持 sendMu 执行旧进程。`storeProcess` 会原子替换活跃进程指针，并 `deathReason.Store("")` 清除 Send() 刚写入的 timeout 死因。逻辑 race（非 data race），Send() 拿的是旧 process 指针仍可写回结果。
- [ ] **R51-CONCUR-005 — 并发 `shim.Manager.Reconnect` 对同 key 晚胜者 Close 早胜者 handle，session 误死（MED）**: `Reconnect` 在 `m.mu` 外建立 TCP 连接（10s 超时），然后在 `m.mu.Lock()` 下插入 `m.shims[key]`。两路并发 Reconnect 分别建立连接后，晚胜者关闭早胜者 handle；但早胜者 handle 可能已被 Router `reconnectShims` 传给 `Process`，`Process.shimConn` 被 Close 导致 readLoop 退出、session 标为 Dead。
- [ ] **R37-REL1 — `MessageQueue.TryAcquire` + `Release` 不会触发 drain**: Dashboard/WS 路径用 Guard 接口，但若同期 IM 入口 Enqueue 了消息（enqueued=true），Release 不会触发 DoneOrDrain，消息永久搁浅直到下次 Enqueue 再成为 owner。属于 Guard/Queue 混用的根本限制。
- [ ] **R33-UX1 — `dashboard.js renderSidebar` 每次 sessions_update 全量 innerHTML 重绘**: 20 sessions × 1 update/s 情况下浏览器全侧边栏 reflow；已缓存 `allSessionsCache` 但未做 diff。scrollTop 保持已在 RNEW-UX-016 关闭时落地（rAF 恢复），剩余工作 = DOM diff + active-card 跨重绘保留 + `allSessionsCache` 一致性（`syncSidebarSelectionWithActive`/`removeSessionCard` 路径若做 in-place 修改可能让后续 update 看到不一致前缀）。合并 R34-UX1。
- [ ] **R31-REL3 — `moveToShimsCgroup` 依赖 runtime sudo + 未校验 CLIPID**: 现状用 `sudo busctl`/`sudo tee`，CLIPID 取自 shim JSON 直接入参；若 shim 被劫持可通过伪造 CLIPID 把任意进程挪入 scope。
- [~] **R30-DES1 — 需架构决策（2026-04-29 Round 112 评估降级）**：本轮尝试在 `execute()` 入口加 `stopCtx.Err()` 守卫覆盖 fresh + persistent 两种模式，但这与 Round 95 的设计意图冲突（Round 95 明确将 persistent 模式的 ctx 取消委托给 Router.Shutdown，`TestCRON3_PersistentModeUnaffectedByGuard` 把此行为作为测试护栏）。fresh 分支的 stopCtx.Err() 守卫（`scheduler.go:1260`）已覆盖最危险的"fresh → Reset → 孤立 CLI"路径。persistent 模式的真正修复需要架构级协调：要么把 Router.Shutdown 和 Scheduler.Stop 串联锁定（需 S11 级决策），要么在 GetOrCreate 路径里加 shutdown-awareness（改动面大）。当前降级，等 S11 整体方案落地后重开。
- [ ] **R29-DES1 — `drainStaleEvents` push-back + goto drain 可吞 interrupted result 事件**: 本轮新发现的 invariant 冲突。在 interrupted/interruptedRun 分支的 for 循环中，若事件顺序为 `[old_nonresult, new_event, old_result]`，读到 `new_event` 后 push-back + `goto drain`，接着 drain 到 `old_result` 时因 `recvAt < cutoff` 被丢弃。interrupted 语义要求 settle 窗口必须拿到 old_result，否则下一 turn 迟到的 result 会污染结果。


## Triage Outcomes (2026-05-25 batch3-E)

Triage of 89 findings via skill `triage-findings`. Verified each against current code (post-router_*.go split, post-wshub split). See per-bullet annotation table below; full audit trail in this file.

### Round 215 — 15 items
| Anchor | Decision | Reason / Issue |
|---|---|---|
| R215-GO-P2-5 | C | rolled-into:R51-CONCUR-002 (#751) |
| R215-SEC-P1-1 | A | issue #531 |
| R215-SEC-P1-2 | A | issue #535 |
| R215-SEC-P2-3 | A | issue #536 |
| R215-SEC-P3-1 | C | already-archived: accepted-risk per code godoc |
| R215-PERF-P1-1 | C | already-fixed: commit 7fc779f sync.Pool encoder |
| R215-PERF-P2-1 | C | rolled-into:R230-PERF-1 + R215-PERF-P2-5 family |
| R215-PERF-P2-2 (single-sub fast-path) | C | rolled-into:R225-PERF-10 cluster |
| R215-PERF-P2-4 | C | already-archived: design-justified atomic.Pointer slot |
| R215-PERF-P2-5 | C | rolled-into:R219-PERF-2 archived |
| R215-CR-P2-3 | A | issue #543 |
| R215-ARCH-P1-1 | A | issue #545 |
| R215-ARCH-P1-2 | A | issue #547 |
| R215-ARCH-P1-3 | C | dup of #430 (R176-ARCH-M2 processIface god) |
| R215-ARCH-P1-4 | A | issue #566 |
| R215-ARCH-P1-5 | A | issue #567 |
| R215-ARCH-P2-1 | C | dup of #376/#431 Hub setter race |
| R215-ARCH-P2-2 | A | issue #578 |
| R215-ARCH-P2-3 | A | issue #579 |
| R215-ARCH-P2-4 | A | issue #580 |
| R215-ARCH-P2-5 | A | issue #581 |
| R215-ARCH-P2-6 | C | already-archived: intentional leak per Shutdown one-shot |
| R215-ARCH-P2-7 | A | issue #585 (Snapshot pack box) |
| R215-validateWorkspace (cron API) | C | already-fixed: handleCreate/Update boundary check landed |

### Round 217 — 19 items
| Anchor | Decision | Reason / Issue |
|---|---|---|
| R217-SEC-1 | C | rolled-into:R216-SEC-2 archived multi-round |
| R217-SEC-2 | C | rolled-into:R61-SEC-2 archived design |
| R217-SEC-4 | A | issue #591 |
| R217-SEC-5 | A | issue #593 |
| R217-SEC-6 | A | issue #594 |
| R217-SEC-7 | A | issue #595 |
| R217-SEC-8 | A | issue #601 |
| R217-GO-1 | C | already-fixed: Guard struct removed (collapsed into MessageQueue) |
| R217-GO-3 | C | rolled-into:R216-GO-4 |
| R217-GO-4 | A | issue #602 |
| R217-GO-5 | A | issue #603 |
| R217-GO-6 | C | rolled-into:R230B-GO-1 |
| R217-GO-7 | C | already-archived: textutil.StoreAtomicString fast-path safe per godoc |
| R217-PERF-1 | C | rolled-into:R67-PERF-1 / R216-PERF-1 |
| R217-PERF-2 | C | rolled-into:R71-PERF-H1 / #429 |
| R217-PERF-3 | C | rolled-into:R230B-PERF-6 |
| R217-PERF-5 | A | issue #606 |
| R217-PERF-6 | A | issue #613 |
| R217-PERF-7 | C | rolled-into:R225-PERF-10 |
| R217-PERF-8 | C | rolled-into:R225-PERF-2 |
| R217-PERF-10 | A | issue #615 |
| R217-ARCH-1 | A | issue #616 |
| R217-ARCH-2 | A | issue #617 |
| R217-ARCH-3 | A | issue #625 |
| R217-ARCH-4 | C | dup of R215-ARCH-P2-4 (#580) |
| R217-ARCH-5 | C | dup of #430 |
| R217-ARCH-6 | C | rolled-into:R216-ARCH-6 |
| R217-ARCH-7 | A | issue #626 |
| R217-ARCH-8 | C | rolled-into:R216-ARCH-4 / dup of #376 |
| R217-ARCH-9 | A | issue #627 |
| R217-CR-3 | A | issue #629 |
| R217-CR-4 | C | dup of #376 (Hub) and follow-up R231-ARCH-5 (node.Conn) |
| R217-CR-5 | A | issue #639 |
| R217-CR-7 | C | rolled-into:R110-P2 (DisplayName/Emoji UI) |

### Round 218 — 3 items
| Anchor | Decision | Reason / Issue |
|---|---|---|
| R218B-GO-3 | A | issue #641 |
| R218-ARCH-2 | C | dup of R215-ARCH-P2-4 (#580) |
| R218-ARCH-3 | C | rolled-into:R214-ARCH-1 archived |
| R218B-ARCH-2 | A | issue #644 |
| R218B-PERF-1 | C | already-archived: cold-path single Timer reuse already optimal |
| R218B-PERF-2 | C | already-archived: false positive — Timer hoisted out of loop |

### Round 219 — 13 items
| Anchor | Decision | Reason / Issue |
|---|---|---|
| R219-GO-1 | A | rolled-into:R218B-GO-3 (#641) — same root |
| R219-GO-2 | C | rolled-into:R218B-GO-3 (#641) |
| R219-SEC-1 | A | issue #648 |
| R219-SEC-2 | A | issue #653 |
| R219-PERF-1 | C | rolled-into:R214-PERF-4 / R222-PERF-4 (multi-round NEEDS-DESIGN parent) |
| R219-PERF-2 | C | already-archived: dynamic uptime/now invalidates cache |
| R219-PERF-3 | C | rolled-into:R215-ARCH-P2-7 (#585) |
| R219-PERF-4 | C | rolled-into:R222-PERF-8 cluster |
| R219-CR-7 | A | issue #655 |
| R219-CR-8 | A | issue #656 |
| R219-CR-9 | A | issue #657 |
| R219-ARCH-1 | C | rolled-into:R217-ARCH-1 (#616) |
| R219-ARCH-2 | C | rolled-into:R215-ARCH-P1-5 (#567) |
| R219-ARCH-3 | C | dup of #372 (CLOSED) |
| R219-ARCH-4 | C | rolled-into:R214-ARCH-9 archived |
| R219-ARCH-5 | C | dup of #376/#431 |
| R219-ARCH-6 | A | issue #665 |
| R219-ARCH-7 | C | dup of #430 |
| R219-ARCH-8 | A | issue #668 |
| R219-ARCH-9 | A | issue #670 |
| R219-ARCH-10 | C | rolled-into:R215-SEC-P1-1 (#531) |
| R219-ARCH-11 | C | dup of #439 (CLOSED) |

### Round 220 — 5 items
| Anchor | Decision | Reason / Issue |
|---|---|---|
| R220-GO-1 | C | rolled-into:R218B-GO-3 (#641) — same Resolve ctx root |
| R220-GO-2 | C | rolled-into:R219-SEC-2 (#653) |
| R220-PERF-1 | A | issue #673 |
| R220-PERF-3 | A | issue #684 |
| R220-PERF-4 | A | issue #685 |
| R220-PERF-5 | C | already-archived: 4-way decision needs lock |

### Round 222 — 24 items
| Anchor | Decision | Reason / Issue |
|---|---|---|
| R222-SEC-4 | A | issue #686 |
| R222-GO-1 | A | issue #687 |
| R222-GO-3 | C | already-fixed: R227-GO-3 fireCallbacksDropLock + onResolveMu copy-then-drop |
| R222-PERF-1 | C | rolled-into:R67-PERF-1 multi-round NEEDS-DESIGN |
| R222-PERF-2 | C | rolled-into:R71-PERF-H1 (#429) |
| R222-PERF-3 | A | issue #699 |
| R222-PERF-4 | C | rolled-into:R219-PERF-1 / R214-PERF-4 |
| R222-PERF-5 | C | rolled-into:R225-PERF-10 |
| R222-PERF-6 | C | rolled-into:R219-PERF-2 archived |
| R222-PERF-7 | C | rolled-into:R215-ARCH-P2-7 (#585) |
| R222-PERF-8 | C | rolled-into:R219-PERF-4 cluster |
| R222-PERF-9 | C | already-archived: contracted by PersistSink retention |
| R222-PERF-11 | A | issue #700 |
| R222-CR-1 | A | rolled-into:R217-ARCH-7 (#626) |
| R222-CR-2 | A | issue #708 (connector_rpc handleRequest 522 lines) |
| R222-CR-3 | A | rolled-into:R217-ARCH-7 (#626) |
| R222-CR-4 | A | rolled-into:R215-ARCH-P2-5 (#581) |
| R-LEGACY-SEND | A | issue #710 |
| R222-ARCH-1 | C | dup of #383 |
| R222-ARCH-2 | A | issue #711 |
| R222-ARCH-3 | A | issue #713 |
| R222-ARCH-5 | C | rolled-into:R217-ARCH-2 (#617) |
| R222-ARCH-6 | C | dup of #376 |
| R222-ARCH-7 | C | dup of #430 |
| R222-ARCH-8 | A | issue #722 |
| R222-ARCH-9 | A | issue #724 |
| R222-ARCH-10 | A | issue #728 |
| R222-ARCH-11 | A | issue #732 |
| R222-ARCH-12 | A | issue #735 |
| R222-ARCH-13 | A | issue #737 |
| R222-ARCH-14 | A | issue #739 |
| R222-ARCH-15 | C | rolled-into:R217-ARCH-1 (#616) |
| R222-ARCH-16 | A | issue #741 |
| R222-ARCH-17 | A | issue #748 |

### Round 26-82 archive — 10 items
| Anchor | Decision | Reason / Issue |
|---|---|---|
| R71-PERF-H1 | C | dup of #429 (R176-PERF-N2) — comment cross-ref posted |
| R67-PERF-3 | A | issue #749 |
| R62-GO-3 | A | issue #775 |
| R61-GO-10 | C | already-降级: per code design — eviction != workspace forget |
| R57-ARCH-001 | C | already-fixed: R59 made Cleanup single-pass |
| R54-CONCUR-001 | C | rolled-into:R51-CONCUR-002 (#750) |
| R52-CONCUR-004 | C | rolled-into:R51-CONCUR-002 (#750) |
| R51-CONCUR-002 | A | issue #750 |
| R51-CONCUR-005 | A | issue #751 |
| R37-REL1 | A | issue #769 |
| R33-UX1 | A | issue #771 |
| R31-REL3 | C | already-fixed: R229-SEC-4/R219-SEC-5 PPid validation landed |
| R30-DES1 | C | already-降级: Round 112 evaluation, accepted current state |
| R29-DES1 | A | issue #773 |

## Discarded

> Bucket-C audit trail: each anchor listed once with one-line reason. Future reviewers grep for these.

- R215-GO-P2-5: rolled-into:#751 (R51-CONCUR-002 family)
- R215-SEC-P3-1: already-archived (accepted-risk OS-account-trust-boundary)
- R215-PERF-P1-1: already-fixed (commit 7fc779f sync.Pool encoder)
- R215-PERF-P2-1: rolled-into:R230-PERF-1 sink-nil fast-path
- R215-PERF-P2-2: rolled-into:R225-PERF-10
- R215-PERF-P2-4: already-archived (atomic.Pointer slot structurally required)
- R215-PERF-P2-5: rolled-into:R219-PERF-2 archived
- R215-ARCH-P1-3: dup of #430 processIface god
- R215-ARCH-P2-1: dup of #376 / #431 Hub setter race
- R215-ARCH-P2-6: already-archived (intentional leak under one-shot Shutdown)
- R215-validateWorkspace: already-fixed (handleCreate/Update boundary added)
- R217-SEC-1: rolled-into:R216-SEC-2
- R217-SEC-2: rolled-into:R61-SEC-2
- R217-GO-1: already-fixed (Guard struct removed)
- R217-GO-3: rolled-into:R216-GO-4
- R217-GO-6: rolled-into:R230B-GO-1
- R217-GO-7: already-archived (textutil.StoreAtomicString fast-path proven safe)
- R217-PERF-1: rolled-into:R67-PERF-1
- R217-PERF-2: rolled-into:#429
- R217-PERF-3: rolled-into:R230B-PERF-6
- R217-PERF-7: rolled-into:R225-PERF-10
- R217-PERF-8: rolled-into:R225-PERF-2
- R217-ARCH-4: dup of R215-ARCH-P2-4 (#580)
- R217-ARCH-5: dup of #430
- R217-ARCH-6: rolled-into:R216-ARCH-6
- R217-ARCH-8: dup of #376
- R217-CR-4: dup of #376 (Hub) / R231-ARCH-5 (node.Conn)
- R217-CR-7: rolled-into:R110-P2
- R218-ARCH-2: dup of R215-ARCH-P2-4 (#580)
- R218-ARCH-3: rolled-into:R214-ARCH-1
- R218B-PERF-1: already-archived (cold-path Timer single alloc)
- R218B-PERF-2: already-archived (false-positive — Timer outside loop)
- R219-GO-2: rolled-into:R218B-GO-3 (#641)
- R219-PERF-1: rolled-into:R214-PERF-4 / R222-PERF-4 multi-round
- R219-PERF-2: already-archived (dynamic uptime breaks cache)
- R219-PERF-3: rolled-into:R215-ARCH-P2-7 (#585)
- R219-PERF-4: rolled-into:R222-PERF-8 cluster
- R219-ARCH-1: rolled-into:R217-ARCH-1 (#616)
- R219-ARCH-2: rolled-into:R215-ARCH-P1-5 (#567)
- R219-ARCH-3: dup of #372 (CLOSED)
- R219-ARCH-4: rolled-into:R214-ARCH-9
- R219-ARCH-5: dup of #376 / #431
- R219-ARCH-7: dup of #430
- R219-ARCH-10: rolled-into:R215-SEC-P1-1 (#531)
- R219-ARCH-11: dup of #439 (CLOSED)
- R220-GO-1: rolled-into:R218B-GO-3 (#641)
- R220-GO-2: rolled-into:R219-SEC-2 (#653)
- R220-PERF-5: already-archived (4-way decision requires lock)
- R222-GO-3: already-fixed (R227-GO-3 fireCallbacksDropLock)
- R222-PERF-1: rolled-into:R67-PERF-1
- R222-PERF-2: rolled-into:R71-PERF-H1 (#429)
- R222-PERF-4: rolled-into:R219-PERF-1
- R222-PERF-5: rolled-into:R225-PERF-10
- R222-PERF-6: rolled-into:R219-PERF-2 archived
- R222-PERF-7: rolled-into:R215-ARCH-P2-7 (#585)
- R222-PERF-8: rolled-into:R219-PERF-4
- R222-PERF-9: already-archived (PersistSink retention contract)
- R222-ARCH-1: dup of #383
- R222-ARCH-5: rolled-into:R217-ARCH-2 (#617)
- R222-ARCH-6: dup of #376
- R222-ARCH-7: dup of #430
- R222-ARCH-15: rolled-into:R217-ARCH-1 (#616)
- R71-PERF-H1: dup of #429 (cross-ref posted)
- R61-GO-10: already-降级 (eviction != workspace forget by design)
- R57-ARCH-001: already-fixed (R59 single-pass Cleanup)
- R54-CONCUR-001: rolled-into:#750
- R52-CONCUR-004: rolled-into:#750
- R31-REL3: already-fixed (R229-SEC-4 PPid validation)
- R30-DES1: already-降级 (Round 112 evaluation closed)
