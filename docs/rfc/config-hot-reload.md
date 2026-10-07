# RFC: 配置热重载 —— 第一阶段（im_access / im_rate_limit / log.level / cost.budget）

- Issue: #3437（索引 #3453）
- Status: Phase 1 随本 RFC 落地
- 前置: #3442（`im_access`）已把策略放在 `Dispatcher` 的 `atomic.Pointer` 后面
  （`SetAccessPolicy`）；#3528（`im_rate_limit`）的限流桶在本 RFC 落地时改为同款
  `atomic.Pointer` 并加 `SetRateLimit`；#3447 的 `budget.Gate` 上限同样改为
  `atomic.Pointer` 并加 `SetLimits`。本 RFC 给它们一个重载入口

## 1. 问题

改配置要重启进程。shim 能扛过重启（macOS launchd，或 Linux 配了 sudoers 加固），CLI
会话本身不丢；但重启会丢掉正在回复的 IM 消息（只有 cron 有跨重启的 run adoption），
IM 网关和 dashboard WebSocket 也要重连。对 `im_access` / `im_rate_limit` 这类安全与
成本闸门，封禁一个用户、调低一个限流就要付这个代价不划算。#2538 落地的
`/health.config_sha256` + doctor "restart required" 只是把问题可见化。

## 2. 目标 / 非目标

目标（Phase 1）：

1. 三个入口：`SIGHUP`、`POST /api/system/config/reload`（dashboard token）、
   `naozhi config reload`（走 HTTP）
2. 可热重载集合 **H** = `im_access`、`im_rate_limit`、`log.level`、`cost.budget`（§3.6）
3. `/health.config_sha256` 的含义不变：进程完整反映这份文件。重载后没有
   `restart_required` 才前进到新文件，doctor 的 config-drift 随之变绿；否则指纹
   留在上一份完整应用的文件，`/health.config_restart_required` 列出待重启的段，
   doctor 照列
4. 其它字段变了只报告，不假装生效：返回 `restart_required: [sections...]`
5. 新文件校验失败 → 保留旧配置，返回错误，进程不受影响

非目标（后续）：

- 按设计保持 restart-only（#3437 收尾时写明）：`reverse_nodes`、`agents` /
  `agent_commands`、`access_profiles`、`cron.notify_default`
- `agents` / `agent_commands`：Dispatcher 的 `knownAgentIDs` 等在构造时快照，
  KeyResolver 持有 agent defaults；要么都改成 atomic swap，要么只允许改
  `system_prompt`。另开 issue
- `reverse_nodes`：`node.ReverseServer` 在构造时哈希 token，且已连接的节点不应被
  踢；需要 `UpdateTokens` + 对已连接节点的策略决策
- `platforms.*` 凭据、`cli.*`、`session.*`、`server.addr`：都牵涉进程/连接生命周期，
  永远 restart-required
- fsnotify 自动重载：显式触发更可预期，先不做

## 3. 设计

### 3.1 配置层：两个纯函数

`internal/config/reload.go`：

```go
func HotSections() []string                     // {"im_access","im_rate_limit","log.level","cost.budget"}
func (c *Config) HotChanged(next *Config) []string      // H 中哪些段不同
func (c *Config) RestartRequired(next *Config) []string // H 之外哪些顶层段不同
```

`RestartRequired` 按 `yaml` tag 遍历 `Config` 的顶层字段，跳过 H、跳过
`yaml:"-"` 的派生字段与 `Fingerprint`；`log` 段去掉 `level`、`cost` 段去掉 `budget`
后比较，`cost.budget` 里运行中的闸门接不住的部分另报（§3.6）。用
`reflect.DeepEqual` 比较每个顶层字段，结果是 yaml 段名列表，直接可读。

### 3.2 运行时层：`server.Server.ApplyHotConfig`

```go
type HotConfig struct {
    Access    *imauth.Policy
    RateLimit dispatch.RateLimit
    Sections  []string // 本次要应用的段，其余保持运行值
}
func (s *Server) ApplyHotConfig(h HotConfig)
```

只用已有的 `s.dispatcher`（不新增 Server 字段，struct_budget 不动）：交给 Dispatcher
的两个 setter，且只调 `Sections` 列出的那个。限流桶重建会把所有发送者的令牌桶重新
填满，所以只在 `im_rate_limit` 真的变了时才重建，只改 `log.level` 的重载不碰它。

`/health` 的指纹改为读一个共享的 `*server.ConfigFingerprint`（互斥锁保护的
sha/loadedAt/restartRequired/readSHA），`ConfigOptions.Live` 可选注入；为空时退回原来的静态
字段。`Set` 只在 `restartRequired` 为空时更新 sha/loadedAt；readSHA 每次都记，作为
`/health.config_reloaded_sha256` 下发，doctor 拿它和磁盘比，待重启期间的后续改动也能报出（#3649）。

### 3.3 组合根：`cmd/naozhi/reload.go`

```go
type configReloader struct {
    path     string
    baseline *config.Config     // 进程启动时的配置：restart_required 永远对它比
    level    *slog.LevelVar     // setupLogging 改用 LevelVar
    fp       *server.ConfigFingerprint
    apply    func(server.HotConfig)   // = srv.ApplyHotConfig，srv 建好后绑定一次
    liveProfiles func() map[string]session.AccessProfile // 与 apply 一起绑定
    mu       sync.Mutex
    last     *config.Config     // 上次成功应用的配置：applied 对它比
}
func (r *configReloader) Reload(ctx) (config.ReloadResult, error)
```

流程（整段持有 `mu`，包括读文件：SIGHUP 不限流，与 HTTP 重载重叠时，后拿到锁的
那次读到的就是当时的文件，旧读取不会覆盖新文件）：`config.Load(path)` → 失败返回
err（旧配置不动）→ `applied = last.HotChanged(next)`、
`restart = baseline.RestartRequired(next)`、
`opened = baseline.IMAccessOpened(last, next)` → `level.Set`、applied 非空时
`apply(HotConfig{..., Sections: applied})`、`fp.Set(sha, loadedAt, restart)` →
`last = next` → 日志一行 `config reloaded applied=[...] restart_required=[...]`。

`opened` 是上一份配置限制、新文件对所有人开放的运行中平台。`config.Load` 忽略未知
键，把 `im_access` 拼错（`im_acess:`）等于删掉整段：重载成功、平台悄悄放开。这种
情况打 Error 日志，`ReloadResult.opened_platforms` 列出平台，`naozhi config reload`
退出码 4。`open_platforms` 则列出新文件对所有人开放的全部运行中平台（不论是不是这次
放开的），CLI 每次都打印：放开它的可能是一次只写日志的 SIGHUP。

`restart_required` 对 **baseline** 比而不是对 `last` 比：进程实际在跑的是启动时
的 cli/session 配置，第二次 reload 也必须继续报告这个差异，直到真正重启。唯一的
例外是 dashboard 运行时新建的 access profile（`POST /api/access-profiles` 写入
config.yaml 并注册进 live registry）：比较前把 registry 里 baseline 没有的 profile
补进 baseline（`running()`），否则它会一直被报成 `access_profiles` 待重启并冻住指纹；
之后在文件里改这个 profile 仍然报 `access_profiles`。

`apply` 为什么是绑定而不是构造参数：server 需要 reload 函数（给 HTTP 端点），
reload 又需要 server（`ApplyHotConfig`）。`server.New` 之前先建 reloader 把
`Reload` 方法塞进 `ServerOptions.ConfigReload`，`New` 返回后绑定 `apply`。HTTP
在 `Start` 之后才监听、SIGHUP handler 之后才注册，所以绑定前没有调用者；
`Reload` 仍对 `apply == nil` 做保护。

### 3.4 入口

- `SIGHUP`：`watchSignals` 多监听一个信号，交给 `signalLoop`：SIGHUP 只调 reload、
  永不 shutdown；`SIGTERM/SIGINT` 行为不变。此前没注册 SIGHUP，`kill -HUP` 会按默认
  动作结束进程，现在改为重载。shim 子进程已 `signal.Ignore(SIGHUP)`，不受影响
- `POST /api/system/config/reload`：放 `internal/dashboard/ext/system`（和
  update/apply 同族），走 `/api/*` 鉴权链；全局单桶限流（复用 update/apply 的
  `newUpdateApplyLimiter` 形状，1 次/10s），返回 `ReloadResult`；加载失败 → 422
  带错误文本（已 sanitize）；未接线（无 config 路径）→ 501
- `naozhi config reload [-addr] [-token]`：复用 doctor 的 addr/token 解析，
  POST 上述端点并打印结果；`restart_required` 非空时退出码 3，有 `opened_platforms`
  时退出码 4（优先于 3），方便脚本判断

### 3.5 可观测性

- `naozhi_config_reload_total{result=ok|error}` expvar
- `/health.config_sha256` / `config_loaded_at` 在没有待重启段时即时更新；
  `config_restart_required` 列出待重启段；`config_reloaded_sha256` 是最近一次读到的文件
- 日志 `config reloaded` / `config reload failed`

### 3.6 `cost.budget`

IM 和 cron 共用一个 `budget.Gate`，上限放在 `atomic.Pointer[Limits]` 后面，每次检查
只读一次。`HotChanged` 比较 `BudgetLimits()`（三档上限、`warn_ratio`、`action`），
变了就由 reloader 直接调 `gate.SetLimits`（闸门属于组合根，不经过 Server）。改上限
不清零当天已计的花费；上限全删掉时闸门仍在、放行一切，之后再加上限照样看到当天花费。
规整后的 limits 真变了才清掉当天的提醒标记（`Once`），新上限算新一轮：调高后同一天再
被拦的 cron 任务照样写一条历史并通知，IM 到新上限的 80% 也会再收到提醒行。

闸门接不住的两种情况报 `restart_required`：

- 启动时没配上限：`budget.Attach` 返回 nil，没订阅账本，不存在可调的闸门。新文件加上
  上限报 `cost.budget`，且不列进 applied
- 生效时区变了（`cost.budget.timezone`，或为空时跟随的 `cron.timezone`）：`Index` 按
  启动时区切天，报 `cost.budget.timezone`

## 4. 不变量

- 重载失败时进程状态与重载前逐字节相同（校验在 `config.Load` 内完成，应用阶段
  之前没有任何副作用）
- H 之外的字段永远不会被"部分应用"：`ApplyHotConfig` 只接收 H 的派生值
- 没有新的 `Server` 字段、没有核心包的 late setter（Dispatcher 的两个 setter 和
  `Gate.SetLimits` 都是数据 setter）

## 5. 测试

- config：`HotChanged` / `RestartRequired` 表驱动（只改 `log.level` → 只在 hot
  集；改 `log.format` → restart；改 `cli.model` → restart；改 `im_rate_limit` →
  hot；派生 `yaml:"-"` 字段差异不计入）；`cost.budget` 改上限 / action → hot，
  启动无上限时加上限、改时区 → restart，`cost` 其它字段 → restart
- budget：`SetLimits` 调低 / 改 action / 清空 / 再加回，及与检查并发（`-race`）
- cmd：`configReloader.Reload` 用临时文件：首次 load 成 baseline；改
  `im_access` 后 reload → `apply` 收到新 policy、level 不变、applied=[im_access]；
  改 `cli.model` → restart_required=[cli]，第二次 reload 仍报 cli；写坏 YAML →
  err 且 `last` 不变、`apply` 未调用
- cmd：两次重载重叠时读文件在锁内；只改 `log.level` 不重新应用限流；有
  restart_required 时指纹不前进；拼错 `im_access` 报 opened，再次重载仍报 open；
  dashboard 新建的 profile 不算待重启、之后改它才算；`signalLoop` 对 SIGHUP
  只 reload；真实 SIGHUP 到达 reload；`setupLogging` 装的全局 logger 跟随 LevelVar；
  调低 `cost.budget` 后真实闸门下一次检查即拒绝，没有闸门时加上限只报 restart
- server：`ApplyHotConfig` 把新 policy / 限流交给 dispatcher（用 `/help` 前后被拒/
  放行/限流证明），未列出的段不动；`ConfigFingerprint` 并发读写 `-race`
- system handler：200 结果形状、422 加载失败、501 未接线、限流 429
