# naozhi iOS app 设计（草案）

- 状态: 草案，待 owner 决定是否做原生 app
- 日期: 2026-10-19（2026-10-26 按 RFC v7 更新）
- 依据: `docs/rfc/planner.md` v7（核心模型、全局 Planner、信任阶梯、三个面、承诺与早报、推送）
- 草图: `docs/design/ios-app-mockup.html`（7 屏：锁屏、对话、收件箱、会话详情、工作台、系统、我）

## 1. 为什么考虑原生 app

推送是 Planner 能"替你盯着"的出口：托管、预判、系统性故障、需人决定，最终都要在你不看屏幕时找到你。当前 dashboard 只注册了 service worker，没有 Notification / Web Push，IM 也没接（`platforms: {}`）。

| 方案 | 推送 | 通知上的操作 | 锁屏进度 | 前提 | 结论 |
|---|---|---|---|---|---|
| PWA + Web Push（现有网页） | 有（iOS 16.4+，需添加到主屏） | **没有操作按钮**（iOS 不支持 Notification `actions`，只能点开再操作） | 无 | HTTPS（可用 `tailscale cert`）；无需开发者账号 | 可作过渡，体验上限低 |
| 飞书 / Slack bot | 可靠 | 卡片按钮 | 无 | 依赖企业 IM 与其政策 | 与工作 IM 混在一起 |
| **原生 iOS app** | APNs | **可操作通知**（批准 / 拒绝 / 选项 / 文字回复，敏感操作要求解锁） | **Live Activity + 灵动岛**、锁屏小组件 | **Apple Developer Program（付费）**：免费 Personal Team 不支持 Push Notifications 能力，且描述文件 7 天过期 | 推荐 |

原生 app 的三个关键收益，正好对应 RFC 的三个核心场景：

1. **需人决定** → 可操作通知：锁屏上直接"发版（Face ID）/ 稍后"，不用打开任何东西。
2. **托管长任务** → Live Activity：锁屏和灵动岛实时显示"正在跑测试 · 第 3/5 项验收 · 18/24 PR"，替代你反复问"进展如何"（附录 B：D 类 77% 是纯状态查询）。
3. **注意力分级** → iOS 的通知中断级别（time-sensitive / active / passive）、按 session 分组、专注模式过滤，与 RFC 的打扰预算一一对应。

## 2. 范围：移动伴侣，不是网页的复刻

原生 app 只做手机上真正需要的事：**说话、拍板、看进度、看改动、管边界**。工作台面在手机上只做"这个会话改了什么 + diff"与成品库；文件树、直接运行、轻量编辑、专家操作（斜杠命令、新建会话对话框、tuning、cron 编辑、完整 transcript 深挖）留在网页，app 里用内置浏览器打开同一页面。这样两端不需要功能对齐，维护成本可控。

## 3. 信息架构（五个标签 + 锁屏，对应三个面）

| 位置 | 承载 RFC 概念 | 内容 |
|---|---|---|
| 锁屏 / 灵动岛 | 推送管线、托管、早报 | 一个"总览" Live Activity（"正在替你盯着 N 件事"，第一行是会话的一行状态）；三类推送：需要你决定、守护型提醒、早报；灵动岛显示在跑 / 等你计数 |
| ① 对话（对话面） | 你 ↔ 执行会话；Planner 在旁边 | 一个输入框，常显"→ 发往 X ｜ 交给 Planner ｜ 新话题"；执行会话以卡片出现（一行状态 + "已按建议执行，可撤回"）；Planner 旁注附依据、不插进会话；交给 Planner 时一秒回执并登记承诺；语音输入（含"倒脑子"）、附件 |
| ② 收件箱（监督面） | 需要你处理的事 | 顶部早报 / 离开摘要；分组：等你处理 / 待审 / 答应你的 / 已解决；卡片带有效期，过期作废 |
| ③ 工作台（工作台面） | 你直接看、直接动 | 这个会话改了什么 + diff；成品库；"交给会话" |
| ④ 系统（监督面） | 懂系统（按工作区分区） | 环境健康（全局）、各工作区在途与例行任务、冲突、Planner 状态与信任级别、检查记录（可点"这不对"） |
| ⑤ 我 | 懂你（全局一份） | Now / Next / Not-now、注意力（一句话设置的回显与撤销）、写作风格、信任阶梯、授权（含"每次确认"类）、Planner 对你的推断、三级停止 |
| 会话详情 | 执行会话的契约 / 血缘 / 产物 / 托管 | 状态三层（一行状态 → 进行中的事与里程碑 → 原始记录）、托管与按建议执行开关、意见与依据、直接对它说话 |

交互要点：

- **发往谁**：默认焦点会话（Planner 在旁边）；点"交给 Planner"或说"交给你"时 Planner 站到中间；焦点会话正在运行而新消息不像短跟进时，先提示"像是新话题，要另开吗？"；左滑卡片可"设为焦点"。
- **一句话设置**：频率、静默时段、偏好、例行任务、信任级别都在对话里说，Planner 回显改了什么并给撤销入口；不做设置页。
- **打断**：在 session 详情里说话直通 CC 的 command queue；"停下"是显式按钮，立即生效。
- **为什么**：任何自动动作、顾问意见、Planner 的决定都能点开看依据（事件或先例），对应 RFC §4.4 与 Muse 的教训"解释自身行为只引用记录"。
- **敏感操作**：批准"每次确认"类动作（发版、对外发信、删除远程资源）、修改"我"里的边界与授权，要求 Face ID。

## 4. 推送设计

### 4.1 三级 → iOS 中断级别

| RFC 级别 | 例 | iOS | 行为 |
|---|---|---|---|
| 紧急 | 需人决定、真 blocker、叫停、系统性故障 | `time-sensitive`（突破专注模式与摘要） | 立即推送，带操作按钮；**不受打扰预算限制**（RFC D57） |
| 有时效 | 里程碑、PR 待审、托管任务完成 | `active` | 立即推送；你正在 app 内看对应 session 时不推 |
| 早报 | 其余 | `passive` + 每日早报一条 | 不响铃，进通知中心；早报由网关确定性生成，在你设定的时刻推一次，没有内容就不发 |

- 不申请 critical alert（需要 Apple 特批，且不必要）。
- 每条推送只讲一件事，附"为什么推"（Muse 的做法）；`thread-id` = session，同一 session 的通知自动归组。
- 专注模式过滤（`SetFocusFilterIntent`）：例如"睡眠"专注下只放行紧急。

### 4.2 可操作通知（类别）

| 类别 | 按钮 | 说明 |
|---|---|---|
| `DECISION` | 推荐项 / 其他选项 / 查看 | 选项来自卡片；不可逆的选项 `authenticationRequired` |
| `APPROVAL` | 批准（需解锁）/ 拒绝 / 查看 | 需人决定清单内的动作；卡片有有效期，过期作废，从不默认放行 |
| `QUESTION` | 回复（文字输入）/ 查看 | 回复作为你的消息发给对应 session |
| `PROGRESS` | 暂停托管 / 打开 | 里程碑与完成 |
| `SYSTEM` | 我去处理 / 1 小时后提醒 | Midway 过期、地区 400 等守护型提醒 |
| `COMMITMENT` | 好的 / 推迟 / 关闭 | Planner 登记的承诺到点；承诺不受打扰预算挤压，只有你能关闭 |
| `BRIEF` | 打开早报 | 每日早报 |

按钮回调调用 naozhi 的卡片 API（卡片带请求 ID、首个回答生效、应用前重读待决项，见 RFC §10）。

### 4.3 Live Activity

- **只开一个"总览"活动**，而不是每个 session 一个：iOS 对同时进行的活动有数量限制，多块也会把锁屏塞满。总览显示最重要的 1 件 + 其余计数；点击进入收件箱。
- 托管开始且你离开时，服务端用 **push-to-start**（iOS 17.2+）启动；里程碑用低优先级更新（不限次数），需要你时用高优先级（有系统预算）；全部结束时 `end` 并留下结果摘要。
- 活动上的按钮用 App Intents：暂停托管、打开。
- 需要开启 `NSSupportsLiveActivitiesFrequentUpdates` 以提高高优先级预算。

### 4.4 其他系统入口

- 锁屏 / 主屏小组件："等你的事"计数与第一项。
- 控制中心控件（iOS 18+）：三级停止（暂停推送 / 暂停托管 / 急停）。
- Siri / 快捷指令：`告诉 naozhi …` 作为一条消息发出。
- Apple Watch：通知与按钮自动镜像，可在手表上拍板。

## 5. 架构

```
iPhone app（SwiftUI）
  ├─ 主 app：五个标签、会话详情、内置浏览器打开网页 dashboard
  ├─ Notification Service Extension：收到推送后经 Tailscale 拉取详情并渲染（推送载荷只带最少信息）
  ├─ Widget / Live Activity 扩展
  └─ App Intents（三级停止、发消息、语音倒脑子）
        │  REST + WebSocket（现有 /api、/ws 协议）           ▲ APNs
        ▼                                                      │
naozhi（单机阶段在 Mac；终态在 EC2 控制面）──────────────────┘
  ├─ 设备注册：POST /api/devices {apns_token, push_to_start_token, activity_tokens}
  ├─ 推送管线：从收件箱条目生成三级推送，执行打扰预算 / 静默时段 / 专注规则
  └─ internal/apns：直连 APNs（HTTP/2，token 认证 .p8，ES256 JWT），支持 alert / liveactivity 两种 push-type
```

- **不用第三方推送服务**：naozhi 直接向 APNs 发送，只需要出站访问 `api.push.apple.com`。`.p8` 密钥进 RFC §7.3 的凭证清单，只给网关读。
- **协议复用**：app 与网页用同一套 REST / WS。WS 帧与 REST 响应的 Go 类型（`internal/wsproto` 等）是单一真相源，可以从它们生成 Swift `Codable` 类型。
- **推送载荷最小化**：APNs 载荷经过 Apple，只放 ID 与一句已脱敏的标题；详情由 Notification Service Extension 拉取。拉不到时显示通用文案。
- **认证**：网页 dashboard 上显示一次性配对二维码（服务器地址 + token），app 扫码后把 token 存进钥匙串；"我"里可撤销设备。
- **连通性**：
  - 单机阶段：Mac 与 iPhone 同在一个 Tailscale tailnet，用 MagicDNS + `tailscale cert` 获得 HTTPS；不暴露公网。推送不依赖连通性（naozhi → APNs 是出站），但通知上的操作需要能连到 naozhi——Tailscale 按需 VPN 可以在操作时自动连上；连不上时操作在 app 内排队重试。
  - 终态：EC2 控制面（现有 CloudFront → ALB → EC2 路径），手机直接连 EC2，Mac 作为执行节点（RFC §11）。

## 6. 安全

- 设备 token 与配对 token 都可在"我"里撤销；token 只存钥匙串。
- 需人决定清单内的批准、对"懂你"的修改要求 Face ID；通知按钮用 `authenticationRequired`。
- 推送内容过 RFC §7.6 的统一脱敏器；不在载荷里放账号 ID、内网域名、凭证片段。
- 急停在 app、控制中心、Live Activity 三处可达。

## 7. 分阶段

| 阶段 | 内容 | 前提 |
|---|---|---|
| A 推送打通 | `internal/apns`、设备注册、推送管线最小版（紧急 + 有时效两级）、app 只有收件箱 + 可操作通知 + 配对 | Apple Developer Program；完整 Xcode（本机目前只有 Command Line Tools）；Tailscale |
| B 托管可见 | 总览 Live Activity（push-to-start / 更新 / 结束）、会话详情（状态三层）、早报、三级停止控件 | RFC P1 监控底座与 P2 托管 |
| C 对话与工作台 | 对话标签、发往 X / 交给 Planner、语音、附件、新话题；工作台标签（改动 + diff、成品库） | RFC P3 的路由与轨道 W |
| D 懂你 / 懂系统 | 系统与我两个标签、承诺、信任阶梯、Face ID 修改、专注过滤、小组件 | RFC P3 的两份知识 |

阶段 A 与 RFC 的 P1 监控底座可以并行：推送管线是两者共用的服务端部分。

**可选过渡**：在 app 上线前，先给现有网页加 Web Push（不需要开发者账号，只能点开再操作），一两周即可让"里程碑推送"先用起来。

## 8. 待决定

1. 是否做原生 app（需付费开发者账号与完整 Xcode）。
2. 单机阶段的连通方式：Tailscale（推荐）还是其他隧道。
3. 是否先做网页 Web Push 过渡。
4. 分发方式：TestFlight 内部测试（构建 90 天过期需重传）还是 Xcode 直装（付费描述文件一年）。

## 参考

- Apple：Supported capabilities（Personal Team 不含 Push Notifications）、Developer account overview（Personal Team 描述文件 7 天过期）
- Apple：Sending web push notifications in web apps and browsers；WebKit：Web Push for Web Apps on iOS and iPadOS；Home Screen web app 能力表（Notification `actions` 不支持）
- Apple：Starting and updating Live Activities with ActivityKit push notifications；WWDC23 "Update Live Activities with push notifications"（liveactivity push-type 仅支持 token 认证、低优先级不限次、高优先级有预算、frequent updates）
- Meta Muse 推送管线与"解释只引用记录"（RFC 附录 D）
