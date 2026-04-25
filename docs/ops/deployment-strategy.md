# 部署方案设计

> 2026-04-03 初稿
> 2026-04-05 重构：以个人多设备为核心场景，精简方案；去掉 Docker，统一用 naozhi install

当前状态：CloudFront → ALB → EC2 t4g.small + systemd，手动 `make build` → S3 → SSH 部署。

---

## 核心场景

naozhi 的首要场景是**一个人连接自己的多台电脑和环境**：

```
飞书/Slack (IM 入口)
    ↓ webhook
Cloud naozhi (Hub，始终在线)
    ↓ WSS reverse-connect（已有协议）
    ├── 工作笔记本 (macOS)     — 公司代码库
    ├── 家里台式机 (Linux)     — 个人项目
    └── 云服务器 B (EC2)       — 特定环境
```

用户通过 IM 统一入口访问任意一台自己的机器，不需要 SSH、VPN 或记住各种 IP。

只有一台机器时退化为单机模式——IM webhook 直接打到这台机器，localhost:8180 看 dashboard。

**需要**：设备注册（`naozhi connect`）、设备切换（IM 里 `/use 工作笔记本`）、在线感知、进程保活、Hub-Spoke 间 token 认证。

**不需要**：用户登录体系、权限分级、用量配额、复杂管理后台。

---

## 约束

1. **有状态** — 每个 session 是长生命周期 claude CLI 子进程，in-memory 管理
2. **依赖 claude CLI + Node.js** — 目标用户本身就在用 Claude Code，机器上已有这些依赖
3. **子进程模型** — 排除 Fargate / Lambda 等无服务器方案

---

## 部署模式

**单机**：一台有公网 IP 的机器，跑一个 naozhi 实例。IM webhook 直接指向它。

**Hub-Spoke**：Hub 在云服务器上接 IM webhook，个人设备作为 Spoke 通过 WSS 反连。Hub 和 Spoke 跑的是同一个二进制，区别仅在 config.yaml 里是否配置了 `upstream`。

> 公网暴露由部署环境决定（ALB、直接绑 IP 等）。家里机器当 Hub 需自行解决内网穿透，不在本项目范围内。

---

## 安装流程

三步：下载 → 配置 → 注册服务。

```bash
# 1. 下载二进制（或从 GitHub Release 手动下载）
curl -fsSL https://raw.githubusercontent.com/.../install.sh | bash

# 2. 引导配置（生成 config.yaml）— 待实现
naozhi init
# → 检测 claude CLI
# → 选择 IM 平台 (feishu/slack/discord)
# → 填写凭证
# → 单机 or Spoke？Spoke 则填 Hub 地址和 token
# → 写入 config.yaml

# 3. 注册系统服务
naozhi install
# → Linux: 生成 systemd unit (Restart=always, WantedBy=multi-user.target)
# → macOS: 生成 launchd plist (KeepAlive=true, RunAtLoad=true)
# → 启动服务
```

`naozhi install` 只做一件事：读 config.yaml，注册并启动系统服务。单机还是 Spoke 由配置决定，不由 install 参数决定。

卸载：`naozhi uninstall`（停止服务 + 删除 unit 文件）。

### 设备可靠性

两层保障：

**OS 级保活**：systemd / launchd 提供崩溃重启 + 开机自启 + 休眠恢复。

**连接自愈（已实现）**：WSS 断线后指数退避重连（1s → 30s 上限），重连后自动重新订阅活跃 session（`wsrelay.go:reconnect()`）。

---

## CI/CD

### CI：GitHub Actions（已有）

`release.yml` 在 tag push 时自动构建 6 个平台的二进制并发布到 GitHub Release：

```
push tag v* → 验证 tag 在 master → 交叉编译 6 平台 → GitHub Release（二进制 + checksums）
```

CI 只负责构建和发布，不负责部署。

### CD：`naozhi upgrade`（待实现）

所有设备统一通过客户端拉取升级：

```bash
naozhi upgrade              # 检查 GitHub Release 最新版，下载替换，重启服务
```

逻辑：
1. 查询 GitHub Release API 获取最新版本
2. 对比当前版本（`main.version`，构建时通过 ldflags 注入）
3. 下载对应 OS/ARCH 的二进制 + checksum 校验
4. 替换当前二进制
5. 重启系统服务

可选自动升级：
```bash
naozhi install --auto-upgrade   # 注册 systemd timer / launchd calendar，定时检查新版本
```

### 完整流程

```
开发 → push tag → GitHub Actions 构建 + Release
                                    ↓
                    各设备 naozhi upgrade 拉取
                    ├── Hub (EC2)
                    ├── 工作笔记本
                    └── 家里台式机
```

---

## 不推荐

| 方案 | 原因 |
|------|------|
| Docker | 目标用户已有 Node.js + claude CLI，多一层容器增加复杂度无收益 |
| 内置内网穿透 | 方案太多（frp/ngrok/cloudflare），用户自选 |
| Kubernetes（个人场景） | 杀鸡用牛刀 |
| Fargate / Lambda | 子进程模型不兼容 |
| 裸跑二进制 | 无保活，崩溃后不恢复 |

---

## 未来：企业扩展

个人 Hub-Spoke 的自然延伸——从"一个人多台机器"变成"多个团队多台机器"：

- Hub 加 team 路由层（IM 用户 → Spoke 映射）
- Spoke 自注册 + 管理 API
- token 认证升级为 JWT / mTLS
- 审计日志、用量计量、配额管控
- Admin UI

基础协议（WSS reverse-connect、消息转发）已有，增量开发即可。不在当前优先级内。

---

## 优先级

| 阶段 | 内容 | 价值 |
|------|------|------|
| 1 | `naozhi install` 命令 | 解决当前手动部署 + 设备保活 |
| 2 | `naozhi upgrade` 命令 | Hub 和 Spoke 统一升级，消除手动 SSH |
| 3 | `naozhi init` 交互引导 | 首次配置体验 |
| 4 | 企业 Hub 模式 | 多团队场景 |
