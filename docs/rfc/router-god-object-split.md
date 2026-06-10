# RFC: session.Router god-object decomposition（合并 #383/#600/#805/#580/#577）

> **状态**: Implemented v2（2026-06-10 订正：P0-P5 已全部落地，P6 实质完成，P7 经核实关闭 won't-do；#383/#600/#805/#580/#577 全部 CLOSED）
> **作者**: naozhi team (cron-cr)
> **创建**: 2026-06-04
> **修订**: 2026-06-10（v2：按 master 实测把状态从 Draft 升为 Implemented；正文 §0-§8 保留为当时的设计路线图，"本轮不落地代码"等措辞是写作时点的承诺，已被后续 phase PR 兑现）
> **范围**: 合并五个 Router 拆分锚点为单一拆分路线图；~~本轮纯设计不落地代码~~（设计已评审通过并分 phase 落地，见 §0.1）
> **关联 issue**: #383, #600, #805, #580, #577
> **关联代码**: internal/session/router_*.go, managed.go; dispatch/cron/server/upstream consumer.go; internal/discovery, internal/sysession

---

## 0.1 实施状态（2026-06-10 对 master 逐项核实）

§8.1 路线图的执行结果，候选 A（facet sub-struct 分组 + 单 `r.mu` 不变）路线：

| Phase | 状态 | 落地 PR / 证据 |
| :--- | :--- | :--- |
| **P0** check-router-fields lint | ✅ landed | PR #1762（warn 模式）→ #1796（修 32 处注释漂移 + 翻 fail 模式）；工具在 `tools/check-router-fields/` |
| **P1** WorkspaceStore | ✅ landed | PR #1802（`router_workspace.go` `type workspaceStore`，并扩展 lint 支持 sub-struct） |
| **P2** KnownIDsStore | ✅ landed | PR #1837（`store.go` `type knownIDsStore`，字段 `kid`） |
| **P3** BackendStore | ✅ landed | PR #1804（`router_backend.go` `type backendStore`，字段 `bkStore`） |
| **P4** SessionStore | ✅ landed | PR #1841（`store.go` `type sessionStore`，字段 `ss`） |
| **P5** ProcessPool | ✅ landed | PR #1852（`process_pool.go` `type processPool`，字段 `pp`） |
| **P6** ManagedSession facet | ✅ 实质完成 | 文件级拆分由 `managed-session-split.md`（Implemented v2.3）交付；processIface facet 接口 `ProcessSender`/`ProcessEventReader`/`ProcessLifecycle`/`HistoryInjector` 均已在 `managed.go` 定义（processIface embed，附 contract test）。注：ManagedSession **struct 字段**未做 sub-struct 分组——按 managed-session-split.md 非目标处理，无独立 issue 跟踪 |
| **P7** discovery↔sysession catalog | ✅ closed won't-do | §8.4 step 2 的 follow-up 设计（PR #1881，`session-catalog-boundary.md`）经对抗性核实得出"重叠技术上不成立"（discovery 与 sysession 零耦合、双视角实际在 discovery↔Router 且已被三重机制缝合、无统一类型消费者），#577 据此于 2026-06-07 永久关闭，重开判据写在 closure comment |

验收对照（§8.5）：facet 分组 ✅ / 单 `r.mu` 不变 ✅ / consumer 接口签名不变 ✅ / `tools/check-router-fields` fail 模式绿 ✅；#383/#600/#805/#580 关闭 ✅，#577 收窄后关闭（won't-do，非移交）✅。

遗留事项（不阻塞本 RFC 关闭）：

- `router_core.go` 的 facet 字段仍背着长 UNION `// 读写:` 注释，靠 P0 lint
  钉住——这是设计内权衡（候选 A 不动锁拓扑），非烂尾。
- `RenameSession` 手工逐字段克隆 ManagedSession（`router_lifecycle.go`）与
  `spawnSession` 226 行单方法不在本 RFC 范围（facet 拆的是字段分组非方法重组），
  如需跟进应另立 issue。

---

## 0. 本轮交付边界（先读这一段）

> **（v2 注）** 本节及以下各节是 2026-06-04 写作时点的设计文档，"本轮不落地任何生产代码"
> 指当时那一轮 RFC 交付，承诺已兑现——评审通过后各 phase 经独立 PR 落地，结果见 §0.1。
> 以下正文保留原貌作为设计依据与锁序论证的记录。

**本主题本轮不落地任何生产代码。** 这是一份合并五个独立 issue 锚点
（#383/#600/#805/#580/#577）的设计路线图。god-object 拆分会改动
`internal/session/Router`（53 字段 / 100 方法 / 8 文件，由单一 `mu` 协调）
的字段-方法-锁三方耦合，**锁拓扑必须先经评审再动手**——见 §3、§5、§8。

落盘前置条件：本 RFC 的 §3（单 mutex vs per-aggregate 锁）与 §8（分阶段路线
图 + facet 依赖顺序）必须先评审通过；任何 facet 抽取 PR 必须引用本 RFC 的
phase 编号并附 §4 的锁序 re-proof。

---

## 1. Background & problem

### 1.1 god-object 现状（已用 grep/read 核实，2026-06-04）

`type Router struct` 定义在 `internal/session/router_core.go:293`，截至本轮：

- **53 个字段**（`router_core.go:293-579`，`sed -n '293,579p' | grep -cE '^\s+[a-zA-Z]+ +'` = 53）。
- **100 个 `func (r *Router)` 方法**，分布在 8 个文件：

  | 文件 | 方法数 | 职责（粗分） |
  | :--- | ---: | :--- |
  | `router_core.go` | 24 | 构造/索引 helper(indexAdd/Del)/Stats/Version/notify* |
  | `router_lifecycle.go` | 19 | spawn/Reset/Rename/SetWorkspace/GetWorkspace/countActive/evict |
  | `router_cleanup.go` | 13 | Cleanup/Remove/RemoveAsync/saveIfDirty/Shutdown/prune |
  | `router_discovery.go` | 17 | Takeover/Register*/trackSessionID/knownIDs/DiscoveryExcludeIDs |
  | `router_shim.go` | 6 | ReconnectShims/shimManagedKeys/reconnect |
  | `router_backend.go` | 11 | wrapperFor/managerFor/BackendIDs/Get/SetSessionBackend |
  | `router_workspace.go` | 4 | workspaceOverrides 读写 |
  | `router_capacity.go` | 6 | exempt 子配额/容量门 |

- 所有可变字段统一由**单个 `mu sync.RWMutex`** 协调
  （`router_core.go:295`）。每个字段都带人手维护的 `// 读写: <files>`
  lock-domain 注释（`router_core.go:280` 的维护规则；`router_core.go:282`
  的 R245-ARCH-48 已指出这套注释"silently rots"，需要 CI 校验工具——本 RFC
  把它纳入 §8 的工具化前置项）。

- sibling god-struct：`type ManagedSession struct` 在
  `internal/session/managed.go:333`（~60 字段，绝大多数 `atomic.*`，
  per-session `sendMu`/`historyMu` 自带锁域）。`managed.go` 的**文件分拆**
  已由 `docs/rfc/managed-session-split.md`（Implemented v2.3）完成，但
  **facet struct 分拆**（ProcessSender / ProcessEventReader 等，R242-ARCH-4）
  仍未落地——这是 #805 合并进来的子项。

### 1.2 为什么 god-object 是问题

1. **字段-方法-锁三方隐式耦合**：53 字段全部躺在同一把 `mu` 后面，方法是否
   持锁、持读锁还是写锁，只能靠 `// 读写:` 注释和 godoc 锁序声明
   （`router_core.go:268-273`：`s.sendMu -> r.mu`；`historyMu` 独立）传达。
   注释会腐烂（R245-ARCH-48 已自承）。
2. **review blast radius 巨大**：任何动 `sessions` / `sessionsByChat` /
   `keyhashToKey` 三联二级索引（`router_core.go:299/307/320`）或
   `workspaceOverrides` 两键不变量（`router_core.go:380` 的 godoc）的 PR，
   reviewer 必须人脑回放整把锁。
3. **测试矩阵笨重**：30+ `router_*_test.go` 文件（`router_test.go` 单文件
   2690 行）。
4. **跨包 consumer 接口碎片化**：4 个消费包各自手写"subset of Router"接口
   （见 §1.3），没有中央契约。
5. **职责边界模糊**：discovery（外部 CLI 进程发现）与 sysession（内置后台
   线程）对"session catalog"的认知重叠（#577 / R237-ARCH-4）。

### 1.3 四个 consumer 的 narrow 接口现状（已核实）

| 包 | 文件:行 | 接口 | 方法数 |
| :--- | :--- | :--- | ---: |
| dispatch | `internal/dispatch/consumer.go:35` | `SessionRouter` | 8 |
| cron | `internal/cron/scheduler.go:103` | `SessionRouter`（返回 cron-local 类型） | 3 |
| server | `internal/server/consumer.go` | `HubRouter`(=wshub.HubRouter,14) / `ScratchRouter`(3) / `SendRouter`(2) | — |
| upstream | `internal/upstream/consumer.go:62` | `SessionLookup`/`SessionLifecycle`/`SessionMutator` 组合成 `SessionRouter` | 2+5+2 |

中央化的尝试：`internal/session/api/`（`capabilities.go`）曾计划 hoist
Lookup/Lifecycle/Mutator/Router-union 共享 mixin，但 **#1600 已把它们全删**
——一年零消费者，唯一存活的是 `SessionVisitor`（被 sysession AutoTitler 用，
`assert.go` 钉死）。这是 #580 必须正视的前车之鉴（§3 备选 C）。

### 1.4 五个 issue 的合并关系

| issue | 锚点 | 内容 | 与本 RFC 的映射 |
| :--- | :--- | :--- | :--- |
| #383 | ARCH2 | 拆 sessionStore / processPool / shimReconciler aggregates | §8 Phase 2/4/5 |
| #600 | R237-ARCH-9（含 R239 RouterStore/Policy/Lifecycle） | 拆 SessionStore/KnownIDs/WorkspaceOverrides/BackendOverrides/AutoChainResolver | §8 Phase 1/2/3 |
| #805 | R246-ARCH-22（= R243-ARCH-9 + R245-ARCH-28 + R246-ARCH-22） | ManagedSession facet split | §8 Phase 6（与 managed-session-split.md 协调） |
| #580 | R215-ARCH-P2-4 | 4 包 overlapping consumer 接口 → 中央 CoreReader/CoreMutator | §3 备选 C + §7 + §8 Phase 0 |
| #577 | R237-ARCH-4 | discovery vs sysession single session catalog（须先 dedup R234） | §8 Phase 7 + §8.4 dedup 处理 |

---

## 2. Goals & non-goals

### 2.1 Goals

- G1：产出**一份**合并五锚点的拆分路线图，给出 facet 抽取的依赖顺序与"先抽
  哪个"建议，以及每步的锁不变量 re-proof 要求。
- G2：为单 mutex vs per-aggregate 锁拓扑给出明确决策与理由（§3）。
- G3：定义 #580 中央 consumer 契约（CoreReader/CoreMutator）的形态与
  **不重蹈 `internal/session/api` 覆辙**（§3 备选 C）的判据。
- G4：为 #577 给出 dedup-against-R234 的处理建议（§8.4）。
- G5：保证迁移过程中 30+ `router_*_test.go` 与 4 个 consumer 契约测试**每步
  保持绿**（§4），4 个 consumer 接口的方法签名**不破坏**（§7）。

### 2.2 Non-goals

- NG1：**本轮不落地任何生产代码**（§0）。
- NG2：不改 `ManagedSession` 的 per-session 锁模型（`sendMu`/`historyMu`/
  atomic 字段）——只在 Phase 6 做 facet struct 分组，零语义改动，沿用
  managed-session-split.md 的"文件搬迁 + 编译期断言"手法。
- NG3：不引入外部组件（项目红线：所有状态文件化）。
- NG4：不动 `internal/session/api` 已删除的 mixin（#1600 已结案，不复活）。
- NG5：不做 `internal/discovery` 内部的 path/proc/history 三分（R222-ARCH-16，
  独立 ticket）——本 RFC 只处理 discovery↔sysession 的**catalog 边界**。

---

## 3. Alternatives considered

### 3.1 锁拓扑：单 mutex 保留 vs per-aggregate 锁

**候选 A — 保留单一 `r.mu`，仅做"结构性"拆分（推荐）。**
把 53 字段按职责分组到内嵌的 sub-struct（如 `store` / `policy` /
`lifecycleState` / `knownIDsState`），但 sub-struct **不带自己的锁**，仍由
外层 `r.mu` 统一保护。方法 receiver 仍是 `*Router`，访问改成 `r.store.sessions`
等。锁序契约（`sendMu -> r.mu`，`historyMu` 独立）**完全不变**。

- 优点：锁不变量零变动 → §4 的 lock-order re-proof 是"证明没变"而非"证明新
  拓扑正确"，风险最低；与 managed-session-split.md 的"零语义改动文件搬迁"
  手法同源，已被验证可行；30+ 测试基本零改动（除 source-introspecting 测试，
  见 §4.3）。
- 缺点：不解决"单锁热点"理论争议（但实测无热点：Stats/Version/onChange 已
  用 atomic 走 lock-free 读，见 `router_core.go:395/448/511`）。

**候选 B — 每个 aggregate 一把锁（per-aggregate 锁）。**
`sessionStore.mu` / `knownIDsStore.mu` / `workspaceStore.mu` 各自独立。

- 优点：理论并发度高。
- 缺点（致命）：**引入跨锁顺序新不变量**。今天 `spawnSession` 在一次 `r.mu`
  写临界区内同时改 `sessions` + `sessionsByChat` + `keyhashToKey` +
  `sessionIDToKey` + `activeCount` + `storeDirty`（见 §5 的二级索引不变量）。
  拆成多锁后，这些原子复合更新要么需要按固定顺序获取多把锁（新死锁面），要么
  退化成"持 A 锁时读 B 锁保护的数据"的隐式不变量。`onSessionID` 回调已经在
  `sendMu` 持有时拿 `r.mu`（`router_core.go:270`）——再加锁会让锁图从一条线
  变成一张网。**ROI 为负**：并发收益是理论的，死锁/torn-read 风险是真实的。

**决策：选 A。** 单 mutex 保留。理由：（1）二级索引三联（§5）的原子复合更新
要求一把锁覆盖全部相关字段，per-aggregate 锁会把不变量从"编译器+一把锁可证"
退化成"人脑回放多锁顺序"；（2）lock-free 读路径已用 atomic 解决真实热点；
（3）与已成功落地的 managed-session-split 手法同源，可复用其经验教训。

### 3.2 切法：一次性大切 vs 渐进式 facet 抽取

**候选 X — 一次性大切（一个 PR 把 Router 拆成 N 个 aggregate）。**

- 缺点：blast radius = 全部 100 方法 + 30+ 测试 + 4 consumer 契约同时动；
  contract_test.go 的 stale-merge race（upstream/consumer.go:26 已点名此风险）；
  无法二分定位回归。**否决。**

**候选 Y — 渐进式 facet 抽取（每 PR 一个 facet，引用本 RFC phase 编号）（推荐）。**
每个 phase 抽一个 sub-struct，独立 PR，独立 review，独立 §4 锁 re-proof。

- 优点：单 PR 可回滚（§5）；测试每步绿；与 managed-session-split.md 的 5-phase
  落地节奏一致（每 phase `go build`+`go vet`+`go test -race` 全绿）。

**决策：选 Y。** 渐进式，依赖顺序见 §8。

---

## 4. Test strategy

### 4.1 既有测试资产（已核实）

- **30+ `router_*_test.go`**（`internal/session/`），含 `router_test.go`
  (2690 行) + 专项契约测试：`router_cleanup_*_test.go`(6 个) /
  `router_history_*_test.go`(4 个) / `router_shim_inject_contract_test.go` /
  `router_known_ids_cap_test.go` / `router_restore_entry_test.go` 等。
- **4 个 consumer 契约**：`internal/session/contract_test.go` 钉
  `var _ dispatch.SessionRouter = (*session.Router)(nil)` 等；
  `internal/session/api/assert.go` 钉 `SessionVisitor`；
  各 consumer 包内的 fake-injection 测试。

### 4.2 每步迁移的绿条件（候选 A 下）

因为候选 A **不改任何字段语义、不改任何方法签名、不改锁拓扑**，理论上 30+
router 测试 + 4 consumer 契约**全部零改动通过**。每个 phase PR 的 gate：

```
go build ./...
go vet ./...
go test -race ./internal/session/... ./internal/dispatch/... \
        ./internal/cron/... ./internal/server/... ./internal/upstream/... \
        ./internal/sysession/...
```

### 4.3 source-introspecting 测试（必踩坑，已从 managed-split 学到）

managed-session-split.md v2.3 自承：`reattach_contract_test.go` /
`managed_interrupt_idle_race_test.go` 用 `os.ReadFile("managed.go")` /
`parser.ParseFile` 按**文件名**断言符号定义位置，文件搬迁必破。本 RFC 迁移前
必须先 grep `os.ReadFile(` + `parser.ParseFile(` + `ParseDir(` 跨 router 测试，
列出"焊死文件布局"的测试，每 phase 只改其**文件名字面量**（不改断言意图），
并在 PR 描述中显式标注。**长期**：把这类测试改为按 package AST 扫描（不绑文件
名），消除布局脆性。

### 4.4 lock-order re-prove（每 phase 强制）

候选 A 下锁拓扑不变，但每个 phase PR 仍须在描述中给出 re-proof：

1. **静态**：列出本 phase 搬迁的字段，确认其 `// 读写:` 注释指向的所有
   `router_*.go` 仍在同一把 `r.mu` 临界区访问（无字段逃逸到无锁路径）。
2. **动态**：`go test -race` 覆盖该 facet 的所有 mutation 路径（spawn/Reset/
   Remove/Cleanup/Takeover/SetWorkspace）。
3. **不变量断言**：复用/新增针对 §5 三联索引 + 两键不变量的表驱动测试，断言
   facet 抽取后 `sessions`/`sessionsByChat`/`keyhashToKey` 仍同步增删。
4. **工具化前置**（§8 Phase 0）：先落地 `tools/check-router-fields.go`
   （R245-ARCH-48 已规划），解析每字段 `// 读写:` 注释 vs 实际 grep，CI 失败
   于注释漂移——这是后续每 phase 字段搬迁的安全网。

---

## 5. Risk & rollback

### 5.1 点名的不变量

- **二级索引三联同步不变量**：`sessions`（主表，`router_core.go:299`）、
  `sessionsByChat`（chat key → session key 集合，`router_core.go:307`）、
  `keyhashToKey`（`persist.KeyHash` → key，`router_core.go:320`）、外加
  `sessionIDToKey`（`router_core.go:491`）必须在**同一 `r.mu` 写临界区**内
  原子地一起增删。helper `indexAdd`/`indexDel`（`router_core.go` 读写域）是唯
  一入口。**风险**：facet 抽取若把 `sessions` 移入 `store` sub-struct 而把
  `sessionsByChat` 移入另一个，且任一 mutation 路径漏调 helper，会产生
  stale 索引 → ResetChat 漏删 / keyhash resolver 返回错 workspace
  （`keyhashToKey` godoc 已说 resolver 自愈但代价是一次全扫）。
- **workspaceOverrides 两键不变量**（`router_core.go:369-380` godoc）：chat key
  是 3 段（`platform:chatType:chatID`），session key 是 4 段。`ResetChat` 同清
  `sessionsByChat` + `workspaceOverrides`；`SetWorkspace` 只建 override 不建
  session；`Reset(key)`/`evictOldest` **不得**碰 `workspaceOverrides`（它由用户
  意图驱动，非生命周期驱动）。**风险**：把 workspaceOverrides 抽进 lifecycle
  facet 会诱导误改成"生命周期清理时一并清 override"，违反不变量。建议它单独成
  `workspaceStore` facet（#600 已点名）以物理隔离这条边界。
- **锁序不变量**：`s.sendMu -> r.mu`（`router_core.go:270`）；持 `r.mu` 写锁的
  代码**禁止**再拿 `sendMu`；`historyMu` 独立、不与前两者同持。candidate B 会
  破坏此线性序——这是选 A 的核心理由。
- **pendingSpawns / spawningKeys 不变量**：`pendingSpawnSlot`
  （`router_core.go:594`）RAII 保证 panic 不 strand 计数器；`spawningKeys`
  （`router_core.go:414`）的 done-channel 让 GetOrCreate 等待者唤醒、且让
  ReconnectShims 不把 in-flight spawn 误判为 orphan shim。抽 processPool/
  shimReconciler facet（#383）时必须整组搬，不可拆散。
- **knownIDs FIFO + sorted-cache 不变量**：`knownIDsOrder`(FIFO 驱逐)、
  `knownIDsSortedCache`/`knownIDsSortedGen`(throttled save 的排序缓存)、
  `knownIDsGen`(失效信号) 是一组（`router_core.go:459-485`）。抽 KnownIDs
  facet（#600）必须整组搬，否则 gen 失效逻辑断裂。

### 5.2 Rollback

渐进式（§3 候选 Y）天然支持单 PR 回滚：每 phase 是独立 commit/PR，`git revert`
即可。**教训（managed-split v2.3）**：`git checkout <单文件>` 会把已 cut 的
源文件恢复成 HEAD 全量、与已抽出文件重复——回退须整 PR revert，**勿用单文件
checkout**。

---

## 6. Observability

- 复用现有指标：`naozhi_spawn_panic_recovered_total`
  （`router_core.go:668`，panicSafeSpawn）、`activeCount`/`storeGen`/
  `wsOverridesGen` 等 atomic 计数器。facet 抽取**不新增**运行时指标（纯结构
  重排，无新行为）。
- CI 可观测：§4.4 的 `tools/check-router-fields.go` 把"字段-锁注释漂移"从隐
  患变成 CI 失败信号。
- review 可观测：每 phase PR 标题携带本 RFC phase 编号（如 `[router-split P2]`）
  + §4.4 的锁 re-proof checklist，使拆分进度在 PR 列表可追踪。

N/A 部分：无新增日志/trace——本轮纯结构重排，运行时行为不变。

---

## 7. Compatibility & migration

### 7.1 方法签名冻结（硬约束）

候选 A 下所有 `func (r *Router)` 的**签名、receiver、可见性保持不变**。这是
4 个 consumer 接口不破裂的充要条件：

- `dispatch.SessionRouter`(8)、`cron.SessionRouter`(3)、
  `server.HubRouter`(14)/`ScratchRouter`(3)/`SendRouter`(2)、
  `upstream.{SessionLookup,SessionLifecycle,SessionMutator}`(2/5/2)、
  `api.SessionVisitor`(1) 全部靠 Go 结构化 typing 隐式满足。
- `internal/session/contract_test.go` + `internal/session/api/assert.go` 的
  编译期 `var _ X = (*session.Router)(nil)` 钉死签名——任一 phase 改了被这些
  接口覆盖的方法签名，**build 即红**。
- 内部 helper（`indexAdd`/`indexDel`/`acquirePendingSpawnSlotLocked` 等）可改
  receiver（如改成 `(s *store)`）——它们不在任何 consumer 接口里。

### 7.2 #580 中央契约的迁移姿态

`upstream/consumer.go:17-48` 早已规划 `internal/session/api/` 集中化，但
`api/capabilities.go` 记录 **#1600 已证伪**该计划（Lookup/Lifecycle/Mutator/
union 一年零消费者被删）。因此 #580 的 "CoreReader/CoreMutator" **不应**复活
那套 union mixin。建议姿态见 §8 Phase 0。

---

## 8. Rollout plan

### 8.0 总原则

**本轮不落地代码**（§0）。下列 phase 是评审通过后的执行路线图，每 phase 独立
PR，引用本 RFC 编号，附 §4.4 re-proof。依赖顺序由"低风险 + 解锁后续"决定。

### 8.1 phase 路线图（建议依赖顺序）

| Phase | 内容 | issue | 前置 | 风险 |
| :--- | :--- | :--- | :--- | :--- |
| **P0** | 落地 `tools/check-router-fields.go`（字段-锁注释 CI 校验，R245-ARCH-48）+ 把 source-introspecting 测试改为 AST 扫描（§4.3） | 全部前置 | 无 | 低（纯工具/测试） |
| **P1** | 抽 **WorkspaceStore** facet（workspaceOverrides + 两键不变量物理隔离） | #600 | P0 | 低（4 方法，已在独立文件 router_workspace.go） |
| **P2** | 抽 **KnownIDsStore** facet（knownIDs + Order + SortedCache + Gen 整组） | #600 | P0 | 中（gen 失效逻辑） |
| **P3** | 抽 **BackendStore / PolicyState** facet（backendOverrides + wrappers + 默认 model/args） | #600/#383 | P0 | 低（router_backend.go 已聚拢） |
| **P4** | 抽 **SessionStore** facet（sessions + 三联二级索引 + activeCount + storeDirty/Gen），helper indexAdd/Del 收 receiver | #383/#600 | P1,P2 | **高**（§5 核心不变量） |
| **P5** | 抽 **ProcessPool / ShimReconciler** facet（pendingSpawns/spawningKeys/shimStuckOnReset/removeWg 整组） | #383 | P4 | 高（spawn 并发不变量） |
| **P6** | **ManagedSession facet struct split**（ProcessSender / ProcessEventReader 分组，零语义） | #805 | 独立于 P1-P5 | 中（与 managed-session-split.md 协调） |
| **P7** | **discovery ↔ sysession catalog 边界**（见 §8.4） | #577 | 独立 | 中（须先 dedup） |

建议**最先抽 P1（WorkspaceStore）**：方法已在独立文件、字段少、两键不变量是
最适合用物理隔离消除误改的边界，作为整套手法的"试金石 phase"。P4（SessionStore）
是最高风险的核心，放到 P1/P2 立好规范之后。

### 8.2 与 consumer-interfaces.md 的协调（#580）

- consumer-interfaces.md（Implemented v3）的决策是"每 consumer 各自单接口、
  不拆 sub-interface"，且 `internal/session/api` 的 union mixin 已被 #1600 删。
- 本 RFC 对 #580 的建议：**不重建 union**。若评审仍要中央 CoreReader/CoreMutator，
  唯一可接受形态是"**有真实消费者再加**"——即某个 consumer 主动 embed 时才在
  `api/` 增 capability（沿 `SessionVisitor` 的成功范式：一个 mixin 一个活
  消费者 + 一个 `assert.go` 钉），**禁止**预先 hoist 无消费者的接口。本 phase
  归入 **P0 的判据文档**，不单独排 phase。

### 8.3 与 managed-session-split.md 的协调（#805 / P6）

- managed-session-split.md（Implemented v2.3）已完成 managed.go 的**文件**分拆
  (6 文件)，并明确**解锁** ProcessSender/ProcessEventReader 的 facet struct 分拆
  (R242-ARCH-4)。P6 直接续这条线：沿用其"文件搬迁 + 编译期断言 + 修
  source-introspecting 测试文件名"手法，零语义改动；P6 与 P1-P5 互不依赖，可
  并行排期。

### 8.4 #577 的 dedup-against-R234 处理建议

**核实结论**：R237-ARCH-4（#577，`docs/review/batch3-C-r236-240-raw.md:257`）
自称"项目 brief 与 R234-ARCH-3 已点出"，但 **R234-ARCH-3 实际已被 discard**
（`batch3-D-r225-235-raw.md:47`：dup of #720[exempt pool] + #432[ManagedSession
state machine]），且 R234-ARCH-3 的原始内容是"cron/sysession 各自 RouterView +
DTO 散在两包"——**那部分已由 #1600 解决**（`api/` 只留 `SessionVisitor`，cron/
sysession 各自 narrow 接口）。

因此 #577 与 R234-ARCH-3 **只是表述上交叉，技术内核已分家**：

- R234-ARCH-3 的 "RouterView/DTO 统一" 部分 → **已 closed**（#1600），#577 不再
  重复。
- #577 真正剩余的、未被任何 issue 覆盖的部分 = **discovery 与 sysession 对
  "session catalog" 的职责重叠**：`internal/discovery` 产出
  `DiscoveredSession`（外部 CLI 进程，`scanner.go:291`），`internal/sysession`
  消费 router（`SystemSessionRouter`/`SessionVisitor`）——两者对"哪些 session
  存在"各有视角。R237-ARCH-4 的建议是：**discovery 只产出 DiscoveredSession
  （外部进程发现）；sysession 只消费 router + catalog；引入单一 catalog 抽象
  消除重叠**。

**dedup 处理建议**：
1. 在 #577 上加 comment：明确"与 R234-ARCH-3 的 RouterView/DTO 部分已由 #1600
   关闭，本 issue 收窄到 discovery↔sysession catalog 边界"，把 R234-ARCH-3
   标为 superseded-by #1600（DTO 部分）+ #577（catalog 部分）。
2. P7 的设计**单独立 follow-up RFC**（catalog 抽象是 breaking、影响 RFC 接口，
   R237-ARCH-4 已标 "Breaking: 是"），不与 P1-P6 的纯结构重排混在一个 PR。
   本 RFC 只负责把 #577 收窄、dedup、定边界，不展开 catalog 详细设计。

**closure 状态（2026-06-05）**：上述 step 1 的 dedup-closure comment 已发布到 #577
（`gh issue comment 577`），#577 已据此收窄——R234-ARCH-3 的 RouterView/DTO 部分确认
由 #1600 关闭（`internal/session/api/assert.go` 仅留 `SessionVisitor` 编译期断言），
#577 现仅覆盖 discovery↔sysession 的 session-catalog 职责边界，标记
`needs-design`、blocked-on step 2 的 follow-up RFC（尚未立项）。补核：`internal/discovery`
与 `internal/sysession` 当前**无互相 import**，故 P7 是 net-new breaking 抽象（与 §1.4
"god-object 拆分会改动…接口"、§8.1 P7"中（须先 dedup）"一致），非安全文件搬迁——本轮
不落任何 .go 代码，仅完成 dedup/收窄/定边界。

### 8.5 验收

- 全部 P0-P7 落地后：`Router` 字段按 facet sub-struct 分组、单 `r.mu` 不变、
  4 consumer 接口签名不变、`tools/check-router-fields.go` 绿、30+ 测试 + 4
  契约绿；#383/#600/#805/#580 关闭，#577 收窄并移交 follow-up RFC。

---

## 附录 A — 核实命令（2026-06-04）

```
grep -n 'type Router struct' internal/session/router_core.go      # :293
sed -n '293,579p' internal/session/router_core.go | grep -cE '^\s+[a-zA-Z]+ +'  # 53 字段
grep -rhc '^func (r \*Router)' internal/session/router_*.go | paste -sd+ | bc   # 100 方法
grep -n 'type ManagedSession struct' internal/session/managed.go  # :333
grep -n 'type SessionRouter interface' internal/dispatch/consumer.go internal/cron/scheduler.go
grep -n 'SessionLookup\|SessionLifecycle\|SessionMutator' internal/upstream/consumer.go
```
