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


---

## Discarded

> Triaged 2026-05-25 by `triage-findings` skill. The raw file mixes R216-prefixed
> findings with carried-forward open items from older rounds (R110/R172/R176/R194/RNEW/R214).
> All non-R216 anchors already have GitHub issues opened by earlier triage runs;
> they are catalogued here as "dup of #N" for audit. R216-prefixed items split
> into already-fixed / archived-into-newer-anchor / opened-as-issue per-line.

### R216 anchors

#### Opened as issue
- R216-SEC-3 → #541 (LimitedReader cumulative-cap missing)
- R216-SEC-5 → #546 (CLIPID forge into busctl/cgroup)
- R216-GO-6 → #565 (cmd.Wait reaper goroutine has no Manager ownership)
- R216-CR-1 → #571 (plannerKeyFor duplication; extract internal/sesskey leaf pkg)

#### Bucket C — already fixed (grep verified)
- R216-GO-2 → fixed: `shim.Run` uses `atomic.Pointer[os.File]` at `internal/shim/server.go:104` with explicit `R216-GO-2` annotation.
- R216-GO-4 → fixed: `ReconnectShims` now derives from `r.historyCtx`; SIGTERM aborts handshakes per `internal/session/router_shim.go:85` `R216-GO-4` annotation.
- R216-SEC-6 → fixed: `watchSocketFile` uses `os.Lstat` per `R218-SEC-12` at `internal/shim/server.go:453`. Stale godoc above the function logged to cosmetic-backlog.

#### Bucket C — accepted / contract-locked
- R216-GO-1 → accepted: SAFETY CONSTRAINT documented in godoc + grep-locked by `internal/session/reattach_contract_test.go` (R31-REL1) + R229-GO-1 closed in TODO 2026-05-23. Caller contract is the chosen mitigation; lock-ordering rewrite deferred indefinitely.
- R216-GO-5 → accepted: cron `Stop()` triggerWG single-shot orphan path explicitly documented in scheduler.go:838-851 (`R222-GO-10`, `R246-GO-13`); test pollution under `-count=N` is acknowledged but reclaim is rejected (Scheduler is single-shot, triggerWG.Wait does not accept cancel signal).

#### Bucket C — converged to other anchors (per raw file annotations)
- R216-SEC-2 → R231-SEC-4 (ExtraArgs flag allowlist; multi-round rollup)
- R216-GO-7 → R230B-GO-1 (cron sendCtx Background-derived budget clamp)
- R216-PERF-1 → R231-PERF-1 (ClaudeProtocol.ReadEvent dual-copy; 30+ rounds rollup)
- R216-PERF-2 → R233B-PERF-2 (shimWriter.Write copy)
- R216-PERF-3 → R230B-PERF-6 (eventlog_bridge raw bytes)
- R216-ARCH-1 → dup of #430 (R176-ARCH-M2 processIface 24 methods)
- R216-ARCH-2 → R231-ARCH-3 (session imports backend-specific history packages)
- R216-ARCH-3 → R229-ARCH-5 (KeyResolver migration half-landed)
- R216-ARCH-4 → dup of #431 (R176-ARCH-M3 Hub setter race) and #384 (ARCH4 Server/Hub mutex)
- R216-ARCH-5 → multi-round NEEDS-DESIGN (attachment.GC cron registration)
- R216-ARCH-6 → R231-ARCH-4 (Router god-object; cross-round rollup)
- R216-CR-2 → dup of R216-ARCH-4 → ultimately #431 / #384
- R216-CR-3 → dup of #435 (H6 / R214-ARCH-7 node.Conn 26-method split)
- R216-CR-4 → dup of #433 (R176-ARCH-NX cross-node error asymmetric)
- R216-CR-7 → dup of #448 (R110-P2 Project DisplayName + emoji UI wire)

### Non-R216 anchors carried forward in raw file (already in tracker)

These pre-R216 items appear because the raw dump includes the entire docs/TODO.md
open list at R216 time, not just the new R216 findings. All are already filed:

- Round 194 anchors: SM3 #381, S14 #382, ARCH2 #383, ARCH4 #384, ARCH5 #385, ARCH6 #386, ARCH1 #387, TEST2 #388, S9 #389, S11 #390, X1 #391, OBS1 #392, CLI2 #393, SM2 #394, TEST1 #395, CQ1 #396, CQ2 #397 (CLOSED), UX3 #398, PF1 #399, RES2 #400, CRON1 #401
- Round 194 Q3 → fixed (`MessageQueue.Cleanup` exists at `internal/dispatch/msgqueue.go:317`)
- Round 194 ARCH3 → broadly addressed via KeyResolver adoption across server/dispatch/upstream
- Round 194 DOC2 → moot (docs/TODO.md frozen 2026-05-25; replaced by issue tracker + this skill)
- Round 194 DEP2 → accepted (gogo/protobuf v1.3.2 has CVE-2021-3121 fix; dependabot tracks)
- RNEW-* anchors: RNEW-003 #423, RNEW-008 #424, RNEW-ARCH-401 #425, RNEW-ARCH-402 #426, RNEW-ARCH-403 #427, RNEW-ARCH-404 #428, RNEW-PERF-003 #454, RNEW-PERF-004 #455, RNEW-OPS-415 #456, RNEW-UX-003 #444, RNEW-UX-006 → dup of #441 (H2 same root cause), RNEW-UX-015 → archived (ratchet test landed)
- R214 anchors: R214-ARCH-2..13 #402..#409 (some ranges); R214-PERF-1..7 #410..#416; R214-SEC-1 #417, R214-SEC-4 #418, R214-CODE-1/3/4/5 #419..#422; R214-ARCH-4 → dup of ARCH4 #384; R214-ARCH-6 → dup of S11 #390; R214-ARCH-7 → dup of H6 #435; R214-ARCH-8 → dup of #430; R214-SEC-5 → dup of S14 #382
- R176 anchors: R176-PERF-N2 #429, R176-ARCH-M2 #430, R176-ARCH-M3 #431, R176-ARCH-N4 #432, R176-ARCH-NX #433
- R172 anchors: R172-SEC-H1 #436, R172-SEC-L4 #437, R172-ARCH-D5 #438, R172-ARCH-D9 #439 (CLOSED), R172-ARCH-D10 #440, R172-SEC-M4 → fixed (`SanitizeForLog` wired + `connector_sanitize_contract_test.go` grep-locks the call site), R172-ARCH-D1 → effectively addressed by router_*.go split (R230B-ARCH-1/R231-ARCH-3 ongoing); D3 → dup of ARCH1 #387; D8 → dup of TEST2 #388; PERF-M3 → observation (no action)
- R163 anchor: H9 #434 (CLOSED)
- H2 (CSP/onclick) #441
- H6 (node.Conn split) #435
- R110 anchors: R110-P1 home dashboard #445, R110-P1 sidebar response summary #446, R110-P2 cron card #447, R110-P2 project display #448, R110-P3 hover toolbar #449
- I18N-1 #450
- old "暂缓" architecture cluster (Router God Object / Server pkg / Dispatcher / session→discovery / send-with-broadcast / server business logic) → all duplicated by R214/RNEW/R231 cluster issues already filed
- "命名一致性 Get*/Fetch*/Load*" → cosmetic batch-rename; could be its own task but no functional risk; leaving in raw file for future cosmetic sweep
- "错误消息后端结构化" → covered by I18N-1 (#450) work item (per-locale error map is part of the i18n scope)
- "移动端会话卡片 X 按钮" / "Planner 进程资源监控" / "主题切换 浅色" — UX wishes, no anchor; out of scope for this triage round (wait for next UX review to anchor them)
- "[]byte(line) 每事件字符串拷贝" (cli/protocol_claude.go:59) → dup of R231-PERF-1 (R216-PERF-1 same family)
- LOW section: "parseCronAdd 双引号" / "Reverse node 重放" / "Cookie pre-auth wsAuthLimiter bypass" / "Watchdog timer AfterFunc Reset" — explicitly accepted / not real

### Skill notes

- **High discard rate confirmed**: 116 raw items → 4 new issues (#541, #546, #565, #571), 1 cosmetic, ~111 either already-filed (≈90 older anchors) or already-fixed / archived (≈21 R216 anchors).
- **R216 specifics**: 21 R216 anchors in the file (some "[~]" archived). 4 → bucket A new issues, 3 already-fixed (GO-2/GO-4/SEC-6), 2 contract-locked accepted (GO-1/GO-5), 12 archived to newer roll-up anchors.
- **Collision risk**: Round 216 was a deep multi-reviewer sweep covering ground that had already been opened (e.g. processIface god interface, Hub setter race, Cross-node error asymmetry). Most R216-ARCH/CR items had been re-discovered in R231/R230B and merged there; the originals are bucket C.
- **Mixed file content**: this raw file is not a "R216-only" dump; it includes ALL open items in TODO.md as of R216 time (Round 194, Round 110 carryovers, R172/R176/R214/RNEW). The user prompt's "all 116 are R216" is inaccurate — only ~21 are R216-prefixed. The other ≈95 are pre-existing items that already have issues filed by earlier batch agents.
- **Per-skill rule**: did not modify docs/TODO.md, did not push or commit; only created label, opened issues, edited cosmetic-backlog.md, and appended this Discarded section.

