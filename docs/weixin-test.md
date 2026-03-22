# 微信渠道测试指南

## 前提

- 不需要安装 OpenClaw
- 需要两个微信号（一个登录 bot，一个发消息测试）

## 第一步：扫码获取 Token

```bash
# 1. 获取二维码
curl -s "https://ilinkai.weixin.qq.com/ilink/bot/get_bot_qrcode?bot_type=3"
# 返回 {"qrcode":"xxx","qrcode_img_content":"https://liteapp.weixin.qq.com/q/...","ret":0}

# 2. 用微信扫 qrcode_img_content 里的链接，在手机上确认

# 3. 轮询登录状态（把 QRCODE 换成第 1 步返回的 qrcode 值）
curl -s "https://ilinkai.weixin.qq.com/ilink/bot/get_qrcode_status?qrcode=QRCODE" \
  -H "iLink-App-ClientVersion: 1"
# 扫码确认后返回 {"status":"confirmed","bot_token":"xxx","ilink_bot_id":"xxx",...}

# 4. 记下 bot_token
```

## 第二步：配置 config.yaml

```yaml
platforms:
  weixin:
    token: ${WEIXIN_BOT_TOKEN}   # 或直接填 bot_token
    # base_url: https://ilinkai.weixin.qq.com  # 默认值，可省略
```

## 第三步：启动

```bash
export WEIXIN_BOT_TOKEN="上面拿到的bot_token"
bin/naozhi --config config.yaml
```

## 第四步：发消息测试

用另一个微信号给扫码登录的微信号发消息，naozhi 会通过 Claude CLI 处理后回复。

## 验证清单

- [ ] getUpdates 长轮询正常（日志中有 `weixin platform started`）
- [ ] 收到用户消息（日志中有 session 创建）
- [ ] 回复消息送达微信（对方收到回复）
- [ ] `/new` 重置会话正常

## 技术说明

- 协议：iLink Bot API（`ilinkai.weixin.qq.com`），HTTP JSON 长轮询
- 收消息：`POST ilink/bot/getupdates`，35 秒长轮询
- 发消息：`POST ilink/bot/sendmessage`，需回传 `context_token`
- 认证：`Authorization: Bearer <bot_token>`，无需 App ID/Secret
- 连续失败 3 次自动退避 30 秒

## 踩坑记录：多轮对话必须携带 base_info

### 问题

使用 iLink Bot API 时，第一条 `sendMessage` 能正常送达微信，但第二条及之后的消息全部静默丢弃（API 返回 `{}` 无错误）。

### 根因

iLink Bot API 要求每个请求体携带 `base_info` 字段：

```json
{
  "msg": { ... },
  "base_info": { "channel_version": "naozhi-1.0.0" }
}
```

缺少 `base_info` 时，iLink 服务端将客户端视为非标准接入，**降级为一次性回复模式**——仅允许每次扫码后发送一条消息。

### 发现过程

通过分析腾讯官方 OpenClaw 微信插件 `@tencent-weixin/openclaw-weixin`（npm 包）的源码发现。该插件也使用同样的 HTTP API（`getupdates` / `sendmessage`），但每个请求都带了 `base_info: { channel_version }`。

### 完整修复清单

| 修复项 | 说明 |
|--------|------|
| `base_info.channel_version` | 每个请求体必须携带，标识客户端版本 |
| `msg.client_id` | sendMessage 时带唯一请求 ID |
| `msg.from_user_id` | 设为空字符串 `""`，不是 bot ID |
| `ilink/bot/getconfig` | 新 API：获取 `typing_ticket` |
| `ilink/bot/sendtyping` | 新 API：发送"正在输入"指示器 |

### 参考

- 官方插件包：`npm pack @tencent-weixin/openclaw-weixin`
- 安装器：`npx -y @tencent-weixin/openclaw-weixin-cli@latest install`
- iLink Bot API 无公开文档，以上均通过逆向插件源码获得

