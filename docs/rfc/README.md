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
| [agentcore-cloud-sandbox.md](agentcore-cloud-sandbox.md) | **v2（Phase 0 实测通过）** | 2026-06-10 | AWS Bedrock AgentCore Runtime 接成**执行基底（placement 轴，与 backend flavor 正交）**：控制面/数据面分离 + run-once 作业模型（不 resume，debug 走 replay）；A1-a naozhi hold 流式连接（microVM 无需反向可达）；不新增 flavor/protocol_agentcore，Runner 抽象（localRunner/agentcoreRunner）复用 protocol_claude.go；§7 界面设计（cron placement 选择器+围栏校验 / ☁️ 徽标三态 / run 详情+replay / 副作用人工确认队列）；安全机制：runtimeSessionId=run_id、双跑三件套封堵、run record 排除 secrets、in-flight reconcile；PR-1 Runner seam 已合（#2003） |
| [agentcore-cloud-sandbox-validation.md](agentcore-cloud-sandbox-validation.md) | Phase 0 实测报告 | 2026-06-10 | V1-V4+V8 全过：microVM 内 CC 完整 turn（含 Bash 工具）/ spawn→init 0.6-0.7s / 内存 ~250MB（无 MCP）/ 三态判定 + StopRuntimeSession 即焚实证 / 30.2min hold 流全程不断；6 个实现层发现（SDK hold 流、禁后台任务、sessionId ≥33 字符、非 root、专用执行角色、**F6 idle 按流静默判定→keepalive 硬前提**）（agentcore-cloud-sandbox.md 的支撑材料） |
| [askuser-question.md](askuser-question.md) | Proposal | 2026-05-10 | CC `AskUserQuestion` 工具在 `-p` 模式下的替代交互方案 |
| [auto-workspace-chain.md](auto-workspace-chain.md) | Draft v3（双独立评审通过） | 2026-05-23 | 同 workspace 多 session 自动接 prev_session_ids 让 dashboard 翻历史能跨 sessionID；启动一次性回填 + spawn 自动接，默认开启可配置 |
| [attachment-gc-daemon.md](attachment-gc-daemon.md) | 已实现（v2.1 设计落地；默认 enabled:false + dry_run 待运维启用） | 2026-05-31 | 接活死代码:把 `GCWithRefs` 接进 sysession `attachment-gc` daemon（非 cron），删 legacy `GC`，修复附件磁盘无限增长（#1198）。v2 补三项框架增强（ctx + per-root cap + startup-tick）+ 枚举改扫已知 workspace 根 + 默认 enabled:false/dry-run + 先删 .meta 防孤儿 + refcount best-effort。v2.1 修枚举完整性:root 集合补 project workspace（E1）、规范化去重防 symlink（E2）、per-root cap + round-robin 防饥饿（E3）、dry-run 按原因分桶（E4）、附最小止血版（E5） |
| [attachment-refcount.md](attachment-refcount.md) | v1 MVP 已落地（GC cron 待启用） | 2026-05-10 | 大图跨 TTL 可见：attachment 引用计数与双 TTL GC |
| [direct-user-settings.md](direct-user-settings.md) | Draft v2（architect 评审通过修订；security 评审中） | 2026-05-30 | naozhi spawn cc 改 `--setting-sources user` 直接加载 `~/.claude/settings.json`，删 override 副本 + env 白名单，使 naozhi cc 与命令行 cc 行为一致、单一配置源；保留 `applyClaudeEnvSettings`（父进程 Bedrock 鉴权）与 sysession Runner 的 `--setting-sources ""`（防 AutoTitler 死循环） |
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
| [lightbox-gallery-nav.md](lightbox-gallery-nav.md) | Draft v2（双评审通过修订） | 2026-06-10 | dashboard lightbox 多图导航：同消息图片组内左右切换（按钮/←→/swipe）+ 计数器 + 方向性预加载 + 工具栏 ± 缩放按钮 + 图片点击迁委托监听；纯前端零后端改动；测试主体 Playwright e2e |
| [managed-session-split.md](managed-session-split.md) | Implemented (v2.3) | 2026-05-29 | ARCH-MANAGED-SPLIT：`session/managed.go`(2262 行 / 76 func)按职责拆 6 份，纯文件移动零语义改动（identity/lifecycle/send/history/query），与 router 家族风格对称；解 churn 单文件第 3 高的冲突面 + 解锁 ProcessSender/EventReader facet 拆分。5 phase 全部落地 build/vet/test-race 全绿、func 计数稳定 76；实施中发现 §6"测试零改动"被 2 个 source-introspecting 测试证伪（已修文件名字面量，断言意图不变）。同 process-split 手法 |
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
| [envpolicy-consolidation.md](envpolicy-consolidation.md) | Draft v1（待评审） | 2026-06-04 | #891：抽 `internal/envpolicy` 收敛三处分散的 Claude env 过滤策略；Phase 1 可落地（共享叶子校验器 + per-backend 凭据矩阵，行为零变化） |
| [cron-runstore-facade.md](cron-runstore-facade.md) | Draft v1（待评审） | 2026-06-04 | #509/#978：补全 runStore 半facade，4 个 scheduler 文件经 `*Scheduler` wrapper 访问 + AST gate 测试；Phase 1 可落地（包内 behavior-preserving forwarding） |
| [cron-sysession-merge.md](cron-sysession-merge.md) | Implemented v5（2026-06-10 全 phase 落地） | 2026-05-26 | #1166/#1173/#1164/#734/#945/#1036/#746：cron+sysession 调度层合并 7 phase 全部完成。A1/A2/B/D-main Broadcaster 经 PR #1264+#1754；v4 订正识别的残留项同日清偿：SchedulerDeps (#746, PR #2002)、dispatch→cron import 切断 (#1164, PR #2004)、executeOpt 7-helper 拆分 (#734/#945, PR #2017，race gate -count=10/100 全绿) |
| [sysession-telemetry-and-hardfail.md](sysession-telemetry-and-hardfail.md) | Draft v1（待评审） | 2026-06-04 | #1723/#1169/#1055：sysession 收敛到 `runtelemetry.Broadcaster` seam（复刻 cron 范式，删镜像结构体/hook_holders）；Phase 1 可落地；hard-fail 统一留后续 |
| [eventlog-subsystem-unify.md](eventlog-subsystem-unify.md) | Draft v1（待评审） | 2026-06-04 | #737/#1369：统一 eventlog 5-包散落 / 三层影子；Phase 1 仅 additive 编译期接口断言闸门 |
| [cron-config-and-structs.md](cron-config-and-structs.md) | Draft v1（待评审） | 2026-06-04 | #776/#764/#1282/#1278/#837：cron config functional-options + Job god-struct 拆分 + 大文件拆 + Outbox saga；仅 #776 docstring+parity 测试可落地，余须先评审 |
| [cron-notify-sender.md](cron-notify-sender.md) | Draft v1（待评审） | 2026-06-04 | #725：抽 NotifySender 接口斩 cron→platform 反向依赖；通知协议（chunk/retry/telemetry/stopCtx）迁移须先评审 |
| [selfupdate-signing.md](selfupdate-signing.md) | Draft v1（待评审） | 2026-06-04 | #1738：自更新二进制加密签名（cosign keyless vs 自管 ed25519）；须先评审密钥信任模型，本轮不落地 |
| [dashboard-csp-strict.md](dashboard-csp-strict.md) | Draft v1（待评审） | 2026-06-04 | #1734/#922：dashboard CSP 去 unsafe-inline（nonce/strict-dynamic）；前端+CSP+鉴权联动 high risk，须先评审 |
| [router-god-object-split.md](router-god-object-split.md) | Implemented v2（2026-06-10 订正） | 2026-06-04 | #383/#600/#805/#580/#577：合并五个 session.Router 拆分锚点为单一路线图（单 mutex + 渐进 facet 抽取 P0-P7）。P0-P5 已分 PR 落地（#1762/#1796/#1802/#1837/#1804/#1841/#1852），P6 由 managed-session-split 交付，P7 经核实关闭 won't-do（#577） |
| [lifecycle-policy-and-naming.md](lifecycle-policy-and-naming.md) | Draft v1（待评审） | 2026-06-04 | #870/#463/#729：lifecycle policy 接口（须等 restart RFC）/ Get*Fetch*Load* 165处 rename ADR / AutoTitler 持久进程（撞 SharedCLI 决策）；均须先评审 |

## 已废弃 / 已被取代

| RFC | 状态 | 说明 |
|---|---|---|
| [passthrough-mode.md.v1-deprecated](passthrough-mode.md.v1-deprecated) | Superseded by `passthrough-mode.md` | v1 误判 naozhi 需要节流/合并，基于对 CC CLI 内部队列行为的错误假设 |
| [passthrough-mode-legacy-removal.md](passthrough-mode-legacy-removal.md) | Draft（未开始） | Passthrough 默认开启 + ACP fallback 并发 gate 的遗留代码移除计划 |

> 状态标注以 RFC 文件内首屏（Status / 状态）为准；若 RFC 未写明状态，本表标 "unknown"。如发现表格与 RFC 本体不一致，请同步修正。
