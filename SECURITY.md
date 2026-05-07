# Security Policy

## Supported versions

naozhi 没有 LTS 分支：我们只对 `master` 分支的 HEAD 提供安全修复。自托管用户请在部署时 `git pull && make deploy` 保持跟进。

## Threat model

naozhi 的默认威胁模型是**单人自托管**：

- 运行实例只给自己用，通过 VPN / Tailscale / ALB+token 访问 dashboard
- IM 平台（飞书 / Slack / …）的 webhook 走可信 TLS 端点
- 部署机被视为受信任；process 以单一 Unix 用户运行

**不在威胁模型之内** ——

- 多操作员共享同一 dashboard（项目定位不是多租户）
- 运行在对手能执行任意 `claude CLI` 命令的机器上（该路径默认 `--dangerously-skip-permissions`，设计上就放开）

## Reporting a vulnerability

请勿在公开 issue / discussions 里发布漏洞细节。通过以下任一渠道私下联系：

1. **首选**：GitHub Security Advisory（Draft）—— 仓库 Security 标签页 → "Report a vulnerability"
2. **备选**：邮件到 `security@<maintainer-domain>`（具体见仓库作者 GitHub profile）

我们的响应节奏：

| 窗口          | 动作                                    |
| ------------- | --------------------------------------- |
| 72h 内        | 确认收到，初步分级                      |
| 7d 内         | 回复根因、缓解策略、修复 ETA            |
| 修复后        | 发 advisory + CVE（若符合）+ 致谢       |

## Security-sensitive code paths

如果你在贡献 PR，以下路径触碰时请特别小心 —— 它们是 naozhi 的安全边界：

- `internal/server/dashboard_auth.go` —— dashboard cookie / bearer token
- `internal/server/csrf.go` —— Origin / Referer 校验
- `internal/server/wshub.go HandleUpgrade` —— WS 连接速率限制
- `internal/platform/feishu/transport_hook.go` —— webhook HMAC / nonce 去重
- `internal/project/validate.go` + `internal/server/dashboard_cron.go validateCronPrompt` —— 用户输入注入 CLI argv / stdin 前的消毒
- `internal/server/dashboard_send.go parseAttachmentFile` —— 上传文件 MIME 校验
- `internal/osutil/loginject.go` —— 日志注入控制字符过滤
- `internal/shim/` —— 进程生命周期与 sudo 操作

## Dependency updates

Dependabot 每周一 03:00 Asia/Shanghai 扫一次 `go.mod` 和 `.github/workflows/`。安全修复 PR 立即开，我们会优先合并。

## Out-of-band incidents

如果你怀疑生产部署发生了安全事件而不是代码漏洞（例如 token 泄漏、异常外发流量），使用 `naozhi doctor` 打印当前状态 + 保留 `journalctl -u naozhi` 最近 24h，再联系维护者。
