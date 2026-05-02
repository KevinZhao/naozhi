# naozhi i18n 设计方案 — Review 历史

> 主文档 `docs/design/i18n.md` 状态 APPROVED v4（2026-04-29 冻结）。
> 本文档归档 v1 → v2 → v3 → v4 四轮独立 review 的修复对照表，控制主文档篇幅。
> 每轮 review 的问题编号规则：B*/H*/M*/L*/D* 是 v1→v2；N*/NN* 是 v2→v3；NNN* 是 v3→v4（无，v4 即 APPROVED）。

---

## v1 → v2（2026-04-29 第一轮 review）

v1 首发时未经 review。下列问题是 review 提出后在 v2 修复的。

| 编号 | v1 问题 | v2 修复 |
| --- | --- | --- |
| **B1** | 飞书 locale 来源错 —— 声称 webhook 带 `i18n_info.user.language`，事实不存在 | §3.1 改为"启发式 + `/lang` override + default"；事实表列出各平台真实 API |
| **B2** | G4 目标与 §3.5 实现矛盾（localStorage vs cookie） | G4 改为 cookie 主存储 + localStorage 辅助；后端唯一事实源 |
| **H1** | Placeholder 用 `%s` positional + 声称"missing 不 panic 与 Sprintf 矛盾"| 改命名 `{name}` + 明确 missing 行为 + CI 校验占位符对齐 |
| **H2** | 非 IM 场景 locale 未定义（cron / scratch / 内部错误）| §3.5 新增"非 IM 场景策略"表，引入 `session.Locale` 固化 |
| **H3** | CI 用 regex 扫 `t()` 精度差（与 `*testing.T`、`strings.Title` 等撞车）| 升级为 `go/ast` + `__t`（双下划线唯一标识符）regex |
| **H4** | Bundle 并发安全性未声明 | §3.4 Godoc 明确 immutable + no Reload |
| **H5** | html/template `jsonSafe` 自定义 func 未实现，XSS 风险 | §3.7 用 `html/template` + `template.JS` + `json.Marshal` |
| **H6** | Accept-Language q-value 未提 | §3.2 引入 `x/text/language.ParseAcceptLanguage` + `Matcher` |
| **M1** | 优先级图混淆 Dashboard / IM 两条独立链 | §3.1 拆为两张独立图 |
| **M2** | Fallback 表只举例不完整 | §3.2 完整表 + 白名单外返回空 |
| **M3** | NormalizeLocale 未覆盖 `.UTF-8` / 大小写混合 / 仅 prefix | §3.2 七步规则 |
| **M4** | 体积数字矛盾（40 KiB vs 30 KiB） | §8 统一 <60 KiB |
| **M5** | 测试迁移策略仅一句 "Contains" | §3.9 ABC 三档 + PR6 专门升级 |
| **M6** | embed.FS 热更新未提 | §4 引入 `NAOZHI_DEV_LOCALE_DIR` 逃生舱 |
| **M7** | YAML 写法规范未提 | §3.10 + `docs/i18n-yaml-style.md` + round-trip 测试 |
| **M8** | Cookie 属性未提 | §3.7 明确 SameSite=Lax / 无 HttpOnly / Max-Age=1y |
| **M9** | Slack rate limit / LRU 位置未说 | §3.1 明确放 transport 层内 |
| **M10** | `/lang` key 空间未预留 | §1.2 NG1 + §3.11 命令保留词覆盖 |
| **D1** | 术语表标"待写" | §3.11 + PR1 交付物 |
| **D2** | LLM-facing vs user-facing 未区分 | §3.12 判定表 + `//i18n:ignore` 注释机制 |
| **D3** | 命令保留词未列 | §3.11 保留词清单 |
| **D4** | prompt 语言提示简单"不做" | §7 开放问题 6 改为 backlog + 预留 `{{system_locale_hint}}` 占位 |

---

## v2 → v3（2026-04-29 第二轮 review）

v2 解决 v1 Blocker 后，新的 review 又发现 v2 自身引入的问题。

### v3 修复的 Blocker（NB = New Blocker）

| 编号 | v2 问题 | v3 修复 |
| --- | --- | --- |
| **NB1** | `session.Locale` 首条消息**永久锁**，启发式判错无自愈通道 | §3.1 三档置信度（heuristic/platform/user）+ `/lang` 降为一期 |
| **NB2** | Slack `users_info` LRU cache key 未定义，多 workspace `user_id` 撞车污染 | §3.1 cache key 明确 `team_id:user_id` + 回归测试 |

### v3 修复的 High（NH）

| 编号 | v2 问题 | v3 修复 |
| --- | --- | --- |
| **NH1** | Fallback 表步骤 6 "其他白名单 locale 原样 canonical" 是死代码（白名单只有两项）| §3.2 删除步骤 6；未来扩展再恢复 |
| **NH2** | `zh-TW → zh-CN` 归并伤害繁体用户体验，无注释无开放问题 | §3.2 注释 + §7 开放问题 7 + `_meta.script: Hans` 预留 |
| **NH3** | lint `//i18n:ignore` 语法未定，只提机制名 | §3.8 行级/同行/块级语法规范 + 错误用法示例 |
| **NH4** | renderHTML 每次全量 `json.Marshal` 字典 | §3.4 / §3.7 `FlatMessagesJS` 预计算缓存 |
| **NH5** | `POST /api/i18n/locale` CSRF 未提 | §3.7 验证规则（auth + Content-Type + body 校验） |
| **NH6** | `ResolveForDashboard(*http.Request)` 让 i18n 包依赖 net/http | §3.4 改纯字符串签名 `(cookie, query, acceptLang)` |

### v3 修复的 Medium/Low/Doc（NM/NL/ND）

| 编号 | v2 问题 | v3 修复 |
| --- | --- | --- |
| NM1 | 启动时批量 migration 放大启动时间 + race | §3.3 改懒补（Load 时内存填，Save 时落盘）|
| NM2 | FlatMessages 每次 copy 浪费（Bundle 不可变为何 copy）| §3.4 改返回预计算 `template.JS`，O(1)|
| NM3 | Heuristic 空串语义不清，Resolver 未显式处理 | §3.4 签名 `(locale, confident bool)` |
| NM4 | min_chars 单位歧义（字节 vs 字符） | §4 改名 `min_runes`，明确 `utf8.RuneCountInString` |
| NM5 | scratch prompt 语言二选一未决策 | §3.5 决策：恒英文（LLM 稳定性最佳）|
| NM6 | lint CJK 晚开启 → 合并冲突 | §3.8 基线文件 `i18n-cjk-baseline.txt` + 只拦截增量 + PR6 清零 |
| NM7 | `/api/i18n/locale` server 验证未定义 | §3.7 六条验证规则 |
| NM8 | `__t` regex 边界不严 | §3.8 regex 改 `[^\w.]__t\s*\(` |
| NM9 | 命令中文别名未提 | §7 开放问题 8：不做，消息暗示 |
| NM10 | glossary 工时低估（PR1 要翻译 200+）| §5 PR1 改为 ~20 术语首版 + 后续 PR 增补 |
| NL1 | Bundle 内层 map Godoc 未提未来 Reload 约束 | §3.4 明确"未来 Reload 必须 atomic.Pointer swap 保 read-only"|
| NL2 | supported_locales 空配置 → Load 未校验会 panic | §4 启动校验 defaultLocale / supported / 包含关系 |
| NL3 | DEV_LOCALE_DIR 生产攻击面 | §4 `-tags dev` build 分离 `load_dev.go` / `load_prod.go` |
| NL4 | 性能回归未验收 | §8 新增两条 benchmark 线：Dispatcher <1μs / renderHTML <500μs |
| NL5 | L1-L5 合并一格，附录不细 | v3 附录展开 |
| ND1 | v2 重复对照（header + §10）| v3 header 简化，§10 保留详细 |
| ND2 | __t args null 抛错（`in args` 对 null 崩） | §3.7 加空值守卫 |
| ND3 | PR 工时低估（翻译未计入）| §5 通用注释 + PR5b 修正 ~1000 行 |

---

## v3 → v4（2026-04-29 第三轮 review）

v3 解决 NB/NH 后，新一轮 review 又发现 v3 自身引入的漏洞。

### v4 修复的 Blocker（NNB = Next New Blocker）

| 编号 | v3 问题 | v4 修复 |
| --- | --- | --- |
| **NNB1** | `/lang auto` 清除语义缺失 —— `userOverride` 字段没有"清除"信号，`ResolveIM` 会永远走到 `LocaleSource=="user"` 分支锁死 | §3.1 Step 1/2 明示 dispatcher 在调用 ResolveIM **之前**短路清除 `sess.Locale = "" / sess.LocaleSource = ""`；`ResolveIM` 不接受清除信号 |
| **NNB2** | `TestLint_*` 各 test 的职责与边界文档不清，解除 skip 时机易误会 | §3.9 表格明示每个 test **检查什么 / 不检查什么** + 解除时机 |

### v4 修复的 High（NNH）

| 编号 | v3 问题 | v4 修复 |
| --- | --- | --- |
| **NNH1** | Printer 持 map 引用与 Godoc 声称"未来 Reload 用 atomic.Pointer swap"冲突（swap 后旧 Printer 仍指旧 map）| §3.4 Printer 改为持 `*Bundle` 指针，每次 T() 走 bundle 查表；为未来 atomic.Pointer swap 预留 |
| **NNH2** | `TestLint_NoHardcodedCJK` vs `TestLint_NoHardcodedCJKDelta` 命名在 §3.8 / §5 / §8 之间漂移 | §3.9 统一为 `TestLint_NoHardcodedCJKDelta`；PR6 清零后名字保留，机制保留 |
| **NNH3** | `/lang` 命令帮助文本未进 `/help` 输出，PR3a 描述没强调联动 | §3.1 强调 `/help` 必须新增 `/lang` 行；§5 PR3a 显式列入 |
| **NNH4** | 懒补承诺"下次自然 Save 落盘"在某些情况下假（ResolveIM 恰好算出相同值就不 Save）| §3.3 改措辞"懒补是透明内存修正，不保证落盘；幂等"|
| **NNH5** | v3 API 把 `FlatMessages` 删掉了，CI lint 测试无处取字典对照 | §3.4 恢复 `FlatMessages` API + 新增 `FlatMessagesJS` 并存（前者用于测试 / lint，后者用于 render）|

### v4 修复的 Medium（NNM）

| 编号 | v3 问题 | v4 修复 |
| --- | --- | --- |
| NNM1 | `_meta.script: Hans` 字段声明预留但无代码消费，翻译者困惑 | §3.4 Bundle 新增 `scripts map[string]string` 字段 + `Scripts()` API + Load 读 `_meta.script` + `TestScripts_ReadsMetaField` 测试覆盖 |
| NNM2 | `LocaleCache` 接口放 `internal/platform/slack/` 阻碍其他 transport 复用 | §3.2 移到 `internal/i18n/locale_cache.go`，带 `Scope` 参数支持多 workspace |
| NNM3 | `POST /api/i18n/locale` 错误响应未走 i18n | §3.8 错误 body 走 `ResolveDashboard` + `T("api.i18n.*")` |
| NNM4 | `ResolveIM` 5 个 string 参数顺序易调错 | §3.4 改 `ResolveIM(IMResolveInput{...}) (locale, source)` 结构体签名 |
| NNM5 | `Heuristic` 成了 Bundle 方法但 Bundle 无 HeuristicCfg 字段 | §3.4 Bundle 新增 `heuristicCfg` 字段 + `Load` 接受 cfg 参数 |
| NNM6 | Slack `team_id` 从哪来未确认；单 workspace 部署可能缺 | §3.2 补注：单 workspace 允许 `scope=""`，`slack.single_workspace` 配置开关，测试覆盖两路径 |
| NNM7 | `heuristic.enabled=false` 时 Heuristic 返回什么算法没定 | §3.4 明示 `enabled=false` → `("", false)`；§3.1 算法只看 `confident` |
| NNM8 | session.Locale 向前兼容依赖 Go json 默认行为，文档未说 | §3.3 明示"依赖 Go JSON 忽略未知字段；不要改 strict mode" |
| NNM9 | PR5b ~1000 行仍低估 | §5 PR5b 修正 "~1200 行代码 + ~500 行 YAML + 1 周翻译 review"|
| NNM10 | `FlatMessagesJS` 未说 JSON 序列化稳定序 | §3.4 Godoc 明示 key 排序；§8 新增字节稳定 benchmark |

### v4 修复的 Low / Doc（NNL / NND）

| 编号 | v3 问题 | v4 修复 |
| --- | --- | --- |
| NNL1 | `//i18n:ignore` 块级示例用了 `//go:build !lint_cjk`，混淆编译约束与 lint 机制 | §3.9 块级改为 `//i18n:ignore-file reason="..."` 专用形式 |
| NNL2 | `lint_cjk` build tag 纯虚构从未定义 | 删除；§3.9 明示 i18n:ignore 独立机制 |
| NNL3 | `/lang` 命令 "~80 行" 过乐观 | §5 PR3a 修正 `/lang` ~150 行 |
| NNL4 | 开放问题 7（繁简独立）未定优先级 | §7 补触发阈值：≥5 条 zh-TW 反馈或 6 个月（2026-10-29）主动 review |
| NNL5 | 附录 §10 随 review 轮数爆炸 | 抽到独立 `docs/design/i18n-review-history.md` 文件（本文件）|
| NND1 | 状态仍 `DRAFT v3`，但已修 3 轮 | v4 升为 **APPROVED v4**，决策冻结 |
| NND2 | 无图示解释"三档置信度"| §2.1 ASCII 状态转移图 |
| NND3 | §5 PR 依赖图未画 | §5.1 DAG + 并行指导 |
| NND4 | `NAOZHI_DEV_LOCALE_DIR` 与 `-tags dev` 交互 UX | §4 新增"开发者 workflow"段 + `make dev` target + README 更新 |

---

## 统计

| 轮次 | Blocker | High | Medium | Low | Doc | 合计 |
| --- | --- | --- | --- | --- | --- | --- |
| v1 → v2 | 2 | 6 | 10 | - | 4 | 22 |
| v2 → v3 | 2 | 6 | 10 | 5 | 3 | 26 |
| v3 → v4 | 2 | 5 | 10 | 5 | 4 | 26 |
| **累计** | **6** | **17** | **30** | **10** | **11** | **74** |

---

## v4 APPROVED 冻结

v4 为决策冻结版本。后续任何结构性变更走独立 ADR（`docs/adr/0001-*.md`）。

冻结基线 commit：（PR1 合入时填入）
