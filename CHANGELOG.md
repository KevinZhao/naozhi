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

- **cron 自动暂停不再计入非 job 自身原因的失败**（#3328）：以下 run 仍记为 `failed`、照常发失败通知，但不再增加连续失败计数（也不清零）
  - `sandbox_transport`：云沙箱连接中断、任务状态未知，包括 naozhi 重启后由启动收尾结掉的孤儿 sandbox run。频繁升级重启的主机上，长 sandbox job 不会再因此被自动暂停
  - `turn_failed` 且原因为后端过载、限流或连不上模型服务（`backend_overloaded` / `backend_rate_limited` / `backend_unreachable`）。一次持续二三十分钟的 Bedrock 或网络故障不会再把主机上每 5 分钟一跑的 job 全部暂停
  - 额度用尽、认证失败、max turns、上下文超限和执行超时仍然计入
- **planner 的启动参数只来自项目**（#3300）：在绑定了项目的 IM 会话里拉起的 planner，不再继承 `agents.general` 的 `model` / `args` / `system_prompt` / `effort`，和 dashboard 发消息、resume、重启同一个 planner key 时拿到的参数一致（那几条路径本来就不继承）。要给 planner 指定模型或 prompt，在项目的 `.naozhi/project.yaml` 里设 `planner_model` / `planner_prompt`，或者设 `projects.planner_defaults`。已在运行的 planner 会话不受影响；dashboard resume 一个项目已删除的 planner key 时，也不再拿 `agents.general` 的配置去拉起
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

- **`access_profiles[].default_backend` 现在真正决定新会话的 backend**（#3299）：此前该字段被校验、在 dashboard 展示，却从不参与选 backend，用 profile 的会话照样跑在 agent / router 默认 backend 上。现在它排在 `agents[].backend` 之后、router 默认 backend（`cli.backend`）之前，只作用于 key 上还没有会话的新会话；请求显式 backend、项目 `backend`、dashboard 的 backend 选择、已有会话（resume 不换 CLI）和 `agents[].backend` 仍然优先。所用 profile 包括 `default_access_profile`。`naozhi config check --effective` 按同一规则把 agent 只列在它会落到的 backend 下
- **cron 自动暂停通知的恢复提示按通知去向给出**（#3328）：`/cron resume` 只认创建任务的那个会话，而暂停通知会发到 per-job `notify_platform`/`notify_chat_id` 或 `notify_default` 指定的会话；此前在那里照提示发 `/cron resume <id>` 只会得到"未找到"。通知不在创建会话时改为提示"在创建该任务的会话发送 /cron resume <id>，或在控制台恢复"（不写出创建会话的 id）；发回创建会话时措辞不变
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
