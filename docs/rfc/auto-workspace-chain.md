# RFC: Auto Workspace Chain — workspace 自动 prev_session_ids 拼接

- **Status**: Draft v3（v2 复审：Go 评审发现 1 个新 BLOCKING / Arch APPROVE-WITH-MINOR-CHANGES 给 3 个 MINOR；v3 处理全部 4 项）
- **Date**: 2026-05-23
- **Owner**: Kevin
- **Bucket**: A（Feature；触碰 session router、persistence、startup 流程；Risk Checklist 命中 4 项 → 必走完整设计路径）
- **Reviews**: see §13 for verdict + diff between v1 → v2 → v3

---

## 1. Background & problem

### 现象

在 dashboard 的某个 session（以 `dashboard:direct:2026-05-23-183724-1-gaokao:general` / `sessionID=71241af1-…d4ea2a` 为例）翻历史时，会停在 "没有更早的事件"，但实际上同一个工作区（`/home/ec2-user/workspace/gaokao`）今天还有 7 个其它 naozhi session（命名 `dashboard:direct:2026-05-23-{HHMMSS}-…-gaokao:general`），每个挂独立的 CLI sessionID JSONL，加起来才是用户认知里"今天和这个项目的完整对话"。

### 根因

naozhi 当前只在两条路径上自动接续 chain：

1. 同 naozhi key 内 CLI 换了 sessionID（`/clear` / `/new`）：`router_lifecycle.go:520`
2. dashboard "恢复历史" 按钮：`RegisterForResume`

**没有**"按 workspace 把多次独立打开的 session 自动归并"的机制。

### 为什么必须 naozhi 来做

- dashboard 翻历史走 `LoadHistoryChainBeforeCtx`，要求 chain 已经持久化在 `prev_session_ids`
- 手动维护 `sessions.json` 不可持续
- "看完整原始 JSONL"（直接读文件）解决不了"我就是想在 dashboard 里看"的诉求

---

## 2. Goals & non-goals

### Goals

- **G1**: 新建 naozhi session 时，若该 session 是该 workspace 下"语义上的第一条"（无 prev、无 oldHistory），自动用同 workspace 下近 7 天内、未被任何其它已知 session（含 cron/sys 内部）占用的 sessionID 填充 `prev_session_ids`。
- **G2**: 首次启动 naozhi 时一次性回填所有现存 prev 为空的 session（包括今天 gaokao 那 8 个）。
- **G3**: 默认开启，可在 `config.yaml` 关闭。
- **G4**: 不引入跨 workspace、cron / sys / scratch 内部 sessionID 误接。
- **G5**: chain 长度受现有 `maxPrevSessionIDs = 32` cap 约束，时间窗 7 天可配置。
- **G6**: 用户在 dashboard 看到陌生对话时，运维侧能从 sessions.json + slog 直接溯源到"哪条 sessionID 是 auto-chain 接进来的，何时接的"。
- **G7**: 为 Phase 3 的 per-workspace 开关预留接口位（policy 注入），不要锁死全局开关。

### Non-goals

- ❌ 不改 dashboard 翻历史协议
- ❌ 不新增 UI（开关只在 `config.yaml`）
- ❌ 不修改 chain 长度上限 / `maxPersistedHistory`
- ❌ 不做"语义聚类"
- ❌ 不处理远程 node chain
- ❌ 不做 chain GC / 旧 JSONL 清理

---

## 3. Alternatives considered

### A. 每次 dashboard 进入 session 时即时聚合（不持久化）— ✘ 1Hz poll 下扫盘开销不可接受 + cursor 不稳

### B. dashboard 加"自动接续工作区历史"按钮 — ✘ 用户明确要"后台自动拼接"

### C. 首次启动一次性回填 + 新建时自动接（推荐）— ✓ 复用 chain 持久化路径，最少新增依赖

### D. 直接拓宽 `RecentSessions` 让它返回 prev_session_ids 模型 — ✘ 破坏 RecentSessions 只读语义；chain 决策需要 active session 占用集合，不该从 discovery 反向拉

**选 C。**

---

## 4. Design

### 4.1 整体数据流

```
config.yaml
  session.auto_chain.enabled: true (默认)
  session.auto_chain.window_hours: 168 (7d)
  session.auto_chain.cap: 32

  ┌─────────────────────────────────┐
  │ NewRouter (一次性回填，单线程)    │
  │  1. 收集 used (含 cron/sys)       │
  │  2. 在 r.mu 外按 workspace 扫盘  │
  │  3. 二次 lock 内逐 session 应用  │
  │  4. 必须 BEFORE Tier 2 异步加载   │
  └─────────────────────────────────┘

  ┌─────────────────────────────────┐
  │ spawnSession (新建路径)          │
  │  仅当 oldHistory==nil &&         │
  │     prevIDs==nil:               │
  │  1. lock 内取 used               │
  │  2. lock 外扫盘 + 计算           │
  │  3. lock 内 二次校验 used        │
  │  4. 应用到 installFreshSession   │
  └─────────────────────────────────┘

         pickWorkspaceChain(workspace, ListJSONL, excluder, cfg, now):
           1. ListJSONL(workspace) → discovery 包提供
           2. 排除 excluder.IsExcluded(id) （cron/sys/scratch + active session 占用）
           3. 排除 IsValidSessionID 不通过 / size==0
           4. 排除 mtime < now - window
           5. mtime asc 排序，取前 cap-1
```

### 4.2 抽象层归属（解决 Go-B1 / Arch-B1）

#### `internal/discovery/workspace_jsonl.go`（**新增；扫盘归属正确层**）

```go
// WorkspaceJSONL is one .jsonl file under ~/.claude/projects/<slug>/.
// Re-exports the package-internal jsonlFileInfo as a stable public type
// for callers in session/ that need the (id, mtime) pair.
type WorkspaceJSONL struct {
    SessionID string
    Mtime     int64 // unix ms
}

// ListWorkspaceJSONL enumerates .jsonl files for the workspace's claude
// project directory. Backed by the existing dirFilesCache (mtime-keyed),
// so repeated calls during startup or per-spawn cost ~one Stat each
// after the first ReadDir.
//
// Returns nil for empty claudeDir or workspace, or when the project
// directory does not exist. Filters: must end in .jsonl, size > 0,
// IsValidSessionID(name).
func ListWorkspaceJSONL(claudeDir, workspace string) []WorkspaceJSONL
```

实现：复用 `cachedJSONLFileInfo(projDir)` 既有逻辑，仅做包导出 + IsValidSessionID 二次过滤即可。**零新扫盘成本**。

#### `internal/session/auto_chain.go`（**新增；纯决策**）

```go
// SessionIDExcluder reports whether a sessionID should be excluded from
// auto-chain candidates. Implementations must be safe for concurrent
// use and side-effect-free (no I/O on the hot path). Hooked into the
// router by AddSessionIDExcluder, mirroring the existing
// discovery.RecentSessionsFilter pattern.
type SessionIDExcluder interface {
    IsExcluded(sessionID string) bool
}

// AutoChainPolicy is the per-decision policy interface (Arch-B4).
// Default impl reads from RouterConfig.AutoChain; future ProjectManager
// can implement same interface for per-workspace override.
type AutoChainPolicy interface {
    // Enabled reports whether auto-chain is on for the given workspace.
    Enabled(workspace string) bool
    Window(workspace string) time.Duration
    Cap(workspace string) int
}

// pickWorkspaceChain returns sessionIDs to auto-prefix to a freshly
// created session's prev_session_ids. Pure: no router mutation.
//
// Caller-supplied seams:
//   - listJSONL: discovery.ListWorkspaceJSONL (or test fake)
//   - excluder: combined router + cron + sys excluder set
//   - policy: AutoChainPolicy (global config or per-workspace override)
//   - now: clock injection
func pickWorkspaceChain(
    workspace string,
    listJSONL func(workspace string) []discovery.WorkspaceJSONL,
    excluder SessionIDExcluder,
    policy AutoChainPolicy,
    now time.Time,
) []string
```

**Sentinel checks at top (Go-B5)**:
```go
if workspace == "" || !policy.Enabled(workspace) {
    return nil
}
window := policy.Window(workspace)
if window <= 0 {
    window = 7 * 24 * time.Hour
}
cap := policy.Cap(workspace)
if cap <= 0 || cap > maxPrevSessionIDs {
    cap = maxPrevSessionIDs
}
```

**逻辑（≈40 LOC）**:
1. `files := listJSONL(workspace)`
2. 过滤 mtime ≥ `now - window` AND `!excluder.IsExcluded(file.SessionID)`
3. `sort.SliceStable` by mtime asc
4. 取**末尾** `cap-1` 项（保留 mtime 最新的；留 1 槽给当前 session）
5. 返回 ID 列表（保持 mtime asc，最早的在前）

**关于 4 步语义（v3.1 修订）**：当 workspace 内候选超过 cap 时，**保留最新的 cap-1 项**而不是最早的。理由：dashboard "翻历史" 从最新往老翻，链上最早那段离当前对话语义最远；用户更想看到的是与现在对话连续的最近上下文。返回的 slice 仍按 mtime asc 排序，所以 prev_session_ids 字段保持 oldest→newest 的现有契约。

### 4.3 Excluder 装配（解决 Arch-B2）

#### Router 的 excluder 聚合

```go
// Router 字段新增
excluders atomic.Pointer[[]SessionIDExcluder]

// AddSessionIDExcluder appends an excluder. Called once at startup wiring
// (cmd/naozhi/main.go) for cron Scheduler and sysession Manager.
// Atomic copy-on-write so reads from pickWorkspaceChain are lock-free.
func (r *Router) AddSessionIDExcluder(e SessionIDExcluder) {
    for {
        cur := r.excluders.Load()
        next := make([]SessionIDExcluder, 0, lenOrZero(cur)+1)
        if cur != nil {
            next = append(next, *cur...)
        }
        next = append(next, e)
        if r.excluders.CompareAndSwap(cur, &next) {
            return
        }
    }
}
```

#### 三个内置 excluder

| Excluder | 实现 | 来源 |
|---|---|---|
| `routerSessionsExcluder` | r.mu 下扫 r.sessions 收集 sessionID + prev_session_ids | session 包内 |
| `cronExcluder` | 包装 `*cron.Scheduler.KnownSessionIDs()` | cmd/naozhi 装配 |
| `sysExcluder` | 包装 `*sysession.Manager.KnownSessionIDs()` | cmd/naozhi 装配（sysession 包需新增同名方法） |

#### combinedExcluder

```go
type combinedExcluder struct {
    inner []SessionIDExcluder
}

func (c combinedExcluder) IsExcluded(id string) bool {
    for _, e := range c.inner {
        if e.IsExcluded(id) {
            return true
        }
    }
    return false
}
```

`pickWorkspaceChain` 调用方组装 `combinedExcluder{r.excluders.Load() + routerSnapshotExcluder}`，前者一次构建跨多次 spawn 复用，后者每次 spawn 实时取（因为 r.sessions 在变）。

#### 与 `discovery.RecentSessionsFilter` 的关系（v3 Arch-MINOR-2）

`discovery.RecentSessionsFilter.SkipSessionID(id) bool` 与 `SessionIDExcluder.IsExcluded(id) bool` 是同语义。为避免 cron / sys 在两个接口里实现两遍同代码，提供 adapter：

```go
// recentFilterAsExcluder lets any RecentSessionsFilter satisfy
// SessionIDExcluder for the auto-chain pipeline. Workspace skipping
// is irrelevant here (auto-chain is already scoped to one workspace),
// so SkipWorkspace is ignored.
type recentFilterAsExcluder struct {
    f discovery.RecentSessionsFilter
}

func (a recentFilterAsExcluder) IsExcluded(id string) bool {
    return a.f.SkipSessionID(id)
}

// AsExcluder is the public factory used by cmd/naozhi wiring:
//   router.AddSessionIDExcluder(session.AsExcluder(scheduler.AsRecentFilter()))
func AsExcluder(f discovery.RecentSessionsFilter) SessionIDExcluder {
    return recentFilterAsExcluder{f: f}
}
```

cron Scheduler / sysession Manager 只实现一次 RecentSessionsFilter（其中 `SkipSessionID` 包装 `KnownSessionIDs()`），auto-chain 路径用 `AsExcluder` 复用，history 面板原有路径不变。

### 4.4 Router 注入点（解决 Go-B1 / Go-B2 / Go-B3）

#### A) 新建路径（spawnSession）

`router_lifecycle.go:670` 之后、`installFreshSessionLocked` 调用之前：

```go
oldHistory, prevIDs := collectPreviousHistory(old, oldPrevIDs, resumeID)

// === auto-chain attach BEGIN ===
// Skip 内部 session（cron / sys / scratch）—— 它们不参与 chain 拼接。
isInternal := IsCronKey(key) || IsSysKey(key) || IsScratchKey(key)
var autoChainAttached []string
if !isInternal && r.autoChainPolicy.Enabled(workspace) &&
   len(prevIDs) == 0 && len(oldHistory) == 0 {

    // 阶段 1: lock 内取 used 快照（含 r.sessions + 其它 excluder）
    r.mu.Lock()
    routerExcluder := r.snapshotRouterExcludedLocked()  // O(N) map 拷贝
    extraExcluders := r.excluders.Load()                // atomic ptr
    r.mu.Unlock()

    // 阶段 2: lock 外扫盘 + 决策（耗时操作不阻塞其它 spawn）
    excluder := combinedExcluder{
        inner: append([]SessionIDExcluder{routerExcluder}, deref(extraExcluders)...),
    }
    auto := pickWorkspaceChain(workspace, discovery.ListWorkspaceJSONL,
                               excluder, r.autoChainPolicy, time.Now())

    // 阶段 3: lock 内二次校验（Go-B2 + New-B1 TOCTOU）
    //
    // CRITICAL (New-B1, v3): filterStillUnused 必须重过**完整 combinedExcluder**
    // —— r.sessions + cron + sys + 任何后续注册的 excluder。仅查 r.sessions
    // 会让 cron 在阶段 2-3 间隙起新 job 占用候选 ID 的场景漏防（破 G4）。
    // r.excluders 在 r.mu 内 atomic Load 拿到的是 publish 后的指针，配合
    // routerSnapshotExcludedLocked 的实时 r.sessions 视图，是闭环二次校验。
    if len(auto) > 0 {
        r.mu.Lock()
        verifyExcluder := combinedExcluder{
            inner: append(
                []SessionIDExcluder{r.snapshotRouterExcludedLocked()},
                deref(r.excluders.Load())...,
            ),
        }
        r.mu.Unlock()
        verified := filterByExcluder(auto, verifyExcluder)
        if drops := len(auto) - len(verified); drops > 0 {
            metrics.AutoChainTOCTOUCollisionTotal.Add(int64(drops))
            slog.Warn("auto-chain TOCTOU drop on spawn",
                "key", key, "workspace", workspace,
                "dropped", drops, "kept", len(verified))
        }
        if len(verified) > 0 {
            autoChainAttached = verified
            prevIDs = verified
        }
    }
}
// === auto-chain attach END ===

r.mu.Lock()
// ... TOCTOU guard 2 ...

s := r.installFreshSessionLocked(...)
r.mu.Unlock()

if len(autoChainAttached) > 0 {
    // Origin 标注 + 完整 ID 列表落 slog（Arch-B5）
    s.SetPrevSessionOrigins(autoChainAttached, "auto-spawn")
    slog.Info("auto-chain attached on spawn",
        "key", key, "workspace", workspace,
        "chain_ids", autoChainAttached,  // 完整列表
        "chain_len", len(autoChainAttached))
    metrics.AutoChainSpawnAttachTotal.Add(1)
}
```

#### B) 启动一次性回填

`router_core.go` 在 Tier 1 异步加载启动**之前**（解决 Go-B4 / Arch-B3）：

```go
// === Auto-chain backfill BEFORE Tier 1/2 historical loaders ===
// CRITICAL: must run synchronously before any goroutine in the
// "Async-load history" block below is launched. Otherwise Tier 2
// would observe an outdated chain (still empty for old sessions),
// load only the current sessionID's JSONL, and we'd end up with
// dashboard pagination working only for newly-created sessions.
// Pinned by TestNewRouter_AutoChainPrecedesTier2Loaders (§5.2).
if r.autoChainPolicy != nil {
    r.runAutoChainBackfillOnce()
}

// Tier 1: naozhilog
if r.eventLogPersister != nil { ... goroutines ... }
// Tier 2: Claude CLI JSONL
if r.claudeDir != "" { ... goroutines ... }
```

`runAutoChainBackfillOnce` 行为（解决 Arch B3 + 排序确定性）：

```go
func (r *Router) runAutoChainBackfillOnce() {
    // Phase 1: 单 lock 收集所有候选 + 整体 used baseline
    r.mu.Lock()
    candidates := make([]*ManagedSession, 0)
    for _, s := range r.sessions {
        if IsCronKey(s.key) || IsSysKey(s.key) || IsScratchKey(s.key) {
            continue
        }
        s.historyMu.RLock()
        empty := len(s.prevSessionIDs) == 0
        s.historyMu.RUnlock()
        if !empty {
            continue
        }
        if s.Workspace() == "" {
            metrics.AutoChainBackfillSkippedTotal.WithLabelValues("no_workspace").Inc()
            continue
        }
        candidates = append(candidates, s)
    }
    routerExcluder := r.snapshotRouterExcludedLocked()
    extraExcluders := r.excluders.Load()
    r.mu.Unlock()

    // 排序确定性（Arch-B-nit）：按 lastActive asc，最早创建的优先拿 chain
    sort.SliceStable(candidates, func(i, j int) bool {
        return candidates[i].lastActive.Load() < candidates[j].lastActive.Load()
    })

    // Phase 2: lock 外逐个决策；每次决策 used 集合都加上之前已分配的
    //
    // 并发约束（v3 Arch-MINOR-3）: consumed 是普通 map 无锁。runAutoChainBackfillOnce
    // 是 NewRouter 启动期单线程同步调用 —— 无并发写入。任何把回填改成并发跑的
    // 改造（如 per-workspace 回填 fan-out）必须先把 consumed 换成 sync.Map 或加 mu。
    consumed := map[string]bool{}
    consumedSelfExcluder := selfExcluder{set: consumed}
    excluderBase := combinedExcluder{
        inner: append([]SessionIDExcluder{routerExcluder, consumedSelfExcluder}, deref(extraExcluders)...),
    }

    type decision struct {
        s   *ManagedSession
        ids []string
    }
    decisions := make([]decision, 0, len(candidates))
    for _, s := range candidates {
        ids := pickWorkspaceChain(s.Workspace(), discovery.ListWorkspaceJSONL,
                                  excluderBase, r.autoChainPolicy, time.Now())
        if len(ids) == 0 {
            metrics.AutoChainBackfillSkippedTotal.WithLabelValues("no_candidates").Inc()
            continue
        }
        for _, id := range ids {
            consumed[id] = true
        }
        decisions = append(decisions, decision{s: s, ids: ids})
    }

    // Phase 3: 单 lock 应用所有决策（Go-B3 historyMu + r.mu 双 lock）
    //
    // CRITICAL (New-B1, v3): 在 r.mu 内重新组装 verifyExcluder（含 r.sessions
    // 实时视图 + atomic-load 的 cron/sys 等），逐 ID 过滤 d.ids 后再 apply。
    // 不能复用 Phase 2 的 excluderBase —— 那是 lock 外快照，cron/sys 在
    // Phase 2-3 间隙可能新增 sessionID。
    if len(decisions) == 0 {
        return
    }
    r.mu.Lock()
    verifyBase := []SessionIDExcluder{r.snapshotRouterExcludedLocked()}
    verifyBase = append(verifyBase, deref(r.excluders.Load())...)
    storeDirty := false
    for _, d := range decisions {
        verified := filterByExcluder(d.ids, combinedExcluder{inner: verifyBase})
        if drops := len(d.ids) - len(verified); drops > 0 {
            metrics.AutoChainTOCTOUCollisionTotal.Add(int64(drops))
        }
        if len(verified) == 0 {
            metrics.AutoChainBackfillSkippedTotal.WithLabelValues("toctou_drop").Inc()
            continue
        }

        d.s.historyMu.Lock()
        // 二次校验：lock 间隙可能其它逻辑改了 prev（极少见，但留底）
        if len(d.s.prevSessionIDs) != 0 {
            d.s.historyMu.Unlock()
            metrics.AutoChainBackfillSkippedTotal.WithLabelValues("already_filled").Inc()
            continue
        }
        d.s.prevSessionIDs = slices.Clone(verified)
        d.s.historyMu.Unlock()
        d.s.SetPrevSessionOrigins(verified, "auto-backfill")
        slog.Info("auto-chain backfill",
            "key", d.s.key, "workspace", d.s.Workspace(),
            "chain_ids", verified, "chain_len", len(verified))
        metrics.AutoChainBackfillAttachTotal.Add(1)
        storeDirty = true
    }
    if storeDirty {
        r.storeDirty = true
        r.storeGen.Add(1)
    }
    r.mu.Unlock()
}
```

### 4.5 Lock 顺序契约（解决 Go-B3）

| 场景 | r.mu | historyMu |
|---|---|---|
| 读 prevSessionIDs（pickChain 调用方） | 不持 | RLock 短暂持 |
| 写 prevSessionIDs（spawnSession 路径） | 已通过 installFreshSessionLocked 持有 | 不需要（installFreshSessionLocked 内部 attachProcessAndSnapshotPersisted 走 historyMu） |
| 写 prevSessionIDs（auto-chain 回填路径） | **持** | **持**（顺序：先 r.mu 后 historyMu，与 SnapshotChainIDs 注释 §11.1 兼容） |
| SnapshotChainIDs 读 | 不持（factory 调用） | RLock 持 |

具体实施：`s.prevSessionIDs = slices.Clone(d.ids)` 必须在 `s.historyMu.Lock()` 内完成（v2 §4.4-B 已修正）。

### 4.6 Schema 演化（解决 Arch-B5）

#### `internal/session/store.go::storeEntry`

```go
type storeEntry struct {
    Workspace      string   `json:"workspace,omitempty"`
    SessionID      string   `json:"session_id,omitempty"`
    PrevSessionIDs []string `json:"prev_session_ids,omitempty"`

    // NEW v2: 与 PrevSessionIDs 同位的 origin 标注。空 => "manual"（向后兼容）。
    // 取值: "manual" | "auto-spawn" | "auto-backfill" | "resume"
    PrevSessionOrigins []string `json:"prev_session_origins,omitempty"`

    // ... 其他字段 ...
}
```

#### ManagedSession

```go
prevSessionOrigins []string  // 与 prevSessionIDs 一一对应；len 不一致按 manual 兜底

// SetPrevSessionOrigins records the origin of the most-recently-appended
// chain segment. Existing prefixes are preserved with their prior origin
// (or "manual" when unknown).
//
// INVARIANT (v3 Arch-MINOR-1): prev_session_ids 是 append-only 的。任何
// 插入/删除路径（如假设的中间删除某条）都必须同步改 prevSessionOrigins，
// 否则 origin↔id 错位。当前所有写路径都是 append（spawnSession 链续、
// auto-chain backfill、RegisterCronStubWithChain 替换全量），所以 origin
// 数组只会 grow。SetPrevSessionOrigins 自带断言：如果发现 origins 与 ids
// 长度漂移超过 1（语义上不可能），落 metric 并按 manual 兜底而不 panic。
func (s *ManagedSession) SetPrevSessionOrigins(ids []string, origin string) {
    s.historyMu.Lock()
    defer s.historyMu.Unlock()
    // 长度漂移检测（v3 Arch-MINOR-1）
    diff := len(s.prevSessionIDs) - len(s.prevSessionOrigins)
    if diff < 0 || diff > len(ids) {
        // 漂移：要么 origins 比 ids 长（不可能正常路径），要么本次 append
        // 不在末尾（违反 append-only invariant）。落指标后兜底重建。
        metrics.AutoChainOriginsLengthMismatch.Add(1)
        slog.Warn("auto-chain origins length drift; rebuilding to manual",
            "key", s.key,
            "ids_len", len(s.prevSessionIDs),
            "origins_len", len(s.prevSessionOrigins),
            "incoming_len", len(ids))
        rebuilt := make([]string, len(s.prevSessionIDs))
        for i := range rebuilt {
            rebuilt[i] = "manual"
        }
        s.prevSessionOrigins = rebuilt
    }
    // 扩容到与 prevSessionIDs 同长
    if len(s.prevSessionOrigins) < len(s.prevSessionIDs) {
        grown := make([]string, len(s.prevSessionIDs))
        copy(grown, s.prevSessionOrigins)
        for i := len(s.prevSessionOrigins); i < len(grown); i++ {
            grown[i] = "manual" // 旧前缀默认 manual
        }
        s.prevSessionOrigins = grown
    }
    // 标注本次 append 的尾段
    start := len(s.prevSessionIDs) - len(ids)
    if start < 0 {
        return
    }
    for i := range ids {
        s.prevSessionOrigins[start+i] = origin
    }
}
```

向后兼容：旧 sessions.json 没有 `prev_session_origins` → `nil` → 渲染时按 "manual" 兜底，不破坏 schema 演化。

### 4.7 配置加载

`internal/config/config.go::SessionConfig`：

```go
type SessionConfig struct {
    // ...
    AutoChain AutoChainConfig `yaml:"auto_chain,omitempty"`
}

type AutoChainConfig struct {
    Enabled     *bool `yaml:"enabled,omitempty"`
    WindowHours int   `yaml:"window_hours,omitempty"`
    Cap         int   `yaml:"cap,omitempty"`
}

// Resolved returns the effective enabled flag; nil → default.
func (c AutoChainConfig) Resolved(def bool) bool {
    if c.Enabled == nil {
        return def
    }
    return *c.Enabled
}
```

`cmd/naozhi/main.go`：

```go
policy := session.GlobalAutoChainPolicy{
    EnabledFlag: cfg.Session.AutoChain.Resolved(true),
    WindowDur:   resolveDurationHours(cfg.Session.AutoChain.WindowHours, 7*24*time.Hour),
    CapValue:    resolveInt(cfg.Session.AutoChain.Cap, 32),
}
router := session.NewRouter(session.RouterConfig{
    // ...
    AutoChainPolicy: policy,
})
router.AddSessionIDExcluder(scheduler)        // *cron.Scheduler 实现 SessionIDExcluder
router.AddSessionIDExcluder(sysessionMgr)     // *sysession.Manager 实现 SessionIDExcluder
```

`cron.Scheduler.IsExcluded` / `sysession.Manager.IsExcluded` 包装现有的 `KnownSessionIDs() map[string]bool` 即可。

---

## 5. Test strategy

### 5.1 Unit tests（`internal/session/auto_chain_test.go`）

| 测试 | 期望 |
|---|---|
| `TestPickChain_HappyPath_OrderedByMtime` | 三个 JSONL by mtime 升序返回 |
| `TestPickChain_RespectsWindow` | 超过 window 的被丢 |
| `TestPickChain_ExcludesUsedIDs` | excluder 标记的 ID 不入 chain |
| `TestPickChain_RejectsInvalidSessionID` | 非 UUID 文件被丢 |
| `TestPickChain_RejectsZeroByteJSONL` | 空文件被丢 |
| `TestPickChain_DisabledReturnsNil` | policy.Enabled=false 立即返回 nil |
| `TestPickChain_RespectsCap_KeepsNewest` | 超出 cap 取**最新**的 cap-1 项（v3.1 修订：dashboard 从最新往老翻，链上最早段离当前对话最远，价值低） |
| `TestPickChain_CapZeroDefaultsTo32` | cap=0 默认 32（Go-B5 sentinel） |
| `TestPickChain_WindowZeroDefaultsTo7d` | window=0 默认 7d |
| `TestPickChain_NowInjectionSeam` | now 参数可控 |
| `TestPickChain_EmptyWorkspace` | workspace 空 → nil |
| `TestPickChain_NonexistentSlug` | 没有目录 → nil |

### 5.2 Integration tests（`internal/session/auto_chain_router_test.go`）

| 测试 | 期望 |
|---|---|
| `TestSpawnSession_AutoChainNewSession` | 同 workspace 已有 1 个 sessionID，新 session prev 含它 + origin="auto-spawn" |
| `TestSpawnSession_AutoChainSkipsCronKey` | `cron:` key 不接 chain |
| `TestSpawnSession_AutoChainSkipsResumeBranch` | resumeID 非空时跳过 |
| `TestSpawnSession_AutoChainConcurrentSpawn` | 两 goroutine 同时 spawn 进同一 workspace，两个 session 不会接到同一 sessionID（Go-B2 TOCTOU 二次校验生效） |
| `TestSpawnSession_AutoChainCronStartsBetweenLockWindows` | spawn 阶段 2-3 之间 cron 起新 job 占用候选 ID，阶段 3 verifyExcluder 必须丢弃该 ID（v3 New-B1） |
| `TestSpawnSession_AutoChainExcludesCron` | cron 跑过的 sessionID 不被用户 session 接（Arch-B2） |
| `TestSpawnSession_AutoChainExcludesSys` | sys 内部 sessionID 不被接 |
| `TestNewRouter_BackfillFillsEmptyChains` | 启动时已存 session prev 为空被回填 + origin="auto-backfill" |
| `TestNewRouter_BackfillNoDoubleAssign` | 两 session 抢同 sessionID，按 lastActive asc 谁早谁拿 |
| `TestNewRouter_BackfillRespectsDisabled` | enabled=false 不动现有 prev |
| `TestNewRouter_BackfillSkipsCronStubs` | cron stub prev 为空也不接 |
| `TestNewRouter_AutoChainPrecedesTier2Loaders` | **顺序断言**：backfill 必须在 Tier 2 goroutine 启动前完成（Go-B4 / Arch-B3） |
| `TestNewRouter_BackfillCronStartsMidPhase` | Phase 2-3 之间 cron 注册新 sessionID 占用候选，Phase 3 verifyBase 丢弃（v3 New-B1） |
| `TestBackfillConcurrentWithSpawn` | 启动期 backfill 进行中，模拟 spawn 请求到达；两条路径不会写到同一 sessionID（v3 Go-MEDIUM） |
| `TestSetPrevSessionOrigins_LengthDriftRecovery` | 人为构造 origins 长度漂移 → metric 落 + 兜底 manual + 不 panic（v3 Arch-MINOR-1） |
| `TestStoreRoundtrip_PrevSessionOrigins` | sessions.json 写入后再加载，origin 字段保留 |
| `TestStoreLoad_LegacySchemaWithoutOrigins` | 旧 schema 无 origins 字段 → 兜底 manual |
| `TestRecentFilterAsExcluder_Adapter` | discovery.RecentSessionsFilter → SessionIDExcluder adapter 正确转发 SkipSessionID（v3 Arch-MINOR-2） |

### 5.3 Race detection

`go test -race ./internal/session/... -run AutoChain` 必须全绿。重点：spawnSession concurrent 测试 + backfill 与 Tier 2 race。

### 5.3.1 测试注入钩子（v3 Go 终审强制要求）

并发测试**禁止用 sleep 复现 race**，必须机械化复现两次 lock 间隙的 cron/sys 注册。Router 上加 build-tag 隔离的 test hooks：

```go
//go:build testhooks

// testHookBeforeSpawnPhase3 在 spawnSession 阶段 2 完成、阶段 3 lock 之前
// 触发。生产构建（无 testhooks tag）此字段不存在，零开销。
//
// 测试用法（auto_chain_router_test.go）:
//   ready := make(chan struct{})
//   r.testHookBeforeSpawnPhase3 = func() {
//       close(ready)
//       <-cronInjectedDone  // 等 cron excluder 加新 sessionID 完成
//   }
testHookBeforeSpawnPhase3 func()
testHookBeforeBackfillPhase3 func()
```

调用点：
- spawnSession §4.4-A 阶段 2 末尾（`auto := pickWorkspaceChain(...)` 之后、阶段 3 `r.mu.Lock()` 之前）`if r.testHookBeforeSpawnPhase3 != nil { r.testHookBeforeSpawnPhase3() }`
- runAutoChainBackfillOnce §4.4-B Phase 2 末尾（decisions 收集完之后、Phase 3 lock 之前）同上

`TestSpawnSession_AutoChainCronStartsBetweenLockWindows` / `TestNewRouter_BackfillCronStartsMidPhase` / `TestBackfillConcurrentWithSpawn` 三个测试**必须**通过这两个钩子，配合 sync.WaitGroup / channel 完成精确卡点，**禁止 `time.Sleep`**。审查时若发现 sleep 必须打回。

### 5.4 Regression evidence

启动前抓 `sessions.json`；启动后 dashboard 翻 gaokao session 应能看到陌生 sessionID 的对话内容（这正是用户诉求）。

### 5.5 不能 break 的现有测试

- `TestSnapshotChainIDs_*`（managed.go）
- `cron_stub_test.go::RegisterCronStubWithChain`
- `router_history_test.go`
- `event_entries_test.go`

---

## 6. Risk & rollback

### 风险矩阵 v2

| 风险 | 概率 | 严重性 | 缓解（已实施） |
|---|---|---|---|
| 误接到别人对话（非本 workspace） | 低 | 高 | `discovery.ClaudeProjectSlug` 路径锚定 + `IsValidSessionID` |
| 抢走 active session 的 sessionID | 低 | 高 | 三段 lock + 二次校验（Go-B2） + selfExcluder（回填阶段每次决策叠加） |
| 接到 cron / sys 内部 sessionID | 低 | 中 | SessionIDExcluder 接口 + cron/sys 装配（Arch-B2） |
| 启动期回填阻塞 spawn | 低 | 中 | 三段 lock 模式：扫盘在 lock 外 |
| Tier 2 顺序错乱 | 低 | 高 | 顺序由代码强制 + `TestNewRouter_AutoChainPrecedesTier2Loaders` pin（Go-B4 / Arch-B3） |
| sessions.json schema 不兼容 | 极低 | 中 | omitempty + 默认值兜底（Arch-B5） |
| 用户报"陌生对话"无法溯源 | 中 | 中 | `prev_session_origins` 字段 + slog dump 完整 ID 列表（Arch-B5） |
| historyMu vs r.mu lock 顺序错 | 低 | 高 | §4.5 lock 契约表 + race test |

### 回滚路径

- **配置回滚**: `auto_chain.enabled: false` → restart → 立即停止新接，已写入的 prev 保留，**origin 字段保留可供事后审计** ← 比 v1 更可控
- **数据回滚**: 升级前自动备份 `~/.naozhi/sessions.json`（在 install.sh / `naozhi upgrade` 钩子里加），改回 → restart
- **代码回滚**: feature 在 4 个文件（auto_chain.go、router_core.go 注入点、router_lifecycle.go 注入点、store.go schema 字段），revert 1 个 commit 即可

---

## 7. Observability

### 新增 metrics（`internal/metrics`）

```go
AutoChainSpawnAttachTotal       // 新建路径接 chain 次数
AutoChainBackfillAttachTotal    // 启动回填触及 session 数（一次性）
AutoChainBackfillSkippedTotal   // 标签: no_workspace | no_candidates | already_filled
AutoChainPickReturnZero         // pickChain 返回空次数
AutoChainTOCTOUCollisionTotal   // 二次校验发现冲突的次数（理论应低；v3 含 cron/sys 二次过滤命中）
AutoChainAttachLengthHist       // chain_len histogram (1..32)
AutoChainOriginsLengthMismatch  // origins 数组与 prev_session_ids 长度漂移（v3 Arch-MINOR-1，理论应为 0）
```

### slog（关键事件）

- 每次新建接 chain：`Info "auto-chain attached on spawn" key= workspace= chain_ids=[...] chain_len=`
- 启动回填：`Info "auto-chain backfill" key= workspace= chain_ids=[...] chain_len=`
- 启动回填总览：`Info "auto-chain backfill complete" updated= skipped= duration_ms=`
- TOCTOU 冲突：`Warn "auto-chain TOCTOU drop" key= dropped_ids=[...]`
- 异常：`Warn "auto-chain pick failed" workspace= err=`

### 溯源协议（用户反馈"看到陌生对话"时）

1. `cat ~/.naozhi/sessions.json | jq '.[] | select(.key=="<key>") | {prev_session_ids, prev_session_origins}'` —— 一眼看到哪些是 auto
2. `journalctl -u naozhi | grep "auto-chain attached" | grep <sessionID>` —— 找接入时间
3. 对照 dashboard 行为 → 确认是 feature 还是 bug

---

## 8. Compatibility & migration

### 向后兼容

- ✅ `sessions.json` 旧记录无 `prev_session_origins` 字段 → 加载时 nil → 渲染兜底 "manual"
- ✅ `config.yaml` 无 `session.auto_chain` 段 → 默认 enabled=true / window=7d / cap=32
- ✅ 关闭 auto-chain 后行为完全等同当前主线
- ✅ 老的 RegisterForResume / `/clear` / 手动 prev 路径不变

### Migration

- 首次启动**自动**回填所有 prev 为空的 session（标 origin=auto-backfill）
- 操作者透明；行为变化体现为"翻历史能翻更早了"

### Schema 演化（Arch-B5 落实）

- `prev_session_origins` 新增 + omitempty
- 长度可能 < `prev_session_ids` → 缺位按 manual 兜底
- 一次性回填会将所有现存 prev 的 origin 标 manual（保留对历史的诚实标注）

---

## 9. Rollout plan

### Phase 1 — 实现 + 测试（此 PR）

- discovery.ListWorkspaceJSONL
- session.AutoChainPolicy + SessionIDExcluder + pickWorkspaceChain
- spawnSession 注入点（三段 lock）
- NewRouter 启动回填
- store.go schema + ManagedSession.prevSessionOrigins
- config 字段
- metrics + slog
- cron.Scheduler / sysession.Manager 实现 SessionIDExcluder
- 单元 + 集成 + race 测试

### Phase 2 — 灰度

- 默认 enabled=true 跟 PR 上线
- 在 author 部署上跑 24h 观察:
  - dashboard 是否能翻 chain 出来的更早 sessionID
  - sessions.json 大小: 100 session × 32 prev × (36B+12B origin) ≈ 154KB
  - 启动期回填耗时 < 50ms（纯 map 操作 + 复用 dirFilesCache）
  - TOCTOUCollisionTotal 应接近 0

### Phase 3 — 后续可选（不在此 RFC 范围）

- per-workspace 开关（Phase 3 直接实现 `ProjectManagerAutoChainPolicy`，无需改 pickChain 签名 ← Arch-B4 预留生效）
- 命令行触发 backfill：`naozhi backfill-chains`
- window 下调到 24h 的可观测信号驱动

---

## 10. Open questions

| 问题 | v2 回答 |
|---|---|
| cap=32 不够 | 7d × 32 已足；放 OQ 追踪 |
| --resume 路径下跳过 auto-chain 是否过严 | 是合理收紧 |
| 启动回填多 session 共 workspace 是否重复扫盘 | discovery.ListWorkspaceJSONL 走 dirFilesCache，per-projDir 单次 ReadDir |
| 跨 workspace 自动归并 | 不做 |
| 用户故意排除某条 sessionID | Phase 3 加 ignore list；本 RFC 暂用 `chmod 0` JSONL 工作绕开 |
| origin 字段是否需要在 dashboard 渲染 | Phase 3，UI 上加"该段历史来自自动接续"小标签即可 |
| Phase 3 per-workspace policy 是否要 ProjectManager 反向 import | 不要让 session 包反 import projectmanager 多结构。Phase 3 让 ProjectManager 暴露**单方法** `AutoChainOverride(workspace) (enabled, window, cap, ok bool)`，session 包 import 该单方法 interface（已经因 cron/sys 复用 RecentSessionsFilter 模式被验证可行）。本 RFC 不实现，仅记录约定。 |

---

## 11. Out of scope

- 远程 node chain 同步
- 跨 workspace 自动归并
- chain 内分组渲染
- 旧 JSONL 归档/压缩
- dashboard origin 标签 UI

---

## 12. Implementation checklist

- [ ] `internal/discovery/workspace_jsonl.go` — `WorkspaceJSONL` + `ListWorkspaceJSONL`
- [ ] `internal/session/auto_chain.go` — `SessionIDExcluder` / `AutoChainPolicy` / `GlobalAutoChainPolicy` / `combinedExcluder` / `pickWorkspaceChain`
- [ ] `internal/session/router_core.go` — `runAutoChainBackfillOnce` + `excluders` 字段 + `AddSessionIDExcluder`
- [ ] `internal/session/router_lifecycle.go` — spawnSession 注入点（三段 lock）
- [ ] `internal/session/managed.go` — `prevSessionOrigins` 字段 + `SetPrevSessionOrigins`
- [ ] `internal/session/store.go` — `PrevSessionOrigins` 字段（save + load）
- [ ] `internal/cron/scheduler.go` — 实现 `SessionIDExcluder.IsExcluded`
- [ ] `internal/sysession/manager.go` — 同上 + `KnownSessionIDs()` 暴露
- [ ] `internal/config/config.go` — `AutoChainConfig` + `Resolved`
- [ ] `cmd/naozhi/main.go` — wire AutoChainPolicy + AddSessionIDExcluder × 2
- [ ] `internal/metrics/metrics.go` — 7 个 metric（含 v3 新增 AutoChainOriginsLengthMismatch）
- [ ] `config.example.yaml` — 加 `session.auto_chain` 注释样例
- [ ] 单元测试 12 例
- [ ] 集成测试 13 例
- [ ] `go test ./... -race` 全绿
- [ ] `go vet ./...` 无新告警

---

## 13. Reviewer history (audit trail)

### v1 → v2 diff

| ID | 来源 | 问题 | v2 处理 |
|---|---|---|---|
| Go-B1 / Arch-B1 | Go + Arch | pickChain 不该 r.mu 下扫盘；扫盘应归属 discovery | §4.2 抽出 `discovery.ListWorkspaceJSONL`；§4.4-A 三段 lock |
| Go-B2 | Go | TOCTOU: 取 used → 扫盘 → 应用 间隔 | §4.4-A 阶段 3 二次校验 + filterStillUnusedLocked |
| Go-B3 | Go | prevSessionIDs 写要尊重 historyMu | §4.5 lock 契约表 + §4.4-B 双 lock |
| Go-B4 / Arch-B3 | Go + Arch | Tier 2 顺序需代码强制 | §4.4-B 注释 + `TestNewRouter_AutoChainPrecedesTier2Loaders` |
| Go-B5 | Go | cap=0 默认必须 sentinel | §4.2 pickChain 顶部 sentinel checks |
| Arch-B2 | Arch | usedSessionIDs 漏 cron/sys | §4.3 SessionIDExcluder 接口 + cron/sys 装配 |
| Arch-B4 | Arch | 留 per-workspace 开关接口 | §4.2 AutoChainPolicy interface 替代 cfg struct |
| Arch-B5 | Arch | sessions.json schema 加 origins + slog dump 完整 ID | §4.6 schema + §7 slog |
| Nit | Arch | 回填按 lastActive asc 排序 | §4.4-B Phase 1 sort |
| Nit | Go | concurrent spawn 测试 | §5.2 `TestSpawnSession_AutoChainConcurrentSpawn` |
| Nit | Arch | cron 干扰测试 | §5.2 `TestSpawnSession_AutoChainExcludesCron` |
| Nit | Go | chain_len histogram | §7 `AutoChainAttachLengthHist` |

### Verdicts (v1)

- **Go reviewer**: APPROVE-WITH-CHANGES（5 BLOCKING + 4 nit）
- **Architecture reviewer**: APPROVE-WITH-CHANGES（5 BLOCKING + 4 nit）

### v2 复审

| Reviewer | Verdict | 项 |
|---|---|---|
| **Go** | REJECT-V3-NEEDED | 新发现 1 BLOCKING（New-B1）+ 1 MEDIUM（缺 backfill+spawn 并发测试） |
| **Architecture** | APPROVE-WITH-MINOR-CHANGES | 3 MINOR：origins 长度漂移、Excluder 与 RecentSessionsFilter 接口收敛、consumed 单线程注释 |

### v2 → v3 diff

| ID | 来源 | 问题 | v3 处理 |
|---|---|---|---|
| **New-B1** | Go v2 复审 | spawnSession 阶段 3 + backfill Phase 3 二次校验只过 r.sessions，未重过 cron/sys excluder。两次 lock 间隙 cron 起新 job 占用候选 ID 会让 cron sessionID 错接入用户 chain（破 G4）。 | §4.4-A 阶段 3 改为 lock 内重组 verifyExcluder 含 r.excluders；§4.4-B Phase 3 同样重组 verifyBase 逐元素过滤。补 metric `AutoChainTOCTOUCollisionTotal` slog warn |
| **MEDIUM** | Go v2 复审 | 缺 "backfill + spawn 并发"测试 | §5.2 加 `TestBackfillConcurrentWithSpawn` |
| **Arch-MINOR-1** | Arch v2 复审 | origins 数组长度漂移静默 | §4.6 SetPrevSessionOrigins 加 invariant 注释 + 漂移检测 + metric `AutoChainOriginsLengthMismatch` + 兜底重建。§5.2 加 `TestSetPrevSessionOrigins_LengthDriftRecovery` |
| **Arch-MINOR-2** | Arch v2 复审 | SessionIDExcluder 与 discovery.RecentSessionsFilter.SkipSessionID 同语义双套接口 | §4.3 加 `recentFilterAsExcluder` adapter + 公开工厂 `session.AsExcluder(filter)`。cron / sys 只实现 RecentSessionsFilter 一次。§5.2 加 `TestRecentFilterAsExcluder_Adapter` |
| **Arch-MINOR-3** | Arch v2 复审 | Phase 2 consumed map 无锁未声明单线程契约 | §4.4-B Phase 2 加注释明确 NewRouter 启动期单线程；任何并发化改造必须先加锁 |
| **Arch OQ** | Arch v2 复审 | Phase 3 ProjectManagerPolicy 反向 import 风险 | §10 OQ 表新增条目：约定单方法接口 `AutoChainOverride(ws) (enabled, window, cap, ok bool)`，避免 session 包 import projectmanager 多结构 |

### v3 终审

| Reviewer | Verdict | 备注 |
|---|---|---|
| **Go** | APPROVE-WITH-MINOR-CHANGES | New-B1 闭环 PASS；MEDIUM 并发测试 PARTIAL：测试名覆盖正确但需明确钩子接口，避免 sleep-based。**v3 §5.3.1 已落实测试钩子约定，闭环。** |
| **Architecture** | **APPROVE** | 3 MINOR + 1 OQ 全 PASS；无新增架构风险；可立即实施 |

**Status**: 设计阶段闭环。Implementation 可立即开始。

### v3.1 增量（Go 终审 minor 落地）

| ID | 处理 |
|---|---|
| Go-MINOR-test-hooks | §5.3.1 加测试钩子 testHookBeforeSpawnPhase3 / testHookBeforeBackfillPhase3，build-tag testhooks 隔离零生产开销，禁 sleep |

### v3.2 增量（PR diff 评审 BLOCKING / NIT 落地）

| ID | 来源 | 处理 |
|---|---|---|
| Diff-BLOCKING-1 | code-reviewer | §4.2 步骤 4 由"取前 cap-1（最早）"改为"取末尾 cap-1（最新）"。语义正名：dashboard 从最新往老翻，链上最早段离当前对话最远价值低，保留最新更符合用户意图。代码已实现这一语义，本次仅同步 RFC 文字与之一致 |
| Diff-BLOCKING-2 | code-reviewer | §5.2 缺 `TestBackfillConcurrentWithSpawn` 已补：testHookBeforeBackfillPhase3 暂停 backfill 中段，期间触发 spawn，断言两条路径不会重复占用同一 sessionID |
| Diff-NIT-1 | code-reviewer | §7 backfill 总览 slog `auto-chain backfill complete` 已实现：在 runAutoChainBackfillOnce 末尾输出 updated/skipped/duration_ms |
| Diff-NIT-2 | code-reviewer | §7 `AutoChainAttachLengthHist` / `AutoChainPickReturnZero` 不在本期范围（expvar 无原生 histogram；这两项需 prom 或类似基础设施，留 follow-up） |
| Diff-NIT-3 | code-reviewer | testHookBeforeBackfillPhase3 钩子位置无功能问题；确认现有测试在调用前已构造候选 |
| Diff-NIT-4 | code-reviewer | snapshotRouterExcludedLocked 的 r.mu→historyMu 顺序与全工程一致（race 测试已 36 包覆盖）；不增加额外 lock-order 测试，留作未来加固空间 |
