# ADR-0001 PR-1: 命名碰撞审计报告（Get* / Fetch* / Load*）

- **Status**: Audit artifact (PR-1 deliverable)
- **Date**: 2026-06-06
- **关联**: [`0001-getter-fetch-load-naming.md`](./0001-getter-fetch-load-naming.md) §f PR-1，#463
- **范围**: 纯审计文档，**零生产代码改动，零 rename**。本报告枚举受 ADR-0001 决策 (a)/(b)/(c)/(d) 影响的全部导出符号，为后续 PR-2（去 `Get` 前缀）/ PR-3（本地 `Fetch*`/`Load*` 统一为 `Load*`）提供执行清单与碰撞规避建议。

引用计数说明：`prod` = 非 `_test.go` 文件里 `.Sym(`/`Sym(`/`func ...) Sym` 的 grep 命中行数；`test` = `_test.go` 里的命中行数。计数为量级参考，非精确 AST 引用数（grep 会把定义行、接口声明行、注释命中都算入）。实际 rename 以 `gopls rename` 跨包级联结果为准。

---

## 决策 (a)：去 `Get` 前缀的本地 accessor

下列方法返回已持有的内存/磁盘状态，按 `io.Reader.Read` 惯例应去 `Get` 前缀。标注是否为**接口方法**（接口方法 `gopls rename` 会级联所有实现 + 所有 caller，影响面更大）。

| 符号 | 定义位置 | 接口方法？ | prod 引用 | test 引用 | 建议新名 | 碰撞风险 |
|------|----------|-----------|----------:|----------:|----------|----------|
| `Router.GetSession` | `internal/session/router_core.go:1637` | **是** — 接口声明于 `dashboard/ext/scratch/deps.go:26`、`server/consumer.go:58,70`、`upstream/consumer.go:63`、`wshub/types.go:54`（5 处接口契约 + 1 处测试 fake `scratch/router_iface_test.go:35`） | 24 | 29 | **`SessionFor`** 或 `Lookup`（**不可**裸 `Session`，见 (d)） | **高** — 见决策 (d) 碰撞 |
| `Router.GetOrCreate` | `internal/session/router_lifecycle.go:286` | **是** — 接口声明于 `cron/scheduler_config.go:152`、`dispatch/consumer.go:36`、`upstream/consumer.go:73`、`wshub/types.go:53`；另有 cron/dispatch 多个测试 fake + `wireup/cron_router_adapter.go:121` 适配器实现 | 16 | 50 | **保留 `GetOrCreate`** 或改 `EnsureSession`——裸 `OrCreate` 不通顺（ADR 显式标注）。建议**保留**，理由见下「`GetOrCreate` 特例」 | 中 — 接口级联最广（test 引用 50） |
| `Router.GetWorkspace` | `internal/session/router_workspace.go:144` | **是** — 接口声明于 `dispatch/consumer.go:49`、`server/consumer.go:71`、`wshub/types.go:58` | 8 | 26 | **`Workspace`** 或 `WorkspaceFor` | 低 — 无同名类型 |
| `Router.GetSessionBackend` | `internal/session/router_backend.go:309` | 否（无接口声明命中；仅具体方法） | 1 | 3 | **`SessionBackend`** | 低 |
| `cli.Process.GetState` | `internal/cli/process.go:787` | **是** — 接口声明于 `cli/process_facets.go:68`、`session/managed.go:223`；实现还有 `session/testutil.go:105`（TestProcess，**生产构建**，`//go:build !release` 无 `_test.go` 后缀） | 5 | 19 | **`State`** | 低 — 但注意 `State` 作为方法返回 `ProcessState`，类型名带 `Process` 前缀不撞 |
| `cli.Process.GetSessionID` | `internal/cli/process.go:810` | **是** — 接口声明于 `cli/process_facets.go:70`、`session/managed.go:222`；实现还有 `session/testutil.go:104`（TestProcess） | 9 | 7 | **`SessionID`** | 低 — 注意 `cron.Session` 接口已有 `SessionID()` 方法（语义一致，不同包不碰撞，反而对齐） |
| `cli.Process.GetTotalTimeout` | `internal/cli/process.go:949` | 否 | 1 | 2 | **`TotalTimeout`** | 低 |
| `Scheduler.GetRun` | `internal/cron/scheduler_finish.go:106` | **是** — 接口声明于 `cron/scheduler_finish.go:53` | 3 | 7 | **`Run`** 或 `RunByID`——裸 `Run` 可能与 cron 动词语义混淆，建议 `RunByID`/`FindRun` | 中 — `Run` 在 cron 包语境下歧义 |
| `nodeAccessor.GetNode` / `hubNodeLookup.GetNode` | `internal/server/node_accessor.go:49`、`select_node_for_backend.go:169` | **是** — 接口声明于 `dashboard/ext/agentevents/deps.go:19`、`dashboard/project/deps.go:23`、`dashboard/session/deps.go:19`、`server/node_accessor.go:15`、`server/select_node_for_backend.go:77`（5 处接口契约，多实现） | 5 | 7 | **`Node`** 或 `NodeByID` | 低 — `node.Conn` 是返回类型，`Node` 方法名不与之撞 |

### `GetActiveCount` / `GetCurrentBackend`（保留名，未声明）

allowlist 里这两个是 #870 lifecycle contract 的**预留名**，当前**未在任何文件声明**（`grep` prod=0 test=0）。无需 rename，无碰撞。PR-2 不涉及。

### `ScratchPool.Get`（map-style accessor）

`internal/session/scratch.go` 的 `*ScratchPool.Get` 是 map 风格的访问器（`m.Get(key)`），allowlist 里以裸 `Get` 冻结。它**不是** `GetXxx` 形式的 getter 前缀债务，去前缀后会变成无名方法，**保留 `Get`**，PR-2 不动。

### `GetOrCreate` 特例（ADR (a) 显式标注）

ADR 决策 (a) 在表述里明确：`GetOrCreate` 去前缀变裸 `OrCreate` **不通顺**。本审计**建议保留 `GetOrCreate` 原名**（它语义上不是纯 getter，而是 get-or-create 的复合动作，`Get` 在此是动词而非 getter 前缀，符合 Go 习惯如 `sync.Map.LoadOrStore`）。备选 `EnsureSession`。最终取舍留给 PR-2 执行者，但本审计倾向**保留**——它是接口级联最广的符号（4 处接口契约 + 9 个测试 fake + 1 个适配器），rename 收益最低、风险最高。

---

## 决策 (d)：碰撞清单 + 安全替代名

### D-1（关键碰撞）：`Router.GetSession` → 裸 `Session` 撞 `cron.Session` 类型

- **碰撞源**：`internal/cron/agent_opts.go:62` 已有 `type Session interface { Send(...); SessionID() string; InterruptViaControl() ... }`。
- **风险**：机械地把 `GetSession()` rename 成裸 `Session()`，会在同时引用 `cron.Session` 类型与持有 `*session.Router` 的作用域（如 `internal/wireup/cron_router_adapter.go`、cron 调度路径）里制造 method-name-vs-type-name 的可读性混淆。即使分属不同包不会编译失败，可读性退化明确。
- **决策 (d) 裁定**：**绝不**用裸 `Session()`。
- **安全替代名**：`SessionFor(key)`（首选，描述「按 key 取 session」）或 `Lookup(key)`。本审计**首选 `SessionFor`**。

### D-2（潜在歧义，非硬碰撞）：`Scheduler.GetRun` → 裸 `Run`

- `Run` 在 cron 包语境是高频动词（执行 job/run）。裸 `Run()` 作为「按 ID 取 CronRun」的 getter 语义不清。
- **建议**：`RunByID(jobID, runID)` 或 `FindRun`，不用裸 `Run`。

### D-3（无碰撞，记录确认）

- `GetState → State`、`GetSessionID → SessionID`、`GetWorkspace → Workspace`、`GetSessionBackend → SessionBackend`、`GetTotalTimeout → TotalTimeout`、`GetNode → Node`：经检查，去前缀后的裸名**不与同包内现有类型/符号撞名**。
  - `GetSessionID → SessionID`：`cli.Process` / `session` 侧无 `SessionID` 类型；与 `cron.Session.SessionID()` 同名但跨包且语义一致（都是「返回 session id」），属对齐而非碰撞。
  - `GetState → State`：返回类型是 `ProcessState`（带前缀），方法名 `State` 不与之撞。
  - `GetNode → Node`：返回 `node.Conn`，方法名 `Node` 不与 `node` 包名/`node.Conn` 撞（不同包限定）。

---

## 决策 (b)/(c)：`Fetch*` vs `Load*`

### (c) 网络往返 — **保留 `Fetch`**（不在 PR-3 范围）

`NodeFetcher` 接口族（`internal/node/conn.go:46-50`）执行真实网络往返（reverse-RPC / HTTP），保留 `Fetch` 动词。两套实现：`HTTPClient`（`httpclient.go`）+ `ReverseConn`（`reverseconn.go`）。**不改名**。

| 符号 | 接口声明 | 实现 | prod+test 引用 | 处置 |
|------|----------|------|---------------:|------|
| `FetchSessions` | `node/conn.go:46` | `httpclient.go:142`、`reverseconn.go:257` | 28 | **保留** |
| `FetchProjects` | `node/conn.go:47` | `httpclient.go:211`、`reverseconn.go:266` | 11 | **保留** |
| `FetchDiscovered` | `node/conn.go:48` | `httpclient.go:231`、`reverseconn.go:275` | 11 | **保留** |
| `FetchDiscoveredPreview` | `node/conn.go:49` | `httpclient.go:251`、`reverseconn.go:284` | 9 | **保留** |
| `FetchEvents` | `node/conn.go:50` | `httpclient.go:164`、`reverseconn.go:293` | 18 | **保留** |

确认：repo 内**无其它** `Fetch*` 方法定义——全部 5 个 `Fetch*` 都属 `node` 网络族（见 `grep -rn "func.*) Fetch"`）。因此 (c) 的「保留 Fetch」覆盖了所有现存 `Fetch*`，PR-3 **不会**碰到任何 `Fetch→Load` 改名（这正是 ADR (c) 显式纠正 RFC §3 过宽规则的结果）。

### (b) 本地检索 `Load*` — 已是 `Load`，无 rename 工作

本地内存/磁盘检索方法已经叫 `Load*`，符合 ADR (b)。PR-3 的「统一为 Load*」对它们是 no-op（已合规），仅作命名一致性确认：

| 符号 | 接口 / 定义 | 说明 |
|------|-------------|------|
| `HistorySource.LoadBefore` | 接口 `cli/history.go:89`；实现 5 处：`cli/history.go:99`（Noop）、`history/claudejsonl/source.go:79`、`history/kirojsonl/source.go:152`、`history/naozhilog/source.go:132`、`history/merged/source.go:68` | 本地分页加载，已是 `Load` |
| `naozhilog.Source.LoadLatest` | `history/naozhilog/source.go:98` | 本地加载最新，已是 `Load` |
| `HistoryLoader.LoadHistoryChainTail` | 接口 `session/router_core.go:762`；实现 `router_core.go:769`（wrap `discovery.LoadHistoryChainTailCtx`） | 本地 JSONL 链遍历，已是 `Load` |

**结论**：(b)/(c) 两条决策对现有代码库均**零 rename**——`Fetch*` 全部保留（网络语义），`Load*` 全部已合规。PR-3 实际上只需在文档/护栏层确认无新增违规，无生产代码改动。

---

## PR-2 执行清单汇总（供后续 PR 参考，本 PR 不执行）

逐符号 `gopls rename`（决策 (e)，**绝不用 sed**——`GetSession` 是 `GetSessionID`/`GetSessionBackend` 子串）：

1. `Router.GetSession` → `SessionFor`（接口级联 5 处 + 实现 + caller；避开 `cron.Session` 碰撞）
2. `Router.GetWorkspace` → `Workspace`（接口级联 3 处）
3. `Router.GetSessionBackend` → `SessionBackend`（无接口）
4. `cli.Process.GetState` → `State`（接口级联 2 处 + TestProcess）
5. `cli.Process.GetSessionID` → `SessionID`（接口级联 2 处 + TestProcess）
6. `cli.Process.GetTotalTimeout` → `TotalTimeout`（无接口）
7. `Scheduler.GetRun` → `RunByID`（接口级联 1 处）
8. `GetNode` → `Node`（接口级联 5 处，多实现）
9. `Router.GetOrCreate` → **建议保留**（或 `EnsureSession`，执行者裁定）
10. `ScratchPool.Get` → **保留**（map accessor，非前缀债务）
11. `GetActiveCount` / `GetCurrentBackend` → 预留名未声明，无操作

**每符号 rename 后必须立即 `go build ./...`**（漏改→编译失败即时暴露）。PR-2 须**同步更新** `getter_surface_test.go` 的 allowlist（移除被 rename 掉的旧名），否则护栏会因「allowlist 残留不存在的符号」而……实际上不会——该护栏只对**超出 allowlist 的新符号**报错，残留 allowlist 条目是无害的（见测试注释 line 54）。但 PR-2 应清理 allowlist 保持准确。

---

## on-disk / wire / config / log / metric 零变化确认

ADR 通用红线：rename 只动 Go 标识符。本审计涉及的全部符号都是**方法名**（Go 标识符），不是字符串字面量。`gopls rename` 仅改引用、不碰字符串/注释/JSON tag/log 字段/metric 名。本 PR-1 **不执行任何 rename**，因此 on-disk 格式、wire 协议、config key、log 字段名、metric 名**全部零变化**。PR-2/PR-3 执行时须用 `gopls rename`（非 sed）保证同样的不变量。

---

## 护栏（lint）审阅结论

`internal/session/getter_surface_test.go` 的 `TestGetterSurfaceFrozen` 已是有效护栏：

- **机制**：AST walker（`parser.ParseFile` + `ast.Inspect`）扫描 `internal/session` + `internal/cli` 生产文件，任何**新增**的导出 `Get*`/`Fetch*` 方法若不在冻结 allowlist 内即 RED-fail。已能阻止 PR-2/PR-3 之外悄悄新增 getter 债务。
- **scope 正确性**：只扫 `internal/session` + `internal/cli`，与 ADR (c) 一致——`internal/node` 的网络 `Fetch*` 故意 OUT-of-scope，不被冻结也不被改名。
- **gap 评估**：本 PR **不补 allowlist、不改护栏**。审阅认为护栏 scope 与 ADR §c 精确对齐，allowlist 当前与生产符号一致（测试绿）。PR-1 对护栏**零改动**——allowlist 内容的同步留给 PR-2（rename 后清理旧名）。
- **验证**：`go test ./internal/session/ -run TestGetterSurfaceFrozen` → PASS（baseline 绿）。

---

## References

- [`0001-getter-fetch-load-naming.md`](./0001-getter-fetch-load-naming.md) — 本 ADR（决策 (a)-(f)）
- `internal/cron/agent_opts.go:62` — `type Session interface`（决策 (d) 碰撞源）
- `internal/session/getter_surface_test.go` — `TestGetterSurfaceFrozen` 活护栏
- `internal/node/conn.go:46-50` — `NodeFetcher` 网络 `Fetch*` 族（决策 (c) 保留）
