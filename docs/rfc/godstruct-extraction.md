# ARCH-GODSTRUCT — server 包 god-struct 抽取（增量步骤 G1–G4）

| 字段 | 值 |
| :--- | :--- |
| 状态 | G1 Implemented · G2–G4 Draft（v2，2026-06-21；v2 按对抗性 review 重排步骤 + 订正事实） |
| 作者 | naozhi team |
| 创建日期 | 2026-06-21 |
| 修订日期 | 2026-06-21 |
| 关联代码 | `internal/server/server.go`<br/>`internal/server/wshub.go`<br/>`internal/server/node_accessor.go`<br/>`internal/server/consumer.go`<br/>`internal/wshub/types.go`<br/>`internal/server/shutdown_lock_order_test.go`<br/>`internal/server/hub_shared_state_test.go`<br/>`tools/lint-server-handlers/` |
| 关联设计 | `docs/design/server-split-phase4-design.md`（Phase 0-5 总体计划，**本 RFC 与之协调，见 §0.1**）<br/>`docs/design/server-split-phase4-baseline.md`（字段 baseline） |
| 关联 RFC | `docs/rfc/consumer-interfaces.md`（IoC 接口约束） |
| 关联 issue | #2195 (Hub god-struct + wshub 空壳) · #2192 (nodes 注册表无单一所有者) · #2197 (Server god-struct) · #376/R248-ARCH-6（Hub 子结构锚点） |

## 0. 摘要

三个 issue 指向 `internal/server` 的 god-struct 技术债（#2197 Server、#2195 Hub+wshub、#2192 nodes 注册表）。本 RFC 提出**风险分层、move-only 优先**的增量步骤，每步独立 PR、以编译 + 既有测试为不变量兜底。

步骤命名用 **G1–G4**（不用 "Phase 1-5"），以**避免与既有 `server-split-phase4-design.md` 的 Phase 0-5 整数碰撞**（见 §0.1）。

**本 RFC 承诺实施的是 G1（wshub 死接口清理）**——经对抗性 review 验证为最隔离、零行为变更、与既有设计方向无冲突的一步。G2–G4 经各自 review 后再分别落地。

### 0.1 与既有 `server-split-phase4-design.md` 的关系

既有设计文档（v0.6，方案 B）规划了完整的 server 包瘦身（Server 47→≤12 字段、≤5000 行），其推荐合并顺序是 **Phase 0 → 4a/4b/4c（Hub 搬到 wshub 包）→ 1 → 2 → 3a-3f → 5**，且**明确否决了 handler-grouping**：将 god-struct 折叠为「god struct + 12 个 view」是 v0.1 reviewer 一致指出的「换汤不换药」，registration-only handler 的终态是**删成 `routes.go` 局部变量**而非永久子结构（design.md:107）。

**本 RFC 的定位 = 既有大计划的补充小步（ORTHOGONAL increments）**，只做不与方案 B 冲突、且能立即落地的低风险清理：

- ✅ **G1（wshub 死接口清理）** 与方案 B 不冲突——方案 B 的 Phase 4 是把 Hub 搬「进」wshub 包，而 wshub 当前是 Phase 4a 残留空壳（仅剩接口骨架，Phase 4a 的 49-字段 mirror 已于 #1741 删除）。清理死接口 + 把 live 的 `HubRouter` 移回 server，是为方案 B Phase 4 让路的预清理，不是反向。
- ⚠️ **G4（Server handler-grouping，#2197）** 与方案 B 终态冲突（方案 B 要把 registration-only handler 删成 locals，而非收进子结构）。**因此本 RFC 不推进 G4 的"收进子结构"做法**；#2197 的正解应并入方案 B 的 Phase 5（handler 外移），本 RFC 仅记录此结论，不实施。

## 1. 侦察事实（已核实，订正 issue + v1 RFC 的错误前提）

侦察 + 对抗性 review（git show origin/master + grep + CI config）核实如下，**订正了若干流传的错误前提**：

1. **不存在编译期"字段计数"测试。** "48/47 字段"是文档数字，实测 origin/master 的 Server/Hub 均 **51 字段**（baseline 文档 47/49 全部 stale）。所谓"awk 字段计数"只是 baseline.md 里的验证命令，非 test。`lint-fact-table` 只比对 markdown 内 prose-vs-speech-table，不数真实字段。
2. **`internal/wshub` 不是 dead code，但 6 个接口里 5 个是死的。** `types.go` 含 6 个 interface；唯一 live 的是 `HubRouter`（14 方法），被 `consumer.go:44` 用作 `type HubRouter = wshub.HubRouter` type alias，`*session.Router` 结构化满足、受 `internal/session/contract_test.go` 守护。另 5 个（`MessageEnqueuer`/`CronView`/`ScratchOps`/`UploadOps`/`Auth`）零 live 引用；其中 `MessageEnqueuer` 已与 server 包同名 interface 漂移（缺 `evictedID` 返回值），是明确的腐化证据。
3. **`internal/server` 有两个硬 gate（都 blocking）**，不是 v1 RFC 说的"唯一一个"：
   - **`shutdown_lock_order_test.go`** — Hub 锁序（`mu ⊃ authMu`、`mu ⊃ eventLog.subMu`），含**硬编码文件列表** `wshubLockOrderFiles`（新 wshub_ 文件不自动覆盖）+ 字面量锚点 `LOCK ORDER CONTRACT (R35-REL2)`。
   - **`hub_shared_state_test.go`** — 用 `reflect.ValueOf(s.hub.nodes).Pointer()` 断言 `Server.nodes`/`Hub.nodes` 是同一 map header、`s.hub.nodesMu == &s.nodesMu` 同一把锁。**G2（nodes 注册表）必须改写此测试**为新不变量（Server/Hub 共享同一 `*nodeRegistry` 实例），不能声称"不触及"。
   - 其余 `lint-fact-table`、`lint-server-handlers` field_block rule 3a 是 CI `continue-on-error: true` 的 **warn/non-blocking**。`tools/check-router-fields`（`-mode fail`，blocking）只作用于 **Router**，不作用于 Server/Hub。

## 2. 增量步骤（按风险，每步独立 PR）

| 步骤 | 范围 | issue | 风险 | 行为变更 | 硬 gate 触及 | 本 RFC 实施 |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **G1** | 删 `internal/wshub` 5 个死接口，`HubRouter` 移回 server | #2195(②) | 低 | 无 | 无 | ✅ 本次 |
| **G2** | `nodes`/`nodesMu`/`knownNodes` 收敛为单一 owner `*nodeRegistry` | #2192 | 中 | 无 | **改写 `hub_shared_state_test.go`** | 后续 |
| **G3** | Hub 子结构抽取（Subscriber/Broadcast/Send，拆 3 子 PR） | #2195(①) | 高 | 无 | **扩 `wshubLockOrderFiles` + 保 R35-REL2** | 后续 |
| **G4** | Server handler 字段瘦身 | #2197 | — | — | — | ❌ 并入方案 B Phase 5（见 §0.1） |

### 2.1 G1 详细设计（本次实施）

**目标**：消除 `internal/wshub` 的腐化死接口，把唯一 live 的 `HubRouter` 收回 server 包，移除 server→wshub 的 import 边。

**改动**：
1. 在 `internal/server` 新增（或就近 `consumer.go`）一个直接的 `HubRouter` interface 声明（14 方法，与原 `wshub.HubRouter` 逐字一致），取代 `consumer.go:44` 的 `type HubRouter = wshub.HubRouter` 别名。
2. 删除 `import "github.com/naozhi/naozhi/internal/wshub"`（consumer.go）。
3. 删除整个 `internal/wshub` 包（`types.go` 是唯一文件）——其余 5 个接口零 live 引用，直接随包删除。
4. 清理对 wshub 的 stale 引用：
   - `tools/lint-server-handlers/rule_field_block.go:55` 里"Phase 4 后改 scan internal/wshub"的注释假设。
   - `internal/server/cronview.go` / `internal/dashboard/cronview/cronview.go` 中以 wshub 别名模式为设计先例的**注释**（更新为不再引用已删包）。
   - `consumer-interfaces.md` / baseline 对 wshub 作为 Phase 4b landing zone 的引用（加一行说明 Phase 4a 空壳已清）。

**必须保**：
- `HubRouter` 的 14 方法集与 `*session.Router` 的实现一致——`internal/session/contract_test.go` 守护；若该 contract test 引用 `wshub.HubRouter`，同步改指 server 包的新声明。
- 不引入 import cycle：wshub 原本 import dispatch + session，server 已 import 两者，把 `HubRouter` 移进 server 无新边。

**验证**：
- `go build ./...` + `go vet ./internal/server/ ./internal/session/`。
- `go test ./internal/server/ -race`（含 hub_shared_state / shutdown_lock_order — G1 不碰 Hub struct/锁，应原样通过）+ `go test ./internal/session/`（contract_test）。
- `grep -r "internal/wshub"` 全仓库零生产命中（仅允许 CHANGELOG/RFC 历史提及）。
- `gofmt`。

**回滚**：单 PR，纯接口搬迁 + 删包，可整体 revert。

### 2.2 G2–G3 概要（后续 PR，本次不实施）

- **G2（#2192，中风险）**：新建 `*nodeRegistry`（持 `mu`+`nodes`+`knownNodes`，暴露 `Add`/`Remove`/`NodeByID`/`Snapshot`/`Status`/`Known`）。`Server`+`Hub` 各持 `*nodeRegistry` 指针；`OnRegister`/`OnDeregister` 改调 registry 方法；`nodeAccessor`（读侧雏形）并入/委托。**必须**：① 改写 `hub_shared_state_test.go` 为"共享同一 registry 实例"不变量；② 保 reverseserver owns-check、unregister sync.Pool 复用、health 两路径一致；③ 4 个 dashboard 包的 subset interface 仍被满足（discovery 只要 `HasNodes`+`LookupNode`，agentevents 无 `NodesSnapshot`）。
- **G3（#2195①，高风险）**：按锚点抽 `SubscriberRegistry`/`BroadcastDispatcher`/`SendCoordinator`，**拆 3 子 PR**。每个必须：① 新文件加入 `wshubLockOrderFiles`；② 保 `LOCK ORDER CONTRACT (R35-REL2)`；③ 新文件带 `WRITES:`/`READS-ALSO:`/`LIFECYCLE-METHOD` marker；④ `-race` 全过。

## 3. 不做什么

- 不动 `scheduler.go` 拆分（#2193）——无 CI gate、价值低。
- 不推进 Server handler-grouping 收子结构（G4）——与既有方案 B 终态（删成 locals）冲突，应并入 Phase 5。
- G1 不碰 Hub struct、不碰 nodes、不碰任何 wshub_*.go 文件、不动锁。
- 不翻 lint-server 为 fail-mode（独立决策）。

## 4. 验证矩阵

| 步骤 | 编译 | 既有测试 | 硬 gate | 文档同步 |
| :--- | :--- | :--- | :--- | :--- |
| G1 | `go build ./...` | server -race + session contract_test | 不触及（不碰 Hub/锁/nodes） | wshub 引用清理（lint 注释 + 设计文档一行） |
| G2 | `go build ./...` | server -race + dashboard 各包 | **改写 hub_shared_state_test.go** | baseline nodes 节 |
| G3 | `go build ./...` | server -race | **扩 wshubLockOrderFiles + 保 R35-REL2** | baseline §3 + wshub.go 字段图 |

## 5. 对抗性 review 留痕（v2）

v1 RFC 经 3 视角对抗 review，关键修正：
- **[major, 已验证]** 发现第二个硬 gate `hub_shared_state_test.go`（v1 漏，recon 只 grep 了 NumField/TypeOf 未覆盖 reflect.Pointer）→ §1.3 补列，G2 验证矩阵更新。
- **[major]** v1 的 Phase 1（handler-grouping）与既有 design.md 否决方向冲突 → 降级为 G4 不实施，改以 G1（wshub 清理）为首个落地步骤。
- **[major]** Phase 整数与既有文档碰撞 → 重命名 G1-G4 + §0.1 明确 ORTHOGONAL 关系。
- **[minor, 已验证]** `sessionH`/`healthH` 非 registration-only（`healthH.dispatcherMetrics` ctor 后写、`sessionH` 在 server_loops.go 运行时用）→ 印证 handler 不同质，进一步支持不做 grouping。
- **[nit]** baseline 现已 drift（47/49 vs 实测 51）→ §1 记录；重基线留待真正改 Server/Hub struct 的步骤顺带做，G1 不碰 struct 故不重基线。
