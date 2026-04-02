# 语音消息转写设计

## 背景

飞书用户可以发送语音消息（`message_type: "audio"`）。当前 naozhi 在 transport 层直接过滤掉所有非 `text`/`image` 消息类型，语音消息被静默丢弃。

本文档设计支持飞书语音消息转写为文字，再送入现有 claude CLI 处理流程的完整方案。

---

## 目标

- 飞书语音消息到达后，自动转写为文字，与文字消息走相同处理路径
- 不引入新的认证凭据（复用 EC2 IAM role，与 Bedrock 同一身份）
- 转写失败时有明确的错误回复，不静默丢弃
- 对 claude CLI 完全透明（CLI 收到的就是普通文本）

**不在本期范围**：Slack / Discord 语音；实时流式语音（hold-to-talk）。

---

## 方案选型

| 方案 | 优点 | 缺点 |
|------|------|------|
| Amazon Transcribe Streaming | 无 S3 依赖，直接发音频字节，延迟低 | 不支持 AMR/SILK 格式，需格式转换 |
| Amazon Transcribe Batch | 支持更多格式（含 AMR） | 需要 S3，额外依赖，延迟高（轮询） |
| OpenAI Whisper API | 支持格式多，中文准确率高 | 需要额外 API key，不在 AWS 体系内 |
| 自托管 Whisper | 无外部依赖 | 需要 GPU/内存，运维成本高 |

**选型：Amazon Transcribe Streaming**

理由：
- EC2 已有 `AmazonBedrockFullAccess` 的 IAM role，加 Transcribe 权限即可，无额外凭据
- Streaming API 直接接收音频字节，无需 S3 中转
- 中文（`zh-CN`）支持良好
- 飞书语音消息若为 AMR 格式，通过 ffmpeg 预转换为 OGG_OPUS（见下文音频格式分析）

---

## 音频格式分析

飞书语音消息的实际格式因客户端而异：

| 客户端 | 格式 | Transcribe Streaming 支持 |
|--------|------|--------------------------|
| iOS Lark | AAC / M4A | ❌ 需转换 |
| Android Lark | AMR-NB | ❌ 需转换 |
| 桌面端 Lark | OGG_OPUS | ✅ 直接支持 |

**应对策略**：在下载音频后，检测 `Content-Type`：
- `audio/ogg` → 直接使用
- 其他格式 → 调用 `ffmpeg -i input -ar 16000 -ac 1 -c:a libopus output.ogg`

EC2 实例安装 ffmpeg：`dnf install -y ffmpeg`（Amazon Linux 2023 通过 RPM Fusion 支持）。

若无 ffmpeg 可回退到 Transcribe Batch（需 S3）。本期以 Streaming + ffmpeg 为主路径。

---

## 架构

```
飞书服务器发来语音消息
    |
    | message_type = "audio", content = {"file_key": "file_v3_xxx"}
    v
transport_hook.go / transport_ws.go
    | 新增 "audio" 分支
    v
feishu.DownloadAudio(ctx, messageID, fileKey)
    | GET /open-apis/im/v1/messages/{id}/resources/{key}?type=audio
    v
[]byte (原始音频) + Content-Type
    |
    +-- OGG/OGG_OPUS ---------> transcribe.Transcribe(ctx, data, "audio/ogg")
    |                                  |
    +-- 其他格式 -> ffmpegConvert() -> transcribe.Transcribe(ctx, oggData, "audio/ogg")
                                       |
                                       v
                               Amazon Transcribe Streaming
                               (HTTP/2, zh-CN, 16kHz)
                                       |
                                       v
                               transcript string
                                       |
                                       v
                           msg.Text = "[🎤 " + transcript + "]"
                                       |
                                       v
                           handler(ctx, msg)   // 后续与文字消息完全一致
```

---

## 模块设计

### 1. `internal/transcribe` 包

```
internal/transcribe/
├── transcribe.go        # Service 接口 + AWS Transcribe Streaming 实现
└── transcribe_test.go
```

**接口设计**（依赖注入，便于测试）：

```go
package transcribe

// Service 将音频字节转写为文本。
type Service interface {
    // Transcribe 接受原始音频字节和 MIME 类型，返回转写文本。
    // 支持的 mimeType: "audio/ogg", "audio/flac", "audio/mpeg" (MP3)
    Transcribe(ctx context.Context, data []byte, mimeType string) (string, error)
}

// Config 是 Amazon Transcribe Streaming 的配置。
type Config struct {
    Region       string // 默认 "us-east-1"，或与 Bedrock 同 region
    LanguageCode string // 默认 "zh-CN"
}

// New 用 IAM role 凭据（从环境/实例元数据自动加载）创建 Service。
func New(ctx context.Context, cfg Config) (Service, error)
```

**核心实现逻辑**（`transcribe.go`）：

```go
func (s *awsService) Transcribe(ctx context.Context, data []byte, mimeType string) (string, error) {
    encoding, sampleRate, err := resolveEncoding(mimeType)
    if err != nil {
        return "", err
    }

    resp, err := s.client.StartStreamTranscription(ctx,
        &transcribestreaming.StartStreamTranscriptionInput{
            LanguageCode:         types.LanguageCode(s.cfg.LanguageCode),
            MediaEncoding:        encoding,
            MediaSampleRateHertz: aws.Int32(sampleRate),
        })
    if err != nil {
        return "", fmt.Errorf("start stream: %w", err)
    }

    stream := resp.GetStream()
    defer stream.Close()

    // 发送音频（分块，64KB/块）
    go func() {
        defer stream.CloseWithError(nil)
        const chunkSize = 64 * 1024
        for i := 0; i < len(data); i += chunkSize {
            end := min(i+chunkSize, len(data))
            stream.Send(ctx, &types.AudioStreamMemberAudioEvent{
                Value: types.AudioEvent{AudioChunk: data[i:end]},
            })
        }
    }()

    // 收集转写结果（取 IsPartial=false 的最终 transcript）
    var parts []string
    for event := range stream.Events() {
        switch e := event.(type) {
        case *types.TranscriptEventMemberTranscriptEvent:
            for _, r := range e.Value.Transcript.Results {
                if !aws.ToBool(r.IsPartial) && len(r.Alternatives) > 0 {
                    parts = append(parts, aws.ToString(r.Alternatives[0].Transcript))
                }
            }
        }
    }
    if err := stream.Err(); err != nil {
        return "", fmt.Errorf("stream error: %w", err)
    }
    return strings.Join(parts, " "), nil
}

// resolveEncoding 根据 MIME 类型返回 Transcribe 编码枚举和推荐采样率。
func resolveEncoding(mimeType string) (types.MediaEncoding, int32, error) {
    switch mimeType {
    case "audio/ogg", "audio/ogg; codecs=opus":
        return types.MediaEncodingOggOpus, 48000, nil
    case "audio/flac":
        return types.MediaEncodingFlac, 16000, nil
    case "audio/mpeg", "audio/mp3":
        return types.MediaEncodingMp3, 44100, nil
    default:
        return "", 0, fmt.Errorf("unsupported audio format: %s", mimeType)
    }
}
```

### 2. `feishu.DownloadAudio`（新增方法）

复用 `DownloadImage` 的模式，只改 `type=audio`：

```go
// DownloadAudio 下载飞书语音消息的音频文件，返回原始字节和 MIME 类型。
func (f *Feishu) DownloadAudio(ctx context.Context, messageID, fileKey string) ([]byte, string, error) {
    token, err := f.getAccessToken(ctx)
    if err != nil {
        return nil, "", fmt.Errorf("get access token: %w", err)
    }

    req, err := http.NewRequestWithContext(ctx, "GET",
        f.baseURL+"/open-apis/im/v1/messages/"+messageID+"/resources/"+fileKey+"?type=audio", nil)
    if err != nil {
        return nil, "", fmt.Errorf("create request: %w", err)
    }
    req.Header.Set("Authorization", "Bearer "+token)

    resp, err := feishuHTTPClient.Do(req)
    if err != nil {
        return nil, "", fmt.Errorf("download audio: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
        return nil, "", fmt.Errorf("download audio: status %d, body: %s", resp.StatusCode, body)
    }

    data, err := io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024)) // 20MB max
    if err != nil {
        return nil, "", fmt.Errorf("read audio body: %w", err)
    }

    mimeType := resp.Header.Get("Content-Type")
    if i := strings.IndexByte(mimeType, ';'); i >= 0 {
        mimeType = strings.TrimSpace(mimeType[:i])
    }
    if mimeType == "" || mimeType == "application/octet-stream" {
        mimeType = "audio/ogg" // 飞书默认格式推断
    }
    return data, mimeType, nil
}
```

### 3. `internal/transcribe/convert.go`（ffmpeg 格式转换）

```go
// ConvertToOgg 调用 ffmpeg 将任意音频格式转换为 OGG_OPUS（16kHz, 单声道）。
// 若系统无 ffmpeg，返回 ErrFFmpegNotFound。
func ConvertToOgg(ctx context.Context, data []byte) ([]byte, error)

var ErrFFmpegNotFound = errors.New("ffmpeg not found in PATH; install with: dnf install -y ffmpeg")
```

主路径（`Transcribe` 入口）：

```go
func (s *awsService) Transcribe(ctx context.Context, data []byte, mimeType string) (string, error) {
    // 若格式不被 Streaming 直接支持，先转换
    if !isSupportedByStreaming(mimeType) {
        var err error
        data, err = convert.ConvertToOgg(ctx, data)
        if err != nil {
            return "", fmt.Errorf("audio convert: %w", err)
        }
        mimeType = "audio/ogg"
    }
    return s.streamTranscribe(ctx, data, mimeType)
}
```

### 4. Transport 层扩展

**`transport_hook.go`** — 在 `msgType` 过滤处新增 `"audio"` 分支：

```go
// 过滤：只处理 text、image、audio
if msgType != "text" && msgType != "image" && msgType != "audio" {
    return
}

// ...已有 text/image switch 后追加：
case "audio":
    var content struct {
        FileKey string `json:"file_key"`
    }
    if err := json.Unmarshal([]byte(event.Message.Content), &content); err != nil || content.FileKey == "" {
        return
    }
    f.wg.Add(1)
    go func() {
        defer f.wg.Done()
        data, mime, err := f.DownloadAudio(context.Background(), event.Message.MessageID, content.FileKey)
        if err != nil {
            slog.Error("feishu download audio failed", "err", err, "key", content.FileKey)
            msg.Text = "[语音消息，下载失败]"
            handler(context.Background(), msg)
            return
        }
        transcript, err := f.transcriber.Transcribe(context.Background(), data, mime)
        if err != nil {
            slog.Error("feishu transcribe failed", "err", err)
            msg.Text = "[语音消息，转写失败]"
            handler(context.Background(), msg)
            return
        }
        if transcript == "" {
            return // 无内容静默丢弃（空白音频）
        }
        msg.Text = transcript
        handler(context.Background(), msg)
    }()
```

**`transport_ws.go`** — `parseSDKEvent` 函数签名扩展：

```go
// 返回值新增 fileKey（音频用），原 imageKey 保留
// (msg, messageID, imageKey, fileKey, ok)
func parseSDKEvent(event *larkim.P2MessageReceiveV1) (platform.IncomingMessage, string, string, string, bool)
```

`startWebSocket` 中增加 audio 分支（与 image 分支并列）：

```go
case imageKey != "":
    // 现有图片处理逻辑

case fileKey != "":
    f.wg.Add(1)
    go func() {
        defer f.wg.Done()
        data, mime, err := f.DownloadAudio(ctx, messageID, fileKey)
        // ...同 hook 版本
    }()
```

### 5. `Feishu` 结构体注入 transcriber

```go
type Feishu struct {
    // ...现有字段...
    transcriber transcribe.Service // nil 时跳过语音消息（未配置 STT）
}

func New(cfg Config, transcriber transcribe.Service) *Feishu { ... }
```

`transcriber` 为 `nil` 时，audio 消息不处理（不报错，info 日志记录），保持向后兼容。

---

## 配置变更

`config.yaml` 新增 `transcribe` 块：

```yaml
transcribe:
  enabled: true
  provider: "aws"           # 目前只有 "aws"
  region: "us-west-2"       # 默认同 cli 使用的 Bedrock region
  language: "zh-CN"         # BCP-47，默认 zh-CN
  # ffmpeg_path: "/usr/bin/ffmpeg"  # 可选，默认从 PATH 查找
```

不配置 `transcribe` 块时，功能禁用，行为与现在一致。

---

## IAM 权限

EC2 的 `naozhi-ec2-role` 需追加以下 inline policy：

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "transcribe:StartStreamTranscription"
      ],
      "Resource": "*"
    }
  ]
}
```

仅需一条权限，最小化授权。

---

## 错误处理策略

| 失败场景 | 处理方式 |
|---------|---------|
| 飞书 API 下载失败（网络/token 过期） | 回复 `[语音消息，下载失败]`，error 日志 |
| 音频格式不支持且无 ffmpeg | 回复 `[语音消息，不支持的音频格式]`，warn 日志 |
| ffmpeg 转换失败 | 回复 `[语音消息，转写失败]`，error 日志 |
| Transcribe Streaming 返回空文本 | 静默丢弃（用户发送了空白/噪声音频） |
| Transcribe 服务报错（网络/权限） | 回复 `[语音消息，转写失败]`，error 日志 |
| Transcribe 超时（> 30s） | context cancel，回复 `[语音消息，转写超时]` |

**超时设置**：`Transcribe` 调用使用独立 context，timeout = 30s（飞书语音消息最长 60s，Transcribe 处理约为音频时长的 0.3~0.5 倍）。

---

## 测试策略

### 单元测试

**`internal/transcribe/transcribe_test.go`**：
- Mock `StartStreamTranscription`，测试 transcript 拼接逻辑
- `resolveEncoding` 覆盖支持/不支持格式
- `ConvertToOgg` — 跳过（依赖系统 ffmpeg），提供 build tag `//go:build integration`

**`feishu/feishu_test.go`**：
- 注入 `mockTranscriber`，测试 audio 分支的错误路径（下载失败/转写失败/空文本）
- `parseSDKEvent` 新增 audio 类型的 case

### 集成测试

手动验证（无法 mock 飞书/Transcribe）：
1. 发送飞书语音消息（OGG 格式，桌面端录制）→ 验证收到转写文本
2. 发送飞书语音消息（AMR 格式，手机端录制）→ 验证 ffmpeg 转换路径
3. Transcribe 权限未配置 → 验证 `[语音消息，转写失败]` 回复

---

## 依赖变更

`go.mod` 新增：

```
github.com/aws/aws-sdk-go-v2                    v1.x
github.com/aws/aws-sdk-go-v2/config             v1.x
github.com/aws/aws-sdk-go-v2/service/transcribestreaming  v1.x
```

无其他外部依赖。ffmpeg 是系统工具，不入 go.mod。

---

## 实现步骤

1. **`internal/transcribe/transcribe.go`** — `Service` 接口 + AWS 实现（~100 行）
2. **`internal/transcribe/convert.go`** — ffmpeg 格式转换（~40 行）
3. **`feishu/feishu.go`** — `DownloadAudio` 方法 + `transcriber` 字段注入（~30 行）
4. **`feishu/transport_hook.go`** — 新增 `audio` 分支（~25 行）
5. **`feishu/transport_ws.go`** — `parseSDKEvent` 扩展 + `startWebSocket` 新增 audio goroutine（~30 行）
6. **`config/config.go`** — `TranscribeConfig` 结构体 + 加载逻辑（~20 行）
7. **`cmd/naozhi/main.go`** — 按配置初始化 `transcribe.Service`，注入 `Feishu`（~10 行）
8. **IAM Policy** — 控制台追加 `transcribe:StartStreamTranscription`
9. **EC2** — `dnf install -y ffmpeg`

**预估总量**：~255 行新代码，改动不触及现有核心逻辑（session/router/cli）。

---

## 风险

| 风险 | 概率 | 缓解 |
|------|------|------|
| 飞书音频为 SILK 格式（非 AMR/OGG） | 中 | 实现时先打印 Content-Type，实测确认；SILK → PCM 有开源 go-silk 库 |
| Amazon Transcribe Streaming 在当前 region 不可用 | 低 | 配置 `transcribe.region` 独立于 Bedrock region |
| ffmpeg 不在 Amazon Linux 2023 默认 repo | 中 | 通过 RPM Fusion 安装；或打包进部署脚本 |
| Transcribe 对中文方言/口音准确率不稳定 | 高 | 预期，无法规避；用户可重发文字消息 |
