# RFC: kiro effort 可配置（naozhi 独立设置档位）

- 状态：Draft v1
- 日期：2026-08-01
- 作者：Kevin Zhao
- 范围：让 naozhi 独立设置 kiro 的 thinking effort 档位，不再依赖 kiro 全局配置
- 前置：`kiro-effort-visibility.md`（只读透出，同 PR 链的上一环）

## 1. Background & problem

`kiro-effort-visibility.md` 让 dashboard 能**看见**档位，但改档仍然只能去动
kiro 自己的全局设置 `~/.kiro/settings/cli.json` 的
`chat.modelDefaults[<model>].output_config.effort`。这条隐式通道有三个实际问题：

1. **不是 naozhi 的配置**。同一台机器上 naozhi 与操作者的交互式 kiro 共享这份
   全局设置，改一边影响另一边。
2. **无法按 backend / agent 区分**。naozhi 的价值之一是同时跑多个 backend 与
   多个 agent（`code-reviewer` / `researcher` / planner），它们对 effort 的合理
   取值不同（例如后台 cron 巡检用 `low` 省钱，planner 用 `max`），全局单值做不到。
3. **不可复现**。部署一台新机器要额外记得手改 kiro 全局设置，`config.yaml`
   无法自描述完整行为。

### 实测事实（2026-08-01，kiro-cli 2.16.0）

`kiro-cli acp --help` 提供 `--effort <EFFORT>`（"Initial effort level (e.g. low,
medium, high, xhigh, max)"）。我用手工 ACP 会话验证了它的**确切语义**：

| 场景 | 传入 | 全局默认 | 实际生效 |
|---|---|---|---|
| 同进程 `session/new` + `session/prompt` | `--effort low` | `xhigh` | **`low`** ✅ |
| 同进程 `session/load` resume + prompt | `--effort low` | `xhigh` | **`low`** ✅ |
| 进程 A 带 `--effort` 建会话；进程 B **不带**去 load | — | `xhigh` | `xhigh` ❌ |

三条结论：
- **`--effort` 压过 kiro 全局默认** —— 这是本 RFC 可行的前提。
- **resume 路径同样生效**，不需要额外的 RPC。
- **effort 绑定进程而非会话** —— 每次 spawn（含 resume spawn）都必须重新传，
  漏传就静默退回全局默认。这直接决定了实现必须落在 `BuildArgs`，而不是
  Init 期的一次性 RPC。

## 2. Goals & non-goals

### Goals

- G1：`cli.backends[].effort` 配置 kiro 档位，`cli.effort` 作为跨 backend 默认。
- G2：`agents[].effort` 可覆盖，使 cron / planner / 各 agent 独立取档。
- G3：spawn 与 resume 两条路径都稳定传入，重启后不静默降档。
- G4：不配置时行为与今天**逐字节一致**（不传 `--effort`，继承 kiro 全局默认）。

### Non-goals（均为用户明确划定）

- NG1：**不做 per-session 覆盖**。不加 dashboard 选择器，`sendParams` 不增字段，
  不碰 `backendOverrides` 那套 one-shot map。配置文件是唯一真相源。
- NG2：**不做运行时改档**。effort 是进程启动参数，改档必然要重启 CLI 进程；
  本 RFC 不提供该交互。改配置 + 重启 naozhi（或让会话自然轮转）是唯一路径。
- NG3：不持久化到 `sessions.json`。**这是 NG1/NG2 的直接推论** —— 既然唯一来源
  是配置文件，重启后从配置重新解析即得，落盘反而会让"改了配置但旧会话还用
  旧档"这种不一致变得可能。注意这与 `Model` 的处理**故意不同**：model 会被
  运行时的 `system/init` 事件改写，所以需要落盘；effort 没有这条运行时输入。
- NG4：不为 claude / codex 造等价物。两者 wire 里都没有 effort 概念
  （已 grep 确认无 `reasoning_effort`）。非 kiro backend 配了该项应当**报错**
  而非静默忽略（见 §4 校验）。

## 3. Alternatives considered

### 方案 A（选定）：完全平行 `model` 的配置链路

`cli.effort` → `cli.backends[].effort` → `agents[].effort` → `SpawnOptions.Effort`
→ `ACPProtocol.BuildArgs` 追加 `--effort`。

优点：
- `model` 已经把这条路走通并被测试覆盖，是**零新概念**的增量。优先级唯一决策点
  `resolveSpawnParamsLocked`（`router_lifecycle.go:580-601`）已有 CONTRACT 注释
  要求所有 spawn 路径走它，effort 自动获得同样的保证。
- 四条 spawn 入口（GetOrCreate fresh / GetOrCreate resume / ResetAndRecreate /
  Takeover）全部收敛到 `spawnSession`，一处实现覆盖全部，含 G3 的 resume。

缺点：要动的文件多（config / wireup / router / protocol 四层），但每层都是
机械平移。

### 方案 B：naozhi 托管 kiro settings 文件

仿 `naozhisettings` 包对 Claude settings 的做法，naozhi 生成一份自己的
`kiro settings` 并通过某种方式让 kiro 加载。

优点：能表达 effort 之外的更多 kiro 设置。

缺点：kiro 没有 `--settings-file` 之类的入口（`kiro-cli acp --help` 只有
`--agent/--model/--effort/--trust-all-tools/--trust-tools/--agent-engine`），
要么改写用户的全局文件（污染交互式 kiro，正是 §1 问题 1），要么靠 `HOME`
重定向（会连带切走 `~/.kiro/sessions/`，破坏历史与 resume）。**否决。**

### 方案 C：per-session 覆盖 + dashboard 选择器

优点：操作者不改配置就能试不同档位。

缺点：用户已明确不需要（NG1/NG2）。且 effort 是启动参数，per-session 改档
必然要 reset+respawn，交互重且容易让人误以为"当前这轮就会变"。**否决。**

## 4. 设计

### 4.1 配置形状

```yaml
cli:
  effort: ""            # 跨 backend 默认；留空 = 不传，继承 kiro 全局默认
  backends:
    - id: "kiro"
      path: "~/.local/bin/kiro-cli"
      model: "claude-fable-5"
      effort: "xhigh"   # 覆盖 cli.effort

agents:
  code-reviewer:
    effort: "max"       # 覆盖 backend 档位
  cron-sweeper:
    effort: "low"
```

### 4.2 优先级（低 → 高）

```
cli.effort  ←  cli.backends[<id>].effort  ←  agents[<id>].effort
```

刻意**不引入** access-profile 层：`model` 有 `access_profiles[].default_model`
是因为 access profile 要约束"这个身份能用哪个模型"（安全语义）；effort 只影响
成本与质量，没有对应的安全需求。少一层就少一处优先级歧义。

### 4.3 校验

新增 `validateEffortString`，与 `validateModelString`
（`config.go:1367-1382`）并列，但**更严** —— effort 是闭集，不是自由字符串：

- 允许空串（= 不传，保持现状）
- 非空必须 ∈ `{low, medium, high, xhigh, max}`
- 拒绝其他一切值，错误信息列出合法取值

**为何这里用白名单，而只读侧刻意不用**：只读侧显示的是 kiro **已经决定**的档位，
naozhi 不该因为不认识而丢弃（forward-compat，见 visibility RFC §5 R4）。而这里是
naozhi **主动构造 argv**，白名单是 flag-injection 防线（同
`validateModelString` 的 R215-SEC-P2-1 动机），且拼错档位应当在启动时报错而不是
让 kiro 静默拒绝或误解析。kiro 未来新增档位时更新这个白名单，是可接受的维护成本。

额外校验：**给非 ACP backend 配 effort 应当报错**（NG4）。claude / codex 收不到
这个参数，静默忽略会让操作者以为生效了。用 `backend.Profile` 的
`Features`/capability 判断，而非硬编码 `id == "kiro"`。

### 4.4 argv 落地

`ACPProtocol.BuildArgs`（`protocol_acp.go:250`）追加，与 `--model` 同形：

```go
if opts.Effort != "" {
    args = append(args, "--effort", opts.Effort)
}
```

`ClaudeProtocol` / `CodexProtocol` 的 `BuildArgs` 不动 —— 它们忽略
`SpawnOptions.Effort`，与它们已经忽略 `PermissionMode` / `DebugFile` 的先例一致。

### 4.5 arg-drift 同步（易漏点）

`router_shim.go:281` 的重启后参数漂移检测也调 `BuildArgs`：

```go
driftModel, driftArgs := r.backendDefaultsFor(recBackendID)
currentArgs = recWrapper.Protocol.BuildArgs(cli.SpawnOptions{
    Model: driftModel, ExtraArgs: driftArgs, ...
})
argsDrift = len(storedBase) > 0 && !slices.Equal(storedBase, currentArgs)
```

一旦 `BuildArgs` 会产出 `--effort`，这里**必须同步传 Effort**，否则每次 naozhi
重启都会把存活的 kiro shim 误判为参数漂移并重启会话（操作者可见为"重启
naozhi 后 kiro 会话全丢"）。`backendDefaultsFor` 当前签名
`(string, []string)` 需扩成带 effort 的三元组或返回一个小 struct。

注意 drift 检测**只能**用 backend 层默认（它不知道会话属于哪个 agent），所以
agent 级 effort 会被它看作漂移。这需要在实现时确认：drift 比较的
`storedBase` 是否本就排除了 agent 层 args？若不是，则 agent 级 effort 必须
排除在 drift 比较之外，或把生效值一起持久化供比较。**这是实现期必须先验证的
第一个问题**，写测试前先确认。

## 5. Test strategy

| 层 | 测试 | 参照 |
|---|---|---|
| config 校验 | 5 个合法档位通过；非法值（`ultra`/`LOW`/`-injected`/超长）被拒；空串通过 | `config_test.go:461` `TestValidateModelString`（表驱动） |
| config 注入 | 逐字段断言拒绝 `-injected` | `config_test.go:501` `TestValidateConfig_ModelInjection` |
| config 继承 | `cli.effort` → backend 空值回落；backend 值覆盖 | `EnabledBackends()` 现有 model fallback 测试 |
| 非 ACP backend | claude/codex 配 effort 报错 | 新增 |
| 优先级 | cli < backend < agent 三层，逐层覆盖 | `resolveSpawnParamsLocked` 现有 model 优先级测试 |
| argv | ACP `BuildArgs` 产出 `--effort v`；空值不产出；claude/codex 不产出 | `protocol_acp.go` BuildArgs 现有 `--model` 测试 |
| **arg-drift** | **配了 effort 后重启不误判漂移**（回归护栏，见 §4.5） | `router_shim.go` 现有 drift 测试 |
| resume | resume spawn 同样带 `--effort`（实测已证协议侧生效，此处测 naozhi 不漏传） | 新增 |

**回归证据**：arg-drift 那条是本 RFC 最有价值的测试 —— 它对应一个"实现正确性
100% 靠人记得"的隐患，且失败后果（重启丢会话）远重于功能本身。

## 6. Risk & rollback

| # | 风险 | 缓解 |
|---|---|---|
| R1 | **arg-drift 误判导致重启丢 kiro 会话** | §4.5 同步 + 专门回归测试。这是最高风险项 |
| R2 | 白名单落后于 kiro 新增档位 | 错误信息明确列出合法值 + 指向本 RFC；只读侧不受影响（仍原样透传未知档位） |
| R3 | 非 kiro backend 配 effort 被静默忽略 | §4.3 启动即报错 |
| R4 | `backendDefaultsFor` 签名变更影响 2 个调用点 | 编译期可见 |
| R5 | 操作者以为改配置能影响存活会话 | 文档明确 NG2；配置注释写明"下次 spawn 生效" |

**Rollback**：纯增量配置项，不配置则行为逐字节不变。`git revert` 即可；
无磁盘格式变更（NG3 不落盘），无 API 变更。

## 7. Observability

- 生效档位已经由前一个 RFC 透出到 dashboard —— **这正是验证配置生效的手段**：
  改配置 → 重启 → 看 header tag 是否变成期望档位。两个 RFC 在此闭环。
- spawn 时的 `slog` 已记录 argv；`--effort` 自动出现在其中。
- 不新增 metrics（同 visibility RFC 的理由：低频枚举状态）。

## 8. Compatibility & migration

- **配置**：三个新字段全部 `omitempty`，不配置 = 现状。无迁移。
- **磁盘格式**：无变更（NG3）。
- **多节点**：effort 是各节点本地 spawn 决策，远端节点用自己的配置。无协议变更。
- **与 kiro 全局设置的关系**：`--effort` 压过全局默认（§1 实测），所以配了
  naozhi 侧就以 naozhi 为准；留空则继续跟随 kiro 全局设置。两种模式都可用。

## 9. Rollout plan

独立 PR，叠在 visibility PR 之上（后者已含 dashboard 显示，是本 PR 的验证手段）。
无 feature flag（纯增量配置项）。tag minor bump。

## 10. 实现清单（依赖序）

1. **先验证 §4.5 的 drift 比较语义**（阻塞后续）
2. `internal/config/config.go` — `CLIConfig.Effort` / `CLIBackendConfig.Effort` /
   `AgentConfig.Effort` + `validateEffortString` + `validateConfig` 调用点 +
   `EnabledBackends()` fallback + 非 ACP backend 报错
3. `cmd/naozhi/main_init.go` — `backendWrappers.Efforts` map + 填充；
   `AgentOpts.Effort`
4. `internal/session/router_core.go` — `RouterConfig.BackendEfforts` + 赋值；
   `router_backend.go` — `bkStore.backendEfforts`（**必须带 `// 读写:` 注释**，
   `tools/check-router-fields` 会 AST 对账）
5. `internal/session/router_backend.go` — `backendDefaultsFor` 返回值扩展
6. `internal/session/router_lifecycle.go` — `AgentOpts.Effort` / `spawnParams.Effort` /
   优先级合并 / `spawnOpts.Effort`
7. `internal/session/router_shim.go` — drift 检测同步传 Effort
8. `internal/cli/wrapper.go` — `SpawnOptions.Effort`；`protocol_acp.go` — BuildArgs
9. `internal/cron/agent_opts.go` + `internal/wireup/cron_router_adapter.go` — cron 平行
10. `config.example.yaml` — 示例 + 「留空继承 kiro 全局默认」措辞
