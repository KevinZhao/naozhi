# Planner 对象模型示意图

对应 `docs/rfc/planner.md` v5 §3–§7。三张图：对象关系与基本行为、执行会话的生命周期、一个典型流程中各对象如何协作。**清晰版请看 `docs/design/planner-object-model.html`**（手工排版的静态 SVG，浏览器直接打开）；下面的 mermaid 是同一内容的源码，GitHub 会自动排版，对象多时布局可能较乱。

## 1. 对象关系与基本行为

- 实线箭头：主动动作（发出、派出、路由、记录）；虚线箭头：派生或依据（由什么得出、按什么判定）。
- 菱形：持有 / 包含（整体与部分）。
- 方框下半部分以 `+` 开头的是该对象的基本行为。

```mermaid
classDiagram
direction TB

class Owner["你"] {
  最终授权来源
  +发消息（代表你）()
  +拍板卡片：批准 驳回 选择 回复()
  +维护关注点 Now Next Not-now()
  +授予或撤销授权()
  +急停()
}

class Workspace["工作区"] {
  仓库 或 领域（邮箱、个人杂事）
}

class Gateway["网关（naozhi 本身，不用模型）"] {
  +路由消息：焦点会话 或 IM 话题()
  +记录事件流()
  +动作前判权：hook()
  +托管 L0 规则：重试 自动继续 心跳()
  +推送管线：紧急 有时效 日报()
  +执行急停()
}

class Planner["Planner（每个工作区一个）"] {
  模型：偏好表首个可用 · xhigh
  工具：只读 + naozhi MCP，无 Edit
  +被唤醒：消息 事件 闹钟 空闲拉活()
  +决定交给谁：通知已有 排后 取代 另开()
  +预判：在你开口前准备好()
  +过程评审：跑偏时提修改意见()
  +代办显而易见的下一步()
  +护栏：先停下再告诉你()
  +提议：等你批准的计划()
  +复盘：把结果写回先例()
  +合并处理系统性故障()
}

class Charter["章程与例行任务"] {
  范围 触发器 审批闸门 升级规则
  介入配置（按场景）
  +按事件或定时触发巡检()
}

class UserModel["懂你"] {
  偏好（想听 别提 何时 形式）
  决定与先例（带证据，可被取代）
  关注点 Now Next Not-now
  节奏与打扰预算
  边界：需人决定清单 约束
  +只有你能改关注点 授权 边界()
  +候选经验满足条件后升格()
}

class Grant["授权"] {
  能力 范围 次数 过期
  来源：你确认的卡片
  +被引用于判权()
  +撤销()
}

class SystemModel["懂系统"] {
  在途会话
  关系：冲突 依赖 重复
  环境健康：Midway 网络 CI 磁盘
  项目长期知识
  外部：issue PR release
  +由事件流确定性派生()
  +接受 Planner 批注()
  +过期检测：锚点改了就标未复核()
}

class Session["执行会话（中立）"] {
  key：只表示来源命名空间
  契约：目标 验收 自主度（可选）
  血缘：parent 派出者 prev 接续
  产物：PR 分支 报告
  托管开关
  污染标记
  生命周期状态
  +接收消息并跑一个回合()
  +调用工具（每次经 hook 判权）()
  +必要时启动 CC Workflow 扇出()
  +交回结构化交接 或 不可完成()
  +把大任务交给 Planner()
  +停车 复活 归档()
}

class Message["消息"] {
  from：直接发送方
  on_behalf_of：代表谁
  origin：根来源，只降不升
  kind：请求 结果 建议 指令 提问 提议 取消
  +投递：你的直通 CC 队列，Planner 的在回合边界()
  +决定这一轮按谁的权限执行()
}

class Event["事件"] {
  回合 工作流 进程死亡 权限拒绝
  状态变更（push merge 发版 云写）
  读入外部内容
  环境异常
  +唤醒 Planner()
  +给会话打污染标记()
}

class InboxItem["收件箱条目（视图）"] {
  类型：通知 提问 待审 决定 错误
  归属 Planner 与工作区、等待时长
  +按三级推送到手机或浏览器()
  +你处理后关闭（通知可自动关闭）()
}

class Helper["一次性助手（不是会话）"] {
  +停顿判读()
  +新消息归属提示()
  +离开摘要()
}

Owner "1" --> "N" Workspace : 拥有
Workspace "1" -- "1" Planner : 配一个
Planner "1" *-- "1" Charter : 遵循
Planner "1" *-- "1" UserModel : 持有
Planner "1" *-- "1" SystemModel : 持有
UserModel "1" *-- "N" Grant : 包含
Owner --> UserModel : 维护关注点 授权 边界
Owner --> Message : 发出
Planner --> Message : 发出 / 接收
Gateway --> Message : 路由
Message --> Session : 投递到
Planner "1" --> "N" Session : 派出 通知 取代 停车
Session --> Session : prev 接续
Gateway --> Session : 动作前判权
Grant ..> Gateway : 判权依据
Session "1" --> "N" Event : 产生
Gateway --> Event : 记录
Event ..> SystemModel : 派生
Event ..> Planner : 唤醒
Charter ..> Planner : 触发例行任务
Message ..> InboxItem : 需要你时投影
Event ..> InboxItem : 需要你时投影
InboxItem --> Owner : 推送
Gateway --> Helper : 调用
```

## 2. 执行会话的生命周期

进程、上下文、记录分层回收，寿命跟产物走（RFC §5.2）。

```mermaid
stateDiagram-v2
direction LR
[*] --> 运行中 : 新开 或 复活
运行中 --> 保温 : 回合结束
保温 --> 运行中 : 新消息（缓存有效期内）
保温 --> 等待中 : 绑定产物，等审阅 / CI / 你
保温 --> 休眠 : 产物仍开着但无订阅
运行中 --> 停车 : 被抢占 或 被取代（先推送半成品）
等待中 --> 运行中 : 审阅意见 / CI 失败 / 你回复（满足复活条件）
休眠 --> 运行中 : 追问（满足复活条件）
停车 --> 运行中 : 资源回来 / 恢复条件满足
等待中 --> 已归档 : 产物关闭 + 24 小时
休眠 --> 已归档 : 空闲超时
停车 --> 已归档 : 被取代且产物已处置
已归档 --> [*]

note right of 休眠
  复活条件：同一产物、上下文约 100k 以内、
  休眠不超过 72 小时、版本不变、无污染；
  不满足就另开新会话 + 交接
end note
```

## 3. 典型流程：你交一个大任务后离开

```mermaid
sequenceDiagram
autonumber
actor 你
participant 网关
participant P as Planner
participant S as 执行会话
participant 助手 as 一次性助手
participant 收件箱

你->>网关: 消息"处理完剩下的 review issue，每个一个 PR，我出去一趟"
网关->>P: 路由（大任务，代表你）
P->>P: 读懂你 / 懂系统，决定"另开"并写契约
P->>S: 派出（parent = Planner，托管开）
loop 每个回合
  S->>网关: 调用工具
  网关->>网关: hook 判权（代表你 · 授权 · 污染）
  S-->>网关: 事件（回合结束 / 工作流阶段 / 状态变更）
end
网关->>助手: 回合结束且托管中 → 停顿判读
助手-->>网关: E 类：做了一部分就停
网关->>S: 自动继续（带剩余验收项，计数 2/8）
S-->>网关: 事件：第一份 diff
网关->>P: 唤醒（关键节点）
P->>S: 建议"这里在改症状，根因在出队顺序"
S-->>P: 接受并修改
S-->>网关: 事件：PR 全部 CI 绿
网关->>P: 唤醒（结果）
P->>P: 预判：你过去合并后都发版 → 备好 changelog
P->>收件箱: 提议"发版 v0.1.46？"（需人决定清单）
收件箱->>你: 推送（紧急 · 可直接点按钮）
你->>收件箱: 批准（Face ID）
收件箱->>P: 你的决定
P->>P: 复盘：写回先例
```
