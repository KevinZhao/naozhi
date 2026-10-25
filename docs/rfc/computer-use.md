# RFC: 计算机使用 —— 让会话看到并操作这台 Mac

- 状态: Draft v1（方向已与 owner 确认，未实施）
- 日期: 2026-10-25
- 范围: 给 naozhi 的执行会话加上两项能力——**把桌面截图交给 CC 作为上下文**，以及**操作这台 Mac 上的浏览器和桌面应用**（含需要 Midway 认证的内网站点）。云端浏览器（AgentCore Browser）与自建的 Bedrock computer use 循环列为候选，未决定。
- 相关: `docs/rfc/planner.md`（PR #3685：执行会话、事件流、污染标记、需人决定清单、按会话挂载、工作台面）、`docs/rfc/cli-mcp-config.md`、`docs/rfc/attachment-refcount.md`、`docs/rfc/dashboard-csp-strict.md`

## 1. 背景

naozhi 现在能用的都是"有接口的系统"：API、命令行、MCP（Outlook、Asana、Sentral、报销、web 搜索、AWS CLI）。补不上的是另一半——**只有界面、没有接口的系统**：只能在浏览器里点的内网站点、只能用鼠标操作的 Mac 应用。还有一类需求是让 agent **亲眼看到**自己的产出（渲染后的 HTML 架构图、幻灯片排版、dashboard 改动）。

owner 使用数据（planner RFC 附录 B）：外部系统操作占全部消息的 9%（录入 Asana / SFDC、提交报销、上传 S3 等），其中一个录入会话就有 51 条消息；幻灯片修改反复出现（"去掉第 9 页"）。这些里没有接口的部分，目前只能 owner 自己点。

**访问方式分四层，计算机使用是最后两层**：

| 层 | 方式 | 特点 |
|---|---|---|
| 1 | API / MCP | 最快、最确定、最省；**有就用这一层** |
| 2 | 命令行 + Midway cookie | 只对有接口的内网服务有效 |
| 3 | 浏览器操作（本 RFC） | 任何能在浏览器里打开的站点，含单页应用与填表 |
| 4 | 桌面操作（本 RFC） | Mac 上的任何应用 |

第 3、4 层比前两层慢、贵、不够可靠（Opus 5.5 在 OSWorld 2.1 部分得分 81.8%，长流程完整完成率约 48%），用于补漏；固定流程最终应沉淀为脚本或 MCP。

## 2. 目标 / 非目标

目标

- C1 **桌面截图作为上下文**：会话能主动截取桌面、窗口或区域交给模型看；你也能在 dashboard / 手机上一键把当前屏幕发给某个会话。
- C2 **截图可见**：工具返回的截图进入事件流，在对话与预览面板里能看到 agent 每一步看到了什么。
- C3 **浏览器操作**：在本机专用浏览器配置文件里访问网页，包括你登录过 Midway 的内网站点；锁屏时也能跑。
- C4 **桌面操作**：操作 Mac 上的应用（PowerPoint、Keynote、Excel、只有桌面版的工具），远程时也能用。
- C5 **安全可控**：以你的身份操作内网时，有专用配置文件、域名 / 应用白名单、提交前确认、污染标记与急停。

非目标

- 不在 naozhi 主进程里实现鼠标键盘驱动（优先复用现成 MCP；naozhi 构建是 `CGO_ENABLED=0`）。
- 不复刻 Claude Code 内置的 computer-use / Claude in Chrome（它们要求 claude.ai 订阅登录与交互式会话，不支持 `-p` 与 Bedrock，§3.1）。
- 不关闭或绕过系统锁屏（§6.3）。
- 本期不接云端浏览器与 Windows 桌面（§8 候选）。

## 3. 可用能力（2026-10 调研）

### 3.1 Claude Code 内置能力：对 naozhi 不可用

- 内置 `computer-use` MCP：研究预览、仅 macOS、需要 Pro / Max 订阅、只能在交互式会话里用，文档明确"不支持 Bedrock 等第三方提供商"（https://code.claude.com/docs/en/computer-use）。
- Claude in Chrome（`--chrome`）：必须 claude.ai 登录，API key / setup-token 下强制关闭（https://code.claude.com/docs/en/chrome）。
- 本机 CLI `2.1.308.1314` 有 `--chrome` 开关，二进制内置原生模块 `computer-use-swift.node` / `computer-use-input.node`，但运行条件同上。naozhi 用 `-p --output-format stream-json` + Bedrock，两者都用不上。

### 3.2 MCP：naozhi 的接入方式

MCP 工具在模型看来是普通自定义工具，不受认证方式限制，Bedrock 下可用。

| 用途 | 候选 | 说明 |
|---|---|---|
| 浏览器 | Playwright MCP（`@playwright/mcp`）、Chrome DevTools MCP | 以无障碍树快照为主、截图为辅；可读控制台与网络请求；支持持久化配置文件与 CDP 连接已开的 Chrome |
| 桌面 | Peekaboo（openclaw）、computer-use-mcp、macos-computer-use-kit | 多数优先走 macOS 辅助功能树（AX），截图兜底；需要屏幕录制与辅助功能授权 |

本机现状：`~/.claude.json` 里配过 playwright（`npx @playwright/mcp --cdp-endpoint http://127.0.0.1:9223`），但 naozhi 使用自己的 settings 与 `~/.naozhi/mcp.json`，看不到它。

代价：用不上 Anthropic 官方的屏幕提示注入分类器（只对官方 computer 工具类型自动生效，自定义工具需填意向表）。

### 3.3 Bedrock 上的 computer use API（候选，见 §8.2）

- Opus / Sonnet 5.5 在 Bedrock 上接受 `computer_20251124`，beta 字段 `computer-use-2025-11-24`（InvokeModel 放请求体 `anthropic_beta`，Mantle 端点用 `anthropic-beta` 头）。
- 新一代工具集 `computer_toolset_20260801`（17 个成员工具、批量动作、无需 beta 头）是否在 Bedrock 可用：AWS 模型卡写支持，Anthropic 文档写"不在 Bedrock 提供"——**矛盾，需实测**（只能用 `awsCredentialExport` 那份凭证）。
- 截图约 1,000–1,800 token / 张；长边上限 2576 px；工具不替你缩放，Retina 设备像素比为 2，坐标要按截图像素换算。

## 4. 设计

### 4.1 总体

```
你（dashboard / 手机） ──"截屏发给会话"──┐
                                          ▼
执行会话（CC） ──MCP 工具──► 浏览器 MCP（专用配置文件）──► Chrome（本机）
              └─MCP 工具──► 桌面 MCP（AX 优先）     ──► Mac 应用
                     │ 工具结果里的图片
                     ▼
naozhi：图片提取 → 附件落盘 + 缩略图进事件流 → 对话 / 预览面板 / 灯箱
```

- 能力以 MCP 形式挂给执行会话；naozhi 只补三件事：**截图进事件流**、**你主动发送屏幕截图**、**按会话挂载与安全规则**。
- 截图默认留在执行会话自己的上下文里；Planner 只读结构化结论与缩略图引用，不把大量截图拉进自己的上下文。

### 4.2 桌面截图作为上下文（C1、C2）

**会话主动截图**：由桌面 MCP 提供 `screenshot(目标)`，目标可以是整个桌面、某个窗口、某个区域。默认截**窗口**而不是整桌面，避免把无关窗口（邮件、聊天、密码管理器）一起拍进去。截图在交给模型前缩放到长边 ≤1568 px（兼顾清晰度与 token），坐标换算由 MCP 负责。

**你主动发送**：dashboard 输入框旁加"截屏发送"按钮（手机 app 同样）。naozhi 在 Mac 上截取当前屏幕或你选的窗口，作为一张图片附件随你的消息发给焦点会话——直接走现有的图片上传路径（`POST /api/sessions/upload` → `SpawnOptions` 的图片 → `EventEntry.Images`），不新增管道。截取用系统自带的 `screencapture`（`-x` 静音，`-l <窗口ID>` 截窗口），由 naozhi 进程执行，所以授权归属 naozhi 本身（§6.1）。

**截图进事件流（必须补的缺口）**：评审核实，`internal/cli/process_event_format.go` 只处理 system / assistant / result 三类事件，**工具结果里的图片块被直接丢掉**，dashboard 看不到 agent 的截图。改动：

- 从 `user` 事件的 `tool_result` 内容中提取 image 块，挂到对应 `tool_use` 条目上。
- 原图写入附件目录（`<workspace>/.naozhi/attachments/…`，沿用 `internal/attachment` 的引用计数与 GC），事件里只存缩略图（复用 `clievent.ThumbMaxDim = 600`）与附件路径。
- 前端 `event_render.js` 已能渲染任意事件的 `images`，点开走灯箱，改动很小。
- 历史回放（`internal/discovery/history.go`）同步提取，保证重启后仍可见。

这一项不止服务计算机使用：任何返回图片的工具（读图、渲染、绘图）都会受益。

### 4.3 浏览器操作（C3）

- **专用配置文件**：为 agent 建一个独立的 Chrome 用户数据目录（例如 `~/.naozhi/browser-profile/`），**不碰你的主配置文件**。你手动在里面登录一次 Midway 及需要的内网站点，之后 agent 在这个登录态下工作；Midway 过期时健康状态机报"凭证将过期"（planner RFC §3.8），收件箱提示你重新登录。
- **运行方式**：Playwright MCP 以持久化配置文件启动（headless 或有窗口）。浏览器自动化走 CDP，**不需要屏幕录制与辅助功能授权，锁屏时也能跑**——这是远程场景最稳的一条。
- **域名白名单**：浏览器 MCP 只允许访问白名单里的源（Playwright MCP 的允许 / 拦截源参数），白名单放在 naozhi 配置里，内网域名按需添加。
- **以无障碍树为主**：页面操作优先用元素引用而不是像素坐标，截图作为辅助；更快、更省、更可靠。

### 4.4 桌面操作（C4）

- **桌面 MCP**：优先选以 macOS 辅助功能 API 操作的实现（AX 优先、截图兜底），能对后台窗口操作，减少抢鼠标、减少 token。候选在 P0 实测后定（§9）。
- **单一控制者**：同一台 Mac 的桌面同一时间只允许一个会话操作（机器级锁，参照 Claude Code 的做法），其他会话的桌面请求排队；浏览器操作不受此限（各自独立上下文）。
- **你在用电脑时让路**：每次动作前读取系统的输入空闲时间（`ioreg -c IOHIDSystem` 的 `HIDIdleTime`），你最近几秒有键鼠操作就暂停并提示，不和你抢鼠标；你按下约定快捷键（例如 Esc 长按）立即中止。
- **应用白名单**：只允许激活和操作白名单里的应用；终端、系统设置、钥匙串访问、密码管理器默认不在名单内。
- **必须醒着且解锁**：桌面操作要求 Mac 处于解锁状态（§6.3）；锁屏时桌面任务进入"等你"，浏览器任务照常。

### 4.5 实时画面（工作台面增强，可后做）

- dashboard 的 CSP 只允许同源（`connect-src 'self'`、`frame-src 'self' blob:`），嵌不了外部实时画面。做法是**服务端代理**：会话正在使用计算机工具时，naozhi 以低帧率（约每秒 1 帧）截取浏览器（CDP `Page.screencastFrame`）或目标窗口，经自己的 `/ws` 推送 JPEG 帧，预览面板用 `<img>` 刷新，CSP 不用改。
- 默认只显示"最近一张工具截图"，你点"实时"才开始推流；离开面板即停止。

## 5. 与 naozhi 的对接点

| 改动 | 位置 | 说明 |
|---|---|---|
| 工具结果图片进事件流 | `internal/cli/process_event_format.go`、`internal/cli/clievent/types.go`、`internal/attachment`、`internal/discovery/history.go`、`static/event_render.js` | §4.2 |
| 截屏发送 | `static/composer_files.js` + 新的 `POST /api/desktop/screenshot`（执行 `screencapture`，结果走现有上传路径） | §4.2 |
| MCP 挂载 | 先用 `~/.naozhi/mcp.json`（全局）；**按会话挂载**落地后（planner RFC §7.3：`--strict-mcp-config` + `SpawnOverlay`）只给"计算机"能力档的会话挂桌面 MCP | §6.4 |
| 桌面控制锁、让路、应用白名单 | 若所选桌面 MCP 不支持，用 PreToolUse hook 对 `mcp__desktop__*` 工具做检查（hook 出错即拒绝） | §4.4 |
| 实时画面 | 新增 `internal/server` 下的截屏推流 + `static` 预览面板 | §4.5，可后做 |

估算：截图进事件流 + 截屏发送约 600–900 行；按会话挂载随 planner RFC；实时画面约 500–800 行。

## 6. macOS 条件与权限

### 6.1 授权

- 本机 TCC 数据库（只读查询）显示：`~/.local/bin/naozhi` 已有**辅助功能**与**完全磁盘访问**授权，对 System Events、Chrome、PowerPoint 等有 AppleEvents 授权；**没有屏幕录制授权**；claude 二进制没有任何授权条目。
- 授权绑定的是 `naozhi-local` 签名证书（`identifier "com.naozhi.agent" and certificate leaf = …`）。坚持用同一张证书重签，升级后授权保留；ad-hoc 签名会让授权失效。
- 授权的"责任进程"：launchd 拉起的 naozhi 是责任进程，shim → claude → MCP 子进程继承这一归属（`Setsid` 不改变归属，代码里无 disclaim 调用）。所以只需给 naozhi 补一个**屏幕录制**授权（由你本人在系统设置里添加）。
- **待实测**：naozhi 重启后，继续存活的 shim 及其子进程，授权是否仍按 naozhi 计算；macOS 26 对屏幕录制有周期性再确认弹窗，远程时无人能点——需要观察频率并在健康状态里提示。

### 6.2 三种运行条件

| 能力 | 需要授权 | 锁屏时 | 睡眠时 |
|---|---|---|---|
| 浏览器操作（CDP） | 无 | **可以** | 不行 |
| 截图（窗口 / 桌面） | 屏幕录制 | 只能截到锁屏界面 | 不行 |
| 桌面操作 | 屏幕录制 + 辅助功能 | **不行** | 不行 |

### 6.3 锁屏

本机设置为显示器 60 分钟后熄屏且**立即锁屏**。桌面操作和有意义的截图都要求解锁。本 RFC **不建议关闭锁屏**（公司设备的安全策略通常要求自动锁屏）。可行做法：

- 远程任务优先走浏览器（锁屏可跑）。
- 桌面任务在锁屏时进入"等你"，推送提示"需要解锁 Mac 才能继续"；你回到电脑前或在电脑旁时执行。
- 正在执行桌面任务期间用 `caffeinate -d -w <pid>` 防止显示器熄屏导致中途锁屏。
- 虚拟显示器（本机装有 BetterDisplay）能否在不降低锁屏安全性的前提下支持后台操作，列为 P0 调研项。

## 7. 安全

计算机使用**以你的身份**操作：Midway 登录态代表你本人，浏览器和桌面上做的一切都记在你名下。规则沿用 planner RFC，补充以下几条：

- **专用配置文件 + 白名单**：浏览器只用专用配置文件、只访问白名单域名；桌面只操作白名单应用。
- **屏幕内容是外部内容**：截图与页面文字一律按外部内容处理，会话被打上污染标记（planner RFC §7.4）。按 Rule of Two：
  - 你本人发起的计算机任务可以读也可以写，但**最终提交一步需要你确认**；
  - Planner 或例行任务发起的计算机任务默认**只读**（查信息、截图、导出），不做提交类动作。
- **提交前确认**：填表、发送、下单、删除、接受条款这类有实际后果的动作，默认停在最后一步之前，给你一张带截图的卡片，你批准后才点（planner RFC 的闸门 = 拒绝 + 放行票据）。通用的"哪个点击是不可逆的"无法可靠自动识别，所以这一步依赖协作约定（会话在最终提交前必须调用 `request_review`）加白名单与审计兜底，并在 RFC 里如实标注这一局限。
- **截图隐私**：默认截窗口不截整桌面；截图原图只存在本机附件目录并随 GC 回收，默认不发到 IM 与 webhook；推送里只带缩略图引用。
- **停止**：planner RFC 的三级停止对计算机工具同样生效；另有本机物理中止（键鼠操作让路、约定快捷键中止）。
- **审计**：每个计算机动作（工具名、目标应用或域名、截图引用、代表谁）写入事件流。

## 8. 候选（未决定）

### 8.1 AgentCore Browser（云端浏览器）

- 能力：每会话一个托管 Chrome（microVM），CDP 自动化 + 2026-04 起的系统级鼠标、键盘与整桌面截图，实时画面（可接管）、录制、持久化登录态、URL 白名单、VPC 模式；约 $0.012 / 10 分钟会话；14 个区域。
- 局限：**进不了 Midway 与内网**；实时画面是外部源，受 CSP 限制需新标签页打开或服务端代理；输入只支持 ASCII。
- 什么时候值得接：需要大量并行的公网网页任务；需要和你的 Mac 完全隔离；或者终态 EC2 控制面上需要在 Mac 睡眠时继续做公网浏览器任务。届时做成一个独立的 MCP（例如 `cmd/naozhi-browser-mcp`），复用 `internal/agentcore` 的凭证链路（SDK `bedrockagentcore v1.50.1` 已含 `Start/Stop/GetBrowserSession`、`UpdateBrowserStream`、`InvokeBrowser`）。

### 8.2 自建 Bedrock computer use 循环

- naozhi 直连 Bedrock 的 computer use API，包装为 MCP 工具（例如 `gui_task(目标)`），让执行会话把 GUI 任务委派给一个子循环；截图留在子循环里，不撑大主会话。
- 好处：用的是官方 computer 工具类型（是否在 Bedrock 上也启用提示注入分类器待核实）。
- 什么时候值得接：要操作隔离的桌面沙箱（Docker + Xvfb、虚拟机），又没有合适的 MCP。

### 8.3 Windows 桌面

WorkSpaces for AI agents（2026-06 GA，托管 MCP 操作 Windows 桌面，可旁观与叫停）——只在需要 Windows 遗留软件时考虑。

不建议：Nova Act（另一套模型，仅 us-east-1）、Bedrock Agents 的 computer use 动作组（维护模式，只支持旧模型）、EC2 Mac 作操控目标（最少按 24 小时计费且同样进不了内网）。

## 9. 分阶段

| 阶段 | 内容 |
|---|---|
| P0 实测 | ① 给 naozhi 授予屏幕录制后，经 shim → claude → MCP 子进程能否截屏；naozhi 重启后存活的 shim 是否仍有授权；② 桌面 MCP 候选（Peekaboo / computer-use-mcp / macos-computer-use-kit）在 launchd 下的表现、是否支持后台窗口与 AX 操作；③ Playwright MCP 持久化配置文件下 Midway 登录态能维持多久、锁屏下 headless 与有窗口两种模式；④ stream-json 中工具结果图片的大小与延迟、每张截图的 token；⑤ Retina 坐标换算；⑥ Bedrock 上 `computer_toolset_20260801` 是否可用；⑦ 屏幕录制周期性再确认的频率 |
| P1 截图进上下文 | 工具结果图片进事件流（§4.2）；"截屏发送"按钮；只读的截图 MCP 工具（全局挂载即可，不涉及写操作） |
| P2 浏览器操作 | Playwright MCP + 专用配置文件 + 域名白名单；以一个真实的、没有接口的内网任务试点；提交前确认 |
| P3 桌面操作 | 桌面 MCP（AX 优先）；单一控制者锁、让路、应用白名单；**前置条件：按会话挂载落地**（只给"计算机"能力档的会话） |
| P4 实时画面 | 预览面板里的服务端截屏推流；Planner / 验证环节用截图做视觉检查 |
| 以后 | §8 候选按需 |

## 10. 待定问题

需要和 owner 讨论后再定：

1. **首个试点任务**：请 owner 提供 2–3 个"只能在网页或桌面上手动点"的真实任务（例如某个没有接口的内网录入页面、某个桌面应用里的固定操作），P2 / P3 以它们为验收场景。
2. **桌面 MCP 选型**：Peekaboo / computer-use-mcp / macos-computer-use-kit，P0 ② 实测后定；若都不满足"AX 优先 + 后台窗口 + 单一控制者 + 让路"，再考虑用 hook 补齐。
3. **虚拟显示器**：本机装有 BetterDisplay，能否在不降低锁屏安全性的前提下支持后台桌面操作，且符合公司设备策略。
4. **屏幕录制周期性再确认**：macOS 26 的再确认弹窗在远程场景下无人能点，频率多高、是否需要在健康状态里提前提醒。
5. **截屏发送的默认对象**：发给焦点会话，还是让你每次选择；截整个桌面还是默认截当前最前面的窗口。
6. **截图预算**：每张截图约 1,000–1,800 token，长任务会累积；默认分辨率（长边 1568 px）与每个会话保留的截图数量是否需要上限。
7. **实时画面是否需要**：只看"最近一张工具截图"是否已足够，还是需要 P4 的推流。
8. **AgentCore Browser 是否接入、何时接入**（§8.1 的条件）。
9. **与 planner RFC 工作台面的关系**：截图、实时画面、文件预览是否统一在工作台面的预览面板里（planner RFC 待定项）。

## 11. 决策记录

| # | 日期 | 决策 | 理由 |
|---|---|---|---|
| CU1 | 10-25 | 桌面截图作为 CC 上下文、操作 Mac（浏览器 + 桌面应用）是确定要做的能力 | owner 确认；补上"只有界面、没有接口"的系统 |
| CU2 | 10-25 | 以 MCP 形式挂给执行会话，不复刻 Claude Code 内置 computer-use，不在 naozhi 主进程里实现键鼠驱动 | 内置能力不支持 Bedrock 与 `-p`；做减法、复用现成实现 |
| CU3 | 10-25 | 先补"工具结果图片进事件流"与"截屏发送" | 改动小、收益大，且对所有返回图片的工具有用 |
| CU4 | 10-25 | 浏览器用专用配置文件 + 域名白名单，优先走无障碍树；锁屏可跑 | 不碰主配置；以你的身份操作内网需要边界 |
| CU5 | 10-25 | 桌面操作 AX 优先、单一控制者、你在用电脑时让路、应用白名单；不关闭锁屏 | 避免抢鼠标与合规问题 |
| CU6 | 10-25 | 屏幕内容按外部内容处理；你发起的任务最终提交前确认，Planner / 例行任务发起的默认只读 | Rule of Two；与 planner RFC 的污染与闸门规则一致 |
| CU7 | 10-25 | 默认截窗口不截整桌面；原图只存本机并随 GC 回收 | 隐私 |
| CU8 | 10-25 | AgentCore Browser、自建 Bedrock 循环、Windows 桌面列为候选，满足 §8 条件再接 | owner 不确定是否需要；进不了内网 |

## 附录：参考

- Claude Code computer use：https://code.claude.com/docs/en/computer-use ；Claude in Chrome：https://code.claude.com/docs/en/chrome
- Claude computer use 工具：https://platform.claude.com/docs/en/agents-and-tools/tool-use/computer-use-tool ；Bedrock computer use：https://docs.aws.amazon.com/bedrock/latest/userguide/computer-use.html
- 计算机与浏览器使用安全建议：https://claude.com/resources/articles/best-practices-for-computer-and-browser-use-with-claude （2026-05-13）
- 参考实现：computer-use-demo 与 computer-use-best-practices（github.com/anthropics/claude-quickstarts）
- Playwright MCP：https://github.com/microsoft/playwright-mcp ；Chrome DevTools MCP：https://github.com/ChromeDevTools/chrome-devtools-mcp ；Peekaboo：https://github.com/openclaw/Peekaboo
- AgentCore Browser：https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/browser-resource-session-management.html ；系统级操作公告（2026-04-08）
- WorkSpaces for AI agents（2026-06 GA）
- OSWorld 成绩：Opus 5.5 公告（2026-09-22）、Sonnet 5.5 公告（2026-09-28）
