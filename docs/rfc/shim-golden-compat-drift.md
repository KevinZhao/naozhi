# RFC: shim 契约 golden 兼容矩阵 + overlay 漂移可见 + server.go 拆分（#2543 B6）

## 问题

1. **跨版本兼容没有任何测试**。零停机重启的前提是老 shim 二进制写的 state
   文件能被新主进程读懂，但现有 9 个兼容测试全部是同一二进制进程内写→读；
   `internal/shim/testdata/` 不存在。`SpawnOverlay`（#2494/#2493）靠
   "additive omitempty 不 bump SchemaVersion" 的注释约定活着；
   `ProtocolVersion`（socket 握手）与 `State.SchemaVersion`（state 文件）
   双轨演进、无联动规则。
2. **overlay 漂移不可见**。老 shim 携带落盘时的 `SpawnOverlay` 存活，
   重连合并用它压过当前 config 默认——config 改了模型对存量 session 无效，
   任何界面都不提示。router 在 30s reconcile 里已经算出 `argsDrift`
   （bool），但只写日志。
3. **server.go 单文件 1299 行 / 38 函数**，混了 socket 监听+握手、CLI 子进程
   监督、state 持久化、限额、socket 文件生命周期五种职责。

## 方案

### 1. golden 兼容矩阵

- `internal/shim/testdata/state_v0.0.81.json`：按 v0.0.81 的 state.go json
  形态手工构造（该 tag 无 `spawn_overlay` 字段——正好覆盖 nil overlay 语义）。
- `internal/shim/testdata/state_v0.0.82.json`：当前 release 形态，
  `spawn_overlay` 五字段全填。
- 测试 `TestStateGoldenCompat` 遍历 `testdata/state_*.json`。git 检出的
  fixture 是 0644，而 `ReadStateFile` 拒绝 group/world 可读文件——loader
  必须先把 fixture 复制到 t.TempDir() 并以 0600 写入再读：
  - `ReadStateFile` 必须成功；
  - 逐字段与测试内嵌的期望值比对。红的机制按改动类别：改 json tag /
    改类型 → 读回零值 → 比对失败；删整个字段 → 期望值 struct 字面量
    编译失败。fixture 的每个 omitempty 字段（Backend / LastConnectedAt /
    SchemaVersion / 全部 overlay 字段）至少在一个 fixture 里取非零值并被
    断言，否则该字段的回归不可见。仅去掉 `,omitempty` 标记（marshal-only）
    不在本测试的守护范围（读路径无影响）；
  - overlay 语义：v0.0.81 fixture 读出 `SpawnOverlay == nil`，v0.0.82 非 nil
    且字段逐一相等。
- `mergeArgvLayers` 不 panic 的部分放 `internal/session`（import 方向），
  遍历同一 testdata 目录（相对路径 `../shim/testdata`）。
- 版本联动规则进 `TestShimVersionConstants_Coherence`：allowlist
  `map[schemaVersion]minProtocolVersion{1:1}`——schema bump 不同时改
  allowlist（含相应 protocol 下限）即红。
- 可持续性：`docs/ops/naozhi-deploy-skill.md` 加"发版归档"小节：**发版人**
  在打 tag 前检查本次 release 是否改过 `internal/shim/state.go` 的 State/
  SpawnOverlay 形态；改过则新增 `state_v<tag>.json` fixture + 期望值断言，
  未改则跳过（矩阵只在形态变化时增长）。fixture 值使用显式占位
  （`FIXTURE-NOT-A-REAL-TOKEN` 类），不得复制真实 state 文件。

### 2. overlay 漂移可见

数据源：reconcile（30s）里 `shimArgsDrift` 已产出 `storedBase`（shim 落盘
argv 去 --resume）与 `currentArgs`（当前 config 重合并后的 argv）。

- 新增纯函数 `session.overlayDriftFields(stored, current []string) []OverlayFieldDrift`：
  对 `--model` / `--effort` / `--append-system-prompt` 三个字段做"最后一次
  出现"解析（与既有 `cli.effortFromArgs` 同规则，加 round-trip 测试），
  逐字段输出 `{Field, Stored, Current}`，值相同的字段不输出。codex 等不以
  `--model/--effort` 为 argv token 的 backend 拿不到 per-field 名目——当
  整条 argv 不等但 per-field 无命中时输出一条 `{Field:"args"}` 兜底项，
  避免 codex 会话的漂移静默丢失。
- 存储遵循 ManagedSession 既有惯例（Snapshot 无锁读 → 字段自持原子性，
  见 managed.go workspace 字段注释）：`overlayDrift
  atomic.Pointer[[]OverlayFieldDrift]`，在 reconnectShims 里
  `shimArgsDrift` 产出 storedBase/currentArgs 的既有位置（r.mu 之外）
  直接写入——不新增锁，也绝不在 r.mu 写锁内调用 driftCompareArgs
  （其内部 RLock 同一把 r.mu，写锁包裹会自锁）。
  `snapshot()` 输出 `SessionSnapshot.OverlayDrift []OverlayFieldDrift
  \`json:"overlay_drift"\``——与 #2532 spawn_diags 相同的"恒为数组"契约，
  前端免判 undefined。语义="重启会话以应用新配置"（活会话不会被自动
  重启；自动重启只发生在无活进程的 shimStateDrift 路径）。
- dashboard：header detail 行加 `overlay-drift-tag`（复用 #2532 spawn-diag
  chip 的挂载/重绘模式），文案「配置漂移·重启会话以应用新配置」，tooltip
  列出逐字段 stored→current。一个 Playwright 用例。
- `naozhi shim list`：需要新增 `-config` flag（经 #2529 的
  `newSubFlagSet("shim list", "config.yaml")`，argv 面变化在 PR 里注明），
  对每个 state 文件（`SpawnOverlay != nil` 时）用 config 当前值重算合并层
  （新导出 `session.MergeArgvLayerValues` 包一层 `mergeArgvLayers`）。
  **tuning 修正**：state 落盘的 overlay 不含 dashboard tuning，直接比对会
  把每个被 tuning 过的健康会话永久标成 DRIFT，误导操作员 `shim stop` 杀
  健康会话。model / effort 两个 tuning 可影响字段在离线路径**不打 DRIFT
  行**，只打印 stored/current 两值 + "以 /api/sessions 的 overlay_drift
  为准"；append_system_prompt 与 extra_args 不受 tuning 影响，照常打
  DRIFT。issue 验收里的手工验证（改 config 默认模型）通过 /api/sessions
  的 overlay_drift 字段或 dashboard chip 验证。
- 同步更新 `docs/guides/shim-testing.md` 的 shim list 输出说明。

### 3. server.go 拆分（零行为）

同包纯移动，`git log --follow` 可追溯：

| 新文件 | 内容 | 预计行数 |
|---|---|---|
| server.go（保留） | 包注释、Config、timer 常量、shimServer 类型、Run、waitForReattach、initiateShutdown、idle timer、setClient/clearClient/enqueueWrite | ~500 |
| server_client.go | performHandshake、handleClient、runCommandLoop、handleClientCommand、writeMsg、writeRaw | ~380 |
| server_cli.go | startCLI、cliProc 全部方法、readStdout/readStderr、tryExtractSessionID | ~250 |
| server_state.go | saveState、saveStateCLIDead | ~30 |
| server_limits.go | 6 个限额 getter/setter + 常量 | ~90 |
| server_socketfile.go | CleanStaleSocket、ensureSocketFreeForReuse、WaitSocketGone、watchSocketFile、socketDir | ~130 |

命名用 `server_*.go` 前缀（同包内聚，避免与 manager.go 家族混淆）。
所有现有测试不改；`gofmt`、`go test -race ./internal/shim` 全绿为过门条件。

## 替代方案

- golden 用 CI 下载旧 release 二进制真实起 shim 落盘：最真实，但 CI 需要
  可执行旧版本 + 起真进程（网络/签名/平台矩阵成本高）。手工构造 fixture
  以 tag 处的 state.go 为依据，等价覆盖"读旧文件"路径；后续 release 归档
  真实文件可逐步替换。
- 漂移比较放 `/health` 汇总：单 session 粒度信息会丢；/api/sessions
  per-session 字段让 dashboard 直接消费，doctor/脚本也能读。
- server.go 拆为子包：跨包要导出大量内部符号，收益低于风险；同包多文件
  已满足"每文件一种职责 ≤500 行"。

## 迁移步骤

1. golden fixtures + 兼容测试 + 版本联动测试（纯新增）。
2. overlay drift 纯函数 + reconcile 接线 + snapshot 字段 + 前端 chip +
   Playwright。
3. server.go 纯移动拆分。
4. docs/ops/naozhi-deploy-skill.md 加归档步骤。

## 验收

见 issue #2543 验收标准；其中"老 shim + 新主进程重连测试"由 v0.0.81
fixture（无 spawn_overlay 的 state）经 `ReadStateFile` + reconcile 路径的
现有单测覆盖（fixture 驱动），不引入真实旧二进制。

## 风险

- argv 字段解析（--model/--effort/--append-system-prompt）与 BuildArgs 的
  渲染规则漂移 → 解析 helper 与 `effortFromArgs` 同置 internal/cli 或
  session，并加 round-trip 测试（BuildArgs 渲染 → 解析 → 相等）。
- reconcile 写 ManagedSession：atomic.Pointer，无锁（见上）；绝不在
  r.mu 写锁内调用 driftCompareArgs。
- 拆分引入隐式 init 顺序问题：无 init()；纯函数移动。
