# Contributing to naozhi

谢谢有兴趣贡献 naozhi。该项目目前主要由 Kevin Zhao 维护，外部 PR 欢迎但先读这份文档，避免来回返工。

## Scope / 适用范围

naozhi 的定位是"轻量 IM -> Claude Code CLI 网关"：用最少的代码把 AI CLI 暴露给飞书 / Slack / Discord / 微信。我们会倾向于接受——

- Bug 修复，且带回归测试
- 已存在渠道（feishu/slack/…）的增强
- 文档、配置示例、运维说明修正
- 跨平台兼容性修复（macOS / Windows build）

我们会 *谨慎* 接受——

- 新 IM 渠道（先开 issue 讨论，确认维护责任归属）
- 新 CLI backend（必须走 `internal/cli/protocol.go` 的 `Protocol` 接口，不能硬编码）
- 大型重构，尤其是 `internal/session/router.go` / `internal/server/` 的拆包

我们通常会拒绝——

- 引入新的外部依赖组件（Redis / MySQL / Kafka / …）—— naozhi 靠 file-backed 状态跑
- 打开 dashboard 给多用户访问的功能（项目定位是单人自托管）
- 需要 CGO 的代码

## Branches / 分支策略

- `master` 是 PR 目标分支，不要直推
- `dev` 是日常开发分支
- PR 命名：`<type>(<scope>): <短描述>`，常见 type：`feat`、`fix`、`perf`、`chore`、`refactor`、`docs`、`test`

## Pull Request 流程

1. Fork + clone + 建分支 `feat/my-thing` 或 `fix/issue-123`
2. 本地跑过完整门禁：
   ```bash
   /usr/local/go/bin/go build ./...
   /usr/local/go/bin/go vet ./...
   /usr/local/go/bin/go test -race -timeout 300s ./...
   ```
   输出里不允许出现任何 `FAIL`，也不接受用 `-run`/`-skip` 跳过失败测试凑绿。
   （当前 85 个包里 79 个有测试、6 个是 `no test files` —— 所以"全 ok"的基准是
   零 `FAIL`，不是数包数。）

   `-timeout` 是**每个包**的上限，不是总时长。别往下调：`internal/server`
   单包带 `-race` 就要 ~230s，用 `-timeout 180s` 会直接
   `panic: test timed out after 3m0s`，看起来像真的测试失败。300s 与
   `.github/workflows/ci.yml` 的 `test` job 保持一致。整套跑完约 4-5 分钟。
3. 如果改动触碰了 `docs/design/`、`CLAUDE.md`、`config.example.yaml`，同步更新其他两处（单一事实源原则）。
4. Commit message 参考 `git log --oneline -20` 近期风格
5. PR 描述写清 **why**（动机、关联的 issue），而非 what（代码本身会说话）

## Testing

- 所有新功能必须带单元测试，目标 80% 新行覆盖
- 并发改动必须跑 `-race` 通过
- 锁契约类测试（例如 `internal/session/*_contract_test.go`）不得删除；如果你的改动让契约过时，**先修契约再改代码**，让 reviewer 能看到契约变更本身
- 禁止引入新的 `time.Sleep` 测试；用 channel 或 `Eventually` pattern

## Code review

- HIGH / CRITICAL 发现必须先阻塞合并再修
- "契约测试"（source-level regex）改动要在 PR 描述里显式列出替换前后的不变量
- 不要跳过 pre-commit hook（`--no-verify`）；本仓 hook 只跑 gofmt + vet，都是秒级的

## Out of scope

以下主题请开 issue 讨论而非直接发 PR：

- 协议版本化（REST API / node reverse / shim state）—— 基础工作已完成
  （#425 / #426 / #427），后续扩展先开 issue 对齐
- 零中断热重启（现状是 `internal/shim` sidecar，见 `docs/design/shim-design.md`）
- Gemini CLI 接入（必须走 ACP `Protocol` 实现，不要硬编码）
- 大规模依赖升级（dependabot 自动开的 PR 优先合并）

待办不再维护 `docs/TODO.md`（该文件已删除），一律走 GitHub Issues；历史 `Rxxx-`
条目到 issue 的映射见 `docs/rfc/todo-to-issues-migration.md`。

## Questions

一般问题走 [Issues](https://github.com/KevinZhao/naozhi/issues) 讨论，不走邮件。
（本仓没有开启 GitHub Discussions。）

安全问题**不要**公开开 issue，走 [SECURITY.md](./SECURITY.md) 描述的私密上报流程。
