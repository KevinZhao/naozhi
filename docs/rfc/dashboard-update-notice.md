# RFC: dashboard 新版本发现与一键生效

- 状态：Draft v2（v1 的主线场景判断错误，见 §1.2；本稿据实测重写）
- 日期：2026-09-01
- 作者：Kevin Zhao
- 范围：dashboard 侧栏 header 出现版本提示，点击后确认 → 让新版本**生效**
  （按状态分叉：下载+替换+重启，或仅重启）。复用既有 `internal/selfupdate`
  全链路，不新增下载/校验逻辑
- 前置：`selfupdate-signing.md`（签名链现状与待办，本 RFC 不动）

## 1. Background & problem

`internal/selfupdate` 的**后端能力已经完整**：`LatestRelease` → `Download`
（SHA-256 校验 + 可选 checksums.txt pin）→ `Replace`（备份 + O_EXCL staging +
原子 rename + 强制 chmod 0755 + 失败回滚）→ `RestartService`。这套流水线同时
被 `naozhi upgrade` CLI 和后台 `selfupdate.Checker` 使用，已经历过 v0.0.27
升级事故的加固。

缺的只是**操作者视角的入口**。dashboard 只在设置的「关于」区显示
`stats.version_tag`（当前版本），没有任何"最新版本是多少 / 该不该动"的信息。
结果是升级动作被迫退回 SSH。

### 1.1 实测事实（2026-09-01，本机 macOS 26.6，全默认 update 配置）

本机 `config-local.yaml` **无 `update:` 段**，因此跑的就是默认值：
`enabled: true` / `mode: download` / `interval: 6h` / `check_on_start: false`。
下表逐条手工验证：

| # | 场景 | 结果 |
|---|---|---|
| F1 | 运行中 binary 能否自替换 | **可以，且已被真实验证过**。`~/.local/bin/naozhi` 属 `zhaokm:staff`、目录可写；后台 Checker 于 19:33 完成了一次真实 `Replace`，`.bak`（`-rw-------`，正是 `copyFileBackup` 的 0600 语义）就躺在旁边 |
| F2 | `LatestRelease` 走的是哪个 GitHub 端点 | **HTML redirect**（`releases/latest` → `releases/tag/vX.Y.Z`，只读 `resp.Request.URL`，不读 body），**不是** `api.github.com` |
| F3 | `api.github.com` 匿名限额 | 实测 `x-ratelimit-limit: 60`（每 IP 每小时）。F2 意味着现状不吃这个限额，但仍不能让每个 dashboard 客户端各自打一次 GitHub → 服务端缓存（§3.1） |
| F4 | 本机 launchd label 是否匹配代码常量 | **不匹配**。`selfupdate.LaunchdLabel = "com.naozhi.naozhi"`，实际是 `com.naozhi.agent`（`launchctl list com.naozhi.naozhi` → `Could not find service`）→ `ServiceRunning()` 返回 false → **重启被静默跳过，无 WARN 无报错** |
| F5 | launchd 是否会把真实 label 告诉进程自己 | **会，且是权威值**。`ps eww 1552` 显示 launchd 注入了 `XPC_SERVICE_NAME=com.naozhi.agent`，与实际 label 逐字相等。这让 F4 的修复变成一行（§3.6a） |
| F6 | `restartLaunchd` 在自重启场景是否可用 | **不可用**。它 `launchctl unload` 后再 `load`，而 unload 摘掉的正是发起调用的这个 job；`load` 至多在自己被杀的竞态里执行，语义上不成立。（时序未逐帧实测，但 §3.6b 的 `kickstart` 方案无论时序如何都严格更优，故不再细究） |
| F7 | `launchctl kickstart -k` 是否可用 | **可用**（`Usage: launchctl kickstart [-k] [-p] <service-target>`，uid 503 → `gui/503/<label>`）。由 launchd 发起 SIGTERM 并在退出后重新 spawn，job 始终留在 domain 里 |
| F8 | **默认 mode 下的稳定态是什么** | **`staged`，不是 `available`**。见 §1.2 —— 这条推翻了 v1 的主线设计 |
| F9 | systemd 侧自重启路径 | **健全**。`Restart=always` + `systemctl restart --no-block`；`RestartServiceNoWait` 的注释已详述自重启时为何不能 poll `is-active` |
| F10 | 活跃 CLI 会话是否会被 naozhi 重启杀掉 | **不会**（既有机制，本 RFC 不重新验证）。`internal/shim` sidecar setsid 脱离 + systemd `KillMode=process`，重启后经 `Manager.Discover`/`Reconnect` 重新接上 |
| F11 | 写端点的 CSRF 门 | **自动覆盖**。`internal/dashboard/auth/csrf.go` 对非安全方法做 Origin 校验 |
| F12 | 前端确认框基础设施 | **已有且契合**。`confirmDialog({title,message,detail,confirmText,variant,countdownSecs})`（`dashboard.js:8781`），`countdownSecs` 速度带正适合"重启服务" |
| F13 | `XPC_SERVICE_NAME` 是否会被非 launchd 场景误继承 | **会，且单读 env 有"重启错服务"的风险**。该变量是继承的：从一个 launchd 管理的父进程（Terminal.app 本身就是个 job）手工启动 naozhi，读到的是**父 job 的 label**。此时 `launchctl list com.apple.Terminal` 照样成功 → 旧的"存在即视为我们的服务"判据不成立 → `kickstart -k` 会重启操作者的终端。修法见 §3.6a 的 `verifiedLaunchdLabel`（实测本机交互 shell 无该变量，但这不构成安全前提） |

### 1.2 关键纠正：稳定态是 `staged`，用户要的是"生效"而非"安装"

v1 通篇假设操作者会看到"发现新版本，可以装"。**默认配置下这个状态几乎不存在。**
本机日志（`~/.naozhi/logs/stdout.log`）：

```
2026-09-01T19:33:07  auto-update: newer release found  current=v0.0.72 latest=v0.0.73 mode=download
2026-09-01T19:33:10  auto-update: binary installed     tag=v0.0.73 path=/Users/zhaokm/.local/bin/naozhi restart=false
```

而三小时后的进程状态：

```
PID 1552  STARTED Fri Aug 28 15:15  /Users/zhaokm/.local/bin/naozhi --config config-local.yaml
~/.local/bin/naozhi        v0.0.73   mtime Sep 1 19:33
~/.local/bin/naozhi.naozhi-upgrade.bak  (0600, Sep 1 19:33)  ← v0.0.72
```

**磁盘上是 v0.0.73，进程里跑的是 v0.0.72，没有任何人知道该重启。**
且这不是偶发——同一台机器上一轮同样如此：8/20 23:10 装好 v0.0.72，
进程直到 8/21 21:59 重启才生效，**空转了 22 小时**。

因为 `mode: download` 是默认值，后台 Checker 一发现新版本就在秒级内装完
（19:33:07 发现 → 19:33:10 装好，3 秒）。所以 `available` 只是一个 3 秒的
过渡态，**操作者打开 dashboard 时看到的稳定态永远是 `staged`**。

三个直接后果：

1. **最有价值的按钮是「立即重启生效」，不是「立即安装」。** 用户诉求
   "发现新版本 → 点击即安装"在默认配置下的正确语义是"发现新版本已就绪 →
   点击即生效"。
2. **v1 的 apply 设计会摧毁回滚能力。** 见 §1.3。
3. `update_available` 不能只比 `latest` vs `current`——那在 staged 态下为真，
   会把操作者引向"再装一遍"。状态机必须区分"待下载"与"待重启"（§3.1）。

### 1.3 v1 漏掉的数据损坏风险：重复 apply 会摧毁 `.bak`

`Replace` 的备份是**当前磁盘上 binary** 的拷贝，不是"当前运行版本"的拷贝。
在 staged 态（磁盘 v0.0.73 / 运行 v0.0.72）下再触发一次安装：

```
copyFileBackup(installPath, backupPath)   // 把 v0.0.73 拷成 .bak，O_TRUNC 覆盖
                                          // → 原本存的 v0.0.72 被销毁
```

回滚 artifact 从"可退回 v0.0.72"变成"退回到自己"，**唯一的逃生舱被静默清空**。
而这恰好是 v1 设计下最容易发生的操作：chip 显示"有新版本"→ 操作者点"立即安装"。

因此：**staged 态下的 apply 必须跳过 download/Replace，只做重启。**

### 1.4 `Checker` 的并发与短路缺口

- `Checker` 现在是单 goroutine 无锁的（`c.installed` 是裸字段，包注释明确
  "safe to run as a single goroutine"）。引入 dashboard 触发后就是两个写者
  → **必须加单飞锁**。
- `checkOnce` 有 `if rel.Tag == c.installed { return }` 的重复安装短路，
  但 **`doInstall` 自身没有**。若 dashboard 直接调 `doInstall`，这道保护
  就被绕过了——正是 §1.3 的触发路径。

## 2. Goals & non-goals

### Goals

- G1 dashboard 常驻可见版本状态提示，区分"待下载"与"**待重启生效**"。
- G2 一次点击 + 一次确认 → 让新版本生效。按状态分叉：`available` 走
  下载+替换+重启，`staged` **只重启**。
- G3 过程与结果可见：进行中 / 已生效 / 失败原因。
- G4 修 F4/F6，让 macOS 的重启真正发生。
- G5 不可操作时（无写权限 / dev build / 不支持的平台 / 重启不可用）UI 明确
  说明并给出手工命令，而不是显示一个注定失败的按钮。
- G6 界面尽量简单：一个 chip + 一个 `confirmDialog`，不新建视图、不新建页面。
- G7 **绝不摧毁回滚 artifact**（§1.3）。

### Non-goals

- NG1 不做版本选择 / 降级。只装 `releases/latest`；`semverGreater` 的防降级
  语义（`checker.go` R20260602141221-SEC-1）继续生效。
- NG2 不做多节点批量升级。**语义明确为：本功能只作用于 dashboard 直连的那个
  进程**（primary）。reverse node 的版本各自管理；UI 文案须写明"本节点"，
  避免在多 node 部署里被误解为全量升级（§3.4）。
- NG3 不升级 CLI（claude / kiro / codex）。
- NG4 不动签名机制。沿用 SHA-256 + 可选 `NAOZHI_UPGRADE_PIN_SHA256`。
- NG5 不做 WebSocket 进度推送（P3 可选）。P2 用低频轮询。
- NG6 不改 `update.mode` 的既有语义。dashboard 是 mode 之外的手工通道。

## 3. 设计

### 3.1 `selfupdate.Status`：区分"待下载"与"待重启"

后台 `Checker` 每 6h 已经在查 latest。把结果落到进程内共享对象，dashboard 只读：

```go
// internal/selfupdate/status.go
type Phase string

const (
    PhaseIdle       Phase = "idle"        // 已是最新，或尚未 check
    PhaseAvailable  Phase = "available"   // 发现更新，磁盘未替换（默认 mode 下仅存续数秒）
    PhaseInstalling Phase = "installing"  // 下载/校验/替换进行中
    PhaseStaged     Phase = "staged"      // 磁盘已是新版本，等重启生效 ←默认 mode 的稳定态
    PhaseRestarting Phase = "restarting"  // 重启已触发，本进程即将死
    PhaseFailed     Phase = "failed"      // 最近一次尝试失败，LastErr 有值
)

type Status struct {
    mu        sync.Mutex
    current   string    // 编译期 version，不变（= 进程真正在跑的版本）
    latest    string    // 最近一次成功 check 的 tag
    checkedAt time.Time
    checkErr  string
    phase     Phase
    staged    string    // 已落盘但未生效的 tag（PhaseStaged 时非空）
    lastErr   string
}

func NewStatus(current string) *Status
func (s *Status) Snapshot() StatusSnapshot
func (s *Status) noteCheck(latest string, err error)
func (s *Status) notePhase(p Phase, staged string, err error)
```

`Status` 为 nil 时全部退化为 no-op，这样 `Checker` 在没有 dashboard 的场景
（以及现有测试）里行为完全不变——现有 `checker_test.go` 一字不改即通过，
这是这条设计的天然回归门。

**`staged` 的跨重启语义**：`staged` 只存活在内存里，无需持久化。因为重启后
新进程的编译期 `version` 就等于原 `staged`，`current == latest` → `phase` 自然
回到 `idle`。若进程在 staged 后 crash 被拉起，拉起的已是新 binary，同样自洽。

**装配**：`main.go` 在 L472 构造 `srv`（`server.NewWithOptions`），L704 才
`startUpdateChecker`。`NewStatus(version)` 需提前到 L472 之前，同一个指针分别
交给 `server.ServerOptions.UpdateStatus` 和 `startUpdateChecker`。

### 3.2 `Checker.InstallLatest`：单飞 + 短路 + staged 只重启

**不复制**安装逻辑，走同一条 `doInstall`：

```go
// ErrInstallInProgress / ErrNothingToDo 供 handler 映射 HTTP 状态。
//
// 语义分叉（§1.3 是这段存在的唯一理由）：
//   - 已 staged（rel.Tag == c.installed）→ 跳过 download/Replace，只重启。
//     绝不第二次 Replace，否则 .bak 里的可回滚版本会被 O_TRUNC 销毁。
//   - 未 staged → 完整 doInstall(ctx, rel, restart)。
func (c *Checker) InstallLatest(ctx context.Context, restart bool) error
```

- `Checker` 新增 `installMu sync.Mutex`；`checkOnce` 的安装段与 `InstallLatest`
  都在它下面跑，`c.installed` 的读写一并纳入保护。
- `InstallLatest` 用 **TryLock** 语义：已有安装在跑时立刻返回
  `ErrInstallInProgress`（handler → 409），不排队堆积。
- `doInstall` 目前把失败吞成 WARN + notify（对后台 tick 是对的）。改为**返回
  error**：`checkOnce` 侧保留原 log+notify 行为，`InstallLatest` 侧把 error
  交给 HTTP 层。两个调用者错误策略不同，流水线只有一份。

### 3.3 API

```
GET  /api/system/update          读状态（可 60s 轮询）
POST /api/system/update/apply    触发，202 + 后台执行
```

放在 `internal/server/dashboard_update.go`，与 `dashboard_system.go` 同构。
两个 handler 太小，不值得开 `internal/dashboard/update/` 子包。

**GET 响应形状**：

```json
{
  "current": "v0.0.72",
  "latest": "v0.0.73",
  "staged": "v0.0.73",
  "phase": "staged",
  "action": "restart",
  "checked_at": "2026-09-01T19:33:07+08:00",
  "check_error": "",
  "last_error": "",
  "can_apply": true,
  "blocked_reason": "",
  "restart_supported": true,
  "running_sessions": 3,
  "enabled": true,
  "install_enabled": true,
  "manual_command": "sudo systemctl restart naozhi",
  "rollback_hint": "cp /usr/local/bin/naozhi.naozhi-upgrade.bak /usr/local/bin/naozhi && chmod 0755 /usr/local/bin/naozhi && sudo systemctl restart naozhi"
}
```

- **`action`** 是给前端的唯一判断依据，服务端算好：
  `"none"`（无事可做）/ `"install"`（需下载+替换+重启）/ `"restart"`（已 staged，
  只需重启）。**前端不做 semver 比较、不自己推状态机**，避免前后端两套语义漂移。
- `can_apply` / `blocked_reason` 是预检结果（§3.5），UI 靠它决定渲染按钮还是
  渲染手工命令。
- `restart_supported` = `ServiceManagesThisProcess()`（受管服务跑的**是本进程**，
  见 §5 F2；不是 `ServiceRunning()`）。false 且 `action == "restart"` 时无事可做，
  UI 给手工命令。
- `running_sessions` 让确认框能如实说明影响面（§3.4）。
- `enabled` = 有 Checker（`update.enabled`）；`install_enabled` = 允许 POST apply
  （`update.dashboard_install`）。
- `manual_command` 由服务端按自身 GOOS + 真实 launchd label 生成（§5 F3），前端
  不可 apply 时原样打印；空表示没有可贴的命令。
- `rollback_hint` 仅在 `action != none` 时给出（§5 P2 设计决定）。

**POST 语义**：

- Body：`{"confirm_action": "install"|"restart"}`。**要求客户端回传它看到的
  action**，服务端不一致时返回 409——防止"UI 显示 restart、点击时后台刚好
  装完变成别的状态"这类 TOCTOU 误操作。
- 立刻返回 `202 {"status":"started"}`，实际工作在后台 goroutine。**必须异步**：
  重启时进程会在响应写出之前被杀，同步 handler 的响应根本发不出去。
- 后台 ctx 用 `context.WithTimeout(context.Background(), 10*time.Minute)`，
  **不能**用 `r.Context()`——请求在 202 后即结束，会立刻取消下载。
- 409：已在安装中 / `can_apply == false` / 无事可做 / `confirm_action` 不匹配。
- 前端不等 POST 结果，改为轮询 GET（进行中提到 3s）。`phase` 走到 `restarting`
  后连接断开，前端进入"重连中"，重连后读到 `current` 已更新即为成功。

### 3.4 前端：一个 chip + 一个 `confirmDialog`

落点是侧栏 header 的 `.hdr-btns`（`dashboard.html:2958`），在 `btn-history`
之前插一个默认隐藏的按钮：

```html
<button type="button" class="hdr-btn hdr-btn-update" id="btn-update" hidden
        title="" aria-label="">
  <svg viewBox="0 0 24 24" aria-hidden="true" focusable="false">
    <path d="M12 19V5"/><path d="M5 12l7-7 7 7"/>
  </svg>
  <span class="hdr-update-tag" id="update-tag"></span>
</button>
```

**文案按 `action` 分叉**（这是 §1.2 在 UI 上的落点）：

| `action` | chip | `confirmDialog` |
|---|---|---|
| `none` | 隐藏 | — |
| `install` | `↑ v0.0.73`，title「有新版本可安装」 | 「下载并安装 v0.0.73，随后重启本节点服务」 |
| `restart` | `↻ v0.0.73`，title「v0.0.73 已就绪，重启生效」 | 「v0.0.73 已下载校验完成，**重启本节点服务即可生效**（无需再次下载）」 |

- 用 `confirmDialog`（F12）：`variant: 'danger'`、`countdownSecs: 3`（速度带，
  防误触），`detail` 里写明影响面：当前 `running_sessions` 个会话正在运行、
  CLI 子进程由 shim 保持存活因此对话不会中断（F10）、以及**"本节点"**字样
  （NG2，多 node 部署下避免误解为全量升级）。
- 进行中：按钮 disabled + spinner，文案跟 `phase` 走。
- `can_apply == false`：不渲染确认按钮，改为显示 `blocked_reason` + 可复制的
  手工命令。
- 失败：chip 变警示态，`confirmDialog` 的 detail 显示 `last_error`，按钮变「重试」。
- 描边用既有 `--nz-accent`，**不引入新颜色 token**（UI token 纪律）。
- 事件绑定走 `addEventListener`（项目惯例），不用 inline handler。

**轮询**：独立低频 timer，60s 一次 GET。不挂到 `fetchSessions` 上——那是秒级
循环，且其 `stats.version` 短路会把无关字段的变化吃掉。进行中临时提到 3s。

### 3.5 可操作性预检

`GET` 时按序返回第一个命中的阻塞原因：

1. `current == "dev" || current == ""` → `"dev build：本地构建不会被自动替换，用 naozhi upgrade --force"`（与 `checkOnce` 的 dev 跳过一致）。
2. `checkPlatform()` 失败 → `"当前平台没有 release 资产"`。
3. `action == "restart"` 且 `!restart_supported` → `"未检测到受管服务，手动重启进程即可生效"`。
4. `action == "install"` 且 install 目录不可写 → `"<dir> 不可写（当前 uid <n>）：用 sudo naozhi upgrade"`。
   探测：对 `filepath.Dir(SelfPath())` 做 `os.CreateTemp` + 立即 `Remove`，与
   `Replace` 真正要做的操作同构（比 `unix.Access` 更准，覆盖只读挂载 / SIP / ACL）。
   **pattern 必须不同于 `Replace` 的 `.naozhi-upgrade-*.staging`**，否则预检的
   临时文件会与真实安装的 staging 撞名/被误清理。结果缓存 60s。

预检是**UI 提示，不是门禁**：它天然有 TOCTOU（权限可能在预检与 apply 之间变化），
真正的错误处理仍在 apply 路径的 `Replace` 返回值上。`can_apply == false` 时
POST 仍返回 409 只是快速失败，不代表 `true` 就保证成功。

### 3.6 macOS 重启修复（G4）

两处改动，都在 `internal/selfupdate/service.go`：

**(a) label 从 `XPC_SERVICE_NAME` 读取。** F5 实测：launchd 给它启动的进程注入
`XPC_SERVICE_NAME=<label>`，与实际 label 逐字相等。这是 launchd 自己给的权威值，
无需解析 `launchctl list`、无需"label 含 naozhi"这类脆弱启发式：

```go
// LaunchdLabel 常量保留为回退值（非 launchd 启动时该 env 为空，例如从
// 终端直接跑 naozhi upgrade —— 那种场景下沿用常量即今天的行为）。
func launchdServiceLabel() string {
    if l := strings.TrimSpace(os.Getenv("XPC_SERVICE_NAME")); l != "" {
        return l
    }
    return LaunchdLabel
}
```

但**光读 env 不够**（F13）：该变量是继承的，从 Terminal 之类的 launchd 子进程
手工启动 naozhi 会读到父 job 的 label，而 `launchctl list <那个 label>` 照样
成功——于是 `kickstart -k` 会重启操作者的终端。所以取到 label 后必须**确认这个
job 跑的就是本 binary**：

```go
// verifiedLaunchdLabel 返回确认在管理本进程的 label，否则 ""。
// 一次 `launchctl list <label>`（单个已知 label，不是扫描），解析 "Program"
// 或 "ProgramArguments"[0]，与 SelfPath() 比（两侧都 EvalSymlinks）。
// 取不到可执行路径就 fail closed —— 少重启一次可恢复，重启错服务不可恢复。
func verifiedLaunchdLabel() string
```

`ServiceRunning()` 与 `restartLaunchd()` **都**走 `verifiedLaunchdLabel()`：

- 只改 `restartLaunchd` 没用，`ServiceRunning()` 是它的前置门，常量留在那里
  就仍然静默跳过（F4 只修一半）；
- 只读 env 不校验，则引入 F13 的误重启。

> v1 曾设计一个 `DetectLaunchdLabel()`：遍历 `launchctl list`、对含 "naozhi"
> 的 label 逐个再查 `Program` 并与 `SelfPath()` 比路径。作为**发现**手段已废弃
> （F5 让它成为过度工程，且启发式在 label 不含 "naozhi" 时失效）；但其中的
> **路径比对**作为**校验**手段被保留下来——成本从"扫描全部 job"降到"查一个
> 已知 label"，而它挡住的正是 F13。

**(b) `restartLaunchd` 改用 `kickstart -k`：**

```go
func restartLaunchd() error {
    if !ServiceRunning() { return nil }
    target := fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdServiceLabel())
    out, err := exec.Command(resolveTrustedBin("launchctl"), "kickstart", "-k", target).CombinedOutput()
    if err != nil {
        return fmt.Errorf("launchctl kickstart -k %s: %w\n%s", target, err, out)
    }
    return nil
}
```

由 launchd 发起 SIGTERM 并在进程退出后重新 spawn，job 始终留在 domain 里——
这正是 `RestartServiceNoWait` 的 fire-and-forget 语义在 macOS 上的正确实现，
与 systemd `restart --no-block` 对称。

`gui/<uid>` domain：naozhi 装的是 LaunchAgent（`~/Library/LaunchAgents`）。
若将来支持 LaunchDaemon 需 `system/`，届时按 plist 位置分支。

**这两项顺带修好了 CLI `naozhi upgrade` 与后台 `Mode: auto` 在 macOS 上的重启**
——它们走同一个 `restartLaunchd`。§1.2 里"装好了但空转 22 小时"的历史正是这个
缺陷的直接产物。

### 3.7 冷启动窗口：on-demand check 提到 P1

默认 `check_on_start: false` + `interval: 6h` 意味着**naozhi 重启后最长 6 小时内
`Status.latest` 为空**，chip 不显示——功能在最需要它的时刻（刚重启完、正想
确认版本）恰好是空白的。v1 把 on-demand check 列为 P3，这是个错误的优先级。

P1 就实现，代价很小：

```go
// CheckNow 供 GET handler 在 latest 为空且距上次 check 超过 minCheckInterval
// 时触发。全局单飞（复用 installMu 之外的 checkMu），结果写入同一个 Status。
// minCheckInterval = 15m —— 即使 100 个 dashboard 客户端同时轮询，对 GitHub
// 也只有每 15 分钟一次请求。
func (c *Checker) CheckNow(ctx context.Context) error
```

触发条件严格限定为「`latest == ""` 且 `time.Since(checkedAt) > 15m`」，
所以稳定运行期间它一次都不会触发，只填冷启动窗口。这同时顺手解决了
`update.enabled: false` 下"想看版本但 checker 不跑"的场景——不过那种部署下
是否该主动联网属于策略问题，保守起见 `enabled: false` 时不触发。

### 3.8 配置开关

```yaml
update:
  # dashboard 上是否提供"一键生效"按钮。false 时 GET 仍返回版本信息
  # （提示照常显示），但 POST apply 返回 403。默认 true。
  dashboard_install: true
```

提示（只读）与生效（改 binary + 重启）的风险量级不同，应能分开关。不复用
`update.enabled`——"我不要后台自动装，但想在 dashboard 上手工点"是合理组合。

## 4. 安全分析

**新增攻击面：几乎没有，但要说清为什么。** 持有 dashboard token 者能让 naozhi
下载并执行来自 GitHub 的新 binary——听起来像 RCE，但：

- 默认配置下**后台 Checker 已经在自动做同一件事**（§1.2 的日志就是证据），
  只是时机由定时器决定。dashboard 只是把时机交给操作者。
- 能装的**只有 `releases/latest`**（NG1），且须 `semverGreater` 通过。攻击者
  无法指定任意 URL / tag / 版本，也无法降级。
- 信任锚不变：SHA-256 对 `checksums.txt`，可选 `NAOZHI_UPGRADE_PIN_SHA256`
  强 pin，`NAOZHI_UPGRADE_REQUIRE_PIN` 严格模式。dashboard 路径不绕过任何一层。
- 真正新增的是「**立即重启**」这个能力：可被用来制造服务中断。缓解：
  `confirmDialog` 的 `countdownSecs` 速度带、`dashboard_install: false` 可整体
  关闭、以及下面的限流。

**限流的实际作用要说准。** `installMu` 的 TryLock 已经天然保证了单飞，所以
限流**不是**为了防并发安装，而是防止"反复点击 → 反复触发失败路径 → 刷日志 /
刷 GitHub 请求"。另外 dashboard token 是单一共享值，per-token 限流实际等于
**全局**限流——对这个端点恰好是想要的语义（安装本来就是全局单例操作），
但必须在代码注释里写明，避免后人误以为是 per-user。实现走
`internal/ratelimit`（`Allow(key)`），key 用固定串而非 token 本身（避免把
secret 放进限流器的 key 空间）。

**其他**：

- CSRF：F11，`auth` 中间件的 Origin 门自动覆盖 POST。
- **错误脱敏**：`last_error` / `check_error` / `blocked_reason` 会进 JSON 到
  浏览器，可能含 URL、install 路径、临时目录。走 `textutil` 的脱敏原语过一遍。
- Body 上限：`http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)` +
  `decodeJSONBody`（`DisallowUnknownFields`），与 `handleClearLabelOrigin` 一致。
- `writeJSON` / `writeOK`：走既有 helper，拿到 `nosniff` +
  `Cache-Control: no-store`（`dashboard_system.go` 的 R246-SEC-3 教训）。

## 5. 分阶段交付

**P1 — 只读提示（零风险，可独立 merge）— ✅ 已实现**
`selfupdate.Status` + `Checker` 写入 + `CheckNow`（§3.7）+
`GET /api/system/update` + chip 显示 + 预检 + §3.6 的 macOS 重启修复。
点击 chip 弹出说明气泡，按 `action` 给对应的手工命令（`install` →
`naozhi upgrade`；`restart` → `launchctl kickstart -k …` /
`systemctl restart naozhi`）。

> **为什么 §3.6 的 macOS 修复落在 P1 而不是 P2**：P1 的 `restart_supported` /
> `can_apply` 都由 `ServiceRunning()` 推导，label 不对就会对着一台明明有受管
> 服务的机器报"未检测到受管服务"。而 (a) 与 (b) 又不能拆开做——单改 (a) 会让
> `ServiceRunning()` 第一次返回 true，从而让既有的坏 `unload`+`load` 路径真的
> 被执行到，把"静默跳过"升级成"把服务摘掉"。所以两者必须同批。

**P1 实测（2026-09-01，隔离实例 127.0.0.1:8199，binary 置于 /tmp 与系统实例无关）**

`mode: notify`、`check_on_start: true`、启动后 8 秒：

```json
{"current":"v0.0.72","latest":"v0.0.73","staged":"","phase":"available",
 "action":"install","can_apply":true,"restart_supported":false}
```

`checked_at` 只比启动晚 6 秒，而 `check_on_start` 自带 30 秒延迟——**这条
`latest` 是 `CheckNow` 填的**，即 §3.7 的冷启动窗口填充确实在工作。

切到 `mode: download` 后等 checkOnce 完成（T+50s）：

```json
{"current":"v0.0.72","latest":"v0.0.73","staged":"v0.0.73","phase":"staged",
 "action":"restart","can_apply":false,
 "blocked_reason":"未检测到受管服务（systemd / launchd），手动重启进程即可让新版本生效",
 "restart_supported":false}
```

```
-rwxr-xr-x  19159458  naozhi                        ← 已换成 v0.0.73
-rw-------  27282914  naozhi.naozhi-upgrade.bak     ← 0600，仍是替换前那份
```

四条关键验收：

1. **`action` 是 `restart` 而不是 `install`** —— §1.3 的核心要求在真实链路上成立。
2. **`.bak` 保住了替换前的版本**，回滚 artifact 未被摧毁。
3. **`CheckNow` 不触发安装**：T+8s 时 `latest` 已知而 `staged` 仍为空，安装是
   到 checkOnce 才发生的。一个 GET 请求不会改变部署。
4. **`restart_supported: false` 正确**：该实例不是 launchd 启动的，
   `verifiedLaunchdLabel()` 返回空 → `can_apply` 随之为 false 并给出手工指引
   （G5），而不是渲染一个点了必失败的按钮。

P1 单独就解决了最大痛点：**本机那种"磁盘 v0.0.73 / 进程 v0.0.72 / 无人知晓"
的状态现在会显示成一枚 `↻ v0.0.73` chip。**

**P2 — 一键生效 — ✅ 已实现**
`Checker.InstallLatest`（含 §1.3 的 staged-只重启分叉 + `installMu` 单飞）+
`POST apply` + `confirmDialog` + 进行中轮询。§3.6 的 macOS 重启修复已随 P1
落地（见上），P2 直接受益。

### P2 实现中发现的第三条 staged 路径（§1.3 的补全）

RFC 原先只识别两种"已 staged"：`c.installed`（本进程装的）与
`rel.Tag == c.installed`。**还有第三种，而它恰好是 §1.3 最容易触发的一种**：
操作者在 shell 里跑了 `naozhi upgrade`。那是另一个进程，本进程的 `installed`
仍为空，`Status.staged` 也为空 → GET 报 `action: install` → apply 走完整
`doInstall` → **第二次 `Replace()` 把 `.bak` 里的可回滚版本 O_TRUNC 掉**，
正是 G7 要防的事故，只是触发者从"重复点击"换成了"命令行 + dashboard 混用"。

补法是在 `InstallLatest` 的 install 分支前探测磁盘：`<SelfPath()> --version`
（打印 ldflag 版本即退出，不读 config、不开端口、不碰状态），等于目标 tag
就降级为 restart-only 并把 `installed` 补上。

执行自己的 install path 不构成新增攻击面：能写那个文件的人在下一次重启就已
获得代码执行，而下一次重启正是本功能要触发的事。探测失败一律视为"测不出"
（`(…, false)`）并走正常安装路径 —— 它是安全网，不是门禁。

`onDiskVersionFn` 是可替换的测试缝，所以单测不会真的 exec 任何东西。

**残留缺口（P3 候选，非阻塞）**：`GET` **不做**这个探测（每次轮询 exec 一次
不可接受），所以上述场景下 chip 文案仍会说"可安装"。点下去的行为是正确的
（降级为只重启、`.bak` 不受损），只是文案偏保守。要修就得给探测结果加一层
与 preflight 同级的 60s 缓存。

### P2 的其它设计决定

- **`POST` 的 `confirm_action` 必须回传**。TOCTOU：UI 显示 `restart` 时后台
  checker 刚好装完变成别的状态，那一次点击就不再是操作者同意过的那件事。
  不一致 → 409，前端立即重读状态。
- **`restart` 参数由 handler 按 `ServiceRunning()` 决定**，且
  `InstallLatest` 内部还会再降一次级：若要求重启而无受管服务，安装照做、
  phase 落回 `staged` 并把 `ErrRestartUnsupported` 写进 `LastErr`。否则
  `doInstall` 会进 `PhaseRestarting` 然后调用一个 no-op 原语，chip 就永远停在
  "正在重启…"——一个不会有人来重启的进程。UI 文案同步分叉为「仅下载安装」。
- **限流不是为了防并发**（`TryLock` 已经防了），是为了防按住按钮刷失败路径 →
  刷日志 / 刷 GitHub。key 是常量 ⇒ 全局限流，对"安装本来就是全局单例"是正确
  语义，但代码注释必须写明，否则后人会以为它是 per-user（dashboard token 是
  单一共享值，per-token 限流实际等价于全局）。拒绝的请求也消耗窗口：要限的是
  尝试次数，不是成功次数。
- **`rollback_hint` 在确认前就给**。它回答的失败是"新 binary 起不来"，而那个
  失败里，本该告诉操作者怎么恢复的 dashboard 正是消失的东西。所以逃生路线必须
  在服务还活着的时候印在屏幕上。无受管服务时**只给 `cp` + `chmod`**，不猜重启
  命令——`launchdServiceLabel()` 此时回退成常量，硬拼出来的是一条注定失败的
  命令，还挂在 `&&` 链尾把前半段的成功一起吞掉。
- **`updateApplyFn` 测试缝**：handler 测试若真调 `InstallLatest` 就会从单测里
  打 GitHub。生产为 nil（走 `s.updateChecker.InstallLatest`）。

### P2 代码评审后的修正（2026-09-02）

**(a) 前端的"正在应用"标志必须有 deadline。** `InstallLatest` 有四条返回
error 但**有意不动 `phase`** 的路径：release lookup 失败（`noteCheck` 在
`err != nil` 时只写 `CheckErr` 就 return，且 `TestInstallLatest_LookupFailureLeavesPhase`
把这条钉成契约——什么都没装，报 `PhaseFailed` 是不诚实的）、`ErrNothingToDo`、
`ErrInstallInProgress`、`ErrCheckSkippedDev`。而前端清 `updateApplying` 的条件
只看 `action`/`phase`（`none`/`failed`/`staged`），在这些状态下 action 仍是
`install`、phase 仍是 `available` → **三个条件永不成立**，chip 会以 3s 轮询
永久停在"正在应用新版本，服务即将重启…"，点击只回一句"升级正在进行中"，
只能靠刷新页面恢复。最常见的触发就是点 apply 时 GitHub 那一下网络抖动。

修法落在前端而不是后端（后端不谎报 phase 是对的）：`UPDATE_APPLY_MAX_MS`
（90s）兜底，与 `markSessionOptimisticRunning` 的 20s 安全定时器同型。90s 是
因为慢 lookup 发生在 `doInstall` 写 `PhaseInstalling` **之前**，窗口必须能装下
它；早退的代价很小（回落到服务端事实，且重复点击安全——TryLock + 三条 staged
短路让二次 apply 成为 no-op），晚退的代价只是一个陈旧但诚实的标签。
`setUpdateApplying` 是该标志的唯一写者，deadline 因此不会被"直接清 flag"的
路径孤儿化。

**(b) 202 必须在起 goroutine 之前 write + Flush。** 原实现先 `go func()` 再
`writeJSONStatus`。仅调顺序还不够：net/http 会把 handler 的响应留在 buffer 里
直到 handler 返回，而 restart-only 路径前面没有下载，goroutine 可在毫秒级走到
`launchctl kickstart -k` / `systemctl restart`，随之而来的 SIGTERM 完全可能
先到。那样浏览器看到的是连接中断，`applyUpdate` 把它报成"升级请求失败"——
**在 apply 正在成功的那一刻告诉操作者它失败了**。所以写完立刻
`http.Flusher.Flush()`，再起 goroutine。

`TestUpdateApply_RespondsBeforeWorkCompletes` 抓不到这个：它用 channel 把 apply
卡住，顺序问题被掩盖。新增两条覆盖：行为测试（自定义 `flushRecorder`，断言
apply 进入时 flush 已发生）+ 源码顺序守卫（httptest 下 flush 永远大幅领先，
所以真正的不变量是**源码顺序**，与 `TestUpdateApply_DetachedContextSourceGuard`
同法）。两条都经过反向验证：把 write+flush 移回 goroutine 之后，二者同时失败。

**(c) `install_enabled` 补进 GET 形状契约的 required 列表。** 前端
`updateCanApply` 要求 `can_apply && install_enabled` 同时为真才渲染按钮，所以
该字段一旦停止下发，功能会静默退化成"永远只显示手工命令"，而其余测试全绿。
形状契约的意义正是钉住这种字段。

**(d) 服务探测由调用方注入，每请求只做一次。** `ServiceRunning()` 在 darwin 上
是一次 `launchctl list` fork（`verifiedLaunchdLabel`），而 staged 稳定态下的一次
GET 会独立探测**三遍**同一个不会变的事实：`CheckPreflight` 里的 `actionGate`
（原注释称它 "cheap"——在 launchd 上它不是）、响应的 `restart_supported` 字段、
以及 `RollbackHint()`。这条路径被每个打开的 dashboard 按 60s 轮询，apply 期间
提到 3s。

`CheckPreflight(action, current, serviceRunning)` 与 `RollbackHint(serviceRunning)`
改为接受该事实，由 handler 探测一次后分发：GET 3→1 次，POST 2→1 次（POST 本来
就要为 `restart` 参数算一遍）。**特意没有加缓存**：缓存会让刚配好的 launchd job
在一个 TTL 内隐形，在 POST 路径上等于拒掉一次本可以成功的 apply。
`TestUpdateHandlers_ProbeServiceOnce` 是源码守卫——这个成本只在 darwin 上存在，
而 darwin 上那个 probe 无法 stub，行为测试数不到。

签名改动顺带修好了两个测试的可达性：`TestRollbackHint` 的 no-service 分支此前
在"本机恰好由 launchd 托管"时被 skip，`TestRollbackHint_IncludesRestartWhenServiceRuns`
此前是 linux-only（`systemdUnitActive` 是唯一能 stub 的探针）。现在两者都在任何
主机上真跑，后者按 GOOS 断言 `kickstart` 或 `systemctl`。

**(e) busy 态的 spinner 补上**（§3.4 原本就要求"disabled + spinner"，实现只做到
了 dim + `pointer-events:none`）。等待的轮询有 3s 间隔，一个变暗的静止图标读起来
像卡住的按钮而不是"正在工作"。复用既有的 `spin` keyframes 与 `--nz-dur-spin`
（与 `.upload-status.uploading` 同一对），不新增动画、不新增颜色 token；站点级
`prefers-reduced-motion` guard 自动覆盖它，无需自带规则。`restart` 态的图标本身
就是循环箭头，转起来正好。

**P2 实测（2026-09-01，隔离实例 127.0.0.1:8198，binary 在 /tmp，`mode: notify`）**

`current=v0.0.72`、远端 `latest=v0.0.74`，`action: install`、`can_apply: true`：

```
POST /api/system/update/apply {"confirm_action":"install"} → 202 {"status":"started"}
23:05:39 dashboard update: apply requested  action=install restart=false
23:05:42 auto-update: binary installed     tag=v0.0.74 restart=false   (2.6s)
```

```
-rwxr-xr-x  19176002  naozhi                        ← 真实下载校验替换成 v0.0.74
-rw-------  27301618  naozhi.naozhi-upgrade.bak     ← 0600，仍是 v0.0.72
```

替换后 `action` 自动变成 `restart`、`can_apply: false` +
`blocked_reason: 未检测到受管服务…`（该实例不是 launchd 启动的，G5 成立）。

随后逐条验收（同一实例）：

| 验收 | 结果 |
|---|---|
| staged 态发 `confirm_action: install` | **409** `state changed: confirmed "install" but current action is "restart"` |
| 紧接第二次 POST | **429** `too many update attempts` |
| 31s 后发 `confirm_action: restart` | **409** `未检测到受管服务…`（preflight 快速失败，未起任何后台 goroutine） |
| **三次 apply 尝试后 `.bak` md5** | **`cb5199963cdb42b7adaf39a8a736a2ef` 不变（G7 成立）** |
| `.bak` 加执行位后 `--version` | **`v0.0.72`** —— 回滚 artifact 真的可用，不只是"文件还在" |
| 无 token 的 GET / POST | 双双 **401** |
| `dashboard_install: false` 重启后 | POST **403**，GET 仍 **200** 且 `install_enabled: false`、`action` 照实报 `install` |

最有价值的一条是 `.bak` 的 md5：它是在**真实下载+替换发生过之后**、又经历三次
apply 尝试（含一次 staged 态的 restart 确认）才校验的，所以 §1.3 的约束是在
真链路上被验证的，不是靠单测断言。

**仍未验证的一条**：G2 的"点击 → launchd 真的重启 → 重连后 `current` 为新版本"
需要一台由 launchd 托管、且跑着含本 PR 版本的实例——只有正式实例 :8180 满足，
需要操作者授权重启它。`restartOnly` 走的是 P1 已实测可用的
`kickstart -k gui/<uid>/<label>`，但"整条链在受管进程上真的换代"仍缺一次实机确认。

### P2 二轮评审修正（2026-09-02）

**F1 · P0 单测会重启生产服务。** `TestInstallLatest_StagedDoesNotDownload` 与
`TestInstallLatest_OnDiskProbeShortCircuits` 调 `InstallLatest(ctx, true)` 却没
stub 服务探测：在一台由 systemd 托管 naozhi 的 Linux 上，restart-only 路径直达
真实的 `systemctl restart --no-block naozhi`——跑一次 `go test` 就重启旁边的生产
实例。修法：`RestartServiceNoWait` 的实现下沉为包内变量 `restartServiceNoWaitFn`，
测试用 `withNoManagedService(t)`（unit 不活跃 + MainPID=0 + 记录调用的 restart
stub）并**断言 restart 未被调用**；新增 `TestInstallLatest_StagedRestartsWhenManaged`
证明受管态下 seam 恰好被调一次（正向），`TestInstallLatest_StagedRefusesForeignService`
证明 unit 活跃但 MainPID 不是自己时拒绝（F2 的回归门）。

**F2 · P1 Linux `ServiceRunning()` 不校验"这个 unit 跑的是我"。** darwin 侧
`verifiedLaunchdLabel` 会核对 job 的可执行文件 = `SelfPath()`，Linux 只查
`is-active`。后果：/tmp 隔离实例、`go test` 进程都会看到 `restart_supported: true`，
点「重启生效」→ 重启的是系统服务，本进程留在旧版本、phase 卡 `restarting`。修法：
拆成两个语义——`ServiceRunning()`（**存在**受管服务，CLI `naozhi upgrade` 从 shell
运行时不是 MainPID，必须保留这条）与新增 `ServiceManagesThisProcess()`（受管的
**是本进程**：Linux = `is-active` && `systemctl show naozhi -p MainPID --value` ==
`os.Getpid()`；`deploy/naozhi.service` 的 ExecStart 直接跑 naozhi、`NotifyAccess=main`，
MainPID 就是主进程，实机核对 `/proc/<MainPID>/exe` 亦为 naozhi 本体；darwin 沿用
`verifiedLaunchdLabel`，两者重合）。dashboard 两个 handler、`InstallLatest` 的降级、
`restartOnly`、`restartSystemdNoWait` 全部改用后者；`upgrade.go` 不动。
`TestUpdateHandlers_ProbeServiceOnce` 同时守住"handler 不得再用 `ServiceRunning()`"。

**F3 · P2 手工命令按浏览器 OS 猜且硬编码 dev 机 label。** `dashboard.js` 用
`navigator.platform` 判 Mac/Linux，并写死 `com.naozhi.agent`——那是操作者笔记本的
OS 与某一台 dev 机的 label，与被升级的节点无关。修法：GET 新增 `manual_command`
（服务端按 `runtime.GOOS` + 真实 `launchdServiceLabel()` 生成；`install` →
`naozhi upgrade`；`restart` 且受管本进程 → 平台重启命令；`restart` 且无受管服务 →
空，`blocked_reason` 已说"手动重启进程"，再给 `systemctl restart naozhi` 恰恰是 F2
那个错误的服务）。`restartCommand()` 与 `RollbackHint` 共用，二者不会漂移
（`TestManualCommand` 断言 hint 以该命令结尾）。前端删掉平台分支，JS 契约测试反向
禁止 `navigator.platform` / `com.naozhi.` / 硬编码重启命令再出现。

**F4 · P2 `.bak` 字节断言永真。** 旧测试把假 `.bak` 写进 `t.TempDir()`，而
`Replace()` 写的是 `SelfPath()+backupSuffix`，两者永不相交，断言不可能失败。已删，
改为 F1 引入的注入点断言（`lookups == 0` + restart stub 调用记录），注释不再声称
保护 `.bak` 字节。

**F7 · P3 后台 apply goroutine 无 recover。** 它不在任何请求栈上，panic 直接带走
整个进程。补 `defer recover()`（同 `wshub_eventpush` / `send_owner_loop` 的写法），
并新增 `Status.MarkFailed` 把 phase 落到 `failed` + `last_error`，否则 chip 会停在
`installing`。`TestUpdateApply_PanicIsRecovered` 覆盖。

**F8 · P3 冷启动补检条件过严 + 可挂 60s。** 原条件 `latest=="" && at.IsZero()`，
而 `noteCheck` 失败也推进 `checkedAt`，启动时一次网络抖动就让补检永久失效直到 6h
tick。放宽为 `latest==""`；`CheckNow` 自带 15min 全局节流（`checkMu` 内、网络调用
**前**盖章），放宽不会打爆 GitHub。同时给 GET 侧的 `CheckNow` 套 10s
`updateColdStartFillTimeout`，替代原本可能吃满的 60s。`TestUpdateStatus_ColdStartFillGuard`
源码守卫。

**顺带：`enabled` 恒为 true。** 原为 `s.updateStatus != nil`，但 main.go 无条件建
Status，生产永远 true。改为 `s.updateChecker != nil`（真正反映 `update.enabled`），
`TestUpdateStatusShape` 的断言随之翻转，新增 `TestUpdateStatusEnabledTracksChecker`
（dev-build Checker，`CheckNow` 对 dev 不出网）。

**P3 — 可选增强**
WS 推进度（取代轮询）；下载字节进度条；`update.enabled: false` 下的按需 check
策略。均非必需。

## 6. 测试

**`internal/selfupdate`**

- `Status` 并发读写（`-race`：一个 goroutine 循环 `Snapshot`，另一个 `notePhase`）。
- `Status == nil` 时 `Checker` 行为与今天逐字节一致——**现有 `checker_test.go`
  全部不改动即通过**，是这条的天然回归门。
- `InstallLatest` 单飞：两个并发调用，第二个必须拿到 `ErrInstallInProgress`
  而非排队。
- `InstallLatest` 与后台 tick 的互斥（stub `latestRelease` + stub 下载）。
- **§1.3 回归门（最重要的一条）**：构造 staged 态（`c.installed = latest`），
  调 `InstallLatest`，断言 (i) 没有发生任何 `Download`；(ii) `.bak` 文件内容
  **未被改写**；(iii) 只触发了重启。这条直接锁死"重复 apply 摧毁回滚 artifact"。
- `launchdServiceLabel()`：`XPC_SERVICE_NAME` 有值 → 用它；空/纯空白 → 回退常量。
- `launchdJobRunsPath()`：喂**本机真实的** `launchctl list com.naozhi.agent`
  输出（含 `Program` 与 `ProgramArguments` 两种形态）；断言匹配本 binary、
  **拒绝 Terminal.app 那种真实存在但不是我们的 job**（F13）、无可执行路径时
  fail closed。
- `restartLaunchd` 命令行拼装：断言 `kickstart -k gui/<uid>/<label>`，且
  **不再出现 `unload`/`load`**（防回归到 F6 的坏形态）。
- `ServiceRunning()` 与 `restartLaunchd()` 在 darwin 分支都用
  `verifiedLaunchdLabel()`（防 F4 只修一半、防 F13）。
- `CheckNow` 的 15m 节流：连续两次调用只发一次请求；8 个并发调用只产生 1 次
  GitHub 查询（全局而非 per-caller）；**失败也消耗节流窗口**（时间戳在网络
  调用之前写，否则 GitHub 挂掉时每次轮询都会排在同一个死端点后面）；dev build
  完全不触网。

**`internal/server`**

- GET 形状契约（跟 `stats_shape_test.go` / `sessions_shape_test.go` 同构）：
  字段名与类型锁死。
- **`action` 的服务端判定矩阵**：
  `current==latest` → `none`；`latest>current && staged==""` → `install`；
  `staged==latest && staged!=current` → `restart`；`latest<current`（降级）→ `none`。
- 未认证 GET/POST → 401；跨 Origin POST → CSRF 拒绝。
- `confirm_action` 与服务端 `action` 不一致 → 409（TOCTOU 门）。
- `can_apply == false` 时 POST → 409 且**不启动任何后台 goroutine**。
- `dashboard_install: false` → POST 403，GET 照常 200。
- 限流：连续 POST 第二次 → 429。
- **202 在起 goroutine 之前 write + Flush**（见 P2 修正 (b)）：行为测试断言
  apply 进入时 flush 已发生，源码守卫断言 `writeJSONStatus` 与 `f.Flush()`
  的位置都早于 `go func()`。
- GET 形状契约的 required 必须含 `install_enabled`——前端拿它当门，字段消失
  会静默退化成"只给手工命令"（见 P2 修正 (c)）。
- **每个 handler 只探测服务一次**（源码守卫，见 P2 修正 (d)）：`ServiceRunning()`
  在 darwin 上是 fork，需要它的地方应取调用方那一份。
- routes.go 每加一行路由要 bump `tools/lint-server-handlers/exemptions.yaml`
  的 baseline（见 `ab29e61e` 先例，本次 +2 行）。

**静态资产契约**（`static_*_contract_test.go` 惯例）

- `#btn-update` 存在且默认带 `hidden`；三种 `action` 的中文文案齐备且互不相同；
  CSS 只用既有 token（grep 断言不出现新 `--nz-*` 定义或裸色值）。
- `UPDATE_APPLY_MAX_MS` 存在，且不出现裸 `updateApplying = true`——标志必须
  经 `setUpdateApplying` 写入，否则 deadline 会被绕过而 chip 可能永久卡在
  "正在应用新版本"（见 P2 修正 (a)）。
- busy 态用既有 `spin` keyframes + `--nz-dur-spin` 转图标（见 P2 修正 (e)）。

**手工验收**

- G1 **本机当前状态**（磁盘 v0.0.73 / 进程 v0.0.72）→ chip 应显示
  `↻ v0.0.73`、`action: restart`。这是现成的、无需构造的验收样本。
- G2 点击 → 确认 → **launchd 真的重启**（`launchctl list com.naozhi.agent`
  的 PID 从 1552 变化）→ 重连后 chip 消失、`current` = v0.0.73。**这是 F4/F6
  修复的唯一有效验收**，单元测试证明不了。
- G3 验证 `.bak` 在 restart-only 路径后仍是 v0.0.72（`.bak` 加执行位后跑
  `--version`）——§1.3 的手工确认。
- G4 把 binary chown 到 root：chip 仍显示，但给手工命令而非按钮。
- G5 Linux 生产节点上的同一路径：`systemctl restart --no-block` 生效。
- G6 `install` 路径断网，分两种失败：**下载/替换**失败 → `phase: failed` +
  `last_error` 可读；**release lookup** 失败 → `phase` 不动（什么都没装，报
  `failed`是不诚实的）、`check_error` 可读，chip 在 `UPDATE_APPLY_MAX_MS`
  后自行回落。两种都要求服务仍在旧版本上正常跑。（原稿只写了前一种，把后一种
  也当成 `failed`——正是 P2 修正 (a) 的前端卡死之源。）

## 7. 风险与缓解

| 风险 | 缓解 |
|---|---|
| **重复 apply 摧毁 `.bak` 里的可回滚版本**（§1.3） | staged 态强制走 restart-only 分支；§6 有专门的回归门断言 `.bak` 未被改写。这是本 RFC 最高优先级的正确性约束（G7） |
| 新 binary 起不来，dashboard 也没了，操作者失去唯一入口 | `Replace` 保留 `.bak`；`Mode: auto` 已确立"自重启路径永不删 backup"；`confirmDialog` 在触发前把回滚命令写在 `detail` 里（`cp <bak> <path> && chmod 0755 && <restart>`）供操作者复制 |
| `kickstart -k` 在某些 macOS 版本 / domain 下行为不同 | 只在 `ServiceRunning()` 为真时调用；失败返回 error → `phase: failed` + 明确文案。plist `KeepAlive=true` 是二次保险 |
| **`XPC_SERVICE_NAME` 被继承，导致重启了别的服务**（F13） | `verifiedLaunchdLabel()` 校验该 label 的 job 确实跑本 binary（比对 `Program` / `ProgramArguments[0]` 与 `SelfPath()`，两侧 EvalSymlinks），取不到路径则 fail closed。人为篡改该 env 需要已能控制进程环境，属更高权限，不构成新增提权路径 |
| 冷启动 6h 窗口内功能空白 | §3.7 的 `CheckNow`，15m 全局节流，只在 `latest == ""` 时触发 |
| 多 node 部署下误解为全量升级 | NG2 + `confirmDialog` 文案强制含"本节点"字样；静态契约测试断言该字样存在 |
| 轮询增加负载 | 60s / 客户端，无网络：Status 是内存读，预检缓存 60s。**唯一的非内存成本是每请求一次 `ServiceRunning()`**——darwin 上是一次 `launchctl list` fork，由 handler 探测后分发给预检 / `restart_supported` / `RollbackHint`（P2 修正 (d) 之前是三次）。有意不缓存它：见修正 (d)。进行中才提到 3s |
| 操作者在 turn 进行中点了重启 | F10：shim 让 CLI 子进程跨重启存活。`detail` 如实说明并列出 `running_sessions` 供判断 |
| `dev` build 被误替换 | `checkOnce` 已跳过 dev；预检把它列为第一条阻塞原因（§3.5） |

## 8. 结论

可行，成本集中在**接线与 UI**，不在核心逻辑——下载/校验/替换/回滚全部复用。
实现工作有四块：`Status` 共享对象、`Checker` 的单飞锁 + staged 分叉、
macOS 重启链的两处修复、以及 chip + `confirmDialog`。

v2 相对 v1 的实质变化：

1. **主线场景从"点击即安装"纠正为"点击即生效"**（§1.2）。默认 `mode: download`
   下稳定态是 `staged`，本机日志显示这个状态曾空转 22 小时。
2. **识别出一个数据损坏风险**（§1.3）：v1 的设计会在最常见的操作路径上摧毁
   `.bak` 里唯一的回滚版本。
3. **macOS label 修复从一套 `launchctl list` 解析简化为一行 `os.Getenv`**
   （F5 实测），并发现 `ServiceRunning()` 也必须一起改，否则只修一半。
4. **on-demand check 从 P3 提到 P1**（§3.7）：否则重启后 6 小时功能空白。

建议按 P1 / P2 两个 PR 走：P1 零风险、独立有价值——本机现在就处于它要暴露的
那个状态，merge 后立刻能看到效果。
