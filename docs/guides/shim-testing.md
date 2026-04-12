# Shim 测试与调试指南

> 本文档用于 shim 功能的手动测试和调试。

## 1. 构建

```bash
cd /home/ec2-user/workspace/naozhi
go build -o bin/naozhi ./cmd/naozhi
```

## 2. 单独测试 shim 进程

shim 是一个独立进程，可以脱离 naozhi 主服务单独启动和调试。

### 2.1 手动启动 shim

```bash
# 启动 shim，连接到一个真实的 claude CLI
./bin/naozhi shim run \
  --key "test:d:user:general" \
  --socket "/tmp/naozhi-test-shim.sock" \
  --state-file "/tmp/naozhi-test-shim.json" \
  --buffer-size 100 \
  --idle-timeout 10m \
  --watchdog-timeout 5m \
  --cli-path ~/.local/bin/claude \
  --cli-arg '-p' \
  --cli-arg '--output-format' --cli-arg 'stream-json' \
  --cli-arg '--input-format' --cli-arg 'stream-json' \
  --cli-arg '--verbose' \
  --cli-arg '--setting-sources' --cli-arg '' \
  --cli-arg '--dangerously-skip-permissions' \
  --cli-arg '--model' --cli-arg 'sonnet' \
  --cwd /tmp
```

shim 启动后会输出一行 JSON 到 stdout：
```json
{"status":"ready","pid":12345,"token":"base64-token-here"}
```

然后关闭 stdout/stdin，在后台运行。

### 2.2 用 socat 手动连接 shim

```bash
# 连接到 shim 的 unix socket
socat - UNIX-CONNECT:/tmp/naozhi-test-shim.sock

# 发送 attach 消息 (用上面输出的 token)
{"type":"attach","last_seq":0,"token":"<上面的 token>"}

# shim 回复 hello + replay_done
# 然后可以发消息:
{"type":"write","line":"{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"hello\"}}"}

# 中断当前生成:
{"type":"interrupt"}

# 心跳:
{"type":"ping"}

# 断开但不杀 shim:
{"type":"detach"}

# 关闭 shim:
{"type":"shutdown"}
```

### 2.3 用 cat 替代 claude CLI 测试协议

如果不想消耗 API 额度，可以用 `cat` 作为 CLI：

```bash
./bin/naozhi shim run \
  --key "test:d:user:general" \
  --socket "/tmp/naozhi-test-shim.sock" \
  --state-file "/tmp/naozhi-test-shim.json" \
  --idle-timeout 5m \
  --cli-path cat \
  --cwd /tmp
```

`cat` 会将 stdin 原样回显到 stdout。发送 `write` 消息后，shim 会将写入的内容回显为 `stdout` 消息。

## 3. 管理命令

### 3.1 列出活跃 shim

```bash
./bin/naozhi shim list
# 或指定 state 目录:
./bin/naozhi shim list --state-dir ~/.naozhi/shims
```

输出示例：
```
SHIM   CLI    ALIVE KEY                                      SESSION
12345  23456  yes   feishu:d:alice:general                    sess_abc123...
12346  23457  yes   feishu:d:bob:code-reviewer                sess_def456...

2 shim(s)
```

### 3.2 停止 shim

```bash
# 停止特定 session 的 shim
./bin/naozhi shim stop --key "feishu:d:alice:general"

# 停止所有 shim
./bin/naozhi shim stop --all
```

## 4. 端到端测试：重启不中断

这是核心场景。验证步骤：

### 4.1 准备 config.yaml

在现有 `config.yaml` 中加入 shim 配置（可选，有默认值）：

```yaml
session:
  shim:
    buffer_size: 10000
    max_buffer_bytes: "50MB"
    idle_timeout: "4h"
    disconnect_watchdog: "30m"
    max_shims: 6
    state_dir: "~/.naozhi/shims"
```

### 4.2 更新 systemd unit

`deploy/naozhi.service` 需要加两行：

```ini
[Service]
# ... 现有配置 ...
KillMode=process            # 只杀 naozhi 主进程，shim 不受影响
TimeoutStopSec=30           # 不再需要等 CLI 完成
```

### 4.3 测试步骤

```bash
# 1. 启动 naozhi
./bin/naozhi --config config.yaml

# 2. 通过飞书/Dashboard 发一个长任务（例如 "帮我 review 整个项目"）

# 3. 确认 shim 在运行
./bin/naozhi shim list

# 4. 重启 naozhi（模拟部署）
kill -TERM $(pgrep -f 'naozhi --config')

# 5. 检查 shim 仍然存活
./bin/naozhi shim list
# 应该看到同样的 shim，ALIVE=yes

# 6. 重新启动 naozhi
./bin/naozhi --config config.yaml

# 7. 观察日志：应看到 "session reconnected via shim" 
# 8. 飞书/Dashboard 收到完整回复，无中断
```

### 4.4 验证断连缓冲

```bash
# 1. 发一个长任务
# 2. 趁 claude 在回复时杀掉 naozhi
kill -TERM $(pgrep -f 'naozhi --config')

# 3. 等几秒（claude 继续工作，shim 缓冲事件）

# 4. 重启 naozhi
./bin/naozhi --config config.yaml

# 5. 日志应显示 "replayed" 事件数 > 0
# 6. 回复应完整送达
```

## 5. 调试技巧

### 5.1 查看 shim 状态文件

```bash
cat ~/.naozhi/shims/*.json | jq .
```

字段说明：
- `shim_pid`: shim 进程 PID
- `cli_pid`: claude CLI 进程 PID
- `cli_alive`: CLI 是否还在运行
- `session_id`: claude session ID
- `buffer_count`: 当前缓冲的 stdout 行数
- `last_connected_at`: naozhi 最后一次连接时间

### 5.2 检查 shim 进程

```bash
# 查看所有 shim 进程
ps aux | grep 'naozhi shim run'

# 查看 shim 的 session leader（setsid 确认）
ps -o pid,pgid,sid,cmd -p $(pgrep -f 'naozhi shim run')
# SID 应该等于 PID（独立 session leader）

# 查看 shim 的子进程（claude CLI）
pstree -p $(pgrep -f 'naozhi shim run')
```

### 5.3 查看 unix socket

```bash
# 列出 shim socket 文件
ls -la /run/user/$(id -u)/naozhi/shim-*.sock 2>/dev/null || \
ls -la ~/.naozhi/run/shim-*.sock 2>/dev/null

# 检查 socket 权限（应该是 srw------- 即 0600）
```

### 5.4 强制停止所有 shim（应急）

```bash
# 方式 1: 通过命令
./bin/naozhi shim stop --all

# 方式 2: 发 SIGUSR2 立即退出
pkill -USR2 -f 'naozhi shim run'

# 方式 3: kill（30s 后优雅退出）
pkill -TERM -f 'naozhi shim run'

# 方式 4: 强杀（最后手段）
pkill -9 -f 'naozhi shim run'
```

### 5.5 日志

shim 启动时将日志写到 stderr（JSON 格式），启动后 stderr 关闭。如果需要持久日志，可以在启动时重定向：

```bash
# 手动调试时保留日志：
./bin/naozhi shim run ... 2>/tmp/shim-debug.log &
```

naozhi 主进程的日志中会显示 shim 相关事件：
```
grep -i shim /path/to/naozhi.log
```

关键日志模式：
- `"shim discovery"` — 启动时扫描 shim
- `"session reconnected via shim"` — 成功重连
- `"shim reconnect failed"` — 重连失败
- `"orphan shim found"` — 发现无对应 session 的孤儿 shim
- `"shim config drifted"` — 配置变更导致旧 shim 被关闭
- `"heartbeat: shim unresponsive"` — 心跳超时

## 6. 常见问题排查

| 症状 | 可能原因 | 排查方法 |
|------|---------|---------|
| 重启后没有重连 | shim 已退出 | `naozhi shim list`，检查 state files |
| 重启后重连但丢事件 | buffer 溢出 | 增大 `buffer_size` / `max_buffer_bytes` |
| shim 启动失败 | socket 残留 | 删除 `~/.naozhi/run/shim-*.sock` |
| "max shims reached" | shim 数量超限 | `naozhi shim stop --all`，增大 `max_shims` |
| "shim config drifted" | config 变更后重连 | 正常行为：旧 shim 被关闭，新消息创建新 shim |
| "shim auth failed" | token 不匹配 | 通常是 state file 与 shim 不一致，重启 shim |
| heartbeat 超时 | shim 卡死 | 检查 shim 进程 CPU 使用，可能需要 `kill -USR2` |

## 7. 自动化测试

```bash
# 单元测试（含 race detector）
go test -race -count=1 ./internal/shim/... -v
go test -race -count=1 ./internal/cli/... -v

# 全量测试
go test -race -count=1 ./... -timeout 120s
```
