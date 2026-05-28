# Phase 1-3f 实施 Playbook

> **状态**：实施前 playbook（2026-05-28）
> **配套**：`docs/design/server-split-phase4-design.md` v0.6.1 §6.3-6.4
> **前置**：Phase 4c 全部已 merged
> **范围**：抽 6 个 dashboard 子包到 `internal/dashboard/`

## 0.0 Pre-flight checklist（每 phase 必跑）

任何 Phase 1-3f 实施前必跑——避免 master 持续涨导致 baseline 漂移：

```bash
# 1. 同步 master + 切新分支
git fetch origin master
git checkout -B server-split/phase<N> origin/master

# 2. 实测受影响文件最新行数，更新 exemptions.yaml current 字段
for f in internal/server/dashboard_<domain>*.go; do
  echo "$f: $(wc -l < $f) lines"
done

# 3. baseline build/test 必须绿
go build ./... && go test -race -count=1 ./internal/server/...

# 4. 实测搬迁前的 lint 噪音 baseline
go run ./tools/lint-server-handlers/ 2>&1 | tail -2

# 5. 记录搬迁前的 race test 时间（验收 gate 用）
time go test -race -count=1 -timeout=300s ./internal/server/...
```

**关键**：master 涨速 4 个月内 ~20% 是常态。设计稿 v0.6.1 §6.1 行数估值
是 v0.6.1 当时实测，但 master 仍在涨——**以实施前实测为准**。

---

## 0.1 Phase 1-3f 总览

按 v0.6.1 §6.1 phase 表，6 个 dashboard 子包按业务域抽出：

| Phase | 子包 | 范围 | 预估行数（不含测试）| 风险 |
|---|---|---|---|---|
| 1 | dashboard/cron | dashboard_cron + cron_transcript + runs + update | **3382**（v0.6.1 实测 2026-05-28；v0.6.1 设计稿写 2810 是 4 个月前的旧值）| 中 |
| 2 | dashboard/project | project_api + project_files | 2091（实测；设计稿 1830）| 中 |
| 3a | dashboard/auth | dashboard_auth + csrf | ~600 | 低 |
| 3b | dashboard/discovery | dashboard_discovered + takeover | ~450 | 低 |
| 3c | dashboard/ext | scratch + memory | ~700 | 低 |
| 3d | dashboard/ext | agent_events + cli + transcribe + system | ~700 | 低 |
| 3e | dashboard/session | list + events + interrupt + label | ~1000 | 中 |
| 3f | dashboard/session | send + upload + attachment + agent_tailer + upload_store | ~1500 | **高**（含 send 路径）|

**重要**：每个 phase 实施前必跑 §0 pre-flight checklist 实测最新行数；
master 涨速 4 个月内 ~20% 是常态。设计稿 v0.6.1 §6.1 行数估值是 v0.6.1
当时实测，但 master 仍在涨——以实施前实测为准。

**核心原则**（v0.6.1 §四.3 不搬业务逻辑、只搬位置）：
- 每个 phase PR 必须满足"机械搬迁"（git diff 中实质代码变化 ≤ 5 行/handler）
- 顺便修 bug / 重命名内部函数 → **必须另起 PR**

## 1. 通用搬迁模板（任何 dashboard 子包都适用）

### 1.1 包初始化

```go
// internal/dashboard/<domain>/handlers.go
package dashboard<domain>

import (
    "context"
    "github.com/naozhi/naozhi/internal/cron"
    "github.com/naozhi/naozhi/internal/session"
    // ...
)

// Handlers groups all dashboard/<domain> HTTP handlers.
type Handlers struct {
    deps Deps
}

// New creates a new Handlers with the given Deps.
func New(deps Deps) *Handlers {
    return &Handlers{deps: deps}
}
```

### 1.2 Deps 双轨规则（v0.6.1 §四.1.1）

```go
// internal/dashboard/<domain>/deps.go
package dashboard<domain>

// 白名单 5 个直接持具体类型（高频、方法多、跨子包稳定）
type Deps struct {
    Router      *session.Router       // 高频，方法多，直接持
    Scheduler   *cron.Scheduler        // 高频，多子包共享
    ProjectMgr  *project.Manager       // 高频
    Resolver    *session.KeyResolver   // 多子包依赖
    Hub         broadcaster            // 低频窄面，本地接口
    AllowedRoot string
}

// 黑名单本地接口化（子包私有窄面 ≤ 3 方法）
type broadcaster interface {
    BroadcastSessionsUpdate()
}
```

### 1.3 路由注册位置

不在子包注册路由——路由仍在 `internal/server/dashboard.go::registerDashboard()`
集中：

```go
// internal/server/dashboard.go (master)
func (s *Server) registerDashboard(...) {
    // Phase 1+ 后：
    cronH := dashboardcron.New(dashboardcron.Deps{
        Router: s.router,
        Scheduler: s.scheduler,
        // ...
    })
    s.mux.HandleFunc("GET /api/cron/list", auth(cronH.HandleList))
    // ...
}
```

### 1.4 测试搬迁

每个 dashboard_<feature>_test.go 跟着 handler 搬到子包。如果 test 调用
server 包私有 helper，搬过去时把 helper 也复制（不再 import server 包）。

## 2. Phase 1: dashboard/cron 详细步骤

**前置**：master 含 Phase 4c。

**搬迁文件**：
- `dashboard_cron.go`（1427 行）
- `dashboard_cron_transcript.go`（1424 行）
- `dashboard_cron_runs.go`（202 行）
- `dashboard_cron_update.go`（208 行）
- `cronview_contract_test.go`（51 行）
- 关联的 `dashboard_cron_*_test.go` 文件（~10 个）

**步骤**：

### Step 1: 创建子包结构

```bash
mkdir -p internal/dashboard/cron
# 写 deps.go / handlers.go / 子包声明
```

### Step 2: 双 commit 之 commit a（机械搬迁）

按 v0.6.1 §6.3 双 commit 策略：

```bash
# 移代码 + 改 receiver + 更新 routes golden
git add internal/dashboard/cron/...
git rm internal/server/dashboard_cron.go
git rm internal/server/dashboard_cron_transcript.go
# ...
UPDATE_GOLDEN=1 go test -run TestRoutesSnapshot ./internal/server/...
git add internal/server/testdata/routes.golden.json
git commit -m "phase 1: extract dashboard/cron (mechanical move + golden)"

# 验证 commit a 独立绿
git stash; go build ./...; go test -race ./...; git stash pop
```

### Step 3: 双 commit 之 commit b（godoc + gofmt）

```bash
goimports -w internal/dashboard/cron/...
gofmt -w internal/dashboard/cron/...
git add internal/dashboard/cron/
git commit -m "phase 1: dashboard/cron godoc + gofmt polish"
```

### Step 4: PR description 必须含

```
跨包字段引用减少数：
  Before: grep -c "*cron\.Scheduler" internal/server/dashboard_cron.go = X
  After:  同 grep on internal/dashboard/cron/handlers.go = Y
  Reduction: X - Y >= 5

Closes-exemption: internal/server/dashboard_cron.go
Closes-exemption: internal/server/dashboard_cron_transcript.go
```

### Step 5: 验收 gate

- [ ] routes_snapshot 通过（HandlerType 从 `*CronHandlers` → `*dashboardcron.Handlers`）
- [ ] go test -race -count=2 ./internal/dashboard/cron/...
- [ ] exemptions.yaml 中 `dashboard_cron.go (until_phase: 1)` 已删除
- [ ] exemptions.yaml 中 `dashboard_cron_transcript.go (until_phase: 1)` 已删除
- [ ] PR description 含 Closes-exemption trailer × 2
- [ ] 双 commit 中每个 commit 独立 build/test 绿

## 3. Phase 2: dashboard/project（同 Phase 1 模板）

**搬迁**：
- `project_api.go` + `project_files.go` + `project_files_open_*.go`
- 关联测试文件

**Closes-exemption**：
- `internal/server/project_files.go` (until_phase: 2)

## 4. Phase 3a-3d: 4 个独立子包（可并行 review）

按 v0.6.1 §7.3，3a/3b/3c/3d 互不依赖，可**并发 review**（merge 仍按
顺序——v0.6.1 §6.4）。

### 3a: dashboard/auth
- `dashboard_auth.go` + csrf 相关
- Closes-exemption: `dashboard_auth.go`

### 3b: dashboard/discovery
- `dashboard_discovered.go` + takeover.go
- 无 exemption（这两个文件原本不超线）

### 3c: dashboard/ext (scratch + memory)
- `dashboard_scratch.go` + `dashboard_memory.go`
- 创建 `internal/dashboard/ext/` 子包入口（3d 复用）

### 3d: dashboard/ext (agent_events + cli + transcribe + system)
- 4 个 `dashboard_<x>.go` 文件
- 复用 3c 创建的 ext 子包入口

## 5. Phase 3e: dashboard/session（list + events + interrupt + label）

**搬迁**：
- `dashboard_session.go` 中 4 类 handlers（list / events / interrupt / label）
- 不含 send 路径（留 Phase 3f）

**Closes-exemption**：
- `dashboard_session.go` (until_phase: 3e)

**风险**：dashboard_session.go 1714 行——是最大文件。机械搬迁后会有大量
import / 类型引用调整。**必须双 commit**。

## 6. Phase 3f: dashboard/session（send + upload + attachment + agent_tailer + upload_store）

**搬迁**：
- `dashboard_send.go` + `send.go` + `upload_store.go` + `agent_tailer*.go`（如果还在 server 包）
- 关联测试文件

**Closes-exemption**：
- `dashboard_send.go` (until_phase: 3f)
- `send.go` (until_phase: 3f)

**风险**：含 send 路径——v0.6.1 §6.5 标"高风险"。**必须**：
- 双 commit
- race -count=100 测试
- 4 小时冻结窗口（v0.6.1 §8.3）
- 凌晨低峰窗口发布

## 7. v0.6.1 §7.3 观察期重叠豁免（节奏关键）

| 当前 Phase | 下一 Phase | 重叠规则 |
|---|---|---|
| Phase 1 → Phase 2 | 重叠 4 天 |
| Phase 2 → Phase 3a | 重叠 4 天 |
| Phase 3a/3b/3c/3d | 完全并行 |
| Phase 3d → 3e | 不可重叠（等 3a-3d 全 merge + 3 天） |
| Phase 3e → 3f | 不可重叠（7 天独立） |
| Phase 3f → 5 | 不可重叠（7 天独立） |

**总观察期**（不含重叠）：8 个 phase × 7 = 56 天
**实际观察期**（含重叠）：56 - 12 = ~44 天 ≈ 6.3 周

## 8. PR description 通用模板

```markdown
## Phase N: 抽 dashboard/<domain>

按 server-split-phase4-design v0.6.1 §6.X / playbook §X 实施。

## 改动

- 新建 internal/dashboard/<domain>/ 子包（handlers + deps）
- 删 internal/server/<files> (X 行)
- routes.golden.json 同步：`HandlerType` 从 `*<X>Handlers` → `*dashboard<x>.Handlers`

## 跨包字段引用减少（v0.6.1 §四.2 KPI）

```
Before: grep -c "*cron.Scheduler" internal/server/dashboard_cron.go = X
After:  同 grep on internal/dashboard/cron/handlers.go = Y
Reduction: X - Y >= 5  ✓
```

## 验收

- [x] routes_snapshot 通过
- [x] go test -race -count=2 ./internal/dashboard/<domain>/...
- [x] linter rules 1-5 0 violation
- [x] 双 commit 各自 build/test 绿

Closes-exemption: internal/server/dashboard_<file>.go
```

## 9. 完成后

Phase 3f merge + 7 天观察期 → Phase 5 启动（Server 字段 47→12）。
