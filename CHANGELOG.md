# Changelog

该项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 的格式。版本号按语义化版本（Semantic Versioning）管理。
本文件只归档对用户 / 运维可感知的变更；GitHub Release 正文由 `generate_release_notes` 生成（PR 列表）。

## [Unreleased]

### Removed

- **BREAKING：删除没人改的配置开关**（#PRNUM）。下面这些键不再读取，行为固定为原默认值；配置里还留着时启动报 `config-deprecated`（服务照常启动，`naozhi config check` 以 1 退出），删掉即可：
  - `projects.public_tmp` 与 `__public_tmp__` 伪项目：dashboard 从来没有入口，只能手工调 API。现在这个名字按普通项目处理（不存在则 404）。`include_root` 的 UID / 敏感文件名 / 非常规文件类型闸门和审计日志不变；
  - `session.project_stable_key`：项目级稳定 session key 始终开启，`/api/projects` 与 `stats.projects` 始终带 `stableKey`；
  - `session.shim.buffer_size` / `max_buffer_bytes` / `disconnect_watchdog` / `max_shims`：固定为 10000 行 / 50MB / 30m / 50 个 shim。`session.shim.state_dir` 与 `idle_timeout` 保留；
  - `log.format`（始终输出 JSON）、`log.stdio_max_size`（stdout / stderr 文件始终按 64MB 截断）；
  - `sysession.tick_timeout`（固定 30s）、`sysession.runner.jsonl_max_age`（sys-session 日志固定保留 7 天）。
- **BREAKING：删除 `GET /api/debug/pprof` 与配置键 `server.debug_mode`**（#3780）。在线 profile 端点不再注册，请求会收到 404；配置里还留着 `server.debug_mode` 时启动报 `config-deprecated`（服务照常启动），删掉这一键即可。`docs/ops/pprof.md` 一并删除。`naozhi doctor` 去掉了 `pprof` 检查和末尾的 `=== CLI Backends === / === Reverse Nodes ===` 段；各 backend 的 `cli backend <id>` 检查保留，协议与能力改看 `GET /api/cli/backends`。
- **BREAKING：删除 `GET /api/debug/vars` 与全部 expvar 计数器**（#3770）。`internal/metrics` 包和各包自带的 `naozhi_*` 计数器 / gauge（session、cron、dispatch、webhook、upstream、node、workflow、eventlog、attachment 等）一并移除，`naozhi doctor` 的 `expvar` 检查也去掉了。`/health` 里的统计（eventlog、attachment_tracker、dispatch、active 等）不受影响，数值不变。抓取 `/api/debug/vars` 的脚本升级后会收到 404。
- **BREAKING：删除飞书 webhook 接入模式，只保留 WebSocket 长连接**（#3767）。`POST /webhook/feishu` 不再注册；配置键 `platforms.feishu.connection_mode` 与 `platforms.feishu.allow_insecure_webhook` 不再读取，启动时报 `config-deprecated`（服务照常启动，飞书始终走长连接），环境变量 `NAOZHI_ALLOW_INSECURE_WEBHOOK` 也不再生效。原先用 `connection_mode: webhook` 的部署，请在飞书开发者后台把事件订阅和卡片回调都改成「使用长连接接收」，否则收不到消息；然后删掉这两个键。`verification_token` / `encrypt_key` 保留。
- **BREAKING：删除 `naozhi cost backfill` 与 `naozhi cost reconcile` 子命令**（#3765）。这两个是一次性修账工具（导入历史 run 为 `Kind=backfill`、追加 `Kind=adjust` 冲回 #3097 之前 `--resume` 恢复额），账本已修过一次，之后的记账不会再产生这类偏差。`naozhi cost ...` 现在是未知子命令。账本读写、已写入的 backfill / adjust 行、`/api/cost/*` 与 `cost.budget` 不变；配置键无变化。

### Changed

- **`config.example.yaml` 注释精简**（#3776）：从 634 行压到 415 行，删掉了已移除拼写、`auto_chain` 沿革之类的考古注释；所有键、取值和注释掉的示例都没变。`im_access` 段原先被出站 webhook 的注释从中间截断，现在拆成两段，并删掉了「改动需要重启」这句错误说明（`im_access` 支持 `naozhi config reload` 热加载）。

## [0.1.48] - 2026-10-26

这一版有两部分：一是对 10 月重构的 review 修复，二是一轮「做减法」。从 0.1.47 起 +7.8k / −120k 行，删掉的主要是自建治理工具、读源码的元测试、历史文档，以及几项没人用的功能。基本功能保持不变：dashboard、claude / kiro / codex 会话经 shim 跑且重启后存活、cron、agents、projects / planner、IM 收发（含图片与语音）、远程多节点、自升级。升级前请先看下面的 **Removed**。

### Added

- **`cli.permission_mode`**（#3686）：`""` / `bypass`（默认，argv 与之前逐字节相同）或 `auto`（渲染 `--permission-mode auto`，不再带 `--dangerously-skip-permissions`），可以试用 Claude auto mode，不行再切回。只对 Claude 生效，kiro / codex 不变。

### Changed

- **dashboard CSP**（#3737）：删除没有浏览器实现的 `require-sri-for` 和同源资源的 SRI pin；`style-src` 允许 `'unsafe-inline'`，KaTeX / mermaid 的样式直接生效。渲染 mermaid 时那 52 条 CSP 控制台报错没了（#3661）。`script-src` 仍然只允许 `'self'` 加 hash。
- **cron 输入校验合成一层**（#3739），统一在 scheduler 写入路径上做。无效 prompt / schedule 的报错改为 `invalid prompt: ...` / `invalid schedule expression`，其他字段统一返回通用 400，不再逐字段给出提示。
- **开发与 CI**，不影响运行：
  - 删除 ratchet 审批账本、comment-budget、`lint-server-handlers`、JS 结构治理工具链、TS checkJs 与 JSON schema 快照层、读 Go / JS 源码的元测试、leakcheck（#3718 #3719 #3720 #3721 #3723 #3724 #3725 #3731 #3732）；
  - Playwright 收缩到 12 个冒烟 spec（#3735）；
  - CI 去掉 build-windows、e2e-mobile-safari 和 flaky 自动登记，必过检查收敛成一个聚合的 `test`；
  - 删除零引用包与死代码（#3728 #3738）、历史文档（#3726）；CLAUDE.md / README 瘦身（#3741）。
- 依赖：Go 依赖 minor / patch 升级（#3688 #3729）。

### Fixed

- **出站 webhook**（#3706）：
  - 启动窗口内 cron / sysession 的 run 事件不再丢失，其中包括重启前被中断那次 run 的 `run.ended`（#3691）；
  - 不再跟随重定向转发带签名的请求体；sysession 的 `run.ended` 不再带零值时间戳；Close 之后 Deliver 不再 panic（#3658）。
- **会话 resume**（#3704）：ACP / codex 握手阶段 CLI 因为 auth、MCP 配置或缺运行时退出，不再被当成「resume 被拒」而永久丢掉 resume，修好登录后对话能接着来（#3692）。cron 在 spawn 阶段失败时，通知里写明原因（#3631）。
- **IM 附件**：
  - Slack 一条消息里的附件共享一个总下载预算，不再每个文件各等 3 分钟、占住 dispatch 槽约 15 分钟（#3700，#3693）；
  - Slack 和飞书在下载前就拒收不支持的文件，例如 docx、zip、视频（#3695）；
  - 拒收提示按原因区分；全仓使用同一份图片白名单，Discord 的 bmp / ico 不再发给模型导致 400；macOS 上 `/tmp` 实际解析为 `/private/tmp`，IM 出站图片因此一直失效，已修复（#3707，#3627）。
- **Slack**（#3702）：bot ID 未知时，bot 自己上传文件产生的 `file_share` 不再作为入站消息回流（#3653）；README 改为订阅 `message.*` 事件（#3625）。
- **飞书**（#3711）：能读取 post 富文本消息；其他不支持的类型会回复说明，不再静默丢弃（#3626）。
- **IM 准入**：
  - 媒体下载和语音转写也受 `im_rate_limit` / `cost.budget` 约束；`not_admin` 拒绝回复做节流；AskUserQuestion 回合不再把当天唯一一次预算提醒吃掉（#3703，#3652 #3694）；
  - 提问卡片只在 dispatch 接受点击后才标为「已回答」，未授权用户不能再作废别人的提问（#3708，#3628）。
- **dashboard**：
  - 在 agent drill-in 里切走会话再切回，父会话不再跳到顶部；换 agent 或返回父会话时会退订 agent tailer；drill-in 期间父会话整页重绘不再覆盖 agent 面板（#3709，#3696 #3697 #3698）；
  - 新会话首发被拒后保留所选的 backend / tuning；KaTeX 异步渲染完成后去掉 pending 样式；时长和字节在取整边界正确进位（1000ms、60s、1024 KiB）（#3710，#3633 #3634）。
- **配置热重载**（#3712）：
  - `workspace.id` 默认值在主机名变化时不再让每次重载都报 `restart_required`；
  - 引用 settings.json env 键的 `${VAR}` 在启动时也会展开；
  - doctor 不再把 `config_loaded_at` 当成进程启动时间（#3655 #3621 #3620）。
- **cron**（#3715）：本地 run 的回合以未请求的 `aborted_*` 结束时记为失败，不再记成功（#3629）。
- **session**（#3714）：恢复的 workflow 若带有未来时间的 `last_observed_at`，会截到当前时间，不再无限拉长会话空闲 TTL（#3657）。
- **`naozhi models sync`**（#3713）：探测的 claude 和实际 spawn 的是同一个 binary（#3632）。
- **server**（#3716）：mermaid 只常驻 gzip 形式，降低空闲 RSS（#3662）；Router 改在构造入口统一校验（#3638）。
- **`naozhi setup weixin`**（#3701）：扫码轮询能处理 iLink 新增的 redirect / verifycode 状态（#3635）。

### Removed

本轮减法里的 breaking 项。残留的已删配置键不会让服务起不来，只报一条 WARN（`config-deprecated`），但 `naozhi config check` 会因此退出 1，请手动删掉。

- **BREAKING：删除 config schema 迁移链与 `naozhi config migrate` 子命令**（#3730）。`Load` 不再在内存里改写 schema v1 旧键，`naozhi config migrate [-write]` 已移除（现在是 usage 错误，退出 2），也不再生成 `<config>.pre-migrate-v<N>` 备份。下列旧键仍能加载但**不再生效**，升级前请手动改名：
  - `nodes:` → `workspaces:`（不改的话远程节点不会被配置）
  - `session.workspace` → `session.cwd`（不改则回落到默认 cwd）
  - `agents[].args` 里的 `--append-system-prompt` → `agents[<id>].system_prompt`（报 `argv-denylist`；不改则该 agent 没有 system prompt）
  - `session.auto_chain` → 直接删掉
  - `schema_version` 仍保留：比 binary 新的版本照旧拒绝加载；`schema_version: abc` 改报通用类型错误
- **BREAKING：删除 Prometheus 导出 `GET /metrics` 与配置键 `server.metrics_enabled`**（#3736）。`naozhi doctor` 的 `metrics` 检查一并移除。计数器仍在 `/api/debug/vars`（需开 `server.debug_mode`）以 expvar JSON 暴露，名称不变；抓取 `/metrics` 升级后会收到 404。
- **BREAKING：删除五个没有调用方的 dashboard API**（#3734）：`GET /api/planner/stats`、`GET /api/cron/preview`、`GET /api/cost/entries`、`POST /api/system/labels/clear-origin`、`POST /api/auth/logout`。自己写脚本调用它们的会收到 404。
- **已鉴权的 dashboard 接口不再按 IP 限流**（#3722）：发送、上传、cron、项目文件、cost、memory 等接口不再返回 429。登录、WS 握手、未鉴权入口的限流，以及 IM 的 `im_rate_limit` 不变。
- **BREAKING：删除 cron 的 AgentCore 云沙箱放置**（#3740）。配置键 `cron.sandbox`（`runtime_arn` / `region`）不再读取，启动时报 `config-deprecated`；`POST` / `PATCH /api/cron` 去掉 `placement` / `side_effects` 字段，外部客户端仍带这两个字段会收到 400；已保存的 `placement: "sandbox"` 任务加载后为**暂停**状态（不会悄悄改在本机运行），下次保存时这个键会被去掉。本地 cron 执行路径不变。
- **移除 v0.1.42 引入的一次性 `.naozhi/project.yaml` 桩文件清理**（#3728）：从 v0.1.40 之前的版本直接升级的用户，如仓库里残留只含 `created_at` 的 `.naozhi/project.yaml`，请手动删除；`projects-index.json` 里遗留的 `stub_cleanup_done` 字段会被忽略。

---

## 历史版本

0.1.42–0.1.44 只保留升级须知的要点；更早及完整条目见 `git log -p CHANGELOG.md`。

## [0.1.42] - 2026-10-05

### 升级须知

- **几类配置现在在加载阶段就失败**（#3148、#3157、#3155、#3227）：`agent_commands` 指向未定义的 agent、`server.dashboard_token` 短于 8 个字符、不合法的 `projects.exclude` glob 直接加载失败（`config check` 退出 2）；duration / size 值解析不了时报 `config-invalid`（`config check` 退出 1），运行时仍用默认值
- **未配置时 turn watchdog 的默认值放宽为无输出 15m / 总时长 2h**（#3194）：Claude 运行工具时每 30s 发 `tool_progress` 心跳；ACP / codex 没有心跳，不出声的工具必须在 15m 内完成
- **`agents[].access_profile` 和 `agents[].backend` 开始真正生效**（#3247、#3279、#3292）：设了 `access_profile` 的 agent 改用该 profile 的凭证，且不能派到远端 node；设了 `backend` 的 agent 的新 IM 会话和 cron run 落到该 backend
- **cron 行为变化**（#3145、#3172、#3192、#3203、#3158）：IM 里 `/cron add` 默认每次新会话（`--keep-context` 延续上下文）；连续失败 `cron.auto_pause_after_failures`（默认 5）次自动暂停；`is_error` 的 turn 记为 `failed`
- **多节点先升级 primary，再升级 node**；**stdout/stderr 日志文件超过 `log.stdio_max_size`（默认 64MB）会被原地截断**（#3154，见 `docs/ops/disk-budget.md`）；`/health` 平台字段改为实时连接状态（#3218 等）；`/api/sessions` 不再返回 `history_sessions`，改用 `GET /api/sessions/history`（#3208）

## [0.1.43] - 2026-10-05

### 升级须知

- **默认 backend 发生回落时，`naozhi config check` 会报出来，退出码随之变化**（#3381、#3389）
- **`access_profiles[].default_backend` 开始生效**（#3364）：新会话按「显式请求 / 项目 / dashboard 选择 > `agents[].backend` > profile 的 `default_backend` > `cli.backend`」选 backend；设了 `default_access_profile` 且其 profile 写了 `default_backend` 的部署，新会话都会落到那个 backend
- **planner 的启动参数只来自项目**（#3342、#3357）：不再继承 `agents.general` 的 `model` / `args` / `system_prompt` / `effort`
- **cron 自动暂停不再计入不是 job 自身原因的失败**（#3345）
- **weixin 的 bot token 过期（iLink `-14`）后，轮询暂停 1 小时**（#3360）；discord 网关掉线后用 REST 探测 bot token（#3367）

## [0.1.44] - 2026-10-07

### 升级须知

- **没配 `im_access` 的部署，`naozhi config check` 从退出 0 变为退出 1**（#3442、#3473）：不配置时行为不变（全放行），但每个已启用、没有条目的平台都会告警；`naozhi setup weixin` 会把扫码用户写进 `im_access`（#3555）
- **SIGHUP 现在是重载配置，不再结束进程**（#3554）：等价于 `naozhi config reload` / `systemctl reload naozhi`；停进程用 `SIGTERM` / `SIGINT`
- **Slack / 飞书群聊默认按话题分开会话，回复留在原话题里**（#3446、#3569、#3583）：`session.group_scope` 默认 `thread`，可改 `chat` / `user`
- **dashboard 的 backend picker 默认改为「自动」**（#3477、#3500、#3586）
- **新增**：IM 渠道接收 PDF / 文本文件（#3571 等）、`im_rate_limit`（#3528）、`cost.budget`（#3542 等）、出站 webhook `integrations.webhooks`（#3594）、dashboard 展示 Claude Code Workflow 进度（#3475）；dashboard 不再访问外部 CDN（#3567、#3577）

## [0.1.45] - 2026-10-09

### Fixed

- **dashboard**
  - 会话时间线不再出现只写着 `commands_changed` 的 ⚙ 行（#3680）：这个帧只是 CLI 告知斜杠命令列表变了（插件或 MCP 加载完成），不带任何可展示的内容，现在不进 event log。已经落盘的旧行不做清理
  - drill-in 一个已经结束的 agent 时，面包屑显示 agent 实际的运行时长（#3668）。以前显示的是 tailer 存活的时长，例如面板上跑了 45.9s 的 agent 显示成 206ms
  - 没有时间戳的 agent 事件，在 HTTP 分页和 WS 推送重叠时、以及每次 HTTP 轮询时，不再重复出现（#3676）
  - 发送消息后，之前留在后台的 workflow / Bash 任务发来进度时，不再提前清掉「已发送，正在处理…」（#3671）

### Security

- **用 Go 1.26.9 构建**（#3682）：修复 GO-2026-6617。Go 1.26.6 的 `net/http` 里，HTTP/2 服务端在编码响应头的同时处理客户端发来的 `SETTINGS_HEADER_TABLE_SIZE` 时存在数据竞争，恶意客户端可以借此让进程崩溃

## [0.1.46] - 2026-10-18

### Fixed

- **dashboard**
  - 回合结束时不再把正在往上翻看的阅读位置拽到最底部（#3687）：回合进行中追加发送的消息会排队，它真正的用户事件要等上一回合结束才推到页面，以前一到就强制滚到底。现在本页自己发出的消息只在发送那一刻滚到底，回显到达时停在底部才跟随；IM 里其他人发来的消息仍然滚到底以保证可见

## [0.1.47] - 2026-10-21

### Fixed

- **dashboard**
  - 往上翻看的会话不再因为后台重新订阅而跳到最底部（#3689）：会话空闲超过 `session.ttl` 后进程被回收，约 1 分钟后服务端结束这条订阅，页面随后从头重新订阅、整页重绘，以前一律滚到底。进程复活、WS 断线重连、HTTP 轮询的全量拉取也走这条路。现在重绘前记下视口顶部那条消息，重绘后放回原处；原本在底部的继续贴底，重绘带进 IM 里其他人的新消息时仍然滚到底以保证可见
  - 切走再切回会话时，按当时看的那条消息恢复位置，不再按距底距离估算，期间多出的新消息不会把位置推偏（#3689）

### Security

- **用 Go 1.26.10 构建**（#3690）：修复 GO-2026-6809（CVE-2026-111943）。系统信任库没有及时更新的环境里，Go 1.26.9 的 `crypto/x509` 仍会接受一批已不再可信的中间证书；1.26.10 内置了这份黑名单，`Verify` 遇到它们时返回错误
