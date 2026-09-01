# RFC: dashboard 新版本发现与一键安装

- 状态：Draft v1
- 日期：2026-09-01
- 作者：Kevin Zhao
- 范围：dashboard 侧栏 header 出现"有新版本"提示，点击后确认 → 下载、校验、
  原子替换、重启服务。复用既有 `internal/selfupdate` 全链路，不新增下载/校验逻辑
- 前置：`selfupdate-signing.md`（签名链现状与待办，本 RFC 不动）

## 1. Background & problem

`internal/selfupdate` 的**后端能力已经完整**：`LatestRelease` → `Download`
（SHA-256 校验 + 可选 checksums.txt pin）→ `Replace`（备份 + O_EXCL staging +
原子 rename + 强制 chmod 0755 + 失败回滚）→ `RestartService`。这套流水线同时
被 `naozhi upgrade` CLI 和后台 `selfupdate.Checker` 使用，已经历过 v0.0.27
升级事故的加固。

缺的只是**操作者视角的入口**。今天三条路径都不满足"看到并点一下"：

1. `Mode: notify` 只发 IM 通知，dashboard 无感知；
2. `Mode: download`（默认）静默装好新 binary，但生效要等下次重启——操作者
   既不知道"已经装好了"，也没有触发重启的按钮；
3. dashboard 只在设置的「关于」区显示 `stats.version_tag`（当前版本），
   **没有任何"最新版本是多少"的信息**，无从判断该不该升。

结果是升级动作被迫退回 SSH：`ssh host && naozhi upgrade`。对一个自带 Web
控制台的服务来说这是明显的缺口。

### 1.1 实测事实（2026-09-01，本机 macOS 26.6 / naozhi v0.0.72）

逐条手工验证，**其中 F4/F5 是会让"一键安装"在 macOS 上装得上却不生效的
真实阻塞项**：

| # | 场景 | 结果 |
|---|---|---|
| F1 | 运行中 binary 能否自替换 | **可以**。`~/.local/bin/naozhi` 是 `-rwxr-xr-x zhaokm:staff`，`~/.local/bin` 可写 → `Replace` 的 backup/stage/rename 三步都在同一 uid 同一目录内 |
| F2 | `LatestRelease` 走的是哪个 GitHub 端点 | **HTML redirect**（`github.com/<repo>/releases/latest` → `/releases/tag/vX.Y.Z`，只读 `resp.Request.URL`，不读 body），**不是** `api.github.com` |
| F3 | `api.github.com` 匿名限额 | 实测 `x-ratelimit-limit: 60`（每 IP 每小时）。F2 意味着现状不吃这个限额，但仍不能让每个 dashboard 客户端各自打一次 GitHub → 设计上服务端缓存（§4.1） |
| F4 | 本机 launchd label 是否匹配代码常量 | **不匹配**。`selfupdate.LaunchdLabel = "com.naozhi.naozhi"`，本机实际是 `com.naozhi.agent`（`launchctl list com.naozhi.naozhi` → `Could not find service`）→ `ServiceRunning()` 返回 false → **重启被静默跳过，无 WARN 无报错** |
| F5 | `restartLaunchd` 在自重启场景是否可用 | **不可用（结构性）**。它 `launchctl unload` 后再 `load`，但 unload 杀死的正是发起调用的进程 → `load` 那行永远执行不到。plist 是 `KeepAlive=true`，所以进程死后会被拉起，但拉起的是**旧 plist 指向的 binary**、且 unload 已把 job 摘掉 → 服务停摆 |
| F6 | `launchctl kickstart -k` 是否可用 | **可用**（`Usage: launchctl kickstart [-k] [-p] <service-target>`，本机 uid 503 → `gui/503/<label>`）。这是 macOS 上"重启自己"的正确原语：由 launchd 而非自己发起 |
| F7 | 是否有真实可升级差值可做端到端验收 | **有**。本机 v0.0.72，GitHub latest v0.0.73；release assets 齐备（`naozhi-darwin-arm64` + `.sha256` + `checksums.txt`） |
| F8 | systemd 侧自重启路径 | **健全**。`Restart=always` + `systemctl restart --no-block`，`RestartServiceNoWait` 的注释已详述为何不能在自重启时 poll `is-active`（TOCTOU + 自己还"active"） |
| F9 | 活跃 CLI 会话是否会被 naozhi 重启杀掉 | **不会**（既有机制，本 RFC 不重新验证）。`internal/shim` 的 sidecar 进程 setsid 脱离 + systemd `KillMode=process`，naozhi 重启后经 `Manager.Discover`/`Reconnect` 重新接上 |
| F10 | 写端点的 CSRF 门 | **自动覆盖**。`internal/dashboard/auth/csrf.go` 对非安全方法做 Origin 校验，新增 POST 无需额外接线 |

四条结论：

- **后端零新增风险面。** 下载/校验/替换/回滚一行都不用重写，dashboard 只是给
  既有流水线加一个人工触发点。
- **macOS 的重启链是断的（F4+F5），必须在本 RFC 内修。** 否则"点击即安装"在
  macOS 上的实际效果是"装好了，但你还得自己 `launchctl` 一下"——正是这个功能
  想消除的体验。这也解释了此前"macOS 升级后需手动重启 launchd"的观察。
- **`Checker` 现在是单 goroutine 无锁的**（`c.installed` 裸字段，包注释明确
  "owns no global state and is safe to run as a single goroutine"）。引入
  dashboard 触发后就是两个写者 → **必须加单飞锁**，否则 data race + 并发装同一
  release。这是本 RFC 最容易被漏掉的正确性要求。
- **权限降级必须在 UI 上如实反映。** binary 属 root 而服务跑普通用户时
  `Replace` 必失败（`checker.go` 的注释已经把这列为常见降级原因）。不能给操作者
  一个点下去注定失败的按钮。

## 2. Goals & non-goals

### Goals

- G1 dashboard 常驻可见"有新版本 vX.Y.Z"提示，不用去设置页翻。
- G2 一次点击 + 一次确认 → 下载、校验、替换、重启，全程在 Web 上完成。
- G3 安装过程与结果可见：进行中 / 已装待重启 / 失败原因。
- G4 修 F4/F5，让 macOS 的重启真正发生。
- G5 不可安装时（无写权限 / dev build / 不支持的平台）UI 明确说明并给出手工命令，
  而不是显示一个会失败的按钮。
- G6 界面尽量简单：一个 chip + 一个确认气泡，不新建视图、不新建页面。

### Non-goals

- NG1 **不做版本选择 / 降级。** 只装 `releases/latest`。`semverGreater` 的
  防降级语义（`checker.go` R20260602141221-SEC-1）继续生效。
- NG2 **不做多节点批量升级。** 多节点部署下每个节点各自升级；本功能只作用于
  当前 dashboard 所连的那个进程。
- NG3 **不升级 CLI（claude / kiro / codex）。** 那是 backend 的事。
- NG4 **不动签名机制。** `selfupdate-signing.md` 另论；本 RFC 沿用现有
  SHA-256 + 可选 `NAOZHI_UPGRADE_PIN_SHA256` 信任链。
- NG5 **不做 WebSocket 进度推送**（P3 可选）。P2 用低频轮询，够了。
- NG6 不改 `update.mode` 的既有语义。dashboard 安装是 mode 之外的**手工**通道。

## 3. 设计

### 3.1 `selfupdate.Status`：单一真相源，零额外 GitHub 请求

后台 `Checker` 每 `update.interval`（默认 6h）已经在查 latest。dashboard 不该
再自己去查——把 Checker 的结果落到一个进程内共享对象，dashboard 只读它：

```go
// internal/selfupdate/status.go
type Phase string

const (
    PhaseIdle       Phase = "idle"        // 尚未 check，或已是最新
    PhaseAvailable  Phase = "available"   // 发现更新，未安装
    PhaseInstalling Phase = "installing"  // 下载/校验/替换进行中
    PhaseStaged     Phase = "staged"      // 已落盘，等重启生效
    PhaseRestarting Phase = "restarting"  // 重启已触发，本进程即将死
    PhaseFailed     Phase = "failed"      // 最近一次尝试失败，LastErr 有值
)

// Status 是 Checker 与 dashboard 之间唯一的共享状态。所有字段经 mu 保护：
// 写者是 Checker（后台 tick）与 dashboard apply handler，读者是 HTTP GET。
type Status struct {
    mu        sync.Mutex
    current   string    // 编译期 version，不变
    latest    string    // 最近一次成功 check 的 tag
    checkedAt time.Time
    checkErr  string    // 最近一次 check 的错误（脱敏后）
    phase     Phase
    staged    string    // 已落盘但未生效的 tag
    lastErr   string
}

func NewStatus(current string) *Status
func (s *Status) Snapshot() StatusSnapshot   // 值拷贝，给 HTTP 层
func (s *Status) noteCheck(latest string, err error)
func (s *Status) notePhase(p Phase, staged string, err error)
```

`Checker` 持有 `*Status` 并在 `checkOnce` / `doInstall` 的每个转折点写它。
`Status` 为 nil 时全部退化为 no-op，这样 `Checker` 在没有 dashboard 的场景
（以及现有测试）里行为完全不变。

**装配**：`main.go` 当前在 `srv` 构造之后才 `startUpdateChecker`（L704），
所以 `NewStatus(version)` 要提前到 `srv` 构造之前，同一个指针分别交给
`server.Options.UpdateStatus` 和 `startUpdateChecker`。

### 3.2 `Checker.InstallLatest`：复用 `doInstall`，加单飞锁

**不复制**安装逻辑。`doInstall` 已经承载了全部回滚语义与那一长串事故教训注释，
dashboard 走同一条路：

```go
// InstallLatest 由 dashboard 的 POST apply 同步调用（在自己的 goroutine 里）。
// 与后台 tick 共用 installMu，所以两个触发点永不并发装同一个 release。
func (c *Checker) InstallLatest(ctx context.Context, restart bool) error
```

`Checker` 新增 `installMu sync.Mutex`，`checkOnce` 与 `InstallLatest` 都在
它下面跑安装段；`c.installed` 的读写一并纳入保护。`InstallLatest` 用
`TryLock` 语义：已有安装在跑时立刻返回 `ErrInstallInProgress`，handler 转
409，而不是排队堆积。

`doInstall` 目前把失败**吞成 WARN + notify**（对后台 tick 是对的）。改为
返回 error，`checkOnce` 侧保留原来的 log+notify 行为，`InstallLatest` 侧把
error 交给 HTTP 层——两个调用者的错误处理策略不同，但流水线只有一份。

### 3.3 API：两个端点

```
GET  /api/system/update          读状态（可 60s 轮询）
POST /api/system/update/apply    触发安装，202 + 后台执行
```

放在 `internal/server/dashboard_update.go`，与 `dashboard_system.go` 同构。
两个 handler 太小，不值得开 `internal/dashboard/update/` 子包——保持简单。

**GET 响应形状**：

```json
{
  "current": "v0.0.72",
  "latest": "v0.0.73",
  "update_available": true,
  "checked_at": "2026-09-01T16:20:00Z",
  "check_error": "",
  "phase": "available",
  "staged": "",
  "last_error": "",
  "can_install": true,
  "install_blocked_reason": "",
  "restart_supported": true
}
```

- `update_available` 由服务端用 `semverGreater` 判定，**前端不做版本比较**
  （避免前后端两套 semver 语义漂移）。
- `can_install` / `install_blocked_reason` 是**预检**结果（§3.5），UI 靠它
  决定渲染按钮还是渲染手工命令提示。
- `restart_supported` = `ServiceRunning() && GOOS ∈ {linux, darwin}`。false
  时安装仍可做，但 UI 文案改成"下次重启生效"而不是"立即生效"。

**POST 语义**：

- Body：`{"restart": true}`（默认 true；`false` 对应"只装不重启"）。
- 立刻返回 `202 {"status":"started"}`，实际工作在后台 goroutine 里跑。
  **必须异步**：`restart: true` 时这个进程会在响应写出之前被杀，同步 handler
  的响应根本发不出去。
- 后台 ctx 用 `context.WithTimeout(context.Background(), 10*time.Minute)`，
  **不能**用 `r.Context()`——请求在 202 之后就结束了，会立刻取消下载。
- 已在安装中 → 409；`can_install == false` → 409 + 原因；无可用更新 → 409。
- 前端不等 POST 的结果，改为轮询 GET（安装中提到 3s）；`phase` 走到
  `restarting` 后连接会断，前端进入"重连中"状态，重连成功后读到新的
  `current` 即为成功。

### 3.4 前端：一个 chip + 一个确认气泡

落点是侧栏 header 的 `.hdr-btns`（`dashboard.html:2958`），在 `btn-history`
之前插一个默认隐藏的按钮：

```html
<button type="button" class="hdr-btn hdr-btn-update" id="btn-update" hidden
        title="有新版本可用" aria-label="有新版本可用">
  <svg viewBox="0 0 24 24" aria-hidden="true" focusable="false">
    <path d="M12 19V5"/><path d="M5 12l7-7 7 7"/>
  </svg>
  <span class="hdr-update-tag" id="update-tag"></span>
</button>
```

- 提示态：`update_available` 为真时 `hidden=false`，tag 显示 `v0.0.73`，
  按钮描边用 `--nz-accent`（**不引入新颜色 token**，遵循 UI token 纪律）。
- 点击 → 一个小 popover（复用项目已有的 popover/modal 基础设施），内容三行：
  当前 → 目标版本；一句风险说明（"服务将重启；正在进行的对话不会中断，
  CLI 子进程由 shim 保持存活"）；一个「立即安装并重启」按钮。
- 安装中：按钮进入 disabled + spinner，popover 文案跟 `phase` 走。
- `can_install == false`：popover 不显示安装按钮，改为显示
  `install_blocked_reason` + 可复制的手工命令（`sudo naozhi upgrade`）。
- 失败：popover 显示 `last_error`，按钮变「重试」。
- 事件绑定走 `addEventListener`（项目惯例），不用 inline handler。

**轮询**：独立的低频 timer，60s 一次 GET。不挂到 `fetchSessions` 上——那是
秒级循环，且它的 `stats.version` 短路逻辑会把这类无关字段的变化吃掉。安装中
临时提到 3s，`phase` 回到终态后退回 60s。

### 3.5 可安装性预检

`GET` 每次调用时做一次廉价预检，按顺序返回第一个命中的阻塞原因：

1. `current == "dev" || current == ""` → `"dev build：本地构建不会被自动替换，用 naozhi upgrade --force"`
   （与 `checkOnce` 的 dev 跳过规则一致）。
2. `checkPlatform()` 失败 → `"当前平台没有 release 资产"`。
3. install 目录不可写 → `"<dir> 不可写（当前 uid <n>）：用 sudo naozhi upgrade"`。
   探测方式：对 `filepath.Dir(SelfPath())` 做一次 `os.CreateTemp` +
   立即 `Remove`，与 `Replace` 真正要做的操作同构（比 `unix.Access` 更准，
   能覆盖只读挂载、SIP、ACL）。结果缓存 60s，避免每次轮询都碰盘。

### 3.6 macOS 重启修复（G4）

两个独立的改动，都在 `internal/selfupdate/service.go`：

**(a) label 自动发现，取代硬编码常量。** F4 说明 `com.naozhi.naozhi` 只对
`naozhi install` 装出来的服务成立；手工装的 plist（本机 `com.naozhi.agent`）
下 `ServiceRunning()` 永远 false。新增：

```go
// DetectLaunchdLabel 解析 `launchctl list` 的表格输出，返回其 Program
// （或 ProgramArguments[0]）解析后等于 SelfPath() 的那个 label。找不到时
// 回退到 LaunchdLabel 常量，保持既有行为。结果按进程缓存一次。
func DetectLaunchdLabel() string
```

`launchctl list` 的三列输出（PID / Status / Label）不含 Program，所以要对
候选 label 再各跑一次 `launchctl list <label>` 读 `"Program"` /
`"ProgramArguments"`。为控制成本：只检查 label 含 `naozhi` 的行（本机 1 条）。
匹配用 `filepath.EvalSymlinks` 后的路径比较，与 `SelfPath()` 同一套归一化。

**(b) `restartLaunchd` 改用 `kickstart -k`。** 取代坏掉的 unload+load：

```go
func restartLaunchd() error {
    if !ServiceRunning() { return nil }
    target := fmt.Sprintf("gui/%d/%s", os.Getuid(), DetectLaunchdLabel())
    out, err := exec.Command(resolveTrustedBin("launchctl"), "kickstart", "-k", target).CombinedOutput()
    ...
}
```

`kickstart -k` 由 launchd 发起 SIGTERM 并在进程退出后重新 spawn，job 始终留在
domain 里——这正是 `RestartServiceNoWait` 的 fire-and-forget 语义在 macOS 上
的正确实现，也和 systemd `restart --no-block` 对称。

`gui/<uid>` domain：naozhi 装的是 LaunchAgent（`~/Library/LaunchAgents`），
所以是 `gui/`；若将来支持 LaunchDaemon 需要 `system/`，届时按 plist 位置分支。

**这两项修复顺带修好了 CLI `naozhi upgrade` 和后台 `Mode: auto` 在 macOS 上的
重启**——它们走的是同一个 `restartLaunchd`。

### 3.7 配置开关

```yaml
update:
  # dashboard 上是否提供"一键安装"按钮。false 时 GET 仍返回版本信息
  # （提示照常显示），但 POST apply 返回 403。默认 true。
  dashboard_install: true
```

理由：提示（只读）和安装（改 binary + 重启）的风险量级不同，应该能分开关。
不复用 `update.enabled`——那个关掉的是后台 checker，而"我不要后台自动装，
但想要 dashboard 上手工点"是完全合理的组合。

`update.enabled: false` 时 checker 不跑 → `Status.latest` 永远为空 → 提示
不出现。若要支持"只在 dashboard 上按需查"，需要 GET 触发一次 on-demand
check；这有 GitHub 请求放大风险（每个客户端每次刷新），**列为 P3**，P1/P2
一律只读 Checker 的缓存结果。

## 4. 安全分析

**新增攻击面：几乎没有，但要说清楚为什么。** 持有 dashboard token 者能让
naozhi 下载并执行一个来自 GitHub 的新 binary——听起来像 RCE，但：

- 默认配置下**后台 Checker 已经在自动做同一件事**（`Mode: download` 是默认值），
  只是时机由定时器决定。dashboard 只是把时机交给操作者。
- 能装的**只有 `releases/latest`**（NG1），且必须 `semverGreater` 通过。攻击者
  无法指定任意 URL、任意 tag、任意版本，也无法降级。
- 信任锚不变：SHA-256 对 `checksums.txt`，可选 `NAOZHI_UPGRADE_PIN_SHA256`
  强 pin，`NAOZHI_UPGRADE_REQUIRE_PIN` 严格模式。dashboard 路径不绕过任何一层。
- 真正新增的是「**立即重启**」这个能力：能被用来制造服务中断。因此
  (i) 需要二次确认；(ii) 端点接 `internal/ratelimit`（每 token 每分钟 1 次）；
  (iii) `dashboard_install: false` 可整体关闭。

**其他**：

- CSRF：F10，`auth` 中间件的 Origin 门自动覆盖 POST。
- 错误脱敏：`last_error` / `check_error` 会进 JSON 到浏览器。下载错误可能含
  URL 与本地路径；用 `textutil` 的脱敏原语过一遍，避免把 install 路径、
  临时目录、token 之类漏进前端。
- Body 上限：POST 走 `http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)` +
  `decodeJSONBody`（`DisallowUnknownFields`），与 `handleClearLabelOrigin` 一致。
- `writeJSON` / `writeOK`：走既有 helper，拿到 `nosniff` + `Cache-Control: no-store`
  （`dashboard_system.go` 的 R246-SEC-3 教训）。

## 5. 分阶段交付

**P1 — 只读提示（零风险，可独立 merge）**
`selfupdate.Status` + `Checker` 写入 + `GET /api/system/update` + chip 显示 +
预检。此时点击 chip 只弹出一个"运行 `naozhi upgrade` 升级"的说明气泡。
P1 单独就解决了"根本不知道有新版本"这个最大痛点。

**P2 — 一键安装**
`Checker.InstallLatest` + `installMu` + `POST apply` + popover 安装按钮 +
安装中轮询 + **§3.6 的 macOS 重启修复**（P2 的前置，否则 macOS 上装了不生效）。

**P3 — 可选增强**
WS 推进度（取代轮询）；`update.enabled: false` 下的 on-demand check；
下载字节进度条。均非必需。

## 6. 测试

**`internal/selfupdate`**

- `Status` 并发读写（`-race`：一个 goroutine 循环 `Snapshot`，另一个循环
  `notePhase`）。
- `InstallLatest` 单飞：两个并发调用，第二个必须拿到 `ErrInstallInProgress`
  而不是排队。
- `InstallLatest` 与后台 tick 的互斥（stub `latestRelease` + stub 下载）。
- `Status == nil` 时 `Checker` 行为与今天逐字节一致（现有 `checker_test.go`
  全部不改动即通过，是这条的天然回归门）。
- `DetectLaunchdLabel`：把 `launchctl list` 输出解析抽成纯函数，喂本机实测的
  真实文本（含 `com.naozhi.agent` 的 `"Program" = "/Users/.../naozhi"` 行）+
  找不到时回退到常量。
- `restartLaunchd` 的命令行拼装：断言 `kickstart -k gui/<uid>/<label>`，
  且不再出现 `unload`（防回归到 F5 的坏形态）。

**`internal/server`**

- GET 形状契约（跟 `stats_shape_test.go` / `sessions_shape_test.go` 同构）：
  字段名与类型锁死，`update_available` 由服务端判定。
- 未认证 GET/POST → 401；跨 Origin POST → CSRF 拒绝。
- `can_install == false` 时 POST → 409 且**不启动任何后台 goroutine**。
- `dashboard_install: false` → POST 403，GET 照常 200。
- 限流：连续 POST 第二次 → 429。
- routes.go 每加一行路由要 bump `tools/lint-server-handlers/exemptions.yaml`
  的 baseline（见 `ab29e61e` 的先例，+2 行）。

**静态资产契约**（`static_*_contract_test.go` 惯例）

- `#btn-update` 存在且默认带 `hidden`；`aria-label` 有中文文案；
  CSS 只用既有 token（grep 断言不出现新的 `--nz-*` 定义或裸色值）。

**手工验收（G-系列，本机有真实差值 F7）**

- G1 本机 v0.0.72 → chip 出现并显示 `v0.0.73`。
- G2 点击 → 确认 → 装上 → **launchd 真的重启**（`launchctl list com.naozhi.agent`
  的 PID 变化）→ 重连后 chip 消失、`current` = v0.0.73。**这是 F4/F5 修复的
  唯一有效验收**，单元测试证明不了。
- G3 把 binary chown 到 root 后：chip 仍显示，但 popover 给手工命令而非按钮。
- G4 Linux（生产节点）上的同一路径：`systemctl restart --no-block` 生效。
- G5 安装中断网 → `phase: failed` + `last_error` 可读，服务仍在旧版本上正常跑。

## 7. 风险与缓解

| 风险 | 缓解 |
|---|---|
| 新 binary 起不来，dashboard 也没了，操作者失去唯一入口 | `Replace` 保留 `.bak`；`Mode: auto` 的注释已确立"自重启路径永不删 backup"；popover 在触发重启前显示回滚命令（`cp <bak> <path> && chmod 0755 && <restart>`）供操作者截图/复制 |
| `kickstart -k` 在某些 macOS 版本或 domain 下行为不同 | 只在 `ServiceRunning()` 为真时调用；失败返回 error → `phase: failed` + 明确文案。plist `KeepAlive=true` 是二次保险（进程死了也会被拉起） |
| `DetectLaunchdLabel` 匹配错 label，重启了别的服务 | 匹配条件是 Program 路径 `EvalSymlinks` 后等于 `SelfPath()`——不可能匹配到非本进程的 job；匹配不到时回退常量（= 今天的行为，不会更差） |
| 轮询增加负载 | 60s / 客户端，纯内存读 + 60s 缓存的预检，无网络。安装中才提到 3s |
| 操作者在 turn 进行中点了升级，中断对话 | F9：shim 让 CLI 子进程跨重启存活。popover 文案如实说明"对话不会中断"，并列出当前 running 会话数供操作者判断 |
| `dev` build 被误替换 | `checkOnce` 已跳过 dev；预检把它列为第一条阻塞原因（§3.5） |

## 8. 结论

可行，且成本集中在**接线与 UI**，不在核心逻辑——下载/校验/替换/回滚全部复用。
真正的实现工作有三块：`Status` 共享对象、`Checker` 的单飞锁、以及 macOS 重启
链的两个修复（F4/F5）。第三块是本 RFC 最有价值的副产品：它同时修好了 CLI
`naozhi upgrade` 和后台 `Mode: auto` 在 macOS 上"装了但不生效"的既有缺陷。

建议按 P1 / P2 两个 PR 走：P1 零风险、独立有价值，可以先落地拿反馈。
