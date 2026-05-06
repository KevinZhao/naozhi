# PDF 附件上传设计文档

> **状态**: 设计提案 → 实现中
> **作者**: naozhi team
> **创建**: 2026-05-06

## 1. 目标

让 dashboard 用户上传 PDF 文件作为附件，与文本一同发送给 Claude CLI，由 CC 原生能力解析 PDF 内容。

**非目标** (本次不做):
- IM 渠道（飞书/Slack/微信）接收 PDF
- OCR / 文本抽取 / 缩略图
- 其他文档格式（docx / xlsx / pptx）——仅 PDF

## 2. 背景与关键事实

### 2.1 CC 的 PDF 解析机制——两条路径

**路径 A: Anthropic Messages API 原生 document content block**

```jsonc
{ "type": "document",
  "source": { "type": "base64", "media_type": "application/pdf", "data": "<b64>" } }
```

CLI 2.1.126 二进制里 `application/pdf` 和 `"document"` 字符串都在，内置支持。但：

| 约束 | 来源 | 值 |
|---|---|---|
| 单 PDF 上限 | Anthropic 文档 | 32 MB, 100 页 |
| shim 单行上限 | `internal/shim/server.go:27` | **16 MB** |
| CLI stdin 单行 | `internal/cli/process.go:45` | **12 MB** |
| CLI bufio.Scanner | `process.go:38` | 10 MB |
| base64 膨胀 | — | +33% |

叠加后 inline document block 只能塞 **~7 MB 原始 PDF**，远小于 32 MB 官方上限。多 PDF 场景更紧张。提高 shim 上限会动到所有渠道的安全护栏，收益不值。

**路径 B: 落盘到 workspace + 引导 Claude 用 Read 工具（推荐）**

1. Dashboard 上传 PDF
2. naozhi 把字节原封不动写到 `<workspace>/.naozhi/attachments/<uuid>.pdf`
3. 发送消息时在 text 里追加：
   > 用户上传了 PDF 附件：`.naozhi/attachments/xxx.pdf`。请用 Read 工具读取它。
4. Claude 调 Read tool → CLI 内部走的正是**官方的 PDF→Anthropic API 通路**（Read 工具对 .pdf 的实现会把 PDF 字节以 document block 递交给 API）

优势：
- 完全绕过 12 MB stdin 行长上限（只传路径字符串）
- 走 Claude **对话历史里的 Read tool_use** — 后续 follow-up 问题、session resume 都能自然引用（文件留在盘上）
- 复用现有 `project_files.go` 的 workspace 安全沙箱，不新增路径解析代码
- 32 MB Anthropic 上限留给单文件；多 PDF 用户自己裁剪
- session 迁移（`--resume`）重启后附件路径仍有效

劣势：
- 需要 workspace 目录可写（几乎所有会话都有，`cli.SpawnOptions.WorkingDir` 非空）
- `.naozhi/attachments/` 目录需要 TTL 清理，否则堆积
- 假如用户明确不希望附件污染 workspace（例如 workspace 是 git repo），我们得在 .gitignore 上加项 / 用 `$HOME/.naozhi/attachments` 而非 workspace 内

**决策**: 采用路径 B，落盘位置 `<workspace>/.naozhi/attachments/<yyyymmdd>/<uuid>.pdf`。后续如有需求再做路径 A 作为小文件快速路径。

### 2.2 现状盘点

- 上传入口 `dashboard_send.go:parseImageFile` (`:64-99`)：`image/` 前缀 + 白名单 jpeg/png/gif/webp，10 MB
- 暂存 `upload_store.go`：`cli.ImageData{Data, MimeType}`，TTL 10 分钟，per-owner 40 / 全局 100
- 发送路径 `sendParams.Images []cli.ImageData` → `dispatch.msgqueue` → `coalesce.allImages` → `proto.WriteMessage(text, images)` → `NewUserMessageWithMeta` 生成 `inputImageBlock`
- 前端 `dashboard.js:1793`：`<input accept="image/*">`

## 3. 设计

### 3.1 存储布局

```
<workspace>/
  .naozhi/
    attachments/
      2026-05-06/
        <uuid>.pdf     ← 原始字节
        <uuid>.meta    ← {orig_name, size, sha256, uploaded_at, owner, session_key}
      2026-05-05/      ← 24h 后由清理任务删除
```

- 使用**会话 workspace 目录**而非共享目录：每个会话天然隔离、session resume 仍可读、删除会话目录时一并清理
- 日期子目录便于 TTL 清理（直接 `rm -rf` 过期日期目录）
- UUID 文件名防重名 & 信息泄漏；原始文件名存 `.meta` 用于日志与 UI 显示
- `.meta` 而非数据库：与 naozhi "所有状态文件化" 的设计一致（见 CLAUDE.md）

### 3.2 类型扩展

`internal/cli/event.go`: `ImageData` 改名为 **`Attachment`**（通过 type alias 保留旧名过渡）：

```go
// Attachment is an inline user-message asset: image or a workspace file
// reference (PDF). When Kind == "image_inline", Data+MimeType carry the raw
// bytes for a direct content block. When Kind == "file_ref", Data is nil and
// WorkspacePath points to a file inside the session's workspace that Claude
// will Read via its native tool.
type Attachment struct {
    Kind          string  // "image_inline" | "file_ref"
    Data          []byte  // populated when Kind=="image_inline"
    MimeType      string  // always set
    WorkspacePath string  // relative path under workspace, set when Kind=="file_ref"
    OrigName      string  // user-facing display name, set for file_ref
}

// ImageData preserved as an alias for the legacy call sites during migration.
// New code uses Attachment directly. Final removal tracked in docs/TODO.md.
type ImageData = Attachment
```

这样 **dispatch / msgqueue / coalesce / send 全链路无需改动**（只要它们传的是 `[]Attachment`）。

### 3.3 消息编码

`NewUserMessageWithMeta` 按 `Kind` 分派：

```go
for _, att := range atts {
    switch att.Kind {
    case "image_inline", "":
        blocks = append(blocks, inputImageBlock{ /* 既有逻辑 */ })
    case "file_ref":
        // file_ref 不生成独立 content block —— 它只影响 text 前缀
        continue
    }
}
// text 拼接时追加 file_ref 提示
if refs := collectFileRefs(atts); len(refs) > 0 {
    text = prependFileRefHint(text, refs)
}
```

其中 `prependFileRefHint` 产出：

```
[系统: 用户上传了 1 个 PDF 附件，已保存到工作区。请使用 Read 工具读取以下文件后再回答用户问题:
  - .naozhi/attachments/2026-05-06/a1b2c3.pdf (原名: 合同.pdf, 1.2 MB)

用户消息:]
<原 text>
```

使用中文还是英文？参考 CLAUDE.md "Always respond in chinese"——但这是给 CC 的 system-style prompt，CC 自身是双语可读的，**用中文**以免语言切换让模型产生不一致反应。不对，再想想：CC 运行在英文 base prompt 下，给它的工具指令用英文更稳：

```
[User uploaded 1 PDF attachment to the workspace. Before answering, read it with the Read tool:
  - .naozhi/attachments/2026-05-06/a1b2c3.pdf (orig: 合同.pdf, 1.2 MB)

User message:]
<原 text>
```

英文版。原始文件名可能是中文，保留 UTF-8 原样。

### 3.4 HTTP 上传端点

改造 `dashboard_send.go`：

```go
func parseAttachmentFile(fh *multipart.FileHeader, allowPDF bool) (cli.Attachment, error) {
    // 1. mime 判定（按扩展名 + magic bytes 双重确认）
    // 2. size 分档: image 10 MB, PDF 32 MB
    // 3. 返回 Kind/Data/MimeType/OrigName（WorkspacePath 暂空，由发送路径落盘后填充）
}
```

**关键问题：PDF 字节何时落盘？**

选项 α: 上传时 (`handleUpload`) 立即落盘到一个**临时池**，发送时 move 到 workspace  
选项 β: 上传时只进内存 `uploadStore`，`handleSend` resolve `file_ids` 时再落盘

选 **β**。理由：
- 上传时还不知道 workspace（workspace 在 `handleSend` 的 req 里）
- 已有 `uploadStore` TTL / per-owner 配额机制可直接复用
- 用户上传后取消（刷新 / 换 session）时无需额外清理临时池

但 32 MB PDF 放内存有压力：`uploadStore` 当前 global cap 100 entries，若都是 PDF 最坏 3.2 GB。对策：
- 新增 `maxUploadBytes` 全局字节上限（例如 200 MB）
- 按字节计的 per-owner 上限（例如 64 MB），字节 accounting 与条数 accounting 并行

**落盘点放在 `sessionSend` 进入前**，`dashboard_send.go` 解析完 `file_ids` 拿到 workspace 后：

```go
// 把 Kind==file_ref 但 WorkspacePath=="" 的 Attachment 落盘到 workspace
for i, att := range images {
    if att.Kind == "file_ref" && att.WorkspacePath == "" {
        rel, err := persistAttachment(workspace, att, owner, key)
        if err != nil { /* 报错回滚 */ }
        images[i].WorkspacePath = rel
        images[i].Data = nil  // 释放内存，避免再被 coalesce 复制
    }
}
```

### 3.5 后端 Claude / ACP

- `ClaudeProtocol.WriteMessage`: 收到 `Attachment{Kind:"file_ref"}` → 不产出 content block；text 前缀由 `NewUserMessageWithMeta` 统一处理
- `ACPProtocol.WriteMessage`: 同上；ACP 当前只生成 image block，file_ref 走 text 前缀即可，零改动

**Read tool 可用性**: 默认 CC session 带 Read tool (`--dangerously-skip-permissions` + 默认 allowedTools)，无需改配置。若用户自定义 `--disallowedTools=Read` 会失效——但此时图片附件也不受影响；在 RFC 后续 PR 里加一段 sanity check。

### 3.6 前端改动

`dashboard.js:1793`:
```js
'<input type="file" id="file-input" accept="image/*,application/pdf" multiple ...>'
```

`handleFiles` 按扩展名分流：
- image → 既有 data URL preview
- PDF → 显示 `📄 <filename> (1.2 MB)` 占位块，点击可取消

单文件校验：
```js
if (f.type === 'application/pdf') {
    if (f.size > 32 * 1024 * 1024) { toast('PDF 超过 32 MB 限制'); return; }
} else if (f.type.startsWith('image/')) {
    if (f.size > 10 * 1024 * 1024) { toast('图片超过 10 MB 限制'); return; }
}
```

### 3.7 清理任务

新增 `internal/server/attachment_gc.go`：
- 每 6 小时扫描所有已知 workspace
- 删除 `<workspace>/.naozhi/attachments/<date>/` 中 `date < today - 7d` 的目录
- 复用 `server.go` 启动流程

**为什么 7 天**: session 里的 Read tool_use 引用的是路径字符串，假如会话续写到一周后，7 天前的 PDF 还需要。7 天是安全-效用平衡点，可配置化放到 `config.go`。

### 3.8 安全

| 攻击 | 防御 |
|---|---|
| 上传非 PDF 伪装 | `http.DetectContentType` magic bytes 校验 `%PDF-` 头 |
| 路径遍历 | UUID 生成文件名，从不使用客户端提供的名字做落盘路径 |
| workspace 逃逸 | 落盘调用 `filepath.Join(workspace, ".naozhi/attachments", ...)`，再做 `strings.HasPrefix` 确认 |
| PDF 携带 JS | Anthropic API 后端渲染，naozhi 侧不解析；`.naozhi/attachments/` 目录不通过 raw 端点暴露（`project_files.go` 已有沙箱，无需改动） |
| 空间耗尽 | per-workspace 目录大小上限（200 MB）+ 每日清理 |
| 跨用户读取 | `.meta` 记 owner，`resolveProjectFile` 不走 `.naozhi/attachments/`（通过目录黑名单） |

### 3.9 可观察性

新增 metrics：
- `naozhi_attachment_upload_total{kind,status}` — 上传计数
- `naozhi_attachment_persist_bytes_total{kind}` — 落盘字节量
- `naozhi_attachment_gc_deleted_total` — 清理计数

## 4. 实现步骤

| Phase | 改动 | 预计 LoC |
|---|---|---|
| A | 类型扩展: `ImageData` → `Attachment` + alias | ~30 |
| B | 落盘模块: `internal/attachment/store.go` (new pkg) | ~150 + test |
| C | upload_store 字节配额 | ~40 + test |
| D | `parseAttachmentFile` + `handleUpload` / `handleSend` 分流 | ~80 + test |
| E | `NewUserMessageWithMeta` 的 file_ref 分支 + text 前缀拼接 | ~50 + test |
| F | 前端: input accept / handleFiles 分流 / preview 卡片 | ~80 |
| G | GC 任务 + config 项 | ~100 + test |
| H | 端到端集成测试: upload → send → assert Read tool_use 被触发 | ~60 |

合计 **~600 LoC + 6 个测试文件**。

## 5. 验收标准

1. 用户在 dashboard 上传 `report.pdf` (25 MB)，与文本 "总结重点" 一同发送
2. Claude 自动调用 Read(`.naozhi/attachments/<date>/<uuid>.pdf`)
3. Claude 返回基于 PDF 内容的总结
4. 一周后，该文件被 GC 清理
5. 上传非法 PDF（改扩展名的 jpg）返回 400
6. 上传 40 MB PDF 返回 400 "超出 32 MB"
7. `go test ./... -race` 全绿

## 6. Out of scope / 遗留项

- IM 渠道接收 PDF（Feishu `file_key` 下载路径） — 后续 RFC
- 多 PDF 批次（当前允许 ≤20 files/send，已自然支持）
- PDF 页数硬校验（不解析 PDF 结构，靠 Anthropic API 拒绝超 100 页）
- workspace 外的"临时 session" PDF 落盘策略（当前强制要求 workspace 非空）
