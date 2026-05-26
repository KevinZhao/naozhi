# RFC: docs/TODO.md → GitHub Issues 迁移

> **状态**: v1 (final, post-implementation, 2026-05-26)
> **PR 链**: #370 / #443 / #462 / #464 / #471 / #994 / #998 / #1063 / #1064
> **作者**: Kevin Zhao + Claude Code agent

## 目录

- [1. Background & problem](#1-background--problem)
- [2. Goals & non-goals](#2-goals--non-goals)
- [3. Alternatives considered](#3-alternatives-considered)
- [4. Architecture](#4-architecture)
- [5. 实施实测数据](#5-实施实测数据)
- [6. Risk & rollback](#6-risk--rollback)
- [7. Observability](#7-observability)
- [8. Compatibility & migration](#8-compatibility--migration)
- [9. Rollout plan](#9-rollout-plan)
- [10. Lessons learned](#10-lessons-learned)
- [11. Reference](#11-reference)

## 1. Background & problem

### 1.1 旧体系痛点

`docs/TODO.md` 是项目从 Round 1 (2026 年 3 月) 起的 review-finding dump 文件。到 2026-05-25 时:

| 维度 | 数值 |
|---|---|
| 文件大小 | 2649 行 / 489 KB |
| Open `[ ]` 条目 | 1054 |
| Paused/降级 `[~]` | 293 |
| Done `[x]` (待清理) | 37 |
| 涉及 Round 数 | 65 (R215..R248) |
| H2 sections | 40 |
| 近 2 周改动次数 | 164 commit (164 PR/commit 触此文件) |

**核心问题**: 这是**所有 PR 都要触的共享文件**。每个 review agent 跑完都 append finding；每个 fix PR 都要标 `[x]`；每周 sweeping PR 都改顶部摘要。结果:

- **每周稳定多个 merge conflict**: 顶部"最近 N 轮重大动作"是热点；行级 diff 不容忍并发
- **同根因反复登记**: 看到 `[REPEAT-2]`、`[REPEAT-5]` 标记，说明系统已无法去重，靠人工 grep 防重
- **review agent 直接 append 原始 finding**: 没有验证 / 去重 / 分流环节，1054 open 里 89% 是未分流的 review 原始产物
- **89 个 P3 godoc / 命名 / 注释建议混在 P0 安全条目中**: 信号噪音比极差
- **293 条降级 `[~]` 留在原位**: 不归档，让 grep 永远要过滤这些

### 1.2 量化分析（迁移前 2026-05-25）

按区拆分（**注：以下三组数字反映不同 grep 边界，做迁移决策时用 1054 总数；批 3 实跑因 R232/R233/R233B 嵌套子区被 D agent 一并 scan，实际处理 1042**）:

| 区域 | Open 数量 | 性质 |
|---|---|---|
| CRITICAL/HIGH/UX/MEDIUM/LOW/新功能 | 103 | 人工 curate "真要做" |
| R248 单独 | 12 | pilot 用 |
| Round 215-247 dump 区（R26-82 archive 含 10）| 939 | review agent 自动堆出，未分流 |
| **总开 `[ ]`** | **1054** | （`grep -c "^- \[ \]"`）|

R215-R247 dump 区里:
- P3 占 189
- godoc / 命名 / 注释类 102 条
- P0/HIGH 仅 11 条

## 2. Goals & non-goals

### Goals
- **消除 PR conflict 来源**: 任何工作流不再触同一个集中文件
- **review-finding 闭环**: 真问题进 issue tracker、cosmetic 进 backlog、误报有 audit trail，每条都被 grep verify
- **issue tracker 无噪音**: cron 自动开 issue 不能淹没人工 backlog
- **可观测性**: priority/area/type label 能让 `gh issue list --label X` 即时切片
- **PR auto-close issue**: GitHub `Closes #N` 机制自动收口

### Non-goals
- 不重写 issue tracker（用 GitHub 原生）
- 不引入 issue body schema 之外的元数据存储
- 不改造现有 review skill（只改 dev-workflow + 加 triage skill）
- 不替代 `docs/cosmetic-backlog.md`（继续作 readability 类草稿）
- 不删除代码里历史 `Removal tracked in docs/TODO.md R-XXX` 注释（保留作 anchor 历史）

## 3. Alternatives considered

### A. Git union merge driver（最低成本，治标）

`.gitattributes` 加 `docs/TODO.md merge=union`:
- ✅ 不同行并发追加自动合并
- ❌ 同一行不同改动仍冲突
- ❌ union 不验语义，可能产生重复行
- ❌ 归档 / 删除类操作仍冲突
- ❌ 文件本身越长越慢，治标

### B. TODO.md 拆 index + 一条一文件（中成本，治本）

`docs/todo/R247-CR-11.md` 这样每条独立文件，TODO.md 退化为薄索引:
- ✅ 文件级粒度，几乎不冲突
- ❌ 失去"一屏纵览"
- ❌ 需要工具脚本（list / grep / archive）
- ❌ 1054 文件批量创建对 git 文件系统压力

### C. PR 不再改 TODO，由定期 sweeping PR 同步（流程治本）

PR commit message 只打 `[R247-CR-11]` anchor，TODO.md 由 master 上的定期 sweeping PR 批量同步:
- ✅ feature PR 不再 touch TODO
- ❌ 状态有滞后
- ❌ 需要纪律（人工失误就破）
- ❌ sweeping PR 仍是冲突源（只是把冲突推后）

### **D. GitHub Issues + LLM-based triage（最终选择）**

将 active backlog 完全迁移到 GitHub Issues:
- ✅ Issues 是数据库记录不是文件，**零文件级冲突**
- ✅ open/closed/labels/milestones 不用自己设计语义
- ✅ `gh issue list --label X` 比 grep 强
- ✅ PR `Closes #N` 自动 close issue
- ✅ 已有现成 review/auto-merge cron skill 改造代价小

代价:
- public repo issue 列表有可见性
- 离线 grep 失效（agent 要 `gh issue view`）
- 1054 历史条目需迁移
- review agent 工作流要改

**结论选 D**: 痛点直击，cron + skill 体系刚好能放大 D 的优势（自动化）。

## 4. Architecture

### 4.1 三桶分流模型

每条 review finding 通过 `triage-findings` skill 必须落到三桶之一:

| Bucket | 去向 | 判定 |
|---|---|---|
| **A** | `gh issue create` | grep verify 还在 + 非 dup + 真功能影响（含 P3 correctness） |
| **B** | `docs/cosmetic-backlog.md` append | 零运行时影响（纯 godoc / 命名 / 注释 / file-move） |
| **C** | raw 文件 `## Discarded` 区 | 已修 / 误报 / 已 dup / 不可行 |

**关键：每条强制 grep verify**。stale review 直接进 C，不浪费 issue tracker。

### 4.2 5 个流程改造

```
旧流程:                              新流程:
review agent runs                    review agent runs
   ↓                                   ↓
append docs/TODO.md (raw, no dedup)  write docs/review/R{N}-raw.md (staging)
   ↓                                   ↓
fix PR 改 [x] 或 [~]                 triage-findings skill
   ↓                                   ├─ A → gh issue create + label 全套
PR conflict (TODO.md 是热点)          ├─ B → docs/cosmetic-backlog.md
                                      └─ C → raw ## Discarded (audit trail)
                                        ↓
                                      cron-Hourly Fix: gh issue list 选题 → fix → PR Closes #N
                                        ↓
                                      cron-PR Merge: cron/* 自动合 + verify Closes #N
                                        ↓
                                      GitHub auto-close
```

### 4.3 Label 体系

```
priority:p0|p1|p2|p3              # 仅打标，不 gate issue 创建
area:cron|wshub|dashboard|cli|    # subsystem 分桶，cron prompt 按 area 拆 fix agent
       dispatch|server|session|
       adapter|shim|persistence
type:correctness|perf|sec|        # finding 性质
     refactor|ux|feature
source:R{ROUND}|curated-{X}       # 出处追溯
needs-design                      # 需 RFC 决策（cron-Hourly Fix 跳过）
needs-triage                      # GitHub UI 创建的 issue（手工 file，cron 不动）
human-review-required             # 人写 PR opt-out cron 自动合（v5）
do-not-merge                      # 同上，cron 跳过
cron-reviewed-pass|changes|block  # fix-pr-review cron 给 cron PR 的 verdict
cron-retry-attempted              # PR Merge cron 已对该 PR 尝试过一次 CI retrigger
```

> **v5 (2026-05-26)**: 取消 `cron-mergeable` 必填 opt-in。人写 PR 默认合，命中
> `(breaking-local)` 标题前缀 / Draft / `human-review-required` / `do-not-merge`
> 才跳过。理由：单人项目里 opt-in 摩擦 > 价值；GitHub Draft 已能表达"还在改"，
> 加新 label `do-not-merge` 显式拒绝合，比每个 PR 都加 `cron-mergeable` 顺手。

### 4.4 5 个 cron 角色

| Cron | schedule | 改造内容 |
|---|---|---|
| Hourly TODO Fix | `15 * * * *` | 旧:扫 TODO.md `[ ]` 项。新:`gh issue list --state open` 按 priority + area 选题 → 4 并行 fix agent → commit `(#N)` → PR `Closes #N #M ...` |
| Daily Code Review | `0 12 * * *` | 旧:5 reviewer + append TODO.md。新:5 reviewer → grep verify + 双层 dedup（anchor + 内容相似） → 即修走 commit `(#N)` / 不能即修走 `gh issue create` |
| Hourly Fix-PR Review | `20 * * * *` | v1: fresh-context reviewer agent 审 `cron/(todo-fix\|code-review)/*` 分支的 PR diff，给 verdict（PASS/CHANGES/BLOCK），label 表达。BLOCK → close PR + reopen 关联 issue |
| Hourly PR Review & Merge | `0 * * * *` | v5 分流: cron PR 必须带 `cron-reviewed-pass\|changes` label 才合（review gate）；人写 PR **默认合**（CLEAN + 全绿），命中 `(breaking-local)` / Draft / `human-review-required` / `do-not-merge` 才跳过；merge 后 `gh pr view --json closingIssuesReferences` verify issue 真 closed，漏关补 close |
| 每日打 tag | `0 3 * * *` | 去掉旧版独立 tag-lock（与其他 cron 隔离的 `naozhi-cron-tag.lock`），新流程下不再需要 (worktree 隔离 + 任务幂等) |

去掉跨 cron lock（worktree 隔离 + GitHub API 幂等，并发安全）。

### 4.5 PR 自动合 path

**cron PR**：

```
1. Hourly Fix cron 跑 → 开 PR (cron/todo-fix-* 分支)
2. CI 跑 (test/lint/vuln/macos/windows)
3. Fix-PR Review cron 跑 → reviewer agent 审 → 上 cron-reviewed-{pass|changes|block}
4. PR Merge cron 下次 tick → 识别 cron/* + reviewed-pass/changes label + CLEAN → 自动 squash merge
5. GitHub Closes #N #M ... 自动 close 关联 issue
6. PR Merge cron verify-pass 兜底
```

**人写 PR (v5)**：默认走 PR Merge cron 自动合。无需 opt-in label。

```
1. 开 PR (任何分支)
2. CI 跑 → CLEAN
3. PR Merge cron 下次 tick → 默认合（除非命中 opt-out）
   opt-out: (breaking-local) 标题前缀 | Draft | human-review-required | do-not-merge label
4. 同上 verify-pass
```

设计原则：cron PR 走 review gate（必须显式 PASS/CHANGES）；人写 PR 默认信任作者，
只在显式拒绝时才不合。单人项目里这个分界比双向 opt-in 顺手。

## 5. 实施实测数据

### 5.1 三批 historical migration

| 批次 | 范围 | finding | issue | cosmetic | discarded | PR |
|---|---|---|---|---|---|---|
| R248 pilot | 12 | 12 | 8 | 4 | 0 | (含在 #464) |
| 批 1 | CRITICAL+HIGH 75 | 75 | 62 | 0 | 13 (+1 已修+1 review-改归 #463) | #464 |
| 批 2 | UX+MED+LOW 28 | 28 | 18 | 1→0 | 9 | #464 |
| 批 3 (6 并行 agent) | Round dump 1042 | 1042 | 493 | 200 | 349 | #1063 |
| **合计** | **1157** | **1157** | **581** | **204** | **371** |

> **数字 reconciliation**: 1054 (§1.2 静态 grep) → 1042 (批 3 D agent scan 含 R232/R233/R233B 子区) + 12 R248 pilot + 75+28 curated = 1157（批 3 因子区扩展多算 12，curated 区与 dump 区有少量重叠在 D agent scan 时被一并处理）。

cron 自产 issue (#465-#915) 不在 581 内（那是 cron-Hourly Fix 自循环产物，独立计数）。

### 5.2 弃率与新旧 round 关系（批 3 关键观察）

弃率 = (finding - issue - cosmetic) / finding（`-A 34% / -B 22% / -C 51% / -D 93% / -E 26% / -F 97%`）。

| Group | Rounds | finding | issue | cosmetic | discarded | 弃率 |
|---|---|---|---|---|---|---|
| A | R245-247 (新) | 190 | 126 | 17 | 47 | 34% |
| B | R241-244 | 169 | 131 | 9 | 12+17 backfill | 22% |
| C | R236-240 | 210 | 103 | 71 | 31+27 backfill | 51% |
| D | R225-235 (旧) | 393 | 27 | 85 | 277 | 93% |
| E | R215-224 + R26-82 (最旧) | 89 | 66 | 0 | 23 | 26% |
| F | R216 单 round (13 天) | 116 | 4 | 1 | 111 | **97%** |

**反直觉**: 不是越旧弃率越高。F (R216 单 round) 弃率 97% 因被 R217-R248 反复覆盖；E (R215-R224) 仅 26% 弃，因架构 / concurrency 类问题在后续 round 中没被 cluster 收口（reviewers 不再点同根因）。D (R225-R235) 弃率最高 93% 因夹在 E 与 A/B 之间，问题被两端 round 双向覆盖。**结论: round 间距 + 同根因 cluster 复发频率共同决定 staleness，单纯时间不能预测**。

### 5.3 GitHub Abuse Detection 触发

并行 6 agent 跑批 3 时，B + C agent 在短时内开 100+ issue 触发 GitHub secondary rate limit:

- 症状: `gh issue create` 静默 exit 0，issue 实际未创建
- 检测: `gh api -X POST /repos/.../issues` 返 403 (`gh issue create` 吞了)
- 影响: 44 finding 被标 `pending-issue:RATE-LIMITED` (B 17 + C 27 = 44 队列总数；4 是已知 dup 立即归 C，36 是真要 create 的)
- 解决: backfill agent 用 7s sleep + 60s cooldown，36 backfill 全开 (#1027-#1062)，4 已知 dup 在 backfill 时归 discarded

**教训**: 批量 issue create 不能裸跑，要 throttle。skill 已记录 `gh api POST` + sleep 模式。

### 5.4 Skill 二次改进（从实战反馈）

| 改进 | 来源 |
|---|---|
| A vs B borderline 7 条具体例子（file move / interface relocate / godoc-only rename / split god-struct） | 批 1 R248 pilot agent 反馈 ambiguity |
| Self-rolled / Not-actionable 自动判定（`合并到 X 跟踪` / `非 action item`） | 批 1 大量遇到 |
| Line-number drift OK 规则（symbol 还在算 verified） | 批 1 router.go split 后线号漂移 |
| Issue title ≤ 90 字符 | 批 1 多个超长 title |
| `--label` 而非 `--search` 做 source-round dedup | 批 1 R248 dedup 错误用 `--search` |
| `降级 LOW / Paused` 不自动弃，verify 后归 p3 | 批 1 误把 deferred 当 discarded |
| `gh api POST` 而非 `gh issue create` 检测 rate limit | 批 3 abuse detection |

## 6. Risk & rollback

### 6.1 风险

| 风险 | 现状 | 缓解 |
|---|---|---|
| GitHub Issues 列表对外可见（public repo） | 已确认 user 接受 | issue body 不放 secret / hostname / 内部路径 |
| LLM triage 把真问题判 C 弃 | 跨 6 batch 实测 | 每条 grep verify + audit trail 留底 |
| cron auto-merge 把人写 PR 误合 | v5 (2026-05-26) | `(breaking-local)` 前缀 / Draft / `human-review-required` / `do-not-merge` label 任一即跳过；CI 必须全绿才合 |
| GitHub abuse detection | 已触发 1 次 | skill 含 throttle 模式 + `gh api POST` 检测 |
| issue tracker 噪音爆炸 | 当前 ~30 open | cron-Hourly Fix 持续消化 + 主动 close stale |
| 历史 anchor 失联 | 4 archive 文件留作 reference | grep 仍可回溯 |

### 6.2 Rollback (不可行)

理论 rollback 需要还原 PR #370 / #464 / #1063 / #1064 + 4 cron prompt 历史版本 + close ~581 issue + 重建 TODO.md 内容。但:

- 581 issue 已开 + 多 cron 已 merge 多 PR (依赖 GitHub auto-close)
- 4 cron prompt 已 PATCH 多版本，原版需从 git log 翻找
- 回滚等于丢失全部 review 数据 + 已 close 的 issue 状态

**承诺前向不回滚**。如需"暂停"新流程（不删除 issue），可:
1. PATCH 4 cron prompt 增加 `exit 0` 短路
2. issue 不 close，等观察期结束再决定
3. 重建 TODO.md 走 `git revert PR #1064`（其他 PR 的内容保留）

实际操作中建议先单独评估具体痛点（流程哪个环节失败），而非整体回滚。

## 7. Observability

### 7.1 流程健康度信号

> **查询提示**: 用 `--label "source:R{N}"` 准确匹配（`gh issue list --label`），不要用 `--search "label:source"`（GitHub 全文搜索不解析 label 语法）。

| 指标 | 健康范围 | 报警 |
|---|---|---|
| `gh issue list --state open` 总数 | 30-100 | > 200 = cron 落后或 review 暴涨 |
| Hourly Fix cron 单 run duration | 15-25 min | > 35 min = stuck |
| PR Merge cron 单 run duration | 2-10 min | > 20 min = 撞 lock 或 CI 慢 |
| 每周 cron 自产 PR 自动合数 | 5-15 | < 3 = cron 不工作 |
| `cron_jobs.json` `last_result` `[OK]` | 100% | 任何 `[FAILED]` 立查 |
| `docs/cosmetic-backlog.md` 行数 | 增 50/月 OK | 增 200/月 = review skill 太松 |

### 7.2 Dashboard cron run history

`/api/cron/runs?job_id=...&limit=N` 提供:
- `state`: succeeded / failed / running
- `duration_ms`
- `last_result`: cron prompt 末尾自定义报告
- `session_id`: dashboard 可看 transcript

## 8. Compatibility & migration

### 8.1 已迁移完成

- ✅ docs/TODO.md → 删除（PR #1064 commit 93ba6889）
- ✅ docs/TODO_ARCHIVE.md / TODO_CHANGELOG.md / TODO-changelog.md → 留作只读 reference + banner 标注
- ✅ 4 个 cron prompt → v8 / v3
- ✅ 21 个 GitHub label 体系
- ✅ `.github/ISSUE_TEMPLATE/finding.yml`
- ✅ `.claude/skills/triage-findings/SKILL.md`
- ✅ `.claude/skills/dev-workflow/SKILL.md` 同步引用

### 8.2 历史 anchor 反查

代码里残留 `Removal tracked in docs/TODO.md R-XXX` 类注释**保留**，因为:
- R-anchor 仍可在 `docs/TODO_ARCHIVE.md` / `docs/TODO-changelog.md` grep 到
- 改这些注释影响面太大（27 个文件），ROI 不高
- 它们指向"曾经的"待办，不是"现在的"工作单

**新加注释禁止**用 `docs/TODO.md` 引用，统一用 `Closes #N` 或 GitHub issue URL。

## 9. Rollout plan

### 9.1 已完成阶段

```
Day 1 (2026-05-25):
  10:00 PR #370 — 冻结 TODO.md + 建 triage skill + label 体系
  14:00 PR #443 — 第 1 轮 Hourly Fix cron 测试，6 issue 自动 close
  19:00 PR #464 — R248 pilot + batch 1 + batch 2 (89 issue + skill 改进)
  
Day 2 (2026-05-26):
  06:00 cron auto-runs (PR #462, #471, #994 自产合并)
  17:00 PR #1063 — batch 3 (493 issue + 200 cosmetic, 6 并行 agent)
  18:00 PR #1064 — 删 TODO.md (闭环)
  20:00 RFC 文档（本文）
```

### 9.2 后续维护手册

**review skill 跑出 finding 时**:
1. agent 写 `docs/review/R{N}-raw.md`
2. 调 `triage-findings` skill
3. skill 自动跑三桶分流
4. 报告:`R{N} triage: K → X issues / Y cosmetic / Z discarded`

**人开 issue 时**:
- GitHub UI 用 finding template (会自动加 `needs-triage`)
- cron 不动它，等人手工分流加 priority/area/type

**cron 异常时**:
1. dashboard `/api/cron/runs` 看 last `[FAILED]` reason
2. 看 worktree `naozhi-cron-*` 是否残留
3. `sudo journalctl -u naozhi --since "1 hour ago" | grep cron`

**rate limit 触发时**:
- skill 已含 throttle 模式 (`gh api POST` + sleep 7s + 60s cooldown / 30 issues)
- 人工干预: 等 30-60 min 自然 reset

## 10. Lessons learned

1. **pilot 1 batch (12 条) 暴露 5 个 skill 缺陷**: anchor 命名不一致 / `--label` vs `--search` dedup / 行号漂移判定 / cosmetic 边界 / `gh issue create` 静默 403 — 都是 design 期间没想到的，避免在 1054 条上踩坑
2. **并行 agent 触发 GitHub abuse detection (~100 issue)**: 不是 token rate limit (5000/h)，是行为检测；`gh issue create` 在 403 时静默 exit 0，必须用 `gh api POST` 才能感知
3. **PR `Closes #N` 闭环是关键**: 没这条，issue tracker 还是会涨；它把 PR Merge cron 自动合 + GitHub auto-close 串成最后一环
4. **archive 文件保留比删除稳**: 删 TODO.md 但保留 3 个 TODO_*.md 让代码里 27 处 `R-anchor` 注释仍可 grep 回溯，避免 27 处文档全改
5. **`降级 LOW / Paused` ≠ discarded**: 第一版 skill 把 deferred 误归 C，批 1 修正后 26 条原本要弃的 finding 转 issue（架构 / concurrency 类）；老 review 锚的真问题没人修，新 review 也不会重发现
6. **更新 cron prompt 必须走 PATCH `/api/cron`，不能直接改 `cron_jobs.json` + restart** (2026-05-26): 直接改文件 → systemctl restart → 旧进程 SIGTERM 时 final persist 把内存里的旧 prompt 写回磁盘覆盖修改 → 新进程加载到的还是旧 prompt。`POST /api/cron/trigger` 后 `gh pr view` 能看到 `prompt_len` 没变；transcript 里 user turn 第一段还是旧版字串就是这个症状
7. **v5 人写 PR 默认合的迁移过程暴露 master pre-existing race** (2026-05-26): `t.Parallel` 加在 swap `slog.Default` 的 test 上 → peer parallel test 的 `slog.Warn` 写 swap 后的 buffer → race。一个 PR rebase 上来 race 触发，其他 PR rebase 也都被同一个 race 阻断。教训：cron 自产 PR 也会引入 master regression，review gate 不能漏（PR #1219 修复，1 文件 6 行）

## 11. Reference

- PR #370 (流程基础设施)
- PR #464 (R248 pilot + batch 1+2)
- PR #1063 (batch 3 Round dump)
- PR #1064 (删 TODO.md)
- PR #1219 (修 master pre-existing race 解锁后续合并)
- `.claude/skills/triage-findings/SKILL.md`
- `.claude/skills/dev-workflow/SKILL.md`
- `.github/ISSUE_TEMPLATE/finding.yml`
- `docs/cosmetic-backlog.md`
- `docs/review/R{248,batch1,batch2,batch3-{A,B,C,D,E,F}}-raw.md` (audit trail)
- 历史 reference: `docs/TODO_ARCHIVE.md` / `docs/TODO_CHANGELOG.md` / `docs/TODO-changelog.md`
