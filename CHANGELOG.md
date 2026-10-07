# Changelog

该项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 的格式。版本号按语义化版本（Semantic Versioning）管理。

真正的 per-round 变更日志曾放在 `docs/TODO.md` 顶部；该文件已于 2026-05-26
删除（待办迁至 GitHub Issues，映射见 `docs/rfc/todo-to-issues-migration.md`）。
本文件只归档对用户 / 运维可感知的大型变更。

## [Unreleased]

### Added

- **Dashboard 版本提示与一键生效**（见 `docs/rfc/dashboard-update-notice.md`）：侧栏 header 新增一枚版本 chip，把此前只存在于日志里的"新版本已就绪"暴露出来，点击后确认即可让新版本生效。默认 `mode: download` 下后台 checker 发现新版本后数秒内就装好 binary，但生效要等重启——本项目自己的部署曾因此空转 22 小时，界面上毫无信号。
  - chip 区分两种状态并给出**相反**的操作：`install`（远端有新版本、磁盘未替换）与 `restart`（binary 已 staged，只需重启）。判定在服务端算好后由 `action` 字段下发，浏览器不做版本比较。这是正确性问题而非展示问题：`Replace()` 备份的是"当前磁盘上的 binary"，所以在 staged 态再装一次会用新版本覆盖 `.bak`，摧毁唯一可回滚的版本
  - 新增 `GET /api/system/update`（状态 + 预检 + 回滚命令）与 `POST /api/system/update/apply`（202 + 后台执行；`confirm_action` 须回传前端看到的 action，不一致返回 409）。不可操作时（dev build / 平台无 release 资产 / install 目录不可写 / 无受管服务）UI 给手工命令而不是一个点了必失败的按钮
  - 新增配置 `update.dashboard_install`（默认 true）：置 false 保留只读提示，apply 端点返回 403
  - **macOS 重启链修复**（同时修好 CLI `naozhi upgrade` 与后台 `mode: auto`）：launchd label 改从 `XPC_SERVICE_NAME` 读取并校验该 job 确实跑本 binary（硬编码常量与实际部署不符时 `ServiceRunning()` 返回 false，使每条重启路径**静默**跳过）；`restartLaunchd` 改用 `launchctl kickstart -k`（原先的 `unload`+`load` 对自重启不成立，`unload` 摘掉的正是发起调用的 job）
- **Attachment 引用计数**（见 `docs/rfc/attachment-refcount.md`）:在 event log 之上再加一层,每个 image attachment 的 `.meta` 现在记录"哪些 session 的 event log 引用了我"+ "最近一次引用时间"。`GCWithRefs(workspace, uploadTTL, refTTL, now)` 按 `(uploaded_at + uploadTTL) AND (last_referenced_at + refTTL)` 双过期判定,大图可在 refTTL(默认 30 天)内持续可见,而不是按 uploadTTL 固定 7 天强删。`/health.attachment_tracker` 暴露 tracker 的 writer_alive / channel 分量 / written_total / cleared_total / dropped_total / meta_error_total;`/debug/vars` 新增 `naozhi_attachment_ref_{bump,clear,meta_error,drop}_total` 4 个 expvar counter。旧 Meta 文件(无新字段)向后兼容,GCWithRefs 对它们走 legacy 单 TTL 路径以避免升级日大量误删。
- **Event log 持久化**（见 `docs/rfc/event-log-persistence.md`）：naozhi 现在把每个 session 的 `EventEntry` 落盘到 `~/.naozhi/events/<keyhash>.log`,带 length-prefix framing + 稀疏 idx sidecar 保证崩溃恢复一致性。好处是切 session、刷新 dashboard、重启服务后,原本只在内存 ring 里的 `Images` / `ImagePaths` / `AskQuestion` / agent-team linkage 等字段仍可见,图片消息不再"切回来就丢"。
  - `EventEntry` 新增 `uuid` 字段(crypto/rand 或从 Claude JSONL uuid 派生),`MergedSource` 用它在本地 tier 与 Claude JSONL fallback 间做精确去重,消除升级期的历史断层
  - `/health.eventlog` 导出 `writer_alive` / `channel_depth` / `channel_cap` / `last_drain_ms_ago` / 5 个计数器 / `fs_type` / `fs_supported`
  - `/debug/vars` 新增 5 个 expvar counter:`naozhi_eventlog_persist_written_total` / `_dropped_total` / `_fsync_total` / `_malformed_lines_total` / `_replay_leak_total`(稳态必须为 0)
  - 启动时 orphan sweep:清理 `events/` 下超过 30 天、stem 不对应任何活跃 session 的孤儿 `.log`/`.idx` 文件
  - 启动时 FS 探测:NFS / overlayfs / tmpfs 等不适合作为持久化目标的文件系统会在启动 slog 告警并在 `/health` 标记 `fs_supported=false`
- **Dashboard lightbox 降级**:点击原图失败(attachment GC 过期)时自动回退到缩略图 data URI;新增 `?v=<time>` cache-busting + `naturalWidth===0` 二次兑底
- **Governance 四件套**（RNEW-DOC-422）：新增 `CONTRIBUTING.md` / `SECURITY.md` / `CHANGELOG.md` / `.github/CODEOWNERS`，外部贡献者不再需要翻 TODO 才能定位流程
- **Dependabot 每周自动扫 gomod + github-actions**（RNEW-OPS-413）
- **config.example.yaml**：顶部新增 *Configuration precedence* 表格（RNEW-ARCH-405），并补充 `cli.backends` 多 backend 示例（RNEW-DOC-421）
- **Dashboard WS 重连加 jitter**（RNEW-UX-001），N 个 tab 同时掉线不再同秒风暴回包
- **Dashboard 后台 tab 暂停 polling**（RNEW-UX-014），手机后台省电省流量
- **触控目标 ≥ 44×44**（RNEW-UX-011），`.btn-dismiss` / `.status-reconnect` 在 `pointer:coarse` 下满足 WCAG 2.5.5

### Changed

- **JSON 状态快照不再跟随末端 symlink**：下列文件统一由 `osutil/jsonfile.Load` 以 `O_NOFOLLOW` 打开，文件本身是 symlink 时按"读不了、但仍在盘上"处理；读取只检查末端，路径中间目录的 symlink 照常跟随。写入不同：数据目录、以及它下面存放这些文件的目录本身不能是 symlink（`datadir.EnsureDir`、runlog、cron sandbox store 拒绝写入），只有数据目录之上的祖先目录照常跟随。此前会跟随的：`sessions.json`、`session-ids.json`、`workspace-overrides.json`、`sessions.meta.json`、uiprefs、retired sessions store、session run-history 记录、shim state、attachment `.meta`、cron sandbox 的 pending / attention / snapshot manifest。`cron_jobs.json` 一直如此，没有变化；projects index 从引入起就是这样
  - session store 的三个主文件（`sessions.json` / `session-ids.json` / `workspace-overrides.json`）：启动照常成功，但这个文件的保存被拒，内存里的改动不落盘。信号是一条 `ERROR session store: refusing to overwrite a file naozhi could not read`、authenticated `/health` 的 `session_store.blocked`，以及 `spawn_diags` 里一条 `layer=store-unreadable`。`sessions.meta.json` 只报告（Warn）不阻塞，下次保存把链接换成普通文件，链接目标不动
  - 旧行为其实也没真正支持过 symlink：`WriteFileAtomic` 是临时文件 + rename，第一次保存就把 symlink 本身换成普通文件，目标文件从此停在旧内容。现在只是把静默分叉变成显式拒绝
  - 迁移：**先停 naozhi**，把真实文件 `mv` 到 symlink 所在位置，再启动 naozhi；或者把 `session.store_path`（`cron.store_path` 同理）直接指向真实位置。要用 bind mount 就挂载**目录**，不要挂载单个文件：保存是 rename 覆盖，被 bind mount 的文件不能被 rename 覆盖（Linux 上 `EBUSY`），每次保存都会失败。数据目录下的其它文件同理，挂载它们的父目录（bind mount），而不是文件本身。不要把数据目录、或直接存放状态文件的目录换成 symlink：`datadir.EnsureDir`、runlog 和 cron sandbox store 会拒绝写入这样的目录，例如 `~/.naozhi` 是 symlink 时 `sessions.json` 等的每次保存都会失败。要把状态放到别处，可行的做法只有两种：把 `store_path` 指向真实位置，或 bind mount 目录
  - naozhi 运行中删掉 symlink，下一次保存就会解除阻塞（#2972），随后把**内存里的状态**写到这个路径。所以不要在运行中先删链接、再把真实文件放回去：只要两步之间发生一次保存，阻塞就会解除，之后的保存会用内存状态覆盖放回去的真实文件
  - 同一改动里，解析失败的 `sessions.meta.json` 改为重命名保留成 `sessions.meta.json.corrupt.<ts>.<nonce>`，不再只记一条 Warn；sidecar 是机器写的，这类文件看过即可删除
- **spawn 门禁告警统一为一行结构化日志，并按 key 去重**：argv、env、能力、配置各层门禁不再各打一句自己的话，统一为 `WARN spawn gate: configured input had no effect layer=… key=… action=… reason=… scope=…`
  - `layer` 取值：`argv-denylist`、`argv-validator`（`--resume` / `--debug-file` / `--mcp-config` / `--append-system-prompt`）、`env-filter`、`caps`、`config-deprecated`、`config-unknown`、`config-invalid`、`store-unreadable`；`action` 取值：`dropped` / `ignored` / `rewritten` / `fallback` / `clamped`
  - 旧文案对照（`scope` 为 session key 的，值就是那个会话的 key）：

    | 旧日志 | layer | scope | key |
    |---|---|---|---|
    | `shim env: rejecting unsafe AWS profile value (credential_process injection guard)` / `… AWS credential file path (path traversal guard)` / `… endpoint base_url` / `shim env: oversized entry rejected` | `env-filter` | `shim-env`（进程环境）或 session key（单次 spawn 的 overlay） | 变量名 |
    | `claude settings env: refusing to propagate auth-source AWS var` / `… CLAUDE_ kill-switch var` / `claude settings env: rejecting unsafe value` / `… unsafe base_url` | `env-filter` | `claude-settings` | 变量名 |
    | `sysession: AWS profile env var rejected (unsafe value)` / `sysession: base-URL env var rejected (unsafe value)` | `env-filter` | `sysession-env` | 变量名 |
    | `cli: --resume rejected by argv validator, spawning fresh session` | `argv-validator` | session key | `--resume` |
    | `cli: AppendSystemPrompt rejected by argv validator, spawning without it` | `argv-validator` | session key | `--append-system-prompt` |
    | （以前没有日志）`--debug-file` / `--mcp-config` 路径不合法被丢弃 | `argv-validator` | session key | `--debug-file` / `--mcp-config` |
    | `config: dropped bare --append-system-prompt with no value` / `config: --append-system-prompt under args is not applied at spawn …` | `config-deprecated` | `config` | `agents[<id>].args` |

  - 去重：`scope` 不是 `config` 时按 (scope, layer, key) 在进程生命周期内去重，首次是 Warn 并计入 metric 与 `/health`，重复只记 Debug、不计数。`config` scope（配置加载、store 读保护）不去重。因此同一会话同一字段反复被拒只留第一条 Warn
  - 被拒绝的值不再写进日志（sysession 以前会打出 sanitized 的值片段），只有 reason 分类；argv-validator 的 reason 只带长度，`--resume` 另带最多 16 字符的前缀
  - 告警建议：按 `msg="spawn gate: configured input had no effect"` 加 `layer=` 字段过滤；或者看 authenticated `/health` 的 `spawn_diags.counts`（`layer|action` → 次数）与 `spawn_diags.recent`；debug 模式下 `/api/debug/vars` 有 `naozhi_spawn_diag_total{layer,action}`。按旧文案写的 grep / metric filter 已经静默失效
  - argv-validator 不豁免去重：shim reconcile 每 30s 会用同一 session key 重新推导 argv 并上报一次，豁免后日志和计数记录的是心跳而不是 spawn 尝试。这一层的丢弃总是 fail safe（新开会话，或不带该字段 spawn），值也不会完整回显，逐次审计的价值有限
- **缺 history factory 的 Warn 前缀**：`cli: no history factory registered for backend; history will be empty` 改为 `history: no history factory registered for backend; history will be empty`（代码从 `internal/cli` 移到了 `internal/history`）。按整句匹配的告警请改为匹配 `no history factory registered`

### Security

- **Multipart Value 字段数上限 32**（RNEW-SEC-001），阻断 padded-body DoS
- **PDF 上传路径显式拒 gzip magic**（RNEW-SEC-002），defence-in-depth
- **Attachment ETag 改为 sha256 前 16 字符**（RNEW-SEC-004），不再通过响应头泄漏纳秒级 mtime
- **`safeUrl` 正则收敛至 `^(https?:|#)`**（RNEW-SEC-007），去除 `mailto:` / `/` 等历史遗留入口

### Fixed

- `spawnSession` panic recover 错误消息不再双前缀 `"spawn process: spawn process:"`（RNEW-009）

### Documentation

- `readLoop` defer 注释按 LIFO 执行序重写，避免未来 reviewer 误判 `isChanAlive` 不变量（RNEW-007）
- `connector.handleRequest` ctx 参数 godoc 列出 appCtx vs connCtx 使用矩阵（RNEW-008）
- `dispatcher.sendAndReply` 显式 `_ = takeoverFn(...)` 并注释为何不 branch（RNEW-010）
- `connector.streamEvents` 加 nil-guard 不变量注释，防未来引入 NPE（RNEW-005）

---

## 历史版本

在 2026-05-07 之前，所有变更记录在已删除的 `docs/TODO.md` 的 `Round NN` 小节里，
不回填到本文件；这些历史条目可通过 `git log -S` 在仓库历史中检索。后续版本发布时，
将抽取对用户可感知的条目归档到这里。

## [0.1.42] - 2026-10-05

### 升级须知

- **几类配置现在在加载阶段就失败，`naozhi config check` 的退出码随之变化**（#3148、#3157、#3155、#3227）
  - `agent_commands` 指向未定义的 agent（`agent_commands["/x"] references undefined agent "ghost"`），或 `server.dashboard_token` 非空但短于 8 个字符（`server.dashboard_token is too short — use at least 8 characters`）：改由 `config.Load` 拒绝。`config check` 对这两种配置以前退出 0，现在退出 2。`config migrate`、`shim list`、`models sync`、`cost` 也会拒绝。服务端以前就会拒绝它们（只是启动到一半才 `os.Exit`），所以能跑起来的部署不受影响
  - 新增的 `projects.exclude` 里写了不合法的 glob：加载失败
  - 以下 key 的值解析不了：`session.shim.idle_timeout`、`disconnect_watchdog`、`max_buffer_bytes`；sysession 的 `tick_timeout`、各 daemon 的 `tick`、`min_rename_interval`、`upload_ttl`、`ref_ttl`、`jsonl_max_age`；`log.stdio_max_size`。以前这类值静默回落到默认值，或只打一条日志。现在会报 `config-invalid` diag：启动时打 Warn，`config check` 退出 1。运行时仍然用默认值
  - 负数的 `jsonl_max_age` 以前等于关掉 sweep，现在回落到 7d 并报告。负数的 `stdio_max_size` 保留 64MB 上限。要关闭请写 `"0"`
  - `config check` 对 sysession / image_orient 默认 backend 的诊断，改为按启动时的实际绑定方式推导。部分以前漏报的配置现在会退出 1
- **多节点集群的升级顺序：先升级 primary，再升级 node**
  - 先升级 node 时，旧 primary 每次节点（重）注册都会打一条 WARN `reverse node advertised unknown capabilities`（新 node 多声明了 subscribe-history 能力，#3171）。注册本身正常，会回落到旧路径
  - 新 primary 轮询 v0.1.41 的 HTTP node 时不发 If-None-Match（旧 node 的 ETag 不随会话状态变化），新 node 才走 304（#3333）
- **升级前打开的 dashboard 标签页请刷新一次**：旧页面的历史弹层会显示为空（见下文 `/api/sessions` 一条），而且旧 JS 里没有版本提示横幅
- **升级重启时恰好退出了 CLI 的会话**：旧版本 shim 在 CLI 退出后还会占用 socket 约 60s，这期间同一会话重建可能失败一次，稍后重发即可（新版本 shim 会被主动回收，#3275）
- **未配置时 turn watchdog 的默认值放宽为无输出 15m / 总时长 2h**（#3194）：没写 `session.watchdog` 的部署从 2m / 5m 变为 15m / 2h；`Router.Cleanup` 的卡死判定（2 倍总时长）从 10m 变为 4h。显式配置的值不变
  - Claude backend 运行工具时每 30s 发一次 `tool_progress` 心跳，不会触发无输出超时。ACP / codex 没有心跳，不出声的工具必须在 15m 内完成
  - passthrough 模式的 turn 现在也受同一个 watchdog 约束（#3191），以前不受
- **`agents[].access_profile` 和 `agents[].backend` 开始真正生效**（#3247、#3279、#3292）：这两个字段以前能通过校验，但从没传到 spawn。已经配置了它们的部署，升级后行为会变：
  - 设了 `access_profile` 的 agent，它的新会话和 cron job 改用该 profile 的凭证和 `default_model`。旧会话如果当时记录的是全局默认（空值；没配 `default_access_profile` 时所有会话都是这样），下次 spawn 会在 agent 的 profile 上 `--resume`。若该 profile 指向另一个 config 目录，就找不到 transcript，会话会作为新会话起来，丢失上下文。已经记录了非空 profile 的会话保持原 profile
  - 这类 key（包括 `cron:<id>`）发往远端 node 的请求一律拒绝（`ErrAccessProfileRemote`）。dashboard 新建的会话 key 都以 `:general` 结尾，所以**给 `agents.general.access_profile` 设值后，dashboard 上除 planner 以外的会话都不能用远端 node**。planner 的 profile 仍只取项目 pin，没有 pin 时取 `default_access_profile`
  - 设了 `backend` 的 agent，新的 IM 会话和 cron run 落到该 backend。持久上下文的 cron 会在下一次运行时切换 backend，transcript 在新 backend 上 resume 不了就开新会话。dashboard 历史面板的 resume、带 `resume_id` 的发送仍走 router 默认 backend
- **默认 backend 的推导规则改了**（#3224）：只影响 `cli.backends` 第一项没写 `id`、又没设 `cli.backend` 的配置。这类配置的默认 backend 从 `claude` 改为第一个有 id 的条目
  - 例如 `[{path}, kiro, claude]` 会改成默认 kiro。sysession 和 image_orient 只接受 claude，于是启动时会被关掉，Warn 为 `sysession manager unavailable; daemons disabled`
  - 修法：显式写 `cli.backend: claude`
- **cron 行为变化**（#3145、#3172、#3192、#3203、#3158）
  - IM 里 `/cron add` 新建的 job 默认每次从新会话开始。要延续上下文，创建时加 `--keep-context`（或 `--keep`）。已有 job 保持原设置，`/cron list` 会给持久上下文的 job 标 `[保留上下文]`。dashboard 的创建入口不变
  - 新增 `cron.auto_pause_after_failures`（默认 5，负数关闭）：连续失败（failed / timed_out）达到这个次数，job 会自动暂停并发一条通知。skipped 和 canceled 不计入；成功、resume、任何一次编辑都会清零。存量 job 从 0 开始计，升级当下不会有 job 被暂停
  - backend 标了 `is_error` 的 turn（如达到 max turns、上下文超限）以前算成功，现在记为 `failed / turn_failed`，计入自动暂停。会话容量不足导致的拒绝改记为 `skipped / session_capacity`，不再算失败。失败通知会写明原因，末尾带 `· run <8 位 id>`
  - 持久上下文的 job 每次运行完就释放 CLI，下次 tick 用 `--resume` 重新启动，每次多几秒 spawn。12 个 cron 豁免名额现在只限制正在运行的 run。释放 CLI 会一并结束本次 run 留下的后台工作（`run_in_background` 的 Bash、后台 Task agent）；依赖这类后台工作的 job 请改用 fresh context。两次运行之间，dashboard 上这类会话显示为 `dead` + `已回收`
- **stdout/stderr 日志文件现在会被原地截断**（#3154）：启动时检查一次，之后每小时一次。条件是 fd 为 O_APPEND 打开的普通文件（launchd 的 `StandardOutPath`、systemd 的 `StandardOutput=append:`），且超过新配置 `log.stdio_max_size`（默认 `"64MB"`）
  - 截断时保留最新的若干整行（上限的 1/8，最多 4MB），并打一条 INFO `stdio log truncated`。升级后第一次检查就会执行
  - 管道、journald 和终端不处理
  - 要保留完整历史，设 `"0"` 自己轮转。macOS 上不要配 newsyslog：它只 rename，naozhi 会继续写进旧 inode
  - systemd 的 `StandardOutput=file:` 不是 O_APPEND，会跳过，只打一次 Warn
  - 详见 `docs/ops/disk-budget.md`
- **一次性清理自动生成的 `.naozhi/project.yaml` 桩文件**（#3138）：旧版本往 `projects.root` 下每个项目里写过只含 `created_at` 的 `.naozhi/project.yaml`，结果每个 git 仓库都多出一个未跟踪的 `.naozhi/`
  - 升级后第一次 Scan 会删除这类文件。只删同时满足以下条件的：内容与自动生成的逐字节一致、是 `.naozhi/` 里唯一的文件、不是 symlink。删完后目录若已空，一并删除；每次删除打一条 Info
  - 每个 root 只清理一次，记录在 `projects-index.json` 的 `stub_cleanup_done` 里
  - 如果曾把这类桩文件 commit 进仓库，git 里会看到一次删除
  - 降级到 v0.1.40 之前的版本会丢失这些项目的排序
- **`/health` 平台字段改为上报实时连接状态**（#3218、#3228、#3239、#3244、#3250）：feishu（websocket 模式）、slack（socket mode）、discord、weixin 的 `platforms.<name>` 以前恒为 `registered`，现在是 `connecting` / `connected` / `disconnected` / `failed`。新增的 `platform_conn.<name>` 带 `since` 和 `last_error`。feishu webhook 模式仍是 `registered`。按 `registered` 写的监控需要改
- **`/api/sessions` 不再返回 `history_sessions`**（#3208）：历史列表移到新接口 `GET /api/sessions/history`（带 ETag，可返回 304），`/api/sessions` 只在 `stats.history_tag` 里带一个版本标记。自己写脚本读这个字段的需要改。升级前打开的旧标签页在刷新之前看到的历史列表是空的
- **`naozhi doctor` 检查更多，退出 1 的条件也更多**（#3175、#3184、#3240、#3276）：用 dashboard token 读一次带鉴权的 `/health`，新增 `cli runtime`、`platforms`、`eventlog writer`、`attachment tracker`、`dispatch` 几项；对每个配置的 backend 跑一次 `<cli> --version`；启用 transcribe 时检查 AWS 凭证链和 ffmpeg
  - 以下情况现在会退出 1：`cli_available=false`；writer 停摆；默认 backend 的 `--version` 失败；默认 id 没有可用 runtime；某个平台 `connecting` / `disconnected` 已满 5 分钟，或已是 `failed`
  - 跑 doctor 的用户和服务用户的环境可能不同，结论以服务用户身份运行为准
  - 详见 `docs/ops/doctor.md`
- **`naozhi upgrade` 拒绝安装不比当前版本新的 release**（#3223）：latest 比当前版本旧时，以前会静默降级，现在拒绝并退出 1（`Latest release vX is not newer than running vY; use --force to install it anyway.`）。`make` 出来的 `vX-N-gabc` 构建、版本号解析不了的构建也一样拒绝。确实要回滚就加 `--force`
- **`naozhi config migrate -write` 会在原文件旁留一份备份**（#3170）：备份名为 `<config>.pre-migrate-v<N>`，权限 0600，**和配置一样含密钥**，不会自动删除。写完会打印回滚用的 `cp -p` 命令，降级前用它恢复（旧版本不认 `schema_version: 2`）。备份写不了，或者配置在 dry run 之后被改过，`-write` 会失败（退出 2），原文件不动
- **新命令 `naozhi cost reconcile`，修正 cost ledger 里历史的重复计费**（#3293，配合 #3097）：#3097 之前，每次 `--resume` 重启都会把恢复出来的 cost-state 累计额记到第一个 turn 上，实测有会话账面达到实际花费的 4 倍。#3097 修好了此后的记账，但已写入的条目还在
  - 用法：`naozhi cost reconcile [-config] [-session <cli-session-id>] [-until YYYY-MM-DD] [-claude-dir] [-write]`
  - 默认只打印对账表；加 `-write` 才写入。写入的是 `Kind=adjust` 修正条目（可以为负），不改已有行
  - 只处理 `-until`（默认今天，UTC）之前的日期；重复运行不会重复追加
  - 写入后要**重启 naozhi**，内存里的汇总才会包含这些修正
  - 只为能证明是 naozhi 自己跑、且没有被别处记过的花费补记（#3335）：跨过午夜或 `-until` 的轮次、resume 之前的历史、终端里跑的（接管前的）轮次、cron 已记的轮次所在的日期一律跳过，并在对账表里列出
- **日志与指标的变化**：按旧文案写的告警规则可能需要调整
  - 稳定状态下的 reconcile tick 不再打 INFO `discovered live shim` / `shim discovery complete`，项目扫描日志也一样（#3189），改为 DEBUG
  - `loaded session history on startup` 改为 `loaded session history from Claude JSONL`，并带 `via` 字段（#3211）
  - CLI 每次非零退出都会打一条带 stderr 尾部的 Warn（#3201）
  - 因会话忙丢弃的消息会打 Info `message dropped: session busy`（#3139）
  - 启动熔断日志的属性名从 `stderr` 改为 `cause`（#3288）
  - `/health` 的 `ws_dropped` 现在统计每一次失败的发送尝试，包括之后重试成功的（#3142）
  - dashboard_token 为空时的告警文案改为 `SECURITY: dashboard_token is empty …`，加载阶段的配置告警输出到 stderr（#3148、#3157）
- **`config.example.yaml` 里的 `trusted_proxy` 改为 `false`**（#3121）：只影响新拷贝模板的部署，已有 config.yaml 不受影响。前面有 ALB / CloudFront / nginx 的部署需要自己改成 `true`

### Added

- **CLI 启动失败会写明原因，反复失败会暂停重试**（#3201、#3207、#3221、#3274、#3288）
  - shim 会保留 CLI 的 stderr 尾部，并按原因分类：认证失败、配置错误、缺少运行时、resume 不可用。IM 回复和 dashboard 显示分类后的中文提示，原始 stderr 不会发到 IM
  - 因 transcript 失效（或原因不明）导致启动失败时，下一条消息不再带 `--resume`，直接开新会话（同一 workspace，历史通过 `prev_session_ids` 关联），IM 用户会收到「之前的会话记录已丢失，已开始新会话。」
  - 第二次连续启动失败起，同一个 key 在冷却期内（30s 起，每次翻倍，最长 10m）不再 spawn，回复「CLI 连续启动失败，已暂停自动重试；请联系管理员，或发送 /new 立即重试。」
- **`projects.exclude`**（#3155）：用文件名 glob（如 `["tmp-*", "archive"]`）把 `projects.root` 下的子目录排除出项目发现
- **dashboard 会发现标签页与服务端版本不一致**（#3186、#3187）：`/static` 资源改为带内容哈希的 URL，缓存头为 `private, max-age=31536000, immutable`，再次打开时不再发任何 static 请求。升级后仍开着的旧标签页会显示一条关不掉的刷新提示，空闲时自动刷新（每个服务端版本最多自动一次）
- **dashboard 补充显示的信息**
  - 「系统」视图：每个 daemon 卡片显示近 30 天的 ledger 花费（#3183）；attachment-gc 在 dry run 下显示可回收的条数和体积（#3169），这是开启真删之前的观察依据
  - cron 执行详情显示该次 run 的 `session_id`（#3281）
  - 退出状态 chip 写明退出码或信号、启动失败原因，以及下一步会发生什么（新开会话、需要管理员处理、暂停到某个时间）（#3270、#3284、#3295）
- **attachment 总量超过 500 MiB 时打启动告警**（#3162）：把各 workspace 下的 `.naozhi/attachments/` 加总，超过 500 MiB 时打 Warn `attachments large`，`hint` 会按 attachment-gc 当前的模式给出下一步建议
- **IM 收到消息后立即给反馈**（#3160、#3166）：发往空闲会话的消息马上加 ⏳（Slack 上是 👀），回复发出后移除。reaction 没加上时，3 秒后补发一条「💭 思考中...」banner，最终答复编辑进这条 banner
- **watchdog 超时的提示会写出正在运行的工具**（#3196）：能区分是模型不出声还是工具一直没返回
- **远端 node 发送的回执带上节点的处理结果**（#3209）：`reset`、`queued`、`busy` 都会反映到 dashboard；HTTP 节点忙时报发送失败，以前会静默丢弃

### Changed

- **reverse node 短暂断线不再影响浏览器订阅**（#3163）：断开 60 秒内不注销，期间浏览器订阅保留，重连后补齐中间的事件。超过 60 秒按原流程注销，只是比以前晚 60 秒
- **转发来的消息走正常的 turn 流程**（#3204）：node 上转发来的消息和 IM、dashboard 消息共用同一个 key 的队列，可以合并成一个 turn，`/new` 会清掉它们，agent 配置也会生效
- **IM 提示调整**
  - 新会话不再提示「新会话已创建（之前的上下文已失效）」（#3156）
  - 失败的 turn 一定会回一条分类后的中文提示，不再沉默，也不再贴原始 RPC 文本（#3202）
  - weixin 在未开启队列时不再发「会话忙」提示（#3139），以免占用一次性 reply token；同时加了每个用户的 context_token 环，连续回复不会再撞上已用过的 token（#3143）
- **长回复按代码块拆分**（#3179、#3185）：IM 和 cron 的长回复拆成多条时，不再从代码块中间切开；先编辑进进度 banner 的答复也会先拆分，不再因超长被平台拒绝
- **`trusted_proxy: true` 下直连访问会说明原因**（#3140）：直连或局域网访问被拒时，登录接口返回明确原因，不再显示 "invalid token"；`/dashboard` 返回 400 加说明，不再是误导性的 429
- **cli-debug 日志的保留规则**（#3146）：shim 仍存活的会话，其调试日志不会被按年龄清理；文件名不是 key hash 的文件一律不清理
- **auto-titler 重启后不再重新起标题**（#3176）：以前重启后所有自动起过标题的会话都会被重新起一遍

### Fixed

- **cost 记账**（#3097、#3126、#3220、#3235、#3265）
  - `--resume` 后的成本基线从恢复出来的 cost-state 开始算，不再把整段历史花费记到第一个 turn 上
  - CLI 自己发起的 turn（后台任务通知、workflow）在它的 result 处结算并记账
  - 进程结束时，从主 transcript、subagent 和 workflow transcript 补记还没报告的花费
  - 中断 turn 的 partial 条目按 CLI 实际观测到的各模型单价估算金额，按 message 去重；这类金额不再计入 run 记录的 `CostUSD`
- **会话生命周期中的竞态**
  - resume spawn 期间执行的 Reset / Remove 不会再被撤销（#3231）
  - spawn 窗口内写入的标签、tuning 和会话链不会被覆盖（#3214）
  - 旧 owner 不会拿到新 owner 的队列（#3213）
  - Reset 崩溃的会话时，直接让已死的 shim 退出，不再干等它超时（#3275、#3287）
  - shim 的握手会遵守 ctx 取消（#3190）
- **scratch（aside）会话**（#3248）：sweeper 不再回收还在跑 turn 的 aside；空闲时间从最后一个 CLI 事件起算
- **会话接管**（#3272、#3289）：从终端接管 Claude 会话时一律走 claude backend；resume 被拒时退为新会话，并关联被接管的 transcript
- **历史恢复与分页**
  - 各个注入点都先读 naozhi event log，再读 Claude JSONL；shim 重连只在历史为空时注入（#3205、#3211）
  - 「加载更早」和会话导出信任服务端的 `X-Events-Has-More`，并按 uuid 去重同一毫秒的边界条目（#3133、#3149、#3168、#3177、#3241）
  - 推送大批事件时按每帧最多 50 条全部送达，丢掉的帧会重试（#3131、#3142）
  - 远端会话的「加载更早」和导出在 node 上分页，加载失败时显示重试，不再是空白面板（#3171、#3200、#3245）
- **cron**
  - naozhi 停机期间完成的 run 会按结果收养（#3246）
  - 收养来的 run 像本地 run 一样释放 CLI（#3222、#3237）
  - 失败的 run 也记录它的 session id（#3273）
  - sandbox run 在结束后 panic，不会再被结束两次（#3217）
  - sandbox blob 去重与 GC 并发时不会误删（#3225）
  - `/cron add` 和 dashboard 创建时，会写明是配额满了还是间隔太短（#3132）
- **配置迁移**（#3164、#3212）：迁移会保留所有注释；`system_prompt` 写成 null 占位时，迁移不再导致 Load 失败
- **dashboard**：后到的 cron 列表响应不再覆盖新 run 的状态（#3219）；打开的 cron 抽屉里运行计时会每秒更新（#3230）；屏幕阅读器不再逐字朗读流式输出，turn 结束时只播报一次（#3173）
- **多节点**：在反向连接的 node 上 `/new` 或 `/clear` 之后，已打开的标签页不会再收不到新对话的事件（#3334）；新 primary 不再把 v0.1.41 HTTP node 的 304 当真（#3333）
- **discord**：`Stop` 加了超时，gateway 迟迟不回 hello 时不会再拖住关停（#3336）
- **doctor**（#3240）：`/health` 里的配置指纹格式不对时只给警告，不再 panic；CLI Backends 一节显示的是配置的路径，而不是 `$PATH` 上找到的那个

### Security

- **release 签名绑定 tag**（#3215、#3134）：签名覆盖 `naozhi-release-v1\ntag <tag>\n` 加 `checksums.txt` 的内容，旧版本的签名产物不能挂到新 tag 下冒充。内置了信任密钥时，`Download` 在 chmod 之前验证 `checksums.txt.sig`，任何失败都直接终止。当前内置密钥为空，对现有 release 没有影响
- **防止通过 "latest" 降级**（#3223）：见升级须知
- **远端调度的 access profile 闸门覆盖 agent 和 cron key**（#3247、#3292）：钉了 profile 的 agent 或 cron key 不会再被派到远端 node，用对方的凭证运行
- **shim 不再给身份不符的 PID 发 SIGUSR2**（#3135）：Reconnect 遇到二进制路径不符的 shim PID 时不再发信号；PID ≤ 0 时也不再调用 kill（以前会把信号发给整个进程组）

## [0.1.43] - 2026-10-05

### 升级须知

- **默认 backend 发生回落时，`naozhi config check` 会报出来，退出码随之变化**（#3381、#3389）
  - 以下三种配置在启动时会改用另一个 backend：`cli.backend` 没列在 `cli.backends` 里；`cli.backend` 不是已注册的 backend id；没设 `cli.backend`，而 `cli.backends` 第一项的 id 没注册。现在 `Validate` 会对 `cli.backend` 报一条 warn（`… startup falls back to "<id>"`，hint 里列出可用的 id），启动时打 Warn，`config check` 退出 1（以前退出 0）
  - 没有任何启用的条目能对应到已注册的 backend 时，`config check` 报 Fatal `no usable cli backend configured` 并退出 2（以前退出 1），摘要行改为 `FATAL — naozhi would refuse to start`。服务端本来就拒绝这种配置
  - `config check` 现在输出 `Validate` 的全部结论，layer 为 `config-validate`，action 为 `warn` / `error`。未知的 `cli.backends[...]` 条目以前报为 `caps` / `ignored`，现在只报一次，归为 `config-validate` / `error`，文案也变了。按旧输出写的脚本需要改
  - 发生回落时，以下几处都改用实际绑定的 backend：router 的默认 backend、dashboard 回复里的默认 backend 标签、启动日志 `naozhi starting` 的 `backend` 字段。以前标签可能写着 `cc`，实际跑的却是 kiro；没设 `cli.backend` 时，这个日志字段以前是空串，现在是例如 `claude`。启动提示里错写的 `cli.default` 改为 `cli.backend`
  - 消除回落的办法：把 `cli.backend` 显式写成 `cli.backends` 里列出的、已注册的 id
- **`access_profiles[].default_backend` 开始生效**（#3364）：这个字段以前能通过校验，dashboard 上也能看到和编辑，但从不参与选 backend。现在 key 上还没有会话时，新会话按以下顺序选 backend：显式请求、项目 `backend`、dashboard 的 backend 选择 > `agents[].backend` > profile 的 `default_backend` > `cli.backend`
  - 生效的 profile 包括 `default_access_profile`。所以**设了 `default_access_profile`、且它的 profile 写了 `default_backend` 的部署，升级后所有新会话都会落到那个 backend 上**
  - 已有会话保持记录里的 backend。项目只钉了 `access_profile`、没钉 `backend` 的 planner，也改用该 profile 的 backend。从终端接管仍固定走 claude
  - `naozhi config check --effective` 按同一规则，只把 agent 列在它实际会落到的 backend 下
  - 已知限制：启用了多个 backend 时，dashboard 新建会话会显式带上 router 默认 backend，profile 的 `default_backend` 在这个入口不生效（#3418，下个版本修）
- **planner 的启动参数只来自项目**（#3342、#3357）：在绑定了项目的 IM 会话里拉起的 planner，以前会继承 `agents.general` 的 `model` / `args` / `system_prompt` / `effort`。现在不再继承，和 dashboard 发消息、resume、重启时拿到的参数一致
  - 要给 planner 指定模型或 prompt，在项目的 `.naozhi/project.yaml` 里设 `planner_model` / `planner_prompt`，或设 `projects.planner_defaults`
  - 已在运行的 planner 不受影响。dashboard resume 一个项目已删除的 planner key 时，也不再借用 `agents.general` 的配置
- **一次性清理 `.naozhi/project.yaml` 桩文件时，保留 git 已跟踪的文件**（#3399）：这一条修正 0.1.42 升级须知里「commit 过的桩文件会在 git 里显示为一次删除」的说法
  - 清理前先跑 `git ls-files` 确认，被跟踪的桩文件保留，并打一条 Info
  - 项目目录及其上级目录里都没有 `.git` 时不调用 git，照旧删除。在 git checkout 里、但 git 给不出结论（没装 git、仓库被拒绝访问、超时、git 报错退出）时保留
  - 调用 git 时不继承 naozhi 环境里的 `GIT_*` 变量。macOS 上没装 Command Line Tools 的主机，`/usr/bin/git` 可能弹出安装提示
  - **只对从 v0.1.41 及更早版本升级的主机有用**：每个 root 只清理一次，已经运行过 v0.1.42 的 root 不会再清理。那次被删掉的已跟踪桩文件，需要在对应仓库里 `git checkout -- .naozhi/project.yaml` 自行恢复
- **cron 自动暂停不再计入不是 job 自身原因的失败**（#3345）：以下 run 仍记为 `failed`，照常发失败通知，但连续失败计数既不增加也不清零
  - `sandbox_transport`：云沙箱连接中断，包括 naozhi 重启后由启动收尾结掉的孤儿 sandbox run
  - `turn_failed`，且原因是 `backend_overloaded` / `backend_rate_limited` / `backend_unreachable`。一次持续二三十分钟的 Bedrock 或网络故障，不会再把每 5 分钟跑一次的 job 全部暂停
  - 额度用尽、认证失败、max turns、上下文超限、执行超时，以及 `timed_out/sandbox_transport`，仍然计入
  - 已知副作用：job 自己的负载每次都把 microVM 弄崩（OOM、崩溃）时，它不会再被自动暂停，每个 tick 都会发一条失败通知
- **`naozhi cost reconcile` 判定得更严，也能归属更多条目**（#3383、#3390、#3400、#3356）
  - 判定恢复额重复计费的条件：只有当一个条目超出它自己那一轮 transcript 用量的部分达到恢复额的一半时，才会标记。以前只要条目金额 ≥ 恢复额的 98% 就会标记，所以很多基线正确、只是本身比较大的 turn 被误标成了 `-r`
  - 一天里 transcript 没有可计的用量、账本上却有花费时，这一天保持原样，不再清零。对账表多一行提示 `N 天 transcript 无用量而账本有，残差未记`
  - 无法判定的条目不标记，对账表多一行提示 `N 条无法判定是否计入恢复额（无单价或回填 run 无结束时间），未标记`
  - 一个 key 先后有过多个 CLI 会话、又找不到 run 记录的 turn 条目，现在按 transcript 时间归属：只有一个候选会话在该轮时间窗内有消息时才归属，否则照旧留着不处理
  - CLI 自己发起的 turn（后台任务通知等）在 ledger 里的 `run_id` 改为 `unowned:<sid>:<id>`，reconcile 可以直接定位到它的会话
  - **已经在 v0.1.42 上跑过 `-write` 的账本，当时写入的误判条目不会被撤销**：误标的恢复额落在不能对账的日期（被留着不处理、有其他来源的花费、无单价）时，或者低于残差门槛时，当时没有被抵消；被清零的空日也不会恢复。0.1.43 只是不再写新的误判条目。还没跑过 `-write` 的，建议升级后再跑。以前因归属不明而留着不处理的日期，升级后重跑 dry-run 可能会出现新的修正条目。写入后仍需重启 naozhi
- **本地 cron run 的 ledger 行按实际运行的 backend 记账**（#3387）：没设 job `backend` 的 job，以前一律记在 `claude` 名下。现在记在会话实际跑的 backend 上（agent 默认 backend 或 router 默认 backend，例如 kiro），所以 `naozhi cost` 和 dashboard 按 backend 分组时，这部分花费会换组。已写入的历史行不变；`naozhi cost backfill` 补记的行仍记为 `claude`
- **weixin 的 bot token 过期（iLink `-14`）后，轮询暂停 1 小时**（#3360）：getUpdates 返回 `ret` 或 `errcode` 为 `-14` 时，平台状态直接置为 `failed`，`last_error` 以 `weixin token expired (iLink -14): run 'naozhi setup weixin' and restart` 开头，并打一条 Error。之后每小时才重试一次，以前是 2s / 30s 重试不停
  - doctor 会立即报 fail，不再先报 5 分钟 `disconnected`
  - 如果中转服务把 `-14` 用于临时状况，入站消息最多会延迟 1 小时
  - token 恢复后，下一次轮询成功即回到 `connected`
  - sendmessage 返回的 `-14` 照旧处理
- **discord 网关掉线后用 REST 探测 bot token**（#3367）：掉线期间按 5s 起、最长 5m 的退避调用 `GET /users/@me`
  - 返回 401/403 时立即置为 `failed`，`last_error` 为 `discord rejected the bot token (HTTP 401): update platforms.discord.bot_token and restart`，doctor 不再等 5 分钟宽限
  - 返回其他错误时，`last_error` 里只记 HTTP 状态码
  - 4013/4014（intent 不允许）这类网关关闭码仍然探测不到
  - 每次掉线会多出几次 REST 请求
- **日志变化**：按旧文案写的告警规则可能需要调整
  - IM reaction 第一次添加失败时，每个平台打一条 Info `ack reaction failed; later failures on this platform log at debug`，之后同一平台的失败只记 Debug（#3340）。缺 `reactions:write` 权限的 Slack 会看到这一条
  - `retiring dead-CLI shim before respawn` 改为 `retiring dead-CLI shim`；由 reset 触发回收时，另打一条 Info `reset: retired dead-CLI shim`（#3392）
  - `naozhi starting` 的 `backend` 字段见上文第一条（#3389）

### Added

- **会话 header 显示该会话发布的 PR**（#3358、#3347）：git chip 旁新增 PR chip，显示最新 3 个 PR 的链接，最新一个附带状态（已合并 / 已创建…），更早的折叠成 `+N`，tooltip 里有 repo#n、状态、分支和 URL。只接受 http(s) 链接
  - 数据来自 Claude Code 的 `code_change_published` 事件，持久化在 `sessions.json` 新增的 `code_changes` 字段。respawn 和 rename 会带过去，`/new` 不带。降级到旧版本后，这个字段会在下一次保存时丢失，不影响其他数据
  - 时间线不再显示光秃秃的 `⚙ vcs_state_changed` / `⚙ code_change_published` 行。已落盘的旧行不回溯清理

### Changed

- **IM 回复不再先等 ⏳ reaction 加上**（#3340）：立即执行的消息（owner 的首条消息、passthrough 消息、`/urgent`）以前要等一次 `AddReaction` 往返（最多 3s）才开始 turn。现在 reaction 异步添加，移除 ⏳ 会等添加完成之后再做。reaction 没加上时，「💭 思考中...」banner 照常补发。回复比 reaction 接口快时，答复可能先于 ⏳ 出现
- **会话被删除时，排队中的消息会收到通知**（#3343）：dashboard 删除、scratch 过期、planner 移除、upstream RPC 删掉一个 key 时，排在它后面的消息以前会被静默丢弃。现在 IM 每个 chat 回一条「会话已结束，这条消息未被处理，请重新发送。」（单次 reply token 的平台不发）；dashboard 里等待中的气泡会收到发送失败，不再一直挂着。`/new` 的行为不变
- **reverse node 排不了队时显示「忙」**（#3354）：node 关闭了队列、会话又在运行时，主节点以前报「发送失败」并广播给所有订阅者，现在和 HTTP node 一样，弹出「忙」提示并撤回乐观气泡。node 关停中仍按错误处理
- **cron 失败通知的措辞**（#3351、#3362）
  - 自动暂停通知发到的不是创建任务的会话时（per-job `notify_platform` / `notify_chat_id` 或 `notify_default`），改为提示「在创建该任务的会话发送 /cron resume <id>，或在控制台恢复」，因为 `/cron resume` 只在创建会话里找得到任务。没有来源会话的任务只提示在控制台恢复
  - 保留上下文的任务因上下文超限失败时，通知会说明之后每次执行都会因此失败，并提示「可在控制台编辑任务勾选“每次全新上下文”」。IM 创建的任务另加「或删除后不带 --keep-context 重新创建」
- **配置类型错误不再报成 YAML 语法错误**（#3388）：例如字符串写进 int 字段、key 重复，以前统一报 `parse config: yaml syntax error`，现在报 `parse config: yaml type error: line 2: cannot unmarshal !!str into int (see naozhi debug logs for details)`，或 `line N: duplicate key (first defined at line M)`。最多列 5 条，多的写成 `(+K more)`。报错里不回显配置的值和 key 名。真正的语法错误照旧报 `yaml syntax error`。启动、`config check`、`doctor`、`shim`、`models`、`cost` 都会显示新文案
- **cost ledger 的模型明细**（#3341）：中断 turn 的 partial 条目按规范化后的模型名合并，`claude-opus-5-5` 和 `claude-opus-5-5[1m]` 不再拆成两行；没有工作目录的会话，其 partial 的 workspace 标签和 turn 行一样是空串，不再是 `"."`。一行超过 16 个模型时，多出的折叠成末尾一行 `model="other"`，不再截断，所以按模型加总等于条目金额
- **进程结束时读取 usage 更快**（#3403）：transcript 超过 8 MiB 时，用二分查找定位到时间窗附近再读，不再从头扫到尾。实测 69 MiB 的 transcript 从 189ms 降到 3.4ms，结果和全量读取一致

### Fixed

- **被放弃的 Send 晚到的中断结果不会被下一轮当成回复**（#3429）：Send 被中断又放弃后（典型是 cron 到截止时间而 CLI 中断较慢），它晚到的 `error_during_execution` 结果以前可能被下一次 Send 当成自己的答案，用户看到空回复、真实回答只被记账。现在这类结果绑定到发起中断的那一轮，只记账不投递
- **cost 记账漏记**（#3365、#3377、#3385）
  - Send 已经放弃（被中断、cron 超时）之后才到的 result 会补记；passthrough 中被取消或被判为孤儿的槽位，它们的 result 也会补记。以前这些花费随 `/new` 或进程回收永久丢失
  - respawn 或 rename 之后，记到旧会话上的花费（例如 stuck_running 被杀后异步补记的 partial）会转给接替它的会话，会话花费卡片和 `sessions.json` 的 `CostSpent` 不再漏掉；rename 之后旧会话上的读数不再在新 key 上被重复计费
- **中断与回复错位**（#3339、#3363、#3353）
  - claude 非 passthrough 模式（collect 模式、cron）下 `/stop` 之后，turn 以前要卡到 15 分钟无输出超时才结束，并把中断误报为成功。现在立即返回中断结果，下一条消息也不再多等 500ms
  - passthrough 模式下 `/stop`、SIGINT 或中途出错之后，排队的消息不再被错误地回「/urgent 已中断」：CLI 本来就会继续执行它们，现在它们各自拿到自己那一轮的答复。只有 `priority:"now"` 抢占才会丢弃排队消息
  - CLI 在 turn 中途重连后，legacy Send 可能把上一轮的 result 当成本轮答复，之后每条回复都错位一轮。现在会忽略 turn 开始之前收到的事件
- **删除 / 重置会话时的竞态**（#3359、#3372）：同一个 key 的会话在旧会话关闭期间被重新创建时，旧会话的收尾不会再删掉新会话的排队条目，也不会造成两个并发 owner；新会话已经写盘的 event log 和 attachment 引用也不再被删除
- **启动失败的展示与暂停**（#3344、#3350、#3391）
  - Init 握手失败后，dashboard 已打开的标签页会立即显示启动失败 chip 和暂停到的时间，并带上分类（认证、MCP 配置、缺少运行时），不再显示 `unknown`
  - `/cd` 会解除该 chat 下所有 key 的启动失败暂停，包括还没有会话条目的 key
  - 通过 `/bin/sh` 包装启动、缺少 `node` 时的报错（dash 的 `sh: 1: node: not found`、busybox 的 127 退出码）归为「缺少运行时」，保留 `--resume`，修好运行时之后可以接着原会话
- **从终端接管会话**（#3369、#3384）：路由此刻会拒绝的接管（进程数已满、同一个 key 正在 spawn、服务正在关停）改为在发 SIGTERM 之前就拒绝，返回 503/409，外部 CLI 不受影响。以前会先杀掉外部 CLI，再让 dashboard 等 10 秒超时
  - SIGTERM 之后才发生的失败，dashboard 会通过新接口 `GET /api/discovered/takeover/status` 立即得知，并显示原因，例如「接管失败：进程数已满，外部 CLI 已终止；请关闭一个空闲会话后从历史记录重新打开该会话（对话记录仍在）」
  - reverse node 上的接管也会先做 cwd 校验和容量预检，被拒时外部 CLI 同样不受影响（主节点只报通用的 `502 upstream error`）
- **dashboard**
  - 在 `session_state` 推送之前发出的 `/api/sessions` 轮询，后到时不再覆盖推送的状态。以前会出现 turn 进行中运行横幅消失、提前播报「回复完成」，或者 turn 结束后横幅又回来（#3348）
  - v0.1.41 及更早的 reverse node 不支持分页。对这类 node，「加载更早」到头时提示「已到该节点内存中最早的事件 — 升级该节点可加载更早历史」，导出改为警告提示，不再声称已完整导出（#3378）

### Security

- **scratch（aside）会话只能在本机运行**（#3376）：发往远端 node 的 `scratch:` key 一律拒绝（`ErrLocalOnlySession`）。scratch 继承源会话的 access profile，而这个信息只在本机的 scratch 池里，远端闸门以前看不到，会把它当成允许。dashboard 不会发这种请求，只有手工构造的已认证请求才会触发

## [0.1.44] - 2026-10-07

### 升级须知

- **没配 `im_access` 的部署，`naozhi config check` 从退出 0 变为退出 1**（#3442、#3473）：新增的 IM 发送者白名单 `im_access` 不配置时行为不变（全放行）。但每个已启用、又没有条目的平台，启动时都会打一条 WARN `IM 入口无鉴权：任何能私聊 bot 的用户都可在宿主机执行命令 (no sender allowlist)`，`config check` 也会报同一条 warn，所以退出码变为 1。按退出码 0 判定的部署脚本需要调整，或者直接把名单配上
  - `naozhi doctor` 新增 `im access` 一行。未受限的平台报 warn，不影响 doctor 的退出码
  - `im_access` 条目里出现未知平台名、空 ID 或没展开的 `${VAR}`，配置会加载失败
- **SIGHUP 现在是重载配置，不再结束进程**（#3554）：以前 naozhi 没有注册 SIGHUP，收到后按默认动作退出。现在 SIGHUP 和新命令 `naozhi config reload`、`systemctl reload naozhi` 走同一段重载代码。要停进程请用 SIGTERM / SIGINT。原来靠 HUP 让 supervisor 拉起新进程的脚本，现在只会触发一次重载
  - 能热重载的只有 `im_access`、`im_rate_limit`、`log.level` 和 `cost.budget` 的上限。其他段的改动会列在 `restart_required` 里，直到重启为止。`reverse_nodes`、`agents` / `agent_commands`、`access_profiles`、`cron.notify_default` 按设计只在重启时生效
  - `naozhi config reload` 的退出码：0 已生效；3 有段需要重启；4 某个原本有名单的平台变成对所有人开放（多半是 `im_access` 键名拼错，优先于 3）；1 服务端拒绝或连不上；2 参数错误。新文件校验失败时，运行中的进程不受任何影响
  - `naozhi install` 生成的 systemd unit 现在带 `ExecReload=/bin/kill -HUP $MAINPID`。**旧版本装出来的 unit 没有这一行**，`systemctl reload naozhi` 会报不支持。重跑 `naozhi install`，或者手动加上这一行再 `daemon-reload`
  - `/health.config_sha256` 只在重载后没有待重启段时才前进。有待重启段时，指纹停在上一份完整生效的文件上，新字段 `config_restart_required` 列出这些段。doctor 的 `config-drift` 相应报 `restart required for: ...`。单纯的指纹不一致，文案从 `restart required: ...` 改为 `not applied: ... naozhi config reload applies it`。另有新字段 `config_reloaded_sha256` 记录最后一次加载或重载读到的文件；有待重启段之后磁盘又改过、但还没重载时，doctor 会同时报 `not applied` 和 `restart required for: ...`（#3665）
- **Slack / 飞书群聊默认按话题分开会话，回复留在原话题里**（#3446、#3569、#3583）：新配置 `session.group_scope` 的默认值是 `thread`
  - Slack 话题串和飞书话题里的提问，各用一个独立会话，不再和频道顶层共享上下文。在 bot 的顶层回答下面开话题追问，也会开一个不带频道上下文的新会话
  - 不在话题里的消息仍用原来的频道会话，已有会话不受影响
  - 话题里的 `/new`、`/stop`、`/urgent` 只作用于该话题的会话。`/cd`、`/project`、`/cron` 仍对整个群生效
  - 回复（含进度 banner、分段、错误提示和卡片）以前一律发到频道或群的顶层，现在发回原话题。飞书只对带 `thread_id` 的话题消息这样处理
  - 要恢复整群共用一个会话，设 `session.group_scope: chat`，但回复仍会进话题。`user` 让每个成员各用一个会话
  - Discord 和微信不受影响
  - 写了不合法的 `group_scope` 时配置加载失败
- **IM 平台侧的设置**（#3451、#3445）
  - Slack 要收文件，Bot Token 需要 `files:read` scope。缺这个 scope 时，用户会收到文件下载失败的提示
  - Slack 的 AskUserQuestion 按钮需要在 app 设置里开启 Interactivity & Shortcuts（Socket Mode 不用填 Request URL）。没开启时点按钮没有反应，卡片上会提示可以直接回复文字
  - Discord 的按钮要求开发者后台的 Interactions Endpoint URL 留空，否则点击不经 Gateway 送达
- **dashboard 的 backend picker 默认改为「自动」**（#3477、#3500、#3586）：以前 picker 总是把 router 默认 backend 当作显式选择发出，所以 `access_profiles[].default_backend` 和 `agents[].backend` 在 dashboard 入口从不生效。现在不动 picker 就不发 `backend`，由服务端决定
  - 保存项目设置时，不再把项目的 `backend` 钉成 router 默认。编辑没设 backend 的 cron 任务，保存时也不再 PATCH 进 router 默认
  - **不做迁移**：以前因为保存操作被钉住的项目和 cron 任务保持原值。要恢复跟随，在项目设置或 cron 编辑里把 backend 选回「自动」并保存
- **cron 自动暂停规则调整**（#3422、#3457、#3470、#3593、#3599）
  - 运行中丢失云沙箱连接（`failed/sandbox_transport`）重新计入连续失败，这一条撤回了 0.1.43 的豁免。只有 naozhi 重启后由启动收尾结掉的孤儿 sandbox run 仍然不计。有副作用的 job 最多会重复执行 `auto_pause_after_failures` 次
  - 后端瞬时故障（过载、限流、连不上）仍然不计入连续失败，但改为单独计数。次数达到阈值、且距第一次已满 6 小时，job 会自动暂停，`paused_reason` 为新值 `auto_transient`。IM 里 `/cron list` 标为 `[自动暂停：后端持续故障]`
  - 每个 job 新增两个落盘字段 `transient_failures` 和 `transient_failing_since`。降级到旧版本时，这类 job 显示为手动暂停
- **cron 失败通知和错误类别的变化**（#3587、#3499、#3495）
  - CLI 因认证失败、MCP 配置无效或缺少运行环境而退出时，通知分别写明原因，错误类别从 `send_error` 变为 `turn_failed`
  - 碰上失效的 `--resume` 时，通知改为「执行失败（上次会话无法恢复），下次执行将尝试开启新会话」
  - 上下文超限的通知和 `/cron add` 的回复，改为建议用 `/cron mode <id> fresh|keep`，不再建议删掉重建。按旧文案 `不带 --keep-context 重新创建` 写的告警需要改
- **`naozhi setup weixin` 会写入 `im_access`**（#3555）：扫码确认的微信用户会写进 `im_access.platforms.weixin.allowed_users`。已有配置原先没有 weixin 条目的，重新扫码后（例如 token 过期时），微信入口会从「所有人可用」变为只放行扫码用户。登录响应里没有可用 ID 时，新建的配置文件写 `default_deny: true`，已有的配置文件只打印需要补的配置。已有的 weixin 条目不会被改动
- **多节点：reverse node 只对声明了 `send-status` 能力的 primary 回答「忙」**（#3496）：在 v0.1.42 / v0.1.43 的 primary 后面，新 node 碰到会话忙、又关了队列时，退回发送失败（「发送失败：会话正忙，消息未送达，请稍后重试」）。升级顺序仍是先 primary、后 node
- **正在跑 Claude Code Workflow 的会话不会被闲置回收**（#3600）：workflow 还在运行、且 6 小时内收到过它的进度时，`Cleanup` 的 idle TTL 和闲置进程释放都会跳过这个会话，这期间它一直占着一个 `max_procs` 名额。`sessions.json` 新增 `workflows` 字段，降级后这个字段会在下次保存时丢失，不影响其他数据
- **`naozhi cost reconcile` 不再下调账本**（#3546）：按天残差现在只往上补。账本高于 transcript 的日子只在报告里列出（「账本高于 transcript 共 X，未下调」），因为 CLI 计费的请求并不都写进 transcript
  - **在 v0.1.42 / v0.1.43 上用 `-write` 写入的负残差修正不会被撤销**：这些条目已经把当天账本拉到了 transcript 用量，重跑时这一天看起来已经对平
  - 已删除 dashboard 会话的首轮条目现在能归属（#3510、#3494），轮次截止点也统一了（#3512），所以重跑 dry-run 可能多出几条修正。写入后仍要重启 naozhi
- **日志与提示文案的变化**：按旧文案写的告警规则可能需要调整
  - 每执行一个 turn 多一条 Info `turn: start`。带 ctx 的日志多出 `trace_id`、`run_id`、`session_key` 三个字段（#3559、#3613），用法见 `docs/ops/log-correlation.md`
  - weixin token 过期时的 `last_error` 改为 `… and restart, unless it reconnects by itself`。discord token 被拒时改为 `update platforms.discord.bot_token; restart unless it reconnects by itself`。doctor 对 `failed` 状态的说明也改为「needs operator action」（#3604、#3454）
  - `/urgent` 的用法提示不再承诺立即中断：工具正在运行时，要等工具返回（#3570）
  - 回复超长的 CLI 输出行时，shim 打 `CLI stdout line over cap; skipped`（#3601）。结果在调用方放弃之后才到时，打 `cli: abandoned run's result booked as unowned`（#3613）

### Added

- **IM 渠道接收文件**（#3571、#3581、#3588、#3597、#3565）：飞书、Slack、Discord 发来的 PDF 和 UTF-8 文本（`.txt` `.md` `.csv` `.json` `.log` `.yaml` `.yml`）会写进会话工作目录的 `.naozhi/attachments/<日期>/`，由 Claude 用 Read 工具读取。kiro / codex 会话收到同样的读取提示
  - 上限：单个文件 32 MiB；每条消息最多 5 个文件、合计 32 MiB
  - 不支持的类型、超限的文件和下载失败的文件，bot 会回一条「以下文件未处理：」逐个列出。Slack 带上传的消息（`file_share`）以前连同说明文字整条被丢弃，现在能收到。Discord 超过 10 MiB 的图片以前被截断后照样发出，现在会被拒并告知
  - 群聊里文件要和 @bot 在同一条消息里。飞书的文件消息没法 @bot，所以请私聊发送
  - 这些文件和 dashboard 上传的文件一样，只由 `attachment-gc` 回收（默认关闭）
- **`session.thread_auto_open`**（#3598）：默认关闭。开启后，Slack 频道或飞书群里不在话题中的 @bot 提问，会在以这条提问为根新开的话题里回答，之后在话题里追问接着同一个会话。飞书拒绝开话题（例如缺少权限）时回答改发到群里，这样的群请保持关闭
- **Slack / Discord 的 AskUserQuestion 渲染成可点击的按钮**（#3547、#3558）：单个问题的每个选项是一个按钮，点击即提交，卡片改为「✅ 已回答」。多个问题、或超出平台限制时，回退为纯文本列表
- **IM 斜杠命令 `/model`、`/effort`、`/backend`**（#3526）：`/model [名称|reset] [agent]` 和 `/effort [档位|reset] [agent]` 切换正在运行的会话，回复里写明立即生效、从下一条消息起生效还是会话启动时生效。`/backend [id|reset] [agent]` 决定下一个会话用哪个 backend，`/new` 之后生效。这三个命令不要求 admin，只能在运维配置过的值里选
- **cron：`/cron mode <id> fresh|keep`**（#3495）：在聊天里切换任务的上下文模式，不用再删掉重建（重建会丢 ID 和执行历史）。只在创建该任务的会话里生效，切换时清零连续失败计数，不会自动恢复已暂停的任务
- **`im_rate_limit`：按发送者限制 IM 消息频率**（#3528）：`msgs_per_min` / `burst` 是按平台加用户 ID 分桶的令牌桶。斜杠命令也计数，`/stop` 不计。超限的消息直接丢弃，发送者每分钟最多收到一次「消息过于频繁」。丢弃次数记在 `naozhi_dispatch_rate_limited_total`。管理员同样受限。负数、或只写 `burst` 不写 `msgs_per_min`，配置加载失败
- **`cost.budget`：每日花费上限**（#3542、#3563、#3582、#3610）：`per_chat_daily_usd`、`per_cron_job_daily_usd`、`daily_usd`（整机），另有 `warn_ratio`（默认 0.8）、`action`（`block` / `warn`）和 `timezone`（默认同 `cron.timezone`）
  - IM：额度用尽后新消息不进 CLI，会话每分钟最多收到一次「今日费用预算已用尽（$X / $Y），MM-DD 00:00 重置」。斜杠命令照常可用。绑定同一项目的群共用一份额度
  - cron：额度用尽后，到点的运行和手动触发记为 `skipped / budget_exceeded`，不计入连续失败。每个任务每天只有第一次跳过写进历史并发通知。dashboard 的沙箱「重放」返回 409
  - dashboard 自己的 turn 不受限，但花费计入整机额度。会话头部和 cron 时间轴显示「今日 $X / $Y」，数据来自新接口 `GET /api/cost/budget`
  - 这是软上限，只统计以 USD 计价的花费。上限设成负数、`warn_ratio` 不在 [0,1] 内、`action` 或时区写错、只写了设置却没设任何上限、或者 `cost.enabled: false` 时设了上限，配置都会加载失败。上限可以热重载；启动时没配任何上限，或者改了时区，仍然要重启
- **Prometheus 指标 `GET /metrics`**（#3590、#3617）：设 `server.metrics_enabled: true` 打开，默认关闭。以 Prometheus 文本格式导出全部 `naozhi_*` 计数器，带有命名 label，cron 执行耗时导出为 histogram
  - 需要配置 dashboard token（没配时返回 403），请求要带 Bearer token，**不限 loopback**，所以持有这个 token 就等于能访问 dashboard
  - doctor 新增 `metrics` 一项，未启用（404）也算 pass。详见 `docs/ops/metrics.md`
- **出站 webhook `integrations.webhooks`**（#3594）：把 cron / sysession 的 `run.started` / `run.ended` POST 到配置的端点，可按 `events` / `subsystems` 过滤，填了 `secret` 会附带 `X-Naozhi-Signature: sha256=<HMAC>`。408 / 429 / 5xx 重试 3 次，队列满时丢弃并计数，不会阻塞调度器。payload 只含 run 元数据，不含 prompt 和结果文本。URL 不合法时加载失败；对非 loopback 主机用明文 `http://` 会报 warn。重试按 1s → 2s → 4s 退避（加 0–25% 抖动，最长 30s），关停时会中断等待；日志只记 `scheme://host` 和 URL 的短哈希，不会写出 URL 里的 token 或 secret（#3664）
- **Dashboard 展示 Claude Code Workflow（ultracode）的实时进度**（#3475，设计见 `docs/rfc/workflow-dashboard.md`）：session 用 Workflow 工具跑多 agent 编排时，会话顶部会出现 workflow 面板
  - 标题区显示名称、当前阶段、done/running/queued/failed 计数、tokens、工具调用数和耗时。下面按 phase 分组，每组带进度条；每个 agent 占一行，显示模型、tokens、工具数、最后动作和重试次数
  - 完成后显示结果和 `log()` 日志。点 agent 行可进入该 agent 的 transcript；侧栏卡片上带 workflow 徽标
  - workflow 在后台跑，父会话空闲时也照样更新；naozhi 重启、shim 重连后状态会恢复；同一会话里多个 workflow 并发也支持
  - workflow 的进度帧不再写入 event log。以前一次大 workflow 会写入上万条只有一行的 task_progress；已有日志随正常轮转淘汰，不做迁移
  - 顺带修复：后台任务帧不再让重连误判为 turn 进行中；agent tailer 改以 `~/.claude/projects` 为根目录，以前会拒绝所有子 agent 的 transcript；hook / control_response 快路径改为按行首匹配，正文里含这些字样的帧不再被误丢
  - 暂不支持从 dashboard 停止或调整 workflow，因为 stream-json 没有这个控制通道
- **turn 的 `run_id` 贯穿日志、run 记录、cost ledger 和 transcript**（#3559、#3613、#3623）：turn 的 user / result 条目带上 `run_id`，可以和 `/api/sessions/runs`、cost ledger 按 run id 关联。cron 和 sysession 的 turn 也有了
- **启动日志点名 `default_backend` 生效的 access profile**（#3491）：profile 的 `default_backend` 和启动时绑定的默认 backend 不同时，每个这样的 profile 打一行日志，带 `default_backend`、`router_default`、`scope` 和 `hint`。该 profile 是 `default_access_profile` 时打 Warn，其他打 Info

### Changed

- **dashboard 不再访问任何外部 CDN**（#3567、#3577、#3548）：KaTeX 0.16.21 和 mermaid 11.14.0 打包进了 binary（约 +1.5 MB），CSP 的 `script-src`、`style-src`、`font-src` 只允许 `'self'`。没有外网出口的部署（例如经 PrivateLink 访问 Bedrock 的 VPC）也能渲染公式和图表。渲染器加载失败时显示源码并提示，每次页面加载最多重试一次
- **kiro 的 RPC 错误会显示真实原因**（#3538）：kiro 把原因放在 `data` 里（`message` 恒为 `Internal error`），现在会拼进错误文本。被后端拒绝的一轮，在 dashboard 时间线上显示为一条提示，以前是无声结束
- **IM 里被 claude 中断的回复会做标记**（#3553、#3568）：claude 2.1.288 起，被中断的 turn 以 `terminal_reason=aborted_*` 结束。生成到一半的文本后面加一行 `*— 已中断，以上为部分回复*`。没有文本的中断回合，把进度 banner 改为「已中断。」

### Fixed

- **发版前 review 修复**
  - `/model` / `/effort` / `/backend` 在话题或成员会话里改的是发命令那一侧的会话，不再误改群顶层会话（#3654）
  - 渲染 mermaid 图时不再弹出误报的「页面遇到异常」提示（#3663）
  - webhook 重试真正等待退避，日志不再泄露完整 URL；`Close` 之后再投递不再 panic（#3664）
  - 热重载留下待重启段后又改了配置文件，doctor 的 config-drift 能发现（#3665）
- **cost 记账**
  - cron 只计入它的 Send 存活期间记到的花费。迟到的 result、被截止时间杀掉的 partial、run 之间从 dashboard 发进去的 turn，以前两边都不记，现在记在会话上，带 job id，cron 抽屉的 30 天合计也会算上（#3461、#3474、#3596）
  - 优雅重启时，在保存会话状态之后才报告的 turn 不再记两次（#3505）
  - 被强杀或崩溃后重新接管 CLI 时，恢复以最新的 ledger 记录为基线，不再重复记账（#3533）
  - scratch 被提升改名之后，旧会话迟到的花费记在存活的 key 下（#3502）
  - 新会话第一条 run 记录带上 CLI 的 session id（#3494）
- **`/urgent` 不再丢弃排队的消息**（#3458）：实测 `priority:"now"` 抢占不会清空队列。排在前面的消息会各自拿到真实回答，不再收到「已被 /urgent 打断，请重发」
- **失效的 `--resume`**（#3485、#3499）：collect 模式和 cron 碰到 claude 的启动错误时，报「上次会话无法恢复」，不再报笼统的执行失败
- **cron 重启后接管的那次执行若被 claude 中断，记为 `canceled`**（#3561）：以前空文本或半截输出会被记成 `succeeded`
- **从终端接管**（#3464、#3484、#3503、#3509）：接管前先跑 router 的接管检查，并在 SIGTERM 之前就预留 key。同一 cwd 的第二次接管、`max_procs` 只剩一个名额时的并发接管，都会在杀掉外部 CLI 之前就被拒绝（409 / 503）。IM 自动接管和经 reverse node 的接管也一样。dashboard 只把消息发进本次接管自己报告 ready 的会话
- **删除后立刻用同一个 key 重建会话**（#3465、#3455）：被删会话的记录不再混进新会话的 event log，新会话的 run-history 也不会被清掉
- **shim**
  - CLI 某一行 stdout 超过 10 MiB 时只跳过这一行。以前 shim 从此不再读管道，CLI 卡在 write 上，两个进程都活着，会话永远挂起（#3601）
  - 迟到的旧 handler 不会再踢掉更新的 client，respawn / reset 不会再因此报 `refusing to clobber`（#3536）
  - 已经在运行的 shim 保留旧代码，直到下次 respawn
- **后台任务和 workflow**
  - 闲置会话里有后台 workflow / Agent / Bash 任务时，naozhi 重启后会话不再一直显示 `Running`（#3493）
  - `local_workflow` 任务不再走 SubagentLinker 的重试（#3482）
  - 名为 `hook_*` 的 agent，或者 tool input 里带 `"hook_` / `"control_response"` 的 assistant 帧，不会再被 stream 快速路径误丢（#3481）
- **scratch**（#3480）：在从未 spawn 过的源会话上打开的 aside，跑在源会话 resume 时会用的 backend 上
- **discord**（#3462、#3606）：bot 身份自愈和掉线探测受 Stop 的 ctx 约束，不会再在 429 或 REST 限流桶上把关停拖满 30 秒
- **dashboard**
  - 在 result 之前发出的 `/api/sessions` 轮询，后到时不会再把已结束的 turn 恢复成运行中，也不会重复播报（#3459）
  - 侧栏相对时间跳秒后重绘不再吞掉点击（#3469）
  - Agent drill-in 走上了 WS 实时推送，以前会降级成 3 秒一次的 HTTP 轮询（#3483）
  - Markdown 里的占位符不会再泄漏进属性值（#3529）
- **doctor**（#3471）：CLI Backends 段的 `Default:` 显示启动时实际绑定的 backend，发生回落时在括号里写明原因

### Security

- **IM 发送者鉴权 `im_access`**（#3463、#3473）：`default_deny`、`deny_reply`、`platforms.<p>.allowed_users`、`admin_users`。在所有入口的命令分发之前统一判定，包括卡片按钮的回答。`/cron`、`/cd`、`/project` 需要 admin；`admin_users` 为空时，所有放行的用户都是 admin。被拒的消息打 Info `im access denied`（带 user ID），计入 `naozhi_dispatch_denied_total`，私聊里同一用户 10 分钟最多收到一次自己的 ID。可以热重载
- **媒体在下载前先过 `im_access`**（#3530、#3541）：名单外的人发来的飞书语音不再下载、不再调 Transcribe 转写（不产生费用），飞书图片、Discord 附件也不再下载。被拒时不回适配器的错误文案，名单外的人无法借此确认 bot 在线
- **`naozhi setup weixin` 默认只放行扫码用户**（#3555）：见升级须知
- **dashboard CSP 不再放行第三方源**（#3567、#3577）：见 Changed
