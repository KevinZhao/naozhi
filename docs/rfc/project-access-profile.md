# RFC: Per-Project Access Profile（项目级默认访问方式：认证 / backend / model）

> **状态**: Draft v1（待评审）
> **作者**: naozhi team
> **创建**: 2026-07-08
> **范围**: 让每个项目（`.naozhi/project.yaml`）能声明一套默认「访问方式」——即用哪条**认证/上游链路**（1P Anthropic 直连 vs 经本机 bedrock-fallback-proxy 的 Bedrock）、用哪个 **backend**（claude / kiro / codex）、用哪个 **model**。核心是引入一个新的、与 backend 正交的配置维度 **access profile（访问档）**，其本质是一组命名的、经白名单校验的 env 覆盖，并把 shim 的进程级 env 快照改造成「基线 + per-spawn overlay」。
> **依赖 / 前置**:
> - `docs/rfc/multi-backend.md`（backend.Profile 抽象、`/api/cli/backends`、new-session picker、per-session backend chip 已就位）
> - `docs/rfc/envpolicy-consolidation.md`（`internal/envpolicy` 叶子校验器 + per-backend 凭据矩阵，本 RFC 在 §5 复用其 SSRF/profile guard 与 `DetectBackendFromEnv`）
> - `docs/rfc/agentcore-cloud-sandbox.md`（`payload.Env map[string]string` per-invoke env 覆盖的既有先例，本 RFC §4 参照其形状）
> **关联代码**:
> - 认证/env：`cmd/naozhi/main_claude_settings.go`（`claudeEnvAllowedPrefixes` :44、`awsEnvDenyList` :85、`filterClaudeEnv` :185）· `internal/shim/manager.go`（`m.shimEnv` 快照 :260、`cmd.Env = m.shimEnv` :479、`shimEnvAllowedPrefixes` ~:1286、`filterShimEnv` ~:1303）· `internal/envpolicy/backend.go`（`DetectBackendFromEnv` :58、`EnvCredsForBackend`）· `internal/sysession/env.go`
> - 解析链：`internal/session/router_lifecycle.go`（`AgentOpts` :305-311、`resolveSpawnParamsLocked` :473-569、`unregisterSessionLocked` :295）· `internal/session/routing.go`（`KeyResolver.ResolveForChat` :172-218）· `internal/session/router_backend.go`（`backendStore` :36-67、`SetSessionBackend` :296）
> - 配置：`internal/config/config.go`（`AgentConfig` :178-181、`PlannerDefaults` :122-125、`CLIBackendConfig` :205-210）· `internal/project/project.go`（`ProjectConfig` :45-72）· `internal/project/validate.go`（`ValidateConfig` :100-177）· `internal/project/manager.go`（`EffectivePlannerModel` :652-660）
> - Dashboard：`internal/server/static/dashboard.js`（`renderBackendPicker` :6164-6191、`getSelectedBackend` :6193、`backendChipHtml` :1490-1518、favorite toggle :1104）· `internal/dashboard/project/api.go`（`HandleConfigGet` :296、`HandleConfigPut` :317、`ValidateConfig` gate :377）· `internal/server/routes.go`（`PUT /api/projects/config` :341）· `internal/dashboard/ext/cli/handler.go`（`/api/cli/backends` :64）
> - backend.Profile：`internal/cli/backend/profile.go`（:30-126）· `profile_{claude,kiro,codex}.go`

---

## 0. TL;DR

今天 naozhi 里「一个 CLI 子进程用什么账号/上游/backend/model」的决策**没有项目粒度**：

- **认证/上游**（1P Anthropic vs Bedrock）完全靠一套**全局 env**——`~/.claude/settings.json` 的 `env` 段在启动时注入 naozhi 主进程，再经 shim 的**进程级单一快照** `m.shimEnv`（`manager.go:260`）赋给**每一个** CLI 子进程（`cmd.Env = m.shimEnv`，`manager.go:479`）。所有 session 共享同一条链路，当前全体走 `bedrock-fallback-proxy`（东京 `temp` profile）。
- **backend** 已有 per-session override（`backendStore.backendOverrides`）+ new-session picker，但**无项目默认**——绑定到项目的 planner 永远跑在 `router` 的 default backend 上。
- **model** 有项目默认（`PlannerModel`）但无认证/backend 联动。

用户诉求（三个真实项目）：
1. **polyquant**（个人）→ 默认 1P Anthropic 认证的 **Fable 5**（直连 `api.anthropic.com`，**不**经本机 proxy）。
2. **POC JD**（公司）→ 默认 **Bedrock 的 Opus 4.8**（经本机 proxy）。
3. 某项目 → 根本**不用 CC，默认用 kiro**。

诉求 1/2 卡在**同一个缺口**：没有 per-session env 覆盖能力。诉求 3 只是补齐已有 backend 链的项目默认层。

**方案**：引入 **access profile（访问档）**——一个命名的 env 覆盖集 + 默认 backend + 默认 model，定义在全局 config，被 project / agent 引用。三条解析链（auth / backend / model）各自独立继承。落地分三个 PR，风险递增：

- **PR-A**（最小，不碰 env）：`project.yaml` / agent 加 `backend` 默认字段，补齐 backend 解析链 → 诉求 3。
- **PR-B**（核心）：shim 从「单快照」改为「基线 + per-spawn overlay」；引入 access profile schema + 解析，env 注入点**复用 shim 白名单已放行的精确 key 集**，secret 走 `*_FILE` 引用 → 诉求 1/2。
- **PR-C**（UI）：dashboard 项目设置面板（后端 `PUT /api/projects/config` **已存在**，今天无 UI）+ new-session access-profile picker + 卡片 chip。

一句话：**access profile 是与 backend 正交的第四维，本质是「命名的、白名单内的 env overlay」；proxy 一行不改**（1P 那条链根本不经过它）。

---

## 1. 背景与动机

### 1.1 现状链路（已核实）

```
naozhi 主进程 os.Environ()
  └─ applyClaudeEnvSettings (main_claude_settings.go)：启动时一次性，从 ~/.claude/settings.json
     注入，前缀白名单 ANTHROPIC_/CLAUDE_/AWS_/… + awsEnvDenyList 屏蔽 AWS_PROFILE/AWS_ROLE_ARN
        └─ shim.Manager 构造时 filterShimEnv(os.Environ()) → m.shimEnv（单一快照, :260）
           └─ 每个 CLI 子进程 cmd.Env = m.shimEnv（:479）—— 全体共享
              当前全局：CLAUDE_CODE_USE_BEDROCK=1 + ANTHROPIC_BEDROCK_BASE_URL=http://127.0.0.1:8889
                 └─ bedrock-fallback-proxy（FORCE_FALLBACK=1，东京 temp profile；fable 固定 us-east-1）
```

三个正交维度的现状：

| 维度 | 变的是 | 现状机制 | 项目默认？ |
|---|---|---|---|
| **认证/上游** | 1P 直连 vs Bedrock proxy；哪个 key | 全局 env（settings.json → m.shimEnv 单快照） | ❌ 无任何粒度 |
| **backend** | wrapper 实现（stream-json / acp / app-server） | per-session `backendOverrides` + picker | ⚠️ 有 per-session，无项目默认 |
| **model** | `--model` 值 | 全局 `cli.model` / 项目 `PlannerModel` | ✓ 有 |

### 1.2 关键事实（三个后台调查的结论，带 file:line）

1. **backend.Profile 不含任何认证字段**（`profile.go:30-126`：`DefaultBinary / NewProtocol / DetectInProc / RequiredNodeCaps / HistoryDir / CostUnit / Features`）。claude vs kiro 的差异是 *wrapper*，**不是**账号/上游。因此认证**不能**塞进 Profile，否则把两个正交概念耦合死。
2. **认证切换全靠 env**：naozhi 不显式「选」1P/Bedrock，只透传 `CLAUDE_CODE_USE_BEDROCK` / `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_BEDROCK_BASE_URL` / `AWS_REGION`，由 claude CLI 自行解读（`envpolicy.DetectBackendFromEnv`，`backend.go:58`）。
3. **无 per-session/per-agent env**：`AgentConfig`（`config.go:178-181`）只有 `Model`+`Args`；shim 用单快照 `m.shimEnv`；`SpawnOptions` 无 env 字段。**这是诉求 1/2 的唯一真障碍。**
4. **shim 白名单是精确 key，且已放行全部需要的认证 key**（`manager.go` ~:1286-1393，安全加固 R214/R219-SEC-3 刻意放弃通配前缀）：`ANTHROPIC_BASE_URL=` / `ANTHROPIC_AUTH_TOKEN=` / `ANTHROPIC_BEDROCK_BASE_URL=` / `CLAUDE_CODE_USE_BEDROCK=` / `CLAUDE_CODE_SKIP_BEDROCK_AUTH=` / `AWS_REGION=` / `AWS_PROFILE=` …。→ **access profile 只在这个已放行集合内覆盖值，不放开白名单。**
5. **父进程注入侧 `awsEnvDenyList`（`main_claude_settings.go:85-94`）故意屏蔽 `AWS_PROFILE` / `AWS_ROLE_ARN` / `AWS_SHARED_CREDENTIALS_FILE`**（防认证源劫持）。→ Bedrock 侧**不靠切 AWS profile 区分账号**（见 §3 决策）。
6. **proxy 无 header 路由**，纯 Bedrock 网关，无「直连 api.anthropic.com」旁路。→ 1P 链路**不经过** proxy，直接指 `api.anthropic.com`；这恰好落在 access profile 的 env 覆盖上，proxy 不改。
7. **`PUT /api/projects/config` 已存在**（`api.go:317`，带 `ValidateConfig` + rate limit），**但零 dashboard UI**——今天 project 配置只能经 CLI 会话内 `update_config` 工具改。→ UI 地基已就绪。
8. **agentcore sandbox 已有 per-invoke env 先例**（`payload.Env map[string]string`）——per-call env overlay 在本仓已验证的形状。

### 1.3 范围

**做**：
- access profile schema（全局命名档）+ project/agent 引用
- shim per-spawn env overlay（把 `m.shimEnv` 单快照改为基线 + overlay）
- 三条解析链（auth / backend / model）在 `resolveSpawnParamsLocked` 汇合
- resume 时锁定历史 access profile（不重解析）
- secret 走 `*_FILE` 引用（不明文进 project.yaml）
- Dashboard：项目设置面板 + new-session access-profile picker + 卡片 chip

**不做**（防 scope creep）：
- **不**改 bedrock-fallback-proxy 一行（1P 不经过它；Bedrock 侧只换 base-URL/region）
- **不**支持单 session 跨 access profile 续传（认证与历史 backend 绑定，物理不可能）
- **不**放开任何 env 白名单前缀，**不**新增可放行的 env key（只在已放行集合内覆盖值）
- **不**在 project.yaml 里存明文 token/key（只存 `*_FILE` 路径）
- **不**碰 `awsEnvDenyList`（多账号 Bedrock 走 §11.3 多 proxy 实例，非本轮）

---

## 2. 概念模型：access profile 是第四维

一个 **access profile** 是一份命名的「怎么访问模型」的声明：

```
access profile = { env overlay（白名单内）, 默认 backend, 默认 model }
```

它与 backend **正交**：access profile 决定「用哪个账号/上游」，backend 决定「用哪个 CLI wrapper」。二者可自由组合（1P + claude、Bedrock + claude、任意 + kiro）。

三条链**各自独立**解析，最后在 `resolveSpawnParamsLocked` 合并进一次 spawn：

```
auth (env overlay):  请求显式 → per-session override → per-agent accessProfile → per-project accessProfile → 全局默认(现状 settings.json，即 profile="")
backend:             请求显式(opts.Backend) → per-session backendOverride(已有) → per-agent backend → per-project backend → cli.defaultBackend
model:               请求显式(opts.Model) → per-session → per-agent model → per-project(PlannerModel / accessProfile.default_model) → backend.DefaultModel
```

> **为何 auth 是独立维而不并进 backend**：§1.2#1 已证 Profile 不含认证；且同一 backend（claude）要能跑在两条认证链上（polyquant 的 1P claude、JD 的 Bedrock claude）。若把认证塞进 backend，用户就得为每条认证链复制一个 backend 定义，且 kiro/codex 的认证语义完全不同——耦合是净负债。

---

## 3. 三个诉求 → 配置落地

### 全局命名档（`config.yaml` 新增顶层）

```yaml
access_profiles:
  1p-fable:                        # 个人 1P Anthropic 直连
    display_name: "1P · Fable 5"
    chip_color: "#d97757"
    env:
      CLAUDE_CODE_USE_BEDROCK: "0"           # 关掉 Bedrock 模式
      ANTHROPIC_BASE_URL: "https://api.anthropic.com"
      ANTHROPIC_AUTH_TOKEN_FILE: "/home/ec2-user/.secrets/anthropic-1p.token"   # 从文件读，不明文
    default_model: "claude-fable-5"

  bedrock-opus:                    # 公司 Bedrock（经本机 proxy）
    display_name: "Bedrock · Opus 4.8"
    chip_color: "#7c5cff"
    env:
      CLAUDE_CODE_USE_BEDROCK: "1"
      CLAUDE_CODE_SKIP_BEDROCK_AUTH: "1"     # proxy 侧签名
      ANTHROPIC_BEDROCK_BASE_URL: "http://127.0.0.1:8889"
      AWS_REGION: "us-west-2"
    default_model: "claude-opus-4-8"
```

> `env` map 的 **key 必须落在 shim 已放行的精确集合内**（§1.2#4），否则 `ValidateConfig` 直接报 error；value 走 §5 的 SSRF/profile 叶子校验；`*_FILE` / `*_TOKEN_FILE` / `*_KEY_FILE` 后缀的 value 是宿主文件路径，spawn 时读文件内容注入对应的非-`_FILE` key（见 §4.3）。

### 项目引用（`.naozhi/project.yaml`）

```yaml
# polyquant/.naozhi/project.yaml —— 诉求 1
access_profile: "1p-fable"

# poc-jd/.naozhi/project.yaml —— 诉求 2
access_profile: "bedrock-opus"
planner_model: "claude-opus-4-8"     # 可覆盖 access profile 的 default_model（现有字段，语义不变）

# some-kiro-project/.naozhi/project.yaml —— 诉求 3
backend: "kiro"                       # 换 backend，认证走全局默认
```

### 一条会改变 Bedrock 诉求设计的约束（点名）

诉求 2「Bedrock Opus」**不靠切 AWS profile 区分账号**——父进程侧 `awsEnvDenyList`（`main_claude_settings.go:85-94`）故意屏蔽 `AWS_PROFILE`。而且当前链路 naozhi 侧根本不持有 AWS 凭据（`CLAUDE_CODE_SKIP_BEDROCK_AUTH=1`，proxy 用它自己的 profile 签名）。所以 Bedrock 侧 access profile 真正要变的只有两样：**`ANTHROPIC_BEDROCK_BASE_URL`（指哪个 proxy 端口）+ `AWS_REGION`**。若未来要真正的多账号 Bedrock，走 §11.3 的「多 proxy 实例」而非切 AWS profile。

---

## 4. 核心改造：shim per-spawn env overlay

这是整个 RFC 的技术命门（PR-B）。

### 4.1 现状与目标

**现状**（`manager.go`）：
```go
// 构造时一次性快照，之后所有 shim 进程共享
m.shimEnv = filterShimEnv(os.Environ())   // :260
...
cmd.Env = m.shimEnv                        // :479，每个 spawn 都赋同一份
```

**目标**：把 `m.shimEnv` 定位为**基线**（unchanged，仍是「全局默认 access profile」的物化），每个 shim spawn 时叠加一层 **per-spawn overlay**：
```go
cmd.Env = mergeShimEnv(m.shimEnv, spawnOpts.EnvOverlay)
```

### 4.2 数据流

```
project.access_profile → 解析出 access profile
  → resolveSpawnParamsLocked 组装 SpawnOptions.EnvOverlay map[string]string   (§6)
    → cli.Wrapper → shim.Manager.Spawn(..., overlay)
      → mergeShimEnv(baseline, overlay)：overlay 内的 key **覆盖** baseline 同名 key
        → 再过一遍 filterShimEnv 的**精确 key 白名单 + 值校验**（overlay 不豁免任何 gate）
          → cmd.Env
```

关键不变量：**overlay 不是「绕过白名单的后门」**。overlay 里出现任何不在 shim 放行集合内的 key，或 value 未过 SSRF/profile 校验，`mergeShimEnv` 丢弃该条并 `slog.Warn`（与现有 `shimEndpointEnvDropped` / `shimProfileEnvDropped` 同风格）。overlay 唯一多出来的能力是**在已放行 key 上覆盖 value**，per-spawn 而非 per-process。

### 4.3 secret：`*_FILE` 间接引用

project.yaml 会进 git，绝不存明文 token。约定：access profile `env` 里 `ANTHROPIC_AUTH_TOKEN_FILE` / `ANTHROPIC_API_KEY_FILE` 的 value 是**宿主文件路径**；`mergeShimEnv` 前的解析阶段：
1. 读文件内容（trim 尾部换行），
2. 注入为对应的 `ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_API_KEY`（已在 shim 放行集合内），
3. `*_FILE` key 本身**不**传给子进程。

文件路径校验：必须绝对路径、`os.Stat` 存在、mode 建议 `0600`（非 0600 时 warn 但不拒）；读失败 → 该 session spawn 失败并给明确错误（不静默降级到全局默认，避免「以为用 1P 实际用了 Bedrock」的静默串账）。

### 4.4 shim 复用池按 access profile 分片

shim 的价值是持久进程复用（避免每次 spawn 冷启动）。但**不同 access profile 的 env 不同，一个 1P 的 idle shim 绝不能被 Bedrock 的 session 复用**（否则拿错账号跑）。因此 shim 复用查找 key 要从 `(backend)` 扩展为 `(backend, accessProfileID)`。

**对已知问题 `project_shim_map_leak_max_shims`（`max shims reached(50)`）的影响**：分片会让 idle shim 池按 access profile 碎片化，更快触顶。缓解：
- access profile 数通常极小（个人环境 2-3 个），碎片化有限；
- `max_shims` 上限提为 per-（backend,profile）而非全局，或整体上调 + 依赖 PR#2353 已修的 reaper（identity-check 删除死 shim）；
- doctor / metrics 暴露 per-profile shim 计数，便于观测。

> **决策点 D3**（§13）：分片 key 是否也要含 model？否——model 是 `--model` CLI arg 而非 env，不影响进程 env，同一 shim 可跨 model 复用。分片只按 `(backend, accessProfileID)`。

---

## 5. 安全：复用 envpolicy，不新开口子

本 RFC 严格站在 `envpolicy-consolidation.md` 的既有不变量上，**不放松任何一条**：

| 不变量 | 来源 | 本 RFC 如何遵守 |
|---|---|---|
| shim 用精确 key、**禁**通配前缀 | R214/R219-SEC-3 | overlay 只在**已有精确集合**内覆盖 value，不新增可放行 key，不引入通配 |
| base-URL SSRF guard（loopback http / https 放行，非 loopback http 拒，IMDS 拒） | R090031-SEC-1 | overlay 的 `ANTHROPIC_*BASE_URL` value 过 `envpolicy.ValidateBaseURLValue`；`http://127.0.0.1:8889` 合法，`https://api.anthropic.com` 合法，`http://169.254.169.254` 拒 |
| AWS profile 值校验 `^[A-Za-z0-9_-]{1,64}$` | R20260603-SEC-1 | overlay 若含 `AWS_REGION` 过 `envpolicy.IsSafeProfileValue` 同族校验；`AWS_PROFILE` 不在本轮 overlay 允许集（§3 约束） |
| per-backend 凭据矩阵（Bedrock 部署不泄漏 `ANTHROPIC_API_KEY`） | R040034-SEC-4 | overlay 解析后过 `envpolicy.DetectBackendFromEnv` → `EnvCredsForBackend`：若 profile 声明 Bedrock 模式却带 `ANTHROPIC_API_KEY`，按矩阵 gate 掉并 warn |
| `CLAUDE_` kill-switch 拒绝 | R20260603-SEC-8 | `claudeEnvDenyList` 原地生效，overlay 不能引入 kill-switch |

**新增攻击面评估**：access profile 从 config.yaml（受信，运维手写）与 project.yaml（半受信，可能来自 git 同步）读取。project.yaml 的 `access_profile` 字段只是**引用一个名字**，不携带 env value——真正的 env 定义在受信的 config.yaml。因此「攻击者改 project.yaml」最多能把项目指向一个**已存在的、运维已定义的** access profile，无法注入任意 env。这是有意的信任边界设计（引用 vs 定义分离）。

`*_FILE` 读取的文件路径来自 config.yaml（受信），非 project.yaml，故不能被 git 同步的 project.yaml 指向任意文件。

---

## 6. 解析链落地（代码级）

### 6.1 config 层

```go
// internal/config/config.go
type AccessProfile struct {
    DisplayName  string            `yaml:"display_name"`
    ChipColor    string            `yaml:"chip_color"`
    Env          map[string]string `yaml:"env"`
    DefaultModel string            `yaml:"default_model"`
    DefaultBackend string          `yaml:"default_backend,omitempty"` // 可选：档内也能定默认 backend
}
// 顶层：AccessProfiles map[string]AccessProfile `yaml:"access_profiles"`

// AgentConfig（:178-181）新增：
type AgentConfig struct {
    Model         string   `yaml:"model"`
    Args          []string `yaml:"args"`
    Backend       string   `yaml:"backend,omitempty"`        // 新增（PR-A）
    AccessProfile string   `yaml:"access_profile,omitempty"` // 新增（PR-B）
}
```

### 6.2 project 层

```go
// internal/project/project.go（ProjectConfig :45-72）新增：
Backend       string `yaml:"backend,omitempty"`         // 新增（PR-A）
AccessProfile string `yaml:"access_profile,omitempty"`  // 新增（PR-B）
```

`ValidateConfig`（`validate.go:100-177`）新增校验（**写错名字不静默降级**，与 §1.2#7 的 rate-limit gate 同层）：
- `backend` 非空 → 必须 `backend.Get(id)` 存在，否则 `error` 级 diag
- `access_profile` 非空 → 必须在 `cfg.AccessProfiles` 有定义，否则 `error` 级 diag
- access profile 的 `env` key 必须全部在 shim 放行集合内、value 过 §5 叶子校验

### 6.3 AgentOpts + 解析

```go
// internal/session/router_lifecycle.go（AgentOpts :305-311）新增：
type AgentOpts struct {
    Model         string
    ExtraArgs     []string
    Workspace     string
    Backend       string            // 已有
    AccessProfile string            // 新增
    EnvOverlay    map[string]string // 新增：解析后的 env 覆盖（已读 *_FILE、已过校验）
    Exempt        bool
}
```

`KeyResolver.ResolveForChat`（`routing.go:172-218`）在设 `opts.Model`/`opts.Workspace` 的同处，补设 `opts.Backend` / `opts.AccessProfile`（从 project binding 读，project binding 由 `datasource.go` 扩展携带这两个字段）。

`resolveSpawnParamsLocked`（`router_lifecycle.go:473-569`）在现有 backend 优先级块（:492-506）旁，新增 **access profile 解析块**：
```
1. opts.AccessProfile 非空 → 用它
2. 否则 existing dead session 记录的 accessProfileID（resume 锁定，§7）
3. 否则 "" = 全局默认（m.shimEnv 基线，overlay 为空）
→ 解析出的 profile.Env（含 *_FILE 展开）→ opts.EnvOverlay
→ profile.DefaultModel 参与 model 合并（低于 opts.Model / PlannerModel）
```

`spawnSession`（:869）把 `opts.EnvOverlay` 塞进 `cli.SpawnOptions.EnvOverlay`，透传到 shim（§4）。

### 6.4 backend 链（PR-A，最小）

复用已有 `backendOverrides` + `wrapperFor`。只需让 project/agent 的 `backend` 默认值参与 `resolveSpawnParamsLocked` 的 backend 优先级——在「per-session override」与「defaultBackend」之间插入「per-agent → per-project」两层。kiro 的 `RequiredNodeCaps=["acp"]` 节点选择（`multi-backend.md §6`）对项目默认 backend 同样生效。

---

## 7. resume 锁定：不可中途换账号（关键正确性约束）

一个 session 的历史与 session ID 是 **backend + 认证 特定**的（claude `--resume` sid 属于某账号；ACP `session/load` 属于某 kiro 实例）。**dead session 恢复必须沿用它当时的 (backend, accessProfileID)，不能按现在的 project 配置重解析**——否则拿 Bedrock 的对话去 1P 端点 resume 会串账/失败。

落地：
- session 持久化（shim state / store record）新增记录 `AccessProfileID` 字段（与已记录的 `Backend` 并列）。
- `resolveSpawnParamsLocked` 的 access profile 优先级里，「existing session 记录值」高于「project 默认」（§6.3 第 2 步）。
- **复查 `unregisterSessionLocked`（:295）**：现在它 `delete(backendOverrides, key)` 清理未 spawn 的 dashboard 选择；加了继承默认后，要确保清理的是「一次性 override」而非「持久化历史值」——历史 accessProfileID 存 session record，不存 override map，二者不混。

---

## 8. UI 设计

三块 UI，全部遵循 `multi-backend.md §8` 的原则（单档零变动、以 session 实际值为准、graceful degrade、`data-*` 属性不用 id 做 CSS class）。

### 8.1 项目设置面板（新增，后端已就绪）

**这是最大的新 UI，但后端 `GET`/`PUT /api/projects/config` 已存在（`api.go:296/317`），今天完全没有前端调它。**

- **入口**：项目分组标题行，紧邻现有 favorite star（`dashboard.js:1104`）加一个齿轮 `⚙` 按钮（`data-action="project-settings"`）。
- **面板形态**：右侧滑出抽屉（复用 scratch drawer 的抽屉容器风格，见记忆 `project_scratch_drawer`），非模态，避免遮挡会话列表。
- **字段**（读 `GET /api/projects/config`，写 `PUT`）：

  | 字段 | 控件 | 数据源 |
  |---|---|---|
  | Display name | text input | `config.display_name` |
  | Emoji | emoji picker / text | `config.emoji` |
  | **Access profile** | `<select>` | `access_profiles` 列表（新端点 `/api/access-profiles`），option 显示 `display_name`，值为 profile id；含「（全局默认）」空选项 |
  | **Backend** | `<select>` | 复用 `renderBackendPicker` 的数据（`/api/cli/backends`），含「（默认）」空选项 |
  | Planner model | text input（可选，占位显示 access profile 的 `default_model`） | `config.planner_model` |
  | Planner prompt | textarea（8192B 上限，前端计数） | `config.planner_prompt` |

- **保存**：`PUT /api/projects/config`（已有 `ValidateConfig` gate + rate limit `configPutLimiter`）。校验失败（未知 backend / 未知 access profile）→ inline 红字提示，复用后端 diag 文案。
- **联动预览**：选定 access profile 后，面板底部只读展示「解析后生效链路」——如 `1P · Fable 5 → api.anthropic.com → claude-fable-5`，让用户看清「这个项目下一个会话会怎么跑」。**只展示 profile 名与非敏感字段（base-URL / model / backend），绝不显示 token 值或 `*_FILE` 内容。**

### 8.2 new-session access-profile picker（扩展现有 backend picker）

`renderBackendPicker`（`dashboard.js:6164-6191`）已建立范式：creation-time `<select>`、`list.length<=1` 时隐藏、`getSelectedBackend` 读值。**照抄一个 `renderAccessProfilePicker`**：

- 位置：new-session 模态 / project palette，紧邻 backend picker 上方。
- 默认值：**该项目绑定的 access profile**（若从项目发起）> `localStorage.naozhi.lastAccessProfile`（仿 backend 的 last-picked 记忆）> 全局默认。
- `access_profiles` 只有 0/1 个时**整体隐藏**（单档部署零变动，与 backend picker 同规则）。
- 提交：随 session 创建 payload 携带（无独立 POST，与 backend 同路径，见 §1.2 报告的 `sessionBackends[key]` 模式）。

### 8.3 session 卡片 access-profile chip（扩展现有 backend chip）

`backendChipHtml`（`dashboard.js:1490-1518`）已渲染 backend 彩色 pill。**并排加一个 access-profile chip**：

- 颜色取 `access_profile.chip_color`（config 定义），文本取 `display_name`（超 8 字符截断 + hover 全名，仿 backend chip 规则）。
- 多档模式才显示；单档隐藏。
- SessionView 新增 `access_profile` 字段（id）+ `access_profile_label` / `access_profile_color`（server 侧从 config 解析，前端不碰 config）。
- **chip 绝不显示认证细节**（无 key、无 token、无 base-URL），只显示档名——运维语义足够，避免肩窥泄漏。

### 8.4 反例（不要这么做）

- ❌ 在 dashboard.js 里写 `if (accessProfile === '1p-fable')` 分支 → 用 config 下发的 `display_name`/`chip_color`
- ❌ chip 或面板显示 token / `ANTHROPIC_AUTH_TOKEN` 值 → 只显示档名与 base-URL 等非敏感字段
- ❌ 把 access profile 的 `env` map 整个下发到前端 → 前端只拿 `{id, display_name, chip_color, default_model, backend}`，env 内容留后端
- ❌ 用 access profile id 做 CSS class → `data-access-profile` 属性 + prefix 选择器

---

## 9. 测试策略

**新增 unit**：
- `internal/config`：`TestAccessProfile_Validate`（未知 backend、未知档、env key 不在放行集、value SSRF 拒、`*_FILE` 缺失）。
- `internal/shim`：`TestMergeShimEnv_OverlayOverridesBaseline`、`TestMergeShimEnv_OverlayStillGated`（overlay 带非放行 key / IMDS base-URL / Bedrock 模式带 ANTHROPIC_API_KEY → 全部丢弃 + warn）、`TestMergeShimEnv_FileSecretExpansion`（`*_FILE` 读文件、trim、原 key 不传）。
- `internal/session`：`TestResolveSpawnParams_AccessProfilePrecedence`（请求 > session记录 > project > 全局）、`TestResolveSpawnParams_ResumeLocksAccessProfile`（dead session resume 不被 project 新档覆盖）。
- `internal/project`：`ValidateConfig` 新分支表驱动。

**Regression（pin，不改断言）**：`envpolicy` 全部等价性测试、`sysession/env_test.go` 13 例、`internal/shim/filter_env_*_test.go` 5 文件——**overlay 改造后这些必须仍全绿**（证明没削弱任何 gate）。

**契约测试**：`internal/server/static_*_test.go` 加 `renderAccessProfilePicker` 隐藏规则（≤1 档不渲染）、chip 单档隐藏、面板不泄漏 env 值。

**E2E**：三档配置（1p-fable / bedrock-opus / 默认）下，项目设置面板保存后新建会话 → 断言 spawn 的子进程 env（经测试 hook）确实带对应 overlay；polyquant 会话 env 有 `ANTHROPIC_BASE_URL=api.anthropic.com` 且**无** `CLAUDE_CODE_USE_BEDROCK=1`。

**命令**：`go test -race ./internal/config/... ./internal/shim/... ./internal/session/... ./internal/project/... ./internal/envpolicy/... ./internal/server/...`（`internal/server` -race 全包实跑久，给 480s timeout，见记忆 `review_cron_cr_2026_06_10`）。

---

## 10. 风险与回滚

| 风险 | 触发 | 缓解 |
|---|---|---|
| overlay 成为白名单后门 | overlay 注入任意 env | overlay 不豁免任何 gate；`mergeShimEnv` 后仍过 filterShimEnv 精确集合 + 值校验（§4.2/§5）；pin 测试守住 |
| resume 串账 | dead session 按新 project 档重解析 | §7 session record 锁定 (backend, accessProfileID)；测试 `ResumeLocksAccessProfile` |
| shim 池碎片化触顶 max_shims | 多档 + 多 backend | §4.4 per-(backend,profile) 分片 + 上限调整 + reaper（PR#2353 已修）+ doctor 计数 |
| secret 泄漏 | token 明文进 git / chip 显示 | `*_FILE` 间接引用；project.yaml 只存档名；UI 绝不显示 env value（§8.4） |
| 静默降级 | 未知档 / `*_FILE` 读失败 悄悄用了 Bedrock | ValidateConfig error 级 diag（不阻断启动但显式 warn）；spawn 时 `*_FILE` 读失败 → 该 session 失败并明确报错，不 fallback |
| 单档部署被打扰 | 只有 claude/无档的用户看到多余 UI | picker/chip/面板字段全部 `≤1` 时隐藏（与 backend picker 同规则） |

**回滚**：三个 PR 独立。PR-A（backend 默认）纯加字段、行为叠加，`git revert` 即回。PR-B（overlay）无档配置时 `EnvOverlay` 恒空、`mergeShimEnv(baseline, nil)==baseline`，行为等同今天；revert 安全。PR-C（UI）纯前端 + 一个只读端点，revert 不影响后端。

---

## 11. 兼容 & 迁移

### 11.1 向后兼容

- 无 `access_profiles` 配置、project.yaml 无 `access_profile`/`backend` → 全部走全局默认，行为**完全等同今天**（`m.shimEnv` 基线、default backend、`PlannerModel`）。
- `SpawnOptions.EnvOverlay == nil` 时 `mergeShimEnv` 返回基线本身，无行为变化。
- project.yaml / config.yaml 旧文件反序列化新字段得零值，无迁移脚本。

### 11.2 刻意保留的非对称

cc child 走 `--setting-sources user` 直接读 `~/.claude/settings.json`（`direct-user-settings.md §7.1` 记录的有意非对称）。access profile overlay 作用于**子进程 env**，与 settings.json 的 `env` 段是两条路径：overlay 的 env 优先级**高于** settings.json（因为它在 cmd.Env 上覆盖）。需在文档明确：**access profile 覆盖 settings.json 的同名 env**（这正是我们要的——让项目档压过全局默认）。

### 11.3 未来：多账号 Bedrock（非本轮）

若要真正的多 AWS 账号 Bedrock（而非同账号跨区），正确做法是**多开 proxy 实例**：每个绑不同 AWS profile/region，各监听不同 loopback 端口（`:8889` / `:8890` …）。access profile 只切 `ANTHROPIC_BEDROCK_BASE_URL` 指向对应端口。凭据始终在 proxy 侧、不进 naozhi、不碰 `awsEnvDenyList`。本 RFC 的 access profile 机制**天然支持**这个未来（换 base-URL 而已），无需再改 naozhi。

---

## 12. Rollout / Sprint

| PR | 内容 | 触碰 | 风险 | 工时 |
|---|---|---|---|---|
| **PR-A** | project/agent `backend` 默认字段 + 解析链插入 + ValidateConfig | config/project/session（不碰 env/shim） | low | 0.5 |
| **PR-B1** | shim `mergeShimEnv` overlay + `SpawnOptions.EnvOverlay`（overlay 恒空，纯地基） | shim/cli | med（碰 shim 热路径） | 1 |
| **PR-B2** | `AccessProfile` schema + 解析 + `*_FILE` 展开 + envpolicy 复用 + resume 锁定 | config/project/session/envpolicy | med | 1.5 |
| **PR-C1** | `/api/access-profiles` 只读端点 + SessionView chip 字段 | server | low | 0.5 |
| **PR-C2** | 项目设置抽屉面板（消费已有 `PUT /api/projects/config`） | dashboard.js | med | 1.5 |
| **PR-C3** | new-session access-profile picker + 卡片 chip | dashboard.js | low | 1 |
| **合计** | | | | **~6 人/天** |

顺序建议：PR-A 先上（独立收益，诉求 3）→ PR-B1 地基（overlay 恒空，零行为变化，可安全 land）→ PR-B2 点亮认证（诉求 1/2）→ PR-C 逐块补 UI。**PR-B1 与 PR-B2 分开是关键**：B1 让「shim 接受 overlay」这个热路径改动先在 overlay 恒空下跑一轮生产验证，再由 B2 真正填 overlay。

---

## 13. 决策点（待 owner 确认）

1. **access profile 定义位置**：全局 `config.yaml` 顶层 `access_profiles`（本 RFC 选型）vs 单独文件 `~/.naozhi/access_profiles.yaml`？
   - 建议：先放 config.yaml（与 `agents`/`cli.backends` 同层，运维心智一致）；档变多再拆文件。
2. **`*_FILE` secret 机制** vs 直接读 `~/.claude/settings.json` 已有的 token？
   - 建议：`*_FILE` 间接引用。理由：不同项目要**不同** key，settings.json 只有一套全局；且 project.yaml 进 git 不能带明文。
3. **shim 分片 key 是否含 model**：否（model 是 CLI arg 非 env，§4.4）。确认。
4. **default backend 也放进 access profile 吗**（§6.1 `AccessProfile.DefaultBackend`）vs 只在 project/agent 顶层？
   - 建议：两者都支持但 project 顶层 `backend` 优先——让「1P 档默认配 claude、但某项目想在 1P 下用别的」成为可能。
5. **单账号跨区（诉求 2 现状）够用吗，还是首发就要多 proxy 实例（§11.3）**？
   - 建议：本轮只做单 proxy（换 base-URL/region 已满足 JD 诉求）；多 proxy 留到有真实多账号需求。

---

## 14. 验收标准（GA）

1. **单档兼容**：无 `access_profiles` 配置时所有现有功能行为完全不变（回归 100% 绿，`m.shimEnv` 路径零变化）。
2. **诉求 1**：polyquant 绑定 `1p-fable`，新建会话的 CLI 子进程 env 带 `ANTHROPIC_BASE_URL=https://api.anthropic.com` + token（来自 `*_FILE`），**无** `CLAUDE_CODE_USE_BEDROCK`，且**不经过** `:8889` proxy（proxy 日志无该会话流量）。
3. **诉求 2**：poc-jd 绑定 `bedrock-opus`，会话经 `:8889` proxy，model=opus-4.8。
4. **诉求 3**：某项目绑定 `backend: kiro`，会话用 ACP wrapper，派到声明 `acp` cap 的节点。
5. **安全**：overlay 无法注入放行集合外的 key；Bedrock 模式下 `ANTHROPIC_API_KEY` 被矩阵 gate；SSRF guard 对 overlay 生效；所有 envpolicy/shim pin 测试不改断言全绿。
6. **resume 锁定**：dead session 恢复沿用历史档，不被 project 新档覆盖。
7. **UI**：项目设置面板可读写 access_profile/backend/planner_*，未知值 inline 报错；picker 单档隐藏；chip 不泄漏认证细节；axe-core 无 violation。
8. `go test -race -count=1 ./...` 全绿。
