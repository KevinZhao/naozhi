---
title: Dashboard memory link 渲染（[[slug]] → popover）
status: Draft v1 (2026-05-23)
author: Kevin
---

# RFC: Dashboard memory link 渲染（方案 A）

## 背景

Claude 的 auto-memory 系统用 `[[slug]]` 作为记忆之间的交叉引用语法（系统 prompt 明确：
"link to related memories with `[[name]]`"）。该 slug **本应只出现在 `~/.claude/projects/<proj>/memory/*.md` 文件内**，
但模型偶尔会把内部交叉引用直接吐到对话输出里，例如最近一张计划表"触发于"列里出现了
`[[feedback_closed_book_recall_only]]`。

对用户来说，光秃秃的 slug 既无信息又像 bug。本 RFC 描述把 dashboard 渲染层做最小改造，让这种引用
变成可悬浮预览的内联链接。

非目标：

- **不**改飞书 / 终端 / 其它渠道的渲染（飞书富文本不支持自定义协议；终端 OSC 8 体验差）。
- **不**做编辑器（方案 C）；**不**做右侧抽屉（方案 B 留作后续增量）。
- **不**改模型行为本身（不试图阻止模型把 `[[]]` 输出到对话）。

## 用户决策

| 维度 | 选择 |
| --- | --- |
| 查找范围 | 全项目搜索：先当前项目，再 `~/.claude/projects/*/memory/`，命中外部时 UI 标注 |
| 触发方式 | hover 300ms 显示 + 点击固定（再次点击外部 / ESC 关闭） |
| 内容粒度 | 全文 markdown 渲染（去 frontmatter） |

## 整体架构

```
[AI 文本含 "[[foo]]"]
        │
        ▼  (frontend) inlineMd 替换为 <span class="md-memlink" data-slug="foo">foo</span>
        │
        ▼  hover/click → fetch /api/memory/foo
        │
        ▼  (backend) MemoryHandler 查找 + 解析
        │
        ▼  返回 {found, scope, project, name, description, type, body}
        │
        ▼  popover 渲染（复用 inlineMd / renderMd）
```

## 后端契约

### 路由

```
GET /api/memory/{slug}
```

走与其它 dashboard API 一致的 auth / IP rate limit。

### slug 校验

- 正则：`^[a-zA-Z0-9_\-]{1,64}$`
- 任何不匹配 → 400，不查盘。

### 查找顺序

1. **当前项目**：`<projectsDir>/<encodedCurrentProject>/memory/<slug>.md`
2. **其它项目**（按目录字母序遍历，第一个命中即返回）：
   `<projectsDir>/*/memory/<slug>.md`

`projectsDir` 默认 `~/.claude/projects`，可通过 `CLAUDE_PROJECTS_DIR` 环境变量覆盖（便于测试）。
"当前项目"通过 `os.Getwd()` 编码（替换 `/` 为 `-`）映射到目录名；查找逻辑容忍找不到当前项目目录。

### 路径白名单

- 用 `filepath.Clean` 后用 `strings.HasPrefix(p, projectsDir+"/")` 二次校验，**绝不**允许结果落在 `projectsDir` 之外。
- slug 正则已禁掉 `..` 和 `/`，这里是纵深防御。

### Frontmatter 解析

支持 YAML frontmatter（`---` 包夹），提取 `name` / `description` / `metadata.type`。
解析失败时不报错，回退为空 metadata + 整文件作为 body。

不引入 yaml 库依赖：手写极简扫描（按行匹配 `key: value` / `metadata:` 嵌套一层）。

### 响应

```jsonc
// 200
{
  "found": true,
  "scope": "current" | "external",
  "project": "naozhi",                  // 仅 external 时有意义
  "slug": "feedback_closed_book_recall_only",
  "name": "feedback-closed-book-recall-only",
  "description": "学习 feedback：仅闭卷默写算建立反射",
  "type": "feedback",
  "body": "...markdown 原文（已去 frontmatter）..."
}

// found=false（200，slug 校验通过但文件不存在）
{ "found": false, "slug": "..." }

// 400 slug 非法 / 路径异常
{ "error": "invalid_slug" }
```

**不**在后端做 markdown → HTML 渲染，body 直接交给前端 `renderMd`，复用现有 sanitize / KaTeX / 代码块逻辑。

### 限速

- 复用 `ipLimiter`，每 IP 10/s 突发 20。memory 文件极小（<10KB），开销主要是 `os.Stat` + `os.ReadFile`。

### 缓存

- 客户端：`Cache-Control: private, max-age=30`
- 服务端：不缓存（文件可能被人手改，30s TTL 足够）

## 前端契约

### inlineMd 改造点

`internal/server/static/dashboard.js` `inlineMd()`，在 `[link](url)` 之前插：

```js
s = s.replace(/\[\[([a-zA-Z0-9_\-]{1,64})\]\]/g, function (_, slug) {
  return '<span class="md-memlink" data-slug="' + escAttr(slug) + '" tabindex="0">[[' + slug + ']]</span>';
});
```

正则严格限定字符集，避免误吞普通 `[[]]`。

### popover 组件

单例浮层（DOM 上只有一个 `#mem-popover`），事件委托：

- `mouseover` 进入 `.md-memlink`：300ms 后取数据，渲染浮层定位到目标元素下方
- `mouseout` 离开：若未"固定"，200ms 后隐藏（鼠标进入浮层期间不计时）
- `click`：fetch（如未取过）+ 显示 + 设为"固定"
- 点击浮层外部 / 按 ESC：取消固定 + 关闭

浮层内容：

```
┌──────────────────────────────────────┐
│ [feedback] · 来自 gaokao 项目          │  ← header（type chip + scope）
│ feedback_closed_book_recall_only      │  ← slug
│ 学习 feedback：仅闭卷默写算建立反射     │  ← description
├──────────────────────────────────────┤
│ <renderMd(body)>                       │
│ ...                                    │
└──────────────────────────────────────┘
```

外部项目命中显示 "来自 gaokao 项目"，当前项目不显示来源。

### 失败处理

- 404：浮层显示 "未找到该记忆"，slug span 改用 `.md-memlink-broken` 样式（虚线点点 + 灰色），
  下次 hover 直接跳过 fetch。
- 5xx / 网络错误：浮层显示 "加载失败"，单次错误不降级 span。

### 客户端缓存

模块级 `Map<slug, response>`，命中即返回，跨多次 hover/click 不重复请求。
登出 / 刷新页面自然清空，不持久化。

## 安全

| 威胁 | 缓解 |
| --- | --- |
| 路径穿越（slug=`../../etc/passwd`） | 正则 + `filepath.Clean` + `HasPrefix` 双层 |
| XSS（记忆 body 含 `<script>`） | renderMd 已做 esc/sanitize，复用即可 |
| 拒绝服务（频繁 fetch） | ipLimiter（已有）+ 客户端 Map 缓存 |
| 信息泄漏（dashboard 用户读其它项目记忆） | dashboard auth 已要求登录；记忆本身存在用户家目录，无新增暴露面 |
| frontmatter YAML 注入 | 自写解析器只取已知字段，不 eval |

## 实现清单

| # | 文件 | 说明 |
| --- | --- | --- |
| 1 | `internal/server/dashboard_memory.go` | Handler + 解析 + scope 查找 |
| 2 | `internal/server/dashboard_memory_test.go` | 404 / 路径穿越 / scope / frontmatter |
| 3 | `internal/server/dashboard.go` | 注册路由 |
| 4 | `internal/server/server.go` | `memoryH` 字段 |
| 5 | `internal/server/static/dashboard.js` | inlineMd + popover 组件 |
| 6 | `internal/server/static/dashboard.html` | popover container + CSS |

## 不做的事

- 不在飞书 / 终端做对应渲染
- 不做记忆编辑（方案 C，未来增量）
- 不做记忆全文搜索（属于另一个功能）
- 不做后端 markdown 渲染
- 不缓存到 sessionStorage

## 风险与未决

- **当前项目识别**：用 `os.Getwd()` 编码到目录名是 Claude Code 的约定，不是契约。如果 Claude 改了规则，"当前项目优先"会退化为"全项目搜索"——不影响功能正确性，只影响命中顺序。
- **跨项目命中歧义**：多个项目都有同名 slug 时只返回字母序第一个。后续可考虑在 popover 加"在 N 个项目中找到"提示，但 v1 先不做。
