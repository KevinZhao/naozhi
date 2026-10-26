# Contributing to naozhi

谢谢有兴趣贡献 naozhi。项目目前主要由 Kevin Zhao 维护，外部 PR 欢迎。

## 适用范围

naozhi 的定位是"轻量 IM -> AI CLI 网关"：用最少的代码把 Claude Code / Kiro / Codex CLI 暴露给飞书 / Slack / Discord / 微信等 IM。

- 欢迎：带回归测试的 bug 修复、已有渠道的增强、文档与配置示例修正、Linux / macOS 兼容性修复（Windows 不在支持范围）。
- 先开 issue 讨论：新 IM 渠道、新 CLI backend（必须走 `internal/cli/protocol.go` 的 `Protocol` 接口）、大型重构、大规模依赖升级。
- 通常拒绝：新增外部依赖组件（Redis / MySQL / Kafka 等，naozhi 只用文件状态）、多用户 dashboard、需要 CGO 的代码。

## Pull Request

1. 从 `master` 建分支，PR 目标是 `master`，不要直推。PR 标题用 `<type>(<scope>): <短描述>`（`feat` / `fix` / `perf` / `refactor` / `docs` / `test` / `chore`）。
2. 推送前本地跑过：
   ```bash
   go build ./...
   go vet ./...
   gofmt -l .        # 输出须为空
   go test ./...
   ```
   不接受用 `-run` / `-skip` 跳过或删除失败测试来凑绿。
3. PR 描述写清动机（why）和关联 issue。
4. 唯一必过的 check 是聚合 job `test`，绿了即可合入。

待办走 GitHub Issues（不要重建 `docs/TODO.md`）。一般问题走 [Issues](https://github.com/KevinZhao/naozhi/issues)；安全问题**不要**公开开 issue，按 [SECURITY.md](./SECURITY.md) 私密上报。
