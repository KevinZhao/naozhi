# 两代 agent 产品与 Planner RFC 的比较（调研 2026-10-26）

- 状态: 调研报告（支撑 `planner.md` §17.1 的讨论，不是决策）。采纳情况（10-26）：A1 修正为全局 Planner + 工作区幕僚（D60），A2 修正为三种主动、守护型先行（D62），A3 → D63，B1–B13 → D67，C1–C6 → `planner.md` §16；另新增 D61 设计原则、D64–D66
- 日期: 2026-10-26
- 范围: 两代产品各自的形态、共同原语与公认难题；逐项对照 `planner.md` v6；列出建议加入 RFC 的条目
- 来源: 官方文档 / 公告与第三方实测，链接见附录；留存、用户数等数据多为第三方估算或厂商自报

## 1. 两代产品

| | 第一代：会话编排 / 编程 agent 管理 | 第二代：主动型个人 agent |
|---|---|---|
| 代表 | Claude Code Projects（09-17）、Codex app、Devin（Managed Devins）、Cursor Projects（09-10）、Kiro Crew（08-04）、Copilot app、Antigravity、Conductor | Grok Bot（08-11）、Instinct（08）、Meta Muse（09-08）、Copilot Autopilot（09-25）、OpenAI Dots（09-29）、Hark Pro（10-06）、Gemini agent（10-08）、Claude Duet（10-21）、Fo；本机路线有 Perplexity Personal Computer |
| 谁先开口 | 你派单 | **agent 先开口**：早报、定时 check-in、跟进沉寂的线程 |
| 工作单元 | 会话 / 线程 / worktree / PR | 一段持续关系：一条主线程 + channels / side chat，共享记忆 |
| "完成"由谁判 | 测试与 CI | 只能由你判断；审批卡片的作用是转移责任，不是验证正确性 |
| 执行环境 | 本机或厂商云的开发环境 | **每人一台云电脑**（含浏览器），没有接口就靠浏览器补位 |
| 主要界面 | 侧栏列表 + diff + "等你处理"收件箱 | 主线程 + 任务列表 + 成品库 + 早报；手机优先，或直接住在短信 / iMessage 里 |
| 词汇 | cron、connector、worktree | feed、ideas、goals、library（刻意避开工程词） |
| 安全 | 沙箱、权限模式、auto mode 分类器 | 凭证不进模型、独立授权方、外发信息分类器、一次性虚拟卡、硬禁区 |
| 公认难题 | 冲突、CI、上下文 | 主动频率校准、个人 agent 疲劳、闭环、延迟、批准疲劳、隐私、留存 |

两代的连接点：**Dots 可以在本机新建 Codex 任务、续跑已有的本地 Codex 线程**（OpenAI 走得最远）；Grok Bot 出身 Cursor；Microsoft 把 Home / Code / Autopilot 放进同一个 Copilot app；Gemini agent 把聊天、执行、写代码放在一个界面。反例：**Duet 能看到你电脑上的 Claude Code 会话，但指挥不了**，这是评测者对它最大的批评之一。目前没有一个产品把"个人主动 agent"和"指挥并评审多个编程会话"做成一体。

## 2. 第二代的共同原语与教训

### 2.1 原语

| 原语 | Duet | Muse | Dots | 其他 |
|---|---|---|---|---|
| 主线程 + 分支 | 主线程 + channels（共享记忆） | 主对话 + side chat | 单一主线程，dot 可开 cloud thread / 本地 Codex 线程 | Grok：多 Bot + 群聊；Hark：Projects |
| 任务列表 | Tasks：进行中 / 即将 / 已排期 / 已解决；**只有你能标完成** | 头像页：Activity / Approvals / Upcoming | profile：Activity / Scheduled / Computers | — |
| 成品库 | Make | Library | Outputs / ChatGPT Space | Hark：Panels（一句话生成的小应用） |
| 早报与 check-in | 内置早报；9:00–22:00 每 3 小时检查 | Feed 栏目与节奏用一句话定义；约 30 分钟一次后台检查 | 没有内置早报，用定时任务做；空闲时只读调研 | — |
| 状态展示 | Tasks 页 | **三层**：头像下一行摘要 → 顶部状态胶囊 → 完整工具调用链路 | Active now；云电脑画面可接管 | — |
| 频率调节 | 一句话改 | 一句话"调高 / 调低 / 关掉"，或改 Soul 文件 | 告诉它哪些变化值得通知、发到哪个渠道 | — |
| 审批 | 卡片列出动作、目的地、内容与全部收件人；独立意图检查 | Sentinel 弹出、agent 无法伪造；通知栏直接 Allow / Deny | Custom Rules 四档，只能在底线之上收紧；Auto-review 独立于 dot | Grok：Allow once / Always / Deny，**无人在场的审批约 10 分钟作废** |
| 硬禁区 | 证券交易、账户间转账、访问他人账户；付款 / 删除订单 / 建账户**永远不能设为"别再问"** | 发布、发送、花钱一律审批 | 改密码、转账交还你亲自做 | — |
| 凭证 | 模型永远看不到；外部代理在请求发出时注入 | authd 替身 token，Sentinel 出网时注入 | 私密表单直送远程浏览器 | Grok：你接管屏幕自己输 |
| 外发信息 | 分级分类器（永不外发 / 需同意） | eBPF 污点出网 | 收件人类别须在授权内 | Google / Microsoft：DLP |
| 记忆可见性 | 沿用 Claude 记忆 | MEMORY.md 可编辑；但**每小时更新的人物档案对你不可见**（TIME 披露） | **不能逐条查看或删除**，只能删掉整个 dot | — |
| 写作风格 | 内置技能读你 33 封已发邮件等生成风格指南 | — | — | — |

### 2.2 教训

1. **最受好评的是预判，不是执行**：日历上的看房日期写错、会前简报加上"这通电话紧挨你的航班，要改时间吗"（Duet）；确认邮件里活动改了日期、改签航班会撞上录制（Dots）；团建预算表公式错误超过 $5,700（Dots）；两天内抓到周六日程撞车（Muse）。
2. **频率两头都会失败**：Muse 被批评"太渴望互动"、误报、未经要求给会议对象建档案而被弃用；Duet 被批评"设置完就再没找过我"、被遗忘。"三点式早报"已经拉不回人（Claire Vo）。
3. **闭环比能力难**：Duet 任务常要 10 分钟以上，通知又轻，用户转头就忘；Dots 被反映"开始丢任务"、报税做一半没提醒。
4. **速度本身是功能**：每个上手 Muse 的测试者第一句都在夸快（输出 200+ tok/s，VM 常驻无冷启动）；Duet 和 Dots 都被说慢。
5. **新鲜感衰减很快**：Muse 的 D1 留存 73%、D7 30%、D14 23%，D1→D14 掉约 50 个百分点（同类约 12 个）。
6. **批准疲劳是真的**：Dots 简单操作要批好几次、批了仍执行不了；第三方引述 Claude Code 用户批准了 97% 的权限请求，混入的危险命令人类只识别出 13.6%。
7. **桌面不能缺**：Duet 没有桌面 app 被认为是日常使用的最大障碍；Muse 的 Mac 版靠菜单栏、全局快捷呼出、状态胶囊常驻桌面而受好评。
8. **浏览器要能看、能接管**：Duet 看不到也接管不了，碰到人机验证就卡死；Dots 可以 Take over。
9. **隐形画像会出事**：Muse 用户看不到的人物档案被曝光，成为信任问题；Dots 不能逐条删记忆被批评。

## 3. 和 Planner RFC v6 逐项对照

状态：✅ 已有　◐ 部分　✗ 缺失　— 有意不同

| # | 第二代的做法 | RFC v6 现状 | 状态 |
|---|---|---|---|
| 1 | 一个持续关系：一条主线程 + 共享记忆的 channels | 每个工作区一个 Planner（D4），多 Planner 经 `request` 协作（D19）；"懂你"在每个 Planner 里各持一份 | ◐ 结构不同 |
| 2 | agent 先开口：第一天就有早报与 check-in | 主动模式在 P5，首个落在 naozhi 仓库（D8）；早报只是收件箱的一个分组 | ◐ 太晚、太窄 |
| 3 | 外部来源（邮件 / 日历）定时巡检 | 只有事件唤醒与网关对账（D31、D49），不做模型心跳；没有覆盖没有事件推送的外部系统 | ✗ |
| 4 | 预判（日历冲突、会前简报） | 顾问 · 预判（§4.3） | ✅ 但信号源没写日历和邮件 |
| 5 | 任务列表：它在做 / 我要做 / 已排期；只有你能标完成 | 收件箱只放需要你的事；只有 notify 自动关闭；系统页有全会话总览 | ◐ 缺"Planner 答应你的事"与已排期视图 |
| 6 | 承诺单独落成定时任务（Muse） | 附录 D 列为候选，正文未采纳 | ◐ |
| 7 | 成品库 | §17.1 ⑧ 列为工作台面第二批 | ◐ |
| 8 | 状态三层展示 | 原因码与计数、Planner 日志、原始记录 | ◐ 有内容，缺分层 |
| 9 | 频率一句话可调 | 打扰预算、推送三级、注意力偏好（§3.5） | ◐ 有结构，没写交互方式 |
| 10 | 安静但不被遗忘：未闭环提醒 | 离开摘要、"等你"计数 | ◐ 缺"托付后长时间没闭环"的提醒 |
| 11 | 速度：先回应、再干活 | 没有对 Planner 回应延迟的要求；Planner 默认 xhigh | ✗ |
| 12 | 审批卡片：动作 + 目的地 + 内容 + 全部收件人；卡片在模型之外 | 卡片带请求 ID、校验发送者本人、首个回答生效（§10）；来源权威只认网关元数据（D58） | ✅ |
| 13 | 永远不能设为"别再问"的动作 | 授权有能力 / 范围 / 次数 / 过期；需人决定清单 | ◐ 缺"不可长期授权"属性 |
| 14 | 无人在场的审批限时作废 | 闸门票据（D53），没写过期语义 | ◐ |
| 15 | 外发前展示目的地与收件人 | 团队群聊隔离；没有统一的外发确认 | ✗ |
| 16 | 独立授权方（Sentinel / 意图检查） | 网关判权 hook，出错即拒绝（D53） | ✅ 同构 |
| 17 | 凭证不进模型 | 不做派生凭证（D42）；会话能读 HOME | — 有意不同，剩余风险见 §7.5 |
| 18 | 没有隐形画像 | "懂你"文件化，只有你能改关注点、授权与边界 | ◐ 没明确禁止"看不见的推断" |
| 19 | 写作风格 | 无 | ✗ |
| 20 | 浏览器能看、能接管 | `computer-use.md` §4.5 实时画面（P4，可后做） | ◐ 接管没写成必须 |
| 21 | 桌面常驻 + 手机 | dashboard（桌面浏览器）+ 候选 iOS app | ✅ 两端都在 |
| 22 | 指挥本机编程会话 | **核心**：派活、通知、取代、停车、过程评审 | ✅ 领先第二代 |
| 23 | 评审执行者、纠偏 | 工作循环、介入阶梯、检查记录 | ✅ 第二代没有 |
| 24 | 覆盖所有会话的监控 | 监控底座（D45） | ✅ 第二代没有 |
| 25 | 内网与本机凭证 | 本机运行，可用 Midway 与本机凭证 | ✅ 第二代的云电脑进不了内网 |
| 26 | agent 自己的邮箱 / 号码 | 无 | — 公司环境不适用 |
| 27 | 多人 / 团队 agent | v1 只支持单 owner | — |

**位置判断**：naozhi 现在是第一代工具（替 CC 会话做路由）。Planner v6 实际上已经是**两代的合流**：它的"手"是第一代的执行会话，"脑"想做成第二代的主动 agent（懂你、预判、推送、早报）。这正是行业还没做成的位置，最接近的是 Dots + Codex（云端、不支持 Bedrock）和 Perplexity Personal Computer（本机 Mac 常驻）。

问题在于 v6 是用第一代的思路写的：监督机制很重（台账、闸门、转向锁、指令四条件、影子模式），而第二代真正被验证的产品原语（一段关系、早报、承诺、闭环、频率交互、成品库、速度）大多排在后面或缺失。第二代的安全也主要靠"审批卡片 + 独立检查 + 硬禁区"，而不是复杂的监督流程，这与 §17.1 ⑥ 的减法方向一致。

## 4. 建议加入 RFC 的条目

### A. 改核心模型或阶段（需 owner 拍板）

| # | 建议 | 依据 | 影响章节 |
|---|---|---|---|
| A1 | **一个 Planner 关系，工作区作为 channel**：你只和一个 Planner 有一条主线程；每个工作区是一个 channel，各有自己的"懂系统"、章程与例行任务；"懂你"只有一份。并行仍在执行会话层；Planner 内部按 channel 排队 | Duet / Muse / Dots 都是一个 agent + 共享记忆的分支；你的工作横跨 naozhi、客户架构、邮件、Asana，"懂你"本来就该只有一份；可以删掉多 Planner 协作（D19）这块复杂度 | §0、§3、§4.1、§6.3、D4、D19 |
| A2 | **主动性提前**：早报 + 外部来源巡检 + 预判，与 Planner v0 同期上线（P3），而不是等到 P5；首个主动场景从"naozhi 仓库"扩到"你的一天"（邮件、日历、Asana、CI、在途会话） | 第二代最受好评的是预判与早报；附录 B 里外部系统操作占你消息的 9% | §9、§15、D8 |
| A3 | **Planner 先回应、再干活**：你发给 Planner 的消息，网关在 1 秒内确定性回执（"收到，交给 X / 正在看"）；Planner 默认 effort 改为自适应（日常路由低，深度评审时调高） | 速度是第二代的分水岭；Anthropic Projects 的协调者默认就是 low effort | §4.1、§4.9、§10 |

### B. 加进现有章节（小改，符合做减法）

| # | 建议 | 依据 | 章节 |
|---|---|---|---|
| B1 | **承诺清单**：Planner 答应你的事落成单独的定时任务，有状态，直到你关闭；不受打扰预算挤压 | Muse；Dots 丢任务的教训 | §4.6、§10 |
| B2 | **未闭环提醒**：托付超过一定时长仍没结果的事项，自动进早报或离开摘要；Planner 可以建议"看起来完成了"，但只有你能关 | Duet 被遗忘、Dots 丢任务 | §10 |
| B3 | **早报规则**：只放需要你动作的事、未闭环事项和预判，不放资讯；连续几天没打开就自动缩短并问你要不要调 | "三点式早报"拉不回人 | §10 |
| B4 | **外部来源巡检**：邮件、日历、Asana 作为例行任务，工作时段每 2–3 小时一次；先用脚本做确定性预检，有新东西才唤醒模型 | Duet 的定时检查 + Kiro 的"没变化不唤醒" | §9、D31 |
| B5 | **频率一句话可调**：你说"少打扰我""晚上别推"，Planner 直接改写"懂你"里的注意力偏好并回显改了什么 | Duet / Muse | §3.5 |
| B6 | **状态三层**：卡片上一行状态 → 展开看进行中的事 → 原始工具调用记录 | Muse，评测者"立刻希望 Codex / Claude Code 也有" | §10 |
| B7 | **不可长期授权**：付款、删除、对外发信、发版这类动作，不能落成长期授权，每次都要卡片 | Duet | §7.2、需人决定清单 |
| B8 | **无人响应的卡片限时作废**：作废后记入收件箱，不默认放行 | Grok Bot、Claude Dispatch 都是约 10 分钟 | §5.6 |
| B9 | **外发确认卡片**：发往邮件、外部群聊、webhook 的内容，卡片上列出目的地、全部收件人与内容；**不自建分类器** | Duet / Muse / Dots 的共同模式 | §7 |
| B10 | **没有隐形画像**：Planner 对你的任何推断都写在你能看到、能删的"懂你"文件里；不维护看不见的档案 | Muse 的人物档案事件、Dots 不能删记忆 | §3.5 |
| B11 | **写作风格**：从你发过的邮件和消息提炼一份风格说明，起草时使用 | Duet 内置技能，评测好评 | §3.5 |
| B12 | **成品库提前**：工作台面首批就做"各会话的产出"列表（PR、HTML、报告、截图），点开即预览 | 第二代都有 Make / Library | §17.1 ⑧ |
| B13 | **浏览器必须能看、能接管**（写进 `computer-use.md`） | Duet 卡在人机验证 | computer-use §4.3 |

### C. 有意不同或不做（写进 §16 或 §7.5）

| # | 第二代做法 | 不做的理由 |
|---|---|---|
| C1 | 凭证不进模型（替身 token / 外部代理注入） | D42 已定不做派生凭证；本机场景下会话能读 HOME，要做就得整套隔离，代价与做减法冲突。作为有意差异写进 §7.5 |
| C2 | 自建外发信息 / 意图分类器 | 复用 CC auto mode 自带的分类器 + B9 卡片即可 |
| C3 | agent 自己的邮箱、电话号码 | 公司环境不适用 |
| C4 | 多人 / 团队 agent | v1 只支持单 owner |
| C5 | 购物、一次性虚拟卡 | 不在使用场景内 |
| C6 | 留存与增长设计（推送拉活） | 单人使用，衡量标准是替你省掉的操作，不是打开次数；不打点（D15） |

## 附录：来源

第二代：
- Claude Duet：https://claude.com/resources/articles/claude-duet （10-21）；安全设计 https://claude.com/resources/articles/claude-duet-safety-privacy-and-security （10-23）；帮助中心 https://support.claude.com/en/articles/17631495-get-started-with-claude-duet （10-24）；评测：Every（Dan Shipper）、Lenny's / How I AI（Claire Vo，10-21）、Peter Yang、Riley Brown（10-23）、uper.pl 汇总（10-24）、TechCrunch（10-22）、Tom's Guide（10-23）
- OpenAI Dots：https://openai.com/index/introducing-dots/ （09-29）；https://learn.chatgpt.com/docs/dots ；https://help.openai.com/en/articles/20001529 ；https://help.openai.com/en/articles/20001554 ；评测：The Verge（10-02）、Wired（10-07）、Fast Company（10-09）、PCMag（10-13）、Axios（10-20）、Every
- Meta Muse：https://research.meta.ai/blog/security-and-safety-for-ai-agents-our-approach-with-muse （09-08）；https://introducing.muse.ai/ ；https://muse.ai/business （09-29）；Meta 帮助中心记忆说明；Sensor Tower 数据（TechCrunch 10-20、PPC Land 10-21）；Q3 财报电话会（10-21）；TIME（10-06）；Vox（10-16）；GhostShares（Ars Technica 10-20、The Verge 10-22）
- 其他：Grok Bot https://x.ai/news/introducing-grok-bot （08-11）、https://docs.x.ai/grok-bot/approvals-security-and-privacy ；Hark https://hark.com/articles/introducing-hark-pro （10-06）；Instinct（TechCrunch 08-24、09-28）；Fo（Agent Tipsheet 10-20）；Copilot Autopilot https://blogs.microsoft.com/blog/2026/09/25/introducing-the-new-copilot-with-home-code-and-autopilot/ ；Gemini agent（Google Cloud 10-08）；Perplexity Personal Computer（05-07）
- 品类评论：Stratechery（09-28）、a16z（09-29）、OrcaRouter（10-22）、The Gradient（10-26）、Every 横评

第一代（10-26 同批调研）：Claude Code Projects https://code.claude.com/docs/en/claude-projects ；Bedrock 可用性 https://code.claude.com/docs/en/feature-availability ；Codex https://learn.chatgpt.com/docs/ ；Devin https://docs.devin.ai/release-notes/2026 ；Kiro Crew https://kiro.dev/docs/crew/ 、https://github.com/kirodotdev/KiroCrew ；Cursor https://cursor.com/blog/projects ；Copilot app、Antigravity、Conductor 见各自 changelog
