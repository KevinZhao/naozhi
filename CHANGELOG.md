# Changelog

该项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 的格式。版本号按语义化版本（Semantic Versioning）管理。
本文件只归档对用户 / 运维可感知的变更；GitHub Release 正文由 `generate_release_notes` 生成（PR 列表）。

## [Unreleased]

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
