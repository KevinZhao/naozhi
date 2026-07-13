# RFC: Profile 配置模型 v2（简洁化 + 防维度爆炸 + 极简 UI）

> **状态**: Draft v1（待评审）
> **作者**: naozhi team
> **创建**: 2026-07-11
> **前身**: `docs/rfc/project-access-profile.md`（v1，已落地 auth overlay：PR #2359/#2360 + OAuth token 注入）
> **一句话**: v1 把「用哪个账号/上游」做成了 per-session 可切的 access profile。落地后暴露两个问题——(1) 每加一个 auth 变量要手工同步 3~4 处白名单，(2) 接下来要进 profile 的配置（thinking effort、超时…）**每个 backend 表达方式都不同**，照 v1「一袋子 env」的建模会引发 (维度 × backend) 矩阵爆炸。本 RFC 收敛数据模型、把 backend 特异性挡在配置层之外，并把 UI 压到「选一次，不用懂」。

---

## 0. TL;DR

v1 的 profile = `Env map[string]string`（一袋子白名单内的环境变量）。这对 auth 勉强成立，因为 auth 恰好能表达成 env。但它有两个根上的问题：

1. **同步负担**：一个 auth 变量的知识被复制到 4 个地方（`OverlayAllowedKeys` / `overlayFileKeys` / `ValidateOverlayEntry` 的 switch / shim `shimEnvAllowedPrefixes`），没有任何东西强制它们一致，漏一处 = **静默失效**（配置校验过、UI 正常、token 到不了 CLI）。
2. **抽象错位**：下一批要进 profile 的配置是**语义配置**（thinking effort、输出上限、超时），它们**不是** env——`MAX_THINKING_TOKENS` 只对 claude 有意义，bedrock 是 API reasoning budget，codex 是 `low/medium/high` 枚举。把它们塞进 env 白名单，等于把 backend 特异性泄漏进配置层，每加一个 backend 都要回头审一遍这袋 env 对它是否有效。

**v2 的两条主线**：

- **数据模型**：profile 拆成 **auth（跨 backend 同构的 env）** + **语义字段（backend 无关的意图，如 `thinking: high`）** 两部分。auth 走 env overlay（保留 v1 机制），语义字段**永不进 env 白名单**——它们是 profile 上的强类型字段，取值走统一 resolve，落地交给 backend 自己翻译。
- **两层分治**：**解析层**（一个值从哪些来源按什么优先级取）是同构的，收敛成一个通用 layered-resolve；**渲染层**（`thinking: high` 在某 backend 上落成什么）是异构的，交给已有的 `cli.Caps` / backend protocol 接口，profile 层完全不感知。
- **准入规矩**（新增，写死在 envpolicy 包文档）：**凡是 backend 语义不同的配置，不进 env 白名单**。env overlay 只放「所有 backend 都当 env 消费、语义一致」的东西（auth token、base-url、region）。

净效果：加一个 auth 变量 = 注册表加 1 行 + 启动断言兜底漏登记；加一个语义维度（thinking effort）= profile 加一个字段 + 每个 backend 实现一次 render，**不碰 env 白名单**。UI 侧用户永远只面对「用哪个（1P / 公司 / 订阅）+ 想多深思考」两个人话选择，不碰任何变量名。

---

## 1. 背景：v1 落地后暴露了什么

v1 已交付并在线上跑：`config.AccessProfile{ DisplayName, ChipColor, Env, DefaultModel, DefaultBackend }`，per-session 解析优先级链（resume-lock > override > opts > default），dashboard picker + chip，runtime create endpoint。**这些都保留，v2 是演进不是重写。**

问题一（同步负担）的实证——加 `CLAUDE_CODE_OAUTH_TOKEN`（commit `00d2f48a`）改了：

| 位置 | 职责 | 漏了会怎样 |
|---|---|---|
| `envpolicy.OverlayAllowedKeys` | 能否声明 | config 直接报错（可见） |
| `envpolicy.overlayFileKeys` | `*_FILE`→concrete 映射 | `*_FILE` 引用不生效（半可见） |
| `envpolicy.ValidateOverlayEntry` switch | 值校验 | 弱校验（低危） |
| `shim.shimEnvAllowedPrefixes` | 能否真的传给子进程 | **静默失效**：全绿但 token 到不了 CLI |

最后一行是唯一没有可见反馈的漏点，也是最难排查的——所有「看得见」的地方都对。

问题二（抽象错位）是**尚未发生但即将发生**的：你已经点名 thinking effort，而 thinking effort 在 claude / bedrock / codex 上分别是数值 env、API 参数、枚举 flag。v1 模型下唯一的落地路径是往 `OverlayAllowedKeys` 塞 `MAX_THINKING_TOKENS`——一旦这么做，用户在一个 bedrock profile 里设了它就是静默无效，又一个「看得见但不生效」的坑，且矩阵从此锁死。

---

## 2. 核心洞察：两种复杂度必须分开治

profile 这层里混了两种本质不同的复杂度，v1 把它们都压进了 env 一个维度，这是乱的根源。

### 解析层（同构，可统一）

「一个值，从哪些来源、按什么优先级取。」auth、model、thinking effort、超时……**几乎全是同一条链**：

```
per-request(opts) > session-override(一次性消费) > resume-lock > profile > backend-default > router-default
```

v1 里 backend 一条链、access-profile 一条链、model 一条链**各手写了一遍**（`resolveSpawnParamsLocked` 里两大段近乎同构的代码 + 注释）。每加一个维度就要再抄一条。这层**该收敛成一个通用 helper**。

### 渲染层（异构，不可统一，也不该统一）

「`thinking: high` 在这个 backend 上落成什么。」这层**每个 backend 天生不同**，强行统一就是把它硬塞进 env 白名单的错误。正确载体项目里**已经有了**：`internal/cli/protocol.go` 的 `Caps` + 各 `protocol_{claude,codex,acp}.go`。让每个 backend 声明「我支不支持 thinking、我怎么表达它」，profile 层只传意图（`high`），不知道也不关心它最终变成 env 还是 flag 还是 API 字段。

> **一句话分界**：解析层回答「取哪个值」，渲染层回答「这个值在这个 backend 上是什么」。profile 只碰前者。

---

## 3. 数据模型 v2

```go
// config.Profile —— v1 AccessProfile 的超集，向后兼容（旧字段全保留）。
type Profile struct {
    // ---- 展示（非敏感，可对外）----
    DisplayName string `yaml:"display_name,omitempty"`
    ChipColor   string `yaml:"chip_color,omitempty"`

    // ---- auth / 上游：跨 backend 同构的 env overlay（v1 机制，保留）----
    // 只放「所有 backend 都当 env 消费、语义一致」的 key：token / base-url /
    // region。*_FILE 间接引用不变。准入由 envpolicy 注册表 + 启动断言把守。
    Env map[string]string `yaml:"env,omitempty"`

    // ---- 语义意图：backend 无关，NEVER 进 env ----
    DefaultModel   string `yaml:"default_model,omitempty"`
    DefaultBackend string `yaml:"default_backend,omitempty"`
    // Thinking 是意图枚举（"", "low", "medium", "high"）。空=不表态，继承下层。
    // 由 backend 渲染层翻译：claude→MAX_THINKING_TOKENS 档位；codex→reasoning
    // effort flag；不支持的 backend→忽略（Caps.Thinking=false）。profile 层
    // 只存意图，绝不存 backend 特定的表达。
    Thinking string `yaml:"thinking,omitempty"`
    // 未来的语义维度（输出上限、超时…）沿此模式加字段，同样不进 Env。
}
```

**判据（作者写新配置时问自己一句话）**：
- 「这个值在所有 backend 上都是同一个 env、同一个含义吗？」→ 是 → 进 `Env`（走白名单）。
- 「不同 backend 表达方式不同，或有的 backend 根本没有？」→ 是 → 语义字段（走 Caps 渲染）。

---

## 4. 收敛 auth 白名单：单一注册表 + 启动断言

治问题一。把散在 4 处的 env-key 知识收成**一张表**，三处从它派生，第四处（shim）用启动断言兜底。

```go
// internal/envpolicy —— 单一事实来源
type overlayKey struct {
    Key      string              // "ANTHROPIC_AUTH_TOKEN"
    FileKey  string              // "ANTHROPIC_AUTH_TOKEN_FILE"，空=不支持 *_FILE
    Validate func(string) error  // nil=仅 charset；base-url/region 各自的叶子
}

var overlayKeys = []overlayKey{
    {Key: "ANTHROPIC_AUTH_TOKEN",     FileKey: "ANTHROPIC_AUTH_TOKEN_FILE"},
    {Key: "ANTHROPIC_API_KEY",        FileKey: "ANTHROPIC_API_KEY_FILE"},
    {Key: "CLAUDE_CODE_OAUTH_TOKEN",  FileKey: "CLAUDE_CODE_OAUTH_TOKEN_FILE"},
    {Key: "ANTHROPIC_BASE_URL",       Validate: ValidateBaseURLValue},
    {Key: "ANTHROPIC_BEDROCK_BASE_URL", Validate: ValidateBaseURLValue},
    {Key: "AWS_REGION",               Validate: validateRegion},
    // ...加一个 auth 变量 = 加一行
}
```

- `OverlayAllowedKeys` / `overlayFileKeys` / `ValidateOverlayEntry` 全部由 `overlayKeys` 在 init 派生，不再手写三份。
- shim 的 `shimEnvAllowedPrefixes` **不能**派生（它是运行时真正 gate，还含 system/AWS/git 等一大堆非 overlay 项，overlay 只是其子集）。改为**启动断言**（放 shim 包，方向对：shim 已 import envpolicy）：

```go
// 启动即崩，而不是运行时静默失效
for _, k := range envpolicy.OverlayConcreteKeys() {
    if !shimAllowsExact(k) {
        panic("overlay key missing from shim allowlist: " + k)
    }
}
```

「四处必须一致」从口头契约变成**漏了就起不来**，那个唯一无可见反馈的静默失效点被堵死。

---

## 5. 收敛解析链：一个通用 layered-resolve

治「每加一维就抄一条链」。抽一个纯函数，backend / profile / model / thinking 全走它：

```go
// resolveLayered 按优先级返回第一个非零值。overrides 命中即消费（caller 传入
// 删除回调）。sources 从高到低：opts、一次性 override、resume-lock、profile、
// default。纯函数，锁语义留给 caller（沿用 resolveSpawnParamsLocked 的 r.mu）。
func resolveLayered(sources ...string) string {
    for _, s := range sources {
        if s != "" {
            return s
        }
    }
    return ""
}
```

> 不动锁、不动 resume-lock 的正确性不变量（那是热路径，review 过）；只把「opts > override > resume > profile > default」这条重复模式从「backend 抄一遍、profile 抄一遍」变成两处都调同一个函数。收益随维度数量线性放大——thinking effort 加进来时直接复用，不再新写第三条链。

---

## 6. 渲染层：thinking effort 落到 backend

治问题二。扩 `Caps`，让 backend 自报能力，protocol 自己翻译意图。profile 层零感知。

```go
// internal/cli/protocol.go
type Caps struct {
    // ...existing...
    Thinking bool // true=该 backend 能表达 thinking effort 意图
}

// 各 backend 自己翻译（示意）：
// protocol_claude.go: high → env MAX_THINKING_TOKENS=<档位>
// protocol_codex.go:  high → --reasoning-effort high
// 不支持的 backend：Caps.Thinking=false，resolve 出的意图被静默忽略（并在
// UI preflight 让 picker 灰掉该项，见 §7）——不静默塞一个无效 env。
```

调用点：`resolveSpawnParamsLocked` 解析出 `thinking` 意图后，交给 `wrapperFor` 选出的 backend 的渲染钩子，由它决定注入 env 还是追加 arg。**profile / config / envpolicy 三个包都不认识 `MAX_THINKING_TOKENS` 这个名字**——它只活在 `protocol_claude.go` 里。

---

## 7. UI 设计：选一次，不用懂

**总原则：用户面对的是人话的「意图」，永远不是变量名 / backend 内部表达。** 复杂度全部下沉到 backend 渲染层，UI 只暴露 2 个选择。

### 7.1 谁配 profile（一次性，管理员心智）

profile 的**创建**是低频管理动作，不该占据日常 UI。保持 v1 已有的 `POST /api/access-profiles` + 设置面板，但收敛表单为**模板优先**：

```
┌ 新建 Profile ───────────────────────────┐
│  用途    ◉ 个人 Anthropic 直连           │   ← 选模板，自动填好 base-url/键类型
│          ○ 公司 Bedrock（经本机代理）     │
│          ○ 订阅额度（Pro/Max，OAuth）     │
│                                          │
│  名称    [ 个人 Fable        ]  🟠 chip  │
│  凭证    [ 粘贴 token / 上传文件      ]   │   ← 落 0600 *_FILE，永不回显
│  ▸ 高级（默认 model / backend / thinking）│   ← 折叠，默认不展开
└──────────────────────────────────────────┘
```

- 模板 = 预填的 auth env 组合，用户**不输入任何变量名**，只粘贴一个 token。
- 「高级」折叠区放语义字段（default model / backend / thinking），有合理默认，绝大多数人不展开。
- token 只写不读：写入 `*_FILE`，UI 永不回显（v1 已如此，保留）。

### 7.2 谁用 profile（高频，日常心智）

日常只在两个既有入口，各加**最多一个**控件：

**(a) new-session picker**（扩展 v1 已有的 backend/profile picker）——两个下拉，人话标签：

```
用哪个？   [ 个人 Fable ▾ ]      ← profile，显示 DisplayName + chip 色
思考深度   [ 标准 ▾ ]            ← thinking：标准 / 深入 / 深度；backend 不支持则整行灰掉
```

- `思考深度` 直接映射 `thinking: ""/medium/high`（低档很少用，先不放）。
- **能力联动**：picker 选中的 profile→default backend 若 `Caps.Thinking=false`，`思考深度` 整行灰掉并提示「当前 backend 不支持」——不给用户设一个静默无效的选项（这正是 §6「不静默忽略」的 UI 兑现）。

**(b) session 卡片 chip**（扩展 v1 已有的 access-profile chip）——只读展示当前 session 用的 profile，thinking 非默认时在 chip 上加一个小角标（如 `⚡深度`），不加则不显示，保持卡片干净。

### 7.3 反例（明确不做，防止 UI 变复杂）

- ❌ 不在日常 UI 暴露任何 env 变量名 / `MAX_THINKING_TOKENS` / backend 内部表达。
- ❌ 不做「per-message 临时改 thinking」的入口——它属于 session 粒度，塞进每条消息旁边只会噪音。真要临时改，走既有的 slash / override，不占 UI。
- ❌ 不给 profile 做「复制/继承/分层组合」——profile 是扁平命名档，组合心智留给 project 绑定层（既有）。
- ❌ 「高级」区默认折叠；一个只想「换个账号」的用户全程只碰模板 + token + 名称三个字段。

---

## 8. 兼容 & 迁移

- **数据向后兼容**：`config.AccessProfile` 字段全保留，`Profile` 是超集；旧 config.yaml 零改动可加载。`Thinking` 空 = 不表态 = v1 行为。
- **API 兼容**：`/api/access-profiles`（GET/POST）形状不变，`Thinking` 作为可选字段追加。
- **解析兼容**：新 layered-resolve 对现有 backend/profile/model 链是**行为等价重构**（同优先级），有 §9 的 golden 测试守。
- **单-auth 部署零感知**：没配 profile 的部署继续走 settings.json 全局基线，本 RFC 全部是 opt-in。

---

## 9. 测试策略

1. **启动断言（§4）**：单测覆盖「overlay 注册表某 concrete key 不在 shim allowlist → panic」，把契约变成 CI 能抓的。
2. **注册表派生一致性**：断言 `OverlayAllowedKeys`/`overlayFileKeys`/`ValidateOverlayEntry` 三者与 `overlayKeys` 表逐项一致（防未来有人绕过表手改其一）。
3. **layered-resolve golden（§5）**：表驱动覆盖 opts>override>resume>profile>default 全部优先级组合 + override 一次性消费语义；断言重构前后对 backend/profile/model 三链行为逐字节等价。
4. **Caps 渲染（§6）**：`thinking: high` 在 claude 渲染为预期 env、在 codex 渲染为预期 arg、在 `Caps.Thinking=false` 的 backend 被忽略（不注入任何东西）。
5. **UI 能力联动（§7.2）**：picker 选中不支持 thinking 的 backend 时该行 disabled 的契约测试。

---

## 10. 风险与回滚

| 风险 | 缓解 |
|---|---|
| 解析链重构引回归（热路径） | §9.3 golden 行为等价测试；分 PR，本项可独立回滚 |
| 启动断言误伤（把合法 key 判成缺失） | 断言只读两个已存在的集合做交集，无新逻辑；先加断言再加新 key，两步分离 |
| `Thinking` 档位→claude 数值映射拍脑袋 | 映射表集中在 protocol_claude.go 一处，可调；档位少（3 档）易验证 |
| UI 能力联动漏判致用户设无效值 | 后端 resolve 仍会忽略无效意图（双保险），UI 灰掉只是体验优化 |

---

## 11. Rollout / Sprint

风险递增，每步可独立合并、独立回滚：

- **PR-1（纯收敛，零行为变化）**：envpolicy 单一注册表 + 三处派生 + shim 启动断言（§4）。**最该先做**，堵静默失效。
- **PR-2（等价重构）**：layered-resolve helper，backend/profile/model 三链改调它（§5）。golden 测试守。
- **PR-3（新维度）**：`Profile.Thinking` 字段 + `Caps.Thinking` + claude/codex 渲染（§3/§6）。
- **PR-4（UI）**：new-session `思考深度` 下拉 + 能力联动 + chip 角标（§7.2/7.3）。

PR-1/PR-2 是纯健康度改造（无新功能），PR-3/PR-4 才交付 thinking effort。可只做 PR-1 立刻止血，其余按需推进。

---

## 12. 决策点（待 owner 确认）

1. **是否现在就要 thinking effort**？若「暂不」，则只做 PR-1（+可选 PR-2），把架构准入规矩（§2/§3 判据）写进 envpolicy 包文档作为约束，PR-3/4 待触发。
2. **thinking 档位数**：3 档（标准/深入/深度）够否，是否要 low？
3. **claude 档位→`MAX_THINKING_TOKENS` 数值**：用哪组数值（需一次实测校准）。
4. **profile 创建入口**是否维持「设置面板 + 模板」形态，还是进一步简化为 CLI-only（管理员用命令建，UI 只消费）？
