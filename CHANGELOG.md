# Changelog

该项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 的格式。版本号按语义化版本（Semantic Versioning）管理。

真正的 per-round 变更日志在 `docs/TODO.md` 顶部，本文件只归档对用户 / 运维可感知的大型变更。

## [Unreleased]

### Added

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

在 2026-05-07 之前，所有变更记录在 `docs/TODO.md` 的 `Round NN` 小节里，不回填到本文件。后续版本发布时，将抽取对用户可感知的条目归档到这里。
