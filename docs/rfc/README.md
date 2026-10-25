# RFC 索引

RFC 是落地前的设计取舍与实测记录，**不是最终规范**——架构层面的事实以 `../design/DESIGN.md` 为准。状态以各 RFC 首屏的 Status / 状态为准，下表只是速查。已实施或废弃的历史 RFC 已删除，需要时用 `git log -- docs/rfc/<name>.md` 找回。

| RFC | 状态 | 范围 |
|---|---|---|
| [agent-team-ui.md](agent-team-ui.md) | 已实装（drill-in / agent_events） | 并行 agent/team 可视化与内部过程查看 |
| [attachment-gc-daemon.md](attachment-gc-daemon.md) | 已实现（默认关闭 + dry_run） | sysession `attachment-gc` daemon |
| [attachment-refcount.md](attachment-refcount.md) | v1 MVP 已落地 | 附件引用计数与双 TTL GC |
| [cc-asset-browser.md](cc-asset-browser.md) | Draft v6 | Claude Code 资产只读浏览器 |
| [cli-mcp-config.md](cli-mcp-config.md) | 已实装 | `cli.mcp_config` → `--mcp-config` |
| [codex-backend.md](codex-backend.md)（+ `-validation` / `-phase2-feasibility`） | Draft v2，Phase 0 实测 | Codex CLI app-server backend |
| [config-hot-reload.md](config-hot-reload.md) | Phase 1 已实现 | SIGHUP / API / CLI 配置热重载 |
| [consumer-interfaces.md](consumer-interfaces.md) | Implemented v3 | 消费端小接口替换 `*session.Router` 指针 |
| [cost-ledger.md](cost-ledger.md) | Draft v2.1 | `internal/costledger` 统一 cost 归口 |
| [cron-panel-consolidation.md](cron-panel-consolidation.md)（+ `-ui`） | 已实装 | cron 执行与历史收编进「定时任务」面板（抽屉状态机） |
| [cron-run-adoption.md](cron-run-adoption.md) | 已实装 | 升级不吞掉正在跑的 cron run |
| [cron-run-history.md](cron-run-history.md) | 已实装 | CronRun 实体 / runs ring / 时间轴 UI |
| [cron-sysession-merge.md](cron-sysession-merge.md) | Implemented v5 | cron + sysession 调度层合并 |
| [cron-v2-polish.md](cron-v2-polish.md) | 已实装 | cron name / jitter / missed / sort / next-run |
| [csp-data-action.md](csp-data-action.md) | 已实施 | 内联 `on*=` 迁 `data-action` 委托 |
| [dashboard-cron-view-extraction.md](dashboard-cron-view-extraction.md) | 已实装 | cron 视图从 dashboard.js 抽出 |
| [dashboard-es-modules.md](dashboard-es-modules.md) | — | static/*.js 原生 ES module 化 |
| [dashboard-event-uuid-idempotent-render.md](dashboard-event-uuid-idempotent-render.md) | Draft v1 | 事件 uuid 幂等渲染 |
| [dashboard-model-effort-control.md](dashboard-model-effort-control.md) | 已实装 | 会话级 model / effort 切换 |
| [dashboard-update-notice.md](dashboard-update-notice.md) | 已实装 | 新版本发现与一键生效 |
| [direct-user-settings.md](direct-user-settings.md) | Draft v2（被 naozhi-owned-settings-v3 部分反转） | `--setting-sources user` |
| [event-log-persistence.md](event-log-persistence.md) | v3 GA | EventLog 磁盘持久化 |
| [evententry-versioning.md](evententry-versioning.md) | — | EventEntry schema 版本 |
| [key-resolver.md](key-resolver.md) | Phase 1-6 已实装 | planner/agent session key 派生 |
| [kiro-effort-control.md](kiro-effort-control.md) | 已实现 | `cli.effort` → `acp --effort` |
| [kiro-effort-visibility.md](kiro-effort-visibility.md) | 已实装 | kiro effort 透出到会话头部 |
| [lightbox-gallery-nav.md](lightbox-gallery-nav.md) | 已实装 | lightbox 多图导航 |
| [memory-link-rendering.md](memory-link-rendering.md) | Draft v1 | memory 链接渲染 |
| [multi-backend.md](multi-backend.md)（+ `-validation`） | v2，基于实测 | Claude + Kiro 多 backend |
| [naozhi-owned-settings-v3.md](naozhi-owned-settings-v3.md) | PR-1/PR-2 已交付 | naozhi 独立 settings |
| [node-pairing.md](node-pairing.md) | Draft v1 | `naozhi node add` / `naozhi join` |
| [outbound-webhooks.md](outbound-webhooks.md) | Phase 1 已实现 | run 生命周期出站 webhook |
| [passthrough-mode.md](passthrough-mode.md)（+ `-validation`） | v2.2 | 直通 CC CLI 原生 command queue |
| [pdf-attachment.md](pdf-attachment.md) | 已实装 | PDF 附件（dashboard + IM） |
| [planner.md](planner.md) | Draft v6（未实施） | Planner 规划 agent，§18 决策 D1–D59 |
| [project-access-profile.md](project-access-profile.md) | Draft v1 | 项目级默认访问方式 |
| [project-stable-session-key.md](project-stable-session-key.md) | 已实装 | 项目级稳定 session key |
| [reverse-protocol.md](reverse-protocol.md) | 已实现 | reverse-connect wire format |
| [selfupdate-signing.md](selfupdate-signing.md) | Draft v1（#1738） | 自更新二进制签名 |
| [send-engine-extraction.md](send-engine-extraction.md) | v2 | `sendEngine` 抽取及其不变量 |
| [session-run-metrics.md](session-run-metrics.md) | Draft v2 | 普通 session run 耗时度量 |
| [shim-golden-compat-drift.md](shim-golden-compat-drift.md) | — | shim 契约 golden 兼容矩阵 |
| [sysession-telemetry-and-hardfail.md](sysession-telemetry-and-hardfail.md) | Draft v1 | sysession 接 runtelemetry Broadcaster |
| [system-session.md](system-session.md) | Draft v2.1 | System Session 统一抽象 |
| [workflow-dashboard.md](workflow-dashboard.md) | 已实现 v5 | Workflow 运行进度看板 |
| [wsproto.md](wsproto.md) | — | WS type 常量与 per-type frame |
