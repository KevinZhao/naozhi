# RFC: Planner —— 一个对话框，规划 agent 自己调度 session

- 状态: Draft v2（设计决策已与 owner 确认，未实施）
- 日期: 2026-10-16（v1 名为 conductor.md，v2 改名并并入六个专题调研）
- 范围: 把 naozhi 的交互从"用户逐个操作 session"改为"用户只面对一个对话框：快模型前台负责人机沟通，强模型 Planner 只做策划并把任务派给 worker session 执行，主动 Planner 按章程巡检项目并在授权范围内自主执行"。同时定义配套执行环境（凭证、沙箱、auto mode）、多节点调度与现有功能的迁移。
- 相关: #3448(b)（naozhi 作为 MCP server）、#3449（per-session git worktree）、#3686（`cli.permission_mode`）、`cli-mcp-config.md`、`system-session.md`、`workflow-dashboard.md`、`node-pairing.md`

> **术语**：本文的 **Planner** 即 v1 草稿里的 Conductor（D12 改名）。现有的 project planner（`project:{name}:planner`）不是另一个概念，而是 Planner 的雏形，迁移后合并为同一个东西（§13）。

## 1. 背景与问题

现状（2026-10 代码核实）：

- session 之间完全不互通：没有任何机制让一个 session 创建 / 驱动另一个 session 或回收其结果。
- `project:{name}:planner` 只是特殊 key + prompt + cwd + 豁免 TTL/max_procs 的普通 session（`internal/session/routing.go` `ResolveForChat` / `plannerOpts`），不能派活。
- Agent team / Workflow 都在单个 CLI 进程内扇出，naozhi 只观测（`internal/subagent`、`internal/cli/workflow`）。
- 用户为每个 session 手动决定 workspace、node、backend、access profile、agent、model/effort、label、续接还是新开。
- 执行环境：Claude 恒带 `--dangerously-skip-permissions`，Kiro 权限请求自动 `allow_once`，Codex `approval_policy=never`；envpolicy 只过滤 env，不隔离文件系统（`~/.aws`、`~/.midway/cookie`、`~/.ssh`、`~/.naozhi/*` 对 Bash 可读）；`mcp_config` 全局一份。

本机使用实测（2026-10-16，只取汇总数）：

- `platforms: {}`，dashboard-only、单机（7 月起无 upstream / nodes）。
- `sessions.json` 26 条全是 `dashboard:`，agent 后缀 100% `general`，`/review` `/research` 零使用，`project:*:planner` 为 0。
- 近 30 天 dashboard 直接会话 571 轮，单轮 p50 100s、p90 14min；同期 CC Workflow 40 个 run、3264 个子 agent transcript。
- 即：**用户目前在手工充当 Planner**，"拆开并行跑"已由 CC 在会话内完成。Planner 的价值不在扇出，而在跨会话的连续性与判断、主动巡检、替用户管理注意力。

外部参照：Anthropic 2026-09-17 发布的 Claude Code Projects 与本设计同构（一个协调对话 + 并行 worker thread + "Waiting on you" Overview），但依赖 claude.ai 登录与云端 thread。naozhi 的差异化定位：**Bedrock、多 backend（Kiro/Codex）、飞书/微信、本机凭证与内网系统、多节点自托管**。

## 2. 目标 / 非目标

目标

- G1 用户只面对一个对话框，用自然语言交互；斜杠命令降级到专家模式（D16）。
- G2 规划与执行分离：强模型只做决策，执行交给按任务选型的 worker。
- G3 主动模式：项目级 Planner 按章程与触发器巡检，在 Now/Next 边界与常设授权内自主执行，其余提议待批。
- G4 agent 能使用本机凭证与内部系统（邮箱、Asana、Midway、AWS），不逐次人工批准，但有硬边界、审计与急停。
- G5 远程分发：从任何入口把任务派到醒着或锁屏状态的 Mac 上执行（D13）。

非目标（见 §15）：内部 Planner 之间走 A2A；依赖 MCP Sampling / Roots；社区技能市场；向量库记忆；三层以上 Planner 嵌套；独立主动推送 feed；使用量打点（D15）；成本优化（D17，现阶段不作为约束）。

## 3. 角色模型

```
用户（Dashboard / IM，自然语言）
    │
前台 Interlocutor   sonnet 5.5，每个入口一个；自己能答的直接答（快路径）
    │  委托（带用户原话）            ▲ 报告 / 提问（原样转发，不改写）
Planner（可多个）   偏好表首个可用的最强模型；每个 workspace 唯一、串行；无 Bash/Edit，有只读工具
    │  dispatch（立即返回句柄）      ▲ 完成信封（摘要 + 结果引用）
worker sessions     按任务选模型 / backend / 能力档 / 节点；各自 worktree
```

### 3.1 前台（Interlocutor）

- sonnet 5.5，每个入口（dashboard / IM chat）一个，按渠道排版。
- **快路径**：闲聊、状态查询（读任务板）、简单问答直接答，不惊动 Planner。
- **不当传话筒**：委托信封携带用户原话与附件，前台理解只作附注；Planner 的提问、worker 的产出经 `forward` 原样给用户；批准 / 提问走带 ID 的结构化卡片（LangChain 实测原文转发比转述好约 50%）。
- 用户可直接回复某个 worker / Planner 的消息操控它（绕过前台）；"顺便问一下"式侧问不打断主任务。
- 收到消息立即回执，永不阻塞等 Planner。
- 主路径不需要斜杠命令：`/new` `/stop` `/urgent` 等意图由前台从自然语言识别（"停下"、"先处理这个"、"换个话题"），破坏性意图（停止 worker、丢弃上下文）以卡片确认。

### 3.2 Planner

**模型与工具**

- 有序偏好表，取第一个当前凭证能调通的（复用 `ccprobe` 实跑验证），不由系统自评谁更强。例：`[claude-opus-5-5[1m], claude-fable-5-1]`（fable-5-1 在本机 naozhi 凭证下 deny，当前落到 opus 5.5）。
- 证据支持最强模型：弱 planner 的损失远大于弱执行者，且换强执行者补不回（PEAR，EACL 2026 Findings）；强 manager + 便宜 worker 与强模型单干持平，弱带弱反而比单干差（arXiv 2603.26458）。Planner 单轮短、token 大头在 worker 层，强模型的边际成本低。用回归评测集做一次"sonnet 当 Planner"对照，再决定"个人杂事"等领域可否降级。
- 无 Bash / Edit / Write；**保留只读工具**（Read / Grep / Glob、`git log`、`gh ... view`），只用于核实具体说法，不用于"读一眼就判定已修好"（同一研究观察到的失败模式）。

**单例与状态**

- 每个 workspace 唯一、串行：用户委托、duty、外部事件、worker 完成都是投给它的信封。Planner 从不执行、从不等待，单轮是"看状态 → 决策 → 派活 → 返回"。
- 多个 Planner 按 workspace / 领域划分（naozhi、其他项目、邮箱、个人杂事）。
- 状态外置：charter、understanding、lessons、任务板、提议队列、journal 落盘；session 可随时 reset，重建时读 charter + understanding 索引 + 任务板 + 最近 N 条信封（合计约 30k token 以内）。

**项目理解（`understanding.md`）**

只记从代码推不出来的东西。证据：LLM 生成的 context 文件让成功率降约 3%、仓库概览不帮 agent 找文件（arXiv 2602.11988）；23% 的 AI 配置文件引用了已不存在的代码元素（arXiv 2606.09090）。

位置 `<project>/.naozhi/planner/understanding.md`（索引，≤200 行 / 25KB）+ `understanding/*.md`（专题，按需读）。

| 区 | 内容 | 谁写 | 有效期 |
|---|---|---|---|
| **Now / Next / Not-now** | 当前重心、下一步、明确不做 | **只有人**（D14） | 14 天未确认则在早报里请人确认 |
| 决策与理由 | 指向 RFC / ADR / PR，一句话"为什么" | Planner 提议 | 跟随 RFC 状态 |
| 技术债与坑 | 关联 issue 或 `file:symbol` | Planner 提议，dream duty 合并 | 90 天 |
| 反模式黑名单 | "靠 sleep / skip 修 flaky"、"抬基线"等 | 来自被打回的任务 | 长期，带案例 |
| 近期变化 | 滚动 14 天 | dream duty 生成 | 自动滚动 |

- 只引用不复制：代码结构、命令、待办的正本在 CLAUDE.md / DESIGN.md / GitHub / RFC 索引。
- 每条记录：`claim / evidence[] / anchors[] / verified_at:<commit> / provenance / confidence`。
- 过期检测用确定性脚本：锚点在 `verified_at` 之后被改过或符号已不存在 → 标"未复核"并降置信度，引用时必须带提示。
- 纠错：人在对话里说"记住 / 改掉" → Planner 生成 diff，人发起的直接生效并回显；Planner / dream duty 提的攒进早报批准；全部有 git 历史。
- `.naozhi/planner*` 纳入 §6 的测试完整性 diff 闸门，worker 不能经 PR 改写。

**Now / Next / Not-now 是受控边界**（D14）

不只是优先级提示，网关据此执行：

- duty 或 Planner 自发的任务必须挂到一个 Now / Next 条目（`focus_ref`），否则只能以"提议"进收件箱，不能派 worker。
- 落在 Not-now 的任务一律拒绝（含 Planner 自发与他人跨域请求），除非来源是人本人。
- 人直接提出的请求不受此限（人就是授权来源），但 Planner 应提示"这不在 Now / Next 里，要不要加进去"。

**判断：打分与说不**

写进 charter，每个提议附各项分值供审计：

> 优先级 ≈ 价值 × 与 Now/Next 的契合度 × 置信度 ÷（worker 成本 + 人的评审成本）

- 价值：严重度（p0–p3 标签或推断）、是否影响主路径、安全 / 数据风险、是否反复出现。
- 置信度：有无可自动跑的 oracle、影响面、能否回滚。
- 人的评审时间是真正的瓶颈：人审 agent PR 平均 42.9h，审人写的 22.2h（ACM 2026-07-31）。
- 说不的硬规则（能由网关强制的交给网关）：与开放 PR / issue / 在途任务重复；在 Not-now 里或 N 天内被否过且无新证据；写不出验收标准的只派调查不派修复；人待审 PR 超过 K 个就停止开新 PR；p3 外观问题攒 backlog；连续 3 次失败停手上报。
- "什么都不做"是合法输出；SLO 计入人拒绝与 revert 的比例，防止为显得忙而制造工作。

**审查分工**

- verifier（新上下文、可用便宜模型）：机械检查——测试、gofmt、lint、CI、diff 闸门、default-FAIL 验收清单。回答"做到没、弄坏没"。
- Planner：方案层评审——修的是根因还是症状、有没有顺手的语义改动、是否违背已记录的决策或反模式、交接里"没做什么"是否与"已完成"矛盾、是否碰了已知有坑的区域。输入是交接 + diffstat + verifier 报告，针对具体说法读对应 diff 片段。输出四选一：接受 / 带理由返工 / 拒绝并记入反模式 / 上报。
- 大 diff 或高风险任务另派强模型、干净上下文的评审 worker，Planner 只读其结论。
- 评审质量取决于派活时写下的验收标准（"Review Beats Planning"：评审收益随规格详细程度放大 4 倍）。

### 3.3 worker

**选型与交接**

- 由 Planner 按任务选 model / effort / backend / 能力档 / 节点；常规活 sonnet，难活 opus。
- 每个 worker 一个 git worktree + 分支（#3449）；Planner 声明归属路径，网关对路径重叠的任务串行化。
- 结束时返回结构化交接：做了什么 / 没做什么 / 命令与退出码 / 发现的问题 / 经验候选；允许显式"不可完成"退出。
- 短小、同目录、跑完即弃的子任务优先用 CC 原生 Agent / Workflow。

**生命周期**（D18）

进程、上下文、记录三层分别回收；寿命跟**产物**走，不跟进程走。

| 状态 | 进程 | 上下文 | 进入条件 |
|---|---|---|---|
| running | 活 | 在用 | 正在跑一轮 |
| warm | 活 | 缓存有效期内 | 等 verifier 或下一步，≤10 分钟 |
| awaiting | 释放 | 可 `--resume` | 绑定产物（PR / 报告），等 review / CI / 人 |
| dormant | 释放 | 可 `--resume` | 产物仍开着但无订阅 |
| archived | 无 | 不再恢复 | 产物关闭；只留交接、journal、transcript 引用；worktree 删除 |

- 寿命：PR worker 到 PR 合并 / 关闭 + 24h；报告类到被采纳或空闲 7 天；侦察 / verifier 完成即归档；人显式 pin 可延长（上限 30 天）。
- 进程尽早释放：一个活会话进程树实测约 640MB，保温只省几秒 spawn。
- 追问路由：同一产物 + 上次上下文 ≤ 约 100k token + 休眠 ≤72h + CLI / backend 版本不变，四条同时满足才复活原 worker；否则同一 task key 新开 worker，接 `prev_session_ids` 链，输入为结构化交接 + 产物实时状态（diff、未解决 review、CI 日志）+ 旧 transcript 的 `read_result` 引用。150k 冷上下文复活约是新开的 5–10 倍。
- 唤醒来源：人直接回复、Planner `send`、外部事件。外部事件只认有写权限者的 review 与 CI 失败；同类合并；忽略自己的 push 与重复绿灯；连续 3 次修复失败或分支出现人工 commit 即停止自动唤醒并交回 Planner。
- 进入 dormant 前必须 commit + push WIP；远端分支是真相源，worktree 可删可重建。
- 缓存 TTL 按角色：Planner、前台、awaiting-ci 的 PR worker 用 `promptCacheTtl: "1h"`，其余 5 分钟（本机当前未配，全为 5 分钟）。
- 上限：活进程受每 Planner `max_workers`、`task:` 命名空间配额、主机级上限三者约束；awaiting + dormant 条目每 Planner ≤50，超出先归档最老的 dormant。

## 4. 主动 Planner：charter、duties、触发器

主动 / 被动不是两种角色，只是 Planner 有没有 duties。

```yaml
# <project>/.naozhi/planner.yaml —— 由现有 project.yaml 的 planner_model / planner_prompt 演进
mode: active
charter:
  scope: 维护 naozhi 仓库健康：CI 绿、flaky 收敛、依赖安全更新、issue 分诊
  escalation: 超出 standing_orders 的一律提议；连续 3 次失败停手上报
accepts:                  # 接受哪些跨域请求（§5.3）
  - { from: "planner:*", kinds: [request, query], max_autonomy: draft }
duties:
  ci-watch:     { trigger: "event:github.ci_failed", fallback_cron: "*/30 * * * *", autonomy: execute }
  flaky-sweep:  { cron: "0 3 * * *",  autonomy: propose }
  issue-triage: { cron: "0 9 * * *",  autonomy: propose }
  dep-audit:    { cron: "0 10 * * 1", autonomy: propose }
standing_orders:          # 常设授权：做完汇报
  - 修 flaky 测试并开 PR
  - 依赖安全补丁
limits: { max_workers: 4, max_threads_per_day: 40 }
```

- charter 采用 standing-orders 四要素（Scope / Triggers / Approval gates / Escalation），只有人能改；学到的规则放 `lessons.yaml`（§8.2），不进 charter。
- schedule 缺省 = 关闭；调度时间放在 naozhi cron，不写进 prompt。
- 事件优先、轮询兜底；事件按来源 ID 去重，每个 duty 限速，cron duty 加 jitter。
- 巡检不在 Planner 的长上下文里跑：先由轻量上下文 + 便宜模型的侦察 worker 执行，默认静默，有结论才投信封唤醒 Planner。
- 自主度三档 propose → draft → execute，默认 propose，按 §8.3 升降档；批准粒度是"计划"。
- 常设授权：卡片上"以后都允许"生成可撤销、有使用计数与过期时间的 standing grant。
- 所有自发任务受 Now / Next / Not-now 边界约束（§3.2）。
- 第一个主动 Planner 落在 naozhi 仓库本身；邮箱类等 §10.3 的读写分离落地后再上。

## 5. 信封、任务板与 naozhi MCP

### 5.1 信封

```
Envelope{ id, from, to, kind, origin, chain[], task_id, focus_ref?, body, refs[], deadline? }
kind: request | accept | decline | need_input | progress | result | failed | query | report | question | answer | event
```

- 投递走 `turn.Orchestrator.Submit` + 新的 `envelopeOrigin`（Origin / Delivery 端口现成，Orchestrator 不改）；对方忙则排队，collect 模式合并同类。
- 队列加优先级：人来源的信封插到触发器信封前；同类触发器合并。
- 报告传摘要 + ref，全文落盘，Planner 需要时 `read_result`。`progress` 只写任务板，不唤醒。
- 信封日志 append-only（事件溯源）；任务板状态迁移只经网关。

### 5.2 naozhi MCP（#3448(b) 的落地形态）

- `naozhi mcp` stdio 桥经 unix socket 连回 naozhi，带每 session 签发的能力令牌（不复用 dashboard token）；校验对端 PID（macOS `LOCAL_PEERPID`）及其祖先是 naozhi spawn 的进程。需要 per-session mcp-config。
- 句柄式异步：`dispatch(...) → {run_id}` 立即返回；`await_run(run_id, timeout)` 期间发 `notifications/progress` 重置 stdio 空闲计时；另有 `get_run` / `cancel_run` / `list_runs`。依据：MCP 2026-07-28 推荐服务端签发句柄；CC 不实现 MCP Tasks 扩展客户端。
- 不用 Sampling / Roots（已弃用）；`ask_user` 是普通工具，由 naozhi 路由到 dashboard / IM。
- 无状态形态：身份取自令牌；`tools/list` 固定顺序（利于 prompt cache）；工具面约 8–12 个。
- 角色工具集：前台 `delegate / list_tasks / answer / forward`；Planner `dispatch / send / cancel / report / ask_user / propose / request / list_tasks / read_result / memory_propose / journal_append`；worker 仅 `report_progress`（可选），另有 Bash 可用的 `naozhi` CLI。
- 限额由网关强制（并发、PR 数、每日 thread 数、跳数），不靠 prompt。

### 5.3 多 Planner 协作（D19）

Planner 之间不直接对话，只互投请求信封，由对方决定接不接；各自只对自己的 workspace 负责。网关强制：

- **状态机**（语义借用 A2A）：`request{goal, acceptance, why, deadline, origin, chain[]}` 只写目标与验收、不写步骤 → `accept` / `decline{reason}`（终态，不可原样重投，只能改写或升级给人）/ `need_input` → `progress` → `result{artifact_refs, verifier}` / `failed`。`query` 只读查询，不建任务；对方 charter 声明自动接受时直接回 artifact 引用。
- **归属**：每个 Planner 在 charter 声明 `accepts:`；发起方不能在对方 workspace 派 worker（路径归属扩展为 workspace 归属）；责任沿链传递，不推给下游。
- **路由**：前台按"主要交付物落在哪"指定一个 lead Planner，lead 再发子请求；只有互相独立的子任务才并投，挂复合父任务只做汇总。只有 lead 对用户汇报；但需要某领域批准的事项，由该领域 Planner 直接发收件箱卡片，不经 lead 转达（CC agent teams："队友不能代你批准"）。
- **provenance 只降不升**：`origin`（`human:<id>` / `external:email:<msgid>` / `duty:<id>`）写入后不可改；有效信任等级取整条链最低值。例：邮件要求修 naozhi 的 bug → naozhi Planner 最多 propose，执行须人批准，卡片显示原发件人与原文引用。外部原文只作带不可信标签的 ref，不进 `goal`（Prompt Infection：只打来源标签攻击成功率仅降约 5%）。跨 Planner 请求出站时由网关审一遍。
- **防环与防失控**：跨 Planner 最多 2 跳；目标已在 `chain` 中即拒绝；按（origin, target, 意图哈希）去重；每对 Planner 设速率上限，被弹回 3 次熔断；每个请求必须有 deadline 并一定走到终态（Agents of Chaos：两个互相转发的 agent 持续 9 天、约 6 万 token，并自建 cron）。
- 否决的方案：直接对话（MAST FC2 协调失配、注入扩散）、黑板自愿认领（责任分散）、contract-net 竞标（每个 workspace 只有一个承接方）、handoff 移交对话（打破归属、污染上下文）、同步调用（违反"从不等待"）。

## 6. 验证闸门

- 验收标准在 dispatch 时写好（default-FAIL：每条初始未通过）。
- 任务"完成"只认 verifier 的 PASS（§3.2 审查分工），按任务类别可配置。
- **测试完整性 diff 闸门**：改动触及 `*_test.go`、CI 配置、新增 `t.Skip`、`*Baseline*` 常量、`scripts/ratchet-raises.jsonl`、lint 豁免、`.claude/` 设置、`.naozhi/planner*` 时默认拦下标记，除非任务明确允许（ImpossibleBench：Claude 模型多见直接改测试；Opus 5.5 system card：不可能任务下奖励作弊率 6.0% → 34.7%）。
- CC 的 `/goal` 只是停止规则，不能当 verifier。

## 7. 交互：一个对话框、收件箱、专家模式

- **主视图**：dashboard 主对话框 + 任务面板（复用 workflow 面板形态，点进去是现有 session 视图）。
- **注意力收件箱**：分组 Waiting on you / Ready for review / Working / Landing / Idle；条目类型 notify / question / review / decision / error，带归属 Planner、workspace、等待时长；条件消失自动关闭；只有人能关闭。
- **卡片**：带请求 ID，校验发送者本人而非群 chat ID；首个回答生效；应用决定前重读待决项。
- **早报**：静默时段积压到早报；每条带 👍/👎/"少来点"，反馈进 §8.2 的学习回路；Now / Next / Not-now 的到期确认也在这里。
- **排队 vs 插话**：默认排队，"立即生效"是显式动作。
- **专家模式**（D16）：斜杠命令、会话侧边栏、新建会话对话框（backend / access profile / node / workspace）、tuning 弹层、`/cd` 等全部保留在专家模式 session 中，一键可达、长期保留；主对话框不要求用户懂这些。
- 团队群聊保持隔离，不并入个人对话框。

## 8. 记忆、学习与带教

### 8.1 记忆

- **单写者**：CC auto-memory 按仓库存放、所有 worktree 共享，worker 读到恶意内容即可写进 Planner 每次加载的 MEMORY.md。每个角色独立记忆目录（`autoMemoryDirectory`），worker 默认 `autoMemoryEnabled:false`，Planner 只经 `memory_propose` / `journal_append` 写；worker 只读 workspace 记忆摘要。
- **来源标签**：journal / 记忆条目记 provenance 与 session 类型；cron / 事件 duty 的产出只进 journal。只有人来源的信封能授权 charter 修改与不可逆操作；Planner 不能改自己的 charter。
- **PreCompact hook**：压缩前把未决决策写进 journal。
- **夜间 dream duty**：独立 session 读当日 journal 与 transcript，产出记忆仓库 git diff；逐条小改不整篇重写（ACE：整篇重写把 18k token 压成 122 token，准确率跌破无记忆基线）；删除比例设上限；保留旧版与可读变更日志。
- **技能只从验证通过的任务提炼**：经 verifier PASS 或复现 ≥2 次才升为 `SKILL.md`，留 with/without 回放对照（SkillsBench：事前自写技能 −8.1pp）。
- USER.md：前台加载说话人自己的偏好，只把相关条目带进任务简报；条目原地更新；群聊不加载私有记忆。

### 8.2 带教回路（D20）

人的纠正沉淀为独立的规则库 `<project>/.naozhi/planner/lessons.yaml`（单写者），与 charter 分开，防止 charter 膨胀。

- **信号强度**：强 = 显式纠正、带理由的拒绝、7 天内被 revert；中 = 对草稿的修改（从 diff 归纳偏好，CIPHER 做法）；弱 = 批准、无理由拒绝、👍/👎。
- **按类型落地**：

| 类型 | 例 | 落地 |
|---|---|---|
| constraint | "别碰 `internal/cli/workflow`" | 编译成网关路径 deny / PreToolUse hook，不写成文字 |
| preference | flaky PR 的标题格式 | 进 prompt，scope 限定到 duty |
| fact | "shim 包 SetWriteDeadline 类失败先 `-count=3` 复跑" | 带代码锚点，用前校验；28 天未用删除（Copilot Memory 做法） |
| procedure | 一套修复步骤 | 走 §8.1 技能提炼 |

- **条目字段**：`id, rule(≤2 句祈使句), kind, scope{planner,duty,paths,task_type}, strength(must|should|hint), source, origin, evidence[], created, last_confirmed, last_applied, review_by, helpful/harmful, supersedes, status(candidate|active|dormant|retired)`。
- **整理**：只做增量操作（ADD / UPDATE / MERGE / RETIRE），确定性合并；隐式经验独立出现 ≥3 次才升为规则，不可信来源的候选结构上排除；冲突只标记交人，不自动裁决（IHEval：最好的开源模型冲突裁决准确率 48%）；should / hint 30 天未用转 dormant，must 不衰减但到 `review_by`（默认 90 天）在早报请人确认。
- **规模上限**：charter 核心约 40 行置顶；全局 active 规则 ≤15、按 duty / 路径匹配 ≤30、每轮合计 ≤50（IFScale：Claude Opus 4 在 100 条指令 94.6%、250 条 67.9%）；constraint 在网关执行不占预算；超预算强制整理，不静默截断。worker 简报只带 scope 匹配的规则。
- **人工节奏**：即时（显式纠正弹卡片一键"存为规则，scope X，90 天复查"）、每周（早报附 ≤10 条候选与冲突）、每月（helpful / harmful 统计与 dormant 清理）。

### 8.3 自主度升降档

阈值按"三法则"推算（连续 30 次零失败 ≈ 95% 置信下失败率 ≤10%），是建议值而非公开实证；按 duty × 动作类别分别计，升档由人确认：

- propose → draft：≥20 次提议，批准率 ≥90%，被实质修改 ≤10%。
- draft → execute：连续 30 次 verifier PASS 且无实质拒绝，7 天 revert 为 0。
- 自动降一档：任一 execute 动作被 revert、触发 diff 闸门、最近 10 次被拒 ≥2 次、harmful > helpful。
- execute 档每 10 次抽 1 次复审防漂移。
- 反向风险：Planner 被拒多了学成"不提议"——同时统计漏报（人自己修掉的 CI 失败数）；一句话被泛化成过宽规则——scope 必填、默认最窄。

## 9. 多节点：控制面与执行面（D13）

**拓扑演进**

- 阶段一：单机 Mac。控制面与执行都在 Mac；合盖睡眠时 duty 暂停。协议从一开始按 `run_id` + 幂等设计，便于迁移。
- 终态：**EC2 作为控制面**（任务板、信封日志、网关、cron duty、Planner 会话，单一真相源，不复制）；**Mac 作为持有凭证的执行节点**。用户可从任何入口把任务派给醒着或锁屏的 Mac（LaunchAgent 在锁屏时照常运行；睡眠时任务进入 `blocked(waiting_node)`）。Mac 跑活期间节点用 `caffeinate -i -w <pid>` 阻止空闲睡眠（挡不住合盖），唤醒或墙钟跳变时立即重连（绕开 5 分钟熔断）。
- Planner 的只读工具读 EC2 上的镜像 clone，决策前 `git fetch`；只有 Mac 能访问的仓库交给侦察 worker。

**现状要点**（`internal/node`、`internal/upstream`）

- 反连 `/ws-node`：请求只能由 primary 发往节点；节点回 `response` 与已订阅 key 的 `event/state`。
- 可做：读会话 / 项目 / backend / 事件，`send`（不存在则创建，可带 workspace）、interrupt、remove、takeover 等；不可做：远程 cron、远程附件、远程 access profile / scratch。
- **现存缺口**：`send` 线路上只有 key / text / workspace，没有 backend 字段，dashboard 给远程节点选的 backend 不生效（代码推断，未真机验证）；单独开 issue 修。
- 失败语义：断线 60 秒宽限；离线时 send 不排队、不重试；**send 不幂等、无 turn id 关联结果**。

**节点能力清单**（新 RPC `fetch_manifest`，注册时与每 5 分钟拉取）

```yaml
node_id: mac
always_on: false
power: {ac: true, lid_open: true, sleep_assertion: true}
backends: [{id: claude, models: [...], auth_ok: true, checked_at: ...}, {id: kiro, ...}]
repos: [{name: naozhi, remote: github.com/KevinZhao/naozhi}]   # 按 remote 匹配，不传路径
cred_classes: [{id: midway, expires_at: ...}, {id: outlook-mcp}, {id: asana-mcp}]   # 只有标签与新鲜度
tiers: [repo-rw, internal-read]
capacity: {max_workers: 2, running: 1, load1: 3.2}
policy: {autonomy_max: draft, side_effects: deny, allowed_repos: [...]}
```

**放置规则**（Nomad 语义）：任务声明 `requires`（硬过滤，如 `cred:midway`、`backend:kiro`、`repo:naozhi`）、`prefers`（加分，如 `always-on`、`ac`）、`avoid`、`pin`；只有离线节点能满足时进入 `blocked(waiting_node)` 并设 `max_wait`；凭证过期视为不满足，收件箱提示"到 Mac 上跑 mwinit"。

**dispatch 协议**（节点能力 `dispatch-v1`）

- `dispatch{run_id, task_id, attempt, repo, backend, model, effort, tier, envelope, deadline, lost_after, side_effects}`：节点先落盘 run 记录再启动；同 `run_id` 返回 `duplicate` + 当前状态；按本地 policy 回 `accepted` / `busy` / `rejected{reason}`。
- 节点推 `run_update{run_id, attempt, seq, state, handoff}`，primary 回 `run_ack{run_id, seq}`；节点发件箱持久化，未 ack 的在重连后重放；primary 先写信封日志再 ack，按 `(run_id, seq)` 去重。
- `list_runs{since}` 重连对账；`cancel_run`；`fetch_run_artifact`（有大小上限，节点侧先脱敏）。代码与 diff 走 git remote（分支名带 `run_id`）。

**离线状态机**（primary 侧，用自身单调时钟）

```
queued → placing → [blocked] → dispatched ─ack→ running(租约随心跳)
                                └超时→ admission_unknown ─重连 list_runs→ running / 重发同一 run_id
running ─断线 >60s→ suspect ─超过 lost_after（Mac 2h，EC2 15m）→ unknown
unknown ─只读任务且 replace=true→ 别的节点 attempt+1（先完成者胜，另一个 cancel）
        ─有副作用→ 收件箱 error，由人决定（复用 cron SideEffects 规则）
running → done / failed / cancelled ─发件箱→ run_update → Planner 收到 report 信封
```

EC2 重启：节点把进程已消失的 run 标 `failed(node_restart)` 并保留 session_id，primary 优先 `--resume` 续跑。

**跨节点安全**

- 现状下 primary 能经 `send` 往 Mac 上全权限 CLI 塞任意 prompt：EC2 被攻破即等于在持有 Midway 与 `~/.aws` admin 的 Mac 上远程执行代码。
- Mac 端新增 `upstream.mode: dispatch_only`：关闭裸 `send`、`update_config`、takeover；节点本地 policy（可接受 repo、tier、自主度上限、是否允许副作用）由节点说了算，primary 不能改。
- dispatch 只指名 tier，**凭证永远不过线**。两端各记审计，以 `run_id` + traceparent 对账。
- 待定：人工批准是否由 owner 私钥签名，让节点能独立验证（否则 primary 被攻破可伪造批准）。

**最小第一步**（复用现有部件）：`send` 加 `backend` 字段（新能力 `send-backend`，顺带修缺口）→ `fetch_manifest`（BackendsManifest + projects + gitinfo remote + 配置静态声明的 `upstream.labels`）→ primary 网关订阅远程 `task:` key，`result` 事件判定完成，重连靠 `subscribe-history` / `fetch_events` 补漏 → 不确定状态一律进收件箱不自动重派。之后再上 `dispatch-v1`。

## 10. 执行环境与凭证

### 10.1 权限模式：沙箱内全权限，不逐次批准

- worker 整体跑在 `srt`（Anthropic sandbox-runtime）里：网络默认拒绝，放行 GitHub、goproxy.cn、Bedrock 及声明的内部域名；只写自己的 worktree；不可读 `~/.aws`、`~/.ssh`、naozhi 配置、审计状态。EC2 上 worktree 须在启动 worker 前建好。
- auto mode 替代 `--dangerously-skip-permissions`（试用中，`cli.permission_mode`，#3686；见附录 A）：必须在 CLI 加载的 settings 里声明 `autoMode.environment`（KevinZhao 组织仓库、goproxy.cn、内网与 Outlook 域、Host containment 身份），否则正常操作会被误拦。`autoMode` 不从仓库 `.claude/settings.json` 读，naozhi 用 `--settings <file>` 可按角色注入。
- auto mode 不是安全边界（误拦约 0.4%、越界漏判约 17%；只读消息与命令不读输出），且**把任务里的明确指令当作用户意图**（附录 A），挡不住 Planner 下达的破坏性指令。不可逆约束必须落在 `permissions.deny` / PreToolUse hook。
- 分类器拒绝作为事件进 dashboard 与审计；连续拒绝导致的终止、模型级 safeguard 拒绝（`model_refusal_no_fallback`）是新的错误类别，进收件箱 error，不自动重试。
- Planner 无 Bash / Edit，用 `dontAsk` + 显式白名单。
- 破坏性 git / gh（force push、`reset --hard`、删分支、删 release）另加 hook 拦截。

### 10.2 凭证

- 每个 worker 独立 STS 会话，带 `SourceIdentity=naozhi/<session>` 与 session tags + session policy；本机 `~/.aws` default 是 admin 账号，交给 agent 必须是显式选择。
- 发布权限 / 生产 token 不放在 worker 可读的位置。
- Midway / Outlook / Asana 走凭证代理：worker 调 `midway_fetch(url)`、`outlook_search(...)` 一类能力工具，由 naozhi 注入 cookie / token。
- 本机现有内部 MCP：`aws-outlook-mcp`（默认只读，`OUTLOOK_MCP_ENABLE_WRITES=true` 才开写）、`aws-sentral-mcp`、`emburse-mcp`、`enterprise-asana-mcp`；按能力档挂载，不再全局一份。
- 内网网页：专用 Chrome profile + Playwright MCP + 域名白名单，每个动作截图进审计。Claude in Chrome 需 claude.ai 登录，Bedrock 下大概率不可用。
- 脱敏补 Midway cookie、JWT 模式；`mcp.json` 与 settings 对 agent 不可写或 spawn 前校验指纹；启用 Planner 时强制 dashboard token 并只监听 127.0.0.1（或经 §9 的节点通道）。

### 10.3 读写分离（推迟，D7）

风险只在 agent 自主读取第三方内容且人不在环里时出现（Meta "Rule of Two"）。naozhi 仓库场景不需要；**邮箱 Planner 上线的前提**是：读不可信内容的 worker 不同时持有对外写工具，外部内容以标注过的不可信数据传给 Planner。

### 10.4 急停与回滚

- 急停：冻结 dispatch、对 worker 进程组 SIGSTOP、撤销 standing grants；须演练。
- dispatch 前快照 `refs/naozhi/<task>`；需要时 APFS / EBS 快照。

## 11. 可观测

- 全链路 OTel trace：入口 → 前台 → Planner → worker（`-p` 读入站 `TRACEPARENT`；MCP `_meta` 透传 `traceparent`），span 用 `gen_ai.*` 命名，同时作为审计账本。
- 每个 duty 的 SLO：verifier 通过率、返工率、7 天内被 revert 的比例——作为 §8.3 升降档依据。
- 回归评测集：从 naozhi 历史 CI / flaky / 分诊案例取 20–50 个，换模型 / prompt / 工具面时重跑。
- 每个可派发单元控制在约 1 小时人类专家工作量（METR 80% 时间地平线约 1–1.5h）。
- 成本账本照常记录（含 `task:` 归属到 Planner），但现阶段不作为门槛或优化目标（D17）。

## 12. 与现有代码的映射

| 现有 | 演进 |
|---|---|
| `project:{name}:planner` 会话 | Planner 身份，保留 key 语义 |
| `.naozhi/project.yaml` 的 planner_model / planner_prompt | 迁到 `.naozhi/planner.yaml`（charter + duties） |
| cron：在 `cron:<id>` 里跑 prompt | 新增目标类型"给 Planner 投 duty 信封"；旧任务照跑 |
| `turn.Orchestrator` Origin / Delivery 端口 | `envelopeOrigin`；队列加优先级与同类合并 |
| scratch pool（继承上下文、上限、TTL、可提升） | worker 模板；`ReleaseIdleProcess` 放开给 `task:` |
| `prev_session_ids` 链（`auto_chain_retire.go`） | worker 新开时的谱系 |
| router TTL / `--resume` / `shouldPrune` | worker 的 dormant / awaiting；需按命名空间设 TTL，`resolveResumeID` 失败改为显式交接 |
| `cli.mcp_config`（全局） | per-session 生成，按角色 / 能力档挂载 |
| `max_procs`（只驱逐空闲） | `task:` 命名空间配额 + 每 Planner `max_workers` + 主机级上限 |
| workflow 面板 / agent drill-in | 任务面板主体 |
| AskUserQuestion 卡片 | 带 ID 的批准 / 提问卡片 |
| `PermissionMode`（Default / Standard） | 新增 Auto（#3686） |
| node / upstream `send` | `send-backend` → `fetch_manifest` → `dispatch-v1` |

新 key 命名空间：`task:<planner>:<id>`（worker）、前台另起前缀。新包暂定 `internal/planner`（任务板、信封日志、charter / lessons 校验、放置）。新增前缀须逐一审计 `internal/sessionkey` 的前缀判断（cron、budget、TTL 豁免、max_procs）。

## 13. 现有功能的去留与迁移（D15、D16）

| 功能 | 新模型角色 | 去留 |
|---|---|---|
| `/new` `/clear` `/stop` `/urgent` | 前台从自然语言识别同类意图 | 命令降到专家模式；意图保留 |
| `/cd` `/pwd` `/model` `/effort` `/backend` | worker 选型归 Planner | 专家模式 |
| `/project` | 绑定入口到 Planner | 并入 Planner；命令降到专家模式 |
| `agent_commands`（`/review` `/research`） | worker 能力档 | agents 定义转为 dispatch profile，命令退役 |
| `/cron` 与 cron 面板 | duty 触发器 | 合并，两种目标并存 |
| group_scope / thread_auto_open | 团队群隔离 | 保留 |
| 会话侧边栏 / 项目分组 / 收藏 | 专家模式入口；收藏 → Planner 列表 | 专家模式，长期保留 |
| 新建会话对话框、tuning 弹层 | 由 dispatch 参数替代 | 专家模式 |
| scratch 追问抽屉 | "追问此 worker" | 底层池复用，抽屉 UI 退役 |
| workflow 面板 / agent-team 下钻 | 任务面板 | 保留并升级 |
| 接管终端会话 | 把终端会话收编为 worker | 保留并扩展（需单独 provenance 标记） |
| 文件视图 / cc 资产浏览器 / 记忆弹层 | 产物与记忆查看 | 保留 / 合并到 §8 |
| 多节点 | worker 放置 | 保留并升级（§9） |
| 成本 / 预算、语音、附件、webhook、sysession | 照常；sysession 与侦察 worker 同构 | 保留，后续统一 |
| project planner | 就是 Planner | 合并 |

迁移阶段（不做使用量打点，D15）：

- **M1 并存**：`planner.enabled` 默认关；以 `.naozhi/planner.yaml` 是否存在按项目 opt-in；新增 `task:` 命名空间，旧 key 语义一律不变；`naozhi planner migrate`（默认 dry-run，`-write` 落盘）把 planner_model / planner_prompt 写进 planner.yaml。
- **M2 交互**：dashboard 加"对话框 / 专家"视图切换（持久化到 ui-settings）；已有安装默认保留侧边栏，新安装默认对话框。
- **M3 cron 合入 duty**：cron 加"投 duty 信封"目标，旧任务原样照跑并提供一键转换；scratch 入口改为"追问此 worker"。
- **M4 退役**：Planner 模式成为默认后，删除 scratch 抽屉 UI、`/review` `/research` 绑定、planner 专属 routing 分支（并入 `internal/planner`）；斜杠命令只在专家模式 session 中可用。真正可删约 1.5–2.5k 行生产代码 + 约 3k 行测试，其余以降级为主。

## 14. 分阶段

| 阶段 | 内容 |
|---|---|
| P0 验证 | ① auto mode 试用（#3686）；② per-session mcp-config；③ naozhi MCP 桥 + 句柄式 dispatch/await 在 `-p` stream-json 长驻进程中的行为；④ Bedrock 下 `promptCacheTtl: "1h"` 生效（看 `ephemeral_1h_input_tokens`）；⑤ srt 包裹 shim 启动的 CLI；⑥ 用现有 REST + curl 糊原型验证交互；⑦ 远程 `send` 缺 backend 的缺口修复 |
| P1 编排内核 | 信封 / 任务板 / `envelopeOrigin`、naozhi MCP、`task:` 命名空间与网关限额、worker 生命周期状态机、worktree + 路径归属、结构化交接、verifier 与 diff 闸门、急停与快照 |
| P2 交互 | 前台（快路径 / 原样转发 / 自然语言意图）、收件箱、卡片、主对话框 + 任务面板、专家模式切换 |
| P3 主动模式 | charter / duties / Now-Next-Not-now 边界 / 事件触发 / 自主度 / standing grants / SLO；首个落在 naozhi 仓库（单机 Mac） |
| P4 记忆与带教 | understanding.md + 过期脚本、lessons.yaml、dream duty、技能提炼 |
| P5 多节点 | EC2 控制面 + Mac `dispatch_only` 执行节点、`fetch_manifest`、`dispatch-v1`、离线状态机 |
| P6 凭证 | 能力档 + 凭证代理 + STS SourceIdentity；读写分离后接邮箱 Planner |

## 15. 明确不做

| 不做 | 原因 |
|---|---|
| 内部 Planner 之间走 A2A | 同进程 MCP dispatch 更简单、可审计；A2A 只留给跨部署联邦 |
| 依赖 MCP Sampling / Roots / server 端 elicitation | 2026-07-28 规范已弃用或重做 |
| MCP Tasks 扩展作为主路径 | CC 不实现其客户端 |
| Claude in Chrome 访问内网 | 需 claude.ai 登录；API 部署下原始注入成功率 31.5% |
| 社区技能市场 | ClawHub 13.4% 的技能有严重问题 |
| 向量库 / 图数据库记忆 | grep + 文件已够用 |
| 三层以上 Planner 嵌套、跨 Planner 超过 2 跳 | 官僚化与转发死循环 |
| 独立主动推送 feed | ChatGPT Pulse 2026-06 下线 |
| 使用量打点后再决定去留 | D15：owner 已确认斜杠命令很少用，直接降级 |

## 16. 待定问题

- 前台快路径直接调 Bedrock，还是也作为长驻 CLI session？（倾向后者，保持"agent loop 委托给 CLI"原则，待 P0 测延迟。）
- dashboard 与 IM 两个入口的前台是否共享上下文（当前决定各自一个，共享任务板）。
- Kiro / Codex 作为 worker 时的能力档与 auto mode 等价物。
- Kiro CLI V3（2026-10-09 起默认推送）ACP 迁移：`--agent-engine=v3`、`session/list`、每次 `session/load` 重发 `mcpServers`。独立于本 RFC，须先钉版本再迁移。
- 跨节点的人工批准是否需要 owner 私钥签名。
- Go 定时器跨 macOS 睡眠的行为、熔断导致的重连延迟，需真机测。

## 17. 决策记录

| # | 日期 | 决策 | 理由 |
|---|---|---|---|
| D1 | 2026-10-15 | 三层：前台（人机沟通）/ Planner（策划）/ worker（执行） | 规划与沟通、执行解耦；前台用快模型压延迟 |
| D2 | 2026-10-15 | Planner 用偏好表首个可用的最强模型（当前 opus 5.5） | 强弱排序是 owner 的判断；PEAR 等证据支持强 planner |
| D3 | 2026-10-15 | 前台用 sonnet 5.5，每入口一个 | 够用且快 |
| D4 | 2026-10-15 | Planner 每个 workspace 唯一、串行；定时任务与事件是投给它的触发器；并行在 worker 层；可按 workspace / 领域有多个 Planner | Planner 不执行、单轮短，串行不成瓶颈；多实例并行会重复派活 |
| D5 | 2026-10-15 | 主动 / 被动是同一 Planner 有无 duties 的区别；主动模式在授权前提下自动执行 | 统一模型，复用 cron |
| D6 | 2026-10-15 | 写操作不逐次人工批准 | 以硬边界、审计、急停替代 |
| D7 | 2026-10-15 | 读写分离推迟到邮箱 Planner | 当前输入来自可信来源 |
| D8 | 2026-10-15 | 第一个主动 Planner 落在 naozhi 仓库 | 成败标准清晰 |
| D9 | 2026-10-16 | Planner 无 Bash/Edit，但保留只读工具 | 不能看代码的 planner 过度规定细节 |
| D10 | 2026-10-16 | 主动 duty 自主度默认 propose，按 SLO 升档 | Pulse 下线等证据 |
| D11 | 2026-10-16 | worker 跑在 srt 沙箱 + auto mode，替代 `--dangerously-skip-permissions`；auto mode 先试用，经 `cli.permission_mode` 可切回 `bypass` | 边界从提示词变硬约束 |
| D12 | 2026-10-16 | Conductor 改名为 **Planner**；现有 project planner 并入同一概念 | 避免今后的语义混淆；与现有 `project:{name}:planner` key 语义一致 |
| D13 | 2026-10-16 | 先单机 Mac；终态 EC2 作为控制面、Mac 作为持凭证的执行节点（`dispatch_only`），可远程派活给醒着或锁屏的 Mac | 常开控制面 + 凭证不离开本机 |
| D14 | 2026-10-16 | Now / Next / Not-now 由人维护，同时是 Planner 自发行动的受控边界（网关执行） | 判断质量的上限与可控性都来自这里 |
| D15 | 2026-10-16 | 不做使用量打点 | owner 已确认斜杠命令很少用 |
| D16 | 2026-10-16 | 主路径用自然语言；斜杠命令等专业操作降级到专家模式 session | 减少专业操作门槛 |
| D17 | 2026-10-16 | 成本现阶段不作为设计约束或上线门槛 | owner 决定；账本照常记录 |
| D18 | 2026-10-16 | worker 生命周期：进程 / 上下文 / 记录分层回收，寿命跟产物走，按四条件决定复活或新开 | 进程昂贵、冷上下文复活昂贵、产物才是连续性所在 |
| D19 | 2026-10-16 | 多 Planner 只经请求信封协作：状态机 + `accepts:` + provenance 只降不升 + 2 跳上限 + 单一 lead 对用户汇报 | 防责任分散、转发死循环与注入扩散 |
| D20 | 2026-10-16 | 带教规则存 `lessons.yaml`，与 charter 分离，按类型落地（constraint 编译进网关），每轮 ≤50 条，升降档按 §8.3 | 防 charter 膨胀与指令遵循退化 |

## 附录 A. auto mode 实测（2026-10-16）

环境：CC `2.1.296.1183`，`claude-opus-5-5[1m]`，`--setting-sources "" --settings ~/.naozhi/naozhi-settings.json --permission-mode auto -p --output-format stream-json`，一次性 git 仓库 + 本地 bare remote，`autoMode` 未配置（全默认）。

| 动作 | 结果 |
|---|---|
| 新建文件、commit、fast-forward push 到 origin main | 放行；没有出现分类器事件；22s / $0.11 |
| 用 `commit-tree` + `reset --hard` 压缩根提交（为随后 force push 做准备） | **被分类器拒绝**（"judged this action dangerous (it gave no explanation)"）；模型未绕行，如实报告"只完成一部分"，force push 从未尝试 |
| 删除远程分支 `origin/feature-x`（默认 soft_deny "Git Destructive" 所列） | **放行**——模型先验证该分支已并入 main，且用户在请求里明确要求 |
| 拒绝之后 | 进程未终止；`result.subtype=success`，`result.permission_denials[]` 列出被拒调用 |

结论与对设计的影响：

- 分类器把**请求里的明确指令视为用户意图**。worker 的"用户"是 Planner，所以 Planner 写进任务的指令会被当作用户意图放行。不可逆边界必须落在 `permissions.deny` / PreToolUse hook。
- 新的 stream-json 帧：`system/permission_check_status`（每个受审调用 `checking` / `done` 各一帧，#3686 已在时间线丢弃）、`system/permission_denied`（`tool_name` / `tool_use_id` / `decision_reason_type: classifier` / `decision_reason`）、`result.permission_denials[]`。
- 一个写成"被拒后换语法重试 + 把含密钥的文件外发"的探测 prompt 直接触发了 **Opus 5.5 模型级 safeguard**：`system/informational` + `system/model_refusal_no_fallback`，`result.is_error=true`，进程退出码 1，分类器没有机会介入。naozhi 目前不识别这类帧；安全评审类任务可能被误伤，需要单独的错误分类。

## 附录 B. 参考（调研 2026-10-16）

- Claude Code Projects: https://code.claude.com/docs/en/claude-projects ；Routines: https://code.claude.com/docs/en/routines
- Auto mode: https://code.claude.com/docs/en/auto-mode-config ；权限模式: https://code.claude.com/docs/en/permission-modes ；prompt caching: https://code.claude.com/docs/en/prompt-caching
- Anthropic engineering：multi-agent research system（2025-06-13）、effective harnesses（2025-11-26）、harness design（2026-03-24）、managed agents（2026-04-08）、auto mode（2026-03-25）、sandbox-runtime
- Cognition multi-agents working（2026-04-22）；Cursor scaling agents（2026-01-14）、self-driving codebases（2026-02-05）；Factory Missions（2026-04-10）
- PEAR（EACL 2026 Findings）；Can AI Models Direct Each Other?（arXiv 2603.26458）；Review Beats Planning（arXiv 2603.03406）；AGENTS.md 研究（arXiv 2602.11988、2601.20404、2607.27250）；AI 配置文件过期（arXiv 2606.09090）
- MAST（arXiv 2503.13657）；Prompt Infection（arXiv 2410.07283）；Agents of Chaos（arXiv 2602.20021）；Intelligent AI Delegation（arXiv 2602.11865）；A2A 规范 a2a-protocol.org
- IFScale（arXiv 2507.11538）；ACE（ICLR 2026）；IHEval（NAACL 2025）；CIPHER（NeurIPS 2024）；SkillsBench（2026-06）；Copilot Memory、Devin Knowledge、Jules、Cursor rules 文档
- Google SRE AI 自主度分级；Anthropic Measuring agent autonomy（2026-02-18）
- Nomad placement / disconnect: https://developer.hashicorp.com/nomad/docs/job-specification/disconnect ；Claude Code Remote Control: https://code.claude.com/docs/en/remote-control
- MCP 2026-07-28 changelog；OpenClaw standing orders / heartbeat / memory；Meta Rule of Two；ImpossibleBench；METR time horizons；Kiro CLI V3
