# Phase 4c 实施 Playbook

> **状态**：实施前 playbook（2026-05-28）
> **配套**：`docs/design/server-split-phase4-design.md` v0.6.1 §6.5
> **前置**：Phase 4b 全部已 merged
> **范围**：agent_tailer + hub_eventpush + hub_agent + hub_eventpush_cache 收尾

## 0. 为什么 4c 比 4b 简单

Phase 4c 是 wshub 抽包的最后一刀，但**风险中等而非高**：

- agent_tailer / eventpush 是**独立子系统**，对 Hub 的依赖是单向的（tailer
  通过 hub.tailers / hub.wiredLinkers 访问，不参与 send/broadcast 协调）
- 没有 send 路径或 broadcast 协调（这些都在 4b 完成）
- Phase 4b 已完成 wsClient / Hub 类型搬迁，4c 仅需扩展 wshub 包覆盖
  agent / eventpush 文件

行数：~1589（不含测试）。比 4b 的 2984 少一半。

## 1. 前置依赖（必须确认）

```bash
# 1. master 含 Phase 4b 全部
git log --oneline origin/master | grep -E "Phase 4b" | head -3

# 2. internal/wshub/ 子包包含完整 Hub 实装（subscribe + broadcast + send）
ls internal/wshub/*.go

# 3. internal/server/ 不再含 wshub_subscribe.go / wshub_broadcast.go /
#    wshub_send.go / wshub_upgrade.go / wsclient.go
ls internal/server/wshub_subscribe.go 2>&1  # 应为 No such file

# 4. exemptions.yaml 中 wshub.go 已删除（Phase 4b Closes-exemption）
grep "wshub.go" tools/lint-server-handlers/exemptions.yaml

# 5. master CI 全绿，无 race regressions
gh run list --branch master --limit 3
```

## 2. 搬迁顺序

### Step 1: agent_tailer 类型搬迁

**文件**：
- 删 `internal/server/agent_tailer.go`（827 行）
- 删 `internal/server/wshub_agent.go`
- 新建 `internal/wshub/tailer.go`
- 扩 `internal/wshub/hub_agent.go`（替换 Phase 4a placeholder）

**类型**：
- `agentTailer` struct + `tailerRegistry` struct + `tailerKey` type
- 私有 helpers: `newTailerRegistry` / `agentTailerPollInterval` /
  `agentTailerIdleGrace` / `agentTailerMax`

**方法**（搬到 `wshub/hub_agent.go`）：
- `SubscribeAgent` / `UnsubscribeAgent`（替换 Phase 4a placeholder）
- `(*Hub).agentTailerStart` / `agentTailerStop`

**自检**：
```bash
go build ./...
go test -race -count=1 ./internal/wshub/
grep "tailerRegistry" internal/server/  # 应为 0（除可能的 type alias）
```

### Step 2: historyMarshalCache + eventpush 搬迁

**文件**：
- 删 `internal/server/wshub_eventpush.go`（~150 行）
- 删 `internal/server/wshub_eventpush_cache.go`（~200 行）
- 扩 `internal/wshub/hub_eventpush.go`（替换 Phase 4a placeholder）
- 新建 `internal/wshub/hub_eventpush_cache.go`

**类型**：
- `historyMarshalCache` struct（已在 Phase 4b 类型 placeholder 占位为 any，
  4c 替换为真实类型）

**方法**（搬到 `wshub/hub_eventpush.go`）：
- `(*Hub).eventPushLoop`
- `(*Hub).getOrMarshal` / `(*Hub).dropHistoryMarshalCache`

**自检**：
```bash
go test -race -count=1 ./internal/wshub/
go test -race -count=10 -run "TestEventPush|TestHistoryMarshalCache" ./internal/wshub/
```

### Step 3: linter rule 3a/3b 切 fail 模式

按 v0.6.1 §6.5 4c 验收 gate：
- linter rule 3a + 3b 切到 fail 模式
- 对 wshub 子包内全部方法 0 违规

**修改**：
- `Makefile`: `lint-server` target 改为 `-mode fail`（默认 fail）
- `Makefile`: 新增 `lint-server-warn` target 给开发本地用
- `tools/lint-server-handlers/main.go`: 默认 fail mode（替换 warn）

**风险**：master 上其他文件（dashboard_*.go 等）仍有 file_size 超线豁免。
切 fail mode 后会爆出大量 violation。**正确做法**：仅 wshub 子包切 fail，
其他维持 warn。改 main.go 加 `-pkg-fail-mode` flag 区分。

### Step 4: agent_tailer.go exemption 删除

```yaml
# tools/lint-server-handlers/exemptions.yaml
# 删除：
- path: internal/server/agent_tailer.go
  current: 807
  limit: 500
  until_phase: 4c
```

**commit message** 必须含：
```
Closes-exemption: internal/server/agent_tailer.go
```

否则 rule 5 (stale_exemption) fail。

### Step 5: 测试 race count=100

```bash
go test -race -count=100 -run TestHubConcurrency ./internal/wshub/
go test -race -count=100 -run TestEventPush ./internal/wshub/
```

## 3. v0.6.1 §6.5 4c 验收 gate

- [ ] `internal/server/wshub*.go` 文件不再存在（grep 0 匹配）
- [ ] `agent_tailer.go` 不再在 server 包
- [ ] linter rule 3 对 wshub 子包内全部方法 0 违规
- [ ] linter rule 3a + 3b 切到 fail 模式（仅对 wshub 子包）
- [ ] exemptions.yaml 中 `agent_tailer.go (until_phase: 4c)` 已删除
- [ ] `hub_concurrency_test.go -race -count=100` 通过
- [ ] PR commit message 含 `Closes-exemption: internal/server/agent_tailer.go`

## 4. 风险控制

### 中风险：agent_tailer 多客户端并发

`tailerRegistry` 用 sync.Map 维护 task_id → *agentTailer 映射。多客户端
订阅同一 task_id 时，refCount + tickFn 会并发——race count=100 必跑。

### 低风险：eventpush 性能回归

`historyMarshalCache` 是 R214-PERF-4 的优化（多 tab 共享 marshal）。搬迁
不应破坏性能；**确认**：搬迁前后 benchmark 对比：

```bash
# 搬迁前 baseline
go test -bench=BenchmarkEventPushFanout -benchmem -count=5 ./internal/server/ > before.txt

# 搬迁后
go test -bench=BenchmarkEventPushFanout -benchmem -count=5 ./internal/wshub/ > after.txt

# diff 性能差异
benchstat before.txt after.txt
```

延迟 / alloc 应 ±5% 内。

## 5. 后续

Phase 4c merge 后启动 Phase 1（dashboard/cron/ 抽包）。按 v0.6.1 §7.3
重叠规则：**4c → 1 不可重叠 / 7 天独立观察期**。
