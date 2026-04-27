<div align="center">

# Naozhi 脑汁

**把 Claude Code 的完整 agent 能力装进你的聊天窗口**

在飞书 / Slack / Discord / 微信中直接使用 Claude Code —— 工具调用、代码编辑、MCP servers，一个都不少。

[快速开始](#快速开始) · [功能一览](#功能一览) · [Dashboard](#实时-dashboard) · [部署指南](#部署)

</div>

---

## 为什么选 Naozhi？

大多数 "AI 聊天机器人" 只是 API wrapper。Naozhi 不同 —— 它直接 spawn 本机 Claude Code CLI 作为长生命周期子进程，通过 stdin/stdout 进行原生协议通信，**保留 CLI 的全部能力**：

- 读写文件、执行 Bash、Git 操作、子 agent 编排
- 所有已配置的 MCP servers
- 自定义 system prompt 和 per-agent 模型选择

```mermaid
graph TD
    IM["飞书 / Slack / Discord / 微信"]
    GW["Naozhi Gateway<br/>(Go, 单二进制)"]
    CLI["Claude Code CLI<br/>(长生命周期进程)"]
    TOOLS["Bash · Read · Edit · Grep<br/>Glob · Agent · MCP servers"]

    IM -- "WebSocket / Socket Mode<br/>Gateway / HTTP 长轮询" --> GW
    GW -- "stdin/stdout<br/>(stream-json / ACP JSON-RPC)" --> CLI
    CLI --- TOOLS

    style IM fill:#e8f4fd,stroke:#4a90d9
    style GW fill:#fff3cd,stroke:#d4a017
    style CLI fill:#d4edda,stroke:#28a745
    style TOOLS fill:#f8f9fa,stroke:#6c757d,stroke-dasharray: 5 5
```

### 核心优势

| | 特性 | 说明 |
|---|---|---|
| **0** | 零基础设施 | 所有平台均支持 WebSocket / 长轮询。无需公网 IP、域名或端口转发 |
| **1** | 完整 Agent 能力 | 不是 API wrapper，是真正的 Claude Code CLI —— 工具调用、代码编辑、MCP 一个不少 |
| **2** | 会话自动恢复 | 进程崩溃/回收后自动 `--resume`，对话上下文完整保留 |
| **3** | 单二进制部署 | Go 编译，无容器、无依赖。6 平台预编译 release |
| **4** | 实时 Dashboard | 浏览器实时查看所有会话、事件流、费用统计 |
| **5** | 多节点 NAT 穿越 | 远程机器反向拨入主节点，统一管理多台工作站 |

---

## 功能一览

### IM 平台接入

| 平台 | 接入方式 | 私聊 | 群聊 | 消息编辑 |
|------|----------|------|------|----------|
| **飞书** | WebSocket 长连接 / Webhook | ✓ | ✓ | ✓ 流式更新 |
| **Slack** | Socket Mode | ✓ | ✓ (mention) | — |
| **Discord** | Gateway WebSocket | ✓ | ✓ (mention) | — |
| **微信** | HTTP 长轮询 (iLink Bot) | ✓ | — | — |

所有平台开箱即用，**无需公网 IP**。

### 多 Agent 路由

一个群聊可同时使用多个专业 agent，各自保持独立上下文：

```
/review 帮我看看这段代码有没有安全问题    → code-reviewer agent (sonnet)
/research Rust async runtime 对比分析     → researcher agent (opus)
普通消息会路由到默认 agent                 → general agent
```

Agent 命令、模型、system prompt 均可在 `config.yaml` 中自定义。

### 会话生命周期

```mermaid
graph LR
    MSG(["消息到达"]) --> FIND["查找/创建<br/>CLI 进程"]
    FIND --> SEND["Send"]
    SEND --> WAIT["等待结果"]
    WAIT --> REPLY(["回复用户"])

    WAIT -. "空闲 30min" .-> RECYCLE["进程回收<br/>(Close)"]
    RECYCLE -. "下次消息" .-> RESUME["自动 --resume<br/>恢复上下文"]
    RESUME --> SEND

    style MSG fill:#e8f4fd,stroke:#4a90d9
    style REPLY fill:#d4edda,stroke:#28a745
    style RECYCLE fill:#f8d7da,stroke:#dc3545
    style RESUME fill:#fff3cd,stroke:#d4a017
```

- **Watchdog 双超时**: 无输出超时 (2min) + 总耗时超时 (5min)，防止进程挂起
- **容量管理**: 可配置最大并发进程数，满载时自动驱逐最久空闲会话
- **中断恢复**: 用户发送新消息时自动中断正在运行的 turn (SIGINT)

### 定时任务 (Cron)

在聊天中直接管理定时任务：

```
/cron add "@every 30m" 检查 staging 环境的健康状态
/cron add "0 9 * * 1-5" /review 扫描最近的 open PRs
/cron list
/cron pause <id>
```

- 标准 cron 表达式 + `@every` 语法
- 每 chat 10 个 / 全局 50 个配额
- 执行结果自动回推到聊天

### 语音转文字

发送语音消息即可与 Claude 对话。基于 Amazon Transcribe Streaming：

- 支持 OGG / FLAC / MP3 / WAV / M4A / AMR 等格式
- 不支持的格式自动通过 ffmpeg 转码为 PCM
- 多语言自动检测（可配置语言列表）
- 转码与上传并发执行，低延迟

### 项目规划器

将聊天绑定到代码项目，获得专属的 planner session：

```
/project my-app          → 绑定到 my-app 项目
普通消息自动路由到 planner → 长期上下文，不受 TTL 回收
/project off             → 解绑
```

- 自动发现 `projects_root` 下含 `CLAUDE.md` 的目录
- planner session 免驱逐、免 TTL，保持长期对话上下文
- 每个项目可配置独立的模型和 system prompt

### 实时 Dashboard

浏览器访问 `http://localhost:8180` 即可查看：

- 所有会话列表（运行中 / 就绪 / 挂起）+ 实时状态更新
- 事件流实时推送（thinking、tool_use、agent 调度、结果）
- 直接在 Dashboard 发送消息、上传文件（图片）
- 发现并接管外部 Claude CLI 进程（一键 Take Over）
- 费用统计（per-session 累计 cost）
- 项目管理（绑定配置、planner 重启）
- 定时任务管理（创建、暂停、删除）

### 多节点分布式

NAT 后面的工作站也能统一管理：

```mermaid
graph RL
    A["Node A<br/>(NAT 内)<br/>本地 Claude CLI"] -- "WebSocket 反向拨入" --> P
    B["Node B<br/>(NAT 内)<br/>本地 Claude CLI"] -- "WebSocket 反向拨入" --> P
    P["Primary<br/>(公网)<br/>Dashboard 统一入口"]

    style P fill:#fff3cd,stroke:#d4a017
    style A fill:#e8f4fd,stroke:#4a90d9
    style B fill:#e8f4fd,stroke:#4a90d9
```

- 远程节点主动拨入 Primary 的 `/ws-node` WebSocket 端点
- Token 认证 + 自动重连（指数退避 1-30s）
- Dashboard 统一展示所有节点的会话
- 支持远程会话订阅、消息发送、进程接管

### 外部进程发现

自动扫描本机运行的 Claude CLI 进程：

- 扫描 `~/.claude/sessions/` 识别非 Naozhi 管理的 Claude 实例
- 显示会话概要（最后一条用户消息预览）
- 一键接管：SIGTERM → 等待 → `--resume` 接入 Naozhi 管理
- 自动首消息接管：检测到外部 session 时自动恢复

---

## 快速开始

### 前置条件

- [Claude Code CLI](https://claude.ai/code) 已安装并配置认证

### 安装

**推荐（macOS / Linux，零依赖）**：

```bash
curl -fsSL https://raw.githubusercontent.com/KevinZhao/naozhi/master/install.sh | bash
```

脚本会按当前 OS/架构从 [GitHub Releases](../../releases) 拉对应二进制、校验 SHA256、装到 `~/.local/bin/naozhi`，全程不用 sudo。支持固定版本：

```bash
curl -fsSL https://raw.githubusercontent.com/KevinZhao/naozhi/master/install.sh \
  | NAOZHI_VERSION=v0.0.3 bash
```

卸载：`curl -fsSL ... /install.sh | bash -s -- --uninstall`

**其它方式**：从 [Releases](../../releases) 手动下载，或源码编译：

```bash
go build -o bin/naozhi ./cmd/naozhi/
```

### 配置文件

仓库只提交 `config.example.yaml` 模板；部署时复制为 `config.yaml` 并填入你自己的值（`config.yaml` 在 `.gitignore` 中，避免提交环境特定数据）：

```bash
cp config.example.yaml config.yaml
# 编辑 config.yaml：填入 workspace 路径、IM 平台凭据、cron notify chat_id 等
```

凭据推荐通过环境变量注入（模板已用 `${VAR}` 占位），避免写死在文件中。

### 微信（两步启动）

```bash
# 1. 交互式扫码，自动获取 token 并生成配置
naozhi setup weixin

# 2. 启动
naozhi --config ~/.naozhi/config.yaml
```

需要两个微信号 —— 一个登录为 bot，另一个发消息测试。

### 飞书

1. 飞书开放平台 → 创建企业自建应用 → 开启"机器人"能力
2. 权限: `im:message`, `im:message:send_as_bot`, `im:message:patch`
3. 事件订阅: 选择 "使用长连接接收事件"，订阅 `im.message.receive_v1`
4. 发布应用版本
5. 配置凭据并启动:
   ```bash
   export FEISHU_APP_ID=your_app_id
   export FEISHU_APP_SECRET=your_app_secret
   naozhi --config config.yaml
   ```

### Slack

1. [api.slack.com/apps](https://api.slack.com/apps) → Create New App
2. 开启 Socket Mode，获取 App-Level Token (`xapp-...`)
3. Bot Token Scopes: `chat:write`, `app_mentions:read`
4. Event Subscriptions: `message.im`, `app_mention`

### Discord

1. [discord.com/developers](https://discord.com/developers/applications) → New Application → Bot
2. 开启 Message Content Intent
3. 获取 Bot Token，邀请到服务器

### 运行

```bash
naozhi --config config.yaml
```

健康检查: `curl http://localhost:8180/health`

Dashboard: 浏览器打开 `http://localhost:8180`

---

## 用户命令

| 命令 | 说明 |
|------|------|
| 普通消息 | 发送给默认 agent，保持多轮上下文 |
| `/review <text>` | 路由到 code-reviewer agent |
| `/research <text>` | 路由到 researcher agent |
| `/new` | 重置默认 agent 对话 |
| `/new review` | 重置指定 agent 对话 |
| `/cd <path>` | 切换工作目录 |
| `/pwd` | 显示当前工作目录 |
| `/project <name>` | 绑定到项目 |
| `/project off` | 解绑项目 |
| `/cron add "<schedule>" <prompt>` | 创建定时任务 |
| `/cron list` | 查看定时任务 |
| `/cron del/pause/resume <id>` | 管理定时任务 |
| `/help` | 显示可用命令 |

Agent 命令通过 `agent_commands` 配置映射，可自定义。

---

## 配置参考

```yaml
server:
  addr: ":8180"
  dashboard_token: "${DASHBOARD_TOKEN}"   # Dashboard 访问密码 (可选)
  trusted_proxy: false                    # ALB/CloudFront 终止 TLS 时设为 true

cli:
  backend: claude                         # "claude" | "kiro"
  path: "~/.local/bin/claude"
  model: "sonnet"                         # sonnet / opus / haiku
  args:
    - "--dangerously-skip-permissions"

session:
  cwd: "/home/user/projects"              # CLI 默认工作目录，亦作 /cd 的允许根路径
  max_procs: 3                            # 最大并发 CLI 进程
  ttl: "30m"                              # 空闲回收超时
  watchdog:
    no_output_timeout: "2m"               # 无输出超时
    total_timeout: "5m"                   # 单轮总超时
  store_path: "~/.naozhi/sessions.json"

agents:                                   # 自定义 agent
  code-reviewer:
    model: "sonnet"
    args: ['--append-system-prompt', 'You are a code reviewer...']
  researcher:
    model: "opus"

agent_commands:                           # 命令 → agent 映射
  review: code-reviewer
  research: researcher

cron:
  store_path: "~/.naozhi/cron.json"
  max_jobs: 50
  execution_timeout: "5m"

transcribe:                               # 语音转文字 (Amazon Transcribe)
  region: "us-east-1"
  language: "zh-CN"                       # BCP-47 单语言代码，默认 zh-CN

projects:
  root: "/home/user/projects"             # 项目扫描根目录

reverse_nodes:                            # 多节点：接受远程拨入
  my-workstation:
    token: "${NODE_TOKEN}"
    display_name: "Kevin's Mac"

# upstream:                               # 多节点：作为远程节点拨入
#   url: "wss://primary.example.com/ws-node"
#   node_id: "my-workstation"
#   token: "${NODE_TOKEN}"

platforms:
  feishu:
    app_id: "${FEISHU_APP_ID}"
    app_secret: "${FEISHU_APP_SECRET}"
    max_reply_length: 4000
  # slack:
  #   bot_token: "${SLACK_BOT_TOKEN}"
  #   app_token: "${SLACK_APP_TOKEN}"
  # discord:
  #   bot_token: "${DISCORD_BOT_TOKEN}"
  # weixin:
  #   token: "${WEIXIN_BOT_TOKEN}"
```

环境变量通过 `${VAR_NAME}` 语法自动展开。

---

## 部署

### 本地运行

所有平台均支持 WebSocket / 长轮询，直接运行即可，**无需公网 IP**。

### 服务器部署 (systemd)

```bash
# 编译
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/naozhi ./cmd/naozhi/

# 上传到服务器
scp bin/naozhi server:/usr/local/bin/

# 安装 systemd service
sudo cp deploy/naozhi.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable naozhi

# 配置凭据
cat > ~/.naozhi/env << 'EOF'
FEISHU_APP_ID=your_app_id
FEISHU_APP_SECRET=your_app_secret
DASHBOARD_TOKEN=your_dashboard_password
EOF
chmod 600 ~/.naozhi/env

# 启动
sudo systemctl start naozhi
journalctl -u naozhi -f
```

### 生产架构

```
CloudFront → ALB (SG: CloudFront-only) → EC2 :8180 → systemd
```

- ALB 安全组仅允许 CloudFront 前缀列表
- EC2 通过 IAM 角色认证 Bedrock（无 AKSK）
- 推荐 t4g.small ARM64 实例

### 发布

```bash
git tag v0.1.0
git push origin v0.1.0    # GitHub Actions 自动构建 6 平台二进制 + Release
```

---

## 项目结构

```
cmd/naozhi/              入口 + CLI 命令 (setup, install, version)
internal/
  cli/                   CLI 进程管理 + Protocol 接口 (stream-json / ACP)
  session/               Session 路由 + 并发控制 + TTL 回收 + 持久化
  server/                HTTP server + Dashboard + WebSocket hub
  platform/              IM 平台统一接口
    feishu/              飞书 (WebSocket + Webhook)
    slack/               Slack (Socket Mode)
    discord/             Discord (Gateway WebSocket)
    weixin/              微信 (iLink Bot HTTP 长轮询)
  cron/                  定时任务调度器
  project/               项目发现 + Planner 路由
  discovery/             外部 Claude 进程扫描 + 接管
  transcribe/            语音转文字 (Amazon Transcribe Streaming)
  connector/             反向连接客户端 (NAT 穿越)
  reverse/               多节点 WebSocket 协议
  routing/               命令解析
  config/                YAML 配置 + 环境变量展开
  pathutil/              路径工具
deploy/                  systemd service unit
```

## 设计文档

完整架构设计见 [DESIGN.md](docs/design/DESIGN.md)。

## License

[BSL 1.1](LICENSE) — 源码可读可改，个人和非生产用途免费。生产环境商用需获得授权。2030-03-21 后自动转为 Apache 2.0。
