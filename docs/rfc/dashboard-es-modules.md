# RFC: static/*.js 原生 ES module 化（#2557 D3）

## 问题

6 个 `<script defer>` 共享一个全局作用域：跨文件依赖靠 dashboard.html 的
加载顺序注释；`typeof X === 'function'`（现值 dashboard 35 / cron 13 /
nz_util 1）是对"另一个文件是否已加载"的运行时猜测；`window.X =` 导出散在
5 个文件。D1（eslint+冻结）只能锁现状，无法让"未声明依赖即错误"成为语言
事实。JS 拼的 HTML 里 ~100 处 `on*="fn(…)"`（dashboard 47 / cron 53）要求
fn 全局可见，是 module 化的主要阻力。

## 前提与硬约束（评审补强）

- **#2539 已合入**且 contract.js 保持 `<script>` 列表首位（classic，全局
  NZ_CONTRACT 先于一切可用）。
- **时序事实**（HTML 规范）：无 async 的 `type="module"` 与 `defer`
  classic 共用同一个 "执行于解析完成后" 队列，按**标签文档序**执行——
  混合迁移阶段跨文件顺序是安全的，前提是：**迁移期间禁止调整
  `<script>` 标签顺序、禁止加 async**（写入 dashboard.html 注释 + 迁移
  checklist）。
- **PR-A 之前必须先修 js-deps-freeze.mjs**（阻断项）：现正则对
  `export function/const` 与 `Object.assign(window, {…})` 双失明，PR-A
  落地即产生假阴性的 baseline 收缩。先给 topLevelDecls 加可选
  `export\s+` 前缀、windowExports 识别 Object.assign 简写，并给 module
  文件加 `window\.` 桥引用计数（PR-E 归零指标）。
- **顶层快照禁令**：module 顶层禁止 `const x = window.y`（函数与基本类
  型都禁；基本类型如 selectedKey 整体重赋值 6+ 处，快照即静默脏数据）。
  只允许调用点解引用 `window.fn(…)`。配 eslint `no-restricted-syntax`
  规则（module 顶层 `= window.` 赋值即红）机械强制。
- **安全网如实评估**：mock-server 对 /ws 恒 404，Playwright 全套对
  dashboard 顶层真实启动序列（`wsm.connect()`、两个 setInterval、多个
  立即 REST 调用——**不是**"只有函数定义"）的 WS 分支是**零信号**。
  PR-A 之前给 mock-server 补最小 /ws 实现（auth 握手 + 一条
  session_state），并加一条"WS 建连发生且 onMessage 分发可达"的回归
  用例，作为每个迁移 PR 的门槛。

## 设计决策

### 1. 过渡桥而非一次到位

module 不能 import classic script 的顶层名，反之 classic script 也看不见
module 的顶层名——**分文件迁移的每一步都需要桥**。桥的形式统一为显式
`window.*`：

- 已迁 module 供未迁文件消费：module 末尾集中一段
  `Object.assign(window, { esc, escAttr, … })`（单处、可 grep、迁移完删）。
- 已迁 module 消费未迁文件：读 `window.fn(…)`（显式，代替裸名）。
- 两边都迁完的边：显式 `import { x } from './y.js'`，桥删除。

### 2. inline handler：`nz.actions` 单入口（#1980 的过渡形态）

100 处 `on*="fn(…)"` 若一次改 data-action 委托即吞并 #1980（5-8 人日再
+4-6）。过渡：handler 引用的函数注册进 `nz.actions`（nz_util 已有
`window.nz` 单入口），inline 串改 `onclick="nz.actions.fn(…)"`。
`window.X =` 归零的验收以「只剩 `window.nz` 单入口」达成；#1980 后续把
nz.actions 再清成 data-action。

### 3. 跨文件可变状态：`nz.state`

只收**跨文件**读写的名字（冻结矩阵实测：cron_view 写 dashboard 的
activeView / eventTimer / selectedKey，读 sessionsData / sending /
projectsData / turnState / navUserEls 等 ~10；agent_view 读 7），不动
dashboard 内部其余 ~90 个顶层 let。`nz.state` 为 getter/setter 对象
（`nz.state.selectedKey` 经 defineProperty 代理回 dashboard module 的私有
变量），语义与现状逐字节一致，写点可 grep。TDZ 名单（现仅 cronJobs）改经
cron module 的导出访问器。跨文件"函数是否存在"探测（typeof 守卫 48 处）
在显式 import 后成为无条件调用或 `nz.bus`（EventTarget）事件。

### 4. 迁移顺序与 PR 切分（每 PR Playwright 全绿）

1. **PR-A**：nz_util → module（导出 esc/escAttr/…；`window.nz` 与
   `window.esc` 等桥保留）；files_view、asset_browser →
   module（消费改 `import`+`window.` 桥；各自 window 导出改挂 nz）。
2. **PR-B**：agent_view → module（读 dashboard 的 7 个名走 `window.`
   桥；AgentView 挂 nz）。
3. **PR-C0**：dashboard.js（**仍 classic**，侵入式前置改动）：在文件内
   注册 `nz.state` 访问器（classic 顶层 `let` 不上 window，getter/setter
   只能由同作用域的闭包提供——`Object.defineProperty(nz.state,
   'selectedKey', {get(){return selectedKey}, set(v){selectedKey=v}})`），
   并补齐 cron 需读的 ~10 个名字的读访问器（sessionsData 等现无
   window 导出）。
   **PR-C1**：cron_view → module（写 dashboard 状态改 `nz.state`；
   dashboard→cron 的 19 处反向引用改 `window.` 桥或 nz.bus）。
4. **PR-D**：dashboard → module（最大；inline handler 全量
   `nz.actions.` 前缀化；跨文件桥收敛成 import；删加载顺序注释）。
5. **PR-E**：清桥——`window.esc` 等别名删除，`typeof` 守卫归零，
   D2 ratchet/D1 冻结基线随每步收紧。

### 5. 工具链适配

- eslint：module 文件 `sourceType: 'module'`；no-undef 在 module 下即真
  作用域检查（D1 的 per-file globals 白名单逐文件清空）。
- js-deps-freeze：module 化文件的"裸引用"天然归零，baseline 逐 PR 收缩；
  `window.` 桥单独统计（新增 bridge 计数，PR-E 后归零）。
- contract.js 保持 classic script（先于所有 module 执行，全局
  NZ_CONTRACT 不变）。
- CSP 不变（inline handler 仍在，#1980 处理）。

## 替代方案

- 一次性 6 文件全转：diff >20k 行，回归面不可 review；分 PR 每步
  Playwright 285 用例全绿兜底。
- 打包器：违反项目"不引转译/打包"原则。
- 先做 #1980 再 module 化：inline handler 归零后桥更薄，但 #1980 自身
  4-6 人日且同样受"fn 必须全局"牵制——nz.actions 是两者共同的中间态，
  先铺它两边都受益。

## 回滚窗口

每个 PR 的独立 revert 窗口只到**下一个 PR 合入为止**：PR-D 的 import
语句要求 cron_view 已是合法 module，此后回滚 PR-C1 必须连带回滚 PR-D。
上线节奏上每个 PR 单独发版并 soak（本项目每 epic 发版的节奏天然满足），
发现问题在下一 PR 前回滚。

## 风险

- import 图深化后，队列中靠前脚本的依赖 fetch 会阻塞其后所有脚本并推迟
  DOMContentLoaded（dashboard 顶层启动序列随之推迟）——无打包器时此代价
  永久存在；HTTP/2 + 6 个小文件的图深度 ≤2，预估可忽略，但 PR-D 附
  Playwright trace 的加载时间对比数据。
- 循环 import（dashboard↔cron 函数互调）：函数声明跨环 hoist 安全；
  let/const 跨环即 TDZ——现仅 cronJobs 一处，PR-C 改访问器消除。
- `window.` 桥期间 D1 冻结矩阵形态变化：每 PR 附 baseline diff 供 review。

## 验收

按 issue #2557 验收执行；「`window.X =` 归零或只剩 window.nz 单入口」按
后者达成（nz.actions / nz.state / nz.bus 皆挂 nz 之下）。
