# RFC: 在远端打开 Planner——经节点链路操作这台 Mac

- 状态: Draft v1（2026-11-06；实验见 §8，实现随 PR 一起交付；决策 D96 记在 `planner.md` §18）
- 相关: `planner.md` §10（三个面）、§11（多节点，D13）、§15.5（D86 远程节点链路补强）、§15.9 / §15.10（托管）、P7；`reverse-protocol.md`；`node-pairing.md`
- 范围: 用户在日本 EC2 上的 naozhi（primary）打开 Planner 页面时，能直接操作以反连节点 `pc` 接入的这台 Mac：派活到 Mac 的工作区、查看进展、接收汇报、托管。远端会话列表照旧同步所有节点的会话，但要按节点分组。

## 1. 现状（基于 `9334b77c` 核实，实验见 §8）

- 本机部署：Mac 上的 naozhi（`planner.enabled: true`）以 `upstream:` 反连 EC2 的 `/ws-node`，节点 ID `pc`。反连上只有 primary 发起 RPC（`fetch_*`、`send`、`takeover`、文件 RPC 等），节点每 10 秒被轮询一次会话列表。
- Planner 的一切都在 Mac 上，而且只在 Mac 上成立：
  - "懂你"只读导入 Mac 上的 auto-memory 与 `~/.claude/CLAUDE.md`；
  - 巡检（§15.6）用的邮件 / Slack / Asana MCP 走 Mac 上的 Midway；
  - 派出的会话向 Planner 汇报走 **CC 原生跨会话消息**（`SendMessage`，D81），只在同一台机器的会话之间可达；
  - 回执、托管（`planner/custody`）、旁路文件、任务、"等你"、事件日志都写在 Mac 的状态根目录里，Planner 用 Read / Grep 读 `index.json` 与事件日志。
- 远端看到的只有会话快照与经反连转发的事件流：
  - E2：节点的 `planner:main`（以及 `cron:` / `scout:` 这类本应隐藏的 key）原样出现在远端的会话列表里；
  - E3：节点上 Planner 派出的 `task:` 会话在远端没有 `purpose` / `purpose_title` / `project`，进不了"Planner 派出"分组，也进不了工作区分组；
  - 远端没有 `/api/planner`、`/api/tasks`、`/api/inbox`、`/api/custody` 的节点版本，Planner 页面只能显示 primary 自己的 Planner（EC2 上没有懂你、没有巡检）；
  - 经 `/api/sessions/send` 发给节点 `planner:main` 的消息被节点按命名空间拒绝（正确），但 primary 的 HTTP 应答仍是 `accepted`（拒绝只进了 primary 日志），见 E2'。

## 2. 远端 Planner 与本机 Planner 的关系（D96 ①，**需你拍板**）

三种做法：

| 做法 | 说明 | 代价 |
|---|---|---|
| A. EC2 跑 Planner，派活跨节点路由（D13 终态 + `dispatch-v1`） | EC2 的 Planner 用 `dispatch` RPC 把 `task:` 派到 Mac，汇报 / 回执 / 托管 / 事件日志跨节点回传 | EC2 上没有懂你与巡检，要另搬或复制；CC 原生跨会话消息跨不了机器，汇报要网关改道；Planner 读不到 Mac 的 `index.json` 与事件日志，要镜像；托管引擎要能远程 `Deliver`；Mac 睡眠时本来就派不了活。估算 3–4k 行，且是一套新链路 |
| B. **Planner 留在持有工作区与凭证的 Mac 上，远端是它的窗口（推荐）** | 全局仍只有一个 Planner（D60）：Mac 的 `planner:main`。EC2 的 Planner 页面经反连"借用"它：对话流沿用现有的远程事件转发，读写类接口经一个新 RPC 转给节点自己的处理器 | 新增一个 RPC 与一个转发路由、前端按节点取接口；Mac 离线时远端只能看最后的快照（这时 Mac 也干不了活） |
| C. 两边各一个 Planner | 违反 D60（上下文割裂、指令冲突、责任分散） | — |

**推荐 B。** 理由：

1. Planner 的价值来自 Mac 上的东西——懂你、巡检、跨会话消息、事件日志、托管，全部是本机事实。B 一样都不搬，原样复用 v0.2.0–v0.5.2 已验收的整条本机链路（实验 E4：节点上派活 → 执行 → `SendMessage` 汇报 → `status done` 用了 24 秒）。
2. 远端缺的只是"窗口"：对话流已经能经反连实时看到（E1 / E5），要补的是十几个读写接口的转发。与 A 相比，新代码少一个数量级，也不新增状态。
3. Mac 睡眠或断线时，派往 Mac 的活本来就跑不了（§11：`blocked(waiting_node)`），Planner 跟着 Mac 下线不损失可用性；反过来 EC2 宕机时 Mac 本机照常可用。
4. 符合 D60：EC2 上 `planner.enabled` 保持关闭（发布版默认就是关），全局一个 Planner。

**改判**：D13 的"终态 EC2 作为控制面（含 Planner）"改为"Planner 跟着持凭证的执行节点走；控制面只做窗口与路由"。`dispatch-v1` 不删，留给"Mac 的 Planner 要把活派到 EC2 / 别的节点"那一天（§7）。

**要你拍板**：
- ① 接受 B 与 D13 的改判；
- ② EC2 的 `planner.enabled` 保持关闭（若已打开，请关掉；两边都开时页面会让你选看哪一个，但这违反 D60，不推荐）。

## 3. 派活怎么落到节点的工作区（D96 ②）

不变：派活由 Mac 的 Planner 发出，网关按 `workspaces.json` 里**本节点 ID** 下的路径解析（`Registry.Find` → `paths[<本节点 ID>]`，§15.1），在 Mac 上拉起 `task:` 会话。

- 远端的你通过 Planner 对话说"把 X 交给 naozhi 工作区"，消息经转发进入 Mac 的 Planner，和你在 Mac 上直接说完全一样（回合来源是"你"，不是外部内容，§15.6 的拒绝规则照旧）。
- 登记表里只有别的节点路径的工作区（例如只在 EC2 上的目录），Planner 派活会收到现有的"工作区「X」在这台机器上没有目录"回执，不跨节点（§7）。
- `paths` 已经按节点分，不改格式。

## 4. 汇报、回执、托管、事件日志怎么跨节点打通（D96 ③）

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

**远程会话的用途与分组**：节点在 `fetch_sessions` 的快照里补上 `purpose` / `purpose_title`（与本机 `/api/sessions` 同一个来源：旁路文件），远程的 `task:` 会话就能进"Planner 派出"分组。primary 合并远程会话时，按与本机相同的规则（`hiddenFromList`）隐去 `planner:main`、`cron:`、`sys:`、`scout:`、`scratch:`（修 E2；订阅与事件读取按 key 走，不受影响）。

## 5. 会话列表分组与手机端（D96 ④）

- **按节点分组**：会话列表里有不止一个节点（`/api/sessions` 的 `nodes` 多于本机）时，最上层是节点组："本机 · <名字>"、"<节点 ID> · <名字>"；节点组里沿用现在的分组顺序（"Planner 派出" → 收藏的工作区 → 其余工作区 → 按目录名的工作区 → 未分组）。只有本机时不显示节点组头，与现在一模一样。
- **排序**：跑着 Planner 的节点排第一（你最常从远端看的就是它），其余按本机、再按节点 ID。
- **默认展开**：全部展开；你折叠过的节点记在本机（localStorage，与工作区的折叠同一处），离线的节点也默认展开、组头标"离线"，会话按最后一次快照显示。不加设置项。
- **组头**：名字 + 状态（离线 / 出错）+ 会话数；折叠后只剩组头。
- **手机端**：同一结构；节点组头是一行（名字 + 状态 + 数量，点一下折叠），不吸顶；工作区组只有一个会话时照旧不显示工作区组头、卡片上写组名（D94）。
- **Planner 页面**：跑 Planner 的不是本机时，页面头部显示"Planner · <节点名>"；本机与节点都开着 Planner 时，头部给一个切换（选择记在本机）。任务卡、"等你"卡里的会话按 Planner 所在节点打开。

## 6. 断线与节点失联的降级；鉴权边界（D96 ⑤）

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
- 方向不变：节点不能向 primary 发起任何请求；Mac 的 Planner 不能操作 EC2。
- 经转发进入 Planner 的消息是"你"的回合，与在 Mac 上直接发等同；外部内容触发的回合（巡检、会前简报）照旧拒绝派活与改状态（§15.6）。
- 不加确认步骤、不加配置项（D88、D94 与做减法原则）。

## 7. 范围外（不做）

- 跨节点派活（Mac 的 Planner → EC2 或别的节点的工作区；EC2 的 Planner → Mac），即 `dispatch-v1`、`fetch_manifest`、放置规则、离线状态机——仍按 §11 / P7 第二段，等真有"活要派到另一台机器"时再做。
- 在 EC2 落盘节点的事件日志、旁路文件或"等你"（不复制）。
- 托管远端节点以外的会话：Mac 的托管只管 Mac 上的会话；EC2 本机会话的托管不在本 RFC。
- 经转发给 Planner 发图片 / 附件（上传区在 primary，节点读不到）；以后可以让 primary 把上传转存到节点，再加回。
- 远端的推送：节点的 macOS 通知仍只在 Mac 上弹；远端浏览器的"等你"计数来自节点的 `/api/inbox`（页面开着时轮询），不做 Web Push（随 iOS app，D80）。
- `naozhi join` 配对、系统视图"节点"区块（`node-pairing.md`）。
- 多个 Planner 之间的协作。

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

**结论**：做法 B 的前提都成立——对话流与实时事件现成可用（E1、E5），本机闭环不用改（E4），缺的正是 E2 / E2' / E3 与读写接口的转发，以及按 E6 定的超时与降级。

## 9. 实现与验收（2026-11-06，同一对隔离实例换上本分支的二进制）

**实现**（按 §4–§6，不加配置项、不加确认步骤）：

| 层 | 改动 |
|---|---|
| 节点协议（`internal/node/api.go`） | 能力位 `planner`；`APIRequest` / `APIResponse`；路由白名单 `apiRoutes` 与 `CheckAPIRequest`（方法、规范路径、2 MiB）；`ReverseConn.CallAPI`（反连 RPC `api`）与 `HTTPClient.CallAPI`（直连节点按 `X-Naozhi-Caps` 里的 `planner` 逐次轮询学到，转发同一路径并带节点 token） |
| 节点侧（`internal/upstream/connector_api.go`） | `ServePlanner`：`api` RPC 交给只挂白名单路由的 mux（用 `httptest.Recorder` 在进程内执行，再查一次白名单）；`fetch_sessions` 补 `purpose` / `purpose_title`；有 Planner 时 register 帧带 `planner` |
| 服务端（`internal/server/node_api.go`、`routes.go`） | `mountRoutes` 把白名单里的路由同时挂到节点 mux（同一个处理器，不经鉴权链）；primary 的 `/api/remote/{node}/{path...}`（GET / POST / PUT / DELETE，在鉴权链之后）：未知节点 400、不在白名单 404、带附件 400、无 Planner 501、未连接 503、超时 504、其他 502 |
| 会话列表（`internal/dashboard/session/list.go`） | 合并远程会话时按 `hiddenFromList` 隐去 `planner:` / `cron:` / `sys:` / `scout:` / `scratch:`；有 Planner 时 `X-Naozhi-Caps` 带 `planner` |
| 前端 | 新模块 `planner_host.js`（Planner 在哪个节点、`nodeAPI` / `plannerAPI` / `eventsURL`）；Planner 页面、任务、托管、旁注、健康卡片、"交给 Planner"、提问卡片的答复都按 Planner 所在节点取接口；`planner:main` 订阅带 `node`；"等你"合并本机与 Planner 节点两份（卡片 ID `<节点>|<ID>`，关闭 / 回答回到对应节点），角标两份相加，Planner 在别的节点时每 15 秒轮询它的 `/api/inbox`，新出现的提问 / 错误在页面在后台时弹浏览器通知；头部显示节点名，两边都开 Planner 时给切换；节点离线时状态"离线"、说明一行、输入框停用；经转发不显示附件按钮；会话列表多节点时按节点分组（`nodeHeaderHtml`，Planner 所在节点排第一并带"Planner"标签，折叠记在 `nz_collapsedProjects`） |

**验收**：

| # | 项 | 结果 |
|---|---|---|
| V1 | E2 / E3 修复 | primary `/api/sessions` 不再列节点的 `planner:main`；节点的 `task:` 会话带 `purpose=task` 与 `purpose_title`，进"Planner 派出"组；节点能力位多了 `planner` |
| V2 | 只经 primary 的接口闭环 | `POST /api/remote/pc/planner/send` → 6.2 秒任务出现在 `/api/remote/pc/tasks` → 27.7 秒 `done`；primary 读到节点的派活回执、`peer_message` 汇报、Planner 结论，`/api/remote/pc/sessions/notes` 读到派活与汇报两条旁注，文件 `remote.txt` 内容正确（脚本 `flow.py`） |
| V3 | 真浏览器走通（Playwright，桌面与手机） | 在 primary 页面的 Planner 输入框发消息 → 1.5 秒发出 → 7.4 秒"任务"里出现派出的会话 → 28 秒对话里出现汇报 → 47 秒 Planner 在对话里告诉用户结果；从"任务"打开派出的会话，`#pl-sess` 的 `data-node=pc`、事件来自节点；会话列表节点组顺序"MBP 模拟（Planner）→ 本机 · EC2 模拟"；两次走查页面零 JS 错误（脚本 `ui_flow.cjs`） |
| V4 | 节点卡死时的转发接口（E8） | `kill -STOP` 节点：第一个转发请求 15.0 秒回 504；之后链路的写超时叠加，个别请求 21.7 秒才回 504；primary 的节点状态约 39 秒变 `error`，页面据此显示"离线"、停用输入框、停止轮询它的"等你"；约 90 秒反连被判断开，此后转发立即回 503；`SIGCONT` 后 2.2 秒恢复 200 |
| V5 | e2e（mock-server） | 新增 `test/e2e/remote_planner.test.js` 5 个用例：按节点分组（桌面 / 手机，含折叠跨刷新保持）、远端 Planner 派活与汇报（桌面 / 手机：订阅帧带 `node`、附件按钮停用、角标算节点的提问、关闭节点的"等你"、发消息、派活卡片、汇报、从任务打开节点的会话）、节点离线；mock-server 新增 `remote` 选项 |

截图与脚本留在本机 `~/workspace/naozhi/tmp/remote-planner/`（仓库的 `tmp/` 已被忽略）。

**已知局限**（不在本 PR 修，按需再做）：

- 托管的"你在旁边"只认本机标签页发出的 `viewing` 帧，远端页面正看着 Mac 上的会话时 Mac 不知道；最近 5 分钟发过消息的规则照常生效（经转发的发送就是普通发送）。结果是远端看着时托管可能照常替你回话，你在屏幕上看得到、可以撤回或关掉。
- 节点的 `session_note`、`inbox_count`、`notify` 帧不经反连转发：远端旁注在打开会话时读一次，"等你"每 15 秒轮询一次。
- 转发超时在节点卡死时会被反连的写超时拉长到二十多秒（V4）。
