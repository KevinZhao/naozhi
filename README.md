# Naozhi

AI CLI Agent 即时通信网关。将 Claude Code / Kiro CLI 的完整 agent 能力（工具调用、代码编辑、MCP servers）通过 IM 平台暴露给用户。

## 工作原理

Naozhi 是一个 Go 编写的消息路由薄层。它不重造 agent loop，而是 spawn 本机 AI CLI 作为长生命周期子进程，通过可插拔的协议接口进行多轮对话：
- **ClaudeProtocol**: stream-json NDJSON（Claude CLI）
- **ACPProtocol**: JSON-RPC 2.0 Agent Client Protocol（Kiro CLI）

```
飞书 / Slack / Discord / 微信
       | WebSocket / Socket Mode / Gateway / HTTP 长轮询
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

- [Claude Code CLI](https://claude.ai/code) 已安装并配置认证（Bedrock / API key / claude.ai）

### 安装

从 [GitHub Releases](../../releases) 下载对应平台的预编译二进制，或从源码编译:

```bash
go build -o bin/naozhi ./cmd/naozhi/
```

### 微信（最简方式）

只需两步:

```bash
# 1. 交互式扫码，自动获取 token 并生成配置
naozhi setup weixin

# 2. 启动
naozhi --config ~/.naozhi/config.yaml
```

`setup weixin` 会自动: 获取二维码 → 打印扫码链接 → 等待确认 → 写入 `~/.naozhi/config.yaml`。

需要两个微信号——一个登录为 bot，另一个发消息测试。Token 失效后重新运行 `setup weixin` 即可。

### 飞书

1. 飞书开放平台 → 创建企业自建应用 → 开启"机器人"能力
2. 权限: `im:message`, `im:message:send_as_bot`, `im:message:patch`
3. 事件订阅:
   - **WebSocket 模式（推荐）**: 配置方式选择"使用长连接接收事件"，订阅 `im.message.receive_v1`
   - **Webhook 模式**: 请求地址填 `https://your-domain/webhook/feishu`，订阅 `im.message.receive_v1`
4. 发布应用版本
5. 配置凭据并启动:
   ```bash
   export FEISHU_APP_ID=your_app_id
   export FEISHU_APP_SECRET=your_app_secret
   naozhi --config config.yaml
   ```

### Slack

1. [api.slack.com/apps](https://api.slack.com/apps) → Create New App
2. 开启 Socket Mode，获取 App-Level Token（`xapp-...`）
3. Bot Token Scopes: `chat:write`, `app_mentions:read`
4. Event Subscriptions: `message.im`, `app_mention`

### Discord

1. [discord.com/developers](https://discord.com/developers/applications) → New Application → Bot
2. 开启 Message Content Intent
3. 获取 Bot Token，邀请到服务器

### 运行

```bash
naozhi --config ~/.naozhi/config.yaml
```

健康检查: `curl http://localhost:8180/health`

所有平台均支持 WebSocket / 长轮询模式，**无需公网 IP**。

## CLI 命令

| 命令 | 说明 |
|------|------|
| `naozhi --config <path>` | 启动网关 |
| `naozhi setup weixin` | 交互式微信扫码配置 |
| `naozhi version` | 显示版本号 |

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
  # weixin:
  #   token: "${WEIXIN_BOT_TOKEN}"
```

## 部署

### 本地运行

所有平台均支持 WebSocket / 长轮询，直接运行即可，**无需公网 IP**。

微信最简流程:
```bash
naozhi setup weixin                    # 扫码 + 自动生成配置
naozhi --config ~/.naozhi/config.yaml  # 启动
```

### 服务器部署（EC2 / systemd）

#### 前置条件

- EC2 实例（推荐 t4g.small ARM64）
- Claude Code CLI 已安装在目标机器 (`~/.local/bin/claude`)
- Bedrock 认证：通过 IAM 角色（无需 AKSK），EC2 需访问 `bedrock-runtime` VPC endpoint
- SSM Agent 已安装（用于远程部署命令）

#### 部署步骤

```bash
# 1. 设置 deploy/deploy.sh 中的 INSTANCE_ID
vim deploy/deploy.sh  # 填入目标 EC2 Instance ID

# 2. 在目标机器上创建环境变量文件（仅首次）
#    通过 SSM 登录目标机器：
aws ssm start-session --target <INSTANCE_ID>
#    创建凭据文件：
cat > ~/.naozhi/env << 'EOF'
IM_APP_ID=your_feishu_app_id
IM_APP_SECRET=your_feishu_app_secret
IM_VERIFICATION_TOKEN=your_verification_token
IM_ENCRYPT_KEY=your_encrypt_key
WEIXIN_BOT_TOKEN=your_weixin_bot_token
EOF
chmod 600 ~/.naozhi/env

# 3. 一键部署（本机执行：交叉编译 + S3 上传 + EC2 安装 + 重启服务）
./deploy/deploy.sh deploy

# 4. 查看状态 / 日志
./deploy/deploy.sh status
./deploy/deploy.sh logs
```

#### systemd 服务

部署脚本会自动安装 `deploy/naozhi.service`，关键配置:
- Bedrock 认证环境变量（`CLAUDE_CODE_USE_BEDROCK=1`、`AWS_REGION`、模型 ID 等）
- 凭据从 `~/.naozhi/env` 加载（`EnvironmentFile`）
- 崩溃自动重启（`Restart=always`）

手动管理:
```bash
sudo systemctl restart naozhi
sudo systemctl status naozhi
journalctl -u naozhi -f          # 实时日志
```

#### 生产架构

```
CloudFront → ALB (SG: CloudFront-only) → EC2 :8180 → systemd
```

- ALB 安全组仅允许 CloudFront 前缀列表
- EC2 通过 IAM 角色认证 Bedrock（无 AKSK）
- EC2 安全组需放行到 `bedrock-runtime` VPC endpoint

### 微信注意事项

- **Token 失效**: iLink Bot API 的 token 有效期未知，失效后重新运行 `naozhi setup weixin`
- **服务器上更新 token**: 更新 `~/.naozhi/env` 中的 `WEIXIN_BOT_TOKEN` 后 `sudo systemctl restart naozhi`
- **限制**: 仅支持私聊（1v1），不支持群聊；不支持消息编辑（流式追加）
- **协议来源**: iLink Bot API 无官方文档，通过逆向腾讯 OpenClaw 微信插件获得

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
    weixin/                   微信 (iLink Bot HTTP 长轮询)
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
