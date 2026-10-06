# Changelog

该项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 的格式。版本号按语义化版本（Semantic Versioning）管理。

真正的 per-round 变更日志曾放在 `docs/TODO.md` 顶部；该文件已于 2026-05-26
删除（待办迁至 GitHub Issues，映射见 `docs/rfc/todo-to-issues-migration.md`）。
本文件只归档对用户 / 运维可感知的大型变更。

## [Unreleased]

### Added

- **Slack：AskUserQuestion 渲染成可点击的按钮**（#3445）：单个问题的每个选项是一个按钮，点一下即提交答案并把卡片改成「✅ 已回答」；多个问题仍是只读列表，请在一条消息里一次回复全部。需要在 Slack app 设置里开启 Interactivity & Shortcuts（Socket Mode 无需 Request URL）；未开启时按钮无响应，卡片上提示可直接回复文字。超出 Block Kit 限制（25 个选项、文本过长）时回退为原来的纯文本列表
- **cron：聊天里用 `/cron mode <id> fresh|keep` 切换任务的上下文模式**（#3406）：此前 IM 里只能在创建时用 `--keep-context` 决定，要换模式只能删掉重建（丢失 ID 与执行历史）
  - 只在创建该任务的会话生效（与 `/cron del/pause/resume` 相同的前缀匹配和跨会话屏蔽）；模式词不区分大小写，`keep-context` 等同 `keep`；设成当前模式也返回成功
  - 下次执行生效，正在执行的那次不受影响；与控制台编辑一样，连续失败计数（含瞬时故障计数）清零。任务处于暂停时不会自动恢复，回复里提示 `/cron resume <id>`
  - 「对话上下文已超出模型上限」的失败通知与 `/cron add` 的创建回复改为给出 `/cron mode <id> fresh|keep`，不再建议删除后重建。按旧文案 `不带 --keep-context 重新创建` 匹配的告警请改为匹配 `/cron mode`
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
- **启动日志点名 `default_backend` 生效的 access profile**（#3419）：profile 的 `default_backend` 与启动实际绑定的默认 backend 不同时，每个这样的 profile 打一行日志，带 `default_backend`、`router_default`、`scope`、`hint`。该 profile 是 `default_access_profile` 时为 Warn（没有解析到其他 profile、也没钉 backend 的新会话都会换 CLI），其余为 Info。与默认 backend 相同的不打。已有会话不受影响，完整的落点用 `naozhi config check --effective` 查看

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
- **cron：云沙箱连接在运行中断开，重新计入自动暂停**（#3422）：撤回 0.1.43 #3345 里「云沙箱连接中断不计」的那一半，以及它写明的已知副作用
  - 运行中丢失 sandbox stream（`failed/sandbox_transport`）重新计入连续失败。job 自己的负载每次都把 microVM 弄崩（OOM、崩溃）时，连续失败达到 `cron.auto_pause_after_failures` 后会照常自动暂停。有副作用的 job 因此最多重复执行这么多次，每次在确认队列里留一条记录。naozhi 所在主机一侧的原因（休眠、网络中断）导致的运行中断开同样计入：连接断在哪一端，代码无法区分
  - 只有 naozhi 重启后由启动收尾结掉的孤儿 sandbox run 仍然不计。run 记录、错误分类和通知文案不变，仍是 `sandbox_transport`
  - 后端瞬时故障（`turn_failed` 且原因是 `backend_overloaded` / `backend_rate_limited` / `backend_unreachable`）仍然不计入连续失败，但改为单独计数，见下一条
- **cron：后端瞬时故障持续 6 小时以上也会自动暂停**（#3422）：此前过载 / 限流 / 连不上模型服务（`apierr` 的网络错误和超时都归到这里）一律不计，模型服务地址配错、凭证所在网络永久不通时，每个 job 每个 tick 都发一条失败通知，永不暂停
  - 每个 job 新增两个落盘字段 `transient_failures`（自上次成功、恢复或编辑以来的瞬时故障次数）与 `transient_failing_since`（其中第一次的结束时间）。成功、恢复、编辑都会清零；job 自身原因的失败和重启孤儿都不动它们
  - 次数达到 `cron.auto_pause_after_failures`，且距第一次已满 6 小时，这次失败就自动暂停该 job。`paused_reason` 仍是 `auto_failures`，通知照常带「已连续失败 N 次，任务已自动暂停」（N 是瞬时故障次数），`cron job auto-paused` 日志多了 `transient=true` 与 `transient_failures` 字段
  - 窗口跟执行频率无关：每 5 分钟一次的 job 要故障 6 小时才停（不会因为半小时的故障就停），每天一次的 job 仍要 5 次。阈值设为负数同样关闭这条规则；6 小时不可配置
- `/urgent` 的文案不再承诺"立即中断"（#3498）：工具正在运行时（例如阻塞的 Bash `sleep 20`），CLI 要等工具返回才结束当前回复（claude 2.1.288 实测，见 `docs/rfc/passthrough-mode-validation.md` V10）。IM 用法提示改为「用法：/urgent <紧急消息>（该消息会中断正在进行的回复；正在运行的工具需先结束）」，`/help`、dashboard 快捷键面板和 README 同步修改；按旧用法文案做匹配的脚本需要更新。`/stop` 的文案不变

### Security

- **IM 发送者鉴权 `im_access`**（#3442）：此前任何能给 bot 发消息的 IM 用户都能让 naozhi 在宿主机上执行命令。新增顶层配置 `im_access`（`default_deny` / `deny_reply` / `platforms.<p>.allowed_users` / `admin_users`），在所有入口（含飞书 AskUserQuestion 卡片回答）命令分发前统一判定；`/cron`、`/cd`、`/project` 需 admin，`admin_users` 为空时所有 allowed 用户都是 admin。被拒消息记 Info `im access denied`（带 user ID）并计入 `naozhi_dispatch_denied_total`；私聊回复一次本人 ID（同一用户 10 分钟内只回一次），群聊静默。**不配置时行为不变**（全放行），但启动日志与 `naozhi config check` 对每个未受限的已启用平台给出 WARN——`config check` 因此会从退出码 0 变为 1，按退出码 0 判定的脚本需要调整。条目里的未知平台名、空 ID、未展开的 `${VAR}` 会让配置加载失败。改名单需重启。`naozhi doctor` 新增 `im access` 一行，同样对未受限平台给 warn（不影响 doctor 退出码），受限时列出每个平台的用户数与 admin 数；README「部署 · IM 访问控制」说明威胁模型、如何从拒绝日志取 ID
- **飞书语音和图片在下载前先过 `im_access`**（#3513）：此前飞书适配器先下载语音、调 Amazon Transcribe（按时长计费）转写，或先下载图片，再交给 dispatcher 判定；名单外的人能刷转写费用，下载/转写失败时还会收到适配器的错误回复，从而确认 bot 在线。现在下载前先按 dispatcher 的同一套规则预判（群聊未 @bot → 静默丢弃；名单外 → 与文字消息相同的 Info 日志、`naozhi_dispatch_denied_total` 计数和私聊 10 分钟一次的 ID 提示），被拒的消息不下载、不转写、不回错误文案。**行为变化**：群里的飞书语音不能 @bot，此前转写完也会被丢弃，现在直接不转写——省下的只是费用，可见行为不变
- **Discord 图片附件在下载前先过 `im_access`**（#3513）：此前 Discord 适配器先从 CDN 下载最多 5 张附件图片（单张 10 MB、合计 32 MiB 封顶）再交给 dispatcher 判定，名单外的人能让 naozhi 白白消耗带宽和内存。现在带图片的消息下载前先用与飞书相同的预判，被拒的整条消息不下载、不进 dispatcher；日志、计数和私聊 ID 提示与被拒的文字消息相同。纯文字消息不走预判，行为不变。群里未 @bot 的带图消息此前下载完才被丢弃，现在直接不下载
- **`naozhi setup weixin` 写入 `im_access`**（#3513）：此前 setup 只写 `platforms.weixin.token`，新装的实例一启动就对所有能给 bot 发消息的人开放。现在把扫码确认的微信用户（登录响应的 `ilink_user_id`，与收到消息的 `from_user_id` 同一 ID 空间）写进 `im_access.platforms.weixin.allowed_users`，并在结束时打印微信入口的放行状态。登录响应没有可用的 ID 时：新建的配置文件写 `im_access.default_deny: true`（被拒的私聊会收到自己的 ID，再加进名单）；已有的配置文件不写 `default_deny`（会把其他平台关在外面），只打印需要补的配置。已有的 `im_access.platforms.weixin` 条目从不改动，没列出扫码用户时给出提示。**行为变化**：对已有配置重新扫码（如 token 过期后）且原先没有 weixin 条目时，微信入口会从「所有人可用」变为只放行扫码用户；`platforms.weixin:` 留空的配置以前不会写入 token，现在会写
- **Multipart Value 字段数上限 32**（RNEW-SEC-001），阻断 padded-body DoS
- **PDF 上传路径显式拒 gzip magic**（RNEW-SEC-002），defence-in-depth
- **Attachment ETag 改为 sha256 前 16 字符**（RNEW-SEC-004），不再通过响应头泄漏纳秒级 mtime
- **`safeUrl` 正则收敛至 `^(https?:|#)`**（RNEW-SEC-007），去除 `mailto:` / `/` 等历史遗留入口

### Fixed

- **`naozhi cost reconcile` 不再按 transcript 下调账本**（#3519）：按天残差以前双向记，transcript 用量比账本少超过 max($1, 5%) 的日子会写入负的 `Kind=adjust`。但 CLI 计费的请求并不都写进 transcript（取消或空闲后整段上下文重发的请求、后台请求，以及流式中途写下、比最终计费少的 output 计数），实测这类日子的差额正好等于这些没落行的用量，负残差会把 CLI 自报的正确花费调低。现在残差只往上补；账本高于 transcript 的日子只在报告里列出天数和金额（「账本高于 transcript 共 X，未下调」），包括 `--resume` 恢复额修正之后的余数。账本当天为负时补到 0 的规则不变
- **naozhi 被强杀或崩溃后，最近一次保存会话状态之后已记的花费不再重复记账**（#3518）：会话状态每 30 秒才落盘一次，而 cost ledger 每条记录约 1 秒内就写盘。naozhi 非正常退出（SIGKILL、panic、OOM、断电）且 CLI 经 shim 存活、重启后重新接管时，下一条 result 按落后的基线做差，这段时间已经记过的花费会在 ledger 里再记一次。现在会话自己的 ledger 记录带上记账后的会话花费与累计基线，重启恢复时若 ledger 比会话状态新，就以 ledger 为准；CLI 未存活时，会话的累计花费也不再少算这段时间
- **优雅重启时，在保存会话状态之后才报告的 turn 不再记两次费用**（#3428）：重启时 CLI 进程存活并在重启后重新接管，以前在会话状态保存之后、断开 shim 之前收到的 result（CLI 自己发起的 turn，或 30 秒关停等待超时后才结束的 turn）会立刻记入 cost ledger，但保存下来的累计基线还是旧值，重启后下一条 result 按旧基线做差，同一段花费又记一次。现在关停在保存前冻结记账，这段花费留给重启后的第一条 result 一并计入；它在 ledger 里归到下一个 run id 名下
- **`naozhi cost reconcile` 能归属已删除 dashboard 会话的首轮条目**（#3411）：#3494 之前，新会话首轮的 session-runs 记录不写 `session_id`，key 删掉后这些条目归不到任何 CLI session（本机实测 26 条）。现在按 transcript 的起头归属：只有一个 naozhi 用过的会话（`session-ids.json`）在该轮 run 的起止时间内（前后各 5s）起头、且这个起头不落在另一条同类 run 里时才归它；并发起头的首轮仍留着不处理（本机剩 2 条）
  - 这类记录的 run 开始时间也用于对账：以前首轮在记账前的消息会被当成"首条记账前的历史"而整天跳过，现在这些日子照常算残差，dry-run 可能多出几条修正
- **`naozhi cost reconcile` 归属与 restore 检测用同一个轮次截止点**（#3415）：key 对应过多个 session 时，归属窗口原本延到条目时间之后 5s，而 restore 检测的轮次窗口截止于条目时间。条目在 result 到达后才打时间戳，这一轮的消息都不会更晚，现在两处都截止于条目时间；下一个 session 的首条消息落在条目之后 5s 内时，这一轮不再归不了属
- `naozhi doctor` 的 CLI Backends 段 `Default:` 现在显示启动时实际绑定的默认 backend：`cli.backend` 未在 `cli.backends` 中列出、不是已注册的 backend id，或未设置且 `cli.backends` 首项无效时，此前打印的是配置值（例如 `Default: bogus`），而启动实际跑的是回退后的 backend。现在打印回退目标，并在括号里附上与启动告警相同措辞的原因（#3409）
- `/urgent` 之后，在它之前已排队的消息现在会拿到自己的真实回答，不再收到"上一条消息已被 /urgent 打断，请在当前任务完成后重发"：真实 CLI 实测（claude 2.1.288）表明 `priority:"now"` 抢占不丢弃队列，紧急消息先跑、排队消息随后各自成轮（`docs/rfc/passthrough-mode-validation.md` V10，#3394）
- 删除会话后立刻在同一个 key 上新建会话时，被删对话的记录不再留在新会话的 event log 里（#3416）：以前重启后它会出现在新会话 dashboard 历史的最前面，旧 workspace 的附件引用也一直不释放。现在删除会先清掉 event log 和附件引用、再关进程，新会话等清理完成（通常几毫秒，最多约 8 秒）才开始落盘
- **启用多个 backend 时，dashboard 不再替运维选 router 默认 backend**（#3418）：backend picker 第一项改为默认选中的「自动（X）」，不动它就不发 `backend`，由服务端按项目钉的 `backend` > `agents[].backend` > 访问档 `default_backend` > `cli.backend` 选；X 是所选访问档会落到的 backend，换访问档时跟着变（项目钉的 backend、`agents[].backend`、cron 任务所属 agent 的访问档、远端节点自己的访问档前端都看不到，这几种情况下 X 只是提示，以服务端为准）。以前 picker 总是预选 router 默认并当成显式选择发出，`default_backend`（#3364）和 `agents[].backend` 在 dashboard 入口从不生效
  - 同一原因的另外两处一起修好：保存项目设置不再把项目的 `backend` 钉成 router 默认（以前因任何原因保存一次，该项目的 IM 会话和 planner 就不再跟随 `default_backend`）；编辑没设 backend 的 cron 任务，保存时不再 PATCH 进 router 默认，新建 cron 任务选「自动」也不带 `backend`
  - 显式选某个 backend（包括 router 默认那个）仍原样发出并优先
  - 还没发出第一条消息的新会话，侧栏图标、会话头的 CLI 名、图片上传开关和模型列表跟随它将落到的 backend（显式选择，否则「自动」解析到的那个）；以前「自动」一律按 router 默认显示，显式选了 kiro 时图片上传开关也仍按 router 默认放行。远端节点上的显式选择同样驱动这些开关（与已列出的远端会话一致，按本节点缓存的 backend 清单查功能）；远端节点上的「自动」、单 backend 部署、以及第一条消息发出后到服务端列出该会话之前的这段时间，仍按 router 默认显示
  - 不做迁移：以前保存时被钉住的项目和 cron 任务保持原值（无法和有意的选择区分）。要恢复跟随，在项目设置或 cron 编辑里把 backend 选回「自动」并保存
- `spawnSession` panic recover 错误消息不再双前缀 `"spawn process: spawn process:"`（RNEW-009）
- IM 首轮自动接管不再在 naozhi 会拒绝接管时（max_procs 已满 / 该 key 正在 spawn / 正在关停 / planner 的 exempt 配额已满 / agent 的 model 或 backend 非法）先 SIGTERM 掉终端里的 Claude CLI；接管前改为先跑 router 的接管检查（#3395）
- 从未 spawn 过的源会话（历史面板 resume 占位 / backend 为空的旧持久化条目）上打开的 scratch 现在跑在源会话 resume 时会用的 CLI（router 默认 backend）上，不再落到 access profile 的 `default_backend`；`/api/scratch/open` 响应里的 `backend` 也改为报告实际解析出的 backend（#3420）
- 接管外部 CLI 时，naozhi 在 SIGTERM 之前就向 router 预留该 key（`Router.ReserveTakeover`：in-flight 标记 + 一个 pending 名额），一直持有到新进程 spawn。此前预检只读状态，旧 CLI 退出的最长约 5s 里 key 上没有任何标记：同一 cwd 的第二个外部 CLI 接管会通过预检并被杀掉，max_procs 只剩一个名额时对两个不同 key 的接管也都能通过、其中一个杀掉 CLI 后才报满。现在第二次接管在杀进程前就返回 409「takeover already in progress」/ 503（dashboard、IM 自动接管同此；#3417）
- 从终端接管后，dashboard 只在本次接管自己的状态报告 ready、且该 key 已列出时才把消息发进去；状态为 failed 时（包括 in_progress）立即停止等待并提示原因。此前 in_progress 会继续轮询，key 上一出现会话就把文本发进去，而那个会话可能是另一次接管恢复出来的。状态已过期（`unknown`，例如 naozhi 重启后）以及远端节点上的接管仍按 key 是否列出判断（#3417）
- Dashboard 的 Agent drill-in 走上 WS 实时推送：agent tailer 此前拿 operator workspace（`allowed_root`）当 transcript 根，`~/.claude/projects` 下的子 agent transcript 全被拒，客户端静默降级成 3s HTTP 轮询。现在 tailer 与 `/api/sessions/agent_events` 共用同一个解析后的 projects 根，并且两处都按 `PathContainedInRoot` 判定（macOS 上大小写与根不同的路径判定一致）
- 主节点经反向连接代理到 node 的接管（`takeover` RPC）同样在 SIGTERM 之前预留 key 并持有到 spawn：此前 node 侧的预检只读状态，同一 cwd 的第二次接管会在第一个 CLI 退出期间通过预检并杀掉第二个 CLI；现在它在杀进程前就被拒绝（`takeover refused: a spawn for this key is already in flight`）。身份校验失败、SIGTERM 失败或连接器关停时预留都会归还（#3417）
- reverse node 只对声明了 `send-status` 能力的 primary 回答「忙」（#3421）：v0.1.43 的 node 接在 v0.1.41 及更早的 primary 后面时，会话正忙、队列关闭而被丢弃的消息在 dashboard 上显示为「已接受」——旧 primary 不读 send 的返回状态。现在 primary 在 `registered` 应答里声明 `send-status`，node 对没有声明的 primary（包括 v0.1.42 / v0.1.43）改回 v0.1.43 之前的错误：「发送失败：会话正忙，消息未送达，请稍后重试」。升级顺序仍是先 primary 后 node；在 v0.1.41 primary 后面跑 v0.1.43 node 的部署请升级 primary。HTTP 拉取模式的 node 接在 v0.1.41 primary 后面同样显示「已接受」，这一侧没有握手可改，只能升级 primary（v0.1.42 起已修，#3209）

### Documentation

- `config.example.yaml` 补上注释掉的 `access_profiles` / `default_access_profile` 示例，以及 `agents[].access_profile` / `agents[].backend`，并写明 profile 的选取顺序、`default_model` 与 `default_backend` 在各自优先级链里的位置和 env 白名单；新测试把这段示例取消注释后跑一遍加载期校验，示例与代码不会再脱节（#3409）
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
