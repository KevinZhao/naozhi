# RFC: AskUserQuestion 交互卡片

状态：Proposal (2026-05-10)
作者：Kevin

## 背景

Claude Code (CC) 内置一个 `AskUserQuestion` 工具，在交互式 TUI 模式下会弹出选择框让用户回答。
naozhi 运行 `claude -p` headless 模式时：
- 原假设：CC 会发 tool_use → **阻塞**等外部系统回 tool_result。设计思路是 naozhi 拦截 + 回注 tool_result。
- **实测否定**：CC 在 `-p` 下**不阻塞**，立刻自注入 `is_error:true` 的 tool_result 把问题给拒了，然后发一段兜底文本并正常 end_turn。

因此"拦截 tool_use 阻塞上游"这条路**物理上不存在**。本 RFC 给出经实测验证的替代方案。

## 验证报告

脚本位于 `test/e2e/askuser/aq{1..7}_*.py`；原始 log 在 `test/e2e/askuser/out/`。

### AQ1 / AQ2 — 触发与 schema

**AQ1**：用明确指令（含 "use the AskUserQuestion tool"）+ 不确定性场景可稳定触发。

**AQ2**：`tool_use.input` 精确 schema：

```json
{
  "questions": [
    {
      "question": "Which error handling approach do you want to use?",
      "header": "Error style",
      "multiSelect": false,
      "options": [
        {"label": "Return an error",
         "description": "Propagate the error to the caller..."},
        {"label": "Log and continue", "description": "..."},
        {"label": "Panic",            "description": "..."}
      ]
    },
    {
      "question": "Which function do you want to add error handling to?",
      "header": "Target",
      "multiSelect": false,
      "options": [
        {"label": "I'll paste the code",   "description": "..."},
        {"label": "Point me to a file",    "description": "..."}
      ]
    }
  ]
}
```

**结论**：字段与系统 prompt 里的 tool schema 一致：`questions[]` 每项含 `question/header/multiSelect/options[{label,description}]`。

### AQ3 — 生命周期（最关键）

```
t+ 8.97s assistant  tool_use    AskUserQuestion(id=toolu_...)
t+ 8.97s user       tool_result is_error:true content:"Answer questions?" tool_use_id=toolu_...
t+12.08s assistant  thinking    "I've asked you two questions..."
t+12.72s assistant  text        "I've asked you two questions..."
t+12.77s result     end_turn    num_turns=2   ✓ 整轮结束
```

- AskUserQuestion 发出后 **~3ms** 就有 `is_error:true tool_result` 注入（CC CLI 内部兜底，不经过外部 stdin）
- 整轮在 12 秒内**正常结束**（`result` 事件发出、`stop_reason=end_turn`）
- 说明 CC 把整个事件链视为完整一轮，后续等用户在**新 user turn**里回答

**推翻的原设计假设**：阻塞等 tool_result、需要 WriteToolResult 接口、需要 pendingQuestion 表、需要 TTL sweeper、需要避开 sendSlot — **全部不需要**。

### AQ4 — 用户答案作为普通 user 消息

第二轮发送：

```
Error style: Return an error.
Target: I'll paste the code — here it is:
```go
func LoadConfig(path string) Config { ... }
```
```

CC turn 2 thinking：
> "The user wants to modify `LoadConfig` to return an error instead of just a `Config`. Let me find this..."

**结论**：CC **完整接上上下文**，没有重问，直接走 Grep 找函数。用户答案只要把 header + 选项 label 拼成普通文本即可，**不需要特殊标记**。模型从对话历史自行关联到之前的 AskUserQuestion。

### AQ5 — 多个问题合并

即便 prompt 明确要求"分 3 次独立调用"，模型仍倾向**把相关问题合并进一次** `AskUserQuestion.questions[]`（实测 1 次调用含多个 question）。

**对实现的影响**：一张卡片渲染多个问题，UI 要支持 1-N 个 question 分组，每个 question 下一组 options。

### AQ6 — `--disallowedTools AskUserQuestion` 可关

添加 `--disallowedTools AskUserQuestion` 后：
- system/init 的 tools 列表里不再包含 AskUserQuestion
- 模型主动降级："There's no `AskUserQuestion` tool available to me — but I don't need one, since I can just ask you directly: Which error handling approach do you prefer? 1. panic ... 2. return error ..."

**结论**：per-session 开关可行。适合给偏好简洁文本回复的用户/渠道。

### AQ7 — 与其他 tool 混用 auto-error 隔离

单一 assistant message 同时包含 `Read` + `AskUserQuestion`：

```
t+2.95s USE    Read             id=i8Ms...
t+4.40s USE    AskUserQuestion  id=C1oe...
t+4.41s RESULT ERR              tuid=C1oe... "Answer questions?"      ← 只针对 AQ
t+4.41s RESULT                  tuid=i8Ms... "1\tip-10-...compute..." ← Read 正常
```

**结论**：auto-error 严格按 `tool_use_id` 定位，与其它工具完全隔离。过滤时**按 tool_use_id 匹配**安全。

## 新设计（大幅简化）

### 数据流

```
CC assistant tool_use{AskUserQuestion, input:{questions}}
              ↓
  naozhi cli event.go 拆出 Event.AskQuestion 字段（观察事件，不拦截不响应）
              ↓
  CC 自己注入 is_error:true tool_result → naozhi 过滤掉此条 user 事件
              ↓
  CC 继续发 assistant text "I've asked you..." → naozhi 默认过滤（或配置为展示）
              ↓
  CC result event → 本轮结束
              ↓
  naozhi dispatch 层向 dashboard + 源 channel 广播 ask_question 卡片（带 tool_use_id 作 correlation id）
              ↓
  用户点选 → dashboard / 飞书 card action → naozhi 构造普通 user 消息文本
              ↓
  复用现有 sendWithBroadcast 路径，作为下一轮 user turn 发送
              ↓
  CC 接上上下文继续生成
```

### 组件变更清单

**internal/cli/event.go**
- Event 增加：
  ```go
  AskQuestion *AskQuestion `json:"ask_question,omitempty"`
  ```
- 新类型 `AskQuestion`、`AskQuestionItem`、`AskQuestionOption` 对应 AQ2 确认的 schema。

**internal/cli/protocol_claude.go (ReadEvent)**
- 识别 assistant content block 为 `tool_use.name == "AskUserQuestion"` 时：
  - 正常发 assistant 事件（保持既有 tool_use 链路）
  - **额外合成一条** `Event{Type:"ask_question", AskQuestion:{ToolUseID, Items}}` push 给 eventCh
- 识别 user content block 为 `tool_result` 且 `is_error:true` 且 `content:"Answer questions?"` 且 `tool_use_id` 命中已知 AskUserQuestion id 时：
  - **丢弃此 user 事件**（done=false, ev.Type=""），不污染对话历史
  - 需要维护一个进程级 short-term 集合 `seenAskIDs map[string]struct{}`（上限 64，LRU 或 TTL 60s 清理）；仅用于识别 auto-error，不用于状态
- 识别 assistant text 紧跟 AQ 的兜底段落（"I've asked you..." 类）默认**保留显示**（AQ3 表明这段本身对用户无害，模型清楚表达 "等你回答"；过度过滤反而让用户看不到任何反馈）

**internal/dispatch**
- `onEvent` 增加 `ev.Type == "ask_question"` 分支：
  - 调 `platform.SendQuestionCard` (dashboard 通过 WS，飞书通过交互卡片)
  - 卡片 payload 包含 `tool_use_id` 作为 correlation
- 不需要 pending 表、不需要 TTL sweeper

**internal/server/dashboard.go + static/dashboard.js**
- WS 广播新消息类型 `ask_question`：`{key, tool_use_id, questions}`
- JS 侧在会话视图内渲染**行内卡片**（而非抽屉）：每个 question 一组按钮/checkbox
- 点按钮后直接**构造文本**，走现有 `POST /api/session/send` 路径，不需要新 endpoint
- 文本格式：`"<header1>: <selected-label1>. <header2>: <selected-label2>."` — AQ4 已验证这种朴素格式 CC 能完整衔接

**internal/platform/feishu/feishu.go**
- 新增 `SendQuestionCard(ctx, chatID, q AskQuestion) (msgID string, err error)`
- 用 Feishu Card Schema 2.0 的 `action_module` + `button` 元素，每个 option 一个 button
  - button.value.kind = "ask_answer"
  - button.value.header = question.header
  - button.value.label = option.label
  - button.value.key = session_key（用于路由回应到对的 session）
- 飞书 card action webhook 回调 → 构造文本 → 走**当前 IM 消息入口**（`platform.MessageHandler`）正常 dispatch
  - 即：card action 本质上**等价于用户发了一条消息**
  - 群聊鉴权：`operator_id` 必须匹配 session 当前 owner（与普通消息同一鉴权逻辑）
- card edit 收尾：回答后把卡片 `EditMessage` 成 "✅ Error style: Return an error; Target: I'll paste the code"，防止其他群成员重复点

### 不再需要

- ~~WriteToolResultLocked 协议扩展~~
- ~~pendingQuestion 表 + answered CAS~~
- ~~TTL sweeper + 超时注入 tool_result~~
- ~~新 HTTP endpoint `/api/session/answer_ask`~~
- ~~sendSlot 归属特殊处理~~
- ~~重启时 flush pending~~

所有"回答"都等同于一条普通 user 消息，复用 queue / passthrough / interrupt / 鉴权 / 持久化全套现有机制。

### 边界条件

| 场景 | 处理 |
|---|---|
| 用户同时从 dashboard 和飞书答 | 两端各产生一条新 user 消息；queue 合并为一轮或接连两轮都可接受（重复答案语义一致）；卡片 edit 用 `tool_use_id` 去重 —— 第一条答案到达时 edit 卡片为"已回答"，第二次用户看到的就已经是 disabled 态 |
| 用户不答 | 没有硬超时；用户可以选择继续发别的消息，CC 会在新 user turn 里基于现有上下文继续（它知道之前问了问题，会合理处理）；卡片留在界面上"候着"，用户可以随时点 |
| session /new 重置 | 卡片自然失效（后端会忽略陈旧 tool_use_id 对应的回答 —— 因为新 session 历史里根本没有这个 AskUserQuestion）；可选前端收起所有过期卡片 |
| 进程重启 | AQ3 的行为保证 CC 不会在 resume 后"记得"要再问一次（因为前一轮已经 end_turn）；原卡片语义上依然有效，用户点击生成的 user 消息会让新 session 按"这是一条选择"处理 |
| multiSelect=true | dashboard 用 checkbox + 提交按钮；飞书第一期退化为单选（Schema 2.0 支持 multi_select_static，二期支持） |

### 工作量估计

单 PR 可完成，约 3 个文件块：

1. **CLI 事件层** (internal/cli/event.go + protocol_claude.go + 1 个单元测试文件)  
   ~150 行 + ~100 行测试。
2. **Dashboard** (internal/server/dashboard.go + static/dashboard.js + static/dashboard.css)  
   ~80 行后端 + ~120 行前端。
3. **Feishu** (internal/platform/feishu/feishu.go + card_action 处理分支)  
   ~150 行 + mock server 测试。

预期 2 个工作日内落地。

## 验证检查清单（实施后）

- [ ] 触发一次 AskUserQuestion，dashboard 显示卡片，auto-error 被过滤不出现在 transcript
- [ ] 点选 dashboard 卡片 → 新 user 消息发送 → CC 正确衔接
- [ ] 飞书发送 + 点选 → 同上
- [ ] 群聊非 session owner 点按钮 → 被拒 + toast 提示
- [ ] multiSelect=true 卡片 → 勾选多项 + 提交
- [ ] `disallowedTools` 配置开关验证
- [ ] `/new` 期间旧卡片回答不影响新 session

## 参考

- 实测脚本：`test/e2e/askuser/aq{1..7}_*.py`
- 原始事件 log：`test/e2e/askuser/out/aq{1,3,4,5,6,7}_full.log`
- CC 版本：2.1.132（Bedrock 后端 claude-opus-4-6）
