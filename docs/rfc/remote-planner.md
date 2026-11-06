# RFC: 每台机器一个 Planner——在远端页面切换着用

- 状态: Draft v2（2026-11-06；v1 推荐"全局一个 Planner、EC2 只做窗口"，owner 当天改判为每个节点一个 Planner，v2 按此重写 §1–§3、§7 并补实验 E9、验收 V6–V8；决策 D96 与 D60 修订记在 `planner.md` §18）
- 相关: `planner.md` §10（三个面）、§11（多节点，D13）、§15.5（D86 远程节点链路补强）、§15.6（巡检）、§15.9 / §15.10（托管）、P7；`reverse-protocol.md`；`node-pairing.md`
- 范围: 日本 EC2 上的 naozhi（primary）与以反连节点 `pc` 接入的这台 Mac 各跑一个 Planner，各管本机。用户在 EC2 页面上可以切换到 Mac 的 Planner（这时页面只是它的窗口），直接操作 Mac：派活到 Mac 的工作区、查看进展、接收汇报、托管。远端会话列表照旧同步所有节点的会话，但要按节点分组。

## 1. 现状（基于 `9334b77c` 核实，实验见 §8）

- 本机部署：Mac 上的 naozhi（`planner.enabled: true`）以 `upstream:` 反连 EC2 的 `/ws-node`，节点 ID `pc`。反连上只有 primary 发起 RPC（`fetch_*`、`send`、`takeover`、文件 RPC 等），节点每 10 秒被轮询一次会话列表。
- EC2 上的 naozhi（v0.5.3）**也已经开着 Planner**（`planner.enabled: true`，状态根目录 `~/.naozhi/planner/` 在，`planner:main` 已拉起但还没有过回合，E9）。
- 两个 Planner 各自的"本机事实"：
  - "懂你"只读导入本机的 auto-memory 与 `~/.claude/CLAUDE.md`（Mac 与 EC2 各有一份，内容不同）；
  - 巡检（§15.6）用的邮件 / Slack / Asana MCP 只配在 Mac 上（EC2 的日志一直是 `scout: off`）；
  - 派出的会话向 Planner 汇报走 **CC 原生跨会话消息**（`SendMessage`，D81），只在同一台机器的会话之间可达；
  - 回执、托管（`planner/custody`）、旁路文件、任务、"等你"、承诺、早报、事件日志都写在各自的状态根目录里，Planner 用 Read / Grep 读本机的 `index.json` 与事件日志。
- 远端看到的只有会话快照与经反连转发的事件流：
  - E2：节点的 `planner:main`（以及 `cron:` / `scout:` 这类本应隐藏的 key）原样出现在远端的会话列表里；
  - E3：节点上 Planner 派出的 `task:` 会话在远端没有 `purpose` / `purpose_title` / `project`，进不了"Planner 派出"分组，也进不了工作区分组；
  - 远端没有 `/api/planner`、`/api/tasks`、`/api/inbox`、`/api/custody` 的节点版本，Planner 页面只能显示 primary 自己的 Planner；
  - 经 `/api/sessions/send` 发给节点 `planner:main` 的消息被节点按命名空间拒绝（正确），但 primary 的 HTTP 应答仍是 `accepted`（拒绝只进了 primary 日志），见 E2'。
  - **重复**：primary 的健康台账把节点的会话也当成自己的单元（`KindRemote`），EC2 的 `index.json` 里已经有 `pc` 的会话（E9）。同一个 Mac 会话失败，Mac 与 EC2 会各开一条"等你"、各推一次；EC2 的早报也会列 Mac 会话的完成与失败。

## 2. 两个 Planner 的关系（D96 ①，D60 修订）

**D60 修订**：从"全局唯一 Planner"改为"**每个节点一个 Planner，只管本节点**"。D60 当初要防的是上下文割裂、指令冲突、责任分散；按节点划分管理范围后，同一件事只有一个 Planner 负责，这三点仍然成立。

| | EC2 的 Planner | Mac 的 Planner |
|---|---|---|
| 管什么 | EC2 本机的 naozhi：EC2 上的会话、cron、工作区（`workspaces.json` 里 `ec2-dev` 下的路径） | Mac 本机的 naozhi：Mac 上的会话、cron、工作区（`pc` 下的路径） |
| `index.json`、健康台账 | 只有 EC2 的单元；跑着 Planner 的节点（`pc`）的会话不进来（§4） | 只有 Mac 的单元 |
| 派活、托管、承诺与任务、"等你"、交接摘要、事件日志 | 只在 EC2 状态根目录，只覆盖 EC2 | 只在 Mac 状态根目录，只覆盖 Mac |
| 懂你 | EC2 的 auto-memory 与 `~/.claude/CLAUDE.md` | Mac 的 auto-memory 与 `~/.claude/CLAUDE.md` |
| 巡检（邮件 / 日历 / Slack / Asana） | 不跑（没配这些 MCP） | 跑 |
| 早报 | 不发（§2.1） | 发 |
| 互相读写 | 不读不写对方的任何状态；不给对方派活 | 同左 |

**页面**：两边都开着 Planner 时，Planner 页面头部的切换是正式功能（默认本机的 Planner，选择记在本机）。切到 Mac 时 EC2 页面只是 Mac Planner 的窗口：对话流沿用现有的远程事件转发，读写类接口经新 RPC `api` 转给 Mac 自己的处理器（§4）。切换后对话、派活、汇报、任务、"等你"及其角标、托管、旁注都只落在所选节点上，不合并两边。

**要你确认**：EC2 的 `planner.enabled` 保持打开（现在就是开的，E9）。

**改判**：D13 的"终态 EC2 作为控制面（含唯一的 Planner）"改为"每个节点一个 Planner；EC2 页面可以切换着看"。`dispatch-v1` 不删，留到哪天要跨节点派活时再做（§7）。

### 2.1 巡检与早报只在一个节点上（D96 ②）

目标：同一件事不推两次。做法都是确定性的，不新增配置项：

- **巡检**：维持现有规则，`cli.mcp_config` 里配了邮件 / Slack / Asana 中任一 MCP 才跑（`scout.Enabled`）。EC2 没配，所以只有 Mac 跑。
- **早报**：多节点部署（配了 `workspaces`、`reverse_nodes` 或 `upstream` 之一）里，只有跑巡检的节点发早报（`brief.Config.Elsewhere`）。单机部署不受影响，照旧发。所以 EC2 不发，Mac 发。EC2 的 Planner 用 `notify` 动作块要求"记入早报"时，网关拒绝并回执原因（"早报只在跑巡检的那台机器上发"），Planner 改成直接提醒或问用户。
- **"等你"与推送**：跑着自己 Planner 的节点，它的会话不进 primary 的健康台账（§4），所以一个会话失败只由它所在节点开一条、推一次。EC2 页面切到 Mac 时，Mac 新出现的提问与错误由页面轮询发现，只弹一次浏览器通知（V8）。
- 代价：EC2 本机会话的完成与失败不进任何早报；它们的失败照旧进 EC2 的"等你"并按级别推送。

## 3. 派活怎么落到节点的工作区（D96 ③）

不变：每个 Planner 只往本节点派活。网关按 `workspaces.json` 里**本节点 ID** 下的路径解析（`Registry.Find` → `paths[<本节点 ID>]`，§15.1），在本机拉起 `task:` 会话。

- 在 EC2 页面切到 Mac 的 Planner 后说"把 X 交给 naozhi 工作区"，消息经转发进入 Mac 的 Planner，和在 Mac 上直接说完全一样（回合来源是"你"，不是外部内容，§15.6 的拒绝规则照旧）。切回 EC2 的 Planner 说的话，就派到 EC2 的工作区。
- 登记表里只有别的节点路径的工作区，Planner 派活会收到现有的"工作区「X」在这台机器上没有目录"回执，不跨节点（§7）。
- `paths` 已经按节点分，不改格式。两个节点各有自己的登记表。

## 4. 汇报、回执、托管、事件日志怎么跨节点打通（D96 ④）

原则：**留在会话运行的那台机器上，远端就地读写，不复制。**

| 项 | 在哪里 | 远端怎么看 / 怎么操作 |
|---|---|---|
| Planner 对话（含汇报 `peer_message`、早报、回执后的回复） | Mac 的 `planner:main` 事件日志 | 现有远程事件转发：订阅 `planner:main` + `node`，首页走 `/api/sessions/events?node=`（E1：远端读到了派活回执与汇报；E5：一轮结束 2.2 秒后远端收到） |
| 汇报（派出的会话 → Planner） | Mac 本机 CC 跨会话消息 | 不变；远端看到的是 Planner 收到后在对话里说的话 |
| 回执（网关 → Planner） | Mac 的 `planner/action` | 不变 |
| 任务、"等你"、承诺、早报、旁注 | Mac 的状态根目录 | 新 RPC `api` 转给 Mac 自己的处理器（见下） |
| 托管（开关、暂停、撤回、出错重试） | Mac 的 `planner/custody` | 同上；会话头部的托管 chip 对远程会话改读写该节点的 `/api/custody` |
| 事件日志 | Mac 的 `events/` | 不变；远端经 `fetch_events` / 订阅读取，不在 EC2 落盘 |

**新 RPC：`api`（能力位 `planner`）**

- 节点只在 `planner.enabled` 时申报能力位 `planner`（反连在 register 帧；直连节点在 `/api/sessions` 应答的 `X-Naozhi-Caps` 头）。
- 请求 `{method, path, query, body}`，应答 `{status, body}`；节点在进程内把它交给一个**只挂了 Planner 相关路由**的 `ServeMux`（与对外的 dashboard mux 用同一批处理器，不经过鉴权链——反连本身已认证）。允许的路由：

  | 路由 |
  |---|
  | `GET /api/planner`、`POST /api/planner/send`、`POST /api/planner/state`、`GET /api/planner/briefs`、`GET /api/planner/notes` |
  | `GET /api/inbox`、`POST /api/inbox/close`、`POST /api/inbox/decide`、`PUT/DELETE /api/inbox/pause` |
  | `GET /api/tasks`、`POST /api/tasks/state`、`POST /api/tasks/stop` |
  | `GET/PUT /api/custody`、`PUT/DELETE /api/custody/pause`、`POST /api/custody/undo` |
  | `GET /api/sessions/notes`、`GET /api/system/health` |

  其余路径节点回 404，primary 也不转发（两边各查一次）。请求体上限 2 MiB（与 `/api/planner/send` 相同），应答上限沿用反连 RPC 的总上限；超时 15 秒（E6：节点卡死时一个 RPC 能挂 70 秒以上，界面不能等这么久）。
- primary 路由：`/api/remote/{node}/{path...}`（GET / POST / PUT / DELETE），在 dashboard 鉴权链之后，转发 `/api/{path}`。节点未连接 → 503，节点不支持 → 501，超时 → 504。直连节点用它自己的 token 走 HTTP 转发同一路径。
- `POST /api/planner/send` 的 `file_ids` 指向 primary 的上传区，节点上找不到：经转发发 Planner 时界面不显示附件按钮，primary 也拒绝带 `file_ids` 的转发（400，§7）。

**健康台账不收跑着 Planner 的节点**：primary 的健康台账（以及由它而来的 `index.json`、"等你"、早报、单元名字）不再收能力位为 `planner` 的节点的会话与节点状态（`Server.RemoteSessions`）。那些会话由它们自己的 Planner 负责；没跑 Planner 的节点照旧由 primary 覆盖。系统视图的"全会话健康"随之只列本机单元；要看 Mac 的，切到 Mac 的 Planner 页面看。

**远程会话的用途与分组**：节点在 `fetch_sessions` 的快照里补上 `purpose` / `purpose_title`（与本机 `/api/sessions` 同一个来源：旁路文件），远程的 `task:` 会话就能进"Planner 派出"分组。primary 合并远程会话时，按与本机相同的规则（`hiddenFromList`）隐去 `planner:main`、`cron:`、`sys:`、`scout:`、`scratch:`（修 E2；订阅与事件读取按 key 走，不受影响）。

## 5. 会话列表分组与手机端（D96 ⑤）

- **按节点分组**：会话列表里有不止一个节点（`/api/sessions` 的 `nodes` 多于本机）时，最上层是节点组："本机 · <名字>"、"<节点 ID> · <名字>"；节点组里沿用现在的分组顺序（"Planner 派出" → 收藏的工作区 → 其余工作区 → 按目录名的工作区 → 未分组）。只有本机时不显示节点组头，与现在一模一样。
- **排序**：Planner 页面当前所选的那个节点排第一，其余按本机、再按节点 ID；跑着 Planner 的节点组头都带"Planner"标签。
- **默认展开**：全部展开；你折叠过的节点记在本机（localStorage，与工作区的折叠同一处），离线的节点也默认展开、组头标"离线"，会话按最后一次快照显示。不加设置项。
- **组头**：名字 + 状态（离线 / 出错）+ 会话数；折叠后只剩组头。
- **手机端**：同一结构；节点组头是一行（名字 + 状态 + 数量，点一下折叠），不吸顶；工作区组只有一个会话时照旧不显示工作区组头、卡片上写组名（D94）。
- **Planner 页面**：不止一个节点开着 Planner 时，头部是一个切换（"本机 · <名字>" / 各节点名，默认本机，选择记在本机）；只有别的节点开着时只显示那个节点的名字。任务卡、"等你"卡里的会话按所选节点打开。

## 6. 断线与节点失联的降级；鉴权边界（D96 ⑥）

**降级**：

| 情况 | 表现 |
|---|---|
| 节点断线 / 失联（反连断开，或 RPC 超时） | Planner 页面头部显示"<节点名> 离线 · 最后在线 M/D HH:mm"，输入框与按钮停用；已加载的对话、任务、"等你"保留不清空；会话列表的节点组标"离线"、保留最后快照（现有缓存行为）。转发接口立即回 503（未连接）或在 15 秒内回 504（卡住），不让界面挂着 |
| 恢复 | 节点组状态回到正常后，Planner 页面重新拉取状态、任务、"等你"，并重新订阅 `planner:main`（现有反连重连会补发漏掉的事件，`resubscribeAll`） |
| Mac 睡眠 | 与断线相同；Mac 上的会话、托管、Planner 都随 Mac 暂停，醒来后按现有睡眠识别继续（§15.1），远端不做任何补偿 |
| EC2 宕机 | Mac 不受影响，本机 dashboard 照常可用 |

**鉴权边界**：

- 节点只认 primary：`api` RPC 只来自节点自己拨出、用节点 token 认证过的那条反连（直连节点则是带它自己 dashboard token 的请求），与现有的 `send` / `takeover` / `update_config` 同一信任级别——primary 本来就能替你给节点上的任何会话发消息。
- 节点侧再收一次：只服务 §4 表里的路由（节点 mux 上只挂了这些），方法必须匹配，路径必须是规范形式（以 `/api/` 开头、不含 `..`、`//`、反斜杠、控制字符），请求体 2 MiB 上限。
- 方向不变：节点不能向 primary 发起任何请求；Mac 的 Planner 不能操作 EC2，EC2 的 Planner 也不读写 Mac 的状态（页面切到 Mac 时，是你在操作 Mac 的 Planner，不是 EC2 的 Planner）。
- 经转发进入 Planner 的消息是"你"的回合，与在 Mac 上直接发等同；外部内容触发的回合（巡检、会前简报）照旧拒绝派活与改状态（§15.6）。
- 不加确认步骤、不加配置项（D88、D94 与做减法原则）。

## 7. 范围外（不做）

- 跨节点派活（Mac 的 Planner → EC2 或别的节点的工作区；EC2 的 Planner → Mac），即 `dispatch-v1`、`fetch_manifest`、放置规则、离线状态机——仍按 §11 / P7 第二段，等真有"活要派到另一台机器"时再做。
- 在 EC2 落盘节点的事件日志、旁路文件或"等你"（不复制）。
- 一个 Planner 管另一个节点的会话：托管、派活、承诺都不跨节点。
- 经转发给 Planner 发图片 / 附件（上传区在 primary，节点读不到）；以后可以让 primary 把上传转存到节点，再加回。
- 远端的推送：节点的 macOS 通知仍只在 Mac 上弹；EC2 页面切到 Mac 时，"等你"计数来自 Mac 的 `/api/inbox`（每 15 秒轮询），不做 Web Push（随 iOS app，D80）。
- `naozhi join` 配对、系统视图"节点"区块（`node-pairing.md`）。
- 两个 Planner 之间的协作（互发消息、互相委托）。

## 8. 实验（2026-11-06，隔离双实例）

**环境**：同一台 Mac 上两个隔离实例，都不碰 :8180 与 EC2。primary `127.0.0.1:8191`（`planner.enabled` 关，`reverse_nodes: {pc}`），节点 `127.0.0.1:8192`（`planner.enabled: true`，`upstream: ws://127.0.0.1:8191/ws-node`，`insecure: true`），各自独立的 `store_path`、`session.shim.state_dir` 与 `XDG_RUNTIME_DIR`，复制一份 `naozhi-settings.json`（同一份 Bedrock 凭证）；工作区 `/tmp/rp-exp/ws/demo`（一个空 git 仓库）。二进制：`master@9334b77c`。脚本在本机 `/tmp/rp-scripts/`。

| # | 假设 | 结果 |
|---|---|---|
| E1 | primary 能读到节点 Planner 的对话流 | ✅ `GET /api/sessions/events?key=planner:main&node=pc` 读到 13 条：你的消息（带快照）、派活回执、"已派出"、`peer_message` 汇报、Planner 的结论 |
| E2 | 节点的隐藏 key 会漏进远端列表 | ✅（bug）远端 `/api/sessions` 列出了节点的 `planner:main` |
| E2' | 经 `/api/sessions/send` 发给节点 `planner:main` | 节点拒绝（"Planner 的消息请从 Planner 页面发送"），但 primary 回 `{"status":"accepted"}`，拒绝只进日志 → 远端发 Planner 必须走专门的转发，不能复用会话发送 |
| E3 | 远端 `task:` 会话带用途 | ❌ 节点本机 `purpose=task`、`purpose_title`、`project=demo` 都有，远端三项全空 |
| E4 | 节点本机闭环：派活 → 执行 → 汇报 | ✅ 节点 `/api/planner/send` 下达 → 拉起 `task:e164cecb3224` → README 追加一行 → `SendMessage` 汇报 → Planner 读文件核对后 `status done`，共 24 秒 |
| E5 | 远端实时看到 Planner 的回合 | ✅ 经 primary `/ws` 订阅 `planner:main`（`node: pc`）：`session_state running` 立即到达，事件以 `history` 帧推来，一轮结束后 2.2 秒到达 |
| E6 | 节点卡死时的表现 | `kill -STOP` 节点：primary 的节点状态 70 秒内仍是 `ok`、约 85 秒变 `error`；这期间一个转发事件的请求挂满客户端的 70 秒超时，之后的请求 12.8 秒回 502；`SIGCONT` 后 2.2 秒恢复 `ok` → 转发 RPC 要有自己的短超时（15 秒），界面按状态与超时降级 |
| E7 | 转发 RPC 的开销 | 本机回环：节点直连 p50 3.3 ms，经 primary 转发 p50 3.0 ms（反连 RPC 本身几乎不加开销；真实链路的耗时取决于 CloudFront 往返）；Planner 接口应答都很小（`/api/tasks` 346 B、`/api/inbox` 54 B、`/api/planner` 124 B） |

| E9 | EC2 上的 Planner 能不能跑（只读 SSM，3 次，不改 EC2） | 服务 v0.5.3、`/health` ok；`planner.enabled: true`，状态根 `~/.naozhi/planner/`（`agent.json` 当天班次、`index.json` 114 个单元，**其中有 `pc` 节点的**，`workspaces.json` 只有 `ec2-dev` 的路径）；`planner:main` 12:11 UTC 已拉起，但还没有事件日志（没跑过回合）；claude 2.1.318 在 `~/.local/bin/claude`；`naozhi_settings` 没开（没有 `naozhi-settings.json`，用的是 `~/.naozhi/claude-settings.json`，`model: opus`，env 里没有 AWS / Bedrock 变量）；实例角色 `AmazonSSMRoleForInstancesQuickSetup`；auto-memory 有 19 份索引（含 `workspace-naozhi` 的）和 `~/.claude/CLAUDE.md`；没有巡检用的 MCP，日志一直是 `scout: off`；11-06 08:30 UTC 发过一份早报（2 项完成、1 项例行） |

**结论**：前提都成立。对话流与实时事件现成可用（E1、E5），本机闭环不用改（E4）；缺的是 E2 / E2' / E3、读写接口的转发、按 E6 定的超时与降级，以及 E9 暴露的健康台账跨节点重复。

**E9 没能确认、发版前要确认的**：
1. Planner 固定用 `claude-opus-5-5[1m]`。EC2 的 claude 用什么凭证、能不能用这个模型：设置里没有 Bedrock 变量，`~/.naozhi/env` 只列出了飞书与反连 token 的变量名。最省事的验证是在 EC2 页面给 EC2 的 Planner 发一句话，看有没有回复。
2. Planner 用 `--setting-sources ""` 启动。`naozhi_settings` 没开时它读哪份设置、认证是否还在，要随第 1 条一起看结果。
3. EC2 的"懂你"是 EC2 自己那 19 份 auto-memory，和 Mac 的不同。这符合"各管各的"，但它不知道 Mac 上的偏好。

## 9. 实现与验收（2026-11-06，同一对隔离实例换上本分支的二进制）

**实现**（按 §4–§6，不加配置项、不加确认步骤）：

| 层 | 改动 |
|---|---|
| 节点协议（`internal/node/api.go`） | 能力位 `planner`；`APIRequest` / `APIResponse`；路由白名单 `apiRoutes` 与 `CheckAPIRequest`（方法、规范路径、2 MiB）；`ReverseConn.CallAPI`（反连 RPC `api`）与 `HTTPClient.CallAPI`（直连节点按 `X-Naozhi-Caps` 里的 `planner` 逐次轮询学到，转发同一路径并带节点 token） |
| 节点侧（`internal/upstream/connector_api.go`） | `ServePlanner`：`api` RPC 交给只挂白名单路由的 mux（用 `httptest.Recorder` 在进程内执行，再查一次白名单）；`fetch_sessions` 补 `purpose` / `purpose_title`；有 Planner 时 register 帧带 `planner` |
| 服务端（`internal/server/node_api.go`、`routes.go`） | `mountRoutes` 把白名单里的路由同时挂到节点 mux（同一个处理器，不经鉴权链）；primary 的 `/api/remote/{node}/{path...}`（GET / POST / PUT / DELETE，在鉴权链之后）：未知节点 400、不在白名单 404、带附件 400、无 Planner 501、未连接 503、超时 504、其他 502 |
| 会话列表（`internal/dashboard/session/list.go`） | 合并远程会话时按 `hiddenFromList` 隐去 `planner:` / `cron:` / `sys:` / `scout:` / `scratch:`；有 Planner 时 `X-Naozhi-Caps` 带 `planner` |
| 健康台账（`internal/server/discovery_cache.go`） | `RemoteSessions` 不再给健康台账与单元名字带跑着 Planner 的节点（v2，修 E9 的重复） |
| 早报（`internal/planner/brief`） | `Config.Elsewhere`：多节点部署且本节点不跑巡检时不发早报，`Hold` 回 `ErrElsewhere`（v2，§2.1） |
| 前端 | 新模块 `planner_host.js`（Planner 在哪个节点、`nodeAPI` / `plannerAPI` / `eventsURL`）；Planner 页面、任务、托管、旁注、健康卡片、"交给 Planner"、提问卡片的答复都按所选节点取接口；`planner:main` 订阅带 `node`，切换时先退订旧节点；"等你"列表与角标只算所选节点的（v2；卡片 ID `<节点>|<ID>`，关闭 / 回答回到对应节点），所选节点不是本机时每 15 秒轮询它的 `/api/inbox`，新出现的提问 / 错误在页面在后台时弹浏览器通知；暂停推送与托管只作用于所选节点；头部显示节点名，多个节点开着 Planner 时是切换；节点离线时状态"离线"、说明一行、输入框停用；经转发不显示附件按钮；会话列表多节点时按节点分组（`nodeHeaderHtml`，Planner 所在节点排第一并带"Planner"标签，折叠记在 `nz_collapsedProjects`） |

**验收**：

| # | 项 | 结果 |
|---|---|---|
| V1 | E2 / E3 修复 | primary `/api/sessions` 不再列节点的 `planner:main`；节点的 `task:` 会话带 `purpose=task` 与 `purpose_title`，进"Planner 派出"组；节点能力位多了 `planner` |
| V2 | 只经 primary 的接口闭环 | `POST /api/remote/pc/planner/send` → 6.2 秒任务出现在 `/api/remote/pc/tasks` → 27.7 秒 `done`；primary 读到节点的派活回执、`peer_message` 汇报、Planner 结论，`/api/remote/pc/sessions/notes` 读到派活与汇报两条旁注，文件 `remote.txt` 内容正确（脚本 `flow.py`） |
| V3 | 真浏览器走通（Playwright，桌面与手机） | 在 primary 页面的 Planner 输入框发消息 → 1.5 秒发出 → 7.4 秒"任务"里出现派出的会话 → 28 秒对话里出现汇报 → 47 秒 Planner 在对话里告诉用户结果；从"任务"打开派出的会话，`#pl-sess` 的 `data-node=pc`、事件来自节点；会话列表节点组顺序"MBP 模拟（Planner）→ 本机 · EC2 模拟"；两次走查页面零 JS 错误（脚本 `ui_flow.cjs`） |
| V4 | 节点卡死时的转发接口（E8） | `kill -STOP` 节点：第一个转发请求 15.0 秒回 504；之后链路的写超时叠加，个别请求 21.7 秒才回 504；primary 的节点状态约 39 秒变 `error`，页面据此显示"离线"、停用输入框、停止轮询它的"等你"；约 90 秒反连被判断开，此后转发立即回 503；`SIGCONT` 后 2.2 秒恢复 200 |
| V6 | 两边都开 Planner，各自派活（v2，脚本 `both.py`） | 同时给 EC2 与 Mac 的 Planner 发派活：40.8 秒内两边各派出一个 `task:` 并完成，`ec2-*.txt` 只出现在 EC2 的工作区，`mac-*.txt` 只出现在 Mac 的；EC2 的 `/api/tasks` 只有自己的任务，Mac 的也只有自己的；EC2 健康台账的单元只剩 `planner:main` 与自己的任务（修复前混着 `pc` 的），Mac 的只有 Mac 的；两边的托管列表、"等你"互不包含对方 |
| V7 | 真浏览器切换（v2，脚本 `ui_switch.cjs`，桌面与手机） | 两个节点组头都带"Planner"标签；Planner 页面默认本机，切换选项"本机 · EC2 模拟 / MBP 模拟"；EC2 的对话只有 `ec2-*`，切到 Mac 后只有 `mac-*`；任务列表随之换成对方的、不混；切换后会话列表把所选节点排到第一；零 JS 错误 |
| V8 | 通知不重复（v2） | 页面置于后台、切到 Mac 的 Planner，让两个 Planner 各用 `ask` 问一个问题：浏览器通知恰好两条（EC2 的经本机推送帧、Mac 的经 15 秒轮询），等过两轮轮询没有重复；角标是 Mac 的 1 条 |
| V5 | e2e（mock-server） | 新增 `test/e2e/remote_planner.test.js` 7 个用例（v2 加两个"两边都开、切换不串"）：按节点分组（桌面 / 手机，含折叠跨刷新保持）、远端 Planner 派活与汇报（桌面 / 手机：订阅帧带 `node`、附件按钮停用、角标算节点的提问、关闭节点的"等你"、发消息、派活卡片、汇报、从任务打开节点的会话）、节点离线；mock-server 新增 `remote` 选项 |

v2 的用例"两边都开 Planner 时切换"（桌面 / 手机）检查：默认本机、切换选项文字、对话与订阅帧（退订本机、订阅 `pc`）、角标与"等你"只算所选节点、任务只列所选节点的、发消息只到所选节点（本机的 `/api/planner/send` 与 `/api/remote/pc/planner/send` 各一次）、切换跨刷新保持。

截图与脚本留在本机 `~/workspace/naozhi/tmp/remote-planner/`（仓库的 `tmp/` 已被忽略）。

**已知局限**（不在本 PR 修，按需再做）：

- 托管的"你在旁边"只认本机标签页发出的 `viewing` 帧，远端页面正看着 Mac 上的会话时 Mac 不知道；最近 5 分钟发过消息的规则照常生效（经转发的发送就是普通发送）。结果是远端看着时托管可能照常替你回话，你在屏幕上看得到、可以撤回或关掉。
- 节点的 `session_note`、`inbox_count`、`notify` 帧不经反连转发：远端旁注在打开会话时读一次，"等你"每 15 秒轮询一次。
- 转发超时在节点卡死时会被反连的写超时拉长到二十多秒（V4）。
