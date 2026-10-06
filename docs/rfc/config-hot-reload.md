# RFC: 配置热重载 —— 第一阶段（im_access / im_rate_limit / log.level）

- Issue: #3437（索引 #3453）
- Status: Phase 1 随本 RFC 落地
- 前置: #3442（`im_access`）已把策略放在 `Dispatcher` 的 `atomic.Pointer` 后面
  （`SetAccessPolicy`）；#3528（`im_rate_limit`）的限流桶在本 RFC 落地时改为同款
  `atomic.Pointer` 并加 `SetRateLimit`。本 RFC 给它们一个重载入口；后续的 chat 预算
  （#3447 系列）接入时只需再加一个 setter

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
2. 可热重载集合 **H** = `im_access`、`im_rate_limit`、`log.level`
3. `/health.config_sha256` 的含义不变：进程完整反映这份文件。重载后没有
   `restart_required` 才前进到新文件，doctor 的 config-drift 随之变绿；否则指纹
   留在上一份完整应用的文件，`/health.config_restart_required` 列出待重启的段，
   doctor 照列
4. 其它字段变了只报告，不假装生效：返回 `restart_required: [sections...]`
5. 新文件校验失败 → 保留旧配置，返回错误，进程不受影响

非目标（后续）：

- 按设计保持 restart-only（#3437 收尾时写明）：`reverse_nodes`、`agents` /
  `agent_commands`、`access_profiles`、`cron.notify_default`；`cost.budget` 的上限
  另有后续 PR 做热重载
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
func HotSections() []string                     // {"im_access","im_rate_limit","log.level"}
func (c *Config) HotChanged(next *Config) []string      // H 中哪些段不同
func (c *Config) RestartRequired(next *Config) []string // H 之外哪些顶层段不同
```

`RestartRequired` 按 `yaml` tag 遍历 `Config` 的顶层字段，跳过 H、跳过
`yaml:"-"` 的派生字段与 `Fingerprint`；`log` 段去掉 `level` 后比较。用
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
sha/loadedAt/restartRequired），`ConfigOptions.Live` 可选注入；为空时退回原来的静态
字段。`Set` 只在 `restartRequired` 为空时更新 sha/loadedAt。

### 3.3 组合根：`cmd/naozhi/reload.go`

```go
type configReloader struct {
    path     string
    baseline *config.Config     // 进程启动时的配置：restart_required 永远对它比
    level    *slog.LevelVar     // setupLogging 改用 LevelVar
    fp       *server.ConfigFingerprint
    apply    func(server.HotConfig)   // = srv.ApplyHotConfig，srv 建好后绑定一次
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
退出码 4。

`restart_required` 对 **baseline** 比而不是对 `last` 比：进程实际在跑的是启动时
的 cli/session 配置，第二次 reload 也必须继续报告这个差异，直到真正重启。

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
  `config_restart_required` 列出待重启段
- 日志 `config reloaded` / `config reload failed`

## 4. 不变量

- 重载失败时进程状态与重载前逐字节相同（校验在 `config.Load` 内完成，应用阶段
  之前没有任何副作用）
- H 之外的字段永远不会被"部分应用"：`ApplyHotConfig` 只接收 H 的派生值
- 没有新的 `Server` 字段、没有核心包的 late setter（Dispatcher 的两个 setter 是
  数据 setter）

## 5. 测试

- config：`HotChanged` / `RestartRequired` 表驱动（只改 `log.level` → 只在 hot
  集；改 `log.format` → restart；改 `cli.model` → restart；改 `im_rate_limit` →
  hot；派生 `yaml:"-"` 字段差异不计入）
- cmd：`configReloader.Reload` 用临时文件：首次 load 成 baseline；改
  `im_access` 后 reload → `apply` 收到新 policy、level 不变、applied=[im_access]；
  改 `cli.model` → restart_required=[cli]，第二次 reload 仍报 cli；写坏 YAML →
  err 且 `last` 不变、`apply` 未调用
- cmd：两次重载重叠时读文件在锁内；只改 `log.level` 不重新应用限流；有
  restart_required 时指纹不前进；拼错 `im_access` 报 opened；`signalLoop` 对 SIGHUP
  只 reload；真实 SIGHUP 到达 reload；`setupLogging` 装的全局 logger 跟随 LevelVar
- server：`ApplyHotConfig` 把新 policy / 限流交给 dispatcher（用 `/help` 前后被拒/
  放行/限流证明），未列出的段不动；`ConfigFingerprint` 并发读写 `-race`
- system handler：200 结果形状、422 加载失败、501 未接线、限流 429
