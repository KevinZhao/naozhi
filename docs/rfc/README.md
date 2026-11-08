# RFC 索引

RFC 是落地前的设计取舍与实测记录，**不是最终规范**——架构层面的事实以 `../design/DESIGN.md` 为准。下表的状态按 v0.6.4 的代码核对过（2026-11-08）；不少 RFC 首屏的状态没跟上，两者不一致时以下表为准。已实施或废弃的历史 RFC 已删除，需要时用 `git log -- docs/rfc/<name>.md` 找回。

| RFC | 状态 | 范围 |
|---|---|---|
| [agent-team-ui.md](agent-team-ui.md) | 已实装（drill-in / agent_events） | 并行 agent/team 可视化与内部过程查看 |
| [attachment-gc-daemon.md](attachment-gc-daemon.md) | 已实现（默认关闭 + dry_run） | sysession `attachment-gc` daemon |
| [attachment-refcount.md](attachment-refcount.md) | v1 MVP 已落地 | 附件引用计数与双 TTL GC |
| [cc-asset-browser.md](cc-asset-browser.md) | 已实装（P2 的 TTL + singleflight 缓存未做） | Claude Code 资产只读浏览器 |
| [cli-mcp-config.md](cli-mcp-config.md) | 已实装 | `cli.mcp_config` → `--mcp-config` |
| [codex-backend.md](codex-backend.md)（+ `-validation` / `-phase2-feasibility`） | 部分实装：Phase 1 已合；Phase 2 只做了 embedded_context，askuser 卡片与 turn/steer 未接 | Codex CLI app-server backend |
| [computer-use.md](computer-use.md) | 部分实装：P1 的工具结果截图进事件流已做；截屏发送、截图 MCP、P2–P4 未做 | 计算机使用：桌面截图进 CC 上下文 + 以 MCP 操作这台 Mac（浏览器专用配置 + 域名白名单、桌面 AX 优先），决策 CU1–CU9 |
| [config-hot-reload.md](config-hot-reload.md) | Phase 1 已实现 | SIGHUP / API / CLI 配置热重载 |
| [cost-ledger.md](cost-ledger.md) | 已实装（PR-1~6；backfill / reconcile 命令已于 v0.1.49 删除） | `internal/costledger` 统一 cost 归口 |
| [cron-run-adoption.md](cron-run-adoption.md) | 已实装 | 升级不吞掉正在跑的 cron run |
| [cron-sysession-merge.md](cron-sysession-merge.md) | Implemented v5 | cron + sysession 调度层合并 |
| [dashboard-model-effort-control.md](dashboard-model-effort-control.md) | 已实装 | 会话级 model / effort 切换 |
| [dashboard-update-notice.md](dashboard-update-notice.md) | 已实装 | 新版本发现与一键生效 |
| [event-log-persistence.md](event-log-persistence.md) | v3 GA | EventLog 磁盘持久化 |
| [memory-link-rendering.md](memory-link-rendering.md) | 已实装（#261） | memory 链接渲染 |
| [meeting-notes.md](meeting-notes.md) | Draft v1（方向待确认，未实施） | 会议录音、转写与纪要：Mac 线上会议自动录（进程音频捕获 + 麦克风两条音轨）、现场会议与手机 dashboard 录音、本机 Qwen3-ASR 识别、纪要进成品库、待办进任务，决策 MR1–MR7 |
| [multi-backend.md](multi-backend.md) | 已实装（v2，基于实测） | Claude + Kiro 多 backend |
| [naozhi-owned-settings-v3.md](naozhi-owned-settings-v3.md) | 部分实装：PR-1/PR-2 已交付，PR-3（thinking 旋钮）、PR-4（UI）未做 | naozhi 独立 settings |
| [node-pairing.md](node-pairing.md) | Draft v1（未实施） | `naozhi node add` / `naozhi join` |
| [outbound-webhooks.md](outbound-webhooks.md) | Phase 1 已实现 | run 生命周期出站 webhook |
| [passthrough-mode.md](passthrough-mode.md)（+ `-validation`） | 已实装（opt-in，代码默认仍是 collect；Phase E 未做） | 直通 CC CLI 原生 command queue |
| [planner.md](planner.md)（+ `planner-landscape`） | 已实装到 v0.6.4（§15.1–§15.11）；v0.6.5–v0.8.0 改造规格见 §15.12（D100–D108，owner 11-08 批准，§17.2 已全部采纳），剩余内容排期见 §15.13 | Planner 规划 agent：每个节点一个 Planner（D96 修订"全局唯一"）+ 按需工作区幕僚、设计原则（确定性放网关 / 先信任后惊喜）、三种主动、信任阶梯与治理减法、三个面与工作台面、现有 dashboard 上新增 Planner 页面；§18 决策 D1–D108；`planner-landscape.md` 是两代 agent 产品调研与对照 |
| [project-access-profile.md](project-access-profile.md) | 已实装（PR-A / B / C，#2359） | 项目级默认访问方式 |
| [project-stable-session-key.md](project-stable-session-key.md) | 已实装 | 项目级稳定 session key |
| [remote-planner.md](remote-planner.md) | 已实装（#3900，随 v0.6.0 发布） | 每个节点一个 Planner、只管本节点（D60 修订）；EC2 页面可以切换到 Mac 的 Planner，经反连 RPC `api` 转发 Planner 路由；巡检与早报只在一个节点上；会话列表按节点分组；断线降级与鉴权边界，决策 D96 |
| [reverse-protocol.md](reverse-protocol.md) | 已实现 | reverse-connect wire format |
| [selfupdate-signing.md](selfupdate-signing.md) | 部分实装：签名工具、release 流水线与 `.sig` 校验已合；`trustedSigKeys` 仍为空，客户端实际不验签（#1738；排期见 planner.md §15.13） | 自更新二进制签名 |
| [sysession-telemetry-and-hardfail.md](sysession-telemetry-and-hardfail.md) | 部分实装：Phase 1（telemetry）已合；Phase 2 未做，`OnHardFail` 默认仍是 `os.Exit` | sysession 接 runtelemetry Broadcaster |
| [system-session.md](system-session.md) | 部分实装：Phase 1 已上线；Phase 2 只加了 attachment-gc daemon | System Session 统一抽象 |
| [workflow-dashboard.md](workflow-dashboard.md) | 已实现 v5 | Workflow 运行进度看板 |
| [wsproto.md](wsproto.md) | Phase 1 已实装（含 `contract.js`） | WS type 常量与 per-type frame |

> 已删除的历史 RFC（代码注释里仍有路径引用）：`consumer-interfaces`, `cron-panel-consolidation`, `cron-panel-consolidation-ui`, `cron-run-history`, `cron-v2-polish`, `csp-data-action`, `dashboard-cron-view-extraction`, `dashboard-es-modules`, `dashboard-event-uuid-idempotent-render`, `direct-user-settings`, `evententry-versioning`, `key-resolver`, `kiro-effort-control`, `kiro-effort-visibility`, `lightbox-gallery-nav`, `multi-backend-validation`, `pdf-attachment`, `send-engine-extraction`, `session-run-metrics`, `shim-golden-compat-drift`。原文可用 `git show 1793eda8:docs/rfc/<name>.md` 找回。
