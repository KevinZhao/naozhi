# Phase 5 实施 Playbook

> **状态**：实施前 playbook（2026-05-28）
> **配套**：`docs/design/server-split-phase4-design.md` v0.6.1 §6.6 / §6.7
> **前置**：Phase 3f 全部已 merged
> **范围**：Server 字段瘦身 47→12 + Hub 字段验收 ≤40 + 删 god setter +
> scratchPool 三角钉死 + lock-profile 入档

## 0. Phase 5 是收尾刀

Phase 5 是 server-split 的最后一刀，行数最少（v0.6.1 §6.1 估
-300 / +400），但**验收 gate 最严**：

- Server 字段必须 ≤ 12
- Hub 字段必须 ≤ 40
- exemptions.yaml 必须**空**
- mutex pprof 数据入档
- linter rule 3a/3b/4/5 切 fail mode
- scratchPool sweeper 自管 + main ctx 注入

## 1. 前置依赖

```bash
# 1. master 含 Phase 3f
git log --oneline origin/master | grep "Phase 3f" | head -1

# 2. exemptions.yaml 仅剩 server.go 一条（其他 Phase 1-3f 都已 Closes-exemption）
yq '.file_size[].path' tools/lint-server-handlers/exemptions.yaml

# 3. 全仓 race test 全绿
go test -race -count=1 ./...
```

## 2. 47 个字段的搬迁（按 v0.6.1 §6.6）

### 保留 12 个 Server struct 字段

```
HTTP 入口   addr / mux / startedAt / onReady / appCtx
核心依赖    router / scheduler / hub / projectMgr
多节点      nodes / nodesMu / reverseNodeServer
```

### 搬走 35 个

| 去向 | 个数 | 字段清单 |
|---|---|---|
| `routes.go` 局部变量 | 13 | 12 handler-group + nodeAccess |
| `NewHub` Options 注入 | **10** | dedup / sessionGuard / msgQueue / agents / agentCommands / dashboardToken / allowedRoot / noOutputTimeout / totalTimeout / **scratchPool** |
| `dashboard/*` 子包持有 | 4 | claudeDir / workspaceName / discoveryCache / sysessionMgr |
| server 包内重组 | 3 | debugMode → server/debug.go / resolver → routes.go / nodeCache → server/nodecache.go |
| `metrics` 包 | 2 | watchdogNoOutputKills / watchdogTotalKills |
| 待评估删除（写前 grep + 小设计文档） | 3 | platforms / backendTag / knownNodes |

加法：13 + 10 + 4 + 3 + 2 + 3 = 35 ✓

## 3. 实施 Step

### Step 1: scratchPool 三角钉死（v0.6.1 §6.7）

按 §6.7 Phase 5 必做的 4 件事：

1. **`*session.ScratchPool` 自管 sweeper**
   ```go
   // internal/session/scratchpool.go
   func NewScratchPool(ctx context.Context, ...) *ScratchPool {
       p := &ScratchPool{...}
       go p.runSweeper(ctx)  // 自管
       return p
   }
   ```

2. **Server 不再持 scratchPool 字段**
   ```go
   // server.go
   // - scratchPool *session.ScratchPool  ← 删
   //
   // routes.go (Phase 5 后)
   pool := session.NewScratchPool(mainCtx, ...)
   hub := wshub.NewHub(wshub.HubOptions{ScratchPool: pool, ...})
   ```

3. **删 server.go:1080 显式 Stop pool**

4. **NewScratchPool 接 main ctx**（不是 Hub.ctx）

**验证**：
```bash
grep -n "scratchPool" internal/server/server.go              # 必须 == 0
grep -n "SetScratchPool" internal/wshub/*.go                 # 必须 == 0
grep -n "StartSweeper" internal/{server,session}/*.go        # 必须 == 0
grep -n "NewScratchPool(ctx" internal/session/*.go           # 必须 ≥ 1
```

### Step 2: 13 个 handler-group 字段搬到 routes.go 局部变量

```go
// internal/server/routes.go (Phase 5)
func (s *Server) registerRoutes(...) {
    // handler-group 在构造期创建，不再挂在 Server struct
    auth := newAuthHandlers(...)
    cronH := dashboardcron.New(dashboardcron.Deps{...})
    sessionH := dashboardsession.New(...)
    // ...

    s.mux.HandleFunc("GET /api/sessions", auth(sessionH.HandleList))
    // ...
    // handler-group 局部变量出 registerRoutes 后即丢弃
}
```

### Step 3: 9 + 1 个字段搬到 NewHub Options

```go
// internal/wshub/types.go (Phase 5 扩 HubOptions)
type HubOptions struct {
    // 已有：Router / Agents / AgentCmds / ...
    // Phase 5 新增（从 Server 搬）：
    Dedup            *platform.Dedup
    SessionGuard     *session.Guard
    MsgQueue         MessageEnqueuer  // 已存在
    DashboardToken   string
    AllowedRoot      string
    NoOutputTimeout  time.Duration
    TotalTimeout     time.Duration
    ScratchPool      *session.ScratchPool  // §6.7 钉死
}
```

### Step 4: 4 个字段搬到 dashboard 子包

每个子包加构造期字段：

```go
// internal/dashboard/discovery/handlers.go (Phase 5)
type Handlers struct {
    deps Deps
    claudeDir       string  // 从 Server 搬
    discoveryCache  *discoveryCache
}
```

### Step 5: 3 个字段重组到 server 包内独立文件

- `debugMode` → `server/debug.go`（已部分存在）
- `resolver` → `routes.go` 局部变量
- `nodeCache` → `server/nodecache.go`（已部分存在）

### Step 6: 2 个字段搬到 metrics 包

```go
// internal/metrics/metrics.go (Phase 5)
var (
    WatchdogNoOutputKills atomic.Int64
    WatchdogTotalKills    atomic.Int64
)
```

调用方从 `s.watchdogNoOutputKills.Add(1)` → `metrics.WatchdogNoOutputKills.Add(1)`。

### Step 7: 3 个待评估删除字段

按 v0.6.1 §6.6 N7 整改，每个字段 PR 写前必须：
1. `grep -rn "<field>" internal/` 看实装依赖
2. 写小设计文档 `docs/ops/server-field-removal-<field>.md`
3. 在 Phase 5 PR description 引用文档

字段：
- `platforms` ← grep 后看是否仅 routes 注册期用
- `backendTag` ← 与 `dispatch.BackendTag()` 派生路径合并
- `knownNodes` ← 与 `nodes` map 合并

### Step 8: 删 god setter

按 v0.6.1 §6.6 删 setter：
```bash
grep -E "func \(h \*Hub\) Set" internal/wshub/*.go | wc -l
# 必须 == 0
```

如果 4b/4c 已经搬到 wshub 但仍有 SetScheduler / SetUploadStore /
SetScratchPool 等 setter，本 Phase 删除——改 NewHub Options 一次性注入。

### Step 9: linter mode 切换

按 v0.6.1 §6.2.0.4 阶段化交付表：

```
| Rule 1 handle_decl       | mode=fail |
| Rule 2 file_size         | exemptions 必须空 |
| Rule 3a field_block 骨架 | mode=fail |
| Rule 3b field_block AST  | mode=fail |
| Rule 4 iface_match       | mode=fail |
| Rule 5 stale_exemption   | mode=fail |
```

Makefile：
```makefile
lint-server:
    go run ./tools/lint-server-handlers -mode fail   # ← Phase 5 切 fail
```

### Step 10: mutex profile 入档（v0.6.1 §十二.2）

```bash
# Phase 5 PR 必须含 mutex profile baseline
curl -s 'http://localhost:8080/api/debug/pprof/mutex?seconds=30' > mutex.pb.gz
go tool pprof -top -cum mutex.pb.gz | head -10 > docs/ops/lock-profile-2026-XX.md

# Phase 5 PR description 必须含
# - mutex profile attached: docs/ops/lock-profile-2026-XX.md
# - top contended locks 列表
# - Hub.mu 是否进 top 3 / 4-10 / 不入 top 10 的判定
```

按结果决定 Phase 6（锁分离）触发：
- top 3 → 立即开 Phase 6 RFC
- top 4-10 → 30 天内开 Phase 6 RFC
- 不入 top 10 → Phase 6 推迟，每 90 天复查

### Step 11: lint-fact-table 切 fail

```yaml
# .github/workflows/ci.yml (Phase 5)
lint-fact-table:
    continue-on-error: false   # ← Phase 5 切 fail
```

工具的 v2 fine-tune（消除 7 个 false positive）必须在 Phase 5 之前完成。

## 4. v0.6.1 §九.1 Phase 5 验收 gate

```
- [ ] wc -l internal/server/*.go | tail -1 ≤ 5000
- [ ] Server 字段数 ≤ 12（脚本验证）
- [ ] Hub 字段数 ≤ 40 且按 §五字段块（7 块）分组组织
- [ ] grep -r "*session.Router" internal/dashboard/ 返回 0（子包不应直接持 *Router）
- [ ] mutex profile 入档 docs/ops/lock-profile-2026-XX.md
- [ ] scratchPool 三角验证脚本通过（详 §6.7）
- [ ] exemptions.yaml file_size 段必须空
- [ ] lint-server / lint-fact-table 全切 fail mode
```

## 5. Phase 5 → final 14 天独立观察期（v0.6.1 §7.3）

Phase 5 完工后**双倍观察期 14 天**——mutex pprof 数据采集需稳定流量。

期间监控指标：
- /health 200 率 ≥ 99.99%
- WebSocket 连接错误率 ≤ baseline + 0.1%
- dashboard 页加载 P95 ≤ baseline × 1.1
- mutex profile top 3 锁列表稳定（不应每 90 天换一批）

## 6. 完成后宣告

Phase 5 → final 14 天观察期通过后，server-split-phase4 全部完成。

```bash
git tag server-split-final
git push origin server-split-final
```

撰写 retrospective：
- `docs/ops/server-split-retrospective.md`
- 实际工时 vs §十一 估算 65-150 人天
- 实际节奏 vs 13-15 周
- 实际收益（dashboard 月均冲突时间下降）vs ROI 预估
- v2 lint-fact-table fine-tune 落地
- Phase 6 / Phase 7 触发判定
