# 工作笔记：Per-Project Access Profile（写 RFC v2 前的思路存档）

> **性质**: 非 RFC，是讨论过程的思路存档。RFC v1 在 `project-access-profile.md`；本文件记录 v1 之后三轮评审的全部发现 + 已拍板的方向决策 + v2 待补清单。写 v2 时以本文件为工作底稿。
> **创建**: 2026-07-09
> **状态**: 待写 v2

---

## 1. 需求（用户原话）

不同项目要有默认的、可配置的「访问方式」。三个真实项目：
1. **polyquant**（个人）→ 默认 **1P Anthropic 认证的 Fable 5**（直连 api.anthropic.com，不经本机 proxy）。
2. **POC JD**（公司）→ 默认 **Bedrock 的 Opus 4.8**（经本机 proxy）。
3. 某项目 → 根本**不用 CC，默认用 kiro**。

前两个都基于 cc，区别在认证链路 + model；第三个是换 backend。

---

## 2. 已核实的现状事实（带 file:line，三个后台调查的结论）

### 2.1 三个正交维度

| 维度 | 变的是 | 现状机制 | 项目默认？ |
|---|---|---|---|
| **认证/上游** | 1P 直连 vs Bedrock proxy；哪个 key | 全局 env（settings.json → shim 单快照） | ❌ 无任何粒度 |
| **backend** | wrapper 实现（stream-json / acp / app-server） | per-session `backendOverrides` + picker | ⚠️ 有 per-session，无项目默认 |
| **model** | `--model` 值 | 全局 `cli.model` / 项目 `PlannerModel` | ✓ 有 |

### 2.2 关键事实

1. **backend.Profile 不含任何认证字段**（`internal/cli/backend/profile.go:30-126`：`DefaultBinary / DefaultTag / NewProtocol / DetectInProc / RequiredNodeCaps / HistoryDir / CostUnit / Features`）。claude vs kiro 差异是 *wrapper*，不是账号/上游。→ 认证不能塞进 Profile。
2. **认证切换全靠 env**：naozhi 不显式「选」1P/Bedrock，只透传 `CLAUDE_CODE_USE_BEDROCK` / `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_BEDROCK_BASE_URL` / `AWS_REGION`，由 claude CLI 自解读（`envpolicy.DetectBackendFromEnv`，`internal/envpolicy/backend.go:58`）。
3. **无 per-session/per-agent env**：`AgentConfig`（`config.go:178-181`）只有 `Model`+`Args`；shim 用**进程级单快照** `m.shimEnv`（`internal/shim/manager.go:260`），每个子进程 `cmd.Env = m.shimEnv`（:479）。`SpawnOptions` 无 env 字段。**这是诉求 1/2 的唯一真障碍。**
4. **shim 白名单是精确 key，已放行全部需要的认证 key**（`manager.go` ~:1286-1393，安全加固 R214/R219-SEC-3 刻意放弃通配前缀）：`ANTHROPIC_BASE_URL=` / `ANTHROPIC_AUTH_TOKEN=` / `ANTHROPIC_BEDROCK_BASE_URL=` / `CLAUDE_CODE_USE_BEDROCK=` / `CLAUDE_CODE_SKIP_BEDROCK_AUTH=` / `AWS_REGION=` / `AWS_PROFILE=` …。→ access profile 只在此集合内覆盖 value，不放开白名单。
5. **父进程注入侧 `awsEnvDenyList`（`main_claude_settings.go:85-94`）故意屏蔽 `AWS_PROFILE` / `AWS_ROLE_ARN` / `AWS_SHARED_CREDENTIALS_FILE`**（防认证源劫持）。→ Bedrock 侧不靠切 AWS profile 区分账号。
6. **proxy 无 header 路由**，纯 Bedrock 网关（`127.0.0.1:8889`，`FORCE_FALLBACK=1` 东京 temp profile，fable 固定 us-east-1），无「直连 api.anthropic.com」旁路。→ 1P 链路不经过 proxy，直接指 api.anthropic.com；proxy 一行不改。
7. **`PUT /api/projects/config` 已存在**（`internal/dashboard/project/api.go:317`，带 `ValidateConfig` + rate limit），**但零 dashboard UI**——今天 project 配置只能经 CLI 会话内 `update_config` MCP 工具改。→ UI 后端地基已就绪。
8. **agentcore sandbox 已有 per-invoke env 先例**（`payload.Env map[string]string`）——per-call env overlay 在本仓已验证的形状。

---

## 3. 核心机制（v1 设计，评审确认「机制正确，不推翻」）

**access profile（暂定改名，见 §6-决策）= { env overlay（白名单内）, 默认 backend, 默认 model }**，与 backend 正交。

三条链各自独立解析，最后在 `resolveSpawnParamsLocked` 合并：
```
auth (env overlay):  请求显式 → per-session override → per-agent → per-project → 全局默认(现状 settings.json)
backend:             请求显式 → per-session backendOverride(已有) → per-agent → per-project → cli.defaultBackend
model:               请求显式 → per-session → per-agent → project(PlannerModel > profile.default_model) → backend.DefaultModel
```

**shim per-spawn overlay**（技术命门）：`m.shimEnv` 保留为**基线**，spawn 时 `cmd.Env = mergeShimEnv(baseline, overlay)`；overlay **不豁免任何 gate**，仍过 filterShimEnv 精确白名单 + SSRF/profile 校验，唯一多出的能力是「在已放行 key 上 per-spawn 覆盖 value」。

**secret**：`*_FILE` 间接引用宿主文件（不明文进 git）；spawn 时读文件注入对应非-`_FILE` key。

**安全基线（评审确认 solid，全部保留）**：复用 `envpolicy` 叶子校验器；不放开白名单前缀；resume 锁定历史档防串账。

---

## 4. 三轮评审的发现汇总

> 三个独立视角：功能完备性（两轮，第二轮带完整代码追踪）+ UX 友好度。核心共识：**机制正确、安全姿态 solid，但 RFC 默认了「单节点 + 只有用户 IM 会话」两个隐含前提，三个最大的洞全是「静默用错账号」——恰是威胁模型声称要消灭的那类。**

### P1（阻塞性，v2 必须解决）

**P1-a｜跨节点 overlay/secret 物理不可达（最大洞，已代码坐实）**
- reverse-RPC `send` handler（`internal/upstream/connector_rpc.go:107-160`）只反序列化 `{Key, Text, Workspace}`；`ClientMsg`/`ReverseMsg`（`internal/node/protocol.go:68-120`）带 `Backend`+`Workspace` 但**无 env 字段**。
- 远端节点用**自己的** `m.shimEnv` 基线 spawn = 远端主机的全局默认档，不是主节点解析的档。
- 绑 polyquant(1P) 的会话派到默认 Bedrock 的远端节点 → **静默串账，穿过节点边界**。
- `*_FILE` 是 host-local 的：路径在主节点解析，远端可能不存在或存不同 token。要么明文过网络（打脸「secret 不离开主机」），要么路径契约未定义。
- **决策（已拍板）**：缩范围到「access profile **仅本地派发**」。加一个类似 `selectNodeForBackend`（`internal/server/select_node_for_backend.go`）的 gate：拒绝把非默认档会话派到远端节点，fail-loud。overlay-over-wire + 远端重跑 filterShimEnv 留作未来。

**P1-b｜cron × access profile 未定义（已代码坐实）**
- `cron.Job`（`internal/cron/job.go:155`）有 `Backend` 字段但**无 `AccessProfile`**。
- cron 是 `cron:` 命名空间 exempt 长期会话（`internal/session/exempt.go:93 maxCronExempt`）；§7 resume-lock 会让**首次运行冻结档永久**，运维后续改项目档，存量 cron 静默留旧档，且 cron 不在 §8 面板范围。
- POC JD 必有 cron；polyquant 若配 cron 会用公司 Bedrock 跑个人任务 = 串账。
- **决策**：cron.Job 加 `AccessProfile` 字段（平行 `Backend`），创建时默认取绑定项目的档、之后冻结、cron UI 可见可改。runJob 路径显式走档解析，不绕过。

**P1-c｜sysession 独立 env 路径，overlay 够不着（已代码坐实）**
- sysession **不走 shim**：`internal/sysession/env.go:71` 自建 `cmd.Env`，走 `filterEnv` + 自己的 `envAlwaysPassthrough` + `--setting-sources ""` runner。
- §4 只改 `mergeShimEnv` 完全够不着。AutoTitler 给 1P 会话起标题会用全局 Bedrock 账号 + 可能挑该账号不存在的 model。
- `envAlwaysPassthrough` 透传 `ANTHROPIC_MODEL`/`ANTHROPIC_DEFAULT_*`，与 profile 靠 `--model` CLI arg 改 model 会 diverge。
- **决策**：v2 显式声明「access profile **仅用户 IM/dashboard 会话**；sysession（AutoTitler/dispatcher 等 `sys:` 线程）永远用全局默认档」，并注明 AutoTitler 成本/model caveat。若未来要 sysession 继承，是独立第二 overlay 路径 + 独立 PR。

**P1-d｜创建/编辑 UI 断崖（UX 最大洞）**
- §8 的抽屉/picker/chip **只做「引用已有档」，从不「创建档」**。创建仍要手写 config.yaml、懂 env 名、填 hex 颜色、建 0600 secret 文件。
- naozhi 定位是**个人装机工具——「运维」和「用户」是同一人**。第一次用就撞 YAML 墙。
- **决策（已拍板）**：dashboard 加「新建 access profile」引导表单：预置模板（「个人 Anthropic」/「公司 Bedrock」pre-fill env keys）+ token 文件选择器（自动写 `*_FILE` 并 chmod 0600）+ 自动分配颜色。写回 config.yaml（受信文件，需后端新增写端点）。

**P1-e｜飞书渠道空白（UX，认证维度使其严重）**
- §8 全是 dashboard。飞书群用户**看不到也切不了**自己这条对话在走个人 1P 计费还是公司 Bedrock = 计费 + 数据治理脚枪。
- **决策**：新增 §8.5 IM 渠道节。至少飞书卡片 footer 显示档 `display_name`（同 chip 的非敏感 label）。切换能力：v2 决定「只读跟随绑定 + 显示 label」，切换留 dashboard-only（显式声明）。

**P1-f｜错误反馈太晚（UX）**
- `*_FILE` 缺失只在 spawn 时报错，而 spawn 在**用户已打字发消息之后**。是可预检的静态条件。
- **决策**：选档时预检。`/api/access-profiles` 只读端点 `os.Stat` 所有 `*_FILE`，在 picker/预览里把「⚠ secret 文件缺失」的档灰显/内联警告，fail at picker 不 fail after message。

### P2（重要，v2 应解决）

**P2-1｜UI 三个平级下拉过度暴露（UX，最重要的模型简化）**
- profile 已捆 backend+model，用户心智只有「一个预设」。§8.1 却给 profile select + backend select + planner-model input 三个平级控件，交互经不可见的优先级 = 需要「生效链路预览」当解码器。
- 正交 internals ≠ 正交 UI。
- **决策（已拍板）**：profile 做**主控件且通常唯一**（渐进披露）；backend/model 折进「高级/覆盖」disclosure。95% 场景用户选一个「公司 Bedrock · Opus 4.8」就完事。

**P2-2｜非 general agent 静默丢 project config（功能，正确性）**
- `KeyResolver.ResolveForChat`（`internal/session/routing.go:194-201`）：非 general agent 只继承 `Workspace`，`PlannerModel`/prompt 刻意丢，`Exempt=false`。
- auth 不是 model——非 general agent 跑错账号是**正确性 bug 不是偏好**。
- `ProjectBinding`（`internal/projectapi/projectapi.go:27-33`）只带 `{Bound,Name,WorkspaceDir,PlannerModel,PlannerPrompt}`，要扩 `Backend`+`AccessProfile`，牵动 `projectapi_alias_test.go` 契约测试。
- **决策**：v2 显式定义非 general agent 是否继承项目档（倾向：auth 必须继承，与 model-drop 规则不同）。

**P2-3｜档定义变更 → idle shim 陈旧复用（功能）**
- §4.4 分片按 `(backend, accessProfileID)`。改了 `bedrock-opus` 的 base-URL 但保留 id，池里旧 env 的 idle shim 会被命中复用 → 错误上游无警告。
- 活会话同理：running 长期会话 spawn 前的 env 保持到进程死，config 改不重读。
- **决策**：分片 key 含 overlay 内容 hash（`(backend, profileID, envHash)`），改档 = cache miss，旧 shim 自然 drain。显式声明活会话 pin 档至 respawn，并暴露（见 observability）。

**P2-4｜observability 只 assert 未设计（功能）**
- §8.3 只有 UI chip；§10「静默降级」只列 warning。**无后端日志/metrics**记录「这次 spawn 实际用了哪个档 + 哪条链路」。chip 显示的是**配置**档，不一定是 §7 锁定 / §P2-3 stale-shim 复用后的**实际**档。
- **决策（已拍板）**：v2 新增独立 observability 章节。spawn 时 `slog.Info` 记 `{key, backend, access_profile_id, resolved_base_url_host, model, node}`（脱敏，绝不记 token）；`internal/metrics` 加 per-profile spawn counter；session-status 端点加 `resolved profile` 字段让 chip 反映现实；doctor 加 per-profile shim + spawn 计数。

**P2-5｜活会话重绑定 + 档变化未定义（功能）**
- §7 只讲 dead session resume 锁定。chat 从项目 A(1P) rebind 到项目 B(Bedrock)，活进程 env 已固定，继续用旧档直到重启；但 chip 若实时读 project binding 会显示 B（撒谎）。
- **决策**：一切 UI 一律读 **session record 的实际锁定档**（§7 已引入 `AccessProfileID` 字段），不读 project binding 当前值；rebind 后提示「新档下次会话重启生效」。

### P3（完备性/打磨，可 GA 后跟进，但 v2 应记录）

- **P3-1 remote-created session 绕过 resolver**：`connector_rpc.go:414-420` legacy inlined 路径直接从远端 `EffectivePlannerModel` 建 `AgentOpts`，只改主节点 resolver 会被绕过。→ 与 P1-a「仅本地派发」gate 一并处理。
- **P3-2 scratch/quick/passthrough 入口**：走同一 `AgentOpts`/`GetOrCreate`，**若解析下沉到 `resolveSpawnParamsLocked`（不是 `ResolveForChat`）则自动继承正确**。scratch 追问继承**父会话锁定档**（不重解析）；quick session 无项目绑定 → 全局默认档。→ 强化「解析下沉」的架构决策。
- **P3-3 codex 认证语义未验证**：overlay 的 env key 是 claude 语义（`ANTHROPIC_*`/`CLAUDE_CODE_USE_BEDROCK`），codex app-server 协议可能需不同 key。→ v2 声明「本轮仅验证 claude/kiro，codex 待验证」。
- **P3-4 model×backend 兼容性 + PlannerModel vs profile.default_model 同级裁决**：kiro model 是 `deepseek-3.2`/`glm-5`，claude 是 `claude-*`；档 default_model 用在错 backend 上无效。ValidateConfig 应交叉校验或 doctor 警告。裁决：显式 `PlannerModel` > `profile.default_model`。
- **P3-5 未知档硬失败 vs degraded fallback**：§10 当前设计未知档「spawn 明确报错不 fallback」会让整个项目会话起不来。→ v2 决策点：warn + fallback 全局默认（degraded 但可用）vs 硬失败。倾向 fallback + 显著 warn + observability 记录（因为有了 §P2-4 就能证明没静默）。
- **P3-6 display_name 内嵌 model 会让 chip 撒谎**：`"Bedrock · Opus 4.8"` 写死 model，但 model 可被 `planner_model` 独立覆盖。→ 档按账户/环境命名（「公司 Bedrock」），resolved model 单独显示；或 chip 的 model 部分反映 effective 值。
- **P3-7 "access profile" 命名撞 IAM**：改名到账户/环境语域——倾向「环境预设 / Environment」或「连接预设」。（**已拍板要改名**，具体词 v2 定。）
- **P3-8 生效链路预览混三种语域**：`1P · Fable 5 → api.anthropic.com → claude-fable-5` 把 marketing 名 + 裸 hostname + 内部 id 混一起，`Fable 5` 出现两次像 bug。→ 默认人话（「个人 Anthropic 账户，Fable 5，直连」），raw 细节折进「技术详情」。
- **P3-9 localStorage lastAccessProfile 风险**：auth=计费/数据路径，「sticky 上次账户」比 sticky backend 危险；且可能是已删除 stale id（同 sidebar activeFilter 孤儿崩溃类）。→ 无项目会话不持久化 auth 档 / 全局默认赢；加「仍存在」守卫。
- **P3-10 `*_FILE` 0600 只 warn 太松**：auth token world-readable 是真 finding。→ 考虑 reject 或 config 开关强制。
- **P3-11 §11.2「overlay 覆盖 settings.json」是 CLI 行为未实测**：cc child 走 `--setting-sources user` 在 CLI 内部读 settings.json 的 env，它与进程 env 的优先级是 **claude CLI 行为**，不是 naozhi 靠覆盖 cmd.Env 能定。→ v2 前**必须对实际 CLI 版本实测**（呼应记忆 `project_cli_debug_filter_noop`「CLI flag 行为须对实际版本实测」）。
- **P3-12 new-session picker 即使项目已绑定默认仍每次出现**：→ 项目有绑定档时折叠成单个可点 chip（「公司 Bedrock · Opus 4.8 — 更改」），点击才展开。

---

## 5. 已拍板的三个方向（写 v2 的地基）

1. **跨节点**：缩范围到「access profile 仅本地派发」+ gate 拒绝远端非默认档会话（诚实的 v1）。overlay-over-wire 留未来。
2. **创建 UI**：dashboard 引导表单（预置「个人 Anthropic」/「公司 Bedrock」模板 + token 文件选择器自动 chmod 0600 + 自动配色），需后端新增 config 写端点。
3. **UI 模型**：从「三个平级下拉」重构为「一个预设主控 + 高级覆盖 disclosure」。

配套已定：
- **改名**：避开 IAM 歧义（v2 定具体词，倾向「环境预设 / Environment」）。
- **解析下沉**：profile 解析放 `resolveSpawnParamsLocked`（尽可能低），让 scratch/quick/passthrough/cron 等所有走 spawn 的入口自动继承，而不是只在 `ResolveForChat`。
- **新增 observability 独立章节**。
- **一切 UI 读 session record 实际锁定档**，不读 project binding 当前值。

---

## 6. v2 相对 v1 的结构增补清单（写 RFC 时逐条落）

- [ ] 全文改名（access profile → 定稿词），保留一句「旧称 access profile」注脚。
- [ ] §1.3 范围：显式声明「仅本地派发」「仅用户 IM/dashboard 会话，不含 sysession」「本轮仅验证 claude/kiro」。
- [ ] §4.4 分片 key 改 `(backend, profileID, envHash)`；显式活会话 pin 至 respawn。
- [ ] 新增 §4.5「跨节点：仅本地派发 + 远端非默认档 gate（fail-loud）」，点名 `connector_rpc.go:107/414`、`node/protocol.go:68`、`select_node_for_backend.go`。
- [ ] §6.3 解析下沉到 `resolveSpawnParamsLocked`（不是 ResolveForChat）；`ProjectBinding` 扩 `Backend`+`AccessProfile`（点名 `projectapi.go:27` + 契约测试）；覆盖 remote-created 第二构造点。
- [ ] 新增 §7.1「cron 档继承」：`cron.Job` 加 `AccessProfile`，创建时取项目档、冻结、cron UI 可见。
- [ ] 新增 §7.2「sysession 档语义」：永远全局默认 + AutoTitler caveat。
- [ ] 新增 §7.3「活会话重绑定 + 档变化」：UI 读 session record 锁定档。
- [ ] 新增 §7.4「特殊会话入口继承」：scratch=父会话锁定档、quick=全局默认、passthrough=同会话同档。
- [ ] 重写 §8 UI：主控件=预设 + 高级覆盖折叠；新增「创建/编辑档」引导表单 + 模板 + token 文件选择器；预览改人话 + 技术详情折叠；chip 用 resolved model；picker 项目已绑定时折叠成 chip。
- [ ] 新增 §8.5「IM/飞书渠道」：卡片 footer 显示档 label；切换 dashboard-only（显式）。
- [ ] §8.x 选档预检 `*_FILE`（fail at picker）；localStorage stale-id 守卫 + 无项目会话不 sticky auth。
- [ ] 新增独立「可观测性」章节：spawn slog + per-profile metric + session-status resolved 字段 + doctor。
- [ ] §10 风险表补：未知档 fallback vs 硬失败决策（倾向 degraded fallback + warn + observability）；`*_FILE` 0600 reject/开关。
- [ ] §11.2「overlay vs settings.json 优先级」标注「**待对实际 CLI 版本实测**」，不 assert。
- [ ] §12 Sprint 增补：跨节点 gate、cron 字段、创建 UI 表单 + config 写端点、observability 各自成 PR。
- [ ] §13 决策点更新：未知档 fallback 策略、改名定稿词、飞书是否可切档。

---

## 7. 一句话结论

核心 env-overlay 机制正确、安全姿态 solid，无需推翻。v2 的全部工作是**把两个隐含前提（单节点 / 只有用户 IM 会话）显式化**，对三个「静默用错账号」的洞（跨节点/cron/sysession）各给一个**显式 scope 决策 + fail-loud gate**，并把 UI 从「实现者的三维模型」收敛成「用户的一个预设」+ 补齐创建/飞书/预检三块 UX 断层。
