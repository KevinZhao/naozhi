# RFC: 配置热重载 —— 第一阶段（im_access / im_limits / log.level）

- Issue: #3437（索引 #3453）
- Status: Phase 1 随本 RFC 落地
- 前置: #3442（`im_access`）与 #3447（`im_limits`）已把各自的运行时状态放在
  `Dispatcher` 的 `atomic.Pointer` 后面（`SetAccessPolicy` / `SetBudgetGate` /
  `SetUserLimiter`），本 RFC 只是给它们一个重载入口

## 1. 问题

改配置要重启进程，重启会打断所有进行中的会话（README 部署节 WARN）。对
`im_access` / `im_limits` 这类安全与成本闸门尤其不可接受：封禁一个用户、调低一个
预算，都要先打断所有人的对话。#2538 落地的 `/health.config_sha256` + doctor
"restart required" 只是把问题可见化。

## 2. 目标 / 非目标

目标（Phase 1）：

1. 三个入口：`SIGHUP`、`POST /api/system/config/reload`（dashboard token）、
   `naozhi config reload`（走 HTTP）
2. 可热重载集合 **H** = `im_access`、`im_limits`、`log.level`
3. 重载后 `/health.config_sha256` / `config_loaded_at` 反映新文件，doctor 的
   config-drift 检查随之变绿
4. 其它字段变了只报告，不假装生效：返回 `restart_required: [sections...]`
5. 新文件校验失败 → 保留旧配置，返回错误，进程不受影响

非目标（后续）：

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
func HotSections() []string                     // {"im_access","im_limits","log.level"}
func (c *Config) HotChanged(next *Config) []string      // H 中哪些段不同
func (c *Config) RestartRequired(next *Config) []string // H 之外哪些顶层段不同
```

`RestartRequired` 按 `yaml` tag 遍历 `Config` 的顶层字段，跳过 H、跳过
`yaml:"-"` 的派生字段与 `Fingerprint`；`log` 段去掉 `level` 后比较。用
`reflect.DeepEqual` 比较每个顶层字段，结果是 yaml 段名列表，直接可读。

### 3.2 运行时层：`server.Server.ApplyHotConfig`

```go
type HotConfig struct {
    Access   *imauth.Policy
    IMLimits IMLimitsOptions
}
func (s *Server) ApplyHotConfig(h HotConfig)
```

只用已有的 `s.dispatcher` 与 `s.router`（不新增 Server 字段，struct_budget 不动）：
重建 budget gate / user limiter（和启动时同一个 builder），交给 Dispatcher 的
三个 setter。budget gate 重建会丢掉 15s 缓存，可接受。

`/health` 的指纹改为读一个共享的 `*server.ConfigFingerprint`（互斥锁保护的
sha/loadedAt），`ConfigOptions.Live` 可选注入；为空时退回原来的静态字段。

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

流程：`config.Load(path)` → 失败返回 err（旧配置不动）→
`applied = last.HotChanged(next)`、`restart = baseline.RestartRequired(next)` →
`level.Set`、`apply(HotConfig{...})`、`fp.Set(next.Fingerprint)` → `last = next`
→ 日志一行 `config reloaded applied=[...] restart_required=[...]`。

`restart_required` 对 **baseline** 比而不是对 `last` 比：进程实际在跑的是启动时
的 cli/session 配置，第二次 reload 也必须继续报告这个差异，直到真正重启。

`apply` 为什么是绑定而不是构造参数：server 需要 reload 函数（给 HTTP 端点），
reload 又需要 server（`ApplyHotConfig`）。`server.New` 之前先建 reloader 把
`Reload` 方法塞进 `ServerOptions.ConfigReload`，`New` 返回后绑定 `apply`。HTTP
在 `Start` 之后才监听、SIGHUP handler 之后才注册，所以绑定前没有调用者；
`Reload` 仍对 `apply == nil` 做保护。

### 3.4 入口

- `SIGHUP`：main 的 signal loop 多监听一个信号；`SIGTERM/SIGINT` 行为不变。
  shim 子进程已 `signal.Ignore(SIGHUP)`，不受影响
- `POST /api/system/config/reload`：放 `internal/dashboard/ext/system`（和
  update/apply 同族），走 `/api/*` 鉴权链；全局单桶限流（复用 update/apply 的
  `newUpdateApplyLimiter` 形状，1 次/10s），返回 `ReloadResult`；加载失败 → 422
  带错误文本（已 sanitize）；未接线（无 config 路径）→ 501
- `naozhi config reload [-addr] [-token]`：复用 doctor 的 addr/token 解析，
  POST 上述端点并打印结果；`restart_required` 非空时退出码 3，方便脚本判断

### 3.5 可观测性

- `naozhi_config_reload_total{result=ok|error}` expvar
- `/health.config_sha256` / `config_loaded_at` 即时更新
- 日志 `config reloaded` / `config reload failed`

## 4. 不变量

- 重载失败时进程状态与重载前逐字节相同（校验在 `config.Load` 内完成，应用阶段
  之前没有任何副作用）
- H 之外的字段永远不会被"部分应用"：`ApplyHotConfig` 只接收 H 的派生值
- 没有新的 `Server` 字段、没有核心包的 late setter（Dispatcher 的三个 setter 是
  数据 setter，#3442/#3447 已存在）

## 5. 测试

- config：`HotChanged` / `RestartRequired` 表驱动（只改 `log.level` → 只在 hot
  集；改 `log.format` → restart；改 `cli.model` → restart；改 `im_limits` →
  hot；派生 `yaml:"-"` 字段差异不计入）
- cmd：`configReloader.Reload` 用临时文件：首次 load 成 baseline；改
  `im_access` 后 reload → `apply` 收到新 policy、level 不变、applied=[im_access]；
  改 `cli.model` → restart_required=[cli]，第二次 reload 仍报 cli；写坏 YAML →
  err 且 `last` 不变、`apply` 未调用
- server：`ApplyHotConfig` 把新 policy 交给 dispatcher（用 `/help` 前后被拒/放行
  证明）；`ConfigFingerprint` 并发读写 `-race`
- system handler：200 结果形状、422 加载失败、501 未接线、限流 429
