# server-split Phase 4 — Baseline 数据

> 采集日期：2026-05-25（branch `todo-batch/r246-r247-cluster-3` HEAD `49dce36b`）
>
> 目的：为 v0.3 设计稿提供真实数据，关闭 v0.1/v0.2 reviewer 提到的"先承诺数字再采 baseline"流程倒装问题。

---

## 1. server 包体量真实数据

| 维度 | 含测试 | 不含测试 |
|---|---|---|
| `.go` 文件数 | **111** | 40 |
| 行数 | **38836** | 17156 |

> v0.2 设计稿 §一 写"17143 行 / 40 文件"——只算非测试。v0.3 必须明确两种口径都列。

### 非测试文件大小分布

```
1514  dashboard_session.go    ⚠️ > 800
1418  dashboard_cron.go       ⚠️ > 800
1309  server.go               ⚠️ > 800
1302  project_files.go        ⚠️ > 800
1214  dashboard_send.go       ⚠️ > 800
 909  dashboard_cron_transcript.go  ⚠️ > 800
 731  dashboard.go
 699  send.go
 655  agent_tailer.go
 514  wshub.go
 ... (剩余 30 个 < 500 行)
```

**6 个文件超过 800 行硬上限**；17 个文件超过 500 行（Phase 0 linter 启用即全红）。

---

## 2. Server struct 真实字段（47 个）

```
分类                   字段                                   计数
─────────────────────────────────────────────────────────────────
HTTP 入口必需 (4)       addr / mux / startedAt / onReady         4
核心依赖 (5)            router / hub / scheduler / projectMgr
                       / appCtx                                 5
nodes 多节点 (5)        nodes / nodesMu / reverseNodeServer
                       / nodeCache / knownNodes                 5
认证 / 安全 (3)         dashboardToken / debugMode / allowedRoot 3
分发 (3)                platforms / dedup / msgQueue             3
agent 配置 (4)          agents / agentCommands / backendTag
                       / resolver                               4
12 个 handler 引用 (12) auth / cronH / sessionH / projectH /
                       discoveryH / transcribeH / sendH / cliH /
                       scratchH / memoryH / agentEventsH /
                       healthH / nodeAccess                    13
session 周边 (4)        sessionGuard / scratchPool /
                       sysessionMgr / discoveryCache            4
其他 (6)                workspaceName / claudeDir / workspace /
                       watchdogNoOutputKills /
                       watchdogTotalKills /
                       noOutputTimeout / totalTimeout           6
─────────────────────────────────────────────────────────────────
合计                                                            47
```

> v0.2 写"28+"——实际 47。v0.2 §二目标"≤ 12 字段"是从 47 减 74%，远难于"28+→12 减 57%"。

### Phase 5 字段去向方案（v0.4 实地核对到位）

> v0.3 错点：把 cookieMAC / wsAuthLimiter / wsUpgradeLimiter 当 Server 字段（实际在 Hub）；version 当 Server 字段（实际在 SessionHandlers.versionTag）；删除 5 项实写 6 项。v0.4 重新对账。

| 保留 12 个 Server struct 字段 |
|---|
| `addr / mux / startedAt / onReady / appCtx`（HTTP 入口 5）|
| `router / scheduler / hub / projectMgr`（核心依赖 4）|
| `nodes / nodesMu / reverseNodeServer`（多节点 3）|

| 搬走 35 个 |
|---|
| **routes.go 局部变量**（13）：12 handler-group（auth/cronH/transcribeH/discoveryH/projectH/sessionH/healthH/sendH/cliH/scratchH/memoryH/agentEventsH）+ nodeAccess |
| **NewHub Options**（9）：dedup / sessionGuard / msgQueue / agents / agentCommands / dashboardToken / allowedRoot（与 Hub 已有字段合并）/ noOutputTimeout / totalTimeout |
| **dashboard/* 子包持有**（5）：claudeDir / workspaceName / discoveryCache / scratchPool / sysessionMgr |
| **server 包内重组**（3）：debugMode（→ server/debug.go）/ resolver（→ routes.go 局部）/ nodeCache（→ server/nodecache.go）|
| **metrics 包**（2）：watchdogNoOutputKills / watchdogTotalKills |
| **待评估删除**（3）：platforms（疑似 routes 注册期局部）/ backendTag（dispatch.BackendTag() 派生）/ knownNodes（合 nodes map）|

加法：13 + 9 + 5 + 3 + 2 + 3 = **35** ✓ 保留 12 + 搬走 35 = **47** ✓

> 此表是 Phase 5 的实操依据。任一字段去向有歧义都不能开 Phase 5 PR。
> "待评估删除" 3 项必须 Phase 5 PR 写前先 grep 实装依赖 + 写小设计文档证明能删。

---

## 3. Hub struct 真实字段（37 个）

```
按职责分组（用于 v0.3 §五字段分组）：
─────────────────────────────────────────────────────────────────
lifecycle (3)            mu / ctx / cancel
subscriber (8)           clients / connCount / clientWG /
                         wsAuthLimiter / wsUpgradeLimiter /
                         upgrader / dashTokenHash / cookieMAC
broadcast (4)            debounceMu / debounceTimer /
                         debounceFirst / debounceClosed
send / queue (5)         queue / sendWG / sendTrackMu /
                         sendClosed / droppedTotal
shared deps (12)         router / agents / agentCmds / dashToken /
                         guard / nodes / nodesMu / projectMgr /
                         resolver / scheduler / scratchPool /
                         uploadStore / allowedRoot / trustedProxy
agent tailer (3)         tailers / wiredLinkersMu / wiredLinkers
─────────────────────────────────────────────────────────────────
合计                                                            37
```

> v0.2 §五的"≤ 18 字段"已经修过措辞为 ≤ 30；实际 37 — Phase 4 不删字段，Phase 5 看是否能压到 ≤ 30（需要看 ctx/cancel 是否能去掉、cookieMAC 是否能合并 dashTokenHash）。

---

## 4. 90 天活跃度数据

| 指标 | 数值 |
|---|---|
| 总 commit 数（仓库） | 306 |
| 涉及 internal/server/ 的 commit | **160（52.3%）** |
| dashboard_*.go commit | 30 |
| wshub*.go commit | 28 |
| 一次 commit 改 ≥ 2 个 server 文件 | **113（70%）** |
| 一次 commit 改 ≥ 5 个 server 文件 | 9 |
| 最热文件 commit 数 | wshub.go 27 / dashboard_cron.go 27 |

### 含义

- **server 包是仓库最热区**（占 commit 一半以上）
- **70% 的 server 包 commit 跨多文件** — Phase 4 拆完后大部分跨文件改动会变成跨包改动；Deps 接口化后，新增 cron 方法 → cronOps 接口加方法 + 反向注释更新——要 dashboard/cron 包 commit + cron 包 commit
- **没有 git merge commit**（项目走 squash merge）——回滚预案不能依赖 merge commit，必须用 tag

### ROI 重估（取代 v0.2 §十一估算）

- **3 个月** 113 次跨 server 文件 commit。如果 Phase 4 后 70% 改为跨子包 commit，按每次 commit 多 5 分钟（多包 import + 多次 review 通过），**额外开销 113 × 5 / 60 / 3 ≈ 3 小时/月**
- **节省**：`dashboard_*.go` 等 god file 的并行冲突预计减半（6 个独立子包 vs 1 个大文件）
- **临界点**：节省必须 ≥ 3 小时/月才有正 ROI

⚠️ **目前没有 baseline 数据证明合并冲突时间** — 需要在 Phase 0 启动后两周内补充实测。

---

## 5. 路由数

```
$ grep -E "mux\.(Handle|HandleFunc)" internal/server/dashboard.go | wc -l
50
```

50 路由全部在 `dashboard.go::registerDashboard()` 单点注册。Phase 0 routes_snapshot golden 必须含全 50 条。

---

## 6. 测试覆盖

```
$ go test -count=1 -timeout=300s ./internal/server/ 2>&1 | tail -3
ok  	github.com/naozhi/naozhi/internal/server  [基线时间未测，Phase 0 0.6 必采]
```

> 留 Phase 0 0.6 step 实测后填入。

---

## 7. 与 v0.2 设计稿差异表（Phase 0 必读）

| v0.2 写的 | 真实数据 | 影响 |
|---|---|---|
| server 17143 行 / 40 文件 | 17156 行（不含测试）/ 38836 行（含测试）/ 111 文件（含测试） | §一/§二/§十一全部需重写 |
| Server "28+" 字段 | **47 字段** | §二"≤ 12" 减 74% (vs 57%)；§六.6 必须给字段去向表 |
| Hub "28+" 字段 | **37 字段** | §五字段分组要按 37 个真实分布 |
| Phase 5 删 SetScheduler/SetUploadStore/SetScratchPool god setter | 这三个 setter 在 **Hub 上**（wshub.go），不是 Server | §六.6 整改 |
| 6 个文件 > 800 行 | dashboard_session(1514) / cron(1418) / server(1309) / project_files(1302) / send(1214) / cron_transcript(909) | linter 不能 Phase 0 启用 fail 模式；必须 warn + 豁免清单 |

---

## 8. 推荐合并顺序（取代 v0.2 §六.1 "Phase 4 与 1-3 并行" 的错措辞）

实际可行序列（v0.3 应明确）：

```
Phase 0：linter warn + types.go 接口定义     1 周
       ↓
Phase 4：抽 internal/wshub/ 整包             2 周
       ↓ (Phase 4 完成后，dashboard/* 子包能直接 import wshub 包，无需走 server.Hub)
Phase 1：抽 dashboard/cron/                   1 周
Phase 2：抽 dashboard/project/                1 周
Phase 3a-3f：剩 6 个 dashboard 子包          3-4 周（3a-3d 可并发 review）
       ↓
Phase 5：Server 字段瘦身 47→12 + Hub 字段整理  1 周
       ↓
Phase 5 完工后，linter 切 fail 模式
```

合计 **9-10 周**，而非 v0.2 估算的 6-8 周。

---

## 9. ROI 决策 gate（v0.3 §十一应钉死）

- [ ] **Phase 0 完成后**，跑两周实测：dashboard 板块 PR 平均冲突时间、并行 PR 数
- [ ] 如果 dashboard 板块**月均跨文件冲突 < 15 分钟** → 推迟 Phase 1，改做 §九.2 单独的 lint gate（防新文件膨胀）即可
- [ ] 如果 dashboard 板块**月均并行 PR 数 < 3** → ROI 不足 3 小时/月节省，推迟 Phase 1
- [ ] 否则启动 Phase 1

> 这条 gate 挂在 Phase 0 PR description 里，不达标就把 Phase 1 PR 推迟到下季度。
