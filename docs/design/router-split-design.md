# session.Router 拆分设计

> **状态**: 设计稿 v4 (2026-05-18)，已应用三轮 architect + go-reviewer 评审反馈。**定稿，可施工**。
>
> **动机**: `internal/session/router.go` 4279 行 / 71 个 Router 方法 / 25 个字段，是当前并行开发最大的物理瓶颈：feat 分支并行修改它必然在 merge 时冲突（git status 当下就有 `UU`）。三个月被改了 11 次。
>
> **目标**: 把 Router 切成 6 个职责单一的"切片文件"，让多个 agent 能在不同切片上并行工作而不撞同一文件；公开 API 完全保持不变，外部 59 个调用点零改动。

---

## 现状快照

### router.go 结构（按行号区段）

| 区段 | 行号 | 内容 | 大小 |
|---|---|---|---|
| 验证 / 错误 | 44–166 | 常量、`validateModel`、`validateBackend`、`isExemptKey` | ~120 |
| Router struct | 242–385 | 25 个字段 | ~145 |
| backend 选择 | 391–650 | `panicSafeSpawn*`、`spawnerFunc`、`wrapperFor`、`managerFor`、`BackendIDs`、`BackendWrapper`、`indexAdd/Del`、`computeBackendIDs` | ~260 |
| Config + 构造 | 658–1130 | `RouterConfig`、`NewRouter` | ~470 |
| history wiring | 1131–1177 | `attachHistorySource` | ~50 |
| shim reconcile | 1178–1656 | `shimManagedKeys`、`shimManagers`、`ReconnectShims*`、`reconnectShims`、`classifyShimState`、`shutdownShimViaReconnect` | ~480 |
| 回调注册 | 1657–1733 | `SetOnChange`、`notifyChange`、`SetOnKeyRetired`、`notifyKeyRetired`、`NotifyIdle` | ~80 |
| chat workspace + backend override | 1735–1804 | `SetWorkspace`、`GetWorkspace`、`SetSessionBackend`、`GetSessionBackend` | ~70 |
| **lifecycle 主流程** | 1805–2961 | `ResetChat`、`GetOrCreate`、`spawnSession`、`installFreshSessionLocked`、`installPersistSink`、`countActive`、`countExempt`、`evictOldest`、`unregisterSessionLocked`、`resetLocked`、`Reset*`、`finishResetUnlocked`、`ResetAndRecreate`、`RenameSession` | **~1156** |
| Remove + cleanup | 2959–3296 | `Remove`、`dropEventLogForKey`、`clearAttachmentTrackerRefs`、`Cleanup`、`shouldPrune` | ~340 |
| 后台 loops | 3297–3503 | `StartCleanupLoop`、`saveIfDirty`、`StartShimReconcileLoop` | ~200 |
| Shutdown | 3504–3675 | `Shutdown`、`shutdown` | ~170 |
| 只读访问器 | 3676–3845 | `DefaultWorkspace`、`Version`、`BumpVersion`、`MaxProcs`、`CLIPath`、`Stats`、`HealthCheck`、`ListSessions`、`GetSession` | ~170 |
| label / interrupt | 3847–3985 | `SetUserLabel`、`InterruptSession`、`InterruptSessionSafe`、`InterruptSessionViaControl` | ~140 |
| discovery 协作 | 3986–4214 | `DiscoveryExcludeIDs`、`trackSessionID`、`RegisterForResume`、`RegisterCronStub*`、`ManagedExcludeSets` | ~230 |
| Takeover | 4216–end | `Takeover` | ~60 |

### 外部依赖面（不能动的契约）

- **27 个公开方法**（grep `func (r *Router) [A-Z]`）
- **59 个 Go 文件 import session 包**
- 主要消费者：`internal/server/`、`internal/dispatch/`、`internal/cron/`、`internal/upstream/`、`cmd/naozhi/`

### 核心问题

1. **router.go 是 god file**：lifecycle / shim / callback / cleanup / shutdown / discovery 全挤一起。
2. **`mu` 是事实上的全局锁**：25 个字段共用一把读写锁；拆分必须保留这把锁的语义。
3. **方法相互调用形成密网**：`GetOrCreate` → `resolveSpawnParamsLocked` → `evictOldest` → `unregisterSessionLocked` → `resetLocked` 全在 lifecycle；shim reconcile 走另一条线但也走 lifecycle 入口（`reconnectShims` → `installFreshSessionLocked`）。

---

## 设计原则

- **零行为变更**：不改逻辑、不改并发模型、不改公开 API。任何"顺便清理"另起 PR。
- **不拆包，只拆文件**：保留 `package session`。Go 同包跨文件访问零成本，保持当前 `r.<field>` 不变。
- **按"修改原因"分组**（SRP 实际定义）：每个新文件对应一组在同一种业务诱因下一起改动的方法。
- **测试不动**：`router_test.go` / `routing_test.go` 等不重命名、不切分。
- **可增量推进**：单 PR / 多 commit；4 小时连续窗口推完（详见 §冻结策略）。

### 不做什么（Non-goals）

- ❌ 不引入接口拆分 Router
- ❌ 不拆分 `mu`
- ❌ 不动 `managed.go`（1377 行）
- ❌ 不动 `routing.go` / `scratch.go` / `store.go`
- ❌ 不动 `Router` struct 字段定义；字段全部留在 router_core.go

---

## 切分方案

### 文件映射（最终态）

| 新文件 | 来源 | 行数估计 | 主要内容 |
|---|---|---|---|
| `router_core.go` | 大部分 core 区段 | ~970 | Router/Config struct、`NewRouter`、`spawnerFunc`、`panicSafeSpawn*`、回调注册、只读访问器、`indexAdd/Del`、`isExemptKey`、`chatKeyFor`、`ChatKey`、blank import |
| `router_backend.go` | backend 区段 + 验证器 | ~270 | `wrapperFor`、`managerFor`、`BackendIDs`、`DefaultBackend`、`BackendWrapper`、`computeBackendIDs`、`SetSessionBackend`、`GetSessionBackend`、`CLIName`、`CLIVersion`、`CLIPath`、validators |
| `router_lifecycle.go` | lifecycle 主流程 | ~1280 | `attachHistorySource`、workspace overrides、`AgentOpts`、`SessionStatus`、`spawnParams`、所有 spawn / Reset / Rename 路径；**Phase 1 内部再切 1a/1b**（见 §实施步骤）|
| `router_shim.go` | shim 区段 + reconcile loop | ~570 | shim reconcile 全套、`shimState`、`StartShimReconcileLoop` |
| `router_cleanup.go` | cleanup + cleanup loop + Shutdown | ~660 | `Remove`、`Cleanup`、`shouldPrune`、`StartCleanupLoop`、`saveIfDirty`、`Shutdown` |
| `router_discovery.go` | discovery + label + interrupt + Takeover | ~430 | `SetUserLabel`、`InterruptSession*`、`DiscoveryExcludeIDs`、`trackSessionID`、`RegisterForResume`、`RegisterCronStub*`、`ManagedExcludeSets`、`Takeover` |

合计约 **4180 行**（含每文件独立 import block + 文件头注释）。

### 顶层符号归属表（覆盖所有 type / const / var）

> 评审反馈：避免 phase 间漂移，**每一个顶层符号都必须有归属**。下表覆盖 `grep -nE '^(type |const |var )' internal/session/router.go` 全部输出。

#### types（9 个）

| 类型 | 定义行 | 归属 | 备注 |
|---|---|---|---|
| `Router` | 242 | `router_core.go` | 字段定义唯一来源 |
| `RouterConfig` | 658 | `router_core.go` | 与 NewRouter 同归 |
| `spawnerFunc` | 391 | `router_core.go` | `panicSafeSpawn*` 通用 helper |
| `onChangeHolder` | 1657 | `router_core.go` | atomic.Pointer 包装 |
| `onKeyRetiredHolder` | 1679 | `router_core.go` | atomic.Pointer 包装 |
| `AgentOpts` | 1897 | `router_lifecycle.go` | 主消费者是 spawn / GetOrCreate |
| `SessionStatus` | 1906 | `router_lifecycle.go` | 跟 GetOrCreate 同归 |
| `spawnParams` | 2013 | `router_lifecycle.go` | spawn 内部辅助 |
| `shimState` | 1238 | `router_shim.go` | shim 文件私有 |

#### const（4 个 const block + 6 个独立 const）

| 符号 | 行号 | 归属 | 理由 |
|---|---|---|---|
| `maxModelBytes` | 44 | `router_backend.go` | 仅 validateModel 用 |
| `maxBackendBytes` | 80 | `router_backend.go` | 仅 validateBackend 用 |
| `ShutdownTimeout` | 109 | `router_core.go` | 公开 API，core 是基础设施 |
| `DefaultMaxProcs` | 172 | `router_core.go` | 被 NewRouter 用 |
| `DefaultTTL` | 175 | `router_core.go` | 同上 |
| `DefaultPruneTTL` | 179 | `router_core.go` | 同上 |
| `maxExemptSessions` | 194 | `router_lifecycle.go` | 仅 GetOrCreate 路径用 |
| `historyLoadConcurrency` | 198 | `router_core.go` | NewRouter 启动时用 |
| `ProjectScanInterval` | 202 | `router_core.go` | 公开常量供 server 包用 |
| `shimReconnectTimeout` | 208 | `router_shim.go` | shim reconcile 路径 |
| `shimReconnectGraceDelay` | 221 | `router_shim.go` | 同上 |
| `knownIDsSaveInterval` | 226 | `router_cleanup.go` | Cleanup 与 saveIfDirty 共享 |
| `sessionSaveInterval` | 232 | `router_cleanup.go` | StartCleanupLoop 用 |
| `shimState` 枚举值（5 个） | 1240–1246 | `router_shim.go` | 跟 `shimState` 同归 |
| `maxWorkspaceOverrides` | 1735 | `router_lifecycle.go` | 仅 SetWorkspace 用 |
| `maxBackendOverrides` | 1773 | `router_backend.go` | 仅 SetSessionBackend 用 |
| `SessionStatus` 枚举值（3 个） | 1908–1912 | `router_lifecycle.go` | 跟 `SessionStatus` 同归 |
| `maxKnownIDs` | 4006 | `router_discovery.go` | 仅 trackSessionID 用 |

#### var（8 个）

| 符号 | 行号 | 归属 | 理由 |
|---|---|---|---|
| `modelRe` | 53 | `router_backend.go` | validators 同归 |
| `ErrInvalidModel` | 72 | `router_backend.go` | 同上 |
| `backendRe` | 78 | `router_backend.go` | 同上 |
| `ErrInvalidBackend` | 83 | `router_backend.go` | 同上 |
| `ErrMaxProcs` | 112 | `router_core.go` | 公开错误，被 GetOrCreate 返回但定义归 core |
| `ErrMaxExemptSessions` | 119 | `router_core.go` | 同上 |
| `ErrNoCLIWrapper` | 125 | `router_core.go` | 同上 |
| `ErrNoActiveProcess` | 132 | `router_core.go` | 同上 |
| `exemptKeyPrefixes` | 145 | `router_core.go` | `isExemptKey` 用 |
| `listRefsPool` | 3788 | `router_core.go` | `ListSessions` 用 |

#### functions（包级 free function，需显式归属）

| 符号 | 行号 | 归属 | 理由 |
|---|---|---|---|
| `validateModel` | 57 | `router_backend.go` | validators 同归 |
| `validateBackend` | 90 | `router_backend.go` | 同上 |
| `isExemptKey` | 157 | `router_core.go` | 被 NewRouter (core) 和 RegisterForResume (discovery) 跨文件调用，定义放 core |
| `chatKeyFor` | 440 | `router_core.go` | 公用 key 工具 |
| `isENOENTErr` | 455 | `router_core.go` | errno helper |
| `claudeProjectSlug` | 465 | `router_core.go` | 公用路径工具 |
| `resolveResumeID` | 489 | `router_core.go` | 公用 resume 工具 |
| `panicSafeSpawn` | 400 | `router_core.go` | 与 spawnerFunc 同归 |
| `panicSafeSpawnFn` | 412 | `router_core.go` | 同上 |
| `computeBackendIDs` | 1082 | `router_backend.go` | backend 初始化 helper |
| `classifyShimState` | 1256 | `router_shim.go` | shim 私有 |
| `shutdownShimViaReconnect` | 1288 | `router_shim.go` | 同上 |
| `ChatKey` (exported) | 1726 | `router_core.go` | 公开 API |
| `collectPreviousHistory` | 2113 | `router_lifecycle.go` | spawn 内部 helper |
| `stripResumeArgs` | 3687 | `router_lifecycle.go` | spawn 内部 helper |
| `waitSocketGoneForKey` | 2760 | `router_lifecycle.go` | reset 流程 helper |

#### 包级 init / blank import（1 个）

| Import | 归属 | 注 |
|---|---|---|
| `_ "github.com/naozhi/naozhi/internal/history/claudejsonl"` | `router_core.go` | 触发 init() 注册 history factory；**唯一归属，其他文件不重复**（否则 `duplicate import`） |

#### 普通 import 归属（关键决议）

> 多个文件共用同一 import 是 Go 的常态，不冲突。重要的是确保每个新文件的 import 列表与它实际使用的符号匹配，避免 `unused import` 或 `undefined`。

| Import 包 | 在哪些新文件出现 | 备注 |
|---|---|---|
| `cli` | core, backend, lifecycle, shim | 几乎每个文件都用 |
| `shim` | core, shim, lifecycle, cleanup | shim manager 字段在 core，但 shim/lifecycle 实际调用 |
| `discovery` | core, discovery | discoveryCache 在 core 持有 |
| `merged` / `naozhilog` / `history` | core (NewRouter 启动 persister), lifecycle (attachHistorySource) | 两文件都需要 import |
| `eventlog/persist` | core (字段类型), lifecycle, cleanup | Persister 字段在 core，spawn / Cleanup 都用 |
| `metrics` | lifecycle, shim, cleanup | counter 注入分散 |
| `osutil` | core, cleanup | atomic file write 共享 |

**goimports 的角色（区分 phase）**：

| 阶段 | goimports 能做 | 不能做 |
|---|---|---|
| Phase 0 | 一次性清理整包 import 顺序、stdlib 分组 | — |
| Phase 1-5 创建新文件时 | **新文件 import block 必须人工初建**（goimports 不会自动加项目内/第三方包，只能补 stdlib） | 自动补 stdlib 之外的 import |
| Phase 1-5 修改 router.go 后 | 自动删除变成 unused 的 import | — |

**实操流程**：在新文件落代码后 `go build ./internal/session/` 报 undefined 时人工加 import → `goimports -w 新文件` 排顺序 → router.go 端 `goimports -w` 删 unused。**别把 goimports 当银弹**。

---

## `*Locked` 契约的跨文件审查补救

拆完后 `reconnectShims`（shim 文件）调用 `installFreshSessionLocked`（lifecycle 文件）—— 跨文件调用 `*Locked` 方法的可读性比同文件差。

**Phase 0 必做**：所有 `*Locked` 方法 godoc 行上方加：

```go
// LOCK: caller must hold r.mu for writing.
func (r *Router) installFreshSessionLocked(...) { ... }
```

清单：
- `resolveSpawnParamsLocked`
- `installFreshSessionLocked`
- `unregisterSessionLocked`
- `resetLocked`
- `reconcileSessionActiveByBackendLocked`

## Router struct 字段注释（Phase 0 必做）

每个字段加一行 `// 读写: <文件1>, <文件2>` 注释。新人改 lifecycle 时能立即看到 `spawningKeys` 也被 shim 文件读，避免在不知情下破坏并发不变量。

**长效保护**：`router_core.go` 文件头加规约：

```go
// Maintenance rule: any new field added to Router must carry a
// `// 读写: <files>` comment listing which router_*.go files
// access it. Reviewers MUST block PRs that omit this annotation.
```

---

## git blame 真相（重写）

> 评审反馈：v2 在不同章节互相矛盾。这里钉死真相。

### 可保血缘的文件（仅 1 个）

| 文件 | 操作 | blame |
|---|---|---|
| `router_core.go` | Phase 6 整体 `git mv router.go router_core.go` | ✅ 完整。git 默认 -M50% 阈值能识别（100% 相同内容） |

### 不可保血缘的文件（5 个）

| 文件 | 原因 |
|---|---|
| `router_lifecycle.go` | 1280/4279 ≈ 30% 相似度，低于默认 -M50% |
| `router_backend.go` | 270/4279 ≈ 6% |
| `router_shim.go` | 570/4279 ≈ 13% |
| `router_cleanup.go` | 660/4279 ≈ 15% |
| `router_discovery.go` | 430/4279 ≈ 10% |

**任何"双 commit"也救不回来**——git rename detection 是按文件对粒度计算的，剪一段 1280 行到新文件 ≠ rename。

### 补救（认知层而非技术层）

每个非 core 新文件头加：

```go
// Package session router functionality.
//
// Extracted from router.go on 2026-05-18 as part of the router-split
// refactor. For history prior to <SHA-of-extraction-commit>, see:
//   git log --follow internal/session/router.go
//   git log <SHA>..HEAD -- internal/session/router_lifecycle.go
```

未来 reviewer 看新文件时，注释会指引去 router.go 查老历史。

### blame 断裂的成本评估（为什么仍然值得做）

> 评审反馈：blame 断 5 个文件这件事在工程上是可感知的损失，必须证明"仍然值得"。下面是基于实际数据的论证。

**实测数据（截至 2026-05-18）**：

| 指标 | 数值 | 说明 |
|---|---|---|
| router.go 过去 12 个月 commit 数 | 14 次 | `git log --since="12 months ago" --oneline -- router.go` |
| 其中 hotfix/incident 标识 commit | ~6-8 次 | grep `fix\|hotfix\|p0\|p1\|incident` |
| commit message 含"blame/archeolog/追溯/归因" | **0 次** | 实际查 blame 调古的事件未在文档中显式留痕 |
| 当前并行开发被 router.go 阻塞的频率 | 高 | git status 当下就有 `UU`，三个月 11 次 churn |

**结论**：
- router.go 是**变更高频**而非**调古高频**文件。它是一个活跃的工程主战场，不是历史档案。
- 真正在用 `git blame` 调古的场景集中在**罕见的设计回溯**（"为什么这把锁这样写"）和**不可复现 incident 归因**（"这行 panic 是哪次合的"），过去 12 个月没有显式记录。
- **代价**：5 个新文件失去"3 年历史一键追溯"能力。
- **收益**：把并行上限从 1 提到 5+，每月减少 ~3-5 次 router.go 的 merge 冲突，每次冲突 30-90 分钟。粗算每月省 2-7 小时工程时间。

**何时这个权衡会反转**：
- 如果未来某季度 router.go 上有 ≥3 次 incident 调古需求，重新评估
- 如果团队规模翻倍（>10 人共享 session 包），blame 价值会上升

**对调古的兜底**：
1. 新文件头注释（见上节）指向 `git log --follow router.go`，老历史 1 行命令仍可查
2. 重大 bug fix commit message 强制格式化引用相关 commit SHA（[feedback_dev_workflow_skill] 已规约）
3. `docs/TODO.md` 的"已修复条目"清单是事实上的归因日志

---

## Commit 策略（重写）

> 评审反馈：v2 的"双 commit 保 blame"是错觉。**单 commit 一次到位**才是 Go 工程实操。双 commit 仅作 review 友好可选，不承担 blame 保留职责。

### 每个 phase 一个 commit（默认）

```
commit: "refactor(session): split <area> from router.go (phase N)"

操作：
  1. 在新文件创建 import block + 文件头注释 + 待搬代码段
  2. 从 router.go 删除对应代码段
  3. router.go 的 import 由 goimports 自动清理（unused 自动删）
  4. 提交：build / test / vet / fmt 全绿

期望效果：
  - go build ./... 通过
  - go test -race -count=1 ./... 全绿
  - 单 commit 自洽
  - blame 不指望 git rename 帮忙；靠新文件头注释 + commit SHA 引用人工补救
```

### 「拆分主体 + polish」双 commit（按行数分档钉死）

| phase 改动行数 | commit 策略 |
|---|---|
| **> 500 行** | **强烈推荐双 commit**——主体迁移和文件头注释/gofmt 分开，让 reviewer 能先看"无逻辑变化"的 mechanical diff、再看修饰 |
| **300–500 行** | 双 commit 可选，看作者偏好 |
| **< 300 行** | **禁止双 commit**——避免 commit 噪音，单 commit 一次到位 |

按本设计文档：Phase 1 (lifecycle ~1280) **必须双 commit**；Phase 4 (cleanup ~660) 推荐双 commit；Phase 2/3/5 (270/570/430) 单 commit。

双 commit 模板：

```
commit a: "refactor(session): extract <area> to router_<area>.go (mechanical)"
  - 内容：纯结构化迁移（行段移动 + 必要的 import 调整以让 build 通过）
  - 必须 build + test 全绿

commit b: "refactor(session): polish <area> file (gofmt + headers)"
  - 内容：文件头注释 + gofmt + 注释格式统一
  - 不动逻辑
```

> **注意**：两 commit 都不是 git mv，blame 都不会自动 follow。这种"双 commit"纯粹为了让 review 时能分别看「迁移」和「修饰」两个维度——**与 blame 保留无关**。

---

## 实施步骤

### Phase 0: 准备（独立合入，不阻塞主拆分）

1. `Router struct` 25 个字段加 `// 读写: <files>` 注释
2. 5 个 `*Locked` 方法加 `// LOCK: caller must hold r.mu for writing.`
3. `router.go` 文件头加"新增字段必填注释"维护规约（Phase 6 后随 router_core.go 文件头一起继承）
4. 跑 baseline：
   - `go test -race -count=1 ./...`
   - `go test -race -count=1 ./internal/session/...` 记录 pass 数与耗时
   - **inline baseline**：`go build -gcflags='-m=2' ./internal/session/ 2>&1 | grep -c 'inlining'`，结果写进 commit message
5. **此 commit 单独可合并**，与拆分主 PR 解耦。即使后续主拆分搁置，注释和 baseline 本身有价值。

### Phase 1: 抽 `router_lifecycle.go`（一刀切，~1280 行）

> v3 曾把这刀切成 1a/1b，第三轮评审认为"types + workspace + history wiring"是凑出来的烟雾测试 phase，业务上不自洽。**v4 决议：一刀切**。lifecycle 是一个完整业务单元，强切两半反而让两半都不自洽。失去的兜底用 Phase 0 注释 + race ×2 + 手工冒烟 + Phase 2 backend 作为后继的"机制验证"补回。

**搬动**（落地前必须 `grep -nE '^func' router.go` 与本清单对账）：

- types：`AgentOpts`、`SessionStatus`、`spawnParams`
- const：`maxExemptSessions`、`maxWorkspaceOverrides`、`SessionStatus` 枚举值
- 包级 free function：`collectPreviousHistory` (line 2113)、`waitSocketGoneForKey` (line 2760)、`stripResumeArgs` (line 3687)
- methods：`attachHistorySource`、`SetWorkspace`、`GetWorkspace`、`ResetChat`、`GetOrCreate`、`resolveSpawnParamsLocked`、`spawnSession`、`installFreshSessionLocked`、`installPersistSink`、`countActive`、`reconcileSessionActiveByBackendLocked`、`countExempt`、`evictOldest`、`unregisterSessionLocked`、`resetLocked`、`Reset`、`ResetAndDiscardOverride`、`finishResetUnlocked`、`ResetAndRecreate`、`RenameSession`

> **commit 策略**：本 phase 改动 ~1280 行 > 500 行阈值（见 §commit 策略），**强烈推荐双 commit**——commit a 纯机械迁移、commit b 文件头注释 + gofmt polish。

**验证**：
- `go build ./...`
- `go test -race -count=2 ./internal/session/...`
- `go test -race -count=1 ./...`（全仓库回归，因为这是最大刀）
- `go vet ./...`
- `gofmt -l internal/session/` 输出空

### Phase 2: 抽 `router_backend.go`

**搬动**：
- types/const/var：`maxModelBytes`、`maxBackendBytes`、`maxBackendOverrides`、`modelRe`、`backendRe`、`ErrInvalidModel`、`ErrInvalidBackend`
- functions：`validateModel`、`validateBackend`
- methods：`wrapperFor`、`managerFor`、`BackendIDs`、`DefaultBackend`、`BackendWrapper`、`computeBackendIDs`、`SetSessionBackend`、`GetSessionBackend`、`CLIName`、`CLIVersion`、`CLIPath`

### Phase 3: 抽 `router_shim.go`

**搬动**：
- types/const：`shimState` 类型 + 枚举、`shimReconnectTimeout`、`shimReconnectGraceDelay`
- functions：`classifyShimState`、`shutdownShimViaReconnect`
- methods：`shimManagedKeys`、`shimManagers`、`ReconnectShims`、`ReconnectShimsCtx`、`reconnectShims`、`StartShimReconcileLoop`

注意：`reconnectShims` 调用 `installFreshSessionLocked`（lifecycle 文件）—— 同包跨文件调用零成本，由 Phase 0 加的 `// LOCK:` 注释保审查可读性。

### Phase 4: 抽 `router_cleanup.go`

**搬动**：
- const：`knownIDsSaveInterval`、`sessionSaveInterval`
- methods：`Remove`、`dropEventLogForKey`、`clearAttachmentTrackerRefs`、`Cleanup`、`shouldPrune`、`StartCleanupLoop`、`saveIfDirty`、`Shutdown`、`shutdown`

### Phase 5: 抽 `router_discovery.go`

**搬动**：
- const：`maxKnownIDs`
- methods：`SetUserLabel`、`InterruptSession`、`InterruptSessionSafe`、`InterruptSessionViaControl`、`DiscoveryExcludeIDs`、`trackSessionID`、`RegisterForResume`、`RegisterCronStub`、`RegisterCronStubWithChain`、`ManagedExcludeSets`、`Takeover`

### Phase 6: `git mv router.go router_core.go`

到此 router.go 应当只剩 §文件映射表中 `router_core.go` 列出的内容。`git mv` 操作让这一步是真 rename，blame 完整保留。

### Phase 7: 验证（全套金标）

- `go build ./...`
- `go test -race -count=2 ./...`
- `go vet ./...`
- `gofmt -l internal/session/` 输出空
- `go build -gcflags='-m=2' ./internal/session/ 2>&1 | grep -c 'inlining'` 与 Phase 0 baseline 对比，差异不超过 ±5%
- 手工 dashboard 冒烟：启动 + 创建 session + 拉历史 + interrupt + 关闭

---

## 锁与并发

- 所有现有 `r.mu.Lock()` / `RLock()` 位置原样保留
- `r.mu`、`r.shutdownCond`、原子字段都留在 `router_core.go`
- `*Locked` 后缀契约不变，由 Phase 0 注释显式化
- `historyCtx` / `historyCancel` / `historyWg` 留 core，由 cleanup 文件中的 `Shutdown` 消费

**风险点**：lifecycle 与 cleanup 都触碰 `r.mu` 下的多个字段。拆分前后 `r.mu` 的实际持有时长**必须等价**——验证手段：race detector 跑两遍 + 现有的 contract test（`atomic_pointer_contract_test.go`、`shutdown_order_contract_test.go`、`reattach_contract_test.go`）。

---

## 测试策略

### 不变量验证

- `internal/session/` 现有测试**一行不改**。同包测试不受文件拆分影响。
- 测试套实测耗时：`go test -race -count=1 ./internal/session/...` ≈ 3.7 秒；`-count=2` ≈ 7-8 秒。
- 已知测试调用拆走的内部 helper（`waitSocketGoneForKey`、`stripResumeArgs`）：同包，编译不受影响。

### 增加的测试

无。纯重构按"零新增测试"标准，靠 baseline 测试通过证明等价。

### Phase 间金标

每个 Phase 提交前必须满足：
1. `go build ./...` 通过
2. `go test -race -count=1 ./...` 全绿
3. `gofmt -l internal/session/` 输出空
4. `go vet ./...` 通过
5. 文件行数符合本文件映射表（±10% 容忍）

---

## 冻结策略（统一时长）

> 评审反馈：v2 中"4 小时窗口"和"1-2 天合入"自相矛盾。这里钉死。

- **Phase 0**: 独立 PR，无冻结需求，正常 review + merge
- **Phase 1-7**: 单 PR / 7 commit（或 8 commit 如果 1a/1b 各算一个），在**单一 4 小时连续窗口**内推完
  - 推前在群里通知："今晚 X 点起 4 小时窗口，期间 router.go 不要新提 PR；遇到紧急 fix 喊我"
  - 4 小时内推完合入，期间冲突优先 rebase 到主拆分 PR 而非 master
  - **超时容忍**：若 4 小时没推完，暂停于最近 phase 完成处（router.go 此时已 import-clean、build 通过、可挂在那里若干小时），第二天在新窗口续推

---

## 风险与缓解

| 风险 | 概率 | 缓解 |
|---|---|---|
| import cycle | 极低 | 同包 |
| git blame 部分丢失 | **必然** | 接受 5 个新文件 blame 断裂；core 完整；非 core 文件头注释引导 |
| 单 commit 自洽失败 | 中 | 每 commit 强制 build + test 全绿；失败立刻 git revert |
| 拆分窗口期 master 冲突 | 高 | 4 小时窗口短；通知冻结；冲突 rebase 到主 PR |
| 跨 phase 隐藏行为变化 | 低 | 每 phase 跑 race test 两遍 |
| `*Locked` 跨文件审查难度 | 中 | Phase 0 统一 `// LOCK:` 注释 |
| blank import 重复编译失败 | 低 | 归属表显式钉住，仅 core 持有 |
| inline 阻断性能回归 | 低 | Phase 0 baseline + Phase 7 对比 |
| 新增字段忘加注释 | 低 | router_core.go 文件头维护规约 |
| Phase 1b 单刀过大 | 中 | Phase 1 切成 1a / 1b；1a 先验证机制 |

### 与并行开发的关系

完成后期待的并行能力：

```
✅ 改 backend 选择          → router_backend.go（~270 行）
✅ 改 shim 重连协议         → router_shim.go（~570 行）
✅ 改 cleanup 策略         → router_cleanup.go（~660 行）
✅ 改 discovery / resume    → router_discovery.go（~430 行）
🟡 改 spawn / GetOrCreate   → router_lifecycle.go（~1280 行，仍是热点但已隔离）
```

**仍然不能并行的场景**：两个 PR 都改 lifecycle（~30% 的 session 类 PR）。这是预期的——lifecycle 的拆分代价大于收益，留待将来 spawn 流水线重构时一起做。

---

## 回滚预案

每个 Phase 一个 commit（Phase 1/4 为双 commit）。出问题：

```bash
# 回滚单个 phase
git revert <phase-N-commit>

# 回滚多个连续 phase（每个原 commit 生成一个 revert commit）
git revert <phase-N-commit>^..<phase-M-commit>

# 一次性 revert + 人工合并成单个 revert commit（推荐用于多 phase 整体回退）
git revert -n <phase-N-commit>^..<phase-M-commit>
git commit -m "Revert router-split phases N-M"

# 显式列出
git revert <phase-N-commit> <phase-N+1-commit> ... <phase-M-commit>
```

> 注意：
> - `<a>..<b>` 在 git revert 里会跳过 `<a>`，必须用 `<a>^..<b>` 把起点也包含进去
> - `-n` 让 revert 不立即创建 commit，便于多 phase 合并成一个 revert，避免污染主分支历史

由于每个 Phase 都跑过完整 race test，回滚某一 phase 不影响其他 phase。

最坏情况整体回滚：

```bash
git checkout master -- internal/session/router*.go
go build ./... && go test -race ./...
```

---

## 评审反馈追溯（v4 改动清单）

v4 相对 v3 的修订（基于第三轮 architect + go-reviewer 反馈）：

| 评审项 | 类别 | v4 修订 |
|---|---|---|
| Phase 1a 凑出来的烟雾测试，业务不自洽 | 阻断 | **采用方案 A**：砍掉 1a/1b，Phase 1 一刀切 lifecycle ~1280 行 |
| blame 断裂缺"为什么仍然值得"硬论证 | 阻断 | §blame 断裂的成本评估：14 commits/12mo + 0 次显式调古 → 接受断裂 |
| 双 commit 规则未按行数分档 | 改进 | 行数分档钉死：>500 推荐 / 300-500 可选 / <300 禁止 |
| `isExemptKey` 等 free function 归属未列 | 改进 | §functions 表新增，覆盖 16 个包级 free function |
| goimports "自动修"描述误导 | 改进 | 区分 Phase 0 / 1-5 角色 + 实操流程 |
| `git revert <a>^..<b>` 多 commit 困惑 | 改进 | 加 `-n` 选项 + 人工合并说明 |
| Phase 1b 落地前需 grep 对账 | 改进 | Phase 1 搬动清单加"落地前必须 grep 对账"说明 |

v3 → v4 删除的错误描述：
- "Phase 1a / 1b 切分"（机械烟雾测试，业务不自洽）

---

## 评审历史（v1 → v4）

| 版本 | 日期 | 主要修订 |
|---|---|---|
| v1 | 2026-05-18 | 初稿，6 文件切分 |
| v2 | 2026-05-18 | 第一轮反馈：归属漂移修复、双 commit 策略、phase 顺序倒过来、字段读写注释 |
| v3 | 2026-05-18 | 第二轮反馈：删双 commit 保 blame 错觉、顶层符号归属表、Phase 1 切 1a/1b、冻结时长统一 |
| v4 | 2026-05-18 | 第三轮反馈：砍 1a/1b、blame 论证、双 commit 行数分档、goimports 角色、free function 归属 |

---

## 参考

- [docs/design/server-split-design.md](server-split-design.md) —— server 包拆分的同款方法论
- [feedback_dev_workflow_skill](../../.claude/skills/dev-workflow) —— 项目级开发流程：功能必走设计 + 独立评审
- 调研：业界 worktree + 模块化耦合关系——"模块化质量决定并行上限"，本次拆分就是把 router.go 的并行上限从 1 提到 5+
