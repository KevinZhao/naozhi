# Phase 4b 实施 Playbook

> **状态**：实施前 playbook（2026-05-28）
> **配套**：`docs/design/server-split-phase4-design.md` v0.6.1 §6.5
> **前置**：Phase 0-RFC / 0a / 0b / 0-LFT / 4a 全部已 merged
> **范围**：把 `internal/server/wshub_*.go` 真实方法搬到 `internal/wshub/`

## 0. 为什么要 playbook

Phase 4b 是 v0.6.1 §6.5 钉死的"风险最高一刀"——含 send 路径与 broadcast
协调。单次实施 ~2000 行不含测试 / ~2700 含测试，远超 §6.0 的 1500 行/PR
阈值。**v0.6.1 §6.5 已批准 4b 例外**，但 review 难度依然高。

本 playbook 把 4b 拆成 7 个有序步骤，每步独立 build/test 绿，便于：
- 人工实施（一次完成或分多 commit）
- 多 AI agent 协作（每个 agent 负责一步）
- review 复盘（按步骤定位破坏面）

## 1. 前置依赖（必须确认）

```bash
# 1. master 含 Phase 0-RFC + 0a + 0b + 0-LFT + 4a
git log --oneline origin/master | grep -E "Phase 0|Phase 4a|lint-fact-table" | head -5

# 2. internal/wshub/ 子包存在且 build 绿
ls internal/wshub/*.go
go build ./internal/wshub/

# 3. linter rule 3a 已实装（不报 wshub.go 缺 marker）
go run ./tools/lint-server-handlers/

# 4. master 上 wshub.go 含 921 行（含 godoc 字段表）
wc -l internal/server/wshub.go
```

## 2. 类型搬迁顺序（依赖图）

按依赖反向拓扑：先搬被依赖最少的，最后搬调用方最多的。

```
Step 1: wsClient 类型 + 字段搬到 wshub/client.go
        ← 不依赖 server 包其他类型；wshub.Hub 已有 *wsClient 占位
        破坏面：极低（type alias 兼容）

Step 2: HubRouter 接口搬到 wshub/types.go (覆盖 Phase 4a 的 placeholder)
        ← server.HubRouter 完整方法集替换 wshub.HubRouter (Phase 4a 仅 1 method)
        破坏面：中（dispatch / cron 等 import 路径调整）

Step 3: subscribe 块方法（Register/Unregister/HandleUpgrade）搬到
        wshub/hub_subscribe.go + wshub/hub_upgrade.go
        ← 依赖 wsClient (step 1) + HubRouter (step 2)
        破坏面：中（HTTP upgrade 是 dashboard 入口）

Step 4: broadcast 块方法搬到 wshub/hub_broadcast.go
        ← 依赖 subscriber.clients map 已搬到 wshub
        破坏面：中（debounce 时序敏感）

Step 5: historyMarshalCache 类型 + send 块方法搬到 wshub/hub_send.go +
        wshub/hub_eventpush_cache.go
        ← 风险最高：含 sessionSendLegacy / TrackSend / queue + cache
        破坏面：高（生产 send 路径）

Step 6: server.Hub → wshub.Hub 调用方切换
        ← server.go / cmd/naozhi/main.go / dashboard.go 等的 NewHub 调用
        破坏面：中（机械搬运，编译错误立刻暴露）

Step 7: 删 internal/server/wshub*.go + wsclient.go
        ← 旧文件全部删除；exemptions.yaml 中 wshub.go 条目删除
        破坏面：低（删文件，build 失败暴露遗漏）
```

## 3. 每步详细 checklist

### Step 1: wsClient 类型搬迁

**文件**：
- 删 `internal/server/wsclient.go`（455 行）
- 新建 `internal/wshub/client.go`

**方法**：
1. `wsClient` struct 定义 + `readPump` / `writePump` / `SendRaw` / `close` 方法
2. wshub.Hub 中已有 `wsClient struct{}` placeholder——替换为完整定义
3. server 包内剩余 wsClient 引用：暂时加 `type wsClient = wshub.wsClient` 别名

**自检**：
```bash
go build ./...                           # 全 build 绿
go test -race -count=1 ./internal/server/  # server 包测试通过
go test -race -count=1 ./internal/wshub/   # wshub 包测试通过
grep -c "type wsClient struct" internal/server/  # 必须 0
```

### Step 2: HubRouter 接口完整搬迁

**文件**：`internal/wshub/types.go`（覆盖 Phase 4a placeholder）

**方法**：把 `internal/server/consumer.go` 的 HubRouter 接口（~30 方法）
完整搬到 wshub/types.go。consumer.go 改为 type alias：
```go
type HubRouter = wshub.HubRouter
```

**自检**：
```bash
go vet ./...
grep "type HubRouter interface" internal/server/  # 必须 0
go test -race -count=1 ./internal/{server,wshub,session,cron}/...
```

### Step 3: subscribe + upgrade 方法搬迁

**文件**：
- 删 `internal/server/wshub_subscribe.go`（~400 行）
- 删 `internal/server/wshub_upgrade.go`（~310 行）
- 扩 `internal/wshub/hub_subscribe.go`（用真方法替换 placeholder）
- 新建 `internal/wshub/hub_upgrade.go`

**方法**：
- Register / Unregister / HandleUpgrade / handleAuth / wsAuthGate
- 私有 helper：`reserveOwnerSlot` / `releaseOwnerSlot` / `wsDeriveUploadOwner` /
  `clientIP` / `sameOriginOK`

**自检**：
```bash
go test -race -count=1 ./internal/wshub/
go test -race -count=10 ./internal/wshub/  # race detection 加强
go test -race -count=1 ./internal/server/  # server 包仍能跑（Hub 未完全搬走）
```

### Step 4: broadcast 块搬迁

**文件**：
- 删 `internal/server/wshub_broadcast.go`（~400 行）
- 扩 `internal/wshub/hub_broadcast.go`

**方法**：
- BroadcastSessionsUpdate / scheduleDebounce / debounceFire
- 私有 helper：debounce 时序

**自检**：
```bash
# debounce 时序敏感——必须 race count=100
go test -race -count=100 -run TestDebounce ./internal/wshub/
```

### Step 5: send 块 + cache 搬迁（**最高风险**）

**文件**：
- 删 `internal/server/wshub_send.go`（~400 行）
- 删 `internal/server/wshub_eventpush_cache.go`（~150 行）
- 扩 `internal/wshub/hub_send.go`
- 新建 `internal/wshub/hub_eventpush_cache.go`

**方法**：
- SendWithBroadcast / TrackSend / DoneSend / sessionSendLegacy
- historyMarshalCache 类型 + getOrMarshal / dropHistoryMarshalCache

**自检**：
```bash
# v0.6.1 §6.5 4b 验收 gate
go test -race -count=100 -run "TestHubConcurrency|TestShutdown|TestBroadcast" ./internal/wshub/
# Hub.queue 走接口（不是 *dispatch.MessageQueue 具体类型）
grep "dispatch.MessageQueue" internal/server/*.go | grep -v "_test.go" | wc -l  # 必须 0
```

### Step 6: 调用方切换

**文件**：
- `cmd/naozhi/main.go`：`server.NewHub` → `wshub.NewHub`
- `internal/server/server.go`：移除 Hub struct ctor（仅保留 *wshub.Hub 字段）
- `internal/server/dashboard.go`：`s.hub` 类型从 `*Hub` → `*wshub.Hub`

**自检**：
```bash
go build ./...
go test -race -count=1 ./...  # 全仓 race test
```

### Step 7: 清理

**文件**：
- 删 `internal/server/wshub.go`（921 行）
- 删 `internal/server/consumer.go` 中已搬走的接口
- 更新 `tools/lint-server-handlers/exemptions.yaml`：
  - 删除 `internal/server/wshub.go` 条目（until_phase: 4b）
  - PR commit message 必须含 `Closes-exemption: internal/server/wshub.go`

**自检**：
```bash
ls internal/server/wshub*.go 2>&1 | grep -v "_test.go"  # 仅可剩余 _test.go
ls internal/server/wsclient.go 2>&1                      # 不应存在
go run ./tools/lint-server-handlers/                     # rule 5 不报 stale
```

## 4. v0.6.1 §6.5 4b 验收 gate

- [ ] linter rule 3b（AST 字段访问对账）启用 warn 模式
- [ ] hub_concurrency_test.go -race -count=100 通过
- [ ] routes_snapshot 不变
- [ ] internal/server/ 不再 import dispatch.MessageQueue 直接类型
- [ ] exemptions.yaml 中 wshub.go 已删除（PR commit 含 Closes-exemption）

## 5. 风险控制

### 高风险：debounce 时序

`debounceTimer` + `debounceClosed` + `debounceClosedFast` 三字段协调
broadcast 节流。Shutdown 时序错会导致 panic 或 use-after-free。
**必须**：race count=100 + Shutdown 中触发 broadcast / Register 与
Shutdown 并发 / send 与 broadcast 同时进行 三种场景测试。

### 中风险：Hub 字段类型

Phase 4a 用 `any` 占位的字段（agents/guard/projectMgr 等）需要换成
真实类型——意味着 wshub 包必须 import session/project/node 等。
**确认**：`go mod tidy` 后 wshub 包依赖图无循环。

### 低风险：lint-fact-table 抑制噪音

Phase 0-LFT 工具在 design.md 跑出 7 个 false positive，CI mode=warn
不阻塞。Phase 4b PR 不修这些 noise——v2 fine-tune 独立 PR。

## 6. 不在 4b 范围

- agent_tailer + eventpush 主循环：留 Phase 4c
- routes_snapshot golden 更新：Hub 不直接挂路由，无需更新
- Phase 5 Server 字段瘦身：等 4c 完成后才能 47→12

## 7. 单 PR vs 多 PR 决策

如果 Phase 4b 一刀实施失败，可拆 4b1/4b2/4b3：
- 4b1: Step 1 + 2（wsClient + HubRouter 类型搬迁）
- 4b2: Step 3 + 4（subscribe + broadcast）
- 4b3: Step 5 + 6 + 7（send + 切换 + 清理 + race 测试）

代价：3 个 PR + 3 个 7 天观察期 = ~3 周；好处：每刀风险可控。

**v0.6.1 §6.5 已经批准 4b 单 PR 例外**——但实施时若发现风险过高，可
即时切换到 4b1/4b2/4b3 三刀（不需重新设计评审）。

## 8. 实施前最终 checklist

- [ ] master 含 Phase 0-RFC + 0a + 0b + 0-LFT + 4a 五个 commits
- [ ] internal/wshub/ 子包存在且 build/test 绿
- [ ] go test -race -count=1 ./... 在干净 master 全绿（不含 worktree 残留）
- [ ] design v0.6.1 §0 速查表 47 字段 / 7 块 / 13 PR / 13-15 周确认无误
- [ ] Phase 4a 观察期 7 天已过（或重叠豁免符合 §7.3）
- [ ] mutex profile baseline 采集（Phase 5 验收 gate 前置）

## 9. 完成后 hand-off

Phase 4b merge 后立即启动 Phase 4c（按 §7.3 重叠 4 天 OK）。Phase 4c
范围：agent_tailer.go + wshub_eventpush.go + wshub_agent.go 收尾。
