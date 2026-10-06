# naozhi doctor

一条命令拉取 naozhi 实例的健康快照，覆盖从二进制、systemd、HTTP、认证到零停机重启（sudoers hardening）各层。适用于：

- **第一现场排障**：SSH 到宿主打 `naozhi doctor`，20 秒看完所有常见故障点
- **部署回归**：`make deploy` 后自动跑，任一项 `fail` 就退出非零
- **监控探活**：`--json` 每行一个 finding，drop 到 Datadog/Loki

## 用法

```bash
# 最简：用默认 127.0.0.1:8180 + 自动发现 token
naozhi doctor

# 自定义端口（多实例 / 端口转发）
naozhi doctor --addr http://127.0.0.1:9180

# 显式 token（避免从 ~/.naozhi/env 读）
NAOZHI_DASHBOARD_TOKEN=xxx naozhi doctor

# JSON 一行一条，monitoring-friendly
naozhi doctor --json | jq -r 'select(.level=="fail") | .detail'

# 短超时（CI smoke）
naozhi doctor --timeout 2s
```

## 检查项

| 类别 | 通过 | 警告 | 失败 |
|---|---|---|---|
| `binary` | 能解析自身路径 | 路径不可读 | - |
| `codesign` | 非 darwin；或签名身份固定（leaf / Developer ID），升级后 macOS 授权保留 | ad-hoc 签名，每次升级都会重新弹文件夹授权（见 [macos-codesign.md](macos-codesign.md)）/ 读不到签名 | - |
| `systemd` | `systemctl is-active = active` | 非 Linux 或 systemctl 不存在 | 服务不活跃 |
| `http /health` | 返回 200（有 token 时摘要 status/uptime/version） | - | 不可达 / 非 200 |
| `auth` | token 通过 `/api/sessions` 200 | 无 token / 响应码意外 | token 被 401/403；请求构造或发送失败 |
| `cli runtime` | 服务端找得到默认 CLI 二进制（`cli_available=true`，仅 stat） | - | `cli_available=false`，新会话起不来 |
| `platforms` | 每个能上报连接状态的平台都是 `connected`；不能上报的（如 feishu webhook 模式）列为 `registered`，只是注册，不代表已连上 | 没有任何平台（dashboard-only）；某平台 `connecting` / `disconnected` 不足 5 分钟（重连中，或服务端没给出持续时长）；未知状态 | 某平台 `connecting` / `disconnected` 已满 5 分钟；或 `failed`（需要运维处理：修复原因；适配器可能低频重试并自行恢复，否则需重启） |
| `eventlog writer` | `writer_alive=true`；或该子系统未启用（skipped） | - | `writer_alive=false`，事件没落盘 |
| `attachment tracker` | 同上 | - | `writer_alive=false`，附件元数据没记录 |
| `dispatch` | 有成功回复（显示多久前）/ 还没消息或刚启动 | 自启动起只有失败没有成功；或有只报 `registered` 的平台、启动超 10 分钟仍零条 IM 入站（这些平台可能没连上） | - |
| `config-drift` | 磁盘 `config.yaml` 的 sha256 与进程上报的 `config_sha256` 一致（显示前 12 位和 `loaded_at`）；无 token、配置读不出、进程不可达、`/health` 非 200 或 token 不被接受时 skipped | 不一致（not applied，显示 mtime 与 `loaded_at`，提示跑 `naozhi config reload`）；`/health` 有 `config_restart_required`（上次重载留下的待重启段，报 `restart required for: ...`，此时指纹停在上一份完整应用的文件）；进程不上报指纹（早于 #2538）或指纹格式不对；`/health` JSON 解析失败 | - |
| `pprof` | `/api/debug/pprof/` 200 | 无 token；403（远端调用 / hardening 生效）或意外码 | 请求构造或发送失败 |
| `expvar` | `/api/debug/vars` 200 且 payload 含 `naozhi_session_create_total` | 无 token；403（远端调用 / hardening 生效）或意外码 | 请求构造或发送失败；读 body 失败；200 但没有该计数器（路由挂错） |
| `state dir` | `~/.naozhi` 可写 | 目录不存在（首次运行） | 存在但不可写 / 非目录 |
| `cli backend <id>` | 配置的路径 `--version` 成功（显示版本与路径） | 非默认 backend 探测失败；或非默认 id 未注册（启动时跳过）；或默认 id 没有可用 runtime（未注册或不在 `cli.backends` 里），默认路由的会话改落到第一个已注册的 backend 且它探测成功 | 默认 backend 探测失败（没有健康的兄弟 backend 时启动直接拒绝；有则默认路由的会话起不来）；默认 id 没有可用 runtime，且没有任何已注册 backend 探测成功（启动拒绝），或兜底的那个 backend 探测失败 |
| `transcribe creds` | `transcribe.enabled` 时 AWS 凭证链取得到凭证（显示来源）；未启用则 skipped | 取不到凭证，语音消息会失败 | - |
| `transcribe ffmpeg` | 找得到 ffmpeg（`NAOZHI_FFMPEG_PATH` 优先，其次 `$PATH`）；未启用则 skipped | 找不到，ogg/flac/pcm 以外的语音格式转不了 | - |
| `zero-downtime` | `naozhi-shim-*.scope` 有 ≥1 | 0 个 scope（sudoers hardening 未生效） | systemctl list-units 失败 |
| `server security` | 没配 `dashboard_token`；`addr` 是 loopback；或 `trusted_proxy: true`；配置读不出时 skipped | 配了 `dashboard_token`、`addr` 非 loopback 且 `trusted_proxy: false`：放在 TLS 终结的反代后面时 dashboard cookie 不带 `Secure` | - |
| `im access` | 每个已配置的 IM 平台都有 `im_access` 条目（列出去重后的用户数、管理员数；`admin_users` 为空时写 `all admin`），或被 `default_deny` 拒绝；没配任何平台；配置读不出时 skipped | 某平台没有 `im_access` 条目且 `default_deny` 关闭：任何能给 bot 发消息的人都能在宿主机执行命令（与启动日志、`naozhi config check` 的告警同源） | - |

`cli runtime` 到 `dispatch` 五项和 `config-drift` 读的是同一次带 token 的 `GET /health`（整次 doctor 只发一次）。没有 token、token 被拒或 `/health` 不可达时，这五项各输出一行 `skipped (…)`，不计 fail。`platforms` 一行读 `/health` 的 `platforms`（每个平台的状态名）和 `platform_conn`（状态起始时间 `since`、最近一次错误）：整行取最差那个平台的级别，每个平台一段，最近错误只在未连上时显示，超过 120 字节截断并以 `...` 结尾（每段单独截断，一个平台的长错误不会挤掉其他平台）。状态持续时长按 `/health` 响应的 `Date` 头（服务端时钟）减 `since` 计算，不受两边时钟偏差影响；响应没有 `Date` 头时才退回本机时钟。5 分钟的宽限覆盖 feishu 长连接默认 2 分钟的重连间隔加抖动和 weixin 的 30 秒退避。discord 掉线期间会用 REST（`GET /users/@me`）探测 bot token：被拒（401/403）直接报 `failed`，其他错误作为最近错误显示；4013/4014（intent 不允许）这类网关关闭码探测不到，仍只显示 `disconnected`，靠 5 分钟宽限报出。`failed` 不代表适配器停止重试：discord 报 401/403 之后 discordgo 仍在重连，拒绝在 Discord 侧解除后会自行回到 `connected`；weixin 遇到 iLink -14 也会每小时继续轮询一次，token 恢复可用后同样回到 `connected`。只报 `registered` 的平台（适配器观察不到连接，或服务端早于连接状态上报）没有连接状态，「平台没连上」只能从 `dispatch` 的入站计数推断，所有平台都能上报时 `dispatch` 不再做这条推断：这个计数不含斜杠命令，只收到 `/help` 之类命令（或确实没人发消息）的安静 bot 启动 10 分钟后也会报这条 warn（不影响退出码）；启动时长同样按 `Date` 头减服务端的 `config_loaded_at` 计算，没有 `Date` 头时才退回本机时钟。

`cli backend <id>` 对 `cli.backends`（或单 backend 的 `cli.path`）里每一项跑一遍启动时同款 `--version` 探测，读的是配置里的路径（没配路径时按启动的解析顺序找），不是 `$PATH` 上的默认二进制。下方 `=== CLI Backends ===` 段直接复用这次探测的结果，每个 backend 一次运行只探测一次；只有配置读不出时，这一段才改为列出 `$PATH` 上能找到的 backend。`cli backend`、`transcribe creds`、`transcribe ffmpeg` 都按**运行 doctor 的用户**解析：路径里的 `~`、`$PATH`、AWS 凭证链都可能和 launchd / systemd 下的服务用户不同。`transcribe creds` 和启动时一样，先把 `~/.claude/settings.json` 的 `env`（同一套过滤）补进环境里再查凭证链；但 systemd 的 `Environment=` 和 launchd plist 的 `EnvironmentVariables` 不在 doctor 的环境里。准确结论要以服务用户身份、带着服务的环境变量跑 doctor。配置读不出时这几项输出 `skipped (config not loaded)`，由 `naozhi config check` 负责报错。整次 doctor 只读一次配置。

## 退出码

- `0` — 全部 pass 或仅 warn
- `1` — 至少 1 个 fail（CI 友好，`|| exit 1` 直接传播）
- `2` — flag 解析错误

## 示例

生产正常态：

```
$ naozhi doctor
✓ binary                 /home/ec2-user/naozhi/bin/naozhi · version=v0.0.3-31-g8b832fa · linux/arm64
✓ systemd                active · MainPID=2730834 · NRestarts=0 · ActiveEnterTimestamp=Wed 2026-04-29 19:32:51 UTC
✓ http /health           status=ok uptime=1h3m28s version=v0.0.3-31-g8b832fa-dirty
✓ auth                   token accepted (/api/sessions 200)
✓ cli runtime            the server finds its default CLI binary (cli_available=true)
✓ platforms              feishu connected for 1h3m20s
✓ eventlog writer        writer alive (queue 0/1024, dropped 0)
✓ attachment tracker     writer alive (queue 0/256, dropped 0)
✓ dispatch               last successful reply 4m12s ago · messages=37 reply_errors=0 send_fails=0
✓ pprof                  reachable at http://127.0.0.1:8180/api/debug/pprof/
✓ state dir              /home/ec2-user/.naozhi writable
✓ cli backend claude     2.1.288 at /home/ec2-user/.local/bin/claude
✓ transcribe creds       AWS credentials from EC2RoleProvider
✓ transcribe ffmpeg      /usr/bin/ffmpeg
✓ zero-downtime          2 shim scope(s) active (sudoers hardening is working)
✓ im access              feishu 3 user(s), 1 admin(s)
```

服务 down：

```
$ naozhi doctor
✓ binary                 /home/ec2-user/naozhi/bin/naozhi · version=dev · linux/arm64
✗ systemd                naozhi.service is "inactive" (expected active)
✗ http /health           http://127.0.0.1:8180/health unreachable: dial tcp 127.0.0.1:8180: connect: connection refused
⚠ auth                   no token; auth-scoped checks skipped
...
```

退出码 1 —— `make deploy && naozhi doctor || exit 1` 直接失败出局。

## Token 查找顺序

1. `--token` flag
2. `NAOZHI_DASHBOARD_TOKEN` 环境变量
3. `DASHBOARD_TOKEN` 环境变量（legacy 别名）
4. `~/.naozhi/env` 文件扫描 `NAOZHI_DASHBOARD_TOKEN=` 或 `DASHBOARD_TOKEN=` 行

都没有 → `auth` / `pprof` 检查降级为 warn，`cli runtime` 等鉴权段检查输出 skipped（都不算 fail）。

## 非零停机场景的 `zero-downtime` 解读

`zero-downtime` 检查通过 `systemctl list-units --type=scope | grep naozhi-shim-` 数 scope 数量。

- **有 scope**：说明 sudoers hardening 生效（或曾经生效，scope 一旦建立就持续存在直到 shim 退出）。[`docs/ops/sudoers-hardening.md`](sudoers-hardening.md) 是否安装决定未来 restart 能否新增 scope
- **0 scope**：sudoers 没配 / 尚无任何活跃 shim。发一条消息让 shim spawn 一次再跑 doctor 即可分辨两者
- **`systemctl list-units` 失败**：通常非 Linux 或 systemd 用户空间不可达

## 不在本期

- 深度探测（JSON 结构校验 /api/sessions 响应形状、cron job 执行历史）—— doctor 保持"顶层快照"语义，深度分析走 pprof + journalctl
- 自动拉 pprof heap/goroutine snapshot —— 想要就用 [`docs/ops/pprof.md`](pprof.md)
- 监控级集成（Prometheus exporter）—— 单独立项
