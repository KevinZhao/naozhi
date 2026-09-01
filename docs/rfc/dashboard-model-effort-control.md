# RFC: dashboard 会话级 model / effort 切换（header chip 控制面）

- 状态：Draft v1
- 日期：2026-09-01
- 作者：Kevin Zhao
- 范围：在 dashboard 会话 header 的现有 model / effort chip 上提供运行中切换能力，
  per-session 覆盖并持久化；不做 backend 切换，不改 new-session 弹窗
- 前置：`kiro-effort-visibility.md`（chip 只读透出）、`kiro-effort-control.md`
  （配置驱动的 effort 链路，本 RFC 解除其 NG1/NG2）

## 1. Background & problem

`kiro-effort-visibility.md` 让操作者能**看见**当前会话的 model / effort，
`kiro-effort-control.md` 让部署者能在 `config.yaml` 里**配置**它们。但两者都
回答不了操作者最高频的诉求：**"这个会话，现在，换个模型 / 档位"**——

1. 改配置 + 重启 naozhi 影响所有会话，粒度错了；
2. 一个长期 planner 会话在"日常用 sonnet 省钱、攻坚时切 opus"之间来回，
   目前唯一路径是去改 kiro 全局设置或 naozhi 配置，都属于运维动作而非会话交互；
3. effort-control RFC 当时把 dashboard 选择器列为 Non-goal（NG1/NG2），理由是
   "effort 是启动参数、运行时改档必然 reset+respawn、交互重"。**下述实测证明
   该前提对 model 已不成立，对 effort 依然成立但代价可控**——本 RFC 正式解除
   这两条 NG。

### 实测事实（2026-09-01，kiro-cli 2.20.2 / claude 2.1.251）

手工逐条验证。kiro：ACP 会话（initialize → session/new → 目标 RPC → 最小
prompt → 读 `_kiro.dev/metadata` 与落盘 session 文件）；claude：stream-json
会话（`-p --input-format stream-json` → user 消息 / control_request → 读
`system/init.model`、assistant `message.model` 与 control_response）：

| # | 场景 | 结果 |
|---|---|---|
| F1 | 活会话 `session/set_model {sessionId, modelId}` | **成功**（返回 `{}`），随后 `session/prompt` 正常完成。ACP 有正式的运行时改模型 RPC，**不需要重启进程** |
| F2 | set_model 后关进程 → 新进程 `session/load` | **模型弹回默认值**。set_model 不写入 `~/.kiro/sessions/cli/<sid>.json`（文件中无新模型痕迹），效果**绑定进程而非会话** |
| F3 | 新进程带 `--model <m>` 做 `session/load` resume | **生效**：`result.models.currentModelId` = flag 值。与 `--effort` 的 resume 行为（effort-control RFC §1）完全同构 |
| F4 | 活会话 `session/set_config_option {configId:"effortLevel"}` | **失败**：默认 v2 engine 返回 `-32601 Method not found`。二进制代码显示 `effortLevel` config option 只存在于 v3 engine（KAS），naozhi 用默认 engine → **effort 无运行时通道** |
| F5 | `session/new` 响应 `result.models.availableModels` | 返回完整模型清单（20 项，含 `modelId`/`name`/`description`）。naozhi 每次 spawn 已收到这份数据，当前 `ACPSessionNewResult`（`rpc_types.go:131`）未定义该字段、直接丢弃 |
| F6 | claude 活会话发 `{"type":"control_request","request_id":…,"request":{"subtype":"set_model","model":"opus"}}` | **成功**：`control_response {subtype:"success"}`，下一轮 `system/init.model` 与 assistant `message.model` 均变为 opus。与 naozhi 已在用的 interrupt 同通道、同信封格式（`protocol_claude.go:424`） |
| F7 | claude set_model 请求受限模型（本环境 `haiku`） | 被组织策略拒绝：`control_response {subtype:"error", error:"Model \"haiku\" is restricted by your organization's settings. Using claude-sonnet-5 instead."}`。**策略拒绝是正常返回路径**，UI 必须透出并回滚 chip |
| F8 | claude set_model 的持久性 | **与 kiro（F2）相反，落盘**：切到 opus 后新进程 `--resume`（不带 `--model`）仍是 opus；带 `--model sonnet` 则 flag 压过会话状态。naozhi "override 落盘 + respawn 必重放 flag" 的设计恰好把两端行为拉齐 |
| F9 | kiro `--effort low` 启动 → set_model 切到另一模型 | **effort 被重置为新模型的默认档**：set_model 后 metadata 上报 `effort:"high"`，且随后真实 prompt 全程保持 high——显式 `--effort` flag 被静默丢弃。二进制代码印证：模型条目携带 `defaultEffortLevel`，切模型即应用 |
| F10 | kiro set_model 传无效 `modelId:"no-such-model-xyz"` | **静默返回成功 `{}`**；失败推迟到下一次 prompt（`-32603 "The model 'no-such-model-xyz' is not available"`，整轮报错）。模型 ID 校验必须在 naozhi 侧做 |
| F11 | kiro set_model 的确认信号 | set_model 后 kiro 主动推一帧 `_kiro.dev/metadata`（含 effort，**不含 model**）。model 无 wire 回显 → 确认只能以 RPC 成功为准；effort 的连带变化（F9）恰好经 visibility 链路对操作者可见 |
| F12 | kiro `session/load` 响应 | 与 session/new 一致返回 `models.availableModels`（20 项）+ `currentModelId` —— 清单缓存在 fresh spawn 与 resume 两条路径都能刷新 |
| F13 | kiro **mid-turn** set_model（prompt 进行中 1.5s 后发） | RPC **立即**返回 `{}`；进行中的 turn **不被打断**，chunk 流继续、正常 `end_turn`；新模型对后续 turn 生效 |
| F14 | claude **mid-turn** set_model（turn 进行中 2.5s 后发） | ack 在 turn 进行中返回（7.5s 后）；**当前 turn 剩余输出直接切到新模型**（assistant `message.model` 已是 opus），turn 不中断、正常完成。即 claude 的语义是"尽快生效、可及当前回合"，不是"下一轮生效" |
| F15 | claude set_model 传未知模型名 / 完整规范 id | 未知名同 F7 走 error ack（复用 restricted 文案）——**claude 自带校验，不会静默吞**（与 kiro F10 相反）；完整 id（`claude-opus-5`）与别名（`opus`）均接受 |

五条结论：

- **model：运行时可切，但 kiro 侧不持久。** 切换 = 活进程发 RPC（即时）+ naozhi 落盘
  override（respawn 时经 `--model` 重放）。claude 侧虽自带持久（F8），但 naozhi 不依赖
  它——统一重放 flag 让两端行为一致，也覆盖"override 改过之后 resume"的场景。
- **kiro 切模型会连带重置 effort（F9）。** `session/set_model` 把 effort 拉回新模型的
  默认档，显式 `--effort` 被静默丢弃。因此 **kiro 的 model RPC 路径必须在 set_model
  之后立即跟一次 effort 恢复**——而 v2 engine 没有 effort 的运行时通道（F4），恢复
  只能靠 respawn。设计后果见 §4.4：当会话有生效的 effort（配置链或 override 非空）时，
  kiro 切模型直接走 respawn 路径（一次重启同时携带 `--model` + `--effort`），RPC
  快路径仅用于"无 effort 约束"的会话；claude 无 effort 概念，不受影响。
- **effort：只能经由 respawn。** 优雅关闭 → 带新 `--effort` spawn → `session/load`
  resume，上下文保留（F3 同构路径已验证）。naozhi 的 TTL 回收 + 自动 resume
  机制已把这条路走通，增量只是"主动触发一次"。
- **校验与拒绝都在 naozhi/UI 侧兜底。** kiro 对无效模型静默吞掉（F10）、claude 有
  组织策略拒绝（F7）——前者靠 naozhi 白名单（清单内选择 + 手动输入校验），后者靠
  透出 control_response 错误并回滚 chip。
- **模型清单免费。** 不需要新的枚举端点，透传 `session/new` / `session/load`
  已有数据即可（F5/F12）。

## 2. Goals & non-goals

### Goals

- G1：会话 header 的 model label 与 effort chip 升级为可点击 popover，
  运行中切换 model（两 backend）与 effort（仅 kiro）。
- G2：per-session override 持久化到 sessions.json，跨 TTL 回收、跨 naozhi
  重启均不弹回；优先级压过配置链全部层级。
- G3：切换结果按路径可确认：claude model = control_response ack（F6/F7）、
  kiro model RPC = RPC 成功（F11：无 wire 回显，以 F1 的行为证据背书）、
  respawn 路径 = 重启后 `system/init` / `_kiro.dev/metadata` 回读。前端
  pending 态只到对应确认信号为止，不做乐观转正。
- G4：不切换时行为与今天逐字节一致；未配置 override 的会话走原配置链。

### Non-goals

- NG1：**不做 backend 切换**（用户明确划定）。resume 历史绑定 backend 的
  session 存储格式，跨 backend 切换等价于丢上下文重开，交互价值为负。
- NG2：**不改 new-session 弹窗**（用户明确要求保持简化）。创建时用配置链
  默认值；要换，创建后在 header 切，路径统一。
- NG3：不做 v3 engine（KAS）的 `effortLevel` 运行时通道。补充实测
  （2026-09-01，`--agent-engine v3 --auth-method cli`，KAS 0.54.8）：v3 可
  初始化且 `session/set_config_option` 方法存在（不再 -32601），configOptions
  含 mode/model/autopilot 等运行时开关——**通道在 v3 真实存在**。仍然排除的
  原因是集成面：KAS 是独立 agent server，要求 client 应答反向请求
  （`_kiro/auth/getAccessToken`、`_kiro/terminal/shell_type`——空应答会导致
  session/new 内部错误）、session id 换为 `sess_` 前缀格式、agent/mode 体系
  不同（vibe/spec/plan/…），naozhi 的 ACP 客户端需要一轮独立适配。留待
  kiro v3 转正后作为 effort 免重启的升级路径，届时 §4.4 的 respawn 行可
  整行替换为 RPC。
- NG4：首版 override API 仅支持 local 节点。远程会话的 popover 置灰并提示，
  与 git chip 只支持 local 的先例（`fetchGitState`）一致。节点协议转发留待后续。
- NG5：不做 per-message 模型（"这一条用 opus"）。override 是会话状态，
  语义与 IM 端一致。
- NG6：不为 codex 做任何事（无 set_model 等价物证据，模型清单来源不明）。
  codex 会话两个 popover 均不出现。

## 3. Alternatives considered

### 位置：方案 A（选定）header chip popover

model label（`renderMainShell` 的 `.model-label`）与 effort chip
（`#header-effort`）本来就是操作者确认当前值的地方（visibility RFC 的闭环
手段），点它来改是零学习成本；header = 当前会话，per-session 语义自明。
视觉增量控制为 hover-reveal caret（复用 `.btn-rename` 手法），尊重 header
的减法历史（backend chip / cost chip / ctx-bar 均因争夺注意力被移除）。

### 位置：方案 B（否决）composer 加按钮

input-row 已有 attach / mic / hold-talk / send / stop 五个控件，移动端已挤。
且放在"发送"旁边暗示 per-message 语义（NG5），误导。

### 位置：方案 C（否决，被用户明确排除）new-session 弹窗加 picker

创建时选定本是技术上免费的时机，但用户要求 new-session 保持简化。创建后
在 header 切换覆盖同一需求，且只维护一条切换路径。

### 位置：方案 D（否决）全局设置面板

作用域错误：model / effort 是 per-session 的，全局默认值的修改本来就该留在
`config.yaml`（保持配置自描述原则，effort-control RFC §1 问题 3）。

### 机制：respawn-only vs RPC+respawn 混合（选定后者，带 F9 限定）

全部走 respawn（model 也重启）实现最简单——一条路径覆盖两个字段。否决：
F1 证明 model 有免重启通道，respawn 对活跃会话意味着丢弃当前 CLI 进程的
warm state（MCP 连接、文件缓存），且"切个模型要等重启"的体感明显更差。
代价是 model 走双路径（RPC + 落盘重放），用 G3 的确认回路兜住不一致。

**F9 限定**：kiro 的 set_model 会把 effort 重置为新模型默认档，所以 RPC
快路径只对"无生效 effort"的 kiro 会话开放；有 effort 约束的 kiro 会话切
模型退回 respawn（详见 §4.4）。这不推翻混合方案——claude 会话与无 effort
约束的 kiro 会话（预期是多数：effort-control 是可选配置）仍然免重启。

## 4. 设计

### 4.1 交互（前端）

```
claude-code · sonnet-4.6 ▾    [git]    xhigh ▾    3.2s
              └ click ┐                └ click ┐
   ┌──────────────────┴───┐   ┌────────────────┴──┐
   │ 模型                  │   │ Effort            │
   │ ● claude-sonnet-4.6  │   │   low             │
   │ ○ claude-opus-4.7    │   │   medium          │
   │ ○ claude-haiku-4.5   │   │   high            │
   │ ○ …                  │   │ ● xhigh           │
   │ ─────────────────    │   │   max             │
   │ ⓘ 下一轮生效          │   │ ─────────────────  │
   └──────────────────────┘   │ ⓘ 将重启 CLI 进程  │
                              │   并恢复上下文     │
                              └───────────────────┘
```

- **Affordance**：默认视觉零增量；hover 时 chip 尾部浮现 `▾` caret。
  移动端（无 hover）chip 直接可点。
- **三态**：`当前值` →（点击后）`新值` 半透明斜体 + "切换中…" tooltip →
  确认后转正。确认信号按路径不同：claude model = control_response success
  ack（F6；error ack = 策略拒绝，F7，chip 回滚并 toast 透出 CLI 原文
  "restricted by your organization's settings"）；kiro model RPC 路径 =
  RPC 成功即转正（F11：无 wire 回显，naozhi 更新 `SessionView.Model` 广播）；
  respawn 路径（effort、带 effort 的 kiro model）= respawn 后
  `_kiro.dev/metadata` / `system/init` 回读，**不做前端乐观转正**——弹回可见。
- **提示文案按实际路径动态化**（F9/F14 的直接后果）：model popover 的 ⓘ 行
  分三种——无 effort 的 kiro "下一轮生效"（F13）；claude "立即生效，可能
  影响当前回合"（F14）；有 effort 的 kiro "将重启 CLI 进程并恢复上下文
  （保持 effort 档位）"。API ack
  返回实际采用的路径（`applied_via: rpc | respawn | deferred`），前端据此
  渲染，不在 JS 里复刻判定逻辑。
- **运行中防护**：`s.state === 'running'` 时，走 RPC 路径的 model 切换保持
  可用，但生效时机两端不同（均实测）：kiro = 当前 turn 不受影响、下一轮
  生效（F13）；claude = **可及当前回合**——ack 与切换发生在 turn 进行中，
  剩余输出直接换模型，turn 不中断（F14）。菜单项 hover 提示相应区分
  （kiro "下一轮生效" / claude "立即生效，可能影响当前回合"）。走 respawn
  路径的切换（effort、带 effort 的 kiro model）点击弹确认"将中断当前回合并
  重启会话进程（上下文保留）"。这正面回应 effort-control RFC 否决方案 C 时
  的担忧（"误以为当前这轮就会变"）——现在两端的真实时机都写在 UI 上。
- **pending 时长预期**：claude 的 ack 在 mid-turn 场景实测 7.5s 才返回
  （F14，CLI 在流式输出间隙处理 control 队列）——三态的"切换中…"必须容忍
  秒级驻留，不设短超时误报失败；服务端 SetModel 的 ack 等待超时对齐
  interrupt 通道的既有值。
- **Capability gating**：effort popover 仅当会话 backend 上报
  `Caps.EffortTier` 时挂载（claude / codex 的 effort chip 本来就 `:empty`
  折叠，行为自洽）。model popover：kiro / claude 出现，codex 不出现（NG6）；
  远程节点会话两者置灰（NG4）。
- **`(模型未配置)` 态**：spawn 窗口期 model label 的 unset 占位同样可点，
  popover 列表来自 backend 清单缓存——这反而是操作者最想切的时刻之一。

### 4.2 模型清单

- **kiro**：`ACPProtocol` 的 `session/new` / `session/load` 结果解析新增
  `models.availableModels`（F5/F12，两条路径均返回 20 项清单），存入
  wrapper 级缓存（进程级足够：清单随
  kiro 版本变，不随会话变）。经 `/api/cli/backends` 的 `BackendInfo` 新增
  `models []` 字段透出——该端点已被 new-session picker 消费且已是
  node-aware（`ext/cli/handler.go` 的 NodeAccessor 代理），popover 直接复用
  `cliBackendsByNode` 缓存。冷启动（还没 spawn 过任何 kiro 会话）时清单为空，
  popover 显示"清单在首次会话后可用"+ 手动输入框兜底。
- **claude**：静态别名 `sonnet / opus / haiku` + `cli.backends[].models`
  配置可声明的补充清单（别名与完整规范 id 均被 CLI 接受，F15）+ popover
  手动输入框。claude 对未知模型自带 error ack 校验（F15），naozhi 侧只做
  `validateModelString` 防注入格式校验，不需要清单比对——与 kiro（F10
  静默吞、必须比对）刻意不对称。
- **effort**：闭集 `{low, medium, high, xhigh, max}` 硬编码，与
  `validateEffortString` 同源。

### 4.3 override 的数据流与持久化

新增 API：

```
POST /api/sessions/override   {key, node?, model?, effort?}
```

服务端处理（Router 新增 `SetSessionTuning(key, model, effort)`）：

1. **校验**：model 走 `validateModelString`（flag-injection 防线，
   R215-SEC-P2-1 同源）；**kiro 目标额外对照清单缓存**——kiro 对无效
   modelId 静默返回成功、失败推迟到下一次 prompt 且整轮报错（F10），
   naozhi 是唯一能提前拦截的位置。清单未命中时（冷启动 / 手动输入）放行
   但 `slog.Warn`，不硬拒——清单可能滞后于 kiro 新模型（R4）。claude 目标
   不做清单比对：CLI 自带 error ack 校验（F15），错误会经 F7 的拒绝路径
   回到 UI。effort 走 `validateEffortString` 闭集；effort 非空时校验目标
   backend `Caps.EffortTier`（对 claude 会话设 effort → 400）。
2. **记录 override 并落盘**：`ManagedSession` 新增 `TuningModel` /
   `TuningEffort` 两个持久字段（sessions.json）。与 `backendOverrides` 的
   one-shot 语义**刻意不同**——backend pick 消费一次即弃（Reset 后不应粘住），
   而 tuning 是操作者对"这个会话此后如何跑"的持续声明，必须跨 respawn 存活。
   这也回答了 effort-control RFC NG3 的落盘判断标准："model 落盘是因为存在
   运行时输入"——dashboard override 正是 effort 的第一个运行时输入，落盘
   随之成立。
3. **应用**（见 4.4）。

`resolveSpawnParamsLocked` 的 model / effort 链各加最高一层：

```
model:  cli.model ← backends[].model ← access_profile.default_model
        ← opts.Model(agent/planner) ← session.TuningModel   ← 新增，最高
effort: cli.effort ← backends[].effort ← agents[].effort
        ← session.TuningEffort                               ← 新增，最高
```

放最高层的理由：override 是操作者对**这一个会话**的显式动作，比任何配置
（部署者对**一类会话**的声明）都更具体。

**清除语义**（popover "恢复默认"项）：置空 override，随后**等价于把值设为
配置链解析结果**，走 4.4 同一套应用矩阵（活进程 RPC / respawn / suspended
记录）。两个边界：

- 配置链解析非空：直接按该值应用，行为与普通切换一致。
- 配置链解析为空（部署未配置 model，让 CLI 用自身默认）：kiro 侧走 respawn
  即可回落——F2 证明 kiro 不持久，不带 `--model` 的 resume 自然回到 kiro
  默认。**claude 侧做不到**：F8 证明 set_model 粘在会话状态里，不带
  `--model` 的 resume 仍保持上次切换值，且"CLI 自身默认"对 naozhi 不可知、
  无法显式 set 回去。处理：如实呈现——清除成功但 chip 保持当前值，
  toast 提示"已清除覆盖；当前模型将保持到配置指定或再次切换"。不做
  假装回落的乐观 UI。

### 4.4 应用路径（按字段 × 进程状态）

| 字段 | 进程存活 | 进程已回收（suspended） |
|---|---|---|
| model / kiro，**会话无生效 effort** | 发 `session/set_model` RPC（F1，快路径）；失败则降级为"记 override + 提示下次生效" | 仅记 override；下次消息 spawn 时经 `--model` 注入（F3） |
| model / kiro，**会话有生效 effort** | **走 respawn 路径**（同 effort 行）：F9 证明 set_model 会把 effort 重置为新模型默认档且 v2 engine 无恢复通道，RPC 快路径会静默丢档。一次重启同时携带新 `--model` + 原 `--effort` | 同上（spawn 本来就同时带两个 flag，无此问题） |
| model / claude | 写 `set_model` control_request（F6）；`control_response {subtype:"error"}`（组织策略拒绝，F7）→ 透出错误 + 回滚 chip + **不落 override**；写失败降级同 kiro | 同 kiro suspended，`--model` + `--resume`（flag 压过 claude 自身落盘的会话模型，F8） |
| effort / kiro | 记 override → 优雅关闭（复用 TTL 回收的 Close 路径）→ 立即带新 `--effort` respawn + `session/load`。正在跑 turn 时先走确认弹窗（4.1） | 仅记 override；下次 spawn 生效 |

"会话有生效 effort" = resolveSpawnParamsLocked 的 effort 链（4.3）解析结果
非空——含配置链任意层与 TuningEffort。判定在 `SetSessionTuning` 内做，
复用同一解析函数，不另写第二份优先级逻辑。

Protocol 接口新增 `SetModel(rw *JSONRW, sessionID, model string) error`：
ACP 发 RPC，stream-json 发 control_request 并等 control_response（错误 ack
原样上抛，携带 CLI 的策略拒绝文案），codex 返回
`ErrSetModelUnsupported`（沿 `Interrupt` 的 `ErrInterruptUnsupported` 先例）。

**确认回路**：kiro model 无 wire 回显（F11：set_model 后的 metadata 帧不含
model），RPC 成功即视为生效——F1 已证 RPC 成功 ⇒ 下一轮真实使用新模型，且
F10 的无效值风险已由 4.3 的清单校验前置拦截；naozhi 主动更新
`SessionView.Model` 并广播。claude 侧以 control_response success ack 为准
（F6），error ack 回滚。effort 与 respawn 路径的 model 完全被动——等
respawn 后首帧 `_kiro.dev/metadata` / `system/init` 回读，弹回可见。

### 4.5 arg-drift 同步（最高风险项，与 effort-control RFC §4.5 同源）

`router_shim.go` 的重启后参数漂移检测用 `backendDefaultsFor`（backend 层）
重建 argv 与 shim state 里存的 argv 比较。session tuning 落在 argv
（`--model` / `--effort`）后，**必须让 drift 重建路径读到同一份 override**，
否则每次 naozhi 重启都会把带 override 的存活 kiro 会话误判漂移并重启——
操作者可见为"切过模型的会话，naozhi 一重启就全部丢进程"。

与 `agents[].effort` 的既存 drift 限制不同（那是 agent 层数据在 drift 比较
时不可得，effort-control RFC §4.5.1 决定如实记录不修），**session tuning
在 drift 检查点是可得的**（`r.ss.sessions[key]` 就在手边），没有理由留同样
的坑。实现上 drift 重建必须叠加 `TuningModel` / `TuningEffort` 后再比较。
这是本 RFC 第一优先级的回归测试。

**语义确认**：操作者改了 override 且进程存活期间 naozhi 重启——此时 model
的 drift 判定要求 argv 与 override 一致，而活进程可能是"argv 旧 + RPC 已切"
状态。处理：drift 比较统一以 override 后的期望 argv 为准；判为漂移则重启
进程，重启本身让 override 经 flag 生效，结果正确（等价于 effort-control
RFC §4.5.1 末段"drift 是正确的而非误判"）。

### 4.6 校验与安全

- 两个字段均服务端校验（4.3 第 1 步），dashboard 输入不可直达 argv。
- API 沿用 `ValidateSessionKey` 的 key 校验门（R175-SEC-P1 同源）与
  dashboard 既有 auth / CSRF。
- override 落盘值在 sessions.json 加载时再过一遍校验：手改文件注入
  `-flag` 形态的值必须被拒（load-time 校验先例：`validateModelString`
  已用于 config 加载）。

## 5. Test strategy

| 层 | 测试 | 参照 |
|---|---|---|
| **arg-drift** | **带 TuningModel/TuningEffort 的会话重启 naozhi 不误判漂移**；改 override 后重启判定为漂移且重启后生效 | `effort_drift_parity_test.go` |
| 优先级 | TuningModel 压过 opts.Model / profile.DefaultModel / backend 层；TuningEffort 压过 agents[].effort；空 override 逐层回落 | `resolveSpawnParamsLocked` 现有 model/effort 表驱动测试 |
| 持久化 | override 写入 sessions.json；重启加载后 spawn argv 含 `--model`/`--effort`；load-time 拒绝 `-injected` 值 | store 现有 round-trip 测试 |
| 协议 ACP | `SetModel` 发出正确 `session/set_model` wire；session/new + session/load 结果均解析 `availableModels`（F12）；解析失败不阻塞 Init | `cli_test.go` ACP wire 测试 |
| 协议 stream-json | `SetModel` 产出 `control_request {subtype:set_model,"model":…}`；success ack 关联；**error ack（策略拒绝，F7）上抛且携带 CLI 原文** | `cli_test.go:422` control_request 测试。wire 行为已真 CLI 实测（F6/F7），单测锁形状即可 |
| 协议 codex | `SetModel` 返回 ErrSetModelUnsupported | Interrupt 同款测试 |
| **F9 路径选择** | 有生效 effort 的 kiro 会话切 model → 走 respawn（argv 同时含新 `--model` + 原 `--effort`）；无 effort → 走 RPC 且不重启进程；claude 始终 RPC | 新增 `SetSessionTuning` 表驱动测试 |
| mid-turn | running 会话走 RPC 路径切 model 不触发 interrupt / respawn（F13/F14 已证 CLI 侧安全）；ack 等待超时对齐 interrupt 通道既有值 | process_send 现有 control 通道测试 |
| 清除语义 | 清除 override 后按配置链解析值重放；配置链为空 + kiro → respawn 回落 CLI 默认；配置链为空 + claude → 保持现值不假装回落（F8） | 新增 |
| 校验 | kiro model 不在清单 → 放行 + Warn；`-injected` 形态被拒；claude+effort → 400；claude error ack → override 不落盘（F7） | dashboard handler 现有 envelope 测试 |
| API | 校验拒绝（非法 model、claude+effort、坏 key）；成功路径记 override + ack 返回 `applied_via` | 同上 |
| capability gate | claude 会话 effort popover 不挂载；codex 两者不挂载；remote 会话置灰 | `applyFeatureGates` 现有测试 |
| effort respawn | 活会话切 effort → Close + resume 带新 `--effort`，session_id 不变，事件历史连续 | router 现有 resume 测试 |

## 6. Risk & rollback

| # | 风险 | 缓解 |
|---|---|---|
| R1 | **drift 误判丢会话**（4.5） | 同步实现 + 第一优先级回归测试 |
| R2 | set_model RPC 成功但实际未生效（kiro 未来版本语义变化，或 F10 类静默吞值再现） | naozhi 侧清单校验前置拦截 + G3 确认回路不做乐观转正；弹回在 UI 可见 |
| R3 | ~~claude set_model wire 格式与静态证据不符~~ **已解除**：2026-09-01 真 CLI 实测通过（F6/F7/F8） | — |
| R4 | 模型清单陈旧（kiro 升级后新模型不在缓存） | 清单随每次 session/new / session/load 刷新（F12）；popover 手动输入兜底；未命中放行 + Warn（4.3） |
| R5 | 操作者在 IM 端与 dashboard 并发操作同一会话 | override 写入走 r.mu，last-write-wins；确认回路让最终态可见 |
| R6 | effort respawn 与消息队列竞争（respawn 期间新消息到达） | 复用现有 suspended→spawn 路径：respawn 期间消息进 per-session 队列，spawn 完成后 drain——机制已存在，加一条测试 |
| R7 | F9 的"有无生效 effort"判定与 spawn 实际行为漂移（两处各算一遍） | 判定复用 resolveSpawnParamsLocked 的同一解析（4.4），表驱动测试锁 parity |
| R8 | claude 组织策略拒绝后 override 半落盘（记了 override 但 CLI 拒了） | 顺序约束：先发 control_request 等 ack，success 才落 override（4.4）；respawn/suspended 路径无此问题（spawn 失败即整体失败） |

**Rollback**：纯增量。API 不调用则零行为变化；sessions.json 新字段
`omitempty`，旧版本 naozhi 读带新字段的文件时忽略未知字段（json 默认行为），
无迁移。`git revert` 即可。

## 7. Observability

- 生效确认本身就是可观测闭环（G3）：model 看 header label，effort 看
  effort chip——两个 visibility 链路都已在生产。
- 离线核对：override 在 sessions.json 可读；respawn 后 argv 落
  `~/.naozhi/shims/<hash>.json` 的 `cli_args`（effort-control RFC §7 同款）。
- `SetSessionTuning` 与应用路径各打一条 slog（key 哈希 + 字段 + 新值 +
  应用方式 rpc/respawn/deferred），失败降级路径 `slog.Warn`。
- 不新增 metrics（低频操作者动作）。

## 8. Compatibility & migration

- **配置**：`cli.backends[].models`（claude 清单补充）为唯一新配置项，
  `omitempty`。无迁移。
- **磁盘**：sessions.json 新增两个 `omitempty` 字段；shim state 不变
  （argv 变化经 4.5 的 drift 语义处理）。
- **多节点**：首版 local-only（NG4）。`/api/cli/backends` 的 models 字段
  经既有 node 代理自然透出，为后续远程支持留好读路径；写路径（override API
  转发）是后续 RFC 的事。
- **IM 端**：不受影响。后续可加 `/model <id>` slash 命令复用同一
  `SetSessionTuning` 入口，不在本 RFC 范围。

## 9. Rollout plan

单 PR 可评审性优先，按依赖切三片（每片独立可回滚）：

1. **PR-A 服务端核心**：Protocol.SetModel（三实现）+ TuningModel/TuningEffort
   持久化 + resolveSpawnParamsLocked 层 + drift 同步 + override API +
   F9 路径选择。
2. **PR-B 模型清单**：ACP availableModels 解析 → wrapper 缓存 →
   /api/cli/backends 透出 + `cli.backends[].models` 配置。
3. **PR-C dashboard**：chip popover + 三态（含 F7 拒绝回滚）+ gating +
   远程置灰。

无 feature flag（API 不调用 = 现状）。tag minor bump。

## 10. 实现清单（依赖序）

1. ~~claude set_model 真 CLI 集成验证~~ **已完成**（2026-09-01，F6/F7/F8：
   success/error ack、策略拒绝文案、resume 持久语义均实测）
2. `internal/cli/protocol.go` — `SetModel` 接口方法 + `ErrSetModelUnsupported`；
   `protocol_acp.go` — session/set_model RPC + session/new 与 session/load
   的 `availableModels` 解析（F12）；`protocol_claude.go` — set_model
   control_request（success/error ack 关联）；codex stub
3. `internal/session/managed_identity.go` + store — `TuningModel` /
   `TuningEffort` 持久字段 + load-time 校验
4. `internal/session/router_lifecycle.go` — `resolveSpawnParamsLocked`
   两条链各加 session tuning 顶层
5. `internal/session/router_shim.go` — drift 重建叠加 tuning（**先写回归测试**）
6. `internal/session` — `SetSessionTuning(key, model, effort)`：校验 +
   落盘 + 按 4.4 矩阵应用（活进程 RPC / effort respawn / suspended 仅记录）
7. `internal/dashboard/session/handlers.go` — `POST /api/sessions/override`
8. `internal/dashboard/ext/cli/handler.go` — BackendInfo.models 透出；
   `internal/config` — `cli.backends[].models`
9. `internal/server/static/dashboard.js` — chip popover / 三态 /
   applyFeatureGates 接线 / 远程置灰
10. `config.example.yaml` + README 功能表更新
