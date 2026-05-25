# Server Packages Contract

> **Status**: 设计契约（v0.1，2026-05-25）
> **Source**: [server-split-phase4-design.md](server-split-phase4-design.md) §六.2.0.5.1
> **Enforcement**: `tools/lint-server-handlers/` AST linter + `make lint-server`

每个 PR 在 description 中必须引用本文档相关条款，确保改动符合包契约。

---

## 1. 目录结构（拆分后定型）

```
internal/
  server/                  ~3500 行 / ≤ 15 个非测试文件
    server.go              核心 struct (≤ 12 字段) + Start/Shutdown
    routes.go              50 条路由注册（构造期）
    middleware.go          auth/csrf/gzip/ip_limiter wrappers
    debug.go               /api/debug/{pprof,vars}（debug_mode gate）
    workspace.go           validateWorkspace + sentinel errors
    routes_snapshot_test.go        Phase 0 起 CI 防漂移 gate
    testdata/routes.golden.json    snapshot

  dashboard/               ~7000 行（按业务子域）
    auth/                  ~600 行（dashboard_auth + csrf）
    session/               ~3500 行（list/events/send/upload/agent_tailer/upload_store）
    cron/                  ~2700 行（cron CRUD + runs + transcript）
    project/               ~1700 行（project_api + project_files）
    discovery/             ~600 行（discovered + takeover）
    ext/                   ~1400 行（scratch / memory / agent_events / cli / transcribe / system）

  wshub/                   ~3500 行（WebSocket Hub，保单 struct）
    hub.go                 Hub struct + ctor + Shutdown
    hub_subscribe.go       Register/Unregister/HandleUpgrade
    hub_broadcast.go       BroadcastSessionsUpdate / debounce
    hub_send.go            SendWithBroadcast / TrackSend
    hub_eventpush.go       事件推送循环
    hub_agent.go           agent_subscribe / agent_unsubscribe
    client.go              wsClient
    upgrade.go             HTTP upgrade + auth gate
    consumer.go            HubRouter / cronHubOps / scratchOps interfaces
    types.go               MessageEnqueuer / MessageQueueControl interfaces
    tailer.go              agentTailer (跟 agent_subscribe 强耦合)
```

无循环依赖：

```
cmd/naozhi/main.go
  ├─ server  ────── wshub, dashboard/{cron,project,session,auth,discovery,ext}, …
  ├─ wshub   ────── cli, session, dispatch, project, node, cron
  └─ dashboard/{cron,project,session,auth,discovery,ext}
                ─── 各自只 import 自己需要的具体类型 + 子包私有接口
```

---

## 2. 包契约（按目录）

### 2.1 `internal/server/`

**职责**：HTTP 入口；只放 routes / middleware / debug；不含业务逻辑。

**约束**：
- `Server` struct ≤ 12 字段（addr / mux / startedAt / onReady / appCtx / router / hub / scheduler / projectMgr / nodes / nodesMu / reverseNodeServer）
- 单文件 ≤ 500 行（exemptions.yaml 列出过渡期豁免）
- 不允许新增 `func (s *Server) handle*` 方法（Phase 0 baseline 已锁定 7 个，新增 PR 必须移到 dashboard 子包）
- 路由注册集中在 `routes.go`；任何改动同步更新 `testdata/routes.golden.json`

### 2.2 `internal/dashboard/<domain>/`

**职责**：HTTP handler 子包，按业务域拆分。

**约束**：
- 每个子包 ≤ 1500 行（含测试）
- 单文件 ≤ 800 行
- handler struct 取代 `func (s *Server)`：`type Handlers struct{ deps Deps }` + `func New(Deps) *Handlers`
- Deps 字段按 [§四.1.1 双轨硬名单](server-split-phase4-design.md#411-双轨硬名单)：
  - **白名单 5 个直接持具体类型**：`*session.Router`, `*cron.Scheduler`, `*project.Manager`, `*session.KeyResolver`, `*wshub.Hub`
  - **黑名单必须本地接口化**：私有窄面 ≤ 3 方法（如 `broadcaster`, `pathLocator`, `sessionWriter`）
  - 中间地带：PR reviewer 单条决定 + 给理由
- 不允许 import `internal/server` 包（消除反向依赖）

### 2.3 `internal/wshub/`

**职责**：WebSocket Hub，单实例，跨子包共享。

**约束**：
- `Hub` struct 保单一（不拆三子 struct）；字段按 5 块物理分组（[§五](server-split-phase4-design.md#五hub-单-struct--方法分文件)）
- 方法严格按文件分组：`hub_<block>.go` 只 WRITE 对应字段块
- 跨块只读豁免：godoc 头部声明 `READS-ALSO: <block>` 时 linter rule 3 放行；写跨块永远禁止
- 单文件 ≤ 500 行（exemptions.yaml 列豁免直到 Phase 4 抽完）

---

## 3. PR description 模板

每个 server-split phase PR 必须含：

```markdown
## Server-split Phase X PR

- [ ] 本 PR 改动符合 server-packages-contract.md §<相关节>
- [ ] AST linter `make lint-server` 输出无新违规（warn 模式）
- [ ] routes_snapshot golden 同步更新（如有路由变化）
- [ ] PR diff stat 中跨包字段引用减少数：__ ≥ 5
  ```bash
  # before
  $ grep -c "*cron\.Scheduler" internal/server/dashboard_cron.go
  # after
  $ grep -c "*cron\.Scheduler" internal/dashboard/cron/handlers.go
  ```
- [ ] 手工冒烟通过（按 [docs/ops/phase4-smoke-test.md](../ops/phase4-smoke-test.md) checklist）
- [ ] 7 天观察期 SLA 知悉（merge 后 7 天内不接下个 phase merge）
```

---

## 4. 维护

- **新增字段到 Server / Hub**：必须同步更新 `// 读写: <files>` 注释；linter rule 3 验证
- **新增跨包接口**：同步更新 [server-consumer-contracts.md](server-consumer-contracts.md) 的方法集 + 跨方法时序契约
- **改动路由**：`UPDATE_GOLDEN=1 go test -run TestRoutesSnapshot` 重生 golden + PR description 说明
- **超 800 行文件**：必须有 `until_phase` 豁免；豁免清单只能在 Phase 0 一次性添加，后续 phase 只能减不能增
