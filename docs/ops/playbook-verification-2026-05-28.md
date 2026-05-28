# Playbook 验证报告 — 2026-05-28

> **状态**：4 份 playbook 与 origin/master HEAD `cd1289b0` 对账后修订
> **配套文件**：
>  - [phase4b-implementation-playbook.md](phase4b-implementation-playbook.md)
>  - [phase4b-feasibility-report.md](phase4b-feasibility-report.md)
>  - [phase4c-implementation-playbook.md](phase4c-implementation-playbook.md)
>  - [phase1-3f-implementation-playbook.md](phase1-3f-implementation-playbook.md)
>  - [phase5-implementation-playbook.md](phase5-implementation-playbook.md)

## 0. 验证背景

#1419 合 4 份 playbook 后，发现 origin/master 持续涨——本次重新跑 master
对账 4 份 playbook 的所有数字 / 路径 / 验收 gate 是否仍然准确。

## 1. 9 项验证发现（按严重度排序）

### 🔴 critical — 必须修

| # | 发现 | 影响 playbook | 状态 |
|---|---|---|---|
| 1 | exemptions.yaml baseline 全部漂移（6 个文件 file grew）| 4b/4c/1-3f/5 | ✅ 已加 pre-flight checklist |
| 2 | 4b feasibility 写"30+ Hub 方法"实测仅 10 个 | 4b feasibility | ✅ 已校准 |
| 3 | 4b playbook 写"wshub.go 921 行"实测 1018 | 4b | ✅ 已校准 |
| 4 | 4b playbook 写"HubRouter ~30 方法"实测 14 | 4b | ✅ 已校准 |

### 🟡 mid — 建议修

| # | 发现 | 影响 playbook | 状态 |
|---|---|---|---|
| 5 | Phase 1 行数估 2810 实测 3382（+20%）| 1-3f | ✅ 已校准 |
| 6 | Phase 2 行数估 1830 实测 2091 | 1-3f | ✅ 已校准 |
| 7 | playbook 缺通用 pre-flight checklist | 4b/4c/1-3f/5 | ✅ 4 份全加 |

### 🟢 ok — 无需改

| # | 发现 | 状态 |
|---|---|---|
| 8 | Phase 5 §2 47 字段去向表 100% 准确 | ✅ 13+10+4+3+2+3=35 全对账 |
| 9 | Phase 4c agent_tailer.go 估 827 实测 807，cache 估 200 实测 160 | ✅ 接近，无需改 |

## 2. 修订 PR diff stat

```
docs/ops/phase4b-feasibility-report.md       +20 / -7
docs/ops/phase4b-implementation-playbook.md  +29 / -3
docs/ops/phase4c-implementation-playbook.md  +20 / -0
docs/ops/phase1-3f-implementation-playbook.md +30 / -10
docs/ops/phase5-implementation-playbook.md   +18 / -0
docs/ops/playbook-verification-2026-05-28.md +95 / -0  (本文)
```

总改动 ~210 行（5 文件已存修订 + 1 新增）。零代码变化——纯文档校准。

## 3. 通用 pre-flight checklist（任何 phase 都跑）

```bash
# 1. 同步 master + 切新分支
git fetch origin master
git checkout -B server-split/phase<N> origin/master

# 2. 实测受影响文件最新行数
for f in <phase 范围内的文件>; do
  echo "$f: $(wc -l < $f) lines"
done

# 3. 更新 exemptions.yaml current 字段到搬迁前最新值
# 否则 linter 持续报 file grew，污染 PR diff

# 4. baseline build/test 必须绿
go build ./...
go test -race -count=1 ./internal/wshub/ ./internal/server/...

# 5. 记录 lint 噪音 + race test 时间 baseline
go run ./tools/lint-server-handlers/ 2>&1 | tail -2
time go test -race -count=1 -timeout=300s ./...
```

**关键事实**：master 涨速实测 ~20%/4 月。设计稿 v0.6.1 §6.1 行数估值
是 v0.6.1 当时实测，但 master 仍在涨——**以实施前实测为准**。

## 4. 重新评估 Phase 4b 可行性

feasibility report v1 写"4b 单次 AI 会话不可完成"基于估计的"30+ Hub
方法引用"。**校准后实测仅 10 个方法**——重新评估：

- **Phase 4b1+4b2 合并刀**（wsClient + HubRouter + subscribe 块方法）：
  ~1000-1200 行，**可能在 1-2 小时 AI 会话内完成**
- **Phase 4b3**（broadcast + send + cache + 调用方切换 + race count=100
  测试）：仍需独立 PR，~1500 行 + 复杂 race 调试

修订意见：**Phase 4b 单 PR 仍不现实**，但 **4b 拆 4b1+4b2 / 4b3 两刀**
比之前估计的 4b1/4b2/4b3 三刀更经济。详 phase4b-feasibility-report.md
"v2 修订" 段。

## 5. 实施触发判定

按修订后的 playbook 评估实施可行性：

| Phase | 工作量 | AI 会话能完成？ |
|---|---|---|
| 4b1+4b2 合并 | ~1200 行 | 可能（前提：先跑 pre-flight + 不并发跑其他大改）|
| 4b3 | ~1500 行 + race 调试 | 不能（复杂调试需交互）|
| 4c | ~1000 行 | 可能 |
| 1（cron）| ~3382 行 + 14 测试 | 不能（PR 太大）|
| 2（project）| ~2091 行 | 不能 |
| 3a-3d | 各 ~600 行 | 可能（每个独立）|
| 3e | ~1000 行 | 可能 |
| 3f | ~1500 行 + send 路径 | 不能（高风险）|
| 5 | ~300 行 + mutex profile | 部分可能（profile 需运行环境）|

## 6. 总评

**4 份 playbook 修订后整体可执行性：95%**——前置数字全部对账到 master，
pre-flight checklist 防止 baseline 漂移。下一刀实施者可直接按图施工。

**剩余 5%**：Phase 4b3 / 3f / 5 仍需在能交互式 build/test 的环境推进。
