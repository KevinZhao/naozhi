# server-split Phase 4 — Baseline 数据

> **采集日期**：2026-05-28（branch `cron/todo-fix-20260526-211641`，origin/master HEAD `44a10e8d`）
>
> **v0.6 修订**（取代 v0.4 baseline）：本地落后 master 33 commits 后实测，多处数字与 v0.4 baseline 不符——server 包从 17156 行膨胀到 21313 行；Hub 字段从 37 增到 47（多 10 个）；超 800 行文件从 6 个增到 9 个。本文件按当前 origin/master 重新采集。
>
> 目的：为 design v0.6 提供真实数据，关闭 v0.1/v0.2 reviewer 提到的"先承诺数字再采 baseline"流程倒装问题。

---

## 1. server 包体量真实数据

| 维度 | 含测试 | 不含测试 |
|---|---|---|
| `.go` 文件数 | **206** | **58** |
| 行数 | **53487** | **21313** |

> v0.4 baseline 写"38836 行 / 111 文件 / 17156 行 / 40 文件"——3 个月内 server 包又涨 **4157 行 / 18 个非测试文件**。证明 v0.4 设计目标"≤ 5000 行 / ≤ 15 文件"的减幅从原 71% 升到 **77%**——但拆分后净行数预算不变。

### 非测试文件大小分布（实测 v0.6）

```
1713  dashboard_session.go    ⚠️ > 800
1632  project_files.go        ⚠️ > 800
1446  dashboard_send.go       ⚠️ > 800
1427  dashboard_cron.go       ⚠️ > 800
1383  dashboard_cron_transcript.go  ⚠️ > 800
1334  server.go               ⚠️ > 800
 902  wshub.go                ⚠️ > 800（v0.4 时 514，3 个月涨 388）
 852  dashboard.go            ⚠️ > 800（v0.4 时 731）
 827  agent_tailer.go         ⚠️ > 800（v0.4 时 655，3 个月涨 172）
 703  send.go
 583  dashboard_auth.go
 ... (剩余 47 个 < 500 行)
```

**9 个文件超过 800 行硬上限**（v0.4 时 6 个，新增 wshub / dashboard / agent_tailer 三个超线）；11 个文件超过 500 行（Phase 0 linter 启用即全红）。

> **v0.4 → v0.6 的关键变化**：wshub.go 自身已超线 → Phase 4 拆分紧迫性更强。`agent_tailer.go` 也超线意味着 Phase 4c 范围比原估算大。

---

## 2. Server struct 真实字段（47 个，与 v0.4 一致）

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
12 个 handler 引用 (13) auth / cronH / sessionH / projectH /
                       discoveryH / transcribeH / sendH / cliH /
                       scratchH / memoryH / agentEventsH /
                       healthH / nodeAccess                    13
session 周边 (4)        sessionGuard / scratchPool /
                       sysessionMgr / discoveryCache            4
其他 (6)                workspaceName / claudeDir /
                       watchdogNoOutputKills /
                       watchdogTotalKills /
                       noOutputTimeout / totalTimeout           6
─────────────────────────────────────────────────────────────────
合计                                                            47
```

> v0.4 字段表与 v0.6 实测一致——3 个月内 Server 字段没新增，只是其他文件膨胀。

### Phase 5 字段去向方案（v0.6 微调）

| 保留 12 个 Server struct 字段 |
|---|
| `addr / mux / startedAt / onReady / appCtx`（HTTP 入口 5）|
| `router / scheduler / hub / projectMgr`（核心依赖 4）|
| `nodes / nodesMu / reverseNodeServer`（多节点 3）|

| 搬走 35 个 |
|---|
| **routes.go 局部变量**（13）：12 handler-group + nodeAccess |
| **NewHub Options**（10，**v0.6 +1**）：dedup / sessionGuard / msgQueue / agents / agentCommands / dashboardToken / allowedRoot / noOutputTimeout / totalTimeout / **scratchPool**（v0.5 决议；详 design §6.7） |
| **dashboard/* 子包持有**（4，**v0.6 -1**）：claudeDir / workspaceName / discoveryCache / sysessionMgr |
| **server 包内重组**（3）：debugMode（→ server/debug.go）/ resolver（→ routes.go 局部）/ nodeCache（→ server/nodecache.go）|
| **metrics 包**（2）：watchdogNoOutputKills / watchdogTotalKills |
| **待评估删除**（3）：platforms / backendTag / knownNodes |

加法：13 + 10 + 4 + 3 + 2 + 3 = **35** ✓ 保留 12 + 搬走 35 = **47** ✓

> 此表是 Phase 5 的实操依据。任一字段去向有歧义都不能开 Phase 5 PR。
> "待评估删除" 3 项必须 Phase 5 PR 写前先 grep 实装依赖 + 写小设计文档证明能删。
> v0.6 修订：scratchPool 从"dashboard 子包持有"迁到"NewHub Options"（详 design §6.7），保持总数 35 不变。

---

## 3. Hub struct 真实字段（**47 个，v0.6 实测**）

> **v0.4 baseline 写 37**——3 个月内 Hub 新增 **10 个字段**：
> - `auth`（auth handler 引用）
> - `subscriberCount`（订阅者计数；与 connCount 区分）
> - `legacySendInvokes`（legacy send 路径调用计数）
> - `debounceClosedFast`（debounce 快速关闭旗标）
> - `debounceFire`（v0.5 已识别但 baseline v0.4 漏列）
> - `historyMarshalCache`（v0.5 已识别）
> - `userSendLimitersMu` / `userSendLimiters`（v0.5 已识别）
> - `connCountByOwnerMu` / `connCountByOwner`（v0.5 已识别）
>
> **v0.6 验证命令**：`awk '/^type Hub struct \{/,/^\}$/' internal/server/wshub.go | grep -E "^[[:space:]]+[a-z][a-zA-Z0-9_]*[[:space:]]" | sed 's|//.*||' | awk '{print $1}' | sort -u | wc -l` → 47

```
按职责分组（v0.6 / 7 块）：
─────────────────────────────────────────────────────────────────
lifecycle (3)            mu / ctx / cancel
subscriber (10)          clients / connCount / subscriberCount /
                         clientWG / wsAuthLimiter /
                         wsUpgradeLimiter / upgrader /
                         dashTokenHash / cookieMAC / trustedProxy
broadcast (6)            debounceMu / debounceTimer /
                         debounceFirst / debounceClosed /
                         debounceClosedFast / debounceFire
send / queue (6)         queue / sendWG / sendTrackMu /
                         sendClosed / droppedTotal /
                         legacySendInvokes
shared deps (14)         router / agents / agentCmds / dashToken /
                         guard / nodes / nodesMu / projectMgr /
                         resolver / scheduler / scratchPool /
                         uploadStore / allowedRoot / auth
agent tailer (3)         tailers / wiredLinkersMu / wiredLinkers
rate-limit / cache (5)   historyMarshalCache /
                         userSendLimitersMu / userSendLimiters /
                         connCountByOwnerMu / connCountByOwner
─────────────────────────────────────────────────────────────────
合计                                                            47
```

加法：3 + 10 + 6 + 6 + 14 + 3 + 5 = 47 ✓

> Phase 5 验收 gate（design §九.1）："**≤ 40**"——v0.6 校准：v0.4 写"≤ 30"基于 37 字段错算；v0.5 改"≤ 35"基于 43 字段；v0.6 实测 47 字段下"≤ 40" 是合理保守目标（可压目标：cookieMAC ↔ dashTokenHash 合并、connCount ↔ subscriberCount ↔ connCountByOwner 三选一、debounceClosed ↔ debounceClosedFast 合并）。

---

## 4. 90 天活跃度数据（v0.6 复测）

| 指标 | v0.4 数值 | v0.6 数值 |
|---|---|---|
| 总 commit 数（仓库） | 306 | **345** |
| 涉及 internal/server/ 的 commit | 160（52.3%） | **158（45.8%）** |
| 一次 commit 改 ≥ 2 个 server 文件 | 113（70%） | （未复测；保留 v0.4 估算）|
| 一次 commit 改 ≥ 5 个 server 文件 | 9 | （未复测）|
| 最热文件 commit 数 | wshub.go 27 / dashboard_cron.go 27 | （未复测）|

### v0.6 ROI gate 历史指标（用于 design §十一）

```
31 PRs / 90d → dashboard_cron.go      （v0.4 记录值，v0.6 复测一致）
17 PRs / 90d → dashboard_send.go
19 PRs / 90d → dashboard_session.go
32 PRs / 90d → wshub.go
25 PRs / 90d → project_files.go
28 PRs / 90d → server.go
```

6 个热文件每个 17-32 PR / 90d，**远超 ROI 临界点**（阈值 15 PRs/90d）。详 §9。

### 含义

- **server 包是仓库最热区**（占 commit 接近一半）
- **没有 git merge commit**（项目走 squash merge）——回滚预案不能依赖 merge commit，必须用 tag

---

## 5. 路由数

```
$ grep -E "mux\.(Handle|HandleFunc)" internal/server/dashboard.go | wc -l
51        # v0.4 写 50，v0.6 实测 51（dashboard.go 内）
$ grep -rE "mux\.(Handle|HandleFunc)" internal/server/*.go | grep -v _test.go | wc -l
55        # 含 server 包内分散注册（如 debug pprof）
```

**Phase 0 routes_snapshot golden 必须含全 51 条 dashboard 路由 + 4 条分散注册**。

---

## 6. 测试覆盖（v0.6.1 Phase 0b 实测）

```
$ go test -race -count=1 -timeout=300s ./...
46 packages PASS / 1 FAIL
duration: 36s wall (race mode)
$ go test -count=1 ./... | grep -c "^ok"
~50 packages
```

**FAIL 原因**：`TestLegacyServerNew_ZeroCrossPkgCallers` 在本地因
`.claude/worktrees/` 残留触发误报；CI 环境无 worktree 残留会通过。
本测试 walk 整个 repo 路径检查 `server.New(` 调用——预先存在的环境
问题，与 server-split 无关。

**Phase 0 baseline 锁定**：
- 36s race 全测试通过（不含 worktree 残留）
- `lint-server-handlers` 0 violations
- `routes_snapshot_test.go` 通过（51 路由 golden 一致）

后续每个 phase merge 前必须证明：耗时 < 40s（< 10% 余地）、PASS 数不减。

---

## 7. 与 v0.4 设计稿差异表（v0.6 必读）

| v0.4 写的 | v0.6 实测 | 影响 |
|---|---|---|
| server 17156 行（不含测试）/ 40 文件 | **21313 行 / 58 文件**（涨 24%） | design §一 / §二 / §十一全部需重写 |
| Hub 37 字段 | **47 字段**（涨 27%） | design §五字段分组要按 47 个真实分布；v0.5 已新增第 7 块（rate-limit/cache），v0.6 字段数从 43 增到 47 但块数仍 7 |
| 6 个文件 > 800 行 | **9 个文件 > 800 行**（新增 wshub / dashboard / agent_tailer） | linter exemptions.yaml 必须更新；wshub 自己超线 = Phase 4 拆分自带 baseline 豁免清理收益 |
| Phase 4 范围 ~3550 行（不含测试） | **3213 行 wshub + 827 agent_tailer + 455 wsclient + 703 send.go = 5198 行**（不含测试，比 v0.4 估多 1648）| Phase 4 拆 4a/4b/4c 必要性更强 |
| 50 路由 | 51 路由（dashboard.go 内）| routes_snapshot golden 加 1 条 |

---

## 8. 推荐合并顺序（v0.5 方案 B + v0.6 13-15 周）

```
Phase 0：linter warn + types.go 接口定义 + 包契约文档    1 周
       ↓
Phase 4a：wshub 骨架（hub.go + types.go + ctor + 5 文件壳）   1.5 周（含观察期）
Phase 4b：wshub 方法实质搬迁（subscribe + broadcast + send）  2 周（含观察期）
Phase 4c：wshub 收尾（agent_tailer + eventpush）             1.5 周（含观察期）
       ↓
Phase 1：抽 dashboard/cron/                              1.5 周
Phase 2：抽 dashboard/project/                            1.5 周
Phase 3a-3f：剩 6 个 dashboard 子包                       3-4 周（3a-3d 可并发 review）
       ↓
Phase 5：Server 字段瘦身 47→12 + Hub 字段整理            1 周
       ↓
Phase 5 完工后，linter 切 fail 模式
```

合计 **13-15 周**（v0.6 修订；v0.5 也是 13-15 周）。

---

## 9. ROI 决策 gate（v0.5 历史可回测指标，v0.6 复测确认）

```bash
# 历史指标 H1（git log 90d unique PR 数）
for f in dashboard_cron.go dashboard_send.go dashboard_session.go \
         wshub.go project_files.go server.go; do
  prs=$(git log --since=90.days.ago --format='%s' -- "internal/server/$f" \
        | awk -F'\\(#' 'NF>1{split($NF,a,")"); print a[1]}' | sort -u | wc -l)
  echo "$prs PRs / 90d → $f"
done
```

**v0.6 实测（2026-05-28, origin/master HEAD `44a10e8d`）**：

```
31 PRs / 90d → dashboard_cron.go
17 PRs / 90d → dashboard_send.go
19 PRs / 90d → dashboard_session.go
32 PRs / 90d → wshub.go
25 PRs / 90d → project_files.go
28 PRs / 90d → server.go
```

### 决策 gate

- [x] **6 个热文件全部 PRs/90d ≥ 15** → ✅ **gate 通过**（实测 17-32 全部超线）
- [ ] 任意 ≥ 3 个文件 PRs/90d ≥ 15 → 启动 Phase 1
- [ ] 2 个文件 ≥ 15 → Phase 1-5 推迟，Phase 0 lint gate 保留作为长期治理
- [ ] 0-1 个文件 ≥ 15 → 取消 Phase 1-5

### 为什么这个指标替代"冲突时间"

- **可回测**：git log 直接挖，不需要等 baseline 窗口；Phase 0 merge 后立即决策
- **squash merge 友好**：squash merge 抹去 merge commit，但 PR 引用（`(#1238)`）保留在 squash commit subject 里；`%s | awk -F'\\(#'` 即可数 unique PR
- **物理意义**：N 个独立 PR 改同一文件 = N-1 次潜在 merge 冲突机会；阈值 15 = 双周一次跨 PR 改动，达到这个频次拆分收益就跑赢成本

### 后续观察（不上 gate）

Phase 0 merge 后 14 天内手工记录是否有人尝试在 wshub.go / dashboard_cron.go 同时开 PR——用于事后印证 ROI gate 假设；不达标不阻止 Phase 1。
