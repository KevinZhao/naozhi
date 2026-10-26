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
| [computer-use.md](computer-use.md) | Draft v1（方向已确认，未实施） | 计算机使用：桌面截图进 CC 上下文 + 以 MCP 操作这台 Mac（浏览器专用配置 + 域名白名单、桌面 AX 优先），决策 CU1–CU8 |
| [config-hot-reload.md](config-hot-reload.md) | Phase 1 已实现 | SIGHUP / API / CLI 配置热重载 |
| [cost-ledger.md](cost-ledger.md) | Draft v2.1 | `internal/costledger` 统一 cost 归口 |
| [cron-run-adoption.md](cron-run-adoption.md) | 已实装 | 升级不吞掉正在跑的 cron run |
| [cron-sysession-merge.md](cron-sysession-merge.md) | Implemented v5 | cron + sysession 调度层合并 |
| [dashboard-model-effort-control.md](dashboard-model-effort-control.md) | 已实装 | 会话级 model / effort 切换 |
| [dashboard-update-notice.md](dashboard-update-notice.md) | 已实装 | 新版本发现与一键生效 |
| [event-log-persistence.md](event-log-persistence.md) | v3 GA | EventLog 磁盘持久化 |
| [memory-link-rendering.md](memory-link-rendering.md) | Draft v1 | memory 链接渲染 |
| [multi-backend.md](multi-backend.md) | v2，基于实测 | Claude + Kiro 多 backend |
| [naozhi-owned-settings-v3.md](naozhi-owned-settings-v3.md) | PR-1/PR-2 已交付 | naozhi 独立 settings |
| [node-pairing.md](node-pairing.md) | Draft v1 | `naozhi node add` / `naozhi join` |
| [outbound-webhooks.md](outbound-webhooks.md) | Phase 1 已实现 | run 生命周期出站 webhook |
| [passthrough-mode.md](passthrough-mode.md)（+ `-validation`） | v2.2 | 直通 CC CLI 原生 command queue |
| [planner.md](planner.md) | Draft v6（未实施） | Planner 规划 agent，§18 决策 D1–D59 |
| [project-access-profile.md](project-access-profile.md) | Draft v1 | 项目级默认访问方式 |
| [project-stable-session-key.md](project-stable-session-key.md) | 已实装 | 项目级稳定 session key |
| [reverse-protocol.md](reverse-protocol.md) | 已实现 | reverse-connect wire format |
| [selfupdate-signing.md](selfupdate-signing.md) | Draft v1（#1738） | 自更新二进制签名 |
| [sysession-telemetry-and-hardfail.md](sysession-telemetry-and-hardfail.md) | Draft v1 | sysession 接 runtelemetry Broadcaster |
| [system-session.md](system-session.md) | Draft v2.1 | System Session 统一抽象 |
| [workflow-dashboard.md](workflow-dashboard.md) | 已实现 v5 | Workflow 运行进度看板 |
| [wsproto.md](wsproto.md) | — | WS type 常量与 per-type frame |

> 已删除的历史 RFC（代码注释里仍有路径引用）：`consumer-interfaces`, `cron-panel-consolidation`, `cron-panel-consolidation-ui`, `cron-run-history`, `cron-v2-polish`, `csp-data-action`, `dashboard-cron-view-extraction`, `dashboard-es-modules`, `dashboard-event-uuid-idempotent-render`, `direct-user-settings`, `evententry-versioning`, `key-resolver`, `kiro-effort-control`, `kiro-effort-visibility`, `lightbox-gallery-nav`, `multi-backend-validation`, `pdf-attachment`, `send-engine-extraction`, `session-run-metrics`, `shim-golden-compat-drift`。原文可用 `git show 1793eda8:docs/rfc/<name>.md` 找回。
