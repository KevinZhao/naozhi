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
