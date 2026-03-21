# Naozhi

AI CLI Agent 即时通信网关。将 Claude Code / Kiro CLI 的完整 agent 能力（工具调用、代码编辑、MCP servers）通过 IM 平台暴露给用户。

## 工作原理

Naozhi 是一个 Go 编写的消息路由薄层。它不重造 agent loop，而是 spawn 本机 AI CLI 作为长生命周期子进程，通过可插拔的协议接口进行多轮对话：
- **ClaudeProtocol**: stream-json NDJSON（Claude CLI）
- **ACPProtocol**: JSON-RPC 2.0 Agent Client Protocol（Kiro CLI）

```
飞书 / Slack / Discord
       | WebSocket / Socket Mode / Gateway
       v
  Naozhi Gateway (Go)
       | stdin/stdout (stream-json / JSON-RPC)
       v
  AI CLI (长生命周期进程)
    + 完整工具链 (Bash/Read/Edit/...)
    + MCP servers
    + 自定义 system prompt per agent
```

每个用户/群聊 + agent 组合维护独立的 CLI 进程和对话上下文。进程空闲 30 分钟后回收，下次消息自动 resume 恢复。

默认使用 WebSocket 长连接接收消息，**无需公网 IP、域名或端口转发**。

## 快速开始

### 前置条件

- Go 1.25+
- [Claude Code CLI](https://claude.ai/code) 已安装并配置认证（Bedrock / API key / claude.ai）
- 至少一个 IM 平台应用（飞书 / Slack / Discord）

### 安装

```bash
# 编译
go build -o bin/naozhi ./cmd/naozhi/

# 交叉编译全平台
make release    # 输出到 dist/ (linux/darwin/windows × amd64/arm64)
```

预编译二进制可从 [GitHub Releases](../../releases) 下载。

### 平台配置

#### 飞书

1. 飞书开放平台 → 创建企业自建应用 → 开启"机器人"能力
2. 权限: `im:message`, `im:message:send_as_bot`, `im:message:patch`
3. 事件订阅:
   - **WebSocket 模式（推荐）**: 配置方式选择"使用长连接接收事件"，订阅 `im.message.receive_v1`
   - **Webhook 模式**: 请求地址填 `https://your-domain/webhook/feishu`，订阅 `im.message.receive_v1`
4. 发布应用版本

#### Slack

1. [api.slack.com/apps](https://api.slack.com/apps) → Create New App
2. 开启 Socket Mode，获取 App-Level Token（`xapp-...`）
3. Bot Token Scopes: `chat:write`, `app_mentions:read`
4. Event Subscriptions: `message.im`, `app_mention`

#### Discord

1. [discord.com/developers](https://discord.com/developers/applications) → New Application → Bot
2. 开启 Message Content Intent
3. 获取 Bot Token，邀请到服务器

### 配置

```bash
cp config.yaml ~/.naozhi/config.yaml
# 通过环境变量注入凭据
export FEISHU_APP_ID=your_app_id
export FEISHU_APP_SECRET=your_app_secret
```

### 运行

```bash
bin/naozhi --config ~/.naozhi/config.yaml
```

健康检查: `curl http://localhost:8180/health`

## 用户命令

| 命令 | 说明 |
|------|------|
| 普通消息 | 发送给默认 agent，保持多轮上下文 |
| `/review <text>` | 路由到 code-reviewer agent |
| `/research <text>` | 路由到 researcher agent |
| `/new` | 重置默认 agent 对话 |
| `/new review` | 重置 code-reviewer agent 对话 |
| `/cron add "@every 30m" 检查状态` | 创建定时任务 |
| `/cron list` | 查看当前聊天的定时任务 |
| `/cron del <id>` | 删除定时任务 |
| `/cron pause/resume <id>` | 暂停/恢复定时任务 |

Agent 命令通过 `agent_commands` 配置映射，可自定义。

## 配置项

```yaml
server:
  addr: ":8180"

cli:
  backend: claude                        # "claude" | "kiro"
  path: "~/.local/bin/claude"
  model: "sonnet"                        # sonnet / opus / haiku
  args:
    - "--dangerously-skip-permissions"

session:
  max_procs: 3                           # 最大并发 CLI 进程
  ttl: "30m"                             # 空闲回收超时
  no_output_timeout: "2m"                # 无输出 watchdog
  total_timeout: "5m"                    # 单轮总超时 watchdog
  store_path: "~/.naozhi/sessions.json"  # session 持久化路径

agents:
  code-reviewer:
    model: "sonnet"
    args: ['--append-system-prompt', 'You are a code reviewer...']
  researcher:
    model: "opus"
    args: ['--append-system-prompt', 'You are a research assistant...']

agent_commands:
  review: code-reviewer
  research: researcher

cron:
  store_path: "~/.naozhi/cron.json"
  max_jobs: 50
  execution_timeout: "5m"

platforms:
  feishu:
    app_id: "${FEISHU_APP_ID}"
    app_secret: "${FEISHU_APP_SECRET}"
    # connection_mode: websocket          # "websocket"(默认) | "webhook"
    max_reply_length: 4000
  # slack:
  #   bot_token: "${SLACK_BOT_TOKEN}"
  #   app_token: "${SLACK_APP_TOKEN}"
  # discord:
  #   bot_token: "${DISCORD_BOT_TOKEN}"
```

## 部署

### 本地运行

WebSocket / Socket Mode 下直接运行即可，无需公网 IP。

### 服务器部署（EC2 / systemd）

```bash
./deploy/deploy.sh deploy    # 构建 + 推送 + 启动
./deploy/deploy.sh status    # 查看服务状态
./deploy/deploy.sh logs      # 查看日志
```

### 发布

```bash
git tag v0.1.0
git push origin v0.1.0      # GitHub Actions 自动构建 6 平台二进制 + Release
```

## 项目结构

```
cmd/naozhi/main.go           入口
internal/
  cli/                        CLI 进程管理 + Protocol 接口 (Claude/ACP)
  session/                    Session 路由 + 并发控制 + TTL 回收 + 持久化
  platform/                   IM 平台接口
    feishu/                   飞书 (WebSocket + Webhook)
    slack/                    Slack (Socket Mode)
    discord/                  Discord (Gateway WebSocket)
  config/                     YAML 配置加载 + 环境变量展开
  server/                     HTTP server + 消息处理 + Agent 路由
  cron/                       定时任务调度器
  routing/                    命令解析 (共享)
  pathutil/                   路径工具 (共享)
deploy/                       systemd + 部署脚本
```

## 设计文档

完整架构设计见 [DESIGN.md](DESIGN.md)。

## License

[BSL 1.1](LICENSE) — 源码可读可改，个人和非生产用途免费。生产环境商用需获得授权。2030-03-21 后自动转为 Apache 2.0。
