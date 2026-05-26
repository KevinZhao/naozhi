# RFC 索引

本目录收录 naozhi 的 RFC 工作文档（proposal / design notes / phase 报告），用于在落地前沉淀设计取舍与实测证据。**RFC 不是最终规范**——架构层面的事实请以 `../design/DESIGN.md` 为准，本目录文件可能处于 Draft / 已实装 / 已废弃等任意状态，请先看下表确认。

## 命名约定

- 普通文件名（如 `passthrough-mode.md`）是**当前有效**的那一份。
- 带 `.v1-deprecated`、`*-legacy-removal` 等后缀或状态标注为"Superseded"的文件保留是为了给后续读者提供历史上下文，不应再作为实施依据。
- Phase/验证报告（如 `passthrough-mode-validation.md`、`passthrough-mode-phase-c-report.md`）是实测快照，不是设计本身。

## 当前 RFC

| RFC | 状态 | 日期 | 范围 |
|---|---|---|---|
| [agent-team-ui.md](agent-team-ui.md) | Ready for implementation (v4) | 2026-05-10 | 并行 agent/team 可视化与内部过程查看（dashboard） |
| [askuser-question.md](askuser-question.md) | Proposal | 2026-05-10 | CC `AskUserQuestion` 工具在 `-p` 模式下的替代交互方案 |
| [auto-workspace-chain.md](auto-workspace-chain.md) | Draft v3（双独立评审通过） | 2026-05-23 | 同 workspace 多 session 自动接 prev_session_ids 让 dashboard 翻历史能跨 sessionID；启动一次性回填 + spawn 自动接，默认开启可配置 |
| [attachment-refcount.md](attachment-refcount.md) | v1 MVP 已落地（GC cron 待启用） | 2026-05-10 | 大图跨 TTL 可见：attachment 引用计数与双 TTL GC |
| [codex-backend.md](codex-backend.md) | 设计提案 Draft v1（未实测） | 2026-05-20 | OpenAI Codex CLI 作为第三个 backend：走 `codex app-server` JSON-RPC 2.0 over stdio（**非** `codex proto` / `exec --json` / `mcp-server`），新增 `protocol_codex.go` + `profile_codex.go` + `codexjsonl` 历史 source；待跑 Phase 0 V1-V12 实测后升 v2 |
| [consumer-interfaces.md](consumer-interfaces.md) | Proposal v2 | 2026-05-11 | ARCH-CONSUMER-IF：dispatch/hub/upstream 以消费端小接口替换 `*session.Router` 具体指针（v1 因方法清单虚构已重写） |
| [cron-v2-polish.md](cron-v2-polish.md) | 设计提案（未实现） | 2026-05-09 | Cron 面板 5 项增量打磨（name/jitter/missed/sort/next-run） |
| [cron-run-history.md](cron-run-history.md) | 设计提案 → 实施中 | 2026-05-17 | Cron 执行历史与生命周期可见性：CronRun 实体 / runs/ 滚动 ring / runInflight / WS started/ended / 时间轴 UI |
| [cron-panel-consolidation.md](cron-panel-consolidation.md) | 设计提案 | 2026-05-20 | 把 cron 当前执行 + 历史从 sidebar/mainShell 全部收编进「定时任务」面板（drawer 布局；`/api/sessions` 过滤 cron stub） |
| [cron-panel-consolidation-ui.md](cron-panel-consolidation-ui.md) | 设计提案（UI/UX 详细） | 2026-05-20 | 配套 cron-panel-consolidation：响应式断点、列表选中态、抽屉 6 段结构、状态机、文案、a11y、像素级 mockup |
| [cron-history-redesign.md](cron-history-redesign.md) | Draft v1（提案中） | 2026-05-21 | 修订 cron-panel-consolidation-ui v2：detail 默认进历史 view、run 详情独立 sheet（桌面右抽屉/移动 bottom sheet 共组件）、双栏取代三栏、移动 3 级 push view、状态信号收敛、URL deep-link |
| [event-log-persistence.md](event-log-persistence.md) | v3 GA 就绪 | 2026-05-10 | EventLog 磁盘持久化，图片与历史事件跨重启可见 |
| [key-resolver.md](key-resolver.md) | Phase 1-6 已实装（PR #9）；Phase 7 dashboard buildSessionOpts 待 | 2026-05-14 | ARCH3：收敛 planner/agent session key 派生；chat-view / planner-view 双接口（v2 修 v1 漏掉 #6/#7 不继承 defaults 的语义；Phase 6 删 dispatch 侧 legacy nil-resolver 分支） |
| [learning-system.md](learning-system.md) | 设计提案 | 2026-04-14 | 会话结束触发的闭环自学习（skills/MEMORY/USER） |
| [message-queue.md](message-queue.md) | 设计提案（未实现） | 2026-04-14 | 替代 sessionGuard 丢消息的 per-session 消息队列策略 |
| [multi-backend.md](multi-backend.md) | 设计提案 v2（基于实测修订） | 2026-05-18 | Claude + Kiro 多 backend 切换/并存：backend.Profile 抽象、kirojsonl 历史、ACP cancel notification、reverse cap 路由、per-session ReplyTag、Dashboard §8 26 项 UI 差异化规约 |
| [multi-backend-validation.md](multi-backend-validation.md) | Phase 0 实测报告 | 2026-05-18 | V1-V12 验证点的脚本、原始输出与 2 个真 bug 复现（multi-backend.md 的支撑材料） |
| [passthrough-mode.md](passthrough-mode.md) | v2.2 设计文档 | 2026-05-09 | 直通 CC CLI 原生 command queue，不做合并/节流 |
| [passthrough-mode-cc-tui-analysis.md](passthrough-mode-cc-tui-analysis.md) | 分析报告 | 2026-05-09 | CC TUI mid-turn 机制的源码级分析，交叉验证实测数据 |
| [passthrough-mode-validation.md](passthrough-mode-validation.md) | Phase 0 实测报告 | 2026-05-09 | V1-V9 验证点的脚本与原始日志汇总 |
| [passthrough-mode-phase-c-report.md](passthrough-mode-phase-c-report.md) | Phase C 实测报告 | 2026-05-06 | Dashboard 路径灰度实测结果与修复的 2 个 bug |
| [pdf-attachment.md](pdf-attachment.md) | 设计提案 → 实现中 | 2026-05-06 | Dashboard PDF 附件上传，走 workspace + Read 工具路径 |
| [process-split.md](process-split.md) | Proposal v2 | 2026-05-11 | ARCH-PROCESS-SPLIT：`cli/process.go` 2464 行按职责拆 7 份，纯文件移动零语义改动（v2 修正 shimMsg 归属、EventCallback 跨包使用、测试文件数） |
| [system-session.md](system-session.md) | 设计提案 Draft v2.1（已过三路 review + OQ 决议） | 2026-05-20 | naozhi 内置后台线程（System Session）统一抽象：`sys:` 命名空间、`internal/sysession/` Daemon+Manager+Runner（派生 transient system session 调 LLM）、AutoTitler MVP（中英双语 prompt、默认跳群聊、跟随 default backend）、`LabelOrigin` + ClearUserLabelOrigin、二段式 structured prompt + sweepOldJSONL on startup、cron/sys 共享 ExemptStub 注册路径。Phase 2 仅承诺浅归并不做接口归并 |
| [todo-to-issues-migration.md](todo-to-issues-migration.md) | 已实施（PR #370/#464/#1063/#1064） | 2026-05-26 | docs/TODO.md 1054 finding 迁移到 GitHub Issues：`triage-findings` skill 三桶分流 + 21 label 体系 + 4 cron prompt 改造（v8 GitHub Issues 单源 + v3 PR Merge cron-mergeable + verify Closes #N） + 6 并行 agent 处理 1042 Round dump finding + 581 issue 落地 / 200 cosmetic-backlog / 372 audit-trail discarded |

## 已废弃 / 已被取代

| RFC | 状态 | 说明 |
|---|---|---|
| [passthrough-mode.md.v1-deprecated](passthrough-mode.md.v1-deprecated) | Superseded by `passthrough-mode.md` | v1 误判 naozhi 需要节流/合并，基于对 CC CLI 内部队列行为的错误假设 |
| [passthrough-mode-legacy-removal.md](passthrough-mode-legacy-removal.md) | Draft（未开始） | Passthrough 默认开启 + ACP fallback 并发 gate 的遗留代码移除计划 |

> 状态标注以 RFC 文件内首屏（Status / 状态）为准；若 RFC 未写明状态，本表标 "unknown"。如发现表格与 RFC 本体不一致，请同步修正。
