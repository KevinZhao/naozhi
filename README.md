# Naozhi

Claude Code 即时通信网关。将 Claude Code CLI 的完整 agent 能力（工具调用、代码编辑、MCP servers）通过 IM 平台暴露给用户。

## 工作原理

Naozhi 是一个 Go 编写的消息路由薄层。它不重造 agent loop，而是 spawn 本机 `claude` CLI 作为长生命周期子进程，通过 stream-json 协议（stdin/stdout NDJSON）进行多轮对话。

```
飞书/Slack/Telegram
       | webhook
       v
  Naozhi Gateway (Go)
       | stdin/stdout stream-json
       v
  claude CLI (长生命周期进程)
    + 完整工具链 (Bash/Read/Edit/...)
    + MCP servers
    + ECC skills
```

每个用户/群聊维护独立的 claude 进程和对话上下文。进程空闲 30 分钟后回收，下次消息自动 `--resume` 恢复。

## 快速开始

### 前置条件

- Go 1.21+
- [Claude Code CLI](https://claude.ai/code) 已安装并配置认证（Bedrock / API key / claude.ai）
- 飞书自建应用（[创建指南](https://open.feishu.cn/document/home/introduction-to-custom-app-development/self-built-apps)）

### 构建

```bash
go build -o bin/naozhi ./cmd/naozhi/
```

### 配置

```bash
cp config.yaml ~/.naozhi/config.yaml
# 编辑配置，或通过环境变量注入飞书凭据
export IM_APP_ID=your_app_id
export IM_APP_SECRET=your_app_secret
export IM_VERIFICATION_TOKEN=your_token
```

### 运行

```bash
bin/naozhi --config ~/.naozhi/config.yaml
```

健康检查: `curl http://localhost:8180/health`

### 飞书配置

1. 飞书开放平台 → 创建企业自建应用 → 开启"机器人"能力
2. 权限: `im:message`, `im:message:send_as_bot`, `im:message:patch`
3. 事件订阅: 请求地址填 `https://your-domain/webhook/feishu`，订阅 `im.message.receive_v1`
4. 发布应用版本

## 用户命令

| 命令 | 说明 |
|------|------|
| 普通消息 | 发送给 claude，保持多轮上下文 |
| `/new` | 重置对话，开始全新 session |

## 配置项

```yaml
server:
  addr: ":8180"

cli:
  path: "~/.local/bin/claude"
  model: "sonnet"                    # sonnet / opus / haiku
  args:
    - "--dangerously-skip-permissions"

session:
  max_procs: 3                       # 最大并发 claude 进程 (~350MB/进程)
  ttl: "30m"                         # 空闲回收超时

platforms:
  feishu:
    app_id: "${IM_APP_ID}"
    app_secret: "${IM_APP_SECRET}"
    verification_token: "${IM_VERIFICATION_TOKEN}"
    encrypt_key: "${IM_ENCRYPT_KEY}"
    max_reply_length: 4000
```

## 部署

生产环境架构: CloudFront → ALB → EC2 (systemd)

```bash
# 首次部署
./deploy/setup-env.sh <instance-id>   # 配置飞书凭据
./deploy/deploy.sh deploy             # 构建 + 推送 + 启动

# 日常更新
./deploy/deploy.sh deploy             # 重新构建并部署

# 运维
./deploy/deploy.sh status             # 查看服务状态
./deploy/deploy.sh logs               # 查看日志
```

详见 [DESIGN.md](DESIGN.md) 中的部署架构章节。

## 项目结构

```
cmd/naozhi/main.go          入口
internal/
  cli/                       Claude CLI 进程管理 (stream-json 协议)
  session/                   Session 路由 + 并发控制 + TTL 回收
  platform/                  IM 平台接口 + 飞书实现
  config/                    YAML 配置加载
  server/                    HTTP server + 消息处理
deploy/                      systemd + 部署脚本
```

## 设计文档

完整的架构设计、技术选型（含 OpenClaw 对比分析、Channels/Agent SDK 可行性验证）、协议细节见 [DESIGN.md](DESIGN.md)。

## License

[BSL 1.1](LICENSE) — 源码可读可改，个人和非生产用途免费。生产环境商用或作为托管服务提供需获得授权。2030-03-21 后自动转为 Apache 2.0。
