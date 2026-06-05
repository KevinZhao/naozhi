# ADR-0001: `Get*` / `Fetch*` / `Load*` 命名约定收口

- **Status**: Accepted
- **Date**: 2026-06-05
- **关联 issue**: #463 (CURATED-NAMING-1)
- **来源**: 把 [`docs/rfc/lifecycle-policy-and-naming.md`](../rfc/lifecycle-policy-and-naming.md) §1.2 / §3 / §8 的命名散文固化为可执行的决策记录，作为后续执行型 PR 的批准门（gate）。本 ADR **纯文档、零生产代码、零行为变化**，不动任何已 pin 的不变量。

---

## Context

Go 惯用 getter 不带 `Get` 前缀（`io.Reader.Read`，而非 `GetRead`）。naozhi 现状（实测）偏离这一惯例：

- `\.GetSession|\.GetOrCreate|\.GetWorkspace|\.GetActiveCount|\.GetSessionID|\.GetCurrentBackend|\.GetSessionBackend|\.FetchSession|\.LoadHistory` 跨 `internal/` + `cmd/` 共 **165 处引用**（issue 写 "125+"，实测更多，以 RFC §1.2 为准）。
- 方法定义侧：`Router.GetSession`（`internal/session/router_core.go`）、`GetOrCreate`（`router_lifecycle.go`）、`GetWorkspace`（`router_workspace.go`）、`GetSessionBackend`（`router_backend.go`）；`cli.Process.GetState` / `GetSessionID` / `GetTotalTimeout`（`internal/cli/process.go`）；以及 `LoadHistoryChainTail` / `LoadBefore` 等。

两条 source-of-truth 之间存在一处需要调和的矛盾：

- RFC §3 草拟的规则把 `Fetch*/Load*` **一律**统一为 `Load*`。
- 但活的护栏测试 [`internal/session/getter_surface_test.go`](../../internal/session/getter_surface_test.go) 的 `TestGetterSurfaceFrozen` scope note（第 33–40 行）已显式声明：`node.FetchSessions`（`internal/node/conn.go` `NodeFetcher`，`httpclient.go` / `reverseconn.go`）**故意保留 `Fetch` 动词**——它走 reverse-RPC / HTTP 传输做真实网络拉取，`Load` 会误导为内存/磁盘检索。

本 ADR 把这两处来源调和成**单一权威决策**，并确认 caller-visible rename 不满足 triage 的 "zero functional impact" 门槛，因此该项已从 `docs/cosmetic-backlog.md` 升级进 issue tracker（#463）。

此外存在**命名碰撞**：`internal/cron/agent_opts.go:62` 已有 `type Session interface`（`Send` / `SessionID` / `InterruptViaControl`）。机械地把 `GetSession()` rename 成裸 `Session()` 会在同时引用该类型与持有 router 的作用域里制造 method-vs-type 的可读性混淆，必须先做碰撞审计。

---

## Decision

### (a) 本地 accessor 去 `Get` 前缀

对返回已持有的内存/磁盘状态的本地 accessor，**去掉 `Get` 前缀**，遵循 `io.Reader.Read` 惯例（`GetState → State`、`GetWorkspace → Workspace` 等）。

### (b) `Fetch*` / `Load*` 仅对本地检索统一为 `Load*`

对**本地**（内存 / 磁盘）检索语义的方法，把 `Fetch*` / `Load*` 统一为 `Load*`。

### (c) 对真实网络往返 RETAIN `Fetch` 动词

`node.FetchSessions`（以及 `NodeFetcher` 接口的同族 `FetchProjects` / `FetchDiscovered` / `FetchDiscoveredPreview` / `FetchEvents`）执行真实网络往返，**保留 `Fetch` 动词**。`Load` 隐含内存/磁盘检索，会误标网络拉取。这条**显式纠正** RFC §3 过宽的 "Fetch→Load" 规则，并与 `getter_surface_test.go` scope note 对齐——该测试的 #463 护栏只冻结 `internal/session` + `internal/cli` 的本地 accessor，`internal/node` 的网络 fetch 动词不在其管辖内、也不被本 ADR 改名。

### (d) 碰撞规则：`GetSession` 不得变成裸 `Session`

`GetSession` rename **绝不**采用裸 `Session()`——它与 `internal/cron/agent_opts.go:62` 的 `type Session interface` 碰撞。改用 `SessionFor` 或 `Lookup` 等替代名，规避 method-vs-type 混淆。任何在执行期发现的同类碰撞按同一原则处理：选描述性替代名，不选与现有类型同名的裸名。

### (e) 工具强制：逐符号 `gopls rename`，绝不用 sed

rename 必须走 `gopls rename` **逐符号**执行，**绝不用 `sed`**：`GetSession` 是 `GetSessionID` / `GetSessionBackend` 的子串，sed 会误伤；且 sed 无法跨包更新导出符号引用、无法处理 `Session` 类型碰撞，也会错改注释/字符串字面量。`gopls rename` 理解 Go 语义（跨包、跨文件、只改引用），可逐符号原子提交。

### (f) 执行 PR 切分顺序

实际 165 处 rename **不在本轮**——本 ADR 只批准约定与顺序，执行 PR 在后续轮次：

- **PR-1**：碰撞审计（产出受 (d) 影响的全部符号清单）+（可选）lint 禁止**新增** `Get*` getter。
- **PR-2**：`Get*` 去前缀（`GetSession → SessionFor`/`Lookup` 等），逐符号 `gopls rename`，避开 `cron.Session` 碰撞。
- **PR-3**：`Fetch*` / `Load*` 本地检索统一为 `Load*`（不动 (c) 的网络 `Fetch*`）。
- 每 PR 独立 `go build ./... && go vet ./... && go test ./...` 全绿、独立 merge、独立可 `git revert`。

---

## Guard formalized

`TestGetterSurfaceFrozen`（[`internal/session/getter_surface_test.go`](../../internal/session/getter_surface_test.go)）是本 ADR 形式化的**活护栏**：它用 AST walker 冻结 `internal/session` + `internal/cli` 生产文件里导出的 `Get*` / `Fetch*` accessor allowlist。

- 本 ADR **不拓宽也不矛盾**该 allowlist——它只记录意图，不新增/不 rename 任何 accessor，因此护栏在本轮保持 GREEN。
- 当未来某 PR 新增 `Get*` / `Fetch*` getter 时护栏 RED-fail，作者须按本 ADR 方向 rename，或在同一 PR 里有意识地扩 allowlist（把命名决策记录在案）。
- 本 ADR 保留该测试的 scope 豁免：`node.FetchSessions` 保留 `Fetch` 动词（网络 fetch，非本地 accessor）——见决策 (c)。

---

## Consequences

- **正面**：命名向 Go 惯例收口；新增 getter 债务被护栏阻止增长；执行期碰撞与工具风险被 (d)/(e) 前置规避。
- **代价 / 风险**：165 处跨 50+ 文件的 caller-visible rename。漏改 → 编译失败（即时暴露，低风险）；`Session` 类型碰撞 → 可读性退化（中风险，靠 (d) 审计规避）；在途 PR 的 merge 冲突（中风险，靠分小 PR + 快速 merge 降低）。
- **兼容性**：破坏性仅在源码标识符层；naozhi 是单仓应用、无外部 import 该 `internal` 包，无下游兼容负担。on-disk / wire / config 全不变（rename 只动 Go 标识符；log 字段名、metric 名是字符串字面量，`gopls` 不动）。
- **回滚**：每个执行 PR 是独立原子 rename，`git revert` 单 PR 即回滚，无运行时状态残留。

---

## References

- [`docs/rfc/lifecycle-policy-and-naming.md`](../rfc/lifecycle-policy-and-naming.md) §1.2（问题陈述）/ §3（备选方案，含本 ADR 纠正的过宽 Fetch→Load 规则）/ §8（PR 切分与 ADR 门）
- [`internal/session/getter_surface_test.go`](../../internal/session/getter_surface_test.go) `TestGetterSurfaceFrozen`（本 ADR 形式化的活护栏，含 `node.FetchSessions` scope 豁免）
- `internal/cron/agent_opts.go:62` `type Session interface`（决策 (d) 碰撞源）
