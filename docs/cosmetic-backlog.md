# Cosmetic Backlog

> godoc / 命名 / 注释 / 纯结构调整建议的归档。无运行时影响的 review finding 进这里，不开 issue。
>
> 来源：`triage-findings` skill 自动 append。
>
> 处理频率：每 1-2 个月由人工批量扫一次决定是否值得一个清理 PR。

## 格式

```
- [R{ROUND}-{CAT}-{IDX}] <one-line summary> — <file>:<line>
```

## Entries

<!-- triage-findings skill 在此 append；新条目加到末尾 -->

- [R248-ARCH-5] AgentLinker interface 放 consumer-local (server pkg) 而非 session/agentlink — internal/session/agentlink/agentlink.go:1
- [R248-ARCH-7] SetScheduler/SetUploadStore/SetScratchPool 搬到 wshub_send.go 或新建 wshub_lifecycle.go — internal/server/wshub.go:369-378
- [R248-CR-5] AgentLinker.Query → Lookup, QueryOrResolveFast → Resolve — internal/session/agentlink/agentlink.go:34-40
- [R248-CR-7] handleAgentSubscribe 并入 wshub_subscribe.go (与 ValidateSessionKey 入口模式对齐) — internal/server/wshub_agent.go:127
<!-- CURATED-NAMING-1 已改提为 GitHub Issue (caller-visible API rename, 125+ call sites,
不符合 "zero functional impact" 标准；按 skill rule "godoc-AND-callers → A" 归 issue)。 -->

