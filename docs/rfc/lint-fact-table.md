# RFC: lint-fact-table — 设计稿事实速查表自动校验（LINT-FACT-TABLE）

> **状态**：Draft v1（2026-05-28）
> **作者**：naozhi team
> **创建日期**：2026-05-28
> **范围**：新建 `tools/lint-fact-table/` 工具，扫描 markdown 设计稿中的关键数字 token 与事实速查表对账，漂移即 fail
> **关联文档**：
>  - [docs/design/server-split-phase4-design.md](../design/server-split-phase4-design.md) §0 事实速查表 + 修订纪律
>  - [docs/design/server-split-phase4-baseline.md](../design/server-split-phase4-baseline.md)
> **触发条件**：v0.6.1 §0 修订纪律第 5 条钉死的必交付物——Phase 0 PR description 必须含 `lint-rule-6 RFC: <link>` 才能合并

---

## 1. 动机

### 1.1 痼疾：长设计稿的事实漂移

server-split-phase4 设计稿从 v0.1 到 v0.6.1 经过 11 轮 reviewer 反馈，文档体量从 200 行涨到 1537 行。每轮 review 都暴露**同一份事实在文档多处出现，单作者用 grep 同步必漏**：

| 版本 | 痼疾示例 | 漏改处数 |
|---|---|---|
| v0.5 | Hub 字段 37 → 43 | 9 处中只同步 4 处 |
| v0.5 | PR 数 11 → 13 | 4 处不一致 |
| v0.5 | 节奏 9-10 周 → 13-15 周 | §6.1 改了，§十一替代方案表没改 |
| v0.6 | exemptions.yaml limit 800 → 500 | yaml 模板与 §9.2 阈值矛盾，linter 启用即误豁免 |

**根本原因**：设计稿是单一文件，但事实在 §一/§二/§五/§九.1/§7.3/§十一 等多处反复引用，**没有机器可读的"单一真相源"**。每次修订 reviewer 都在 catch up grep，效率低且漏检。

### 1.2 v0.6 的部分缓解：§0 事实速查表

v0.6 引入"§0 事实速查表"——把所有关键事实集中在文档头部的一张表里，正文从此读、不重复定义。配套的"修订纪律"4 条：

1. PR description 第一行声明 `update §0 fact-table: <field>: <old> → <new>`
2. grep 全文确认所有引用同步，单 commit 提交
3. 每个 phase merge 时 baseline 实测对账
4. 本表为单一真相源

**纪律 1-4 全靠人维护——这正是 v0.5 痼疾的复演风险**。本 RFC 提议加纪律 5：用 lint rule 6 把约定升级为机器约束。

### 1.3 不只 server-split

后续 cron / cli / dispatch 等 god 系统拆分都会写 1000+ 行 RFC（process-split v3 已经 1500+ 行）。一份可复用的"speech 表 lint"是**项目级方法论沉淀**——单次实现 + 应用到所有大型 RFC。

---

## 2. 设计目标

| # | 目标 | 量化指标 |
|---|---|---|
| 1 | 扫 markdown 关键数字 token | 支持 47/13/21313/≤40/13-15周 等格式 |
| 2 | 与速查表对账 | speech 表用 markdown 表格 + 元数据标注 |
| 3 | 漂移即 fail | CI mode=warn / fail 双轨；Phase 0 后 mode=fail |
| 4 | 工具单文件 | `tools/lint-fact-table/main.go` ≤ 500 行 |
| 5 | 零误报 | 速查表覆盖所有数字；非速查表数字不扫（白名单机制）|
| 6 | 可复用到其他 RFC | speech 表语法 = 通用 markdown，任何文档可加 |

### 不做什么

- ❌ 不解析自然语言判断"这是不是事实声明"——只扫 token + 速查表对账
- ❌ 不替代 §0 修订纪律——纪律是行为约定，lint 是机器兜底
- ❌ 不与 server-split-phase4 设计稿耦合——通用工具，speech 表语法独立
- ❌ 不扫所有 markdown 文件——只扫含 `<!-- fact-table: ... -->` 标注的设计稿

---

## 3. speech 表语法（核心）

### 3.1 标注格式

每个设计稿在速查表前后用 HTML 注释包围：

```markdown
<!-- fact-table:start name="server-split-phase4" -->

| 维度 | v0.6 实测值 | v0.4 写过 | v0.5 写过 | 备注 |
|---|---|---|---|---|
| Server struct 字段 | **47** | 47 | 47 | 一致 |
| Hub struct 字段 | **47** | 37 | 43 | 多次校准 |
| ...（更多行）

<!-- fact-table:end -->
```

linter 解析步骤：
1. 找到 `<!-- fact-table:start name="<id>" -->` ... `<!-- fact-table:end -->` 包围的表格
2. 第一列是"维度"（key），第二列是"实测值"（value）——其余列为历史记录，不参与 lint
3. **粗体 token 抽取**：`**47**` / `**21313 行**` / `**≤ 40**` / `**13-15 周（含观察期）**` 提取为 token

### 3.2 token 引用语法（正文）

正文要引用速查表的事实，用粗体 + 同样的字符串：

```markdown
Server struct 47 字段，Phase 5 后 ≤ 12 字段（v0.6 实测）。
                ^^^^^                 ^^^^^^
                ↑ 与速查表"Server struct 字段 = 47"对账 → ✓
                                      ↑ 速查表无此 token，但符合"Server 字段 ≤ 12"目标声明，需白名单
```

### 3.3 白名单（非速查表 token）

不是所有粗体数字都是事实——**例如**：百分比、阈值、目标值、外部数据。这些用 `<!-- lint:allow:<reason> -->` 标注豁免：

```markdown
减幅 **-77%** <!-- lint:allow:derived-percentage -->（v0.4 写 -71% 基于 17143）
```

### 3.4 token 漂移检测

扫描器对每个抽出的粗体 token：
- 如果在速查表 key 列里能找到对应描述（启发式：上下文 ≤ 50 字符内含 "Server" / "Hub" / "字段" / "PR" 等）→ 与 value 列对账
- 如果对账成功 → 静默
- 如果对账失败（值不一致）→ **fail**：报告 `<file>:<line>: token "47 字段" 与速查表 "Hub struct 字段 = 47" 不一致（找到 43）`
- 如果未在速查表 + 无白名单标注 → **warn**：建议加入速查表或加白名单

---

## 4. 实现范围

### 4.1 工具结构

```
tools/lint-fact-table/
  main.go              入口（CLI）+ SARIF 输出
  parser.go            speech 表 markdown 解析（fact-table:start ... :end）
  scanner.go           扫描正文 token（粗体 + 上下文）
  matcher.go           速查表 key 匹配 + value 对账
  exemption.go         白名单解析（<!-- lint:allow:<reason> -->）
  testdata/
    valid_doc.md       通过 lint 的样例
    invalid_token.md   token 漂移样例（fail）
    missing_table.md   无速查表样例（warn）
```

预计 **~400 行 Go + 100 行测试样例**。复用 `gopkg.in/yaml.v3` 已有依赖；markdown 解析用 `github.com/yuin/goldmark`（标准 lib）或简单字符扫描（推荐——避免新依赖）。

### 4.2 CLI 接口

```bash
# 扫单文件
lint-fact-table docs/design/server-split-phase4-design.md

# 扫多文件
lint-fact-table docs/design/*.md docs/rfc/*.md

# warn 模式（CI 默认）
lint-fact-table -mode warn docs/design/server-split-phase4-design.md

# fail 模式（Phase 0 完成后切到此）
lint-fact-table -mode fail docs/design/server-split-phase4-design.md

# SARIF 输出（GitHub PR Annotations）
lint-fact-table -sarif docs/design/server-split-phase4-design.md > findings.sarif
```

### 4.3 CI 集成

加到 `.github/workflows/ci.yml`：

```yaml
- name: Lint design docs (fact-table)
  run: |
    go run ./tools/lint-fact-table/ \
      docs/design/server-split-phase4-design.md \
      docs/design/server-split-phase4-baseline.md
  continue-on-error: true   # Phase 0 期间 warn 不卡 PR
```

Phase 0 完工后切 fail：

```yaml
  continue-on-error: false   # mode=fail
```

---

## 5. 阶段化交付

### 5.1 Phase 0 实施（本 RFC 落地）

| 阶段 | 交付物 | 验收 |
|---|---|---|
| **Phase 0-RFC**（本 RFC PR） | `docs/rfc/lint-fact-table.md` Draft v1 | reviewer 批准 |
| **Phase 0a**（实装） | `tools/lint-fact-table/` main.go + parser + scanner + 1 个 demo case；CI 集成 mode=warn | `make lint-fact-table` 跑 server-split-phase4-design.md 0 fail |
| **Phase 0b**（速查表反向标注） | server-split-phase4-design.md §0 加 `<!-- fact-table:start -->` ... `:end -->` 包围；正文粗体 token 加白名单标注 | lint warn 输出 ≤ 5 项 |
| **Phase 5 完工后** | mode=warn → mode=fail | exemptions.yaml 无残留 + lint 0 fail |

### 5.2 后续可扩展（不在 v1 范围）

- **rule 6.1**：扫 yaml / go 代码注释里的关键数字 token（如 `// 47 字段` 不一致）
- **rule 6.2**：跨多个设计稿的全局事实速查表（如 cron-sysession-merge 的"7 phase"也在 design 文档里）
- **rule 6.3**：自动生成速查表（从 baseline.md 推导）

---

## 6. 风险与缓解

| 风险 | 概率 | 缓解 |
|---|---|---|
| 误报：粗体数字不是事实声明（如百分比、外部数据）| 高 | 白名单 `<!-- lint:allow:<reason> -->` 显式标注；启发式上下文匹配（≤ 50 字符）|
| 漏报：事实没用粗体写 | 中 | speech 表纪律强制粗体 + reviewer 习惯训练 |
| 实装复杂度低估 | 低 | Phase 0a 单文件 ≤ 500 行，参考 lint-server-handlers/main.go 347 行同款规模 |
| markdown 表格解析边界条件 | 中 | 用最简字符扫描，不依赖完整 markdown parser；testdata 覆盖边界 |
| 速查表自身写错 | 低 | speech 表是单一真相源——错也会全文扫出；reviewer 必看 |

---

## 7. 未解决的问题

1. **token 单位识别**："13-15 周" vs "13 PR" vs "47 字段"——这些都是粗体但单位不同。v1 用纯字符串匹配（启发式），v2 可加单位语义解析。
2. **跨文档速查表**：server-split-phase4-design 引用的 `Hub 47 字段` 也在 baseline §3 出现。当前 baseline 不带 fact-table 标注。**v1 范围**：只扫 design.md；baseline 作为数据源不上 lint。
3. **Phase 0 后的速查表更新流程**：Phase X merge 时 baseline 重新实测，speech 表也要更新。但 lint rule 6 不强制 baseline 数字一致——它只对账 speech 表内部 + 正文。**v2 可加 cross-doc 模式**。

---

## 8. 验收清单

- [ ] `tools/lint-fact-table/main.go` 实装（~400 行）
- [ ] testdata 含 3 个 case（valid / invalid_token / missing_table）
- [x] CI 集成 mode=warn（`.github/workflows/ci.yml` lint-fact-table job）
- [x] server-split-phase4-design.md §0 加 `<!-- fact-table:start ... -->` 包围
- [x] Makefile `lint-fact-table` / `lint-fact-table-fail` targets
- [x] Phase 0a PR description 含 `lint-rule-6 RFC: docs/rfc/lint-fact-table.md`

## 8.1 v1 实装状态（2026-05-28 Phase 0-LFT 落地）

- ✅ tools/lint-fact-table/main.go ~510 行 + main_test.go 7 个单元测试 PASS
- ✅ testdata 3 个 case（valid_doc / invalid_token / missing_table）全 PASS
- ⚠️ 真实 design v0.6.1 跑 7 个 token_drift false positive（启发式过激进）
- ✅ CI mode=warn 不阻塞 PR；7 个 noise 给作者提示

## 8.2 v2 fine-tune backlog

| 启发式问题 | 真实例子 | v2 修复 |
|---|---|---|
| nearestKeyContext 跨段落 | 介绍段 `**21313**` 错配 Server 字段 key | 加最近 5 粗体 token 反向回溯 |
| normValue 不处理修饰文字 | `**47 → ≤ 12**` / `**47 字段维持不变**` | strip-trailing-text 模式 |
| value-type ambiguous | `**≤ 40**` (Hub) 错配 "Server ≤ 12" | 加 value-type signature 比对 |
| 阈值 vs 事实 | `**500 行**` 是 server 包硬上限 | 加 "threshold" 白名单标注 |
| 描述性数字 | `**默认 7 天观察期**` | normValue 处理"默认 N 天" 模式 |

CI 走 `continue-on-error: true`，本 v1 不阻塞 PR。v2 PR 精化启发式。

---

## 9. 修订历史

| 版本 | 日期 | 修订 |
|---|---|---|
| v1 | 2026-05-28 | 初稿 — Draft；server-split-phase4-design v0.6.1 §0 修订纪律 5 钉死的必交付物 |

---

## 10. 参考

- [docs/design/server-split-phase4-design.md](../design/server-split-phase4-design.md) §0 事实速查表 + 修订纪律
- [tools/lint-server-handlers/main.go](../../tools/lint-server-handlers/main.go) — 同款 lint 工具实现参考（347 行）
- [.github/workflows/ci.yml](../../.github/workflows/ci.yml) — CI 集成位置
